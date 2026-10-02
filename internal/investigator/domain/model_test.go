package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/thulasiram/oto/internal/platform/errs"
)

var testIdentity = ModelIdentity{Endpoint: "https://models.example.test/v1", Model: "m-1"}

func TestNewTurnRefusesAnAnswerWithoutUsage(t *testing.T) {
	turn, err := NewTurn(testIdentity, "done", nil, nil, FinishStop)
	if !IsUsageMissing(err) {
		t.Fatalf("NewTurn without usage = %v, want ErrUsageMissing", err)
	}
	if !errors.Is(err, ErrUsageMissing) {
		t.Fatalf("errors.Is(err, ErrUsageMissing) is false for %v", err)
	}
	if turn.Text != "" || turn.ToolCalls != nil {
		t.Fatalf("a refused turn still carried content: %+v", turn)
	}
	// ⭐ It is an upstream failure with its OWN code, so it is distinguishable from
	// every other upstream failure — a timeout must not match it.
	if errs.KindOf(err) != errs.KindUpstreamDown || errs.CodeOf(err) != CodeUsageMissing {
		t.Fatalf("kind/code = %s/%s, want %s/%s", errs.KindOf(err), errs.CodeOf(err),
			errs.KindUpstreamDown, CodeUsageMissing)
	}
	if errors.Is(errs.UpstreamDown("other", "a 502", nil), ErrUsageMissing) {
		t.Fatal("an ordinary upstream failure matched ErrUsageMissing")
	}
}

func TestNewTurnKeepsZeroUsageAsUsage(t *testing.T) {
	// Zero is an answer (a cached reply an endpoint chose not to bill); absent is not.
	u, err := NewUsage(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := NewTurn(testIdentity, "ok", nil, &u, FinishStop)
	if err != nil {
		t.Fatalf("NewTurn with zero usage: %v", err)
	}
	if turn.Usage.Total() != 0 {
		t.Fatalf("usage = %+v", turn.Usage)
	}
}

func TestNewUsageRefusesNegatives(t *testing.T) {
	if _, err := NewUsage(-1, 3); err == nil {
		t.Fatal("negative input tokens accepted")
	}
	if _, err := NewUsage(3, -1); err == nil {
		t.Fatal("negative output tokens accepted")
	}
	u, _ := NewUsage(10, 5)
	if got := u.Add(Usage{InputTokens: 1, OutputTokens: 2}); got.Total() != 18 {
		t.Fatalf("Add = %+v", got)
	}
}

func TestNewTurnRefusesUnanswerableToolCalls(t *testing.T) {
	u, _ := NewUsage(1, 1)
	cases := map[string][]ToolCall{
		"no id":        {{Name: "logs_query", Arguments: "{}"}},
		"no name":      {{ID: "call_1", Arguments: "{}"}},
		"duplicate id": {{ID: "call_1", Name: "a"}, {ID: "call_1", Name: "b"}},
	}
	for name, calls := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewTurn(testIdentity, "", calls, &u, FinishToolCalls); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestNewTurnKeepsMalformedArgumentsRaw(t *testing.T) {
	// The loop records what the model wrote; the domain does not tidy it.
	u, _ := NewUsage(1, 1)
	turn, err := NewTurn(testIdentity, "", []ToolCall{{ID: "c", Name: "t", Arguments: `{"ns": "pay`}}, &u, FinishToolCalls)
	if err != nil {
		t.Fatal(err)
	}
	if turn.ToolCalls[0].Arguments != `{"ns": "pay` {
		t.Fatalf("arguments = %q", turn.ToolCalls[0].Arguments)
	}
}

func TestNewToolSchema(t *testing.T) {
	s, err := NewToolSchema("logs_query", "  Query VictoriaLogs.  ", nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Description != "Query VictoriaLogs." || !isJSONObject(s.Parameters) {
		t.Fatalf("schema = %+v", s)
	}
	for _, bad := range []string{"", "has space", "dot.ted", strings.Repeat("a", MaxToolNameLength+1)} {
		if _, err := NewToolSchema(bad, "", nil); err == nil {
			t.Fatalf("name %q accepted", bad)
		}
	}
	for _, bad := range []string{`[]`, `"x"`, `null`, `{`} {
		if _, err := NewToolSchema("ok", "", json.RawMessage(bad)); err == nil {
			t.Fatalf("parameters %s accepted", bad)
		}
	}
}

func TestModelRequestValidate(t *testing.T) {
	tool, _ := NewToolSchema("k8s_get", "", nil)
	call := ToolCall{ID: "call_1", Name: "k8s_get", Arguments: "{}"}
	valid := ModelRequest{
		Messages: []Message{
			SystemMessage("you investigate"),
			UserMessage("case 7"),
			{Role: RoleAssistant, ToolCalls: []ToolCall{call}},
			ToolResultMessage("call_1", "pod is CrashLoopBackOff"),
		},
		Tools: []ToolSchema{tool},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}

	cases := map[string]ModelRequest{
		"no messages":    {},
		"negative cap":   {Messages: []Message{UserMessage("x")}, MaxOutputTokens: -1},
		"orphan result":  {Messages: []Message{UserMessage("x"), ToolResultMessage("call_9", "?")}},
		"result first":   {Messages: []Message{ToolResultMessage("call_1", "?"), {Role: RoleAssistant, ToolCalls: []ToolCall{call}}}},
		"empty reply":    {Messages: []Message{UserMessage("x"), {Role: RoleAssistant}}},
		"unknown role":   {Messages: []Message{{Role: "developer", Content: "x"}}},
		"user with call": {Messages: []Message{{Role: RoleUser, Content: "x", ToolCallID: "c"}}},
		"duplicate tool": {Messages: []Message{UserMessage("x")}, Tools: []ToolSchema{tool, tool}},
		"unbuilt tool":   {Messages: []Message{UserMessage("x")}, Tools: []ToolSchema{{Name: "t"}}},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if err := req.Validate(); !errs.IsKind(err, errs.KindValidation) {
				t.Fatalf("Validate = %v, want a validation error", err)
			}
		})
	}
}

func TestAssistantMessageReplaysTheTurn(t *testing.T) {
	u, _ := NewUsage(1, 1)
	turn, _ := NewTurn(testIdentity, "looking", []ToolCall{{ID: "c1", Name: "t", Arguments: "{}"}}, &u, FinishToolCalls)
	m := AssistantMessage(turn)
	if m.Role != RoleAssistant || m.Content != "looking" || len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "c1" {
		t.Fatalf("message = %+v", m)
	}
	// A copy, not an alias: the loop may append to the Turn's calls without
	// rewriting history it already sent.
	turn.ToolCalls[0].ID = "mutated"
	if m.ToolCalls[0].ID != "c1" {
		t.Fatal("AssistantMessage aliased the Turn's calls")
	}
}

func TestUsageMissingNamesTheIdentityNotASecret(t *testing.T) {
	msg := fmt.Sprint(UsageMissing(testIdentity))
	if !strings.Contains(msg, testIdentity.String()) {
		t.Fatalf("message %q does not name the endpoint", msg)
	}
}
