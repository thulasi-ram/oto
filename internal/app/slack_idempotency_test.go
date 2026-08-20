package app

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/db"
)

// ⭐⭐ TestAnUnlinkedPresserIsNamedEvenThoughItCannotBeClaimed is the seam the
// duplicate card came through.
//
// `slackIdempotency` answers TWO questions about one press and they have different
// answers. Can the press be CLAIMED? Only if `actorID` is a uuid:
// `idempotency_claims.principal_id` is NOT NULL, `Claim.validate` refuses
// `uuid.Nil`, and a raw Slack handle like `U024BE7LH` does not parse. Can the press
// be NAMED? Yes, whenever Slack sent a `response_url`, because a name needs no
// principal at all.
//
// ⚠️ WHICH PRESSES REACH HERE WITHOUT A UUID CHANGED WITH git-bug a74d6b2 AND THIS
// FUNCTION DID NOT. An unlinked Slack member used to be the ordinary case — nothing
// could invent a principal for them, and migration 00041 left open where a `slack`
// principal's uuid comes from. It now comes from a SHADOW MEMBER, minted by
// `slackActors.SlackActor` on first press (migration 00074), so an unlinked presser
// arrives WITH a uuid and is claimed. A handle still arrives on the DEGRADED path,
// when the identity lookup failed or the link is stale, and that is what the
// `U024BE7LH` cases below now describe. The function's rule — "the principal is a
// uuid or nothing" — is unchanged, which is why the fix needed no edit here.
//
// ⛔ IT USED TO RETURN THE UNKEYED ZERO FOR BOTH, throwing the name away with the
// claim, and that is what let a redelivered press mint a second §C.7 occasion and
// post a second amendment into the channel for one human gesture. Every assertion
// below is about keeping the two answers apart.
func TestAnUnlinkedPresserIsNamedEvenThoughItCannotBeClaimed(t *testing.T) {
	scope, err := db.NewTenantScope(uuid.New())
	require.NoError(t, err)
	alertID := uuid.New()

	// What `channels/service.interactionKey` mints: a sha256 over the interaction's
	// `response_url`, which is oto's only per-interaction identity.
	const interaction = "slack:" +
		"3b1f8e0d5c4a2b9f7e6d1c0b8a9f7e6d5c4b3a2918f7e6d5c4b3a2918f7e6d5c"
	const other = "slack:" +
		"aa1f8e0d5c4a2b9f7e6d1c0b8a9f7e6d5c4b3a2918f7e6d5c4b3a2918f7e6daa"

	unlinked := slackIdempotency(scope, alertID, "U024BE7LH", interaction)
	require.False(t, unlinked.Keyed,
		"a presser with no oto user has no principal, so no claim can be taken for them")
	require.NotEqual(t, uuid.Nil, unlinked.KeyID,
		"but the interaction still HAS an identity, and dropping it is what produced a "+
			"second card for a redelivered press")

	// Stability is the whole property: the redelivery of one interaction carries the
	// same `response_url`, so it must reduce to the same id.
	require.Equal(t, unlinked.KeyID,
		slackIdempotency(scope, alertID, "U024BE7LH", interaction).KeyID,
		"a redelivered interaction must name the same occasion, or the announcement it "+
			"already made is made again")

	// And distinctness is the other half: a genuine second press is a different
	// `response_url`, so a re-snooze from 1h to 4h still gets its own announcement.
	require.NotEqual(t, unlinked.KeyID,
		slackIdempotency(scope, alertID, "U024BE7LH", other).KeyID,
		"two interactions that collapsed to one id would silence the second press")

	// ⚠️ THE NAME IS THE INTERACTION'S, NOT THE PRESSER'S. A linked member pressing
	// the same interaction names the same occasion; what linking adds is the CLAIM,
	// which converges the act itself rather than only its announcement.
	userID := uuid.New()
	linked := slackIdempotency(scope, alertID, userID.String(), interaction)
	require.True(t, linked.Keyed)
	require.Equal(t, userID, linked.Principal.UserID)
	require.Equal(t, scope.OrgID(), linked.Principal.OrgID)
	require.Equal(t, interaction, linked.Key.String())
	require.Equal(t, unlinked.KeyID, linked.KeyID,
		"the occasion is a property of the interaction; who pressed it decides only "+
			"whether a claim is possible")

	// ⛔ AN INTERACTION WITH NO `response_url` IS THE ONE CASE WITH NEITHER ANSWER,
	// and it must stay the fully zero intent. A non-zero KeyID here would hand EVERY
	// anonymous press in the deployment one shared occasion — strictly worse than
	// none, because two genuinely different snoozes would then collide on one §C.7
	// key and the second would be swallowed as a duplicate of the first.
	anonymous := slackIdempotency(scope, alertID, "U024BE7LH", "")
	require.Equal(t, uuid.Nil, anonymous.KeyID)
	require.False(t, anonymous.Keyed)
	require.Equal(t, slackIdempotency(scope, alertID, userID.String(), ""), anonymous,
		"with nothing to name it, a linked presser's press is as unnamed as an unlinked "+
			"one's: the name comes from the interaction")
}
