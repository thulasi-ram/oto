package domain

// Reason is WHY oto is communicating — the §H.6 enum, and the closed vocabulary
// of `notifications.reason` (notifications_reason_ck, as narrowed by migration
// 00018).
//
// A Reason is a fact about a SIGNAL, never a routing decision about a human. It
// is the single input that decides update-in-place versus thread reply, which is
// why it is a closed set and not a free string.
//
// It also decides WHAT the fact is about: every Reason declares a SubjectKind
// (`reasonSubjects` below), because a notification whose subject is fixed cannot
// address the thing it reports on.
type Reason string

// The closed Reason set, as narrowed by migration 00018 and again by migration
// 00060. The scope-banned word migration 00018 removed is gone from this
// vocabulary and from this file; `unacked_reminder` took its place, because oto
// has ONE reminder stage, forever, and it reminds a CHANNEL rather than a person
// (§G.9.1).
//
// ⛔ `storm` IS GONE FROM IT, DELETED RATHER THAN RETIRED. It announced storm
// damping engaging — one group going quiet, plus a once-per-channel notice that
// oto had started withholding — and storm damping is removed outright (ADR 0042):
// a storm is many DIFFERENT alerts arriving together, the thing that owns many
// different alerts is an INCIDENT (`correlation`, DEFERRED-POST-V1), and a defence
// built before its object had nowhere to put what it detected, so it put it at
// delivery where a withheld notification is indistinguishable from a signal that
// never fired (§B.6). It was briefly kept as a RETIRED value so a decoder meeting
// an older row could still render it; migration 00060 narrows
// `notifications_reason_ck` and the maintainer has authorised the database reset
// that answers it, so no such row and no such binary survives. There is nothing
// left to decode, and a vocabulary entry with no reader is a value the next person
// has to rule out.
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
	//
	// ⛔ NOTHING PRODUCES IT ANY MORE (ADR 0040). It was T8's reason — the edge
	// that reopened a closed episode inside `refire_grace` — and a re-fire now
	// always opens a new episode, which is `ReasonFired`. The value stays declared
	// and stays in `notifications_reason_ck` because rows already carry it, a
	// policy may already match on it, and the renderer still has to draw one.
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
	// definition changed since the previous case.
	ReasonRuleChanged Reason = "rule_changed"
	// ReasonComment is a human speaking into the thread.
	ReasonComment Reason = "comment"
	// ReasonUnackedReminder is oto's ONE reminder stage (§G.9.1). It is a fact
	// about how long the SIGNAL has gone unacknowledged, delivered to the channels
	// the policy already routes to. It is not a ladder and has no second stage.
	ReasonUnackedReminder Reason = "unacked_reminder"
	// ReasonDigest is the eighteenth Reason and the only one whose subject is not
	// an object: it is a WINDOW OVER A NAMESPACE (migration 00058).
	//
	// ⭐ IT IS THE ONE REASON NO TRANSITION PRODUCES. The other seventeen are facts
	// about a change to one thing, and something observed each of them happening. A
	// digest is minted by a TICK: at the top of a policy's window the evaluator
	// counts the Cases that opened inside it, and if the count clears the policy's
	// floor it says so once. Nothing "happens" to trigger it, which is exactly why
	// the question it answers had no subject before this Reason existed.
	//
	// ⛔ IT IS NOT A SECOND THROTTLE AND NOT A DAMPER. `throttle` suppresses a fact
	// oto would otherwise have sent, and records that it did (§B.6) — it is the
	// world's rate limit rather than oto's opinion, which is why it survived the
	// removal of the two dampers that were oto's opinion (see suppression.go). A
	// digest suppresses nothing: a policy that carries a window sends the
	// digest IN ADDITION to whatever else it routes, and an alert-based or
	// case-based policy gains no window at all. oto does not decide to be quiet
	// about a firing.
	ReasonDigest Reason = "digest"
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
	// `digest` is APPENDED, and the position is a contract rather than a
	// preference: `test/contract/dto_schema_test.go` holds this slice to the
	// `NotificationReason` enum in `api/openapi/openapi.yaml` as the same set IN
	// THE SAME ORDER, and migration 00058 appends the value to
	// notifications_reason_ck the same way. Inserting it anywhere else means
	// re-ordering a published enum for nothing.
	ReasonDigest,
}

// AllReasons returns the closed Reason set. The slice is freshly built so a
// caller cannot mutate the vocabulary.
func AllReasons() []Reason {
	out := make([]Reason, len(allReasons))
	copy(out, allReasons)
	return out
}

