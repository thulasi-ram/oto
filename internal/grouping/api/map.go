package api

import (
	"time"

	"github.com/google/uuid"

	alertdomain "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/grouping/domain"
)

// The domain → DTO mappers. Every field is copied by hand, so that renaming a
// domain accessor can never silently rename a JSON field.

func groupDTO(g domain.Group) GroupDTO {
	labels := g.GroupLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	c := g.Counts()

	dto := GroupDTO{
		ID:         g.ID(),
		GroupKey:   g.Key().String(),
		Generation: int32(g.Generation()),
		SourceID:   g.SourceID(),
		// ⛔ NOT group_labels["cluster"]. The cluster key is now first-class on
		// the domain entity, resolved from `cluster_id` through `clusters`.
		// Reading it out of the group labels made it vanish the moment an
		// operator changed Alertmanager's `group_by`.
		ClusterKey:      g.ClusterKey(),
		SourceGroupKey:  strPtr(g.SourceGroupKey()),
		Receiver:        g.Receiver(),
		GroupLabels:     labels,
		Title:           g.Title(),
		State:           g.State().String(),
		Severity:        strPtr(g.Severity()),
		StateVersion:    int32(g.StateVersion()),
		FiringCount:     int32(c.Firing),
		SuppressedCount: int32(c.Suppressed),
		ResolvedCount:   int32(c.Resolved),
		ExpiredCount:    int32(c.Expired),
		TotalCount:      int32(c.Total),
		AckedCount:      int32(c.Acked),
		StormMode:       g.StormMode(),
		StormSince:      timePtr(g.StormSince()),
		FirstSeenAt:     utc(g.FirstSeenAt()),
		LastActivityAt:  utc(g.LastActivityAt()),
		ClosedAt:        timePtr(g.ClosedAt()),
	}
	dto.LastNotificationReason = strPtr(g.LastNotificationReason())
	return dto
}

func alertDTO(a alertdomain.Alert) AlertDTO {
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

func alertRefDTO(a alertdomain.Alert) AlertRefDTO {
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

func eventDTO(e alertdomain.Event) AlertEventDTO {
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
