package repository_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"

	"github.com/thulasiram/oto/internal/alerts/repository"
)

// ⭐⭐ THE ALERT LIST HAS TWO TABS AND ONE SOURCE OF TRUTH.
//
// `?snoozed=false` is the main tab, `?snoozed=true` is **Quiet**, and both are
// answered from `alert_snoozes` — the row that knows who asked for quiet, what
// they wrote and how the quiet period ended. They used to be answered from
// `alerts.snoozed_until`, a bare timestamp kept in step by three separate write
// paths, and the interesting rows are exactly the ones where a timestamp and a
// row disagree:
//
//	an ENDED snooze     — the row says the quiet period is over; the mirror said
//	                      so only once somebody remembered to clear it.
//	an UNSWEPT snooze   — `ended_at` is still NULL and the clock has run out. The
//	                      60-second expiry job has not been round yet, and oto must
//	                      already be speaking again.
//
// ⛔ THESE ARE INTEGRATION TESTS AGAINST A REAL POSTGRES ON PURPOSE. What is
// under test is which rows a predicate selects. A fake would select whichever
// rows its author believed in, which is the belief being checked.
//
// TestMain lives in prune_test.go; this file adds no second one.

// tabWorld is one tenant holding four alerts, one for each way an alert can
// relate to a snooze.
type tabWorld struct {
	h    *harness.H
	repo *repository.AlertRepository
	org  harness.Org

	// awake never had a snooze at all.
	awake uuid.UUID
	// quiet holds a snooze that is in force right now.
	quiet uuid.UUID
	// woken held a snooze that somebody ended early. `ended_at` is set.
	woken uuid.UUID
	// unswept holds a snooze whose `ended_at` is still NULL and whose clock ran
	// out ten minutes ago. The expiry sweep has not reached it.
	unswept uuid.UUID
}

func newTabWorld(t *testing.T) tabWorld {
	t.Helper()

	h := harness.New(t)
	org := h.Org()
	cl := h.Cluster(org)
	now := h.Now()

	w := tabWorld{
		h:    h,
		repo: repository.NewAlertRepository(h.Pool, h.Clock, false),
		org:  org,
	}

	mk := func(name string) uuid.UUID {
		return h.AlertWith(org, cl, map[string]string{
			"alertname": name, "severity": "critical",
		}).ID
	}

	w.awake = mk("NeverQuietened")
	w.quiet = mk("QuietRightNow")
	w.woken = mk("WokenEarly")
	w.unswept = mk("ClockRanOutUnswept")

	seedSnooze(t, h, org, w.quiet, snoozeSeed{
		at: now.Add(-time.Hour), until: now.Add(3 * time.Hour),
	})
	seedSnooze(t, h, org, w.woken, snoozeSeed{
		at: now.Add(-4 * time.Hour), until: now.Add(2 * time.Hour),
		endedAt: ptrTime(now.Add(-30 * time.Minute)), endedReason: ptrString("manual"),
	})
	seedSnooze(t, h, org, w.unswept, snoozeSeed{
		at: now.Add(-2 * time.Hour), until: now.Add(-10 * time.Minute),
	})

	return w
}

func (w tabWorld) list(t *testing.T, f domain.AlertFilter) []uuid.UUID {
	t.Helper()

	alerts, _, err := w.repo.List(w.h.Ctx, w.org.Scope, f, db.Keyset{Limit: 50})
	require.NoError(t, err)
	out := make([]uuid.UUID, len(alerts))
	for i, a := range alerts {
		out[i] = a.ID()
	}
	return out
}

// TestTheMainTabExcludesOnlyTheAlertsThatAreActuallyQuiet is the anti-join.
func TestTheMainTabExcludesOnlyTheAlertsThatAreActuallyQuiet(t *testing.T) {
	t.Parallel()

	w := newTabWorld(t)
	got := w.list(t, domain.AlertFilter{Snoozed: ptrBool(false)})

	require.ElementsMatch(t, []uuid.UUID{w.awake, w.woken, w.unswept}, got,
		"the main tab hides an alert only while oto is genuinely holding its notifications. "+
			"An ended snooze is over and an expired-but-unswept snooze is over in every way "+
			"that matters to an operator — hiding either would take a live alert off the "+
			"primary screen for reasons that have already stopped applying.")
	require.NotContains(t, got, w.quiet)
}

// TestTheQuietTabIsExactlyWhatTheMainTabWithheld is the other half, and the two
// have to be complements or something has been lost.
func TestTheQuietTabIsExactlyWhatTheMainTabWithheld(t *testing.T) {
	t.Parallel()

	w := newTabWorld(t)

	main := w.list(t, domain.AlertFilter{Snoozed: ptrBool(false)})
	quiet := w.list(t, domain.AlertFilter{Snoozed: ptrBool(true)})
	both := w.list(t, domain.AlertFilter{})

	require.Equal(t, []uuid.UUID{w.quiet}, quiet)

	// ⛔ THE PARTITION IS THE WHOLE SAFETY ARGUMENT. Splitting the list is only
	// defensible if nothing falls between the two tabs: an alert on neither is an
	// alert that has left the product, which is precisely the incident §B.8.6's
	// old rule was written to prevent.
	require.Len(t, both, len(main)+len(quiet),
		"every alert is on exactly one tab; an alert on neither has vanished from oto")
	require.ElementsMatch(t, both, append(append([]uuid.UUID{}, main...), quiet...))
}

