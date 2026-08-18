package domain

// Verbosity is how much a Channel wants, mirroring `channels.verbosity`
// (channels_verbosity_ck).
//
// It gates THREAD REPLIES ONLY. Root updates are NEVER gated by verbosity
// (§H.6): an operator must always be able to trust that the card in front of
// them describes the world as it is now. A quiet channel is a channel with fewer
// replies, never a channel with a stale card.
type Verbosity string

// The verbosity levels.
const (
	// VerbosityAll delivers every reply type.
	VerbosityAll Verbosity = "all"
	// VerbosityStatusChanges is the schema default.
	VerbosityStatusChanges Verbosity = "status_changes"
	// VerbosityFiringAndResolved drops the human-action replies.
	VerbosityFiringAndResolved Verbosity = "firing_and_resolved"
	// VerbosityFiringOnly is the quietest setting.
	VerbosityFiringOnly Verbosity = "firing_only"
)

// Valid reports whether v is one of the four levels.
func (v Verbosity) Valid() bool {
	switch v {
	case VerbosityAll, VerbosityStatusChanges, VerbosityFiringAndResolved, VerbosityFiringOnly:
		return true
	default:
		return false
	}
}

// Normalise maps an empty or unknown value onto the schema default. An unknown
// verbosity must never mean "send nothing": a channel silently going quiet is
// the failure mode §B.6 exists to prevent.
func (v Verbosity) Normalise() Verbosity {
	if v.Valid() {
		return v
	}
	return VerbosityStatusChanges
}

// replySets is the §H.6 "Verbosity semantics" table, literally.
//
// `all` is absent on purpose and is handled by AllowsReply: enumerating it would
// invite the two lists to drift, and "all means all" is the one rule that must
// never need maintenance.
var replySets = map[Verbosity]map[Reason]bool{
	VerbosityStatusChanges: {
		ReasonAcked: true, ReasonUnacked: true, ReasonSuppressed: true,
		ReasonUnsuppressed: true, ReasonExpired: true, ReasonRefired: true,
		ReasonNewAlerts: true, ReasonAllResolved: true, ReasonRuleChanged: true,
		ReasonComment: true, ReasonSnoozed: true, ReasonUnsnoozed: true,
		ReasonUnackedReminder: true,
	},
	VerbosityFiringAndResolved: {
		ReasonNewAlerts: true, ReasonAllResolved: true, ReasonExpired: true,
		ReasonRuleChanged: true, ReasonSnoozed: true, ReasonUnsnoozed: true,
		ReasonUnackedReminder: true,
	},
	VerbosityFiringOnly: {
		// ⛔ `storm` WAS IN ALL FOUR SETS AND IS GONE FROM THE VOCABULARY (reason.go,
		// migration 00060). It survived the quietest setting on the argument that a
		// channel which asked for less has not asked to be lied to about oto
		// withholding things — and oto withholds nothing now, so there is no such
		// fact and no Reason naming one.
		ReasonNewAlerts: true, ReasonRuleChanged: true, ReasonSnoozed: true,
		ReasonUnsnoozed: true, ReasonUnackedReminder: true,
	},
}

// ungatedReplies are the Reasons whose reply is delivered at EVERY verbosity.
//
// §H.6 states these as an explicit per-Reason override in the "Verbosity gate"
// column, and where that column and the semantics table disagree the override
// wins — it is the more deliberate statement and it carries its own argument:
//
//   - expired      "always (an expiry must never be silent)". Losing sight of a
//     signal is the one thing a quiet channel must still hear.
//   - rule_changed "always — never gated". The headline differentiator.
//   - snoozed      "always — exempt from snooze suppression (§B.8.4)".
//   - unsnoozed    same.
//   - unacked_reminder "always". It is the reminder; gating it away deletes it.
//   - comment      a human deliberately spoke into this thread. Swallowing a
//     person's words because the channel is quiet is not a volume
//     setting, it is data loss.
var ungatedReplies = map[Reason]bool{
	ReasonExpired:         true,
	ReasonRuleChanged:     true,
	ReasonSnoozed:         true,
	ReasonUnsnoozed:       true,
	ReasonUnackedReminder: true,
	ReasonComment:         true,
}

// AllowsReply reports whether this verbosity delivers the thread reply for r.
//
// It answers only the volume question. Whether the Reason HAS a reply at all is
// ModeFor's business, and whether the channel can carry one is the capability
// negotiation's.
func (v Verbosity) AllowsReply(r Reason) bool {
	if ungatedReplies[r] {
		return true
	}
	v = v.Normalise()
	if v == VerbosityAll {
		return true
	}
	return replySets[v][r]
}
