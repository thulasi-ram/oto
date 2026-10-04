package repository_test

// INVESTIGATORS, INVESTIGATIONS AND STEPS AGAINST A REAL POSTGRES (migration 00092,
// git-bug 180a525). Every claim here is one the SQL makes on its own, whatever the Go
// above it does: a Step is append-only, an ended run is frozen, a reason belongs to its
// status, a version number is taken once, and another org's rows are a 404.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/investigator/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/harness"
)

type world struct {
	h     *harness.H
	scope db.TenantScope
	invs  *repository.InvestigatorRepository
	runs  *repository.InvestigationRepository
	inv   domain.Investigator
}

func newWorld(t *testing.T) world {
	t.Helper()
	h := harness.New(t)
	org := h.Org()
	providers := repository.NewProviderRepository(h.Pool)
	p, err := providers.Insert(h.Ctx, org.Scope, draft("gateway", "https://gw.example.test/v1"), uuid.Nil, h.Now())
	require.NoError(t, err)
	tools, err := domain.NewAllowlist([]string{"oto_case_timeline"})
	require.NoError(t, err)
	spec, err := domain.NewVersionSpec(p.ID, "Read the Case.", tools)
	require.NoError(t, err)
	invs := repository.NewInvestigatorRepository(h.Pool)
	inv, err := invs.Create(h.Ctx, org.Scope, domain.InvestigatorDraft{Name: "firstlook", Enabled: true,
		Budgets: domain.DefaultBudgets(), Spec: spec}, p.Identity(), h.Now())
	require.NoError(t, err)
	return world{h: h, scope: org.Scope, invs: invs, runs: repository.NewInvestigationRepository(h.Pool), inv: inv}
}

func (w world) queued(t *testing.T, alertKey string, at time.Time) domain.Investigation {
	t.Helper()
	run, err := w.runs.Insert(w.h.Ctx, w.scope, domain.Investigation{
		SubjectKind: domain.SubjectCase, SubjectID: uuid.New(), AlertKey: alertKey,
		InvestigatorID: w.inv.ID, VersionID: w.inv.Current.ID, Status: domain.StatusQueued,
		Budgets: w.inv.Budgets, RequestedBy: domain.Requester{Label: "Ada Lovelace"}, RequestedAt: at,
	})
	require.NoError(t, err)
	return run
}

// start starts a run the way the service does: inside a transaction, under the org's
// advisory lock, with room for `maxRunning` at once.
func (w world) start(t *testing.T, id uuid.UUID, at time.Time, maxRunning int) domain.StartOutcome {
	t.Helper()
	var out domain.StartOutcome
	require.NoError(t, db.NewTxRunner(w.h.Pool).InTx(w.h.Ctx, func(ctx context.Context) error {
		var err error
		out, err = w.runs.Start(ctx, w.scope, id, at, maxRunning)
		return err
	}))
	return out
}

func TestAnInvestigatorIsVersionedAndAVersionIsNeverRewritten(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	require.Equal(t, 1, w.inv.Current.Number)
	require.Equal(t, "https://gw.example.test/v1", w.inv.Current.Model.Endpoint)

	spec := domain.VersionSpec{ProviderID: w.inv.Current.ProviderID, Prompt: "Be brief.", Tools: w.inv.Current.Tools}
	_, err := w.invs.AddVersion(w.h.Ctx, w.scope, w.inv.ID, 2, spec, w.inv.Current.Model, w.h.Now())
	require.NoError(t, err)
	_, err = w.invs.AddVersion(w.h.Ctx, w.scope, w.inv.ID, 2, spec, w.inv.Current.Model, w.h.Now())
	require.Equal(t, "investigator_versions_number_uniq", errs.CodeOf(err))

	got, err := w.invs.Get(w.h.Ctx, w.scope, w.inv.ID)
	require.NoError(t, err)
	require.Equal(t, 2, got.Current.Number)
	require.Equal(t, "Be brief.", got.Current.Prompt)
	vs, err := w.invs.Versions(w.h.Ctx, w.scope, w.inv.ID)
	require.NoError(t, err)
	require.Len(t, vs, 2)
	require.Equal(t, 2, vs[0].Number, "newest first")

	// ⛔ An allowlist with a wildcard cannot be stored, whatever wrote it.
	_, err = w.h.Pool.Exec(w.h.Ctx, `UPDATE investigator_versions SET tool_allowlist = ARRAY['oto_*'] WHERE id = $1`, vs[0].ID)
	require.Error(t, err)

	// Another org's Investigator is a 404.
	_, err = w.invs.Get(w.h.Ctx, w.h.Org().Scope, w.inv.ID)
	require.True(t, errs.IsKind(err, errs.KindNotFound), "got %v", err)
}

