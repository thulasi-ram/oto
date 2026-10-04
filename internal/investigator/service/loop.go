package service

// OTO'S OWN LOOP (ADR 0053 §3, §6; git-bug 180a525). No framework owns it: a framework
// that owns the loop owns exactly the two things ADR 0053 exists to make readable —
// what happened, and what it cost.
//
// One run is: tell the model the prompt and the subject; ask it for one turn; record the
// turn as a Step; run each Tool call it asked for, recording each as a Step and answering
// it; repeat until the model answers without calling anything — or a budget stops it.
//
// ⭐⭐ EVERY STEP IS WRITTEN BEFORE THE NEXT THING HAPPENS. A turn is recorded before any
// Tool it asked for runs, and a Tool call before the model hears its result, so a worker
// that dies mid-run leaves a transcript that is true as far as it goes — and the retry
// ends the run `interrupted` rather than paying for every turn twice.
//
// ⭐ THE THREE PER-RUN BUDGETS END A RUN `exhausted`, AND WHAT IT HAD SAID IS KEPT. The
// partial Finding is the last thing the model said before the budget stopped it — an
// honest "this is as far as it got", marked partial, never dressed up as a conclusion:
//
//   - STEPS: the most Tool calls one run may make. A call asked for past it is recorded
//     as a refused Step, so the transcript shows what the model wanted next, and the run
//     ends.
//   - TOKENS: the most input + output tokens. Each turn is capped at what is left, and a
//     turn that reaches the budget ends the run — even one cut short mid-answer.
//   - WALL TIME: from the moment the run starts, on the injected clock and on a context
//     deadline, so a model call that never returns is cut off at the budget.
//
// ⛔ A TURN WITHOUT USAGE FAILS THE RUN (`usage_missing`). It is never counted as free:
// a provider that never reports usage would otherwise run unbudgeted forever.
//
// ⛔ THE PER-CALL CONTROLS NEVER END A RUN. A call outside the allowlist is refused; one
// past its timeout is a `timeout`; a result past the size cap is `truncated` — each is
// recorded on its Step and answered to the model, and the run continues (ADR 0053 §6).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Limits are the per-Tool-call controls (ADR 0053 §6: "Per-call timeout and result size
// cap — each Tool call — the Step records the timeout or the truncation; the run
// continues").
type Limits struct {
	// ToolTimeout bounds one Tool call.
	ToolTimeout time.Duration
	// MaxToolResult bounds, in bytes, the result one Tool call may hand the model.
	MaxToolResult int
}

// DefaultLimits are the built-in Tools' per-call controls; a ToolServer's Tools run
// under the ToolServer's own (domain.CallLimits, migration 00093). Fifteen seconds is
// long for a read of oto's own tables and short against a run's wall budget; sixteen
// KiB is a long timeline and a fraction of any model's context.
func DefaultLimits() Limits { return Limits{ToolTimeout: 15 * time.Second, MaxToolResult: 16 << 10} }

func (l Limits) orDefault() Limits {
	d := DefaultLimits()
	if l.ToolTimeout <= 0 {
		l.ToolTimeout = d.ToolTimeout
	}
	if l.MaxToolResult <= 0 {
		l.MaxToolResult = d.MaxToolResult
	}
	return l
}

// plan is everything one run needs, resolved before it starts.
type plan struct {
	model   domain.ModelProvider
	prompt  string
	subject string
	// offered are the Tools the model is told about: allowlisted AND served. A call
	// to anything else is refused.
	offered []Tool
	allow   domain.Allowlist
	// unavailable says why an allowlisted Tool the run cannot hold is not offered — on
	// a write ToolServer, on none, or not listed — so a call to it is refused with the
	// reason rather than as an unknown name.
	unavailable map[string]string
	// redact is the org's ingest redaction rules, applied to every result.
	redact  domain.ResultRedactor
	budgets domain.Budgets
	scope   db.TenantScope
	run     RunSubject
}

// outcome is what one run came to.
type outcome struct {
	ending    domain.Ending
	finding   string
	spent     domain.Usage
	toolCalls int
}

