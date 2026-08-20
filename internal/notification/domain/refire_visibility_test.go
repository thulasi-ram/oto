package domain_test

import (
	"testing"

	"github.com/thulasiram/oto/internal/notification/domain"
)

// ⛔⛔ THIS FILE PINS A KNOWN CONTRADICTION, NOT A DESIRED BEHAVIOUR.
//
// ADR 0020 granted exactly two Reasons an irreversible `reply_broadcast`, and
// `refired` was one of them, on this reasoning: a re-fire lands in a conversation
// that is already open, so it produces a root UPDATE and a thread reply and NO new
// root message — *"the thread said resolved and people stopped following it"*. Its
// quiet form is invisible, which is the only property that earned a channel post.
//
// ⛔⛔ THREAD-BROADCAST IS NOW REMOVED FROM OTO ENTIRELY (git-bug `7570090`), AND THAT
// MAKES THIS FILE'S DEFECT WORSE RATHER THAN MOOT. The mechanism was already reaching
// nobody — `refired` had no producer after ADR 0040 retired T8 and the only other
// member was opt-in and default off (see the ⛔⭐ block in `PlanFor`) — so no operator
// loses a delivery. But it was also the one mechanism that could have rescued a
// re-fire from the verbosity hole below, and the hole is unchanged. ⭐ THE VERBOSITY
// GATE IS NOW THE ONLY THING STANDING BETWEEN A RE-FIRE AND SILENCE, which is exactly
// what the second test asserts.
//
// ⚠️ THE REASONING SURVIVED ADR 0040 AND THE WORDING DID NOT. This paragraph used
// to open "a re-fire INSIDE `refire_grace` reopens the existing case". T8 is
// retired: a closed case is strictly terminal and a re-fire ALWAYS opens a new one
// at the next `seq`, with no window deciding anything. That changed which ROW is
// written, not which message is sent — the root card belongs to the AlertGroup and
// is updated in place while the generation is open — so everything below still
// holds. It holds for MORE re-fires than it used to, because there is no longer a
// window that some of them fall outside of.
//
// §H.6's verbosity table then drops the `refired` reply entirely at
// `firing_and_resolved` and `firing_only`, and `refired` is NOT in
// `ungatedReplies`. So on a channel set quieter than the default `status_changes`,
// a re-fire is **completely silent** — the one case ADR 0020 said must never be.
//
// ADR 0026 found this while deriving `refire_grace`, back when the length of the
// grace decided how many re-fires fell into the hole — it is why the derived
// default is the SMALLEST value that reaches the modal rule rather than the
// largest that covers every rule. Since ADR 0040 all of them do. It deliberately
// did NOT fix it: resolving it means deciding
// whether `firing_only` may delete a transition ADR 0020 called unmissable, which
// is a product decision about what a documented verbosity level MEANS, not a
// tuning one. It touches SPEC §H.6, ADR 0020, the OpenAPI schema and the renderer.
//
// The tests below assert what ships TODAY. When the owner decides, one of them
// will fail, which is the point: the defect is visible in the test suite rather
// than discovered during an incident.

// The half that is correct: at the shipped default verbosity, a re-fire survives the
// gate and reaches the thread.
//
// (⛔ THIS TEST ALSO ASSERTED `DefaultBroadcastPolicy().Warrants(ReasonRefired)`, and
// there is no broadcast policy to ask — git-bug `7570090`. The assertion is deleted
// rather than re-pointed: it pinned a default set, and an empty mechanism has no
// default set to pin.)
func TestARefireReachesTheThreadAtTheDefaultVerbosity(t *testing.T) {
	t.Parallel()

	// The schema default, and what `Normalise` falls back to.
	v := domain.VerbosityStatusChanges
	if !v.AllowsReply(domain.ReasonRefired) {
		t.Fatalf("verbosity %q drops the `refired` reply. A re-fire produces no new root "+
			"message, so dropping it makes a genuine re-fire silent at "+
			"oto's OWN DEFAULT — a missed page out of the box", v)
	}
}

// ⚠️ THE HALF THAT IS NOT. This asserts the DEFECT, so that fixing it is a
// deliberate act with a failing test attached rather than a silent behaviour
// change. If you are here because this test failed, read the header, then update
// SPEC §H.6 and ADR 0020 together and delete this test.
func TestAQuieterChannelSilentlyDropsTheRefire(t *testing.T) {
	t.Parallel()

	for _, v := range []domain.Verbosity{
		domain.VerbosityFiringAndResolved,
		domain.VerbosityFiringOnly,
	} {
		if v.AllowsReply(domain.ReasonRefired) {
			t.Fatalf("verbosity %q now delivers the `refired` reply. That is almost certainly an "+
				"improvement — ADR 0020 calls this transition unmissable — but it resolves the "+
				"contradiction ADR 0026 recorded, so §H.6, ADR 0020 and the warning in "+
				"docs/setup/tuning.md must be updated to match, and this test deleted", v)
		}
	}

	// And it is not rescued by the always-deliver override, which is what makes the
	// drop total rather than merely quiet. `expired` IS in that set, on the stated
	// reasoning that losing sight of a signal must never be silent — a re-fire
	// nobody is told about is the same class of failure.
	if domain.VerbosityFiringOnly.AllowsReply(domain.ReasonExpired) !=
		domain.VerbosityStatusChanges.AllowsReply(domain.ReasonExpired) {
		t.Fatal("`expired` is no longer ungated at every verbosity; the comparison this test " +
			"draws on has moved")
	}
}
