package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	alertsrepo "github.com/thulasiram/oto/internal/alerts/repository"
	alertsservice "github.com/thulasiram/oto/internal/alerts/service"
	enrichdomain "github.com/thulasiram/oto/internal/enrichment/domain"
	enrichrepo "github.com/thulasiram/oto/internal/enrichment/repository"
	enrichservice "github.com/thulasiram/oto/internal/enrichment/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	rulesdomain "github.com/thulasiram/oto/internal/rules/domain"
	rulesrepo "github.com/thulasiram/oto/internal/rules/repository"
	rulesservice "github.com/thulasiram/oto/internal/rules/service"
	"github.com/thulasiram/oto/test/harness"
)

// ⭐⭐ THE FIVE TYPES THAT HAD NO WRITER.
//
// `rules/service.EventRecorder` and `enrichment/service.EventRecorder` were both
// declared, both documented and both CALLED — and nothing in the tree implemented
// either, so `if s.events == nil { return }` was unconditional in a shipped binary
// and five of the thirty-six §D.4.1 types could never appear in `alert_events`:
// rule.snapshot_captured, rule.definition_changed, rule.lookup_failed,
// enrichment.completed and enrichment.failed.
//
// ⛔ THE DEFECT WAS INVISIBLE TO A UNIT TEST, AND THAT IS WHY THESE ARE HERE.
// Every one of those call sites already had a passing test against a fake recorder
// — `rules/service/capture_test.go`'s eventLog and `enrichment/service`'s
// fakeEvents — and both fakes went on passing for as long as production had no
// recorder at all. What a fake cannot ask is "does a row land in `alert_events`",
// so these run the REAL alerts write path against a REAL migrated Postgres and
// read the table back. They also pin the composition: the recorder under test is
// the one `container.go` injects, not a stand-in for it.
//
// ⚠️ WHAT EACH TEST DRIVES IS THE MOMENT, NOT THE METHOD. AC-16 is a claim about
// when a row appears — "when a rule's threshold changes between cases" — so
// the drift test edits a threshold between two captures of the same rule rather
// than asserting that a recorder called with a drift event writes one.

// ------------------------------------------------------------------ fixtures

// timelineRig is the alerts write path plus the two narrators that record onto
// it, wired the way the container wires them.
type timelineRig struct {
	h        *harness.H
	clk      *clock.Fake
	scope    db.TenantScope
	sourceID uuid.UUID
	alertID  uuid.UUID
	caseID   uuid.UUID

	recorder *timelineRecorder
	rules    *rulesservice.Service
	lookup   *stubRuleLookup
}

func newTimelineRig(t *testing.T) *timelineRig {
	t.Helper()

	h := harness.New(t)
	org := h.Org()
	cluster := h.Cluster(org)
	source := h.Source(org, cluster)
	group := h.Group(org, source, cluster)
	alert := h.Alert(org, cluster)
	ac := h.Case(alert, group)

	r := &timelineRig{
		h: h,
		// The harness clock, which is Epoch. It used to be a fake pinned at the
		// wall clock instead, because `alert_events` had no partition covering
		// Epoch and an append there failed with a bare 23514; the harness template
		// now gives Epoch its own partitions (git-bug 6547228), so the rig can use
		// the one clock every other harness test uses.
		clk:      h.Clock,
		scope:    org.Scope,
		sourceID: source.ID,
		alertID:  alert.ID,
		caseID:   ac.ID,
		lookup:   &stubRuleLookup{},
	}

	// The alerts service over its real repositories: the seam under test is
	// `AppendTimelineEvent`, and what it has to satisfy — the closed enum, the C.8
	// key claim, ev_subject_ck, ev_summary_ck — is enforced below it, in SQL.
	alerts, err := alertsservice.New(alertsservice.Deps{
		Alerts:     alertsrepo.NewAlertRepository(h.Pool, r.clk, false),
		Cases:      alertsrepo.NewCaseRepository(h.Pool),
		Events:     alertsrepo.NewEventRepository(h.Pool, r.clk),
		Snoozes:    alertsrepo.NewSnoozeRepository(h.Pool, r.clk),
		Tx:         alertsrepo.NewTxRunner(h.Pool),
		AlertBatch: alertsrepo.NewAlertRepository(h.Pool, r.clk, false),
		OccBatch:   alertsrepo.NewCaseRepository(h.Pool),
		Clock:      r.clk,
		Logger:     quietLogger(),
	})
	require.NoError(t, err)

	// ⭐ THE PRODUCTION ADAPTER, LATE-BOUND EXACTLY AS THE CONTAINER LATE-BINDS IT.
	r.recorder = &timelineRecorder{}
	r.recorder.svc = alerts

	r.rules, err = rulesservice.New(rulesservice.Options{
		Repo:   rulesrepo.NewSnapshotRepository(h.Pool),
		Lookup: r.lookup,
		Events: r.recorder,
		Clock:  r.clk,
		Logger: quietLogger(),
	})
	require.NoError(t, err)
	return r
}