// stepSink records one Step. An error from it is oto failing to keep its record,
// which stops the run: a transcript with a hole in it is not one.
type stepSink func(ctx context.Context, step domain.Step) error

// runLoop runs one Investigation from its first turn to its Ending.
//
// It returns an error ONLY when the run could not keep its record (the sink failed) or
// its own context was cancelled from outside (the worker is stopping). Everything the
// model or a Tool does — failing, timing out, running past a budget — is an outcome,
// not an error.
func (s *Service) runLoop(ctx context.Context, p plan, startedAt time.Time, record stepSink) (outcome, error) {
	deadline := startedAt.Add(p.budgets.MaxWall)
	wallCtx, cancel := context.WithTimeout(ctx, max(deadline.Sub(s.now()), 0))
	defer cancel()

	schemas := make([]domain.ToolSchema, 0, len(p.offered))
	byName := make(map[string]Tool, len(p.offered))
	for _, t := range p.offered {
		schemas = append(schemas, t.Schema())
		byName[t.Schema().Name] = t
	}

	messages := []domain.Message{domain.SystemMessage(p.prompt), domain.UserMessage(p.subject)}
	var (
		out outcome
		seq int
	)
	next := func() int { seq++; return seq }
	end := func(e domain.Ending) (outcome, error) { out.ending = e; return out, nil }
	pastWall := func() bool { return wallCtx.Err() != nil || !s.now().Before(deadline) }
	wallSpent := func() (outcome, error) {
		return end(domain.EndedBy(domain.ReasonWallTime,
			fmt.Sprintf("the wall-time budget of %s ran out", p.budgets.MaxWall)))
	}

	for {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if pastWall() {
			return wallSpent()
		}
		left := p.budgets.MaxTokens - out.spent.Total()
		if left <= 0 {
			return end(domain.EndedBy(domain.ReasonTokenBudget,
				fmt.Sprintf("the token budget of %d was spent", p.budgets.MaxTokens)))
		}

		began := s.now()
		turn, err := p.model.Complete(wallCtx, domain.ModelRequest{
			Messages: messages, Tools: schemas, MaxOutputTokens: left,
		})
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return out, ctx.Err()
			case domain.IsUsageMissing(err):
				return end(domain.EndedBy(domain.ReasonUsageMissing, safeMessage(err)))
			case pastWall():
				return wallSpent()
			default:
				return end(domain.EndedBy(domain.ReasonModelError, safeMessage(err)))
			}
		}

		out.spent = out.spent.Add(turn.Usage)
		if err := record(ctx, domain.NewModelTurnStep(next(), turn, s.now().Sub(began), s.now())); err != nil {
			return out, err
		}
		if f := domain.NewFinding(turn.Text); f != "" {
			out.finding = f
		}

		overBudget := out.spent.Total() > p.budgets.MaxTokens
		if len(turn.ToolCalls) == 0 {
			// ⭐ AN ANSWER CUT AT THE OUTPUT CAP IS THE TOKEN BUDGET SPEAKING, not the
			// model finishing: the cap is what was left of the budget.
			if turn.Finish == domain.FinishLength || overBudget {
				return end(domain.EndedBy(domain.ReasonTokenBudget, fmt.Sprintf(
					"the token budget of %d ran out at %d tokens, during the answer", p.budgets.MaxTokens, out.spent.Total())))
			}
			return end(domain.Completed())
		}
		if out.spent.Total() >= p.budgets.MaxTokens {
			return end(domain.EndedBy(domain.ReasonTokenBudget, fmt.Sprintf(
				"the token budget of %d ran out at %d tokens, with %d Tool call(s) unanswered",
				p.budgets.MaxTokens, out.spent.Total(), len(turn.ToolCalls))))
		}

		messages = append(messages, domain.AssistantMessage(turn))
		for i, call := range turn.ToolCalls {
			if out.toolCalls >= p.budgets.MaxSteps {
				// ⭐ WHAT THE MODEL ASKED FOR NEXT IS STILL RECORDED — as refused, with
				// the reason — so the transcript ends where the model was, not where
				// oto stopped listening.
				for _, rest := range turn.ToolCalls[i:] {
					msg := fmt.Sprintf("not run: the step budget of %d Tool calls is spent", p.budgets.MaxSteps)
					if err := record(ctx, domain.NewToolStep(next(), rest, domain.OutcomeRefused, msg, 0, s.now())); err != nil {
						return out, err
					}
				}
				return end(domain.EndedBy(domain.ReasonStepBudget,
					fmt.Sprintf("the step budget of %d Tool calls was spent", p.budgets.MaxSteps)))
			}
			if pastWall() {
				return wallSpent()
			}
			out.toolCalls++
			began := s.now()
			outcome, result := s.callTool(wallCtx, p, byName, call)
			if err := ctx.Err(); err != nil {
				return out, err
			}
			if err := record(ctx, domain.NewToolStep(next(), call, outcome, result, s.now().Sub(began), s.now())); err != nil {
				return out, err
			}
			messages = append(messages, domain.ToolResultMessage(call.ID, result))
		}
	}
}

