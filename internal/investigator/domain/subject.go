package domain

// WHAT AN INVESTIGATION READS: oto's own history, as plain values (ADR 0053 §3: "Memory
// is not a new store: it is oto's own history — prior Findings on the same alert_key,
// the Case timeline, the rule as it stood at fire time — offered as built-in Tools").
//
// ⛔ THESE ARE COPIES, NOT THE OTHER MODULES' TYPES. `investigator` imports no other
// module (CONTEXT.md §4): `internal/app` reads a Case, its timeline and its rule
// snapshot through the alerts and rules services and hands them over in these shapes,
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

// PriorFinding is one earlier Investigation's Finding on the same alert_key.
type PriorFinding struct {
	InvestigationID  uuid.UUID
	SubjectID        uuid.UUID
	InvestigatorName string
	VersionNumber    int
	Status           Status
	Finding          string
	EndedAt          time.Time
}

// PublishedFinding is a Finding as it is published to the enrichment store
// (`investigator.<name>`), with the provenance an Enrichment carries.
type PublishedFinding struct {
	InvestigationID uuid.UUID
	CaseID          uuid.UUID
	// Enricher is `investigator.<name>`; Version is the Investigator version number,
	// which is the Enrichment's own version.
	Enricher  string
	Version   int
	VersionID uuid.UUID
	Model     ModelIdentity
	Status    Status
	Reason    Reason
	Summary   string
	Partial   bool
	Spent     Usage
	ToolCalls int
	StartedAt time.Time
	EndedAt   time.Time
}
