package httpx

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// ContentTypeEventStream is the media type of an SSE response (SPEC §E.4).
const ContentTypeEventStream = "text/event-stream; charset=utf-8"

// ErrStreamingUnsupported is returned when the ResponseWriter cannot flush, which
// makes SSE impossible: an unflushed frame sits in the write buffer until the
// buffer fills, which for a 200-byte change notice is approximately never.
var ErrStreamingUnsupported = errors.New("httpx: response writer does not support flushing")

// SSEWriter frames Server-Sent Events onto a response.
//
// It is not safe for concurrent use: one goroutine owns the connection, which is
// also what keeps frame ordering — and therefore Last-Event-ID correctness —
// trivially true.
type SSEWriter struct {
	w   http.ResponseWriter
	rc  *http.ResponseController
	buf bytes.Buffer
}

// NewSSEWriter writes the SSE response headers and returns a framer.
//
// The header set is binding (SPEC §E.4, openapi.yaml `streamEvents`):
//
//   - Content-Type: text/event-stream; charset=utf-8
//   - Cache-Control: no-cache, no-transform — `no-transform` stops a proxy
//     "helpfully" gzipping and therefore buffering the stream.
//   - Connection: keep-alive
//   - X-Accel-Buffering: no — nginx buffers proxied responses by default, which
//     turns a live stream into bursts arriving minutes late. This header is the
//     only thing that turns it off, and it is why SSE "works locally, not in prod".
//
// ⛔ ORDER IS LOAD-BEARING. Every header MUST be set before the first flush.
// The first flush commits the status line and the header block to the wire; a
// `Header().Set` afterwards mutates a map nobody will ever read, silently. That
// is exactly how this function shipped a 200 with no Content-Type at all, which
// a browser EventSource rejects outright — the stream framed correctly and the
// live UI still could not attach. Flushability is therefore probed LAST, and the
// probe doubles as the flush that sends the headers.
func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, error) {
	rc := http.NewResponseController(w)

	// Clear the server's write deadline. http.Server.WriteTimeout is a per-request
	// budget, and a stream that is meant to live for hours would otherwise be cut
	// at 30 seconds — a bug that presents as "SSE randomly reconnects" and is
	// almost never traced back to the server config. Best-effort: a wrapped
	// ResponseWriter that does not support it simply keeps the deadline.
	_ = rc.SetWriteDeadline(time.Time{})

	h := w.Header()
	h.Set("Content-Type", ContentTypeEventStream)
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	w.WriteHeader(http.StatusOK)

	// The first flush both commits the header block above and proves the writer
	// can stream at all. If it cannot, the response is already committed as a 200
	// — but an SSEWriter that cannot flush is useless, so the caller is told, and
	// the caller's only remaining move is to close the connection.
	if err := rc.Flush(); err != nil {
		return nil, ErrStreamingUnsupported
	}

	return &SSEWriter{w: w, rc: rc}, nil
}

// Event writes one frame and flushes it.
//
// id may be empty for a frame that must not move the client's Last-Event-ID
// cursor. name is the SSE `event:` field. data is written verbatim as a single
// `data:` line, so it MUST NOT contain a newline — every oto frame is compact
// JSON, which never does.
func (s *SSEWriter) Event(id, name string, data []byte) error {
	s.buf.Reset()
	if id != "" {
		s.buf.WriteString("id: ")
		s.buf.WriteString(id)
		s.buf.WriteByte('\n')
	}
	if name != "" {
		s.buf.WriteString("event: ")
		s.buf.WriteString(name)
		s.buf.WriteByte('\n')
	}
	s.buf.WriteString("data: ")
	s.buf.Write(data)
	s.buf.WriteString("\n\n")

	if _, err := s.w.Write(s.buf.Bytes()); err != nil {
		return fmt.Errorf("httpx: write sse frame: %w", err)
	}
	return s.flush()
}

// EventSeq is Event with an int64 id, which is the shape every oto frame uses:
// the `ui_events.seq` the client will echo back in Last-Event-ID.
func (s *SSEWriter) EventSeq(seq int64, name string, data []byte) error {
	return s.Event(strconv.FormatInt(seq, 10), name, data)
}

// Cursor advances the client's Last-Event-ID WITHOUT delivering a frame.
//
// It writes an `id:` line and nothing else — no `event:`, no `data:`. That is not
// a malformed frame, it is the one mechanism the EventSource specification gives
// for moving a cursor over something the client is not being told about: on the
// blank line the parser assigns the last event ID from its buffer and only THEN
// returns early because the data buffer is empty. The cursor moves; no event is
// dispatched; the page never sees it.
//
// ⛔ IT IS ONLY EVER CORRECT WHEN THE SKIPPED SEQS ARE ONES THE CLIENT MUST NOT
// ASK FOR AGAIN — a subscriber filter it declared, not a gap oto failed to
// deliver. Emitting it over events the client is missing is exactly the mistake
// `writeResync` avoids by carrying no id at all: the reconnect would resume past
// the very rows it needed.
func (s *SSEWriter) Cursor(seq int64) error {
	if _, err := fmt.Fprintf(s.w, "id: %d\n\n", seq); err != nil {
		return fmt.Errorf("httpx: write sse cursor: %w", err)
	}
	return s.flush()
}

// Comment writes a bare comment line. This is the heartbeat (`: ping`): it is not
// a frame, carries no data and does not move the client's cursor, but it does
// make the write fail promptly once the peer is gone and keeps intermediaries
// from reaping an idle connection (SPEC §E.4).
func (s *SSEWriter) Comment(text string) error {
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return fmt.Errorf("httpx: write sse comment: %w", err)
	}
	return s.flush()
}

// Retry advertises the client's reconnect delay.
func (s *SSEWriter) Retry(d time.Duration) error {
	if _, err := fmt.Fprintf(s.w, "retry: %d\n\n", d.Milliseconds()); err != nil {
		return fmt.Errorf("httpx: write sse retry: %w", err)
	}
	return s.flush()
}

func (s *SSEWriter) flush() error {
	if err := s.rc.Flush(); err != nil {
		return fmt.Errorf("httpx: flush sse: %w", err)
	}
	return nil
}
