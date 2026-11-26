// Package backup turns a snapshot primitive into the two things an application
// needs: a fresh copy streamed on demand, and a dated copy written to disk on a
// schedule.
//
// The artefact is deliberately a bare database file — no wrapper, no manifest,
// no compression. There is nothing to parse, validate, or version: restore by
// stopping the service, dropping the file in place, and starting again.
//
// What it guards against is accidental deletion, a bad migration, or corruption
// — NOT disk failure, since by default the copies sit on the same volume as the
// original. For real disaster recovery point dir at another volume and copy the
// files off the box.
package backup

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/neodata-io/neokit/jobs"
)

// scheduleTimeout bounds one run. A snapshot is a file copy, so an hour is
// generous for a large database and still short of unbounded — the failure
// mode jobs exists to prevent.
const scheduleTimeout = time.Hour

// Snapshotter writes a consistent, standalone copy of the live database to dst,
// which does not exist when it is called. Wrap neokit/sqlitex's SnapshotTo.
type Snapshotter interface {
	SnapshotTo(ctx context.Context, dst string) error
}

const (
	// DefaultPrefix begins a backup filename when Options.Prefix is empty.
	DefaultPrefix = "backup-"

	// DefaultHour is when the scheduled backup runs if Options.At is unset.
	DefaultHour = 3

	fileSuffix = ".db"
	dateLayout = "2006-01-02"

	// tempSuffix marks a snapshot still being written. It ends the name, so an
	// unfinished file never matches isBackupName.
	tempSuffix = ".tmp"

	// staleTemp is when an unfinished snapshot is surely abandoned: no run lasts
	// longer than scheduleTimeout, so a younger one may belong to a live run.
	staleTemp = scheduleTimeout
)

// Clock is a local wall-clock time of day, read in the deployment's own
// timezone.
type Clock struct{ Hour, Minute int }

func (c Clock) String() string { return fmt.Sprintf("%02d:%02d", c.Hour, c.Minute) }

// valid reports whether c names a real time of day.
func (c Clock) valid() bool {
	return c.Hour >= 0 && c.Hour <= 23 && c.Minute >= 0 && c.Minute <= 59
}

// Options configures a [Service].
type Options struct {
	// Dir is where scheduled backups are written. Empty disables them — writing
	// into the working directory instead would scatter database copies wherever
	// the process happened to start.
	Dir string

	// Retention is how many backups to keep. Clamped to at least 1: a retention
	// of zero would delete each backup the moment it was written, the one
	// outcome a backup system must never have.
	Retention int

	// At is the local time the scheduled backup runs. Nil means [DefaultHour]:00
	// — the quiet part of the night in the deployment's own timezone, which is
	// what a wall-clock schedule is for.
	//
	// A pointer so that midnight stays expressible: &Clock{} is 00:00, where a
	// plain zero value could only ever mean "unset".
	At *Clock

	// Prefix begins every backup filename. Empty means [DefaultPrefix].
	//
	// It is configurable because this package adopts a directory that may already
	// have files in it. A hardcoded prefix would make an existing deployment's
	// backups invisible to List and to prune the day it migrated — orphaned
	// rather than deleted, which is worse: they stop being listed and never stop
	// accumulating.
	//
	// Only <Prefix><YYYY-MM-DD>.db is this service's. Any other file under the
	// prefix is neither listed nor pruned.
	Prefix string
}

// Service produces snapshots and owns the on-disk backup directory.
type Service struct {
	snap      Snapshotter
	dir       string
	retention int
	prefix    string
	at        Clock
	now       func() time.Time

	// mu serialises WriteDaily within one Service; random temp names keep
	// separate Services on one Dir apart.
	mu sync.Mutex
}

// New wires the service: the daily schedule Run drives keeps the backup
// directory current when one is configured. See [Options] for field meaning.
//
// An [Options.At] outside 0:00–23:59 panics here, where the value is still
// traceable to the call that supplied it. The scheduler panics on one too, but
// it does so inside a supervised goroutine minutes later — a stack trace with no
// caller in it, on a loop that never produces a backup.
func New(s Snapshotter, o Options) *Service {
	if o.Retention < 1 {
		o.Retention = 1
	}
	prefix := o.Prefix
	if prefix == "" {
		prefix = DefaultPrefix
	}
	at := Clock{Hour: DefaultHour}
	if o.At != nil {
		at = *o.At
	}
	if !at.valid() {
		panic(fmt.Sprintf("backup: Options.At is %d:%d, which is not a time of day", at.Hour, at.Minute))
	}

	return &Service{snap: s, dir: o.Dir, retention: o.Retention, prefix: prefix, at: at, now: time.Now}
}

// Run drives the daily backup schedule until ctx is done. It's a no-op that
// returns immediately when no backup directory is configured — start it
// unconditionally with safe.Go(a.Context(), "backups", func() { svc.Run(a.Context()) }).
func (s *Service) Run(ctx context.Context) {
	if strings.TrimSpace(s.dir) == "" {
		return
	}
	s.schedule(s.at).Run(ctx)
}

// schedule is the daily job that keeps the declared line honest.
//
// RunAtStart is on, which [jobs.Daily] warns against for announcements but is
// right here: WriteDaily is named for the day, so a restart tops up a missing
// backup and a restart loop is a no-op. Without it a service that restarts each
// morning after its backup hour would never back up at all.
func (s *Service) schedule(at Clock) jobs.Daily {
	return jobs.Daily{
		Name:       "backups",
		Hour:       at.Hour,
		Minute:     at.Minute,
		Timeout:    scheduleTimeout,
		RunAtStart: true,
		Do:         s.WriteDaily,
	}
}

