package db

import (
	"errors"

	"github.com/google/uuid"
)

// TenantScope is the proof that a caller has been authenticated into exactly one
// org. Every repository method takes one as its second argument, so there is no
// way to write a query that forgets its org_id.
//
// The field is unexported and there is no literal construction path outside this
// package: a TenantScope can only come from NewTenantScope, which the authn
// package calls after it has resolved a Principal.
type TenantScope struct {
	orgID uuid.UUID
}

// ErrNoTenant is returned when a scope is built without an org.
var ErrNoTenant = errors.New("db: tenant scope requires a non-zero org id")

// NewTenantScope builds a scope. Only platform/authn should call this, from an
// already-authenticated principal.
func NewTenantScope(orgID uuid.UUID) (TenantScope, error) {
	if orgID == uuid.Nil {
		return TenantScope{}, ErrNoTenant
	}
	return TenantScope{orgID: orgID}, nil
}

// OrgID is the org this scope authorises. It is the first column of every
// composite index in the schema.
func (s TenantScope) OrgID() uuid.UUID { return s.orgID }

// Valid reports whether the scope carries an org.
func (s TenantScope) Valid() bool { return s.orgID != uuid.Nil }

// String renders the scope for logs.
func (s TenantScope) String() string { return "org:" + s.orgID.String() }
