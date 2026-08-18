package domain

// ⭐ THIS FILE IS IN-PACKAGE, AND THAT IS THE POINT OF IT. Every other test in
// this package is `domain_test`, because every other test asks a question the
// exported surface can answer. This one asks whether the unexported
// `reasonSubjects` map is TOTAL over the Reason vocabulary — that no Reason is
// missing a subject AND that no key in it is something other than a Reason. The
// second half cannot be asked from outside: `Subject` and `Valid` can be
// interrogated one Reason at a time, but only this package can ENUMERATE the map
// and see a key that should not be there.

import "testing"

// TestTheReasonSubjectAllocationIsTotal is issue 36bb67d's done-when #2: the
// Reason → SubjectKind allocation covers the closed Reason set EXACTLY.
//
// ⭐ IT IS THE GATE ON A TWENTIETH REASON. `Subject` reads a map, and a Go map
// answers a missing key with the zero value — so a Reason added to `allReasons`
// and forgotten here would not fail to compile and would not panic. It would
// return the EMPTY SubjectKind, and a notification would be written claiming to be
// about nothing, where the only thing that noticed would be
// notifications_subjkind_ck rejecting a 23514 at 3am. `Valid` consults the same
// map, so the forgotten Reason is refused at the door as well — this test is what
// says that out loud, and it fails on the ADDITION rather than on the first row.
//
// The three assertions are the three ways the allocation can be wrong:
//
//   - a Reason with no subject — the forgotten nineteenth;
//   - a key that is not a Reason — a typo'd string literal, or a Reason that
//     migration 00018 removed from the vocabulary and nobody removed from here,
//     which would keep `Valid` accepting a value notifications_reason_ck refuses;
//   - a subject that is not an admitted kind — a value outside
//     notifications_subjkind_ck, which is the same 23514 arriving by a different
//     route.
func TestTheReasonSubjectAllocationIsTotal(t *testing.T) {
	t.Parallel()

	reasons := AllReasons()

	// Every Reason declares a subject, and that subject is one the schema admits.
	for _, r := range reasons {
		kind, ok := reasonSubjects[r]
		if !ok {
			t.Errorf("reason %q declares no subject. Every Reason must say WHAT it is "+
				"about (alert, case, alert_group or digest): a Reason with no entry here "+
				"returns the empty SubjectKind from Subject() and is refused by Valid(), so "+
				"it cannot produce a notification at all", r)
			continue
		}
		if !kind.Valid() {
			t.Errorf("reason %q declares subject %q, which is not one of the four kinds "+
				"notifications_subjkind_ck admits (%q, %q, %q, %q)",
				r, kind, SubjectAlert, SubjectCase, SubjectAlertGroup, SubjectDigest)
		}
		if got := r.Subject(); got != kind {
			t.Errorf("reason %q: Subject() returned %q but the allocation says %q", r, got, kind)
		}
	}

	// And nothing else is in there. The map is also the membership test used by
	// `Valid`, so a key that is not a Reason is a value oto would accept and
	// notifications_reason_ck would then reject.
	known := make(map[Reason]struct{}, len(reasons))
	for _, r := range reasons {
		known[r] = struct{}{}
	}
	for r := range reasonSubjects {
		if _, ok := known[r]; !ok {
			t.Errorf("reasonSubjects has a key %q that is not in the closed Reason set. "+
				"The map is what Valid() consults, so this makes oto accept a reason "+
				"notifications_reason_ck refuses", r)
		}
	}

	if len(reasonSubjects) != len(reasons) {
		t.Errorf("the allocation has %d entries for %d reasons; it must cover the closed "+
			"set exactly — no reason missing and no key that is not a reason",
			len(reasonSubjects), len(reasons))
	}

	// ⛔⛔ A RETIRED-REASON LOOP STOOD HERE AND IS DELETED WITH THE ONE VALUE IT
	// COVERED. `storm` was the only member of `retiredReasons`, and it was RETIRED —
	// unmintable but still decodable — on the argument that stored rows spell it and
	// `notifications_reason_ck` was not being narrowed. Migration 00060 narrows it,
	// with no `UPDATE` and with the maintainer's authorised database reset behind it,
	// so `storm` is DELETED from the vocabulary and `Reason.Retired()` no longer
	// exists. The loop would now iterate over nothing and assert nothing, which is a
	// weaker thing to leave behind than the two total-coverage assertions above —
	// those still hold every live Reason to having a subject, which is the property
	// the loop existed to protect for the retired one.
	//
	// ⚠️ `refired` IS THE VALUE TO WATCH IF RETIREMENT COMES BACK. Nothing has minted
	// it since ADR 0040 deleted T8, its own comment in reason.go says so, and its
	// retirement was deliberately never made mechanical. Whoever makes it so brings
	// back a predicate and a loop like this one with it.
}
