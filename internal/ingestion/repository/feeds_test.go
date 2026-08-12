package repository_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/ingestion/decode"
	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/ingestion/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

// The read half of `ingest_rejections` and `ingest_batches` — the per-source
// rejection feed and the failed-batch list.
//
// ⭐ WHAT THESE TESTS ARE ABOUT. `ingest_rejections` has recorded every refused
// alert since 00006 and nothing could read one back, so "my alert never appeared"
// was answerable only with `psql`. The assertions below are the properties that
// make the answer trustworthy: it belongs to the asking tenant, it is about the
// source the operator is looking at, it is complete across page boundaries, and
// it NAMES THE ALERT rather than just counting a reason.
//
// ⚠️ WHY `received_at` IS NOT `h.Now()`. Both tables are PARTITION BY RANGE on
// `received_at` with daily partitions and NO default partition, and the harness
// template runs `oto_partitions_manage()` at REAL wall-clock time — today plus
// the next seven days. The harness FakeClock is pinned at a fixed Epoch that is
// nowhere near today, so a row written at `h.Now()` has no partition to go in and
// fails with 23514 before any of this is exercised. `harness_test.go` seeds
// `alert_events` with `now()` for exactly this reason. Every timestamp here
// therefore hangs off today's noon UTC, which is always inside today's partition
// whatever hour the suite runs at, and every offset is minutes so nothing
// straddles midnight.

// feedDay is noon UTC today: an instant guaranteed to be inside a partition that
// exists, with a full twelve hours of headroom on either side.
func feedDay() time.Time {
	n := time.Now().UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 12, 0, 0, 0, time.UTC)
}

// alertRaw renders the `raw` column exactly the way the write path does: a
// marshalled `decode.Alert`, whose `labels` key is what the feed lifts out.
func alertRaw(t *testing.T, labels map[string]string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(decode.Alert{
		Status:      decode.StatusFiring,
		Labels:      labels,
		Annotations: map[string]string{"summary": "an alert that was refused"},
		StartsAt:    feedDay(),
	})
	require.NoError(t, err)
	return b
}

// seedRejection writes one rejection through the REAL write path, so that what
// the feed reads back is what ingestion actually stores.
func seedRejection(
	h *harness.H, repo *repository.RejectionRepository, scope db.TenantScope,
	sourceID uuid.UUID, at time.Time, reason domain.Reason, detail string, raw json.RawMessage,
) uuid.UUID {
	t := h.T
	t.Helper()
	rid := id.New()
	batchID := id.New()
	require.NoError(t, repo.Record(h.Ctx, scope, domain.Rejection{
		ID:         rid,
		OrgID:      scope.OrgID(),
		SourceID:   sourceID,
		BatchID:    &batchID,
		ReceivedAt: at,
		Reason:     reason,
		Detail:     detail,
		Raw:        raw,
	}))
	return rid
}

// TestTheRejectionFeedAnswersOnlyItsOwnTenant is the leak assertion.
//
// `ingest_rejections` carries a redacted copy of another customer's alert
// payload. It is the last table in the schema that may answer a question it was
// not asked, so the org predicate is pinned rather than reasoned about.
func TestTheRejectionFeedAnswersOnlyItsOwnTenant(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewRejectionRepository(h.Pool)

	victim := h.Org()
	victimSrc := h.Source(victim, h.Cluster(victim))
	seedRejection(h, repo, victim.Scope, victimSrc.ID, feedDay(),
		domain.ReasonTooManyLabels, "101 labels", alertRaw(t, map[string]string{"alertname": "TheirAlert"}))

	// The fixture works when asked by the tenant that owns it. Asserted first so
	// the emptiness below cannot pass because nothing was ever written.
	own, _, err := repo.List(h.Ctx, victim.Scope, domain.RejectionFilter{SourceID: victimSrc.ID}, db.Keyset{})
	require.NoError(t, err)
	require.Len(t, own, 1, "the owning tenant must see its own rejection")

	snooper := h.Org()
	got, cur, err := repo.List(h.Ctx, snooper.Scope, domain.RejectionFilter{SourceID: victimSrc.ID}, db.Keyset{})
	require.NoError(t, err,
		"another tenant's source id is a not-found, never an error that confirms the source exists")
	require.Empty(t, got,
		"one org read another org's rejection feed: a redacted copy of their alert payload crossed a tenant boundary")
	require.False(t, cur.HasMore)
}

