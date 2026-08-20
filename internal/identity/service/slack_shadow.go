package service

import (
	"context"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// SlackPresser is who pressed a button in Slack, in oto's own terms: the sighting
// row and the oto user it resolves to.
//
// The two are returned together because the caller needs both and neither implies
// the other. `Identity.ActorLabel()` is the timeline label — a Slack handle, which
// is what an operator recognises — while `User.ID` is the PRINCIPAL, which is what
// an idempotency claim needs. A caller holding only the user would have to invent a
// label from a display name; a caller holding only the identity is back where this
// whole ticket started.
type SlackPresser struct {
	Identity domain.SlackIdentity
	// User is the oto user this Slack member resolves to. It is the ZERO User when
	// the identity is linked to a user this org can no longer read — a stale link,
	// which is not the press's fault. `User.ID == uuid.Nil` is the test.
	User domain.User
}

// ⚠️ THERE IS NO `Shadowed bool` ON THIS STRUCT, AND THERE WAS ONE. It reported
// "this call MINTED the user" — which no caller wants, because the question a caller
// actually asks is about the ROW and not about the call: `User.IsShadow()` answers
// "does this person have an address", and it answers it identically on the press that
// created the row and on the thousandth press that found it. A field distinguishing
// the two would have exactly one plausible reader, a log line, and `lintreach` is
// right to refuse a field whose value goes nowhere.

// ResolveSlackPresser records a sighting of a Slack workspace member and returns
// the oto user their press is attributable to, MINTING ONE IF THERE IS NONE.
//
// ⭐⭐ THIS IS THE FIX FOR git-bug a74d6b2, AND THE DEFECT IT FIXES IS NOT ABOUT
// SLACK. `idempotency_claims`' primary key is
// (org_id, principal_id, operation, idempotency_key), `principal_id` is NOT NULL,
// and `idempotency.Claim.validate` refuses `uuid.Nil` as a wiring bug. So an act
// can only be made to converge under retry if oto can name WHO performed it. A
// Slack member who had never linked an account had no name: `channels/service.actor`
// recorded them as `actor_kind = 'slack'` with a member id like `U024BE7LH`, which
// is a string and not a uuid, so `app.slackIdempotency` returned an UNKEYED intent
// and `alerts/service.Snooze` skipped its claim entirely (`if idem.Keyed`). Slack's
// redelivery of ONE human press then executed the snooze a SECOND time: the
// incumbent closed as `superseded`, a second `alert_snoozes` row inserted, and the
// Case timeline carried two `alert.unsnoozed(superseded)` + `alert.snoozed` pairs
// for one gesture. The duplicate CARD was already prevented — commit 8b56e45 keys
// the §C.7 occasion on the interaction — but only a claim can undo the ACT, and the
// timeline is the audit record oto sells.
//
// ⛔ THE ROW IS A SHADOW MEMBER AND CARRIES NO EMAIL, which is the owner's ruling of
// 2026-08-20 and the reason migration 00074 exists. The rejected alternative was a
// synthetic address — `U024BE7LH@slack.invalid` — and it was rejected because an
// invented mailbox is indistinguishable from a real one at every reader, while a
// NULL email answers "has this person given oto an address" exactly once, for all of
// them. It carries no password hash either, so it cannot log in; see
// `domain.NewShadowUser` and `repository.insertShadowUserSQL` for the three
// independent refusals.
//
// ⛔ AN UNLINKED MEMBER IS STILL A FIRST-CLASS STATE AND THIS DOES NOT CHANGE THAT.
// What changed is where the state lives. `slack_identities.user_id` no longer stays
// NULL for somebody who has acted — the row is linked to the shadow, because
// `slack_identities_link_ck` makes `user_id`/`linked_at` all-or-nothing and a
// half-written pair is a 23514 — while the FACT that this person has never onboarded
// is now recorded where it belongs, as the absence of an address on their user row.
// Nothing here requires a link before an ack is accepted, which is the property
// `domain.SlackIdentity`'s own comment defends: the press succeeds either way, and
// on the degraded paths below it succeeds with no user at all.
//
// ⭐ THE WHOLE THING IS ONE TRANSACTION, AND THE LOCK IS THE UPSERT'S. Two presses
// by the same member arriving at once must not mint two shadows. `Upsert`'s
// `INSERT … ON CONFLICT ON CONSTRAINT slack_identities_uniq DO UPDATE` takes a
// ROW-LEVEL LOCK on the conflicting row and holds it to the end of the transaction,
// so the second caller blocks there, and its `DO UPDATE … RETURNING` then re-reads
// the row the first caller committed — `user_id` already set — and takes the
// `Linked()` branch instead of minting anything. Serialising on the row oto is
// about to link is exactly the right granularity: two DIFFERENT members press
// concurrently without contending.
//
// ⚠️ WITHOUT A TxRunner IT STILL WORKS AND THE RACE IS REAL. `Deps.Tx` is nil only
// in a deployment that did not wire it; production does. Degrading to three
// statements loses the lock, so two simultaneous first-presses by one member can
// each insert a shadow and the second `Link` wins — leaving one orphaned row, which
// costs a duplicate on the members list and nothing else. That is strictly better
// than refusing the press, which is the one outcome this path may never produce.
func (s *Service) ResolveSlackPresser(
	ctx context.Context, scope db.TenantScope, rawTeam, rawMember, handle string,
) (SlackPresser, error) {
	team, err := domain.NewSlackTeamID(rawTeam)
	if err != nil {
		return SlackPresser{}, err
	}
	member, err := domain.NewSlackUserID(rawMember)
	if err != nil {
		return SlackPresser{}, err
	}

	var out SlackPresser
	work := func(ctx context.Context) error {
		var werr error
		out, werr = s.resolveSlackPresser(ctx, scope, team, member, handle)
		return werr
	}
	if s.tx == nil {
		if err := work(ctx); err != nil {
			return SlackPresser{}, err
		}
		return out, nil
	}
	if err := s.tx.InTx(ctx, work); err != nil {
		return SlackPresser{}, err
	}
	return out, nil
}

// resolveSlackPresser is the body, so that the transactional and degraded callers
// above run byte-identical logic rather than two copies of it.
func (s *Service) resolveSlackPresser(
	ctx context.Context, scope db.TenantScope,
	team domain.SlackTeamID, member domain.SlackUserID, handle string,
) (SlackPresser, error) {
	now := s.clk.Now()

	si, err := domain.NewSlackIdentity(id.New(), scope.OrgID(), team, member, handle)
	if err != nil {
		return SlackPresser{}, err
	}
	// The sighting. A repeat press by the same member is not a conflict — it is the
	// same person pressing a button again — so this refreshes the denormalised
	// handle and returns the row that already existed. It is also what takes the
	// lock everything below relies on.
	si, err = s.slack.Upsert(ctx, scope, si, now)
	if err != nil {
		return SlackPresser{}, err
	}

	if si.Linked() {
		user, err := s.users.Get(ctx, scope, si.UserID)
		if err != nil {
			if errs.IsKind(err, errs.KindNotFound) {
				// ⚠️ A LINK THIS ORG CAN NO LONGER READ — the user row is gone; note
				// that `selectUserSQL` does NOT filter `disabled_at`, so a merely
				// disabled member still resolves here and still gets their claim —
				// IS A STALE LINK AND NOT A FAILED PRESS. Minting a fresh
				// shadow here would be worse than reporting nothing: it would give one
				// human two oto users, and it would do so on the path where the FIRST
				// one still holds their acknowledgement history. The caller falls back
				// to the Slack handle, which loses the CLAIM for this press and keeps
				// the ACK, and that trade is `channels/service.actor`'s to make.
				return SlackPresser{Identity: si}, nil
			}
			return SlackPresser{}, err
		}
		return SlackPresser{Identity: si, User: user}, nil
	}

	// Never seen linked. Mint the shadow, then bind it — in that order, because
	// `slack_identities.user_id` is a real FK to `users(id)` and the reverse order
	// is a 23503.
	//
	// The label is `ActorLabel()` rather than the raw handle so that a member whose
	// handle Slack did not send still gets a `display_name`: `users_name_ck` is
	// `length(btrim(display_name)) BETWEEN 1 AND 120` and has no default to fall
	// back to, and the member id is the honest answer when the handle is unknown.
	shadow, err := domain.NewShadowUser(id.New(), scope.OrgID(), si.ActorLabel())
	if err != nil {
		return SlackPresser{}, err
	}
	if err := s.users.InsertShadow(ctx, scope, shadow, now); err != nil {
		return SlackPresser{}, err
	}
	si, err = s.slack.Link(ctx, scope, si.ID, shadow.ID, now)
	if err != nil {
		return SlackPresser{}, err
	}
	s.log.InfoContext(ctx, "identity: minted a shadow member for a Slack presser",
		"org_id", scope.OrgID(), "user_id", shadow.ID,
		"slack_team_id", team.String(), "slack_user_id", member.String())
	return SlackPresser{Identity: si, User: shadow}, nil
}
