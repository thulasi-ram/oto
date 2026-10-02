package modelfake_test

import (
	"context"
	"errors"
	"testing"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/modelcontract"
	"github.com/thulasiram/oto/test/modelfake"
)

func TestFakeKeepsTheContract(t *testing.T) {
	modelcontract.Run(t, func(t *testing.T, steps []modelfake.Step) (domain.ModelProvider, func() int) {
		p := modelfake.New(steps...)
		return p, func() int { return len(p.Requests()) }
	})
}

func TestFakeRecordsRequestsAsSentNotAsLaterMutated(t *testing.T) {
	p := modelfake.New(modelfake.Text("a", 1, 1), modelfake.Text("b", 1, 1))
	req := domain.ModelRequest{Messages: []domain.Message{domain.UserMessage("first")}}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Messages[0].Content = "rewritten after sending"
	got := p.Requests()
	if len(got) != 1 || got[0].Messages[0].Content != "first" {
		t.Fatalf("recorded = %+v", got)
	}
	if p.Remaining() != 1 {
		t.Fatalf("remaining = %d", p.Remaining())
	}
}

func TestFakeRefusesToRunPastItsScript(t *testing.T) {
	p := modelfake.New(modelfake.Text("only", 1, 1))
	req := domain.ModelRequest{Messages: []domain.Message{domain.UserMessage("x")}}
	_, _ = p.Complete(context.Background(), req)
	_, err := p.Complete(context.Background(), req)
	if errs.CodeOf(err) != modelfake.CodeScriptExhausted || errs.KindOf(err) != errs.KindInternal {
		t.Fatalf("err = %v, want %s", err, modelfake.CodeScriptExhausted)
	}
}

func TestFakeReturnsScriptedFailures(t *testing.T) {
	boom := errs.UpstreamDown("model_unavailable", "502 from the gateway", nil)
	p := modelfake.New(modelfake.Fail(boom), modelfake.Calls(5, 2, domain.ToolCall{ID: "c", Name: "t", Arguments: "{}"}))
	req := domain.ModelRequest{Messages: []domain.Message{domain.UserMessage("x")}}
	if _, err := p.Complete(context.Background(), req); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the scripted failure", err)
	}
	turn, err := p.Complete(context.Background(), req)
	if err != nil || turn.Finish != domain.FinishToolCalls || len(turn.ToolCalls) != 1 {
		t.Fatalf("turn = %+v, err = %v", turn, err)
	}
}

func TestFakeReportsTheIdentityItWasGiven(t *testing.T) {
	id := domain.ModelIdentity{Endpoint: "https://gw.test/v1", Model: "m-2"}
	if got := modelfake.NewWithIdentity(id).Identity(); got != id {
		t.Fatalf("identity = %+v", got)
	}
	if got := modelfake.WithoutUsage("x"); !got.NoUsage {
		t.Fatal("WithoutUsage did not script a missing usage")
	}
}
