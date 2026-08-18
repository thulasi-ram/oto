package domain

import (
	"context"
	"time"
)

// Phase says when an Enricher runs relative to the first notification.
type Phase int

// The two phases.
const (
	// PhaseInline runs inside the pre-notification budget: 2 000 ms total for all
	// inline enrichers, run concurrently. The budget is a CEILING, never a wait —
	// stragglers are recorded as timed out and the notification proceeds anyway.
	PhaseInline Phase = 1
	// PhaseAsync runs after the first notification; its result triggers an update,
	// not a reply.
	PhaseAsync Phase = 2
)

// Status is the outcome of one enrichment run. Every result is provenanced: a
// missing enrichment is a recorded fact, never a silent gap.
type Status string

// The enrichment outcomes.
const (
	// StatusOK means the Enricher produced a complete result.
	StatusOK Status = "ok"
	// StatusPartial means it produced some of what it looked for.
	StatusPartial Status = "partial"
	// StatusSkipped means Applicable said no.
	StatusSkipped Status = "skipped"
	// StatusFailed means it errored.
	StatusFailed Status = "failed"
	// StatusTimeout means it exceeded its budget and was re-enqueued as PhaseAsync.
	StatusTimeout Status = "timeout"
)

// Enricher is a named, versioned producer of derived context about an Alert.
// Registered once at boot. v1 ships exactly four: prom.rule, runbook,
// alert.history and silence.match.
//
// There is deliberately no DependsOn: no v1 enricher depends on another, and
// adding a DAG requires a SPEC amendment.
type Enricher interface {
	Name() string // stable id: "prom.rule", "alert.history", "silence.match", "runbook"
	Version() int // bump => cache invalidation + re-run on next case
	Phase() Phase
	Timeout() time.Duration
	Applicable(s *Subject) bool
	Enrich(ctx context.Context, s *Subject) (Result, error)
}

// Subject is what an Enricher is asked about. It is denormalised so that an
// Enricher never has to query oto's own database to know what it is enriching.
type Subject struct {
	OrgID       string
	SubjectKind string // "alert" | "case" | "group"
	SubjectID   string
	Alert       AlertSnapshot
	Case        CaseSnapshot
	Source      SourceRef
	// Prior carries results from already-completed enrichers in this run.
	Prior map[string]Result
}

// AlertSnapshot is the Alert an enrichment is about, frozen at dispatch.
type AlertSnapshot struct {
	ID, AlertKey, SourceFingerprint                     string
	AlertName, Severity, Namespace, Service, ClusterKey string
	Labels, Annotations                                 map[string]string
	GeneratorURL                                        string
}

// CaseSnapshot is the firing episode an enrichment is about.
type CaseSnapshot struct {
	ID             string
	Seq            int
	State          string
	StartedAt      time.Time
	SourceStartsAt time.Time
}

// SourceRef is the AlertSource an Enricher may call back into.
type SourceRef struct {
	ID, ClusterID, ClusterKey string
	BaseURL, PrometheusURL    string
	Kind                      string
}

// Result is one Enricher's provenanced output.
type Result struct {
	Status   Status
	Payload  any    // typed struct, marshalled to JSONB
	CacheKey string // "" => not cacheable
	TTL      time.Duration
	Warnings []string
}
