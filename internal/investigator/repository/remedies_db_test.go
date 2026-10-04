package repository_test

// A FINDING'S REMEDIES AGAINST A REAL POSTGRES (migration 00100, git-bug 4148256). Every claim
// here is one the SQL makes on its own: the hash IS the hash of the arguments; a proposal is
// never rewritten; a Remedy moves only along its transitions, from the state its writer read;
// a terminal one is frozen; a Remedy with no Tool can never be approved; one person approves
// once; the approvals and transitions are append-only; and another org's Remedy is a 404.

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

const dbRemedyArgs = `{"namespace":"checkout","deployment":"api","generation":12345678901234567890}`

// user seeds one member of the world's org.
func (w world) user(t *testing.T, name string) domain.Requester {
	t.Helper()
	id := uuid.New()
	w.h.Exec(`INSERT INTO users (id, org_id, email, display_name, created_at, updated_at)
	          VALUES ($1, $2, $3, $4, $5, $5)`, id, w.scope.OrgID(), id.String()+"@example.test", name, w.h.Now())
	return domain.Requester{UserID: id, Label: name}
}

func (w world) remedy(t *testing.T, repo *repository.RemedyRepository, withTool bool) domain.Remedy {
	t.Helper()
	run := w.queued(t, "k", w.h.Now())
	at := w.h.Now()
	r := domain.Remedy{ID: uuid.New(), OrgID: w.scope.OrgID(), InvestigationID: run.ID,
		SubjectKind: domain.SubjectCase, SubjectID: run.SubjectID, ProposedBy: "Investigator firstlook v1",
		Target: "Deployment checkout/api", Description: "Restart it.", RequiredApprovals: domain.DefaultRequiredApprovals,
		State: domain.RemedyProposed, ProposedAt: at, ExpiresAt: at.Add(time.Hour)}
	if withTool {
		r.Tool = domain.RemedyTool{ToolServerID: uuid.New(), ToolServerName: "k8s-write", Tool: "rollout_restart"}
		r.Arguments, r.ArgumentsSHA256 = dbRemedyArgs, domain.HashArguments(dbRemedyArgs)
	}
	proposal := domain.RemedyTransition{ID: uuid.New(), To: domain.RemedyProposed,
		Actor: domain.RemedyActor{Kind: domain.ActorInvestigator, Label: r.ProposedBy}, At: at}
	require.NoError(t, repo.InsertRemedy(w.h.Ctx, w.scope, r, proposal))
	return r
}

func move(from, to domain.RemedyState, actor domain.RemedyActor, at time.Time) domain.RemedyTransition {
	return domain.RemedyTransition{ID: uuid.New(), From: from, To: to, Actor: actor, At: at}
}

