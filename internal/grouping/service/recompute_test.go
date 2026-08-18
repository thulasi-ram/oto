package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	alerts "github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/grouping/domain"
	"github.com/thulasiram/oto/internal/grouping/repository"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// A storm is exactly when the rollup must stop being O(alerts).
//
// A 500-alert Alertmanager batch lands as ONE notification group, so it opens 500
// episodes that all belong to ONE generation. Re-deriving that generation once per
// episode means 500 full aggregates over `alert_cases` and 500
// compare-and-set writes to a single `alert_groups` row — writes the CAS then
// serialises, making ingestion slowest exactly while Alertmanager is retrying
// hardest and its ~5-minute budget (ADR 0007) is running out. 499 of the 500
// results are discarded, because a rollup is a pure projection of the settled
// membership.
//
// ⭐ SINCE 00051 THE BATCH IS NOT AN ARGUMENT TO THIS SERVICE AT ALL. Membership
// is `alert_cases.group_id`, written by `alerts` as each episode opens, so
// there is nothing for grouping to insert and `JoinMany` is gone. What is left is
// the derivation, and the property under test is that ONE call is ONE of
// everything — the guard against a future caller putting it back in a loop.
//
// This test pins the shape rather than the timing: the work is counted through
// fake ports, so it needs no database and cannot go green because a machine was
// fast.
func TestRecomputeIsOneOfEverythingPerGeneration(t *testing.T) {
	t.Parallel()

	h := newJoinHarness(t)
	h.members.total = 500

	g, err := h.svc.Recompute(h.ctx, h.scope, h.groupID, h.at)
	if err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	if got := g.Counts().Total; got != 500 {
		t.Fatalf("total = %d, want 500 — the projection did not settle over the batch", got)
	}

	// THE POINT: the projection is O(generations), not O(alerts).
	if got := h.members.rollups; got != 1 {
		t.Errorf("members.Rollup calls = %d, want 1 for a one-group batch", got)
	}
	if got := h.groups.setRollups; got != 1 {
		t.Errorf("SetRollup writes = %d, want 1 for a one-group batch", got)
	}
	if got := h.groups.reads; got != 1 {
		t.Errorf("GetByID reads = %d, want 1 for a one-group batch", got)
	}
	if got := h.tx.calls; got != 1 {
		t.Errorf("transactions = %d, want 1 for a one-group batch", got)
	}
	// ⛔ ZERO ORG SETTINGS READS, WHERE IT USED TO BE "ONE PER BATCH, NOT ONE PER
	// MEMBER". The one setting the rollup ever read was the storm policy, and storm
	// damping is removed; `group_close_delay_s` is read by `CloseIdle` and by nothing
	// here. A read reappearing on this path is a tuning lookup on the hot ingest loop.
	if got := h.settings.reads; got != 0 {
		t.Errorf("org settings reads = %d, want 0 — the rollup reads no org tuning", got)
	}
	// The UI is told once, so the SSE spine does not carry 500 frames describing
	// the same generation.
	if got := h.stream.appends; got != 1 {
		t.Errorf("group.upserted frames = %d, want 1", got)
	}
}

// ⛔ THE TWO MEMBER EVENTS ARE RETIRED AND MUST NEVER BE APPENDED AGAIN.
//
// `group.member_joined` and `group.member_left` were facts about the EPISODE
// phrased as if the group were the actor, and each is implied by one that
// survives: `case.opened`, and `case.resolved`/`.expired`. They stay
// in the closed EventType enum because thirteen months of `alert_events` still
// contain the first of them — but nothing may write one, and the settling of a
// generation is the exact place they used to come from.
func TestSettlingAGenerationAppendsNoMembershipEvents(t *testing.T) {
	t.Parallel()

	h := newJoinHarness(t)
	h.members.total = 12

	if _, err := h.svc.Recompute(h.ctx, h.scope, h.groupID, h.at); err != nil {
		t.Fatalf("Recompute: %v", err)
	}

	// ⚠️ NOT JUST THE TWO BY NAME. A membership event returning under a third
	// spelling is the same defect, so what is asserted is that nothing RETIRED was
	// appended at all.
	for typ, n := range h.events.byType {
		if typ.Retired() && n > 0 {
			t.Errorf("%s events = %d, want 0 — settling a generation is where the two member "+
				"events used to be appended, and the type is retired", typ, n)
		}
	}

	// ⛔ AND THE COUNTER IS NOT ZERO BY CONSTRUCTION. `h.events` counts what the
	// service asked for, so "no membership events" means nothing unless the port is
	// demonstrably the one the service appends through.
	//
	// ⚠️ THE CONTROL USED TO BE THE STORM PATH ON THIS SAME HARNESS — 500 distinct
	// joins, one `group.storm_started` — and storm damping is removed, so it is the
	// CLOSE path instead. It goes through the identical `appendGroupEvent` seam, and
	// settling a generation now appends nothing at all, which makes a wired-port proof
	// mandatory rather than merely tidy.
	control := newJoinHarness(t)
	quiet := goneQuiet(t, control)
	control.groups.group = quiet
	control.groups.candidates = []domain.Group{quiet}
	control.members.total = 0
	if _, err := control.svc.CloseIdle(control.ctx, control.scope, 10); err != nil {
		t.Fatalf("CloseIdle (control): %v", err)
	}
	if got := control.events.byType[kernel.EventGroupClosed]; got != 1 {
		t.Fatalf("the events port recorded %d group.closed — it is not wired to the "+
			"service, so the absence asserted above is an artefact of the harness",
			got)
	}
	for typ, n := range control.events.byType {
		if typ.Retired() && n > 0 {
			t.Errorf("%s events = %d on the close path, want 0", typ, n)
		}
	}
}

