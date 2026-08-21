package wording

import (
	"strings"
	"testing"
	"time"
	"unicode"
)

// walkLeaves visits every scalar leaf of a StanzaInput tree.
func walkLeaves(t *testing.T, path string, v any, fn func(path string, leaf any)) {
	t.Helper()
	switch n := v.(type) {
	case StanzaInput:
		for k, c := range n {
			walkLeaves(t, path+"."+k, c, fn)
		}
	case map[string]any:
		for k, c := range n {
			walkLeaves(t, path+"."+k, c, fn)
		}
	default:
		fn(path, v)
	}
}

// TestStanzaInputHoldsOnlyScalars is the mechanical form of ADR 0037's bindings
// rule. The ADR says "a flat map of scalars"; oto uses a NESTED map so an author
// can write {{ alert.name }}, and this test is what keeps the property the ADR
// actually wanted — that nothing reflectable is reachable.
func TestStanzaInputHoldsOnlyScalars(t *testing.T) {
	for _, f := range Fixtures() {
		walkLeaves(t, f.Name, f.Input, func(path string, leaf any) {
			switch leaf.(type) {
			case nil, string, bool, int, int32, int64, float32, float64:
			default:
				t.Errorf("%s is %T — a Wording must only ever reach scalars, "+
					"because liquid reflects into anything else", path, leaf)
			}
		})
	}
}

