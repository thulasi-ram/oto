package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// These tests drive the consumer with SYNTHETIC payloads and fakes, because the
// whole point of the split is that neither half needs Slack. The transport's own
// tests prove the HMAC; these prove what happens once an envelope is trusted.

var (
	orgAlpha = uuid.MustParse("019fe200-0000-7000-8000-00000000000a")
	orgBeta  = uuid.MustParse("019fe200-0000-7000-8000-00000000000b")
	caseOne  = uuid.MustParse("019fe297-d84f-7599-b5b2-1f231749104a")
	userRAM  = uuid.MustParse("019fe111-0000-7000-8000-000000000001")
)

// ------------------------------------------------------------------- fakes

type fakeConversations struct {
	// byPair maps "team/channel" onto the tenant that configured it.
	byPair map[string]SlackDestination
	err    error
}

func (f fakeConversations) ResolveSlackConversation(
	_ context.Context, teamID, conversationID string,
) (SlackDestination, error) {
	if f.err != nil {
		return SlackDestination{}, f.err
	}
	d, ok := f.byPair[teamID+"/"+conversationID]
	if !ok {
		return SlackDestination{}, errs.NotFound("slack_conversation_not_found", "no such conversation")
	}
	return d, nil
}

type fakeActors struct {
	actor SlackActor
	err   error
	// gotScope records the tenant the lookup was made in — the property that
	// stops a member linked in one org being attributed in another.
	gotScope db.TenantScope
}

func (f *fakeActors) SlackActor(
	_ context.Context, s db.TenantScope, _, _, _ string,
) (SlackActor, error) {
	f.gotScope = s
	if f.err != nil {
		return SlackActor{}, f.err
	}
	return f.actor, nil
}

type ackCall struct {
	// verb is "ack" or "unack". It is recorded because the two verbs are
	// deliberately identical in shape, so a press wired to the WRONG one would
	// otherwise satisfy every other assertion in this file.
	verb       string
	scope      db.TenantScope
	caseID     uuid.UUID
	actorKind  string
	actorID    string
	actorLabel string
}

// fakeCases stands in for the Case-shaped port.
//
// ⛔ IT WAS `fakeGroups`, AND `result GroupAckResult` IS GONE WITH THE FAN-OUT
// (git-bug 7570090). A verb over one Case either applies or is refused, so the only
// thing left to program is `err` — and the refusals are the domain's own
// `errs.KindPrecondition` codes rather than a per-member tally this fake would have
// had to invent.
type fakeCases struct {
	// live is the set of Cases visible in each tenant.
	live  map[uuid.UUID]uuid.UUID // caseID -> orgID
	err   error
	calls []ackCall
}

func (f *fakeCases) CaseExists(_ context.Context, s db.TenantScope, caseID uuid.UUID) (bool, error) {
	org, ok := f.live[caseID]
	return ok && org == s.OrgID(), nil
}

func (f *fakeCases) AcknowledgeCase(
	_ context.Context, s db.TenantScope, caseID uuid.UUID, kind, id, label string,
) error {
	return f.record("ack", s, caseID, kind, id, label)
}

func (f *fakeCases) UnacknowledgeCase(
	_ context.Context, s db.TenantScope, caseID uuid.UUID, kind, id, label string,
) error {
	return f.record("unack", s, caseID, kind, id, label)
}

func (f *fakeCases) record(
	verb string, s db.TenantScope, caseID uuid.UUID, kind, id, label string,
) error {
	f.calls = append(f.calls, ackCall{
		verb: verb, scope: s, caseID: caseID, actorKind: kind, actorID: id, actorLabel: label,
	})
	return f.err
}

// fakeLabels stands in for the read-only labels port.
//
// It is scoped like the real thing: a Case in another org answers NotFound rather
// than handing over a label set, because that is the only property of this port
// that could leak one tenant's alert into another tenant's channel.
type fakeLabels struct {
	byCase map[uuid.UUID]map[string]string
	orgOf  map[uuid.UUID]uuid.UUID
	err    error
	calls  int
}

func (f *fakeLabels) CaseLabels(
	_ context.Context, s db.TenantScope, caseID uuid.UUID,
) (map[string]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if org, ok := f.orgOf[caseID]; !ok || org != s.OrgID() {
		return nil, errs.NotFound("alert_case_not_found", "no such case in this tenant")
	}
	return f.byCase[caseID], nil
}

type fakeNotice struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeNotice) Ephemeral(_ context.Context, _, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	return nil
}

func (f *fakeNotice) only() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 1 {
		return ""
	}
	return f.sent[0]
}

type fakeEnqueuer struct {
	reqs []db.JobRequest
	err  error
}

func (f *fakeEnqueuer) Enqueue(ctx context.Context, args db.JobArgs, opts ...db.JobOption) (db.EnqueueResult, error) {
	res, err := f.EnqueueMany(ctx, []db.JobRequest{{Args: args, Opts: opts}})
	if err != nil {
		return db.EnqueueResult{}, err
	}
	return res[0], nil
}

func (f *fakeEnqueuer) EnqueueMany(_ context.Context, reqs []db.JobRequest) ([]db.EnqueueResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.reqs = append(f.reqs, reqs...)
	out := make([]db.EnqueueResult, len(reqs))
	for i, r := range reqs {
		out[i] = db.EnqueueResult{Kind: r.Args.Kind()}
	}
	return out, nil
}

// newService wires the consumer over fakes. Every argument may be nil, which is
// also the assertion that each collaborator degrades rather than panics.
func newService(t *testing.T, conv SlackConversations, actors SlackActors, cases Cases,
	enq db.Enqueuer, notice SlackNotice,
) *InteractionService {
	t.Helper()
	return newServiceWith(t, InteractionOptions{
		Conversations: conv,
		Actors:        actors,
		Cases:         cases,
		Enqueuer:      enq,
		Notice:        notice,
	})
}

