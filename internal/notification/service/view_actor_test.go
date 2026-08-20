package service_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/notification/service"
	"github.com/thulasiram/oto/test/harness"
)

// ⛔⛔ WHO CAUSED THIS CARD, AND WHAT THEY SAID.
//
// git-bug 56a9951: `NotificationView.Actor` was nil and `.Comment` was empty in
// every deployment, on every card, because nothing wrote either one. The Slack
// renderer has drawn both since the beginning — `Acknowledged by …`, the comment
// body — so the whole defect lived in this read path.
//
// The fix reads the RECORD instead of carrying a copy: the actor and the comment
// body are already on `alert_events`, written once by the module that owns the
// human verb, in the same transaction that enqueued the notification. That is
// why these tests run against a real Postgres — the thing under test is a query
// against a table this module does not own, and a fake source cannot be wrong
// about it in any of the ways that matter.

// causeFixture is one alert, one open episode — which is the whole conversation
// since git-bug `7570090` — and whatever the timeline says happened to it.
type causeFixture struct {
	h       *harness.H
	org     harness.Org
	cluster harness.Cluster
	alert   harness.Alert
	ac      harness.Case
}

func newCauseFixture(t *testing.T) causeFixture {
	t.Helper()

	h := harness.New(t)
	org := h.Org()
	cluster := h.Cluster(org)
	alert := h.Alert(org, cluster)
	ac := h.Case(alert)

	return causeFixture{h: h, org: org, cluster: cluster, alert: alert, ac: ac}
}

// appendEvent writes one timeline row the way `alerts` writes it.
//
// `recorded_at` is the REAL clock rather than the harness's fake one because
// `alert_events` is partitioned monthly on that column and has no default
// partition: a row stamped years away from the partitions the migrations built
// has nowhere to land.
func (f causeFixture) appendEvent(eventType, actorKind, actorID, actorLabel, body string) {
	f.h.T.Helper()
	f.appendEventFor(f.alert.ID, f.ac.ID, eventType, actorKind, actorID, actorLabel, body)
}

// appendEventFor writes the same row against ANY episode in the tenant, which is
// what makes a sibling's action expressible.
//
// ⛔ IT NO LONGER WRITES `group_id`, AND THAT MIRRORS THE PRODUCTION WRITER
// (git-bug `7570090`). `repository.subjectOfEvent` returns nil always now, so
// nothing oto appends names a group; the column and `ev_subject_ck` survive as the
// 00051/00054 READABLE-BUT-UNWRITABLE bargain over thirteen months of history.
// `alert_id` plus `case_id` satisfies the CHECK on their own, which is exactly what
// the production path relies on.
func (f causeFixture) appendEventFor(
	alertID, caseID uuid.UUID, eventType, actorKind, actorID, actorLabel, body string,
) {
	f.h.T.Helper()

	payload := `{}`
	if body != "" {
		payload = `{"body": ` + quoteJSON(body) + `}`
	}
	f.h.Exec(`INSERT INTO alert_events
	            (id, org_id, alert_id, case_id, type, occurred_at,
	             recorded_at, actor_kind, actor_id, actor_label, summary, payload)
	          VALUES (gen_random_uuid(), $1, $2, $3, $4, now(), now(), $5,
	                  nullif($6,''), nullif($7,''), $8, $9::jsonb)`,
		f.org.ID, alertID, caseID, eventType,
		actorKind, actorID, actorLabel, "a harness event: "+eventType, payload)
}

// sibling is a second CONVERSATION in the same tenant: another alert, another open
// episode, acted on by somebody else.
//
// ⛔ IT WAS "a second member of the same group" AND THERE IS NO GROUP (git-bug
// `7570090`). What it is FOR is unchanged and is the point: it supplies a
// competing, NEWER cause event that a lookup keyed on anything wider than
// `case_id` would return instead of the right one.
func (f causeFixture) sibling() (harness.Alert, harness.Case) {
	f.h.T.Helper()

	alert := f.h.AlertWith(f.org, f.cluster, map[string]string{
		"alertname": "HighErrorRate", "severity": "critical", "service": "payments",
	})
	return alert, f.h.Case(alert)
}

// quoteJSON is enough JSON quoting for a test literal.
func quoteJSON(s string) string {
	out := make([]rune, 0, len(s)+2)
	out = append(out, '"')
	for _, r := range s {
		if r == '"' || r == '\\' {
			out = append(out, '\\')
		}
		out = append(out, r)
	}
	return string(append(out, '"'))
}

// view projects the card oto would render for one fact, THROUGH the real read
// model — the same two calls `DispatchService.claim` makes at claim time.
func (f causeFixture) view(t *testing.T, reason domain.Reason) *service.NotificationView {
	t.Helper()

	alertID, caseID := f.alert.ID, f.ac.ID
	return f.build(t, domain.Notification{
		// The Case is the conversation, and `ViewService.Build` keys the snapshot on
		// `ConversationID` — so naming it is what makes this the real claim-time shape
		// rather than a literal that happens to work.
		ConversationKind: domain.ConversationCase,
		ConversationID:   caseID,
		AlertID:          &alertID,
		CaseID:           &caseID,
		Reason:           reason,
	})
}

// ⛔ `viewByAlert` WAS HERE AND IS DELETED (git-bug `7570090`). It projected a
// notification that named an ALERT and no case — "the shape the policy-preview
// endpoint mints, since it takes exactly one of alert_id / case_id / group_id" —
// and that shape can no longer be built. `POST /policies/preview` takes `case_id`
// ONLY, and `ViewService.Build` keys the snapshot on `ConversationID`, so a
// notification with no conversation reads against the nil UUID and resolves to
// nothing. The episode is no longer RESOLVED from the alert; it is named.

