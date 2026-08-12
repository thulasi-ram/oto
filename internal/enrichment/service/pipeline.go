package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// Error codes this service mints.
const (
	// CodeMissingRegistry means the service was built with no enrichers.
	CodeMissingRegistry = "enrichment_missing_registry"
	// CodeMissingRepository means the service was built without storage.
	CodeMissingRepository = "enrichment_missing_repository"
	// CodeMissingSubjects means the service was built without a subject loader.
	CodeMissingSubjects = "enrichment_missing_subject_loader"
	// CodeNoOccurrence means the run named no occurrence.
	CodeNoOccurrence = "enrichment_no_occurrence"
)

// The alert timeline types this service appends (SPEC §D.4.1, transition T11).
const (
	// EventCompleted records that a phase produced results.
	EventCompleted = "enrichment.completed"
	// EventFailed records that a phase produced none.
	EventFailed = "enrichment.failed"
)

// Options are the Service's dependencies. Everything is a port, so the whole
// pipeline runs against fakes with no Postgres, no Prometheus and no clock.
type Options struct {
	Registry *Registry
	Repo     EnrichmentRepository
	Cache    CacheRepository
	Subjects SubjectLoader
	Notifier Notifier
	Events   EventRecorder
	// Enqueuer re-enqueues inline stragglers as an async pass. Without one, a
	// timed-out enricher is still RECORDED as timed out; it is simply not
	// retried, and the run says so.
	Enqueuer db.Enqueuer
	Clock    clock.Clock
	Logger   *slog.Logger
	// InlineBudget and AsyncBudget default to the SPEC §F.3 values.
	InlineBudget time.Duration
	AsyncBudget  time.Duration
}

// Service is the budgeted, two-phase enrichment pipeline.
//
// The design has exactly one hard commitment, and everything else follows from
// it: ENRICHMENT MUST NEVER DELAY OR FAIL A NOTIFICATION. An alert that fired is
// already real and already worth telling someone about; context is a bonus. So
// the inline phase runs under a ceiling rather than a wait, a failing enricher
// degrades to a recorded failure rather than an error, and the slow phase
// amends what was already said instead of holding it back.
type Service struct {
	reg      *Registry
	repo     EnrichmentRepository
	cache    CacheRepository
	subjects SubjectLoader
	notifier Notifier
	events   EventRecorder
	enqueuer db.Enqueuer
	clk      clock.Clock
	log      *slog.Logger

	inlineBudget time.Duration
	asyncBudget  time.Duration
}

