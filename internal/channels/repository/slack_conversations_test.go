package repository_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/repository"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/harness"
)

// `ResolveSlackConversation` is THE TENANT RESOLVER FOR EVERY SLACK INTERACTION,
// and until this file existed it had no repository test at all.
//
// `InteractionService.Apply` calls it as step 1 — "---- 1. THE TENANT ----" — and
// hands its `OrgID` straight to `db.NewTenantScope`. Everything the Acknowledge
// button does afterwards is bound by the scope this one query produced, so a row
// this query should not have returned is a scope oto had no right to mint.
//
// ⛔ IT IS ONE OF THE FIVE UNSCOPED ORG-PRODUCING RESOLVERS, and it is the only
// one a human presses directly. The other four live in `identity/repository` and
// are pinned by `auth_resolvers_test.go`; the invariant they share is the `orgs`
// join, and this file is where it is pinned for `channels`.

// seedSlackChannel configures one Slack destination for an org, under a
// connection created for the same (team, name) pair.
//
// A `slack` connection MUST carry a credential (`channel_connections_cred_ck`),
// so the credential repository seals a throwaway one through `fakeSealer` —
// the same one `credentials_clock_test.go` uses. The connection's config
// carries `team_id`; the channel's own config carries only `conversation_id` —
// `resolveSlackConversationSQL` now joins the two, matching `cx.config` for the
// workspace and `c.config` for the conversation.
func seedSlackChannel(t *testing.T, h *harness.H, org harness.Org, name, team, conversation string) uuid.UUID {
	t.Helper()

	creds := repository.NewCredentialRepository(h.Pool, fakeSealer{}, nil, h.Clock)
	cred, err := creds.Create(h.Ctx, org.Scope, "slack_bot_token", map[string]string{"token": "xoxb-" + name})
	require.NoError(t, err)

	conn, err := repository.NewConnectionRepository(h.Pool, h.Clock).Create(h.Ctx, org.Scope, domain.NewConnection{
		Type:         domain.TypeSlack,
		Name:         name + "-connection",
		Config:       json.RawMessage(fmt.Sprintf(`{"team_id":%q}`, team)),
		CredentialID: &cred.ID,
	})
	require.NoError(t, err)

	inst, err := repository.NewChannelRepository(h.Pool, h.Clock).Create(h.Ctx, org.Scope, domain.NewInstance{
		Type:         domain.TypeSlack,
		Name:         name,
		Config:       json.RawMessage(fmt.Sprintf(`{"conversation_id":%q}`, conversation)),
		ConnectionID: conn.ID,
		Renderer:     "slack.default",
		Verbosity:    domain.VerbosityAll,
		Enabled:      true,
	})
	require.NoError(t, err)
	return inst.ID
}

// ⭐ TestSlackConversationResolutionIsOrgBlindAndRefusesAnAmbiguousConversation
// pins the shape of the resolver: what it is for, and what it refuses.
//
// The pair (team_id, conversation_id) is what oto's own operator configured, so
// an interaction from workspace T1 can only ever land in an org that configured a
// channel in T1. `LIMIT 2` is what stops the planner's physical ordering from
// deciding WHICH org, in the one case where two of them each claimed the same
// conversation.
func TestSlackConversationResolutionIsOrgBlindAndRefusesAnAmbiguousConversation(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewChannelRepository(h.Pool, h.Clock)

	const team, conversation = "T9TK3CUKW", "C7F2X9QLM"

	orgA := h.Org()
	channelA := seedSlackChannel(t, h, orgA, "sre-alerts", team, conversation)

	dest, err := repo.ResolveSlackConversation(h.Ctx, team, conversation)
	require.NoError(t, err, "a single configured conversation must resolve")
	require.Equal(t, orgA.ID, dest.OrgID, "the resolved row is what supplies the tenancy")
	require.Equal(t, channelA, dest.ChannelID)

	// Two oto channels pointing at ONE Slack conversation, inside one org, is
	// normal and is not an ambiguity: the tenancy is the same either way.
	seedSlackChannel(t, h, orgA, "sre-alerts-mirror", team, conversation)
	dest, err = repo.ResolveSlackConversation(h.Ctx, team, conversation)
	require.NoError(t, err, "two channels in the SAME org agree about the tenant")
	require.Equal(t, orgA.ID, dest.OrgID)

	// An unconfigured conversation, and an unconfigured workspace, are both
	// not-found rather than an error the worker would retry.
	_, err = repo.ResolveSlackConversation(h.Ctx, team, "C000UNKNOWN")
	require.ErrorIs(t, err, errs.ErrNotFound)
	_, err = repo.ResolveSlackConversation(h.Ctx, "T000UNKNOWN", conversation)
	require.ErrorIs(t, err, errs.ErrNotFound,
		"the workspace is half the key; matching on the conversation alone would cross tenants")

	// A disabled destination must not still accept commands from Slack, and a
	// deleted one must not either. Both predicates are in the SQL.
	h.Exec(`UPDATE channels SET enabled = false WHERE org_id = $1`, orgA.ID)
	_, err = repo.ResolveSlackConversation(h.Ctx, team, conversation)
	require.ErrorIs(t, err, errs.ErrNotFound, "an operator who turned a destination off meant it")

	h.Exec(`UPDATE channels SET enabled = true, deleted_at = $1 WHERE org_id = $2`, h.Now(), orgA.ID)
	_, err = repo.ResolveSlackConversation(h.Ctx, team, conversation)
	require.ErrorIs(t, err, errs.ErrNotFound)
}

