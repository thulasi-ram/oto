package domain

// THE INVESTIGATION, ITS STEPS AND ITS FINDING (ADR 0053 §1, §3, §6; git-bug 180a525).
//
// An Investigation is one run of one Investigator version against one subject, frozen
// once it ends. A Step is one immutable entry in its transcript. The Finding is what it
// concluded — kept, and marked partial, when a budget stopped it first.
//
// ⭐⭐ EVERY WAY A RUN ENDS IS A NAMED (Status, Reason) PAIR, AND THE PAIRS ARE CLOSED.
// ADR 0053 §6: "hitting one is recorded, never silent". An Ending can only be built
// through the constructors below, which refuse a reason that does not belong to its
// status, so "exhausted, because the model was disabled" cannot be written — and
// `investigations_reason_ck` refuses it again at the row.

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// SubjectKind is what an Investigation is about. ADR 0053 §4 names four subjects —
// `case | incident | digest | policy` — and only a Case has a run path today, so the
// set is closed at one and `investigations_subjkind_ck` admits only it. Adding a kind
// is a constant here, an arm in ParseSubjectKind, and a widened CHECK.
type SubjectKind string

// SubjectCase is one firing episode.
const SubjectCase SubjectKind = "case"

// ParseSubjectKind reads a stored subject kind.
func ParseSubjectKind(s string) (SubjectKind, error) {
	if SubjectKind(s) == SubjectCase {
		return SubjectCase, nil
	}
	return "", errs.Newf(errs.KindInternal, "investigation_subject_kind",
		"an Investigation's subject is a case, not %q", s)
}

// Status is where an Investigation is.
type Status string

// The six statuses (`investigations_status_ck`).
const (
	// StatusQueued is requested and waiting for its job.
	StatusQueued Status = "queued"
	// StatusRunning has begun: a model may have been called.
	StatusRunning Status = "running"
	// StatusCompleted ended with the model's own answer.
	StatusCompleted Status = "completed"
	// StatusExhausted ended at a per-run budget; whatever Finding it reached is kept
	// and is partial.
	StatusExhausted Status = "exhausted"
	// StatusFailed ended on an error: no usage, a model error, the subject gone.
	StatusFailed Status = "failed"
	// StatusSkipped never started, and says why (the kill switch).
	StatusSkipped Status = "skipped"
)

// ParseStatus reads a stored status.
func ParseStatus(s string) (Status, error) {
	switch st := Status(s); st {
	case StatusQueued, StatusRunning, StatusCompleted, StatusExhausted, StatusFailed, StatusSkipped:
		return st, nil
	default:
		return "", errs.Newf(errs.KindInternal, "investigation_status", "unknown Investigation status %q", s)
	}
}

// Terminal reports whether the run has ended — and is therefore frozen
// (`investigations_frozen`).
func (s Status) Terminal() bool {
	return s == StatusCompleted || s == StatusExhausted || s == StatusFailed || s == StatusSkipped
}

// Reason is why a run ended any way but `completed`.
type Reason string

// The reasons, by the status each belongs to (`investigations_reason_ck`).
const (
	// ReasonStepBudget: the run asked for more Tool calls than its step budget.
	ReasonStepBudget Reason = "step_budget"
	// ReasonTokenBudget: the run spent its input + output token budget.
	ReasonTokenBudget Reason = "token_budget"
	// ReasonWallTime: the run ran out of wall time.
	ReasonWallTime Reason = "wall_time_budget"

	// ReasonUsageMissing: the model answered without token usage, so the run could
	// not be budgeted (ADR 0053 §3). It will recur until the endpoint is changed.
	ReasonUsageMissing Reason = "usage_missing"
	// ReasonModelError: the model endpoint failed — unreachable, refused, malformed.
	ReasonModelError Reason = "model_error"
	// ReasonModelChanged: the endpoint row no longer has the identity the version
	// pinned, so a Finding could not truthfully name the model that produced it.
	ReasonModelChanged Reason = "model_changed"
	// ReasonSubjectGone: the Case no longer exists (a drill disposed of its own).
	ReasonSubjectGone Reason = "subject_gone"
	// ReasonInterrupted: the worker running it stopped before it ended. It is not
	// re-run: a re-run would pay for every turn a second time.
	ReasonInterrupted Reason = "interrupted"
	// ReasonInternal: oto failed — a write it could not make.
	ReasonInternal Reason = "internal"

	// ReasonDisabled: the org's or the Investigator's kill switch was off when the
	// run would have begun (ADR 0053 §6: "Nothing starts").
	ReasonDisabled Reason = "disabled"
)

