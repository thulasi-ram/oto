package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/platform/httpx/middleware"
	"github.com/thulasiram/oto/internal/streaming/domain"
	"github.com/thulasiram/oto/internal/streaming/service"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// `streamEvents` — GET /api/v1/stream.
//
// The conformance audit's finding on this handler was "SSE headers never sent":
// NewSSEWriter flushed before it set them, so the wire carried a 200 with no
// Content-Type at all. A browser EventSource rejects that outright and fires
// onerror, which made the entire live UI unreachable while every log line and
// every metric said the stream was healthy. Nothing in this package guarded it.
//
// Every test below runs against a REAL net/http server rather than a
// ResponseRecorder, and that is not incidental: a recorder cannot distinguish a
// header that was SET from a header that was SENT, which is the exact distinction
// the shipped bug turned on. It is also the only way to prove the second half of
// the promise — that frames are flushed to the client while the connection is
// still open, rather than buffered until the handler returns.

/* -------------------------------------------------------------------------- */
/* Fakes                                                                      */
/* -------------------------------------------------------------------------- */

// fakeReplay is the StreamService port. Beyond answering, it RECORDS the cursor
// it was handed and can run a hook while the query is "in flight", which is what
// makes the subscribe-before-replay ordering observable from outside.
type fakeReplay struct {
	result service.ReplayResult
	err    error

	calls int
	// since is the Last-Event-ID the handler resolved, which is the only evidence
	// that a malformed header degraded to "attach live" rather than to a refusal.
	since int64
	// during runs inside Replay, standing in for everything that commits while the
	// replay query is executing.
	during func()
}

func (f *fakeReplay) Replay(_ context.Context, _ db.TenantScope, sinceSeq int64) (service.ReplayResult, error) {
	f.calls++
	f.since = sinceSeq
	if f.during != nil {
		f.during()
	}
	if f.err != nil {
		return service.ReplayResult{}, f.err
	}
	return f.result, nil
}

// The Hub is NOT faked. `*service.Subscription` is a concrete type with
// unexported channels, so a fake hub could only hand back a subscription that
// never delivers anything — which would make every live-phase assertion vacuous.
// The real hub is cheap, in-process and has no dependencies.
var _ Hub = (*service.Hub)(nil)

/* -------------------------------------------------------------------------- */
/* Harness                                                                    */
/* -------------------------------------------------------------------------- */

// testOrg is the caller's tenant, and testOtherOrg is somebody else's. They are
// the shared constants so that a failure message names an id a reader recognises.
var (
	testOrg      = apitest.OrgID
	testOtherOrg = apitest.OtherOrgID
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type streamRig struct {
	srv *httptest.Server
	hub *service.Hub
	svc *fakeReplay
}

// newStreamRig starts a real server for the stream route.
//
// The coalescing window is squeezed to a millisecond so the live-phase tests do
// not sit for 250 ms each; the window's VALUE is the hub's contract and is tested
// there, not here.
func newStreamRig(t *testing.T, scope func(context.Context) (db.TenantScope, error)) *streamRig {
	t.Helper()

	hub := service.NewHub(service.HubConfig{CoalesceWindow: time.Millisecond, Logger: quiet()})
	t.Cleanup(hub.Shutdown)

	svc := &fakeReplay{}
	if scope == nil {
		scope = func(context.Context) (db.TenantScope, error) { return mustScope(t, testOrg), nil }
	}

	rt := NewRouter(svc, hub, ScopeResolverFunc(scope), clock.New(), quiet(), service.NewMetrics(nil))

	r := chi.NewRouter()
	// The request-id middleware is not decoration: `request_id` is a REQUIRED
	// member of every problem document this route can produce.
	r.Use(middleware.RequestID)
	rt.Mount(r)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	return &streamRig{srv: srv, hub: hub, svc: svc}
}

func mustScope(t *testing.T, org uuid.UUID) db.TenantScope {
	t.Helper()
	s, err := db.NewTenantScope(org)
	if err != nil {
		t.Fatalf("build a tenant scope: %v", err)
	}
	return s
}

// open connects to the stream and returns the live response. The caller owns
// BOTH the cancel and the body: every one of these tests must be able to end a
// handler that is designed to live for hours, and a stream whose body is never
// closed holds its connection — and its hub subscription — for the rest of the
// run. `defer resp.Body.Close()` at the call site is what bodyclose checks, so
// the leak is caught by the linter rather than by a flaky suite.
func (rg *streamRig) open(t *testing.T, query string, headers map[string]string) (*http.Response, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rg.srv.URL+"/stream"+query, nil)
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := rg.srv.Client().Do(req) //nolint:bodyclose // the caller closes it; see the doc comment
	if err != nil {
		cancel()
		t.Fatalf("GET /stream: %v", err)
	}
	t.Cleanup(cancel)
	return resp, cancel
}

/* -------------------------------------------------------------------------- */
/* An SSE reader that can prove a frame arrived BEFORE the response ended      */
/* -------------------------------------------------------------------------- */

