package webhookjson

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/render/wording"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Renderer emits the stable oto.notification.v1 envelope.
//
// Like the Slack renderer it is a pure function of the view, but it is much less
// interesting on purpose. It has no layout to get right, no limits to respect and
// no vocabulary of its own. That is the point: the generic webhook is the control
// experiment for the Channel abstraction (R5).
type Renderer struct {
	clock clock.Clock
}

// New builds the webhook renderer.
func New(clk clock.Clock) *Renderer {
	if clk == nil {
		clk = clock.New()
	}
	return &Renderer{clock: clk}
}

// ID implements domain.Renderer.
func (r *Renderer) ID() domain.RendererID { return domain.RendererWebhookJSON }

// Supports implements domain.Renderer. A JSON envelope suits any capability set,
// including none at all.
func (r *Renderer) Supports(domain.Capability) bool { return true }

// Render builds the envelope for one delivery.
func (r *Renderer) Render(
	_ context.Context, v *domain.NotificationView, o domain.RenderOptions,
) (domain.RenderedMessage, error) {
	if v == nil {
		return domain.RenderedMessage{}, errs.New(errs.KindInternal, "render_nil_view",
			"a notification view is required")
	}

	at := v.RenderedAt
	if at.IsZero() {
		at = r.clock.Now()
	}

	env := Envelope{
		Schema:      Schema,
		Reason:      v.Reason,
		Mode:        string(o.Mode),
		Continued:   o.Continued,
		DeliveredAt: at.UTC(),
		Org:         Org{ID: v.Org.ID, Slug: v.Org.Slug, Name: v.Org.Name},
		Alerts:      mapAlerts(v.Alerts, o.MaxInstances),
		Links:       mapLinks(v.Links),
		Comment:     v.Comment,
	}

	// ⭐⭐ THE DIGEST IS DECIDED BEFORE THE GROUP IS READ, and it is decided on
	// `v.Digest` rather than on the Reason — the view says what it IS, and the only view
	// that carries a `Digest` is one `notification/service.ViewService.digest` built
	// (git-bug `78388fb`). The Slack renderer branches in the same place on the same
	// field, deliberately: two renderers agreeing on the discriminator is what stops a
	// third one inventing a rule of its own.
	//
	// ⛔ IT IS THE ARM THAT WAS MISSING WHILE THE SLACK ONE LANDED. `78388fb` moved the
	// digest's headline out of `Group.Title` — where a pre-composed sentence had been
	// smuggled so that a renderer which had never heard of a digest still drew something
	// true — and taught Slack the layout. This file kept reading `v.Group.Title`, so the
	// sentence it had been living off vanished and `summarise` fell all the way through
	// to its own defaults: `[UNKNOWN] alert group (digest)`, over a `group` object of
	// zeros. Every consumer's digest became garbage, silently, because nothing here
	// looked at `v.Digest`.
	//
	// ⚠️ `org` COMES OUT WITH EMPTY STRINGS ON A DIGEST AND THAT IS NOT FIXED HERE. The
	// tenant is absent because `ViewService.digest` reads no snapshot to take one from,
	// and that omission is argued at that seam ("a card whose destination already
	// belongs to exactly one org"). `org` is a frozen non-optional v1 key whose members
	// are all strings, so its absence surfaces as empty strings rather than as an
	// invented tenant — which is the honest shape available without a schema bump.
	env.Rendered = renderWordings(v, o, at)

	if v.Digest != nil {
		env.Digest = mapDigest(*v.Digest)
		env.Summary = digestSummary(*v.Digest)
	} else {
		g := mapGroup(v.Group)
		env.Group = &g
		env.Summary = summarise(v)
	}

	// ⚠️ EVERYTHING BELOW IS NIL ON A DIGEST VIEW BY CONSTRUCTION, not by a guard here.
	// `ViewService.digest` returns a view carrying a Reason, a `Digest` and a render
	// time and nothing else — no focus, no case, no rule, no actor, no enrichment, no
	// action and no link — so each check below already answers "absent" and the digest
	// arm needs no second copy of the tail. A future digest view that DID populate one
	// of these would be a change at that seam and would have to argue for itself there;
	// re-indenting this block into an else would only hide that argument.
	if v.Focus != nil {
		f := mapAlert(*v.Focus)
		env.Focus = &f
	}
	if v.Case != nil {
		env.Case = mapCase(*v.Case)
	}
	if v.Rule != nil {
		env.Rule = mapRule(*v.Rule)
	}
	if v.RuleChange != nil {
		env.RuleChange = mapRuleChange(*v.RuleChange)
	}
	if v.Actor != nil {
		env.Actor = &Actor{Kind: v.Actor.Kind, ID: v.Actor.ID, Label: v.Actor.Label}
	}
	if v.Previous != nil {
		env.Previous = map[string]string{"state": v.Previous.State, "ack_state": v.Previous.AckState}
	}
	if len(v.Enrichments) > 0 {
		env.Enrichments = make(map[string]Enrichment, len(v.Enrichments))
		for k, e := range v.Enrichments {
			env.Enrichments[k] = Enrichment{
				Enricher:   e.Enricher,
				Status:     e.Status,
				Payload:    e.Payload,
				Warnings:   e.Warnings,
				Error:      e.Error,
				ComputedAt: e.ComputedAt.UTC(),
			}
		}
	}
	// Buttons on a non-interactive channel degrade to links (§H.10). A value-only
	// action has nowhere to go on a webhook, so it is dropped rather than sent as
	// an affordance the consumer cannot use.
	for _, a := range v.Actions {
		if a.URL == "" {
			continue
		}
		env.Actions = append(env.Actions, Action{ID: a.ID, Label: a.Label, URL: a.URL})
	}

	raw, err := marshal(env)
	if err != nil {
		return domain.RenderedMessage{}, errs.Wrap(err, errs.KindInternal, "render_marshal",
			"the webhook envelope could not be encoded")
	}

	sum := sha256.Sum256(raw)
	msg := domain.RenderedMessage{
		Fallback: env.Summary,
		Summary:  env.Summary,
		Payload:  raw,
		Hash:     hex.EncodeToString(sum[:]),
		Metadata: map[string]string{
			"renderer": string(domain.RendererWebhookJSON),
			"schema":   Schema,
			"mode":     string(o.Mode),
			"reason":   v.Reason,
		},
	}

	if err := Validate(raw); err != nil {
		return msg, err
	}
	return msg, nil
}

