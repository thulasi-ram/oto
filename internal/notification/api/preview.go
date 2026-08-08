package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// previewNotificationPolicy serves POST /api/v1/notification-policies/preview.
//
// ⛔ IT SENDS NOTHING, WRITES NO ROW AND ENQUEUES NO JOB.
//
// This endpoint is what stops a policy change becoming an outage, so its output
// is deliberately EXPLANATORY rather than minimal. It answers four questions:
//
//  1. WHICH POLICY MATCHED — and, in `warnings`, which policies it SHADOWED, and
//     which clause of a near-miss failed. "Why did this go to #general?" is
//     almost always a shadowing question, and a preview that reported only the
//     winner would answer the easy half.
//  2. WHICH DESTINATIONS RESULTED — every channel the winner names, live or not,
//     with the §H.6 mode each would receive.
//  3. WHICH SUPPRESSORS WOULD APPLY — per destination, with the reason named in
//     the §B.8.2 precedence order rather than a bare "nothing would be sent".
//  4. WHAT WOULD ACTUALLY GO OUT — rendered by the REAL renderer, from the same
//     view a delivery builds, so the operator sees the card and not a mock-up.
//
// The dry run deliberately does NOT consult a snooze, a storm or a throttle: those
// are properties of a MOMENT, and a preview that changed its answer between two
// clicks would be worse than no preview at all. `warnings` says so explicitly
// rather than leaving the operator to infer it.
func (rt *Router) previewNotificationPolicy(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.preview != nil, "preview_unavailable",
		"policy preview is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto, err := httpx.Bind[PolicyPreviewRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	reason, err := previewReason(dto.Reason)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	groupID, err := rt.resolveSubject(r.Context(), scope, dto)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// ONE read serves both halves: the labels the matcher runs against and the
	// view the renderer turns into a payload. Reading them separately would let
	// the preview show a card built from labels other than the ones it matched on.
	view, viewErr := rt.buildView(r.Context(), scope, groupID, dto, reason)

	labels := map[string]string{}
	if view != nil {
		labels = view.Group.GroupLabels
	}

	req := service.PreviewRequest{Reason: reason, Labels: labels}
	if dto.Policy != nil {
		draft, derr := dto.Policy.toDraft()
		if derr != nil {
			httpx.WriteProblem(w, r, derr)
			return
		}
		candidate := materialise(scope, draft)
		if verr := candidate.Validate(); verr != nil {
			httpx.WriteProblem(w, r, verr)
			return
		}
		req.Policy = &candidate
	}

	res, err := rt.preview.Preview(r.Context(), scope, req)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := rt.previewDTO(r.Context(), res, view, reason)
	if viewErr != nil {
		// A failure to build the view costs the RENDERED payload and nothing else.
		// The routing answer — who is told and where — is the part that stops an
		// outage, and refusing to give it because a card could not be drawn would
		// be the wrong trade.
		out.Warnings = append(out.Warnings,
			"the card could not be rendered for this subject, so `rendered` is omitted: "+
				safeMessage(viewErr))
	}
	httpx.Data(w, r, http.StatusOK, out, started)
}

// previewReason resolves the reason to simulate, defaulting to `fired`.
func previewReason(raw string) (domain.Reason, error) {
	if raw == "" {
		return domain.ReasonFired, nil
	}
	r := domain.Reason(raw)
	if !r.Valid() {
		return "", errs.Validation("validation_failed", "1 field failed validation.",
			errs.Violation{Field: "reason", Code: "enum", Message: "unknown notification reason " + raw})
	}
	return r, nil
}

// resolveSubject turns exactly one of alert_id / occurrence_id / group_id into
// the group generation whose card would carry the fact.
//
// Routing is about the GROUP: the thing being routed is the group's card, and
// routing two members of one group to two different channels would split one
// conversation across two rooms.
func (rt *Router) resolveSubject(
	ctx context.Context, scope db.TenantScope, dto PolicyPreviewRequest,
) (uuid.UUID, error) {
	supplied := 0
	for _, p := range []*uuid.UUID{dto.AlertID, dto.OccurrenceID, dto.GroupID} {
		if p != nil && *p != uuid.Nil {
			supplied++
		}
	}
	if supplied != 1 {
		return uuid.Nil, errs.Validation("validation_failed", "1 field failed validation.",
			errs.Violation{
				Field: "group_id", Code: "required",
				Message: "supply exactly one of alert_id, occurrence_id or group_id",
			})
	}

	if dto.GroupID != nil && *dto.GroupID != uuid.Nil {
		return *dto.GroupID, nil
	}
	if rt.subjects == nil {
		// Refusing is honest. Previewing against the wrong subject would produce a
		// confidently wrong answer, which is the one thing this endpoint must
		// never do.
		return uuid.Nil, errs.Validation("validation_failed", "1 field failed validation.",
			errs.Violation{
				Field: "group_id", Code: "required",
				Message: "this deployment can only preview by group_id",
			})
	}
	if dto.AlertID != nil {
		return rt.subjects.GroupIDForAlert(ctx, scope, *dto.AlertID)
	}
	return rt.subjects.GroupIDForOccurrence(ctx, scope, *dto.OccurrenceID)
}

