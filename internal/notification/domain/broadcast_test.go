package domain_test

import (
	"testing"

	"github.com/thulasiram/oto/internal/notification/domain"
)

// The §H.6 broadcast column (ADR 0020). Every case here is a rule the ADR states
// in prose and nothing else enforces.

const allCaps = domain.CapThreading | domain.CapAmend | domain.CapRichLayout |
	domain.CapInteractive | domain.CapBroadcast

func base(r domain.Reason) domain.PlanInput {
	return domain.PlanInput{
		Reason:        r,
		Verbosity:     domain.VerbosityAll,
		ThreadUpdates: true,
		Capabilities:  allCaps,
		ThreadExists:  true,
		Broadcast:     domain.DefaultBroadcastPolicy(),
	}
}

func lastMode(p domain.Plan) domain.Mode {
	if len(p.Modes) == 0 {
		return ""
	}
	return p.Modes[len(p.Modes)-1]
}

// TestTheDefaultSetBroadcasts pins ADR 0020's approved default set, in both
// directions. The "does not broadcast" half is the half that will be argued
// about, so it is asserted rather than assumed.
func TestTheDefaultSetBroadcasts(t *testing.T) {
	t.Parallel()

	// ⭐ THE SET IS TWO. It was four; the ADR was revised. `severity_raised` is
	// gone because it names a transition oto cannot observe, and `storm` is gone
	// three times over — first because a storm is MANY alerts and a per-thread
	// broadcast of it is the flood the damping existed to prevent, then because storm
	// damping itself was removed, and now because migration 00060 DELETES the Reason
	// rather than retiring it. There is no `domain.ReasonStorm` left to assert
	// against; a value that does not compile cannot broadcast.
	// ⛔ `unacked_reminder` WAS THE SECOND ENTRY AND IS GONE (git-bug bd0fb1d), for
	// the same reason `storm` is: the value does not compile, so it cannot
	// broadcast. `refired` is the whole default set now.
	broadcasts := []domain.Reason{
		domain.ReasonRefired,
	}
	for _, r := range broadcasts {
		if got := lastMode(domain.PlanFor(base(r))); got != domain.ModeBroadcastReply {
			t.Errorf("%s: got mode %q, want a broadcast — ADR 0020 puts it in the default set", r, got)
		}
	}

	// Facts about the RESPONSE, addressed to people already in the thread.
	// Broadcasting an ack would double the channel traffic of every well-handled
	// alert, punishing exactly the behaviour oto wants.
	quiet := []domain.Reason{
		domain.ReasonAcked, domain.ReasonComment,
		domain.ReasonEnriched, domain.ReasonSnoozed,
	}
	for _, r := range quiet {
		if got := lastMode(domain.PlanFor(base(r))); got == domain.ModeBroadcastReply {
			t.Errorf("%s: broadcasts, and ADR 0020 says it must not", r)
		}
	}
}

// TestResolvedBroadcastIsConfigurableAndOffByDefault pins the one dial.
func TestResolvedBroadcastIsConfigurableAndOffByDefault(t *testing.T) {
	t.Parallel()

	in := base(domain.ReasonAllResolved)
	if got := lastMode(domain.PlanFor(in)); got == domain.ModeBroadcastReply {
		t.Fatalf("all_resolved broadcasts by default; ADR 0020 ships it OFF")
	}

	in.Broadcast = domain.BroadcastPolicy{Resolved: true}
	if got := lastMode(domain.PlanFor(in)); got != domain.ModeBroadcastReply {
		t.Fatalf("all_resolved with the switch on: got %q, want a broadcast", got)
	}
}

