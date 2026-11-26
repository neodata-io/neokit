package backup_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/neodata-io/neokit/backup"
)

// fakeSnap writes a recognisable file wherever it is pointed.
type fakeSnap struct{ calls int }

func (f *fakeSnap) SnapshotTo(_ context.Context, dst string) error {
	f.calls++
	return os.WriteFile(dst, []byte("snapshot"), 0o600)
}

func TestWriteDailyIsIdempotentWithinADay(t *testing.T) {
	dir := t.TempDir()
	snap := &fakeSnap{}
	s := backup.New(snap, backup.Options{Dir: dir, Retention: 7})

	for range 3 {
		if err := s.WriteDaily(t.Context()); err != nil {
			t.Fatalf("WriteDaily: %v", err)
		}
	}
	if snap.calls != 1 {
		t.Errorf("snapshotted %d times, want 1 — the dated name makes a rerun a no-op", snap.calls)
	}
}

func TestRetentionKeepsTheNewest(t *testing.T) {
	dir := t.TempDir()
	// Seed more dated files than the retention allows, oldest first.
	for i := 1; i <= 5; i++ {
		name := filepath.Join(dir, fmt.Sprintf("backup-2026-01-%02d.db", i))
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := backup.New(&fakeSnap{}, backup.Options{Dir: dir, Retention: 3})
	if err := s.WriteDaily(t.Context()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}

	got, err := s.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("kept %d backups, want the retention of 3: %+v", len(got), got)
	}
	// The oldest must be the ones pruned.
	for _, info := range got {
		if info.Name == "backup-2026-01-01.db" || info.Name == "backup-2026-01-02.db" {
			t.Errorf("kept %q — pruning must drop the oldest first", info.Name)
		}
	}
}

// A retention of zero or below would delete every backup the moment it was
// written, which is the one outcome a backup system must never have.
func TestRetentionIsClampedToAtLeastOne(t *testing.T) {
	dir := t.TempDir()
	s := backup.New(&fakeSnap{}, backup.Options{Dir: dir, Retention: 0})
	if err := s.WriteDaily(t.Context()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}
	got, err := s.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("kept %d, want 1 — retention must clamp, never delete everything", len(got))
	}
}

// An empty directory disables the scheduled backup rather than writing into the
// working directory.
func TestEmptyDirDisablesScheduledBackups(t *testing.T) {
	snap := &fakeSnap{}
	s := backup.New(snap, backup.Options{Dir: "", Retention: 7})
	if err := s.WriteDaily(t.Context()); err != nil {
		t.Fatalf("a disabled backup must not be an error: %v", err)
	}
	if snap.calls != 0 {
		t.Error("an empty dir must write nothing")
	}
}

// Backup streams a *fresh* snapshot, never a stored file — a download must
// reflect the database now, not whenever the scheduler last ran.
func TestBackupStreamsAFreshSnapshot(t *testing.T) {
	snap := &fakeSnap{}
	s := backup.New(snap, backup.Options{Dir: t.TempDir(), Retention: 7})

	var buf writeCounter
	if err := s.Backup(t.Context(), &buf); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if snap.calls != 1 {
		t.Errorf("snapshotted %d times, want 1 fresh snapshot", snap.calls)
	}
	if buf.n == 0 {
		t.Error("nothing was streamed")
	}
}

type writeCounter struct{ n int }

func (w *writeCounter) Write(p []byte) (int, error) { w.n += len(p); return len(p), nil }

var _ io.Writer = (*writeCounter)(nil)

func TestListIsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"01", "03", "02"} {
		p := filepath.Join(dir, "backup-2026-01-"+d+".db")
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	s := backup.New(&fakeSnap{}, backup.Options{Dir: dir, Retention: 10})
	got, err := s.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 || got[0].Name != "backup-2026-01-03.db" {
		t.Errorf("List = %+v, want newest-dated first", got)
	}
}

