package domain

// A REMEDY EARNS THE WRITE PATH (ADR 0054; git-bug 4148256).
//
// "A change to a cluster that an Investigator proposes and oto executes only after a human
// approves it: proposed → approved → executed | failed, or declined, or expired, each by a
// named actor."
//
// ⭐⭐ ONLY AN INVESTIGATOR PROPOSES, AND IT NEVER HOLDS THE WRITE TOOL. A run proposes through
// the built-in `oto_propose_remedy`, which the loop answers itself — the way it answers
// `oto_classify` and the Suggestion Tools — and the proposal is kept with the Finding. A write
// ToolServer's Tools are never offered to a model (ToolServerConfig.Readable); the Remedy only
// NAMES one, with the exact arguments it would be sent.
//
// ⭐⭐ A REMEDY NAMES THE WRITE TOOL THAT WOULD CARRY IT OUT, OR SAYS THAT NONE CAN (ADR 0054
// §1, ruling of 2026-10-02). A Remedy with no Tool says NoToolCanCarryItOut, can never be
// approved (RemedyHasNoTool, and `remedies_no_tool_ck` under it), and can still be declined or
// expire. A Remedy whose Tool was removed from configuration after the proposal — its
// ToolServer deleted, re-declared `read`, or no longer listing the Tool — is unapprovable and
// unexecutable rather than executed against nothing (RemedyBinding), checked at approval AND
// again at execution.
//
// ⭐⭐ WHAT IS APPROVED IS WHAT IS EXECUTED. Arguments is the compact JSON object the
// Investigator proposed, byte for byte, and ArgumentsSHA256 is the SHA-256 of those bytes —
// the schema checks it (`remedies_arguments_hash_ck`) and freezes both (`remedies_frozen`).
// An approver approves a hash they were shown; the executor sends only when every hash agrees.
//
// ⭐ A REMEDY NEEDS ONE OR TWO APPROVALS FROM DIFFERENT GRANT HOLDERS, AS THE OPERATOR'S RISK
// RULES SAY (git-bug eb4f21b, remedy_risk.go). RequiredApprovals is set at proposal from the
// rules' verdict — two when no rule matches or the command is unparseable — and a risk model
// may only raise it; Risk records which rule (or which of those) set it. One person approving
// twice counts once (`remedy_approvals_user_uniq`).
//
// ⭐ EXPIRY IS READ OFF THE CLOCK AT ONCE AND RECORDED BY THE SWEEP. A proposed or approved
// Remedy past ExpiresAt reads as expired (StateAt) — it cannot be approved, declined or
// executed from then — and `remedies.sweep` records the transition by `system`, declared like
// every other: recorded, never silent.
//
// ⛔ A FAILED REMEDY IS NEVER RETRIED (§6). A retry is a new Remedy and a new approval.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// NoToolCanCarryItOut is what a Remedy that names no Tool says, in these words, wherever it is
// shown — the API, the UI and the fact sent outbound.
const NoToolCanCarryItOut = "no configured Tool can carry this out"

// Bounds mirroring migration 00104's CHECKs.
const (
	// MaxRemediesPerRun bounds what one Investigation may propose. A proposal past it is
	// refused on the record.
	MaxRemediesPerRun = 3
	// MaxRemedyArgumentsBytes is `remedies_arguments_ck`: the compact JSON object a write Tool
	// would be sent.
	MaxRemedyArgumentsBytes = 16384
	// MaxRemedyTarget is `remedies_target_ck`, in characters: what the change is made to.
	MaxRemedyTarget = 500
	// MaxRemedyDescription is `remedies_description_ck`, in characters: the Investigator's
	// account of the change, shown BELOW the exact command.
	MaxRemedyDescription = 2000
	// MaxRemedyDetail is `remedies_detail_ck`.
	MaxRemedyDetail = 2000
	// MaxRemedyResult is `remedies_result_ck`: what the write Tool answered, kept.
	MaxRemedyResult = 16384
)

