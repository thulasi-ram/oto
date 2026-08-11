package httpx_test

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/platform/httpx"
)

// TestSSEWriterSendsTheDeclaredHeaders is a regression test for a shipped bug in
// which NewSSEWriter flushed BEFORE setting its headers. The stream framed
// perfectly and every header was a no-op on a committed response, so the wire
// carried a 200 with no Content-Type at all: a browser EventSource rejects that
// and fires onerror, which made the entire live UI unreachable while every log
// line said the stream was healthy.
//
// The four headers are the contract (openapi.yaml `streamEvents`, SPEC §E.4):
// Content-Type identifies the stream, no-transform stops a proxy gzipping it into
// buffered bursts, keep-alive holds the connection, and X-Accel-Buffering: no is
// the only thing that turns off nginx's proxy buffering — the reason SSE
// classically "works locally, not in prod".
//
// It runs against a real net/http server rather than a ResponseRecorder on
// purpose: a recorder cannot tell a header that was set from a header that was
// sent, which is the exact distinction the bug turned on.
func TestSSEWriterSendsTheDeclaredHeaders(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sse, err := httpx.NewSSEWriter(w)
		if err != nil {
			t.Errorf("NewSSEWriter: %v", err)
			return
		}
		if err := sse.Comment("ping"); err != nil {
			t.Errorf("Comment: %v", err)
			return
		}
		if err := sse.EventSeq(42, "alert.acked", []byte(`{"seq":42}`)); err != nil {
			t.Errorf("EventSeq: %v", err)
		}
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

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
		if h == "Connection" && got == "" {
			// net/http manages Connection itself on HTTP/1.1 keep-alive and strips
			// the hop-by-hop header from the parsed response; the wire carries it.
			continue
		}
		t.Errorf("header %s = %q, want %q — a browser EventSource rejects the response "+
			"outright when Content-Type is wrong, and buffers forever without X-Accel-Buffering",
			h, got, v)
	}
}

// TestSSEWriterFramesEvents pins the frame syntax. `id:` is what the client
// echoes back as Last-Event-ID, so a missing or reordered field is a resume that
// silently replays or silently skips.
func TestSSEWriterFramesEvents(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sse, err := httpx.NewSSEWriter(w)
		if err != nil {
			t.Errorf("NewSSEWriter: %v", err)
			return
		}
		_ = sse.Retry(3 * time.Second)
		_ = sse.Comment("ping")
		_ = sse.EventSeq(7, "alert.acked", []byte(`{"seq":7}`))
		_ = sse.Event("", "resync", []byte(`{"reason":"gap"}`))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL) //nolint:noctx // the server closes the stream itself
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(bufio.NewReader(resp.Body))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	want := "retry: 3000\n\n" +
		": ping\n\n" +
		"id: 7\nevent: alert.acked\ndata: {\"seq\":7}\n\n" +
		"event: resync\ndata: {\"reason\":\"gap\"}\n\n"

	if got := string(body); got != want {
		t.Errorf("stream body mismatch\n got: %q\nwant: %q", got, want)
	}
	if strings.Contains(string(body), "id: \n") {
		t.Error("an empty id: line moves the client's cursor to nowhere; omit the field instead")
	}
}

// ⛔ TestSSEWriterCursorMovesTheClientWithoutDeliveringAFrame pins the exact bytes
// of the id-only block, because its whole behaviour depends on them.
//
// The EventSource parser assigns the last event ID from its buffer at the top of
// the dispatch step and only THEN returns early on an empty data buffer, so
// `id: N\n\n` moves the cursor and dispatches nothing. Add a `data:` line — even
// an empty one — and the block becomes a real message event with an empty body,
// which every `onmessage` handler in the client would try to parse as a frame. Add
// an `event:` line and a typed listener fires with no payload. Neither is a thing
// the client can survive, and neither would be caught by anything that only checks
// that the cursor advanced.
func TestSSEWriterCursorMovesTheClientWithoutDeliveringAFrame(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sse, err := httpx.NewSSEWriter(w)
		if err != nil {
			t.Errorf("NewSSEWriter: %v", err)
			return
		}
		_ = sse.Cursor(16)
		_ = sse.EventSeq(17, "alert.upserted", []byte(`{"seq":17}`))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL) //nolint:noctx // the server closes the stream itself
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(bufio.NewReader(resp.Body))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	want := "id: 16\n\n" +
		"id: 17\nevent: alert.upserted\ndata: {\"seq\":17}\n\n"
	if got := string(body); got != want {
		t.Errorf("a cursor line must be an `id:` and nothing else\n got: %q\nwant: %q", got, want)
	}
}
