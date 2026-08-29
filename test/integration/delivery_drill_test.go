package integration

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/app"
	"github.com/thulasiram/oto/internal/platform/db"
)

// ⭐⭐ THIS FILE ANSWERS THE QUESTION THAT MATTERS ABOUT DELIVERY DRILLS, and it
// is not "does the card arrive".
//
// A drill manufactures an alert and pushes it through the real pipeline. That
// makes it useful. It also makes it DANGEROUS, because oto is sold on the history
// and the hygiene numbers it keeps, and a fake alert that leaked into them would
// make every figure slightly false — and a slightly false figure is believed.
//
// So these tests are about EXCLUSION and DISPOSAL, against the real schema and
// the real SQL. They insert a synthetic alert the way the pipeline would and then
// ask every aggregate the issue named: the daily hygiene rollup, the dashboard
// overview, the alert list default and the label typeaheads. If any of them
// counts it, the number a customer pays for is wrong.

// syntheticFixture writes one synthetic alert, its episode, its group, its
// notification and its delivery — exactly the row shape a drill produces — plus a
// REAL alert beside it, so every assertion below is "one, not two" rather than
// "zero", and a query that excluded everything would fail just as loudly as one
// that excluded nothing.
type syntheticFixture struct {
	realAlert      uuid.UUID
	syntheticAlert uuid.UUID
	drillLabel     string
	// day is the instant every seeded row is stamped with, and the UTC day the
	// hygiene rollup is asked to recompute.
	//
	// ⛔ IT MUST BE IN THE PAST AND INSIDE THE DASHBOARD'S DEFAULT WINDOW, which is
	// `[now-24h, now]` (stats/service.DefaultOverviewWindow) and is applied as
	// `last_seen_at >= since AND last_seen_at <= until`. A fixture pinned to a fixed
	// HOUR of the UTC day — 06:00, say — is in the FUTURE for the six hours after
	// midnight UTC, and an alert stamped in the future is outside `<= until`: the
	// overview then counts zero firing alerts and the "one, not two" assertion below
	// reads as a product bug that is really a clock. One hour ago is inside the
	// window at every hour of the day, and still lands squarely inside the UTC day
	// `stats.Rollup` recomputes, which is the other thing this stamp has to satisfy.
	day time.Time
}

func seedSynthetic(t *testing.T, e *env, orgID uuid.UUID, clusterID, _ uuid.UUID) syntheticFixture {
	t.Helper()

	f := syntheticFixture{
		realAlert:      uuid.New(),
		syntheticAlert: uuid.New(),
		drillLabel:     uuid.NewString(),
		day:            time.Now().UTC().Add(-time.Hour),
	}

	insertAlert := func(id uuid.UUID, name string, synthetic bool, labels string) {
		t.Helper()
		if _, err := e.pool.Exec(e.ctx, `
INSERT INTO alerts (id, org_id, cluster_id, alert_key, source_fingerprint, alertname, severity,
                    namespace, service, cluster_key, labels, annotations, state,
                    first_seen_at, last_seen_at, last_state_change_at, total_cases, synthetic)
VALUES ($1, $2, $3, $4, $5, $6, 'warning', 'oto', 'oto', 'prod-eu', $7::jsonb, '{}'::jsonb,
        'firing', $8, $8, $8, 1, $9)`,
			id, orgID, clusterID, alertKeyFor(id), "0123456789abcdef", name, labels, f.day, synthetic,
		); err != nil {
			t.Fatalf("seed alert %s: %v", name, err)
		}
	}

	insertAlert(f.realAlert, "RealHighErrorRate", false, `{"alertname":"RealHighErrorRate"}`)
	insertAlert(f.syntheticAlert, "OtoDeliveryDrill", true,
		`{"alertname":"OtoDeliveryDrill","oto_drill":"`+f.drillLabel+`"}`)

	for _, a := range []uuid.UUID{f.realAlert, f.syntheticAlert} {
		if _, err := e.pool.Exec(e.ctx, `
WITH allocated AS (
  INSERT INTO org_case_numbers (org_id, next_number) VALUES ($2, 2)
  ON CONFLICT (org_id) DO UPDATE
          SET next_number = org_case_numbers.next_number + 1
    RETURNING next_number - 1 AS number
)
INSERT INTO alert_cases (id, org_id, alert_id, seq, number, state, started_at, last_observed_at,
                               source_starts_at, ack_state)
SELECT $1, $2, $3, 1, (SELECT number FROM allocated), 'open', $4, $4, $4, 'unacked'`,
			uuid.New(), orgID, a, f.day); err != nil {
			t.Fatalf("seed case: %v", err)
		}
	}

	// ⛔ THE SYNTHETIC GROUP AND ITS MEMBERSHIP UPDATE WERE SEEDED HERE AND ARE GONE
	// (git-bug `7570090`, migration `00069`): `alert_groups` is dropped and so is
	// `alert_cases.group_id`, so there is no generation to open and no membership to
	// point at one. The synthetic ALERT and its Case above carry the whole fixture now.
	//
	// ⭐ AND THE PROVENANCE MARK IS NOT WEAKENED BY LOSING THE SECOND COPY.
	// `alert_groups.synthetic` was a DENORMALISATION — `00039`'s own header calls
	// `alerts.synthetic` the source of truth and says the copy existed to avoid "a
	// nested loop through `alert_group_members`", a table `00051` dropped. So the
	// justification for the copy had already evaporated; this only removes the copy.

	return f
}