func mapGroup(g domain.GroupView) Group {
	return Group{
		ID:              g.ID,
		GroupKey:        g.GroupKey,
		Generation:      g.Generation,
		Title:           g.Title,
		Receiver:        g.Receiver,
		GroupLabels:     g.GroupLabels,
		State:           g.State,
		Severity:        g.Severity,
		FiringCount:     g.FiringCount,
		SuppressedCount: g.SuppressedCount,
		ResolvedCount:   g.ResolvedCount,
		ExpiredCount:    g.ExpiredCount,
		TotalCount:      g.TotalCount,
		AckedCount:      g.AckedCount,
		FirstSeenAt:     g.FirstSeenAt.UTC(),
		LastActivityAt:  g.LastActivityAt.UTC(),
		ClusterKey:      g.ClusterKey,
		SourceGroupKey:  g.SourceGroupKey,
	}
}

// mapDigest projects the digest's facts and NOTHING ELSE.
//
// ⭐ IT COPIES A COUNT AND A SPAN AND COMPOSES NO SENTENCE, which is the shape
// `78388fb` was about. `DigestView` carries facts precisely so that each channel can
// lay them out its own way; a webhook consumer wants the number and the two instants
// in machine form, and the one human sentence it also gets lives in `summary` where
// every other envelope's sentence lives.
//
// ⛔ THE SPAN IS COPIED OR LEFT NIL — NEVER DERIVED, NEVER DEFAULTED. `DigestView`
// documents both instants as zero on a digest written before migration 00070, and a
// zero `time.Time` would marshal as `0001-01-01T00:00:00Z`: a valid, UTC, in-spec
// timestamp that `Validate` would pass and every consumer would believe. The pair is
// therefore mapped through pointers, so "not recorded" is an ABSENT key and can never
// be mistaken for a span in the year 1.
func mapDigest(d domain.DigestView) *Digest {
	out := &Digest{Count: d.Count}
	if d.CoveredFrom.IsZero() || d.CoveredTo.IsZero() {
		// Both or neither: half a span is not a narrower answer than none, it is an
		// unbounded one, and a consumer given only `covered_from` would read it as
		// "everything since".
		return out
	}
	from, to := d.CoveredFrom.UTC(), d.CoveredTo.UTC()
	span := to.Sub(from).Seconds()
	out.CoveredFrom, out.CoveredTo, out.SpanSeconds = &from, &to, &span
	return out
}