// callTool runs one call and says what came of it. It never fails the run.
//
// ⭐ EVERY RESULT IS REDACTED BEFORE IT IS CUT, RECORDED OR READ, failures included: a
// ToolServer's error text is as likely to quote a secret as its answer. Redaction runs
// before the size cap so a matched name is seen whole, and the cap then bounds what was
// redacted — what the Step keeps and what the model reads are the same bytes.
func (s *Service) callTool(ctx context.Context, p plan, served map[string]Tool, call domain.ToolCall) (domain.ToolOutcome, string) {
	if !p.allow.Allows(call.Name) {
		return domain.OutcomeRefused, fmt.Sprintf("refused: %q is not on this Investigator's Tool allowlist", call.Name)
	}
	tool, ok := served[call.Name]
	if !ok {
		if why, held := p.unavailable[call.Name]; held {
			return domain.OutcomeRefused, fmt.Sprintf("refused: %s cannot be called: %s", call.Name, why)
		}
		return domain.OutcomeRefused, fmt.Sprintf("refused: no configured Tool is named %q", call.Name)
	}
	args := strings.TrimSpace(call.Arguments)
	if args == "" {
		args = "{}"
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &probe) != nil || probe == nil {
		return domain.OutcomeFailed, "failed: the arguments are not one JSON object"
	}

	limits := s.limits
	if lt, ok := tool.(limitedTool); ok {
		limits = lt.callLimits().orDefault()
	}
	callCtx, cancel := context.WithTimeout(ctx, limits.ToolTimeout)
	defer cancel()
	result, err := tool.Call(callCtx, p.scope, p.run, json.RawMessage(args))
	if callCtx.Err() != nil {
		if ctx.Err() != nil {
			return domain.OutcomeTimeout, fmt.Sprintf("timeout: the run's wall time ran out while %s was answering", call.Name)
		}
		return domain.OutcomeTimeout, fmt.Sprintf("timeout: %s gave no result within %s", call.Name, limits.ToolTimeout)
	}
	outcome := domain.OutcomeOK
	if err != nil {
		outcome, result = domain.OutcomeFailed, "failed: "+safeMessage(err)
	}

	result, redacted := p.redact.Redact(result)
	note := ""
	if len(result) > limits.MaxToolResult {
		cut := truncateUTF8(result, limits.MaxToolResult)
		note = fmt.Sprintf("\n[truncated: %d of %d bytes]", len(cut), len(result))
		result = cut
		if outcome == domain.OutcomeOK {
			outcome = domain.OutcomeTruncated
		}
	}
	if redacted > 0 {
		note += fmt.Sprintf("\n[redacted: %d value(s) matched this org's redaction rules]", redacted)
	}
	return outcome, result + note
}

// safeMessage is an error as a Step or an ending may record it: oto's own code and
// message, which errs promises are safe to show, and never the cause chain — a
// transport error's text can carry the request it failed on.
func safeMessage(err error) string {
	var e *errs.Error
	if errors.As(err, &e) && e.Message != "" {
		if e.Code != "" {
			return e.Code + ": " + e.Message
		}
		return e.Message
	}
	return "an unexpected error"
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
