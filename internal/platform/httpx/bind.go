package httpx

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/validate"
)

// DefaultMaxJSONBody bounds an API request body. It is deliberately far smaller
// than the ingest ceiling: a notification policy is kilobytes, and a caller that
// sends megabytes to /api/v1 has a bug rather than a large alert batch.
const DefaultMaxJSONBody int64 = 1 << 20

// Bind is THE ONLY DOOR through which a request body becomes a DTO (SPEC §L.2.1).
//
// It decodes with DisallowUnknownFields — the trusted-input policy — and then
// runs layer 1 validation, so no handler can forget either half. A handler that
// calls json.NewDecoder or validate.Struct itself has stepped around the one
// place violations are turned into JSON-name paths.
//
// The returned error is already an errs.Error and is handed straight to
// WriteProblem: 400 for an unparseable body, 415 for the wrong media type, 422
// with violations[] for a well-formed body that breaks a rule.
func Bind[T any](w http.ResponseWriter, r *http.Request) (T, error) {
	var dto T
	if err := Decode(w, r, &dto, DefaultMaxJSONBody); err != nil {
		return dto, err
	}
	if err := validate.Struct(dto); err != nil {
		return dto, err
	}
	return dto, nil
}

// BindEmpty validates a DTO that was assembled from somewhere other than a JSON
// body — a query string, path parameters, a form post. Layer 1 still applies:
// the DTO is where the bounds live, whatever filled it in.
func BindEmpty[T any](dto T) (T, error) {
	if err := validate.Struct(dto); err != nil {
		return dto, err
	}
	return dto, nil
}

// PathUUID reads a `{name}` path parameter as a UUID.
//
// A malformed id is a 404 and not a 422: `/alerts/banana` names no alert, and
// telling an unauthenticated scanner that the segment was "well-formed but not a
// UUID" is a distinction with no value to a legitimate caller.
func PathUUID(r *http.Request, name string) (uuid.UUID, error) {
	raw := chi.URLParam(r, name)
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, errs.NotFound("not_found", "no such resource")
	}
	return id, nil
}

// PathString reads a `{name}` path parameter as a bounded, non-empty string.
func PathString(r *http.Request, name string, maxLen int) (string, error) {
	v := chi.URLParam(r, name)
	if v == "" || len(v) > maxLen {
		return "", errs.NotFound("not_found", "no such resource")
	}
	return v, nil
}

// Params is a checked view over the query string.
//
// SPEC §E.3 is binding: an unknown query parameter is REJECTED with
// `400 unknown_parameter`. That is not pedantry — a typo'd `?serverity=critical`
// that is silently ignored returns a page of the wrong alerts and looks right,
// which is how the UI and the API stop agreeing with each other.
//
// Accessors accumulate their failures rather than returning early, so one
// request produces one problem document listing every bad parameter instead of a
// conversation.
type Params struct {
	values map[string][]string
	viol   []errs.Violation
	fatal  error
}

// NewParams builds a Params over r's query string, rejecting any parameter not in
// allowed. A name ending in `[` is a PREFIX permission, which is how §E.3's
// `label[team]` and `label[!tier]` families are admitted without enumerating
// every label an operator might own.
func NewParams(r *http.Request, allowed ...string) *Params {
	p := &Params{values: map[string][]string{}}
	exact := make(map[string]struct{}, len(allowed))
	var prefixes []string
	for _, a := range allowed {
		if strings.HasSuffix(a, "[") {
			prefixes = append(prefixes, a)
			continue
		}
		exact[a] = struct{}{}
	}

	var unknown []string
	for k, v := range r.URL.Query() {
		if _, ok := exact[k]; ok {
			p.values[k] = v
			continue
		}
		matched := false
		for _, pre := range prefixes {
			if strings.HasPrefix(k, pre) && strings.HasSuffix(k, "]") {
				matched = true
				break
			}
		}
		if matched {
			p.values[k] = v
			continue
		}
		unknown = append(unknown, k)
	}

	if len(unknown) > 0 {
		e := errs.Malformed("unknown_parameter",
			"unknown query parameter: "+strings.Join(unknown, ", "))
		for _, u := range unknown {
			e.Violations = append(e.Violations, errs.Violation{
				Field: u, Code: "unknown_parameter", Message: u + " is not a known query parameter",
			})
		}
		p.fatal = e
	}
	return p
}

