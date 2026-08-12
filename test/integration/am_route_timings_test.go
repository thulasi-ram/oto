package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/app"
	"github.com/thulasiram/oto/internal/platform/config"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/platform/migrate"
	"github.com/thulasiram/oto/internal/sources/domain"
	"github.com/thulasiram/oto/internal/sources/repository"
)

// ⭐ THIS FILE IS THE PROOF THAT 00028 ROUND-TRIPS FROM A FRESH DATABASE.
//
// The container is created, `goose up` runs every migration from nothing, and the
// real repository writes and re-reads the three Alertmanager timings through the
// real SQL. A unit test over the mapper would agree with whatever the mapper does
// and would not notice a column name that never made it into the INSERT, a CHECK
// that rejects a legal value, or a NULL that comes back as zero — which is the
// one failure mode that matters here, because zero and unknown mean opposite
// things for these three.

// seedSource writes the minimum an org/cluster/source needs for a health row.
func seedSource(t *testing.T, e *env) (db.TenantScope, *repository.SourceRepository, domain.SourceHealth) {
	t.Helper()

	orgID, clusterID, sourceID := id.New(), id.New(), id.New()
	// `created_at`/`updated_at` are NAMED: 00033 removed this table's DEFAULT
	// now(), because `orgs` timestamps come from the application's clock.
	if _, err := e.pool.Exec(e.ctx,
		`INSERT INTO orgs (id, slug, name, created_at, updated_at) VALUES ($1, $2, $3, $4, $4)`,
		// The slug carries the WHOLE id, hyphens stripped. The first eight hex
		// digits of a uuidv7 are the high bits of its millisecond timestamp, which
		// only turn over every ~65 seconds — so two orgs seeded inside one test
		// collided on `orgs_slug_key` and the failure read as a migration defect.
		orgID, "t"+strings.ReplaceAll(orgID.String(), "-", ""), "timings org",
		time.Now().UTC()); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	// `created_at`/`updated_at` are NAMED on both: 00034 removed their DEFAULT
	// now() for the reason 00033 removed `orgs`'.
	if _, err := e.pool.Exec(e.ctx,
		`INSERT INTO clusters (id, org_id, cluster_key, display_name, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $5)`,
		clusterID, orgID, "prod", "prod", time.Now().UTC()); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}

	if _, err := e.pool.Exec(e.ctx,
		`INSERT INTO alert_sources (id, org_id, cluster_id, name, kind, base_url,
		                            created_at, updated_at)
		 VALUES ($1, $2, $3, $4, 'alertmanager', 'http://am.test', $5, $5)`,
		sourceID, orgID, clusterID, "am-"+sourceID.String()[:8],
		time.Now().UTC()); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	scope, err := db.NewTenantScope(orgID)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	return scope, repository.NewSourceRepository(e.pool, e.container.Clock), domain.SourceHealth{
		SourceID: sourceID,
		OrgID:    orgID,
		Status:   domain.HealthHealthy,
	}
}

func dur(d time.Duration) *time.Duration { return &d }

