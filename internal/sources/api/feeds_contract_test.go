package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// THE TWO FEEDS THAT ANSWER "MY ALERT NEVER APPEARED", ASSERTED AGAINST THE
// CONTRACT RATHER THAN AGAINST A SECOND COPY OF IT.
//
// The bug these endpoints close was not a wrong answer, it was NO answer: every
// per-alert bound failure and every terminal batch failure has been durably
// recorded since the first migration, and the only way to read one back was
// `psql`. So the assertions below care most about the two ways a feed can lie
// while returning 200 —
//
//   - an empty page that means "nothing was rejected" when it really means
//     "that source is not yours", and
//   - a cursor honoured across a filter change, which pages through the wrong
//     list while looking perfectly well-formed.

/* -------------------------------------------------------------------------- */
/* Fixtures                                                                   */
/* -------------------------------------------------------------------------- */

var contractBatchID = uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b79")

// contractRejections is a page with BOTH shapes of rejection on it, because the
// two are the whole subtlety of the payload:
//
//   - one refused ALERT, which has a batch and a label set naming it;
//   - one refused BODY, which has neither, because the bytes never became an
//     alert to point at.
//
// A fixture with only the first would let a handler that dropped `labels` or
// nil-mapped it pass, and `{}` versus `null` is the difference between "there is
// no alert here" and "we do not know".
func contractRejections() []RejectionEntry {
	return []RejectionEntry{
		{
			ID:         uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b80"),
			SourceID:   contractSourceID,
			BatchID:    &contractBatchID,
			ReceivedAt: contractStamp.Add(-2 * time.Minute),
			Reason:     "label_value_too_large",
			Detail:     "label value for `password` is 5121 bytes, over the 4096 cap",
			// Already redacted on disk, which is why the value reads like this and
			// not like a secret.
			Labels: map[string]string{
				"alertname": "HighErrorRate",
				"cluster":   "prod-eu",
				"password":  "[redacted]",
			},
		},
		{
			ID:         uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b81"),
			SourceID:   contractSourceID,
			BatchID:    nil,
			ReceivedAt: contractStamp.Add(-9 * time.Minute),
			Reason:     "undecodable",
			Detail:     "body is not an Alertmanager webhook envelope",
			Labels:     nil,
		},
	}
}

// contractFailedBatches carries one of each troubled status, for the same
// reason: `failed` decided to stop and has an error, `partial` stopped by dying
// and usually has none.
func contractFailedBatches() []BatchFailure {
	stopped := contractStamp.Add(-time.Minute)
	return []BatchFailure{
		{
			ID:              uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b82"),
			SourceID:        contractSourceID,
			Mode:            "push",
			ReceivedAt:      contractStamp.Add(-3 * time.Minute),
			Status:          "failed",
			ProcessedAt:     &stopped,
			Error:           "alert upsert failed: context deadline exceeded",
			AlertCount:      37,
			TruncatedAlerts: 0,
		},
		{
			ID:              uuid.MustParse("0198f3c1-6a2e-7c31-9b4d-2f5a1c8e0b83"),
			SourceID:        contractSourceID,
			Mode:            "push",
			ReceivedAt:      contractStamp.Add(-30 * time.Minute),
			Status:          "partial",
			ProcessedAt:     &stopped,
			Error:           "",
			AlertCount:      2400,
			TruncatedAlerts: 400,
		},
	}
}

/* -------------------------------------------------------------------------- */
/* Fake                                                                       */
/* -------------------------------------------------------------------------- */

// contractFeeds is an IngestFeeds that records what the handler asked it for.
//
// It deliberately does NOT scope by source id: the point of the tenant probe
// below is that the HANDLER refuses a stranger before the feed is ever reached,
// and a fake that filtered would prove that fact about itself instead.
type contractFeeds struct {
	rejections []RejectionEntry
	batches    []BatchFailure

	gotReasons  []string
	gotStatuses []string
	gotSource   uuid.UUID
	calls       int
}

