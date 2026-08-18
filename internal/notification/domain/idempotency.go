package domain

import (
	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
)

// SubjectKind is what a Notification is about — the closed set of
// `notifications.subject_kind` (notifications_subjkind_ck, widened to three
// members by migration 00056 and to four by 00058) and of
// `channel_threads.subject_kind` (threads_subjkind_ck).
//
// It has always been hashed into the idempotency key, and that foresight is what
// let the second and third kinds arrive without re-keying anything: the same
// Reason at the same `state_version` about a Case and about its group are two
// intents rather than a collision, because the kind was in the pre-image from the
// beginning.
//
// ⛔ THE KIND IS WHAT THE FACT IS ABOUT, NEVER WHERE IT IS DELIVERED.
// `notifications.group_id` is the delivery target for the seventeen signal Reasons
// and a thread is keyed by the AlertGroup generation whatever this says. `digest`
// is the exception in both halves: it has no group (notifications_target_ck) and
// its thread is keyed by the policy.
type SubjectKind string

// SubjectAlertGroup is one AlertGroup GENERATION — a re-opened group is a new
// generation and therefore a new subject.
//
// ⭐ IT KEEPS THE SPELLING `alert_group` while `enrichments.subject_kind` spells
// the same altitude `group`; migration 00056 records why the two vocabularies
// differ by one word. Rows already carry `alert_group` under `notif_subject_idx`
// and `threads_subject_uniq`, and re-spelling a persisted enum value to match a
// neighbouring table buys nothing and re-keys everything.
//
// The other two members are declared in reason.go, beside the Reason → SubjectKind
// allocation that gives them meaning.
const SubjectAlertGroup SubjectKind = "alert_group"

// subjectKinds is the closed set, and it is the SPELLING half of the contract:
// exactly the four values notifications_subjkind_ck and threads_subjkind_ck
// admit. WHICH of them a given fact may claim is a separate question, answered by
// `reasonSubjects` in reason.go.
var subjectKinds = map[SubjectKind]struct{}{
	SubjectAlert:      {},
	SubjectCase:       {},
	SubjectAlertGroup: {},
	// `digest` (migration 00058) is admitted by both CHECKs, and it is the one kind
	// whose two tables mean different things by `subject_id`: a NOTIFICATION's
	// digest subject is (policy, window) and carries the policy id, while a
	// THREAD's digest subject is the POLICY alone — one ongoing conversation per
	// policy per channel, one reply per window.
	SubjectDigest: {},
}

// Valid reports whether k is in the closed set.
//
// ⚠️ IT USED TO BE `k == SubjectAlertGroup`, WHICH WENT FALSE THE MOMENT A
// NOTIFICATION COULD SAY `case`. A membership test that names one member of a set
// that has grown does not fail loudly — it rejects the new, honest values as
// unknown, which is the shape of bug this whole change exists to remove.
func (k SubjectKind) Valid() bool {
	_, ok := subjectKinds[k]
	return ok
}

// String renders the kind as stored.
func (k SubjectKind) String() string { return string(k) }

// IdempotencyKey is SPEC §C.7, literally:
//
//	idempotency_key := hex( sha256(
//	      field(org_id_bytes(16))
//	   || field(subject_kind) || field(subject_id_bytes(16))
//	   || field(reason)       || itoa(state_version)
//	) )
//
// where field(x) := uint32be(len(x)) || x.
//
// It is what makes "all_resolved at state_version 7" exist EXACTLY ONCE, under
// `notifications_idem_uniq UNIQUE (org_id, idempotency_key)`. A 23505 on that
// index is the mechanism WORKING and must be handled as success, never surfaced
// as an error (§L.9).
//
// Three details are load-bearing, and all three are now the KERNEL's to keep:
//
//   - the UUIDs are hashed as their 16 RAW BYTES, not their textual form, so a
//     change in how oto formats a UUID cannot silently re-key every notification;
//   - state_version is `strconv.Itoa`, matching the spec's `itoa`, so 7 hashes as
//     "7" and never as "07" or "7.0";
//   - every field but the last carries a 4-byte big-endian BYTE COUNT, so the
//     pre-image decodes to exactly one field tuple and no pair of adjacent fields
//     can be re-split into a different pair with the same bytes. The predecessor
//     framing separated fields with 0x00, which is injective only while no field
//     can CONTAIN a NUL — true of these five, but not of `receiver` and `expr` in
//     the neighbouring §C keys, and one key framed differently from its
//     neighbours is a trap. The argument in full is on alerts/domain writeField.
//
// The trailing itoa(state_version) needs no prefix: it is the remainder.
//
// # THIS IS AN ADAPTER, NOT AN IMPLEMENTATION
//
// It used to be the second of two spellings of §C.7, and the live one: the
// kernel's ComputeIdempotencyKey had no production caller, so the copy a reader
// would assume canonical was the dead one and only a cross-check test kept the
// pair honest. What this function contributes is the part the kernel cannot have:
// SubjectKind and Reason are this module's closed enums, and alerts/domain may
// import no other domain package. So the types stop here and the bytes are the
// kernel's.
//
// The signature is unchanged, so notify.go's call site is unchanged, and the
// digest is unchanged for every input — the two implementations were already
// byte-identical, which is why this could collapse without re-keying anything.
func IdempotencyKey(
	orgID uuid.UUID,
	kind SubjectKind,
	subjectID uuid.UUID,
	reason Reason,
	stateVersion int,
) string {
	return kernel.ComputeIdempotencyKey(
		orgID, string(kind), subjectID, string(reason), stateVersion,
	).String()
}

// idempotencyKeyLength is the hex width notifications_idem_ck enforces.
const idempotencyKeyLength = 64

// ValidIdempotencyKey reports whether s has the shape
// notifications_idem_ck demands: exactly 64 lowercase hex characters.
func ValidIdempotencyKey(s string) bool {
	if len(s) != idempotencyKeyLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
