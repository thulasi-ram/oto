package api

// ⭐⭐ WHAT `change` COMPARES, ASKED OF A REAL DATABASE.
//
// `rules_contract_test.go` proves the handler picks the predecessor out of a set
// of episodes a stub hands it. What a stub cannot show is that the predecessor
// EXISTS as a row and is found by a query — that "the episode before this one,
// that had a rule bound to it" is `alert_cases` ordered by `seq`, and that
// a newer capture of the same rule key, made for somebody else's alert, does not
// become one side of this alert's diff.
//
// So this file runs the real alerts write path, the real rule capture and the
// real HTTP handler over a real migrated Postgres, and edits the rule between
// fires the way a human does: same rule, same recovery path, one number moved.
//
// The scenario both tests share is the one the ticket describes:
//
//	the alert fires under v1, somebody raises the threshold, it fires again under
//	v2, and then the rule is edited a THIRD time and captured for a different
//	alert. `change` must be (v1 → v2) — the edit this alert lived through — and
//	never (v2 → v3), which is an edit no episode of this alert has ever seen.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	alertsrepo "github.com/thulasiram/oto/internal/alerts/repository"
	alerts "github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/rules/domain"
	rulesrepo "github.com/thulasiram/oto/internal/rules/repository"
	"github.com/thulasiram/oto/internal/rules/service"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
	"github.com/thulasiram/oto/test/harness"
)

func TestMain(m *testing.M) { harness.Main(m) }

const (
	driftRuleName = "HighErrorRate"
	// The three texts, oldest first. Only the threshold moves, so every
	// difference the response reports is one this file put there.
	driftExprV1 = `rate(errors_total[5m]) > 0.05`
	driftExprV2 = `rate(errors_total[5m]) > 0.02`
	driftExprV3 = `rate(errors_total[5m]) > 0.01`
)

// driftRig is the rules HTTP surface over the real services it is composed with
// in production: `rules/service` for the snapshots, `alerts/service` for the
// episodes, and this test's own database under both.
type driftRig struct {
	h       *harness.H
	scope   db.TenantScope
	org     harness.Org
	cluster harness.Cluster
	group   harness.Group
	source  uuid.UUID

	alerts *alerts.Service
	rules  *service.Service
	lookup *driftLookup
	c      *apitest.Client
}

func newDriftRig(t *testing.T) *driftRig {
	t.Helper()

	h := harness.New(t)
	org := h.Org()
	cluster := h.Cluster(org)
	source := h.Source(org, cluster)

	r := &driftRig{
		h:       h,
		scope:   org.Scope,
		org:     org,
		cluster: cluster,
		group:   h.Group(org, source, cluster),
		source:  source.ID,
		lookup:  &driftLookup{},
	}

	alertsSvc, err := alerts.New(alerts.Deps{
		Alerts:  alertsrepo.NewAlertRepository(h.Pool, h.Clock, false),
		Cases:   alertsrepo.NewCaseRepository(h.Pool),
		Events:  alertsrepo.NewEventRepository(h.Pool, h.Clock),
		Snoozes: alertsrepo.NewSnoozeRepository(h.Pool, h.Clock),
		Tx:      alertsrepo.NewTxRunner(h.Pool),
		Clock:   h.Clock,
		Logger:  driftLogger(),
	})
	require.NoError(t, err)
	r.alerts = alertsSvc

	// ⚠️ NO EventRecorder. These tests are about a READ, and the timeline append
	// would need `alert_events` partitions around the database's own `now()`
	// rather than around the harness epoch. What the capture NARRATES is asserted
	// in `internal/app/timeline_events_db_test.go`, against real partitions.
	rulesSvc, err := service.New(service.Options{
		Repo:   rulesrepo.NewSnapshotRepository(h.Pool),
		Lookup: r.lookup,
		Clock:  h.Clock,
		Logger: driftLogger(),
	})
	require.NoError(t, err)
	r.rules = rulesSvc

	r.c = apitest.New(NewRouter(rulesSvc, alertsSvc, h.Clock)).As(apitest.MemberOf(org.ID))
	return r
}

// alert seeds one Alert of the shared rule — same `alertname`, so the same
// RuleKey — distinguished by the instance label.
func (r *driftRig) alert(t *testing.T, instance string) harness.Alert {
	t.Helper()
	return r.h.AlertWith(r.org, r.cluster, map[string]string{
		"alertname": driftRuleName,
		"severity":  "critical",
		"service":   instance,
	})
}