// ⛔⛔ TWO STORM TESTS WERE HERE AND BOTH ARE DELETED WITH THE DAMPER THEY PINNED.
//
// `TestStormDampsEveryBroadcastButTheStormItself` was ADR 0020 constraint 3: during a
// storm exactly ONE broadcast was permitted — the storm announcement, and only from the
// group that held the channel latch — while every other broadcasting transition lost its
// reply entirely and recorded the drop as `storm`. `TestTheUnackedReminderIsDampedNot`
// `DroppedDuringAStorm` pinned the hole that closed: the reminder branch returned an
// UNCONDITIONAL broadcast, so a storm across two hundred unacknowledged alerts produced
// two hundred `chat.postMessage` calls into one channel, and the fix degraded it to a
// quiet thread reply that still landed.
//
// ⭐⭐ BOTH WERE CORRECT ABOUT THE VOLUME AND WRONG ABOUT THE PREMISE. The reason a
// damper needed a constraint stating how loudly it may announce ITSELF is that the thing
// it was damping had no object to be reported on: a storm is many DIFFERENT alerts, the
// object that owns them is an INCIDENT (`correlation`, DEFERRED-POST-V1), and with no
// such object the fact was pushed into the notification layer as "withheld". Storm
// damping is removed, so there is no announcement to ration and no reply to drop.
// `PlanFor` has no storm input at all — see `TestNothingInThePlanIsOtosOwnJudgement`
// below, which is the assertion that replaces both of these.

// TestFlappingNoLongerDampsAnything is the inverse of the test that used to stand
// here, and the inversion is migration 00057.
//
// It asserted "a flapping alert produces a digest, and a digest does not
// broadcast". That damper lived at DELIVERY, where a withheld notification is
// indistinguishable from a signal that never fired (§B.6), and it existed only
// because a flapping alert produced one CASE per flap. The case retention window
// removes the cause: a re-fire inside W lands in the still-open case, so a flap
// produces one root card and one reply, and damping that ONE reply would make the
// flap invisible.
//
// `PlanInput.Flapping` is retired and read by nothing, so this proves the plan is
// IDENTICAL either way — the strongest form of "the damper is gone".
func TestFlappingNoLongerDampsAnything(t *testing.T) {
	t.Parallel()

	for _, reason := range []domain.Reason{
		domain.ReasonRefired, domain.ReasonFired, domain.ReasonSomeResolved,
		domain.ReasonAcked, domain.ReasonComment,
	} {
		calm := domain.PlanFor(base(reason))

		flapping := base(reason)
		flapping.Flapping = true
		damped := domain.PlanFor(flapping)

		if lastMode(calm) != lastMode(damped) {
			t.Fatalf("%s: flapping changed the mode (%q -> %q); the flap damper moved to "+
				"case formation and must not survive here", reason,
				lastMode(calm), lastMode(damped))
		}
		if damped.ReplyDropped != calm.ReplyDropped ||
			damped.ReplyDropReason != calm.ReplyDropReason {
			t.Fatalf("%s: flapping dropped a reply (%v %q); the one reply a flap now "+
				"produces is the only one there is", reason,
				damped.ReplyDropped, damped.ReplyDropReason)
		}
		if damped.BroadcastDamped != calm.BroadcastDamped ||
			damped.BroadcastDampReason != calm.BroadcastDampReason {
			t.Fatalf("%s: flapping damped a broadcast (%v %q)", reason,
				damped.BroadcastDamped, damped.BroadcastDampReason)
		}
	}
}

