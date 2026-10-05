package service

// A FINDING SUGGESTS, AND A HUMAN APPLIES IT OR IT LAPSES (ADR 0053 §2, ADR 0052 §2 and §4;
// git-bug 8327c00).
//
// ⭐ PROPOSING IS TWO BUILT-IN TOOLS, AND THE LOOP ANSWERS THEM ITSELF.
// `oto_suggest_count_condition` proposes a notification policy's count condition (ADR 0044);
// `oto_suggest_membership` proposes a Case for an Incident. Like every built-in Tool an
// Investigator holds one only when its allowlist names it. Like `oto_classify` the loop
// answers the call — the proposal is checked against the org as the run reads it, a valid
// one is recorded as an `ok` Step and kept for the Finding, and an invalid one is REFUSED on
// the record with the reason, so the model's next turn can correct it. A proposal is the
// shape of the answer, not a look at the cluster: it costs no step and is not one of the
// run's Tool calls, and at most domain.MaxSuggestionsPerRun are taken.
//
// ⭐ A SUGGESTION BELONGS TO A FINDING. The proposals a run made are written in the
// transaction that records its Finding — none for a run that reached none — and each lapses
// domain.SuggestionLapse after it.
//
// ⛔⛔ THE INVESTIGATOR NEVER APPLIES ONE. Nothing in a run calls PolicyEditor's or
// MembershipEditor's write method; ApplySuggestion is reached only from the API, on a
// human's request, and it makes the ORDINARY edit — the same service method the human's own
// policy PATCH or Incident add/move goes through — with the applier as actor and the
// Investigation as provenance.
//
// ⛔ THERE IS NO VERB BUT APPLY. An unapplied Suggestion lapses on the clock and stops being
// shown; nothing here declines one, and nothing routes one to anybody (H-1).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// The two proposing Tools' names. They are what an allowlist names to hold them.
const (
	ToolSuggestCountCondition = "oto_suggest_count_condition"
	ToolSuggestMembership     = "oto_suggest_membership"
)

// maxNamedPolicies bounds how many policy names a refusal lists back to the model.
const maxNamedPolicies = 20

// proposingTool is a built-in Tool whose call is a proposal the loop answers itself: it is
// checked here and kept for the Finding, never run as a read.
type proposingTool interface {
	Tool
	propose(ctx context.Context, scope db.TenantScope, run RunSubject, args json.RawMessage) (domain.SuggestionDraft, string, error)
}

// errAnsweredByLoop is what a proposing Tool's Call says if anything ever reaches it: the
// loop answers a proposal before the Tool-call path, so this is an oto bug, said as one.
var errAnsweredByLoop = errors.New("a proposal is answered by the loop, never called as a Tool")

func proposingTools(s *Service) []Tool {
	const why = `"why":{"type":"string","minLength":1,"maxLength":1000,"description":"One or two sentences a human reads before deciding to apply it: what you saw that makes this the right change."}`
	return []Tool{
		countSuggestionTool{s: s, schema: mustSchema(ToolSuggestCountCondition,
			"Suggest a count condition for one notification policy: stay silent until count_min of its Cases have "+
				"happened inside count_window_seconds. A human applies it or it lapses after seven days; you never "+
				"apply it. Use it only when what you found shows this policy's messages are noise below a threshold.",
			`{"type":"object","properties":{`+
				`"policy":{"type":"string","minLength":1,"maxLength":120,"description":"The policy's name, exactly as the operator wrote it."},`+
				`"count_min":{"type":"integer","minimum":2,"maximum":10000,"description":"How many Cases must happen inside the window before the policy speaks."},`+
				`"count_window_seconds":{"type":"integer","minimum":60,"maximum":86400,"description":"The window, in seconds."},`+
				why+`},"required":["policy","count_min","count_window_seconds","why"],"additionalProperties":false}`)},
		membershipSuggestionTool{s: s, schema: mustSchema(ToolSuggestMembership,
			"Suggest that a Case belongs in an Incident. Investigating an Incident: name the Case to add to it by "+
				"case_id. Investigating a Case: name the Incident it belongs in by incident_number. A Case already in "+
				"another Incident would be moved, and the human is told so before applying it. A human applies it or "+
				"it lapses after seven days; you never apply it.",
			`{"type":"object","properties":{`+
				`"case_id":{"type":"string","format":"uuid","description":"Investigating an Incident: the Case to add to it."},`+
				`"incident_number":{"type":"integer","minimum":1,"description":"Investigating a Case: the Incident it belongs in."},`+
				why+`},"required":["why"],"additionalProperties":false}`)},
	}
}

