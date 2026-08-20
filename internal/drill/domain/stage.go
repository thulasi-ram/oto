package domain

import "github.com/thulasiram/oto/internal/platform/errs"

// StageName is one link in the chain a real alert travels.
//
// ⭐⭐ THIS LIST IS THE WHOLE OPERATOR-FACING VALUE OF A DRILL. "It did not work"
// is what the channel test already says. "The policy matched nothing", "the
// thread would not open because Slack says not_in_channel", "the card was
// rendered but the ordering gate is holding it behind an earlier delivery" —
// those are answers, and each of them is a different afternoon.
//
// The names are the REAL stages, read off the code rather than invented: each one
// is a row some part of oto writes, and the drill reports its status by looking at
// that row rather than by being told.
type StageName string

const (
	// StageAccept is §G.1: the synthetic batch is durably on disk and its
	// `ingest.process_batch` job is queued, in one transaction. Evidence:
	// `ingest_batches`.
	StageAccept StageName = "accept"
	// StageProcess is §G.4: the batch was decoded, bounded and normalised into
	// Observations. Evidence: `ingest_batches.status`, `ingest_rejections`.
	StageProcess StageName = "process"
	// StageIdentity is §C.2: the label set became an Alert, with an alert_key.
	// Evidence: `alerts`.
	StageIdentity StageName = "identity"
	// StageCase is §B.3 T1: a firing episode opened. Evidence: `alert_cases`.
	//
	// ⭐ IT IS ALSO THE CONVERSATION, WHICH IS WHAT THE GROUP STAGE USED TO PROVE
	// (git-bug `7570090`). A Case is what the thread belongs to and what every
	// stage after `policy` is addressed by, so "the drill reached an object that
	// owns a thread" is asserted here rather than one stage later.
	StageCase StageName = "case"
	// ⛔ `StageGroup StageName = "group"` WAS HERE AND IS DELETED (git-bug
	// `7570090`). It was §C.4 — "an AlertGroup generation was resolved and the alert
	// joined it", evidenced by `alert_groups` and `alert_cases.group_id` — and both
	// of those are dropped from the schema, so the stage had no row left to look at
	// and a stage that cannot look is not a stage a drill may report.
	//
	// ⭐ WHAT IT PROVED IS STILL PROVED, BY TWO STAGES THAT ALREADY EXISTED. Its
	// real assertion was never "a generation exists" — it was "the alert reached the
	// object that owns the thread the card will land in". `StageCase` now asserts
	// that object exists and `StageThread` asserts the conversation opened on it, so
	// the chain still breaks at a nameable link when the join between them fails.
	//
	// ⚠️ ONE THING IT PROVED IS GENUINELY GONE, and pretending otherwise would be
	// the dishonest fix: `alert_groups.synthetic`. The group stage failed loudly
	// when the provenance mark reached the alert but not its generation. There is no
	// second row to mark any more — `alerts.synthetic` is the whole mark, and
	// `StageIdentity` still fails loudly when it is missing.
	//
	// The `group` member of `DrillStageName` in `api/openapi/openapi.yaml` has been
	// removed too, so the contract and `AllStages()` agree — which
	// `TestContractEnumsMatchTheirDomainEnum` demands as set AND order parity.

	// StageRuleSnapshot is ADR 0009: what the rule said at fire time. Evidence:
	// `alert_cases.rule_snapshot_id`.
	//
	// ⭐ THIS STAGE IS ALLOWED TO SKIP, and that is not a weakness. A drill's
	// alert corresponds to no Prometheus rule, because oto did not invent one in
	// somebody's cluster. What it can honestly report is whether the LOOKUP ran
	// and came back empty, which is a different fact from the pipeline stalling.
	StageRuleSnapshot StageName = "rule_snapshot"
	// StagePolicy is §G.5: a Notification intent was minted and a notification
	// policy matched it. Evidence: `notifications`, including a row with
	// `status='suppressed'` and `suppressed_reason='no_policy'`, which is the
	// single most common real answer and the reason this feature exists.
	StagePolicy StageName = "policy"
	// StageThread is §H.1: a `channel_threads` row reached `open` with both
	// halves of the provider handle — and the conversation id came from the API
	// RESPONSE, never from config.
	StageThread StageName = "thread"
	// StageOrdering is §G.7: the delivery took a `thread_seq` and the gate let it
	// through, so `last_sent_seq` advanced to it. A drill that stalls here is
	// looking at a genuine FIFO stall, which is invisible everywhere else.
	StageOrdering StageName = "ordering"
	// StageDelivery is the send itself: a `notification_deliveries` row reached
	// `sent`, with the provider's message id. Evidence: that row.
	StageDelivery StageName = "delivery"
)

