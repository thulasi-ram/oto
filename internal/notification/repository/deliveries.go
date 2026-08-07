package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// deliveryRow is the row model of `notification_deliveries`.
type deliveryRow struct {
	id                     uuid.UUID
	orgID                  uuid.UUID
	notificationID         uuid.UUID
	channelID              uuid.UUID
	threadID               *uuid.UUID
	threadSeq              *int
	mode                   string
	status                 string
	attempts               int
	nextAttemptAt          *time.Time
	rendered               []byte
	renderedHash           *string
	renderedFallback       *string
	providerMessageID      *string
	providerConversationID *string
	providerResponse       []byte
	errText                *string
	errClass               *string
	ambiguous              bool
	sentAt                 *time.Time
	createdAt              time.Time
	updatedAt              time.Time
}

func (r deliveryRow) toDomain() domain.Delivery {
	d := domain.Delivery{
		ID:               r.id,
		OrgID:            r.orgID,
		NotificationID:   r.notificationID,
		ChannelID:        r.channelID,
		ThreadID:         r.threadID,
		Mode:             domain.Mode(r.mode),
		Status:           domain.DeliveryStatus(r.status),
		Attempts:         r.attempts,
		NextAttemptAt:    r.nextAttemptAt,
		Rendered:         json.RawMessage(r.rendered),
		ProviderResponse: json.RawMessage(r.providerResponse),
		Ambiguous:        r.ambiguous,
		SentAt:           r.sentAt,
		CreatedAt:        r.createdAt,
		UpdatedAt:        r.updatedAt,
	}
	if r.threadSeq != nil {
		d.ThreadSeq = *r.threadSeq
	}
	for _, p := range []struct {
		src *string
		dst *string
	}{
		{r.renderedHash, &d.RenderedHash},
		{r.renderedFallback, &d.RenderedFallback},
		{r.providerMessageID, &d.ProviderMessageID},
		{r.providerConversationID, &d.ProviderConversationID},
		{r.errText, &d.Error},
	} {
		if p.src != nil {
			*p.dst = *p.src
		}
	}
	if r.errClass != nil {
		d.ErrorClass = domain.ErrorClass(*r.errClass)
	}
	return d
}

// NewDelivery is the input for one fan-out row.
type NewDelivery struct {
	ID             uuid.UUID
	NotificationID uuid.UUID
	ChannelID      uuid.UUID
	// ThreadID is nil for a destination that has no thread at all — the generic
	// webhook, which can neither thread nor amend. deliveries_thread_ck permits it
	// only for post_root, which is the only mode such a destination ever gets.
	ThreadID *uuid.UUID
	// ThreadSeq is 0 when there is no thread. Otherwise it was allocated from
	// channel_threads.next_seq INSIDE this transaction.
	ThreadSeq int
	Mode      domain.Mode
	CreatedAt time.Time
}

// DeliveryRepository is the SQL over `notification_deliveries`.
type DeliveryRepository struct {
	q db.Querier
}

// NewDeliveryRepository builds the repository over a fallback querier.
func NewDeliveryRepository(q db.Querier) *DeliveryRepository { return &DeliveryRepository{q: q} }

