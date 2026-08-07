package httpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Client is one configured upstream. It is safe for concurrent use and is meant
// to be built once per AlertSource and cached, not per request.
type Client struct {
	base      string
	prefix    string
	hc        *http.Client
	auth      Auth
	retry     Retry
	maxBytes  int64
	userAgent string
	clk       clock.Clock
	sleep     Sleeper
}

// New builds a Client. It fails only on configuration that can never work: a
// base URL that is not canonicalisable, an unusable CA bundle, a missing error
// prefix.
func New(cfg Config) (*Client, error) {
	base, err := NormalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	if cfg.ErrPrefix == "" {
		return nil, errs.New(errs.KindValidation, CodeInvalidConfig,
			"an outbound HTTP client needs an error-code prefix")
	}

	tr := cfg.Transport
	if tr == nil {
		tr, err = buildTransport(cfg.TLS)
		if err != nil {
			return nil, errs.Wrap(err, errs.KindValidation, Code(cfg.ErrPrefix, CodeInvalidConfig),
				messageFor(CodeInvalidConfig))
		}
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxBytes := cfg.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxResponseBytes
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.New()
	}
	sleep := cfg.Sleep
	if sleep == nil {
		sleep = realSleep
	}
	ua := cfg.UserAgent
	if ua == "" {
		ua = "oto"
	}

	return &Client{
		base:   base,
		prefix: cfg.ErrPrefix,
		hc: &http.Client{
			Transport: tr,
			Timeout:   timeout,
			// oto never follows a redirect to an upstream API. A 30x from an
			// Alertmanager path means a proxy is in the way, and silently
			// following it is how a credential leaks to another host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		auth:      cfg.Auth,
		retry:     cfg.Retry.normalise(),
		maxBytes:  maxBytes,
		userAgent: ua,
		clk:       clk,
		sleep:     sleep,
	}, nil
}

// BaseURL is the canonical upstream root, without a trailing slash.
func (c *Client) BaseURL() string { return c.base }

// Prefix is the error-code namespace this client stamps on every failure.
func (c *Client) Prefix() string { return c.prefix }

// Clock exposes the injected time source, so a caller can measure latency
// through the same clock the client retries on.
func (c *Client) Clock() clock.Clock { return c.clk }

// Errorf builds a namespaced error with one of this package's code suffixes. It
// exists so that a domain client can raise, say, `alertmanager_malformed_response`
// for a body that decoded but made no sense, using the same taxonomy.
func (c *Client) Errorf(suffix string, cause error) *errs.Error {
	return newError(c.prefix, suffix, cause)
}

// Response is the metadata of a completed call.
type Response struct {
	// StatusCode is the final status.
	StatusCode int
	// Header is the response header. Bodies are not exposed: GetJSON owns
	// decoding, so that the size cap can never be bypassed.
	Header http.Header
	// Date is the upstream's own clock as advertised in the Date header, or the
	// zero time. This is oto's only cheap skew signal against an Alertmanager
	// (C12), and it has one-second granularity.
	Date time.Time
	// Bytes is the decoded body length.
	Bytes int64
	// Attempts is how many round trips it took.
	Attempts int
}

// GetJSON performs GET base+path with the given query and decodes a JSON body
// into out.
//
// Decoding is LENIENT: unknown fields are accepted, because an upstream is
// untrusted in shape as well as in content and the next Alertmanager release
// will add a field (SPEC §L.3.1). out may be nil to discard the body.
func (c *Client) GetJSON(ctx context.Context, path string, q url.Values, out any) (Response, error) {
	body, resp, err := c.get(ctx, path, q)
	if err != nil {
		return resp, err
	}
	if out == nil {
		return resp, nil
	}

	ct := resp.Header.Get("Content-Type")
	if err := json.Unmarshal(body, out); err != nil {
		suffix := CodeMalformedResponse
		if !isJSONContentType(ct) {
			suffix = CodeUnexpectedContentType
		}
		return resp, newError(c.prefix, suffix, &bodyError{status: resp.StatusCode, snippet: snippet(body)})
	}
	return resp, nil
}

// get runs the attempt loop and returns the raw, size-capped 2xx body.
func (c *Client) get(ctx context.Context, path string, q url.Values) ([]byte, Response, error) {
	target := c.base + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}

	var meta Response
	var lastErr error

	for attempt := 1; attempt <= c.retry.MaxAttempts; attempt++ {
		meta.Attempts = attempt

		body, head, retryable, err := c.attempt(ctx, target)
		meta.StatusCode = head.StatusCode
		meta.Header = head.Header
		meta.Date = parseDate(head.Header.Get("Date"))
		if err == nil {
			meta.Bytes = int64(len(body))
			return body, meta, nil
		}
		lastErr = err

		// A 4xx is terminal, always: retrying a 401 just burns the upstream's
		// rate limit, and retrying a 410 will never stop being a 410.
		if !retryable || attempt == c.retry.MaxAttempts {
			break
		}
		if err := c.sleep(ctx, c.retry.backoff(attempt)); err != nil {
			return nil, meta, newError(c.prefix, classifyTransport(ctx, err), err)
		}
	}
	return nil, meta, lastErr
}