// answerSuggestion answers one proposing Tool call and, when the proposal holds, keeps it
// for the run's Finding. It never fails the run: a proposal that does not hold is refused,
// with the reason, and the model hears it before its next turn.
func (s *Service) answerSuggestion(
	ctx context.Context, p plan, tool proposingTool, call domain.ToolCall, out *outcome,
) (domain.ToolOutcome, string) {
	if len(out.suggestions) >= domain.MaxSuggestionsPerRun {
		return domain.OutcomeRefused, fmt.Sprintf(
			"refused: an Investigation suggests at most %d changes, and this one has", domain.MaxSuggestionsPerRun)
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
	draft, note, err := tool.propose(callCtx, p.scope, p.run, json.RawMessage(args))
	switch {
	case err == nil:
	case errs.IsKind(err, errs.KindValidation):
		return domain.OutcomeRefused, "refused: " + safeMessage(err)
	default:
		return domain.OutcomeFailed, "failed: " + safeMessage(err)
	}
	for _, kept := range out.suggestions {
		if kept.Target() == draft.Target() {
			return domain.OutcomeRefused, "refused: this Investigation already suggested that change"
		}
	}
	out.suggestions = append(out.suggestions, draft)
	msg := fmt.Sprintf("recorded: Suggestion %d of at most %d. It is kept with your Finding; a human applies it "+
		"or it lapses after %d days. You do not apply it.",
		len(out.suggestions), domain.MaxSuggestionsPerRun, int(domain.SuggestionLapse/(24*time.Hour)))
	if note != "" {
		msg += " " + note
	}
	return domain.OutcomeOK, msg
}

// ------------------------------------------------------------ count condition

type countSuggestionTool struct {
	s      *Service
	schema domain.ToolSchema
}

func (t countSuggestionTool) Schema() domain.ToolSchema { return t.schema }

func (t countSuggestionTool) Call(context.Context, db.TenantScope, RunSubject, json.RawMessage) (string, error) {
	return "", errs.Internal("suggestion_tool_called", errAnsweredByLoop)
}

func (t countSuggestionTool) propose(
	ctx context.Context, scope db.TenantScope, _ RunSubject, args json.RawMessage,
) (domain.SuggestionDraft, string, error) {
	var in struct {
		Policy             string `json:"policy"`
		CountMin           *int   `json:"count_min"`
		CountWindowSeconds *int   `json:"count_window_seconds"`
		Why                string `json:"why"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return domain.SuggestionDraft{}, "", errs.Validation("suggestion_invalid",
			"policy is a string; count_min and count_window_seconds are integers")
	}
	name := strings.TrimSpace(in.Policy)
	if name == "" || in.CountMin == nil || in.CountWindowSeconds == nil {
		return domain.SuggestionDraft{}, "", errs.Validation("suggestion_invalid",
			"policy, count_min and count_window_seconds are all required")
	}
	policies, err := t.s.policies.SuggestionPolicies(ctx, scope)
	if err != nil {
		return domain.SuggestionDraft{}, "", err
	}
	for _, p := range policies {
		if p.Name == name {
			draft, err := domain.NewCountSuggestion(p, *in.CountMin, time.Duration(*in.CountWindowSeconds)*time.Second, in.Why)
			return draft, "", err
		}
	}
	return domain.SuggestionDraft{}, "", errs.Validation("suggestion_invalid", unknownPolicy(name, policies))
}

// unknownPolicy is the refusal for a name no live policy has, and it is the re-ask: it
// lists the policies a count condition can sit on, so the model's next call can name one.
func unknownPolicy(name string, policies []domain.PolicyTarget) string {
	var countable []string
	for _, p := range policies {
		if p.Countable() && len(countable) < maxNamedPolicies {
			countable = append(countable, p.Name)
		}
	}
	if len(countable) == 0 {
		return fmt.Sprintf("no live notification policy is named %q, and none of this organisation's policies is "+
			"bound to exactly the %s subject, so no count condition can be suggested", name, domain.CountSubject)
	}
	return fmt.Sprintf("no live notification policy is named %q. The policies a count condition can sit on: %s",
		name, strings.Join(countable, ", "))
}

// ---------------------------------------------------------------- membership

type membershipSuggestionTool struct {
	// A membership is between a Case and an Incident, and one side is always the run's
	// own subject — so a digest window's run is not offered it (git-bug 3e96f5a).
	caseOrIncident
	s      *Service
	schema domain.ToolSchema
}

func (t membershipSuggestionTool) Schema() domain.ToolSchema { return t.schema }

func (t membershipSuggestionTool) Call(context.Context, db.TenantScope, RunSubject, json.RawMessage) (string, error) {
	return "", errs.Internal("suggestion_tool_called", errAnsweredByLoop)
}

// propose checks a membership proposal. ⭐ ONE SIDE IS ALWAYS THE RUN'S OWN SUBJECT: an
// Incident's run proposes Cases for that Incident, and a Case's run proposes an Incident for
// that Case — so a model cannot propose a change between two things it was not asked about.
func (t membershipSuggestionTool) propose(
	ctx context.Context, scope db.TenantScope, run RunSubject, args json.RawMessage,
) (domain.SuggestionDraft, string, error) {
	var in struct {
		CaseID         string `json:"case_id"`
		IncidentNumber *int64 `json:"incident_number"`
		Why            string `json:"why"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return domain.SuggestionDraft{}, "", errs.Validation("suggestion_invalid",
			"case_id is a string and incident_number an integer")
	}
	var (
		incident domain.IncidentSubject
		c        domain.CaseSubject
	)
	switch run.Kind {
	case domain.SubjectIncident:
		if in.IncidentNumber != nil && *in.IncidentNumber != run.Incident.Number {
			return domain.SuggestionDraft{}, "", errs.Validation("suggestion_invalid", fmt.Sprintf(
				"investigating Incident #%d, you suggest Cases for it and no other Incident; omit incident_number",
				run.Incident.Number))
		}
		caseID, err := uuid.Parse(strings.TrimSpace(in.CaseID))
		if err != nil {
			return domain.SuggestionDraft{}, "", errs.Validation("suggestion_invalid",
				"name the Case to add by case_id, the id oto gave it")
		}
		c, err = t.s.cases.InvestigationCase(ctx, scope, caseID)
		if errs.IsKind(err, errs.KindNotFound) {
			return domain.SuggestionDraft{}, "", errs.Validation("suggestion_invalid", "no Case in this organisation has that id")
		}
		if err != nil {
			return domain.SuggestionDraft{}, "", err
		}
		incident = run.Incident
	default:
		if in.CaseID != "" && in.CaseID != run.Case.CaseID.String() {
			return domain.SuggestionDraft{}, "", errs.Validation("suggestion_invalid", fmt.Sprintf(
				"investigating Case #%d, you suggest an Incident for it and no other Case; omit case_id", run.Case.Number))
		}
		if in.IncidentNumber == nil || *in.IncidentNumber < 1 {
			return domain.SuggestionDraft{}, "", errs.Validation("suggestion_invalid",
				"name the Incident this Case belongs in by incident_number")
		}
		var err error
		incident, err = t.s.incidents.InvestigationIncidentNumbered(ctx, scope, *in.IncidentNumber)
		if errs.IsKind(err, errs.KindNotFound) {
			return domain.SuggestionDraft{}, "", errs.Validation("suggestion_invalid",
				fmt.Sprintf("this organisation has no Incident #%d", *in.IncidentNumber))
		}
		if err != nil {
			return domain.SuggestionDraft{}, "", err
		}
		c = run.Case
	}
	draft, err := domain.NewMembershipSuggestion(incident, c, in.Why)
	if err != nil {
		return domain.SuggestionDraft{}, "", err
	}
	from, err := t.s.movesFrom(ctx, scope, draft.Membership)
	if err != nil {
		return domain.SuggestionDraft{}, "", err
	}
	note := ""
	if !from.IsZero() {
		note = fmt.Sprintf("Case #%d is in Incident #%d now, so applying it would move the Case, and the human "+
			"applying it is told so first.", c.Number, from.Number)
	}
	return draft, note, nil
}

// movesFrom is the Incident a membership change would move its Case FROM: the one the Case
// is in now, when that is not the Incident proposed. Zero when applying it would add.
func (s *Service) movesFrom(ctx context.Context, scope db.TenantScope, m domain.MembershipChange) (domain.IncidentRef, error) {
	holding, err := s.incidents.HoldingIncident(ctx, scope, m.CaseID)
	if err != nil || holding == uuid.Nil || holding == m.IncidentID {
		return domain.IncidentRef{}, err
	}
	from, err := s.incidents.InvestigationIncident(ctx, scope, holding)
	if err != nil {
		return domain.IncidentRef{}, err
	}
	return domain.IncidentRef{ID: from.IncidentID, Number: from.Number}, nil
}

// ------------------------------------------------------------- read and apply

// ListSuggestions reads one Investigation's SHOWN Suggestions, in the order they were
// proposed: every applied one, and every one that has not lapsed. ⭐ A LAPSED ONE IS NOT
// SHOWN — it is not read at all. Each open membership Suggestion says, as it is read, the
// Incident applying it would move its Case from. An Investigation this org does not have
// is a 404, like the Investigation itself.
func (s *Service) ListSuggestions(ctx context.Context, scope db.TenantScope, investigationID uuid.UUID) ([]domain.Suggestion, error) {
	if err := db.RequireScope(scope); err != nil {
		return nil, err
	}
	if _, err := s.investigations.Get(ctx, scope, investigationID); err != nil {
		return nil, err
	}
	now := s.now()
	list, err := s.suggestions.ListSuggestions(ctx, scope, investigationID, now)
	if err != nil {
		return nil, err
	}
	for i, sg := range list {
		if sg.Kind != domain.SuggestMembership || sg.StateAt(now) != domain.SuggestionOpen {
			continue
		}
		from, err := s.movesFrom(ctx, scope, sg.Membership)
		if err != nil && !errs.IsKind(err, errs.KindNotFound) {
			return nil, err
		}
		list[i].MovesFrom = from
	}
	return list, nil
}

// ApplySuggestion is a human applying one Suggestion: the ORDINARY edit it proposes, made in
// one transaction with the record of who applied it.
//
// ⭐ THE EDIT IS THE ONE A HUMAN MAKES BY HAND. A count condition is set through the policy
// PATCH's own service method, so the merged policy is validated exactly as a hand edit's
// is — a refusal there is the same `422`. A membership is the Incident service's own Add,
// or its Move when the Case is in another Incident, with the applier as actor and the
// Investigation as provenance on the Case's timeline fact. Its own refusals (the Case is
// already in this Incident, a drill's Case) come back unchanged.
//
// ⛔ A MOVE IS NEVER A SURPRISE. When applying a membership Suggestion would move its Case,
// the request must name the Incident it moves from (`movesFrom`); one that does not — or
// names another, because the Case moved since the screen was drawn — is refused
// `suggestion_moves_case` with the sentence the screen shows, and nothing is written. A
// confirmation that is no longer needed (the Case left its Incident meanwhile) is not a
// refusal: the Suggestion applies as the add it now is.
//
// Refusals, each typed: `suggestion_not_found` (404), `suggestion_already_applied` and
// `suggestion_lapsed` (409), `suggestion_target_gone` (409) when the policy, Incident or
// Case was deleted since it was proposed.
func (s *Service) ApplySuggestion(
	ctx context.Context, scope db.TenantScope, id uuid.UUID, by domain.Requester, movesFrom int64,
) (domain.Suggestion, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.Suggestion{}, err
	}
	var out domain.Suggestion
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		sg, err := s.suggestions.LockSuggestion(ctx, scope, id)
		if errs.IsKind(err, errs.KindNotFound) {
			return domain.SuggestionNotFound()
		}
		if err != nil {
			return err
		}
		now := s.now()
		if err := sg.Applicable(now); err != nil {
			return err
		}
		switch sg.Kind {
		case domain.SuggestCountCondition:
			if err := s.applyCount(ctx, scope, sg.Count); err != nil {
				return err
			}
		case domain.SuggestMembership:
			if err := s.applyMembership(ctx, scope, sg, by, movesFrom); err != nil {
				return err
			}
		default:
			return errs.Newf(errs.KindInternal, "suggestion_kind", "unknown Suggestion kind %q", sg.Kind)
		}
		if err := s.suggestions.MarkApplied(ctx, scope, sg.ID, by, now); err != nil {
			return err
		}
		sg.AppliedAt, sg.AppliedBy = now, by
		if sg.AppliedAt.Before(sg.ProposedAt) {
			sg.AppliedAt = sg.ProposedAt
		}
		out = sg
		return nil
	})
	if err != nil {
		return domain.Suggestion{}, err
	}
	return out, nil
}

