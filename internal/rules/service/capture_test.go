package service_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/rules/domain"
	"github.com/thulasiram/oto/internal/rules/service"
)

// Capture is the write half of the differentiator (ADR 0009): an alert fires,
// the rule behind it is recovered, content-addressed and stored, and the fact
// that it is NOT the rule the previous fire saw is what oto sells.
//
// These tests run the real Service against fakes, so they are about the
// SERVICE's decisions and not about SQL: which recoveries become which
// snapshots, when `Drifted` is true, which timeline events are emitted, and —
// the whole point of the "degrade, never fail" contract — that a Prometheus that
// is down still produces a stored row saying so.
//
// The drift story is told twice on purpose. Here with an in-memory repository,
// so the decision logic is asserted in isolation; and in drift_db_test.go
// against a real migrated Postgres, so the CTE upsert, the content-uniqueness
// constraint and the ordering are asserted too. Neither subsumes the other.

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// memRepo is a SnapshotRepository whose whole memory is a map.
//
// It reproduces the ONE property the real table is built around: rows are
// deduplicated by (org, rule key, rule_fingerprint), so capturing the same rule
// text a thousand times yields one row and `inserted` is true exactly once. A
// fake that inserted a row per call could not tell "a new version of the rule"
// from "the thousandth fire of an unchanged one", which is the distinction under
// test.
//
// ⛔ THE RULE KEY IS IN THE DEDUP TUPLE because it is in
// rule_snapshots_content_uniq (00040), and a fake that left it out would go on
// passing the tests the defect used to pin. The fingerprint addresses the
// DEFINITION only, so it cannot separate two rules whose recovered text is
// identical — and every `unavailable` capture's text is identical.
//
// It also re-runs Snapshot.Validate, because the real repository does: an
// invalid snapshot must be refused by storage and not only by the caller.
type memRepo struct {
	mu    sync.Mutex
	rows  map[string]domain.Snapshot // org|source|fingerprint
	order []string                   // insertion order, for a stable ListByKey

	upsertErr error
	latestErr error
	// upserts counts writes, so "one row per rule text" is provable.
	upserts int
	// listByKeyCalls counts history reads, so "the store was never asked" is
	// provable — which is the only way to assert that an UNADDRESSABLE key never
	// reaches SQL. The real repository refuses such a key with a validation
	// error, and this fake cannot: it matches on struct fields, so an empty
	// source id simply matches nothing and the refusal is invisible here.
	listByKeyCalls int
}

func newMemRepo() *memRepo { return &memRepo{rows: map[string]domain.Snapshot{}} }

// contentKey mirrors rule_snapshots_content_uniq: (org, source, name, group,
// file, fingerprint). The components are joined on a byte no rule key component
// can contain, so two different keys cannot alias into one string.
func contentKey(s domain.Snapshot) string {
	return strings.Join([]string{
		s.OrgID, s.Key.SourceID, s.Key.Name, s.Key.Group, s.Key.File, s.Fingerprint,
	}, "\x00")
}

