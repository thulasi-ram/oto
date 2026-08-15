package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	alertsdomain "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/app"
	"github.com/thulasiram/oto/internal/ingestion"
	ingestdomain "github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/ingestion/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/test/harness"
)

// ⭐ THIS FILE IS THE INGEST BOUNDS OF SPEC §L.3.2 SEEN FROM OUTSIDE — the status
// code an Alertmanager actually receives, the `errs` code in the problem body, and
// the `ingest_rejections` row that makes the answer defensible.
//
// The decode-layer arithmetic for B2-B16 is pinned in
// `internal/ingestion/decode/bounds_boundary_test.go`, without a database. What
// CANNOT be asserted there, and is asserted here, is the part the product actually
// promises (CONTEXT.md §5b, ADR 0007):
//
//	a bound violation is RECORDED, never fatal to the batch, and never a 4xx.
//
// The only exceptions are B1 (413) and B16 (400), because those two are the only
// ones no retry of the same bytes could ever fix — and BOTH still write a row.
// Everything else answers 202 while an operator can still see, in one table, what
// oto refused to keep.
//
// ⛔ WHY THE 4xx COUNT MATTERS SO MUCH. Alertmanager retries 5xx and ONLY 5xx. A
// 4xx — 429 included — makes it discard the notification permanently and silently,
// during exactly the window when the customer's cluster is on fire. A bound that
// answers 4xx one alert too eagerly does not "reject a payload"; it deletes an
// alert, at the upstream, with no trace anywhere.

// ------------------------------------------------------------------- fixtures

// ingestEnv is `env` plus a configured source and the credential that reaches it.
type ingestEnv struct {
	*env
	orgID    uuid.UUID
	scope    db.TenantScope
	sourceID uuid.UUID
	path     string
	token    string
}

