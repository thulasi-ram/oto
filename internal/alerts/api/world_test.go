package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/contract/apitest"
)

// The harness for the twenty-three operations this package serves.
//
// # What these tests protect
//
// Every finding of the conformance audit lived in this layer, and every one of
// them was invisible for the same reason: nothing ever compared a response the
// handler ACTUALLY wrote against the schema the contract DECLARES. A missing
// `{data,meta}` envelope, a `delivery_summary` that was declared and never
// emitted, a `NotificationReason` the server wrote and the contract rejected —
// all three would have failed on the first request had one been made.
//
// So no test in this package re-states an expected shape by hand. The assertion
// is always `schema.Assert(t, <operationId>, <status>, resp.Body())`, which
// compiles the ONE contract and validates the real bytes. A test that spells the
// shape out itself is a second copy of the contract, and a second copy drifts.
//
// The fixtures below are deliberately RICH — timestamps, uuids, enums, an
// ended case, a suppressed one, a failed enrichment, a suppressed
// notification, an active snooze and an expired one — because `format:` and the
// closed enums are asserted, and a fixture full of zero values proves only that
// the empty case validates.
//
// ⛔ THIS FILE IS THE HARNESS, NOT DEAD CODE. Its consumers live in
// alerts_contract_test.go; deleting it because a linter cannot see through a
// test-only constructor deletes the only evidence twenty-three operations have.

/* -------------------------------------------------------------------------- */
/* The fake service port                                                      */
/* -------------------------------------------------------------------------- */

// fakeAlertsService is an AlertService whose whole memory is one tenant's world.
//
// ⛔ IT REFUSES EVERY ID IT DOES NOT OWN WITH A NOT-FOUND, which is exactly what
// the real repository does under `db.TenantScope`: the predicate is
// `WHERE org_id = $scope AND id = $id`, so another tenant's row does not come
// back as forbidden, it does not come back at all. That is what makes the
// tenant-scoping probe in alerts_contract_test.go a probe of the API layer rather
// than of the fake.
type fakeAlertsService struct {
	now time.Time

	// The ids this tenant owns. Anything else is a stranger.
	alertID      uuid.UUID
	alertKey     string
	caseID       uuid.UUID
	casePolicyID uuid.UUID

	detail        service.AlertDetail
	list          service.ListResult
	rollups       service.RollupResult
	cases         service.CaseResult
	caseList      service.CaseListResult
	alertCase     domain.Case
	timelineRes   service.TimelineResult
	enrichments   []service.EnrichmentSummary
	notifications service.NotificationResult
	delivery      service.DeliveryRollup
	snoozeHistory []domain.Snooze
	activeSnoozes service.ActiveSnoozeResult
	labelNames    []domain.LabelCount
	labelValues   []domain.LabelCount
	// The CASE RETENTION WINDOW rules (migration 00057). Two rows, so the list
	// proves the alertname-then-namespace order the endpoint promises.
	casePolicies     []domain.CasePolicy
	casePolicyCursor db.Cursor
	ackedCase        domain.Case
	commentEvent     domain.Event
	snoozeRow        domain.Snooze

	// Injected failures, for the branches where oto's silence must stay
	// distinguishable from an answer.
	failEnrichments error
	failCaseRollup  error
	// failVerb is what every human verb answers once the subject has been
	// resolved — the service's way of saying "this alert is in the wrong state
	// for that", which is a PRECONDITION failure and not a conflict.
	failVerb error

	// What the handlers actually asked for.
	lastScope         db.TenantScope
	lastActor         domain.Actor
	lastSnoozeUntil   time.Time
	lastCommentBody   string
	lastIdempotency   service.Idempotency
	lastAckNote       string
	lastListQuery     service.ListQuery
	lastRollupQuery   service.RollupQuery
	lastCaseListQuery service.CaseListQuery
	lastWindow        db.TimeWindow
	lastKeyset        db.Keyset
	lastLabelName     string
	lastLabelPrefix   string
	lastUnsnoozeIDs   []uuid.UUID
	lastUnsnoozeNote  string
	// What the case-policy handlers passed down, so a test can prove the mapper
	// converted seconds to a Duration and trimmed the namespace.
	lastCasePolicyDraft domain.CasePolicyDraft
	lastCasePolicyPatch domain.CasePolicyPatch
	lastLimit           int
	calls               map[string]int
}

func (f *fakeAlertsService) note(name string, s db.TenantScope) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[name]++
	f.lastScope = s
}

// strangerAlert is the refusal a tenant-scoped read makes for somebody else's
// alert id. It is a 404 and never a 403: a 403 would confirm the row exists.
func strangerAlert() error { return errs.NotFound("alert_not_found", "no such alert") }

func strangerCase() error {
	return errs.NotFound("case_not_found", "no such case")
}

