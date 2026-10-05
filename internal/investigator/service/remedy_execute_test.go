package service

// git-bug 4148256's "Done when", for execution, against the real MCP adapter and the official
// SDK's own server in-process over TLS (ADR 0054 §5, §6): a Remedy runs only after two
// different holders approve it; the arguments executed equal the arguments approved, byte for
// byte; it is sent at most once, whatever the job does; a Remedy whose Tool was removed after
// approval, whose approvers lost the grant, or whose window passed is not sent and says why; a
// failed Remedy stays failed and is never retried; a claim with no answer is recorded
// `outcome_unknown` and never sent again; and each transition is declared to the Incident. No
// Docker.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/jobs"
	"github.com/thulasiram/oto/test/toolserverfake"
)

// executeJobs counts the `remedies.execute` jobs enqueued for one Remedy.
func executeJobs(r *rig, remedyID uuid.UUID) int {
	r.queue.mu.Lock()
	defer r.queue.mu.Unlock()
	n := 0
	for _, j := range r.queue.jobs {
		if a, ok := j.(jobs.RemediesExecuteArgs); ok && a.RemedyID == remedyID {
			n++
		}
	}
	return n
}

// recorder is a write Tool that records the exact arguments each call carried, and answers
// what the test says.
type recorder struct {
	mu   sync.Mutex
	sent []json.RawMessage
	auth []string
}

func (rc *recorder) tool(answer string, isError bool) toolserverfake.Tool {
	return toolserverfake.Tool{Name: "rollout_restart", Description: "restarts a Deployment",
		Handle: func(_ context.Context, c toolserverfake.Call) (string, bool) {
			rc.mu.Lock()
			defer rc.mu.Unlock()
			rc.sent = append(rc.sent, append(json.RawMessage(nil), c.Arguments...))
			rc.auth = append(rc.auth, c.Authorization)
			return strings.ReplaceAll(answer, "$AUTH", c.Authorization), isError
		}}
}

func (rc *recorder) calls() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.sent)
}