// A directory that already has files in it is the case this exists for: a
// hardcoded prefix would make an existing deployment's backups invisible to List
// and to prune the day it migrated.
func TestCustomPrefixIsUsedForWritingAndReading(t *testing.T) {
	dir := t.TempDir()
	snap := &fakeSnap{}
	s := backup.New(snap, backup.Options{Dir: dir, Retention: 7, Prefix: "neogate-"})

	if err := s.WriteDaily(t.Context()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}
	got, err := s.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List = %+v, want the one backup just written", got)
	}
	if !strings.HasPrefix(got[0].Name, "neogate-") {
		t.Errorf("Name = %q, want the configured prefix", got[0].Name)
	}
}

// A file under a different prefix is not this service's to list or to delete.
func TestAForeignPrefixIsIgnoredEntirely(t *testing.T) {
	dir := t.TempDir()
	foreign := filepath.Join(dir, "someone-elses-2026-01-01.db")
	if err := os.WriteFile(foreign, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := backup.New(&fakeSnap{}, backup.Options{Dir: dir, Retention: 1, Prefix: "backup-"})

	// Write enough of our own that pruning is guaranteed to run.
	if err := s.WriteDaily(t.Context()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}
	got, err := s.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, info := range got {
		if info.Name == "someone-elses-2026-01-01.db" {
			t.Error("List returned a file under a foreign prefix")
		}
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("prune deleted a file it does not own: %v", err)
	}
}

func TestEmptyPrefixFallsBackToTheDefault(t *testing.T) {
	dir := t.TempDir()
	s := backup.New(&fakeSnap{}, backup.Options{Dir: dir, Retention: 7})
	if err := s.WriteDaily(t.Context()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}
	got, _ := s.List(t.Context())
	if len(got) != 1 || !strings.HasPrefix(got[0].Name, backup.DefaultPrefix) {
		t.Errorf("List = %+v, want the default prefix", got)
	}
}

// Run has to have something behind it. Configuring "daily, keep 7" and
// scheduling nothing is the bug this replaces, so the assertion is the file.
func TestRunWritesABackup(t *testing.T) {
	dir := t.TempDir()
	svc := backup.New(&fakeSnap{}, backup.Options{Dir: dir, Retention: 7})

	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() { defer close(stopped); svc.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-stopped })

	// The job writes today's file before waiting for the next occurrence, so a
	// service restarted after its backup hour still backs up that day.
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		if len(entries) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Run wrote no backup")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An hour the scheduler would reject has to fail here, at the call that supplied
// it. Deferred, it becomes a panic inside a supervised goroutine that respawns
// forever and never writes a backup.
func TestAnImpossibleTimeFailsAtWiring(t *testing.T) {
	for _, at := range []backup.Clock{{Hour: 24}, {Hour: -1}, {Minute: 60}, {Minute: -1}} {
		t.Run(at.String(), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("New accepted At = %s", at)
				}
			}()
			backup.New(&fakeSnap{}, backup.Options{Dir: t.TempDir(), At: &at})
		})
	}
}

// Off means off: no directory, Run returns immediately instead of scheduling.
func TestRunIsANoopWhenNoDirIsConfigured(t *testing.T) {
	svc := backup.New(&fakeSnap{}, backup.Options{})

	done := make(chan struct{})
	go func() { defer close(done); svc.Run(t.Context()) }()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run must return immediately when no backup directory is configured")
	}
}

// today is the name WriteDaily gives a backup written now.
func today(prefix string) string {
	return prefix + time.Now().Format("2006-01-02") + ".db"
}

// dyingSnap writes half a file and then fails, as a cancelled or killed
// snapshot does.
type dyingSnap struct{}

func (dyingSnap) SnapshotTo(_ context.Context, dst string) error {
	if err := os.WriteFile(dst, []byte("half"), 0o600); err != nil {
		return err
	}
	return fmt.Errorf("snapshot died")
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestAFailedSnapshotIsNotTodaysBackup(t *testing.T) {
	dir := t.TempDir()
	if err := backup.New(dyingSnap{}, backup.Options{Dir: dir}).WriteDaily(t.Context()); err == nil {
		t.Fatal("WriteDaily must report the failed snapshot")
	}
	if got := names(t, dir); len(got) != 0 {
		t.Fatalf("a failed snapshot left %v behind", got)
	}

	// The same day's retry has to write a real one rather than find the wreck.
	snap := &fakeSnap{}
	if err := backup.New(snap, backup.Options{Dir: dir}).WriteDaily(t.Context()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, today(backup.DefaultPrefix)))
	if err != nil || string(got) != "snapshot" {
		t.Errorf("today's backup = %q, %v; want the retried snapshot", got, err)
	}
}