func (r *DeliveryRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const deliveryColumns = `
  id, org_id, notification_id, channel_id, thread_id, thread_seq, mode, status,
  attempts, next_attempt_at, rendered, rendered_hash, rendered_fallback,
  provider_message_id, provider_conversation_id, provider_response, error,
  error_class, ambiguous, sent_at, created_at, updated_at`

func scanDelivery(row pgx.Row) (domain.Delivery, error) {
	var r deliveryRow
	err := row.Scan(
		&r.id, &r.orgID, &r.notificationID, &r.channelID, &r.threadID, &r.threadSeq,
		&r.mode, &r.status, &r.attempts, &r.nextAttemptAt, &r.rendered, &r.renderedHash,
		&r.renderedFallback, &r.providerMessageID, &r.providerConversationID,
		&r.providerResponse, &r.errText, &r.errClass, &r.ambiguous, &r.sentAt,
		&r.createdAt, &r.updatedAt,
	)
	if err != nil {
		return domain.Delivery{}, err
	}
	return r.toDomain(), nil
}

const insertDeliverySQL = `
INSERT INTO notification_deliveries (
  id, org_id, notification_id, channel_id, thread_id, thread_seq, mode,
  status, attempts, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,'pending',0,$8,$8)
ON CONFLICT (notification_id, channel_id) DO NOTHING
RETURNING` + deliveryColumns

// Create records one fan-out row.
//
// The `ON CONFLICT (notification_id, channel_id) DO NOTHING` is the fan-out
// idempotency guard (§G.5): a redelivered `notify.evaluate` re-derives the same
// destinations and must not produce a second message on any of them. Zero rows
// therefore means "already fanned out", which is success.
//
// ⚠ deliveries_fanout_uniq permits exactly ONE delivery per (notification,
// channel). Where §H.6 asks for `update_root` PLUS `thread_reply`, the reply is
// the delivery and the root refresh rides along inside the same claim — see
// service.DispatchService. Creating two rows here would violate the constraint.
func (r *DeliveryRepository) Create(
	ctx context.Context, s db.TenantScope, in NewDelivery,
) (domain.Delivery, bool, error) {
	var seq *int
	if in.ThreadSeq > 0 {
		v := in.ThreadSeq
		seq = &v
	}

	d, err := scanDelivery(r.db(ctx).QueryRow(ctx, insertDeliverySQL,
		in.ID, s.OrgID(), in.NotificationID, in.ChannelID, in.ThreadID, seq,
		string(in.Mode), in.CreatedAt,
	))
	switch {
	case err == nil:
		return d, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		existing, gerr := r.GetForChannel(ctx, s, in.NotificationID, in.ChannelID)
		if gerr != nil {
			return domain.Delivery{}, false, gerr
		}
		return existing, false, nil
	default:
		return domain.Delivery{}, false, mapErr(err, "delivery_not_found", "create delivery")
	}
}

const getDeliverySQL = `
SELECT` + deliveryColumns + `
  FROM notification_deliveries
 WHERE org_id = $1 AND id = $2`

// Get reads one delivery.
func (r *DeliveryRepository) Get(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
) (domain.Delivery, error) {
	d, err := scanDelivery(r.db(ctx).QueryRow(ctx, getDeliverySQL, s.OrgID(), id))
	if err != nil {
		return domain.Delivery{}, mapErr(err, "delivery_not_found", "delivery")
	}
	return d, nil
}

const getDeliveryForChannelSQL = `
SELECT` + deliveryColumns + `
  FROM notification_deliveries
 WHERE org_id = $1 AND notification_id = $2 AND channel_id = $3`

// GetForChannel reads the one delivery a notification has on a channel.
func (r *DeliveryRepository) GetForChannel(
	ctx context.Context, s db.TenantScope, notificationID, channelID uuid.UUID,
) (domain.Delivery, error) {
	d, err := scanDelivery(r.db(ctx).QueryRow(ctx, getDeliveryForChannelSQL,
		s.OrgID(), notificationID, channelID))
	if err != nil {
		return domain.Delivery{}, mapErr(err, "delivery_not_found", "delivery")
	}
	return d, nil
}

// claimSQL is the §G.5 optimistic-lock claim, with the one addition §G.5 itself
// requires.
//
// The documented shape is:
//
//	UPDATE notification_deliveries SET status='sending', attempts=attempts+1
//	 WHERE id=$1 AND status IN ('pending','failed') RETURNING *
//
// A duplicate worker gets ZERO ROWS and exits. That is the whole mechanism, and
// it is why a delivery id is safe to hand to an at-least-once queue.
//
// The extra disjunct reclaims an ABANDONED claim: a row still `sending`, with no
// provider message id, whose `updated_at` predates the claim lease. §G.5 names
// this the ambiguous case — oto may have crashed after the provider accepted —
// and resolves it by RE-SENDING, because under-delivering a firing alert is
// worse than a visible, labelled duplicate. `ambiguous` is set on exactly those
// rows and only for `post_root`, the one mode where a duplicate is a new message
// rather than an idempotent edit.
const claimSQL = `
UPDATE notification_deliveries
   SET status     = 'sending',
       attempts   = attempts + 1,
       ambiguous  = ambiguous OR (status = 'sending' AND mode = 'post_root'),
       updated_at = $4
 WHERE org_id = $1
   AND id = $2
   AND ( status IN ('pending','failed')
      OR ( status = 'sending'
       AND provider_message_id IS NULL
       AND updated_at < $3 ) )
RETURNING` + deliveryColumns

// Claim takes ownership of one delivery for one attempt.
//
// The second result is false when the row was not claimable: another worker
// holds it, or it is already resolved. That is a NORMAL outcome of an
// at-least-once queue and the caller must exit quietly, not retry.
func (r *DeliveryRepository) Claim(
	ctx context.Context, s db.TenantScope, id uuid.UUID, leaseCutoff, now time.Time,
) (domain.Delivery, bool, error) {
	d, err := scanDelivery(r.db(ctx).QueryRow(ctx, claimSQL, s.OrgID(), id, leaseCutoff, now))
	switch {
	case err == nil:
		return d, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return domain.Delivery{}, false, nil
	default:
		return domain.Delivery{}, false, mapErr(err, "delivery_not_found", "claim delivery")
	}
}

const persistRenderedSQL = `
UPDATE notification_deliveries
   SET rendered = $3, rendered_hash = $4, rendered_fallback = $5, updated_at = $6
 WHERE org_id = $1 AND id = $2`

// PersistRendered writes the exact payload BEFORE the network call (C11, §L.6).
//
// This ordering is not an optimisation and must not be relaxed: a delivery that
// crashed mid-send is only debuggable if the bytes that were about to go out are
// on disk. It is also how a payload that fails structural validation reaches the
// dead-letter with its evidence rather than being silently truncated.
func (r *DeliveryRepository) PersistRendered(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
	payload json.RawMessage, hash, fallback string, now time.Time,
) error {
	if len(payload) == 0 || hash == "" || fallback == "" {
		// deliveries_render_ck and deliveries_fb_ck would both reject this. Failing
		// here names the missing piece; failing there names a constraint.
		return mapErr(errors.New("a rendered payload needs bytes, a hash and a fallback sentence"),
			"delivery_not_found", "persist rendered payload")
	}
	_, err := r.db(ctx).Exec(ctx, persistRenderedSQL, s.OrgID(), id,
		[]byte(payload), hash, fallback, now)
	return mapErr(err, "delivery_not_found", "persist rendered payload")
}

const markSentSQL = `
UPDATE notification_deliveries
   SET status = 'sent',
       provider_message_id = $3,
       provider_conversation_id = $4,
       provider_response = $5,
       sent_at = $6,
       error = NULL,
       error_class = NULL,
       next_attempt_at = NULL,
       updated_at = $6
 WHERE org_id = $1 AND id = $2 AND status = 'sending'`

// MarkSent records the provider handle. The `status = 'sending'` guard makes it
// impossible for a late-returning duplicate worker to overwrite a newer result.
func (r *DeliveryRepository) MarkSent(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
	messageID, conversationID string, raw json.RawMessage, now time.Time,
) error {
	if messageID == "" {
		// deliveries_sent_ck: a sent delivery MUST carry the provider handle.
		return mapErr(errors.New("a sent delivery must carry a provider message id"),
			"delivery_not_found", "mark delivery sent")
	}
	var conv *string
	if conversationID != "" {
		conv = &conversationID
	}
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	_, err := r.db(ctx).Exec(ctx, markSentSQL, s.OrgID(), id,
		messageID, conv, []byte(raw), now)
	return mapErr(err, "delivery_not_found", "mark delivery sent")
}

const markFailedSQL = `
UPDATE notification_deliveries
   SET status = $3, error = $4, error_class = $5, next_attempt_at = $6, updated_at = $7
 WHERE org_id = $1 AND id = $2`

// MarkFailed records a retryable failure and when to try again.
func (r *DeliveryRepository) MarkFailed(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
	message string, class domain.ErrorClass, nextAttemptAt, now time.Time,
) error {
	if message == "" {
		message = string(class)
	}
	_, err := r.db(ctx).Exec(ctx, markFailedSQL, s.OrgID(), id,
		string(domain.DeliveryFailed), message, string(class), nextAttemptAt, now)
	return mapErr(err, "delivery_not_found", "mark delivery failed")
}

// MarkDead records a terminal failure. There is no next attempt.
//
// A dead delivery is not the end of the story: gap recovery advances the
// thread's head past it (§G.7.3) so one poisoned message can never wedge a
// thread forever, and the UI shows delivery state PER ALERT so that oto's
// silence is never indistinguishable from "no alert".
func (r *DeliveryRepository) MarkDead(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
	message string, class domain.ErrorClass, now time.Time,
) error {
	if message == "" {
		message = string(class)
	}
	const q = `
UPDATE notification_deliveries
   SET status = 'dead', error = $3, error_class = $4, next_attempt_at = NULL, updated_at = $5
 WHERE org_id = $1 AND id = $2`
	_, err := r.db(ctx).Exec(ctx, q, s.OrgID(), id, message, string(class), now)
	return mapErr(err, "delivery_not_found", "mark delivery dead")
}

const markSkippedSQL = `
UPDATE notification_deliveries
   SET status = 'skipped', error = $3, error_class = NULL, next_attempt_at = NULL, updated_at = $4
 WHERE org_id = $1 AND id = $2`

// MarkSkipped records a delivery that was deliberately not sent: a coalesced
// no-op update, a slot the gap recovery moved past, or a fact that arrived after
// its thread was frozen.
//
// `skipped` is NOT a failure and must never be rendered as one. It is the row
// that lets an operator asking "why is there no message?" get an answer instead
// of a gap.
func (r *DeliveryRepository) MarkSkipped(
	ctx context.Context, s db.TenantScope, id uuid.UUID, why string, now time.Time,
) error {
	if why == "" {
		why = "skipped"
	}
	_, err := r.db(ctx).Exec(ctx, markSkippedSQL, s.OrgID(), id, why, now)
	return mapErr(err, "delivery_not_found", "mark delivery skipped")
}

const repointToRootSQL = `
UPDATE notification_deliveries
   SET mode = 'post_root',
       status = 'pending',
       ambiguous = true,
       rendered = NULL,
       rendered_hash = NULL,
       rendered_fallback = NULL,
       provider_message_id = NULL,
       provider_conversation_id = NULL,
       error = $3,
       error_class = NULL,
       next_attempt_at = $4,
       updated_at = $4
 WHERE org_id = $1 AND id = $2`

// RepointToRoot converts a delivery whose THREAD POINTER died into a fresh root
// post (§H.9).
//
// The distinction it encodes is the important one: `message_not_found` and
// `edit_window_closed` mean the DESTINATION IS FINE and only oto's handle is
// gone. Retrying the edit would fail forever; giving up would lose the alert.
// So the pointer is cleared, this delivery becomes a root post, and the thread
// re-points itself when it lands.
//
// The rendered payload is cleared because the card must be re-rendered as a
// ROOT, and `ambiguous` is set because the previous card may still be sitting in
// the channel: a reader deserves to know this one might look like a duplicate.
// oto does NOT go and look — its database is its memory of the destination, and
// a system that re-derives its memory from the thing it is remembering has none.
func (r *DeliveryRepository) RepointToRoot(
	ctx context.Context, s db.TenantScope, id uuid.UUID, why string, now time.Time,
) error {
	if why == "" {
		why = "thread pointer lost; re-rooted"
	}
	_, err := r.db(ctx).Exec(ctx, repointToRootSQL, s.OrgID(), id, why, now)
	return mapErr(err, "delivery_not_found", "re-point delivery to a fresh root")
}

const statusesForSQL = `
SELECT status FROM notification_deliveries WHERE org_id = $1 AND notification_id = $2`

// StatusesFor reads the fan-out states a notification's aggregate status is
// folded from.
func (r *DeliveryRepository) StatusesFor(
	ctx context.Context, s db.TenantScope, notificationID uuid.UUID,
) ([]domain.DeliveryStatus, error) {
	rows, err := r.db(ctx).Query(ctx, statusesForSQL, s.OrgID(), notificationID)
	if err != nil {
		return nil, mapErr(err, "delivery_not_found", "read delivery statuses")
	}
	defer rows.Close()

	out := make([]domain.DeliveryStatus, 0, 4)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, mapErr(err, "delivery_not_found", "scan delivery status")
		}
		out = append(out, domain.DeliveryStatus(v))
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "delivery_not_found", "read delivery statuses")
	}
	return out, nil
}

const lastRootHashSQL = `
SELECT rendered_hash
  FROM notification_deliveries
 WHERE org_id = $1
   AND thread_id = $2
   AND status = 'sent'
   AND mode IN ('post_root','update_root')
   AND rendered_hash IS NOT NULL
 ORDER BY sent_at DESC
 LIMIT 1`

// LastRootHash is the sha256 of the payload the thread's root card currently
// shows, or "" if nothing has landed.
//
// It is what turns a flapping alert's forty identical `chat.update` calls into
// one send and thirty-nine `skipped` rows (§G.7.4). The comparison is on the
// RENDERED BYTES rather than on the state, because two different states can
// render identically once the card has been truncated — and a card that does not
// change is a call that buys nothing.
func (r *DeliveryRepository) LastRootHash(
	ctx context.Context, s db.TenantScope, threadID uuid.UUID,
) (string, error) {
	var hash *string
	err := r.db(ctx).QueryRow(ctx, lastRootHashSQL, s.OrgID(), threadID).Scan(&hash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	case err != nil:
		return "", mapErr(err, "delivery_not_found", "read the thread's last rendered hash")
	case hash == nil:
		return "", nil
	default:
		return *hash, nil
	}
}
