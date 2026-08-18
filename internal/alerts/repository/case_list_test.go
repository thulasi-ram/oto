package repository_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/test/harness"

	"github.com/thulasiram/oto/internal/alerts/repository"
)

// ⭐⭐ THE ORG-WIDE CASE LIST IS THE ONE READ THAT CROSSES TWO TABLES.
//
// `GET /api/v1/cases` filters on `alert_cases` — state, ack, group — and on
// `alerts` — severity, cluster, namespace, alertname — and the second half is
// reached through a correlated `EXISTS` rather than a JOIN, because the two
// tables share five column names. What is under test here is WHICH ROWS EACH
// PREDICATE SELECTS, and a fake would select whichever rows its author believed
// in — which is the belief being checked.
//
// ⛔ THE THREE THINGS THAT WOULD BE INVISIBLE TO A UNIT TEST:
//
//   - the `EXISTS` correlation. Getting `a.org_id = alert_cases.org_id` wrong
//     leaks across tenants and every in-process assertion still passes.
//   - `synthetic` defaulting to EXCLUDE. It lives inside that subquery, so an
//     `EXISTS` that dropped it would put oto's own drill plumbing into the
//     customer's queue.
//   - the two spellings of the ack facet. One is `= ANY($2)` and the other is
//     `= ($2::text[])[1]`; only a real Parse proves both bind and both answer.
//
// TestMain lives in prune_test.go; this file adds no second one.

// caseListWorld is one tenant holding four episodes, chosen so that every
// dimension of the filter separates at least two of them.
type caseListWorld struct {
	h    *harness.H
	repo *repository.CaseRepository
	org  harness.Org

	groupA uuid.UUID
	groupB uuid.UUID

	// openUnacked is the triage queue's whole population: firing, nobody has
	// looked at it, severity critical, group A.
	openUnacked uuid.UUID
	// openAcked is firing and signed for. Group A, severity critical.
	openAcked uuid.UUID
	// ended is a closed episode, resolved upstream. It must never appear under
	// `?state=open`, and it is the row that proves the state facet selects on
	// liveness rather than on nothing at all.
	ended uuid.UUID
	// warning is firing and unacked like the first, but severity `warning` and
	// group B — the row every alert-side facet has to be able to exclude.
	warning uuid.UUID
	// synthetic belongs to an alert oto manufactured for a delivery drill. It
	// must be absent unless asked for by name.
	synthetic uuid.UUID
}