func mapAlerts(in []domain.AlertView, limit int) []Alert {
	if limit <= 0 || limit > len(in) {
		limit = len(in)
	}
	out := make([]Alert, 0, limit)
	for _, a := range in[:limit] {
		out = append(out, mapAlert(a))
	}
	return out
}

func mapCase(o domain.CaseView) *Case {
	out := &Case{
		ID:                o.ID,
		Seq:               o.Seq,
		State:             o.State,
		AckState:          o.AckState,
		SuppressionReason: o.SuppressionReason,
		ResolveReason:     o.ResolveReason,
		StartedAt:         o.StartedAt.UTC(),
		DurationSeconds:   o.Duration.Seconds(),
		ReopenCount:       0, // frozen v1 key, see envelope.go

		AckedByLabel: o.AckedByLabel,
		AckNote:      o.AckNote,
	}
	if o.EndedAt != nil {
		t := o.EndedAt.UTC()
		out.EndedAt = &t
	}
	if o.AckedAt != nil {
		t := o.AckedAt.UTC()
		out.AckedAt = &t
	}
	return out
}

func mapRule(r domain.RuleView) *Rule {
	return &Rule{
		SnapshotID:          r.SnapshotID,
		Fingerprint:         r.Fingerprint,
		File:                r.File,
		Group:               r.Group,
		Name:                r.Name,
		Expr:                r.Expr,
		ForSeconds:          r.For.Seconds(),
		KeepFiringForSecond: r.KeepFiringFor.Seconds(),
		Labels:              r.Labels,
		Annotations:         r.Annotations,
		Origin:              r.Origin,
		MatchConfidence:     r.MatchConfidence,
		CapturedAt:          r.CapturedAt.UTC(),
	}
}

func mapRuleChange(rc domain.RuleChangeView) *RuleChange {
	return &RuleChange{
		PreviousSnapshotID:  rc.PreviousSnapshotID,
		PreviousFingerprint: rc.PreviousFingerprint,
		PreviousCapturedAt:  rc.PreviousCapturedAt.UTC(),
		ExprChanged:         rc.ExprChanged,
		PreviousExpr:        rc.PreviousExpr,
		NewExpr:             rc.NewExpr,
		ForChanged:          rc.ForChanged,
		PreviousForSeconds:  rc.PreviousFor.Seconds(),
		NewForSeconds:       rc.NewFor.Seconds(),
		LabelDiff:           rc.LabelDiff,
		AnnotationDiff:      rc.AnnotationDiff,
	}
}

// mapLinks flattens Links into a map so a consumer can iterate it and so adding a
// link never breaks a schema-validating receiver. Empty links are omitted.
func mapLinks(l domain.Links) map[string]string {
	out := make(map[string]string, 10)
	put := func(k, v string) {
		if v != "" {
			out[k] = v
		}
	}
	put("group", l.Group)
	put("alert", l.Alert)
	put("timeline", l.Timeline)
	put("prometheus", l.Prometheus)
	put("alertmanager", l.Alertmanager)
	put("alertmanager_silence_new", l.AlertmanagerSilenceNew)
	put("runbook", l.Runbook)
	put("grafana_dashboard", l.GrafanaDashboard)
	put("grafana_panel", l.GrafanaPanel)
	put("grafana_image", l.GrafanaImage)
	if len(out) == 0 {
		return nil
	}
	return out
}

// summarise is the envelope's one human sentence, so a consumer that only wants a
// string has one that is already written. It is the same discipline as the Slack
// top-level text, without the Slack.
func summarise(v *domain.NotificationView) string {
	title := v.Group.Title
	if title == "" {
		title = v.Group.GroupLabels["alertname"]
	}
	if title == "" {
		title = "alert group"
	}
	parts := []string{"[" + strings.ToUpper(stateWord(v.Group)) + "]", title}
	if v.Group.ClusterKey != "" {
		parts = append(parts, "on "+v.Group.ClusterKey)
	}
	if v.Group.TotalCount > 0 {
		parts = append(parts, "— "+strconv.Itoa(v.Group.FiringCount)+" of "+
			strconv.Itoa(v.Group.TotalCount)+" instances firing")
	}
	if v.Reason != "" {
		parts = append(parts, "("+v.Reason+")")
	}
	return strings.Join(parts, " ")
}

