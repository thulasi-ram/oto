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
//
// ⛔ AND THE EXCEPTION THIS PARAGRAPH NAMED IS NOW THE RULE (git-bug `7570090`). It
// used to say `notifications.group_id` was the delivery target for the seventeen
// signal Reasons, that a thread was keyed by the AlertGroup generation whatever the
// kind said, and that `digest` was "the exception in both halves". `alert_groups` is
// deleted: every conversation is now keyed by what the fact is ABOUT — a Case or a
// policy — so the two halves have converged and `digest` is no longer special.
type SubjectKind string

// ⛔ `SubjectAlertGroup SubjectKind = "alert_group"` WAS HERE AND IS DELETED
// (git-bug `7570090`). It named the `alert_groups` generation a fact was about, and
// there is no such row. Every Reason that allocated it now allocates `SubjectCase`,
// and the two that could only ever have meant a generation — `new_alerts` and
// `some_resolved` — left the vocabulary with it: a Case is one alert's episode, so
// "more of them started" and "some of them stopped" have no plurality to be about.

// subjectKinds is the closed set, and it is the SPELLING half of the contract:
// exactly the THREE values notifications_subjkind_ck and threads_subjkind_ck admit
// since `alert_group` was dropped (git-bug `7570090`); it was four. WHICH of them a given fact may claim is a separate question, answered by
// `reasonSubjects` in reason.go.
var subjectKinds = map[SubjectKind]struct{}{
	SubjectAlert: {},
	SubjectCase:  {},
	// `digest` (migration 00058) is admitted by both CHECKs, and it is the one kind
	// whose two tables mean different things by `subject_id`: a NOTIFICATION's
	// digest subject is (policy, window) and carries the policy id, while a
	// THREAD's digest subject is the POLICY alone — one ongoing conversation per
	// policy per channel, one reply per window.
	SubjectDigest: {},
}

// allSubjectKinds is the same closed set as an ORDERED slice, and the two
// declarations are separate for the reason `allReasons` and `reasonSubjects` are:
// this one is the ORDER a published vocabulary is rendered in, that one is the
// membership test. `TestTheSubjectKindOrderIsTheSubjectKindSet` binds them, so a
// fourth kind added to one and not the other fails a test rather than producing a
// contract enum that is missing a value the column admits.
//
// ⭐ THE ORDER IS THE ORDER THE KINDS ARRIVED IN — alert and case at migration
// 00056, digest at 00058 — which is the same convention `allReasons` follows
// ("migration 00018 declares it, 00058 appends to it"). Re-sorting a published
// enum is a contract change for nothing.
var allSubjectKinds = []SubjectKind{SubjectAlert, SubjectCase, SubjectDigest}

// AllSubjectKinds returns the closed SubjectKind vocabulary in declaration order.
// The slice is freshly built so a caller cannot mutate the vocabulary.
//
// It exists because a policy's subject-kind binding (`SubjectBinding` in
// policy.go, migration `00072`) is a SET OVER THIS VOCABULARY and has to validate
// against it in Go — the same division `toReasons` makes for `reasons`, and for the
// same reason: a duplicated list at the API boundary is the second copy that
// drifts.
func AllSubjectKinds() []SubjectKind {
	out := make([]SubjectKind, len(allSubjectKinds))
	copy(out, allSubjectKinds)
	return out
}

// Valid reports whether k is in the closed set.
//
// ⚠️ IT USED TO BE `k == SubjectAlertGroup`, WHICH WENT FALSE THE MOMENT A
// NOTIFICATION COULD SAY `case`. A membership test that names one member of a set
// that has grown does not fail loudly — it rejects the new, honest values as
// unknown. ⭐ The set has now SHRUNK past the member that test named, which is the
// other half of the same lesson: a closed set belongs in one place.
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
//	   || field(occasion_id_bytes(16))   -- only when the occasion is non-nil
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
//   - the occasion is the ONE optional field, and `uuid.Nil` writes no bytes at
//     all rather than sixteen zero ones — so the thirteen Reasons that have no
//     occasion keep the exact key they have always had, and only a Reason that
//     names one is re-keyed. See ComputeIdempotencyKey for why appending after the
//     raw tail is still uniquely decodable, and NeedsOccasion below for who names one;
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
// The digest is unchanged for every input the five-argument form could express —
// the two implementations were already byte-identical, which is why they could
// collapse without re-keying anything, and `occasionID = uuid.Nil` is that form.
func IdempotencyKey(
	orgID uuid.UUID,
	kind SubjectKind,
	subjectID uuid.UUID,
	reason Reason,
	stateVersion int,
	occasionID uuid.UUID,
) string {
	return kernel.ComputeIdempotencyKey(
		orgID, string(kind), subjectID, string(reason), stateVersion, occasionID,
	).String()
}

// NeedsOccasion reports whether this Reason's facts are told apart by an occasion
// id rather than by `state_version` alone (§C.7).
//
// ⭐ IT IS A PROPERTY OF THE REASON, NOT OF THE CALLER, and that is why it is
// declared here beside the key rather than left implicit in whatever the alerts
// service happens to pass. The two members are `snoozed` and `unsnoozed`, and they
// are the same two exempted from snooze suppression in §B.8.4 — for the same
// underlying reason. A snooze is not a Case state transition: `StartSnooze` takes
// an Alert, `alert_cases.state_version` never moves, and every fact about snoozing
// inside one episode therefore arrives at the same `state_version`. Without an
// occasion the second one is a byte-identical key and
// `notifications_idem_uniq` drops it — a re-snooze from 1h to 4h that nobody is
// told about, and a second wake-up in the same episode that nobody is told about
// either.
//
// ⚠️ IT IS DELIBERATELY NOT CONSULTED BY `IdempotencyKey`, AND IT IS NOT A
// VALIDATION. The key hashes what it is given; this answers whether a CALLER that
// has no occasion to give is missing one. `evaluate` uses it to WARN and carry on,
// never to refuse: an occasion-less snooze is a wiring gap whose worst outcome is
// the swallowed duplicate this field exists to prevent, and refusing it would turn
// that into a dead-lettered job — the FIRST announcement lost to protect the
// second, which is the trade in the wrong direction (§B.6).
func (r Reason) NeedsOccasion() bool {
	switch r {
	case ReasonSnoozed, ReasonUnsnoozed:
		return true
	default:
		return false
	}
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
