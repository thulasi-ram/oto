package api

// THE THREE ID-ADDRESSED RULE READS, CHECKED AGAINST THE CONTRACT ITSELF.
//
//	getRuleSnapshot     GET /api/v1/rule-snapshots/{id}
//	getCaseRule   GET /api/v1/cases/{id}/rule
//	getAlertRuleHistory GET /api/v1/alerts/{id}/rule
//
// ⭐ NOTHING HERE RE-STATES A RESPONSE SHAPE BY HAND. Every success body goes to
// `schema.Assert`, which compiles the JSON Schema `api/openapi/openapi.yaml`
// declares for that operationId and that status and validates the bytes the
// handler actually wrote. A test that spelled the shape out a second time would
// be a second copy of the contract, and a second copy drifts exactly the way the
// DTOs did.
//
// The properties this file protects, and what broke when each did not hold:
//
//   - all three take a path id, so all three can leak across tenants. Another
//     org's id must answer 404 — never 403, which confirms the row exists, and
//     never somebody else's rule text. The fakes 404 every id they do not own,
//     which is exactly what `WHERE org_id = $scope AND id = $id` does: no row,
//     not a forbidden one;
//   - a `404` on the case rule means NO SNAPSHOT COULD BE CAPTURED AT ALL.
//     A stored snapshot whose `origin` is `unavailable` is a DIFFERENT fact — the
//     capture was attempted and honestly recorded as empty — and it is a 200.
//     Collapsing the two would turn "we looked and could not see it" into "there
//     is nothing here", which is the one distinction the rules module exists for;
//   - the history is bound to the EPISODE, not to the newest text upstream: an
//     alert from six weeks ago must still report the threshold that was in force
//     when it fired;
//   - `meta.request_id` is present, because it is REQUIRED by the contract and
//     `httpx.Meta` omits it when empty.
//
// Names in here are all prefixed `ruleContract`/`stub` because `batch_test.go`
// already owns `fakeRules`, `fakeAlerts` and `snapshotFixture` at package scope.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	alertdomain "github.com/thulasiram/oto/internal/alerts/domain"
	alerts "github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/rules/domain"
	"github.com/thulasiram/oto/internal/rules/service"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// The fixture ids are CONSTANTS, never freshly generated: a tenant-scoping
// failure has to be reproducible from the failure message, and the whole point
// of the probe is that the message names the exact id that leaked.
var (
	// ruleContractOldSnapshotID is the text the episode under test fired under.
	ruleContractOldSnapshotID = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b77")
	// ruleContractNewSnapshotID is the text the rule was edited to afterwards.
	ruleContractNewSnapshotID = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b78")
	// ruleContractBlindSnapshotID is a capture that was ATTEMPTED and recorded
	// as empty — `origin: unavailable`. It is not a missing row.
	ruleContractBlindSnapshotID = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b79")

	ruleContractSourceID  = uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	ruleContractAlertID   = uuid.MustParse("0198f3c2-1111-7111-8111-111111111111")
	ruleContractClusterID = uuid.MustParse("0198f3c2-2222-7222-8222-222222222222")
	ruleContractGroupID   = uuid.MustParse("0198f3c2-3333-7333-8333-333333333333")

	// The four episodes of the alert under test, oldest first: one bound to
	// nothing at all (seq 1), one bound to an `unavailable` capture (seq 2), the
	// PREVIOUS episode bound to the old text (seq 3), and the episode under test
	// (seq 4).
	//
	// ⭐ THE PREDECESSOR IS PART OF THE FIXTURE BECAUSE IT IS PART OF THE ANSWER.
	// `change` is the diff between what the previous episode fired under and what
	// this one fired under, so a world with one episode in it cannot tell a
	// correct diff from a diff against the newest text upstream.
	ruleContractCaseID         = uuid.MustParse("0198f3c3-1111-7111-8111-111111111111")
	ruleContractBlindCaseID    = uuid.MustParse("0198f3c3-2222-7222-8222-222222222222")
	ruleContractBareCaseID     = uuid.MustParse("0198f3c3-3333-7333-8333-333333333333")
	ruleContractPreviousCaseID = uuid.MustParse("0198f3c3-4444-7444-8444-444444444444")
)

const (
	ruleContractAlertName = "HighErrorRate"
	ruleContractBlindName = "DiskFillingUp"
	ruleContractPromURL   = "https://prometheus.example.com"

	// ruleContractGeneratorURL is the link Prometheus puts on an alert: it
	// carries `g0.expr`, which is the whole input to the zero-API-call recovery
	// path and the reason a rule can be captured without reaching Prometheus.
	ruleContractGeneratorURL = "https://prometheus.example.com/graph?g0.expr=up"
	// ruleContractBareGeneratorURL is a generatorURL with NO EXPRESSION IN IT,
	// which is entirely ordinary — a Grafana-sourced alert, a hand-fired one, an
	// Alertmanager pointed at a Prometheus that is unreachable. Nothing can be
	// recovered from it, so nothing is bound to the episode.
	ruleContractBareGeneratorURL = "https://grafana.example.com/alerting/grafana/abc123/view"
	ruleContractOldExpr          = `sum(rate(http_requests_total{code=~"5.."}[5m])) / sum(rate(http_requests_total[5m])) > 0.05`
	ruleContractNewExpr          = `sum(rate(http_requests_total{code=~"5.."}[5m])) / sum(rate(http_requests_total[5m])) > 0.1`
)

// ruleContractEpoch is when the episode under test fired. The rule was edited
// three days later, which is what makes `versions` two entries long and the
// bound snapshot the OLDER of them.
var ruleContractEpoch = time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)

/* -------------------------------------------------------------------------- */
/* Fixtures                                                                   */
/* -------------------------------------------------------------------------- */

// ruleContractKey is the identity across which drift is detected:
// `(source_id, rule_file, rule_group, rule_name)` and never the alert name
// alone, because two files can define the same one.
func ruleContractKey() domain.Key {
	return domain.Key{
		SourceID: ruleContractSourceID.String(),
		File:     "/etc/prometheus/rules/api-slo.yaml",
		Group:    "api-slo",
		Name:     ruleContractAlertName,
	}
}

