package domain_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/thulasiram/oto/internal/ingestion/domain"
)

var dedupSource = uuid.MustParse("018f3a4b-0000-7000-8000-0000000000c3")

// TestBatchDedupKeyPreImageIsLengthPrefixed pins the §C.5 framing itself rather
// than a digest.
//
// Every field but the last carries a 4-byte big-endian BYTE COUNT; the joined
// fingerprint list is the remainder and carries none.
func TestBatchDedupKeyPreImageIsLengthPrefixed(t *testing.T) {
	t.Parallel()

	var want []byte
	want = append(want, 0x00, 0x00, 0x00, 0x10) // len(source_id_bytes) == 16
	want = append(want, dedupSource[:]...)
	want = append(want, 0x00, 0x00, 0x00, 0x02) // len("gk")
	want = append(want, "gk"...)
	want = append(want, 0x00, 0x00, 0x00, 0x03) // len("rcv")
	want = append(want, "rcv"...)
	want = append(want, 0x00, 0x00, 0x00, 0x05) // len("fired")
	want = append(want, "fired"...)
	want = append(want, "aaaa:firing\x1fbbbb:resolved"...) // sorted, 0x1F-joined, raw
	sum := sha256.Sum256(want)

	assert.Equal(t, hex.EncodeToString(sum[:]),
		domain.ComputeBatchDedupKey(dedupSource, "gk", "rcv", "fired", []domain.AlertIdentity{
			{Fingerprint: "bbbb", Status: "resolved"},
			{Fingerprint: "aaaa", Status: "firing"},
		}),
		"the §C.5 pre-image is uint32be(len(x))||x per field, with the joined alert list raw")
}

// TestBatchDedupKeyIsInjectiveOverAdversarialFields is the property the framing
// exists to have.
//
// §C.4 says explicitly that Alertmanager's own `groupKey` is unescaped and
// unbounded, and `receiver` is free-form text out of alertmanager.yml — so both
// may carry any byte, including the NUL the retired framing used as a terminator.
// A collision here is not a duplicate alert: it is a LOST batch, a genuine
// notification suppressed as a replay by `UNIQUE (source_id, dedup_key)`.
func TestBatchDedupKeyIsInjectiveOverAdversarialFields(t *testing.T) {
	t.Parallel()

	adversarial := []string{
		"",
		"\x00",
		"\x00\x00\x00\x00",
		"a",
		"a\x00",
		"\x00a",
		"a\x00b",
		"ab",
		`{}:{alertname="X"}`,
		"\x1f",
		"日本語",
		strings.Repeat("x", 300),
	}
	alertSets := [][]domain.AlertIdentity{
		nil,
		{{Fingerprint: "aaaa", Status: "firing"}},
		{{Fingerprint: "aaaa", Status: "resolved"}},
		{{Fingerprint: "aaaa", Status: "firing"}, {Fingerprint: "bbbb", Status: "firing"}},
	}

	seen := map[string]string{}
	for _, groupKey := range adversarial {
		for _, receiver := range adversarial {
			for _, alerts := range alertSets {
				id := strconv.Quote(groupKey) + "|" + strconv.Quote(receiver) + "|" + identify(alerts)
				key := domain.ComputeBatchDedupKey(dedupSource, groupKey, receiver, "fired", alerts)
				if prev, dup := seen[key]; dup && prev != id {
					t.Fatalf("batch_dedup_key collision:\n  %s\n  %s\nboth key to %s", prev, id, key)
				}
				seen[key] = id
			}
		}
	}
	assert.Len(t, seen, len(adversarial)*len(adversarial)*len(alertSets),
		"one dedup key per distinct (groupKey, receiver, alert set)")
}

// TestBatchDedupKeyIsMeaningNotBytes is the property that makes §C.5 the right
// replay-suppression key: two HA Alertmanagers notifying about the same group send
// byte-different bodies and different map orderings, and must still collapse.
func TestBatchDedupKeyIsMeaningNotBytes(t *testing.T) {
	t.Parallel()

	forward := []domain.AlertIdentity{
		{Fingerprint: "aaaa", Status: "firing"},
		{Fingerprint: "bbbb", Status: "resolved"},
	}
	reversed := []domain.AlertIdentity{forward[1], forward[0]}

	assert.Equal(t,
		domain.ComputeBatchDedupKey(dedupSource, "gk", "rcv", "fired", forward),
		domain.ComputeBatchDedupKey(dedupSource, "gk", "rcv", "fired", reversed),
		"the alert list is sorted, so slice order cannot move the key")

	// `notification_reason` is IN the pre-image on purpose: a first notification
	// and a repeat over the same alert set are different events.
	assert.NotEqual(t,
		domain.ComputeBatchDedupKey(dedupSource, "gk", "rcv", "fired", forward),
		domain.ComputeBatchDedupKey(dedupSource, "gk", "rcv", "repeat", forward))

	other := uuid.MustParse("018f3a4b-0000-7000-8000-0000000000d4")
	assert.NotEqual(t,
		domain.ComputeBatchDedupKey(dedupSource, "gk", "rcv", "fired", forward),
		domain.ComputeBatchDedupKey(other, "gk", "rcv", "fired", forward))
}

func identify(alerts []domain.AlertIdentity) string {
	parts := make([]string, 0, len(alerts))
	for _, a := range alerts {
		parts = append(parts, strconv.Quote(a.Fingerprint)+":"+strconv.Quote(a.Status))
	}
	return "[" + strings.Join(parts, ",") + "]"
}