func TestARemedyMovesOnlyAlongItsTransitionsAndEndsFrozen(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	repo := repository.NewRemedyRepository(w.h.Pool)
	r := w.remedy(t, repo, true)
	ada, grace := w.user(t, "Ada Lovelace"), w.user(t, "Grace Hopper")
	at := w.h.Now().Add(time.Minute)

	got, err := repo.GetRemedy(w.h.Ctx, w.scope, r.ID)
	require.NoError(t, err)
	require.Equal(t, dbRemedyArgs, got.Arguments, "the arguments are the bytes proposed")
	require.Equal(t, domain.HashArguments(dbRemedyArgs), got.ArgumentsSHA256)
	require.Len(t, got.Transitions, 1)
	require.Equal(t, domain.ActorInvestigator, got.Transitions[0].Actor.Kind)

	// ⭐ ONE PERSON, ONE APPROVAL.
	a := domain.RemedyApproval{UserID: ada.UserID, Label: ada.Label, ArgumentsSHA256: r.ArgumentsSHA256, ApprovedAt: at}
	require.NoError(t, repo.AddApproval(w.h.Ctx, w.scope, r.ID, a))
	require.Equal(t, "remedy_already_approved", errs.CodeOf(repo.AddApproval(w.h.Ctx, w.scope, r.ID, a)))
	require.NoError(t, repo.AddApproval(w.h.Ctx, w.scope, r.ID, domain.RemedyApproval{UserID: grace.UserID,
		Label: grace.Label, ArgumentsSHA256: r.ArgumentsSHA256, ApprovedAt: at}))

	tx := db.NewTxRunner(w.h.Pool)
	require.NoError(t, tx.InTx(w.h.Ctx, func(ctx context.Context) error {
		if _, err := repo.LockRemedy(ctx, w.scope, r.ID); err != nil {
			return err
		}
		return repo.Transition(ctx, w.scope, r.ID, move(domain.RemedyProposed, domain.RemedyApproved,
			domain.UserActor(grace), at), at.Add(time.Hour), "")
	}))
	// A writer that read `proposed` meanwhile is refused: the Remedy moved.
	err = repo.Transition(w.h.Ctx, w.scope, r.ID, move(domain.RemedyProposed, domain.RemedyDeclined,
		domain.UserActor(ada), at), time.Time{}, "")
	require.Equal(t, "remedy_moved", errs.CodeOf(err))

	got, err = repo.GetRemedy(w.h.Ctx, w.scope, r.ID)
	require.NoError(t, err)
	require.Equal(t, domain.RemedyApproved, got.State)
	require.Len(t, got.Approvals, 2)
	require.Len(t, got.Transitions, 2)
	require.WithinDuration(t, at.Add(time.Hour), got.ExpiresAt, time.Microsecond)

	require.NoError(t, repo.Transition(w.h.Ctx, w.scope, r.ID, move(domain.RemedyApproved, domain.RemedyDeclined,
		domain.UserActor(ada), at.Add(time.Minute)), time.Time{}, ""))

	// ⛔ A terminal Remedy is frozen, and no move leaves it.
	_, err = w.h.Pool.Exec(w.h.Ctx, `UPDATE remedies SET state = 'executing', executing_at = now() WHERE id = $1`, r.ID)
	require.Error(t, err)
	// ⛔ The approvals and transitions are append-only.
	_, err = w.h.Pool.Exec(w.h.Ctx, `DELETE FROM remedy_approvals WHERE remedy_id = $1`, r.ID)
	require.Error(t, err)
	_, err = w.h.Pool.Exec(w.h.Ctx, `UPDATE remedy_transitions SET actor_label = 'someone else' WHERE remedy_id = $1`, r.ID)
	require.Error(t, err)

	// Another org's Remedy is a 404.
	_, err = repo.GetRemedy(w.h.Ctx, w.h.Org().Scope, r.ID)
	require.True(t, errs.IsKind(err, errs.KindNotFound), "%v", err)
}

func TestTheDatabaseRefusesARewrittenProposalAForgedHashAndAnApprovedRemedyWithNoTool(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	repo := repository.NewRemedyRepository(w.h.Pool)
	r := w.remedy(t, repo, true)

	// ⛔ What was approved is what is executed: the arguments, the Tool and the target are
	// never rewritten.
	for _, stmt := range []string{
		`UPDATE remedies SET arguments = '{"namespace":"kube-system"}', state = 'approved', approved_at = proposed_at WHERE id = $1`,
		`UPDATE remedies SET tool_name = 'delete', state = 'declined', ended_at = proposed_at WHERE id = $1`,
		`UPDATE remedies SET target = 'everything', state = 'declined', ended_at = proposed_at WHERE id = $1`,
		// An UPDATE that moves no state is not a transition.
		`UPDATE remedies SET expires_at = expires_at + interval '1 day' WHERE id = $1`,
		// A move outside the transitions.
		`UPDATE remedies SET state = 'executed', approved_at = proposed_at, executing_at = proposed_at, ended_at = proposed_at WHERE id = $1`,
	} {
		_, err := w.h.Pool.Exec(w.h.Ctx, stmt, r.ID)
		require.Error(t, err, stmt)
	}

	// ⛔ A hash that is not the arguments' is refused at the row.
	run := w.queued(t, "k2", w.h.Now())
	_, err := w.h.Pool.Exec(w.h.Ctx, `
INSERT INTO remedies (id, org_id, investigation_id, subject_kind, subject_id, proposed_by_label, tool_server_id,
  tool_server_name, tool_name, arguments, arguments_sha256, target, description, required_approvals, state,
  proposed_at, expires_at)
VALUES ($1, $2, $3, 'case', $4, 'x', $5, 'k8s-write', 'rollout_restart', '{"a":1}', $6, 't', 'd', 2, 'proposed', $7, $8)`,
		uuid.New(), w.scope.OrgID(), run.ID, run.SubjectID, uuid.New(), domain.HashArguments(`{"a":2}`),
		w.h.Now(), w.h.Now().Add(time.Hour))
	require.Equal(t, "remedies_arguments_hash_ck", errs.CodeOf(mapPG(err)))

	// ⛔ A Remedy with no Tool can never be approved.
	none := w.remedy(t, repo, false)
	ada := w.user(t, "Ada Lovelace")
	err = repo.Transition(w.h.Ctx, w.scope, none.ID, move(domain.RemedyProposed, domain.RemedyApproved,
		domain.UserActor(ada), w.h.Now()), time.Time{}, "")
	require.Error(t, err)
	require.NoError(t, repo.Transition(w.h.Ctx, w.scope, none.ID, move(domain.RemedyProposed, domain.RemedyExpired,
		domain.SystemActor(), w.h.Now().Add(2*time.Hour)), time.Time{}, ""))

	ids, err := repo.PastDeadline(w.h.Ctx, w.scope, w.h.Now().Add(2*time.Hour), 10)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{r.ID}, ids, "the proposed one is past its deadline; the expired one is recorded")
}

