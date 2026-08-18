//go:build load

package load

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------- the driver

// driver is an ALERTMANAGER-SHAPED sender, and the shape is the point.
//
// ⭐ IT RETRIES 5xx AND NEVER RETRIES 4xx, because that is exactly what
// Alertmanager does (ADR 0007): a 503 with a Retry-After is a designed
// backpressure channel the upstream recovers from, and a 4xx makes it discard the
// notification permanently and silently. A driver that retried everything would
// hide the one failure mode this whole package exists to detect — an alert that
// is simply gone.
type driver struct {
	e *env

	mu sync.Mutex
	// latencies is one entry per HTTP ATTEMPT, not per batch: a shed 503 answered
	// in 2 ms is a real measurement of the accept path and averaging it away would
	// flatter it.
	latencies []time.Duration
	statuses  map[int]int
	// shed counts 503 answers, retried counts attempts made because of one.
	shed, retried int
	// retryAfterAsked is the histogram of `Retry-After` values oto actually sent,
	// in seconds, keyed by the value.
	//
	// ⭐ IT IS RECORDED BECAUSE IT IS THE THING THAT CAUGHT THE FIRST BUG HERE. The
	// driver's own budget was shorter than the backoff oto asked for, so the driver
	// reported "alerts lost" when what had actually happened was an upstream
	// ignoring the deadline it was handed. What oto ASKS FOR and what the upstream
	// WILL WAIT are two different numbers and both belong in the record.
	retryAfterAsked map[int]int
	// retryWaited is the total time the driver actually spent honouring those asks.
	retryWaited time.Duration
	// retryAfterCapped counts the times oto asked for longer than maxRetryWait, so
	// the record shows where the driver stopped being a faithful Alertmanager.
	retryAfterCapped int
	// offered is how many alerts were handed to oto; accepted is how many of them
	// were in a batch that eventually answered 202.
	offered, accepted int
	// refused names the batches oto permanently refused, with the status and body.
	// ⛔ THIS SLICE MUST BE EMPTY. A non-empty entry IS a lost alert.
	refused []string
	batches int
}

func newDriver(e *env) *driver {
	return &driver{e: e, statuses: map[int]int{}, retryAfterAsked: map[int]int{}}
}

// The retry budget, and it is NOT a free parameter.
//
// ⭐ IT MUST BE LONGER THAN THE `Retry-After` OTO SENDS, or the driver becomes the
// thing that loses the alerts. The first run of the sustained case reported two
// permanently-refused batches; oto had answered every one of them
// `503 … retry_after_seconds: 10` and the driver had given up after six attempts
// 250 ms apart — 1.5 s against a 10 s ask. That is not oto shedding an alert into
// oblivion, it is an upstream ignoring the deadline it was given, and a load case
// that models the upstream badly reports a defect that does not exist.
//
// Alertmanager's real budget is `max(group_interval,10s) + peer_position×15s`,
// roughly five minutes. Twenty attempts at up to two seconds is forty seconds:
// far shorter than Alertmanager's, comfortably longer than oto's Retry-After, and
// bounded enough that a genuine wedge still ends the test rather than the timeout.
const (
	maxRetries   = 20
	maxRetryWait = 2 * time.Second
	minRetryWait = 250 * time.Millisecond
)

// post sends one batch the way Alertmanager would, and reports whether oto ever
// took responsibility for it.
func (d *driver) post(spec batchSpec) {
	body := spec.body()

	for attempt := 1; attempt <= maxRetries; attempt++ {
		status, retryAfter, payload := d.attempt(body)

		switch {
		case status == http.StatusAccepted:
			d.record(spec.Alerts, true, "")
			return

		case status >= 500:
			d.mu.Lock()
			d.shed++
			// ⭐ RECORD WHAT OTO ASKED FOR, before deciding what to do about it.
			if secs := int(retryAfter / time.Second); secs > 0 {
				d.retryAfterAsked[secs]++
			}
			d.mu.Unlock()
			if attempt == maxRetries {
				d.record(spec.Alerts, false,
					fmt.Sprintf("%d after %d attempts (oto asked for %s): %s",
						status, attempt, retryAfter, payload))
				return
			}
			// Honour Retry-After the way Alertmanager does, capped so the whole
			// budget fits in a test rather than in an incident.
			//
			// ⚠️ THE CAP IS THE ONE PLACE THIS DRIVER IS NOT A FAITHFUL ALERTMANAGER,
			// so it is counted rather than hidden. Alertmanager would wait the full
			// ask; this driver waits at most maxRetryWait and simply tries again, and
			// because maxRetries × maxRetryWait (40 s) comfortably exceeds any single
			// ask oto makes, the batch is still recovered — the shape is preserved
			// even though the sleep is shortened.
			wait := retryAfter
			if wait <= 0 {
				wait = minRetryWait
			}
			if wait > maxRetryWait {
				wait = maxRetryWait
				d.mu.Lock()
				d.retryAfterCapped++
				d.mu.Unlock()
			}
			d.mu.Lock()
			d.retried++
			d.retryWaited += wait
			d.mu.Unlock()
			time.Sleep(wait)

		default:
			// ⛔ A 4xx IS TERMINAL AND THE ALERTS ARE GONE. Recording it rather than
			// retrying is what makes the loss visible.
			d.record(spec.Alerts, false, fmt.Sprintf("%d (terminal): %s", status, payload))
			return
		}
	}
}

