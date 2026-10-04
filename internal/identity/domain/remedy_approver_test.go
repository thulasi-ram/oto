package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// TestARemedyApproverGrantCountsOnlyForAnEnabledUserWithAnAddress — ADR 0054 §4,
// git-bug 47f67c8. A disabled holder's grant stays on the record and stops counting; a
// grant on a shadow member (which only a hand-written row could produce) never counts.
func TestARemedyApproverGrantCountsOnlyForAnEnabledUserWithAnAddress(t *testing.T) {
	email, err := NewEmail("ada@example.test")
	require.NoError(t, err)
	held := RemedyApprover{UserID: uuid.New(), Email: email, DisplayName: "Ada",
		GrantedAt: time.Now(), GrantedBy: RemedyApproverGrantedByCLI}
	require.True(t, held.Counts(), "an enabled holder with an address counts")

	disabled := held
	at := time.Now()
	disabled.DisabledAt = &at
	require.False(t, disabled.Counts(), "a disabled user's grant counted")

	shadow := held
	shadow.Email = Email{}
	require.False(t, shadow.Counts(), "a shadow member's grant counted; a shadow member can never hold one")
}

// TestRemedyApproverRequiredIsATyped403 — the refusal 4148256's approval path returns.
func TestRemedyApproverRequiredIsATyped403(t *testing.T) {
	err := RemedyApproverRequired()
	require.True(t, errs.IsKind(err, errs.KindForbidden))
	require.Equal(t, RemedyApproverRequiredCode, errs.CodeOf(err))
	require.Contains(t, err.Error(), "oto grant remedy-approver")
}
