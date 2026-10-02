package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/template"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/netguard"
)

const providerName = "webhook"

// signatureHeader carries the HMAC-SHA256 signatures of the outbound request,
// when the channel's connection has a signing secret. It is set only when there
// is a secret to sign with — an unsigned request carries no header at all, never
// an empty one, so a receiver checking for its presence gets an honest answer.
//
// ⛔ ITS FORMAT IS A PUBLISHED PROMISE (docs/setup/webhook.md), WRITTEN DOWN FOR
// THE FIRST TIME BY git-bug 2765f74. Until then it was `sha256=<hex>` over the body
// alone, undocumented — which is the only reason it could change once, to the
// shape below, without breaking anybody who had been told. From here it moves
// only the way the envelope does: a new scheme is a new `v2=` entry sent beside
// `v1=`, never a change to what `v1=` means.
//
//	X-Oto-Timestamp: 1696240000
//	X-Oto-Signature: v1=<hex>[, v1=<hex>]
//
// where each <hex> is HMAC-SHA256(secret, "v1:" + timestamp + ":" + body). Two
// entries appear only during a rotation's overlap, the current secret's first.
const signatureHeader = "X-Oto-Signature"

// timestampHeader is the signed send instant, in Unix seconds.
//
// ⭐ IT IS INSIDE THE SIGNATURE, WHICH IS ITS WHOLE POINT. A signature over the
// body alone verifies forever: a captured request replayed next month passes. With
// the timestamp in the signed base a receiver rejects anything older than its
// tolerance (the docs say five minutes), and cannot be fooled by an edited header
// because editing it breaks the signature. It is stamped per ATTEMPT, not per
// delivery: a retry three hours later is a fresh request and must verify as one.
// `X-Oto-Delivery-Id` is what stays the same across retries, for de-duplication.
const timestampHeader = "X-Oto-Timestamp"

// signatureScheme is the version tag on every signature entry and the first field
// of the signed base string. See signatureHeader.
const signatureScheme = "v1"

// maxResponseBytes bounds what oto reads back before giving up on draining the
// body. The bytes are COUNTED, PARSED FOR AN ECHO ON A 2xx, AND DISCARDED — never
// recorded: an unbounded read is a denial-of-service against oto by its own
// configuration, and a recorded one is worse (see recordResponse). The one thing
// that may survive the read is an incident tool's validated `external_url` /
// `external_id` (see echo.go), and nothing else of it.
const maxResponseBytes = 4096

// maxRetryAfter caps what a receiver may ask oto to wait.
//
// `Retry-After` is upstream-controlled, and an unbounded one is a receiver — or
// whatever an SSRF pointed oto at — parking a firing alert's notification for a
// year. Anything longer is clamped; the backoff schedule owns the rest (§G.6).
const maxRetryAfter = time.Hour

// Channel is one generic webhook destination.
//
// It posts JSON and reports the status code. That is the whole implementation,
// and its plainness is the point: the notification module drives it through
// exactly the same Channel port it drives Slack through, with no webhook-specific
// branch anywhere (R5).
type Channel struct {
	cfg    Config
	cred   domain.Credential
	client *http.Client
	guard  *netguard.Guard
	clock  clock.Clock
	// echo reads an incident tool's handle out of a 2xx response (echo.go).
	echo responseEcho
	// mapped is set when the channel's Connection carries a payload mapping (ADR 0055
	// §2): only a mapped message may then be sent (see send).
	mapped bool
}

// Capabilities reports CapRichLayout and nothing else (§H.10).
//
// No threading, no amend, no interactivity. The dispatch service reads this and
// degrades centrally — a reply becomes nothing (the update carries the same
// facts), a state change becomes a fresh message, and a button becomes a link.
// The provider never makes that decision itself.
func (c *Channel) Capabilities() domain.Capability { return capabilities }

// Deliver posts the rendered envelope.
//
// Every mode is the same request. A webhook has no thread to reply into and no
// message to amend, so a "reply" and an "update" are just another POST carrying
// the current state — which is exactly what a stateless receiver wants.
func (c *Channel) Deliver(ctx context.Context, req domain.DeliverRequest) (domain.DeliverResult, error) {
	return c.send(ctx, req.Message, req.DeliveryID.String())
}

