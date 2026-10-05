package service_test

// git-bug 3e96f5a's "Done when", the send half (ADR 0053 §2, §4): A DIGEST NEVER WAITS ON
// AN INVESTIGATION. It is sent when its window closes, with whatever Finding the window's
// run had reached at that moment, and the built-in body when it had none — still running,
// queued, failed, exhausted-without-a-Finding or skipped all look the same from here: the
// reader says "none". A Finding that arrives after the send is not posted as a correction.
// The investigator half — which runs a digest may carry — is
// `investigator/service/digests_test.go`'s.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
)

// windowFindings is the Finding reader as the window's run stands when it is asked.
type windowFindings struct {
	finding *domain.DigestFinding
	err     error
	asked   []digestSpans
}

func (f *windowFindings) DigestFinding(
	_ context.Context, _ db.TenantScope, _ uuid.UUID, start, end time.Time,
) (domain.DigestFinding, bool, error) {
	f.asked = append(f.asked, digestSpans{start: start, end: end})
	if f.err != nil {
		return domain.DigestFinding{}, false, f.err
	}
	if f.finding == nil {
		return domain.DigestFinding{}, false, nil
	}
	return *f.finding, true, nil
}

// summarisedPolicy is a ten-minute digest policy that asked an Investigator for a summary.
func summarisedPolicy() domain.Policy {
	p := tickPolicy(10*time.Minute, 0)
	p.Digest.InvestigatorID = uuid.New()
	return p
}

func findingRig(t *testing.T, p domain.Policy, findings *windowFindings) digestRig {
	t.Helper()
	reads := &digestReads{
		coveredTo: coveredThrough1330,
		cases:     sameEvery(episodes("payments", newestClosed.Add(time.Minute), 2)...),
	}
	return newDigestRigWith(t, tickNow, p, reads, &digestNotifications{}, findings)
}

func digestView(t *testing.T, n domain.Notification) *service.NotificationView {
	t.Helper()
	views, err := service.NewViewService(service.ViewConfig{
		Snapshots: digestSnapshots{t: t}, Clock: clock.NewFake(tickNow),
	})
	require.NoError(t, err)
	v, err := views.Build(t.Context(), db.TenantScope{}, service.ViewRequest{Notification: n})
	require.NoError(t, err)
	require.NotNil(t, v.Digest)
	return v
}

// TestADigestSentWhileItsInvestigationIsStillRunningCarriesTheBuiltInBody — THE pin.
// The run has not ended when the window closes, so the reader says "none": the digest is
// sent on this tick, at once, with the built-in body, and the reader was asked exactly
// once — no wait, no retry, no second look.
func TestADigestSentWhileItsInvestigationIsStillRunningCarriesTheBuiltInBody(t *testing.T) {
	t.Parallel()
	running := &windowFindings{} // the run is queued or running: nothing to carry yet
	rig := findingRig(t, summarisedPolicy(), running)

	sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)

	assert.Equal(t, 1, sent, "a digest whose Investigation is still running was not sent on time")
	require.Len(t, rig.notifs.inserted, 1)
	assert.Nil(t, rig.notifs.inserted[0].DigestFinding, "a digest carried a Finding nobody had reached")
	assert.NotEmpty(t, rig.deliver.created, "the digest was minted and not fanned out")
	require.Len(t, running.asked, 1, "the Finding was asked for more than once: that is waiting")
	assert.Equal(t, digestSpans{start: newestClosed, end: newestClosedTo}, running.asked[0],
		"the Finding was asked for some other window than the one being sent")

	v := digestView(t, rig.notifs.inserted[0])
	assert.Nil(t, v.Digest.Finding, "the built-in body grew a Finding at render time")
}

