package repository_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/sources/domain"
	"github.com/thulasiram/oto/internal/sources/repository"
	"github.com/thulasiram/oto/test/harness"
)

// `ListByIDs` is the batch behind the silence deep link: a page of mirrored
// silences names N sources, and this answers which of them still resolve, in ONE
// query rather than one per row.
//
// Its predicate is doing three jobs at once — `org_id = $1`, `deleted_at IS
// NULL`, `id = ANY($2)` — and each of them fails differently. Dropping the org
// predicate leaks another tenant's Alertmanager URL into this tenant's page, which
// is a cross-tenant disclosure through a link an operator is invited to click.
// Dropping the deleted_at predicate hands out the address of a source that was
// retired, possibly because it was compromised. Getting `ANY` wrong turns an
// unknown id into an error and costs the whole page its links.
//
// The suite is written as reads THROUGH the repository against a real Postgres,
// because the failure would be in the SQL, and SQL is the one thing a fake cannot
// be wrong about on the same terms.

// listByIDsRepo builds the repository under test over this test's own database.
func listByIDsRepo(h *harness.H) *repository.SourceRepository {
	return repository.NewSourceRepository(h.Pool, h.Clock)
}

// idsOf is the set of source ids a result carries, for assertions that are about
// membership rather than about the order the database happened to return.
func idsOf(srcs []domain.Source) map[string]string {
	out := make(map[string]string, len(srcs))
	for _, s := range srcs {
		out[s.ID.String()] = s.BaseURL
	}
	return out
}

// TestListByIDsAnswersOnlyAboutTheCallersOwnTenant is the disclosure case.
//
// The id is supplied by the CALLER — it comes off a row the caller already
// holds — so "the caller could only know an id they own" is not a defence: a
// caller who guesses, or who replays an id seen anywhere else, must get nothing.
func TestListByIDsAnswersOnlyAboutTheCallersOwnTenant(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := listByIDsRepo(h)

	mine := h.Org()
	ours := h.Source(mine, h.Cluster(mine))

	stranger := h.Org()
	theirs := h.SourceAt(stranger, h.Cluster(stranger), "https://am.stranger.example")

	got, err := repo.ListByIDs(h.Ctx, mine.Scope, []uuid.UUID{ours.ID, theirs.ID})
	require.NoError(t, err)

	set := idsOf(got)
	require.Contains(t, set, ours.ID.String(), "the caller's own source must resolve")
	require.NotContains(t, set, theirs.ID.String(),
		"another tenant's source resolved for this caller: its Alertmanager URL would be rendered "+
			"as a deep link on this tenant's silences page")
	require.Len(t, got, 1, "exactly one of the two ids belongs to this tenant")
}

// TestListByIDsHidesRetiredSources.
//
// Unlike `Get`, this read hides soft-deleted rows rather than handing them over:
// its caller is asking which sources still resolve, and has no id-shaped 404 to
// tell "deleted" from "never existed" against. A retired source must not keep
// answering — it may have been retired precisely because its address stopped
// being one this tenant should be sent to.
func TestListByIDsHidesRetiredSources(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := listByIDsRepo(h)

	org := h.Org()
	cl := h.Cluster(org)
	live := h.Source(org, cl)
	retired := h.SourceAt(org, cl, "https://am.retired.example")

	// Alive first, so the assertion below is about the DELETION and not about a
	// fixture that never resolved in the first place.
	got, err := repo.ListByIDs(h.Ctx, org.Scope, []uuid.UUID{live.ID, retired.ID})
	require.NoError(t, err)
	require.Len(t, got, 2, "both sources resolve while both are live")

	h.Exec(`UPDATE alert_sources SET deleted_at = $1 WHERE id = $2`, h.Now(), retired.ID)

	got, err = repo.ListByIDs(h.Ctx, org.Scope, []uuid.UUID{live.ID, retired.ID})
	require.NoError(t, err)

	set := idsOf(got)
	require.Contains(t, set, live.ID.String(), "one retirement must not cost the live source its answer")
	require.NotContains(t, set, retired.ID.String(), "a soft-deleted source still resolved")
}

// TestListByIDsTreatsAnUnknownIDAsAnAbsenceRatherThanAnError.
//
// The caller batches every source id on a page and asks which resolve; a missing
// one is the ordinary answer, not a failure. An error here would take the whole
// page's links down because one row named a source that had been purged.
func TestListByIDsTreatsAnUnknownIDAsAnAbsenceRatherThanAnError(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := listByIDsRepo(h)

	org := h.Org()
	src := h.Source(org, h.Cluster(org))
	unknown := id.New()

	got, err := repo.ListByIDs(h.Ctx, org.Scope, []uuid.UUID{src.ID, unknown})
	require.NoError(t, err, "an id with no row behind it is an absence, not an error")

	set := idsOf(got)
	require.Contains(t, set, src.ID.String())
	require.NotContains(t, set, unknown.String())
	require.Equal(t, src.BaseURL, set[src.ID.String()],
		"the row that did resolve came back whole, mapped through the domain")
}

// TestListByIDsWithNoIDsAsksTheDatabaseNothing.
//
// A page with no rows is the common case for a list endpoint — an empty filter
// result, the last page of a keyset walk — and the short-circuit is what stops
// each of those from costing a round trip that can only return nothing.
//
// The querier below FAILS the test rather than counting: a statement issued here
// is not a slow path to the right answer, it is the bug, and it should say so at
// the moment it happens.
func TestListByIDsWithNoIDsAsksTheDatabaseNothing(t *testing.T) {
	t.Parallel()

	// ⭐ No harness: a test that needs no database is a test that must not start
	// Docker, and refusing the querier is a stronger claim than watching a
	// connection go unused.
	scope := harness.Scope(t, id.New())
	repo := repository.NewSourceRepository(refusingQuerier{t: t}, nil)

	got, err := repo.ListByIDs(context.Background(), scope, nil)
	require.NoError(t, err)
	require.Empty(t, got, "no ids asked about, nothing to answer")

	got, err = repo.ListByIDs(context.Background(), scope, []uuid.UUID{})
	require.NoError(t, err)
	require.Empty(t, got)
}

// refusingQuerier is a `db.Querier` that fails the test if anything reaches the
// database. The embedded interface is nil on purpose: a method this file does not
// override panics, which is the same verdict arriving by a louder route.
type refusingQuerier struct {
	db.Querier
	t *testing.T
}

func (q refusingQuerier) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	q.t.Helper()
	q.t.Fatalf("a lookup of no ids still issued a statement:\n%s", sql)
	return nil, nil
}