func (r *memRepo) Upsert(_ context.Context, s db.TenantScope, snap domain.Snapshot) (domain.Snapshot, bool, error) {
	if r.upsertErr != nil {
		return domain.Snapshot{}, false, r.upsertErr
	}
	if err := snap.Validate(); err != nil {
		return domain.Snapshot{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.upserts++
	snap.OrgID = s.OrgID().String()
	k := contentKey(snap)
	if existing, ok := r.rows[k]; ok {
		return existing, false, nil
	}
	snap.ID = id.New().String()
	r.rows[k] = snap
	r.order = append(r.order, k)
	return snap, true, nil
}

func (r *memRepo) Get(_ context.Context, s db.TenantScope, snapID uuid.UUID) (domain.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range r.order {
		row := r.rows[k]
		if row.ID == snapID.String() && row.OrgID == s.OrgID().String() {
			return row, nil
		}
	}
	return domain.Snapshot{}, errs.NotFound("rules_snapshot_not_found", "no such rule snapshot")
}

func (r *memRepo) GetMany(_ context.Context, s db.TenantScope, ids []uuid.UUID) ([]domain.Snapshot, error) {
	want := make(map[string]struct{}, len(ids))
	for _, i := range ids {
		want[i.String()] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	out := []domain.Snapshot{}
	for _, k := range r.order {
		row := r.rows[k]
		if row.OrgID != s.OrgID().String() {
			continue
		}
		if _, ok := want[row.ID]; ok {
			out = append(out, row)
		}
	}
	return out, nil
}

// matchesKey mirrors the repository's keyPredicate: an EMPTY file or group means
// "unknown", not "the empty string", ON BOTH SIDES. A generatorURL capture knows
// the expression but not the file it is written in, and treating that as an
// equality in EITHER direction splits one rule's history in two on the day
// Prometheus becomes reachable — the query direction was always lenient, and the
// stored direction now is too.
func matchesKey(row domain.Snapshot, key domain.Key) bool {
	if row.Key.SourceID != key.SourceID || row.Key.Name != key.Name {
		return false
	}
	if key.Group != "" && row.Key.Group != "" && row.Key.Group != key.Group {
		return false
	}
	if key.File != "" && row.Key.File != "" && row.Key.File != key.File {
		return false
	}
	return true
}

func (r *memRepo) forKey(s db.TenantScope, key domain.Key) []domain.Snapshot {
	out := []domain.Snapshot{}
	for _, k := range r.order {
		row := r.rows[k]
		if row.OrgID == s.OrgID().String() && matchesKey(row, key) {
			out = append(out, row)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CapturedAt.Before(out[j].CapturedAt) })
	return out
}

func (r *memRepo) ListByKey(_ context.Context, s db.TenantScope, key domain.Key, limit int) ([]domain.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listByKeyCalls++
	out := r.forKey(s, key)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *memRepo) ListPage(_ context.Context, s db.TenantScope, key domain.Key, p db.Keyset) ([]domain.Snapshot, db.Cursor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := r.forKey(s, key)
	// Newest first, like the real one.
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, db.Cursor{}, nil
}

func (r *memRepo) Latest(_ context.Context, s db.TenantScope, key domain.Key) (domain.Snapshot, bool, error) {
	if r.latestErr != nil {
		return domain.Snapshot{}, false, r.latestErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := r.forKey(s, key)
	if len(rows) == 0 {
		return domain.Snapshot{}, false, nil
	}
	return rows[len(rows)-1], true, nil
}

// LatestDefinition mirrors the real one's `AND origin <> 'unavailable'`. A fake
// that returned the newest row whatever it held would go on passing the tests
// the outage defect used to pin: the drift comparison would be handed a row with
// an empty expr, and every fire recovering from an outage would report an edit.
func (r *memRepo) LatestDefinition(_ context.Context, s db.TenantScope, key domain.Key) (domain.Snapshot, bool, error) {
	if r.latestErr != nil {
		return domain.Snapshot{}, false, r.latestErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := r.forKey(s, key)
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Available() {
			return rows[i], true, nil
		}
	}
	return domain.Snapshot{}, false, nil
}

// stubLookup is the upstream. `fn` is nil for "no rule recovery configured".
type stubLookup struct {
	fn    func(service.LookupRequest) (domain.Recovery, error)
	calls int
	last  service.LookupRequest
}

func (l *stubLookup) Lookup(_ context.Context, _ db.TenantScope, req service.LookupRequest) (domain.Recovery, error) {
	l.calls++
	l.last = req
	return l.fn(req)
}

// eventLog records the timeline appends. It can also fail, because a timeline
// write must never be able to fail the capture it describes.
type eventLog struct {
	mu     sync.Mutex
	events []service.RuleEvent
	err    error
}

func (e *eventLog) RecordRuleEvent(_ context.Context, _ db.TenantScope, ev service.RuleEvent) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
	return e.err
}

func (e *eventLog) types() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.events))
	for _, ev := range e.events {
		out = append(out, ev.Type)
	}
	return out
}

func (e *eventLog) all() []service.RuleEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]service.RuleEvent(nil), e.events...)
}

func (e *eventLog) find(typ string) (service.RuleEvent, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.events {
		if ev.Type == typ {
			return ev, true
		}
	}
	return service.RuleEvent{}, false
}

// ---------------------------------------------------------------------------
// Harnessless fixtures
// ---------------------------------------------------------------------------

var errUpstreamDown = errors.New("prometheus: connection refused")

type rig struct {
	svc    *service.Service
	repo   *memRepo
	lookup *stubLookup
	events *eventLog
	clk    *clock.Fake
	scope  db.TenantScope
	source uuid.UUID
}

func newRig(t *testing.T, fn func(service.LookupRequest) (domain.Recovery, error)) *rig {
	t.Helper()

	scope, err := db.NewTenantScope(id.New())
	require.NoError(t, err)

	r := &rig{
		repo:   newMemRepo(),
		lookup: &stubLookup{fn: fn},
		events: &eventLog{},
		clk:    clock.NewFake(time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)),
		scope:  scope,
		source: id.New(),
	}

	opts := service.Options{
		Repo:   r.repo,
		Events: r.events,
		Clock:  r.clk,
		// Discarded: the degradation paths log at Warn and Error by design, and a
		// test that prints them is a test nobody reads the output of.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if fn != nil {
		opts.Lookup = r.lookup
	}

	svc, err := service.New(opts)
	require.NoError(t, err)
	r.svc = svc
	return r
}

func (r *rig) capture(t *testing.T, alertID, occurrenceID uuid.UUID) service.Capture {
	t.Helper()
	c, err := r.svc.Capture(context.Background(), r.scope, service.CaptureRequest{
		SourceID:     r.source,
		AlertID:      alertID,
		OccurrenceID: occurrenceID,
		Labels: map[string]string{
			"alertname": "HighErrorRate",
			"severity":  "critical",
			"service":   "checkout",
		},
		Annotations:  map[string]string{"summary": "checkout is unhappy"},
		GeneratorURL: "https://prom.internal/graph?g0.expr=up+%3D%3D+0&g0.tab=1",
	})
	require.NoError(t, err)
	return c
}