// TestBroadcastNeverOverridesTheDestinationsOwnVolume is ADR 0020 constraint 1.
//
// Policy decides that a transition WARRANTS a broadcast; the destination decides
// whether it gets one. A channel that has opted out of thread replies does not
// receive louder ones.
func TestBroadcastNeverOverridesTheDestinationsOwnVolume(t *testing.T) {
	t.Parallel()

	in := base(domain.ReasonRefired)
	in.ThreadUpdates = false

	p := domain.PlanFor(in)
	if lastMode(p) == domain.ModeBroadcastReply {
		t.Fatalf("a channel with thread_updates=false received a broadcast")
	}
	if p.ReplyDropReason != "thread_updates" {
		t.Fatalf("drop reason %q, want thread_updates", p.ReplyDropReason)
	}

	// ⛔ THERE IS NO EXCEPTION ANY MORE, AND THAT IS THE ASSERTION. The unacked
	// reminder was the single documented one — always a broadcast, because a
	// reminder nobody sees is not a reminder — and it is gone (git-bug bd0fb1d).
	// `thread_updates=false` now means what it says for EVERY Reason, including the
	// one that still broadcasts by default.
	rem := base(domain.ReasonRefired)
	rem.ThreadUpdates = false
	p2 := domain.PlanFor(rem)
	if lastMode(p2) == domain.ModeBroadcastReply {
		t.Fatal("a broadcasting reason still broadcast with thread_updates=false — the " +
			"reminder's exception outlived the reminder")
	}
	if p2.ReplyDropReason != "thread_updates" {
		t.Fatalf("drop reason %q, want thread_updates", p2.ReplyDropReason)
	}
}

// TestCapBroadcastDegradesForEveryBroadcastingReason. §H.10 degradation used to
// apply only to the reminder, because only the reminder could broadcast.
func TestCapBroadcastDegradesForEveryBroadcastingReason(t *testing.T) {
	t.Parallel()

	noBroadcast := allCaps &^ domain.CapBroadcast
	for _, r := range []domain.Reason{
		// ⛔ `storm` WAS THE THIRD ENTRY AND IS GONE. It only ever wanted a broadcast
		// once it held the channel latch, and there is no latch: nothing withholds, so
		// nothing announces withholding.
		domain.ReasonRefired,
	} {
		in := base(r)
		in.Capabilities = noBroadcast

		p := domain.PlanFor(in)
		if got := lastMode(p); got != domain.ModeThreadReply {
			t.Errorf("%s on a channel that cannot broadcast: got %q, want a plain reply", r, got)
		}
		if !p.BroadcastDamped || p.BroadcastDampReason != "no_capability" {
			t.Errorf("%s: degradation not recorded (damped=%v, %q)", r, p.BroadcastDamped, p.BroadcastDampReason)
		}
	}

	// ⛔ THE NO-THREADING FALLBACK WAS THE REMINDER'S OWN BRANCH and went with it
	// (git-bug bd0fb1d): it fell all the way back to a root update, because a
	// reminder had to stay loud. Nothing has that privilege now — a broadcasting
	// reason on a channel that cannot thread takes the ordinary §H.10 path.
}

// TestChannelPolicyCodesNeverKillAThread is the other half of the Slack
// classification fix, asserted in the module that owns the closed set.
//
// `restricted_action*` are permanent, but the CONVERSATION is alive and the
// credential is good. If any of them reached this set, `DispatchService.fail`
// would mark the thread dead and degrade the Channel over a channel preference an
// administrator may reverse tomorrow — losing the thread's history for nothing.
func TestChannelPolicyCodesNeverKillAThread(t *testing.T) {
	t.Parallel()

	for _, code := range []string{
		"restricted_action_non_threadable_channel",
		"restricted_action_thread_only_channel",
		"restricted_action_read_only_channel",
		"restricted_action",
		"msg_blocks_too_long",
	} {
		if _, isThreadError := domain.DeadReasonFor(code); isThreadError {
			t.Errorf("%s is a thread-killing dead_reason; it must not be", code)
		}
	}

	// The genuine thread error next to it still is one, so the test above is not
	// passing merely because DeadReasonFor recognises nothing.
	if _, ok := domain.DeadReasonFor("restricted_action_thread_locked"); !ok {
		t.Fatal("restricted_action_thread_locked stopped being a thread error")
	}
}

