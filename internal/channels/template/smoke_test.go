package template

import (
	"strings"
	"testing"
)

const cardExample = `# {{ alert.name }}

{{ annotations.summary }}

---

:::fields
Severity | {{ alert.severity | upper }}
Firing | {{ group.firing_for }}
Seen | {{ alert.total_cases }} times
:::

{% for l in label_list %}- {{ l.name }}: {{ l.value }}
{% endfor %}
> Rule: [{{ rule.name }}]({{ links.group }})

{{ actions }}`

// ⭐ THE WHOLE POINT, IN ONE TEST: one document, two providers, no template
// change. If this ever renders the same bytes for both dialects, the portability
// claim is a lie and the Dialect layer has stopped doing anything.
func TestOneCardTwoSpellings(t *testing.T) {
	tpl, err := Compile(FormatCard, cardExample)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	in, links := Fixtures()[0].Bind(FormatCard)
	doc, probs := tpl.RenderCard(in, links)
	if len(probs) > 0 {
		t.Fatalf("render: %v", probs)
	}

	slack := renderDoc(SlackDialect{}, doc)
	plain := renderDoc(PlainDialect{}, doc)
	t.Logf("SLACK:\n%s", slack)
	t.Logf("PLAIN:\n%s", plain)

	if slack == plain {
		t.Fatal("both providers spelled the card identically; the dialect layer is inert")
	}
	if !strings.Contains(slack, "*CRITICAL*") && !strings.Contains(slack, "CRITICAL") {
		t.Errorf("the severity field did not survive: %s", slack)
	}
	if !doc.HasActions {
		t.Error("`{{ actions }}` did not place an action row")
	}
	if !strings.Contains(slack, "service: checkout") {
		t.Errorf("`{%% for %%}` over label_list produced no items: %s", slack)
	}
	if !strings.Contains(slack, "<https://") {
		t.Errorf("slack did not spell the link as <url|text>: %s", slack)
	}
	if strings.Contains(plain, "<https://") {
		t.Errorf("plain text should not carry slack link syntax: %s", plain)
	}
}

// ⛔ THE INJECTION TEST. A label value is written by anyone who can fire a
// metric. It must never become syntax.
func TestAlertValuesCannotBecomeSyntax(t *testing.T) {
	tpl, err := Compile(FormatCard, "{{ labels.evil }}")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	v := firingView()
	v.Alerts[0].Labels = map[string]string{
		"evil": "# HEADING\n---\n**bold** [click](" + linkOpen + "group" + linkShut + ") <@U123> @channel",
	}
	in, links := BuildInput(v, fixtureClock, FormatCard)
	doc, probs := tpl.RenderCard(in, links)
	if len(probs) > 0 {
		t.Fatalf("render: %v", probs)
	}
	if n := len(doc.Blocks); n != 1 {
		t.Fatalf("a label produced %d blocks; it must only ever produce prose", n)
	}
	if doc.Blocks[0].Kind != BlockParagraph {
		t.Fatalf("a label became a %s", doc.Blocks[0].Kind)
	}
	for _, sp := range doc.Blocks[0].Inline {
		if sp.Kind != SpanText {
			t.Fatalf("a label produced a %s span: %+v", sp.Kind, sp)
		}
	}
	out := renderDoc(SlackDialect{}, doc)
	for _, forbidden := range []string{"@channel", "<@U123>", "<https://"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("a label smuggled %q through: %s", forbidden, out)
		}
	}
	t.Logf("hostile label rendered as: %s", out)
}

// ⛔ A FORGED HANDLE IS THE ONE WAY A VALUE COULD HAVE BECOME A LINK, and
// sanitise is what stops it. This proves the strip, not the intent.
func TestATemplateCannotForgeAHandle(t *testing.T) {
	tpl, err := Compile(FormatCard, "[click]("+linkOpen+"group"+linkShut+") and group")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if strings.ContainsAny(tpl.Source, "") {
		t.Fatal("a private-use codepoint typed into a template source survived Compile")
	}
}

func renderDoc(d Dialect, doc *Document) string {
	var b strings.Builder
	for _, blk := range doc.Blocks {
		switch blk.Kind {
		case BlockDivider:
			b.WriteString("\n---\n")
		case BlockActions:
			b.WriteString("\n[buttons]\n")
		case BlockFields:
			for _, f := range blk.Fields {
				b.WriteString(Inline(d, f.Label) + " = " + Inline(d, f.Value) + "\n")
			}
		case BlockList:
			for _, it := range blk.Items {
				b.WriteString("• " + Inline(d, it) + "\n")
			}
		default:
			b.WriteString(Inline(d, blk.Inline) + "\n")
		}
	}
	return b.String()
}