// recoveredRule is a complete rules-API recovery, the shape the enrichment path
// produces on a good day.
func recoveredRule(expr string, forSeconds float64) domain.Recovery {
	return domain.Recovery{
		Origin:         domain.OriginPrometheusAPI,
		Strategy:       domain.StrategyRulesAPI,
		Confidence:     domain.ConfidenceExact,
		CandidateCount: 1,
		RuleName:       "HighErrorRate",
		RuleGroup:      "checkout",
		RuleFile:       "/etc/prometheus/rules/checkout.yml",
		Expr:           expr,
		ForSeconds:     forSeconds,
		Labels:         map[string]string{"severity": "critical"},
		Annotations:    map[string]string{"summary": "error rate high"},
		PrometheusURL:  "https://prom.internal",
	}
}

// ---------------------------------------------------------------------------
// The request contract
// ---------------------------------------------------------------------------

func TestCaptureRefusesARequestItCannotKey(t *testing.T) {
	t.Parallel()

	r := newRig(t, func(service.LookupRequest) (domain.Recovery, error) {
		return recoveredRule("up == 0", 300), nil
	})

	t.Run("no source", func(t *testing.T) {
		_, err := r.svc.Capture(context.Background(), r.scope, service.CaptureRequest{
			Labels: map[string]string{"alertname": "X"},
		})
		requireCode(t, err, service.CodeNoSource)
	})

	t.Run("no alertname", func(t *testing.T) {
		_, err := r.svc.Capture(context.Background(), r.scope, service.CaptureRequest{
			SourceID: r.source,
			Labels:   map[string]string{"severity": "critical"},
		})
		requireCode(t, err, service.CodeNoAlertName)
	})

	t.Run("a whitespace-only alertname is no alertname", func(t *testing.T) {
		_, err := r.svc.Capture(context.Background(), r.scope, service.CaptureRequest{
			SourceID: r.source,
			Labels:   map[string]string{"alertname": "   "},
		})
		requireCode(t, err, service.CodeNoAlertName)
	})

	assert.Equal(t, 0, r.lookup.calls, "a request that cannot be keyed must not reach Prometheus")
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, code, errs.CodeOf(err))
}

// ---------------------------------------------------------------------------
// The happy path
// ---------------------------------------------------------------------------

func TestCaptureStoresWhatTheRuleSaid(t *testing.T) {
	t.Parallel()

	r := newRig(t, func(service.LookupRequest) (domain.Recovery, error) {
		return recoveredRule("rate(errors[5m]) > 0.05", 300), nil
	})
	alertID, occID := id.New(), id.New()

	c := r.capture(t, alertID, occID)

	require.True(t, c.Recovered())
	assert.True(t, c.NewVersion, "the first capture of a rule text is a new version")
	assert.False(t, c.Drifted, "there is nothing to have drifted from")
	assert.Empty(t, c.PreviousFingerprint)

	snap := c.Snapshot
	assert.Equal(t, "rate(errors[5m]) > 0.05", snap.Expr)
	assert.Equal(t, 300.0, snap.ForSeconds)
	assert.Equal(t, domain.OriginPrometheusAPI, snap.Origin)
	assert.Equal(t, domain.ConfidenceExact, snap.Confidence)
	assert.Equal(t, 1, snap.CandidateCount)
	assert.Equal(t, "HighErrorRate", snap.Key.Name)
	assert.Equal(t, "checkout", snap.Key.Group)
	assert.Equal(t, r.source.String(), snap.Key.SourceID)
	assert.Equal(t, r.clk.Now(), snap.CapturedAt)
	require.NoError(t, snap.Validate())

	// The alertname, not the recovered rule name, is what the key is built from:
	// the alert is the thing that fired.
	assert.Equal(t, "HighErrorRate", r.lookup.last.Labels["alertname"])

	assert.Equal(t, []string{service.EventSnapshotCaptured}, r.events.types(),
		"a first capture is captured, not changed")
}

// TestCaptureCostsOneRowPerRuleText is what makes the history a list of EDITS.
// A rule that fires every thirty seconds for a week is one row.
func TestCaptureCostsOneRowPerRuleText(t *testing.T) {
	t.Parallel()

	r := newRig(t, func(service.LookupRequest) (domain.Recovery, error) {
		return recoveredRule("rate(errors[5m]) > 0.05", 300), nil
	})

	first := r.capture(t, id.New(), id.New())
	require.True(t, first.NewVersion)

	for i := 0; i < 10; i++ {
		r.clk.Advance(30 * time.Second)
		again := r.capture(t, id.New(), id.New())
		assert.False(t, again.NewVersion, "an unchanged rule is not a new version")
		assert.False(t, again.Drifted, "an unchanged rule has not drifted")
		assert.Equal(t, first.Snapshot.ID, again.Snapshot.ID, "the same content is the same row")
		assert.Equal(t, first.Snapshot.Fingerprint, again.Snapshot.Fingerprint)
		assert.Equal(t, first.Snapshot.CapturedAt, again.Snapshot.CapturedAt,
			"the stored row keeps the FIRST capture's timestamp; it is the row that already existed")
		assert.Equal(t, first.Snapshot.Fingerprint, again.PreviousFingerprint,
			"the previous capture is the same content, which is how Drifted stays false")
	}

	history, err := r.svc.History(context.Background(), r.scope, first.Snapshot.Key)
	require.NoError(t, err)
	assert.Equal(t, 1, history.Len(), "eleven fires, one version")
}

// ---------------------------------------------------------------------------
// Drift
// ---------------------------------------------------------------------------