func TestAStepIsAppendOnly(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	run := w.queued(t, "k", w.h.Now())
	require.Equal(t, domain.StartBegan, w.start(t, run.ID, w.h.Now(), 2))

	turn, err := domain.NewTurn(domain.ModelIdentity{}, "Reading.", []domain.ToolCall{{ID: "c1", Name: "oto_case_timeline", Arguments: "{}"}},
		&domain.Usage{InputTokens: 100, OutputTokens: 10}, domain.FinishToolCalls)
	require.NoError(t, err)
	require.NoError(t, w.runs.AppendStep(w.h.Ctx, w.scope, run.ID, domain.NewModelTurnStep(1, turn, time.Second, w.h.Now())))
	require.NoError(t, w.runs.AppendStep(w.h.Ctx, w.scope, run.ID,
		domain.NewToolStep(2, turn.ToolCalls[0], domain.OutcomeRefused, "refused: not allowed", 0, w.h.Now())))
	// A position is taken once.
	require.Error(t, w.runs.AppendStep(w.h.Ctx, w.scope, run.ID,
		domain.NewToolStep(2, turn.ToolCalls[0], domain.OutcomeOK, "x", 0, w.h.Now())))

	steps, err := w.runs.Steps(w.h.Ctx, w.scope, run.ID)
	require.NoError(t, err)
	require.Len(t, steps, 2)
	require.Equal(t, domain.StepModelTurn, steps[0].Kind)
	require.Equal(t, int64(100), steps[0].Usage.InputTokens)
	require.Equal(t, "oto_case_timeline", steps[0].Calls[0].Name)
	require.Equal(t, domain.OutcomeRefused, steps[1].Outcome)

	// ⛔ The table refuses a rewrite and a direct delete.
	_, err = w.h.Pool.Exec(w.h.Ctx, `UPDATE investigation_steps SET result = 'rewritten' WHERE investigation_id = $1`, run.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "immutable transcript entry")
	_, err = w.h.Pool.Exec(w.h.Ctx, `DELETE FROM investigation_steps WHERE investigation_id = $1`, run.ID)
	require.Error(t, err)

	// ⭐ But the record goes with its owner: deleting the run cascades through.
	_, err = w.h.Pool.Exec(w.h.Ctx, `DELETE FROM investigations WHERE id = $1`, run.ID)
	require.NoError(t, err)
	var n int
	require.NoError(t, w.h.Pool.QueryRow(w.h.Ctx, `SELECT count(*) FROM investigation_steps WHERE investigation_id = $1`, run.ID).Scan(&n))
	require.Zero(t, n)
}

func TestAnEndedRunIsFrozenAndItsReasonBelongsToItsStatus(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	run := w.queued(t, "k", w.h.Now())
	w.start(t, run.ID, w.h.Now(), 2)

	// ⛔ exhausted/disabled is not a pair.
	err := w.runs.Finish(w.h.Ctx, w.scope, run.ID, domain.Ending{Status: domain.StatusExhausted, Reason: domain.ReasonDisabled, Detail: "x"},
		domain.Usage{}, 0, "", w.h.Now())
	require.Error(t, err)

	end := domain.EndedBy(domain.ReasonTokenBudget, "the token budget of 200000 was spent")
	require.NoError(t, w.runs.Finish(w.h.Ctx, w.scope, run.ID, end, domain.Usage{InputTokens: 190000, OutputTokens: 12000}, 7,
		"So far: a deploy.", w.h.Now().Add(time.Minute)))
	got, err := w.runs.Get(w.h.Ctx, w.scope, run.ID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusExhausted, got.Status)
	require.True(t, got.Partial())
	require.Equal(t, end, got.Ending)
	require.Equal(t, int64(202000), got.Spent.Total())
	require.Equal(t, 7, got.ToolCalls)
	require.Equal(t, "firstlook", got.InvestigatorName)
	require.Equal(t, 1, got.VersionNumber)

	// ⛔ Frozen: neither the repository nor a hand can change it now.
	err = w.runs.Finish(w.h.Ctx, w.scope, run.ID, domain.Completed(), domain.Usage{}, 0, "", w.h.Now())
	require.Equal(t, "investigation_already_ended", errs.CodeOf(err))
	_, err = w.h.Pool.Exec(w.h.Ctx, `UPDATE investigations SET finding = 'better' WHERE id = $1`, run.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "frozen")
}

