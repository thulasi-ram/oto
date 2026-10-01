package service_test

// ADR 0052 §5 — DECLARING AN INCIDENT IS A NOTIFICATION (git-bug aa6d18b).
//
// These run the REAL notification, delivery and policy-routing path over a REAL
// Postgres, because every clause of the ticket's done-when is a claim about a row:
// that a policy bound to `incident` writes one `notifications` row and one webhook
// delivery, that the row names no alert and no case (`notifications_subject_ck`'s
// fourth arm), that a redelivered fact is ONE notification (§C.7 over the
// occasion), and that an org with no such policy sends nothing. A fake store would
// agree with whatever the service handed it.
//
// The Incident itself comes through the `IncidentReader` port as a literal — the
// contract between the two modules is a value, exactly as `snapshots` is for Cases.

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
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// incidents answers the one question the notification layer asks the incidents
// side, for the one Incident the fixture names.
type incidents struct{ facts domain.IncidentFacts }

func (r incidents) Incident(_ context.Context, _ db.TenantScope, incidentID uuid.UUID) (domain.IncidentFacts, error) {
	if incidentID != r.facts.ID {
		return domain.IncidentFacts{}, assert.AnError
	}
	return r.facts, nil
}

func incidentFixtureFacts(caseID, alertID uuid.UUID) domain.IncidentFacts {
	drawn := time.Now().UTC().Add(-time.Minute)
	return domain.IncidentFacts{
		ID: id.New(), Number: 4, Active: true, DrawnAt: drawn, DrawnByLabel: "Priya R.",
		Members: []domain.IncidentMemberFacts{{
			CaseID: caseID, CaseNumber: 412, CaseOpen: true, AlertID: alertID,
			Alertname: "HighErrorRate",
			Labels:    map[string]string{"alertname": "HighErrorRate", "namespace": "checkout"},
			AddedAt:   drawn, AddedByLabel: "Priya R.",
		}},
	}
}

type incidentRig struct {
	fx       fixture
	facts    domain.IncidentFacts
	jobs     *enqueuer
	notifier *service.NotificationService
	views    *service.ViewService
}

// newIncidentRig routes through ONE policy: the catch-all "every Incident goes to
// the webhook" shape ADR 0052 §5 names, unless the caller passes another.
func newIncidentRig(t *testing.T, caps domain.Capability, policy func(fixture) domain.Policy) incidentRig {
	t.Helper()
	fx := newFixture(t, caps)
	facts := incidentFixtureFacts(fx.caseID, fx.alertID)

	channels := repository.NewChannelRepository(fx.pool)
	policies, err := service.NewPolicyService(policyStore{policy: policy(fx)}, channels)
	require.NoError(t, err)

	q := &enqueuer{}
	clk := clock.New()
	reader := incidents{facts: facts}
	notifier, err := service.NewNotificationService(service.NotificationConfig{
		Tx:            txRunner{pool: fx.pool},
		Policies:      policies,
		Notifications: repository.NewNotificationRepository(fx.pool),
		Deliveries:    repository.NewDeliveryRepository(fx.pool),
		Threads:       repository.NewThreadRepository(fx.pool),
		Snapshots:     snapshots{fx: fx},
		Events:        repository.NewEventRepository(fx.pool, clk),
		Enqueuer:      q,
		Channels:      channels,
		Incidents:     reader,
		Clock:         clk,
	})
	require.NoError(t, err)
	views, err := service.NewViewService(service.ViewConfig{
		Snapshots: snapshots{fx: fx}, BaseURL: "https://oto.example", Clock: clk, Incidents: reader,
	})
	require.NoError(t, err)
	return incidentRig{fx: fx, facts: facts, jobs: q, notifier: notifier, views: views}
}

func incidentPolicy(fx fixture) domain.Policy {
	return domain.Policy{
		ID: fx.policyID, OrgID: fx.orgID, Name: "every incident", Priority: 1, Enabled: true,
		Reasons: []domain.Reason{
			domain.ReasonDrawn, domain.ReasonCaseAdded, domain.ReasonCaseRemoved,
			domain.ReasonQuiet, domain.ReasonActiveAgain,
		},
		Subjects:   domain.SubjectBinding{domain.SubjectIncident},
		ChannelIDs: []uuid.UUID{fx.channel.ID},
	}
}

