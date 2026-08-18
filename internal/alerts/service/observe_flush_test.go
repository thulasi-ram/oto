package service

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/alerts/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/test/harness"
)

// These pin the batched write half of `observe` (§G.4): the reads were collapsed
// to one round trip each long ago, and these are the tests that stop the writes
// from quietly fanning back out to one statement per observation — the shape
// that turns a 500-alert storm chunk into thousands of sequential statements
// against a pool whose statement timeout is 2s.

// countingStream records every frame the service hands the SSE spine and HOW —
// per item or batched. It is the counting-port pattern the sweep tests use: what
// a real Postgres cannot say is how many round trips a batch cost.
type countingStream struct {
	appendCalls int
	batchCalls  int
	frames      []StreamFrame
}

func (c *countingStream) Append(
	_ context.Context, _ db.TenantScope, kind string, resourceID uuid.UUID, payload []byte,
) error {
	c.appendCalls++
	c.frames = append(c.frames, StreamFrame{Kind: kind, ResourceID: resourceID, Payload: payload})
	return nil
}

func (c *countingStream) AppendBatch(_ context.Context, _ db.TenantScope, frames []StreamFrame) error {
	c.batchCalls++
	c.frames = append(c.frames, frames...)
	return nil
}

func (c *countingStream) kinds() []string {
	out := make([]string, 0, len(c.frames))
	for _, f := range c.frames {
		out = append(out, f.Kind)
	}
	return out
}