// capture drives one rule capture for this rig's case, the way the
// `prom.rule` enricher drives it in production.
func (r *timelineRig) capture(t *testing.T) rulesservice.Capture {
	t.Helper()
	c, err := r.rules.Capture(context.Background(), r.scope, rulesservice.CaptureRequest{
		SourceID:     r.sourceID,
		AlertID:      r.alertID,
		CaseID:       r.caseID,
		Labels:       map[string]string{"alertname": "HighErrorRate", "severity": "critical"},
		Annotations:  map[string]string{"summary": "error rate high"},
		GeneratorURL: "https://prom.internal/graph?g0.expr=up+%3D%3D+0&g0.tab=1",
	})
	require.NoError(t, err)
	return c
}

// enrichment builds the pipeline over this rig's case with one enricher.
func (r *timelineRig) enrichment(t *testing.T, e enrichdomain.Enricher) *enrichservice.Service {
	t.Helper()

	registry, err := enrichservice.NewRegistry(e)
	require.NoError(t, err)

	svc, err := enrichservice.New(enrichservice.Options{
		Registry: registry,
		Repo:     enrichrepo.NewEnrichmentRepository(r.h.Pool),
		Subjects: stubSubjects{alertID: r.alertID, caseID: r.caseID},
		Events:   r.recorder,
		Clock:    r.clk,
		Logger:   quietLogger(),
	})
	require.NoError(t, err)
	return svc
}

// timelineRow is one `alert_events` row, read back as the client reads it.
type timelineRow struct {
	Type       string
	ActorKind  string
	ActorID    string
	ActorLabel string
	Summary    string
	Payload    map[string]any
	AlertID    uuid.UUID
	DedupeKey  string
}

// events reads every timeline row for this rig's case, in timeline order.
func (r *timelineRig) events(t *testing.T) []timelineRow {
	t.Helper()

	rows, err := r.h.Pool.Query(r.h.Ctx, `
		SELECT type, actor_kind, coalesce(actor_id, ''), coalesce(actor_label, ''),
		       summary, payload, alert_id, coalesce(dedupe_key, '')
		  FROM alert_events
		 WHERE case_id = $1
		 ORDER BY recorded_at, id`, r.caseID)
	require.NoError(t, err)
	defer rows.Close()

	var out []timelineRow
	for rows.Next() {
		var ev timelineRow
		var raw []byte
		require.NoError(t, rows.Scan(&ev.Type, &ev.ActorKind, &ev.ActorID, &ev.ActorLabel,
			&ev.Summary, &raw, &ev.AlertID, &ev.DedupeKey))
		require.NoError(t, json.Unmarshal(raw, &ev.Payload))
		out = append(out, ev)
	}
	require.NoError(t, rows.Err())
	return out
}

// only returns the single row of a type, and fails if there is not exactly one.
// "Exactly one" is half of every assertion here: a narrator that wrote the same
// fact twice would be a dedupe key that does not dedupe.
func only(t *testing.T, rows []timelineRow, typ string) timelineRow {
	t.Helper()
	var found []timelineRow
	for _, row := range rows {
		if row.Type == typ {
			found = append(found, row)
		}
	}
	require.Len(t, found, 1, "expected exactly one %s row, got %d of them in %v",
		typ, len(found), typesOf(rows))
	return found[0]
}

func typesOf(rows []timelineRow) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Type)
	}
	return out
}