// RemedyOutcomeDeadline is how long after its claim an `executing` Remedy may go without a
// recorded answer before the sweep records it `failed` with `outcome_unknown`: the longest
// per-call timeout a ToolServer may set, and five minutes for the worker to record what came
// back. A Remedy still `executing` past it belonged to a worker that died mid-call — the
// change may or may not have been made — and ⛔ IT IS NEVER SENT AGAIN.
const RemedyOutcomeDeadline = MaxCallTimeoutSeconds*time.Second + 5*time.Minute

// DefaultRequiredApprovals is how many different grant holders must approve a Remedy the risk
// rules did not lower (git-bug eb4f21b) — and every Remedy that names no Tool, which nobody
// can approve at all: two.
const DefaultRequiredApprovals = DoubleApproval

// DefaultRemedyApprovalWindow is `remedy_approval_window_s`'s shipped default: how long a
// proposed Remedy waits for its approvals, and an approved one for its execution.
const DefaultRemedyApprovalWindow = time.Hour

// RemedyWindow is the org's approval window, or the shipped default when none was read.
func (c OrgControls) RemedyWindow() time.Duration {
	if c.RemedyApprovalWindow <= 0 {
		return DefaultRemedyApprovalWindow
	}
	return c.RemedyApprovalWindow
}

// RemedyState is where a Remedy is (`remedies_state_ck`).
type RemedyState string

// The seven states. Four are terminal and frozen.
const (
	// RemedyProposed waits for its approvals.
	RemedyProposed RemedyState = "proposed"
	// RemedyApproved has them, and waits for the executor.
	RemedyApproved RemedyState = "approved"
	// RemedyExecuting is the executor's claim, committed before the write Tool is called: a
	// worker that dies mid-call leaves it here, and nothing sends it again.
	RemedyExecuting RemedyState = "executing"
	// RemedyExecuted: the write Tool answered without reporting a failure.
	RemedyExecuted RemedyState = "executed"
	// RemedyFailed: it was not executed, or the write Tool reported a failure, or what
	// happened is not known. Never retried.
	RemedyFailed RemedyState = "failed"
	// RemedyDeclined: a human said no.
	RemedyDeclined RemedyState = "declined"
	// RemedyExpired: nobody approved it, or it was not executed, in time.
	RemedyExpired RemedyState = "expired"
)

// ParseRemedyState reads a stored state.
func ParseRemedyState(s string) (RemedyState, error) {
	switch st := RemedyState(s); st {
	case RemedyProposed, RemedyApproved, RemedyExecuting, RemedyExecuted, RemedyFailed, RemedyDeclined, RemedyExpired:
		return st, nil
	default:
		return "", errs.Newf(errs.KindInternal, "remedy_state", "unknown Remedy state %q", s)
	}
}

// Terminal reports whether the state is one a Remedy never leaves.
func (s RemedyState) Terminal() bool {
	switch s {
	case RemedyExecuted, RemedyFailed, RemedyDeclined, RemedyExpired:
		return true
	default:
		return false
	}
}

// Declared reports whether reaching this state goes outbound as an Incident fact. Every
// transition does but the executor's claim, which is a bookkeeping step between two that do.
func (s RemedyState) Declared() bool { return s != RemedyExecuting }

// FactReason is the Incident fact a transition into this state is declared as: the state,
// prefixed — `remedy_proposed`, `remedy_approved`, and so on.
func (s RemedyState) FactReason() string { return "remedy_" + string(s) }

// RemedyFailure is why a Remedy failed (`remedies_failure_ck`).
type RemedyFailure string