// TestAClaimIsCommittedBeforeTheCallAndAnUnansweredOneIsListed — the executor's claim
// (`approved → executing`) stamps `executing_at`; until its answer is recorded it is listed
// as overdue once the caller's deadline passes; the answer is kept on the row; and a failed
// Remedy is frozen — no move, and certainly not back to `approved`.
func TestAClaimIsCommittedBeforeTheCallAndAnUnansweredOneIsListed(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	repo := repository.NewRemedyRepository(w.h.Pool)
	r := w.remedy(t, repo, true)
	grace := w.user(t, "Grace Hopper")
	at := w.h.Now().Add(time.Minute)
	require.NoError(t, repo.Transition(w.h.Ctx, w.scope, r.ID, move(domain.RemedyProposed, domain.RemedyApproved,
		domain.UserActor(grace), at), at.Add(time.Hour), ""))
	claim := at.Add(time.Second)
	require.NoError(t, repo.Transition(w.h.Ctx, w.scope, r.ID, move(domain.RemedyApproved, domain.RemedyExecuting,
		domain.SystemActor(), claim), time.Time{}, ""))

	ids, err := repo.OutcomeOverdue(w.h.Ctx, w.scope, claim.Add(-time.Second), 10)
	require.NoError(t, err)
	require.Empty(t, ids, "a claim inside its deadline is not overdue")
	ids, err = repo.OutcomeOverdue(w.h.Ctx, w.scope, claim.Add(domain.RemedyOutcomeDeadline), 10)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{r.ID}, ids)

	failed := move(domain.RemedyExecuting, domain.RemedyFailed, domain.SystemActor(), claim.Add(time.Minute))
	failed.Failure, failed.Detail = domain.FailToolError, "the write Tool answered that the call failed"
	require.NoError(t, repo.Transition(w.h.Ctx, w.scope, r.ID, failed, time.Time{}, `deployments.apps "api" is forbidden`))
	got, err := repo.GetRemedy(w.h.Ctx, w.scope, r.ID)
	require.NoError(t, err)
	require.Equal(t, domain.RemedyFailed, got.State)
	require.Equal(t, domain.FailToolError, got.Failure)
	require.Contains(t, got.Result, "forbidden")
	require.False(t, got.ExecutingAt.IsZero())

	// ⛔ A failed Remedy is never retried: nothing moves it, back to approved least of all.
	_, err = w.h.Pool.Exec(w.h.Ctx, `UPDATE remedies SET state = 'approved', ended_at = NULL, failure_reason = NULL WHERE id = $1`, r.ID)
	require.Error(t, err)
	ids, err = repo.OutcomeOverdue(w.h.Ctx, w.scope, claim.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Empty(t, ids)
}
