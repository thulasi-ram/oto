package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ⭐⭐ WHY THIS FILE EXISTS.
//
// `AppendTimelineEvent` refuses a RETIRED event type (seam.go). That refusal is
// the whole of the "never again" argument on `domain.retiredEventTypes` — *"a
// comment saying 'do not emit this' is advice; a refusal at the single write path
// is a guarantee"* — and until this file it was five lines nothing exercised.
// `event_type_retired` appeared exactly once in the tree, at the `return` that
// produces it.
//
// ⛔ AND IT IS UNREACHABLE FROM EVERY CURRENT CALLER, which is what makes it
// fragile rather than merely untested: `Join`, `JoinMany` and `Leave` were
// deleted with migration 00051, so nothing constructs `EventGroupMemberJoined`
// any more and a reader sweeping for dead code finds a branch no profile ever
// enters. The next person to delete it as unreachable breaks nothing that any
// test would notice — and the branch is precisely the thing standing between a
// future membership event and thirteen months of timeline that must never gain
// one.
//
// ⭐⭐ AND `case.reopened` JOINED THEM, WHICH IS WHY THE SEAM IS NO LONGER THE
// WHOLE STORY. ADR 0040 retired T8 — a closed Case is terminal and a re-fire opens
// the next `seq` — so nothing may append `case.reopened` again either. But the two
// `group.*` values were only ever minted OUTSIDE `alerts`, which is what made one
// check on the seam sufficient for them; `case.reopened` was minted by this
// module's OWN transition table and reached the column through `appendEvents` and
// `appendEventsBatched`, neither of which touches the seam. So the retirement
// needed a second refusal, `refuseRetired`, on the in-module path — and this file
// covers both, because a guarantee proved at one of two doors is not one.
//
// ⚠️ THE OTHER HALF IS AS IMPORTANT AS THE REFUSAL. Retired is not deleted. All
// three values must still PARSE, because `alert_events` contains rows carrying
// them and a timeline that errors is worse than one that renders a fact nobody
// writes any more. A guard that refused them on read as well would satisfy the
// first half of this file and destroy the product. The read side is proved
// against the real row mapper in `alerts/repository`; what is proved here is that
// the refusal is a WRITE-side refusal only.

// retiredTestScope is one tenant, built without a database: the refusal happens
// before any port is touched, which is itself part of what is asserted.
func retiredTestScope(t *testing.T) db.TenantScope {
	t.Helper()
	scope, err := db.NewTenantScope(uuid.New())
	if err != nil {
		t.Fatalf("tenant scope: %v", err)
	}
	return scope
}

// recordingEvents is an EventRepository that records what it was asked to write.
//
// ⭐ IT EMBEDS THE INTERFACE AND IMPLEMENTS ONE METHOD. Every other method is a
// nil call that panics, which is the assertion made structurally: a refused
// append must not reach the repository at all, so any path that did would fail
// loudly rather than quietly counting to zero.
type recordingEvents struct {
	EventRepository
	batches [][]domain.Event
}

func (r *recordingEvents) AppendBatch(_ context.Context, _ db.TenantScope, e []domain.Event) (int, error) {
	r.batches = append(r.batches, e)
	return len(e), nil
}

func (r *recordingEvents) written() []domain.EventType {
	var out []domain.EventType
	for _, batch := range r.batches {
		for _, e := range batch {
			out = append(out, e.Type())
		}
	}
	return out
}

type stubAlertRepo struct{ AlertRepository }
type stubCaseRepo struct{ CaseRepository }
type stubSnoozeRepo struct{ SnoozeRepository }

type inlineTx struct{}