// ruleContractSnapshot builds a capture through the REAL domain constructor, so
// the fingerprint is the genuine content address of the definition and the
// origin/confidence/candidate-count triple is one the DDL would actually accept.
//
// A fixture of zero values would validate against the schema and prove only that
// the empty case validates: the contract asserts `format: uuid`, `format:
// date-time`, a 64-hex `rule_fingerprint` and two closed enums, and none of
// those is exercised by a blank row.
func ruleContractSnapshot(t *testing.T, id uuid.UUID, expr string, capturedAt time.Time) domain.Snapshot {
	t.Helper()

	s := domain.NewSnapshot(apitest.OrgID.String(), ruleContractKey(), domain.Recovery{
		Origin:               domain.OriginPrometheusAPI,
		Strategy:             domain.StrategyRulesAPI,
		Confidence:           domain.ConfidenceExact,
		CandidateCount:       1,
		RuleName:             ruleContractAlertName,
		RuleGroup:            "api-slo",
		RuleFile:             "/etc/prometheus/rules/api-slo.yaml",
		Expr:                 expr,
		ForSeconds:           600,
		KeepFiringForSeconds: 120,
		Labels:               map[string]string{"severity": "critical", "team": "payments"},
		Annotations: map[string]string{
			"summary":     "Error rate above 5% for 10m",
			"runbook_url": "https://runbooks.example.com/HighErrorRate",
		},
		PrometheusURL: ruleContractPromURL,
	}, capturedAt)
	s.ID = id.String()

	// ⭐ The fixture is one the write path would have accepted. A fixture that
	// could never have been stored proves nothing about the read.
	if err := s.Validate(); err != nil {
		t.Fatalf("the snapshot fixture is not one the domain would store: %v", err)
	}
	return s
}

// ruleContractBlindSnapshot is the capture that WAS attempted and came back
// empty. `origin: unavailable` with an empty `expr` is a legal, storable row —
// the DDL binds the pair in both directions — and the contract returns it with a
// 200. It is the fact a 404 must never be confused with.
func ruleContractBlindSnapshot(t *testing.T) domain.Snapshot {
	t.Helper()

	s := domain.NewSnapshot(apitest.OrgID.String(), domain.Key{
		SourceID: ruleContractSourceID.String(),
		Name:     ruleContractBlindName,
	}, domain.Recovery{
		Origin:     domain.OriginUnavailable,
		Strategy:   domain.StrategyNone,
		Confidence: domain.ConfidenceNone,
		RuleName:   ruleContractBlindName,
		Notes:      []string{"no_prometheus_url", "no_generator_url"},
	}, ruleContractEpoch.Add(time.Hour))
	s.ID = ruleContractBlindSnapshotID.String()

	if err := s.Validate(); err != nil {
		t.Fatalf("the unavailable-capture fixture is not one the domain would store: %v", err)
	}
	if s.Origin != domain.OriginUnavailable {
		t.Fatalf("origin = %q, want unavailable — the fixture must exercise the empty capture", s.Origin)
	}
	return s
}

// ruleContractAlertFrom builds the alert through `alerts/domain`, so `alert_key`
// and the label set are the real derivations rather than strings that look like
// them.
//
// The `generatorURL` is a parameter because it is the whole input to the
// zero-API-call recovery path, and therefore the whole difference between an
// alert oto can capture a rule for and one it cannot — which is the distinction
// 015b25b turned from a 422 into an honest empty history.
func ruleContractAlertFrom(t *testing.T, generatorURL string) alertdomain.Alert {
	t.Helper()

	labels, err := alertdomain.NewLabelSet(map[string]string{
		"alertname": ruleContractAlertName,
		"severity":  "critical",
		"namespace": "payments",
		"job":       "api",
	})
	if err != nil {
		t.Fatalf("build the label set: %v", err)
	}
	clusterKey, err := alertdomain.NewClusterKey("prod-eu")
	if err != nil {
		t.Fatalf("build the cluster key: %v", err)
	}

	a, err := alertdomain.NewAlert(alertdomain.AlertParams{
		ID:                ruleContractAlertID,
		OrgID:             apitest.OrgID,
		ClusterID:         ruleContractClusterID,
		Key:               alertdomain.ComputeAlertKey(apitest.OrgID, clusterKey, labels, nil),
		Fingerprint:       alertdomain.ComputeSourceFingerprint(labels),
		ClusterKey:        clusterKey,
		Labels:            labels,
		GeneratorURL:      generatorURL,
		State:             alertdomain.StateFiring,
		CurrentCaseID:     ruleContractCaseID,
		FirstSeenAt:       ruleContractEpoch,
		LastSeenAt:        ruleContractEpoch.Add(5 * time.Minute),
		LastStateChangeAt: ruleContractEpoch,
		TotalCases:        4,
	})
	if err != nil {
		t.Fatalf("build the alert: %v", err)
	}
	return a
}

// ruleContractCase builds one episode bound to snapID. A uuid.Nil snapID
// is the "nothing could be captured" case the contract spells 404.
func ruleContractCase(t *testing.T, id uuid.UUID, seq int, snapID uuid.UUID) alertdomain.Case {
	t.Helper()

	started := ruleContractEpoch.Add(time.Duration(seq) * time.Hour)
	o, err := alertdomain.NewCase(alertdomain.CaseParams{
		ID:              id,
		OrgID:           apitest.OrgID,
		AlertID:         ruleContractAlertID,
		GroupID:         ruleContractGroupID,
		Seq:             seq,
		State:           alertdomain.StateFiring,
		AckState:        alertdomain.AckStateUnacked,
		StartedAt:       started,
		LastObservedAt:  started.Add(2 * time.Minute),
		SourceStartsAt:  started,
		SourceUpdatedAt: started.Add(time.Minute),
		StateVersion:    1,
		RuleSnapshotID:  snapID,
	})
	if err != nil {
		t.Fatalf("build case %s: %v", id, err)
	}
	return o
}

/* -------------------------------------------------------------------------- */
/* Doubles                                                                    */
/* -------------------------------------------------------------------------- */

// stubRuleReads is a RuleService that owns a fixed set of snapshots and 404s
// EVERY id it does not own.
//
// ⛔ That is what makes the tenant probe honest. The real repository queries
// `WHERE org_id = $scope AND id = $id`, so another tenant's id returns no row —
// which is a 404 and not a 403. A double that answered 403, or that ignored the
// id, would let a handler pass the probe by accident.
type stubRuleReads struct {
	snaps map[uuid.UUID]domain.Snapshot
	// hist is the per-key capture set, oldest first, as the repository would
	// return it.
	hist map[domain.Key][]domain.Snapshot

	getCalls     int
	getIDs       []uuid.UUID
	historyCalls int
	getErr       error
}

func (s *stubRuleReads) Get(_ context.Context, _ db.TenantScope, id uuid.UUID) (domain.Snapshot, error) {
	s.getCalls++
	s.getIDs = append(s.getIDs, id)
	if s.getErr != nil {
		return domain.Snapshot{}, s.getErr
	}
	snap, ok := s.snaps[id]
	if !ok {
		return domain.Snapshot{}, errs.NotFound("not_found", "no such rule snapshot")
	}
	return snap, nil
}

func (s *stubRuleReads) GetMany(
	_ context.Context, _ db.TenantScope, ids []uuid.UUID,
) ([]domain.Snapshot, error) {
	out := make([]domain.Snapshot, 0, len(ids))
	for _, id := range ids {
		if snap, ok := s.snaps[id]; ok {
			out = append(out, snap)
		}
	}
	return out, nil
}

