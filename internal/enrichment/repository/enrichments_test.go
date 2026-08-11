package repository_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/enrichment/repository"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

// `enrichments` is the PROVENANCED RECORD, and it is the counterweight to the
// cache next door: truncating the cache costs latency, truncating this destroys
// the record. A failed or timed-out enrichment is stored here, never discarded,
// because a missing enrichment and a failed one must be distinguishable in the
// UI.
//
// These tests also settle a question the conformance review raised about
// domain.Enrichment: it has exported fields, no constructor and an OPTIONAL
// Validate(). UpsertMany is the one place that closes that hole, and
// TestUpsertManyRefusesWhatTheDDLWouldRefuse is the test that pins it there.

func result(orgID, subjectID string) domain.Enrichment {
	return domain.Enrichment{
		ID:          id.NewString(),
		OrgID:       orgID,
		SubjectKind: domain.SubjectOccurrence,
		SubjectID:   subjectID,
		Enricher:    "prom.rule",
		Version:     1,
		Phase:       domain.PhaseInline,
		Status:      domain.StatusOK,
		Payload:     map[string]any{"expr": "up == 0", "available": true},
		Duration:    123 * time.Millisecond,
		ComputedAt:  harness.Epoch,
		ExpiresAt:   harness.Epoch.Add(5 * time.Minute),
	}
}

// ------------------------------------------------------------- round trip

func TestAProvenancedResultRoundTrips(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)
	subjectID := id.NewString()

	in := result(org.ID.String(), subjectID)
	in.Warnings = []string{"ambiguous_rule_match"}
	in.FromCache = true
	require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{in}))

	got, err := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectOccurrence, subjectID)
	require.NoError(t, err)
	require.Len(t, got, 1)

	out := got[0]
	assert.Equal(t, in.ID, out.ID)
	assert.Equal(t, org.ID.String(), out.OrgID)
	assert.Equal(t, "prom.rule", out.Enricher)
	assert.Equal(t, 1, out.Version)
	assert.Equal(t, domain.PhaseInline, out.Phase)
	assert.Equal(t, domain.StatusOK, out.Status)
	assert.Equal(t, []string{"ambiguous_rule_match"}, out.Warnings)
	assert.True(t, out.FromCache, "provenance: a reused answer must be distinguishable from a fresh one")
	assert.Equal(t, 123*time.Millisecond, out.Duration,
		"wall time is recorded even on success because it is what feeds the budget")
	assert.True(t, out.ComputedAt.Equal(harness.Epoch))
	assert.True(t, out.ExpiresAt.Equal(harness.Epoch.Add(5*time.Minute)))
	assert.Equal(t, time.UTC, out.ComputedAt.Location())

	raw, ok := out.Payload.(json.RawMessage)
	require.True(t, ok, "the payload comes back as raw JSON: this layer must not know every enricher's shape")
	assert.JSONEq(t, `{"expr":"up == 0","available":true}`, string(raw))

	assert.NoError(t, out.Validate(), "what comes back out is storable again")
}

func TestAFailedResultIsStoredWithTheReasonAttached(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)
	subjectID := id.NewString()

	failed := result(org.ID.String(), subjectID)
	failed.Enricher = "alert.history"
	failed.Status = domain.StatusFailed
	failed.Error = "the pool is exhausted"
	failed.Payload = map[string]any{}
	failed.ExpiresAt = time.Time{}

	timedOut := result(org.ID.String(), subjectID)
	timedOut.Enricher = "alert.related"
	timedOut.Status = domain.StatusTimeout
	timedOut.Error = "exceeded its 800ms budget"
	timedOut.Payload = map[string]any{}
	timedOut.ExpiresAt = time.Time{}

	require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{failed, timedOut}))

	got, err := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectOccurrence, subjectID)
	require.NoError(t, err)
	require.Len(t, got, 2)

	assert.Equal(t, "alert.history", got[0].Enricher, "ordered by enricher, so two reads render the same")
	assert.Equal(t, "alert.related", got[1].Enricher)

	for _, e := range got {
		assert.NotEmpty(t, e.Error, "enrichments_err_ck: a failure that cannot say why is a rumour")
		assert.Zero(t, e.ExpiresAt, "a failure does not go stale; it is simply never reused")
		assert.False(t, e.Status.Succeeded())
	}
}

