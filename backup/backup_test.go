package backup_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	s := backup.New(snap, dir, 7)

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
	s := backup.New(&fakeSnap{}, dir, 3)
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
	s := backup.New(&fakeSnap{}, dir, 0)
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
	s := backup.New(snap, "", 7)
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
	s := backup.New(snap, t.TempDir(), 7)

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
	s := backup.New(&fakeSnap{}, dir, 10)
	got, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 || got[0].Name != "backup-2026-01-03.db" {
		t.Errorf("List = %+v, want newest-dated first", got)
	}
}