// head is the part of a response that outlives the closed body.
type head struct {
	StatusCode int
	Header     http.Header
}

// attempt performs exactly one round trip and CLOSES the body before returning,
// which is why it hands back a head rather than an *http.Response. The bool
// reports whether the failure is worth another attempt: network errors and 5xx
// are, nothing else is.
func (c *Client) attempt(ctx context.Context, target string) ([]byte, head, bool, error) {
	var h head

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, h, false, newError(c.prefix, CodeInvalidRequest, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if err := c.auth.apply(req); err != nil {
		return nil, h, false, newError(c.prefix, CodeInvalidConfig, err)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		suffix := classifyTransport(ctx, err)
		// A cancelled or expired CALLER context is never retried: the budget
		// that would pay for the retry is exactly the one that just ran out.
		retryable := suffix == CodeUnreachable
		return nil, h, retryable, newError(c.prefix, suffix, err)
	}
	defer func() {
		// Drain a little so the connection can be reused, then close.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()
	h = head{StatusCode: resp.StatusCode, Header: resp.Header}

	body, tooLarge, readErr := c.readCapped(resp.Body)
	switch {
	case tooLarge:
		return nil, h, false, newError(c.prefix, CodeResponseTooLarge, nil)
	case readErr != nil:
		suffix := classifyTransport(ctx, readErr)
		return nil, h, suffix == CodeUnreachable, newError(c.prefix, suffix, readErr)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, h, false, nil
	}

	suffix := statusSuffix(resp.StatusCode)
	e := newError(c.prefix, suffix, &bodyError{status: resp.StatusCode, snippet: snippet(body)})
	if suffix == CodeRateLimited {
		if d, ok := c.retryAfter(resp.Header.Get("Retry-After")); ok {
			e = e.WithRetryAfter(d)
		}
	}
	// A 30x reaches here as CodeRejected because redirects are not followed.
	retryable := resp.StatusCode >= 500
	return nil, h, retryable, e
}

// readCapped reads at most maxBytes+1 and reports whether the cap was blown.
func (c *Client) readCapped(r io.Reader) (body []byte, tooLarge bool, err error) {
	buf, err := io.ReadAll(io.LimitReader(r, c.maxBytes+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(buf)) > c.maxBytes {
		return nil, true, nil
	}
	return buf, false, nil
}

// retryAfter parses a Retry-After header in either of its two legal forms. The
// HTTP-date form needs a "now", which comes from the injected clock and never
// from time.Now.
func (c *Client) retryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(c.clk.Now()); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

// parseDate reads an HTTP Date header, returning the zero time when it is absent
// or unparseable. It is deliberately not clock-dependent.
func parseDate(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// isJSONContentType reports whether ct is a JSON media type. An empty or
// unparseable Content-Type is treated as "maybe JSON": some Alertmanager builds
// behind a proxy omit it, and rejecting on that alone would be wrong.
func isJSONContentType(ct string) bool {
	if strings.TrimSpace(ct) == "" {
		return true
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return true
	}
	mt = strings.ToLower(mt)
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

// snippet renders a bounded, printable prefix of an upstream body for use as an
// error CAUSE. It never becomes an errs.Message: SPEC §L.1 keeps raw upstream
// payloads out of anything rendered to a caller.
func snippet(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if len(b) > snippetBytes {
		b = b[:snippetBytes]
	}
	var sb strings.Builder
	sb.Grow(len(b))
	for _, r := range string(bytes.ToValidUTF8(b, []byte("?"))) {
		if unicode.IsControl(r) {
			r = ' '
		}
		sb.WriteRune(r)
	}
	return strings.TrimSpace(sb.String())
}

// IsContextError reports whether err is the caller's own cancellation rather
// than an upstream fault. Callers use it to avoid counting their own shutdown as
// a source failure in source_health.
func IsContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		HasCode(err, CodeCanceled)
}