// TestReRunningAPhaseOverwritesItsOwnRows is what makes `enrich.run` idempotent
// on (occurrence_id, phase): the conflict target omits the version, so a version
// bump REPLACES the old answer rather than accumulating beside it.
func TestReRunningAPhaseOverwritesItsOwnRows(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)
	subjectID := id.NewString()

	first := result(org.ID.String(), subjectID)
	first.Status = domain.StatusFailed
	first.Error = "prometheus was down"
	first.Payload = map[string]any{}
	first.ExpiresAt = time.Time{}
	require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{first}))

	// The retry succeeds, at a bumped version.
	second := result(org.ID.String(), subjectID)
	second.Version = 2
	require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{second}))

	got, err := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectOccurrence, subjectID)
	require.NoError(t, err)
	require.Len(t, got, 1, "a retry converges; it never double-counts a failure")
	assert.Equal(t, domain.StatusOK, got[0].Status)
	assert.Equal(t, 2, got[0].Version)
	assert.Empty(t, got[0].Error, "the old failure's reason does not survive the success")
}

func TestAnEmptyBatchIsNotARoundTrip(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)

	require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope, nil))
	require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{}))
}

func TestAWholePhaseIsWrittenInOneBatch(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)
	subjectID := id.NewString()

	batch := make([]domain.Enrichment, 0, 4)
	for _, name := range []string{"prom.rule", "alert.history", "runbook.link", "alert.related"} {
		e := result(org.ID.String(), subjectID)
		e.Enricher = name
		batch = append(batch, e)
	}
	require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope, batch))

	got, err := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectOccurrence, subjectID)
	require.NoError(t, err)
	assert.Len(t, got, 4, "the inline phase has 2 000 ms for everything including its own writes")
}

// -------------------------------------------------------- payload encoding

func TestANonObjectPayloadIsWrappedRatherThanDropped(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)

	for name, payload := range map[string]any{
		"a bare number": 42,
		"a bare string": "up == 0",
		"an array":      []int{1, 2, 3},
	} {
		subjectID := id.NewString()
		e := result(org.ID.String(), subjectID)
		e.Payload = payload
		require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{e}), name)

		got, err := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectOccurrence, subjectID)
		require.NoError(t, err)
		require.Len(t, got, 1)
		raw, ok := got[0].Payload.(json.RawMessage)
		require.True(t, ok)
		assert.Contains(t, string(raw), `"value"`,
			"enrichments_payload_ck requires an object; the result is still provenance and is wrapped, not lost")
	}
}

func TestANilPayloadBecomesAnEmptyObject(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)
	subjectID := id.NewString()

	e := result(org.ID.String(), subjectID)
	e.Payload = nil
	require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{e}))

	got, err := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectOccurrence, subjectID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.JSONEq(t, `{}`, string(got[0].Payload.(json.RawMessage)),
		"a null would be rejected by the CHECK, and the place to notice that is here")
}

func TestNilWarningsAreStoredAsAnEmptyArray(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)
	subjectID := id.NewString()

	require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope,
		[]domain.Enrichment{result(org.ID.String(), subjectID)}))

	got, err := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectOccurrence, subjectID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].Warnings, "warnings NOT NULL DEFAULT '{}'")
}

// -------------------------------------------- the Validate() call site

// TestUpsertManyRefusesWhatTheDDLWouldRefuse is the answer to "is
// Enrichment.Validate() an optional validator nobody runs?".
//
// It is optional on the TYPE — domain.Enrichment has exported fields, no
// constructor, and a zero value that violates six CHECKs at once — but it is NOT
// optional on the WRITE PATH: UpsertMany calls it on every element before it
// queues a single statement, and refuses the whole batch on the first failure.
//
// Every case below therefore fails as a 422-shaped validation error naming the
// invariant, rather than as a 23514 from Postgres with a constraint name in it.
func TestUpsertManyRefusesWhatTheDDLWouldRefuse(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)

	tests := []struct {
		name   string
		mutate func(*domain.Enrichment)
		code   string
	}{
		{
			name:   "no org",
			mutate: func(e *domain.Enrichment) { e.OrgID = "" },
			code:   "enrichment_no_org",
		},
		{
			name:   "no subject",
			mutate: func(e *domain.Enrichment) { e.SubjectID = "" },
			code:   "enrichment_no_subject",
		},
		{
			name:   "a subject kind outside the enum",
			mutate: func(e *domain.Enrichment) { e.SubjectKind = "incident" },
			code:   "enrichment_bad_subject_kind",
		},
		{
			// The dot is mandatory: a bare "runbook" is not storable.
			name:   "an undotted enricher name",
			mutate: func(e *domain.Enrichment) { e.Enricher = "runbook" },
			code:   "enrichment_bad_enricher_name",
		},
		{
			name:   "a version below one",
			mutate: func(e *domain.Enrichment) { e.Version = 0 },
			code:   "enrichment_bad_version",
		},
		{
			name:   "a phase outside the enum",
			mutate: func(e *domain.Enrichment) { e.Phase = domain.Phase(3) },
			code:   "enrichment_bad_phase",
		},
		{
			name:   "a status outside the enum",
			mutate: func(e *domain.Enrichment) { e.Status = "brilliant" },
			code:   "enrichment_bad_status",
		},
		{
			name: "a failure that cannot say why",
			mutate: func(e *domain.Enrichment) {
				e.Status, e.Error = domain.StatusFailed, "   "
			},
			code: "enrichment_missing_error",
		},
		{
			name:   "a negative duration",
			mutate: func(e *domain.Enrichment) { e.Duration = -time.Second },
			code:   "enrichment_negative_duration",
		},
		{
			name:   "an expiry that precedes the computation",
			mutate: func(e *domain.Enrichment) { e.ExpiresAt = e.ComputedAt.Add(-time.Second) },
			code:   "enrichment_bad_expiry",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			subjectID := id.NewString()
			bad := result(org.ID.String(), subjectID)
			tc.mutate(&bad)

			err := repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{bad})

			require.Error(t, err)
			assert.Equal(t, errs.KindValidation, errs.KindOf(err),
				"layer 6 is the backstop, never the error message")
			assert.Equal(t, tc.code, errs.CodeOf(err))

			got, listErr := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectOccurrence, subjectID)
			require.NoError(t, listErr)
			assert.Empty(t, got, "and nothing was written")
		})
	}
}

