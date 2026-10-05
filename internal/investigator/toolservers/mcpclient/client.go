// Package mcpclient is the one ToolServer adapter (ADR 0053 §3, git-bug 2e9a086): MCP
// over HTTP, spoken through the official `github.com/modelcontextprotocol/go-sdk` client.
//
// ⭐ BOTH HTTP TRANSPORTS, AND NOTHING ELSE. Streamable HTTP is MCP's current transport and
// the older HTTP+SSE one is the same few lines in the SDK, so a ToolServer that has not
// moved still works. ⛔ There is no stdio transport and there will not be one: a stdio
// server is a subprocess oto would start, and oto runs no subprocess — a ToolServer is a
// server its operator runs, secures and grants RBAC to (ADR 0016, 0054 §5).
//
// ⛔ THE SDK'S TYPES STOP AT THIS PACKAGE'S EDGE. Nothing outside it names `mcp.*`; the
// port is `investigator/domain.ToolServerClient` (depguard: `mcp-sdk-is-adapter-only`).
//
// ⛔ THE ACCESS TOKEN IS A HEADER THIS PACKAGE ADDS AND NOTHING ELSE SEES. It is set per
// request by the round tripper below, which also refuses to follow a redirect — a
// redirected request would carry the header to wherever the ToolServer pointed it.
//
// ⛔ AND IT GOES ONLY TO THE ORIGIN THE OPERATOR CONFIGURED (review A2). The SSE
// transport POSTs to whatever URL the server's `endpoint` event names, resolved against
// the stream's URL — any scheme, any host. A ToolServer (or anything that can speak on
// its stream) could otherwise point oto's token at another host, or at plain http. The
// round tripper refuses every request whose scheme or host is not the configured URL's,
// before anything is sent: `tool_server_endpoint_off_origin`. This is the one check on
// every session — an Investigation's read calls and a Remedy's write call alike.
package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// DefaultMaxResponseBytes bounds one HTTP response body, and one server-sent event, from
// a ToolServer. The per-call result cap (domain.CallLimits) is what the model reads;
// this is what oto will hold in memory to get there, so a ToolServer answering with a
// gigabyte is a failed call rather than an exhausted worker.
const DefaultMaxResponseBytes int64 = 8 << 20

// streamRetries is how often the streamable transport may reconnect a broken response
// stream. A Tool here only reads, so a resumed stream costs nothing but time, and the
// per-call timeout bounds that.
const streamRetries = 2

// Dialer opens sessions with ToolServers. It is the concrete half of
// `investigator/service.ToolServerDialer`, and carries what every ToolServer in a
// deployment shares: the guarded HTTP client.
type Dialer struct {
	// HTTPClient is REQUIRED. `internal/app` passes one whose transport dials through
	// `platform/netguard`: a ToolServer's URL is operator-supplied, and this is an SSRF
	// surface like every other URL oto dials. Its Timeout should be zero — the per-call
	// timeout bounds every request through ctx, and an SSE stream outlives any one call.
	HTTPClient *http.Client
	// MaxResponseBytes is DefaultMaxResponseBytes when zero.
	MaxResponseBytes int64
}

