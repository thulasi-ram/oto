package harness

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/thulasiram/oto/internal/channels/providers/webhook"
	"github.com/thulasiram/oto/internal/platform/netguard"
)

// Webhook is a fake outbound generic-webhook receiver: the other end of
// `channels/providers/webhook`.
//
// ⚠️ IT LISTENS ON LOOPBACK, WHICH THE SSRF GUARD REFUSES BY DEFAULT — and that
// refusal is the control, not a nuisance: `netguard` checks the address the
// socket actually connects to, so a test cannot smuggle a private target past
// it. Use Provider (below) to build a webhook provider that is explicitly
// allowed to reach private space, and never disable the guard globally.
type Webhook struct {
	srv *httptest.Server

	mu       sync.Mutex
	status   int
	body     string
	requests []WebhookRequest
}

// WebhookRequest is one delivery the fake received.
type WebhookRequest struct {
	// Method is the HTTP method.
	Method string
	// Path is the request path.
	Path string
	// Header is the request header, including the Authorization the sealed
	// credential put there — which is how a test proves the secret travelled on
	// the transport and never through Config.
	Header http.Header
	// Body is the rendered payload, byte for byte.
	Body []byte
}

// NewWebhook starts a fake receiver that answers 200 and stops when the test
// ends.
func NewWebhook(t *testing.T) *Webhook {
	t.Helper()
	w := &Webhook{status: http.StatusOK, body: `{"ok":true}`}
	w.srv = httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.srv.Close)
	return w
}

// URL is the receiver's address, for a webhook channel's `config.url`.
func (w *Webhook) URL() string { return w.srv.URL }

// Provider builds the REAL webhook provider, permitted to reach this fake.
//
// AllowPrivateTargets is the operator-facing switch (OTO_ALLOW_PRIVATE_WEBHOOK_
// TARGETS) and it is off in production. Turning it on here is what makes a
// loopback httptest.Server reachable, and it is scoped to the provider this
// test built — the guard is untouched everywhere else.
func (w *Webhook) Provider(opts webhook.Options) *webhook.Provider {
	opts.AllowPrivateTargets = true
	if opts.Guard == nil {
		opts.Guard = netguard.New(netguard.Options{
			AllowPrivate: true, Code: "config_invalid", Field: "url",
		})
	}
	return webhook.NewProvider(opts)
}

// RespondWith sets the status and body every subsequent delivery receives. A 429
// or a 503 is how a test drives the retry classification; a 400 is how it drives
// `dead`.
func (w *Webhook) RespondWith(status int, body string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status, w.body = status, body
}

// Requests returns every delivery received, in order.
func (w *Webhook) Requests() []WebhookRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]WebhookRequest(nil), w.requests...)
}

// Count is how many deliveries arrived. It is the assertion that separates
// "oto was silent" from "oto delivered", which is the distinction the product
// exists to keep visible.
func (w *Webhook) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.requests)
}

func (w *Webhook) serve(rw http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	w.mu.Lock()
	w.requests = append(w.requests, WebhookRequest{
		Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body,
	})
	status, payload := w.status, w.body
	w.mu.Unlock()

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_, _ = io.WriteString(rw, payload)
}