// newServiceWith is the same wiring with every port in reach, for the ports the
// positional helper above does not name. The defaults it fills in are the ones no
// test wants an opinion about.
func newServiceWith(t *testing.T, o InteractionOptions) *InteractionService {
	t.Helper()
	if o.Enqueuer == nil {
		o.Enqueuer = &fakeEnqueuer{}
	}
	// A real counter on no registry: the increments are observable without
	// any test having to own a registry or worry about duplicate registration.
	o.Metrics = NewInteractionMetrics(nil)
	o.Clock = clock.NewFake(time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := NewInteractionService(o)
	if err != nil {
		t.Fatalf("NewInteractionService: %v", err)
	}
	return s
}

// envelope builds a block_actions payload with one action.
func envelope(actionID, value string) json.RawMessage {
	return json.RawMessage(`{
	  "type": "block_actions",
	  "team": {"id": "T9TK3CUKW"},
	  "user": {"id": "U0123456789", "username": "ram"},
	  "channel": {"id": "C0123456789"},
	  "container": {"message_ts": "1712345678.000100"},
	  "response_url": "https://hooks.slack.com/actions/T9TK3CUKW/1/2",
	  "actions": [{"action_id": "` + actionID + `", "type": "button", "value": "` + value + `"}]
	}`)
}

func ackArgs() jobs.SlackInteractionArgs {
	return jobs.SlackInteractionArgs{
		ActionID:      ActionAcknowledge,
		Value:         caseOne.String(),
		TeamID:        "T9TK3CUKW",
		ChannelID:     "C0123456789",
		SlackUserID:   "U0123456789",
		SlackUserName: "ram",
		ResponseURL:   "https://hooks.slack.com/actions/T9TK3CUKW/1/2",
	}
}

// unackArgs is the same press on the other button, which is the whole point: the
// two paths differ in the action id and in nothing else the transport carries.
func unackArgs() jobs.SlackInteractionArgs {
	a := ackArgs()
	a.ActionID = ActionUnacknowledge
	return a
}

// refusal builds the shape `alerts/domain` answers a press it cannot apply with:
// `errs.KindPrecondition` carrying one of three stable codes. It is spelt out here
// rather than borrowed so this file states the contract it depends on.
func refusal(code string) error {
	return errs.New(errs.KindPrecondition, code, "the case does not admit the verb")
}

func alphaConversations() fakeConversations {
	return fakeConversations{byPair: map[string]SlackDestination{
		"T9TK3CUKW/C0123456789": {OrgID: orgAlpha, ChannelID: uuid.New()},
	}}
}

// ------------------------------------------------------- Handle (the ack path)

// TestHandleEnqueuesTheAcknowledgeAndNothingElse is the dispatch table.
//
// ⭐ It is also the regression test for the original defect. The endpoint
// verified its HMAC, answered 200 and dropped the payload on the floor; a press
// that enqueues NOTHING is exactly that bug, so every row here asserts the job
// count and not merely the absence of an error.
func TestHandleEnqueuesTheAcknowledgeAndNothingElse(t *testing.T) {
	tests := []struct {
		name      string
		payload   json.RawMessage
		wantJobs  int
		wantKinds []string
	}{
		{
			name:      "an Acknowledge press becomes exactly one job",
			payload:   envelope(ActionAcknowledge, caseOne.String()),
			wantJobs:  1,
			wantKinds: []string{jobs.KindSlackInteraction},
		},
		{
			name:      "an Un-acknowledge press is dispatched too",
			payload:   envelope(ActionUnacknowledge, caseOne.String()),
			wantJobs:  1,
			wantKinds: []string{jobs.KindSlackInteraction},
		},
		{
			// A URL button. Slack delivers an interaction oto must acknowledge and
			// there is nothing to do; enqueueing work for it would be worse than
			// the no-op it replaced.
			name:     "a link button enqueues nothing",
			payload:  envelope("oto.noop.runbook", ""),
			wantJobs: 0,
		},
		{
			// The links overflow. It is a CONTAINER of places to look, so pressing
			// the container is not a verb — and it is not `oto.noop.*` either, so
			// before ActionOverflow existed this press fell to the default arm and
			// counted as an action oto could not route.
			name:     "the links overflow enqueues nothing",
			payload:  envelope(ActionOverflow, ""),
			wantJobs: 0,
		},
		{
			name:     "an unknown action enqueues nothing",
			payload:  envelope("oto.something.new", "x"),
			wantJobs: 0,
		},
		{
			name: "an interaction type oto does not serve is ignored",
			payload: json.RawMessage(`{"type":"view_submission","team":{"id":"T1"},
			  "user":{"id":"U1"},"channel":{"id":"C1"}}`),
			wantJobs: 0,
		},
		{
			// An envelope with no workspace cannot resolve a tenant, so there is
			// nothing safe to do with it.
			name: "an envelope naming no workspace is refused",
			payload: json.RawMessage(`{"type":"block_actions","team":{"id":""},
			  "user":{"id":"U1"},"channel":{"id":"C1"},
			  "actions":[{"action_id":"oto.ack","value":"x"}]}`),
			wantJobs: 0,
		},
		{
			name:     "a block action carrying no actions is ignored",
			payload:  json.RawMessage(`{"type":"block_actions","team":{"id":"T1"},"user":{"id":"U1"},"channel":{"id":"C1"},"actions":[]}`),
			wantJobs: 0,
		},
		{
			name:     "an unparseable envelope is swallowed, never retried",
			payload:  json.RawMessage(`{"type":`),
			wantJobs: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enq := &fakeEnqueuer{}
			s := newService(t, alphaConversations(), nil, &fakeCases{}, enq, nil)

			if err := s.Handle(context.Background(), tc.payload); err != nil {
				t.Fatalf("Handle returned %v; a payload it cannot act on must not become a retry", err)
			}
			if len(enq.reqs) != tc.wantJobs {
				t.Fatalf("enqueued %d jobs, want %d", len(enq.reqs), tc.wantJobs)
			}
			for i, want := range tc.wantKinds {
				if got := enq.reqs[i].Args.Kind(); got != want {
					t.Fatalf("job %d kind = %q, want %q", i, got, want)
				}
			}
		})
	}
}

// counterValue reads a counter without pulling in client_golang's `testutil`.
//
// `testutil.ToFloat64` is the obvious call and is deliberately not used: it
// drags `prometheus/common/expfmt` into the test build, which this module does
// not otherwise require, and one assertion helper is not worth a `go mod tidy`
// in the dependency graph. `Write` is the same interface `Gather` uses.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// TestUnknownActionIsRecordedAndNotMerelyLogged.
//
// ⭐ THE 200 IS NOT NEGOTIABLE, SO THE COUNTER IS THE ONLY RECORD. Slack
// disables an app's event subscriptions when more than 95 % of deliveries fail
// inside a 60-minute window, which means oto may not answer 4xx to a button it
// cannot route — the person who pressed it sees a tick either way. §H.8 requires
// `slack_unknown_action_total` for exactly that reason: without it an unroutable
// press is indistinguishable, from outside, from the silent no-op the whole
// interaction path was fixed to abolish.
//
// The counter is asserted to stay STILL for the routable and the deliberately
// inert ids too. A counter that also fires for `oto.noop.*` would read as a
// permanent fault on every card oto posts, and an operator learns within a week
// to ignore a metric that is never zero.
func TestUnknownActionIsRecordedAndNotMerelyLogged(t *testing.T) {
	tests := []struct {
		name     string
		actionID string
		want     float64
	}{
		{
			name:     "an unrecognised action id increments the counter",
			actionID: "oto.something.new",
			want:     1,
		},
		{
			// Not oto's namespace at all — a button from another app delivered to
			// oto's request URL, or a forgery inside an authentic envelope.
			name:     "a foreign action id increments the counter",
			actionID: "acme.approve",
			want:     1,
		},
		{
			name:     "a link button is inert, not unknown",
			actionID: "oto.noop.runbook",
			want:     0,
		},
		{
			// ⛔ THIS IS THE REGRESSION GUARD. Every links-overflow press used to
			// land here, so the series had a floor on every card oto posts and the
			// metric said "oto cannot route its own menu". The overflow is known,
			// routed, and deliberately inert; see ActionOverflow.
			name:     "the links overflow is inert, not unknown",
			actionID: ActionOverflow,
			want:     0,
		},
		{
			name:     "the Acknowledge button is routable",
			actionID: ActionAcknowledge,
			want:     0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newService(t, alphaConversations(), nil, &fakeCases{}, &fakeEnqueuer{}, nil)

			if err := s.Handle(context.Background(), envelope(tc.actionID, caseOne.String())); err != nil {
				t.Fatalf("Handle returned %v; an unroutable press must never become a retry", err)
			}
			if got := counterValue(t, s.metrics.UnknownAction); got != tc.want {
				t.Fatalf("oto_slack_unknown_action_total = %v after %q, want %v",
					got, tc.actionID, tc.want)
			}
		})
	}
}