// Connect implements `investigator/service.ToolServerDialer`: one initialised session.
// The token is held by the session's round tripper and by nothing else.
func (d Dialer) Connect(ctx context.Context, cfg domain.ToolServerConfig, token string) (domain.ToolServerClient, error) {
	if d.HTTPClient == nil {
		return nil, errs.New(errs.KindInternal, "tool_server_http_client_missing",
			"the ToolServer adapter needs an HTTP client; internal/app supplies the guarded one")
	}
	if err := domain.TokenNeedsHTTPS(cfg.URL, token != ""); err != nil {
		return nil, err
	}
	origin, err := url.Parse(cfg.URL)
	if err != nil || origin.Host == "" {
		return nil, errs.Newf(errs.KindInternal, "tool_server_url_invalid",
			"the ToolServer %s has a URL oto cannot parse", cfg.Name)
	}
	limit := d.MaxResponseBytes
	if limit <= 0 {
		limit = DefaultMaxResponseBytes
	}
	base := d.HTTPClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	rt := &roundTripper{base: base, token: token, limit: limit, scheme: origin.Scheme, host: hostPort(origin)}
	hc := &http.Client{
		Transport: rt,
		Timeout:   d.HTTPClient.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	life, endLife := context.WithCancel(context.WithoutCancel(ctx))
	var transport mcp.Transport
	switch cfg.Transport {
	case domain.TransportSSE:
		// ⚠️ THE SSE TRANSPORT'S STREAM LIVES ON THE CONTEXT IT IS CONNECTED WITH, and
		// the caller's is one call's timeout: a session opened on a run's first call
		// would lose its stream the moment that call returned. So the stream gets the
		// session's own lifetime, ended by Close, and the caller's deadline still
		// bounds the wait for it (detached.Connect).
		transport = &detached{inner: &mcp.SSEClientTransport{Endpoint: cfg.URL, HTTPClient: hc, MaxEventSize: int(limit)},
			life: life, end: endLife}
	case domain.TransportStreamableHTTP, "":
		transport = &mcp.StreamableClientTransport{
			Endpoint: cfg.URL, HTTPClient: hc, MaxRetries: streamRetries, MaxEventSize: int(limit),
			// oto asks and reads the answer; it listens for nothing a ToolServer might
			// volunteer, so it holds no standing stream open between calls.
			DisableStandaloneSSE: true,
		}
	default:
		endLife()
		return nil, errs.Newf(errs.KindInternal, "tool_server_transport_invalid",
			"the ToolServer %s has transport %q", cfg.Name, cfg.Transport)
	}

	// ⭐ NO HANDLERS: oto advertises no sampling, no elicitation and no roots it would
	// answer. A ToolServer cannot ask oto's model anything, nor ask a person through
	// oto — it answers calls, and that is all.
	client := mcp.NewClient(&mcp.Implementation{Name: "oto", Version: "1"}, nil)
	cs, err := bounded(ctx, func() (*mcp.ClientSession, error) {
		cs, err := client.Connect(ctx, transport, nil)
		if err == nil && ctx.Err() != nil {
			// Connected after the caller gave up: nobody will hold this session.
			go closeQuietly(cs, endLife)
			return nil, ctx.Err()
		}
		return cs, err
	})
	if err != nil {
		endLife()
		return nil, mapErr(ctx, cfg.Name, rt, err)
	}
	return &Session{name: cfg.Name, cs: cs, rt: rt, end: endLife}, nil
}

// bounded runs fn and returns when it does or when ctx ends, whichever is first.
//
// ⚠️ THE SDK OWES A CANCELLED REQUEST A GOODBYE, AND WAITS FOR IT. On a deadline it sends
// `notifications/cancelled` and waits up to five seconds for that to be delivered — to a
// ToolServer that has just shown it is not answering. A per-call timeout is a promise
// about when the Step is written, so the caller is answered at the deadline and the
// SDK's goodbye finishes on its own.
func bounded[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := fn()
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

func closeQuietly(cs *mcp.ClientSession, end context.CancelFunc) {
	_ = cs.Close()
	end()
}

// detached connects its transport on the session's lifetime rather than the caller's
// context, while still giving up — and ending that lifetime — when the caller's
// deadline passes first, so a ToolServer that never answers cannot hold a call past
// its timeout.
type detached struct {
	inner mcp.Transport
	life  context.Context
	end   context.CancelFunc
}

func (d *detached) Connect(ctx context.Context) (mcp.Connection, error) {
	type result struct {
		conn mcp.Connection
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := d.inner.Connect(d.life)
		done <- result{conn, err}
	}()
	select {
	case r := <-done:
		return r.conn, r.err
	case <-ctx.Done():
		d.end()
		return nil, ctx.Err()
	}
}

// Session is one open MCP session with one ToolServer.
type Session struct {
	name string
	cs   *mcp.ClientSession
	rt   *roundTripper
	end  context.CancelFunc
}

var _ domain.ToolServerClient = (*Session)(nil)

// ListTools implements domain.ToolServerClient, following every page — and stopping one
// past domain.MaxDiscoveredTools, which the caller refuses.
func (s *Session) ListTools(ctx context.Context) ([]domain.DiscoveredTool, error) {
	var (
		out    []domain.DiscoveredTool
		cursor string
	)
	for {
		res, err := bounded(ctx, func() (*mcp.ListToolsResult, error) {
			return s.cs.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		})
		if err != nil {
			return nil, mapErr(ctx, s.name, s.rt, err)
		}
		for _, t := range res.Tools {
			if t == nil {
				continue
			}
			schema, err := json.Marshal(t.InputSchema)
			if err != nil {
				schema = nil
			}
			var hint *bool
			if t.Annotations != nil {
				h := t.Annotations.ReadOnlyHint
				hint = &h
			}
			d, err := domain.NewDiscoveredTool(t.Name, t.Description, schema, hint)
			if err != nil {
				return nil, err
			}
			out = append(out, d)
		}
		if res.NextCursor == "" || len(out) > domain.MaxDiscoveredTools {
			return out, nil
		}
		cursor = res.NextCursor
	}
}

// CallTool implements domain.ToolServerClient. The ToolServer's own `isError` comes
// back as a ToolResult, not an error: the model reads why.
func (s *Session) CallTool(ctx context.Context, name string, args json.RawMessage) (domain.ToolResult, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	res, err := bounded(ctx, func() (*mcp.CallToolResult, error) {
		return s.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	})
	if err != nil {
		return domain.ToolResult{}, mapErr(ctx, s.name, s.rt, err)
	}
	return domain.ToolResult{Text: render(res), IsError: res.IsError}, nil
}

// Close implements domain.ToolServerClient: the session, then its lifetime. It returns
// at once — ending a session sends a DELETE the SDK waits up to five seconds on, and a
// run's ending is not held for a ToolServer's goodbye.
func (s *Session) Close() error {
	go closeQuietly(s.cs, s.end)
	return nil
}

// render is a result as the text a model reads: every text part, in order, with a
// placeholder for a part that is not text — an image is not something a Step can keep,
// and a model told nothing would not know one was there. A result with no content but a
// structured value is that value, as JSON.
func render(res *mcp.CallToolResult) string {
	parts := make([]string, 0, len(res.Content))
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, v.Text)
		case *mcp.ImageContent:
			parts = append(parts, fmt.Sprintf("[image omitted: %s, %d bytes]", v.MIMEType, len(v.Data)))
		case *mcp.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio omitted: %s, %d bytes]", v.MIMEType, len(v.Data)))
		case *mcp.ResourceLink:
			parts = append(parts, "[resource link: "+v.URI+"]")
		case *mcp.EmbeddedResource:
			switch {
			case v.Resource == nil:
				parts = append(parts, "[embedded resource omitted]")
			case v.Resource.Text != "":
				parts = append(parts, v.Resource.Text)
			default:
				parts = append(parts, fmt.Sprintf("[embedded resource omitted: %s, %d bytes]", v.Resource.URI, len(v.Resource.Blob)))
			}
		default:
			parts = append(parts, "[content omitted: a kind oto does not read]")
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			return string(b)
		}
	}
	return strings.Join(parts, "\n")
}

