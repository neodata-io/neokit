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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Snapshotter writes a consistent, standalone copy of the live database to dst,
// which must not already exist. neokit/sqlitex's SnapshotTo has this shape.
type Snapshotter interface {
	SnapshotTo(ctx context.Context, dst string) error
}

const (
	filePrefix = "backup-"
	fileSuffix = ".db"
	dateLayout = "2006-01-02"
)

// Service produces snapshots and owns the on-disk backup directory.
type Service struct {
	snap      Snapshotter
	dir       string
	retention int
	now       func() time.Time
}

// New wires the service. An empty dir disables the scheduled backup — writing
// into the working directory instead would scatter database copies wherever the
// process happened to start. retention is clamped to at least 1, because a
// retention of zero would delete each backup the moment it was written.
func New(s Snapshotter, dir string, retention int) *Service {
	if retention < 1 {
		retention = 1
	}
	return &Service{snap: s, dir: dir, retention: retention, now: time.Now}
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
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("backup: create directory: %w", err)
	}

	path := filepath.Join(s.dir, filePrefix+s.now().Format(dateLayout)+fileSuffix)
	if _, err := os.Stat(path); err == nil {
		s.prune()
		return nil // already written today
	}

	if err := s.snap.SnapshotTo(ctx, path); err != nil {
		return fmt.Errorf("backup: snapshot: %w", err)
	}
	s.prune()
	return nil
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
		if e.IsDir() || !isBackupName(e.Name()) {
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

// prune deletes all but the newest retention backups. Best-effort: a file that
// cannot be removed is left alone rather than failing the backup that just
// succeeded.
func (s *Service) prune() {
	list, err := s.List(context.Background())
	if err != nil || len(list) <= s.retention {
		return
	}
	for _, info := range list[s.retention:] {
		_ = os.Remove(filepath.Join(s.dir, info.Name))
	}
}

func isBackupName(name string) bool {
	return strings.HasPrefix(name, filePrefix) && strings.HasSuffix(name, fileSuffix)
}
