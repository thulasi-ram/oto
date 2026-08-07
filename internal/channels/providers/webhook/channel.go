package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
)

const providerName = "webhook"

// maxResponseBytes bounds what oto reads back. A receiver's response body is
// never interesting beyond a diagnostic snippet, and an unbounded read is a
// denial-of-service against oto by its own configuration.
const maxResponseBytes = 4096

// Channel is one generic webhook destination.
//
// It posts JSON and reports the status code. That is the whole implementation,
// and its plainness is the point: the notification module drives it through
// exactly the same Channel port it drives Slack through, with no webhook-specific
// branch anywhere (R5).
type Channel struct {
	cfg    Config
	client *http.Client
	guard  *Guard
	clock  clock.Clock
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
	// Re-checked on every send, not just at configuration time: DNS can be
	// re-pointed at a link-local address long after the config was saved.
	if err := c.guard.CheckURL(ctx, c.cfg.URL); err != nil {
		return domain.DeliverResult{}, &domain.Error{
			Class: domain.ClassConfigInvalid, Provider: providerName,
			Code: "target_not_allowed", Cause: err,
		}
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, c.cfg.Method, c.cfg.URL, bytes.NewReader(msg.Payload))
	if err != nil {
		return domain.DeliverResult{}, &domain.Error{
			Class: domain.ClassConfigInvalid, Provider: providerName,
			Code: "invalid_request", Cause: err,
		}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "oto/1")
	req.Header.Set("Accept", "application/json")
	if deliveryID != "" {
		// The receiver's own idempotency handle. oto's queue is at-least-once, so
		// a receiver that wants exactly-once has what it needs to get there.
		req.Header.Set("X-Oto-Delivery-Id", deliveryID)
	}
	if msg.Hash != "" {
		req.Header.Set("X-Oto-Content-Hash", msg.Hash)
	}
	for k, v := range c.cfg.Headers {
		// Config headers are applied last but can never override oto's framing:
		// CheckHeaders already refused the reserved names at configuration time.
		req.Header.Set(k, v)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return domain.DeliverResult{}, classifyTransport(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return domain.DeliverResult{}, classifyStatus(resp, body)
	}

	return domain.DeliverResult{
		Ref: domain.MessageRef{
			// A webhook returns no message identity, so there is nothing to
			// thread from and nothing to amend. ProviderKey carries the delivery
			// id purely so a Deliveries row has something to show a human.
			ProviderKey: deliveryID,
		},
		DeliveredAt: c.clock.Now().UTC(),
		Raw:         rawResult(resp.StatusCode, body),
	}, nil
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
// 429 honours Retry-After exactly; 5xx retries; 4xx is permanent, because
// retrying a request the receiver has already rejected twelve times is how a
// notification backlog becomes an outage.
func classifyStatus(resp *http.Response, body []byte) *domain.Error {
	cause := fmt.Errorf("webhook responded %d: %s", resp.StatusCode, snippet(body))
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
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func rawResult(status int, body []byte) json.RawMessage {
	raw, err := json.Marshal(map[string]any{"status": status, "body": snippet(body)})
	if err != nil {
		return nil
	}
	return raw
}
