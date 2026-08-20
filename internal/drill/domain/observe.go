package domain

import (
	"strconv"

	"github.com/google/uuid"
)

// Artifacts is the EVIDENCE a drill reports on: the live rows the real pipeline
// wrote, read back exactly as they are.
//
// ⭐⭐ A DRILL IS NEVER TOLD WHAT HAPPENED; IT LOOKS. Nothing in the pipeline
// knows a drill is watching, no stage reports itself, and there is no callback to
// forget to fire. That is the difference between a self-test and a mock: if the
// notification worker stops writing `notification_deliveries`, a drill notices,
// whereas an instrumented pipeline would happily report a stage that no longer
// exists.
type Artifacts struct {
	Batch BatchFact
	Alert AlertFact
	// Case is also THE CONVERSATION. Every artefact below is reached through it:
	// the notification by `(conversation_kind, conversation_id) = ('case', case)`
	// and the thread by `(subject_kind, subject_id) = ('case', case)`.
	//
	// ⛔ `Group GroupFact` WAS HERE AND IS DELETED (git-bug `7570090`). It carried
	// the `alert_groups` generation and the membership row, and both are dropped from
	// the schema; the notification and the thread were reached THROUGH it, by
	// `notifications.group_id` and by `subject_kind = 'alert_group'`, and they are now
	// reached through the Case directly. The evidence got shorter, not weaker: one
	// fewer row has to exist for a drill to prove a card landed.
	Case         CaseFact
	Notification NotificationFact
	Threads      []ThreadFact
	Deliveries   []DeliveryFact
	// Rejections are `ingest_rejections` rows this batch produced. A drill's own
	// payload producing one is an oto bug and is reported as a `process` failure.
	Rejections []RejectionFact
	// RuleLookupPossible is false when the source has no Prometheus configured,
	// which is why the rule-snapshot stage may honestly skip.
	RuleLookupPossible bool
}

// BatchFact is the `ingest_batches` row.
type BatchFact struct {
	Found bool
	// Status is pending | processed | partial | failed.
	Status     string
	Error      string
	AlertCount int
}

// AlertFact is the `alerts` row the drill's label set became.
type AlertFact struct {
	Found bool
	ID    uuid.UUID
	Key   string
	// Synthetic must be TRUE. A false here means the provenance mark did not
	// propagate, which is a correctness bug of the first order: the drill's alert
	// would be counted in every statistic oto reports.
	Synthetic bool
	State     string
}

// CaseFact is the firing episode.
type CaseFact struct {
	Found          bool
	ID             uuid.UUID
	Seq            int
	State          string
	RuleSnapshotID uuid.UUID
	RuleName       string
}

// ⛔ `GroupFact` WAS HERE AND IS DELETED (git-bug `7570090`). It was the
// `alert_groups` generation plus the membership row — `ID`, `Key`, `Generation`,
// `Synthetic`, `Member`, `Title` — and every one of those columns is dropped with
// the table. `Member` was the interesting one: it existed because a generation
// could be resolved BEFORE the alert joined it, so a drill that reported "grouped"
// one poll early would be claiming something it had not seen. That race has no
// successor to inherit — a Case is opened BY its alert, so there is no window in
// which the conversation exists and the alert is not in it.

// NotificationFact is the intent, including a suppressed one.
type NotificationFact struct {
	Found bool
	ID    uuid.UUID
	// Status is pending | dispatched | partial | delivered | failed | suppressed.
	Status string
	// SuppressedReason is the §B.8.2 winner. `no_policy` is the answer this whole
	// feature exists to surface.
	SuppressedReason string
	Reason           string
	PolicyID         uuid.UUID
	PolicyName       string
}

// ThreadFact is one `channel_threads` row.
type ThreadFact struct {
	ChannelID              uuid.UUID
	ChannelName            string
	State                  string
	ProviderConversationID string
	ProviderThreadID       string
	DeadReason             string
	LastSentSeq            int
}

// DeliveryFact is one `notification_deliveries` row.
type DeliveryFact struct {
	ChannelID         uuid.UUID
	ChannelName       string
	Status            string
	Mode              string
	ThreadSeq         int
	Attempts          int
	Error             string
	ErrorClass        string
	ProviderMessageID string
	Ambiguous         bool
}