func newContractFeeds() *contractFeeds {
	return &contractFeeds{rejections: contractRejections(), batches: contractFailedBatches()}
}

func (f *contractFeeds) ListRejections(
	_ context.Context, _ db.TenantScope, sourceID uuid.UUID, reasons []string, _ db.Keyset,
) ([]RejectionEntry, db.Cursor, error) {
	f.calls++
	f.gotSource, f.gotReasons = sourceID, reasons
	return f.rejections, db.Cursor{}, nil
}

func (f *contractFeeds) ListFailedBatches(
	_ context.Context, _ db.TenantScope, sourceID uuid.UUID, statuses []string, _ db.Keyset,
) ([]BatchFailure, db.Cursor, error) {
	f.calls++
	f.gotSource, f.gotStatuses = sourceID, statuses
	return f.batches, db.Cursor{}, nil
}

/* -------------------------------------------------------------------------- */
/* 1. The happy page                                                          */
/* -------------------------------------------------------------------------- */

// TestTheRejectionFeedAnswersTheShapeTheContractDeclares.
func TestTheRejectionFeedAnswersTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	s := newContractStack()
	resp := s.client().GET(sourcePath(contractSourceID, "/rejections")).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "listSourceRejections", http.StatusOK, resp.Body())

	body := resp.JSON(t)
	rows, ok := body["data"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("data = %#v, want the two rejections the feed owns", body["data"])
	}

	// ⭐ THE LABEL SET IS THE ANSWER. Without it the feed says an alert was
	// refused and cannot say WHICH, which is the whole question.
	named, _ := rows[0].(map[string]any)
	labels, ok := named["labels"].(map[string]any)
	if !ok || labels["alertname"] != "HighErrorRate" {
		t.Fatalf("labels = %#v, want the label set that names the refused alert", named["labels"])
	}
	if labels["password"] != "[redacted]" {
		t.Fatalf("password = %v, want the stored `[redacted]`: this feed reads back what was "+
			"written and must never have a way to un-redact it", labels["password"])
	}

	// ⛔ AND THE EVIDENCE DOCUMENT IS NOT ON THE WIRE. `raw` is up to a batch's
	// worth of body per row.
	if _, leaked := named["raw"]; leaked {
		t.Fatal("the row carries `raw`; a page of fifty would ship a batch of evidence to render a table")
	}

	// The rejection with no alert to name: an empty set, never a null.
	anonymous, _ := rows[1].(map[string]any)
	empty, ok := anonymous["labels"].(map[string]any)
	if !ok || len(empty) != 0 {
		t.Fatalf("labels = %#v, want `{}`: a body that never became an alert has no label set, "+
			"and `null` would read as \"we do not know\"", anonymous["labels"])
	}
	if anonymous["batch_id"] != nil {
		t.Fatalf("batch_id = %v, want null: an undecodable body has no batch row", anonymous["batch_id"])
	}
}

// TestTheFailedBatchFeedAnswersTheShapeTheContractDeclares.
func TestTheFailedBatchFeedAnswersTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	s := newContractStack()
	resp := s.client().GET(sourcePath(contractSourceID, "/failed-batches")).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "listSourceFailedBatches", http.StatusOK, resp.Body())

	body := resp.JSON(t)
	rows, ok := body["data"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("data = %#v, want both troubled batches", body["data"])
	}

	failed, _ := rows[0].(map[string]any)
	if failed["error"] == "" {
		t.Fatal("a `failed` batch came back with no error; the reason it stopped is the whole row")
	}
	// The number that says what the failure cost.
	if failed["alert_count"] != float64(37) {
		t.Fatalf("alert_count = %v, want 37 alerts sitting unprocessed", failed["alert_count"])
	}
	// ⛔ The 8 MiB column is not on the list.
	if _, leaked := failed["payload"]; leaked {
		t.Fatal("the row carries `payload`; a page of fifty is four hundred megabytes")
	}

	partial, _ := rows[1].(map[string]any)
	if partial["status"] != "partial" {
		t.Fatalf("status = %v, want the resumable half of the feed too", partial["status"])
	}
}

