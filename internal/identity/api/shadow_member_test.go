package api

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/identity/domain"
)

// ⭐⭐ TestAShadowMemberRoundTripsAsAnExplicitNull is the wire half of git-bug
// a74d6b2.
//
// A SHADOW MEMBER is a `users` row oto minted for a Slack workspace member who
// pressed a button without ever linking an account, so that the press had a
// principal uuid to take an idempotency claim under. It carries no email, because
// the owner ruled that oto records the ABSENCE of an address rather than inventing a
// synthetic one — and an absence has to survive the mapper, the encoder and the
// contract, or every client learns about it as a crash.
//
// ⛔ THE ASSERTION IS `null` AND NOT `""`, AND THE KEY MUST BE PRESENT. Three wrong
// answers are each individually plausible:
//
//   - `"email": ""` — what a `string` field renders. Indistinguishable from a real
//     value that happens to be empty, and it would pass the `format: email`
//     validator in no client and fail loudly in some.
//   - the key MISSING — what `omitempty` renders. `UserDTO.email` is REQUIRED in
//     `api/openapi/openapi.yaml` and nullable; a client typed from the contract
//     expects the key, and `omitempty` would give the same absence two spellings
//     depending on which encoder ran.
//   - `"email": "U024BE7LH@slack.invalid"` — the synthetic address the ruling
//     rejected, because an invented mailbox is indistinguishable from a real one on
//     every screen that renders this field.
//
// So the JSON is read as a raw map, not decoded back into the DTO: decoding would
// turn all three of the wrong answers into something a `*string` comparison could
// still accept.
func TestAShadowMemberRoundTripsAsAnExplicitNull(t *testing.T) {
	shadow, err := domain.NewShadowUser(uuid.New(), uuid.New(), "@ada")
	require.NoError(t, err)
	require.True(t, shadow.IsShadow())

	raw, err := json.Marshal(toUserDTO(shadow, "U024BE7LH"))
	require.NoError(t, err)

	var wire map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &wire))

	got, present := wire["email"]
	require.True(t, present,
		"`email` is REQUIRED and nullable in the contract, so the key must be there: %s", raw)
	require.JSONEq(t, "null", string(got),
		"a shadow member's address must render as an explicit null, not as \"\" and not as an "+
			"invented mailbox: %s", raw)

	// The display name is the Slack handle, which is what an operator recognises and
	// what the timeline has always shown for this person.
	require.JSONEq(t, `"@ada"`, string(wire["display_name"]))

	// ⭐ AND THE ROUND TRIP BACK, which is what a client library does. A `*string`
	// distinguishes "absent" from "present and empty"; the pointer being nil is the
	// only reading of `null` that a consumer can act on.
	//
	// ⚠️ IT DECODES INTO A LOCAL STRUCT AND NOT INTO `UserDTO`, DELIBERATELY.
	// `tools/lintreach` classifies any type a test unmarshals into as a DECODE TARGET
	// and then requires its fields to be READ somewhere in production — and `UserDTO`
	// is a response shape that oto only ever writes, so decoding into it here would
	// report all four of its fields as unreachable. This local mirror is what a client
	// is, which is also the honest thing to test against: it proves the JSON is
	// consumable without claiming oto consumes it.
	var back struct {
		ID          uuid.UUID `json:"id"`
		Email       *string   `json:"email"`
		DisplayName string    `json:"display_name"`
		SlackUserID *string   `json:"slack_user_id"`
	}
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Nil(t, back.Email)
	require.Equal(t, shadow.ID, back.ID)
	require.Equal(t, "@ada", back.DisplayName)
	require.NotNil(t, back.SlackUserID)
	require.Equal(t, "U024BE7LH", *back.SlackUserID)
}

// ⚠️ TestAMemberWithAnAddressStillRendersIt is the other half, and it exists because
// the change that breaks it is the same one-line edit. A mapper that rendered every
// email as null — `IsShadow` inverted, or the pointer never assigned — would pass
// the test above completely.
func TestAMemberWithAnAddressStillRendersIt(t *testing.T) {
	email, err := domain.NewEmail("Priya@Example.com")
	require.NoError(t, err)
	u, err := domain.NewUser(uuid.New(), uuid.New(), email, "Priya R.", domain.NoPassword())
	require.NoError(t, err)
	require.False(t, u.IsShadow())

	dto := toUserDTO(u, "")
	require.NotNil(t, dto.Email)
	// Lower-cased by the domain constructor, because the column is CITEXT and two
	// spellings of one address must not read as two accounts.
	require.Equal(t, "priya@example.com", *dto.Email)
	require.Nil(t, dto.SlackUserID)
}
