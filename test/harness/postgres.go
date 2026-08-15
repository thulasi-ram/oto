package harness

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/migrate"
)

// Image is the Postgres the whole suite runs against. It is pinned: "latest"
// would make a schema failure depend on the day the test ran.
const Image = "postgres:17-alpine"

// templateDB is the fully migrated database every test's own database is cloned
// from. Nothing ever connects to it after bootstrap — CREATE DATABASE refuses a
// template that has a live session.
const templateDB = "oto"

// adminDB is the database the harness connects to in order to CREATE and DROP
// the per-test ones. It cannot be the template and it cannot be a test's own.
const adminDB = "postgres"

// Epoch is the instant every harness FakeClock is pinned at. It is a fixed,
// far-from-any-boundary UTC time so that a test which advances the clock can
// state its expectations as arithmetic rather than as "roughly now".
//
// It stays fixed, and the PARTITIONS COME TO IT: `migrateTemplate` builds the
// four partitioned tables a window around Epoch as well as around the database's
// own `now()`, so a row stamped at `h.Now()` always has somewhere to land. See
// epochPartitionsSQL below — and git-bug 6547228 for what happened when it did
// not.
var Epoch = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// pg is the single Postgres shared by every test in one test binary.
//
// ONE CONTAINER PER TEST BINARY, not per test: starting Postgres and applying
// thirty goose migrations plus River's own costs seconds, and `test/integration`
// used to pay it fourteen times over because `newEnv` started a container of its
// own for each test function.
type pg struct {
	container *tcpostgres.PostgresContainer
	baseDSN   string
	admin     *pgxpool.Pool
	err       error
}

var (
	bootOnce sync.Once
	shared   *pg
	dbSeq    atomic.Int64
)

// Main is the TestMain body for any package that uses the harness:
//
//	func TestMain(m *testing.M) { harness.Main(m) }
//
// It does NOT start the container. Startup is lazy, on the first New, so that a
// package whose tests all skip (`go test -short`, or a -run selector that
// matches no database-backed test) never pays for Docker at all.
func Main(m *testing.M) { os.Exit(Run(m)) }

// Run is Main without the os.Exit, for a TestMain that has teardown of its own.
func Run(m *testing.M) int {
	code := m.Run()
	stop()
	return code
}

// New gives one test its OWN database on the shared Postgres.
//
// # Why a database per test, and not a schema
//
// ⛔ SCHEMA-PER-TEST IS NOT AVAILABLE HERE, and the reason is in the schema
// itself: `db/migrations/00005_partitions.sql` hard-codes `public.` inside
// `oto_partitions_manage()` — `to_regclass(format('public.%I', v_name))`,
// `CREATE TABLE ... public.%I PARTITION OF public.%I`. Under a per-test
// `search_path` the partition manager would keep creating and attaching
// partitions in `public` while the tables lived somewhere else, so every
// timeline append into `alert_events` would fail. Fixing that means editing a
// migration, and migrations are production code.
//
// A DATABASE has its own `public` schema, so `public.` resolves correctly and
// the partition manager is untouched. It is the same isolation the ADR asks for,
// one level up.
//
// # Why not truncate-between-tests
//
// Because `internal/notification/service` runs its database tests with
// t.Parallel, and a shared database cannot have one test truncate it while
// another is mid-transaction — the first attempt at this deadlocked on
// `channel_threads` exactly that way. Cloning is also cheap: `CREATE DATABASE
// ... TEMPLATE` is a file copy of an EMPTY schema, and it costs a fraction of
// re-running thirty migrations.
//
// The database is dropped when the test ends, so a run leaves nothing behind.
func New(t *testing.T) *H {
	t.Helper()
	if testing.Short() {
		t.Skip("harness: -short skips the tests that need Docker")
	}

	p := start()
	if p.err != nil {
		t.Fatalf("harness: postgres: %v", p.err)
	}

	name := fmt.Sprintf("oto_t%d", dbSeq.Add(1))
	ctx := context.Background()

	// CREATE DATABASE cannot run inside a transaction, and it is serialised so
	// that a parallel package never races two clones of the same template.
	if _, err := p.admin.Exec(ctx,
		fmt.Sprintf(`CREATE DATABASE %q TEMPLATE %q`, name, templateDB)); err != nil {
		t.Fatalf("harness: clone database: %v", err)
	}

	dsn := withDatabase(p.baseDSN, name)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("harness: pool for %s: %v", name, err)
	}

	// Registered FIRST so it runs LAST: a test that builds an app.Container or
	// pools of its own registers those cleanups afterwards, and t.Cleanup is
	// LIFO, so everything is shut down before the database goes away.
	t.Cleanup(func() {
		pool.Close()
		drop(t, p, name)
	})

	return &H{
		T:     t,
		Ctx:   ctx,
		Pool:  pool,
		DSN:   dsn,
		Clock: clock.NewFake(Epoch),
	}
}

