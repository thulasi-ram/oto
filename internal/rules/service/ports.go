package service

import (
	"context"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/rules/domain"
)

// The ports below are declared by the CONSUMER (SPEC §F.5): this service says
// what it needs, `internal/rules/repository` implements the storage half and
// `internal/app/container.go` injects an adapter over `sources/service` for the
// lookup half. Every repository method takes a db.TenantScope second, which can
// only be built from an authenticated principal, so there is no way to write a
// query that forgets its org_id.

// SnapshotRepository owns `rule_snapshots`.
//
// The table is IMMUTABLE and content-addressed: there is deliberately no Update
// and no Delete. A changed rule is a new row, which is the entire mechanism
// behind "how has this threshold drifted".
type SnapshotRepository interface {
	// Upsert stores a snapshot, returning the stored row.
	//
	// It is an INSERT ... ON CONFLICT (org_id, source_id, rule_name, rule_group,
	// rule_file, rule_fingerprint) DO NOTHING followed by the read of the
	// winner, so capturing the same rule text on every one of a thousand fires
	// costs one row and never raises a conflict error. The returned Snapshot's
	// ID and CapturedAt are those of the row that already existed when the
	// content matched, and the bool reports whether THIS call inserted it —
	// which is how the service distinguishes "a new version of the rule" from
	// "the thousandth fire of an unchanged one" without a second query.
	//
	// ⛔ THE RULE KEY IS PART OF THAT TUPLE and an implementation may not drop
	// it. `rule_fingerprint` addresses the DEFINITION only (SPEC §C.6), so it
	// is not an identity: every `unavailable` capture in a source has the same
	// empty content and therefore the same digest. Deduplicating on content
	// alone gave one firewalled Prometheus a single shared row for every rule
	// it owns, named after whichever alert failed first, which no read path
	// could then retrieve (00040).
	Upsert(ctx context.Context, s db.TenantScope, snap domain.Snapshot) (domain.Snapshot, bool, error)

	// Get returns one snapshot by id, or an errs.KindNotFound.
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Snapshot, error)

	// GetMany returns the snapshots among ids that exist in the caller's org,
	// in one round trip and in `(captured_at DESC, id DESC)` order.
	//
	// A missing id is ABSENT from the result rather than an error: the table is
	// append-only, so a miss means "not yours" or "invented", and failing the
	// batch for one of those would take down the whole page that asked (ADR
	// 0023).
	GetMany(ctx context.Context, s db.TenantScope, ids []uuid.UUID) ([]domain.Snapshot, error)

	// ListByKey returns every distinct capture for one rule key, oldest first
	// and bounded by limit. This is the edit history, and it is what the
	// version-numbered diff is computed over.
	ListByKey(ctx context.Context, s db.TenantScope, key domain.Key, limit int) ([]domain.Snapshot, error)

	// ListPage is ListByKey's paginated sibling: one KEYSET page, newest first.
	//
	// The two exist side by side because they answer different questions.
	// ListByKey rebuilds the NUMBERED history — version 1 is the oldest capture
	// and the numbering only exists relative to the whole set, so a diff cannot
	// be computed from a page. ListPage renders a list, where the whole set may
	// be larger than anything worth materialising.
	ListPage(ctx context.Context, s db.TenantScope, key domain.Key, p db.Keyset) ([]domain.Snapshot, db.Cursor, error)

	// Latest returns the newest capture for one rule key. The bool is false
	// when the rule has never been captured, which is a state and not an error.
	//
	// An `unavailable` capture counts: it is the newest thing oto knows, and
	// "we looked and could not see it" is what the read paths want to render.
	Latest(ctx context.Context, s db.TenantScope, key domain.Key) (domain.Snapshot, bool, error)

	// LatestDefinition returns the newest capture that CARRIES a definition,
	// skipping `unavailable` rows. The bool is false when this rule has never
	// been successfully recovered, which is a state and not an error.
	//
	// ⛔ DRIFT IS MEASURED AGAINST THIS ONE, NEVER AGAINST Latest. An
	// unavailable row has an empty expr and an empty rule key beyond the name,
	// so it is both the newest row and a row every query for the key matches;
	// comparing a recovered capture against it reports a rule edit on every
	// fire for the whole of an outage's recovery. Stepping OVER it rather than
	// declining to compare is equally load-bearing: an edit made while
	// Prometheus was unreachable has to be reported by the fire that can first
	// see it (domain.Drifted).
	LatestDefinition(ctx context.Context, s db.TenantScope, key domain.Key) (domain.Snapshot, bool, error)
}

// RuleLookup recovers an alert's originating rule from its upstream.
//
// It is declared here, in the consumer, with THIS module's types on both sides.
// The implementation is a thin adapter over `sources/service.ResolveRule`,
// which owns the generatorURL decoding and the /api/v1/rules matching; the
// adapter lives at the wiring seam because `rules` may not import another
// module's internals (CONTEXT.md §5.4).
//
// A lookup that recovers nothing MUST return a zero-value Recovery and a nil
// error. Failing to find a rule is the normal degraded path, not a fault, and
// an implementation that returns an error for it will produce no snapshot at
// all — which is exactly the outcome this design refuses.
type RuleLookup interface {
	Lookup(ctx context.Context, s db.TenantScope, req LookupRequest) (domain.Recovery, error)
}

// LookupRequest is one rule-recovery request.
type LookupRequest struct {
	// SourceID is the AlertSource the alert arrived from.
	SourceID uuid.UUID
	// Labels are the alert's RENDERED labels; alertname is required.
	Labels map[string]string
	// Annotations are the alert's rendered annotations.
	Annotations map[string]string
	// GeneratorURL is the primary strategy's whole input.
	GeneratorURL string
	// SkipUpstream forces the zero-API-call path. The enrichment pipeline sets
	// it when its budget is nearly spent: a generatorURL-only snapshot in time
	// beats a complete one that misses the notification.
	SkipUpstream bool
}

// EventRecorder appends to the alert timeline (SPEC §D.4.1, transition T11).
//
// It is optional: a nil recorder means the capture still happens and is still
// stored, it is merely not narrated. Timeline writes must never be able to fail
// a capture.
type EventRecorder interface {
	// RecordRuleEvent appends one of rule.snapshot_captured,
	// rule.definition_changed or rule.lookup_failed.
	RecordRuleEvent(ctx context.Context, s db.TenantScope, ev RuleEvent) error
}

// RuleEvent is one timeline entry about a rule capture.
type RuleEvent struct {
	// Type is one of the three rule.* types in SPEC §D.4.1.
	//
	// ⭐ IT IS THE KERNEL'S CLOSED ENUM AND NOT A `string`, WHICH IS WHY THE
	// COMMENT NO LONGER HAS TO SAY "implementers MUST NOT invent types". They
	// cannot: `EventType`'s only field is unexported and `NewEventType` is its only
	// parser, so an invented type is a compile error here rather than a runtime
	// KindValidation error three hops away in `alerts/service`. RULE K (§5.2b)
	// grants this package the import; the WRITE is still the port below, so this
	// remains a value crossing and not a `rules ──► alerts` module edge.
	Type kernel.EventType
	// AlertID and CaseID scope the event; at least one is required.
	AlertID uuid.UUID
	CaseID  uuid.UUID
	// SnapshotID is the snapshot this event is about, when there is one.
	SnapshotID uuid.UUID
	// Summary is the pre-rendered one-liner for the timeline, 1..500 bytes.
	Summary string
	// Payload is the structured detail (fingerprints, confidence, notes).
	Payload map[string]any
	// DedupeKey makes the append idempotent (SPEC §C.8).
	DedupeKey string
}