func quietLogger() *slog.Logger {
	// The degradation paths log at Warn and Error by design; a test that prints
	// them is a test nobody reads the output of.
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubRuleLookup is the upstream Prometheus, under the test's control. Editing
// `recovery` between two captures IS "somebody lowered the threshold".
type stubRuleLookup struct {
	recovery rulesdomain.Recovery
	err      error
}

func (l *stubRuleLookup) Lookup(
	context.Context, db.TenantScope, rulesservice.LookupRequest,
) (rulesdomain.Recovery, error) {
	return l.recovery, l.err
}

// recoveredRule is a complete rules-API recovery: the shape a healthy
// `/api/v1/rules` round trip produces.
func recoveredRule(expr string, forSeconds float64) rulesdomain.Recovery {
	return rulesdomain.Recovery{
		Origin:         rulesdomain.OriginPrometheusAPI,
		Strategy:       rulesdomain.StrategyRulesAPI,
		Confidence:     rulesdomain.ConfidenceExact,
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

// stubSubjects is `enrichment/service.SubjectLoader`. The real one is
// `subjectLoader` in adapters.go, which needs the alerts, grouping and sources
// services; what these tests are about is what the pipeline NARRATES, and the
// subject only has to be well-formed for that.
type stubSubjects struct {
	alertID uuid.UUID
	caseID  uuid.UUID
}

func (s stubSubjects) LoadSubject(
	_ context.Context, scope db.TenantScope, caseID uuid.UUID,
) (enrichservice.Loaded, error) {
	return enrichservice.Loaded{
		Subject: enrichdomain.Subject{
			OrgID:       scope.OrgID().String(),
			SubjectKind: enrichdomain.SubjectCase,
			SubjectID:   caseID.String(),
			Alert: enrichdomain.AlertSnapshot{
				ID:        s.alertID.String(),
				AlertName: "HighErrorRate",
				Severity:  "critical",
			},
			Case: enrichdomain.CaseSnapshot{
				ID: caseID.String(), Seq: 1, State: "firing",
			},
		},
		AlertID: s.alertID,
	}, nil
}

// stubEnricher is one inline enricher whose outcome the test dictates.
type stubEnricher struct {
	name string
	res  enrichdomain.Result
	err  error
}

func (e stubEnricher) Name() string                          { return e.name }
func (e stubEnricher) Version() int                          { return 1 }
func (e stubEnricher) Phase() enrichdomain.Phase             { return enrichdomain.PhaseInline }
func (e stubEnricher) Timeout() time.Duration                { return time.Second }
func (e stubEnricher) Applicable(*enrichdomain.Subject) bool { return true }
func (e stubEnricher) Enrich(
	context.Context, *enrichdomain.Subject,
) (enrichdomain.Result, error) {
	return e.res, e.err
}

// ------------------------------------------------------------------- rule.*

// TestRuleCaptureWritesTheSnapshotEvent is T12's first half: a capture that
// recovered a definition says so on the timeline, at the moment it is stored.
func TestRuleCaptureWritesTheSnapshotEvent(t *testing.T) {
	t.Parallel()

	r := newTimelineRig(t)
	r.lookup.recovery = recoveredRule(`rate(errors[5m]) > 0.02`, 300)

	capture := r.capture(t)
	require.True(t, capture.Recovered(), "the fixture must recover a definition")

	ev := only(t, r.events(t), "rule.snapshot_captured")
	require.Equal(t, r.alertID, ev.AlertID, "the event names the alert it is about")
	require.Equal(t, "enricher", ev.ActorKind, "§D.4.1's actor for a capture")
	require.NotEmpty(t, ev.Summary)
	require.Equal(t, capture.Snapshot.ID, ev.Payload["snapshot_id"],
		"the timeline names the snapshot the sentence is about; alert_events has no column for it")
	require.Equal(t, "prometheus_api", ev.Payload["origin"])
	require.Equal(t, "exact", ev.Payload["confidence"])
	require.NotEmpty(t, ev.DedupeKey, "C.8: the append is idempotent or it is not an append")
}

// ⭐⭐ TestThresholdChangeBetweenCasesWritesTheDiff IS ACCEPTANCE CRITERION
// 16, or the half of it that lives in this repository's timeline:
//
//	"When a rule's threshold changes between cases, the alert timeline shows
//	 `rule.definition_changed` with a diff, and Slack receives a `rule_changed`
//	 thread reply — regardless of channel verbosity."
//
// So the test edits the threshold the way a human does — same rule, same
// recovery path, one number moved — and then asks the table. The diff is asserted
// as CONTENT, not as presence: a `rule.definition_changed` row whose payload does
// not carry the two expressions is a timeline entry that says "something changed"
// and leaves the operator to go and find out what, which is the product's headline
// claim reduced to a notification.
func TestThresholdChangeBetweenCasesWritesTheDiff(t *testing.T) {
	t.Parallel()

	r := newTimelineRig(t)

	const before = `rate(errors[5m]) > 0.02`
	const after = `rate(errors[5m]) > 0.05`

	r.lookup.recovery = recoveredRule(before, 300)
	first := r.capture(t)

	// The edit. Between the two fires, somebody lowers the threshold.
	r.clk.Advance(time.Hour)
	r.lookup.recovery = recoveredRule(after, 300)
	second := r.capture(t)
	require.True(t, second.Drifted, "the fixture must be a drift, or the test proves nothing")

	rows := r.events(t)
	ev := only(t, rows, "rule.definition_changed")

	require.Equal(t, "enricher", ev.ActorKind)
	require.Contains(t, ev.Summary, "HighErrorRate")
	require.Equal(t, before, ev.Payload["previous_expr"], "the diff carries what the rule WAS")
	require.Equal(t, after, ev.Payload["new_expr"], "and what it now is")
	require.Equal(t, true, ev.Payload["expr_changed"])
	require.Equal(t, false, ev.Payload["for_changed"], "only the threshold moved")
	require.Equal(t, first.Snapshot.Fingerprint, ev.Payload["previous_fingerprint"],
		"the diff addresses the predecessor by content, not by position")
	require.Equal(t, second.Snapshot.Fingerprint, ev.Payload["fingerprint"])
	require.Equal(t, first.Snapshot.ID, ev.Payload["previous_snapshot_id"])

	// The capture event is still written for the second fire: drift is an
	// ADDITIONAL fact, not a replacement for "this is the rule that fired".
	captured := 0
	for _, row := range rows {
		if row.Type == "rule.snapshot_captured" {
			captured++
		}
	}
	require.Equal(t, 2, captured,
		"each fire records the rule it was bound to; the second also records the edit")
}

// TestUnrecoverableRuleWritesTheLookupFailure is the degraded path, which is a
// RECORDED fact and not silence: "we looked and could not see it" is exactly what
// an operator staring at a rule panel with nothing in it needs told.
func TestUnrecoverableRuleWritesTheLookupFailure(t *testing.T) {
	t.Parallel()

	r := newTimelineRig(t)
	// A lookup that recovers nothing returns a ZERO Recovery and a nil error —
	// the port's documented contract for "Prometheus is down".
	r.lookup.recovery = rulesdomain.Recovery{}

	capture := r.capture(t)
	require.False(t, capture.Recovered())

	rows := r.events(t)
	ev := only(t, rows, "rule.lookup_failed")
	require.Equal(t, "enricher", ev.ActorKind)
	require.Equal(t, "HighErrorRate", ev.Payload["rule_name"])

	for _, row := range rows {
		require.NotEqual(t, "rule.snapshot_captured", row.Type,
			"a capture that recovered nothing must not claim it captured a rule")
	}
}

// ------------------------------------------------------------- enrichment.*

// TestEnrichmentRunWritesTheCompletedEvent is T11: an Enricher completes, and the
// timeline says so once for the PHASE — not once per enricher, which is the
// coalescing rule §F.3 exists for.
func TestEnrichmentRunWritesTheCompletedEvent(t *testing.T) {
	t.Parallel()

	r := newTimelineRig(t)
	svc := r.enrichment(t, stubEnricher{
		name: "test.context",
		res: enrichdomain.Result{
			Status:  enrichdomain.StatusOK,
			Payload: map[string]any{"note": "something useful"},
		},
	})

	res, err := svc.Run(context.Background(), r.scope, enrichservice.RunRequest{
		CaseID: r.caseID,
		Phase:  enrichdomain.PhaseInline,
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.Succeeded())

	ev := only(t, r.events(t), "enrichment.completed")
	require.Equal(t, r.alertID, ev.AlertID)
	require.Equal(t, "enricher", ev.ActorKind)
	require.NotEmpty(t, ev.Summary)

	detail, ok := ev.Payload["test.context"].(map[string]any)
	require.True(t, ok, "the payload carries per-enricher provenance, got %v", ev.Payload)
	require.Equal(t, "ok", detail["status"])
}

// TestEnrichmentFailureWritesTheFailedEvent is the other half, and the reason
// `enrichment.failed` is a type at all: a failed enrichment and a missing one must
// stay distinguishable, in the timeline as well as in `enrichments`.
func TestEnrichmentFailureWritesTheFailedEvent(t *testing.T) {
	t.Parallel()

	r := newTimelineRig(t)
	svc := r.enrichment(t, stubEnricher{
		name: "test.broken",
		// A plain error, not a deadline: this test is about an enricher that
		// FAILED, and a timeout is the pipeline's other, separately-handled shape.
		err: errors.New("upstream refused the connection"),
	})

	_, err := svc.Run(context.Background(), r.scope, enrichservice.RunRequest{
		CaseID: r.caseID,
		Phase:  enrichdomain.PhaseInline,
	})
	require.NoError(t, err, "an enricher that fails is a recorded result, never a failed run")

	rows := r.events(t)
	ev := only(t, rows, "enrichment.failed")
	require.Equal(t, "enricher", ev.ActorKind)
	require.NotEmpty(t, ev.Summary)

	for _, row := range rows {
		require.NotEqual(t, "enrichment.completed", row.Type,
			"a phase that produced no context must not report completion")
	}
}
