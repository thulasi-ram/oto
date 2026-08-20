package service_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/enrichment/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
)

// baseTime is the instant every fake clock in this package is pinned at. It is
// fixed and far from any boundary so a test that advances the clock can state
// its expectations as arithmetic rather than as "roughly now".
var baseTime = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

// ------------------------------------------------------------------ enrichers

// stubEnricher is one registered Enricher whose every behaviour the test owns.
//
// It implements service.CacheSeeder unconditionally and returns `seed`, which
// is "" by default — the pipeline reads an empty seed as "not cacheable", so a
// stub is uncached unless a test says otherwise.
type stubEnricher struct {
	name    string
	version int
	phase   domain.Phase
	timeout time.Duration
	seed    string
	// notApplicable makes Applicable answer false, which is the only path to a
	// recorded `skipped` that never calls Enrich.
	notApplicable bool
	fn            func(ctx context.Context, s *domain.Subject) (domain.Result, error)

	mu       sync.Mutex
	calls    int
	sawPrior map[string]domain.Result
}

var (
	_ domain.Enricher       = (*stubEnricher)(nil)
	_ service.CacheSeeder   = (*stubEnricher)(nil)
	_ domain.Enricher       = (*plainEnricher)(nil)
	_ service.SubjectLoader = (*fakeSubjects)(nil)
)

func (e *stubEnricher) Name() string { return e.name }

func (e *stubEnricher) Version() int {
	if e.version == 0 {
		return 1
	}
	return e.version
}

func (e *stubEnricher) Phase() domain.Phase {
	if e.phase == 0 {
		return domain.PhaseInline
	}
	return e.phase
}

func (e *stubEnricher) Timeout() time.Duration { return e.timeout }

func (e *stubEnricher) Applicable(*domain.Subject) bool { return !e.notApplicable }

func (e *stubEnricher) CacheSeed(*domain.Subject) string { return e.seed }

func (e *stubEnricher) Enrich(ctx context.Context, s *domain.Subject) (domain.Result, error) {
	e.mu.Lock()
	e.calls++
	if s != nil {
		e.sawPrior = s.Prior
	}
	e.mu.Unlock()

	if e.fn != nil {
		return e.fn(ctx, s)
	}
	return domain.Result{Status: domain.StatusOK, Payload: map[string]any{"who": e.name}}, nil
}

func (e *stubEnricher) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func (e *stubEnricher) prior() map[string]domain.Result {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sawPrior
}

// plainEnricher is an Enricher that does NOT implement CacheSeeder, so the
// pipeline must never consult the shared cache for it.
type plainEnricher struct{ name string }

func (e plainEnricher) Name() string                  { return e.name }
func (plainEnricher) Version() int                    { return 1 }
func (plainEnricher) Phase() domain.Phase             { return domain.PhaseInline }
func (plainEnricher) Timeout() time.Duration          { return 0 }
func (plainEnricher) Applicable(*domain.Subject) bool { return true }
func (plainEnricher) Enrich(context.Context, *domain.Subject) (domain.Result, error) {
	return domain.Result{Status: domain.StatusOK, Payload: map[string]any{}}, nil
}

// blocking returns an Enrich body that waits for the context and reports why it
// stopped. It never sleeps: it returns the instant the budget is withdrawn.
func blocking() func(context.Context, *domain.Subject) (domain.Result, error) {
	return func(ctx context.Context, _ *domain.Subject) (domain.Result, error) {
		<-ctx.Done()
		return domain.Result{}, ctx.Err()
	}
}

// failing returns an Enrich body that reports an upstream failure.
func failing(msg string) func(context.Context, *domain.Subject) (domain.Result, error) {
	return func(context.Context, *domain.Subject) (domain.Result, error) {
		return domain.Result{}, stubErr(msg)
	}
}

// panicking returns an Enrich body that is a bug in a third-party-shaped
// component.
func panicking(msg string) func(context.Context, *domain.Subject) (domain.Result, error) {
	return func(context.Context, *domain.Subject) (domain.Result, error) {
		panic(msg)
	}
}

// stubErr is a plain sentinel-shaped error a test can compare by value.
type stubErr string

func (e stubErr) Error() string { return string(e) }

// ---------------------------------------------------------------- repository

type fakeRepo struct {
	mu        sync.Mutex
	existing  []domain.Enrichment
	listErr   error
	upsertErr error
	upserts   [][]domain.Enrichment
}

func (r *fakeRepo) ListBySubject(
	context.Context, db.TenantScope, string, string,
) ([]domain.Enrichment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	return append([]domain.Enrichment(nil), r.existing...), nil
}

