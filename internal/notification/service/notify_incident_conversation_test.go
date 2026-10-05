package service_test

// ADR 0052 §6 — AN INCIDENT MAY BE A CONVERSATION (git-bug bf5fc7e).
//
// Every clause of the ticket's done-when is a claim about a row, so these run the
// real evaluation and fan-out over a real Postgres, with the two incidents-side
// answers — the Incident, and which conversation a Case belongs in — supplied as
// literals through their ports:
//
//   - a Case fact evaluated after its membership exists lands in the Incident's
//     conversation, on a thread keyed by the Incident, with no root of its own;
//   - one evaluated before it stays in the Case's own conversation — the boundary is
//     the membership as read when the fact is evaluated, so there is no race to
//     test around;
//   - an Incident fact on a threaded channel opens the Incident's thread when the
//     Incident is a conversation;
//   - each member Case that already had its own thread gets ONE pointer reply there,
//     and nothing already posted moves.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/notification/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
)

// membership answers `IncidentConversations` for the one Case the fixture names: in
// the Incident while `in` is true, in no conversation otherwise. Flipping it is the
// membership row committing.
type membership struct {
	in  bool
	ref domain.IncidentRef
}

func (m *membership) ConversationFor(
	_ context.Context, _ db.TenantScope, _ uuid.UUID,
) (domain.IncidentRef, bool, error) {
	if !m.in {
		return domain.IncidentRef{}, false, nil
	}
	return m.ref, true, nil
}

type conversationRig struct {
	fx       fixture
	facts    domain.IncidentFacts
	member   *membership
	notifier *service.NotificationService
	threads  *repository.ThreadRepository
}

// newConversationRig routes Case facts AND Incident facts to one threaded channel,
// and draws the fixture's Incident as a conversation.
func newConversationRig(t *testing.T) conversationRig {
	t.Helper()
	fx := newFixture(t, domain.CapThreading|domain.CapAmend)
	facts := incidentFixtureFacts(fx.caseID, fx.alertID)
	facts.DrawnByLabel, facts.DrawnByCorrelator = "", id.New()
	facts.Conversation = true

	channels := repository.NewChannelRepository(fx.pool)
	policies, err := service.NewPolicyService(policyStore{policy: domain.Policy{
		ID: fx.policyID, OrgID: fx.orgID, Name: "everything", Priority: 1, Enabled: true,
		Reasons: []domain.Reason{
			domain.ReasonFired, domain.ReasonAllResolved,
			domain.ReasonDrawn, domain.ReasonCaseAdded, domain.ReasonQuiet,
		},
		ChannelIDs: []uuid.UUID{fx.channel.ID},
	}}, channels)
	require.NoError(t, err)

	member := &membership{ref: domain.IncidentRef{ID: facts.ID}}
	threads := repository.NewThreadRepository(fx.pool)
	clk := clock.New()
	notifier, err := service.NewNotificationService(service.NotificationConfig{
		Tx:            txRunner{pool: fx.pool},
		Policies:      policies,
		Notifications: repository.NewNotificationRepository(fx.pool),
		Deliveries:    repository.NewDeliveryRepository(fx.pool),
		Threads:       threads,
		Snapshots:     snapshots{fx: fx},
		Events:        repository.NewEventRepository(fx.pool, clk),
		Enqueuer:      &enqueuer{},
		Channels:      channels,
		Incidents:     incidents{facts: facts},
		Conversations: member,
		Clock:         clk,
	})
	require.NoError(t, err)
	return conversationRig{fx: fx, facts: facts, member: member, notifier: notifier, threads: threads}
}

func conversationOf(t *testing.T, r conversationRig, notificationID uuid.UUID) (string, uuid.UUID) {
	t.Helper()
	var (
		kind string
		conv uuid.UUID
	)
	require.NoError(t, r.fx.pool.QueryRow(t.Context(),
		`SELECT conversation_kind, conversation_id FROM notifications WHERE id = $1`, notificationID).
		Scan(&kind, &conv))
	return kind, conv
}

