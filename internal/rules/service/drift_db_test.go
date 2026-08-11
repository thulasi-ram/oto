package service_test

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/rules/domain"
	"github.com/thulasiram/oto/internal/rules/repository"
	"github.com/thulasiram/oto/internal/rules/service"
	"github.com/thulasiram/oto/test/harness"
)

// Drift, end to end, against a real migrated Postgres.
//
// capture_test.go asserts the service's DECISIONS against an in-memory
// repository. This file asserts the parts only Postgres can answer:
//
//   - the CTE upsert really does return the incumbent row when the content
//     matches, so `inserted` distinguishes a new version of a rule from the
//     thousandth fire of an unchanged one WITHOUT a second round trip;
//   - `rule_snapshots_content_uniq` really does collapse repeat captures OF ONE
//     RULE to one row, which is what makes "snapshot on every fire" affordable,
//     while keeping two different rules apart however alike their text (00040);
//   - both snapshots survive the edit and are still readable by id, because the
//     table has no UPDATE and no DELETE — that absence IS the drift feature;
//   - the eleven CHECK constraints agree with Snapshot.Validate, so a snapshot
//     that passes in Go is a row Postgres accepts (SPEC §L.1: a CHECK reaching
//     the HTTP layer means layers 1-3 have a hole).
//
// The rule is edited between the two fires the way it happens in production:
// somebody lowers a threshold, and the next occurrence of the SAME alert
// recovers different text.

func TestMain(m *testing.M) { harness.Main(m) }

// dbRig is the service wired to the real repository, on this test's own database.
type dbRig struct {
	h      *harness.H
	svc    *service.Service
	repo   *repository.SnapshotRepository
	scope  db.TenantScope
	source uuid.UUID
	// recovery is what the next Capture will recover. Reassign it to edit the
	// rule between fires.
	recovery *domain.Recovery
}

