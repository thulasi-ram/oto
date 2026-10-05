package api

// A POLICY'S DIGEST INVESTIGATIONS, CHECKED AGAINST THE CONTRACT (review D4).
//
//   - the list answers the shape the contract declares, latest first, with a run that
//     never started (`skipped`/`window_closed`) on it — the record this route exists to
//     make readable;
//   - a policy this org does not have, or a path that is not a UUID, is a 404.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/contract/schema"
)

// fxPolicy (suggestions_contract_test.go) is the one notification policy this tenant owns;
// here it names a digest Investigator.

func (f *fakeInvestigators) ListPolicyDigestInvestigations(
	_ context.Context, s db.TenantScope, policyID uuid.UUID, _ db.Keyset,
) ([]domain.Investigation, db.Cursor, error) {
	if !mine(s) || policyID != fxPolicy {
		return nil, db.Cursor{}, errs.NotFound("policy_not_found", "no such notification policy")
	}
	window := domain.DigestWindow{Start: fxEpoch, End: fxEpoch.Add(10 * time.Minute)}
	completed := fxInvestigationValue(domain.StatusExhausted)
	completed.SubjectKind, completed.SubjectID, completed.AlertKey = domain.SubjectDigest, fxPolicy, ""
	completed.DigestWindow = window
	completed.RequestedBy = domain.Requester{Label: "oto: the digest window of policy crashloops"}
	closed := fxInvestigationValue(domain.StatusSkipped)
	closed.SubjectKind, closed.SubjectID, closed.AlertKey = domain.SubjectDigest, fxPolicy, ""
	closed.DigestWindow = domain.DigestWindow{Start: window.End, End: window.End.Add(10 * time.Minute)}
	closed.Ending = domain.EndedBy(domain.ReasonWindowClosed, "the digest window closed before this run could start")
	closed.RequestedBy = completed.RequestedBy
	return []domain.Investigation{closed, completed}, db.Cursor{}, nil
}

func TestAPolicysDigestRunsAreReadableIncludingTheOnesThatNeverRan(t *testing.T) {
	t.Parallel()
	_, c := newClient(t)

	resp := c.GET("/notification-policies/"+fxPolicy.String()+"/investigations").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listPolicyDigestInvestigations", http.StatusOK, resp.Body())
	rows := resp.JSON(t)["data"].([]any)
	if len(rows) != 2 {
		t.Fatalf("got %d runs, want the policy's two", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["subject_kind"] != "digest" || first["status"] != "skipped" || first["reason"] != "window_closed" {
		t.Fatalf("the newest run answered %v/%v/%v, want digest skipped/window_closed",
			first["subject_kind"], first["status"], first["reason"])
	}
	if first["digest_window_start"] == nil || first["digest_window_end"] == nil {
		t.Fatalf("a digest run answered without its window: %v", first)
	}

	for _, path := range []string{"/notification-policies/" + uuid.NewString() + "/investigations",
		"/notification-policies/banana/investigations"} {
		resp := c.GET(path).MustStatus(t, http.StatusNotFound)
		schema.AssertProblem(t, "listPolicyDigestInvestigations", http.StatusNotFound, resp.Body())
	}
}