// AllStages is the chain in causal order. A result always reports every stage,
// including the ones that never started — an operator needs to see how far it got
// as much as what broke.
func AllStages() []StageName {
	return []StageName{
		StageAccept, StageProcess, StageIdentity, StageCase,
		StageRuleSnapshot, StagePolicy, StageThread, StageOrdering, StageDelivery,
	}
}

// StageStatus is what a stage did.
type StageStatus string

const (
	// StatusPending means the stage has not been reached yet. On a settled drill
	// it means the chain stopped before it.
	StatusPending StageStatus = "pending"
	// StatusPassed means the stage's evidence row exists and says what it should.
	StatusPassed StageStatus = "passed"
	// StatusFailed means the stage was reached and did not succeed.
	StatusFailed StageStatus = "failed"
	// StatusSkipped means the stage did not apply and the chain carried on. It is
	// NOT a failure and never sets `failed_stage`.
	StatusSkipped StageStatus = "skipped"
)

// Stage is one reported link.
type Stage struct {
	Name   StageName
	Status StageStatus
	// Detail is one sentence an operator can act on. It names the provider's own
	// error code where there is one ("slack: not_in_channel") because that is
	// what a support question is answered with — and NEVER the underlying error
	// string, which can carry a request URL and therefore a token.
	Detail string
	// Facts are the small, typed pieces of evidence the UI shows beside the
	// stage: an alert_key, a case seq, a Slack ts, a policy name. Keys are stable
	// and snake_case; values are strings because this is a display surface, not a
	// second API.
	Facts map[string]string
}

// Result is the whole staged verdict.
type Result struct {
	Status      Status
	FailedStage StageName
	Stages      []Stage
	// Destinations is where the card actually went, one row per
	// `notification_deliveries` row. It is beside the stages rather than inside
	// one because "which channels, and did each land" is the answer an operator
	// wants even when every stage passed.
	Destinations []Destination
}

// Status is the drill's overall verdict (delivery_drills.status).
type Status string

const (
	// DrillRunning means the chain is still in flight and its deadline has not
	// passed.
	DrillRunning Status = "running"
	// DrillPassed means every stage passed or honestly skipped.
	DrillPassed Status = "passed"
	// DrillFailed means one stage failed. `FailedStage` names it.
	DrillFailed Status = "failed"
	// DrillTimedOut means the deadline passed with the chain still incomplete.
	//
	// ⭐ It is a DIFFERENT VERDICT from failed on purpose. "Slack rejected the
	// card" and "nothing has picked the job up in ninety seconds" send an
	// operator to completely different places — the second one usually means no
	// worker is running, which no per-stage error could ever say.
	DrillTimedOut Status = "timed_out"
)

// String renders the status.
func (s Status) String() string { return string(s) }

// String renders the stage name.
func (s StageName) String() string { return string(s) }

// String renders the stage status.
func (s StageStatus) String() string { return string(s) }

// NewStatus parses a stored status, refusing anything outside drills_status_ck.
func NewStatus(s string) (Status, error) {
	switch Status(s) {
	case DrillRunning, DrillPassed, DrillFailed, DrillTimedOut:
		return Status(s), nil
	default:
		return "", errs.New(errs.KindValidation, "enum",
			"status must be one of: running, passed, failed, timed_out")
	}
}

// Terminal reports whether the verdict is settled and will never move again.
func (s Status) Terminal() bool { return s != DrillRunning }

// Summarise folds a stage list into a verdict.
//
// ⭐ THE FIRST FAILURE WINS AND THE REST ARE LEFT PENDING. A chain that broke at
// `policy` has nothing to say about `thread`, and reporting eight downstream
// "failures" caused by one upstream one is how a diagnostic screen becomes
// noise. Exactly one stage is named, and it is the earliest one.
//
// `timedOut` is decided by the caller against the clock rather than in here,
// because a pure function must not read one.
func Summarise(stages []Stage, timedOut bool) Result {
	res := Result{Status: DrillRunning, Stages: stages}
	complete := true
	for _, st := range stages {
		switch st.Status {
		case StatusFailed:
			res.Status = DrillFailed
			res.FailedStage = st.Name
			return res
		case StatusPending:
			complete = false
		case StatusPassed, StatusSkipped:
		}
	}
	switch {
	case complete:
		res.Status = DrillPassed
	case timedOut:
		res.Status = DrillTimedOut
	}
	return res
}