// sseFrame is one blank-line-terminated block off the wire.
type sseFrame struct {
	// ID is the `id:` field and HasID distinguishes "absent" from "empty". The
	// difference is the whole resync guarantee: an `id:` on a resync would move
	// the client's cursor past the events it is being told to refetch.
	ID    string
	HasID bool
	Event string
	Data  string
	// Comment is the payload of a bare `: ` line — the heartbeat, which is NOT a
	// frame and must never carry data.
	Comment string
	Raw     string
}

// readFrames parses the stream in a goroutine, so that every assertion about
// arrival is an assertion about arrival IN TIME rather than about the bytes that
// happened to be present once the handler finished. Nothing here reads to EOF.
func readFrames(body io.Reader) <-chan sseFrame {
	out := make(chan sseFrame, 16)
	go func() {
		defer close(out)
		br := bufio.NewReader(body)
		var cur sseFrame
		var seen bool
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				trimmed := strings.TrimRight(line, "\r\n")
				cur.Raw += line
				switch {
				case trimmed == "":
					if seen {
						out <- cur
					}
					cur, seen = sseFrame{}, false
				case strings.HasPrefix(trimmed, ":"):
					cur.Comment = strings.TrimSpace(trimmed[1:])
					seen = true
				case strings.HasPrefix(trimmed, "id: "):
					cur.ID, cur.HasID, seen = trimmed[len("id: "):], true, true
				case strings.HasPrefix(trimmed, "event: "):
					cur.Event, seen = trimmed[len("event: "):], true
				case strings.HasPrefix(trimmed, "data: "):
					cur.Data, seen = trimmed[len("data: "):], true
				default:
					seen = true
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return out
}

// next waits for one frame. The timeout is the assertion: a stream that only
// materialises at EOF never produces one, which is precisely the buffering
// failure X-Accel-Buffering exists to prevent.
func next(t *testing.T, frames <-chan sseFrame) sseFrame {
	t.Helper()
	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatal("the stream ended without sending a frame")
		}
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("no frame arrived within 5s: the response is being buffered rather than flushed, " +
			"which is exactly what an EventSource cannot survive")
		return sseFrame{}
	}
}

/* -------------------------------------------------------------------------- */
/* Fixtures                                                                   */
/* -------------------------------------------------------------------------- */

// alertEvent is a durable `alert.upserted` row whose payload is a real
// AlertUpsertedData, so the frame it produces can be validated against the
// contract's StreamFrame rather than against a shape invented here.
func alertEvent(org uuid.UUID, seq int64) domain.Event {
	id := uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b77")
	payload := `{"state":"firing","ack_state":"unacked","severity":"critical",` +
		`"alertname":"HighErrorRate","namespace":"payments","cluster_key":"prod-eu",` +
		`"last_seen_at":"2026-08-07T09:20:12.443Z","total_cases":7,"is_flapping":false}`
	return domain.Event{
		Seq:        seq,
		OrgID:      org,
		Kind:       domain.KindAlertUpserted,
		Resource:   domain.ResourceAlert,
		ResourceID: id,
		Payload:    json.RawMessage(payload),
		At:         time.Date(2026, 8, 7, 9, 20, 12, 443_000_000, time.UTC),
		AlertID:    &id,
	}
}

// otherEvent is a durable row about something an alerts-only client did NOT
// subscribe to. The payload is deliberately empty: if one of these ever reaches
// the wire the test has already failed on the `id:` line, and giving it a
// realistic body would only invite someone to assert against it.
//
// Kind and Resource are paired through domain.ResourceFor rather than restated,
// so a fixture cannot invent a combination the writer path could never produce.
func otherEvent(t *testing.T, org uuid.UUID, seq int64, kind domain.Kind) domain.Event {
	t.Helper()
	res, ok := domain.ResourceFor(kind)
	if !ok {
		t.Fatalf("%q is not a persisted kind, so no ui_events row can carry it", kind)
	}
	return domain.Event{
		Seq:        seq,
		OrgID:      org,
		Kind:       kind,
		Resource:   res,
		ResourceID: uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b78"),
		Payload:    json.RawMessage(`{}`),
		At:         time.Date(2026, 8, 7, 9, 20, 12, 443_000_000, time.UTC),
	}
}

/* -------------------------------------------------------------------------- */
/* The promises                                                               */
/* -------------------------------------------------------------------------- */

// ⭐ TestTheStreamSendsEveryHeaderAnEventSourceNeedsBeforeAnyFrame is the audit
// finding, pinned at the handler rather than at the writer.
//
// `httpx` has its own regression test for NewSSEWriter, but that test proves the
// WRITER sets the headers. This one proves the ROUTE does — that nothing between
// chi and the handler committed the response first, and that the handler reaches
// NewSSEWriter at all before it starts framing. A stream that answers 200 with no
// Content-Type is rejected by every browser EventSource, and the failure is
// invisible from the server: the frames are perfect and nobody can read them.
func TestTheStreamSendsEveryHeaderAnEventSourceNeedsBeforeAnyFrame(t *testing.T) {
	t.Parallel()

	rg := newStreamRig(t, nil)
	rg.svc.result = service.ReplayResult{Events: []domain.Event{alertEvent(testOrg, 42)}, LastSeq: 42}

	resp, cancel := rg.open(t, "", map[string]string{"Last-Event-ID": "41"})
	defer resp.Body.Close()
	defer cancel()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// The header block is committed by the FIRST flush, so reading one frame is
	// what proves these values were on the wire and not merely in a map.
	frames := readFrames(resp.Body)
	_ = next(t, frames)

	want := map[string]string{
		"Content-Type":      httpx.ContentTypeEventStream,
		"Cache-Control":     "no-cache, no-transform",
		"Connection":        "keep-alive",
		"X-Accel-Buffering": "no",
	}
	for h, v := range want {
		got := resp.Header.Get(h)
		if got == v {
			continue
		}
		// net/http manages Connection itself on an HTTP/1.1 keep-alive response and
		// strips the hop-by-hop header from the parsed result; the wire carries it.
		if h == "Connection" && got == "" {
			continue
		}
		t.Errorf("header %s = %q, want %q — %s", h, got, v, whyTheHeaderMatters(h))
	}
}