// applyCount makes the ordinary policy edit. A policy deleted since the proposal is
// `suggestion_target_gone`, read before the edit and again from it.
//
// ⭐ THE POLICY IS LOCKED BEFORE IT IS COMPARED (judgment 2, E6). The stale check is a read and
// the edit a write; without the row lock a hand edit committing between them was overwritten
// by a change nobody proposed against it. Under it, that edit waits for this one, or this one
// waits for it and then reads what it wrote — and refuses `suggestion_stale`.
func (s *Service) applyCount(ctx context.Context, scope db.TenantScope, c domain.CountChange) error {
	gone := func() error { return domain.SuggestionTargetGone("the notification policy " + c.PolicyName) }
	target, err := s.policies.LockSuggestionPolicy(ctx, scope, c.PolicyID)
	if err != nil {
		if errs.IsKind(err, errs.KindNotFound) {
			return gone()
		}
		return err
	}
	if target.CountMin != c.WasMin || target.CountWindow != c.WasWindow {
		// ⛔ STALE: the policy changed since the proposal, so applying would overwrite a
		// later hand edit with a change nobody proposed against it (review B3).
		return domain.SuggestionStale(target.Name, target.CountMin, target.CountWindow, c.WasMin, c.WasWindow)
	}
	err = s.policies.ApplyCountCondition(ctx, scope, c.PolicyID, c.CountMin, c.CountWindow)
	if errs.IsKind(err, errs.KindNotFound) {
		return gone()
	}
	return err
}

