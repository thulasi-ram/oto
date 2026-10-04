package service

// AN INVESTIGATOR PROPOSES A REMEDY, AND TWO DIFFERENT APPROVERS ON ITS TOOLSERVER MUST SAY YES
// (ADR 0054 §1, §2, §4, §5; git-bug 4148256).
//
// ⭐⭐ PROPOSING IS A BUILT-IN TOOL THE LOOP ANSWERS ITSELF, AND THE WRITE TOOL IS NEVER HELD.
// `oto_propose_remedy` is answered like `oto_classify` and the Suggestion Tools: the proposal is
// checked against the org's write ToolServers as the run reads them, a valid one is an `ok`
// Step kept for the Finding, and an invalid one is REFUSED on the record with the reason. It
// costs no step. `oto_write_tools` reads what the write ToolServers listed at their last
// discovery — oto's own record, not a call to any of them — so the model can name a Tool and
// shape its arguments. ⛔ Neither offers, holds or calls a write Tool: a write ToolServer's
// Tools are never in a run's offered set (toolServerTools refuses them), and nothing in this
// file dials a ToolServer at all.
//
// ⭐ A REMEDY BELONGS TO A FINDING. The proposals a run made are written in the transaction
// that records its Finding — none for a run that reached none — each `proposed` by the
// Investigator, with its proposal transition declared outbound in that same transaction.
//
// ⭐⭐ APPROVAL IS A HUMAN HOLDING THE GRANT ON THAT REMEDY'S TOOLSERVER, AND IT TAKES TWO.
// ApproveRemedy refuses, each with a typed error: a Remedy with no Tool (`remedy_has_no_tool`);
// one past its window (`remedy_expired`) or not waiting for approval (`remedy_not_proposed`);
// one whose Tool can no longer carry it out (`remedy_tool_unavailable` — checked against the
// configuration NOW, not as it stood at the proposal); a user without the grant
// (`remedy_approver_required`, identity's 403); a second approval by the same user
// (`remedy_already_approved` — one person counts once); and arguments other than the ones the
// approver was shown (`remedy_arguments_changed`). When the DIFFERENT approvers reach
// RequiredApprovals, the Remedy is `approved` by the one who completed it, its window restarts,
// and the transition goes outbound.
//
// ⭐ DECLINING IS ANY HUMAN'S, AND EXPIRY IS RECORDED BY oto. Saying no is the safe direction,
// so a member of the org may decline a proposed or approved Remedy that is not yet being sent;
// a Remedy with no Tool, which nobody can approve, can still be declined. ExpireRemedies is
// the `remedies.sweep` job: every Remedy still proposed or approved past its window is moved
// to `expired` by `system`, and declared.
//
// ⭐ EVERY TRANSITION GOES OUTBOUND, TO THE INCIDENT IT IS ABOUT (ADR 0052 §5). An Incident's
// Remedy is declared to that Incident; a Case's to the Incident holding the Case at the time;
// a Case in no Incident has no outbound target, and the transition records that rather than
// inventing one (`declared_incident_id` NULL).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// The two Remedy Tools' names. They are what an allowlist names to hold them.
const (
	ToolProposeRemedy = "oto_propose_remedy"
	ToolWriteTools    = "oto_write_tools"
)

// maxListedWriteTools bounds how many write Tools a refusal names back to the model.
const maxListedWriteTools = 20

// remedySweepBatch bounds one sweep's reads; the next tick takes the rest.
const remedySweepBatch = 500

// remedyProposingTool is a built-in Tool whose call proposes a Remedy: checked here and kept
// for the Finding, never run as a read and never as a write.
type remedyProposingTool interface {
	Tool
	proposeRemedy(ctx context.Context, scope db.TenantScope, args json.RawMessage) (domain.RemedyDraft, error)
}