func (r Reason) status() Status {
	switch r {
	case ReasonStepBudget, ReasonTokenBudget, ReasonWallTime:
		return StatusExhausted
	case ReasonUsageMissing, ReasonModelError, ReasonModelChanged, ReasonSubjectGone, ReasonInterrupted, ReasonInternal:
		return StatusFailed
	case ReasonDisabled:
		return StatusSkipped
	default:
		return ""
	}
}

// MaxReasonDetail mirrors `investigations_detail_ck`.
const MaxReasonDetail = 2000

// Ending is how a run ended: a terminal Status, the Reason (empty for `completed`)
// and a sentence for a human.
type Ending struct {
	Status Status
	Reason Reason
	Detail string
}

// Completed is a run that ended with the model's own answer.
func Completed() Ending { return Ending{Status: StatusCompleted} }

// EndedBy is a run that ended for a reason. The status is the reason's own; the
// detail is clipped to the column and never empty.
func EndedBy(r Reason, detail string) Ending {
	st := r.status()
	if st == "" {
		// A reason with no status is an oto bug; recording it as internal is the
		// truthful fallback, and the detail says what was attempted.
		return Ending{Status: StatusFailed, Reason: ReasonInternal,
			Detail: clip("an unknown ending reason "+string(r)+" was recorded: "+detail, MaxReasonDetail)}
	}
	d := strings.TrimSpace(detail)
	if d == "" {
		d = string(r)
	}
	return Ending{Status: st, Reason: r, Detail: clip(d, MaxReasonDetail)}
}

// RestoreEnding rebuilds an Ending read from a row, refusing a pair
// `investigations_reason_ck` would have.
func RestoreEnding(status Status, reason, detail string) (Ending, error) {
	switch {
	case status == StatusCompleted && reason == "":
		return Completed(), nil
	case status.Terminal() && status != StatusCompleted && Reason(reason).status() == status:
		return Ending{Status: status, Reason: Reason(reason), Detail: detail}, nil
	default:
		return Ending{}, errs.Newf(errs.KindInternal, "investigation_ending_invalid",
			"an Investigation cannot end %s with reason %q", status, reason)
	}
}

// Requester is who asked for an Investigation (ADR 0053 §4: "a human asks").
//
// ACTOR METADATA in the acked_by mould: the user id is nulled when the user goes, the
// label is frozen beside it, and no per-person metric is derived from either.
type Requester struct {
	UserID uuid.UUID
	Label  string
}

// MaxRequesterLabel mirrors `investigations_label_ck`.
const MaxRequesterLabel = 200

// NewRequester builds a Requester. The label may not be blank: a run nobody asked for
// is not attributable.
func NewRequester(userID uuid.UUID, label string) (Requester, error) {
	l := strings.TrimSpace(label)
	if l == "" {
		return Requester{}, errs.New(errs.KindValidation, "investigation_requester_required",
			"an Investigation is attributed to whoever asked for it")
	}
	return Requester{UserID: userID, Label: clip(l, MaxRequesterLabel)}, nil
}

// MaxFindingLength mirrors `investigations_finding_ck`, in characters.
const MaxFindingLength = 16384

// NewFinding clips a model's final text to what a Finding may hold, and reports ""
// for one that said nothing.
func NewFinding(text string) string {
	t := strings.TrimSpace(text)
	if t == "" {
		return ""
	}
	return clip(t, MaxFindingLength)
}