func whyTheHeaderMatters(h string) string {
	switch h {
	case "Content-Type":
		return "a browser EventSource rejects the response outright and fires onerror"
	case "Cache-Control":
		return "no-transform is what stops a proxy gzipping the stream into buffered bursts"
	case "Connection":
		return "the connection is meant to live for hours"
	default:
		return "nginx buffers proxied responses by default; this header is the only thing that turns it off"
	}
}

// ⭐ TestAReplayedEventIsAFrameTheContractRecognises.
//
// Three separate promises live in one frame and each has its own failure mode:
// `id:` is what the client echoes back as Last-Event-ID, so a wrong one silently
// replays or silently skips; `event:` is what a typed listener subscribes to, and
// the contract binds it to equal the payload's `kind`; and `data:` must satisfy
// StreamFrame, because the generated TypeScript client validates it at runtime and
// drops anything that does not.
func TestAReplayedEventIsAFrameTheContractRecognises(t *testing.T) {
	t.Parallel()

	// The route this test drives is the one the contract declares.
	if got := schema.Op(t, "streamEvents").Path; got != "/api/v1/stream" {
		t.Fatalf("the contract now declares streamEvents at %s", got)
	}

	rg := newStreamRig(t, nil)
	rg.svc.result = service.ReplayResult{Events: []domain.Event{alertEvent(testOrg, 918274)}, LastSeq: 918274}

	resp, cancel := rg.open(t, "", map[string]string{"Last-Event-ID": "918273"})
	defer resp.Body.Close()
	defer cancel()

	f := next(t, readFrames(resp.Body))

	if !f.HasID || f.ID != "918274" {
		t.Fatalf("id = %q (present=%v), want 918274 — this is the value the client resumes from",
			f.ID, f.HasID)
	}
	if f.Event != string(domain.KindAlertUpserted) {
		t.Fatalf("event = %q, want %q; the contract binds the SSE event name to the payload's kind",
			f.Event, domain.KindAlertUpserted)
	}
	if !strings.HasSuffix(f.Raw, "\n\n") {
		t.Fatalf("the frame is not terminated by a blank line; SSE never delivers it: %q", f.Raw)
	}

	// The one assertion that cannot drift: the bytes are checked against the
	// contract's own schema, not against a shape restated here.
	sch := schema.Component(t, "StreamFrame")
	var v any
	if err := json.Unmarshal([]byte(f.Data), &v); err != nil {
		t.Fatalf("the data: line is not JSON: %v (%q)", err, f.Data)
	}
	if err := sch.Validate(v); err != nil {
		t.Fatalf("the frame does not satisfy StreamFrame:\n%v\ndata = %s", err, f.Data)
	}

	// And the fields a client joins on, spelled out, because a schema-shaped frame
	// about the wrong resource is still a frame that updates the wrong row.
	var frame struct {
		Seq      int64  `json:"seq"`
		Kind     string `json:"kind"`
		Resource string `json:"resource"`
		OrgID    string `json:"org_id"`
	}
	if err := json.Unmarshal([]byte(f.Data), &frame); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if frame.Seq != 918274 {
		t.Fatalf("data.seq = %d, want 918274 — it must agree with the id: line", frame.Seq)
	}
	if frame.Resource != string(domain.ResourceAlert) {
		t.Fatalf("data.resource = %q, want %q; the client picks its refetch endpoint from this",
			frame.Resource, domain.ResourceAlert)
	}
	if frame.OrgID != testOrg.String() {
		t.Fatalf("data.org_id = %q, want the caller's own org", frame.OrgID)
	}
}

// ⭐ TestFramesArriveWhileTheConnectionIsStillOpen.
//
// This is the property the whole feature is: a change notice reaches the browser
// when it happens, not when the response ends. A handler that buffered would pass
// every shape assertion above and still leave the UI dark, and that failure mode
// is invisible in any test that reads to EOF.
//
// The proof is ordering: the second frame is published only AFTER the first has
// been read off the socket, so it cannot have been sitting in the same buffer.
func TestFramesArriveWhileTheConnectionIsStillOpen(t *testing.T) {
	t.Parallel()

	rg := newStreamRig(t, nil)
	rg.svc.result = service.ReplayResult{Events: []domain.Event{alertEvent(testOrg, 100)}, LastSeq: 100}

	resp, cancel := rg.open(t, "", map[string]string{"Last-Event-ID": "99"})
	defer resp.Body.Close()
	defer cancel()

	frames := readFrames(resp.Body)

	first := next(t, frames)
	if first.ID != "100" {
		t.Fatalf("first frame id = %q, want the replayed 100", first.ID)
	}

	// Nothing else had been written when that frame arrived. Now something happens
	// in the world.
	rg.hub.Publish([]domain.Event{alertEvent(testOrg, 101)})

	live := next(t, frames)
	if live.ID != "101" {
		t.Fatalf("live frame id = %q, want 101 — the connection is not delivering live events",
			live.ID)
	}
}