// The wreck sorts newest, so accepting it used to prune the last good backup.
func TestAFailedSnapshotDoesNotCostAnOlderBackup(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "backup-2020-01-01.db")
	if err := os.WriteFile(good, []byte("good"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := backup.New(dyingSnap{}, backup.Options{Dir: dir, Retention: 1})
	for range 2 {
		if err := s.WriteDaily(t.Context()); err == nil {
			t.Fatal("WriteDaily must report the failed snapshot")
		}
	}
	if _, err := os.Stat(good); err != nil {
		t.Errorf("the only good backup was pruned: %v", err)
	}
}

// While the snapshot is being written it must not be visible as a backup.
func TestASnapshotInProgressIsNotListed(t *testing.T) {
	dir := t.TempDir()
	var s *backup.Service
	var during []backup.Info
	s = backup.New(snapFunc(func(_ context.Context, dst string) error {
		if err := os.WriteFile(dst, []byte("snapshot"), 0o600); err != nil {
			return err
		}
		var err error
		during, err = s.List(t.Context())
		return err
	}), backup.Options{Dir: dir})

	if err := s.WriteDaily(t.Context()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}
	if len(during) != 0 {
		t.Errorf("List during the snapshot = %+v, want nothing until it is complete", during)
	}
	if got := names(t, dir); len(got) != 1 || got[0] != today(backup.DefaultPrefix) {
		t.Errorf("directory holds %v, want only today's backup", got)
	}
}

type snapFunc func(context.Context, string) error

func (f snapFunc) SnapshotTo(ctx context.Context, dst string) error { return f(ctx, dst) }

// A process killed mid-snapshot cannot clean up after itself; the next run does,
// once the file is older than any run can last.
func TestALeftoverTempFromACrashIsRemoved(t *testing.T) {
	dir := t.TempDir()
	leftovers := []string{"backup-2020-01-01.db.tmp", "backup-2020-01-01.db.ABC123.tmp"}
	foreign := []string{"notes.tmp", "backup-pre-upgrade.db.tmp"}
	old := time.Now().Add(-2 * time.Hour)
	for _, name := range append(foreign, leftovers...) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := backup.New(&fakeSnap{}, backup.Options{Dir: dir}).WriteDaily(t.Context()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}
	for _, name := range leftovers {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("the crashed run's temp file %q is still there (stat err = %v)", name, err)
		}
	}
	for _, name := range foreign {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("removed %q, which is not this service's temp file: %v", name, err)
		}
	}
}

// What an older version left when its snapshot was cancelled.
func TestAZeroByteDatedFileIsReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, today(backup.DefaultPrefix))
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backup.New(&fakeSnap{}, backup.Options{Dir: dir}).WriteDaily(t.Context()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "snapshot" {
		t.Errorf("today's backup = %q, want the empty file replaced by a snapshot", got)
	}
}

// A file someone kept by hand sorts after every date, so it used to take the
// one retention slot and get each day's backup deleted as it was written.
func TestPruneLeavesAHandKeptFileAndTodaysBackup(t *testing.T) {
	dir := t.TempDir()
	kept := filepath.Join(dir, "backup-pre-upgrade.db")
	if err := os.WriteFile(kept, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := backup.New(&fakeSnap{}, backup.Options{Dir: dir, Retention: 1})
	if err := s.WriteDaily(t.Context()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}
	for _, p := range []string{kept, filepath.Join(dir, today(backup.DefaultPrefix))} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s is gone: %v", filepath.Base(p), err)
		}
	}
	got, _ := s.List(t.Context())
	if len(got) != 1 || got[0].Name != today(backup.DefaultPrefix) {
		t.Errorf("List = %+v, want only the dated backup", got)
	}
}