// newIngestEnv boots the real container and walks the real install path to a
// usable ingest endpoint. Nothing is seeded with SQL: the bounds are a contract
// with a customer's Alertmanager, so the endpoint under test has to be the one a
// customer would actually be given.
func newIngestEnv(t *testing.T, slug string) *ingestEnv {
	t.Helper()

	e := newEnv(t)

	boot, err := app.Bootstrap(e.ctx, e.pool, app.BootstrapRequest{
		OrgSlug:     slug,
		OrgName:     slug,
		Email:       "ops@" + slug + ".example",
		DisplayName: "Ops",
		Password:    "correct-horse-battery-staple",
		TokenName:   "bootstrap",
	}, time.Now())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	var cluster struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	e.do(t, http.MethodPost, "/api/v1/clusters", boot.Token, map[string]any{
		"cluster_key": "prod", "display_name": "Production",
	}, http.StatusCreated, &cluster)

	var created struct {
		Data struct {
			Source struct {
				ID         string `json:"id"`
				IngestPath string `json:"ingest_path"`
			} `json:"source"`
			IngestToken string `json:"ingest_token"`
		} `json:"data"`
	}
	e.do(t, http.MethodPost, "/api/v1/sources", boot.Token, map[string]any{
		"name": "am-bounds", "cluster_id": cluster.Data.ID,
		"kind": "alertmanager", "base_url": "https://am.bounds.example",
	}, http.StatusCreated, &created)

	var orgID uuid.UUID
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM orgs WHERE slug = $1`, slug).Scan(&orgID); err != nil {
		t.Fatalf("resolve org: %v", err)
	}

	sourceID, err := uuid.Parse(created.Data.Source.ID)
	if err != nil {
		t.Fatalf("source id: %v", err)
	}

	return &ingestEnv{
		env:      e,
		orgID:    orgID,
		scope:    harness.Scope(t, orgID),
		sourceID: sourceID,
		path:     created.Data.Source.IngestPath,
		token:    created.Data.IngestToken,
	}
}

// acceptedWire is the 202 receipt (`IngestAcceptedDTO`) under httpx's data envelope.
type acceptedWire struct {
	Data struct {
		BatchID         string `json:"batch_id"`
		AlertCount      int    `json:"alert_count"`
		Duplicate       bool   `json:"duplicate"`
		TruncatedAlerts int    `json:"truncated_alerts"`
		RejectedAlerts  int    `json:"rejected_alerts"`
	} `json:"data"`
}

// problemWire is the RFC 9457 body every refusal takes (§L.2.2).
type problemWire struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
}

// postRaw fires BYTES at the ingest endpoint. It cannot go through `env.do`,
// which marshals a Go value — B1 and B16 are about the bytes on the wire, and a
// re-encoded body is not the body under test.
func (e *ingestEnv) postRaw(t *testing.T, body []byte) (int, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(e.ctx, http.MethodPost, e.server.URL+e.path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.token)

	resp, err := e.server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", e.path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}

// accept posts body and requires a 202, returning the receipt.
func (e *ingestEnv) accept(t *testing.T, body []byte) acceptedWire {
	t.Helper()

	status, raw := e.postRaw(t, body)
	if status != http.StatusAccepted {
		t.Fatalf("POST ingest → %d, want 202: %s", status, raw)
	}
	var out acceptedWire
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode receipt: %v (%s)", err, raw)
	}
	return out
}

// refuse posts body, requires the given status, and returns the problem's code.
func (e *ingestEnv) refuse(t *testing.T, body []byte, wantStatus int) string {
	t.Helper()

	status, raw := e.postRaw(t, body)
	if status != wantStatus {
		t.Fatalf("POST ingest → %d, want %d: %s", status, wantStatus, raw)
	}
	var p problemWire
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode problem: %v (%s)", err, raw)
	}
	if p.Status != wantStatus {
		t.Errorf("problem.status = %d, want %d", p.Status, wantStatus)
	}
	return p.Code
}

// reasonsFor counts `ingest_rejections.reason` for one batch. This table is the
// ONLY place a rejected alert survives (§C.9.1), so it is what every case here
// ultimately asserts.
func (e *ingestEnv) reasonsFor(t *testing.T, batchID string) map[string]int {
	t.Helper()
	return e.countReasons(t, `SELECT reason, count(*) FROM ingest_rejections
	                           WHERE org_id = $1 AND batch_id = $2 GROUP BY reason`, e.orgID, batchID)
}

// reasonsWithoutBatch counts the rejections that have NO batch row — B1, B16 and
// an unknown source, where oto refused before anything was durably recorded.
// `ingest_rejections.batch_id` is nullable for exactly this case.
func (e *ingestEnv) reasonsWithoutBatch(t *testing.T) map[string]int {
	t.Helper()
	return e.countReasons(t, `SELECT reason, count(*) FROM ingest_rejections
	                           WHERE org_id = $1 AND source_id = $2 AND batch_id IS NULL
	                           GROUP BY reason`, e.orgID, e.sourceID)
}

func (e *ingestEnv) countReasons(t *testing.T, sql string, args ...any) map[string]int {
	t.Helper()

	rows, err := e.pool.Query(e.ctx, sql, args...)
	if err != nil {
		t.Fatalf("read rejections: %v", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var reason string
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			t.Fatalf("scan rejection: %v", err)
		}
		out[reason] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read rejections: %v", err)
	}
	return out
}

// batchFacts reads the durable row behind a receipt.
func (e *ingestEnv) batchFacts(t *testing.T, batchID string) (time.Time, string, int) {
	t.Helper()

	var (
		receivedAt time.Time
		status     string
		alertCount int
	)
	err := e.pool.QueryRow(e.ctx,
		`SELECT received_at, status, alert_count FROM ingest_batches WHERE org_id = $1 AND id = $2`,
		e.orgID, batchID).Scan(&receivedAt, &status, &alertCount)
	if err != nil {
		t.Fatalf("read batch: %v", err)
	}
	return receivedAt, status, alertCount
}

// mustJSON encodes a payload or fails.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return raw
}

// envelopeOf builds a v4 webhook body around alerts.
//
// ⚠️ `groupKey` MUST BE UNIQUE PER CALL. `batch_dedup_key` (§C.5) is computed from
// the source, the groupKey, the receiver, the notification reason and the alert
// fingerprints — NOT from the bytes — so two payloads differing only in padding
// would be collapsed by `ingest_dedup` and the second would answer 202 with the
// FIRST batch's id, silently invalidating whatever the case meant to assert.
func envelopeOf(groupKey string, alerts []map[string]any) map[string]any {
	return map[string]any{
		"version":         "4",
		"groupKey":        groupKey,
		"truncatedAlerts": 0,
		"status":          "firing",
		"receiver":        "oto",
		"groupLabels":     map[string]string{"alertname": "Bounds"},
		"alerts":          alerts,
	}
}

// wireAlertOf is one alert of the envelope, at a startsAt inside the B12 window.
func wireAlertOf(labels, annotations map[string]string) map[string]any {
	return map[string]any{
		"status":      "firing",
		"labels":      labels,
		"annotations": annotations,
		"startsAt":    time.Now().UTC().Format(time.RFC3339Nano),
		"endsAt":      "0001-01-01T00:00:00Z",
	}
}

// bulkAlerts is n distinct, entirely legal alerts.
func bulkAlerts(n int) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := range n {
		out = append(out, wireAlertOf(map[string]string{
			"alertname": "Bulk",
			"idx":       fmt.Sprintf("%05d", i),
		}, nil))
	}
	return out
}

// ------------------------------------------------------------------------ B1

// TestIngestB1BodySizeBound is the 8 MiB cap, and it is one of only two bounds in
// §L.3.2 permitted to answer a 4xx.
//
// The permission is narrow and conditional: the same bytes could never succeed on
// a retry, so no retry is being denied — AND the row goes to `ingest_rejections`
// first, because a permanent refusal with no trace is exactly the silent drop
// §C.9.1 forbids.
//
// The bound is INCLUSIVE. A body of exactly 8 388 608 bytes is accepted, and it
// has to be: `ingest_batches_bytes_ck` is `body_bytes <= 8388608`, so a transport
// that refused the on-the-nose body would refuse a payload the schema is built to
// hold.
func TestIngestB1BodySizeBound(t *testing.T) {
	e := newIngestEnv(t, "b1")

	// Padding is trailing WHITESPACE, which is not a JSON token: it changes the
	// byte count B1 measures without touching the depth B16 measures or the shape
	// the decoder sees.
	padTo := func(body []byte, size int64) []byte {
		pad := size - int64(len(body))
		if pad < 0 {
			t.Fatalf("the base payload is already %d bytes, over the %d target", len(body), size)
		}
		return append(body, bytes.Repeat([]byte(" "), int(pad))...)
	}

	t.Run("exactly on the bound is accepted", func(t *testing.T) {
		body := padTo(mustJSON(t, envelopeOf("b1-on", []map[string]any{
			wireAlertOf(map[string]string{"alertname": "OnTheBound"}, nil),
		})), ingestdomain.MaxBodyBytes)
		if int64(len(body)) != ingestdomain.MaxBodyBytes {
			t.Fatalf("fixture is %d bytes, want exactly %d", len(body), ingestdomain.MaxBodyBytes)
		}

		got := e.accept(t, body)

		if got.Data.AlertCount != 1 {
			t.Errorf("alert_count = %d, want 1", got.Data.AlertCount)
		}
		if n := len(e.reasonsWithoutBatch(t)); n != 0 {
			t.Errorf("a body ON the bound wrote %d rejection kinds; it is legal and must write none", n)
		}
	})

	t.Run("one byte over the bound is a RECORDED 413", func(t *testing.T) {
		body := padTo(mustJSON(t, envelopeOf("b1-over", []map[string]any{
			wireAlertOf(map[string]string{"alertname": "OverTheBound"}, nil),
		})), ingestdomain.MaxBodyBytes+1)

		code := e.refuse(t, body, http.StatusRequestEntityTooLarge)

		if code != service.CodeBodyTooLarge {
			t.Errorf("problem code = %q, want %q", code, service.CodeBodyTooLarge)
		}

		// ⭐ THE ROW IS WHAT MAKES THE 413 DEFENSIBLE. The oversized body itself is
		// not stored — that is the point of the bound — so the evidence carries the
		// size and nothing else, and `batch_id` is NULL because no batch exists.
		got := e.reasonsWithoutBatch(t)
		want := map[string]int{ingestdomain.ReasonBodyTooLarge.String(): 1}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("rejections = %v, want %v", got, want)
		}
	})
}

// ------------------------------------------------------------------------ B2

// TestIngestB2AlertsPerBatchBound is the one bound whose over-the-line verdict is
// TRUNCATE, and it is the clearest illustration of the governing rule.
//
// Ten thousand and one alerts do not make a bad request. They make a batch with
// one alert too many, and answering 4xx would delete ALL TEN THOUSAND AND ONE at
// the upstream. So the batch is truncated to the cap, the excess is counted into
// `ingest_rejections`, and the answer is 202 with `truncated_alerts` and
// `rejected_alerts` telling a human exactly what was lost.
func TestIngestB2AlertsPerBatchBound(t *testing.T) {
	e := newIngestEnv(t, "b2")

	t.Run("exactly on the bound is accepted whole", func(t *testing.T) {
		got := e.accept(t, mustJSON(t, envelopeOf("b2-on", bulkAlerts(ingestdomain.MaxAlertsPerBatch))))

		if got.Data.AlertCount != ingestdomain.MaxAlertsPerBatch {
			t.Errorf("alert_count = %d, want %d", got.Data.AlertCount, ingestdomain.MaxAlertsPerBatch)
		}
		if got.Data.TruncatedAlerts != 0 || got.Data.RejectedAlerts != 0 {
			t.Errorf("truncated=%d rejected=%d; 10 000 is legal and B2 is inclusive",
				got.Data.TruncatedAlerts, got.Data.RejectedAlerts)
		}
		if n := len(e.reasonsFor(t, got.Data.BatchID)); n != 0 {
			t.Errorf("a batch ON the bound wrote %d rejection kinds, want none", n)
		}
	})

	t.Run("one over the bound truncates, records, and is STILL 202", func(t *testing.T) {
		got := e.accept(t, mustJSON(t, envelopeOf("b2-over", bulkAlerts(ingestdomain.MaxAlertsPerBatch+1))))

		if got.Data.AlertCount != ingestdomain.MaxAlertsPerBatch {
			t.Errorf("alert_count = %d, want the batch truncated to %d",
				got.Data.AlertCount, ingestdomain.MaxAlertsPerBatch)
		}
		if got.Data.TruncatedAlerts != 1 {
			t.Errorf("truncated_alerts = %d, want 1", got.Data.TruncatedAlerts)
		}
		if got.Data.RejectedAlerts != 1 {
			t.Errorf("rejected_alerts = %d, want 1 — the truncation IS a recorded rejection",
				got.Data.RejectedAlerts)
		}

		want := map[string]int{ingestdomain.ReasonTooManyAlerts.String(): 1}
		if g := e.reasonsFor(t, got.Data.BatchID); fmt.Sprint(g) != fmt.Sprint(want) {
			t.Errorf("rejections = %v, want %v", g, want)
		}

		// And the row on disk agrees with the receipt: `ingest_batches_count_ck` caps
		// `alert_count` at the same 10 000, so a truncation that failed to happen
		// would be a check violation and a 500 where an alert belongs.
		if _, _, n := e.batchFacts(t, got.Data.BatchID); n != ingestdomain.MaxAlertsPerBatch {
			t.Errorf("ingest_batches.alert_count = %d, want %d", n, ingestdomain.MaxAlertsPerBatch)
		}
	})
}

// ----------------------------------------------------------------------- B16

// TestIngestB16NestingDepthBound is the decoder's nesting ceiling: the second and
// last bound allowed to answer a 4xx, and for the same reason as B1 — a body that
// nests 33 deep will nest 33 deep on every retry.
//
// The nesting is placed INSIDE a real envelope, as an unknown field on a real
// alert, because that is the shape that actually arrives: Alertmanager's custom
// `payload:` template is an unsupported escape hatch that can emit anything, and
// the decoder is deliberately lenient about unknown fields (§L.3.1).
func TestIngestB16NestingDepthBound(t *testing.T) {
	e := newIngestEnv(t, "b16")

	// An envelope whose deepest point is exactly `total`. The alert object sits at
	// depth 3 (envelope → alerts[] → alert), so the extra value carries total-3.
	envelopeNestedTo := func(groupKey string, total int) []byte {
		extra := total - 3
		nested := strings.Repeat(`{"a":`, extra-1) + `{}` + strings.Repeat(`}`, extra-1)
		return []byte(`{"version":"4","groupKey":"` + groupKey + `","status":"firing","receiver":"oto",` +
			`"alerts":[{"status":"firing","labels":{"alertname":"Deep"},"startsAt":"` +
			time.Now().UTC().Format(time.RFC3339) + `","x":` + nested + `}]}`)
	}

	t.Run("exactly on the bound decodes and is accepted", func(t *testing.T) {
		got := e.accept(t, envelopeNestedTo("b16-on", ingestdomain.MaxJSONDepth))

		if got.Data.AlertCount != 1 {
			t.Errorf("alert_count = %d, want 1 — depth 32 is legal and B16 is inclusive",
				got.Data.AlertCount)
		}
		if n := len(e.reasonsWithoutBatch(t)); n != 0 {
			t.Errorf("a body ON the bound wrote %d rejection kinds, want none", n)
		}
	})

	t.Run("one level over the bound is a RECORDED 400", func(t *testing.T) {
		code := e.refuse(t, envelopeNestedTo("b16-over", ingestdomain.MaxJSONDepth+1),
			http.StatusBadRequest)

		if code != service.CodeUndecodable {
			t.Errorf("problem code = %q, want %q", code, service.CodeUndecodable)
		}

		got := e.reasonsWithoutBatch(t)
		want := map[string]int{ingestdomain.ReasonUndecodable.String(): 1}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("rejections = %v, want %v — a 400 with no row is a silent drop", got, want)
		}
	})
}

