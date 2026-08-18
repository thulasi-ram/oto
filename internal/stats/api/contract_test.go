package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/stats/domain"
	"github.com/thulasiram/oto/internal/stats/repository"
	"github.com/thulasiram/oto/internal/stats/service"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// THE STATS TAG, ASSERTED AGAINST api/openapi/openapi.yaml.
//
// The dashboard roll-up and the hygiene report are two READS with no path
// parameters, which makes them look like the safest surface in the API and is
// exactly why they went unchecked. Two things here are not merely cosmetic:
//
//   - `resolved` and `expired` are SEPARATE required members and are never summed
//     into one "closed" bucket. Conflating them is the precise lie oto exists to
//     prevent, and a DTO that dropped one would still return a plausible-looking
//     dashboard;
//   - there is NO PER-PERSON DATA and no way to ask for it (SPEC R8). The rollup
//     carries no user column, so the absence is structural — and an operation
//     that started accepting a `user` filter would fail §E.3 here rather than
//     ship.
//
// Every response assertion below is `schema.Assert`, never a hand-written shape:
// a second copy of the contract drifts exactly the way the DTOs did.

/* -------------------------------------------------------------------------- */
/* Fixtures and fakes                                                         */
/* -------------------------------------------------------------------------- */

// statsStamp is the instant every fixture timestamp derives from, so that
// `format: date-time` is asserted against a value a test can name.
var statsStamp = time.Date(2026, 8, 9, 12, 0, 0, 114_000_000, time.UTC)

// overviewFixture populates EVERY count, including the optional ones. A fixture
// that left `channels`, `window` and the skew/divergence pair zero would satisfy
// the schema while proving nothing about the members that are hardest to get
// right.
func overviewFixture() service.OverviewResult {
	return service.OverviewResult{
		Overview: domain.Overview{
			Alerts: domain.AlertCounts{
				Firing: 12, Suppressed: 3, Resolved: 118, Expired: 4,
				Acked: 9, Unacked: 6, Flapping: 2,
			},
			Groups:     domain.GroupCounts{Open: 5, Closed: 61, Storm: 1},
			Deliveries: domain.DeliveryCounts{Sent: 402, Failed: 7, Dead: 1, Skipped: 12, Pending: 3, Ambiguous: 2},
			Sources: domain.SourceCounts{
				Healthy: 4, Degraded: 1, Unreachable: 0, Unknown: 1,
				MaxClockSkewMS: 412, TotalDivergence: 2,
			},
			Channels: domain.ChannelCounts{Healthy: 3, Degraded: 1, AuthFailed: 0, ConfigInvalid: 1},
		},
		Window: service.Window{
			Since: statsStamp.Add(-24 * time.Hour),
			Until: statsStamp,
		},
		GeneratedAt: statsStamp,
	}
}

// qualityFixture is the row the endpoint exists to produce: a rule that fired 47
// times, cost 47 notifications and was acknowledged 0 times.
func qualityFixture() service.QualityResult {
	q := domain.AlertQuality{
		AlertName:          "KubePodCrashLooping",
		ClusterKey:         "prod-eu",
		Cases:              47,
		Notifications:      47,
		Deliveries:         51,
		AckedCases:         0,
		AutoResolved:       31,
		Expired:            16,
		TotalFiringSeconds: 86_400,
		FlapTransitions:    9,
	}
	return service.QualityResult{
		Rows: []repository.QualityRow{{
			Quality:   q,
			SortValue: float64(q.Cases),
			KeysetKey: "prod-eu\x1fKubePodCrashLooping",
		}},
		HasMore: false,
		Window:  service.Window{Since: statsStamp.Add(-30 * 24 * time.Hour), Until: statsStamp},
	}
}

// fakeStats is a StatsService that answers from fixtures and REMEMBERS what it
// was asked, so a test can prove the query the handler compiled is the query the
// caller wrote.
type fakeStats struct {
	overview service.OverviewResult
	quality  service.QualityResult

	overviewCalls int
	qualityCalls  int
	lastQuality   service.QualityQuery
}