// History mirrors the REPOSITORY, refusals included.
//
// ⛔ THIS REFUSAL IS THE GATE THAT WAS MISSING. `rule_snapshots` is addressed on
// `(org_id, source_id, rule_name, …)`, so `repository.keyPredicate` parses
// `key.SourceID` and returns `422 rules_invalid_id` — "a rule key's source id
// must be a UUID" — when it cannot. A double that answered an empty history for
// a key with no source id was MORE FORGIVING THAN PRODUCTION, and that gap is
// precisely where the bug lived: the handler asked for the history of the
// alertname-only key it derives when nothing was captured, the stub said "no
// versions", the test went green, and a real server answered 422 for four
// alerts in five.
func (s *stubRuleReads) History(
	_ context.Context, _ db.TenantScope, key domain.Key,
) (domain.History, error) {
	s.historyCalls++
	if _, err := uuid.Parse(key.SourceID); err != nil {
		return domain.History{}, errs.New(errs.KindValidation, "rules_invalid_id",
			"a rule key's source id must be a UUID")
	}
	return domain.NewHistory(key, s.hist[key]), nil
}

func (s *stubRuleReads) ListSnapshots(
	context.Context, db.TenantScope, domain.Key, db.Keyset,
) (service.SnapshotPage, error) {
	return service.SnapshotPage{}, nil
}

// stubAlertReads is the cross-domain port. It owns one alert and a handful of
// episodes and, like the repository behind it, 404s everything else.
type stubAlertReads struct {
	detail alerts.AlertDetail
	acs    map[uuid.UUID]alertdomain.Case

	alertCalls int
	occCalls   int
	prevCalls  int
}

func (s *stubAlertReads) Get(
	_ context.Context, _ db.TenantScope, alertID uuid.UUID,
) (alerts.AlertDetail, error) {
	s.alertCalls++
	if alertID != s.detail.Alert.ID() {
		return alerts.AlertDetail{}, errs.NotFound("not_found", "no such resource")
	}
	return s.detail, nil
}

func (s *stubAlertReads) GetCase(
	_ context.Context, _ db.TenantScope, caseID uuid.UUID,
) (alertdomain.Case, error) {
	s.occCalls++
	ac, ok := s.acs[caseID]
	if !ok {
		return alertdomain.Case{}, errs.NotFound("not_found", "no such resource")
	}
	return ac, nil
}

// PreviousCaseWithRule mirrors `repository.PreviousWithRuleSnapshot`
// predicate for predicate — same alert, `seq` strictly lower, a rule snapshot
// bound, highest `seq` wins — because the whole point of the read is WHICH
// episode it picks. A double that returned "the one the test meant" would prove
// the handler asked a question, not that the answer is the predecessor.
func (s *stubAlertReads) PreviousCaseWithRule(
	_ context.Context, _ db.TenantScope, alertID uuid.UUID, beforeSeq int,
) (alertdomain.Case, bool, error) {
	s.prevCalls++

	var (
		best  alertdomain.Case
		found bool
	)
	for _, ac := range s.acs {
		if ac.AlertID() != alertID || ac.Seq() >= beforeSeq {
			continue
		}
		if ac.RuleSnapshotID() == uuid.Nil {
			continue
		}
		if !found || ac.Seq() > best.Seq() {
			best, found = ac, true
		}
	}
	return best, found, nil
}

/* -------------------------------------------------------------------------- */
/* Harness                                                                    */
/* -------------------------------------------------------------------------- */

type ruleContractFixture struct {
	rules  *stubRuleReads
	reader *stubAlertReads
	c      *apitest.Client

	oldSnap   domain.Snapshot
	newSnap   domain.Snapshot
	blindSnap domain.Snapshot
}

// newRuleContractFixture builds the world every test here reads from:
//
//	the rule was captured at the epoch, the previous episode AND the episode
//	under test both fired under THAT text, and three days later — after both
//	fires — somebody doubled the threshold.
//
// So the alert's current case is bound to the OLDER of two versions, which
// is the only arrangement in which "the rule as it was when this fired" and "the
// rule as it is now" can be told apart at all. `change` is null in this world and
// SHOULD be: nothing moved between the two fires, and the later edit is a fact
// `versions` carries, not a drift either episode experienced.
//
// The clock is the REAL one on purpose: `meta.elapsed_ms` is derived from
// `time.Since(started)` and a fake epoch would make every success body report an
// elapsed time of several months.
func newRuleContractFixture(t *testing.T) *ruleContractFixture {
	t.Helper()
	return newRuleContractFixtureBoundTo(t, ruleContractOldSnapshotID)
}

// newRuleContractFixtureBoundTo is newRuleContractFixture with the current
// episode bound to a chosen snapshot, so the "this episode fired under the
// newest text" arrangement is reachable too.
func newRuleContractFixtureBoundTo(t *testing.T, boundSnapshot uuid.UUID) *ruleContractFixture {
	t.Helper()
	return newRuleContractFixtureFor(t, boundSnapshot, ruleContractGeneratorURL)
}

// newRuleContractFixtureFor is newRuleContractFixtureBoundTo with the alert's
// `generatorURL` chosen too, so the ordinary "there was never anything to
// capture" world — no expression in the URL, nothing bound to the episode — is
// reachable as the single arrangement it really is.
func newRuleContractFixtureFor(
	t *testing.T, boundSnapshot uuid.UUID, generatorURL string,
) *ruleContractFixture {
	t.Helper()

	oldSnap := ruleContractSnapshot(t, ruleContractOldSnapshotID, ruleContractOldExpr, ruleContractEpoch)
	newSnap := ruleContractSnapshot(t, ruleContractNewSnapshotID, ruleContractNewExpr,
		ruleContractEpoch.Add(72*time.Hour))
	blindSnap := ruleContractBlindSnapshot(t)

	if oldSnap.Fingerprint == newSnap.Fingerprint {
		t.Fatal("the two versions content-address the same; the fixture cannot show drift")
	}

	rules := &stubRuleReads{
		snaps: map[uuid.UUID]domain.Snapshot{
			ruleContractOldSnapshotID:   oldSnap,
			ruleContractNewSnapshotID:   newSnap,
			ruleContractBlindSnapshotID: blindSnap,
		},
		hist: map[domain.Key][]domain.Snapshot{
			ruleContractKey(): {oldSnap, newSnap},
		},
	}

	current := ruleContractCase(t, ruleContractCaseID, 4, boundSnapshot)
	reader := &stubAlertReads{
		detail: alerts.AlertDetail{
			Alert:       ruleContractAlertFrom(t, generatorURL),
			CurrentCase: &current,
			LatestCase:  &current,
		},
		acs: map[uuid.UUID]alertdomain.Case{
			ruleContractCaseID: current,
			// The episode BEFORE the one under test. It fired under the old
			// text, which is the text `change` must measure from.
			ruleContractPreviousCaseID: ruleContractCase(
				t, ruleContractPreviousCaseID, 3, ruleContractOldSnapshotID),
			ruleContractBlindCaseID: ruleContractCase(
				t, ruleContractBlindCaseID, 2, ruleContractBlindSnapshotID),
			ruleContractBareCaseID: ruleContractCase(
				t, ruleContractBareCaseID, 1, uuid.Nil),
		},
	}

	rt := NewRouter(rules, reader, clock.New())
	return &ruleContractFixture{
		rules:     rules,
		reader:    reader,
		c:         apitest.New(rt),
		oldSnap:   oldSnap,
		newSnap:   newSnap,
		blindSnap: blindSnap,
	}
}

