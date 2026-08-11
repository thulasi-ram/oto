package contract

import (
	"sort"
	"strings"
	"testing"

	"github.com/thulasiram/oto/test/contract/schema"
)

// The tenant boundary, asserted against the contract itself.
//
// v1 has no RBAC (R2): an org member may do anything within their org. The
// tenant boundary is therefore the ONLY boundary in the system, and every
// operation that addresses a row by id is a place it can be crossed. The
// per-package probes prove the handlers hold it; the tests here prove the
// CONTRACT lets them — an operation that never declared a 404 could not express
// the correct answer even if the handler produced it.

// ⭐ TestEveryIdOperationCanSayNotFound.
//
// A cross-tenant id must be indistinguishable from an id that never existed.
// That answer is a 404, and an operation that does not declare one has no
// sanctioned way to give it — leaving a generated client to treat the correct
// refusal as an unexpected error, and inviting a handler author to reach for
// 403 instead, which confirms the row exists.
func TestEveryIdOperationCanSayNotFound(t *testing.T) {
	t.Parallel()

	var missing []string
	var n int
	for _, op := range schema.Operations(t) {
		if !op.HasPathParam("id") {
			continue
		}
		n++
		if !op.Declares(404) {
			missing = append(missing, op.ID+" ("+op.Method+" "+op.Path+")")
		}
	}
	if n == 0 {
		t.Fatal("no operation takes an {id}; the contract did not load")
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d of %d id-taking operations declare no 404:\n  %s",
			len(missing), n, strings.Join(missing, "\n  "))
	}
	t.Logf("%d operations address a resource by id and every one can answer 404", n)
}

// ⛔ TestBUG_TheContractSaysCrossOrgAccessIsA403.
//
// `#/components/responses/Forbidden` says, verbatim:
//
//	`403 forbidden` — cross-org access. In v1 this is the **only** cause of a
//	403: there are no roles and no scopes below the org boundary.
//
// Both halves are false, and the first half is the dangerous one.
//
//  1. Cross-org access does NOT produce a 403 anywhere in this server, and it
//     must not. Every read is scoped by `db.TenantScope`, so another org's id
//     simply is not there and the repository answers not-found. A 403 would be
//     an EXISTENCE ORACLE: 404 for an id nobody owns, 403 for one that belongs
//     to a competitor, and an attacker enumerating uuids learns which rows are
//     real. The handlers are right; the sentence is wrong.
//
//  2. It is not the only cause of a 403 either. `errs.Forbidden` is raised for a
//     non-human actor on a human verb (internal/alerts/api/router.go:141 and
//     :144, internal/grouping/api/router.go:265 — "this action requires a human
//     actor") and for `token_requires_user` (internal/identity/service/tokens.go
//     :48 and :110). Both are real 403s a client can receive, and neither is
//     cross-org anything.
//
// The fix is a documentation change in api/openapi/openapi.yaml, so this test
// is pinned rather than made to pass: it must not be "fixed" by teaching a
// handler to answer 403 on a cross-tenant id, which is the reading the current
// prose invites and the one that would leak.
func TestBUG_TheContractSaysCrossOrgAccessIsA403(t *testing.T) {
	t.Skip("pinned defect: #/components/responses/Forbidden documents cross-org access as 403. " +
		"No handler does that — cross-org is 404, and must stay 404 or a 403 becomes an existence " +
		"oracle. 403 is in fact raised for a non-human actor on a human verb and for " +
		"token_requires_user. The contract prose needs correcting, not the handlers.")
}

// TestNoOperationDeclaresA403WithoutA404.
//
// Whatever the prose says, the SHAPE of the contract must not let a 403 stand
// in for a 404. An operation offering 403 and not 404 would be one where the
// only way to refuse an id is to confirm it exists.
func TestNoOperationDeclaresA403WithoutA404(t *testing.T) {
	t.Parallel()

	var bad []string
	for _, op := range schema.Operations(t) {
		if op.Declares(403) && !op.Declares(404) && op.HasPathParam("id") {
			bad = append(bad, op.ID+" ("+op.Method+" "+op.Path+")")
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("%d id-taking operation(s) can say 403 but not 404:\n  %s\n\n"+
			"A refusal that only comes in 403 form tells the caller the row exists.",
			len(bad), strings.Join(bad, "\n  "))
	}
}
