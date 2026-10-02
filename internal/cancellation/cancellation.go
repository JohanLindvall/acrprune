// Package cancellation distinguishes an intentional cancellation from failures
// that happen alongside it.
package cancellation

import "context"

// Only recognizes cancellation through wrappers and joined errors without
// hiding a separate failure. A deadline is a failure, not a user cancellation.
func Only(err error) bool {
	if err == context.Canceled {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() error }:
		return Only(wrapped.Unwrap())
	case interface{ Unwrap() []error }:
		errs := wrapped.Unwrap()
		for _, inner := range errs {
			if !Only(inner) {
				return false
			}
		}
		return len(errs) > 0
	}
	return false
}
