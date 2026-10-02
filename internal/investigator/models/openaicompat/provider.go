// Package openaicompat is the one model adapter (ADR 0053 §3, git-bug 8f1f071 comment
// #1): the OpenAI-compatible Chat Completions API with tool calling, spoken through the
// official `github.com/openai/openai-go` SDK and configured by exactly three things — a
// base URL, a model name and an API key.
//
// ⭐ THE PACKAGE IS NAMED FOR THE PROTOCOL, NOT A VENDOR. One adapter reaches every
// provider or gateway that serves this API — hosted APIs, Azure, OpenRouter, LiteLLM,
// Bifrost, vLLM, Ollama — and a provider that does not serve it is reached through the
// operator's gateway, not through a second adapter here. A native adapter for one
// vendor is a later widening behind the same port, justified only by a tool-calling
// fidelity gap shown in a test.
//
// ⛔ THE SDK'S TYPES STOP AT THIS PACKAGE'S EDGE. Nothing outside it names
// `openai.ChatCompletion*`; the port speaks `investigator/domain` and nothing else
// (depguard: `model-sdk-is-adapter-only`).
package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Config is one endpoint.
//
// ⛔ APIKey IS PLAINTEXT, freshly unsealed by the caller for the life of one Provider.
// Config redacts it in String, GoString and LogValue, and nothing in this package puts
// it in an error.
type Config struct {
	// BaseURL is the endpoint, normalised by domain.NormalizeBaseURL — the Chat
	// Completions path is appended to it (`<base>/chat/completions`).
	BaseURL string
	// Model is sent as `model` and pinned in the identity.
	Model string
	// APIKey is sent as a bearer token, and only over https (domain.KeyNeedsHTTPS —
	// the SDK refuses a credential over plaintext too). Empty sends no Authorization
	// header at all: a self-hosted endpoint on the cluster network may take none.
	APIKey string
	// HTTPClient is REQUIRED. `internal/app` passes one whose transport dials through
	// `platform/netguard`, because the base URL is operator-supplied and this is an
	// SSRF surface like every other URL oto dials; a default client here would be the
	// one dial in oto that skipped the guard.
	HTTPClient *http.Client
	// MaxRetries is how many times the SDK re-sends a request that got NO answer — a
	// connection failure, a 408/409/429 or a 5xx. Zero retries nothing. A request the
	// model answered is never retried: that would spend its tokens twice.
	MaxRetries int
}

// String renders the config with the key redacted.
func (c Config) String() string {
	key := "none"
	if c.APIKey != "" {
		key = "[redacted]"
	}
	return fmt.Sprintf("openaicompat.Config{base_url=%s model=%s api_key=%s retries=%d}",
		c.BaseURL, c.Model, key, c.MaxRetries)
}

// GoString redacts too.
func (c Config) GoString() string { return c.String() }

// LogValue redacts for slog.
func (c Config) LogValue() slog.Value { return slog.StringValue(c.String()) }

// Provider is the adapter. Safe for concurrent use: the SDK service is.
type Provider struct {
	identity domain.ModelIdentity
	chat     openai.ChatCompletionService
}

var _ domain.ModelProvider = (*Provider)(nil)

// New builds a Provider.
//
// ⚠️ IT BUILDS THE CHAT SERVICE DIRECTLY, NOT `openai.NewClient`. NewClient layers the
// process environment under the options — OPENAI_API_KEY, OPENAI_BASE_URL,
// OPENAI_ORG_ID, OPENAI_CUSTOM_HEADERS — so an operator's shell variable would decide
// which endpoint an org's Investigation reached and which key it presented. Every
// option here is explicit, and the environment is never read.
func New(cfg Config) (*Provider, error) {
	base, err := domain.NormalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" || utf8.RuneCountInString(model) > domain.MaxModelNameLength {
		return nil, errs.Validation("model_name_invalid", "a model name is 1 to 200 characters")
	}
	if err := domain.KeyNeedsHTTPS(base, cfg.APIKey != ""); err != nil {
		return nil, err
	}
	if cfg.HTTPClient == nil {
		return nil, errs.New(errs.KindInternal, "model_http_client_missing",
			"the model adapter needs an HTTP client; internal/app supplies the guarded one")
	}
	if cfg.MaxRetries < 0 {
		return nil, errs.New(errs.KindInternal, "model_retries_invalid", "retries are zero or more")
	}

	opts := []option.RequestOption{
		// The SDK joins paths onto the base with a trailing slash.
		option.WithBaseURL(base + "/"),
		option.WithHTTPClient(cfg.HTTPClient),
		option.WithMaxRetries(cfg.MaxRetries),
	}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	return &Provider{
		identity: domain.ModelIdentity{Endpoint: base, Model: model},
		chat:     openai.NewChatCompletionService(opts...),
	}, nil
}

