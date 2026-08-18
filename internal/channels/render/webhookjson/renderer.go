package webhookjson

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/thulasiram/oto/internal/channels/domain"
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
		Group:       mapGroup(v.Group),
		Alerts:      mapAlerts(v.Alerts, o.MaxInstances),
		Links:       mapLinks(v.Links),
		StormCount:  v.StormCount,
		Comment:     v.Comment,
		Summary:     summarise(v),
	}

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
		StormMode:       g.StormMode,
		FirstSeenAt:     g.FirstSeenAt.UTC(),
		LastActivityAt:  g.LastActivityAt.UTC(),
		ClusterKey:      g.ClusterKey,
		SourceGroupKey:  g.SourceGroupKey,
	}
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

// stateWord never says "resolved" when it means "expired". Losing sight of an
// alert is not the alert resolving (CONTEXT.md §3).
func stateWord(g domain.GroupView) string {
	switch {
	case g.StormMode:
		return "storm"
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