func (r *fakeRepo) UpsertMany(_ context.Context, _ db.TenantScope, in []domain.Enrichment) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.upsertErr != nil {
		return r.upsertErr
	}
	r.upserts = append(r.upserts, append([]domain.Enrichment(nil), in...))
	return nil
}

// stored is every enrichment handed to UpsertMany, flattened.
func (r *fakeRepo) stored() []domain.Enrichment {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Enrichment
	for _, batch := range r.upserts {
		out = append(out, batch...)
	}
	return out
}

func (r *fakeRepo) writes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.upserts)
}

// --------------------------------------------------------------------- cache

type fakeCache struct {
	mu        sync.Mutex
	entries   map[string]domain.CacheEntry
	getErr    error
	putErr    error
	deleteErr error
	deleted   int64

	gets       int
	puts       int
	lastBefore time.Time
	lastLimit  int
}

func newFakeCache() *fakeCache {
	return &fakeCache{entries: map[string]domain.CacheEntry{}}
}

func (c *fakeCache) Get(_ context.Context, _ db.TenantScope, key string) (domain.CacheEntry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	if c.getErr != nil {
		return domain.CacheEntry{}, false, c.getErr
	}
	e, ok := c.entries[key]
	return e, ok, nil
}

func (c *fakeCache) Put(_ context.Context, _ db.TenantScope, e domain.CacheEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts++
	if c.putErr != nil {
		return c.putErr
	}
	c.entries[e.Key()] = e
	return nil
}

func (c *fakeCache) DeleteExpired(_ context.Context, before time.Time, limit int) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastBefore, c.lastLimit = before, limit
	if c.deleteErr != nil {
		return 0, c.deleteErr
	}
	return c.deleted, nil
}

func (c *fakeCache) counts() (gets, puts int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets, c.puts
}

// keys returns the cache keys written, for a test that asserts the derivation
// is the domain's and not the enricher's.
func (c *fakeCache) keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.entries))
	for k := range c.entries {
		out = append(out, k)
	}
	return out
}

// ------------------------------------------------------------------ subjects

type fakeSubjects struct {
	loaded service.Loaded
	err    error
	mu     sync.Mutex
}

func (l *fakeSubjects) LoadSubject(
	_ context.Context, _ db.TenantScope, _ uuid.UUID,
) (service.Loaded, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return service.Loaded{}, l.err
	}
	return l.loaded, nil
}

// ------------------------------------------------------------------ notifier

type fakeNotifier struct {
	mu          sync.Mutex
	enrichedErr error
	releaseErr  error
	enriched    []service.EnrichedNotice
	released    []service.PreNotificationNotice
}

func (n *fakeNotifier) NotifyEnriched(_ context.Context, _ db.TenantScope, notice service.EnrichedNotice) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.enriched = append(n.enriched, notice)
	return n.enrichedErr
}

func (n *fakeNotifier) NotifyPreNotificationReady(
	_ context.Context, _ db.TenantScope, notice service.PreNotificationNotice,
) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.released = append(n.released, notice)
	return n.releaseErr
}

func (n *fakeNotifier) enrichedCalls() []service.EnrichedNotice {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]service.EnrichedNotice(nil), n.enriched...)
}

func (n *fakeNotifier) releaseCalls() []service.PreNotificationNotice {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]service.PreNotificationNotice(nil), n.released...)
}

// -------------------------------------------------------------------- events

type fakeEvents struct {
	mu     sync.Mutex
	err    error
	events []service.EnrichmentEvent
}

func (e *fakeEvents) RecordEnrichmentEvent(
	_ context.Context, _ db.TenantScope, ev service.EnrichmentEvent,
) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
	return e.err
}

func (e *fakeEvents) recorded() []service.EnrichmentEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]service.EnrichmentEvent(nil), e.events...)
}

// ------------------------------------------------------------------ enqueuer

type fakeEnqueuer struct {
	mu   sync.Mutex
	err  error
	jobs []db.JobArgs
}

func (q *fakeEnqueuer) Enqueue(
	_ context.Context, args db.JobArgs, _ ...db.JobOption,
) (db.EnqueueResult, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return db.EnqueueResult{}, q.err
	}
	q.jobs = append(q.jobs, args)
	return db.EnqueueResult{ID: int64(len(q.jobs)), Kind: args.Kind()}, nil
}