// ⛔ TestAnEventThatArrivedDuringTheReplayIsDeliveredExactlyOnce.
//
// The handler subscribes BEFORE it replays, so an event committed while the
// replay query runs is buffered rather than lost — and is then de-duplicated
// against the replay watermark. Both halves are needed and neither is visible
// from the outside without forcing the race, which is what the `during` hook does.
//
// Replay-then-subscribe loses a hole exactly the width of the query; subscribe
// without the watermark delivers every such event twice. In a UI driven by
// upserts the second is survivable and the first is not, which is why the code
// chooses this order — and why both need pinning.
func TestAnEventThatArrivedDuringTheReplayIsDeliveredExactlyOnce(t *testing.T) {
	t.Parallel()

	rg := newStreamRig(t, nil)
	replayed := alertEvent(testOrg, 500)
	rg.svc.result = service.ReplayResult{Events: []domain.Event{replayed}, LastSeq: 500}
	rg.svc.during = func() {
		// Committed while the replay query is in flight: once as a row the replay
		// will return, and once as a live publication.
		rg.hub.Publish([]domain.Event{replayed, alertEvent(testOrg, 501)})
	}

	resp, cancel := rg.open(t, "", map[string]string{"Last-Event-ID": "499"})
	defer resp.Body.Close()
	defer cancel()

	frames := readFrames(resp.Body)

	if got := next(t, frames); got.ID != "500" {
		t.Fatalf("first frame id = %q, want the replayed 500", got.ID)
	}
	// 501 committed during the replay. It is not in the replay result, and it must
	// not have been dropped on the floor.
	second := next(t, frames)
	if second.ID != "501" {
		t.Fatalf("second frame id = %q, want 501 — an event that committed during the replay was %s",
			second.ID,
			"either lost (replay-then-subscribe) or duplicated (no watermark)")
	}
}

// ⛔ TestAResumeReplaysOnlyTheResourcesTheClientSubscribedTo.
//
// `?resources=` narrowed the LIVE feed and nothing else. Hub.Publish consults
// Interest.Matches on the way out; the replay path consulted nothing, so a tab
// watching `resources=alerts` saw alert frames for as long as its socket stayed
// up and then, on the first reconnect, the entire log — cases, alert
// events, groups — arriving as `alert.upserted` listeners that never fire and
// payloads the store has no reducer for.
//
// The shape below is the one observed against a running server: resume from 10
// over a gap of 11..16 that holds exactly one alert. The client asked for alerts.
// It gets one frame.
//
// The gap is deliberately arranged so that the alert is NOT last: 13..16 come
// after it, which is what makes the cursor assertion at the end meaningful rather
// than accidentally satisfied by the alert's own id.
func TestAResumeReplaysOnlyTheResourcesTheClientSubscribedTo(t *testing.T) {
	t.Parallel()

	rg := newStreamRig(t, nil)
	rg.svc.result = service.ReplayResult{
		Events: []domain.Event{
			otherEvent(t, testOrg, 11, domain.KindCaseUpserted),
			alertEvent(testOrg, 12),
			otherEvent(t, testOrg, 13, domain.KindEventAppended),
			otherEvent(t, testOrg, 14, domain.KindEventAppended),
			otherEvent(t, testOrg, 15, domain.KindEventAppended),
			otherEvent(t, testOrg, 16, domain.KindGroupUpserted),
		},
		LastSeq: 16,
	}

	resp, cancel := rg.open(t, "?resources=alerts", map[string]string{"Last-Event-ID": "10"})
	defer resp.Body.Close()
	defer cancel()

	frames := readFrames(resp.Body)

	// The sequence contract is untouched by the filter: the handler still resolved
	// the cursor as 10 and the replay still means `seq > 10`. Narrowing the stream
	// must never narrow the RANGE, or a filtered client would also be a client with
	// a hole in it.
	if rg.svc.since != 10 {
		t.Fatalf("the handler replayed from %d, want 10 — a resume from N must still ask for N+1 onwards",
			rg.svc.since)
	}

	first := next(t, frames)
	if first.ID != "12" || first.Event != string(domain.KindAlertUpserted) {
		t.Fatalf("first frame is id %q event %q, want id 12 event %s — "+
			"the replay is ignoring `resources=alerts` and shipping every kind in the gap",
			first.ID, first.Event, domain.KindAlertUpserted)
	}

	// ⛔ And the cursor lands on 16, not on 12. See the next test for why that is
	// the difference between a reconnect that terminates and one that does not.
	cursor := next(t, frames)
	if cursor.Data != "" || cursor.Event != "" {
		t.Fatalf("frame after the replay is event %q data %q, want the id-only cursor line — "+
			"a filtered-out event was delivered after all", cursor.Event, cursor.Data)
	}
	if !cursor.HasID || cursor.ID != "16" {
		t.Fatalf("cursor line = %q (present=%v), want 16 — the highest seq in the gap, "+
			"filtered or not", cursor.ID, cursor.HasID)
	}

	// The live phase resumes from the true watermark, not from 12: the filtered ids
	// are behind the connection, not pending in front of it.
	rg.hub.Publish([]domain.Event{alertEvent(testOrg, 17)})
	if f := next(t, frames); f.ID != "17" {
		t.Fatalf("live frame id = %q, want 17", f.ID)
	}
}