// TestOneBadRowRefusesTheWholeBatch. UpsertMany validates every element BEFORE
// it queues any statement, so a phase never lands half-written.
func TestOneBadRowRefusesTheWholeBatch(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)
	subjectID := id.NewString()

	good := result(org.ID.String(), subjectID)
	bad := result(org.ID.String(), subjectID)
	bad.Enricher = "alert.history"
	bad.Status, bad.Error = domain.StatusTimeout, ""

	err := repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{good, bad})
	require.Error(t, err)
	assert.Equal(t, "enrichment_missing_error", errs.CodeOf(err))

	got, listErr := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectOccurrence, subjectID)
	require.NoError(t, listErr)
	assert.Empty(t, got, "a phase never lands half-written")
}

func TestASubjectIDThatIsNotAUUIDIsRefused(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)

	bad := result(org.ID.String(), "occurrence-42")
	err := repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{bad})
	require.Error(t, err)
	assert.Equal(t, "enrichment_bad_subject_id", errs.CodeOf(err))

	_, err = repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectOccurrence, "occurrence-42")
	require.Error(t, err)
	assert.Equal(t, "enrichment_bad_subject_id", errs.CodeOf(err))
}

// ------------------------------------------------------------------ tenancy

// TestOneOrgNeverReadsAnotherOrgsEnrichments. `enrichments` is a multi-tenant
// table and a read without an org_id predicate is a cross-tenant leak, not a
// slow query.
func TestOneOrgNeverReadsAnotherOrgsEnrichments(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	alice, bob := h.Org(), h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)
	subjectID := id.NewString()

	require.NoError(t, repo.UpsertMany(h.Ctx, alice.Scope,
		[]domain.Enrichment{result(alice.ID.String(), subjectID)}))

	got, err := repo.ListBySubject(h.Ctx, bob.Scope, domain.SubjectOccurrence, subjectID)
	require.NoError(t, err)
	assert.Empty(t, got, "the same subject id read as another tenant returns nothing")

	got, err = repo.ListBySubject(h.Ctx, alice.Scope, domain.SubjectOccurrence, subjectID)
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

// TestTheScopeOwnsTheOrgColumnNotThePayload: the OrgID a caller put on the
// struct is display data. The row takes the AUTHENTICATED scope's org.
func TestTheScopeOwnsTheOrgColumnNotThePayload(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	alice, bob := h.Org(), h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)
	subjectID := id.NewString()

	// A struct claiming to belong to bob, written under alice's scope.
	claiming := result(bob.ID.String(), subjectID)
	require.NoError(t, repo.UpsertMany(h.Ctx, alice.Scope, []domain.Enrichment{claiming}))

	got, err := repo.ListBySubject(h.Ctx, alice.Scope, domain.SubjectOccurrence, subjectID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, alice.ID.String(), got[0].OrgID,
		"the Subject carries OrgID as a string for display; it is not, and must never be, authorisation")

	got, err = repo.ListBySubject(h.Ctx, bob.Scope, domain.SubjectOccurrence, subjectID)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestAnUnknownSubjectIsAnEmptyListAndNotAnError(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)

	got, err := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectOccurrence, id.NewString())
	require.NoError(t, err, "an un-enriched subject is the normal case on a first fire")
	assert.Empty(t, got)
}