func threadsKeyedBy(t *testing.T, r conversationRig, kind string) int {
	t.Helper()
	var n int
	require.NoError(t, r.fx.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM channel_threads WHERE org_id = $1 AND subject_kind = $2`,
		r.fx.orgID, kind).Scan(&n))
	return n
}

func TestACaseFactBeforeTheMembershipStaysInTheCasesOwnThread(t *testing.T) {
	t.Parallel()
	r := newConversationRig(t)

	res, err := r.notifier.Evaluate(t.Context(), r.fx.scope, service.Intent{
		CaseID: r.fx.caseID, Reason: domain.ReasonFired, StateVersion: 1,
	})
	require.NoError(t, err)
	require.True(t, res.Created)

	kind, conv := conversationOf(t, r, res.Notification.ID)
	assert.Equal(t, "case", kind, "no membership yet, so ADR 0045's one conversation per Case")
	assert.Equal(t, r.fx.caseID, conv)
	assert.Equal(t, 1, threadsKeyedBy(t, r, "case"))
	assert.Zero(t, threadsKeyedBy(t, r, "incident"))
}

func TestACaseFactAfterTheMembershipPostsIntoTheIncidentsThread(t *testing.T) {
	t.Parallel()
	r := newConversationRig(t)
	r.member.in = true // the membership row has committed
	ctx := t.Context()

	res, err := r.notifier.Evaluate(ctx, r.fx.scope, service.Intent{
		CaseID: r.fx.caseID, Reason: domain.ReasonAllResolved, StateVersion: 1,
	})
	require.NoError(t, err)
	require.True(t, res.Created)

	kind, conv := conversationOf(t, r, res.Notification.ID)
	assert.Equal(t, "incident", kind)
	assert.Equal(t, r.facts.ID, conv, "the conversation is the Incident")

	// The SUBJECT did not move: the fact is still about the Case.
	var subjectKind string
	var caseID *uuid.UUID
	require.NoError(t, r.fx.pool.QueryRow(ctx,
		`SELECT subject_kind, case_id FROM notifications WHERE id = $1`, res.Notification.ID).
		Scan(&subjectKind, &caseID))
	assert.Equal(t, "case", subjectKind)
	require.NotNil(t, caseID)
	assert.Equal(t, r.fx.caseID, *caseID)

	assert.Equal(t, 1, threadsKeyedBy(t, r, "incident"), "the thread is keyed by the Incident")
	assert.Zero(t, threadsKeyedBy(t, r, "case"), "no Case thread, so no root of its own")

	// No root has landed on this channel, so the Incident's card is posted first and
	// the fact replies under it: post_root, then thread_reply, in that order.
	rows, err := r.fx.pool.Query(ctx,
		`SELECT mode FROM notification_deliveries WHERE notification_id = $1 ORDER BY thread_seq`,
		res.Notification.ID)
	require.NoError(t, err)
	defer rows.Close()
	var modes []string
	for rows.Next() {
		var m string
		require.NoError(t, rows.Scan(&m))
		modes = append(modes, m)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"post_root", "thread_reply"}, modes)
}

func TestAConversationIncidentsFactOpensItsThreadOnAThreadedChannel(t *testing.T) {
	t.Parallel()
	r := newConversationRig(t)
	ctx := t.Context()

	res, err := r.notifier.EvaluateIncident(ctx, r.fx.scope, service.IncidentIntent{
		IncidentID: r.facts.ID, Reason: domain.ReasonDrawn, OccasionID: id.New(),
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.Deliveries)

	var (
		mode, status string
		threadID     *uuid.UUID
	)
	require.NoError(t, r.fx.pool.QueryRow(ctx,
		`SELECT mode, status, thread_id FROM notification_deliveries WHERE notification_id = $1`,
		res.Notification.ID).Scan(&mode, &status, &threadID))
	assert.Equal(t, "post_root", mode)
	assert.NotEqual(t, "skipped", status, "a conversation is not skipped")
	require.NotNil(t, threadID)
	assert.Equal(t, 1, threadsKeyedBy(t, r, "incident"))
}

func TestEachCaseThatHadItsOwnThreadGetsOnePointerAndNothingMoves(t *testing.T) {
	t.Parallel()
	r := newConversationRig(t)
	ctx := t.Context()
	now := time.Now().UTC()

	// The Case's own thread, with its root landed — what a `fired` before the
	// membership left behind.
	th, err := r.threads.Ensure(ctx, r.fx.scope, r.fx.channel.ID, domain.SubjectCase, r.fx.caseID, now)
	require.NoError(t, err)
	seq, err := r.threads.AllocateSeq(ctx, r.fx.scope, th.ID, now)
	require.NoError(t, err)
	require.NoError(t, r.threads.RecordRoot(ctx, r.fx.scope, th.ID,
		"C0123456789", "1786000000.000100", id.New(), seq, now))

	declare := func(reason domain.Reason) {
		t.Helper()
		_, err := r.notifier.EvaluateIncident(ctx, r.fx.scope, service.IncidentIntent{
			IncidentID: r.facts.ID, Reason: reason, OccasionID: id.New(),
		})
		require.NoError(t, err)
	}
	declare(domain.ReasonDrawn)
	declare(domain.ReasonCaseAdded) // a second fact must not point twice

	var pointers int
	require.NoError(t, r.fx.pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications
		  WHERE org_id = $1 AND subject_kind = 'incident' AND conversation_kind = 'case'
		    AND conversation_id = $2`, r.fx.orgID, r.fx.caseID).Scan(&pointers))
	assert.Equal(t, 1, pointers, "one pointer per Case per Incident, ever")

	var mode string
	var onThread uuid.UUID
	require.NoError(t, r.fx.pool.QueryRow(ctx,
		`SELECT d.mode, d.thread_id FROM notification_deliveries d
		   JOIN notifications n ON n.id = d.notification_id
		  WHERE n.org_id = $1 AND n.subject_kind = 'incident' AND n.conversation_kind = 'case'`,
		r.fx.orgID).Scan(&mode, &onThread))
	assert.Equal(t, "thread_reply", mode)
	assert.Equal(t, th.ID, onThread, "the pointer is a reply in the Case's OWN thread")

	// Nothing already posted moved: the Case thread's root is where it was.
	after, err := r.threads.Get(ctx, r.fx.scope, th.ID)
	require.NoError(t, err)
	assert.Equal(t, "1786000000.000100", after.ProviderThreadID)
}

