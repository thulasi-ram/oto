package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// baseTime is a fixed instant. Nothing in this package reads a clock, so every
// time-dependent assertion is expressed relative to this.
var baseTime = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

// validEnrichment is the provenanced baseline every Validate case mutates one
// field of: one typed result, from one named, versioned Enricher (CONTEXT §3).
func validEnrichment() domain.Enrichment {
	return domain.Enrichment{
		ID:          "0198c0de-0000-7000-8000-000000000001",
		OrgID:       "0198c0de-0000-7000-8000-0000000000aa",
		SubjectKind: domain.SubjectOccurrence,
		SubjectID:   "0198c0de-0000-7000-8000-0000000000bb",
		Enricher:    "prom.rule",
		Version:     1,
		Phase:       domain.PhaseInline,
		Status:      domain.StatusOK,
		Duration:    120 * time.Millisecond,
		ComputedAt:  baseTime,
		ExpiresAt:   baseTime.Add(time.Hour),
	}
}

// requireValidationCode asserts err is a validation-kind errs.Error carrying the
// given stable machine code. Callers depend on the code, not the message.
func requireValidationCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, errs.KindValidation, errs.KindOf(err), "every domain rejection is a validation failure")
	assert.Equal(t, code, errs.CodeOf(err))
}

// ---------------------------------------------------------------- Phase

func TestParsePhase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		in    string
		want  domain.Phase
		isErr bool
	}{
		// The documented rule: an absent field means the pass that blocks the
		// notification, because that is what an older producer meant.
		{name: "empty means inline", in: "", want: domain.PhaseInline},
		{name: "inline", in: "inline", want: domain.PhaseInline},
		{name: "inline is case insensitive", in: "INLINE", want: domain.PhaseInline},
		{name: "inline is space tolerant", in: "  inline\t", want: domain.PhaseInline},
		{name: "numeric inline", in: "1", want: domain.PhaseInline},
		{name: "async", in: "async", want: domain.PhaseAsync},
		{name: "async is case insensitive", in: "Async", want: domain.PhaseAsync},
		{name: "numeric async", in: "2", want: domain.PhaseAsync},
		{name: "background alias", in: "background", want: domain.PhaseAsync},
		{name: "slow alias", in: "slow", want: domain.PhaseAsync},

		// "Anything else is rejected rather than guessed at."
		{name: "unknown word", in: "later", isErr: true},
		{name: "zero", in: "0", isErr: true},
		{name: "one over the enum", in: "3", isErr: true},
		{name: "negative", in: "-1", isErr: true},
		{name: "whitespace only trims to empty, which means inline", in: "   ", want: domain.PhaseInline},
		{name: "plural", in: "inlines", isErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := domain.ParsePhase(tc.in)
			if tc.isErr {
				requireValidationCode(t, err, "enrichment_unknown_phase")
				assert.Contains(t, err.Error(), tc.in, "the rejection must name what it rejected")
				assert.Equal(t, domain.Phase(0), got, "a rejected phase must not leak a usable value")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestPhaseValidAndString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		phase     domain.Phase
		wantValid bool
		wantStr   string
	}{
		{name: "inline", phase: domain.PhaseInline, wantValid: true, wantStr: domain.PhaseNameInline},
		{name: "async", phase: domain.PhaseAsync, wantValid: true, wantStr: domain.PhaseNameAsync},
		{name: "zero value is not a phase", phase: domain.Phase(0), wantValid: false, wantStr: "unknown"},
		{name: "one over", phase: domain.Phase(3), wantValid: false, wantStr: "unknown"},
		{name: "negative", phase: domain.Phase(-1), wantValid: false, wantStr: "unknown"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.wantValid, tc.phase.Valid())
			assert.Equal(t, tc.wantStr, tc.phase.String())
		})
	}
}

// TestPhaseRoundTripsThroughAJobPayload is the property that matters: String is
// documented as "the phase as it appears in a job payload", and ParsePhase
// decodes a job payload. A worker must read back what a producer wrote.
func TestPhaseRoundTripsThroughAJobPayload(t *testing.T) {
	t.Parallel()

	for _, p := range []domain.Phase{domain.PhaseInline, domain.PhaseAsync} {
		got, err := domain.ParsePhase(p.String())
		require.NoError(t, err)
		assert.Equal(t, p, got)
	}
}

