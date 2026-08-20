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
// ⚠️ IT WAITS ON THE PIPELINE KINDS BY NAME. The periodic sweeps (`case.reap`,
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
// assertBurstInvariants and none of them reads a clock.
type report struct {
	Case string `json:"case"`
	Note string `json:"note,omitempty"`

	// ---- what was pushed ------------------------------------------------
	Batches       int `json:"batches"`
	AlertsOffered int `json:"alerts_offered"`
	// AlertsAccepted is how many were in a batch oto answered 202 to.
	AlertsAccepted int `json:"alerts_accepted"`
	// Conversations is how many distinct CONVERSATIONS the batch should have
	// produced. ⛔ IT WAS `Groups` (git-bug `7570090`): a conversation was one
	// `alert_groups` generation and a whole batch shared one, and a conversation is
	// now one Case — one per Alert. The number therefore tracks the alert count
	// instead of the batch count, which is not a rename but a different claim.
	Conversations int `json:"conversations"`

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
	OpenCaseRows      int            `json:"open_case_rows"`
	RejectionsByReasn map[string]int `json:"ingest_rejections_by_reason"`

	// ⛔⛔ THE O(groups) BLOCK WAS HERE AND IS DELETED IN FULL (git-bug `7570090`):
	// `RollupPublishes`, `MaxStateVersion`, `AlertsPerLargeGrp` and
	// `RollupsPerAlertPct`. It measured ONE THING — that a 500-alert batch performed
	// a number of rollups proportional to the GROUPS it touched rather than to the
	// alerts — and the fan-in that made that a property is gone. A conversation
	// holds exactly one Case and a Case belongs to one Alert, so 500 alerts are 500
	// conversations by construction and "rollups per alert" is 1 by definition, not
	// by merit. Retargeting the ratio at Cases would have produced a bound that
	// cannot fail, which is the shape of a green gate over nothing.
	//
	// ⚠️ THIS LEAVES THE PACKAGE WITHOUT A FAN-OUT BOUND, AND THAT IS A REAL GAP,
	// not a tidy-up. Whether a per-Case fan-out needs its own ceiling — 500 threads
	// and 500 root cards from one batch is a different cost from 500 rollups on one
	// row, and it is not obviously a smaller one — is a question git-bug `7570090`
	// did not answer. It needs a ruling and a budget before this package can assert
	// on it, and inventing one here would be deciding it by accident.

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

	// ---- the ordering gate ----------------------------------------------
	Threads       int            `json:"threads"`
	ThreadsAtHead int            `json:"threads_with_head_at_end"`
	ThreadStates  map[string]int `json:"thread_states"`
	GapsRecovered int            `json:"delivery_skipped_events"`

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
	// ⭐ THE OPEN CASES ARE THE CONVERSATIONS (git-bug `7570090`). One Case, one
	// thread, always — so this is the count the delivery numbers below have to be
	// read against, exactly as `group_rows` used to be.
	r.OpenCaseRows = e.queryInt(
		`SELECT count(*) FROM alert_cases WHERE org_id = $1 AND state = 'open'`, e.orgID)
	r.Conversations = r.OpenCaseRows
	r.RejectionsByReasn = e.countBy(
		`SELECT reason, count(*) FROM ingest_rejections WHERE org_id = $1 GROUP BY 1`)
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

// ⛔ `rollupPublishes` WAS HERE AND IS DELETED (git-bug `7570090`). It joined
// `alert_groups` to `ui_events{kind=group.upserted}` and keyed the result by the
// generation's TITLE. There is no generation, no title and no group frame, and the
// property it fed is tombstoned on `report` above.

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
	// Conversations is how many CONVERSATIONS should exist — open Cases, one per
	// Alert (git-bug `7570090`).
	//
	// ⛔ IT WAS `Groups`, AND THE NUMBER EVERY CALLER PASSES CHANGES WITH IT: a
	// 500-alert batch used to expect ONE generation and now expects five hundred
	// conversations. A caller left on the old number is asserting that 499
	// conversations were never opened.
	//
	// ⛔ `MaxRollupsPerGroup` WAS THE SECOND FIELD AND IS DELETED. See the
	// tombstone on `report`: the O(groups) bound lost its subject and its
	// replacement is an open question, not a smaller number.
	Conversations int
}

