package repository_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
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
// What this file no longer tests is as telling as what it does: every CHECK on
// `enrichments` is proven by domain.NewEnrichment — see the domain tests — so a
// row the table would refuse cannot reach a repository method at all. What is
// left here is the half the domain cannot answer: the shape SQL needs, the
// payload encoding, and the tenancy of the write.

// resultParams is the provenanced baseline: one typed result from one named,
// versioned Enricher.
func resultParams(orgID, subjectID string) domain.EnrichmentParams {
	return domain.EnrichmentParams{
		ID:          id.NewString(),
		OrgID:       orgID,
		SubjectKind: domain.SubjectCase,
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

// result builds a storable row. Every fixture goes through the constructor
// because there is no other way in: a result the table would refuse is not
// representable, so a test cannot assemble one either.
func result(t *testing.T, p domain.EnrichmentParams) domain.Enrichment {
	t.Helper()

	e, err := domain.NewEnrichment(p)
	require.NoError(t, err)
	return e
}

// ------------------------------------------------------------- round trip

func TestAProvenancedResultRoundTrips(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)
	subjectID := id.NewString()

	p := resultParams(org.ID.String(), subjectID)
	p.Warnings = []string{"ambiguous_rule_match"}
	p.FromCache = true
	in := result(t, p)
	require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{in}))

	got, err := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectCase, subjectID)
	require.NoError(t, err)
	require.Len(t, got, 1)

	// The read itself is the assertion that what comes back out is storable
	// again: enrichmentRow.toDomain goes through the same constructor as the
	// write, so a row the domain would refuse surfaces as a failed ListBySubject
	// rather than as a valid-looking record.
	out := got[0]
	assert.Equal(t, in.ID(), out.ID())
	assert.Equal(t, org.ID.String(), out.OrgID())
	assert.Equal(t, "prom.rule", out.Enricher())
	assert.Equal(t, 1, out.Version())
	assert.Equal(t, domain.PhaseInline, out.Phase())
	assert.Equal(t, domain.StatusOK, out.Status())
	assert.Equal(t, []string{"ambiguous_rule_match"}, out.Warnings())
	assert.True(t, out.FromCache(), "provenance: a reused answer must be distinguishable from a fresh one")
	assert.Equal(t, 123*time.Millisecond, out.Duration(),
		"wall time is recorded even on success because it is what feeds the budget")
	assert.True(t, out.ComputedAt().Equal(harness.Epoch))
	assert.True(t, out.ExpiresAt().Equal(harness.Epoch.Add(5*time.Minute)))
	assert.Equal(t, time.UTC, out.ComputedAt().Location())

	raw, ok := out.Payload().(json.RawMessage)
	require.True(t, ok, "the payload comes back as raw JSON: this layer must not know every enricher's shape")
	assert.JSONEq(t, `{"expr":"up == 0","available":true}`, string(raw))
}

func TestAFailedResultIsStoredWithTheReasonAttached(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)
	subjectID := id.NewString()

	failedParams := resultParams(org.ID.String(), subjectID)
	failedParams.Enricher = "alert.history"
	failedParams.Status = domain.StatusFailed
	failedParams.Error = "the pool is exhausted"
	failedParams.Payload = map[string]any{}
	failedParams.ExpiresAt = time.Time{}
	failed := result(t, failedParams)

	timedOutParams := resultParams(org.ID.String(), subjectID)
	timedOutParams.Enricher = "alert.related"
	timedOutParams.Status = domain.StatusTimeout
	timedOutParams.Error = "exceeded its 800ms budget"
	timedOutParams.Payload = map[string]any{}
	timedOutParams.ExpiresAt = time.Time{}
	timedOut := result(t, timedOutParams)

	require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{failed, timedOut}))

	got, err := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectCase, subjectID)
	require.NoError(t, err)
	require.Len(t, got, 2)

	assert.Equal(t, "alert.history", got[0].Enricher(), "ordered by enricher, so two reads render the same")
	assert.Equal(t, "alert.related", got[1].Enricher())

	for _, e := range got {
		assert.NotEmpty(t, e.ErrorText(), "enrichments_err_ck: a failure that cannot say why is a rumour")
		assert.Zero(t, e.ExpiresAt(), "a failure does not go stale; it is simply never reused")
		assert.False(t, e.Status().Succeeded())
	}
}

