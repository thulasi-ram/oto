package domain_test

// THE REMEDY'S OWN RULES (ADR 0054, git-bug 4148256): what a proposal may hold, that the
// arguments kept are the arguments written — only the whitespace goes — and that the hash is
// the hash of those bytes; when a Remedy reads expired; and when its Tool can carry it out.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

var writeTool = domain.RemedyTool{ToolServerID: uuid.New(), ToolServerName: "k8s-write", Tool: "rollout_restart"}

func TestADraftKeepsTheArgumentsExactlyAsWrittenButTheWhitespace(t *testing.T) {
	t.Parallel()
	written := "{ \"z\": 1,\n  \"a\": 12345678901234567890, \"s\": \"two  spaces\" }"
	d, err := domain.NewRemedyDraft(writeTool, json.RawMessage(written), " Deployment checkout/api ", "restart it")
	if err != nil {
		t.Fatal(err)
	}
	// Key order and the 20-digit literal survive; a re-encoder would sort and round.
	if want := `{"z":1,"a":12345678901234567890,"s":"two  spaces"}`; d.Arguments != want {
		t.Fatalf("arguments = %s, want %s", d.Arguments, want)
	}
	sum := sha256.Sum256([]byte(d.Arguments))
	if d.ArgumentsSHA256() != hex.EncodeToString(sum[:]) || d.ArgumentsSHA256() != domain.HashArguments(d.Arguments) {
		t.Fatalf("hash = %s", d.ArgumentsSHA256())
	}
	if d.Target != "Deployment checkout/api" {
		t.Fatalf("target = %q", d.Target)
	}
}

func TestADraftThatCouldNotBeExecutedAsWrittenIsRefused(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		tool domain.RemedyTool
		args string
		tgt  string
	}{
		"arguments not an object":   {writeTool, `["kubectl","delete"]`, "x"},
		"arguments not JSON":        {writeTool, `{"a":`, "x"},
		"arguments past 16 KiB":     {writeTool, `{"a":"` + strings.Repeat("x", domain.MaxRemedyArgumentsBytes) + `"}`, "x"},
		"arguments with no Tool":    {domain.RemedyTool{}, `{"a":1}`, "x"},
		"no target":                 {writeTool, `{}`, "  "},
		"a target past 500":         {writeTool, `{}`, strings.Repeat("t", domain.MaxRemedyTarget+1)},
		"no Tool and no target":     {domain.RemedyTool{}, ``, ""},
		"a Tool with no arguments ": {writeTool, ``, "x"},
		// ⛔ judgment 2, C7: what jsonb or a TEXT column refuses would roll back the whole Finding.
		"a NUL in the target":            {writeTool, `{}`, "x\x00"},
		"a target that is not UTF-8":     {writeTool, `{}`, "x\xff"},
		"a NUL escape":                   {writeTool, `{"a":"\u0000"}`, "x"},
		"an upper-case surrogate escape": {writeTool, `{"a":"\uD800"}`, "x"},
		"a lone surrogate":               {writeTool, `{"a":"\ud800"}`, "x"},
		"an escaped surrogate pair":      {writeTool, `{"a":"\ud83d\ude00"}`, "x"},
		"a surrogate escape in a key":    {writeTool, `{"\uDC00":"x"}`, "x"},
	} {
		_, err := domain.NewRemedyDraft(c.tool, json.RawMessage(c.args), c.tgt, "why")
		if !errs.IsKind(err, errs.KindValidation) {
			t.Errorf("%s: err = %v, want a validation refusal", name, err)
		}
	}
	if _, err := domain.NewRemedyDraft(writeTool, json.RawMessage(`{}`), "x", " "); !errs.IsKind(err, errs.KindValidation) {
		t.Errorf("a draft with no description was accepted: %v", err)
	}
	// The rune itself, as UTF-8, and an escaped backslash before a `u`, are ordinary arguments.
	for _, args := range []string{`{"a":"😀"}`, `{"a":"\\u0000"}`, `{"a":"\u00e9"}`} {
		d, err := domain.NewRemedyDraft(writeTool, json.RawMessage(args), "x", "why")
		if err != nil || d.Arguments != args {
			t.Errorf("%s: %+v, %v", args, d, err)
		}
	}
	none, err := domain.NewRemedyDraft(domain.RemedyTool{}, nil, "node pool eu-1", "add a node")
	if err != nil || none.Arguments != "" || none.ArgumentsSHA256() != "" || none.Tool.Named() {
		t.Fatalf("a no-Tool draft = %+v, %v", none, err)
	}
}

func TestARemedyReadsExpiredAtItsDeadlineAndOnlyAWaitingOneCan(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	r := domain.Remedy{Tool: writeTool, State: domain.RemedyProposed, ProposedAt: at, ExpiresAt: at.Add(time.Hour)}
	if r.StateAt(at.Add(59*time.Minute)) != domain.RemedyProposed || r.StateAt(at.Add(time.Hour)) != domain.RemedyExpired {
		t.Fatal("a proposed Remedy does not expire at its deadline")
	}
	r.State = domain.RemedyApproved
	if r.StateAt(at.Add(2*time.Hour)) != domain.RemedyExpired {
		t.Fatal("an approved Remedy does not expire at its deadline")
	}
	for _, st := range []domain.RemedyState{domain.RemedyExecuting, domain.RemedyExecuted, domain.RemedyFailed,
		domain.RemedyDeclined} {
		r.State = st
		if r.StateAt(at.Add(48*time.Hour)) != st {
			t.Fatalf("%s expired on the clock", st)
		}
	}
	if err := (domain.Remedy{State: domain.RemedyProposed, ExpiresAt: at.Add(time.Hour)}).Approvable(at); err == nil ||
		!strings.Contains(err.Error(), "remedy_has_no_tool") {
		t.Fatalf("a Remedy with no Tool is approvable: %v", err)
	}
}

func TestARemedysToolCarriesItOutOnlyWhileAWriteToolServerListsIt(t *testing.T) {
	t.Parallel()
	cfg := domain.ToolServerConfig{ID: writeTool.ToolServerID, Name: "k8s-write", Access: domain.AccessWrite}
	listed := []domain.DiscoveredTool{{Name: "rollout_restart"}}
	if why := domain.RemedyBinding(writeTool, cfg, true, listed); why != "" {
		t.Fatalf("a listed write Tool cannot carry it: %s", why)
	}
	if why := domain.RemedyBinding(writeTool, cfg, false, nil); !strings.Contains(why, "no longer configured") {
		t.Fatalf("a removed ToolServer: %q", why)
	}
	read := cfg
	read.Access = domain.AccessRead
	if why := domain.RemedyBinding(writeTool, read, true, listed); !strings.Contains(why, "no longer declared write") {
		t.Fatalf("a ToolServer re-declared read: %q", why)
	}
	if why := domain.RemedyBinding(writeTool, cfg, true, nil); !strings.Contains(why, "no longer lists") {
		t.Fatalf("a Tool no longer listed: %q", why)
	}
	if why := domain.RemedyBinding(domain.RemedyTool{}, cfg, true, listed); why != domain.NoToolCanCarryItOut {
		t.Fatalf("no Tool: %q", why)
	}
}