// snapshotPath, caseRulePath and alertRulePath keep the routes in one
// place, because the tenant probe runs the same shape over all three.
func snapshotPath(id uuid.UUID) string { return "/rule-snapshots/" + id.String() }
func caseRulePath(id string) string    { return "/cases/" + id + "/rule" }
func alertRulePath(id string) string   { return "/alerts/" + id + "/rule" }

/* -------------------------------------------------------------------------- */
/* Happy paths                                                                */
/* -------------------------------------------------------------------------- */

// TestGetRuleSnapshotAnswersTheSnapshotShapeTheContractDeclares.
//
// The promise: `GET /api/v1/rule-snapshots/{id}` returns {data, meta} where data
// is a `RuleSnapshotDTO` with every required member — including the ones a
// hand-written test never thinks to check: a 64-hex `rule_fingerprint`, a
// `captured_at` that is an RFC 3339 instant rather than a unix integer, and an
// `origin`/`match_confidence` pair drawn from the contract's closed enums.
//
// What broke when it did not hold: the conformance audit found `delivery_summary`
// missing two required members with a green test suite beside it, because the
// test restated the shape instead of asserting it.
func TestGetRuleSnapshotAnswersTheSnapshotShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	f := newRuleContractFixture(t)
	resp := f.c.GET(snapshotPath(ruleContractOldSnapshotID)).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getRuleSnapshot", http.StatusOK, resp.Body())

	if ct := resp.Header("Content-Type"); ct != apitest.ContentTypeJSON {
		t.Fatalf("Content-Type = %q, want %q", ct, apitest.ContentTypeJSON)
	}
	if f.rules.getCalls != 1 {
		t.Fatalf("the service was consulted %d time(s), want exactly 1", f.rules.getCalls)
	}
	if len(f.rules.getIDs) != 1 || f.rules.getIDs[0] != ruleContractOldSnapshotID {
		t.Fatalf("the handler read %v, want the id in the path (%s)", f.rules.getIDs, ruleContractOldSnapshotID)
	}
	if f.reader.alertCalls != 0 || f.reader.occCalls != 0 {
		t.Fatal("a snapshot read reached into alerts; it takes a snapshot id and nothing else")
	}
}

// ⭐ TestGetCaseRuleAnswersTheSnapshotBoundToThatEpisode.
//
// The promise: the episode's OWN capture is returned — the threshold that was
// actually in force when it fired — resolved through the id the case
// carries and not by looking up the newest text for the rule.
//
// What broke when it did not hold: an alert from six weeks ago showing today's
// threshold is worse than showing none. The operator reads a number, believes it
// explains the fire, and it is a number that did not exist at the time.
func TestGetCaseRuleAnswersTheSnapshotBoundToThatEpisode(t *testing.T) {
	t.Parallel()

	f := newRuleContractFixture(t)
	resp := f.c.GET(caseRulePath(ruleContractCaseID.String())).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getCaseRule", http.StatusOK, resp.Body())

	if f.reader.occCalls != 1 {
		t.Fatalf("the case was read %d time(s), want exactly 1", f.reader.occCalls)
	}
	// ⭐ The snapshot fetched is the one the CASE named, not the newest
	// version of the same rule.
	if len(f.rules.getIDs) != 1 || f.rules.getIDs[0] != ruleContractOldSnapshotID {
		t.Fatalf("the handler read %v, want the snapshot the episode is bound to (%s)",
			f.rules.getIDs, ruleContractOldSnapshotID)
	}

	data, ok := resp.JSON(t)["data"].(map[string]any)
	if !ok {
		t.Fatalf("the body has no data object: %s", resp)
	}
	if got := data["id"]; got != ruleContractOldSnapshotID.String() {
		t.Fatalf("id = %v, want the bound snapshot %s and never the newest one (%s)",
			got, ruleContractOldSnapshotID, ruleContractNewSnapshotID)
	}
}

// ⭐ TestAnAttemptedButEmptyCaptureIsATwoHundredAndNotAFourOhFour.
//
// The promise, stated by the contract in as many words: a `404` means no
// snapshot could be captured for this episode AT ALL, and a stored snapshot whose
// `origin` is `unavailable` means the capture was attempted and honestly recorded
// as empty. The two must stay distinguishable, and the second one is a 200.
//
// What broke when it did not hold: collapsing them destroys the only fact the
// rules module exists to publish. "We looked and could not see the rule" is a
// statement about oto's reach into your cluster and is actionable — configure a
// Prometheus URL. "There is nothing here" is a shrug. A 404 for both tells the
// operator the second when the truth is the first.
func TestAnAttemptedButEmptyCaptureIsATwoHundredAndNotAFourOhFour(t *testing.T) {
	t.Parallel()

	f := newRuleContractFixture(t)
	resp := f.c.GET(caseRulePath(ruleContractBlindCaseID.String())).
		MustStatus(t, http.StatusOK)
	// ⭐ Still a legal RuleSnapshotResponse: an empty `expr` under `origin:
	// unavailable` is expressible without breaking the schema, which is what
	// makes the honest degradation representable at all.
	schema.Assert(t, "getCaseRule", http.StatusOK, resp.Body())

	data, ok := resp.JSON(t)["data"].(map[string]any)
	if !ok {
		t.Fatalf("the body has no data object: %s", resp)
	}
	if got := data["origin"]; got != "unavailable" {
		t.Fatalf("origin = %v, want unavailable — the row records a capture that came back empty", got)
	}
	if got := data["expr"]; got != "" {
		t.Fatalf("expr = %v, want the empty string; the contract binds it to origin=unavailable", got)
	}
	if got := data["match_confidence"]; got != "none" {
		t.Fatalf("match_confidence = %v, want none", got)
	}
	if got := data["prometheus_url"]; got != nil {
		t.Fatalf("prometheus_url = %v, want null; oto never guesses a link it does not have", got)
	}
}