// TestTheRejectionFeedIsAboutOneSource pins the second half of the index
// predicate. A source is the unit an operator debugs, and two Alertmanagers in
// one org are two different questions.
func TestTheRejectionFeedIsAboutOneSource(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewRejectionRepository(h.Pool)

	org := h.Org()
	cl := h.Cluster(org)
	wanted, other := h.Source(org, cl), h.Source(org, cl)

	mine := seedRejection(h, repo, org.Scope, wanted.ID, feedDay(),
		domain.ReasonMissingAlertname, "no alertname", alertRaw(t, map[string]string{"job": "mine"}))
	seedRejection(h, repo, org.Scope, other.ID, feedDay().Add(time.Minute),
		domain.ReasonMissingAlertname, "no alertname", alertRaw(t, map[string]string{"job": "theirs"}))

	got, _, err := repo.List(h.Ctx, org.Scope, domain.RejectionFilter{SourceID: wanted.ID}, db.Keyset{})
	require.NoError(t, err)
	require.Len(t, got, 1, "the other source's rejection leaked into this source's feed")
	require.Equal(t, mine, got[0].ID)
}

// TestTheRejectionFeedNamesTheAlertItRefused is the whole point of the feature.
//
// `oto_ingest_rejected_total{reason}` already says that SOMETHING was refused.
// Only the label set says WHICH ALERT, which is the question the operator
// actually has. The second half pins the other three writers of `raw`, which
// carry no alert at all — a body oto could not decode has no labels to show, and
// answering with an empty set is honest where inventing one would not be.
func TestTheRejectionFeedNamesTheAlertItRefused(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewRejectionRepository(h.Pool)

	org := h.Org()
	src := h.Source(org, h.Cluster(org))

	labels := map[string]string{
		"alertname": "DiskWillFill",
		"namespace": "payments",
		// Redaction runs BEFORE the insert, so a matched value is this literal on
		// disk. The feed reads back what was stored and has no way to undo it.
		"api_key": "[redacted]",
	}
	seedRejection(h, repo, org.Scope, src.ID, feedDay(),
		domain.ReasonLabelValueTooLarge, "label `detail` exceeded 4096 bytes", alertRaw(t, labels))

	// A whole-body rejection: `raw` is the undecodable-path object, which has no
	// `labels` key at all.
	bodyRaw, err := json.Marshal(map[string]any{
		"reason": domain.ReasonUndecodable.String(), "detail": "not an envelope",
		"body_sample": "<html>", "body_bytes": 6,
	})
	require.NoError(t, err)
	seedRejection(h, repo, org.Scope, src.ID, feedDay().Add(-time.Minute),
		domain.ReasonUndecodable, "not an envelope", bodyRaw)

	got, _, err := repo.List(h.Ctx, org.Scope, domain.RejectionFilter{SourceID: src.ID}, db.Keyset{})
	require.NoError(t, err)
	require.Len(t, got, 2)

	require.Equal(t, labels, got[0].Labels,
		"the rejected label set did not survive the round trip: the feed can say a reason and not which alert")
	require.Equal(t, domain.ReasonLabelValueTooLarge, got[0].Reason)
	require.Equal(t, "label `detail` exceeded 4096 bytes", got[0].Detail)
	require.NotNil(t, got[0].BatchID, "a per-alert rejection knows the batch it came from")

	require.NotNil(t, got[1].Labels, "an undecodable body has an EMPTY label set, never a nil one")
	require.Empty(t, got[1].Labels,
		"a body that never became an alert has no labels to name; reason and detail carry that story")
	require.Equal(t, "not an envelope", got[1].Detail)
}

// TestTheRejectionFeedFiltersByReason covers the closed enum as a filter.
func TestTheRejectionFeedFiltersByReason(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewRejectionRepository(h.Pool)

	org := h.Org()
	src := h.Source(org, h.Cluster(org))

	base := feedDay()
	tooMany := seedRejection(h, repo, org.Scope, src.ID, base,
		domain.ReasonTooManyLabels, "101 labels", alertRaw(t, map[string]string{"alertname": "A"}))
	badName := seedRejection(h, repo, org.Scope, src.ID, base.Add(-time.Minute),
		domain.ReasonInvalidLabelName, "`my-label` is not a valid name", alertRaw(t, map[string]string{"alertname": "B"}))
	seedRejection(h, repo, org.Scope, src.ID, base.Add(-2*time.Minute),
		domain.ReasonMissingAlertname, "no alertname", alertRaw(t, map[string]string{"job": "C"}))

	one, _, err := repo.List(h.Ctx, org.Scope,
		domain.RejectionFilter{SourceID: src.ID, Reasons: []domain.Reason{domain.ReasonTooManyLabels}}, db.Keyset{})
	require.NoError(t, err)
	require.Len(t, one, 1)
	require.Equal(t, tooMany, one[0].ID)

	// Two reasons are an OR, not an AND: a chip set of two must widen the feed.
	two, _, err := repo.List(h.Ctx, org.Scope, domain.RejectionFilter{
		SourceID: src.ID,
		Reasons:  []domain.Reason{domain.ReasonTooManyLabels, domain.ReasonInvalidLabelName},
	}, db.Keyset{})
	require.NoError(t, err)
	require.Len(t, two, 2)
	require.Equal(t, []uuid.UUID{tooMany, badName}, []uuid.UUID{two[0].ID, two[1].ID})

	// No reasons is EVERY reason. An operator who does not yet know which bound
	// they hit must not be shown a subset.
	all, _, err := repo.List(h.Ctx, org.Scope, domain.RejectionFilter{SourceID: src.ID}, db.Keyset{})
	require.NoError(t, err)
	require.Len(t, all, 3)
}

