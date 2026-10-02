// Package modelcontract is the one test every domain.ModelProvider passes: the scripted
// fake, and the OpenAI-compatible adapter behind an httptest endpoint (git-bug 8f1f071:
// "the real adapter passes the same contract tests the fake does").
//
// ⭐ WHY A SHARED CONTRACT AND NOT TWO SUITES. The loop's tests run against the fake,
// and production runs against the adapter. Every promise the loop relies on — usage is
// present or the turn fails, an invalid request never leaves the process, a cancelled
// context is an error and never a turn, Tool calls arrive whole and in order — is
// asserted here once, against both. A promise only one of them keeps is a test that
// passes against behaviour production does not have.
//
// It imports `testing`, lives under `test/` with the harness, and is only ever imported
// from _test files.
package modelcontract

import (
	"context"
	"errors"
	"testing"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/modelfake"
)

// Harness builds a provider that answers with `steps` in order. A step is the fake's
// own script type: the fake plays it directly, and a recorded-endpoint harness renders
// it as the wire body an endpoint would have sent. `reached` reports how many requests
// actually got to the endpoint — the fake's recorded requests, the httptest server's
// hit count.
//
// Only a step's Text, ToolCalls, Usage and NoUsage are scripted here; Err is the fake's
// alone, because a failure's shape is the transport's and has no common wire form.
type Harness func(t *testing.T, steps []modelfake.Step) (p domain.ModelProvider, reached func() int)

// Run asserts the contract.
func Run(t *testing.T, h Harness) {
	t.Helper()
	ctx := context.Background()
	tool := mustTool(t, "logs_query")
	start := domain.ModelRequest{
		Messages: []domain.Message{
			domain.SystemMessage("You investigate alerts. Read before you conclude."),
			domain.UserMessage("Case 7: KubePodCrashLooping in payments."),
		},
		Tools: []domain.ToolSchema{tool},
	}

	t.Run("identity is stable and names an endpoint and a model", func(t *testing.T) {
		p, _ := h(t, nil)
		id := p.Identity()
		if id.Endpoint == "" || id.Model == "" {
			t.Fatalf("identity = %+v", id)
		}
		if p.Identity() != id {
			t.Fatal("identity changed between two reads")
		}
	})

	t.Run("a text turn carries its text and its usage", func(t *testing.T) {
		p, reached := h(t, []modelfake.Step{modelfake.Text("The pod is out of memory.", 120, 9)})
		turn, err := p.Complete(ctx, start)
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if turn.Text != "The pod is out of memory." || len(turn.ToolCalls) != 0 {
			t.Fatalf("turn = %+v", turn)
		}
		if turn.Usage != (domain.Usage{InputTokens: 120, OutputTokens: 9}) {
			t.Fatalf("usage = %+v", turn.Usage)
		}
		if turn.Finish != domain.FinishStop {
			t.Fatalf("finish = %q", turn.Finish)
		}
		if reached() != 1 {
			t.Fatalf("reached = %d, want 1", reached())
		}
	})

	t.Run("a tool-calling turn carries every call in order with raw arguments", func(t *testing.T) {
		calls := []domain.ToolCall{
			{ID: "call_a", Name: "logs_query", Arguments: `{"query":"namespace:payments error","limit":50}`},
			{ID: "call_b", Name: "logs_query", Arguments: `{"query":"oom"}`},
		}
		p, _ := h(t, []modelfake.Step{modelfake.Calls(130, 40, calls...)})
		turn, err := p.Complete(ctx, start)
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if len(turn.ToolCalls) != 2 {
			t.Fatalf("calls = %+v", turn.ToolCalls)
		}
		for i, want := range calls {
			if turn.ToolCalls[i] != want {
				t.Fatalf("call %d = %+v, want %+v", i, turn.ToolCalls[i], want)
			}
		}
		if turn.Usage.Total() != 170 || turn.Finish != domain.FinishToolCalls {
			t.Fatalf("usage = %+v finish = %q", turn.Usage, turn.Finish)
		}
	})

	t.Run("a conversation replaying a call and its result is answered", func(t *testing.T) {
		call := domain.ToolCall{ID: "call_1", Name: "logs_query", Arguments: `{"query":"oom"}`}
		p, reached := h(t, []modelfake.Step{
			modelfake.Calls(100, 20, call),
			modelfake.Text("OOMKilled at 02:14; the limit is 256Mi.", 180, 15),
		})
		first, err := p.Complete(ctx, start)
		if err != nil {
			t.Fatalf("first turn: %v", err)
		}
		next := start
		next.Messages = append(append([]domain.Message(nil), start.Messages...),
			domain.AssistantMessage(first),
			domain.ToolResultMessage(call.ID, `{"lines":["OOMKilled"]}`))
		second, err := p.Complete(ctx, next)
		if err != nil {
			t.Fatalf("second turn: %v", err)
		}
		if second.Text == "" || len(second.ToolCalls) != 0 {
			t.Fatalf("second turn = %+v", second)
		}
		if total := first.Usage.Add(second.Usage).Total(); total != 315 {
			t.Fatalf("running total = %d, want 315", total)
		}
		if reached() != 2 {
			t.Fatalf("reached = %d, want 2", reached())
		}
	})

	t.Run("a turn without usage fails as ErrUsageMissing and returns no turn", func(t *testing.T) {
		p, _ := h(t, []modelfake.Step{modelfake.WithoutUsage("free, apparently")})
		turn, err := p.Complete(ctx, start)
		if !domain.IsUsageMissing(err) {
			t.Fatalf("err = %v, want ErrUsageMissing", err)
		}
		if turn.Text != "" || turn.ToolCalls != nil || turn.Usage.Total() != 0 {
			t.Fatalf("a failed turn carried content: %+v", turn)
		}
	})

	t.Run("an invalid request never reaches the endpoint", func(t *testing.T) {
		p, reached := h(t, []modelfake.Step{modelfake.Text("unused", 1, 1)})
		orphan := domain.ModelRequest{Messages: []domain.Message{
			domain.UserMessage("x"), domain.ToolResultMessage("call_nobody_made", "?"),
		}}
		if _, err := p.Complete(ctx, orphan); !errs.IsKind(err, errs.KindValidation) {
			t.Fatalf("err = %v, want a validation error", err)
		}
		if _, err := p.Complete(ctx, domain.ModelRequest{}); !errs.IsKind(err, errs.KindValidation) {
			t.Fatalf("empty request: err = %v, want a validation error", err)
		}
		if reached() != 0 {
			t.Fatalf("reached = %d, want 0", reached())
		}
	})

	t.Run("a cancelled context is an error, never a turn", func(t *testing.T) {
		p, reached := h(t, []modelfake.Step{modelfake.Text("unused", 1, 1)})
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		turn, err := p.Complete(cctx, start)
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want one wrapping context.Canceled", err)
		}
		if turn.Text != "" || turn.ToolCalls != nil {
			t.Fatalf("turn = %+v", turn)
		}
		if reached() != 0 {
			t.Fatalf("reached = %d, want 0", reached())
		}
	})
}

func mustTool(t *testing.T, name string) domain.ToolSchema {
	t.Helper()
	s, err := domain.NewToolSchema(name, "Query VictoriaLogs with LogsQL.",
		[]byte(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
