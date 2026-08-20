package domain_test

import (
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/notification/domain"
)

// The two axes migration 00072 added to `notification_policies` — the
// subject-kind binding and the count-over-window condition (git-bug `7570090`,
// done-when 8).
//
// ⭐ THE FILE EXISTS BECAUSE BOTH AXES ARE CROSS-FIELD AND THE CROSSES ARE THE
// POINT. Neither is a range that a `validate` tag could have carried: a binding is
// checked against the policy's own `reasons`, and a count is checked against the
// binding. A test per bound would assert the halves and miss every rule that
// makes the pair mean anything.

// TestTheSubjectKindOrderIsTheSubjectKindSet binds the ORDERED vocabulary to the
// MEMBERSHIP one.
//
// They are two declarations in idempotency.go — `allSubjectKinds` for the order a
// published enum is rendered in, `subjectKinds` for the test `Valid` consults —
// exactly as `allReasons` and `reasonSubjects` are two declarations. A fourth kind
// added to one and not the other would produce a contract enum missing a value the
// column admits, or a value the column admits that no client can name, and neither
// fails to compile.
func TestTheSubjectKindOrderIsTheSubjectKindSet(t *testing.T) {
	t.Parallel()

	all := domain.AllSubjectKinds()
	if len(all) == 0 {
		t.Fatal("AllSubjectKinds() enumerates nothing, so every assertion below is vacuous")
	}

	seen := map[domain.SubjectKind]bool{}
	for _, k := range all {
		if !k.Valid() {
			t.Errorf("AllSubjectKinds() lists %q, which SubjectKind.Valid() refuses — the "+
				"ordered vocabulary and the membership test have drifted, and a kind that is "+
				"published and unstorable is worse than either mistake alone", k)
		}
		if seen[k] {
			t.Errorf("AllSubjectKinds() lists %q twice; it is a SET", k)
		}
		seen[k] = true
	}

	// The mutation guard. `AllSubjectKinds` copies for the reason `AllReasons` does:
	// a caller holding the package's own backing array can re-order a published
	// vocabulary for every later caller in the process.
	all[0] = domain.SubjectKind("mutated")
	if again := domain.AllSubjectKinds(); again[0] == domain.SubjectKind("mutated") {
		t.Fatal("AllSubjectKinds() hands out its backing array, so one caller can re-order " +
			"the vocabulary for every other")
	}
}

// TestTheSubjectKindCeilingIsTheSizeOfTheSubjectKindEnum holds
// MaxPolicySubjectKinds to `len(AllSubjectKinds())`, which is the same rule
// MaxPolicyReasons is held to and for the same reason: `subject_kinds` is a SET
// over a closed vocabulary, so the most a policy can carry is that vocabulary
// once. Any other ceiling is either unreachable or outlaws a legal policy.
//
// The number has already moved once in the direction people forget — it would have
// been 4 while `alert_group` was a kind — so it is asserted rather than reviewed.
func TestTheSubjectKindCeilingIsTheSizeOfTheSubjectKindEnum(t *testing.T) {
	t.Parallel()

	all := domain.AllSubjectKinds()
	if domain.MaxPolicySubjectKinds != len(all) {
		t.Fatalf("MaxPolicySubjectKinds is %d and the SubjectKind enum has %d values. The "+
			"column is a SET, so the most a policy can carry is the whole vocabulary once",
			domain.MaxPolicySubjectKinds, len(all))
	}
}

// TestAnEmptyBindingClaimsEveryAltitude is the direction that must not be got
// backwards.
//
// The failure mode on this path is a `no_policy` SUPPRESSION rather than an error
// anybody sees, so an empty binding that claimed NOTHING would silently mute every
// policy written before migration 00072 — every policy that exists. `Binds` and
// `Handles` are asserted separately because the second folds the first in, and a
// `Handles` that stopped consulting the binding would still pass a `Binds` test.
func TestAnEmptyBindingClaimsEveryAltitude(t *testing.T) {
	t.Parallel()

	var empty domain.SubjectBinding
	if !empty.Unrestricted() {
		t.Fatal("the zero SubjectBinding does not report itself unrestricted")
	}
	for _, k := range domain.AllSubjectKinds() {
		if !empty.Binds(k) {
			t.Errorf("an empty binding refuses %q — an unconfigured policy must claim every "+
				"altitude, or migration 00072 mutes every policy that predates it", k)
		}
	}

	p := validPolicy()
	for _, r := range p.Reasons {
		if !p.Handles(r) {
			t.Errorf("a policy with no binding stopped handling %q, which it declared", r)
		}
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("a policy with no binding was refused: %v", err)
	}
}