// TestRouteTimingsRoundTripFromAFreshDatabase.
func TestRouteTimingsRoundTripFromAFreshDatabase(t *testing.T) {
	env := newEnv(t)
	scope, repo, health := seedSource(t, env)

	// 1. A source nobody has probed: every timing unknown, and the observed-at
	//    NULL. This is the state 00028 leaves every existing row in.
	if err := repo.SaveHealth(env.ctx, scope, health); err != nil {
		t.Fatalf("save an unprobed source: %v", err)
	}
	got, err := repo.GetHealth(env.ctx, scope, health.SourceID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.RouteTimingsAt != nil {
		t.Fatalf("observed_at = %v on a source nobody has probed", got.RouteTimingsAt)
	}
	if got.RouteTimings.Known() {
		t.Fatalf("timings %+v on a source nobody has probed", got.RouteTimings)
	}

	// 2. A probe that read the config. All three observed, plus the per-route
	//    caveat count.
	observedAt := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	health.RouteTimings = domain.RouteTimings{
		GroupWait:           dur(10 * time.Second),
		GroupInterval:       dur(30 * time.Second),
		RepeatInterval:      dur(4 * time.Hour),
		ChildRoutes:         4,
		ChildrenWithTimings: 2,
	}
	health.RouteTimingsAt = &observedAt
	if err := repo.SaveHealth(env.ctx, scope, health); err != nil {
		t.Fatalf("save observed timings: %v", err)
	}

	got, err = repo.GetHealth(env.ctx, scope, health.SourceID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.RouteTimings.GroupWait == nil || *got.RouteTimings.GroupWait != 10*time.Second {
		t.Fatalf("group_wait = %v", got.RouteTimings.GroupWait)
	}
	if got.RouteTimings.GroupInterval == nil || *got.RouteTimings.GroupInterval != 30*time.Second {
		t.Fatalf("group_interval = %v", got.RouteTimings.GroupInterval)
	}
	if got.RouteTimings.RepeatInterval == nil || *got.RouteTimings.RepeatInterval != 4*time.Hour {
		t.Fatalf("repeat_interval = %v", got.RouteTimings.RepeatInterval)
	}
	if got.RouteTimings.ChildRoutes != 4 || got.RouteTimings.ChildrenWithTimings != 2 {
		t.Fatalf("child routes %d/%d, want 2/4",
			got.RouteTimings.ChildrenWithTimings, got.RouteTimings.ChildRoutes)
	}
	if got.RouteTimingsAt == nil || !got.RouteTimingsAt.Equal(observedAt) {
		t.Fatalf("observed_at = %v, want %v", got.RouteTimingsAt, observedAt)
	}

	// 3. ⛔ THE ONE THAT WOULD LIE. A source whose config states NONE of the three
	//    is read back with three NULLs, not three zeros. Zero means `group_wait:
	//    0s` — "notify immediately" — and is a completely different statement about
	//    the upstream from "oto has not been told".
	health.RouteTimings = domain.RouteTimings{ChildRoutes: 1, ChildrenWithTimings: 1}
	if err := repo.SaveHealth(env.ctx, scope, health); err != nil {
		t.Fatalf("save an unstated config: %v", err)
	}
	got, err = repo.GetHealth(env.ctx, scope, health.SourceID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.RouteTimings.Known() {
		t.Fatalf("a config stating none of the three came back as %+v; NULL must stay NULL, "+
			"because 0 is a real Alertmanager setting", got.RouteTimings)
	}
	if got.RouteTimingsAt == nil {
		t.Fatal("observed_at was cleared: oto DID read this config and found nothing stated, " +
			"which is an observation")
	}

	// 4. A zero timing is a value, and survives as one.
	health.RouteTimings = domain.RouteTimings{GroupWait: dur(0)}
	if err := repo.SaveHealth(env.ctx, scope, health); err != nil {
		t.Fatalf("save a zero group_wait: %v", err)
	}
	got, err = repo.GetHealth(env.ctx, scope, health.SourceID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.RouteTimings.GroupWait == nil {
		t.Fatal("group_wait: 0s round-tripped as unknown")
	}
	if *got.RouteTimings.GroupWait != 0 {
		t.Fatalf("group_wait = %v, want 0", *got.RouteTimings.GroupWait)
	}
}

// TestTheHealthListReadsTheTimingsToo. The sources screen renders health for a
// page of sources through HealthFor, which has its own column list; a column
// added to one query and not the other is a field that is present on the detail
// page and mysteriously absent on the list.
func TestTheHealthListReadsTheTimingsToo(t *testing.T) {
	env := newEnv(t)
	scope, repo, health := seedSource(t, env)

	at := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	health.RouteTimings = domain.RouteTimings{GroupInterval: dur(5 * time.Minute)}
	health.RouteTimingsAt = &at
	if err := repo.SaveHealth(env.ctx, scope, health); err != nil {
		t.Fatalf("save: %v", err)
	}

	byID, err := repo.HealthFor(env.ctx, scope, []uuid.UUID{health.SourceID})
	if err != nil {
		t.Fatalf("list health: %v", err)
	}
	got, ok := byID[health.SourceID]
	if !ok {
		t.Fatal("the source is absent from the list read")
	}
	if got.RouteTimings.GroupInterval == nil || *got.RouteTimings.GroupInterval != 5*time.Minute {
		t.Fatalf("group_interval = %v on the list read", got.RouteTimings.GroupInterval)
	}
	if got.RouteTimingsAt == nil || !got.RouteTimingsAt.Equal(at) {
		t.Fatalf("observed_at = %v on the list read", got.RouteTimingsAt)
	}
}

// TestEveryMigrationDownTo00028IsReversible. Expand/contract (CONTEXT.md §6) is
// only a property if the contract half actually runs: a migration nobody has
// rolled back is a migration nobody can deploy on a Friday.
//
// It rolls the stack back to 00027 — every migration from the top down to and
// including 00028 — because `migrate.Down` reverts exactly one, and a test that
// pinned the count would have silently stopped testing the older ones the day a
// new one landed. The name says 00028 rather than a count for the same reason:
// the floor is what this test promises, and the ceiling moves.
//
// Each Down below is asserted by an OBSERVABLE PROPERTY flipping back, never by
// `migrate.Down` returning nil. A Down that runs cleanly and restores nothing is
// the failure this test exists to catch, and it is indistinguishable from a
// working one at the exit code.
//
// WHAT EACH DOWN HAS TO PUT BACK, newest first:
//
//   - 00043 changed two COMMENTs and nothing else — `alert_event_keys` and its
//     prune index — so its Down is two more COMMENTs and the property that flips
//     is the text `obj_description` returns. It is the one migration here with no
//     structure at all, which makes it the one whose Down is likeliest to be a
//     copy of its Up; and the text matters because the sentence it replaces
//     ("Pruned at created_at < now() - 30 days") was stated as fact from 00007
//     while nothing on earth pruned the table.
//
//   - 00042 added the two range indexes `stats.rollup` filters on,
//     `occ_started_idx` and `notif_created_idx`, so its Down is a pair of DROP
//     INDEXes and the property that flips is their presence in `pg_indexes`. An
//     index is the cheapest thing in this list to roll back — nothing to
//     backfill, no row it can make illegal — which is exactly why its Down is the
//     one most likely to be written from memory and never run. Whether the
//     indexes are USED is a different question and not one a round trip can
//     answer; that is asserted against a real plan in
//     internal/stats/repository/rollup_plan_test.go.
//
//   - ⭐ 00041 added `idempotency_claims`, so its Down is a DROP TABLE and the
//     property that flips is the table's existence. What is asserted on the way
//     back UP is the PRIMARY KEY's tuple, not merely the table: the four columns
//     (org, principal, operation, key) ARE the security property — a round trip
//     that restored the table with a narrower key would leave one tenant's or one
//     principal's key able to refuse another's create, and a round trip that
//     restored it without the key at all would let the same key be claimed twice
//     and mint the second credential the table exists to prevent. Nothing
//     references these rows and their horizon is 24 hours, so the Down loses no
//     history; what it loses is the protection, which is a property of rolling
//     back to a release that never claimed keys.
//
//   - ⭐ 00039 added `delivery_drills` and WIDENED `ingest_batches_mode_ck` to
//     admit `synthetic`. Its Down therefore does both halves — drop the table and
//     NARROW the CHECK back — and both are asserted, the CHECK out of
//     `pg_get_constraintdef` the way 00035's is. Narrowing a CHECK is the
//     dangerous half: ADR 0027 records it as a one-way door, because a rollback
//     past 00039 meets `synthetic` batches that the narrowed CHECK cannot admit.
//     That warning is EXERCISED rather than believed — a real synthetic batch is
//     written here, the Down is attempted against it and must REFUSE, and the
//     stack must still be at 00039 afterwards. A Down that dropped the table and
//     then failed on the CHECK would leave an operator with neither the old schema
//     nor the new one, and it would leave this test's remaining assertions running
//     against a database no migration describes.
//
//   - ⭐ 00038 dropped `alert_sources.reconcile_enabled`, so its Down is an ADD
//     COLUMN — the direction that has never been executed, since the column has
//     existed since 00004 for every database that has one. It is asserted ABSENT
//     at the top of the stack, PRESENT after the Down, and absent again after the
//     final Up: a Down that no-oped would be invisible at the exit code and would
//     leave a rolled-back release-N pod naming a column in its INSERT that the
//     database does not have.
//
//   - 00037 added `source_health.am_routes`. Its Down is a DROP COLUMN, asserted
//     gone on the way down and back on the way up.
//
//   - 00036 moved `oto_partitions_manage`'s `p_raw_retention_days` DEFAULT from
//     14 to 30. Its Down is a CREATE OR REPLACE that must land the OLD body,
//     defaults and all — so the function's argument list is introspected out of
//     `pg_proc` rather than trusted.
//
//   - ⭐ 00035 widened `ingest_rejections_reason_ck` with `invalid_label_value`
//     and `annotation_unstorable`. Its Down NARROWS the enum, which makes rows
//     already written under the widened one illegal — so before dropping the
//     constraint it REWRITES them to `undecodable` with the true reason
//     preserved at the front of `detail`. That row rewrite is the dangerous
//     part: `ingest_rejections` is the only place a rejected alert survives, the
//     rewrite runs during a rollback (when an operator is most likely to be
//     reading it), and a Down that deleted the rows instead would pass every
//     schema-shaped assertion. So real rows are written under the widened enum
//     here and read back after the Down.
//
//   - 00034, 00033 and 00032 take DEFAULTs away, so their Downs put them back,
//     and a restored default is what keeps a rolled-back release able to write
//     those tables at all.
//
//   - ⭐ 00030 is a DROP, so its Down is a CREATE, and that is the direction most
//     likely to be written carelessly and never run. Rolling it back here is what
//     proves an operator can undeploy the migration that removed a table.
//
//   - 00029 is an index and 00028 is the six route-timing columns this file is
//     otherwise about.
//
// The whole stack is migrated back up at the end, so the rest of the suite sees
// the schema it expects.
func TestEveryMigrationDownTo00028IsReversible(t *testing.T) {
	env := newEnv(t)
	dsn := env.cfg.DB.URL

	latest, err := migrate.Latest()
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest != 43 {
		t.Fatalf("latest migration is %d, want 43 — this test pins the number so that a "+
			"second migration claiming the same version is caught here. ⛔ Bumping this number "+
			"is HALF the change: the new migration's Down needs an assertion below, or the pin "+
			"is the only thing the new migration got and this test quietly shrank", latest)
	}

	// 00032 and 00033 took the DATABASE's clock away from three tables whose
	// other timestamps have always come from the application. Their Downs put the
	// defaults back, and that is the direction an operator runs at 02:00 — a DROP
	// DEFAULT whose reverse was never executed is exactly the one-line migration
	// everybody assumes works. Both states are asserted: here at the top of the
	// stack, and again below once the rollback loop has passed them.
	//
	// The two are counted separately because they roll back separately, and a
	// combined count would pass while one of the two Downs did nothing.
	clockDefaults := func(table string, columns ...string) int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx,
			// The identifier columns are `information_schema.sql_identifier`, a
			// domain over `name`, so both sides are cast to text: a bare parameter
			// would leave Postgres resolving `name = text[]` and erroring.
			`SELECT count(*) FROM information_schema.columns
			  WHERE table_name::text = $1 AND column_name::text = ANY($2::text[])
			    AND column_default IS NOT NULL`, table, columns).Scan(&n); err != nil {
			t.Fatalf("introspect %s defaults: %v", table, err)
		}
		return n
	}
	channelsDefaults := func() int {
		t.Helper()
		return clockDefaults("channels", "created_at", "updated_at")
	}
	appClockDefaults := func() int {
		t.Helper()
		return clockDefaults("orgs", "created_at", "updated_at") +
			clockDefaults("channel_credentials", "created_at")
	}
	// 00034 finished the sweep 00032 and 00033 started: twenty columns across
	// thirteen tables. Counted as one number because they are one migration and
	// therefore roll back together — unlike 00032 and 00033, which do not.
	remainingClockDefaults := func() int {
		t.Helper()
		return clockDefaults("users", "created_at", "updated_at") +
			clockDefaults("api_tokens", "created_at") +
			clockDefaults("sessions", "created_at") +
			clockDefaults("slack_identities", "created_at") +
			clockDefaults("clusters", "created_at", "updated_at") +
			clockDefaults("alert_sources", "created_at", "updated_at") +
			clockDefaults("source_health", "updated_at") +
			clockDefaults("ingest_dedup", "seen_at") +
			clockDefaults("notification_policies", "created_at", "updated_at") +
			clockDefaults("notifications", "created_at", "updated_at") +
			clockDefaults("notification_deliveries", "created_at", "updated_at") +
			clockDefaults("channel_threads", "created_at", "updated_at") +
			clockDefaults("silences", "mirrored_at")
	}
	// ⛔ The six tables 00034 deliberately did NOT touch. Their live writers OMIT
	// these columns, so a DEFAULT here is load-bearing rather than a trap, and an
	// over-enthusiastic follow-up that "finished the job" would break the ingest
	// path and the ui_events partition router. Asserted so that the exception is
	// pinned rather than remembered.
	keptDefaults := func() int {
		t.Helper()
		return clockDefaults("alerts", "created_at", "updated_at") +
			clockDefaults("alert_occurrences", "created_at", "updated_at") +
			clockDefaults("alert_groups", "created_at", "updated_at") +
			clockDefaults("alert_event_keys", "created_at") +
			clockDefaults("ui_events", "at") +
			clockDefaults("alert_snoozes", "created_at")
	}
	if channelsDefaults() != 0 {
		t.Fatal("channels.created_at or channels.updated_at still has a DEFAULT at migration 33; " +
			"00032 exists to take the database's clock off this table")
	}
	if appClockDefaults() != 0 {
		t.Fatal("orgs or channel_credentials still has a DEFAULT now() on a column the " +
			"application stamps; 00033 exists to take the database's clock off both tables")
	}
	if n := remainingClockDefaults(); n != 0 {
		t.Fatalf("%d column(s) still carry a DEFAULT now() that the repository already "+
			"supplies explicitly; 00034 exists to take the database's clock off all of them", n)
	}
	if n := keptDefaults(); n != 9 {
		t.Fatalf("the six tables 00034 deliberately left alone have %d defaults, want 9 — "+
			"their live writers OMIT these columns, so dropping one is not tidying, it is a "+
			"23502 on the ingest path or a ui_events row with no partition to go in", n)
	}

	// 00039's table and CHECK, 00038's dropped column, 00037's column, 00036's
	// function defaults and 00035's CHECK, introspected rather than assumed. Each
	// is read at the top of the stack and again after its own Down, because a Down
	// asserted only on the way back up would pass on a migration whose Up is
	// idempotent and whose Down does nothing.
	//
	// 00038's is the one where the top-of-stack read carries the weight: its Down
	// ADDS a column, so "present" is the interesting state and it only ever exists
	// between that Down and the Up that follows.
	reconcileEnabledColumns := func() int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx,
			`SELECT count(*) FROM information_schema.columns
			  WHERE table_name = 'alert_sources' AND column_name = 'reconcile_enabled'`).Scan(&n); err != nil {
			t.Fatalf("introspect alert_sources.reconcile_enabled: %v", err)
		}
		return n
	}
	drillTables := func() int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx,
			`SELECT count(*) FROM pg_tables WHERE tablename = 'delivery_drills'`).Scan(&n); err != nil {
			t.Fatalf("introspect delivery_drills: %v", err)
		}
		return n
	}
	// Read as rendered SQL rather than as a list of members, for the reason
	// `rejectionReasonCheck` is: the constraint name is the runtime contract, and
	// what matters is that the SAME name carries a different member list.
	batchModeCheck := func() string {
		t.Helper()
		var def string
		if err := env.pool.QueryRow(env.ctx,
			`SELECT pg_get_constraintdef(oid) FROM pg_constraint
			  WHERE conname = 'ingest_batches_mode_ck'
			    AND conrelid = 'ingest_batches'::regclass`).Scan(&def); err != nil {
			t.Fatalf("introspect ingest_batches_mode_ck: %v", err)
		}
		return def
	}
	amRoutesColumns := func() int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx,
			`SELECT count(*) FROM information_schema.columns
			  WHERE table_name = 'source_health' AND column_name = 'am_routes'`).Scan(&n); err != nil {
			t.Fatalf("introspect source_health.am_routes: %v", err)
		}
		return n
	}
	// `pronargs` is the count of IN arguments, so it selects the (int,int,int)
	// signature and would not silently pick up an overload. The rendered list also
	// carries the OUT columns of the RETURNS TABLE, hence a substring match on the
	// one default 00036 moves rather than an equality on the whole string.
	partitionsManageArgs := func() string {
		t.Helper()
		var args string
		if err := env.pool.QueryRow(env.ctx,
			`SELECT pg_get_function_arguments(oid) FROM pg_proc
			  WHERE proname = 'oto_partitions_manage' AND pronargs = 3`).Scan(&args); err != nil {
			t.Fatalf("introspect oto_partitions_manage: %v", err)
		}
		return args
	}
	rejectionReasonCheck := func() string {
		t.Helper()
		var def string
		if err := env.pool.QueryRow(env.ctx,
			`SELECT pg_get_constraintdef(oid) FROM pg_constraint
			  WHERE conname = 'ingest_rejections_reason_ck'
			    AND conrelid = 'ingest_rejections'::regclass`).Scan(&def); err != nil {
			t.Fatalf("introspect ingest_rejections_reason_ck: %v", err)
		}
		return def
	}
	claimTables := func() int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx,
			`SELECT count(*) FROM pg_tables WHERE tablename = 'idempotency_claims'`).Scan(&n); err != nil {
			t.Fatalf("introspect idempotency_claims: %v", err)
		}
		return n
	}
	// Read as rendered SQL, like the CHECKs above: the tuple is what carries the
	// property, and a PK under the same name with fewer columns in it is the
	// failure worth catching.
	claimKeyTuple := func() string {
		t.Helper()
		var def string
		if err := env.pool.QueryRow(env.ctx,
			`SELECT pg_get_constraintdef(oid) FROM pg_constraint
			  WHERE conname = 'idempotency_claims_pk'
			    AND conrelid = 'idempotency_claims'::regclass`).Scan(&def); err != nil {
			t.Fatalf("introspect idempotency_claims_pk: %v", err)
		}
		return def
	}
	// 00042's two indexes, counted together because they are one migration and
	// therefore roll back together. `pg_indexes` rather than `pg_class` so a name
	// that came back as something other than an index would not count.
	rollupRangeIndexes := func() int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx,
			`SELECT count(*) FROM pg_indexes
			  WHERE indexname = ANY($1::text[])`,
			[]string{"occ_started_idx", "notif_created_idx"}).Scan(&n); err != nil {
			t.Fatalf("introspect the rollup range indexes: %v", err)
		}
		return n
	}
	// 00043's two comments, read as text because text is all they are. The table
	// comment is the one that carries the property: it stated a 30-day pruner as
	// FACT from 00007 until `retention.prune` finally swept this table, and the
	// index comment is what stops the next reader wondering what
	// `alert_event_keys_prune_idx` is for.
	eventKeyComments := func() (table, index string) {
		t.Helper()
		if err := env.pool.QueryRow(env.ctx,
			`SELECT coalesce(obj_description('alert_event_keys'::regclass, 'pg_class'), ''),
			        coalesce(obj_description('alert_event_keys_prune_idx'::regclass, 'pg_class'), '')`).
			Scan(&table, &index); err != nil {
			t.Fatalf("introspect the alert_event_keys comments: %v", err)
		}
		return table, index
	}
	snapshotUniqCols := func() string {
		t.Helper()
		var def string
		if err := env.pool.QueryRow(env.ctx,
			`SELECT pg_get_constraintdef(oid) FROM pg_constraint
			  WHERE conname = 'rule_snapshots_content_uniq'
			    AND conrelid = 'rule_snapshots'::regclass`).Scan(&def); err != nil {
			t.Fatalf("introspect rule_snapshots_content_uniq: %v", err)
		}
		return def
	}

	// ⭐ 00043 is comments and nothing else, which is exactly why it is asserted:
	// a migration with no structure to introspect is the one whose Down is most
	// likely to be a copy of its Up. The table comment promised a 30-day pruner as
	// fact for thirty-six migrations while nothing swept the table, so the sentence
	// an operator reads at `\d+` is the deliverable here.
	if table, index := eventKeyComments(); !strings.Contains(table, "retention.prune") {
		t.Fatalf("alert_event_keys still describes its own pruning as %q at the top of the "+
			"stack — 00043 exists to name the job that finally does it, and to stop the comment "+
			"claiming a flat 30 days when the sweep widens to the longest raw_retention_days any "+
			"tenant configured", table)
	} else if !strings.Contains(index, "retention.prune") {
		t.Fatalf("alert_event_keys_prune_idx carries %q at the top of the stack — the index has "+
			"existed since 00007 to serve one query that did not exist, and 00043 is where it "+
			"finally names the one it does serve", index)
	}

	// 00042's two indexes. Asserted at the top of the stack as well as after the
	// round trip because "the Down dropped them" is only interesting if the Up put
	// them there in the first place.
	if n := rollupRangeIndexes(); n != 2 {
		t.Fatalf("%d of 00042's two range indexes exist at the top of the stack, want 2 — "+
			"occ_started_idx and notif_created_idx are the only indexes either table has that "+
			"lead with (org_id, timestamp), and without them stats.rollup scans both tables in "+
			"full, twice per org, every fifteen minutes", n)
	}

	// ⭐ 00041's table, and the tuple that makes it worth having. The four columns
	// are asserted here rather than only after the round trip because "the table
	// exists" is the weakest possible reading of this migration: a claim keyed on
	// less than (org, principal, operation, key) is a claim that refuses somebody
	// else's request, and one keyed on more would never recognise a retry at all.
	if claimTables() != 1 {
		t.Fatal("idempotency_claims is absent at the top of the stack; 00041 exists to create it, " +
			"and without it every `Idempotency-Key` the contract declares is read by nothing")
	}
	for _, column := range []string{"org_id", "principal_id", "operation", "idempotency_key"} {
		if def := claimKeyTuple(); !strings.Contains(def, column) {
			t.Fatalf("idempotency_claims_pk does not carry %s at the top of the stack: %s — the "+
				"four-column tuple IS the property: without org_id two tenants collide, without "+
				"principal_id one org member's key refuses another's, and without operation one "+
				"key claimed on a create refuses the revoke of the same gesture", column, def)
		}
	}

	// ⛔ 00040 WIDENS `rule_snapshots_content_uniq` from (org, source,
	// fingerprint) to include the rule key, and its Down is LOSSY IN ONE
	// DIRECTION ONLY. Widening cannot fail: no existing row can violate a
	// constraint with more columns in it. Narrowing can, because the whole point
	// of e670d5b was that an `unavailable` capture has an empty expr and therefore
	// hashes identically for every rule in a source — so the rows the Up allows to
	// exist separately are exactly the rows the Down must fold back together.
	//
	// The assertion here is on the TOP of the stack; the Down is exercised by the
	// rollback below, and a Down that silently dropped the constraint rather than
	// restoring the narrow one would leave the fold undone and let a re-Up succeed
	// against data the original schema forbade.
	if def := snapshotUniqCols(); !strings.Contains(def, "rule_name") {
		t.Fatalf("rule_snapshots_content_uniq does not carry the rule key at the top of the "+
			"stack: %s — 00040 exists to widen it, and without the key every unrecoverable "+
			"rule in a source collapses into one row named after whichever failed first", def)
	}
	if drillTables() != 1 {
		t.Fatal("delivery_drills is absent at the top of the stack; 00039 exists to create it")
	}
	if def := batchModeCheck(); !strings.Contains(def, "synthetic") {
		t.Fatalf("ingest_batches_mode_ck does not admit synthetic at the top of the stack: %s — "+
			"00039 exists to widen it, and the drill endpoint is the only writer of that mode", def)
	}
	if n := reconcileEnabledColumns(); n != 0 {
		t.Fatal("alert_sources.reconcile_enabled is still present at the top of the stack; 00038 " +
			"exists to drop it, and while it is there the reaper can still be made to trust a " +
			"frozen health verdict")
	}
	if amRoutesColumns() != 1 {
		t.Fatal("source_health.am_routes is absent at the top of the stack; 00037 exists to add it")
	}
	if args := partitionsManageArgs(); !strings.Contains(args, "p_raw_retention_days integer DEFAULT 30") {
		t.Fatalf("oto_partitions_manage(%s) does not carry 00036's raw-retention default of 30", args)
	}

	// ⭐ 00039's Down NARROWS `ingest_batches_mode_ck`, and ADR 0027 calls the
	// widening a one-way door: a rollback past 00039 meets `synthetic` batches the
	// narrowed CHECK cannot admit. That is a claim about what happens to a real
	// database, so a real synthetic batch is written here — under the widened
	// CHECK, which is the state an operator's database is in at the moment they
	// decide to roll back — and the Down is attempted against it below.
	//
	// `ingest_batches` is PARTITION BY RANGE (received_at) with daily partitions
	// and 00006 creates today plus the next seven, so a `now()` received_at lands
	// in a partition that exists. `org_id`/`source_id` carry no FK on this table,
	// which is why a batch can be recorded before anything else is known about it.
	syntheticBatch := id.New()
	if _, err := env.pool.Exec(env.ctx,
		`INSERT INTO ingest_batches (id, org_id, source_id, mode, received_at, body_bytes,
		                             checksum, dedup_key, alert_count, payload)
		 VALUES ($1, $2, $3, 'synthetic', $4, 512, decode($5, 'hex'), $5, 1,
		         '{"alerts":[]}'::jsonb)`,
		syntheticBatch, id.New(), id.New(), time.Now().UTC(), strings.Repeat("ab", 32)); err != nil {
		t.Fatalf("record a synthetic batch at the top of the stack: %v — 00039 exists to make "+
			"this mode writable, so a 23514 here means its Up never widened the CHECK", err)
	}

	// ⭐ 00035's Down rewrites rows, so it needs rows. These two are written under
	// the WIDENED enum — version N+1's own output — which is exactly the state an
	// operator's database is in at the moment they decide to roll back.
	//
	// `ingest_rejections` is PARTITION BY RANGE (received_at) with daily
	// partitions, and 00006 creates today plus the next seven, so a `now()`
	// received_at lands in a partition that exists. `raw` is NOT NULL and so are
	// the three ids; `batch_id` is legitimately NULL here, because a rejection
	// that never reached a batch has no batch to point at.
	rejections := []struct {
		id             uuid.UUID
		reason, detail string
	}{
		{id.New(), "invalid_label_value", "label instance carries U+0000 at byte 3"},
		{id.New(), "annotation_unstorable", "annotation description: 2 code points replaced with U+FFFD"},
	}
	for _, r := range rejections {
		if _, err := env.pool.Exec(env.ctx,
			`INSERT INTO ingest_rejections (id, org_id, source_id, received_at, reason, detail, raw)
			 VALUES ($1, $2, $3, $4, $5, $6, '{"labels":{"alertname":"x"}}'::jsonb)`,
			r.id, id.New(), id.New(), time.Now().UTC(), r.reason, r.detail); err != nil {
			t.Fatalf("record a %s rejection at migration 35: %v — 00035 exists to make this "+
				"reason writable, so a 23514 here means its Up never widened the CHECK", r.reason, err)
		}
	}

	// Migrations land concurrently, and a migration this test has never heard of
	// must not be rolled back unasserted underneath an assertion meant for another
	// one. So the rollback is stepped by NAME: `down` refuses to move unless the
	// top applied version is the one the next assertion is about.
	appliedTop := func() int64 {
		t.Helper()
		st, err := migrate.Statuses(env.ctx, dsn)
		if err != nil {
			t.Fatalf("statuses: %v", err)
		}
		var top int64
		for _, s := range st {
			if s.Applied && s.Version > top {
				top = s.Version
			}
		}
		return top
	}
	// down rolls back exactly one migration and names the version it BELIEVED it
	// was undoing, so an assertion that has drifted out of order fails as a
	// drifted assertion rather than as a baffling introspection result three
	// steps later.
	down := func(want int64) {
		t.Helper()
		if top := appliedTop(); top != want {
			t.Fatalf("about to roll back %s, but the top applied migration is %s",
				migrate.FormatVersion(want), migrate.FormatVersion(top))
		}
		if err := migrate.Down(env.ctx, dsn); err != nil {
			t.Fatalf("goose down %s: %v", migrate.FormatVersion(want), err)
		}
	}

	// 00042 down: both range indexes go. `DROP INDEX IF EXISTS` is the kind of
	// statement that runs cleanly whether or not it does anything, which is
	// precisely why the property is read afterwards instead of the error being
	// trusted — a Down that named a typo'd index would be green at the exit code
	// and would leave a rolled-back deployment carrying indexes for a query the
	// release it rolled back to does not run.
	// 00043 down: both comments go back to what 00007 wrote, and the index comment
	// back to nothing. A Down that simply left the new text would be green on every
	// structural assertion in this file, which is the whole reason a comment-only
	// migration is worth rolling back at all: the rolled-back release does NOT
	// sweep this table, and a comment that says it does would send an operator
	// looking for a job that is not running.
	down(43)
	if table, index := eventKeyComments(); strings.Contains(table, "retention.prune") {
		t.Fatalf("alert_event_keys still names retention.prune after 00043's Down: %q — the "+
			"release this rolls back to does not prune the table, so the comment would be the "+
			"same false promise 00007 shipped, now pointing at a job by name", table)
	} else if index != "" {
		t.Fatalf("alert_event_keys_prune_idx kept its comment %q after 00043's Down", index)
	}

	down(42)
	if n := rollupRangeIndexes(); n != 0 {
		t.Fatalf("%d of 00042's two range indexes survived its Down, want 0", n)
	}

	// 00041 down: the claims table goes, and it has to go WITH ROWS IN IT. A claim
	// is written first because that is the only state an operator ever rolls this
	// back from — the table is useless empty — and because a Down that tried to
	// preserve the rows, or that had left a dependency behind, would fail here
	// rather than in production at 02:00. Nothing references these rows and their
	// horizon is 24 hours, so dropping them loses no history; what it loses is the
	// protection, and that is a property of running a release that never claimed
	// keys rather than of this statement.
	claimOrg, _, _ := seedSource(t, env)
	if _, err := env.pool.Exec(env.ctx,
		`INSERT INTO idempotency_claims (org_id, principal_id, operation, idempotency_key,
		                                 request_hash, created_ref, claimed_at)
		 VALUES ($1, $2, 'createApiToken', $3, decode($4, 'hex'), $5, now())`,
		claimOrg.OrgID(), id.New(), "01JD8Z2K7M3TQ9YB4V6H0XW5RE", strings.Repeat("ab", 32),
		id.New()); err != nil {
		t.Fatalf("claim a key at the top of the stack: %v — 00041 exists to make this row "+
			"writable, so a failure here means its Up never created the table it describes", err)
	}
	down(41)
	if claimTables() != 0 {
		t.Fatal("idempotency_claims survived 00041's Down, so a rolled-back release keeps a table " +
			"nothing writes, nothing reads and `retention.prune` no longer sweeps — rows with a " +
			"24-hour horizon that now live forever")
	}

	// ⭐ 00040's Down FOLDS ROWS, so it needs rows — and it needs the exact rows
	// that make the fold interesting, written under the WIDE constraint, which is
	// the state an operator's database is in at the moment they decide to roll
	// back. Asserting only the constraint text after the rollback tested a DELETE
	// against an empty table: a Down that folded on the WRONG key, or that folded
	// nothing, or that let `occ_rule_fk` blank every occurrence's bound rule,
	// would all have been green.
	//
	// The three snapshots are e670d5b's own scenario. Two `unavailable` captures
	// of DIFFERENT rules share a content address, because an unavailable capture
	// is an empty expr, zero durations and empty maps and there is nothing else in
	// the digest (SPEC §C.6) — under the narrow tuple they are one row, which is
	// the defect 00040 fixed, and the Down has to put them back into one. The
	// third is a real recovered rule that happens to share a rule_name with the
	// first, so a Down that folded on the rule KEY rather than on the content
	// address would delete it and be caught here.
	foldOrg, _, foldHealth := seedSource(t, env)
	foldSource := foldHealth.SourceID
	var foldCluster uuid.UUID
	if err := env.pool.QueryRow(env.ctx,
		`SELECT cluster_id FROM alert_sources WHERE id = $1`, foldSource).Scan(&foldCluster); err != nil {
		t.Fatalf("read the seeded source's cluster: %v", err)
	}

	emptyFP := strings.Repeat("ab", 32) // every unavailable capture's address
	recoveredFP := strings.Repeat("cd", 32)
	base := time.Now().UTC().Add(-time.Hour)

	// (id, rule_name, fingerprint, captured_at) — `survivor` is the earliest row
	// per (org, source, fingerprint), which is the row the pre-00040 upsert would
	// itself have kept as the incumbent.
	survivor, folded, untouched := id.New(), id.New(), id.New()
	snapshots := []struct {
		snapID     uuid.UUID
		name, fp   string
		origin     string
		expr       string
		promURL    any
		confidence string
		candidates int
		at         time.Time
	}{
		{survivor, "AlertA", emptyFP, "unavailable", "", nil, "none", 0, base},
		{folded, "AlertB", emptyFP, "unavailable", "", nil, "none", 0, base.Add(time.Minute)},
		{untouched, "AlertA", recoveredFP, "prometheus_api", "up == 0",
			"http://prom.test", "exact", 1, base.Add(2 * time.Minute)},
	}
	for _, s := range snapshots {
		if _, err := env.pool.Exec(env.ctx,
			`INSERT INTO rule_snapshots (id, org_id, source_id, rule_fingerprint, rule_name,
			                             expr, origin, prometheus_url, match_confidence,
			                             candidate_count, captured_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			s.snapID, foldOrg.OrgID(), foldSource, s.fp, s.name, s.expr, s.origin, s.promURL,
			s.confidence, s.candidates, s.at); err != nil {
			t.Fatalf("store the %s/%s snapshot under the wide constraint: %v — 00040 exists to "+
				"make two rules with one content address storable, so a 23505 here means its Up "+
				"never widened the tuple", s.name, s.origin, err)
		}
	}

	// One occurrence bound to the row that is about to be folded away, and one
	// bound to a row that survives. `occ_open_uniq` allows at most one open
	// occurrence per alert, so they hang off two alerts.
	boundToFolded, boundToUntouched := id.New(), id.New()
	occurrences := []struct {
		occID, snapID uuid.UUID
		alertname     string
	}{
		{boundToFolded, folded, "AlertB"},
		{boundToUntouched, untouched, "AlertA"},
	}
	for i, o := range occurrences {
		alertID := id.New()
		if _, err := env.pool.Exec(env.ctx,
			`INSERT INTO alerts (id, org_id, cluster_id, alert_key, source_fingerprint, alertname,
			                     cluster_key, labels, state, first_seen_at, last_seen_at,
			                     last_state_change_at)
			 VALUES ($1, $2, $3, $4, $5, $6, 'prod', $7::jsonb, 'firing', $8, $8, $8)`,
			alertID, foldOrg.OrgID(), foldCluster,
			"ak_"+strings.Repeat(string(rune('a'+i)), 26), strings.Repeat("0f", 8), o.alertname,
			`{"alertname":"`+o.alertname+`"}`, base); err != nil {
			t.Fatalf("seed the alert behind %s: %v", o.alertname, err)
		}
		if _, err := env.pool.Exec(env.ctx,
			`INSERT INTO alert_occurrences (id, org_id, alert_id, seq, state, started_at,
			                                last_observed_at, source_starts_at, rule_snapshot_id)
			 VALUES ($1, $2, $3, 1, 'firing', $4, $4, $4, $5)`,
			o.occID, foldOrg.OrgID(), alertID, base, o.snapID); err != nil {
			t.Fatalf("bind an occurrence of %s to its rule snapshot: %v", o.alertname, err)
		}
	}

	// 00040 down: the rule-snapshot uniqueness tuple narrows back to (org, source,
	// fingerprint).
	//
	// ⛔ THIS DIRECTION IS LOSSY AND THE UP IS NOT. Widening cannot fail — no row
	// can violate a constraint with more columns in it — but narrowing folds
	// together exactly the rows 00040 exists to keep apart: an `unavailable`
	// capture has an empty expr, so every unrecoverable rule in a source hashes
	// identically, which is the whole of e670d5b. The Down therefore keeps the
	// earliest row per content address, drops the rest, and REMAPS the
	// occurrences bound to the dropped rows onto the survivor first — every
	// folded row is byte-identical in definition to the row it folds into, so
	// letting `occ_rule_fk` null the pointer instead would discard "what the rule
	// said when this fired" for an answer sitting one row away.
	//
	// It is asserted here rather than trusted because a Down that DROPPED the
	// constraint instead of restoring the narrow one would leave the fold undone,
	// and a later re-Up would then succeed against data the original schema forbade.
	down(40)
	if def := snapshotUniqCols(); strings.Contains(def, "rule_name") {
		t.Fatalf("rule_snapshots_content_uniq still carries the rule key after 00040's Down: %s — "+
			"the narrow constraint has to come back, or a rollback leaves a schema that admits "+
			"rows 00039 and earlier could never have stored", def)
	}
	if !strings.Contains(snapshotUniqCols(), "rule_fingerprint") {
		t.Fatalf("rule_snapshots_content_uniq is not the narrow (org, source, fingerprint) tuple "+
			"after 00040's Down: %s", snapshotUniqCols())
	}

	// The fold itself: two rows out of three, and the RIGHT two.
	snapshotSurvives := func(snapID uuid.UUID) bool {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx,
			`SELECT count(*) FROM rule_snapshots WHERE id = $1`, snapID).Scan(&n); err != nil {
			t.Fatalf("introspect rule_snapshots after the fold: %v", err)
		}
		return n == 1
	}
	if !snapshotSurvives(survivor) {
		t.Fatal("00040's Down deleted the EARLIEST row of the folded pair; the survivor has to be " +
			"the one the narrow constraint's own upsert would have kept as the incumbent, or a " +
			"rollback and a re-Up disagree about which capture is version 1")
	}
	if snapshotSurvives(folded) {
		t.Fatal("00040's Down left both unavailable captures in place, so the narrow constraint " +
			"was re-added over rows that violate it — either the fold DELETE did nothing, or it " +
			"folded on a key that keeps them apart, which is the key the narrow tuple does not have")
	}
	if !snapshotSurvives(untouched) {
		t.Fatal("00040's Down deleted a snapshot with its OWN content address, which no narrowing " +
			"of (org, source, fingerprint) can require — the fold is running on the rule key, not " +
			"on the content address, and it is destroying real captured rules")
	}

	// ⭐ AND THE OCCURRENCES STILL KNOW WHAT THE RULE SAID. This is the assertion
	// the `ON DELETE SET NULL` would fail: the folded row and its survivor carry
	// the same rule_fingerprint and therefore byte-identical text, so unbinding
	// loses the one fact the product exists to show for no reason at all.
	boundSnapshot := func(occID uuid.UUID) *uuid.UUID {
		t.Helper()
		var out *uuid.UUID
		if err := env.pool.QueryRow(env.ctx,
			`SELECT rule_snapshot_id FROM alert_occurrences WHERE id = $1`, occID).Scan(&out); err != nil {
			t.Fatalf("read the occurrence's bound snapshot: %v", err)
		}
		return out
	}
	switch got := boundSnapshot(boundToFolded); {
	case got == nil:
		t.Fatal("the occurrence bound to the folded snapshot came out of 00040's Down unbound — " +
			"occ_rule_fk is ON DELETE SET NULL, so a Down that deletes before it remaps throws " +
			"away the rule text behind a fired alert while an identical copy of that text " +
			"survives in the row it was folded into")
	case *got != survivor:
		t.Fatalf("the occurrence bound to the folded snapshot now points at %s, want the survivor "+
			"%s", *got, survivor)
	}
	if got := boundSnapshot(boundToUntouched); got == nil || *got != untouched {
		t.Fatalf("an occurrence bound to a snapshot that was never folded came out of the Down "+
			"pointing at %v; the remap must touch only the rows being deleted", got)
	}

	// ⛔ 00039 down, ATTEMPT ONE: REFUSED, because a synthetic batch is live. This
	// is the assertion that turns ADR 0027's warning from a sentence into a
	// property — and the failure has to be ATOMIC, because 00039's Down drops the
	// table BEFORE it narrows the CHECK. A non-transactional Down would leave the
	// operator with `delivery_drills` gone, the CHECK still wide, and goose still
	// claiming 00039 is applied: neither schema, and nothing to roll forward to.
	if top := appliedTop(); top != 39 {
		t.Fatalf("about to attempt %s against a live synthetic batch, but the top applied "+
			"migration is %s", migrate.FormatVersion(39), migrate.FormatVersion(top))
	}
	refusal := migrate.Down(env.ctx, dsn)
	if refusal == nil {
		t.Fatal("00039's Down succeeded with a live synthetic batch in ingest_batches — either " +
			"the narrowed CHECK is not being re-added, or it was added NOT VALID; ADR 0027 " +
			"records this rollback as a one-way door precisely because the surviving rows " +
			"violate it, and a Down that quietly accepts them leaves a table whose own " +
			"constraint is a lie")
	}
	// Named, so that this step cannot start passing on an unrelated failure — a
	// Down that broke for any other reason would otherwise look like the refusal
	// this asserts.
	if !strings.Contains(refusal.Error(), "ingest_batches_mode_ck") {
		t.Fatalf("00039's Down failed, but not on the narrowed CHECK: %v", refusal)
	}
	if top := appliedTop(); top != 39 {
		t.Fatalf("00039 is recorded as %s after a FAILED Down — the rollback is not atomic, so "+
			"the database is in a state no migration describes", migrate.FormatVersion(top))
	}
	if drillTables() != 1 {
		t.Fatal("delivery_drills was dropped by a Down that then failed; the DROP TABLE and the " +
			"CHECK narrowing have to succeed or fail together")
	}
	if def := batchModeCheck(); !strings.Contains(def, "synthetic") {
		t.Fatalf("ingest_batches_mode_ck was narrowed by a Down that then failed: %s", def)
	}

	// What ADR 0027 tells the operator to do first. `retention.prune` disposes of a
	// drill's rows by id in production; here one DELETE stands in for it, because
	// what is under test is the migration, not the reaper.
	if _, err := env.pool.Exec(env.ctx,
		`DELETE FROM ingest_batches WHERE id = $1`, syntheticBatch); err != nil {
		t.Fatalf("dispose of the synthetic batch: %v", err)
	}

	// 00039 down: the drills table goes AND the CHECK narrows. Both halves are
	// asserted, because a Down that dropped the table and left the widened CHECK
	// would pass a table-shaped assertion while leaving release N able to write a
	// mode it has never heard of.
	down(39)
	if drillTables() != 0 {
		t.Fatal("delivery_drills survived 00039's Down, so a release-N deployment keeps a table " +
			"whose rows nothing will ever finish, dispose of, or read")
	}
	if def := batchModeCheck(); strings.Contains(def, "synthetic") {
		t.Fatalf("ingest_batches_mode_ck still admits synthetic after 00039's Down: %s — the "+
			"constraint name is a runtime contract (returned as errs.Error.Code on a 23514), so "+
			"it must be re-added under the same name with the OLD member list", def)
	}

	// ⭐ 00038 down: the column comes back. This is the interesting direction —
	// 00038's Up is a DROP COLUMN, so its Down is an ADD COLUMN that no database
	// has ever executed, and it has to arrive NOT NULL DEFAULT true or a
	// rolled-back release-N pod meets a 23502 on the first source it writes.
	down(38)
	if n := reconcileEnabledColumns(); n != 1 {
		t.Fatal("alert_sources.reconcile_enabled did not come back on 00038's Down; the Down of " +
			"a DROP COLUMN is an ADD COLUMN, and one that never runs is the whole reason this " +
			"test asserts the state rather than the exit code")
	}
	var nullable, colDefault string
	if err := env.pool.QueryRow(env.ctx,
		`SELECT is_nullable, coalesce(column_default, '') FROM information_schema.columns
		  WHERE table_name = 'alert_sources' AND column_name = 'reconcile_enabled'`).
		Scan(&nullable, &colDefault); err != nil {
		t.Fatalf("introspect the restored reconcile_enabled: %v", err)
	}
	if nullable != "NO" || !strings.HasPrefix(colDefault, "true") {
		t.Fatalf("reconcile_enabled came back is_nullable=%q default=%q, want NO / true — a "+
			"release-N writer OMITS this column on some paths and every existing row has to read "+
			"as enabled, which is the value 00038 says is the only correct one", nullable, colDefault)
	}

	// 00037 down: the route-tree column goes.
	down(37)
	if amRoutesColumns() != 0 {
		t.Fatal("source_health.am_routes survived 00037's Down, so a release-N pod meets a " +
			"column its INSERT column list has never heard of")
	}

	// 00036 down: the function comes back with the OLD default in its signature.
	down(36)
	if args := partitionsManageArgs(); !strings.Contains(args, "p_raw_retention_days integer DEFAULT 14") {
		t.Fatalf("oto_partitions_manage(%s) did not go back to the 14-day raw-retention "+
			"default; 00036's Down is a CREATE OR REPLACE and it has to replace the whole body, "+
			"not just the comments", args)
	}

	// ⭐ 00035 down: the two widened reasons become `undecodable` — what version N
	// would itself have recorded — WITHOUT losing the rows or the true reason.
	down(35)
	for _, r := range rejections {
		var reason, detail string
		if err := env.pool.QueryRow(env.ctx,
			`SELECT reason, detail FROM ingest_rejections WHERE id = $1`, r.id).Scan(&reason, &detail); err != nil {
			t.Fatalf("the %s rejection did not survive 00035's Down: %v — this table is the "+
				"ENTIRE audit trail for a rejected alert, and deleting the rows during a "+
				"rollback destroys the evidence at the moment somebody is reading it",
				r.reason, err)
		}
		if reason != "undecodable" {
			t.Fatalf("a %s rejection reads %q after 00035's Down, want undecodable — the "+
				"narrowed CHECK cannot admit the old value, so a row left alone is a row the "+
				"next validating scan rejects", r.reason, reason)
		}
		if want := "[" + r.reason + "] " + r.detail; detail != want {
			t.Fatalf("detail = %q, want %q — the true reason is preserved at the FRONT of "+
				"detail precisely so that rewriting reason to undecodable loses nothing",
				detail, want)
		}
	}
	if def := rejectionReasonCheck(); strings.Contains(def, "invalid_label_value") ||
		strings.Contains(def, "annotation_unstorable") {
		t.Fatalf("ingest_rejections_reason_ck still admits the widened members after 00035's "+
			"Down: %s — the constraint name is a runtime contract (returned as errs.Error.Code "+
			"on a 23514), so it must be re-added under the same name with the OLD member list", def)
	}

	// 00034 through 00031 are rolled back without individual comment; what the
	// next three assertions prove is that 00034, 00033 and 00032 put their
	// DEFAULTs back, and what the rest of the test proves is that 00030, 00029 and
	// 00028 can be undone.
	for top := appliedTop(); top > 30; top = appliedTop() {
		down(top)
	}
	if channelsDefaults() != 2 {
		t.Fatal("channels lost its DEFAULT now() permanently: 00032's Down did not restore it, " +
			"so an operator rolling the release back would meet a not-null violation on the " +
			"first channel a release-N pod creates")
	}
	if appClockDefaults() != 3 {
		t.Fatal("orgs or channel_credentials lost its DEFAULT now() permanently: 00033's Down " +
			"did not restore it, so an operator rolling the release back would meet a not-null " +
			"violation on the first tenant or credential a release-N pod writes")
	}
	if n := remainingClockDefaults(); n != 20 {
		t.Fatalf("00034's Down restored %d of its 20 defaults; the ones it missed are gone "+
			"permanently, so an operator rolling the release back would meet a not-null "+
			"violation on the first row a release-N pod writes to that table", n)
	}

	// The table 00030 dropped is gone at the top of the stack.
	buckets := func() int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx,
			`SELECT count(*) FROM pg_tables WHERE tablename = 'rate_limit_buckets'`).Scan(&n); err != nil {
			t.Fatalf("introspect rate_limit_buckets: %v", err)
		}
		return n
	}
	if buckets() != 0 {
		t.Fatal("rate_limit_buckets is still present at migration 30; 00030 is supposed to drop it")
	}

	// 00030 down: the table comes back, empty, exactly as 00014 left it.
	if err := migrate.Down(env.ctx, dsn); err != nil {
		t.Fatalf("goose down 00030: %v", err)
	}
	if buckets() != 1 {
		t.Fatal("rate_limit_buckets did not come back on the way down; the Down of a DROP " +
			"is a CREATE, and an unrunnable one makes the migration undeployable")
	}

	// 00029 down: the partial index behind the occurrence delivery roll-up goes.
	if err := migrate.Down(env.ctx, dsn); err != nil {
		t.Fatalf("goose down 00029: %v", err)
	}
	var indexes int
	if err := env.pool.QueryRow(env.ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'notif_occurrence_idx'`).Scan(&indexes); err != nil {
		t.Fatalf("introspect indexes: %v", err)
	}
	if indexes != 0 {
		t.Fatal("notif_occurrence_idx survived the down migration")
	}

	// 00028 down: the six route-timing columns go with it.
	if err := migrate.Down(env.ctx, dsn); err != nil {
		t.Fatalf("goose down 00028: %v", err)
	}
	var n int
	if err := env.pool.QueryRow(env.ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name = 'source_health' AND column_name LIKE 'am_%route%'`).Scan(&n); err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d route-timing columns survived the down migration", n)
	}

	if err := migrate.Up(env.ctx, dsn); err != nil {
		t.Fatalf("goose up again: %v", err)
	}
	if err := env.pool.QueryRow(env.ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'notif_occurrence_idx'`).Scan(&indexes); err != nil {
		t.Fatalf("introspect indexes: %v", err)
	}
	if indexes != 1 {
		t.Fatal("notif_occurrence_idx did not come back on the way up")
	}
	if buckets() != 0 {
		t.Fatal("rate_limit_buckets survived the way back up")
	}
	if n := rollupRangeIndexes(); n != 2 {
		t.Fatalf("%d of 00042's two range indexes came back on the way up, want 2 — the rest of "+
			"the suite runs against this schema, and stats.rollup is a sequential scan of two "+
			"never-reaped tables without them", n)
	}
	// ⭐ 00041 comes back WITH ITS KEY. The table alone is not the migration: a
	// re-created `idempotency_claims` whose PK had lost a column would take every
	// claim without complaint and refuse the wrong caller's request, and nothing
	// downstream would notice until two operators shared a key.
	if claimTables() != 1 {
		t.Fatal("idempotency_claims did not come back on the way up; every `Idempotency-Key` the " +
			"contract declares is claimed against this table, and without it the credential " +
			"endpoints are unguarded again")
	}
	for _, column := range []string{"org_id", "principal_id", "operation", "idempotency_key"} {
		if def := claimKeyTuple(); !strings.Contains(def, column) {
			t.Fatalf("idempotency_claims_pk came back without %s: %s — the round trip has to "+
				"restore the tuple, not merely the table", column, def)
		}
	}
	// The round trip is only closed if the way back up restores what the way down
	// removed — and the rest of the suite runs against this schema.
	if drillTables() != 1 {
		t.Fatal("delivery_drills did not come back on the way up")
	}
	if def := batchModeCheck(); !strings.Contains(def, "synthetic") {
		t.Fatalf("ingest_batches_mode_ck did not re-widen on the way up: %s — the drill endpoint "+
			"is the only writer of that mode, and without it every drill fails at accept", def)
	}
	// ⚠️ 00038 closes in the OTHER direction: its Up is the DROP, so the column
	// that came back on the way down has to be gone again, or the rest of the suite
	// is running against a schema where the flag it deleted still exists.
	if n := reconcileEnabledColumns(); n != 0 {
		t.Fatal("alert_sources.reconcile_enabled survived the way back up; 00038's Up is the " +
			"DROP COLUMN, and a column left behind here is one the ORM-free INSERT column lists " +
			"in internal/sources no longer name")
	}
	if amRoutesColumns() != 1 {
		t.Fatal("source_health.am_routes did not come back on the way up")
	}
	if args := partitionsManageArgs(); !strings.Contains(args, "p_raw_retention_days integer DEFAULT 30") {
		t.Fatalf("oto_partitions_manage(%s) did not come back up to the 30-day default", args)
	}
	if def := rejectionReasonCheck(); !strings.Contains(def, "invalid_label_value") ||
		!strings.Contains(def, "annotation_unstorable") {
		t.Fatalf("ingest_rejections_reason_ck did not re-widen on the way up: %s", def)
	}
	// ⚠️ The rewritten rows STAY rewritten: 00035's Up widens the enum and does not
	// undo the Down's rewrite, which is why the true reason is kept in `detail` and
	// a re-application can be reconciled by eye rather than by re-derivation.
	for _, r := range rejections {
		var reason, detail string
		if err := env.pool.QueryRow(env.ctx,
			`SELECT reason, detail FROM ingest_rejections WHERE id = $1`, r.id).Scan(&reason, &detail); err != nil {
			t.Fatalf("the %s rejection did not survive the round trip: %v", r.reason, err)
		}
		if reason != "undecodable" || !strings.HasPrefix(detail, "["+r.reason+"] ") {
			t.Fatalf("after the round trip the %s rejection reads reason=%q detail=%q; the Up "+
				"is a pure relaxation and must not touch rows", r.reason, reason, detail)
		}
	}
}

