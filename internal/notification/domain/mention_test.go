package domain_test

import (
	"testing"

	"github.com/thulasiram/oto/internal/notification/domain"
)

// TestTheDefaultMentionAudienceIsNobody.
//
// ⭐ THE ZERO VALUE MUST BE SILENT. The mention policy is read from org settings,
// and a settings read that fails, or an org that never opened the screen, yields
// the zero value. If the zero value mentioned anybody, every install would start
// pinging people on upgrade.
func TestTheDefaultMentionAudienceIsNobody(t *testing.T) {
	t.Parallel()

	var p domain.MentionPolicy
	for _, sev := range []string{"critical", "page", "warning", "info", "none", "", "banana"} {
		if got := p.Audience(sev); len(got) != 0 {
			t.Errorf("the zero mention policy addressed %v at severity %q", got, sev)
		}
	}
}

// TestTheSeverityGateDefaultsToCriticalOnly is the constraint that keeps a
// channel from muting oto.
//
// `@here` on every unacked warning is how a channel learns to scroll past oto,
// and a channel that scrolls past oto misses the incident this whole mechanism
// exists for.
func TestTheSeverityGateDefaultsToCriticalOnly(t *testing.T) {
	t.Parallel()

	// MinSeverity empty means the shipped floor, which is `critical`.
	p := domain.MentionPolicy{Mode: "here"}

	for _, sev := range []string{"critical", "page"} {
		if got := p.Audience(sev); len(got) != 1 || got[0] != domain.SlackMentionHere {
			t.Errorf("severity %q: got %v, want a mention", sev, got)
		}
	}
	for _, sev := range []string{"warning", "info", "none"} {
		if got := p.Audience(sev); len(got) != 0 {
			t.Errorf("severity %q: got %v, want NO mention at the default floor", sev, got)
		}
	}

	// Lowering the floor works, and lowers it exactly as far as asked.
	warn := domain.MentionPolicy{Mode: "channel", MinSeverity: "warning"}
	if got := warn.Audience("warning"); len(got) != 1 || got[0] != domain.SlackMentionChannel {
		t.Errorf("floor=warning at warning: got %v, want @channel", got)
	}
	if got := warn.Audience("info"); len(got) != 0 {
		t.Errorf("floor=warning at info: got %v, want nothing", got)
	}
}

// ⛔ TestTheSeverityGateFailsClosed. A severity oto cannot rank is OFF the scale,
// not at the bottom of it. A typo'd `severity:` label must not be able to ping
// ten people, and an absent one must not either.
func TestTheSeverityGateFailsClosed(t *testing.T) {
	t.Parallel()

	// Even at the most permissive floor oto offers.
	p := domain.MentionPolicy{
		Mode:        "list",
		List:        []string{"<@U123AB>"},
		MinSeverity: "info",
	}
	for _, sev := range []string{"", "unknown", "sev1", "P1", "CRITICAL"} {
		if got := p.Audience(sev); len(got) != 0 {
			t.Errorf("unrankable severity %q produced %v; the gate must fail closed", sev, got)
		}
	}

	// An unreadable FLOOR is treated as the strictest one, never as "no gate".
	bad := domain.MentionPolicy{Mode: "here", MinSeverity: "whatever"}
	if got := bad.Audience("warning"); len(got) != 0 {
		t.Errorf("an unreadable floor let a warning through: %v", got)
	}
	if got := bad.Audience("critical"); len(got) != 1 {
		t.Errorf("an unreadable floor blocked a critical: %v", got)
	}
}

// TestAnUnknownModeAddressesNobody. The mode crosses a module boundary as a
// string, so the two vocabularies can drift. Drift must not be able to invent a
// channel-wide ping.
func TestAnUnknownModeAddressesNobody(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"everyone", "all", "team", "@here", "HERE"} {
		p := domain.MentionPolicy{Mode: mode}
		if got := p.Audience("critical"); len(got) != 0 {
			t.Errorf("mode %q addressed %v", mode, got)
		}
	}
}

// TestTheExplicitListIsCappedAndCopied. The cap is the last gate before a string
// reaches a message: a reminder that notifies forty people is a page, and oto
// pages nobody (ADR 0013).
func TestTheExplicitListIsCappedAndCopied(t *testing.T) {
	t.Parallel()

	long := make([]string, 0, 25)
	for range 25 {
		long = append(long, "<@U000001>")
	}
	p := domain.MentionPolicy{Mode: "list", List: long}

	got := p.Audience("critical")
	if len(got) != domain.MaxMentions {
		t.Fatalf("a list of %d resolved to %d, want the cap of %d", len(long), len(got), domain.MaxMentions)
	}

	// The result must not alias the policy's own slice: a caller that truncates or
	// appends to it would be editing configuration.
	got[0] = "<@UMUTATED>"
	if p.List[0] == "<@UMUTATED>" {
		t.Fatal("Audience handed back a slice aliasing the configured list")
	}

	// A `list` mode with nothing in it addresses nobody rather than falling back
	// to something louder.
	empty := domain.MentionPolicy{Mode: "list"}
	if got := empty.Audience("critical"); len(got) != 0 {
		t.Fatalf("an empty list resolved to %v", got)
	}
}