// TestUnknownActionOnTheWorkerIsRecordedToo.
//
// The worker's default branch is reachable only ACROSS A DEPLOY — `Handle`
// enqueues nothing it cannot route, so a job naming an unserved action is one an
// older binary wrote and a newer one drained. It lands on the same series
// because from an operator's chair it is the same fact: a press oto could not
// act on. It must also not become a job retry, because retrying it twelve times
// cannot make the action id known.
func TestUnknownActionOnTheWorkerIsRecordedToo(t *testing.T) {
	s := newService(t, alphaConversations(), nil, &fakeCases{}, &fakeEnqueuer{}, nil)

	args := ackArgs()
	args.ActionID = "oto.retired.verb"
	if err := s.Apply(context.Background(), args); err != nil {
		t.Fatalf("Apply returned %v; an action the worker cannot serve is an outcome, not a transient failure", err)
	}
	if got := counterValue(t, s.metrics.UnknownAction); got != 1 {
		t.Fatalf("oto_slack_unknown_action_total = %v, want 1", got)
	}
}

// TestHandleCopiesTheEnvelopeOntoTheJob.
//
// Every field the worker needs must survive the hop, because the worker has no
// other copy of the press: the HTTP request is long gone by the time it runs.
func TestHandleCopiesTheEnvelopeOntoTheJob(t *testing.T) {
	enq := &fakeEnqueuer{}
	s := newService(t, alphaConversations(), nil, &fakeCases{}, enq, nil)

	if err := s.Handle(context.Background(), envelope(ActionAcknowledge, caseOne.String())); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(enq.reqs) != 1 {
		t.Fatalf("enqueued %d jobs, want 1", len(enq.reqs))
	}
	args, ok := enq.reqs[0].Args.(jobs.SlackInteractionArgs)
	if !ok {
		t.Fatalf("enqueued %T, want SlackInteractionArgs", enq.reqs[0].Args)
	}

	want := ackArgs()
	want.MessageTS = "1712345678.000100"
	if args != want {
		t.Fatalf("job args =\n %+v\nwant\n %+v", args, want)
	}
}

// TestHandleReturnsTheEnqueueFailure.
//
// The ONE error worth returning. The transport logs it, and the user has already
// seen their button settle — but a lost enqueue is a lost acknowledgement, and it
// must be visible in the logs rather than swallowed with everything else.
func TestHandleReturnsTheEnqueueFailure(t *testing.T) {
	enq := &fakeEnqueuer{err: errors.New("postgres is on fire")}
	s := newService(t, alphaConversations(), nil, &fakeCases{}, enq, nil)

	if err := s.Handle(context.Background(), envelope(ActionAcknowledge, caseOne.String())); err == nil {
		t.Fatal("Handle swallowed an enqueue failure; a lost press would be invisible")
	}
}

// ---------------------------------------------------------- Apply (the work)

// TestApplyAcknowledgesAsTheRightPerson.
//
// ⛔ THE UNLINKED CASE IS THE IMPORTANT ONE. A Slack member with no oto account
// must still be able to acknowledge: refusing would silently lose every press
// from anybody who has not onboarded, which is a worse failure than the one this
// whole change fixes.
func TestApplyAcknowledgesAsTheRightPerson(t *testing.T) {
	tests := []struct {
		name      string
		actors    SlackActors
		wantKind  string
		wantID    string
		wantLabel string
	}{
		{
			name:      "a linked Slack member acks as their oto user",
			actors:    &fakeActors{actor: SlackActor{UserID: userRAM, Label: "ram@example.com"}},
			wantKind:  "user",
			wantID:    userRAM.String(),
			wantLabel: "ram@example.com",
		},
		{
			name:      "an UNLINKED Slack member still acks, as themselves",
			actors:    &fakeActors{},
			wantKind:  "slack",
			wantID:    "U0123456789",
			wantLabel: "@ram",
		},
		{
			// A directory lookup that fails must never cost an acknowledgement.
			name:      "a failed identity lookup falls back rather than losing the ack",
			actors:    &fakeActors{err: errors.New("identity store is down")},
			wantKind:  "slack",
			wantID:    "U0123456789",
			wantLabel: "@ram",
		},
		{
			name:      "no identity port at all still records the press",
			actors:    nil,
			wantKind:  "slack",
			wantID:    "U0123456789",
			wantLabel: "@ram",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cases := &fakeCases{live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha}}
			notice := &fakeNotice{}
			s := newService(t, alphaConversations(), tc.actors, cases, nil, notice)

			if err := s.Apply(context.Background(), ackArgs()); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if len(cases.calls) != 1 {
				t.Fatalf("acknowledged %d times, want 1", len(cases.calls))
			}
			got := cases.calls[0]
			if got.actorKind != tc.wantKind || got.actorID != tc.wantID || got.actorLabel != tc.wantLabel {
				t.Fatalf("actor = (%q, %q, %q), want (%q, %q, %q)",
					got.actorKind, got.actorID, got.actorLabel, tc.wantKind, tc.wantID, tc.wantLabel)
			}
			if got.caseID != caseOne {
				t.Fatalf("acked case %s, want %s", got.caseID, caseOne)
			}
			// A successful ack says nothing in Slack: the CARD is the feedback,
			// and it is updated by the dispatch path.
			if n := len(notice.sent); n != 0 {
				t.Fatalf("a successful acknowledgement sent %d ephemeral messages: %v", n, notice.sent)
			}
		})
	}
}

