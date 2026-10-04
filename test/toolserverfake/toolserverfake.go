// Package toolserverfake runs a real MCP server in-process, over TLS, for tests of the
// ToolServer path (git-bug 2e9a086): the official go-sdk's own server behind
// `httptest`, so the adapter under test speaks the real protocol to the real
// implementation rather than to a hand-rolled imitation of it.
//
// It lives under `test/`, beside `modelfake`, because the SDK is confined to its one
// adapter inside `internal/` (depguard: `mcp-sdk-is-adapter-only`); a test that needs a
// server builds it here and never names the SDK itself.
package toolserverfake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Call is what a Tool's handler is told about one call.
type Call struct {
	Arguments json.RawMessage
	// Authorization is the header the call arrived with, so a test can echo it back
	// and prove oto never records it.
	Authorization string
}

// Tool is one Tool the fake serves.
type Tool struct {
	Name        string
	Description string
	// ReadOnly sets the Tool's readOnlyHint annotation; nil sends no annotations.
	ReadOnly *bool
	// Handle answers a call: the text, and whether the Tool reports it failed.
	Handle func(ctx context.Context, c Call) (text string, isError bool)
}

// Server is a running fake.
type Server struct {
	// URL is the MCP endpoint.
	URL string
	// Client trusts the server's certificate. It is a plain client — no SSRF guard —
	// because the server is on loopback.
	Client *http.Client

	mu    sync.Mutex
	calls int
}

// Calls is how many Tool calls the server has answered.
func (s *Server) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// Options shape a fake.
type Options struct {
	// Token, when set, is the bearer token every request must carry; anything else is
	// a 401.
	Token string
	// SSE serves the older HTTP+SSE transport instead of streamable HTTP.
	SSE bool
}

// Start runs a fake until the test ends.
func Start(t testing.TB, opts Options, tools ...Tool) *Server {
	t.Helper()
	srv := &Server{}
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "toolserverfake", Version: "1"}, nil)
	for _, tool := range tools {
		def := &mcp.Tool{Name: tool.Name, Description: tool.Description,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}}
		if tool.ReadOnly != nil {
			def.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: *tool.ReadOnly}
		}
		mcpServer.AddTool(def, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			srv.mu.Lock()
			srv.calls++
			srv.mu.Unlock()
			c := Call{Arguments: req.Params.Arguments}
			if req.Extra != nil && req.Extra.Header != nil {
				c.Authorization = req.Extra.Header.Get("Authorization")
			}
			text, isError := tool.Handle(ctx, c)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: isError}, nil
		})
	}

	var handler http.Handler
	if opts.SSE {
		handler = mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)
	} else {
		handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)
	}
	if opts.Token != "" {
		inner := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+opts.Token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			inner.ServeHTTP(w, r)
		})
	}

	ts := httptest.NewTLSServer(handler)
	t.Cleanup(ts.Close)
	srv.URL = ts.URL + "/mcp"
	srv.Client = ts.Client()
	return srv
}
