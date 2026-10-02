package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// This file holds the webhook signature to what docs/setup/webhook.md promises a
// receiver (ADR 0055 §1, git-bug 2765f74). The receiver side is written HERE, from
// the docs' words, rather than by calling signatureHex: a test that verified with
// the function under test would pass for any algorithm at all, including one the
// docs do not describe.

// The worked example docs/setup/webhook.md prints, byte for byte. If this constant
// has to change, the docs' example has to change with it, and so does every
// receiver anybody wrote from them — which is to say it does not change; a new
// scheme is a `v2=` entry.
const (
	docsSecret    = "oto-example-signing-secret"
	docsTimestamp = "1759400000"
	docsBody      = `{"schema":"oto.notification.v1","reason":"drawn"}`
	docsSignature = "e8ae2236d80ddab5f6b8da2c535675fede395d66a342cd33e5eeee61588bcb22"
)

// receiverTolerance is the replay window the docs tell a receiver to enforce.
const receiverTolerance = 5 * time.Minute

// verify is a receiver, written from docs/setup/webhook.md "Verifying a request":
//
//  1. read X-Oto-Timestamp; reject it if it is further than the tolerance from now;
//  2. compute HMAC-SHA256(secret, "v1:" + timestamp + ":" + raw body), hex;
//  3. split X-Oto-Signature on commas; accept if any trimmed `v1=` entry equals it,
//     compared in constant time.
func verify(secret string, h http.Header, body []byte, now time.Time) bool {
	ts := h.Get("X-Oto-Timestamp")
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if d := now.Sub(time.Unix(secs, 0)); d > receiverTolerance || d < -receiverTolerance {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v1:" + ts + ":"))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	for _, entry := range strings.Split(h.Get("X-Oto-Signature"), ",") {
		scheme, got, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if ok && scheme == "v1" && hmac.Equal([]byte(got), []byte(want)) {
			return true
		}
	}
	return false
}

// captured is one request a stub receiver saw.
type captured struct {
	header http.Header
	body   []byte
}

