package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

func entry(key, configKey string, value any) domain.DeclaredEntry {
	return domain.DeclaredEntry{Key: key, ConfigKey: configKey, Value: value}
}

func mustDeclarative(t *testing.T, entries ...domain.DeclaredEntry) domain.Declarative {
	t.Helper()
	d, err := domain.NewDeclarative(entries)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return d
}

// TestTheZeroDeclarativeChangesNothing. Every install that has not opted in runs
// through this path on every settings read, so it has to be inert AND safe.
func TestTheZeroDeclarativeChangesNothing(t *testing.T) {
	t.Parallel()

	var zero domain.Declarative
	if !zero.Empty() {
		t.Fatal("the zero value claims to manage keys")
	}
	if zero.Manages(domain.KeyRefireGrace) || zero.ConfigKey(domain.KeyRefireGrace) != "" {
		t.Fatal("the zero value manages a key")
	}

	org := domain.Org{Settings: domain.DefaultSettings()}.WithDeclarative(zero)
	if org.Settings.RefireGrace != domain.DefaultRefireGrace {
		t.Fatalf("refire_grace = %v, want the shipped default", org.Settings.RefireGrace)
	}
	if got := org.Origin(domain.KeyRefireGrace); got != domain.OriginDefault {
		t.Fatalf("origin %q, want default", got)
	}
}

// TestPrecedenceIsDefaultThenOrgThenConfig walks all three sources on one key.
func TestPrecedenceIsDefaultThenOrgThenConfig(t *testing.T) {
	t.Parallel()

	// 1. Nothing set: the shipped default, origin `default`.
	plain := domain.Org{}.WithDeclarative(domain.Declarative{})
	if plain.Settings.RefireGrace != domain.DefaultRefireGrace {
		t.Fatalf("default layer: %v", plain.Settings.RefireGrace)
	}
	if got := plain.Origin(domain.KeyRefireGrace); got != domain.OriginDefault {
		t.Fatalf("default layer origin %q", got)
	}

	// 2. The org writes 900: 900 wins over the default, origin `org`.
	nine := 900
	withOrg := domain.Org{Overrides: domain.SettingsPatch{RefireGraceS: &nine}}.
		WithDeclarative(domain.Declarative{})
	if withOrg.Settings.RefireGrace != 900*time.Second {
		t.Fatalf("org layer: %v", withOrg.Settings.RefireGrace)
	}
	if got := withOrg.Origin(domain.KeyRefireGrace); got != domain.OriginOrg {
		t.Fatalf("org layer origin %q", got)
	}

	// 3. Configuration says 600: 600 wins over BOTH, origin `config`.
	withCfg := withOrg.WithDeclarative(mustDeclarative(t,
		entry("refire_grace_s", "OTO_TUNING_REFIRE_GRACE_S", "600")))
	if withCfg.Settings.RefireGrace != 600*time.Second {
		t.Fatalf("config layer: %v, want 600s — declarative must beat the org override",
			withCfg.Settings.RefireGrace)
	}
	if got := withCfg.Origin(domain.KeyRefireGrace); got != domain.OriginConfig {
		t.Fatalf("config layer origin %q, want config", got)
	}
	if got := withCfg.ConfigKey(domain.KeyRefireGrace); got != "OTO_TUNING_REFIRE_GRACE_S" {
		t.Fatalf("config key %q", got)
	}
	if s := withCfg.Shadowed(); s.RefireGraceS == nil || *s.RefireGraceS != 900 {
		t.Fatalf("the shadowed org value is not visible: %+v", s.RefireGraceS)
	}
}

// TestADeclarativeValueIsBoundedLikeAnyOther. Configuration is authoritative
// about WHICH value is in force; it is not privileged about which values are
// legal. `refire_grace_s: 0` is a Slack thread per transition from any source.
func TestADeclarativeValueIsBoundedLikeAnyOther(t *testing.T) {
	t.Parallel()

	_, err := domain.NewDeclarative([]domain.DeclaredEntry{
		entry("refire_grace_s", "OTO_TUNING_REFIRE_GRACE_S", 0),
	})
	if err == nil {
		t.Fatal("a refire_grace of 0 was accepted from configuration")
	}
	vs := errs.ViolationsOf(err)
	if len(vs) != 1 || vs[0].Field != "OTO_TUNING_REFIRE_GRACE_S" {
		t.Fatalf("violations %+v: the field must be the key the operator can act on, "+
			"not the settings key they cannot find", vs)
	}
	if !strings.Contains(vs[0].Message, "60") {
		t.Fatalf("the reason for the bound was lost: %q", vs[0].Message)
	}
}

