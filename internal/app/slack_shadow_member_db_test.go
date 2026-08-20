package app

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	alertsrepo "github.com/thulasiram/oto/internal/alerts/repository"
	alertsservice "github.com/thulasiram/oto/internal/alerts/service"
	identityrepo "github.com/thulasiram/oto/internal/identity/repository"
	identityservice "github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/idempotency"
	"github.com/thulasiram/oto/test/harness"
)

// ⭐⭐ THE TESTS IN THIS FILE ARE ABOUT ONE SENTENCE FROM git-bug a74d6b2: "an
// unlinked Slack presser has a claim principal, so a redelivered press writes one
// snooze row and one pair of events".
//
// ⛔ THEY RUN AGAINST A REAL MIGRATED POSTGRES BECAUSE EVERY GUARD IN THE CHANGE IS
// IN SQL. `users.email` being nullable is a DDL fact; `users_email_uniq` admitting
// many NULLs is Postgres's three-valued semantics; the claim that refuses the
// redelivery is a row in `idempotency_claims` whose primary key includes
// `principal_id NOT NULL`; and the reason the old code failed was a `uuid.Parse` of
// a string that came out of a `slack_identities` row. A fake identity store would
// have passed against the defect: it would have handed back a uuid because a Go map
// can hold one, which is exactly the thing the schema could not.

// shadowRig is the Slack button-press path, wired the way `container.go` wires it:
// the identity service that mints the shadow member, the alerts service that takes
// the claim, and the two adapters in `adapters.go` that join them.
type shadowRig struct {
	h *harness.H

	scope    db.TenantScope
	alertID  uuid.UUID
	caseID   uuid.UUID
	identity *identityservice.Service
	actors   slackActors
	snoozes  slackSnoozeActions
}

func newShadowRig(t *testing.T) *shadowRig {
	t.Helper()

	h := harness.New(t)
	org := h.Org()
	cluster := h.Cluster(org)
	alert := h.Alert(org, cluster)
	ac := h.Case(alert)

	identity := identityservice.New(identityservice.Deps{
		Users: identityrepo.NewUserRepository(h.Pool),
		Slack: identityrepo.NewSlackIdentityRepository(h.Pool),
		// ⭐ THE TX RUNNER IS WIRED BECAUSE PRODUCTION WIRES IT, and because it is
		// what holds the row lock the upsert takes: without it two first presses by
		// one member could each mint a shadow. `ResolveSlackPresser` degrades without
		// one rather than refusing, and a rig that omitted it would be testing the
		// degraded path while claiming to test the wired one.
		Tx:     identityrepo.NewTxRunner(h.Pool),
		Clock:  h.Clock,
		Logger: quietLogger(),
	})

	alerts, err := alertsservice.New(alertsservice.Deps{
		Alerts:  alertsrepo.NewAlertRepository(h.Pool, h.Clock, false),
		Cases:   alertsrepo.NewCaseRepository(h.Pool),
		Events:  alertsrepo.NewEventRepository(h.Pool, h.Clock),
		Snoozes: alertsrepo.NewSnoozeRepository(h.Pool, h.Clock),
		Tx:      alertsrepo.NewTxRunner(h.Pool),
		// ⛔ THE CLAIM STORE IS THE SUBJECT OF THE TEST AND NOT A DETAIL OF THE
		// FIXTURE. `Snooze` guards its whole `idempotency.Resolve` block on
		// `if idem.Keyed`, so a rig without a claim store would pass whether or not
		// the intent were keyed — it would simply never take a claim, which is the
		// defect.
		Claims:     idempotency.NewRepository(h.Pool),
		AlertBatch: alertsrepo.NewAlertRepository(h.Pool, h.Clock, false),
		OccBatch:   alertsrepo.NewCaseRepository(h.Pool),
		Clock:      h.Clock,
		Logger:     quietLogger(),
	})
	require.NoError(t, err)

	return &shadowRig{
		h:        h,
		scope:    org.Scope,
		alertID:  alert.ID,
		caseID:   ac.ID,
		identity: identity,
		actors:   slackActors{identity: identity},
		snoozes:  slackSnoozeActions{alerts: alerts},
	}
}

// press drives ONE Slack interaction all the way through, resolving the actor and
// then applying the snooze.
//
// ⚠️ IT REPRODUCES `channels/service.actor`'s THREE-VALUE DECISION RATHER THAN
// CALLING IT, and the reproduction is asserted rather than assumed. That function
// turns a `SlackActor` into (kind, id, label) and falls back to the raw Slack member
// id when `UserID` is zero OR `Label` is empty — the second disjunct being the one
// this change had to satisfy, because a shadow member has no email and an empty
// label would have thrown away the very uuid the shadow exists to provide. The
// assertions below are that contract; importing `channels/service` here would make
// this test depend on the module `adapters.go` exists to keep at arm's length.
func (r *shadowRig) press(t *testing.T, member, handle, interaction string, until time.Time) {
	t.Helper()

	a, err := r.actors.SlackActor(r.h.Ctx, r.scope, harnessTeamID, member, handle)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, a.UserID,
		"an unlinked member's press must resolve to an oto user: that uuid is the claim's "+
			"principal, and without it Snooze skips its claim and the redelivery is applied")
	require.NotEmpty(t, a.Label,
		"an empty label sends channels/service.actor down its unlinked() fallback, which "+
			"discards the uuid above and reinstates the double execution")

	require.NoError(t, r.snoozes.SnoozeAlert(r.h.Ctx, r.scope, r.alertID,
		"user", a.UserID.String(), a.Label, until, interaction))
}

// harnessTeamID is a well-formed Slack workspace id. Any value matching
// `slack_identities_team_ck` does; it is a constant so that two presses in one test
// cannot accidentally name two workspaces and therefore two identities.
const harnessTeamID = "T024BE7LH"

func (r *shadowRig) snoozeRows(t *testing.T) []uuid.UUID {
	t.Helper()
	rows, err := r.h.Pool.Query(r.h.Ctx,
		`SELECT id FROM alert_snoozes WHERE alert_id = $1 ORDER BY created_at, id`, r.alertID)
	require.NoError(t, err)
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	require.NoError(t, rows.Err())
	return out
}

func (r *shadowRig) eventTypes(t *testing.T) []string {
	t.Helper()
	rows, err := r.h.Pool.Query(r.h.Ctx,
		`SELECT type FROM alert_events WHERE alert_id = $1 ORDER BY recorded_at, id`, r.alertID)
	require.NoError(t, err)
	defer rows.Close()

	var out []string
	for rows.Next() {
		var typ string
		require.NoError(t, rows.Scan(&typ))
		out = append(out, typ)
	}
	require.NoError(t, rows.Err())
	return out
}

// ⭐⭐ TestARedeliveredPressByAnUnlinkedMemberIsAppliedOnce is the regression the
// ticket is about.
//
// An unlinked Slack member presses `Snooze 1h`; Slack does not get its 200 in time
// and sends the SAME interaction again. Before this change both executions ran: the
// second closed the incumbent as `superseded`, inserted a second `alert_snoozes`
// row, and the Case timeline carried two `alert.unsnoozed`/`alert.snoozed` pairs for
// ONE human gesture. The duplicate CARD was already prevented — the §C.7 occasion is
// keyed on the interaction — but only a claim can undo the ACT, and the timeline is
// the audit record oto sells.
//
// ⛔ THE ROW COUNT AND THE EVENT LIST ARE BOTH ASSERTED, because either alone is
// satisfiable by a wrong answer. One row with two event pairs would be a snooze that
// was superseded and re-created in place; one event pair with two rows would be an
// append that silently deduplicated. The defect produced two of each.
func TestARedeliveredPressByAnUnlinkedMemberIsAppliedOnce(t *testing.T) {
	r := newShadowRig(t)
	until := r.h.Now().Add(1 * time.Hour)

	// The `response_url` digest `channels/service.interactionKey` mints: byte-identical
	// on a redelivery of one interaction, different for the next press.
	const interaction = "slack:" +
		"3b1f8e0d5c4a2b9f7e6d1c0b8a9f7e6d5c4b3a2918f7e6d5c4b3a2918f7e6d5c"

	r.press(t, "U024BE7LH", "ada", interaction, until)
	first := r.snoozeRows(t)
	require.Len(t, first, 1, "the first press grants one snooze")

	// The redelivery. It must be REFUSED rather than served: `SnoozeAlert` discards the
	// replay flag because a replay is a success from the presser's chair — the snooze
	// they asked for is in force — so the observable is the database, not the return.
	r.press(t, "U024BE7LH", "ada", interaction, until)

	require.Equal(t, first, r.snoozeRows(t),
		"the redelivered press inserted a second alert_snoozes row, so the claim was not "+
			"taken. That is the whole defect: `Snooze` guards idempotency.Resolve on "+
			"`if idem.Keyed`, and an unlinked presser with no user row produced an unkeyed "+
			"intent because idempotency_claims.principal_id is NOT NULL")

	events := r.eventTypes(t)
	require.NotContains(t, events, "alert.unsnoozed",
		"an alert.unsnoozed(superseded) event means the redelivery closed the incumbent and "+
			"started again — two facts on the timeline for one press, which is the audit "+
			"record being wrong rather than merely noisy")
	var snoozed int
	for _, e := range events {
		if e == "alert.snoozed" {
			snoozed++
		}
	}
	require.Equal(t, 1, snoozed, "one gesture, one alert.snoozed event: got %v", events)
}

