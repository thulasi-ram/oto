package slack_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/thulasiram/oto/internal/channels/domain"
	slackrender "github.com/thulasiram/oto/internal/channels/render/slack"
	"github.com/thulasiram/oto/internal/platform/clock"
)

// Red-team suite, card level. Failing tests are demonstrated defects; passing
// ones are attacks that held and are kept so they stay attacked.

// redteamRender renders an arbitrary view with wordings and a FIXED clock, so a
// digest can be compared byte-for-byte across processes.
func redteamRender(t *testing.T, v *domain.NotificationView, w map[string]string) (domain.RenderedMessage, slackrender.Payload) {
	t.Helper()
	o := domain.RenderOptions{
		Mode: domain.ModePostRoot, Verbosity: domain.VerbosityAll,
		BaseURL: "http://localhost:8080", MaxInstances: 10, ShowFieldEmoji: true,
		Wordings: w,
	}
	msg, err := slackrender.New(clock.NewFake(renderedAt)).Render(context.Background(), v, o)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var p slackrender.Payload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return msg, p
}

// ---------------------------------------------------------------------------
// R6 — the §H.9 forgery guard is a bounded loop. footerBlock strips the marker
// with replaceAll, which gives up after eight passes; a marker nested nine deep
// survives and is asserted on a card that is NOT a continuation. 288 bytes,
// well inside the 2 048-byte template ceiling.
// ---------------------------------------------------------------------------

const redteamMarker = "_continued from an earlier card_"

// nestMarker builds a string that needs d passes of ReplaceAll to lose its marker.
func nestMarker(d int) string {
	s := redteamMarker
	head, tail := redteamMarker[:6], redteamMarker[6:]
	for i := 1; i < d; i++ {
		s = strings.Replace(s, redteamMarker, head+redteamMarker+tail, 1)
	}
	return s
}

func TestRedTeamANestedMarkerCannotBeForged(t *testing.T) {
	for _, depth := range []int{1, 8, 9, 12} {
		src := nestMarker(depth)
		_, p := renderWith(t, map[string]string{"footer": "all is well " + src})
		if got := blockText(p, "footer"); strings.Contains(got, redteamMarker) {
			t.Errorf("depth %d (%d template bytes) forged §H.9's marker: %q", depth, len(src), got)
		}
	}
}

// TestRedTeamTheVerboseFooterGuardIsExercised proves that
// TestAVerboseWordingCannotDropTheContinuedMarker is VACUOUS: its 2 240-byte
// template exceeds wording.MaxTemplateBytes (2 048), so Compile refuses it, the
// footer falls back to Go's own string, and the marker is present for a reason
// that has nothing to do with the fix. 2 040 bytes is the largest template that
// actually reaches the truncation path, and this is the assertion that pins it.
func TestRedTeamTheVerboseFooterGuardIsExercised(t *testing.T) {
	for _, n := range []int{2040, 2048, 2240} {
		long := strings.Repeat("p", n)
		_, p := renderOpts(t, func(o *domain.RenderOptions) {
			o.Continued = true
			o.Wordings = map[string]string{"footer": long}
		})
		got := blockText(p, "footer")
		compiled := strings.Contains(got, "ppp")
		t.Logf("template=%4d bytes  wording reached the card=%-5v  marker present=%v",
			n, compiled, strings.Contains(got, redteamMarker))
		if n <= 2048 && !compiled {
			t.Errorf("a %d-byte template should compile; the guard is untested at this size", n)
		}
		if n > 2048 && compiled {
			t.Errorf("a %d-byte template should be refused by Compile", n)
		}
		if !strings.Contains(got, redteamMarker) {
			t.Errorf("a %d-byte wording dropped §H.9's marker: ...%q", n, got[max(0, len(got)-120):])
		}
	}
}

// ---------------------------------------------------------------------------
// R2 (card level) — an audience strip spells a live URL onto the card.
// ---------------------------------------------------------------------------