// ⛔ TestAnEpisodeNothingCouldBeCapturedForIsAFourOhFour.
//
// The promise: a case carrying NO bound snapshot id — no Prometheus URL
// configured and no usable `generatorURL`, so nothing was ever written — is the
// 404 the contract describes, and the rule store is never even consulted.
//
// What broke when it did not hold: a handler that fell through to a lookup with
// the nil UUID would ask the database for a row that cannot exist, and the
// difference between "no capture" and "capture failed" would depend on whether
// somebody had ever inserted a nil-id row.
func TestAnEpisodeNothingCouldBeCapturedForIsAFourOhFour(t *testing.T) {
	t.Parallel()

	f := newRuleContractFixture(t)
	resp := f.c.GET(caseRulePath(ruleContractBareCaseID.String())).
		MustStatus(t, http.StatusNotFound)
	schema.AssertProblem(t, "getCaseRule", http.StatusNotFound, resp.Body())

	if ct := resp.Header("Content-Type"); !strings.Contains(ct, "problem+json") {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	if f.rules.getCalls != 0 {
		t.Fatalf("the rule store was consulted %d time(s) for an episode bound to nothing", f.rules.getCalls)
	}
}

// ⭐ TestGetAlertRuleHistoryAnswersTheHistoryShapeTheContractDeclares.
//
// The promise: {rule_key, current, change, versions} — the rule as it was when
// this episode fired, every version oto has ever captured for the same RuleKey
// newest first, and a structured diff when the definition changed.
//
// What broke when it did not hold: `versions` rendered oldest-first, or `current`
// resolved to the newest text upstream, both read as "the rule has not changed"
// on a screen whose entire purpose is to say that it has.
func TestGetAlertRuleHistoryAnswersTheHistoryShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	f := newRuleContractFixture(t)
	resp := f.c.GET(alertRulePath(ruleContractAlertID.String())).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getAlertRuleHistory", http.StatusOK, resp.Body())

	data, ok := resp.JSON(t)["data"].(map[string]any)
	if !ok {
		t.Fatalf("the body has no data object: %s", resp)
	}

	// ⭐ `current` is the EPISODE's snapshot, not the newest one upstream.
	current, ok := data["current"].(map[string]any)
	if !ok {
		t.Fatalf("current = %v, want the snapshot bound to the current case: %s", data["current"], resp)
	}
	if got := current["id"]; got != ruleContractOldSnapshotID.String() {
		t.Fatalf("current.id = %v, want the bound snapshot %s; the newest text is %s and this endpoint is not about it",
			got, ruleContractOldSnapshotID, ruleContractNewSnapshotID)
	}

	// Newest first, both versions present.
	versions, ok := data["versions"].([]any)
	if !ok || len(versions) != 2 {
		t.Fatalf("versions = %v, want the two distinct texts this rule has had: %s", data["versions"], resp)
	}
	first, _ := versions[0].(map[string]any)
	last, _ := versions[1].(map[string]any)
	if first["id"] != ruleContractNewSnapshotID.String() || last["id"] != ruleContractOldSnapshotID.String() {
		t.Fatalf("versions are [%v, %v], want newest first: [%s, %s]",
			first["id"], last["id"], ruleContractNewSnapshotID, ruleContractOldSnapshotID)
	}

	// The rule key is the four-part identity, not the alert name alone.
	key, ok := data["rule_key"].(map[string]any)
	if !ok {
		t.Fatalf("rule_key is %T, want an object", data["rule_key"])
	}
	if key["source_id"] != ruleContractSourceID.String() || key["rule_name"] != ruleContractAlertName {
		t.Fatalf("rule_key = %v, want the (source, file, group, name) identity of the bound snapshot", key)
	}

	// ⭐ `change` IS NULL, AND THAT IS THE ANSWER. Both episodes fired under the
	// old text; the newer version was captured after both of them. `change` is
	// the diff between two FIRES, so an edit neither fire experienced is not one
	// — it is in `versions`, where the operator can see it dated.
	if raw, present := data["change"]; !present || raw != nil {
		t.Fatalf("change = %v, want null — the rule did not move between the two episodes, "+
			"it moved after both of them: %s", raw, resp)
	}
	if f.rules.historyCalls == 0 {
		t.Fatal("the history was never read")
	}
}

// TestAnAlertNoRuleWasEverCapturedForIsAnEmptyHistoryAndNotAFourOhFour.
//
// The promise: the RuleKey is still answerable from the alert itself, so a caller
// gets `current: null` and `versions: []` rather than a refusal. "We have never
// captured this rule" is a fact worth returning.
//
// What broke when it did not hold: a 404 on the rule tab of an alert that plainly
// exists reads as a broken page, and the operator goes looking for an outage in
// oto instead of in their cluster.
func TestAnAlertNoRuleWasEverCapturedForIsAnEmptyHistoryAndNotAFourOhFour(t *testing.T) {
	t.Parallel()

	f := newRuleContractFixtureBoundTo(t, uuid.Nil)
	resp := f.c.GET(alertRulePath(ruleContractAlertID.String())).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getAlertRuleHistory", http.StatusOK, resp.Body())

	data, ok := resp.JSON(t)["data"].(map[string]any)
	if !ok {
		t.Fatalf("the body has no data object: %s", resp)
	}
	if raw, present := data["current"]; !present || raw != nil {
		t.Fatalf("current = %v, want null — nothing was ever bound to this episode: %s", raw, resp)
	}
	versions, ok := data["versions"].([]any)
	if !ok {
		t.Fatalf("versions is %T, want an array; [] and null are different answers", data["versions"])
	}
	if len(versions) != 0 {
		t.Fatalf("versions has %d entries, want 0 — the key oto can derive from the alert has no captures",
			len(versions))
	}
}

