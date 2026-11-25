package taskshttp

import (
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/fiberx"

	"github.com/neodata-io/neokit/examples/reference/internal/app/tasks"
	"github.com/neodata-io/neokit/examples/reference/internal/domain"
)

type handler struct {
	tasks    Tasks
	validate *validator.Validate
}

func newHandler(t Tasks) *handler {
	return &handler{tasks: t, validate: fiberx.NewValidator()}
}

// taskView is a task on the wire. Its own type, not the domain's with json
// tags: what the API promises and what the domain holds change for different
// reasons, and the version travels as an ETag rather than a field.
type taskView struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Done        bool    `json:"done"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
	CompletedAt *string `json:"completedAt"`
}

func viewOf(t domain.Task) taskView {
	v := taskView{
		ID: t.ID, Title: t.Title, Done: t.Done,
		CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: t.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if t.CompletedAt != nil {
		at := t.CompletedAt.UTC().Format(time.RFC3339)
		v.CompletedAt = &at
	}
	return v
}

// A handler returns the service's error as it stands. The app's error handler
// turns it into the {"error","code"} envelope — 404 for ErrNotFound, 400 for
// ErrInvalidInput, 412 for this service's own ErrStale — so no handler chooses
// a status for a failure, and none can choose a different one than its
// neighbour did.

func (h *handler) list(c fiber.Ctx) error {
	list, err := h.tasks.List(c.Context())
	if err != nil {
		return err
	}
	views := make([]taskView, 0, len(list))
	for _, t := range list {
		views = append(views, viewOf(t))
	}
	return c.JSON(views)
}

type createRequest struct {
	Title string `json:"title" validate:"required,max=200"`
}

func (h *handler) create(c fiber.Ctx) error {
	var req createRequest
	if err := fiberx.BindAndValidate(c, h.validate, &req); err != nil {
		return err
	}
	t, err := h.tasks.Create(c.Context(), req.Title)
	if err != nil {
		return err
	}
	fiberx.SetVersionETag(c, t.Version)
	return fiberx.Created(c, fiberx.MemberURL(c, t.ID), viewOf(*t))
}

func (h *handler) get(c fiber.Ctx) error {
	t, err := h.tasks.Get(c.Context(), fiberx.PathParam(c, "id"))
	if err != nil {
		return err
	}
	fiberx.SetVersionETag(c, t.Version)
	return c.JSON(viewOf(*t))
}

// updateRequest is a partial edit: an absent field is left alone, which is why
// both are pointers.
type updateRequest struct {
	Title *string `json:"title" validate:"omitempty,min=1,max=200"`
	Done  *bool   `json:"done"`
}

func (h *handler) update(c fiber.Ctx) error {
	var req updateRequest
	if err := fiberx.BindAndValidate(c, h.validate, &req); err != nil {
		return err
	}
	change := tasks.Change{Title: req.Title, Done: req.Done}
	// If-Match is opt-in: a client that sends the ETag it was given is protected
	// from overwriting an edit it never saw, and one that does not is not refused.
	if version, conditional := fiberx.IfMatchVersion(c); conditional {
		change.IfVersion = &version
	}
	t, err := h.tasks.Update(c.Context(), fiberx.PathParam(c, "id"), change)
	if err != nil {
		return err
	}
	fiberx.SetVersionETag(c, t.Version)
	return c.JSON(viewOf(*t))
}

// A command with nothing to return answers 204, never {"ok": true}.
func (h *handler) delete(c fiber.Ctx) error {
	if err := h.tasks.Delete(c.Context(), fiberx.PathParam(c, "id")); err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}