// TestReRunningAPhaseOverwritesItsOwnRows is what makes `enrich.run` idempotent
// on (case_id, phase): the conflict target omits the version, so a version
// bump REPLACES the old answer rather than accumulating beside it.
func TestReRunningAPhaseOverwritesItsOwnRows(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)
	subjectID := id.NewString()

	firstParams := resultParams(org.ID.String(), subjectID)
	firstParams.Status = domain.StatusFailed
	firstParams.Error = "prometheus was down"
	firstParams.Payload = map[string]any{}
	firstParams.ExpiresAt = time.Time{}
	require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{result(t, firstParams)}))

	// The retry succeeds, at a bumped version.
	secondParams := resultParams(org.ID.String(), subjectID)
	secondParams.Version = 2
	require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{result(t, secondParams)}))

	got, err := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectCase, subjectID)
	require.NoError(t, err)
	require.Len(t, got, 1, "a retry converges; it never double-counts a failure")
	assert.Equal(t, domain.StatusOK, got[0].Status())
	assert.Equal(t, 2, got[0].Version())
	assert.Empty(t, got[0].ErrorText(), "the old failure's reason does not survive the success")
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
		p := resultParams(org.ID.String(), subjectID)
		p.Enricher = name
		batch = append(batch, result(t, p))
	}
	require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope, batch))

	got, err := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectCase, subjectID)
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
		p := resultParams(org.ID.String(), subjectID)
		p.Payload = payload
		require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{result(t, p)}), name)

		got, err := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectCase, subjectID)
		require.NoError(t, err)
		require.Len(t, got, 1)
		raw, ok := got[0].Payload().(json.RawMessage)
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

	p := resultParams(org.ID.String(), subjectID)
	p.Payload = nil
	require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{result(t, p)}))

	got, err := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectCase, subjectID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.JSONEq(t, `{}`, string(got[0].Payload().(json.RawMessage)),
		"a null would be rejected by the CHECK, and the place to notice that is here")
}

func TestNilWarningsAreStoredAsAnEmptyArray(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)
	subjectID := id.NewString()

	require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope,
		[]domain.Enrichment{result(t, resultParams(org.ID.String(), subjectID))}))

	got, err := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectCase, subjectID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].Warnings(), "warnings NOT NULL DEFAULT '{}'")
}

// ------------------------------------------- what the write path still owns

// The DDL's CHECK constraints are NOT tested here any more, and that is the
// point of the change that removed them: they are proven by
// domain.NewEnrichment, so the rows this file could once build and hand to
// UpsertMany — no org, an undotted enricher, a failure with no reason — are not
// values that exist. TestNewEnrichmentRefusesWhatTheTableWouldRefuse in the
// domain package is where each of them now fails, one layer earlier and without
// a database.
//
// What UpsertMany still owns is the shape SQL needs and the domain does not: the
// subject id as a UUID. It is checked for EVERY element before a single
// statement is queued, so a phase never lands half-written.
func TestOneBadRowRefusesTheWholeBatch(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)
	subjectID := id.NewString()

	good := result(t, resultParams(org.ID.String(), subjectID))

	badParams := resultParams(org.ID.String(), "case-42")
	badParams.Enricher = "alert.history"
	bad := result(t, badParams)

	err := repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{good, bad})
	require.Error(t, err)
	assert.Equal(t, errs.KindValidation, errs.KindOf(err))
	assert.Equal(t, "enrichment_bad_subject_id", errs.CodeOf(err))

	got, listErr := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectCase, subjectID)
	require.NoError(t, listErr)
	assert.Empty(t, got, "a phase never lands half-written")
}

// ------------------------------------------------- a row the domain refuses

