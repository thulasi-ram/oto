package domain

import (
	"github.com/google/uuid"
)

// Subject is the resolved answer to "who is this, and in which tenant".
//
// It exists so that resolving a credential is ONE round trip. The alternative —
// resolve the session, then read the user, then read the org — puts three
// queries on every authenticated request, and the two extra ones exist only to
// fill in an attribution label. A join returns the same facts once.
//
// It is deliberately NOT a User and NOT an Org. It carries the handful of fields
// a Principal needs and nothing else: no settings, no password hash, no
// timestamps. A type that grew towards User would eventually carry the hash into
// every request context in the process.
type Subject struct {
	OrgID   uuid.UUID
	OrgSlug string
	OrgName string

	UserID      uuid.UUID
	Email       Email
	DisplayName string
}

// Valid reports whether the subject names a tenant and a human.
func (s Subject) Valid() bool { return s.OrgID != uuid.Nil && s.UserID != uuid.Nil }

// AuthenticatedToken pairs a live PAT with the subject it resolves to.
//
// The pairing is what makes the verification step honest: the caller has a
// candidate row and the identity that row would grant, and has NOT yet compared
// digests. Nothing here is authenticated until that comparison succeeds.
type AuthenticatedToken struct {
	Token   APIToken
	Subject Subject
}

// AuthenticatedSession pairs a live session with the subject it resolves to.
type AuthenticatedSession struct {
	Session Session
	Subject Subject
}