// Raw returns every value supplied for name.
func (p *Params) Raw(name string) []string { return p.values[name] }

// All returns every parameter that survived the allow-list, for the callers that
// need to walk a `label[…]` family.
func (p *Params) All() map[string][]string { return p.values }

// Has reports whether the caller supplied name at all. It distinguishes
// "absent" from "present and empty", which for a filter is the difference
// between "do not filter" and "match the empty string".
func (p *Params) Has(name string) bool { _, ok := p.values[name]; return ok }

// String reads name, falling back to def.
func (p *Params) String(name, def string) string {
	v := p.values[name]
	if len(v) == 0 || v[0] == "" {
		return def
	}
	return v[0]
}

// CSV reads name as a comma-separated list, trimming and dropping empties.
// Repeating the parameter is equivalent to one comma-joined value.
func (p *Params) CSV(name string) []string {
	var out []string
	for _, raw := range p.values[name] {
		for _, part := range strings.Split(raw, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// Enum reads name and requires it to be one of allowed. An empty value yields def.
func (p *Params) Enum(name, def string, allowed ...string) string {
	v := p.String(name, def)
	if v == "" {
		return def
	}
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	p.violate(name, "enum", name+" must be one of: "+strings.Join(allowed, ", "))
	return def
}

// EnumCSV reads name as a comma list and requires every entry to be allowed.
func (p *Params) EnumCSV(name string, allowed ...string) []string {
	vals := p.CSV(name)
	for _, v := range vals {
		ok := false
		for _, a := range allowed {
			if v == a {
				ok = true
				break
			}
		}
		if !ok {
			p.violate(name, "enum", name+" must be one of: "+strings.Join(allowed, ", "))
			return nil
		}
	}
	return vals
}

// Int reads name as an integer, falling back to def.
func (p *Params) Int(name string, def int) int {
	raw := p.String(name, "")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		p.violate(name, "type", name+" must be an integer")
		return def
	}
	return n
}

// Bool reads name as a boolean, returning nil when it was not supplied. A
// pointer rather than a value because a tri-state filter — `?snoozed=true`,
// `?snoozed=false`, absent — has three meanings and a bool has two.
func (p *Params) Bool(name string) *bool {
	raw := p.String(name, "")
	if raw == "" {
		return nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		p.violate(name, "type", name+" must be true or false")
		return nil
	}
	return &b
}

// Time reads name as an RFC 3339 instant, returning the zero time when absent.
func (p *Params) Time(name string) time.Time {
	raw := p.String(name, "")
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		p.violate(name, "format", name+" must be an RFC 3339 timestamp")
		return time.Time{}
	}
	return t.UTC()
}

// UUID reads name as a UUID, returning uuid.Nil when absent.
func (p *Params) UUID(name string) uuid.UUID {
	raw := p.String(name, "")
	if raw == "" {
		return uuid.Nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		p.violate(name, "uuid", name+" must be a UUID")
		return uuid.Nil
	}
	return id
}

// Limit reads `limit`, defaulting to DefaultPageLimit and capping at
// MaxPageLimit (SPEC §E.1). A caller asking for more than the ceiling is a
// caller who will page anyway; silently capping beats a 422 that breaks a UI.
func (p *Params) Limit() int {
	n := p.Int("limit", DefaultPageLimit)
	switch {
	case n <= 0:
		return DefaultPageLimit
	case n > MaxPageLimit:
		return MaxPageLimit
	default:
		return n
	}
}

// Cursor reads the opaque `cursor` parameter, unverified. The filter-hash check
// happens in DecodeCursor, where the current filter is known.
func (p *Params) Cursor() string { return p.String("cursor", "") }

func (p *Params) violate(field, code, message string) {
	p.viol = append(p.viol, errs.Violation{Field: field, Code: code, Message: message})
}

// Err returns the accumulated failure, or nil. Every handler that builds a query
// MUST check it before touching a service.
func (p *Params) Err() error {
	if p.fatal != nil {
		return p.fatal
	}
	if len(p.viol) == 0 {
		return nil
	}
	return errs.Validation("validation_failed", "invalid query parameters", p.viol...)
}
