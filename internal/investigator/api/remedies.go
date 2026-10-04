package api

// REMEDIES (ADR 0054; git-bug 4148256): a Finding's proposed cluster changes, read, approved
// and declined.
//
// ⭐ THE EXACT COMMAND COMES FIRST, ON THE WIRE AS ON THE SCREEN. A RemedyDTO carries the
// write Tool and the exact arguments it would be sent — byte for byte, with their hash — or
// `no_tool`, the sentence "no configured Tool can carry this out"; then the target, and only
// then the Investigator's description.
//
// ⛔ A HUMAN APPROVES AND A HUMAN DECLINES. A system principal is refused before the service
// is reached, as for a Suggestion's apply: the Investigator never approves its own Remedy, and
// neither does any other machine. Who may approve is the grant on the Remedy's ToolServer,
// checked by the service — never by this layer.
//
// ⛔ NO ROUTE HERE NAMES A GRANT. `approve` and `decline` act on a Remedy; who holds the grant
// is read-only at `/tool-servers/{id}/remedy-approvers`, and written only by `oto grant` on the
// host (test/scope walks the mounted routes to hold that).

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// RemedyDTO renders `RemedyDTO`: one Remedy, the exact command first.
type RemedyDTO struct {
	ID                uuid.UUID             `json:"id"`
	InvestigationID   uuid.UUID             `json:"investigation_id"`
	SubjectKind       string                `json:"subject_kind"`
	SubjectID         uuid.UUID             `json:"subject_id"`
	State             string                `json:"state"`
	Tool              *RemedyToolDTO        `json:"tool"`
	NoTool            *string               `json:"no_tool"`
	Blocked           *string               `json:"blocked"`
	Arguments         *string               `json:"arguments"`
	ArgumentsDisplay  *string               `json:"arguments_display"`
	ArgumentsSHA256   *string               `json:"arguments_sha256"`
	Target            string                `json:"target"`
	Description       string                `json:"description"`
	ProposedByLabel   string                `json:"proposed_by_label"`
	RequiredApprovals int                   `json:"required_approvals"`
	Approvals         []RemedyApprovalDTO   `json:"approvals"`
	ProposedAt        time.Time             `json:"proposed_at"`
	ExpiresAt         time.Time             `json:"expires_at"`
	ApprovedAt        *time.Time            `json:"approved_at"`
	ExecutingAt       *time.Time            `json:"executing_at"`
	EndedAt           *time.Time            `json:"ended_at"`
	FailureReason     *string               `json:"failure_reason"`
	Detail            *string               `json:"detail"`
	Result            *string               `json:"result"`
	Transitions       []RemedyTransitionDTO `json:"transitions"`
}

// RemedyToolDTO renders `RemedyToolDTO`: the write Tool a Remedy names.
type RemedyToolDTO struct {
	ToolServerID   uuid.UUID `json:"tool_server_id"`
	ToolServerName string    `json:"tool_server_name"`
	ToolName       string    `json:"tool_name"`
}

// RemedyApprovalDTO renders `RemedyApprovalDTO`: one person's approval.
type RemedyApprovalDTO struct {
	UserID     *uuid.UUID `json:"user_id"`
	Label      string     `json:"label"`
	ApprovedAt time.Time  `json:"approved_at"`
}

// RemedyTransitionDTO renders `RemedyTransitionDTO`: one transition, by a named actor, and
// where it was declared.
type RemedyTransitionDTO struct {
	From               *string    `json:"from"`
	To                 string     `json:"to"`
	ActorKind          string     `json:"actor_kind"`
	ActorLabel         string     `json:"actor_label"`
	At                 time.Time  `json:"at"`
	FailureReason      *string    `json:"failure_reason"`
	Detail             *string    `json:"detail"`
	DeclaredIncidentID *uuid.UUID `json:"declared_incident_id"`
}

// ApproveRemedyRequest is the body of `POST /api/v1/remedies/{id}/approve`: the hash of the
// arguments the approver was shown, which is what they approve.
type ApproveRemedyRequest struct {
	ArgumentsSHA256 string `json:"arguments_sha256" validate:"required,len=64,hexadecimal,lowercase"`
}