// ⭐ TestTheFirstPressMintsOneShadowMemberAndTheSecondFindsIt pins the two halves of
// the shadow member that are properties of the SCHEMA rather than of the Go code.
//
// ⛔ THE SECOND PRESS MUST NOT MINT A SECOND ROW, and the reason it does not is the
// row lock `Upsert`'s `ON CONFLICT … DO UPDATE` takes rather than a check in Go: the
// identity is already linked by then, so the linked branch is taken. A second shadow
// would be a duplicate member on the operator's members list AND a second principal,
// which would split one person's claims across two key spaces.
func TestTheFirstPressMintsOneShadowMemberAndTheSecondFindsIt(t *testing.T) {
	r := newShadowRig(t)

	first, err := r.identity.ResolveSlackPresser(r.h.Ctx, r.scope, harnessTeamID, "U024BE7LH", "ada")
	require.NoError(t, err)
	require.True(t, first.User.IsShadow(),
		"the minted row must carry NO EMAIL: a74d6b2 was decided in favour of recording the "+
			"absence of an address rather than inventing a synthetic one")
	require.True(t, first.User.PasswordHash.IsZero(),
		"and no password hash, which is what makes the row unable to log in")
	require.Equal(t, "@ada", first.Identity.ActorLabel(),
		"the timeline label stays the Slack handle it was before the shadow existed, so "+
			"one person does not read as two across the change")
	require.True(t, first.Identity.Linked(),
		"slack_identities_link_ck makes user_id/linked_at all-or-nothing, so a press that "+
			"minted a user and did not bind it is a half-written pair")

	second, err := r.identity.ResolveSlackPresser(r.h.Ctx, r.scope, harnessTeamID, "U024BE7LH", "ada")
	require.NoError(t, err)
	require.Equal(t, first.User.ID, second.User.ID,
		"a second press by the same member must find the same principal; a fresh one would "+
			"give one human two users and two claim key spaces")

	// ⭐ AND A DIFFERENT MEMBER GETS THEIR OWN, WHICH IS THE `users_email_uniq`
	// ASSERTION. The constraint is `UNIQUE (org_id, email)` and it is deliberately
	// UNCHANGED by 00074: Postgres treats NULLs as distinct, so a second NULL-email row
	// in the same org is legal. If that were not true this insert would be a 23505 —
	// the second Slack presser in an org would be unable to press a button at all — so
	// this is the one assertion that proves the migration needed no unique-index change.
	other, err := r.identity.ResolveSlackPresser(r.h.Ctx, r.scope, harnessTeamID, "U0ABCDEFG", "bo")
	require.NoError(t, err)
	require.NotEqual(t, first.User.ID, other.User.ID)
	require.True(t, other.User.IsShadow())

	var shadows int
	require.NoError(t, r.h.Pool.QueryRow(r.h.Ctx,
		`SELECT count(*) FROM users WHERE org_id = $1 AND email IS NULL`,
		r.scope.OrgID()).Scan(&shadows))
	require.Equal(t, 2, shadows,
		"two Slack members, two shadow rows, one org — which many databases would refuse "+
			"and Postgres does not, because a unique constraint treats NULLs as distinct")
}

// ⭐⭐ TestAShadowMemberCannotLogIn is the security half, and it is asserted against
// SQL rather than against Go.
//
// A shadow member is refused three times over and the three are independent: the row
// has no `password_hash`, `User.CanPasswordLogin` is AND-ed with `!IsShadow()`, and
// `resolveByEmailSQL` compares `u.email = $1`, which is never TRUE for a NULL. This
// test attacks the third one, because it is the only one an attacker gets to choose
// the input to — and because `NULL = anything` being not-TRUE is a property of the
// DATABASE that no Go test can observe.
func TestAShadowMemberCannotLogIn(t *testing.T) {
	r := newShadowRig(t)

	presser, err := r.identity.ResolveSlackPresser(r.h.Ctx, r.scope, harnessTeamID, "U024BE7LH", "ada")
	require.NoError(t, err)
	require.True(t, presser.User.IsShadow())
	require.False(t, presser.User.CanPasswordLogin(),
		"a shadow member has no password hash and no address; the domain must refuse it "+
			"before any query runs")

	// ⛔ THE QUERY IS RUN DIRECTLY, WITH EVERY SPELLING OF "NOTHING" A LOGIN COULD
	// CARRY. `Service.Login` cannot even reach the resolver with these, because
	// `domain.NewEmail` rejects all of them — which is a second refusal and not a
	// substitute for this one: the day something calls the resolver from a path that
	// does not parse first, this is what still holds. The empty string is the dangerous
	// candidate, because it is what a `string` field holds when a NULL was meant.
	for _, attempt := range []string{"", " ", "null", "NULL", "%", "U024BE7LH"} {
		var found int
		require.NoError(t, r.h.Pool.QueryRow(r.h.Ctx,
			`SELECT count(*) FROM users u
			  JOIN orgs o ON o.id = u.org_id AND o.deleted_at IS NULL
			 WHERE u.email = $1 AND u.email IS NOT NULL AND u.disabled_at IS NULL`,
			attempt).Scan(&found))
		require.Zero(t, found,
			"resolveByEmailSQL matched a row for %q — a NULL email must never equal a "+
				"presented address, and a shadow member that resolved here would be an "+
				"account nobody ever created", attempt)
	}
}