func (f *fakeAlertsService) ownsAlert(id uuid.UUID) error {
	if id != f.alertID {
		return strangerAlert()
	}
	return nil
}

func (f *fakeAlertsService) ownsCase(id uuid.UUID) error {
	if id != f.caseID {
		return strangerCase()
	}
	return nil
}

func (f *fakeAlertsService) List(
	_ context.Context, s db.TenantScope, q service.ListQuery,
) (service.ListResult, error) {
	f.note("List", s)
	f.lastListQuery = q
	return f.list, nil
}

func (f *fakeAlertsService) Rollups(
	_ context.Context, s db.TenantScope, q service.RollupQuery,
) (service.RollupResult, error) {
	f.note("Rollups", s)
	f.lastRollupQuery = q
	return f.rollups, nil
}

func (f *fakeAlertsService) Get(
	_ context.Context, s db.TenantScope, alertID uuid.UUID,
) (service.AlertDetail, error) {
	f.note("Get", s)
	if err := f.ownsAlert(alertID); err != nil {
		return service.AlertDetail{}, err
	}
	return f.detail, nil
}

func (f *fakeAlertsService) GetByKey(
	_ context.Context, s db.TenantScope, alertKey string,
) (service.AlertDetail, error) {
	f.note("GetByKey", s)
	if alertKey != f.alertKey {
		return service.AlertDetail{}, strangerAlert()
	}
	return f.detail, nil
}

func (f *fakeAlertsService) Cases(
	_ context.Context, s db.TenantScope, alertID uuid.UUID, p db.Keyset,
) (service.CaseResult, error) {
	f.note("Cases", s)
	f.lastKeyset = p
	if err := f.ownsAlert(alertID); err != nil {
		return service.CaseResult{}, err
	}
	return f.cases, nil
}

func (f *fakeAlertsService) GetCase(
	_ context.Context, s db.TenantScope, caseID uuid.UUID,
) (domain.Case, error) {
	f.note("GetCase", s)
	if err := f.ownsCase(caseID); err != nil {
		return domain.Case{}, err
	}
	return f.alertCase, nil
}

func (f *fakeAlertsService) AlertTimeline(
	_ context.Context, s db.TenantScope, alertID uuid.UUID, w db.TimeWindow, p db.Keyset,
) (service.TimelineResult, error) {
	f.note("AlertTimeline", s)
	f.lastWindow, f.lastKeyset = w, p
	if err := f.ownsAlert(alertID); err != nil {
		return service.TimelineResult{}, err
	}
	return f.timelineRes, nil
}

func (f *fakeAlertsService) CaseTimeline(
	_ context.Context, s db.TenantScope, caseID uuid.UUID, w db.TimeWindow, p db.Keyset,
) (service.TimelineResult, error) {
	f.note("CaseTimeline", s)
	f.lastWindow, f.lastKeyset = w, p
	if err := f.ownsCase(caseID); err != nil {
		return service.TimelineResult{}, err
	}
	return f.timelineRes, nil
}

func (f *fakeAlertsService) Enrichments(
	_ context.Context, s db.TenantScope, alertID uuid.UUID,
) ([]service.EnrichmentSummary, error) {
	f.note("Enrichments", s)
	if f.failEnrichments != nil {
		return nil, f.failEnrichments
	}
	if err := f.ownsAlert(alertID); err != nil {
		return nil, err
	}
	return f.enrichments, nil
}

func (f *fakeAlertsService) Notifications(
	_ context.Context, s db.TenantScope, alertID uuid.UUID, p db.Keyset,
) (service.NotificationResult, error) {
	f.note("Notifications", s)
	f.lastKeyset = p
	if err := f.ownsAlert(alertID); err != nil {
		return service.NotificationResult{}, err
	}
	return f.notifications, nil
}

func (f *fakeAlertsService) DeliveryRollupForAlert(
	_ context.Context, s db.TenantScope, alertID uuid.UUID,
) (service.DeliveryRollup, error) {
	f.note("DeliveryRollupForAlert", s)
	if err := f.ownsAlert(alertID); err != nil {
		return service.DeliveryRollup{}, err
	}
	return f.delivery, nil
}

func (f *fakeAlertsService) DeliveryRollupForCase(
	_ context.Context, s db.TenantScope, caseID uuid.UUID,
) (service.DeliveryRollup, error) {
	f.note("DeliveryRollupForCase", s)
	if f.failCaseRollup != nil {
		return service.DeliveryRollup{}, f.failCaseRollup
	}
	if err := f.ownsCase(caseID); err != nil {
		return service.DeliveryRollup{}, err
	}
	return f.delivery, nil
}

func (f *fakeAlertsService) ListCases(
	_ context.Context, s db.TenantScope, q service.CaseListQuery,
) (service.CaseListResult, error) {
	f.note("ListCases", s)
	f.lastCaseListQuery = q
	return f.caseList, nil
}