// ---------------------------------------------------------------- Status

func TestStatusPredicates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		status        domain.Status
		wantValid     bool
		wantTerminal  bool
		wantSucceeded bool
		wantNeedsErr  bool
	}{
		{
			name: "ok is a complete answer", status: domain.StatusOK,
			wantValid: true, wantTerminal: true, wantSucceeded: true, wantNeedsErr: false,
		},
		{
			name: "partial still produced content", status: domain.StatusPartial,
			wantValid: true, wantTerminal: true, wantSucceeded: true, wantNeedsErr: false,
		},
		{
			name: "skipped is final but produced nothing", status: domain.StatusSkipped,
			wantValid: true, wantTerminal: true, wantSucceeded: false, wantNeedsErr: false,
		},
		{
			name: "failed is final and must say why", status: domain.StatusFailed,
			wantValid: true, wantTerminal: true, wantSucceeded: false, wantNeedsErr: true,
		},
		{
			// The one outcome that earns a second pass in the async phase.
			name: "timeout is the only non-terminal status", status: domain.StatusTimeout,
			wantValid: true, wantTerminal: false, wantSucceeded: false, wantNeedsErr: true,
		},
		{
			name: "empty is not storable", status: domain.Status(""),
			wantValid: false, wantTerminal: true, wantSucceeded: false, wantNeedsErr: false,
		},
		{
			name: "unknown is not storable", status: domain.Status("cancelled"),
			wantValid: false, wantTerminal: true, wantSucceeded: false, wantNeedsErr: false,
		},
		{
			name: "case matters", status: domain.Status("OK"),
			wantValid: false, wantTerminal: true, wantSucceeded: false, wantNeedsErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.wantValid, tc.status.Valid(), "Valid")
			assert.Equal(t, tc.wantTerminal, tc.status.Terminal(), "Terminal")
			assert.Equal(t, tc.wantSucceeded, tc.status.Succeeded(), "Succeeded")
			assert.Equal(t, tc.wantNeedsErr, tc.status.NeedsError(), "NeedsError")
		})
	}
}

// TestExactlyFiveStorableStatuses pins the closed set. This constant block, the
// DTO enum and enrichments_status_ck must stay identical (CONTEXT §5b).
func TestExactlyFiveStorableStatuses(t *testing.T) {
	t.Parallel()

	storable := []domain.Status{
		domain.StatusOK, domain.StatusPartial, domain.StatusSkipped,
		domain.StatusFailed, domain.StatusTimeout,
	}
	require.Len(t, storable, 5)

	seen := map[domain.Status]struct{}{}
	for _, s := range storable {
		assert.True(t, s.Valid(), "%q must be storable", s)
		_, dup := seen[s]
		assert.False(t, dup, "%q is declared twice", s)
		seen[s] = struct{}{}
	}
	assert.Equal(t, []domain.Status{"ok", "partial", "skipped", "failed", "timeout"}, storable,
		"the wire values are enrichments_status_ck and may not drift")
}

// ------------------------------------------------------- Enrichment.Validate

func TestEnrichmentValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mutate   func(e *domain.Enrichment)
		wantCode string // "" => must be accepted
	}{
		{name: "the provenanced baseline", mutate: func(*domain.Enrichment) {}},

		// Provenance: an enrichment that cannot say who it belongs to.
		{
			name:     "no org is not storable",
			mutate:   func(e *domain.Enrichment) { e.OrgID = "" },
			wantCode: "enrichment_no_org",
		},
		{
			name:     "no subject is not storable",
			mutate:   func(e *domain.Enrichment) { e.SubjectID = "" },
			wantCode: "enrichment_no_subject",
		},

		// subject_kind — enrichments_subjkind_ck.
		{name: "subject alert", mutate: func(e *domain.Enrichment) { e.SubjectKind = domain.SubjectAlert }},
		{name: "subject occurrence", mutate: func(e *domain.Enrichment) { e.SubjectKind = domain.SubjectOccurrence }},
		{name: "subject group", mutate: func(e *domain.Enrichment) { e.SubjectKind = domain.SubjectGroup }},
		{
			name:     "empty subject kind",
			mutate:   func(e *domain.Enrichment) { e.SubjectKind = "" },
			wantCode: "enrichment_bad_subject_kind",
		},
		{
			name:     "subject kind is case sensitive",
			mutate:   func(e *domain.Enrichment) { e.SubjectKind = "Occurrence" },
			wantCode: "enrichment_bad_subject_kind",
		},
		{
			name:     "a kind outside the closed set",
			mutate:   func(e *domain.Enrichment) { e.SubjectKind = "cluster" },
			wantCode: "enrichment_bad_subject_kind",
		},

		// enricher name — enrichments_name_ck. The dot is mandatory.
		{
			name:     "an anonymous enricher is not storable",
			mutate:   func(e *domain.Enrichment) { e.Enricher = "" },
			wantCode: "enrichment_bad_enricher_name",
		},
		{
			name:     "a bare name has no namespace",
			mutate:   func(e *domain.Enrichment) { e.Enricher = "runbook" },
			wantCode: "enrichment_bad_enricher_name",
		},

		// enricher_version — enrichments_ver_ck, and the invalidation mechanism.
		{name: "version 1 is the floor", mutate: func(e *domain.Enrichment) { e.Version = 1 }},
		{
			name:     "version zero",
			mutate:   func(e *domain.Enrichment) { e.Version = 0 },
			wantCode: "enrichment_bad_version",
		},
		{
			name:     "version negative",
			mutate:   func(e *domain.Enrichment) { e.Version = -1 },
			wantCode: "enrichment_bad_version",
		},

		// phase — enrichments_phase_ck.
		{name: "phase async", mutate: func(e *domain.Enrichment) { e.Phase = domain.PhaseAsync }},
		{
			name:     "phase zero value",
			mutate:   func(e *domain.Enrichment) { e.Phase = domain.Phase(0) },
			wantCode: "enrichment_bad_phase",
		},
		{
			name:     "phase one over",
			mutate:   func(e *domain.Enrichment) { e.Phase = domain.Phase(3) },
			wantCode: "enrichment_bad_phase",
		},

		// status — enrichments_status_ck.
		{
			name:     "empty status",
			mutate:   func(e *domain.Enrichment) { e.Status = "" },
			wantCode: "enrichment_bad_status",
		},
		{
			name:     "status outside the closed set",
			mutate:   func(e *domain.Enrichment) { e.Status = "cancelled" },
			wantCode: "enrichment_bad_status",
		},

		// enrichments_err_ck — a failure that cannot say why is a rumour.
		{
			name:     "failed with no reason",
			mutate:   func(e *domain.Enrichment) { e.Status = domain.StatusFailed },
			wantCode: "enrichment_missing_error",
		},
		{
			name: "failed with a whitespace reason is still no reason",
			mutate: func(e *domain.Enrichment) {
				e.Status = domain.StatusFailed
				e.Error = "   \t\n"
			},
			wantCode: "enrichment_missing_error",
		},
		{
			name:     "timeout with no reason",
			mutate:   func(e *domain.Enrichment) { e.Status = domain.StatusTimeout },
			wantCode: "enrichment_missing_error",
		},
		{
			name: "failed with a reason is recorded, never discarded",
			mutate: func(e *domain.Enrichment) {
				e.Status = domain.StatusFailed
				e.Error = "dial tcp: connection refused"
			},
		},
		{
			name: "timeout with a reason",
			mutate: func(e *domain.Enrichment) {
				e.Status = domain.StatusTimeout
				e.Error = "exceeded 500ms budget"
			},
		},
		{
			name: "skipped needs no reason",
			mutate: func(e *domain.Enrichment) {
				e.Status = domain.StatusSkipped
				e.Error = ""
			},
		},
		{
			name: "a succeeding enrichment may still carry an error string",
			mutate: func(e *domain.Enrichment) {
				e.Status = domain.StatusPartial
				e.Error = "one of three lookups failed"
			},
		},

		// duration_ms — enrichments_dur_ck. Recorded even on failure.
		{name: "zero duration is the floor", mutate: func(e *domain.Enrichment) { e.Duration = 0 }},
		{
			name:     "negative duration",
			mutate:   func(e *domain.Enrichment) { e.Duration = -time.Nanosecond },
			wantCode: "enrichment_negative_duration",
		},

		// expires_at — enrichments_exp_ck: NULL or strictly after computed_at.
		{
			name:   "no expiry means it never goes stale on its own",
			mutate: func(e *domain.Enrichment) { e.ExpiresAt = time.Time{} },
		},
		{
			name:   "one nanosecond of life is still life",
			mutate: func(e *domain.Enrichment) { e.ExpiresAt = e.ComputedAt.Add(time.Nanosecond) },
		},
		{
			name:     "expiry equal to computed_at is not strictly after",
			mutate:   func(e *domain.Enrichment) { e.ExpiresAt = e.ComputedAt },
			wantCode: "enrichment_bad_expiry",
		},
		{
			name:     "expiry before computed_at",
			mutate:   func(e *domain.Enrichment) { e.ExpiresAt = e.ComputedAt.Add(-time.Nanosecond) },
			wantCode: "enrichment_bad_expiry",
		},

		// Provenance is not weakened by a cache hit.
		{name: "a cached result is still a full record", mutate: func(e *domain.Enrichment) { e.FromCache = true }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e := validEnrichment()
			tc.mutate(&e)

			err := e.Validate()
			if tc.wantCode == "" {
				require.NoError(t, err)
				return
			}
			requireValidationCode(t, err, tc.wantCode)
		})
	}
}