// assertBurstInvariants is the whole hard contract of this package.
//
// ⛔ NOT ONE OF THESE READS A CLOCK. Timings vary by machine and are recorded in
// the report; what must never vary is that nothing was lost, nothing was said
// twice, and nothing wedged.
func assertBurstInvariants(t *testing.T, e *env, r *report, want invariantSpec) {
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

	// 2. ONE CONVERSATION PER ALERT, and every accepted alert has one (git-bug
	//    `7570090`). It was "one group per §C.4 identity, and every alert is a
	//    member of one"; the identity and the membership are both gone, and what is
	//    left is the cardinality the whole stage rests on — a conversation holds
	//    exactly one Case, and an Alert that landed with no open Case has no thread
	//    and therefore reaches nobody.
	if r.OpenCaseRows != want.Conversations {
		t.Errorf("open cases = %d, want %d — a conversation was split, duplicated or never opened",
			r.OpenCaseRows, want.Conversations)
	}
	// One alert opens one episode in this driver, so the two numbers have to agree.
	if r.OpenCaseRows != r.AlertRows {
		t.Errorf("open cases = %d but alerts = %d — an alert landed with no conversation, "+
			"so nothing it did could be delivered anywhere", r.OpenCaseRows, r.AlertRows)
	}

	// 3. ⛔⛔ THE O(groups) PROPERTY WAS ASSERTED HERE AND IS DELETED (git-bug
	//    `7570090`). It was this package's headline claim — one rollup per group
	//    per batch, not one per alert — and the group is what made it a claim. See
	//    the tombstone on `report`: it is a GAP, not a simplification, and the
	//    replacement question (what bounds a per-Case fan-out) has no ruling yet.

	// 4. ⭐ A BURST ADDS NOTHING TO THE CHANNEL. Every fact a burst produces lands
	//    on a thread — as a root card or as a reply under it — and NOTHING is
	//    broadcast out of the thread into the channel.
	//
	//    ⛔ THIS ASSERTION REPLACED "EXACTLY ONE STORM NOTICE PER CHANNEL", AND IT
	//    IS STRICTLY STRONGER. Storm damping used to answer a busy group by going
	//    quiet and then announcing, once per channel, that it had gone quiet — a
	//    latch on `channels.storm_notice_at` whose whole job was to bound the
	//    announcement. ADR 0042 removed the damping, so there is nothing to
	//    announce.
	//
	//    ⭐ AND IT IS STRONGER AGAIN SINCE git-bug `7570090`: `broadcast_reply` is
	//    not a delivery mode any more. Slack thread-broadcast was removed outright
	//    by ruling, `deliveries_mode_ck` admits only `post_root | update_root |
	//    thread_reply`, and `all_resolved` — the one Reason broadcast could still
	//    have been reached by — survives as an ordinary reply in the Case's
	//    conversation. So zero is not merely the correct count for these cases, it
	//    is the only count the schema can now hold, and a non-zero reading here
	//    means oto found a way to write a value the database refuses.
	if r.SlackBroadcasts != 0 {
		t.Errorf("%d broadcast replies escaped into the channel; want 0 — thread-broadcast "+
			"was removed outright (git-bug 7570090) and no burst of any size may surface "+
			"anything beyond the conversation's own root card",
			r.SlackBroadcasts)
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

	// 6. ONE ROOT PER THREAD. A burst that opened five hundred Slack threads for
	//    one incident is the failure this product exists to prevent, and it would
	//    show up here as roots > threads.
	if r.SlackRoots != r.Threads {
		t.Errorf("%d root messages for %d threads — oto opened a conversation it does not "+
			"remember, or forgot one it opened", r.SlackRoots, r.Threads)
	}

	// 7. ⛔⛔ "OTO'S OWN CHATTER IS AN ORDER OF MAGNITUDE BELOW THE ALERT COUNT"
	//    WAS ASSERTED HERE AND IS DELETED (git-bug `7570090`). It read:
	//
	//        if r.AlertsAccepted >= 200 && r.SlackCalls*10 > r.AlertsAccepted { ... }
	//
	//    and its own comment named the mechanism: "a notification is minted per
	//    TRIGGERING CHANGE PER GROUP — one batch arriving at one group is one fact,
	//    whether it carries four alerts or five hundred". GROUPING WAS THE WHOLE
	//    MECHANISM, and the ruling deleted the group. One Case per Alert means one
	//    conversation per alert, so a 500-alert burst sits at roughly 100 % — the
	//    exact number the old comment used to describe "a receiver that posts per
	//    alert".
	//
	//    ⚠️ THIS IS THE LOAD SUITE'S HEADLINE PRODUCT CLAIM AND IT IS NOW UNOWNED.
	//    It is deleted rather than loosened because there is no honest number to
	//    loosen it to: any ratio that today's behaviour satisfies would be a bound
	//    fitted to the measurement, which is the opposite of a budget. What
	//    replaces it — if anything does — needs a ruling on what noise level a
	//    per-Case conversation model is allowed to produce. The RATIO IS STILL
	//    MEASURED AND STILL PUBLISHED in the report (`slack_calls` against
	//    `alerts_accepted`), so the number does not disappear from the record; only
	//    the claim about it does.
	t.Logf("chatter: %d Slack calls for %d accepted alerts (%v) — NOT asserted, see the "+
		"tombstone above", r.SlackCalls, r.AlertsAccepted, r.SlackByMethod)

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

// requireHarnessSanity fails when a periodic sweep closed a CONVERSATION mid-run,
// which would make several of the invariants above measure the wrong world.
//
// ⛔ IT READ `alert_groups.state <> 'open'` (git-bug `7570090`). The conversation
// is the Case, so the row to watch is the episode: nothing in these cases resolves
// and the retention window W is far longer than any of them, so an episode that
// left `open` was closed by something the harness did not ask for — and every
// count above that keys on an open Case is then measuring a smaller world than the
// one the driver pushed.
func requireHarnessSanity(t *testing.T, e *env) {
	t.Helper()
	if closed := e.queryInt(
		`SELECT count(*) FROM alert_cases WHERE org_id = $1 AND state <> 'open'`,
		e.orgID); closed > 0 {
		t.Fatalf("%d conversations closed during the run; nothing in this case resolves and "+
			"the retention window is far longer than it, so something else closed them", closed)
	}
}