// TestCaptureDetectsDriftAcrossARuleEdit is the product, in one test: the same
// alert fires, somebody lowers the threshold, it fires again, and oto can say so
// and show both texts.
func TestCaptureDetectsDriftAcrossARuleEdit(t *testing.T) {
	t.Parallel()

	expr := "rate(errors[5m]) > 0.05"
	forSeconds := 300.0
	r := newRig(t, func(service.LookupRequest) (domain.Recovery, error) {
		return recoveredRule(expr, forSeconds), nil
	})

	before := r.capture(t, id.New(), id.New())
	require.True(t, before.NewVersion)
	require.False(t, before.Drifted)

	// Somebody edits the rule: the threshold drops and `for` is shortened.
	r.clk.Advance(2 * time.Hour)
	expr, forSeconds = "rate(errors[5m]) > 0.02", 60

	after := r.capture(t, id.New(), id.New())

	assert.True(t, after.NewVersion, "a changed rule is a new row")
	assert.True(t, after.Drifted, "this is drift: the rule is not what the previous fire saw")
	assert.Equal(t, before.Snapshot.Fingerprint, after.PreviousFingerprint)
	assert.NotEqual(t, before.Snapshot.Fingerprint, after.Snapshot.Fingerprint)
	assert.NotEqual(t, before.Snapshot.ID, after.Snapshot.ID)

	// BOTH snapshots are still retrievable, and they differ. The old one is not
	// overwritten — that is the entire reason the table has no UPDATE.
	ctx := context.Background()
	oldID := uuid.MustParse(before.Snapshot.ID)
	newID := uuid.MustParse(after.Snapshot.ID)

	oldSnap, err := r.svc.Get(ctx, r.scope, oldID)
	require.NoError(t, err)
	newSnap, err := r.svc.Get(ctx, r.scope, newID)
	require.NoError(t, err)

	assert.Equal(t, "rate(errors[5m]) > 0.05", oldSnap.Expr, "the rule as it was when the FIRST alert fired")
	assert.Equal(t, "rate(errors[5m]) > 0.02", newSnap.Expr)
	assert.Equal(t, 300.0, oldSnap.ForSeconds)
	assert.Equal(t, 60.0, newSnap.ForSeconds)

	// The numbered history has two versions and diffs them the right way round.
	history, err := r.svc.History(ctx, r.scope, after.Snapshot.Key)
	require.NoError(t, err)
	require.Equal(t, 2, history.Len())
	assert.True(t, history.Drifted(before.Snapshot.Fingerprint),
		"the fingerprint the first fire was bound to is no longer the newest")

	diff, err := r.svc.DiffVersions(ctx, r.scope, after.Snapshot.Key, 1, 2)
	require.NoError(t, err)
	assert.True(t, diff.SameRule)
	assert.True(t, diff.Changed)
	assert.True(t, diff.ExprChanged)
	assert.True(t, diff.ForChanged)
	assert.Equal(t, -240.0, diff.ForDelta, "`for` went from 300s to 60s")

	// And the same question asked the way the alert card asks it.
	since, ok, err := r.svc.DiffSince(ctx, r.scope, after.Snapshot.Key, before.Snapshot.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok, "there is something to say: the rule has changed since that fire")
	assert.Equal(t, before.Snapshot.Fingerprint, since.From.Fingerprint)
	assert.Equal(t, after.Snapshot.Fingerprint, since.To.Fingerprint)

	// Nothing to say when the bound fingerprint IS the newest.
	_, ok, err = r.svc.DiffSince(ctx, r.scope, after.Snapshot.Key, after.Snapshot.Fingerprint)
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestCaptureNarratesDrift: SPEC §D.4.1 gives drift its own timeline type, and
// ADR 0009 says the reply goes out regardless of channel verbosity. Both need the
// event to exist and to carry the two fingerprints.
func TestCaptureNarratesDrift(t *testing.T) {
	t.Parallel()

	expr := "up == 0"
	r := newRig(t, func(service.LookupRequest) (domain.Recovery, error) {
		return recoveredRule(expr, 300), nil
	})

	first := r.capture(t, id.New(), id.New())
	expr = "up == 1"
	r.clk.Advance(time.Hour)
	second := r.capture(t, id.New(), id.New())

	assert.Equal(t, []string{
		service.EventSnapshotCaptured,
		service.EventSnapshotCaptured,
		service.EventDefinitionChanged,
	}, r.events.types(), "drift is narrated IN ADDITION to the capture, not instead of it")

	ev, ok := r.events.find(service.EventDefinitionChanged)
	require.True(t, ok)
	assert.Equal(t, first.Snapshot.Fingerprint, ev.Payload["previous_fingerprint"])
	assert.Equal(t, second.Snapshot.Fingerprint, ev.Payload["fingerprint"])
	assert.Equal(t, "HighErrorRate", ev.Payload["rule_name"])
	assert.NotEmpty(t, ev.DedupeKey, "the append is idempotent (SPEC §C.8)")
	assert.LessOrEqual(t, len(ev.Summary), 500, "ev_summary_ck is 1..500 bytes")
	assert.NotEmpty(t, ev.Summary)
}

// TestDriftIsPerRuleKey: two rules are not each other's history. A rule oto has
// never seen before has no predecessor, however busy the source is.
func TestDriftIsPerRuleKey(t *testing.T) {
	t.Parallel()

	exprs := map[string]string{"AlertA": "up == 0", "AlertB": "up == 1"}
	r := newRig(t, func(req service.LookupRequest) (domain.Recovery, error) {
		name := req.Labels["alertname"]
		rec := recoveredRule(exprs[name], 300)
		rec.RuleName = name
		return rec, nil
	})

	ctx := context.Background()
	captures := map[string]service.Capture{}
	for _, name := range []string{"AlertA", "AlertB"} {
		c, err := r.svc.Capture(ctx, r.scope, service.CaptureRequest{
			SourceID: r.source,
			AlertID:  id.New(),
			Labels:   map[string]string{"alertname": name},
		})
		require.NoError(t, err)
		assert.False(t, c.Drifted, "%s has no predecessor of its own", name)
		assert.True(t, c.NewVersion)
		assert.Equal(t, name, c.Snapshot.Key.Name)
		captures[name] = c
	}
	assert.NotEqual(t, captures["AlertA"].Snapshot.ID, captures["AlertB"].Snapshot.ID)

	// Editing one rule does not make the other look edited.
	exprs["AlertA"] = "up == 2"
	r.clk.Advance(time.Hour)

	edited, err := r.svc.Capture(ctx, r.scope, service.CaptureRequest{
		SourceID: r.source, AlertID: id.New(),
		Labels: map[string]string{"alertname": "AlertA"},
	})
	require.NoError(t, err)
	assert.True(t, edited.Drifted)
	assert.Equal(t, captures["AlertA"].Snapshot.Fingerprint, edited.PreviousFingerprint,
		"the predecessor must be AlertA's own previous capture, not the newest row on the source")

	unedited, err := r.svc.Capture(ctx, r.scope, service.CaptureRequest{
		SourceID: r.source, AlertID: id.New(),
		Labels: map[string]string{"alertname": "AlertB"},
	})
	require.NoError(t, err)
	assert.False(t, unedited.Drifted, "AlertB was not touched")
}

// TestTwoRulesWithIdenticalContentKeepTheirOwnRows: identical CONTENT is not
// identical IDENTITY, and the row is not pure content.
//
// ⭐ WHY THIS IS NOT DE-DUPLICATION. It looks like it could be — two rules whose
// definitions are byte-identical could plausibly share one content-addressed
// row, the way two fires of one unchanged rule do. They cannot, and the reason is
// structural rather than aesthetic: a `rule_snapshots` row is NOT the definition,
// it is the definition PLUS the RuleKey it was captured under (SPEC §C.6 keeps
// the key out of `rule_fingerprint` deliberately, so that a renamed rule can be
// recognised as the same text). One row cannot carry two keys. So a shared row
// must pick one rule_name and be WRONG about the other — and every read path in
// the module (Latest, ListByKey, ListPage) filters on rule_name, which makes the
// loser's own capture unreachable to it forever. De-duplication that the reader
// cannot undo is not de-duplication, it is loss.
//
// The saving was illusory anyway. The case that makes it common is an
// `unavailable` recovery: empty expr, no durations, empty maps, therefore ONE
// fingerprint for every unrecoverable rule in a source. That is the case where
// per-rule rows matter most, because "oto could not see YOUR rule" is the entire
// content of the row.
//
// 00040 put the rule key in rule_snapshots_content_uniq. Dedup still costs one
// row per rule text — it just counts texts per RULE now, not per source.
func TestTwoRulesWithIdenticalContentKeepTheirOwnRows(t *testing.T) {
	t.Parallel()

	// Neither alert's rule can be recovered — the ordinary firewalled case.
	r := newRig(t, func(req service.LookupRequest) (domain.Recovery, error) {
		return domain.Recovery{
			Origin:   domain.OriginUnavailable,
			Strategy: domain.StrategyNone,
			RuleName: req.Labels["alertname"],
			Notes:    []string{"rules_api_no_match"},
		}, nil
	})

	ctx := context.Background()
	first, err := r.svc.Capture(ctx, r.scope, service.CaptureRequest{
		SourceID: r.source, AlertID: id.New(),
		Labels: map[string]string{"alertname": "AlertA"},
	})
	require.NoError(t, err)
	require.True(t, first.NewVersion)
	require.Equal(t, "AlertA", first.Snapshot.Key.Name)

	second, err := r.svc.Capture(ctx, r.scope, service.CaptureRequest{
		SourceID: r.source, AlertID: id.New(),
		Labels: map[string]string{"alertname": "AlertB"},
	})
	require.NoError(t, err)

	assert.Equal(t, first.Snapshot.Fingerprint, second.Snapshot.Fingerprint,
		"every unavailable capture has the same content: nothing")
	assert.NotEqual(t, first.Snapshot.ID, second.Snapshot.ID,
		"...but two rules are two rows, because the row carries a rule key and one row cannot carry two")
	assert.Equal(t, "AlertB", second.Snapshot.Key.Name,
		"AlertB's capture is AlertB's, not the first failure's")
	assert.True(t, second.NewVersion,
		"a rule oto had never captured is a new version, whatever else in the source hashes the same")
	assert.False(t, second.Drifted, "AlertB has no predecessor of its own")

	// Each alert can find its own capture again, which is the property the shared
	// row destroyed: Latest filters on rule_name.
	for name, want := range map[string]service.Capture{"AlertA": first, "AlertB": second} {
		latest, ok, err := r.svc.Latest(ctx, r.scope, domain.Key{SourceID: r.source.String(), Name: name})
		require.NoError(t, err)
		require.True(t, ok, "%s must be able to reach its own stored capture", name)
		assert.Equal(t, want.Snapshot.ID, latest.ID)
	}

	// And the timeline is where an operator would have seen the old lie: each
	// event now names the rule it is actually about.
	all := r.events.all()
	require.Len(t, all, 2)
	assert.Equal(t, "AlertA", all[0].Payload["rule_name"])
	assert.Equal(t, "AlertB", all[1].Payload["rule_name"])
	assert.Equal(t, service.EventLookupFailed, all[1].Type)
	assert.Contains(t, all[1].Summary, "AlertB",
		`the "could not be recovered" line must name the rule that could not be recovered`)
}

// ---------------------------------------------------------------------------
// match_confidence and candidate_count
// ---------------------------------------------------------------------------

// TestCaptureCarriesTheMatchConfidenceItWasGiven pins what reaches
// `rule_snapshots.match_confidence` and `candidate_count` for zero, one and
// several candidate rules.
//
// SPEC §D.6 and ADR 0009 are unambiguous about why this matters: `ambiguous` is
// surfaced in the UI and in Slack and is NEVER silently guessed. A service that
// quietly upgraded an ambiguous match to an exact one would turn "we picked one
// of three rules with this name" into "here is your rule", which is the single
// most misleading thing this module could do.
//
// The confidence/count pairs are locked to each other by rule_snapshots_conf_ck.
// The matcher's own derivation of them — why three candidates come back
// `ambiguous` rather than `probable` — is asserted in matching_test.go against
// the real matcher.
func TestCaptureCarriesTheMatchConfidenceItWasGiven(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		recovery   domain.Recovery
		confidence domain.Confidence
		count      int
		available  bool
		ambiguous  bool
	}{
		{
			name: "zero candidates: nothing matched, and the row says so",
			recovery: domain.Recovery{
				Origin:     domain.OriginUnavailable,
				Strategy:   domain.StrategyNone,
				Confidence: domain.ConfidenceNone,
				RuleName:   "HighErrorRate",
				Notes:      []string{"rules_api_no_match"},
			},
			confidence: domain.ConfidenceNone,
			count:      0,
		},
		{
			name: "one candidate: exact",
			recovery: func() domain.Recovery {
				r := recoveredRule("up == 0", 300)
				r.Confidence, r.CandidateCount = domain.ConfidenceExact, 1
				return r
			}(),
			confidence: domain.ConfidenceExact,
			count:      1,
			available:  true,
		},
		{
			name: "several candidates with a clear winner: probable",
			recovery: func() domain.Recovery {
				r := recoveredRule("up == 0", 300)
				r.Confidence, r.CandidateCount = domain.ConfidenceProbable, 3
				r.Notes = []string{"duplicate_alertname", "rule_label_mismatch"}
				return r
			}(),
			confidence: domain.ConfidenceProbable,
			count:      3,
			available:  true,
		},
		{
			name: "several candidates tied: ambiguous, and it must be visible",
			recovery: func() domain.Recovery {
				r := recoveredRule("up == 0", 300)
				r.Confidence, r.CandidateCount = domain.ConfidenceAmbiguous, 3
				r.Notes = []string{"duplicate_alertname"}
				return r
			}(),
			confidence: domain.ConfidenceAmbiguous,
			count:      3,
			available:  true,
			ambiguous:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, func(service.LookupRequest) (domain.Recovery, error) {
				return tc.recovery, nil
			})

			c := r.capture(t, id.New(), id.New())

			assert.Equal(t, tc.confidence, c.Snapshot.Confidence)
			assert.Equal(t, tc.count, c.Snapshot.CandidateCount)
			assert.Equal(t, tc.available, c.Snapshot.Available())
			assert.Equal(t, tc.ambiguous, c.Snapshot.Ambiguous())
			require.NoError(t, c.Snapshot.Validate(),
				"the confidence/count pair must satisfy rule_snapshots_conf_ck")

			// The reason codes are shown, never swallowed.
			for _, note := range tc.recovery.Notes {
				assert.Contains(t, c.Warnings, note)
			}

			ev, ok := r.events.find(service.EventSnapshotCaptured)
			if !tc.available {
				ev, ok = r.events.find(service.EventLookupFailed)
			}
			require.True(t, ok)
			assert.Equal(t, string(tc.confidence), ev.Payload["confidence"],
				"the timeline must carry the confidence too; an operator reading events must not have to guess")
			assert.Equal(t, tc.count, ev.Payload["candidate_count"])
		})
	}
}