// ---------------------------------------------------- B3-B13, per alert, 202

// TestIngestPerAlertBoundsRecordAndKeepTheBatchAt202 is the contract of
// CONTEXT.md §5b in one request: twelve alerts, eleven of them tripping a
// different bound, ONE HTTP response, and it is 202.
//
// ⭐ THE POINT IS THE ASYMMETRY. A 10 000-alert storm batch containing one alert
// with a 9 KiB label value has 9 999 alerts that are real, and a customer's
// cluster is on fire while oto decides what to do about the one. So a per-alert
// bound drops (or repairs) THAT ALERT, records why, and the batch proceeds.
//
// It also pins something easy to get wrong: `rejected_alerts` on the 202 is ZERO
// here. Per-alert bounds deliberately do NOT run inside the accept transaction —
// they need the source's inject/ignore labels and build a LabelSet per alert, and
// every millisecond of that is spent inside Alertmanager's retry budget with the
// connection held open — so they run in `ingest.process_batch` against the durable
// payload. The receipt counts only what accept itself recorded.
func TestIngestPerAlertBoundsRecordAndKeepTheBatchAt202(t *testing.T) {
	e := newIngestEnv(t, "peralert")

	over := func(n int) string { return strings.Repeat("v", n) }

	// One alert per bound. Each carries a legal `alertname` unless the case is
	// about `alertname`, so exactly one bound fires per alert.
	tooManyLabels := map[string]string{"alertname": "X"}
	for i := len(tooManyLabels); i <= alertsdomain.MaxLabels; i++ {
		tooManyLabels[fmt.Sprintf("l%d", i)] = "v"
	}

	// B6: five labels whose §C.1 serialisation (name + value + an 8-byte length
	// prefix pair each) exceeds 16 384. No single value can do it — B5 caps one at
	// 4 096 — which is exactly why B6 exists separately.
	overSizedSet := map[string]string{"alertname": "X"}
	for i := range 4 {
		overSizedSet[fmt.Sprintf("pad_%02d", i)] = over(alertsdomain.MaxLabelValueBytes)
	}

	manyAnnotations := map[string]string{}
	for i := range alertsdomain.MaxAnnotations + 1 {
		manyAnnotations[fmt.Sprintf("a%03d", i)] = "v"
	}

	now := time.Now().UTC()
	alerts := []map[string]any{
		// The control: entirely legal, and it must survive everything below it.
		wireAlertOf(map[string]string{"alertname": "Healthy", "severity": "critical"}, nil),

		// --- dropped, one `ingest_rejections` row each --------------------------
		wireAlertOf(tooManyLabels, nil), // B3
		wireAlertOf(map[string]string{"alertname": "X", over(alertsdomain.MaxLabelNameBytes + 1): "v"}, nil),    // B4
		wireAlertOf(map[string]string{"alertname": "X", "pod": over(alertsdomain.MaxLabelValueBytes + 1)}, nil), // B5
		wireAlertOf(overSizedSet, nil),                                                                          // B6
		wireAlertOf(map[string]string{"alertname": "X", "not-a-label": "v"}, nil),                               // B9
		wireAlertOf(map[string]string{"severity": "critical"}, nil),                                             // B10
		wireAlertOf(map[string]string{"alertname": strings.Repeat("A", alertsdomain.MaxAlertNameBytes+1)}, nil), // B11
		{ // B12: a startsAt past `now + 24h`, the one untrusted value that steers
			// partition routing. This alert is DROPPED.
			"status":   "firing",
			"labels":   map[string]string{"alertname": "FromTheFuture"},
			"startsAt": now.Add(48 * time.Hour).Format(time.RFC3339Nano),
			"endsAt":   "0001-01-01T00:00:00Z",
		},

		// --- kept, repaired, and still recorded ---------------------------------
		wireAlertOf(map[string]string{"alertname": "Chatty"}, manyAnnotations), // B7
		wireAlertOf(map[string]string{"alertname": "Verbose"},
			map[string]string{"summary": over(alertsdomain.MaxAnnotationValueBytes + 1)}), // B8
		{ // B13: an endsAt past `now + 365d`. CLAMPED to startsAt and KEPT — §B.3.2
			// says a skewed upstream clock must never cost an alert.
			"status":   "firing",
			"labels":   map[string]string{"alertname": "LongLived"},
			"startsAt": now.Format(time.RFC3339Nano),
			"endsAt":   now.AddDate(0, 0, 400).Format(time.RFC3339Nano),
		},
	}

	got := e.accept(t, mustJSON(t, envelopeOf("per-alert", alerts)))

	if got.Data.AlertCount != len(alerts) {
		t.Fatalf("alert_count = %d, want %d", got.Data.AlertCount, len(alerts))
	}
	if got.Data.RejectedAlerts != 0 {
		t.Errorf("rejected_alerts on the receipt = %d, want 0: per-alert bounds run in "+
			"ingest.process_batch, not inside the accept transaction", got.Data.RejectedAlerts)
	}

	// Now the asynchronous half, driven directly rather than through River so the
	// assertion is deterministic. The worker pool is off in this env.
	receivedAt, status, _ := e.batchFacts(t, got.Data.BatchID)
	if status != string(ingestdomain.StatusPending) {
		t.Fatalf("batch status before processing = %q, want pending", status)
	}

	batchID, err := uuid.Parse(got.Data.BatchID)
	if err != nil {
		t.Fatalf("batch id: %v", err)
	}
	// The alerts module is swapped for a recorder at its ONE narrow port, so that
	// `Observed` is exactly "how many alerts survived the bounds" and not "what the
	// state machine decided to do about them". Everything else — the repositories,
	// the batch row, the rejection writes — is real, on this test's own Postgres.
	res, err := recordingIngestion(t, e, &chunkRecorder{
		pool: e.container.Pools.Ingest, batchID: batchID,
	}).ProcessBatch(e.ctx, e.scope, batchID, receivedAt)
	if err != nil {
		t.Fatalf("ProcessBatch: %v", err)
	}

	// Eight alerts dropped, three repaired-and-kept: eleven recorded rejections,
	// four observations, and not one 4xx anywhere in the story.
	if res.Rejected != 11 {
		t.Errorf("rejected = %d, want 11", res.Rejected)
	}
	if res.Observed != 4 {
		t.Errorf("observed = %d, want 4 (the control plus the three repaired alerts)", res.Observed)
	}
	if res.FinalStat != ingestdomain.StatusProcessed {
		t.Errorf("final status = %q, want processed — one bad alert must never fail the batch",
			res.FinalStat)
	}

	want := map[string]int{
		ingestdomain.ReasonTooManyLabels.String():        1, // B3
		ingestdomain.ReasonLabelNameTooLarge.String():    1, // B4
		ingestdomain.ReasonLabelValueTooLarge.String():   2, // B5 and B11 share this reason
		ingestdomain.ReasonLabelSetTooLarge.String():     1, // B6
		ingestdomain.ReasonInvalidLabelName.String():     1, // B9
		ingestdomain.ReasonMissingAlertname.String():     1, // B10
		ingestdomain.ReasonTimestampOutOfWindow.String(): 2, // B12 dropped, B13 clamped
		ingestdomain.ReasonTooManyAnnotations.String():   1, // B7, alert kept
		ingestdomain.ReasonAnnotationTooLarge.String():   1, // B8, alert kept
	}
	if g := e.reasonsFor(t, got.Data.BatchID); fmt.Sprint(g) != fmt.Sprint(want) {
		t.Errorf("recorded reasons = %v\nwant                = %v", g, want)
	}

	// ⛔ AND `undecodable` APPEARS NOWHERE. It means "the body was not a webhook
	// payload"; this body decoded perfectly and eleven of its elements failed a
	// bound. Recording those as undecodable would send an operator hunting for
	// malformed JSON that does not exist — the exact defect §L.3.2a was written to
	// close.
	if n := e.reasonsFor(t, got.Data.BatchID)[ingestdomain.ReasonUndecodable.String()]; n != 0 {
		t.Errorf("%d rejections degraded to `undecodable`", n)
	}
}

