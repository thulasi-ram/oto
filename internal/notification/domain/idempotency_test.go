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
	want = append(want, 0x00, 0x00, 0x00, byte(len(domain.SubjectCase)))
	want = append(want, domain.SubjectCase...)
	want = append(want, 0x00, 0x00, 0x00, 0x10) // len(subject_id_bytes) == 16
	want = append(want, idemSubject[:]...)
	want = append(want, 0x00, 0x00, 0x00, byte(len(domain.ReasonAllResolved)))
	want = append(want, domain.ReasonAllResolved...)
	want = append(want, "7"...) // itoa(state_version), raw
	sum := sha256.Sum256(want)

	assert.Equal(t, hex.EncodeToString(sum[:]),
		domain.IdempotencyKey(idemOrg, domain.SubjectCase, idemSubject, domain.ReasonAllResolved, 7, uuid.Nil),
		"the §C.7 pre-image is uint32be(len(x))||x per field, with itoa(state_version) raw")

	// ⭐ AND THE OCCASION IS APPENDED AFTER THAT RAW TAIL, as ONE framed 16-byte
	// field, ONLY when it is non-nil. The nil case above is the inertness proof —
	// the same bytes §C.7 has always hashed — and this is the shape of the other.
	occasion := uuid.MustParse("018f3a4b-0000-7000-8000-00000000ab01")
	want = append(want, 0x00, 0x00, 0x00, 0x10) // len(occasion_id_bytes) == 16
	want = append(want, occasion[:]...)
	occSum := sha256.Sum256(want)

	assert.Equal(t, hex.EncodeToString(occSum[:]),
		domain.IdempotencyKey(idemOrg, domain.SubjectCase, idemSubject, domain.ReasonAllResolved, 7, occasion),
		"the occasion is field(uuid) after the tail, and its leading 0x00 is what keeps the split decodable")
}