func dispatches(q *enqueuer) int {
	n := 0
	for _, j := range q.jobs {
		if _, ok := j.(jobs.DeliverDispatchArgs); ok {
			n++
		}
	}
	return n
}

func TestAPolicyBoundToIncidentRoutesTheFactToTheWebhook(t *testing.T) {
	t.Parallel()
	r := newIncidentRig(t, 0, incidentPolicy) // the generic webhook: no threading, no amend
	ctx := t.Context()

	intent := service.IncidentIntent{IncidentID: r.facts.ID, Reason: domain.ReasonDrawn, OccasionID: id.New()}
	res, err := r.notifier.EvaluateIncident(ctx, r.fx.scope, intent)
	require.NoError(t, err)
	require.True(t, res.Created)
	require.Equal(t, 1, res.Deliveries)
	require.Equal(t, 1, dispatches(r.jobs), "one webhook post, enqueued in the same transaction")

	// The row names the Incident and nothing narrower — `notifications_subject_ck`'s
	// fourth arm, read back from the table rather than from the struct.
	var (
		kind, conv string
		alertID    *uuid.UUID
		caseID     *uuid.UUID
		subject    uuid.UUID
	)
	require.NoError(t, r.fx.pool.QueryRow(ctx,
		`SELECT subject_kind, subject_id, conversation_kind, alert_id, case_id
		   FROM notifications WHERE id = $1`, res.Notification.ID).
		Scan(&kind, &subject, &conv, &alertID, &caseID))
	assert.Equal(t, "incident", kind)
	assert.Equal(t, r.facts.ID, subject)
	assert.Equal(t, "incident", conv)
	assert.Nil(t, alertID, "an Incident fact names no alert")
	assert.Nil(t, caseID, "an Incident fact names no case")

	var mode string
	require.NoError(t, r.fx.pool.QueryRow(ctx,
		`SELECT mode FROM notification_deliveries WHERE notification_id = $1`, res.Notification.ID).Scan(&mode))
	assert.Equal(t, "post_root", mode)

	// ⭐ EACH FACT IS ONE IDEMPOTENT NOTIFICATION. The redelivered job carries the same
	// occasion and is swallowed; a second happening carries a new one and is not.
	again, err := r.notifier.EvaluateIncident(ctx, r.fx.scope, intent)
	require.NoError(t, err)
	assert.False(t, again.Created)
	assert.Zero(t, again.Deliveries)
	assert.Equal(t, 1, dispatches(r.jobs))

	next, err := r.notifier.EvaluateIncident(ctx, r.fx.scope, service.IncidentIntent{
		IncidentID: r.facts.ID, Reason: domain.ReasonDrawn, OccasionID: id.New(),
	})
	require.NoError(t, err)
	assert.True(t, next.Created, "a second happening is a second fact")
}

// TestAnOrgWithNoIncidentPolicySendsNothing — the policy routes Case facts only, so
// the Incident fact is recorded as `no_policy` and nothing is enqueued.
func TestAnOrgWithNoIncidentPolicySendsNothing(t *testing.T) {
	t.Parallel()
	r := newIncidentRig(t, 0, func(fx fixture) domain.Policy {
		return domain.Policy{
			ID: fx.policyID, OrgID: fx.orgID, Name: "cases", Priority: 1, Enabled: true,
			Reasons:    []domain.Reason{domain.ReasonFired, domain.ReasonAllResolved},
			ChannelIDs: []uuid.UUID{fx.channel.ID},
		}
	})

	res, err := r.notifier.EvaluateIncident(t.Context(), r.fx.scope, service.IncidentIntent{
		IncidentID: r.facts.ID, Reason: domain.ReasonQuiet, OccasionID: id.New(),
	})
	require.NoError(t, err)
	assert.Equal(t, domain.SuppressedNoPolicy, res.Suppressed)
	assert.Zero(t, res.Deliveries)
	assert.Zero(t, dispatches(r.jobs), "an org with no Incident policy sends nothing")
}

