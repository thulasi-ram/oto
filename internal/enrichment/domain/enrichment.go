package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// Phase names as they travel in a job payload (SPEC §G.3:
// `enrich.run {case_id, phase, enrichers[]}`).
const (
	// PhaseNameInline is the pre-notification phase.
	PhaseNameInline = "inline"
	// PhaseNameAsync is the post-notification phase.
	PhaseNameAsync = "async"
)

// Valid reports whether p is one of the two phases.
func (p Phase) Valid() bool { return p == PhaseInline || p == PhaseAsync }

// String renders the phase as it appears in a job payload.
func (p Phase) String() string {
	switch p {
	case PhaseInline:
		return PhaseNameInline
	case PhaseAsync:
		return PhaseNameAsync
	default:
		return "unknown"
	}
}

// ParsePhase decodes a phase from a job payload.
//
// An empty string means inline: the ingest path enqueues the first pass and an
// older producer that omitted the field meant the pass that blocks the
// notification. Anything else is rejected rather than guessed at.
func ParsePhase(s string) (Phase, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", PhaseNameInline, "1":
		return PhaseInline, nil
	case PhaseNameAsync, "2", "background", "slow":
		return PhaseAsync, nil
	default:
		return 0, errs.Newf(errs.KindValidation, "enrichment_unknown_phase",
			"unknown enrichment phase %q: expected inline or async", s)
	}
}

// Terminal reports whether a status is a final answer for this run. Every
// status except StatusTimeout is: a timeout is the one outcome that earns the
// subject a second pass in the async phase.
func (s Status) Terminal() bool { return s != StatusTimeout }

// Succeeded reports whether the enricher produced usable content.
func (s Status) Succeeded() bool { return s == StatusOK || s == StatusPartial }

// NeedsError reports whether enrichments_err_ck requires an error string for
// this status. A failure that cannot say why is a rumour.
func (s Status) NeedsError() bool { return s == StatusFailed || s == StatusTimeout }

// Valid reports whether s is one of the five storable statuses.
func (s Status) Valid() bool {
	switch s {
	case StatusOK, StatusPartial, StatusSkipped, StatusFailed, StatusTimeout:
		return true
	default:
		return false
	}
}

// Budget constants for the pipeline (SPEC §F.3).
const (
	// InlineBudget is the CEILING on all inline enrichers together, run
	// concurrently. It is never a wait: when it expires, stragglers are
	// recorded StatusTimeout and the notification proceeds anyway.
	InlineBudget = 2000 * time.Millisecond

	// AsyncBudget bounds the post-notification pass. It is generous because
	// nothing is waiting on it, and bounded because a wedged enricher must not
	// hold a worker forever.
	AsyncBudget = 30 * time.Second

	// DefaultEnricherTimeout applies to an enricher that declares none.
	DefaultEnricherTimeout = 500 * time.Millisecond

	// MinCacheTTL and MaxCacheTTL bound what an enricher may ask for. A
	// one-second TTL is a cache that only costs, and a one-week TTL is a stale
	// answer presented as a fresh fact.
	MinCacheTTL = 10 * time.Second
	MaxCacheTTL = 24 * time.Hour
)

// MaxCacheKeyBytes mirrors enrichment_cache_key_ck.
const MaxCacheKeyBytes = 512

// MaxWarnings caps the warnings recorded per result. An enricher emitting more
// than this is reporting noise, and the column is not a log.
const MaxWarnings = 32

// Enrichment is ONE typed, provenanced result from ONE named, versioned
// Enricher — a row of `enrichments`.
//
// Provenance is the point (SPEC §D.7): a result that cannot say who computed
// it, at which version, from cache or not, and whether it succeeded, is a
// rumour. A failed or timed-out enrichment is RECORDED, never discarded: a
// missing enrichment and a failed one must be distinguishable in the UI.
//
// Every field is unexported and reachable only through NewEnrichment, so a
// result the table would refuse — a failure that cannot say why, an enricher
// with no namespace, an expiry that precedes its own computation — cannot be
// built at all, and the CHECK constraints stop being the thing that reports the
// bug. A record assembled in pieces is exactly how a record becomes invalid,
// which is why there is one door and it takes every field at once.
type Enrichment struct {
	id          string
	orgID       string
	subjectKind string
	subjectID   string

	enricher string
	version  int
	phase    Phase
	status   Status

	payload  any
	warnings []string
	errText  string

	duration   time.Duration
	fromCache  bool
	computedAt time.Time
	expiresAt  time.Time
}

