package service

// AN APPROVED REMEDY IS EXECUTED AT MOST ONCE WITH THE ARGUMENTS APPROVED, AND A FAILURE STAYS
// FAILED (ADR 0054 §5, §6; git-bug 4148256).
//
// "Execution is a separate step through the ToolServer with the approved arguments; never
// retried automatically." ExecuteRemedy is the `remedies.execute` job, enqueued by the
// approval that completed a Remedy, in that transaction.
//
// ⭐⭐ AT MOST ONCE, BY A CLAIM COMMITTED BEFORE THE CALL. The executor locks the Remedy and,
// only if it is still `approved`, moves it to `executing` and COMMITS — then, and only then,
// calls the write Tool. So:
//
//   - a redelivered job, or a second worker, finds it `executing` or ended and does nothing;
//   - a worker that dies between the claim and the record leaves it `executing`, never
//     `approved`, and `remedies.sweep` records it `failed` with `outcome_unknown` once
//     domain.RemedyOutcomeDeadline passes — the change may or may not have been made, and it
//     is said so;
//   - a claim whose commit was lost to the network is the same case: it was not sent, and it
//     is recorded `outcome_unknown` rather than ever sent twice.
//
// ⭐⭐ BEFORE THE CLAIM, EVERYTHING IS CHECKED AGAIN, AND A REFUSAL IS A FAILURE THAT SENT
// NOTHING. The window (an approved Remedy past it is `expired`, by `system`); the Tool,
// against the configuration NOW (`tool_unavailable` — never executed against nothing); the
// arguments against the hash approved (`arguments_changed`); and the approvers against the
// grant NOW, counting only DIFFERENT holders whose approval named these arguments
// (`approvals_withdrawn`). Each is recorded and declared like any transition.
//
// ⭐ THE ARGUMENTS SENT ARE THE BYTES STORED. The Remedy's `arguments` — the compact JSON the
// Investigator proposed and the approvers approved by hash — are handed to the MCP client as
// they are, never decoded and re-encoded.
//
// ⭐ WHAT CAME BACK IS KEPT REDACTED AND CAPPED. The org's ingest redaction rules, the
// ToolServer's own token scrubbed out, then cut at domain.MaxRemedyResult — the rules a Tool
// result in a Step follows.
//
// ⛔ NOTHING RETRIES A FAILED REMEDY. `failed` is terminal and frozen (`remedies_frozen`); a
// retry is a new Remedy and a new approval.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// recordTimeout bounds recording what the write Tool answered, on a context its own job's
// cancellation cannot reach: a call that was made must be recorded if it can be.
const recordTimeout = 10 * time.Second

// enqueueExecution enqueues `remedies.execute` for a Remedy that just became `approved`, in
// the approval's transaction: approved and enqueued, or neither.
func (s *Service) enqueueExecution(ctx context.Context, scope db.TenantScope, remedyID uuid.UUID) error {
	_, err := s.queue.Enqueue(ctx, jobs.RemediesExecuteArgs{OrgID: scope.OrgID(), RemedyID: remedyID})
	return err
}

// claimed is what a claim hands the call: the Remedy as claimed and the ToolServer to reach.
type claimed struct {
	remedy domain.Remedy
	server domain.ToolServerConfig
}

// ExecuteRemedy is the `remedies.execute` job: claim one approved Remedy, call its write Tool
// with the approved arguments exactly, and record what came back. It returns an error only
// when the record could not be read or written — before the claim a retry may claim; after
// it, a retry finds the Remedy claimed and does nothing.
func (s *Service) ExecuteRemedy(ctx context.Context, scope db.TenantScope, remedyID uuid.UUID) error {
	if err := db.RequireScope(scope); err != nil {
		return err
	}
	r, err := s.remedies.GetRemedy(ctx, scope, remedyID)
	if errs.IsKind(err, errs.KindNotFound) {
		return nil // gone with its Investigation or its org: nothing to do.
	}
	if err != nil {
		return err
	}
	if r.State != domain.RemedyApproved {
		return nil // ⛔ claimed, ended, or never approved: at most once.
	}
	// Read before the claim, so a database that cannot answer is a retry that has sent
	// nothing. ⛔ No answer is ever kept unredacted: no rules read is no call.
	redact, err := s.redaction.ToolResultRedactor(ctx, scope)
	if err != nil {
		return err
	}

	var c *claimed
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		got, err := s.claimRemedy(ctx, scope, remedyID)
		c = got
		return err
	})
	if err != nil || c == nil {
		return err
	}

	// ⛔ FROM HERE THE CALL IS MADE AT MOST ONCE. The claim is committed; nothing below
	// returns the Remedy to `approved`.
	outcome := s.callWriteTool(ctx, scope, c, redact)
	rec, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	return s.tx.InTx(rec, func(ctx context.Context) error {
		cur, err := s.remedies.LockRemedy(ctx, scope, remedyID)
		if err != nil {
			return err
		}
		if cur.State != domain.RemedyExecuting {
			return nil // the sweep recorded it meanwhile; its record stands.
		}
		_, err = s.moveRemedy(ctx, scope, cur, outcome.to, domain.SystemActor(), s.now(), outcome.failure,
			outcome.detail, time.Time{}, outcome.result)
		return err
	})
}

