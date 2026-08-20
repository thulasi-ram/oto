package api

import (
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/alerts/service"
)

// The domain → DTO mappers.
//
// Every field is copied by hand. That is deliberate: an embedded struct or a
// shared type would mean a rename in the domain silently renaming a JSON field,
// and the contract would drift from the server without anybody editing either.

func alertDTO(a domain.Alert) AlertDTO {
	return AlertDTO{
		ID:                a.ID(),
		AlertKey:          a.Key().String(),
		SourceFingerprint: a.Fingerprint().String(),
		AlertName:         a.AlertName(),
		Severity:          strPtr(a.Severity().String()),
		Namespace:         strPtr(a.Namespace()),
		Service:           strPtr(a.Service()),
		ClusterKey:        a.ClusterKey().String(),
		Labels:            emptyMap(a.Labels().Map()),
		Annotations:       emptyMap(a.Annotations().Map()),
		GeneratorURL:      strPtr(a.GeneratorURL()),
		// ⭐ THE DISPLAY READING, NOT THE STORED ONE (ADR 0041). `alerts.state`
		// narrowed to `firing | resolved | expired` so that every AGGREGATE counts
		// a silenced firing alert as firing; a human looking at ONE row still wants
		// to be told nobody is being paged about it, and `suppressed` is the word
		// this product has always used. The contract is therefore unchanged — the
		// four values, the chip and `?state=suppressed` all still mean what they
		// meant — and the column underneath them stopped hiding firing alerts.
		State:             a.DisplayState().String(),
		FirstSeenAt:       utc(a.FirstSeenAt()),
		LastSeenAt:        utc(a.LastSeenAt()),
		LastStateChangeAt: utc(a.LastStateChangeAt()),
		TotalCases:        int32(a.TotalCases()),
		FlapScore:         a.FlapScore(),
		IsFlapping:        a.IsFlapping(),
		Synthetic:         a.Synthetic(),
	}
}

func alertRefDTO(a domain.Alert) AlertRefDTO {
	return AlertRefDTO{
		ID:         a.ID(),
		AlertKey:   a.Key().String(),
		AlertName:  a.AlertName(),
		Severity:   strPtr(a.Severity().String()),
		Namespace:  strPtr(a.Namespace()),
		ClusterKey: a.ClusterKey().String(),
		State:      a.DisplayState().String(),
	}
}

// caseDTO renders one episode.
//
// `duration_seconds` is computed against `now` rather than a fresh clock reading
// so that every row on one page is measured from the same instant — a list where
// each row asks the clock again disagrees with itself.
func caseDTO(o domain.Case, now time.Time) CaseDTO {
	dto := CaseDTO{
		ID:             o.ID(),
		AlertID:        o.AlertID(),
		Seq:            int32(o.Seq()),
		State:          o.State().String(),
		AckState:       o.AckState().String(),
		AckedByLabel:   strPtr(o.AckedByLabel()),
		AckedAt:        timePtr(o.AckedAt()),
		AckNote:        strPtr(o.AckNote()),
		StartedAt:      utc(o.StartedAt()),
		EndedAt:        timePtr(o.EndedAt()),
		LastObservedAt: utc(o.LastObservedAt()),
		SourceStartsAt: utc(o.SourceStartsAt()),
		SourceEndsAt:   timePtr(o.SourceEndsAt()),
		RuleSnapshotID: idPtr(o.RuleSnapshotID()),
		Value:          o.Value(),
		ObservedSkewMS: o.ObservedSkew().Milliseconds(),
	}
	if r := o.SuppressionReason(); !r.IsZero() {
		dto.SuppressionReason = strPtr(r.String())
	}
	// The witnesses, when Alertmanager named any. `suppression_reason: silenced`
	// says a silence is muting the alert; this says WHICH one, which is the half
	// an operator can act on.
	if sb := o.SuppressedBy(); !sb.IsZero() {
		dto.SuppressedBy = &SuppressedByDTO{
			SilencedBy:  sb.SilencedBy,
			InhibitedBy: sb.InhibitedBy,
			MutedBy:     sb.MutedBy,
		}
	}
	if r := o.ResolveReason(); !r.IsZero() {
		dto.ResolveReason = strPtr(r.String())
	}
	secs := o.Duration(now).Seconds()
	dto.DurationSeconds = &secs
	return dto
}

// caseListItemDTO renders one row of the org-wide case list: the episode, plus
// the identity it belongs to.
//
// The alert arrives from the map the service batch-loaded beside the page. A
// zero Alert cannot occur — the repository proved the row's alert is in the
// caller's org before returning it — and `alertRefDTO` of a zero value would
// still marshal, so the honest thing is to render what was handed over rather
// than to invent a fallback for a state the query cannot produce.
func caseListItemDTO(o domain.Case, a domain.Alert, now time.Time) CaseListItemDTO {
	return CaseListItemDTO{
		CaseDTO: caseDTO(o, now),
		Alert:   alertRefDTO(a),
	}
}

