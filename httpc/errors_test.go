package httpc

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestIsConflict(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"409 direct", &APIError{Service: "immich", StatusCode: http.StatusConflict}, true},
		{"409 wrapped", fmt.Errorf("create: %w", &APIError{StatusCode: http.StatusConflict}), true},
		{"404", &APIError{StatusCode: http.StatusNotFound}, false},
		{"400", &APIError{StatusCode: http.StatusBadRequest}, false},
		{"plain error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsConflict(tc.err); got != tc.want {
				t.Errorf("IsConflict(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A plugin wraps ErrAlreadyExists so the host can recognise an identity clash
// with errors.Is regardless of the surrounding message.
func TestErrAlreadyExists_Wrapped(t *testing.T) {
	err := fmt.Errorf("jellyfin: the username %q is already taken: %w", "john", ErrAlreadyExists)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Error("wrapped error should match ErrAlreadyExists via errors.Is")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("ErrAlreadyExists must not be confused with ErrNotFound")
	}
}