// TestIdempotencyKeyAgreesWithTheKernel was written because §C.7 had TWO
// implementations, agreeing by luck, and the live one was NOT the kernel's. There
// is one now: this package keeps the closed enums (alerts/domain may import no
// other domain package, so SubjectKind and Reason cannot move there) and the
// kernel keeps the bytes.
//
// The test survives the collapse deliberately. It reads as a tautology and is not
// one in the way that matters: the day somebody re-inlines sha256 here rather than
// import the kernel — which is how the pair arose the first time — this goes red,
// and the UNIQUE (org_id, idempotency_key) index keeps meaning what it says.
func TestIdempotencyKeyAgreesWithTheKernel(t *testing.T) {
	t.Parallel()

	for _, kind := range []domain.SubjectKind{domain.SubjectCase, domain.SubjectAlert, domain.SubjectDigest, ""} {
		for _, reason := range []domain.Reason{
			domain.ReasonAllResolved, domain.ReasonFired, "", "a", "ab",
		} {
			for _, version := range []int{0, 1, 7, 12, -1, 1 << 40} {
				for _, subject := range []uuid.UUID{idemSubject, idemOther} {
					// Both occasion arms, because the adapter now has one more argument
					// to pass through and passing `uuid.Nil` for everything would leave
					// the conditional field untested on this side of the seam.
					for _, occasion := range []uuid.UUID{uuid.Nil, idemOther} {
						got := domain.IdempotencyKey(idemOrg, kind, subject, reason, version, occasion)
						want := alerts.ComputeIdempotencyKey(
							idemOrg, string(kind), subject, string(reason), version, occasion).String()
						require.Equal(t, want, got,
							"§C.7 must have one value: kind=%q reason=%q v=%d occasion=%s",
							kind, reason, version, occasion)
					}
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

	kinds := []domain.SubjectKind{"", "a", "ab", "alert_group", "a\x00b", "case"}
	reasons := []domain.Reason{"", "a", "ab", "b", "1", "12", "a\x00b", "all_resolved"}
	versions := []int{0, 1, 2, 12, 120, 7}
	// uuid.Nil is in the corpus on purpose: it is the ABSENT occasion, and it must
	// not collide with any present one — including the neighbouring versions, which
	// is where a framing that ran the digits into the appended field would fail.
	occasions := []uuid.UUID{uuid.Nil, idemSubject, idemOther}

	seen := map[string]string{}
	for _, kind := range kinds {
		for _, reason := range reasons {
			for _, version := range versions {
				for _, occasion := range occasions {
					id := strings.Join([]string{
						strconv.Quote(string(kind)),
						strconv.Quote(string(reason)),
						strconv.Itoa(version),
						occasion.String(),
					}, "|")
					key := domain.IdempotencyKey(idemOrg, kind, idemSubject, reason, version, occasion)
					if prev, dup := seen[key]; dup && prev != id {
						t.Fatalf("idempotency_key collision:\n  %s\n  %s\nboth key to %s", prev, id, key)
					}
					seen[key] = id
				}
			}
		}
	}
	assert.Len(t, seen, len(kinds)*len(reasons)*len(versions)*len(occasions),
		"one key per (subject_kind, reason, state_version, occasion)")
}

// TestIdempotencyKeyEveryInputParticipates keeps the key's inputs honest: dropping
// any one of them would make two genuinely different notifications collapse into
// one that can never be re-sent.
func TestIdempotencyKeyEveryInputParticipates(t *testing.T) {
	t.Parallel()

	base := domain.IdempotencyKey(idemOrg, domain.SubjectCase, idemSubject, domain.ReasonAllResolved, 7, uuid.Nil)
	require.True(t, domain.ValidIdempotencyKey(base))

	assert.NotEqual(t, base, domain.IdempotencyKey(idemOther, domain.SubjectCase, idemSubject, domain.ReasonAllResolved, 7, uuid.Nil))
	assert.NotEqual(t, base, domain.IdempotencyKey(idemOrg, domain.SubjectAlert, idemSubject, domain.ReasonAllResolved, 7, uuid.Nil))
	assert.NotEqual(t, base, domain.IdempotencyKey(idemOrg, domain.SubjectCase, idemOther, domain.ReasonAllResolved, 7, uuid.Nil))
	assert.NotEqual(t, base, domain.IdempotencyKey(idemOrg, domain.SubjectCase, idemSubject, domain.ReasonFired, 7, uuid.Nil))
	assert.NotEqual(t, base, domain.IdempotencyKey(idemOrg, domain.SubjectCase, idemSubject, domain.ReasonAllResolved, 8, uuid.Nil))
	assert.NotEqual(t, base, domain.IdempotencyKey(idemOrg, domain.SubjectCase, idemSubject, domain.ReasonAllResolved, 7, idemOther),
		"the occasion participates when it is named")
}

// TestOnlyTheSnoozeReasonsNeedAnOccasion pins the closed list, because it is the
// answer to "which Reasons is `state_version` not a discriminator for" and getting
// it wrong is silent in both directions: a Reason wrongly on the list would be
// re-keyed for nothing, and one wrongly off it goes back to swallowing its own
// second announcement.
//
// `snoozed` and `unsnoozed` are the two, and they are the SAME two §B.8.4 exempts
// from snooze suppression — because both facts follow from the one structural
// truth that a snooze is taken on an Alert and moves no Case lock.
func TestOnlyTheSnoozeReasonsNeedAnOccasion(t *testing.T) {
	t.Parallel()

	var need []domain.Reason
	for _, r := range domain.AllReasons() {
		if r.NeedsOccasion() {
			need = append(need, r)
		}
	}
	assert.Equal(t, []domain.Reason{domain.ReasonSnoozed, domain.ReasonUnsnoozed}, need)
	for _, r := range need {
		assert.True(t, r.SnoozeExempt(),
			"a reason that needs an occasion is a reason about snoozing, so it is also exempt from snooze suppression")
	}
}
