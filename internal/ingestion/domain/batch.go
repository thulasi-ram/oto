package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Mode distinguishes the two producers of a batch (ingest_batches_mode_ck).
type Mode string

const (
	// ModePush is an Alertmanager webhook: the only thing that reaches the ingest
	// endpoint. It can never witness suppression beginning, because MuteStage
	// drops suppressed alerts before the webhook fires (C1).
	ModePush Mode = "push"
	// ModeReconcile is a GET /api/v2/alerts sweep. It is NOT a second ingestion
	// path (C18) — it produces Observations for the same state machine — but its
	// raw response is recorded here so the two are equally auditable.
	ModeReconcile Mode = "reconcile"
)

// String renders the mode.
func (m Mode) String() string { return string(m) }

// Valid reports membership of ingest_batches_mode_ck.
func (m Mode) Valid() bool { return m == ModePush || m == ModeReconcile }

// Status is the lifecycle of one batch (ingest_batches_state_ck).
type Status string

const (
	// StatusPending means the batch is durably on disk and its job is queued.
	// This is the state a 202 promises.
	StatusPending Status = "pending"
	// StatusProcessed means every alert in the batch has been through the state
	// machine. Terminal.
	StatusProcessed Status = "processed"
	// StatusPartial means a batch over ChunkThreshold is mid-chunking (§G.4).
	// It is RESUMABLE, not terminal — see Resumable.
	StatusPartial Status = "partial"
	// StatusFailed means processing gave up. Terminal; `error` is required.
	StatusFailed Status = "failed"
)

// String renders the status.
func (s Status) String() string { return string(s) }

// Resumable reports whether `ingest.process_batch` should do any work.
//
// ⭐ SPEC §G.4 step 1 says "if status != 'pending', exit (replay-safe)", and
// §G.4's own chunking clause then creates `partial` for a batch that is halfway
// through its chunks. Read literally, those two sentences strand every batch over
// ChunkThreshold whose worker died mid-flight: the retry would see `partial` and
// exit, and thousands of alerts would sit on disk forever.
//
// `partial` is therefore treated as resumable, and that is safe because every
// write underneath is idempotent by construction (§G.5): the alert upsert is
// ON CONFLICT, and event appends dedupe through `alert_event_keys`. Re-running a
// chunk that already committed produces no second observation.
//
// `processed` and `failed` are the two genuinely terminal states, and those are
// the ones the "no longer pending" rule is about.
func (s Status) Resumable() bool { return s == StatusPending || s == StatusPartial }

// TopStatus is the batch-level `status` field of the webhook envelope
// (ingest_batches_status_ck). It is `resolved` only when EVERY alert in the group
// is resolved; a single payload may mix both, and the per-alert status is what
// actually drives the state machine.
const (
	// TopStatusFiring is the firing batch status.
	TopStatusFiring = "firing"
	// TopStatusResolved is the resolved batch status.
	TopStatusResolved = "resolved"
)

// Batch is one raw, durably-recorded webhook body or reconciler sweep
// (`ingest_batches`).
//
// It is written inside the short ingest transaction and processed
// asynchronously. THIS TABLE IS THE REASON A 2xx IS A PROMISE: the row is on disk
// before the status line goes out, so an acknowledged payload is recoverable even
// if every worker dies immediately afterwards.
type Batch struct {
	ID       uuid.UUID
	OrgID    uuid.UUID
	SourceID uuid.UUID
	Mode     Mode

	// ReceivedAt is oto's clock at accept time and the PARTITION KEY. It is never
	// an upstream timestamp: partition routing must not be steerable by a broken
	// or hostile upstream clock (§C12).
	ReceivedAt time.Time

	// BodyBytes is the raw body length before redaction, 1..MaxBodyBytes.
	BodyBytes int
	// Checksum is sha256 of the RAW request body, exactly 32 bytes. It identifies
	// BYTES; DedupKey identifies MEANING, and the difference is why both exist.
	Checksum []byte
	// DedupKey is the C.5 batch_dedup_key, 64 lowercase hex characters. Its
	// uniqueness is enforced in `ingest_dedup`, not here.
	DedupKey string

	// AMVersion is the payload's `version` field, the literal "4". It is NOT a
	// feature-detection signal: do not branch on it, do not reject other values.
	AMVersion string
	// GroupKey is Alertmanager's raw groupKey, stored verbatim for observability.
	// OPAQUE — MUST NOT be parsed (C3, §C.4). oto computes its own group_key.
	GroupKey string
	Receiver string
	// NotificationReason is AM >= 0.32.0's `notification_reason`, "" when absent.
	// It drives the post-vs-update decision downstream (C5); an empty value falls
	// back to fingerprint-set diffing rather than guessing.
	NotificationReason string
	// StatusTop is the envelope's batch-level status, or "" when absent.
	StatusTop string

	AlertCount int
	// TruncatedAlerts is how many alerts were dropped from this payload — by the
	// upstream's own `max_alerts` and by oto's B2 cap, summed. Non-zero from the
	// upstream means those bodies are permanently lost and oto can only say
	// "+N more".
	TruncatedAlerts int

	// Payload is the raw body AFTER redaction (§C.9.2). Redaction happens before
	// persistence, so a secret in an annotation never lands on disk.
	Payload json.RawMessage

	Status      Status
	ProcessedAt *time.Time
	Error       string
}

// NewBatchParams is the input to a batch insert. It is data, not an entity: the
// invariants live in the DDL and in the bounds that produced it.
type NewBatchParams struct {
	ID                 uuid.UUID
	SourceID           uuid.UUID
	Mode               Mode
	ReceivedAt         time.Time
	BodyBytes          int
	Checksum           []byte
	DedupKey           string
	AMVersion          string
	GroupKey           string
	Receiver           string
	NotificationReason string
	StatusTop          string
	AlertCount         int
	TruncatedAlerts    int
	Payload            json.RawMessage
}

// DedupHit is the outcome of the `ingest_dedup` insert (§C.5).
//
// A conflict is the NORMAL, EXPECTED outcome for an HA Alertmanager pair:
// Prometheus fans out to every Alertmanager and their gossip dedup is
// best-effort, so duplicates are guaranteed by design rather than exceptional.
// On conflict the handler answers 202 with the ORIGINAL batch id and does
// nothing else, so the response is stable across every replay.
type DedupHit struct {
	// BatchID is the batch that won the race — this one, or the original.
	BatchID uuid.UUID
	// Inserted is false when this batch lost to an earlier identical one.
	Inserted bool
}