// TestApplyResolvesTheTenantFromTheConversationAndNowhereElse.
//
// ⭐⭐ THE HIGHEST-STAKES PROPERTY IN THE CHANGE. The payload names a workspace
// and a channel, never an org, and everything downstream runs under the scope
// this resolution produces. A tenant taken from anything the presser controls
// would be a cross-tenant write with a friendly name.
func TestApplyResolvesTheTenantFromTheConversationAndNowhereElse(t *testing.T) {
	actors := &fakeActors{}
	cases := &fakeCases{live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha}}
	s := newService(t, alphaConversations(), actors, cases, nil, &fakeNotice{})

	if err := s.Apply(context.Background(), ackArgs()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := cases.calls[0].scope.OrgID(); got != orgAlpha {
		t.Fatalf("acked in org %s, want %s", got, orgAlpha)
	}
	// The identity lookup is made in the SAME tenant. A Slack member linked to a
	// user in another org must not be attributed here.
	if got := actors.gotScope.OrgID(); got != orgAlpha {
		t.Fatalf("resolved the actor in org %s, want %s", got, orgAlpha)
	}
}

// TestApplyRefusesACaseFromAnotherOrg.
//
// ⛔⛔ THE CROSS-TENANT PRESS. The conversation belongs to org alpha; the button's
// value names a Case in org beta. Every call below is already scoped, so the ack
// would write nothing — a SILENT nothing, which is the exact failure being fixed.
// It must refuse loudly, tell the user, and never reach the acknowledgement at all.
//
// ⛔ BOTH VERBS ARE HELD TO IT. The withdrawal reads and writes the same rows
// through the same scope, so a tenancy check present on one path and absent on the
// other is a hole with a friendly name on half the buttons.
func TestApplyRefusesACaseFromAnotherOrg(t *testing.T) {
	for _, args := range []jobs.SlackInteractionArgs{ackArgs(), unackArgs()} {
		t.Run(args.ActionID, func(t *testing.T) {
			cases := &fakeCases{live: map[uuid.UUID]uuid.UUID{caseOne: orgBeta}}
			notice := &fakeNotice{}
			s := newService(t, alphaConversations(), &fakeActors{}, cases, nil, notice)

			if err := s.Apply(context.Background(), args); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if len(cases.calls) != 0 {
				t.Fatalf("a press in one workspace reached another org's Case: %+v", cases.calls)
			}
			if got := notice.only(); !strings.Contains(got, "different oto organisation") {
				t.Fatalf("the user was told %q; a cross-tenant press must be answered honestly", got)
			}
		})
	}
}

// TestApplyTellsTheUserWhenTheActionCannotApply is the edge-case table.
//
// Every row is a press that changes nothing, and every row must produce a
// SENTENCE. "Nothing happened and nobody said so" is the defect; answering all
// of them with one shrug is only marginally better.
func TestApplyTellsTheUserWhenTheActionCannotApply(t *testing.T) {
	tests := []struct {
		name      string
		conv      SlackConversations
		cases     *fakeCases
		args      jobs.SlackInteractionArgs
		wantSaid  string
		wantCalls int
	}{
		{
			name: "a conversation oto has no channel for",
			conv: fakeConversations{byPair: map[string]SlackDestination{}},
			cases: &fakeCases{
				live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha},
			},
			args:     ackArgs(),
			wantSaid: "no channel configured",
		},
		{
			name:     "a button value that is not an identifier",
			conv:     alphaConversations(),
			cases:    &fakeCases{live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha}},
			args:     func() jobs.SlackInteractionArgs { a := ackArgs(); a.Value = "not-a-uuid"; return a }(),
			wantSaid: "could not read that button",
		},
		{
			name:     "a Case that no longer exists",
			conv:     alphaConversations(),
			cases:    &fakeCases{live: map[uuid.UUID]uuid.UUID{}},
			args:     ackArgs(),
			wantSaid: "no longer find that alert",
		},
		{
			// The double-click, and "somebody else got there first". Both are the
			// same honest fact, and neither is an error.
			name: "already acknowledged",
			conv: alphaConversations(),
			cases: &fakeCases{
				live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha},
				err:  refusal("already_acked"),
			},
			args:      ackArgs(),
			wantSaid:  "Already acknowledged",
			wantCalls: 1,
		},
		{
			// The Case resolved or expired while the human was deciding.
			name: "the episode has already ended",
			conv: alphaConversations(),
			cases: &fakeCases{
				live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha},
				err:  refusal("no_open_case"),
			},
			args:      ackArgs(),
			wantSaid:  "resolved or expired",
			wantCalls: 1,
		},
		{
			// ⛔ TWO ROWS WERE HERE AND ARE DELETED (git-bug 7570090): "a group whose
			// members have all left it" (`Members: 0` → "no live alerts left") and
			// "part acked, part ended" (a mixed `SkippedCodes` map). Both describe a
			// SET, and a press now touches one Case. Neither shape is reachable.
			//
			// ⛔ WHAT REPLACES THEM IS THE `default` ARM OF THE COPY, AND IT IS NOT
			// DEAD WEIGHT. A precondition code this surface has never seen must still
			// produce a SENTENCE: silence is the defect this whole file was written
			// against, and a new code in `alerts/domain` must not be able to
			// reintroduce it.
			name: "a refusal code this surface has never seen",
			conv: alphaConversations(),
			cases: &fakeCases{
				live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha},
				err:  refusal("some_future_code"),
			},
			args:      ackArgs(),
			wantSaid:  "nothing to acknowledge",
			wantCalls: 1,
		},
		{
			// The mirror of the unreadable-value row above, and it must point at the
			// mirror of the action: "withdraw the acknowledgement there", not
			// "acknowledge it there".
			name:     "an Un-acknowledge button value that is not an identifier",
			conv:     alphaConversations(),
			cases:    &fakeCases{live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha}},
			args:     func() jobs.SlackInteractionArgs { a := unackArgs(); a.Value = "not-a-uuid"; return a }(),
			wantSaid: "withdraw the acknowledgement there",
		},
		{
			// ⛔ THE MIRROR OF "already acknowledged", AND THE ROW MOST AT RISK OF
			// BORROWING THE WRONG SENTENCE. `not_acked` means the Case is OPEN and
			// simply carries no receipt — possibly because somebody withdrew it a
			// second earlier. It has not resolved, and saying so would be oto naming
			// the wrong nothing.
			name: "the alert was not acknowledged",
			conv: alphaConversations(),
			cases: &fakeCases{
				live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha},
				err:  refusal("not_acked"),
			},
			args:      unackArgs(),
			wantSaid:  "no receipt here to take back",
			wantCalls: 1,
		},
		{
			// ⭐ THE `no_open_case` / 412 PATH, told in Slack's terms. It is the same
			// refusal the HTTP contract answers `412 no_open_case` to, and the same
			// one the ack reports: there is no open episode left to write on.
			name: "the episode has already ended, so there is no receipt to withdraw",
			conv: alphaConversations(),
			cases: &fakeCases{
				live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha},
				err:  refusal("no_open_case"),
			},
			args:      unackArgs(),
			wantSaid:  "nothing to withdraw — this alert has already resolved",
			wantCalls: 1,
		},
		{
			// The withdrawal's own `default` arm, for the reason given on the ack's.
			name: "a withdrawal refusal code this surface has never seen",
			conv: alphaConversations(),
			cases: &fakeCases{
				live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha},
				err:  refusal("some_future_code"),
			},
			args:      unackArgs(),
			wantSaid:  "nothing to withdraw",
			wantCalls: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			notice := &fakeNotice{}
			s := newService(t, tc.conv, &fakeActors{}, tc.cases, nil, notice)

			if err := s.Apply(context.Background(), tc.args); err != nil {
				t.Fatalf("Apply returned %v; a press that cannot apply is an outcome, not a retry", err)
			}
			got := notice.only()
			if got == "" {
				t.Fatalf("the user was told nothing (%d messages); a silent no-op is the defect", len(notice.sent))
			}
			if !strings.Contains(got, tc.wantSaid) {
				t.Fatalf("the user was told %q, which does not contain %q", got, tc.wantSaid)
			}
			if len(tc.cases.calls) != tc.wantCalls {
				t.Fatalf("reached the case verb %d times, want %d", len(tc.cases.calls), tc.wantCalls)
			}
			// ⛔ THE VERB IS DERIVED FROM THE BUTTON AND ASSERTED, never taken on
			// trust. The two verbs have identical signatures, so a press wired to the
			// wrong one would pass every other assertion in this row — while
			// acknowledging an alert somebody had just asked oto to forget seeing.
			if tc.wantCalls > 0 {
				wantVerb := "ack"
				if tc.args.ActionID == ActionUnacknowledge {
					wantVerb = "unack"
				}
				if got := tc.cases.calls[0].verb; got != wantVerb {
					t.Fatalf("the press reached the %q fan-out, want %q", got, wantVerb)
				}
			}
		})
	}
}

