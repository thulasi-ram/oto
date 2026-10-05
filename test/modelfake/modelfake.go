// Package modelfake is a scripted ModelProvider: it answers each Complete with the next
// step of a script and records every request it was asked, so an Investigation's loop
// can be tested end to end with no network and no key (git-bug 8f1f071 comment #1:
// "it is what makes 180a525's criteria testable").
//
// ⭐ IT KEEPS THE SAME PROMISES AS THE REAL ADAPTER, AND THE CONTRACT TEST PROVES IT.
// `test/modelcontract` runs against this package and against
// `investigator/models/openaicompat` behind httptest; a fake that answered where the
// adapter fails — a turn without usage, an invalid request, a cancelled context — would
// let the loop's tests pass against behaviour production never has.
//
// It lives under `test/`, beside the harness's other fakes, because it is test support
// and nothing else: tests in many packages import it, and no production path may.
package modelfake

import (
	"context"
	"sync"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// DefaultIdentity is the identity a Provider reports when the script does not name
// one. It is deliberately not a URL any real endpoint answers on.
var DefaultIdentity = domain.ModelIdentity{Endpoint: "fake://model", Model: "scripted"}

// Step is one scripted answer: a turn, an error, or a turn whose usage is missing.
type Step struct {
	// Text and ToolCalls are the turn's content.
	Text      string
	ToolCalls []domain.ToolCall
	// Usage is what the turn cost. Ignored when NoUsage is set.
	Usage domain.Usage
	// NoUsage makes this step answer the way an endpoint that does not report usage
	// does: domain.ErrUsageMissing and no Turn — built by domain.NewTurn, the same
	// constructor the adapter uses, so the two cannot disagree about it.
	NoUsage bool
	// Err, when set, is returned instead of a turn — a 5xx, a timeout, a refusal.
	Err error
	// Finish, when set, is the finish reason the turn reports — `length` for an answer
	// cut at its output cap, `content_filter` for a filtered one. Unset, it is the one an
	// endpoint reports for the turn's shape.
	Finish domain.FinishReason
}

// Text scripts a turn that only speaks.
func Text(text string, input, output int64) Step {
	return Step{Text: text, Usage: domain.Usage{InputTokens: input, OutputTokens: output}}
}

// Calls scripts a turn that calls Tools.
func Calls(input, output int64, calls ...domain.ToolCall) Step {
	return Step{ToolCalls: calls, Usage: domain.Usage{InputTokens: input, OutputTokens: output}}
}

// WithoutUsage scripts a turn whose answer carries no usage.
func WithoutUsage(text string) Step { return Step{Text: text, NoUsage: true} }

// Fail scripts a failure.
func Fail(err error) Step { return Step{Err: err} }

// CodeScriptExhausted is returned when the loop asks for more turns than were scripted.
// It is an internal error, never a turn: a loop that ran past its script is a loop
// that did not stop when its test said it would.
const CodeScriptExhausted = "fake_model_script_exhausted"

// Provider is the scripted ModelProvider. Safe for concurrent use.
type Provider struct {
	identity domain.ModelIdentity

	mu       sync.Mutex
	script   []Step
	next     int
	requests []domain.ModelRequest
}

var _ domain.ModelProvider = (*Provider)(nil)

// New builds a Provider that answers with the steps in order.
func New(steps ...Step) *Provider { return NewWithIdentity(DefaultIdentity, steps...) }

// NewWithIdentity builds a Provider reporting the given identity — for a test of the
// Investigator version pin.
func NewWithIdentity(identity domain.ModelIdentity, steps ...Step) *Provider {
	return &Provider{identity: identity, script: append([]Step(nil), steps...)}
}

// Identity implements domain.ModelProvider.
func (p *Provider) Identity() domain.ModelIdentity { return p.identity }

// Complete implements domain.ModelProvider.
//
// The order of refusals is the adapter's: an invalid request and a cancelled context
// are refused before the script advances or anything is recorded — the adapter would
// not have sent either — and only a request that "left the process" consumes a step.
func (p *Provider) Complete(ctx context.Context, req domain.ModelRequest) (domain.Turn, error) {
	if err := req.Validate(); err != nil {
		return domain.Turn{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.Turn{}, errs.Wrap(err, errs.KindUpstreamSlow, "model_request_cancelled",
			"the model request was cancelled before it was answered")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, cloneRequest(req))
	if p.next >= len(p.script) {
		return domain.Turn{}, errs.Newf(errs.KindInternal, CodeScriptExhausted,
			"the fake model was asked for turn %d of a %d-turn script", p.next+1, len(p.script))
	}
	step := p.script[p.next]
	p.next++

	if step.Err != nil {
		return domain.Turn{}, step.Err
	}
	// The finish reason an endpoint reports for the turn's shape.
	finish := domain.FinishStop
	if len(step.ToolCalls) > 0 {
		finish = domain.FinishToolCalls
	}
	if step.Finish != "" {
		finish = step.Finish
	}
	var usage *domain.Usage
	if !step.NoUsage {
		u, err := domain.NewUsage(step.Usage.InputTokens, step.Usage.OutputTokens)
		if err != nil {
			return domain.Turn{}, err
		}
		usage = &u
	}
	return domain.NewTurn(p.identity, step.Text, step.ToolCalls, usage, finish)
}

// Requests returns a copy of every request that consumed a step, in order.
func (p *Provider) Requests() []domain.ModelRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]domain.ModelRequest, len(p.requests))
	for i, r := range p.requests {
		out[i] = cloneRequest(r)
	}
	return out
}

// Remaining is how many scripted steps have not been asked for. A test that expects
// the loop to stop asserts it is zero.
func (p *Provider) Remaining() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.script) - p.next
}

// cloneRequest deep-copies a request, so a loop that appends to its conversation after
// sending it cannot rewrite what the fake recorded.
func cloneRequest(r domain.ModelRequest) domain.ModelRequest {
	out := domain.ModelRequest{MaxOutputTokens: r.MaxOutputTokens}
	if r.Messages != nil {
		out.Messages = make([]domain.Message, len(r.Messages))
		for i, m := range r.Messages {
			m.ToolCalls = append([]domain.ToolCall(nil), m.ToolCalls...)
			out.Messages[i] = m
		}
	}
	if r.Tools != nil {
		out.Tools = make([]domain.ToolSchema, len(r.Tools))
		for i, t := range r.Tools {
			t.Parameters = append([]byte(nil), t.Parameters...)
			out.Tools[i] = t
		}
	}
	return out
}
