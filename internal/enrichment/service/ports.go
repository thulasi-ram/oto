package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// The ports below are declared by the CONSUMER (SPEC §F.5): this service says
// what it needs, `internal/enrichment/repository` implements the storage half,
// and `internal/app/container.go` injects adapters over the other modules'
// services for the rest. Every scoped method takes a db.TenantScope second,
// which can only be built from an authenticated principal.

// EnrichmentRepository owns `enrichments` — the provenanced record.
type EnrichmentRepository interface {
	// ListBySubject returns everything already computed about one subject.
	// It is read before a run so a retry can skip what it already has.
	ListBySubject(ctx context.Context, s db.TenantScope, subjectKind, subjectID string) ([]domain.Enrichment, error)

	// UpsertMany stores a whole phase's results in one round trip, upserting on
	// enrichments_subject_uniq (subject_kind, subject_id, enricher).
	//
	// That constraint is what makes `enrich.run` idempotent on
	// (occurrence_id, phase): re-running a phase overwrites ITS OWN rows and can
	// never accumulate duplicates or double-count a failure.
	UpsertMany(ctx context.Context, s db.TenantScope, in []domain.Enrichment) error
}

// CacheRepository owns `enrichment_cache` — the disposable layer.
//
// The two layers are not redundant. `enrichments` answers "what does oto know
// about THIS occurrence, and who computed it"; the cache answers "has anyone
// asked this same question of an upstream recently". Truncating the cache costs
// latency; truncating `enrichments` destroys the record.
type CacheRepository interface {
	// Get returns a live entry. A miss and an expired entry are both (_, false,
	// nil): an expired row is never served.
	Get(ctx context.Context, s db.TenantScope, key string) (domain.CacheEntry, bool, error)

	// Put writes an entry, overwriting any existing one for the key.
	Put(ctx context.Context, s db.TenantScope, e domain.CacheEntry) error

	// DeleteExpired evicts entries whose expiry has passed, bounded by limit.
	//
	// It takes NO TenantScope, deliberately: it is the `cache.expire`
	// maintenance sweep of SPEC §G.3, a global background job with no tenant to
	// authenticate as, over a table with no foreign key by design.
	DeleteExpired(ctx context.Context, before time.Time, limit int) (int64, error)
}

// SubjectLoader denormalises one occurrence into the Subject an Enricher is
// asked about.
//
// This is the narrow interface onto the `alerts` module. It is ONE method on
// purpose: enrichment does not need alerts' repository, its state machine or
// its timeline — it needs the frozen facts about one fire, plus the coordinates
// the async phase must quote back when it asks for the root message to be
// amended.
type SubjectLoader interface {
	LoadSubject(ctx context.Context, s db.TenantScope, occurrenceID uuid.UUID) (Loaded, error)
}

// Loaded is a Subject plus the notification coordinates that go with it.
type Loaded struct {
	// Subject is what the Enrichers see. It is denormalised so that an Enricher
	// never has to query oto's own database to know what it is enriching.
	Subject domain.Subject
	// AlertID is the Alert identity this occurrence belongs to.
	AlertID uuid.UUID
	// GroupID is the AlertGroup generation carrying it, or uuid.Nil when the
	// occurrence is not in an open group. No group means nothing to amend.
	GroupID uuid.UUID
	// StateVersion is `alert_groups.state_version` at load time. It pins the
	// enriched notification to the group state it was minted against
	// (SPEC §C.7), which is what stops a late enrichment resending an old card.
	StateVersion int
	// SourceID is the AlertSource, needed by any enricher that calls upstream.
	SourceID uuid.UUID
}

// Notifier asks for the notification to be revisited now that more is known.
//
// This is the narrow interface onto the `notification` module, and it is
// deliberately not "post a message". SPEC §H.6/§H.7 make `enriched` an
// UPDATE_ROOT reason: the root card is amended in place, and a thread reply is
// added only at verbosity=all. Enrichment states the fact; notification decides
// what, if anything, is worth saying.
//
// The pipeline calls this AT MOST ONCE per async phase, whatever the number of
// enrichers that completed. That is the coalescing rule: five slow enrichers
// finishing produce one amended card and one reply, never five replies.
type Notifier interface {
	NotifyEnriched(ctx context.Context, s db.TenantScope, n EnrichedNotice) error
}

// EnrichedNotice is the one coalesced fact the async phase reports.
type EnrichedNotice struct {
	GroupID      uuid.UUID
	AlertID      uuid.UUID
	OccurrenceID uuid.UUID
	StateVersion int
	// Enrichers names what completed, in deterministic order. It is the raw
	// material for the ":sparkles: +2 enrichments — …" context line (SPEC §H.6)
	// and is why the reply can be one line rather than one per enricher.
	Enrichers []string
}

// EventRecorder appends to the alert timeline (SPEC §D.4.1, transition T11).
//
// Optional: a nil recorder means results are still computed and still stored,
// they are merely not narrated. A timeline write must never be able to fail an
// enrichment.
type EventRecorder interface {
	// RecordEnrichmentEvent appends enrichment.completed or enrichment.failed.
	RecordEnrichmentEvent(ctx context.Context, s db.TenantScope, ev EnrichmentEvent) error
}

// EnrichmentEvent is one timeline entry about an enrichment run.
type EnrichmentEvent struct {
	// Type is enrichment.completed or enrichment.failed. The enum in SPEC
	// §D.4.1 is CLOSED; implementers must not invent types.
	Type         string
	AlertID      uuid.UUID
	OccurrenceID uuid.UUID
	// Summary is the pre-rendered one-liner for the timeline, 1..500 bytes.
	Summary string
	// Payload is the structured detail: per-enricher status and duration.
	Payload map[string]any
	// DedupeKey makes the append idempotent (SPEC §C.8).
	DedupeKey string
}
