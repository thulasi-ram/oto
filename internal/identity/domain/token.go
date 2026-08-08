package domain

import (
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// The bounds of `api_tokens` (SPEC §D.1), each mirroring a named DDL CHECK.
const (
	// PatternTokenPrefix mirrors api_tokens_prefix_ck.
	//
	// ⚠️ Declared here rather than in `internal/platform/validate` for the same
	// reason PatternUserEmail is, and written exactly as the migration spells it
	// so it can be lifted into `validate/patterns.go` unchanged.
	PatternTokenPrefix = `^oto_(pat|ingest)_[A-Za-z0-9]{4}$`

	// TokenPrefixRandomChars is how many characters of the random half of a
	// secret are kept for display.
	//
	// ⭐ THE STORED PREFIX IS `<kind literal> + TokenPrefixRandomChars`, and its
	// LENGTH THEREFORE DEPENDS ON THE KIND. `oto_pat_` is eight characters so a
	// PAT prefix is twelve; `oto_ingest_` is ELEVEN so an ingest prefix is
	// FIFTEEN. A single fixed length cannot be right for both, and assuming one
	// was is exactly how `POST /api/v1/sources` came to reject every request it
	// was ever sent: a twelve-character slice of an ingest secret is
	// `oto_ingest_X`, one random character, which api_tokens_prefix_ck refuses.
	//
	// api_tokens_prefix_ck — `^oto_(pat|ingest)_[A-Za-z0-9]{4}$` — has ALWAYS
	// admitted both lengths, and `prefix` is TEXT, so no migration is implied by
	// fixing this. The DDL was right; the Go constant was not.
	TokenPrefixRandomChars = 4

	// TokenPrefixLenPAT is the stored prefix length of a PAT: len("oto_pat_") + 4.
	TokenPrefixLenPAT = len(SecretPrefixPAT) + TokenPrefixRandomChars
	// TokenPrefixLenIngest is the stored prefix length of an ingest token:
	// len("oto_ingest_") + 4.
	TokenPrefixLenIngest = len(SecretPrefixIngest) + TokenPrefixRandomChars

	// MaxTokenPrefixLen bounds the longest prefix any kind can produce. It is the
	// display width the UI and the DTO documentation are written against.
	MaxTokenPrefixLen = TokenPrefixLenIngest

	// TokenHashBytes mirrors api_tokens_hash_ck and sessions_hash_ck: a sha256
	// digest, exactly 32 bytes. A shorter value would silently be a different
	// hash function.
	TokenHashBytes = 32

	// MinTokenNameBytes and MaxTokenNameBytes mirror api_tokens_name_ck.
	MinTokenNameBytes = 1
	MaxTokenNameBytes = 120
)

// The two secret prefixes. They are part of the credential, not decoration: the
// transport rejects a `oto_pat_…` presented to the ingest endpoint and an
// `oto_ingest_…` presented to the API before either reaches the database, which
// is one lookup an attacker does not get to cause.
const (
	// SecretPrefixPAT prefixes a personal access token.
	SecretPrefixPAT = "oto_pat_"
	// SecretPrefixIngest prefixes a per-AlertSource webhook token.
	SecretPrefixIngest = "oto_ingest_"
)

var tokenPrefixRe = regexp.MustCompile(PatternTokenPrefix)

// TokenKind discriminates the two bearer credentials (api_tokens_kind_ck).
type TokenKind string

// The closed kind set.
const (
	// TokenKindPAT is a user's personal access token: same access as a session.
	TokenKindPAT TokenKind = "pat"
	// TokenKindIngest is scoped to exactly one AlertSource and can read nothing
	// (SPEC §G.2).
	TokenKindIngest TokenKind = "ingest"
)

// NewTokenKind parses a kind, enforcing api_tokens_kind_ck.
func NewTokenKind(s string) (TokenKind, error) {
	switch TokenKind(s) {
	case TokenKindPAT:
		return TokenKindPAT, nil
	case TokenKindIngest:
		return TokenKindIngest, nil
	default:
		return "", errs.Validation("invalid_token_kind", "kind must be one of: pat, ingest")
	}
}

// SecretPrefix is the literal a secret of this kind must start with.
func (k TokenKind) SecretPrefix() string {
	if k == TokenKindIngest {
		return SecretPrefixIngest
	}
	return SecretPrefixPAT
}

// TokenHash is the sha256 digest of a presented secret.
//
// The domain names the SHAPE of a stored credential and never computes it:
// hashing is `platform/authn`'s job. String() is a redaction for the same reason
// PasswordHash's is — a digest is not a secret, but it is a lookup key on a
// unique index, and a log line containing one is a log line that identifies a
// specific live credential.
type TokenHash [TokenHashBytes]byte

// NewTokenHash wraps a digest, enforcing the 32-byte CHECK.
func NewTokenHash(b []byte) (TokenHash, error) {
	var h TokenHash
	if len(b) != TokenHashBytes {
		return TokenHash{}, errs.Validation("invalid_token_hash",
			"a token hash is a 32-byte sha256 digest")
	}
	copy(h[:], b)
	return h, nil
}

// Bytes returns the digest for a query parameter.
func (h TokenHash) Bytes() []byte { return h[:] }

// IsZero reports whether the digest is unset.
func (h TokenHash) IsZero() bool { return h == TokenHash{} }

// String hides the digest from every formatter in the process.
func (h TokenHash) String() string { return "[redacted]" }

// TokenPrefix is the leading, displayable slice of a secret. It is what an
// operator sees in the token list, and it is the LOOKUP KEY used to find the
// small candidate set a presented secret is then compared against in constant
// time — four random characters is not an identifier, so the comparison that
// follows is the one that decides.
type TokenPrefix struct{ v string }

// NewTokenPrefix parses a stored prefix, enforcing api_tokens_prefix_ck.
//
// ⚠️ The message names the SHAPE and not the regex. A problem+json body echoing
// `^oto_(pat|ingest)_[A-Za-z0-9]{4}$` publishes an internal invariant to every
// caller, including the ones probing for one (§L3).
func NewTokenPrefix(s string) (TokenPrefix, error) {
	if !tokenPrefixRe.MatchString(s) {
		return TokenPrefix{}, errs.Validation("invalid_token_prefix",
			"a token prefix is a kind literal followed by four alphanumeric characters")
	}
	return TokenPrefix{v: s}, nil
}

// PrefixLenOfKind is how many characters of a secret of this kind are stored.
func PrefixLenOfKind(k TokenKind) int {
	return len(k.SecretPrefix()) + TokenPrefixRandomChars
}

// PrefixOfSecret derives the stored prefix from a freshly minted or presented
// secret.
//
// ⭐ THE SPLIT IS KIND-RELATIVE, and this is the ONE place it lives, so the
// prefix written at creation and the prefix looked up at verification cannot
// drift. It reads the kind off the secret's own literal first, because the two
// literals are different lengths — see TokenPrefixRandomChars for what a single
// fixed length cost.
func PrefixOfSecret(secret string) (TokenPrefix, error) {
	kind, ok := KindOfSecret(secret)
	if !ok {
		return TokenPrefix{}, errs.Validation("invalid_token_prefix",
			"a token secret starts with its kind literal")
	}
	n := PrefixLenOfKind(kind)
	if len(secret) < n {
		return TokenPrefix{}, errs.Validation("invalid_token_prefix",
			"a token secret is longer than its prefix")
	}
	return NewTokenPrefix(secret[:n])
}

// KindOfSecret reports which credential a presented secret claims to be, from
// its prefix alone. It is a CLAIM and never an authorisation: the digest
// comparison is what authenticates.
func KindOfSecret(secret string) (TokenKind, bool) {
	switch {
	case strings.HasPrefix(secret, SecretPrefixIngest):
		return TokenKindIngest, true
	case strings.HasPrefix(secret, SecretPrefixPAT):
		return TokenKindPAT, true
	default:
		return "", false
	}
}

// String renders the prefix, which is safe: it identifies a token without
// revealing it, which is the entire reason the column exists.
func (p TokenPrefix) String() string { return p.v }

// IsZero reports whether the prefix is unset.
func (p TokenPrefix) IsZero() bool { return p.v == "" }

// APIToken is a bearer credential.
//
// ⭐ THE PREFIX/HASH SPLIT is the security shape of this type. The secret is
// shown exactly once, at creation, and is never stored: the row holds a sha256
// digest and a short display prefix, so a database disclosure yields
// neither a usable credential nor an offline attack worth mounting against 256
// bits of entropy. There is no field on this struct that could hold plaintext,
// which is what makes "the secret is never stored" a property of the type rather
// than a habit of its callers.
type APIToken struct {
	ID    uuid.UUID
	OrgID uuid.UUID
	// UserID is the owner of a PAT and is zero for an ingest token
	// (api_tokens_pat_user).
	UserID uuid.UUID
	Kind   TokenKind
	Name   string
	Hash   TokenHash
	Prefix TokenPrefix
	// SourceID is the one AlertSource an ingest token may post to, and is zero
	// for a PAT (api_tokens_ingest_scope).
	SourceID   uuid.UUID
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	CreatedAt  time.Time
	RevokedAt  *time.Time
}

// NewAPITokenParams carries the fields NewAPIToken needs. A params struct rather
// than nine positional arguments, because two adjacent uuid.UUIDs that mean
// different things is a bug waiting for a refactor.
type NewAPITokenParams struct {
	ID        uuid.UUID
	OrgID     uuid.UUID
	UserID    uuid.UUID
	Kind      TokenKind
	Name      string
	Hash      TokenHash
	Prefix    TokenPrefix
	SourceID  uuid.UUID
	ExpiresAt *time.Time
	CreatedAt time.Time
}

// NewAPIToken builds a token, enforcing every `api_tokens_*` invariant plus one
// the DDL cannot express: the prefix must announce the same kind as the row.
func NewAPIToken(p NewAPITokenParams) (APIToken, error) {
	if p.ID == uuid.Nil {
		return APIToken{}, errs.Validation("invalid_token_id", "a token needs an id")
	}
	if p.OrgID == uuid.Nil {
		return APIToken{}, errs.Validation("invalid_token_org", "a token belongs to exactly one org")
	}
	kind, err := NewTokenKind(string(p.Kind))
	if err != nil {
		return APIToken{}, err
	}

	name := strings.TrimSpace(p.Name)
	if l := len(name); l < MinTokenNameBytes || l > MaxTokenNameBytes {
		return APIToken{}, errs.Validation("invalid_token_name", "name must be 1..120 characters")
	}
	if p.Hash.IsZero() {
		return APIToken{}, errs.Validation("invalid_token_hash", "a token needs a hash")
	}
	if p.Prefix.IsZero() {
		return APIToken{}, errs.Validation("invalid_token_prefix", "a token needs a prefix")
	}
	if !strings.HasPrefix(p.Prefix.String(), kind.SecretPrefix()) {
		return APIToken{}, errs.Validation("invalid_token_prefix",
			"prefix must announce the token's kind")
	}

	// api_tokens_pat_user and api_tokens_ingest_scope, restated. Each is
	// bidirectional here where the DDL states only one direction: an ingest token
	// carrying a user_id, or a PAT carrying a source_id, is a credential whose
	// scope two different readers would resolve differently.
	switch kind {
	case TokenKindPAT:
		if p.UserID == uuid.Nil {
			return APIToken{}, errs.Validation("invalid_token_user", "a pat belongs to a user")
		}
		if p.SourceID != uuid.Nil {
			return APIToken{}, errs.Validation("invalid_token_scope", "a pat is not scoped to a source")
		}
	case TokenKindIngest:
		if p.SourceID == uuid.Nil {
			return APIToken{}, errs.Validation("invalid_token_scope",
				"an ingest token is scoped to exactly one source")
		}
		if p.UserID != uuid.Nil {
			return APIToken{}, errs.Validation("invalid_token_user",
				"an ingest token belongs to a source, not a user")
		}
	}

	if p.ExpiresAt != nil && !p.ExpiresAt.After(p.CreatedAt) {
		// api_tokens_expiry_ck. A token that expires at or before its own creation
		// is dead on arrival, and silently accepting one produces a credential that
		// "does not work" with no diagnosable reason.
		return APIToken{}, errs.Validation("invalid_token_expiry",
			"expires_at must be in the future")
	}

	return APIToken{
		ID:        p.ID,
		OrgID:     p.OrgID,
		UserID:    p.UserID,
		Kind:      kind,
		Name:      name,
		Hash:      p.Hash,
		Prefix:    p.Prefix,
		SourceID:  p.SourceID,
		ExpiresAt: p.ExpiresAt,
		CreatedAt: p.CreatedAt.UTC(),
	}, nil
}

// Revoked reports whether the token has been revoked.
func (t APIToken) Revoked() bool { return t.RevokedAt != nil }

// Expired reports whether the token's expiry has passed. A token with no expiry
// never expires.
func (t APIToken) Expired(now time.Time) bool {
	return t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)
}

// Usable is the single predicate every authentication path asks.
//
// It FAILS CLOSED: an unset expiry means "does not expire", but a revoked or
// past-expiry token is unusable, and there is no third answer. Spreading these
// two conditions across call sites is how one of them gets forgotten in the one
// place that matters.
func (t APIToken) Usable(now time.Time) bool {
	return !t.Revoked() && !t.Expired(now)
}