func TestAnIncidentThatIsNotAConversationPointsNowhere(t *testing.T) {
	t.Parallel()
	r := newConversationRig(t)
	r.facts.Conversation = false
	ctx := t.Context()

	// The rig's reader holds the facts by value, so rebuild it over the changed copy.
	channels := repository.NewChannelRepository(r.fx.pool)
	policies, err := service.NewPolicyService(policyStore{policy: domain.Policy{
		ID: r.fx.policyID, OrgID: r.fx.orgID, Name: "incidents", Priority: 1, Enabled: true,
		Reasons:    []domain.Reason{domain.ReasonDrawn},
		ChannelIDs: []uuid.UUID{r.fx.channel.ID},
	}}, channels)
	require.NoError(t, err)
	clk := clock.New()
	notifier, err := service.NewNotificationService(service.NotificationConfig{
		Tx: txRunner{pool: r.fx.pool}, Policies: policies,
		Notifications: repository.NewNotificationRepository(r.fx.pool),
		Deliveries:    repository.NewDeliveryRepository(r.fx.pool),
		Threads:       r.threads, Snapshots: snapshots{fx: r.fx},
		Events: repository.NewEventRepository(r.fx.pool, clk), Enqueuer: &enqueuer{},
		Channels: channels, Incidents: incidents{facts: r.facts}, Clock: clk,
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	th, err := r.threads.Ensure(ctx, r.fx.scope, r.fx.channel.ID, domain.SubjectCase, r.fx.caseID, now)
	require.NoError(t, err)
	seq, err := r.threads.AllocateSeq(ctx, r.fx.scope, th.ID, now)
	require.NoError(t, err)
	require.NoError(t, r.threads.RecordRoot(ctx, r.fx.scope, th.ID,
		"C0123456789", "1786000000.000200", id.New(), seq, now))

	_, err = notifier.EvaluateIncident(ctx, r.fx.scope, service.IncidentIntent{
		IncidentID: r.facts.ID, Reason: domain.ReasonDrawn, OccasionID: id.New(),
	})
	require.NoError(t, err)

	var pointers int
	require.NoError(t, r.fx.pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications
		  WHERE org_id = $1 AND subject_kind = 'incident' AND conversation_kind = 'case'`,
		r.fx.orgID).Scan(&pointers))
	assert.Zero(t, pointers)
	assert.Zero(t, threadsKeyedBy(t, r, "incident"))
}
