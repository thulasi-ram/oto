package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
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
	// (case_id, phase): re-running a phase overwrites ITS OWN rows and can
	// never accumulate duplicates or double-count a failure.
	UpsertMany(ctx context.Context, s db.TenantScope, in []domain.Enrichment) error
}

// CacheRepository owns `enrichment_cache` — the disposable layer.
//
// The two layers are not redundant. `enrichments` answers "what does oto know
// about THIS case, and who computed it"; the cache answers "has anyone
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

// SubjectLoader denormalises one case into the Subject an Enricher is
// asked about.
//
// This is the narrow interface onto the `alerts` module. It is ONE method on
// purpose: enrichment does not need alerts' repository, its state machine or
// its timeline — it needs the frozen facts about one fire, plus the coordinates
// the async phase must quote back when it asks for the root message to be
// amended.
type SubjectLoader interface {
	LoadSubject(ctx context.Context, s db.TenantScope, caseID uuid.UUID) (Loaded, error)
}

// Loaded is a Subject plus the notification coordinates that go with it.
type Loaded struct {
	// Subject is what the Enrichers see. It is denormalised so that an Enricher
	// never has to query oto's own database to know what it is enriching.
	Subject domain.Subject
	// AlertID is the Alert identity this case belongs to.
	AlertID uuid.UUID
	// CaseID is the firing episode this run is about, and it is what the
	// notification coordinates are keyed on.
	//
	// ⛔ IT WAS `GroupID uuid.UUID` — "the AlertGroup generation carrying it, or
	// uuid.Nil when the case is not in an open group" (git-bug `7570090`). A
	// conversation holds exactly one Case, so the Case IS the conversation and
	// there is no container left for an episode to be in or absent from.
	//
	// ⭐ THE `uuid.Nil` GUARD SURVIVES THE RENAME AND STILL MEANS SOMETHING, which
	// is why this is a typed field rather than a re-parse of `Subject.Case.ID`. The
	// old guard read "no group means nothing to amend"; the new one reads "no case
	// means no conversation to amend", and a loader that could not resolve the
	// episode has to be able to say so instead of handing a zero id downstream.
	CaseID uuid.UUID
	// StateVersion is the version the enriched notification is pinned to, so a late
	// enrichment amends the card it was minted against instead of resending an
	// older one (SPEC §C.7). It feeds `NotifyEvaluateArgs.StateVersion`, which
	// feeds `notifications_idem_uniq`.
	//
	// ⛔⛔ IT WAS `alert_groups.state_version` AND ITS SOURCE IS GONE (git-bug
	// `7570090`). The group carried the version; the Case has no version column, so
	// the adapter in `internal/app` that used to fill this has nothing to read and
	// leaves it 0. THE FIELD IS KEPT RATHER THAN DELETED BECAUSE THE GUARANTEE IS
	// STILL OWED, and deleting it would erase the question along with the answer.
	//
	// ⚠️ WHAT A CONSTANT 0 COSTS, stated plainly so nobody has to rediscover it from
	// a support ticket. The key is sha256(org, subject_kind, subject_id, reason,
	// state_version), so every `enriched` evaluation for one Case now hashes
	// IDENTICALLY, forever. The FIRST amendment is minted; every later one collides
	// on the unique index and is swallowed silently, because a 23505 there is the
	// mechanism working (§L.9). A slow enricher finishing second amends nothing.
	// Note this is the OPPOSITE failure to the one the pin prevented — not a stale
	// card resent, but a fresh card never sent — and only the Case carrying its own
	// monotonic version fixes it.
	//
	//oto:retired `alert_groups.state_version` was the only column that ever fed this
	// and the table is deleted; the adapter in `internal/app` that filled it has
	// nothing left to read. The field is KEPT because the §C.7 idempotency guarantee
	// it carries is still OWED and unmet, and deleting the field would erase the
	// question along with the answer — the paragraphs above are the standing account
	// of what a constant 0 costs, and they have to sit on the declaration that causes
	// it. `reachable-ok` would be a lie: no writer exists anywhere, seen or unseen.
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

	// NotifyPreNotificationReady releases the FIRST notification for a case
	// whose inline pass has just finished (SPEC §F.3, ADR 0009).
	//
	// ⛔ ENRICHMENT IS NOT DECIDING THAT AN ALERT FIRED. `alerts` already decided
	// that and already enqueued the evaluation, scheduled at the far end of the
	// pre-notification budget as a backstop. This call only says "the budget is
	// spent, you need not wait for me" — the two evaluations carry the same
	// (case, reason, state_version) and collapse on `notifications_idem_uniq`
	// (§C.7), so at most one card is ever posted no matter which arrives first, or
	// whether this one arrives at all.
	//
	// It exists because the rule snapshot is the thing oto has that nothing else
	// does, and a differentiator that lands on a later SILENT `chat.update` is a
	// differentiator no human ever reads.
	NotifyPreNotificationReady(ctx context.Context, s db.TenantScope, n PreNotificationNotice) error
}

// PreNotificationNotice names the case whose pre-notification pass is over.
//
// ⛔ `GroupID` LED BOTH NOTICES AND IS DELETED FROM BOTH (git-bug `7570090`). It
// was the DESTINATION — which generation's thread the fact landed on — while
// `CaseID` beside it was the optional narrowing of the SUBJECT. One Case per
// conversation makes destination and subject the same id, so the two fields
// collapse into one and `CaseID` is now REQUIRED on both notices. uuid.Nil is no
// longer a "no narrowing given" signal: it means the notice has nowhere to go,
// and the pipeline declines to send it rather than enqueueing a job that would
// evaluate against the zero UUID.
type PreNotificationNotice struct {
	CaseID       uuid.UUID
	AlertID      uuid.UUID
	StateVersion int
}

// EnrichedNotice is the one coalesced fact the async phase reports.
type EnrichedNotice struct {
	CaseID       uuid.UUID
	AlertID      uuid.UUID
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
	// Type is enrichment.completed or enrichment.failed (SPEC §D.4.1).
	//
	// ⭐ IT IS THE KERNEL'S CLOSED ENUM AND NOT A `string`, WHICH IS WHY THIS
	// COMMENT NO LONGER HAS TO SAY "implementers must not invent types". They
	// cannot: `EventType`'s only field is unexported and `NewEventType` is its
	// only parser, so an invented type is a compile error here rather than a
	// runtime KindValidation error three hops away inside `alerts/service`.
	// RULE K (§5.2b) grants this package the import; the WRITE is still the port
	// above, so this remains a value crossing and not an `enrichment ──► alerts`
	// module edge.
	Type    kernel.EventType
	AlertID uuid.UUID
	CaseID  uuid.UUID
	// Summary is the pre-rendered one-liner for the timeline, 1..500 bytes.
	Summary string
	// Payload is the structured detail: per-enricher status and duration.
	Payload map[string]any
	// DedupeKey makes the append idempotent (SPEC §C.8).
	DedupeKey string
}