func alertKeyFor(id uuid.UUID) string { return "ak_" + base32ish(id) }

// base32ish renders 26 characters from the `[0-9a-v]` alphabet the DDL's
// `alerts_key_ck` regex demands. It is not the real §C.2 hash and does not need
// to be: these tests are about what the aggregates COUNT, not about identity.
func base32ish(id uuid.UUID) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuv"
	b := id[:]
	out := make([]byte, 26)
	for i := range out {
		out[i] = alphabet[int(b[i%len(b)])%len(alphabet)]
	}
	return string(out)
}

// ⭐⭐ THE HYGIENE ROLLUP. `alert_quality_daily` is an UPSERT of a whole day, so a
// synthetic alert counted here would survive every recompute forever, in the
// exact table the report the product is sold on reads from.
func TestSyntheticAlertsAreExcludedFromTheHygieneRollup(t *testing.T) {
	e := newEnv(t)
	boot, cluster, source := bootstrapSource(t, e, "drill-rollup")
	f := seedSynthetic(t, e, boot.OrgID, cluster, source)

	scope, err := db.NewTenantScope(boot.OrgID)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	if _, err := e.container.Stats.Rollup(e.ctx, scope, f.day); err != nil {
		t.Fatalf("rollup: %v", err)
	}

	var names []string
	rows, err := e.pool.Query(e.ctx,
		`SELECT alertname FROM alert_quality_daily WHERE org_id = $1 AND day = $2::date`,
		boot.OrgID, f.day)
	if err != nil {
		t.Fatalf("read rollup: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, n)
	}

	if len(names) != 1 || names[0] != "RealHighErrorRate" {
		t.Fatalf("alert_quality_daily holds %v — a delivery drill has been counted as alert "+
			"history, and because the rollup is an upsert the wrong number is now permanent", names)
	}
}

// ⭐ THE DASHBOARD. It was THREE separate CTEs that had to learn about synthetics,
// one per table counted — `alerts`, `alert_groups` and `notification_deliveries`.
// ⛔ It is TWO since `alert_groups` was dropped (git-bug `7570090`), and the delivery
// CTE's exclusion changed shape with it: it used to reach the provenance mark through
// `notification -> group.synthetic` and now reaches `alerts.synthetic` directly. This
// test is the one that proves the new path still excludes, so it matters MORE than it
// did — a wrong predicate here changes dashboard numbers with nothing else noticing.
func TestSyntheticAlertsAreExcludedFromTheDashboardOverview(t *testing.T) {
	e := newEnv(t)
	boot, cluster, source := bootstrapSource(t, e, "drill-overview")
	seedSynthetic(t, e, boot.OrgID, cluster, source)

	// ⛔ `data.groups.open` WAS READ HERE AND IS DELETED (git-bug `7570090`).
	// `StatsOverviewDTO.groups` left the contract with `alert_groups`, so the key is
	// simply absent — and an absent key decodes to the zero value, which means the
	// old `want 0` assertion would have gone on PASSING for ever while proving
	// nothing at all. A vacuous assertion is worse than none: it reads like
	// coverage.
	var out struct {
		Data struct {
			Alerts struct {
				Firing int `json:"firing"`
			} `json:"alerts"`
			Deliveries struct {
				Sent int `json:"sent"`
			} `json:"deliveries"`
		} `json:"data"`
	}
	e.do(t, http.MethodGet, "/api/v1/stats/overview", boot.Token, nil, http.StatusOK, &out)

	if out.Data.Alerts.Firing != 1 {
		t.Errorf("firing = %d, want 1 — the dashboard is counting a delivery drill as a firing alert",
			out.Data.Alerts.Firing)
	}
	// The delivery CTE is the half whose exclusion CHANGED SHAPE: it used to reach
	// the provenance mark through `notification -> group.synthetic` and now reaches
	// `alerts.synthetic` directly. The drill seeds no deliveries, so a predicate
	// that stopped excluding would have to invent one to break this — but a
	// predicate that started excluding EVERYTHING breaks it too, which is the half
	// the group-shaped assertion used to carry.
	if out.Data.Deliveries.Sent != 0 {
		t.Errorf("sent deliveries = %d, want 0 — the dashboard is counting a drill's fan-out",
			out.Data.Deliveries.Sent)
	}
}

// ⭐ THE ALERT LIST. The default excludes synthetics — the OPPOSITE default from
// `snoozed`, which is included, because a snoozed alert is real and a synthetic
// one is not.
func TestTheAlertListExcludesSyntheticsByDefaultAndCanBeAskedForThem(t *testing.T) {
	e := newEnv(t)
	boot, cluster, source := bootstrapSource(t, e, "drill-list")
	seedSynthetic(t, e, boot.OrgID, cluster, source)

	type listOut struct {
		Data []struct {
			AlertName string `json:"alertname"`
			Synthetic bool   `json:"synthetic"`
		} `json:"data"`
	}

	var def listOut
	e.do(t, http.MethodGet, "/api/v1/alerts", boot.Token, nil, http.StatusOK, &def)
	if len(def.Data) != 1 || def.Data[0].AlertName != "RealHighErrorRate" {
		t.Fatalf("the default alert list returned %d rows (%+v) — a drill's alert is in the "+
			"history the product is selling", len(def.Data), def.Data)
	}
	if def.Data[0].Synthetic {
		t.Error("the real alert is marked synthetic")
	}

	var only listOut
	e.do(t, http.MethodGet, "/api/v1/alerts?synthetic=true", boot.Token, nil, http.StatusOK, &only)
	if len(only.Data) != 1 || !only.Data[0].Synthetic {
		t.Fatalf("?synthetic=true returned %+v — a drill's result screen cannot link to the row it made",
			only.Data)
	}
}

// The label typeaheads feed the filter bar. A drill writes an `oto_drill` label
// carrying a uuid; offering it, with a count of one, forever, would advertise
// oto's own plumbing as if it were the customer's estate.
func TestLabelDiscoveryExcludesSynthetics(t *testing.T) {
	e := newEnv(t)
	boot, cluster, source := bootstrapSource(t, e, "drill-labels")
	seedSynthetic(t, e, boot.OrgID, cluster, source)

	var out struct {
		Data []struct {
			Value string `json:"value"`
		} `json:"data"`
	}
	e.do(t, http.MethodGet, "/api/v1/labels", boot.Token, nil, http.StatusOK, &out)
	for _, row := range out.Data {
		if row.Value == "oto_drill" {
			t.Fatal("the label typeahead offers `oto_drill` — oto is advertising its own plumbing " +
				"as part of the operator's estate")
		}
	}
}

// ⭐ THE ENDPOINT. It answers 202 with the full stage list, because the pipeline
// is asynchronous by contract and a client must render the same component for the
// first response and every poll after it.
func TestStartingADrillAnswers202WithEveryStage(t *testing.T) {
	e := newEnv(t)
	boot, _, source := bootstrapSource(t, e, "drill-start")

	var started struct {
		Data struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Stages []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"stages"`
			FailedStage *string `json:"failed_stage"`
		} `json:"data"`
	}
	e.do(t, http.MethodPost, "/api/v1/drills", boot.Token,
		map[string]any{"source_id": source.String()}, http.StatusAccepted, &started)

	// ⛔ IT WAS TEN AND THE `group` STAGE IS THE ONE THAT WENT (git-bug `7570090`).
	// It reported §C.4 — "an AlertGroup generation was resolved and the alert joined
	// it" — and there is no generation to resolve, so `AllStages()` is nine. The
	// number is asserted rather than derived from `AllStages()` on purpose: this
	// test is about what a CLIENT is sent over HTTP, and reading the enum the server
	// renders from would agree with the server however wrong both were.
	if len(started.Data.Stages) != 9 {
		t.Fatalf("got %d stages, want the full chain of 9", len(started.Data.Stages))
	}
	if started.Data.Stages[0].Name != "accept" || started.Data.Stages[0].Status != "passed" {
		t.Fatalf("accept = %+v, want passed: the batch is durably on disk before the 202",
			started.Data.Stages[0])
	}

	// ⭐ THE PROVENANCE MARK IS ON THE BATCH, and it is the one thing no payload
	// can set. Reading it from the database rather than from the response is the
	// point: this is what every aggregate ultimately keys off.
	var mode string
	if err := e.pool.QueryRow(e.ctx,
		`SELECT mode FROM ingest_batches WHERE org_id = $1 ORDER BY received_at DESC LIMIT 1`,
		boot.OrgID).Scan(&mode); err != nil {
		t.Fatalf("read the drill's batch: %v", err)
	}
	if mode != "synthetic" {
		t.Fatalf("ingest_batches.mode = %q, want synthetic — without it the drill's alert "+
			"propagates no mark and pollutes every statistic", mode)
	}

	// A second drill for the same source is refused while the first is in flight:
	// one button must not be able to put two cards in a channel.
	if got := e.status(t, http.MethodPost, "/api/v1/drills", boot.Token,
		map[string]any{"source_id": source.String()}); got != http.StatusPreconditionFailed {
		t.Fatalf("a concurrent drill returned %d, want 412", got)
	}

	// The receipt is readable immediately, and its verdict is the same one.
	var polled struct {
		Data struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	e.do(t, http.MethodGet, "/api/v1/drills/"+started.Data.ID, boot.Token, nil, http.StatusOK, &polled)
	if polled.Data.ID != started.Data.ID {
		t.Fatalf("poll returned drill %q, want %q", polled.Data.ID, started.Data.ID)
	}
}

// A running drill may not be disposed of: deleting the rows it is still being
// judged from would make it report failures that never happened.
func TestARunningDrillRefusesDisposal(t *testing.T) {
	e := newEnv(t)
	boot, _, source := bootstrapSource(t, e, "drill-dispose")

	var started struct {
		Data struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	e.do(t, http.MethodPost, "/api/v1/drills", boot.Token,
		map[string]any{"source_id": source.String()}, http.StatusAccepted, &started)

	if started.Data.Status != "running" {
		t.Skipf("the drill settled immediately (%s); disposal is legal", started.Data.Status)
	}
	if got := e.status(t, http.MethodDelete, "/api/v1/drills/"+started.Data.ID, boot.Token, nil); got != http.StatusPreconditionFailed {
		t.Fatalf("disposing of a running drill returned %d, want 412", got)
	}
}

// bootstrapSource is the three-call preamble every test here shares.
func bootstrapSource(t *testing.T, e *env, slug string) (app.BootstrapResult, uuid.UUID, uuid.UUID) {
	t.Helper()

	boot, err := app.Bootstrap(e.ctx, e.pool, app.BootstrapRequest{
		OrgSlug: slug, Email: "ops@" + slug + ".example",
		Password: "correct-horse-battery-staple",
	}, time.Now())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	var cluster struct {
		Data struct {
			ID uuid.UUID `json:"id"`
		} `json:"data"`
	}
	e.do(t, http.MethodPost, "/api/v1/clusters", boot.Token, map[string]any{
		"cluster_key": "prod-eu", "display_name": "Production EU",
	}, http.StatusCreated, &cluster)

	var created struct {
		Data struct {
			Source struct {
				ID uuid.UUID `json:"id"`
			} `json:"source"`
		} `json:"data"`
	}
	e.do(t, http.MethodPost, "/api/v1/sources", boot.Token, map[string]any{
		"name": "am-1", "cluster_id": cluster.Data.ID.String(),
		"kind": "alertmanager", "base_url": "https://alertmanager." + slug + ".example",
	}, http.StatusCreated, &created)

	return boot, cluster.Data.ID, created.Data.Source.ID
}