func newCaseListWorld(t *testing.T) caseListWorld {
	t.Helper()

	h := harness.New(t)
	org := h.Org()
	cl := h.Cluster(org)
	src := h.Source(org, cl)

	gA := h.GroupWith(org, src, cl, map[string]string{
		"alertname": "HighErrorRate", "namespace": "payments", "severity": "critical",
	})
	gB := h.GroupWith(org, src, cl, map[string]string{
		"alertname": "DiskFilling", "namespace": "storage", "severity": "warning",
	})

	w := caseListWorld{
		h:      h,
		repo:   repository.NewCaseRepository(h.Pool),
		org:    org,
		groupA: gA.ID,
		groupB: gB.ID,
	}

	seed := func(g harness.Group, kv map[string]string) uuid.UUID {
		a := h.AlertWith(org, cl, kv)
		return h.Case(a, g).ID
	}

	w.openUnacked = seed(gA, map[string]string{
		"alertname": "HighErrorRate", "namespace": "payments",
		"severity": "critical", "instance": "i-1",
	})
	w.openAcked = seed(gA, map[string]string{
		"alertname": "HighErrorRate", "namespace": "payments",
		"severity": "critical", "instance": "i-2",
	})
	w.ended = seed(gA, map[string]string{
		"alertname": "HighErrorRate", "namespace": "payments",
		"severity": "critical", "instance": "i-3",
	})
	w.warning = seed(gB, map[string]string{
		"alertname": "DiskFilling", "namespace": "storage",
		"severity": "warning", "instance": "i-4",
	})
	w.synthetic = seed(gA, map[string]string{
		"alertname": "HighErrorRate", "namespace": "payments",
		"severity": "critical", "instance": "i-5",
	})

	// ⭐ `started_at` IS SPREAD FIRST, AND EVERY LATER TIMESTAMP IS DERIVED FROM
	// IT. The harness opens every episode at one instant, so the page order would
	// otherwise be whatever the planner returned; and `case_ackorder_ck` and
	// `case_order_ck` both compare against `started_at`, so an `acked_at` or an
	// `ended_at` pinned to the wall clock before the spread is a row the database
	// refuses. Deriving them in SQL keeps the fixture legal by construction.
	//
	// The order is oldest-first in the slice, so the newest-first page is the
	// slice reversed: synthetic, warning, ended, openAcked, openUnacked.
	order := []uuid.UUID{w.openUnacked, w.openAcked, w.ended, w.warning, w.synthetic}
	now := h.Now()
	for i, c := range order {
		at := now.Add(-time.Duration(len(order)-1-i) * time.Minute)
		h.Exec(`UPDATE alert_cases
		           SET started_at = $2, source_starts_at = $2,
		               last_observed_at = GREATEST($2, last_observed_at)
		         WHERE id = $1`, c, at)
	}

	// The receipt is written on the CASE and nowhere else (00049).
	h.Exec(`UPDATE alert_cases
	           SET ack_state = 'acked', acked_at = started_at, acked_by_label = 'Ada Lovelace'
	         WHERE id = $1`, w.openAcked)
	// A closed episode: the state, an end time and a reason, all three together —
	// `case_terminal_ended` and `case_resolve_ck` refuse any two of them without
	// the third, and since ADR 0040 `resolve_reason` is the ONLY thing that says
	// this one was resolved upstream rather than expired.
	h.Exec(`UPDATE alert_cases
	           SET state = 'closed', ended_at = started_at + interval '30 seconds',
	               resolve_reason = 'upstream'
	         WHERE id = $1`, w.ended)
	h.Exec(`UPDATE alerts SET synthetic = true
	         WHERE id = (SELECT alert_id FROM alert_cases WHERE id = $1)`, w.synthetic)

	// ⚠️ `harness.AlertWith` WRITES `alertname`, `severity` AND `cluster_key` BUT
	// NOT `namespace` — the promoted column is nullable and the builder leaves it
	// alone. The real ingest path projects it out of the label set, and the
	// `?namespace=` facet reads the COLUMN, so a fixture that skipped this would
	// assert that the filter narrows to nothing and call it a pass.
	h.Exec(`UPDATE alerts SET namespace = labels->>'namespace' WHERE org_id = $1`, org.ID)

	return w
}

func (w caseListWorld) list(t *testing.T, f domain.CaseFilter) []uuid.UUID {
	t.Helper()

	rows, _, err := w.repo.ListCases(w.h.Ctx, w.org.Scope, f, db.Keyset{Limit: 50})
	require.NoError(t, err)

	out := make([]uuid.UUID, 0, len(rows))
	for _, c := range rows {
		out = append(out, c.ID())
	}
	return out
}

func ptrTo[T any](v T) *T { return &v }

// TestTheCaseListDefaultsToEverythingRealAndNewestFirst.
//
// The promise: with no filter at all the list is every NON-SYNTHETIC episode in
// the org, ordered by `started_at DESC`. `synthetic` is the one dimension whose
// absence means EXCLUDE, and it is enforced inside the `EXISTS` — so this is also
// the assertion that the subquery is applied at all.
func TestTheCaseListDefaultsToEverythingRealAndNewestFirst(t *testing.T) {
	t.Parallel()

	w := newCaseListWorld(t)
	got := w.list(t, domain.CaseFilter{})

	require.Equal(t, []uuid.UUID{w.warning, w.ended, w.openAcked, w.openUnacked}, got,
		"the default list is every real episode, newest first; the synthetic one is a "+
			"delivery drill and is not the customer's history")
}