// ---------------------------------------------------------------------------
// Degradation: the contract that this never fails
// ---------------------------------------------------------------------------

func TestCaptureDegradesRatherThanFailing(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		fn   func(service.LookupRequest) (domain.Recovery, error)
		note string
	}{
		{
			name: "no rule lookup is configured at all",
			fn:   nil,
			note: "rule_lookup_not_configured",
		},
		{
			name: "the lookup returned an error",
			fn: func(service.LookupRequest) (domain.Recovery, error) {
				return domain.Recovery{}, errUpstreamDown
			},
			note: "rule_lookup_failed",
		},
		{
			name: "the lookup recovered nothing",
			fn: func(service.LookupRequest) (domain.Recovery, error) {
				return domain.Recovery{RuleName: "HighErrorRate", Notes: []string{"rules_api_no_match"}}, nil
			},
			note: "rules_api_no_match",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, tc.fn)

			c := r.capture(t, id.New(), id.New())

			// A row exists, and it says "we looked and could not see it".
			assert.False(t, c.Recovered())
			assert.Equal(t, domain.OriginUnavailable, c.Snapshot.Origin)
			assert.Empty(t, c.Snapshot.Expr)
			assert.Equal(t, domain.ConfidenceNone, c.Snapshot.Confidence)
			assert.Equal(t, 0, c.Snapshot.CandidateCount)
			assert.NotEmpty(t, c.Snapshot.Fingerprint, "even 'unavailable' is content-addressed")
			require.NoError(t, c.Snapshot.Validate())
			assert.Contains(t, c.Warnings, tc.note)

			assert.Equal(t, []string{service.EventLookupFailed}, r.events.types(),
				"the operator learns that oto tried; an empty panel would not say that")
		})
	}
}