func (inlineTx) InTx(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

// newSeamService builds the real service over stub ports. Nothing about the
// retirement check is faked: `AppendTimelineEvent` is the production method.
func newSeamService(t *testing.T) (*Service, *recordingEvents) {
	t.Helper()

	events := &recordingEvents{}
	svc, err := New(Deps{
		Alerts:  stubAlertRepo{},
		Cases:   stubCaseRepo{},
		Events:  events,
		Snoozes: stubSnoozeRepo{},
		Tx:      inlineTx{},
		Clock:   clock.NewFake(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("build service: %v", err)
	}
	return svc, events
}

// TestAppendTimelineEventRefusesARetiredType is the gate.
func TestAppendTimelineEventRefusesARetiredType(t *testing.T) {
	t.Parallel()

	scope := retiredTestScope(t)

	for _, typ := range []domain.EventType{
		domain.EventGroupMemberJoined,
		domain.EventGroupMemberLeft,
		// ⭐ THE SEAM REFUSES `case.reopened` TOO, and it costs nothing to say so:
		// the check asks `Retired()` rather than naming values, so the third
		// retirement was free here. It is NOT free on the in-module path, which is
		// what the next test is for.
		domain.EventCaseReopened,
		// ⛔ `group.storm_started` AND `group.storm_ended` WERE LISTED HERE AND ARE
		// NOT RETIRED ANY MORE — THEY ARE GONE. Migration 00060 narrows `ev_type_ck`
		// to refuse the spellings and `alerts/domain` no longer declares the
		// constants, so there is no `EventType` value left to hand this seam and
		// nothing for it to refuse. A deleted value is refused one layer earlier,
		// by `NewEventType`, and `TestNewEventTypeRefusesTheDeletedDampers` is where
		// that is asserted.
	} {
		t.Run(typ.String(), func(t *testing.T) {
			svc, events := newSeamService(t)

			err := svc.AppendTimelineEvent(context.Background(), scope, TimelineEventRequest{
				Type:    typ,
				AlertID: uuid.New(),
				CaseID:  uuid.New(),
				Summary: "a fact nothing may record any more",
			})
			if err == nil {
				t.Fatalf("%s was appended. It is retired: `alert_events` still CONTAINS it and "+
					"nothing may add another. This is the single write path, so this refusal is "+
					"the only place the distinction can be made.", typ)
			}

			if got := errs.KindOf(err); got != errs.KindInternal {
				t.Errorf("kind = %v, want %v — no request can ask for a retired type, so "+
					"reaching this means code asked for it and a 4xx would blame the caller",
					got, errs.KindInternal)
			}
			if got := errs.CodeOf(err); got != "event_type_retired" {
				t.Errorf("code = %q, want \"event_type_retired\"", got)
			}
			if !strings.Contains(err.Error(), typ.String()) {
				t.Errorf("the error does not name the type it refused: %v", err)
			}

			// ⛔ AND NOTHING WAS WRITTEN. A refusal that happened after the row went
			// in would satisfy every assertion above and still put the fact on the
			// timeline.
			if n := len(events.batches); n != 0 {
				t.Errorf("the repository saw %d append batch(es): %v", n, events.written())
			}
		})
	}
}

// TestAppendTimelineEventStillAcceptsLiveTypes is the control, and without it the
// test above passes over a seam that refuses everything.
func TestAppendTimelineEventStillAcceptsLiveTypes(t *testing.T) {
	t.Parallel()

	scope := retiredTestScope(t)

	// `case.opened` is the fact that REPLACED the membership events:
	// `group.member_joined` is implied by it, `group.member_left` by the episode
	// ending. If the guard could not tell it from the retired pair it would have
	// taken the replacement with it.
	//
	// ⛔ TWO ENTRIES WERE `group.opened` AND `group.closed` AND THEY ARE REPLACED
	// BY `rule.definition_changed` AND `enrichment.completed` (git-bug `7570090`).
	// The group pair was chosen because it was group-scoped, live, and carried
	// through this same seam — and it was `grouping/service` that minted both. That
	// module is deleted, and with `TimelineEventRequest.GroupID` deleted too a
	// group-scoped request now has NO subject at all, so it would be refused by
	// `ev_subject_ck` before the retirement guard was ever consulted. The control
	// would then have passed for the wrong reason, or failed while claiming the
	// guard was too broad.
	//
	// ⭐ THE REPLACEMENTS ARE THE SEAM'S REAL TRAFFIC, which is what a control
	// should be made of: `rule.*` and `enrichment.*` are exactly what the two
	// remaining callers narrate through `timelineRecorder`, and both are subjected
	// by an alert and an episode rather than by a container.
	for _, tc := range []struct {
		typ domain.EventType
		req TimelineEventRequest
	}{
		{domain.EventRuleDefinitionChanged, TimelineEventRequest{
			AlertID: uuid.New(), CaseID: uuid.New(), Summary: "the rule drifted",
		}},
		{domain.EventCaseOpened, TimelineEventRequest{
			AlertID: uuid.New(), CaseID: uuid.New(), Summary: "case opened",
		}},
		{domain.EventEnrichmentCompleted, TimelineEventRequest{
			AlertID: uuid.New(), CaseID: uuid.New(), Summary: "enrichment completed",
		}},
	} {
		t.Run(tc.typ.String(), func(t *testing.T) {
			svc, events := newSeamService(t)

			req := tc.req
			req.Type = tc.typ
			if err := svc.AppendTimelineEvent(context.Background(), scope, req); err != nil {
				t.Fatalf("%s was refused: %v", tc.typ, err)
			}

			written := events.written()
			if len(written) != 1 || written[0] != tc.typ {
				t.Fatalf("the repository was asked to write %v, want exactly [%s]", written, tc.typ)
			}
			if tc.typ.Retired() {
				t.Errorf("%s reports itself retired, so this control is asserting the opposite "+
					"of what it claims", tc.typ)
			}
		})
	}
}

// TestTheModulesOwnAppendPathRefusesARetiredType is the half the seam could not
// cover, and the half ADR 0040 made necessary.
//
// ⭐ `case.reopened` NEVER CAME THROUGH `AppendTimelineEvent`. It was built by this
// module's own transition table and handed straight to `appendEvents`, so a
// refusal on the seam would have refused it from every caller that never minted
// one and from nobody who did. The T8 rows that built one are gone, which is what
// SHOULD make this unreachable; this is what makes "should" into "cannot".
//
// ⛔ AND NOTHING REACHES THE REPOSITORY. `recordingEvents` counts batches, so a
// guard that ran after the INSERT would be visible here as a batch that should not
// exist rather than as a passing test.
func TestTheModulesOwnAppendPathRefusesARetiredType(t *testing.T) {
	t.Parallel()

	scope := retiredTestScope(t)
	svc, events := newSeamService(t)

	ev := retiredEvent(t, scope.OrgID(), domain.EventCaseReopened)
	n, err := svc.appendEvents(context.Background(), scope, []domain.Event{ev})
	if err == nil {
		t.Fatalf("%s was appended through the in-module path (%d rows). ADR 0040 retired T8, "+
			"so nothing may mint one again — and this path never touches the seam, so this "+
			"refusal is the only place the distinction can be made here.", ev.Type(), n)
	}
	if got := errs.KindOf(err); got != errs.KindInternal {
		t.Errorf("kind = %v, want %v — no request can ask for a retired type, so reaching "+
			"this means code asked for it and a 4xx would blame the caller", got, errs.KindInternal)
	}
	if got := errs.CodeOf(err); got != "event_type_retired" {
		t.Errorf("code = %q, want \"event_type_retired\"", got)
	}
	if !strings.Contains(err.Error(), ev.Type().String()) {
		t.Errorf("the error does not name the type it refused: %v", err)
	}
	if n := len(events.batches); n != 0 {
		t.Errorf("the repository saw %d append batch(es): %v", n, events.written())
	}
}

// retiredEvent builds a well-formed AlertEvent of a retired type. `NewEvent` does
// NOT refuse one, and that is deliberate rather than a gap: the same constructor
// rehydrates the rows `alert_events` already carries, so a kernel that could not
// build one could not read the timeline back.
func retiredEvent(t *testing.T, orgID uuid.UUID, typ domain.EventType) domain.Event {
	t.Helper()

	at, err := domain.NewObservationTime(retiredEventAt, retiredEventAt)
	if err != nil {
		t.Fatalf("observation time: %v", err)
	}
	actor, err := domain.SystemActor(domain.ActorIngest)
	if err != nil {
		t.Fatalf("ingest actor: %v", err)
	}
	ev, err := domain.NewEvent(domain.EventParams{
		ID:      uuid.New(),
		OrgID:   orgID,
		AlertID: uuid.New(),
		CaseID:  uuid.New(),
		Type:    typ,
		At:      at,
		Actor:   actor,
		Summary: "a fact nothing may record any more",
	})
	if err != nil {
		t.Fatalf("build a %s event: %v", typ, err)
	}
	return ev
}

var retiredEventAt = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// TestRetiredTypesAreRefusedOnWriteAndNotOnRead states the asymmetry in one
// place, because it is the whole design and the two halves live in different
// packages.
func TestRetiredTypesAreRefusedOnWriteAndNotOnRead(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"group.member_joined", "group.member_left", "case.reopened"} {
		t.Run(s, func(t *testing.T) {
			typ, err := domain.NewEventType(s)
			if err != nil {
				t.Fatalf("%q no longer parses: thirteen months of `alert_events` carry it, and "+
					"a row that cannot be read is a timeline that errors instead of rendering "+
					"(%v)", s, err)
			}
			if !typ.Retired() {
				t.Fatalf("%s parses but does not report itself retired, so the seam would let "+
					"it back onto the timeline", typ)
			}

			// It is still on the wire contract, which is not an oversight: a value oto
			// can still emit from history must be one its own generated client accepts.
			var onContract bool
			for _, live := range domain.AllEventTypes() {
				if live == typ {
					onContract = true
				}
			}
			if !onContract {
				t.Errorf("%s is readable but absent from AllEventTypes, so "+
					"`components.schemas.AlertEventType` cannot describe a timeline "+
					"containing it", typ)
			}
		})
	}
}
