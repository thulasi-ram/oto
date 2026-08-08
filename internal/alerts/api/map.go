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
		State:             a.State().String(),
		AckState:          a.AckState().String(),
		FirstSeenAt:       utc(a.FirstSeenAt()),
		LastSeenAt:        utc(a.LastSeenAt()),
		LastStateChangeAt: utc(a.LastStateChangeAt()),
		TotalOccurrences:  int32(a.TotalOccurrences()),
		FlapScore:         a.FlapScore(),
		IsFlapping:        a.IsFlapping(),
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
		State:      a.State().String(),
		AckState:   a.AckState().String(),
	}
}

// occurrenceDTO renders one episode.
//
// `duration_seconds` is computed against `now` rather than a fresh clock reading
// so that every row on one page is measured from the same instant — a list where
// each row asks the clock again disagrees with itself.
func occurrenceDTO(o domain.Occurrence, now time.Time) OccurrenceDTO {
	dto := OccurrenceDTO{
		ID:             o.ID(),
		AlertID:        o.AlertID(),
		GroupID:        idPtr(o.GroupID()),
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
		ReopenCount:    int32(o.ReopenCount()),
		ReopenOf:       idPtr(o.ReopenOf()),
		RuleSnapshotID: idPtr(o.RuleSnapshotID()),
		Value:          o.Value(),
		ObservedSkewMS: o.ObservedSkew().Milliseconds(),
	}
	if r := o.SuppressionReason(); !r.IsZero() {
		dto.SuppressionReason = strPtr(r.String())
	}
	if r := o.ResolveReason(); !r.IsZero() {
		dto.ResolveReason = strPtr(r.String())
	}
	secs := o.Duration(now).Seconds()
	dto.DurationSeconds = &secs
	return dto
}

func eventDTO(e domain.Event) AlertEventDTO {
	actor := e.Actor()
	return AlertEventDTO{
		ID:           e.ID(),
		AlertID:      idPtr(e.AlertID()),
		OccurrenceID: idPtr(e.OccurrenceID()),
		GroupID:      idPtr(e.GroupID()),
		Type:         e.Type().String(),
		OccurredAt:   utc(e.OccurredAt()),
		RecordedAt:   utc(e.RecordedAt()),
		ActorKind:    actor.Kind().String(),
		ActorID:      strPtr(actor.ID()),
		ActorLabel:   strPtr(actor.Label()),
		Summary:      e.Summary(),
		Payload:      e.Payload(),
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
		SubjectKind:     "alert_group",
		SubjectID:       n.GroupID,
		GroupID:         n.GroupID,
		AlertID:         n.AlertID,
		OccurrenceID:    n.OccurrenceID,
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
		AckedCount:      int32(r.Acked),
		UnackedCount:    int32(r.Unacked()),
		FlappingCount:   int32(r.Flapping),
		SnoozedCount:    int32(r.Snoozed),
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
