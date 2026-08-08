package service_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	chdomain "github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/notification/service"
	"github.com/thulasiram/oto/internal/platform/clock"
)

// renderer is a pure stub whose hash per mode the test controls, so "the card
// renders the bytes it showed before the amend" is expressible.
type renderer struct {
	mu     sync.Mutex
	hashes map[chdomain.Mode]string
	// seen records every RenderOptions the dispatcher asked for, so a test can
	// assert on what the card was told about itself.
	seen []chdomain.RenderOptions
}

func newRenderer() *renderer {
	return &renderer{hashes: map[chdomain.Mode]string{
		chdomain.ModePostRoot:    hex("a"),
		chdomain.ModeUpdateRoot:  hex("b"),
		chdomain.ModeThreadReply: hex("c"),
	}}
}

// hex is a 64-character lowercase hash, which is all deliveries_hash_ck asks.
func hex(seed string) string { return strings.Repeat(seed, 64) }

func (r *renderer) set(mode chdomain.Mode, h string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hashes[mode] = h
}

func (r *renderer) ID() chdomain.RendererID         { return "webhook.json" }
func (*renderer) Supports(chdomain.Capability) bool { return true }

func (r *renderer) Render(
	_ context.Context, _ *chdomain.NotificationView, o chdomain.RenderOptions,
) (chdomain.RenderedMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, o)
	h := r.hashes[o.Mode]
	return chdomain.RenderedMessage{
		Fallback: "an alert is firing",
		Summary:  "an alert is firing",
		Payload:  json.RawMessage(`{"mode":"` + string(o.Mode) + `","hash":"` + h + `"}`),
		Hash:     h,
	}, nil
}

// target records what reached the provider and fails whatever it is told to.
type target struct {
	mu       sync.Mutex
	caps     chdomain.Capability
	amendErr error
	amends   int
	delivers int
	closed   int
}

func (t *target) Capabilities() chdomain.Capability { return t.caps }

func (t *target) Deliver(
	context.Context, chdomain.DeliverRequest,
) (chdomain.DeliverResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.delivers++
	return chdomain.DeliverResult{
		Ref: chdomain.MessageRef{ConversationID: "C123", MessageID: "1700000000.00020" + itoa(t.delivers)},
	}, nil
}

func (t *target) Amend(
	context.Context, chdomain.MessageRef, chdomain.RenderedMessage,
) (chdomain.DeliverResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.amends++
	if t.amendErr != nil {
		return chdomain.DeliverResult{}, t.amendErr
	}
	return chdomain.DeliverResult{}, nil
}

func (t *target) Probe(context.Context) error { return nil }

func (t *target) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed++
	return nil
}

func itoa(n int) string { return string(rune('0' + n%10)) }

// registry hands out the stub renderer and the stub destination.
type registry struct {
	renderer *renderer
	target   *target
}

func (g registry) Renderer(chdomain.Type, chdomain.RendererID) (chdomain.Renderer, error) {
	return g.renderer, nil
}

func (g registry) Open(
	context.Context, chdomain.Type, chdomain.ChannelConfig, chdomain.Credential,
) (chdomain.Channel, error) {
	return g.target, nil
}

// harness is one wired notification + dispatch pair over the real database.
type harness struct {
	fx         fixture
	notifier   *service.NotificationService
	dispatcher *service.DispatchService
	deliveries *repository.DeliveryRepository
	threads    *repository.ThreadRepository
	renderer   *renderer
	target     *target
	jobs       *enqueuer
}