func (f *fakeAlertsService) LabelNames(
	_ context.Context, s db.TenantScope, prefix string, limit int,
) ([]domain.LabelCount, error) {
	f.note("LabelNames", s)
	f.lastLabelPrefix, f.lastLimit = prefix, limit
	return f.labelNames, nil
}

func (f *fakeAlertsService) LabelValues(
	_ context.Context, s db.TenantScope, name, prefix string, limit int,
) ([]domain.LabelCount, error) {
	f.note("LabelValues", s)
	f.lastLabelName, f.lastLabelPrefix, f.lastLimit = name, prefix, limit
	return f.labelValues, nil
}

func (f *fakeAlertsService) Acknowledge(
	_ context.Context, s db.TenantScope, caseID uuid.UUID, actor domain.Actor, note string,
) (domain.Case, error) {
	f.note("Acknowledge", s)
	f.lastActor, f.lastAckNote = actor, note
	if err := f.ownsCase(caseID); err != nil {
		return domain.Case{}, err
	}
	if f.failVerb != nil {
		return domain.Case{}, f.failVerb
	}
	return f.ackedCase, nil
}

func (f *fakeAlertsService) Unacknowledge(
	_ context.Context, s db.TenantScope, caseID uuid.UUID, actor domain.Actor, note string,
) (domain.Case, error) {
	f.note("Unacknowledge", s)
	f.lastActor, f.lastAckNote = actor, note
	if err := f.ownsCase(caseID); err != nil {
		return domain.Case{}, err
	}
	if f.failVerb != nil {
		return domain.Case{}, f.failVerb
	}
	return f.alertCase, nil
}

func (f *fakeAlertsService) Comment(
	_ context.Context, s db.TenantScope, alertID uuid.UUID, actor domain.Actor, body string,
	idem service.Idempotency,
) (domain.Event, bool, error) {
	f.note("Comment", s)
	f.lastActor, f.lastCommentBody, f.lastIdempotency = actor, body, idem
	if err := f.ownsAlert(alertID); err != nil {
		return domain.Event{}, false, err
	}
	return f.commentEvent, false, nil
}

func (f *fakeAlertsService) Snooze(
	_ context.Context, s db.TenantScope, alertID uuid.UUID, actor domain.Actor,
	until time.Time, _ string, idem service.Idempotency,
) (domain.Snooze, bool, error) {
	f.note("Snooze", s)
	f.lastActor, f.lastSnoozeUntil, f.lastIdempotency = actor, until, idem
	if err := f.ownsAlert(alertID); err != nil {
		return domain.Snooze{}, false, err
	}
	if f.failVerb != nil {
		return domain.Snooze{}, false, f.failVerb
	}
	return f.snoozeRow, false, nil
}

func (f *fakeAlertsService) Unsnooze(
	_ context.Context, s db.TenantScope, alertID uuid.UUID, actor domain.Actor, _ string,
) (domain.Snooze, error) {
	f.note("Unsnooze", s)
	f.lastActor = actor
	if err := f.ownsAlert(alertID); err != nil {
		return domain.Snooze{}, err
	}
	if f.failVerb != nil {
		return domain.Snooze{}, f.failVerb
	}
	return f.snoozeRow, nil
}

// UnsnoozeMany is the fake's copy of the BULK wake's classification rule: a
// stranger's id and an alert in the wrong state are both SKIPPED with the code the
// real service would have refused with, and neither fails the request. The real
// rule lives in service.UnsnoozeMany; this restates it so the handler can be
// driven without a database, exactly as `ownsAlert` restates tenant scoping.
func (f *fakeAlertsService) UnsnoozeMany(
	_ context.Context, s db.TenantScope, alertIDs []uuid.UUID, actor domain.Actor, note string,
) (service.UnsnoozeManyResult, error) {
	f.note("UnsnoozeMany", s)
	f.lastActor, f.lastUnsnoozeIDs, f.lastUnsnoozeNote = actor, alertIDs, note

	res := service.UnsnoozeManyResult{Outcomes: make([]service.UnsnoozeOutcome, 0, len(alertIDs))}
	for _, id := range alertIDs {
		switch {
		case f.ownsAlert(id) != nil:
			res.Outcomes = append(res.Outcomes,
				service.UnsnoozeOutcome{AlertID: id, Code: "alert_not_found"})
		case f.failVerb != nil:
			res.Outcomes = append(res.Outcomes,
				service.UnsnoozeOutcome{AlertID: id, Code: errs.CodeOf(f.failVerb)})
		default:
			res.Outcomes = append(res.Outcomes, service.UnsnoozeOutcome{AlertID: id, Woken: true})
		}
	}
	return res, nil
}