// TestDeclarativeTuningOverTheWire is the contract the UI is about to be built
// against, asserted end to end: real config loading, real container, real router,
// real Postgres.
//
// ⭐ THE THREE THINGS A UI NEEDS AND CANNOT INVENT: the origin says `config`, the
// config key says WHERE, and the shadowed override is still there to show beside
// the value in force. Plus the 409, which is what stops the screen from offering
// an edit that would silently revert on the next deploy.
func TestDeclarativeTuningOverTheWire(t *testing.T) {
	t.Setenv("OTO_TUNING_REFIRE_GRACE_S", "600")

	env := newEnvWith(t, func(c *config.Config) {
		loaded, err := config.Load("")
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		// Keep the container's own plumbing; take only the tuning layer.
		loaded.DB, loaded.HTTP, loaded.Telemetry, loaded.Jobs = c.DB, c.HTTP, c.Telemetry, c.Jobs
		*c = loaded
	})

	boot, err := app.Bootstrap(env.ctx, env.pool, app.BootstrapRequest{
		OrgSlug: "acme", OrgName: "Acme", Email: "ops@acme.example",
		DisplayName: "Ops", Password: "correct-horse-battery-staple", TokenName: "bootstrap",
	}, time.Now())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// The org already had an override of 900 — written before the deployment
	// started stating a value, which is the whole scenario.
	if _, err := env.pool.Exec(env.ctx,
		`UPDATE orgs SET settings = '{"refire_grace_s": 900}'::jsonb WHERE slug = 'acme'`); err != nil {
		t.Fatalf("seed the override: %v", err)
	}

	var view struct {
		Data struct {
			Settings   map[string]any    `json:"settings"`
			Origins    map[string]string `json:"origins"`
			ConfigKeys map[string]string `json:"config_keys"`
			Shadowed   map[string]any    `json:"shadowed"`
		} `json:"data"`
	}
	env.do(t, http.MethodGet, "/api/v1/org/settings", boot.Token, nil, http.StatusOK, &view)

	if got := view.Data.Settings["refire_grace_s"]; got != float64(600) {
		t.Fatalf("effective refire_grace_s = %v, want 600: configuration must beat the override", got)
	}
	if got := view.Data.Origins["refire_grace_s"]; got != "config" {
		t.Fatalf("origin %q, want config", got)
	}
	if got := view.Data.ConfigKeys["refire_grace_s"]; got != "OTO_TUNING_REFIRE_GRACE_S" {
		t.Fatalf("config_keys[refire_grace_s] = %q, want the env var an operator can edit", got)
	}
	if got := view.Data.Shadowed["refire_grace_s"]; got != float64(900) {
		t.Fatalf("shadowed = %v, want the org's own 900 — hiding it is how somebody spends "+
			"an afternoon on a number they can see in the database and never in force", got)
	}
	// A key the deployment does not manage is not shadowed and carries no config key.
	if _, present := view.Data.Shadowed["storm_threshold"]; present {
		t.Fatal("an unmanaged key was reported as shadowed")
	}
	if _, present := view.Data.ConfigKeys["storm_threshold"]; present {
		t.Fatal("an unmanaged key was given a config key")
	}

	// Writing the tuning is SESSION-ONLY (a leaked token must not be able to make
	// oto go quiet), so the write half of this test logs in.
	session := login(t, env, "ops@acme.example", "correct-horse-battery-staple")

	// ⛔ The write is REFUSED, and the refusal names the key to edit.
	status, raw := session.patch(t, map[string]any{"refire_grace_s": 1200})
	if status != http.StatusConflict {
		t.Fatalf("PATCH on a config-managed key → %d, want 409: %s", status, raw)
	}
	if !strings.Contains(string(raw), "OTO_TUNING_REFIRE_GRACE_S") {
		t.Fatalf("the 409 does not name the config key: %s", raw)
	}

	// A key the deployment does not manage is still writable, and the write does
	// not drop the declarative overlay from the response.
	status, raw = session.patch(t, map[string]any{"storm_threshold": 40})
	if status != http.StatusOK {
		t.Fatalf("PATCH on an unmanaged key → %d: %s", status, raw)
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := view.Data.Settings["storm_threshold"]; got != float64(40) {
		t.Fatalf("storm_threshold = %v after a legal write", got)
	}
	if got := view.Data.Settings["refire_grace_s"]; got != float64(600) {
		t.Fatalf("the write dropped the declarative overlay: refire_grace_s = %v", got)
	}
	if got := view.Data.Origins["storm_threshold"]; got != "org" {
		t.Fatalf("origin of a freshly written unmanaged key = %q, want org", got)
	}
}

// sessionEnv is a cookie-carrying client for the session-only settings write.
type sessionEnv struct {
	base   string
	client *http.Client
}

// login exchanges a password for a session cookie, which is the only credential
// `PATCH /api/v1/org/settings` accepts.
func login(t *testing.T, e *env, email, password string) sessionEnv {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{Jar: jar}

	body, err := json.Marshal(map[string]any{"email": email, "password": password})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequestWithContext(e.ctx, http.MethodPost,
		e.server.URL+"/api/v1/auth/login", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login → %d: %s", resp.StatusCode, raw)
	}
	return sessionEnv{base: e.server.URL, client: client}
}

func (s sessionEnv) patch(t *testing.T, body any) (int, []byte) {
	t.Helper()

	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPatch,
		s.base+"/api/v1/org/settings", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}