func newHarness(t *testing.T, tgt *target) harness {
	t.Helper()

	fx := newFixture(t, domain.CapThreading|domain.CapAmend)
	clk := clock.New()
	rend := newRenderer()

	channels := repository.NewChannelRepository(testPool)
	deliveries := repository.NewDeliveryRepository(testPool)
	threads := repository.NewThreadRepository(testPool)
	events := repository.NewEventRepository(testPool, clk)
	jobs := &enqueuer{}

	policies, err := service.NewPolicyService(policyStore{policy: domain.Policy{
		ID: fx.policyID, OrgID: fx.orgID, Name: "all", Priority: 1, Enabled: true,
		Reasons:    []domain.Reason{domain.ReasonFired, domain.ReasonNewAlerts},
		ChannelIDs: []uuid.UUID{fx.channel.ID},
	}}, channels)
	require.NoError(t, err)

	notifier, err := service.NewNotificationService(service.NotificationConfig{
		Tx:            txRunner{pool: testPool},
		Policies:      policies,
		Notifications: repository.NewNotificationRepository(testPool),
		Deliveries:    deliveries,
		Threads:       threads,
		Snapshots:     snapshots{fx: fx},
		Events:        events,
		Enqueuer:      jobs,
		Clock:         clk,
	})
	require.NoError(t, err)

	views, err := service.NewViewService(service.ViewConfig{
		Snapshots: snapshots{fx: fx}, BaseURL: "https://oto.example.com", Clock: clk,
	})
	require.NoError(t, err)

	dispatcher, err := service.NewDispatchService(service.DispatchConfig{
		Tx:            txRunner{pool: testPool},
		Notifications: repository.NewNotificationRepository(testPool),
		Deliveries:    deliveries,
		Threads:       threads,
		Channels:      channels,
		Events:        events,
		Views:         views,
		Registry:      registry{renderer: rend, target: tgt},
		Gates: repository.NewOrderingGates(repository.GatesConfig{
			Pool: testPool, Clock: clk,
		}),
		Enqueuer: jobs,
		BaseURL:  "https://oto.example.com",
		Clock:    clk,
	})
	require.NoError(t, err)

	return harness{
		fx: fx, notifier: notifier, dispatcher: dispatcher,
		deliveries: deliveries, threads: threads,
		renderer: rend, target: tgt, jobs: jobs,
	}
}

// rowsFor lists this notification's delivery rows in thread order.
func (h harness) rowsFor(t *testing.T, notificationID uuid.UUID) []domain.Delivery {
	t.Helper()
	rows, err := testPool.Query(t.Context(),
		`SELECT id FROM notification_deliveries
		  WHERE org_id = $1 AND notification_id = $2
		  ORDER BY thread_seq NULLS FIRST`, h.fx.orgID, notificationID)
	require.NoError(t, err)
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())

	out := make([]domain.Delivery, 0, len(ids))
	for _, id := range ids {
		d, err := h.deliveries.Get(t.Context(), h.fx.scope, id)
		require.NoError(t, err)
		out = append(out, d)
	}
	return out
}

func (h harness) evaluate(t *testing.T, reason domain.Reason, version int) uuid.UUID {
	t.Helper()
	res, err := h.notifier.Evaluate(t.Context(), h.fx.scope, service.Intent{
		GroupID: h.fx.groupID, Reason: reason, StateVersion: version,
	})
	require.NoError(t, err)
	require.True(t, res.Created)
	return res.Notification.ID
}

// TestRootAmendIsItsOwnDeliveryRow is the §H.6 pair, and the end of the
// LastRootHash lie.
//
// `deliveries_fanout_uniq` used to be (notification_id, channel_id), so a fact
// that §H.6 says produces `update_root` AND `thread_reply` could only have one
// row. The amend rode along inside the reply's claim and wrote nothing, and the
// consequence that mattered was not the failure case but the SUCCESS case: the
// card changed, no `sent` row recorded it, and `LastRootHash` went on returning
// the PRE-amend bytes. A later genuine `update_root` rendering those bytes was
// then abandoned as `duplicate_render` — leaving the card permanently, silently
// wrong, which is the worst combination this system has.
//
// With `mode` in the key the amend is an ordinary delivery: claimed, sequenced
// ahead of the reply, hashed, and visible to the cache like anything else.
func TestRootAmendIsItsOwnDeliveryRow(t *testing.T) {
	t.Parallel()

	h := newHarness(t, &target{caps: chdomain.Capability(domain.CapThreading | domain.CapAmend)})
	ctx := t.Context()

	// 1. The first fact posts the root.
	firstID := h.evaluate(t, domain.ReasonFired, 1)
	first := h.rowsFor(t, firstID)
	require.Len(t, first, 1)
	require.Equal(t, domain.ModePostRoot, first[0].Mode)
	require.NoError(t, h.dispatcher.Dispatch(ctx, h.fx.scope, first[0].ID))

	th, err := h.threads.Ensure(ctx, h.fx.scope, h.fx.channel.ID,
		domain.SubjectAlertGroup, h.fx.groupID, first[0].CreatedAt)
	require.NoError(t, err)
	require.True(t, th.RootLanded())

	hash, err := h.deliveries.LastRootHash(ctx, h.fx.scope, th.ID)
	require.NoError(t, err)
	require.Equal(t, hex("a"), hash)

	// 2. A fact that touches the root AND says something new: §H.6's two rows.
	secondID := h.evaluate(t, domain.ReasonNewAlerts, 2)
	second := h.rowsFor(t, secondID)
	require.Len(t, second, 2, "update_root and thread_reply each need their own row")
	require.Equal(t, domain.ModeUpdateRoot, second[0].Mode)
	require.Equal(t, domain.ModeThreadReply, second[1].Mode)
	require.Equal(t, second[0].ThreadSeq+1, second[1].ThreadSeq,
		"the amend is sequenced AHEAD of the reply, so the card agrees with it on arrival")

	require.NoError(t, h.dispatcher.Dispatch(ctx, h.fx.scope, second[0].ID))
	require.NoError(t, h.dispatcher.Dispatch(ctx, h.fx.scope, second[1].ID))

	amend, err := h.deliveries.Get(ctx, h.fx.scope, second[0].ID)
	require.NoError(t, err)
	require.Equal(t, domain.DeliverySent, amend.Status, "a successful amend is a SENT delivery")
	require.Equal(t, 1, h.target.amends)
	require.Equal(t, 2, h.target.delivers, "the root post and the reply")

	// THE LIE, GONE: the cache describes the card as it now is.
	hash, err = h.deliveries.LastRootHash(ctx, h.fx.scope, th.ID)
	require.NoError(t, err)
	require.Equal(t, hex("b"), hash,
		"LastRootHash must return the AMENDED bytes, not the bytes from before the amend")

	// 3. The world moves back to what the original card described, so a genuine
	//    later update_root renders the PRE-amend bytes. It must be SENT: the card
	//    currently shows something else. Under the old cache it was abandoned as a
	//    duplicate render and the card stayed wrong forever.
	h.renderer.set(chdomain.ModeUpdateRoot, hex("a"))

	thirdID := h.evaluate(t, domain.ReasonFired, 3)
	third := h.rowsFor(t, thirdID)
	require.Len(t, third, 1)
	require.Equal(t, domain.ModeUpdateRoot, third[0].Mode)
	require.NoError(t, h.dispatcher.Dispatch(ctx, h.fx.scope, third[0].ID))

	final, err := h.deliveries.Get(ctx, h.fx.scope, third[0].ID)
	require.NoError(t, err)
	require.Equal(t, domain.DeliverySent, final.Status,
		"an update_root rendering the pre-amend bytes is a REAL change and must be sent")
	require.NotEqual(t, domain.DeliverySkipped, final.Status)
	require.Equal(t, 2, h.target.amends)
}