func remedyTools(s *Service) []Tool {
	return []Tool{
		writeToolsTool{s: s, schema: mustSchema(ToolWriteTools,
			"The write Tools a Remedy may name: every Tool the operator's write ToolServers listed at their last "+
				"discovery, as <toolserver>__<tool>, with its description and its input schema. Reading this changes "+
				"nothing and calls no ToolServer. You never call a write Tool; you name one in "+ToolProposeRemedy+".",
			"")},
		proposeRemedyTool{s: s, schema: mustSchema(ToolProposeRemedy,
			"Propose a Remedy: a change to the cluster that a human may approve and oto then executes through the "+
				"named write Tool, with exactly the arguments you give. You never execute it. Name the write Tool as "+
				"<toolserver>__<tool>, exactly as "+ToolWriteTools+" lists it, with the exact arguments it would be "+
				"sent. If no configured Tool can make the change, omit tool and arguments: the Remedy then says that no "+
				"configured Tool can carry it out, cannot be approved, and stands as advice a human may act on by hand. "+
				"Two different people holding the approval grant on the Tool's ToolServer must approve it before it runs.",
			`{"type":"object","properties":{`+
				`"tool":{"type":"string","minLength":4,"maxLength":160,"description":"The write Tool that would carry the change out, as <toolserver>__<tool>. Omit it when no configured Tool can."},`+
				`"arguments":{"type":"object","description":"The exact arguments the write Tool would be sent, as its input schema asks. Omit them when tool is omitted."},`+
				`"target":{"type":"string","minLength":1,"maxLength":500,"description":"What the change is made to, e.g. Deployment checkout/api in cluster prod-eu."},`+
				`"description":{"type":"string","minLength":1,"maxLength":2000,"description":"What the change does and why you propose it, in words a human reads after the exact command and before approving it."}`+
				`},"required":["target","description"],"additionalProperties":false}`)},
	}
}

// answerRemedy answers one `oto_propose_remedy` call and, when the proposal holds, keeps it for
// the run's Finding. It never fails the run: a proposal that does not hold is refused, with
// the reason, and the model hears it before its next turn.
func (s *Service) answerRemedy(
	ctx context.Context, p plan, tool remedyProposingTool, call domain.ToolCall, out *outcome,
) (domain.ToolOutcome, string) {
	if len(out.remedies) >= domain.MaxRemediesPerRun {
		return domain.OutcomeRefused, fmt.Sprintf(
			"refused: an Investigation proposes at most %d Remedies, and this one has", domain.MaxRemediesPerRun)
	}
	args := strings.TrimSpace(call.Arguments)
	if args == "" {
		args = "{}"
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &probe) != nil || probe == nil {
		return domain.OutcomeRefused, "refused: the arguments are not one JSON object"
	}
	callCtx, cancel := context.WithTimeout(ctx, s.limits.ToolTimeout)
	defer cancel()
	draft, err := tool.proposeRemedy(callCtx, p.scope, json.RawMessage(args))
	switch {
	case err == nil:
	case errs.IsKind(err, errs.KindValidation):
		return domain.OutcomeRefused, "refused: " + safeMessage(err)
	default:
		return domain.OutcomeFailed, "failed: " + safeMessage(err)
	}
	for _, kept := range out.remedies {
		if kept.Key() == draft.Key() {
			return domain.OutcomeRefused, "refused: this Investigation already proposed that Remedy"
		}
	}
	out.remedies = append(out.remedies, draft)
	if !draft.Tool.Named() {
		return domain.OutcomeOK, fmt.Sprintf("recorded: Remedy %d of at most %d. It says %s, so nobody can approve it; "+
			"it is kept with your Finding as advice a human may act on by hand, or decline, and it expires.",
			len(out.remedies), domain.MaxRemediesPerRun, domain.NoToolCanCarryItOut)
	}
	return domain.OutcomeOK, fmt.Sprintf("recorded: Remedy %d of at most %d, through %s. It is kept with your Finding; "+
		"it runs only if %d different people holding the approval grant on %s approve exactly these arguments, and "+
		"you never execute it.", len(out.remedies), domain.MaxRemediesPerRun, draft.Tool.Qualified(),
		domain.DefaultRequiredApprovals, draft.Tool.ToolServerName)
}