// TestTheCaseListsAckFacetAnswersFromTheEpisode.
//
// The promise: `?ack=` selects on `alert_cases.ack_state`. It exists nowhere else
// — `alerts` has carried no ack column since 00049 — so this is the only list in
// the product that can answer it.
//
// ⭐ BOTH SPELLINGS ARE EXERCISED. One value compiles to `ack_state =
// ($3::text[])[1]`, which is what lets `case_ack_idx` be an Index Cond and the
// LIMIT stop the scan; two compile to `= ANY($3)`. A test that only ever sent one
// would leave the other arm's binding unproven.
func TestTheCaseListsAckFacetAnswersFromTheEpisode(t *testing.T) {
	t.Parallel()

	w := newCaseListWorld(t)

	unacked := w.list(t, domain.CaseFilter{
		AckStates: []domain.AckState{domain.AckStateUnacked},
		States:    []domain.CaseState{domain.CaseOpen},
	})
	require.Equal(t, []uuid.UUID{w.warning, w.openUnacked}, unacked,
		"the queue is the OPEN episodes nobody has signed for")

	acked := w.list(t, domain.CaseFilter{
		AckStates: []domain.AckState{domain.AckStateAcked},
	})
	require.Equal(t, []uuid.UUID{w.openAcked}, acked)

	both := w.list(t, domain.CaseFilter{
		AckStates: []domain.AckState{domain.AckStateUnacked, domain.AckStateAcked},
		States:    []domain.CaseState{domain.CaseOpen},
	})
	require.Equal(t, []uuid.UUID{w.warning, w.openAcked, w.openUnacked}, both,
		"the multi-value arm is `= ANY($2)` and must select the union, not the first value")
}

// ⭐⭐ TestTheStateFacetIsTheOneLivenessAxis — the successor to a test called
// `…OpenFacetAndItsStateFacetAgree`, and its DELETION is half the assertion.
//
// There used to be two spellings of this question: a four-valued `?state=` and a
// separate `?open=` boolean, kept apart for a planner reason rather than a
// semantic one. That test existed to catch them drifting. ADR 0040 removed the
// drift by removing the second spelling, so what is left to prove is that the
// surviving one still selects the rows the removed one did — and, critically,
// that naming BOTH values is not accidentally narrower than naming neither.
//
// ⛔ THE `both` CASE IS NOT PADDING. `ListCases` only splices a predicate when
// exactly one value is named; a future edit that emitted `ended_at IS NULL AND
// ended_at IS NOT NULL` for a two-value filter would return nothing at all, and
// nothing is a plausible-looking answer for an empty queue.
func TestTheStateFacetIsTheOneLivenessAxis(t *testing.T) {
	t.Parallel()

	w := newCaseListWorld(t)

	open := w.list(t, domain.CaseFilter{States: []domain.CaseState{domain.CaseOpen}})
	require.Equal(t, []uuid.UUID{w.warning, w.openAcked, w.openUnacked}, open)

	closed := w.list(t, domain.CaseFilter{States: []domain.CaseState{domain.CaseClosed}})
	require.Equal(t, []uuid.UUID{w.ended}, closed)

	unfiltered := w.list(t, domain.CaseFilter{})
	both := w.list(t, domain.CaseFilter{
		States: []domain.CaseState{domain.CaseOpen, domain.CaseClosed},
	})
	require.Equal(t, unfiltered, both,
		"the column has two values, so naming both is no constraint at all")
	require.Equal(t, []uuid.UUID{w.warning, w.ended, w.openAcked, w.openUnacked}, both)
}

