package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	alertdomain "github.com/thulasiram/oto/internal/alerts/domain"
	alertsvc "github.com/thulasiram/oto/internal/alerts/service"
	"github.com/thulasiram/oto/internal/grouping/domain"
	gsvc "github.com/thulasiram/oto/internal/grouping/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// The eight declared operations of the Groups tag, checked against the ONE
// contract rather than against a second hand-written copy of it.
//
// Every finding of the conformance audit lived in this layer — an envelope that
// had lost its `{data,meta}`, a DTO missing a required member, an enum value the
// server emitted and the contract rejected — and the causal chain was one link
// long: nothing had ever compared a response body to the schema that describes
// it. `schema.Assert` closes that. When someone adds a required member to
// `GroupDTO`, every handler here that fails to emit it fails in this package,
// with the JSON pointer of the missing member in the message.
//
// The second promise these tests hold is the tenant boundary. v1 has no roles,
// so the org is the ONLY boundary there is: an id belonging to another tenant
// must answer 404 — not 403, which confirms the row exists, and never that
// tenant's data.

/* -------------------------------------------------------------------------- */
/* Fixtures                                                                   */
/* -------------------------------------------------------------------------- */

// The ids every test in this file is written against. They are CONSTANTS: a
// failure message naming a freshly generated uuid cannot be reproduced from the
// failure message alone, which is the whole value of a tenant-scoping probe.
var (
	fxGroupID    = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	fxAlertID    = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	fxAlertBID   = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	fxSourceID   = uuid.MustParse("44444444-4444-4444-8444-444444444444")
	fxClusterID  = uuid.MustParse("55555555-5555-4555-8555-555555555555")
	fxOccID      = uuid.MustParse("66666666-6666-4666-8666-666666666666")
	fxOccBID     = uuid.MustParse("77777777-7777-4777-8777-777777777777")
	fxEventID    = uuid.MustParse("88888888-8888-4888-8888-888888888888")
	fxCommentID  = uuid.MustParse("99999999-9999-4999-8999-999999999999")
	fxSnoozeID   = uuid.MustParse("aaaaaaa1-aaaa-4aaa-8aaa-aaaaaaaaaaa1")
	fxOtherGroup = apitest.StrangerID
)