// TestADigestCarriesTheFindingThatWasReadyWhenItsWindowClosed — a completed (or exhausted,
// partial) Finding is copied onto the digest and reaches the card from the row alone.
func TestADigestCarriesTheFindingThatWasReadyWhenItsWindowClosed(t *testing.T) {
	t.Parallel()
	ready := &domain.DigestFinding{
		InvestigationID: uuid.New(), Investigator: "digest", Version: 2,
		Summary: "Two crash loops in payments, one deploy.", Classification: "deploy",
		Partial: true, ConcludedAt: newestClosedTo.Add(-time.Minute),
	}
	rig := findingRig(t, summarisedPolicy(), &windowFindings{finding: ready})

	sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)
	require.Equal(t, 1, sent)
	require.Len(t, rig.notifs.inserted, 1)
	require.NotNil(t, rig.notifs.inserted[0].DigestFinding)
	assert.Equal(t, *ready, *rig.notifs.inserted[0].DigestFinding)

	v := digestView(t, rig.notifs.inserted[0])
	require.NotNil(t, v.Digest.Finding)
	assert.Equal(t, ready.Summary, v.Digest.Finding.Summary)
	assert.True(t, v.Digest.Finding.Partial, "a partial Finding lost its mark")
	assert.Equal(t, 2, v.Digest.Count, "the Finding replaced the digest's own count")
}

// TestAFindingThatArrivesAfterTheSendIsNotPostedAsACorrection — the window was sent with
// the built-in body; the run ends afterwards. The next tick owes nothing for that window,
// mints nothing, delivers nothing, and does not even ask.
func TestAFindingThatArrivesAfterTheSendIsNotPostedAsACorrection(t *testing.T) {
	t.Parallel()
	late := &windowFindings{}
	rig := findingRig(t, summarisedPolicy(), late)

	_, err := rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)
	require.Len(t, rig.notifs.inserted, 1)
	deliveries := len(rig.deliver.created)

	// The run ends now, with a Finding — after the send.
	late.finding = &domain.DigestFinding{InvestigationID: uuid.New(), Investigator: "digest", Version: 1,
		Summary: "Too late to be the body.", ConcludedAt: tickNow}
	require.NotEmpty(t, rig.reads.advanced)
	rig.reads.coveredTo = rig.reads.advanced[len(rig.reads.advanced)-1]

	sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)
	assert.Zero(t, sent, "a late Finding sent the window again")
	assert.Len(t, rig.notifs.inserted, 1, "a late Finding minted a correction")
	assert.Len(t, rig.deliver.created, deliveries, "a late Finding was delivered somewhere")
	assert.Nil(t, rig.notifs.inserted[0].DigestFinding, "the sent digest was amended with a late Finding")
	assert.Len(t, late.asked, 1, "the tick came back for the Finding after the send")
}

// TestAFindingThatCannotBeReadIsTheBuiltInBodyNotALateDigest — an error reading the run
// is the built-in body too, said in the log: holding the window for a summary is the
// waiting the rule forbids.
func TestAFindingThatCannotBeReadIsTheBuiltInBodyNotALateDigest(t *testing.T) {
	t.Parallel()
	rig := findingRig(t, summarisedPolicy(), &windowFindings{err: errors.New("investigations unreachable")})

	sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)
	assert.Equal(t, 1, sent)
	require.Len(t, rig.notifs.inserted, 1)
	assert.Nil(t, rig.notifs.inserted[0].DigestFinding)
	assert.True(t, strings.Contains(rig.logs.String(), "sending the built-in body"),
		"an unreadable Finding was swallowed without a word")
}

// TestAPolicyThatAskedForNoSummaryNeverAsks — the opt-in is the policy's.
func TestAPolicyThatAskedForNoSummaryNeverAsks(t *testing.T) {
	t.Parallel()
	never := &windowFindings{finding: &domain.DigestFinding{Summary: "unasked"}}
	rig := findingRig(t, tickPolicy(10*time.Minute, 0), never)

	sent, err := rig.svc.SweepOrg(t.Context(), rig.scope)
	require.NoError(t, err)
	assert.Equal(t, 1, sent)
	assert.Nil(t, rig.notifs.inserted[0].DigestFinding)
	assert.Empty(t, never.asked, "a policy that named no Investigator asked for a Finding")
}
