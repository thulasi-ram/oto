package api

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/investigator/service"
)

// The wire shapes of the Investigators tag (api/openapi/openapi.yaml). Gate G1 diffs
// every struct here against its component schema.
//
// ⛔ NO DTO HERE CARRIES AN API KEY ON THE WAY OUT. `CreateModelProviderRequest.api_key`
// is write-only; every response says only `has_key`.

// ModelProviderDTO renders `ModelProviderDTO`: one model endpoint, without its key.
type ModelProviderDTO struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	BaseURL   string    `json:"base_url"`
	Model     string    `json:"model"`
	HasKey    bool      `json:"has_key"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func modelProviderDTO(c domain.ProviderConfig) ModelProviderDTO {
	return ModelProviderDTO{ID: c.ID, Name: c.Name, BaseURL: c.BaseURL, Model: c.Model,
		HasKey: c.HasKey(), CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

// CreateModelProviderRequest is the body of `POST /api/v1/model-providers`.
//
// ⛔ `api_key` IS WRITE-ONLY. It is sealed before the row is written and never read
// back; the response carries `has_key` alone.
type CreateModelProviderRequest struct {
	Name    string  `json:"name"              validate:"required,notblank,min=1,max=120"`
	BaseURL string  `json:"base_url"          validate:"required,min=1,max=2048"`
	Model   string  `json:"model"             validate:"required,notblank,min=1,max=200"`
	APIKey  *string `json:"api_key,omitempty" validate:"omitempty,max=4096"`
}

// ModelIdentityDTO renders `ModelIdentityDTO`: which endpoint and which model.
type ModelIdentityDTO struct {
	Endpoint string `json:"endpoint"`
	Name     string `json:"name"`
}

func modelIdentityDTO(m domain.ModelIdentity) ModelIdentityDTO {
	return ModelIdentityDTO{Endpoint: m.Endpoint, Name: m.Model}
}

// InvestigatorBudgetsDTO renders `InvestigatorBudgetsDTO`: the three per-run budgets.
type InvestigatorBudgetsDTO struct {
	MaxSteps       int   `json:"max_steps"        validate:"required,min=1,max=100"`
	MaxTokens      int64 `json:"max_tokens"       validate:"required,min=1000,max=2000000"`
	MaxWallSeconds int   `json:"max_wall_seconds" validate:"required,min=10,max=1800"`
}

func budgetsDTO(b domain.Budgets) InvestigatorBudgetsDTO {
	return InvestigatorBudgetsDTO{MaxSteps: b.MaxSteps, MaxTokens: b.MaxTokens, MaxWallSeconds: b.WallSeconds()}
}

func (b InvestigatorBudgetsDTO) toDomain() (domain.Budgets, error) {
	return domain.NewBudgets(b.MaxSteps, b.MaxTokens, b.MaxWallSeconds)
}

// InvestigatorVersionDTO renders `InvestigatorVersionDTO`: one immutable version.
type InvestigatorVersionDTO struct {
	ID              uuid.UUID        `json:"id"`
	Version         int              `json:"version"`
	ModelProviderID uuid.UUID        `json:"model_provider_id"`
	Model           ModelIdentityDTO `json:"model"`
	Prompt          string           `json:"prompt"`
	Tools           []string         `json:"tools"`
	CreatedAt       time.Time        `json:"created_at"`
}

func versionDTO(v domain.Version) InvestigatorVersionDTO {
	return InvestigatorVersionDTO{ID: v.ID, Version: v.Number, ModelProviderID: v.ProviderID,
		Model: modelIdentityDTO(v.Model), Prompt: v.Prompt, Tools: v.Tools.Names(), CreatedAt: v.CreatedAt}
}

// InvestigatorDTO renders `InvestigatorDTO`: one Investigator and the version new
// runs use.
type InvestigatorDTO struct {
	ID       uuid.UUID              `json:"id"`
	Name     string                 `json:"name"`
	Enricher string                 `json:"enricher"`
	Enabled  bool                   `json:"enabled"`
	Budgets  InvestigatorBudgetsDTO `json:"budgets"`
	// MinIntervalSeconds is the least time between two runs on one subject (ADR 0053
	// §6): membership-change triggers inside it coalesce into one run.
	MinIntervalSeconds int `json:"min_interval_seconds"`
	// InvestigatesIncidents is whether an Incident being drawn, and its membership
	// changing, starts a run of this Investigator on it (ADR 0053 §4).
	InvestigatesIncidents bool                   `json:"investigates_incidents"`
	CurrentVersion        InvestigatorVersionDTO `json:"current_version"`
	CreatedAt             time.Time              `json:"created_at"`
	UpdatedAt             time.Time              `json:"updated_at"`
}

func investigatorDTO(i domain.Investigator) InvestigatorDTO {
	return InvestigatorDTO{ID: i.ID, Name: i.Name, Enricher: i.EnricherName(), Enabled: i.Enabled,
		Budgets: budgetsDTO(i.Budgets), MinIntervalSeconds: int(i.MinInterval / time.Second),
		InvestigatesIncidents: i.InvestigatesIncidents,
		CurrentVersion:        versionDTO(i.Current),
		CreatedAt:             i.CreatedAt, UpdatedAt: i.UpdatedAt}
}

// InvestigatorDetailDTO renders `InvestigatorDetailDTO`: the Investigator and every
// version it has had, newest first. It embeds InvestigatorDTO exactly as the
// contract's `allOf` composes it.
type InvestigatorDetailDTO struct {
	InvestigatorDTO
	Versions []InvestigatorVersionDTO `json:"versions"`
}

func investigatorDetailDTO(d service.InvestigatorDetail) InvestigatorDetailDTO {
	out := InvestigatorDetailDTO{InvestigatorDTO: investigatorDTO(d.Investigator),
		Versions: make([]InvestigatorVersionDTO, 0, len(d.Versions))}
	for _, v := range d.Versions {
		out.Versions = append(out.Versions, versionDTO(v))
	}
	return out
}

// CreateInvestigatorRequest is the body of `POST /api/v1/investigators`.
type CreateInvestigatorRequest struct {
	Name    string                  `json:"name"              validate:"required,min=1,max=63"`
	Enabled *bool                   `json:"enabled,omitempty"`
	Budgets *InvestigatorBudgetsDTO `json:"budgets,omitempty"`
	// MinIntervalSeconds defaults to 600 (domain.DefaultIntervalSeconds).
	MinIntervalSeconds *int `json:"min_interval_seconds,omitempty" validate:"omitempty,min=0,max=86400"`
	// InvestigatesIncidents defaults to false: an operator opts an Investigator in to
	// the runs an Incident starts on its own.
	InvestigatesIncidents *bool     `json:"investigates_incidents,omitempty"`
	ModelProviderID       uuid.UUID `json:"model_provider_id" validate:"required"`
	Prompt                string    `json:"prompt"            validate:"required,notblank,min=1,max=32768"`
	Tools                 []string  `json:"tools"             validate:"max=64,dive,min=1,max=64"`
}

// UpdateInvestigatorRequest is the body of `PATCH /api/v1/investigators/{id}`.
//
// ⭐ A NEW VERSION IS THE SERVER'S CALL, NOT THE CLIENT'S. Any of `model_provider_id`,
// `prompt` and `tools` is folded over the current version, and only a result that
// differs from it — a different endpoint or model, prompt or allowlist — writes version
// N+1 (ADR 0053 §6). `enabled`, `budgets`, `min_interval_seconds` and
// `investigates_incidents` change in place and never version.
type UpdateInvestigatorRequest struct {
	Enabled               *bool                   `json:"enabled,omitempty"`
	Budgets               *InvestigatorBudgetsDTO `json:"budgets,omitempty"`
	MinIntervalSeconds    *int                    `json:"min_interval_seconds,omitempty" validate:"omitempty,min=0,max=86400"`
	InvestigatesIncidents *bool                   `json:"investigates_incidents,omitempty"`
	ModelProviderID       *uuid.UUID              `json:"model_provider_id,omitempty"`
	Prompt                *string                 `json:"prompt,omitempty" validate:"omitempty,notblank,min=1,max=32768"`
	Tools                 *[]string               `json:"tools,omitempty"  validate:"omitempty,max=64,dive,min=1,max=64"`
}

// RequestInvestigationRequest is the body of `POST /api/v1/cases/{id}/investigations`
// and `POST /api/v1/incidents/{number}/investigations`.
type RequestInvestigationRequest struct {
	InvestigatorID uuid.UUID `json:"investigator_id" validate:"required"`
}

// InvestigationDTO renders `InvestigationDTO`: one run, without its transcript.
type InvestigationDTO struct {
	ID                    uuid.UUID              `json:"id"`
	SubjectKind           string                 `json:"subject_kind"`
	SubjectID             uuid.UUID              `json:"subject_id"`
	InvestigatorID        uuid.UUID              `json:"investigator_id"`
	InvestigatorName      string                 `json:"investigator_name"`
	InvestigatorVersion   int                    `json:"investigator_version"`
	InvestigatorVersionID uuid.UUID              `json:"investigator_version_id"`
	Model                 ModelIdentityDTO       `json:"model"`
	Status                string                 `json:"status"`
	Reason                *string                `json:"reason"`
	ReasonDetail          *string                `json:"reason_detail"`
	Budgets               InvestigatorBudgetsDTO `json:"budgets"`
	TokensIn              int64                  `json:"tokens_in"`
	TokensOut             int64                  `json:"tokens_out"`
	ToolCalls             int                    `json:"tool_calls"`
	Finding               *string                `json:"finding"`
	Classification        *string                `json:"classification"`
	Partial               bool                   `json:"partial"`
	RequestedByLabel      string                 `json:"requested_by_label"`
	RequestedAt           time.Time              `json:"requested_at"`
	NotBefore             *time.Time             `json:"not_before"`
	StartedAt             *time.Time             `json:"started_at"`
	EndedAt               *time.Time             `json:"ended_at"`
}

func investigationDTO(i domain.Investigation) InvestigationDTO {
	return InvestigationDTO{
		ID: i.ID, SubjectKind: string(i.SubjectKind), SubjectID: i.SubjectID,
		InvestigatorID: i.InvestigatorID, InvestigatorName: i.InvestigatorName,
		InvestigatorVersion: i.VersionNumber, InvestigatorVersionID: i.VersionID,
		Model: modelIdentityDTO(i.Model), Status: string(i.Status),
		Reason: optString(string(i.Ending.Reason)), ReasonDetail: optString(i.Ending.Detail),
		Budgets: budgetsDTO(i.Budgets), TokensIn: i.Spent.InputTokens, TokensOut: i.Spent.OutputTokens,
		ToolCalls: i.ToolCalls, Finding: optString(i.Finding), Partial: i.Partial(),
		Classification:   optString(i.Classification),
		RequestedByLabel: i.RequestedBy.Label, RequestedAt: i.RequestedAt, NotBefore: optTime(i.NotBefore),
		StartedAt: optTime(i.StartedAt), EndedAt: optTime(i.EndedAt),
	}
}

// StepToolCallDTO renders `StepToolCallDTO`: one Tool call a model turn asked for.
type StepToolCallDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// InvestigationStepDTO renders `InvestigationStepDTO`: one immutable transcript entry
// — a model turn or one Tool call. The fields of the other kind are null.
type InvestigationStepDTO struct {
	Seq          int                `json:"seq"`
	Kind         string             `json:"kind"`
	Text         *string            `json:"text"`
	ToolCalls    *[]StepToolCallDTO `json:"tool_calls"`
	FinishReason *string            `json:"finish_reason"`
	TokensIn     *int64             `json:"tokens_in"`
	TokensOut    *int64             `json:"tokens_out"`
	CallID       *string            `json:"call_id"`
	ToolName     *string            `json:"tool_name"`
	Arguments    *string            `json:"arguments"`
	Outcome      *string            `json:"outcome"`
	Result       *string            `json:"result"`
	DurationMS   int64              `json:"duration_ms"`
	RecordedAt   time.Time          `json:"recorded_at"`
}

func stepDTO(s domain.Step) InvestigationStepDTO {
	out := InvestigationStepDTO{Seq: s.Seq, Kind: string(s.Kind), DurationMS: s.Duration.Milliseconds(), RecordedAt: s.RecordedAt}
	switch s.Kind {
	case domain.StepModelTurn:
		calls := make([]StepToolCallDTO, 0, len(s.Calls))
		for _, c := range s.Calls {
			calls = append(calls, StepToolCallDTO{ID: c.ID, Name: c.Name, Arguments: c.Arguments})
		}
		in, outTok := s.Usage.InputTokens, s.Usage.OutputTokens
		text, finish := s.Text, string(s.Finish)
		out.Text, out.ToolCalls, out.FinishReason, out.TokensIn, out.TokensOut = &text, &calls, &finish, &in, &outTok
	case domain.StepToolCall:
		id, name, args, outcome, result := s.Call.ID, s.Call.Name, s.Call.Arguments, string(s.Outcome), s.Result
		out.CallID, out.ToolName, out.Arguments, out.Outcome, out.Result = &id, &name, &args, &outcome, &result
	}
	return out
}

// InvestigationDetailDTO renders `InvestigationDetailDTO`: the run and its whole
// transcript, in order. It embeds InvestigationDTO exactly as the contract's `allOf`
// composes it.
type InvestigationDetailDTO struct {
	InvestigationDTO
	Steps []InvestigationStepDTO `json:"steps"`
}

func investigationDetailDTO(d service.InvestigationDetail) InvestigationDetailDTO {
	out := InvestigationDetailDTO{InvestigationDTO: investigationDTO(d.Investigation),
		Steps: make([]InvestigationStepDTO, 0, len(d.Steps))}
	for _, s := range d.Steps {
		out.Steps = append(out.Steps, stepDTO(s))
	}
	return out
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// ---------------------------------------------------------------- ToolServers
//
// git-bug 2e9a086. ⛔ NO DTO HERE CARRIES A TOOLSERVER'S TOKEN ON THE WAY OUT.
// `CreateToolServerRequest.token` is write-only; every response says only `has_token`.

// ToolServerDTO renders `ToolServerDTO`: one ToolServer, without its token.
type ToolServerDTO struct {
	ID                 uuid.UUID  `json:"id"`
	Name               string     `json:"name"`
	URL                string     `json:"url"`
	Transport          string     `json:"transport"`
	Access             string     `json:"access"`
	HasToken           bool       `json:"has_token"`
	CallTimeoutSeconds int        `json:"call_timeout_seconds"`
	MaxResultBytes     int        `json:"max_result_bytes"`
	DiscoveredAt       *time.Time `json:"discovered_at"`
	DiscoveryFailedAt  *time.Time `json:"discovery_failed_at"`
	DiscoveryError     *string    `json:"discovery_error"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