// ⛔ TestAnAlertWhoseGeneratorURLCarriesNoExpressionIsNotAValidationFailure.
//
// This is TICKET 015b25b, and it is the ordinary case rather than the exotic
// one: measured against a real server, four of five alerts ingested through the
// real webhook answered `422 rules_invalid_id` — "a rule key's source id must be
// a UUID" — and the only one that answered 200 was the only one whose
// `generatorURL` carried `g0.expr`. An alert with no expression in its URL is a
// Grafana-sourced alert, a hand-fired one, or an Alertmanager whose Prometheus
// is unreachable. All three are normal.
//
// The promise, in three parts, each of which the 422 broke:
//
//   - the STATUS is 200. Absence of a snapshot is not a client error, and 4xx
//     says the caller did something wrong;
//   - the RULE STORE IS NEVER ASKED. The key oto derives from an alert with no
//     capture names no AlertSource, and `rule_snapshots` cannot be addressed
//     without one — the repository refuses such a key as a validation error, so
//     the handler must not hand it one. `stubRuleReads.History` refuses it here
//     exactly as the repository does, which is what makes this assertion real
//     rather than a restatement of a permissive double;
//   - the BODY says so: `current: null`, `versions: []`. That is the shape the
//     contract already declares (`RuleHistoryDTO.current` is `oneOf
//     [RuleSnapshotDTO, null]`) and the shape the alert LIST already renders as
//     a plain em-dash, so the detail page can be as calm as the list is.
//
// What broke when it did not hold: the "Rule at fire time" panel — the product's
// headline promise, the first thing the README claims — rendered a red
// "Validation failed — a rule key's source id must be a UUID" box with a Try
// again button. Internal vocabulary, blaming the operator for something they did
// not do, and inviting a retry that can never succeed.
func TestAnAlertWhoseGeneratorURLCarriesNoExpressionIsNotAValidationFailure(t *testing.T) {
	t.Parallel()

	f := newRuleContractFixtureFor(t, uuid.Nil, ruleContractBareGeneratorURL)
	resp := f.c.GET(alertRulePath(ruleContractAlertID.String())).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getAlertRuleHistory", http.StatusOK, resp.Body())

	if f.rules.historyCalls != 0 {
		t.Fatalf("the rule store was asked for the history of an unaddressable key %d time(s); "+
			"that read is the 422 this test exists to prevent", f.rules.historyCalls)
	}
	if f.reader.prevCalls != 0 {
		t.Fatalf("the previous episode was looked up %d time(s) for an episode with no snapshot of "+
			"its own; there is nothing for its predecessor to be compared against", f.reader.prevCalls)
	}

	data, ok := resp.JSON(t)["data"].(map[string]any)
	if !ok {
		t.Fatalf("the body has no data object: %s", resp)
	}
	if raw, present := data["current"]; !present || raw != nil {
		t.Fatalf("current = %v, want null — no expression in the generatorURL means nothing to capture: %s",
			raw, resp)
	}
	if raw, present := data["change"]; present && raw != nil {
		t.Fatalf("change = %v, want null — there is no pair of snapshots to compare", raw)
	}
	versions, ok := data["versions"].([]any)
	if !ok || len(versions) != 0 {
		t.Fatalf("versions = %v, want []", data["versions"])
	}

	// ⚠️ The refusal's own words must not reach the operator by any route. They
	// are the vocabulary of a database index, not of an alerting tool.
	if body := string(resp.Body()); strings.Contains(body, "rules_invalid_id") ||
		strings.Contains(body, "must be a UUID") {
		t.Fatalf("the answer carries the repository's validation vocabulary: %s", resp)
	}
}

// TestTheCaseVariantOfTheSameAlertIsAFourOhFourAndNotA422.
//
// The sibling read of the same fact, checked at the same time because it has the
// same shape of hole. `/cases/{id}/rule` answers ONE snapshot and its
// success schema is a `RuleSnapshotDTO` — there is no 200 that can spell "there
// isn't one", so absence is the 404 the contract declares in as many words. It
// must still never be a 422, and it must never reach the rule store with an id
// it knows is nil.
//
// Two endpoints, two statuses, one reason: the answer is shaped by what the
// operation returns, not by a house preference for 404s.
func TestTheCaseVariantOfTheSameAlertIsAFourOhFourAndNotA422(t *testing.T) {
	t.Parallel()

	f := newRuleContractFixtureFor(t, uuid.Nil, ruleContractBareGeneratorURL)
	resp := f.c.GET(caseRulePath(ruleContractBareCaseID.String())).
		MustStatus(t, http.StatusNotFound)
	schema.AssertProblem(t, "getCaseRule", http.StatusNotFound, resp.Body())

	if code := resp.Problem(t).Code; code != "not_found" {
		t.Fatalf("code = %q, want not_found — absence is not a validation failure", code)
	}
	if f.rules.getCalls != 0 || f.rules.historyCalls != 0 {
		t.Fatalf("the rule store was consulted (get=%d, history=%d) for an episode bound to nothing",
			f.rules.getCalls, f.rules.historyCalls)
	}
}

// TestTheHistoryLimitBoundsTheVersionsReturned, newest first.
//
// A rule edited two hundred times is a real thing, and a caller asking for the
// last one must not be handed the lot.
func TestTheHistoryLimitBoundsTheVersionsReturned(t *testing.T) {
	t.Parallel()

	f := newRuleContractFixture(t)
	resp := f.c.GET(alertRulePath(ruleContractAlertID.String())+"?limit=1").
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "getAlertRuleHistory", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	versions, ok := data["versions"].([]any)
	if !ok || len(versions) != 1 {
		t.Fatalf("versions = %v, want exactly the one version asked for", data["versions"])
	}
	only, _ := versions[0].(map[string]any)
	if got := only["id"]; got != ruleContractNewSnapshotID.String() {
		t.Fatalf("versions[0].id = %v, want the newest version %s — a limit truncates the tail, not the head",
			got, ruleContractNewSnapshotID)
	}
}

/* -------------------------------------------------------------------------- */
/* Boundaries                                                                 */
/* -------------------------------------------------------------------------- */

// ⛔ TestARuleReadOutsideTheCallersTenantIsANotFound.
//
// The promise: every id-addressed rule read answers 404 for an id it does not
// own — never 403, never another tenant's rule text, never a 500.
//
// What broke when it did not hold: a 403 confirms the id exists somewhere, which
// turns the id space into a cross-tenant existence oracle. v1 has no roles, so
// the ONLY cause of a 403 would be cross-org access — precisely the case that
// must be indistinguishable from "no such thing". And the rule text is the most
// sensitive thing in this module: `expr` names another company's metrics, their
// thresholds and their internal service names.
//
// `apitest.StrangerID` belongs to `apitest.OtherOrgID`, and the fakes behave the
// way `db.TenantScope` makes the repository behave: an id outside the scope
// simply is not there.
func TestARuleReadOutsideTheCallersTenantIsANotFound(t *testing.T) {
	t.Parallel()

	routes := []apitest.Route{
		{Name: "a snapshot id owned by another org", Op: "getRuleSnapshot",
			Method: http.MethodGet, Path: snapshotPath(apitest.StrangerID)},
		{Name: "a case id owned by another org", Op: "getCaseRule",
			Method: http.MethodGet, Path: caseRulePath(apitest.StrangerID.String())},
		{Name: "an alert id owned by another org", Op: "getAlertRuleHistory",
			Method: http.MethodGet, Path: alertRulePath(apitest.StrangerID.String())},
		{Name: "a snapshot id that is not a uuid at all", Op: "getRuleSnapshot",
			Method: http.MethodGet, Path: "/rule-snapshots/banana"},
		{Name: "a case id that is not a uuid at all", Op: "getCaseRule",
			Method: http.MethodGet, Path: caseRulePath("banana")},
		{Name: "an alert id that is not a uuid at all", Op: "getAlertRuleHistory",
			Method: http.MethodGet, Path: alertRulePath("banana")},
		{Name: "the nil uuid as a snapshot id", Op: "getRuleSnapshot",
			Method: http.MethodGet, Path: "/rule-snapshots/00000000-0000-0000-0000-000000000000"},
		{Name: "the nil uuid as a case id", Op: "getCaseRule",
			Method: http.MethodGet, Path: caseRulePath("00000000-0000-0000-0000-000000000000")},
		{Name: "the nil uuid as an alert id", Op: "getAlertRuleHistory",
			Method: http.MethodGet, Path: alertRulePath("00000000-0000-0000-0000-000000000000")},
	}

	apitest.AssertCrossTenant404(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		f := newRuleContractFixture(t)
		return f.c, func(t *testing.T, _ apitest.Route, resp *apitest.Response) {
			// `instance` echoes the request path by design (RFC 9457 §3.1), and
			// the caller already knows the id it asked for — so the leak to guard
			// is the ANSWER, not the question. Nothing of ours may come back.
			body := string(resp.Body())
			if strings.Contains(body, ruleContractOldExpr) || strings.Contains(body, ruleContractNewExpr) {
				t.Fatalf("the refusal carries a rule expression: %s", resp)
			}
		}
	}, routes)
}

