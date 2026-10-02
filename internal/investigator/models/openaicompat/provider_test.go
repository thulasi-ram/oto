package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/modelcontract"
	"github.com/thulasiram/oto/test/modelfake"
)

const testKey = "sk-test-0123456789-never-printed"

// endpoint is an httptest Chat Completions server that answers with canned bodies in
// order and records every request it received.
type endpoint struct {
	srv *httptest.Server

	mu       sync.Mutex
	bodies   [][]byte
	headers  []http.Header
	paths    []string
	answers  []canned
	answered int
}

type canned struct {
	status int
	body   []byte
}

// newEndpoint serves over TLS, because a key is only ever sent over https; the
// keyless plaintext case uses newPlainEndpoint.
func newEndpoint(t *testing.T, answers ...canned) *endpoint {
	t.Helper()
	return startEndpoint(t, httptest.NewTLSServer, answers...)
}

func newPlainEndpoint(t *testing.T, answers ...canned) *endpoint {
	t.Helper()
	return startEndpoint(t, httptest.NewServer, answers...)
}

func startEndpoint(t *testing.T, start func(http.Handler) *httptest.Server, answers ...canned) *endpoint {
	t.Helper()
	e := &endpoint{answers: answers}
	e.srv = start(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		defer e.mu.Unlock()
		e.bodies = append(e.bodies, body)
		e.headers = append(e.headers, r.Header.Clone())
		e.paths = append(e.paths, r.URL.Path)
		if e.answered >= len(e.answers) {
			http.Error(w, `{"error":{"message":"script exhausted","type":"test"}}`, http.StatusInternalServerError)
			return
		}
		a := e.answers[e.answered]
		e.answered++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		_, _ = w.Write(a.body)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *endpoint) reached() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.bodies)
}

func (e *endpoint) request(t *testing.T, i int) map[string]any {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	var out map[string]any
	if err := json.Unmarshal(e.bodies[i], &out); err != nil {
		t.Fatalf("request %d is not JSON: %v\n%s", i, err, e.bodies[i])
	}
	return out
}