// TestABindingNarrowsHandles is the axis actually deciding something. It is the
// whole of what makes `subject_kinds` more than stored text, and it decides it
// through `Handles` — the gate `PolicyService.Evaluate` already calls — rather than
// through a second gate with its own call sites.
func TestABindingNarrowsHandles(t *testing.T) {
	t.Parallel()

	p := validPolicy()
	// `fired` is case-subject and `snoozed` is alert-subject, so this policy spans
	// two altitudes and a binding can cut it in half.
	p.Reasons = []domain.Reason{domain.ReasonFired, domain.ReasonSnoozed}
	p.Subjects = domain.SubjectBinding{domain.SubjectCase}

	if !p.Handles(domain.ReasonFired) {
		t.Error("a case-bound policy stopped handling `fired`, which is a case-subject reason")
	}
	if p.Handles(domain.ReasonSnoozed) {
		t.Error("a case-bound policy still handles `snoozed`, which is an alert-subject " +
			"reason — the binding decides nothing")
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("a coherent binding was refused: %v", err)
	}
}

// TestABindingThatAdmitsNoReasonIsRefused is the coherence rule, and it is the one
// rule the database cannot hold: it needs `Reason.Subject()`, a Go map a CHECK may
// not consult.
//
// ⚠️ THE POLICY IT REFUSES DOES NOT ERROR AT RUNTIME, WHICH IS WHY IT IS REFUSED AT
// THE DOOR. It routes nothing and records a `no_policy` suppression for every fact
// it sees, forever, while its settings screen looks configured — silent suppression
// (SPEC §B.6) produced by a combination each half of which is individually legal.
func TestABindingThatAdmitsNoReasonIsRefused(t *testing.T) {
	t.Parallel()

	p := validPolicy()
	// Both declared reasons are case-subject; the binding admits only alerts.
	p.Subjects = domain.SubjectBinding{domain.SubjectAlert}

	err := p.Validate()
	if err == nil {
		t.Fatal("a policy whose binding admits none of its own reasons was accepted — it " +
			"would route nothing and suppress every fact as no_policy")
	}
	if _, ok := violation(err, "subject_kinds", "incoherent"); !ok {
		t.Errorf("the refusal is not reported as subject_kinds/incoherent, so the settings "+
			"form has no control to point at: %v", err)
	}
}

// TestABindingIsAClosedSet covers the two rules the DDL also holds, so that a
// bad binding is a field-level 422 rather than a 23514 an operator has to decode.
func TestABindingIsAClosedSet(t *testing.T) {
	t.Parallel()

	p := validPolicy()
	p.Subjects = domain.SubjectBinding{domain.SubjectKind("alert_group")}
	if err := p.Validate(); err == nil {
		t.Fatal("a binding naming the deleted `alert_group` kind was accepted")
	} else if _, ok := violation(err, "subject_kinds", "enum"); !ok {
		t.Errorf("the refusal is not reported as subject_kinds/enum: %v", err)
	}

	p = validPolicy()
	p.Subjects = domain.SubjectBinding{domain.SubjectCase, domain.SubjectCase}
	if err := p.Validate(); err == nil {
		t.Fatal("a binding listing `case` twice was accepted; subject_kinds is a SET")
	} else if _, ok := violation(err, "subject_kinds", "duplicate_items"); !ok {
		t.Errorf("the refusal is not reported as subject_kinds/duplicate_items: %v", err)
	}
}

