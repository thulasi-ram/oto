package api

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/registry"
	"github.com/thulasiram/oto/internal/channels/service"
	"github.com/thulasiram/oto/internal/platform/db"
)

// Every interface in this file is a PORT DECLARED BY THE CONSUMER (CONTEXT.md
// §5.4). This layer says exactly what it calls; `internal/app/container.go`
// decides what satisfies it.

// ProviderRegistry is the subset of `channels/registry` the HTTP layer uses.
//
// ⛔ `ConfigSchema` returns the SAME BYTES the server validates against. That
// identity is the whole argument for a schema over a hand-written DTO (§L.5): the
// settings form is generated from it, so the UI and the server cannot disagree
// about what a valid config is, and adding a provider needs no UI code at all.
type ProviderRegistry interface {
	Descriptors() []domain.Descriptor
	ConfigSchema(t domain.Type) (json.RawMessage, error)
	// ValidateConfig runs the provider's JSON Schema PLUS the rules a schema
	// cannot express — an `Authorization` header in a webhook's `headers`, a URL
	// resolving to a loopback address.
	ValidateConfig(ctx context.Context, t domain.Type, raw json.RawMessage) error
	Types() []domain.Type
}

// Compile-time proof that the registry satisfies the port this layer declares.
var _ ProviderRegistry = (*registry.Registry)(nil)

// ChannelStore is the persistence side, satisfied by
// `*channels/repository.ChannelRepository`.
//
// ⚠️ ARCHITECTURAL NOTE. CONTEXT.md §5.1 says `api` calls `service`. The channels
// module's service owns the one operation with behaviour (`Test`); the CRUD here
// is genuinely CRUD — validate against the published schema, then write — and
// interposing a pass-through service would add a layer that does nothing except
// make the schema check harder to find. This layer therefore declares the narrow
// store port it needs and the composition root injects the repository. `api`
// still does not IMPORT `repository`, so depguard's rule holds.
//
// ⛔ AND `Create` IS NO LONGER ON IT. The note above held right up to the moment
// one of these writes had to commit beside something else: an `Idempotency-Key`
// claim refuses to run outside the caller's transaction, so a handler wired
// straight to the repository had nowhere to take one. See ChannelWriter.
type ChannelStore interface {
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Instance, error)
	List(ctx context.Context, s db.TenantScope, includeDeleted bool, p db.Keyset) ([]domain.Instance, db.Cursor, error)
	Update(ctx context.Context, s db.TenantScope, id uuid.UUID, p domain.InstancePatch) (domain.Instance, error)
	SoftDelete(ctx context.Context, s db.TenantScope, id uuid.UUID) error
	// ReferencingPolicies names the live policies routing to this channel. A
	// channel still named by an enabled policy is a 409, never a cascade: silently
	// orphaning a policy's only destination would make it stop notifying without
	// saying so.
	ReferencingPolicies(ctx context.Context, s db.TenantScope, id uuid.UUID) ([]string, error)
}

// CredentialWriter seals a destination's secret.
//
// ⛔ The `values` map is secret material. It arrives on a write-only DTO field,
// is handed straight here, and is never logged, echoed or retained. There is no
// read method on this port, because there is nothing any endpoint may read back.
type CredentialWriter interface {
	CreateCredential(ctx context.Context, s db.TenantScope, kind string, values map[string]string) (uuid.UUID, error)
	RotateCredential(ctx context.Context, s db.TenantScope, id uuid.UUID, kind string, values map[string]string) error
}

// ChannelWriter owns the two operations whose side effect is worth claiming, and
// is satisfied by `*channels/service.Writer`.
//
// ⭐⭐ BOTH TAKE THE CALLER'S `Idempotency-Key` INTENT AND NEITHER TAKES THE CLAIM
// HERE. A claim has to be taken inside the transaction of the act it guards, and
// that transaction belongs to the service — so what crosses this seam is the
// intent, and the service decides whether this deployment can honour it (a
// `503`), whether the key is held for a different body (a `409`), and whether the
// call is a replay.
//
// ⛔ `TestChannel` IS THE SHARPEST OPERATION IN THE WHOLE CONTRACT. Its side
// effect is a message a real person reads in a real room, and nothing can undo
// one. It called through to the tester unconditionally on every request while the
// contract's header promised the opposite (ticket a6cc834).
type ChannelWriter interface {
	CreateChannel(ctx context.Context, s db.TenantScope, in domain.NewInstance, idem service.Idempotency) (domain.Instance, error)
	TestChannel(ctx context.Context, s db.TenantScope, id uuid.UUID, idem service.Idempotency) (domain.TestResult, error)
}

// Compile-time proof that the writer satisfies the port this layer declares.
var _ ChannelWriter = (*service.Writer)(nil)

// SlackInteractions receives an already-verified Slack block-action payload.
//
// ⛔ THE SLACK SDK IS NOT IMPORTED HERE, AND MUST NOT BE: it lives only in
// `channels/providers/slack`. This layer verifies the HMAC itself with
// `crypto/hmac` over the RAW body and hands the opaque envelope on — which is
// also why the port takes `json.RawMessage` rather than a parsed Slack type.
//
// The handler acknowledges within Slack's three-second window regardless of what
// this returns, so a slow consumer can never make a user see "This app is not
// responding".
type SlackInteractions interface {
	Handle(ctx context.Context, payload json.RawMessage) error
}