// stubReceiver records every request and answers 200.
func stubReceiver(t *testing.T) (*httptest.Server, func() captured) {
	t.Helper()
	var (
		mu   sync.Mutex
		last captured
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		last = captured{header: r.Header.Clone(), body: body}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, func() captured {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

// openSigned opens a webhook channel to url with the given credential, under a
// fake clock. AllowPrivateTargets because httptest binds loopback; the SSRF guard
// is ssrf_test.go's subject, not this file's.
func openSigned(t *testing.T, clk clock.Clock, raw json.RawMessage, cred domain.Credential) domain.Channel {
	t.Helper()
	p := NewProvider(Options{Clock: clk, AllowPrivateTargets: true})
	ch, err := p.Open(context.Background(), domain.ChannelConfig{Raw: raw}, cred)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	return ch
}

func deliverOnce(t *testing.T, ch domain.Channel, id uuid.UUID) {
	t.Helper()
	if _, err := ch.Deliver(context.Background(), domain.DeliverRequest{
		Message: testMessage(), Mode: domain.ModePostRoot, DeliveryID: id,
	}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
}

// TestTheDocumentedExampleIsWhatTheProviderComputes pins the docs' worked example to
// the provider's own function, so the page and the code cannot drift apart silently.
func TestTheDocumentedExampleIsWhatTheProviderComputes(t *testing.T) {
	t.Parallel()
	if got := signatureHex(docsSecret, docsTimestamp, []byte(docsBody)); got != docsSignature {
		t.Fatalf("signatureHex over the docs' example = %s, want the documented %s — "+
			"the published algorithm and the provider disagree", got, docsSignature)
	}
	h := http.Header{}
	h.Set("X-Oto-Timestamp", docsTimestamp)
	h.Set("X-Oto-Signature", "v1="+docsSignature)
	at, _ := strconv.ParseInt(docsTimestamp, 10, 64)
	if !verify(docsSecret, h, []byte(docsBody), time.Unix(at, 0)) {
		t.Fatal("the docs' receiver steps do not verify the docs' own example")
	}
}

// TestASignedRequestVerifiesAndATamperedOrStaleOneDoesNot is the whole promise to a
// receiver in one test: the request oto sends verifies with the documented steps;
// one changed byte does not; and the same bytes replayed past the tolerance do not.
func TestASignedRequestVerifiesAndATamperedOrStaleOneDoesNot(t *testing.T) {
	t.Parallel()
	srv, last := stubReceiver(t)
	clk := clock.NewFake(time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	ch := openSigned(t, clk, rawConfig(t, srv.URL),
		domain.Credential{Signing: domain.SigningSecret{Current: "s3cret"}})

	deliverOnce(t, ch, uuid.New())
	got := last()

	if ts := got.header.Get("X-Oto-Timestamp"); ts != strconv.FormatInt(clk.Now().Unix(), 10) {
		t.Fatalf("X-Oto-Timestamp = %q, want the send instant %d", ts, clk.Now().Unix())
	}
	if n := len(strings.Split(got.header.Get("X-Oto-Signature"), ",")); n != 1 {
		t.Fatalf("a connection that has never rotated sent %d signatures, want 1", n)
	}
	if !verify("s3cret", got.header, got.body, clk.Now()) {
		t.Fatalf("oto's own request does not verify with the documented algorithm: %q",
			got.header.Get("X-Oto-Signature"))
	}
	if verify("not-the-secret", got.header, got.body, clk.Now()) {
		t.Fatal("the request verified with the wrong secret")
	}

	tampered := append([]byte(nil), got.body...)
	tampered[len(tampered)-2] ^= 0x01
	if verify("s3cret", got.header, tampered, clk.Now()) {
		t.Fatal("a body with one byte changed still verified — the body is not under the signature")
	}

	// The timestamp header is under the signature too: moving it forward to defeat
	// the tolerance breaks the signature instead.
	forged := got.header.Clone()
	forged.Set("X-Oto-Timestamp", strconv.FormatInt(clk.Now().Add(time.Hour).Unix(), 10))
	if verify("s3cret", forged, got.body, clk.Now().Add(time.Hour)) {
		t.Fatal("an edited X-Oto-Timestamp still verified — the timestamp is not under the signature")
	}

	if verify("s3cret", got.header, got.body, clk.Now().Add(receiverTolerance+time.Second)) {
		t.Fatal("a captured request replayed past the tolerance still verified")
	}
}

// TestARotationSignsWithBothSecretsUntilTheOverlapEnds is the rotation: during the
// overlap a receiver holding EITHER secret verifies, the current one's entry comes
// first, and once the overlap ends only the current secret signs.
func TestARotationSignsWithBothSecretsUntilTheOverlapEnds(t *testing.T) {
	t.Parallel()
	srv, last := stubReceiver(t)
	start := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	ch := openSigned(t, clk, rawConfig(t, srv.URL), domain.Credential{Signing: domain.SigningSecret{
		Current: "new-secret", Previous: "old-secret", PreviousUntil: start.Add(domain.SigningSecretOverlap),
	}})

	deliverOnce(t, ch, uuid.New())
	during := last()
	entries := strings.Split(during.header.Get("X-Oto-Signature"), ",")
	if len(entries) != 2 {
		t.Fatalf("during the overlap X-Oto-Signature = %q, want two entries",
			during.header.Get("X-Oto-Signature"))
	}
	if !strings.HasPrefix(strings.TrimSpace(entries[0]),
		"v1="+signatureHex("new-secret", during.header.Get("X-Oto-Timestamp"), during.body)) {
		t.Errorf("the current secret's signature is not first: %q", entries[0])
	}
	if !verify("new-secret", during.header, during.body, clk.Now()) {
		t.Error("a receiver that already has the new secret rejected a request during the overlap")
	}
	if !verify("old-secret", during.header, during.body, clk.Now()) {
		t.Error("a receiver still on the old secret rejected a request during the overlap — " +
			"that is the flag day the overlap exists to remove")
	}

	// THE SAME OPEN CHANNEL, past the end: the provider decides at the send instant,
	// not at Open, so a long-lived Channel cannot keep a retired secret alive.
	clk.Set(start.Add(domain.SigningSecretOverlap))
	deliverOnce(t, ch, uuid.New())
	after := last()
	if n := len(strings.Split(after.header.Get("X-Oto-Signature"), ",")); n != 1 {
		t.Fatalf("after the overlap X-Oto-Signature = %q, want only the current secret's",
			after.header.Get("X-Oto-Signature"))
	}
	if !verify("new-secret", after.header, after.body, clk.Now()) {
		t.Error("after the overlap the current secret no longer verifies")
	}
	if verify("old-secret", after.header, after.body, clk.Now()) {
		t.Error("the retired secret still verifies after the overlap ended")
	}
}

// TestABearerTokenAndASignatureTravelTogether is the coexistence the second slot
// (migration 00088) exists for: a receiver that requires a bearer token AND checks
// the signature — incident.io's HTTP alert source is the motivating one — gets both.
func TestABearerTokenAndASignatureTravelTogether(t *testing.T) {
	t.Parallel()
	srv, last := stubReceiver(t)
	clk := clock.NewFake(time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	ch := openSigned(t, clk, rawConfig(t, srv.URL), domain.Credential{
		Kind: CredBearer, Values: map[string]string{"token": "tok-123"},
		Signing: domain.SigningSecret{Current: "s3cret"},
	})

	deliverOnce(t, ch, uuid.New())
	got := last()
	if a := got.header.Get("Authorization"); a != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want the bearer token beside the signature", a)
	}
	if !verify("s3cret", got.header, got.body, clk.Now()) {
		t.Error("the signature did not survive alongside a bearer token")
	}
}

// TestAnUnsignedConnectionSendsNeitherHeader keeps the old promise: no secret, no
// signature header and no timestamp header — never an empty one.
func TestAnUnsignedConnectionSendsNeitherHeader(t *testing.T) {
	t.Parallel()
	srv, last := stubReceiver(t)
	ch := openSigned(t, clock.New(), rawConfig(t, srv.URL), domain.Credential{Kind: CredNone})

	deliverOnce(t, ch, uuid.New())
	got := last()
	for _, h := range []string{"X-Oto-Signature", "X-Oto-Timestamp"} {
		if _, present := got.header[http.CanonicalHeaderKey(h)]; present {
			t.Errorf("an unsigned connection sent %s: %q", h, got.header.Get(h))
		}
	}
}

// headersConfig is a webhook config with the given static headers.
func headersConfig(t *testing.T, url string, headers map[string]string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"url": url, "headers": headers})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestAConfiguredOtoHeaderIsRefusedAtSave is the save-time half: every `X-Oto-*`
// name is oto's, so a channel naming one — in any case — is a 422 on that header.
func TestAConfiguredOtoHeaderIsRefusedAtSave(t *testing.T) {
	t.Parallel()
	p := NewProvider(Options{Clock: clock.New()})
	for _, name := range []string{"X-Oto-Delivery-Id", "x-oto-signature", "X-OTO-Timestamp", "X-Oto-Anything-Later"} {
		err := p.ValidateConfig(context.Background(),
			headersConfig(t, "https://receiver.example.com/hook", map[string]string{name: "constant"}))
		if err == nil {
			t.Errorf("a channel configured with %s was accepted", name)
			continue
		}
		e, ok := errs.As(err)
		if !ok || e.Kind != errs.KindValidation {
			t.Errorf("%s: want a validation error, got %v", name, err)
			continue
		}
		found := false
		for _, v := range e.Violations {
			if v.Field == "headers/"+name && v.Code == "forbidden" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no forbidden violation naming the header: %+v", name, e.Violations)
		}
	}

	// An ordinary custom header is still the operator's.
	if err := p.ValidateConfig(context.Background(),
		headersConfig(t, "https://receiver.example.com/hook", map[string]string{"X-Team": "sre"})); err != nil {
		t.Errorf("an ordinary header was refused: %v", err)
	}
}

// TestAStoredOtoHeaderCannotOverrideTheDeliveryId is the send-time half, for a
// channel saved before the prefix was reserved: it still OPENS (refusing would turn
// the hardening into silent non-delivery), and the receiver sees oto's delivery id,
// not the configured constant.
func TestAStoredOtoHeaderCannotOverrideTheDeliveryId(t *testing.T) {
	t.Parallel()
	srv, last := stubReceiver(t)
	ch := openSigned(t, clock.New(),
		headersConfig(t, srv.URL, map[string]string{"X-Oto-Delivery-Id": "constant", "X-Team": "sre"}),
		domain.Credential{Kind: CredNone})

	id := uuid.New()
	deliverOnce(t, ch, id)
	got := last()
	if v := got.header.Values("X-Oto-Delivery-Id"); len(v) != 1 || v[0] != id.String() {
		t.Fatalf("X-Oto-Delivery-Id = %q, want exactly oto's own %s", v, id)
	}
	if got.header.Get("X-Team") != "sre" {
		t.Error("an ordinary configured header was dropped along with the reserved one")
	}
}