// Identity implements domain.ModelProvider.
func (p *Provider) Identity() domain.ModelIdentity { return p.identity }

// Complete implements domain.ModelProvider: one Chat Completions request, one Turn.
func (p *Provider) Complete(ctx context.Context, req domain.ModelRequest) (domain.Turn, error) {
	if err := req.Validate(); err != nil {
		return domain.Turn{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.Turn{}, errs.Wrap(err, errs.KindUpstreamSlow, "model_request_cancelled",
			"the model request was cancelled before it was answered")
	}
	params, err := p.params(req)
	if err != nil {
		return domain.Turn{}, err
	}

	completion, err := p.chat.New(ctx, params)
	if err != nil {
		return domain.Turn{}, p.mapErr(ctx, err)
	}
	return p.turn(completion)
}

// params translates a domain request into the SDK's.
func (p *Provider) params(req domain.ModelRequest) (openai.ChatCompletionNewParams, error) {
	out := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(p.identity.Model),
		Messages: make([]openai.ChatCompletionMessageParamUnion, 0, len(req.Messages)),
	}
	for _, m := range req.Messages {
		switch m.Role {
		case domain.RoleSystem:
			out.Messages = append(out.Messages, openai.SystemMessage(m.Content))
		case domain.RoleUser:
			out.Messages = append(out.Messages, openai.UserMessage(m.Content))
		case domain.RoleTool:
			out.Messages = append(out.Messages, openai.ToolMessage(m.Content, m.ToolCallID))
		case domain.RoleAssistant:
			a := openai.ChatCompletionAssistantMessageParam{}
			if m.Content != "" {
				a.Content.OfString = openai.String(m.Content)
			}
			for _, c := range m.ToolCalls {
				a.ToolCalls = append(a.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: c.ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name: c.Name,
							// Replayed exactly as the model wrote it, even when it is not
							// valid JSON: the model must see what it actually said.
							Arguments: c.Arguments,
						},
					},
				})
			}
			out.Messages = append(out.Messages, openai.ChatCompletionMessageParamUnion{OfAssistant: &a})
		}
	}
	for _, t := range req.Tools {
		var schema shared.FunctionParameters
		if err := json.Unmarshal(t.Parameters, &schema); err != nil {
			// Validate already proved it is one object; this is unreachable short of a bug.
			return openai.ChatCompletionNewParams{}, errs.Internal("model_tool_schema_decode", err)
		}
		def := shared.FunctionDefinitionParam{Name: t.Name, Parameters: schema}
		if t.Description != "" {
			def.Description = openai.String(t.Description)
		}
		out.Tools = append(out.Tools, openai.ChatCompletionFunctionTool(def))
	}
	if req.MaxOutputTokens > 0 {
		out.MaxCompletionTokens = openai.Int(req.MaxOutputTokens)
	}
	return out, nil
}

