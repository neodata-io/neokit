// Package tasks holds the use cases: what the service does with tasks, in terms
// of the domain and of the ports it declares for itself.
//
// It knows there is a store, by the interface below, and does not know which
// one. It does not know there is HTTP at all — no status codes, no request
// types — which is what lets a CLI, a scheduler and a handler call the same
// method and mean the same thing.
package tasks

import (
	"context"
	"time"

	"github.com/neodata-io/neokit/examples/reference/internal/domain"
)

// Repository is what this service needs from storage — declared here, by the
// consumer, and only as wide as the methods below use. The SQLite store
// satisfies it without naming it, and a test passes a map.
type Repository interface {
	CreateTask(ctx context.Context, t *domain.Task) error
	// Task answers domain.ErrNotFound for an id that does not exist.
	Task(ctx context.Context, id string) (*domain.Task, error)
	ListTasks(ctx context.Context) ([]domain.Task, error)
	// UpdateTask writes t if the stored version is still t.Version, bumping it;
	// otherwise it answers domain.ErrConflict.
	UpdateTask(ctx context.Context, t *domain.Task) error
	DeleteTask(ctx context.Context, id string) error
	DeleteTasksDoneBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// Deps is what a Service is built from. A struct rather than a parameter list:
// three arguments of which two are functions read badly at the call site, and
// the fourth one added later would change every caller.
type Deps struct {
	Repo Repository
	// NewID mints a task id.
	NewID func() string
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Service is the task use cases.
type Service struct {
	repo  Repository
	newID func() string
	now   func() time.Time
}

// New builds the service.
func New(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Service{repo: d.Repo, newID: d.NewID, now: d.Now}
}

// Create adds a task.
func (s *Service) Create(ctx context.Context, title string) (*domain.Task, error) {
	t, err := domain.NewTask(s.newID(), title, s.now())
	if err != nil {
		return nil, err
	}
	if err := s.repo.CreateTask(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

// Get returns one task.
func (s *Service) Get(ctx context.Context, id string) (*domain.Task, error) {
	return s.repo.Task(ctx, id)
}

// List returns every task, newest first.
func (s *Service) List(ctx context.Context) ([]domain.Task, error) {
	return s.repo.ListTasks(ctx)
}

// Change is a partial edit: a nil field is left as it is.
type Change struct {
	Title *string
	Done  *bool
	// IfVersion, when set, is the version the caller last saw. An edit made
	// against an older one is refused with domain.ErrStale rather than applied
	// over a change the caller never read.
	IfVersion *int
}

// Update applies a change.
//
// Two different things can go wrong with a concurrent edit, and they are kept
// apart on purpose. ErrStale is the caller's precondition failing: they said
// which version they were editing and it is not the current one. ErrConflict is
// the store's: nobody stated a version, and another write landed between this
// method's read and its write. The first is the caller's to resolve by
// reloading; the second is safe to retry as is.
func (s *Service) Update(ctx context.Context, id string, c Change) (*domain.Task, error) {
	t, err := s.repo.Task(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.IfVersion != nil && *c.IfVersion != t.Version {
		return nil, domain.ErrStale
	}

	now := s.now()
	if c.Title != nil {
		if err := t.Rename(*c.Title, now); err != nil {
			return nil, err
		}
	}
	if c.Done != nil {
		t.SetDone(*c.Done, now)
	}
	if err := s.repo.UpdateTask(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

// Delete removes a task.
func (s *Service) Delete(ctx context.Context, id string) error {
	return s.repo.DeleteTask(ctx, id)
}

// PurgeDone removes tasks completed longer ago than keep, and reports how many
// went. The nightly job calls it; nothing about it is a job's business.
func (s *Service) PurgeDone(ctx context.Context, keep time.Duration) (int64, error) {
	return s.repo.DeleteTasksDoneBefore(ctx, s.now().Add(-keep))
}
