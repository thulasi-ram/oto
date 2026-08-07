package httpc

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/validate"
)

// Transport and payload defaults.
const (
	// DefaultTimeout bounds ONE attempt, not the whole call.
	DefaultTimeout = 10 * time.Second

	// DefaultMaxResponseBytes caps a decoded body. A 10 000-alert
	// GET /api/v2/alerts is the sizing case (SPEC §L.3.2 B2).
	DefaultMaxResponseBytes int64 = 32 << 20

	// MaxBaseURLBytes mirrors the `max=2048` on base_url and prometheus_url in
	// the source DTOs (SPEC §L.2.5).
	MaxBaseURLBytes = 2048

	// snippetBytes bounds how much of an upstream error body is kept as a cause.
	snippetBytes = 512

	// defaultIdleConns sizes the shared connection pool per client.
	defaultIdleConns = 8
)

// AuthKind is the subset of channel_credentials.kind that an outbound HTTP
// client understands (SPEC §D.8). An AlertSource uses exactly these three.
type AuthKind string

// The outbound auth kinds.
const (
	// AuthNone sends no credential.
	AuthNone AuthKind = "none"
	// AuthBearer sends `Authorization: Bearer <token>`.
	AuthBearer AuthKind = "bearer"
	// AuthBasic sends RFC 7617 basic credentials.
	AuthBasic AuthKind = "basic"
)

// Auth is a resolved outbound credential. Its fields are secrets: they are never
// logged, never rendered into an errs.Message and never put in a URL.
type Auth struct {
	Kind     AuthKind
	Token    string
	Username string
	Password string
}

// apply stamps the credential onto req.
func (a Auth) apply(req *http.Request) error {
	switch a.Kind {
	case "", AuthNone:
		return nil
	case AuthBearer:
		if a.Token == "" {
			return errors.New("bearer auth configured with an empty token")
		}
		req.Header.Set("Authorization", "Bearer "+a.Token)
		return nil
	case AuthBasic:
		if a.Username == "" {
			return errors.New("basic auth configured with an empty username")
		}
		req.SetBasicAuth(a.Username, a.Password)
		return nil
	default:
		return errors.New("unknown auth kind " + string(a.Kind))
	}
}

// TLSOptions carries the per-source TLS configuration.
//
// SkipVerify mirrors alert_sources.tls_skip_verify: an explicit, per-source,
// operator opt-in for the self-signed certificates that in-cluster Alertmanagers
// habitually ship with. It is never a default and never inferred.
type TLSOptions struct {
	SkipVerify bool
	CACertPEM  []byte
	ServerName string
	MinVersion uint16
}

// Retry is the in-band retry policy.
//
// It is deliberately far tighter than the JOB retry policy of SPEC §G.6 (base
// 2 s, cap 300 s, 12 attempts): that budget belongs to a river job that may sleep
// for minutes, while these attempts all have to fit inside one caller's context.
// Only 5xx and network failures are retried; a 4xx is terminal, always.
type Retry struct {
	MaxAttempts    int
	Base           time.Duration
	Max            time.Duration
	JitterFraction float64
}

// DefaultRetry is three attempts over roughly a second of backoff.
func DefaultRetry() Retry {
	return Retry{MaxAttempts: 3, Base: 250 * time.Millisecond, Max: 4 * time.Second, JitterFraction: 0.5}
}

// normalise clamps a Retry into a usable shape.
func (r Retry) normalise() Retry {
	if r.MaxAttempts <= 0 {
		r.MaxAttempts = 1
	}
	if r.MaxAttempts > 10 {
		r.MaxAttempts = 10
	}
	if r.Base <= 0 {
		r.Base = 250 * time.Millisecond
	}
	if r.Max <= 0 || r.Max < r.Base {
		r.Max = 10 * r.Base
	}
	if r.JitterFraction < 0 || r.JitterFraction > 1 {
		r.JitterFraction = 0.5
	}
	return r
}

// backoff returns the delay before attempt n (1-based), exponential with factor
// 2, capped at Max, with +/- JitterFraction of jitter.
func (r Retry) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 20 {
		shift = 20
	}
	d := time.Duration(float64(r.Base) * math.Pow(2, float64(shift)))
	if d > r.Max || d <= 0 {
		d = r.Max
	}
	return jitter(d, r.JitterFraction)
}

// jitter spreads d by +/- frac. math/rand is banned repo-wide, so the randomness
// comes from crypto/rand; a read failure degrades to the undithered delay rather
// than to a panic, because a thundering herd is better than a dead client.
func jitter(d time.Duration, frac float64) time.Duration {
	if frac <= 0 || d <= 0 {
		return d
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return d
	}
	// Map to [-1, +1).
	u := float64(binary.BigEndian.Uint64(buf[:])>>11) / float64(uint64(1)<<53)
	out := float64(d) * (1 + frac*(2*u-1))
	if out < 0 {
		return 0
	}
	return time.Duration(out)
}