// The five reasons. The first two are about a call that was made; the last three are the
// executor refusing to make one.
const (
	// FailToolError: the write Tool answered, and said the call failed.
	FailToolError RemedyFailure = "tool_error"
	// FailOutcomeUnknown: the call was sent, or may have been, and no answer was recorded —
	// a timeout, a broken connection, a worker that died mid-call. The change may or may not
	// have been made.
	FailOutcomeUnknown RemedyFailure = "outcome_unknown"
	// FailToolUnavailable: the ToolServer or the Tool was gone when it was to be sent. Not sent.
	FailToolUnavailable RemedyFailure = "tool_unavailable"
	// FailArgumentsChanged: the arguments no longer hash to what was approved. Not sent.
	FailArgumentsChanged RemedyFailure = "arguments_changed"
	// FailApprovalsWithdrawn: fewer approvers than it needs still hold the grant. Not sent.
	FailApprovalsWithdrawn RemedyFailure = "approvals_withdrawn"
)

// ParseRemedyFailure reads a stored reason; "" is none.
func ParseRemedyFailure(s string) (RemedyFailure, error) {
	switch f := RemedyFailure(s); f {
	case "", FailToolError, FailOutcomeUnknown, FailToolUnavailable, FailArgumentsChanged, FailApprovalsWithdrawn:
		return f, nil
	default:
		return "", errs.Newf(errs.KindInternal, "remedy_failure", "unknown Remedy failure %q", s)
	}
}

// RemedyActorKind is who moved a Remedy (`remedy_transitions_actor_ck`).
type RemedyActorKind string

// The three actors.
const (
	// ActorInvestigator proposes, and does nothing else.
	ActorInvestigator RemedyActorKind = "investigator"
	// ActorUser approves or declines.
	ActorUser RemedyActorKind = "user"
	// ActorSystem is oto itself: it records an expiry, claims an approved Remedy, and records
	// what the write Tool answered.
	ActorSystem RemedyActorKind = "system"
)

// SystemActorLabel names oto as the actor of what it does on its own.
const SystemActorLabel = "oto"

// RemedyActor is who made one transition: a kind, and — for a user — who, with the label
// frozen beside the id the way acked_by is.
type RemedyActor struct {
	Kind   RemedyActorKind
	UserID uuid.UUID
	Label  string
}

// SystemActor is oto.
func SystemActor() RemedyActor { return RemedyActor{Kind: ActorSystem, Label: SystemActorLabel} }

// UserActor is the human who approved or declined.
func UserActor(by Requester) RemedyActor {
	return RemedyActor{Kind: ActorUser, UserID: by.UserID, Label: by.Label}
}

// RemedyTool names the write Tool a Remedy would be carried out by: a ToolServer, by id and
// by the name it had when proposed, and the ToolServer's own name for the Tool. Zero is no
// Tool.
type RemedyTool struct {
	ToolServerID   uuid.UUID
	ToolServerName string
	Tool           string
}

// Named reports whether a Tool is named at all.
func (t RemedyTool) Named() bool { return t.ToolServerID != uuid.Nil }

// Qualified is the Tool as an allowlist and a model would spell it, `<toolserver>__<tool>`;
// "" for none.
func (t RemedyTool) Qualified() string {
	if !t.Named() {
		return ""
	}
	return t.ToolServerName + QualifiedToolSeparator + t.Tool
}

// RemedyBinding says why a Remedy's write Tool cannot carry it out NOW — "" when it can. It
// is asked at approval and again at execution, because configuration outlives a proposal:
// the ToolServer may have been removed (found is false), re-declared `read`, or have stopped
// listing the Tool at its last discovery.
func RemedyBinding(t RemedyTool, cfg ToolServerConfig, found bool, listed []DiscoveredTool) string {
	if !t.Named() {
		return NoToolCanCarryItOut
	}
	switch {
	case !found:
		return "the ToolServer " + t.ToolServerName + " is no longer configured, so " + t.Qualified() +
			" can no longer carry this out"
	case cfg.Access != AccessWrite:
		return "the ToolServer " + cfg.Name + " is no longer declared write, so its Tools carry out no Remedy"
	}
	for _, d := range listed {
		if d.Name == t.Tool {
			return ""
		}
	}
	return "the ToolServer " + cfg.Name + " no longer lists a Tool named " + quoteShort(t.Tool) +
		" at its last discovery, so it can no longer carry this out"
}