// EnrichmentParams is the full constructor input, and also the rehydration
// shape: the repository maps a row into it and the constructor re-proves every
// invariant.
type EnrichmentParams struct {
	ID          string
	OrgID       string
	SubjectKind string
	SubjectID   string

	Enricher string
	Version  int
	Phase    Phase
	Status   Status

	// Payload is the enricher's typed output, marshalled to JSONB. A nil
	// payload becomes `{}` so enrichments_payload_ck always holds.
	Payload any
	// Warnings are non-fatal notes surfaced alongside the result.
	Warnings []string
	// Error is REQUIRED when Status is failed or timeout (enrichments_err_ck).
	Error string

	// Duration is wall time spent, recorded even on failure because it is what
	// feeds the budget.
	Duration time.Duration
	// FromCache reports that this result was served from enrichment_cache
	// rather than recomputed. It is part of the provenance, not an optimisation
	// detail.
	FromCache  bool
	ComputedAt time.Time
	// ExpiresAt is when the result should be considered stale. The zero time
	// means it never goes stale on its own — unlike the cache's expiry, this
	// column is NULLABLE (enrichments_exp_ck), because a recorded failure has
	// nothing to go stale.
	ExpiresAt time.Time
}

// NewEnrichment builds a provenanced result, enforcing every constraint
// `enrichments` carries.
func NewEnrichment(p EnrichmentParams) (Enrichment, error) {
	switch {
	case p.OrgID == "":
		return Enrichment{}, errs.New(errs.KindValidation, "enrichment_no_org",
			"an enrichment must be org-scoped")
	case p.SubjectID == "":
		return Enrichment{}, errs.New(errs.KindValidation, "enrichment_no_subject",
			"an enrichment must name its subject")
	case !validSubjectKind(p.SubjectKind):
		return Enrichment{}, errs.New(errs.KindValidation, "enrichment_bad_subject_kind",
			"subject_kind must be alert, case or group")
	case !ValidEnricherName(p.Enricher):
		return Enrichment{}, errs.Newf(errs.KindValidation, "enrichment_bad_enricher_name",
			"enricher %q must match ^[a-z][a-z0-9]*(\\.[a-z][a-z0-9]*)+$", p.Enricher)
	case p.Version < 1:
		return Enrichment{}, errs.New(errs.KindValidation, "enrichment_bad_version",
			"enricher_version must be >= 1")
	case !p.Phase.Valid():
		return Enrichment{}, errs.New(errs.KindValidation, "enrichment_bad_phase",
			"phase must be 1 (inline) or 2 (async)")
	case !p.Status.Valid():
		return Enrichment{}, errs.New(errs.KindValidation, "enrichment_bad_status",
			"status must be ok, partial, skipped, failed or timeout")
	case p.Status.NeedsError() && strings.TrimSpace(p.Error) == "":
		return Enrichment{}, errs.New(errs.KindValidation, "enrichment_missing_error",
			"a failed or timed-out enrichment must record why (enrichments_err_ck)")
	case p.Duration < 0:
		return Enrichment{}, errs.New(errs.KindValidation, "enrichment_negative_duration",
			"duration_ms must be >= 0")
	case !p.ExpiresAt.IsZero() && !p.ExpiresAt.After(p.ComputedAt):
		return Enrichment{}, errs.New(errs.KindValidation, "enrichment_bad_expiry",
			"expires_at must be strictly after computed_at (enrichments_exp_ck)")
	}

	// A nil payload becomes `{}` here rather than at the write, because
	// enrichments_payload_ck requires a JSON OBJECT and `null` is not one. The
	// repository still has to wrap a payload that marshals to a non-object — this
	// layer cannot know what an arbitrary `any` renders as — but "the enricher
	// produced nothing" is answerable here, on the one path every result passes
	// through.
	payload := p.Payload
	if payload == nil {
		payload = map[string]any{}
	}

	return Enrichment{
		id:          p.ID,
		orgID:       p.OrgID,
		subjectKind: p.SubjectKind,
		subjectID:   p.SubjectID,
		enricher:    p.Enricher,
		version:     p.Version,
		phase:       p.Phase,
		status:      p.Status,
		payload:     payload,
		warnings:    append([]string(nil), p.Warnings...),
		errText:     p.Error,
		duration:    p.Duration,
		fromCache:   p.FromCache,
		computedAt:  p.ComputedAt.UTC(),
		expiresAt:   p.ExpiresAt.UTC(),
	}, nil
}

// ID is the row's uuidv7, rendered as a string.
func (e Enrichment) ID() string { return e.id }

// OrgID is the tenant this result belongs to.
func (e Enrichment) OrgID() string { return e.orgID }

// SubjectKind is what was enriched: alert, case or group.
func (e Enrichment) SubjectKind() string { return e.subjectKind }

// SubjectID is the subject row id. It is polymorphic, so SubjectKind names the
// table it points into.
func (e Enrichment) SubjectID() string { return e.subjectID }

// Enricher is the dotted registry name of the Enricher that produced this.
func (e Enrichment) Enricher() string { return e.enricher }

