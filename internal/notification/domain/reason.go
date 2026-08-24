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

// The closed Reason set, as narrowed by migration 00018 and again by migrations
// 00060 and 00069. The scope-banned word migration 00018 removed is gone from this
// vocabulary and from this file, and it has NO REPLACEMENT TERM — nothing in this
// set names a second stage of anything, because there are no stages.
//
// ⛔ `unacked_reminder` BRIEFLY TOOK ITS PLACE AND IS ITSELF DELETED (git-bug
// bd0fb1d). It was one reminder stage aimed at a CHANNEL rather than a person,
// which is what kept it inside scope; the owner then withdrew the feature outright.
// OTO SENDS NOTHING UNPROMPTED. Every Reason below is oto relaying something it
// OBSERVED — a transition, a comment, an enrichment landing — or, in `digest`'s
// case, answering a window the operator configured. None of them is oto deciding on
// its own that a human has been quiet too long. See the ⛔ note at
// `ReasonUnackedReminder`'s grave further down for why the value went rather than
// being retired.
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

	// ⛔ `ReasonNewAlerts` ("new_alerts") AND `ReasonSomeResolved` ("some_resolved")
	// WERE HERE AND ARE DELETED OUTRIGHT (git-bug `7570090`). Each asserted a
	// PLURALITY — "more of them started", "some of them stopped" — and a conversation
	// now holds exactly ONE Case, which is ONE Alert's firing episode. A fact about a
	// subset has no subset to be about, so neither has anything left to report.
	//
	// ⛔ DELETED RATHER THAN RETIRED, and that is the distinction the `severity_raised`
	// note below draws. `ReasonRefired` further down is what retirement is FOR: nothing
	// produces it, but rows on disk carry it and a renderer still has to draw one.
	// Nothing carries these two that survives — oto is unreleased, migration 00069
	// narrows `notifications_reason_ck` to match, and the maintainer has authorised the
	// reset that answers it. A vocabulary entry whose only possible reader cannot exist
	// is not caution; it is a CHECK-constraint value with no writer, which is a trap
	// the next person has to rule out.
	//
	// ⚠️ THE UPSTREAM VALUES THEY MAPPED DID NOT SIMPLY MOVE TO ANOTHER WORD. What
	// `new alerts added` and `some alerts resolved` now resolve to is `ReasonFromWire`'s
	// business, and the answer there is neither of the obvious ones — read its ⛔ block
	// before assuming a rename.

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
	// ⛔ `ReasonUnackedReminder` WAS HERE AND IS DELETED OUTRIGHT (git-bug bd0fb1d).
	// It was oto's one reminder stage; the owner withdrew the feature — oto sends
	// nothing unprompted — and then ruled that the value goes rather than being
	// retired, because oto is UNRELEASED and the database is being reset. There is
	// no history to keep decodable, so a value kept for a reader that cannot exist
	// is not caution: it is a vocabulary entry the next person has to rule out.
	// That is `EventType`'s own test, verbatim, applied in the other direction.
	// ReasonDigest is the fifteenth Reason and the only one whose subject is not
	// an object: it is a WINDOW OVER A NAMESPACE (migration 00058).
	//
	// (It was the eighteenth when 00058 added it. The ordinal has moved twice since —
	// `unacked_reminder` left with git-bug bd0fb1d, `new_alerts` and `some_resolved`
	// with 7570090 — and it is spelled out rather than dropped because the counts in
	// this file are load-bearing: `MaxPolicyReasons` is `len(AllReasons())` BY RULE.)
	//
	// ⭐ IT IS THE ONE REASON NO TRANSITION PRODUCES. The other fourteen are facts
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
	// `new_alerts` and `some_resolved` held positions two and three here and are
	// DELETED (git-bug `7570090`). The remaining values keep 00018's relative order:
	// the contract the dto_schema test enforces is the ORDER, and closing a gap left
	// by a deletion is not a re-ordering.
	ReasonFired, ReasonAllResolved,
	ReasonRepeat, ReasonSuppressed, ReasonUnsuppressed, ReasonExpired,
	ReasonRefired, ReasonAcked, ReasonUnacked, ReasonSnoozed, ReasonUnsnoozed,
	ReasonEnriched, ReasonRuleChanged, ReasonComment,
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
//
// ⚠️ IT HAS FIFTEEN MEMBERS: seventeen after git-bug bd0fb1d removed
// `unacked_reminder`, less the two plurality Reasons 7570090 deleted.
// `MaxPolicyReasons` is `len(AllReasons())` BY RULE and follows it — it is declared
// in policy.go as a literal, so a deletion here that is not mirrored there leaves
// `policies_reasons_ck` admitting a list longer than the vocabulary it draws from.
func AllReasons() []Reason {
	out := make([]Reason, len(allReasons))
	copy(out, allReasons)
	return out
}

// ⛔ `retiredReasons`, `Retired()` AND `SubscribableReasons()` WERE HERE AND ARE
// DELETED (git-bug bd0fb1d). They existed for one value — `unacked_reminder` —
// held readable while unwritable, and split "what a policy may name" from "what a
// row may hold". With the value gone outright those are the same set again, and
// `AllReasons()` is the one answer. Re-introducing the split needs a value that is
// genuinely on disk and genuinely unwritable; there is none.

