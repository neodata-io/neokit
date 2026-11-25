package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/neodata-io/neokit/sqlitex"

	"github.com/neodata-io/neokit/examples/reference/internal/domain"
)

const taskColumns = `id, title, done, version, created_at, updated_at, completed_at`

// CreateTask stores a new task.
func (s *Store) CreateTask(ctx context.Context, t *domain.Task) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO task (id, title, done, version, created_at, updated_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Title, t.Done, t.Version, stamp(t.CreatedAt), stamp(t.UpdatedAt), stampPtr(t.CompletedAt))
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	return nil
}

// Task loads one task, or domain.ErrNotFound.
func (s *Store) Task(ctx context.Context, id string) (*domain.Task, error) {
	t, err := sqlitex.QueryOne(ctx, s.db, `SELECT `+taskColumns+` FROM task WHERE id = ?`, []any{id}, scanTask)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load task: %w", err)
	}
	return &t, nil
}

// ListTasks returns every task, newest first. Never nil: the handler sends it
// as is, and a list is `[]`, not `null`.
func (s *Store) ListTasks(ctx context.Context) ([]domain.Task, error) {
	out, err := sqlitex.Query(ctx, s.db,
		`SELECT `+taskColumns+` FROM task ORDER BY created_at DESC, rowid DESC`, nil, scanTask)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	if out == nil {
		out = []domain.Task{}
	}
	return out, nil
}

// UpdateTask writes t if nobody else has written since it was loaded.
//
// The version is in the WHERE clause, so the check and the write are one
// statement: there is no moment between "it is still version 3" and "now it is
// version 4" for another writer to land in. On success t.Version is the new
// one, so the caller can hand the task straight back to a client.
func (s *Store) UpdateTask(ctx context.Context, t *domain.Task) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE task SET title = ?, done = ?, updated_at = ?, completed_at = ?, version = version + 1
		WHERE id = ? AND version = ?`,
		t.Title, t.Done, stamp(t.UpdatedAt), stampPtr(t.CompletedAt), t.ID, t.Version)
	if err != nil {
		return fmt.Errorf("update task: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update task: %w", err)
	}
	if n == 0 {
		// Gone, or moved on. Say which: the first is a 404 and the second a 409,
		// and a client does different things with them.
		if _, err := s.Task(ctx, t.ID); err != nil {
			return err
		}
		return domain.ErrConflict
	}
	t.Version++
	return nil
}

// DeleteTask removes a task, or answers domain.ErrNotFound — so a handler says
// 404 for an id that does not exist instead of a 204 claiming it was removed.
func (s *Store) DeleteTask(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM task WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	return sqlitex.CheckAffected(res)
}

// DeleteTasksDoneBefore removes tasks completed before cutoff and reports how
// many went.
func (s *Store) DeleteTasksDoneBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM task WHERE completed_at < ?`, stamp(cutoff))
	if err != nil {
		return 0, fmt.Errorf("purge tasks: %w", err)
	}
	return res.RowsAffected()
}

func scanTask(r *sql.Rows) (domain.Task, error) {
	var (
		t                domain.Task
		created, updated string
		completed        sql.NullString
	)
	if err := r.Scan(&t.ID, &t.Title, &t.Done, &t.Version, &created, &updated, &completed); err != nil {
		return domain.Task{}, err
	}
	var err error
	if t.CreatedAt, err = time.Parse(time.RFC3339, created); err != nil {
		return domain.Task{}, fmt.Errorf("task %s created_at: %w", t.ID, err)
	}
	if t.UpdatedAt, err = time.Parse(time.RFC3339, updated); err != nil {
		return domain.Task{}, fmt.Errorf("task %s updated_at: %w", t.ID, err)
	}
	if completed.Valid {
		at, err := time.Parse(time.RFC3339, completed.String)
		if err != nil {
			return domain.Task{}, fmt.Errorf("task %s completed_at: %w", t.ID, err)
		}
		t.CompletedAt = &at
	}
	return t, nil
}
