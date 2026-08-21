package wording

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osteele/liquid"
	"github.com/osteele/liquid/expressions"
	"github.com/osteele/liquid/render"
)

// Red-team suite. Every test here is an executed attack, not a reasoned one.
//
// The ones that FAIL are demonstrated defects that survived three reviews and the
// fixes those reviews prompted; each names the fix it defeats. The ones that PASS
// are attacks that held, kept so they stay attacked.

// ---------------------------------------------------------------------------
// R1 — the audience strip is still not a fixpoint. The loop was added to
// replaceFold; stripCommonAudience as a WHOLE was not, and the bracketed-span
// removal runs after the word removal, so its join reassembles a banned token.
// ---------------------------------------------------------------------------

func TestRedTeamABracketedSpanRemovalCannotSpellAnAudience(t *testing.T) {
	labels := map[string]string{
		"j1": "@ch<@U024BE7LH>annel",
		"j2": "@he<@U1>re",
		"j3": "@every<!subteam^SAZ94GDB8>one",
	}
	v := firingView()
	v.Alerts[0].Labels = labels
	in := BuildInput(v, fixtureClock)

	for key, raw := range labels {
		w, err := Compile(StanzaBody, `x {{ labels.`+key+` }} y`)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		for _, d := range []Dialect{SlackDialect{}, PlainDialect{}} {
			out, err := w.Render(in, d)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			low := strings.ToLower(out)
			for _, tok := range []string{"@channel", "@here", "@everyone"} {
				if strings.Contains(low, tok) {
					t.Errorf("%s: removing a bracketed span spelled %q out of %q: %q",
						d.Name(), tok, raw, out)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// R2 — the bare-URL defusal is bypassed by the same reassembly. defuseLinks runs
// inside Spell's flush, BEFORE StripAudience, so any address StripAudience
// creates is never defused and lands on the card as a live link. This is the
// phishing shape ADR 0037 cites as its own motivation, from an alert label.
// ---------------------------------------------------------------------------

func TestRedTeamAnAudienceStripCannotSpellALiveURL(t *testing.T) {
	labels := map[string]string{
		"u1": "htt@channelps://evil.example/phish",
		"u2": "www@here.evil.example",
		"u3": "htt<@U1>ps://evil.example/reset",
	}
	v := firingView()
	v.Alerts[0].Labels = labels
	in := BuildInput(v, fixtureClock)

	for key, raw := range labels {
		w, _ := Compile(StanzaBody, `see {{ labels.`+key+` }}`)
		out, err := w.Render(in, SlackDialect{})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		for _, addr := range []string{"https://evil.example", "www.evil.example"} {
			if strings.Contains(out, addr) && !strings.Contains(out, "`"+addr) {
				t.Errorf("%q reassembled into a live, undefused address: %q", raw, out)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// R3 — unbalanced() counts delimiters, so a template that is balanced by COUNT
// and unbalanced by ORDER walks straight through the gate it exists to be. The
// card then carries literal Liquid syntax, which is the exact outcome the
// function's own doc comment says is "worse than a failure because nothing
// anywhere reports it".
// ---------------------------------------------------------------------------

func TestRedTeamADelimiterCannotBeBalancedByCountAlone(t *testing.T) {
	for _, src := range []string{
		`}}{{ alert.name }}{{`,
		`}}{{`,
		`%}{%`,
		`}}{{ alert.name`,
	} {
		if p := Validate(StanzaBody, src); len(p) == 0 {
			w, err := Compile(StanzaBody, src)
			if err != nil {
				continue
			}
			out, err := w.Render(Fixtures()[0].Input, SlackDialect{})
			if err == nil && (strings.Contains(out, "{{") || strings.Contains(out, "}}") ||
				strings.Contains(out, "{%") || strings.Contains(out, "%}")) {
				t.Errorf("%q saved clean and printed liquid syntax on the card: %q", src, out)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// R4 — the typo pass. referencedPaths scans the whole quoted expression for
// DOTTED tokens, which is both too much and too little:
//   - a dotted STRING LITERAL is reported as a misspelt field (false refusal);
//   - a single-segment root typo is reported by nothing at all (silent pass);
//   - the review's own D2 repro still reports neither of its two typos.
// ---------------------------------------------------------------------------

func TestRedTeamAStringLiteralIsNotAField(t *testing.T) {
	// An entirely ordinary template: a free-form annotation with a prose default.
	const src = `{{ annotations.runbook | default: "runbook.example.com" }}`
	for _, p := range Validate(StanzaBody, src) {
		if strings.Contains(p.Message, "runbook.example.com") {
			t.Errorf("a quoted string literal was refused as a misspelt field: %s", p.Message)
		}
	}
}

func TestRedTeamAMisspeltRootIsStillReported(t *testing.T) {
	// `labls` is a misspelling of `labels`, a real root. It renders empty forever.
	const src = `{{ labls | default: "-" }}`
	msgs := problemText(Validate(StanzaBody, src))
	if !strings.Contains(msgs, "labls") {
		t.Errorf("a misspelt root saved clean; the pass only looks at dotted paths:\n%s", msgs)
	}
}

func TestRedTeamABracketIndexDoesNotHideTheNextTypo(t *testing.T) {
	// Review finding D2's own stated repro. Liquid's bracket-index syntax is legal
	// and parses; the expression it quotes has no dotted token, so referencedPaths
	// returns nothing and the whole pass gives up on the first probe.
	const src = `Error on {{ labls["team"] }} for {{ alert.nmae | default: "-" }}`
	msgs := problemText(Validate(StanzaBody, src))
	if !strings.Contains(msgs, "alert.nmae") {
		t.Errorf("a typo after a bracket-index reference was never reported:\n%s", msgs)
	}
}

// TestRedTeamTheOldTypoPassAlreadyPassedTheCommittedRegression proves that
// TestASecondTypoIsNotHiddenBehindTheFirst is VACUOUS: it asserts a property the
// algorithm it claims to pin already had. The old algorithm is reimplemented here
// exactly as review-correctness.md D1/D2 describe it.
func TestRedTeamTheOldTypoPassAlreadyPassedTheCommittedRegression(t *testing.T) {
	const committed = `{{ alert.srvice | default: "-" }} and {{ group.titel | default: "-" }}`
	got := oldUnknownFields(committed)
	if len(got) != 2 || got[0] != "alert.srvice" || got[1] != "group.titel" {
		t.Fatalf("the old pass was expected to report both, got %v", got)
	}
	t.Logf("VACUOUS: the pre-fix algorithm reports %v for the committed regression input", got)

	// And the input the fix was actually written for still reports nothing, under
	// either algorithm.
	const d2 = `{{ labls["team"] }} and {{ alert.nmae }}`
	if old, now := oldUnknownFields(d2), Validate(StanzaBody, d2); len(old) == 0 && len(now) == 0 {
		t.Logf("UNFIXED: %q -> old=%v new=%v", d2, old, now)
	}
}

// oldUnknownFields is undefinedPath + unknownFields as they stood before the D1
// fix: cut liquid's message at the first `|`, give up on anything unparseable.
func oldUnknownFields(src string) []string {
	tpl, err := strictly().ParseString(src)
	if err != nil {
		return nil
	}
	probe := maximalInput()
	var out []string
	for i := 0; i < maxReferenceProbes; i++ {
		_, err := tpl.Render(liquid.Bindings(probe))
		if err == nil {
			return out
		}
		const marker = "undefined variable in {{"
		msg := err.Error()
		k := strings.Index(msg, marker)
		if k < 0 {
			return out
		}
		e := msg[k+len(marker):]
		if j := strings.Index(e, "}}"); j >= 0 {
			e = e[:j]
		}
		if j := strings.Index(e, "|"); j >= 0 {
			e = e[:j]
		}
		e = strings.TrimSpace(e)
		if e == "" || strings.ContainsAny(e, "[]()\"' ") {
			return out
		}
		plant(probe, e)
		if root := strings.SplitN(e, ".", 2)[0]; !freeFormRoots[root] {
			out = append(out, e)
		}
	}
	return out
}

func problemText(ps []Problem) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(string(p.Kind) + ": " + p.Message + "\n")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// R5 — the fit review's "no simpler public-API route" for the read-set probe.
// render.ObjectNode embeds parser.Token, whose Args field is EXPORTED and holds
// the expression source; expressions.Parse, expressions.NewContext and
// expressions.Config.StrictVariables are exported too. That is a structured,
// per-expression route with no error-prose parsing and no probe loop — and it
// isolates the bracket-index expression the current implementation gives up on.
// ---------------------------------------------------------------------------

func TestRedTeamTheReadSetHasAPublicASTRoute(t *testing.T) {
	e := liquid.NewBasicEngine()
	registerFilters(e)
	tpl, err := e.ParseString(`Error on {{ labls["team"] }} for {{ alert.nmae }} ok {{ alert.name }}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg := expressions.NewConfig()
	cfg.StrictVariables = true
	cfg.LaxFilters = true
	ctx := expressions.NewContext(maximalInput(), cfg)

	got := map[string]string{}
	var walk func(render.Node)
	walk = func(n render.Node) {
		switch v := n.(type) {
		case *render.SeqNode:
			for _, c := range v.Children {
				walk(c)
			}
		case *render.ObjectNode:
			expr, perr := expressions.Parse(v.Args)
			if perr != nil {
				got[v.Args] = perr.Error()
				return
			}
			if _, eerr := expr.Evaluate(ctx); eerr != nil {
				got[v.Args] = eerr.Error()
			} else {
				got[v.Args] = ""
			}
		}
	}
	walk(tpl.GetRoot())

	if got[`labls["team"]`] == "" {
		t.Errorf("the AST route should isolate the bracket-index expression, got %v", got)
	}
	if got[`alert.nmae`] == "" {
		t.Errorf("the AST route should isolate the typo, got %v", got)
	}
	if got[`alert.name`] != "" {
		t.Errorf("the AST route should accept a real field, got %v", got)
	}
	t.Logf("per-expression verdicts: %v", got)
}

// ---------------------------------------------------------------------------
// Attacks that HELD. Kept so they stay attacked.
// ---------------------------------------------------------------------------

// A fifty-deep filter chain, a chain as a filter argument, a template that is
// nothing but marks, and 2 048 bytes of pure interpolation.
func TestRedTeamMalformedAndEnormousTemplatesHold(t *testing.T) {
	in := Fixtures()[0].Input
	for _, tc := range []struct{ name, src string }{
		{"fifty-filters", `{{ alert.name` + strings.Repeat(` | upper | lower`, 25) + ` }}`},
		{"chain-as-default-arg", `{{ nothing | default: labels.service | default: "x" }}`},
		{"typed-arg-mismatch", `{{ alert.name | truncate_runes: labels.service }}`},
		{"all-marks", strings.Repeat(`{{ "" | bold }}`, 3)},
		{"2048-of-interpolation", strings.Repeat(`{{ alert.name }}`, 2048/len(`{{ alert.name }}`))},
		{"nested-open", `{{ {{ }}`},
		{"tag-open", `{%}}`},
	} {
		w, err := Compile(StanzaBody, tc.src)
		if err != nil {
			continue // refused at compile: a fine answer
		}
		out, err := w.Render(in, SlackDialect{})
		if err != nil {
			continue // fell back: also a fine answer
		}
		for _, r := range out {
			if (r >= '' && r <= '') || r >= 0xF0000 {
				t.Errorf("%s: a private-use mark reached the output: %U in %q", tc.name, r, out)
			}
		}
	}
}

// A 1 MB annotation and a 12 000-byte emoji string must not error, split a rune,
// or take a pathological amount of time.
func TestRedTeamEnormousValuesHold(t *testing.T) {
	v := firingView()
	v.Alerts[0].Annotations = map[string]string{
		"summary":     strings.Repeat("word ", 200000), // 1 MB
		"description": strings.Repeat("\U0001F525", 3000),
	}
	in := BuildInput(v, fixtureClock)
	for _, src := range []string{`{{ annotations.summary }}`, `{{ annotations.description }}`} {
		w, _ := Compile(StanzaBody, src)
		start := time.Now()
		out, err := w.Render(in, SlackDialect{})
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if el := time.Since(start); el > 5*time.Second {
			t.Errorf("%s took %s", src, el)
		}
		if strings.ContainsRune(out, '�') {
			t.Errorf("%s split a rune", src)
		}
	}
}

// The process-global compiled cache, hammered.
func TestRedTeamTheCompiledCacheHoldsUnderRace(t *testing.T) {
	srcs := make([]string, 64)
	for i := range srcs {
		srcs[i] = `{{ alert.name | default: "` + strings.Repeat("x", i) + `" }}`
	}
	in := Fixtures()[0].Input
	var wg sync.WaitGroup
	for g := 0; g < 128; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				src := srcs[(g+i)%len(srcs)]
				w, err := Compiled(StanzaBody, src)
				if err != nil {
					t.Errorf("compiled: %v", err)
					return
				}
				if _, err := w.Render(in, SlackDialect{}); err != nil {
					t.Errorf("render: %v", err)
					return
				}
				if _, err := Compiled(StanzaFields, src); err == nil {
					t.Error("a refused stanza compiled")
					return
				}
				_ = RenderAll(map[string]string{"body": src}, in, PlainDialect{})
				Validate(StanzaBody, `{{ alert.nmae`+strconv.Itoa(g)+` | default: "-" }}`)
			}
		}(g)
	}
	wg.Wait()
}
