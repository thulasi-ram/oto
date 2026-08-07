package db

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// LockNamespace partitions the 64-bit Postgres advisory-lock space so that two
// unrelated subsystems can never collide on a key.
//
// The space is split as `(namespace << 32) | fnv32(name)`, which is deliberately
// the same shape River uses for its own locks: River builds its keys from a
// configurable 32-bit `AdvisoryLockPrefix` in the high half. Every namespace
// below is therefore reserved against River's prefix as well as against the other
// oto namespaces — see JobsAdvisoryLockPrefix, which is what the river client MUST
// be configured with.
//
// A collision here is not a crash. It is two unrelated operations silently
// serialising against each other, forever, under load. That is why the split is
// explicit rather than "just hash the string".
type LockNamespace int32

// The reserved advisory-lock namespaces. Adding one means adding it here, never
// inline at a call site.
const (
	// LockNamespaceRiver is River's own half of the space. oto never mints a key
	// in it; it exists so the reservation is visible.
	LockNamespaceRiver LockNamespace = 0x6F74_0001

	// LockNamespaceThread serialises all sends for one ChannelThread across every
	// worker pod (SPEC §G.7).
	LockNamespaceThread LockNamespace = 0x6F74_0002

	// LockNamespacePartition serialises partition creation and detachment
	// (SPEC §D.11), which is otherwise racy between two `partitions.manage` runs.
	LockNamespacePartition LockNamespace = 0x6F74_0003

	// LockNamespaceReconcile serialises one source's reconciler run so two pods
	// cannot both walk the same Alertmanager (SPEC §G.8).
	LockNamespaceReconcile LockNamespace = 0x6F74_0004
)

// JobsAdvisoryLockPrefix is the value river.Config.AdvisoryLockPrefix MUST be set
// to. It equals LockNamespaceRiver, which is reserved and never minted by
// AdvisoryKey, so River's keys and oto's keys occupy disjoint halves of the space.
const JobsAdvisoryLockPrefix = int32(LockNamespaceRiver)

// ErrNotInTransaction is returned by the xact-scoped lock helpers when the caller
// is not inside a transaction.
//
// `pg_advisory_xact_lock` outside a transaction acquires a lock that is released
// at the end of the implicit single-statement transaction — that is, immediately.
// The call succeeds and guards nothing, which is the worst possible failure mode
// for a mutual-exclusion primitive, so it is refused instead.
var ErrNotInTransaction = errors.New("db: advisory xact lock requires an open transaction")

// AdvisoryKey derives the advisory-lock key for name within ns.
//
// The low 32 bits are the leading bytes of sha256(name). sha256 rather than a
// cheap hash because the names are attacker-adjacent (they derive from ids that
// appear in payloads) and because the cost is irrelevant next to the round trip.
func AdvisoryKey(ns LockNamespace, name string) int64 {
	sum := sha256.Sum256([]byte(name))
	low := binary.BigEndian.Uint32(sum[:4])
	return int64(uint64(uint32(ns))<<32 | uint64(low)) //nolint:gosec // deliberate 32/32 packing
}

// AdvisoryKeyUUID is AdvisoryKey over an id's canonical string form.
func AdvisoryKeyUUID(ns LockNamespace, id uuid.UUID) int64 {
	return AdvisoryKey(ns, id.String())
}

// AdvisoryXactLock takes the transaction-scoped advisory lock for key, blocking
// until it is held. The lock is released by COMMIT or ROLLBACK — there is no
// unlock call and therefore no way to leak one.
//
// The caller MUST already be inside a transaction; see ErrNotInTransaction.
func AdvisoryXactLock(ctx context.Context, q Querier, key int64) error {
	if !InTx(ctx) {
		return ErrNotInTransaction
	}
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
		return fmt.Errorf("db: pg_advisory_xact_lock(%d): %w", key, err)
	}
	return nil
}

// TryAdvisoryXactLock takes the transaction-scoped advisory lock for key without
// blocking, reporting whether it was acquired.
//
// Use this where losing the race means "somebody else is already doing this, and
// doing it twice is waste rather than corruption" — the periodic maintenance jobs.
// Use AdvisoryXactLock where losing the race means "wait your turn" — ordering.
func TryAdvisoryXactLock(ctx context.Context, q Querier, key int64) (bool, error) {
	if !InTx(ctx) {
		return false, ErrNotInTransaction
	}
	var got bool
	if err := q.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, key).Scan(&got); err != nil {
		return false, fmt.Errorf("db: pg_try_advisory_xact_lock(%d): %w", key, err)
	}
	return got, nil
}
