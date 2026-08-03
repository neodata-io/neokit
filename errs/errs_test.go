package errs_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/neodata-io/neokit/errs"
)

// Each sentinel must be its own value. If two were the same, errors.Is would
// cross-match and a "conflict" would render as a 404.
func TestSentinelsAreDistinct(t *testing.T) {
	all := map[string]error{
		"not found":     errs.ErrNotFound,
		"invalid input": errs.ErrInvalidInput,
		"conflict":      errs.ErrConflict,
	}
	for aName, a := range all {
		for bName, b := range all {
			if aName == bName {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("%s matches %s — they must be distinct values", aName, bName)
			}
		}
	}
}

// The sentinels are always wrapped in practice ("user %q: %w"), so the wrapped
// form is the one that has to match.
func TestSentinelsSurviveWrapping(t *testing.T) {
	for _, sentinel := range []error{errs.ErrNotFound, errs.ErrInvalidInput, errs.ErrConflict} {
		wrapped := fmt.Errorf("user %q: %w", "ruben", sentinel)
		if !errors.Is(wrapped, sentinel) {
			t.Errorf("errors.Is failed for wrapped %v", sentinel)
		}
	}
}
