package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/thulasiram/oto/internal/platform/clock"
)

// ⭐⭐ THE SIGNATURE IS THE ONLY THING STANDING BETWEEN THIS ENDPOINT AND THE
// INTERNET.
//
// It is mounted outside every authenticator oto has — Slack carries no session
// and no PAT — so `verifySlack` IS the authentication. A hole here is not "an
// endpoint bug": it is anybody on the internet acknowledging anybody's alert,
// which would make oto's only human-facing verb forgeable.
//
// There is no signing secret available to this repository and there never will
// be one in a test, so every case below computes the HMAC itself over the exact
// bytes the request carries. That is not a limitation — it is the point: the
// tests prove the ALGORITHM against Slack's published contract, and a real
// secret would prove nothing extra.

const testSigningSecret = "8f742231b10e8888abcd99yyyzzz85a5" //nolint:gosec // a fixture, not a credential

// signedForm builds a form body and the two headers Slack sends with it.
func signedForm(secret, payload string, ts time.Time) (body string, headers map[string]string) {
	body = url.Values{"payload": {payload}}.Encode()
	stamp := strconv.FormatInt(ts.Unix(), 10)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + stamp + ":" + body))

	return body, map[string]string{
		slackTimestampHeader: stamp,
		slackSignatureHeader: "v0=" + hex.EncodeToString(mac.Sum(nil)),
		"Content-Type":       "application/x-www-form-urlencoded",
	}
}

// blockActionPayload is a realistic `block_actions` envelope.
func blockActionPayload(actionID, value string) string {
	return `{
	  "type": "block_actions",
	  "team": {"id": "T9TK3CUKW", "domain": "acme"},
	  "user": {"id": "U0123456789", "username": "ram", "name": "ram"},
	  "channel": {"id": "C0123456789", "name": "sre-alerts"},
	  "container": {"type": "message", "message_ts": "1712345678.000100"},
	  "response_url": "https://hooks.slack.com/actions/T9TK3CUKW/1/2",
	  "actions": [{"action_id": "` + actionID + `", "type": "button", "value": "` + value + `"}]
	}`
}

// captor records what the consumer was handed, and can be told to fail.
type captor struct {
	mu      sync.Mutex
	calls   int
	payload json.RawMessage
	err     error
	block   chan struct{}
	handled chan struct{}
}

// newCaptor returns a captor whose completed calls can be WAITED ON.
//
// ⛔ THE 200 IS FLUSHED BEFORE THE CONSUMER RUNS, and that ordering is the whole
// point of the endpoint (see `receiveSlackInteraction`). It also means the
// client's `Do` can return while the dispatch that follows the flush has not
// been scheduled yet — the request is finished on the wire and the handler
// goroutine is still between the flush and the call.
//
// A test that reads `seen()` the instant `postRaw` returns is therefore racing
// the very ordering it is meant to rely on. It wins that race on an idle laptop
// and loses it on a loaded CI runner, where it reports "the consumer saw 0
// payloads" for a request that was accepted correctly and dispatched a moment
// later. Every assertion that the consumer WAS reached waits on the signal
// below instead.
func newCaptor() *captor {
	return &captor{handled: make(chan struct{}, 8)}
}

func (c *captor) Handle(ctx context.Context, payload json.RawMessage) error {
	if c.block != nil {
		select {
		case <-c.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	c.calls++
	c.payload = payload
	err := c.err
	c.mu.Unlock()

	// Signalled AFTER the call is recorded, so anything woken by it observes the
	// state it was waiting for. Non-blocking: a captor nobody waits on must not
	// wedge the handler.
	select {
	case c.handled <- struct{}{}:
	default:
	}
	return err
}

func (c *captor) seen() (int, json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.payload
}

// awaitCalls blocks until the consumer has completed n calls.
//
// The wait is on the captor's own signal, never on a duration: a slow machine
// makes this test SLOWER and never makes it red. The deadline exists only so a
// consumer that is genuinely never reached fails here, with the count it did
// see, rather than hanging until the package timeout prints a goroutine dump.
func (c *captor) awaitCalls(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-c.handled:
		case <-time.After(30 * time.Second):
			calls, _ := c.seen()
			t.Fatalf("the consumer saw %d payloads, want %d", calls, n)
		}
	}
}

