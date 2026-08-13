package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	alertdomain "github.com/thulasiram/oto/internal/alerts/domain"
	alerts "github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/rules/domain"
	"github.com/thulasiram/oto/internal/rules/service"
)

// The batch snapshot read (ADR 0025).
//
// The behaviour under test is not "the query works". It is the set of promises
// the alert list is built on top of: ONE call answers a whole page, an id the
// caller cannot be sure about does not take the page down with it, duplicates
// are the normal case rather than an error, and the payload is the same
// `RuleSnapshotDTO` `/alerts/{id}/rule` returns — because the moment those two
// differ, the list and the detail page start telling different stories about the
// same rule.

/* -------------------------------------------------------------------------- */
/* Fakes                                                                      */
/* -------------------------------------------------------------------------- */

// fakeRules is a RuleService whose whole memory is a map of snapshots. It also
// COUNTS the calls, because "one call for the page" is the property this
// endpoint exists to provide and a fake that cannot count it cannot prove it.
type fakeRules struct {
	snaps map[uuid.UUID]domain.Snapshot
	// batchCalls is how many times GetMany was invoked.
	batchCalls int
	// batchIDs is the id list of the most recent GetMany, exactly as the handler
	// passed it, so the test can see whether duplicates were forwarded.
	batchIDs []uuid.UUID
	// getCalls counts the SINGLE-snapshot reads, which must stay at zero: a
	// batch that loops Get() per id is the N+1 wearing a different hat.
	getCalls int
	err      error
}

func (f *fakeRules) Get(_ context.Context, _ db.TenantScope, id uuid.UUID) (domain.Snapshot, error) {
	f.getCalls++
	s, ok := f.snaps[id]
	if !ok {
		return domain.Snapshot{}, errs.NotFound("rules_snapshot_not_found", "no such rule snapshot")
	}
	return s, nil
}