func (d *driver) attempt(body []byte) (status int, retryAfter time.Duration, payload string) {
	req, err := http.NewRequestWithContext(d.e.ctx, http.MethodPost,
		d.e.srv.URL+"/api/v1/ingest/alertmanager/"+d.e.sourceID.String(), bytes.NewReader(body))
	if err != nil {
		d.e.t.Fatalf("load: request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.e.token)

	started := time.Now()
	resp, err := d.e.client.Do(req)
	elapsed := time.Since(started)
	if err != nil {
		// A transport failure is not an oto answer, but it IS a batch that did not
		// land. It is recorded as a 0 so it cannot be mistaken for a success.
		d.observe(elapsed, 0)
		return 0, 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()

	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	d.observe(elapsed, resp.StatusCode)

	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
		retryAfter = time.Duration(secs) * time.Second
	}
	return resp.StatusCode, retryAfter, string(buf[:n])
}

func (d *driver) observe(elapsed time.Duration, status int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.latencies = append(d.latencies, elapsed)
	d.statuses[status]++
}

func (d *driver) record(alerts int, accepted bool, why string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.batches++
	d.offered += alerts
	if accepted {
		d.accepted += alerts
		return
	}
	d.refused = append(d.refused, why)
}

// wave posts every spec concurrently and returns when all have settled. It is how
// the shedder is reached: a burst is many accepts contending for the SAME small
// ingest pool, which is the only condition under which oto sheds.
func (d *driver) wave(specs []batchSpec) {
	var wg sync.WaitGroup
	for _, s := range specs {
		wg.Add(1)
		go func(s batchSpec) {
			defer wg.Done()
			d.post(s)
		}(s)
	}
	wg.Wait()
}

// ------------------------------------------------------------------- draining

// drain waits until the pipeline has genuinely stopped: every batch terminal,
// every pipeline job gone, every delivery settled.
//
// ⚠️ IT WAITS ON THE PIPELINE KINDS BY NAME. The periodic sweeps (`group.close`,
// `stats.rollup`, `partitions.manage`) are always scheduled for a future tick, so
// "no jobs at all" is a condition that can never be true and a drain written that
// way would hang until its deadline every single time.
var pipelineKinds = []string{
	"ingest.process_batch", "enrich.run", "notify.evaluate", "deliver.dispatch",
}

func (e *env) drain(deadline time.Duration) {
	e.t.Helper()
	stop := time.Now().Add(deadline)

	for {
		pendingBatches := e.queryInt(
			`SELECT count(*) FROM ingest_batches
			  WHERE org_id = $1 AND status IN ('pending','partial')`, e.orgID)
		liveJobs := e.queryInt(
			`SELECT count(*) FROM river_job
			  WHERE kind = ANY($1)
			    AND state IN ('available','running','retryable','scheduled','pending')`,
			pipelineKinds)
		liveDeliveries := e.queryInt(
			`SELECT count(*) FROM notification_deliveries
			  WHERE org_id = $1 AND status IN ('pending','sending')`, e.orgID)

		if pendingBatches == 0 && liveJobs == 0 && liveDeliveries == 0 {
			return
		}
		if time.Now().After(stop) {
			e.t.Fatalf("load: the pipeline did not drain in %s — "+
				"%d batches still pending/partial, %d pipeline jobs live, %d deliveries pending/sending. "+
				"THIS IS A WEDGE: something is queued that nothing will ever complete.",
				deadline, pendingBatches, liveJobs, liveDeliveries)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// --------------------------------------------------------------- measurement

// report is everything one load case measured. It is JSON so a run can be diffed
// against the checked-in numbers in RESULTS.md.
//
// ⛔ NOTHING IN HERE IS A CONTRACT. Every duration depends on the machine, the
// container runtime and whatever else is running; the assertions live in
// assertStormInvariants and none of them reads a clock.
type report struct {
	Case string `json:"case"`
	Note string `json:"note,omitempty"`

	// ---- what was pushed ------------------------------------------------
	Batches       int `json:"batches"`
	AlertsOffered int `json:"alerts_offered"`
	// AlertsAccepted is how many were in a batch oto answered 202 to.
	AlertsAccepted int `json:"alerts_accepted"`
	Groups         int `json:"groups"`

	// ---- the accept path ------------------------------------------------
	StatusCodes map[string]int `json:"status_codes"`
	Shed503     int            `json:"shed_503"`
	Retries     int            `json:"retries_after_503"`
	Refused     []string       `json:"permanently_refused"`

	// ---- what oto ASKED the upstream to wait -----------------------------
	// RetryAfterAsked is the histogram of `Retry-After` seconds oto sent with its
	// 503s, keyed by the number of seconds. RetryAfterCapped counts how often this
	// driver waited less than it was told to, and RetryBudgetS is the driver's own
	// worst-case budget — which MUST exceed the largest ask, or the driver becomes
	// the thing that loses the alerts.
	RetryAfterAsked  map[string]int `json:"retry_after_seconds_asked_by_oto"`
	RetryAfterMaxS   int            `json:"retry_after_max_seconds_asked"`
	RetryAfterCapped int            `json:"retry_after_capped_by_driver"`
	RetryWaitedS     float64        `json:"retry_seconds_waited_by_driver"`
	RetryBudgetS     float64        `json:"driver_retry_budget_seconds"`
	IngestP50MS      float64        `json:"ingest_p50_ms"`
	IngestP90MS      float64        `json:"ingest_p90_ms"`
	IngestP99MS      float64        `json:"ingest_p99_ms"`
	IngestMaxMS      float64        `json:"ingest_max_ms"`
	PushSeconds      float64        `json:"push_seconds"`
	DrainSeconds     float64        `json:"drain_seconds"`

	// ---- what landed ----------------------------------------------------
	BatchStatus       map[string]int `json:"ingest_batch_status"`
	AlertRows         int            `json:"alert_rows"`
	CaseRows          int            `json:"case_rows"`
	GroupMemberRows   int            `json:"group_member_rows"`
	GroupRows         int            `json:"group_rows"`
	RejectionsByReasn map[string]int `json:"ingest_rejections_by_reason"`

	// ---- the O(groups) property ----------------------------------------
	// RollupPublishes is `ui_events{kind=group.upserted}` per group: one per
	// generation opened, one per MATERIAL rollup, one per storm transition. Under
	// the defect this issue is adjacent to it was one PER ALERT.
	RollupPublishes    map[string]int `json:"rollup_publishes_per_group"`
	MaxStateVersion    int            `json:"max_group_state_version"`
	AlertsPerLargeGrp  int            `json:"alerts_in_largest_group"`
	RollupsPerAlertPct float64        `json:"rollups_per_alert_pct"`

	// ---- notification and delivery --------------------------------------
	NotificationsByReason map[string]int `json:"notifications_by_reason"`
	NotificationsSuppress map[string]int `json:"notifications_suppressed_by_reason"`
	DeliveriesByStatus    map[string]int `json:"deliveries_by_status"`
	DeliveriesByMode      map[string]int `json:"deliveries_by_mode"`
	DeliveryAttemptsSum   int            `json:"delivery_attempts_sum"`

	// ---- Slack, MEASURED not estimated ----------------------------------
	SlackCalls       int            `json:"slack_calls_total"`
	SlackByMethod    map[string]int `json:"slack_ok_calls_by_method"`
	SlackErrors      map[string]int `json:"slack_error_calls_by_code"`
	SlackRoots       int            `json:"slack_root_posts"`
	SlackThreadPosts int            `json:"slack_thread_replies"`
	SlackBroadcasts  int            `json:"slack_broadcast_replies"`
	StormNotices     int            `json:"storm_notices_in_channel"`

	// ---- the ordering gate ----------------------------------------------
	Threads         int            `json:"threads"`
	ThreadsAtHead   int            `json:"threads_with_head_at_end"`
	ThreadStates    map[string]int `json:"thread_states"`
	GapsRecovered   int            `json:"delivery_skipped_events"`
	StormModeGroups int            `json:"groups_in_storm_mode"`

	// ---- jobs -----------------------------------------------------------
	JobsByKind map[string]int `json:"jobs_by_kind"`
}

// measure reads everything the report needs out of the database and the Slack
// double. It runs AFTER drain, so nothing it sees is in flight.
func (e *env) measure(name string, d *driver, push, drainFor time.Duration) *report {
	e.t.Helper()

	r := &report{
		Case:           name,
		Batches:        d.batches,
		AlertsOffered:  d.offered,
		AlertsAccepted: d.accepted,
		Shed503:        d.shed,
		Retries:        d.retried,
		Refused:        d.refused,
		PushSeconds:    push.Seconds(),
		DrainSeconds:   drainFor.Seconds(),
		StatusCodes:    map[string]int{},
	}
	if r.Refused == nil {
		r.Refused = []string{}
	}
	for code, n := range d.statuses {
		r.StatusCodes[strconv.Itoa(code)] = n
	}

	r.RetryAfterAsked = map[string]int{}
	for secs, n := range d.retryAfterAsked {
		r.RetryAfterAsked[strconv.Itoa(secs)] = n
		if secs > r.RetryAfterMaxS {
			r.RetryAfterMaxS = secs
		}
	}
	r.RetryAfterCapped = d.retryAfterCapped
	r.RetryWaitedS = d.retryWaited.Seconds()
	r.RetryBudgetS = (maxRetries - 1) * maxRetryWait.Seconds()

	p := percentiles(d.latencies)
	r.IngestP50MS, r.IngestP90MS, r.IngestP99MS, r.IngestMaxMS = p[0], p[1], p[2], p[3]

	r.BatchStatus = e.countBy(`SELECT status, count(*) FROM ingest_batches WHERE org_id = $1 GROUP BY 1`)
	r.AlertRows = e.queryInt(`SELECT count(*) FROM alerts WHERE org_id = $1`, e.orgID)
	r.CaseRows = e.queryInt(`SELECT count(*) FROM alert_cases WHERE org_id = $1`, e.orgID)
	r.GroupMemberRows = e.queryInt(
		`SELECT count(*) FROM alert_cases WHERE org_id = $1 AND group_id IS NOT NULL`, e.orgID)
	r.GroupRows = e.queryInt(`SELECT count(*) FROM alert_groups WHERE org_id = $1`, e.orgID)
	r.Groups = r.GroupRows
	r.RejectionsByReasn = e.countBy(
		`SELECT reason, count(*) FROM ingest_rejections WHERE org_id = $1 GROUP BY 1`)

	r.RollupPublishes = e.rollupPublishes()
	r.MaxStateVersion = e.queryInt(
		`SELECT coalesce(max(state_version), 0) FROM alert_groups WHERE org_id = $1`, e.orgID)
	r.AlertsPerLargeGrp = e.queryInt(
		`SELECT coalesce(max(n), 0) FROM (
		   SELECT count(*) AS n FROM alert_cases
		    WHERE org_id = $1 AND group_id IS NOT NULL GROUP BY group_id
		 ) t`, e.orgID)
	if r.AlertsPerLargeGrp > 0 {
		worst := 0
		for _, n := range r.RollupPublishes {
			if n > worst {
				worst = n
			}
		}
		r.RollupsPerAlertPct = 100 * float64(worst) / float64(r.AlertsPerLargeGrp)
	}

	r.NotificationsByReason = e.countBy(
		`SELECT reason, count(*) FROM notifications WHERE org_id = $1 GROUP BY 1`)
	r.NotificationsSuppress = e.countBy(
		`SELECT suppressed_reason, count(*) FROM notifications
		  WHERE org_id = $1 AND suppressed_reason IS NOT NULL GROUP BY 1`)
	r.DeliveriesByStatus = e.countBy(
		`SELECT status, count(*) FROM notification_deliveries WHERE org_id = $1 GROUP BY 1`)
	r.DeliveriesByMode = e.countBy(
		`SELECT mode, count(*) FROM notification_deliveries WHERE org_id = $1 GROUP BY 1`)
	r.DeliveryAttemptsSum = e.queryInt(
		`SELECT coalesce(sum(attempts), 0) FROM notification_deliveries WHERE org_id = $1`, e.orgID)

	r.Threads = e.queryInt(`SELECT count(*) FROM channel_threads WHERE org_id = $1`, e.orgID)
	r.ThreadsAtHead = e.queryInt(
		`SELECT count(*) FROM channel_threads WHERE org_id = $1 AND last_sent_seq = next_seq - 1`, e.orgID)
	r.ThreadStates = e.countBy(`SELECT state, count(*) FROM channel_threads WHERE org_id = $1 GROUP BY 1`)
	r.GapsRecovered = e.queryInt(
		`SELECT count(*) FROM alert_events WHERE org_id = $1 AND type = 'delivery.skipped'`, e.orgID)
	r.StormModeGroups = e.queryInt(
		`SELECT count(*) FROM alert_groups WHERE org_id = $1 AND storm_mode`, e.orgID)

	r.JobsByKind = e.jobsByKind()
	e.readSlack(r)
	return r
}

// readSlack turns the conformance double's call log into the numbers the issue
// asks for. ⭐ THESE ARE COUNTED CALLS, NOT ESTIMATES.
func (e *env) readSlack(r *report) {
	calls := e.slack.Calls()
	r.SlackCalls = len(calls)
	r.SlackByMethod = map[string]int{}
	r.SlackErrors = map[string]int{}

	for _, c := range calls {
		switch {
		case c.Status == http.StatusTooManyRequests:
			r.SlackErrors["http_429"]++
			continue
		case !c.OK:
			r.SlackErrors[c.Error]++
			continue
		}
		r.SlackByMethod[c.Method]++
		if c.Method != "chat.postMessage" {
			continue
		}
		switch {
		case c.ThreadTS == "":
			r.SlackRoots++
		case c.ReplyBroadcast:
			r.SlackBroadcasts++
		default:
			r.SlackThreadPosts++
		}
	}
	// The storm notice IS the broadcast reply: ADR 0020 permits exactly one
	// broadcast while a storm is on and it is the storm announcement itself.
	r.StormNotices = r.SlackBroadcasts
}

func (e *env) countBy(sql string) map[string]int {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, sql, e.orgID)
	if err != nil {
		e.t.Fatalf("load: count: %v\nSQL: %s", err, sql)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			e.t.Fatalf("load: scan count: %v", err)
		}
		out[key] = n
	}
	return out
}

// rollupPublishes counts `group.upserted` per group. It is keyed by the group's
// TITLE rather than its uuid so the checked-in results stay readable across runs.
func (e *env) rollupPublishes() map[string]int {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx,
		`SELECT g.title, count(u.seq)
		   FROM alert_groups g
		   LEFT JOIN ui_events u
		     ON u.org_id = g.org_id AND u.kind = 'group.upserted' AND u.resource_id = g.id
		  WHERE g.org_id = $1
		  GROUP BY g.id, g.title`, e.orgID)
	if err != nil {
		e.t.Fatalf("load: rollup publishes: %v", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var title string
		var n int
		if err := rows.Scan(&title, &n); err != nil {
			e.t.Fatalf("load: scan rollup publishes: %v", err)
		}
		out[title] = n
	}
	return out
}

func (e *env) jobsByKind() map[string]int {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx,
		`SELECT kind, count(*) FROM river_job WHERE kind = ANY($1) GROUP BY 1`, pipelineKinds)
	if err != nil {
		e.t.Fatalf("load: jobs by kind: %v", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			e.t.Fatalf("load: scan jobs: %v", err)
		}
		out[kind] = n
	}
	return out
}

func percentiles(in []time.Duration) [4]float64 {
	if len(in) == 0 {
		return [4]float64{}
	}
	sorted := append([]time.Duration(nil), in...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	at := func(q float64) float64 {
		i := int(q * float64(len(sorted)-1))
		return float64(sorted[i].Microseconds()) / 1000
	}
	return [4]float64{at(0.50), at(0.90), at(0.99), at(1.0)}
}

// publish logs the report and, when OTO_LOAD_RESULTS names a file, appends it
// there so a run can be checked in.
func (r *report) publish(t *testing.T) {
	t.Helper()
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatalf("load: encode report: %v", err)
	}
	t.Logf("\n--- LOAD RESULT %s ---\n%s\n--- END LOAD RESULT ---", r.Case, raw)

	path := os.Getenv("OTO_LOAD_RESULTS")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("load: open results file: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		t.Fatalf("load: write results: %v", err)
	}
}

// ------------------------------------------------------------- the invariants

// invariantSpec is what THIS case expected of the world, so the shared assertions
// can be exact rather than approximate.
type invariantSpec struct {
	// DistinctAlerts is how many distinct alert identities were pushed. Every one
	// of them must be an `alerts` row or an `ingest_rejections` row.
	DistinctAlerts int
	// Groups is how many distinct §C.4 generations should exist.
	Groups int
	// MaxRollupsPerGroup is the tight bound on `group.upserted` per group: one for
	// the generation opening, one per MATERIAL rollup, one per storm transition,
	// plus slack. Blowing it means the rollup went back to being O(alerts).
	MaxRollupsPerGroup int
	// StormGroups is how many generations must have entered storm mode, and
	// therefore how many `storm` notifications must exist — one per thread, so the
	// record is complete on every group.
	StormGroups int
	// RequireChannelBroadcast demands that the channel-level notice actually
	// surfaced in-channel, which is only reachable when the storm arrives at a
	// group whose root card has ALREADY landed. See the note on the storm
	// assertion for why the single-batch cases cannot require it.
	RequireChannelBroadcast bool
}

// assertStormInvariants is the whole hard contract of this package.
//
// ⛔ NOT ONE OF THESE READS A CLOCK. Timings vary by machine and are recorded in
// the report; what must never vary is that nothing was lost, nothing was said
// twice, and nothing wedged.
func assertStormInvariants(t *testing.T, e *env, r *report, want invariantSpec) {
	t.Helper()

	// 1. NOTHING SILENTLY LOST. Every alert oto took responsibility for is either
	//    an `alerts` row or a recorded rejection, and no batch was permanently
	//    refused. This is the invariant ADR 0007 is about: a refusal Alertmanager
	//    will not retry is an alert that no longer exists anywhere.
	if len(r.Refused) > 0 {
		t.Errorf("ALERTS WERE LOST: %d batches were permanently refused: %v", len(r.Refused), r.Refused)
	}
	if r.AlertsAccepted != r.AlertsOffered {
		t.Errorf("oto accepted %d of %d offered alerts; the difference was never recorded anywhere",
			r.AlertsAccepted, r.AlertsOffered)
	}
	// ⭐ THE DRIVER MUST NOT BE THE THING THAT LOSES THE ALERTS. oto's 503 carries a
	// `Retry-After`; an upstream whose whole budget is shorter than one ask gives up
	// before oto ever said it was ready, and then reports oto as having dropped the
	// batch. That is precisely the false positive the first run of this package
	// produced — and it is Alertmanager's own ~5-minute budget in miniature (ADR
	// 0007). Asserting it here keeps the measurement honest about which side failed.
	if float64(r.RetryAfterMaxS) > r.RetryBudgetS {
		t.Errorf("oto asked the upstream to wait up to %d s but this driver's entire retry "+
			"budget is %.0f s — any loss reported below is the DRIVER giving up early, not oto "+
			"shedding into oblivion; raise maxRetries/maxRetryWait",
			r.RetryAfterMaxS, r.RetryBudgetS)
	}
	perAlertRejections := 0
	for reason, n := range r.RejectionsByReasn {
		if reason == "body_too_large" || reason == "undecodable" || reason == "unknown_source" {
			continue
		}
		perAlertRejections += n
	}
	if got := r.AlertRows + perAlertRejections; got != want.DistinctAlerts {
		t.Errorf("accounting: %d alert rows + %d recorded per-alert rejections = %d, "+
			"want %d distinct alerts pushed — the difference vanished with no trace",
			r.AlertRows, perAlertRejections, got, want.DistinctAlerts)
	}
	if stuck := r.BatchStatus["pending"] + r.BatchStatus["partial"] + r.BatchStatus["failed"]; stuck > 0 {
		t.Errorf("%d ingest batches did not reach `processed` (%v)", stuck, r.BatchStatus)
	}

	// 2. ONE GROUP PER §C.4 IDENTITY, and every accepted alert is a member of one.
	if r.GroupRows != want.Groups {
		t.Errorf("alert_groups = %d, want %d — a group generation was split or duplicated",
			r.GroupRows, want.Groups)
	}
	// Membership is `alert_cases.group_id` since 00051, so this counts
	// grouped EPISODES. One alert opens one episode in this driver, so the two
	// numbers still have to agree.
	if r.GroupMemberRows != r.AlertRows {
		t.Errorf("grouped episodes = %d but alerts = %d — an alert landed with no membership, "+
			"so the card it belongs to under-counts the incident", r.GroupMemberRows, r.AlertRows)
	}

	// 3. ⭐ THE O(groups) PROPERTY, END TO END. `Recompute` is called once per
	//    affected group per batch, so one batch performs ONE rollup and ONE storm
	//    evaluation per group rather than one per alert. Two statements, because one alone is weak: the ratio
	//    catches the regression, the absolute bound catches drift.
	for title, n := range r.RollupPublishes {
		if n > want.MaxRollupsPerGroup {
			t.Errorf("group %q published %d `group.upserted` events, bound is %d — "+
				"the rollup is being recomputed more often than once per batch per group",
				title, n, want.MaxRollupsPerGroup)
		}
	}
	if r.AlertsPerLargeGrp >= 200 && r.RollupsPerAlertPct > 20 {
		t.Errorf("the largest group has %d members and %.1f%% as many rollups — "+
			"that is O(alerts), not O(groups)", r.AlertsPerLargeGrp, r.RollupsPerAlertPct)
	}

	// 4. ⭐ EXACTLY ONE STORM NOTICE PER CHANNEL (ADR 0020), IN TWO HALVES.
	//
	//    Storm mode is decided per GROUP and a channel carries many groups, so
	//    §B.6 has to reconcile two things that pull opposite ways: every group's
	//    thread must RECORD that it went quiet, and the channel must be TOLD only
	//    once. The two halves are asserted separately because they are two
	//    different claims about two different surfaces.
	//
	//    ⚠️ THE IN-CHANNEL FORM IS NOT ALWAYS A BROADCAST, and discovering that is
	//    one of the things this load case bought. When five hundred alerts arrive
	//    in ONE batch, the storm transition and the group's first notification are
	//    minted in the SAME ingest transaction, so the storm evaluation frequently
	//    runs before any root card exists — §H.6 then answers `post_root` and drops
	//    the reply as `fresh_root`, and the channel learns about the storm from the
	//    root card itself, which already says "Storm — N alerts in this group".
	//    That is correct and is not a broadcast. Requiring one unconditionally
	//    would be asserting a race.
	if want.StormGroups > 0 {
		if r.StormModeGroups != want.StormGroups {
			t.Errorf("%d generations are in storm mode, want %d", r.StormModeGroups, want.StormGroups)
		}
		if got := r.NotificationsByReason["storm"]; got != want.StormGroups {
			t.Errorf("%d `storm` notifications recorded for %d storming generations — "+
				"every group's own thread must record that oto went quiet (§B.6)",
				got, want.StormGroups)
		}
		// ⛔ THE FLOOD GUARD, AND THE WHOLE POINT OF THE LATCH. However many groups
		// storm, AT MOST ONE of them may shout in the channel.
		if r.SlackBroadcasts > 1 {
			t.Errorf("%d in-channel storm broadcasts across %d storming groups, want at most 1 — "+
				"the once-per-channel latch on channels.storm_notice_at did not hold, and the "+
				"damper is now producing the flood it exists to prevent",
				r.SlackBroadcasts, want.StormGroups)
		}
		if want.RequireChannelBroadcast && r.SlackBroadcasts != 1 {
			t.Errorf("%d in-channel storm broadcasts, want exactly 1: every group's root card "+
				"had already landed, so the notice had nowhere to hide", r.SlackBroadcasts)
		}
		if want.RequireChannelBroadcast {
			latched := e.queryInt(
				`SELECT count(*) FROM channels WHERE org_id = $1 AND storm_notice_at IS NOT NULL`, e.orgID)
			if latched != 1 {
				t.Errorf("%d channels carry a storm_notice_at latch, want 1", latched)
			}
		}
	}

	// 5. ⭐ NO DELIVERY LOST OR DUPLICATED. The two sides of the wire are compared
	//    directly: one accepted Slack write per `sent` row, and one `sent` row per
	//    accepted Slack write. A message in the channel with no row behind it and a
	//    row claiming a message that was never sent are both counted here.
	//
	//    ⚠️ THE COMPARISON IS DELIBERATELY VERB-AGNOSTIC, and finding out why was
	//    worth the run. `notification_deliveries.mode` records the mode decided at
	//    FAN-OUT; `DispatchService.effectiveMode` re-derives it at CLAIM time
	//    against the thread as it then stands, and the re-derived value is not
	//    written back — so a row that says `post_root` can legitimately have been
	//    sent with `chat.update` because the root landed in between. Splitting this
	//    assertion by verb reports a phantom off-by-one whenever that happens.
	sentDeliveries := e.queryInt(
		`SELECT count(*) FROM notification_deliveries WHERE org_id = $1 AND status = 'sent'`,
		e.orgID)
	writes := r.SlackByMethod["chat.postMessage"] + r.SlackByMethod["chat.update"]
	if writes != sentDeliveries {
		t.Errorf("%d accepted Slack writes (%v) against %d `sent` deliveries — either a "+
			"message exists in the channel with no row behind it, or a row claims a message "+
			"that was never sent", writes, r.SlackByMethod, sentDeliveries)
	}
	if len(r.SlackErrors) > 0 {
		t.Errorf("the conforming Slack double REFUSED calls: %v — oto sent something that "+
			"violates Slack's documented contract", r.SlackErrors)
		// The counts alone name the code and not the payload, and the payload is
		// the only thing that can be fixed. One refused call is dumped in full.
		for _, c := range e.slack.Calls() {
			if c.OK || c.Status == http.StatusTooManyRequests {
				continue
			}
			t.Logf("first refused Slack call: method=%s error=%s\n  text=%q\n  blocks=%s\n  attachments=%s",
				c.Method, c.Error, c.Text, string(c.Blocks), string(c.Attachments))
			break
		}
	}
	if dead := r.DeliveriesByStatus["dead"]; dead > 0 {
		t.Errorf("%d deliveries dead-lettered; nothing in this case should fail permanently", dead)
	}
	if live := r.DeliveriesByStatus["pending"] + r.DeliveriesByStatus["sending"]; live > 0 {
		t.Errorf("%d deliveries are still pending/sending after the drain", live)
	}

	// 6. ONE ROOT PER THREAD. A storm that opened five hundred Slack threads for
	//    one incident is the failure this product exists to prevent, and it would
	//    show up here as roots > threads.
	if r.SlackRoots != r.Threads {
		t.Errorf("%d root messages for %d threads — oto opened a conversation it does not "+
			"remember, or forgot one it opened", r.SlackRoots, r.Threads)
	}

	// 7. ⭐ OTO'S OWN CHATTER IS AN ORDER OF MAGNITUDE BELOW THE ALERT COUNT.
	//    This is the product claim, stated as a ratio rather than as a budget so
	//    that it does not depend on how many batches a machine happened to shed:
	//    a receiver that posts per alert would sit at 100 %, and storm collapse
	//    plus amend-in-place is supposed to put oto below 10 %.
	if r.AlertsAccepted >= 200 && r.SlackCalls*10 > r.AlertsAccepted {
		t.Errorf("%d Slack calls for %d accepted alerts (%v) — that is more than one message "+
			"per ten alerts, which is the noise level oto exists to be below",
			r.SlackCalls, r.AlertsAccepted, r.SlackByMethod)
	}

	// 8. THE ORDERING GATE MADE PROGRESS. `last_sent_seq = next_seq - 1` on every
	//    thread means the head walked to the end of the queue: nothing is waiting
	//    on a predecessor that will never run (§G.7.3).
	if r.ThreadsAtHead != r.Threads {
		t.Errorf("%d of %d threads have their ordering head at the end of the queue — "+
			"the rest are WEDGED", r.ThreadsAtHead, r.Threads)
	}
	if n := r.ThreadStates["dead"]; n > 0 {
		t.Errorf("%d threads died; nothing in this case should kill a thread", n)
	}
}

// requireHarnessSanity fails when a periodic sweep closed a generation mid-run,
// which would make several of the invariants above measure the wrong world.
func requireHarnessSanity(t *testing.T, e *env) {
	t.Helper()
	if closed := e.queryInt(
		`SELECT count(*) FROM alert_groups WHERE org_id = $1 AND state <> 'open'`,
		e.orgID); closed > 0 {
		t.Fatalf("%d group generations closed during the run; the close delay is 20 minutes "+
			"and this case is far shorter, so something else closed them", closed)
	}
}
