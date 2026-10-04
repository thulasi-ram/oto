package repository_test

// A FINDING'S SUGGESTIONS AGAINST A REAL POSTGRES (migration 00097, git-bug 8327c00). Every
// claim here is one the SQL makes on its own: a lapsed Suggestion is not read, an applied
// one stays readable with who applied it, it is applied once, a proposal is never
// rewritten, a row cannot say half of two changes, and another org's Suggestion is a 404.

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
)

func TestASuggestionIsShownUntilItLapsesAndIsAppliedOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	repo := repository.NewSuggestionRepository(w.h.Pool)
	run := w.queued(t, "k", w.h.Now())
	at := w.h.Now()

	drafts := []domain.SuggestionDraft{
		{Kind: domain.SuggestCountCondition, Why: "It flaps.", Count: domain.CountChange{
			PolicyID: uuid.New(), PolicyName: "crashloops", CountMin: 3, CountWindow: 10 * time.Minute}},
		{Kind: domain.SuggestMembership, Why: "Same story.", Membership: domain.MembershipChange{
			IncidentID: uuid.New(), IncidentNumber: 4, CaseID: uuid.New(), CaseNumber: 9}},
	}
	require.NoError(t, repo.InsertSuggestions(w.h.Ctx, w.scope, run.ID, drafts, at, at.Add(domain.SuggestionLapse)))

	shown, err := repo.ListSuggestions(w.h.Ctx, w.scope, run.ID, at)
	require.NoError(t, err)
	require.Len(t, shown, 2)
	require.Equal(t, domain.SuggestCountCondition, shown[0].Kind, "in the order proposed")
	require.Equal(t, 3, shown[0].Count.CountMin)
	require.Equal(t, 10*time.Minute, shown[0].Count.CountWindow)
	require.Zero(t, shown[0].Count.WasMin)
	require.Equal(t, int64(9), shown[1].Membership.CaseNumber)

	// Applied, once, inside a transaction holding the row.
	by := domain.Requester{Label: "Grace Hopper"}
	tx := db.NewTxRunner(w.h.Pool)
	require.NoError(t, tx.InTx(w.h.Ctx, func(ctx context.Context) error {
		locked, err := repo.LockSuggestion(ctx, w.scope, shown[0].ID)
		if err != nil {
			return err
		}
		return repo.MarkApplied(ctx, w.scope, locked.ID, by, at.Add(time.Minute))
	}))
	err = repo.MarkApplied(w.h.Ctx, w.scope, shown[0].ID, by, at.Add(2*time.Minute))
	require.Equal(t, "suggestion_already_applied", errs.CodeOf(err))

	// ⭐ Past the lapse: the applied one stays shown with who applied it, the open one is
	// not read at all — and is still there to be refused as lapsed.
	later := at.Add(domain.SuggestionLapse + time.Second)
	shown, err = repo.ListSuggestions(w.h.Ctx, w.scope, run.ID, later)
	require.NoError(t, err)
	require.Len(t, shown, 1)
	require.Equal(t, "Grace Hopper", shown[0].AppliedBy.Label)
	require.Equal(t, domain.SuggestionApplied, shown[0].StateAt(later))
	all, err := repo.ListSuggestions(w.h.Ctx, w.scope, run.ID, at)
	require.NoError(t, err)
	lapsed, err := repo.LockSuggestion(w.h.Ctx, w.scope, all[1].ID)
	require.NoError(t, err)
	require.Equal(t, domain.SuggestionLapsed, lapsed.StateAt(later))

	// Another org's Suggestion is a 404, read or applied.
	_, err = repo.LockSuggestion(w.h.Ctx, w.h.Org().Scope, all[1].ID)
	require.True(t, errs.IsKind(err, errs.KindNotFound), "%v", err)
}

func TestTheDatabaseRefusesARewrittenProposalAndAHalfShapedRow(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	repo := repository.NewSuggestionRepository(w.h.Pool)
	run := w.queued(t, "k", w.h.Now())
	at := w.h.Now()
	require.NoError(t, repo.InsertSuggestions(w.h.Ctx, w.scope, run.ID, []domain.SuggestionDraft{
		{Kind: domain.SuggestCountCondition, Why: "It flaps.", Count: domain.CountChange{
			PolicyID: uuid.New(), PolicyName: "crashloops", CountMin: 3, CountWindow: 10 * time.Minute}},
	}, at, at.Add(domain.SuggestionLapse)))
	shown, err := repo.ListSuggestions(w.h.Ctx, w.scope, run.ID, at)
	require.NoError(t, err)
	id := shown[0].ID

	// ⛔ A proposal is never rewritten — not its number, not its lapse.
	_, err = w.h.Pool.Exec(w.h.Ctx, `UPDATE investigation_suggestions SET count_min = 50 WHERE id = $1`, id)
	require.Error(t, err)
	_, err = w.h.Pool.Exec(w.h.Ctx, `UPDATE investigation_suggestions SET lapses_at = lapses_at + interval '1 year' WHERE id = $1`, id)
	require.Error(t, err)

	// ⛔ A row cannot say half of two changes.
	_, err = w.h.Pool.Exec(w.h.Ctx, `
INSERT INTO investigation_suggestions (id, org_id, investigation_id, kind, policy_id, policy_name, count_min,
  count_window_s, case_id, case_number, why, proposed_at, lapses_at)
VALUES ($1, $2, $3, 'policy_count_condition', $4, 'p', 3, 600, $5, 9, 'x', $6, $7)`,
		uuid.New(), w.scope.OrgID(), run.ID, uuid.New(), uuid.New(), at, at.Add(time.Hour))
	require.Equal(t, "investigation_suggestions_shape_ck", errs.CodeOf(mapPG(err)))

	// ⛔ An applied row is frozen.
	require.NoError(t, repo.MarkApplied(w.h.Ctx, w.scope, id, domain.Requester{Label: "Grace"}, at.Add(time.Minute)))
	_, err = w.h.Pool.Exec(w.h.Ctx, `UPDATE investigation_suggestions SET applied_by_label = 'someone else' WHERE id = $1`, id)
	require.Error(t, err)
}

// mapPG is the repository's own translation of a raw statement's error, so a CHECK's name
// is read as the code oto would report.
func mapPG(err error) error {
	return db.MapError(err, db.ErrorPolicy{QueryFailed: "q", QueryFailedMessage: "q"})
}
