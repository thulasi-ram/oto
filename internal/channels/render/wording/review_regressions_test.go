package wording

import (
	"strings"
	"testing"
)

// Every test here is a defect a review reproduced against the real path. They are
// kept together because the thing they have in common is how they were found:
// each one passed every test that existed at the time.

// TestABareURLIsDefusedRatherThanLinked — the bracketed form `<url|label>` never
// survived escaping, so links looked handled. Slack auto-links a BARE url in
// mrkdwn, so `{{ annotations.summary }}` over an annotation containing one put a
// live, customer-controlled link on the card. ADR 0037 refuses user-authored URLs.
func TestABareURLIsDefusedRatherThanLinked(t *testing.T) {
	v := firingView()
	v.Alerts[0].Annotations = map[string]string{
		"summary": "see https://evil.example/phish and www.evil.example for details.",
	}
	in := BuildInput(v, fixtureClock)
	w, err := Compile(StanzaBody, `{{ annotations.summary }}`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	slack, err := w.Render(in, SlackDialect{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(slack, "`https://evil.example/phish`") {
		t.Errorf("a bare url must be wrapped so Slack cannot linkify it: %q", slack)
	}
	if !strings.Contains(slack, "`www.evil.example`") {
		t.Errorf("a bare www host must be defused too: %q", slack)
	}
	// The reader still sees exactly what the alert said — defusing, not deleting.
	if !strings.Contains(slack, "evil.example/phish") {
		t.Errorf("the address must still be readable: %q", slack)
	}
	// Trailing sentence punctuation belongs to the prose, not the address.
	if strings.Contains(slack, "details.`") {
		t.Errorf("the defuser swallowed the sentence: %q", slack)
	}

	// A literal typed into the template is the same trust class.
	lit, _ := Compile(StanzaBody, `click https://evil.example now`)
	out, _ := lit.Render(in, SlackDialect{})
	if !strings.Contains(out, "`https://evil.example`") {
		t.Errorf("a literal url must be defused too: %q", out)
	}
}

// TestAUrlInsideACodeSpanIsNotDoubleWrapped — no provider linkifies inside code,
// which is exactly why defusing wraps in one.
func TestAUrlInsideACodeSpanIsNotDoubleWrapped(t *testing.T) {
	v := firingView()
	v.Alerts[0].Annotations = map[string]string{"summary": "https://x.example"}
	w, _ := Compile(StanzaBody, `{{ annotations.summary | code }}`)
	out, err := w.Render(BuildInput(v, fixtureClock), SlackDialect{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(out, "``") {
		t.Errorf("a url already in a code span was wrapped again: %q", out)
	}
}

// TestRemovingAnAudienceTokenCannotCreateOne. A single left-to-right pass resumes
// PAST the join it just made, so the two halves are never re-examined — and
// joining them can spell the token being removed. The values come from upstream
// alert data, so this is anything that can set a label.
func TestRemovingAnAudienceTokenCannotCreateOne(t *testing.T) {
	nested := map[string]string{
		"a": "@ch@channelannel",
		"b": "@ever@everyoneyone",
		"c": "@he@herere",
		"d": "<!chan<!channel>nel>",
	}
	v := firingView()
	v.Alerts[0].Labels = nested
	in := BuildInput(v, fixtureClock)

	for _, d := range []Dialect{SlackDialect{}, PlainDialect{}} {
		for key := range nested {
			w, _ := Compile(StanzaBody, `x {{ labels.`+key+` }} y`)
			out, _ := w.Render(in, d)
			low := strings.ToLower(out)
			for _, tok := range []string{"@channel", "@everyone", "@here", "<!channel>"} {
				if strings.Contains(low, strings.ToLower(tok)) {
					t.Errorf("%s/%s: stripping reassembled %q: %q", d.Name(), key, tok, out)
				}
			}
		}
	}
}

// TestTheWebhookRefusesAnAudienceToo. PlainDialect used to refuse four bare words
// and nothing else, so `<!channel>` and `<@U0123>` reached envelope.rendered
// verbatim — the exact laundering its own comment claimed to prevent, since a
// consumer commonly forwards oto's text into a chat product.
func TestTheWebhookRefusesAnAudienceToo(t *testing.T) {
	w, err := Compile(StanzaBody, `<!channel> <@U0123> <!subteam^S9> @everyone deploy`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	out, err := w.Render(Fixtures()[0].Input, PlainDialect{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, tok := range []string{"<!channel>", "<@U", "<!subteam^", "@everyone"} {
		if strings.Contains(strings.ToLower(out), strings.ToLower(tok)) {
			t.Errorf("a webhook consumer was handed %q: %q", tok, out)
		}
	}
	if !strings.Contains(out, "deploy") {
		t.Errorf("the words must survive: %q", out)
	}
}

// TestAnUnclosedBracketDoesNotEatTheStanza. It used to `return s[:i]`, deleting
// everything after a label containing a bare "<@" — a truncated sentence with no
// ellipsis, no link and no way to know, which is the failure text.go's truncation
// doctrine exists to refuse.
func TestAnUnclosedBracketDoesNotEatTheStanza(t *testing.T) {
	v := firingView()
	v.Alerts[0].Labels = map[string]string{"weird": "<@"}
	w, _ := Compile(StanzaBody, `KEEP {{ labels.weird }} TAIL`)
	out, err := w.Render(BuildInput(v, fixtureClock), SlackDialect{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "KEEP") || !strings.Contains(out, "TAIL") {
		t.Errorf("an unterminated bracket deleted the rest of the stanza: %q", out)
	}
}

// TestTheSameViewAlwaysRendersTheSameText. Two labels can normalise onto one key;
// ranging a Go map picks the winner at random, so the same view rendered twice
// produced two different cards. SPEC §F.1 requires purity, and oto hashes the
// rendered payload to suppress no-op chat.update calls — a wobbling hash re-sends
// the same card forever.
func TestTheSameViewAlwaysRendersTheSameText(t *testing.T) {
	v := firingView()
	v.Alerts[0].Labels = map[string]string{
		"cases.7d": "colliding-a", "cases_7d": "colliding-b", "cases-7d": "colliding-c",
	}
	v.Enrichments["alert_history"] = v.Enrichments["alert.history"]
	w, _ := Compile(StanzaBody,
		`{{ labels.cases_7d }}|{{ enrichment.alert_history.status | default: "-" }}`)

	first, err := w.Render(BuildInput(v, fixtureClock), SlackDialect{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for i := 0; i < 200; i++ {
		got, err := w.Render(BuildInput(v, fixtureClock), SlackDialect{})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got != first {
			t.Fatalf("the same view rendered two different cards:\n %q\n %q", first, got)
		}
	}
}

// TestATypoInAFilterArgumentNamesTheRightField. Liquid quotes the whole expression
// and does not say which reference was undefined; cutting at the first `|` named a
// field the author spelled CORRECTLY, planted it, made no progress, burned every
// probe, and refused the save with a false message.
func TestATypoInAFilterArgumentNamesTheRightField(t *testing.T) {
	p := Validate(StanzaBody, `{{ alert.name | default: alert.nmae }}`)
	if len(p) == 0 {
		t.Fatal("the typo was accepted")
	}
	var msgs string
	for _, one := range p {
		msgs += one.Message + "\n"
	}
	if !strings.Contains(msgs, "alert.nmae") {
		t.Errorf("the message must name the misspelled field, got:\n%s", msgs)
	}
	if strings.Contains(msgs, "alert.name is not") {
		t.Errorf("a correctly spelled field was blamed:\n%s", msgs)
	}
}

// TestASecondTypoIsNotHiddenBehindTheFirst. Any message the parser could not read
// used to `break` the whole pass, so a typo after an unparseable reference shipped.
func TestASecondTypoIsNotHiddenBehindTheFirst(t *testing.T) {
	p := Validate(StanzaBody, `{{ alert.srvice | default: "-" }} and {{ group.titel | default: "-" }}`)
	var msgs string
	for _, one := range p {
		msgs += one.Message + "\n"
	}
	for _, want := range []string{"alert.srvice", "group.titel"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("%s was not reported:\n%s", want, msgs)
		}
	}
}

// TestABacktickCannotBreakOutOfACodeSpan — slack.code() has always stripped them.
func TestABacktickCannotBreakOutOfACodeSpan(t *testing.T) {
	v := firingView()
	v.Alerts[0].Labels = map[string]string{"svc": "a`b *bold* c"}
	w, _ := Compile(StanzaBody, `{{ labels.svc | code }}`)
	out, err := w.Render(BuildInput(v, fixtureClock), SlackDialect{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Count(out, "`") != 2 {
		t.Errorf("a value broke out of its code span: %q", out)
	}
}

// TestTruncatingATimestampDoesNotPrintTheEpoch — a time mark carries the epoch and
// a fallback between delimiters; slicing it leaves a half-mark Spell cannot parse.
func TestTruncatingATimestampDoesNotPrintTheEpoch(t *testing.T) {
	w, _ := Compile(StanzaBody, `{{ group.started_at | truncate_runes: 4 }}`)
	out, err := w.Render(Fixtures()[0].Input, PlainDialect{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(out, "17734") || strings.Contains(out, "…") {
		t.Errorf("a truncated timestamp leaked its epoch: %q", out)
	}
	if !strings.Contains(out, "2026-03-14") {
		t.Errorf("the timestamp should pass through whole: %q", out)
	}
}

// TestATitleWordingCanReachTheRuleAnnotation. slack.annotation() resolves focus,
// then group, then the RULE snapshot — so a Wording that took only the focus could
// not reach the very summary the subtitle it replaces was showing.
func TestATitleWordingCanReachTheRuleAnnotation(t *testing.T) {
	v := firingView()
	v.Alerts[0].Annotations = nil
	v.Rule.Annotations = map[string]string{"summary": "from the rule snapshot"}
	w, _ := Compile(StanzaTitle, `{{ annotations.summary | default: "none" }}`)
	out, err := w.Render(BuildInput(v, fixtureClock), PlainDialect{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "from the rule snapshot") {
		t.Errorf("the rule's annotation was unreachable: %q", out)
	}
}