// TestRootAmendFailureIsRetryable is the failure half of the same change.
//
// A failed amend used to be a `log.Warn` and nothing else: no row, so no retry,
// no dead-letter, no timeline entry, and a Slack card left stale in silence. With
// its own row it fails like any other delivery — recorded, classified, retryable —
// and the reply behind it is NOT held hostage, because gap recovery advances the
// head past a dead amend.
func TestRootAmendFailureIsRetryable(t *testing.T) {
	t.Parallel()

	tgt := &target{
		caps: chdomain.Capability(domain.CapThreading | domain.CapAmend),
		amendErr: &chdomain.Error{
			Class: chdomain.ClassRetryable, Provider: "webhook", Code: "service_unavailable",
		},
	}
	h := newHarness(t, tgt)
	ctx := t.Context()

	firstID := h.evaluate(t, domain.ReasonFired, 1)
	first := h.rowsFor(t, firstID)
	require.NoError(t, h.dispatcher.Dispatch(ctx, h.fx.scope, first[0].ID))

	secondID := h.evaluate(t, domain.ReasonNewAlerts, 2)
	second := h.rowsFor(t, secondID)
	require.Len(t, second, 2)

	// The retry error is handed back to the queue; that is the job's verdict.
	require.Error(t, h.dispatcher.Dispatch(ctx, h.fx.scope, second[0].ID))

	amend, err := h.deliveries.Get(ctx, h.fx.scope, second[0].ID)
	require.NoError(t, err)
	require.Equal(t, domain.DeliveryFailed, amend.Status,
		"a failed amend must leave a retryable row, not vanish into a log line")
	require.False(t, amend.Status.Resolved())
	require.Equal(t, 1, amend.Attempts)
	require.Equal(t, domain.ClassRetryable, amend.ErrorClass)
	require.NotNil(t, amend.NextAttemptAt)

	// The fact reached the timeline, which is what the UI reads.
	var failures int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM alert_events
		  WHERE org_id = $1 AND type = 'delivery.failed'
		    AND payload->>'delivery_id' = $2`,
		h.fx.orgID, second[0].ID.String()).Scan(&failures))
	require.Equal(t, 1, failures)

	// The reply behind it has not been sent out of order.
	reply, err := h.deliveries.Get(ctx, h.fx.scope, second[1].ID)
	require.NoError(t, err)
	require.Equal(t, domain.DeliveryPending, reply.Status)
}
