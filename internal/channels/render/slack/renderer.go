package slack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// defaultMaxInstances is the §H.3 default: render ten member instances inline,
// then "… and N more".
const defaultMaxInstances = 10

// Renderer turns a NotificationView into a Slack Block Kit payload.
//
// It is a PURE FUNCTION of its input (SPEC §F.1). It performs no I/O, holds no
// connection, and reads no clock except the injected one — and even that is only
// a fallback for a view that did not stamp RenderedAt. That is what makes it
// golden-file testable, and golden files are how a broken layout is caught at
// build time instead of at 03:00.
// ⛔ THE RENDERER HOLDS NO MENTION AUDIENCE, AND IT USED TO. There was a
// `mentions` field, a `WithMentions` option and a `For` method to mint a
// channel-scoped copy — and NOTHING EVER CALLED THEM. The registry builds one
// shared `New(clk)` and hands it to every dispatch, so
// `channels.config.mention_on_reminder` was parsed, schema-validated, rendered
// into the settings form and then silently discarded: a control an operator could
// set and that could never do anything. That is the exact trap ADR 0020 is trying
// to close, so it was closed here too.
//
// ⛔⛔ AND THEN THE REPLACEMENT WENT TOO. This paragraph used to say the audience
// "now arrives per delivery … as `RenderOptions.Mentions`". There is no such field:
// git-bug `bd0fb1d` withdrew the unacked reminder and the owner ruled the mention
// goes with it, so `RenderOptions` carries no audience and no delivery resolves one.
// oto has NO mention surface at all.
//
// ⭐ Two deletions, one comment block, and for two DIFFERENT reasons — which is why
// both halves are kept rather than collapsed. The first was a control that could
// never act (wired to nothing). The second was a mechanism that worked and was
// withdrawn on product grounds. A reader who only learns the first would conclude
// the port field is the fix, and it was, until the thing it served stopped existing.
type Renderer struct {
	clock clock.Clock
}

// New builds the Slack renderer.
func New(clk clock.Clock) *Renderer {
	if clk == nil {
		clk = clock.New()
	}
	return &Renderer{clock: clk}
}

// ID implements domain.Renderer.
func (r *Renderer) ID() domain.RendererID { return domain.RendererSlackDefault }

// Supports reports whether this renderer can serve a channel with the given
// capabilities. Capability negotiation itself is the dispatch service's job, never
// a provider's or a renderer's (§H.10).
func (r *Renderer) Supports(c domain.Capability) bool {
	return c&domain.CapRichLayout != 0
}

// Render produces the provider-native message for one delivery.
//
// On a validation failure it returns BOTH the offending message and a terminal
// error. That is deliberate: §L.6 requires the payload to be persisted in
// notification_deliveries.rendered so a dead delivery can be debugged. A caller
// that only checks the error still gets the right behaviour; a caller that wants
// the evidence has it.
func (r *Renderer) Render(
	_ context.Context, v *domain.NotificationView, o domain.RenderOptions,
) (domain.RenderedMessage, error) {
	if v == nil {
		return domain.RenderedMessage{}, errs.New(errs.KindInternal, "render_nil_view",
			"a notification view is required")
	}

	var (
		payload  Payload
		fallback string
		summary  string
	)
	switch {
	// ⭐ THE DIGEST IS DECIDED BEFORE THE MODE IS, because a digest is not a
	// transition and the mode table has nothing to say about it (§H.6 does not list
	// it; `notification/service.digestModes` is its whole rule). Its first window
	// arrives as `post_root` and every later one as `thread_reply`, and both are the
	// same fact in the same shape — so one layout serves both and neither may fall
	// through to a Case-shaped arm. Branching on `v.Digest` rather than on the Reason
	// is what makes that structural: the view says what it IS, and the only view that
	// carries a `Digest` is one built by `ViewService.digest` (git-bug `78388fb`).
	case v.Digest != nil:
		payload, fallback = r.renderDigest(v, o)
		summary = fallback
	case o.Mode == domain.ModeThreadReply:
		payload, fallback, summary = r.renderReply(v, o)
	default:
		// `post_root`, `update_root`, and anything a future mode adds: the root card
		// is the safe answer, because it is the complete one.
		payload, fallback = r.renderRoot(v, o)
		summary = fallback
	}

	raw, err := marshal(payload)
	if err != nil {
		return domain.RenderedMessage{}, errs.Wrap(err, errs.KindInternal, "render_marshal",
			"the slack payload could not be encoded")
	}

	msg := domain.RenderedMessage{
		Fallback: fallback,
		Summary:  truncateRunes(summary, 200),
		Payload:  raw,
		Hash:     hashOf(raw),
		Metadata: map[string]string{
			"renderer": string(domain.RendererSlackDefault),
			"mode":     string(o.Mode),
			"reason":   v.Reason,
		},
	}

	// Never send a message we have not proved is legal (§L.6). The message is
	// returned alongside the error so the dead-letter keeps the evidence.
	if err := Validate(raw); err != nil {
		return msg, err
	}
	return msg, nil
}

