package service

// THE BUILT-IN TOOLS (ADR 0053 §3): oto's own history, offered to the model on the
// same footing as any ToolServer's Tool. "Memory is not a new store: it is oto's own
// history — prior Findings on the same alert_key, the Case timeline, the rule as it
// stood at fire time."
//
// ⭐ EVERY ONE IS READ-ONLY AND READS ONLY THE SUBJECT'S OWN HISTORY. A Tool takes the
// subject from the run, never from the model's arguments, so no argument a model writes
// can point it at another Case — let alone another org: the scope is the run's. An
// Incident's run reads the Incident's own earlier Findings and its member Cases'; a
// Tool that reads one Case (its timeline, its rule) is not offered to it, and a call
// to one is refused with that reason (git-bug 74ea849).
//
// ⚠️ AN INVESTIGATOR ONLY HOLDS THE ONES ITS ALLOWLIST NAMES. Being built in is not
// being allowed: a built-in Tool the allowlist omits is never offered, and a call to it
// is refused and recorded like any other call outside the list (ADR 0053 §6).

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// The built-in Tools' names. They are what an allowlist names to hold them.
const (
	ToolPriorFindings = "oto_prior_findings"
	ToolCaseTimeline  = "oto_case_timeline"
	ToolRuleAtFire    = "oto_rule_at_fire"
	// ToolMemberFindings reads an Incident's member Cases' earlier Findings (ADR 0053
	// §3, §4; git-bug 74ea849): the Incident is investigated as a whole, and what was
	// already concluded about its parts is the first thing worth reading.
	ToolMemberFindings = "oto_member_findings"
)

// RunSubject is what a Tool is told about the run calling it: which kind of subject,
// and that subject — the Case, or the Incident.
type RunSubject struct {
	InvestigationID uuid.UUID
	Kind            domain.SubjectKind
	Case            domain.CaseSubject
	Incident        domain.IncidentSubject
}

// subjectTool is a built-in Tool that reads one kind of subject only. A Tool that is
// not one — every ToolServer's — reads the cluster, whatever the run is about.
type subjectTool interface {
	reads(kind domain.SubjectKind) bool
	subjectNoun() string
}

// caseOnly and incidentOnly are the two answers a built-in Tool gives.
type caseOnly struct{}

func (caseOnly) reads(kind domain.SubjectKind) bool { return kind == domain.SubjectCase }
func (caseOnly) subjectNoun() string                { return "Case" }

type incidentOnly struct{}

func (incidentOnly) reads(kind domain.SubjectKind) bool { return kind == domain.SubjectIncident }
func (incidentOnly) subjectNoun() string                { return "Incident" }

// Tool is one capability a run may call. Call answers with the text the model is
// given; an error is recorded as a `failed` Step and answered to the model, and the
// run continues.
type Tool interface {
	Schema() domain.ToolSchema
	Call(ctx context.Context, scope db.TenantScope, run RunSubject, args json.RawMessage) (string, error)
}

// builtinTools builds the four, with schemas from domain.NewToolSchema — the one
// constructor the port's request validation trusts.
func builtinTools(s *Service) []Tool {
	const findingsLimit = `{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":10,"description":"How many Findings, 1 to 10; default 5."}},"additionalProperties":false}`
	return []Tool{
		priorFindingsTool{s: s, schema: mustSchema(ToolPriorFindings,
			"Earlier Investigations' Findings about the same subject, newest first: for a Case, the same alert "+
				"(the same alert_key) the last times it fired; for an Incident, the same Incident.",
			findingsLimit)},
		memberFindingsTool{s: s, schema: mustSchema(ToolMemberFindings,
			"Earlier Investigations' Findings about this Incident's member Cases, newest first: what was "+
				"already concluded about the parts of the story before it was investigated as a whole.",
			findingsLimit)},
		caseTimelineTool{s: s, schema: mustSchema(ToolCaseTimeline,
			"The Case's timeline as oto recorded it: every state change, acknowledgement, comment and enrichment, oldest first.",
			`{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":100,"description":"How many of the most recent entries, 1 to 100; default 50."}},"additionalProperties":false}`)},
		ruleAtFireTool{s: s, schema: mustSchema(ToolRuleAtFire,
			"The Prometheus alerting rule as it stood when this Case fired: its expression, for-duration, labels and annotations.",
			"")},
	}
}

// mustSchema builds a built-in Tool's schema. The literals above are fixed, so a
// failure is a broken build of oto, not an input to handle.
func mustSchema(name, description, params string) domain.ToolSchema {
	s, err := domain.NewToolSchema(name, description, json.RawMessage(params))
	if err != nil {
		panic("investigator: built-in Tool " + name + ": " + err.Error())
	}
	return s
}

