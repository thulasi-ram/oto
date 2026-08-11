package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/jobs/ordering"
)

// threadRow is the row model of `channel_threads`.
type threadRow struct {
	id             uuid.UUID
	orgID          uuid.UUID
	channelID      uuid.UUID
	subjectKind    string
	subjectID      uuid.UUID
	conversationID *string
	threadID       *string
	rootDeliveryID *uuid.UUID
	replyCount     int
	lastSentSeq    int
	nextSeq        int
	state          string
	deadReason     *string
	createdAt      time.Time
	updatedAt      time.Time
}

func (r threadRow) toDomain() domain.Thread {
	t := domain.Thread{
		ID:             r.id,
		OrgID:          r.orgID,
		ChannelID:      r.channelID,
		SubjectKind:    domain.SubjectKind(r.subjectKind),
		SubjectID:      r.subjectID,
		RootDeliveryID: r.rootDeliveryID,
		ReplyCount:     r.replyCount,
		LastSentSeq:    r.lastSentSeq,
		NextSeq:        r.nextSeq,
		State:          domain.ThreadState(r.state),
		CreatedAt:      r.createdAt,
		UpdatedAt:      r.updatedAt,
	}
	if r.conversationID != nil {
		t.ProviderConversationID = *r.conversationID
	}
	if r.threadID != nil {
		t.ProviderThreadID = *r.threadID
	}
	if r.deadReason != nil {
		t.DeadReason = domain.DeadReason(*r.deadReason)
	}
	return t
}

// ThreadRepository is the SQL over `channel_threads`.
//
// THIS TABLE IS OTO'S MEMORY OF THE DESTINATION. oto never reads a chat provider
// back to rediscover a thread: `conversations.history` is a rate-limited,
// paginated, eventually-consistent view of somebody else's database, and a
// system that reconstructs its own state from it has no state of its own (C9).
// Everything oto knows about where a message went is here, written at the moment
// the provider answered.
type ThreadRepository struct {
	q db.Querier
}

// NewThreadRepository builds the repository over a fallback querier.
func NewThreadRepository(q db.Querier) *ThreadRepository { return &ThreadRepository{q: q} }