// RejectionFact is one `ingest_rejections` row.
type RejectionFact struct {
	Reason string
	Detail string
}

// Destination is one channel the drill reached, for the result screen's list.
type Destination struct {
	ChannelID         uuid.UUID
	ChannelName       string
	Status            string
	Mode              string
	ThreadID          string
	ProviderMessageID string
	Error             string
	ErrorClass        string
}

// ⛔ `Destination.Broadcast bool` WAS HERE AND IS DELETED. It was
// `Mode == "broadcast_reply"`, reported "so an operator can see the decision was
// made rather than skipped" — and there is no decision left to see: the thread
// broadcast mechanism is removed, `BroadcastPolicy.Warrants` had exactly one
// reachable Reason and the owner ruled it goes. A boolean that can only ever read
// false is not evidence, it is a column an operator learns to ignore.
//
// ⚠️ `Mode` SURVIVES AND IS THE HONEST VERSION OF THE SAME QUESTION. It carries the
// provider's own word for what oto did — `post_root`, `update_root`, `thread_reply`
// — so a result screen that wants to say how the card landed reads that instead of
// a derived flag with one possible value.
//
// ⭐ THE TRANSPORT WENT WITH IT, AND THE ORDER WAS FORCED. `broadcast` was a
// `required` property of `DrillDestinationDTO` under `additionalProperties: false`,
// so the published contract had to drop it before `api/dto.go` could — a schema that
// demands a byte the domain has no fact for is a schema no honest handler satisfies.

// Observe turns evidence into the staged result.
//
// ⛔ IT NEVER GUESSES. A stage is `passed` only when the row that proves it
// exists and says what it should; anything else is `pending` until the deadline
// makes the whole drill `timed_out`. The one exception is `rule_snapshot`, which
// may honestly `skip`, and the reason is written on that branch.
//
// It is pure: no clock, no I/O. `timedOut` comes from the caller.
func Observe(a Artifacts, timedOut bool) Result {
	stages := []Stage{
		acceptStage(a),
		processStage(a),
		identityStage(a),
		caseStage(a),
		ruleStage(a),
		policyStage(a),
		threadStage(a),
		orderingStage(a),
		deliveryStage(a),
	}
	res := Summarise(stages, timedOut)
	res.Destinations = destinations(a)
	return res
}

func acceptStage(a Artifacts) Stage {
	if !a.Batch.Found {
		return Stage{Name: StageAccept, Status: StatusPending,
			Detail: "waiting for the synthetic batch to appear on disk"}
	}
	return Stage{Name: StageAccept, Status: StatusPassed,
		Detail: "the synthetic batch was durably recorded and its processing job queued in one transaction",
		Facts:  map[string]string{"alert_count": strconv.Itoa(a.Batch.AlertCount)}}
}

func processStage(a Artifacts) Stage {
	if len(a.Rejections) > 0 {
		// ⭐ A drill's payload is built by oto. If ingestion rejected part of it,
		// the bug is oto's own bounds disagreeing with oto's own producer, and
		// saying so plainly is far more useful than a green tick.
		r := a.Rejections[0]
		return Stage{Name: StageProcess, Status: StatusFailed,
			Detail: "oto's own synthetic payload was rejected by ingest bounds (" + r.Reason +
				"): " + r.Detail + " — this is an oto bug, not a configuration problem",
			Facts: map[string]string{"rejections": strconv.Itoa(len(a.Rejections)), "reason": r.Reason}}
	}
	if !a.Batch.Found {
		return Stage{Name: StageProcess, Status: StatusPending}
	}
	switch a.Batch.Status {
	case "processed":
		return Stage{Name: StageProcess, Status: StatusPassed,
			Detail: "the batch was decoded, bounded and normalised into observations"}
	case "failed":
		return Stage{Name: StageProcess, Status: StatusFailed,
			Detail: "the ingest worker gave up on this batch: " + a.Batch.Error}
	default:
		return Stage{Name: StageProcess, Status: StatusPending,
			Detail: "the ingest worker has not finished with this batch yet",
			Facts:  map[string]string{"batch_status": a.Batch.Status}}
	}
}