// TestACountConditionClearsNothingUntilItIsConfigured is the default-direction
// assertion for the second axis, and it is the OPPOSITE default from
// `Digest.Clears`.
//
// A disabled condition must clear every count, including zero. Returning false for
// the unconfigured case would make silence the default state of every policy in the
// system — the `no_policy` suppression this axis exists to avoid becoming.
func TestACountConditionClearsNothingUntilItIsConfigured(t *testing.T) {
	t.Parallel()

	var none domain.CountOverWindow
	if none.Enabled() {
		t.Fatal("the zero CountOverWindow reports itself enabled")
	}
	for _, n := range []int{0, 1, 1000} {
		if !none.Clears(n) {
			t.Errorf("an unconfigured count condition refused a count of %d — a policy that "+
				"asked for no condition has no reason not to speak", n)
		}
	}

	c := domain.CountOverWindow{Min: 5, Window: time.Hour}
	if !c.Enabled() {
		t.Fatal("a condition with both halves reports itself disabled")
	}
	if c.Clears(4) {
		t.Error("a count of 4 cleared a floor of 5")
	}
	if !c.Clears(5) {
		t.Error("a count of 5 did not clear a floor of 5; the floor is inclusive, and the " +
			"fact being evaluated is one of the five")
	}

	// Half a condition constrains nothing, because `policies_count_pair_ck` refuses
	// the row and this method refuses to act on one that somehow exists.
	if (domain.CountOverWindow{Min: 5}).Enabled() {
		t.Error("a threshold with no window reports itself enabled — it is a threshold over " +
			"unbounded history")
	}
	if (domain.CountOverWindow{Window: time.Hour}).Enabled() {
		t.Error("a window with no threshold reports itself enabled — it counts facts and " +
			"compares the number against nothing")
	}
}

// TestACountConditionNeedsBothHalvesAndOneUnit is the pair rule and the unit rule,
// which are the two crosses that make the two axes one feature.
func TestACountConditionNeedsBothHalvesAndOneUnit(t *testing.T) {
	t.Parallel()

	// A threshold with no window: reported against the window, which is the field
	// the operator has to fill in.
	p := validPolicy()
	p.Subjects = domain.SubjectBinding{domain.SubjectCase}
	p.Count = domain.CountOverWindow{Min: 5}
	if err := p.Validate(); err == nil {
		t.Fatal("a count threshold with no window was accepted")
	} else if _, ok := violation(err, "count_window_seconds", "incomplete"); !ok {
		t.Errorf("the refusal is not reported as count_window_seconds/incomplete: %v", err)
	}

	// A window with no threshold: the SYMMETRIC half, which the digest's pair rule
	// deliberately permits and this one does not.
	p = validPolicy()
	p.Subjects = domain.SubjectBinding{domain.SubjectCase}
	p.Count = domain.CountOverWindow{Window: time.Hour}
	if err := p.Validate(); err == nil {
		t.Fatal("a count window with no threshold was accepted; the pair rule is symmetric, " +
			"unlike policies_digest_pair_ck")
	} else if _, ok := violation(err, "count_min", "incomplete"); !ok {
		t.Errorf("the refusal is not reported as count_min/incomplete: %v", err)
	}

	// A threshold of one states no condition: the fact being evaluated is inside the
	// window, so it clears unconditionally.
	p = validPolicy()
	p.Subjects = domain.SubjectBinding{domain.SubjectCase}
	p.Count = domain.CountOverWindow{Min: 1, Window: time.Hour}
	if err := p.Validate(); err == nil {
		t.Fatal("a count threshold of 1 was accepted; it is cleared by the fact being " +
			"evaluated and describes a behaviour that does not exist")
	} else if _, ok := violation(err, "count_min", "range"); !ok {
		t.Errorf("the refusal is not reported as count_min/range: %v", err)
	}

	// THE UNIT RULE, in both directions a binding can fail it: none, and two.
	for _, tc := range []struct {
		name  string
		bind  domain.SubjectBinding
		count int
	}{
		{"unrestricted", nil, 0},
		{"two kinds", domain.SubjectBinding{domain.SubjectAlert, domain.SubjectCase}, 2},
	} {
		p := validPolicy()
		// `snoozed` keeps the two-kind binding coherent, so this test is testing the
		// unit rule and not the coherence rule.
		p.Reasons = []domain.Reason{domain.ReasonFired, domain.ReasonSnoozed}
		p.Subjects = tc.bind
		p.Count = domain.CountOverWindow{Min: 5, Window: time.Hour}
		err := p.Validate()
		if err == nil {
			t.Errorf("%s: a count condition over %d subject kinds was accepted — a count "+
				"needs a unit, and adding an identity to an episode yields a number about "+
				"nothing", tc.name, tc.count)
			continue
		}
		if _, ok := violation(err, "subject_kinds", "required"); !ok {
			t.Errorf("%s: the refusal is not reported as subject_kinds/required: %v",
				tc.name, err)
		}
	}

	// And the shape that must be legal, so this file is not asserting that nothing
	// works: one kind, both halves, in range.
	p = validPolicy()
	p.Subjects = domain.SubjectBinding{domain.SubjectCase}
	p.Count = domain.CountOverWindow{Min: 5, Window: time.Hour}
	if err := p.Validate(); err != nil {
		t.Fatalf("a well-formed count condition was refused: %v", err)
	}
}