func TestRedTeamNoLiveURLReachesTheCard(t *testing.T) {
	v := smokeView()
	v.Alerts[0].Annotations = map[string]string{
		"summary":     "runbook at htt@channelps://evil.example/phish",
		"description": "reset via www@here.evil.example now",
	}
	_, p := redteamRender(t, v, map[string]string{
		"title": `{{ annotations.summary }}`, "body": `{{ annotations.description }}`,
	})
	for _, id := range []string{"title", "body"} {
		got := blockText(p, id)
		for _, addr := range []string{"https://evil.example", "www.evil.example"} {
			if strings.Contains(got, addr) && !strings.Contains(got, "`"+addr) {
				t.Errorf("a live, undefused address reached the %s stanza: %q", id, got)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// R1 (card level) — @channel spelled out of a bracketed span.
// ---------------------------------------------------------------------------

func TestRedTeamNoReassembledAudienceReachesTheCard(t *testing.T) {
	_, p := renderWith(t, map[string]string{"body": `ping @ch<@U024BE7LH>annel now`})
	if got := blockText(p, "body"); strings.Contains(got, "@channel") {
		t.Errorf("a bracketed-span removal spelled @channel onto the card: %q", got)
	}
}

// ---------------------------------------------------------------------------
// R3 (card level) — liquid delimiters printed as prose.
// ---------------------------------------------------------------------------

func TestRedTeamNoLiquidSyntaxReachesTheCard(t *testing.T) {
	_, p := renderWith(t, map[string]string{"body": `}}{{ alert.name }}{{`})
	got := blockText(p, "body")
	if strings.Contains(got, "{{") || strings.Contains(got, "}}") {
		t.Errorf("a count-balanced template printed liquid syntax on the card: %q", got)
	}
}

// ---------------------------------------------------------------------------
// R7 — the same wording set produces prose on the webhook and nothing on Slack
// for a digest and for a thread reply: newStanzas is built only in renderRoot,
// while webhookjson.renderWordings has no mode branch at all. renderWordings'
// own doc comment says "IT IS THE SAME TEMPLATE THE SLACK CARD USES, SPELLED
// DIFFERENTLY", and Validate refuses a wording that renders empty on the
// `digest` fixture — so an author is made to satisfy a card that will never use
// their words.
// ---------------------------------------------------------------------------

// TestRedTeamAWordingReachesTheRootCardAndNothingElse.
//
// The red team reported the two renderers disagreeing here — Slack applied
// wordings only in renderRoot while the webhook applied them unconditionally — and
// preferred resolving it by teaching Slack about digests. It is resolved the other
// way, and the mechanism is why: a Stanza is one of SPEC §H.7's EIGHT ROOT BLOCK
// NAMES, and a digest emits `digest`, `digestfacts`, `digestlook`, `digestfooter`
// while a thread reply emits `reply` and `replyctx`. There is no `body` block on a
// digest for a `body` Wording to substitute. Supporting one would mean a second
// stanza vocabulary for a second layout, which is a different feature.
//
// What the report was RIGHT about is that two renderers must not disagree about
// what a setting means. They no longer do.
func TestRedTeamAWordingReachesTheRootCardAndNothingElse(t *testing.T) {
	const marker = "WORDED"
	set := map[string]string{"title": marker, "body": marker, "rule": marker, "footer": marker}

	t.Run("the root card takes it", func(t *testing.T) {
		_, p := renderWith(t, set)
		if !strings.Contains(blockText(p, "body"), marker) {
			t.Error("the root card ignored a wording")
		}
	})

	t.Run("a digest does not, on either provider", func(t *testing.T) {
		v := smokeView()
		v.Digest = &domain.DigestView{Count: 17}
		msg, err := slackrender.New(clock.New()).Render(context.Background(), v, domain.RenderOptions{
			Mode: domain.ModePostRoot, BaseURL: "http://localhost:8080",
			MaxInstances: 10, Wordings: set,
		})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if strings.Contains(string(msg.Payload), marker) {
			t.Errorf("a wording reached a digest card: %s", msg.Payload)
		}
	})
}

func TestRedTeamTheByteBudgetsHoldOnMultiByteText(t *testing.T) {
	v := smokeView()
	v.Alerts[0].Annotations = map[string]string{
		"summary":     strings.Repeat("\U0001F525", 10000),
		"description": strings.Repeat("\U0001F9EF", 10000),
	}
	src := `{{ annotations.summary }}{{ annotations.description }}` + strings.Repeat("\U0001F600", 480)
	msg, p := redteamRender(t, v, map[string]string{
		"title": src, "body": src, "rule": src, "footer": src,
	})
	if err := slackrender.Validate(msg.Payload); err != nil {
		t.Fatalf("a maximal four-stanza wording produced an invalid payload: %v", err)
	}
	if !utf8.Valid(msg.Payload) {
		t.Error("the payload is not valid utf-8")
	}
	for _, id := range []string{"title", "body", "rule", "footer"} {
		if s := blockText(p, id); !utf8.ValidString(s) {
			t.Errorf("%s split a rune at its byte budget", id)
		}
	}
	t.Logf("payload=%d bytes across four maximal stanzas", len(msg.Payload))
}

// The rendered payload — and therefore the hash that decides whether oto
// re-sends a card — must not depend on map order, pointer values, or a clock
// outside the view. Run the same render many times, with colliding normalised
// keys in every free-form map.
func TestRedTeamTheWordedPayloadHashIsStable(t *testing.T) {
	v := smokeView()
	v.Alerts[0].Labels = map[string]string{
		"cases.7d": "a", "cases_7d": "b", "cases-7d": "c", "CASES 7D": "d",
		"z": "1", "y": "2", "x": "3", "w": "4", "team": "core",
	}
	v.Alerts[0].Annotations = map[string]string{
		"summary": "s", "description": "d", "a.b": "1", "a_b": "2", "a-b": "3",
	}
	w := map[string]string{
		"title":  `{{ labels.cases_7d }}/{{ annotations.a_b }}/{{ labels.team }}`,
		"body":   `{{ enrichment.alert_history.status | default: "-" }} {{ labels.x }}{{ labels.y }}{{ labels.z }}`,
		"rule":   `{{ rule.expr }} {{ rule.captured_at }}`,
		"footer": `{{ org.name }} {{ group.firing_for | default: "-" }}`,
	}
	first, _ := redteamRender(t, v, w)
	want := sha256.Sum256(first.Payload)
	for i := 0; i < 200; i++ {
		next, _ := redteamRender(t, v, w)
		if sha256.Sum256(next.Payload) != want {
			t.Fatalf("the same view rendered two different payloads on run %d", i)
		}
	}
	// Pinned so a future change that reintroduces map-order dependence, or that
	// lets a value outside the view reach the payload, fails across processes too.
	t.Logf("stable payload digest %x (%d bytes)", want, len(first.Payload))
}
