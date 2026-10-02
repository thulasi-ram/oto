package api

import (
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/incidents/domain"
)

// The wire shapes of `/api/v1/incidents*` (api/openapi/openapi.yaml, tag
// Incidents). Gate G1 diffs every struct here against its component schema.
//
// ⛔ NO DTO HERE HAS A `status`, A `lead`, A `severity` OR ANY FIELD A CLIENT
// COULD SET AN INCIDENT'S STATE WITH. `state` is on the response and on no
// request: it is derived from the member Cases on every read (ADR 0052 §3).

// IncidentAttributionDTO renders `IncidentAttributionDTO`: who decided — a human
// or an operator-written Correlator, and never both.
type IncidentAttributionDTO struct {
	Kind         string     `json:"kind"`
	Label        *string    `json:"label"`
	CorrelatorID *uuid.UUID `json:"correlator_id"`
}

// IncidentDTO renders `IncidentDTO`: one Incident with its derived state.
type IncidentDTO struct {
	ID              uuid.UUID              `json:"id"`
	Number          int64                  `json:"number"`
	State           string                 `json:"state"`
	DrawnAt         time.Time              `json:"drawn_at"`
	DrawnBy         IncidentAttributionDTO `json:"drawn_by"`
	MemberCount     int                    `json:"member_count"`
	OpenMemberCount int                    `json:"open_member_count"`
	Alertnames      []string               `json:"alertnames"`
}

// IncidentMemberDTO renders `IncidentMemberDTO`: one spell of one Case inside
// the Incident, current or tombstoned.
type IncidentMemberDTO struct {
	CaseID         uuid.UUID              `json:"case_id"`
	CaseNumber     int64                  `json:"case_number"`
	CaseState      string                 `json:"case_state"`
	AlertID        uuid.UUID              `json:"alert_id"`
	Alertname      string                 `json:"alertname"`
	Labels         map[string]string      `json:"labels"`
	AddedAt        time.Time              `json:"added_at"`
	AddedBy        IncidentAttributionDTO `json:"added_by"`
	RemovedAt      *time.Time             `json:"removed_at"`
	RemovedByLabel *string                `json:"removed_by_label"`
	MovedToNumber  *int64                 `json:"moved_to_number"`
}

// IncidentDetailDTO renders `IncidentDetailDTO`: the Incident and its whole
// membership history. It embeds IncidentDTO exactly as the contract's `allOf`
// composes it, so the two can never disagree about the summary fields.
type IncidentDetailDTO struct {
	IncidentDTO
	Members []IncidentMemberDTO `json:"members"`
	// Outbound is every external incident a destination echoed back for this
	// Incident (ADR 0052 §5, migration 00089). `[]`, never null, when there is none.
	Outbound []IncidentOutboundDTO `json:"outbound"`
}

// IncidentOutboundDTO renders `IncidentOutboundDTO`: one destination and the
// incident its tool opened for this Incident — the receipt of a delivery, never
// the external incident's state.
type IncidentOutboundDTO struct {
	ChannelID   uuid.UUID `json:"channel_id"`
	ChannelName string    `json:"channel_name"`
	ExternalURL *string   `json:"external_url"`
	ExternalID  *string   `json:"external_id"`
	RecordedAt  time.Time `json:"recorded_at"`
}

// CreateIncidentRequest is the body of `POST /api/v1/incidents`.
type CreateIncidentRequest struct {
	// CaseIDs names the Cases to draw the Incident over: at least one, because an
	// Incident is a set of one or more Cases, and at most domain.MaxCasesPerDraw.
	CaseIDs []uuid.UUID `json:"case_ids" validate:"required,min=1,max=100,unique"`
}

// AddIncidentCaseRequest is the body of `POST /api/v1/incidents/{number}/cases`.
type AddIncidentCaseRequest struct {
	CaseID uuid.UUID `json:"case_id" validate:"required"`
}

// MoveIncidentCaseRequest is the body of
// `POST /api/v1/incidents/{number}/cases/{case_id}/move`.
type MoveIncidentCaseRequest struct {
	ToNumber int64 `json:"to_number" validate:"required,min=1"`
}

// ------------------------------------------------------------------- mapping

func attributionDTO(a domain.Attribution) IncidentAttributionDTO {
	if !a.IsHuman() {
		id := a.CorrelatorID()
		return IncidentAttributionDTO{Kind: "correlator", CorrelatorID: &id}
	}
	label := a.Label()
	return IncidentAttributionDTO{Kind: "human", Label: &label}
}

func incidentDTO(i domain.Incident) IncidentDTO {
	names := i.Alertnames
	if names == nil {
		names = []string{}
	}
	return IncidentDTO{
		ID:              i.ID,
		Number:          i.Number,
		State:           i.State().String(),
		DrawnAt:         i.DrawnAt.UTC(),
		DrawnBy:         attributionDTO(i.DrawnBy),
		MemberCount:     i.MemberCount,
		OpenMemberCount: i.OpenMemberCount,
		Alertnames:      names,
	}
}

func memberDTO(m domain.Member) IncidentMemberDTO {
	labels := m.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	out := IncidentMemberDTO{
		CaseID:     m.CaseID,
		CaseNumber: m.CaseNumber,
		CaseState:  m.CaseState.String(),
		AlertID:    m.AlertID,
		Alertname:  m.Alertname,
		Labels:     labels,
		AddedAt:    m.AddedAt.UTC(),
		AddedBy:    attributionDTO(m.AddedBy),
	}
	if !m.Current() {
		at := m.RemovedAt.UTC()
		out.RemovedAt = &at
		label := m.RemovedByLabel
		out.RemovedByLabel = &label
	}
	if m.MovedToNumber > 0 {
		n := m.MovedToNumber
		out.MovedToNumber = &n
	}
	return out
}

func detailDTO(d domain.Detail) IncidentDetailDTO {
	members := make([]IncidentMemberDTO, 0, len(d.Members))
	for _, m := range d.Members {
		members = append(members, memberDTO(m))
	}
	outbound := make([]IncidentOutboundDTO, 0, len(d.Outbound))
	for _, o := range d.Outbound {
		outbound = append(outbound, IncidentOutboundDTO{
			ChannelID:   o.ChannelID,
			ChannelName: o.ChannelName,
			ExternalURL: nonEmpty(o.ExternalURL),
			ExternalID:  nonEmpty(o.ExternalID),
			RecordedAt:  o.RecordedAt.UTC(),
		})
	}
	return IncidentDetailDTO{IncidentDTO: incidentDTO(d.Incident), Members: members, Outbound: outbound}
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