func (f *fakeAlertsService) SnoozeHistory(
	_ context.Context, s db.TenantScope, alertID uuid.UUID, limit int,
) ([]domain.Snooze, error) {
	f.note("SnoozeHistory", s)
	f.lastLimit = limit
	if err := f.ownsAlert(alertID); err != nil {
		return nil, err
	}
	return f.snoozeHistory, nil
}

func (f *fakeAlertsService) ActiveSnoozes(
	_ context.Context, s db.TenantScope, p db.Keyset,
) (service.ActiveSnoozeResult, error) {
	f.note("ActiveSnoozes", s)
	f.lastKeyset = p
	return f.activeSnoozes, nil
}

// strangerCasePolicy is the refusal a tenant-scoped read makes for somebody
// else's retention-rule id — a 404, never a 403.
func strangerCasePolicy() error {
	return errs.NotFound("case_policy_not_found", "no such case retention policy")
}

func (f *fakeAlertsService) ownsCasePolicy(id uuid.UUID) error {
	if id != f.casePolicyID {
		return strangerCasePolicy()
	}
	return nil
}

func (f *fakeAlertsService) CasePolicies(
	_ context.Context, s db.TenantScope, p db.Keyset,
) ([]domain.CasePolicy, db.Cursor, error) {
	f.note("CasePolicies", s)
	f.lastKeyset = p
	return f.casePolicies, f.casePolicyCursor, nil
}

func (f *fakeAlertsService) CreateCasePolicy(
	_ context.Context, s db.TenantScope, in domain.CasePolicyDraft, idem service.Idempotency,
) (domain.CasePolicy, bool, error) {
	f.note("CreateCasePolicy", s)
	f.lastCasePolicyDraft = in
	f.lastIdempotency = idem
	if f.failVerb != nil {
		return domain.CasePolicy{}, false, f.failVerb
	}
	return domain.CasePolicy{
		ID:              f.casePolicyID,
		Namespace:       in.Namespace,
		Alertname:       in.Alertname,
		RetentionWindow: in.RetentionWindow,
		CreatedAt:       f.now,
		UpdatedAt:       f.now,
	}, false, nil
}

func (f *fakeAlertsService) UpdateCasePolicy(
	_ context.Context, s db.TenantScope, policyID uuid.UUID, p domain.CasePolicyPatch,
) (domain.CasePolicy, error) {
	f.note("UpdateCasePolicy", s)
	f.lastCasePolicyPatch = p
	if err := f.ownsCasePolicy(policyID); err != nil {
		return domain.CasePolicy{}, err
	}
	out := f.casePolicies[0]
	if p.RetentionWindow != nil {
		out.RetentionWindow = *p.RetentionWindow
	}
	return out, nil
}

func (f *fakeAlertsService) DeleteCasePolicy(
	_ context.Context, s db.TenantScope, policyID uuid.UUID,
) error {
	f.note("DeleteCasePolicy", s)
	return f.ownsCasePolicy(policyID)
}

// The fake really does satisfy the port the handlers were written against; if it
// ever stops, this line fails before any test does.
var _ AlertService = (*fakeAlertsService)(nil)

/* -------------------------------------------------------------------------- */
/* The world                                                                  */
/* -------------------------------------------------------------------------- */

// The ids the fixtures are built from. They are CONSTANTS rather than freshly
// generated: a failure message that names a different uuid on every run cannot be
// reproduced from the log it was found in.
var (
	fxAlertID     = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b77")
	fxClusterID   = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b78")
	fxCase        = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b79")
	fxEndedOccID  = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b7b")
	fxSuppOccID   = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b7c")
	fxGroupID     = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b7d")
	fxSnapshotID  = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b7e")
	fxPolicyID    = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b7f")
	fxSnoozeID    = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b80")
	fxOldSnoozeID = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b81")
	fxOtherSnooze = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b82")
	fxOtherAlert  = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b83")
	fxEnrichOK    = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b84")
	fxEnrichFail  = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b85")
	fxNotifSent   = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b86")
	fxNotifQuiet  = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b87")
	fxEventOpened = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b88")
	fxEventAcked  = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b89")
	fxEventNote   = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b8a")
	fxCursorID    = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b8b")
	// The two `case_policy_config` rows the fixture world holds.
	fxCasePolicyID  = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b8c")
	fxCasePolicy2ID = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b8d")
)

// fxLabels is the label set every fixture Alert carries. It exercises the
// promoted labels AND an arbitrary one, because `promoted` on the typeahead is
// computed from that distinction.
func fxLabels(t *testing.T, alertname string) domain.LabelSet {
	t.Helper()
	ls, err := domain.NewLabelSet(map[string]string{
		"alertname": alertname,
		"severity":  "critical",
		"namespace": "payments",
		"service":   "checkout-api",
		"cluster":   "prod-eu",
		"team":      "payments",
		"instance":  "api-7f9c-2x4k:9100",
	})
	if err != nil {
		t.Fatalf("build the fixture label set: %v", err)
	}
	return ls
}