// TestCaptureDegradesAnInvalidSnapshotRatherThanStoringIt is the answer to
// "NewSnapshot returns no error, so can an invalid snapshot be built?".
//
// It CAN — NewSnapshot is total and will happily produce a Snapshot whose
// confidence and candidate count disagree — and the service is the layer that
// notices. Rather than letting a CHECK violation surface as a 500 (SPEC §L.1),
// Capture rebuilds the snapshot as a well-formed `unavailable` row and records
// why. A fabricated snapshot would be worse than an honest absence.
func TestCaptureDegradesAnInvalidSnapshotRatherThanStoringIt(t *testing.T) {
	t.Parallel()

	// A recovery a buggy adapter could produce: "exactly one candidate" claimed
	// alongside a count of three. rule_snapshots_conf_ck forbids the pair.
	bad := recoveredRule("up == 0", 300)
	bad.Confidence, bad.CandidateCount = domain.ConfidenceExact, 3

	// The constructor does NOT refuse it. This is the fact the issue asked about.
	built := domain.NewSnapshot("org-1", domain.Key{SourceID: id.New().String(), Name: "X"},
		bad, time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	require.Error(t, built.Validate(),
		"NewSnapshot is total: an invalid snapshot is constructible and only Validate says so")

	r := newRig(t, func(service.LookupRequest) (domain.Recovery, error) { return bad, nil })
	c := r.capture(t, id.New(), id.New())

	// ...but it is not storable, and the service does not try.
	assert.False(t, c.Recovered())
	assert.Equal(t, domain.OriginUnavailable, c.Snapshot.Origin)
	assert.Contains(t, c.Warnings, "snapshot_invariant_violation")
	require.NoError(t, c.Snapshot.Validate())
	assert.Equal(t, 1, r.repo.upserts, "one write, and it was the degraded row")
}

// TestCaptureFailsOnlyWhenStorageDoes: the two cases where pretending would put a
// lie in the database.
func TestCaptureFailsOnlyWhenStorageDoes(t *testing.T) {
	t.Parallel()

	t.Run("the write failed", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, func(service.LookupRequest) (domain.Recovery, error) {
			return recoveredRule("up == 0", 300), nil
		})
		r.repo.upsertErr = errors.New("postgres: connection reset")
		_, err := r.svc.Capture(context.Background(), r.scope, service.CaptureRequest{
			SourceID: r.source,
			Labels:   map[string]string{"alertname": "HighErrorRate"},
		})
		require.Error(t, err)
	})

	t.Run("the previous-capture read failed", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, func(service.LookupRequest) (domain.Recovery, error) {
			return recoveredRule("up == 0", 300), nil
		})
		r.repo.latestErr = errors.New("postgres: connection reset")
		_, err := r.svc.Capture(context.Background(), r.scope, service.CaptureRequest{
			SourceID: r.source,
			Labels:   map[string]string{"alertname": "HighErrorRate"},
		})
		require.Error(t, err, "drift cannot be decided without the previous capture, and guessing is not an option")
	})
}