func (f *fakeStats) Overview(
	_ context.Context, _ db.TenantScope, _, _ time.Time, _ []string,
) (service.OverviewResult, error) {
	f.overviewCalls++
	return f.overview, nil
}

func (f *fakeStats) AlertQuality(
	_ context.Context, _ db.TenantScope, q service.QualityQuery,
) (service.QualityResult, error) {
	f.qualityCalls++
	f.lastQuality = q
	return f.quality, nil
}

func newStatsStack(t *testing.T) (*apitest.Client, *fakeStats) {
	t.Helper()
	svc := &fakeStats{overview: overviewFixture(), quality: qualityFixture()}
	return apitest.New(NewRouter(svc, clock.New())), svc
}

/* -------------------------------------------------------------------------- */
/* 1. Both operations answer the shape their own schema declares              */
/* -------------------------------------------------------------------------- */

// TestTheOverviewAnswersTheShapeTheContractDeclares.
//
// ⛔ AND `resolved` AND `expired` ARE BOTH THERE, SEPARATELY. Summing them into
// one number would make every dashboard read "118 closed" when 4 of those are
// alerts oto merely lost sight of.
func TestTheOverviewAnswersTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	c, svc := newStatsStack(t)
	resp := c.GET("/stats/overview?since=2026-08-08T12:00:00Z&until=2026-08-09T12:00:00Z&cluster=prod-eu").
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "getStatsOverview", http.StatusOK, resp.Body())

	if svc.overviewCalls != 1 {
		t.Fatalf("the service was asked %d times, want 1", svc.overviewCalls)
	}

	data, _ := resp.JSON(t)["data"].(map[string]any)
	alerts, ok := data["alerts"].(map[string]any)
	if !ok {
		t.Fatalf("alerts is %T, want the open-state roll-up", data["alerts"])
	}
	if alerts["resolved"] != float64(118) || alerts["expired"] != float64(4) {
		t.Fatalf("resolved = %v and expired = %v; they are counted separately and never "+
			"summed into one closed bucket", alerts["resolved"], alerts["expired"])
	}
}

// TestTheHygieneReportAnswersTheShapeTheContractDeclares.
//
// It is a keyset page, so the envelope carries `page` beside `data` and `meta` —
// and `page` has no `total`, because counting an unbounded collection is a query
// oto refuses to make.
func TestTheHygieneReportAnswersTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	c, svc := newStatsStack(t)
	resp := c.GET("/stats/alert-quality?sort=-cases&limit=25&cluster=prod-eu").
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "getAlertQualityStats", http.StatusOK, resp.Body())

	if svc.qualityCalls != 1 {
		t.Fatalf("the service was asked %d times, want 1", svc.qualityCalls)
	}
	if svc.lastQuality.Sort != domain.SortCasesDesc {
		t.Fatalf("sort = %q, want the key the caller asked for", svc.lastQuality.Sort)
	}
	if svc.lastQuality.Limit != 25 {
		t.Fatalf("limit = %d, want 25", svc.lastQuality.Limit)
	}

	body := resp.JSON(t)
	if _, ok := body["page"]; !ok {
		t.Fatal("the hygiene report is paginated and must carry a page object")
	}
	rows, _ := body["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("data has %d rows, want 1", len(rows))
	}
	row, _ := rows[0].(map[string]any)
	if row["ack_rate"] != float64(0) || row["cases"] != float64(47) {
		t.Fatalf("row = %#v; the answer this endpoint exists for is \"47 times, "+
			"acknowledged 0\"", row)
	}
}