// New builds the Service.
func New(o Options) (*Service, error) {
	switch {
	case o.Registry == nil:
		return nil, errs.New(errs.KindInternal, CodeMissingRegistry, "an enricher registry is required")
	case o.Repo == nil:
		return nil, errs.New(errs.KindInternal, CodeMissingRepository, "an enrichment repository is required")
	case o.Subjects == nil:
		return nil, errs.New(errs.KindInternal, CodeMissingSubjects, "a subject loader is required")
	}

	s := &Service{
		reg: o.Registry, repo: o.Repo, cache: o.Cache, subjects: o.Subjects,
		notifier: o.Notifier, events: o.Events, enqueuer: o.Enqueuer,
		clk: o.Clock, log: o.Logger,
		inlineBudget: o.InlineBudget, asyncBudget: o.AsyncBudget,
	}
	if s.clk == nil {
		s.clk = clock.New()
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.inlineBudget <= 0 {
		s.inlineBudget = domain.InlineBudget
	}
	if s.asyncBudget <= 0 {
		s.asyncBudget = domain.AsyncBudget
	}
	return s, nil
}

// Registry exposes the wired enrichers, for the settings screen and for the
// health endpoint that reports phase, version and hit rate (SPEC §E).
func (s *Service) Registry() *Registry { return s.reg }

// RunRequest is one pass of the pipeline over one occurrence.
type RunRequest struct {
	// OccurrenceID is the firing episode being enriched. Required.
	OccurrenceID uuid.UUID
	// Phase selects the pass: inline (pre-notification) or async (after).
	Phase domain.Phase
	// Enrichers narrows the run to named enrichers. Empty means every enabled
	// enricher declaring this phase.
	Enrichers []string
}

// RunResult is what one pass did. It is returned for the worker to log and for
// tests to assert on; nothing downstream depends on it.
type RunResult struct {
	Phase   domain.Phase
	Subject domain.Subject
	// Results are the stored enrichments, in deterministic order.
	Results []domain.Enrichment
	// Skipped names enrichers whose stored result was still reusable, so they
	// were not run again. A retry of a phase is cheap by construction.
	Skipped []string
	// Deferred names inline enrichers that ran out of budget and were
	// re-enqueued as an async pass.
	Deferred []string
	// Notified reports that the one coalesced `enriched` notification was
	// requested. It is true at most once per async pass.
	Notified bool
	// Released reports that the deferred `fired` evaluation was let go early
	// because this inline pass finished inside the pre-notification budget.
	Released bool
	Duration time.Duration
}

// Succeeded counts the results that produced usable content.
func (r RunResult) Succeeded() int {
	n := 0
	for _, e := range r.Results {
		if e.Status().Succeeded() {
			n++
		}
	}
	return n
}

// Run executes one phase of the pipeline for one occurrence.
//
// It returns an error ONLY for the things that make the run meaningless: an
// unloadable subject, a storage failure, and a result the domain refuses to
// build — the last being a bug in this package rather than a degraded upstream.
// An enricher that panics, times out, returns garbage or cannot reach its
// upstream produces a RECORDED result with the reason attached, and the phase
// carries on. That asymmetry is the design: partial enrichment is the expected
// steady state, not an exception path.
func (s *Service) Run(ctx context.Context, scope db.TenantScope, req RunRequest) (RunResult, error) {
	started := s.clk.Now()

	if req.OccurrenceID == uuid.Nil {
		return RunResult{}, errs.New(errs.KindValidation, CodeNoOccurrence,
			"an enrichment run must name an occurrence")
	}
	phase := req.Phase
	if !phase.Valid() {
		phase = domain.PhaseInline
	}

	loaded, err := s.subjects.LoadSubject(ctx, scope, req.OccurrenceID)
	if err != nil {
		return RunResult{}, err
	}
	subject := loaded.Subject
	if subject.SubjectKind == "" {
		subject.SubjectKind = domain.SubjectOccurrence
	}
	if subject.SubjectID == "" {
		subject.SubjectID = req.OccurrenceID.String()
	}
	if subject.OrgID == "" {
		subject.OrgID = scope.OrgID().String()
	}

	// Everything already known about this subject serves two purposes: it is
	// how a re-run skips work it has already paid for, and it is the Prior map
	// an async enricher reads to see what the inline pass found.
	existing, err := s.repo.ListBySubject(ctx, scope, subject.SubjectKind, subject.SubjectID)
	if err != nil {
		return RunResult{}, err
	}
	byName := make(map[string]domain.Enrichment, len(existing))
	prior := make(map[string]domain.Result, len(existing))
	for _, e := range existing {
		byName[e.Enricher()] = e
		prior[e.Enricher()] = domain.Result{
			Status:   e.Status(),
			Payload:  e.Payload(),
			Warnings: e.Warnings(),
		}
	}
	subject.Prior = prior

	selected := s.reg.Select(phase, req.Enrichers)
	out := RunResult{Phase: phase, Subject: subject}

	budget := s.inlineBudget
	if phase == domain.PhaseAsync {
		budget = s.asyncBudget
	}

	// The budget is a CEILING, never a wait (SPEC §F.3). It is applied to the
	// whole phase, and the deadline is derived once so that every enricher sees
	// the same horizon regardless of when its goroutine is scheduled.
	phaseCtx, cancel := context.WithTimeout(WithScope(ctx, scope), budget)
	defer cancel()

	type slot struct {
		enricher domain.Enricher
		result   domain.Enrichment
		ran      bool
		// buildErr is the domain refusing to build this slot's row. It is the one
		// error runOne can return, and it is kept per-slot rather than acted on in
		// the goroutine so that one enricher's bug cannot cut another's short.
		buildErr error
	}
	slots := make([]slot, 0, len(selected))
	for _, e := range selected {
		if prev, ok := byName[e.Name()]; ok && prev.Reusable(e.Version(), started) {
			out.Skipped = append(out.Skipped, e.Name())
			continue
		}
		slots = append(slots, slot{enricher: e})
	}

	var wg sync.WaitGroup
	for i := range slots {
		wg.Add(1)
		go func(sl *slot) {
			defer wg.Done()
			rec, err := s.runOne(phaseCtx, scope, sl.enricher, subject, phase, budget)
			if err != nil {
				// A row the domain refuses is recorded here and FAILS THE RUN below.
				//
				// It is logged at ERROR rather than WARN because, unlike a cache that
				// is down or a deferral that could not be enqueued, it cannot happen
				// from the outside: the registry proves the enricher's name and version
				// at boot, Run fills in the subject's coordinates, and every branch of
				// runOne produces a status the table accepts. If this fires it is a bug
				// in oto, not a bad Tuesday upstream. A recorded-failure fallback would
				// be dead code for the same reason: whatever made these params
				// unbuildable is identity, which is the same for every branch, so the
				// fallback would be refused too.
				s.log.ErrorContext(phaseCtx, "enrichment: refusing to record an invalid result",
					"enricher", sl.enricher.Name(), "error", err)
				sl.buildErr = err
				return
			}
			sl.result = rec
			sl.ran = true
		}(&slots[i])
	}
	wg.Wait()

	for i := range slots {
		if slots[i].ran {
			out.Results = append(out.Results, slots[i].result)
		}
	}
	// Registry order is already deterministic; sorting by name again makes the
	// guarantee independent of how Select was asked.
	sort.SliceStable(out.Results, func(i, j int) bool {
		return out.Results[i].Enricher() < out.Results[j].Enricher()
	})

	// A CONSTRUCTION failure is a bug in oto, and it must not be laundered into a
	// successful run. runOne turns every way an ENRICHER can misbehave into a
	// provenanced row, so the only error it returns is the domain refusing to
	// build one — which means this branch is not the "one enricher failing must
	// not fail the others" case, it is the "we would have written a row the table
	// refuses" case. Before the constructor moved in front of the write, these
	// params reached UpsertMany, which refused the WHOLE batch and failed the run,
	// so the job retried and somebody saw it. Returning the first one restores
	// exactly that signal — and returning it BEFORE the write for the same reason
	// UpsertMany refused the batch: a phase never lands half-written on a bug
	// nobody has diagnosed yet. Slot order is the registry's, so which one is
	// "first" is deterministic.
	for i := range slots {
		if slots[i].buildErr != nil {
			return out, slots[i].buildErr
		}
	}

	if len(out.Results) > 0 {
		if err := s.repo.UpsertMany(ctx, scope, out.Results); err != nil {
			return out, err
		}
	}

	if phase == domain.PhaseInline {
		out.Deferred = s.deferStragglers(ctx, req.OccurrenceID, out.Results)
		// ⭐ THE FIRST CARD WAITS FOR THIS AND NOTHING ELSE. Releasing it here —
		// after the results are stored, before anything slow — is what puts the rule
		// snapshot on the message a human actually reads instead of on a silent
		// `chat.update` some seconds later. It is released whatever the results
		// were: an inline pass that found nothing has still spent the budget, and
		// holding the card back for a rule that is not coming would be the exact
		// delay §F.3 forbids.
		out.Released = s.release(ctx, scope, loaded)
	} else {
		out.Notified = s.announce(ctx, scope, loaded, out.Results)
	}

	s.narrate(ctx, scope, loaded, phase, out.Results)
	out.Duration = s.clk.Since(started)

	s.log.InfoContext(ctx, "enrichment: phase complete",
		"occurrence_id", req.OccurrenceID,
		"phase", phase.String(),
		"ran", len(out.Results),
		"succeeded", out.Succeeded(),
		"skipped", len(out.Skipped),
		"deferred", len(out.Deferred),
		"duration_ms", out.Duration.Milliseconds())

	return out, nil
}

// runOne executes a single enricher under its own timeout, and converts every
// possible way it can misbehave into a provenanced row.
//
// No way an ENRICHER can end produces an error here. That is deliberate: the
// caller is a fan-out with a budget, and an enricher that can fail the phase is
// an enricher that can delay a notification. The one error this returns is the
// domain refusing to build the row at all — see the branches below, each of
// which completes `base` into a whole EnrichmentParams and calls NewEnrichment
// exactly once. A result is decided in one place or it is not decided at all.
func (s *Service) runOne(
	ctx context.Context,
	scope db.TenantScope,
	e domain.Enricher,
	subject domain.Subject,
	phase domain.Phase,
	budget time.Duration,
) (domain.Enrichment, error) {
	started := s.clk.Now()

	// Identity and provenance are the same however the enricher ends; only the
	// outcome differs. Each branch copies this and fills in the rest.
	base := domain.EnrichmentParams{
		ID:          id.NewString(),
		OrgID:       subject.OrgID,
		SubjectKind: subject.SubjectKind,
		SubjectID:   subject.SubjectID,
		Enricher:    e.Name(),
		Version:     e.Version(),
		Phase:       phase,
		ComputedAt:  started,
	}

	// Each enricher gets its OWN copy of the Subject. Enrichers are documented
	// as pure-ish, but "documented" is not "enforced", and a shared pointer
	// across a fan-out is a data race waiting for a busy Tuesday.
	local := subject

	if !e.Applicable(&local) {
		p := base
		p.Status = domain.StatusSkipped
		p.Payload = map[string]any{}
		p.Duration = s.clk.Since(started)
		return domain.NewEnrichment(p)
	}

	// Layer one: the shared cache. Only enrichers that can name their inputs up
	// front participate; the rest simply always compute.
	seed := ""
	if seeder, ok := e.(CacheSeeder); ok {
		seed = seeder.CacheSeed(&local)
	}
	if hit, ok := s.cacheGet(ctx, scope, e, seed); ok {
		p := base
		p.Status = domain.StatusOK
		p.Payload = hit.Payload()
		p.FromCache = true
		p.ExpiresAt = hit.ExpiresAt()
		p.Duration = s.clk.Since(started)
		return domain.NewEnrichment(p)
	}

	callCtx, cancel := context.WithTimeout(ctx, timeoutOf(e, budget))
	defer cancel()

	res, err := s.invoke(callCtx, e, &local)
	// Measured here, once, for every branch below: wall time is what feeds the
	// budget, and it is recorded whether the call succeeded or not.
	duration := s.clk.Since(started)

	// A deadline is either the enricher's own timeout or the phase budget
	// expiring underneath it; both arrive here as DeadlineExceeded, and both
	// mean the same thing to the caller — this one did not finish in time.
	timedOut := err != nil &&
		(errors.Is(err, context.DeadlineExceeded) || errors.Is(callCtx.Err(), context.DeadlineExceeded))

	switch {
	case timedOut:
		// A timeout is not a failure of the enricher, it is a failure of the
		// budget, and the two want different remedies: a failure is retried by
		// the job's own retry policy, a timeout is deferred to the slow phase.
		p := base
		p.Status = domain.StatusTimeout
		p.Error = fmt.Sprintf("exceeded its %s budget", timeoutOf(e, budget))
		p.Payload = map[string]any{}
		p.Duration = duration
		return domain.NewEnrichment(p)

	case err != nil && res.Payload != nil && res.Status == domain.StatusPartial:
		// A partial answer alongside an error is still worth keeping. The
		// enricher itself must have labelled it `partial`: a payload presented as
		// complete when the call errored would render as a fact.
		p := base
		p.Status = domain.StatusPartial
		p.Payload = res.Payload
		p.Warnings = clampWarnings(append(res.Warnings, truncate(err.Error(), 200)))
		p.Duration = duration
		return domain.NewEnrichment(p)

	case err != nil:
		p := base
		p.Status = domain.StatusFailed
		p.Error = truncate(err.Error(), 2000)
		p.Payload = map[string]any{}
		p.Duration = duration
		return domain.NewEnrichment(p)
	}

	p := base
	p.Duration = duration
	p.Status = res.Status
	if !p.Status.Valid() {
		p.Status = domain.StatusOK
	}
	p.Payload = res.Payload
	if p.Payload == nil {
		p.Payload = map[string]any{}
	}
	p.Warnings = clampWarnings(res.Warnings)
	if p.Status.NeedsError() {
		p.Error = "the enricher reported " + string(p.Status) + " without a reason"
	}

	// Layer two: write back. The enricher's own key wins over the seed, because
	// it may know more after the call than it did before.
	ttl := domain.ClampTTL(res.TTL)
	if ttl > 0 && p.Status.Succeeded() {
		p.ExpiresAt = started.Add(ttl)
		key := res.CacheKey
		if key == "" {
			key = seed
		}
		s.cachePut(ctx, scope, e, key, p.Payload, started, ttl)
	}
	return domain.NewEnrichment(p)
}

// invoke calls the enricher with a panic guard.
//
// A panicking enricher is a bug, but it is a bug in an OPTIONAL, isolated,
// third-party-shaped component, and it must not take the pipeline — or the
// notification behind it — down with it.
func (s *Service) invoke(ctx context.Context, e domain.Enricher, subject *domain.Subject) (res domain.Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			s.log.ErrorContext(ctx, "enrichment: panic in enricher",
				"enricher", e.Name(), "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			res = domain.Result{}
			err = fmt.Errorf("enricher %s panicked: %v", e.Name(), p)
		}
	}()
	return e.Enrich(ctx, subject)
}

