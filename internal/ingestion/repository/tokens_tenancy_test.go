package repository_test

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/ingestion/repository"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

func TestMain(m *testing.M) { harness.Main(m) }

// `Lookup` is the SIXTH unscoped org-producing resolver, and it is the one oto
// can least afford to get wrong.
//
// `api.Authenticator.Authenticate` hands the `OrgID` this query returns straight
// to `db.NewTenantScope`, and a scope cannot re-check what produced it. So this
// predicate is the whole of Alertmanager webhook authentication: everything
// ingest does afterwards — storing, grouping, enriching, notifying — is bound by
// the tenancy this one row minted.
//
// ⛔ IT IS THE WORST OF THE SIX TO GET WRONG, for a reason none of the others
// share. The other five are presented by a human at a keyboard who can be told to
// stop. An ingest token lives in an `alertmanager.yml` on every cluster the
// customer runs; when their tenant is deleted, nothing on their side knows, and
// the alerts keep arriving every evaluation interval. This predicate is the only
// place that can refuse them.
//
// It has no lockout half — `api_tokens_hash_idx` is UNIQUE, so a dead tenant
// cannot shadow a live one the way a dead org could on `resolveByEmailSQL`'s
// `LIMIT 2`. The defect is admission, not ambiguity.
//
// The roll-call of all six is in `identity/repository/users.go`, and
// `identity/repository/tenancy_guard_test.go` fails if a seventh appears without
// the join.

// seedIngestToken mints an `ingest` token for one org's source and returns the
// digest a webhook request would present.
//
// The row is written directly rather than through the identity service because
// what is under test is the READ predicate: the point is a token that is valid in
// every respect except that its tenant has since been deleted.
func seedIngestToken(t *testing.T, h *harness.H, org harness.Org) []byte {
	t.Helper()

	cl := h.Cluster(org)
	src := h.Source(org, cl)

	// `api_tokens_prefix_ck` is `^oto_(pat|ingest)_[A-Za-z0-9]{4}$`, so the stored
	// prefix is the scheme plus exactly four alphanumerics — a uuid's dashes do
	// not qualify, which is why the random part is hex with them stripped.
	body := strings.ReplaceAll(id.New().String(), "-", "")
	secret := "oto_ingest_" + body
	sum := sha256.Sum256([]byte(secret))
	digest := sum[:]

	h.Exec(`INSERT INTO api_tokens (id, org_id, source_id, kind, name, token_hash, prefix, created_at)
	        VALUES ($1, $2, $3, 'ingest', $4, $5, $6, $7)`,
		id.New(), org.ID, src.ID, "am-"+org.Slug, digest, secret[:len("oto_ingest_")+4], h.Now())

	return digest
}

// TestAnIngestTokenStopsWorkingWhenItsTenantIsDeleted is the defect.
//
// Before the `orgs` join, a soft-deleted tenant's Alertmanager went on ingesting
// indefinitely: alerts stored, grouped and notified for an org that no longer
// exists, with no way to make it stop short of revoking every token by hand.
func TestAnIngestTokenStopsWorkingWhenItsTenantIsDeleted(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewTokenRepository(h.Pool)

	org := h.Org()
	digest := seedIngestToken(t, h, org)

	// Alive: the token authenticates, which is what makes the assertion below
	// about the DELETION and not about a fixture that never worked.
	tok, err := repo.Lookup(h.Ctx, digest, h.Now())
	require.NoError(t, err, "the token must work while its tenant is alive")
	require.Equal(t, org.ID, tok.OrgID)

	h.Exec(`UPDATE orgs SET deleted_at = $1 WHERE id = $2`, h.Now(), org.ID)

	// ⛔ The row is untouched — there is no cascade for a soft delete, and that is
	// exactly why the predicate has to ask. Asserting the row survived first means
	// this test cannot start passing for the wrong reason if a cascade is ever
	// added.
	var alive int
	require.NoError(t,
		h.Pool.QueryRow(h.Ctx, `SELECT count(*) FROM api_tokens WHERE org_id = $1`, org.ID).Scan(&alive))
	require.Equal(t, 1, alive, "a soft delete must not remove the token; the read is what refuses it")

	_, err = repo.Lookup(h.Ctx, digest, h.Now())
	require.Error(t, err,
		"a soft-deleted tenant's ingest token still authenticated: its Alertmanager keeps posting "+
			"from every cluster it is installed on, and oto keeps storing and notifying for a dead org")
	require.ErrorIs(t, err, errs.ErrNotFound,
		"and it must be the same not-found an unknown token gets, so the refusal tells an attacker "+
			"nothing about whether the tenant ever existed")
}

// TestDeletingOneTenantDoesNotDisturbAnother is the blast-radius half.
//
// The join is an INNER join on a primary key, so it cannot fan out or drop a live
// row — but that is the kind of claim worth pinning rather than reasoning about,
// because the failure would be a whole tenant's ingest going dark.
func TestDeletingOneTenantDoesNotDisturbAnother(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewTokenRepository(h.Pool)

	dead := h.Org()
	deadDigest := seedIngestToken(t, h, dead)

	live := h.Org()
	liveDigest := seedIngestToken(t, h, live)

	h.Exec(`UPDATE orgs SET deleted_at = $1 WHERE id = $2`, h.Now(), dead.ID)

	_, err := repo.Lookup(h.Ctx, deadDigest, h.Now())
	require.Error(t, err, "the deleted tenant's token must be refused")

	tok, err := repo.Lookup(h.Ctx, liveDigest, h.Now())
	require.NoError(t, err, "an unrelated tenant's ingest must be untouched by someone else's deletion")
	require.Equal(t, live.ID, tok.OrgID)
}
