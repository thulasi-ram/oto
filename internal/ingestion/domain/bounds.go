package domain

import (
	"time"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
)

// The ingest bounds of SPEC §L.3.2, every value literal and binding.
//
// These are LAYER 2 bounds: they apply to an untrusted, hostile-by-default
// upstream payload, and their governing rule is the opposite of layer 1's —
//
//	a bound violation is RECORDED, never fatal to the batch, and never a 4xx.
//
// A 4xx (including 429) makes Alertmanager delete the notification permanently
// and silently (C4, ADR 0007), so the only violations that may answer with a 4xx
// are the two that no retry of the same bytes could ever fix: B1 (413) and B16
// (400). Both are still written to `ingest_rejections`. Everything else is
// recorded and answered 202.
//
// Where a bound is also a domain invariant, the constant is ALIASED from the
// shared kernel rather than restated: R9 binds a bound to the DTO tag, the
// domain constructor and the DDL CHECK, and two literals is how they drift.
const (
	// MaxBodyBytes is B1: the HTTP body cap, 8 MiB. Over it: 413 and an
	// `ingest_rejections` row with reason `body_too_large`.
	MaxBodyBytes int64 = 8 << 20 // 8388608, == ingest_batches_bytes_ck

	// MaxAlertsPerBatch is B2. Over it the batch is TRUNCATED to this many, the
	// excess is recorded as `too_many_alerts`, the rest is processed, and the
	// response is still 202.
	MaxAlertsPerBatch = 10000 // == ingest_batches_count_ck

	// MaxLabelsPerAlert is B3. Over it THAT ALERT is rejected, not the batch.
	MaxLabelsPerAlert = alerts.MaxLabels // 64
	// MaxLabelNameBytes is B4.
	MaxLabelNameBytes = alerts.MaxLabelNameBytes // 1024
	// MaxLabelValueBytes is B5.
	MaxLabelValueBytes = alerts.MaxLabelValueBytes // 4096
	// MaxLabelSetBytes is B6: the total canonical serialisation of one label set.
	MaxLabelSetBytes = alerts.MaxLabelSetBytes // 16384
	// MaxAnnotations is B7. Over it the EXCESS ANNOTATIONS are dropped and the
	// alert is kept — an annotation is display text and never identity.
	MaxAnnotations = alerts.MaxAnnotations // 32
	// MaxAnnotationValueBytes is B8. Over it the value is truncated with a marker
	// and the alert is kept.
	MaxAnnotationValueBytes = alerts.MaxAnnotationValueBytes // 16384
	// MaxAlertNameBytes is B11.
	MaxAlertNameBytes = alerts.MaxAlertNameBytes // 1024
	// MaxGeneratorURLBytes is B14. Over it the URL is truncated and the alert kept.
	MaxGeneratorURLBytes = alerts.MaxGeneratorURLBytes // 8192

	// MaxReceiverBytes is B15 for `receiver`. Truncate; keep the batch.
	MaxReceiverBytes = 4096
	// MaxGroupKeyBytes is B15 for Alertmanager's `groupKey`. Truncate; keep the
	// batch. AM's groupKey is unescaped and unbounded and MUST NOT be parsed (C3).
	MaxGroupKeyBytes = 4096

	// MaxJSONDepth is B16: the nesting ceiling of the decoder. Over it the body is
	// `undecodable` — recorded, then 400, because the same bytes never will decode.
	MaxJSONDepth = 32

	// ChunkSize is B17: alerts per processing transaction.
	ChunkSize = 500
	// ChunkThreshold is B17's split point. A batch larger than this is processed in
	// ChunkSize chunks, each its own transaction, with the batch marked `partial`
	// until the last chunk commits.
	ChunkThreshold = 2000
)

// B9's label-name charset is `validate.PatternLabelName`, enforced by
// alerts/domain.NewLabels. B10's "alertname present and non-empty" is enforced by
// alerts/domain.NewLabelSet. Neither is restated here; restating a regex is how a
// charset acquires two definitions and one bug.
//
// B18 and B19 are the same kind of bound and live in the same place: what
// Postgres can actually STORE. `alerts/domain.UnstorableReason` is the single
// definition — no U+0000, valid UTF-8 — and the two bounds differ only in the
// verdict. B18 (label value) REJECTS the alert through NewLabels, because a label
// value is identity. B19 (annotation) SANITISES the value or drops the annotation
// in decode.boundAnnotations, because an annotation is prose. Both record; neither
// is silent.

// The B12/B13 timestamp sanity windows.
//
// They exist because a broken upstream clock (§C12) poisons partition routing:
// an alert claiming `startsAt: 2087` would demand a partition sixty years out.
// Skew inside the window is MEASURED AND SURFACED, never a reason to reject.
const (
	// MaxStartsAtPast is B12's floor: `startsAt` may not be older than now-365d.
	MaxStartsAtPast = 365 * 24 * time.Hour
	// MaxStartsAtFuture is B12's ceiling: `startsAt` may not be later than now+24h.
	MaxStartsAtFuture = 24 * time.Hour
	// MaxEndsAtFuture is B13's ceiling: `endsAt` may not be later than now+365d.
	// A violation CLAMPS to startsAt and keeps the alert; it never drops it.
	MaxEndsAtFuture = 365 * 24 * time.Hour
)

// DedupTTL is how long an `ingest_dedup` row suppresses a replay (SPEC §C.5).
//
// It covers `n_peers × cluster.peer-timeout` (45 s for a three-node cluster) and
// Alertmanager's own retry backoff, whose ceiling is ~5 minutes, so both an HA
// sibling and a retry after a partition land inside it. It is a TRANSPORT
// window: its only job is to recognise the same delivery arriving twice.
//
// ⛔ IT MUST STAY STRICTLY BELOW `refire_grace`, AND THAT IS NOT A COINCIDENCE
// TO BE PRESERVED BY LUCK. It was ten minutes, and `refire_grace` then defaulted
// to ten minutes too, so the two windows were exactly equal — which made the T8
// re-fire path UNREACHABLE by construction:
//
//   - a re-fire inside `refire_grace` is, by the equality, also inside the replay
//     window, so it was dropped at ingest and the state machine never saw it;
//   - a re-fire the replay window let through was, by the same equality, already
//     outside `refire_grace`, so it opened a new generation (T7) instead.
//
// The first live tester had to alter the alert set — changing the dedup key — to
// exercise re-fire at all. The relationship is now enforced from the other end:
// `MinRefireGrace` in `identity/domain` is `2 × DedupTTL`, so every legal
// configuration leaves a window at least this wide in which a re-fire is both
// OBSERVABLE and INSIDE the grace. `TestTheReplayWindowIsStrictlyInsideRefireGrace`
// pins the two constants together.
//
// The transport window is the one that yielded, because it has a known lower
// bound (a peer timeout and a retry budget, both properties of Alertmanager) while
// `refire_grace` is a product setting an operator owns.
const DedupTTL = 5 * time.Minute

// RetryAfter is the `Retry-After` sent with every 503 on this path (§G.2).
// Never a 429, and never a 4xx for anything transient.
const RetryAfter = 5 * time.Second
