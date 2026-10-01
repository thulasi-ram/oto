package domain

import (
	"time"

	"github.com/google/uuid"
)

// IncidentFacts is an Incident as the notification layer reads it (ADR 0052 §5):
// enough to route a fact about it, and enough to build its card at claim time.
//
// ⭐ IT IS A STRUCT COPY OF `incidents/domain.Detail`, AND THE COPY IS THE PRICE OF
// THE BOUNDARY. `notification` never imports `incidents` (CONTEXT.md §4); it
// declares the port it needs (`service.IncidentReader`) and `internal/app` maps the
// one onto the other, exactly as `notificationReader` maps the other way for
// `alerts`.
//
// ⛔ THERE IS NO STATUS, LEAD OR SEVERITY HERE BECAUSE THERE IS NONE ANYWHERE. What
// an Incident says about itself inside oto is `Active` — derived from its member
// Cases — and nothing else.
type IncidentFacts struct {
	ID     uuid.UUID
	Number int64
	// Active is true while any CURRENT member Case is open (ADR 0052 §3).
	Active  bool
	DrawnAt time.Time
	// DrawnByLabel and DrawnByCorrelator are the two possible authors; exactly one
	// is set.
	DrawnByLabel      string
	DrawnByCorrelator uuid.UUID
	// Members is every spell of every Case that has been in the Incident, current
	// and removed, in the order they joined.
	Members []IncidentMemberFacts
}

// IncidentMemberFacts is one spell of one Case inside an Incident.
type IncidentMemberFacts struct {
	CaseID            uuid.UUID
	CaseNumber        int64
	CaseOpen          bool
	AlertID           uuid.UUID
	Alertname         string
	Labels            map[string]string
	AddedAt           time.Time
	AddedByLabel      string
	AddedByCorrelator uuid.UUID
	// RemovedAt is zero while the Case is a member.
	RemovedAt      time.Time
	RemovedByLabel string
	MovedToNumber  int64
}

// Current reports whether the spell is still running.
func (m IncidentMemberFacts) Current() bool { return m.RemovedAt.IsZero() }

// MatchLabels is the label set a policy is evaluated against for an Incident
// fact: the labels EVERY current member Case's Alert carries with the same value.
//
// ⭐ IT IS THE INTERSECTION, FOR THE REASON THE GROUP'S AXES ONCE WERE THE INPUT.
// A policy routes the whole story, and the only labels that are true OF the story
// are the ones all of its Cases agree on — `namespace=checkout` when every member
// fired in checkout, nothing about `pod` when each fired on a different one. A
// union would let one member's label route the whole Incident somewhere the other
// members never belonged.
//
// ⚠️ AN INCIDENT WITH NO CURRENT MEMBER HAS NO LABELS, AND THAT IS CORRECT: a
// policy with matchers does not claim it, and a catch-all policy — the shape ADR
// 0052 §5 names, "every Incident goes to incident.io" — still does. Never nil, so a
// caller cannot tell "no labels" from "no Incident" by shape.
func (f IncidentFacts) MatchLabels() map[string]string {
	var out map[string]string
	for _, m := range f.Members {
		if !m.Current() {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(m.Labels))
			for k, v := range m.Labels {
				out[k] = v
			}
			continue
		}
		for k, v := range out {
			if m.Labels[k] != v {
				delete(out, k)
			}
		}
	}
	if out == nil {
		out = map[string]string{}
	}
	return out
}