func identityStage(a Artifacts) Stage {
	if !a.Alert.Found {
		return Stage{Name: StageIdentity, Status: StatusPending,
			Detail: "waiting for the label set to become an Alert"}
	}
	if !a.Alert.Synthetic {
		// ⛔ THIS IS A CORRECTNESS ALARM, NOT A DELIVERY PROBLEM. The alert exists
		// but the provenance mark did not reach it, which means this row is about
		// to be counted in the hygiene rollup, the dashboard and the alert list.
		// It is reported as a FAILURE even though the pipeline is working.
		return Stage{Name: StageIdentity, Status: StatusFailed,
			Detail: "the Alert was created but is NOT marked synthetic — it would pollute alert " +
				"statistics. Report this: the provenance mark did not survive ingestion.",
			Facts: map[string]string{"alert_key": a.Alert.Key}}
	}
	return Stage{Name: StageIdentity, Status: StatusPassed,
		Detail: "the label set was resolved to an Alert identity and marked synthetic",
		Facts:  map[string]string{"alert_key": a.Alert.Key, "alert_id": a.Alert.ID.String()}}
}

func caseStage(a Artifacts) Stage {
	if !a.Case.Found {
		return Stage{Name: StageCase, Status: StatusPending,
			Detail: "waiting for a firing episode to open"}
	}
	return Stage{Name: StageCase, Status: StatusPassed,
		Detail: "a firing episode opened — this is the row an operator would acknowledge, " +
			"and it is the conversation the card lands in",
		Facts: map[string]string{
			"case_id": a.Case.ID.String(),
			"seq":     strconv.Itoa(a.Case.Seq),
			"state":   a.Case.State,
		}}
}

// ⛔ `groupStage` WAS HERE AND IS DELETED (git-bug `7570090`). It reported four
// things and each has a successor or an obituary:
//
//   - "waiting for the §C.4 group generation to resolve" — there is no generation.
//   - "the generation exists but the alert has not joined it yet" — a Case is opened
//     BY its alert, so there is no window between the two to report on.
//   - "the group generation was opened but is NOT marked synthetic" — that alarm was
//     about `alert_groups.synthetic`, a second copy of the provenance mark that no
//     longer exists. `identityStage` still raises it for `alerts.synthetic`, which is
//     now the whole mark.
//   - "this generation owns the thread" — the CASE owns the thread, and `caseStage`
//     says so in the sentence an operator reads.

func ruleStage(a Artifacts) Stage {
	if !a.Case.Found {
		return Stage{Name: StageRuleSnapshot, Status: StatusPending}
	}
	if a.Case.RuleSnapshotID != uuid.Nil {
		return Stage{Name: StageRuleSnapshot, Status: StatusPassed,
			Detail: "a rule snapshot was captured and bound to the episode — this is what oto shows " +
				"as “what the rule said at that moment”",
			Facts: map[string]string{
				"rule_snapshot_id": a.Case.RuleSnapshotID.String(),
				"rule_name":        a.Case.RuleName,
			}}
	}
	// ⭐ SKIPPED, NOT FAILED, AND THE DISTINCTION IS THE HONEST ONE. A drill's
	// alert corresponds to no rule in anybody's Prometheus, because oto did not
	// write one there. The lookup ran and found nothing, which is the correct
	// outcome and tells an operator nothing bad. What it CANNOT prove is that a
	// real alert's rule would be captured, and pretending otherwise with a green
	// tick would be the worst kind of false confidence.
	detail := "no rule snapshot — expected: a drill's alert matches no Prometheus rule, so there is " +
		"nothing to capture. This stage cannot prove rule capture works for real alerts."
	if !a.RuleLookupPossible {
		detail = "no rule snapshot, and this source has no Prometheus configured, so oto could not " +
			"have looked one up for a real alert either."
	}
	return Stage{Name: StageRuleSnapshot, Status: StatusSkipped, Detail: detail}
}

func policyStage(a Artifacts) Stage {
	if !a.Notification.Found {
		return Stage{Name: StagePolicy, Status: StatusPending,
			Detail: "waiting for the notification intent to be minted"}
	}
	if a.Notification.SuppressedReason != "" {
		return Stage{Name: StagePolicy, Status: StatusFailed,
			Detail: suppressionDetail(a.Notification.SuppressedReason),
			Facts:  map[string]string{"suppressed_reason": a.Notification.SuppressedReason}}
	}
	facts := map[string]string{
		"notification_id": a.Notification.ID.String(),
		"reason":          a.Notification.Reason,
	}
	if a.Notification.PolicyName != "" {
		facts["policy"] = a.Notification.PolicyName
		facts["policy_id"] = a.Notification.PolicyID.String()
	}
	return Stage{Name: StagePolicy, Status: StatusPassed,
		Detail: "a notification policy matched and routed this Case to at least one destination",
		Facts:  facts}
}