// applyMembership makes the ordinary membership edit: an add, or — when the Case is in
// another Incident and the request confirmed it — a move.
func (s *Service) applyMembership(
	ctx context.Context, scope db.TenantScope, sg domain.Suggestion, by domain.Requester, confirmed int64,
) error {
	m := sg.Membership
	if _, err := s.incidents.InvestigationIncident(ctx, scope, m.IncidentID); err != nil {
		if errs.IsKind(err, errs.KindNotFound) {
			return domain.SuggestionTargetGone(fmt.Sprintf("Incident #%d", m.IncidentNumber))
		}
		return err
	}
	if _, err := s.cases.InvestigationCase(ctx, scope, m.CaseID); err != nil {
		if errs.IsKind(err, errs.KindNotFound) {
			return domain.SuggestionTargetGone(fmt.Sprintf("Case #%d", m.CaseNumber))
		}
		return err
	}
	from, err := s.movesFrom(ctx, scope, m)
	if err != nil {
		return err
	}
	if !from.IsZero() && confirmed != from.Number {
		return domain.SuggestionMovesCase(m, from)
	}
	return s.memberships.ApplySuggestedMembership(ctx, scope, domain.AppliedMembership{
		IncidentNumber: m.IncidentNumber, CaseID: m.CaseID, FromNumber: from.Number,
		By: by, SuggestedBy: sg.InvestigationID,
	})
}
