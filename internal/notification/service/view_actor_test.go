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

// causeFixture is one group with one alert, one open episode, and whatever the
// timeline says happened to it.
type causeFixture struct {
	h       *harness.H
	org     harness.Org
	cluster harness.Cluster
	group   harness.Group
	alert   harness.Alert
	occ     harness.Occurrence
}

func newCauseFixture(t *testing.T) causeFixture {
	t.Helper()

	h := harness.New(t)
	org := h.Org()
	cluster := h.Cluster(org)
	source := h.Source(org, cluster)
	group := h.Group(org, source, cluster)
	alert := h.Alert(org, cluster)
	occ := h.Occurrence(alert, group)

	return causeFixture{h: h, org: org, cluster: cluster, group: group, alert: alert, occ: occ}
}

// appendEvent writes one timeline row the way `alerts` writes it.
//
// `recorded_at` is the REAL clock rather than the harness's fake one because
// `alert_events` is partitioned monthly on that column and has no default
// partition: a row stamped years away from the partitions the migrations built
// has nowhere to land.
func (f causeFixture) appendEvent(eventType, actorKind, actorID, actorLabel, body string) {
	f.h.T.Helper()
	f.appendEventFor(f.alert.ID, f.occ.ID, eventType, actorKind, actorID, actorLabel, body)
}

// appendEventFor writes the same row against ANY member of the group, which is
// what makes a sibling's action expressible: every event carries the group id, so
// a read scoped to the group sees every member's timeline at once.
func (f causeFixture) appendEventFor(
	alertID, occID uuid.UUID, eventType, actorKind, actorID, actorLabel, body string,
) {
	f.h.T.Helper()

	payload := `{}`
	if body != "" {
		payload = `{"body": ` + quoteJSON(body) + `}`
	}
	f.h.Exec(`INSERT INTO alert_events
	            (id, org_id, alert_id, occurrence_id, group_id, type, occurred_at,
	             recorded_at, actor_kind, actor_id, actor_label, summary, payload)
	          VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, now(), now(), $6,
	                  nullif($7,''), nullif($8,''), $9, $10::jsonb)`,
		f.org.ID, alertID, occID, f.group.ID, eventType,
		actorKind, actorID, actorLabel, "a harness event: "+eventType, payload)
}

// sibling is a second member of the same group: another alert, another open
// episode, acted on by somebody else.
func (f causeFixture) sibling() (harness.Alert, harness.Occurrence) {
	f.h.T.Helper()

	alert := f.h.AlertWith(f.org, f.cluster, map[string]string{
		"alertname": "HighErrorRate", "severity": "critical", "service": "payments",
	})
	return alert, f.h.Occurrence(alert, f.group)
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

	alertID, occID := f.alert.ID, f.occ.ID
	return f.build(t, domain.Notification{
		GroupID: f.group.ID, AlertID: &alertID, OccurrenceID: &occID, Reason: reason,
	})
}

// viewByAlert is the same projection for a notification that names an ALERT and
// nothing narrower — the shape the policy-preview endpoint mints, since it takes
// exactly one of alert_id / occurrence_id / group_id plus any reason. The read
// model resolves the episode from the alert; the cause has to narrow the same
// way or the card names one person for another person's action.
func (f causeFixture) viewByAlert(t *testing.T, reason domain.Reason) *service.NotificationView {
	t.Helper()

	alertID := f.alert.ID
	return f.build(t, domain.Notification{
		GroupID: f.group.ID, AlertID: &alertID, Reason: reason,
	})
}

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
	f.appendEvent("occurrence.acknowledged", "slack", "U0123456789", "@ram", "")

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
	f.appendEvent("occurrence.unacknowledged", "system", "", "", "")

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
	f.appendEvent("occurrence.acknowledged", "slack", "U0123456789", "@ram", "")

	v := f.view(t, domain.ReasonFired)

	require.Nil(t, v.Actor, "a `fired` card was attributed to the person who acked it")
	require.Empty(t, v.Comment)
}

// The lookup is scoped to the EPISODE the notification names, not to the group.
// A group is many alerts, they are acknowledged one by one, and a group-scoped
// read would put whoever acted most recently against everybody else's card.
func TestTheActorComesFromTheEpisodeTheCardIsAboutAndNotFromASibling(t *testing.T) {
	t.Parallel()

	f := newCauseFixture(t)
	f.appendEvent("occurrence.acknowledged", "user", uuid.NewString(), "ada@example.com", "")

	// A second member of the same group, acknowledged afterwards by somebody else.
	sibling, siblingOcc := f.sibling()
	f.appendEventFor(sibling.ID, siblingOcc.ID, "occurrence.acknowledged",
		"user", uuid.NewString(), "grace@example.com", "")

	v := f.view(t, domain.ReasonAcked)

	require.NotNil(t, v.Actor)
	require.Equal(t, "ada@example.com", v.Actor.Label,
		"the card named the sibling episode's acker: the lookup is not scoped to the "+
			"occurrence this notification is about")
}

// ⛔ THE SAME SCOPING, FOR A NOTIFICATION THAT NAMES AN ALERT AND NO OCCURRENCE.
// `POST /policies/preview` accepts exactly one of alert_id / occurrence_id /
// group_id and any reason, so `alert_id` + `reason=acked` is a shape an operator
// can ask for at any time. The episode is resolved from the alert; if the cause
// falls back to the GROUP instead, the card pairs Ada's episode with the newest
// acknowledgement in the whole group — grace's — and a preview of a routing
// decision becomes a confident lie about who acted.
func TestAPreviewByAlertReadsTheCauseFromThatAlertAndNotFromTheGroup(t *testing.T) {
	t.Parallel()

	f := newCauseFixture(t)
	f.appendEvent("occurrence.acknowledged", "user", uuid.NewString(), "ada@example.com", "")

	// The sibling acts LAST, so a group-scoped read returns it every time.
	sibling, siblingOcc := f.sibling()
	f.appendEventFor(sibling.ID, siblingOcc.ID, "occurrence.acknowledged",
		"user", uuid.NewString(), "grace@example.com", "")
	f.appendEventFor(sibling.ID, siblingOcc.ID, "comment.added",
		"user", uuid.NewString(), "grace@example.com", "on the payments alert, not this one")

	v := f.viewByAlert(t, domain.ReasonAcked)

	require.NotNil(t, v.Actor, "the previewed card names nobody at all")
	require.Equal(t, "ada@example.com", v.Actor.Label,
		"a preview by alert_id named the sibling's acker: the cause fell back to the "+
			"group while the episode came from the alert")

	// The same fallback drags a sibling's WORDS onto the card, which is the part
	// that cannot be shrugged off as a stale name.
	c := f.viewByAlert(t, domain.ReasonComment)
	require.Empty(t, c.Comment,
		"a preview by alert_id carried another alert's comment body")
}
