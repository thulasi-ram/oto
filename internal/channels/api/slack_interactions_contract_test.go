package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/httpx/middleware"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// `receiveSlackInteraction`, against the contract rather than against a status
// code.
//
// slack_interactions_test.go already proves the SECURITY property — the HMAC is
// the only authentication this endpoint has, and every forgery is refused before
// anything downstream sees it. What it deliberately does not do is look at the
// response BODY: it drains and discards it, because Slack's contract is a prompt
// 2xx and the status was the whole assertion there.
//
// The two cases here close that gap. The contract declares the 200's body as
// `text/plain` with `maxLength: 0` — an EMPTY acknowledgement — and the 401 as an
// RFC 9457 problem. Those are two promises a status code cannot carry, and a
// refusal that answered 401 with a bare string would fail every client that
// branches on `code`.

// slackContractNow pins the replay window so the signature is a fact rather than
// a race against the wall clock.
var slackContractNow = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

// newSlackContractClient mounts the integrations routes WITH the request-id
// middleware.
//
// The middleware is not decoration here: `request_id` is a REQUIRED member of the
// Problem schema, so a 401 produced without it fails its own contract. The real
// server runs it in front of everything, and a test that omitted it would be
// asserting a document oto never emits.
func newSlackContractClient(t *testing.T, secret string) *apitest.Client {
	t.Helper()

	rt := NewRouter(Options{
		Interactions:  &captor{},
		SigningSecret: secret,
		Clock:         clock.NewFake(slackContractNow),
	})
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	rt.RegisterIntegrations(r)
	return apitest.NewHandler(r)
}

// postSigned sends one form-encoded interaction with the headers Slack sends.
// Unlike postRaw in slack_interactions_test.go it KEEPS the body, which is the
// only reason this helper exists.
func postSigned(c *apitest.Client, body string, headers map[string]string) *apitest.Response {
	req := httptest.NewRequest(http.MethodPost,
		"/integrations/slack/interactions", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.Do(req)
}

// TestSlackAcknowledgementIsTheEmptyBodyTheContractDeclares.
//
// Slack requires only a prompt 2xx, and the contract says so in the strongest
// form available to it: `text/plain` with `maxLength: 0`. Any work the
// interaction implies happens on oto's own queues, so a body here would be
// something oto invented for nobody — and `schema.AssertNoBody` refuses to let it
// appear unnoticed.
func TestSlackAcknowledgementIsTheEmptyBodyTheContractDeclares(t *testing.T) {
	t.Parallel()

	c := newSlackContractClient(t, testSigningSecret)
	body, headers := signedForm(testSigningSecret,
		blockActionPayload("oto.ack", "019fe297-d84f-7599-b5b2-1f231749104a"), slackContractNow)

	resp := postSigned(c, body, headers).MustStatus(t, http.StatusOK)
	schema.AssertNoBody(t, "receiveSlackInteraction", http.StatusOK, resp.Body())
}

// ⛔ TestARefusedSlackInteractionIsAProblemDocumentAndNotABareString.
//
// This endpoint is mounted outside every authenticator oto has, so its 401 is the
// one refusal an unauthenticated caller can reach. The contract types it as
// `application/problem+json`, which means a `code` a client can branch on and a
// `request_id` an operator can correlate — and `status` that agrees with the
// status line, so nobody has to choose which of the two to believe.
func TestARefusedSlackInteractionIsAProblemDocumentAndNotABareString(t *testing.T) {
	t.Parallel()

	c := newSlackContractClient(t, testSigningSecret)
	body, headers := signedForm(testSigningSecret,
		blockActionPayload("oto.ack", "019fe297-d84f-7599-b5b2-1f231749104a"), slackContractNow)
	headers[slackSignatureHeader] = "v0=" + strings.Repeat("ab", 32)

	resp := postSigned(c, body, headers).MustStatus(t, http.StatusUnauthorized)
	schema.AssertProblem(t, "receiveSlackInteraction", http.StatusUnauthorized, resp.Body())

	if got := resp.Header("Content-Type"); !strings.HasPrefix(got, apitest.ContentTypeProblem) {
		t.Fatalf("Content-Type = %q, want %s (RFC 9457 §3)", got, apitest.ContentTypeProblem)
	}
	// The refusal must not tell an attacker WHICH check failed: a signature, a
	// timestamp and a missing header are one answer to somebody who does not know
	// the secret, and three answers to somebody probing for one.
	if strings.Contains(strings.ToLower(resp.Problem(t).Detail), "secret") {
		t.Fatalf("the refusal describes the signing secret: %s", resp.Body())
	}
}
