package app

// A SLACK MEMBER LINKS THEMSELVES WITH A CODE oto SHOWED ONLY THEM (git-bug a556a5c), over the real
// repositories and the real transaction — the SQL is what decides a code is live, single-use and
// not over-presented, and what holds the identity's row while the link is checked and written.
//
// ⭐ THE LAST STEP IS THE ONE THE TICKET IS FOR. After the link, the Slack press path
// (`slackActors`, wired as `container.go` wires it) resolves the member to the signed-in user as a
// REAL member — `Shadow` false — which is exactly what `channels/service.applyRemedy` requires
// before it calls `Remedies.ApproveRemedy` with that user's id; `slackRemedyActions` then calls the
// same `investigator/service.ApproveRemedy` the UI's route does. That half is pinned by
// `channels/service.TestALinkedMemberIsNeverIssuedACode` and `TestALinkedHolderApprovesAndDeclinesFromSlack`.

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	identitydomain "github.com/thulasiram/oto/internal/identity/domain"
	identityrepo "github.com/thulasiram/oto/internal/identity/repository"
	identityservice "github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/harness"
)

type linkRig struct {
	h        *harness.H
	org      harness.Org
	identity *identityservice.Service
	actors   slackActors
	codes    slackLinkCodes
}

func newLinkRig(t *testing.T) *linkRig {
	t.Helper()
	h := harness.New(t)
	org := h.Org()
	identity := identityservice.New(identityservice.Deps{
		Users:      identityrepo.NewUserRepository(h.Pool),
		Slack:      identityrepo.NewSlackIdentityRepository(h.Pool),
		SlackLinks: identityrepo.NewSlackLinkRepository(h.Pool),
		Tx:         identityrepo.NewTxRunner(h.Pool),
		Clock:      h.Clock,
		Logger:     quietLogger(),
	})
	return &linkRig{h: h, org: org, identity: identity,
		actors: slackActors{identity: identity}, codes: slackLinkCodes{identity: identity}}
}

func (r *linkRig) session(u harness.User) authn.Principal {
	return authn.Principal{Kind: authn.KindSession, OrgID: r.org.ID, UserID: u.ID, Email: u.Email}
}

// press is an unlinked member pressing a Remedy button: the presser is resolved (minting the
// shadow), and the code the ephemeral would carry is issued.
func (r *linkRig) press(t *testing.T, member string) string {
	t.Helper()
	a, err := r.actors.SlackActor(r.h.Ctx, r.org.Scope, harnessTeamID, member, "ada")
	require.NoError(t, err)
	require.True(t, a.Shadow, "a first press resolves to a shadow member")
	got, err := r.codes.IssueSlackLinkCode(r.h.Ctx, r.org.Scope, harnessTeamID, member)
	require.NoError(t, err)
	return got.Code
}

func TestASlackMemberLinksThemselvesAndTheirNextPressIsTheirs(t *testing.T) {
	r := newLinkRig(t)
	me := r.h.User(r.org)
	code := r.press(t, "U024BE7LH")

	// Only the hash is stored.
	var stored []byte
	require.NoError(t, r.h.Pool.QueryRow(r.h.Ctx, `SELECT code_hash FROM slack_link_codes`).Scan(&stored))
	require.Len(t, stored, 32)
	require.NotContains(t, string(stored), code[:5])

	// The preview names the member and consumes nothing.
	p, err := r.identity.PreviewSlackLink(r.h.Ctx, r.org.Scope, r.session(me), code)
	require.NoError(t, err)
	require.Equal(t, "U024BE7LH", p.Identity.SlackUserID.String())
	require.Equal(t, harnessTeamID, p.Identity.TeamID.String())
	require.False(t, p.AlreadyYours)

	linked, err := r.identity.ConfirmSlackLink(r.h.Ctx, r.org.Scope, r.session(me), code)
	require.NoError(t, err)
	require.Equal(t, me.ID, linked.UserID)

	// The shadow was retired and the fact recorded with its actor.
	var change string
	var userID, actorID uuid.UUID
	var displaced *uuid.UUID
	require.NoError(t, r.h.Pool.QueryRow(r.h.Ctx,
		`SELECT change, user_id, actor_id, displaced_user_id FROM slack_identity_links`).
		Scan(&change, &userID, &actorID, &displaced))
	require.Equal(t, "linked", change)
	require.Equal(t, me.ID, userID)
	require.Equal(t, me.ID, actorID)
	require.NotNil(t, displaced, "the fact names the shadow the link retired")
	var shadowDisabled *time.Time
	require.NoError(t, r.h.Pool.QueryRow(r.h.Ctx,
		`SELECT disabled_at FROM users WHERE id = $1`, *displaced).Scan(&shadowDisabled))
	require.NotNil(t, shadowDisabled)

	// ⭐ The member's next press is the signed-in user, as a real member.
	a, err := r.actors.SlackActor(r.h.Ctx, r.org.Scope, harnessTeamID, "U024BE7LH", "ada")
	require.NoError(t, err)
	require.Equal(t, me.ID, a.UserID)
	require.False(t, a.Shadow)

	// Single use, in SQL.
	_, err = r.identity.ConfirmSlackLink(r.h.Ctx, r.org.Scope, r.session(me), code)
	require.Equal(t, identitydomain.SlackLinkCodeInvalidCode, errs.CodeOf(err))

	// And a linked member is issued no further code.
	_, err = r.codes.IssueSlackLinkCode(r.h.Ctx, r.org.Scope, harnessTeamID, "U024BE7LH")
	require.Equal(t, identitydomain.SlackIdentityLinkedElsewhereCode, errs.CodeOf(err))
}