// TestABadHistoryLimitIsRefusedWithTheFieldNamed — the 422 the contract declares
// for `getAlertRuleHistory` and for neither of its siblings.
//
// The promise: `limit` outside 1..200, or not an integer at all, is a 422 whose
// `violations[]` names `limit` with a machine code a form can branch on.
//
// What broke when it did not hold: a refusal carrying only prose leaves a UI with
// nothing to highlight, so the caller retries a different wrong value. And a
// silently clamped `limit=0` returns an empty history that looks exactly like a
// rule oto has never captured.
func TestABadHistoryLimitIsRefusedWithTheFieldNamed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
	}{
		{"zero", "?limit=0"},
		{"negative", "?limit=-1"},
		{"above the contract ceiling", "?limit=201"},
		{"not an integer", "?limit=banana"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newRuleContractFixture(t)
			resp := f.c.GET(alertRulePath(ruleContractAlertID.String())+tc.query).
				MustStatus(t, http.StatusUnprocessableEntity)
			schema.AssertProblem(t, "getAlertRuleHistory", http.StatusUnprocessableEntity, resp.Body())

			p := resp.MustViolate(t, "limit")
			if p.Status != http.StatusUnprocessableEntity {
				t.Fatalf("the problem says %d, the status line said 422", p.Status)
			}
			if f.reader.alertCalls != 0 || f.rules.historyCalls != 0 {
				t.Fatal("a refused query still reached the services")
			}
		})
	}
}

// TestAnUnauthenticatedCallerGetsTheSame401OnEveryRuleRoute.
//
// The promise: with no principal in the context there is no tenant, so there is
// nothing to read — one code, one shape, on all three routes.
//
// What broke when it did not hold: a handler that resolved its scope from
// anything other than `authn.Scope` would serve rule text to a caller who proved
// nothing. All three re-derive it rather than trusting that a middleware ran,
// because "the middleware guarantees it" stops being true the first time a route
// is mounted somewhere else.
func TestAnUnauthenticatedCallerGetsTheSame401OnEveryRuleRoute(t *testing.T) {
	t.Parallel()

	routes := []apitest.Route{
		{Op: "getRuleSnapshot", Method: http.MethodGet, Path: snapshotPath(ruleContractOldSnapshotID)},
		{Op: "getCaseRule", Method: http.MethodGet, Path: caseRulePath(ruleContractCaseID.String())},
		{Op: "getAlertRuleHistory", Method: http.MethodGet, Path: alertRulePath(ruleContractAlertID.String())},
	}

	apitest.AssertUnauthenticated(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		f := newRuleContractFixture(t)
		return f.c, func(t *testing.T, _ apitest.Route, _ *apitest.Response) {
			if f.rules.getCalls != 0 || f.rules.historyCalls != 0 ||
				f.reader.alertCalls != 0 || f.reader.occCalls != 0 {
				t.Fatal("an unauthenticated request reached the services")
			}
		}
	}, routes)
}

// TestAFailedRuleReadIsNotAnEmptyRule.
//
// oto's silence must stay distinguishable from an answer: a read that could not
// run is a 500, never a 200 carrying a blank rule. It is the same promise the
// `unavailable` origin makes on the success path, enforced on the failure one.
func TestAFailedRuleReadIsNotAnEmptyRule(t *testing.T) {
	t.Parallel()

	f := newRuleContractFixture(t)
	f.rules.getErr = errs.New(errs.KindInternal, "rules_query_failed", "could not read the rule snapshot")

	resp := f.c.GET(snapshotPath(ruleContractOldSnapshotID)).MustStatus(t, http.StatusInternalServerError)
	schema.AssertProblem(t, "getRuleSnapshot", http.StatusInternalServerError, resp.Body())
}

/* -------------------------------------------------------------------------- */
/* The contract's own declarations                                            */
/* -------------------------------------------------------------------------- */

// TestTheDeclaredRuleReadOperationsAreTheOnesThisPackageServes guards against the
// failure mode that made the audit necessary: an operation declared in the
// contract that no test ever names, because the list of what to test was kept by
// hand.
func TestTheDeclaredRuleReadOperationsAreTheOnesThisPackageServes(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"getRuleSnapshot", "getCaseRule", "getAlertRuleHistory"} {
		op := schema.Op(t, id)
		if op.SuccessStatus() != http.StatusOK {
			t.Fatalf("%s declares success %d, and this file asserts 200", id, op.SuccessStatus())
		}
		if !op.HasPathParam("id") {
			t.Fatalf("%s no longer takes an {id}; the tenant probe in this file is addressing nothing", id)
		}
		if !op.Declares(http.StatusUnauthorized) {
			t.Fatalf("%s declares no 401, but the handler can produce one", id)
		}
		if !op.Declares(http.StatusNotFound) {
			t.Fatalf("%s declares no 404, but an id outside the tenant must produce one", id)
		}
	}

	// Only the history read takes a query parameter, so only it can refuse one.
	if !schema.Op(t, "getAlertRuleHistory").Declares(http.StatusUnprocessableEntity) {
		t.Fatal("getAlertRuleHistory declares no 422, but `limit` is bounded 1..200")
	}
	for _, id := range []string{"getRuleSnapshot", "getCaseRule"} {
		if schema.Op(t, id).Declares(http.StatusUnprocessableEntity) {
			t.Fatalf("%s has grown a 422; it takes no query parameters and its only input is a path id", id)
		}
	}
}

/* -------------------------------------------------------------------------- */
/* The diff is between two fires                                              */
/* -------------------------------------------------------------------------- */

