package service_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

// These tests run against a REAL Postgres. Every defect they cover is a defect in
// the interaction between two SQL statements — an allocation in front of an
// `ON CONFLICT DO NOTHING`, a claim that must survive a COMMIT boundary — and a
// fake store cannot have it.
//
// The container, the migrations and the between-tests reset live in
// `test/harness` (ADR 0021 §1).

func TestMain(m *testing.M) { harness.Main(m) }

// fixture is one tenant with everything the FK graph insists on.
//
// ⚠️ The pool is PER FIXTURE, not a package global. These tests run with
// t.Parallel and the harness gives each one its own database, so a shared
// `testPool` would have every parallel test but the last one writing into
// somebody else's tenant — which showed up as `notifications_org_id_fkey:
// references a row that is gone`.
type fixture struct {
	pool  *pgxpool.Pool
	scope db.TenantScope
	orgID uuid.UUID
	// caseID is THE CONVERSATION.
	//
	// ⛔ IT WAS `groupID`, AN `alert_groups` GENERATION (git-bug `7570090`, migration
	// `00069`). Every test in this package addressed the group because the group owned
	// the thread; a conversation holds exactly one Case, so the Case owns it now and
	// this is the id every `Intent`, every `channel_threads` row and every
	// `notifications.conversation_id` below names.
	caseID   uuid.UUID
	alertID  uuid.UUID
	policyID uuid.UUID
	channel  domain.Channel
}

func newFixture(t *testing.T, caps domain.Capability) fixture {
	t.Helper()

	h := harness.New(t)
	org := h.Org()
	cluster := h.Cluster(org)
	alert := h.Alert(org, cluster)
	ac := h.Case(alert)

	var (
		channelID = id.New()
		policyID  = id.New()
		suffix    = org.ID.String()[:8]
	)

	// The timestamps come from the HARNESS CLOCK, like every other builder here,
	// and the columns are named because 00032 took the database's `DEFAULT now()`
	// away: `channels` has no clock of its own.
	//
	// This used to read `now() - interval '1 hour'`, and the backdating was
	// covering for a real defect rather than a test bug — `created_at` took the
	// DATABASE's clock while `SetHealth` wrote `updated_at` from the GO process's,
	// so an app server milliseconds behind its database failed the first delivery's
	// health write on `channels_time_ck` with a 500. On a Colima VM the two clocks
	// differ by exactly that much and this fixture failed roughly one run in two.
	// 00032 plus the monotonic `updated_at` writers fixed it where it lived, so the
	// fudge is gone: seed the honest instant and let the constraint mean something.
	// ADR 0047: a destination references an org-wide connection, and the column is
	// NOT NULL — the credential that used to sit on this row lives there now.
	conn := h.WebhookConnection(org)
	h.Exec(`INSERT INTO channels
	          (id, org_id, type, name, config, connection_id, capabilities, renderer,
	           thread_updates, created_at, updated_at)
	        VALUES ($1,$2,'webhook',$3,'{}'::jsonb,$4,$5,'webhook.json',true,$6,$6)`,
		channelID, org.ID, "chan-"+suffix, conn.ID, int64(caps), h.Now())
	// Same story on `notification_policies`, which 00034 took the database's
	// `DEFAULT now()` away from for the same reason.
	//
	// ⛔ THE SECOND REASON WAS `new_alerts`, WHICH IS DELETED (git-bug `7570090`): it
	// asserted a plurality, and a conversation holds one Case.
	//
	// ⭐ `all_resolved` IS ITS SUCCESSOR HERE, AND `domain/mode.go:hasReply` NAMES IT
	// AS SUCH RATHER THAN THIS BEING A GUESS: the tests that need a second reason need
	// one PROPERTY from it — a fact that amends the root card AND ALSO earns a thread
	// reply — and `all_resolved` is the Reason that inherited exactly that shape from
	// the two deleted ones. It has to be a Reason the DEFAULT verbosity admits a reply
	// for; `enriched` is in neither `replySets[status_changes]` nor `ungatedReplies`,
	// so it amends the root and drops the reply, and the two-row assertions would see
	// one row.
	h.Exec(`INSERT INTO notification_policies (id, org_id, name, priority, reasons, channel_ids,
	           created_at, updated_at)
	        VALUES ($1,$2,$3,1,ARRAY['fired','all_resolved'],ARRAY[$4::uuid],$5,$5)`,
		policyID, org.ID, "pol-"+suffix, channelID, h.Now())

	return fixture{
		pool:     h.Pool,
		scope:    org.Scope,
		orgID:    org.ID,
		caseID:   ac.ID,
		alertID:  alert.ID,
		policyID: policyID,
		channel: domain.Channel{
			ID:             channelID,
			OrgID:          org.ID,
			Type:           domain.ChannelTypeWebhook,
			Name:           "chan-" + suffix,
			Config:         json.RawMessage(`{}`),
			Capabilities:   caps,
			Renderer:       "webhook.json",
			Verbosity:      domain.VerbosityAll,
			ThreadUpdates:  true,
			ShowFieldEmoji: true,
			Enabled:        true,
			HealthStatus:   domain.HealthUnknown,
		},
	}
}

// txRunner is the real unit of work, so the advisory lock and the claim behave
// exactly as they do in production.
type txRunner struct{ pool *pgxpool.Pool }

func (r txRunner) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return db.Tx(ctx, r.pool, fn)
}

// enqueuer records job inserts instead of writing them, so a test can assert on
// what the transaction intended without running a queue.
type enqueuer struct{ jobs []db.JobArgs }

func (e *enqueuer) Enqueue(
	_ context.Context, args db.JobArgs, _ ...db.JobOption,
) (db.EnqueueResult, error) {
	e.jobs = append(e.jobs, args)
	return db.EnqueueResult{Kind: args.Kind()}, nil
}

func (e *enqueuer) EnqueueMany(
	_ context.Context, reqs []db.JobRequest,
) ([]db.EnqueueResult, error) {
	out := make([]db.EnqueueResult, 0, len(reqs))
	for _, r := range reqs {
		e.jobs = append(e.jobs, r.Args)
		out = append(out, db.EnqueueResult{Kind: r.Args.Kind()})
	}
	return out, nil
}

// snapshots answers the ONE question the notification module asks the alerts
// side. A literal is a complete substitute: the contract is a value.
type snapshots struct{ fx fixture }

func (s snapshots) Snapshot(
	_ context.Context, _ db.TenantScope, q domain.SnapshotQuery,
) (domain.Snapshot, error) {
	now := time.Now().UTC()
	return domain.Snapshot{
		Org: domain.OrgFacts{ID: s.fx.orgID, Slug: "org", Name: "Org"},
		Group: domain.GroupFacts{
			ID: q.CaseID, GroupKey: "", Generation: 1, Title: "A case",
			GroupLabels: map[string]string{"severity": "critical"},
			State:       "open", Severity: "critical", StateVersion: 1,
			FiringCount: 1, TotalCount: 1,
			FirstSeenAt: now, LastActivityAt: now,
		},
		MemberCount: 1,
		TakenAt:     now,
	}, nil
}

func nextSeqOf(t *testing.T, pool *pgxpool.Pool, threadID uuid.UUID) (next, lastSent int) {
	t.Helper()
	err := pool.QueryRow(t.Context(),
		`SELECT next_seq, last_sent_seq FROM channel_threads WHERE id = $1`, threadID).
		Scan(&next, &lastSent)
	require.NoError(t, err)
	return next, lastSent
}
