package domain_test

// THE REMEDY APPROVAL WINDOW (ADR 0054 §2, git-bug 4148256): the operator-set time after which
// a proposed Remedy, or an approved one not yet executed, expires. An integer key on the
// bounds table like every other — an hour shipped, refused outside a minute to a day, origin
// reported, and a zero read back repaired to the hour rather than to "never expires".

import (
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

func TestTheRemedyWindowShipsAnHourIsBoundedAndReportsItsOrigin(t *testing.T) {
	t.Parallel()

	var none domain.SettingsPatch
	if got := none.Settings().RemedyApprovalWindow; got != time.Hour {
		t.Fatalf("an org that wrote nothing expires Remedies after %s, want an hour", got)
	}
	if none.Origin(domain.KeyRemedyApprovalWindow) != domain.OriginDefault {
		t.Fatal("an unwritten window does not report origin default")
	}
	set := none.Merge(domain.SettingsPatch{RemedyApprovalWindowS: intp(900)})
	if err := set.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := set.Settings().RemedyApprovalWindow; got != 15*time.Minute {
		t.Fatalf("the org's window did not take: %s", got)
	}
	if v, o, ok := set.EffectiveInt(domain.KeyRemedyApprovalWindow); !ok || v != 900 || o != domain.OriginOrg {
		t.Fatalf("EffectiveInt = %d %q %v", v, o, ok)
	}
	for name, p := range map[string]domain.SettingsPatch{
		"zero":           {RemedyApprovalWindowS: intp(0)},
		"under a minute": {RemedyApprovalWindowS: intp(59)},
		"past a day":     {RemedyApprovalWindowS: intp(86401)},
	} {
		if err := p.Validate(); !errs.IsKind(err, errs.KindValidation) {
			t.Fatalf("%s: err = %v, want a validation refusal", name, err)
		}
	}
	if n := (domain.Settings{}).Normalise(); n.RemedyApprovalWindow != time.Hour {
		t.Fatalf("Normalise left the window at %s; a zero must never read as never expiring", n.RemedyApprovalWindow)
	}
}