// ------------------------------------------------------------ the write Tools listing

// writeToolsTool lists what the write ToolServers listed. ⛔ IT READS oto'S RECORD AND DIALS
// NOTHING: a listing is not a call, and a write ToolServer is never reached from a run.
type writeToolsTool struct {
	caseOrIncident
	s      *Service
	schema domain.ToolSchema
}

func (t writeToolsTool) Schema() domain.ToolSchema { return t.schema }

func (t writeToolsTool) Call(ctx context.Context, scope db.TenantScope, _ RunSubject, _ json.RawMessage) (string, error) {
	type tool struct {
		Tool        string          `json:"tool"`
		Description string          `json:"description,omitempty"`
		InputSchema json.RawMessage `json:"input_schema,omitempty"`
	}
	servers, err := t.s.toolServers.List(ctx, scope)
	if err != nil {
		return "", err
	}
	out := []tool{}
	for _, cfg := range servers {
		if cfg.Access != domain.AccessWrite {
			continue
		}
		listed, err := t.s.toolServers.Tools(ctx, scope, cfg.ID)
		if err != nil {
			return "", err
		}
		for _, d := range listed {
			out = append(out, tool{Tool: cfg.Name + domain.QualifiedToolSeparator + d.Name,
				Description: d.Description, InputSchema: d.InputSchema})
		}
	}
	answer := map[string]any{"write_tools": out}
	if len(out) == 0 {
		answer["note"] = "no write ToolServer has listed a Tool. A Remedy can still be proposed with no tool; it then says " +
			domain.NoToolCanCarryItOut + " and cannot be approved"
	}
	return asJSON(answer)
}

// ------------------------------------------------------------ proposing

type proposeRemedyTool struct {
	caseOrIncident
	s      *Service
	schema domain.ToolSchema
}

func (t proposeRemedyTool) Schema() domain.ToolSchema { return t.schema }

func (t proposeRemedyTool) Call(context.Context, db.TenantScope, RunSubject, json.RawMessage) (string, error) {
	return "", errs.Internal("remedy_tool_called", errAnsweredByLoop)
}

