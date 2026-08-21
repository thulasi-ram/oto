package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Bounds mirroring the CHECK constraints in db/migrations/00076. They are
// duplicated here on purpose: a constraint the database enforces and Go does not
// surfaces to an operator as a 500 with a constraint name, and the same rule
// enforced in Go surfaces as a 422 naming the field.
const (
	MaxWordingTemplate = 2048
	MaxWordingMatchers = 32
	MaxWordingReasons  = 32
	MaxWordingPriority = 100000
)

// WordableStanzas are the four SPEC §H.7 stanzas that are prose.
//
// ⛔ THE OTHER FOUR ARE STRUCTURE AND ARE REFUSED, NOT OMITTED. `fields` is a grid
// of separately-budgeted cells in a binding shed order; `members` and `trail` are
// sequences a loop-free template cannot iterate (ADR 0037's accepted ceiling); and
// `actions` carries button labels bound to their action ids. The refusal is
// answered with a sentence at the API rather than by the name simply being absent
// from a menu, because a name that is absent teaches nobody why.
var WordableStanzas = []string{"title", "body", "rule", "footer"}

// A Wording is one Liquid template producing the text of one Stanza (ADR 0037).
//
// ⛔ IT CHOOSES WORDS AND NOTHING ELSE. Structure, colour, mentions, links and
// destination all stay oto's, which is what makes it impossible for a Wording to
// mark a delivery dead. See internal/channels/render/wording for the mechanism.
type Wording struct {
	ID    uuid.UUID
	OrgID uuid.UUID
	// ChannelID is nil for the org-wide house voice. A Wording bound to one
	// destination is more specific and WINS over an org-wide one (ADR 0049).
	ChannelID *uuid.UUID
	Stanza    string
	Template  string
	// Matchers and Reasons are the `when` clause, in ADR 0017's vocabulary. An
	// empty clause matches everything, which is what makes a single org-wide row
	// the natural way to set a house voice.
	Matchers []Matcher
	Reasons  []string
	// Priority orders evaluation, LOWER FIRST, and the first match wins — the same
	// sentence notification_policies.priority carries, deliberately.
	Priority  int
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// Matcher is one label predicate.
//
// ⚠️ IT IS A DELIBERATE TWIN OF notification/domain.Matcher, NOT AN IMPORT. This
// module does not depend on the notification module — the rule reply.go already
// states for the Reason values — so the type is redeclared with a structurally
// identical JSON shape. If the two ever disagree, a Wording's clause simply fails
// to match and the built-in text is used, which is the safe direction.
type Matcher struct {
	Name  string
	Op    MatchOp
	Value string
}

// MatchOp is the closed set ADR 0017 chose over an expression language.
type MatchOp string

const (
	MatchEq    MatchOp = "="
	MatchNotEq MatchOp = "!="
	MatchRe    MatchOp = "=~"
	MatchNotRe MatchOp = "!~"
)

// Valid reports whether op is one of the four.
func (o MatchOp) Valid() bool {
	switch o {
	case MatchEq, MatchNotEq, MatchRe, MatchNotRe:
		return true
	}
	return false
}

// Live reports whether this Wording is eligible for resolution.
func (w Wording) Live() bool { return w.Enabled && w.DeletedAt == nil }

// OrgWide reports whether this Wording is the house voice rather than one
// destination's exception.
func (w Wording) OrgWide() bool { return w.ChannelID == nil }

// NewWording is the create command. Server-owned fields are absent so a caller
// cannot assert them.
type NewWording struct {
	ID        uuid.UUID
	ChannelID *uuid.UUID
	Stanza    string
	Template  string
	Matchers  []Matcher
	Reasons   []string
	Priority  int
	Enabled   bool
}

// WordingPatch is the partial update. Every field is a pointer so that "set this
// to empty" and "leave this alone" are different requests.
type WordingPatch struct {
	Template *string
	Matchers *[]Matcher
	Reasons  *[]string
	Priority *int
	Enabled  *bool
}

// IsEmpty reports whether the patch would change nothing.
func (p WordingPatch) IsEmpty() bool {
	return p.Template == nil && p.Matchers == nil && p.Reasons == nil &&
		p.Priority == nil && p.Enabled == nil
}

// StanzaTakesAWording reports whether s is one of the four prose stanzas.
func StanzaTakesAWording(s string) bool {
	for _, k := range WordableStanzas {
		if k == s {
			return true
		}
	}
	return false
}

// ValidateWording checks everything that does not need the template engine.
//
// ⚠️ IT IS DELIBERATELY NOT THE WHOLE GATE. Whether the template PARSES, names a
// field that exists and uses only curated filters is decided by
// render/wording.Validate, which must RENDER against a fixture corpus to find out —
// an unknown filter is a render-time error in Liquid, not a parse-time one. This
// function is the part that belongs to the domain: shape, bounds and vocabulary.
func ValidateWording(stanza, template string, matchers []Matcher, reasons []string, priority int) []errs.Violation {
	var v []errs.Violation
	add := func(field, code, msg string) {
		v = append(v, errs.Violation{Field: field, Code: code, Message: msg})
	}

	if !StanzaTakesAWording(stanza) {
		add("stanza", "unsupported_stanza", stanzaRefusal(stanza))
	}
	switch {
	case strings.TrimSpace(template) == "":
		add("template", "required", "a wording with no text would blank the stanza; delete it instead")
	case len(template) > MaxWordingTemplate:
		add("template", "too_long", "a wording is one line of prose, and the limit is 2048 bytes")
	}
	if len(matchers) > MaxWordingMatchers {
		add("matchers", "too_many", "at most 32 matchers")
	}
	for i, m := range matchers {
		if strings.TrimSpace(m.Name) == "" {
			add("matchers", "required", "matcher "+itoa(i)+" has no label name")
		}
		if !m.Op.Valid() {
			add("matchers", "unsupported_op", "matcher "+itoa(i)+" uses "+string(m.Op)+
				`; the operators are "=", "!=", "=~" and "!~"`)
		}
	}
	if len(reasons) > MaxWordingReasons {
		add("reasons", "too_many", "at most 32 reasons")
	}
	if priority < 0 || priority > MaxWordingPriority {
		add("priority", "out_of_range", "priority is 0 to 100000, lower first")
	}
	return v
}

// stanzaRefusal explains, to the person who typed it, why a stanza takes no
// Wording.
func stanzaRefusal(stanza string) string {
	switch stanza {
	case "fields":
		return "the fields grid is up to ten separately-budgeted cells in a binding order " +
			"that decides what is dropped on overflow, so one line of prose would replace " +
			"the grid rather than re-word it"
	case "members":
		return "the member list is an enumeration, and a wording cannot iterate: one line " +
			"would drop the instances it exists to name"
	case "trail":
		return "the state trail is an enumeration, and a wording cannot iterate: one line " +
			"would drop the transitions it exists to record"
	case "actions":
		return "the actions row carries interactive buttons whose labels are bound to their " +
			"action ids, so its text is structure rather than wording"
	default:
		return "not a stanza of an oto notification; the ones that take a wording are " +
			strings.Join(WordableStanzas, ", ")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
