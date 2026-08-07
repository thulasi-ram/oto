package configschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// Schema is one provider's compiled config schema.
//
// The same bytes do three jobs (SPEC §L.5): they validate every create/update on
// the server, they are served verbatim by GET /api/v1/channel-types, and they
// render the settings form in the UI. There is no second copy of these rules
// anywhere, which is why adding a provider changes no UI code.
type Schema struct {
	id       string
	raw      json.RawMessage
	compiled *jsonschema.Schema
}

// MustCompile compiles raw as a draft 2020-12 schema published at id.
//
// It PANICS on a schema that does not compile, deliberately: a provider whose
// schema is broken cannot validate anything, and a process that boots in that
// state would accept configuration it can never honour (SPEC §L.5). Schemas are
// embedded, so this can only fail on a developer's own edit.
func MustCompile(id string, raw []byte) *Schema {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		panic("configschema: " + id + " is not valid JSON: " + err.Error())
	}

	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	// Formats are assertions here, not annotations: `format: "uri"` on a webhook
	// endpoint is a rule the operator expects us to enforce.
	c.AssertFormat()
	if err := c.AddResource(id, doc); err != nil {
		panic("configschema: cannot add " + id + ": " + err.Error())
	}
	compiled, err := c.Compile(id)
	if err != nil {
		panic("configschema: cannot compile " + id + ": " + err.Error())
	}

	cp := make(json.RawMessage, len(raw))
	copy(cp, raw)
	return &Schema{id: id, raw: cp, compiled: compiled}
}

// ID is the schema's $id.
func (s *Schema) ID() string { return s.id }

// Raw returns a copy of the schema bytes, exactly as they are served to the UI.
// It is a copy because a Descriptor is shared by every caller and a mutated
// schema would be a silently divergent second source of truth.
func (s *Schema) Raw() json.RawMessage {
	out := make(json.RawMessage, len(s.raw))
	copy(out, s.raw)
	return out
}

// Validate checks one channel's stored config against the schema.
//
// A failure is errs.KindValidation carrying one Violation per leaf schema error,
// with the JSON Pointer of the offending value as the field and the failing
// keyword as the code (SPEC §L.5). Field paths are JSON names, '/'-separated,
// never Go names.
func (s *Schema) Validate(raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return errs.Validation("config_invalid", "channel config is empty",
			errs.Violation{Field: "", Code: "required", Message: "a channel config object is required"})
	}

	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return errs.Malformed("config_unparseable", "channel config is not valid JSON")
	}

	if err := s.compiled.Validate(inst); err != nil {
		var verr *jsonschema.ValidationError
		if errors.As(err, &verr) {
			return errs.Validation("config_invalid", "channel config does not match the provider schema",
				Violations(verr)...)
		}
		return errs.Validation("config_invalid", "channel config does not match the provider schema")
	}
	return nil
}

// Violations flattens a jsonschema error tree into the leaf failures a human can
// act on. Intermediate applicator nodes (allOf, $ref, the root schema wrapper)
// carry no field-level information and are dropped.
func Violations(verr *jsonschema.ValidationError) []errs.Violation {
	var out []errs.Violation
	collect(verr, &out)
	if len(out) == 0 {
		out = append(out, errs.Violation{
			Field:   field(verr.InstanceLocation),
			Code:    "invalid",
			Message: clean(verr.Error()),
		})
	}
	return out
}

func collect(e *jsonschema.ValidationError, out *[]errs.Violation) {
	if len(e.Causes) > 0 {
		for _, c := range e.Causes {
			collect(c, out)
		}
		return
	}

	base := field(e.InstanceLocation)
	msg := clean(e.Error())

	switch k := e.ErrorKind.(type) {
	case *kind.Schema:
		// The root wrapper. It says only "validation failed"; its causes carried
		// the detail, and a leaf wrapper means an empty instance.
		*out = append(*out, errs.Violation{Field: base, Code: "invalid", Message: msg})
	case *kind.Required:
		for _, m := range k.Missing {
			*out = append(*out, errs.Violation{
				Field:   join(base, m),
				Code:    "required",
				Message: "this field is required",
			})
		}
	case *kind.AdditionalProperties:
		for _, p := range k.Properties {
			*out = append(*out, errs.Violation{
				Field:   join(base, p),
				Code:    "additionalProperties",
				Message: "unknown field",
			})
		}
	case *kind.PropertyNames:
		*out = append(*out, errs.Violation{
			Field:   join(base, k.Property),
			Code:    "propertyNames",
			Message: msg,
		})
	default:
		*out = append(*out, errs.Violation{Field: base, Code: code(e), Message: msg})
	}
}

// code is the failing keyword, which is the last segment of the keyword path.
func code(e *jsonschema.ValidationError) string {
	path := e.ErrorKind.KeywordPath()
	if len(path) == 0 {
		return "invalid"
	}
	return path[len(path)-1]
}

// field renders an instance location as a '/'-separated JSON-name path with no
// leading slash: matchers[0].name becomes "matchers/0/name" (SPEC §L.2.2).
func field(loc []string) string { return strings.Join(loc, "/") }

func join(base, leaf string) string {
	if base == "" {
		return leaf
	}
	return base + "/" + leaf
}

// clean strips the library's leading "at '/x': " locator, which duplicates the
// Field we already report, and collapses the message onto one line.
func clean(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "': "); strings.HasPrefix(s, "at '") && i > 0 {
		s = s[i+3:]
	}
	return strings.TrimSpace(s)
}