// TestCaptureSurvivesATimelineThatWillNotWrite: a timeline is a record of what
// happened. Failing the thing that happened because it could not be written down
// is backwards.
func TestCaptureSurvivesATimelineThatWillNotWrite(t *testing.T) {
	t.Parallel()

	r := newRig(t, func(service.LookupRequest) (domain.Recovery, error) {
		return recoveredRule("up == 0", 300), nil
	})
	r.events.err = errors.New("alert_events: no partition for this range")

	c := r.capture(t, id.New(), id.New())
	assert.True(t, c.Recovered())
	assert.NotEmpty(t, c.Snapshot.ID)
}

// TestCaptureNarratesOnlyWhatItCanScope: an event must hang off an alert or an
// occurrence, so an unscoped capture is stored and simply not narrated.
func TestCaptureNarratesOnlyWhatItCanScope(t *testing.T) {
	t.Parallel()

	r := newRig(t, func(service.LookupRequest) (domain.Recovery, error) {
		return recoveredRule("up == 0", 300), nil
	})

	c, err := r.svc.Capture(context.Background(), r.scope, service.CaptureRequest{
		SourceID: r.source,
		Labels:   map[string]string{"alertname": "HighErrorRate"},
	})
	require.NoError(t, err)
	assert.True(t, c.Recovered())
	assert.Empty(t, r.events.types())
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// TestGetManyCollapsesDuplicates is ADR 0025's arithmetic: fifty alerts firing
// under one unchanged rule carry fifty copies of ONE snapshot id, and the batch
// must cost what the distinct set costs.
func TestGetManyCollapsesDuplicates(t *testing.T) {
	t.Parallel()

	r := newRig(t, func(service.LookupRequest) (domain.Recovery, error) {
		return recoveredRule("up == 0", 300), nil
	})
	c := r.capture(t, id.New(), id.New())
	snapID := uuid.MustParse(c.Snapshot.ID)

	ids := make([]uuid.UUID, 0, 52)
	for i := 0; i < 50; i++ {
		ids = append(ids, snapID)
	}
	// A nil id and an id from nowhere: neither may take the page down.
	ids = append(ids, uuid.Nil, id.New())

	out, err := r.svc.GetMany(context.Background(), r.scope, ids)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, c.Snapshot.Fingerprint, out[0].Fingerprint)

	empty, err := r.svc.GetMany(context.Background(), r.scope, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestDiffVersionsRejectsAVersionThatDoesNotExist(t *testing.T) {
	t.Parallel()

	r := newRig(t, func(service.LookupRequest) (domain.Recovery, error) {
		return recoveredRule("up == 0", 300), nil
	})
	c := r.capture(t, id.New(), id.New())

	_, err := r.svc.DiffVersions(context.Background(), r.scope, c.Snapshot.Key, 1, 9)
	requireCode(t, err, service.CodeUnknownVersion)
}

// ⛔ TestAKeyThatCannotAddressTheStoreHasAnEmptyHistoryRatherThanARefusal.
//
// `rule_snapshots` is addressed on `(org_id, source_id, rule_name, …)`, so
// `repository.keyPredicate` parses `key.SourceID` and refuses the key as a
// VALIDATION error — `422 rules_invalid_id` — when it cannot. That is the right
// answer for a caller who typed a source id and typed it wrong, and the wrong
// answer for a key OTO ITSELF derived and had nothing to derive from: the
// alertname-only key `rules/api` builds for an alert with no captured snapshot.
//
// What broke when it did not hold (TICKET 015b25b): `GET
// /api/v1/alerts/{id}/rule` answered 422 "a rule key's source id must be a UUID"
// for every alert whose `generatorURL` carried no expression — measured at four
// in five on a real server — and the alert detail page rendered oto's own
// missing data as the operator's mistake, with a Try again button that could
// never succeed.
//
// The store must not even be asked. An unaddressable key describes no rows, so a
// round trip for it is a query that cannot match wearing the costume of one that
// might.
func TestAKeyThatCannotAddressTheStoreHasAnEmptyHistoryRatherThanARefusal(t *testing.T) {
	t.Parallel()

	r := newRig(t, func(service.LookupRequest) (domain.Recovery, error) {
		return recoveredRule("up == 0", 300), nil
	})
	captured := r.capture(t, id.New(), id.New())
	require.NotEmpty(t, captured.Snapshot.Key.SourceID)

	// The addressable key still works, and reading it is what proves the counter
	// below is counting something real.
	full, err := r.svc.History(context.Background(), r.scope, captured.Snapshot.Key)
	require.NoError(t, err)
	require.Equal(t, 1, full.Len())
	before := r.repo.listByKeyCalls
	require.Positive(t, before)

	unaddressable := []struct {
		name string
		key  domain.Key
	}{
		{
			// The exact key `rules/api.keyOf` builds when no snapshot was bound:
			// the alertname and nothing else.
			name: "the alertname alone, which is what oto can name from an alert with no capture",
			key:  domain.Key{Name: captured.Snapshot.Key.Name},
		},
		{
			name: "a source id that is not a uuid",
			key:  domain.Key{SourceID: "not-a-uuid", Name: captured.Snapshot.Key.Name},
		},
		{
			// It parses, so the repository would build a predicate from it — and
			// no AlertSource is ever the zero id, so the round trip cannot match.
			name: "the nil uuid, which parses and still addresses nothing",
			key:  domain.Key{SourceID: uuid.Nil.String(), Name: captured.Snapshot.Key.Name},
		},
		{
			name: "a real source with no rule name",
			key:  domain.Key{SourceID: r.source.String(), Name: "   "},
		},
	}

	for _, tc := range unaddressable {
		t.Run(tc.name, func(t *testing.T) {
			assert.False(t, service.Addressable(tc.key))

			h, err := r.svc.History(context.Background(), r.scope, tc.key)
			require.NoError(t, err,
				"absence of a capture is not a client error; this is the 422 the ticket is about")
			assert.Equal(t, 0, h.Len(), "an unaddressable key describes no rows")
			assert.Equal(t, tc.key, h.Key, "the key is still echoed back; it is nameable, just not queryable")
			assert.Equal(t, before, r.repo.listByKeyCalls,
				"the store was asked about a key that cannot address it")
		})
	}
}

func TestNewRequiresARepository(t *testing.T) {
	t.Parallel()

	_, err := service.New(service.Options{})
	requireCode(t, err, service.CodeMissingRepository)
}