// ⛔⛔ `TestAnIncompleteFanOutIsSaidToBeIncomplete` WAS HERE AND IS DELETED (git-bug
// 7570090), AND IT IS THE MOST VALUABLE TEST THIS CHANGE RETIRES — so what it knew
// is written down instead of thrown away.
//
// It pinned a real defect. The partial notice was gated inside `Applied > 0`, and
// the shape a bounded fan-out actually produces after one press is the opposite: a
// group above `domain.FanOutLimit` whose oldest 500 members are ALREADY
// acknowledged concludes on 500 refusals, applies nothing, and has thousands of
// unacknowledged alerts behind the ceiling. That fell through to "Already
// acknowledged. An acknowledgement records that somebody has seen this" — told to
// somebody in a storm, about 4 500 alerts nobody had seen. The fix was to answer
// REACH FIRST and let `Applied` only choose the sentence.
//
// ⭐ THE RULE IT ESTABLISHED SURVIVES AND IS NOW STRUCTURAL: a press may never be
// answered with a sentence about a bigger set than it actually touched. A press
// touches one Case, so there is no ceiling, no remainder and no arithmetic left to
// get wrong. If this surface ever acts on more than one Case again it needs an
// account before it needs copy — that is the whole lesson, and the `GroupAckResult`
// tombstone in `interactions.go` says so at the other end.

// ------------------------------------------- Apply (the withdrawal), in mirror

// TestApplyWithdrawsAnAcknowledgementAsTheRightPerson is the ack test read
// backwards, and the point of running it twice is that the two verbs are the same
// four steps over the same members.
//
// ⛔ THE UNLINKED CASE IS THE IMPORTANT ONE HERE TOO, and for a sharper reason: a
// receipt belongs to nobody, so the person withdrawing it need not be the person
// who wrote it and there is nothing to check them against. Refusing an unlinked
// member would leave a wrong acknowledgement standing on an alert with no way to
// take it back from the surface it was written on.
func TestApplyWithdrawsAnAcknowledgementAsTheRightPerson(t *testing.T) {
	tests := []struct {
		name      string
		actors    SlackActors
		wantKind  string
		wantID    string
		wantLabel string
	}{
		{
			name:      "a linked Slack member withdraws as their oto user",
			actors:    &fakeActors{actor: SlackActor{UserID: userRAM, Label: "ram@example.com"}},
			wantKind:  "user",
			wantID:    userRAM.String(),
			wantLabel: "ram@example.com",
		},
		{
			name:      "an UNLINKED Slack member still withdraws, as themselves",
			actors:    &fakeActors{},
			wantKind:  "slack",
			wantID:    "U0123456789",
			wantLabel: "@ram",
		},
		{
			name:      "a failed identity lookup falls back rather than losing the withdrawal",
			actors:    &fakeActors{err: errors.New("identity store is down")},
			wantKind:  "slack",
			wantID:    "U0123456789",
			wantLabel: "@ram",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cases := &fakeCases{live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha}}
			notice := &fakeNotice{}
			s := newService(t, alphaConversations(), tc.actors, cases, nil, notice)

			if err := s.Apply(context.Background(), unackArgs()); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if len(cases.calls) != 1 {
				t.Fatalf("withdrew %d times, want 1", len(cases.calls))
			}
			got := cases.calls[0]
			if got.verb != "unack" {
				t.Fatalf("an Un-acknowledge press reached the %q fan-out; it must reach the withdrawal", got.verb)
			}
			if got.actorKind != tc.wantKind || got.actorID != tc.wantID || got.actorLabel != tc.wantLabel {
				t.Fatalf("actor = (%q, %q, %q), want (%q, %q, %q)",
					got.actorKind, got.actorID, got.actorLabel, tc.wantKind, tc.wantID, tc.wantLabel)
			}
			if got.caseID != caseOne {
				t.Fatalf("withdrew from case %s, want %s", got.caseID, caseOne)
			}
			// The tenancy comes from the conversation on this path too.
			if org := got.scope.OrgID(); org != orgAlpha {
				t.Fatalf("withdrew in org %s, want %s", org, orgAlpha)
			}
			// A withdrawal that applied says nothing in Slack, exactly as
			// the ack does not: the CARD is the feedback and the dispatch path owns it.
			if n := len(notice.sent); n != 0 {
				t.Fatalf("a complete withdrawal sent %d ephemeral messages: %v", n, notice.sent)
			}
		})
	}
}