// TestACountConditionCountsCasesAndNothingElse is the narrowing that keeps
// `count_min` from being a permanent mute on one binding and an inert knob on the
// other (`policies_count_case_ck`).
//
// Both bindings below satisfy the CARDINALITY rule above — exactly one kind — so
// nothing else in this file catches them, and each fails for its own reason. An
// alert-subject `subject_id` is the alert IDENTITY, unchanged across every firing, so
// `count(DISTINCT subject_id)` for that policy never climbs and `seen + 1 < count_min`
// suppresses every notification it routes, forever. A digest is minted by the digest
// tick against `digest_floor` and never reaches a suppressor, so a digest-bound count
// decides nothing at all.
func TestACountConditionCountsCasesAndNothingElse(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		bind    domain.SubjectKind
		reasons []domain.Reason
	}{
		{"alert", domain.SubjectAlert, []domain.Reason{domain.ReasonComment}},
		{"digest", domain.SubjectDigest, []domain.Reason{domain.ReasonDigest}},
	} {
		p := validPolicy()
		// A Reason at the bound altitude, so this is testing the unit rule and not the
		// coherence rule in `validateSubjects`.
		p.Reasons = tc.reasons
		p.Subjects = domain.SubjectBinding{tc.bind}
		p.Count = domain.CountOverWindow{Min: 5, Window: time.Hour}

		err := p.Validate()
		if err == nil {
			t.Errorf("%s: a count condition bound to `%s` was accepted — a count over that "+
				"kind either never climbs above one or is read by nothing", tc.name, tc.bind)
			continue
		}
		if _, ok := violation(err, "subject_kinds", "unsupported"); !ok {
			t.Errorf("%s: the refusal is not reported as subject_kinds/unsupported: %v",
				tc.name, err)
		}
	}

	// The binding that must stay legal: five firings of one alert are five Cases and
	// five distinct subjects, which is the count the feature is sold on.
	p := validPolicy()
	p.Subjects = domain.SubjectBinding{domain.SubjectCase}
	p.Count = domain.CountOverWindow{Min: 5, Window: time.Hour}
	if err := p.Validate(); err != nil {
		t.Fatalf("a case-bound count condition was refused: %v", err)
	}
}