// newInteractionServer mounts the real route table over a router with a fixed
// clock, so the replay window is a fact a test can pin rather than a race.
func newInteractionServer(t *testing.T, secret string, now time.Time, cons SlackInteractions) *httptest.Server {
	t.Helper()
	rt := NewRouter(Options{
		Interactions:  cons,
		SigningSecret: secret,
		Clock:         clock.NewFake(now),
	})
	r := chi.NewRouter()
	rt.RegisterIntegrations(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// postRaw sends one request and returns its status. The body is drained and
// closed here rather than handed back, because nothing in this file reads it —
// Slack's contract is an EMPTY 200, and the status is the whole assertion.
func postRaw(t *testing.T, srv *httptest.Server, body string, headers map[string]string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, srv.URL+"/integrations/slack/interactions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestSlackSignatureVerification is the accept/reject table.
//
// Every rejection answers 401 and NOTHING reaches the consumer, which is the
// property that matters: a forged envelope must not merely fail, it must fail
// before anything downstream has seen it.
func TestSlackSignatureVerification(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	payload := blockActionPayload("oto.ack", "019fe297-d84f-7599-b5b2-1f231749104a")

	tests := []struct {
		name string
		// mutate rewrites the signed request just before it is sent.
		mutate     func(body string, h map[string]string) (string, map[string]string)
		signedAt   time.Time
		wantStatus int
		wantCalls  int
	}{
		{
			name:       "a correctly signed request is accepted",
			signedAt:   now,
			wantStatus: http.StatusOK,
			wantCalls:  1,
		},
		{
			// The most important negative case in the file: the whole point of an
			// HMAC is that not knowing the secret is fatal.
			name: "a bad signature is refused",
			mutate: func(b string, h map[string]string) (string, map[string]string) {
				h[slackSignatureHeader] = "v0=" + strings.Repeat("ab", 32)
				return b, h
			},
			signedAt:   now,
			wantStatus: http.StatusUnauthorized,
		},
		{
			// Replay defence. A captured request stays valid for five minutes and
			// not a second longer, in EITHER direction — a timestamp in the future
			// would otherwise let a captured request be pre-dated forever.
			name:       "a stale timestamp is refused",
			signedAt:   now.Add(-6 * time.Minute),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "a timestamp far in the future is refused",
			signedAt:   now.Add(6 * time.Minute),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "a replay INSIDE the window still verifies",
			signedAt:   now.Add(-4*time.Minute - 59*time.Second),
			wantStatus: http.StatusOK,
			wantCalls:  1,
		},
		{
			// The signature covers the RAW body. Changing one byte of the payload
			// after signing must invalidate it, or the "value" on a button could be
			// swapped for another tenant's group id inside an authentic envelope.
			name: "a tampered body is refused",
			mutate: func(b string, h map[string]string) (string, map[string]string) {
				return strings.Replace(b, "019fe297", "119fe297", 1), h
			},
			signedAt:   now,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "a missing timestamp is refused",
			mutate: func(b string, h map[string]string) (string, map[string]string) {
				delete(h, slackTimestampHeader)
				return b, h
			},
			signedAt:   now,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "a missing signature is refused",
			mutate: func(b string, h map[string]string) (string, map[string]string) {
				delete(h, slackSignatureHeader)
				return b, h
			},
			signedAt:   now,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "an unversioned signature is refused",
			mutate: func(b string, h map[string]string) (string, map[string]string) {
				h[slackSignatureHeader] = strings.TrimPrefix(h[slackSignatureHeader], "v0=")
				return b, h
			},
			signedAt:   now,
			wantStatus: http.StatusUnauthorized,
		},
		{
			// A future v1 scheme must not be accepted by code written for v0.
			name: "a v1 signature is refused",
			mutate: func(b string, h map[string]string) (string, map[string]string) {
				h[slackSignatureHeader] = "v1=" + strings.TrimPrefix(h[slackSignatureHeader], "v0=")
				return b, h
			},
			signedAt:   now,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "a malformed hex signature is refused",
			mutate: func(b string, h map[string]string) (string, map[string]string) {
				h[slackSignatureHeader] = "v0=not-hex"
				return b, h
			},
			signedAt:   now,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "a malformed timestamp is refused",
			mutate: func(b string, h map[string]string) (string, map[string]string) {
				h[slackTimestampHeader] = "yesterday"
				return b, h
			},
			signedAt:   now,
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cons := newCaptor()
			srv := newInteractionServer(t, testSigningSecret, now, cons)

			body, headers := signedForm(testSigningSecret, payload, tc.signedAt)
			if tc.mutate != nil {
				body, headers = tc.mutate(body, headers)
			}

			if got := postRaw(t, srv, body, headers); got != tc.wantStatus {
				t.Fatalf("status = %d, want %d", got, tc.wantStatus)
			}

			// An ACCEPTED request dispatches after the response is flushed, so the
			// count is only meaningful once the dispatch has happened.
			//
			// A REFUSED one needs no wait, and that asymmetry is the property under
			// test rather than an inconvenience: verification fails before anything
			// is dispatched, so there is no later moment at which a rejected
			// envelope could still reach the consumer. Reading the count
			// immediately is exactly the assertion "nothing is on its way either".
			cons.awaitCalls(t, tc.wantCalls)
			if calls, _ := cons.seen(); calls != tc.wantCalls {
				t.Fatalf("the consumer saw %d payloads, want %d", calls, tc.wantCalls)
			}
		})
	}
}

// TestSlackEndpointRefusesWhenNoSigningSecretIsConfigured.
//
// An empty secret DISABLES the endpoint rather than accepting whatever arrives.
// slack-go below v0.23.1 made the opposite choice and therefore accepted forged
// requests; oto's floor exists because of it, and this is that decision asserted.
func TestSlackEndpointRefusesWhenNoSigningSecretIsConfigured(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	cons := newCaptor()
	srv := newInteractionServer(t, "", now, cons)

	// Signed with the EMPTY secret, which is what a naive implementation would
	// happily verify.
	body, headers := signedForm("", blockActionPayload("oto.ack", "x"), now)
	if got := postRaw(t, srv, body, headers); got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: an empty signing secret must disable the endpoint, "+
			"not turn it into an unauthenticated write path", got)
	}
	if calls, _ := cons.seen(); calls != 0 {
		t.Fatalf("the consumer was reached %d times with no signing secret configured", calls)
	}
}

