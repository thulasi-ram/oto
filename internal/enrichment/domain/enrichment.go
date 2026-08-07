package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// Phase names as they travel in a job payload (SPEC §G.3:
// `enrich.run {occurrence_id, phase, enrichers[]}`).
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
type Enrichment struct {
	ID          string
	OrgID       string
	SubjectKind string
	SubjectID   string

	Enricher string
	Version  int
	Phase    Phase
	Status   Status

	// Payload is the enricher's typed output, marshalled to JSONB. A nil
	// payload is stored as `{}` so enrichments_payload_ck always holds.
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
	// ExpiresAt is when the result should be considered stale. Zero means it
	// never goes stale on its own.
	ExpiresAt time.Time
}

// Fresh reports whether the enrichment is still within its own expiry at now.
func (e Enrichment) Fresh(now time.Time) bool {
	return e.ExpiresAt.IsZero() || e.ExpiresAt.After(now)
}

// Reusable reports whether a stored result lets the pipeline skip an enricher
// on a re-run: same version, a status that produced an answer, and not stale.
//
// Re-running a phase must be safe AND cheap. Idempotency comes from the unique
// constraint on (subject_kind, subject_id, enricher); this is what stops a
// retry paying for the same Prometheus call twice.
func (e Enrichment) Reusable(version int, now time.Time) bool {
	return e.Version == version && e.Status.Succeeded() && e.Fresh(now)
}

// Validate re-checks the invariants `enrichments` enforces in the DDL.
func (e Enrichment) Validate() error {
	switch {
	case e.OrgID == "":
		return errs.New(errs.KindValidation, "enrichment_no_org", "an enrichment must be org-scoped")
	case e.SubjectID == "":
		return errs.New(errs.KindValidation, "enrichment_no_subject", "an enrichment must name its subject")
	case !validSubjectKind(e.SubjectKind):
		return errs.New(errs.KindValidation, "enrichment_bad_subject_kind",
			"subject_kind must be alert, occurrence or group")
	case !ValidEnricherName(e.Enricher):
		return errs.Newf(errs.KindValidation, "enrichment_bad_enricher_name",
			"enricher %q must match ^[a-z][a-z0-9]*(\\.[a-z][a-z0-9]*)+$", e.Enricher)
	case e.Version < 1:
		return errs.New(errs.KindValidation, "enrichment_bad_version", "enricher_version must be >= 1")
	case !e.Phase.Valid():
		return errs.New(errs.KindValidation, "enrichment_bad_phase", "phase must be 1 (inline) or 2 (async)")
	case !e.Status.Valid():
		return errs.New(errs.KindValidation, "enrichment_bad_status",
			"status must be ok, partial, skipped, failed or timeout")
	case e.Status.NeedsError() && strings.TrimSpace(e.Error) == "":
		return errs.New(errs.KindValidation, "enrichment_missing_error",
			"a failed or timed-out enrichment must record why (enrichments_err_ck)")
	case e.Duration < 0:
		return errs.New(errs.KindValidation, "enrichment_negative_duration", "duration_ms must be >= 0")
	case !e.ExpiresAt.IsZero() && !e.ExpiresAt.After(e.ComputedAt):
		return errs.New(errs.KindValidation, "enrichment_bad_expiry",
			"expires_at must be strictly after computed_at (enrichments_exp_ck)")
	}
	return nil
}

// The subject kinds an Enrichment may be about.
const (
	// SubjectAlert is the Alert identity.
	SubjectAlert = "alert"
	// SubjectOccurrence is one firing episode. This is the v1 default: an
	// enrichment is a fact about a FIRE, not about an identity.
	SubjectOccurrence = "occurrence"
	// SubjectGroup is a notification group generation.
	SubjectGroup = "group"
)

func validSubjectKind(k string) bool {
	switch k {
	case SubjectAlert, SubjectOccurrence, SubjectGroup:
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
type CacheEntry struct {
	Key        string
	OrgID      string
	Payload    []byte
	ComputedAt time.Time
	ExpiresAt  time.Time
}

// Expired reports whether the entry is past its hard expiry. Rows past it are
// deleted, never served.
func (c CacheEntry) Expired(now time.Time) bool { return !c.ExpiresAt.After(now) }

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
