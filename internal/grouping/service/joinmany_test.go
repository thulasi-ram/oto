package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	alerts "github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/grouping/domain"
	"github.com/thulasiram/oto/internal/grouping/repository"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
)

// A storm is exactly when the rollup must stop being O(alerts).
//
// A 500-alert Alertmanager batch lands as ONE notification group, so it opens 500
// occurrences that all join ONE generation. Re-deriving that generation once per
// member means 500 full aggregates over `alert_group_members` and 500
// compare-and-set writes to a single `alert_groups` row — writes the CAS then
// serialises, making ingestion slowest exactly while Alertmanager is retrying
// hardest and its ~5-minute budget (ADR 0007) is running out. 499 of the 500
// results are discarded, because a rollup is a pure projection of the settled
// membership.
//
// This test pins the shape rather than the timing: the work is counted through
// fake ports, so it needs no database and cannot go green because a machine was
// fast.
func TestJoinManyRollsUpOncePerGroupNotOncePerAlert(t *testing.T) {
	t.Parallel()

	const batch = 500

	h := newJoinHarness(t)
	members := make([]JoinMember, 0, batch)
	for range batch {
		members = append(members, JoinMember{AlertID: uuid.New(), OccurrenceID: uuid.New()})
	}

	res, err := h.svc.JoinMany(h.ctx, h.scope, h.groupID, members, h.at)
	if err != nil {
		t.Fatalf("JoinMany: %v", err)
	}

	if res.Joined != batch {
		t.Fatalf("joined = %d, want %d", res.Joined, batch)
	}
	// Every member is still recorded: batching the projection must not batch away
	// the membership or its timeline.
	if got := h.members.joins; got != batch {
		t.Errorf("members.Join calls = %d, want %d", got, batch)
	}
	if got := h.events.byType[alerts.GroupEventMemberJoined]; got != batch {
		t.Errorf("group.member_joined events = %d, want %d", got, batch)
	}

	// THE POINT: the projection is O(groups), not O(alerts).
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
	// One org settings read, not one per member.
	if got := h.settings.reads; got != 1 {
		t.Errorf("storm policy reads = %d, want 1", got)
	}
	// The UI is told once, so the SSE spine does not carry 500 frames describing
	// the same generation.
	if got := h.stream.appends; got != 1 {
		t.Errorf("group.upserted frames = %d, want 1", got)
	}
}

// Storm evaluation is a §B.6 VISIBLE state change, and it must stay exactly as
// loud as it was: one evaluation, one transition, one timeline event, one
// notification intent — no matter how many members arrived in the batch that
// triggered it. The evaluation never counted its own invocations; it counts
// DISTINCT JOINS IN A WINDOW out of the membership table, which is why moving it
// out of the per-member loop cannot change its verdict.
func TestJoinManyAnnouncesOneStormPerBatch(t *testing.T) {
	t.Parallel()

	const batch = 500

	h := newJoinHarness(t)
	// Well past DefaultStormThreshold (25) — this batch IS the storm.
	h.members.distinctJoins = batch

	members := make([]JoinMember, 0, batch)
	for range batch {
		members = append(members, JoinMember{AlertID: uuid.New(), OccurrenceID: uuid.New()})
	}

	res, err := h.svc.JoinMany(h.ctx, h.scope, h.groupID, members, h.at)
	if err != nil {
		t.Fatalf("JoinMany: %v", err)
	}

	if !res.Group.StormMode() {
		t.Fatal("the returned generation is not in storm mode")
	}
	if got := h.members.distinctJoinQueries; got != 1 {
		t.Errorf("storm evaluations = %d, want 1 per group per batch", got)
	}
	if got := h.groups.setStorms; got != 1 {
		t.Errorf("SetStorm writes = %d, want 1", got)
	}
	if got := h.events.byType[alerts.GroupEventStormStarted]; got != 1 {
		t.Errorf("group.storm_started events = %d, want exactly 1", got)
	}
	// One `notify.evaluate` job is what keeps the §H.6 latch on
	// `channels.storm_notice_at` seeing one notice, not 500.
	if got := h.enqueuer.enqueued; got != 1 {
		t.Errorf("notify.evaluate jobs = %d, want exactly 1", got)
	}
	if !res.Group.StormMode() {
		t.Error("the returned generation is not in storm mode")
	}
}