func (f causeFixture) build(t *testing.T, n domain.Notification) *service.NotificationView {
	t.Helper()

	views, err := service.NewViewService(service.ViewConfig{
		Snapshots: repository.NewSnapshotRepository(f.h.Pool, f.h.Clock),
		BaseURL:   "https://oto.example.com",
		Clock:     f.h.Clock,
	})
	require.NoError(t, err)

	out, err := views.Build(f.h.Ctx, harness.Scope(t, f.org.ID), service.ViewRequest{Notification: n})
	require.NoError(t, err)
	return out
}

// A human acting through Slack is recorded as `actor_kind = 'slack'` with their
// member id, because a Slack member with no oto account still acks. The view
// must carry both far enough for the renderer to mint a real `<@U…>` mention —
// which is the ONE difference between naming somebody and mentioning them.
func TestTheAckedCardCarriesTheHumanWhoPressedTheButton(t *testing.T) {
	t.Parallel()

	f := newCauseFixture(t)
	f.appendEvent("case.acknowledged", "slack", "U0123456789", "@ram", "")

	v := f.view(t, domain.ReasonAcked)

	require.NotNil(t, v.Actor, "the acked card names nobody: the actor never reached the view")
	require.Equal(t, "slack_user", v.Actor.Kind,
		"the timeline's `slack` did not become the view's `slack_user`, so the renderer "+
			"will print the member id as text instead of mentioning them")
	require.Equal(t, "U0123456789", v.Actor.ID)
	require.Equal(t, "@ram", v.Actor.Label)
}

// ⛔ THE COMMENT IS THE WHOLE NOTIFICATION. A `comment` exists only because
// somebody wrote one, and CONTEXT.md §6 records that those words live nowhere
// else once the timeline is gone.
func TestTheCommentCardCarriesTheWordsSomebodyTyped(t *testing.T) {
	t.Parallel()

	const body = `Provider confirmed a "regional" incident; ETA 20m.`

	f := newCauseFixture(t)
	f.appendEvent("comment.added", "user", uuid.NewString(), "ada@example.com", body)

	v := f.view(t, domain.ReasonComment)

	require.Equal(t, body, v.Comment, "the comment reply would be a bare emoji")
	require.NotNil(t, v.Actor)
	require.Equal(t, "ada@example.com", v.Actor.Label)
}

// A system transition is ATTRIBUTED TO A MACHINE, not left blank: T10 drops an
// acknowledgement when a new episode opens, and the card has to be able to tell
// that apart from "oto does not know who did this". Only a human actor carries a
// label (ev_actor_ck), so the kind is the whole of the answer.
func TestASystemTransitionCarriesItsMachineActorAndNoName(t *testing.T) {
	t.Parallel()

	f := newCauseFixture(t)
	f.appendEvent("case.unacknowledged", "system", "", "", "")

	v := f.view(t, domain.ReasonUnacked)

	require.NotNil(t, v.Actor,
		"a system transition arrived indistinguishable from an unattributed one")
	require.Equal(t, "system", v.Actor.Kind)
	require.Empty(t, v.Actor.Label)
}

// ⛔ THE REASON DECIDES WHICH EVENT IS THE CAUSE, AND MOST REASONS HAVE NONE.
// A card announcing that the alert fired must not inherit the name of whoever
// acknowledged it an hour ago — and the reasons with no cause to look up must
// not pay for a query on the delivery path either.
func TestAFactNobodyCausedIsNotAttributedToTheLastPersonWhoActed(t *testing.T) {
	t.Parallel()

	f := newCauseFixture(t)
	f.appendEvent("case.acknowledged", "slack", "U0123456789", "@ram", "")

	v := f.view(t, domain.ReasonFired)

	require.Nil(t, v.Actor, "a `fired` card was attributed to the person who acked it")
	require.Empty(t, v.Comment)
}

// The lookup is scoped to the EPISODE the notification names, and to nothing
// wider.
//
// ⛔ IT USED TO SAY "not to the group", and the group is gone (git-bug `7570090`) —
// but the rule it protected is not, because `causeByCaseSQL` keys on `case_id`
// inside an org and a tenant holds many episodes being acknowledged one by one. Any
// widening of that predicate — back to a group, out to the alert, out to the org —
// puts whoever acted most recently against everybody else's card.
func TestTheActorComesFromTheEpisodeTheCardIsAboutAndNotFromASibling(t *testing.T) {
	t.Parallel()

	f := newCauseFixture(t)
	f.appendEvent("case.acknowledged", "user", uuid.NewString(), "ada@example.com", "")

	// A second conversation in the same tenant, acknowledged afterwards by somebody
	// else — so a lookup that is not keyed on this Case returns Grace, not Ada.
	sibling, siblingCase := f.sibling()
	f.appendEventFor(sibling.ID, siblingCase.ID, "case.acknowledged",
		"user", uuid.NewString(), "grace@example.com", "")

	v := f.view(t, domain.ReasonAcked)

	require.NotNil(t, v.Actor)
	require.Equal(t, "ada@example.com", v.Actor.Label,
		"the card named the sibling episode's acker: the lookup is not scoped to the "+
			"case this notification is about")
}

// ⛔ `TestAPreviewByAlertReadsTheCauseFromThatAlertAndNotFromTheGroup` WAS HERE AND
// IS DELETED (git-bug `7570090`). Its whole subject was a notification carrying an
// `alert_id` and NO case — it asserted that the cause narrowed to that alert's
// episode rather than falling back to the group — and BOTH halves of that premise
// are gone: the preview endpoint no longer accepts `alert_id`, and there is no
// group for a lookup to fall back to. Retargeting it would have meant inventing a
// question it never asked. What survives of it is the test directly above, which
// pins the same rule — the cause is scoped to the episode the card names, never to
// a newer event on a different one — in the only shape that still exists.
