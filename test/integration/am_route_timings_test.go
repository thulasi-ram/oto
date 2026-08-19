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
// ⚠️⚠️ THE VOCABULARY CHANGES PART-WAY DOWN, and getting it wrong is the one
// mistake here that does not announce itself. 00052 renames the firing episode
// from `alert_occurrences` to `alert_cases` and carries seven columns,
// twenty-seven constraints and ten indexes with it, so `down(52)` is the FIRST
// step and everything below it must be spelled the OLD way — `alert_occurrences`,
// `occ_started_idx`, `occ_group_live_idx`, `notif_occurrence_idx`,
// `alerts.current_occurrence_id`. A post-rename name used below `down(52)` does
// not error: it reads "" or 0, and the assertion it belongs to passes VACUOUSLY.
//
// WHAT EACH DOWN HAS TO PUT BACK, newest first:
//
//   - ⭐⭐ 00052 is a RENAME and nothing else — one table, seven columns,
//     twenty-seven constraints, ten indexes, plus four columns of live rows —
//     which makes its Down a hand-written list of ninety-odd identifiers whose
//     characteristic defect is a forgotten line. A forgotten `ALTER ... RENAME`
//     is SILENT: only renaming onto a name already taken errors, so a half-applied
//     Down exits 0 with the schema in two vocabularies at once. So both spellings
//     are COUNTED and the assertion is that the totals swap, rather than any one
//     name being spot-checked. The row rewrites are read separately, because two
//     of the four are guarded by CHECKs the Down re-adds and two are not.
//
//   - ⭐⭐ 00051 DROPPED `alert_group_members` and created `occ_group_live_idx`
//     in its place, so it is the one migration here whose Down RESTORES DATA:
//     the table is rebuilt and repopulated with `INSERT ... SELECT ... FROM
//     alert_occurrences`, which is exact rather than approximate because every
//     column the table carried is a column of the episode — and that is the whole
//     argument for dropping it. A structural check alone would miss it, so the
//     rebuilt row count is compared against the number of grouped episodes. The
//     partial predicate on the new index is asserted with its columns for the
//     reason 00044's was: an index of the same name WITHOUT `WHERE ended_at IS
//     NULL` spans every episode the generation ever held.
//
//   - 00050 is FIVE COMMENTS and nothing else, with deliberately no constraint, no
//     backfill and no re-key in either direction, so the prose IS the migration:
//     `alert_groups` goes back to describing itself as an Alertmanager
//     notification group. The ABSENCE of `groups_axes_ck` is asserted in both
//     directions and is not pedantry — an earlier draft added it `NOT VALID`, and
//     `NOT VALID` skips only the validation SCAN: Postgres re-checks a CHECK
//     against the new row version on every UPDATE, so every pre-00050 generation
//     (`group_labels = '{}'` for every reconciler-sourced one) would have become
//     permanently un-UPDATE-able and `group.close` — this migration's whole
//     self-healing story — would have failed on it forever, silently, as a warning.
//
//   - ⭐ 00049 DROPPED `alerts.ack_state`, so its Down ADDS a column — the half
//     that cannot fail. What is asserted is the other half: the Down rebuilds the
//     projection from `alert_occurrences`, its authority, rather than defaulting
//     it, and a defaulted column hands the rolled-back release a database in
//     which nothing is acknowledged.
//
//   - ⭐ 00048 DROPPED `alerts.snoozed_until` and its index, and its Down has the
//     same shape and the same trap as 00049's: the column comes back reprojected
//     from `alert_snoozes`, and `alerts_snooze_idx` comes back WITH its partial
//     predicate rather than merely with its name.
//
//   - ⭐ 00046 TIGHTENED `policies_reasons_ck` — `reasons` became a set of 1..18
//     rather than a bag of 1..32 — so its Down is a RELAXATION, and a relaxation
//     is the Down most likely to be believed rather than run: it cannot fail, and
//     nothing about it is visible at the exit code. Both directions are exercised
//     with a real row instead. A duplicate is refused at the top of the stack, is
//     ACCEPTED once the Down has run, and is FOLDED to its distinct values by the
//     Up on the way back — which is the only place in this suite the migration's
//     backfill meets a row that actually violates the constraint it is about to
//     add. A Down that dropped the constraint without restoring the loose one, or
//     an Up that added the constraint without folding first, would both be green
//     on a constraint-text assertion and neither would survive here. The helper
//     function the CHECK calls (`oto_array_is_set`, which exists because Postgres
//     forbids a subquery in a CHECK) goes with it and comes back with it.
//
//   - ⭐ 00045 added `alert_labels` and `alert_label_names`, the projection the
//     two label typeaheads now read instead of scanning the tenant's `alerts`
//     per keystroke, so its Down is two DROP TABLEs and the property that flips
//     is their existence. It is asserted WITH ROWS IN BOTH, because that is the
//     only state an operator ever rolls this back from — the Up backfills them
//     from `alerts`, so they are never empty on a database that has any alerts —
//     and because a Down that had left a dependency behind would fail here
//     rather than at 02:00. The rows are recoverable by definition: both tables
//     are a pure function of `alerts.labels`, which the Down does not touch, so
//     what the rollback loses is speed and nothing else. Whether the indexes are
//     USED is a different question and not one a round trip can answer; that is
//     asserted against a real plan, with each index dropped as the control, in
//     internal/alerts/repository/labels_plan_test.go.
//
//   - 00044 added `gm_current_idx`, the PARTIAL index the only read of a
//     generation's current members rode until 00051 moved that read onto the
//     episode itself, so its Down is a DROP INDEX and the property that flips
//     is its presence in `pg_indexes`. It is asserted in the middle of the round
//     trip rather than at the top of the stack, because at the top of the stack it
//     no longer exists. The partial predicate is asserted with it, out of
//     `pg_indexes.indexdef`: an index of the same name over the same columns
//     WITHOUT `WHERE left_at IS NULL` would be green on a presence check while
//     being a different index. Whether either is USED is a different question and
//     not one a round trip can answer; that is asserted against a real plan, with
//     the index dropped as the control, in
//     internal/grouping/repository/member_plan_test.go.
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
//     `occ_started_idx` (`case_started_idx` above `down(52)`) and
//     `notif_created_idx`, so its Down is a pair of DROP
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
	if latest != 66 {
		t.Fatalf("latest migration is %d, want 66 — this test pins the number so that a "+
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
			clockDefaults("alert_cases", "created_at", "updated_at") +
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
	//
	// ⛔ The index on the episode table is passed IN rather than hardcoded, because
	// 00052 renames it and this helper is called from both sides of that rename:
	// `case_started_idx` at the top of the stack and after the final Up,
	// `occ_started_idx` down at 00042 where 00052's Down has already run. Hardcoding
	// one spelling makes the reading from the other side return 0 or 1 for a reason
	// that has nothing to do with 00042, which is the failure this whole file is
	// arranged to avoid.
	rollupRangeIndexes := func(startedIdx string) int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx,
			`SELECT count(*) FROM pg_indexes
			  WHERE indexname = ANY($1::text[])`,
			[]string{startedIdx, "notif_created_idx"}).Scan(&n); err != nil {
			t.Fatalf("introspect the rollup range indexes: %v", err)
		}
		return n
	}
	// 00044's index, read as its rendered DEFINITION rather than as a name. The
	// name coming back proves an index exists; the definition is what proves it is
	// THE index — partial on `left_at IS NULL`, which is what keeps it the size of
	// a generation's living membership instead of its whole history, and what the
	// two replay reads deliberately cannot ride. An empty string means absent.
	currentMemberIndexDef := func() string {
		t.Helper()
		var def string
		if err := env.pool.QueryRow(env.ctx,
			`SELECT coalesce(max(indexdef), '') FROM pg_indexes
			  WHERE indexname = 'gm_current_idx'`).Scan(&def); err != nil {
			t.Fatalf("introspect gm_current_idx: %v", err)
		}
		return def
	}
	// Any index, read as its rendered DEFINITION rather than as a name, for the
	// reason `currentMemberIndexDef` is: the name coming back proves an index
	// exists, and only the definition proves it is THE index. 00051's
	// `*_group_live_idx` is why — it is gm_current_idx's successor, same shape over
	// the table that now HOLDS the membership, partial on `ended_at IS NULL`, which
	// unlike the predicate it replaces something actually writes. An index of that
	// name without the predicate spans every episode the generation ever held and
	// is a different index. An empty string means absent.
	//
	// ⛔ The NAME is a parameter for the same reason `rollupRangeIndexes`' is: 00052
	// renames `occ_group_live_idx` to `case_group_live_idx`, so it carries the old
	// spelling everywhere below `down(52)` — including at 00051, which created it.
	// A hardcoded `case_group_live_idx` there returns "" and the assertion that
	// 00051's Down dropped it passes VACUOUSLY, before that Down has even run.
	indexDef := func(name string) string {
		t.Helper()
		var def string
		if err := env.pool.QueryRow(env.ctx,
			`SELECT coalesce(max(indexdef), '') FROM pg_indexes
			  WHERE indexname = $1`, name).Scan(&def); err != nil {
			t.Fatalf("introspect %s: %v", name, err)
		}
		return def
	}
	// Any constraint, as rendered SQL, empty when absent. `pg_get_constraintdef`
	// rather than a bare existence count because a constraint's PREDICATE is the
	// contract and its name is only the handle — and because `NOT VALID` shows up
	// in the rendering, which is the whole point of 00050's.
	// A column's comment. Same reason `tableComment` exists: a migration whose only
	// visible output is a sentence an operator reads at `\d+` is the one whose Down
	// is most likely to be a copy of its Up.
	columnComment := func(table, column string) string {
		t.Helper()
		var c string
		if err := env.pool.QueryRow(env.ctx,
			`SELECT coalesce(col_description($1::regclass, (
			           SELECT attnum FROM pg_attribute
			            WHERE attrelid = $1::regclass AND attname = $2)), '')`,
			table, column).Scan(&c); err != nil {
			t.Fatalf("introspect comment on %s.%s: %v", table, column, err)
		}
		return c
	}

	constraintDef := func(name, table string) string {
		t.Helper()
		var def string
		if err := env.pool.QueryRow(env.ctx,
			`SELECT coalesce((SELECT pg_get_constraintdef(oid) FROM pg_constraint
			                   WHERE conname = $1 AND conrelid = $2::regclass), '')`,
			name, table).Scan(&def); err != nil {
			t.Fatalf("introspect %s on %s: %v", name, table, err)
		}
		return def
	}
	// A table's comment, as text, because text is all it is. Read for 00050 for the
	// reason `eventKeyComments` is read for 00043: a migration whose visible output
	// is a sentence an operator sees at `\d+` is the one whose Down is most likely
	// to be a copy of its Up.
	tableComment := func(table string) string {
		t.Helper()
		var c string
		if err := env.pool.QueryRow(env.ctx,
			`SELECT coalesce(obj_description($1::regclass, 'pg_class'), '')`, table).Scan(&c); err != nil {
			t.Fatalf("introspect the comment on %s: %v", table, err)
		}
		return c
	}
	// Whether `alert_group_members` exists at all. 00051 drops it; its Down
	// rebuilds it AND repopulates it out of `alert_cases`, which is the
	// claim that makes the drop reversible.
	memberTableExists := func() bool {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx,
			`SELECT count(*) FROM information_schema.tables
			  WHERE table_name::text = 'alert_group_members'`).Scan(&n); err != nil {
			t.Fatalf("introspect alert_group_members: %v", err)
		}
		return n == 1
	}

	// ⭐⭐ 00052's rename, read as two VOCABULARIES rather than as a spot-check.
	//
	// 00052 is the largest migration in the stack and its entire content is
	// renaming: one table, seven columns spread over five OTHER tables,
	// twenty-seven constraints and ten indexes, all from the `occurrence` spelling
	// to the `case` one. Its Down is that same list backwards.
	//
	// ⛔ THE FAILURE MODE OF A NINETY-IDENTIFIER LIST IS A FORGOTTEN LINE, AND A
	// FORGOTTEN RENAME IS SILENT. `ALTER ... RENAME` to a name that is already
	// taken errors; omitting one entirely does not error anywhere, and the Down
	// exits 0 having left the schema half in each vocabulary. What finds it is not
	// the exit code but a count, so both spellings are counted by the same three
	// functions and the assertion is that the totals SWAP — complete on the `case`
	// side and zero on the `occurrence` side at the top of the stack, exactly
	// reversed once the Down has run. A Down that renamed the table and forgot one
	// constraint is caught by the second number rather than by nothing.
	countTables := func(names ...string) int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx,
			`SELECT count(*) FROM pg_tables WHERE tablename = ANY($1::text[])`, names).Scan(&n); err != nil {
			t.Fatalf("introspect tables %v: %v", names, err)
		}
		return n
	}
	countIndexes := func(names ...string) int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx,
			`SELECT count(*) FROM pg_indexes WHERE indexname = ANY($1::text[])`, names).Scan(&n); err != nil {
			t.Fatalf("introspect indexes %v: %v", names, err)
		}
		return n
	}
	countConstraints := func(names ...string) int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx,
			`SELECT count(*) FROM pg_constraint WHERE conname = ANY($1::text[])`, names).Scan(&n); err != nil {
			t.Fatalf("introspect constraints %v: %v", names, err)
		}
		return n
	}
	countColumns := func(table string, names ...string) int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx,
			// Both sides cast to text for the reason `clockDefaults` casts them: the
			// information_schema identifier columns are a domain over `name`.
			`SELECT count(*) FROM information_schema.columns
			  WHERE table_name::text = $1 AND column_name::text = ANY($2::text[])`,
			table, names).Scan(&n); err != nil {
			t.Fatalf("introspect %s columns %v: %v", table, names, err)
		}
		return n
	}
	// One spelling of the firing episode across the whole schema. The two values of
	// this struct below are 00052's before and after, and they are written out
	// rather than derived by string substitution because the mapping is NOT
	// mechanical: `occurrence` shortens to `occ` in most names but not in
	// `notif_occurrence_idx`, and `alert_quality_daily.occurrences` has no prefix at
	// all. A substitution rule would agree with itself and with nothing else.
	type episodeNames struct {
		table       string
		alertCols   []string // carried on `alerts`
		refCol      string   // the FK column on alert_events, notifications, delivery_drills
		qualityCols []string // carried on `alert_quality_daily`
		constraints []string // 25 on the episode table + 2 on `alerts`
		indexes     []string
	}
	caseNames := episodeNames{
		table:       "alert_cases",
		alertCols:   []string{"current_case_id", "total_cases"},
		refCol:      "case_id",
		qualityCols: []string{"cases", "acked_cases"},
		constraints: []string{
			"case_seq_uniq", "case_state_ck", "case_supreason_ck", "case_resreason_ck",
			"case_ackstate_ck", "case_terminal_ended", "case_seq_ck", "case_reopen_ck",
			"case_order_ck", "case_obs_ck", "case_src_order_ck", "case_suppress_ck",
			"case_suppby_ck", "case_resolve_ck", "case_resolve_map_ck", "case_ack_ck",
			"case_acklabel_ck", "case_ackorder_ck", "case_acknote_ck", "case_reopenof_ck",
			"case_time_ck", "case_sver_ck", "case_supcount_ck", "case_group_fk", "case_rule_fk",
			"alerts_case_ck", "alerts_current_case_fk",
		},
		indexes: []string{
			"alert_cases_pkey", "case_one_open_idx", "case_alert_idx", "case_group_idx",
			"case_reap_idx", "case_ack_idx", "case_started_idx", "case_group_live_idx",
			"ev_case_idx", "notif_case_idx",
		},
	}
	preRenameNames := episodeNames{
		table:       "alert_occurrences",
		alertCols:   []string{"current_occurrence_id", "total_occurrences"},
		refCol:      "occurrence_id",
		qualityCols: []string{"occurrences", "acked_occurrences"},
		constraints: []string{
			"occ_seq_uniq", "occ_state_ck", "occ_supreason_ck", "occ_resreason_ck",
			"occ_ackstate_ck", "occ_terminal_ended", "occ_seq_ck", "occ_reopen_ck",
			"occ_order_ck", "occ_obs_ck", "occ_src_order_ck", "occ_suppress_ck",
			"occ_suppby_ck", "occ_resolve_ck", "occ_resolve_map_ck", "occ_ack_ck",
			"occ_acklabel_ck", "occ_ackorder_ck", "occ_acknote_ck", "occ_reopenof_ck",
			"occ_time_ck", "occ_sver_ck", "occ_supcount_ck", "occ_group_fk", "occ_rule_fk",
			"alerts_occ_ck", "alerts_current_occ_fk",
		},
		indexes: []string{
			"alert_occurrences_pkey", "occ_one_open_idx", "occ_alert_idx", "occ_group_idx",
			"occ_reap_idx", "occ_ack_idx", "occ_started_idx", "occ_group_live_idx",
			"ev_occ_idx", "notif_occurrence_idx",
		},
	}
	// The four totals, so a partial rename shows up as a number rather than as an
	// unrelated 42P01 several steps later.
	episodeVocabulary := func(n episodeNames) (tables, columns, constraints, indexes int) {
		t.Helper()
		return countTables(n.table),
			countColumns("alerts", n.alertCols...) +
				countColumns("alert_events", n.refCol) +
				countColumns("notifications", n.refCol) +
				countColumns("delivery_drills", n.refCol) +
				countColumns("alert_quality_daily", n.qualityCols...),
			countConstraints(n.constraints...),
			countIndexes(n.indexes...)
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

	// 00046's constraint, read as rendered SQL for the reason `batchModeCheck` is:
	// the constraint NAME is the runtime contract, and what matters is that the
	// same name carries a different predicate. The set rule is the part worth
	// naming — a `policies_reasons_ck` that still counted elements and scanned for
	// NULLs would be the pre-00046 constraint under the post-00046 name.
	policyReasonsCheck := func() string {
		t.Helper()
		var def string
		if err := env.pool.QueryRow(env.ctx,
			`SELECT pg_get_constraintdef(oid) FROM pg_constraint
			  WHERE conname = 'policies_reasons_ck'
			    AND conrelid = 'notification_policies'::regclass`).Scan(&def); err != nil {
			t.Fatalf("introspect policies_reasons_ck: %v", err)
		}
		return def
	}
	// The function that CHECK calls. It is counted separately because it is
	// dropped and created by the same migration and a Down that restored the loose
	// constraint while leaving the function behind would be green everywhere else.
	arrayIsSetFunctions := func() int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx,
			`SELECT count(*) FROM pg_proc WHERE proname = 'oto_array_is_set'`).Scan(&n); err != nil {
			t.Fatalf("introspect oto_array_is_set: %v", err)
		}
		return n
	}

	// 00045's two tables, counted together because they are one migration and
	// therefore roll back together. `pg_tables` rather than `pg_class` so a name
	// that came back as something other than a table would not count.
	labelProjectionTables := func() int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx,
			`SELECT count(*) FROM pg_tables WHERE tablename = ANY($1::text[])`,
			[]string{"alert_labels", "alert_label_names"}).Scan(&n); err != nil {
			t.Fatalf("introspect the label projection tables: %v", err)
		}
		return n
	}

	// ⭐ 00046's constraint, and the only assertion in this file that is about a
	// value the schema must REFUSE. `reasons` is a set in the contract
	// (`uniqueItems: true` on `PolicyDTO`, which is a RESPONSE) and was a set in
	// exactly one of CONTEXT.md §5b's three places until 00046 — so a duplicate
	// reaching this column comes back on a read as a row oto serves and the
	// frontend validator it generated then refuses.
	//
	// The org and the policy seeded here outlive the whole rollback: the same row
	// is written duplicated once the Down has relaxed the constraint, and read back
	// folded after the way up.
	policyScope, _, _ := seedSource(t, env)
	insertPolicy := func(policyID uuid.UUID, name string, reasons []string) error {
		_, err := env.pool.Exec(env.ctx,
			// `created_at`/`updated_at` are NAMED: 00034 removed this table's
			// DEFAULT now() along with twelve others'.
			`INSERT INTO notification_policies (id, org_id, name, reasons, channel_ids,
			     created_at, updated_at)
			 VALUES ($1, $2, $3, $4, ARRAY[$5::uuid], $6, $6)`,
			policyID, policyScope.OrgID(), name, reasons, id.New(), time.Now().UTC())
		return err
	}
	// 00047's natural key, read as rendered SQL for the same reason the two
	// constraints above are: the NAME is the runtime contract — `insertRejectionsSQL`
	// names it in its ON CONFLICT — so a constraint of that name over different
	// columns would be the pre-00047 shape wearing the post-00047 label. Empty
	// means absent, which is what the Down is supposed to produce.
	rejectionNaturalUniq := func() string {
		t.Helper()
		var def string
		if err := env.pool.QueryRow(env.ctx,
			`SELECT COALESCE((SELECT pg_get_constraintdef(oid) FROM pg_constraint
			                   WHERE conname = 'ingest_rejections_natural_uniq'
			                     AND conrelid = 'ingest_rejections'::regclass), '')`).
			Scan(&def); err != nil {
			t.Fatalf("introspect ingest_rejections_natural_uniq: %v", err)
		}
		return def
	}
	// A rejection written straight at the table, because what is being proved is a
	// CONSTRAINT and not a code path: the repository's writer carries its own ON
	// CONFLICT, which would absorb precisely the collision this needs to observe.
	// `received_at` is the partition key, so it is passed rather than defaulted.
	insertRejection := func(batchID uuid.UUID, ordinal *int, at time.Time) error {
		_, err := env.pool.Exec(env.ctx,
			`INSERT INTO ingest_rejections (id, org_id, source_id, batch_id, received_at,
			     ordinal, reason, raw)
			 VALUES ($1, $2, $3, $4, $5, $6, 'missing_alertname', '{}'::jsonb)`,
			id.New(), policyScope.OrgID(), id.New(), batchID, at, ordinal)
		return err
	}
	if def := rejectionNaturalUniq(); !strings.Contains(def, "ordinal") {
		t.Fatalf("ingest_rejections_natural_uniq does not carry `ordinal` at the top of the "+
			"stack: %q — 00047 exists to put it there, and without it `oto replay` re-inserts "+
			"a batch's rejections instead of conflicting with them, which triples the feed an "+
			"operator reads to decide whether their alert was dropped", def)
	}
	if def := policyReasonsCheck(); !strings.Contains(def, "oto_array_is_set") {
		t.Fatalf("policies_reasons_ck does not carry the set rule at the top of the stack: %s — "+
			"00046 exists to put it there, and while it is absent the `unique` tag on the "+
			"request DTOs is the ONLY place uniqueness is written: any writer that does not "+
			"pass through httpx.Bind can store a value the response contract calls impossible",
			def)
	}
	if n := arrayIsSetFunctions(); n != 1 {
		t.Fatalf("%d oto_array_is_set functions exist at the top of the stack, want 1 — the "+
			"CHECK calls it, because Postgres forbids a subquery in a CHECK and every direct "+
			"spelling of set-ness is an aggregate over unnest", n)
	}
	if err := insertPolicy(id.New(), "bag-at-the-top", []string{"fired", "acked", "fired"}); err == nil {
		t.Fatal("a policy listing `fired` twice was STORED at the top of the stack; " +
			"policies_reasons_ck is the backstop for a rule the repository deliberately does " +
			"not re-prove, and a backstop that accepts the value is not one")
	}

	// ⭐ 00045's projection, asserted at the top of the stack because "the Down
	// dropped them" is only interesting if the Up created them in the first
	// place. Without these two tables the label typeahead is back to expanding
	// every non-synthetic alert in the org through a LATERAL jsonb_object_keys
	// once per keystroke, on a table ADR 0024 never reaps — and no index on
	// `alerts` can replace them, because Postgres refuses a set-returning
	// function in an index expression and the value typeahead takes its label
	// name as a runtime parameter.
	if n := labelProjectionTables(); n != 2 {
		t.Fatalf("%d of 00045's two tables exist at the top of the stack, want 2 — "+
			"alert_labels and alert_label_names are what GET /api/v1/labels and "+
			"GET /api/v1/labels/{name}/values read, and without them both read `alerts` in "+
			"full on the filter bar of the incident view", n)
	}

	// 00051 at the top of the stack, in both directions: the join table is GONE and
	// its successor index is present. The partial predicate is asserted with the
	// columns, because an index of this name without `WHERE ended_at IS NULL` spans
	// every episode the generation ever held — the shape the two bounded reads do
	// not want, and the shape gm_current_idx effectively had, since nothing ever
	// wrote the `left_at` it was partial on.
	if memberTableExists() {
		t.Fatal("alert_group_members still exists at the top of the stack; 00051 drops it, " +
			"and while it is there two tables answer `what is in this generation` — one of " +
			"them a table whose `left_at` no production code has ever written")
	}
	if def := indexDef("case_group_live_idx"); def == "" {
		t.Fatal("case_group_live_idx is absent at the top of the stack; 00051 exists to create " +
			"it, and without it the only read of a generation's live members sorts the whole " +
			"membership to return twenty rows — on the detail page and on every ack, snooze " +
			"and unsnooze reply that re-renders it")
	} else if !strings.Contains(def, "ended_at IS NULL") {
		t.Fatalf("case_group_live_idx is not partial at the top of the stack: %s — the predicate "+
			"is half the decision, and an index over ended episodes too is a different index "+
			"under the same name", def)
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
	if n := rollupRangeIndexes("case_started_idx"); n != 2 {
		t.Fatalf("%d of 00042's two range indexes exist at the top of the stack, want 2 — "+
			"case_started_idx and notif_created_idx are the only indexes either table has that "+
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

	// ⭐⭐ THE ROWS THE FOUR DATA-BEARING DOWN-STEPS BELOW ARE ABOUT.
	//
	// ⛔ A COUNT COMPARED AGAINST A COUNT IS NOT AN ASSERTION WHEN BOTH ARE ZERO, and
	// all four of them were. 00052's row rewrites, 00051's membership rebuild,
	// 00049's ack reprojection and 00048's snooze reprojection are the ONLY reasons
	// those steps were argued to be reversible, and each is written as "the rebuilt
	// figure equals its authority" — which holds trivially over empty tables. The
	// suite's other fixtures (`alerts` at 00045, the episodes behind 00040's fold)
	// are all written BELOW `down(48)`, so at the moment those four read, every table
	// they counted was empty and every one of them passed. These rows are what give
	// them teeth, and they are seeded at the TOP of the stack so that they travel
	// through all five Downs and back up again.
	//
	// ⚠️ THE NAMES HERE ARE THE POST-00052 ONES — `alert_cases`,
	// `alerts.current_case_id` — because this runs ABOVE `down(52)`. That is the
	// exact opposite of the rule that governs every assertion inside the rollback
	// loop, which is why the seed lives here rather than beside them.
	episodeScope, _, episodeHealth := seedSource(t, env)
	var episodeCluster uuid.UUID
	if err := env.pool.QueryRow(env.ctx,
		`SELECT cluster_id FROM alert_sources WHERE id = $1`, episodeHealth.SourceID).
		Scan(&episodeCluster); err != nil {
		t.Fatalf("read the episode source's cluster: %v", err)
	}
	episodeAt := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

	// Two generations. `derivedGroup` is the ADR 0038 shape — `group_labels` IS the
	// split axes — and it is the one the episodes hang off. `legacyGroup` is what
	// every reconciler-sourced generation looked like before 00050: the empty object,
	// which `groups_labels_ck` has always permitted and which an axes CHECK would
	// have made permanently un-UPDATE-able. 00050's step below writes to it.
	derivedGroup, legacyGroup := id.New(), id.New()
	for _, g := range []struct {
		id          uuid.UUID
		key, labels string
	}{
		{derivedGroup, "gk_" + strings.Repeat("d", 26), `{"alertname":"RollbackEpisode"}`},
		{legacyGroup, "gk_" + strings.Repeat("l", 26), `{}`},
	} {
		if _, err := env.pool.Exec(env.ctx,
			`INSERT INTO alert_groups (id, org_id, source_id, cluster_id, group_key, group_labels,
			                           title, state, first_seen_at, last_activity_at)
			 VALUES ($1, $2, $3, $4, $5, $6::jsonb, 'RollbackEpisode', 'open', $7, $7)`,
			g.id, episodeScope.OrgID(), episodeHealth.SourceID, episodeCluster, g.key, g.labels,
			episodeAt); err != nil {
			t.Fatalf("seed a generation whose group_labels are %s: %v", g.labels, err)
		}
	}

	// One alert with TWO episodes, both in `derivedGroup`: the first ended, the
	// second live and ACKED. Two members for 00051's rebuild to reproduce, one of
	// them carrying a `left_at` — the column its `INSERT ... SELECT` takes from
	// `ended_at`, and the column the dropped table never had a production writer for.
	// The live episode is the alert's CURRENT one, which is the authority 00049's
	// Down reprojects `alerts.ack_state` from.
	episodeAlert := id.New()
	episodeKey := "ak_" + strings.Repeat("e", 26)
	if _, err := env.pool.Exec(env.ctx,
		`INSERT INTO alerts (id, org_id, cluster_id, alert_key, source_fingerprint, alertname,
		                     cluster_key, labels, state, first_seen_at, last_seen_at,
		                     last_state_change_at)
		 VALUES ($1, $2, $3, $4, 'abababababababab', 'RollbackEpisode', 'prod',
		         '{"alertname":"RollbackEpisode"}'::jsonb, 'firing', $5, $5, $5)`,
		episodeAlert, episodeScope.OrgID(), episodeCluster, episodeKey, episodeAt); err != nil {
		t.Fatalf("seed the alert behind the episodes: %v", err)
	}
	endedCase, liveCase := id.New(), id.New()
	if _, err := env.pool.Exec(env.ctx,
		`INSERT INTO alert_cases (id, org_id, alert_id, group_id, seq, state, resolve_reason,
		                          started_at, ended_at, last_observed_at, source_starts_at)
		 VALUES ($1, $2, $3, $4, 1, 'closed', 'upstream', $5, $6, $6, $5)`,
		endedCase, episodeScope.OrgID(), episodeAlert, derivedGroup,
		episodeAt, episodeAt.Add(time.Hour)); err != nil {
		t.Fatalf("seed the ended episode: %v", err)
	}
	if _, err := env.pool.Exec(env.ctx,
		`INSERT INTO alert_cases (id, org_id, alert_id, group_id, seq, state, started_at,
		                          last_observed_at, source_starts_at, ack_state, acked_at,
		                          acked_by_label)
		 VALUES ($1, $2, $3, $4, 2, 'open', $5, $5, $5, 'acked', $5, 'the rollback suite')`,
		liveCase, episodeScope.OrgID(), episodeAlert, derivedGroup,
		episodeAt.Add(2*time.Hour)); err != nil {
		t.Fatalf("seed the live acked episode: %v", err)
	}
	if _, err := env.pool.Exec(env.ctx,
		`UPDATE alerts SET current_case_id = $2, total_cases = 2 WHERE id = $1`,
		episodeAlert, liveCase); err != nil {
		t.Fatalf("point the alert at its current episode: %v", err)
	}

	// The ACTIVE snooze 00048's Down reprojects `alerts.snoozed_until` from. Written
	// straight at the table, because what is under test is the migration's UPDATE and
	// not the snooze service. `alert_snoozes_active_idx` is UNIQUE (alert_id) WHERE
	// ended_at IS NULL, which is what makes that `UPDATE ... FROM` deterministic.
	if _, err := env.pool.Exec(env.ctx,
		`INSERT INTO alert_snoozes (id, org_id, alert_id, alert_key, snoozed_at, snoozed_until,
		                            snoozed_by_label)
		 VALUES ($1, $2, $3, $4, $5, $6, 'the rollback suite')`,
		id.New(), episodeScope.OrgID(), episodeAlert, episodeKey,
		episodeAt, episodeAt.Add(4*time.Hour)); err != nil {
		t.Fatalf("seed an active snooze: %v", err)
	}

	// ⛔ AND ONE ROW IN EACH OF THE FOUR TABLES 00052 REWRITES, spelled `case`. Two
	// of the four are guarded by CHECKs its Down re-adds, so a missed rewrite there
	// fails the migration outright; `alert_event_keys` and `delivery_drills` have no
	// such guard, which makes them the two that can survive a Down silently — and a
	// stray `case:` dedupe key stops de-duplicating against the `occ:` keys the
	// rolled-back release computes, appending the same event to a timeline twice.
	// `alert_event_keys.event_id` carries no FK: the table is a live claim, not a
	// projection. `ui_events` is PARTITION BY RANGE (at) with hourly partitions and
	// 00013 creates the current hour plus six, so a `now()` row lands in one that
	// exists — it is deliberately NOT stamped `episodeAt`, which is in no partition.
	if _, err := env.pool.Exec(env.ctx,
		`INSERT INTO alert_event_keys (org_id, dedupe_key, event_id) VALUES ($1, $2, $3)`,
		episodeScope.OrgID(), "case:"+liveCase.String()+":opened", id.New()); err != nil {
		t.Fatalf("claim a case-spelled dedupe key: %v", err)
	}
	if _, err := env.pool.Exec(env.ctx,
		`INSERT INTO enrichments (id, org_id, subject_kind, subject_id, enricher,
		                          enricher_version, phase, status, computed_at)
		 VALUES ($1, $2, 'case', $3, 'oto.rollback', 1, 1, 'ok', $4)`,
		id.New(), episodeScope.OrgID(), liveCase, episodeAt); err != nil {
		t.Fatalf("seed a case-scoped enrichment: %v", err)
	}
	if _, err := env.pool.Exec(env.ctx,
		`INSERT INTO delivery_drills (id, org_id, source_id, drill_label, severity, case_id,
		                              status, outcome, failed_stage, started_by_label,
		                              started_at, deadline_at, finished_at)
		 VALUES ($1, $2, $3, $4, 'critical', $5, 'failed', '{}'::jsonb, 'case',
		         'the rollback suite', $6, $7, $7)`,
		id.New(), episodeScope.OrgID(), episodeHealth.SourceID, uuid.NewString(), liveCase,
		episodeAt, episodeAt.Add(time.Minute)); err != nil {
		t.Fatalf("seed a drill that failed at the case stage: %v", err)
	}
	if _, err := env.pool.Exec(env.ctx,
		`INSERT INTO ui_events (org_id, kind, resource, resource_id, payload)
		 VALUES ($1, 'case.upserted', 'case', $2, '{}'::jsonb)`,
		episodeScope.OrgID(), liveCase); err != nil {
		t.Fatalf("seed a case-spelled SSE event: %v", err)
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

	// ⭐⭐ 00052 down: the whole `case` vocabulary becomes the `occurrence` one
	// again. This is the FIRST step of the rollback, and every step below it
	// depends on it having worked — a name that did not come back is not a failed
	// assertion here, it is a 42P01 several steps down attributed to the wrong
	// migration.
	//
	// ⚠️⚠️ EVERY ASSERTION BELOW THIS LINE IS IN THE PRE-00052 VOCABULARY. That is
	// the subtle part of this file, and it is the reason four readings further down
	// were silently vacuous before this step existed: `alert_occurrences`, not
	// `alert_cases`; `occ_started_idx`, not `case_started_idx`;
	// `occ_group_live_idx`, not `case_group_live_idx`; `notif_occurrence_idx`, not
	// `notif_case_idx`. A post-rename name below reads 0 rows or "" for a reason
	// that has nothing to do with the migration being asserted, and passes.
	const (
		wantTables  = 1
		wantColumns = 7
		wantIndexes = 10
		// ⭐ TWO CONSTRAINT TOTALS, AND THE GAP BETWEEN THEM IS 00054. The list in
		// `episodeNames` is the FULL pre-00054 set of 27; three of them —
		// `case_reopen_ck`, `case_reopenof_ck` and `case_resolve_map_ck` — are
		// dropped at the top of the stack and restored by 00054's Down, which runs
		// before the post-`down(52)` reading below. So the same list is counted
		// twice against two different totals, and a Down that forgot one of the
		// three shows up HERE as a number rather than as a puzzle.
		wantConstraintsAtTop = 24
		wantConstraints      = 27
	)
	if tbl, col, con, idx := episodeVocabulary(caseNames); tbl != wantTables ||
		col != wantColumns || con != wantConstraintsAtTop || idx != wantIndexes {
		t.Fatalf("the `case` vocabulary at the top of the stack is %d/%d tables, %d/%d columns, "+
			"%d/%d constraints, %d/%d indexes — 00052's Up IS the rename and nothing else, so a "+
			"short reading here means every name asserted below is being asserted against a "+
			"schema that never finished renaming",
			tbl, wantTables, col, wantColumns, con, wantConstraintsAtTop, idx, wantIndexes)
	}
	if tbl, col, con, idx := episodeVocabulary(preRenameNames); tbl+col+con+idx != 0 {
		t.Fatalf("%d table, %d column, %d constraint and %d index name(s) from the pre-00052 "+
			"vocabulary still exist at the top of the stack — 00052 RENAMES rather than "+
			"duplicates, so anything left under the old spelling is a rename its Up missed",
			tbl, col, con, idx)
	}
	// The four row rewrites, counted where they are still spelled `case`. One row was
	// seeded into each above; a reading other than four means a sub-count names a
	// column that does not hold what this thinks it does, and the reading AFTER the
	// Down would then be zero for a reason that has nothing to do with 00052.
	countStrayCaseRows := func() int {
		t.Helper()
		var n int
		if err := env.pool.QueryRow(env.ctx,
			`SELECT (SELECT count(*) FROM ui_events WHERE kind = 'case.upserted' OR resource = 'case')
			      + (SELECT count(*) FROM enrichments WHERE subject_kind = 'case')
			      + (SELECT count(*) FROM delivery_drills WHERE failed_stage = 'case')
			      + (SELECT count(*) FROM alert_event_keys WHERE dedupe_key LIKE 'case:%')`).
			Scan(&n); err != nil {
			t.Fatalf("introspect the rows 00052's Down rewrites: %v", err)
		}
		return n
	}
	if n := countStrayCaseRows(); n != 4 {
		t.Fatalf("%d of the four `case`-spelled rows are visible at the top of the stack, want 4 "+
			"— the assertion after 00052's Down is that this figure reaches zero, and a figure "+
			"that was never four reaches zero without the Down doing anything at all", n)
	}
	// A column's declared nullability, as `information_schema` spells it: "YES" or
	// "NO", and "" when the column is absent. 00058 is the only migration in this
	// file whose subject is a NOT NULL rather than a constraint or a column, and
	// nullability is invisible to every other reading here — `countColumns` counts
	// the column either way, and no `pg_constraint` row carries it.
	columnNullability := func(table, column string) string {
		t.Helper()
		var nullable string
		if err := env.pool.QueryRow(env.ctx,
			// Both sides cast to text for the reason `clockDefaults` casts them: the
			// information_schema identifier columns are a domain over `name`.
			`SELECT coalesce(max(is_nullable), '') FROM information_schema.columns
			  WHERE table_name::text = $1 AND column_name::text = $2`, table, column).
			Scan(&nullable); err != nil {
			t.Fatalf("introspect %s.%s nullability: %v", table, column, err)
		}
		return nullable
	}

	// ⭐⭐ 00060 IS THREE ENUM NARROWINGS AND NOTHING ELSE: `notifications.reason`
	// lost `storm`, `alert_events.type` gained a refusal of the four damper
	// spellings that left with it, and `policies_reasons_ck`'s ceiling followed the
	// Reason enum from nineteen back to eighteen. Its Down re-widens all three.
	//
	// ⛔ ALL THREE CONSTRAINTS KEEP THEIR NAMES ACROSS THIS MIGRATION, so a name
	// count reads identically on both sides and proves nothing at all. The
	// DEFINITION is the property, the way 00039's and 00035's are: a Down that
	// dropped `notifications_reason_ck` and forgot to re-add the wide predicate
	// leaves the release this rolls back to unable to write the storm announcement
	// it still mints, and one that left `ev_type_ck`'s `NOT IN` standing refuses
	// four timeline event types that release still writes.
	//
	// ⚠️ A NARROWING IS THE HALF THAT CAN FAIL AND A WIDENING IS THE HALF THAT
	// CANNOT, which is exactly why the widening is the one asserted here rather
	// than believed: `ADD CONSTRAINT` with a looser predicate validates every row
	// trivially, so this Down cannot report its own failure at the exit code.
	if def := constraintDef("notifications_reason_ck", "notifications"); strings.Contains(def, "'storm'") {
		t.Fatalf("notifications_reason_ck still admits 'storm' at the top of the stack: %s — "+
			"00060's whole subject is that value leaving the enum with the damper it announced "+
			"(ADR 0042), and the Down assertion below cannot mean anything if the Up never "+
			"happened", def)
	} else if !strings.Contains(def, "'digest'") {
		t.Fatalf("notifications_reason_ck does not admit 'digest' at the top of the stack: %s — "+
			"00058 appended the nineteenth reason and 00060 removed only `storm`, so a reading "+
			"without it means one of the two migrations rewrote the list rather than editing it",
			def)
	}
	if def := constraintDef("ev_type_ck", "alert_events"); !strings.Contains(def, "group.storm_started") {
		t.Fatalf("ev_type_ck does not refuse the damper event spellings at the top of the "+
			"stack: %s — 00060 exists to add that NOT IN clause, and the Down assertion below "+
			"is about it going away again", def)
	}
	if def := policyReasonsCheck(); strings.Contains(def, "19") {
		t.Fatalf("policies_reasons_ck still carries a ceiling of 19 at the top of the stack: "+
			"%s — the ceiling IS the enum size, and 00060 moved it back to 18 when it deleted "+
			"`storm`; a ceiling of 19 over an eighteen-value vocabulary is a number no row "+
			"could ever test", def)
	}

	// 00066 removes `frozen` from the thread state vocabulary (git-bug e5c060b).
	// The assertion is on the CHECK rather than on a column, because a value leaving
	// an enum is invisible everywhere else: the column still exists, still holds
	// text, and only the constraint says which text.
	if def := constraintDef("threads_state_ck", "channel_threads"); strings.Contains(def, "frozen") {
		t.Fatalf("threads_state_ck still admits 'frozen' at the top of the stack: %s — "+
			"nothing ever wrote that state (`Freeze` had no production caller), and a "+
			"vocabulary that keeps a value no writer can produce documents a lifecycle "+
			"stop the code does not have", def)
	} else if !strings.Contains(def, "dead") {
		t.Fatalf("threads_state_ck no longer admits 'dead' at the top of the stack: %s — "+
			"00066 removes ONE value; a reading without `dead` means it rewrote the list "+
			"rather than editing it, and `threads_dead_ck` still pairs that state with "+
			"`dead_reason`", def)
	}
	if c := columnComment("channel_threads", "state"); strings.Contains(c, "frozen means the group closed") {
		t.Fatalf("channel_threads.state still promises a frozen state at the top of the "+
			"stack: %s — the sentence at 00011:197 IS the defect e5c060b was filed about, "+
			"and correcting it is half of what 00066 is for", c)
	}
	// The other half, and it is on a DIFFERENT TABLE. 00008:89 promised that going idle
	// closes the group "and freezes its thread" — the transition INTO the deleted state.
	// A column comment lives in the database, so only 00066's COMMENT ON changes what an
	// operator's `\d+ alert_groups` prints; correcting the source file would change
	// nothing that is already deployed.
	if c := columnComment("alert_groups", "last_activity_at"); strings.Contains(c, "freezes its thread") {
		t.Fatalf("alert_groups.last_activity_at still promises that closing freezes the "+
			"thread: %s — nothing ever froze anything, and this is the sentence an "+
			"operator reads out of the schema rather than out of the code", c)
	}

	down(66)

	if def := constraintDef("threads_state_ck", "channel_threads"); !strings.Contains(def, "frozen") {
		t.Fatalf("threads_state_ck did not re-admit 'frozen' after 00066's Down: %s — the "+
			"release this rolls back to has a `Freeze` method whose UPDATE writes exactly "+
			"that value, so without it the rollback lands on a schema that release cannot "+
			"write to", def)
	}
	if c := columnComment("alert_groups", "last_activity_at"); !strings.Contains(c, "freezes its thread") {
		t.Fatalf("alert_groups.last_activity_at did not get its old sentence back after "+
			"00066's Down: %s — the Down has to restore the comment it rewrote, or a "+
			"rollback leaves the schema describing a release it is no longer running", c)
	}

	// 00065 REVERTS 00063: the owner ruled one Case per conversation, so a policy
	// collapse decides nothing and `group_by` went. The assertion is the ABSENCE of
	// a column, which is the only reading that catches the failure this migration
	// exists to prevent — a knob an operator can still write and nothing honours,
	// which is the defect `0457f1f`, `35d4248`, `39e48e2` and `27a1860` all closed.
	//
	// `columnNullability` answers "" for a column that is not there, which is why an
	// absence can be asserted with the same helper as a presence.
	if nullable := columnNullability("notification_policies", "group_by"); nullable != "" {
		t.Fatalf("notification_policies.group_by is still present at the top of the "+
			"stack (is_nullable=%q) — 00065 drops it, and a surviving column is a "+
			"collapse list the policy API could still accept and return while no "+
			"delivery reads it", nullable)
	}
	if def := constraintDef("policies_group_by_ck", "notification_policies"); def != "" {
		t.Fatalf("policies_group_by_ck survived 00065 at the top of the stack: %s — the "+
			"constraint names the dropped column, so leaving it would make the schema "+
			"depend on DROP COLUMN's cascade rather than on this migration being right",
			def)
	}

	down(65)

	// 00065's Down puts the SHAPE back and cannot put the VALUES back: every policy
	// returns with the `{}` default because the Up dropped the arrays. That is the
	// whole loss, and it is a loss only on paper — nothing ever read the values, so
	// there is no behaviour to restore alongside them.
	if nullable := columnNullability("notification_policies", "group_by"); nullable != "NO" {
		t.Fatalf("notification_policies.group_by is is_nullable=%q after 00065's Down, "+
			"want NO — a nullable collapse list gives 'no collapse' two spellings, NULL "+
			"and {}, and the release this rolls back to could tell them apart", nullable)
	}
	if def := constraintDef("policies_group_by_ck", "notification_policies"); !strings.Contains(def, "8") {
		t.Fatalf("policies_group_by_ck did not come back bounded after 00065's Down: %s — "+
			"restoring the column without its bound rolls back to a schema the release "+
			"never shipped", def)
	}

	// 00064 makes the delivery target a PAIR (git-bug 7570090 stage 3). The
	// assertion that matters is not that the columns exist but that the EXCEPTION is
	// gone: `notifications_target_ck` read "every fact names a group EXCEPT a
	// digest", and a shape with an exception in it cannot absorb a third kind.
	if def := constraintDef("notifications_target_ck", "notifications"); def != "" {
		t.Fatalf("notifications_target_ck still stands at the top of the stack: %s — "+
			"the pair exists to retire it, and leaving both means a digest is still "+
			"the exception in a CHECK as well as an ordinary value in a column", def)
	}
	if def := constraintDef("notifications_convkind_ck", "notifications"); !strings.Contains(def, "digest") {
		t.Fatalf("notifications_convkind_ck does not bound the conversation vocabulary "+
			"at the top of the stack: %s", def)
	}
	for _, col := range []string{"conversation_kind", "conversation_id"} {
		if nullable := columnNullability("notifications", col); nullable != "NO" {
			t.Fatalf("notifications.%s is is_nullable=%q, want NO — EVERY row names a "+
				"conversation now, and a nullable half would re-create the exception "+
				"this migration removed", col, nullable)
		}
	}

	down(64)

	if def := constraintDef("notifications_target_ck", "notifications"); !strings.Contains(def, "digest") {
		t.Fatalf("notifications_target_ck did not come back after 00064's Down: %s — "+
			"the release this rolls back to has no conversation pair, so without the "+
			"CHECK a digest row and a group row are indistinguishable to it", def)
	}

	// 00063 moves the grouping decision onto the policy (git-bug 7570090 stage 2).
	// Its Up state — the NOT NULL column and its bound — is asserted after 00065's
	// Down above rather than a second time here: 00064 does not touch the column, so
	// a re-reading would restate a fact three lines of rollback ago and drift from it.
	down(63)

	if def := constraintDef("policies_group_by_ck", "notification_policies"); def != "" {
		t.Fatalf("policies_group_by_ck survived 00063's Down: %s — the constraint "+
			"names a column the Down drops, so leaving it would make the rollback "+
			"depend on drop order rather than on the Down being right", def)
	}

	// 00062 records the flap retirement on the columns themselves. The owner ruled the
	// detector is not needed (git-bug 752cb18), so these say "frozen" rather than the
	// live-state claim 00007 shipped and 00057's header repeated.
	for _, col := range []string{"flap_score", "is_flapping"} {
		if c := columnComment("alerts", col); !strings.Contains(c, "RETIRED IN PLACE") {
			t.Fatalf("alerts.%s does not say it is retired at the top of the stack: %s — "+
				"the column is frozen, nothing recomputes it, and a comment describing a "+
				"live detector sends an operator to trust a measurement taken at a time", col, c)
		}
	}

	down(62)

	for _, col := range []string{"flap_score", "is_flapping"} {
		if c := columnComment("alerts", col); strings.Contains(c, "RETIRED IN PLACE") {
			t.Fatalf("alerts.%s still says retired after 00062's Down: %s — the Down restores "+
				"00007's wording, which was true of the release this rolls back to", col, c)
		}
	}
	if c := columnComment("alerts", "flap_score"); !strings.Contains(c, "flap.score job") {
		t.Fatalf("alerts.flap_score did not get 00007's wording back after 00062's Down: %s", c)
	}

	// 00061 restates two table comments 00036 shipped and later changes made false.
	// A comment is not a constraint, so nothing else in this suite would notice it
	// silently reverting — which is exactly why it is pinned on both sides.
	if c := tableComment("ingest_batches"); !strings.Contains(c, "CHOSEN, NOT DERIVED") {
		t.Fatalf("ingest_batches does not say the thirty days is chosen at the top of the "+
			"stack: %s — 00061 exists to replace 00036's derivation claim, which `oto replay` "+
			"falsified when it started gating on supersession rather than on age", c)
	}
	if c := tableComment("ingest_rejections"); strings.Contains(c, "No API reads this table yet") {
		t.Fatalf("ingest_rejections still warns that no API reads it: %s — "+
			"GET /api/v1/sources/{id}/rejections is the feed it was waiting for and it "+
			"shipped, so the warning now hides the cost of lowering raw_retention_days", c)
	}

	down(61)

	if c := tableComment("ingest_batches"); !strings.Contains(c, "DERIVED, NOT CHOSEN") {
		t.Fatalf("ingest_batches did not get 00036's derivation wording back after 00061's "+
			"Down: %s — the Down restores the comment the release this rolls back to shipped, "+
			"and a Down that only dropped the new text would leave the schema describing "+
			"neither version", c)
	}
	if c := tableComment("ingest_rejections"); !strings.Contains(c, "No API reads this table yet") {
		t.Fatalf("ingest_rejections did not get 00036's warning back after 00061's Down: %s", c)
	}

	down(60)

	if def := constraintDef("notifications_reason_ck", "notifications"); !strings.Contains(def, "'storm'") {
		t.Fatalf("notifications_reason_ck did not get 'storm' back after 00060's Down: %s — the "+
			"constraint KEEPS ITS NAME across this migration, so a Down that dropped it and "+
			"forgot to re-add the wide predicate leaves the release this rolls back to unable "+
			"to record the one announcement it still knows how to mint", def)
	} else if !strings.Contains(def, "'digest'") {
		t.Fatalf("notifications_reason_ck lost 'digest' in 00060's Down: %s — the Down restores "+
			"the world 00058 left, which is nineteen reasons; restoring 00018's eighteen here "+
			"would silently undo a migration that is still applied", def)
	}
	if def := constraintDef("ev_type_ck", "alert_events"); strings.Contains(def, "group.storm_started") {
		t.Fatalf("ev_type_ck still refuses the damper event spellings after 00060's Down: %s — "+
			"the release this rolls back to can write all four, and a surviving NOT IN turns "+
			"every one of them into a 23514 on the timeline write", def)
	} else if !strings.Contains(def, "[a-z_]+") {
		t.Fatalf("ev_type_ck came back without the SHAPE rule after 00060's Down: %s — the "+
			"regex is the constraint's whole content on this side, and a Down that dropped the "+
			"CHECK without restoring it lets any string at all into a timeline type", def)
	}
	if def := policyReasonsCheck(); !strings.Contains(def, "19") {
		t.Fatalf("policies_reasons_ck did not get its ceiling of 19 back after 00060's Down: "+
			"%s — the release this rolls back to has nineteen reasons and a policy may name all "+
			"of them, so a ceiling of 18 outlaws a policy that release calls legal", def)
	}

	// ⭐⭐ 00059 TOOK STORM DAMPING OUT OF THE SCHEMA: `alert_groups.storm_mode`
	// and `storm_since` and the `groups_storm_ck` that paired them, plus
	// `channels.storm_notice_at`, are DROPPED, and `notifications_suppmap_ck`
	// narrows from 00018's EIGHT admitted values to six.
	//
	// ⛔ ITS DOWN IS THREE ADD COLUMNs AND A RE-WIDENING — every one of them the
	// direction that cannot fail, and therefore every one of them invisible at the
	// exit code. A Down that no-oped would be green, and would hand a rolled-back
	// release an `alert_groups` INSERT naming two columns the database does not
	// have and a `suppressed_reason` domain that refuses the two values it still
	// computes.
	//
	// ⭐ THE DEFAULT ON `storm_mode` IS ASSERTED, NOT JUST THE COLUMN, for the
	// reason 00038's is: the pair is all-or-nothing under `groups_storm_ck`, so the
	// only value a restored `storm_mode` may take beside a NULL `storm_since` is
	// `false` — and a nullable column with no default would 23502 the first
	// generation the rolled-back release writes.
	stormColumns := func() int {
		t.Helper()
		return countColumns("alert_groups", "storm_mode", "storm_since") +
			countColumns("channels", "storm_notice_at")
	}
	if n := stormColumns(); n != 0 {
		t.Fatalf("%d of 00059's three storm columns still exist at the top of the stack, want 0 "+
			"— the migration exists to drop them, and while they are there the schema carries "+
			"live state no writer can ever set again", n)
	}
	if def := constraintDef("groups_storm_ck", "alert_groups"); def != "" {
		t.Fatalf("groups_storm_ck exists at the top of the stack: %s — it pairs two columns "+
			"00059 dropped, so its presence means the Up left it behind", def)
	}
	suppressionMap := constraintDef("notifications_suppmap_ck", "notifications")
	for _, gone := range []string{"'storm'", "'flapping'"} {
		if strings.Contains(suppressionMap, gone) {
			t.Fatalf("notifications_suppmap_ck still admits %s at the top of the stack: %s — the "+
				"two dampers were the only values in this domain that were oto's own opinion "+
				"that a real signal was not worth mentioning, and 00059 exists to remove them",
				gone, suppressionMap)
		}
	}
	if !strings.Contains(suppressionMap, "'duplicate_render'") {
		t.Fatalf("notifications_suppmap_ck is not the six-value domain at the top of the stack: "+
			"%s — 00059 narrows it, it does not replace it", suppressionMap)
	}

	down(59)

	suppressionMap = constraintDef("notifications_suppmap_ck", "notifications")
	for _, want := range []string{
		"'no_policy'", "'throttled'", "'storm'", "'flapping'", "'snoozed'", "'verbosity'",
		"'channel_disabled'", "'duplicate_render'",
	} {
		if !strings.Contains(suppressionMap, want) {
			t.Fatalf("notifications_suppmap_ck did not come back as 00018's eight-value domain "+
				"after 00059's Down (missing %s): %s — the constraint KEEPS ITS NAME across this "+
				"migration, so a Down that dropped it and forgot the wide predicate leaves the "+
				"release this rolls back to unable to record the two suppressions it still "+
				"evaluates", want, suppressionMap)
		}
	}
	if n := countColumns("alert_groups", "storm_mode", "storm_since"); n != 2 {
		t.Fatalf("%d of alert_groups' two storm columns came back on 00059's Down, want 2 — the "+
			"Down of a DROP COLUMN is an ADD COLUMN, the one statement in a rollback that cannot "+
			"fail and therefore the one most likely never to have been run", n)
	}
	if n := countColumns("channels", "storm_notice_at"); n != 1 {
		t.Fatal("channels.storm_notice_at did not come back on 00059's Down — it is the " +
			"once-per-channel notice latch (00027, ADR 0020 Amendment 1), and the release this " +
			"rolls back to writes it on every storm it announces")
	}
	if def := constraintDef("groups_storm_ck", "alert_groups"); def == "" {
		t.Fatal("groups_storm_ck did not come back on 00059's Down — the two columns without " +
			"the CHECK that pairs them all-or-nothing let a generation claim it is storming " +
			"since never, which is the state the constraint has refused since 00008")
	}
	var stormNullable, stormDefault string
	if err := env.pool.QueryRow(env.ctx,
		`SELECT is_nullable, coalesce(column_default, '') FROM information_schema.columns
		  WHERE table_name = 'alert_groups' AND column_name = 'storm_mode'`).
		Scan(&stormNullable, &stormDefault); err != nil {
		t.Fatalf("introspect the restored storm_mode: %v", err)
	}
	if stormNullable != "NO" || !strings.HasPrefix(stormDefault, "false") {
		t.Fatalf("alert_groups.storm_mode came back is_nullable=%q default=%q, want NO / false "+
			"— `false` beside a NULL storm_since is the only pair groups_storm_ck admits, which "+
			"is what lets the Down re-add the constraint with no backfill at all",
			stormNullable, stormDefault)
	}

	// ⭐⭐ 00058 GAVE A NOTIFICATION A SUBJECT THAT IS NOT A ROW: a digest is a
	// WINDOW over a namespace, so `notifications.group_id` — NOT NULL since 00011 —
	// became nullable behind `notifications_target_ck`, four columns arrived, two
	// subject-kind CHECKs admitted `digest`, and `notif_digest_uniq` made one
	// digest per (tenant, policy, window).
	//
	// ⛔ THE NOT NULL IS THE PROPERTY THAT NO OTHER READING IN THIS FILE CAN SEE.
	// `countColumns` counts `group_id` identically on both sides and no
	// `pg_constraint` row carries nullability, so a Down that dropped the digest
	// columns and forgot `SET NOT NULL` would be green on every other assertion
	// here while leaving the release it rolled back to able to write a fact with
	// nowhere to deliver it — the exact invariant 00058 was careful to keep as a
	// CHECK when it relaxed the column.
	//
	// ⛔ AND THE REASON LIST GOES BACK TO EIGHTEEN, WHICH IS THE SAME NUMBER 00060
	// LEFT AND A DIFFERENT LIST. 00060's Down restored `storm` and kept `digest`,
	// nineteen; this Down removes `digest` and keeps `storm`, the eighteen 00018
	// left. A Down that re-issued 00060's predicate would be off by exactly one
	// value in each direction and would still be eighteen long.
	digestColumns := func() int {
		t.Helper()
		return countColumns("notification_policies", "digest_window_s", "digest_floor") +
			countColumns("notifications", "digest_window_start", "digest_count")
	}
	if n := digestColumns(); n != 4 {
		t.Fatalf("%d of 00058's four digest columns exist at the top of the stack, want 4 — the "+
			"Down assertion below is about them going away, and a short reading here means it "+
			"would reach zero without the Down doing anything", n)
	}
	if def := constraintDef("notifications_target_ck", "notifications"); !strings.Contains(def, "'digest'") {
		t.Fatalf("notifications_target_ck does not exempt the digest at the top of the stack: "+
			"%s — it is the half of the old `group_id NOT NULL` that is true of every "+
			"Notification, and 00058 keeps it as a CHECK precisely so that relaxing the column "+
			"could not quietly let an ordinary fact lose its destination", def)
	}
	if nullable := columnNullability("notifications", "group_id"); nullable != "YES" {
		t.Fatalf("notifications.group_id is is_nullable=%q at the top of the stack, want YES — "+
			"00058's whole relaxation is that column, and the Down assertion below cannot mean "+
			"anything if the Up never happened", nullable)
	}
	if def := indexDef("notif_digest_uniq"); def == "" {
		t.Fatal("notif_digest_uniq is absent at the top of the stack; it is the readable " +
			"spelling of the digest idempotency key and the cursor the tick reads backwards " +
			"for the last window it covered")
	} else if !strings.Contains(def, "digest_window_start") || !strings.Contains(def, "UNIQUE") {
		t.Fatalf("notif_digest_uniq is not the unique index over the window at the top of the "+
			"stack: %s — one digest per (tenant, policy, window) is the whole claim, and an "+
			"index of that name without the window column or without UNIQUE is a different one",
			def)
	}
	for _, kind := range []struct{ name, table string }{
		{"notifications_subjkind_ck", "notifications"},
		{"threads_subjkind_ck", "channel_threads"},
	} {
		if def := constraintDef(kind.name, kind.table); !strings.Contains(def, "'digest'") {
			t.Fatalf("%s does not admit 'digest' at the top of the stack: %s — 00058 widens "+
				"both, and the Down assertion below is about both narrowing again",
				kind.name, def)
		}
	}

	down(58)

	if n := digestColumns(); n != 0 {
		t.Fatalf("%d of 00058's four digest columns survived its Down, want 0 — a rolled-back "+
			"release neither reads nor writes them, so what would be left is a window and a "+
			"count nothing maintains", n)
	}
	if nullable := columnNullability("notifications", "group_id"); nullable != "NO" {
		t.Fatalf("notifications.group_id is is_nullable=%q after 00058's Down, want NO — this "+
			"is the assertion no other reading in this file can make: the column counts the "+
			"same on both sides, so a Down that dropped the digest columns and forgot "+
			"SET NOT NULL is green everywhere else while letting a fact exist with nowhere to "+
			"deliver it", nullable)
	}
	for _, name := range []string{"notifications_target_ck", "notifications_digest_ck"} {
		if def := constraintDef(name, "notifications"); def != "" {
			t.Fatalf("%s survived 00058's Down: %s — it constrains columns that no longer exist "+
				"on this side, so the next Up would fail to create them", name, def)
		}
	}
	if def := indexDef("notif_digest_uniq"); def != "" {
		t.Fatalf("notif_digest_uniq survived 00058's Down: %s — it is partial on a "+
			"`subject_kind` value the narrowed CHECK no longer admits and indexes a column that "+
			"has been dropped", def)
	}
	for _, kind := range []struct{ name, table string }{
		{"notifications_subjkind_ck", "notifications"},
		{"threads_subjkind_ck", "channel_threads"},
	} {
		def := constraintDef(kind.name, kind.table)
		if strings.Contains(def, "'digest'") {
			t.Fatalf("%s still admits 'digest' after 00058's Down: %s — the constraint keeps its "+
				"name across this migration, so the rollback restored the name and not the "+
				"domain", kind.name, def)
		}
		if !strings.Contains(def, "'case'") || !strings.Contains(def, "'alert_group'") {
			t.Fatalf("%s did not come back as 00056's three-kind domain after 00058's Down: %s "+
				"— narrowing past the migration that is still applied below it would leave a "+
				"database no migration in the history describes", kind.name, def)
		}
	}
	if def := constraintDef("notifications_reason_ck", "notifications"); strings.Contains(def, "'digest'") {
		t.Fatalf("notifications_reason_ck still admits 'digest' after 00058's Down: %s — the "+
			"nineteenth reason arrived with this migration and leaves with it", def)
	} else if !strings.Contains(def, "'storm'") {
		t.Fatalf("notifications_reason_ck is not the eighteen 00018 left after 00058's Down: "+
			"%s — this Down and 00060's both land on eighteen values and they are DIFFERENT "+
			"eighteen, so a Down that re-issued the other one's predicate is off by one value "+
			"in each direction and still the right length", def)
	}
	if def := policyReasonsCheck(); strings.Contains(def, "19") {
		t.Fatalf("policies_reasons_ck kept its ceiling of 19 after 00058's Down: %s — the "+
			"ceiling is the enum size and the nineteenth value has just been removed, so a "+
			"policy could name a vocabulary one longer than the one that exists", def)
	}

	// ⭐⭐ 00057 LET A CASE OUTLIVE THE FLAP: `case_policy_config` carries the
	// retention window W, and `alert_cases` gained `resolve_pending_at` /
	// `resolve_pending_end_at`, four CHECKs and one partial index for the delayed
	// close.
	//
	// ⛔ ITS DOWN SPENDS THE RECEIPT BEFORE DROPPING IT, AND THAT IS THE HALF
	// WORTH ASSERTING. Dropping two columns and a table cannot fail; what can fail
	// — silently, at the exit code — is the UPDATE that COMPLETES every pending
	// close first. A Down that dropped the columns straight away would forget an
	// upstream resolve oto was already holding, and the rolled-back release would
	// end those episodes through the reaper as `expired`/`timeout`: oto claiming it
	// stopped hearing about an alert whose resolution it had in hand.
	//
	// ⚠️ THAT PROPERTY IS VACUOUS WITHOUT A PENDING ROW, the way 00049's and
	// 00048's were until an acked episode and an active snooze were seeded. So the
	// live episode seeded above the rollback is ARMED here — it is open, carries no
	// suppression reason, and started before the due time, which is exactly what
	// `case_pending_open_ck`, `case_pending_supp_ck` and `case_pending_order_ck`
	// require — and the completed close is read back off it BY VALUE.
	//
	// ⚠️ THE VOCABULARY IS STILL THE `case` ONE: `down(52)` is far below, and
	// `alert_cases.state` is 00054's `open | closed` here, not 00007's four values.
	if n := countTables("case_policy_config"); n != 1 {
		t.Fatal("case_policy_config is absent at the top of the stack; 00057 exists to create " +
			"it, and without it W has no home and every case closes on the resolve")
	}
	if n := countColumns("alert_cases", "resolve_pending_at", "resolve_pending_end_at"); n != 2 {
		t.Fatalf("%d of 00057's two pending-close columns exist at the top of the stack, want 2 "+
			"— the Down assertion below is about them going away, and about the close they "+
			"carry being performed on the way out", n)
	}
	for _, name := range []string{
		"case_pending_pair_ck", "case_pending_open_ck", "case_pending_order_ck",
		"case_pending_supp_ck",
	} {
		if def := constraintDef(name, "alert_cases"); def == "" {
			t.Fatalf("%s is absent at the top of the stack — 00057 adds all four, and they are "+
				"what make the delayed close single-shot rather than a second ending", name)
		}
	}
	if def := indexDef("case_close_due_idx"); def == "" {
		t.Fatal("case_close_due_idx is absent at the top of the stack; it is the whole scan " +
			"case.reap performs to find due closes")
	} else if !strings.Contains(def, "resolve_pending_at IS NOT NULL") {
		t.Fatalf("case_close_due_idx is not partial at the top of the stack: %s — unpartialled "+
			"it spans every episode ever opened instead of the handful inside a retention "+
			"window, which on a deployment that sets no W is none of them", def)
	}
	// The pending close 00057's Down has to complete. `pendingEnd` is the UPSTREAM
	// claim the close will stamp as `ended_at`, and it is after `started_at`
	// (episodeAt + 2h) so that `case_pending_order_ck` and `case_order_ck` both
	// admit it. The due time is the same instant, which is what a W of zero would
	// have produced.
	pendingEnd := episodeAt.Add(3 * time.Hour)
	if _, err := env.pool.Exec(env.ctx,
		`UPDATE alert_cases
		    SET resolve_pending_at = $2, resolve_pending_end_at = $2, updated_at = now()
		  WHERE id = $1`, liveCase, pendingEnd); err != nil {
		t.Fatalf("arm a pending close on the live episode: %v — 00057 exists to make these two "+
			"columns writable on an open, unsuppressed episode, so a failure here means its Up "+
			"never added them or one of its four CHECKs says something other than it claims", err)
	}

	down(57)

	var closedState, closedReason string
	var closedEnded *time.Time
	if err := env.pool.QueryRow(env.ctx,
		`SELECT state, coalesce(resolve_reason, ''), ended_at FROM alert_cases WHERE id = $1`,
		liveCase).Scan(&closedState, &closedReason, &closedEnded); err != nil {
		t.Fatalf("read the episode whose close was pending: %v", err)
	}
	if closedState != "closed" || closedReason != "upstream" {
		t.Fatalf("the episode holding a pending close reads state=%q resolve_reason=%q after "+
			"00057's Down, want closed/upstream — the Down has to SPEND the receipt before "+
			"dropping the column that holds it, or the rolled-back release ends this episode "+
			"through the reaper as expired/timeout: oto claiming it stopped hearing about an "+
			"alert whose resolution it was already holding", closedState, closedReason)
	}
	if closedEnded == nil || !closedEnded.Equal(pendingEnd) {
		t.Fatalf("the completed close stamped ended_at=%v, want the upstream claim %v — closing "+
			"at the sweep's clock instead would charge W to the signal's firing duration, and "+
			"every reader of ended_at would report an episode longer than the signal burned "+
			"(R8)", closedEnded, pendingEnd)
	}
	if n := countColumns("alert_cases", "resolve_pending_at", "resolve_pending_end_at"); n != 0 {
		t.Fatalf("%d of 00057's two pending-close columns survived its Down, want 0 — the "+
			"release this rolls back to has no sweep that reads them, so what would be left is "+
			"a due time nothing will ever act on", n)
	}
	for _, name := range []string{
		"case_pending_pair_ck", "case_pending_open_ck", "case_pending_order_ck",
		"case_pending_supp_ck",
	} {
		if def := constraintDef(name, "alert_cases"); def != "" {
			t.Fatalf("%s survived 00057's Down: %s — it constrains columns that no longer exist "+
				"on this side", name, def)
		}
	}
	if def := indexDef("case_close_due_idx"); def != "" {
		t.Fatalf("case_close_due_idx survived 00057's Down: %s", def)
	}
	if n := countTables("case_policy_config"); n != 0 {
		t.Fatal("case_policy_config survived 00057's Down, so a rolled-back release keeps a " +
			"table nothing writes and nothing reads, holding a window no code evaluates")
	}

	// ⭐⭐ 00056 GAVE A NOTIFICATION A SUBJECT: both `subject_kind` CHECKs widened
	// from the single value `alert_group` to `alert | case | alert_group`,
	// `notifications_subject_ck` tied `subject_id` to the id column its kind names,
	// and `notif_group_idx` gave the two counting readers back the index the
	// widening took from them.
	//
	// ⛔ ITS DOWN IS THE ONE IN THIS FILE THAT REFUSES TO RUN RATHER THAN LOSE
	// SOMETHING, and the refusal is EXERCISED here the way 00039's is rather than
	// believed. `notifications` is NORMALISED first — `subject_kind` back to
	// `alert_group`, `subject_id` back to `group_id`, which is the pair release N
	// wrote and which loses nothing, because the alert or case the row was about is
	// still on `alert_id` / `case_id`. `channel_threads` deliberately gets NO such
	// normalisation: a thread keyed by a case cannot be re-keyed onto its group
	// without colliding under `threads_subject_uniq`, and deleting it would destroy
	// oto's memory of Slack (C9). So a non-group thread makes this Down ABORT, which
	// is the honest outcome for a provider handle a rollback cannot reconstruct.
	//
	// ⛔ AND THE ABORT HAS TO BE ATOMIC, for the reason 00039's does: the Down drops
	// `notif_group_idx` and rewrites `notifications` BEFORE it reaches the narrowing
	// that fails. A non-transactional Down would leave the index gone, the subjects
	// flattened and goose still claiming 00056 is applied — neither schema, and
	// nothing to roll forward to.
	for _, kind := range []struct{ name, table string }{
		{"notifications_subjkind_ck", "notifications"},
		{"threads_subjkind_ck", "channel_threads"},
	} {
		def := constraintDef(kind.name, kind.table)
		if !strings.Contains(def, "'alert'") || !strings.Contains(def, "'case'") {
			t.Fatalf("%s is not the three-kind domain above 00056's Down: %s — the migration's "+
				"whole subject is that a subject VARIES, and the Down assertion below cannot "+
				"mean anything if the Up never happened", kind.name, def)
		}
	}
	if def := constraintDef("notifications_subject_ck", "notifications"); def == "" {
		t.Fatal("notifications_subject_ck is absent above 00056's Down — it is the CHECK that " +
			"stands in for the foreign key a three-table reference cannot have, and without it " +
			"subject_id means whatever the reader assumes it means")
	} else if !strings.Contains(def, "alert_id") || !strings.Contains(def, "case_id") {
		t.Fatalf("notifications_subject_ck does not tie subject_id to the typed columns: %s", def)
	}
	if def := indexDef("notif_group_idx"); def == "" {
		t.Fatal("notif_group_idx is absent above 00056's Down; widening subject_kind took the " +
			"policy throttle and the group notification receipt off notif_subject_idx, whose " +
			"leading column was a constant while only one kind existed, and nothing else " +
			"indexes group_id — the 00011 FK creates no index on the referencing side")
	}
	// The thread the narrowing cannot re-key. A `webhook` channel is used because
	// `channels_cred_ck` exempts it from carrying a credential, and the thread is
	// left in `opening` with no provider handle so that `threads_open_ck` and
	// `threads_ts_ck` are satisfied without inventing a Slack ts. `created_at` and
	// `updated_at` are NAMED on both inserts: 00032 took `channels`' DEFAULT now()
	// away and 00034 took `channel_threads`', and both of those are BELOW this step
	// and therefore still applied.
	threadChannel, caseThread := id.New(), id.New()
	if _, err := env.pool.Exec(env.ctx,
		`INSERT INTO channels (id, org_id, type, name, config, created_at, updated_at)
		 VALUES ($1, $2, 'webhook', 'rollback-suite-thread',
		         '{"url":"http://webhook.test/rollback"}'::jsonb, $3, $3)`,
		threadChannel, episodeScope.OrgID(), time.Now().UTC()); err != nil {
		t.Fatalf("seed the channel the thread hangs off: %v", err)
	}
	if _, err := env.pool.Exec(env.ctx,
		`INSERT INTO channel_threads (id, org_id, channel_id, subject_kind, subject_id,
		                              created_at, updated_at)
		 VALUES ($1, $2, $3, 'case', $4, $5, $5)`,
		caseThread, episodeScope.OrgID(), threadChannel, liveCase, time.Now().UTC()); err != nil {
		t.Fatalf("open a case-keyed conversation: %v — 00056 exists to make a non-group thread "+
			"subject storable, so a 23514 here means its Up never widened threads_subjkind_ck", err)
	}

	// ⛔ 00056 down, ATTEMPT ONE: REFUSED, because a case-keyed thread is live.
	if top := appliedTop(); top != 56 {
		t.Fatalf("about to attempt %s against a live case-keyed thread, but the top applied "+
			"migration is %s", migrate.FormatVersion(56), migrate.FormatVersion(top))
	}
	threadRefusal := migrate.Down(env.ctx, dsn)
	if threadRefusal == nil {
		t.Fatal("00056's Down succeeded with a case-keyed row in channel_threads — either the " +
			"narrowed CHECK is not being re-added, or it was added NOT VALID, or somebody gave " +
			"channel_threads the normalisation the header refuses it: re-keying the thread onto " +
			"its group collides under threads_subject_uniq and deleting it destroys oto's only " +
			"memory of a Slack conversation (C9)")
	}
	// Named, so that this step cannot start passing on an unrelated failure.
	if !strings.Contains(threadRefusal.Error(), "threads_subjkind_ck") {
		t.Fatalf("00056's Down failed, but not on the narrowed thread CHECK: %v", threadRefusal)
	}
	if top := appliedTop(); top != 56 {
		t.Fatalf("00056 is recorded as %s after a FAILED Down — the rollback is not atomic, so "+
			"the database is in a state no migration describes", migrate.FormatVersion(top))
	}
	if def := indexDef("notif_group_idx"); def == "" {
		t.Fatal("notif_group_idx was dropped by a Down that then failed; the DROP INDEX, the " +
			"subject normalisation and the two CHECK narrowings have to succeed or fail together")
	}
	if def := constraintDef("notifications_subjkind_ck", "notifications"); !strings.Contains(def, "'case'") {
		t.Fatalf("notifications_subjkind_ck was narrowed by a Down that then failed: %s", def)
	}

	// What an operator holding this rollback has to decide first, and the reason
	// the migration refuses to decide it for them. Here the conversation is a
	// fixture and nothing was ever posted to it, so one DELETE stands in for that
	// decision — what is under test is the migration, not the operator.
	if _, err := env.pool.Exec(env.ctx,
		`DELETE FROM channel_threads WHERE id = $1`, caseThread); err != nil {
		t.Fatalf("dispose of the case-keyed thread: %v", err)
	}

	down(56)

	for _, kind := range []struct{ name, table string }{
		{"notifications_subjkind_ck", "notifications"},
		{"threads_subjkind_ck", "channel_threads"},
	} {
		def := constraintDef(kind.name, kind.table)
		if strings.Contains(def, "'case'") || strings.Contains(def, "'alert'") {
			t.Fatalf("%s still admits a non-group subject after 00056's Down: %s — both "+
				"constraints KEEP THEIR NAMES across this migration, so a Down that dropped "+
				"them and forgot to re-add 00011's single-value predicate leaves the release "+
				"this rolls back to able to store a subject it cannot resolve", kind.name, def)
		}
		if !strings.Contains(def, "'alert_group'") {
			t.Fatalf("%s did not come back at all after 00056's Down: %s — a CHECK dropped and "+
				"not restored is looser than either release ever intended", kind.name, def)
		}
	}
	if def := constraintDef("notifications_subject_ck", "notifications"); def != "" {
		t.Fatalf("notifications_subject_ck survived 00056's Down: %s — its `case` and `alert` "+
			"arms name a subject_kind the narrowed CHECK no longer admits, and the release this "+
			"rolls back to has never heard of the constraint at all", def)
	}
	if def := indexDef("notif_group_idx"); def != "" {
		t.Fatalf("notif_group_idx survived 00056's Down: %s — the two counting readers go back "+
			"to riding notif_subject_idx, whose leading column is a constant again once "+
			"subject_kind can hold only one value", def)
	}

	// ⭐⭐ 00055 MADE SUPPRESSION AN AXIS: `alerts.state` narrowed to
	// `firing | resolved | expired`, two columns arrived beside it, and
	// `alerts_open_idx` stopped spelling liveness as a disjunction.
	//
	// ⛔ THE DEFINITIONS ARE WHAT IS READ, NEVER THE NAMES — the same rule the
	// 00054 block below states, and it bites harder here. `alerts_state_ck` and
	// `alerts_open_idx` both EXIST ON BOTH SIDES and say different things on each,
	// so a Down that dropped and forgot to re-add either would leave a release
	// that cannot write `state = 'suppressed'` — the only value it knows for a
	// silenced alert — and an index whose partial predicate excludes exactly the
	// rows the landing page asks for.
	//
	// ⭐ THE THREE NEW CONSTRAINTS ARE ABSENT BELOW THIS MIGRATION, which makes
	// them the strongest readings in the block: they cannot be satisfied by a
	// stale object, only by the Down having genuinely dropped them.
	alertColumnType := func(column string) string {
		t.Helper()
		var typ string
		if err := env.pool.QueryRow(env.ctx,
			`SELECT coalesce(max(data_type), '') FROM information_schema.columns
			  WHERE table_name = 'alerts' AND column_name = $1`, column).Scan(&typ); err != nil {
			t.Fatalf("introspect alerts.%s: %v", column, err)
		}
		return typ
	}

	alertState := constraintDef("alerts_state_ck", "alerts")
	if strings.Contains(alertState, "'suppressed'") {
		t.Fatalf("alerts_state_ck still admits 'suppressed' at the top of the stack: %s — "+
			"00055's whole subject is that value domain, and the Down assertion below cannot "+
			"mean anything if the Up never happened", alertState)
	}
	for _, want := range []string{"'firing'", "'resolved'", "'expired'"} {
		if !strings.Contains(alertState, want) {
			t.Fatalf("alerts_state_ck does not admit %s: %s", want, alertState)
		}
	}
	if def := constraintDef("alerts_suppress_ck", "alerts"); !strings.Contains(def, "'firing'") {
		t.Fatalf("alerts_suppress_ck does not bind the axis to the firing state: %s — it is "+
			"what keeps a silence witness off a resolved alert, so oto cannot go on saying "+
			"\"silenced by <id>\" about a signal that has ended", def)
	}
	if def := constraintDef("alerts_supreason_ck", "alerts"); !strings.Contains(def, "'inhibition'") {
		t.Fatalf("alerts_supreason_ck does not mirror Alertmanager's four reasons: %s", def)
	}
	if def := constraintDef("alerts_suppby_ck", "alerts"); !strings.Contains(def, "jsonb_typeof") {
		t.Fatalf("alerts_suppby_ck is not the JSON-object check: %s", def)
	}
	openIdx := indexDef("alerts_open_idx")
	if !strings.Contains(openIdx, "(state = 'firing'::text)") {
		t.Fatalf("alerts_open_idx does not state liveness as a single equality: %s — the "+
			"disjunction it replaced is 00007's workaround for `suppressed` occupying the "+
			"slot `firing` needed, and removing it is the observable point of 00055", openIdx)
	}
	if strings.Contains(openIdx, "'suppressed'") {
		t.Fatalf("alerts_open_idx still names 'suppressed': %s", openIdx)
	}
	for _, col := range []struct{ name, typ string }{
		{"suppression_reason", "text"},
		{"suppressed_by", "jsonb"},
	} {
		if typ := alertColumnType(col.name); typ != col.typ {
			t.Fatalf("alerts.%s is %q at the top of the stack, want %q — 00055 adds it, and "+
				"the Down assertion below is about it going away", col.name, typ, col.typ)
		}
	}

	down(55)

	alertState = constraintDef("alerts_state_ck", "alerts")
	for _, want := range []string{"'firing'", "'suppressed'", "'resolved'", "'expired'"} {
		if !strings.Contains(alertState, want) {
			t.Fatalf("alerts_state_ck did not come back as the four-value domain after 00055's "+
				"Down (missing %s): %s — the constraint KEEPS ITS NAME across this migration, "+
				"so a Down that dropped it and forgot to re-add the old predicate leaves the "+
				"release this rolls back to unable to record a silenced alert at all",
				want, alertState)
		}
	}
	for _, name := range []string{"alerts_suppress_ck", "alerts_supreason_ck", "alerts_suppby_ck"} {
		if def := constraintDef(name, "alerts"); def != "" {
			t.Fatalf("%s survived 00055's Down: %s — it constrains a column that no longer "+
				"exists on this side, so the next Up would fail to create it", name, def)
		}
	}
	openIdx = indexDef("alerts_open_idx")
	if !strings.Contains(openIdx, "'suppressed'") {
		t.Fatalf("alerts_open_idx did not get its disjunction back after 00055's Down: %s — "+
			"the index keeps its name, so a rollback that restored the name and not the "+
			"predicate hides every silenced alert from the release's landing page", openIdx)
	}
	for _, col := range []string{"suppression_reason", "suppressed_by"} {
		if typ := alertColumnType(col); typ != "" {
			t.Fatalf("alerts.%s is still present (%s) after 00055's Down", col, typ)
		}
	}

	// ⭐⭐ 00054 NARROWED `alert_cases.state` TO `open | closed` AND DROPPED THE TWO
	// RE-FIRE COLUMNS, so its Down has to restore a VALUE DOMAIN, three CHECKs and
	// two columns — and every one of those is invisible to a name check.
	//
	// ⛔ THE DEFINITIONS ARE WHAT IS READ, NEVER THE NAMES. Four of the five
	// constraints keep their names across this migration: `case_state_ck`,
	// `case_terminal_ended`, `case_resolve_ck` and `case_suppress_ck` all exist on
	// both sides and say completely different things on each. A Down that dropped
	// them and forgot to re-add the OLD predicates would leave a database that
	// accepts `state = 'open'` on a release that has never heard the word — and a
	// name-counting assertion would pass while it did.
	//
	// ⭐ `case_resolve_map_ck` IS THE ONE THAT IS ABSENT ON THIS SIDE, which makes
	// it the strongest reading in the block: it cannot be satisfied by a stale
	// object, only by the Down having genuinely re-created it.
	caseStateDDL := func() (state, terminal, resolve, resolveMap, suppress string) {
		t.Helper()
		return constraintDef("case_state_ck", "alert_cases"),
			constraintDef("case_terminal_ended", "alert_cases"),
			constraintDef("case_resolve_ck", "alert_cases"),
			constraintDef("case_resolve_map_ck", "alert_cases"),
			constraintDef("case_suppress_ck", "alert_cases")
	}
	caseColumnType := func(column string) string {
		t.Helper()
		var typ string
		if err := env.pool.QueryRow(env.ctx,
			`SELECT coalesce(max(data_type), '') FROM information_schema.columns
			  WHERE table_name = 'alert_cases' AND column_name = $1`, column).Scan(&typ); err != nil {
			t.Fatalf("introspect alert_cases.%s: %v", column, err)
		}
		return typ
	}

	state, terminal, resolve, resolveMap, suppress := caseStateDDL()
	if !strings.Contains(state, "'open'") || !strings.Contains(state, "'closed'") ||
		strings.Contains(state, "'firing'") {
		t.Fatalf("case_state_ck does not admit open|closed at the top of the stack: %s — "+
			"00054's whole subject is that value domain, and the Down assertion below cannot "+
			"mean anything if the Up never happened", state)
	}
	if !strings.Contains(terminal, "'closed'") {
		t.Fatalf("case_terminal_ended is not stated in the narrowed vocabulary: %s", terminal)
	}
	if !strings.Contains(resolve, "'closed'") {
		t.Fatalf("case_resolve_ck is not stated in the narrowed vocabulary: %s — since 00054 "+
			"resolve_reason is the SOLE record of resolved-versus-expired, so this constraint "+
			"is what stops a closed episode saying nothing at all about how it ended", resolve)
	}
	if resolveMap != "" {
		t.Fatalf("case_resolve_map_ck still exists at the top of the stack: %s — it locked "+
			"`state` to `resolve_reason`, and there is no longer a state side to lock", resolveMap)
	}
	if strings.Contains(suppress, "'suppressed'") {
		t.Fatalf("case_suppress_ck still names the `suppressed` state: %s — the state does not "+
			"exist on this side of 00054", suppress)
	}
	for _, col := range []string{"reopen_count", "reopen_of"} {
		if typ := caseColumnType(col); typ != "" {
			t.Fatalf("alert_cases.%s is still present (%s) at the top of the stack — 00054 "+
				"drops it, and the Down assertion below is about it coming back", col, typ)
		}
	}

	down(54)

	state, terminal, resolve, resolveMap, suppress = caseStateDDL()
	if !strings.Contains(state, "'firing'") || !strings.Contains(state, "'suppressed'") ||
		!strings.Contains(state, "'resolved'") || !strings.Contains(state, "'expired'") {
		t.Fatalf("case_state_ck did not come back as the four-value domain after 00054's Down: "+
			"%s — the constraint KEEPS ITS NAME across this migration, so a Down that dropped "+
			"it and forgot to re-add the old predicate leaves the release this rolls back to "+
			"unable to write a single episode", state)
	}
	if strings.Contains(state, "'open'") {
		t.Fatalf("case_state_ck still admits 'open' after its Down: %s — the rollback restored "+
			"the name and not the domain", state)
	}
	if !strings.Contains(terminal, "'resolved'") || !strings.Contains(terminal, "'expired'") {
		t.Fatalf("case_terminal_ended was not restored to the four-value vocabulary: %s — the "+
			"release below 00054 writes `state = 'resolved'` with an `ended_at`, and this "+
			"predicate is what would refuse it", terminal)
	}
	if !strings.Contains(resolve, "'resolved'") || !strings.Contains(resolve, "'expired'") {
		t.Fatalf("case_resolve_ck was not restored to the four-value vocabulary: %s", resolve)
	}
	if resolveMap == "" {
		t.Fatalf("case_resolve_map_ck did not come back from 00054's Down — it is the " +
			"constraint that stops oto claiming `resolved` when it means `expired`, and it is " +
			"absent on the other side, so nothing but the Down can have created it")
	}
	if !strings.Contains(resolveMap, "'upstream'") || !strings.Contains(resolveMap, "'timeout'") {
		t.Fatalf("case_resolve_map_ck came back without the pairing it exists for: %s", resolveMap)
	}
	if !strings.Contains(suppress, "'suppressed'") {
		t.Fatalf("case_suppress_ck was not restored to the biconditional on the `suppressed` "+
			"state: %s — it also keeps its name across 00054", suppress)
	}
	for _, col := range []struct{ name, typ string }{
		{"reopen_count", "integer"},
		{"reopen_of", "uuid"},
	} {
		if typ := caseColumnType(col.name); typ != col.typ {
			t.Fatalf("alert_cases.%s came back as %q, want %q — 00054's Down ADDs the column "+
				"and rebuilds reopen_of from `seq - 1`, which is the half that can fail",
				col.name, typ, col.typ)
		}
	}
	if def := constraintDef("case_reopenof_ck", "alert_cases"); def == "" {
		t.Fatalf("case_reopenof_ck did not come back — a column restored without the CHECK " +
			"that stopped it pointing at its own row is a column the release below cannot trust")
	}

	// ⭐ 00053 WIDENED TWO INDEXES BY THE KEYSET TIEBREAK, IN PLACE. Both keep
	// their names, so the only thing that proves the Down ran is the DEFINITION:
	// a name coming back says nothing, and an index that kept `id` after the
	// rollback is the release-before serving `GET /api/v1/cases` with a sort key
	// it does not know it has.
	for _, idx := range []struct{ name, widened string }{
		{"case_ack_idx", "id DESC"},
		{"case_started_idx", "id)"},
	} {
		if def := indexDef(idx.name); !strings.Contains(def, idx.widened) {
			t.Fatalf("%s is not widened at the top of the stack: %s — 00053's whole subject is "+
				"that column, and a Down assertion below it cannot mean anything if the Up "+
				"never happened", idx.name, def)
		}
	}
	down(53)
	for _, idx := range []struct{ name, widened string }{
		{"case_ack_idx", "id DESC"},
		{"case_started_idx", "id)"},
	} {
		def := indexDef(idx.name)
		if def == "" {
			t.Fatalf("%s did not come back from 00053's Down — it DROPs and re-CREATEs under "+
				"the same name, so a Down that half-ran leaves the queue behind "+
				"`escalation.check` with no index at all", idx.name)
		}
		if strings.Contains(def, idx.widened) {
			t.Fatalf("%s still carries the 00053 tiebreak after its Down: %s — the rollback "+
				"restored the name and not the shape", idx.name, def)
		}
	}
	if def := indexDef("case_ack_idx"); !strings.Contains(def, "ended_at IS NULL") {
		t.Fatalf("case_ack_idx came back without its partial predicate: %s — unpartialled it "+
			"spans every episode ever opened instead of the live set", def)
	}
	down(52)
	if tbl, col, con, idx := episodeVocabulary(preRenameNames); tbl != wantTables ||
		col != wantColumns || con != wantConstraints || idx != wantIndexes {
		t.Fatalf("00052's Down restored %d/%d tables, %d/%d columns, %d/%d constraints and "+
			"%d/%d indexes of the pre-00052 vocabulary — a rename it forgot is SILENT, because "+
			"ALTER ... RENAME only errors on a name already taken, and it leaves the release "+
			"this rolls back to issuing DDL against a name the database does not have",
			tbl, wantTables, col, wantColumns, con, wantConstraints, idx, wantIndexes)
	}
	if tbl, col, con, idx := episodeVocabulary(caseNames); tbl+col+con+idx != 0 {
		t.Fatalf("%d table, %d column, %d constraint and %d index name(s) from the `case` "+
			"vocabulary survived 00052's Down — the schema is now half in each vocabulary, "+
			"which is the one state neither release can run against",
			tbl, col, con, idx)
	}
	// ⛔ AND THE ROWS, which a DDL-only reading would miss entirely. 00052's Up
	// rewrote four columns of live data into the `case` spelling and its Down
	// rewrites them back. Two of the four are guarded by CHECKs the Down re-adds,
	// so a missed rewrite there fails the migration outright; `alert_event_keys`
	// and `delivery_drills` have no such guard, and a stray `case:` dedupe key
	// silently stops de-duplicating against the `occ:` keys the rolled-back release
	// writes — the same event appended twice to a timeline.
	//
	// One row was seeded into each of the four above the rollback, which is what
	// makes this a subtraction rather than `0 != 0`. The four are counted BEFORE the
	// Down as well: a reading of four there is what proves each sub-count is looking
	// at the column it names, and without it a typo in any of them would leave the
	// reading afterwards passing for the wrong reason.
	if strayCaseRows := countStrayCaseRows(); strayCaseRows != 0 {
		t.Fatalf("%d row(s) still spell the episode `case` after 00052's Down, want 0 — the "+
			"rename is not only DDL, and the rolled-back release reads these values against "+
			"enums that no longer contain them", strayCaseRows)
	}

	// ⭐ 00051 down: the join table comes back, REPOPULATED, and the successor
	// index goes.
	//
	// ⛔ THIS IS THE ONE DOWN IN THIS FILE THAT RESTORES DATA, and asserting only
	// the DDL would miss the whole claim. `alert_group_members` was dropped because
	// every column it carried is a column of the episode, so its Down rebuilds it
	// with `INSERT ... SELECT ... FROM alert_occurrences`. If that argument were
	// wrong the rebuild would be empty or short, and a rolled-back release would
	// read an empty membership for every live generation — a card saying "0 alerts"
	// about an incident. The two counts are compared rather than a fixed number
	// asserted, so the property holds however many episodes the seed grows to.
	//
	// ⛔ AND THE COMPARISON IS GUARDED BY A NON-ZERO CHECK, because for a while it
	// was `0 == 0`. Two grouped episodes are seeded above the rollback — one ended,
	// one live — precisely so that this reads 2 == 2 and so that `left_at` (which the
	// rebuild takes from `ended_at`, and which the dropped table never had a
	// production writer for) is populated on one of them. Without the guard, a Down
	// whose INSERT selected nothing at all would be indistinguishable from a correct
	// one, which is the state this assertion shipped in.
	//
	// ⚠️ THE NAMES HERE ARE PRE-00052 because `down(52)` has already run:
	// `occ_group_live_idx` and `alert_occurrences`, which are also the names 00051
	// itself was written against.
	down(51)
	if def := indexDef("occ_group_live_idx"); def != "" {
		t.Fatalf("occ_group_live_idx survived 00051's Down: %s — a rolled-back deployment would "+
			"carry an index for a query the release it rolled back to does not run", def)
	}
	if !memberTableExists() {
		t.Fatal("alert_group_members did not come back on 00051's Down — the release this rolls " +
			"back to reads it for every group card, every fan-out and every reminder, and a " +
			"missing table is a 42P01 on the notification path")
	}
	if def := currentMemberIndexDef(); def == "" {
		t.Fatal("gm_current_idx did not come back on 00051's Down; the rolled-back release " +
			"sorts a storm to return twenty members without it")
	} else if !strings.Contains(def, "left_at IS NULL") {
		t.Fatalf("gm_current_idx came back without its partial predicate: %s — 00051's Down has "+
			"to restore the index, not merely the name", def)
	}
	var rebuiltMembers, groupedEpisodes, closedMemberships int
	if err := env.pool.QueryRow(env.ctx,
		`SELECT (SELECT count(*) FROM alert_group_members),
		        (SELECT count(*) FROM alert_occurrences WHERE group_id IS NOT NULL),
		        (SELECT count(*) FROM alert_group_members WHERE left_at IS NOT NULL)`).
		Scan(&rebuiltMembers, &groupedEpisodes, &closedMemberships); err != nil {
		t.Fatalf("introspect the rebuilt membership: %v", err)
	}
	if groupedEpisodes == 0 {
		t.Fatal("no episode in the database carries a group_id when 00051's Down runs, so the " +
			"comparison below is 0 == 0 and proves nothing — the seed above the rollback is what " +
			"is supposed to put grouped episodes here, and this assertion exists because this " +
			"whole step once passed on an empty table")
	}
	if rebuiltMembers != groupedEpisodes {
		t.Fatalf("00051's Down rebuilt %d membership rows for %d grouped episodes — the drop is "+
			"reversible only because every column the table carried is a column of the episode, "+
			"and a short rebuild means that argument is wrong", rebuiltMembers, groupedEpisodes)
	}
	// `left_at` comes back POPULATED, which the Up never had: the dropped table's
	// only writer never set it, so every `left_at IS NULL` predicate in the release
	// this rolls back to matched every row ever inserted. The Down takes it from
	// `ended_at`, and an ended episode is seeded above precisely so that a Down which
	// selected `NULL` there instead would be caught.
	if closedMemberships == 0 {
		t.Fatal("every rebuilt membership row has left_at IS NULL, though an ENDED episode is " +
			"seeded above the rollback — 00051's Down maps ended_at onto left_at, and a rebuild " +
			"that leaves it NULL hands the rolled-back release a generation whose membership can " +
			"only ever grow, which is the defect 00051 was written about")
	}

	// ⭐ 00050 down: the table comment goes back to describing Alertmanager's
	// grouping, and that is the whole of it.
	//
	// 00050 is five comments, which makes it the migration in this range whose Down
	// is likeliest to be a no-op nobody notices. There is deliberately NO backfill
	// and NO re-key in either direction — the file argues at length that re-keying is
	// neither computable from `alert_groups` alone nor safe against a live Slack
	// thread — so the prose IS the migration and it is all there is to assert.
	// Asserting nothing because there is no structure to introspect is how a
	// comment-only Down ships as a copy of its Up.
	//
	// ⛔⛔ AND `groups_axes_ck` MUST NOT EXIST, IN EITHER DIRECTION. An earlier draft
	// of 00050 added it as `CHECK (group_labels ->> 'alertname' IS NOT NULL) NOT
	// VALID`, reading `NOT VALID` as "the rows already there are exempt". It is not:
	// it skips the one-time validation SCAN, and Postgres then re-checks the
	// constraint against the NEW ROW VERSION on every UPDATE. A pre-00050 generation
	// carries whatever the operator's `group_by` was — `{}` for every
	// reconciler-sourced one — so `updateRollupSQL` and `closeGroupSQL` would both
	// have raised 23514 on it forever, `CloseIdle` would have swallowed that as a
	// warning, and every legacy generation's Slack thread would have stayed live
	// permanently. That is the exact opposite of the "visible, bounded and
	// self-healing" transition the migration is justified by. The invariant is the
	// writer's (`kernel.SplitLabels` is total over a `LabelSet` that refuses an empty
	// `alertname`) and 00050's header says so in place of enforcing it.
	if def := constraintDef("groups_axes_ck", "alert_groups"); def != "" {
		t.Fatalf("groups_axes_ck exists at the top of the stack: %s — 00050 deliberately adds "+
			"NO constraint on group_labels, because a CHECK re-runs on UPDATE and would make "+
			"every pre-00050 generation permanently un-closeable; whoever re-added it has "+
			"re-broken group.close for every row this migration promised would age out", def)
	}
	if c := tableComment("alert_groups"); !strings.Contains(c, "MACHINE-DERIVED") {
		t.Fatalf("alert_groups describes itself as %q above 00050's Down — the sentence an "+
			"operator reads at \\d+ is this migration's whole visible output", c)
	}
	// The legacy shape the constraint would have bricked, proved UPDATE-able at the
	// top of the stack rather than argued about: `{}` is what every pre-00050
	// reconciler-sourced generation carries, and `group.close` has to be able to
	// write to it. This is the assertion that fails the moment anybody adds the
	// CHECK back, whether or not they mark it NOT VALID.
	if _, err := env.pool.Exec(env.ctx,
		`UPDATE alert_groups SET group_labels = '{}'::jsonb, updated_at = now() WHERE id = $1`,
		legacyGroup); err != nil {
		t.Fatalf("a generation whose group_labels is the empty object could not be updated at "+
			"the top of the stack: %v — that is what a CHECK on group_labels does to every row "+
			"written before 00050, and the two UPDATEs it blocks (updateRollupSQL, "+
			"closeGroupSQL) are the sweep that was supposed to retire them", err)
	}
	down(50)
	if def := constraintDef("groups_axes_ck", "alert_groups"); def != "" {
		t.Fatalf("groups_axes_ck exists after 00050's Down: %s — the release this rolls back to "+
			"writes Alertmanager's groupLabels into group_labels, which has no alertname key at "+
			"all for a reconciler-sourced group, so such a constraint refuses its inserts", def)
	}
	if c := tableComment("alert_groups"); !strings.Contains(c, "Alertmanager notification group") {
		t.Fatalf("alert_groups still describes itself as %q after 00050's Down — the rolled-back "+
			"release derives nothing, and a comment promising a machine-derived key sends an "+
			"operator looking for axes the running code does not compute", c)
	}

	// ⭐ 00049 down: `alerts.ack_state` comes back, and it comes back REBUILT.
	//
	// ⛔ RESTORING THE COLUMN IS THE EASY HALF, and it is the half that cannot
	// fail: `ADD COLUMN ... NOT NULL DEFAULT 'unacked'` always succeeds. A Down
	// that stopped there would be green on every structural reading in this file
	// and would hand the rolled-back release a database in which nothing is
	// acknowledged — every acked incident back in somebody's queue, at the worst
	// possible moment. So the projection is read against its AUTHORITY instead:
	// `alert_occurrences.ack_state` was where the answer always lived, and after
	// the Down no alert may disagree with its own current episode.
	//
	// ⚠️ THAT PROPERTY IS VACUOUS ON AN EMPTY TABLE, and for a long time it was: the
	// suite's other alerts and episodes are written BELOW this step, so the join
	// matched nothing and `0 != 0` passed. An ACKED live episode is now seeded above
	// the rollback and is its alert's current one, so the projection is read here
	// against a row that has an answer — and the answer is asserted directly as well
	// as through the disagreement count, because a Down that defaulted the column
	// would satisfy `o.ack_state <> a.ack_state` for every UNACKED row in the world.
	if n := countColumns("alerts", "ack_state"); n != 0 {
		t.Fatal("alerts.ack_state exists above 00049's Down; 00049 drops it because an ack is a " +
			"receipt for one firing episode, and while the column is there a September firing " +
			"can arrive pre-acknowledged because somebody acked in March")
	}
	down(49)
	if n := countColumns("alerts", "ack_state"); n != 1 {
		t.Fatal("alerts.ack_state did not come back on 00049's Down — the release this rolls " +
			"back to filters, counts and serves it, so a missing column is a 42703 on the alert " +
			"list rather than a degraded one")
	}
	if def := constraintDef("alerts_ackstate_ck", "alerts"); def == "" {
		t.Fatal("alerts_ackstate_ck did not come back on 00049's Down; the column without its " +
			"CHECK accepts any string at all, and the release reading it compares against two")
	}
	var ackDisagreements int
	if err := env.pool.QueryRow(env.ctx,
		`SELECT count(*) FROM alerts a
		   JOIN alert_occurrences o ON o.id = a.current_occurrence_id
		  WHERE o.ack_state <> a.ack_state`).Scan(&ackDisagreements); err != nil {
		t.Fatalf("introspect the rebuilt ack projection: %v", err)
	}
	if ackDisagreements != 0 {
		t.Fatalf("%d alert(s) disagree with their own current episode about ack after 00049's "+
			"Down — the column is a PROJECTION of alert_occurrences and the Down rebuilds it "+
			"from there on purpose, so a mismatch means it defaulted instead of rebuilding",
			ackDisagreements)
	}
	// The same property stated positively. The count above now has a row to work on
	// and would catch a defaulted column, but it reports "1 alert disagrees" — this
	// names the value, so a failure reads as `unacked, want acked` rather than as an
	// arithmetic result somebody has to go and reconstruct.
	var reprojectedAck string
	if err := env.pool.QueryRow(env.ctx,
		`SELECT ack_state FROM alerts WHERE id = $1`, episodeAlert).Scan(&reprojectedAck); err != nil {
		t.Fatalf("read the reprojected ack_state: %v", err)
	}
	if reprojectedAck != "acked" {
		t.Fatalf("the alert whose current episode is acked reads ack_state=%q after 00049's "+
			"Down, want acked — the Down's UPDATE is the whole migration in this direction, and "+
			"a rolled-back release reading `unacked` puts every acknowledged incident back in "+
			"somebody's queue at the worst possible moment", reprojectedAck)
	}

	// ⭐ 00048 down: `alerts.snoozed_until` comes back, REPROJECTED, and its index
	// comes back with it.
	//
	// The same shape as 00049 and for the same reason. `ADD COLUMN` cannot fail, so
	// a Down that stopped at the DDL hands the rolled-back release a database in
	// which nothing is snoozed — every muted alert back in the feed at once. The
	// projection is read against `alert_snoozes`, which was the authority all along
	// and is the only reason dropping the column was reversible.
	//
	// The index is read as its DEFINITION: `alerts_snooze_idx` without
	// `WHERE snoozed_until IS NOT NULL` is a full-width index over a column that is
	// NULL for almost every row, which is a different index under the same name.
	//
	// ⚠️ It was vacuous on an empty `alert_snoozes` for the same reason 00049's was.
	// An ACTIVE snooze is now seeded above the rollback, so the anti-join has a row,
	// and the restored value is read directly as well — for the reason 00049's is: a
	// Down that added the column and stopped disagrees with nothing while every
	// alert's snoozed_until is NULL on both sides.
	if n := countColumns("alerts", "snoozed_until"); n != 0 {
		t.Fatal("alerts.snoozed_until exists above 00048's Down; 00048 drops it so that the " +
			"snooze answer has exactly one home, and while the projection is there two rows " +
			"answer `is this muted` and only one of them is written by the snooze path")
	}
	if def := indexDef("alerts_snooze_idx"); def != "" {
		t.Fatalf("alerts_snooze_idx exists above 00048's Down: %s — it indexes a column 00048 "+
			"dropped, so its presence means the Up left it behind", def)
	}
	down(48)
	if n := countColumns("alerts", "snoozed_until"); n != 1 {
		t.Fatal("alerts.snoozed_until did not come back on 00048's Down — the release this " +
			"rolls back to reads the projection rather than alert_snoozes, and a missing column " +
			"is a 42703 on the alert list")
	}
	if def := indexDef("alerts_snooze_idx"); def == "" {
		t.Fatal("alerts_snooze_idx did not come back on 00048's Down; the rolled-back release " +
			"filters on snoozed_until and would scan the table to do it")
	} else if !strings.Contains(def, "snoozed_until IS NOT NULL") {
		t.Fatalf("alerts_snooze_idx came back without its partial predicate: %s — an index over "+
			"the NULLs too is most of the table, and 00048's Down has to restore the index "+
			"rather than merely the name", def)
	}
	var snoozeDisagreements int
	if err := env.pool.QueryRow(env.ctx,
		`SELECT count(*) FROM alerts a
		   JOIN alert_snoozes s
		     ON s.alert_id = a.id AND s.org_id = a.org_id AND s.ended_at IS NULL
		  WHERE a.snoozed_until IS DISTINCT FROM s.snoozed_until`).Scan(&snoozeDisagreements); err != nil {
		t.Fatalf("introspect the reprojected snoozes: %v", err)
	}
	if snoozeDisagreements != 0 {
		t.Fatalf("%d alert(s) disagree with their own ACTIVE snooze after 00048's Down — the "+
			"column is a projection of alert_snoozes and the Down rebuilds it from there, so a "+
			"mismatch means it came back NULL and the rolled-back release un-muted them",
			snoozeDisagreements)
	}
	var reprojectedSnooze *time.Time
	if err := env.pool.QueryRow(env.ctx,
		`SELECT snoozed_until FROM alerts WHERE id = $1`, episodeAlert).
		Scan(&reprojectedSnooze); err != nil {
		t.Fatalf("read the reprojected snoozed_until: %v", err)
	}
	// As with 00049's: the anti-join above would already have caught this now that a
	// snooze row exists, and this is here so the failure names the state rather than
	// a count.
	if reprojectedSnooze == nil {
		t.Fatal("the alert carrying an active snooze came back with snoozed_until NULL after " +
			"00048's Down — the column is the only thing the rolled-back release reads to " +
			"decide an alert is muted, so a NULL here puts every quiet alert back in the feed " +
			"at once")
	}

	// ⭐ 00047 down: the natural key goes, and `ordinal` goes with it because
	// nothing else reads it.
	//
	// DROPPING A UNIQUE CONSTRAINT CANNOT FAIL, which is why introspection alone is
	// not the assertion here — a Down that dropped the column but left the
	// constraint, or dropped neither and returned nil, would satisfy any
	// shape-shaped check that only looked at one of the two. So the pair that the
	// constraint refuses is WRITTEN twice after the rollback: the release this
	// rolls back to has no idempotence, and a rolled-back binary re-running a
	// batch has to be able to append its rejections a second time rather than
	// 23505 at an operator mid-incident.
	rejectionAt := time.Now().UTC()
	rejectionBatch := id.New()
	zeroth := 0
	if err := insertRejection(rejectionBatch, &zeroth, rejectionAt); err != nil {
		t.Fatalf("seeding a rejection before 00047's Down: %v", err)
	}
	if err := insertRejection(rejectionBatch, &zeroth, rejectionAt); err == nil {
		t.Fatalf("a second rejection at the same (batch, ordinal, reason) was accepted while " +
			"00047 is applied — the natural key is the whole mechanism behind `oto replay` not " +
			"duplicating the feed, and a constraint that admits the duplicate is not enforcing it")
	}
	down(47)
	if def := rejectionNaturalUniq(); def != "" {
		t.Fatalf("ingest_rejections_natural_uniq survived 00047's Down: %s — the release this "+
			"rolls back to has no `ordinal` column, so a surviving constraint would reference "+
			"a column the next dump cannot restore", def)
	}
	var ordinalSurvives int
	if err := env.pool.QueryRow(env.ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name::text = 'ingest_rejections' AND column_name::text = 'ordinal'`).
		Scan(&ordinalSurvives); err != nil {
		t.Fatalf("introspect ingest_rejections.ordinal: %v", err)
	}
	if ordinalSurvives != 0 {
		t.Fatalf("ingest_rejections.ordinal survived 00047's Down — the Down drops the " +
			"constraint and the column together, and a half-run Down leaves a column no " +
			"writer in the rolled-back release populates")
	}
	// The duplicate the constraint used to refuse, now accepted twice: the proof
	// that the rollback restored the earlier release's behaviour and not merely
	// its schema.
	// Written without naming `ordinal`, because after the Down there is no such
	// column — which is the pre-00047 writer's statement exactly, and the reason
	// this is spelled out rather than reusing the helper above it.
	insertRolledBackRejection := func() error {
		_, err := env.pool.Exec(env.ctx,
			`INSERT INTO ingest_rejections (id, org_id, source_id, batch_id, received_at,
			     reason, raw)
			 VALUES ($1, $2, $3, $4, $5, 'missing_alertname', '{}'::jsonb)`,
			id.New(), policyScope.OrgID(), id.New(), rejectionBatch, rejectionAt)
		return err
	}
	for i := range 2 {
		if err := insertRolledBackRejection(); err != nil {
			t.Fatalf("rejection %d was refused after 00047's Down: %v — the release this rolls "+
				"back to double-counts a replayed batch by design, and a rolled-back binary "+
				"must not 23505 on the write that produces the duplicate", i, err)
		}
	}

	// ⭐ 00046 down: `reasons` goes back to being a bag of 1..32, and the helper
	// function goes with it.
	//
	// A relaxation CANNOT FAIL, which is precisely why reading the constraint text
	// is not enough here: a Down that dropped `policies_reasons_ck` and forgot to
	// re-add the loose one would leave the column with no length bound and no NULL
	// scan at all, and would pass a "does not contain oto_array_is_set" check
	// comfortably. So the row the tight constraint refuses is WRITTEN instead, and
	// it is deliberately left in the table: the way back up has to fold it.
	down(46)
	if def := policyReasonsCheck(); strings.Contains(def, "oto_array_is_set") {
		t.Fatalf("policies_reasons_ck still carries the set rule after 00046's Down: %s — the "+
			"release this rolls back to has no such function, so the next dump would restore "+
			"a constraint calling something that does not exist", def)
	} else if !strings.Contains(def, "array_length") {
		t.Fatalf("policies_reasons_ck is not the constraint 00011 shipped after 00046's Down: "+
			"%s — a Down that drops a CHECK without restoring the previous predicate leaves "+
			"the column with NO bound, which is looser than either release ever intended", def)
	}
	if n := arrayIsSetFunctions(); n != 0 {
		t.Fatalf("%d oto_array_is_set functions survived 00046's Down, want 0 — a rolled-back "+
			"deployment would carry a function nothing calls", n)
	}
	// The row that proves the relaxation is real, and the row 00046's backfill has
	// to meet on the way back up. `acked` sits between the two `fired`s so that the
	// fold has to keep FIRST-case order rather than sorting or keeping the
	// last: an operator who wrote this list should recognise it afterwards.
	foldedPolicy := id.New()
	if err := insertPolicy(foldedPolicy, "bag-after-the-down",
		[]string{"fired", "acked", "fired"}); err != nil {
		t.Fatalf("a duplicate reason was still refused after 00046's Down: %v — the Down is "+
			"supposed to restore the schema of a release whose only gate is the `unique` DTO "+
			"tag, and a rolled-back binary writing through any other path must not 23514", err)
	}

	// ⭐ 00045 down: both projection tables go, and they have to go WITH ROWS IN
	// THEM. A real alert is written first, and its label rows with it, because
	// that is the only state an operator ever rolls this back from: 00045's Up
	// backfills from `alerts`, so on any database with alerts these tables are
	// populated the moment the migration lands. Dropping them loses nothing that
	// cannot be recomputed — both are a pure function of `alerts.labels`, which
	// the Down does not touch — and a re-Up backfills them again, which is why
	// this is the rare Down in this list that is genuinely free.
	labelOrg, _, labelHealth := seedSource(t, env)
	var labelCluster uuid.UUID
	if err := env.pool.QueryRow(env.ctx,
		`SELECT cluster_id FROM alert_sources WHERE id = $1`, labelHealth.SourceID).
		Scan(&labelCluster); err != nil {
		t.Fatalf("read the seeded source's cluster: %v", err)
	}
	labelAlert := id.New()
	if _, err := env.pool.Exec(env.ctx,
		`INSERT INTO alerts (id, org_id, cluster_id, alert_key, source_fingerprint, alertname,
		                     cluster_key, labels, state, first_seen_at, last_seen_at,
		                     last_state_change_at)
		 VALUES ($1, $2, $3, $4, $5, 'RollbackProbe', 'prod',
		         '{"alertname":"RollbackProbe","severity":"critical"}'::jsonb,
		         'firing', now(), now(), now())`,
		labelAlert, labelOrg.OrgID(), labelCluster,
		"ak_"+strings.Repeat("a", 26), strings.Repeat("ab", 8)); err != nil {
		t.Fatalf("seed an alert to project: %v", err)
	}
	// Written the way `upsertAlertsSQL` writes them and the way 00045's backfill
	// defines them, so the rows the Down meets are the rows the ingest path makes.
	if _, err := env.pool.Exec(env.ctx,
		`WITH put AS (
		   INSERT INTO alert_labels (org_id, alert_id, label_name, label_value)
		   SELECT a.org_id, a.id, e.key, coalesce(e.value, '')
		     FROM alerts a, LATERAL jsonb_each_text(a.labels) AS e(key, value)
		    WHERE a.id = $1 AND NOT a.synthetic
		   RETURNING org_id, label_name)
		 INSERT INTO alert_label_names (org_id, label_name, alert_count)
		 SELECT org_id, label_name, count(*) FROM put GROUP BY org_id, label_name
		     ON CONFLICT ON CONSTRAINT alert_label_names_pk DO UPDATE
		    SET alert_count = alert_label_names.alert_count + EXCLUDED.alert_count`,
		labelAlert); err != nil {
		t.Fatalf("project the seeded alert's labels: %v — 00045 exists to make these rows "+
			"writable, so a failure here means its Up never created the tables it describes", err)
	}
	down(45)
	if n := labelProjectionTables(); n != 0 {
		t.Fatalf("%d of 00045's two tables survived its Down, want 0 — a rolled-back release "+
			"neither reads nor maintains them, so what would be left is a projection of "+
			"`alerts.labels` that silently stops tracking it and is wrong from the next "+
			"observation onwards", n)
	}

	// 00044 down: the current-member index goes. Like 00042's pair this is a
	// `DROP INDEX IF EXISTS`, the kind of statement that runs cleanly whether or
	// not it does anything — so the property is read afterwards rather than the
	// error trusted. The release this rolls back to still returns the right twenty
	// members; it just sorts the storm to find them, which is the state the index
	// exists to leave behind.
	down(44)
	if def := currentMemberIndexDef(); def != "" {
		t.Fatalf("gm_current_idx survived 00044's Down: %s — a rolled-back deployment would "+
			"carry an index for a query the release it rolled back to does not run", def)
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
	// `occ_started_idx`, not `case_started_idx`: 00052's Down ran ten steps ago and
	// took the `case` vocabulary with it.
	if n := rollupRangeIndexes("occ_started_idx"); n != 0 {
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
	// nothing, or that let `case_rule_fk` blank every case's bound rule,
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

	// One case bound to the row that is about to be folded away, and one
	// bound to a row that survives. `case_open_uniq` allows at most one open
	// case per alert, so they hang off two alerts.
	boundToFolded, boundToUntouched := id.New(), id.New()
	cases := []struct {
		caseID, snapID uuid.UUID
		alertname      string
	}{
		{boundToFolded, folded, "AlertB"},
		{boundToUntouched, untouched, "AlertA"},
	}
	for i, o := range cases {
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
		// ⛔ `alert_occurrences`, not `alert_cases`: 00052's Down ran eleven steps ago
		// and renamed the episode table back. Its own columns were never renamed —
		// 00052 only carried `alerts`, `alert_events`, `notifications`,
		// `delivery_drills` and `alert_quality_daily` with it — so the column list
		// below is the same on both sides of that rename.
		if _, err := env.pool.Exec(env.ctx,
			`INSERT INTO alert_occurrences (id, org_id, alert_id, seq, state, started_at,
			                                last_observed_at, source_starts_at, rule_snapshot_id)
			 VALUES ($1, $2, $3, 1, 'firing', $4, $4, $4, $5)`,
			o.caseID, foldOrg.OrgID(), alertID, base, o.snapID); err != nil {
			t.Fatalf("bind a case of %s to its rule snapshot: %v", o.alertname, err)
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
	// cases bound to the dropped rows onto the survivor first — every
	// folded row is byte-identical in definition to the row it folds into, so
	// letting `case_rule_fk` null the pointer instead would discard "what the rule
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

	// ⭐ AND THE CASES STILL KNOW WHAT THE RULE SAID. This is the assertion
	// the `ON DELETE SET NULL` would fail: the folded row and its survivor carry
	// the same rule_fingerprint and therefore byte-identical text, so unbinding
	// loses the one fact the product exists to show for no reason at all.
	boundSnapshot := func(caseID uuid.UUID) *uuid.UUID {
		t.Helper()
		var out *uuid.UUID
		if err := env.pool.QueryRow(env.ctx,
			// `alert_occurrences`: below 00052's Down, same as the seed above.
			`SELECT rule_snapshot_id FROM alert_occurrences WHERE id = $1`, caseID).Scan(&out); err != nil {
			t.Fatalf("read the case's bound snapshot: %v", err)
		}
		return out
	}
	switch got := boundSnapshot(boundToFolded); {
	case got == nil:
		t.Fatal("the case bound to the folded snapshot came out of 00040's Down unbound — " +
			"case_rule_fk is ON DELETE SET NULL, so a Down that deletes before it remaps throws " +
			"away the rule text behind a fired alert while an identical copy of that text " +
			"survives in the row it was folded into")
	case *got != survivor:
		t.Fatalf("the case bound to the folded snapshot now points at %s, want the survivor "+
			"%s", *got, survivor)
	}
	if got := boundSnapshot(boundToUntouched); got == nil || *got != untouched {
		t.Fatalf("a case bound to a snapshot that was never folded came out of the Down "+
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

	// 00029 down: the partial index behind the episode delivery roll-up goes.
	//
	// ⛔ `notif_occurrence_idx`, not `notif_case_idx`: 00052's Down renamed it back
	// twenty-three steps ago, and 00029 created it under the older spelling in the
	// first place. The post-00052 name here would read 0 before this Down had run
	// and the assertion would pass vacuously.
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
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'notif_case_idx'`).Scan(&indexes); err != nil {
		t.Fatalf("introspect indexes: %v", err)
	}
	if indexes != 1 {
		t.Fatal("notif_case_idx did not come back on the way up")
	}
	if buckets() != 0 {
		t.Fatal("rate_limit_buckets survived the way back up")
	}
	// ⭐ 00046 comes back AND FOLDS THE ROW IT MEETS. This is the only place in the
	// suite where its backfill runs against a policy that actually violates the
	// constraint about to be added, and it is the half that decides whether the
	// migration is deployable at all: tightening a CHECK on a live table FAILS if
	// any stored row is already illegal, so an Up that added the constraint without
	// folding first would abort here — on a database whose only offending row was
	// written by the release it is upgrading from.
	if def := policyReasonsCheck(); !strings.Contains(def, "oto_array_is_set") {
		t.Fatalf("policies_reasons_ck did not re-tighten on the way up: %s — the rest of the "+
			"suite runs against this schema, and `reasons` is a set in the contract", def)
	}
	if n := arrayIsSetFunctions(); n != 1 {
		t.Fatalf("%d oto_array_is_set functions came back on the way up, want 1", n)
	}
	var foldedReasons []string
	if err := env.pool.QueryRow(env.ctx,
		`SELECT reasons FROM notification_policies WHERE id = $1`, foldedPolicy).Scan(&foldedReasons); err != nil {
		t.Fatalf("the policy written under the relaxed constraint did not survive the round "+
			"trip: %v — the fold rewrites rows and must not delete the ones it cannot keep "+
			"verbatim; a policy that vanished is a tenant silently stopping being notified", err)
	}
	if strings.Join(foldedReasons, ",") != "fired,acked" {
		t.Fatalf("the duplicated policy reads %v after the way back up, want [fired acked] — "+
			"the fold keeps each reason ONCE and in FIRST-case order, because the column "+
			"is read back verbatim into PolicyDTO.reasons and neither order is more correct "+
			"than the other; sorting it would rearrange an operator's list under them",
			foldedReasons)
	}

	// ⭐ 00045 comes back AND REFILLS ITSELF. Its Up is a CREATE plus a backfill,
	// so the way up is the only place the backfill runs against a database that
	// already has alerts in it — the alert seeded above for the Down is still
	// there, and its labels have to be back in the projection. A round trip that
	// restored the tables empty would be green on a presence check and would leave
	// the typeahead silently offering nothing for every alert that predates the
	// rollback.
	if n := labelProjectionTables(); n != 2 {
		t.Fatalf("%d of 00045's two tables came back on the way up, want 2 — the rest of the "+
			"suite runs against this schema, and both label typeaheads read them", n)
	}
	var reprojected int
	if err := env.pool.QueryRow(env.ctx,
		`SELECT count(*) FROM alert_labels WHERE alert_id = $1`, labelAlert).Scan(&reprojected); err != nil {
		t.Fatalf("introspect the re-backfilled projection: %v", err)
	}
	if reprojected != 2 {
		t.Fatalf("the alert seeded before 00045's Down has %d projected labels after the way "+
			"back up, want 2 — the Up's backfill IS the definition of these tables, and a "+
			"re-created pair that only tracks alerts observed after the rollback is a typeahead "+
			"that has silently forgotten the estate", reprojected)
	}
	// ⭐ THE WAY BACK UP RUNS THE WHOLE STACK, not a stop at 00044. `migrate.Up`
	// above goes to the top, so 00044's Up re-creates `gm_current_idx` and then
	// 00051 drops it again and 00052 renames its successor. Asserting the index
	// is BACK would assert the state of a database eight migrations stale. What
	// must be true at the top is the successor: `case_group_live_idx`, partial on
	// the predicate something actually writes.
	if def := currentMemberIndexDef(); def != "" {
		t.Fatalf("gm_current_idx is present at the top of the stack: %s — 00051 drops it and "+
			"00052 renames its successor; an index for `alert_group_members` outliving the "+
			"table is a rollback that only half happened", def)
	}
	// Its successor is asserted a few lines below, where the suite already checks
	// the top-of-stack shape from the other direction.
	// ⭐ AND THE TOP OF THE STACK, AGAIN, FROM THE OTHER DIRECTION. The rest of the
	// suite runs against this schema, and the member plan test asserts a plan that
	// only exists while case_group_live_idx does.
	if memberTableExists() {
		t.Fatal("alert_group_members is back after the way up — 00051's Up drops it, and a " +
			"round trip that leaves it behind leaves two answers to `what is in this " +
			"generation`, one of them stale from the moment the rollback ended")
	}
	if def := indexDef("case_group_live_idx"); def == "" {
		t.Fatal("case_group_live_idx did not come back on the way up — the rest of the suite " +
			"runs against this schema, and the member plan test asserts a plan that only " +
			"exists while it does")
	} else if !strings.Contains(def, "ended_at IS NULL") {
		t.Fatalf("case_group_live_idx came back without its partial predicate: %s — the round "+
			"trip has to restore the index, not merely the name", def)
	}
	if n := rollupRangeIndexes("case_started_idx"); n != 2 {
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
	if _, present := view.Data.Shadowed["flap_threshold"]; present {
		t.Fatal("an unmanaged key was reported as shadowed")
	}
	if _, present := view.Data.ConfigKeys["flap_threshold"]; present {
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
	status, raw = session.patch(t, map[string]any{"flap_threshold": 40})
	if status != http.StatusOK {
		t.Fatalf("PATCH on an unmanaged key → %d: %s", status, raw)
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := view.Data.Settings["flap_threshold"]; got != float64(40) {
		t.Fatalf("flap_threshold = %v after a legal write", got)
	}
	if got := view.Data.Settings["refire_grace_s"]; got != float64(600) {
		t.Fatalf("the write dropped the declarative overlay: refire_grace_s = %v", got)
	}
	if got := view.Data.Origins["flap_threshold"]; got != "org" {
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