// TestARowTheDomainCannotInterpretIsAbsentRatherThanFatal is the read path's
// half of strict construction, and it is the opposite answer to the write path's.
//
// The DDL is LOOSER than the constructor and always will be. enrichments_err_ck
// is `status NOT IN ('failed','timeout') OR error IS NOT NULL` — an EMPTY STRING
// satisfies it — while NewEnrichment demands a non-blank reason. So this row is
// legal in the table and unbuildable in Go, which is what a row written before
// the constructor existed, or edited by hand at 3am, looks like.
//
// Failing the whole read on it would wedge it permanently: the pipeline lists a
// subject's enrichments BEFORE it runs the enrichers, so the row that would
// repair this one is written downstream of the read that refuses it, and the
// early return means no enricher runs, nothing is written, and the deferred
// notification is never released. Treating it as ABSENT is what lets the
// enricher re-run and overwrite it — which is the honest reading anyway: a row
// we cannot interpret is one we should recompute.
func TestARowTheDomainCannotInterpretIsAbsentRatherThanFatal(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()

	var logs bytes.Buffer
	repo := repository.NewEnrichmentRepository(h.Pool).
		WithLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})))
	subjectID := id.New()
	rowID := id.New()

	// Written around the domain, because there is no way through it: this is a
	// row the table accepts and the constructor does not.
	h.Exec(`
		INSERT INTO enrichments (
		  id, org_id, subject_kind, subject_id, enricher, enricher_version,
		  phase, status, payload, warnings, error, duration_ms, from_cache, computed_at)
		VALUES ($1,$2,'case',$3,'prom.rule',1,1,'failed','{}'::jsonb,'{}','',0,false,$4)`,
		rowID, org.ID, subjectID, harness.Epoch)

	got, err := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectCase, subjectID.String())
	require.NoError(t, err,
		"a stored row the domain cannot interpret must not fail the read that would repair it")
	assert.Empty(t, got, "it is absent, not fatal: absent is what makes the caller recompute it")

	// Loudly, though. Skipping silently would leave a permanently unreadable row
	// with nothing anywhere saying so, and nothing this repository writes can
	// produce one — so the log line is the only way it gets found.
	line := logs.String()
	assert.Contains(t, line, `"level":"ERROR"`)
	for _, want := range []string{
		rowID.String(), org.ID.String(), subjectID.String(), "prom.rule", "case",
	} {
		assert.Contains(t, line, want, "the log must carry enough identity to find the row")
	}

	// And the repair actually happens: the enricher re-runs and its result lands
	// on the same (subject_kind, subject_id, enricher) conflict target.
	repaired := resultParams(org.ID.String(), subjectID.String())
	require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{result(t, repaired)}))

	got, err = repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectCase, subjectID.String())
	require.NoError(t, err)
	require.Len(t, got, 1, "the row that could not be read has been overwritten by one that can")
	assert.Equal(t, domain.StatusOK, got[0].Status())
	assert.Equal(t, "prom.rule", got[0].Enricher())
}

// TestOneUnreadableRowDoesNotHideItsSiblings: the skip is per row, not per read.
func TestOneUnreadableRowDoesNotHideItsSiblings(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool).
		WithLogger(slog.New(slog.DiscardHandler))
	subjectID := id.New()

	good := resultParams(org.ID.String(), subjectID.String())
	good.Enricher = "alert.history"
	require.NoError(t, repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{result(t, good)}))

	h.Exec(`
		INSERT INTO enrichments (
		  id, org_id, subject_kind, subject_id, enricher, enricher_version,
		  phase, status, payload, warnings, error, duration_ms, from_cache, computed_at)
		VALUES ($1,$2,'case',$3,'prom.rule',1,1,'timeout','{}'::jsonb,'{}','',0,false,$4)`,
		id.New(), org.ID, subjectID, harness.Epoch)

	got, err := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectCase, subjectID.String())
	require.NoError(t, err)
	require.Len(t, got, 1, "one damaged row must not take the subject's whole context down with it")
	assert.Equal(t, "alert.history", got[0].Enricher())
}

func TestASubjectIDThatIsNotAUUIDIsRefused(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)

	bad := result(t, resultParams(org.ID.String(), "case-42"))
	err := repo.UpsertMany(h.Ctx, org.Scope, []domain.Enrichment{bad})
	require.Error(t, err)
	assert.Equal(t, "enrichment_bad_subject_id", errs.CodeOf(err))

	_, err = repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectCase, "case-42")
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
		[]domain.Enrichment{result(t, resultParams(alice.ID.String(), subjectID))}))

	got, err := repo.ListBySubject(h.Ctx, bob.Scope, domain.SubjectCase, subjectID)
	require.NoError(t, err)
	assert.Empty(t, got, "the same subject id read as another tenant returns nothing")

	got, err = repo.ListBySubject(h.Ctx, alice.Scope, domain.SubjectCase, subjectID)
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
	claiming := result(t, resultParams(bob.ID.String(), subjectID))
	require.NoError(t, repo.UpsertMany(h.Ctx, alice.Scope, []domain.Enrichment{claiming}))

	got, err := repo.ListBySubject(h.Ctx, alice.Scope, domain.SubjectCase, subjectID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, alice.ID.String(), got[0].OrgID(),
		"the Subject carries OrgID as a string for display; it is not, and must never be, authorisation")

	got, err = repo.ListBySubject(h.Ctx, bob.Scope, domain.SubjectCase, subjectID)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestAnUnknownSubjectIsAnEmptyListAndNotAnError(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewEnrichmentRepository(h.Pool)

	got, err := repo.ListBySubject(h.Ctx, org.Scope, domain.SubjectCase, id.NewString())
	require.NoError(t, err, "an un-enriched subject is the normal case on a first fire")
	assert.Empty(t, got)
}