// streamedService is the fixture's service with the SSE port wired, which
// newService deliberately leaves nil.
func (f *fixture) streamedService(stream StreamAppender) *Service {
	f.t.Helper()
	svc, err := New(Deps{
		Alerts:     repository.NewAlertRepository(f.pool, f.clk, false),
		Cases:      repository.NewCaseRepository(f.pool),
		Events:     repository.NewEventRepository(f.pool, f.clk),
		Snoozes:    repository.NewSnoozeRepository(f.pool, f.clk),
		Tx:         repository.NewTxRunner(f.pool),
		AlertBatch: repository.NewAlertRepository(f.pool, f.clk, false),
		OccBatch:   repository.NewCaseRepository(f.pool),
		Stream:     stream,
		Clock:      f.clk,
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		f.t.Fatalf("build service: %v", err)
	}
	return svc
}

// observationFor is fixture.observation for an arbitrary label set, so one batch
// can carry more than one alert.
func (f *fixture) observationFor(
	labels domain.LabelSet, status string, at, startsAt, endsAt time.Time,
) domain.Observation {
	f.t.Helper()
	return domain.Observation{
		Source:            domain.ObservedByIngest,
		ClusterID:         f.clusterID,
		ClusterKey:        f.clusterKey,
		AlertKey:          harness.AlertKey(f.orgID, f.clusterKey, labels),
		SourceFingerprint: domain.ComputeSourceFingerprint(labels),
		Labels:            labels,
		Annotations:       map[string]string{},
		Status:            status,
		SourceStartsAt:    startsAt,
		SourceEndsAt:      endsAt,
		SourceUpdatedAt:   at,
		ObservedAt:        at,
	}
}

// TestObserveBatchPublishesEveryFrameInOneRoundTrip is the flush-count claim:
// a batch of N observations queues its frames and hands them to the spine ONCE,
// through AppendBatch, never through per-item Append — and in the order the
// per-item appends used to produce: case and alert frames per observation
// in batch order first, event frames after.
func TestObserveBatchPublishesEveryFrameInOneRoundTrip(t *testing.T) {
	now := harness.Epoch
	f := newFixture(t, now)
	stream := &countingStream{}
	svc := f.streamedService(stream)

	second := harness.DefaultLabels()
	second["alertname"] = "SecondAlert"
	obs := []domain.Observation{
		f.observation(domain.ObservedByIngest, "firing", now, now.Add(-time.Minute), time.Time{}),
		f.observationFor(harness.Labels(t, second), "firing", now, now.Add(-time.Minute), time.Time{}),
	}

	res, err := svc.ObserveBatch(t.Context(), f.scope, obs, ObserveOptions{})
	require.NoError(t, err)
	require.Len(t, res.Outcomes, 2)

	assert.Equal(t, 1, stream.batchCalls, "one batch, one flush")
	assert.Zero(t, stream.appendCalls, "the observe path must not publish per item")

	// Two first sightings: each queues its case and alert frames in the
	// loop, and each appends `alert.created` + `case.opened`, whose frames
	// follow the loop's — the same order N sequential appends produced.
	assert.Equal(t, []string{
		StreamCaseUpserted, StreamAlertUpserted,
		StreamCaseUpserted, StreamAlertUpserted,
		StreamEventAppended, StreamEventAppended,
		StreamEventAppended, StreamEventAppended,
	}, stream.kinds())
	assert.Equal(t, res.Outcomes[0].CaseID, stream.frames[0].ResourceID)
	assert.Equal(t, res.Outcomes[0].AlertID, stream.frames[1].ResourceID)
	assert.Equal(t, res.Outcomes[1].CaseID, stream.frames[2].ResourceID)
	assert.Equal(t, res.Outcomes[1].AlertID, stream.frames[3].ResourceID)
	assert.Equal(t, 4, res.EventsWritten)
}

// TestObserveBatchProjectsAnAlertOnceWithTheLastWrite is the collapse claim: M
// observations of ONE alert in a batch stage M projections and flush the LAST —
// and the row a fire-then-resolve batch leaves behind is exactly the row the M
// sequential SetProjection calls left: the resolved state, no current episode,
// and the episode counted once.
func TestObserveBatchProjectsAnAlertOnceWithTheLastWrite(t *testing.T) {
	now := harness.Epoch
	f := newFixture(t, now)

	startsAt := now.Add(-10 * time.Minute)
	obs := []domain.Observation{
		f.observation(domain.ObservedByIngest, "firing", now.Add(-time.Minute), startsAt, time.Time{}),
		f.observation(domain.ObservedByIngest, "resolved", now, startsAt, now),
	}
	res, err := f.svc.ObserveBatch(t.Context(), f.scope, obs, ObserveOptions{})
	require.NoError(t, err)
	require.Len(t, res.Outcomes, 2)
	require.Equal(t, domain.TransitionT1.String(), res.Outcomes[0].Transition)
	require.Equal(t, domain.TransitionT5.String(), res.Outcomes[1].Transition)

	var (
		state       string
		currentCase *uuid.UUID
		lastSeen    time.Time
		total       int
	)
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT state, current_case_id, last_seen_at, total_cases
		  FROM alerts WHERE org_id = $1 AND alert_key = $2`,
		f.orgID, f.alertKey.String()).Scan(&state, &currentCase, &lastSeen, &total))

	assert.Equal(t, domain.StateResolved.String(), state, "the LAST observation's verdict")
	assert.Nil(t, currentCase, "a resolved episode is nobody's current case")
	assert.Equal(t, 1, total, "one episode opened, counted once")
	assert.True(t, lastSeen.Equal(now), "last_seen_at is the newest recorded_at, got %v", lastSeen)
}

// TestStageProjectionKeepsTheNewestLastSeen isolates the one field the collapse
// may not simply overwrite. SetProjection's SQL folds every sequential write
// through GREATEST(last_seen_at, $7), so M writes left max(all M) behind; a
// collapse keeping only the last observation's clock reading would let an
// out-of-order pair inside one batch rewind it.
func TestStageProjectionKeepsTheNewestLastSeen(t *testing.T) {
	alertID := uuid.New()
	newer := harness.Epoch
	older := newer.Add(-time.Minute)

	acc := &observeAccum{}
	acc.stageProjection(alertID, domain.AlertProjection{
		State: domain.StateFiring, LastSeenAt: newer, LastStateChangeAt: newer,
		TotalCases: 1,
	})
	acc.stageProjection(alertID, domain.AlertProjection{
		State: domain.StateResolved, LastSeenAt: older, LastStateChangeAt: older,
		TotalCases: 1,
	})

	require.Len(t, acc.projectionOrder, 1, "M stages of one alert flush once")
	got := acc.projections[alertID]
	assert.Equal(t, domain.StateResolved, got.State, "every other field is the last write")
	assert.Equal(t, older, got.LastStateChangeAt)
	assert.Equal(t, newer, got.LastSeenAt, "last_seen_at keeps the maximum staged")
}
