package domain

import (
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// The bounds of `users` (SPEC §D.1). Each one mirrors a named DDL CHECK, and
// R9 binds the three copies — DTO tag, this constructor, the CHECK — to be
// identical.
//
// ⚠️ PatternUserEmail mirrors `users_email_ck` and is declared HERE rather than
// in `internal/platform/validate` only because that package does not yet carry
// it. It is written byte-for-byte as `00003_identity.sql` spells it, including
// the POSIX class `[:space:]`, which Go's RE2 understands natively — so this
// string can be lifted into `validate/patterns.go` unchanged the next time that
// package is touched, and `TestValidatorMatchesDDL` will accept it. Do not
// "simplify" it: the drift test compares strings, not regular-expression
// semantics.
const (
	// PatternUserEmail mirrors users_email_ck.
	PatternUserEmail = `^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$`

	// MaxEmailBytes mirrors the length half of users_email_ck.
	MaxEmailBytes = 254
	// MaxDisplayNameBytes mirrors users_name_ck.
	MaxDisplayNameBytes = 120
	// MinDisplayNameBytes mirrors users_name_ck.
	MinDisplayNameBytes = 1
)

// userEmailRe evaluates PatternUserEmail.
var userEmailRe = regexp.MustCompile(PatternUserEmail)

// Email is a user's login identity, normalised to lower case because the column
// is CITEXT and `users_email_uniq` is therefore case-insensitive. Normalising in
// the constructor is what stops `Priya@example.com` and `priya@example.com` from
// looking like two accounts in Go and one row in Postgres.
type Email struct{ v string }

// NewEmail parses and normalises an address, enforcing users_email_ck.
func NewEmail(s string) (Email, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) == 0 || len(s) > MaxEmailBytes || !userEmailRe.MatchString(s) {
		return Email{}, errs.Validation("invalid_email",
			"email must be a valid address of at most 254 characters")
	}
	return Email{v: s}, nil
}

// String renders the address.
func (e Email) String() string { return e.v }

// IsZero reports whether the address is unset.
func (e Email) IsZero() bool { return e.v == "" }

// PasswordHashPrefix mirrors users_pw_ck: oto stores argon2id encoded hashes and
// nothing else, so a bcrypt or scrypt hash cannot be written by accident.
const PasswordHashPrefix = "$argon2id$"

// MaxPasswordHashBytes bounds the encoded hash. An argon2id encoding of oto's
// parameters is ~100 bytes; the ceiling exists so that a mapper bug cannot push
// an unbounded string at the column.
const MaxPasswordHashBytes = 512

// PasswordHash is an argon2id encoded hash.
//
// It is a value object rather than a string for one reason: its String method
// returns a REDACTION. A password hash that reaches a log line is an offline
// cracking target, and `slog.Any("user", u)` is exactly how one gets there. The
// real bytes come out only through Encoded(), which is named so that a reviewer
// notices the call site.
type PasswordHash struct{ v string }

// NewPasswordHash wraps an already-computed argon2id encoding. Computing one is
// `platform/authn`'s job; the domain only states what shape a stored hash has.
func NewPasswordHash(encoded string) (PasswordHash, error) {
	if !strings.HasPrefix(encoded, PasswordHashPrefix) || len(encoded) > MaxPasswordHashBytes {
		return PasswordHash{}, errs.Validation("invalid_password_hash",
			"a stored password hash must be an argon2id encoding")
	}
	return PasswordHash{v: encoded}, nil
}

// NoPassword is the hash of a user who cannot log in with a password —
// `users.password_hash IS NULL`, which is SSO-only or Slack-only, not "no
// password required".
func NoPassword() PasswordHash { return PasswordHash{} }

// Encoded returns the argon2id encoding. Every caller of this is a caller that
// must not log its result.
func (h PasswordHash) Encoded() string { return h.v }

// IsZero reports whether password login is disabled for this user.
func (h PasswordHash) IsZero() bool { return h.v == "" }

// String deliberately hides the hash from every formatter, logger and stack
// trace in the process.
func (h PasswordHash) String() string { return "[redacted]" }

// User is a human principal within one Org.
//
// Acknowledgement identity is stored because it is operationally necessary; per
// person response-time metrics are unrepresentable by construction (R8), and
// nothing on this type carries a workload, a rota or an obligation
// (CONTEXT.md §1b, FR-1).
type User struct {
	ID          uuid.UUID
	OrgID       uuid.UUID
	Email       Email
	DisplayName string
	// PasswordHash is zero when password login is disabled.
	PasswordHash PasswordHash
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// DisabledAt is a soft disable. A disabled user keeps their acked_by rows so
	// the timeline stays honest.
	DisabledAt *time.Time
}

// NewUser builds a User, enforcing every invariant `users_*_ck` enforces. If you
// can construct it, it is valid: there is no optional Validate() in this package.
func NewUser(id, orgID uuid.UUID, email Email, displayName string, hash PasswordHash) (User, error) {
	if id == uuid.Nil {
		return User{}, errs.Validation("invalid_user_id", "a user needs an id")
	}
	if orgID == uuid.Nil {
		return User{}, errs.Validation("invalid_user_org", "a user belongs to exactly one org")
	}
	if email.IsZero() {
		return User{}, errs.Validation("invalid_email", "a user needs an email address")
	}
	displayName = strings.TrimSpace(displayName)
	if l := len(displayName); l < MinDisplayNameBytes || l > MaxDisplayNameBytes {
		return User{}, errs.Validation("invalid_display_name",
			"display_name must be 1..120 characters")
	}
	return User{
		ID:           id,
		OrgID:        orgID,
		Email:        email,
		DisplayName:  displayName,
		PasswordHash: hash,
	}, nil
}

// Active reports whether the user has not been soft disabled.
func (u User) Active() bool { return u.DisabledAt == nil }

// CanPasswordLogin reports whether this user may authenticate with a password.
//
// It is deliberately AND-ed with Active(): a disabled user with a hash still on
// the row must not be able to log in, and expressing that here rather than at
// each call site is what stops the third caller from forgetting.
func (u User) CanPasswordLogin() bool { return u.Active() && !u.PasswordHash.IsZero() }