// A prefix without a separator also matches the live database's own name.
func TestPruneNeverDeletesTheLiveDatabase(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "app.db")
	if err := os.WriteFile(live, []byte("LIVE"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := backup.New(&fakeSnap{}, backup.Options{Dir: dir, Retention: 1, Prefix: "app"})
	if err := s.WriteDaily(t.Context()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("prune deleted the live database: %v", err)
	}
}

func TestNewBackupsArePrivateToTheServiceUser(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix permission bits")
	}
	dir := filepath.Join(t.TempDir(), "backups")
	// A snapshotter that leaves the file world-readable, as SQLite does.
	s := backup.New(snapFunc(func(_ context.Context, dst string) error {
		return os.WriteFile(dst, []byte("snapshot"), 0o644)
	}), backup.Options{Dir: dir})
	if err := s.WriteDaily(t.Context()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}
	if got := mode(t, dir); got != 0o700 {
		t.Errorf("new backup directory is %o, want 700", got)
	}
	if got := mode(t, filepath.Join(dir, today(backup.DefaultPrefix))); got != 0o600 {
		t.Errorf("new backup file is %o, want 600", got)
	}
}

// An operator who opened the directory up did so on purpose.
func TestAnExistingBackupDirectoryKeepsItsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix permission bits")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := backup.New(&fakeSnap{}, backup.Options{Dir: dir}).WriteDaily(t.Context()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}
	if got := mode(t, dir); got != 0o755 {
		t.Errorf("existing directory became %o, want it left at 755", got)
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return fi.Mode().Perm()
}

// A future-dated name (a clock that once ran ahead, a copy from another host)
// sorts newest, so ranking by name used to delete each day's backup on write.
func TestAFutureDatedFileDoesNotCostTodaysBackup(t *testing.T) {
	dir := t.TempDir()
	future := filepath.Join(dir, "backup-2099-01-01.db")
	if err := os.WriteFile(future, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := backup.New(&fakeSnap{}, backup.Options{Dir: dir, Retention: 1})
	if err := s.WriteDaily(t.Context()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, today(backup.DefaultPrefix))); err != nil {
		t.Errorf("today's backup was pruned as it was written: %v", err)
	}
}

// Two services on one directory (two processes, or a restart overlapping a run)
// must not delete or publish each other's half-written snapshot.
func TestTwoServicesOnOneDirectoryDoNotClobberEachOther(t *testing.T) {
	dir := t.TempDir()
	started, release := make(chan struct{}), make(chan struct{})
	var aTemp string
	a := backup.New(snapFunc(func(_ context.Context, dst string) error {
		aTemp = dst
		if err := os.WriteFile(dst, []byte("half"), 0o600); err != nil {
			return err
		}
		close(started)
		<-release
		return os.WriteFile(dst, []byte("snapshot-a"), 0o600)
	}), backup.Options{Dir: dir})

	done := make(chan error, 1)
	go func() { done <- a.WriteDaily(t.Context()) }()
	<-started

	// B runs to completion while A is mid-snapshot.
	b := backup.New(snapFunc(func(_ context.Context, dst string) error {
		if dst == aTemp {
			t.Errorf("B snapshotted to A's temp name %q", dst)
		}
		return os.WriteFile(dst, []byte("snapshot-b"), 0o600)
	}), backup.Options{Dir: dir})
	if err := b.WriteDaily(t.Context()); err != nil {
		t.Fatalf("B WriteDaily: %v", err)
	}
	if _, err := os.Stat(aTemp); err != nil {
		t.Errorf("B removed A's snapshot in progress: %v", err)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("A WriteDaily: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, today(backup.DefaultPrefix)))
	if err != nil || !strings.HasPrefix(string(got), "snapshot-") {
		t.Errorf("today's backup = %q, %v; want a complete snapshot", got, err)
	}
	if left := names(t, dir); len(left) != 1 {
		t.Errorf("directory holds %v, want only today's backup", left)
	}
}
