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
	//
	// It is a fact about ONE GROUP going quiet, and it stays on that group's
	// thread. The CHANNEL-level "oto has started withholding things" notice is a
	// separate, once-per-channel decision — see broadcast.go, and ADR 0020.
	ReasonStorm Reason = "storm"
)

// ⛔ THERE IS NO `severity_raised`, AND ADDING ONE WOULD BE ADDING AN ENUM VALUE
// NOTHING CAN EVER WRITE.
//
// ADR 0020 originally proposed it as the purest case for broadcasting — a card
// going amber to red under a silent `chat.update`. A migration (00027) was
// written for it and has been deleted. The premise does not survive contact with
// §C.2: `severity` is an ordinary Prometheus LABEL and is hashed into
// `alert_key`, so two severities of one rule are TWO ALERTS, not one Alert
// changing. Nothing observes a rise, so nothing can emit the Reason, and a
// CHECK-constraint value with no writer is a trap for the next person reading
// this file. `test/integration/alert_identity_test.go` is the proof.

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

// ReconcileWithWire applies the §H.6 table to a Reason that was derived from
// oto's OWN per-alert transitions, using Alertmanager's `notification_reason`
// as the group-level authority it is.
//
// ⭐ THIS IS §H.6's ONLY CALLER OF ReasonFromWire, AND IT IS WHERE THE TWO
// VOCABULARIES MEET. They answer different questions and both are needed:
//
//   - oto's transitions know WHAT CHANGED about one alert. They are the only
//     source for `acked`, `refired`, `expired`, `suppressed` — facts Alertmanager
//     cannot see or does not have a word for.
//   - Alertmanager's `notification_reason` knows WHY THIS BATCH WAS DELIVERED
//     about a whole group. It is the only source that can tell a first fire from
//     a member joining a group that was already notified, because the per-alert
//     view of both is identical: an occurrence opened.
//
// Before this existed, `new_alerts`, `all_resolved` and `repeat` were CHECK
// constraint values nothing could ever write — the first live run posted a fully
// resolved card whose footer read "some alerts resolved", which is false.
//
// The reconciliation is deliberately NARROW. The wire value may only widen a
// reason to the group-scoped sibling that describes the same delivery; it may
// never contradict an observed transition, because oto saw that and Alertmanager
// did not. An unknown or absent wire value changes nothing: an Alertmanager
// below 0.32.0 sends no field at all and must not lose its notifications for it.
func ReconcileWithWire(derived Reason, wire string, allResolved bool) Reason {
	mapped, verdict := ReasonFromWire(wire)

	switch derived {
	case ReasonSomeResolved:
		// oto watched ONE alert resolve. Whether that was the LAST one is a fact
		// about the group's membership, which oto projects itself — so the counts
		// decide and the wire value is corroboration, not authority. §H.6 makes the
		// difference load-bearing: `some_resolved` is update-only, `all_resolved`
		// earns a thread reply and may be broadcast.
		if allResolved || (verdict == WireMapped && mapped == ReasonAllResolved) {
			return ReasonAllResolved
		}
		return derived

	case ReasonFired:
		// An occurrence opened. Whether it opened a group or JOINED one that had
		// already been notified is a distinction only Alertmanager can draw:
		// oto sees an identical transition either way, and guessing from the member
		// count would turn three alerts firing in one first batch into three
		// "more instances now firing" replies and no root card at all.
		if verdict == WireMapped && mapped == ReasonNewAlerts {
			return ReasonNewAlerts
		}
		return derived

	default:
		return derived
	}
}