func eventDTO(e domain.Event) AlertEventDTO {
	actor := e.Actor()
	return AlertEventDTO{
		ID:         e.ID(),
		AlertID:    idPtr(e.AlertID()),
		CaseID:     idPtr(e.CaseID()),
		GroupID:    idPtr(e.GroupID()),
		Type:       e.Type().String(),
		OccurredAt: utc(e.OccurredAt()),
		RecordedAt: utc(e.RecordedAt()),
		ActorKind:  actor.Kind().String(),
		ActorID:    strPtr(actor.ID()),
		ActorLabel: strPtr(actor.Label()),
		Summary:    e.Summary(),
		Payload:    e.Payload(),
	}
}

func enrichmentDTO(e service.EnrichmentSummary) EnrichmentDTO {
	return EnrichmentDTO{
		ID:              e.ID,
		SubjectKind:     e.SubjectKind,
		SubjectID:       e.SubjectID,
		Enricher:        e.Enricher,
		EnricherVersion: int32(e.EnricherVersion),
		Phase:           int32(e.Phase),
		Status:          e.Status,
		Payload:         emptyPayload(e.Payload),
		Warnings:        e.Warnings,
		Error:           strPtr(e.Error),
		DurationMS:      int32(e.DurationMS),
		FromCache:       e.FromCache,
		ComputedAt:      utc(e.ComputedAt),
		ExpiresAt:       e.ExpiresAt,
	}
}

// enrichmentSummaryDTO is the compact row of the alert detail view.
//
// `headline` is read out of the payload when the enricher put one there. It is
// never synthesised: a summary line oto invented would be indistinguishable from
// one the enricher stands behind.
func enrichmentSummaryDTO(e service.EnrichmentSummary) EnrichmentSummaryDTO {
	out := EnrichmentSummaryDTO{
		Enricher:        e.Enricher,
		EnricherVersion: int32(e.EnricherVersion),
		Status:          e.Status,
		ComputedAt:      utc(e.ComputedAt),
	}
	if v, ok := e.Payload["headline"].(string); ok && v != "" {
		out.Headline = &v
	}
	return out
}

// notificationDTO renders one intent plus its delivery roll-up.
//
// `subject_kind` is the constant `alert_group`: v1 notifies about group
// generations only, which is why `subject_id` is the group id.
func notificationDTO(n service.NotificationSummary) NotificationDTO {
	summary := DeliverySummaryDTO{
		Total:   int32(n.DeliveriesTotal),
		Sent:    int32(n.DeliveriesSent),
		Failed:  int32(n.DeliveriesFailed),
		Dead:    int32(n.DeliveriesDead),
		Skipped: int32(n.DeliveriesSkipped),
		Pending: int32(n.DeliveriesPending),
	}
	// `pending` is DERIVED only when the producer supplies none, and the
	// derivation is exact rather than a guess: sent, failed, dead and pending
	// exhaust the delivery states, so whatever `total` has left over is queued or
	// in flight. It is documented as such in the contract.
	if n.DeliveriesPending == 0 {
		if pending := n.DeliveriesTotal - n.DeliveriesSent - n.DeliveriesFailed - n.DeliveriesDead; pending > 0 {
			summary.Pending = int32(pending)
		}
	}

	dto := NotificationDTO{
		ID:              n.ID,
		SubjectKind:     n.SubjectKind,
		SubjectID:       n.SubjectID,
		AlertID:         n.AlertID,
		CaseID:          n.CaseID,
		PolicyID:        n.PolicyID,
		Reason:          n.Reason,
		StateVersion:    int32(n.StateVersion),
		Status:          n.Status,
		DeliverySummary: &summary,
		CreatedAt:       utc(n.CreatedAt),
		// ⛔ NEVER CreatedAt. A projection that has no updated_at says so, with
		// null. Substituting the creation instant made "never changed" and
		// "changed a minute ago" indistinguishable, which is exactly the kind of
		// quiet lie a system of record cannot afford.
		UpdatedAt: timePtr(n.UpdatedAt),
	}
	if n.SuppressedReason != "" {
		dto.SuppressedReason = strPtr(n.SuppressedReason)
	}
	return dto
}