// Amend re-posts. A webhook cannot edit, and pretending otherwise would make the
// Channel port a lie. The dispatch service knows this from Capabilities and sends
// a standalone message instead.
func (c *Channel) Amend(
	ctx context.Context, _ domain.MessageRef, msg domain.RenderedMessage,
) (domain.DeliverResult, error) {
	return c.send(ctx, msg, "")
}

func (c *Channel) send(
	ctx context.Context, msg domain.RenderedMessage, deliveryID string,
) (domain.DeliverResult, error) {
	// Checked again here so a target that is ALREADY known-bad fails as
	// `config_invalid` (permanent, visible, fixable) rather than as a dial error
	// twelve retries later. It is NOT the control — the guard's dialer under
	// c.client is, and it re-checks the address the socket connects to, which is
	// why an UNDECIDED answer is passed through to the dial rather than treated
	// as a refusal.
	if err := c.guard.CheckURL(ctx, c.cfg.URL); err != nil && !netguard.Undecided(err) {
		return domain.DeliverResult{}, &domain.Error{
			Class: domain.ClassConfigInvalid, Provider: providerName,
			Code: "target_not_allowed", Cause: err,
		}
	}

	wire, mappedHeaders, err := c.request(msg)
	if err != nil {
		return domain.DeliverResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, c.cfg.Method, c.cfg.URL, bytes.NewReader(wire))
	if err != nil {
		return domain.DeliverResult{}, &domain.Error{
			Class: domain.ClassConfigInvalid, Provider: providerName,
			Code: "invalid_request", Cause: err,
		}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "oto/1")
	req.Header.Set("Accept", "application/json")
	for k, v := range c.cfg.Headers {
		// ⛔ AN `X-Oto-*` NAME IS SKIPPED, NOT SET. CheckHeaders refuses the whole
		// prefix at save, but a channel stored before it did may still carry one,
		// and Open deliberately does not refuse it (checkStoredHeaders). The comment
		// that used to sit here claimed the save-time check made this safe while
		// that check named none of oto's headers — so this loop, which ran LAST,
		// was free to replace oto's idempotency handle with a constant.
		if reservedHeader(k) {
			continue
		}
		req.Header.Set(k, v)
	}
	// A payload mapping's headers come after the channel's static ones, under the
	// same two rules: an `X-Oto-*` name is skipped, and so is a credential header —
	// both are refused when the mapping is saved, and skipped here regardless.
	for k, v := range mappedHeaders {
		if reservedHeader(k) || forbiddenHeader(k) {
			continue
		}
		req.Header.Set(k, v)
	}
	// ⭐ OTO'S FRAMING IS SET AFTER THE CONFIGURED HEADERS, so it wins by order as
	// well as by the rule above: two independent reasons a receiver's idempotency
	// and signature checks can never be shadowed by a static header.
	if deliveryID != "" {
		// The receiver's own idempotency handle. oto's queue is at-least-once, so
		// a receiver that wants exactly-once has what it needs to get there.
		req.Header.Set("X-Oto-Delivery-Id", deliveryID)
	}
	if msg.Hash != "" {
		req.Header.Set("X-Oto-Content-Hash", msg.Hash)
	}
	// The signature covers the bytes on the wire: a mapped body, with its secrets
	// filled, is what the receiver verifies.
	if ts, signature := c.sign(wire, c.clock.Now()); signature != "" {
		req.Header.Set(timestampHeader, ts)
		req.Header.Set(signatureHeader, signature)
	}

	started := c.clock.Now()
	resp, err := c.client.Do(req)
	if err != nil {
		return domain.DeliverResult{}, classifyTransport(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// ⛔ THE BODY IS NEVER RECORDED. It is read (bounded) so the connection can be
	// reused and so a 2xx can be asked for an echo, and then it is gone. See
	// recordResponse for why not one byte of it may reach `provider_response`.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	bodyBytes := int64(len(body))
	elapsed := c.clock.Now().Sub(started)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A refusal's body is not read for an echo: an incident the receiver
		// refused to open has no handle worth keeping.
		return domain.DeliverResult{}, classifyStatus(resp, bodyBytes)
	}

	return domain.DeliverResult{
		Ref: domain.MessageRef{
			// A webhook returns no message identity, so there is nothing to
			// thread from and nothing to amend. ProviderKey carries the delivery
			// id purely so a Deliveries row has something to show a human.
			ProviderKey: deliveryID,
		},
		DeliveredAt: c.clock.Now().UTC(),
		Raw:         recordResponse(resp.StatusCode, bodyBytes, elapsed),
		External:    c.readEcho(body),
	}, nil
}

// request is the body and mapped headers this message is sent as.
//
// ⛔ A MAPPED CONNECTION SENDS A MAPPED BODY OR NOTHING (ADR 0055 §2). A message
// that did not go through the Connection's payload mapping is refused as
// `config_invalid` rather than sent: the plain envelope reaching a vendor that
// cannot parse it is a missing incident, and "never a fallback" has to hold even
// for a caller that forgot to map.
//
// ⭐ A MAPPED BODY'S SECRETS ARE FILLED HERE, AT THE MOMENT OF SENDING. The
// delivery row recorded the body with `secrets.<name>` references in it, so the
// stored request never holds a secret; the values come from the Connection's sealed
// mapping-secret slot, unsealed into Credential.Secrets for this Open only. A
// reference to a secret the Connection no longer holds fails the delivery.
func (c *Channel) request(msg domain.RenderedMessage) ([]byte, map[string]string, error) {
	if !msg.Mapped {
		if c.mapped {
			return nil, nil, &domain.Error{
				Class: domain.ClassConfigInvalid, Provider: providerName,
				Code:  "payload_mapping_not_applied",
				Cause: errors.New("this connection carries a payload mapping and the message was not mapped; the plain envelope is never sent in its place"),
			}
		}
		return msg.Payload, nil, nil
	}
	body, headers, err := template.FillSecrets(msg.Payload, msg.Headers, c.cred.Secrets)
	if err != nil {
		return nil, nil, &domain.Error{
			Class: domain.ClassConfigInvalid, Provider: providerName,
			Code: "payload_mapping_secret", Cause: err,
		}
	}
	return body, headers, nil
}

// sign returns the timestamp and signature headers for a body sent at `at`, or two
// empty strings when the connection carries no signing secret.
//
// One entry per live secret — the current one, and its predecessor while the
// rotation overlap lasts (domain.SigningSecret.Secrets decides that against THIS
// send's instant) — joined as an RFC 9110 list. A receiver splits on commas and
// accepts the request if ANY `v1=` entry matches what it computes with the secret
// it holds, which is what lets an operator rotate without a flag day.
func (c *Channel) sign(body []byte, at time.Time) (timestamp, signature string) {
	secrets := c.cred.Signing.Secrets(at)
	if len(secrets) == 0 {
		return "", ""
	}
	timestamp = strconv.FormatInt(at.Unix(), 10)
	entries := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		entries = append(entries, signatureScheme+"="+signatureHex(secret, timestamp, body))
	}
	return timestamp, strings.Join(entries, ", ")
}