// TestEnrichmentValidateRejectsEveryBadEnricherName drives the name rule through
// Validate as well as through the predicate, so the two cannot drift.
func TestEnrichmentValidateRejectsEveryBadEnricherName(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"", "runbook", "Prom.rule", "prom..rule", ".prom", "prom.", "1prom.rule", "prom rule"} {
		e := validEnrichment()
		e.Enricher = bad

		err := e.Validate()
		requireValidationCode(t, err, "enrichment_bad_enricher_name")
	}
}

// -------------------------------------------------------- ValidEnricherName

func TestValidEnricherName(t *testing.T) {
	t.Parallel()

	// Exactly 128 bytes: 1 + 62 + 1 (dot) + 64.
	name128 := "a" + strings.Repeat("b", 62) + "." + strings.Repeat("c", 64)
	require.Len(t, name128, 128)

	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "the four v1 enrichers: prom.rule", in: "prom.rule", want: true},
		{name: "runbook.link", in: "runbook.link", want: true},
		{name: "alert.history", in: "alert.history", want: true},
		{name: "silence.match", in: "silence.match", want: true},
		{name: "three segments", in: "prom.rule.drift", want: true},
		{name: "digits after the first letter", in: "prom2.rule9", want: true},
		{name: "single letter segments", in: "a.b", want: true},

		// THE DOT IS MANDATORY.
		{name: "a bare word has no namespace", in: "runbook", want: false},
		{name: "empty", in: "", want: false},

		// Segment shape.
		{name: "leading dot", in: ".rule", want: false},
		{name: "trailing dot", in: "prom.", want: false},
		{name: "empty middle segment", in: "prom..rule", want: false},
		{name: "uppercase first", in: "Prom.rule", want: false},
		{name: "uppercase inside", in: "prom.Rule", want: false},
		{name: "digit first", in: "1prom.rule", want: false},
		{name: "digit-first second segment", in: "prom.1rule", want: false},
		{name: "underscore", in: "prom_rule.x", want: false},
		{name: "hyphen", in: "prom-rule.x", want: false},
		{name: "space", in: "prom rule.x", want: false},
		{name: "trailing space", in: "prom.rule ", want: false},
		{name: "non-ascii", in: "prom.rulé", want: false},

		// Length bound: at the limit, one over.
		{name: "exactly 128 bytes", in: name128, want: true},
		{name: "one byte over 128", in: name128 + "c", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, domain.ValidEnricherName(tc.in))
		})
	}
}

// ------------------------------------------------- freshness and reusability

func TestEnrichmentFresh(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		expiresAt time.Time
		now       time.Time
		want      bool
	}{
		{name: "no expiry never goes stale", expiresAt: time.Time{}, now: baseTime.Add(100 * 365 * 24 * time.Hour), want: true},
		{name: "one nanosecond before expiry", expiresAt: baseTime.Add(time.Nanosecond), now: baseTime, want: true},
		{name: "exactly at expiry is stale", expiresAt: baseTime, now: baseTime, want: false},
		{name: "one nanosecond after expiry", expiresAt: baseTime, now: baseTime.Add(time.Nanosecond), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e := validEnrichment()
			e.ExpiresAt = tc.expiresAt
			assert.Equal(t, tc.want, e.Fresh(tc.now))
		})
	}
}