func TestASkippedRunNeverStarted(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	at := w.h.Now()
	run, err := w.runs.Insert(w.h.Ctx, w.scope, domain.Investigation{
		SubjectKind: domain.SubjectCase, SubjectID: uuid.New(), InvestigatorID: w.inv.ID, VersionID: w.inv.Current.ID,
		Status: domain.StatusSkipped, Ending: domain.EndedBy(domain.ReasonDisabled, "switched off"), EndedAt: at,
		Budgets: w.inv.Budgets, RequestedBy: domain.Requester{Label: "Ada"}, RequestedAt: at,
	})
	require.NoError(t, err)
	require.Equal(t, domain.StatusSkipped, run.Status)
	require.True(t, run.StartedAt.IsZero())
	require.Empty(t, run.AlertKey)

	// A queued run skipped by its job also never started.
	q := w.queued(t, "k", at)
	require.NoError(t, w.runs.Finish(w.h.Ctx, w.scope, q.ID, domain.EndedBy(domain.ReasonDisabled, "off"), domain.Usage{}, 0, "", at))
	got, err := w.runs.Get(w.h.Ctx, w.scope, q.ID)
	require.NoError(t, err)
	require.True(t, got.StartedAt.IsZero())
}

func TestASubjectsRunsAreLatestFirstAndPriorFindingsStayInTheirKey(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	subject := uuid.New()
	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		run, err := w.runs.Insert(w.h.Ctx, w.scope, domain.Investigation{
			SubjectKind: domain.SubjectCase, SubjectID: subject, AlertKey: "same",
			InvestigatorID: w.inv.ID, VersionID: w.inv.Current.ID, Status: domain.StatusQueued,
			Budgets: w.inv.Budgets, RequestedBy: domain.Requester{Label: "Ada"},
			RequestedAt: w.h.Now().Add(time.Duration(i) * time.Minute),
		})
		require.NoError(t, err)
		require.Equal(t, domain.StartBegan, w.start(t, run.ID, run.RequestedAt, 2))
		require.NoError(t, w.runs.Finish(w.h.Ctx, w.scope, run.ID, domain.Completed(), domain.Usage{}, 0,
			"finding "+string(rune('a'+i)), run.RequestedAt.Add(time.Second)))
		ids = append(ids, run.ID)
	}
	w.queued(t, "different", w.h.Now())

	page, cur, err := w.runs.ListBySubject(w.h.Ctx, w.scope, domain.SubjectCase, subject, db.Keyset{Limit: 2})
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.Equal(t, ids[2], page[0].ID)
	require.True(t, cur.HasMore)
	rest, _, err := w.runs.ListBySubject(w.h.Ctx, w.scope, domain.SubjectCase, subject, db.Keyset{Limit: 2, Cursor: cur})
	require.NoError(t, err)
	require.Len(t, rest, 1)
	require.Equal(t, ids[0], rest[0].ID)

	prior, err := w.runs.PriorFindings(w.h.Ctx, w.scope, "same", ids[2], 10)
	require.NoError(t, err)
	require.Len(t, prior, 2)
	require.Equal(t, "finding b", prior[0].Finding, "newest first, the asking run excluded")
	none, err := w.runs.PriorFindings(w.h.Ctx, w.h.Org().Scope, "same", uuid.Nil, 10)
	require.NoError(t, err)
	require.Empty(t, none, "another org's Findings are not this org's memory")

	_, err = w.runs.Get(w.h.Ctx, w.h.Org().Scope, ids[0])
	require.True(t, errs.IsKind(err, errs.KindNotFound))
}

