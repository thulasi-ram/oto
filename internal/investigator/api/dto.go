package api

import (
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
	ID             uuid.UUID              `json:"id"`
	Name           string                 `json:"name"`
	Enricher       string                 `json:"enricher"`
	Enabled        bool                   `json:"enabled"`
	Budgets        InvestigatorBudgetsDTO `json:"budgets"`
	CurrentVersion InvestigatorVersionDTO `json:"current_version"`
	CreatedAt      time.Time              `json:"created_at"`
	UpdatedAt      time.Time              `json:"updated_at"`
}

func investigatorDTO(i domain.Investigator) InvestigatorDTO {
	return InvestigatorDTO{ID: i.ID, Name: i.Name, Enricher: i.EnricherName(), Enabled: i.Enabled,
		Budgets: budgetsDTO(i.Budgets), CurrentVersion: versionDTO(i.Current),
		CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt}
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
	Name            string                  `json:"name"              validate:"required,min=1,max=63"`
	Enabled         *bool                   `json:"enabled,omitempty"`
	Budgets         *InvestigatorBudgetsDTO `json:"budgets,omitempty"`
	ModelProviderID uuid.UUID               `json:"model_provider_id" validate:"required"`
	Prompt          string                  `json:"prompt"            validate:"required,notblank,min=1,max=32768"`
	Tools           []string                `json:"tools"             validate:"max=64,dive,min=1,max=64"`
}

// UpdateInvestigatorRequest is the body of `PATCH /api/v1/investigators/{id}`.
//
// ⭐ A NEW VERSION IS THE SERVER'S CALL, NOT THE CLIENT'S. Any of `model_provider_id`,
// `prompt` and `tools` is folded over the current version, and only a result that
// differs from it — a different endpoint or model, prompt or allowlist — writes version
// N+1 (ADR 0053 §6). `enabled` and `budgets` change in place and never version.
type UpdateInvestigatorRequest struct {
	Enabled         *bool                   `json:"enabled,omitempty"`
	Budgets         *InvestigatorBudgetsDTO `json:"budgets,omitempty"`
	ModelProviderID *uuid.UUID              `json:"model_provider_id,omitempty"`
	Prompt          *string                 `json:"prompt,omitempty" validate:"omitempty,notblank,min=1,max=32768"`
	Tools           *[]string               `json:"tools,omitempty"  validate:"omitempty,max=64,dive,min=1,max=64"`
}

// RequestInvestigationRequest is the body of `POST /api/v1/cases/{id}/investigations`.
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
	Partial               bool                   `json:"partial"`
	RequestedByLabel      string                 `json:"requested_by_label"`
	RequestedAt           time.Time              `json:"requested_at"`
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
		RequestedByLabel: i.RequestedBy.Label, RequestedAt: i.RequestedAt,
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