func (r *ThreadRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const threadColumns = `
  id, org_id, channel_id, subject_kind, subject_id, provider_conversation_id,
  provider_thread_id, root_delivery_id, reply_count, last_sent_seq, next_seq,
  state, dead_reason, created_at, updated_at`

func scanThread(row pgx.Row) (domain.Thread, error) {
	var r threadRow
	err := row.Scan(
		&r.id, &r.orgID, &r.channelID, &r.subjectKind, &r.subjectID,
		&r.conversationID, &r.threadID, &r.rootDeliveryID, &r.replyCount,
		&r.lastSentSeq, &r.nextSeq, &r.state, &r.deadReason, &r.createdAt, &r.updatedAt,
	)
	if err != nil {
		return domain.Thread{}, err
	}
	return r.toDomain(), nil
}

const ensureThreadSQL = `
INSERT INTO channel_threads (
  id, org_id, channel_id, subject_kind, subject_id, state, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,'opening',$6,$6)
ON CONFLICT (channel_id, subject_kind, subject_id) DO NOTHING
RETURNING` + threadColumns

const getThreadBySubjectSQL = `
SELECT` + threadColumns + `
  FROM channel_threads
 WHERE org_id = $1 AND channel_id = $2 AND subject_kind = $3 AND subject_id = $4`

// Ensure returns the thread binding one AlertGroup GENERATION to one Channel,
// creating it if this is the first fact about that generation.
//
// The uniqueness is on (channel_id, subject_kind, subject_id) rather than on the
// group alone, because a group generation fans out to every channel its policy
// names and each destination owns its own conversation. A RE-OPENED group is a
// NEW generation and therefore a new subject_id and a new thread — which is what
// stops an incident from six weeks ago growing a reply today.
func (r *ThreadRepository) Ensure(
	ctx context.Context, s db.TenantScope,
	channelID uuid.UUID, kind domain.SubjectKind, subjectID uuid.UUID, now time.Time,
) (domain.Thread, error) {
	t, err := scanThread(r.db(ctx).QueryRow(ctx, ensureThreadSQL,
		uuid.New(), s.OrgID(), channelID, string(kind), subjectID, now))
	switch {
	case err == nil:
		return t, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return domain.Thread{}, mapErr(err, "thread_not_found", "create channel thread")
	}

	t, err = scanThread(r.db(ctx).QueryRow(ctx, getThreadBySubjectSQL,
		s.OrgID(), channelID, string(kind), subjectID))
	if err != nil {
		return domain.Thread{}, mapErr(err, "thread_not_found", "channel thread")
	}
	return t, nil
}

const getThreadSQL = `
SELECT` + threadColumns + `
  FROM channel_threads
 WHERE org_id = $1 AND id = $2`

// Get reads one thread.
func (r *ThreadRepository) Get(
	ctx context.Context, s db.TenantScope, id uuid.UUID,
) (domain.Thread, error) {
	t, err := scanThread(r.db(ctx).QueryRow(ctx, getThreadSQL, s.OrgID(), id))
	if err != nil {
		return domain.Thread{}, mapErr(err, "thread_not_found", "channel thread")
	}
	return t, nil
}

// ⭐ EVERY WRITE IN THIS FILE ADVANCES `updated_at` MONOTONICALLY —
// GREATEST(updated_at, $n) — and that is a correctness guard, not a nicety.
//
// Both timestamps on this row come from the application: Ensure above names
// `created_at` from the caller's clock. But "the application" is N pods with N
// clocks, and a thread is CREATED by whichever pod evaluated the policy and then
// written by whichever pod the dispatcher happened to run on. A few milliseconds
// of lag between them writes an `updated_at` BELOW `created_at` and fails
// `threads_time_ck` with a 23514 — and the first write after Ensure is the one
// with the least clock distance to absorb.
//
// On THIS table a constraint failure is not merely a 500: these statements are
// what record where a message landed. Losing one leaves oto's memory of the
// destination behind the destination itself, which is the state §H the whole
// module is written to avoid. OrderingStore.Advance below has always done this;
// it is the same idiom, for the same reason, as `channels` and `orgs`.
const allocateSeqSQL = `
UPDATE channel_threads
   SET next_seq = next_seq + 1, updated_at = GREATEST(updated_at, $3)
 WHERE org_id = $1 AND id = $2
RETURNING next_seq - 1`

// AllocateSeq takes the next FIFO position in a thread.
//
// It MUST be called inside the transaction that creates the delivery. That is
// the entire ordering design (§G.7): sequence assignment is totally ordered by
// the CAUSAL ORDER OF DOMAIN EVENTS, because the number is taken in the same
// transaction as the event that justified the message. Allocating it later — at
// dispatch, say — would order sends by worker scheduling instead, which is not
// an order anybody wants to explain to an operator.
//
// ⚠ IT MUST ALSO BE CALLED ONLY WHEN A ROW WAS ACTUALLY CREATED. `next_seq` is a
// plain column, not a sequence: a ROLLBACK undoes `next_seq = next_seq + 1`, and
// a concurrent allocator blocks on the row lock until then and re-uses the
// number, so an aborted transaction leaves no hole at all. What DOES leave one is
// a COMMITTED allocation with no delivery behind it — which is what an
// unconditional call in front of an `ON CONFLICT DO NOTHING` insert produces on
// every re-run of an at-least-once job. Gap recovery can heal that hole, but only
// after something has waited for it, so the fix is not to punch it: see
// service.fanOut, which inserts first and allocates only for a row it created.
func (r *ThreadRepository) AllocateSeq(
	ctx context.Context, s db.TenantScope, threadID uuid.UUID, now time.Time,
) (int, error) {
	var seq int
	err := r.db(ctx).QueryRow(ctx, allocateSeqSQL, s.OrgID(), threadID, now).Scan(&seq)
	if err != nil {
		return 0, mapErr(err, "thread_not_found", "allocate a thread sequence")
	}
	return seq, nil
}

const recordRootSQL = `
UPDATE channel_threads
   SET provider_conversation_id = $3,
       provider_thread_id       = $4,
       root_delivery_id         = $5,
       state                    = 'open',
       dead_reason              = NULL,
       last_sent_seq            = GREATEST(last_sent_seq, $6),
       updated_at               = GREATEST(updated_at, $7)
 WHERE org_id = $1 AND id = $2`

// RecordRoot stores the durable handle the provider just returned.
//
// BOTH HALVES COME FROM THE API RESPONSE. The conversation id is never taken
// from the request or from the channel's configuration: a configured name can be
// stale, renamed, or ambiguous between a private and a public conversation, and
// the response is the only statement of where the message actually landed
// (§H.1 S7).
//
// The message id is a STRING and is stored as one. Parsing it as a float rounds
// it, and a rounded id silently threads replies onto nothing.
func (r *ThreadRepository) RecordRoot(
	ctx context.Context, s db.TenantScope, threadID uuid.UUID,
	conversationID, messageID string, rootDeliveryID uuid.UUID, seq int, now time.Time,
) error {
	if conversationID == "" || messageID == "" {
		// threads_open_ck: an OPEN thread must carry both halves of the handle.
		return mapErr(errors.New("a landed root needs both a conversation id and a message id"),
			"thread_not_found", "record the thread root")
	}
	_, err := r.db(ctx).Exec(ctx, recordRootSQL, s.OrgID(), threadID,
		conversationID, messageID, rootDeliveryID, seq, now)
	return mapErr(err, "thread_not_found", "record the thread root")
}

const recordReplySQL = `
UPDATE channel_threads
   SET reply_count   = reply_count + 1,
       last_sent_seq = GREATEST(last_sent_seq, $3),
       updated_at    = GREATEST(updated_at, $4)
 WHERE org_id = $1 AND id = $2`

// RecordReply advances the thread past a reply that landed.
func (r *ThreadRepository) RecordReply(
	ctx context.Context, s db.TenantScope, threadID uuid.UUID, seq int, now time.Time,
) error {
	_, err := r.db(ctx).Exec(ctx, recordReplySQL, s.OrgID(), threadID, seq, now)
	return mapErr(err, "thread_not_found", "record a thread reply")
}

const advanceSeqSQL = `
UPDATE channel_threads
   SET last_sent_seq = GREATEST(last_sent_seq, $3), updated_at = GREATEST(updated_at, $4)
 WHERE org_id = $1 AND id = $2 AND $3 < next_seq`

// AdvanceSent moves the ordering head. GREATEST makes it convergent, so a
// redelivered worker cannot walk the head backwards — and it guards `updated_at`
// for the same reason on the other axis, so a redelivered worker on a lagging
// pod cannot walk the row's version backwards either.
func (r *ThreadRepository) AdvanceSent(
	ctx context.Context, s db.TenantScope, threadID uuid.UUID, seq int, now time.Time,
) error {
	_, err := r.db(ctx).Exec(ctx, advanceSeqSQL, s.OrgID(), threadID, seq, now)
	return mapErr(err, "thread_not_found", "advance the thread sequence")
}

const markThreadDeadSQL = `
UPDATE channel_threads
   SET state = 'dead', dead_reason = $3, updated_at = GREATEST(updated_at, $4)
 WHERE org_id = $1 AND id = $2 AND state <> 'dead'`

// MarkDead records that a terminal provider error killed this thread.
//
// THIS IS A STATE TRANSITION, NOT A RETRY (§H.9). `is_archived` and
// `channel_not_found` will still be true on the thirteenth attempt; retrying
// them burns a worker slot a real alert needs and delays the moment an operator
// is told their destination is gone.
func (r *ThreadRepository) MarkDead(
	ctx context.Context, s db.TenantScope, threadID uuid.UUID,
	reason domain.DeadReason, now time.Time,
) error {
	_, err := r.db(ctx).Exec(ctx, markThreadDeadSQL, s.OrgID(), threadID, string(reason), now)
	return mapErr(err, "thread_not_found", "mark the thread dead")
}

const clearPointerSQL = `
UPDATE channel_threads
   SET provider_thread_id = NULL,
       root_delivery_id   = NULL,
       reply_count        = 0,
       state              = 'opening',
       dead_reason        = NULL,
       updated_at         = GREATEST(updated_at, $3)
 WHERE org_id = $1 AND id = $2`

// ClearPointer degrades a thread whose ROOT MESSAGE is gone but whose
// DESTINATION is fine (§H.9: message_not_found, cannot_reply_to_message,
// restricted_action_thread_locked, edit_window_closed).
//
// The next root-mode delivery posts a fresh card and re-points the thread. The
// reply count resets because the new root starts a new conversation, and the
// conversation id is deliberately KEPT: it is still the right destination, and it
// came from an API response rather than from config, so it is worth more than
// anything oto could re-derive.
func (r *ThreadRepository) ClearPointer(
	ctx context.Context, s db.TenantScope, threadID uuid.UUID, now time.Time,
) error {
	_, err := r.db(ctx).Exec(ctx, clearPointerSQL, s.OrgID(), threadID, now)
	return mapErr(err, "thread_not_found", "clear the thread pointer")
}

const freezeThreadSQL = `
UPDATE channel_threads
   SET state = 'frozen', updated_at = GREATEST(updated_at, $3)
 WHERE org_id = $1 AND id = $2 AND state IN ('opening','open')`

// Freeze closes a thread because its group generation closed. Anything still
// queued for it is about a state nobody will look at again.
func (r *ThreadRepository) Freeze(
	ctx context.Context, s db.TenantScope, threadID uuid.UUID, now time.Time,
) error {
	_, err := r.db(ctx).Exec(ctx, freezeThreadSQL, s.OrgID(), threadID, now)
	return mapErr(err, "thread_not_found", "freeze the thread")
}

// ---------------------------------------------------------------- ordering.Store

// OrderingStore adapts this module's two tables to the platform ordering gate.
//
// The gate lives in platform because the ADVISORY-LOCK + SEQUENCE-GATING +
// GAP-RECOVERY primitive is general; the tables it gates are ours. This adapter
// is the whole of the coupling, and it is why platform never depends on a domain.
//
// EVERY METHOD MUST RUN INSIDE THE CALLER'S TRANSACTION AND UNDER THE THREAD'S
// ADVISORY LOCK. A read taken outside the lock is a read of state that may
// already be false, and acting on it is exactly the race the lock exists for.
type OrderingStore struct {
	q      db.Querier
	scope  db.TenantScope
	events *EventRepository
}

// NewOrderingStore builds the adapter for one tenant.
//
// It is scoped rather than taking a scope per call because ordering.Store is
// platform's interface and platform has no concept of a tenant. Binding the org
// at construction is what keeps every query it issues org-scoped anyway.
func NewOrderingStore(q db.Querier, s db.TenantScope, events *EventRepository) *OrderingStore {
	return &OrderingStore{q: q, scope: s, events: events}
}

func (o *OrderingStore) db(ctx context.Context) db.Querier { return db.FromContext(ctx, o.q) }

const loadOrderingThreadSQL = `
SELECT state, last_sent_seq, next_seq, provider_thread_id IS NOT NULL
  FROM channel_threads
 WHERE org_id = $1 AND id = $2`

// LoadThread reads the four-field ordering projection.
func (o *OrderingStore) LoadThread(ctx context.Context, threadID uuid.UUID) (ordering.Thread, error) {
	var (
		state      string
		last, next int
		rooted     bool
	)
	err := o.db(ctx).QueryRow(ctx, loadOrderingThreadSQL, o.scope.OrgID(), threadID).
		Scan(&state, &last, &next, &rooted)
	if err != nil {
		return ordering.Thread{}, mapErr(err, "thread_not_found", "load the thread ordering state")
	}
	return ordering.Thread{
		ID:          threadID,
		State:       ordering.ThreadState(state),
		LastSentSeq: last,
		NextSeq:     next,
		RootLanded:  rooted,
	}, nil
}

const slotAtSQL = `
SELECT id, status, updated_at
  FROM notification_deliveries
 WHERE org_id = $1 AND thread_id = $2 AND thread_seq = $3
 LIMIT 1`

// SlotAt describes whatever occupies one sequence position.
func (o *OrderingStore) SlotAt(
	ctx context.Context, threadID uuid.UUID, seq int,
) (ordering.Slot, error) {
	var (
		id        uuid.UUID
		status    string
		updatedAt time.Time
	)
	err := o.db(ctx).QueryRow(ctx, slotAtSQL, o.scope.OrgID(), threadID, seq).
		Scan(&id, &status, &updatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The allocating transaction rolled back after taking the number. The slot
		// can never be filled, and the gate must be told so rather than waiting for
		// a row that cannot exist.
		return ordering.Slot{Present: false}, nil
	case err != nil:
		return ordering.Slot{}, mapErr(err, "delivery_not_found", "read a thread sequence slot")
	}

	st := domain.DeliveryStatus(status)
	return ordering.Slot{
		Present:   true,
		ItemID:    id,
		Resolved:  st.Resolved(),
		Sent:      st == domain.DeliverySent,
		Skipped:   st == domain.DeliverySkipped,
		InFlight:  st == domain.DeliverySending,
		UpdatedAt: updatedAt,
	}, nil
}

// Advance moves the head past a slot that will never send, and records the fact.
//
// THIS IS THE LIVENESS HALF OF THE ORDERING INVARIANT (§G.7.3). Sequence gating
// alone wedges permanently the first time a delivery can never complete; this is
// what guarantees a poisoned message costs one skipped reply rather than a dead
// thread.
//
// The `delivery.skipped` event is not optional bookkeeping. The UI shows
// delivery state per alert precisely so that oto's silence is never
// indistinguishable from "no alert" — a head that moved on with no record of
// what it moved past would recreate exactly that ambiguity.
//
// ⚠ ordering.ReasonAlreadySent IS NOT A SKIP. The head is catching up with a
// message the destination is currently displaying; writing `delivery.skipped` for
// it would put a false statement into an append-only timeline that the rest of
// the system treats as the truth. That reason advances the head and records
// nothing else.
func (o *OrderingStore) Advance(
	ctx context.Context, threadID uuid.UUID, seq int, itemID uuid.UUID, reason ordering.SkipReason,
) error {
	// GREATEST keeps `updated_at` MONOTONIC. This is the one write in the module
	// that stamps the DATABASE's clock onto a row whose other timestamps came from
	// the caller's, and `threads_time_ck`/`deliveries_time_ck` compare the two: a
	// pod a second ahead of Postgres would otherwise fail gap recovery on a
	// constraint, which is the liveness path failing for a clock reason.
	const advance = `
UPDATE channel_threads
   SET last_sent_seq = GREATEST(last_sent_seq, $3),
       updated_at    = GREATEST(updated_at, now())
 WHERE org_id = $1 AND id = $2 AND $3 < next_seq`
	if _, err := o.db(ctx).Exec(ctx, advance, o.scope.OrgID(), threadID, seq); err != nil {
		return mapErr(err, "thread_not_found", "advance the thread sequence")
	}

	if !reason.Anomalous() {
		return nil
	}

	if itemID != uuid.Nil {
		const skip = `
UPDATE notification_deliveries
   SET status = 'skipped', error = $3, error_class = NULL,
       next_attempt_at = NULL, updated_at = GREATEST(updated_at, now())
 WHERE org_id = $1 AND id = $2 AND status IN ('pending','failed')`
		if _, err := o.db(ctx).Exec(ctx, skip, o.scope.OrgID(), itemID, string(reason)); err != nil {
			return mapErr(err, "delivery_not_found", "mark a skipped delivery")
		}
	}

	if o.events == nil {
		return nil
	}
	return o.events.AppendThreadSkip(ctx, o.scope, threadID, seq, itemID, string(reason))
}