// ------------------------------------------------------------------ ADR 0053 §6
//
// git-bug bf172fe: the org's daily budget, its concurrency, and an Investigator's
// minimum interval, as the SQL holds them (migration 00094).

func TestASkippedBudgetRunIsAdmittedAndADisabledReasonStaysSkippedOnly(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	at := w.h.Now()
	run, err := w.runs.Insert(w.h.Ctx, w.scope, domain.Investigation{
		SubjectKind: domain.SubjectCase, SubjectID: uuid.New(), InvestigatorID: w.inv.ID, VersionID: w.inv.Current.ID,
		Status: domain.StatusSkipped, Ending: domain.EndedBy(domain.ReasonBudget, "the day is spent"), EndedAt: at,
		Budgets: w.inv.Budgets, RequestedBy: domain.Requester{Label: "Ada"}, RequestedAt: at,
	})
	require.NoError(t, err)
	require.Equal(t, domain.ReasonBudget, run.Ending.Reason)
	require.True(t, run.StartedAt.IsZero())

	// ⛔ `budget` belongs to `skipped`, and nothing else.
	q := w.queued(t, "k", at)
	w.start(t, q.ID, at, 2)
	_, err = w.h.Pool.Exec(w.h.Ctx,
		`UPDATE investigations SET status = 'failed', reason = 'budget', reason_detail = 'x', ended_at = $2 WHERE id = $1`, q.ID, at)
	require.Error(t, err)
}

func TestTheDaysSpendIsTodaysModelTurns(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	midnight := domain.DayStart(w.h.Now())
	turn := func(in, out int64) domain.Turn {
		tr, err := domain.NewTurn(domain.ModelIdentity{}, "x", nil, &domain.Usage{InputTokens: in, OutputTokens: out}, domain.FinishStop)
		require.NoError(t, err)
		return tr
	}
	run := w.queued(t, "k", midnight.Add(-time.Hour))
	w.start(t, run.ID, midnight.Add(-time.Hour), 2)
	// Yesterday's turn is yesterday's; today's two count, and a Tool call has no tokens.
	require.NoError(t, w.runs.AppendStep(w.h.Ctx, w.scope, run.ID, domain.NewModelTurnStep(1, turn(1000, 100), 0, midnight.Add(-time.Minute))))
	require.NoError(t, w.runs.AppendStep(w.h.Ctx, w.scope, run.ID, domain.NewModelTurnStep(2, turn(300, 30), 0, midnight)))
	require.NoError(t, w.runs.AppendStep(w.h.Ctx, w.scope, run.ID,
		domain.NewToolStep(3, domain.ToolCall{ID: "c", Name: "oto_case_timeline", Arguments: "{}"}, domain.OutcomeOK, "ok", 0, midnight)))
	require.NoError(t, w.runs.AppendStep(w.h.Ctx, w.scope, run.ID, domain.NewModelTurnStep(4, turn(50, 5), 0, midnight.Add(time.Hour))))

	spent, err := w.runs.SpentSince(w.h.Ctx, w.scope, midnight)
	require.NoError(t, err)
	require.Equal(t, int64(385), spent, "a run still going is counted, from its Steps")

	other, err := w.runs.SpentSince(w.h.Ctx, w.h.Org().Scope, midnight)
	require.NoError(t, err)
	require.Zero(t, other, "another org's spend is not this org's")
}