func TestEnrichmentReusable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(e *domain.Enrichment)
		version int
		now     time.Time
		want    bool
	}{
		{
			name:    "ok, same version, fresh — the retry pays nothing",
			mutate:  func(e *domain.Enrichment) { e.Status = domain.StatusOK },
			version: 1, now: baseTime, want: true,
		},
		{
			name:   "partial counts as an answer",
			mutate: func(e *domain.Enrichment) { e.Status = domain.StatusPartial }, version: 1, now: baseTime, want: true,
		},
		{
			// SPEC §F.3: a version bump IS the invalidation mechanism.
			name:   "a version bump invalidates",
			mutate: func(e *domain.Enrichment) { e.Version = 1 }, version: 2, now: baseTime, want: false,
		},
		{
			name:   "an older stored version is not reused either",
			mutate: func(e *domain.Enrichment) { e.Version = 3 }, version: 2, now: baseTime, want: false,
		},
		{
			name:   "skipped produced no answer",
			mutate: func(e *domain.Enrichment) { e.Status = domain.StatusSkipped }, version: 1, now: baseTime, want: false,
		},
		{
			name: "failed is recorded but never reused",
			mutate: func(e *domain.Enrichment) {
				e.Status = domain.StatusFailed
				e.Error = "boom"
			},
			version: 1, now: baseTime, want: false,
		},
		{
			name: "a timeout earns a second pass, so it is not reusable",
			mutate: func(e *domain.Enrichment) {
				e.Status = domain.StatusTimeout
				e.Error = "budget exceeded"
			},
			version: 1, now: baseTime, want: false,
		},
		{
			name:    "stale is not reusable however good the status",
			mutate:  func(e *domain.Enrichment) { e.ExpiresAt = baseTime },
			version: 1, now: baseTime.Add(time.Nanosecond), want: false,
		},
		{
			name:    "no expiry stays reusable",
			mutate:  func(e *domain.Enrichment) { e.ExpiresAt = time.Time{} },
			version: 1, now: baseTime.Add(365 * 24 * time.Hour), want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e := validEnrichment()
			tc.mutate(&e)
			assert.Equal(t, tc.want, e.Reusable(tc.version, tc.now))
		})
	}
}

// -------------------------------------------------------------- cache entry

func TestCacheEntryExpired(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		expiresAt time.Time
		now       time.Time
		want      bool
	}{
		{name: "before expiry it is served", expiresAt: baseTime.Add(time.Second), now: baseTime, want: false},
		{name: "exactly at expiry it is not served", expiresAt: baseTime, now: baseTime, want: true},
		{name: "past expiry", expiresAt: baseTime, now: baseTime.Add(time.Nanosecond), want: true},
		{name: "the zero expiry is already past", expiresAt: time.Time{}, now: baseTime, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := domain.CacheEntry{Key: "e:prom.rule:v1:org:abc", ExpiresAt: tc.expiresAt}
			assert.Equal(t, tc.want, c.Expired(tc.now))
		})
	}
}

// ---------------------------------------------------------------- CacheKey

const (
	orgA = "0198c0de-0000-7000-8000-0000000000aa"
	orgB = "0198c0de-0000-7000-8000-0000000000bb"
)

func TestCacheKeyIsDeterministic(t *testing.T) {
	t.Parallel()

	a := domain.CacheKey(orgA, "prom.rule", 1, "seed")
	b := domain.CacheKey(orgA, "prom.rule", 1, "seed")

	assert.NotEmpty(t, a)
	assert.Equal(t, a, b, "the same inputs must hit the same cache row")
}

func TestCacheKeyEmptySeedIsNotCacheable(t *testing.T) {
	t.Parallel()

	assert.Empty(t, domain.CacheKey(orgA, "prom.rule", 1, ""),
		`an enricher that supplies no seed is saying "not cacheable"`)
}