// ----------------------------------------------------------------------- B17

// chunkRecorder stands in for the alerts module at the ONE narrow port ingestion
// has into it (`service.AlertObserver`). It is not a mocked repository — every
// repository below stays real, on a real Postgres — it is the cross-module seam,
// and it is the only vantage point from which "each chunk is its own transaction"
// is observable at all.
type chunkRecorder struct {
	pool    *pgxpool.Pool
	batchID uuid.UUID

	mu     sync.Mutex
	sizes  []int
	txids  []string
	status []string
}

func (r *chunkRecorder) ObserveBatch(ctx context.Context, _ db.TenantScope, obs []alertsdomain.Observation) (int, error) {
	var (
		xid    string
		status string
	)
	// pg_current_xact_id() is stable for the life of one transaction, so a distinct
	// value per call IS the proof that the chunks did not share one.
	err := db.FromContext(ctx, r.pool).QueryRow(ctx,
		`SELECT pg_current_xact_id()::text,
		        (SELECT status FROM ingest_batches WHERE id = $1 LIMIT 1)`,
		r.batchID).Scan(&xid, &status)
	if err != nil {
		return 0, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.sizes = append(r.sizes, len(obs))
	r.txids = append(r.txids, xid)
	r.status = append(r.status, status)
	return len(obs), nil
}

func (r *chunkRecorder) distinctTxIDs() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]struct{}{}
	for _, x := range r.txids {
		seen[x] = struct{}{}
	}
	return len(seen)
}