func TestStartHoldsTheOrgsConcurrencyAndNeedsATransaction(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	at := w.h.Now()
	a, b, c := w.queued(t, "a", at), w.queued(t, "b", at), w.queued(t, "c", at)

	require.Equal(t, domain.StartBegan, w.start(t, a.ID, at, 2))
	require.Equal(t, domain.StartBegan, w.start(t, b.ID, at, 2))
	require.Equal(t, domain.StartAtCapacity, w.start(t, c.ID, at, 2))
	got, err := w.runs.Get(w.h.Ctx, w.scope, c.ID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusQueued, got.Status, "a run past the concurrency waits; it is never dropped")
	n, err := w.runs.CountRunning(w.h.Ctx, w.scope)
	require.NoError(t, err)
	require.Equal(t, 2, n)

	// A slot frees; the waiting run takes it. A run already started is not queued.
	require.NoError(t, w.runs.Finish(w.h.Ctx, w.scope, a.ID, domain.Completed(), domain.Usage{}, 0, "", at))
	require.Equal(t, domain.StartBegan, w.start(t, c.ID, at, 2))
	require.Equal(t, domain.StartNotQueued, w.start(t, b.ID, at, 3))

	// ⛔ Outside a transaction the advisory lock would guard nothing, so it is refused.
	_, err = w.runs.Start(w.h.Ctx, w.scope, w.queued(t, "d", at).ID, at, 10)
	require.Error(t, err)
}

func TestTheIntervalReadsTheSubjectsQueuedAndLastRunsAndNotBeforeRoundTrips(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	at := w.h.Now()
	subject := uuid.New()
	insert := func(status domain.Status, requested time.Time, notBefore time.Time) domain.Investigation {
		inv := domain.Investigation{
			SubjectKind: domain.SubjectCase, SubjectID: subject, InvestigatorID: w.inv.ID, VersionID: w.inv.Current.ID,
			Status: status, Budgets: w.inv.Budgets, RequestedBy: domain.Requester{Label: "Correlator"},
			RequestedAt: requested, NotBefore: notBefore,
		}
		if status == domain.StatusSkipped {
			inv.Ending, inv.EndedAt = domain.EndedBy(domain.ReasonBudget, "spent"), requested
		}
		out, err := w.runs.Insert(w.h.Ctx, w.scope, inv)
		require.NoError(t, err)
		return out
	}
	lock := func() domain.SubjectRuns {
		var out domain.SubjectRuns
		require.NoError(t, db.NewTxRunner(w.h.Pool).InTx(w.h.Ctx, func(ctx context.Context) error {
			var err error
			out, err = w.runs.LockSubjectRuns(ctx, w.scope, w.inv.ID, domain.SubjectCase, subject)
			return err
		}))
		return out
	}

	none := lock()
	require.Nil(t, none.Queued)
	require.Nil(t, none.Last)

	first := insert(domain.StatusQueued, at, time.Time{})
	w.start(t, first.ID, at.Add(time.Minute), 2)
	insert(domain.StatusSkipped, at.Add(2*time.Minute), time.Time{}) // a skip opens no interval
	deferred := insert(domain.StatusQueued, at.Add(3*time.Minute), at.Add(11*time.Minute))
	require.Equal(t, at.Add(11*time.Minute).UTC(), deferred.NotBefore)

	runs := lock()
	require.NotNil(t, runs.Queued)
	require.Equal(t, deferred.ID, runs.Queued.ID)
	require.NotNil(t, runs.Last)
	require.Equal(t, first.ID, runs.Last.ID)
	require.Equal(t, at.Add(time.Minute).UTC(), runs.Last.StartedAt)

	// ⛔ A not_before earlier than the request is refused at the row.
	_, err := w.h.Pool.Exec(w.h.Ctx, `UPDATE investigations SET not_before = requested_at - interval '1 second' WHERE id = $1`, deferred.ID)
	require.Error(t, err)
}

func TestAnInvestigatorsMinimumIntervalIsStoredAndBounded(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	require.Equal(t, time.Duration(0), w.inv.MinInterval, "the draft named none, and the repository writes what it is given")
	require.NoError(t, w.invs.Update(w.h.Ctx, w.scope, w.inv.ID, true, w.inv.Budgets, 15*time.Minute, w.h.Now()))
	got, err := w.invs.Get(w.h.Ctx, w.scope, w.inv.ID)
	require.NoError(t, err)
	require.Equal(t, 15*time.Minute, got.MinInterval)
	require.Equal(t, 1, got.Current.Number, "an interval is not a version")

	_, err = w.h.Pool.Exec(w.h.Ctx, `UPDATE investigators SET min_interval_s = 86401 WHERE id = $1`, w.inv.ID)
	require.Error(t, err)
}