// TestCacheKeySeparatesTheThingsAnEnricherCannotBeTrustedToRemember covers the
// two documented reasons the derivation is owned here: the org (a cross-tenant
// hit would be a data leak) and the version (SPEC §F.3's invalidation lever).
func TestCacheKeySeparatesTheThingsAnEnricherCannotBeTrustedToRemember(t *testing.T) {
	t.Parallel()

	base := domain.CacheKey(orgA, "prom.rule", 1, "seed")

	tests := []struct {
		name string
		got  string
	}{
		{name: "a different org must not share a row", got: domain.CacheKey(orgB, "prom.rule", 1, "seed")},
		{name: "a version bump must miss", got: domain.CacheKey(orgA, "prom.rule", 2, "seed")},
		{name: "a different enricher must miss", got: domain.CacheKey(orgA, "alert.history", 1, "seed")},
		{name: "a different seed must miss", got: domain.CacheKey(orgA, "prom.rule", 1, "seed2")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.NotEqual(t, base, tc.got)
		})
	}
}

// TestCacheKeyVersionsDoNotCollide guards the hand-rolled integer rendering: a
// reversed or truncated one would make v12 and v21 (or v1 and v10) share a row,
// which is a version bump that silently does nothing.
func TestCacheKeyVersionsDoNotCollide(t *testing.T) {
	t.Parallel()

	seen := map[string]int{}
	for _, v := range []int{1, 2, 9, 10, 11, 12, 21, 99, 100, 101, 110} {
		key := domain.CacheKey(orgA, "prom.rule", v, "seed")
		if prev, dup := seen[key]; dup {
			t.Fatalf("version %d collides with version %d on key %q", v, prev, key)
		}
		seen[key] = v
	}
}

// TestCacheKeyHashesTheSeed proves the documented reason the seed is hashed: it
// may be arbitrarily long and may contain label values without risking the
// 512-byte enrichment_cache_key_ck bound, and without echoing them into a key.
func TestCacheKeyHashesTheSeed(t *testing.T) {
	t.Parallel()

	secretish := strings.Repeat("customer-acme-prod-", 5000) // ~95 KB
	key := domain.CacheKey(orgA, "prom.rule", 1, secretish)

	require.NotEmpty(t, key)
	assert.NotContains(t, key, "customer-acme-prod-", "the seed must not survive verbatim into the key")
	assert.LessOrEqual(t, len(key), domain.MaxCacheKeyBytes)
}

// TestCacheKeyStaysWithinTheColumnBoundAtEveryLegalExtreme uses the longest
// storable enricher name and a long org id: the key must still fit
// enrichment_cache_key_ck (1..512).
func TestCacheKeyStaysWithinTheColumnBoundAtEveryLegalExtreme(t *testing.T) {
	t.Parallel()

	longestName := "a" + strings.Repeat("b", 62) + "." + strings.Repeat("c", 64)
	require.True(t, domain.ValidEnricherName(longestName))

	key := domain.CacheKey(orgA, longestName, 999999, strings.Repeat("x", 1<<20))

	assert.GreaterOrEqual(t, len(key), 1)
	assert.LessOrEqual(t, len(key), domain.MaxCacheKeyBytes)
}

// ---------------------------------------------------------------- ClampTTL

func TestClampTTL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{name: "zero means no caching at all", in: 0, want: 0},
		{name: "negative means no caching at all", in: -time.Hour, want: 0},

		// A one-second TTL is a cache that only costs.
		{name: "one nanosecond clamps up to the floor", in: time.Nanosecond, want: domain.MinCacheTTL},
		{name: "one nanosecond under the floor", in: domain.MinCacheTTL - time.Nanosecond, want: domain.MinCacheTTL},
		{name: "exactly the floor", in: domain.MinCacheTTL, want: domain.MinCacheTTL},
		{name: "one nanosecond over the floor is kept", in: domain.MinCacheTTL + time.Nanosecond, want: domain.MinCacheTTL + time.Nanosecond},

		{name: "in range", in: time.Hour, want: time.Hour},

		// A one-week TTL is a stale answer presented as a fresh fact.
		{name: "one nanosecond under the ceiling is kept", in: domain.MaxCacheTTL - time.Nanosecond, want: domain.MaxCacheTTL - time.Nanosecond},
		{name: "exactly the ceiling", in: domain.MaxCacheTTL, want: domain.MaxCacheTTL},
		{name: "one nanosecond over the ceiling clamps down", in: domain.MaxCacheTTL + time.Nanosecond, want: domain.MaxCacheTTL},
		{name: "a week clamps down to a day", in: 7 * 24 * time.Hour, want: domain.MaxCacheTTL},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, domain.ClampTTL(tc.in))
		})
	}
}

