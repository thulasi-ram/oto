package authn

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Kind names how a request proved who it is (SPEC §E.1).
type Kind string

// The credential kinds. v1 has no RBAC and no roles (R2): an org member can do
// anything in that org, so a Principal answers "which tenant" and "who to
// attribute it to", and nothing else.
const (
	// KindSession is a browser session cookie.
	KindSession Kind = "session"
	// KindPAT is a personal access token, `Authorization: Bearer oto_pat_…`.
	KindPAT Kind = "pat"
	// KindIngest is a per-AlertSource webhook token, scoped to exactly one
	// source_id. It authorises the ingest path and NOTHING else (SPEC §G.2).
	KindIngest Kind = "ingest"
	// KindSlack is a request that proved itself with Slack's HMAC signature.
	KindSlack Kind = "slack"
	// KindSystem is oto acting on its own behalf: a worker, a sweep, a reconciler.
	KindSystem Kind = "system"
)

// Principal is an authenticated caller.
//
// It is a platform type rather than an identity type deliberately: every domain
// reads it out of the request context, and depguard forbids any of them from
// importing `internal/identity`. `identity/service` MINTS one; everybody else
// consumes it.
//
// It carries no secret. A Principal that held the presented token would put that
// token in every log line and every stack trace this package can produce.
type Principal struct {
	Kind Kind
	// OrgID is the tenancy axis and is never zero on a valid Principal.
	OrgID uuid.UUID
	// UserID is the human behind a session or a PAT. Zero for an ingest token.
	UserID uuid.UUID
	// DisplayName and Email describe the human, for attribution and `GET /me`.
	DisplayName string
	Email       string
	// OrgSlug and OrgName describe the tenant, for `GET /me`.
	OrgSlug string
	OrgName string
	// SessionID and TokenID identify the credential itself, for revocation and
	// last-used bookkeeping. THEY ARE NEVER LOGGED: a session id is a bearer
	// handle in everything but name.
	SessionID uuid.UUID
	TokenID   uuid.UUID
	// SourceID is the one AlertSource an ingest token may post to.
	SourceID uuid.UUID
	// ExpiresAt is when the credential stops working, zero for one that does not.
	ExpiresAt time.Time
}

// Valid reports whether the principal names a tenant.
func (p Principal) Valid() bool { return p.OrgID != uuid.Nil && p.Kind != "" }

// Scope converts the principal into the proof every repository method demands.
//
// This is the ONLY sanctioned path from a request to a db.TenantScope: the
// scope's field is unexported and db.NewTenantScope takes an org id, so a query
// that forgot its tenant is not a bug that can be written.
func (p Principal) Scope() (db.TenantScope, error) {
	if !p.Valid() {
		return db.TenantScope{}, errs.Unauthorized("unauthenticated", "authentication required")
	}
	s, err := db.NewTenantScope(p.OrgID)
	if err != nil {
		return db.TenantScope{}, errs.Unauthorized("unauthenticated", "authentication required")
	}
	return s, nil
}

// ActorKind is how this principal appears on a timeline: `user`, `system` or
// `slack`. It is ACTOR METADATA on a signal and never a subject (CONTEXT.md §1b).
func (p Principal) ActorKind() string {
	switch p.Kind {
	case KindSession, KindPAT:
		return "user"
	case KindSlack:
		return "slack"
	default:
		return "system"
	}
}

// ActorID is the stable id recorded next to a human action.
func (p Principal) ActorID() string {
	if p.UserID != uuid.Nil {
		return p.UserID.String()
	}
	return ""
}

// ActorLabel is the human-readable attribution shown on the timeline.
func (p Principal) ActorLabel() string {
	switch {
	case p.DisplayName != "":
		return p.DisplayName
	case p.Email != "":
		return p.Email
	default:
		return "system"
	}
}

type ctxKey struct{}

// Into returns a context carrying p.
func Into(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// From returns the principal travelling in ctx.
func From(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok && p.Valid()
}

// Require returns the principal or a 401. Handlers behind the auth middleware
// still call it: "the middleware guarantees it" is exactly the assumption that
// stops being true the first time a route is mounted somewhere else.
func Require(ctx context.Context) (Principal, error) {
	p, ok := From(ctx)
	if !ok {
		return Principal{}, errs.Unauthorized("unauthenticated", "authentication required")
	}
	return p, nil
}

// Scope is Require followed by Scope, which is what almost every handler wants.
func Scope(ctx context.Context) (Principal, db.TenantScope, error) {
	p, err := Require(ctx)
	if err != nil {
		return Principal{}, db.TenantScope{}, err
	}
	s, err := p.Scope()
	if err != nil {
		return Principal{}, db.TenantScope{}, err
	}
	return p, s, nil
}

// System builds the principal a background worker acts as. It is not reachable
// from any HTTP path.
func System(orgID uuid.UUID) Principal {
	return Principal{Kind: KindSystem, OrgID: orgID, DisplayName: "system"}
}
