package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ⚠️ WHY THIS TYPE EXISTS.
//
// A Connection is the ORG-WIDE setup a provider needs once: a Slack workspace's
// bot token and team id, or a webhook receiver family's shared basic/bearer
// credential or outbound signing secret. An Instance (see instance.go) is one
// destination — a specific #channel, a specific URL — that references a
// Connection by id. Several Instances share one Connection; that sharing is the
// entire point of the split (SPEC §A — see the ADR introducing it).
//
// Connection carries no renderer, verbosity, thread-updates or health: those are
// all properties of one destination, not of the org-wide setup behind it.

// Bounds mirrored from the `channel_connections` CHECK constraints.
const (
	// MaxConnectionNameLength is channel_connections_name_ck.
	MaxConnectionNameLength = 120
)

// Connection is one org-wide provider setup — a row of `channel_connections`.
type Connection struct {
	ID    uuid.UUID
	OrgID uuid.UUID
	Type  Type
	Name  string

	// Config is the non-secret, connection-level settings blob — e.g. a Slack
	// workspace's `team_id`. Validated against the provider's
	// ConnectionConfigSchema on every write, same as Instance.Config.
	Config json.RawMessage

	// CredentialID names the sealed secret, or is nil. channel_connections_cred_ck
	// makes it MANDATORY for `slack`; a webhook connection may have none.
	CredentialID *uuid.UUID
	// CredentialKind and CredentialRotatedAt are the safe-to-show half of the
	// credential, joined in for the detail view — never the secret itself.
	CredentialKind      string
	CredentialRotatedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// Deleted reports whether the connection is soft deleted.
func (c Connection) Deleted() bool { return c.DeletedAt != nil }

// NewConnection is the create command.
type NewConnection struct {
	// ID lets the caller NAME the row before it exists, for the same
	// `Idempotency-Key` reason NewInstance.ID exists — a retry that inserted
	// first would hit channel_connections_name_uniq and be answered with a name
	// conflict rather than the connection the caller already created.
	ID           uuid.UUID
	Type         Type
	Name         string
	Config       json.RawMessage
	CredentialID *uuid.UUID
}

// ConnectionPatch is the partial update. Every field is a pointer for the same
// reason InstancePatch's are: "absent" and "set to the zero value" must not be
// confused.
//
// ⛔ `Type` is absent. A connection's provider is its identity, same as an
// Instance's — a Slack connection reinterpreted as a webhook would carry a bot
// token that means nothing to the webhook provider.
type ConnectionPatch struct {
	Name   *string
	Config *json.RawMessage
	// CredentialID is a double pointer: nil leaves it, a pointer to nil detaches
	// it, a pointer to a pointer attaches a new one.
	CredentialID **uuid.UUID
}

// IsEmpty reports whether the patch would change nothing.
func (p ConnectionPatch) IsEmpty() bool {
	return p.Name == nil && p.Config == nil && p.CredentialID == nil
}
