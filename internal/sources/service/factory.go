package service

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpc"
	"github.com/thulasiram/oto/internal/sources/client/alertmanager"
	"github.com/thulasiram/oto/internal/sources/client/prometheus"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// DefaultUserAgent identifies oto to an upstream.
const DefaultUserAgent = "oto/1"

// HTTPClientFactory is the production ClientFactory: it builds real HTTP clients
// over internal/platform/httpc.
type HTTPClientFactory struct {
	// Timeout bounds one attempt against an upstream.
	Timeout time.Duration
	// MaxResponseBytes caps an upstream body.
	MaxResponseBytes int64
	// Retry is the in-band retry policy. 5xx and network only, never 4xx.
	Retry httpc.Retry
	// UserAgent identifies oto.
	UserAgent string
	// Clock is the time source shared with the clients.
	Clock clock.Clock
	// Transport, when set, replaces the built TLS transport. It exists so a test
	// can hand the whole factory an httptest.Server; production leaves it nil,
	// and setting it makes the per-source TLS options inert.
	Transport http.RoundTripper
	// Sleep, when set, replaces the retry sleeper.
	Sleep httpc.Sleeper

	// Dial is ⭐ THE SSRF CONTROL for every outbound call this module makes.
	//
	// A source's `base_url` and `prometheus_url` are tenant-supplied and oto dials
	// them from inside the operator's network, so `POST /sources/{id}/test` was a
	// working request forgery: any org member could point a source at
	// 169.254.169.254 and read the cloud metadata service out of the probe
	// response, which reflects upstream content back to the caller.
	//
	// The guard is installed as the DIALER rather than run as a pre-flight lookup
	// because those are two different questions. A pre-flight `LookupNetIP`
	// followed by `client.Do` resolves the name TWICE, and an attacker who serves
	// a TTL-0 record alternating public → link-local wins the second resolution
	// every time. This function is handed the address the socket is about to
	// connect to, and there is no later resolution for anyone to poison.
	//
	// Nil means UNGUARDED, which is correct only for a test.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)

	// AllowInsecureTLS is a DEPLOYMENT-LEVEL switch. `alert_sources.tls_skip_verify`
	// is a row a tenant can write, and honouring it unconditionally let any org
	// member turn off certificate verification for a connection oto's own process
	// makes. With this false the column is ignored and verification always
	// happens (§M2).
	AllowInsecureTLS bool
}

// HTTPClientFactory satisfies the port.
var _ ClientFactory = (*HTTPClientFactory)(nil)

// NewClientFactory builds the production factory with sane defaults.
func NewClientFactory(clk clock.Clock) *HTTPClientFactory {
	if clk == nil {
		clk = clock.New()
	}
	return &HTTPClientFactory{
		Timeout:          10 * time.Second,
		MaxResponseBytes: httpc.DefaultMaxResponseBytes,
		Retry:            httpc.DefaultRetry(),
		UserAgent:        DefaultUserAgent,
		Clock:            clk,
	}
}

// Alertmanager builds a v2 client for the source's base URL.
func (f *HTTPClientFactory) Alertmanager(src domain.Source, cred domain.Credential) (AlertmanagerClient, error) {
	return alertmanager.New(alertmanager.Config{
		BaseURL:          src.BaseURL,
		Auth:             toHTTPAuth(cred),
		TLS:              httpc.TLSOptions{SkipVerify: f.skipVerify(src)},
		Timeout:          f.Timeout,
		MaxResponseBytes: f.MaxResponseBytes,
		Retry:            f.Retry,
		UserAgent:        f.userAgent(),
		Clock:            f.clock(),
		Transport:        f.Transport,
		DialContext:      f.Dial,
		Sleep:            f.Sleep,
	})
}

// Prometheus builds a v1 client for the source's Prometheus, or for overrideURL.
func (f *HTTPClientFactory) Prometheus(src domain.Source, cred domain.Credential, overrideURL string) (PrometheusClient, error) {
	base := overrideURL
	if base == "" {
		base = src.PrometheusURL
	}
	if base == "" {
		return nil, errs.New(errs.KindValidation, "sources_prometheus_not_configured",
			"this source has no Prometheus URL, so rule definitions cannot be fetched")
	}
	return prometheus.New(prometheus.Config{
		BaseURL:          base,
		Auth:             toHTTPAuth(cred),
		TLS:              httpc.TLSOptions{SkipVerify: f.skipVerify(src)},
		Timeout:          f.Timeout,
		MaxResponseBytes: f.MaxResponseBytes,
		Retry:            f.Retry,
		UserAgent:        f.userAgent(),
		Clock:            f.clock(),
		Transport:        f.Transport,
		DialContext:      f.Dial,
		Sleep:            f.Sleep,
	})
}

func (f *HTTPClientFactory) userAgent() string {
	if f.UserAgent == "" {
		return DefaultUserAgent
	}
	return f.UserAgent
}

// skipVerify resolves the effective TLS posture for one source.
//
// ⛔ A TENANT CANNOT TURN OFF CERTIFICATE VERIFICATION. The column is honoured
// only when the DEPLOYMENT has opted in, because "trust an unverified
// certificate on this network" is a statement about the operator's network and
// not about one org's source. Rows written before that gate existed are ignored
// here rather than migrated, so an existing `true` cannot quietly keep working.
func (f *HTTPClientFactory) skipVerify(src domain.Source) bool {
	return f.AllowInsecureTLS && src.TLSSkipVerify
}

func (f *HTTPClientFactory) clock() clock.Clock {
	if f.Clock == nil {
		return clock.New()
	}
	return f.Clock
}

// toHTTPAuth maps the domain credential onto the transport credential. The two
// types are kept separate on purpose: domain.Credential is what the secret store
// returns, httpc.Auth is what a socket needs, and platform must not import a
// domain (the depguard rule that keeps the import graph acyclic).
func toHTTPAuth(c domain.Credential) httpc.Auth {
	switch c.Kind {
	case domain.AuthBearer:
		return httpc.Auth{Kind: httpc.AuthBearer, Token: c.Token}
	case domain.AuthBasic:
		return httpc.Auth{Kind: httpc.AuthBasic, Username: c.Username, Password: c.Password}
	case domain.AuthNone:
		return httpc.Auth{Kind: httpc.AuthNone}
	default:
		return httpc.Auth{Kind: httpc.AuthNone}
	}
}