// The two subjects that were not expressible before migration 00056. They are
// declared HERE, next to the allocation that gives them meaning, rather than
// beside SubjectAlertGroup in idempotency.go: a SubjectKind is only ever chosen by
// a Reason, and a kind nothing allocates is the same dead enum value the
// `severity_raised` note above refuses.
const (
	// SubjectAlert is the Alert IDENTITY — a fact true of the label set across
	// every firing it has ever had. `alerts.suppression_reason`, `alert_snoozes`
	// and the comment timeline all live at this altitude (ADR 0041).
	SubjectAlert SubjectKind = "alert"
	// SubjectCase is ONE FIRING EPISODE. A fact that is only true while this
	// firing lasts belongs here, and `alert_cases.ack_state` is the reason the
	// kind had to exist (migration 00049).
	SubjectCase SubjectKind = "case"
	// SubjectDigest is A WINDOW OVER A NAMESPACE — the pair
	// (`policy_id`, `digest_window_start`), which migration 00058 added.
	//
	// ⭐ IT IS THE FIRST SUBJECT THAT IS NOT A ROW IN THE SIGNAL GRAPH, and that is
	// the whole point of the ticket that added it. The other three are an identity,
	// a firing and a generation; this one is a SET SELECTED BY PROPERTIES OVER TIME,
	// and it is what makes "what happened in this namespace in the last ten minutes"
	// expressible at all. `subject_id` carries the POLICY half, because one UUID
	// column cannot hold a pair and hashing the pair into a synthetic id would make
	// `subject_id` resolve against no table — the exact defect 00056 removed.
	//
	// ⛔ IT IS ALSO THE ONE SUBJECT WITH NO `group_id`. A digest spans many
	// generations, so `notifications.group_id` is NULL for it and
	// `notifications_target_ck` admits that for this kind alone. Read `Subject`
	// below before assuming a Notification has a group.
	SubjectDigest SubjectKind = "digest"
)

// reasonSubjects is the TOTAL Reason → SubjectKind allocation: what each fact is
// ABOUT, as distinct from `notifications.group_id`, which is only where it is
// DELIVERED. It is the closed vocabulary of `notifications.subject_kind`
// (notifications_subjkind_ck, widened by migration 00056).
//
// ⭐ IT IS ALSO THE MEMBERSHIP TEST FOR THE ENUM, AND THAT IS DELIBERATE. `Valid`
// consults this map, so a twentieth Reason cannot be added without deciding what
// it is about: a Reason declared with no subject fails validation at the door
// instead of quietly inheriting the group, which is exactly how every fact came to
// claim the group of forty in the first place. `allReasons` above is the ORDER
// (migration 00018 declares it, 00058 appends to it); this map is the SET.
//
// The four altitudes, and why each Reason sits where it does:
//
//   - alert_group — a fact about the GENERATION. `fired` is here and not on the
//     Case even though a Case opens: the idempotency key is
//     (org, subject, reason, state_version), so a group-subject `fired` is ONE
//     root card per generation-version, while a case-subject one would mint a
//     `fired` per member and post a root card per alert — the failure
//     ReconcileWithWire already documents. `new_alerts`, `some_resolved` and
//     `all_resolved` are membership arithmetic over the generation. `repeat` is
//     Alertmanager's group-level `repeat interval elapsed`. `unacked_reminder` is a
//     fact about the generation and is LATCHED as one: the reminder fires at most
//     once per generation because `unackedGroupsSQL` looks for a prior
//     notification at (subject_kind='alert_group', subject_id=group), so moving
//     its subject would silently unlatch it.
//
//   - case — a fact that is only true of THIS FIRING. `acked` and `unacked` are
//     here because ack is a Case verb: 00049 moved `ack_state` off the Alert
//     precisely because a claim projected onto the identity outlived the firing it
//     was about. `expired` is transition T6 ending one episode. `refired` named
//     the reopening of one episode (nothing writes it since ADR 0040, and the
//     retired value keeps the meaning it had).
//
//   - alert — a fact about the IDENTITY, true whether or not anything is firing.
//     `suppressed` / `unsuppressed` report `alerts.suppression_reason`, ADR 0041's
//     live delivery axis on the Alert. `snoozed` / `unsnoozed` report oto's own
//     quiet, which is keyed by `alert_key` and suppresses every reason for that
//     key (snooze.go). `comment` is an annotation on the ALERT's timeline: it is
//     deduped as `comment:{alert_id}:{ts}` and is appended even when no episode is
//     open, so a Case is not something it can be guaranteed to have.
//
//   - digest — a fact about a WINDOW OVER A NAMESPACE, and the only altitude that
//     is not a row in the signal graph. `digest` is here alone and always will be:
//     the kind exists because the question has no object, so a second Reason at
//     this altitude would be a second way to ask the same thing. It is also the
//     only Reason whose row has no `group_id` (migration 00058,
//     notifications_target_ck), which is why anything reading `GroupID` off a
//     Notification has to ask this map first.
//
// ⭐ `enriched` IS A CASE FACT. The enrichment pipeline coalesces one
// `NotifyEnriched` per RUN, and a run is loaded against one episode — the notice
// carries `GroupID`, `AlertID` and `CaseID` together and the enrichers are the
// ones that completed for that firing. Group-subject would be actively wrong, not
// merely coarse: two alerts enriched at the same group `state_version` would
// collide on `notifications_idem_uniq` and the second alert's context would be
// swallowed, so the card would amend with one alert's evidence and silently drop
// the other's.
//
// ⭐ `rule_changed` IS A CASE FACT TOO, and it is the closer call. The DRIFT is
// about the Alert's rule, which argues `alert`. What is being reported is not "the
// rule is different" but "the definition under which THIS firing happened differs
// from the one the previous firing ran under" — a comparison that has no meaning
// except as a property of one episode. `rules/service` says so in its own keys: it
// captures a snapshot per Case and emits `rule.definition_changed` deduped as
// (rule_changed, CaseID, fingerprint), so the fact is already once-per-firing
// before it reaches this map. An alert-subject `rule_changed` would be one intent
// for a drift that recurs at every fire.
var reasonSubjects = map[Reason]SubjectKind{
	ReasonFired:           SubjectAlertGroup,
	ReasonNewAlerts:       SubjectAlertGroup,
	ReasonSomeResolved:    SubjectAlertGroup,
	ReasonAllResolved:     SubjectAlertGroup,
	ReasonRepeat:          SubjectAlertGroup,
	ReasonUnackedReminder: SubjectAlertGroup,

	ReasonAcked:       SubjectCase,
	ReasonUnacked:     SubjectCase,
	ReasonExpired:     SubjectCase,
	ReasonRefired:     SubjectCase,
	ReasonEnriched:    SubjectCase,
	ReasonRuleChanged: SubjectCase,

	ReasonSuppressed:   SubjectAlert,
	ReasonUnsuppressed: SubjectAlert,
	ReasonSnoozed:      SubjectAlert,
	ReasonUnsnoozed:    SubjectAlert,
	ReasonComment:      SubjectAlert,

	ReasonDigest: SubjectDigest,
}

