package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/ingestion/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// `ingestAlertmanagerWebhook` — POST /api/v1/ingest/alertmanager/{source_id}.
//
// ⭐ THE RESPONSE CONTRACT IS THE SINGLE MOST CONSEQUENTIAL RULE IN oto (§G.2,
// C4, ADR 0007). Alertmanager retries 5xx and ONLY 5xx. A 4xx — 429 included —
// makes it discard the notification permanently and silently, during exactly the
// window when the customer's cluster is on fire. So the shape of the answer is
// not a detail of this handler; it is the product's promise:
//
//	persisted, or a duplicate                202
//	ANYTHING transient                       503 + Retry-After, never 429
//	missing / wrong / wrongly-scoped token   401
//	body over 8 MiB                          413, recorded
//
// Nothing guarded any of it. These tests are written against the CONTRACT — the
// bodies are validated by the schema the OpenAPI document declares, never by a
// shape restated here, because a hand-written expectation is a second copy of the
// contract and a second copy drifts exactly the way the DTOs did.

/* -------------------------------------------------------------------------- */
/* Fakes                                                                      */
/* -------------------------------------------------------------------------- */

// fakeIngest is the IngestService port. It records what it was handed, because
// half the promises here are about what NEVER reaches the service.
type fakeIngest struct {
	mu sync.Mutex

	result service.AcceptResult
	err    error

	calls int
	// gotBody is the exact bytes the handler forwarded. The service hashes them
	// verbatim for the dedup checksum, so a transport that rewrote them would
	// silently break duplicate suppression.
	gotBody []byte
	gotMode domain.Mode
	gotID   uuid.UUID

	// tooLarge counts RecordBodyTooLarge calls: a 413 without one is a permanent
	// refusal with no row in `ingest_rejections` to justify it.
	tooLarge int
	gotBytes int64

	// entered is closed on the first Accept and hold blocks it, which is how the
	// concurrency gate is filled deterministically.
	entered chan struct{}
	hold    chan struct{}
	once    sync.Once
}

func (f *fakeIngest) Accept(_ context.Context, _ db.TenantScope, cmd service.AcceptCommand) (service.AcceptResult, error) {
	f.mu.Lock()
	f.calls++
	f.gotBody = append([]byte(nil), cmd.Body...)
	f.gotMode = cmd.Mode
	f.gotID = cmd.SourceID
	f.mu.Unlock()

	if f.entered != nil {
		f.once.Do(func() { close(f.entered) })
	}
	if f.hold != nil {
		<-f.hold
	}
	if f.err != nil {
		return service.AcceptResult{}, f.err
	}
	return f.result, nil
}

func (f *fakeIngest) RecordBodyTooLarge(_ context.Context, _ db.TenantScope, _ uuid.UUID, n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tooLarge++
	f.gotBytes = n
}

func (f *fakeIngest) accepts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeTokenStore is the TokenAuthenticator port. It never sees a plaintext
// secret — only the sha256 digest — which is the property that keeps an ingest
// credential out of query logs and stack traces.
type fakeTokenStore struct {
	token domain.IngestToken
	err   error

	lookups int
	// gotDigest proves the handler hashed rather than forwarded.
	gotDigest []byte
}

func (f *fakeTokenStore) Lookup(_ context.Context, digest []byte, _ time.Time) (domain.IngestToken, error) {
	f.lookups++
	f.gotDigest = append([]byte(nil), digest...)
	if f.err != nil {
		return domain.IngestToken{}, f.err
	}
	return f.token, nil
}

// fakeDepth is the QueueDepthSampler port.
type fakeDepth struct {
	depth   int
	err     error
	samples int
}

func (f *fakeDepth) Depth(context.Context) (int, error) {
	f.samples++
	if f.err != nil {
		return 0, f.err
	}
	return f.depth, nil
}

/* -------------------------------------------------------------------------- */
/* Harness                                                                    */
/* -------------------------------------------------------------------------- */

const (
	// goodSecret is a well-formed ingest credential. The prefix is load-bearing:
	// anything without it is refused before the database is touched.
	goodSecret = "oto_ingest_" + "0123456789abcdef0123456789abcdef0123456"
	// patSecret is a personal access token. It is a real credential for the UI and
	// must be worthless here.
	patSecret = "oto_pat_" + "0123456789abcdef0123456789abcdef0123456"
)

var (
	testSourceID = uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	otherSource  = uuid.MustParse("6ba7b811-9dad-11d1-80b4-00c04fd430c8")
)