// HashArguments is the SHA-256 of a Remedy's argument bytes, as lower-case hex — the value
// `remedies_arguments_hash_ck` computes the same way.
func HashArguments(args string) string {
	sum := sha256.Sum256([]byte(args))
	return hex.EncodeToString(sum[:])
}

// RemedyDraft is one proposal a run made, checked, waiting for the run's Finding.
type RemedyDraft struct {
	Tool RemedyTool
	// Arguments is the compact JSON object; "" when no Tool is named.
	Arguments   string
	Target      string
	Description string
}

// ArgumentsSHA256 is the hash of the draft's arguments; "" when it names no Tool.
func (d RemedyDraft) ArgumentsSHA256() string {
	if !d.Tool.Named() {
		return ""
	}
	return HashArguments(d.Arguments)
}

// Key names what the draft would do, so one run cannot propose the same change twice.
func (d RemedyDraft) Key() string {
	return d.Tool.Qualified() + "\x00" + d.Arguments + "\x00" + d.Target
}

// invalidRemedy is the refusal a model hears for a proposal that does not hold.
func invalidRemedy(field, msg string) error {
	return errs.Validation("remedy_invalid", msg, errs.Violation{Field: field, Code: "invalid", Message: msg})
}

// NewRemedyDraft checks one proposal. A Tool, when named, has been checked against the org's
// write ToolServers by the caller; here the shape is held:
//
//   - with a Tool, the arguments are ONE JSON object of at most 16 KiB, kept compact — the
//     whitespace a model wrote is dropped, and nothing else about the bytes changes: key
//     order and every number literal are exactly as written (json.Compact never re-encodes);
//   - with no Tool, there are no arguments — arguments belong to a Tool;
//   - a target and a description are said, the target in at most 500 characters (refused
//     rather than cut: a clipped target names a different thing) and the description clipped
//     to 2000.
func NewRemedyDraft(tool RemedyTool, args json.RawMessage, target, description string) (RemedyDraft, error) {
	d := RemedyDraft{Tool: tool}
	raw := bytes.TrimSpace(args)
	if tool.Named() {
		if tool.ToolServerName == "" || tool.Tool == "" {
			return RemedyDraft{}, errs.New(errs.KindInternal, "remedy_tool_incomplete",
				"a Remedy's Tool names its ToolServer and the Tool")
		}
		if !isJSONObject(raw) {
			return RemedyDraft{}, invalidRemedy("arguments",
				"arguments are the one JSON object the write Tool would be sent, exactly")
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			return RemedyDraft{}, invalidRemedy("arguments", "arguments are not valid JSON")
		}
		if compact.Len() > MaxRemedyArgumentsBytes || !utf8.Valid(compact.Bytes()) {
			return RemedyDraft{}, invalidRemedy("arguments",
				fmt.Sprintf("arguments are at most %d bytes of UTF-8 JSON", MaxRemedyArgumentsBytes))
		}
		d.Arguments = compact.String()
	} else if len(raw) > 0 && string(raw) != "null" {
		return RemedyDraft{}, invalidRemedy("arguments",
			"arguments belong to a Tool: name the write Tool that would carry this out, or omit both")
	}
	t := strings.TrimSpace(target)
	switch {
	case t == "":
		return RemedyDraft{}, invalidRemedy("target", "say what the change is made to: the target, e.g. a Deployment and its namespace")
	case utf8.RuneCountInString(t) > MaxRemedyTarget:
		return RemedyDraft{}, invalidRemedy("target", fmt.Sprintf("the target is at most %d characters", MaxRemedyTarget))
	}
	desc := strings.TrimSpace(description)
	if desc == "" {
		return RemedyDraft{}, invalidRemedy("description",
			"describe the change and why, in words a human reads below the exact command before approving it")
	}
	d.Target, d.Description = t, clip(desc, MaxRemedyDescription)
	return d, nil
}

