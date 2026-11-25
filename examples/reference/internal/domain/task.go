// Package domain is the innermost ring: what a task is and what may happen to
// one. It imports the standard library and neokit's errs, and nothing else —
// no database, no HTTP, no framework. Everything else in the service depends on
// this package; this package depends on none of it.
package domain

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/neodata-io/neokit/errs"
)

// The three failures every service has are neokit's own values, not copies. A
// copy compiles, reads the same, and never matches errors.Is — every "not
// found" would then leave as a 500. Aliased here so the inner rings can name
// them without knowing where the HTTP layer looks.
var (
	ErrNotFound     = errs.ErrNotFound
	ErrInvalidInput = errs.ErrInvalidInput
	ErrConflict     = errs.ErrConflict
)

// ErrStale is a sentinel that is genuinely this service's: the caller edited a
// version of the task that is no longer the current one. The HTTP adapter maps
// it to 412 — see httpx.MapError in cmd/api.
var ErrStale = errors.New("task was changed since it was loaded")

// TitleMaxLen is the longest title a task can carry, in characters.
const TitleMaxLen = 200

// Task is the aggregate. Its fields are exported so a store can load one, but
// every change goes through a method, which is where the rules live.
type Task struct {
	ID    string
	Title string
	Done  bool
	// Version counts writes. A store bumps it on every update and refuses one
	// made against an older value, which is what turns two people editing the
	// same task into a conflict instead of a silent overwrite.
	Version     int
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

// NewTask makes a task. The id and the clock are parameters: a domain type that
// reaches for uuid.New or time.Now cannot be tested without them.
func NewTask(id, title string, now time.Time) (*Task, error) {
	title, err := cleanTitle(title)
	if err != nil {
		return nil, err
	}
	now = now.UTC().Truncate(time.Second)
	return &Task{ID: id, Title: title, Version: 1, CreatedAt: now, UpdatedAt: now}, nil
}

// Rename changes the title.
func (t *Task) Rename(title string, now time.Time) error {
	title, err := cleanTitle(title)
	if err != nil {
		return err
	}
	t.Title = title
	t.touch(now)
	return nil
}

// SetDone completes or reopens the task. Setting it to what it already is
// changes nothing — including the completion time, which records when the task
// was first finished, not when someone last said so.
func (t *Task) SetDone(done bool, now time.Time) {
	if t.Done == done {
		return
	}
	t.Done = done
	t.CompletedAt = nil
	if done {
		at := now.UTC().Truncate(time.Second)
		t.CompletedAt = &at
	}
	t.touch(now)
}

func (t *Task) touch(now time.Time) { t.UpdatedAt = now.UTC().Truncate(time.Second) }

// cleanTitle is the one place a title is judged. The error wraps
// ErrInvalidInput, so it reaches a client as a 400 without any layer in
// between having to recognise it.
func cleanTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	switch {
	case title == "":
		return "", errors.Join(ErrInvalidInput, errors.New("a task needs a title"))
	case utf8.RuneCountInString(title) > TitleMaxLen:
		return "", errors.Join(ErrInvalidInput, errors.New("the title is too long"))
	}
	return title, nil
}