// claimRemedy re-checks a Remedy under its row lock and claims it — or records why it will
// not be sent and returns nil. Inside the caller's transaction.
func (s *Service) claimRemedy(ctx context.Context, scope db.TenantScope, remedyID uuid.UUID) (*claimed, error) {
	r, err := s.remedies.LockRemedy(ctx, scope, remedyID)
	if err != nil {
		return nil, err
	}
	if r.State != domain.RemedyApproved {
		return nil, nil
	}
	now := s.now()
	refuse := func(to domain.RemedyState, f domain.RemedyFailure, detail string) (*claimed, error) {
		_, err := s.moveRemedy(ctx, scope, r, to, domain.SystemActor(), now, f, detail, time.Time{}, "")
		return nil, err
	}
	if r.StateAt(now) == domain.RemedyExpired {
		return refuse(domain.RemedyExpired, "", "it was approved, and not executed within the approval window")
	}
	cfg, err := s.toolServers.Get(ctx, scope, r.Tool.ToolServerID)
	found := err == nil
	if err != nil && !errs.IsKind(err, errs.KindNotFound) {
		return nil, err
	}
	var listed []domain.DiscoveredTool
	if found {
		if listed, err = s.toolServers.Tools(ctx, scope, cfg.ID); err != nil {
			return nil, err
		}
	}
	if why := domain.RemedyBinding(r.Tool, cfg, found, listed); why != "" {
		return refuse(domain.RemedyFailed, domain.FailToolUnavailable, why+"; nothing was sent")
	}
	if domain.HashArguments(r.Arguments) != r.ArgumentsSHA256 {
		return refuse(domain.RemedyFailed, domain.FailArgumentsChanged,
			"its arguments no longer hash to what was approved; nothing was sent")
	}
	holders, err := s.standingApprovers(ctx, scope, r)
	if err != nil {
		return nil, err
	}
	if holders < r.RequiredApprovals {
		return refuse(domain.RemedyFailed, domain.FailApprovalsWithdrawn, fmt.Sprintf(
			"%d of the %d different approvers it needs still hold the grant on %s and approved these arguments; "+
				"nothing was sent", holders, r.RequiredApprovals, r.Tool.ToolServerName))
	}
	claim, err := s.moveRemedy(ctx, scope, r, domain.RemedyExecuting, domain.SystemActor(), now, "", "", time.Time{}, "")
	if err != nil {
		return nil, err
	}
	return &claimed{remedy: claim, server: cfg}, nil
}

// standingApprovers counts the DIFFERENT people whose approval named this Remedy's arguments
// and who still hold a counting grant on its ToolServer now. A grant revoked, or a user
// disabled, since they approved does not count.
func (s *Service) standingApprovers(ctx context.Context, scope db.TenantScope, r domain.Remedy) (int, error) {
	seen := map[uuid.UUID]bool{}
	for _, a := range r.Approvals {
		if a.UserID == uuid.Nil || seen[a.UserID] || a.ArgumentsSHA256 != r.ArgumentsSHA256 {
			continue
		}
		err := s.approvers.RequireRemedyApprover(ctx, scope, r.Tool.ToolServerID, a.UserID)
		switch {
		case err == nil:
			seen[a.UserID] = true
		case errs.IsKind(err, errs.KindForbidden), errs.IsKind(err, errs.KindNotFound):
			// no longer a holder: not counted
		default:
			return 0, err
		}
	}
	return len(seen), nil
}

// callOutcome is how one call ended, as it is recorded.
type callOutcome struct {
	to      domain.RemedyState
	failure domain.RemedyFailure
	detail  string
	result  string
}