// ⛔ TestAResumeAdvancesTheClientsCursorPastTheEventsItFilteredOut.
//
// This is the half of the filter that is easy to get wrong in the direction that
// looks correct. The server's `watermark` is not the client's cursor: the client
// resumes from the last `id:` LINE IT RECEIVED. Filter frames away and that value
// stops moving, so every reconnect hands back the same stale id, and the gap oto
// is asked to replay only grows — in a busy org with a narrow filter it crosses
// MaxReplayRows and the connection resyncs forever over events the client never
// wanted. Putting the filter in the SQL `WHERE` produces exactly this, and it
// passes a test that only checks which frames arrive.
//
// So the gap here contains NOTHING the client asked for. There are no data frames
// to hide behind: the only thing that may appear on the wire is a bare `id:` line
// carrying the highest seq in the gap, which moves EventSource.lastEventId without
// dispatching an event to the page.
func TestAResumeAdvancesTheClientsCursorPastTheEventsItFilteredOut(t *testing.T) {
	t.Parallel()

	rg := newStreamRig(t, nil)
	rg.svc.result = service.ReplayResult{
		Events: []domain.Event{
			otherEvent(t, testOrg, 11, domain.KindCaseUpserted),
			otherEvent(t, testOrg, 12, domain.KindEventAppended),
			otherEvent(t, testOrg, 13, domain.KindDeliveryUpdated),
		},
		LastSeq: 13,
	}

	resp, cancel := rg.open(t, "?resources=alerts", map[string]string{"Last-Event-ID": "10"})
	defer resp.Body.Close()
	defer cancel()

	frames := readFrames(resp.Body)

	f := next(t, frames)
	if f.Data != "" {
		t.Fatalf("a frame with data arrived for an alerts-only client over a gap holding no alerts: "+
			"event=%q data=%s", f.Event, f.Data)
	}
	if !f.HasID {
		t.Fatal("the only thing on the wire carries no `id:`, so the client's cursor is still at 10; " +
			"it will ask for 11 again on every reconnect, forever")
	}
	if f.ID != "13" {
		t.Fatalf("cursor = %q, want 13 — the cursor must clear the WHOLE gap, not the part of it "+
			"this subscriber happened to want", f.ID)
	}

	// Nothing else follows: the cursor line is one block, not a frame with an empty
	// body, and it must not be repeated per filtered event.
	rg.hub.Publish([]domain.Event{alertEvent(testOrg, 14)})
	if live := next(t, frames); live.ID != "14" || live.Event != string(domain.KindAlertUpserted) {
		t.Fatalf("next frame is id %q event %q, want the live alert 14 — the replay emitted "+
			"more than one cursor line", live.ID, live.Event)
	}
}

// ⛔ TestAFilteredResumeNeverEmitsACursorLineAfterAResync.
//
// The two mechanisms are opposites and they must not meet. A resync carries no
// `id:` precisely so a reconnect cannot resume PAST the events it is being told to
// refetch; a cursor line does nothing but move the client past events. Emitting
// one after the other would convert "refetch everything, you have a hole" into
// "you are up to date", silently, which is the worst outcome in the whole feature.
func TestAFilteredResumeNeverEmitsACursorLineAfterAResync(t *testing.T) {
	t.Parallel()

	rg := newStreamRig(t, nil)
	rg.svc.result = service.ReplayResult{Resync: domain.ResyncReplayWindowExceeded, LastSeq: 10}

	resp, cancel := rg.open(t, "?resources=alerts", map[string]string{"Last-Event-ID": "10"})
	defer resp.Body.Close()
	defer cancel()

	frames := readFrames(resp.Body)

	if f := next(t, frames); f.Event != string(domain.KindResync) || f.HasID {
		t.Fatalf("first frame is event %q (id present=%v), want an id-less resync", f.Event, f.HasID)
	}
	// If a cursor line followed the resync it would be the next block on the wire.
	rg.hub.Publish([]domain.Event{alertEvent(testOrg, 11)})
	f := next(t, frames)
	if f.Data == "" {
		t.Fatalf("a bare cursor line (id %q) followed the resync; the client would reconnect from "+
			"there and never refetch the gap the resync exists to announce", f.ID)
	}
	if f.ID != "11" {
		t.Fatalf("frame after the resync is id %q, want the live alert 11", f.ID)
	}
}