func fxAnnotations(t *testing.T) domain.Annotations {
	t.Helper()
	an, err := domain.NewAnnotations(map[string]string{
		"summary":     "Error rate above 5% for 10m",
		"description": "api-7f9c-2x4k error rate is 12.4%",
		"runbook_url": "https://runbooks.example.com/HighErrorRate",
	})
	if err != nil {
		t.Fatalf("build the fixture annotations: %v", err)
	}
	return an
}

func fxClusterKey(t *testing.T) domain.ClusterKey {
	t.Helper()
	k, err := domain.NewClusterKey("prod-eu")
	if err != nil {
		t.Fatalf("build the fixture cluster key: %v", err)
	}
	return k
}

// fxAlert builds an Alert whose every optional-but-typed member is populated:
// a severity, a namespace, a service, a generator URL that is a real URI, and
// three timestamps that are ordered. `format: uri` and `format: date-time` are
// asserted by the schema, so a fixture that left any of them empty would prove
// only that `null` validates.
func fxAlert(t *testing.T, id uuid.UUID, alertname string, now time.Time) domain.Alert {
	t.Helper()

	ls := fxLabels(t, alertname)
	ck := fxClusterKey(t)
	a, err := domain.NewAlert(domain.AlertParams{
		ID:                id,
		OrgID:             apitest.OrgID,
		ClusterID:         fxClusterID,
		Key:               domain.ComputeAlertKey(apitest.OrgID, ck, ls, nil),
		Fingerprint:       domain.ComputeSourceFingerprint(ls),
		ClusterKey:        ck,
		Labels:            ls,
		Annotations:       fxAnnotations(t),
		GeneratorURL:      "https://prometheus.example.com/graph?g0.expr=up%3D%3D0&g0.tab=1",
		State:             domain.StateFiring,
		CurrentCaseID:     fxCase,
		FirstSeenAt:       now.Add(-72 * time.Hour),
		LastSeenAt:        now.Add(-5 * time.Minute),
		LastStateChangeAt: now.Add(-2 * time.Hour),
		TotalCases:        7,
		FlapScore:         1.4,
		IsFlapping:        false,
	})
	if err != nil {
		t.Fatalf("build the fixture alert: %v", err)
	}
	return a
}

