package api

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// policyDTO maps a policy onto the wire.
//
// The wire name `escalate_after_seconds` comes from the published contract; the
// field it reads is `UnackedReminderAfter`, which is what the column, the domain
// and this codebase call it. The two names are reconciled here, in one function,
// rather than by renaming the concept back.
func policyDTO(p domain.Policy) PolicyDTO {
	matchers := make([]MatcherDTO, 0, len(p.Matchers))
	for _, m := range p.Matchers {
		matchers = append(matchers, MatcherDTO{Name: m.Name, Op: string(m.Op), Value: m.Value})
	}
	reasons := make([]string, 0, len(p.Reasons))
	for _, r := range p.Reasons {
		reasons = append(reasons, string(r))
	}
	channels := p.ChannelIDs
	if channels == nil {
		channels = []uuid.UUID{}
	}

	out := PolicyDTO{
		ID:         p.ID,
		Name:       p.Name,
		Priority:   int32(p.Priority), //nolint:gosec // bounded by policies_prio_ck
		Enabled:    p.Enabled,
		Matchers:   matchers,
		Reasons:    reasons,
		ChannelIDs: channels,
		CreatedAt:  p.CreatedAt.UTC(),
		UpdatedAt:  p.UpdatedAt.UTC(),
	}
	if p.Throttle.Enabled() {
		out.Throttle = &ThrottleDTO{
			Max:           int32(p.Throttle.Max),                  //nolint:gosec // bounded by the DTO
			WindowSeconds: int32(p.Throttle.Window / time.Second), //nolint:gosec // bounded by the DTO
		}
	}
	if p.UnackedReminderAfter > 0 {
		v := int32(p.UnackedReminderAfter / time.Second) //nolint:gosec // bounded by policies_reminder_ck
		out.EscalateAfterSeconds = &v
	}
	return out
}

// notificationDTO maps one intent onto the wire.
func notificationDTO(n domain.Notification, summary *DeliverySummaryDTO) NotificationDTO {
	out := NotificationDTO{
		ID:              n.ID,
		SubjectKind:     string(n.SubjectKind),
		SubjectID:       n.SubjectID,
		GroupID:         n.GroupID,
		AlertID:         n.AlertID,
		OccurrenceID:    n.OccurrenceID,
		Reason:          string(n.Reason),
		PolicyID:        n.PolicyID,
		StateVersion:    int32(n.StateVersion), //nolint:gosec // bounded by notifications_sver_ck
		Status:          string(n.Status),
		DeliverySummary: summary,
		CreatedAt:       n.CreatedAt.UTC(),
		UpdatedAt:       n.UpdatedAt.UTC(),
	}
	if n.SuppressedReason != "" {
		v := string(n.SuppressedReason)
		out.SuppressedReason = &v
	}
	return out
}

// summarise folds a fan-out into the counts the list carries.
//
// A `skipped` delivery counts as SENT: it means the destination already shows
// exactly this content — a coalesced no-op update — and reporting it as a failure
// would make a healthy, quiet thread look broken.
func summarise(ds []domain.Delivery) *DeliverySummaryDTO {
	if len(ds) == 0 {
		return nil
	}
	out := DeliverySummaryDTO{Total: int32(len(ds))} //nolint:gosec // bounded by the fan-out
	for _, d := range ds {
		switch d.Status {
		case domain.DeliverySent, domain.DeliverySkipped:
			out.Sent++
		case domain.DeliveryFailed:
			out.Failed++
		case domain.DeliveryDead:
			out.Dead++
		}
	}
	return &out
}

// deliveryDTO maps one materialisation onto the wire.
func deliveryDTO(d domain.Delivery, c domain.DeliveryContext) DeliveryDTO {
	out := DeliveryDTO{
		ID:                     d.ID,
		NotificationID:         d.NotificationID,
		ChannelID:              d.ChannelID,
		ChannelName:            c.ChannelName,
		ChannelType:            string(c.ChannelType),
		ThreadID:               d.ThreadID,
		Mode:                   string(d.Mode),
		Status:                 string(d.Status),
		Attempts:               int32(d.Attempts), //nolint:gosec // bounded by deliveries_attempts_ck
		NextAttemptAt:          utcPtr(d.NextAttemptAt),
		ProviderMessageID:      optionalString(d.ProviderMessageID),
		ProviderConversationID: optionalString(d.ProviderConversationID),
		Error:                  optionalString(d.Error),
		ErrorClass:             optionalString(string(d.ErrorClass)),
		Ambiguous:              d.Ambiguous,
		RenderedFallback:       optionalString(d.RenderedFallback),
		SentAt:                 utcPtr(d.SentAt),
		CreatedAt:              d.CreatedAt.UTC(),
		UpdatedAt:              d.UpdatedAt.UTC(),
	}
	if d.ThreadSeq > 0 {
		v := int32(d.ThreadSeq) //nolint:gosec // bounded by deliveries_seq_ck
		out.ThreadSeq = &v
	}
	return out
}