// Version is the Enricher version that produced this. Bumping it is how a
// result is invalidated (SPEC §F.3).
func (e Enrichment) Version() int { return e.version }

// Phase is the budgeted pass this result came out of.
func (e Enrichment) Phase() Phase { return e.phase }

// Status is one of the five storable outcomes.
func (e Enrichment) Status() Status { return e.status }

// Payload is the enricher's typed output. It is handed back as it was given —
// a map, a struct or raw JSON — because decoding it into the enricher's own
// shape is the reader's job, and this package must not know every enricher's
// output or adding an enricher would mean editing SQL.
func (e Enrichment) Payload() any { return e.payload }

// Warnings are the non-fatal notes surfaced alongside the result.
//
// Cloned on ingress as well as egress, for the same reason CacheEntry clones its
// payload: a result a caller can edit after construction is no longer one the
// constructor vouched for.
func (e Enrichment) Warnings() []string { return append([]string(nil), e.warnings...) }

// ErrorText is why the enricher failed, REQUIRED when Status is failed or
// timeout (enrichments_err_ck).
//
// It is not called Error, deliberately: a method named `Error() string` would
// make every Enrichment satisfy the `error` interface, and a provenanced result
// that can be returned where an error is expected is a mistake the compiler
// would stop reporting.
func (e Enrichment) ErrorText() string { return e.errText }

// Duration is the wall time spent. It is recorded even on failure, because it
// is what feeds the budget.
func (e Enrichment) Duration() time.Duration { return e.duration }

// FromCache reports that this result was served from enrichment_cache rather
// than recomputed. It is part of the provenance, not an optimisation detail.
func (e Enrichment) FromCache() bool { return e.fromCache }

// ComputedAt is when the result was produced.
func (e Enrichment) ComputedAt() time.Time { return e.computedAt }

// ExpiresAt is when the result should be considered stale, or the zero time
// when it never goes stale on its own.
func (e Enrichment) ExpiresAt() time.Time { return e.expiresAt }

// Fresh reports whether the enrichment is still within its own expiry at now.
func (e Enrichment) Fresh(now time.Time) bool {
	return e.expiresAt.IsZero() || e.expiresAt.After(now)
}

// Reusable reports whether a stored result lets the pipeline skip an enricher
// on a re-run: same version, a status that produced an answer, and not stale.
//
// Re-running a phase must be safe AND cheap. Idempotency comes from the unique
// constraint on (subject_kind, subject_id, enricher); this is what stops a
// retry paying for the same Prometheus call twice.
func (e Enrichment) Reusable(version int, now time.Time) bool {
	return e.version == version && e.status.Succeeded() && e.Fresh(now)
}

// The subject kinds an Enrichment may be about.
const (
	// SubjectAlert is the Alert identity.
	SubjectAlert = "alert"
	// SubjectCase is one firing episode. This is the v1 default: an
	// enrichment is a fact about a FIRE, not about an identity.
	SubjectCase = "case"
	// SubjectGroup is a notification group generation.
	SubjectGroup = "group"
)

func validSubjectKind(k string) bool {
	switch k {
	case SubjectAlert, SubjectCase, SubjectGroup:
		return true
	default:
		return false
	}
}

// ValidEnricherName reports whether name satisfies enrichments_name_ck:
// `^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)+$`.
//
// The DOT IS MANDATORY — a bare "runbook" is not storable. The name is a
// namespaced registry id, and the constraint is what stops one from drifting
// into a free-text label.
func ValidEnricherName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	parts := strings.Split(name, ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if p == "" || p[0] < 'a' || p[0] > 'z' {
			return false
		}
		for i := 1; i < len(p); i++ {
			c := p[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
				return false
			}
		}
	}
	return true
}

// CacheEntry is one row of `enrichment_cache`: a disposable, expiring result
// shared across subjects.
//
// It is deliberately NOT the same thing as an Enrichment. This table is a
// cache and may be truncated at any moment with no loss of meaning;
// `enrichments` is the provenanced record and may not.
//
// Every field is unexported and reachable only through NewCacheEntry, so an
// entry the table would refuse — a key the column cannot hold, an expiry that
// does not follow its own computation — cannot be built at all, and the CHECK
// constraints stop being the thing that reports the bug.
type CacheEntry struct {
	key        string
	orgID      string
	payload    []byte
	computedAt time.Time
	expiresAt  time.Time
}

// CacheEntryParams is the constructor input, and also the rehydration shape: the
// repository maps a row into it and the constructor re-proves every invariant.
type CacheEntryParams struct {
	Key   string
	OrgID string
	// Payload is the cached body as raw JSON. Empty means `{}`.
	Payload    []byte
	ComputedAt time.Time
	ExpiresAt  time.Time
}