// Sleeper waits for d or until ctx is done. It is injectable so that retry
// behaviour is testable without real time passing.
type Sleeper func(ctx context.Context, d time.Duration) error

// realSleep is the default Sleeper.
func realSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Config builds a Client. Every field has a usable zero value except BaseURL and
// ErrPrefix.
type Config struct {
	// BaseURL is the upstream root. It is normalised on construction and must
	// survive alert_sources_base_ck: absolute http(s), no whitespace, no
	// trailing slash.
	BaseURL string

	// Auth is the resolved outbound credential.
	Auth Auth

	// TLS carries per-source certificate handling.
	TLS TLSOptions

	// Timeout bounds one attempt. Zero means DefaultTimeout.
	Timeout time.Duration

	// MaxResponseBytes caps a body. Zero means DefaultMaxResponseBytes.
	MaxResponseBytes int64

	// Retry is the in-band retry policy. A zero value means DefaultRetry.
	Retry Retry

	// UserAgent identifies oto to the upstream.
	UserAgent string

	// ErrPrefix namespaces every error Code this client produces, so that a
	// caller can tell an Alertmanager failure from a Prometheus one without
	// unwrapping. Required.
	ErrPrefix string

	// Clock is the time source. Required in production wiring; defaults to the
	// system clock so that a bare Config still works.
	Clock clock.Clock

	// Transport overrides the built transport. Leave nil in production; set it
	// from an httptest.Server in tests.
	Transport http.RoundTripper

	// Sleep overrides the retry sleeper. Leave nil in production.
	Sleep Sleeper
}

// NormalizeBaseURL returns the canonical form of an upstream base URL, or a
// validation error.
//
// The rules are exactly alert_sources_base_ck plus the two things a regex cannot
// express: the URL must parse, and it must carry no credentials (a userinfo
// section ends up in every log line that prints a URL). A trailing slash is
// removed rather than rejected, because the DDL stores the trimmed form and
// every path in this package is written to be concatenated onto it.
func NormalizeBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	switch {
	case s == "":
		return "", errs.New(errs.KindValidation, CodeInvalidBaseURL, "a base URL is required")
	case len(s) > MaxBaseURLBytes:
		return "", errs.Newf(errs.KindValidation, CodeInvalidBaseURL,
			"a base URL must be at most %d bytes", MaxBaseURLBytes)
	}

	// Trim before the regex so that "https://am.example.com/" is normalised
	// rather than rejected; the regex itself then guards charset and scheme.
	s = strings.TrimRight(s, "/")
	if !validate.HTTPURLRe.MatchString(s) {
		return "", errs.Newf(errs.KindValidation, CodeInvalidBaseURL,
			"a base URL must match %s", validate.PatternHTTPURL)
	}

	u, err := url.Parse(s)
	if err != nil {
		return "", errs.Wrap(err, errs.KindValidation, CodeInvalidBaseURL, "the base URL could not be parsed")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", errs.New(errs.KindValidation, CodeInvalidBaseURL, "a base URL must use http or https")
	case u.Host == "":
		return "", errs.New(errs.KindValidation, CodeInvalidBaseURL, "a base URL must name a host")
	case u.User != nil:
		return "", errs.New(errs.KindValidation, CodeInvalidBaseURL,
			"a base URL must not embed credentials; configure an auth credential instead")
	case u.RawQuery != "" || u.ForceQuery:
		return "", errs.New(errs.KindValidation, CodeInvalidBaseURL, "a base URL must not carry a query string")
	case u.Fragment != "":
		return "", errs.New(errs.KindValidation, CodeInvalidBaseURL, "a base URL must not carry a fragment")
	}

	out := u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/")
	return out, nil
}

// buildTransport turns TLSOptions into a RoundTripper.
func buildTransport(o TLSOptions) (http.RoundTripper, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("http.DefaultTransport is not an *http.Transport")
	}
	tr := base.Clone()
	tr.MaxIdleConnsPerHost = defaultIdleConns
	tr.ForceAttemptHTTP2 = true

	minVersion := o.MinVersion
	if minVersion == 0 {
		minVersion = tls.VersionTLS12
	}
	// #nosec G402 -- InsecureSkipVerify is alert_sources.tls_skip_verify: an
	// explicit, per-source, operator-set opt-in for in-cluster self-signed
	// certificates. It is never a default and is surfaced in the UI.
	cfg := &tls.Config{
		MinVersion:         minVersion,
		InsecureSkipVerify: o.SkipVerify,
		ServerName:         o.ServerName,
	}
	if len(o.CACertPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(o.CACertPEM) {
			return nil, errors.New("no certificate found in the configured CA bundle")
		}
		cfg.RootCAs = pool
	}
	tr.TLSClientConfig = cfg
	return tr, nil
}