// TestClampTTLIsIdempotent — clamping an already-clamped value must not move it,
// or a re-run of a phase would keep shrinking its own cache lifetime.
func TestClampTTLIsIdempotent(t *testing.T) {
	t.Parallel()

	for _, d := range []time.Duration{
		-time.Hour, 0, time.Nanosecond, time.Second, domain.MinCacheTTL, time.Hour,
		domain.MaxCacheTTL, 7 * 24 * time.Hour,
	} {
		once := domain.ClampTTL(d)
		assert.Equal(t, once, domain.ClampTTL(once), "ClampTTL(%v)", d)
	}
}

// ----------------------------------------------------------------- budgets

// TestBudgetsBound checks the only budget property this tier can see: the
// declared ceilings are ordered so that they actually bound something. A default
// enricher timeout at or above the inline ceiling would make the ceiling a
// promise the pipeline could never keep, and an async budget below the inline
// one would make the "generous, because nothing is waiting on it" comment false.
//
// The enforcement itself (concurrent run, stragglers recorded StatusTimeout, the
// notification proceeding anyway) lives in internal/enrichment/service and is
// not reachable from here.
func TestBudgetsBound(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 2000*time.Millisecond, domain.InlineBudget, "SPEC §F.3 pins the inline ceiling at 2 000 ms")
	assert.Equal(t, 30*time.Second, domain.AsyncBudget)
	assert.Equal(t, 500*time.Millisecond, domain.DefaultEnricherTimeout)

	assert.Less(t, domain.DefaultEnricherTimeout, domain.InlineBudget,
		"a single enricher's default must fit inside the ceiling it shares")
	assert.Less(t, domain.InlineBudget, domain.AsyncBudget,
		"the async pass is the generous one; nothing is waiting on it")
	assert.Greater(t, domain.AsyncBudget, time.Duration(0),
		"a wedged enricher must not hold a worker forever")

	assert.Less(t, domain.MinCacheTTL, domain.MaxCacheTTL)
	assert.Equal(t, 10*time.Second, domain.MinCacheTTL)
	assert.Equal(t, 24*time.Hour, domain.MaxCacheTTL)

	assert.Equal(t, 512, domain.MaxCacheKeyBytes, "mirrors enrichment_cache_key_ck")
	assert.Equal(t, 32, domain.MaxWarnings, "the warnings column is not a log")
}

// ---------------------------------------------------------------- known bug

// TestBUG_EnrichmentIsConstructibleWhileInvalid demonstrates that
// internal/enrichment/domain has no `New…() (T, error)` constructor at all: the
// only invariant enforcement is an optional Enrichment.Validate() that any
// caller may forget to call, and CacheEntry has no enforcement whatsoever.
//
// CONTEXT.md §5b: "No optional Validate() in domain. If you can construct it, it
// is valid." CONTEXT.md §5 layer table: "Domain invariants | value objects +
// New…() (T, error)".
//
// The zero value below is a fully constructible Enrichment that violates six
// DDL CHECKs at once, and nothing in the type system stops it reaching a
// repository. This is the invariant hole, not a behaviour defect in Validate.
func TestBUG_EnrichmentIsConstructibleWhileInvalid(t *testing.T) {
	t.Skip("BUG: internal/enrichment/domain has no New* constructor; Enrichment relies on an optional Validate(), which CONTEXT.md §5b forbids (enrichment.go:159). CacheEntry has no invariant enforcement at all — enrichment_cache_key_ck and enrichment_cache_exp_ck are re-checked in internal/enrichment/repository/cache.go instead of in the domain.")

	var e domain.Enrichment
	require.Error(t, e.Validate(), "the zero value is invalid, yet it constructed")

	var c domain.CacheEntry
	assert.Empty(t, c.Key, "an empty key violates enrichment_cache_key_ck and the domain cannot say so")
}
