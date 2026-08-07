package domain

import "github.com/thulasiram/oto/internal/platform/errs"

// errInvalid mints the module's validation error. It exists so that every
// invariant in this package produces one shape, with a stable, greppable code.
func errInvalid(code, message string) error {
	return errs.New(errs.KindValidation, code, message)
}