// Backup streams a fresh snapshot to w.
//
// Fresh, never a stored file: a download must reflect the database now, not
// whenever the scheduler last ran. The snapshot goes to a temporary file first
// because the primitive writes to a path, and it is removed on every exit path.
func (s *Service) Backup(ctx context.Context, w io.Writer) error {
	tmp, err := os.MkdirTemp("", "neokit-backup-")
	if err != nil {
		return fmt.Errorf("backup: temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	path := filepath.Join(tmp, "snapshot.db")
	if err := s.snap.SnapshotTo(ctx, path); err != nil {
		return fmt.Errorf("backup: snapshot: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("backup: open snapshot: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(w, f); err != nil {
		return fmt.Errorf("backup: stream: %w", err)
	}
	return nil
}

// WriteDaily writes today's backup and prunes old ones.
//
// The dated filename is what makes this idempotent: a second call on the same
// day finds the file already there and returns without touching the database, so
// a scheduler that ticks more than once — or a restart — costs nothing.
func (s *Service) WriteDaily(ctx context.Context) error {
	if strings.TrimSpace(s.dir) == "" {
		return nil // scheduled backups disabled
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 0700 applies only to a directory created here; an existing one is left alone.
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("backup: create directory: %w", err)
	}
	s.removeTemps()

	now := s.now()
	path := filepath.Join(s.dir, s.prefix+now.Format(dateLayout)+fileSuffix)
	// An empty file is not a backup: releases that snapshotted straight to this
	// name left one behind when the snapshot was cancelled.
	if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
		s.prune(now)
		return nil // already written today
	}

	if err := s.write(ctx, path); err != nil {
		return err
	}
	s.prune(now)
	return nil
}

// write snapshots to a temporary name and renames it into place, so the dated
// name only ever holds a complete backup. The temp name is random so a second
// Service on the same Dir never writes into or publishes this one's file.
func (s *Service) write(ctx context.Context, path string) error {
	tmp := path + "." + rand.Text() + tempSuffix
	if err := s.snap.SnapshotTo(ctx, tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("backup: snapshot: %w", err)
	}
	if err := seal(tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("backup: sync snapshot: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("backup: publish snapshot: %w", err)
	}
	// Best-effort: make the rename itself durable. Not every platform can sync
	// a directory.
	if d, err := os.Open(s.dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// seal makes a finished snapshot private and flushes it to disk. SQLite does
// neither for a VACUUM INTO target.
func seal(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// removeTemps clears snapshots a killed run left unfinished: only names this
// service would itself have written, and only once older than any run can be.
func (s *Service) removeTemps() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !s.isTempName(e.Name()) {
			continue
		}
		if fi, err := e.Info(); err == nil && s.now().Sub(fi.ModTime()) > staleTemp {
			_ = os.Remove(filepath.Join(s.dir, e.Name()))
		}
	}
}

// isTempName reports whether name is a snapshot write left: a backup name plus
// a random part and tempSuffix, or plus tempSuffix alone as older releases wrote.
func (s *Service) isTempName(name string) bool {
	name, ok := strings.CutSuffix(name, tempSuffix)
	if !ok {
		return false
	}
	if s.isBackupName(name) {
		return true
	}
	i := strings.LastIndexByte(name, '.')
	return i > 0 && s.isBackupName(name[:i])
}

// Info describes one stored backup.
type Info struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modifiedAt"`
}

// List returns the stored backups, newest first. A missing directory is an empty
// list rather than an error: nothing has been written yet is a normal state.
func (s *Service) List(context.Context) ([]Info, error) {
	if strings.TrimSpace(s.dir) == "" {
		return []Info{}, nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Info{}, nil
		}
		return nil, fmt.Errorf("backup: read directory: %w", err)
	}

	out := make([]Info, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !s.isBackupName(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue // vanished between ReadDir and Info; not worth failing the list
		}
		out = append(out, Info{Name: e.Name(), Size: fi.Size(), ModTime: fi.ModTime()})
	}
	// By name, which is by date: the dated filename sorts lexically in
	// chronological order, so this does not depend on mtimes a copy would rewrite.
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// prune deletes all but the newest retention backups dated up to now, so a
// future-dated file can never outrank, and cost, the one just written.
// Best-effort: a file that cannot be removed is left alone.
func (s *Service) prune(now time.Time) {
	list, err := s.List(context.Background())
	if err != nil {
		return
	}
	latest := s.prefix + now.Format(dateLayout) + fileSuffix
	list = slices.DeleteFunc(list, func(i Info) bool { return i.Name > latest })
	if len(list) <= s.retention {
		return
	}
	for _, info := range list[s.retention:] {
		_ = os.Remove(filepath.Join(s.dir, info.Name))
	}
}

// isBackupName reports whether name is one WriteDaily wrote: prefix, a date,
// suffix. The date is required so that prune can never claim a file that merely
// shares the prefix — a hand-kept copy, or the live database itself.
func (s *Service) isBackupName(name string) bool {
	date, ok := strings.CutPrefix(name, s.prefix)
	if !ok {
		return false
	}
	if date, ok = strings.CutSuffix(date, fileSuffix); !ok {
		return false
	}
	_, err := time.Parse(dateLayout, date)
	return err == nil
}