// TestAWithdrawalThatAppliedIsSilent.
//
// ⛔ THIS USED TO BE `TestAWithdrawalThatCoveredTheGroupIsSilentEvenWhenMembersRefused`
// (git-bug 7570090), whose point was that "some members were not acknowledged" is
// NOT a partial press: the press asked for a group where nothing carries a receipt,
// and that is exactly what it got. There is one Case now, so the mixed outcome it
// described cannot occur — but the property it was defending is the one that
// matters and it is asserted here: a gesture that landed says NOTHING in Slack. The
// card is the feedback, and inventing a notice would nag somebody about a press
// that worked.
func TestAWithdrawalThatAppliedIsSilent(t *testing.T) {
	cases := &fakeCases{live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha}}
	notice := &fakeNotice{}
	s := newService(t, alphaConversations(), &fakeActors{}, cases, nil, notice)

	if err := s.Apply(context.Background(), unackArgs()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n := len(notice.sent); n != 0 {
		t.Fatalf("the user was told %v; this alert now carries no receipt, "+
			"which is what the press asked for", notice.sent)
	}
}

// ⛔⛔ `TestAnIncompleteWithdrawalSaysWhatIsStillAcknowledged` WAS HERE AND IS
// DELETED (git-bug 7570090). It guarded the sharpest asymmetry on this surface, and
// the asymmetry itself is NOT deleted — only the counting is.
//
// The outstanding count meant the OPPOSITE THING on the two paths. A bounded ack
// left its unreached members UNACKNOWLEDGED; a bounded withdrawal left them
// ACKNOWLEDGED — receipts standing on thousands of alerts nobody had looked at
// since. "Still unacknowledged" told to that person is precisely backwards, so the
// test asserted the ack's sentence ABSENT from the withdrawal's copy rather than
// merely un-asserted.
//
// ⭐ THE ASYMMETRY LIVES ON IN `ackRefusalText` vs `unackRefusalText`, which is
// where it still applies: `already_acked` and `not_acked` are different afternoons,
// and `TestApplyTellsTheUserWhenTheActionCannotApply` holds each to its own
// sentence. There is no remainder to describe any more, so there is no count left
// to get backwards.

// TestNothingOtoSaysImpliesOwnership.
//
// ⛔ SCOPE-BOUNDARY §5.1, ASSERTED ON THE COPY. An acknowledgement is a fact
// about an ALERT — "a human has seen this" — and never a claim over it. The day
// a sentence here says "assigned", "yours" or "I'm on it" is the day oto has
// grown an ownership axis in its user-visible language, which is how every
// on-call product started.
func TestNothingOtoSaysImpliesOwnership(t *testing.T) {
	// ⛔ THE SWEEP IS OVER THE REFUSAL CODES NOW, NOT OVER A FAN-OUT ACCOUNT
	// (git-bug 7570090). It covers every code `alerts/domain` can answer with PLUS
	// the unknown-code arm, so a sentence added to the `default` branch cannot slip
	// past this check either.
	codes := []string{"already_acked", "not_acked", "no_open_case", "some_future_code"}
	said := []string{}
	for _, c := range codes {
		said = append(said, ackRefusalText(c))
		// The withdrawal's copy is held to the same standard. It is arguably the
		// likelier place to slip: the sentence a person wants after taking a
		// receipt back is "it's yours again", and oto has no such axis to hand it
		// back along.
		said = append(said, unackRefusalText(c))
	}

	// Collect the ephemeral copy the two remaining paths emit, too.
	for _, args := range []jobs.SlackInteractionArgs{
		func() jobs.SlackInteractionArgs { a := unackArgs(); a.Value = "nope"; return a }(),
		func() jobs.SlackInteractionArgs { a := ackArgs(); a.Value = "nope"; return a }(),
		func() jobs.SlackInteractionArgs { a := ackArgs(); a.ChannelID = "C-unknown"; return a }(),
	} {
		notice := &fakeNotice{}
		s := newService(t, alphaConversations(), &fakeActors{},
			&fakeCases{live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha}}, nil, notice)
		if err := s.Apply(context.Background(), args); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		said = append(said, notice.sent...)
	}

	banned := []string{
		"own", "yours", "mine", "assign", "claim", "taking", "on it",
		"responsible", "handle it", "looking into",
	}
	for _, s := range said {
		low := strings.ToLower(s)
		for _, b := range banned {
			if strings.Contains(low, b) {
				t.Fatalf("copy %q contains %q: an acknowledgement is a receipt, never a claim over the alert", s, b)
			}
		}
	}
}

// TestApplyRetriesOnlyWhatIsWorthRetrying.
//
// A database that is down IS worth twelve retries. An alert that resolved is
// not, and turning it into one would re-run the acknowledgement all night to
// redeliver a sentence.
func TestApplyRetriesOnlyWhatIsWorthRetrying(t *testing.T) {
	boom := errors.New("connection refused")

	t.Run("a resolver failure is retried", func(t *testing.T) {
		s := newService(t, fakeConversations{err: boom}, &fakeActors{}, &fakeCases{}, nil, &fakeNotice{})
		if err := s.Apply(context.Background(), ackArgs()); err == nil {
			t.Fatal("a transient resolver failure must be retried, not swallowed")
		}
	})

	t.Run("an acknowledgement failure is retried", func(t *testing.T) {
		cases := &fakeCases{live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha}, err: boom}
		s := newService(t, alphaConversations(), &fakeActors{}, cases, nil, &fakeNotice{})
		if err := s.Apply(context.Background(), ackArgs()); err == nil {
			t.Fatal("a transient acknowledgement failure must be retried, not swallowed")
		}
	})

	t.Run("a withdrawal failure is retried", func(t *testing.T) {
		cases := &fakeCases{live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha}, err: boom}
		s := newService(t, alphaConversations(), &fakeActors{}, cases, nil, &fakeNotice{})
		if err := s.Apply(context.Background(), unackArgs()); err == nil {
			t.Fatal("a transient withdrawal failure must be retried, not swallowed")
		}
	})
}

// TestApplyWithNoResponseURLStillAcknowledges.
//
// The reply channel is a nicety; the receipt is the product. A press with no
// `response_url` must still land on the timeline.
func TestApplyWithNoResponseURLStillAcknowledges(t *testing.T) {
	cases := &fakeCases{live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha}}
	args := ackArgs()
	args.ResponseURL = ""

	s := newService(t, alphaConversations(), &fakeActors{}, cases, nil, nil)
	if err := s.Apply(context.Background(), args); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(cases.calls) != 1 {
		t.Fatal("a press with no response_url was dropped")
	}
}

// ------------------------------------------------- the links overflow's one verb

// labelsValue is what the renderer mints for `Show all labels`. It is built from
// the shared constant rather than typed out, because a literal here would pass
// while the two ends disagreed — which is the defect the constant exists to
// prevent.
func labelsValue(id uuid.UUID) string {
	return domain.ShowLabelsValuePrefix + id.String()
}