// deliverySummaryDTO renders one subject's fan-out health.
//
// ⛔ IT NEVER RETURNS NIL AND THE CALLER NEVER OMITS IT. An all-zero summary is a
// real answer — "nobody has been told anything about this" — and it is the answer
// an operator most needs when Slack is quiet. Omitting the field instead, which
// is what oto did until now, makes that state indistinguishable from a page that
// simply never computed it.
//
// `skipped` is counted BOTH separately and inside `sent`, exactly as the contract
// describes: a skipped delivery means the destination already shows this content,
// which is a healthy quiet thread rather than a failure.
func deliverySummaryDTO(r service.DeliveryRollup) DeliverySummaryDTO {
	out := DeliverySummaryDTO{
		Total:   int32(r.Total),   //nolint:gosec // bounded by the fan-out
		Sent:    int32(r.Sent),    //nolint:gosec // bounded by the fan-out
		Failed:  int32(r.Failed),  //nolint:gosec // bounded by the fan-out
		Dead:    int32(r.Dead),    //nolint:gosec // bounded by the fan-out
		Skipped: int32(r.Skipped), //nolint:gosec // bounded by the fan-out
		Pending: int32(r.Pending), //nolint:gosec // bounded by the fan-out
	}
	if r.LastErrorClass != "" {
		out.LastErrorClass = strPtr(r.LastErrorClass)
	}
	if r.LastSentAt != nil {
		v := r.LastSentAt.UTC()
		out.LastSentAt = &v
	}
	return out
}

func snoozeDTO(s domain.Snooze) SnoozeDTO {
	return SnoozeDTO{
		ID:             s.ID(),
		SnoozedAt:      utc(s.SnoozedAt()),
		SnoozedUntil:   utc(s.SnoozedUntil()),
		SnoozedByLabel: s.SnoozedByLabel(),
		Note:           strPtr(s.Note()),
		EndedAt:        timePtr(s.EndedAt()),
	}
}

// withSnooze attaches the §B.8 quiet period to an alert row.
//
// ⭐ IT IS SET FROM THE `alert_snoozes` ROW, WHICH IS NOW THE ONLY PLACE A
// SNOOZE EXISTS. There was once a bare `alerts.snoozed_until` mirror beside it —
// deliberately bare, because a person reference on a signal row is the one door
// §D.4.0 keeps shut — and being bare is exactly why it had to go: it could render
// a countdown and could never say who asked or why. Both are shown wherever a
// snooze is shown (§B.8.1): a quiet period nobody can be asked about is the
// silent suppression §B.6 forbids.
//
// A snooze whose clock has run out but which `snooze.expire` has not yet swept
// is deliberately still rendered. It is a fact about the ROW, `ended_at` is
// still null, and pretending otherwise would make the list and the detail page
// disagree for up to a minute.
func withSnooze(dto *AlertDTO, s domain.Snooze, ok bool) {
	if !ok {
		return
	}
	v := snoozeDTO(s)
	dto.Snooze = &v
}

// activeSnoozeDTO renders one row of the §B.8.6 ORG-WIDE view.
//
// `remaining_seconds` is measured against `now` — one clock reading shared by
// every row on the page — rather than against a fresh reading per row, for the
// same reason `duration_seconds` is: a banner whose rows disagree about what time
// it is has already stopped being trustworthy.
func activeSnoozeDTO(s domain.Snooze, alert *domain.Alert, now time.Time) ActiveSnoozeDTO {
	out := ActiveSnoozeDTO{
		SnoozeDTO:        snoozeDTO(s),
		AlertID:          s.AlertID(),
		AlertKey:         s.AlertKey().String(),
		RemainingSeconds: s.RemainingAt(now).Seconds(),
	}
	if alert != nil {
		ref := alertRefDTO(*alert)
		out.Alert = &ref
	}
	return out
}

// snoozeHistoryDTO renders one row of the §B.8.6 history.
//
// `ended_reason` is copied and never derived: `expired`, `manual` and
// `superseded` are three different stories about the same ending, and a
// history that could not tell them apart would not be worth keeping.
func snoozeHistoryDTO(s domain.Snooze) SnoozeHistoryDTO {
	out := SnoozeHistoryDTO{
		SnoozeDTO: snoozeDTO(s),
		Active:    s.EndedAt().IsZero(),
	}
	if r := s.EndedReason(); !r.IsZero() {
		out.EndedReason = strPtr(r.String())
	}
	out.EndedByLabel = strPtr(s.EndedByLabel())
	return out
}