// RemedyApproval is one human's approval: who, when, and the hash of the arguments they
// were shown.
type RemedyApproval struct {
	UserID          uuid.UUID
	Label           string
	ArgumentsSHA256 string
	ApprovedAt      time.Time
}

// RemedyTransition is one move of one Remedy, by a named actor, and where it was declared.
type RemedyTransition struct {
	ID uuid.UUID
	// From is "" for the proposal itself.
	From    RemedyState
	To      RemedyState
	Actor   RemedyActor
	At      time.Time
	Failure RemedyFailure
	Detail  string
	// DeclaredIncidentID is the Incident the transition was declared to as a fact; uuid.Nil
	// when the Remedy's subject was in no Incident — recorded, never invented.
	DeclaredIncidentID uuid.UUID
}

// Remedy is one proposal as stored, with its approvals and — on a full read — its history.
type Remedy struct {
	ID              uuid.UUID
	OrgID           uuid.UUID
	InvestigationID uuid.UUID
	SubjectKind     SubjectKind
	SubjectID       uuid.UUID
	// ProposedBy names the Investigator and the version whose run proposed it.
	ProposedBy        string
	Tool              RemedyTool
	Arguments         string
	ArgumentsSHA256   string
	Target            string
	Description       string
	RequiredApprovals int
	// Risk is how RequiredApprovals was set: the rule, or why no rule could, and what the
	// risk model did. Zero for a Remedy with no Tool.
	Risk        RemedyRisk
	State       RemedyState
	ProposedAt  time.Time
	ExpiresAt   time.Time
	ApprovedAt  time.Time
	ExecutingAt time.Time
	EndedAt     time.Time
	Failure     RemedyFailure
	Detail      string
	// Result is what the write Tool answered, redacted and capped; "" until it was sent.
	Result      string
	Approvals   []RemedyApproval
	Transitions []RemedyTransition

	// Blocked is READ, NEVER STORED: why it cannot be approved now — no Tool, the Tool gone,
	// or not waiting for approval — and "" when it can. Filled by the service on every read,
	// so a screen offers an approve control only where one would be accepted.
	Blocked string
}

// StateAt is the Remedy's state at `now`: a proposed or approved Remedy past ExpiresAt is
// expired, recorded or not yet. The one place the rule is written.
func (r Remedy) StateAt(now time.Time) RemedyState {
	if (r.State == RemedyProposed || r.State == RemedyApproved) && !now.Before(r.ExpiresAt) {
		return RemedyExpired
	}
	return r.State
}

// OutcomeOverdue reports whether an `executing` Remedy has gone past RemedyOutcomeDeadline
// with no answer recorded.
func (r Remedy) OutcomeOverdue(now time.Time) bool {
	return r.State == RemedyExecuting && !now.Before(r.ExecutingAt.Add(RemedyOutcomeDeadline))
}

// Counted is how many DIFFERENT people have approved it. A user deleted since is not
// counted: their id is gone with them.
func (r Remedy) Counted() int {
	seen := map[uuid.UUID]bool{}
	for _, a := range r.Approvals {
		if a.UserID != uuid.Nil {
			seen[a.UserID] = true
		}
	}
	return len(seen)
}

// ApprovedBy reports whether this user has approved it already.
func (r Remedy) ApprovedBy(userID uuid.UUID) (RemedyApproval, bool) {
	for _, a := range r.Approvals {
		if userID != uuid.Nil && a.UserID == userID {
			return a, true
		}
	}
	return RemedyApproval{}, false
}

