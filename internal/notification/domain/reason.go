package domain

// Reason is WHY oto is communicating — the §H.6 enum, and the closed vocabulary
// of `notifications.reason` (notifications_reason_ck, as narrowed by migration
// 00018).
//
// A Reason is a fact about a SIGNAL, never a routing decision about a human. It
// is the single input that decides update-in-place versus thread reply, which is
// why it is a closed set and not a free string.
type Reason string

// The closed Reason set, as narrowed by migration 00018. The scope-banned word
// migration 00018 removed is gone from this vocabulary and from this file;
// `unacked_reminder` took its place, because oto has ONE reminder stage,
// forever, and it reminds a CHANNEL rather than a person (§G.9.1).
const (
	// ReasonFired is Alertmanager's `first notification`.
	ReasonFired Reason = "fired"
	// ReasonNewAlerts is `new alerts added`.
	ReasonNewAlerts Reason = "new_alerts"
	// ReasonSomeResolved is `some alerts resolved`.
	ReasonSomeResolved Reason = "some_resolved"
	// ReasonAllResolved is `all alerts resolved`.
	ReasonAllResolved Reason = "all_resolved"
	// ReasonRepeat is `repeat interval elapsed`. IT UPDATES AND NEVER REPOSTS.
	ReasonRepeat Reason = "repeat"
	// ReasonSuppressed is the reconciler seeing Alertmanager suppress the signal.
	ReasonSuppressed Reason = "suppressed"
	// ReasonUnsuppressed is Alertmanager delivering again.
	ReasonUnsuppressed Reason = "unsuppressed"
	// ReasonExpired is oto losing sight of the signal. NEVER a resolution.
	ReasonExpired Reason = "expired"
	// ReasonRefired is a re-fire inside the grace window.
	ReasonRefired Reason = "refired"
	// ReasonAcked is a human acknowledging.
	ReasonAcked Reason = "acked"
	// ReasonUnacked is an acknowledgement being withdrawn or invalidated.
	ReasonUnacked Reason = "unacked"
	// ReasonSnoozed announces a snooze beginning. Exempt from snooze suppression.
	ReasonSnoozed Reason = "snoozed"
	// ReasonUnsnoozed announces a snooze ending. Exempt from snooze suppression.
	ReasonUnsnoozed Reason = "unsnoozed"
	// ReasonEnriched is a late enrichment landing.
	ReasonEnriched Reason = "enriched"
	// ReasonRuleChanged is the headline differentiator: the alerting rule's
	// definition changed since the previous occurrence.
	ReasonRuleChanged Reason = "rule_changed"
	// ReasonComment is a human speaking into the thread.
	ReasonComment Reason = "comment"
	// ReasonUnackedReminder is oto's ONE reminder stage (§G.9.1). It is a fact
	// about how long the SIGNAL has gone unacknowledged, delivered to the channels
	// the policy already routes to. It is not a ladder and has no second stage.
	ReasonUnackedReminder Reason = "unacked_reminder"
	// ReasonStorm announces storm damping engaging. A VISIBLE state, never a
	// silent suppression.
	ReasonStorm Reason = "storm"
)

// allReasons is the closed set, in the order migration 00018 declares it.
var allReasons = []Reason{
	ReasonFired, ReasonNewAlerts, ReasonSomeResolved, ReasonAllResolved,
	ReasonRepeat, ReasonSuppressed, ReasonUnsuppressed, ReasonExpired,
	ReasonRefired, ReasonAcked, ReasonUnacked, ReasonSnoozed, ReasonUnsnoozed,
	ReasonEnriched, ReasonRuleChanged, ReasonComment, ReasonUnackedReminder,
	ReasonStorm,
}

// AllReasons returns the closed Reason set. The slice is freshly built so a
// caller cannot mutate the vocabulary.
func AllReasons() []Reason {
	out := make([]Reason, len(allReasons))
	copy(out, allReasons)
	return out
}

// Valid reports whether r is in the closed set. A Reason that fails this would
// be rejected by notifications_reason_ck anyway; failing here turns a 23514 into
// a validation error with a field name.
func (r Reason) Valid() bool {
	for _, k := range allReasons {
		if k == r {
			return true
		}
	}
	return false
}

// String renders the Reason as stored.
func (r Reason) String() string { return string(r) }

// AlertScoped reports whether this Reason is a fact about ONE Alert and
// therefore REQUIRES an alert_id (notifications_focus_ck).
func (r Reason) AlertScoped() bool {
	switch r {
	case ReasonAcked, ReasonUnacked, ReasonRefired, ReasonRuleChanged:
		return true
	default:
		return false
	}
}

// SnoozeExempt reports whether a snooze may NOT suppress this Reason (§B.8.4).
//
// Exactly two Reasons are exempt, and they are necessary: a snooze that cannot
// announce its own beginning and end is the silent suppression §B.6 forbids.
func (r Reason) SnoozeExempt() bool {
	return r == ReasonSnoozed || r == ReasonUnsnoozed
}

// WireVerdict is what an Alertmanager `notification_reason` resolved to.
type WireVerdict int

// The closed WireVerdict set (§H.6).
const (
	// WireUnmapped means the wire value is not one oto recognises.
	WireUnmapped WireVerdict = iota
	// WireMapped means the wire value named a Reason directly.
	WireMapped
	// WireSuppress means Alertmanager said `none`: record a suppressed
	// Notification and stop. It is still RECORDED — never a silent drop.
	WireSuppress
	// WireDiffFallback means the payload predates AM 0.32.0 (or said `unknown`),
	// so the Reason must be derived by diffing the incoming fingerprint set
	// against alert_group_members.
	WireDiffFallback
)

// Alertmanager's wire `notification_reason` values (AM >= 0.32.0), verbatim.
const (
	wireFirstNotification = "first notification"
	wireNewAlertsAdded    = "new alerts added"
	wireSomeResolved      = "some alerts resolved"
	wireAllResolved       = "all alerts resolved"
	wireRepeatInterval    = "repeat interval elapsed"
	wireNone              = "none"
	wireUnknown           = "unknown"
)

// ReasonFromWire maps Alertmanager's `notification_reason` onto an oto Reason
// (§H.6, BINDING).
//
// The empty string is NOT an error: Alertmanager below 0.32.0 does not send the
// field at all, and treating "absent" as "broken" would drop every notification
// from an older cluster. It falls back to the fingerprint-set diff.
func ReasonFromWire(wire string) (Reason, WireVerdict) {
	switch wire {
	case wireFirstNotification:
		return ReasonFired, WireMapped
	case wireNewAlertsAdded:
		return ReasonNewAlerts, WireMapped
	case wireSomeResolved:
		return ReasonSomeResolved, WireMapped
	case wireAllResolved:
		return ReasonAllResolved, WireMapped
	case wireRepeatInterval:
		return ReasonRepeat, WireMapped
	case wireNone:
		// "none" is Alertmanager telling us it had nothing to say. oto records the
		// intent as suppressed and stops; it does not invent a Reason.
		return "", WireSuppress
	case "", wireUnknown:
		return "", WireDiffFallback
	default:
		return "", WireUnmapped
	}
}