func TestALinkCodeExpiresAndAReissueKillsThePreviousOne(t *testing.T) {
	r := newLinkRig(t)
	me := r.h.User(r.org)

	first := r.press(t, "U024BE7LH")
	second := r.press(t, "U024BE7LH")
	_, err := r.identity.PreviewSlackLink(r.h.Ctx, r.org.Scope, r.session(me), first)
	require.Equal(t, identitydomain.SlackLinkCodeInvalidCode, errs.CodeOf(err), "the replaced code still worked")

	r.h.Clock.Advance(10 * time.Minute)
	_, err = r.identity.ConfirmSlackLink(r.h.Ctx, r.org.Scope, r.session(me), second)
	require.Equal(t, identitydomain.SlackLinkCodeInvalidCode, errs.CodeOf(err), "an expired code linked")

	var n int
	require.NoError(t, r.h.Pool.QueryRow(r.h.Ctx,
		`SELECT count(*) FROM slack_link_attempts WHERE user_id = $1`, me.ID).Scan(&n))
	require.Equal(t, 2, n, "both refusals are counted against the user, in the database")
}

func TestWrongCodesAreLimitedPerUserInTheDatabase(t *testing.T) {
	r := newLinkRig(t)
	me := r.h.User(r.org)
	code := r.press(t, "U024BE7LH")
	for range identitydomain.SlackLinkWrongAttemptLimit {
		_, err := r.identity.PreviewSlackLink(r.h.Ctx, r.org.Scope, r.session(me), "ZZZZZ-ZZZZZ")
		require.Equal(t, identitydomain.SlackLinkCodeInvalidCode, errs.CodeOf(err))
	}
	_, err := r.identity.ConfirmSlackLink(r.h.Ctx, r.org.Scope, r.session(me), code)
	require.True(t, errs.IsKind(err, errs.KindRateLimited), "err = %v", err)

	r.h.Clock.Advance(identitydomain.SlackLinkWrongAttemptWindow + time.Second)
	code = r.press(t, "U024BE7LH")
	_, err = r.identity.ConfirmSlackLink(r.h.Ctx, r.org.Scope, r.session(me), code)
	require.NoError(t, err)
}

func TestALinkToAnotherRealUserIsNeverMovedAndOnlyTheirOwnUnlinkDropsIt(t *testing.T) {
	r := newLinkRig(t)
	ada := r.h.User(r.org)
	mallory := r.h.User(r.org)

	code := r.press(t, "U024BE7LH")
	_, err := r.identity.ConfirmSlackLink(r.h.Ctx, r.org.Scope, r.session(ada), code)
	require.NoError(t, err)

	// A code for that member cannot be had any more; one stored before the link is refused at
	// confirm under the row lock, and the refusal rolls back with nothing written.
	si, err := identityrepo.NewSlackIdentityRepository(r.h.Pool).GetByUser(r.h.Ctx, r.org.Scope, ada.ID)
	require.NoError(t, err)
	stale, err := identitydomain.NewSlackLinkCode(fixedEntropy{})
	require.NoError(t, err)
	hash, err := stale.Hash()
	require.NoError(t, err)
	require.NoError(t, identityrepo.NewSlackLinkRepository(r.h.Pool).IssueCode(r.h.Ctx, r.org.Scope, si.ID, hash,
		r.h.Clock.Now(), r.h.Clock.Now().Add(time.Minute)))

	_, err = r.identity.ConfirmSlackLink(r.h.Ctx, r.org.Scope, r.session(mallory), stale.Display())
	require.Equal(t, identitydomain.SlackIdentityLinkedElsewhereCode, errs.CodeOf(err))
	a, err := r.actors.SlackActor(r.h.Ctx, r.org.Scope, harnessTeamID, "U024BE7LH", "ada")
	require.NoError(t, err)
	require.Equal(t, ada.ID, a.UserID, "a link to another real user moved")

	// Mallory cannot unlink it either: a 404.
	err = r.identity.UnlinkSlackIdentity(r.h.Ctx, r.org.Scope, r.session(mallory), si.ID)
	require.True(t, errs.IsKind(err, errs.KindNotFound), "err = %v", err)

	// Ada can, and the fact is recorded.
	require.NoError(t, r.identity.UnlinkSlackIdentity(r.h.Ctx, r.org.Scope, r.session(ada), si.ID))
	var facts int
	require.NoError(t, r.h.Pool.QueryRow(r.h.Ctx,
		`SELECT count(*) FROM slack_identity_links WHERE change = 'unlinked' AND actor_id = $1`, ada.ID).Scan(&facts))
	require.Equal(t, 1, facts)
	mine, err := r.identity.ListMySlackIdentities(r.h.Ctx, r.org.Scope, r.session(ada))
	require.NoError(t, err)
	require.Empty(t, mine)
}

// fixedEntropy is a reader of ones, so a test can mint a known code.
type fixedEntropy struct{}

func (fixedEntropy) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 1
	}
	return len(p), nil
}
