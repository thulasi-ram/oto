package domain

// THE MODEL PORT (ADR 0053 §3, git-bug 8f1f071 comment #1, which governs).
//
// An Investigation reaches a model through exactly one interface, ModelProvider, and
// everything it says to the model and hears back is spelled in the types below. The
// first adapter speaks the Chat Completions protocol with tool calling, which reaches
// any provider or gateway serving that API; a provider that does not
// speak it is reached through the operator's gateway, never through a second adapter,
// until a test shows a tool-calling fidelity gap only a native adapter closes.
//
// ⛔ NO AGENT FRAMEWORK OWNS THE LOOP. The port answers ONE turn and stops. oto's own
// loop (git-bug 180a525) decides whether to call a Tool, records every turn, call and
// result as a Step, and enforces every §6 budget — because a framework that owns the
// loop owns exactly the two things ADR 0053 exists to make readable: what happened,
// and what it cost.
//
// ⭐⭐ USAGE IS MANDATORY, AND ITS ABSENCE IS A FAILURE, NOT A ZERO. The per-run token
// budget, the org's daily budget and the cost every Investigation records are all
// summed from Turn.Usage. A turn that came back without usage cannot be budgeted, and
// counting it as zero would let a provider that never reports usage run unbudgeted
// forever. So a response without usage is ErrUsageMissing — a distinct, typed kind the
// loop turns into a `failed` Investigation — and NewTurn will not build a Turn without
// one. If you can construct a Turn, it has been paid for.

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// ModelProvider is the port an Investigation talks to a model through.
//
// Complete sends one request and returns one Turn. It never loops, never calls a
// Tool and never retries a turn the model already answered: a second attempt that
// succeeds would spend tokens the first already spent, and only the loop may decide
// that is worth it. Transport-level retries of a request that got NO answer are the
// adapter's business and cost nothing.
//
// Every implementation must:
//   - refuse an invalid request (ModelRequest.Validate) before anything leaves the
//     process;
//   - return ErrUsageMissing, and no Turn, when the answer carries no token usage;
//   - never put the API key in an error, a log line or a Turn.
//
// `test/modelcontract` is the test every implementation passes.
type ModelProvider interface {
	// Identity is what an Investigator version pins: the endpoint and the model.
	// Changing either is a new version (ADR 0053 §6).
	Identity() ModelIdentity
	// Complete answers one turn.
	Complete(ctx context.Context, req ModelRequest) (Turn, error)
}

// ErrUsageMissing is the kind a provider returns when an answer carried no token
// usage. Match it with errors.Is; the Investigation that receives it ends `failed`,
// never `exhausted` and never as if the turn were free.
//
// It is an upstream failure (the model endpoint did not keep the protocol's promise)
// with a code of its own, so errors.Is distinguishes it from every other upstream
// failure — a timeout or a 5xx may be worth a later run; this one will recur on every
// run until the operator points the Investigator at an endpoint that reports usage.
var ErrUsageMissing error = &errs.Error{Kind: errs.KindUpstreamDown, Code: CodeUsageMissing}

// CodeUsageMissing is ErrUsageMissing's code.
const CodeUsageMissing = "model_usage_missing"

// UsageMissing builds the error a provider returns for an answer without usage.
func UsageMissing(identity ModelIdentity) error {
	return errs.Newf(errs.KindUpstreamDown, CodeUsageMissing,
		"the model endpoint %s answered without token usage; an Investigation cannot be budgeted without it",
		identity)
}

// IsUsageMissing reports whether err is, or wraps, ErrUsageMissing.
func IsUsageMissing(err error) bool { return errors.Is(err, ErrUsageMissing) }

// Role is who said a Message.
type Role string

// The four roles. They are the protocol's, and every endpoint that speaks it spells
// them the same way, so they are also the wire values.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one entry of the conversation sent to a model.
//
// A Message is a plain value. The invariants that relate one Message to another —
// a Tool result answers a call an earlier assistant Message made — are checked over
// the whole request by ModelRequest.Validate, which is the only place they can be.
type Message struct {
	Role Role
	// Content is the text. An assistant Message that only called Tools may have none.
	Content string
	// ToolCalls are the calls an assistant Message made, replayed verbatim on the next
	// request so the model sees what it asked for. Only RoleAssistant carries them.
	ToolCalls []ToolCall
	// ToolCallID names the call a RoleTool Message answers.
	ToolCallID string
}

// SystemMessage is the Investigator's prompt.
func SystemMessage(text string) Message { return Message{Role: RoleSystem, Content: text} }

// UserMessage is the subject handed to the model.
func UserMessage(text string) Message { return Message{Role: RoleUser, Content: text} }

