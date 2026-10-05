package api

// A SLACK MEMBER LINKS THEMSELVES TO THE SIGNED-IN oto USER WITH A CODE (git-bug a556a5c; the ruling
// of 2026-10-05: SELF-SERVICE IN THE UI). Four operations under `/me`, because each is about the
// caller and nobody else:
//
//	GET    /me/slack-identities          listMySlackIdentities   session | pat
//	POST   /me/slack-identities/preview  previewSlackLink        session
//	POST   /me/slack-identities          linkSlackIdentity       session
//	DELETE /me/slack-identities/{id}     unlinkSlackIdentity     session
//
// ⛔⛔ NO ROUTE HERE TAKES A USER ID. The body is a code; the path id is an identity the caller
// must already hold; the user is `authn.Principal.UserID`, which the authenticator resolved from
// the credential. A link decides whose approval a Slack click counts as (ADR 0054 §4), so the
// three writes sit in the session-only group: a leaked PAT must not be able to make its holder's
// Slack clicks count as the PAT's owner.

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// SlackLinks is the port this layer declares for the self-service Slack link, satisfied by
// *service.Service. It is separate from IdentityService so a deployment — or a test — that does
// not wire it answers 503 rather than failing to build.
type SlackLinks interface {
	ListMySlackIdentities(ctx context.Context, scope db.TenantScope, p authn.Principal) ([]domain.SlackIdentity, error)
	PreviewSlackLink(ctx context.Context, scope db.TenantScope, p authn.Principal, code string) (service.SlackLinkPreview, error)
	ConfirmSlackLink(ctx context.Context, scope db.TenantScope, p authn.Principal, code string) (domain.SlackIdentity, error)
	UnlinkSlackIdentity(ctx context.Context, scope db.TenantScope, p authn.Principal, identityID uuid.UUID) error
}

var _ SlackLinks = (*service.Service)(nil)

// maxSlackIdentities is the bound `repository.listSlackIdentitiesByUserSQL` applies: one person has
// a handful of Slack accounts at most, so the list is never paged and says so with this limit.
const maxSlackIdentities = 50

func (rt *Router) slackLinksOrUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if rt.links == nil {
		httpx.WriteProblem(w, r, errs.Unavailable("slack_link_unavailable",
			"linking a Slack account is not available in this deployment", 0))
		return false
	}
	return true
}

// listMySlackIdentities is `GET /api/v1/me/slack-identities`.
func (rt *Router) listMySlackIdentities(w http.ResponseWriter, r *http.Request) {
	started := rt.clk.Now()
	if !rt.slackLinksOrUnavailable(w, r) {
		return
	}
	p, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	got, err := rt.links.ListMySlackIdentities(r.Context(), scope, p)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]SlackIdentityDTO, 0, len(got))
	for _, si := range got {
		out = append(out, toSlackIdentityDTO(si))
	}
	httpx.List(w, r, out, httpx.PageOf(db.Cursor{}, maxSlackIdentities), started)
}

// previewSlackLink is `POST /api/v1/me/slack-identities/preview`: which Slack member the code
// would link, WITHOUT using it up.
func (rt *Router) previewSlackLink(w http.ResponseWriter, r *http.Request) {
	started := rt.clk.Now()
	if !rt.slackLinksOrUnavailable(w, r) {
		return
	}
	p, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	req, err := httpx.Bind[SlackLinkCodeRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	got, err := rt.links.PreviewSlackLink(r.Context(), scope, p, req.Code)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, SlackLinkPreviewDTO{
		TeamID:       got.Identity.TeamID.String(),
		SlackUserID:  got.Identity.SlackUserID.String(),
		Handle:       handlePtr(got.Identity.Handle),
		ExpiresAt:    got.ExpiresAt.UTC(),
		AlreadyYours: got.AlreadyYours,
	}, started)
}

// linkSlackIdentity is `POST /api/v1/me/slack-identities`: use the code up and link its Slack
// member to the signed-in user.
func (rt *Router) linkSlackIdentity(w http.ResponseWriter, r *http.Request) {
	started := rt.clk.Now()
	if !rt.slackLinksOrUnavailable(w, r) {
		return
	}
	p, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	req, err := httpx.Bind[SlackLinkCodeRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	got, err := rt.links.ConfirmSlackLink(r.Context(), scope, p, req.Code)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusCreated, toSlackIdentityDTO(got), started)
}

// unlinkSlackIdentity is `DELETE /api/v1/me/slack-identities/{id}`: an identity linked to the
// signed-in user is unlinked; any other is a 404.
func (rt *Router) unlinkSlackIdentity(w http.ResponseWriter, r *http.Request) {
	if !rt.slackLinksOrUnavailable(w, r) {
		return
	}
	p, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	identityID, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.links.UnlinkSlackIdentity(r.Context(), scope, p, identityID); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusNoContent, nil)
}

func toSlackIdentityDTO(si domain.SlackIdentity) SlackIdentityDTO {
	var linked time.Time
	if si.LinkedAt != nil {
		linked = si.LinkedAt.UTC()
	}
	return SlackIdentityDTO{
		ID:          si.ID,
		TeamID:      si.TeamID.String(),
		SlackUserID: si.SlackUserID.String(),
		Handle:      handlePtr(si.Handle),
		LinkedAt:    linked,
	}
}

func handlePtr(h string) *string {
	if h == "" {
		return nil
	}
	return &h
}