// digestSummary is a digest's one human sentence — the `summary` a consumer that only
// wants a string reads, and the `Fallback` the delivery writes to satisfy
// `deliveries_fb_ck`.
//
// ⭐ IT IS WRITTEN HERE RATHER THAN CARRIED IN THE VIEW, and that relocation is the
// whole of `78388fb`. The old design pre-composed this sentence in
// `notification/service` and rode it in `Group.Title` — the one field that could not be
// left empty, because an empty title produced an empty fallback and failed the CHECK
// with a 23514 AFTER the message had gone out. The constraint was real; it is met here
// now, in the renderer, which is where a sentence about layout belongs.
//
// ⚠️ IT SAYS "UP TO" AND NOT "TO", for the reason the `Digest` type states at length:
// `covered_to` is exclusive, and prose that reads as a closed span teaches the one
// human reading it to double-count a boundary the machine fields do not.
//
// It deliberately does NOT reuse `summarise`'s `[<STATE>]` prefix vocabulary. A reader
// (or a grep) that has learned to scan for `[FIRING]` / `[RESOLVED]` must not read a
// digest as a sixth state, so the bracketed word is `[DIGEST]` — the same choice, for
// the same reason, that the Slack card's top-level text makes.
func digestSummary(d domain.DigestView) string {
	cases := " new cases"
	if d.Count == 1 {
		cases = " new case"
	}
	parts := []string{"[DIGEST]", strconv.Itoa(d.Count) + cases}
	if d.CoveredFrom.IsZero() || d.CoveredTo.IsZero() {
		// The absence is stated rather than skipped, exactly as the Slack card states
		// it. A sentence that simply stopped after the count would read as a digest
		// covering some window the reader is left to guess at, and the guess available
		// — the policy's window today — is the one inference this pair exists to
		// prevent.
		return strings.Join(append(parts, "in a window whose span was not recorded"), " ")
	}
	from, to := d.CoveredFrom.UTC(), d.CoveredTo.UTC()
	// RFC 3339 UTC, because that is this envelope's only timestamp dialect (§L.6's W2
	// check enforces it on every instant-shaped string in the payload) and because a
	// digest's span is routinely not on the reader's own day: a recovered tick emits up
	// to `MaxDigestBackfill` windows in one pass, so a bare clock time would be
	// ambiguous in exactly the case a reader is least able to resolve.
	parts = append(parts,
		"in "+to.Sub(from).String(),
		"from "+from.Format(time.RFC3339),
		"up to "+to.Format(time.RFC3339))
	return strings.Join(parts, " ")
}

// stateWord never says "resolved" when it means "expired". Losing sight of an
// alert is not the alert resolving (CONTEXT.md §3).
func stateWord(g domain.GroupView) string {
	switch {
	case g.FiringCount > 0 && g.AckedCount < g.FiringCount:
		return "firing"
	case g.FiringCount > 0:
		return "acknowledged"
	case g.SuppressedCount > 0:
		return "suppressed"
	case g.ExpiredCount > 0:
		return "expired"
	case g.ResolvedCount > 0:
		return "resolved"
	default:
		return "unknown"
	}
}

func marshal(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// renderWordings produces the customer's per-Stanza prose for a webhook consumer.
//
// ⭐ IT IS THE SAME TEMPLATE THE SLACK CARD USES, SPELLED DIFFERENTLY. A Wording is
// text and text is portable; what is not portable is punctuation. PlainDialect
// drops every emphasis mark and renders a timestamp as oto's UTC string rather
// than as Slack's <!date^…> token, so a consumer receives values it can process
// instead of one product's markup it would have to strip.
//
// ⛔ A FAILING WORDING OMITS ITS KEY RATHER THAN EMITTING AN ERROR OR AN EMPTY
// STRING. The Slack renderer falls back to oto's own text because a card must say
// something; there is no equivalent here, because every fact a Stanza could
// mention is ALREADY in the envelope as a structured field. An absent key is the
// truthful rendering of "the customer's prose for this stanza did not render",
// and it is a smaller claim than an empty string a consumer would print.
func renderWordings(v *domain.NotificationView, o domain.RenderOptions, at time.Time) map[string]string {
	if len(o.Wordings) == 0 {
		return nil
	}
	in := wording.BuildInput(v, at)
	out := make(map[string]string, len(o.Wordings))
	for _, id := range wording.AllStanzas {
		src, ok := o.Wordings[string(id)]
		if !ok || src == "" || !id.Wordable() {
			continue
		}
		w, err := wording.Compile(id, src)
		if err != nil {
			continue
		}
		text, err := w.Render(in, wording.PlainDialect{})
		if err != nil {
			continue
		}
		out[string(id)] = text
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