type ingestRig struct {
	client *apitest.Client
	svc    *fakeIngest
	tokens *fakeTokenStore
	depth  *fakeDepth
}

type rigOptions struct {
	shed  ShedConfig
	depth *fakeDepth
	svc   *fakeIngest
}

func newRig(t *testing.T) *ingestRig { return newRigWith(t, rigOptions{}) }

func newRigWith(t *testing.T, o rigOptions) *ingestRig {
	t.Helper()

	svc := o.svc
	if svc == nil {
		svc = &fakeIngest{}
	}
	if svc.result == (service.AcceptResult{}) && svc.err == nil {
		svc.result = service.AcceptResult{
			BatchID:    uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b77"),
			AlertCount: 2,
		}
	}

	tokens := &fakeTokenStore{token: domain.IngestToken{
		ID:       uuid.MustParse("0198f3c1-6b02-7a55-9c10-4d5e6f708192"),
		OrgID:    apitest.OrgID,
		SourceID: testSourceID,
	}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	auth := NewAuthenticator(tokens, clock.New(), DefaultTokenTTL)

	var sampler QueueDepthSampler
	if o.depth != nil {
		sampler = o.depth
	}
	shed := NewShedder(o.shed, nil, sampler, clock.New(), logger, service.NewMetrics(nil))

	rt := NewRouter(svc, auth, shed, clock.New(), logger, service.NewMetrics(nil))

	// Anonymous: the ingest route carries its own credential and a session cookie
	// must never be able to post alerts. Presenting a principal here would make
	// every authentication assertion below meaningless.
	return &ingestRig{
		client: apitest.New(rt).Anonymous(),
		svc:    svc,
		tokens: tokens,
		depth:  o.depth,
	}
}

// apiV1 is where internal/app roots the versioned tree. The contract states the
// FULL path, the domain router registers the part below the root, and apitest
// mounts that router at the chi root — so the prefix has to come off here, and
// nowhere else.
const apiV1 = "/api/v1"

// target is the path the CONTRACT declares, with the id substituted and the
// version root removed — so a route that moved fails here rather than silently
// answering 404 forever.
func target(t *testing.T, sourceID string) string {
	t.Helper()
	op := schema.Op(t, "ingestAlertmanagerWebhook")
	if !op.HasPathParam(SourceIDParam) {
		t.Fatalf("the contract's path %s no longer carries {%s}", op.Path, SourceIDParam)
	}
	if !strings.HasPrefix(op.Path, apiV1+"/") {
		t.Fatalf("the contract's path %s is not under %s; the router mounts below that root",
			op.Path, apiV1)
	}
	routed := strings.TrimPrefix(op.Path, apiV1)
	return strings.Replace(routed, "{"+SourceIDParam+"}", sourceID, 1)
}

// post sends one webhook with a bearer credential.
func (rg *ingestRig) post(t *testing.T, sourceID, secret string, body []byte) *apitest.Response {
	t.Helper()
	return rg.postReader(t, sourceID, secret, bytes.NewReader(body))
}

func (rg *ingestRig) postReader(t *testing.T, sourceID, secret string, body io.Reader) *apitest.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target(t, sourceID), body)
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	return rg.client.Do(req)
}

// realBatch is an Alertmanager v4 envelope as one actually arrives on the wire,
// undocumented `routeLabels` and all. It is validated against the contract's own
// request schema by every test that sends it, so a fixture no client could send
// cannot make a handler test pass.
func realBatch(t *testing.T) []byte {
	t.Helper()

	raw := []byte(`{
      "version": "4",
      "groupKey": "{}:{alertname=\"HighErrorRate\"}",
      "truncatedAlerts": 0,
      "status": "firing",
      "receiver": "oto",
      "notification_reason": "first notification",
      "groupLabels": {"alertname": "HighErrorRate"},
      "commonLabels": {"alertname": "HighErrorRate", "severity": "critical"},
      "commonAnnotations": {"summary": "error rate is above 5%"},
      "routeLabels": {"team": "payments"},
      "externalURL": "https://alertmanager.example.com",
      "alerts": [
        {
          "status": "firing",
          "labels": {"alertname": "HighErrorRate", "severity": "critical", "cluster": "prod-eu"},
          "annotations": {"summary": "error rate is above 5%"},
          "startsAt": "2026-08-07T09:14:22.114Z",
          "endsAt": "0001-01-01T00:00:00Z",
          "generatorURL": "https://prometheus.example.com/graph?g0.expr=up",
          "fingerprint": "b7a1f0c2d3e4f506"
        },
        {
          "status": "resolved",
          "labels": {"alertname": "DiskFillingUp", "severity": "warning", "cluster": "prod-eu"},
          "annotations": {"summary": "disk is 90% full"},
          "startsAt": "2026-08-07T08:02:11.000Z",
          "endsAt": "2026-08-07T09:10:00.000Z"
        }
      ]
    }`)

	// The fixture is proved to be one a real client could send BEFORE it is used
	// to prove anything about the handler.
	schema.AssertRequest(t, "ingestAlertmanagerWebhook", raw)
	return raw
}