// A batch of one must still cost exactly one of everything. This is the case a
// single-member caller would hit, and it is the reason the deleted single-member
// `Join` façade is not missed: the batch form already answers it correctly.
func TestABatchOfOneCostsOneOfEverything(t *testing.T) {
	t.Parallel()

	h := newJoinHarness(t)
	res, err := h.svc.JoinMany(h.ctx, h.scope, h.groupID,
		[]JoinMember{{AlertID: uuid.New(), OccurrenceID: uuid.New()}}, h.at)
	if err != nil {
		t.Fatalf("JoinMany: %v", err)
	}
	if res.Joined != 1 {
		t.Errorf("Joined = %d, want 1 for a first join", res.Joined)
	}
	if h.members.joins != 1 || h.members.rollups != 1 || h.members.distinctJoinQueries != 1 {
		t.Errorf("joins/rollups/storm evaluations = %d/%d/%d, want 1/1/1",
			h.members.joins, h.members.rollups, h.members.distinctJoinQueries)
	}
	if res.Group.Counts().Total != 1 {
		t.Errorf("total = %d, want 1", res.Group.Counts().Total)
	}
}

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
		events:   &fakeEvents{byType: map[string]int{}},
		stream:   &fakeStream{},
		settings: &fakeSettings{policy: domain.DefaultStormPolicy()},
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
		Enqueuer: h.enqueuer,
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
	setStorms  int
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

func (f *fakeGroups) SetStorm(_ context.Context, _ db.TenantScope, g domain.Group, _ int) error {
	f.setStorms++
	f.group = g
	return nil
}

func (f *fakeGroups) GetOpenByKey(context.Context, db.TenantScope, string) (domain.Group, bool, error) {
	return f.group, true, nil
}

func (f *fakeGroups) OpenGeneration(context.Context, db.TenantScope, repository.NewGeneration) (domain.Group, error) {
	return f.group, nil
}

func (f *fakeGroups) Close(context.Context, db.TenantScope, domain.Group, int) error { return nil }

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
	return nil, nil
}

// fakeMembers is `alert_group_members`. Rollup is the expensive aggregate the
// issue is about, so it is counted; it answers out of the joins it has seen, so a
// batched projection still reports the whole batch.
type fakeMembers struct {
	joined              map[uuid.UUID]bool
	joins               int
	rollups             int
	distinctJoinQueries int
	distinctJoins       int
	lastJoinAt          time.Time
}

func (f *fakeMembers) Join(
	_ context.Context, _ db.TenantScope, _, occurrenceID, _ uuid.UUID, at time.Time,
) (bool, error) {
	f.joins++
	if f.joined == nil {
		f.joined = map[uuid.UUID]bool{}
	}
	if f.joined[occurrenceID] {
		return false, nil
	}
	f.joined[occurrenceID] = true
	f.lastJoinAt = at
	return true, nil
}

func (f *fakeMembers) Rollup(
	context.Context, db.TenantScope, uuid.UUID,
) (domain.Counts, string, error) {
	f.rollups++
	n := len(f.joined)
	return domain.Counts{Firing: n, Total: n}, "critical", nil
}

func (f *fakeMembers) DistinctJoinsSince(
	context.Context, db.TenantScope, uuid.UUID, time.Time,
) (int, time.Time, error) {
	f.distinctJoinQueries++
	n := f.distinctJoins
	if n == 0 {
		n = len(f.joined)
	}
	return n, f.lastJoinAt, nil
}

func (f *fakeMembers) Leave(context.Context, db.TenantScope, uuid.UUID, uuid.UUID, time.Time) (bool, error) {
	return false, nil
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
	context.Context, db.TenantScope, uuid.UUID,
) ([]repository.MemberAlert, error) {
	return nil, nil
}

// fakeEvents counts the append-only timeline by event type.
type fakeEvents struct{ byType map[string]int }

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
	policy domain.StormPolicy
	reads  int
}

func (f *fakeSettings) Storm(context.Context, db.TenantScope) (domain.StormPolicy, error) {
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