// fxNow is the one instant the fixtures are built around, so that every
// timestamp on the wire is a real RFC 3339 instant and `format: date-time` is
// asserted against something rather than against a zero value.
var fxNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// fxGroup builds one generation with every optional member populated.
//
// Rich on purpose: `format: date-time`, the `gk_` and cluster-key patterns and
// the int32 count bounds are all asserted, and a fixture of zero values would
// exercise none of them.
func fxGroup(t *testing.T) domain.Group {
	t.Helper()

	key, err := domain.NewGroupKey("gk_4b7n2p9v1c6d3f0h8j5k2m0q7s")
	if err != nil {
		t.Fatalf("build a group key: %v", err)
	}
	g, err := domain.NewGroup(domain.GroupParams{
		ID:             fxGroupID,
		OrgID:          apitest.OrgID,
		SourceID:       fxSourceID,
		ClusterID:      fxClusterID,
		ClusterKey:     "prod-eu",
		Key:            key,
		Generation:     2,
		SourceGroupKey: `{}/{severity="critical"}:{alertname="HighErrorRate"}`,
		Receiver:       "oto-webhook",
		GroupLabels: map[string]string{
			"alertname": "HighErrorRate",
			"cluster":   "prod-eu",
			"severity":  "critical",
		},
		Title:                  "HighErrorRate · prod-eu",
		State:                  domain.StateOpen,
		Severity:               "critical",
		StateVersion:           7,
		Counts:                 domain.Counts{Firing: 3, Suppressed: 1, Resolved: 1, Total: 5, Acked: 2},
		StormMode:              true,
		StormSince:             fxNow.Add(-10 * time.Minute),
		LastNotificationReason: "new alerts added",
		FirstSeenAt:            fxNow.Add(-2 * time.Hour),
		LastActivityAt:         fxNow.Add(-5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("build a group: %v", err)
	}
	return g
}

// fxAlert builds one member alert. `alertKey` and `fingerprint` are spelled out
// rather than computed because the contract constrains their SHAPE, and a
// computed value would silently satisfy a pattern nobody had read.
func fxAlert(t *testing.T, id uuid.UUID, alertKey, fingerprint, name, severity string) alertdomain.Alert {
	t.Helper()

	ak, err := alertdomain.NewAlertKey(alertKey)
	if err != nil {
		t.Fatalf("build an alert key: %v", err)
	}
	fp, err := alertdomain.NewSourceFingerprint(fingerprint)
	if err != nil {
		t.Fatalf("build a source fingerprint: %v", err)
	}
	ck, err := alertdomain.NewClusterKey("prod-eu")
	if err != nil {
		t.Fatalf("build a cluster key: %v", err)
	}
	labels, err := alertdomain.NewLabelSet(map[string]string{
		"alertname": name,
		"cluster":   "prod-eu",
		"namespace": "payments",
		"service":   "checkout-api",
		"severity":  severity,
	})
	if err != nil {
		t.Fatalf("build a label set: %v", err)
	}
	annotations, err := alertdomain.NewAnnotations(map[string]string{
		"summary":     "Error rate above 5% for 10m",
		"runbook_url": "https://runbooks.example.com/HighErrorRate",
	})
	if err != nil {
		t.Fatalf("build annotations: %v", err)
	}
	state, err := alertdomain.NewState("firing")
	if err != nil {
		t.Fatalf("build an alert state: %v", err)
	}
	ack, err := alertdomain.NewAckState("acked")
	if err != nil {
		t.Fatalf("build an ack state: %v", err)
	}

	a, err := alertdomain.NewAlert(alertdomain.AlertParams{
		ID:                id,
		OrgID:             apitest.OrgID,
		ClusterID:         fxClusterID,
		Key:               ak,
		Fingerprint:       fp,
		ClusterKey:        ck,
		Labels:            labels,
		Annotations:       annotations,
		GeneratorURL:      "https://prometheus.example.com/graph?g0.expr=up&g0.tab=1",
		State:             state,
		AckState:          ack,
		FirstSeenAt:       fxNow.Add(-3 * time.Hour),
		LastSeenAt:        fxNow.Add(-1 * time.Minute),
		LastStateChangeAt: fxNow.Add(-90 * time.Minute),
		TotalOccurrences:  7,
		FlapScore:         1.4,
	})
	if err != nil {
		t.Fatalf("build an alert: %v", err)
	}
	return a
}

// fxSnooze builds an ACTIVE snooze on a member alert.
//
// It is here because `snooze` on a member row is the ONLY place the group
// snooze fan-out's own effect is visible: without it the button could be
// pressed and never be seen to have worked.
func fxSnooze(t *testing.T, alertID uuid.UUID, alertKey string) *alertdomain.Snooze {
	t.Helper()

	ak, err := alertdomain.NewAlertKey(alertKey)
	if err != nil {
		t.Fatalf("build an alert key: %v", err)
	}
	s, err := alertdomain.NewSnooze(alertdomain.SnoozeParams{
		ID:             fxSnoozeID,
		OrgID:          apitest.OrgID,
		AlertID:        alertID,
		AlertKey:       ak,
		SnoozedAt:      fxNow.Add(-30 * time.Minute),
		SnoozedUntil:   fxNow.Add(4 * time.Hour),
		SnoozedBy:      apitest.UserID,
		SnoozedByLabel: "ada@example.com",
		Note:           "deploy window, expected until 17:00",
	})
	if err != nil {
		t.Fatalf("build a snooze: %v", err)
	}
	return &s
}

// fxMember builds one membership row.
func fxMember(t *testing.T, alertID, occurrenceID uuid.UUID, joinedAt time.Time) domain.Member {
	t.Helper()

	m, err := domain.NewMember(domain.MemberParams{
		GroupID:      fxGroupID,
		OccurrenceID: occurrenceID,
		OrgID:        apitest.OrgID,
		AlertID:      alertID,
		JoinedAt:     joinedAt,
	})
	if err != nil {
		t.Fatalf("build a member: %v", err)
	}
	return m
}

// fxEvent builds one timeline entry, attributed to a human so that `actor_id`
// and `actor_label` are exercised rather than being null on every row.
func fxEvent(t *testing.T, id uuid.UUID, typ alertdomain.EventType, summary string) alertdomain.Event {
	t.Helper()

	at, err := alertdomain.NewObservationTime(fxNow.Add(-20*time.Minute), fxNow.Add(-19*time.Minute))
	if err != nil {
		t.Fatalf("build an observation time: %v", err)
	}
	actor, err := alertdomain.NewActor(alertdomain.ActorUser, apitest.UserID.String(), "Ada Lovelace")
	if err != nil {
		t.Fatalf("build an actor: %v", err)
	}
	e, err := alertdomain.NewEvent(alertdomain.EventParams{
		ID:           id,
		OrgID:        apitest.OrgID,
		AlertID:      fxAlertID,
		OccurrenceID: fxOccID,
		GroupID:      fxGroupID,
		Type:         typ,
		At:           at,
		Actor:        actor,
		Summary:      summary,
		Payload:      map[string]any{"reason": "manual"},
	})
	if err != nil {
		t.Fatalf("build an event: %v", err)
	}
	return e
}

/* -------------------------------------------------------------------------- */
/* Fakes                                                                      */
/* -------------------------------------------------------------------------- */

// fxNotFound is what every port in this file answers for an id it does not own.
//
// ⛔ It is a 404 and not a 403, and that is the point of the tenant probe below:
// a 403 tells an unauthenticated scanner that the row exists in SOMEBODY's org,
// which is the leak the boundary exists to prevent.
func fxNotFound() error {
	return errs.NotFound("not_found", "no such alert group")
}

// fakeGroupService is a GroupService whose whole memory is "which group id do I
// own". Everything else is a canned answer, because what is under test is the
// HANDLER — the envelope it writes, the status it chooses and the id it refuses.
type fakeGroupService struct {
	owned   uuid.UUID
	detail  gsvc.Detail
	page    []domain.Group
	snoozes map[uuid.UUID]domain.SnoozeRollup
	members []domain.Member
	events  []alertdomain.Event
	comment alertdomain.Event

	// ackApplied is how many member occurrences accepted the ack. Zero is the
	// 412 branch: a group with nothing open to acknowledge.
	ackApplied int
	// snoozeMembers is how many currently-joined members the snooze reached.
	// Zero is the 412 branch.
	snoozeMembers int

	listErr error
}

func (f *fakeGroupService) own(id uuid.UUID) error {
	if id != f.owned {
		return fxNotFound()
	}
	return nil
}

func (f *fakeGroupService) List(context.Context, db.TenantScope, gsvc.ListQuery) (gsvc.ListResult, error) {
	if f.listErr != nil {
		return gsvc.ListResult{}, f.listErr
	}
	return gsvc.ListResult{Groups: f.page, Snoozes: f.snoozes}, nil
}

func (f *fakeGroupService) Get(_ context.Context, _ db.TenantScope, id uuid.UUID) (gsvc.Detail, error) {
	if err := f.own(id); err != nil {
		return gsvc.Detail{}, err
	}
	return f.detail, nil
}

func (f *fakeGroupService) Members(
	_ context.Context, _ db.TenantScope, id uuid.UUID, _ db.Keyset,
) (gsvc.MemberResult, error) {
	if err := f.own(id); err != nil {
		return gsvc.MemberResult{}, err
	}
	return gsvc.MemberResult{Members: f.members}, nil
}

func (f *fakeGroupService) Timeline(
	_ context.Context, _ db.TenantScope, id uuid.UUID, _ db.TimeWindow, _ db.Keyset,
) (alertsvc.TimelineResult, error) {
	if err := f.own(id); err != nil {
		return alertsvc.TimelineResult{}, err
	}
	return alertsvc.TimelineResult{Events: f.events}, nil
}

func (f *fakeGroupService) Acknowledge(
	_ context.Context, _ db.TenantScope, id uuid.UUID, _, _, _, _ string,
) (gsvc.FanOutResult, error) {
	if err := f.own(id); err != nil {
		return gsvc.FanOutResult{}, err
	}
	return gsvc.FanOutResult{Members: len(f.members), Applied: f.ackApplied}, nil
}

func (f *fakeGroupService) Comment(
	_ context.Context, _ db.TenantScope, id uuid.UUID, _, _, _, _ string,
) (gsvc.CommentResult, error) {
	if err := f.own(id); err != nil {
		return gsvc.CommentResult{}, err
	}
	return gsvc.CommentResult{
		FanOut: gsvc.FanOutResult{Members: len(f.members), Applied: len(f.members)},
		Event:  f.comment,
	}, nil
}

func (f *fakeGroupService) Snooze(
	_ context.Context, _ db.TenantScope, id uuid.UUID, _, _, _ string, _ time.Time, _ string,
) (gsvc.FanOutResult, error) {
	if err := f.own(id); err != nil {
		return gsvc.FanOutResult{}, err
	}
	return gsvc.FanOutResult{Members: f.snoozeMembers, Applied: f.snoozeMembers}, nil
}

func (f *fakeGroupService) Unsnooze(
	_ context.Context, _ db.TenantScope, id uuid.UUID, _, _, _, _ string,
) (gsvc.FanOutResult, error) {
	if err := f.own(id); err != nil {
		return gsvc.FanOutResult{}, err
	}
	return gsvc.FanOutResult{Members: f.snoozeMembers, Applied: f.snoozeMembers}, nil
}

// fakeAlertReader is the cross-domain port that turns a member id into an Alert.
type fakeAlertReader struct {
	alerts map[uuid.UUID]alertsvc.AlertDetail
}

func (f *fakeAlertReader) Get(
	_ context.Context, _ db.TenantScope, alertID uuid.UUID,
) (alertsvc.AlertDetail, error) {
	d, ok := f.alerts[alertID]
	if !ok {
		return alertsvc.AlertDetail{}, fxNotFound()
	}
	return d, nil
}

// fakeRollups answers "did this generation's notifications land". A non-zero
// `dead` is carried deliberately: it is a product signal, not a footnote, and a
// roll-up of zeroes would never prove the field travels.
type fakeRollups struct{ rollup DeliveryRollup }

func (f fakeRollups) DeliveryRollupForGroup(
	context.Context, db.TenantScope, uuid.UUID,
) (DeliveryRollup, error) {
	return f.rollup, nil
}

/* -------------------------------------------------------------------------- */
/* Harness                                                                    */
/* -------------------------------------------------------------------------- */

// newGroupRouter wires a fully-populated router: one group the caller owns, two
// member alerts (one of them snoozed), one timeline event and a delivery
// roll-up that is not all zeroes.
func newGroupRouter(t *testing.T) (*Router, *fakeGroupService) {
	t.Helper()

	group := fxGroup(t)
	alertA := fxAlert(t, fxAlertID, "ak_9d3k1m7q4v0b2n8s5t6u1c3e7g",
		"3f8c1a2b9d4e5f60", "HighErrorRate", "critical")
	alertB := fxAlert(t, fxAlertBID, "ak_1a2b3c4d5e6f7g8h9i0j1k2l3m",
		"a1b2c3d4e5f60718", "DiskFillingUp", "warning")

	members := []domain.Member{
		fxMember(t, fxAlertID, fxOccID, fxNow.Add(-90*time.Minute)),
		fxMember(t, fxAlertBID, fxOccBID, fxNow.Add(-45*time.Minute)),
	}
	lastSent := fxNow.Add(-3 * time.Minute)

	svc := &fakeGroupService{
		owned: fxGroupID,
		detail: gsvc.Detail{
			Group:       group,
			Members:     members,
			StormActive: true,
			Snooze:      domain.SnoozeRollup{Count: 1, Until: fxNow.Add(4 * time.Hour)},
		},
		page:    []domain.Group{group},
		snoozes: map[uuid.UUID]domain.SnoozeRollup{fxGroupID: {Count: 1, Until: fxNow.Add(4 * time.Hour)}},
		members: members,
		events: []alertdomain.Event{
			fxEvent(t, fxEventID, alertdomain.EventOccurrenceAcknowledged, "Acknowledged by Ada Lovelace"),
		},
		comment:       fxEvent(t, fxCommentID, alertdomain.EventCommentAdded, "Tracking on the provider status page"),
		ackApplied:    3,
		snoozeMembers: 2,
	}

	reader := &fakeAlertReader{alerts: map[uuid.UUID]alertsvc.AlertDetail{
		fxAlertID: {
			Alert:      alertA,
			Snooze:     fxSnooze(t, fxAlertID, "ak_9d3k1m7q4v0b2n8s5t6u1c3e7g"),
			SnoozedNow: true,
		},
		// Deliberately awake: `snooze: null` and a snooze object are different
		// answers, and both must survive the schema.
		fxAlertBID: {Alert: alertB},
	}}

	rollups := fakeRollups{rollup: DeliveryRollup{
		Total: 9, Sent: 6, Failed: 1, Dead: 1, Skipped: 2, Pending: 1,
		LastErrorClass: "permanent", LastSentAt: &lastSent,
	}}

	// ⛔ The SYSTEM clock, deliberately. `meta.elapsed_ms` is `minimum: 0` in the
	// contract and is computed as `time.Since(started)` where `started` is this
	// clock's reading — so a fake pinned to the fixtures' instant would emit an
	// elapsed time measured in months, or a negative one the day the fixture date
	// passes. The fixtures carry their own instants; the request does not need to.
	return NewRouter(svc, reader, rollups, clock.New()), svc
}

// mustJSON renders a request fixture so it can be checked against the contract's
// own request schema before it is sent. A handler test that "passes" with a body
// no client would be allowed to send is testing a request that cannot happen.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal a request fixture: %v", err)
	}
	return raw
}