/* -------------------------------------------------------------------------- */
/* The happy path                                                             */
/* -------------------------------------------------------------------------- */

// ⭐ TestARealAlertmanagerBatchIsAcceptedAndReceipted.
//
// A 2xx is a promise, and the receipt is what oto's own tests and a curl-wielding
// operator read it from. The body is checked against the contract's
// `IngestAcceptedResponse`, which is the `{data, meta}` envelope with
// `additionalProperties: false` — so a bare DTO, a missing `meta.request_id`, or
// a stray field all fail here rather than in a browser.
func TestARealAlertmanagerBatchIsAcceptedAndReceipted(t *testing.T) {
	t.Parallel()

	rg := newRig(t)
	resp := rg.post(t, testSourceID.String(), goodSecret, realBatch(t))

	resp.MustStatus(t, http.StatusAccepted)
	schema.Assert(t, "ingestAlertmanagerWebhook", http.StatusAccepted, resp.Body())

	if rg.svc.accepts() != 1 {
		t.Fatalf("Accept called %d times, want 1", rg.svc.accepts())
	}
	if rg.svc.gotMode != domain.ModePush {
		t.Fatalf("mode = %q, want %q — a webhook is a push and nothing else",
			rg.svc.gotMode, domain.ModePush)
	}
	if rg.svc.gotID != testSourceID {
		t.Fatalf("source id = %s, want %s", rg.svc.gotID, testSourceID)
	}
	// The dedup checksum is taken over these bytes verbatim. A transport that
	// re-encoded them would break duplicate suppression for an HA Alertmanager
	// pair, which is the normal case and not the exception.
	if !bytes.Equal(rg.svc.gotBody, realBatch(t)) {
		t.Fatal("the handler did not forward the raw body verbatim; the dedup checksum is taken over it")
	}
}