// Subject is what a Notification carrying this Reason is ABOUT — the value its
// `subject_kind` declares, and therefore which of `alert_id`, `case_id` and
// `group_id` its `subject_id` must equal (notifications_subject_ck).
//
// ⛔ IT IS NOT WHERE THE FACT IS DELIVERED. `group_id` is the delivery target for
// the eighteen signal Reasons and is mandatory for all eighteen
// (notifications_target_ck); the thread is keyed by the AlertGroup generation
// whatever this returns, so forty alerts still produce one thread.
//
// ⚠️ `digest` IS THE ONE EXCEPTION AND IT IS THE REASON THE COLUMN IS NULLABLE. A
// digest spans many generations, so it has no group to be delivered to and opens
// its own conversation keyed by its policy. Code that reads `GroupID` off a
// Notification must handle the zero value for this Reason.
//
// The empty SubjectKind is returned only for a Reason that is not a Reason, which
// `Valid` has already refused.
func (r Reason) Subject() SubjectKind { return reasonSubjects[r] }

// Valid reports whether r is in the closed set. A Reason that fails this would
// be rejected by notifications_reason_ck anyway; failing here turns a 23514 into
// a validation error with a field name.
//
// It asks `reasonSubjects` rather than `allReasons` so that membership and the
// subject allocation cannot drift apart: a Reason with no declared subject has no
// honest row to write and is refused here.
func (r Reason) Valid() bool {
	_, ok := reasonSubjects[r]
	return ok
}

// String renders the Reason as stored.
func (r Reason) String() string { return string(r) }

// AlertScoped reports whether this Reason is a fact about ONE Alert and
// therefore REQUIRES an alert_id (notifications_focus_ck).
//
// ⛔ IT IS NOT `Subject() == SubjectAlert`, AND CONFUSING THE TWO IS A TRAP. This
// answers "must the row NAME an alert"; `Subject` answers "what is the row ABOUT".
// `acked` is alert-scoped AND case-subject — the receipt names the alert it was
// filed against and is a fact about the firing it was filed on — while `comment`
// is alert-SUBJECT and not alert-scoped, because `notifications_focus_ck` predates
// migration 00056 and names four reasons it does not include.
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
	// against the generation's current members (alert_cases.group_id).
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
//     view of both is identical: a case opened.
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
		// A case opened. Whether it opened a group or JOINED one that had
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
