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
	// Conversation reports that the Correlator which drew this Incident says its
	// Incidents are conversations (ADR 0052 §6). Read when the fact is evaluated, as
	// everything else here is; a human-drawn Incident is never one.
	Conversation bool
	// Outbound is every external incident a destination echoed back for this
	// Incident (ADR 0052 §5's outbound mapping, git-bug 506ff21), one per channel,
	// oldest first. Empty when no receiver echoed anything — the ordinary case.
	Outbound []IncidentOutbound
	// Finding is the latest Finding an Investigation of the Incident reached (ADR 0053
	// §4: "the latest is shown"), or nil when none has. Read at claim time like
	// everything here, so a `finding` fact whose run a newer one has since overtaken
	// carries the newer Finding — the card is the Incident as it is now (C11).
	//
	// ⛔ READ, NEVER ROUTED ON. Nothing in evaluation consults it: a Finding changes
	// what a card says, never whether the fact is sent (ADR 0053 §2).
	Finding *IncidentFinding
}

// IncidentFinding is an Incident's latest Finding as a card carries it: what was
// concluded, by which Investigator version, and when — a snapshot, never live state.
type IncidentFinding struct {
	InvestigationID uuid.UUID
	// Investigator is the Investigator's name; Version the version that concluded it.
	Investigator string
	Version      int
	Summary      string
	// Classification is the class the Finding was given — one of the org's own, or
	// `unclassified` — and "" when the org had no classes (ADR 0053 §5). A model's
	// judgement: carried, never routed on.
	Classification string
	// Partial is true when a budget stopped the run before it concluded.
	Partial     bool
	ConcludedAt time.Time
}

// IncidentOutbound is one receipt: the incident a destination's tool opened for
// this Incident, as that tool named it in its 2xx response.
type IncidentOutbound struct {
	ChannelName string
	ExternalURL string
	ExternalID  string
}

// IncidentReceipt is what the dispatcher records when an Incident fact's delivery
// is answered with an echo (migration 00089). The two external values are already
// validated by the provider that read them.
type IncidentReceipt struct {
	IncidentID  uuid.UUID
	ChannelID   uuid.UUID
	DeliveryID  uuid.UUID
	ExternalURL string
	ExternalID  string
	RecordedAt  time.Time
}

// IncidentRef names the Incident conversation a Case's fact belongs in (ADR 0052
// §6): the Incident's id, which keys its `channel_threads` row. The number a
// human quotes is not carried: the card reads it off the Incident at claim time.
type IncidentRef struct {
	ID uuid.UUID
}

// Member returns the CURRENT spell of the given Case, if it is in the Incident now.
func (f IncidentFacts) Member(caseID uuid.UUID) (IncidentMemberFacts, bool) {
	for _, m := range f.Members {
		if m.CaseID == caseID && m.Current() {
			return m, true
		}
	}
	return IncidentMemberFacts{}, false
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
