package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// The ports this service declares FOR ITSELF (CONTEXT.md §5.3). The concrete
// implementations in `identity/repository` are injected by
// `internal/app/container.go`; nothing here names them, which is what lets a
// test substitute an in-memory double without a database.
//
// Every method takes a db.TenantScope except the four resolvers, which are the
// queries that PRODUCE a scope and therefore cannot consume one — they are named
// `Resolve…` uniformly so that "is this org-scoped?" is answerable from the call
// site alone — and `OrgReader.ListLive`, the one maintenance walk, which
// produces no scope at all (see its own comment).

// OrgReader reads the tenant root.
//
// ⚠️ UpdateSettings is the ONLY write on this port, and it writes exactly one
// column. `orgs.slug` and `orgs.name` have no write path and are not missing one:
// a tenant renaming itself invalidates every deep link oto has ever emitted, and
// v1 has no RBAC to decide who may do it (R2).
type OrgReader interface {
	Get(ctx context.Context, s db.TenantScope) (domain.Org, error)
	UpdateSettings(ctx context.Context, s db.TenantScope, p domain.SettingsPatch) (domain.Org, error)

	// ListLive reads ONE keyset page of LIVE orgs, walking the primary key from
	// `after`.
	//
	// ⚠️ It is the one read on this port that sees more than the caller's own
	// org, and it is not a resolver either: nothing external chooses its input
	// and no scope comes out of it. It exists for exactly one caller —
	// MaxRetention — whose question, "the widest window ANY tenant asked for",
	// is a whole-population reduce that no TenantScope could ask.
	ListLive(ctx context.Context, after uuid.UUID, limit int) ([]domain.Org, error)
}

// TxRunner runs a unit of work inside ONE transaction, satisfied by
// `identity/repository.TxRunner`.
//
// It exists for the ingest-token rotation: the mint and the revocation sweep
// beside it must commit together or not at all (see IssueIngestToken). It is a
// port rather than a pool because a service that names pgx has stopped being
// testable without a database — and because the transaction travels in the
// context, a caller that already opened one (the sources service wraps a
// rotation in its own unit of work) is joined rather than nested.
type TxRunner interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// UserReader reads `users`.
type UserReader interface {
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.User, error)
	GetByEmail(ctx context.Context, s db.TenantScope, email domain.Email) (domain.User, error)
	ListMembers(ctx context.Context, s db.TenantScope, k db.Keyset) ([]domain.User, db.Cursor, error)

	// ResolveByEmail is UNSCOPED: the login request carries no org, so this is
	// what produces one. It reports not-found for an ambiguous address.
	ResolveByEmail(ctx context.Context, email domain.Email) (domain.User, error)
}

// TokenStore reads and writes `api_tokens`.
type TokenStore interface {
	Insert(ctx context.Context, s db.TenantScope, t domain.APIToken) error
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.APIToken, error)
	List(ctx context.Context, s db.TenantScope, kind domain.TokenKind, userID uuid.UUID, k db.Keyset) ([]domain.APIToken, db.Cursor, error)
	Revoke(ctx context.Context, s db.TenantScope, id uuid.UUID, at time.Time) (bool, error)
	TouchLastUsed(ctx context.Context, s db.TenantScope, id uuid.UUID, at time.Time) error

	// ResolveByPrefix is UNSCOPED and AUTHENTICATES NOTHING: it returns the live
	// candidates sharing a public display prefix, and the caller decides with a
	// constant-time comparison.
	ResolveByPrefix(ctx context.Context, prefix domain.TokenPrefix, now time.Time) ([]domain.AuthenticatedToken, error)
}

// SessionStore reads and writes `sessions`.
type SessionStore interface {
	Insert(ctx context.Context, s db.TenantScope, sess domain.Session) error
	Revoke(ctx context.Context, s db.TenantScope, id uuid.UUID, at time.Time) error
	RevokeAllForUser(ctx context.Context, s db.TenantScope, userID uuid.UUID, at time.Time) error
	DeleteExpired(ctx context.Context, before time.Time, batch int) (int64, error)

	// ResolveByHash is UNSCOPED: a cookie names no org. Its SQL already excludes
	// revoked and expired rows; the service checks Live() again anyway.
	ResolveByHash(ctx context.Context, hash domain.TokenHash, now time.Time) (domain.AuthenticatedSession, error)
}

// SlackIdentityStore reads and writes `slack_identities`.
type SlackIdentityStore interface {
	Upsert(ctx context.Context, s db.TenantScope, si domain.SlackIdentity, now time.Time) (domain.SlackIdentity, error)
	GetByUser(ctx context.Context, s db.TenantScope, userID uuid.UUID) (domain.SlackIdentity, error)
	GetBySlackUser(ctx context.Context, s db.TenantScope, team domain.SlackTeamID, member domain.SlackUserID) (domain.SlackIdentity, error)
	Link(ctx context.Context, s db.TenantScope, id, userID uuid.UUID, at time.Time) (domain.SlackIdentity, error)

	// ResolveBySlackUser is UNSCOPED: a Slack interaction names a workspace and a
	// member, never an org. Its caller has already authenticated by Slack's HMAC
	// signature (§H.8).
	ResolveBySlackUser(ctx context.Context, team domain.SlackTeamID, member domain.SlackUserID) (domain.SlackIdentity, error)
}