// signatureHex is HMAC-SHA256(secret, "v1:" + timestamp + ":" + body), hex-encoded.
//
// ⛔ THIS LINE IS THE PUBLISHED ALGORITHM. docs/setup/webhook.md states the base
// string byte for byte and verifies a worked example against it; changing either
// side without the other is a broken promise to every receiver, and changing
// both is a `v2=` scheme, not an edit.
func signatureHex(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signatureScheme + ":" + timestamp + ":"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Probe checks the destination without delivering an alert.
//
// It verifies only that the target is allowed and reachable. oto deliberately does
// NOT send a synthetic alert to test a channel: a fake page is indistinguishable
// from a real one at 03:00.
func (c *Channel) Probe(ctx context.Context) error {
	if err := c.guard.CheckURL(ctx, c.cfg.URL); err != nil {
		return &domain.Error{
			Class: domain.ClassConfigInvalid, Provider: providerName,
			Code: "target_not_allowed", Cause: err,
		}
	}
	return nil
}

// Close releases the Channel.
func (c *Channel) Close() error {
	c.client.CloseIdleConnections()
	return nil
}

// classifyStatus maps an HTTP status onto the port's ErrorClass.
//
// 429 honours Retry-After (clamped); 5xx retries; 4xx is permanent, because
// retrying a request the receiver has already rejected twelve times is how a
// notification backlog becomes an outage.
func classifyStatus(resp *http.Response, bodyBytes int64) *domain.Error {
	cause := fmt.Errorf("webhook responded %d (%d body bytes, not recorded)",
		resp.StatusCode, bodyBytes)
	code := "http_" + strconv.Itoa(resp.StatusCode)

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return &domain.Error{
			Class: domain.ClassRateLimited, RetryAfter: retryAfter(resp),
			Provider: providerName, Code: "rate_limited", Cause: cause,
		}
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return &domain.Error{
			Class: domain.ClassAuthExpired, Provider: providerName, Code: code, Cause: cause,
		}
	case resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode == http.StatusServiceUnavailable,
		resp.StatusCode == http.StatusGatewayTimeout,
		resp.StatusCode >= 500:
		return &domain.Error{
			Class: domain.ClassRetryable, RetryAfter: retryAfter(resp),
			Provider: providerName, Code: code, Cause: cause,
		}
	case resp.StatusCode >= 400:
		return &domain.Error{
			Class: domain.ClassPermanent, Provider: providerName, Code: code, Cause: cause,
		}
	default:
		return &domain.Error{
			Class: domain.ClassRetryable, Provider: providerName, Code: code, Cause: cause,
		}
	}
}