func toolServerDTO(c domain.ToolServerConfig) ToolServerDTO {
	return ToolServerDTO{
		ID: c.ID, Name: c.Name, URL: c.URL, Transport: string(c.Transport), Access: string(c.Access),
		HasToken: c.HasToken(), CallTimeoutSeconds: c.Limits.TimeoutSeconds(), MaxResultBytes: c.Limits.MaxResultBytes,
		DiscoveredAt: optTime(c.DiscoveredAt), DiscoveryFailedAt: optTime(c.DiscoveryFailedAt),
		DiscoveryError: optString(c.DiscoveryError), CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

// CreateToolServerRequest is the body of `POST /api/v1/tool-servers`.
//
// ⛔ `token` IS WRITE-ONLY. It is sealed before the row is written and never read back.
// ⭐ `access` HAS NO DEFAULT: whether a ToolServer only reads is the operator's
// declaration to make, and the one an Investigator's allowlist is checked against.
type CreateToolServerRequest struct {
	Name               string  `json:"name"                           validate:"required,min=1,max=24"`
	URL                string  `json:"url"                            validate:"required,min=1,max=2048"`
	Transport          *string `json:"transport,omitempty"            validate:"omitempty,oneof=streamable_http sse"`
	Access             string  `json:"access"                         validate:"required,oneof=read write"`
	Token              *string `json:"token,omitempty"                validate:"omitempty,max=4096"`
	CallTimeoutSeconds *int    `json:"call_timeout_seconds,omitempty" validate:"omitempty,min=1,max=120"`
	MaxResultBytes     *int    `json:"max_result_bytes,omitempty"     validate:"omitempty,min=1024,max=61440"`
}

func (dto CreateToolServerRequest) toDomain() (domain.ToolServerDraft, error) {
	timeout, maxResult := 0, 0
	if dto.CallTimeoutSeconds != nil {
		timeout = *dto.CallTimeoutSeconds
	}
	if dto.MaxResultBytes != nil {
		maxResult = *dto.MaxResultBytes
	}
	limits, err := domain.NewCallLimits(timeout, maxResult)
	if err != nil {
		return domain.ToolServerDraft{}, err
	}
	transport, token := "", ""
	if dto.Transport != nil {
		transport = *dto.Transport
	}
	if dto.Token != nil {
		token = *dto.Token
	}
	return domain.NewToolServerDraft(dto.Name, dto.URL, transport, dto.Access, token, limits)
}

// ToolServerToolDTO renders `ToolServerToolDTO`: one Tool a ToolServer listed, and the
// name an allowlist holds it by — or why it cannot be held.
type ToolServerToolDTO struct {
	Name           string          `json:"name"`
	QualifiedName  *string         `json:"qualified_name"`
	Description    string          `json:"description"`
	InputSchema    json.RawMessage `json:"input_schema"`
	ReadOnlyHint   *bool           `json:"read_only_hint"`
	Usable         bool            `json:"usable"`
	UnusableReason *string         `json:"unusable_reason"`
}

func toolServerToolDTO(server domain.ToolServerConfig, t domain.DiscoveredTool) ToolServerToolDTO {
	qualified, why := t.Usable(server.Name)
	if why == "" && !server.Readable() {
		// ⛔ Listed, named, and never held: a write ToolServer's Tools are not an
		// Investigator's (ADR 0054 §5). The qualified name is still shown — it is how
		// a Remedy will name the Tool it would use.
		why = "it is on a write ToolServer; an Investigator holds only read Tools"
	}
	schema := t.InputSchema
	if schema == nil {
		schema = json.RawMessage("null")
	}
	return ToolServerToolDTO{Name: t.Name, QualifiedName: optString(qualified), Description: t.Description,
		InputSchema: schema, ReadOnlyHint: t.ReadOnlyHint, Usable: why == "", UnusableReason: optString(why)}
}

// ---------------------------------------------------------------- Classification
//
// ADR 0053 §5, git-bug 4298aa0: the org's closed class set, read and replaced whole.
// ⛔ `unclassified` is never in it — it is always admissible and is not the operator's.

// InvestigationClassDTO renders `InvestigationClassDTO`: one class, as the operator
// wrote it.
type InvestigationClassDTO struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// InvestigationClassSetDTO renders `InvestigationClassSetDTO`: the org's whole set, in
// the operator's order. Empty: Findings carry no classification.
type InvestigationClassSetDTO struct {
	Classes []InvestigationClassDTO `json:"classes"`
}

func classSetDTO(set domain.ClassSet) InvestigationClassSetDTO {
	classes := set.Classes()
	out := InvestigationClassSetDTO{Classes: make([]InvestigationClassDTO, 0, len(classes))}
	for _, c := range classes {
		out.Classes = append(out.Classes, InvestigationClassDTO{Name: c.Name, Description: c.Description})
	}
	return out
}

// InvestigationClassRequest is one class in `ReplaceInvestigationClassesRequest`. The
// name's alphabet, its uniqueness and the reserved `unclassified` are the domain's to
// refuse (domain.NewClassSet), each with the field it is about.
type InvestigationClassRequest struct {
	Name        string `json:"name"                  validate:"required,min=1,max=63"`
	Description string `json:"description,omitempty" validate:"max=500"`
}

// ReplaceInvestigationClassesRequest is the body of `PUT
// /api/v1/investigation-classes`: the whole set, which replaces the old one. An empty
// list is legal, and is how an operator stops Findings being classified.
type ReplaceInvestigationClassesRequest struct {
	Classes []InvestigationClassRequest `json:"classes" validate:"required,max=50,dive"`
}

func (dto ReplaceInvestigationClassesRequest) toDomain() (domain.ClassSet, error) {
	classes := make([]domain.Class, 0, len(dto.Classes))
	for _, c := range dto.Classes {
		classes = append(classes, domain.Class{Name: c.Name, Description: c.Description})
	}
	return domain.NewClassSet(classes)
}

// ---------------------------------------------------------------- Suggestions
//
// git-bug 8327c00. ⛔ THERE IS NO REQUEST BODY HERE BUT APPLY'S: a Suggestion is applied or
// it lapses, and no DTO carries a verb that declines one.

// SuggestionDTO renders `SuggestionDTO`: one change a Finding proposed, `open` or `applied`.
// A lapsed one is never rendered — it stops being shown.
type SuggestionDTO struct {
	ID              uuid.UUID                    `json:"id"`
	InvestigationID uuid.UUID                    `json:"investigation_id"`
	Kind            string                       `json:"kind"`
	State           string                       `json:"state"`
	Why             string                       `json:"why"`
	ProposedAt      time.Time                    `json:"proposed_at"`
	LapsesAt        time.Time                    `json:"lapses_at"`
	AppliedAt       *time.Time                   `json:"applied_at"`
	AppliedByLabel  *string                      `json:"applied_by_label"`
	CountCondition  *CountConditionSuggestionDTO `json:"count_condition"`
	Membership      *MembershipSuggestionDTO     `json:"membership"`
}

// CountConditionSuggestionDTO renders `CountConditionSuggestionDTO`: the policy, the count
// condition proposed, and the one it carried when proposed (null for none).
type CountConditionSuggestionDTO struct {
	PolicyID              uuid.UUID `json:"policy_id"`
	PolicyName            string    `json:"policy_name"`
	CountMin              int       `json:"count_min"`
	CountWindowSeconds    int       `json:"count_window_seconds"`
	WasCountMin           *int      `json:"was_count_min"`
	WasCountWindowSeconds *int      `json:"was_count_window_seconds"`
}

// MembershipSuggestionDTO renders `MembershipSuggestionDTO`: this Case, into this Incident —
// and, while it is open, the Incident applying it would move the Case from.
type MembershipSuggestionDTO struct {
	IncidentID              uuid.UUID `json:"incident_id"`
	IncidentNumber          int64     `json:"incident_number"`
	CaseID                  uuid.UUID `json:"case_id"`
	CaseNumber              int64     `json:"case_number"`
	MovesFromIncidentNumber *int64    `json:"moves_from_incident_number"`
}

// ApplySuggestionRequest is the body of `POST /api/v1/suggestions/{id}/apply`. It is empty
// for every Suggestion but a membership one that would MOVE its Case, which must name the
// Incident it moves from — the move is said before it is applied.
type ApplySuggestionRequest struct {
	MovesFromIncidentNumber *int64 `json:"moves_from_incident_number,omitempty" validate:"omitempty,min=1"`
}

// suggestionDTO renders one Suggestion the service SHOWED — applied, or open when it was
// read — so its state is read off `applied_at` alone: a lapsed one never reaches here.
func suggestionDTO(s domain.Suggestion) SuggestionDTO {
	state := domain.SuggestionOpen
	if !s.AppliedAt.IsZero() {
		state = domain.SuggestionApplied
	}
	out := SuggestionDTO{ID: s.ID, InvestigationID: s.InvestigationID, Kind: string(s.Kind),
		State: string(state), Why: s.Why, ProposedAt: s.ProposedAt, LapsesAt: s.LapsesAt,
		AppliedAt: optTime(s.AppliedAt)}
	if !s.AppliedAt.IsZero() {
		out.AppliedByLabel = &s.AppliedBy.Label
	}
	switch s.Kind {
	case domain.SuggestCountCondition:
		c := s.Count
		cc := &CountConditionSuggestionDTO{PolicyID: c.PolicyID, PolicyName: c.PolicyName,
			CountMin: c.CountMin, CountWindowSeconds: int(c.CountWindow / time.Second)}
		if c.WasMin > 0 {
			was, wasWin := c.WasMin, int(c.WasWindow/time.Second)
			cc.WasCountMin, cc.WasCountWindowSeconds = &was, &wasWin
		}
		out.CountCondition = cc
	case domain.SuggestMembership:
		m := s.Membership
		ms := &MembershipSuggestionDTO{IncidentID: m.IncidentID, IncidentNumber: m.IncidentNumber,
			CaseID: m.CaseID, CaseNumber: m.CaseNumber}
		if !s.MovesFrom.IsZero() {
			n := s.MovesFrom.Number
			ms.MovesFromIncidentNumber = &n
		}
		out.Membership = ms
	}
	return out
}
