package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/google/uuid"
)

// SubjectKind is what a Notification is about — the closed set of
// `notifications.subject_kind` (notifications_subjkind_ck).
//
// v1 has exactly one member. It exists as a type anyway because it is hashed
// into the idempotency key: the day a second kind appears, keys minted for the
// first must not collide with it, and that is only true if the kind was in the
// hash from the beginning.
type SubjectKind string

// SubjectAlertGroup is the only v1 subject: one AlertGroup GENERATION.
const SubjectAlertGroup SubjectKind = "alert_group"

// Valid reports whether k is in the closed set.
func (k SubjectKind) Valid() bool { return k == SubjectAlertGroup }

// String renders the kind as stored.
func (k SubjectKind) String() string { return string(k) }

// IdempotencyKey is SPEC §C.7, literally:
//
//	idempotency_key := hex( sha256(
//	      org_id_bytes(16) || 0x00
//	   || subject_kind || 0x00 || subject_id_bytes(16) || 0x00
//	   || reason       || 0x00 || itoa(state_version)
//	) )
//
// It is what makes "all_resolved at state_version 7" exist EXACTLY ONCE, under
// `notifications_idem_uniq UNIQUE (org_id, idempotency_key)`. A 23505 on that
// index is the mechanism WORKING and must be handled as success, never surfaced
// as an error (§L.9).
//
// Three details are load-bearing:
//
//   - the UUIDs are hashed as their 16 RAW BYTES, not their textual form, so a
//     change in how oto formats a UUID cannot silently re-key every notification;
//   - state_version is `strconv.Itoa`, matching the spec's `itoa`, so 7 hashes as
//     "7" and never as "07" or "7.0";
//   - every field is 0x00-separated, so no pair of adjacent fields can be
//     concatenated into a different pair with the same bytes.
func IdempotencyKey(
	orgID uuid.UUID,
	kind SubjectKind,
	subjectID uuid.UUID,
	reason Reason,
	stateVersion int,
) string {
	h := sha256.New()
	sep := []byte{0x00}

	org := orgID
	subj := subjectID

	_, _ = h.Write(org[:])
	_, _ = h.Write(sep)
	_, _ = h.Write([]byte(kind))
	_, _ = h.Write(sep)
	_, _ = h.Write(subj[:])
	_, _ = h.Write(sep)
	_, _ = h.Write([]byte(reason))
	_, _ = h.Write(sep)
	_, _ = h.Write([]byte(strconv.Itoa(stateVersion)))

	return hex.EncodeToString(h.Sum(nil))
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