// ⭐ TestAResyncCarriesNoIdSoAReconnectCannotSkipWhatItMustRefetch.
//
// A resync says "everything you hold is suspect, refetch". If it carried an `id:`,
// EventSource would remember it, and the reconnect would resume from a cursor PAST
// the events the client was just told it never received — turning a recoverable
// gap into permanent, silent staleness.
func TestAResyncCarriesNoIdSoAReconnectCannotSkipWhatItMustRefetch(t *testing.T) {
	t.Parallel()

	rg := newStreamRig(t, nil)
	rg.svc.result = service.ReplayResult{Resync: domain.ResyncReplayWindowExceeded, LastSeq: 918273}

	resp, cancel := rg.open(t, "", map[string]string{"Last-Event-ID": "918273"})
	defer resp.Body.Close()
	defer cancel()

	f := next(t, readFrames(resp.Body))

	if f.HasID {
		t.Fatalf("the resync frame carries `id: %s`; a reconnect would resume past the events "+
			"this frame exists to tell the client it is missing", f.ID)
	}
	if f.Event != string(domain.KindResync) {
		t.Fatalf("event = %q, want resync", f.Event)
	}

	var v any
	if err := json.Unmarshal([]byte(f.Data), &v); err != nil {
		t.Fatalf("the resync data: line is not JSON: %v (%q)", err, f.Data)
	}
	if err := schema.Component(t, "StreamFrame").Validate(v); err != nil {
		t.Fatalf("the resync frame does not satisfy StreamFrame:\n%v\ndata = %s", err, f.Data)
	}

	var payload struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(f.Data), &payload); err != nil {
		t.Fatalf("decode resync frame: %v", err)
	}
	var pv any
	if err := json.Unmarshal(payload.Data, &pv); err != nil {
		t.Fatalf("decode resync payload: %v", err)
	}
	if err := schema.Component(t, "ResyncData").Validate(pv); err != nil {
		t.Fatalf("the resync payload is not a ResyncData the client can switch on:\n%v\n%s",
			err, payload.Data)
	}
}

// ⛔ TestBUG_AResyncOnAFreshConnectionCarriesSeqZero.
//
// The contract types `StreamFrame.seq` as `minimum: 1`, and the generated
// TypeScript client validates frames at runtime — a frame that fails validation
// is DROPPED, which for a resync means the one instruction that tells the client
// to refetch is the one instruction it never sees.
//
// What the server does: a connection that opens with no Last-Event-ID gets
// `ReplayResult{}` (stream.go:117), so `watermark` is 0 (stream.go:139). If the
// hub overflows that connection before any event has been written, pump calls
// writeResync with that watermark (stream.go:191) and dto.go:59 emits `"seq":0`.
// That is reachable: an overflow needs only a storm arriving in the window
// between Subscribe and the first live batch.
//
// What the contract says: seq >= 1.
//
// The CONTRACT is right about the constraint and the SERVER is right that it has
// no sequence to name — a resync is a statement about the stream, not a position
// in it. The fix belongs in the contract or in the frame (omit `seq` for resync,
// as `resource` and `id` already are), not in this test. Skipped until that is
// decided; the assertion below is written against the contract as published.
func TestBUG_AResyncOnAFreshConnectionCarriesSeqZero(t *testing.T) {
	t.Skip("live divergence: a resync with no replay watermark emits seq:0, which StreamFrame " +
		"declares as minimum:1 — see the comment above for which side should move")

	rg := newStreamRig(t, nil)
	// A fresh EventSource: no Last-Event-ID, so no replay and no watermark.
	rg.svc.result = service.ReplayResult{}

	resp, cancel := rg.open(t, "", nil)
	defer resp.Body.Close()
	defer cancel()

	frames := readFrames(resp.Body)

	// Overflow the connection before anything has been delivered.
	burst := make([]domain.Event, 0, service.DefaultBufferSize*2)
	for i := range cap(burst) {
		burst = append(burst, alertEvent(testOrg, int64(i+1)))
	}
	rg.hub.Publish(burst)

	for {
		f := next(t, frames)
		if f.Event != string(domain.KindResync) {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(f.Data), &v); err != nil {
			t.Fatalf("the resync data: line is not JSON: %v", err)
		}
		if err := schema.Component(t, "StreamFrame").Validate(v); err != nil {
			t.Fatalf("the resync frame does not satisfy StreamFrame:\n%v\ndata = %s", err, f.Data)
		}
		return
	}
}

// ⛔ TestTheStreamNeverWidensPastTheCallersOwnOrg.
//
// Org scoping is applied server-side from the authenticated principal and no
// query parameter can widen it. A frame belonging to another tenant on this
// socket is a cross-tenant leak that arrives in a browser, live, with no audit
// trail — the worst shape a leak can have.
func TestTheStreamNeverWidensPastTheCallersOwnOrg(t *testing.T) {
	t.Parallel()

	rg := newStreamRig(t, nil)
	rg.svc.result = service.ReplayResult{}

	resp, cancel := rg.open(t, "", nil)
	defer resp.Body.Close()
	defer cancel()

	frames := readFrames(resp.Body)

	// The stranger's event is published FIRST, so if scoping leaked it would be
	// the first frame off the wire rather than a straggler a timeout could hide.
	rg.hub.Publish([]domain.Event{alertEvent(testOtherOrg, 7)})
	rg.hub.Publish([]domain.Event{alertEvent(testOrg, 8)})

	f := next(t, frames)
	if f.ID != "8" {
		t.Fatalf("the first frame is id %q; another tenant's event reached this connection", f.ID)
	}
	var frame struct {
		OrgID string `json:"org_id"`
	}
	if err := json.Unmarshal([]byte(f.Data), &frame); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if frame.OrgID != testOrg.String() {
		t.Fatalf("org_id = %q, want %q", frame.OrgID, testOrg)
	}
}

