package domain

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ⭐⭐ TestAShadowMemberCanNeverPasswordLogin pins the domain half of the three
// independent refusals that keep a NULL-email row out of the login path (git-bug
// a74d6b2, migration 00074).
//
// The other two are in SQL and are tested against a real database: `password_hash`
// is NULL on the row, and `resolveByEmailSQL` compares `u.email = $1`, which is never
// TRUE for a NULL. This one is the conjunct that survives BOTH of those being
// defeated — by an invite flow, an SSO shim or a repair script that writes a hash
// onto an existing row — and it is asserted with a hash forced on, because that is
// the only state in which it does any work at all.
func TestAShadowMemberCanNeverPasswordLogin(t *testing.T) {
	shadow, err := NewShadowUser(uuid.New(), uuid.New(), "@ada")
	require.NoError(t, err)

	require.True(t, shadow.IsShadow(),
		"a row with no address IS the shadow member; IsShadow is the question every "+
			"email reader is meant to ask instead of comparing against \"\"")
	require.True(t, shadow.Email.IsZero())
	require.True(t, shadow.PasswordHash.IsZero())
	require.True(t, shadow.Active(), "a shadow member is NOT disabled: it is the answer to "+
		"\"who acked this\" for every Slack-only presser, and users_org_idx serves that "+
		"lookup only for live rows")
	require.False(t, shadow.CanPasswordLogin())

	// ⛔ THE STATE THAT MATTERS: a hash arrives on a row that still has no address.
	// Every other refusal is defeated here — the hash is present and parseable, and a
	// caller holding this value has skipped the query entirely — and the answer must
	// still be no. A credential is proof of an identity somebody claimed; a shadow row
	// is oto's own record of a button press, and nobody claimed it.
	hash, err := NewPasswordHash("$argon2id$v=19$m=19456,t=2,p=1$c29tZXNhbHQ$aGFzaA")
	require.NoError(t, err)
	forced := shadow
	forced.PasswordHash = hash
	require.False(t, forced.CanPasswordLogin(),
		"a shadow member with a password hash forced onto it must still be refused — this "+
			"is the conjunct that outlives the two SQL guards")

	// And the control: the same row with an address is an ordinary member again.
	email, err := NewEmail("ada@example.com")
	require.NoError(t, err)
	adopted := forced
	adopted.Email = email
	require.False(t, adopted.IsShadow())
	require.True(t, adopted.CanPasswordLogin(),
		"the refusal must be about the ABSENT ADDRESS and nothing else, or it is a bug "+
			"that locks real members out")
}

// ⚠️ TestNewShadowUserEnforcesTheDisplayNameBound is about the one column a shadow
// member cannot leave empty.
//
// `users_name_ck` is `length(btrim(display_name)) BETWEEN 1 AND 120` and has no
// default to fall back on, so an empty or over-long label is a 23514 at the INSERT —
// on the Slack button-press path, where the cost of a refusal is a lost
// acknowledgement. The bound is therefore enforced in the constructor, and the
// over-long case is TRUNCATED rather than refused, because the label comes from a
// foreign system that owes oto no bound.
func TestNewShadowUserEnforcesTheDisplayNameBound(t *testing.T) {
	org := uuid.New()

	_, err := NewShadowUser(uuid.New(), org, "   ")
	require.Error(t, err, "a blank label is refused here rather than as a 23514 mid-press")

	_, err = NewShadowUser(uuid.Nil, org, "@ada")
	require.Error(t, err)
	_, err = NewShadowUser(uuid.New(), uuid.Nil, "@ada")
	require.Error(t, err)

	// A handle at the ceiling, with the "@" that `ActorLabel()` prepends, is 121 bytes
	// — one over — which is exactly the case a naive `s[:120]` was written for and the
	// reason the constructor truncates instead of failing.
	long := "@" + strings.Repeat("a", MaxSlackHandleBytes)
	require.Len(t, long, MaxDisplayNameBytes+1)
	u, err := NewShadowUser(uuid.New(), org, long)
	require.NoError(t, err)
	require.Len(t, u.DisplayName, MaxDisplayNameBytes)

	// ⛔ AND THE TRUNCATION IS ON A RUNE BOUNDARY. `MaxDisplayNameBytes` counts BYTES
	// while the DDL's `length()` counts CHARACTERS, so a byte ceiling is the stricter
	// of the two and can never admit a row the CHECK refuses. What it CAN do, cut
	// naively, is split a multi-byte rune and hand Postgres a byte sequence that is not
	// valid UTF-8 — a 22021 that names no column, on the press path, for a Slack handle
	// with an emoji in it.
	emoji := strings.Repeat("😀", 40) // 4 bytes each: 160 bytes, 40 runes.
	u, err = NewShadowUser(uuid.New(), org, emoji)
	require.NoError(t, err)
	require.True(t, len(u.DisplayName) <= MaxDisplayNameBytes)
	require.Equal(t, u.DisplayName, strings.ToValidUTF8(u.DisplayName, ""),
		"a truncated display name that is not valid UTF-8 is a 22021 from Postgres, which "+
			"is why TruncateDisplayName walks back to a rune start")
}