// ⭐ TestAGenuineLinkAdoptsTheShadowRatherThanLeavingTwoMembers is the "adoption"
// half of the ticket.
//
// A Slack member who pressed a button is already linked — to a shadow. When an
// operator later links that identity to the person's genuine oto account, the shadow
// stops being the answer to "who is this Slack member", and a shadow left live is a
// second row on the members list for one human. `LinkSlackIdentity` retires it in the
// same transaction.
//
// ⚠️ RETIRED MEANS SOFT-DISABLED AND THE HISTORY IS **NOT** MERGED, which this test
// pins in both directions: the shadow's `disabled_at` is set, and the shadow row is
// still THERE. Every `cases.acked_by` and `alert_snoozes.snoozed_by` pointing at it
// is `ON DELETE SET NULL`, so deleting it would strip acknowledgements off timelines
// to tidy a members list. Re-pointing those rows onto the adopting user is a data
// migration with an audit trail to preserve and is deliberately a separate change.
func TestAGenuineLinkAdoptsTheShadowRatherThanLeavingTwoMembers(t *testing.T) {
	r := newShadowRig(t)
	genuine := r.h.User(harness.Org{ID: r.scope.OrgID(), Scope: r.scope})

	presser, err := r.identity.ResolveSlackPresser(r.h.Ctx, r.scope, harnessTeamID, "U024BE7LH", "ada")
	require.NoError(t, err)
	shadowID := presser.User.ID

	linked, err := r.identity.LinkSlackIdentity(r.h.Ctx, r.scope, presser.Identity.ID, genuine.ID)
	require.NoError(t, err)
	require.Equal(t, genuine.ID, linked.UserID)

	var disabled *time.Time
	require.NoError(t, r.h.Pool.QueryRow(r.h.Ctx,
		`SELECT disabled_at FROM users WHERE id = $1`, shadowID).Scan(&disabled))
	require.NotNil(t, disabled,
		"the displaced shadow is still live, so the org now has two members for one "+
			"human: the person who just linked, and the stand-in oto minted for them")

	// The row survives, because the acknowledgements pointing at it do.
	var stillThere int
	require.NoError(t, r.h.Pool.QueryRow(r.h.Ctx,
		`SELECT count(*) FROM users WHERE id = $1`, shadowID).Scan(&stillThere))
	require.Equal(t, 1, stillThere,
		"the shadow must be disabled and NOT deleted: cases.acked_by and "+
			"alert_snoozes.snoozed_by are ON DELETE SET NULL, so a DELETE here succeeds "+
			"quietly and takes the attribution off every timeline it appeared on")

	// ⛔ AND THE SAME METHOD MUST NOT BE ABLE TO DISABLE A REAL ACCOUNT. Linking the
	// identity onward from the genuine user to a second genuine user displaces a row that is
	// not a shadow; `retireShadowSQL`'s WHERE clause is what refuses it, so this holds
	// however the service is called.
	another := r.h.User(harness.Org{ID: r.scope.OrgID(), Scope: r.scope})
	_, err = r.identity.LinkSlackIdentity(r.h.Ctx, r.scope, presser.Identity.ID, another.ID)
	require.NoError(t, err)

	var genuineDisabled *time.Time
	require.NoError(t, r.h.Pool.QueryRow(r.h.Ctx,
		`SELECT disabled_at FROM users WHERE id = $1`, genuine.ID).Scan(&genuineDisabled))
	require.Nil(t, genuineDisabled,
		"a genuine member was soft-disabled by a Slack re-link — retireShadowSQL matches only "+
			"`email IS NULL AND password_hash IS NULL`, and losing that predicate turns this "+
			"method into the `disable a member` operation v1 deliberately does not have (R2)")
}