// The two subjects that were not expressible before migration 00056. They are
// declared HERE, next to the allocation that gives them meaning, rather than
// beside `SubjectKind` itself in idempotency.go — where `alert_group` used to be
// declared and where its ⛔ deletion note now stands: a SubjectKind is only ever chosen by
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
	// the whole point of the ticket that added it. The other two are an identity and a
	// firing — there was a third, the generation, until git-bug `7570090` deleted
	// `alert_group`; this one is a SET SELECTED BY PROPERTIES OVER TIME,
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
// consults this map, so a sixteenth Reason cannot be added without deciding what
// it is about: a Reason declared with no subject fails validation at the door
// instead of quietly inheriting the group, which is exactly how every fact came to
// claim the group of forty in the first place. `allReasons` above is the ORDER
// (migration 00018 declares it, 00058 appends to it); this map is the SET.
//
// The three altitudes, and why each Reason sits where it does:
//
//   - ⛔ alert_group — THE ALTITUDE ITSELF IS DELETED (git-bug `7570090`), and it is
//     the one the plurality Reasons lived at. It held a fact about the GENERATION,
//     and the argument for it was an idempotency argument: the key is
//     (org, subject, reason, state_version), so a group-subject `fired` was ONE root
//     card per generation-version while a case-subject one minted a `fired` per
//     member and posted a root card per alert.
//
//     ⭐ THAT CONSEQUENCE IS NOW THE REQUIREMENT, WHICH IS WHY THE ARGUMENT DID NOT
//     SURVIVE ITS OWN PREMISE. A conversation holds exactly one Case, so one root
//     card per firing alert is the shape the owner ruled for, and the collapse the
//     group subject bought is the thing being removed. `fired`, `all_resolved` and
//     `repeat` moved to `case` intact — each is true of one episode. `new_alerts` and
//     `some_resolved` could not move: they were MEMBERSHIP ARITHMETIC over the
//     generation, and arithmetic over a set of one has no answer to give, so they
//     left the vocabulary (see the ⛔ block in the const list above). `unacked_reminder`
//     was latched at this altitude and left earlier, with git-bug bd0fb1d.
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
	// ⛔ THESE FIVE ALLOCATED `SubjectAlertGroup` AND THREE NOW ALLOCATE `SubjectCase`
	// (git-bug `7570090`). `fired`, `all_resolved` and `repeat` are each true of ONE
	// episode, so a Case carries them exactly. `new_alerts` and `some_resolved` are
	// DELETED outright: both assert a plurality — "more of them started", "some of
	// them stopped" — and a conversation holds one Case, so neither has anything to
	// be about. They are not retired to a dormant value; a Reason nothing can
	// allocate is the dead-enum shape the `severity_raised` note above refuses.
	ReasonFired:       SubjectCase,
	ReasonAllResolved: SubjectCase,
	ReasonRepeat:      SubjectCase,

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
// ⛔ IT USED NOT TO BE WHERE THE FACT WAS DELIVERED, AND THE TWO HAVE CONVERGED
// (git-bug `7570090`). This paragraph read "`group_id` is the delivery target for the
// seventeen signal Reasons and is mandatory for all seventeen
// (notifications_target_ck); the thread is keyed by the AlertGroup generation whatever
// this returns, so forty alerts still produce one thread." There is no generation to
// key a thread by: a conversation holds ONE Case, so what a fact is ABOUT and where it
// is DELIVERED are now the same answer. `SubjectKind`'s own note in idempotency.go is
// the full statement of the convergence; the fourteen signal Reasons here are the
// count that used to be seventeen.
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
	// WireDiffFallback means the wire cannot name this notification's Reason, so
	// oto's OWN observation has to. The payload predates AM 0.32.0 (or said
	// `unknown`) — or, since git-bug `7570090`, it named a plurality oto no longer
	// has a Reason for.
	//
	// ⚠️ IT USED TO SAY "derived by diffing the incoming fingerprint set against the
	// generation's current members (alert_cases.group_id)", and there is no
	// generation to diff against. With one Alert per Case the diff degenerates into
	// the per-alert transition `alerts/service` already observed, which is precisely
	// what `derived` carries into ReconcileWithWire below.
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
// from an older cluster. It falls back to oto's own observation.
//
// ⚠️ IT HAS NO CALLER SINCE git-bug `7570090`. `ReconcileWithWire` was the only one
// and it no longer consults the table, because a group-level authority has no
// group-level decision left to make (read its ⛔ block). This stays declared because
// §H.6 states the mapping as BINDING and it is the honest record of what oto does
// with each upstream spelling — including `none`, the one wire value that still
// carries a per-Case fact (`WireSuppress`: record a suppressed Notification and
// stop) and the one no code has ever acted on.
func ReasonFromWire(wire string) (Reason, WireVerdict) {
	switch wire {
	case wireFirstNotification:
		return ReasonFired, WireMapped
	case wireNewAlertsAdded, wireSomeResolved:
		// ⛔ THESE TWO MAPPED TO `new_alerts` AND `some_resolved`, AND BOTH REASONS ARE
		// DELETED (git-bug `7570090`). The values are NOT re-pointed at a surviving
		// Reason and they are NOT refused. Both of those would be wrong, for opposite
		// reasons, and the ruling is the whole content of this arm.
		//
		// ⭐ THE TEST IS WHETHER THE WIRE VALUE'S QUANTIFIER DISTRIBUTES OVER THE BATCH.
		// Alertmanager speaks about a whole GROUP; oto mints one notification per Case,
		// and a Case is one Alert's firing episode. The three values still mapped above
		// are UNIVERSAL over the batch, so each remains true of the one alert in focus:
		// if this is the group's `first notification` then every member in it is newly
		// firing, if `all alerts resolved` then this one resolved, if `repeat interval
		// elapsed` then this one is being re-delivered. These two are EXISTENTIAL —
		// "new alerts added" says SOME member is new, "some alerts resolved" says SOME
		// member stopped, and neither says WHICH. Handing either to the Case in focus
		// would let one member's transition relabel a different member's card, which is
		// a louder failure than saying nothing.
		//
		// ⛔ AND REFUSING THEM WOULD BE WORSE THAN IMPRECISE. `WireUnmapped` means "not
		// a value oto recognises". oto recognises both perfectly well — they are what a
		// healthy AM >= 0.32.0 sends on most deliveries into a grouped receiver — and a
		// caller that reads "unrecognised" as "broken payload" would drop them, which is
		// exactly the failure this function's own doc refuses for a cluster below 0.32.0.
		// So the verdict is the fallback: the wire cannot name this Case's Reason, and
		// oto's observed transition is the only thing that can.
		//
		// The two constants stay declared. Deleting them would fold two values oto
		// understands into `default`, where they would be indistinguishable from a
		// spelling Alertmanager has never sent.
		return "", WireDiffFallback
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

// ReconcileWithWire USED TO APPLY the §H.6 table to a Reason derived from oto's OWN
// per-alert transitions, using Alertmanager's `notification_reason` as the
// group-level authority it is. ⛔ IT NOW RETURNS `derived` UNCHANGED, ALWAYS, AND
// BOTH OF ITS OTHER PARAMETERS ARE BLANK (git-bug `7570090`).
//
// ⭐ IT DID NOT LOSE AN ARM, IT LOST ITS OBJECT. The two vocabularies it joined both
// still exist and still answer different questions:
//
//   - oto's transitions know WHAT CHANGED about one alert. They are the only source
//     for `acked`, `refired`, `expired`, `suppressed` — facts Alertmanager cannot
//     see or does not have a word for.
//   - Alertmanager's `notification_reason` knows WHY THIS BATCH WAS DELIVERED about
//     a whole GROUP.
//
// The second was load-bearing only where a fact about the group could out-rank a
// fact about one alert, and there were exactly two such places:
//
//   - `some_resolved` → `all_resolved` widened one alert's resolve into the group's
//     LAST resolve. With one Alert per Case, one alert's resolve IS the whole of it:
//     `alerts/service` derives `all_resolved` at the transition and there is no
//     narrower sibling left to widen from.
//   - `fired` → `new_alerts` drew the one distinction oto genuinely could not see —
//     a first fire versus a member joining a group already notified — because the
//     per-alert view of both is identical: a case opened. What it DECIDED was
//     whether a root card was posted or a reply appended into the generation's
//     thread. A conversation holds one Case now, so the joining alert opens its own
//     conversation and posts its own root card either way. The question still has
//     two answers upstream; it no longer has two consequences here.
//
// Before this function existed, `new_alerts`, `all_resolved` and `repeat` were CHECK
// values nothing could write, and the first live run posted a fully resolved card
// whose footer read "some alerts resolved". ⚠️ THAT FAILURE IS BACK WITHIN REACH FOR
// `repeat`: the wire table is the only thing that ever named it, and with this
// function ignoring the table nothing mints a `repeat` at all. It is left declared
// because §H.6 still describes the delivery; a writer for it is a separate ruling.
//
// ⛔ THE NAME NOW OVERSTATES WHAT THIS DOES, AND IT HAS NO CALLER AT ALL. An earlier
// version of this comment said it "SHOULD BE DELETED WITH ITS ONE CALLER —
// `notification/service.notify`" and that was already false when written: `mint`
// assigns `in.Reason` verbatim and reaches nothing here, so every remaining mention
// of this function in the tree is a comment. Deleting it therefore costs one
// declaration and no call site — it is kept only because §H.6 still describes the
// `repeat` delivery whose writer this would have been. ⛔ A future group-level authority is NOT a reason to keep the seam:
// the object that owns a fact about many alerts at once is an INCIDENT
// (`correlation`, DEFERRED-POST-V1), and a fact about an incident belongs ON the
// incident, not widening one member's Reason.
func ReconcileWithWire(derived Reason, _ string, _ bool) Reason { return derived }
