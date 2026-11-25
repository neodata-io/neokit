// Package taskshttp is the HTTP adapter for tasks: it turns a request into a
// call on the service and the answer into a response, and holds no rule of its
// own.
//
// One package per module, each with one Register. Module packages never import
// each other — they share only what sits below them (the domain, fiberx) — so
// adding a module is adding a directory and one line in the composition root.
package taskshttp

import (
	"context"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/examples/reference/internal/app/tasks"
	"github.com/neodata-io/neokit/examples/reference/internal/domain"
)

// Tasks is what the handlers need from the service — an interface declared
// here, so a handler test passes a fake and the package does not depend on how
// the service is built.
type Tasks interface {
	Create(ctx context.Context, title string) (*domain.Task, error)
	Get(ctx context.Context, id string) (*domain.Task, error)
	List(ctx context.Context) ([]domain.Task, error)
	Update(ctx context.Context, id string, c tasks.Change) (*domain.Task, error)
	Delete(ctx context.Context, id string) error
}

// Deps is everything the module is handed. A named struct, so the call in the
// composition root reads as a list of what this module can reach.
type Deps struct {
	Tasks Tasks

	// Member and Owner are the guards for "anyone signed in" and "an owner".
	// The module decides which of its routes take which — that knowledge lives
	// with the routes, not in a central list of paths that drifts from them.
	// Both pass everything through while no login is configured.
	Member fiber.Handler
	Owner  fiber.Handler
}

// Register mounts the module on api, which is already at the versioned base:
// routes here never name "/api/v1". Hand it a.API, not a.HTTP — both satisfy
// fiber.Router, and the wrong one mounts everything at the root.
func Register(api fiber.Router, d Deps) {
	h := newHandler(d.Tasks)

	api.Get("/tasks", d.Member, h.list)
	api.Post("/tasks", d.Member, h.create)
	api.Get("/tasks/:id", d.Member, h.get)
	api.Patch("/tasks/:id", d.Member, h.update)
	api.Delete("/tasks/:id", d.Owner, h.delete)
}