/* -------------------------------------------------------------------------- */
/* 2. The filters                                                             */
/* -------------------------------------------------------------------------- */

// TestTheRejectionFeedFiltersByReason.
//
// The filter is what turns "something was refused" into "THIS bound was hit",
// and it has to reach the feed rather than being parsed and dropped.
func TestTheRejectionFeedFiltersByReason(t *testing.T) {
	t.Parallel()

	s := newContractStack()
	s.client().GET(sourcePath(contractSourceID, "/rejections")+"?reason=undecodable,body_too_large").
		MustStatus(t, http.StatusOK)

	if got := s.feeds.gotReasons; len(got) != 2 || got[0] != "undecodable" || got[1] != "body_too_large" {
		t.Fatalf("the feed was asked for %v, want both reasons the caller named", got)
	}
	if s.feeds.gotSource != contractSourceID {
		t.Fatalf("the feed was asked about %v, want the source in the path", s.feeds.gotSource)
	}
}

// TestTheFailedBatchFeedFiltersByStatus.
func TestTheFailedBatchFeedFiltersByStatus(t *testing.T) {
	t.Parallel()

	s := newContractStack()
	s.client().GET(sourcePath(contractSourceID, "/failed-batches")+"?status=partial").
		MustStatus(t, http.StatusOK)

	if got := s.feeds.gotStatuses; len(got) != 1 || got[0] != "partial" {
		t.Fatalf("the feed was asked for %v, want just `partial`", got)
	}
}

// ⛔ TestAnUnknownReasonIsRefusedRatherThanIgnored.
//
// `reason` is a closed enum, so a member nobody minted matches no row. Serving
// `?reason=too_many_lables` would answer 200 with an empty page — which on THIS
// screen reads as "nothing was rejected", the one answer that must never be
// wrong. §E.3 says the same about an unknown parameter name.
func TestAnUnknownReasonIsRefusedRatherThanIgnored(t *testing.T) {
	t.Parallel()

	s := newContractStack()
	resp := s.client().GET(sourcePath(contractSourceID, "/rejections")+"?reason=too_many_lables").
		MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "listSourceRejections", http.StatusUnprocessableEntity, resp.Body())
	resp.MustViolate(t, "reason")

	if s.feeds.calls != 0 {
		t.Fatal("a refused filter still reached the feed")
	}
}

/* -------------------------------------------------------------------------- */
/* 3. Tenant scoping                                                          */
/* -------------------------------------------------------------------------- */

// ⛔ TestAnotherTenantsSourceHasNoFeedAtAll.
//
// This is the highest-value assertion in the file and the one a list endpoint
// most easily gets wrong. Both feeds are org-scoped in SQL, so a stranger's id
// would come back as an EMPTY PAGE with a 200 — no leak, and a perfect lie: the
// screen would report "no rejections" about a source the caller may not see.
// 404 is the only true statement oto can make about an id it does not own, and
// it must be reached before the feed is queried at all.
func TestAnotherTenantsSourceHasNoFeedAtAll(t *testing.T) {
	t.Parallel()

	stranger := apitest.StrangerID
	for _, tc := range []struct {
		op     string
		suffix string
	}{
		{"listSourceRejections", "/rejections"},
		{"listSourceFailedBatches", "/failed-batches"},
	} {
		t.Run(tc.op, func(t *testing.T) {
			t.Parallel()

			s := newContractStack()
			resp := s.client().GET(sourcePath(stranger, tc.suffix)).
				MustStatus(t, http.StatusNotFound)
			schema.AssertProblem(t, tc.op, http.StatusNotFound, resp.Body())

			if s.feeds.calls != 0 {
				t.Fatal("a stranger's id still reached the feed; the refusal must come first")
			}
		})
	}
}

/* -------------------------------------------------------------------------- */
/* 4. The cursor                                                              */
/* -------------------------------------------------------------------------- */