// TestTheRejectionFeedPagesNewestFirstWithoutLosingARow is the keyset property.
//
// There is no OFFSET in this codebase, and the reason is exactly this table: a
// feed that is being appended to while an operator reads it would silently skip
// or repeat rows under an offset scan. The assertion is that walking the pages
// reconstructs the whole set, in order, with nothing seen twice.
func TestTheRejectionFeedPagesNewestFirstWithoutLosingARow(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewRejectionRepository(h.Pool)

	org := h.Org()
	src := h.Source(org, h.Cluster(org))

	// Five rows over a page size of two, so the walk crosses two boundaries and
	// ends on a short page.
	const total = 5
	want := make([]uuid.UUID, 0, total)
	for i := range total {
		// Newest first: row 0 is the most recent, so `want` is already in the order
		// the feed must produce.
		at := feedDay().Add(-time.Duration(i) * time.Minute)
		want = append(want, seedRejection(h, repo, org.Scope, src.ID, at,
			domain.ReasonTooManyLabels, "101 labels", alertRaw(t, map[string]string{"alertname": "A"})))
	}

	var (
		seen   []uuid.UUID
		cursor db.Cursor
	)
	for pages := 0; ; pages++ {
		require.Less(t, pages, total+2, "the feed never stopped paging")

		page, next, err := repo.List(h.Ctx, org.Scope,
			domain.RejectionFilter{SourceID: src.ID}, db.Keyset{Limit: 2, Cursor: cursor})
		require.NoError(t, err)
		for _, e := range page {
			seen = append(seen, e.ID)
		}
		if !next.HasMore {
			require.LessOrEqual(t, len(page), 2)
			break
		}
		require.Len(t, page, 2, "a page that reports more must have been full")
		cursor = next
	}

	require.Equal(t, want, seen,
		"paging the rejection feed did not reconstruct it exactly: a keyset that skips or repeats "+
			"is how an operator concludes their alert was never rejected")
}

// TestARejectionFeedWithNothingInItIsAnAnswer. A source that has refused nothing
// returns an empty page and no cursor — not an error, and not a 404. "Nothing was
// rejected" is a real answer to "why did my alert not appear", and it points the
// operator at the next question instead of at a stack trace.
func TestARejectionFeedWithNothingInItIsAnAnswer(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewRejectionRepository(h.Pool)

	org := h.Org()
	src := h.Source(org, h.Cluster(org))

	got, cur, err := repo.List(h.Ctx, org.Scope, domain.RejectionFilter{SourceID: src.ID}, db.Keyset{})
	require.NoError(t, err)
	require.Empty(t, got)
	require.False(t, cur.HasMore)
}

// ---------------------------------------------------------- failed batches

// seedBatch writes a batch through the real insert path and closes it out in the
// given status.
func seedBatch(
	h *harness.H, repo *repository.BatchRepository, scope db.TenantScope,
	sourceID uuid.UUID, at time.Time, status domain.Status, failure string,
) uuid.UUID {
	t := h.T
	t.Helper()

	batchID := id.New()
	sum := sha256.Sum256([]byte(batchID.String()))
	_, err := repo.Insert(h.Ctx, scope, domain.NewBatchParams{
		ID:         batchID,
		SourceID:   sourceID,
		Mode:       domain.ModePush,
		ReceivedAt: at,
		BodyBytes:  128,
		Checksum:   sum[:],
		// ingest_batches_dedup_ck is `^[0-9a-f]{64}$`, which is exactly a sha256 in
		// hex — the same shape the C.5 dedup key really has.
		DedupKey:   hex.EncodeToString(sum[:]),
		AlertCount: 3,
		Payload:    json.RawMessage(`{"alerts":[]}`),
	})
	require.NoError(t, err)

	if status != domain.StatusPending {
		// ingest_batches_procts_ck: processed_at must not precede received_at.
		require.NoError(t, repo.MarkProcessed(h.Ctx, scope, batchID, at, status,
			at.Add(time.Second), failure))
	}
	return batchID
}