// NewCacheEntry builds a cache entry, enforcing every constraint
// `enrichment_cache` carries.
func NewCacheEntry(p CacheEntryParams) (CacheEntry, error) {
	switch {
	case p.Key == "" || len(p.Key) > MaxCacheKeyBytes:
		return CacheEntry{}, errs.Newf(errs.KindValidation, "enrichment_bad_cache_key",
			"a cache key must be 1..%d bytes (enrichment_cache_key_ck)", MaxCacheKeyBytes)
	case p.OrgID == "":
		return CacheEntry{}, errs.New(errs.KindValidation, "enrichment_no_cache_org",
			"a cache entry must be org-scoped: one global primary key means an unscoped entry is a cross-tenant read waiting to happen")
	case p.ComputedAt.IsZero() || p.ExpiresAt.IsZero():
		return CacheEntry{}, errs.New(errs.KindValidation, "enrichment_bad_cache_expiry",
			"computed_at and expires_at are required")
	case !p.ExpiresAt.After(p.ComputedAt):
		// enrichment_cache_exp_ck. An entry that expires before it was computed is
		// a clock bug. Note this expiry is NOT nullable, unlike enrichments_exp_ck:
		// a cache row with no expiry would never be swept, and a cache that cannot
		// forget is a stale answer presented as a fresh fact.
		return CacheEntry{}, errs.New(errs.KindValidation, "enrichment_bad_cache_expiry",
			"expires_at must be strictly after computed_at (enrichment_cache_exp_ck)")
	}

	// An empty payload becomes `{}` here rather than being refused. The column is
	// JSONB NOT NULL with NO jsonb_typeof check — `enrichment_cache` has no
	// counterpart to enrichments_payload_ck — so "the enricher answered, and the
	// answer was nothing" is a legitimate, storable entry worth caching: it is
	// exactly the recomputation an empty answer would otherwise repeat every time.
	// Empty BYTES, though, are not JSON at all and would fail the ::jsonb cast at
	// the driver, so the default belongs on the one path every entry passes
	// through, not in one repository method.
	payload := p.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}

	return CacheEntry{
		key:        p.Key,
		orgID:      p.OrgID,
		payload:    append([]byte(nil), payload...),
		computedAt: p.ComputedAt.UTC(),
		expiresAt:  p.ExpiresAt.UTC(),
	}, nil
}

// Key is the globally unique cache key, 1..512 bytes. Derive it with CacheKey.
func (c CacheEntry) Key() string { return c.key }

// OrgID is the tenant that populated the entry.
func (c CacheEntry) OrgID() string { return c.orgID }

// Payload is the cached body as raw JSON: decoding it is the reader's job.
//
// Cloned on ingress as well as egress, for the same reason Silence clones its
// matchers: handing back the internal slice lets a caller edit a stored entry in
// place, and an entry that can be edited after construction is no longer one the
// constructor vouched for.
func (c CacheEntry) Payload() []byte { return append([]byte(nil), c.payload...) }

// ComputedAt is when the cached answer was produced.
func (c CacheEntry) ComputedAt() time.Time { return c.computedAt }

// ExpiresAt is the hard expiry, strictly after ComputedAt.
func (c CacheEntry) ExpiresAt() time.Time { return c.expiresAt }

// Expired reports whether the entry is past its hard expiry. Rows past it are
// deleted, never served.
func (c CacheEntry) Expired(now time.Time) bool { return !c.expiresAt.After(now) }

// CacheKey derives the storable cache key for one enricher's seed.
//
// The derivation is owned HERE rather than by each enricher, because two of the
// three things that must be in every key are things an enricher should not have
// to remember:
//
//   - the org, because `enrichment_cache` has one global primary key and a
//     cross-tenant cache hit would be a data leak, not a performance win;
//   - the enricher VERSION, because SPEC §F.3 defines a version bump as the
//     invalidation mechanism, and a cache that survived one would make bumping
//     a version do nothing.
//
// The enricher supplies only `seed`: a stable rendering of ITS OWN inputs. The
// seed is hashed, so it may be arbitrarily long and may contain label values
// without risking the 512-byte column bound.
func CacheKey(orgID, enricher string, version int, seed string) string {
	if seed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(seed))
	return "e:" + enricher + ":v" + itoa(version) + ":" + orgID + ":" + hex.EncodeToString(sum[:])
}

// ClampTTL bounds a requested TTL into the storable range, returning zero when
// the enricher asked for no caching at all.
func ClampTTL(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return 0
	case d < MinCacheTTL:
		return MinCacheTTL
	case d > MaxCacheTTL:
		return MaxCacheTTL
	default:
		return d
	}
}

// itoa renders a small non-negative int without importing strconv into a hot
// helper. Versions are single- or double-digit by construction.
func itoa(n int) string {
	if n <= 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