// TestIngestB17ChunkBoundaries is the processing transaction size, and it is the
// one bound with no rejection reason at all: nothing is dropped, nothing is
// repaired, and the only observable is HOW THE WORK IS SPLIT.
//
// It still has to be asserted, because getting it wrong is not cosmetic. A single
// transaction holding twenty thousand upserts blocks vacuum, bloats WAL, and is
// the one shape that makes a 2 s statement timeout look like an outage — which on
// this path means a 503 storm during an incident.
//
// ⛔ THE CHUNKING IS UNCONDITIONAL, AND THAT IS THE RULE THESE CASES PIN.
// `applyChunks` slices by ChunkSize for every batch, so 500 alerts is one
// transaction, 501 is two and 2 000 is FOUR. ChunkThreshold splits nothing: it is
// the point above which the batch is additionally marked `partial` while its
// chunks run, and both `pending` and `partial` are resumable, so the mark costs no
// batch its resumability.
//
// This used to read the other way round — the code's own doc comment and §L.3.2's
// B17 row both promised "one transaction at or under ChunkThreshold", and only
// these assertions said otherwise. The comment and the SPEC were corrected to the
// behaviour rather than the other way about: a chunk is the size of ONE multi-row
// statement on the ingest pool, and that pool's `statement_timeout` is 2 s
// (§G.10), so a 2 000-row upsert that crosses it would roll back the whole batch
// and retry at exactly the same size until the job's budget ran out.
func TestIngestB17ChunkBoundaries(t *testing.T) {
	e := newIngestEnv(t, "b17")

	for _, tc := range []struct {
		name string
		n    int
		// wantChunks is the expected split of the batch into transactions.
		wantChunks []int
		// wantPartial is whether the batch is marked `partial` before the chunks run,
		// which happens only ABOVE ChunkThreshold.
		wantPartial bool
	}{
		{
			name:       "exactly one chunk",
			n:          ingestdomain.ChunkSize,
			wantChunks: []int{500},
		},
		{
			name:       "one over a chunk boundary spills a chunk of one",
			n:          ingestdomain.ChunkSize + 1,
			wantChunks: []int{500, 1},
		},
		{
			name:       "exactly on the chunking threshold",
			n:          ingestdomain.ChunkThreshold,
			wantChunks: []int{500, 500, 500, 500},
			// Inclusive: 2 000 is NOT "larger than ChunkThreshold", so no partial.
			wantPartial: false,
		},
		{
			name:        "one over the chunking threshold marks the batch partial",
			n:           ingestdomain.ChunkThreshold + 1,
			wantChunks:  []int{500, 500, 500, 500, 1},
			wantPartial: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := e.accept(t, mustJSON(t,
				envelopeOf(fmt.Sprintf("b17-%d", tc.n), bulkAlerts(tc.n))))

			receivedAt, _, _ := e.batchFacts(t, got.Data.BatchID)
			batchID, err := uuid.Parse(got.Data.BatchID)
			if err != nil {
				t.Fatalf("batch id: %v", err)
			}

			rec := &chunkRecorder{pool: e.container.Pools.Ingest, batchID: batchID}
			svc := recordingIngestion(t, e, rec)

			res, err := svc.ProcessBatch(e.ctx, e.scope, batchID, receivedAt)
			if err != nil {
				t.Fatalf("ProcessBatch: %v", err)
			}

			if res.Observed != tc.n {
				t.Errorf("observed = %d, want %d — chunking must not lose an alert", res.Observed, tc.n)
			}
			if fmt.Sprint(rec.sizes) != fmt.Sprint(tc.wantChunks) {
				t.Errorf("chunk sizes = %v, want %v", rec.sizes, tc.wantChunks)
			}
			for _, size := range rec.sizes {
				if size > ingestdomain.ChunkSize {
					t.Fatalf("a chunk of %d observations exceeds B17's %d",
						size, ingestdomain.ChunkSize)
				}
			}
			if n := rec.distinctTxIDs(); n != len(tc.wantChunks) {
				t.Errorf("%d chunks shared %d transactions; B17 gives each chunk its own",
					len(rec.sizes), n)
			}

			// The `partial` marking is what makes a worker that dies mid-chunking
			// RESUMABLE rather than stranded (domain.Status.Resumable).
			wantStatusDuring := string(ingestdomain.StatusPending)
			if tc.wantPartial {
				wantStatusDuring = string(ingestdomain.StatusPartial)
			}
			for i, s := range rec.status {
				if s != wantStatusDuring {
					t.Errorf("batch status during chunk %d = %q, want %q", i, s, wantStatusDuring)
				}
			}

			// Terminal state, and no rejection row anywhere: B17 drops nothing.
			if res.FinalStat != ingestdomain.StatusProcessed {
				t.Errorf("final status = %q, want processed", res.FinalStat)
			}
			if n := len(e.reasonsFor(t, got.Data.BatchID)); n != 0 {
				t.Errorf("B17 wrote %d rejection kinds; it has no rejection reason", n)
			}
		})
	}
}

// recordingIngestion assembles a SECOND ingestion module over the container's own
// pools and job client, with the alerts observer swapped for rec.
//
// Everything under it — every repository, the batch row, the rejection writes, the
// transactions themselves — is the real thing on the real Postgres. Only the
// cross-module port is substituted, which is exactly the seam
// `ingestion.Deps.Alerts` exists to expose (it is legitimately nil before the
// alerts module is wired).
func recordingIngestion(t *testing.T, e *ingestEnv, rec *chunkRecorder) *service.Service {
	t.Helper()

	mod, err := ingestion.New(ingestion.Deps{
		Pools:    e.container.Pools,
		Enqueuer: e.container.Jobs,
		Config:   e.cfg.Ingest,
		Alerts:   rec,
		Clock:    e.container.Clock,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("ingestion module: %v", err)
	}
	return mod.Service
}
