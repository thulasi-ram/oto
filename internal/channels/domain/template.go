package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// A NotificationTemplate is one whole message an operator wrote.
//
// ⭐ IT IS A DOCUMENT, NOT A SET OF SLOTS, AND THAT IS THE WHOLE PIVOT. The design
// this replaced let an operator override the text of four named blocks and kept
// the structure for oto. It was safe and it was defensible and nobody's mental
// model of a template is "four holes in somebody else's card". A template you can
// read top to bottom is one you can predict; four independent overrides are not.
//
// ⛔ IT CARRIES NO `when` CLAUSE. Selection belongs to the NotificationPolicy,
// which already has matchers, already chose the channels, and now also names the
// template. One routing decision, one place to read it. The predecessor selected
// itself, and that was right only while an operator needed four different
// predicates for four different slots.
type NotificationTemplate struct {
	ID    uuid.UUID
	OrgID uuid.UUID
	// Name is what a policy's editor shows in its picker.
	Name string
	// Provider is the destination kind this template was WRITTEN FOR.
	//
	// ⚠️ IT IS DECLARED INTENT AND NOT AN ENFORCED CONSTRAINT. A policy fans out
	// to as many as sixteen channels and they need not share a provider, so a
	// template can always reach a destination it was not written for. `card` and
	// `text` are portable and render there correctly anyway; `raw` cannot, and
	// degrades to oto's built-in card. oto warns at save and does not block —
	// pairing them up is the operator's call.
	Provider string
	// Format is `card`, `text` or `raw`.
	Format string
	// Source is the template body.
	Source string
	// Version increments on every edit that changes Source or Format.
	//
	// ⭐ IT IS THE PROVENANCE HALF OF THE DELIVERY ROW. A delivery records the
	// template id AND this number, so a card that read strangely last Tuesday can
	// be attributed to a revision even after somebody has edited it since. It is
	// deliberately not a full version history: the rendered payload is already
	// persisted beside it, so the bytes that actually went out are never in doubt.
	Version int
	Enabled bool

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// Live reports whether this template may be used by a delivery.
func (t NotificationTemplate) Live() bool { return t.Enabled && t.DeletedAt == nil }

// NewNotificationTemplate is the create draft.
type NewNotificationTemplate struct {
	ID       uuid.UUID
	Name     string
	Provider string
	Format   string
	Source   string
	Enabled  bool
}

// NotificationTemplatePatch is the partial update. A nil field is untouched.
type NotificationTemplatePatch struct {
	Name     *string
	Provider *string
	Format   *string
	Source   *string
	Enabled  *bool
}

// Empty reports whether this patch asks for nothing.
func (p NotificationTemplatePatch) Empty() bool {
	return p.Name == nil && p.Provider == nil && p.Format == nil &&
		p.Source == nil && p.Enabled == nil
}

// The bounds a template is held to. They are here rather than in the template
// engine because they are also the database's CHECK constraints, and a limit that
// lives in two places is a limit that will diverge.
const (
	MaxTemplateNameRunes = 120
	// MaxTemplateSourceBytes matches template.MaxSourceBytes. A whole card is a
	// document, so this is far larger than the one line the slot design allowed.
	MaxTemplateSourceBytes = 16384
)

// TemplateFormats is the closed set of shapes an author may write in.
var TemplateFormats = []string{"card", "text", "raw"}

// ValidateNotificationTemplate holds the rules that do not need the engine.
//
// ⛔ IT DOES NOT PROVE THE TEMPLATE RENDERS, AND CANNOT. An unknown filter is a
// RENDER-time error, not a parse-time one, so proving a template works means
// executing it against the fixture corpus — which needs the engine, which lives
// in `channels/template`, which imports this package. `channels/service` owns
// that half and calls both.
func ValidateNotificationTemplate(name, provider, format, source string) error {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return errors.New("a template needs a name; it is what a policy's picker shows")
	case len([]rune(name)) > MaxTemplateNameRunes:
		return fmt.Errorf("a template name may be %d characters and this is %d",
			MaxTemplateNameRunes, len([]rune(name)))
	}
	if !validTemplateFormat(format) {
		return fmt.Errorf("%q is not a template format; the formats are %s",
			format, strings.Join(TemplateFormats, ", "))
	}
	if strings.TrimSpace(provider) == "" {
		return errors.New("a template names the destination kind it was written for")
	}
	switch {
	case strings.TrimSpace(source) == "":
		return errors.New("a template with no body would send an empty message")
	case len(source) > MaxTemplateSourceBytes:
		return fmt.Errorf("a template may be %d bytes and this is %d",
			MaxTemplateSourceBytes, len(source))
	}
	return nil
}

func validTemplateFormat(f string) bool {
	for _, v := range TemplateFormats {
		if v == f {
			return true
		}
	}
	return false
}
