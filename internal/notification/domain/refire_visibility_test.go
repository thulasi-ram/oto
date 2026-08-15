package domain_test

import (
	"testing"

	"github.com/thulasiram/oto/internal/notification/domain"
)

// ⛔⛔ THIS FILE PINS A KNOWN CONTRADICTION, NOT A DESIRED BEHAVIOUR.
//
// ADR 0020 grants exactly two Reasons an irreversible `reply_broadcast`, and
// `refired` is one of them, on this reasoning: a re-fire INSIDE `refire_grace`
// reopens the existing occurrence, so it produces an update and a thread reply and
// NO new root message — *"the thread said resolved and people stopped following
// it"*. Its quiet form is invisible, which is the only property that earns a
// channel post.
//
// §H.6's verbosity table then drops the `refired` reply entirely at
// `firing_and_resolved` and `firing_only`, and `refired` is NOT in
// `ungatedReplies`. So on a channel set quieter than the default `status_changes`,
// a re-fire inside the grace is **completely silent** — the one case ADR 0020 said
// must never be.
//
// ADR 0026 found this while deriving `refire_grace` (it is the reason the derived
// default is the SMALLEST value that reaches the modal rule rather than the
// largest that covers every rule — the longer the grace, the more re-fires fall
// into this hole) and deliberately did NOT fix it: resolving it means deciding
// whether `firing_only` may delete a transition ADR 0020 called unmissable, which
// is a product decision about what a documented verbosity level MEANS, not a
// tuning one. It touches SPEC §H.6, ADR 0020, the OpenAPI schema and the renderer.
//
// The tests below assert what ships TODAY. When the owner decides, one of them
// will fail, which is the point: the defect is visible in the test suite rather
// than discovered during an incident.

// The half that is correct: at the shipped default verbosity, a re-fire inside the
// grace both survives the gate and warrants a broadcast.
func TestARefireIsBroadcastAtTheDefaultVerbosity(t *testing.T) {
	t.Parallel()

	// The schema default, and what `Normalise` falls back to.
	v := domain.VerbosityStatusChanges
	if !v.AllowsReply(domain.ReasonRefired) {
		t.Fatalf("verbosity %q drops the `refired` reply. It is the one re-fire case that "+
			"produces no new root message, so dropping it makes a genuine re-fire silent at "+
			"oto's OWN DEFAULT — a missed page out of the box", v)
	}
	if !domain.DefaultBroadcastPolicy().Warrants(domain.ReasonRefired) {
		t.Fatal("`refired` no longer warrants a broadcast; ADR 0020 puts it in the set of two " +
			"because its quiet form is invisible, and ADR 0026's refire_grace derivation leans " +
			"on that being true")
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
