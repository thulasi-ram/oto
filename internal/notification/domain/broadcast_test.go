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
	// because a storm is MANY alerts and a per-thread broadcast of it is the flood
	// the damping exists to prevent.
	broadcasts := []domain.Reason{
		domain.ReasonRefired,
		domain.ReasonUnackedReminder,
	}
	for _, r := range broadcasts {
		if got := lastMode(domain.PlanFor(base(r))); got != domain.ModeBroadcastReply {
			t.Errorf("%s: got mode %q, want a broadcast — ADR 0020 puts it in the default set", r, got)
		}
	}

	// ⛔ `storm` DOES NOT BROADCAST PER GROUP. Its channel-level notice is a
	// separate, latched decision, and without the latch it is a plain reply on the
	// group's own thread.
	if got := lastMode(domain.PlanFor(base(domain.ReasonStorm))); got == domain.ModeBroadcastReply {
		t.Error("storm broadcast per group; ADR 0020 moved it to a once-per-channel notice")
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

// TestStormDampsEveryBroadcastButTheStormItself is ADR 0020 constraint 3, and it
// is the property most likely to be broken by a later change.
//
// A broadcast is a `chat.postMessage` against the ~1 message/second/channel
// budget; an update is not. "One broadcast per interesting transition" during the
// exact event that generates hundreds of them is a self-inflicted flood — oto
// shouting, once per alert, about the fact that it has started being quiet.
func TestStormDampsEveryBroadcastButTheStormItself(t *testing.T) {
	t.Parallel()

	// The storm announcement survives and broadcasts: the collapse must be
	// visible, or the silence that follows is indistinguishable from nothing
	// happening.
	storm := base(domain.ReasonStorm)
	storm.StormMode = true
	storm.ChannelNoticeClaimed = true
	if got := lastMode(domain.PlanFor(storm)); got != domain.ModeBroadcastReply {
		t.Fatalf("the storm announcement itself: got %q, want a broadcast", got)
	}

	// ⭐ AND THE SECOND GROUP TO STORM DOES NOT. It lost the channel latch, so its
	// reply stays on its own thread. This is the line that turns twenty
	// simultaneous group collapses into one channel message.
	second := base(domain.ReasonStorm)
	second.StormMode = true
	second.ChannelNoticeClaimed = false
	if got := lastMode(domain.PlanFor(second)); got != domain.ModeThreadReply {
		t.Fatalf("a storm that lost the channel latch: got %q, want a quiet thread reply", got)
	}

	// Every other broadcasting transition loses its reply entirely during a
	// storm, which necessarily loses its broadcast.
	for _, r := range []domain.Reason{domain.ReasonRefired} {
		in := base(r)
		in.StormMode = true
		p := domain.PlanFor(in)
		if lastMode(p) == domain.ModeBroadcastReply {
			t.Errorf("%s broadcasts during a storm; ADR 0020 permits exactly one", r)
		}
		if !p.ReplyDropped || p.ReplyDropReason != "storm" {
			t.Errorf("%s during a storm: dropped=%v reason=%q, want a recorded storm drop",
				r, p.ReplyDropped, p.ReplyDropReason)
		}
	}
}

// TestTheUnackedReminderIsDampedNotDroppedDuringAStorm pins the hole this work
// closed.
//
// The reminder branch runs BEFORE the ordinary gates, because `thread_updates`
// may not silence a reminder — and it used to return an UNCONDITIONAL broadcast.
// A storm across two hundred unacknowledged alerts therefore produced two hundred
// `chat.postMessage` calls into one channel. The reminder must still land, so it
// degrades to a quiet thread reply rather than disappearing.
func TestTheUnackedReminderIsDampedNotDroppedDuringAStorm(t *testing.T) {
	t.Parallel()

	in := base(domain.ReasonUnackedReminder)
	in.StormMode = true

	p := domain.PlanFor(in)
	if got := lastMode(p); got != domain.ModeThreadReply {
		t.Fatalf("reminder during a storm: got %q, want a quiet thread reply", got)
	}
	if !p.BroadcastDamped || p.BroadcastDampReason != "storm" {
		t.Fatalf("the damping was not recorded: damped=%v reason=%q — a damper that cannot "+
			"account for its own quiet is the silent suppression §B.6 forbids",
			p.BroadcastDamped, p.BroadcastDampReason)
	}
}

// TestFlappingDampsBroadcastToo — a flapping alert produces a digest, and a
// digest does not broadcast.
func TestFlappingDampsBroadcastToo(t *testing.T) {
	t.Parallel()

	in := base(domain.ReasonUnackedReminder)
	in.Flapping = true

	p := domain.PlanFor(in)
	if got := lastMode(p); got != domain.ModeThreadReply {
		t.Fatalf("reminder while flapping: got %q, want a quiet thread reply", got)
	}
	if p.BroadcastDampReason != "flapping" {
		t.Fatalf("damp reason %q, want flapping", p.BroadcastDampReason)
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

	// The reminder is the single documented exception, and it survives.
	rem := base(domain.ReasonUnackedReminder)
	rem.ThreadUpdates = false
	if got := lastMode(domain.PlanFor(rem)); got != domain.ModeBroadcastReply {
		t.Fatalf("reminder with thread_updates=false: got %q — a reminder nobody sees is not a reminder", got)
	}
}

// TestCapBroadcastDegradesForEveryBroadcastingReason. §H.10 degradation used to
// apply only to the reminder, because only the reminder could broadcast.
func TestCapBroadcastDegradesForEveryBroadcastingReason(t *testing.T) {
	t.Parallel()

	noBroadcast := allCaps &^ domain.CapBroadcast
	for _, r := range []domain.Reason{
		domain.ReasonRefired, domain.ReasonStorm, domain.ReasonUnackedReminder,
	} {
		in := base(r)
		in.Capabilities = noBroadcast
		// The storm only wants a broadcast at all once it holds the channel latch.
		in.ChannelNoticeClaimed = true

		p := domain.PlanFor(in)
		if got := lastMode(p); got != domain.ModeThreadReply {
			t.Errorf("%s on a channel that cannot broadcast: got %q, want a plain reply", r, got)
		}
		if !p.BroadcastDamped || p.BroadcastDampReason != "no_capability" {
			t.Errorf("%s: degradation not recorded (damped=%v, %q)", r, p.BroadcastDamped, p.BroadcastDampReason)
		}
	}

	// With no threading either, the reminder falls all the way back to a root
	// update — loud enough to still be a reminder.
	rem := base(domain.ReasonUnackedReminder)
	rem.Capabilities = domain.CapAmend
	if got := lastMode(domain.PlanFor(rem)); got != domain.ModeUpdateRoot {
		t.Fatalf("reminder with no threading: got %q, want update_root", got)
	}
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

// TestTheChannelStormNoticeCannotRepeatPerAlert is the property the storm
// relocation exists for.
//
// A storm is MANY alerts across MANY groups, all landing in the same channel. The
// only thing that stops the "oto has gone quiet" announcement from becoming the
// flood it is announcing is that exactly one of them holds the channel latch.
func TestTheChannelStormNoticeCannotRepeatPerAlert(t *testing.T) {
	t.Parallel()

	// Twenty groups collapse. One won the latch; nineteen did not.
	broadcasts := 0
	for i := range 20 {
		in := base(domain.ReasonStorm)
		in.StormMode = true
		in.ChannelNoticeClaimed = i == 0
		if lastMode(domain.PlanFor(in)) == domain.ModeBroadcastReply {
			broadcasts++
		}
	}
	if broadcasts != 1 {
		t.Fatalf("%d channel-level storm broadcasts for 20 storming groups, want exactly 1", broadcasts)
	}

	// And the notice is not something an ordinary Reason can claim: holding the
	// latch does nothing for anything but `storm`, so a future Reason cannot
	// acquire a channel-wide post by setting one flag.
	for _, r := range []domain.Reason{domain.ReasonAcked, domain.ReasonComment, domain.ReasonNewAlerts} {
		in := base(r)
		in.ChannelNoticeClaimed = true
		if lastMode(domain.PlanFor(in)) == domain.ModeBroadcastReply {
			t.Errorf("%s broadcast by holding the channel storm latch", r)
		}
	}
}