// fxOpenCase is the episode currently running: open, acked, at `seq` 3 — so it
// is the third episode of this alert and two ended before it — with a sample
// value and a measured clock skew.
func fxOpenCase(t *testing.T, now time.Time) domain.Case {
	t.Helper()

	value := 12.4
	o, err := domain.NewCase(domain.CaseParams{
		ID:              fxCase,
		OrgID:           apitest.OrgID,
		AlertID:         fxAlertID,
		GroupID:         fxGroupID,
		Seq:             3,
		State:           domain.CaseOpen,
		StartedAt:       now.Add(-2 * time.Hour),
		LastObservedAt:  now.Add(-1 * time.Minute),
		SourceStartsAt:  now.Add(-2*time.Hour - time.Minute),
		SourceUpdatedAt: now.Add(-1 * time.Minute),
		StateVersion:    4,
		AckState:        domain.AckStateAcked,
		AckedBy:         apitest.UserID,
		AckedByLabel:    "Ada Lovelace",
		AckedAt:         now.Add(-30 * time.Minute),
		AckNote:         "Known deploy, rolling back",
		RuleSnapshotID:  fxSnapshotID,
		Value:           &value,
		ObservedSkew:    412 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("build the open fixture case: %v", err)
	}
	return o
}

// fxEndedCase is a closed episode, so `ended_at`, `resolve_reason` and
// `source_ends_at` are exercised rather than always rendering as null.
func fxEndedCase(t *testing.T, now time.Time) domain.Case {
	t.Helper()

	o, err := domain.NewCase(domain.CaseParams{
		ID:             fxEndedOccID,
		OrgID:          apitest.OrgID,
		AlertID:        fxAlertID,
		GroupID:        fxGroupID,
		Seq:            2,
		State:          domain.CaseClosed,
		StartedAt:      now.Add(-30 * time.Hour),
		EndedAt:        now.Add(-28 * time.Hour),
		LastObservedAt: now.Add(-28 * time.Hour),
		SourceStartsAt: now.Add(-30 * time.Hour),
		SourceEndsAt:   now.Add(-28 * time.Hour),
		ResolveReason:  domain.ResolveUpstream,
		StateVersion:   9,
	})
	if err != nil {
		t.Fatalf("build the ended fixture case: %v", err)
	}
	return o
}

// fxSuppressedCase exercises the two members that exist only while an
// episode is suppressed: `suppression_reason` and the upstream witnesses that
// say WHICH silence is muting it.
func fxSuppressedCase(t *testing.T, now time.Time) domain.Case {
	t.Helper()

	o, err := domain.NewCase(domain.CaseParams{
		ID:      fxSuppOccID,
		OrgID:   apitest.OrgID,
		AlertID: fxAlertID,
		GroupID: fxGroupID,
		Seq:     1,
		// ⭐ SUPPRESSED IS NOT A STORED STATE (ADR 0040): it is an OPEN episode with a
		// suppression reason, and `AlertState` is what reads the pair back as
		// `suppressed`. The fixture has to be built the way the column is written or
		// it would prove the mapper against a row the database cannot hold.
		State:             domain.CaseOpen,
		SuppressionReason: domain.SuppressionSilence,
		SuppressedBy: domain.SuppressedBy{
			SilencedBy: []string{"1c9e5c9a-6e9d-4b1e-9e1a-2d5f3a7b9c11"},
		},
		StartedAt:      now.Add(-50 * time.Hour),
		LastObservedAt: now.Add(-49 * time.Hour),
		SourceStartsAt: now.Add(-50 * time.Hour),
		StateVersion:   2,
		SuppressCount:  1,
	})
	if err != nil {
		t.Fatalf("build the suppressed fixture case: %v", err)
	}
	return o
}

func fxMachineEvent(t *testing.T, id uuid.UUID, typ domain.EventType, summary string, now time.Time) domain.Event {
	t.Helper()

	actor, err := domain.SystemActor(domain.ActorIngest)
	if err != nil {
		t.Fatalf("build the fixture ingest actor: %v", err)
	}
	at, err := domain.NewObservationTime(now.Add(-2*time.Hour), now.Add(-2*time.Hour).Add(412*time.Millisecond))
	if err != nil {
		t.Fatalf("build the fixture observation time: %v", err)
	}
	e, err := domain.NewEvent(domain.EventParams{
		ID:      id,
		OrgID:   apitest.OrgID,
		AlertID: fxAlertID,
		CaseID:  fxCase,
		GroupID: fxGroupID,
		Type:    typ,
		At:      at,
		Actor:   actor,
		Summary: summary,
		Payload: map[string]any{"instances": 4},
	})
	if err != nil {
		t.Fatalf("build the fixture event: %v", err)
	}
	return e
}

// fxHumanEvent carries an actor id and label, which the contract requires for
// `user` and `slack` actors and which nothing else in the fixtures exercises.
func fxHumanEvent(t *testing.T, now time.Time) domain.Event {
	t.Helper()

	actor, err := domain.NewActor(domain.ActorUser, apitest.UserID.String(), "Ada Lovelace")
	if err != nil {
		t.Fatalf("build the fixture human actor: %v", err)
	}
	at, err := domain.NewObservationTime(now.Add(-25*time.Minute), now.Add(-25*time.Minute))
	if err != nil {
		t.Fatalf("build the fixture observation time: %v", err)
	}
	e, err := domain.NewEvent(domain.EventParams{
		ID:      fxEventNote,
		OrgID:   apitest.OrgID,
		AlertID: fxAlertID,
		CaseID:  fxCase,
		Type:    domain.EventCommentAdded,
		At:      at,
		Actor:   actor,
		Summary: "Ada Lovelace commented",
		Payload: map[string]any{"body": "Confirmed upstream provider incident."},
	})
	if err != nil {
		t.Fatalf("build the fixture comment event: %v", err)
	}
	return e
}

// fxActiveSnooze is the quiet period in force. Its window is four hours, which
// is inside the §B.8.3 bounds of five minutes to thirty days — a fixture outside
// them would not construct at all, which is the point of the bounds.
func fxActiveSnooze(t *testing.T, id, alertID uuid.UUID, key domain.AlertKey, now time.Time) domain.Snooze {
	t.Helper()

	s, err := domain.NewSnooze(domain.SnoozeParams{
		ID:             id,
		OrgID:          apitest.OrgID,
		AlertID:        alertID,
		AlertKey:       key,
		SnoozedAt:      now.Add(-time.Hour),
		SnoozedUntil:   now.Add(3 * time.Hour),
		SnoozedBy:      apitest.UserID,
		SnoozedByLabel: "ada@example.com",
		Note:           "deploy window, expected until 17:00",
	})
	if err != nil {
		t.Fatalf("build the active fixture snooze: %v", err)
	}
	return s
}

// fxEndedSnooze is a snooze somebody woke early. It exists because membership of
// a snooze is HISTORY, not a boolean: `ended_reason` and `ended_by_label` are
// only ever populated on a row like this one.
func fxEndedSnooze(t *testing.T, key domain.AlertKey, now time.Time) domain.Snooze {
	t.Helper()

	s, err := domain.NewSnooze(domain.SnoozeParams{
		ID:             fxOldSnoozeID,
		OrgID:          apitest.OrgID,
		AlertID:        fxAlertID,
		AlertKey:       key,
		SnoozedAt:      now.Add(-30 * time.Hour),
		SnoozedUntil:   now.Add(-25 * time.Hour),
		SnoozedBy:      apitest.UserID,
		SnoozedByLabel: "ada@example.com",
		Note:           "overnight batch",
		EndedAt:        now.Add(-26 * time.Hour),
		EndedReason:    domain.SnoozeEndedManual,
		EndedBy:        apitest.UserID,
		EndedByLabel:   "Ada Lovelace",
	})
	if err != nil {
		t.Fatalf("build the ended fixture snooze: %v", err)
	}
	return s
}

// newAlertsWorld assembles one tenant's alerts, cases, events,
// enrichments, notifications and snoozes.
func newAlertsWorld(t *testing.T) *fakeAlertsService {
	t.Helper()

	// A real instant rather than a distant fixed one, so that `elapsed_ms` on
	// every envelope stays a plausible small number while the fixtures around it
	// remain deterministic relative to it.
	now := time.Now().UTC().Truncate(time.Millisecond)

	alert := fxAlert(t, fxAlertID, "HighErrorRate", now)
	other := fxAlert(t, fxOtherAlert, "KubePodCrashLooping", now)
	open := fxOpenCase(t, now)
	ended := fxEndedCase(t, now)
	suppressed := fxSuppressedCase(t, now)
	snooze := fxActiveSnooze(t, fxSnoozeID, fxAlertID, alert.Key(), now)

	lastSent := now.Add(-9 * time.Minute)
	expires := now.Add(55 * time.Minute)
	updated := now.Add(-8 * time.Minute)

	cursor := db.Cursor{SortKey: now.Add(-time.Hour), ID: fxCursorID, HasMore: true}

	f := &fakeAlertsService{
		now:          now,
		alertID:      fxAlertID,
		alertKey:     alert.Key().String(),
		caseID:       fxCase,
		casePolicyID: fxCasePolicyID,

		// ⭐ THE SECOND ROW CARRIES THE EMPTY NAMESPACE, which is the
		// absent-namespace partition and not a missing value. A fixture with only a
		// populated namespace would never notice a serialiser that dropped `""`.
		casePolicies: []domain.CasePolicy{
			{
				ID:              fxCasePolicyID,
				Namespace:       "production",
				Alertname:       "HighErrorRate",
				RetentionWindow: 10 * time.Minute,
				CreatedAt:       now.Add(-time.Hour),
				UpdatedAt:       now.Add(-time.Minute),
			},
			{
				ID:              fxCasePolicy2ID,
				Namespace:       "",
				Alertname:       "KubePodCrashLooping",
				RetentionWindow: 0,
				CreatedAt:       now.Add(-2 * time.Hour),
				UpdatedAt:       now.Add(-2 * time.Hour),
			},
		},
		casePolicyCursor: db.Cursor{ID: fxCasePolicy2ID, HasMore: true},

		detail: service.AlertDetail{
			Alert:       alert,
			CurrentCase: &open,
			LatestCase:  &open,
			Snooze:      &snooze,
			SnoozedNow:  true,
		},
		list: service.ListResult{
			Alerts:  []domain.Alert{alert, other},
			Snoozes: map[uuid.UUID]domain.Snooze{fxAlertID: snooze},
			Cursor:  cursor,
		},
		rollups: service.RollupResult{
			Rollups: []domain.AlertRollup{{
				Key:            "KubePodCrashLooping",
				Total:          47,
				Firing:         31,
				Suppressed:     4,
				Resolved:       8,
				Expired:        4,
				Flapping:       3,
				SeverityCounts: map[string]int{"critical": 31, "warning": 16},
				FirstSeenAt:    now.Add(-96 * time.Hour),
				LastSeenAt:     now.Add(-3 * time.Minute),
			}},
			HasMore: true,
		},
		cases: service.CaseResult{
			Cases:  []domain.Case{open, ended, suppressed},
			Cursor: cursor,
		},
		// The ORG-WIDE list carries the same three episodes plus the identity
		// each belongs to, because that is what the row shape is: an episode has
		// no `alertname` and no `severity` of its own.
		caseList: service.CaseListResult{
			Cases:  []domain.Case{open, ended, suppressed},
			Alerts: map[uuid.UUID]domain.Alert{alert.ID(): alert},
			Cursor: cursor,
		},
		alertCase: open,
		timelineRes: service.TimelineResult{
			Events: []domain.Event{
				fxMachineEvent(t, fxEventOpened, domain.EventCaseOpened,
					"Case #3 opened — 4 instances firing", now),
				fxMachineEvent(t, fxEventAcked, domain.EventCaseAcknowledged,
					"Acknowledged by Ada Lovelace", now),
				fxHumanEvent(t, now),
			},
			Cursor: cursor,
		},
		enrichments: []service.EnrichmentSummary{
			{
				ID:              fxEnrichOK,
				SubjectKind:     "case",
				SubjectID:       fxCase,
				Enricher:        "alert.history",
				EnricherVersion: 2,
				Phase:           1,
				Status:          "ok",
				Payload: map[string]any{
					"headline": "7 episodes in the last 30 days",
					"episodes": 7,
				},
				Warnings:   []string{"history truncated at 30 days"},
				DurationMS: 143,
				FromCache:  false,
				ComputedAt: now.Add(-2 * time.Hour),
				ExpiresAt:  &expires,
			},
			{
				// A FAILED enrichment is recorded, never discarded — which is why
				// `status` exists and why `error` is non-null exactly here.
				ID:              fxEnrichFail,
				SubjectKind:     "case",
				SubjectID:       fxCase,
				Enricher:        "silence.match",
				EnricherVersion: 1,
				Phase:           2,
				Status:          "failed",
				Payload:         map[string]any{},
				Error:           "alertmanager did not answer within 2000 ms",
				DurationMS:      2000,
				FromCache:       false,
				ComputedAt:      now.Add(-2 * time.Hour),
			},
		},
		notifications: service.NotificationResult{
			Notifications: []service.NotificationSummary{
				{
					ID:                fxNotifSent,
					GroupID:           fxGroupID,
					AlertID:           &fxAlertID,
					CaseID:            &fxCase,
					PolicyID:          &fxPolicyID,
					Reason:            "fired",
					Status:            "delivered",
					StateVersion:      3,
					CreatedAt:         now.Add(-2 * time.Hour),
					UpdatedAt:         updated,
					DeliveriesTotal:   4,
					DeliveriesSent:    3,
					DeliveriesFailed:  1,
					DeliveriesSkipped: 1,
				},
				{
					// A SUPPRESSED intent, in oto's own suppression vocabulary. Its
					// `updated_at` is deliberately absent so the null renders.
					ID:               fxNotifQuiet,
					GroupID:          fxGroupID,
					AlertID:          &fxAlertID,
					Reason:           "repeat",
					Status:           "suppressed",
					SuppressedReason: "snoozed",
					StateVersion:     4,
					CreatedAt:        now.Add(-30 * time.Minute),
					DeliveriesTotal:  2,
				},
			},
			Cursor: cursor,
		},
		delivery: service.DeliveryRollup{
			Total: 6, Sent: 4, Failed: 1, Dead: 1, Skipped: 1, Pending: 0,
			// ⛔ One of the FIVE §G.6 classes and never a shorthand: the contract
			// closes this enum, and `notification/domain` only ever produces
			// `retryable`, `rate_limited`, `permanent`, `config_invalid` or
			// `auth_expired`. A fixture outside that set is a row the repository
			// could not hand a handler.
			LastErrorClass: "auth_expired", LastSentAt: &lastSent,
		},
		snoozeHistory: []domain.Snooze{snooze, fxEndedSnooze(t, alert.Key(), now)},
		activeSnoozes: service.ActiveSnoozeResult{
			Snoozes: []domain.Snooze{
				snooze,
				// A snooze whose Alert could not be read is STILL LISTED, with a
				// null `alert`. Dropping the row would hide a quiet period, which
				// is the exact failure this endpoint exists to prevent.
				fxActiveSnooze(t, fxOtherSnooze, fxOtherAlert, other.Key(), now),
			},
			Alerts: map[string]domain.Alert{alert.Key().String(): alert},
			Cursor: cursor,
		},
		labelNames: []domain.LabelCount{
			{Value: "namespace", Count: 1420},
			{Value: "team", Count: 87},
		},
		labelValues: []domain.LabelCount{
			{Value: "payments", Count: 42},
			{Value: "checkout", Count: 7},
		},
		ackedCase:    open,
		commentEvent: fxHumanEvent(t, now),
		snoozeRow:    snooze,
	}
	return f
}

// newAlertsProbe mounts the alerts router on a chi tree with the request-id
// middleware and returns a client calling as an ordinary org member.
//
// ⛔ The middleware is not decoration. `meta.request_id` is a REQUIRED member of
// every success envelope and `httpx.Meta` marshals it with `omitempty`, so a
// response produced without it fails its own schema — which would make every
// assertion in this package a test of the harness rather than of the handler.
func newAlertsProbe(t *testing.T) (*apitest.Client, *fakeAlertsService) {
	t.Helper()

	svc := newAlertsWorld(t)
	return apitest.New(NewRouter(svc, clock.NewFake(svc.now))), svc
}

// fxAlertKey is the §C.2 key of the fixture alert, which `getAlert` accepts in
// place of the uuid.
func fxAlertKey(t *testing.T) string {
	t.Helper()
	return fxAlert(t, fxAlertID, "HighErrorRate", time.Now().UTC()).Key().String()
}
