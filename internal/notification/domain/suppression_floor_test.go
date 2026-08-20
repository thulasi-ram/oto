package domain_test

import (
	"testing"

	"github.com/thulasiram/oto/internal/notification/domain"
)

// TestTheFloorIsInTheVocabularyDirectlyBelowTheCeiling pins the one property of
// `below_threshold` that is invisible everywhere else: WHERE it sits.
//
// ⭐ THE VALUE ITSELF IS ALREADY GATED THREE TIMES AND ITS POSITION IS GATED
// NOWHERE. `notifications_suppmap_ck` (migration 00073) admits the string,
// `TestContractEnumsMatchTheirDomainEnum` holds the contract enum to this
// vocabulary as an ORDERED list, and `TestTheSuppressedReasonFilterNamesEveryDomainReason`
// holds the query filter to it as a SET. What none of them can catch is the chain
// being re-ordered coherently everywhere at once — and the order is not a
// presentation detail: `Suppressors.Winner` records the FIRST match, so a policy
// carrying both a floor and a ceiling reports whichever of the two this slice puts
// first, and that is the sentence an operator reads to decide which number to
// change.
//
// The rank itself is this release's PROPOSAL and not a citation — SPEC §B.8.2 as
// written lists six values and not `below_threshold` (see `suppressorOrder`) — so what
// is pinned here is the argument for it, in both directions:
//
//   - THE CEILING OUTRANKS THE FLOOR. A spent throttle is an ACTIVE fact — oto has
//     been speaking about this conversation and stopped — while an unmet floor is
//     the RESTING state of every policy that carries one, since a count condition
//     is unmet for most of its own window by design. A resting state that outranked
//     an active damper would mask it on every policy carrying both.
//   - THE FLOOR OUTRANKS `verbosity`. Both dampers belong to the POLICY that routed
//     the fact; `verbosity` is a property of a DESTINATION, and §B.8.2 already ranks
//     the policy's own decisions above the channel's preferences.
func TestTheFloorIsInTheVocabularyDirectlyBelowTheCeiling(t *testing.T) {
	t.Parallel()

	if !domain.SuppressedBelowThreshold.Valid() {
		t.Fatalf("%q is not in the closed SuppressedReason set. `Suppressors.Add` silently "+
			"IGNORES a reason that fails Valid(), so an unlisted value does not fail loudly — "+
			"the count condition suppresses the fact and the row records no reason at all, "+
			"which notifications_supp_ck then refuses as a 23514 on the notification path",
			domain.SuppressedBelowThreshold)
	}

	rank := map[domain.SuppressedReason]int{}
	for i, r := range domain.SuppressorOrder() {
		rank[r] = i
	}

	if rank[domain.SuppressedThrottled] >= rank[domain.SuppressedBelowThreshold] {
		t.Errorf("the count condition's floor (%q, rank %d) is not ranked below the throttle's "+
			"ceiling (%q, rank %d). They are the same two policy columns read with opposite "+
			"senses, and the ceiling wins when both apply: a spent cap is an active fact, an "+
			"unmet floor is the resting state of every policy that carries one",
			domain.SuppressedBelowThreshold, rank[domain.SuppressedBelowThreshold],
			domain.SuppressedThrottled, rank[domain.SuppressedThrottled])
	}
	if rank[domain.SuppressedBelowThreshold] >= rank[domain.SuppressedVerbosity] {
		t.Errorf("the count condition's floor (%q, rank %d) is not ranked above %q (rank %d). "+
			"Both dampers belong to the POLICY; verbosity belongs to a DESTINATION, and §B.8.2 "+
			"ranks the policy's own decision first because it explains the silence for every "+
			"destination at once",
			domain.SuppressedBelowThreshold, rank[domain.SuppressedBelowThreshold],
			domain.SuppressedVerbosity, rank[domain.SuppressedVerbosity])
	}

	// The winner when a policy carrying both is refused by both. This is the whole
	// observable consequence of the two rankings above, asserted through the API the
	// evaluator actually calls rather than through the slice.
	var sup domain.Suppressors
	sup.Add(domain.SuppressedBelowThreshold)
	sup.Add(domain.SuppressedThrottled)
	got, ok := sup.Winner()
	if !ok || got != domain.SuppressedThrottled {
		t.Errorf("a fact refused by both the ceiling and the floor recorded %q (found=%v), "+
			"want %q", got, ok, domain.SuppressedThrottled)
	}
	// And the full set is still reported, because "why was I not told?" deserves both
	// numbers rather than the one that won.
	if all := sup.All(); len(all) != 2 {
		t.Errorf("both dampers applied but All() returned %v — the winner is what the row "+
			"records, the set is what the UI shows, and an operator told only about the "+
			"throttle would raise a cap that was not the whole reason", all)
	}
}

// TestAFloorWithNoUnitEvaluatesNothing is the guard on the one branch in the
// evaluator that decides NOT to decide.
//
// `policies_count_subject_ck` requires a count condition to name exactly one
// `subject_kind`, because a count needs a unit. The gate in
// `NotificationService.suppressors` reads that unit with `SubjectBinding.Sole` and
// skips the whole comparison when there is none — the default-OPEN direction, which
// is `CountOverWindow.Clears`'s own rule. This asserts the two halves of `Sole`
// that direction depends on, so a future change that made the empty binding return
// a default kind would fail here rather than by suppressing every fact under a
// policy whose count is about nothing.
func TestAFloorWithNoUnitEvaluatesNothing(t *testing.T) {
	t.Parallel()

	if _, ok := (domain.SubjectBinding{}).Sole(); ok {
		t.Error("the empty binding named a sole subject kind. Empty means EVERY kind, which " +
			"is precisely the answer that has no unit — a count over it would be adding " +
			"alert identities to firing episodes")
	}
	if _, ok := (domain.SubjectBinding{domain.SubjectAlert, domain.SubjectCase}).Sole(); ok {
		t.Error("a two-kind binding named a sole subject kind; two kinds supply two units")
	}
	kind, ok := (domain.SubjectBinding{domain.SubjectCase}).Sole()
	if !ok || kind != domain.SubjectCase {
		t.Errorf("a single-kind binding returned (%q, %v), want (%q, true) — this is the "+
			"value the count query is scoped by, so a wrong answer here counts the wrong "+
			"altitude of fact", kind, ok, domain.SubjectCase)
	}
}
