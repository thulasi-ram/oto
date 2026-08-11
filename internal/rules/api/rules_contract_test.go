package api

// THE THREE ID-ADDRESSED RULE READS, CHECKED AGAINST THE CONTRACT ITSELF.
//
//	getRuleSnapshot     GET /api/v1/rule-snapshots/{id}
//	getOccurrenceRule   GET /api/v1/occurrences/{id}/rule
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
//   - a `404` on the occurrence rule means NO SNAPSHOT COULD BE CAPTURED AT ALL.
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

	// The three episodes: one bound to a real capture, one bound to an
	// `unavailable` capture, one bound to nothing at all.
	ruleContractOccurrenceID      = uuid.MustParse("0198f3c3-1111-7111-8111-111111111111")
	ruleContractBlindOccurrenceID = uuid.MustParse("0198f3c3-2222-7222-8222-222222222222")
	ruleContractBareOccurrenceID  = uuid.MustParse("0198f3c3-3333-7333-8333-333333333333")
)

const (
	ruleContractAlertName = "HighErrorRate"
	ruleContractBlindName = "DiskFillingUp"
	ruleContractPromURL   = "https://prometheus.example.com"
	ruleContractOldExpr   = `sum(rate(http_requests_total{code=~"5.."}[5m])) / sum(rate(http_requests_total[5m])) > 0.05`
	ruleContractNewExpr   = `sum(rate(http_requests_total{code=~"5.."}[5m])) / sum(rate(http_requests_total[5m])) > 0.1`
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

// ruleContractAlert builds the alert through `alerts/domain`, so `alert_key` and
// the label set are the real derivations rather than strings that look like them.
func ruleContractAlert(t *testing.T) alertdomain.Alert {
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
		ID:                  ruleContractAlertID,
		OrgID:               apitest.OrgID,
		ClusterID:           ruleContractClusterID,
		Key:                 alertdomain.ComputeAlertKey(apitest.OrgID, clusterKey, labels, nil),
		Fingerprint:         alertdomain.ComputeSourceFingerprint(labels),
		ClusterKey:          clusterKey,
		Labels:              labels,
		GeneratorURL:        "https://prometheus.example.com/graph?g0.expr=up",
		State:               alertdomain.StateFiring,
		AckState:            alertdomain.AckStateUnacked,
		CurrentOccurrenceID: ruleContractOccurrenceID,
		FirstSeenAt:         ruleContractEpoch,
		LastSeenAt:          ruleContractEpoch.Add(5 * time.Minute),
		LastStateChangeAt:   ruleContractEpoch,
		TotalOccurrences:    3,
	})
	if err != nil {
		t.Fatalf("build the alert: %v", err)
	}
	return a
}