// TestAnUnknownKeyFailsTheBoot. A values file line that does nothing and says
// nothing is the same class of failure as the silent revert this layer removes.
func TestAnUnknownKeyFailsTheBoot(t *testing.T) {
	t.Parallel()

	_, err := domain.NewDeclarative([]domain.DeclaredEntry{
		entry("refire_grace", "OTO_TUNING_REFIRE_GRACE", "600"),
	})
	if err == nil {
		t.Fatal("a misspelled tuning key booted silently")
	}
	vs := errs.ViolationsOf(err)
	if len(vs) != 1 || vs[0].Code != "unknown_key" {
		t.Fatalf("violations %+v", vs)
	}
	if !strings.Contains(vs[0].Message, "refire_grace_s") {
		t.Fatalf("the error does not say what should have been typed: %q", vs[0].Message)
	}
}

// TestValuesArriveAsStringsOrAsTypes. An env var is a string and YAML is typed,
// so the same key reaches this code as "600" or as 600 depending only on which
// provider set it. Both must land on the same number.
func TestValuesArriveAsStringsOrAsTypes(t *testing.T) {
	t.Parallel()

	fromEnv := mustDeclarative(t,
		entry("refire_grace_s", "OTO_TUNING_REFIRE_GRACE_S", "900"),
		entry("broadcast_on_resolved", "OTO_TUNING_BROADCAST_ON_RESOLVED", "true"),
		entry("unacked_reminder_mention_list", "OTO_TUNING_UNACKED_REMINDER_MENTION_LIST",
			[]string{"<@U012AB3CD>", "<!subteam^S01ABCDEF>"}),
	)
	fromFile := mustDeclarative(t,
		entry("refire_grace_s", "tuning.refire_grace_s", 900),
		entry("broadcast_on_resolved", "tuning.broadcast_on_resolved", true),
		entry("unacked_reminder_mention_list", "tuning.unacked_reminder_mention_list",
			[]any{"<@U012AB3CD>", "<!subteam^S01ABCDEF>"}),
	)

	env := domain.Org{}.WithDeclarative(fromEnv).Settings
	file := domain.Org{}.WithDeclarative(fromFile).Settings
	if env.RefireGrace != file.RefireGrace || env.RefireGrace != 900*time.Second {
		t.Fatalf("env %v, file %v", env.RefireGrace, file.RefireGrace)
	}
	if !env.BroadcastOnResolved || !file.BroadcastOnResolved {
		t.Fatalf("env %v, file %v", env.BroadcastOnResolved, file.BroadcastOnResolved)
	}
	if len(env.UnackedReminderMentionList) != 2 || len(file.UnackedReminderMentionList) != 2 {
		t.Fatalf("env %v, file %v", env.UnackedReminderMentionList, file.UnackedReminderMentionList)
	}
}

// TestAValueOfTheWrongTypeFailsByName.
func TestAValueOfTheWrongTypeFailsByName(t *testing.T) {
	t.Parallel()

	_, err := domain.NewDeclarative([]domain.DeclaredEntry{
		entry("refire_grace_s", "tuning.refire_grace_s", "ten minutes"),
	})
	if err == nil {
		t.Fatal("an unparseable duration was accepted")
	}
	vs := errs.ViolationsOf(err)
	if len(vs) != 1 || vs[0].Field != "tuning.refire_grace_s" {
		t.Fatalf("violations %+v", vs)
	}
}

// TestThePatchIsCopiedOut. The resolved layer is process-wide and read on every
// settings lookup; a caller that could mutate its slice would be editing the
// deployment's stated configuration for every tenant at once.
func TestThePatchIsCopiedOut(t *testing.T) {
	t.Parallel()

	d := mustDeclarative(t, entry("unacked_reminder_mention_list",
		"tuning.unacked_reminder_mention_list", []string{"<@U012AB3CD>"}))

	first := d.Patch()
	(*first.UnackedReminderMentionList)[0] = "<@UDEADBEEF>"

	second := d.Patch()
	if (*second.UnackedReminderMentionList)[0] != "<@U012AB3CD>" {
		t.Fatal("the resolved declarative layer was mutated through a returned patch")
	}
}