func classifyTransport(err error) *domain.Error {
	// ⭐ A GUARD REFUSAL IS NOT A NETWORK BLIP. The SSRF guard now lives in the
	// DIALER, so its refusal surfaces here, wrapped in a *url.Error, rather than
	// from the pre-flight check. Left to the default it would be classified
	// `retryable` and re-dialled a dozen times — a blocked target retried on a
	// backoff instead of shown to the operator as the configuration error it is.
	if e, ok := errs.As(err); ok && e.Kind == errs.KindValidation {
		return &domain.Error{
			Class: domain.ClassConfigInvalid, Provider: providerName,
			Code: "target_not_allowed", Cause: err,
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &domain.Error{Class: domain.ClassRetryable, Provider: providerName, Code: "timeout", Cause: err}
	}
	if errors.Is(err, context.Canceled) {
		return &domain.Error{Class: domain.ClassRetryable, Provider: providerName, Code: "cancelled", Cause: err}
	}
	return &domain.Error{Class: domain.ClassRetryable, Provider: providerName, Code: "network", Cause: err}
}

func retryAfter(resp *http.Response) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return clampRetryAfter(time.Duration(secs) * time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return clampRetryAfter(d)
		}
	}
	return 0
}

func clampRetryAfter(d time.Duration) time.Duration {
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}

// recordResponse is what `provider_response` on GET /deliveries/{id} shows.
//
// ⛔ IT MUST NEVER CARRY ONE BYTE THE RECEIVER SENT. The webhook URL is
// operator-supplied and oto dials it from inside the operator's network; the
// guard makes reaching an internal address hard, but a channel pointed at a
// merely-unintended target must not additionally hand its response back through
// the API. A body snippet here made every webhook an SSRF READ primitive: the
// attacker did not just cause the request, they got the answer. Status code,
// body SIZE and round-trip time answer "did it arrive, was it healthy, was it
// slow" — which is what debugging a delivery actually needs — and none of the
// three is a channel for content.
//
// A "redacted" or truncated snippet is NOT an acceptable middle ground. 200
// characters of an internal page is still an internal page, and redaction that
// has to guess what is sensitive in an unknown upstream's output is redaction
// that will be wrong.
func recordResponse(status int, bodyBytes int64, elapsed time.Duration) json.RawMessage {
	raw, err := json.Marshal(map[string]any{
		"status":      status,
		"body_bytes":  bodyBytes,
		"duration_ms": elapsed.Milliseconds(),
	})
	if err != nil {
		return nil
	}
	return raw
}
