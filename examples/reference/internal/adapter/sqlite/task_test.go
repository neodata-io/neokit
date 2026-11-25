package sqlite_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/neodata-io/neokit/examples/reference/internal/adapter/sqlite"
	"github.com/neodata-io/neokit/examples/reference/internal/app/tasks"
	"github.com/neodata-io/neokit/examples/reference/internal/domain"
)

// The store is tested against a real database file. A fake of SQL proves that
// the fake agrees with itself.
func openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// Fails to compile the day the store stops satisfying what the service asks
// for, rather than at the composition root.
var _ tasks.Repository = (*sqlite.Store)(nil)

var t0 = time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)

func TestATaskRoundTrips(t *testing.T) {
	s := openStore(t)
	in, _ := domain.NewTask("a", "write the report", t0)
	in.SetDone(true, t0.Add(time.Hour))
	if err := s.CreateTask(t.Context(), in); err != nil {
		t.Fatal(err)
	}

	out, err := s.Task(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if out.Title != in.Title || !out.Done || out.Version != 1 ||
		!out.CreatedAt.Equal(in.CreatedAt) || out.CompletedAt == nil || !out.CompletedAt.Equal(*in.CompletedAt) {
		t.Errorf("out = %+v, want %+v", out, in)
	}
}

func TestMissingRowsAreNotFound(t *testing.T) {
	s := openStore(t)
	if _, err := s.Task(t.Context(), "ghost"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Task: %v, want ErrNotFound", err)
	}
	if err := s.DeleteTask(t.Context(), "ghost"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("DeleteTask: %v, want ErrNotFound", err)
	}
	ghost, _ := domain.NewTask("ghost", "x", t0)
	if err := s.UpdateTask(t.Context(), ghost); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("UpdateTask: %v, want ErrNotFound", err)
	}
}

func TestAnEmptyListIsNotNil(t *testing.T) {
	list, err := openStore(t).ListTasks(t.Context())
	if err != nil || list == nil {
		t.Errorf("list = %#v (err %v), want an empty non-nil slice", list, err)
	}
}

// Two copies of the same task, loaded at the same version. The first write
// wins; the second is told, instead of quietly replacing it.
func TestTheSecondOfTwoWritersIsRefused(t *testing.T) {
	s := openStore(t)
	task, _ := domain.NewTask("a", "original", t0)
	if err := s.CreateTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	first, _ := s.Task(t.Context(), "a")
	second, _ := s.Task(t.Context(), "a")

	first.Rename("first", t0)
	if err := s.UpdateTask(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if first.Version != 2 {
		t.Errorf("version after the write = %d, want 2", first.Version)
	}

	second.Rename("second", t0)
	if err := s.UpdateTask(t.Context(), second); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second writer: %v, want ErrConflict", err)
	}
	if now, _ := s.Task(t.Context(), "a"); now.Title != "first" {
		t.Errorf("title = %q, want the first writer's", now.Title)
	}
}

func TestPurgeLeavesOpenAndRecentTasks(t *testing.T) {
	s := openStore(t)
	for id, doneAt := range map[string]time.Duration{"old": time.Hour, "recent": 40 * 24 * time.Hour, "open": 0} {
		task, _ := domain.NewTask(id, id, t0)
		if doneAt > 0 {
			task.SetDone(true, t0.Add(doneAt))
		}
		if err := s.CreateTask(t.Context(), task); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.DeleteTasksDoneBefore(t.Context(), t0.Add(30*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("purged %d (err %v), want 1", n, err)
	}
	left, _ := s.ListTasks(t.Context())
	if len(left) != 2 {
		t.Errorf("%d tasks left, want the open one and the recent one", len(left))
	}
}