// TestTheSeamRefusesARetiredTypeThroughThisPort is the assertion that used to be
// `!typ.Retired()` — a lookup in a map in another package, whose failure message
// claimed something about `AppendTimelineEvent` that it never touched.
//
// ⭐ IT CALLS THE REAL WRITER, THROUGH THE REAL PORT. `EventAppender` (ports.go)
// is satisfied in production by `alerts/service.Service`, and the guarantee
// `domain.retiredEventTypes` rests on is that THAT implementation refuses a
// retired type — not that the type reports itself retired. So the test builds the
// real service over stub repositories and hands it exactly the request grouping
// would have sent.
func TestTheSeamRefusesARetiredTypeThroughThisPort(t *testing.T) {
	t.Parallel()

	scope, err := db.NewTenantScope(uuid.New())
	if err != nil {
		t.Fatalf("NewTenantScope: %v", err)
	}

	for _, typ := range []kernel.EventType{
		kernel.EventGroupMemberJoined,
		kernel.EventGroupMemberLeft,
	} {
		t.Run(typ.String(), func(t *testing.T) {
			var port EventAppender = newRealTimelineWriter(t)

			err := port.AppendTimelineEvent(context.Background(), scope, alerts.TimelineEventRequest{
				Type:    typ,
				GroupID: uuid.New(),
				AlertID: uuid.New(),
				Summary: "an alert joined the generation",
			})
			if err == nil {
				t.Fatalf("the real writer accepted %s. Membership stopped being an event with "+
					"migration 00051; a future caller putting it back must fail at the write "+
					"path, because a comment is advice and a refusal is a guarantee.", typ)
			}
			if got := errs.CodeOf(err); got != "event_type_retired" {
				t.Errorf("code = %q, want \"event_type_retired\" — %s was refused for some "+
					"other reason, so this test would keep passing after the guard was deleted",
					got, typ)
			}
			if got := errs.KindOf(err); got != errs.KindInternal {
				t.Errorf("kind = %v, want %v: no request can ask for this, so reaching it means "+
					"code did", got, errs.KindInternal)
			}
		})
	}

	// ⛔ THE CONTROL. Without it the test above passes over a writer that refuses
	// everything, which would be a far worse bug than the one it guards.
	t.Run("a live group fact is accepted", func(t *testing.T) {
		port := newRealTimelineWriter(t)
		if err := port.AppendTimelineEvent(context.Background(), scope, alerts.TimelineEventRequest{
			// ⚠️ THIS WAS `group.storm_started` UNTIL IT WAS RETIRED. `group.closed`
			// is the live group fact this module still appends through the seam.
			Type:    kernel.EventGroupClosed,
			GroupID: uuid.New(),
			Summary: "generation closed",
		}); err != nil {
			t.Fatalf("the real writer refused a live type: %v", err)
		}
	})
}

