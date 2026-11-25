package tasks_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/neodata-io/neokit/examples/reference/internal/app/tasks"
	"github.com/neodata-io/neokit/examples/reference/internal/domain"
)

// memRepo is the Repository as a map. A use case is tested against this, not
// against SQLite: what is under test is the rule, and the store has tests of
// its own.
type memRepo struct {
	byID map[string]domain.Task
	// beforeUpdate, when set, runs just before a write lands — the window in
	// which somebody else's edit can arrive.
	beforeUpdate func()
}

func newMemRepo() *memRepo { return &memRepo{byID: map[string]domain.Task{}} }

func (m *memRepo) CreateTask(_ context.Context, t *domain.Task) error {
	m.byID[t.ID] = *t
	return nil
}

func (m *memRepo) Task(_ context.Context, id string) (*domain.Task, error) {
	t, ok := m.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return &t, nil
}

func (m *memRepo) ListTasks(context.Context) ([]domain.Task, error) {
	out := []domain.Task{}
	for _, t := range m.byID {
		out = append(out, t)
	}
	return out, nil
}

func (m *memRepo) UpdateTask(_ context.Context, t *domain.Task) error {
	if m.beforeUpdate != nil {
		m.beforeUpdate()
	}
	if m.byID[t.ID].Version != t.Version {
		return domain.ErrConflict
	}
	t.Version++
	m.byID[t.ID] = *t
	return nil
}

func (m *memRepo) DeleteTask(_ context.Context, id string) error {
	if _, ok := m.byID[id]; !ok {
		return domain.ErrNotFound
	}
	delete(m.byID, id)
	return nil
}

func (m *memRepo) DeleteTasksDoneBefore(_ context.Context, cutoff time.Time) (int64, error) {
	var n int64
	for id, t := range m.byID {
		if t.CompletedAt != nil && t.CompletedAt.Before(cutoff) {
			delete(m.byID, id)
			n++
		}
	}
	return n, nil
}

type rig struct {
	svc  *tasks.Service
	repo *memRepo
	now  time.Time
}

func newRig() *rig {
	r := &rig{repo: newMemRepo(), now: time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)}
	next := 0
	r.svc = tasks.New(tasks.Deps{
		Repo:  r.repo,
		NewID: func() string { next++; return strconv.Itoa(next) },
		Now:   func() time.Time { return r.now },
	})
	return r
}

func TestCreateRefusesWhatTheDomainRefuses(t *testing.T) {
	r := newRig()
	if _, err := r.svc.Create(t.Context(), "  "); !errors.Is(err, domain.ErrInvalidInput) {
		t.Errorf("err = %v, want ErrInvalidInput", err)
	}
	if len(r.repo.byID) != 0 {
		t.Error("a refused task was stored anyway")
	}
}

func TestUpdateChangesOnlyWhatWasNamed(t *testing.T) {
	r := newRig()
	task, _ := r.svc.Create(t.Context(), "write the report")

	got, err := r.svc.Update(t.Context(), task.ID, tasks.Change{Done: new(true)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "write the report" || !got.Done || got.Version != 2 {
		t.Errorf("task = %+v, want the same title, done, at version 2", got)
	}
}

// The caller said which version they were editing, and it is not the current
// one: refused before anything is written.
func TestAnEditAgainstAnOldVersionIsStale(t *testing.T) {
	r := newRig()
	task, _ := r.svc.Create(t.Context(), "write the report")
	if _, err := r.svc.Update(t.Context(), task.ID, tasks.Change{Done: new(true)}); err != nil {
		t.Fatal(err)
	}

	_, err := r.svc.Update(t.Context(), task.ID, tasks.Change{Title: new("overwrite"), IfVersion: new(1)})
	if !errors.Is(err, domain.ErrStale) {
		t.Fatalf("err = %v, want ErrStale", err)
	}
	if r.repo.byID[task.ID].Title != "write the report" {
		t.Error("the stale edit was applied")
	}
}

// Nobody stated a version, and another write landed between the read and the
// write. That is the store's conflict, not the caller's precondition.
func TestAWriteThatLosesARaceIsAConflict(t *testing.T) {
	r := newRig()
	task, _ := r.svc.Create(t.Context(), "write the report")
	r.repo.beforeUpdate = func() {
		r.repo.beforeUpdate = nil
		other := r.repo.byID[task.ID]
		other.Version++
		r.repo.byID[task.ID] = other
	}

	_, err := r.svc.Update(t.Context(), task.ID, tasks.Change{Done: new(true)})
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("err = %v, want ErrConflict", err)
	}
}

func TestPurgeRemovesOnlyWhatWasDoneLongAgo(t *testing.T) {
	r := newRig()
	old, _ := r.svc.Create(t.Context(), "old and done")
	recent, _ := r.svc.Create(t.Context(), "recently done")
	open, _ := r.svc.Create(t.Context(), "old and open")

	r.svc.Update(t.Context(), old.ID, tasks.Change{Done: new(true)})
	r.now = r.now.Add(40 * 24 * time.Hour)
	r.svc.Update(t.Context(), recent.ID, tasks.Change{Done: new(true)})

	n, err := r.svc.PurgeDone(t.Context(), 30*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("purged %d (err %v), want 1", n, err)
	}
	if _, gone := r.repo.byID[old.ID]; gone {
		t.Error("the old completed task survived")
	}
	for _, id := range []string{recent.ID, open.ID} {
		if _, ok := r.repo.byID[id]; !ok {
			t.Errorf("task %s was purged and should not have been", id)
		}
	}
}
