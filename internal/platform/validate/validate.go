package validate

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/go-playground/validator/v10"

	"github.com/thulasiram/oto/internal/platform/errs"
)

var (
	once     sync.Once
	instance *validator.Validate
	initErr  error
)

// Validator returns the process-wide *validator.Validate (SPEC §L.2.1).
//
// It is configured with RegisterTagNameFunc so that EVERY reported field path is
// the JSON name the caller actually sent, never the Go field name. That is not
// optional: a violation naming `EscalateAfterS` instead of
// `escalate_after_seconds` cannot be mapped onto a form control (§L.8.2).
//
// The oto-specific rules of §L.2.1 are registered here, once. A rule registered
// anywhere else is invisible to the tag→code map and would surface as `invalid`.
func Validator() *validator.Validate {
	once.Do(build)
	if initErr != nil {
		panic("validate: " + initErr.Error())
	}
	return instance
}

func build() {
	val := validator.New(validator.WithRequiredStructEnabled())

	// Field paths in errors MUST be JSON names. This is not optional.
	val.RegisterTagNameFunc(func(f reflect.StructField) string {
		name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
		if name == "-" || name == "" {
			return f.Name
		}
		return name
	})

	rules := map[string]validator.Func{
		"labelname":  isPrometheusLabelName,
		"matcherop":  isMatcherOp,
		"cursor":     isOpaqueCursor,
		"notblank":   isNotBlank,
		"httpurl":    isAbsoluteHTTPURL,
		"clusterkey": isClusterKey,
	}
	for tag, fn := range rules {
		if err := val.RegisterValidation(tag, fn); err != nil {
			initErr = fmt.Errorf("register %q: %w", tag, err)
			return
		}
	}

	instance = val
}

// Struct validates a DTO at the API boundary and returns a KindValidation
// errs.Error carrying one Violation per failure, with JSON-name paths.
//
// This is the only entry point. No handler calls it directly: httpx owns the
// single door that decodes and then validates, so that no handler can forget
// (SPEC §L.2.1, TestNoDirectDecode).
//
// It validates NOTHING about domain invariants. Layer 1 exists to produce a good
// error message; layer 3 exists to make illegal states unrepresentable.
func Struct(dto any) error {
	err := Validator().Struct(dto)
	if err == nil {
		return nil
	}

	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) {
		// An InvalidValidationError means we were handed a non-struct: a bug here,
		// never the caller's fault.
		return errs.Wrap(err, errs.KindInternal, "validator_misuse", "an internal error occurred")
	}

	out := errs.Validation("validation_failed", pluralise(len(verrs)))
	for _, fe := range verrs {
		code, message := describe(fe)
		out.Violations = append(out.Violations, errs.Violation{
			Field:   FieldPath(fe),
			Code:    code,
			Message: message,
		})
	}
	return out
}

// Var validates a single value against a tag list, for query parameters that have
// no struct to hang a tag on. Field names the parameter in the violation.
func Var(field string, value any, tag string) error {
	err := Validator().Var(value, tag)
	if err == nil {
		return nil
	}

	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) {
		return errs.Wrap(err, errs.KindInternal, "validator_misuse", "an internal error occurred")
	}

	out := errs.Validation("validation_failed", pluralise(len(verrs)))
	for _, fe := range verrs {
		code, message := describeAs(field, fe)
		out.Violations = append(out.Violations, errs.Violation{Field: field, Code: code, Message: message})
	}
	return out
}

// FieldPath renders a validator namespace as the binding §L.2.2 path: JSON names,
// '/'-separated, array indices as numeric segments, map keys verbatim.
// `CreatePolicyRequest.matchers[0].name` becomes `matchers/0/name`.
func FieldPath(fe validator.FieldError) string {
	ns := fe.Namespace()
	if i := strings.IndexByte(ns, '.'); i >= 0 {
		ns = ns[i+1:] // drop the root struct name
	}
	if ns == "" {
		return fe.Field()
	}

	var b strings.Builder
	b.Grow(len(ns))
	for i := 0; i < len(ns); i++ {
		switch ns[i] {
		case '.':
			b.WriteByte('/')
		case '[':
			b.WriteByte('/')
		case ']':
			// The closing bracket becomes nothing; the next '.' or '[' supplies
			// the separator.
		default:
			b.WriteByte(ns[i])
		}
	}
	return strings.Trim(b.String(), "/")
}

func pluralise(n int) string {
	if n == 1 {
		return "1 field failed validation."
	}
	return fmt.Sprintf("%d fields failed validation.", n)
}

func describe(fe validator.FieldError) (code, message string) {
	return describeAs(fe.Field(), fe)
}