// TestBothTabsHonourEveryOtherFilter is what makes the Quiet tab a tab rather
// than a second, poorer screen.
func TestBothTabsHonourEveryOtherFilter(t *testing.T) {
	t.Parallel()

	w := newTabWorld(t)

	named := domain.AlertFilter{
		Snoozed:    ptrBool(true),
		AlertNames: []string{"QuietRightNow"},
	}
	require.Equal(t, []uuid.UUID{w.quiet}, w.list(t, named))

	missed := domain.AlertFilter{
		Snoozed:    ptrBool(true),
		AlertNames: []string{"NeverQuietened"},
	}
	require.Empty(t, w.list(t, missed),
		"the Quiet tab carries the operator's filters; a tab that ignored them would make "+
			"its own badge a promise it could not keep")
}

// TestTheSnoozePredicateIsScopedToTheTenant is the §5.6 guard, on the one
// predicate in this file that reaches a second table.
//
// ⚠️ A JOIN IS A NEW WAY TO LEAK. The alert rows were already org-scoped; the
// snooze rows this correlates against are a different table with its own
// `org_id`, and a correlation on `alert_id` alone would have been *correct* —
// alert ids are unique — while quietly making one tenant's snooze table part of
// another tenant's query plan.
func TestTheSnoozePredicateIsScopedToTheTenant(t *testing.T) {
	t.Parallel()

	w := newTabWorld(t)
	other := w.h.Org()

	quiet, _, err := w.repo.List(w.h.Ctx, other.Scope,
		domain.AlertFilter{Snoozed: ptrBool(true)}, db.Keyset{Limit: 50})
	require.NoError(t, err)
	require.Empty(t, quiet, "another tenant's Quiet tab must be empty")
}

// TestTheRollUpSharesTheTabWithTheListBesideIt is the identity the roll-up
// endpoint promises: the buckets summarise exactly the list they sit beside.
func TestTheRollUpSharesTheTabWithTheListBesideIt(t *testing.T) {
	t.Parallel()

	w := newTabWorld(t)
	key, err := domain.NewRollupKey("alertname")
	require.NoError(t, err)

	quiet, _, err := w.repo.Rollup(w.h.Ctx, w.org.Scope,
		domain.AlertFilter{Snoozed: ptrBool(true)}, key, "", 50)
	require.NoError(t, err)
	require.Len(t, quiet, 1)
	require.Equal(t, "QuietRightNow", quiet[0].Key)
	require.Equal(t, 1, quiet[0].Total)

	main, _, err := w.repo.Rollup(w.h.Ctx, w.org.Scope,
		domain.AlertFilter{Snoozed: ptrBool(false)}, key, "", 50)
	require.NoError(t, err)
	for _, b := range main {
		require.NotEqual(t, "QuietRightNow", b.Key,
			"a bucket on the main tab must not count an alert the main tab does not list")
	}
}

// TestAlertsCarriesNoSnoozeColumn is the sequencing guard, asked of the live
// schema rather than of the migration files.
//
// ⛔ A CROSS-MODULE SQL READ HAS NO IMPORT AND NO COMPILER ERROR. `notification`
// and `grouping` both reached into `alerts` for this column as a string literal;
// dropping it while a literal still named it would have surfaced at runtime, on
// the card path, as a 42703 nobody sees until a page does not go out. The reads
// were redirected BEFORE the drop, and every statement above is planned against
// the real schema, which is the failure the compiler could not give us.
func TestAlertsCarriesNoSnoozeColumn(t *testing.T) {
	t.Parallel()

	w := newTabWorld(t)

	var n int
	require.NoError(t, w.h.Pool.QueryRow(w.h.Ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'alerts'
		   AND column_name LIKE '%snooz%'`).Scan(&n))
	require.Zero(t, n, "`alerts` has a snooze column again; the row is the record")

	require.NoError(t, w.h.Pool.QueryRow(w.h.Ctx, `
		SELECT count(*) FROM pg_indexes
		 WHERE schemaname = 'public' AND indexname = 'alerts_snooze_idx'`).Scan(&n))
	require.Zero(t, n, "alerts_snooze_idx served a base-table predicate that no longer exists")

	// And the index the two tabs actually ride is present, org-leading and
	// partial — neither of 00017's three indexes leads with `org_id`.
	var def string
	require.NoError(t, w.h.Pool.QueryRow(w.h.Ctx, `
		SELECT indexdef FROM pg_indexes
		 WHERE schemaname = 'public' AND indexname = 'alert_snoozes_active_org_idx'`).Scan(&def))
	require.Contains(t, def, "org_id")
	require.Contains(t, def, "ended_at IS NULL")
}

// ---------------------------------------------------------------- the seeding

// snoozeSeed is one `alert_snoozes` row, written directly: these tests are about
// which row a predicate reaches, not about how a snooze comes to exist.
type snoozeSeed struct {
	at          time.Time
	until       time.Time
	endedAt     *time.Time
	endedReason *string
}

func seedSnooze(t *testing.T, h *harness.H, org harness.Org, alertID uuid.UUID, s snoozeSeed) {
	t.Helper()

	h.Exec(`INSERT INTO alert_snoozes
	          (id, org_id, alert_id, alert_key, snoozed_at, snoozed_until,
	           snoozed_by_label, ended_at, ended_reason)
	        SELECT $1, $2, a.id, a.alert_key, $4, $5, 'Ada L.', $6, $7
	          FROM alerts a WHERE a.id = $3 AND a.org_id = $2`,
		id.New(), org.ID, alertID, s.at, s.until, s.endedAt, s.endedReason)
}

func ptrBool(v bool) *bool           { return &v }
func ptrTime(v time.Time) *time.Time { return &v }
func ptrString(v string) *string     { return &v }