// renderedAt is the one place a clock is read. A view built at claim time carries
// its own RenderedAt (C11); the injected clock is the fallback so that a caller
// that forgot still gets a deterministic, testable time source.
func (r *Renderer) renderedAt(v *domain.NotificationView) time.Time {
	if !v.RenderedAt.IsZero() {
		return v.RenderedAt.UTC()
	}
	return r.clock.Now().UTC()
}

// cardState is the state the colour will encode. It is the group's member counts
// and nothing else: no reason and no delivery-time flag overrides what the group
// actually is (§H.4).
func cardState(v *domain.NotificationView) CardState {
	return DeriveCardState(v.Group)
}

// renderNonce derives the per-render block_id suffix required by S12.
//
// It is a hash of the view's identity rather than a clock or a random source, so
// two renders of the same fact produce the same bytes. That is what lets the
// notification module skip a no-op chat.update by comparing hashes, and it is
// what makes golden files stable.
func renderNonce(v *domain.NotificationView, o domain.RenderOptions) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	}
	write(v.Group.ID, strconv.Itoa(v.Group.Generation), v.Reason, string(o.Mode))
	write(strconv.FormatBool(o.Continued))
	write(strconv.FormatInt(v.RenderedAt.UTC().Unix(), 10))
	write(strconv.Itoa(v.Group.FiringCount), strconv.Itoa(v.Group.ResolvedCount),
		strconv.Itoa(v.Group.AckedCount), strconv.Itoa(v.Group.SuppressedCount),
		strconv.Itoa(v.Group.ExpiredCount))
	return hex.EncodeToString(h.Sum(nil))[:10]
}

// annotation reads the first non-empty annotation from the focused alert, then
// from the newest member. The renderer never queries for one (§F.1).
func annotation(v *domain.NotificationView, keys ...string) string {
	sources := make([]map[string]string, 0, 2)
	if v.Focus != nil {
		sources = append(sources, v.Focus.Annotations)
	}
	if len(v.Alerts) > 0 {
		sources = append(sources, v.Alerts[0].Annotations)
	}
	if v.Rule != nil {
		sources = append(sources, v.Rule.Annotations)
	}
	for _, key := range keys {
		for _, m := range sources {
			if val := strings.TrimSpace(m[key]); val != "" {
				return val
			}
		}
	}
	return ""
}

// oneLine collapses whitespace. A multi-line annotation inside a section field
// destroys the grid, and a PromQL expression wrapped over three lines is unreadable.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// focusField reads a field from the focused alert, falling back to the newest
// member. It exists so a group whose members disagree still shows the value that
// belongs to the fact being communicated.
func focusField(v *domain.NotificationView, get func(domain.AlertView) string) string {
	if v.Focus != nil {
		if s := get(*v.Focus); s != "" {
			return s
		}
	}
	if len(v.Alerts) > 0 {
		return get(v.Alerts[0])
	}
	return ""
}

// flappingCount counts the members whose LAST STORED flap verdict is true, for
// §H.4's `Flapping` field.
//
// ⛔ IT IS NOT A DETECTOR AND THERE IS NO LONGER ONE BEHIND IT. `AlertView.IsFlapping`
// carries `alerts.is_flapping`, which is RETIRED IN PLACE (SPEC §B.6.2, ADR 0041
// Amendment 1): the `flap.score` job and `AlertRepository.SetFlap` are deleted, so the
// column keeps the last value it was written and nothing recomputes it. Rendering it
// is right — a value on a row is history, and history is what the card is a receipt
// for — but the card must say what it is. The field `root.go` builds from this count
// reads "31 transitions" and never "flapping now", and nothing is withheld because of
// it: replies follow the Case like any other alert's.
func flappingCount(v *domain.NotificationView) (int, bool) {
	n := 0
	for _, a := range v.Alerts {
		if a.IsFlapping {
			n++
		}
	}
	return n, n > 0
}

// instanceName picks the label that identifies one member of the group. Falling
// back to the alert name is the honest last resort; falling back to a UUID would
// be a string nobody can act on.
func instanceName(a domain.AlertView) string {
	return firstNonEmpty(
		a.Labels["instance"], a.Labels["pod"], a.Labels["container"],
		a.Labels["node"], a.Labels["job"], a.Service, a.AlertName, a.AlertKey,
	)
}

// instanceDetail is the one number worth putting next to an instance.
func instanceDetail(a domain.AlertView) string {
	if a.Value != nil {
		return number(*a.Value)
	}
	if a.AckState == "acked" {
		return ":eyes: acked"
	}
	if a.State != "" && a.State != "firing" {
		return escape(a.State)
	}
	return ""
}

