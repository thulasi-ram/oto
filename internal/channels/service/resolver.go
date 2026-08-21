package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ConnectionStore is the narrow, read-only view this service takes of
// connections — just enough to unseal one's credential. `channels/api`
// declares a wider CRUD port over the same repository; this one is
// deliberately smaller because resolution never writes.
type ConnectionStore interface {
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Connection, error)
}

// ConversationRegistry is the subset of `channels/registry` this service uses:
// just the optional per-provider capability, never Open or Render — a
// resolution never mints a delivery Channel.
type ConversationRegistry interface {
	ResolveConversation(
		ctx context.Context, t domain.Type, cred domain.Credential, query domain.ConversationQuery,
	) (domain.ConversationResult, error)
}

// ResolverOptions are the Resolver's dependencies.
type ResolverOptions struct {
	Store    ConnectionStore
	Creds    CredentialResolver
	Registry ConversationRegistry
}

// Resolver answers "what is the other half of this Slack channel" for the
// settings UI, at the moment an operator is naming one destination — not at
// delivery time, and not by reading Slack back to reconstruct oto's own state
// (C9, ADR 0008). See domain.ConversationResolver for why this capability
// exists at all.
type Resolver struct {
	store    ConnectionStore
	creds    CredentialResolver
	registry ConversationRegistry
}

// NewResolver builds the resolver.
func NewResolver(o ResolverOptions) (*Resolver, error) {
	if o.Store == nil || o.Registry == nil {
		return nil, errs.New(errs.KindInternal, "channels_resolver_deps",
			"a connection store and a registry are required")
	}
	return &Resolver{store: o.Store, creds: o.Creds, registry: o.Registry}, nil
}

// ResolveConversation opens the connection's credential and asks the provider.
func (r *Resolver) ResolveConversation(
	ctx context.Context, scope db.TenantScope, connectionID uuid.UUID, query domain.ConversationQuery,
) (domain.ConversationResult, error) {
	conn, err := r.store.Get(ctx, scope, connectionID)
	if err != nil {
		return domain.ConversationResult{}, err
	}
	if conn.Deleted() {
		return domain.ConversationResult{}, errs.NotFound("connection_deleted",
			"this connection has been deleted")
	}

	cred, err := r.credential(ctx, scope, conn)
	if err != nil {
		return domain.ConversationResult{}, err
	}
	return r.registry.ResolveConversation(ctx, conn.Type, cred, query)
}

// credential unseals the connection's secret, or returns the empty credential
// when it has none — the same shape as Tester.credential, one layer up.
func (r *Resolver) credential(ctx context.Context, scope db.TenantScope, conn domain.Connection) (domain.Credential, error) {
	if conn.CredentialID == nil {
		return domain.Credential{}, nil
	}
	if r.creds == nil {
		return domain.Credential{}, errs.New(errs.KindInternal, "credential_resolver_missing",
			"this deployment cannot unseal channel credentials")
	}
	kind, values, err := r.creds.Resolve(ctx, scope, *conn.CredentialID)
	if err != nil {
		return domain.Credential{}, err
	}
	return domain.Credential{Kind: kind, Values: values}, nil
}