// limitArg reads an optional `limit` argument inside [1, ceiling], defaulting to def.
func limitArg(args json.RawMessage, def, ceiling int) (int, error) {
	var in struct {
		Limit *int `json:"limit"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return 0, errs.New(errs.KindValidation, "tool_arguments_invalid", "limit must be an integer")
		}
	}
	if in.Limit == nil {
		return def, nil
	}
	if *in.Limit < 1 || *in.Limit > ceiling {
		return 0, errs.Newf(errs.KindValidation, "tool_arguments_invalid", "limit must be 1 to %d", ceiling)
	}
	return *in.Limit, nil
}

func asJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", errs.Internal("tool_result_encode", err)
	}
	return string(b), nil
}

func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

// ------------------------------------------------------------ prior Findings

type priorFindingsTool struct {
	s      *Service
	schema domain.ToolSchema
}

func (t priorFindingsTool) Schema() domain.ToolSchema { return t.schema }

func (t priorFindingsTool) Call(ctx context.Context, scope db.TenantScope, run RunSubject, args json.RawMessage) (string, error) {
	limit, err := limitArg(args, 5, 10)
	if err != nil {
		return "", err
	}
	var prior []domain.PriorFinding
	switch {
	case run.Kind == domain.SubjectIncident:
		prior, err = t.s.investigations.SubjectFindings(ctx, scope, domain.SubjectIncident,
			[]uuid.UUID{run.Incident.IncidentID}, run.InvestigationID, limit)
	case run.Case.AlertKey == "":
		return `{"findings":[]}`, nil
	default:
		prior, err = t.s.investigations.PriorFindings(ctx, scope, run.Case.AlertKey, run.InvestigationID, limit)
	}
	if err != nil {
		return "", err
	}
	return findingsJSON(prior)
}

// findingsJSON renders earlier Findings as a Tool answers them, each naming the subject
// it was about.
func findingsJSON(prior []domain.PriorFinding) (string, error) {
	type finding struct {
		InvestigationID string `json:"investigation_id"`
		SubjectKind     string `json:"subject_kind"`
		SubjectID       string `json:"subject_id"`
		Investigator    string `json:"investigator"`
		Version         int    `json:"version"`
		Status          string `json:"status"`
		Partial         bool   `json:"partial"`
		EndedAt         any    `json:"ended_at"`
		Finding         string `json:"finding"`
	}
	out := make([]finding, 0, len(prior))
	for _, p := range prior {
		out = append(out, finding{
			InvestigationID: p.InvestigationID.String(), SubjectKind: string(p.SubjectKind), SubjectID: p.SubjectID.String(),
			Investigator: p.InvestigatorName, Version: p.VersionNumber, Status: string(p.Status),
			Partial: p.Status == domain.StatusExhausted, EndedAt: timeOrNil(p.EndedAt), Finding: p.Finding,
		})
	}
	return asJSON(map[string]any{"findings": out})
}

// ------------------------------------------------------- member Cases' Findings

// memberFindingsTool reads the earlier Findings about an Incident's CURRENT member
// Cases. Those Findings exist because a human asked about one Case, or because the Case
// was investigated before it joined: a member Case starts nothing of its own
// automatically once it is in (ADR 0053 §4), so this is everything there is.
type memberFindingsTool struct {
	incidentOnly
	s      *Service
	schema domain.ToolSchema
}

func (t memberFindingsTool) Schema() domain.ToolSchema { return t.schema }

func (t memberFindingsTool) Call(ctx context.Context, scope db.TenantScope, run RunSubject, args json.RawMessage) (string, error) {
	limit, err := limitArg(args, 5, 10)
	if err != nil {
		return "", err
	}
	found, err := t.s.investigations.SubjectFindings(ctx, scope, domain.SubjectCase,
		run.Incident.CurrentCases(), run.InvestigationID, limit)
	if err != nil {
		return "", err
	}
	return findingsJSON(found)
}

// ------------------------------------------------------------- Case timeline

type caseTimelineTool struct {
	caseOnly
	s      *Service
	schema domain.ToolSchema
}

func (t caseTimelineTool) Schema() domain.ToolSchema { return t.schema }

func (t caseTimelineTool) Call(ctx context.Context, scope db.TenantScope, run RunSubject, args json.RawMessage) (string, error) {
	limit, err := limitArg(args, 50, 100)
	if err != nil {
		return "", err
	}
	entries, err := t.s.timeline.CaseTimeline(ctx, scope, run.Case.CaseID, limit)
	if err != nil {
		return "", err
	}
	type entry struct {
		At      any    `json:"at"`
		Type    string `json:"type"`
		Actor   string `json:"actor"`
		Summary string `json:"summary"`
	}
	out := make([]entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, entry{At: timeOrNil(e.At), Type: e.Type, Actor: e.Actor, Summary: e.Summary})
	}
	return asJSON(map[string]any{"case_number": run.Case.Number, "entries": out})
}

// -------------------------------------------------------------- rule at fire

type ruleAtFireTool struct {
	caseOnly
	s      *Service
	schema domain.ToolSchema
}

func (t ruleAtFireTool) Schema() domain.ToolSchema { return t.schema }

func (t ruleAtFireTool) Call(ctx context.Context, scope db.TenantScope, run RunSubject, _ json.RawMessage) (string, error) {
	if run.Case.RuleSnapshotID == uuid.Nil {
		// ⭐ "NO RULE WAS CAPTURED" IS AN ANSWER, NOT A FAILURE. The model must hear it
		// as one rather than guess at an empty object.
		return asJSON(map[string]any{"available": false,
			"reason": "oto bound no rule snapshot to this Case when it fired"})
	}
	r, err := t.s.rules.RuleAtFire(ctx, scope, run.Case.RuleSnapshotID)
	if err != nil {
		return "", err
	}
	if !r.Available {
		return asJSON(map[string]any{"available": false, "captured_at": timeOrNil(r.CapturedAt),
			"reason": "oto looked for the rule when this Case fired and could not see it"})
	}
	return asJSON(map[string]any{
		"available":   true,
		"captured_at": timeOrNil(r.CapturedAt),
		"name":        r.Name,
		"group":       r.Group,
		"expr":        r.Expr,
		"for_seconds": r.For.Seconds(),
		"labels":      r.Labels,
		"annotations": r.Annotations,
		"origin":      r.Origin,
		"confidence":  r.Confidence,
	})
}