func (f *fakeRules) GetMany(_ context.Context, _ db.TenantScope, ids []uuid.UUID) ([]domain.Snapshot, error) {
	f.batchCalls++
	f.batchIDs = append([]uuid.UUID(nil), ids...)
	if f.err != nil {
		return nil, f.err
	}
	out := make([]domain.Snapshot, 0, len(ids))
	seen := map[uuid.UUID]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if s, ok := f.snaps[id]; ok {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeRules) History(context.Context, db.TenantScope, domain.Key) (domain.History, error) {
	return domain.History{}, nil
}

func (f *fakeRules) ListSnapshots(context.Context, db.TenantScope, domain.Key, db.Keyset) (service.SnapshotPage, error) {
	return service.SnapshotPage{}, nil
}

// fakeAlerts satisfies the cross-domain port. The batch endpoint must never
// touch it — it takes snapshot ids, not alert ids — so every method fails loudly
// if it is reached.
type fakeAlerts struct{ t *testing.T }

func (f fakeAlerts) Get(context.Context, db.TenantScope, uuid.UUID) (alerts.AlertDetail, error) {
	f.t.Fatal("the snapshot batch read reached into alerts; it takes snapshot ids and nothing else")
	return alerts.AlertDetail{}, nil
}

func (f fakeAlerts) GetOccurrence(context.Context, db.TenantScope, uuid.UUID) (alertdomain.Occurrence, error) {
	f.t.Fatal("the snapshot batch read reached into alerts; it takes snapshot ids and nothing else")
	return alertdomain.Occurrence{}, nil
}

func (f fakeAlerts) PreviousOccurrenceWithRule(
	context.Context, db.TenantScope, uuid.UUID, int,
) (alertdomain.Occurrence, bool, error) {
	f.t.Fatal("the snapshot batch read reached into alerts; it takes snapshot ids and nothing else")
	return alertdomain.Occurrence{}, false, nil
}

/* -------------------------------------------------------------------------- */
/* Harness                                                                    */
/* -------------------------------------------------------------------------- */

func snapshotFixture(id uuid.UUID, name, expr string, forSeconds float64) domain.Snapshot {
	return domain.Snapshot{
		ID:    id.String(),
		OrgID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		Key: domain.Key{
			SourceID: "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
			File:     "/etc/rules/app.yml",
			Group:    "app",
			Name:     name,
		},
		Fingerprint: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Expr:        expr,
		ForSeconds:  forSeconds,
		Labels:      map[string]string{"severity": "critical"},
		Annotations: map[string]string{"summary": "it is on fire"},
		Origin:      domain.OriginPrometheusAPI,
		Confidence:  domain.ConfidenceExact,
		CapturedAt:  time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}
}

func newBatchRouter(t *testing.T, snaps ...domain.Snapshot) (*Router, *fakeRules) {
	t.Helper()

	byID := make(map[uuid.UUID]domain.Snapshot, len(snaps))
	for _, s := range snaps {
		byID[uuid.MustParse(s.ID)] = s
	}
	svc := &fakeRules{snaps: byID}
	return NewRouter(svc, fakeAlerts{t: t}, clock.New()), svc
}

// getBatch runs one request as an authenticated org member — the least
// privileged caller the endpoint accepts.
func getBatch(t *testing.T, rt *Router, query string) *httptest.ResponseRecorder {
	t.Helper()

	r := chi.NewRouter()
	rt.Mount(r)

	req := httptest.NewRequest(http.MethodGet, "/rule-snapshots/batch"+query, nil)
	ctx := authn.Into(req.Context(), authn.Principal{
		Kind:   authn.KindSession,
		OrgID:  uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"),
		UserID: uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"),
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

// decodeBatch reads the response as the caller sees it — a `data` array and a
// `meta`, and NO `page`.
func decodeBatch(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v; body = %s", err, rec.Body.String())
	}
	return body
}

func batchData(t *testing.T, rec *httptest.ResponseRecorder) []any {
	t.Helper()

	body := decodeBatch(t, rec)
	data, ok := body["data"].([]any)
	if !ok {
		t.Fatalf("data is %T, want an array; body = %s", body["data"], rec.Body.String())
	}
	return data
}

/* -------------------------------------------------------------------------- */
/* The promises                                                               */
/* -------------------------------------------------------------------------- */

// ⭐ TestOnePageOfAlertsCostsOneCall is the whole issue, in one assertion.
//
// Fifty alert rows carrying three distinct snapshot ids must be answerable by a
// single request. Before this endpoint the only way to render `expr` on the list
// was `GET /alerts/{id}/rule` fifty times, which is why the list never showed the
// rule at all — losing the product's first promise on the screen users live in.
func TestOnePageOfAlertsCostsOneCall(t *testing.T) {
	t.Parallel()

	a := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	b := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	c := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	rt, svc := newBatchRouter(t,
		snapshotFixture(a, "HighErrorRate", `sum(rate(http_errors[5m])) > 0.05`, 300),
		snapshotFixture(b, "DiskFillingUp", `disk_free < 0.1`, 600),
		snapshotFixture(c, "PodCrashLooping", `restarts > 5`, 0),
	)

	// Fifty rows, three distinct rules — the shape a real page has, because a
	// content-addressed snapshot is shared by every alert that fired under it.
	ids := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		ids = append(ids, []uuid.UUID{a, b, c}[i%3].String())
	}

	rec := getBatch(t, rt, "?id="+strings.Join(ids, ","))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if svc.batchCalls != 1 {
		t.Fatalf("GetMany called %d times, want exactly 1 for the whole page", svc.batchCalls)
	}
	if svc.getCalls != 0 {
		t.Fatalf("the batch made %d single-snapshot reads; a loop over Get is the N+1 it replaces",
			svc.getCalls)
	}
	if got := len(batchData(t, rec)); got != 3 {
		t.Fatalf("data has %d snapshots, want 3 — the distinct rules behind fifty rows", got)
	}
}

// ⭐ TestTheSnapshotCarriesWhatTheRuleSaid: the point of the endpoint is `expr`
// and `for`, so a payload that omitted either would satisfy every other test here
// and still fail the user.
func TestTheSnapshotCarriesWhatTheRuleSaid(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	rt, _ := newBatchRouter(t,
		snapshotFixture(id, "HighErrorRate", `sum(rate(http_errors[5m])) > 0.05`, 300))

	rec := getBatch(t, rt, "?id="+id.String())
	data := batchData(t, rec)
	if len(data) != 1 {
		t.Fatalf("data has %d entries, want 1", len(data))
	}
	snap, ok := data[0].(map[string]any)
	if !ok {
		t.Fatalf("data[0] is %T, want an object", data[0])
	}

	if got := snap["expr"]; got != `sum(rate(http_errors[5m])) > 0.05` {
		t.Fatalf("expr = %v, want the captured expression", got)
	}
	if got := snap["for_seconds"]; got != float64(300) {
		t.Fatalf("for_seconds = %v, want 300", got)
	}
	if got := snap["rule_name"]; got != "HighErrorRate" {
		t.Fatalf("rule_name = %v, want HighErrorRate", got)
	}
	// The id is the join key the alert row holds. Without it the client cannot
	// put the rule back on the row it came from, and the batch is useless.
	if got := snap["id"]; got != id.String() {
		t.Fatalf("id = %v, want %s — the row's join key", got, id)
	}
	// `origin` and `match_confidence` travel too: an `ambiguous` match is
	// surfaced, never silently resolved (ADR 0009).
	if got := snap["match_confidence"]; got != "exact" {
		t.Fatalf("match_confidence = %v, want exact", got)
	}
	if got := snap["origin"]; got != "prometheus_api" {
		t.Fatalf("origin = %v, want prometheus_api", got)
	}
}

// ⛔ TestAnUnknownIdDoesNotTakeThePageDown.
//
// A caller mixing one id it cannot resolve — another org's, or a stale one held
// in a browser tab — must still get every rule it CAN have. Failing the request
// would blank the rule column of a page that was otherwise entirely answerable,
// which is a worse lie than the missing row.
func TestAnUnknownIdDoesNotTakeThePageDown(t *testing.T) {
	t.Parallel()

	known := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	stranger := uuid.MustParse("99999999-9999-4999-8999-999999999999")
	rt, _ := newBatchRouter(t, snapshotFixture(known, "HighErrorRate", `up == 0`, 60))

	rec := getBatch(t, rt, "?id="+known.String()+","+stranger.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an unresolvable id is absent, not an error; body = %s",
			rec.Code, rec.Body.String())
	}
	data := batchData(t, rec)
	if len(data) != 1 {
		t.Fatalf("data has %d entries, want 1: the known snapshot, with the stranger simply absent",
			len(data))
	}
	snap, ok := data[0].(map[string]any)
	if !ok {
		t.Fatalf("data[0] is %T, want an object", data[0])
	}
	if got := snap["id"]; got != known.String() {
		t.Fatalf("id = %v, want the known snapshot %s", got, known)
	}
}

// TestNothingKnownIsAnEmptyArrayAndNotNull.
//
// `[]` and `null` are different answers to a client that indexes the result. The
// envelope must never make a caller guess.
func TestNothingKnownIsAnEmptyArrayAndNotNull(t *testing.T) {
	t.Parallel()

	rt, _ := newBatchRouter(t)

	rec := getBatch(t, rt, "?id=99999999-9999-4999-8999-999999999999")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := decodeBatch(t, rec)
	raw, present := body["data"]
	if !present {
		t.Fatal("data is absent; the key must be present and an empty array")
	}
	if raw == nil {
		t.Fatal("data is null; an empty batch is [], never null")
	}
	if got := len(batchData(t, rec)); got != 0 {
		t.Fatalf("data has %d entries, want 0", got)
	}
}

// TestTheBatchCarriesNoPageObject.
//
// This is a bag the caller enumerated itself, not a keyset page. A `page` object
// would offer a `next_cursor` that can never exist and invite a client to follow
// it forever.
func TestTheBatchCarriesNoPageObject(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	rt, _ := newBatchRouter(t, snapshotFixture(id, "HighErrorRate", `up == 0`, 60))

	body := decodeBatch(t, getBatch(t, rt, "?id="+id.String()))
	if _, ok := body["page"]; ok {
		t.Fatalf("the batch response carries a page object: %#v", body["page"])
	}
	if _, ok := body["meta"]; !ok {
		t.Fatal("meta is absent; every envelope in this API carries one")
	}
}

// TestDuplicateIdsAreNormalAndNotAnError.
//
// The ids come off alert rows and repetition is the SIGNAL — a page where nothing
// changed upstream is one snapshot id over and over. A 422 here would force every
// caller to dedupe before asking a question it already knew the answer to.
func TestDuplicateIdsAreNormalAndNotAnError(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	rt, svc := newBatchRouter(t, snapshotFixture(id, "HighErrorRate", `up == 0`, 60))

	rec := getBatch(t, rt, "?id="+strings.Join([]string{id.String(), id.String(), id.String()}, ","))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := len(batchData(t, rec)); got != 1 {
		t.Fatalf("data has %d entries, want 1 — three copies of one id are one snapshot", got)
	}
	if len(svc.batchIDs) != 3 {
		t.Fatalf("the handler forwarded %d ids, want 3 — collapsing them is the service's job",
			len(svc.batchIDs))
	}
}

// TestRepeatedIdParameterIsTheSameAsACommaList, because `?id=a&id=b` is what a
// naive client and most URL builders produce, and it means exactly what
// `?id=a,b` means.
func TestRepeatedIdParameterIsTheSameAsACommaList(t *testing.T) {
	t.Parallel()

	a := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	b := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	rt, _ := newBatchRouter(t,
		snapshotFixture(a, "HighErrorRate", `up == 0`, 60),
		snapshotFixture(b, "DiskFillingUp", `disk_free < 0.1`, 600))

	rec := getBatch(t, rt, "?id="+a.String()+"&id="+b.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := len(batchData(t, rec)); got != 2 {
		t.Fatalf("data has %d entries, want 2", got)
	}
}

// ⛔ TestAMalformedIdIsRefusedRatherThanIgnored.
//
// Silently dropping `?id=banana` would return a page whose rule column is missing
// rows for a reason nothing on the wire explains — the same class of failure as a
// silently ignored filter (§E.3). The violation names the field.
func TestAMalformedIdIsRefusedRatherThanIgnored(t *testing.T) {
	t.Parallel()

	rt, svc := newBatchRouter(t)

	rec := getBatch(t, rt, "?id=11111111-1111-4111-8111-111111111111,banana")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "id") {
		t.Fatalf("the violation should name the parameter: %s", rec.Body.String())
	}
	if svc.batchCalls != 0 {
		t.Fatal("a refused request still reached the service")
	}
}

// TestNoIdsAtAllIsRefused. An unbounded "give me everything" is not what this
// endpoint is, and an empty answer would hide the caller's bug.
func TestNoIdsAtAllIsRefused(t *testing.T) {
	t.Parallel()

	rt, svc := newBatchRouter(t)

	rec := getBatch(t, rt, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
	if svc.batchCalls != 0 {
		t.Fatal("a request with no ids still reached the service")
	}
}

// ⛔ TestTheBatchIsBounded. MaxBatchSnapshotIDs is a URL-length bound as much as a
// database one: a request line that a proxy truncates fails in a way no error
// message ever reaches the user.
func TestTheBatchIsBounded(t *testing.T) {
	t.Parallel()

	rt, svc := newBatchRouter(t)

	ids := make([]string, 0, MaxBatchSnapshotIDs+1)
	for i := 0; i <= MaxBatchSnapshotIDs; i++ {
		ids = append(ids, fmt.Sprintf("11111111-1111-4111-8111-%012d", i))
	}

	rec := getBatch(t, rt, "?id="+strings.Join(ids, ","))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for %d ids; body = %s",
			rec.Code, len(ids), rec.Body.String())
	}
	if svc.batchCalls != 0 {
		t.Fatal("an over-long batch still reached the service")
	}
}