// actorLabel renders who did it. Slack member ids become real mentions; anything
// else is escaped, because an actor label is upstream-supplied text.
//
// ⛔ IT RETURNS "" FOR A MACHINE, AND "" IS NOT "NOBODY". oto's own agents —
// system, reconciler, ingest — are recorded on the timeline with a kind and no
// label at all (only a human actor is guaranteed an id and a label), so an empty
// answer here with `v.Actor` set means "something other than a person did this".
// Callers must never paste this straight after " by": see `by` in reply.go,
// which is the only place allowed to turn this into a clause.
func actorLabel(v *domain.NotificationView) string {
	if v.Actor == nil {
		return ""
	}
	if v.Actor.Kind == "slack_user" && isSlackUserID(v.Actor.ID) {
		return "<@" + v.Actor.ID + ">"
	}
	if v.Actor.Label != "" {
		return code(v.Actor.Label)
	}
	if v.Actor.ID != "" {
		return code(v.Actor.ID)
	}
	return ""
}

func isSlackUserID(s string) bool {
	if len(s) < 2 {
		return false
	}
	if s[0] != 'U' && s[0] != 'W' {
		return false
	}
	for _, r := range s[1:] {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// reasonPhrase is the human clause in the footer explaining why the card moved.
// A card that changes without saying why teaches an operator to distrust it.
func reasonPhrase(reason string) string {
	switch reason {
	case reasonFired:
		return "first notification"
	// ⛔ `new_alerts` ("new alerts added") AND `some_resolved` ("some alerts
	// resolved") HAD FOOTER CLAUSES HERE AND THEY ARE DELETED (git-bug 7570090).
	// Both Reasons are gone from the vocabulary: each asserted a plurality, and a
	// conversation holds one Case, which is one Alert's episode. A footer clause for
	// a Reason no row can spell is a card nobody can see.
	case reasonAllResolved:
		return "all alerts resolved"
	case reasonRepeat:
		return "still firing"
	case reasonSuppressed:
		return "silenced"
	case reasonUnsuppressed:
		return "silence ended"
	// The footer clause a snooze never had. `reasonPhrase` returning "" for
	// `snoozed` is why the amended root card moved for no stated reason at all —
	// the header changed, the fields changed, and the one line whose job is to say
	// why was absent. "A card that changes without saying why teaches an operator
	// to distrust it", and the snooze card was the worst case of it: the operator
	// distrusting it is the same operator who had just asked oto to be quiet.
	case reasonSnoozed:
		return "snoozed"
	case reasonUnsnoozed:
		return "snooze ended"
	case reasonExpired:
		return "stopped being reported"
	case reasonRefired:
		return "re-fired"
	case reasonAcked:
		return "acknowledged"
	case reasonUnacked:
		return "un-acknowledged"
	case reasonEnriched:
		return "enrichment arrived"
	case reasonRuleChanged:
		return "the rule changed"
	case reasonComment:
		return "comment added"
	default:
		return ""
	}
}

// truncateURL keeps a URL inside Slack's 3 000-char budget by dropping the query
// string rather than cutting mid-parameter, which would produce a link that looks
// valid and is not (§H.7).
func truncateURL(u string) string {
	if len(u) <= maxURL {
		return u
	}
	if i := strings.IndexByte(u, '?'); i > 0 && i <= maxURL {
		return u[:i]
	}
	return u[:maxURL]
}

// safeURL returns u if it is a URL oto may put in a message, and "" if it is not.
//
// ⛔⛔ AN UPSTREAM ANNOTATION MUST NOT BE ABLE TO SUPPRESS ITS OWN ALERT, AND IT
// COULD. `Links.Runbook` is `runbook_url` copied verbatim out of the alert
// (`internal/notification/service/view.go`), and `Links.Prometheus` is
// Alertmanager's `generatorURL`. Neither is validated anywhere upstream of here.
// Both were handed straight to `truncateURL` and emitted as a button `url` or an
// overflow option `url` — where V10 refuses anything that is not absolute
// http(s), which fails the render, which kills the WHOLE DELIVERY as a
// config_invalid dead letter.
//
// So `runbook_url: "see the wiki"` — a plausible thing for a human to write in a
// Prometheus rule — made oto silently stop delivering that alert. For an alerting
// tool that is the worst available failure: CONTEXT.md §3 is that oto's silence
// must never be indistinguishable from "there was no alert", and this was
// upstream holding the switch.
//
// Dropping the link degrades the card by one affordance. Refusing the payload
// deletes the alert. The card wins.
//
// It is also the mrkdwn escape hatch: a URL is the one string oto puts in a
// message that CANNOT be `escape`d, because `<`, `>` and `&` are legal in a URL
// and encoding them would break the link. The scheme check is therefore doing
// double duty — it is what makes `javascript:` and `<!channel>` unable to reach a
// message through this door at all.
func safeURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return ""
	}
	// A URL carrying an mrkdwn control character cannot be rendered faithfully:
	// inside `<url|label>` a `>` or a `|` terminates the link early and the tail
	// becomes visible text, and a `<` opens a second one. Percent-encoded forms
	// are untouched, which is what every real deep link uses.
	if strings.ContainsAny(u, "<>|") {
		return ""
	}
	return truncateURL(u)
}