// callWriteTool reaches the ToolServer and calls the write Tool once, with the stored
// arguments exactly, inside the ToolServer's own per-call timeout. It never fails: every way
// the call goes is an outcome to record.
func (s *Service) callWriteTool(ctx context.Context, scope db.TenantScope, c *claimed, redact domain.ResultRedactor) callOutcome {
	r, cfg := c.remedy, c.server
	callCtx, cancel := context.WithTimeout(ctx, cfg.Limits.Timeout)
	defer cancel()

	token, err := s.toolServerToken(callCtx, scope, cfg)
	if err != nil {
		// oto could not unseal the token: no session was opened, so nothing was sent.
		return callOutcome{to: domain.RemedyFailed, failure: domain.FailToolUnavailable,
			detail: "oto could not open the ToolServer's token, so nothing was sent: " + safeMessage(err)}
	}
	client, err := s.toolDialer.Connect(callCtx, cfg, token)
	if err != nil {
		// No session, so no call: the write Tool was never asked.
		return callOutcome{to: domain.RemedyFailed, failure: domain.FailToolUnavailable,
			detail: "the ToolServer " + cfg.Name + " could not be reached, so nothing was sent: " +
				scrubSecret(safeMessage(err), token)}
	}
	defer func() { _ = client.Close() }()

	res, err := client.CallTool(callCtx, r.Tool.Tool, json.RawMessage(r.Arguments))
	if err != nil {
		// ⚠️ The call may have reached the ToolServer: a timeout or a broken connection says
		// nothing about whether the change was made, and the record must not guess.
		why := scrubSecret(safeMessage(err), token)
		if callCtx.Err() != nil {
			why = fmt.Sprintf("the write Tool gave no answer within %s", cfg.Limits.Timeout)
		}
		return callOutcome{to: domain.RemedyFailed, failure: domain.FailOutcomeUnknown,
			detail: why + "; the change may or may not have been made, and it is not sent again"}
	}
	result := keptResult(scrubSecret(res.Text, token), redact)
	if res.IsError {
		return callOutcome{to: domain.RemedyFailed, failure: domain.FailToolError,
			detail: "the write Tool answered that the call failed", result: result}
	}
	return callOutcome{to: domain.RemedyExecuted, result: result}
}

// keptResult is a write Tool's answer as it is stored: redacted with the org's rules, then cut
// at MaxRemedyResult with a note saying so. "" is kept as a sentence, so an executed Remedy
// always says what came back.
func keptResult(text string, redact domain.ResultRedactor) string {
	out, redacted := redact.Redact(text)
	note := ""
	if n := len([]rune(out)); n > domain.MaxRemedyResult-200 {
		out = clipText(out, domain.MaxRemedyResult-200)
		note = fmt.Sprintf("\n[truncated: %d of %d characters]", domain.MaxRemedyResult-200, n)
	}
	if redacted > 0 {
		note += fmt.Sprintf("\n[redacted: %d value(s) matched this org's redaction rules]", redacted)
	}
	if strings.TrimSpace(out) == "" {
		out = "[the write Tool answered with no text]"
	}
	return out + note
}

// FailOverdueRemedies is the second half of the `remedies.sweep` job: every Remedy claimed for
// execution longer than RemedyOutcomeDeadline ago with no answer recorded is moved to `failed`
// with `outcome_unknown`, by `system`, and declared. ⛔ IT SENDS NOTHING AND NOTHING SENDS IT
// AGAIN: the change may or may not have been made, and the record says exactly that.
func (s *Service) FailOverdueRemedies(ctx context.Context, scope db.TenantScope) (int, error) {
	if err := db.RequireScope(scope); err != nil {
		return 0, err
	}
	ids, err := s.remedies.OutcomeOverdue(ctx, scope, s.now().Add(-domain.RemedyOutcomeDeadline), remedySweepBatch)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, rid := range ids {
		err := s.tx.InTx(ctx, func(ctx context.Context) error {
			r, err := s.remedies.LockRemedy(ctx, scope, rid)
			if err != nil {
				return err
			}
			now := s.now()
			if !r.OutcomeOverdue(now) {
				return nil // its worker recorded the answer meanwhile.
			}
			if _, err := s.moveRemedy(ctx, scope, r, domain.RemedyFailed, domain.SystemActor(), now,
				domain.FailOutcomeUnknown, fmt.Sprintf(
					"oto claimed it for execution at %s and no answer was recorded; the change may or may not "+
						"have been made, and it is not sent again", r.ExecutingAt.UTC().Format(time.RFC3339)),
				time.Time{}, ""); err != nil {
				return err
			}
			n++
			return nil
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