// describeAs maps a validator tag onto the closed code set of SPEC §L.2.3 and
// renders its message template. An unmapped tag yields `invalid`, which is a SPEC
// gap by definition — TestEveryTagHasACode fails on it.
func describeAs(field string, fe validator.FieldError) (code, message string) {
	tag, param := fe.Tag(), fe.Param()

	switch tag {
	case "required", "required_if", "required_with", "required_without",
		"required_with_all", "required_without_all", "required_unless":
		return "required", field + " is required"
	case "notblank":
		return "not_blank", field + " must not be blank"
	case "min":
		switch collectionKind(fe) {
		case kindString:
			return "min_length", fmt.Sprintf("%s must have at least %s characters", field, param)
		case kindCollection:
			return "min_items", fmt.Sprintf("%s must contain at least %s items", field, param)
		default:
			return "min", fmt.Sprintf("%s must be >= %s", field, param)
		}
	case "max":
		switch collectionKind(fe) {
		case kindString:
			return "max_length", fmt.Sprintf("%s must have at most %s characters", field, param)
		case kindCollection:
			return "max_items", fmt.Sprintf("%s must contain at most %s items", field, param)
		default:
			return "max", fmt.Sprintf("%s must be <= %s", field, param)
		}
	case "len":
		return "exact_length", fmt.Sprintf("%s must be exactly %s long", field, param)
	case "oneof":
		return "enum", fmt.Sprintf("%s must be one of: %s", field, strings.ReplaceAll(param, " ", ", "))
	case "uuid", "uuid4", "uuid7":
		return "uuid", field + " must be a UUID"
	case "email":
		return "email", field + " must be a valid email address"
	case "url", "httpurl":
		return "url", field + " must be an absolute http(s) URL"
	case "gt", "gte", "lt", "lte":
		return tag, fmt.Sprintf("%s must be %s %s", field, comparator(tag), param)
	case "ltefield", "gtefield", "ltfield", "gtfield":
		return "field_order", fmt.Sprintf("%s must be %s %s", field, comparator(strings.TrimSuffix(tag, "field")), param)
	case "unique":
		return "duplicate_items", field + " must not contain duplicates"
	case "labelname":
		return "labelname", "must be a valid Prometheus label name"
	case "matcherop":
		return "matcher_op", "must be one of: =, !=, =~, !~"
	case "clusterkey":
		return "cluster_key", "must match " + PatternClusterKey
	case "cursor":
		return "cursor", "cursor is not valid for the current filter"
	default:
		return "invalid", field + " is invalid"
	}
}

func comparator(tag string) string {
	switch tag {
	case "gt":
		return ">"
	case "gte":
		return ">="
	case "lt":
		return "<"
	case "lte":
		return "<="
	default:
		return tag
	}
}

type valueKind int

const (
	kindNumeric valueKind = iota
	kindString
	kindCollection
)

// collectionKind decides whether `min`/`max` meant a bound on a number, a string
// length or an item count. §L.2.3 gives each its own code.
func collectionKind(fe validator.FieldError) valueKind {
	switch fe.Kind() {
	case reflect.String:
		return kindString
	case reflect.Slice, reflect.Array, reflect.Map:
		return kindCollection
	default:
		return kindNumeric
	}
}

// isPrometheusLabelName implements the `labelname` rule (SPEC §L.2.4, bound B9).
func isPrometheusLabelName(fl validator.FieldLevel) bool {
	s, ok := stringOf(fl)
	return ok && LabelNameRe.MatchString(s)
}

// isClusterKey implements the `clusterkey` rule. The charset participates in
// Alert identity, so it is byte-identical to clusters_key_ck.
func isClusterKey(fl validator.FieldLevel) bool {
	s, ok := stringOf(fl)
	return ok && ClusterKeyRe.MatchString(s)
}

// isMatcherOp implements the `matcherop` rule. Alertmanager encodes all four
// operators via (isRegex, isEqual); oto's wire form spells them out.
func isMatcherOp(fl validator.FieldLevel) bool {
	s, ok := stringOf(fl)
	if !ok {
		return false
	}
	switch s {
	case "=", "!=", "=~", "!~":
		return true
	default:
		return false
	}
}

// isNotBlank implements the `notblank` rule: present is not the same as
// meaningful. A string of spaces is blank; an empty slice or map is blank.
func isNotBlank(fl validator.FieldLevel) bool {
	f := fl.Field()
	switch f.Kind() {
	case reflect.String:
		return strings.TrimSpace(f.String()) != ""
	case reflect.Slice, reflect.Array, reflect.Map:
		return f.Len() > 0
	case reflect.Pointer, reflect.Interface:
		return !f.IsNil()
	default:
		return f.IsValid() && !f.IsZero()
	}
}

// isAbsoluteHTTPURL implements the `httpurl` rule. The regex is the one half of
// alert_sources_base_ck that is a URL rule; the DDL's additional "no trailing
// slash" predicate is a column rule and is enforced by the domain constructor.
func isAbsoluteHTTPURL(fl validator.FieldLevel) bool {
	s, ok := stringOf(fl)
	return ok && HTTPURLRe.MatchString(s)
}

// MaxCursorBytes bounds an opaque keyset cursor. A cursor is a base64url token
// minted by oto; the filter-hash check that makes it "valid for this filter"
// happens when it is decoded, not here (§L.2.3).
const MaxCursorBytes = 512

// isOpaqueCursor implements the `cursor` rule: a well-formed base64url token of a
// sane length. An empty cursor means "first page" and is always acceptable.
func isOpaqueCursor(fl validator.FieldLevel) bool {
	s, ok := stringOf(fl)
	if !ok {
		return false
	}
	if s == "" {
		return true
	}
	if len(s) > MaxCursorBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '=':
		default:
			return false
		}
	}
	return true
}

func stringOf(fl validator.FieldLevel) (string, bool) {
	f := fl.Field()
	if f.Kind() != reflect.String {
		return "", false
	}
	return f.String(), true
}
