package service

// git-bug 4148256's "Done when", for proposal, approval, decline and expiry, against the
// scripted model, the real MCP adapter over an in-process ToolServer, and in-memory ports
// that keep 00104's rules (ADR 0054 §1, §2, §4, §5): an Investigator proposes a Remedy and
// never holds the write Tool; a Remedy with no Tool says so, is refused on approve, is
// declinable and expires; a Remedy runs only after two DIFFERENT holders of the grant on its
// ToolServer approve it, the same one twice counts once, and a non-holder is refused; a Remedy
// whose Tool was removed after the proposal is unapprovable; the arguments approved are the
// arguments proposed, byte for byte; expiry is recorded by `system`; and every transition is
// declared to the Incident as a fact. No Docker.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/investigator/toolservers/mcpclient"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/modelfake"
	"github.com/thulasiram/oto/test/toolserverfake"
)

// ------------------------------------------------------------------- fakes

// memRemedies is the Remedy tables, keeping 00104's rules where a test depends on them: a
// Remedy is inserted proposed; a terminal one is frozen; every move is one of the legal
// transitions from the state the caller read; one person approves once.
type memRemedies struct {
	mu    sync.Mutex
	rows  map[uuid.UUID]domain.Remedy
	order []uuid.UUID
	// failMove, when set, fails the next transition into that state — a worker dying
	// between the call and its record, for the at-most-once test — failMoveTimes times in a
	// row (once when 0): the executor tries its record more than once (C6).
	failMove      domain.RemedyState
	failMoveTimes int
}

func newMemRemedies() *memRemedies { return &memRemedies{rows: map[uuid.UUID]domain.Remedy{}} }

func cloneRemedy(r domain.Remedy) domain.Remedy {
	r.Approvals = append([]domain.RemedyApproval{}, r.Approvals...)
	r.Transitions = append([]domain.RemedyTransition{}, r.Transitions...)
	return r
}

func (m *memRemedies) InsertRemedy(_ context.Context, s db.TenantScope, r domain.Remedy, proposal domain.RemedyTransition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.State != domain.RemedyProposed || proposal.To != domain.RemedyProposed || r.OrgID != s.OrgID() {
		return errs.New(errs.KindInternal, "remedy_insert_state", "a Remedy is inserted proposed")
	}
	if r.Tool.Named() && r.ArgumentsSHA256 != domain.HashArguments(r.Arguments) {
		return errs.New(errs.KindInternal, "remedies_arguments_hash_ck", "the hash is not the arguments'")
	}
	r.Approvals, r.Transitions = []domain.RemedyApproval{}, []domain.RemedyTransition{proposal}
	m.rows[r.ID] = r
	m.order = append(m.order, r.ID)
	return nil
}

func (m *memRemedies) ListRemedies(_ context.Context, s db.TenantScope, investigationID uuid.UUID) ([]domain.Remedy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Remedy{}
	for _, id := range m.order {
		if r := m.rows[id]; r.OrgID == s.OrgID() && r.InvestigationID == investigationID {
			out = append(out, cloneRemedy(r))
		}
	}
	return out, nil
}

func (m *memRemedies) GetRemedy(_ context.Context, s db.TenantScope, id uuid.UUID) (domain.Remedy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok || r.OrgID != s.OrgID() {
		return domain.Remedy{}, errs.NotFound("remedy_not_found", "no such Remedy")
	}
	return cloneRemedy(r), nil
}

func (m *memRemedies) LockRemedy(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Remedy, error) {
	return m.GetRemedy(ctx, s, id)
}

func (m *memRemedies) AddApproval(_ context.Context, s db.TenantScope, remedyID uuid.UUID, a domain.RemedyApproval) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[remedyID]
	for _, have := range r.Approvals {
		if have.UserID == a.UserID {
			return domain.RemedyAlreadyApproved(have) // remedy_approvals_user_uniq
		}
	}
	r.Approvals = append(r.Approvals, a)
	m.rows[remedyID] = r
	return nil
}