func labelsArgs() jobs.SlackInteractionArgs {
	a := ackArgs()
	a.ActionID = ActionOverflow
	a.Value = labelsValue(caseOne)
	return a
}

// TestHandleRoutesShowAllLabelsAndLeavesTheLinksInert is the split arm.
//
// ⛔ THIS IS THE REGRESSION TEST FOR git-bug `60e6e10`. `oto.more` was inert for
// every option in the menu, which is right for the four url destinations — Slack
// navigates those itself — and wrong for `Show all labels`, the one option that
// asks oto to render something. A press of it enqueued nothing, so nothing was
// logged, nothing errored, and the operator was answered with silence.
//
// ⭐ EVERY ROW ASSERTS THE JOB COUNT, in both directions. "Enqueues nothing" is
// the defect for one option and the design for the other four, and only the value
// tells them apart.
func TestHandleRoutesShowAllLabelsAndLeavesTheLinksInert(t *testing.T) {
	tests := []struct {
		name     string
		actionID string
		value    string
		wantJobs int
	}{
		{
			name:     "a Show all labels press becomes exactly one job",
			actionID: ActionOverflow,
			value:    labelsValue(caseOne),
			wantJobs: 1,
		},
		{
			// A url option. Slack sends no value for one, and it is the absence of a
			// value that keeps the four links inert.
			name:     "a link option in the same menu stays inert",
			actionID: ActionOverflow,
			value:    "",
			wantJobs: 0,
		},
		{
			name:     "a link button stays inert",
			actionID: "oto.noop.runbook",
			value:    "",
			wantJobs: 0,
		},
		{
			// ⛔ THE NAMESPACE DECIDES, NOT THE VALUE. `oto.noop.*` means "there is
			// nothing here to do"; a labels-shaped value on one is a card oto never
			// rendered, and honouring it would make the namespace stop being true.
			name:     "a link button carrying a labels value stays inert",
			actionID: "oto.noop.silence",
			value:    labelsValue(caseOne),
			wantJobs: 0,
		},
		{
			name:     "a labels value naming nothing enqueues nothing",
			actionID: ActionOverflow,
			value:    domain.ShowLabelsValuePrefix,
			wantJobs: 0,
		},
		{
			// A value oto did not mint is not one to interpret charitably — the rule
			// snoozeSubject states and labelsSubject is held to.
			name:     "a labels value with a third field enqueues nothing",
			actionID: ActionOverflow,
			value:    labelsValue(caseOne) + "|drop-tables",
			wantJobs: 0,
		},
		{
			name:     "a bare uuid on the overflow enqueues nothing",
			actionID: ActionOverflow,
			value:    caseOne.String(),
			wantJobs: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enq := &fakeEnqueuer{}
			s := newService(t, alphaConversations(), nil, &fakeCases{}, enq, nil)

			if err := s.Handle(context.Background(), envelope(tc.actionID, tc.value)); err != nil {
				t.Fatalf("Handle returned %v; a payload it cannot act on must not become a retry", err)
			}
			if len(enq.reqs) != tc.wantJobs {
				t.Fatalf("enqueued %d jobs, want %d", len(enq.reqs), tc.wantJobs)
			}
			// ⛔ NOT ONE OF THESE PRESSES IS AN UNKNOWN ACTION. The whole menu is
			// known and routed; a counter that ticked here would put a permanent
			// floor under a series that means "oto is broken".
			if got := counterValue(t, s.metrics.UnknownAction); got != 0 {
				t.Fatalf("oto_slack_unknown_action_total = %v; the links overflow is known and routed", got)
			}
			if tc.wantJobs == 0 {
				return
			}
			args, ok := enq.reqs[0].Args.(jobs.SlackInteractionArgs)
			if !ok {
				t.Fatalf("enqueued %T, want jobs.SlackInteractionArgs", enq.reqs[0].Args)
			}
			if args.ActionID != ActionOverflow || args.Value != tc.value {
				t.Fatalf("enqueued action %q value %q, want %q / %q",
					args.ActionID, args.Value, ActionOverflow, tc.value)
			}
			// ⛔ NO UNIQUENESS WINDOW ON THIS ONE. The four writing actions collapse a
			// byte-identical replay as a convenience over an idempotent write; here the
			// outcome IS the reply, so a second press collapsed onto the first would be
			// answered with silence — the defect, restored by an optimisation.
			if len(enq.reqs[0].Opts) != 0 {
				t.Fatalf("a Show all labels press carried %d job options; a collapsed replay is an unanswered press",
					len(enq.reqs[0].Opts))
			}
		})
	}
}

// TestApplyShowAllLabelsNamesEveryLabel is the owner's ruling, asserted.
//
// The press is answered with an ephemeral listing the labels — no modal, no
// `views.open`, nothing on §H.8's three-second path — and EVERY label is in it.
// A list that quietly drops one is a card that lies about a label set, which is
// the failure mode this product exists not to have.
func TestApplyShowAllLabelsNamesEveryLabel(t *testing.T) {
	labels := map[string]string{
		"alertname": "HighErrorRate",
		"severity":  "critical",
		"namespace": "observability",
		"service":   "checkout",
		"team":      "payments",
		// ⛔ UPSTREAM TEXT, AND IT MUST NOT BECOME MARKUP. A label value is
		// Alertmanager's, never oto's: one containing a broadcast must arrive as
		// characters, and one containing a backtick must not break out of its span.
		"note": "<!channel> `oops` & more",
	}
	fake := &fakeLabels{
		byCase: map[uuid.UUID]map[string]string{caseOne: labels},
		orgOf:  map[uuid.UUID]uuid.UUID{caseOne: orgAlpha},
	}
	notice := &fakeNotice{}
	cases := &fakeCases{live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha}}
	s := newServiceWith(t, InteractionOptions{
		Conversations: alphaConversations(),
		Actors:        &fakeActors{},
		Cases:         cases,
		Labels:        fake,
		Notice:        notice,
	})

	if err := s.Apply(context.Background(), labelsArgs()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	said := notice.only()
	if said == "" {
		t.Fatal("a Show all labels press was answered with silence; SPEC §B.8.6: buttons are never no-ops")
	}
	for name, value := range labels {
		if !strings.Contains(said, name) {
			t.Fatalf("the reply %q does not name the label %q", said, name)
		}
		if name == "note" {
			continue
		}
		if !strings.Contains(said, value) {
			t.Fatalf("the reply %q does not carry the value of %q", said, name)
		}
	}
	if strings.Contains(said, "<!channel>") || strings.Contains(said, "`oops`") {
		t.Fatalf("the reply %q passed upstream markup through unescaped", said)
	}
	if !strings.Contains(said, "(6)") {
		t.Fatalf("the reply %q does not say how many labels there are", said)
	}
	// ⛔ IT IS A READ AND NOTHING ELSE. No receipt, no snooze, no timeline entry:
	// the day this press writes something is the day a menu of places to look grew
	// a verb.
	if len(cases.calls) != 0 {
		t.Fatalf("a Show all labels press made %d case writes; listing labels changes nothing", len(cases.calls))
	}
	// ⭐ ONE READ, NOT TWO. There is no `CaseExists` probe in front of this port —
	// the read is scoped and answers the tenancy question itself — so a second call
	// here would be a round trip re-asking what the first one already answered.
	if fake.calls != 1 {
		t.Fatalf("the labels port was called %d times, want exactly 1", fake.calls)
	}
}

