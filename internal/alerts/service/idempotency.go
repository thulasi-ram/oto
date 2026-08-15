package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/idempotency"
)

// ⭐⭐ THIS FILE IS WHY A RETRIED SNOOZE OR COMMENT NO LONGER ACTS TWICE, AND IT
// IS IN THE SERVICE BECAUSE THE CLAIM MUST JOIN THE ACT'S OWN TRANSACTION.
//
// `Idempotency-Key` was declared on both operations by the contract and read by
// nothing (ticket a6cc834). Snooze superseded its own incumbent and inserted a
// second row; Comment minted its §C.8 dedupe key from the WALL CLOCK, so a retry
// arrived a few nanoseconds later, produced a different key, and appended a
// second annotation. Both are the exact failure the header exists to prevent, and
// both happened during precisely the network conditions a client retries under.
//
// ⛔ THERE IS NO MIDDLEWARE THAT COULD HAVE DONE THIS, and there cannot be:
// `platform/idempotency` refuses to claim outside the caller's transaction
// (`repository.go`, `idempotency_claim_outside_tx`), and a filter that ran before
// the handler opened one could never call Claim at all. It is wired per verb, the
// same way `createSource` and the three token operations were.
//
// The protocol itself — the intent, the unwired-deployment `503`, the
// Fresh/Conflicted/replay interpretation — is `platform/idempotency`'s
// (intent.go). These verbs REPLAY rather than refuse: nothing a snooze or a
// comment returns is a secret, so their policy is `idempotency.Replay`, and what
// the replay read returns is decided per verb at the call sites in actions.go.

// The operationIds a key is claimed under here. One key must not be replayable
// across two different operations, so the operation is part of the claim's
// identity, and these are the contract's own operationIds spelled once so a claim
// and the contract cannot drift.
//
// ⭐ THE GROUP FORMS ARE THEIR OWN OPERATIONS. `snoozeAlertGroup` fans out onto
// the same `Snooze` primitive, but it is not the same request: one press means
// "quieten these forty", and claiming it under `snoozeAlert` would let a group
// gesture refuse a later single-alert one carrying the same client key.
// ⚠️ THEY ARE EXPORTED BECAUSE THE OPERATION IS THE CALLER'S FACT, NOT THIS
// SERVICE'S. `Snooze` serves two contract operations — `snoozeAlert` from the
// alerts router and `snoozeAlertGroup` from grouping's fan-out — and only the
// layer that received the request knows which one the caller's key belongs to.
var (
	OpSnoozeAlert         = idempotency.MustOperation("snoozeAlert")
	OpCommentOnAlert      = idempotency.MustOperation("commentOnAlert")
	OpSnoozeAlertGroup    = idempotency.MustOperation("snoozeAlertGroup")
	OpCommentOnAlertGroup = idempotency.MustOperation("commentOnAlertGroup")
)

// CodeIdempotencyUnavailable means the caller sent an `Idempotency-Key` this
// deployment cannot honour. It is a DEPLOYMENT fact, not a caller error.
const CodeIdempotencyUnavailable = idempotency.CodeUnavailable

// Claims is the `Idempotency-Key` claim store, satisfied by
// `*platform/idempotency.Repository`.
//
// ⛔ It is OPTIONAL, like every other port this module declares — but "absent"
// does not degrade to "served unguarded". A KEYED request this deployment cannot
// honour is refused with a `503`; see idempotency.Require at the call sites.
type Claims interface {
	Claim(ctx context.Context, s db.TenantScope, c idempotency.Claim) (idempotency.Result, error)
}

// Idempotency is the caller's `Idempotency-Key` intent for one human verb —
// the platform's own Intent, under the name this module's seams (grouping's
// fan-out, the alerts router) have always crossed it as.
type Idempotency = idempotency.Intent

// commentDedupeKey is the §C.8 key a KEYED comment appends under.
//
// ⭐ IT IS DERIVED FROM THE CALLER'S KEY, NOT FROM THE WALL CLOCK, and that is
// what makes `alert_event_keys`' existing `ON CONFLICT` finally catch a retry.
// The unkeyed form (`Comment` below) still mints a clock key, because two people
// who type "restarted it" ten minutes apart wrote two facts and a content-derived
// key would silently discard the second — losing a human's words is worse than
// keeping one too many.
//
// ⛔ THE KEY IS HASHED RATHER THAN CONCATENATED, for a reason the constraint
// states: `alert_event_keys_ck` bounds a dedupe key at 200 characters and the
// contract lets a client send 200 on its own. `"comment:" + uuid + ":" + key`
// would overflow it and turn a legal header into a 23514. A sha256 is 64 hex
// characters whatever the client sent.
func commentDedupeKey(alertID uuid.UUID, idem Idempotency) string {
	sum := sha256.Sum256([]byte(idem.Key.String()))
	return "comment:" + alertID.String() + ":idem:" + hex.EncodeToString(sum[:])
}