// ⛔ TestAFeedCursorIsBoundToItsFilter.
//
// A cursor describes a position in ONE sequence. Replayed against a different
// filter it names a position in a list that no longer exists, and the server
// would serve a page from the middle of the wrong one with nothing looking
// wrong (§E.1). A token that is not a token at all is the same failure with a
// louder symptom.
func TestAFeedCursorIsBoundToItsFilter(t *testing.T) {
	t.Parallel()

	base := sourcePath(contractSourceID, "/rejections")

	t.Run("a cursor that is not a token", func(t *testing.T) {
		t.Parallel()

		s := newContractStack()
		resp := s.client().GET(base+"?cursor=not-a-cursor").
			MustStatus(t, http.StatusBadRequest)
		schema.AssertProblem(t, "listSourceRejections", http.StatusBadRequest, resp.Body())

		if s.feeds.calls != 0 {
			t.Fatal("a malformed cursor still reached the feed")
		}
	})

	t.Run("a cursor minted under another filter", func(t *testing.T) {
		t.Parallel()

		// A cursor minted while `?reason=undecodable` was applied, replayed
		// against the unfiltered feed.
		token := encodedCursorFor(t, contractSourceID, []string{"undecodable"})
		s := newContractStack()
		resp := s.client().GET(base+"?cursor="+token).
			MustStatus(t, http.StatusBadRequest)
		schema.AssertProblem(t, "listSourceRejections", http.StatusBadRequest, resp.Body())

		if s.feeds.calls != 0 {
			t.Fatal("a cursor from another filter still reached the feed")
		}
	})
}

/* -------------------------------------------------------------------------- */
/* 5. The empty case                                                          */
/* -------------------------------------------------------------------------- */

// TestAnEmptyFeedIsAnEmptyPageAndNotAnAbsence.
//
// "Nothing was refused" is a real, common and reassuring answer, and it has to
// be expressible. `data: []` rather than `null` is what lets a client render
// "no rejections" instead of crashing on a nil list — and the envelope must
// still carry `page`, or the client cannot tell an empty list from a truncated
// one.
func TestAnEmptyFeedIsAnEmptyPageAndNotAnAbsence(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		op     string
		suffix string
		empty  func(*contractStack)
	}{
		{"listSourceRejections", "/rejections", func(s *contractStack) { s.feeds.rejections = nil }},
		{"listSourceFailedBatches", "/failed-batches", func(s *contractStack) { s.feeds.batches = nil }},
	} {
		t.Run(tc.op, func(t *testing.T) {
			t.Parallel()

			s := newContractStack()
			tc.empty(s)

			resp := s.client().GET(sourcePath(contractSourceID, tc.suffix)).
				MustStatus(t, http.StatusOK)
			schema.Assert(t, tc.op, http.StatusOK, resp.Body())

			body := resp.JSON(t)
			rows, ok := body["data"].([]any)
			if !ok || len(rows) != 0 {
				t.Fatalf("data = %#v, want an empty array rather than null", body["data"])
			}
			page, ok := body["page"].(map[string]any)
			if !ok {
				t.Fatalf("page = %#v, want the keyset envelope even on an empty page", body["page"])
			}
			if page["has_more"] != false || page["next_cursor"] != nil {
				t.Fatalf("page = %#v, want has_more false and no cursor", page)
			}
		})
	}
}

/* -------------------------------------------------------------------------- */
/* Helpers                                                                    */
/* -------------------------------------------------------------------------- */

// encodedCursorFor mints a cursor bound to the filter named, using the same
// helpers the handler does, so the test never spells the encoding out.
func encodedCursorFor(t *testing.T, sourceID uuid.UUID, reasons []string) string {
	t.Helper()
	hash := httpx.FilterHash(rejectionFilterParts(sourceID, reasons)...)
	return httpx.EncodeCursor(db.Cursor{
		SortKey: contractStamp.Add(-time.Hour),
		ID:      contractBatchID,
		Hash:    hash,
		HasMore: true,
	})
}