// mapErr turns an SDK failure into an errs.Error whose message is safe to record.
//
// ⛔ THE SDK'S ERROR TEXT IS NOT THE MESSAGE. A transport error names the URL (which
// carries no secret — NormalizeToolServerURL refuses one), but its text is the SDK's
// prose and changes between versions; the message here is oto's, and the cause is kept
// for a log. A ToolServer's own JSON-RPC error IS quoted, clipped: it is what tells the
// model its arguments were wrong. The service scrubs the token from all of it anyway.
func mapErr(ctx context.Context, server string, rt *roundTripper, err error) error {
	// First: a refused off-origin request can surface as anything — a broken stream, a
	// timeout waiting for the answer it never got — and it is the reason.
	if rt.offOrigin.Load() || errors.Is(err, errOffOrigin) {
		return errs.UpstreamDown("tool_server_endpoint_off_origin",
			fmt.Sprintf("the ToolServer %s told oto to send its requests to another origin than its configured URL; "+
				"oto sends its access token nowhere else", server), errOffOrigin)
	}
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errs.Wrap(err, errs.KindUpstreamSlow, "tool_server_timeout",
			fmt.Sprintf("the ToolServer %s did not answer in time", server))
	}
	var rpc *jsonrpc.Error
	if errors.As(err, &rpc) {
		return errs.UpstreamDown("tool_server_refused",
			fmt.Sprintf("the ToolServer %s answered with an error (%d): %s", server, rpc.Code, clip(rpc.Message, 512)), nil)
	}
	if st := rt.lastStatus.Load(); st == http.StatusUnauthorized || st == http.StatusForbidden {
		return errs.UpstreamDown("tool_server_auth_failed",
			fmt.Sprintf("the ToolServer %s refused oto's access token (%d)", server, st), err)
	}
	if errors.Is(err, errBodyTooLarge) {
		return errs.UpstreamDown("tool_server_answer_too_large",
			fmt.Sprintf("the ToolServer %s answered with more than oto will read", server), err)
	}
	return errs.UpstreamDown("tool_server_unreachable",
		fmt.Sprintf("the ToolServer %s could not be reached or did not speak MCP", server), err)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n] + "…"
}

// roundTripper adds the access token to every request, refuses any request off the
// configured origin, bounds every response body it can, and remembers the last refusal
// so an auth failure is said as one.
type roundTripper struct {
	base  http.RoundTripper
	token string
	limit int64
	// scheme and host are the configured URL's origin (host with its port, the default
	// one filled in); no request leaves for any other.
	scheme, host string
	lastStatus   atomic.Int32
	offOrigin    atomic.Bool
}

var errOffOrigin = errors.New("mcpclient: a request was addressed off the ToolServer's configured origin")

// hostPort is a URL's host with its port, the scheme's default filled in, lowercased —
// so `https://k8s.test` and `https://K8S.test:443` are one origin.
func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return strings.ToLower(u.Hostname()) + ":" + port
}

func (rt *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.EqualFold(req.URL.Scheme, rt.scheme) || hostPort(req.URL) != rt.host {
		// ⛔ Refused, not sent without the header: a ToolServer that points oto elsewhere
		// is not one oto keeps talking to.
		rt.offOrigin.Store(true)
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, errOffOrigin
	}
	if rt.token != "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+rt.token)
	}
	resp, err := rt.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		rt.lastStatus.Store(int32(resp.StatusCode))
	}
	// A stream's events are bounded one by one by MaxEventSize; every other body is
	// bounded whole here.
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		resp.Body = &limitedBody{rc: resp.Body, left: rt.limit}
	}
	return resp, nil
}

var errBodyTooLarge = errors.New("mcpclient: the ToolServer's answer is larger than oto will read")

type limitedBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		// One byte more tells a body of exactly the limit from a longer one.
		var probe [1]byte
		if n, _ := b.rc.Read(probe[:]); n > 0 {
			return 0, errBodyTooLarge
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