// turn translates the SDK's answer into a domain Turn.
//
// ⭐⭐ USAGE IS READ FOR PRESENCE, NOT FOR VALUE. The SDK decodes an absent `usage`
// object into a zero struct, which is indistinguishable from "this turn was free" by
// value alone; its JSON metadata says whether the field was there. An answer without
// it — or with it but without both counts — is domain.ErrUsageMissing, and the
// Investigation fails rather than run unbudgeted.
func (p *Provider) turn(c *openai.ChatCompletion) (domain.Turn, error) {
	var usage *domain.Usage
	if c.JSON.Usage.Valid() && c.Usage.JSON.PromptTokens.Valid() && c.Usage.JSON.CompletionTokens.Valid() {
		u, err := domain.NewUsage(c.Usage.PromptTokens, c.Usage.CompletionTokens)
		if err != nil {
			return domain.Turn{}, err
		}
		usage = &u
	}
	if usage == nil {
		return domain.Turn{}, domain.UsageMissing(p.identity)
	}
	if len(c.Choices) == 0 {
		return domain.Turn{}, errs.Newf(errs.KindUpstreamDown, "model_no_choice",
			"the model endpoint %s answered with no choice", p.identity)
	}

	// One choice is asked for (`n` is never sent), so the first is the answer.
	choice := c.Choices[0]
	calls := make([]domain.ToolCall, 0, len(choice.Message.ToolCalls))
	for _, tc := range choice.Message.ToolCalls {
		if tc.Type != "" && tc.Type != "function" {
			// oto offers only function Tools, so a `custom` call is an endpoint that
			// answered a question it was not asked.
			return domain.Turn{}, errs.Newf(errs.KindUpstreamDown, "model_tool_call_unsupported",
				"the model endpoint %s returned a %q Tool call; oto offers only function Tools", p.identity, tc.Type)
		}
		calls = append(calls, domain.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return domain.NewTurn(p.identity, choice.Message.Content, calls, usage, domain.FinishReason(choice.FinishReason))
}

// mapErr turns a transport or API failure into an errs.Error.
//
// ⛔ THE SDK'S ERROR IS NOT KEPT AS THE CAUSE. `*openai.Error` carries the outbound
// *http.Request — Authorization header and all — and a log line that rendered it with
// `%+v`, or a later DumpRequest, would print the key. The cause recorded here is
// rebuilt from the status and the endpoint's own error fields, and nothing else.
func (p *Provider) mapErr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		cause := ctxErr
		if cause == nil {
			cause = context.DeadlineExceeded
			if errors.Is(err, context.Canceled) {
				cause = context.Canceled
			}
		}
		return errs.Wrap(cause, errs.KindUpstreamSlow, "model_request_cancelled",
			"the model request was cancelled or ran out of time before it was answered")
	}

	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		cause := fmt.Errorf("model endpoint answered %d: type=%q code=%q message=%q",
			apiErr.StatusCode, apiErr.Type, apiErr.Code, truncate(apiErr.Message, 512))
		switch {
		case apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden:
			return errs.UpstreamDown("model_auth_failed",
				fmt.Sprintf("the model endpoint %s refused the API key (%d)", p.identity, apiErr.StatusCode), cause)
		case apiErr.StatusCode == http.StatusTooManyRequests:
			return errs.UpstreamDown("model_rate_limited",
				fmt.Sprintf("the model endpoint %s is rate limiting oto", p.identity), cause)
		case apiErr.StatusCode >= 400 && apiErr.StatusCode < 500:
			return errs.UpstreamDown("model_request_refused",
				fmt.Sprintf("the model endpoint %s refused the request (%d)", p.identity, apiErr.StatusCode), cause)
		default:
			return errs.UpstreamDown("model_unavailable",
				fmt.Sprintf("the model endpoint %s failed (%d)", p.identity, apiErr.StatusCode), cause)
		}
	}
	// A dial, TLS or decode failure. Its text names the URL, never a header.
	return errs.UpstreamDown("model_unavailable",
		fmt.Sprintf("the model endpoint %s could not be reached", p.identity), err)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Dialer builds a Provider for one stored endpoint. It is the concrete half of
// `investigator/service.ModelDialer`, and carries what every endpoint in a deployment
// shares: the guarded HTTP client and the retry count.
type Dialer struct {
	HTTPClient *http.Client
	MaxRetries int
}

// Dial implements `investigator/service.ModelDialer`. The key is held by the Provider
// it returns and by nothing else.
func (d Dialer) Dial(cfg domain.ProviderConfig, apiKey string) (domain.ModelProvider, error) {
	return New(Config{
		BaseURL: cfg.BaseURL, Model: cfg.Model, APIKey: apiKey,
		HTTPClient: d.HTTPClient, MaxRetries: d.MaxRetries,
	})
}
