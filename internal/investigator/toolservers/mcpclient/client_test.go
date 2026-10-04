package mcpclient

// The adapter against the SDK's own server, in-process over TLS (test/toolserverfake):
// a session opens, lists its Tools with their hints, calls one with the token in a
// header, hears a Tool's own failure as a result rather than an error, says a refused
// token is a refused token, and speaks the older SSE transport too. No Docker.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/toolserverfake"
)

const token = "tst-live-toolserver-token"

func cfg(t *testing.T, url string, transport domain.ToolServerTransport) domain.ToolServerConfig {
	t.Helper()
	limits, err := domain.NewCallLimits(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return domain.ToolServerConfig{Name: "k8s", URL: url, Transport: transport, Access: domain.AccessRead, Limits: limits}
}

func ptr(b bool) *bool { return &b }

func fakeTools() []toolserverfake.Tool {
	return []toolserverfake.Tool{
		{Name: "pods_list", Description: "List pods in a namespace.", ReadOnly: ptr(true),
			Handle: func(_ context.Context, c toolserverfake.Call) (string, bool) {
				return `{"pods":["api-1"],"args":` + string(c.Arguments) + `,"auth":"` + c.Authorization + `"}`, false
			}},
		{Name: "pods_delete", Description: "Delete a pod.",
			Handle: func(context.Context, toolserverfake.Call) (string, bool) { return "pod not found", true }},
	}
}

func TestASessionListsAndCallsWithTheTokenInAHeader(t *testing.T) {
	for _, transport := range []domain.ToolServerTransport{domain.TransportStreamableHTTP, domain.TransportSSE} {
		t.Run(string(transport), func(t *testing.T) {
			srv := toolserverfake.Start(t, toolserverfake.Options{Token: token, SSE: transport == domain.TransportSSE}, fakeTools()...)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			s, err := Dialer{HTTPClient: srv.Client}.Connect(ctx, cfg(t, srv.URL, transport), token)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer func() { _ = s.Close() }()

			tools, err := s.ListTools(ctx)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(tools) != 2 {
				t.Fatalf("listed %+v", tools)
			}
			byName := map[string]domain.DiscoveredTool{}
			for _, tl := range tools {
				byName[tl.Name] = tl
			}
			list := byName["pods_list"]
			if list.ReadOnlyHint == nil || !*list.ReadOnlyHint || list.Description != "List pods in a namespace." ||
				!json.Valid(list.InputSchema) {
				t.Fatalf("pods_list = %+v", list)
			}
			if byName["pods_delete"].ReadOnlyHint != nil {
				t.Fatal("a Tool with no annotations was given a hint")
			}

			res, err := s.CallTool(ctx, "pods_list", json.RawMessage(`{"namespace":"payments"}`))
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if res.IsError || !strings.Contains(res.Text, `"namespace":"payments"`) {
				t.Fatalf("result = %+v", res)
			}
			// The fake refuses any request without the token, so reaching it at all is
			// the proof the header went; over streamable HTTP the SDK also hands the
			// handler the header, and the fake echoes it so the service's scrub has
			// something to scrub (the SSE server does not pass it through).
			if transport == domain.TransportStreamableHTTP && !strings.Contains(res.Text, "Bearer "+token) {
				t.Fatalf("result = %+v, want the echoed header", res)
			}

			// ⭐ The Tool's own failure is a result the model reads, not an error.
			res, err = s.CallTool(ctx, "pods_delete", nil)
			if err != nil || !res.IsError || res.Text != "pod not found" {
				t.Fatalf("isError result = %+v, %v", res, err)
			}
			if srv.Calls() != 2 {
				t.Fatalf("the server answered %d calls", srv.Calls())
			}
		})
	}
}

func TestARefusedTokenIsSaidAsOne(t *testing.T) {
	srv := toolserverfake.Start(t, toolserverfake.Options{Token: token}, fakeTools()...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Dialer{HTTPClient: srv.Client}.Connect(ctx, cfg(t, srv.URL, domain.TransportStreamableHTTP), "wrong-token")
	if errs.CodeOf(err) != "tool_server_auth_failed" {
		t.Fatalf("err = %v, want tool_server_auth_failed", err)
	}
	if e, _ := errs.As(err); e != nil && strings.Contains(e.Message, "wrong-token") {
		t.Fatal("the token reached the message")
	}
}

func TestARedirectIsNotFollowed(t *testing.T) {
	var hits int
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, "should not be reached", http.StatusTeapot)
	}))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Dialer{HTTPClient: redirect.Client()}.Connect(ctx, cfg(t, redirect.URL+"/mcp", domain.TransportStreamableHTTP), token)
	if err == nil || hits != 0 {
		t.Fatalf("err = %v, the redirect target was hit %d time(s); the token would have gone with it", err, hits)
	}
}

func TestATokenIsNeverSentOverPlaintext(t *testing.T) {
	_, err := Dialer{HTTPClient: http.DefaultClient}.Connect(context.Background(),
		cfg(t, "http://k8s-mcp.tools.svc:8080/mcp", domain.TransportStreamableHTTP), token)
	if errs.CodeOf(err) != "tool_server_token_needs_https" {
		t.Fatalf("err = %v", err)
	}
}

func TestAnOversizedAnswerIsAFailureNotAnExhaustedWorker(t *testing.T) {
	srv := toolserverfake.Start(t, toolserverfake.Options{}, toolserverfake.Tool{Name: "huge",
		Handle: func(context.Context, toolserverfake.Call) (string, bool) { return strings.Repeat("x", 64<<10), false }})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Dialer{HTTPClient: srv.Client, MaxResponseBytes: 16 << 10}.Connect(ctx, cfg(t, srv.URL, domain.TransportStreamableHTTP), "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.CallTool(ctx, "huge", nil); err == nil {
		t.Fatal("a 64 KiB answer was read past a 16 KiB ceiling")
	}
}

// TestASessionOutlivesTheCallThatOpenedIt — a run opens a session on its first call,
// under that call's timeout, and makes its next call after that context is gone.
func TestASessionOutlivesTheCallThatOpenedIt(t *testing.T) {
	for _, transport := range []domain.ToolServerTransport{domain.TransportStreamableHTTP, domain.TransportSSE} {
		t.Run(string(transport), func(t *testing.T) {
			srv := toolserverfake.Start(t, toolserverfake.Options{SSE: transport == domain.TransportSSE}, fakeTools()...)
			first, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			s, err := Dialer{HTTPClient: srv.Client}.Connect(first, cfg(t, srv.URL, transport), "")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			if _, err := s.CallTool(first, "pods_list", nil); err != nil {
				t.Fatal(err)
			}
			cancel()

			next, cancelNext := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelNext()
			if _, err := s.CallTool(next, "pods_list", nil); err != nil {
				t.Fatalf("the second call, after the first call's context ended: %v", err)
			}
		})
	}
}

// TestAToolServerThatNeverAnswersIsBoundedByTheCallersDeadline.
func TestAToolServerThatNeverAnswersIsBoundedByTheCallersDeadline(t *testing.T) {
	stuck := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second): // past every deadline below, short enough to close
		}
	}))
	defer stuck.Close()
	for _, transport := range []domain.ToolServerTransport{domain.TransportStreamableHTTP, domain.TransportSSE} {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		began := time.Now()
		_, err := Dialer{HTTPClient: stuck.Client()}.Connect(ctx, cfg(t, stuck.URL+"/mcp", transport), "")
		cancel()
		if err == nil || time.Since(began) > time.Second {
			t.Fatalf("%s: err = %v after %s", transport, err, time.Since(began))
		}
	}
}
