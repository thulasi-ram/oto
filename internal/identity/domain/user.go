package domain

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

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
//
// ⚠️ THE ZERO VALUE IS MEANINGFUL SINCE 00074: it is `users.email IS NULL`, which
// is a SHADOW MEMBER — somebody who has never given oto an address. It is not
// "unset because we have not read it yet", and no reader may treat the empty
// string as a lookup key: `Email.IsZero()` is the question, and `NewEmail("")`
// still fails, so a zero Email cannot be produced by parsing anything.
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
	ID    uuid.UUID
	OrgID uuid.UUID
	// Email is ZERO for a SHADOW MEMBER (`users.email IS NULL`, migration 00074).
	// Every reader of this field must tolerate that; `IsShadow` is the question to
	// ask rather than comparing against "".
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

// NewShadowUser builds a SHADOW MEMBER: a real `users` row, in a real org, that
// carries NO EMAIL and NO PASSWORD (git-bug a74d6b2, migration 00074).
//
// ⭐⭐ WHAT IT IS FOR IS A PRINCIPAL UUID, AND NOTHING ELSE. A Slack workspace
// member who has never linked an oto account presses `Snooze 1h`; Slack's ack
// times out and redelivers the interaction. `idempotency_claims.principal_id` is
// NOT NULL and `idempotency.Claim.validate` refuses `uuid.Nil`, so with no user
// row there was no principal, no claim, and `alerts/service.Snooze` executed the
// snooze TWICE — two `alert_snoozes` rows and two
// `alert.unsnoozed(superseded)`/`alert.snoozed` pairs for one human press. This
// row is what gives that press a principal, so the redelivery is refused.
//
// ⛔ IT IS NOT AN ACCOUNT, AN INVITATION OR A CLAIM ABOUT A PERSON. It cannot log
// in — no password hash, and a NULL email that `resolveByEmailSQL`'s `u.email = $1`
// can never match — it has no session, no PAT and no way to acquire either. And it
// is deliberately NOT given a synthetic address: an invented mailbox is
// indistinguishable from a real one at every reader, while a zero Email answers
// "has this person given oto an address" exactly once, for all of them. See 00074's
// header for the whole argument.
//
// ⚠️ `displayName` IS THE SLACK HANDLE, and it is the one field with no fallback in
// the schema: `users_name_ck` is `length(btrim(display_name)) BETWEEN 1 AND 120`, so
// an empty label is refused here rather than turned into a 23514 at the INSERT.
// `SlackIdentity.ActorLabel()` is what callers pass, because it already answers
// "handle, or the member id when even that is unknown" and is never empty for a
// well-formed identity.
func NewShadowUser(id, orgID uuid.UUID, displayName string) (User, error) {
	if id == uuid.Nil {
		return User{}, errs.Validation("invalid_user_id", "a user needs an id")
	}
	if orgID == uuid.Nil {
		return User{}, errs.Validation("invalid_user_org", "a user belongs to exactly one org")
	}
	displayName = strings.TrimSpace(TruncateDisplayName(displayName))
	if l := len(displayName); l < MinDisplayNameBytes || l > MaxDisplayNameBytes {
		return User{}, errs.Validation("invalid_display_name",
			"display_name must be 1..120 characters")
	}
	return User{
		ID:          id,
		OrgID:       orgID,
		DisplayName: displayName,
		// Both zero, and both load-bearing. See the doc comment.
		Email:        Email{},
		PasswordHash: PasswordHash{},
	}, nil
}

// TruncateDisplayName clips a label to `users_name_ck`'s ceiling ON A RUNE
// BOUNDARY.
//
// ⚠️ THE BOUNDARY IS THE POINT. `MaxDisplayNameBytes` is measured in BYTES, like
// every other bound in this package, while the DDL's `length(btrim(display_name))`
// counts CHARACTERS — so a byte ceiling is the stricter of the two and cannot
// admit a row the CHECK would refuse. What a naive `s[:120]` CAN do is split a
// multi-byte rune and hand Postgres a byte sequence that is not valid UTF-8, which
// a UTF8 database rejects with a 22021 that names no column. A Slack handle is
// usually ASCII and this is usually a no-op; "usually" is not a bound.
func TruncateDisplayName(s string) string {
	if len(s) <= MaxDisplayNameBytes {
		return s
	}
	cut := MaxDisplayNameBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// IsShadow reports whether this row is a SHADOW MEMBER: a user oto minted for a
// Slack presser who has never given it an address (00074).
//
// ⭐ IT IS THE QUESTION EVERY EMAIL READER SHOULD ASK, rather than comparing
// `Email.String()` against "". The absence of an address is a FACT about the
// person, not a missing field, and naming it here means a renderer, a mapper and a
// log line all decide it the same way.
func (u User) IsShadow() bool { return u.Email.IsZero() }

// Active reports whether the user has not been soft disabled.
func (u User) Active() bool { return u.DisabledAt == nil }

// CanPasswordLogin reports whether this user may authenticate with a password.
//
// It is deliberately AND-ed with Active(): a disabled user with a hash still on
// the row must not be able to log in, and expressing that here rather than at
// each call site is what stops the third caller from forgetting.
//
// ⛔ IT IS ALSO AND-ED WITH `!IsShadow()`, AND THAT CONJUNCT IS DELIBERATELY
// REDUNDANT. A shadow member (00074) is already refused twice over: the row's
// `password_hash` is NULL so `PasswordHash.IsZero()` is true, and
// `resolveByEmailSQL` compares `u.email = $1`, which is never TRUE for a NULL
// email, so `Login` never even holds the row. This third refusal costs one
// comparison and removes a whole class of future defect: the day something wants
// to WRITE a password onto an existing row — an invite flow, an SSO shim, a repair
// script — the two refusals it would defeat are both about the hash and the query,
// and neither of them is about whether this person ever gave oto an address. A
// credential is only ever proof of an identity somebody claimed; a shadow row is
// oto's own record of a Slack press, and nobody claimed it.
func (u User) CanPasswordLogin() bool {
	return u.Active() && !u.IsShadow() && !u.PasswordHash.IsZero()
}
