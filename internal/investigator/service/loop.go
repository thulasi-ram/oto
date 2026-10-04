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
//
// ⭐⭐ A FINDING IS CLASSIFIED ONLY IN THE OPERATOR'S WORDS (ADR 0053 §5, git-bug 4298aa0).
// When the org has classes the model is told the set in its prompt and offered one more
// Tool, `oto_classify`, whose one argument is an enum of exactly the operator's classes
// and `unclassified`. The loop answers that call itself — it reads nothing — and:
//
//   - a word in the set is recorded as an `ok` Step and is the run's pick (a later
//     valid call replaces it: the last word the model said is its answer);
//   - a word OUTSIDE the set is REFUSED on the record, and the refusal answers the
//     model with the set, so its next turn is the re-ask — the transcript shows the
//     word it tried and that oto did not take it;
//   - a run that ends without a word in the set — silence, only refused words, or a
//     budget first — is `unclassified`, which is always admissible and is the right
//     answer under doubt. ⛔ A value outside the set is never stored.
//
// The classify call is the shape of the answer, not a look at anything, so it does not
// count against the step budget and is not one of the run's Tool calls; it is still a
// Step, because what the model said is the record. With no classes nothing is offered,
// the prompt is the Investigator's own, and the Finding carries no classification.

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
	// classes is the org's Classification set as the run read it when it began. Empty:
	// nothing is offered and the Finding carries no classification.
	classes domain.ClassSet
}

// outcome is what one run came to.
type outcome struct {
	ending    domain.Ending
	finding   string
	spent     domain.Usage
	toolCalls int
	// picked is the last word in the set the model classified with, "" for none;
	// classification is what the Finding is stored with once the run has ended
	// (domain.ClassSet.Settle) — "" when no set was offered.
	picked         string
	classification string
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

	schemas := make([]domain.ToolSchema, 0, len(p.offered)+1)
	byName := make(map[string]Tool, len(p.offered))
	for _, t := range p.offered {
		schemas = append(schemas, t.Schema())
		byName[t.Schema().Name] = t
	}
	prompt := p.prompt
	classifying := !p.classes.Empty()
	if classifying {
		schema, err := p.classes.ClassifySchema()
		if err != nil {
			return outcome{}, err
		}
		schemas = append(schemas, schema)
		prompt += "\n\n" + p.classes.ClassifyPrompt()
	}

	messages := []domain.Message{domain.SystemMessage(prompt), domain.UserMessage(p.subject)}
	var (
		out outcome
		seq int
	)
	next := func() int { seq++; return seq }
	end := func(e domain.Ending) (outcome, error) {
		out.ending, out.classification = e, p.classes.Settle(out.picked)
		return out, nil
	}
	// takeClassification answers one `oto_classify` call and records it. It is not a
	// Tool call against the step budget (the loop comment says why), and never ends
	// the run.
	takeClassification := func(call domain.ToolCall) error {
		o, result := answerClassify(p.classes, call, &out)
		if err := record(ctx, domain.NewToolStep(next(), call, o, result, 0, s.now())); err != nil {
			return err
		}
		messages = append(messages, domain.ToolResultMessage(call.ID, result))
		return nil
	}
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
			if classifying && call.Name == domain.ClassifyTool {
				if err := takeClassification(call); err != nil {
					return out, err
				}
				continue
			}
			if out.toolCalls >= p.budgets.MaxSteps {
				// ⭐ WHAT THE MODEL ASKED FOR NEXT IS STILL RECORDED — as refused, with
				// the reason — so the transcript ends where the model was, not where
				// oto stopped listening. A classification among them is still taken:
				// it costs no step, and the partial Finding is classified by it.
				for _, rest := range turn.ToolCalls[i:] {
					if classifying && rest.Name == domain.ClassifyTool {
						if err := takeClassification(rest); err != nil {
							return out, err
						}
						continue
					}
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

// answerClassify answers one `oto_classify` call: a word in the set becomes the run's
// pick and is answered `ok`; anything else is refused and answered with the set, so
// the model hears exactly what it may say before its next turn — that answer IS the
// re-ask. It never fails the run.
func answerClassify(set domain.ClassSet, call domain.ToolCall, out *outcome) (domain.ToolOutcome, string) {
	picked := domain.ParseClassifyCall(call.Arguments)
	if set.Admits(picked) {
		out.picked = picked
		return domain.OutcomeOK, fmt.Sprintf("recorded: this Finding is classified %s", picked)
	}
	said := fmt.Sprintf("%q is", picked)
	if picked == "" {
		said = "the arguments name no class, which is"
	}
	return domain.OutcomeRefused, fmt.Sprintf(
		"refused: %s not one of this organisation's classes. Call %s again with exactly one of: %s. "+
			"A Finding that ends without one is recorded %s.",
		said, domain.ClassifyTool, strings.Join(set.Answers(), ", "), domain.Unclassified)
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