// proposeRemedy checks one proposal against the org's configuration as the run reads it: the
// Tool it names must be one a WRITE ToolServer listed at its last discovery. A name that is not
// one is refused with the write Tools that are, so the model's next call can name one — or
// propose with no Tool.
func (t proposeRemedyTool) proposeRemedy(ctx context.Context, scope db.TenantScope, args json.RawMessage) (domain.RemedyDraft, error) {
	var in struct {
		Tool        *string         `json:"tool"`
		Arguments   json.RawMessage `json:"arguments"`
		Target      string          `json:"target"`
		Description string          `json:"description"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return domain.RemedyDraft{}, errs.Validation("remedy_invalid",
			"tool, target and description are strings, and arguments is one JSON object")
	}
	var tool domain.RemedyTool
	if in.Tool != nil && strings.TrimSpace(*in.Tool) != "" {
		var err error
		if tool, err = t.s.writeTool(ctx, scope, strings.TrimSpace(*in.Tool)); err != nil {
			return domain.RemedyDraft{}, err
		}
	}
	return domain.NewRemedyDraft(tool, in.Arguments, in.Target, in.Description)
}

// writeTool resolves a qualified name to a write Tool the org has: a write ToolServer of that
// name, listing a Tool of that name at its last discovery.
func (s *Service) writeTool(ctx context.Context, scope db.TenantScope, qualified string) (domain.RemedyTool, error) {
	server, name, ok := domain.SplitQualifiedToolName(qualified)
	if !ok {
		return domain.RemedyTool{}, errs.Validation("remedy_invalid", fmt.Sprintf(
			"%q is not <toolserver>__<tool>. %s", qualified, s.writeToolsSentence(ctx, scope)))
	}
	servers, err := s.toolServers.ByNames(ctx, scope, []string{server})
	if err != nil {
		return domain.RemedyTool{}, err
	}
	if len(servers) == 0 {
		return domain.RemedyTool{}, errs.Validation("remedy_invalid", fmt.Sprintf(
			"no ToolServer named %s is configured. %s", server, s.writeToolsSentence(ctx, scope)))
	}
	cfg := servers[0]
	if cfg.Access != domain.AccessWrite {
		return domain.RemedyTool{}, errs.Validation("remedy_invalid", fmt.Sprintf(
			"%s is a read ToolServer, and a Remedy is carried out by a write Tool. %s", server, s.writeToolsSentence(ctx, scope)))
	}
	listed, err := s.toolServers.Tools(ctx, scope, cfg.ID)
	if err != nil {
		return domain.RemedyTool{}, err
	}
	for _, d := range listed {
		if d.Name == name {
			return domain.RemedyTool{ToolServerID: cfg.ID, ToolServerName: cfg.Name, Tool: name}, nil
		}
	}
	return domain.RemedyTool{}, errs.Validation("remedy_invalid", fmt.Sprintf(
		"the write ToolServer %s has not listed a Tool named %q. %s", server, name, s.writeToolsSentence(ctx, scope)))
}

// writeToolsSentence is the re-ask a refusal ends with: the write Tools there are, or that
// there are none and a Remedy may name no Tool.
func (s *Service) writeToolsSentence(ctx context.Context, scope db.TenantScope) string {
	none := "No write ToolServer has listed a Tool; propose with no tool, and the Remedy says " + domain.NoToolCanCarryItOut + "."
	servers, err := s.toolServers.List(ctx, scope)
	if err != nil {
		return none
	}
	var names []string
	for _, cfg := range servers {
		if cfg.Access != domain.AccessWrite {
			continue
		}
		listed, err := s.toolServers.Tools(ctx, scope, cfg.ID)
		if err != nil {
			continue
		}
		for _, d := range listed {
			if len(names) < maxListedWriteTools {
				names = append(names, cfg.Name+domain.QualifiedToolSeparator+d.Name)
			}
		}
	}
	if len(names) == 0 {
		return none
	}
	return "The write Tools a Remedy may name: " + strings.Join(names, ", ") + ". Or omit tool if none of them can make the change."
}

// proposeRemedies writes the Remedies a run proposed, in the transaction that records its
// Finding: each `proposed` by the Investigator, with RequiredApprovals two and its window
// starting at the Finding, and each proposal declared outbound.
func (s *Service) proposeRemedies(
	ctx context.Context, scope db.TenantScope, inv domain.Investigation, drafts []domain.RemedyDraft,
	at time.Time, window time.Duration,
) error {
	if window <= 0 {
		window = domain.DefaultRemedyApprovalWindow
	}
	by := fmt.Sprintf("Investigator %s v%d", inv.InvestigatorName, inv.VersionNumber)
	for i, d := range drafts {
		// ⭐ PROPOSED IN ORDER: a microsecond apart, so the run's order survives the
		// `(proposed_at, id)` sort the read uses.
		proposed := at.UTC().Add(time.Duration(i) * time.Microsecond)
		r := domain.Remedy{
			ID: id.New(), OrgID: scope.OrgID(), InvestigationID: inv.ID,
			SubjectKind: inv.SubjectKind, SubjectID: inv.SubjectID, ProposedBy: by,
			Tool: d.Tool, Arguments: d.Arguments, ArgumentsSHA256: d.ArgumentsSHA256(),
			Target: d.Target, Description: d.Description,
			RequiredApprovals: domain.DefaultRequiredApprovals,
			State:             domain.RemedyProposed, ProposedAt: proposed, ExpiresAt: proposed.Add(window),
			Approvals: []domain.RemedyApproval{},
		}
		t := domain.RemedyTransition{ID: id.New(), To: domain.RemedyProposed,
			Actor: domain.RemedyActor{Kind: domain.ActorInvestigator, Label: by}, At: proposed}
		incident, err := s.remedyIncident(ctx, scope, r)
		if err != nil {
			return err
		}
		t.DeclaredIncidentID = incident
		if err := s.remedies.InsertRemedy(ctx, scope, r, t); err != nil {
			return err
		}
		r.Transitions = []domain.RemedyTransition{t}
		if err := s.declareRemedy(ctx, scope, r, t); err != nil {
			return err
		}
	}
	return nil
}

// remedyIncident is the Incident a Remedy's transition is declared to NOW: its own Incident
// when it is about one (and that Incident still exists), the one holding its Case when it is
// about a Case, and uuid.Nil — no outbound target — otherwise.
func (s *Service) remedyIncident(ctx context.Context, scope db.TenantScope, r domain.Remedy) (uuid.UUID, error) {
	switch r.SubjectKind {
	case domain.SubjectIncident:
		if _, err := s.incidents.InvestigationIncident(ctx, scope, r.SubjectID); err != nil {
			if errs.IsKind(err, errs.KindNotFound) {
				return uuid.Nil, nil
			}
			return uuid.Nil, err
		}
		return r.SubjectID, nil
	case domain.SubjectCase:
		return s.incidents.HoldingIncident(ctx, scope, r.SubjectID)
	default:
		return uuid.Nil, nil
	}
}

// declareRemedy declares one transition to the Incident it records, when it records one.
func (s *Service) declareRemedy(ctx context.Context, scope db.TenantScope, r domain.Remedy, t domain.RemedyTransition) error {
	if t.DeclaredIncidentID == uuid.Nil || !t.To.Declared() {
		return nil
	}
	return s.remedyDeclarer.DeclareRemedy(ctx, scope, t.DeclaredIncidentID, domain.RemedyFact{Transition: t, Remedy: r})
}

// moveRemedy makes one transition — the row, its record, and its declaration — in the caller's
// transaction, and returns the Remedy as it now stands.
func (s *Service) moveRemedy(
	ctx context.Context, scope db.TenantScope, r domain.Remedy, to domain.RemedyState, actor domain.RemedyActor,
	at time.Time, failure domain.RemedyFailure, detail string, expiresAt time.Time, result string,
) (domain.Remedy, error) {
	t := domain.RemedyTransition{ID: id.New(), From: r.State, To: to, Actor: actor, At: at,
		Failure: failure, Detail: clipText(strings.TrimSpace(detail), domain.MaxRemedyDetail)}
	if to.Declared() {
		incident, err := s.remedyIncident(ctx, scope, r)
		if err != nil {
			return domain.Remedy{}, err
		}
		t.DeclaredIncidentID = incident
	}
	if err := s.remedies.Transition(ctx, scope, r.ID, t, expiresAt, result); err != nil {
		return domain.Remedy{}, err
	}
	stamp := func(cur time.Time, floor ...time.Time) time.Time {
		out := cur
		for _, f := range floor {
			if f.After(out) {
				out = f
			}
		}
		return out
	}
	switch to {
	case domain.RemedyApproved:
		r.ApprovedAt = stamp(at, r.ProposedAt)
	case domain.RemedyExecuting:
		r.ExecutingAt = stamp(at, r.ApprovedAt)
	}
	if to.Terminal() {
		r.EndedAt = stamp(at, r.ProposedAt, r.ApprovedAt, r.ExecutingAt)
	}
	if !expiresAt.IsZero() {
		r.ExpiresAt = expiresAt.UTC()
	}
	r.State, r.Failure = to, failure
	if t.Detail != "" {
		r.Detail = t.Detail
	}
	if result != "" {
		r.Result = result
	}
	r.Transitions = append(r.Transitions, t)
	if err := s.declareRemedy(ctx, scope, r, t); err != nil {
		return domain.Remedy{}, err
	}
	return r, nil
}

// ------------------------------------------------------------ read

// ListRemedies reads one Investigation's Remedies, in the order proposed, each saying — as it
// is read — whether its Tool can still carry it out. An Investigation this org does not have
// is a 404, like the Investigation itself.
func (s *Service) ListRemedies(ctx context.Context, scope db.TenantScope, investigationID uuid.UUID) ([]domain.Remedy, error) {
	if err := db.RequireScope(scope); err != nil {
		return nil, err
	}
	if _, err := s.investigations.Get(ctx, scope, investigationID); err != nil {
		return nil, err
	}
	list, err := s.remedies.ListRemedies(ctx, scope, investigationID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	for i := range list {
		if list[i].Blocked, err = s.remedyBlocked(ctx, scope, list[i], now); err != nil {
			return nil, err
		}
	}
	return list, nil
}

// GetRemedy reads one Remedy, with its approvals and history.
func (s *Service) GetRemedy(ctx context.Context, scope db.TenantScope, id uuid.UUID) (domain.Remedy, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.Remedy{}, err
	}
	r, err := s.remedies.GetRemedy(ctx, scope, id)
	if errs.IsKind(err, errs.KindNotFound) {
		return domain.Remedy{}, domain.RemedyNotFound()
	}
	if err != nil {
		return domain.Remedy{}, err
	}
	r.Blocked, err = s.remedyBlocked(ctx, scope, r, s.now())
	return r, err
}

// remedyBlocked is why a Remedy's Tool cannot carry it out now — NoToolCanCarryItOut for one
// that names none, whatever its state — or "". A Remedy that ended is not asked about its Tool.
func (s *Service) remedyBlocked(ctx context.Context, scope db.TenantScope, r domain.Remedy, now time.Time) (string, error) {
	if !r.Tool.Named() {
		return domain.NoToolCanCarryItOut, nil
	}
	if st := r.StateAt(now); st != domain.RemedyProposed && st != domain.RemedyApproved {
		return "", nil
	}
	return s.remedyBinding(ctx, scope, r.Tool)
}

// remedyBinding asks the configuration as it stands NOW whether a Remedy's write Tool can carry
// it out: "" when it can, and why not otherwise.
func (s *Service) remedyBinding(ctx context.Context, scope db.TenantScope, t domain.RemedyTool) (string, error) {
	if !t.Named() {
		return domain.NoToolCanCarryItOut, nil
	}
	cfg, err := s.toolServers.Get(ctx, scope, t.ToolServerID)
	switch {
	case errs.IsKind(err, errs.KindNotFound):
		return domain.RemedyBinding(t, domain.ToolServerConfig{}, false, nil), nil
	case err != nil:
		return "", err
	}
	listed, err := s.toolServers.Tools(ctx, scope, cfg.ID)
	if err != nil {
		return "", err
	}
	return domain.RemedyBinding(t, cfg, true, listed), nil
}

// ------------------------------------------------------------ approve and decline

// ApproveRemedy is one human approving one Remedy, having been shown the arguments whose hash
// they send. The checks, in order, each a typed refusal and none of them a write: the Remedy
// names a Tool and is waiting for approval; its Tool can carry it out NOW; the human holds the
// grant on its ToolServer; they have not approved it already; and the arguments they were
// shown are its arguments. Then the approval is recorded, and when DIFFERENT approvers reach
// its RequiredApprovals it is `approved` — its window to execution starts — and declared.
func (s *Service) ApproveRemedy(
	ctx context.Context, scope db.TenantScope, remedyID uuid.UUID, by domain.Requester, argumentsSHA256 string,
) (domain.Remedy, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.Remedy{}, err
	}
	if by.UserID == uuid.Nil {
		return domain.Remedy{}, errs.Forbidden("remedy_approver_required",
			"a Remedy is approved by a person holding the approval grant on its ToolServer")
	}
	controls, err := s.orgControls.InvestigationControls(ctx, scope)
	if err != nil {
		return domain.Remedy{}, err
	}
	var out domain.Remedy
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		r, err := s.remedies.LockRemedy(ctx, scope, remedyID)
		if errs.IsKind(err, errs.KindNotFound) {
			return domain.RemedyNotFound()
		}
		if err != nil {
			return err
		}
		now := s.now()
		if err := r.Approvable(now); err != nil {
			return err
		}
		why, err := s.remedyBinding(ctx, scope, r.Tool)
		if err != nil {
			return err
		}
		if why != "" {
			return domain.RemedyToolUnavailable(why)
		}
		if err := s.approvers.RequireRemedyApprover(ctx, scope, r.Tool.ToolServerID, by.UserID); err != nil {
			return err
		}
		if a, ok := r.ApprovedBy(by.UserID); ok {
			return domain.RemedyAlreadyApproved(a)
		}
		if strings.TrimSpace(argumentsSHA256) != r.ArgumentsSHA256 {
			return domain.RemedyArgumentsMismatch()
		}
		a := domain.RemedyApproval{UserID: by.UserID, Label: by.Label, ArgumentsSHA256: r.ArgumentsSHA256, ApprovedAt: now}
		if err := s.remedies.AddApproval(ctx, scope, r.ID, a); err != nil {
			return err
		}
		r.Approvals = append(r.Approvals, a)
		if r.Counted() >= r.RequiredApprovals {
			if r, err = s.moveRemedy(ctx, scope, r, domain.RemedyApproved, domain.UserActor(by), now, "", "",
				now.Add(controls.RemedyWindow()), ""); err != nil {
				return err
			}
		}
		out = r
		return nil
	})
	if err != nil {
		return domain.Remedy{}, err
	}
	out.Blocked, err = s.remedyBlocked(ctx, scope, out, s.now())
	return out, err
}

// DeclineRemedy is one human saying no to a Remedy that is proposed or approved and not yet
// being sent. A Remedy no configured Tool can carry out is declinable like any other.
func (s *Service) DeclineRemedy(ctx context.Context, scope db.TenantScope, remedyID uuid.UUID, by domain.Requester) (domain.Remedy, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.Remedy{}, err
	}
	if by.UserID == uuid.Nil {
		return domain.Remedy{}, errs.Forbidden("forbidden", "declining a Remedy requires a human actor")
	}
	var out domain.Remedy
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		r, err := s.remedies.LockRemedy(ctx, scope, remedyID)
		if errs.IsKind(err, errs.KindNotFound) {
			return domain.RemedyNotFound()
		}
		if err != nil {
			return err
		}
		now := s.now()
		if err := r.Declinable(now); err != nil {
			return err
		}
		out, err = s.moveRemedy(ctx, scope, r, domain.RemedyDeclined, domain.UserActor(by), now, "", "", time.Time{}, "")
		return err
	})
	if err != nil {
		return domain.Remedy{}, err
	}
	out.Blocked, err = s.remedyBlocked(ctx, scope, out, s.now())
	return out, err
}

// ------------------------------------------------------------ expiry

// ExpireRemedies is the `remedies.sweep` job's expiry: every Remedy still proposed or approved
// whose window has passed is moved to `expired` by `system`, one transaction each, and the
// transition is declared like any other. It returns how many it recorded.
//
// ⭐ RECORDED, NEVER SILENT. A Remedy past its window already reads as expired (StateAt), so
// nothing can approve or execute it in the meantime; this writes the fact down and sends it.
func (s *Service) ExpireRemedies(ctx context.Context, scope db.TenantScope) (int, error) {
	if err := db.RequireScope(scope); err != nil {
		return 0, err
	}
	ids, err := s.remedies.PastDeadline(ctx, scope, s.now(), remedySweepBatch)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, rid := range ids {
		err := s.tx.InTx(ctx, func(ctx context.Context) error {
			r, err := s.remedies.LockRemedy(ctx, scope, rid)
			if err != nil {
				return err
			}
			now := s.now()
			if r.State != domain.RemedyProposed && r.State != domain.RemedyApproved || r.StateAt(now) != domain.RemedyExpired {
				return nil // moved meanwhile: approved, declined or claimed before its deadline.
			}
			detail := "nobody approved it within the approval window"
			if r.State == domain.RemedyApproved {
				detail = "it was approved, and not executed within the approval window"
			}
			if _, err := s.moveRemedy(ctx, scope, r, domain.RemedyExpired, domain.SystemActor(), now, "", detail,
				time.Time{}, ""); err != nil {
				return err
			}
			n++
			return nil
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