// ⭐⭐ TestTheRuleChangeIsDiffedAgainstThePreviousEpisodeAndNotTheNewestVersion.
//
// The contract defines `RuleChangeDTO` as "a structured diff between the rule
// snapshot bound to this case and the one bound to the PREVIOUS case
// of the same RuleKey", and `RuleHistoryDTO.change` as "the diff against the
// previous case's snapshot, when the definition changed". `dto.go` repeats
// that definition word for word, and now so does the handler.
//
// It used to compute a `DiffSince(bound.Fingerprint)` — `Compare(this episode's
// snapshot, the NEWEST version in the history)`, a service method deleted once
// this handler stopped calling it — which was wrong on the wire twice over:
//
//   - `previous_snapshot_id`, `previous_fingerprint`, `previous_captured_at` and
//     `previous_expr` described THIS episode's own snapshot, the same row the
//     response already returned as `current`, and `new_expr` was a text this
//     episode never fired under;
//   - in the case the panel exists for — the rule was edited BEFORE this episode
//     fired, so the bound snapshot IS the newest one — the two sides of the
//     compare were identical and `change` came back null exactly when an
//     operator asking "did somebody change this before it fired?" needed it
//     populated.
//
// That second case is what this test drives, which is why it is the arrangement
// worth spending a fixture on: this episode fired under the new text and the one
// before it under the old, so the ONLY diff a correct implementation can produce
// is (old → new), and the old implementation produced nothing at all.
func TestTheRuleChangeIsDiffedAgainstThePreviousEpisodeAndNotTheNewestVersion(t *testing.T) {
	t.Parallel()

	// This episode fired under the NEW text; the previous episode fired under the
	// old one. The contract wants change = (old -> new).
	f := newRuleContractFixtureBoundTo(t, ruleContractNewSnapshotID)
	resp := f.c.GET(alertRulePath(ruleContractAlertID.String())).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getAlertRuleHistory", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	change, ok := data["change"].(map[string]any)
	if !ok {
		t.Fatalf("change = %v, want the diff against the previous case's snapshot: %s",
			data["change"], resp)
	}
	if got := change["previous_snapshot_id"]; got != ruleContractOldSnapshotID.String() {
		t.Fatalf("previous_snapshot_id = %v, want the PREVIOUS episode's snapshot %s",
			got, ruleContractOldSnapshotID)
	}
	if got := change["previous_expr"]; got != ruleContractOldExpr {
		t.Fatalf("previous_expr = %v, want the text the previous episode fired under", got)
	}
	if got := change["new_expr"]; got != ruleContractNewExpr {
		t.Fatalf("new_expr = %v, want the text this episode fired under", got)
	}
	if got := change["expr_changed"]; got != true {
		t.Fatalf("expr_changed = %v, want true — the threshold moved between the two fires", got)
	}

	// ⛔ The predecessor is the episode's, not the snapshot list's. The blind
	// episode (seq 2) and the episode with nothing bound (seq 1) are both older
	// than the one this diff came from; picking either would have produced a
	// different `previous_snapshot_id` or none at all.
	if f.reader.prevCalls != 1 {
		t.Fatalf("the previous episode was asked for %d time(s), want exactly 1", f.reader.prevCalls)
	}
}

// TestTheFirstEpisodeOfAnAlertHasNoRuleChangeToShow.
//
// The promise: an alert firing for the first time reports `change: null` rather
// than a diff of its own snapshot against itself. There is no previous fire, so
// there is nothing that changed BETWEEN fires.
//
// What broke when it did not hold: `previous_*` naming the row the response
// already returned as `current`, which reads as "this rule changed" on the panel
// whose only job is to say whether it did.
func TestTheFirstEpisodeOfAnAlertHasNoRuleChangeToShow(t *testing.T) {
	t.Parallel()

	f := newRuleContractFixtureBoundTo(t, ruleContractNewSnapshotID)
	// The alert's whole history is this one episode, and it is seq 1.
	only := ruleContractCase(t, ruleContractCaseID, 1, ruleContractNewSnapshotID)
	f.reader.detail.CurrentCase = &only
	f.reader.detail.LatestCase = &only
	f.reader.acs = map[uuid.UUID]alertdomain.Case{ruleContractCaseID: only}

	resp := f.c.GET(alertRulePath(ruleContractAlertID.String())).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getAlertRuleHistory", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	if raw, present := data["change"]; !present || raw != nil {
		t.Fatalf("change = %v, want null — a first fire has no previous fire to differ from: %s",
			raw, resp)
	}
	// `current` is still answered: "what the rule said when this fired" does not
	// depend on there having been a previous fire.
	if current, ok := data["current"].(map[string]any); !ok ||
		current["id"] != ruleContractNewSnapshotID.String() {
		t.Fatalf("current = %v, want the snapshot this episode fired under", data["current"])
	}
}

// TestAnEpisodeWhosePredecessorWentBlindReportsNoChange.
//
// The promise: when the previous episode's capture recovered NOTHING — a stored
// `unavailable` row — the panel says nothing changed rather than reporting the
// whole rule as new. `domain.Drifted` is the gate and it is the same gate the
// capture path uses: "oto went blind for an hour" is not somebody editing a rule.
//
// What broke when it did not hold: every alert in a source whose Prometheus was
// briefly unreachable claims its rule was rewritten, against an empty expression.
func TestAnEpisodeWhosePredecessorWentBlindReportsNoChange(t *testing.T) {
	t.Parallel()

	f := newRuleContractFixtureBoundTo(t, ruleContractNewSnapshotID)
	// Drop the episode that fired under the old text, leaving the blind capture
	// (seq 2) as the newest predecessor that has anything bound at all.
	delete(f.reader.acs, ruleContractPreviousCaseID)

	resp := f.c.GET(alertRulePath(ruleContractAlertID.String())).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getAlertRuleHistory", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	if raw, present := data["change"]; !present || raw != nil {
		t.Fatalf("change = %v, want null — the previous episode's capture recovered nothing, "+
			"and an outage is not an edit: %s", raw, resp)
	}
}

// TestAnUnknownQueryParameterIsRefusedWithADeclared400.
//
// SPEC §E.3 is binding and the handlers obey it: `httpx.NewParams` refuses an
// unknown query parameter with `400 unknown_parameter` on all three routes, which
// is the right behaviour — a silently dropped `?limt=1` returns a plausible
// answer to a question nobody asked.
//
// ⭐ THE DIVERGENCE WAS IN THE CONTRACT, not in the handlers (git-bug ee3ae9c):
// none of the three declared a `400`, so a generated client had no schema for a
// response its own server sends, and the assertion below could not even be
// written — `schema.AssertProblem` failed at the LOOKUP rather than at the
// validation. `getAlertRuleHistory` is the sharpest of the three: it takes a
// `limit`, so a client that misspells it is a client that reaches this.
func TestAnUnknownQueryParameterIsRefusedWithADeclared400(t *testing.T) {
	t.Parallel()

	apitest.AssertUnknownQueryParamRefused(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		return newRuleContractFixture(t).c, nil
	}, []apitest.Route{
		{Op: "getRuleSnapshot", Method: http.MethodGet,
			Path: snapshotPath(ruleContractOldSnapshotID) + "?verbose=true"},
		{Op: "getCaseRule", Method: http.MethodGet,
			Path: caseRulePath(ruleContractCaseID.String()) + "?verbose=true"},
		{Op: "getAlertRuleHistory", Method: http.MethodGet,
			Path: alertRulePath(ruleContractAlertID.String()) + "?limt=1"},
	})
}
