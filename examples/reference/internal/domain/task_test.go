package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/neodata-io/neokit/examples/reference/internal/domain"
)

var t0 = time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)

func TestATaskNeedsAUsableTitle(t *testing.T) {
	for name, title := range map[string]string{
		"empty":    "",
		"blank":    "   ",
		"too long": strings.Repeat("é", domain.TitleMaxLen+1),
	} {
		if _, err := domain.NewTask("1", title, t0); !errors.Is(err, domain.ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", name, err)
		}
	}
	// The limit counts characters, not bytes.
	task, err := domain.NewTask("1", "  "+strings.Repeat("é", domain.TitleMaxLen)+"  ", t0)
	if err != nil {
		t.Fatalf("a title at the limit: %v", err)
	}
	if strings.HasPrefix(task.Title, " ") || task.Version != 1 {
		t.Errorf("task = %+v, want a trimmed title at version 1", task)
	}
}

// The completion time records when the task was first finished. Saying "done"
// again must not move it.
func TestCompletingTwiceKeepsTheFirstCompletion(t *testing.T) {
	task, _ := domain.NewTask("1", "write the report", t0)

	task.SetDone(true, t0.Add(time.Hour))
	first := *task.CompletedAt
	task.SetDone(true, t0.Add(2*time.Hour))

	if !task.CompletedAt.Equal(first) || !task.UpdatedAt.Equal(first) {
		t.Errorf("completedAt = %v, updatedAt = %v; want both still %v", task.CompletedAt, task.UpdatedAt, first)
	}

	task.SetDone(false, t0.Add(3*time.Hour))
	if task.Done || task.CompletedAt != nil {
		t.Errorf("a reopened task still reads as completed: %+v", task)
	}
}