// AssistantMessage replays a Turn the model produced, text and calls alike.
func AssistantMessage(t Turn) Message {
	return Message{Role: RoleAssistant, Content: t.Text, ToolCalls: append([]ToolCall(nil), t.ToolCalls...)}
}

// ToolResultMessage answers one ToolCall. A refused call (outside the allowlist), a
// timeout or a truncated result is still answered — with the refusal or the
// truncation said in Content — because the model must hear why it got nothing.
func ToolResultMessage(callID, content string) Message {
	return Message{Role: RoleTool, Content: content, ToolCallID: callID}
}

// MaxToolNameLength is the protocol's ceiling on a function name, and the pattern
// below is its alphabet. A Tool whose name an endpoint would refuse is refused here,
// where the error can name it, rather than as a 400 from the endpoint mid-run.
const MaxToolNameLength = 64

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ToolSchema is one Tool as the model is told about it: its name, what it is for, and
// a JSON Schema of its arguments.
type ToolSchema struct {
	Name        string
	Description string
	// Parameters is a JSON Schema object. Never nil once constructed: a Tool with no
	// arguments is `{"type":"object","properties":{}}`, which NewToolSchema fills in.
	Parameters json.RawMessage
}

// emptyObjectSchema is the schema of a Tool that takes no arguments.
var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// NewToolSchema builds a ToolSchema, refusing a name the protocol cannot carry and
// parameters that are not one JSON object.
func NewToolSchema(name, description string, parameters json.RawMessage) (ToolSchema, error) {
	if !toolNamePattern.MatchString(name) {
		return ToolSchema{}, errs.Validation("tool_name_invalid",
			"a Tool's name is 1 to 64 letters, digits, underscores or hyphens",
			errs.Violation{Field: "tool/name", Code: "pattern", Message: "invalid Tool name"})
	}
	if len(parameters) == 0 {
		parameters = emptyObjectSchema
	}
	if !isJSONObject(parameters) {
		return ToolSchema{}, errs.Validation("tool_parameters_invalid",
			"a Tool's parameters are one JSON Schema object",
			errs.Violation{Field: "tool/parameters", Code: "type", Message: "not a JSON object"})
	}
	return ToolSchema{
		Name:        name,
		Description: strings.TrimSpace(description),
		Parameters:  append(json.RawMessage(nil), parameters...),
	}, nil
}

// ToolCall is one Tool call a model asked for.
//
// ⚠️ Arguments IS KEPT RAW, ON PURPOSE. A model may emit arguments that are not valid
// JSON; that is a fact about the model, and the loop records it as a Step and answers
// the call with the refusal rather than failing the whole turn. Parsing here would
// lose the bytes the Step must keep.
type ToolCall struct {
	// ID is the model's handle for the call; the Tool result answers it.
	ID string
	// Name is the Tool's name, as offered in ToolSchema.Name — or not: a model can ask
	// for a Tool it was never offered, and the allowlist check is the loop's.
	Name string
	// Arguments is the raw JSON the model wrote.
	Arguments string
}

// Usage is what one turn cost. Input and output are kept apart because the §6 token
// budget sums both and an operator's bill prices them differently.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

// NewUsage builds a Usage. A negative count is a broken endpoint, not a refund.
func NewUsage(input, output int64) (Usage, error) {
	if input < 0 || output < 0 {
		return Usage{}, errs.Newf(errs.KindUpstreamDown, "model_usage_invalid",
			"the model endpoint reported negative token usage (input %d, output %d)", input, output)
	}
	return Usage{InputTokens: input, OutputTokens: output}, nil
}

// Total is what the token budgets count.
func (u Usage) Total() int64 { return u.InputTokens + u.OutputTokens }

// Add sums two usages, for the running total of an Investigation.
func (u Usage) Add(o Usage) Usage {
	return Usage{InputTokens: u.InputTokens + o.InputTokens, OutputTokens: u.OutputTokens + o.OutputTokens}
}

// FinishReason is why the model stopped this turn, as the endpoint said it. It is
// recorded, never decided on: a turn with Tool calls is a turn with Tool calls
// whatever the endpoint called its reason.
type FinishReason string

// The reasons the protocol names. An endpoint may say something else; it is kept.
const (
	FinishStop      FinishReason = "stop"
	FinishToolCalls FinishReason = "tool_calls"
	FinishLength    FinishReason = "length"
	FinishFiltered  FinishReason = "content_filter"
)

// Turn is one answer from a model: text, Tool calls, or both, and what it cost.
//
// Build one with NewTurn. Its zero value is never returned by a provider with a nil
// error — the contract test asserts that — so a Turn in hand has its usage.
type Turn struct {
	Text      string
	ToolCalls []ToolCall
	Usage     Usage
	Finish    FinishReason
}