// open starts a new episode of a, ending the one that is running.
//
// The episodes are seeded rather than driven through `ObserveBatch` because what
// is under test is a read over them: what matters is that `seq` really is the
// episode ordinal and that `case_one_open_idx` is really satisfied, both of which
// the database enforces here exactly as it does in production.
func (r *driftRig) open(t *testing.T, a harness.Alert, seq int) uuid.UUID {
	t.Helper()

	// ⭐ `closed` PLUS A `resolve_reason`, NOT `resolved` (ADR 0040). `state` says
	// only that the episode ended; `case_resolve_ck` demands it say why, and
	// `upstream` is what the four-way reading spells `resolved`.
	r.h.Exec(`UPDATE alert_cases
	             SET state = 'closed', ended_at = $2, resolve_reason = 'upstream'
	           WHERE alert_id = $1 AND ended_at IS NULL`, a.ID, r.h.Now())

	caseID := id.New()
	now := r.h.Now()
	r.h.Exec(`INSERT INTO alert_cases
	            (id, org_id, alert_id, group_id, seq, state, started_at, last_observed_at,
	             source_starts_at, source_updated_at)
	          VALUES ($1, $2, $3, $4, $5, 'open', $6, $6, $6, $6)`,
		caseID, a.OrgID, a.ID, r.group.ID, seq, now)
	r.h.Exec(`UPDATE alerts SET current_case_id = $1, total_cases = $2, last_seen_at = $3
	           WHERE id = $4`, caseID, seq, now, a.ID)
	return caseID
}

// capture recovers the rule behind an episode and binds it, which is what the
// `prom.rule` enricher does at fire time.
func (r *driftRig) capture(t *testing.T, a harness.Alert, caseID uuid.UUID) service.Capture {
	t.Helper()

	c, err := r.rules.Capture(r.h.Ctx, r.scope, service.CaptureRequest{
		SourceID:    r.source,
		AlertID:     a.ID,
		CaseID:      caseID,
		Labels:      map[string]string{"alertname": driftRuleName, "severity": "critical"},
		Annotations: map[string]string{"summary": "error rate high"},
	})
	require.NoError(t, err)
	require.True(t, c.Recovered(), "the fixture must recover a definition")
	require.NoError(t, r.alerts.BindRuleSnapshot(
		r.h.Ctx, r.scope, caseID, uuid.MustParse(c.Snapshot.ID)))
	return c
}

// fire is one whole episode: it opens, captures and binds.
func (r *driftRig) fire(t *testing.T, a harness.Alert, seq int) service.Capture {
	t.Helper()
	return r.capture(t, a, r.open(t, a, seq))
}

// history reads `GET /alerts/{id}/rule` and validates it against the contract.
func (r *driftRig) history(t *testing.T, a harness.Alert) map[string]any {
	t.Helper()

	resp := r.c.GET("/alerts/"+a.ID.String()+"/rule").MustStatus(t, http.StatusOK)
	schema.Assert(t, "getAlertRuleHistory", http.StatusOK, resp.Body())

	data, ok := resp.JSON(t)["data"].(map[string]any)
	require.True(t, ok, "the body has no data object: %s", resp)
	return data
}

// driftLookup is the upstream Prometheus under the test's control. Reassigning
// `recovery` between two fires IS somebody editing the rule.
type driftLookup struct{ recovery domain.Recovery }

func (l *driftLookup) Lookup(
	context.Context, db.TenantScope, service.LookupRequest,
) (domain.Recovery, error) {
	return l.recovery, nil
}

// driftRule is a complete rules-API recovery: the shape a healthy
// `/api/v1/rules` round trip produces.
func driftRule(expr string) domain.Recovery {
	return domain.Recovery{
		Origin:         domain.OriginPrometheusAPI,
		Strategy:       domain.StrategyRulesAPI,
		Confidence:     domain.ConfidenceExact,
		CandidateCount: 1,
		RuleName:       driftRuleName,
		RuleGroup:      "checkout",
		RuleFile:       "/etc/prometheus/rules/checkout.yml",
		Expr:           expr,
		ForSeconds:     300,
		Labels:         map[string]string{"severity": "critical"},
		Annotations:    map[string]string{"summary": "error rate high"},
		PrometheusURL:  "https://prom.internal",
	}
}

