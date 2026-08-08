package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// MaxUserAgentBytes bounds what is captured from the User-Agent header. The
// column has no CHECK; the bound exists because the header is attacker-supplied
// and unbounded, and it is stored for one screen that nothing authorises on.
const MaxUserAgentBytes = 512

// Session is a browser session for the SolidJS UI.
//
// The cookie carries the secret; this row carries its sha256. The session id is
// therefore NOT the credential — but it is still treated as one throughout
// (never logged, never returned in a response body), because an id that appears
// in a log next to an org is a correlation an operator's laptop should not be
// able to make from a shipped log stream.
type Session struct {
	ID     uuid.UUID
	OrgID  uuid.UUID
	UserID uuid.UUID
	Hash   TokenHash
	// UserAgent is captured at creation for the "your sessions" screen. It is
	// NEVER used for authorisation: pinning a session to a user agent locks out
	// every user whose browser auto-updates, and stops no attacker who can read
	// the cookie.
	UserAgent string
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// NewSession mints a session that lives for ttl from createdAt, enforcing
// sessions_expiry_ck.
//
// ttl is a REQUIRED argument rather than a default: a session with no expiry is
// a permanent credential in a cookie, and the type system should not make one
// available by leaving a field unset.
func NewSession(
	id, orgID, userID uuid.UUID,
	hash TokenHash,
	userAgent string,
	createdAt time.Time,
	ttl time.Duration,
) (Session, error) {
	if id == uuid.Nil {
		return Session{}, errs.Validation("invalid_session_id", "a session needs an id")
	}
	if orgID == uuid.Nil {
		return Session{}, errs.Validation("invalid_session_org", "a session belongs to exactly one org")
	}
	if userID == uuid.Nil {
		return Session{}, errs.Validation("invalid_session_user", "a session belongs to a user")
	}
	if hash.IsZero() {
		return Session{}, errs.Validation("invalid_session_hash", "a session needs a hash")
	}
	if ttl <= 0 {
		return Session{}, errs.Validation("invalid_session_ttl", "a session must expire after it starts")
	}
	if createdAt.IsZero() {
		return Session{}, errs.Validation("invalid_session_time", "a session needs a creation time")
	}

	createdAt = createdAt.UTC()
	return Session{
		ID:        id,
		OrgID:     orgID,
		UserID:    userID,
		Hash:      hash,
		UserAgent: TruncateUserAgent(userAgent),
		CreatedAt: createdAt,
		ExpiresAt: createdAt.Add(ttl),
	}, nil
}

// TruncateUserAgent bounds and cleans a User-Agent header for storage.
func TruncateUserAgent(ua string) string {
	ua = strings.TrimSpace(ua)
	if len(ua) > MaxUserAgentBytes {
		ua = ua[:MaxUserAgentBytes]
	}
	return ua
}

// Revoked reports whether the session was explicitly ended.
func (s Session) Revoked() bool { return s.RevokedAt != nil }

// Expired reports whether the session's expiry has passed.
func (s Session) Expired(now time.Time) bool {
	return s.ExpiresAt.IsZero() || !now.Before(s.ExpiresAt)
}

// Live is the single predicate session authentication asks, and it FAILS CLOSED.
//
// A zero ExpiresAt — a row a mapper filled in wrongly, or a struct nobody passed
// through NewSession — reads as EXPIRED, not as eternal. That asymmetry is the
// whole point: the failure mode of a bug in this path must be "everybody is
// logged out", never "one session never ends".
//
// Expiry is enforced HERE and again in the SQL predicate. Two enforcements, one
// meaning: the query keeps a stale row from ever being scanned, and this keeps a
// cached or in-flight session from outliving its window.
func (s Session) Live(now time.Time) bool {
	return !s.Revoked() && !s.Expired(now)
}
