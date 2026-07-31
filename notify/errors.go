package notify

import "errors"

// joinErrors returns nil for an empty slice and the joined error otherwise.
// errors.Join already does this, but going through one helper keeps [Multi]'s
// success path an explicit `return nil` rather than relying on the reader
// knowing that Join of nothing is nil.
func joinErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}
