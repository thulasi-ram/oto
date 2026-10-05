package domain_test

// THE ORG'S OTHER TWO INVESTIGATION CONTROLS (ADR 0053 §6, git-bug bf172fe): the daily
// token budget and the concurrency. Integer keys, so they ride the bounds table like
// every other — shipped inside their own bound, refused outside it, origin reported,
// declarable — and a zero read back is "never written", repaired to the finite default
// rather than to "unlimited".

import (
	"testing"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

func TestTheInvestigationControlsShipFiniteAndReportTheirOrigin(t *testing.T) {
	t.Parallel()

	var none domain.SettingsPatch
	s := none.Settings()
	if s.InvestigationDailyTokens != domain.DefaultInvestigationDailyTokens ||
		s.InvestigationConcurrency != domain.DefaultInvestigationConcurrency {
		t.Fatalf("an org that wrote nothing runs under %d tokens/day and %d at once", s.InvestigationDailyTokens,
			s.InvestigationConcurrency)
	}
	for _, k := range []domain.SettingKey{domain.KeyInvestigationDailyTokens, domain.KeyInvestigationConcurrency} {
		if none.Origin(k) != domain.OriginDefault {
			t.Fatalf("%s: origin %q, want default", k, none.Origin(k))
		}
		if _, ok := domain.Bounds(k); !ok {
			t.Fatalf("%s has no bound, so nothing can validate it", k)
		}
	}

	set := none.Merge(domain.SettingsPatch{InvestigationDailyTokens: intp(50_000), InvestigationConcurrency: intp(1)})
	if err := set.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := set.Settings(); got.InvestigationDailyTokens != 50_000 || got.InvestigationConcurrency != 1 {
		t.Fatalf("the org's write did not take: %+v", got)
	}
	if set.Origin(domain.KeyInvestigationDailyTokens) != domain.OriginOrg ||
		set.Origin(domain.KeyInvestigationConcurrency) != domain.OriginOrg {
		t.Fatal("an org's write does not report origin org")
	}
	if v, o, ok := set.EffectiveInt(domain.KeyInvestigationDailyTokens); !ok || v != 50_000 || o != domain.OriginOrg {
		t.Fatalf("EffectiveInt = %d %q %v", v, o, ok)
	}
	back := set.Clear(domain.KeyInvestigationDailyTokens, domain.KeyInvestigationConcurrency)
	if back.Settings().InvestigationDailyTokens != domain.DefaultInvestigationDailyTokens {
		t.Fatal("Clear did not return the budget to oto's default")
	}
}

// TestThereIsNoUnlimitedAndNoZero — §6: "every control is a number an operator can
// read back". Zero is refused on the write path for both: a zero budget or a zero
// concurrency would be a kill switch by another name, and that is
// investigations_enabled's job.
func TestThereIsNoUnlimitedAndNoZero(t *testing.T) {
	t.Parallel()

	for name, p := range map[string]domain.SettingsPatch{
		"zero budget":         {InvestigationDailyTokens: intp(0)},
		"budget under a run":  {InvestigationDailyTokens: intp(999)},
		"budget past ceiling": {InvestigationDailyTokens: intp(1_000_000_001)},
		"zero concurrency":    {InvestigationConcurrency: intp(0)},
		"concurrency past 32": {InvestigationConcurrency: intp(33)},
	} {
		err := p.Validate()
		if !errs.IsKind(err, errs.KindValidation) {
			t.Fatalf("%s: err = %v, want a validation refusal", name, err)
		}
	}

	// A zero READ back (a hand-built Settings, a pre-key row) is repaired to the
	// finite default, never to "unlimited".
	n := (domain.Settings{}).Normalise()
	if n.InvestigationDailyTokens != domain.DefaultInvestigationDailyTokens ||
		n.InvestigationConcurrency != domain.DefaultInvestigationConcurrency {
		t.Fatalf("Normalise left %d / %d", n.InvestigationDailyTokens, n.InvestigationConcurrency)
	}
}

func TestTheInvestigationControlsCanBeDeclared(t *testing.T) {
	t.Parallel()

	d, err := domain.NewDeclarative([]domain.DeclaredEntry{
		{Key: "investigation_daily_tokens", ConfigKey: "OTO_TUNING_INVESTIGATION_DAILY_TOKENS", Value: "100000"},
		{Key: "investigation_concurrency", ConfigKey: "tuning.investigation_concurrency", Value: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	org := domain.Org{Overrides: domain.SettingsPatch{InvestigationConcurrency: intp(1)}}.WithDeclarative(d)
	if org.Settings.InvestigationDailyTokens != 100_000 || org.Settings.InvestigationConcurrency != 4 {
		t.Fatalf("declared values did not win: %+v", org.Settings)
	}
	if org.Origin(domain.KeyInvestigationConcurrency) != domain.OriginConfig {
		t.Fatalf("origin = %q, want config", org.Origin(domain.KeyInvestigationConcurrency))
	}

	_, err = domain.NewDeclarative([]domain.DeclaredEntry{
		{Key: "investigation_concurrency", ConfigKey: "OTO_TUNING_INVESTIGATION_CONCURRENCY", Value: "0"},
	})
	if vs := errs.ViolationsOf(err); len(vs) != 1 || vs[0].Field != "OTO_TUNING_INVESTIGATION_CONCURRENCY" {
		t.Fatalf("a declared zero: violations %+v do not name the config key", vs)
	}
}
