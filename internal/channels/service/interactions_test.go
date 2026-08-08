package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

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
	groupOne = uuid.MustParse("019fe297-d84f-7599-b5b2-1f231749104a")
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
	scope      db.TenantScope
	groupID    uuid.UUID
	actorKind  string
	actorID    string
	actorLabel string
}

type fakeGroups struct {
	// live is the set of groups visible in each tenant.
	live   map[uuid.UUID]uuid.UUID // groupID -> orgID
	result GroupAckResult
	err    error
	calls  []ackCall
}

func (f *fakeGroups) GroupExists(_ context.Context, s db.TenantScope, groupID uuid.UUID) (bool, error) {
	org, ok := f.live[groupID]
	return ok && org == s.OrgID(), nil
}

func (f *fakeGroups) AcknowledgeGroup(
	_ context.Context, s db.TenantScope, groupID uuid.UUID, kind, id, label string,
) (GroupAckResult, error) {
	f.calls = append(f.calls, ackCall{
		scope: s, groupID: groupID, actorKind: kind, actorID: id, actorLabel: label,
	})
	if f.err != nil {
		return GroupAckResult{}, f.err
	}
	return f.result, nil
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
func newService(t *testing.T, conv SlackConversations, actors SlackActors, groups AlertGroups,
	enq db.Enqueuer, notice SlackNotice,
) *InteractionService {
	t.Helper()
	if enq == nil {
		enq = &fakeEnqueuer{}
	}
	s, err := NewInteractionService(InteractionOptions{
		Conversations: conv,
		Actors:        actors,
		Groups:        groups,
		Enqueuer:      enq,
		Notice:        notice,
		Clock:         clock.NewFake(time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
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
		Value:         groupOne.String(),
		TeamID:        "T9TK3CUKW",
		ChannelID:     "C0123456789",
		SlackUserID:   "U0123456789",
		SlackUserName: "ram",
		ResponseURL:   "https://hooks.slack.com/actions/T9TK3CUKW/1/2",
	}
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
			payload:   envelope(ActionAcknowledge, groupOne.String()),
			wantJobs:  1,
			wantKinds: []string{jobs.KindSlackInteraction},
		},
		{
			name:      "an Un-acknowledge press is dispatched too",
			payload:   envelope(ActionUnacknowledge, groupOne.String()),
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
			s := newService(t, alphaConversations(), nil, &fakeGroups{}, enq, nil)

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

// TestHandleCopiesTheEnvelopeOntoTheJob.
//
// Every field the worker needs must survive the hop, because the worker has no
// other copy of the press: the HTTP request is long gone by the time it runs.
func TestHandleCopiesTheEnvelopeOntoTheJob(t *testing.T) {
	enq := &fakeEnqueuer{}
	s := newService(t, alphaConversations(), nil, &fakeGroups{}, enq, nil)

	if err := s.Handle(context.Background(), envelope(ActionAcknowledge, groupOne.String())); err != nil {
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
	s := newService(t, alphaConversations(), nil, &fakeGroups{}, enq, nil)

	if err := s.Handle(context.Background(), envelope(ActionAcknowledge, groupOne.String())); err == nil {
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
			groups := &fakeGroups{
				live:   map[uuid.UUID]uuid.UUID{groupOne: orgAlpha},
				result: GroupAckResult{Members: 2, Applied: 2},
			}
			notice := &fakeNotice{}
			s := newService(t, alphaConversations(), tc.actors, groups, nil, notice)

			if err := s.Apply(context.Background(), ackArgs()); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if len(groups.calls) != 1 {
				t.Fatalf("acknowledged %d times, want 1", len(groups.calls))
			}
			got := groups.calls[0]
			if got.actorKind != tc.wantKind || got.actorID != tc.wantID || got.actorLabel != tc.wantLabel {
				t.Fatalf("actor = (%q, %q, %q), want (%q, %q, %q)",
					got.actorKind, got.actorID, got.actorLabel, tc.wantKind, tc.wantID, tc.wantLabel)
			}
			if got.groupID != groupOne {
				t.Fatalf("acked group %s, want %s", got.groupID, groupOne)
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
	groups := &fakeGroups{
		live:   map[uuid.UUID]uuid.UUID{groupOne: orgAlpha},
		result: GroupAckResult{Members: 1, Applied: 1},
	}
	s := newService(t, alphaConversations(), actors, groups, nil, &fakeNotice{})

	if err := s.Apply(context.Background(), ackArgs()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := groups.calls[0].scope.OrgID(); got != orgAlpha {
		t.Fatalf("acked in org %s, want %s", got, orgAlpha)
	}
	// The identity lookup is made in the SAME tenant. A Slack member linked to a
	// user in another org must not be attributed here.
	if got := actors.gotScope.OrgID(); got != orgAlpha {
		t.Fatalf("resolved the actor in org %s, want %s", got, orgAlpha)
	}
}

// TestApplyRefusesAGroupFromAnotherOrg.
//
// ⛔⛔ THE CROSS-TENANT CASE. The conversation belongs to org alpha; the button's
// value names a group in org beta. Every call below is already scoped, so the
// ack would find no members and do nothing — a SILENT nothing, which is the exact
// failure being fixed. It must refuse loudly, tell the user, and never reach the
// acknowledgement at all.
func TestApplyRefusesAGroupFromAnotherOrg(t *testing.T) {
	groups := &fakeGroups{
		live:   map[uuid.UUID]uuid.UUID{groupOne: orgBeta},
		result: GroupAckResult{Members: 9, Applied: 9},
	}
	notice := &fakeNotice{}
	s := newService(t, alphaConversations(), &fakeActors{}, groups, nil, notice)

	if err := s.Apply(context.Background(), ackArgs()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(groups.calls) != 0 {
		t.Fatalf("a press in one workspace acknowledged another org's alert group: %+v", groups.calls)
	}
	if got := notice.only(); !strings.Contains(got, "different oto organisation") {
		t.Fatalf("the user was told %q; a cross-tenant press must be answered honestly", got)
	}
}

// TestApplyTellsTheUserWhenTheActionCannotApply is the edge-case table.
//
// Every row is a press that changes nothing, and every row must produce a
// SENTENCE. "Nothing happened and nobody said so" is the defect; answering all
// of them with one shrug is only marginally better.
func TestApplyTellsTheUserWhenTheActionCannotApply(t *testing.T) {
	tests := []struct {
		name     string
		conv     SlackConversations
		groups   *fakeGroups
		args     jobs.SlackInteractionArgs
		wantSaid string
		wantAcks int
	}{
		{
			name: "a conversation oto has no channel for",
			conv: fakeConversations{byPair: map[string]SlackDestination{}},
			groups: &fakeGroups{
				live: map[uuid.UUID]uuid.UUID{groupOne: orgAlpha},
			},
			args:     ackArgs(),
			wantSaid: "no channel configured",
		},
		{
			name:     "a button value that is not an identifier",
			conv:     alphaConversations(),
			groups:   &fakeGroups{live: map[uuid.UUID]uuid.UUID{groupOne: orgAlpha}},
			args:     func() jobs.SlackInteractionArgs { a := ackArgs(); a.Value = "not-a-uuid"; return a }(),
			wantSaid: "could not read that button",
		},
		{
			name:     "a group that no longer exists",
			conv:     alphaConversations(),
			groups:   &fakeGroups{live: map[uuid.UUID]uuid.UUID{}},
			args:     ackArgs(),
			wantSaid: "no longer find that alert group",
		},
		{
			// The double-click, and "somebody else got there first". Both are the
			// same honest fact, and neither is an error.
			name: "already acknowledged",
			conv: alphaConversations(),
			groups: &fakeGroups{
				live: map[uuid.UUID]uuid.UUID{groupOne: orgAlpha},
				result: GroupAckResult{
					Members: 2, Applied: 0,
					SkippedCodes: map[string]int{"already_acked": 2},
				},
			},
			args:     ackArgs(),
			wantSaid: "Already acknowledged",
			wantAcks: 1,
		},
		{
			// The occurrence resolved or expired while the human was deciding.
			name: "every member has already ended",
			conv: alphaConversations(),
			groups: &fakeGroups{
				live: map[uuid.UUID]uuid.UUID{groupOne: orgAlpha},
				result: GroupAckResult{
					Members: 3, Applied: 0,
					SkippedCodes: map[string]int{"no_open_occurrence": 3},
				},
			},
			args:     ackArgs(),
			wantSaid: "resolved or expired",
			wantAcks: 1,
		},
		{
			name: "a group whose members have all left it",
			conv: alphaConversations(),
			groups: &fakeGroups{
				live:   map[uuid.UUID]uuid.UUID{groupOne: orgAlpha},
				result: GroupAckResult{Members: 0, Applied: 0},
			},
			args:     ackArgs(),
			wantSaid: "no live alerts left",
			wantAcks: 1,
		},
		{
			name: "part acked, part ended",
			conv: alphaConversations(),
			groups: &fakeGroups{
				live: map[uuid.UUID]uuid.UUID{groupOne: orgAlpha},
				result: GroupAckResult{
					Members: 4, Applied: 0,
					SkippedCodes: map[string]int{"already_acked": 1, "no_open_occurrence": 3},
				},
			},
			args:     ackArgs(),
			wantSaid: "already acknowledged and the rest has resolved",
			wantAcks: 1,
		},
		{
			// The button oto renders and cannot yet serve. It says where the
			// action DOES exist rather than looking like it worked.
			name:     "the Un-acknowledge button is answered honestly",
			conv:     alphaConversations(),
			groups:   &fakeGroups{live: map[uuid.UUID]uuid.UUID{groupOne: orgAlpha}},
			args:     func() jobs.SlackInteractionArgs { a := ackArgs(); a.ActionID = ActionUnacknowledge; return a }(),
			wantSaid: "not available from Slack yet",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			notice := &fakeNotice{}
			s := newService(t, tc.conv, &fakeActors{}, tc.groups, nil, notice)

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
			if len(tc.groups.calls) != tc.wantAcks {
				t.Fatalf("acknowledged %d times, want %d", len(tc.groups.calls), tc.wantAcks)
			}
		})
	}
}

// TestNothingOtoSaysImpliesOwnership.
//
// ⛔ SCOPE-BOUNDARY §5.1, ASSERTED ON THE COPY. An acknowledgement is a fact
// about an ALERT — "a human has seen this" — and never a claim over it. The day
// a sentence here says "assigned", "yours" or "I'm on it" is the day oto has
// grown an ownership axis in its user-visible language, which is how every
// on-call product started.
func TestNothingOtoSaysImpliesOwnership(t *testing.T) {
	said := []string{
		nothingAppliedText(GroupAckResult{Members: 0}),
		nothingAppliedText(GroupAckResult{Members: 2, SkippedCodes: map[string]int{"already_acked": 2}}),
		nothingAppliedText(GroupAckResult{Members: 2, SkippedCodes: map[string]int{"no_open_occurrence": 2}}),
		nothingAppliedText(GroupAckResult{Members: 4, SkippedCodes: map[string]int{"already_acked": 1, "no_open_occurrence": 3}}),
	}

	// Collect the ephemeral copy the two remaining paths emit, too.
	for _, args := range []jobs.SlackInteractionArgs{
		func() jobs.SlackInteractionArgs { a := ackArgs(); a.ActionID = ActionUnacknowledge; return a }(),
		func() jobs.SlackInteractionArgs { a := ackArgs(); a.Value = "nope"; return a }(),
		func() jobs.SlackInteractionArgs { a := ackArgs(); a.ChannelID = "C-unknown"; return a }(),
	} {
		notice := &fakeNotice{}
		s := newService(t, alphaConversations(), &fakeActors{},
			&fakeGroups{live: map[uuid.UUID]uuid.UUID{groupOne: orgAlpha}}, nil, notice)
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
		s := newService(t, fakeConversations{err: boom}, &fakeActors{}, &fakeGroups{}, nil, &fakeNotice{})
		if err := s.Apply(context.Background(), ackArgs()); err == nil {
			t.Fatal("a transient resolver failure must be retried, not swallowed")
		}
	})

	t.Run("an acknowledgement failure is retried", func(t *testing.T) {
		groups := &fakeGroups{live: map[uuid.UUID]uuid.UUID{groupOne: orgAlpha}, err: boom}
		s := newService(t, alphaConversations(), &fakeActors{}, groups, nil, &fakeNotice{})
		if err := s.Apply(context.Background(), ackArgs()); err == nil {
			t.Fatal("a transient acknowledgement failure must be retried, not swallowed")
		}
	})
}

// TestApplyWithNoResponseURLStillAcknowledges.
//
// The reply channel is a nicety; the receipt is the product. A press with no
// `response_url` must still land on the timeline.
func TestApplyWithNoResponseURLStillAcknowledges(t *testing.T) {
	groups := &fakeGroups{
		live:   map[uuid.UUID]uuid.UUID{groupOne: orgAlpha},
		result: GroupAckResult{Members: 1, Applied: 1},
	}
	args := ackArgs()
	args.ResponseURL = ""

	s := newService(t, alphaConversations(), &fakeActors{}, groups, nil, nil)
	if err := s.Apply(context.Background(), args); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(groups.calls) != 1 {
		t.Fatal("a press with no response_url was dropped")
	}
}