// TestAnUnknownResourceSelectorIsRefusedBeforeTheStreamOpens.
//
// A typo in `?resources=` must not be silently ignored (§E.3): a stream quietly
// narrowed to nothing looks exactly like a quiet system. The refusal has to
// happen BEFORE the 200 is committed, because after that there is no way to send
// a problem document — it would be appended to a text/event-stream body.
func TestAnUnknownResourceSelectorIsRefusedBeforeTheStreamOpens(t *testing.T) {
	t.Parallel()

	rg := newStreamRig(t, nil)

	resp, cancel := rg.open(t, "?resources=alerts,banana", nil)
	defer resp.Body.Close()
	defer cancel()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q, want problem+json; a refusal must not be labelled as a stream", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	schema.AssertProblem(t, "streamEvents", http.StatusUnprocessableEntity, body)
	if !strings.Contains(string(body), "resources") {
		t.Fatalf("the violation should name the parameter: %s", body)
	}
	if rg.svc.calls != 0 {
		t.Fatal("a refused request still ran a replay query")
	}
}

// TestAMalformedAlertIdIsRefusedRatherThanIgnored, for the same reason: a filter
// oto could not parse and silently dropped is a stream showing the wrong rows.
func TestAMalformedAlertIdIsRefusedRatherThanIgnored(t *testing.T) {
	t.Parallel()

	rg := newStreamRig(t, nil)

	resp, cancel := rg.open(t, "?alert_id=banana", nil)
	defer resp.Body.Close()
	defer cancel()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	schema.AssertProblem(t, "streamEvents", http.StatusUnprocessableEntity, body)
	if !strings.Contains(string(body), "alert_id") {
		t.Fatalf("the violation should name the parameter: %s", body)
	}
}

// ⭐ TestAMalformedLastEventIDAttachesLiveInsteadOfRefusing.
//
// Last-Event-ID is set by EventSource, not by the user, and the user cannot see
// or clear it. Refusing the connection over a value nobody chose would leave the
// UI permanently dark with no way out but a hard reload — so a header that does
// not parse degrades to "no cursor", which is the safe direction.
func TestAMalformedLastEventIDAttachesLiveInsteadOfRefusing(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"banana", "-1", "9223372036854775808", "  "} {
		t.Run(strconv.Quote(raw), func(t *testing.T) {
			t.Parallel()

			rg := newStreamRig(t, nil)
			rg.svc.result = service.ReplayResult{}

			resp, cancel := rg.open(t, "", map[string]string{"Last-Event-ID": raw})
			defer resp.Body.Close()
			defer cancel()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 — a header the user cannot see must not close the UI",
					resp.StatusCode)
			}

			// Prove the connection really attached: publish and read.
			frames := readFrames(resp.Body)
			rg.hub.Publish([]domain.Event{alertEvent(testOrg, 3)})
			if f := next(t, frames); f.ID != "3" {
				t.Fatalf("frame id = %q, want 3", f.ID)
			}
			if rg.svc.since != 0 {
				t.Fatalf("the handler resolved a cursor of %d from %q; an unparseable header is no cursor",
					rg.svc.since, raw)
			}
		})
	}
}

// TestAReplayFailureIsARefusalAndNotAnEmptyStream.
//
// A replay that could not run must not present as "nothing has happened". oto's
// silence has to stay distinguishable from an answer, and here the distinction is
// the difference between a UI that retries and a UI that sits confidently empty.
func TestAReplayFailureIsARefusalAndNotAnEmptyStream(t *testing.T) {
	t.Parallel()

	rg := newStreamRig(t, nil)
	rg.svc.err = errs.New(errs.KindInternal, "stream_replay_failed", "could not read ui_events")

	resp, cancel := rg.open(t, "", map[string]string{"Last-Event-ID": "10"})
	defer resp.Body.Close()
	defer cancel()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — a failed replay must not render as a healthy empty stream",
			resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Fatal("the failure was labelled as a stream; the client would wait forever for frames")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	schema.AssertProblem(t, "streamEvents", http.StatusInternalServerError, body)
}

// TestAnUnauthenticatedCallerNeverReachesTheHub.
//
// The scope resolver is the only thing standing between an anonymous socket and a
// tenant's live feed, and its refusal must happen before Subscribe — a
// subscription made for a caller with no org is a connection nothing can scope.
func TestAnUnauthenticatedCallerNeverReachesTheHub(t *testing.T) {
	t.Parallel()

	rg := newStreamRig(t, func(context.Context) (db.TenantScope, error) {
		return db.TenantScope{}, errs.Unauthorized("unauthenticated", "authentication is required")
	})

	resp, cancel := rg.open(t, "", nil)
	defer resp.Body.Close()
	defer cancel()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	schema.AssertProblem(t, "streamEvents", http.StatusUnauthorized, body)
	if rg.svc.calls != 0 {
		t.Fatal("an unauthenticated request still ran a replay query")
	}
	if n := rg.hub.Subscribers(); n != 0 {
		t.Fatalf("%d subscriber(s) attached for a caller with no org", n)
	}
}