func newDBRig(t *testing.T) *dbRig {
	t.Helper()

	h := harness.New(t)
	org := h.Org()
	cluster := h.Cluster(org)
	source := h.Source(org, cluster)

	rec := recoveredRule("rate(errors_total[5m]) > 0.05", 300)
	r := &dbRig{
		h:        h,
		repo:     repository.NewSnapshotRepository(h.Pool),
		scope:    org.Scope,
		source:   source.ID,
		recovery: &rec,
	}

	svc, err := service.New(service.Options{
		Repo:   r.repo,
		Lookup: &stubLookup{fn: func(service.LookupRequest) (domain.Recovery, error) { return *r.recovery, nil }},
		Clock:  h.Clock,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	r.svc = svc
	return r
}

func (r *dbRig) fire(t *testing.T) service.Capture {
	t.Helper()
	c, err := r.svc.Capture(r.h.Ctx, r.scope, service.CaptureRequest{
		SourceID:     r.source,
		AlertID:      id.New(),
		OccurrenceID: id.New(),
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

func (r *dbRig) rowCount(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, r.h.Pool.QueryRow(r.h.Ctx,
		`SELECT count(*) FROM rule_snapshots WHERE org_id = $1`, r.scope.OrgID()).Scan(&n))
	return n
}

// TestTheSameAlertFiresTwiceAcrossARuleEdit is the issue's headline scenario and
// the sentence README.md leads with: what the rule said at that moment.
func TestTheSameAlertFiresTwiceAcrossARuleEdit(t *testing.T) {
	t.Parallel()

	r := newDBRig(t)

	// --- the first fire, at 12:00 -------------------------------------------
	before := r.fire(t)
	require.True(t, before.Recovered())
	assert.True(t, before.NewVersion)
	assert.False(t, before.Drifted, "nothing to have drifted from")
	assert.Empty(t, before.PreviousFingerprint)
	assert.Equal(t, r.h.Now(), before.Snapshot.CapturedAt)
	require.Equal(t, 1, r.rowCount(t))

	// --- it fires again at 12:30, unchanged ---------------------------------
	r.h.Advance(30 * time.Minute)
	again := r.fire(t)
	assert.False(t, again.NewVersion, "the same rule text is the same row")
	assert.False(t, again.Drifted)
	assert.Equal(t, before.Snapshot.ID, again.Snapshot.ID)
	assert.Equal(t, before.Snapshot.CapturedAt, again.Snapshot.CapturedAt,
		"the incumbent row is returned intact; the table is append-only")
	assert.Equal(t, 1, r.rowCount(t), "rule_snapshots_content_uniq: one row, two fires")

	// --- somebody lowers the threshold and shortens `for` --------------------
	r.h.Advance(90 * time.Minute)
	*r.recovery = recoveredRule("rate(errors_total[5m]) > 0.02", 60)

	after := r.fire(t)
	assert.True(t, after.NewVersion, "a changed rule is a NEW row, never an update")
	assert.True(t, after.Drifted, "THIS is the product: the rule is not what the last fire saw")
	assert.Equal(t, before.Snapshot.Fingerprint, after.PreviousFingerprint)
	assert.NotEqual(t, before.Snapshot.Fingerprint, after.Snapshot.Fingerprint)
	assert.Equal(t, 2, r.rowCount(t))

	// --- BOTH snapshots are still retrievable, and they differ ---------------
	oldSnap, err := r.svc.Get(r.h.Ctx, r.scope, uuid.MustParse(before.Snapshot.ID))
	require.NoError(t, err, "the rule as it was when the FIRST alert fired must still be readable")
	newSnap, err := r.svc.Get(r.h.Ctx, r.scope, uuid.MustParse(after.Snapshot.ID))
	require.NoError(t, err)

	assert.Equal(t, "rate(errors_total[5m]) > 0.05", oldSnap.Expr)
	assert.Equal(t, "rate(errors_total[5m]) > 0.02", newSnap.Expr)
	assert.Equal(t, 300.0, oldSnap.ForSeconds)
	assert.Equal(t, 60.0, newSnap.ForSeconds)
	assert.NotEqual(t, oldSnap.Fingerprint, newSnap.Fingerprint)
	// The labels and annotations survived the JSONB round trip, which the
	// fingerprint is computed over and therefore cannot be allowed to lose.
	assert.Equal(t, map[string]string{"severity": "critical"}, oldSnap.Labels)
	assert.Equal(t, map[string]string{"summary": "error rate high"}, oldSnap.Annotations)
	assert.Equal(t, oldSnap.Fingerprint,
		domain.Fingerprint(oldSnap.Expr, oldSnap.ForSeconds, oldSnap.KeepFiringForSeconds,
			oldSnap.Labels, oldSnap.Annotations),
		"the stored content address must be recomputable from the stored content")

	// --- and the history says the same thing --------------------------------
	key := after.Snapshot.Key
	latest, ok, err := r.svc.Latest(r.h.Ctx, r.scope, key)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, after.Snapshot.Fingerprint, latest.Fingerprint)

	history, err := r.svc.History(r.h.Ctx, r.scope, key)
	require.NoError(t, err)
	require.Equal(t, 2, history.Len(), "three fires, two versions")

	v1, ok := history.At(1)
	require.True(t, ok)
	v2, ok := history.At(2)
	require.True(t, ok)
	assert.Equal(t, before.Snapshot.Fingerprint, v1.Snapshot.Fingerprint, "version 1 is the OLDEST capture")
	assert.Equal(t, after.Snapshot.Fingerprint, v2.Snapshot.Fingerprint)
	assert.True(t, history.Drifted(before.Snapshot.Fingerprint))
	assert.False(t, history.Drifted(after.Snapshot.Fingerprint))

	diff, err := r.svc.DiffVersions(r.h.Ctx, r.scope, key, 1, 2)
	require.NoError(t, err)
	assert.True(t, diff.SameRule)
	assert.True(t, diff.Changed)
	assert.True(t, diff.ExprChanged)
	assert.True(t, diff.ForChanged)
	assert.Equal(t, -240.0, diff.ForDelta)

	// The question the alert card asks: has the rule changed since THIS fired?
	since, ok, err := r.svc.DiffSince(r.h.Ctx, r.scope, key, before.Snapshot.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, before.Snapshot.Fingerprint, since.From.Fingerprint)
	assert.Equal(t, after.Snapshot.Fingerprint, since.To.Fingerprint)

	// And the paginated read the list endpoint uses, newest first.
	page, err := r.svc.ListSnapshots(r.h.Ctx, r.scope, key, db.Keyset{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Snapshots, 2)
	assert.Equal(t, after.Snapshot.Fingerprint, page.Snapshots[0].Fingerprint)
	assert.Equal(t, before.Snapshot.Fingerprint, page.Snapshots[1].Fingerprint)

	// One batch call answers both, which is how the alert list renders `expr`.
	batch, err := r.svc.GetMany(r.h.Ctx, r.scope, []uuid.UUID{
		uuid.MustParse(before.Snapshot.ID),
		uuid.MustParse(after.Snapshot.ID),
		uuid.MustParse(before.Snapshot.ID), // duplicates are the NORMAL case
		id.New(),                           // and an id from nowhere must not blank the page
	})
	require.NoError(t, err)
	assert.Len(t, batch, 2)
}

// TestAFractionalForIsStoredAndDistinguishable is the same drift story over the
// value that used to be truncated.
//
// `for: 1s500ms` and `for: 1s` had ONE content address in the kernel spelling of
// §C.6 before c133981 collapsed the two implementations. `for_seconds` is DOUBLE
// PRECISION, so the database was never the limitation — the pre-image was. This
// asserts the whole round trip: two rules that differ only below the second are
// two rows, and the edit between them is reported as drift.
func TestAFractionalForIsStoredAndDistinguishable(t *testing.T) {
	t.Parallel()

	r := newDBRig(t)

	*r.recovery = recoveredRule("up == 0", 1)
	first := r.fire(t)
	require.True(t, first.NewVersion)

	r.h.Advance(time.Hour)
	*r.recovery = recoveredRule("up == 0", 1.5)
	second := r.fire(t)

	assert.True(t, second.NewVersion, "`for: 1s500ms` is not `for: 1s`")
	assert.True(t, second.Drifted, "a sub-second edit is still an edit, and it must not be invisible")
	assert.NotEqual(t, first.Snapshot.Fingerprint, second.Snapshot.Fingerprint)
	assert.Equal(t, 2, r.rowCount(t))

	stored, err := r.svc.Get(r.h.Ctx, r.scope, uuid.MustParse(second.Snapshot.ID))
	require.NoError(t, err)
	assert.Equal(t, 1.5, stored.ForSeconds, "the fraction survives DOUBLE PRECISION")

	diff, err := r.svc.DiffVersions(r.h.Ctx, r.scope, second.Snapshot.Key, 1, 2)
	require.NoError(t, err)
	assert.True(t, diff.ForChanged)
	assert.InDelta(t, 0.5, diff.ForDelta, 1e-9)
}

// TestAnInvalidSnapshotIsRefusedByStorage answers the second half of the
// question the issue raised.
//
// NewSnapshot returns a Snapshot and no error, so an invalid one IS
// constructible: nothing stops a caller building a snapshot whose
// match_confidence and candidate_count disagree. What this proves is that it
// cannot be PERSISTED without Validate running — the repository calls
// Snapshot.Validate itself before it builds the INSERT, so the CHECK is never
// the thing that says no and a 23514 never reaches the HTTP layer.
//
// The three cases are the invariants a caller is most likely to violate by
// accident: the conf/count pair, an org-less snapshot, and a fingerprint that is
// not 64 lowercase hex.
func TestAnInvalidSnapshotIsRefusedByStorage(t *testing.T) {
	t.Parallel()

	r := newDBRig(t)
	key := domain.Key{SourceID: r.source.String(), Name: "HighErrorRate"}
	at := r.h.Now()

	cases := []struct {
		name   string
		mutate func(s *domain.Snapshot)
	}{
		{
			name: "match_confidence and candidate_count disagree",
			mutate: func(s *domain.Snapshot) {
				s.Confidence, s.CandidateCount = domain.ConfidenceExact, 3
			},
		},
		{
			name:   "no org",
			mutate: func(s *domain.Snapshot) { s.OrgID = "" },
		},
		{
			name:   "a fingerprint that is not 64 lowercase hex",
			mutate: func(s *domain.Snapshot) { s.Fingerprint = "NOT-A-DIGEST" },
		},
		{
			name: "origin=prometheus_api with no prometheus_url",
			mutate: func(s *domain.Snapshot) {
				s.PrometheusURL = ""
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := domain.NewSnapshot(r.scope.OrgID().String(), key,
				recoveredRule("up == 0", 300), at)
			require.NoError(t, snap.Validate(), "the fixture starts valid")

			tc.mutate(&snap)
			require.Error(t, snap.Validate(), "the mutation must actually make it invalid")

			_, _, err := r.repo.Upsert(r.h.Ctx, r.scope, snap)
			require.Error(t, err, "storage must refuse it")
			assert.NotContains(t, err.Error(), "23514",
				"it must be refused in Go, not by a CHECK violation arriving from Postgres")
		})
	}

	assert.Equal(t, 0, r.rowCount(t), "nothing invalid reached the table")
}

// TestSnapshotsAreScopedToTheirOrg: `rule_snapshots_content_uniq` is
// (org_id, source_id, rule_fingerprint), so identical rule text in two tenants is
// two rows and neither can read the other's.
func TestSnapshotsAreScopedToTheirOrg(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewSnapshotRepository(h.Pool)

	type tenant struct {
		scope db.TenantScope
		snap  domain.Snapshot
	}
	tenants := make([]tenant, 0, 2)

	for i := 0; i < 2; i++ {
		org := h.Org()
		cluster := h.Cluster(org)
		source := h.Source(org, cluster)

		snap := domain.NewSnapshot(org.ID.String(),
			domain.Key{SourceID: source.ID.String(), Name: "HighErrorRate"},
			recoveredRule("up == 0", 300), h.Now())
		stored, inserted, err := repo.Upsert(h.Ctx, org.Scope, snap)
		require.NoError(t, err)
		require.True(t, inserted)
		tenants = append(tenants, tenant{scope: org.Scope, snap: stored})
	}

	assert.Equal(t, tenants[0].snap.Fingerprint, tenants[1].snap.Fingerprint,
		"the content address is over the definition and knows nothing about tenants")
	assert.NotEqual(t, tenants[0].snap.ID, tenants[1].snap.ID,
		"...but the uniqueness constraint is org-scoped, so both rows exist")

	// Neither org can read the other's row.
	_, err := repo.Get(h.Ctx, tenants[0].scope, uuid.MustParse(tenants[1].snap.ID))
	require.Error(t, err)
	_, err = repo.Get(h.Ctx, tenants[1].scope, uuid.MustParse(tenants[0].snap.ID))
	require.Error(t, err)

	// And "never captured" is a STATE, not an error.
	_, ok, err := repo.Latest(h.Ctx, tenants[0].scope,
		domain.Key{SourceID: tenants[0].snap.Key.SourceID, Name: "SomeOtherAlert"})
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestHistoryPaginatesWithoutLosingAVersion walks the keyset over a rule that has
// been edited more times than one page holds.
//
// It exists because `listRuleSnapshots` was once served from History(), which is
// capped and returned whole: `next_cursor` was structurally always null and a
// rule edited past the cap had history the API could not reach.
func TestHistoryPaginatesWithoutLosingAVersion(t *testing.T) {
	t.Parallel()

	r := newDBRig(t)

	const edits = 5
	want := make([]string, 0, edits)
	for i := 0; i < edits; i++ {
		*r.recovery = recoveredRule("rate(errors_total[5m]) > 0.0"+string(rune('1'+i)), 300)
		c := r.fire(t)
		require.True(t, c.NewVersion, "edit %d must be a new version", i)
		want = append(want, c.Snapshot.Fingerprint)
		r.h.Advance(time.Hour)
	}

	key := domain.Key{
		SourceID: r.source.String(),
		File:     "/etc/prometheus/rules/checkout.yml",
		Group:    "checkout",
		Name:     "HighErrorRate",
	}

	var got []string
	page := db.Keyset{Limit: 2}
	for i := 0; i < edits+2; i++ {
		out, err := r.svc.ListSnapshots(r.h.Ctx, r.scope, key, page)
		require.NoError(t, err)
		for _, s := range out.Snapshots {
			got = append(got, s.Fingerprint)
		}
		if !out.Cursor.HasMore {
			break
		}
		page.Cursor = out.Cursor
	}

	// Newest first, every version present exactly once.
	require.Len(t, got, edits)
	for i, fp := range got {
		assert.Equal(t, want[edits-1-i], fp, "page position %d", i)
	}
}

// TestCaptureDegradesToAStorableRowWhenNothingIsRecovered: the `unavailable`
// snapshot is not a placeholder the code invents and never writes. It is a real
// row that satisfies rule_snapshots_expr_ck, rule_snapshots_conf_ck and
// rule_snapshots_promurl_ck, and it is what lets the UI say "the rule could not
// be recovered" instead of showing an empty panel.
func TestCaptureDegradesToAStorableRowWhenNothingIsRecovered(t *testing.T) {
	t.Parallel()

	r := newDBRig(t)
	*r.recovery = domain.Recovery{
		Origin:   domain.OriginUnavailable,
		Strategy: domain.StrategyNone,
		RuleName: "HighErrorRate",
		Notes:    []string{"rules_api_no_match"},
	}

	c := r.fire(t)
	assert.False(t, c.Recovered())
	assert.Equal(t, domain.OriginUnavailable, c.Snapshot.Origin)
	assert.NotEmpty(t, c.Snapshot.ID, "it is a ROW, not a placeholder")
	assert.Contains(t, c.Warnings, "rules_api_no_match")
	assert.Equal(t, 1, r.rowCount(t))

	var origin, confidence string
	var count int
	var promURL *string
	require.NoError(t, r.h.Pool.QueryRow(r.h.Ctx,
		`SELECT origin, match_confidence, candidate_count, prometheus_url
		   FROM rule_snapshots WHERE org_id = $1`, r.scope.OrgID()).
		Scan(&origin, &confidence, &count, &promURL))
	assert.Equal(t, "unavailable", origin)
	assert.Equal(t, "none", confidence)
	assert.Equal(t, 0, count)
	assert.Nil(t, promURL)
	require.NoError(t, c.Snapshot.Validate())
}

// TestEveryUnrecoverableRuleInASourceGetsItsOwnRow, against the real SQL rather
// than a fake, because the mechanism IS the SQL.
//
// ⭐ ONE FIREWALLED PROMETHEUS IS THE WHOLE TEST. An `unavailable` capture has an
// empty expr, zero durations and empty maps — rule_snapshots_expr_ck requires
// exactly that shape — so every unavailable capture in a source hashes to the
// SAME content address. `rule_fingerprint` is over the DEFINITION only (SPEC
// §C.6) and cannot be asked to identify a rule as well, which is why 00040 put
// the rule key in `rule_snapshots_content_uniq`.
//
// Keyed on content alone, the upsert's UNION arm handed every later alert the
// incumbent row: one shared "we could not see it" row for every rule in the
// source, named after whichever alert failed first, unreachable to all the
// others because every read path filters on rule_name. What is asserted here is
// the property that fixed it: each rule's failure is ITS failure, stored under
// its own name and findable again under its own name.
func TestEveryUnrecoverableRuleInASourceGetsItsOwnRow(t *testing.T) {
	t.Parallel()

	r := newDBRig(t)
	unavailable := func(name string) domain.Recovery {
		return domain.Recovery{
			Origin:   domain.OriginUnavailable,
			Strategy: domain.StrategyNone,
			RuleName: name,
			Notes:    []string{"rules_api_no_match"},
		}
	}

	fireNamed := func(name string) service.Capture {
		t.Helper()
		*r.recovery = unavailable(name)
		c, err := r.svc.Capture(r.h.Ctx, r.scope, service.CaptureRequest{
			SourceID: r.source,
			AlertID:  id.New(),
			Labels:   map[string]string{"alertname": name},
		})
		require.NoError(t, err)
		return c
	}

	first := fireNamed("AlertA")
	require.True(t, first.NewVersion)
	require.Equal(t, "AlertA", first.Snapshot.Key.Name)

	r.h.Advance(time.Minute)
	second := fireNamed("AlertB")

	assert.Equal(t, first.Snapshot.Fingerprint, second.Snapshot.Fingerprint,
		"every unavailable capture has the same content: nothing")
	assert.Equal(t, 2, r.rowCount(t),
		"two rules, two rows — the uniqueness tuple carries the rule key (00040)")
	assert.NotEqual(t, first.Snapshot.ID, second.Snapshot.ID)
	assert.Equal(t, "AlertB", second.Snapshot.Key.Name,
		"AlertB's capture comes back named AlertB")
	assert.True(t, second.NewVersion,
		"AlertB had never been captured, so its first capture is a new version")
	assert.False(t, second.Drifted, "and it has no predecessor of its own to have drifted from")

	// Each alert can reach its own capture again. This is the assertion the old
	// row-sharing behaviour could not satisfy for anybody but the first alert.
	for name, want := range map[string]service.Capture{"AlertA": first, "AlertB": second} {
		latest, ok, err := r.svc.Latest(r.h.Ctx, r.scope, domain.Key{
			SourceID: r.source.String(), Name: name,
		})
		require.NoError(t, err)
		require.True(t, ok, "%s must be able to reach its own stored capture", name)
		assert.Equal(t, want.Snapshot.ID, latest.ID)
		assert.Equal(t, name, latest.Key.Name)
	}

	// Repeat failures still cost nothing: the SAME rule failing again is the same
	// row, which is what keeps "capture on every fire" affordable for a source
	// whose Prometheus is down for a week.
	r.h.Advance(time.Minute)
	againA := fireNamed("AlertA")
	assert.False(t, againA.NewVersion, "AlertA's second failure is AlertA's first row")
	assert.Equal(t, first.Snapshot.ID, againA.Snapshot.ID)
	assert.Equal(t, 2, r.rowCount(t), "still two rules, still two rows")
}

// TestTheRuleKeySurvivesPrometheusBecomingReachable.
//
// ⭐ AN EMPTY rule_file OR rule_group MEANS "UNKNOWN", ON BOTH SIDES. A
// generatorURL capture knows the expression but not the file it is written in,
// so it is stored with `”` there; when Prometheus becomes reachable the same
// rule arrives with both. `keyPredicate` was lenient about the empty component
// only when the CALLER's key carried it, and the stored `”` was compared as an
// equality — so a query with a now-known file and group missed every row
// captured before it was known, and one rule's history split in two on exactly
// the day the predicate's own comment said it must not.
//
// ⛔ THE REASON IT MATTERED IS NOT TIDINESS. The split showed up as drift
// reported in ONE DIRECTION ONLY: generator_url after prometheus_api found the
// predecessor (that query omits the filters) and reported drift; prometheus_api
// after generator_url found nothing and reported none. Neither direction is a
// rule edit by itself — but the promotion is exactly when a REAL edit is likely
// to be sitting in the same fire, and the silent direction hid it. Here the
// promotion carries an edited `for`, and it must be reported.
func TestTheRuleKeySurvivesPrometheusBecomingReachable(t *testing.T) {
	t.Parallel()

	r := newDBRig(t)

	viaGeneratorURL := domain.Recovery{
		Origin:         domain.OriginGeneratorURL,
		Strategy:       domain.StrategyGeneratorURL,
		Confidence:     domain.ConfidenceExact,
		CandidateCount: 1,
		RuleName:       "HighErrorRate",
		Expr:           "up == 0",
	}

	// Fire 1: the zero-API-call path. No file, no group.
	*r.recovery = viaGeneratorURL
	viaGenerator := r.fire(t)
	require.True(t, viaGenerator.NewVersion)
	assert.Empty(t, viaGenerator.Snapshot.Key.Group)
	assert.Empty(t, viaGenerator.Snapshot.Key.File)

	// Fire 2: Prometheus is reachable now, so the same rule arrives with its file
	// and group — and, at the same moment, an edited `for`.
	r.h.Advance(time.Hour)
	*r.recovery = recoveredRule("up == 0", 300)
	viaAPI := r.fire(t)

	require.True(t, viaAPI.NewVersion)
	assert.Equal(t, 2, r.rowCount(t), "both captures are stored; nothing is lost")

	// ⭐ The fuller key finds the row stored under the emptier one, so the edit
	// that rode in with the promotion is reported rather than swallowed.
	assert.True(t, viaAPI.Drifted,
		"the `for` went from unknown to 300s in a capture of the same rule, and that is drift")
	assert.Equal(t, viaGenerator.Snapshot.Fingerprint, viaAPI.PreviousFingerprint)

	// One rule, ONE history, whichever end of the key it is asked from.
	full, err := r.svc.History(r.h.Ctx, r.scope, viaAPI.Snapshot.Key)
	require.NoError(t, err)
	assert.Equal(t, 2, full.Len(), "the earlier capture of the same rule is still its own history")

	partial, err := r.svc.History(r.h.Ctx, r.scope, viaGenerator.Snapshot.Key)
	require.NoError(t, err)
	assert.Equal(t, 2, partial.Len())
	assert.Equal(t, full.Versions[0].Snapshot.ID, partial.Versions[0].Snapshot.ID,
		"and they are the same two captures in the same order, not two views that happen to be the same size")
	assert.Equal(t, full.Versions[1].Snapshot.ID, partial.Versions[1].Snapshot.ID)
	assert.Equal(t, viaGenerator.Snapshot.ID, full.Versions[0].Snapshot.ID,
		"version 1 is the OLDEST capture, which is the one made before the file was known")

	// ⭐ SYMMETRY. Firing back down the generatorURL path reports drift too, and
	// against the same predecessor pair — the direction of the promotion is not
	// allowed to change the answer.
	r.h.Advance(time.Hour)
	*r.recovery = viaGeneratorURL
	back := r.fire(t)

	assert.False(t, back.NewVersion, "the content is one it has seen before, under a key it has seen it under")
	assert.Equal(t, viaGenerator.Snapshot.ID, back.Snapshot.ID)
	assert.True(t, back.Drifted)
	assert.Equal(t, viaAPI.Snapshot.Fingerprint, back.PreviousFingerprint)
	assert.Equal(t, 2, r.rowCount(t), "three fires, two rule texts")

	// The alert card's question, asked from the capture the first fire was bound
	// to: the rule HAS changed since, and the newest text is the API one.
	since, ok, err := r.svc.DiffSince(r.h.Ctx, r.scope, viaAPI.Snapshot.Key, viaGenerator.Snapshot.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, viaGenerator.Snapshot.Fingerprint, since.From.Fingerprint)
	assert.Equal(t, viaAPI.Snapshot.Fingerprint, since.To.Fingerprint)
	assert.True(t, since.ForChanged)
}