// ⛔ TestSlackConversationResolutionExcludesASoftDeletedTenant.
//
// SOFT-DELETING AN ORG DOES NOT TOUCH `channels`. The FK is `ON DELETE CASCADE`,
// which fires for a hard DELETE and never for `deleted_at = now()`, so every row
// this resolver reads outlives the tenant that owns it. Nothing else in the Slack
// interaction path asks the question either: `Apply` takes the `OrgID` this query
// returns and mints a `db.TenantScope` from it, and a TenantScope is proof of
// authentication by construction — it cannot re-check what produced it.
//
// So the join is the only place the question can be asked, exactly as it is in
// `resolveSessionSQL`, `resolveByPrefixSQL` and `resolveByEmailSQL`. Without it a
// dead tenant's Acknowledge button still worked: the press resolved, the scope
// was minted, and the acknowledgement was written into an org that no longer
// exists.
//
// The second, worse consequence has its own test below.
func TestSlackConversationResolutionExcludesASoftDeletedTenant(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewChannelRepository(h.Pool, h.Clock)

	const team, conversation = "T9TK3CUKW", "C7F2X9QLM"

	dead := h.Org()
	seedSlackChannel(t, h, dead, "sre-alerts", team, conversation)

	// While the tenant is alive the conversation resolves. Deleting the tenant is
	// the only thing that changes between the two halves of this test — in
	// particular the `channels` row is untouched, which is the whole point.
	dest, err := repo.ResolveSlackConversation(h.Ctx, team, conversation)
	require.NoError(t, err)
	require.Equal(t, dead.ID, dest.OrgID)

	h.Exec(`UPDATE orgs SET deleted_at = $1 WHERE id = $2`, h.Now(), dead.ID)

	var live bool
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT count(*) = 1 FROM channels WHERE org_id = $1 AND deleted_at IS NULL AND enabled`,
		dead.ID).Scan(&live))
	require.True(t, live, "soft-deleting an org must NOT have cascaded to channels; "+
		"if it ever does, this test stops proving anything and the join is still the enforcement")

	_, err = repo.ResolveSlackConversation(h.Ctx, team, conversation)
	require.ErrorIs(t, err, errs.ErrNotFound,
		"a soft-deleted tenant's conversation must not resolve; the OrgID it returns "+
			"becomes a db.TenantScope, and a scope for a dead tenant is a scope oto had no right to mint")
}

// ⛔⭐ TestASoftDeletedTenantDoesNotShadowALiveOneSharingAConversation is the
// USER-VISIBLE half, and it is the same lockout `ResolveByEmail` had.
//
// One Slack workspace connected to two oto tenants is representable and is the
// reason for `LIMIT 2`. Counting a DEAD tenant towards that ceiling turns one
// customer's deletion into a different customer's outage: `len(found) == 2` with
// differing org ids is `slack_conversation_ambiguous`, the worker sends "oto has
// no channel configured for this conversation", and it says that FOREVER — to a
// live, paying tenant, about a channel they can see configured in their own
// settings, because of a tenant they have never heard of.
//
// There is no diagnostic anybody in that Slack channel could act on, and no
// self-service fix: the shadowing row belongs to an org that has been deleted.
//
// The last leg re-asserts that a SECOND LIVE tenant is still refused, so the join
// cannot be mistaken for a licence to relax `LIMIT 2`.
func TestASoftDeletedTenantDoesNotShadowALiveOneSharingAConversation(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewChannelRepository(h.Pool, h.Clock)

	const team, conversation = "T9TK3CUKW", "C7F2X9QLM"

	// The dead tenant configured its channel FIRST, so its uuidv7 sorts first
	// under `ORDER BY c.id` and it is the row the planner would have handed back.
	dead := h.Org()
	seedSlackChannel(t, h, dead, "sre-alerts", team, conversation)
	h.Exec(`UPDATE orgs SET deleted_at = $1 WHERE id = $2`, h.Now(), dead.ID)

	live := h.Org()
	liveChannel := seedSlackChannel(t, h, live, "sre-alerts", team, conversation)

	dest, err := repo.ResolveSlackConversation(h.Ctx, team, conversation)
	require.NoError(t, err,
		"a deleted tenant's channel made a live tenant's conversation ambiguous, and every "+
			"Acknowledge press in it answered 'no oto channel is configured'")
	require.Equal(t, live.ID, dest.OrgID, "the LIVE tenant is the one the press must land in")
	require.Equal(t, liveChannel, dest.ChannelID)

	// A second dead tenant changes nothing: dead rows are not candidates, so they
	// cannot combine into an ambiguity either.
	alsoDead := h.Org()
	seedSlackChannel(t, h, alsoDead, "sre-alerts", team, conversation)
	h.Exec(`UPDATE orgs SET deleted_at = $1 WHERE id = $2`, h.Now(), alsoDead.ID)

	dest, err = repo.ResolveSlackConversation(h.Ctx, team, conversation)
	require.NoError(t, err, "two dead tenants are still nought candidates, not two")
	require.Equal(t, live.ID, dest.OrgID)

	// ⚠️ And the guard is intact: a SECOND LIVE tenant claiming the same
	// conversation is genuinely ambiguous, and is still refused.
	secondLive := h.Org()
	seedSlackChannel(t, h, secondLive, "sre-alerts", team, conversation)

	_, err = repo.ResolveSlackConversation(h.Ctx, team, conversation)
	require.ErrorIs(t, err, errs.ErrNotFound,
		"excluding dead tenants must not have relaxed LIMIT 2 for live ones: attributing a "+
			"human's acknowledgement to whichever org the planner listed first is worse than refusing")
}