// H is one test's handle on the harness.
type H struct {
	// T is the test this handle belongs to.
	T *testing.T
	// Ctx is a background context. It deliberately is not t.Context(), which Go
	// cancels before cleanups run.
	Ctx context.Context
	// Pool is a pgx pool against THIS TEST's own database.
	Pool *pgxpool.Pool
	// DSN is the connection string for this test's database, for code that opens
	// pools of its own (config.DB.URL, a second pool for a race test).
	DSN string
	// Clock is this test's FakeClock, pinned at Epoch. It is the ONLY clock a
	// test should inject: `platform/clock.System` in a test is a sleep waiting
	// to happen.
	Clock *clock.Fake
}

// Advance moves this test's FakeClock forward.
func (h *H) Advance(d time.Duration) { h.Clock.Advance(d) }

// Now is the FakeClock's current instant.
func (h *H) Now() time.Time { return h.Clock.Now() }

// Exec runs one statement and fails the test if it errors. It is for seeding
// only; nothing under test should reach the database this way.
func (h *H) Exec(sql string, args ...any) {
	h.T.Helper()
	if _, err := h.Pool.Exec(h.Ctx, sql, args...); err != nil {
		h.T.Fatalf("harness: exec: %v\nSQL: %s", err, strings.TrimSpace(sql))
	}
}

// ------------------------------------------------------------------ lifecycle

func start() *pg {
	bootOnce.Do(func() { shared = bootstrap() })
	return shared
}

func bootstrap() *pg {
	ctx := context.Background()
	p := &pg{}

	container, err := tcpostgres.Run(ctx, Image,
		tcpostgres.WithDatabase(templateDB),
		tcpostgres.WithUsername("oto"),
		tcpostgres.WithPassword("oto"),
		// BasicWaitStrategies waits for the port AND for the SECOND
		// "ready to accept connections", which is the one that matters: the
		// first is initdb's bootstrap server, and connecting to it races the
		// restart that follows.
		tcpostgres.BasicWaitStrategies(),
	)
	p.container = container
	if err != nil {
		p.err = fmt.Errorf("start %s (is Docker running?): %w", Image, err)
		return p
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		p.err = fmt.Errorf("connection string: %w", err)
		return p
	}
	p.baseDSN = dsn

	if err := migrateTemplate(ctx, dsn); err != nil {
		p.err = err
		return p
	}

	admin, err := pgxpool.New(ctx, withDatabase(dsn, adminDB))
	if err != nil {
		p.err = fmt.Errorf("admin pool: %w", err)
		return p
	}
	p.admin = admin

	// CREATE DATABASE refuses a template that has any other session on it, and a
	// pool that has just been closed can still leave a backend winding down.
	if err := evictTemplateSessions(ctx, admin); err != nil {
		p.err = err
		return p
	}
	return p
}

