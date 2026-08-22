package app

import (
	"strings"
	"testing"

	"github.com/thulasiram/oto/internal/channels/template"
)

// ⛔ THE SEED MUST RENDER, AND NOTHING ELSE PROVES IT. It is written as a Go
// string constant and inserted with a raw SQL statement, so it passes neither the
// API's save-time gate nor the editor's preview — the two places every other
// template in the product is checked. A seed that does not compile is worse than
// no seed at all: an operator picks it, every card silently falls back to oto's
// built-in one, and the feature looks broken on the first day.
func TestTheSeededTemplateActuallyRenders(t *testing.T) {
	t.Parallel()

	problems := template.Validate(template.FormatCard, defaultTemplateSource)
	for _, p := range problems {
		if p.Kind != template.ProblemWarning {
			t.Errorf("the seeded template is refused by the same gate a save runs: %s (%s)",
				p.Message, p.Fixture)
		}
	}

	// ⭐ AND IT MUST CARRY THE BUTTONS. The one warning this gate can emit is a
	// missing `{{ actions }}`, which is a choice an operator is allowed to make —
	// but not one oto should make FOR them in the row it ships.
	if template.Blocking(problems) {
		t.Fatal("the seeded template does not compile; see the errors above")
	}
	for _, p := range problems {
		if p.Kind == template.ProblemWarning {
			t.Errorf("the seed ships a card with no Acknowledge button: %s", p.Message)
		}
	}
}

// The seed is meant to teach, so it should use each construct once. This is a
// cheap check that a future edit does not quietly reduce it to a heading.
func TestTheSeededTemplateShowsEveryConstruct(t *testing.T) {
	t.Parallel()

	for _, want := range []string{
		"# ",            // a heading
		"| default:",    // a filter with a fallback
		"\n---\n",       // a divider
		":::fields",     // the grid
		"{% for ",       // a loop
		"]({{ links.",   // a link, written the only way links can be written
		"{{ actions }}", // the button row
	} {
		if !strings.Contains(defaultTemplateSource, want) {
			t.Errorf("the seeded template no longer demonstrates %q; it is the only "+
				"documentation most operators will read", want)
		}
	}
}