// TestAThreadedDestinationIsSkippedWithItsReason — an Incident whose Correlator does
// not say its Incidents are conversations (here a human drew it, so there is no
// Correlator at all) is not a conversation (ADR 0052 §6), so the delivery is
// RECORDED as skipped with that sentence, no thread is opened, and no job is
// enqueued.
func TestAThreadedDestinationIsSkippedWithItsReason(t *testing.T) {
	t.Parallel()
	r := newIncidentRig(t, domain.CapThreading|domain.CapAmend, incidentPolicy)
	ctx := t.Context()

	res, err := r.notifier.EvaluateIncident(ctx, r.fx.scope, service.IncidentIntent{
		IncidentID: r.facts.ID, Reason: domain.ReasonCaseAdded, OccasionID: id.New(),
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.Deliveries)
	assert.Zero(t, dispatches(r.jobs))

	var status, why string
	require.NoError(t, r.fx.pool.QueryRow(ctx,
		`SELECT status, COALESCE(error, '') FROM notification_deliveries WHERE notification_id = $1`,
		res.Notification.ID).Scan(&status, &why))
	assert.Equal(t, "skipped", status)
	assert.Contains(t, why, "this Incident is not a conversation")

	var threads int
	require.NoError(t, r.fx.pool.QueryRow(ctx,
		`SELECT count(*) FROM channel_threads WHERE org_id = $1`, r.fx.orgID).Scan(&threads))
	assert.Zero(t, threads, "an Incident that is not a conversation opens no thread")
}

// TestTheIncidentCardIsBuiltFromTheIncident — the view the webhook renders carries
// the members with their labels and deep links, who drew it, and the derived state.
func TestTheIncidentCardIsBuiltFromTheIncident(t *testing.T) {
	t.Parallel()
	r := newIncidentRig(t, 0, incidentPolicy)
	ctx := t.Context()

	res, err := r.notifier.EvaluateIncident(ctx, r.fx.scope, service.IncidentIntent{
		IncidentID: r.facts.ID, Reason: domain.ReasonDrawn, OccasionID: id.New(),
	})
	require.NoError(t, err)

	v, err := r.views.Build(ctx, r.fx.scope, service.ViewRequest{Notification: res.Notification})
	require.NoError(t, err)
	require.NotNil(t, v.Incident)
	assert.Nil(t, v.Digest)
	assert.Nil(t, v.Case, "an Incident card names no single Case")
	assert.Equal(t, "drawn", v.Reason)
	assert.Equal(t, int64(4), v.Incident.Number)
	assert.Equal(t, "active", v.Incident.State)
	assert.Equal(t, "Priya R.", v.Incident.DrawnBy.Label)
	assert.Equal(t, "https://oto.example/incidents/4", v.Incident.Link)
	require.Len(t, v.Incident.Members, 1)
	m := v.Incident.Members[0]
	assert.Equal(t, "checkout", m.Labels["namespace"])
	assert.Equal(t, "https://oto.example/cases/"+r.fx.caseID.String(), m.Link)
}

// TestTheIncidentFactsAreTheFiveAndNoneIsACommand pins the vocabulary.
func TestTheIncidentFactsAreTheFiveAndNoneIsACommand(t *testing.T) {
	t.Parallel()
	var got []domain.Reason
	for _, r := range domain.AllReasons() {
		if r.Subject() == domain.SubjectIncident {
			got = append(got, r)
		}
	}
	assert.Equal(t, []domain.Reason{
		domain.ReasonDrawn, domain.ReasonCaseAdded, domain.ReasonCaseRemoved,
		domain.ReasonQuiet, domain.ReasonActiveAgain,
	}, got)
	for _, r := range got {
		for _, command := range []string{"resolve", "close", "mitigat", "status"} {
			assert.NotContains(t, string(r), command, "no Incident fact may mean resolve or close")
		}
		assert.True(t, r.NeedsOccasion(), "an Incident has no version; the occasion is its key")
	}
}