// cacheGet reads the shared layer, treating every failure as a miss.
func (s *Service) cacheGet(ctx context.Context, scope db.TenantScope, e domain.Enricher, seed string) (domain.CacheEntry, bool) {
	if s.cache == nil || seed == "" {
		return domain.CacheEntry{}, false
	}
	key := domain.CacheKey(scope.OrgID().String(), e.Name(), e.Version(), seed)
	if key == "" || len(key) > domain.MaxCacheKeyBytes {
		return domain.CacheEntry{}, false
	}
	entry, ok, err := s.cache.Get(ctx, scope, key)
	if err != nil {
		// A cache that is down is a slow pipeline, never a broken one.
		s.log.WarnContext(ctx, "enrichment: cache read failed, computing instead",
			"enricher", e.Name(), "error", err)
		return domain.CacheEntry{}, false
	}
	if !ok || entry.Expired(s.clk.Now()) {
		return domain.CacheEntry{}, false
	}
	return entry, true
}

// cachePut writes the shared layer, treating every failure as a no-op.
func (s *Service) cachePut(
	ctx context.Context,
	scope db.TenantScope,
	e domain.Enricher,
	seed string,
	payload any,
	computedAt time.Time,
	ttl time.Duration,
) {
	if s.cache == nil || seed == "" {
		return
	}
	key := domain.CacheKey(scope.OrgID().String(), e.Name(), e.Version(), seed)
	if key == "" || len(key) > domain.MaxCacheKeyBytes {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	entry, err := domain.NewCacheEntry(domain.CacheEntryParams{
		Key:        key,
		OrgID:      scope.OrgID().String(),
		Payload:    body,
		ComputedAt: computedAt,
		ExpiresAt:  computedAt.Add(ttl),
	})
	if err != nil {
		// An entry the domain refuses is not cacheable, and a cache is never
		// allowed to be the reason a pipeline fails.
		s.log.WarnContext(ctx, "enrichment: refusing to cache an invalid entry",
			"enricher", e.Name(), "error", err)
		return
	}
	if err := s.cache.Put(ctx, scope, entry); err != nil {
		s.log.WarnContext(ctx, "enrichment: cache write failed",
			"enricher", e.Name(), "error", err)
	}
}

// deferStragglers re-enqueues the inline enrichers that ran out of budget as an
// async pass (SPEC §F.3: "a timed-out inline enricher is re-enqueued as
// enrich.run(phase=2)").
//
// ONE job carries all of them. A job per straggler would produce a notification
// per straggler, which is exactly the noise the coalescing rule exists to stop.
func (s *Service) deferStragglers(ctx context.Context, occurrenceID uuid.UUID, results []domain.Enrichment) []string {
	var names []string
	for _, r := range results {
		if r.Status() == domain.StatusTimeout {
			names = append(names, r.Enricher())
		}
	}
	if len(names) == 0 || s.enqueuer == nil {
		return names
	}

	if _, err := s.enqueuer.Enqueue(ctx, jobs.EnrichRunArgs{
		OccurrenceID: occurrenceID,
		Phase:        domain.PhaseNameAsync,
		Enrichers:    names,
	}); err != nil {
		// The results are already recorded as timed out, so the UI is honest
		// either way; all that is lost is the retry.
		s.log.WarnContext(ctx, "enrichment: could not defer stragglers to the async phase",
			"occurrence_id", occurrenceID, "enrichers", strings.Join(names, ","), "error", err)
	}
	return names
}

// announce asks for the ONE coalesced `enriched` notification.
//
// This is the coalescing rule in code: however many enrichers completed, the
// async phase produces exactly one call. SPEC §H.7 makes `enriched` an
// update_root reason, so what the operator sees is the original card amended
// with the new context, plus at most one thread reply — never a reply per
// enricher, which is how enrichment turns into spam.
func (s *Service) announce(ctx context.Context, scope db.TenantScope, loaded Loaded, results []domain.Enrichment) bool {
	if s.notifier == nil || loaded.GroupID == uuid.Nil {
		return false
	}

	var names []string
	for _, r := range results {
		if r.Status().Succeeded() {
			names = append(names, r.Enricher())
		}
	}
	if len(names) == 0 {
		// Nothing new to say. Amending a card to report that nothing was
		// learned is worse than silence.
		return false
	}

	occurrenceID := uuid.Nil
	if occ, err := uuid.Parse(loaded.Subject.Occurrence.ID); err == nil {
		occurrenceID = occ
	}

	if err := s.notifier.NotifyEnriched(ctx, scope, EnrichedNotice{
		GroupID:      loaded.GroupID,
		AlertID:      loaded.AlertID,
		OccurrenceID: occurrenceID,
		StateVersion: loaded.StateVersion,
		Enrichers:    names,
	}); err != nil {
		s.log.WarnContext(ctx, "enrichment: could not request the enriched notification",
			"group_id", loaded.GroupID, "error", err)
		return false
	}
	return true
}

// release lets the deferred first notification go.
//
// A failure here is LOGGED AND SWALLOWED, and that is the whole reason the
// backstop exists: if this enqueue fails, `alerts`' scheduled evaluation still
// fires at the end of the budget and the operator still gets their card. The
// degradation is a card a second or two later without a rule block, never a card
// that never comes.
func (s *Service) release(ctx context.Context, scope db.TenantScope, loaded Loaded) bool {
	if s.notifier == nil || loaded.GroupID == uuid.Nil {
		return false
	}
	occurrenceID := uuid.Nil
	if occ, err := uuid.Parse(loaded.Subject.Occurrence.ID); err == nil {
		occurrenceID = occ
	}
	if err := s.notifier.NotifyPreNotificationReady(ctx, scope, PreNotificationNotice{
		GroupID:      loaded.GroupID,
		AlertID:      loaded.AlertID,
		OccurrenceID: occurrenceID,
		StateVersion: loaded.StateVersion,
	}); err != nil {
		s.log.WarnContext(ctx, "enrichment: could not release the deferred first notification; "+
			"the scheduled backstop will send it",
			"group_id", loaded.GroupID, "occurrence_id", occurrenceID, "error", err)
		return false
	}
	return true
}

// narrate appends the T11 timeline entries. It never fails a run.
func (s *Service) narrate(ctx context.Context, scope db.TenantScope, loaded Loaded, phase domain.Phase, results []domain.Enrichment) {
	if s.events == nil || len(results) == 0 {
		return
	}
	occurrenceID := uuid.Nil
	if occ, err := uuid.Parse(loaded.Subject.Occurrence.ID); err == nil {
		occurrenceID = occ
	}
	if loaded.AlertID == uuid.Nil && occurrenceID == uuid.Nil {
		return
	}

	detail := make(map[string]any, len(results))
	ok, failed := 0, 0
	for _, r := range results {
		detail[r.Enricher()] = map[string]any{
			"status":      string(r.Status()),
			"version":     r.Version(),
			"duration_ms": r.Duration().Milliseconds(),
			"from_cache":  r.FromCache(),
		}
		if r.Status().Succeeded() {
			ok++
		} else if r.Status() == domain.StatusFailed || r.Status() == domain.StatusTimeout {
			failed++
		}
	}

	typ := EventCompleted
	summary := fmt.Sprintf("%s enrichment: %d of %d enrichers produced context", phase, ok, len(results))
	if ok == 0 && failed > 0 {
		typ = EventFailed
		summary = fmt.Sprintf("%s enrichment: no context could be produced (%d failed)", phase, failed)
	}

	if err := s.events.RecordEnrichmentEvent(ctx, scope, EnrichmentEvent{
		Type:         typ,
		AlertID:      loaded.AlertID,
		OccurrenceID: occurrenceID,
		Summary:      truncate(summary, 500),
		Payload:      detail,
		DedupeKey:    fmt.Sprintf("enrich:%s:%s", occurrenceID, phase),
	}); err != nil {
		s.log.WarnContext(ctx, "enrichment: could not record the enrichment event", "error", err)
	}
}

// ExpireCache evicts stale rows from the shared layer. It is the body of the
// `cache.expire` maintenance job (SPEC §G.3, every 600 s).
func (s *Service) ExpireCache(ctx context.Context, limit int) (int64, error) {
	if s.cache == nil {
		return 0, nil
	}
	if limit <= 0 {
		limit = 10000
	}
	return s.cache.DeleteExpired(ctx, s.clk.Now(), limit)
}

func clampWarnings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, w := range in {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		out = append(out, truncate(w, 500))
		if len(out) == domain.MaxWarnings {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