func driftLogger() *slog.Logger {
	// The degradation paths log at Warn and Error by design; a test that prints
	// them is a test nobody reads the output of.
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ⭐⭐ TestTheChangeIsTheEditBetweenTwoEpisodesAndNotTheNewestCapture is the
// ticket, end to end.
//
// The promise: `change` is what moved between the last time this alert fired and
// this time. The rule is edited twice — once between this alert's two episodes,
// once afterwards, captured for another alert of the same rule — and only the
// first edit is one this alert lived through.
//
// What broke when it did not hold: the panel diffed the bound snapshot against
// the NEWEST capture of the rule key, so it reported an edit this alert never saw
// as though the episode had experienced it, named the episode's own row as
// `previous_*`, and — whenever the episode had fired under the newest text —
// reported no change at all, which is the exact case an operator opens the panel
// for.
func TestTheChangeIsTheEditBetweenTwoEpisodesAndNotTheNewestCapture(t *testing.T) {
	t.Parallel()

	r := newDriftRig(t)
	checkout := r.alert(t, "checkout")

	// 12:00 — the first fire, under the original threshold.
	r.lookup.recovery = driftRule(driftExprV1)
	first := r.fire(t, checkout, 1)

	// 13:00 — somebody tightens the threshold, and the alert fires again. THIS is
	// the edit the operator is owed.
	r.h.Advance(time.Hour)
	r.lookup.recovery = driftRule(driftExprV2)
	second := r.fire(t, checkout, 2)
	require.True(t, second.Drifted, "the fixture must be a drift, or the test proves nothing")
	require.NotEqual(t, first.Snapshot.ID, second.Snapshot.ID)

	// 14:00 — the rule is edited AGAIN, and the next fire of it belongs to a
	// different alert: same rule, another instance. `rule_snapshots` now holds a
	// version newer than anything this alert has ever fired under.
	r.h.Advance(time.Hour)
	r.lookup.recovery = driftRule(driftExprV3)
	third := r.fire(t, r.alert(t, "payments"), 1)
	require.NotEqual(t, second.Snapshot.ID, third.Snapshot.ID)

	data := r.history(t, checkout)

	current, ok := data["current"].(map[string]any)
	require.True(t, ok, "current = %v, want the snapshot this episode fired under", data["current"])
	require.Equal(t, second.Snapshot.ID, current["id"])

	change, ok := data["change"].(map[string]any)
	require.True(t, ok,
		"change = %v, want the diff between this episode and the one before it", data["change"])

	// ⭐ The older side is the PREVIOUS EPISODE's snapshot.
	require.Equal(t, first.Snapshot.ID, change["previous_snapshot_id"],
		"previous_snapshot_id must name the episode before this one, not this episode itself")
	require.Equal(t, first.Snapshot.Fingerprint, change["previous_fingerprint"])
	require.Equal(t, driftExprV1, change["previous_expr"])

	// ⭐ The newer side is THIS episode's snapshot, and never the newest capture:
	// v3 is a text no episode of this alert has ever fired under.
	require.Equal(t, driftExprV2, change["new_expr"],
		"new_expr must be the text this episode fired under, not the newest one captured (%s)",
		driftExprV3)
	require.Equal(t, true, change["expr_changed"])
	require.Equal(t, false, change["for_changed"], "only the threshold moved")

	// The third edit is not lost — it is HISTORY, which is where a version this
	// alert never fired under belongs.
	versions, ok := data["versions"].([]any)
	require.True(t, ok)
	require.Len(t, versions, 3, "three distinct texts have been captured for this rule key")
	newest, _ := versions[0].(map[string]any)
	require.Equal(t, third.Snapshot.ID, newest["id"], "versions is newest first")
}

// ⭐ TestAnEpisodeWithNoCaptureIsSteppedOverWhenLookingBack.
//
// The promise: the predecessor is the last episode that had a RULE bound to it.
// An episode oto captured nothing for — the enricher never ran, the alert
// predates rule capture — holds nothing to compare, and stopping at it would
// report "nothing changed" across an edit that plainly happened.
//
// This is `rule_snapshot_id IS NOT NULL` in the lookback query, and only a
// database can show that the clause is there: a middle episode with no snapshot
// is a NULL column, not a missing row.
func TestAnEpisodeWithNoCaptureIsSteppedOverWhenLookingBack(t *testing.T) {
	t.Parallel()

	r := newDriftRig(t)
	checkout := r.alert(t, "checkout")

	r.lookup.recovery = driftRule(driftExprV1)
	first := r.fire(t, checkout, 1)

	// The middle episode: it fired, and nothing was ever captured for it.
	r.h.Advance(time.Hour)
	r.open(t, checkout, 2)

	r.h.Advance(time.Hour)
	r.lookup.recovery = driftRule(driftExprV2)
	third := r.fire(t, checkout, 3)
	require.True(t, third.Drifted)

	change, ok := r.history(t, checkout)["change"].(map[string]any)
	require.True(t, ok,
		"change is null although the rule was edited since the last episode that had one")
	require.Equal(t, first.Snapshot.ID, change["previous_snapshot_id"],
		"the lookback must step over the episode with no capture, not stop at it")
	require.Equal(t, driftExprV1, change["previous_expr"])
	require.Equal(t, driftExprV2, change["new_expr"])
}