// TestTheCaseListsIdentityFacetsAreAnsweredThroughTheAlert.
//
// The promise: `severity`, `namespace`, `cluster` and `alertname` are columns of
// `alerts`, and filtering on them narrows the CASE list correctly. They travel
// through the correlated `EXISTS`, so a mistake there is either a page that never
// narrows or a page that is silently empty.
func TestTheCaseListsIdentityFacetsAreAnsweredThroughTheAlert(t *testing.T) {
	t.Parallel()

	w := newCaseListWorld(t)

	require.Equal(t,
		[]uuid.UUID{w.ended, w.openAcked, w.openUnacked},
		w.list(t, domain.CaseFilter{Severities: []string{"critical"}}))

	require.Equal(t,
		[]uuid.UUID{w.warning},
		w.list(t, domain.CaseFilter{AlertNames: []string{"DiskFilling"}}))

	require.Equal(t,
		[]uuid.UUID{w.warning},
		w.list(t, domain.CaseFilter{Namespaces: []string{"storage"}}))

	require.Empty(t,
		w.list(t, domain.CaseFilter{ClusterKeys: []string{"a-cluster-that-does-not-exist"}}),
		"an unknown cluster narrows to nothing rather than being ignored")

	require.Equal(t,
		[]uuid.UUID{w.synthetic},
		w.list(t, domain.CaseFilter{Synthetic: ptrTo(true)}),
		"a drill's own result screen is the one caller that asks for these by name")
}

// TestTheCaseListsGroupFacetNamesTheNotificationGrouping.
//
// The promise: `?group_id=` restricts to the episodes one AlertGroup generation
// holds — the object that owns one Slack thread. It is plumbing, not a
// correlation and not an incident, and this filter is how "which episodes is that
// thread about" is answered.
func TestTheCaseListsGroupFacetNamesTheNotificationGrouping(t *testing.T) {
	t.Parallel()

	w := newCaseListWorld(t)

	require.Equal(t,
		[]uuid.UUID{w.ended, w.openAcked, w.openUnacked},
		w.list(t, domain.CaseFilter{GroupIDs: []uuid.UUID{w.groupA}}))
	require.Equal(t,
		[]uuid.UUID{w.warning},
		w.list(t, domain.CaseFilter{GroupIDs: []uuid.UUID{w.groupB}}))
	require.Equal(t,
		[]uuid.UUID{w.warning, w.ended, w.openAcked, w.openUnacked},
		w.list(t, domain.CaseFilter{GroupIDs: []uuid.UUID{w.groupA, w.groupB}}))
}

// ⛔ TestTheCaseListCannotSeeAnotherTenantsEpisodes.
//
// The promise: the org predicate is on the case AND inside the `EXISTS`. This is
// the only assertion standing between a missing `a.org_id = alert_cases.org_id`
// and one customer reading another's queue — and the denormalised
// `alert_cases.org_id` is exactly what would make such a leak look correct to
// every other read.
func TestTheCaseListCannotSeeAnotherTenantsEpisodes(t *testing.T) {
	t.Parallel()

	w := newCaseListWorld(t)
	stranger := w.h.Org()

	rows, _, err := w.repo.ListCases(w.h.Ctx, stranger.Scope, domain.CaseFilter{}, db.Keyset{Limit: 50})
	require.NoError(t, err)
	require.Empty(t, rows, "another tenant's episodes do not come back forbidden; they do not come back")
}

// TestTheCaseListPagesByKeysetAndNeverRepeatsARow.
//
// The promise: the cursor is `(started_at, id)` and paging through it visits
// every row exactly once. `started_at` alone is not a total order — one
// Alertmanager batch opens every episode in it at the same instant — which is why
// the id is a tiebreak and not decoration.
func TestTheCaseListPagesByKeysetAndNeverRepeatsARow(t *testing.T) {
	t.Parallel()

	w := newCaseListWorld(t)

	var seen []uuid.UUID
	page := db.Keyset{Limit: 2}
	for range 5 {
		rows, cur, err := w.repo.ListCases(w.h.Ctx, w.org.Scope, domain.CaseFilter{}, page)
		require.NoError(t, err)
		for _, c := range rows {
			seen = append(seen, c.ID())
		}
		if !cur.HasMore {
			break
		}
		page = db.Keyset{Limit: 2, Cursor: cur}
	}
	require.Equal(t, []uuid.UUID{w.warning, w.ended, w.openAcked, w.openUnacked}, seen,
		"paging two at a time visits the same four rows, in the same order, once each")
}