// TestNoPrivateUseCodepointReachesOutput is the mark contract. A private-use
// codepoint on a card renders as a replacement glyph on some clients and nothing
// on others: invisible to the author, visible to one reader.
func TestNoPrivateUseCodepointReachesOutput(t *testing.T) {
	src := `{{ alert.name | code }} {{ case.state | strike }} {{ group.severity | bold }} ` +
		`{{ group.started_at | datetime }} {{ actor.label | italic | default: "-" }}`
	w, err := Compile(StanzaBody, src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, d := range []Dialect{SlackDialect{}, PlainDialect{}} {
		for _, f := range Fixtures() {
			out, _ := w.Render(f.Input, d)
			for _, r := range out {
				if r >= '' && r <= '' {
					t.Errorf("%s/%s: private-use %U survived into %q", d.Name(), f.Name, r, out)
				}
			}
		}
	}
}

// TestForgedMarksAreStripped is the reason sanitise runs on literals as well as on
// interpolated values. An author typing a raw mark into a template body is the only
// way to reach a Dialect's spelling directly.
func TestForgedMarksAreStripped(t *testing.T) {
	w, err := Compile(StanzaBody, "literal forged end")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	out, err := w.Render(Fixtures()[0].Input, SlackDialect{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(out, "`") {
		t.Errorf("a literal mark was spelled as markup: %q", out)
	}
	if !strings.Contains(out, "forged") {
		t.Errorf("the words were dropped along with the mark: %q", out)
	}
}

// TestUpstreamMarksAreStripped covers the same forgery arriving from an annotation
// rather than from the template body.
func TestUpstreamMarksAreStripped(t *testing.T) {
	w, _ := Compile(StanzaBody, `{{ annotations.forged | default: "-" }}`)
	var hostile Fixture
	for _, f := range Fixtures() {
		if f.Name == "hostile-text" {
			hostile = f
		}
	}
	out, err := w.Render(hostile.Input, SlackDialect{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(out, "`") {
		t.Errorf("an upstream annotation forged a code mark: %q", out)
	}
}

// TestNoWordingCanEmitAnAudience is half of ADR 0037's safety property, and it is
// checked PER DIALECT because a broadcast ping is spelled differently in each.
func TestNoWordingCanEmitAnAudience(t *testing.T) {
	banned := map[string][]string{
		"slack": {"<!channel>", "<!here>", "<!subteam^", "<@U", "@everyone", "@channel", "@here"},
		"plain": {"@everyone", "@channel", "@here", "@room"},
	}
	sources := []string{
		`{{ annotations.summary | default: "-" }}`,
		`{{ actor.label | default: "-" }}`,
		`{{ comment | default: "-" }}`,
		`<!channel> @everyone @here <@U024BE7LH> <!subteam^SAZ94GDB8> deploy now`,
		`{{ comment | upper }}`,
	}
	for _, d := range []Dialect{SlackDialect{}, PlainDialect{}} {
		for _, src := range sources {
			w, err := Compile(StanzaBody, src)
			if err != nil {
				t.Fatalf("compile %q: %v", src, err)
			}
			for _, f := range Fixtures() {
				out, _ := w.Render(f.Input, d)
				low := strings.ToLower(out)
				for _, tok := range banned[d.Name()] {
					if strings.Contains(low, strings.ToLower(tok)) {
						t.Errorf("%s/%s: %q emitted the audience token %q in %q",
							d.Name(), f.Name, src, tok, out)
					}
				}
			}
		}
	}
}

// TestDialectsSpellTheSameMarkDifferently is the whole cross-channel argument in
// one assertion: the SAME wording, two providers, two spellings, one meaning.
func TestDialectsSpellTheSameMarkDifferently(t *testing.T) {
	w, err := Compile(StanzaBody, `{{ group.severity | bold }} {{ alert.service | code }}`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	in := Fixtures()[0].Input
	slack, err := w.Render(in, SlackDialect{})
	if err != nil {
		t.Fatalf("slack: %v", err)
	}
	plain, err := w.Render(in, PlainDialect{})
	if err != nil {
		t.Fatalf("plain: %v", err)
	}
	if !strings.Contains(slack, "*critical*") {
		t.Errorf("slack bold is one asterisk; got %q", slack)
	}
	if !strings.Contains(slack, "`checkout`") {
		t.Errorf("slack code is backticks; got %q", slack)
	}
	if strings.ContainsAny(plain, "*`~_") {
		t.Errorf("the webhook is not a degraded Slack; got markup in %q", plain)
	}
	if !strings.Contains(plain, "critical") || !strings.Contains(plain, "checkout") {
		t.Errorf("plain dropped the words with the markup: %q", plain)
	}
}

// TestTimestampIsSpelledPerProvider pins D2: a Wording says "when", and each
// provider decides how a "when" looks.
func TestTimestampIsSpelledPerProvider(t *testing.T) {
	w, _ := Compile(StanzaBody, `at {{ group.started_at | datetime }}`)
	in := Fixtures()[0].Input
	slack, _ := w.Render(in, SlackDialect{})
	plain, _ := w.Render(in, PlainDialect{})
	if !strings.Contains(slack, "<!date^") {
		t.Errorf("slack should get its own date token: %q", slack)
	}
	if strings.Contains(plain, "<!date^") {
		t.Errorf("a webhook consumer must not be handed Slack's token: %q", plain)
	}
	if !strings.Contains(plain, "2026-03-14") {
		t.Errorf("plain should get oto's UTC rendering: %q", plain)
	}
}

// TestSaveTimeCatchesWhatParseTimeCannot is why Validate renders instead of only
// parsing: liquid reports an unknown filter at render time.
func TestSaveTimeCatchesWhatParseTimeCannot(t *testing.T) {
	if _, err := laxly().ParseString(`{{ x | no_such_filter }}`); err != nil {
		t.Fatalf("premise broken — this is supposed to PARSE cleanly: %v", err)
	}
	problems := Validate(StanzaBody, `{{ alert.name | no_such_filter }}`)
	if len(problems) == 0 {
		t.Fatal("an unknown filter was accepted at save time")
	}
	if problems[0].Kind != ProblemRender {
		t.Errorf("want a render problem, got %s: %s", problems[0].Kind, problems[0].Message)
	}
	if !strings.Contains(problems[0].Message, "no_such_filter") {
		t.Errorf("the message must quote the offending expression: %q", problems[0].Message)
	}
}

// TestStrictAtAuthoringLaxAtDelivery is the ADR's red-team doctrine: refuse at
// write time, degrade at render time.
func TestStrictAtAuthoringLaxAtDelivery(t *testing.T) {
	const typo = `{{ alert.nmae | default: "-" }} fired`
	if got := Validate(StanzaBody, typo); len(got) == 0 {
		t.Error("a typo must be refused while a human is present to be told")
	}
	w, err := Compile(StanzaBody, typo)
	if err != nil {
		t.Fatalf("delivery must still compile it: %v", err)
	}
	out, err := w.Render(Fixtures()[0].Input, SlackDialect{})
	if err != nil {
		t.Fatalf("a typo must degrade the stanza, never kill the delivery: %v", err)
	}
	if !strings.Contains(out, "fired") {
		t.Errorf("the rest of the stanza should survive: %q", out)
	}
}

// TestGoodWordingSurvivesEveryFixture is the corpus doing its job.
func TestGoodWordingSurvivesEveryFixture(t *testing.T) {
	src := `{{ alert.name | default: group.title | default: "a signal" }} has been firing ` +
		`{{ group.firing_for | default: "for a while" }}, ` +
		`{{ alert.total_cases | default: 1 | plural: "time", "times" }} so far`
	if problems := Validate(StanzaBody, src); len(problems) != 0 {
		t.Fatalf("a reasonable wording was refused: %+v", problems)
	}
	w, err := Compile(StanzaBody, src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, f := range Fixtures() {
		for _, d := range []Dialect{SlackDialect{}, PlainDialect{}} {
			if _, err := w.Render(f.Input, d); err != nil && f.Representative {
				t.Errorf("%s/%s: %v", d.Name(), f.Name, err)
			}
		}
	}
}

// TestEmptyRenderIsAFailureNotACard — a stanza that vanishes tells a smaller truth
// with nothing to say it did, so it must fall back to Go's text.
func TestEmptyRenderIsAFailureNotACard(t *testing.T) {
	w, err := Compile(StanzaBody, `{{ actor.label }}`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// The digest fixture has no actor.
	var digest Fixture
	for _, f := range Fixtures() {
		if f.Name == "digest" {
			digest = f
		}
	}
	if _, err := w.Render(digest.Input, SlackDialect{}); err == nil {
		t.Error("an empty render must be an error so the caller falls back")
	}
	if problems := Validate(StanzaBody, `{{ actor.label | default: "" }}`); len(problems) == 0 {
		t.Error("a wording that renders nothing on an ordinary card must be refused at save time")
	}
}

// TestActionsTakesNoWording pins D8.
func TestActionsTakesNoWording(t *testing.T) {
	if StanzaActions.Wordable() {
		t.Fatal("the actions row is structure, not wording")
	}
	if _, err := Compile(StanzaActions, "anything"); err == nil {
		t.Error("compiling a wording for actions must fail")
	}
	p := Validate(StanzaActions, "anything")
	if len(p) != 1 || p[0].Kind != ProblemStanza {
		t.Fatalf("want one stanza refusal, got %+v", p)
	}
	if !strings.Contains(p[0].Message, "action ids") {
		t.Errorf("the refusal must say WHY: %q", p[0].Message)
	}
	for _, s := range AllStanzas {
		if s != StanzaActions && !s.Wordable() {
			t.Errorf("%s should be wordable", s)
		}
	}
	if len(AllStanzas) != 8 {
		t.Errorf("SPEC §H.7 names eight stanzas, this has %d", len(AllStanzas))
	}
}

// TestNoControlOrBidiSurvives — a right-to-left override can reverse a rendered
// sentence, so the author writes one thing and the reader sees another.
func TestNoControlOrBidiSurvives(t *testing.T) {
	w, _ := Compile(StanzaBody, `{{ annotations.bidi | default: "-" }}{{ annotations.control | default: "" }}`)
	for _, f := range Fixtures() {
		out, _ := w.Render(f.Input, SlackDialect{})
		for _, r := range out {
			if unicode.Is(unicode.Cf, r) || (unicode.IsControl(r) && r != '\n' && r != '\t') {
				t.Errorf("%s: %U survived into %q", f.Name, r, out)
			}
		}
	}
}

// TestSourceIsBounded — a stanza is one line of prose.
func TestSourceIsBounded(t *testing.T) {
	long := strings.Repeat("x", MaxTemplateBytes+1)
	if _, err := Compile(StanzaBody, long); err == nil {
		t.Error("an oversized template must be refused")
	}
	if p := Validate(StanzaBody, long); len(p) != 1 || p[0].Kind != ProblemTooLong {
		t.Errorf("want a too_long problem, got %+v", p)
	}
}

// TestHumaniseMatchesSlackWording pins the twin. If slack.humanDuration changes,
// this is what notices before two channels disagree about one signal's age.
func TestHumaniseMatchesSlackWording(t *testing.T) {
	cases := map[time.Duration]string{
		500 * time.Millisecond: "under a second",
		45 * time.Second:       "45s",
		90 * time.Second:       "1m 30s",
		20 * time.Minute:       "20m",
		time.Hour:              "1h",
		95 * time.Minute:       "1h 35m",
		25 * time.Hour:         "1d 1h",
		48 * time.Hour:         "2d",
	}
	for d, want := range cases {
		if got := humanise(d); got != want {
			t.Errorf("humanise(%s) = %q, want %q", d, got, want)
		}
	}
}

// TestEnrichmentPayloadIsReachable closes the design note's residue item 3: the
// alert.history payload had no rendered surface anywhere.
func TestEnrichmentPayloadIsReachable(t *testing.T) {
	w, err := Compile(StanzaBody, `{{ enrichment.alert_history.cases_7d | default: 0 }} cases`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	out, err := w.Render(Fixtures()[0].Input, PlainDialect{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "4 cases") {
		t.Errorf("the alert.history payload should be reachable by name; got %q", out)
	}
}

// TestNestedPayloadIsDropped — a Wording cannot iterate, and a Go map's print order
// is not deterministic, which would wobble the rendered hash oto de-dupes on.
func TestNestedPayloadIsDropped(t *testing.T) {
	w, _ := Compile(StanzaBody, `[{{ enrichment.alert_history.by_day | default: "absent" }}]`)
	out, _ := w.Render(Fixtures()[0].Input, PlainDialect{})
	if !strings.Contains(out, "absent") {
		t.Errorf("a nested payload value must not reach a card: %q", out)
	}
}

// TestTypoIsRefusedButAbsenceIsNot is the distinction StrictVariables could not
// make, and the reason unknownFields exists.
func TestTypoIsRefusedButAbsenceIsNot(t *testing.T) {
	t.Run("a misspelled oto field is refused", func(t *testing.T) {
		p := Validate(StanzaBody, `{{ alert.nmae | default: "-" }}`)
		if len(p) == 0 {
			t.Fatal("a typo in oto's own vocabulary must be refused")
		}
		if p[0].Kind != ProblemUnknownField {
			t.Fatalf("want unknown_field, got %s: %s", p[0].Kind, p[0].Message)
		}
		if !strings.Contains(p[0].Message, "alert.nmae") {
			t.Errorf("the message must name the field: %q", p[0].Message)
		}
	})

	t.Run("a customer label that some signals lack is accepted", func(t *testing.T) {
		for _, src := range []string{
			`{{ labels.team | default: "unowned" }}`,
			`{{ annotations.impact | default: "unknown" }}`,
			`{{ enrichment.alert_history.cases_7d | default: 0 }}`,
		} {
			if p := Validate(StanzaBody, src); len(p) != 0 {
				t.Errorf("%s was refused: %+v", src, p)
			}
		}
	})

	t.Run("a field absent on THIS card but real is accepted", func(t *testing.T) {
		// rule.* is absent on a resolved card with no snapshot and on every digest.
		// It is still a real field and must not read as a typo.
		if p := Validate(StanzaBody, `{{ rule.name | default: "no rule" }} {{ case.acked_by | default: "nobody" }}`); len(p) != 0 {
			t.Errorf("a legitimately-absent field was refused: %+v", p)
		}
	})

	t.Run("several typos are all reported, not just the first", func(t *testing.T) {
		p := Validate(StanzaBody, `{{ alert.nmae }} {{ group.titel }} {{ case.stat }}`)
		if len(p) < 3 {
			t.Errorf("want all three reported, got %d: %+v", len(p), p)
		}
	})
}

// TestDeliveryNeverFailsOnAnAbsentField — the lax half of the doctrine.
func TestDeliveryNeverFailsOnAnAbsentField(t *testing.T) {
	w, err := Compile(StanzaBody, `{{ rule.name | default: "no rule" }} / {{ labels.team | default: "unowned" }}`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, f := range Fixtures() {
		out, err := w.Render(f.Input, SlackDialect{})
		if err != nil {
			t.Errorf("%s: delivery must degrade, not fail: %v", f.Name, err)
		}
		if !strings.Contains(out, "/") {
			t.Errorf("%s: the stanza lost its shape: %q", f.Name, out)
		}
	}
}