// approvedRemedy configures the write ToolServer, proposes one Remedy on a Case an Incident
// holds, and has two different grant holders approve it. It returns the approved Remedy.
func (r *rig) approvedRemedy(t *testing.T, tool toolserverfake.Tool) (domain.Remedy, domain.ToolServerConfig, domain.IncidentSubject, [2]domain.Requester) {
	t.Helper()
	_, cfg := r.withWriteServer(t, tool)
	inv, c := r.remedyInvestigator(t)
	incident := domain.IncidentSubject{IncidentID: uuid.New(), Number: 7}
	r.incidents.incidents[incident.IncidentID] = incident
	r.incidents.holding[c.CaseID] = incident.IncidentID
	_, list := r.propose(t, inv, c, restartProposal(proposedArgs))
	ada, grace := r.grant(cfg.ID, "Ada Lovelace"), r.grant(cfg.ID, "Grace Hopper")
	ctx := context.Background()
	if _, err := r.svc.ApproveRemedy(ctx, r.scope, list[0].ID, ada, list[0].ArgumentsSHA256); err != nil {
		t.Fatal(err)
	}
	got, err := r.svc.ApproveRemedy(ctx, r.scope, list[0].ID, grace, list[0].ArgumentsSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.RemedyApproved || executeJobs(r, got.ID) != 1 {
		t.Fatalf("approved = %+v, %d jobs", got, executeJobs(r, got.ID))
	}
	return got, cfg, incident, [2]domain.Requester{ada, grace}
}

func (r *rig) remedyNow(t *testing.T, id uuid.UUID) domain.Remedy {
	t.Helper()
	got, err := r.svc.GetRemedy(context.Background(), r.scope, id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func movesOf(rem domain.Remedy) string {
	out := []string{}
	for _, tr := range rem.Transitions {
		out = append(out, string(tr.To))
	}
	return strings.Join(out, ">")
}

// TestAnApprovedRemedyRunsOnceWithTheArgumentsApproved — the executor sends exactly the
// bytes approved, records what came back with the ToolServer's token scrubbed, and declares
// `remedy_executed` to the Incident; a second delivery of its job sends nothing.
func TestAnApprovedRemedyRunsOnceWithTheArgumentsApproved(t *testing.T) {
	r := newRig(t)
	rc := &recorder{}
	rem, _, incident, _ := r.approvedRemedy(t, rc.tool("deployment.apps/api restarted (auth was $AUTH)", false))
	ctx := context.Background()

	if err := r.svc.ExecuteRemedy(ctx, r.scope, rem.ID); err != nil {
		t.Fatal(err)
	}
	if rc.calls() != 1 {
		t.Fatalf("the write Tool was called %d times, want 1", rc.calls())
	}
	// ⭐ THE ARGUMENTS EXECUTED ARE THE ARGUMENTS APPROVED: the stored bytes, whose hash
	// both approvals named — key order and the 20-digit number untouched.
	var sent bytes.Buffer
	if err := json.Compact(&sent, rc.sent[0]); err != nil {
		t.Fatal(err)
	}
	if sent.String() != compactArgs || domain.HashArguments(sent.String()) != rem.ArgumentsSHA256 {
		t.Fatalf("sent %s, approved %s", sent.String(), compactArgs)
	}
	got := r.remedyNow(t, rem.ID)
	if got.State != domain.RemedyExecuted || movesOf(got) != "proposed>approved>approved>executing>executed" &&
		movesOf(got) != "proposed>approved>executing>executed" {
		t.Fatalf("executed = %s via %s", got.State, movesOf(got))
	}
	if !strings.Contains(got.Result, "restarted") || strings.Contains(got.Result, writeServerToken) {
		t.Fatalf("result = %q", got.Result)
	}
	last := r.remedyFacts.facts[len(r.remedyFacts.facts)-1]
	if last.fact.Transition.To != domain.RemedyExecuted || last.incident != incident.IncidentID {
		t.Fatalf("the execution was declared as %+v", last)
	}
	for _, d := range r.remedyFacts.facts {
		if d.fact.Transition.To == domain.RemedyExecuting {
			t.Fatal("the executor's claim was declared; it is bookkeeping between two facts")
		}
	}

	// ⛔ AT MOST ONCE: the job delivered again finds it executed and sends nothing.
	if err := r.svc.ExecuteRemedy(ctx, r.scope, rem.ID); err != nil {
		t.Fatal(err)
	}
	if rc.calls() != 1 {
		t.Fatalf("a redelivered job called the write Tool again (%d calls)", rc.calls())
	}
}

// TestARemedyIsSentAtMostOnceUnderAJobRetry — the worker calls the Tool and dies before it can
// record the answer: the job fails and River retries it. The retry finds the Remedy claimed and
// sends nothing; the sweep records it `failed` with `outcome_unknown` once the deadline passes,
// declares it, and nothing sends it again.
func TestARemedyIsSentAtMostOnceUnderAJobRetry(t *testing.T) {
	r := newRig(t)
	rc := &recorder{}
	rem, _, _, _ := r.approvedRemedy(t, rc.tool("restarted", false))
	ctx := context.Background()

	r.remedies.failMove = domain.RemedyExecuted
	if err := r.svc.ExecuteRemedy(ctx, r.scope, rem.ID); err == nil {
		t.Fatal("a record that could not be written was not returned for a retry")
	}
	if rc.calls() != 1 || r.remedyNow(t, rem.ID).State != domain.RemedyExecuting {
		t.Fatalf("after the failed record: %d calls, state %s", rc.calls(), r.remedyNow(t, rem.ID).State)
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := r.svc.ExecuteRemedy(ctx, r.scope, rem.ID); err != nil {
			t.Fatal(err)
		}
	}
	if rc.calls() != 1 {
		t.Fatalf("a retried job sent the Remedy again: %d calls", rc.calls())
	}

	// Inside the deadline the sweep leaves it: its worker may still record the answer.
	if n, _ := r.svc.FailOverdueRemedies(ctx, r.scope); n != 0 {
		t.Fatalf("the sweep failed a claim %d time(s) inside its deadline", n)
	}
	r.clock.Advance(domain.RemedyOutcomeDeadline + time.Second)
	if n, err := r.svc.FailOverdueRemedies(ctx, r.scope); err != nil || n != 1 {
		t.Fatalf("swept %d, err %v", n, err)
	}
	got := r.remedyNow(t, rem.ID)
	if got.State != domain.RemedyFailed || got.Failure != domain.FailOutcomeUnknown ||
		!strings.Contains(got.Detail, "may or may not have been made") {
		t.Fatalf("an unanswered claim = %s %s %q", got.State, got.Failure, got.Detail)
	}
	if last := r.remedyFacts.facts[len(r.remedyFacts.facts)-1]; last.fact.Transition.To != domain.RemedyFailed {
		t.Fatalf("the failure was not declared: %+v", last)
	}
	if err := r.svc.ExecuteRemedy(ctx, r.scope, rem.ID); err != nil || rc.calls() != 1 {
		t.Fatalf("a failed Remedy was sent again: %d calls, %v", rc.calls(), err)
	}
}

// TestAFailedRemedyStaysFailedAndIsNeverRetried — the write Tool reports a failure: the Remedy
// is `failed` with `tool_error` and the Tool's own words, and nothing — the job again, an
// approval, a decline — moves it or sends it again.
func TestAFailedRemedyStaysFailedAndIsNeverRetried(t *testing.T) {
	r := newRig(t)
	rc := &recorder{}
	rem, cfg, _, approvers := r.approvedRemedy(t, rc.tool(`deployments.apps "api" is forbidden`, true))
	ctx := context.Background()

	if err := r.svc.ExecuteRemedy(ctx, r.scope, rem.ID); err != nil {
		t.Fatal(err)
	}
	got := r.remedyNow(t, rem.ID)
	if got.State != domain.RemedyFailed || got.Failure != domain.FailToolError || !strings.Contains(got.Result, "forbidden") {
		t.Fatalf("failed = %+v", got)
	}
	for i := 0; i < 3; i++ {
		if err := r.svc.ExecuteRemedy(ctx, r.scope, rem.ID); err != nil {
			t.Fatal(err)
		}
	}
	third := r.grant(cfg.ID, "Barbara Liskov")
	_, err := r.svc.ApproveRemedy(ctx, r.scope, rem.ID, third, rem.ArgumentsSHA256)
	wantCode(t, err, "remedy_not_proposed")
	_, err = r.svc.DeclineRemedy(ctx, r.scope, rem.ID, approvers[0])
	wantCode(t, err, "remedy_not_open")
	r.clock.Advance(2 * domain.RemedyOutcomeDeadline)
	if n, _ := r.svc.ExpireRemedies(ctx, r.scope); n != 0 {
		t.Fatalf("the sweep moved a failed Remedy")
	}
	if rc.calls() != 1 || r.remedyNow(t, rem.ID).State != domain.RemedyFailed {
		t.Fatalf("a failed Remedy was retried: %d calls, now %s", rc.calls(), r.remedyNow(t, rem.ID).State)
	}
	if executeJobs(r, rem.ID) != 1 {
		t.Fatalf("%d execute jobs enqueued; a failure enqueues none", executeJobs(r, rem.ID))
	}
}

// TestARemedyWhoseToolWasRemovedAfterApprovalIsNotExecuted — the configuration is asked again
// at execution: a ToolServer removed, or no longer `write`, is a failure that sent nothing.
func TestARemedyWhoseToolWasRemovedAfterApprovalIsNotExecuted(t *testing.T) {
	for name, remove := range map[string]func(r *rig, id uuid.UUID){
		"re-declared read": func(r *rig, id uuid.UUID) { r.toolServers.setAccess(id, domain.AccessRead) },
		"removed": func(r *rig, id uuid.UUID) {
			r.toolServers.mu.Lock()
			delete(r.toolServers.rows, id)
			r.toolServers.mu.Unlock()
		},
		"no longer listing the Tool": func(r *rig, id uuid.UUID) {
			r.toolServers.mu.Lock()
			r.toolServers.tools[id] = nil
			r.toolServers.mu.Unlock()
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			rc := &recorder{}
			rem, cfg, _, _ := r.approvedRemedy(t, rc.tool("restarted", false))
			remove(r, cfg.ID)
			if err := r.svc.ExecuteRemedy(context.Background(), r.scope, rem.ID); err != nil {
				t.Fatal(err)
			}
			got := r.remedyNow(t, rem.ID)
			if got.State != domain.RemedyFailed || got.Failure != domain.FailToolUnavailable ||
				!strings.Contains(got.Detail, "nothing was sent") || got.ExecutingAt != (time.Time{}) {
				t.Fatalf("got %+v", got)
			}
			if rc.calls() != 0 {
				t.Fatalf("a Remedy whose Tool was gone was executed against something: %d calls", rc.calls())
			}
		})
	}
}

// TestARemedyWhoseApproversLostTheGrantIsNotExecuted — the approvals are asked again at
// execution: a revoked grant no longer counts, and one approver left is not two.
func TestARemedyWhoseApproversLostTheGrantIsNotExecuted(t *testing.T) {
	r := newRig(t)
	rc := &recorder{}
	rem, cfg, _, approvers := r.approvedRemedy(t, rc.tool("restarted", false))
	r.approvers.mu.Lock()
	kept := r.approvers.rows[cfg.ID][:0]
	for _, a := range r.approvers.rows[cfg.ID] {
		if a.UserID != approvers[1].UserID {
			kept = append(kept, a)
		}
	}
	r.approvers.rows[cfg.ID] = kept
	r.approvers.mu.Unlock()

	if err := r.svc.ExecuteRemedy(context.Background(), r.scope, rem.ID); err != nil {
		t.Fatal(err)
	}
	got := r.remedyNow(t, rem.ID)
	if got.State != domain.RemedyFailed || got.Failure != domain.FailApprovalsWithdrawn || rc.calls() != 0 {
		t.Fatalf("got %s %s with %d calls", got.State, got.Failure, rc.calls())
	}
}

// TestAnApprovedRemedyPastItsWindowExpiresInsteadOfRunning — an approval is good for the
// window and no longer: a job that runs after it records `expired` and sends nothing.
func TestAnApprovedRemedyPastItsWindowExpiresInsteadOfRunning(t *testing.T) {
	r := newRig(t)
	rc := &recorder{}
	rem, _, _, _ := r.approvedRemedy(t, rc.tool("restarted", false))
	r.clock.Advance(domain.DefaultRemedyApprovalWindow + time.Second)
	if err := r.svc.ExecuteRemedy(context.Background(), r.scope, rem.ID); err != nil {
		t.Fatal(err)
	}
	got := r.remedyNow(t, rem.ID)
	last := got.Transitions[len(got.Transitions)-1]
	if got.State != domain.RemedyExpired || last.Actor.Kind != domain.ActorSystem || rc.calls() != 0 {
		t.Fatalf("got %s by %s with %d calls", got.State, last.Actor.Kind, rc.calls())
	}
}

// TestAnUnreachableToolServerSendsNothingAndSaysSo — no session, no call: `tool_unavailable`,
// not `outcome_unknown`, because the write Tool was never asked.
func TestAnUnreachableToolServerSendsNothingAndSaysSo(t *testing.T) {
	r := newRig(t)
	rc := &recorder{}
	rem, _, _, _ := r.approvedRemedy(t, rc.tool("restarted", false))
	r.toolDialer.mu.Lock()
	r.toolDialer.inner = nil
	r.toolDialer.mu.Unlock()
	if err := r.svc.ExecuteRemedy(context.Background(), r.scope, rem.ID); err != nil {
		t.Fatal(err)
	}
	got := r.remedyNow(t, rem.ID)
	if got.State != domain.RemedyFailed || got.Failure != domain.FailToolUnavailable ||
		!strings.Contains(got.Detail, "nothing was sent") || rc.calls() != 0 {
		t.Fatalf("got %s %s %q", got.State, got.Failure, got.Detail)
	}
}

// TestTheExecuteJobOutlastsTheLongestCall — the job's timeout is a copy of the per-call bound
// (platform may not import the investigator domain); this is where the two are held equal in
// spirit: the call's own timeout must be what stops a call, with room to claim and record.
func TestTheExecuteJobOutlastsTheLongestCall(t *testing.T) {
	longest := domain.MaxCallTimeoutSeconds * time.Second
	if jobs.RemedyExecuteJobTimeout < longest+time.Minute {
		t.Fatalf("remedies.execute times out at %s; the longest call is %s", jobs.RemedyExecuteJobTimeout, longest)
	}
	if domain.RemedyOutcomeDeadline <= jobs.RemedyExecuteJobTimeout {
		t.Fatalf("the sweep would fail a claim (%s) its job may still be recording (%s)",
			domain.RemedyOutcomeDeadline, jobs.RemedyExecuteJobTimeout)
	}
}

// TestAWriteToolAnswerWithANulIsStillRecorded — review A4 on the executor: a call that was
// MADE must be recorded, and Postgres refuses U+0000, so the answer is cleaned first.
func TestAWriteToolAnswerWithANulIsStillRecorded(t *testing.T) {
	r := newRig(t)
	rc := &recorder{}
	rem, _, _, _ := r.approvedRemedy(t, rc.tool("deployment.apps/api\x00 restarted", false))
	if err := r.svc.ExecuteRemedy(context.Background(), r.scope, rem.ID); err != nil {
		t.Fatal(err)
	}
	got := r.remedyNow(t, rem.ID)
	if got.State != domain.RemedyExecuted || strings.Contains(got.Result, "\x00") || !strings.Contains(got.Result, "restarted") {
		t.Fatalf("executed = %s, result %q", got.State, got.Result)
	}
}