var legalRemedyMoves = map[domain.RemedyState][]domain.RemedyState{
	domain.RemedyProposed:  {domain.RemedyApproved, domain.RemedyDeclined, domain.RemedyExpired},
	domain.RemedyApproved:  {domain.RemedyExecuting, domain.RemedyDeclined, domain.RemedyExpired, domain.RemedyFailed},
	domain.RemedyExecuting: {domain.RemedyExecuted, domain.RemedyFailed},
}

func (m *memRemedies) Transition(
	_ context.Context, s db.TenantScope, remedyID uuid.UUID, t domain.RemedyTransition, expiresAt time.Time, result string,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[remedyID]
	if !ok || r.OrgID != s.OrgID() {
		return errs.NotFound("remedy_not_found", "no such Remedy")
	}
	if r.State.Terminal() {
		return errs.New(errs.KindInternal, "remedies_frozen", "a Remedy that ended is never rewritten")
	}
	if m.failMove != "" && m.failMove == t.To {
		if m.failMoveTimes--; m.failMoveTimes <= 0 {
			m.failMove = ""
		}
		return errs.New(errs.KindInternal, "investigator_query_failed", "the worker died before it could record this")
	}
	if r.State != t.From {
		return errs.Conflict("remedy_moved", "this Remedy moved meanwhile")
	}
	if !slices.Contains(legalRemedyMoves[t.From], t.To) {
		return errs.New(errs.KindInternal, "remedies_frozen", "not a transition")
	}
	if !r.Tool.Named() && t.To != domain.RemedyDeclined && t.To != domain.RemedyExpired {
		return errs.New(errs.KindInternal, "remedies_no_tool_ck", "a Remedy with no Tool cannot be approved")
	}
	r.State, r.Failure = t.To, t.Failure
	switch t.To {
	case domain.RemedyApproved:
		r.ApprovedAt = t.At
	case domain.RemedyExecuting:
		r.ExecutingAt = t.At
	}
	if t.To.Terminal() {
		r.EndedAt = t.At
	}
	if !expiresAt.IsZero() {
		r.ExpiresAt = expiresAt
	}
	if t.Detail != "" {
		r.Detail = t.Detail
	}
	if result != "" {
		r.Result = result
	}
	r.Transitions = append(r.Transitions, t)
	m.rows[remedyID] = r
	return nil
}

func (m *memRemedies) PastDeadline(_ context.Context, s db.TenantScope, now time.Time, limit int) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []uuid.UUID{}
	for _, id := range m.order {
		r := m.rows[id]
		if r.OrgID == s.OrgID() && (r.State == domain.RemedyProposed || r.State == domain.RemedyApproved) &&
			!now.Before(r.ExpiresAt) && len(out) < limit {
			out = append(out, id)
		}
	}
	return out, nil
}

func (m *memRemedies) OutcomeOverdue(_ context.Context, s db.TenantScope, claimedBy time.Time, limit int) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []uuid.UUID{}
	for _, id := range m.order {
		r := m.rows[id]
		if r.OrgID == s.OrgID() && r.State == domain.RemedyExecuting && !r.ExecutingAt.After(claimedBy) && len(out) < limit {
			out = append(out, id)
		}
	}
	return out, nil
}

// declaredRemedy is one Remedy fact declared outbound, and the Incident it went to.
type declaredRemedy struct {
	incident uuid.UUID
	fact     domain.RemedyFact
}

// memRemedyDeclarer records every Remedy transition declared outbound.
type memRemedyDeclarer struct {
	mu    sync.Mutex
	facts []declaredRemedy
}

func (m *memRemedyDeclarer) DeclareRemedy(_ context.Context, _ db.TenantScope, incidentID uuid.UUID, f domain.RemedyFact) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.facts = append(m.facts, declaredRemedy{incident: incidentID, fact: f})
	return nil
}