// suppressionDetail turns a `suppressed_reason` into the sentence that actually
// tells an operator what to change.
//
// ⭐ `no_policy` is the reason this feature exists. A brand-new install has
// working credentials, a working renderer and no notification policy, so the
// channel test passes and no alert ever arrives — and until now nothing in the
// product said so.
//
// ⚠️ ITS SENTENCE USED TO SAY "THIS GROUP'S LABELS" AND NOW SAYS "THIS ALERT'S"
// (git-bug `7570090`). That is a correction, not a rewording: a policy is matched
// against the alert's own label set rather than against `SplitLabels` of a
// generation, so the labels an operator has to widen a matcher over are the ones
// the identity stage printed one line earlier — not a derived four-axis subset they
// have no way to see.
func suppressionDetail(reason string) string {
	switch reason {
	case "no_policy":
		return "no notification policy matched this alert's labels, so nothing was sent. " +
			"This is the most common reason a working oto install never delivers anything: " +
			"add a notification policy, or widen an existing one's matchers."
	case "channel_disabled":
		return "a policy matched, but every destination it routes to is disabled or deleted."
	case "snoozed":
		return "this alert is snoozed, so oto's own notifications are suppressed until the snooze expires."
	// ⛔ A `storm` AND A `flapping` ARM STOOD HERE AND BOTH ARE DELETED. They were
	// written in the past tense, on the reasoning that `notifications.suppressed_reason`
	// has no reaper and a drill replaying an old delivery could still read one — telling
	// an operator a damper "is engaged" when the damper no longer exists would send them
	// hunting for a setting that decides nothing. Migration 00059 narrowed
	// `notifications_suppmap_ck` to six with no backfill and the database was reset, so
	// there is no old delivery left to replay and no reason left to explain. The
	// `default` below is the honest answer for anything this switch has never heard of.
	case "throttled":
		return "the throttle for this destination is engaged, so this notification was suppressed."
	case "verbosity":
		return "a policy matched, but every destination's verbosity setting says this fact is not " +
			"worth a message."
	case "duplicate_render":
		return "the rendered card was byte-identical to the one already on the channel, so oto " +
			"suppressed a no-op update."
	default:
		return "the notification was suppressed: " + reason
	}
}

func threadStage(a Artifacts) Stage {
	if len(a.Threads) == 0 {
		return Stage{Name: StageThread, Status: StatusPending,
			Detail: "waiting for a channel thread to be opened for this Case"}
	}
	for _, t := range a.Threads {
		if t.State == "open" {
			return Stage{Name: StageThread, Status: StatusPassed,
				Detail: "a thread was opened in " + channelLabel(t.ChannelName) +
					" and oto holds both halves of the provider handle",
				Facts: map[string]string{
					"channel":         channelLabel(t.ChannelName),
					"conversation_id": t.ProviderConversationID,
					"thread_ts":       t.ProviderThreadID,
				}}
		}
	}
	for _, t := range a.Threads {
		if t.State == "dead" {
			return Stage{Name: StageThread, Status: StatusFailed,
				Detail: "the thread for " + channelLabel(t.ChannelName) + " is dead: " + t.DeadReason +
					" — this is a terminal provider error, not something a retry fixes",
				Facts: map[string]string{"channel": channelLabel(t.ChannelName), "dead_reason": t.DeadReason}}
		}
	}
	return Stage{Name: StageThread, Status: StatusPending,
		Detail: "the thread row exists but has not been bound to a provider conversation yet"}
}