// migrateTemplate brings the template database fully up and then DISCONNECTS
// from it for good.
func migrateTemplate(ctx context.Context, dsn string) error {
	if err := migrate.Up(ctx, dsn); err != nil {
		return fmt.Errorf("goose: %w", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("template pool: %w", err)
	}
	defer pool.Close()

	// River's tables are part of the schema, not an optional extra: ADR 0001
	// makes the job queue Postgres, so `platform/jobs` is a REAL collaborator in
	// every test and its migrations belong in the one bootstrap.
	m, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("river: %w", err)
	}
	if _, err := m.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("river: %w", err)
	}

	// alert_events, ingest_batches, ingest_rejections and ui_events are
	// partitioned with NO default partition, deliberately: a row outside every
	// range must fail loudly rather than pile up in a bucket nobody can drop.
	// Without this call every timeline append fails and every test using the
	// harness fails for the wrong reason.
	if _, err := pool.Exec(ctx, `SELECT oto_partitions_manage()`); err != nil {
		return fmt.Errorf("partitions: %w", err)
	}

	// ⚠️ AND THE SAME WINDOW AGAIN, AROUND Epoch — git-bug 6547228. The call
	// above builds its partitions around the DATABASE's `now()`, while every
	// harness FakeClock is pinned at Epoch, so the two disagree by as many months
	// as have passed since Epoch was written down and the gap widens on the first
	// of every one. A timeline append stamped at `h.Now()` then matches no range
	// and fails with SQLSTATE 23514, "no partition of relation alert_events found
	// for row" — a check_violation naming no constraint, because there is no
	// constraint to name, which `platform/db` maps to a `sqlstate_23514` 500 that
	// reads like a generic database defect rather than like a calendar
	// disagreement. Five tests had already worked around it one call site at a
	// time by deriving their own `now` from the wall clock. This is that fix,
	// once, where the clock and the partitions are both decided.
	//
	// WHICH PARENTS THIS ACTUALLY RESCUES. `alert_events` (`recorded_at`),
	// `ingest_batches` and `ingest_rejections` (`received_at`) take their
	// partition key from a Go caller, so a harness clock reaches them and the trap
	// is real. `ui_events.at` is `DEFAULT now()` and every writer omits the column
	// (`internal/streaming/repository/events.go`), so no row is ever stamped at
	// Epoch and its Epoch partitions are precaution, not repair — they cost an
	// empty table each and they are there for the day a writer supplies `at`.
	//
	// ⚠️ ONE LIMIT, HONESTLY. This runs ONCE, at template bootstrap. A test that
	// DISPATCHES the `partitions.manage` worker (`app/workers.go`) against its own
	// database re-runs retention with the real `now()`, which drops the Epoch
	// `ui_events` and `ingest_*` partitions immediately — they are months past a
	// 24-hour and a 30-day cutoff. `alert_events` survives until Epoch is 13
	// months old. Nothing dispatches that job in a test today; a test that starts
	// to will have to re-run this statement afterwards.
	if _, err := pool.Exec(ctx, epochPartitionsSQL, Epoch); err != nil {
		return fmt.Errorf("epoch partitions: %w", err)
	}
	return nil
}

// epochPartitionsSQL gives Epoch the window `oto_partitions_manage` gives
// `now()` — the grains and the counts are 00005's, table for table — plus one
// period of headroom BEHIND it, so a test that stamps something just before
// Epoch (an occurrence "three hours ago", a batch "yesterday") has a partition
// too.
//
// Widening it is cheap; a partition is an empty table in a template nothing else
// reads. Narrowing it is how git-bug 6547228 comes back.
const epochPartitionsSQL = `
	SELECT oto_ensure_partitions_ahead('ui_events',         'hour',  7, $1::timestamptz - interval '1 hour'),
	       oto_ensure_partitions_ahead('ingest_batches',    'day',   8, $1::timestamptz - interval '1 day'),
	       oto_ensure_partitions_ahead('ingest_rejections', 'day',   8, $1::timestamptz - interval '1 day'),
	       oto_ensure_partitions_ahead('alert_events',      'month', 4, $1::timestamptz - interval '1 month')`

func evictTemplateSessions(ctx context.Context, admin *pgxpool.Pool) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := admin.Exec(ctx,
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			  WHERE datname = $1 AND pid <> pg_backend_pid()`, templateDB); err != nil {
			return fmt.Errorf("evict template sessions: %w", err)
		}

		var n int
		if err := admin.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE datname = $1`, templateDB).Scan(&n); err != nil {
			return fmt.Errorf("count template sessions: %w", err)
		}
		if n == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("template database still has %d sessions", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func drop(t *testing.T, p *pg, name string) {
	t.Helper()
	// Not t.Context(): it is already cancelled by the time cleanups run. FORCE
	// terminates any connection a test forgot to close, so one leaked pool
	// cannot leave a database — and a container — behind.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := p.admin.Exec(ctx,
		fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name)); err != nil {
		t.Errorf("harness: drop database %s: %v", name, err)
	}
}

func stop() {
	if shared == nil {
		return
	}
	if shared.admin != nil {
		shared.admin.Close()
	}
	if shared.container != nil {
		if err := testcontainers.TerminateContainer(shared.container); err != nil {
			fmt.Fprintf(os.Stderr, "harness: terminate container: %v\n", err)
		}
	}
}

// withDatabase rewrites the database name in a DSN, leaving credentials, host
// and query parameters alone.
func withDatabase(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		// The DSN came from testcontainers; an unparseable one is a bug here,
		// not a test failure, and there is no *testing.T in scope to report it.
		panic("harness: unparseable dsn: " + err.Error())
	}
	u.Path = "/" + name
	return u.String()
}
