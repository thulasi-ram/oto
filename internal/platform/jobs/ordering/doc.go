// Package ordering implements per-thread delivery ordering: sequence gating
// under a Postgres advisory lock, with dead-delivery gap recovery (SPEC §G.7).
//
// # The problem
//
// Slack thread replies must land in lifecycle order — the root first, then
// "3 more alerts joined", then "resolved" — while ordering ACROSS threads must
// stay fully parallel, because parallelism across threads is the entire point.
//
// # Why not a per-thread queue
//
// Thread count is unbounded: one per AlertGroup generation, created and abandoned
// continuously. No queue system handles millions of ephemeral ordered partitions
// well. Advisory locks make ordering a property of the WRITE, cost one hash, and
// need no new infrastructure.
//
// # The invariant
//
//	Within one thread, `thread_seq` is allocated by `channel_threads.next_seq++`
//	INSIDE the transaction that creates the delivery. Sequence order is therefore
//	the causal order of domain events, and the sequence is contiguous from 1.
//
//	A delivery may be sent only while holding the thread's transaction-scoped
//	advisory lock AND only when `thread_seq == channel_threads.last_sent_seq + 1`.
//
//	`last_sent_seq` advances by exactly one per RESOLVED slot, where resolved
//	means sent, dead or skipped. It never skips a slot that is still in play, and
//	it never fails to skip one that is finished.
//
// The lock is what makes the read-decide-write sequence atomic across worker
// pods; the sequence check is what makes it ordered; and gap recovery is what
// makes it live.
//
// # Liveness: why gap recovery exists
//
// Sequence gating alone deadlocks the moment one delivery can never complete. A
// Slack `channel_not_found` on seq 4 leaves seq 5, 6, 7… snoozing on
// `last_sent_seq+1 == 4` forever, and the thread is wedged for the lifetime of
// the group. SPEC §G.7.3 is therefore not an optimisation: `Recover` advances
// `last_sent_seq` past a slot that is finished-but-unsent and records why.
// Head-of-line blocking is the real killer here, not throughput.
//
// # Liveness is a CONTRACT, not a guarantee this package can keep alone
//
// "A poisoned message can never wedge a thread forever" is only true if the
// caller upholds three obligations, because a River snooze consumes no attempt
// and no retry ceiling can therefore end one:
//
//  1. Item.NeedsRoot MUST be the RE-DERIVED mode. Feeding the gate the mode
//     stored on the row makes it block on a missing root that the sender already
//     knows how to replace with a fresh one.
//  2. ActionRecoverThread MUST reach a terminal outcome when recovery advances
//     nothing. Answering a fruitless recovery with another snooze is the wedge.
//  3. Recovery.StalledItem MUST be acted on. Recover deliberately refuses to skip
//     an unresolved slot, so it names the delivery that owns the head; if nothing
//     restarts that delivery, the refusal is itself a wedge.
//
// Each of those was violated once, and each cost a thread its voice permanently.
//
// # Layering
//
// This package holds no SQL and knows nothing about Slack or about
// `notification_deliveries`. It declares Store, which the notification module
// implements, and Decide, which is a pure function over the two small structs
// below. Platform may not depend on a domain, and the ordering rule is not a
// domain rule — it is a property of writing to any ordered external stream.
package ordering