// newRealTimelineWriter builds the production `alerts/service.Service` over stub
// repositories. The retirement check runs before any of them is touched, which is
// why stubs that panic on use are the right shape here.
func newRealTimelineWriter(t *testing.T) *alerts.Service {
	t.Helper()

	svc, err := alerts.New(alerts.Deps{
		Alerts:  stubAlertRepo{},
		Cases:   stubCaseRepo{},
		Events:  stubEventRepo{},
		Snoozes: stubSnoozeRepo{},
		Tx:      inlineTx{},
		Clock:   clock.NewFake(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("build alerts service: %v", err)
	}
	return svc
}

// The stubs embed their interface, so every method they do not name is a nil call
// that panics. That is deliberate: a refused append must reach no repository, and
// an accepted one must reach exactly `AppendBatch`.
type (
	stubAlertRepo  struct{ alerts.AlertRepository }
	stubCaseRepo   struct{ alerts.CaseRepository }
	stubSnoozeRepo struct{ alerts.SnoozeRepository }
	stubEventRepo  struct{ alerts.EventRepository }
	inlineTx       struct{}
)

func (stubEventRepo) AppendBatch(
	_ context.Context, _ db.TenantScope, e []kernel.Event,
) (int, error) {
	return len(e), nil
}

func (inlineTx) InTx(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

// ⛔⛔ `TestSettlingAnnouncesOneStormPerBatch` WAS HERE AND IS DELETED WITH THE
// EVALUATION IT PINNED. It asserted that a 500-episode batch produced ONE storm
// evaluation, ONE `SetStorm` write, ONE `group.storm_started` row and ONE
// `notify.evaluate` job — the loudness budget behind ADR 0020's
// `channels.storm_notice_at` latch, which existed so a channel was told once rather
// than 500 times that oto had started withholding.
//
// ⭐ THE PROPERTY IT PROVED IS NOT LOST, IT IS UNNEEDED. "One of everything per
// generation per batch" is still asserted above, over the rollup, which is the only
// thing `Recompute` does now. What the deleted test additionally pinned was the
// visibility of a damper, and the damper is gone: storm collapse held a generation to
// one root card and dropped every per-alert reply, which made a withheld notification
// indistinguishable from a signal that never fired. See the tombstone at the top of
// `domain/lifecycle.go`.

// ------------------------------------------------------------------ harness

type joinHarness struct {
	ctx      context.Context
	scope    db.TenantScope
	at       time.Time
	groupID  uuid.UUID
	svc      *Service
	groups   *fakeGroups
	members  *fakeMembers
	events   *fakeEvents
	stream   *fakeStream
	settings *fakeSettings
	enqueuer *fakeEnqueuer
	tx       *fakeTx
}

func newJoinHarness(t *testing.T) *joinHarness {
	t.Helper()

	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	orgID := uuid.New()
	scope, err := db.NewTenantScope(orgID)
	if err != nil {
		t.Fatalf("NewTenantScope: %v", err)
	}
	key, err := domain.NewGroupKey("gk_0123456789abcdefghijklmnop")
	if err != nil {
		t.Fatalf("NewGroupKey: %v", err)
	}
	g, err := domain.NewGroup(domain.GroupParams{
		ID:             uuid.New(),
		OrgID:          orgID,
		SourceID:       uuid.New(),
		ClusterID:      uuid.New(),
		ClusterKey:     "prod",
		Key:            key,
		Generation:     1,
		Receiver:       "sre",
		GroupLabels:    map[string]string{"alertname": "NodeDown"},
		Title:          "NodeDown",
		State:          domain.StateOpen,
		StateVersion:   1,
		FirstSeenAt:    at,
		LastActivityAt: at,
	})
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}

	h := &joinHarness{
		ctx:      context.Background(),
		scope:    scope,
		at:       at,
		groupID:  g.ID(),
		groups:   &fakeGroups{group: g},
		members:  &fakeMembers{},
		events:   &fakeEvents{byType: map[kernel.EventType]int{}},
		stream:   &fakeStream{},
		settings: &fakeSettings{policy: domain.DefaultLifecyclePolicy()},
		enqueuer: &fakeEnqueuer{},
		tx:       &fakeTx{},
	}
	svc, err := New(Deps{
		Groups:   h.groups,
		Members:  h.members,
		Tx:       h.tx,
		Events:   h.events,
		Stream:   h.stream,
		Settings: h.settings,
		Clock:    clock.NewFake(at),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.svc = svc
	return h
}

// fakeTx runs the unit of work inline. There is no database here on purpose: the
// assertion is about how many times the service ASKS for work, and a real
// Postgres would answer that question with a stopwatch instead of a number.
type fakeTx struct{ calls int }

func (f *fakeTx) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	f.calls++
	return fn(ctx)
}

// fakeGroups is `alert_groups`, counting reads and compare-and-set writes.
type fakeGroups struct {
	group      domain.Group
	reads      int
	setRollups int
	// candidates is what CloseCandidates hands back, and closes counts the
	// generations actually closed. Both exist for closeidle_test.go: a sweep that
	// is REFUSED must be distinguishable from a sweep that found nothing to do,
	// and only the second number tells them apart.
	candidates []domain.Group
	closes     int
}

func (f *fakeGroups) GetByID(context.Context, db.TenantScope, uuid.UUID) (domain.Group, error) {
	f.reads++
	return f.group, nil
}

func (f *fakeGroups) SetRollup(_ context.Context, _ db.TenantScope, g domain.Group, _ int) error {
	f.setRollups++
	f.group = g
	return nil
}

func (f *fakeGroups) GetOpenByKey(context.Context, db.TenantScope, string) (domain.Group, bool, error) {
	return f.group, true, nil
}

func (f *fakeGroups) OpenGeneration(context.Context, db.TenantScope, repository.NewGeneration) (domain.Group, error) {
	return f.group, nil
}

func (f *fakeGroups) Close(context.Context, db.TenantScope, domain.Group, int) error {
	f.closes++
	return nil
}

func (f *fakeGroups) Touch(context.Context, db.TenantScope, uuid.UUID, time.Time) error { return nil }

func (f *fakeGroups) SetNotificationReason(context.Context, db.TenantScope, uuid.UUID, string) error {
	return nil
}

func (f *fakeGroups) StateVersion(context.Context, db.TenantScope, uuid.UUID) (int, error) {
	return f.group.StateVersion(), nil
}

func (f *fakeGroups) List(
	context.Context, db.TenantScope, domain.GroupFilter, string, db.Keyset,
) ([]domain.Group, db.Cursor, error) {
	return nil, db.Cursor{}, nil
}

func (f *fakeGroups) CloseCandidates(
	context.Context, db.TenantScope, time.Time, int,
) ([]domain.Group, error) {
	return f.candidates, nil
}

// fakeMembers is the membership read model over `alert_cases`. Rollup is
// the expensive aggregate the issue is about, so it is counted.
//
// ⛔ IT HAS NO `Join` AND NO `Leave`, because the port has none: membership is
// written by `alerts` when an episode opens, and it is not this service's to
// record. `total` is what the generation's membership settled at.
type fakeMembers struct {
	total int
	// severity is what the rollup reports. It is settable because a rollup that
	// MOVES the severity is a material change, and a material change bumps
	// `last_activity_at` — which silently makes a generation un-closable for
	// another `group_close_delay`. closeidle_test.go needs a rollup that changes
	// nothing in order to reach the close at all.
	severity string
	rollups  int
}

func (f *fakeMembers) Rollup(
	context.Context, db.TenantScope, uuid.UUID,
) (domain.Counts, string, error) {
	f.rollups++
	sev := f.severity
	if sev == "" && f.total > 0 {
		sev = "critical"
	}
	return domain.Counts{Firing: f.total, Total: f.total}, sev, nil
}

func (f *fakeMembers) MembersAt(
	context.Context, db.TenantScope, uuid.UUID, time.Time,
) ([]domain.Member, error) {
	return nil, nil
}

func (f *fakeMembers) ListCurrentMembers(
	context.Context, db.TenantScope, uuid.UUID, db.Keyset,
) ([]domain.Member, db.Cursor, error) {
	return nil, db.Cursor{}, nil
}

func (f *fakeMembers) AllMembers(context.Context, db.TenantScope, uuid.UUID) ([]domain.Member, error) {
	return nil, nil
}

func (f *fakeMembers) GroupsForAlert(
	context.Context, db.TenantScope, uuid.UUID, int,
) ([]domain.Member, error) {
	return nil, nil
}

func (f *fakeMembers) SnoozeRollup(
	context.Context, db.TenantScope, []uuid.UUID, time.Time,
) (map[uuid.UUID]domain.SnoozeRollup, error) {
	return nil, nil
}

func (f *fakeMembers) CurrentMemberAlerts(
	context.Context, db.TenantScope, uuid.UUID, int,
) ([]repository.MemberAlert, error) {
	return nil, nil
}

func (f *fakeMembers) CountCurrentMembers(
	context.Context, db.TenantScope, uuid.UUID,
) (int, error) {
	return 0, nil
}

// fakeEvents counts the append-only timeline by event type.
type fakeEvents struct{ byType map[kernel.EventType]int }

func (f *fakeEvents) AppendTimelineEvent(
	_ context.Context, _ db.TenantScope, in alerts.TimelineEventRequest,
) error {
	f.byType[in.Type]++
	return nil
}

type fakeStream struct{ appends int }

func (f *fakeStream) Append(context.Context, db.TenantScope, string, uuid.UUID, []byte) error {
	f.appends++
	return nil
}

type fakeSettings struct {
	policy domain.LifecyclePolicy
	reads  int
}

func (f *fakeSettings) GroupLifecycle(
	context.Context, db.TenantScope,
) (domain.LifecyclePolicy, error) {
	f.reads++
	return f.policy, nil
}

type fakeEnqueuer struct{ enqueued int }

func (f *fakeEnqueuer) Enqueue(
	context.Context, db.JobArgs, ...db.JobOption,
) (db.EnqueueResult, error) {
	f.enqueued++
	return db.EnqueueResult{}, nil
}

func (f *fakeEnqueuer) EnqueueMany(
	_ context.Context, reqs []db.JobRequest,
) ([]db.EnqueueResult, error) {
	f.enqueued += len(reqs)
	return make([]db.EnqueueResult, len(reqs)), nil
}