// ArgumentsDisplay is the arguments indented for reading. json.Indent changes whitespace
// only, so it shows exactly the keys, order and literals that are sent.
func (r Remedy) ArgumentsDisplay() string {
	if r.Arguments == "" {
		return ""
	}
	var b bytes.Buffer
	if err := json.Indent(&b, []byte(r.Arguments), "", "  "); err != nil {
		return r.Arguments
	}
	return b.String()
}

// Approvable refuses an approval the Remedy's own record rules out, typed for each reason —
// no Tool, expired, not waiting for approval. Whether its Tool can still carry it out is
// RemedyBinding's question, asked by the service against the configuration.
func (r Remedy) Approvable(now time.Time) error {
	if !r.Tool.Named() {
		return RemedyHasNoTool()
	}
	switch st := r.StateAt(now); st {
	case RemedyProposed:
		return nil
	case RemedyExpired:
		return RemedyExpiredErr(r)
	case RemedyApproved:
		return errs.Conflict("remedy_not_proposed",
			"this Remedy already has the approvals it needs and is waiting to be executed")
	default:
		return errs.Conflict("remedy_not_proposed", fmt.Sprintf("this Remedy is %s and can no longer be approved", st))
	}
}

// Declinable refuses a decline once the Remedy is past deciding: it is being sent, has ended,
// or has expired.
func (r Remedy) Declinable(now time.Time) error {
	switch st := r.StateAt(now); st {
	case RemedyProposed, RemedyApproved:
		return nil
	case RemedyExpired:
		return RemedyExpiredErr(r)
	case RemedyExecuting:
		return errs.Conflict("remedy_not_open",
			"this Remedy is being executed now and can no longer be declined")
	default:
		return errs.Conflict("remedy_not_open", fmt.Sprintf("this Remedy is %s and can no longer be declined", st))
	}
}

// RemedyNotFound is the one answer for a Remedy this org does not have.
func RemedyNotFound() error { return errs.NotFound("remedy_not_found", "no such Remedy") }

// RemedyHasNoTool refuses approving a Remedy that names no Tool. Typed, so a client branches
// on the code; the sentence is the one the Remedy says everywhere.
func RemedyHasNoTool() error {
	return errs.Conflict("remedy_has_no_tool",
		"this Remedy cannot be approved: "+NoToolCanCarryItOut+". Decline it, or make the change by hand")
}

// RemedyExpiredErr refuses acting on a Remedy whose window has passed.
func RemedyExpiredErr(r Remedy) error {
	return errs.Conflict("remedy_expired", fmt.Sprintf(
		"this Remedy expired at %s and can no longer be approved, declined or executed; ask for another "+
			"Investigation if the change still matters", r.ExpiresAt.UTC().Format(time.RFC3339)))
}

// RemedyToolUnavailable refuses approving a Remedy whose write Tool can no longer carry it out.
func RemedyToolUnavailable(why string) error {
	return errs.Conflict("remedy_tool_unavailable", "this Remedy cannot be approved: "+why)
}

// RemedyAlreadyApproved refuses a second approval by the same person: one person counts once,
// and the next approval must come from a different holder of the grant.
func RemedyAlreadyApproved(a RemedyApproval) error {
	return errs.Conflict("remedy_already_approved", fmt.Sprintf(
		"you approved this Remedy at %s; one person counts once, so the next approval must come from a different "+
			"holder of the grant on its ToolServer", a.ApprovedAt.UTC().Format(time.RFC3339)))
}

// RemedyArgumentsMismatch refuses an approval of arguments that are not this Remedy's: the
// screen the approver read is not the Remedy they would be approving.
func RemedyArgumentsMismatch() error {
	return errs.Conflict("remedy_arguments_changed",
		"the arguments you were shown are not this Remedy's arguments; read it again before approving")
}

// RemedyFact is one transition as it is declared outbound: the transition, and the Remedy as
// it stands once the transition is made — a snapshot, never re-read.
type RemedyFact struct {
	Transition RemedyTransition
	Remedy     Remedy
}