// deliveryDetailDTO adds the payload that was actually rendered.
func deliveryDetailDTO(d domain.Delivery, c domain.DeliveryContext) DeliveryDetailDTO {
	return DeliveryDetailDTO{
		DeliveryDTO:      deliveryDTO(d, c),
		Rendered:         rawOrNil(d.Rendered),
		RenderedHash:     optionalString(d.RenderedHash),
		ProviderResponse: rawOrNil(d.ProviderResponse),
	}
}

// ------------------------------------------------------------- request → domain

// toDraft maps a create request onto the domain command.
func (r CreatePolicyRequest) toDraft() (domain.PolicyDraft, error) {
	matchers, err := toMatchers(r.Matchers)
	if err != nil {
		return domain.PolicyDraft{}, err
	}
	reasons, err := toReasons(r.Reasons, "reasons")
	if err != nil {
		return domain.PolicyDraft{}, err
	}

	d := domain.PolicyDraft{
		Name:       r.Name,
		Enabled:    r.Enabled,
		Matchers:   matchers,
		Reasons:    reasons,
		ChannelIDs: r.ChannelIDs,
	}
	if r.Priority != nil {
		v := int(*r.Priority)
		d.Priority = &v
	}
	if r.Throttle != nil {
		t := toThrottle(*r.Throttle)
		d.Throttle = &t
	}
	if r.EscalateAfterSeconds != nil {
		v := time.Duration(*r.EscalateAfterSeconds) * time.Second
		d.UnackedReminderAfter = &v
	}
	return d, nil
}

// toPatch maps an update request onto the domain command.
func (r UpdatePolicyRequest) toPatch() (domain.PolicyPatch, error) {
	p := domain.PolicyPatch{
		Name:       r.Name,
		Enabled:    r.Enabled,
		ChannelIDs: r.ChannelIDs,
	}
	if r.Priority != nil {
		v := int(*r.Priority)
		p.Priority = &v
	}
	if r.Matchers != nil {
		ms, err := toMatchers(*r.Matchers)
		if err != nil {
			return domain.PolicyPatch{}, err
		}
		p.Matchers = &ms
	}
	if r.Reasons != nil {
		rs, err := toReasons(*r.Reasons, "reasons")
		if err != nil {
			return domain.PolicyPatch{}, err
		}
		p.Reasons = &rs
	}
	if r.Throttle.Set {
		var t *domain.Throttle
		if r.Throttle.Value != nil {
			v := toThrottle(*r.Throttle.Value)
			t = &v
		}
		p.Throttle = &t
	}
	if r.EscalateAfterSeconds.Set {
		var d *time.Duration
		if r.EscalateAfterSeconds.Value != nil {
			v := time.Duration(*r.EscalateAfterSeconds.Value) * time.Second
			d = &v
		}
		p.UnackedReminderAfter = &d
	}
	return p, nil
}

func toMatchers(in []MatcherDTO) ([]domain.Matcher, error) {
	out := make([]domain.Matcher, 0, len(in))
	for _, m := range in {
		out = append(out, domain.Matcher{Name: m.Name, Op: domain.MatchOp(m.Op), Value: m.Value})
	}
	return out, nil
}

func toThrottle(t ThrottleDTO) domain.Throttle {
	return domain.Throttle{
		Max:    int(t.Max),
		Window: time.Duration(t.WindowSeconds) * time.Second,
	}
}

// toReasons validates against the domain's CLOSED vocabulary.
//
// It is done here rather than with an `oneof` tag because migration 00018
// narrowed the set — dropping `escalation`, adding `unacked_reminder`, `snoozed`
// and `unsnoozed` — and a duplicated list in a struct tag is the second copy that
// drifts. The violation path is the JSON name with the offending index, as §L.2.2
// requires.
func toReasons(in []string, field string) ([]domain.Reason, error) {
	out := make([]domain.Reason, 0, len(in))
	var violations []errs.Violation
	for i, raw := range in {
		r := domain.Reason(raw)
		if !r.Valid() {
			violations = append(violations, errs.Violation{
				Field:   field + "/" + itoa(i),
				Code:    "enum",
				Message: "unknown notification reason " + raw,
			})
			continue
		}
		out = append(out, r)
	}
	if len(violations) > 0 {
		return nil, errs.Validation("validation_failed",
			pluralise(len(violations)), violations...)
	}
	return out, nil
}

// ------------------------------------------------------------------- helpers

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

// rawOrNil renders an absent jsonb column as JSON null rather than as the empty
// string, which is not valid JSON and would corrupt the whole response.
func rawOrNil(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func pluralise(n int) string {
	if n == 1 {
		return "1 field failed validation."
	}
	return itoa(n) + " fields failed validation."
}
