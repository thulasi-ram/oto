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

	// ChunkSize is B17: alerts per processing transaction, and EVERY batch is
	// sliced by it. 500 alerts is one transaction, 501 is two, 2 000 is four.
	// The chunk is the size of one multi-row statement on the ingest pool, whose
	// `statement_timeout` is 2 s — see service.applyChunks for why that makes 500
	// a cap rather than a target.
	ChunkSize = 500
	// ChunkThreshold is B17's PARTIAL-MARKING point. ⛔ It is NOT the point at
	// which chunking begins: chunking is unconditional. A batch larger than this is
	// marked `partial` before its first chunk and until the last one commits, so an
	// operator can tell a batch that was in the middle of a long job from one that
	// never started. Both states are resumable, so nothing under the threshold is
	// stranded by not being marked.
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
// ⭐ IT IS DERIVED FROM ALERTMANAGER'S OWN TRANSPORT, AND FROM NOTHING ELSE. Two
// numbers put it here, both properties of the sender rather than of oto:
//
//   - `n_peers × cluster.peer-timeout` — 45 s for a three-node HA cluster at the
//     default 15 s timeout — so an HA sibling's copy of the same notification
//     lands inside the window;
//   - Alertmanager's notify retry backoff, whose ceiling is ~5 minutes, so a
//     retry after a partition lands inside it too.
//
// It is a TRANSPORT window: its only job is to recognise the same delivery
// arriving twice. `TestTheReplayWindowStillCoversHAAndRetries` asserts both
// floors, which is what keeps this constant honest.
//
// ⛔⛔ IT USED TO BE STATED THE OTHER WAY ROUND, AGAINST `refire_grace`, AND THAT
// FRAMING IS DELETED WITH THE SETTING (git-bug 7287b28). The history is worth one
// paragraph, because the trap it describes is real and could return.
//
// This was ten minutes and `refire_grace` then defaulted to ten minutes too, so
// the two windows were exactly EQUAL — which made the inside-the-grace re-fire
// path unreachable by construction: a re-fire inside the grace was also inside
// the replay window and was dropped at ingest, and one the replay window let
// through was already outside the grace. The first live tester had to alter the
// alert set, changing the dedup key, to exercise re-fire at all. Nobody noticed,
// because nothing connected the two numbers.
//
// ⚠️ THE FIX WENT INTO THE OTHER CONSTANT, WHICH IS WHY DELETING THAT ONE COSTS
// THIS BOUND NOTHING. `identity/domain.MinRefireGraceSeconds` was defined as
// `2 × DedupTTL` — the DEPENDENT half — so the grace could not be configured
// underneath the window it had to clear. This constant was never computed from
// the grace; the transport window is the one with a known lower bound, and the
// product setting is the one that had to yield to it. ADR 0040 then retired the
// transition the grace selected, and the owner's ruling of 2026-08-19 deleted the
// setting outright. What is gone is a derivation that pointed AT this number, not
// a derivation OF it.
const DedupTTL = 5 * time.Minute

// RetryAfter is the `Retry-After` sent with every 503 on this path (§G.2).
// Never a 429, and never a 4xx for anything transient.
const RetryAfter = 5 * time.Second
