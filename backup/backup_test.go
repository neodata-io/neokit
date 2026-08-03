package backup_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
		if err := s.WriteDaily(context.Background()); err != nil {
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
	if err := s.WriteDaily(context.Background()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}

	got, err := s.List(context.Background())
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
	if err := s.WriteDaily(context.Background()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}
	got, err := s.List(context.Background())
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
	if err := s.WriteDaily(context.Background()); err != nil {
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
	if err := s.Backup(context.Background(), &buf); err != nil {
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
	got, err := s.List(context.Background())
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

	if err := s.WriteDaily(context.Background()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}
	got, err := s.List(context.Background())
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
	if err := s.WriteDaily(context.Background()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}
	got, err := s.List(context.Background())
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
	if err := s.WriteDaily(context.Background()); err != nil {
		t.Fatalf("WriteDaily: %v", err)
	}
	got, _ := s.List(context.Background())
	if len(got) != 1 || !strings.HasPrefix(got[0].Name, backup.DefaultPrefix) {
		t.Errorf("List = %+v, want the default prefix", got)
	}
}

// Run has to have something behind it. Configuring "daily, keep 7" and
// scheduling nothing is the bug this replaces, so the assertion is the file.
func TestRunWritesABackup(t *testing.T) {
	dir := t.TempDir()
	svc := backup.New(&fakeSnap{}, backup.Options{Dir: dir, Retention: 7})

	ctx, cancel := context.WithCancel(context.Background())
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
	go func() { defer close(done); svc.Run(context.Background()) }()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run must return immediately when no backup directory is configured")
	}
}