// NewTurn builds a Turn. `usage` is a pointer because ABSENT is a different answer
// from zero: nil is ErrUsageMissing, never an empty Usage.
//
// A call with no ID or no name is refused, and so are two calls sharing an ID —
// a Tool result could not say which one it answers.
func NewTurn(identity ModelIdentity, text string, calls []ToolCall, usage *Usage, finish FinishReason) (Turn, error) {
	if usage == nil {
		return Turn{}, UsageMissing(identity)
	}
	seen := make(map[string]struct{}, len(calls))
	for i, c := range calls {
		if c.ID == "" || c.Name == "" {
			return Turn{}, errs.Newf(errs.KindUpstreamDown, "model_tool_call_invalid",
				"the model endpoint %s returned Tool call %d without an id or a name", identity, i)
		}
		if _, dup := seen[c.ID]; dup {
			return Turn{}, errs.Newf(errs.KindUpstreamDown, "model_tool_call_invalid",
				"the model endpoint %s returned two Tool calls with the id %q", identity, c.ID)
		}
		seen[c.ID] = struct{}{}
	}
	return Turn{Text: text, ToolCalls: append([]ToolCall(nil), calls...), Usage: *usage, Finish: finish}, nil
}

// ModelRequest is one turn's question: the conversation so far and the Tools the
// model may ask for.
type ModelRequest struct {
	Messages []Message
	Tools    []ToolSchema
	// MaxOutputTokens caps this turn's output. Zero sends no cap. The loop derives it
	// from what is left of the Investigation's token budget, so a turn cannot spend
	// past the budget by more than its own input.
	MaxOutputTokens int64
}

// Validate refuses a request no endpoint could answer coherently. Every provider calls
// it before anything leaves the process, so a malformed conversation is an oto bug
// reported as one, not a 400 from the endpoint after tokens were counted.
func (r ModelRequest) Validate() error {
	if len(r.Messages) == 0 {
		return errs.Validation("model_request_empty", "a model request carries at least one message")
	}
	if r.MaxOutputTokens < 0 {
		return errs.Validation("model_request_max_tokens", "a model request's output cap is zero or positive")
	}

	tools := make(map[string]struct{}, len(r.Tools))
	for _, t := range r.Tools {
		if !toolNamePattern.MatchString(t.Name) || !isJSONObject(t.Parameters) {
			return errs.Newf(errs.KindValidation, "model_request_tool_invalid",
				"Tool %q was not built by NewToolSchema", t.Name)
		}
		if _, dup := tools[t.Name]; dup {
			return errs.Newf(errs.KindValidation, "model_request_tool_duplicate",
				"Tool %q is offered twice", t.Name)
		}
		tools[t.Name] = struct{}{}
	}

	// ⭐ A Tool result must answer a call an EARLIER assistant Message made. The
	// protocol refuses an orphan with a 400, and an orphan here means the loop lost
	// track of which call it was answering — which is a Step recorded against the
	// wrong call.
	asked := map[string]struct{}{}
	for i, m := range r.Messages {
		switch m.Role {
		case RoleSystem, RoleUser:
			if len(m.ToolCalls) > 0 || m.ToolCallID != "" {
				return errs.Newf(errs.KindValidation, "model_request_message_invalid",
					"message %d (%s) carries Tool call fields", i, m.Role)
			}
		case RoleAssistant:
			if m.ToolCallID != "" {
				return errs.Newf(errs.KindValidation, "model_request_message_invalid",
					"message %d (assistant) answers a Tool call", i)
			}
			if m.Content == "" && len(m.ToolCalls) == 0 {
				return errs.Newf(errs.KindValidation, "model_request_message_invalid",
					"message %d (assistant) says nothing and calls nothing", i)
			}
			for _, c := range m.ToolCalls {
				if c.ID == "" || c.Name == "" {
					return errs.Newf(errs.KindValidation, "model_request_message_invalid",
						"message %d replays a Tool call without an id or a name", i)
				}
				asked[c.ID] = struct{}{}
			}
		case RoleTool:
			if _, ok := asked[m.ToolCallID]; !ok {
				return errs.Newf(errs.KindValidation, "model_request_tool_result_orphan",
					"message %d answers Tool call %q, which no earlier message made", i, m.ToolCallID)
			}
		default:
			return errs.Newf(errs.KindValidation, "model_request_message_invalid",
				"message %d has role %q", i, m.Role)
		}
	}
	return nil
}

func isJSONObject(raw json.RawMessage) bool {
	var probe map[string]json.RawMessage
	return len(raw) > 0 && json.Unmarshal(raw, &probe) == nil && probe != nil
}