// TestADuplicateIsA202CarryingTheOriginalBatchId.
//
// A clustered Alertmanager is at-least-once BY DESIGN and a network partition
// guarantees duplicates, so `duplicate: true` is a normal outcome and not a
// warning. Answering anything but 202 here would make oto's correctness depend on
// the upstream never retrying.
func TestADuplicateIsA202CarryingTheOriginalBatchId(t *testing.T) {
	t.Parallel()

	original := uuid.MustParse("0198f3c1-6c33-7f01-a2b3-c4d5e6f70819")
	rg := newRigWith(t, rigOptions{svc: &fakeIngest{result: service.AcceptResult{
		BatchID:    original,
		AlertCount: 2,
		Duplicate:  true,
	}}})

	resp := rg.post(t, testSourceID.String(), goodSecret, realBatch(t))
	resp.MustStatus(t, http.StatusAccepted)
	schema.Assert(t, "ingestAlertmanagerWebhook", http.StatusAccepted, resp.Body())

	var body struct {
		Data struct {
			BatchID   string `json:"batch_id"`
			Duplicate bool   `json:"duplicate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body(), &body); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if !body.Data.Duplicate {
		t.Fatal("duplicate = false; a replayed batch must say so")
	}
	if body.Data.BatchID != original.String() {
		t.Fatalf("batch_id = %s, want the ORIGINAL %s — every replay of the same payload gets the same answer",
			body.Data.BatchID, original)
	}
}

// TestPerAlertProblemsNeverFailTheBatch.
//
// A bad label, a missing alertname or a timestamp from 2087 is recorded as a
// rejection and the rest of the batch is processed. Failing the request instead
// would let one malformed alert delete two hundred good ones at the upstream.
func TestPerAlertProblemsNeverFailTheBatch(t *testing.T) {
	t.Parallel()

	rg := newRigWith(t, rigOptions{svc: &fakeIngest{result: service.AcceptResult{
		BatchID:         uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b77"),
		AlertCount:      198,
		TruncatedAlerts: 3,
		RejectedAlerts:  2,
	}}})

	resp := rg.post(t, testSourceID.String(), goodSecret, realBatch(t))
	resp.MustStatus(t, http.StatusAccepted)
	schema.Assert(t, "ingestAlertmanagerWebhook", http.StatusAccepted, resp.Body())

	var body struct {
		Data struct {
			AlertCount      int `json:"alert_count"`
			TruncatedAlerts int `json:"truncated_alerts"`
			RejectedAlerts  int `json:"rejected_alerts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body(), &body); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if body.Data.RejectedAlerts != 2 || body.Data.TruncatedAlerts != 3 || body.Data.AlertCount != 198 {
		t.Fatalf("receipt = %+v; the losses must be reported, never silent", body.Data)
	}
}

/* -------------------------------------------------------------------------- */
/* 401 — and it tells the caller nothing else                                 */
/* -------------------------------------------------------------------------- */

// ⛔ TestEveryAuthenticationFailureIsTheSame401.
//
// Absent, malformed, unknown, and scoped-to-another-source must be
// INDISTINGUISHABLE. A caller that can tell "that token exists but is for a
// different source" from "no such token" is a caller enumerating an org's sources
// with nothing but a bearer header.
//
// 401 is one of only three 4xx this endpoint may produce, and it qualifies for the
// same reason the other two do: retrying the same bytes with the same bad
// credential could not succeed, so no retry is being denied.
func TestEveryAuthenticationFailureIsTheSame401(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		secret string
		// wrongScope makes the store answer with a token for a DIFFERENT source —
		// a real, live, non-revoked credential that simply is not one for this
		// endpoint.
		wrongScope bool
		unknown    bool
	}{
		{name: "no credential at all", secret: ""},
		{name: "a bearer with no secret", secret: " "},
		{name: "a personal access token", secret: patSecret},
		{name: "something that is not a token", secret: "hunter2"},
		{name: "an unknown ingest token", secret: goodSecret, unknown: true},
		{name: "a token for another source", secret: goodSecret, wrongScope: true},
	}

	var bodies []string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rg := newRig(t)
			if tc.wrongScope {
				rg.tokens.token.SourceID = otherSource
			}
			if tc.unknown {
				rg.tokens.err = errors.New("no such token")
			}

			resp := rg.post(t, testSourceID.String(), tc.secret, realBatch(t))
			resp.MustStatus(t, http.StatusUnauthorized)
			schema.AssertProblem(t, "ingestAlertmanagerWebhook", http.StatusUnauthorized, resp.Body())

			if rg.svc.accepts() != 0 {
				t.Fatal("a refused request still reached the ingest service")
			}
			p := resp.Problem(t)
			bodies = append(bodies, p.Code+"|"+p.Detail+"|"+p.Title)
		})
	}

	// The refusals must be byte-identical in everything a caller can observe.
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Fatalf("the refusals differ:\n  %q\n  %q\nan attacker reads that difference as "+
				"confirmation that a source exists", bodies[0], bodies[i])
		}
	}
}

// TestAPersonalAccessTokenNeverReachesTheTokenStore.
//
// The `oto_ingest_` prefix is checked before anything is hashed or queried. That
// is not an optimisation: it is what keeps a UI credential — which can read
// alerts, list sources and mint its own siblings — from ever being resolved on a
// path that lives in an `alertmanager.yml` on every cluster.
func TestAPersonalAccessTokenNeverReachesTheTokenStore(t *testing.T) {
	t.Parallel()

	rg := newRig(t)
	rg.post(t, testSourceID.String(), patSecret, realBatch(t)).MustStatus(t, http.StatusUnauthorized)

	if rg.tokens.lookups != 0 {
		t.Fatalf("the token store was queried %d time(s) for a PAT; the prefix check is the whole "+
			"reason a UI credential cannot be probed against this endpoint", rg.tokens.lookups)
	}
}