// ⛔ TestTheHygieneReportHasNoPerPersonMember is SPEC R8 made mechanical.
//
// Response-time leaderboards and per-individual aggregates are unrepresentable
// rather than merely omitted — the rollup carries no user column — and this
// asserts the boundary at the only place a future change could quietly cross it:
// the wire. A feature that does not exist cannot be misused.
func TestTheHygieneReportHasNoPerPersonMember(t *testing.T) {
	t.Parallel()

	c, _ := newStatsStack(t)
	rows, _ := c.GET("/stats/alert-quality").MustStatus(t, http.StatusOK).JSON(t)["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("data has %d rows, want 1", len(rows))
	}
	row, _ := rows[0].(map[string]any)
	// vocab:allow the scope-boundary guard must name the words it forbids; this list IS the enforcement.
	for _, forbidden := range []string{"user", "user_id", "acked_by", "responder", "assignee", "mttr_by_user"} {
		if _, present := row[forbidden]; present {
			t.Fatalf("the hygiene row carries %q; there is no per-person data here and no "+
				"way to add one without changing a rollup that has no user column", forbidden)
		}
	}
}

/* -------------------------------------------------------------------------- */
/* 2. Tenant scoping                                                          */
/* -------------------------------------------------------------------------- */

// TestTheRollUpIsScopedToTheCallersTenant.
//
// Neither stats operation takes a resource id, so there is no "somebody else's
// id" to put in a path: the whole surface is scoped by the principal. What has to
// hold instead is that a caller with NO principal never sees a roll-up, because a
// dashboard is an org's operational posture and answering it unauthenticated
// would leak the shape of somebody's incident load.
func TestTheRollUpIsScopedToTheCallersTenant(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ op, target string }{
		{"getStatsOverview", "/stats/overview"},
		{"getAlertQualityStats", "/stats/alert-quality"},
	} {
		t.Run(tc.op, func(t *testing.T) {
			t.Parallel()

			c, svc := newStatsStack(t)
			resp := c.Anonymous().GET(tc.target).MustStatus(t, http.StatusUnauthorized)
			schema.AssertProblem(t, tc.op, http.StatusUnauthorized, resp.Body())

			if svc.overviewCalls != 0 || svc.qualityCalls != 0 {
				t.Fatal("an unauthenticated request still reached the rollup")
			}
		})
	}
}

/* -------------------------------------------------------------------------- */
/* 3. The refusals a caller can provoke                                       */
/* -------------------------------------------------------------------------- */

// ⛔ TestATypoedWindowParameterIsRefusedRatherThanIgnored is §E.3 on the
// dashboard.
//
// `?clusters=prod-eu` — the plural, which is what a caller writes from memory —
// silently ignored would produce an org-wide roll-up rendered under a cluster
// filter's heading. The numbers would be wrong and would look right.
func TestATypoedWindowParameterIsRefusedRatherThanIgnored(t *testing.T) {
	t.Parallel()

	c, svc := newStatsStack(t)
	resp := c.GET("/stats/overview?clusters=prod-eu").MustStatus(t, http.StatusBadRequest)
	schema.AssertProblem(t, "getStatsOverview", http.StatusBadRequest, resp.Body())
	resp.MustViolate(t, "clusters")

	if svc.overviewCalls != 0 {
		t.Fatal("a refused request still reached the rollup")
	}
}

// TestAnUnsupportedSortKeyIsRefusedByName.
//
// The sort set is CLOSED because each value maps onto one indexed ordering over
// the rollup; an arbitrary key would be an unindexed sort over an aggregate. The
// refusal names `sort` so the control that offered it can be highlighted.
func TestAnUnsupportedSortKeyIsRefusedByName(t *testing.T) {
	t.Parallel()

	c, svc := newStatsStack(t)
	resp := c.GET("/stats/alert-quality?sort=-chaos").
		MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "getAlertQualityStats", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "sort")

	if svc.qualityCalls != 0 {
		t.Fatal("a refused sort key still reached the rollup")
	}
}

// TestAMalformedWindowIsRefusedByName. `since` and `until` are RFC 3339
// instants; "yesterday" is a value a human would try and a value no window
// arithmetic can use.
func TestAMalformedWindowIsRefusedByName(t *testing.T) {
	t.Parallel()

	c, svc := newStatsStack(t)
	resp := c.GET("/stats/overview?since=yesterday").
		MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "getStatsOverview", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "since")

	if svc.overviewCalls != 0 {
		t.Fatal("a refused window still reached the rollup")
	}
}