func orderingStage(a Artifacts) Stage {
	if len(a.Deliveries) == 0 {
		return Stage{Name: StageOrdering, Status: StatusPending,
			Detail: "waiting for a delivery to take its position in the thread"}
	}
	lastSent := map[uuid.UUID]int{}
	for _, t := range a.Threads {
		lastSent[t.ChannelID] = t.LastSentSeq
	}
	for _, d := range a.Deliveries {
		if d.ThreadSeq == 0 {
			continue
		}
		if lastSent[d.ChannelID] >= d.ThreadSeq {
			return Stage{Name: StageOrdering, Status: StatusPassed,
				Detail: "the delivery took position " + strconv.Itoa(d.ThreadSeq) +
					" in the thread and the FIFO gate released it in order",
				Facts: map[string]string{
					"thread_seq":    strconv.Itoa(d.ThreadSeq),
					"last_sent_seq": strconv.Itoa(lastSent[d.ChannelID]),
				}}
		}
	}
	if d := firstSequenced(a.Deliveries); d != nil {
		// ⭐ A REAL STALL IS VISIBLE HERE AND NOWHERE ELSE. The gate holds a
		// delivery whose seq is not `last_sent_seq + 1`, so an earlier message
		// that never landed silently freezes everything behind it. Naming both
		// numbers is what turns "nothing arrived" into a diagnosis.
		return Stage{Name: StageOrdering, Status: StatusPending,
			Detail: "the delivery holds position " + strconv.Itoa(d.ThreadSeq) +
				" and the thread has only sent up to " + strconv.Itoa(lastSent[d.ChannelID]) +
				" — the FIFO gate is holding it behind an earlier message",
			Facts: map[string]string{
				"thread_seq":    strconv.Itoa(d.ThreadSeq),
				"last_sent_seq": strconv.Itoa(lastSent[d.ChannelID]),
			}}
	}
	return Stage{Name: StageOrdering, Status: StatusPending,
		Detail: "no thread position has been allocated yet"}
}

func firstSequenced(in []DeliveryFact) *DeliveryFact {
	for i := range in {
		if in[i].ThreadSeq > 0 {
			return &in[i]
		}
	}
	return nil
}

func deliveryStage(a Artifacts) Stage {
	if len(a.Deliveries) == 0 {
		return Stage{Name: StageDelivery, Status: StatusPending,
			Detail: "waiting for the delivery record to be created"}
	}
	terminal := true
	for _, d := range a.Deliveries {
		if d.Status == "sent" {
			return Stage{Name: StageDelivery, Status: StatusPassed,
				Detail: "the card was delivered to " + channelLabel(d.ChannelName) +
					" and the delivery was recorded — this is the row that makes delivery failure " +
					"visible per alert",
				Facts: map[string]string{
					"channel":             channelLabel(d.ChannelName),
					"mode":                d.Mode,
					"provider_message_id": d.ProviderMessageID,
				}}
		}
		if d.Status != "dead" && d.Status != "skipped" {
			terminal = false
		}
	}
	if terminal {
		d := a.Deliveries[0]
		detail := "the delivery to " + channelLabel(d.ChannelName) + " will not be retried: " + d.Error
		if d.Status == "skipped" {
			detail = "the delivery to " + channelLabel(d.ChannelName) + " was skipped: " + d.Error
		}
		if d.ErrorClass != "" {
			detail += " (" + d.ErrorClass + ")"
		}
		return Stage{Name: StageDelivery, Status: StatusFailed, Detail: detail,
			Facts: map[string]string{
				"channel":     channelLabel(d.ChannelName),
				"status":      d.Status,
				"error_class": d.ErrorClass,
			}}
	}
	return Stage{Name: StageDelivery, Status: StatusPending,
		Detail: "the delivery record exists and the send has not settled yet",
		Facts:  map[string]string{"attempts": strconv.Itoa(a.Deliveries[0].Attempts)}}
}

func destinations(a Artifacts) []Destination {
	if len(a.Deliveries) == 0 {
		return nil
	}
	ts := map[uuid.UUID]string{}
	for _, t := range a.Threads {
		ts[t.ChannelID] = t.ProviderThreadID
	}
	out := make([]Destination, 0, len(a.Deliveries))
	for _, d := range a.Deliveries {
		out = append(out, Destination{
			ChannelID:         d.ChannelID,
			ChannelName:       d.ChannelName,
			Status:            d.Status,
			Mode:              d.Mode,
			ThreadID:          ts[d.ChannelID],
			ProviderMessageID: d.ProviderMessageID,
			Error:             d.Error,
			ErrorClass:        d.ErrorClass,
		})
	}
	return out
}

func channelLabel(name string) string {
	if name == "" {
		return "the destination"
	}
	return name
}
