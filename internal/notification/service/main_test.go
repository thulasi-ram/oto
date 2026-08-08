package service_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/migrate"
)

// These tests run against a REAL Postgres. Every defect they cover is a defect in
// the interaction between two SQL statements — an allocation in front of an
// `ON CONFLICT DO NOTHING`, a claim that must survive a COMMIT boundary — and a
// fake store cannot have it.

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("oto"),
		tcpostgres.WithUsername("oto"),
		tcpostgres.WithPassword("oto"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres container: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = testcontainers.TerminateContainer(container) }()

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		os.Exit(1)
	}
	if err := migrate.Up(ctx, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
	if testPool, err = pgxpool.New(ctx, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "pool: %v\n", err)
		os.Exit(1)
	}
	defer testPool.Close()

	os.Exit(m.Run())
}

// fixture is one tenant with everything the FK graph insists on.
type fixture struct {
	scope    db.TenantScope
	orgID    uuid.UUID
	groupID  uuid.UUID
	policyID uuid.UUID
	channel  domain.Channel
}

func newFixture(t *testing.T, caps domain.Capability) fixture {
	t.Helper()
	ctx := t.Context()

	var (
		orgID     = uuid.New()
		clusterID = uuid.New()
		sourceID  = uuid.New()
		groupID   = uuid.New()
		channelID = uuid.New()
		policyID  = uuid.New()
		suffix    = orgID.String()[:8]
		now       = time.Now().UTC()
	)

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := testPool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}

	exec(`INSERT INTO orgs (id, slug, name) VALUES ($1,$2,$3)`,
		orgID, "org-"+suffix, "Org "+suffix)
	exec(`INSERT INTO clusters (id, org_id, cluster_key, display_name) VALUES ($1,$2,$3,$4)`,
		clusterID, orgID, "cl-"+suffix, "Cluster")
	exec(`INSERT INTO alert_sources (id, org_id, cluster_id, name, kind, base_url)
	      VALUES ($1,$2,$3,$4,'alertmanager','http://am.example.com')`,
		sourceID, orgID, clusterID, "src-"+suffix)
	exec(`INSERT INTO alert_groups
	        (id, org_id, source_id, cluster_id, group_key, title, state, group_labels,
	         first_seen_at, last_activity_at)
	      VALUES ($1,$2,$3,$4,$5,'A group','open','{"severity":"critical"}'::jsonb,$6,$6)`,
		groupID, orgID, sourceID, clusterID, groupKey(suffix), now)
	exec(`INSERT INTO channels
	        (id, org_id, type, name, config, capabilities, renderer, thread_updates)
	      VALUES ($1,$2,'webhook',$3,'{}'::jsonb,$4,'webhook.json',true)`,
		channelID, orgID, "chan-"+suffix, int64(caps))
	exec(`INSERT INTO notification_policies (id, org_id, name, priority, reasons, channel_ids)
	      VALUES ($1,$2,$3,1,ARRAY['fired','new_alerts'],ARRAY[$4::uuid])`,
		policyID, orgID, "pol-"+suffix, channelID)

	scope, err := db.NewTenantScope(orgID)
	require.NoError(t, err)

	return fixture{
		scope:    scope,
		orgID:    orgID,
		groupID:  groupID,
		policyID: policyID,
		channel: domain.Channel{
			ID:             channelID,
			OrgID:          orgID,
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

// groupKey satisfies groups_key_ck: `gk_` plus 26 Crockford-ish characters.
func groupKey(suffix string) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuv"
	out := make([]byte, 26)
	for i := range out {
		out[i] = alphabet[int(suffix[i%len(suffix)])%len(alphabet)]
	}
	return "gk_" + string(out)
}

// txRunner is the real unit of work, so the advisory lock and the claim behave
// exactly as they do in production.
type txRunner struct{ pool *pgxpool.Pool }

func (r txRunner) Tx(ctx context.Context, fn func(ctx context.Context) error) error {
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
			ID: q.GroupID, GroupKey: "gk", Generation: 1, Title: "A group",
			GroupLabels: map[string]string{"severity": "critical"},
			State:       "open", Severity: "critical", StateVersion: 1,
			FiringCount: 1, TotalCount: 1,
			FirstSeenAt: now, LastActivityAt: now,
		},
		MemberCount: 1,
		TakenAt:     now,
	}, nil
}

func nextSeqOf(t *testing.T, threadID uuid.UUID) (next, lastSent int) {
	t.Helper()
	err := testPool.QueryRow(t.Context(),
		`SELECT next_seq, last_sent_seq FROM channel_threads WHERE id = $1`, threadID).
		Scan(&next, &lastSent)
	require.NoError(t, err)
	return next, lastSent
}