// ⛔ TestSeverityRaisedIsNotAReason. It was one, briefly, with a migration behind
// it. It named a transition oto cannot observe: `severity` is a Prometheus LABEL
// and is hashed into `alert_key` (§C.2), so two severities of one rule are two
// ALERTS with two identities — no row is ever `warning` and later `critical`.
//
// This test is the tombstone. `test/integration/alert_identity_test.go` proves
// the premise against a real database; this one stops the enum value coming back
// by habit.
func TestSeverityRaisedIsNotAReason(t *testing.T) {
	t.Parallel()

	if domain.Reason("severity_raised").Valid() {
		t.Fatal("severity_raised is back in the closed Reason set. " +
			"It cannot fire: severity is part of alert identity, so a severity rise " +
			"is a NEW alert, not a change to an existing one (ADR 0020)")
	}
	for _, r := range domain.AllReasons() {
		if r == "severity_raised" {
			t.Fatal("severity_raised is back in AllReasons")
		}
	}
}

// ⛔⛔ `TestTheChannelStormNoticeCannotRepeatPerAlert` WAS HERE AND IS DELETED. Twenty
// groups collapsed, one held `channels.storm_notice_at`, and the test asserted exactly
// ONE channel-level broadcast came out — plus that holding the latch bought nothing for
// any other Reason, so a future Reason could not acquire a channel-wide post by setting
// one flag. Both properties are now vacuous: the latch, the flag and the notice are all
// deleted with storm damping.

// TestNothingInThePlanIsOtosOwnJudgement is what the deleted storm tests are replaced
// by, and it is the ticket's title as an assertion.
//
// ⭐⭐ THE DEFECT WAS NEVER "TOO MANY BROADCASTS", IT WAS THAT OTO DECIDED. A suppressed
// notification and a signal that never fired look identical to an operator, so the only
// safe rule is that oto never withholds on its OWN judgement about a signal: every drop
// must be traceable to a human's setting, a channel switch, a missing provider
// capability, or a root card that already carries the fact. `storm` and `flapping` were
// the two that broke the rule and both are gone.
//
// ⛔ THE TEST IS OVER THE WHOLE REASON SET AND THE WHOLE LABEL SPACE, deliberately: a
// third damper would arrive as a new drop reason, and this is what refuses it.
func TestNothingInThePlanIsOtosOwnJudgement(t *testing.T) {
	t.Parallel()

	// The only labels a plan may give for dropping a reply or quietening a broadcast.
	// Each names something OUTSIDE oto's opinion of the signal.
	allowedDrop := map[string]bool{
		"verbosity":      true, // a human set this channel's volume
		"thread_updates": true, // a human switched replies off for this channel
		"no_threading":   true, // the provider cannot thread
		"fresh_root":     true, // the card being posted already says it
	}
	allowedDamp := map[string]bool{
		"no_capability": true, // the provider cannot broadcast
	}

	// Every capability/switch/verbosity combination, for every Reason.
	caps := []domain.Capability{allCaps, allCaps &^ domain.CapBroadcast, allCaps &^ domain.CapThreading,
		allCaps &^ domain.CapAmend, domain.CapAmend, 0}
	verbosities := []domain.Verbosity{domain.VerbosityAll, domain.VerbosityStatusChanges,
		domain.VerbosityFiringAndResolved, domain.VerbosityFiringOnly}

	for _, r := range domain.AllReasons() {
		for _, c := range caps {
			for _, v := range verbosities {
				for _, updates := range []bool{true, false} {
					for _, exists := range []bool{true, false} {
						in := base(r)
						in.Capabilities, in.Verbosity = c, v
						in.ThreadUpdates, in.ThreadExists = updates, exists
						p := domain.PlanFor(in)
						if p.ReplyDropped && !allowedDrop[p.ReplyDropReason] {
							t.Fatalf("%s dropped a reply for %q — that is not a fact about the "+
								"destination, so it is oto deciding a firing is not worth "+
								"mentioning, which §B.6 forbids", r, p.ReplyDropReason)
						}
						if p.BroadcastDamped && !allowedDamp[p.BroadcastDampReason] {
							t.Fatalf("%s damped a broadcast for %q — a damper oto chooses for "+
								"itself is how a withheld notification becomes "+
								"indistinguishable from a signal that never fired", r,
								p.BroadcastDampReason)
						}
					}
				}
			}
		}
	}
}