// TestTheConnectionIsReleasedWhenTheClientHangsUp.
//
// The handler is meant to live for hours, which makes leaking one a slow, silent
// resource exhaustion: every reload of a dashboard would add a subscription the
// hub fans every event out to forever.
func TestTheConnectionIsReleasedWhenTheClientHangsUp(t *testing.T) {
	t.Parallel()

	rg := newStreamRig(t, nil)
	rg.svc.result = service.ReplayResult{Events: []domain.Event{alertEvent(testOrg, 1)}, LastSeq: 1}

	resp, cancel := rg.open(t, "", map[string]string{"Last-Event-ID": "0"})
	defer resp.Body.Close()
	frames := readFrames(resp.Body)
	_ = next(t, frames) // attached

	if n := rg.hub.Subscribers(); n != 1 {
		t.Fatalf("subscribers = %d, want 1 while the client is attached", n)
	}

	cancel()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rg.hub.Subscribers() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("subscribers = %d five seconds after the client hung up; the handler leaked its subscription",
		rg.hub.Subscribers())
}

// TestTheHeartbeatCadenceIsTheOneTheContractPublishes.
//
// The contract tells clients a `: ping` arrives every 15 seconds, and a proxy's
// idle-reap window is tuned against that number. Waiting 15 s in a unit test buys
// nothing, so what is pinned is the constant itself: it is part of the published
// API, and "tuning" it is an API change rather than a configuration change.
func TestTheHeartbeatCadenceIsTheOneTheContractPublishes(t *testing.T) {
	t.Parallel()

	if service.HeartbeatInterval != 15*time.Second {
		t.Fatalf("HeartbeatInterval = %s, want 15s — openapi `streamEvents` promises a ping every 15 seconds",
			service.HeartbeatInterval)
	}
	// And the heartbeat is a COMMENT, not a frame: `heartbeat` is deliberately
	// absent from UiEventKind, so a client that treated it as one would be
	// switching on a kind the contract does not define.
	var kind any = "heartbeat"
	if err := schema.Component(t, "UiEventKind").Validate(kind); err == nil {
		t.Fatal("UiEventKind now accepts `heartbeat`; the heartbeat is a bare comment line, not a frame")
	}
}

// TestOneBadFrameDoesNotEndAnOtherwiseHealthyStream.
//
// writeEvent swallows a marshal failure on purpose: a single unencodable payload
// is a bug in one producer, and hanging up would take down every other resource
// the tab is watching with it. The frame is dropped, the stream continues.
func TestOneBadFrameDoesNotEndAnOtherwiseHealthyStream(t *testing.T) {
	t.Parallel()

	rg := newStreamRig(t, nil)
	broken := alertEvent(testOrg, 10)
	// json.RawMessage marshals its bytes verbatim and fails on invalid JSON, which
	// is exactly the "one producer wrote nonsense" case.
	broken.Payload = json.RawMessage(`{not json`)
	rg.svc.result = service.ReplayResult{
		Events:  []domain.Event{broken, alertEvent(testOrg, 11)},
		LastSeq: 11,
	}

	resp, cancel := rg.open(t, "", map[string]string{"Last-Event-ID": "9"})
	defer resp.Body.Close()
	defer cancel()

	f := next(t, readFrames(resp.Body))
	if f.ID != "11" {
		t.Fatalf("first readable frame id = %q, want 11 — the healthy frame after the broken one", f.ID)
	}
}

// TestTheScopeResolverIsTheOnlySourceOfTheTenant is a compile-shaped guard with a
// runtime symptom: the port takes a context and returns a scope, so no query
// parameter, header or path segment can ever contribute to it.
func TestTheScopeResolverIsTheOnlySourceOfTheTenant(t *testing.T) {
	t.Parallel()

	var called int
	rg := newStreamRig(t, func(context.Context) (db.TenantScope, error) {
		called++
		return mustScope(t, testOrg), nil
	})
	rg.svc.result = service.ReplayResult{}

	// Every widening a caller could plausibly attempt, in one request.
	resp, cancel := rg.open(t, "?org_id="+testOtherOrg.String()+"&resources=alerts", map[string]string{
		"X-Org-Id": testOtherOrg.String(),
	})
	defer resp.Body.Close()
	defer cancel()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	frames := readFrames(resp.Body)
	rg.hub.Publish([]domain.Event{alertEvent(testOtherOrg, 1), alertEvent(testOrg, 2)})
	if f := next(t, frames); f.ID != "2" {
		t.Fatalf("frame id = %q; a query parameter widened the tenant scope", f.ID)
	}
	if called != 1 {
		t.Fatalf("the scope resolver was consulted %d times, want exactly 1", called)
	}
}