func (e *endpoint) provider(t *testing.T, key string) *Provider {
	t.Helper()
	p, err := New(Config{
		BaseURL: e.srv.URL + "/v1/", Model: "gateway-model", APIKey: key,
		HTTPClient: e.srv.Client(), MaxRetries: 0,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func tool(t *testing.T, name, params string) domain.ToolSchema {
	t.Helper()
	s, err := domain.NewToolSchema(name, "desc of "+name, json.RawMessage(params))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestAdapterKeepsTheContract runs the shared contract against the adapter behind an
// httptest endpoint, each scripted answer rendered as a Chat Completions body.
func TestAdapterKeepsTheContract(t *testing.T) {
	modelcontract.Run(t, func(t *testing.T, steps []modelfake.Step) (domain.ModelProvider, func() int) {
		bodies := make([]canned, len(steps))
		for i, a := range steps {
			bodies[i] = canned{status: http.StatusOK, body: completionBody(t, i, a)}
		}
		e := newEndpoint(t, bodies...)
		return e.provider(t, testKey), e.reached
	})
}

func completionBody(t *testing.T, i int, a modelfake.Step) []byte {
	t.Helper()
	msg := map[string]any{"role": "assistant", "content": nil}
	if a.Text != "" {
		msg["content"] = a.Text
	}
	finish := "stop"
	if len(a.ToolCalls) > 0 {
		finish = "tool_calls"
		calls := make([]map[string]any, len(a.ToolCalls))
		for j, c := range a.ToolCalls {
			calls[j] = map[string]any{"id": c.ID, "type": "function",
				"function": map[string]any{"name": c.Name, "arguments": c.Arguments}}
		}
		msg["tool_calls"] = calls
	}
	body := map[string]any{
		"id": fmt.Sprintf("chatcmpl-%d", i), "object": "chat.completion", "created": 1790912040,
		"model":   "gateway-model",
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
	}
	if !a.NoUsage {
		body["usage"] = map[string]any{"prompt_tokens": a.Usage.InputTokens, "completion_tokens": a.Usage.OutputTokens,
			"total_tokens": a.Usage.Total()}
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRecordedToolCallingExchange replays a two-turn tool-calling exchange in the
// Chat Completions wire shape: the model asks for two Tools, oto answers both, the
// model concludes. It asserts both directions — what the adapter sent, and what it
// made of what came back.
func TestRecordedToolCallingExchange(t *testing.T) {
	e := newEndpoint(t,
		canned{http.StatusOK, fixture(t, "exchange_1_tool_calls.json")},
		canned{http.StatusOK, fixture(t, "exchange_2_final.json")},
	)
	p := e.provider(t, testKey)
	ctx := context.Background()

	req := domain.ModelRequest{
		Messages: []domain.Message{
			domain.SystemMessage("You investigate alerts for oto. Use only the Tools offered."),
			domain.UserMessage("Case 41: KubePodCrashLooping, namespace payments, pod api-7d9f."),
		},
		Tools: []domain.ToolSchema{
			tool(t, "logs_query", `{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`),
			tool(t, "k8s_describe", `{"type":"object","properties":{"kind":{"type":"string"},"namespace":{"type":"string"},"name":{"type":"string"}},"required":["kind","name"]}`),
		},
		MaxOutputTokens: 800,
	}

	first, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if first.Text != "" || first.Finish != domain.FinishToolCalls || len(first.ToolCalls) != 2 {
		t.Fatalf("first turn = %+v", first)
	}
	if first.ToolCalls[0] != (domain.ToolCall{ID: "call_Vq3n8YkR2b", Name: "logs_query",
		Arguments: `{"query":"namespace:payments AND (OOMKilled OR \"out of memory\")","limit":50}`}) {
		t.Fatalf("call 0 = %+v", first.ToolCalls[0])
	}
	if first.ToolCalls[1].ID != "call_Hc7p1WsT0d" || first.ToolCalls[1].Name != "k8s_describe" {
		t.Fatalf("call 1 = %+v", first.ToolCalls[1])
	}
	if first.Usage != (domain.Usage{InputTokens: 412, OutputTokens: 87}) {
		t.Fatalf("first usage = %+v", first.Usage)
	}

	// What went out on the wire, turn one.
	sent := e.request(t, 0)
	if sent["model"] != "gateway-model" {
		t.Fatalf("model = %v", sent["model"])
	}
	if sent["max_completion_tokens"] != float64(800) {
		t.Fatalf("max_completion_tokens = %v", sent["max_completion_tokens"])
	}
	msgs := sent["messages"].([]any)
	if len(msgs) != 2 || role(msgs[0]) != "system" || role(msgs[1]) != "user" {
		t.Fatalf("messages = %v", msgs)
	}
	tools := sent["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if tools[0].(map[string]any)["type"] != "function" || fn["name"] != "logs_query" ||
		fn["description"] != "desc of logs_query" {
		t.Fatalf("tool 0 = %v", tools[0])
	}
	if req := fn["parameters"].(map[string]any)["required"].([]any); len(req) != 1 || req[0] != "query" {
		t.Fatalf("tool 0 parameters lost their schema: %v", fn["parameters"])
	}
	if got := e.headers[0].Get("Authorization"); got != "Bearer "+testKey {
		t.Fatalf("Authorization = %q", got)
	}
	if e.paths[0] != "/v1/chat/completions" {
		t.Fatalf("path = %q", e.paths[0])
	}

	// Turn two: replay the calls and answer both.
	next := req
	next.Messages = append(append([]domain.Message(nil), req.Messages...),
		domain.AssistantMessage(first),
		domain.ToolResultMessage("call_Vq3n8YkR2b", `{"lines":["OOMKilled","OOMKilled"]}`),
		domain.ToolResultMessage("call_Hc7p1WsT0d", `{"limits":{"memory":"256Mi"}}`),
	)
	next.MaxOutputTokens = 0
	second, err := p.Complete(ctx, next)
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if !strings.HasPrefix(second.Text, "api-7d9f was OOMKilled") || len(second.ToolCalls) != 0 ||
		second.Finish != domain.FinishStop {
		t.Fatalf("second turn = %+v", second)
	}
	if second.Usage != (domain.Usage{InputTokens: 1630, OutputTokens: 41}) {
		t.Fatalf("second usage = %+v", second.Usage)
	}

	sent = e.request(t, 1)
	if _, capped := sent["max_completion_tokens"]; capped {
		t.Fatal("a zero cap was sent as a cap")
	}
	msgs = sent["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("turn two sent %d messages, want 5", len(msgs))
	}
	assistant := msgs[2].(map[string]any)
	if role(assistant) != "assistant" {
		t.Fatalf("message 2 = %v", assistant)
	}
	replayed := assistant["tool_calls"].([]any)
	if len(replayed) != 2 {
		t.Fatalf("replayed calls = %v", replayed)
	}
	rfn := replayed[0].(map[string]any)["function"].(map[string]any)
	if replayed[0].(map[string]any)["id"] != "call_Vq3n8YkR2b" || rfn["arguments"] != first.ToolCalls[0].Arguments {
		t.Fatalf("replayed call 0 = %v", replayed[0])
	}
	for i, wantID := range []string{"call_Vq3n8YkR2b", "call_Hc7p1WsT0d"} {
		m := msgs[3+i].(map[string]any)
		if role(m) != "tool" || m["tool_call_id"] != wantID {
			t.Fatalf("message %d = %v, want a tool result for %s", 3+i, m, wantID)
		}
	}
}

func role(m any) string { s, _ := m.(map[string]any)["role"].(string); return s }

func TestAnAnswerWithoutUsageFails(t *testing.T) {
	for _, name := range []string{"no_usage.json", "usage_without_counts.json"} {
		t.Run(name, func(t *testing.T) {
			e := newEndpoint(t, canned{http.StatusOK, fixture(t, name)})
			turn, err := e.provider(t, testKey).Complete(context.Background(),
				domain.ModelRequest{Messages: []domain.Message{domain.UserMessage("why is the pod restarting?")}})
			if !domain.IsUsageMissing(err) {
				t.Fatalf("err = %v, want ErrUsageMissing", err)
			}
			if turn.Text != "" {
				t.Fatalf("a turn without usage still returned text %q", turn.Text)
			}
		})
	}
}

func TestFailuresNeverCarryTheKey(t *testing.T) {
	cases := map[int]string{
		http.StatusUnauthorized:        "model_auth_failed",
		http.StatusTooManyRequests:     "model_rate_limited",
		http.StatusBadRequest:          "model_request_refused",
		http.StatusInternalServerError: "model_unavailable",
	}
	for status, code := range cases {
		t.Run(code, func(t *testing.T) {
			e := newEndpoint(t, canned{status, []byte(`{"error":{"message":"nope","type":"invalid_request_error","code":"x"}}`)})
			_, err := e.provider(t, testKey).Complete(context.Background(),
				domain.ModelRequest{Messages: []domain.Message{domain.UserMessage("x")}})
			if errs.CodeOf(err) != code || errs.KindOf(err) != errs.KindUpstreamDown {
				t.Fatalf("err = %v, want %s", err, code)
			}
			if domain.IsUsageMissing(err) {
				t.Fatal("an HTTP failure matched ErrUsageMissing")
			}
			e2, _ := errs.As(err)
			for _, rendered := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%+v", e2.Cause)} {
				if strings.Contains(rendered, testKey) {
					t.Fatalf("the key leaked into %q", rendered)
				}
			}
		})
	}
}

func TestNoKeyMeansNoAuthorizationHeader(t *testing.T) {
	// Over plain http: a keyless self-hosted endpoint on the cluster network.
	e := newPlainEndpoint(t, canned{http.StatusOK, fixture(t, "exchange_2_final.json")})
	if _, err := e.provider(t, "").Complete(context.Background(),
		domain.ModelRequest{Messages: []domain.Message{domain.UserMessage("x")}}); err != nil {
		t.Fatal(err)
	}
	if got := e.headers[0].Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want none", got)
	}
}

func TestTheEnvironmentIsNeverRead(t *testing.T) {
	// NewClient would layer these under the options; New must not.
	t.Setenv("OPENAI_API_KEY", "sk-from-the-shell")
	t.Setenv("OPENAI_BASE_URL", "http://127.0.0.1:1/elsewhere")
	e := newEndpoint(t, canned{http.StatusOK, fixture(t, "exchange_2_final.json")})
	if _, err := e.provider(t, "").Complete(context.Background(),
		domain.ModelRequest{Messages: []domain.Message{domain.UserMessage("x")}}); err != nil {
		t.Fatalf("the environment redirected the request: %v", err)
	}
	if got := e.headers[0].Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q — the shell's key was sent", got)
	}
}

func TestNewRefusesAnIncompleteConfig(t *testing.T) {
	client := http.DefaultClient
	for name, cfg := range map[string]Config{
		"no client":      {BaseURL: "https://x.test/v1", Model: "m"},
		"bad url":        {BaseURL: "x.test", Model: "m", HTTPClient: client},
		"key in url":     {BaseURL: "https://u:sk@x.test/v1", Model: "m", HTTPClient: client},
		"no model":       {BaseURL: "https://x.test/v1", HTTPClient: client},
		"negative retry": {BaseURL: "https://x.test/v1", Model: "m", HTTPClient: client, MaxRetries: -1},
		"key over http":  {BaseURL: "http://x.test/v1", Model: "m", APIKey: "sk", HTTPClient: client},
	} {
		if _, err := New(cfg); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	p, err := New(Config{BaseURL: "https://GW.test/v1/", Model: " m ", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	if p.Identity() != (domain.ModelIdentity{Endpoint: "https://gw.test/v1", Model: "m"}) {
		t.Fatalf("identity = %+v", p.Identity())
	}
}

func TestConfigNeverPrintsItsKey(t *testing.T) {
	c := Config{BaseURL: "https://x.test", Model: "m", APIKey: testKey}
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c), c.LogValue().String()} {
		if strings.Contains(s, testKey) {
			t.Fatalf("key leaked: %s", s)
		}
	}
}

func TestDialerPinsTheStoredIdentity(t *testing.T) {
	e := newEndpoint(t, canned{http.StatusOK, fixture(t, "exchange_2_final.json")})
	cfg := domain.ProviderConfig{BaseURL: e.srv.URL + "/v1", Model: "gateway-model"}
	p, err := Dialer{HTTPClient: e.srv.Client()}.Dial(cfg, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if p.Identity() != cfg.Identity() {
		t.Fatalf("identity = %+v, want %+v", p.Identity(), cfg.Identity())
	}
	if _, err := p.Complete(context.Background(),
		domain.ModelRequest{Messages: []domain.Message{domain.UserMessage("x")}}); err != nil {
		t.Fatal(err)
	}
}
