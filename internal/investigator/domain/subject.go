package domain

// WHAT AN INVESTIGATION READS: oto's own history, as plain values (ADR 0053 §3: "Memory
// is not a new store: it is oto's own history — prior Findings on the same alert_key,
// the Case timeline, the rule as it stood at fire time — offered as built-in Tools").
//
// ⛔ THESE ARE COPIES, NOT THE OTHER MODULES' TYPES. `investigator` imports no other
// module (CONTEXT.md §4): `internal/app` reads a Case, its timeline and its rule
// snapshot through the alerts and rules services, and an Incident through the incidents
// service, and hands them over in these shapes,
// so what an Investigation can see is exactly what is spelled here — and a field the
// model was never shown cannot leak into a Finding.

import (
	"time"

	"github.com/google/uuid"
)

// CaseSubject is a Case as an Investigation is told about it.
type CaseSubject struct {
	CaseID uuid.UUID
	// Number is the Case's name within the org — what a human quotes.
	Number  int64
	AlertID uuid.UUID
	// AlertKey is the Alert's identity; prior Findings are looked up by it.
	AlertKey    string
	Alertname   string
	Labels      map[string]string
	Annotations map[string]string
	// State is `open` or `closed`.
	State     string
	StartedAt time.Time
	// EndedAt is zero while the Case is open.
	EndedAt time.Time
	// RuleSnapshotID is the rule as it stood at fire time, uuid.Nil when none was
	// bound.
	RuleSnapshotID uuid.UUID
}

// IncidentSubject is an Incident as an Investigation is told about it (ADR 0052, 0053
// §4): the story, and every Case that has been in it.
//
// ⛔ NO STATUS, LEAD OR SEVERITY, BECAUSE THERE IS NONE ANYWHERE (ADR 0052 §3). What an
// Incident says about itself is `Active`, read off its member Cases.
type IncidentSubject struct {
	IncidentID uuid.UUID
	// Number is the Incident's name within the org — what a human quotes.
	Number int64
	// Active is true while any current member Case is open; quiet otherwise.
	Active  bool
	DrawnAt time.Time
	// DrawnBy is who decided this is one story: a human's frozen label, or
	// "Correlator <name>".
	DrawnBy string
	// Members is every spell of every Case that has been in it, in the order they
	// joined. A removed spell carries RemovedAt.
	Members []IncidentMember
}

// IncidentMember is one spell of one Case inside an Incident.
type IncidentMember struct {
	CaseID     uuid.UUID
	CaseNumber int64
	Alertname  string
	Labels     map[string]string
	// State is the Case's own `open` or `closed`.
	State   string
	AddedAt time.Time
	// RemovedAt is zero while the Case is a member.
	RemovedAt time.Time
}

// Current reports whether the spell is still running.
func (m IncidentMember) Current() bool { return m.RemovedAt.IsZero() }

// CurrentCases is the Cases in the Incident now — the ones whose earlier Findings an
// Investigation of it reads (oto_member_findings).
func (s IncidentSubject) CurrentCases() []uuid.UUID {
	out := make([]uuid.UUID, 0, len(s.Members))
	for _, m := range s.Members {
		if m.Current() {
			out = append(out, m.CaseID)
		}
	}
	return out
}

// TimelineEntry is one thing that happened to a Case, as the timeline recorded it.
type TimelineEntry struct {
	At      time.Time
	Type    string
	Actor   string
	Summary string
}

// RuleAtFire is the alerting rule as it stood when the Case fired: the snapshot oto
// captured, content-addressed.
type RuleAtFire struct {
	CapturedAt  time.Time
	Name        string
	Group       string
	Expr        string
	For         time.Duration
	Labels      map[string]string
	Annotations map[string]string
	// Origin is where the definition came from; Confidence how sure the match is.
	Origin     string
	Confidence string
	// Available is false when oto looked and could not see the rule: the snapshot
	// records the looking, and the model must hear that rather than an empty rule.
	Available bool
}

// PriorFinding is one earlier Investigation's Finding: on the same alert_key, on the
// same subject, or on one of an Incident's member Cases.
type PriorFinding struct {
	InvestigationID  uuid.UUID
	SubjectKind      SubjectKind
	SubjectID        uuid.UUID
	InvestigatorName string
	VersionNumber    int
	Status           Status
	Finding          string
	// Classification is the class that Finding was given, "" for none (ADR 0053 §5).
	Classification string
	EndedAt        time.Time
}

// PublishedFinding is a Finding as it is published to the enrichment store
// (`investigator.<name>`) on its subject — the Case, or the Incident — with the
// provenance an Enrichment carries.
type PublishedFinding struct {
	InvestigationID uuid.UUID
	SubjectKind     SubjectKind
	SubjectID       uuid.UUID
	// Enricher is `investigator.<name>`; Version is the Investigator version number,
	// which is the Enrichment's own version.
	Enricher  string
	Version   int
	VersionID uuid.UUID
	Model     ModelIdentity
	Status    Status
	Reason    Reason
	Summary   string
	// Classification is the class the Finding was given, "" when the org had no
	// classes (ADR 0053 §5). It travels with the Finding, outbound included.
	Classification string
	Partial        bool
	Spent          Usage
	ToolCalls      int
	StartedAt      time.Time
	EndedAt        time.Time
}