// TestTheTokenStoreOnlyEverSeesADigest. The plaintext secret must not escape
// auth.go — not into a query, not into an error string, not into a log line.
func TestTheTokenStoreOnlyEverSeesADigest(t *testing.T) {
	t.Parallel()

	rg := newRig(t)
	rg.post(t, testSourceID.String(), goodSecret, realBatch(t)).MustStatus(t, http.StatusAccepted)

	if rg.tokens.lookups != 1 {
		t.Fatalf("lookups = %d, want 1 — §G.1 budgets exactly one indexed lookup", rg.tokens.lookups)
	}
	if len(rg.tokens.gotDigest) != 32 {
		t.Fatalf("the store was handed %d bytes, want a 32-byte sha256 digest", len(rg.tokens.gotDigest))
	}
	if bytes.Contains(rg.tokens.gotDigest, []byte(goodSecret)) {
		t.Fatal("the plaintext secret reached the token store")
	}
}

// TestTheResolvedTokenIsCachedSoOneWebhookIsOneLookup.
//
// §G.1 budgets ONE indexed lookup per webhook, cached for 60 s. On the only path
// in oto with a latency budget an upstream enforces, the cache is the difference
// between one database round trip and two.
func TestTheResolvedTokenIsCachedSoOneWebhookIsOneLookup(t *testing.T) {
	t.Parallel()

	rg := newRig(t)
	for range 5 {
		rg.post(t, testSourceID.String(), goodSecret, realBatch(t)).MustStatus(t, http.StatusAccepted)
	}
	if rg.tokens.lookups != 1 {
		t.Fatalf("lookups = %d for five webhooks, want 1 — the positive cache is not holding",
			rg.tokens.lookups)
	}
}

// ⛔ TestAFailedLookupIsNeverCached.
//
// Caching a failure would let one probe with a wrong token lock a source out for
// a minute, and would turn the cache into an amplifier for exactly the traffic it
// should not serve.
func TestAFailedLookupIsNeverCached(t *testing.T) {
	t.Parallel()

	rg := newRig(t)
	rg.tokens.err = errors.New("no such token")

	for range 3 {
		rg.post(t, testSourceID.String(), goodSecret, realBatch(t)).MustStatus(t, http.StatusUnauthorized)
	}
	if rg.tokens.lookups != 3 {
		t.Fatalf("lookups = %d for three failures, want 3; a cached refusal would lock a source out "+
			"for the whole TTL after one bad probe", rg.tokens.lookups)
	}
}

// TestASourceIdThatIsNotAUuidIs401AndNot404.
//
// A 404 would tell an unauthenticated caller which ids exist. No token can be
// scoped to something that is not a uuid, so 401 is both true and silent.
func TestASourceIdThatIsNotAUuidIs401AndNot404(t *testing.T) {
	t.Parallel()

	rg := newRig(t)
	resp := rg.post(t, "not-a-uuid", goodSecret, realBatch(t))

	resp.MustStatus(t, http.StatusUnauthorized)
	schema.AssertProblem(t, "ingestAlertmanagerWebhook", http.StatusUnauthorized, resp.Body())
	if rg.tokens.lookups != 0 {
		t.Fatal("an unparseable source id still cost a token lookup")
	}
}

/* -------------------------------------------------------------------------- */
/* 503 — backpressure, and never a 4xx                                        */
/* -------------------------------------------------------------------------- */

// ⭐ TestBackpressureIsA503WithARetryAfterAndNeverA429.
//
// This is C4. Shedding is a FEATURE: Alertmanager retries 5xx for about five
// minutes, so a 503 with a Retry-After is a designed, sufficient backpressure
// channel. A 429 would look like the polite answer and would make the upstream
// DELETE the notification permanently and silently — the alert is gone, and
// nobody finds out.
//
// The queue-depth branch is the one exercised here: it is the cheapest of the
// three and the only one whose trigger a test can state exactly.
func TestBackpressureIsA503WithARetryAfterAndNeverA429(t *testing.T) {
	t.Parallel()

	depth := &fakeDepth{depth: 50_000}
	rg := newRigWith(t, rigOptions{
		shed:  ShedConfig{MaxQueueDepth: 25_000, RetryAfter: domain.RetryAfter},
		depth: depth,
	})

	resp := rg.post(t, testSourceID.String(), goodSecret, realBatch(t))

	if resp.Code() == http.StatusTooManyRequests {
		t.Fatal("⛔ the shed answered 429. Alertmanager deletes the notification permanently for " +
			"ANY 4xx, so this is not backpressure — it is data loss with a polite status line")
	}
	if resp.Code() < 500 {
		t.Fatalf("status = %d; a transient condition may NEVER be a 4xx on this path\n%s",
			resp.Code(), resp)
	}
	resp.MustStatus(t, http.StatusServiceUnavailable)
	schema.AssertProblem(t, "ingestAlertmanagerWebhook", http.StatusServiceUnavailable, resp.Body())

	// The header is what Alertmanager and every intermediary actually read.
	raw := resp.Header("Retry-After")
	if raw == "" {
		t.Fatal("no Retry-After on the 503; the contract declares it and an upstream has nothing " +
			"but this header to pace itself with")
	}
	secs, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("Retry-After = %q, want an integer number of seconds", raw)
	}
	if secs <= 0 {
		t.Fatalf("Retry-After = %d; the contract declares minimum 1", secs)
	}

	// And the body mirrors it, so a client that reads the problem rather than the
	// header gets the same number.
	if got := resp.Problem(t).RetryAfter; got != secs {
		t.Fatalf("retry_after_seconds = %d but the header says %d; a client cannot believe both",
			got, secs)
	}

	if rg.svc.accepts() != 0 {
		t.Fatal("a shed request still reached the ingest service")
	}
}

