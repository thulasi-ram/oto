package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// AlertIdentity is one alert's contribution to the C.5 batch dedup key: the
// recomputed Alertmanager fingerprint and the per-alert status.
type AlertIdentity struct {
	Fingerprint string
	Status      string
}

// ComputeBatchDedupKey derives `batch_dedup_key` (SPEC §C.5):
//
//	hex( sha256(
//	     source_id_bytes(16) || 0x00
//	  || groupKey            || 0x00
//	  || receiver            || 0x00
//	  || notification_reason || 0x00
//	  || join(sorted("<fingerprint>:<status>" for each alert), 0x1F) ) )
//
// This is the identity of a batch's MEANING, which is what makes it the right
// replay-suppression key: two HA Alertmanagers notifying about the same group at
// the same moment send byte-different bodies (different `externalURL`, different
// map iteration order) and must still collapse onto one batch. The sha256 of the
// body would not do that; this does.
//
// `notification_reason` is IN the pre-image on purpose: "first notification" and
// "repeat interval elapsed" for an otherwise identical alert set are genuinely
// different events, and collapsing them would hide a repeat from the update path.
//
// The alert list is sorted, so map and slice ordering cannot change the key.
func ComputeBatchDedupKey(sourceID uuid.UUID, groupKey, receiver, notificationReason string, alerts []AlertIdentity) string {
	parts := make([]string, 0, len(alerts))
	for _, a := range alerts {
		parts = append(parts, a.Fingerprint+":"+a.Status)
	}
	slices.Sort(parts)

	h := sha256.New()
	writeField(h, sourceID[:])
	writeField(h, []byte(groupKey))
	writeField(h, []byte(receiver))
	writeField(h, []byte(notificationReason))
	_, _ = h.Write([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(h.Sum(nil))
}

// Checksum is sha256 of the raw request body, exactly 32 bytes
// (ingest_batches_checksum_ck). It identifies BYTES, where the dedup key
// identifies meaning; storing both is what lets an operator tell "Alertmanager
// sent this twice" apart from "the same thing happened twice".
func Checksum(body []byte) []byte {
	sum := sha256.Sum256(body)
	return sum[:]
}

// RawFingerprint recomputes Alertmanager's own fingerprint over an ARBITRARY
// label map (SPEC §C.3).
//
// It reproduces `prometheus/common/model.LabelSet.Fingerprint().String()`
// exactly: FNV-1a 64 over the labels sorted by name, writing
// `name || 0xFF || value || 0xFF`, rendered "%016x".
//
// It exists alongside `alerts/domain.Labels.Fingerprint` rather than calling it
// because the two have different domains. The kernel's version takes a
// constructed LabelSet, which cannot hold 900 labels or a 9 KiB value. This one
// is TOTAL over untrusted input, because the C.5 dedup key must be computable for
// a payload that is about to be rejected — otherwise a hostile batch could evade
// replay suppression simply by being malformed. For any label set the kernel
// would accept, the two agree byte for byte.
//
// oto always uses its own value and never the wire `fingerprint` (C10); a
// mismatch is recorded as a metric, never a failure.
func RawFingerprint(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	slices.Sort(names)

	h := fnv.New64a()
	for _, name := range names {
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte{fingerprintSep})
		_, _ = h.Write([]byte(labels[name]))
		_, _ = h.Write([]byte{fingerprintSep})
	}
	s := strconv.FormatUint(h.Sum64(), 16)
	return strings.Repeat("0", fingerprintHexLen-len(s)) + s
}

// fingerprintSep is prometheus/common/model.SeparatorByte.
const fingerprintSep = 0xFF

// fingerprintHexLen is the width of Alertmanager's "%016x" fingerprint.
const fingerprintHexLen = 16

// writeField writes one NUL-terminated field of a digest pre-image. Every §C key
// separates its fields with 0x00 so that two different field splits can never
// produce the same byte string.
func writeField(h interface{ Write([]byte) (int, error) }, b []byte) {
	_, _ = h.Write(b)
	_, _ = h.Write([]byte{0x00})
}
