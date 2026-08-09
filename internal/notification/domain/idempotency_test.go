package domain_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/notification/domain"
)

var (
	idemOrg     = uuid.MustParse("018f3a4b-0000-7000-8000-0000000000a1")
	idemSubject = uuid.MustParse("018f3a4b-0000-7000-8000-0000000000e5")
	idemOther   = uuid.MustParse("018f3a4b-0000-7000-8000-0000000000f6")
)

// TestIdempotencyKeyPreImageIsLengthPrefixed pins the §C.7 framing itself rather
// than a digest, so a change to the pre-image cannot pass as an unrelated hash
// difference.
//
// Every field but the last carries a 4-byte big-endian BYTE COUNT; the trailing
// itoa(state_version) is the remainder and carries none.
func TestIdempotencyKeyPreImageIsLengthPrefixed(t *testing.T) {
	t.Parallel()

	var want []byte
	want = append(want, 0x00, 0x00, 0x00, 0x10) // len(org_id_bytes) == 16
	want = append(want, idemOrg[:]...)
	want = append(want, 0x00, 0x00, 0x00, byte(len(domain.SubjectAlertGroup)))
	want = append(want, domain.SubjectAlertGroup...)
	want = append(want, 0x00, 0x00, 0x00, 0x10) // len(subject_id_bytes) == 16
	want = append(want, idemSubject[:]...)
	want = append(want, 0x00, 0x00, 0x00, byte(len(domain.ReasonAllResolved)))
	want = append(want, domain.ReasonAllResolved...)
	want = append(want, "7"...) // itoa(state_version), raw
	sum := sha256.Sum256(want)

	assert.Equal(t, hex.EncodeToString(sum[:]),
		domain.IdempotencyKey(idemOrg, domain.SubjectAlertGroup, idemSubject, domain.ReasonAllResolved, 7),
		"the §C.7 pre-image is uint32be(len(x))||x per field, with itoa(state_version) raw")
}

// TestIdempotencyKeyAgreesWithTheKernel is the reason both implementations may
// exist at all: §C.7 is ONE key with two spellings, and the day they disagree the
// UNIQUE (org_id, idempotency_key) index stops meaning what it says.
//
// alerts/domain owns the shape; this package owns the closed enums and is what the
// notify path calls. Nothing else keeps them in step, so this does.
func TestIdempotencyKeyAgreesWithTheKernel(t *testing.T) {
	t.Parallel()

	for _, kind := range []domain.SubjectKind{domain.SubjectAlertGroup, "occurrence", ""} {
		for _, reason := range []domain.Reason{
			domain.ReasonAllResolved, domain.ReasonFired, "", "a", "ab",
		} {
			for _, version := range []int{0, 1, 7, 12, -1, 1 << 40} {
				for _, subject := range []uuid.UUID{idemSubject, idemOther} {
					got := domain.IdempotencyKey(idemOrg, kind, subject, reason, version)
					want := alerts.ComputeIdempotencyKey(
						idemOrg, string(kind), subject, string(reason), version).String()
					require.Equal(t, want, got,
						"§C.7 must have one value: kind=%q reason=%q v=%d", kind, reason, version)
				}
			}
		}
	}
}

// TestIdempotencyKeyIsInjective is the property the framing exists to have. The
// fields here are closed internal enums, so this key was never forgeable through a
// field's CONTENT — but "safe because of a charset elsewhere" is precisely the
// reasoning that made the retired NUL framing look sound, so the property is
// asserted over adversarial values rather than assumed from the enums.
//
// The version/reason pair is the sharp edge: under a framing that let a field's
// bytes run into its neighbour, ("ab", 1) and ("a", 12) are the same notification.
func TestIdempotencyKeyIsInjective(t *testing.T) {
	t.Parallel()

	kinds := []domain.SubjectKind{"", "a", "ab", "alert_group", "a\x00b", "occurrence"}
	reasons := []domain.Reason{"", "a", "ab", "b", "1", "12", "a\x00b", "all_resolved"}
	versions := []int{0, 1, 2, 12, 120, 7}

	seen := map[string]string{}
	for _, kind := range kinds {
		for _, reason := range reasons {
			for _, version := range versions {
				id := strings.Join([]string{
					strconv.Quote(string(kind)),
					strconv.Quote(string(reason)),
					strconv.Itoa(version),
				}, "|")
				key := domain.IdempotencyKey(idemOrg, kind, idemSubject, reason, version)
				if prev, dup := seen[key]; dup && prev != id {
					t.Fatalf("idempotency_key collision:\n  %s\n  %s\nboth key to %s", prev, id, key)
				}
				seen[key] = id
			}
		}
	}
	assert.Len(t, seen, len(kinds)*len(reasons)*len(versions),
		"one key per (subject_kind, reason, state_version)")
}

// TestIdempotencyKeyEveryInputParticipates keeps the key's inputs honest: dropping
// any one of them would make two genuinely different notifications collapse into
// one that can never be re-sent.
func TestIdempotencyKeyEveryInputParticipates(t *testing.T) {
	t.Parallel()

	base := domain.IdempotencyKey(idemOrg, domain.SubjectAlertGroup, idemSubject, domain.ReasonAllResolved, 7)
	require.True(t, domain.ValidIdempotencyKey(base))

	assert.NotEqual(t, base, domain.IdempotencyKey(idemOther, domain.SubjectAlertGroup, idemSubject, domain.ReasonAllResolved, 7))
	assert.NotEqual(t, base, domain.IdempotencyKey(idemOrg, "occurrence", idemSubject, domain.ReasonAllResolved, 7))
	assert.NotEqual(t, base, domain.IdempotencyKey(idemOrg, domain.SubjectAlertGroup, idemOther, domain.ReasonAllResolved, 7))
	assert.NotEqual(t, base, domain.IdempotencyKey(idemOrg, domain.SubjectAlertGroup, idemSubject, domain.ReasonFired, 7))
	assert.NotEqual(t, base, domain.IdempotencyKey(idemOrg, domain.SubjectAlertGroup, idemSubject, domain.ReasonAllResolved, 8))
}