func groupPath(suffix string) string { return "/alert-groups/" + fxGroupID.String() + suffix }

/* -------------------------------------------------------------------------- */
/* Happy paths: every response is the shape the contract declares              */
/* -------------------------------------------------------------------------- */

// TestTheGroupListIsTheShapeTheContractDeclares.
//
// The group list is the default UI landing view, so a drift here is the first
// thing a user sees and the last thing anybody notices. The assertion is against
// the contract itself: `GroupListResponse` sets `additionalProperties: false`
// and requires `data`, `page` and `meta`, and `GroupDTO` requires twenty
// members including the `snoozed_count` that makes the snooze fan-out visible.
func TestTheGroupListIsTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	rt, _ := newGroupRouter(t)
	resp := apitest.New(rt).GET("/alert-groups").MustStatus(t, http.StatusOK)

	schema.Assert(t, "listAlertGroups", http.StatusOK, resp.Body())
}

// TestTheGroupDetailIsTheShapeTheContractDeclares.
//
// `delivery_summary` is REQUIRED on this response and was declared on four
// schemas and emitted by none of them for as long as it was optional — which
// made oto's silence indistinguishable from "no alert", the exact failure the
// field exists to prevent, while every schema validator still passed. It is
// required now, so this assertion is what keeps it emitted.
func TestTheGroupDetailIsTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	rt, _ := newGroupRouter(t)
	resp := apitest.New(rt).GET(groupPath("")).MustStatus(t, http.StatusOK)

	schema.Assert(t, "getAlertGroup", http.StatusOK, resp.Body())
}

// TestTheGroupTimelineIsTheShapeTheContractDeclares.
//
// The merged timeline is the signature view, and every row of it is an
// `AlertEventDTO` whose `actor_kind` is a closed enum and whose two clocks are
// both `format: date-time`. An event rendered with a unix integer or an actor
// kind the contract has never heard of is precisely the drift this catches.
func TestTheGroupTimelineIsTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	rt, _ := newGroupRouter(t)
	resp := apitest.New(rt).GET(groupPath("/timeline")).MustStatus(t, http.StatusOK)

	schema.Assert(t, "getAlertGroupTimeline", http.StatusOK, resp.Body())
}

// TestAckingAGroupAnswersWithTheUpdatedGroup.
//
// The ack is a FAN-OUT of the same receipt over every open member, and the
// contract answers it with the whole updated generation rather than a bare 204 —
// so the button that acknowledges forty alerts can repaint the card it was
// pressed on without a second request. That body is a `GroupDetailResponse` and
// has to satisfy the same schema the read does.
func TestAckingAGroupAnswersWithTheUpdatedGroup(t *testing.T) {
	t.Parallel()

	rt, _ := newGroupRouter(t)
	body := map[string]any{"note": "Known deploy, rolling back"}
	schema.AssertRequest(t, "ackAlertGroup", mustJSON(t, body))

	resp := apitest.New(rt).POST(t, groupPath("/ack"), body).MustStatus(t, http.StatusOK)
	schema.Assert(t, "ackAlertGroup", http.StatusOK, resp.Body())
}

// TestACommentIsAnsweredWithTheEventItWrote.
//
// The 201 body is the event the WRITE returned, never one read back afterwards:
// re-reading the timeline to find "the newest comment.added" can legitimately
// return somebody else's comment appended a millisecond later and hand it back
// as the caller's own. The status is 201 and not 200, and the envelope is the
// single-resource one, both of which the contract fixes.
func TestACommentIsAnsweredWithTheEventItWrote(t *testing.T) {
	t.Parallel()

	rt, _ := newGroupRouter(t)
	body := map[string]any{"body": "Confirmed upstream provider incident."}
	schema.AssertRequest(t, "commentOnAlertGroup", mustJSON(t, body))

	resp := apitest.New(rt).POST(t, groupPath("/comments"), body).MustStatus(t, http.StatusCreated)
	schema.Assert(t, "commentOnAlertGroup", http.StatusCreated, resp.Body())
}

// TestSnoozingAGroupAnswersWithTheUpdatedGroup.
//
// The group snooze is a fan-out of the per-alert primitive and writes nothing
// onto the group, so the ONLY evidence it worked is `snoozed_count` on the body
// it answers with. A response that did not satisfy `GroupDetailResponse` would
// leave the caller unable to see the effect of the button it just pressed.
func TestSnoozingAGroupAnswersWithTheUpdatedGroup(t *testing.T) {
	t.Parallel()

	rt, _ := newGroupRouter(t)
	body := map[string]any{"duration_seconds": 14400, "note": "deploy window"}
	schema.AssertRequest(t, "snoozeAlertGroup", mustJSON(t, body))

	resp := apitest.New(rt).POST(t, groupPath("/snooze"), body).MustStatus(t, http.StatusOK)
	schema.Assert(t, "snoozeAlertGroup", http.StatusOK, resp.Body())
}

// TestUnsnoozingAGroupAnswersWithTheUpdatedGroup. The body is optional on the
// wire, so this also proves the argument-free spelling — the shape the Slack
// button actually sends — reaches the handler at all.
func TestUnsnoozingAGroupAnswersWithTheUpdatedGroup(t *testing.T) {
	t.Parallel()

	rt, _ := newGroupRouter(t)
	resp := apitest.New(rt).POST(t, groupPath("/unsnooze"), nil).MustStatus(t, http.StatusOK)

	schema.Assert(t, "unsnoozeAlertGroup", http.StatusOK, resp.Body())
}

// TestTheMemberListCollapsesAnAlertThatJoinedTwice.
//
// One Alert can hold several episodes in one generation over its lifetime, and
// the LIST is of alerts: a repeat must appear once. The page boundary stays the
// membership row's, not the deduplicated slice's, so a duplicate never silently
// shortens the next page.
//
// It asserts the deduplication by hand — a schema says nothing about how many
// rows there should be — and the envelope against the contract, which is the
// schema half of the same case.
func TestTheMemberListCollapsesAnAlertThatJoinedTwice(t *testing.T) {
	t.Parallel()

	rt, svc := newGroupRouter(t)
	// The same alert joins twice under two occurrences, which is what a re-fire
	// inside one generation looks like.
	svc.members = append(svc.members, fxMember(t, fxAlertID, fxOccBID, fxNow.Add(-10*time.Minute)))

	resp := apitest.New(rt).GET(groupPath("/alerts")).MustStatus(t, http.StatusOK)

	body := resp.JSON(t)
	data, ok := body["data"].([]any)
	if !ok {
		t.Fatalf("data is %T, want an array\n%s", body["data"], resp)
	}
	if len(data) != 2 {
		t.Fatalf("data has %d rows, want 2 — three memberships over two alerts is two rows\n%s",
			len(data), resp)
	}
	if _, ok := body["page"]; !ok {
		t.Fatalf("the member list carries no page object; it is a keyset page\n%s", resp)
	}
	if _, ok := body["meta"]; !ok {
		t.Fatalf("meta is absent; every envelope in this API carries one\n%s", resp)
	}
	schema.Assert(t, "listAlertGroupAlerts", http.StatusOK, resp.Body())
}

/* -------------------------------------------------------------------------- */
/* The tenant boundary                                                        */
/* -------------------------------------------------------------------------- */

// ⭐ TestAnotherOrgsGroupIsIndistinguishableFromOneThatDoesNotExist.
//
// v1 has no roles, so the org boundary is the ONLY boundary there is, and every
// operation that takes an id in its path is a place it can leak. The answer must
// be 404 on all seven:
//
//   - never 200, which would hand another tenant their neighbour's incident;
//   - never 403, which confirms the row exists in somebody's org and turns the
//     endpoint into an existence oracle for a scanner with a list of uuids;
//   - never 500, which is oto admitting it does not know what it just did.
//
// The refusal is a problem document, so the body is asserted against the
// contract's `Problem` schema too: a 404 whose `status` member says 200, or that
// carries no `code`, is a refusal a client cannot act on.
func TestAnotherOrgsGroupIsIndistinguishableFromOneThatDoesNotExist(t *testing.T) {
	t.Parallel()

	stranger := "/alert-groups/" + fxOtherGroup.String()

	cases := []struct {
		op     string
		method string
		target string
		body   any
	}{
		{"getAlertGroup", http.MethodGet, stranger, nil},
		{"listAlertGroupAlerts", http.MethodGet, stranger + "/alerts", nil},
		{"getAlertGroupTimeline", http.MethodGet, stranger + "/timeline", nil},
		{"ackAlertGroup", http.MethodPost, stranger + "/ack", nil},
		{"commentOnAlertGroup", http.MethodPost, stranger + "/comments", map[string]any{"body": "hello"}},
		{"snoozeAlertGroup", http.MethodPost, stranger + "/snooze", map[string]any{"duration_seconds": 3600}},
		{"unsnoozeAlertGroup", http.MethodPost, stranger + "/unsnooze", nil},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			t.Parallel()

			rt, _ := newGroupRouter(t)
			c := apitest.New(rt)

			var resp *apitest.Response
			if tc.method == http.MethodGet {
				resp = c.GET(tc.target)
			} else {
				resp = c.POST(t, tc.target, tc.body)
			}

			if resp.Code() != http.StatusNotFound {
				t.Fatalf("%s answered %d for another org's id, want 404 — "+
					"403 confirms the row exists and 200 hands over the row itself\n%s",
					tc.op, resp.Code(), resp)
			}
			schema.AssertProblem(t, tc.op, http.StatusNotFound, resp.Body())
		})
	}
}

/* -------------------------------------------------------------------------- */
/* The error branches the handlers actually have                              */
/* -------------------------------------------------------------------------- */

// ⛔ TestANonHumanCannotAcknowledgeAGroup.
//
// A group ack is forty receipts, and a receipt nobody signed is not a receipt.
// The refusal is a 403 rather than a 401 — the credential is perfectly valid,
// it is simply not a person — and it happens BEFORE the service is reached, so
// nothing is written.
func TestANonHumanCannotAcknowledgeAGroup(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ op, suffix string }{
		{"ackAlertGroup", "/ack"},
		{"snoozeAlertGroup", "/snooze"},
		{"unsnoozeAlertGroup", "/unsnooze"},
	} {
		t.Run(tc.op, func(t *testing.T) {
			t.Parallel()

			rt, _ := newGroupRouter(t)
			resp := apitest.New(rt).As(apitest.Machine()).
				POST(t, groupPath(tc.suffix), map[string]any{"duration_seconds": 3600})

			if resp.Code() != http.StatusForbidden {
				t.Fatalf("%s answered %d for a machine principal, want 403 — "+
					"a group ack is forty receipts and a machine cannot sign one\n%s",
					tc.op, resp.Code(), resp)
			}
			schema.AssertProblem(t, tc.op, http.StatusForbidden, resp.Body())
		})
	}
}

// TestAckingAGroupWithNothingOpenIsAPrecondition.
//
// Members that have already ended are SKIPPED rather than failing the request —
// refusing the other thirty-nine because one resolved would make the button
// unusable in exactly the storm it exists for. But a group where NOTHING
// accepted the ack is a 412 and not a cheerful 200: "I acknowledged forty
// alerts" and "there was nothing to acknowledge" are different sentences, and a
// 200 would tell the operator the first one.
func TestAckingAGroupWithNothingOpenIsAPrecondition(t *testing.T) {
	t.Parallel()

	rt, svc := newGroupRouter(t)
	svc.ackApplied = 0

	resp := apitest.New(rt).POST(t, groupPath("/ack"), nil).
		MustStatus(t, http.StatusPreconditionFailed)

	schema.AssertProblem(t, "ackAlertGroup", http.StatusPreconditionFailed, resp.Body())
	if code := resp.Problem(t).Code; code != "no_open_occurrence" {
		t.Fatalf("code = %q, want no_open_occurrence — a client has to be able to say WHICH "+
			"nothing-happened this was\n%s", code, resp)
	}
}

// TestSnoozingAGroupWithNoMembersIsAPrecondition. The same rule as the ack: a
// snooze that reached nobody is a fact, not a success.
func TestSnoozingAGroupWithNoMembersIsAPrecondition(t *testing.T) {
	t.Parallel()

	rt, svc := newGroupRouter(t)
	svc.snoozeMembers = 0

	resp := apitest.New(rt).POST(t, groupPath("/snooze"), map[string]any{"duration_seconds": 3600}).
		MustStatus(t, http.StatusPreconditionFailed)

	schema.AssertProblem(t, "snoozeAlertGroup", http.StatusPreconditionFailed, resp.Body())
}

// ⛔ TestASnoozeMustEnd (§B.8.3).
//
// There is no indefinite snooze, so a body giving NEITHER `until` nor
// `duration_seconds` is refused rather than defaulted — a default window would
// be oto inventing how long to stay quiet about somebody else's production.
// The violation names the field, because a form can only act on a field name
// and a machine code.
func TestASnoozeMustEnd(t *testing.T) {
	t.Parallel()

	rt, _ := newGroupRouter(t)
	resp := apitest.New(rt).POST(t, groupPath("/snooze"), map[string]any{"note": "quiet please"}).
		MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "snoozeAlertGroup", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "until")
}

// TestASnoozeCannotBeSpelledBothWays. `until` and `duration_seconds` are two
// spellings of one answer; supplying both leaves the server to pick, and a
// server that picks is a server that will one day pick the other one.
func TestASnoozeCannotBeSpelledBothWays(t *testing.T) {
	t.Parallel()

	rt, _ := newGroupRouter(t)
	resp := apitest.New(rt).POST(t, groupPath("/snooze"), map[string]any{
		"until":            fxNow.Add(2 * time.Hour).Format(time.RFC3339),
		"duration_seconds": 3600,
	}).MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "snoozeAlertGroup", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "until")
}

// TestACommentWithNoBodyIsRefused. `body` is required and must not be blank
// after trimming: an empty `comment.added` event is a permanent, immutable row
// on the timeline that says nothing.
func TestACommentWithNoBodyIsRefused(t *testing.T) {
	t.Parallel()

	rt, _ := newGroupRouter(t)
	resp := apitest.New(rt).POST(t, groupPath("/comments"), map[string]any{"body": "   "}).
		MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "commentOnAlertGroup", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "body")
}

// ⛔ TestATypoedFilterIsRefusedRatherThanIgnored (§E.3).
//
// A silently ignored `?serverity=critical` returns a page of the WRONG groups
// and looks exactly right, which is how the UI and the API stop agreeing with
// each other without anybody noticing. The refusal names the offending
// parameter so the caller can fix its own typo.
func TestATypoedFilterIsRefusedRatherThanIgnored(t *testing.T) {
	t.Parallel()

	rt, _ := newGroupRouter(t)
	resp := apitest.New(rt).GET("/alert-groups?serverity=critical").
		MustStatus(t, http.StatusBadRequest)

	schema.AssertProblem(t, "listAlertGroups", http.StatusBadRequest, resp.Body())
	if code := resp.Problem(t).Code; code != "unknown_parameter" {
		t.Fatalf("code = %q, want unknown_parameter\n%s", code, resp)
	}
	resp.MustViolate(t, "serverity")
}

// TestAnUnknownParameterOnTheMemberListIsRefused keeps the same rule on the
// paginated sub-resource, where the only legal parameters are `limit` and
// `cursor`. `since_seq` in particular was accepted, validated and then never
// pushed down — a resume protocol whose two halves were both missing.
func TestAnUnknownParameterOnTheMemberListIsRefused(t *testing.T) {
	t.Parallel()

	rt, _ := newGroupRouter(t)
	resp := apitest.New(rt).GET(groupPath("/alerts?since_seq=42")).
		MustStatus(t, http.StatusBadRequest)

	schema.AssertProblem(t, "listAlertGroupAlerts", http.StatusBadRequest, resp.Body())
	resp.MustViolate(t, "since_seq")
}

// TestTheTimelineRefusesAnEventTypeThatDoesNotExist.
//
// `AlertEventType` is a closed enum: a filter naming a type that cannot exist
// would return an empty timeline that looks exactly like a quiet one, which is
// the worst possible answer on the view that IS the product.
func TestTheTimelineRefusesAnEventTypeThatDoesNotExist(t *testing.T) {
	t.Parallel()

	rt, _ := newGroupRouter(t)
	resp := apitest.New(rt).GET(groupPath("/timeline?type=comment.added,banana")).
		MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "getAlertGroupTimeline", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "type")
}

// TestAFailedGroupReadIsNotAnEmptyList. oto's silence must stay
// distinguishable from an answer: a list that could not be read must never
// render as "there are no groups".
func TestAFailedGroupReadIsNotAnEmptyList(t *testing.T) {
	t.Parallel()

	rt, svc := newGroupRouter(t)
	svc.listErr = errs.New(errs.KindInternal, "groups_query_failed", "could not read the group list")

	resp := apitest.New(rt).GET("/alert-groups")
	if resp.Code() != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — a failed read must not render as an empty list\n%s",
			resp.Code(), resp)
	}
	schema.AssertProblem(t, "listAlertGroups", http.StatusInternalServerError, resp.Body())
}

// TestMemberAlertRowsCarrySynthetic — the fixed half of what was
// TestBUG_MemberAlertRowsOmitSynthetic.
//
// `listAlertGroupAlerts` answers with an `AlertListResponse`, whose rows are
// `AlertDTO`, which lists `synthetic` among its REQUIRED members. For a while
// `grouping/api.AlertDTO` had no `Synthetic` field at all while
// `alerts/api.AlertDTO` did: the two hand-written copies of one wire schema had
// drifted, which is exactly the failure mode §5.5's three-model rule accepts in
// exchange for layering, and exactly why the contract has to be executable.
//
// The SERVER was wrong. `synthetic` is provenance — an alert oto manufactured
// for a delivery drill — and the group screen is one of the places a synthetic
// alert is legitimately visible; `alertdomain.Alert.Synthetic()` already
// existed and the mapper simply never read it.
func TestMemberAlertRowsCarrySynthetic(t *testing.T) {
	t.Parallel()

	rt, _ := newGroupRouter(t)
	resp := apitest.New(rt).GET(groupPath("/alerts")).MustStatus(t, http.StatusOK)

	schema.Assert(t, "listAlertGroupAlerts", http.StatusOK, resp.Body())
}

/* -------------------------------------------------------------------------- */
/* Live divergences between the handlers and the contract                     */
/* -------------------------------------------------------------------------- */

// ⛔ TestBUG_ADetailReadCanAnswerAnUndeclared400.
//
// WHAT THE SERVER DOES: `getAlertGroup` runs every request through
// `httpx.NewParams(r)` with an EMPTY allow-list (internal/grouping/api/handlers.go:371),
// so any query parameter at all — `?foo=1`, a cache-buster, a tracking
// parameter a proxy appended — is `400 unknown_parameter`. That is §E.3 and it
// is the right behaviour.
//
// WHAT THE CONTRACT SAYS: `getAlertGroup` declares 200, 401, 403, 404, 429, 500
// and 503 and NO 400 (api/openapi/openapi.yaml:1653). A generated client
// therefore has no branch for the status it will receive the first time a proxy
// or a browser adds a parameter to the URL.
//
// WHICH IS WRONG: the CONTRACT. The refusal is correct and is specified in
// §E.3; the response list simply never gained the `BadRequest` entry that
// `listAlertGroupAlerts` and `getAlertGroupTimeline` both have. The same gap
// exists on `getRuleSnapshot` and `getOccurrenceRule`.
//
// The test asserts the CONTRACT — that the 400 this server really emits has a
// schema to be validated against — and is skipped until the entry is added.
func TestBUG_ADetailReadCanAnswerAnUndeclared400(t *testing.T) {
	t.Skip("getAlertGroup emits 400 unknown_parameter, which the contract does not declare; " +
		"the contract is wrong — see the comment above")
	t.Parallel()

	rt, _ := newGroupRouter(t)
	resp := apitest.New(rt).GET(groupPath("?foo=1")).MustStatus(t, http.StatusBadRequest)

	schema.AssertProblem(t, "getAlertGroup", http.StatusBadRequest, resp.Body())
}

// ⛔ TestBUG_TheTimelineTypeFilterCeilingIsSmallerThanTheEnum.
//
// WHAT THE CONTRACT SAYS: the `type` parameter of `getAlertGroupTimeline` is an
// array with `maxItems: 34` (api/openapi/openapi.yaml:1725).
//
// WHAT THE SERVER DOES: `TimelineQuery.Type` is bounded at `max=36`
// (internal/grouping/api/dto.go:257), which is the size of the closed
// `AlertEventType` set — and the contract's own enum has 36 values
// (api/openapi/openapi.yaml:5177–5213).
//
// WHICH IS WRONG: the CONTRACT. `maxItems` was never updated when
// `alert.snoozed` and `alert.unsnoozed` joined the enum, so a caller asking for
// EVERY type it is allowed to see — "show me everything on this group" — writes
// a request its own generated client refuses to send, while the server would
// have answered it. A filter ceiling below the enum it filters is a bound that
// can only ever be wrong.
//
// The test asserts the CONTRACT: 35 types would be refused. It is skipped
// because the server (correctly) accepts them.
func TestBUG_TheTimelineTypeFilterCeilingIsSmallerThanTheEnum(t *testing.T) {
	t.Skip("the contract caps ?type= at 34 items while its own AlertEventType enum has 36; " +
		"the contract is wrong — see the comment above")
	t.Parallel()

	rt, _ := newGroupRouter(t)

	all := alertdomain.AllEventTypes()
	names := make([]string, 0, len(all))
	for _, ty := range all {
		names = append(names, ty.String())
	}

	target := groupPath("/timeline?type=" + joinSorted(names))
	resp := apitest.New(rt).GET(target)
	if resp.Code() != http.StatusUnprocessableEntity {
		t.Fatalf("asking for all %d declared event types answered %d, "+
			"but the contract caps the parameter at 34 items\n%s", len(names), resp.Code(), resp)
	}
}
