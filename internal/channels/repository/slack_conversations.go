package repository

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// SlackDestination is one configured Slack conversation and the tenant it
// belongs to. It is what an inbound interaction is resolved into BEFORE anything
// else happens, because until the org is known there is no db.TenantScope and
// therefore no legal query about an alert.
type SlackDestination struct {
	OrgID     uuid.UUID
	ChannelID uuid.UUID
}

// resolveSlackConversationSQL is ORG-BLIND, and it is the ONLY statement in this
// module that is.
//
// ⚠️⚠️ READ THIS BEFORE COPYING IT. `channels.go` opens by promising that every
// statement over `channels` carries an `org_id` predicate, because a missing one
// is a data leak. That promise still holds for every query a REQUEST can reach.
// This one cannot be reached by a request: a Slack interaction payload names a
// workspace and a conversation and never an org, so resolving the tenant IS the
// question, and a scope cannot be an input to the query that produces it.
//
// Two things make that safe rather than merely necessary:
//
//   - The caller has already authenticated by another means — Slack's v0 HMAC
//     over the raw body, inside the five-minute replay window (§H.8). This
//     function authenticates nothing and must never be called before that.
//   - The pair (team_id, conversation_id) is what oto's own operator configured.
//     An interaction from workspace T1 can only ever resolve to an org that
//     configured a channel in T1, so a press in one workspace cannot address
//     another tenant's alerts.
//
// LIMIT 2 for the same reason `resolveSlackIdentitySQL` uses it: two orgs may
// legitimately each configure a channel pointing at the same conversation — one
// Slack workspace connected to two oto tenants is representable — and letting
// the planner's first row decide a tenancy would decide it by physical ordering.
// Two rows in ONE org is fine and common (two oto channels, one Slack channel);
// two rows across two orgs is genuinely ambiguous and is refused.
//
// Deleted and disabled channels are excluded: a destination an operator has
// turned off must not still accept commands from Slack.
//
// ⛔ THE `orgs` JOIN IS INNER AND IT ASKS ABOUT THE TENANT, NOT THE CHANNEL.
// `c.deleted_at IS NULL AND c.enabled` are questions about the DESTINATION, and
// answering them is not the same as asking whether the tenant is still alive:
// SOFT-DELETING AN ORG DOES NOT TOUCH `channels`. The FK is `ON DELETE CASCADE`,
// which fires for a hard DELETE and never for `deleted_at = now()`, so every row
// this statement can see outlives the tenant that owns it. Nor can anything
// downstream re-check: `InteractionService.Apply` hands the `org_id` below
// straight to `db.NewTenantScope`, and a TenantScope is proof of authentication
// by construction — it cannot interrogate what produced it. This join is the
// only place the question can be asked, which is why it is the same join
// `resolveSessionSQL`, `resolveByPrefixSQL` and `resolveByEmailSQL` carry. See
// the roll-call in `identity/repository/users.go`.
//
// Without it, both halves of the defect 7f8e710 fixed for the login path were
// live here, in the resolver a human actually presses:
//
//  1. a dead tenant's Acknowledge button still worked — the press resolved, a
//     scope was minted for an org that no longer exists, and the acknowledgement
//     was written into it;
//  2. worse, the dead row still counted towards the `LIMIT 2` below, so a LIVE
//     tenant that had configured the same conversation in the same workspace was
//     refused as `slack_conversation_ambiguous` and told "no oto channel is
//     configured for this conversation" on every press, forever, with nothing in
//     the message that names the deleted tenant shadowing them.
//
// ⚠️ IT IS ALSO WHY `LIMIT 2` CAN STILL BE TRUSTED. The ambiguity this query
// refuses is "more than one LIVE org claims this conversation"; counting dead
// orgs towards that ceiling turns a deletion into somebody else's lockout.
// ⛔ `team_id` NO LONGER LIVES ON `channels.config`. It moved to
// `channel_connections.config` when a channel stopped carrying its own
// workspace and started referencing a connection that does (see the ADR
// introducing ChannelConnection) — so this resolver now joins through
// `connection_id` to find it. The org-blindness argument above is unchanged:
// the join adds a hop, not a scope.
const resolveSlackConversationSQL = `
SELECT c.org_id, c.id
  FROM channels c
  JOIN channel_connections cx ON cx.id = c.connection_id AND cx.org_id = c.org_id
  JOIN orgs o ON o.id = c.org_id AND o.deleted_at IS NULL
 WHERE c.type = 'slack'
   AND c.deleted_at IS NULL
   AND c.enabled
   AND cx.deleted_at IS NULL
   AND cx.config->>'team_id' = $1
   AND c.config->>'conversation_id' = $2
 ORDER BY c.id
 LIMIT 2`

// ResolveSlackConversation maps a Slack workspace and conversation onto the
// tenant that configured it.
//
// An unknown pair and an ambiguous one both answer NotFound. The caller cannot
// act on either, and distinguishing them for a Slack user would tell them
// something about another tenant's configuration.
func (r *ChannelRepository) ResolveSlackConversation(
	ctx context.Context, teamID, conversationID string,
) (SlackDestination, error) {
	if teamID == "" || conversationID == "" {
		return SlackDestination{}, errs.NotFound("slack_conversation_not_found",
			"no oto channel is configured for that Slack conversation")
	}

	rows, err := r.db(ctx).Query(ctx, resolveSlackConversationSQL, teamID, conversationID)
	if err != nil {
		return SlackDestination{}, mapErr(err, "slack_conversation_not_found", "slack conversation")
	}
	defer rows.Close()

	var found []SlackDestination
	for rows.Next() {
		var d SlackDestination
		if err := rows.Scan(&d.OrgID, &d.ChannelID); err != nil {
			return SlackDestination{}, mapErr(err, "slack_conversation_not_found", "slack conversation")
		}
		found = append(found, d)
	}
	if err := rows.Err(); err != nil {
		return SlackDestination{}, mapErr(err, "slack_conversation_not_found", "slack conversation")
	}

	switch {
	case len(found) == 0:
		return SlackDestination{}, errs.NotFound("slack_conversation_not_found",
			"no oto channel is configured for that Slack conversation")
	case len(found) == 2 && found[0].OrgID != found[1].OrgID:
		// Two tenants claim the same conversation. Picking one would attribute a
		// human's acknowledgement to whichever org the planner listed first.
		return SlackDestination{}, errs.NotFound("slack_conversation_ambiguous",
			"more than one oto organisation claims that Slack conversation")
	default:
		return found[0], nil
	}
}