func (q *fakeEnqueuer) EnqueueMany(
	_ context.Context, reqs []db.JobRequest,
) ([]db.EnqueueResult, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return nil, q.err
	}
	out := make([]db.EnqueueResult, 0, len(reqs))
	for _, r := range reqs {
		q.jobs = append(q.jobs, r.Args)
		out = append(out, db.EnqueueResult{ID: int64(len(q.jobs)), Kind: r.Args.Kind()})
	}
	return out, nil
}

func (q *fakeEnqueuer) enqueued() []db.JobArgs {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]db.JobArgs(nil), q.jobs...)
}

// ------------------------------------------------------------------- harness

// env is one wired pipeline plus every double behind it.
type env struct {
	svc      *service.Service
	repo     *fakeRepo
	cache    *fakeCache
	subjects *fakeSubjects
	notifier *fakeNotifier
	events   *fakeEvents
	enqueuer *fakeEnqueuer
	clk      *clock.Fake
	scope    db.TenantScope

	caseID  uuid.UUID
	alertID uuid.UUID
	orgID   uuid.UUID
}

// newEnv builds a Service over the given enrichers with every optional port
// wired, so a test only has to say which one it is about.
func newEnv(t *testing.T, tune func(*service.Options), enrichers ...domain.Enricher) *env {
	t.Helper()

	reg, err := service.NewRegistry(enrichers...)
	require.NoError(t, err, "the registry must accept the test's enrichers")

	e := &env{
		repo:     &fakeRepo{},
		cache:    newFakeCache(),
		notifier: &fakeNotifier{},
		events:   &fakeEvents{},
		enqueuer: &fakeEnqueuer{},
		clk:      clock.NewFake(baseTime),
		caseID:   id.New(),
		alertID:  id.New(),
		orgID:    id.New(),
	}
	e.scope, err = db.NewTenantScope(e.orgID)
	require.NoError(t, err)

	e.subjects = &fakeSubjects{loaded: service.Loaded{
		Subject: domain.Subject{
			OrgID:       e.orgID.String(),
			SubjectKind: domain.SubjectCase,
			SubjectID:   e.caseID.String(),
			Alert: domain.AlertSnapshot{
				ID:        e.alertID.String(),
				AlertName: "HighErrorRate",
				Severity:  "critical",
			},
			Case: domain.CaseSnapshot{
				ID:        e.caseID.String(),
				Seq:       1,
				State:     "firing",
				StartedAt: baseTime,
			},
		},
		AlertID:      e.alertID,
		CaseID:       e.caseID,
		StateVersion: 7,
	}}

	opts := service.Options{
		Registry: reg,
		Repo:     e.repo,
		Cache:    e.cache,
		Subjects: e.subjects,
		Notifier: e.notifier,
		Events:   e.events,
		Enqueuer: e.enqueuer,
		Clock:    e.clk,
		Logger:   slog.New(slog.DiscardHandler),
	}
	if tune != nil {
		tune(&opts)
	}

	e.svc, err = service.New(opts)
	require.NoError(t, err)
	return e
}

// run executes one phase over the whole registry.
func (e *env) run(t *testing.T, phase domain.Phase, names ...string) service.RunResult {
	t.Helper()
	out, err := e.svc.Run(context.Background(), e.scope, service.RunRequest{
		CaseID:    e.caseID,
		Phase:     phase,
		Enrichers: names,
	})
	require.NoError(t, err, "an enrichment failure is never a run failure")
	return out
}

// byName indexes a run's results.
func byName(results []domain.Enrichment) map[string]domain.Enrichment {
	out := make(map[string]domain.Enrichment, len(results))
	for _, r := range results {
		out[r.Enricher()] = r
	}
	return out
}

// priorResult builds a row the repository already holds for this subject. It
// goes through the constructor because there is no other way in: a result the
// table would refuse is not representable, so a fixture cannot seed one either.
func (e *env) priorResult(t *testing.T, with func(p *domain.EnrichmentParams)) domain.Enrichment {
	t.Helper()

	p := domain.EnrichmentParams{
		ID:          id.NewString(),
		OrgID:       e.orgID.String(),
		SubjectKind: domain.SubjectCase,
		SubjectID:   e.caseID.String(),
		Enricher:    "test.alpha",
		Version:     1,
		Phase:       domain.PhaseInline,
		Status:      domain.StatusOK,
		// Computed an hour before baseTime, so a fixture may state any expiry it
		// likes around baseTime without tripping enrichments_exp_ck.
		ComputedAt: baseTime.Add(-time.Hour),
	}
	if with != nil {
		with(&p)
	}
	out, err := domain.NewEnrichment(p)
	require.NoError(t, err)
	return out
}
