package service

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
)

// ⛔⛔ WHICH ALERT'S CLOCK REACHES THE CARD.
//
// git-bug 1f7bdd0: a snooze reached Slack as a red ":fire: Firing" card, because
// `NotificationView` had no snooze axis and the renderer cannot draw a fact it
// cannot see. `snoozedUntil` is the projection that fixed it, and the ONE
// judgment call in it is the question this file exists to pin: a snooze is scoped
// to an `alert_key` (§B.8.1) while a card is about a group, so when the two
// disagree, whose wake-up time does the card print?
//
// The answer is the focus's, matching `Snapshot.FocusSnoozed`, and getting it
// wrong is not a rounding error — it puts one member's countdown on a card about
// another member, which is a specific false statement to the person who typed the
// snooze. The group-wide answer applies only where there is no focus at all.
//
// It is a pure test in the internal package on purpose. `project` "performs no
// I/O, so the seam is exercisable from a snapshot literal" — and the sibling view
// tests need a real Postgres because they read a table this module does not own,
// which this one does not.
func TestTheCardPrintsTheFocusedAlertsSnoozeAndNotTheGroups(t *testing.T) {
	t.Parallel()

	var (
		focusID = uuid.New()
		otherID = uuid.New()
		base    = time.Date(2026, 8, 19, 17, 56, 0, 0, time.UTC)
		soon    = base.Add(30 * time.Minute)
		late    = base.Add(7 * 24 * time.Hour)
	)

	for _, tc := range []struct {
		name string
		snap domain.Snapshot
		want *time.Time
	}{
		{
			name: "no snooze anywhere is nil, not the zero time",
			snap: domain.Snapshot{Focus: &domain.AlertFacts{ID: focusID}},
			want: nil,
		},
		{
			// The whole point of the ticket: the fact the renderer could not reach.
			name: "the focus's own row",
			snap: domain.Snapshot{
				Focus: &domain.AlertFacts{ID: focusID, SnoozedUntil: &soon},
			},
			want: &soon,
		},
		{
			// ⛔ THE REGRESSION THIS FILE IS REALLY FOR. Taking the group's latest
			// here would print `late` — another member's wake-up — on a card whose
			// subject is the focused alert, and it would look entirely plausible.
			name: "the focus wins over a later-waking sibling",
			snap: domain.Snapshot{
				Focus:         &domain.AlertFacts{ID: focusID},
				SnoozedAlerts: map[uuid.UUID]time.Time{focusID: soon, otherID: late},
			},
			want: &soon,
		},
		{
			// A focused alert that is not itself snoozed reports no snooze, even
			// while the group around it is quiet. Anything else would tell the
			// reader oto has gone quiet about the one thing it is talking about.
			name: "a focus absent from the map has no snooze of its own",
			snap: domain.Snapshot{
				Focus:         &domain.AlertFacts{ID: focusID},
				SnoozedAlerts: map[uuid.UUID]time.Time{otherID: late},
			},
			want: nil,
		},
		{
			// No focus: the card is about the group, and a reader of a partly
			// snoozed group is asking when oto starts talking again. That is when
			// the LAST of them wakes — the same reading `grouping/api` takes of the
			// same fact. Map iteration order is random, so this also pins that the
			// answer does not depend on it.
			name: "no focus takes the latest, not whichever came first",
			snap: domain.Snapshot{
				SnoozedAlerts: map[uuid.UUID]time.Time{focusID: soon, otherID: late},
			},
			want: &late,
		},
		{
			name: "no focus and no snoozed members is nil",
			snap: domain.Snapshot{SnoozedAlerts: map[uuid.UUID]time.Time{}},
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := snoozedUntil(tc.snap)

			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("want no snooze, got %s", got.UTC())
			case tc.want == nil:
				return
			case got == nil:
				t.Fatalf("want %s, got no snooze", tc.want.UTC())
			case !got.Equal(*tc.want):
				t.Fatalf("want %s, got %s", tc.want.UTC(), got.UTC())
			}
		})
	}
}

// ⚠️ A SNOOZE WHOSE CLOCK HAS ALREADY RUN OUT STILL REACHES THE CARD, and that is
// deliberate rather than an oversight in the predicate.
//
// The row is live until the 60-second expiry sweep ends it, so `snoozed_until` can
// be in the past while the row is still in force. Filtering it here against the
// snapshot's own clock would erase the until-when from the very card being sent to
// announce it — whether oto speaks was decided upstream, and this projection is
// not entitled to re-decide it. `Snapshot.Snoozed(now)` is where that question is
// asked, and it takes an instant precisely because this does not.
func TestAnExpiredButUnsweptSnoozeStillNamesWhenTheQuietEnds(t *testing.T) {
	t.Parallel()

	var (
		id    = uuid.New()
		past  = time.Date(2026, 8, 19, 17, 0, 0, 0, time.UTC)
		taken = past.Add(45 * time.Second)
		snap  = domain.Snapshot{Focus: &domain.AlertFacts{ID: id, SnoozedUntil: &past}, TakenAt: taken}
		got   = snoozedUntil(snap)
	)

	if got == nil {
		t.Fatal("an unswept snooze row must still tell the card when the quiet ends")
	}
	if !got.Equal(past) {
		t.Fatalf("want %s, got %s", past, got.UTC())
	}
}
