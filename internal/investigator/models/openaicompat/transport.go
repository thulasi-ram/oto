package openaicompat

// THE CLIENT EVERY REQUEST GOES THROUGH (reviews A3, A8). `internal/app` hands the
// adapter a client that dials through `platform/netguard`; this wraps it with the two
// things the SDK does not do for an operator-supplied endpoint:
//
//   - ⛔ A BOUNDED ANSWER. The SDK reads a response with io.ReadAll, so an endpoint that
//     answers with a gigabyte — misconfigured, or hostile — would be held whole in the
//     shared worker's memory. Every body is cut at DefaultMaxResponseBytes, and one
//     past it is `model_answer_too_large`: a failed turn, not an exhausted process.
//   - ⛔ NO REDIRECT IS FOLLOWED. Go's client re-sends `Authorization` on a redirect to
//     the same host, including one that downgrades https to http, so the API key would
//     cross the wire in the clear to wherever the endpoint pointed it. A redirect is
//     refused before it is followed (`model_request_refused`); an endpoint that moved
//     is re-configured by its operator, not chased by oto.

import (
	"errors"
	"io"
	"net/http"
)

// DefaultMaxResponseBytes bounds one response body from a model endpoint. A turn is a
// few kilobytes of JSON — MaxTurnOutputTokens of text and its Tool calls — so 32 MiB is
// room for any honest answer and a ceiling on a dishonest one.
const DefaultMaxResponseBytes int64 = 32 << 20

var (
	errAnswerTooLarge  = errors.New("openaicompat: the model endpoint's answer is larger than oto will read")
	errRedirectRefused = errors.New("openaicompat: the model endpoint answered with a redirect, which oto does not follow")
)

// guarded wraps a client: the same transport and timeout, every body bounded to limit,
// and no redirect followed. The caller's client is not modified.
func guarded(c *http.Client, limit int64) *http.Client {
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	return &http.Client{
		Transport: &boundedTransport{base: base, limit: limit},
		Timeout:   c.Timeout,
		Jar:       c.Jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errRedirectRefused
		},
	}
}

// boundedTransport bounds every response body it returns.
type boundedTransport struct {
	base  http.RoundTripper
	limit int64
}

func (t *boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &limitedBody{rc: resp.Body, left: t.limit}
	return resp, nil
}

// limitedBody reads at most `left` bytes and then fails with errAnswerTooLarge if the
// body had more — never a silent truncation, which would decode as a different answer.
type limitedBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		// One byte more tells a body of exactly the limit from a longer one.
		var probe [1]byte
		if n, _ := b.rc.Read(probe[:]); n > 0 {
			return 0, errAnswerTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }
