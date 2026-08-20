package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// The Notification tag, asserted against the ONE contract.
//
// ⭐ THIS PACKAGE IS WHERE THE CONFORMANCE AUDIT FOUND ITS WORST FINDING, AND IT
// FOUND IT TWICE. `delivery_summary` was declared on the detail response and
// emitted by none of them, which made "oto formed this intent and told nobody"
// indistinguishable from "the server did not compute one" — the exact failure
// that field exists to prevent. And a `NotificationReason` the server emitted was
// missing from the contract's enum, so a conforming client crashed on ordinary
// data. Both are fixed. Neither was ever guarded.
//
// Nothing below restates an expected shape by hand. `schema.Assert` compiles the
// schema `api/openapi/openapi.yaml` declares for the operation and the status and
// validates the bytes the handler wrote — including `format:` and
// `additionalProperties: false` — so a re-introduction fails here, in the package
// that owns it, with the JSON pointer of the offending member.

/* -------------------------------------------------------------------------- */
/* Fixtures                                                                   */
/* -------------------------------------------------------------------------- */

// notifNow pins every timestamp, so a failure message names an instant a reader
// can find rather than whatever the wall clock said.
var notifNow = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

// The ids this file addresses. They are CONSTANTS: a tenant probe whose ids
// change per run cannot be reproduced from a failure message.
var (
	notifMine    = uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317490101")
	policyMine   = uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317490201")
	deliveryMine = uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317490301")
	notifChannel = uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317490401")
	notifAlert   = uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317490601")
	// notifCase is THE CONVERSATION. Since git-bug `7570090` a conversation holds
	// exactly one Case, so the subject id, the conversation id and `case_id` are all
	// this one value — asserting them as three separate ids would pin a distinction
	// the schema no longer has.
	notifCase = uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317490701")
)

// renderedHash is a real SHA-256 spelling, because the contract pins
// `rendered_hash` to `^[0-9a-f]{64}$` and a placeholder would pass a hand-written
// assertion and fail the real one.
const renderedHash = "3b1f8c2e9a7d4f6b0c5e8a1d2f4b6c8e0a3d5f7b9c1e3a5d7f9b1c3e5a7d9f1b"

func notificationFixture(id, org uuid.UUID) domain.Notification {
	alert := notifAlert
	ac := notifCase
	return domain.Notification{
		ID:          id,
		OrgID:       org,
		SubjectKind: domain.SubjectCase,
		SubjectID:   notifCase,
		// The delivery target is the PAIR, and for a `fired` it names the same Case the
		// subject does — `notifications_subject_ck` requires subject_id = case_id, and
		// `notifications_convkind_ck` admits only `case` and `digest`.
		ConversationKind: domain.ConversationCase,
		ConversationID:   notifCase,
		AlertID:          &alert,
		CaseID:           &ac,
		Reason:           domain.ReasonFired,
		StateVersion:     3,
		Status:           domain.StatusDelivered,
		CreatedAt:        notifNow.Add(-time.Hour),
		UpdatedAt:        notifNow.Add(-time.Minute),
	}
}

func deliveryFixture(id, org, notification uuid.UUID) domain.Delivery {
	sent := notifNow.Add(-time.Minute)
	return domain.Delivery{
		ID:                     id,
		OrgID:                  org,
		NotificationID:         notification,
		ChannelID:              notifChannel,
		ThreadSeq:              1,
		Mode:                   domain.ModePostRoot,
		Status:                 domain.DeliverySent,
		Attempts:               1,
		Rendered:               json.RawMessage(`{"text":"[FIRING] HighErrorRate"}`),
		RenderedHash:           renderedHash,
		RenderedFallback:       "[FIRING] HighErrorRate in prod-eu/payments — 4 instances, unacknowledged.",
		ProviderMessageID:      "1723023262.114300",
		ProviderConversationID: "C7F2X9QLM",
		ProviderResponse:       json.RawMessage(`{"ok":true}`),
		SentAt:                 &sent,
		CreatedAt:              notifNow.Add(-2 * time.Minute),
		UpdatedAt:              sent,
	}
}

func policyFixture(id, org uuid.UUID) domain.Policy {
	return domain.Policy{
		ID:       id,
		OrgID:    org,
		Name:     "critical → #sre-alerts",
		Priority: 100,
		Enabled:  true,
		Matchers: []domain.Matcher{{Name: "severity", Op: domain.OpEqual, Value: "critical"}},
		Reasons: []domain.Reason{
			domain.ReasonFired, domain.ReasonAllResolved, domain.ReasonAcked,
		},
		ChannelIDs: []uuid.UUID{notifChannel},
		Throttle:   domain.Throttle{Max: 5, Window: time.Hour},
		CreatedAt:  notifNow.Add(-30 * 24 * time.Hour),
		UpdatedAt:  notifNow.Add(-time.Hour),
	}
}

/* -------------------------------------------------------------------------- */
/* Fakes                                                                      */
/* -------------------------------------------------------------------------- */

// notifPolicies is a PolicyStore that answers NOT FOUND for any row whose OrgID
// is not the caller's — the honest shape of `WHERE org_id = $1 AND id = $2`.
type notifPolicies struct {
	byID    map[uuid.UUID]domain.Policy
	page    []domain.Policy
	created []domain.PolicyDraft
	patched []domain.PolicyPatch
	deleted []uuid.UUID
}

func (f *notifPolicies) ListPolicies(
	_ context.Context, s db.TenantScope, _ db.Keyset,
) ([]domain.Policy, db.Cursor, error) {
	out := make([]domain.Policy, 0, len(f.page))
	for _, p := range f.page {
		if p.OrgID == s.OrgID() {
			out = append(out, p)
		}
	}
	return out, db.Cursor{}, nil
}

func (f *notifPolicies) GetPolicy(
	_ context.Context, s db.TenantScope, id uuid.UUID,
) (domain.Policy, error) {
	p, ok := f.byID[id]
	if !ok || p.OrgID != s.OrgID() {
		return domain.Policy{}, errs.NotFound("policy_not_found", "no such notification policy")
	}
	return p, nil
}

// CreatePolicy carries the claim now: `createNotificationPolicy` goes through
// the writer so the `Idempotency-Key` joins the insert's transaction. The fake
// ignores the key — what it is here to answer is the wire shape — and the
// claim's own behaviour is pinned where the writer lives.
func (f *notifPolicies) CreatePolicy(
	_ context.Context, s db.TenantScope, in domain.PolicyDraft, _ service.Idempotency,
) (domain.Policy, error) {
	f.created = append(f.created, in)
	p := materialise(s, in)
	p.ID = uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317490999")
	p.CreatedAt, p.UpdatedAt = notifNow, notifNow
	return p, nil
}

func (f *notifPolicies) UpdatePolicy(
	ctx context.Context, s db.TenantScope, id uuid.UUID, patch domain.PolicyPatch,
) (domain.Policy, error) {
	p, err := f.GetPolicy(ctx, s, id)
	if err != nil {
		return domain.Policy{}, err
	}
	f.patched = append(f.patched, patch)
	if patch.Name != nil {
		p.Name = *patch.Name
	}
	p.UpdatedAt = notifNow
	return p, nil
}

func (f *notifPolicies) SoftDeletePolicy(ctx context.Context, s db.TenantScope, id uuid.UUID) error {
	if _, err := f.GetPolicy(ctx, s, id); err != nil {
		return err
	}
	f.deleted = append(f.deleted, id)
	return nil
}

// notifReader is the NotificationReader, scoped the same way.
type notifReader struct {
	byID map[uuid.UUID]domain.Notification
}

func (f *notifReader) Get(
	_ context.Context, s db.TenantScope, id uuid.UUID,
) (domain.Notification, error) {
	n, ok := f.byID[id]
	if !ok || n.OrgID != s.OrgID() {
		return domain.Notification{}, errs.NotFound("notification_not_found", "no such notification")
	}
	return n, nil
}

// deliveryStore is shared by the DeliveryReader and the AuditStore, because in
// production they read the same table and a retry that could disagree with a
// detail read would be a fake proving nothing.
type deliveryStore struct{ byID map[uuid.UUID]domain.Delivery }

func (f *deliveryStore) Get(
	_ context.Context, s db.TenantScope, id uuid.UUID,
) (domain.Delivery, error) {
	d, ok := f.byID[id]
	if !ok || d.OrgID != s.OrgID() {
		return domain.Delivery{}, errs.NotFound("delivery_not_found", "no such delivery")
	}
	return d, nil
}

// notifAudit serves the two audit lists, the fan-out read and the retry.
type notifAudit struct {
	deliveries *deliveryStore

	notifications []domain.Notification
	page          []domain.Delivery
	// fanOut is what DeliveriesFor answers. An EMPTY fan-out is the interesting
	// case: it is what a suppressed intent looks like.
	fanOut   []domain.Delivery
	contexts map[uuid.UUID]domain.DeliveryContext
	// requeued counts successful RequeueDead transitions.
	requeued int
}

func (f *notifAudit) ListNotifications(
	_ context.Context, s db.TenantScope, _ domain.NotificationFilter, _ db.Keyset,
) ([]domain.Notification, db.Cursor, error) {
	out := make([]domain.Notification, 0, len(f.notifications))
	for _, n := range f.notifications {
		if n.OrgID == s.OrgID() {
			out = append(out, n)
		}
	}
	return out, db.Cursor{}, nil
}

func (f *notifAudit) ListDeliveries(
	_ context.Context, s db.TenantScope, _ domain.DeliveryFilter, _ db.Keyset,
) ([]domain.Delivery, map[uuid.UUID]domain.DeliveryContext, db.Cursor, error) {
	out := make([]domain.Delivery, 0, len(f.page))
	ctxs := map[uuid.UUID]domain.DeliveryContext{}
	for _, d := range f.page {
		if d.OrgID != s.OrgID() {
			continue
		}
		out = append(out, d)
		// The list's context map is keyed by DELIVERY id, which is what the handler
		// indexes it by.
		ctxs[d.ID] = domain.DeliveryContext{ChannelName: "#sre-alerts", ChannelType: domain.ChannelTypeSlack}
	}
	return out, ctxs, db.Cursor{}, nil
}

func (f *notifAudit) DeliveriesFor(
	context.Context, db.TenantScope, uuid.UUID,
) ([]domain.Delivery, error) {
	return f.fanOut, nil
}

func (f *notifAudit) ChannelContextFor(
	_ context.Context, _ db.TenantScope, ids []uuid.UUID,
) (map[uuid.UUID]domain.DeliveryContext, error) {
	out := map[uuid.UUID]domain.DeliveryContext{}
	for _, id := range ids {
		if c, ok := f.contexts[id]; ok {
			out[id] = c
		}
	}
	return out, nil
}

// RequeueDead is the three-way answer the handler branches on: an error for an id
// the caller cannot have, `false` for a row that was not dead, and the moved row
// for a genuine retry.
func (f *notifAudit) RequeueDead(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
) (domain.Delivery, bool, error) {
	d, err := f.deliveries.Get(ctx, s, id)
	if err != nil {
		return domain.Delivery{}, false, err
	}
	if d.Status != domain.DeliveryDead {
		return domain.Delivery{}, false, nil
	}
	f.requeued++
	d.Status = domain.DeliveryPending
	d.UpdatedAt = notifNow
	return d, true, nil
}

// notifPreviewer is the Previewer. It records the request so a test can prove the
// dry run used the labels the view supplied rather than an empty set.
type notifPreviewer struct {
	result service.Preview
	err    error
	seen   []service.PreviewRequest
}

func (f *notifPreviewer) Preview(
	_ context.Context, _ db.TenantScope, req service.PreviewRequest,
) (service.Preview, error) {
	f.seen = append(f.seen, req)
	if f.err != nil {
		return service.Preview{}, f.err
	}
	return f.result, nil
}

// notifViews is the ViewBuilder. It supplies the group labels the matcher runs
// against; the handler reads them and the renderer would read the same object,
// which is what stops a preview showing a card built from other labels.
type notifViews struct {
	view *service.NotificationView
	err  error
}

func (f *notifViews) Build(
	context.Context, db.TenantScope, service.ViewRequest,
) (*service.NotificationView, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.view, nil
}

// ⛔ `notifSubjects` WAS HERE AND IS DELETED (git-bug `7570090`). It faked
// `SubjectResolver.GroupIDForAlert` / `GroupIDForCase` — "which `alert_groups`
// generation would this preview land in" — and both methods are gone with the
// entity. The preview now takes `case_id`, which IS the conversation, so there is
// nothing left to resolve and no seam left to fake.

// notifQueue is the Requeuer. The row moving back to `pending` is the durable
// fact and the job insert is what makes it prompt, so the count matters.
type notifQueue struct {
	enqueued int
	err      error
}

func (f *notifQueue) Enqueue(
	context.Context, db.JobArgs, ...db.JobOption,
) (db.EnqueueResult, error) {
	if f.err != nil {
		return db.EnqueueResult{}, f.err
	}
	f.enqueued++
	return db.EnqueueResult{ID: 1, Kind: "deliver_dispatch", Queue: "deliver_slack"}, nil
}

/* -------------------------------------------------------------------------- */
/* Harness                                                                    */
/* -------------------------------------------------------------------------- */

// notifWorld is one wired router plus the fakes behind it.
type notifWorld struct {
	policies   *notifPolicies
	audit      *notifAudit
	reader     *notifReader
	deliveries *deliveryStore
	preview    *notifPreviewer
	views      *notifViews
	queue      *notifQueue
	client     *apitest.Client
}

// newNotifWorld wires the Notification router with one row of each kind owned by
// apitest.OrgID and one owned by apitest.OtherOrgID.
//
// The stranger rows EXIST. A probe against an absent id proves nothing about
// tenancy: "no such row" and "not yours" would be the same answer for the wrong
// reason, and only one of them survives a handler that forgets its scope.
//
// `Renderers` is deliberately nil. The port is documented as OPTIONAL — without
// it the preview still answers who is told, where, and what would suppress it,
// and simply omits the payload, which the contract types as nullable. That is the
// branch a deployment without a renderer actually takes.
func newNotifWorld(t *testing.T) *notifWorld {
	t.Helper()

	mineN := notificationFixture(notifMine, apitest.OrgID)
	strangerN := notificationFixture(apitest.StrangerID, apitest.OtherOrgID)
	mineD := deliveryFixture(deliveryMine, apitest.OrgID, notifMine)
	strangerD := deliveryFixture(apitest.StrangerID, apitest.OtherOrgID, apitest.StrangerID)
	mineP := policyFixture(policyMine, apitest.OrgID)
	strangerP := policyFixture(apitest.StrangerID, apitest.OtherOrgID)

	deliveries := &deliveryStore{byID: map[uuid.UUID]domain.Delivery{
		deliveryMine:       mineD,
		apitest.StrangerID: strangerD,
	}}

	w := &notifWorld{
		policies: &notifPolicies{
			byID: map[uuid.UUID]domain.Policy{policyMine: mineP, apitest.StrangerID: strangerP},
			page: []domain.Policy{mineP, strangerP},
		},
		deliveries: deliveries,
		reader: &notifReader{byID: map[uuid.UUID]domain.Notification{
			notifMine:          mineN,
			apitest.StrangerID: strangerN,
		}},
		audit: &notifAudit{
			deliveries:    deliveries,
			notifications: []domain.Notification{mineN, strangerN},
			page:          []domain.Delivery{mineD, strangerD},
			fanOut:        []domain.Delivery{mineD},
			contexts: map[uuid.UUID]domain.DeliveryContext{
				notifChannel: {ChannelName: "#sre-alerts", ChannelType: domain.ChannelTypeSlack},
			},
		},
		preview: &notifPreviewer{result: service.Preview{
			Matched: &mineP,
			Outcomes: []service.PreviewOutcome{
				{PolicyID: policyMine, PolicyName: mineP.Name, Priority: 100, Matched: true, Verdict: "matched"},
			},
			Destinations: []service.PreviewDestination{{
				ChannelID: notifChannel, ChannelName: "#sre-alerts",
				ChannelType: domain.ChannelTypeSlack, Live: true,
				Modes: []domain.Mode{domain.ModePostRoot},
			}},
		}},
		views: &notifViews{view: &service.NotificationView{
			Reason: string(domain.ReasonFired),
			Group: service.GroupView{
				ID: notifCase.String(), Title: "HighErrorRate",
				GroupLabels: map[string]string{"severity": "critical"},
			},
		}},
		queue: &notifQueue{},
	}

	rt := NewRouter(Options{
		Policies:      w.policies,
		PolicyWrites:  w.policies,
		Audit:         w.audit,
		Notifications: w.reader,
		Deliveries:    w.deliveries,
		Preview:       w.preview,
		Views:         w.views,
		Enqueuer:      w.queue,
		Clock:         clock.NewFake(notifNow),
		BaseURL:       "https://oto.example.com",
	})
	w.client = apitest.New(rt)
	return w
}

/* -------------------------------------------------------------------------- */
/* Happy paths — one per operation, asserted against the contract             */
/* -------------------------------------------------------------------------- */

// TestListingPoliciesAnswersAPageInTheShapeTheContractDeclares.
func TestListingPoliciesAnswersAPageInTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	resp := w.client.GET("/notification-policies").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listNotificationPolicies", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("the page carries %d policies, want 1 — the other belongs to %s", len(data), apitest.OtherOrgID)
	}
}

// TestCreatingAPolicyReturnsTheStoredRoutingRule.
//
// The fixture is validated against the contract's own request schema first, so
// this test cannot pass with a body no client would be allowed to send.
func TestCreatingAPolicyReturnsTheStoredRoutingRule(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	body := map[string]any{
		"name":     "critical → #sre-alerts",
		"priority": 50,
		"matchers": []map[string]any{{"name": "severity", "op": "=", "value": "critical"}},
		"reasons":  []string{"fired", "all_resolved"},
		"channel_ids": []string{
			notifChannel.String(),
		},
		"throttle": map[string]any{"max": 5, "window_seconds": 3600},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	schema.AssertRequest(t, "createNotificationPolicy", raw)

	resp := w.client.POST(t, "/notification-policies", body).MustStatus(t, http.StatusCreated)
	schema.Assert(t, "createNotificationPolicy", http.StatusCreated, resp.Body())

	if len(w.policies.created) != 1 {
		t.Fatalf("CreatePolicy ran %d times, want 1", len(w.policies.created))
	}
	// ⛔ ROUTING IS SIGNAL → DESTINATION. The response must never name a person:
	// a policy that routes to a PERSON is a rota, and a rota is the thing oto is
	// not (SCOPE-BOUNDARY §5.3).
	for _, banned := range []string{"user_id", "team_id", "schedule_id", "rotation", "time_of_day"} {
		if strings.Contains(string(resp.Body()), banned) {
			t.Fatalf("⛔ the policy response carries %q; routing ends at a CHANNEL, never at a person", banned)
		}
	}
}

// ⭐ TestThePreviewSaysWhoWouldBeToldAndWritesNothing.
//
// The endpoint exists to stop a policy change becoming an outage, which only
// works if an operator can run it against production as often as they like. It
// writes no row and enqueues no job, and the fakes here would notice if it did.
func TestThePreviewSaysWhoWouldBeToldAndWritesNothing(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	body := map[string]any{"case_id": notifCase.String(), "reason": "fired"}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	schema.AssertRequest(t, "previewNotificationPolicy", raw)

	resp := w.client.POST(t, "/notification-policies/preview", body).MustStatus(t, http.StatusOK)
	schema.Assert(t, "previewNotificationPolicy", http.StatusOK, resp.Body())

	if len(w.policies.created) != 0 || len(w.policies.patched) != 0 || w.queue.enqueued != 0 {
		t.Fatal("⛔ the dry run wrote something; a preview that has effects is not a preview")
	}
	if len(w.preview.seen) != 1 {
		t.Fatalf("the matcher ran %d times, want 1", len(w.preview.seen))
	}
	// ONE read serves both halves: the labels the matcher runs against come from
	// the same view the renderer would draw the card from.
	if got := w.preview.seen[0].Labels["severity"]; got != "critical" {
		t.Fatalf("the matcher ran against labels %v; the view's group labels never reached it",
			w.preview.seen[0].Labels)
	}

	data, _ := resp.JSON(t)["data"].(map[string]any)
	if data["matched"] != true {
		t.Fatalf("matched = %v, want true", data["matched"])
	}
	// The honest caveat is not optional: a preview that quietly ignored the
	// time-dependent dampers would read as a guarantee, and it is not one.
	warnings, _ := data["warnings"].([]any)
	if len(warnings) == 0 {
		t.Fatal("the preview carries no warnings; the routing-only caveat is part of the answer")
	}
}

// TestUpdatingAPolicyReturnsTheMergedRule.
func TestUpdatingAPolicyReturnsTheMergedRule(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	body := map[string]any{"name": "critical → #sre-alerts (v2)", "enabled": false}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	schema.AssertRequest(t, "updateNotificationPolicy", raw)

	resp := w.client.PATCH(t, "/notification-policies/"+policyMine.String(), body).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "updateNotificationPolicy", http.StatusOK, resp.Body())
}

// TestClearingAThrottleIsDifferentFromLeavingItAlone.
//
// `throttle: null` REMOVES the damper and an omitted `throttle` leaves it. A
// plain pointer cannot express both, which is why NullableThrottle exists — and a
// PATCH that could not tell them apart would silently un-throttle a policy that
// only meant to be renamed.
func TestClearingAThrottleIsDifferentFromLeavingItAlone(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	resp := w.client.PATCH(t, "/notification-policies/"+policyMine.String(), map[string]any{
		"throttle": nil,
	}).MustStatus(t, http.StatusOK)
	schema.Assert(t, "updateNotificationPolicy", http.StatusOK, resp.Body())

	if len(w.policies.patched) != 1 {
		t.Fatalf("UpdatePolicy ran %d times, want 1", len(w.policies.patched))
	}
	p := w.policies.patched[0]
	if p.Throttle == nil || *p.Throttle != nil {
		t.Fatalf("an explicit `throttle: null` reached the store as %#v, want a request to CLEAR it", p.Throttle)
	}

	// And the other half: omitting the fields must leave them untouched.
	w2 := newNotifWorld(t)
	w2.client.PATCH(t, "/notification-policies/"+policyMine.String(), map[string]any{"name": "renamed"}).
		MustStatus(t, http.StatusOK)
	if got := w2.policies.patched[0]; got.Throttle != nil {
		t.Fatalf("a rename asked to change the throttle: %#v", got.Throttle)
	}
}

// TestDeletingAPolicyIsA204WithNothingInIt. It stops future matching; the
// notifications it already caused keep their `policy_id`, so the audit trail of
// WHY something was sent outlives the rule.
func TestDeletingAPolicyIsA204WithNothingInIt(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	resp := w.client.DELETE("/notification-policies/"+policyMine.String()).
		MustStatus(t, http.StatusNoContent)
	schema.AssertNoBody(t, "deleteNotificationPolicy", http.StatusNoContent, resp.Body())

	if resp.Header("Content-Type") != "" {
		t.Fatalf("the 204 advertises Content-Type %q for a body that cannot exist", resp.Header("Content-Type"))
	}
	if len(w.policies.deleted) != 1 {
		t.Fatalf("SoftDeletePolicy ran %d times, want 1", len(w.policies.deleted))
	}
}

// TestListingNotificationsAnswersAPageOfIntents.
//
// The list carries no per-row `delivery_summary` on purpose — computing one would
// be a query per row — and the contract marks the field optional for exactly that
// reason. The DETAIL endpoint is where it is required, and that is asserted below.
func TestListingNotificationsAnswersAPageOfIntents(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	resp := w.client.GET("/notifications").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listNotifications", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("the page carries %d intents, want 1 — the other belongs to %s", len(data), apitest.OtherOrgID)
	}
}

// ⭐⭐ TestASuppressedIntentStillCarriesAnAllZeroDeliverySummary.
//
// THIS IS THE AUDIT'S WORST FINDING, ASSERTED. `delivery_summary` was declared on
// the detail responses and emitted by none of them, because `summarise` returned
// nil for an empty fan-out and the field carried `omitempty`. The field vanished
// for precisely the intents where it matters most: a SUPPRESSED notification has
// no deliveries at all, and "oto formed this intent and told nobody" is the fact
// an operator is on this page to learn. An omitted field made oto's silence
// indistinguishable from a server that never computed one — the exact failure the
// field exists to prevent — and every schema validator still passed, because the
// field was optional.
//
// An all-zero roll-up is an ANSWER, never an omission.
func TestASuppressedIntentStillCarriesAnAllZeroDeliverySummary(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	n := w.reader.byID[notifMine]
	n.Status = domain.StatusSuppressed
	n.SuppressedReason = domain.SuppressedNoPolicy
	w.reader.byID[notifMine] = n
	w.audit.fanOut = nil // nothing was ever sent, which is the point

	resp := w.client.GET("/notifications/"+notifMine.String()).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getNotification", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	summary, ok := data["delivery_summary"].(map[string]any)
	if !ok {
		t.Fatalf("⛔ delivery_summary is absent from a suppressed intent: %s", resp.Body())
	}
	for _, member := range []string{"total", "sent", "failed", "dead", "skipped", "pending"} {
		v, present := summary[member]
		if !present {
			t.Fatalf("⛔ delivery_summary omits the required member %q: %#v", member, summary)
		}
		if v != float64(0) {
			t.Fatalf("delivery_summary.%s = %v, want 0 for an intent with no deliveries", member, v)
		}
	}
	if data["suppressed_reason"] != "no_policy" {
		t.Fatalf("suppressed_reason = %v, want no_policy — silence oto cannot explain destroys trust",
			data["suppressed_reason"])
	}
	if deliveries, ok := data["deliveries"].([]any); !ok || len(deliveries) != 0 {
		t.Fatalf("deliveries is %#v, want [] — an empty fan-out is [], never null", data["deliveries"])
	}
}

// TestASkippedDeliveryCountsAsSentAndIsAlsoBrokenOut.
//
// A `skipped` delivery is a coalesced no-op: the destination already shows exactly
// this content. Reporting it as a failure would make a healthy, quiet thread look
// broken — and hiding it inside `sent` would make "nothing was actually posted"
// unanswerable, which is why the contract requires both members.
func TestASkippedDeliveryCountsAsSentAndIsAlsoBrokenOut(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	skipped := deliveryFixture(uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317490302"),
		apitest.OrgID, notifMine)
	skipped.Status = domain.DeliverySkipped
	w.audit.fanOut = []domain.Delivery{w.audit.fanOut[0], skipped}

	resp := w.client.GET("/notifications/"+notifMine.String()).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getNotification", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	summary, _ := data["delivery_summary"].(map[string]any)
	if summary["sent"] != float64(2) {
		t.Fatalf("sent = %v, want 2 — a skipped delivery is a healthy quiet thread, not a failure",
			summary["sent"])
	}
	if summary["skipped"] != float64(1) {
		t.Fatalf("skipped = %v, want 1 — broken out so \"nothing was posted\" stays answerable",
			summary["skipped"])
	}
}

// TestListingDeliveriesAnswersAPageOfMaterialisations.
func TestListingDeliveriesAnswersAPageOfMaterialisations(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	resp := w.client.GET("/deliveries").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listDeliveries", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("the page carries %d deliveries, want 1", len(data))
	}
	row, _ := data[0].(map[string]any)
	if row["channel_name"] != "#sre-alerts" {
		t.Fatalf("channel_name = %v; a column of raw UUIDs is unreadable", row["channel_name"])
	}
}

// ⛔ TestOneDeliveryCarriesThePayloadThatWasRendered.
//
// When outbound validation rejects a message the delivery goes straight to `dead`
// with `config_invalid` and the offending payload is retrievable HERE. It is never
// truncated to fit and never sent — that would be an oto bug, and oto alerts on
// itself for it.
func TestOneDeliveryCarriesThePayloadThatWasRendered(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	d := w.deliveries.byID[deliveryMine]
	d.Status = domain.DeliveryDead
	d.Error = "the payload had fifty-one blocks"
	d.ErrorClass = domain.ClassConfigInvalid
	w.deliveries.byID[deliveryMine] = d

	resp := w.client.GET("/deliveries/"+deliveryMine.String()).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getDelivery", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	if _, ok := data["rendered"].(map[string]any); !ok {
		t.Fatalf("rendered is %#v; the refused payload must stay retrievable", data["rendered"])
	}
	if data["rendered_hash"] != renderedHash {
		t.Fatalf("rendered_hash = %v, want the stored digest", data["rendered_hash"])
	}
	// ⛔ There is no `permalink`. One was declared and written by nothing: a
	// `format: uri` field that is always null is a deep link that never links.
	if _, present := data["permalink"]; present {
		t.Fatalf("the delivery carries a permalink again: %#v", data["permalink"])
	}
}

// TestRetryingADeadDeliveryMovesItBackAndPutsItOnTheQueue.
//
// The state transition and the re-enqueue are separate on purpose: the row moving
// to `pending` is the durable fact and the job insert is what makes it prompt. If
// the queue were unavailable the delivery would still be pending and the sweep
// would collect it, so the operator's action is never silently lost.
func TestRetryingADeadDeliveryMovesItBackAndPutsItOnTheQueue(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	d := w.deliveries.byID[deliveryMine]
	d.Status = domain.DeliveryDead
	d.ErrorClass = domain.ClassAuthExpired
	d.Error = "the bot token was revoked"
	w.deliveries.byID[deliveryMine] = d

	resp := w.client.POST(t, "/deliveries/"+deliveryMine.String()+"/retry", nil).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "retryDelivery", http.StatusOK, resp.Body())

	if w.audit.requeued != 1 {
		t.Fatalf("RequeueDead ran %d times, want 1", w.audit.requeued)
	}
	if w.queue.enqueued != 1 {
		t.Fatalf("the dispatch job was enqueued %d times, want 1 — an operator who just rotated a "+
			"token is asking for NOW, not for the next sweep", w.queue.enqueued)
	}
	data, _ := resp.JSON(t)["data"].(map[string]any)
	if data["status"] != "pending" {
		t.Fatalf("status = %v, want pending", data["status"])
	}
}

// TestAFailedEnqueueStillReportsTheRetryAsDone.
//
// The delivery is ALREADY pending, so a queue failure is not a failure of the
// operator's request. Reporting it would invite them to retry an action that has
// already taken effect, and the retry sweep collects the row anyway.
func TestAFailedEnqueueStillReportsTheRetryAsDone(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	d := w.deliveries.byID[deliveryMine]
	d.Status = domain.DeliveryDead
	w.deliveries.byID[deliveryMine] = d
	w.queue.err = errs.Unavailable("queue_unavailable", "the queue is unreachable", 0)

	resp := w.client.POST(t, "/deliveries/"+deliveryMine.String()+"/retry", nil).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "retryDelivery", http.StatusOK, resp.Body())

	if w.audit.requeued != 1 {
		t.Fatal("the durable half of the retry did not happen")
	}
}

/* -------------------------------------------------------------------------- */
/* The tenant boundary                                                        */
/* -------------------------------------------------------------------------- */

// ⭐⭐ TestAnotherTenantsNotificationIdIsAlwaysA404.
//
// v1 has no roles, so the tenant boundary is the ONLY boundary there is. Every
// endpoint addressed by `{id}` must answer 404 for an id belonging to somebody
// else: never 403, which confirms the row exists; never 200 with that tenant's
// data; and never a 500, which is a boundary held by accident.
//
// The stranger rows are REAL and owned by apitest.OtherOrgID, so a handler that
// dropped its scope would find them and this probe would see it.
func TestAnotherTenantsNotificationIdIsAlwaysA404(t *testing.T) {
	t.Parallel()

	stranger := apitest.StrangerID.String()
	routes := []apitest.Route{
		{Op: "updateNotificationPolicy", Method: http.MethodPatch,
			Path: "/notification-policies/" + stranger, Body: `{"name":"mine now"}`},
		{Op: "deleteNotificationPolicy", Method: http.MethodDelete,
			Path: "/notification-policies/" + stranger},
		{Op: "getNotification", Method: http.MethodGet, Path: "/notifications/" + stranger},
		{Op: "getDelivery", Method: http.MethodGet, Path: "/deliveries/" + stranger},
		{Op: "retryDelivery", Method: http.MethodPost, Path: "/deliveries/" + stranger + "/retry"},
	}

	apitest.AssertCrossTenant404(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		w := newNotifWorld(t)
		return w.client, func(t *testing.T, _ apitest.Route, resp *apitest.Response) {
			if strings.Contains(string(resp.Body()), "#sre-alerts") {
				t.Fatalf("⛔ the refusal leaked the other tenant's data:\n%s", resp.Body())
			}
			if len(w.policies.deleted) != 0 || len(w.policies.patched) != 0 || w.audit.requeued != 0 {
				t.Fatal("⛔ a cross-tenant request still reached a write")
			}
		}
	}, routes)
}

// ⭐ THE PREVIEW REFUSES A SUBJECT IT COULD NOT READ, and this test replaced one that
// asserted the opposite.
//
// It supersedes `TestPreviewingAgainstAnotherTenantsAlertIsA404`, which probed
// `alert_id` — a field the preview no longer accepts (git-bug `7570090`) — and then
// `TestAPreviewAgainstAnUnreadableSubjectAnswers200`, which pinned the DEFECT that
// briefly shipped in between.
//
// ⛔ THE DEFECT IS WORTH REMEMBERING BECAUSE OF HOW IT ARRIVED. `resolveSubject` used
// to call `SubjectResolver.GroupIDForCase`, a TENANT-SCOPED read whose `case_not_found`
// went straight to the caller. The port was deleted on the argument that the caller
// "now supplies the answer directly" — true of RESOLUTION, false of the tenancy check
// riding along inside it. The preview then answered 200 for another org's Case, with
// the matcher running against no labels, and `openapi.yaml` still declaring a 404 that
// nothing could produce. A port's stated purpose is not necessarily its whole purpose.
//
// This subject arrives in the BODY rather than the path, so it is the one subject
// `apitest.AssertCrossTenant404` cannot reach — which is why it needs its own test.
func TestAPreviewAgainstAnUnreadableSubjectIsA404(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	// The honest shape of "this Case is not yours": the scoped read refuses it. Which
	// id the fake happens to know is not the point — the handler's duty is to surface
	// a subject it could not read, whatever refused it.
	w.views.err = errs.NotFound("case_not_found", "no such case")

	w.client.POST(t, "/notification-policies/preview", map[string]any{
		"case_id": apitest.StrangerID.String(),
	}).MustStatus(t, http.StatusNotFound)

	// ⚠️ NO `schema.Assert` HERE, deliberately: the contract declares this response as
	// `application/problem+json` and the schema asserter only covers declared JSON
	// bodies. The problem shape is pinned centrally for every endpoint; what is
	// specific to this one is the status and the matcher having been skipped.

	// ⛔ AND THE MATCHER MUST NOT HAVE RUN. This is the assertion that makes the fix
	// real rather than a status-code change: a routing verdict computed against a
	// subject that was never read is a confidently wrong answer, and answering 404
	// while still having evaluated policies would leave that half of the defect in
	// place.
	if len(w.preview.seen) != 0 {
		t.Fatalf("the matcher ran %d times against an unreadable subject", len(w.preview.seen))
	}
}

/* -------------------------------------------------------------------------- */
/* Refusals the handlers actually have                                        */
/* -------------------------------------------------------------------------- */

// ⛔ TestRetryingADeliveryThatIsNotDeadIsA412.
//
// Only a `dead` delivery can be retried this way. Pending and failed deliveries
// are already on their own backoff schedule, and nudging one would double-send —
// so the refusal says which state the row is actually in rather than "no".
func TestRetryingADeliveryThatIsNotDeadIsA412(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	resp := w.client.POST(t, "/deliveries/"+deliveryMine.String()+"/retry", nil).
		MustStatus(t, http.StatusPreconditionFailed)

	schema.AssertProblem(t, "retryDelivery", http.StatusPreconditionFailed, resp.Body())

	p := resp.Problem(t)
	if p.Code != "delivery_not_dead" {
		t.Fatalf("code = %q, want delivery_not_dead", p.Code)
	}
	if !strings.Contains(p.Detail, "sent") {
		t.Fatalf("the refusal does not say what state the delivery IS in: %q", p.Detail)
	}
	if w.queue.enqueued != 0 {
		t.Fatal("a refused retry still enqueued a dispatch; the message would have gone twice")
	}
}

// TestAnUnknownReasonFilterIsRefusedRatherThanIgnored.
//
// §E.3: a silently ignored filter returns the wrong page and looks right. The
// reason vocabulary is validated against the DOMAIN's closed set rather than a
// list retyped in the handler, because migration 00018 narrowed it and a
// duplicated list is the second copy that drifts.
func TestAnUnknownReasonFilterIsRefusedRatherThanIgnored(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	// `escalation` is the value migration 00019 removed. A client still sending it
	// must be told, not quietly served a page filtered by nothing.
	// vocab:allow the retired reason value IS the input under test; the assertion is that the server refuses it.
	resp := w.client.GET("/notifications?reason=escalation").
		MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "listNotifications", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "reason")
	_ = w
}

// TestAPolicyNamingAnUnknownReasonNamesTheOffendingIndex.
//
// The violation path is the JSON name with the index — `reasons/0` — because that
// is what the settings form maps onto a control. "reasons is invalid" would leave
// an operator to find which of thirty-two entries is wrong.
func TestAPolicyNamingAnUnknownReasonNamesTheOffendingIndex(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	resp := w.client.POST(t, "/notification-policies", map[string]any{
		"name": "everything → #noise",
		// vocab:allow the retired reason value IS the input under test; the assertion is that the server refuses it.
		"reasons":     []string{"escalation", "fired"},
		"channel_ids": []string{notifChannel.String()},
	}).MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "createNotificationPolicy", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "reasons/0")

	if len(w.policies.created) != 0 {
		t.Fatal("a refused policy was written anyway")
	}
}

// TestAPreviewWithNoSubjectNamesTheFieldToSupply.
//
// ⛔ IT USED TO SAY "exactly one of alert_id / case_id / group_id", AND THE ARITY
// IS GONE WITH THE OTHER TWO FIELDS (git-bug `7570090`): `case_id` is the only
// subject a preview takes, because a Case IS the conversation. The property under
// test is unchanged and is the reason the arity could go — previewing against the
// wrong subject would produce a confidently wrong answer, which is the one thing
// this endpoint must never do — so the refusal must still NAME the field to supply
// rather than being a bare 422.
func TestAPreviewWithNoSubjectNamesTheFieldToSupply(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	resp := w.client.POST(t, "/notification-policies/preview", map[string]any{"reason": "fired"}).
		MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "previewNotificationPolicy", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "case_id")

	if len(w.preview.seen) != 0 {
		t.Fatal("the matcher ran with no subject")
	}
}

// TestAPreviewOfADraftThatBreaksTheDomainIsRefusedBeforeItRuns.
//
// The inline draft is the important case — "who would this reach BEFORE I save
// it?" — and a draft the database would reject must come back as a field
// violation rather than as a preview of a policy that can never exist.
func TestAPreviewOfADraftThatBreaksTheDomainIsRefusedBeforeItRuns(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	resp := w.client.POST(t, "/notification-policies/preview", map[string]any{
		"case_id": notifCase.String(),
		"policy": map[string]any{
			"name":        "bad matcher → #sre",
			"reasons":     []string{"fired"},
			"channel_ids": []string{notifChannel.String()},
			"matchers":    []map[string]any{{"name": "severity", "op": "=~", "value": "critical("}},
		},
	}).MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "previewNotificationPolicy", http.StatusUnprocessableEntity, resp.Body())
	if len(w.preview.seen) != 0 {
		t.Fatal("an invalid draft still reached the matcher")
	}
}

// TestAnUpdateThatAsksForNothingIsRefused, because the contract marks the body
// `minProperties: 1`.
func TestAnUpdateThatAsksForNothingIsRefused(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	resp := w.client.PATCH(t, "/notification-policies/"+policyMine.String(), map[string]any{}).
		MustStatus(t, http.StatusUnprocessableEntity)

	schema.AssertProblem(t, "updateNotificationPolicy", http.StatusUnprocessableEntity, resp.Body())
	if len(resp.Problem(t).Violations) == 0 {
		t.Fatalf("the refusal carries no violations[]: %s", resp.Body())
	}
	if len(w.policies.patched) != 0 {
		t.Fatal("an empty patch reached the store")
	}
}

// TestAnUnknownQueryParameterOnTheDeliveryListIsA400.
//
// §E.3 again, on the list an operator uses to find channels that have stopped
// working: `?statuss=dead` silently ignored returns every delivery and reads as
// "nothing is broken".
func TestAnUnknownQueryParameterOnTheDeliveryListIsA400(t *testing.T) {
	t.Parallel()

	apitest.AssertUnknownQueryParamRefused(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		return newNotifWorld(t).client, nil
	}, []apitest.Route{
		{Op: "listDeliveries", Method: http.MethodGet, Path: "/deliveries?statuss=dead"},
	})
}

// TestANegativeSinceSeqIsRefused. `since_seq` addresses a position in a monotonic
// sequence, and a negative one is a client bug that would otherwise be rounded
// silently into "from the beginning".
func TestANegativeSinceSeqIsRefused(t *testing.T) {
	t.Parallel()

	w := newNotifWorld(t)
	resp := w.client.GET("/deliveries?since_seq=-1").MustStatus(t, http.StatusBadRequest)
	schema.AssertProblem(t, "listDeliveries", http.StatusBadRequest, resp.Body())
	_ = w
}