// Investigation is one run, as stored.
type Investigation struct {
	ID          uuid.UUID
	OrgID       uuid.UUID
	SubjectKind SubjectKind
	SubjectID   uuid.UUID
	// AlertKey is the subject Alert's key, copied at request time, so prior Findings
	// on the same key are one read of this module's own table.
	AlertKey string

	InvestigatorID   uuid.UUID
	InvestigatorName string
	VersionID        uuid.UUID
	VersionNumber    int
	// Model is the identity the version pinned: what produced the Finding.
	Model ModelIdentity

	Status Status
	// Ending is set once Status is terminal.
	Ending Ending

	// Budgets are the ones in force when it was requested.
	Budgets Budgets
	// Spent is the tokens it spent; ToolCalls the Tool calls it made.
	Spent     Usage
	ToolCalls int
	// Finding is what it concluded, "" for none. Partial when Status is exhausted.
	Finding string

	RequestedBy Requester
	RequestedAt time.Time
	StartedAt   time.Time
	EndedAt     time.Time
}

// Partial reports whether the Finding was cut short by a budget.
func (i Investigation) Partial() bool { return i.Status == StatusExhausted }

// EnricherName is the Enrichment its Finding is published as.
func (i Investigation) EnricherName() string { return EnricherPrefix + i.InvestigatorName }

// StepKind is what one transcript entry records.
type StepKind string

// The two kinds (`investigation_steps_kind_ck`).
const (
	// StepModelTurn is one answer from the model: text, Tool calls, finish reason,
	// tokens.
	StepModelTurn StepKind = "model_turn"
	// StepToolCall is one Tool call the model asked for and what came of it.
	StepToolCall StepKind = "tool_call"
)

// ToolOutcome is what came of one Tool call.
type ToolOutcome string

// The outcomes (`investigation_steps_call_ck`). Every one but `ok` is still answered
// to the model, which must hear why it got nothing; only the step budget ends the run.
const (
	OutcomeOK ToolOutcome = "ok"
	// OutcomeRefused: outside the allowlist, no such Tool, or past the step budget.
	OutcomeRefused ToolOutcome = "refused"
	// OutcomeTimeout: the call ran past its per-call timeout.
	OutcomeTimeout ToolOutcome = "timeout"
	// OutcomeTruncated: the result was cut to the per-call size cap.
	OutcomeTruncated ToolOutcome = "truncated"
	// OutcomeFailed: the Tool failed, or the model's arguments did not parse.
	OutcomeFailed ToolOutcome = "failed"
)

// MaxStepText mirrors `investigation_steps_size_ck` for every text column.
const MaxStepText = 65536

// Step is one immutable transcript entry. Build it with NewModelTurnStep or
// NewToolStep; Seq is its 1-based position in the transcript.
type Step struct {
	ID   uuid.UUID
	Seq  int
	Kind StepKind

	// A model turn.
	Text   string
	Calls  []ToolCall
	Finish FinishReason
	Usage  Usage

	// A Tool call.
	Call    ToolCall
	Outcome ToolOutcome
	Result  string

	Duration   time.Duration
	RecordedAt time.Time
}

// NewModelTurnStep records one model turn.
func NewModelTurnStep(seq int, t Turn, took time.Duration, at time.Time) Step {
	calls := make([]ToolCall, len(t.ToolCalls))
	for i, c := range t.ToolCalls {
		c.Arguments = clip(c.Arguments, MaxStepText)
		calls[i] = c
	}
	finish := t.Finish
	if finish == "" {
		finish = FinishStop
	}
	return Step{Seq: seq, Kind: StepModelTurn, Text: clip(t.Text, MaxStepText), Calls: calls,
		Finish: finish, Usage: t.Usage, Duration: nonNegative(took), RecordedAt: at.UTC()}
}

// NewToolStep records one Tool call and what came of it. The arguments are the raw
// bytes the model wrote, kept even when they did not parse — that is a fact about the
// model.
func NewToolStep(seq int, call ToolCall, outcome ToolOutcome, result string, took time.Duration, at time.Time) Step {
	call.Arguments = clip(call.Arguments, MaxStepText)
	return Step{Seq: seq, Kind: StepToolCall, Call: call, Outcome: outcome,
		Result: clip(result, MaxStepText), Duration: nonNegative(took), RecordedAt: at.UTC()}
}

// clip cuts s to at most n characters on a rune boundary.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func nonNegative(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}