// ruleContractOccurrence builds one episode bound to snapID. A uuid.Nil snapID
// is the "nothing could be captured" case the contract spells 404.
func ruleContractOccurrence(t *testing.T, id uuid.UUID, seq int, snapID uuid.UUID) alertdomain.Occurrence {
	t.Helper()

	started := ruleContractEpoch.Add(time.Duration(seq) * time.Hour)
	o, err := alertdomain.NewOccurrence(alertdomain.OccurrenceParams{
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
		t.Fatalf("build occurrence %s: %v", id, err)
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
	diffCalls    int
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

func (s *stubRuleReads) History(
	_ context.Context, _ db.TenantScope, key domain.Key,
) (domain.History, error) {
	s.historyCalls++
	return domain.NewHistory(key, s.hist[key]), nil
}

func (s *stubRuleReads) ListSnapshots(
	context.Context, db.TenantScope, domain.Key, db.Keyset,
) (service.SnapshotPage, error) {
	return service.SnapshotPage{}, nil
}

// DiffSince mirrors `service.Service.DiffSince` exactly: locate the bound
// version by content address, and compare it with the newest one. A double that
// invented its own answer here would be testing the double.
func (s *stubRuleReads) DiffSince(
	ctx context.Context, scope db.TenantScope, key domain.Key, boundFingerprint string,
) (domain.Diff, bool, error) {
	s.diffCalls++
	h, err := s.History(ctx, scope, key)
	if err != nil {
		return domain.Diff{}, false, err
	}
	bound, ok := h.ByFingerprint(boundFingerprint)
	if !ok {
		return domain.Diff{}, false, nil
	}
	latest, ok := h.Latest()
	if !ok || latest.Number == bound.Number {
		return domain.Diff{}, false, nil
	}
	return domain.Compare(bound.Snapshot, latest.Snapshot), true, nil
}

// stubAlertReads is the cross-domain port. It owns one alert and a handful of
// episodes and, like the repository behind it, 404s everything else.
type stubAlertReads struct {
	detail alerts.AlertDetail
	occs   map[uuid.UUID]alertdomain.Occurrence

	alertCalls int
	occCalls   int
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

func (s *stubAlertReads) GetOccurrence(
	_ context.Context, _ db.TenantScope, occurrenceID uuid.UUID,
) (alertdomain.Occurrence, error) {
	s.occCalls++
	occ, ok := s.occs[occurrenceID]
	if !ok {
		return alertdomain.Occurrence{}, errs.NotFound("not_found", "no such resource")
	}
	return occ, nil
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
//	the rule was captured at the epoch, the episode under test fired under THAT
//	text, and three days later somebody doubled the threshold.
//
// So the alert's current occurrence is bound to the OLDER of two versions, which
// is the only arrangement in which "the rule as it was when this fired" and "the
// rule as it is now" can be told apart at all.
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

	current := ruleContractOccurrence(t, ruleContractOccurrenceID, 3, boundSnapshot)
	reader := &stubAlertReads{
		detail: alerts.AlertDetail{
			Alert:             ruleContractAlert(t),
			CurrentOccurrence: &current,
			LatestOccurrence:  &current,
		},
		occs: map[uuid.UUID]alertdomain.Occurrence{
			ruleContractOccurrenceID: current,
			ruleContractBlindOccurrenceID: ruleContractOccurrence(
				t, ruleContractBlindOccurrenceID, 2, ruleContractBlindSnapshotID),
			ruleContractBareOccurrenceID: ruleContractOccurrence(
				t, ruleContractBareOccurrenceID, 1, uuid.Nil),
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

// snapshotPath, occurrenceRulePath and alertRulePath keep the routes in one
// place, because the tenant probe runs the same shape over all three.
func snapshotPath(id uuid.UUID) string    { return "/rule-snapshots/" + id.String() }
func occurrenceRulePath(id string) string { return "/occurrences/" + id + "/rule" }
func alertRulePath(id string) string      { return "/alerts/" + id + "/rule" }

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

// ⭐ TestGetOccurrenceRuleAnswersTheSnapshotBoundToThatEpisode.
//
// The promise: the episode's OWN capture is returned — the threshold that was
// actually in force when it fired — resolved through the id the occurrence
// carries and not by looking up the newest text for the rule.
//
// What broke when it did not hold: an alert from six weeks ago showing today's
// threshold is worse than showing none. The operator reads a number, believes it
// explains the fire, and it is a number that did not exist at the time.
func TestGetOccurrenceRuleAnswersTheSnapshotBoundToThatEpisode(t *testing.T) {
	t.Parallel()

	f := newRuleContractFixture(t)
	resp := f.c.GET(occurrenceRulePath(ruleContractOccurrenceID.String())).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getOccurrenceRule", http.StatusOK, resp.Body())

	if f.reader.occCalls != 1 {
		t.Fatalf("the occurrence was read %d time(s), want exactly 1", f.reader.occCalls)
	}
	// ⭐ The snapshot fetched is the one the OCCURRENCE named, not the newest
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
	resp := f.c.GET(occurrenceRulePath(ruleContractBlindOccurrenceID.String())).
		MustStatus(t, http.StatusOK)
	// ⭐ Still a legal RuleSnapshotResponse: an empty `expr` under `origin:
	// unavailable` is expressible without breaking the schema, which is what
	// makes the honest degradation representable at all.
	schema.Assert(t, "getOccurrenceRule", http.StatusOK, resp.Body())

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
// The promise: an occurrence carrying NO bound snapshot id — no Prometheus URL
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
	resp := f.c.GET(occurrenceRulePath(ruleContractBareOccurrenceID.String())).
		MustStatus(t, http.StatusNotFound)
	schema.AssertProblem(t, "getOccurrenceRule", http.StatusNotFound, resp.Body())

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
		t.Fatalf("current = %v, want the snapshot bound to the current occurrence: %s", data["current"], resp)
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

	// The definition did move between the two captures, so a diff is on offer.
	// WHICH two snapshots it compares is pinned separately, in
	// TestBUG_TheRuleChangeIsDiffedAgainstTheNewestVersionRatherThanThePreviousEpisode.
	if data["change"] == nil {
		t.Fatalf("change is null although the rule text changed between versions: %s", resp)
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

	cases := []struct {
		name string
		op   string
		path string
	}{
		{
			name: "a snapshot id owned by another org",
			op:   "getRuleSnapshot",
			path: snapshotPath(apitest.StrangerID),
		},
		{
			name: "an occurrence id owned by another org",
			op:   "getOccurrenceRule",
			path: occurrenceRulePath(apitest.StrangerID.String()),
		},
		{
			name: "an alert id owned by another org",
			op:   "getAlertRuleHistory",
			path: alertRulePath(apitest.StrangerID.String()),
		},
		{
			name: "a snapshot id that is not a uuid at all",
			op:   "getRuleSnapshot",
			path: "/rule-snapshots/banana",
		},
		{
			name: "an occurrence id that is not a uuid at all",
			op:   "getOccurrenceRule",
			path: occurrenceRulePath("banana"),
		},
		{
			name: "an alert id that is not a uuid at all",
			op:   "getAlertRuleHistory",
			path: alertRulePath("banana"),
		},
		{
			name: "the nil uuid as a snapshot id",
			op:   "getRuleSnapshot",
			path: "/rule-snapshots/00000000-0000-0000-0000-000000000000",
		},
		{
			name: "the nil uuid as an occurrence id",
			op:   "getOccurrenceRule",
			path: occurrenceRulePath("00000000-0000-0000-0000-000000000000"),
		},
		{
			name: "the nil uuid as an alert id",
			op:   "getAlertRuleHistory",
			path: alertRulePath("00000000-0000-0000-0000-000000000000"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newRuleContractFixture(t)
			resp := f.c.GET(tc.path).MustStatus(t, http.StatusNotFound)
			schema.AssertProblem(t, tc.op, http.StatusNotFound, resp.Body())

			if ct := resp.Header("Content-Type"); !strings.Contains(ct, "problem+json") {
				t.Fatalf("Content-Type = %q, want application/problem+json", ct)
			}
			// ⚠️ The refusal says nothing about the other tenant. Naming the
			// owning org would be the same leak the status code just avoided.
			body := string(resp.Body())
			if strings.Contains(body, apitest.OtherOrgID.String()) {
				t.Fatalf("the 404 names the owning org: %s", resp)
			}
			// `instance` echoes the request path by design (RFC 9457 §3.1), and
			// the caller already knows the id it asked for — so the leak to guard
			// is the ANSWER, not the question. Nothing of ours may come back.
			if strings.Contains(body, ruleContractOldExpr) || strings.Contains(body, ruleContractNewExpr) {
				t.Fatalf("the refusal carries a rule expression: %s", resp)
			}
		})
	}
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

	routes := []struct {
		op   string
		path string
	}{
		{"getRuleSnapshot", snapshotPath(ruleContractOldSnapshotID)},
		{"getOccurrenceRule", occurrenceRulePath(ruleContractOccurrenceID.String())},
		{"getAlertRuleHistory", alertRulePath(ruleContractAlertID.String())},
	}

	for _, route := range routes {
		t.Run(route.op, func(t *testing.T) {
			t.Parallel()

			f := newRuleContractFixture(t)
			resp := f.c.Anonymous().GET(route.path).MustStatus(t, http.StatusUnauthorized)
			schema.AssertProblem(t, route.op, http.StatusUnauthorized, resp.Body())

			if code := resp.Problem(t).Code; code != "unauthenticated" {
				t.Fatalf("code = %q, want unauthenticated", code)
			}
			if f.rules.getCalls != 0 || f.rules.historyCalls != 0 ||
				f.reader.alertCalls != 0 || f.reader.occCalls != 0 {
				t.Fatal("an unauthenticated request reached the services")
			}
		})
	}
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

	for _, id := range []string{"getRuleSnapshot", "getOccurrenceRule", "getAlertRuleHistory"} {
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
	for _, id := range []string{"getRuleSnapshot", "getOccurrenceRule"} {
		if schema.Op(t, id).Declares(http.StatusUnprocessableEntity) {
			t.Fatalf("%s has grown a 422; it takes no query parameters and its only input is a path id", id)
		}
	}
}

/* -------------------------------------------------------------------------- */
/* Divergences, pinned rather than papered over                               */
/* -------------------------------------------------------------------------- */

// ⛔ TestBUG_TheRuleChangeIsDiffedAgainstTheNewestVersionRatherThanThePreviousEpisode.
//
// The contract defines `RuleChangeDTO` as "a structured diff between the rule
// snapshot bound to this occurrence and the one bound to the PREVIOUS occurrence
// of the same RuleKey", and `RuleHistoryDTO.change` as "the diff against the
// previous occurrence's snapshot, when the definition changed". `dto.go` repeats
// that definition word for word.
//
// The handler computes something else: `DiffSince(bound.Fingerprint)`, which is
// `Compare(this episode's snapshot, the NEWEST version in the history)`. Two
// consequences, both wrong on the wire:
//
//   - `previous_snapshot_id`, `previous_fingerprint`, `previous_captured_at` and
//     `previous_expr` describe THIS episode's own snapshot — the same row the
//     response already returned as `current` — and `new_expr` is a text this
//     episode never fired under;
//   - in the case the contract is actually about (the rule was edited BEFORE
//     this episode fired, so the bound snapshot is the newest one), `DiffSince`
//     finds `latest.Number == bound.Number` and returns changed=false, so
//     `change` is null exactly when the contract says it should be populated.
//
// That second case is what this test drives. It is skipped rather than inverted
// because the fix belongs in production code, and it is not a one-liner: the
// `AlertReader` port this package declares exposes only `CurrentOccurrence` and
// `LatestOccurrence`, so the previous occurrence's snapshot is not reachable from
// here at all. Either the port grows a way to ask for it, or the contract is
// amended to describe drift-since-this-episode — but the two must be made to
// agree, and today they do not.
func TestBUG_TheRuleChangeIsDiffedAgainstTheNewestVersionRatherThanThePreviousEpisode(t *testing.T) {
	t.Skip("BUG: getAlertRuleHistory builds `change` from DiffSince(bound.Fingerprint) = Compare(this episode's snapshot, the newest version upstream) (rules/api/handlers.go getAlertRuleHistory); the contract's RuleChangeDTO is the diff between the PREVIOUS occurrence's snapshot and this one, so `previous_*` names this episode's own row and an episode that fired under the newest text reports `change: null` instead of the edit that preceded it. The AlertReader port cannot reach the previous occurrence, so the fix is a port change, not a handler tweak.")

	// This episode fired under the NEW text; the previous episode fired under the
	// old one. The contract wants change = (old -> new).
	f := newRuleContractFixtureBoundTo(t, ruleContractNewSnapshotID)
	resp := f.c.GET(alertRulePath(ruleContractAlertID.String())).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getAlertRuleHistory", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	change, ok := data["change"].(map[string]any)
	if !ok {
		t.Fatalf("change = %v, want the diff against the previous occurrence's snapshot: %s",
			data["change"], resp)
	}
	if got := change["previous_snapshot_id"]; got != ruleContractOldSnapshotID.String() {
		t.Fatalf("previous_snapshot_id = %v, want the PREVIOUS episode's snapshot %s",
			got, ruleContractOldSnapshotID)
	}
	if got := change["new_expr"]; got != ruleContractNewExpr {
		t.Fatalf("new_expr = %v, want the text this episode fired under", got)
	}
}

// ⛔ TestBUG_AnUnknownQueryParameterIsRefusedWithAStatusTheContractNeverDeclares.
//
// SPEC §E.3 is binding and the handlers obey it: `httpx.NewParams` refuses an
// unknown query parameter with `400 unknown_parameter` on all three routes, which
// is the right behaviour — a silently dropped `?limt=1` returns a plausible
// answer to a question nobody asked.
//
// `api/openapi/openapi.yaml` declares no `400` for any of the three. A generated
// client therefore has no schema for a response its server will send, and the
// contract-driven assertion below cannot even be written: `schema.AssertProblem`
// fails at the lookup, not at the validation.
//
// The divergence is in the CONTRACT here rather than in the handler, which is why
// this is pinned instead of asserted — the fix is three `'400': $ref:
// '#/components/responses/BadRequest'` entries, and this package may not edit the
// contract.
func TestBUG_AnUnknownQueryParameterIsRefusedWithAStatusTheContractNeverDeclares(t *testing.T) {
	t.Skip("BUG: getRuleSnapshot, getOccurrenceRule and getAlertRuleHistory answer `400 unknown_parameter` for an unknown query parameter (httpx.NewParams, rules/api/handlers.go), but api/openapi/openapi.yaml declares no 400 for any of the three; the contract must declare the refusal SPEC §E.3 requires.")

	routes := []struct {
		op   string
		path string
	}{
		{"getRuleSnapshot", snapshotPath(ruleContractOldSnapshotID) + "?verbose=true"},
		{"getOccurrenceRule", occurrenceRulePath(ruleContractOccurrenceID.String()) + "?verbose=true"},
		{"getAlertRuleHistory", alertRulePath(ruleContractAlertID.String()) + "?limt=1"},
	}

	for _, route := range routes {
		t.Run(route.op, func(t *testing.T) {
			f := newRuleContractFixture(t)
			resp := f.c.GET(route.path).MustStatus(t, http.StatusBadRequest)
			schema.AssertProblem(t, route.op, http.StatusBadRequest, resp.Body())

			if code := resp.Problem(t).Code; code != "unknown_parameter" {
				t.Fatalf("code = %q, want unknown_parameter", code)
			}
		})
	}
}
