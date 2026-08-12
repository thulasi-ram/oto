// Package idempotency claims a client-supplied `Idempotency-Key` so a retried
// mutation cannot act twice (SPEC §E.1, `parameters/IdempotencyKeyHeader`).
//
// ⭐⭐ IT STORES NO RESPONSE AND NO SECRET, and that is the design rather than an
// omission. The header's wording — "returns the original result rather than
// acting twice" — reads as an instruction to cache responses, and for
// `createApiToken` the response IS a plaintext credential. A response cache would
// turn a table meant to prevent one orphaned credential into a table holding
// every credential, in the clear, addressed by a string the client chose. So a
// claim records only THAT a key was used, the sha256 of the body it was used
// with, and the id of what that call created. There is no field on any type here
// that could hold a secret, which is the same property `identity/domain.APIToken`
// is built around.
//
// ⭐ WHY IT LIVES UNDER `platform` AND NOT IN `identity`.
//
// The first callers are `identity` (`createApiToken`, `revokeApiToken`) and
// `sources` (`createSource`, `rotateSourceIngestToken`), and 28 operations
// across nine modules declare the header. `Idempotency-Key` is a TRANSPORT mechanism — a property of
// how a request is retried, not a fact about orgs, users or tokens — so putting
// its store in `identity` would make every other module declare a port into
// identity for a header identity has nothing to do with, and would invert the
// dependency direction the moment `alerts` or `grouping` needed it.
//
// `platform` is the layer every module may already import and that may import
// none (`platform-must-not-import-domains`). This package holds no domain type
// and needs none: an org id, a principal id, a contract operationId, an opaque
// string and a digest are all it takes, which is exactly why it can live here at
// all. It is the same argument `platform/tuning` and `platform/secrets` make.
//
// ⭐ THE CLAIM PARTICIPATES IN THE CALLER'S TRANSACTION and refuses to run
// outside one. Pass 2 mints a credential and claims the key in a single unit of
// work, so that a claim which loses the race rolls the mint back with it — the
// alternative is a token that exists with no claim naming it, which is the very
// orphan this package exists to prevent.
package idempotency
