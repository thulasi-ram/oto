package domain_test

// THE ORG'S INVESTIGATION KILL SWITCH (ADR 0053 §6, git-bug 180a525): one boolean
// settings key, `investigations_enabled`, shipped true, with an origin like every
// other key and a declarative override that wins.

import (
	"testing"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

func boolp(v bool) *bool { return &v }

func TestTheInvestigationSwitchShipsOnAndReportsItsOrigin(t *testing.T) {
	t.Parallel()

	var none domain.SettingsPatch
	if !none.Settings().InvestigationsEnabled || !domain.DefaultSettings().InvestigationsEnabled {
		t.Fatal("an org that wrote nothing must be able to investigate")
	}
	if got := none.Origin(domain.KeyInvestigationsEnabled); got != domain.OriginDefault {
		t.Fatalf("origin = %q, want default", got)
	}

	off := none.Merge(domain.SettingsPatch{InvestigationsEnabled: boolp(false)})
	if off.Settings().InvestigationsEnabled {
		t.Fatal("the org pulled the switch and Settings still reads enabled")
	}
	if got := off.Origin(domain.KeyInvestigationsEnabled); got != domain.OriginOrg {
		t.Fatalf("origin = %q, want org", got)
	}
	if err := off.Validate(); err != nil {
		t.Fatalf("a boolean has no range to violate: %v", err)
	}
	found := false
	for _, k := range off.Overridden() {
		found = found || k == domain.KeyInvestigationsEnabled
	}
	if !found {
		t.Fatal("Overridden does not list the switch the org wrote")
	}

	// A later write of an unrelated key leaves the switch alone.
	still := off.Merge(domain.SettingsPatch{ResolveGraceS: intp(600)})
	if still.Settings().InvestigationsEnabled {
		t.Fatal("merging another key released the switch")
	}

	back := off.Clear(domain.KeyInvestigationsEnabled)
	if !back.Settings().InvestigationsEnabled || back.Origin(domain.KeyInvestigationsEnabled) != domain.OriginDefault {
		t.Fatal("Clear did not return the switch to oto's default")
	}
}

// TestAZeroSettingsFailsClosed — a Settings built by hand reads "disabled", and
// Normalise must not "repair" it into enabled.
func TestAZeroSettingsFailsClosed(t *testing.T) {
	t.Parallel()
	if (domain.Settings{}).Normalise().InvestigationsEnabled {
		t.Fatal("Normalise turned a false kill switch into true")
	}
}

func TestTheInvestigationSwitchCanBeDeclared(t *testing.T) {
	t.Parallel()

	for name, value := range map[string]any{"env string": " FALSE ", "yaml bool": false} {
		d, err := domain.NewDeclarative([]domain.DeclaredEntry{{
			Key: "investigations_enabled", ConfigKey: "OTO_TUNING_INVESTIGATIONS_ENABLED", Value: value,
		}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !d.Manages(domain.KeyInvestigationsEnabled) {
			t.Fatalf("%s: the declarative layer does not manage the switch", name)
		}
		// The org says on; configuration says off; configuration wins.
		org := domain.Org{Overrides: domain.SettingsPatch{InvestigationsEnabled: boolp(true)}}.WithDeclarative(d)
		if org.Settings.InvestigationsEnabled {
			t.Fatalf("%s: the declared false did not win over the org's true", name)
		}
		if org.Origin(domain.KeyInvestigationsEnabled) != domain.OriginConfig {
			t.Fatalf("%s: origin = %q, want config", name, org.Origin(domain.KeyInvestigationsEnabled))
		}
		if org.Shadowed().InvestigationsEnabled == nil || !*org.Shadowed().InvestigationsEnabled {
			t.Fatalf("%s: the org's own true is not reported as shadowed", name)
		}
	}

	for _, bad := range []any{"off", "yes", 1, ""} {
		_, err := domain.NewDeclarative([]domain.DeclaredEntry{{
			Key: "investigations_enabled", ConfigKey: "tuning.investigations_enabled", Value: bad,
		}})
		if !errs.IsKind(err, errs.KindValidation) {
			t.Fatalf("value %#v: err = %v, want a validation refusal", bad, err)
		}
		if vs := errs.ViolationsOf(err); len(vs) != 1 || vs[0].Field != "tuning.investigations_enabled" {
			t.Fatalf("value %#v: violations %+v do not name the config key", bad, vs)
		}
	}
}