// unsnoozeAlertsDTO renders the account of a bulk wake.
//
// ⭐ `requested` IS DERIVED FROM `results` AND NOT CARRIED SEPARATELY, so the
// invariant the contract promises — `woken + skipped == requested ==
// results.length` — holds by construction rather than by two writers agreeing.
// A number that could disagree with the list beside it is a number a client would
// eventually have to distrust.
//
// ⛔ `Results` IS ALWAYS A LIST AND NEVER `null`. The contract declares it
// required and non-nullable, and a nil Go slice marshals to `null`, which is the
// one shape a caller iterating the account cannot handle.
func unsnoozeAlertsDTO(res service.UnsnoozeManyResult) UnsnoozeAlertsDTO {
	out := UnsnoozeAlertsDTO{
		Requested: len(res.Outcomes),
		Woken:     res.Woken(),
		Skipped:   res.Skipped(),
		Results:   make([]UnsnoozeOutcomeDTO, 0, len(res.Outcomes)),
	}
	for _, o := range res.Outcomes {
		row := UnsnoozeOutcomeDTO{AlertID: o.AlertID, Outcome: outcomeSkipped}
		if o.Woken {
			row.Outcome = outcomeWoken
		} else {
			// The refusal's own code, so a surface can say "already awake" and
			// "no longer here" in different words. It is nil on a wake, because
			// there is nothing to explain.
			row.Reason = strPtr(o.Code)
		}
		out.Results = append(out.Results, row)
	}
	return out
}

// The two members of `UnsnoozeOutcomeDTO.outcome`. They are constants because the
// contract closes the enum, and a typo in a string literal is a response no
// generated client can parse.
const (
	outcomeWoken   = "woken"
	outcomeSkipped = "skipped"
)

// rollupDTO renders one §E.3a bucket.
//
// `state` is the domain's roll-up: a bucket is as alive as its liveliest member,
// and `resolved` and `expired` are never merged, because "the upstream said it
// ended" and "we stopped hearing about it" are different facts and the second is
// the more interesting one.
func rollupDTO(r domain.AlertRollup, by string) AlertRollupDTO {
	sev := make(map[string]int32, len(r.SeverityCounts))
	for k, v := range r.SeverityCounts {
		sev[k] = int32(v)
	}
	return AlertRollupDTO{
		Key:             r.Key,
		GroupBy:         by,
		State:           r.RollupState().String(),
		TotalCount:      int32(r.Total),
		FiringCount:     int32(r.Firing),
		SuppressedCount: int32(r.Suppressed),
		ResolvedCount:   int32(r.Resolved),
		ExpiredCount:    int32(r.Expired),
		FlappingCount:   int32(r.Flapping),
		SeverityCounts:  sev,
		FirstSeenAt:     utc(r.FirstSeenAt),
		LastSeenAt:      utc(r.LastSeenAt),
	}
}

// promotedLabels are the individually indexed columns. Filtering on one is
// markedly cheaper than filtering on an arbitrary label, and the filter bar says
// so rather than letting the operator find out during an incident.
var promotedLabels = map[string]bool{
	"alertname": true, "severity": true, "namespace": true, "service": true, "cluster": true,
}

// casePolicyDTO maps one retention rule onto the wire.
//
// The seconds ⇄ Duration conversion happens here and nowhere else, and the wire
// name is `retention_window_seconds` rather than the column's
// `retention_window_s`: a DTO field name is what a client maps onto a control, and
// the column spelling is one no client has ever been sent.
func casePolicyDTO(p domain.CasePolicy) CasePolicyDTO {
	return CasePolicyDTO{
		ID:        p.ID,
		Namespace: p.Namespace,
		Alertname: p.Alertname,
		//nolint:gosec // bounded by case_policy_window_ck
		RetentionWindowSeconds: int32(p.RetentionWindow / time.Second),
		CreatedAt:              utc(p.CreatedAt),
		UpdatedAt:              utc(p.UpdatedAt),
	}
}

// toCasePolicyDraft maps a create request onto the domain command.
func (r CreateCasePolicyRequest) toCasePolicyDraft() domain.CasePolicyDraft {
	return domain.CasePolicyDraft{
		Namespace:       r.Namespace,
		Alertname:       r.Alertname,
		RetentionWindow: time.Duration(r.RetentionWindowSeconds) * time.Second,
	}
}

// toCasePolicyPatch maps an update request onto the domain command.
func (r UpdateCasePolicyRequest) toCasePolicyPatch() domain.CasePolicyPatch {
	var p domain.CasePolicyPatch
	if r.RetentionWindowSeconds != nil {
		d := time.Duration(*r.RetentionWindowSeconds) * time.Second
		p.RetentionWindow = &d
	}
	return p
}

func labelNameDTO(l domain.LabelCount) LabelNameDTO {
	return LabelNameDTO{
		Name:       l.Value,
		AlertCount: int32(l.Count),
		Promoted:   promotedLabels[l.Value],
	}
}

func labelValueDTO(l domain.LabelCount) LabelValueDTO {
	return LabelValueDTO{Value: l.Value, AlertCount: int32(l.Count)}
}

// ------------------------------------------------------------------ helpers

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func idPtr(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func utc(t time.Time) time.Time { return t.UTC() }

func emptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func emptyPayload(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