// TestAnUnknownParameterIsRefused keeps the endpoint under §E.3: a typo must not
// be silently ignored on a read the UI depends on.
func TestAnUnknownParameterIsRefused(t *testing.T) {
	t.Parallel()

	rt, _ := newBatchRouter(t)

	rec := getBatch(t, rt, "?id=11111111-1111-4111-8111-111111111111&ids=oops")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 unknown_parameter; body = %s", rec.Code, rec.Body.String())
	}
}

// TestBatchIsNotMistakenForASnapshotId proves the literal segment wins over
// `/{id}`. If it ever lost, `batch` would be parsed as a UUID and every caller
// would get a 404 with nothing to explain it.
func TestBatchIsNotMistakenForASnapshotId(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	rt, svc := newBatchRouter(t, snapshotFixture(id, "HighErrorRate", `up == 0`, 60))

	rec := getBatch(t, rt, "?id="+id.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if svc.batchCalls != 1 {
		t.Fatalf("GetMany called %d times; /rule-snapshots/batch was routed to the wrong handler",
			svc.batchCalls)
	}
}

// TestAStorageFailureIsNotAnEmptyBatch.
//
// A read that could not run must NOT look like "this page has no rules". oto's
// silence has to stay distinguishable from an answer — the same rule the delivery
// roll-up follows on the alert detail page.
func TestAStorageFailureIsNotAnEmptyBatch(t *testing.T) {
	t.Parallel()

	rt, svc := newBatchRouter(t)
	svc.err = errs.New(errs.KindInternal, "rules_query_failed", "could not read the rule snapshots")

	rec := getBatch(t, rt, "?id=11111111-1111-4111-8111-111111111111")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — a failed read must not render as an empty batch; body = %s",
			rec.Code, rec.Body.String())
	}
}