// TestADigestWindowNeedsTheDigestAltitudeAndSaysSo is the second way `Policy.Handles`
// can refuse a policy's own digests since migration 00072, and the field path is the
// point of the test.
//
// `policies_digest_reason_ck` makes the window imply the `digest` REASON, and a policy
// that lists the Reason but does not BIND the altitude still routes none of its own
// digests: `Digests()` is false, so `SweepOrg` warns once per tick forever and
// `ReconcileOrg` skips the policy, hiding the gap the skip creates. The violation has
// to name `subject_kinds` and not `reasons`, because a settings form points at the
// field it is given and the `reasons` list in this policy is already correct.
func TestADigestWindowNeedsTheDigestAltitudeAndSaysSo(t *testing.T) {
	t.Parallel()

	p := validPolicy()
	p.Reasons = []domain.Reason{domain.ReasonFired, domain.ReasonDigest}
	p.Subjects = domain.SubjectBinding{domain.SubjectCase}
	p.Digest = domain.Digest{Window: 10 * time.Minute}

	err := p.Validate()
	if err == nil {
		t.Fatal("a digest window on a binding that omits `digest` was accepted — the policy " +
			"routes none of its own digests and nothing but this refusal ever says so")
	}
	if _, ok := violation(err, "subject_kinds", "incoherent"); !ok {
		t.Errorf("the refusal is not reported as subject_kinds/incoherent: %v", err)
	}
	if _, ok := violation(err, "reasons", "required"); ok {
		t.Error("the refusal is ALSO reported against `reasons`, which is the misattribution " +
			"this split exists to remove: the reason list names `digest` and is correct")
	}

	// And the binding that must stay legal, so this is not asserting that a digest
	// policy cannot be written at all.
	p.Subjects = domain.SubjectBinding{domain.SubjectCase, domain.SubjectDigest}
	if err := p.Validate(); err != nil {
		t.Fatalf("a digest policy binding both altitudes it routes was refused: %v", err)
	}
}

// TestAnExplicitCountBelowItsFloorIsCaughtOnThePatch covers the hole the MERGED
// view cannot see.
//
// `Count.Min` and `Count.Window` both use zero for "unset", so `{"count_min": 1}`
// and `{"count_min": 0}` fold to the same value `validateCount` reads as absent —
// while the repository writes the literal the operator sent and
// `policies_count_min_ck` answers with a 23514 carrying no field path. The check
// therefore has to happen where the distinction still exists: on the patch, before
// the fold. It is the same defect `ValidateExplicit` was introduced for on
// `digest_floor`, in both halves rather than one.
func TestAnExplicitCountBelowItsFloorIsCaughtOnThePatch(t *testing.T) {
	t.Parallel()

	one := 1
	pOne := &one
	patch := domain.PolicyPatch{CountMin: &pOne}
	if err := patch.ValidateExplicit(); err == nil {
		t.Error("an explicit count_min of 1 survived the patch check, so it reaches " +
			"policies_count_min_ck as a 23514 with no field for the form to point at")
	} else if _, ok := violation(err, "count_min", "range"); !ok {
		t.Errorf("the refusal is not reported as count_min/range: %v", err)
	}

	short := 30 * time.Second
	pShort := &short
	patch = domain.PolicyPatch{CountWindow: &pShort}
	if err := patch.ValidateExplicit(); err == nil {
		t.Error("an explicit count window of 30s survived the patch check")
	} else if _, ok := violation(err, "count_window_seconds", "range"); !ok {
		t.Errorf("the refusal is not reported as count_window_seconds/range: %v", err)
	}

	// A CLEAR — a pointer to nil — is not a violation. It is how an operator turns
	// the condition off, and a check that refused it would make the axis one-way.
	var clearMin *int
	var clearWin *time.Duration
	patch = domain.PolicyPatch{CountMin: &clearMin, CountWindow: &clearWin}
	if err := patch.ValidateExplicit(); err != nil {
		t.Errorf("clearing both halves of the condition was refused: %v", err)
	}
	if patch.IsEmpty() {
		t.Error("a patch clearing the count condition reports itself empty, so it would be " +
			"answered with `supply at least one field to change`")
	}
}

// TestAPatchNamingOnlyABindingIsNotEmpty is the `IsEmpty` half, which is easy to
// forget and fails as a 422 on a request that asked for something real.
func TestAPatchNamingOnlyABindingIsNotEmpty(t *testing.T) {
	t.Parallel()

	// The EMPTY binding, which is how a binding is REMOVED. It is the case most
	// likely to be treated as absent, because the slice it carries has no elements.
	empty := domain.SubjectBinding{}
	if (domain.PolicyPatch{Subjects: &empty}).IsEmpty() {
		t.Error("a patch clearing the subject binding reports itself empty — `[]` means " +
			"`claim every altitude`, which is a real instruction and not an absence")
	}
}
