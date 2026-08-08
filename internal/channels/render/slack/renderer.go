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
type Renderer struct {
	clock clock.Clock
	// mentions is the fixed audience an unacked reminder addresses, from
	// channels.config.mention_on_reminder (§L.5.1).
	//
	// It is a FIXED AUDIENCE, not a rota. It must never become time-aware and it
	// must never acquire a second stage (§G.9.1). oto does not know who is on
	// call and will never pretend to.
	mentions []string
}

// Option configures a Renderer.
type Option func(*Renderer)

// WithMentions sets the fixed unacked-reminder audience for a channel-scoped
// renderer. The registry's shared renderer has none; the channels service mints a
// scoped copy when it dispatches to a channel that configured one.
func WithMentions(mentions []string) Option {
	return func(r *Renderer) {
		r.mentions = append([]string(nil), mentions...)
	}
}

// New builds the Slack renderer.
func New(clk clock.Clock, opts ...Option) *Renderer {
	if clk == nil {
		clk = clock.New()
	}
	r := &Renderer{clock: clk}
	for _, o := range opts {
		o(r)
	}
	return r
}

// For returns a channel-scoped copy of r carrying the given options. The shared
// renderer is never mutated: it is used concurrently by every dispatch worker.
func (r *Renderer) For(opts ...Option) *Renderer {
	cp := &Renderer{clock: r.clock, mentions: append([]string(nil), r.mentions...)}
	for _, o := range opts {
		o(cp)
	}
	return cp
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
	switch o.Mode {
	case domain.ModeThreadReply, domain.ModeBroadcastReply:
		payload, fallback, summary = r.renderReply(v, o)
	case domain.ModePostRoot, domain.ModeUpdateRoot:
		payload, fallback = r.renderRoot(v, o)
		summary = fallback
	default:
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

// cardState is the state the colour will encode, with the view's storm flag
// overriding the group's counts: storm mode is a visible state and outranks
// everything (§H.4).
func cardState(v *domain.NotificationView) CardState {
	if v.StormCount > 0 {
		return CardStorm
	}
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
	case reasonNewAlerts:
		return "new alerts added"
	case reasonSomeResolved:
		return "some alerts resolved"
	case reasonAllResolved:
		return "all alerts resolved"
	case reasonRepeat:
		return "still firing"
	case reasonSuppressed:
		return "silenced"
	case reasonUnsuppressed:
		return "silence ended"
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
	case reasonUnackedReminder:
		return "still unacknowledged"
	case reasonStorm:
		return "storm damping"
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