// ⭐ TestTheShedDecisionHappensBeforeTheBodyIsRead.
//
// Shedding AFTER reading eight megabytes has already spent the resource the shed
// was meant to protect — under real overload that is the difference between
// backpressure and a slower way to fall over.
func TestTheShedDecisionHappensBeforeTheBodyIsRead(t *testing.T) {
	t.Parallel()

	rg := newRigWith(t, rigOptions{
		shed:  ShedConfig{MaxQueueDepth: 1, RetryAfter: domain.RetryAfter},
		depth: &fakeDepth{depth: 2},
	})

	probe := &countingReader{r: bytes.NewReader(realBatch(t))}
	resp := rg.postReader(t, testSourceID.String(), goodSecret, probe)

	resp.MustStatus(t, http.StatusServiceUnavailable)
	if probe.reads != 0 {
		t.Fatalf("the handler read the body %d time(s) before shedding; the point of the gate is "+
			"to refuse before the bytes are spent", probe.reads)
	}
}

// ⛔ TestTheConcurrencyGateShedsRatherThanQueueingPastTheAcquisitionBudget.
//
// `Wait` IS `ingest.acquire_timeout`: pgxpool has no acquisition timeout of its
// own, so this gate is where that budget is enforced. oto would rather answer 503
// in half a second and let Alertmanager retry inside its ~5-minute budget than
// hold the upstream's connection open for five — a request queued past the
// upstream's own deadline is a notification nobody ever hears about.
func TestTheConcurrencyGateShedsRatherThanQueueingPastTheAcquisitionBudget(t *testing.T) {
	t.Parallel()

	svc := &fakeIngest{
		result:  service.AcceptResult{BatchID: uuid.New(), AlertCount: 1},
		entered: make(chan struct{}),
		hold:    make(chan struct{}),
	}
	rg := newRigWith(t, rigOptions{
		shed: ShedConfig{MaxInFlight: 1, Wait: 20 * time.Millisecond, RetryAfter: domain.RetryAfter},
		svc:  svc,
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		rg.post(t, testSourceID.String(), goodSecret, realBatch(t))
	}()

	// The gate now holds its only slot, and it is held by a request that is inside
	// Accept — which is exactly the state real overload produces.
	select {
	case <-svc.entered:
	case <-time.After(5 * time.Second):
		close(svc.hold)
		t.Fatal("the first request never reached the service")
	}

	resp := rg.post(t, testSourceID.String(), goodSecret, realBatch(t))

	close(svc.hold)
	<-done

	resp.MustStatus(t, http.StatusServiceUnavailable)
	if resp.Header("Retry-After") == "" {
		t.Fatal("the in-flight shed carries no Retry-After")
	}
	schema.AssertProblem(t, "ingestAlertmanagerWebhook", http.StatusServiceUnavailable, resp.Body())
}

// ⛔ TestAFailedQueueDepthSampleDoesNotShed.
//
// If `river_job` cannot be counted the honest state is "unknown", and turning
// unknown into 503 would mean a hiccup on the GENERAL pool stops ingestion —
// which is the exact coupling the two-pool design exists to prevent (§G.10).
func TestAFailedQueueDepthSampleDoesNotShed(t *testing.T) {
	t.Parallel()

	depth := &fakeDepth{err: errors.New("river_job is unreadable")}
	rg := newRigWith(t, rigOptions{
		shed:  ShedConfig{MaxQueueDepth: 1, RetryAfter: domain.RetryAfter},
		depth: depth,
	})

	resp := rg.post(t, testSourceID.String(), goodSecret, realBatch(t))
	resp.MustStatus(t, http.StatusAccepted)

	if depth.samples == 0 {
		t.Fatal("the sampler was never consulted; this test is not exercising the branch it names")
	}
	if rg.svc.accepts() != 1 {
		t.Fatal("an unreadable queue depth stopped ingestion")
	}
}

// TestAQueueDepthBelowTheThresholdAdmits, so the shed above is a decision and not
// a constant.
func TestAQueueDepthBelowTheThresholdAdmits(t *testing.T) {
	t.Parallel()

	rg := newRigWith(t, rigOptions{
		shed:  ShedConfig{MaxQueueDepth: 25_000, RetryAfter: domain.RetryAfter},
		depth: &fakeDepth{depth: 24_999},
	})

	rg.post(t, testSourceID.String(), goodSecret, realBatch(t)).MustStatus(t, http.StatusAccepted)
}

// ⛔ TestAnUnreadableBodyIs503AndNotA400.
//
// A dropped connection mid-upload is transient BY DEFINITION: the upstream still
// holds the notification and can send it again. Answering 400 would tell it the
// bytes are permanently bad and destroy an alert over a network blip.
func TestAnUnreadableBodyIs503AndNotA400(t *testing.T) {
	t.Parallel()

	rg := newRig(t)
	resp := rg.postReader(t, testSourceID.String(), goodSecret,
		&failingReader{err: errors.New("connection reset by peer")})

	if resp.Code() >= 400 && resp.Code() < 500 {
		t.Fatalf("status = %d; a read that failed mid-stream is transient, and a 4xx makes "+
			"Alertmanager discard the notification permanently", resp.Code())
	}
	resp.MustStatus(t, http.StatusServiceUnavailable)
	schema.AssertProblem(t, "ingestAlertmanagerWebhook", http.StatusServiceUnavailable, resp.Body())
	if resp.Header("Retry-After") == "" {
		t.Fatal("no Retry-After: the upstream has nothing to pace its retry with")
	}
	if rg.svc.accepts() != 0 {
		t.Fatal("a body that could not be read still reached the ingest service")
	}
}

// TestAServiceFailureIsA503AndNotA500WhenItIsTransient — the mapping is errs'
// job, but the handler is the last place it can be got wrong, and getting it
// wrong here is silent alert loss.
func TestAServiceFailureIsA503AndNotA500WhenItIsTransient(t *testing.T) {
	t.Parallel()

	rg := newRigWith(t, rigOptions{svc: &fakeIngest{
		err: newUnavailable(),
	}})

	resp := rg.post(t, testSourceID.String(), goodSecret, realBatch(t))
	resp.MustStatus(t, http.StatusServiceUnavailable)
	schema.AssertProblem(t, "ingestAlertmanagerWebhook", http.StatusServiceUnavailable, resp.Body())
	if resp.Header("Retry-After") == "" {
		t.Fatal("no Retry-After on a failed accept; Alertmanager retries blind")
	}
}

/* -------------------------------------------------------------------------- */
/* 413 — permanent, and recorded                                              */
/* -------------------------------------------------------------------------- */

// ⛔ TestABodyOverTheEightMebibyteBoundIsRefusedAndRecorded.
//
// 413 is one of the three permitted 4xx, and it is permitted ONLY because the row
// in `ingest_rejections` makes it defensible: the operator can see what arrived
// and why it was refused. A bare 413 from a body-cap middleware — which is why
// this route must not sit behind the global one — refuses permanently and leaves
// no trace at all.
func TestABodyOverTheEightMebibyteBoundIsRefusedAndRecorded(t *testing.T) {
	t.Parallel()

	rg := newRig(t)

	// One byte over B1, which is also `ingest_batches_bytes_ck`.
	oversized := bytes.Repeat([]byte("x"), int(domain.MaxBodyBytes)+1)
	resp := rg.post(t, testSourceID.String(), goodSecret, oversized)

	resp.MustStatus(t, http.StatusRequestEntityTooLarge)
	schema.AssertProblem(t, "ingestAlertmanagerWebhook", http.StatusRequestEntityTooLarge, resp.Body())

	if rg.svc.tooLarge != 1 {
		t.Fatalf("RecordBodyTooLarge called %d times, want 1 — the row is the whole reason a "+
			"permanent refusal is defensible here", rg.svc.tooLarge)
	}
	if rg.svc.gotBytes <= domain.MaxBodyBytes {
		t.Fatalf("the recorded size is %d, which is not over the %d-byte bound",
			rg.svc.gotBytes, domain.MaxBodyBytes)
	}
	if rg.svc.accepts() != 0 {
		t.Fatal("an oversized body still reached Accept")
	}
}

// TestABodyExactlyAtTheBoundIsAccepted, because an off-by-one at B1 refuses a
// batch the DDL would happily have stored — permanently, at an upstream that
// never retries a 4xx.
func TestABodyExactlyAtTheBoundIsAccepted(t *testing.T) {
	t.Parallel()

	rg := newRig(t)

	// A valid envelope padded out to EXACTLY MaxBodyBytes with an annotation whose
	// length nothing else depends on.
	base := `{"version":"4","status":"firing","alerts":[{"status":"firing","labels":{"alertname":"A"},` +
		`"startsAt":"2026-08-07T09:14:22.114Z","annotations":{"summary":"`
	tail := `"}}]}`
	pad := int(domain.MaxBodyBytes) - len(base) - len(tail)
	body := []byte(base + strings.Repeat("y", pad) + tail)
	if int64(len(body)) != domain.MaxBodyBytes {
		t.Fatalf("the fixture is %d bytes, want exactly %d", len(body), domain.MaxBodyBytes)
	}
	schema.AssertRequest(t, "ingestAlertmanagerWebhook", body)

	resp := rg.post(t, testSourceID.String(), goodSecret, body)
	resp.MustStatus(t, http.StatusAccepted)
	if rg.svc.tooLarge != 0 {
		t.Fatal("a body exactly at the bound was recorded as too large")
	}
}

// TestAnEmptyBodyIsNotShortCircuitedByTheTransport.
//
// An empty body is undecodable, and undecodable bodies are recorded in
// `ingest_rejections` BY THE SERVICE. Answering 400 from the transport would skip
// the row that makes the 400 defensible, which is the same failure the 413 path
// above exists to avoid.
func TestAnEmptyBodyIsNotShortCircuitedByTheTransport(t *testing.T) {
	t.Parallel()

	rg := newRig(t)
	rg.post(t, testSourceID.String(), goodSecret, nil)

	if rg.svc.accepts() != 1 {
		t.Fatal("an empty body was refused by the transport; the service never got the chance to " +
			"record why, and a 400 with no row behind it is a permanent refusal nobody can explain")
	}
	if len(rg.svc.gotBody) != 0 {
		t.Fatalf("the service was handed %d bytes for an empty body", len(rg.svc.gotBody))
	}
}

/* -------------------------------------------------------------------------- */
/* The route itself                                                           */
/* -------------------------------------------------------------------------- */

// TestTheIngestRouterMountsExactlyOneRoute.
//
// "One route, and deliberately only one" is a security claim: this surface is
// unauthenticated by the UI's standards, carries no body cap from the global
// middleware and has no rate limiter. Anything else that appeared here would
// inherit all three properties by accident.
func TestTheIngestRouterMountsExactlyOneRoute(t *testing.T) {
	t.Parallel()

	rg := newRig(t)

	// A neighbouring path under the same prefix must not exist.
	resp := rg.postReader(t, testSourceID.String(), goodSecret, bytes.NewReader(realBatch(t)))
	resp.MustStatus(t, http.StatusAccepted)

	req := httptest.NewRequest(http.MethodGet, target(t, testSourceID.String()), nil)
	req.Header.Set("Authorization", "Bearer "+goodSecret)
	if got := rg.client.Do(req).Code(); got != http.StatusMethodNotAllowed && got != http.StatusNotFound {
		t.Fatalf("GET on the ingest path answered %d; only POST is mounted here", got)
	}
}

/* -------------------------------------------------------------------------- */
/* Small helpers                                                              */
/* -------------------------------------------------------------------------- */

// countingReader records whether the body was touched at all.
type countingReader struct {
	r     io.Reader
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads++
	return c.r.Read(p)
}

// failingReader is a connection that died mid-upload.
type failingReader struct{ err error }

func (f *failingReader) Read([]byte) (int, error) { return 0, f.err }

// newUnavailable builds the error a slow database or a failed queue insert
// produces, through the same constructor the service uses.
func newUnavailable() error {
	return errs.Unavailable("ingest_enqueue_failed", "the batch could not be enqueued; retry",
		domain.RetryAfter)
}