// TestApplyShowAllLabelsIsNeverSilent is the edge-case table, and it is the whole
// point of the ticket rather than a completeness exercise.
//
// Every row is a press that cannot produce a label list, and every row must
// produce a SENTENCE. "Nothing happened and nobody said so" is the defect being
// fixed; a row that ends in silence has reintroduced it through a different door.
func TestApplyShowAllLabelsIsNeverSilent(t *testing.T) {
	tests := []struct {
		name     string
		labels   Labels
		args     jobs.SlackInteractionArgs
		wantSaid string
	}{
		{
			// ⚠️ THE UNWIRED DEPLOYMENT. The renderer is a pure function of the view,
			// so the option is on the card whether or not the port was injected; the
			// only thing left to decide is whether the human finds out.
			name:     "a deployment with no labels port",
			labels:   nil,
			args:     labelsArgs(),
			wantSaid: "cannot list labels from Slack in this deployment yet",
		},
		{
			name: "a Case that belongs to another organisation",
			labels: &fakeLabels{
				byCase: map[uuid.UUID]map[string]string{caseOne: {"alertname": "HighErrorRate"}},
				orgOf:  map[uuid.UUID]uuid.UUID{caseOne: orgBeta},
			},
			args:     labelsArgs(),
			wantSaid: "no longer find that alert",
		},
		{
			// Reachable across a deploy: an older binary enqueued a value this one
			// cannot parse. It is answered, not dropped.
			name:     "a menu value oto did not mint",
			labels:   &fakeLabels{},
			args:     func() jobs.SlackInteractionArgs { a := labelsArgs(); a.Value = "labels|nope"; return a }(),
			wantSaid: "could not read that menu choice",
		},
		{
			name: "a Case with no labels at all",
			labels: &fakeLabels{
				byCase: map[uuid.UUID]map[string]string{caseOne: {}},
				orgOf:  map[uuid.UUID]uuid.UUID{caseOne: orgAlpha},
			},
			args:     labelsArgs(),
			wantSaid: "no labels recorded",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			notice := &fakeNotice{}
			s := newServiceWith(t, InteractionOptions{
				Conversations: alphaConversations(),
				Actors:        &fakeActors{},
				Cases:         &fakeCases{live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha}},
				Labels:        tc.labels,
				Notice:        notice,
			})

			if err := s.Apply(context.Background(), tc.args); err != nil {
				t.Fatalf("Apply returned %v; a press that cannot apply is an outcome, not a retry", err)
			}
			said := notice.only()
			if said == "" {
				t.Fatal("the press was answered with silence")
			}
			if !strings.Contains(said, tc.wantSaid) {
				t.Fatalf("said %q, want it to contain %q", said, tc.wantSaid)
			}
		})
	}
}

// TestApplyShowAllLabelsRetriesOnlyWhatIsWorthRetrying.
//
// A database that is down IS worth twelve retries, and the retry is free here:
// the job writes nothing, so running it again produces the same sentence and no
// second effect.
func TestApplyShowAllLabelsRetriesOnlyWhatIsWorthRetrying(t *testing.T) {
	notice := &fakeNotice{}
	s := newServiceWith(t, InteractionOptions{
		Conversations: alphaConversations(),
		Actors:        &fakeActors{},
		Cases:         &fakeCases{live: map[uuid.UUID]uuid.UUID{caseOne: orgAlpha}},
		Labels:        &fakeLabels{err: errors.New("connection refused")},
		Notice:        notice,
	})
	if err := s.Apply(context.Background(), labelsArgs()); err == nil {
		t.Fatal("a transient label read failure must be retried, not swallowed")
	}
}

// TestShowAllLabelsFitsInsideSlacksLimit.
//
// ⛔ THE LIMIT IS REACHED IN PRACTICE, NOT IN THEORY. An Alert may carry 64 labels
// (B3), each value up to 4 KiB (B5), and a whole label set up to 16 KiB (B6) —
// five times what one Slack message may say. An unbounded list is REFUSED by
// Slack, which turns "here are the labels" back into the silence this ticket is
// about.
//
// ⭐ AND THE CUT MUST SAY SO (§H.7). A reader told that nine labels are not listed
// knows to open oto; a reader shown 55 of 64 with no note believes they have seen
// the label set.
func TestShowAllLabelsFitsInsideSlacksLimit(t *testing.T) {
	labels := map[string]string{}
	for i := range 64 {
		labels["label_"+strconv.Itoa(i)] = strings.Repeat("x", 4096)
	}
	said := labelListText(labels)

	if len(said) > maxNoticeText {
		t.Fatalf("the reply is %d bytes, over Slack's %d: a payload over the limit is rejected, "+
			"which answers the press with nothing", len(said), maxNoticeText)
	}
	if !strings.Contains(said, "(64)") {
		t.Fatalf("the reply %q does not say how many labels the alert carries", said)
	}
	if !strings.Contains(said, "not listed") {
		t.Fatalf("the reply was cut without saying so: %q", said)
	}
	// Every line that IS listed must be whole — a value cut mid-rune, or a code
	// span left open, is the mojibake that makes an operator distrust the screen.
	if strings.Contains(said, "�") {
		t.Fatalf("the reply contains a broken rune: %q", said)
	}
	if strings.Count(said, "`")%2 != 0 {
		t.Fatalf("the reply left a code span open: %q", said)
	}
}

// TestOneEnormousLabelStillLeavesRoomForTheRest.
//
// The pathological single label: one 4 KiB value must not eat the whole message
// and leave the other labels unlisted. Both cuts are visible, and both are
// counted or ellipsised rather than silent.
func TestOneEnormousLabelStillLeavesRoomForTheRest(t *testing.T) {
	said := labelListText(map[string]string{
		"alertname": "HighErrorRate",
		"aaa_huge":  strings.Repeat("y", 4096),
		"zzz_last":  "still here",
	})
	if len(said) > maxNoticeText {
		t.Fatalf("the reply is %d bytes, over %d", len(said), maxNoticeText)
	}
	for _, want := range []string{"alertname", "HighErrorRate", "zzz_last", "still here", "…"} {
		if !strings.Contains(said, want) {
			t.Fatalf("the reply %q dropped %q: one long value must not cost the other labels", said, want)
		}
	}
}
