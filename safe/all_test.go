package safe

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAll_ReturnsResultsInItemOrder(t *testing.T) {
	items := []int{3, 1, 2}
	results, ok := All(t.Context(), items, 0, "square", func(_ context.Context, n int) (string, error) {
		return strconv.Itoa(n * n), nil
	})
	assert.Equal(t, []string{"9", "1", "4"}, results)
	assert.Equal(t, []bool{true, true, true}, ok)
}

func TestAll_AFailingItemIsAbsentNotFatal(t *testing.T) {
	items := []string{"a", "b", "c"}
	results, ok := All(t.Context(), items, 0, "fetch", func(_ context.Context, s string) ([]string, error) {
		if s == "b" {
			return nil, errors.New("down")
		}
		return []string{s + "!"}, nil
	})
	assert.Equal(t, [][]string{{"a!"}, nil, {"c!"}}, results)
	assert.Equal(t, []bool{true, false, true}, ok)
}

func TestAll_APanickingItemIsAbsentNotFatal(t *testing.T) {
	items := []int{1, 2}
	results, ok := All(t.Context(), items, 0, "fetch", func(_ context.Context, n int) (int, error) {
		if n == 2 {
			panic("source exploded")
		}
		return n, nil
	})
	assert.Equal(t, []int{1, 0}, results)
	assert.Equal(t, []bool{true, false}, ok)
}

func TestAll_BoundsEachItemSeparately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		items := []string{"fast", "hung"}
		start := time.Now()
		results, ok := All(t.Context(), items, time.Second, "fetch", func(ctx context.Context, s string) (string, error) {
			if s == "hung" {
				<-ctx.Done() // a source that never answers only ends on its own deadline
				return "", ctx.Err()
			}
			return s, nil
		})
		assert.Equal(t, []string{"fast", ""}, results)
		assert.Equal(t, []bool{true, false}, ok)
		assert.Equal(t, time.Second, time.Since(start), "the hung source cost exactly its own timeout")
	})
}

func TestAll_EmptyInputReturnsEmptyNotNil(t *testing.T) {
	results, ok := All(t.Context(), []int(nil), 0, "none", func(context.Context, int) (int, error) { return 0, nil })
	assert.NotNil(t, results)
	assert.Empty(t, results)
	assert.Empty(t, ok)
}