func (m *memRemedyDeclarer) reasons() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []string{}
	for _, d := range m.facts {
		out = append(out, d.fact.Transition.To.FactReason())
	}
	return out
}

// ------------------------------------------------------------------- rig

// writeServerToken is the write ToolServer's token, for the scrub assertions.
const writeServerToken = "tst-live-write-toolserver-token"

// restartTool is a write Tool that records the arguments it was sent and answers ok.
func restartTool(sent *[]json.RawMessage, mu *sync.Mutex) toolserverfake.Tool {
	return toolserverfake.Tool{Name: "rollout_restart", Description: "restarts a Deployment",
		Handle: func(_ context.Context, c toolserverfake.Call) (string, bool) {
			mu.Lock()
			defer mu.Unlock()
			*sent = append(*sent, append(json.RawMessage(nil), c.Arguments...))
			return `deployment.apps/api restarted`, false
		}}
}

// withWriteServer starts an in-process MCP server serving one write Tool, configures it as
// the WRITE ToolServer `k8s-write`, and discovers it.
func (r *rig) withWriteServer(t *testing.T, tools ...toolserverfake.Tool) (*toolserverfake.Server, domain.ToolServerConfig) {
	t.Helper()
	srv := toolserverfake.Start(t, toolserverfake.Options{Token: writeServerToken}, tools...)
	r.toolDialer.inner = mcpclient.Dialer{HTTPClient: srv.Client}
	draft, err := domain.NewToolServerDraft("k8s-write", srv.URL, "", string(domain.AccessWrite), writeServerToken, defaultLimits(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := r.svc.CreateToolServer(context.Background(), r.scope, draft)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.DiscoverToolServer(context.Background(), r.scope, cfg.ID); err != nil {
		t.Fatalf("discover: %v", err)
	}
	return srv, cfg
}

// grant gives a user the Remedy approval grant on a ToolServer, as `oto grant` would.
func (r *rig) grant(toolServerID uuid.UUID, name string) domain.Requester {
	r.approvers.mu.Lock()
	defer r.approvers.mu.Unlock()
	if r.approvers.rows == nil {
		r.approvers.rows = map[uuid.UUID][]domain.RemedyApprover{}
	}
	by := domain.Requester{UserID: uuid.New(), Label: name}
	r.approvers.rows[toolServerID] = append(r.approvers.rows[toolServerID],
		domain.RemedyApprover{UserID: by.UserID, DisplayName: name, GrantedBy: "cli", Counts: true})
	return by
}

// remedyInvestigator is `firstlook` holding the two Remedy Tools.
func (r *rig) remedyInvestigator(t *testing.T) (domain.Investigator, domain.CaseSubject) {
	t.Helper()
	inv, c := r.setup(t, domain.DefaultBudgets())
	return r.allow(t, inv, ToolWriteTools, ToolProposeRemedy), c
}

// propose runs one Investigation whose model proposes each of the given argument documents
// to `oto_propose_remedy` in one turn, then concludes, and returns the run and its Remedies.
func (r *rig) propose(t *testing.T, inv domain.Investigator, c domain.CaseSubject, proposals ...string) (domain.Investigation, []domain.Remedy) {
	t.Helper()
	calls := make([]domain.ToolCall, 0, len(proposals))
	for i, p := range proposals {
		calls = append(calls, call("p"+string(rune('a'+i)), ToolProposeRemedy, p))
	}
	r.dial.script = []modelfake.Step{
		modelfake.Calls(300, 20, calls...),
		modelfake.Text("The api pods crash-loop after the 14:02 deploy; a restart picks up the reverted config.", 500, 40),
	}
	got, _ := r.run(t, r.request(t, inv, c).ID)
	list, err := r.svc.ListRemedies(context.Background(), r.scope, got.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got, list
}

// The arguments a test proposes: key order a re-encoder would sort, a number a float would
// round, and whitespace the stored form drops.
const (
	proposedArgs = `{ "namespace": "checkout", "deployment": "api", "generation": 12345678901234567890 }`
	compactArgs  = `{"namespace":"checkout","deployment":"api","generation":12345678901234567890}`
)

func restartProposal(args string) string {
	return `{"tool":"k8s-write__rollout_restart","arguments":` + args +
		`,"target":"Deployment checkout/api","description":"Restart the api to pick up the reverted config."}`
}

const noToolProposal = `{"target":"node pool eu-1","description":"Add a node: the pool is out of memory, and no configured Tool can add one."}`

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *errs.Error
	if err == nil || !errors.As(err, &e) || e.Code != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
}

// ------------------------------------------------------------------- tests

// TestAnInvestigatorProposesARemedyAndNeverHoldsTheWriteTool — the loop answers the proposal
// itself; the write Tool is never among the Tools any model turn was offered, and nothing in
// the run calls it; the Remedy keeps the EXACT arguments, compact, with their hash, two
// required approvals and its window; and a Case in no Incident declares nothing.
func TestAnInvestigatorProposesARemedyAndNeverHoldsTheWriteTool(t *testing.T) {
	r := newRig(t)
	var (
		sent []json.RawMessage
		mu   sync.Mutex
	)
	srv, cfg := r.withWriteServer(t, restartTool(&sent, &mu))
	inv, c := r.remedyInvestigator(t)

	// ⛔ The allowlist refuses the write Tool outright: an Investigator holds only read Tools.
	tools, err := domain.NewAllowlist([]string{ToolProposeRemedy, "k8s-write__rollout_restart"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.UpdateInvestigator(context.Background(), r.scope, inv.ID, domain.InvestigatorChange{Tools: &tools}); !errs.IsKind(err, errs.KindValidation) {
		t.Fatalf("an allowlist naming a write Tool was accepted: %v", err)
	}

	r.dial.script = []modelfake.Step{
		modelfake.Calls(300, 20, call("w", ToolWriteTools, `{}`)),
		// The model tries the write Tool directly, and then proposes it.
		modelfake.Calls(300, 20, call("x", "k8s-write__rollout_restart", proposedArgs),
			call("p", ToolProposeRemedy, restartProposal(proposedArgs))),
		modelfake.Text("Restart the api.", 500, 40),
	}
	got, steps := r.run(t, r.request(t, inv, c).ID)

	want := "model_turn tool_call:oto_write_tools:ok model_turn tool_call:k8s-write__rollout_restart:refused " +
		"tool_call:oto_propose_remedy:ok model_turn"
	if kinds(steps) != want {
		t.Fatalf("transcript = %s\nwant       %s", kinds(steps), want)
	}
	if !strings.Contains(steps[1].Result, "k8s-write__rollout_restart") {
		t.Fatalf("the write Tools listing does not name the write Tool: %s", steps[1].Result)
	}
	// ⛔⛔ THE WRITE TOOL WAS NEVER OFFERED, AND NEVER CALLED.
	for i, req := range r.dial.model().Requests() {
		for _, s := range req.Tools {
			if strings.HasPrefix(s.Name, cfg.Name+domain.QualifiedToolSeparator) {
				t.Fatalf("turn %d offered the model the write Tool %s", i, s.Name)
			}
		}
	}
	if srv.Calls() != 0 || len(sent) != 0 {
		t.Fatalf("a run called the write Tool %d time(s)", srv.Calls())
	}

	list, err := r.svc.ListRemedies(context.Background(), r.scope, got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("%d Remedies kept, want 1", len(list))
	}
	rem := list[0]
	if rem.Tool.ToolServerID != cfg.ID || rem.Tool.Tool != "rollout_restart" || rem.Arguments != compactArgs ||
		rem.ArgumentsSHA256 != domain.HashArguments(compactArgs) {
		t.Fatalf("Remedy = %+v", rem)
	}
	if rem.State != domain.RemedyProposed || rem.RequiredApprovals != 2 || rem.Blocked != "" ||
		!rem.ExpiresAt.Equal(rem.ProposedAt.Add(domain.DefaultRemedyApprovalWindow)) {
		t.Fatalf("Remedy = %+v", rem)
	}
	if len(rem.Transitions) != 1 || rem.Transitions[0].Actor.Kind != domain.ActorInvestigator ||
		!strings.Contains(rem.Transitions[0].Actor.Label, "firstlook v") {
		t.Fatalf("proposal = %+v", rem.Transitions)
	}
	// A Case in no Incident: recorded, declared nowhere.
	if rem.Transitions[0].DeclaredIncidentID != uuid.Nil || len(r.remedyFacts.facts) != 0 {
		t.Fatalf("a Remedy on a Case in no Incident was declared: %+v", r.remedyFacts.facts)
	}
	if got.ToolCalls != 2 {
		t.Fatalf("the run made %d Tool calls; the listing and the refused write call are two, and the "+
			"proposal is none", got.ToolCalls)
	}
}

// TestAProposalThatDoesNotHoldIsRefusedOnTheRecord — a read ToolServer's Tool, a Tool nobody
// listed, and arguments with no Tool are refused with the reason, and only what holds is kept.
func TestAProposalThatDoesNotHoldIsRefusedOnTheRecord(t *testing.T) {
	r := newRig(t)
	var (
		sent []json.RawMessage
		mu   sync.Mutex
	)
	r.withWriteServer(t, restartTool(&sent, &mu))
	inv, c := r.remedyInvestigator(t)
	_, list := r.propose(t, inv, c,
		`{"tool":"k8s-write__scale","arguments":{},"target":"x","description":"y"}`,
		`{"tool":"nope__restart","arguments":{},"target":"x","description":"y"}`,
		`{"arguments":{"a":1},"target":"x","description":"y"}`,
		restartProposal(`"not an object"`),
		restartProposal(proposedArgs))
	if len(list) != 1 || list[0].Arguments != compactArgs {
		t.Fatalf("kept %d Remedies: %+v", len(list), list)
	}
	steps, err := r.svc.GetInvestigation(context.Background(), r.scope, list[0].InvestigationID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps.Steps[1:5] {
		if s.Outcome != domain.OutcomeRefused {
			t.Fatalf("step %d = %s %q, want refused", s.Seq, s.Outcome, s.Result)
		}
	}
	if !strings.Contains(steps.Steps[1].Result, "k8s-write__rollout_restart") {
		t.Fatalf("the refusal does not name the write Tools there are: %q", steps.Steps[1].Result)
	}
}

// TestARemedyWithNoToolSaysSoIsRefusedOnApproveDeclinableAndExpires — ruling of 2026-10-02.
func TestARemedyWithNoToolSaysSoIsRefusedOnApproveDeclinableAndExpires(t *testing.T) {
	r := newRig(t)
	inv, c := r.remedyInvestigator(t)
	_, list := r.propose(t, inv, c, noToolProposal,
		`{"target":"node pool eu-2","description":"The same, for the other pool."}`)
	if len(list) != 2 {
		t.Fatalf("%d Remedies kept", len(list))
	}
	first, second := list[0], list[1]
	if first.Tool.Named() || first.Arguments != "" || first.Blocked != domain.NoToolCanCarryItOut {
		t.Fatalf("a Remedy with no Tool does not say so: %+v", first)
	}
	someone := r.grant(uuid.New(), "Grace Hopper")
	_, err := r.svc.ApproveRemedy(context.Background(), r.scope, first.ID, someone, "")
	wantCode(t, err, "remedy_has_no_tool")

	declined, err := r.svc.DeclineRemedy(context.Background(), r.scope, first.ID, someone)
	if err != nil {
		t.Fatal(err)
	}
	if declined.State != domain.RemedyDeclined || declined.Transitions[1].Actor.Label != "Grace Hopper" {
		t.Fatalf("declined = %+v", declined)
	}

	// The other expires: past its window it reads expired at once, and the sweep records it.
	r.clock.Advance(domain.DefaultRemedyApprovalWindow + time.Second)
	got, err := r.svc.GetRemedy(context.Background(), r.scope, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StateAt(r.clock.Now()) != domain.RemedyExpired {
		t.Fatalf("a Remedy past its window reads %s", got.StateAt(r.clock.Now()))
	}
	_, err = r.svc.DeclineRemedy(context.Background(), r.scope, second.ID, someone)
	wantCode(t, err, "remedy_expired")
	n, err := r.svc.ExpireRemedies(context.Background(), r.scope)
	if err != nil || n != 1 {
		t.Fatalf("expired %d, err %v", n, err)
	}
	got, _ = r.svc.GetRemedy(context.Background(), r.scope, second.ID)
	last := got.Transitions[len(got.Transitions)-1]
	if got.State != domain.RemedyExpired || last.Actor.Kind != domain.ActorSystem || last.To != domain.RemedyExpired {
		t.Fatalf("expiry not recorded by system: %+v", got)
	}
	// A second sweep records nothing more.
	if n, _ := r.svc.ExpireRemedies(context.Background(), r.scope); n != 0 {
		t.Fatalf("a second sweep expired %d", n)
	}
}

// TestTwoDifferentHoldersMustApproveAndTheSameOneCountsOnce — ADR 0054 §4.
func TestTwoDifferentHoldersMustApproveAndTheSameOneCountsOnce(t *testing.T) {
	r := newRig(t)
	var (
		sent []json.RawMessage
		mu   sync.Mutex
	)
	_, cfg := r.withWriteServer(t, restartTool(&sent, &mu))
	inv, c := r.remedyInvestigator(t)
	_, list := r.propose(t, inv, c, restartProposal(proposedArgs))
	rem := list[0]
	ada, grace := r.grant(cfg.ID, "Ada Lovelace"), r.grant(cfg.ID, "Grace Hopper")
	stranger := domain.Requester{UserID: uuid.New(), Label: "Mallory"}
	ctx := context.Background()

	// ⛔ A non-holder is refused, and nothing is recorded.
	_, err := r.svc.ApproveRemedy(ctx, r.scope, rem.ID, stranger, rem.ArgumentsSHA256)
	wantCode(t, err, "remedy_approver_required")

	got, err := r.svc.ApproveRemedy(ctx, r.scope, rem.ID, ada, rem.ArgumentsSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.RemedyProposed || got.Counted() != 1 {
		t.Fatalf("one approval made it %s with %d", got.State, got.Counted())
	}
	// ⛔ The same person again counts once.
	_, err = r.svc.ApproveRemedy(ctx, r.scope, rem.ID, ada, rem.ArgumentsSHA256)
	wantCode(t, err, "remedy_already_approved")
	if got, _ := r.svc.GetRemedy(ctx, r.scope, rem.ID); got.Counted() != 1 || got.State != domain.RemedyProposed {
		t.Fatalf("a second approval by the same person counted: %+v", got)
	}

	// ⛔ An approval of other arguments is refused.
	_, err = r.svc.ApproveRemedy(ctx, r.scope, rem.ID, grace, domain.HashArguments(`{"namespace":"payments"}`))
	wantCode(t, err, "remedy_arguments_changed")

	r.clock.Advance(10 * time.Minute)
	got, err = r.svc.ApproveRemedy(ctx, r.scope, rem.ID, grace, rem.ArgumentsSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.RemedyApproved || got.Counted() != 2 ||
		!got.ExpiresAt.Equal(r.clock.Now().UTC().Add(domain.DefaultRemedyApprovalWindow)) {
		t.Fatalf("two different approvals made %+v", got)
	}
	last := got.Transitions[len(got.Transitions)-1]
	if last.To != domain.RemedyApproved || last.Actor.Kind != domain.ActorUser || last.Actor.Label != "Grace Hopper" {
		t.Fatalf("the approval transition = %+v", last)
	}
	for _, a := range got.Approvals {
		if a.ArgumentsSHA256 != domain.HashArguments(compactArgs) {
			t.Fatalf("an approval names other arguments: %+v", a)
		}
	}
	// Approved is not proposed: a third approval is refused.
	third := r.grant(cfg.ID, "Barbara Liskov")
	_, err = r.svc.ApproveRemedy(ctx, r.scope, rem.ID, third, rem.ArgumentsSHA256)
	wantCode(t, err, "remedy_not_proposed")
	// ⛔ Approving executes nothing: execution is a separate step, enqueued with the approval.
	if len(sent) != 0 {
		t.Fatalf("approving called the write Tool")
	}
	if n := executeJobs(r, rem.ID); n != 1 {
		t.Fatalf("%d remedies.execute jobs enqueued for the approval, want 1", n)
	}
}

// TestOnePersonApprovingInSlackAndInTheUICountsOnce — ADR 0054 §4, git-bug ac9b492. The Slack
// card approves through this same call as the linked user, so the two surfaces differ only in
// how the person is labelled; the count is by user id, and the second is refused.
func TestOnePersonApprovingInSlackAndInTheUICountsOnce(t *testing.T) {
	r := newRig(t)
	var (
		sent []json.RawMessage
		mu   sync.Mutex
	)
	_, cfg := r.withWriteServer(t, restartTool(&sent, &mu))
	inv, c := r.remedyInvestigator(t)
	_, list := r.propose(t, inv, c, restartProposal(proposedArgs))
	rem := list[0]
	inUI := r.grant(cfg.ID, "Ada Lovelace")
	fromSlack := domain.Requester{UserID: inUI.UserID, Label: "ada@example.com"}
	ctx := context.Background()

	if _, err := r.svc.ApproveRemedy(ctx, r.scope, rem.ID, fromSlack, rem.ArgumentsSHA256); err != nil {
		t.Fatal(err)
	}
	_, err := r.svc.ApproveRemedy(ctx, r.scope, rem.ID, inUI, rem.ArgumentsSHA256)
	wantCode(t, err, "remedy_already_approved")
	if got, _ := r.svc.GetRemedy(ctx, r.scope, rem.ID); got.Counted() != 1 || got.State != domain.RemedyProposed {
		t.Fatalf("one person in two places counted as %d and moved it to %s", got.Counted(), got.State)
	}
}

// TestARemedyWhoseToolWasRemovedCannotBeApproved — a ToolServer re-declared read, one that
// stopped listing the Tool, and one removed outright.
func TestARemedyWhoseToolWasRemovedCannotBeApproved(t *testing.T) {
	r := newRig(t)
	var (
		sent []json.RawMessage
		mu   sync.Mutex
	)
	_, cfg := r.withWriteServer(t, restartTool(&sent, &mu))
	inv, c := r.remedyInvestigator(t)
	_, list := r.propose(t, inv, c, restartProposal(proposedArgs))
	rem := list[0]
	ada := r.grant(cfg.ID, "Ada Lovelace")
	ctx := context.Background()

	r.toolServers.setAccess(cfg.ID, domain.AccessRead)
	_, err := r.svc.ApproveRemedy(ctx, r.scope, rem.ID, ada, rem.ArgumentsSHA256)
	wantCode(t, err, "remedy_tool_unavailable")
	r.toolServers.setAccess(cfg.ID, domain.AccessWrite)

	r.toolServers.mu.Lock()
	listed := r.toolServers.tools[cfg.ID]
	r.toolServers.tools[cfg.ID] = nil
	r.toolServers.mu.Unlock()
	_, err = r.svc.ApproveRemedy(ctx, r.scope, rem.ID, ada, rem.ArgumentsSHA256)
	wantCode(t, err, "remedy_tool_unavailable")
	r.toolServers.mu.Lock()
	r.toolServers.tools[cfg.ID] = listed
	saved := r.toolServers.rows[cfg.ID]
	delete(r.toolServers.rows, cfg.ID)
	r.toolServers.mu.Unlock()
	_, err = r.svc.ApproveRemedy(ctx, r.scope, rem.ID, ada, rem.ArgumentsSHA256)
	wantCode(t, err, "remedy_tool_unavailable")
	got, err := r.svc.GetRemedy(ctx, r.scope, rem.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Blocked == "" || !strings.Contains(got.Blocked, "no longer configured") || got.Counted() != 0 {
		t.Fatalf("an unapprovable Remedy reads %+v", got)
	}
	// Restored, it is approvable again: the check is against the configuration NOW.
	r.toolServers.mu.Lock()
	r.toolServers.rows[cfg.ID] = saved
	r.toolServers.mu.Unlock()
	if _, err := r.svc.ApproveRemedy(ctx, r.scope, rem.ID, ada, rem.ArgumentsSHA256); err != nil {
		t.Fatal(err)
	}
}

// TestEveryTransitionIsDeclaredToTheIncident — ADR 0052 §5, ADR 0054 §2: a Remedy on a Case
// an Incident holds is declared to that Incident at proposal, approval and decline, with the
// snapshot of the transition; a no-Tool Remedy's fact says so.
func TestEveryTransitionIsDeclaredToTheIncident(t *testing.T) {
	r := newRig(t)
	var (
		sent []json.RawMessage
		mu   sync.Mutex
	)
	_, cfg := r.withWriteServer(t, restartTool(&sent, &mu))
	inv, c := r.remedyInvestigator(t)
	incident := domain.IncidentSubject{IncidentID: uuid.New(), Number: 7}
	r.incidents.incidents[incident.IncidentID] = incident
	r.incidents.holding[c.CaseID] = incident.IncidentID

	_, list := r.propose(t, inv, c, restartProposal(proposedArgs), noToolProposal)
	ctx := context.Background()
	ada, grace := r.grant(cfg.ID, "Ada Lovelace"), r.grant(cfg.ID, "Grace Hopper")
	if _, err := r.svc.ApproveRemedy(ctx, r.scope, list[0].ID, ada, list[0].ArgumentsSHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ApproveRemedy(ctx, r.scope, list[0].ID, grace, list[0].ArgumentsSHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.DeclineRemedy(ctx, r.scope, list[0].ID, ada); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(2 * domain.DefaultRemedyApprovalWindow)
	if _, err := r.svc.ExpireRemedies(ctx, r.scope); err != nil {
		t.Fatal(err)
	}

	want := []string{"remedy_proposed", "remedy_proposed", "remedy_approved", "remedy_declined", "remedy_expired"}
	if got := r.remedyFacts.reasons(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("declared %v, want %v", got, want)
	}
	for _, d := range r.remedyFacts.facts {
		if d.incident != incident.IncidentID || d.fact.Transition.DeclaredIncidentID != incident.IncidentID {
			t.Fatalf("a fact went to %s, want Incident %s", d.incident, incident.IncidentID)
		}
	}
	approved := r.remedyFacts.facts[2].fact
	if approved.Remedy.Arguments != compactArgs || len(approved.Remedy.Approvals) != 2 ||
		approved.Transition.Actor.Label != "Grace Hopper" {
		t.Fatalf("the approval's fact = %+v", approved)
	}
	if noTool := r.remedyFacts.facts[1].fact.Remedy; noTool.Tool.Named() {
		t.Fatalf("the no-Tool Remedy's fact names a Tool: %+v", noTool)
	}
	// ⛔ No transition so far sent anything to the write Tool.
	if len(sent) != 0 {
		t.Fatal("a declared transition reached the write Tool")
	}
}