// TestSlackInteractionIsAcknowledgedBeforeTheConsumerRuns is the three-second
// rule, asserted as an ORDERING rather than as a stopwatch.
//
// A duration assertion would be flaky and would prove the wrong thing. What
// matters is that the 200 is on the wire before the consumer is even called, so
// no amount of downstream slowness can turn into "This app is not responding".
func TestSlackInteractionIsAcknowledgedBeforeTheConsumerRuns(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

	release := make(chan struct{})
	cons := newCaptor()
	cons.block = release
	srv := newInteractionServer(t, testSigningSecret, now, cons)

	body, headers := signedForm(testSigningSecret,
		blockActionPayload("oto.ack", "019fe297-d84f-7599-b5b2-1f231749104a"), now)

	done := make(chan int, 1)
	go func() { done <- postRaw(t, srv, body, headers) }()

	select {
	case status := <-done:
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("the endpoint had not answered after three seconds while the consumer was blocked; " +
			"Slack would have shown the user \"This app is not responding\"")
	}
	close(release)
}

// TestSlackInteractionStillAnswers200WhenTheConsumerFails.
//
// A non-2xx makes Slack retry AND show the user a failure banner. The press has
// already been received; oto's own queue owns the retry.
func TestSlackInteractionStillAnswers200WhenTheConsumerFails(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	cons := newCaptor()
	cons.err = errors.New("postgres is on fire")
	srv := newInteractionServer(t, testSigningSecret, now, cons)

	body, headers := signedForm(testSigningSecret,
		blockActionPayload("oto.ack", "019fe297-d84f-7599-b5b2-1f231749104a"), now)
	if got := postRaw(t, srv, body, headers); got != http.StatusOK {
		t.Fatalf("status = %d, want 200 even though the consumer failed", got)
	}
}

// TestSlackPayloadReachesTheConsumerVerbatim.
//
// The consumer is handed the bytes that were SIGNED, parsed out of the same
// buffer the MAC was computed over. Re-reading the body through ParseForm would
// leave the verified bytes and the parsed values with no proven relationship.
func TestSlackPayloadReachesTheConsumerVerbatim(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	cons := newCaptor()
	srv := newInteractionServer(t, testSigningSecret, now, cons)

	payload := blockActionPayload("oto.ack", "019fe297-d84f-7599-b5b2-1f231749104a")
	body, headers := signedForm(testSigningSecret, payload, now)
	if got := postRaw(t, srv, body, headers); got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}

	// The consumer runs after the response is flushed, so wait for it to say so.
	cons.awaitCalls(t, 1)
	_, got := cons.seen()
	if len(got) == 0 {
		t.Fatal("the consumer never received the payload")
	}

	var want, have any
	if err := json.Unmarshal([]byte(payload), &want); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := json.Unmarshal(got, &have); err != nil {
		t.Fatalf("the consumer received something that is not JSON: %v", err)
	}
	wantJSON, _ := json.Marshal(want)
	haveJSON, _ := json.Marshal(have)
	if string(wantJSON) != string(haveJSON) {
		t.Fatalf("the consumer received a different envelope:\n got %s\nwant %s", haveJSON, wantJSON)
	}
}

// TestSlackInteractionWithNoPayloadFieldIsStillAcknowledged.
//
// An authentic envelope oto cannot read is oto's problem, not the Slack user's.
func TestSlackInteractionWithNoPayloadFieldIsStillAcknowledged(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	cons := newCaptor()
	srv := newInteractionServer(t, testSigningSecret, now, cons)

	for _, body := range []string{"", "payload=", "payload=not-json"} {
		stamp := strconv.FormatInt(now.Unix(), 10)
		mac := hmac.New(sha256.New, []byte(testSigningSecret))
		mac.Write([]byte("v0:" + stamp + ":" + body))
		headers := map[string]string{
			slackTimestampHeader: stamp,
			slackSignatureHeader: "v0=" + hex.EncodeToString(mac.Sum(nil)),
			"Content-Type":       "application/x-www-form-urlencoded",
		}
		if got := postRaw(t, srv, body, headers); got != http.StatusOK {
			t.Fatalf("body %q: status = %d, want 200", body, got)
		}
	}
	if calls, _ := cons.seen(); calls != 0 {
		t.Fatalf("an unreadable payload reached the consumer %d times", calls)
	}
}