// remedyDTO renders one Remedy as read at `now`: its state is StateAt(now), so one past its
// window says `expired` before the sweep has recorded it.
func remedyDTO(r domain.Remedy, now time.Time) RemedyDTO {
	out := RemedyDTO{
		ID: r.ID, InvestigationID: r.InvestigationID, SubjectKind: string(r.SubjectKind), SubjectID: r.SubjectID,
		State:  string(r.StateAt(now)),
		Target: r.Target, Description: r.Description, ProposedByLabel: r.ProposedBy,
		RequiredApprovals: r.RequiredApprovals,
		Approvals:         make([]RemedyApprovalDTO, 0, len(r.Approvals)),
		ProposedAt:        r.ProposedAt, ExpiresAt: r.ExpiresAt,
		ApprovedAt: optTime(r.ApprovedAt), ExecutingAt: optTime(r.ExecutingAt), EndedAt: optTime(r.EndedAt),
		FailureReason: optString(string(r.Failure)), Detail: optString(r.Detail), Result: optString(r.Result),
		Blocked:     optString(r.Blocked),
		Transitions: make([]RemedyTransitionDTO, 0, len(r.Transitions)),
	}
	if r.Tool.Named() {
		out.Tool = &RemedyToolDTO{ToolServerID: r.Tool.ToolServerID, ToolServerName: r.Tool.ToolServerName,
			ToolName: r.Tool.Tool}
		out.Arguments, out.ArgumentsSHA256 = optString(r.Arguments), optString(r.ArgumentsSHA256)
		out.ArgumentsDisplay = optString(r.ArgumentsDisplay())
	} else {
		out.NoTool = optString(domain.NoToolCanCarryItOut)
	}
	for _, a := range r.Approvals {
		var user *uuid.UUID
		if a.UserID != uuid.Nil {
			id := a.UserID
			user = &id
		}
		out.Approvals = append(out.Approvals, RemedyApprovalDTO{UserID: user, Label: a.Label, ApprovedAt: a.ApprovedAt})
	}
	for _, t := range r.Transitions {
		var declared *uuid.UUID
		if t.DeclaredIncidentID != uuid.Nil {
			id := t.DeclaredIncidentID
			declared = &id
		}
		out.Transitions = append(out.Transitions, RemedyTransitionDTO{
			From: optString(string(t.From)), To: string(t.To), ActorKind: string(t.Actor.Kind), ActorLabel: t.Actor.Label,
			At: t.At, FailureReason: optString(string(t.Failure)), Detail: optString(t.Detail),
			DeclaredIncidentID: declared,
		})
	}
	return out
}

// listInvestigationRemedies serves GET /api/v1/investigations/{id}/remedies: the Remedies the
// run's Finding proposed, in the order proposed, whatever state each is in.
func (rt *Router) listInvestigationRemedies(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, id, err := scopeAndID(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	list, err := rt.svc.ListRemedies(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	now := rt.now()
	out := make([]RemedyDTO, 0, len(list))
	for _, rem := range list {
		out = append(out, remedyDTO(rem, now))
	}
	httpx.List(w, r, out, httpx.PageOf(db.Cursor{}, maxListed), started)
}

// getRemedy serves GET /api/v1/remedies/{id}.
func (rt *Router) getRemedy(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, err := plainScope(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.WriteProblem(w, r, domain.RemedyNotFound())
		return
	}
	rem, err := rt.svc.GetRemedy(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, remedyDTO(rem, rt.now()), started)
}

// humanOnRemedy resolves the human acting on a Remedy and its id. A system principal is
// refused here — an approval or a decline is a person's — and a malformed id names no Remedy.
func humanOnRemedy(r *http.Request, verb string) (db.TenantScope, uuid.UUID, domain.Requester, error) {
	p, scope, err := authn.Scope(r.Context())
	if err != nil {
		return db.TenantScope{}, uuid.Nil, domain.Requester{}, err
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		return db.TenantScope{}, uuid.Nil, domain.Requester{}, err
	}
	if kind, err := kernel.NewActorKind(p.ActorKind()); err != nil || !kind.IsHuman() {
		return db.TenantScope{}, uuid.Nil, domain.Requester{}, errs.Forbidden("forbidden", verb+" a Remedy requires a human actor")
	}
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		return db.TenantScope{}, uuid.Nil, domain.Requester{}, domain.RemedyNotFound()
	}
	by, err := domain.NewRequester(p.UserID, p.ActorLabel())
	if err != nil {
		return db.TenantScope{}, uuid.Nil, domain.Requester{}, err
	}
	return scope, id, by, nil
}

// approveRemedy serves POST /api/v1/remedies/{id}/approve: one human's approval of the
// arguments whose hash the body names. The refusals are the service's, each typed.
func (rt *Router) approveRemedy(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, id, by, err := humanOnRemedy(r, "approving")
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[ApproveRemedyRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	rem, err := rt.svc.ApproveRemedy(r.Context(), scope, id, by, dto.ArgumentsSHA256)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, remedyDTO(rem, rt.now()), started)
}

// declineRemedy serves POST /api/v1/remedies/{id}/decline: one human saying no. It takes no
// body.
func (rt *Router) declineRemedy(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, id, by, err := humanOnRemedy(r, "declining")
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	rem, err := rt.svc.DeclineRemedy(r.Context(), scope, id, by)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, remedyDTO(rem, rt.now()), started)
}