// TestAFailedBatchIsListableWithTheReasonItStopped.
//
// A `failed` or `partial` batch is a 202 that never became an alert: the payload
// is on disk, the operator was told oto had it, and nothing arrived. Until this
// list existed, `error` was written on every failure and read by nobody.
func TestAFailedBatchIsListableWithTheReasonItStopped(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewBatchRepository(h.Pool)

	org := h.Org()
	src := h.Source(org, h.Cluster(org))
	base := feedDay()

	failed := seedBatch(h, repo, org.Scope, src.ID, base, domain.StatusFailed,
		"the alerts module refused the observation batch")
	partial := seedBatch(h, repo, org.Scope, src.ID, base.Add(-time.Minute), domain.StatusPartial, "")
	// Neither of these is a failure, and neither may appear: `processed` worked,
	// and `pending` is the state every accepted batch passes through.
	seedBatch(h, repo, org.Scope, src.ID, base.Add(-2*time.Minute), domain.StatusProcessed, "")
	seedBatch(h, repo, org.Scope, src.ID, base.Add(-3*time.Minute), domain.StatusPending, "")

	got, _, err := repo.ListFailed(h.Ctx, org.Scope, domain.BatchFailureFilter{SourceID: src.ID}, db.Keyset{})
	require.NoError(t, err)
	require.Len(t, got, 2, "the feed must list failed and partial, and only those")

	require.Equal(t, failed, got[0].ID, "newest first")
	require.Equal(t, domain.StatusFailed, got[0].Status)
	require.Equal(t, "the alerts module refused the observation batch", got[0].Error,
		"a failed batch without its reason is a row that says nothing an operator can act on")
	require.NotNil(t, got[0].ProcessedAt, "ingest_batches_proc_ck ties every terminal status to a timestamp")
	require.Equal(t, 3, got[0].AlertCount, "the count is how much this failure cost")
	require.Equal(t, domain.ModePush, got[0].Mode)

	require.Equal(t, partial, got[1].ID)
	require.Equal(t, domain.StatusPartial, got[1].Status)

	// Narrowing to one status is what lets the screen separate "gave up" from
	// "stopped halfway".
	onlyFailed, _, err := repo.ListFailed(h.Ctx, org.Scope, domain.BatchFailureFilter{
		SourceID: src.ID, Statuses: []domain.Status{domain.StatusFailed},
	}, db.Keyset{})
	require.NoError(t, err)
	require.Len(t, onlyFailed, 1)
	require.Equal(t, failed, onlyFailed[0].ID)
}

// TestTheFailedBatchFeedIsScopedAndPaged carries the same two properties as the
// rejection feed, because it is the same screen answering the same question one
// level up.
func TestTheFailedBatchFeedIsScopedAndPaged(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewBatchRepository(h.Pool)

	victim := h.Org()
	victimSrc := h.Source(victim, h.Cluster(victim))

	const total = 3
	want := make([]uuid.UUID, 0, total)
	for i := range total {
		want = append(want, seedBatch(h, repo, victim.Scope, victimSrc.ID,
			feedDay().Add(-time.Duration(i)*time.Minute), domain.StatusFailed, "gave up"))
	}

	snooper := h.Org()
	leaked, _, err := repo.ListFailed(h.Ctx, snooper.Scope,
		domain.BatchFailureFilter{SourceID: victimSrc.ID}, db.Keyset{})
	require.NoError(t, err)
	require.Empty(t, leaked, "one org listed another org's failed batches")

	var (
		seen   []uuid.UUID
		cursor db.Cursor
	)
	for pages := 0; ; pages++ {
		require.Less(t, pages, total+2, "the feed never stopped paging")

		page, next, err := repo.ListFailed(h.Ctx, victim.Scope,
			domain.BatchFailureFilter{SourceID: victimSrc.ID}, db.Keyset{Limit: 2, Cursor: cursor})
		require.NoError(t, err)
		for _, b := range page {
			seen = append(seen, b.ID)
		}
		if !next.HasMore {
			break
		}
		cursor = next
	}
	require.Equal(t, want, seen, "paging the failed-batch feed did not reconstruct it exactly")
}