// buildView projects the subject into the renderer's read model.
//
// The notification handed to the view builder is UNSAVED and has no id: it exists
// only to name the subject and the reason. Nothing here writes it, which is why a
// preview can be run against production as often as an operator likes.
func (rt *Router) buildView(
	ctx context.Context, scope db.TenantScope, groupID uuid.UUID,
	dto PolicyPreviewRequest, reason domain.Reason,
) (*service.NotificationView, error) {
	if rt.views == nil {
		return nil, errs.Unavailable("preview_view_unavailable",
			"this deployment cannot build a notification view", 0)
	}
	return rt.views.Build(ctx, scope, service.ViewRequest{
		Notification: domain.Notification{
			OrgID:        scope.OrgID(),
			SubjectKind:  domain.SubjectAlertGroup,
			SubjectID:    groupID,
			GroupID:      groupID,
			AlertID:      dto.AlertID,
			OccurrenceID: dto.OccurrenceID,
			Reason:       reason,
		},
	})
}

// previewDTO renders the dry-run answer.
func (rt *Router) previewDTO(
	ctx context.Context, p service.Preview, view *service.NotificationView, reason domain.Reason,
) PolicyPreviewDTO {
	out := PolicyPreviewDTO{
		Matched: p.Matched != nil,
		Results: []PolicyPreviewResultDTO{},
	}

	var (
		policyID   uuid.UUID
		policyName string
	)
	if p.Matched != nil {
		policyID, policyName = p.Matched.ID, p.Matched.Name
	}

	for _, d := range p.Destinations {
		mode := domain.ModePostRoot
		if len(d.Modes) > 0 {
			mode = d.Modes[0]
		}
		row := PolicyPreviewResultDTO{
			PolicyID:    policyID,
			PolicyName:  policyName,
			ChannelID:   d.ChannelID,
			ChannelName: d.ChannelName,
			ChannelType: string(d.ChannelType),
			Mode:        string(mode),
			WouldSend:   d.Live && len(d.Modes) > 0,
		}

		// Per-destination suppression, named rather than implied. A disabled
		// channel is `channel_disabled`; a live channel whose verbosity drops
		// every mode for this reason is `verbosity`. Both are first-class answers.
		switch {
		case !d.Live:
			row.SuppressedReason = strPtr(string(domain.SuppressedChannelDisabled))
		case len(d.Modes) == 0:
			row.SuppressedReason = strPtr(string(domain.SuppressedVerbosity))
		}

		if row.WouldSend && view != nil {
			if msg, ok := rt.render(ctx, d, view, mode); ok {
				row.Rendered = msg.Payload
				row.RenderedFallback = strPtr(msg.Fallback)
			}
		}
		out.Results = append(out.Results, row)
	}

	out.Warnings = append(out.Warnings, previewWarnings(p, reason)...)
	return out
}

// render produces the EXACT payload that would go out, using the real renderer.
//
// A render failure is swallowed into "no payload" rather than failing the
// request: outbound validation rejecting a card is itself useful information, and
// it is reported in `warnings` by the caller rather than losing the routing
// answer.
func (rt *Router) render(
	ctx context.Context, d service.PreviewDestination,
	view *service.NotificationView, mode domain.Mode,
) (service.RenderedMessage, bool) {
	if rt.renderers == nil {
		return service.RenderedMessage{}, false
	}
	renderer, err := rt.renderers.Renderer(service.ProviderType(d.ChannelType), "")
	if err != nil {
		return service.RenderedMessage{}, false
	}
	msg, err := renderer.Render(ctx, view, service.RenderOptions{
		Mode:           service.RenderMode(mode),
		BaseURL:        rt.baseURL,
		ShowFieldEmoji: true,
	})
	if err != nil {
		return service.RenderedMessage{}, false
	}
	return msg, true
}

// previewWarnings turns the matcher's verdicts into sentences an operator can act
// on.
//
// This is the part that makes the endpoint worth having. `matched: false` tells
// you nothing about WHY; "policy `critical → #sre` did not match: matcher `team`
// failed" tells you which clause to fix, and "policy `everything → #noise` was not
// reached because `critical → #sre` matched first" tells you about the shadowing
// you did not know existed.
func previewWarnings(p service.Preview, reason domain.Reason) []string {
	var out []string

	if p.Matched == nil {
		out = append(out, "no enabled policy handles reason `"+string(reason)+
			"` for these labels, so this fact would notify nobody and would be recorded as suppressed with `no_policy`")
	}
	for _, o := range p.Outcomes {
		switch o.Verdict {
		case "matched":
			out = append(out, "policy `"+o.PolicyName+"` matched and claimed this fact; "+
				"evaluation stops at the first match, so no later policy is consulted")
		case "not_reached":
			out = append(out, "policy `"+o.PolicyName+"` was NOT reached: an earlier policy matched first")
		case "reason_not_handled":
			out = append(out, "policy `"+o.PolicyName+"` does not list reason `"+string(reason)+"`")
		case "matcher_failed":
			out = append(out, "policy `"+o.PolicyName+"` did not match: matcher `"+o.FailedMatcher+"` failed")
		case "matcher_invalid":
			out = append(out, "policy `"+o.PolicyName+"` has an unusable matcher `"+o.FailedMatcher+
				"`: its regular expression does not compile, and it is SKIPPED during evaluation")
		}
	}
	if p.Suppressed != "" {
		out = append(out, "nothing would be sent: `"+string(p.Suppressed)+"`")
	}

	// The honest caveat. A preview that quietly ignored the time-dependent
	// dampers would read as a guarantee, and it is not one.
	out = append(out, "this dry run evaluates ROUTING only. Snooze, storm damping, flap damping and the "+
		"per-subject throttle are properties of the moment a notification is minted and are not applied "+
		"here, so a real send may additionally be suppressed by one of them — visibly, with a recorded reason")
	return out
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

// safeMessage renders an error for a caller. It uses the errs.Message, which is
// always safe to show, and never the wrapped cause, which is for logs.
func safeMessage(err error) string {
	if e, ok := errs.As(err); ok && e.Message != "" {
		return e.Message
	}
	return "the subject could not be read"
}
