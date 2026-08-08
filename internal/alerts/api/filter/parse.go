package filter

import (
	"regexp"
	"strings"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/validate"
)

// Bounds on a selector. They exist so that a pathological query is refused
// rather than planned: 32 matchers is already far past any real filter bar, and
// `LabelMap` caps an alert at 64 labels anyway.
const (
	// MaxMatchers is the most predicates one selector may carry.
	MaxMatchers = 32
	// MaxValueBytes mirrors the `LabelMap` value bound in openapi.yaml.
	MaxValueBytes = 4096
	// MaxSelectorBytes bounds the raw selector string.
	MaxSelectorBytes = 8192
)

// ParseSelector parses Alertmanager-style matcher syntax (ADR 0017):
//
//	{namespace="payments", severity=~"critical|warning", tier!="canary"}
//
// The surrounding braces are optional. Values may be quoted or bare; a bare value
// may not contain a comma, a brace or whitespace.
//
// Every failure is a `422` naming `field` verbatim, because a selector the caller
// mistyped must never be silently dropped: a filter that is ignored returns a
// page of the wrong alerts and looks right.
func ParseSelector(field, raw string) (Selector, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Selector{}, nil
	}
	if len(s) > MaxSelectorBytes {
		return Selector{}, violation(field, "max_length", "selector is too long")
	}
	if strings.HasPrefix(s, "{") {
		if !strings.HasSuffix(s, "}") {
			return Selector{}, violation(field, "invalid", "selector is missing its closing brace")
		}
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	if s == "" {
		return Selector{}, nil
	}

	parts, err := splitTopLevel(field, s)
	if err != nil {
		return Selector{}, err
	}

	out := Selector{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		m, err := parseMatcher(field, part)
		if err != nil {
			return Selector{}, err
		}
		out.Matchers = append(out.Matchers, m)
	}
	if len(out.Matchers) > MaxMatchers {
		return Selector{}, violation(field, "max_items", "selector carries too many matchers")
	}
	return out, nil
}

// splitTopLevel splits on commas that are not inside a quoted value. A regex
// alternation like `severity=~"critical|warning"` is common; one containing a
// comma is not, but it must still survive.
func splitTopLevel(field, s string) ([]string, error) {
	var (
		parts   []string
		current strings.Builder
		quoted  bool
		escaped bool
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			current.WriteByte(c)
			escaped = false
		case c == '\\' && quoted:
			current.WriteByte(c)
			escaped = true
		case c == '"':
			quoted = !quoted
			current.WriteByte(c)
		case c == ',' && !quoted:
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteByte(c)
		}
	}
	if quoted {
		return nil, violation(field, "invalid", "selector has an unterminated quoted value")
	}
	parts = append(parts, current.String())
	return parts, nil
}

// opOrder matters: the two-character operators must be tried before `=` and `!`,
// or `severity=~"x"` parses as `severity` `=` `~"x"`.
var opOrder = []Op{OpNotRegex, OpRegex, OpNotEqual, OpEqual}

func parseMatcher(field, part string) (Matcher, error) {
	for _, op := range opOrder {
		idx := strings.Index(part, string(op))
		if idx <= 0 {
			continue
		}
		// `!=` also contains `=`; the ordered scan above already ruled the
		// longer operators out by the time `=` is tried, so a `=` found at
		// idx-1 == '!' cannot happen here.
		name := strings.TrimSpace(part[:idx])
		value := strings.TrimSpace(part[idx+len(op):])
		return newMatcher(field, name, op, unquote(value))
	}
	return Matcher{}, violation(field, "matcher_op",
		"each matcher must use one of: =, !=, =~, !~")
}

func unquote(v string) string {
	if len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
		inner := v[1 : len(v)-1]
		return strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(inner)
	}
	return v
}

func newMatcher(field, name string, op Op, value string) (Matcher, error) {
	if !validate.LabelNameRe.MatchString(name) {
		return Matcher{}, violation(field, "labelname", "must be a valid Prometheus label name")
	}
	if !op.Valid() {
		return Matcher{}, violation(field, "matcher_op", "must be one of: =, !=, =~, !~")
	}
	if len(value) > MaxValueBytes {
		return Matcher{}, violation(field, "max_length", "matcher value is too long")
	}
	if op.IsRegex() {
		// A regex is validated here so that a malformed one is a 422 naming the
		// parameter, rather than an error from the database with no field on it.
		if _, err := regexp.Compile("^(?:" + value + ")$"); err != nil {
			return Matcher{}, violation(field, "invalid", "matcher value is not a valid regular expression")
		}
	}
	return Matcher{Name: name, Op: op, Value: value}, nil
}

// ParseLabelParams compiles the contract's `label[<name>]=<value>` family into a
// Selector (openapi.yaml `listAlerts`).
//
//   - `label[team]=core`            → team="core"
//   - `label[team]=core,platform`   → team="core" OR team="platform"
//   - `label[!tier]=canary`         → tier!="canary"
//
// The `!` prefix on the KEY is the negation marker, and it is the one piece of
// this parameter OpenAPI cannot express structurally. A malformed name is
// reported verbatim, brackets included, as `label[…]`.
func ParseLabelParams(params map[string][]string) (Selector, error) {
	var (
		out   Selector
		viols []errs.Violation
	)

	for key, values := range params {
		if !strings.HasPrefix(key, "label[") || !strings.HasSuffix(key, "]") {
			continue
		}
		name := key[len("label[") : len(key)-1]
		op := OpEqual
		if strings.HasPrefix(name, "!") {
			op = OpNotEqual
			name = name[1:]
		}
		if !validate.LabelNameRe.MatchString(name) {
			viols = append(viols, errs.Violation{
				Field: key, Code: "labelname", Message: "must be a valid Prometheus label name",
			})
			continue
		}
		for _, raw := range values {
			for _, v := range strings.Split(raw, ",") {
				v = strings.TrimSpace(v)
				if v == "" {
					continue
				}
				if len(v) > MaxValueBytes {
					viols = append(viols, errs.Violation{
						Field: key, Code: "max_length", Message: key + " value is too long",
					})
					continue
				}
				out.Matchers = append(out.Matchers, Matcher{Name: name, Op: op, Value: v})
			}
		}
	}

	if len(out.Matchers) > MaxMatchers {
		viols = append(viols, errs.Violation{
			Field: "label", Code: "max_items", Message: "too many label selectors",
		})
	}
	if len(viols) > 0 {
		return Selector{}, errs.Validation("validation_failed", plural(len(viols)), viols...)
	}
	return out, nil
}

func violation(field, code, message string) error {
	return errs.Validation("validation_failed", plural(1), errs.Violation{
		Field: field, Code: code, Message: message,
	})
}

func plural(n int) string {
	if n == 1 {
		return "1 field failed validation."
	}
	return "fields failed validation."
}
