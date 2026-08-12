package app

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

// These tests are about the list every global sweep starts from. It is the one
// query in the process that is deliberately unscoped — it PRODUCES the scopes —
// so the two things that can go wrong with it both go wrong silently: sweeping a
// tenant that is gone, and sweeping only the first page of the ones that are not.
//
// They run against a real Postgres because both defects live in the SQL. A fake
// pool would agree with whatever the query says.

func TestMain(m *testing.M) { harness.Main(m) }

// TestOrgListerSkipsSoftDeletedTenants is the defect: `orgs.deleted_at` is a soft
// delete, and a sweep handed a departed tenant's scope does real work — reaping,
// closing, scoring, reminding — for an org nobody will ever read the answer from.
func TestOrgListerSkipsSoftDeletedTenants(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	live := h.Org()
	gone := h.Org()
	h.Exec(`UPDATE orgs SET deleted_at = $1 WHERE id = $2`, h.Now(), gone.ID)

	seen := listedOrgIDs(t, h)

	require.Equal(t, 1, seen[live.ID], "a live tenant must be swept, exactly once")
	require.Zero(t, seen[gone.ID], "a soft-deleted tenant must not be swept")
}

// TestOrgListerWalksEveryPage is the other half: the lister pages internally, and
// a paging bug is invisible until the tenant count crosses the page boundary —
// at which point the sweeps quietly stop visiting everyone past the first page.
//
// ⚠️ THE SEED SIZE AND THE CHOICE OF WHICH ORG TO DELETE ARE THE TEST. `id.New()`
// is a UUIDv7 and google/uuid's `getV7Time` guarantees the (millis, sequence)
// prefix strictly increases per call, so `seedOrgs` mints ids in ASCENDING order
// and the keyset walk visits them in the order they were minted. That makes the
// arithmetic load-bearing: what must survive is that at least one LIVE org sorts
// AFTER the first page boundary, so a lister that returns after a single page
// FAILS. Seeding `orgPageSize+2` and soft-deleting the SMALLEST id leaves
// `orgPageSize+1` live orgs, the last of which can only come from a second page.
//
// Do not "simplify" this to `orgPageSize+1` orgs with the LARGEST one deleted:
// that leaves exactly `orgPageSize` live orgs, page one returns all of them, and
// the test passes without the walk ever advancing its cursor.
func TestOrgListerWalksEveryPage(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	want := seedOrgs(t, h, orgPageSize+2)

	// The soft-deleted one is the smallest id, so it is removed from the FIRST
	// page — proving the filter is part of the query rather than a post-filter,
	// while leaving a live tenant stranded past the boundary.
	deleted := want[0]
	h.Exec(`UPDATE orgs SET deleted_at = $1 WHERE id = $2`, h.Now(), deleted)
	want = want[1:]
	require.Len(t, want, orgPageSize+1,
		"the live seed must exceed one page, or the walk is never exercised")

	seen := listedOrgIDs(t, h)

	require.Len(t, seen, len(want),
		"every live tenant is listed and none is listed twice")
	for _, orgID := range want {
		require.Equal(t, 1, seen[orgID], "tenant %s was missed or repeated", orgID)
	}
	require.Zero(t, seen[deleted], "a soft-deleted tenant must not be swept")
}

// listedOrgIDs runs the lister and counts what came back per org, so that a
// repeated tenant fails as loudly as a missing one — a keyset walk that does not
// advance would otherwise pass a plain "contains" assertion.
func listedOrgIDs(t *testing.T, h *harness.H) map[uuid.UUID]int {
	t.Helper()

	scopes, err := orgLister{pool: h.Pool}.Scopes(h.Ctx)
	require.NoError(t, err)

	seen := make(map[uuid.UUID]int, len(scopes))
	for _, s := range scopes {
		seen[s.OrgID()]++
	}
	return seen
}

// seedOrgs inserts n tenants in one statement. The harness builder is one round
// trip per org, which is the right shape for the three-org tests and the wrong
// one for a test whose whole point is to cross a 500-row page boundary.
//
// The ids are still minted by platform/id, never by the database: `orgs.id` is a
// UUIDv7 because the sweep's keyset walk orders by it.
func seedOrgs(t *testing.T, h *harness.H, n int) []uuid.UUID {
	t.Helper()

	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = id.New()
	}
	h.Exec(`INSERT INTO orgs (id, slug, name, created_at, updated_at)
	        SELECT o.id, 'page-' || o.ord, 'Page ' || o.ord, $2, $2
	          FROM unnest($1::uuid[]) WITH ORDINALITY AS o(id, ord)`, ids, h.Now())
	return ids
}
