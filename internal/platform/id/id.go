package id

import (
	"crypto/rand"
	"encoding/base32"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// New mints a UUIDv7: time-ordered, so it doubles as an index-friendly primary
// key and a stable tiebreaker for keyset pagination.
//
// oto never uses gen_random_uuid(): ids are minted in Go so that the application
// owns ordering and a row's id is known before the INSERT.
func New() uuid.UUID {
	v, err := uuid.NewV7()
	if err != nil {
		// NewV7 only fails if crypto/rand fails, which is unrecoverable.
		panic("id: cannot generate uuidv7: " + err.Error())
	}
	return v
}

// NewString is New rendered as the canonical hyphenated string.
func NewString() string { return New().String() }

// Parse validates and parses a UUID.
func Parse(s string) (uuid.UUID, error) { return uuid.Parse(s) }

// MustParse parses a UUID or panics. For constants and tests only.
func MustParse(s string) uuid.UUID { return uuid.MustParse(s) }

// Nil is the zero UUID.
var Nil = uuid.Nil

var (
	slugStrip  = regexp.MustCompile(`[^a-z0-9]+`)
	slugTrim   = regexp.MustCompile(`^-+|-+$`)
	tokenAlpha = base32.NewEncoding("abcdefghijkmnpqrstuvwxyz23456789").WithPadding(base32.NoPadding)
)

// Slug lowercases s and reduces it to [a-z0-9-], for human-readable keys.
func Slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugStrip.ReplaceAllString(s, "-")
	return slugTrim.ReplaceAllString(s, "")
}

// Token returns a URL-safe, unambiguous random token of n bytes of entropy.
// Used for the secret half of PATs and ingest tokens.
func Token(n int) string {
	if n <= 0 {
		n = 32
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("id: cannot read random bytes: " + err.Error())
	}
	return tokenAlpha.EncodeToString(b)
}
