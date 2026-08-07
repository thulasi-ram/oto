package validate

import (
	"errors"
	"reflect"
	"strings"
	"sync"

	"github.com/go-playground/validator/v10"

	"github.com/thulasiram/oto/internal/platform/errs"
)

var (
	once sync.Once
	v    *validator.Validate
)

// Validator returns the process-wide validator, configured to report the JSON
// field name rather than the Go field name so that error bodies name what the
// caller actually sent.
func Validator() *validator.Validate {
	once.Do(func() {
		v = validator.New(validator.WithRequiredStructEnabled())
		v.RegisterTagNameFunc(func(f reflect.StructField) string {
			name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
			if name == "-" {
				return ""
			}
			if name == "" {
				return f.Name
			}
			return name
		})
	})
	return v
}

// Struct validates a DTO at the API boundary and returns an errs.Error carrying
// one FieldError per failure. Domain invariants are NOT validated here: they are
// hand-written in the relevant domain package.
func Struct(dto any) error {
	if err := Validator().Struct(dto); err != nil {
		var verrs validator.ValidationErrors
		if !errors.As(err, &verrs) {
			return errs.Wrap(errs.CodeValidationFailed, err, "validation failed")
		}
		out := errs.New(errs.CodeValidationFailed, "one or more fields are invalid")
		for _, fe := range verrs {
			out.Fields = append(out.Fields, errs.FieldError{
				Field: fieldPath(fe),
				Code:  fe.Tag(),
			})
		}
		return out
	}
	return nil
}

func fieldPath(fe validator.FieldError) string {
	ns := fe.Namespace()
	if i := strings.Index(ns, "."); i >= 0 {
		return ns[i+1:]
	}
	return fe.Field()
}
