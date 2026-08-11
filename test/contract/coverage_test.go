package contract

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/thulasiram/oto/test/contract/schema"
)

// This file is the RATCHET.
//
// The issue this suite answers (git-bug e2a6d54) was not "these handlers have
// bugs". It was that eighty-five declared operations shared one test, and the
// only evidence any of them worked was a manual audit that had been performed
// once and could not be performed again. The audit's own verdict: *the causal
// chain is one link long — nothing has ever checked.*
//
// Tests for the operations that exist today close the gap once. This test is
// what stops it reopening: an operation added to `api/openapi/openapi.yaml`
// tomorrow, with a handler and no test, fails HERE, in CI, on the day it is
// written — which is exactly when a single smoke test would have caught every
// C-finding in the audit.
//
// The mechanism is deliberately crude and therefore hard to fool: a covered
// operation is one whose operationId appears as a quoted string in some
// `_test.go` file. Every test in this suite names its operation because
// `schema.Assert(t, "getSource", 200, body)` takes the operationId — the
// coverage record is a by-product of asserting the contract, not a separate
// thing to maintain.

// notYetCovered is the honest record of what this pass did NOT reach, with the
// reason. It is an allow-list that may only ever SHRINK. Adding an entry
// requires saying why in the same breath, which is the point: an untested
// operation should cost an argument, not a shrug.
var notYetCovered = map[string]string{}

// ⭐ TestEveryOperationIsNamedByATest.
func TestEveryOperationIsNamedByATest(t *testing.T) {
	t.Parallel()

	root, err := contractRepoRoot()
	if err != nil {
		t.Fatalf("locate the module root: %v", err)
	}
	corpus, err := readTestCorpus(root)
	if err != nil {
		t.Fatalf("read the test corpus: %v", err)
	}
	if len(corpus) == 0 {
		t.Fatal("no _test.go files were found; the coverage check would pass vacuously")
	}
	t.Logf("scanning %d test files, e.g. %s", len(corpus), corpus[0].path)

	var missing, staleExemption []string
	for _, op := range schema.Operations(t) {
		named := corpusMentions(corpus, op.ID)
		_, exempt := notYetCovered[op.ID]

		switch {
		case named && exempt:
			staleExemption = append(staleExemption, op.ID)
		case !named && !exempt:
			missing = append(missing, fmt.Sprintf("%s (%s %s)", op.ID, op.Method, op.Path))
		}
	}

	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d operation(s) are declared in api/openapi/openapi.yaml and named by no test:\n  %s\n\n"+
			"Every operation needs at least a happy-path httptest case asserting its status and its\n"+
			"response shape against the schema — `schema.Assert(t, \"<operationId>\", <status>, body)`.\n"+
			"If one genuinely cannot be covered yet, add it to notYetCovered in this file WITH THE REASON.",
			len(missing), strings.Join(missing, "\n  "))
	}

	// A ratchet that only tightens: once an exempted operation acquires a test,
	// its exemption must go, or the list stops describing reality.
	sort.Strings(staleExemption)
	if len(staleExemption) > 0 {
		t.Errorf("%d operation(s) are listed in notYetCovered but ARE now covered; delete their entries:\n  %s",
			len(staleExemption), strings.Join(staleExemption, "\n  "))
	}
}

// TestTheExemptionListNamesRealOperations, so that an entry left behind after a
// rename cannot quietly excuse an operation that no longer exists — or, worse,
// go on excusing one that does under a new name.
func TestTheExemptionListNamesRealOperations(t *testing.T) {
	t.Parallel()

	for id, why := range notYetCovered {
		if _, err := schema.Lookup(id); err != nil {
			t.Errorf("notYetCovered names %q (%q), which the contract does not declare", id, why)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("notYetCovered[%q] has no reason; an untested operation costs an argument", id)
		}
	}
}

// ⛔ TestEveryOperationWithAPathIdIsTenantProbed.
//
// The tenant-scoping probe is the highest-value assertion in this suite, and
// the one most easily forgotten: it is the only thing standing between a
// missing `WHERE org_id = $1` and one customer reading another's alerts.
//
// An operation that addresses a resource by id must have a test proving that
// ANOTHER ORG'S ID ANSWERS 404 — not 403, which confirms the row exists, and
// certainly not 200. This checks that such a test exists by looking for the
// operation named in the same file as the shared "stranger" id from
// `test/contract/apitest`, which is the only id in this repository that belongs
// to the other tenant.
func TestEveryOperationWithAPathIdIsTenantProbed(t *testing.T) {
	t.Parallel()

	root, err := contractRepoRoot()
	if err != nil {
		t.Fatalf("locate the module root: %v", err)
	}
	corpus, err := readTestCorpus(root)
	if err != nil {
		t.Fatalf("read the test corpus: %v", err)
	}

	var missing []string
	for _, op := range schema.Operations(t) {
		if !opAddressesAResource(op) {
			continue
		}
		if _, exempt := notYetCovered[op.ID]; exempt {
			continue
		}
		if !corpusTenantProbes(corpus, op.ID) {
			missing = append(missing, fmt.Sprintf("%s (%s %s)", op.ID, op.Method, op.Path))
		}
	}

	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d operation(s) take a resource id and are not tenant-probed:\n  %s\n\n"+
			"Add the operation to the table-driven tenant-scoping test in its api package: another\n"+
			"org's id must answer 404 — never 403, never another tenant's data, never a 500.\n"+
			"The probe is recognised by naming the operationId in a file that also uses\n"+
			"apitest.StrangerID or apitest.OtherOrgID.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// opAddressesAResource reports whether the operation takes an id-shaped path
// parameter — the shape a tenant-scoping mistake can leak through.
//
// `{name}` on listLabelValues is a label name, not a tenant-owned row, and
// `{source_id}` on the ingest webhook is authenticated by a per-source token
// rather than a session; both are excluded deliberately.
func opAddressesAResource(op schema.Operation) bool {
	for _, p := range op.PathParams {
		if p == "id" {
			return true
		}
	}
	return false
}

/* -------------------------------------------------------------------------- */
/* The corpus                                                                 */
/* -------------------------------------------------------------------------- */

type testFile struct {
	path string
	body string
}

// readTestCorpus reads every _test.go file under internal/ and test/.
func readTestCorpus(root string) ([]testFile, error) {
	var out []testFile
	for _, dir := range []string{"internal", "test", "cmd", "pkg"} {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			// This file names every operationId in its failure messages only,
			// never as a literal — but exclude it anyway so the ratchet can
			// never be satisfied by itself.
			if filepath.Base(path) == "coverage_test.go" {
				return nil
			}
			b, err := os.ReadFile(path) //nolint:gosec // a path produced by WalkDir under the module root
			if err != nil {
				return err
			}
			out = append(out, testFile{path: path, body: string(b)})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func corpusMentions(corpus []testFile, operationID string) bool {
	needle := `"` + operationID + `"`
	for _, s := range corpus {
		if strings.Contains(s.body, needle) {
			return true
		}
	}
	return false
}

// corpusTenantProbes looks for the operationId in a file that also reaches for
// the other tenant's id.
func corpusTenantProbes(corpus []testFile, operationID string) bool {
	needle := `"` + operationID + `"`
	for _, s := range corpus {
		if !strings.Contains(s.body, needle) {
			continue
		}
		if strings.Contains(s.body, "StrangerID") || strings.Contains(s.body, "OtherOrgID") {
			return true
		}
	}
	return false
}

func contractRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod above the working directory")
		}
		dir = parent
	}
}
