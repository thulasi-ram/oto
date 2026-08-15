package repository_test

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/identity/repository"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

// The FOUR UNSCOPED resolvers in this module, against a real database.
//
// These are the queries that PRODUCE a tenancy rather than consume one — the
// login lookup, the cookie lookup, the bearer lookup and the Slack member lookup
// — which makes them the only place in the module where a predicate is the
// difference between "this request is org A" and "this request is org B".
// `identity/service`'s tests pin what the service does with what comes back; this
// file pins what comes back.
//
// ⛔ FOUR, NOT THREE. This file used to say three, and it agreed with the comment
// in `users.go` that said the same — which is precisely why neither noticed that
// `resolveSlackIdentitySQL` was missing the `orgs` join, or that a fifth resolver
// (`channels/repository.resolveSlackConversationSQL`) existed at all and was
// missing it too, in the path a human presses. A suite that only tests the
// resolvers its author already believed in cannot contradict a belief. The
// enumeration is now mechanical: see `tenancy_guard_test.go`, which reflects over
// every SQL constant in `internal/` and makes an unaccounted-for one fail.
//
// ⚠️ EVERY EXPIRY ASSERTION USES AN INJECTED `now`, never Postgres's. Both
// resolvers take the time as a parameter for exactly this reason, so a test
// advances a clock instead of sleeping.

// seedUser writes a user with a real argon2id-shaped hash, which the builders
// deliberately will not do: a fixture must not mint a credential a test did not
// ask for.
func seedUser(h *harness.H, org harness.Org, email string, mutate func(*seedOptions)) uuid.UUID {
	h.T.Helper()

	o := seedOptions{displayName: "Test User", passwordHash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2E$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhcw"}
	if mutate != nil {
		mutate(&o)
	}

	userID := id.New()
	h.Exec(`INSERT INTO users (id, org_id, email, display_name, password_hash,
	                           created_at, updated_at, disabled_at)
	        VALUES ($1, $2, $3, $4, $5, $6, $6, $7)`,
		userID, org.ID, email, o.displayName, nullableString(o.passwordHash), h.Now(), o.disabledAt)
	return userID
}

type seedOptions struct {
	displayName  string
	passwordHash string
	disabledAt   *time.Time
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ⭐ TestResolveByEmailIsOrgBlindAndRefusesAnAmbiguousAddress.
//
// `users_email_uniq` is (org_id, email): an address is unique WITHIN an org and
// not across the deployment. The login request carries no org, so this query is
// what produces one — and `LIMIT 2` is what stops it from producing the wrong
// one. With `LIMIT 1` the planner's physical ordering would decide which tenant
// a shared address logs into, which is a cross-tenant login nobody could see in
// a log.
func TestResolveByEmailIsOrgBlindAndRefusesAnAmbiguousAddress(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewUserRepository(h.Pool)

	const address = "priya@example.test"
	email, err := domain.NewEmail(address)
	require.NoError(t, err)

	// One org, one live row: an unambiguous login.
	orgA := h.Org()
	userA := seedUser(h, orgA, address, nil)

	got, err := repo.ResolveByEmail(h.Ctx, email)
	require.NoError(t, err, "a single live row must resolve")
	require.Equal(t, userA, got.ID)
	require.Equal(t, orgA.ID, got.OrgID, "the resolved row is what supplies the tenancy")
	require.False(t, got.PasswordHash.IsZero(), "the login path needs the hash this query returns")

	// The same address in a second org. There is no honest way to pick.
	orgB := h.Org()
	userB := seedUser(h, orgB, address, nil)

	_, err = repo.ResolveByEmail(h.Ctx, email)
	require.Error(t, err, "an address in two orgs must not authenticate either of them")
	require.ErrorIs(t, err, errs.ErrNotFound,
		"ambiguity is reported as not-found, so the service answers the same 401 as a wrong password")

	// A third org: `LIMIT 2` is a guard, not a count. Still refused.
	orgC := h.Org()
	seedUser(h, orgC, address, nil)
	_, err = repo.ResolveByEmail(h.Ctx, email)
	require.ErrorIs(t, err, errs.ErrNotFound)

	// Disabling the others makes the address unambiguous again — the exclusion is
	// in the SQL, not only in the domain, so a caller cannot forget it.
	disabled := h.Now()
	h.Exec(`UPDATE users SET disabled_at = $1 WHERE id <> $2 AND email = $3`, disabled, userA, address)

	got, err = repo.ResolveByEmail(h.Ctx, email)
	require.NoError(t, err, "one live row among three is unambiguous")
	require.Equal(t, userA, got.ID)
	require.NotEqual(t, userB, got.ID)

	// And a fully disabled address resolves to nobody.
	h.Exec(`UPDATE users SET disabled_at = $1 WHERE email = $2`, disabled, address)
	_, err = repo.ResolveByEmail(h.Ctx, email)
	require.ErrorIs(t, err, errs.ErrNotFound, "a disabled user must not be able to log in")
}

// ⛔ TestResolveByEmailExcludesASoftDeletedOrg. Every unscoped resolver asks the
// same question of `orgs`, and this is the one that used not to.
//
// `resolveSessionSQL` and `resolveByPrefixSQL` both INNER JOIN `orgs ... AND
// o.deleted_at IS NULL`, so a soft-deleted tenant's session and token stop
// working the instant the tenant does. `resolveByEmailSQL` now carries the same
// join, and so — since the review that found this — do the two Slack resolvers,
// which did not. The predicate holding in some of them was not a weaker version
// of the rule; it was a hole in it, and it had two consequences.
//
// This test covers the FIRST: a member of a soft-deleted org must not get past
// the lookup at all. Because the resolver reports not-found, `service.Login`
// takes its `err != nil` branch — DummyVerify, then the same unspecific 401 — and
// never reaches `issueSession`, so NO SESSION ROW IS WRITTEN FOR A DEAD TENANT.
// That the not-found branch of Login writes nothing is asserted from the other
// side, over the session store, in `identity/service/login_test.go`
// (TestEveryFailurePathSpendsTheSameArgon2idWork, "unknown address"). Before the
// join, that path did reach the INSERT: nothing could authenticate with the row —
// `resolveSessionSQL` DID ask — but every attempt left an orphan `sessions` row
// behind for the expiry sweep.
//
// The second consequence, the live user shadowed by a dead tenant, has its own
// test below.
func TestResolveByEmailExcludesASoftDeletedOrg(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewUserRepository(h.Pool)

	const address = "priya@example.test"
	email, err := domain.NewEmail(address)
	require.NoError(t, err)

	dead := h.Org()
	deadUser := seedUser(h, dead, address, nil)

	// While the tenant is alive the address resolves; deleting the tenant is the
	// only thing that changes between the two halves of this test.
	got, err := repo.ResolveByEmail(h.Ctx, email)
	require.NoError(t, err)
	require.Equal(t, deadUser, got.ID)

	h.Exec(`UPDATE orgs SET deleted_at = $1 WHERE id = $2`, h.Now(), dead.ID)

	_, err = repo.ResolveByEmail(h.Ctx, email)
	require.ErrorIs(t, err, errs.ErrNotFound,
		"a soft-deleted tenant's member must not get past the login lookup; "+
			"the session and token resolvers both exclude one, and a row that got past here "+
			"would be verified and then handed a session for an org that no longer exists")
}

// ⭐ TestASoftDeletedOrgDoesNotShadowALiveUserSharingTheAddress is the
// USER-VISIBLE half of the same defect, and the reason it was worth a ticket.
//
// `LIMIT 2` refuses an address that could log into more than one org, because
// there is no honest way to pick between them. Counting a DEAD org towards that
// ceiling turns one tenant's deletion into a different tenant's lockout: the live
// user is refused with the same unspecific 401 as a wrong password, there is
// nothing in it that says why, and nothing they can do about it — the row that
// shadows them belongs to a tenant they have never heard of.
//
// The ambiguity that must still be refused is "more than one LIVE org", which the
// last leg re-asserts so the join cannot be mistaken for a licence to relax
// LIMIT 2.
func TestASoftDeletedOrgDoesNotShadowALiveUserSharingTheAddress(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewUserRepository(h.Pool)

	const address = "priya@example.test"
	email, err := domain.NewEmail(address)
	require.NoError(t, err)

	dead := h.Org()
	seedUser(h, dead, address, nil)
	h.Exec(`UPDATE orgs SET deleted_at = $1 WHERE id = $2`, h.Now(), dead.ID)

	live := h.Org()
	liveUser := seedUser(h, live, address, nil)

	got, err := repo.ResolveByEmail(h.Ctx, email)
	require.NoError(t, err,
		"a deleted tenant's row made a live user's address ambiguous and locked them out")
	require.Equal(t, liveUser, got.ID)
	require.Equal(t, live.ID, got.OrgID, "the LIVE tenant is the one the login must land in")
	require.False(t, got.PasswordHash.IsZero(), "the login path needs the hash this query returns")

	// A second dead tenant changes nothing: they are not candidates, so they
	// cannot combine into an ambiguity either.
	alsoDead := h.Org()
	seedUser(h, alsoDead, address, nil)
	h.Exec(`UPDATE orgs SET deleted_at = $1 WHERE id = $2`, h.Now(), alsoDead.ID)

	got, err = repo.ResolveByEmail(h.Ctx, email)
	require.NoError(t, err, "two dead tenants are still nought candidates, not two")
	require.Equal(t, liveUser, got.ID)

	// ⚠️ And the guard is intact: a SECOND LIVE org sharing the address is still
	// ambiguous, and still refused.
	secondLive := h.Org()
	seedUser(h, secondLive, address, nil)

	_, err = repo.ResolveByEmail(h.Ctx, email)
	require.ErrorIs(t, err, errs.ErrNotFound,
		"excluding dead tenants must not have relaxed LIMIT 2 for live ones")
}

// TestResolveByEmailIsCaseInsensitive — `users.email` is CITEXT and the domain
// lower-cases on the way in. Both halves, deliberately, so neither side has to
// trust the other; if either were dropped, `Priya@…` and `priya@…` would be two
// accounts in Go and one row in Postgres.
func TestResolveByEmailIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewUserRepository(h.Pool)

	org := h.Org()
	userID := seedUser(h, org, "Priya.Sharma@Example.test", nil)

	email, err := domain.NewEmail("PRIYA.SHARMA@example.TEST")
	require.NoError(t, err)

	got, err := repo.ResolveByEmail(h.Ctx, email)
	require.NoError(t, err)
	require.Equal(t, userID, got.ID)
}

// insertSession mints a session row through the real writer and returns its
// secret.
func insertSession(
	t *testing.T, h *harness.H, org harness.Org, userID uuid.UUID, ttl time.Duration,
) (string, domain.Session) {
	t.Helper()

	secret := id.Token(32)
	sum := sha256.Sum256([]byte(secret))
	hash, err := domain.NewTokenHash(sum[:])
	require.NoError(t, err)

	sess, err := domain.NewSession(id.New(), org.ID, userID, hash, "Mozilla/5.0 (test)", h.Now(), ttl)
	require.NoError(t, err)
	require.NoError(t, repository.NewSessionRepository(h.Pool).Insert(h.Ctx, org.Scope, sess))
	return secret, sess
}

func digest(t *testing.T, secret string) domain.TokenHash {
	t.Helper()
	sum := sha256.Sum256([]byte(secret))
	hash, err := domain.NewTokenHash(sum[:])
	require.NoError(t, err)
	return hash
}

// ⛔ TestSessionResolutionFailsClosedInTheSQL. Expiry, revocation, a disabled
// user and a soft-deleted org are all PREDICATES here, so a stale session is
// never scanned — there is no code path in which one is examined and then let
// through. The service asks `Live()` again on whatever does come back; that half
// is pinned in `identity/service/sessions_test.go`.
func TestSessionResolutionFailsClosedInTheSQL(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewSessionRepository(h.Pool)
	org := h.Org()
	userID := seedUser(h, org, "priya@example.test", nil)

	const ttl = 24 * time.Hour

	t.Run("a live session resolves with its subject", func(t *testing.T) {
		secret, sess := insertSession(t, h, org, userID, ttl)

		hit, err := repo.ResolveByHash(h.Ctx, digest(t, secret), h.Now())
		require.NoError(t, err)
		require.Equal(t, sess.ID, hit.Session.ID)
		require.Equal(t, org.ID, hit.Subject.OrgID)
		require.Equal(t, userID, hit.Subject.UserID)
		require.Equal(t, org.Slug, hit.Subject.OrgSlug, "the join saves a second round trip per request")
		require.Equal(t, "priya@example.test", hit.Subject.Email.String())
	})

	t.Run("the expiry instant belongs to the dead side", func(t *testing.T) {
		secret, sess := insertSession(t, h, org, userID, ttl)

		_, err := repo.ResolveByHash(h.Ctx, digest(t, secret), sess.ExpiresAt.Add(-time.Second))
		require.NoError(t, err, "one second before expiry the session is still live")

		_, err = repo.ResolveByHash(h.Ctx, digest(t, secret), sess.ExpiresAt)
		require.ErrorIs(t, err, errs.ErrNotFound, "`expires_at > $2`: at the instant itself it is gone")

		_, err = repo.ResolveByHash(h.Ctx, digest(t, secret), sess.ExpiresAt.Add(time.Hour))
		require.ErrorIs(t, err, errs.ErrNotFound)
	})

	t.Run("a revoked session resolves to nothing", func(t *testing.T) {
		secret, sess := insertSession(t, h, org, userID, ttl)
		require.NoError(t, repo.Revoke(h.Ctx, org.Scope, sess.ID, h.Now()))

		_, err := repo.ResolveByHash(h.Ctx, digest(t, secret), h.Now())
		require.ErrorIs(t, err, errs.ErrNotFound)

		// Idempotent, and the timestamp does not move.
		var first time.Time
		require.NoError(t, h.Pool.QueryRow(h.Ctx,
			`SELECT revoked_at FROM sessions WHERE id = $1`, sess.ID).Scan(&first))
		require.NoError(t, repo.Revoke(h.Ctx, org.Scope, sess.ID, h.Now().Add(time.Hour)))
		var second time.Time
		require.NoError(t, h.Pool.QueryRow(h.Ctx,
			`SELECT revoked_at FROM sessions WHERE id = $1`, sess.ID).Scan(&second))
		require.True(t, first.Equal(second),
			"a second revocation moved the timestamp; that column is when the credential stopped working")
	})

	t.Run("revoking cannot reach into another org", func(t *testing.T) {
		secret, sess := insertSession(t, h, org, userID, ttl)
		stranger := h.Org()

		require.NoError(t, repo.Revoke(h.Ctx, stranger.Scope, sess.ID, h.Now()),
			"revoking somebody else's session is a no-op, not an error")

		_, err := repo.ResolveByHash(h.Ctx, digest(t, secret), h.Now())
		require.NoError(t, err, "another org's Revoke must not end this session")
	})

	t.Run("a disabled user's session dies at the next request", func(t *testing.T) {
		disabledOrg := h.Org()
		disabledUser := seedUser(h, disabledOrg, "sam@example.test", nil)
		secret, _ := insertSession(t, h, disabledOrg, disabledUser, ttl)

		h.Exec(`UPDATE users SET disabled_at = $1 WHERE id = $2`, h.Now(), disabledUser)

		_, err := repo.ResolveByHash(h.Ctx, digest(t, secret), h.Now())
		require.ErrorIs(t, err, errs.ErrNotFound,
			"disabling a user must end their browser session without a sweep having to run")
	})

	t.Run("a soft-deleted org's session dies with it", func(t *testing.T) {
		goneOrg := h.Org()
		goneUser := seedUser(h, goneOrg, "alex@example.test", nil)
		secret, _ := insertSession(t, h, goneOrg, goneUser, ttl)

		h.Exec(`UPDATE orgs SET deleted_at = $1 WHERE id = $2`, h.Now(), goneOrg.ID)

		_, err := repo.ResolveByHash(h.Ctx, digest(t, secret), h.Now())
		require.ErrorIs(t, err, errs.ErrNotFound)
	})

	t.Run("a cookie that addresses nothing", func(t *testing.T) {
		_, err := repo.ResolveByHash(h.Ctx, digest(t, id.Token(32)), h.Now())
		require.ErrorIs(t, err, errs.ErrNotFound)
	})
}

// ⚠️ TestTheSessionSweepIsBoundedAndOnlyTakesClosedWindows. It is hygiene: if it
// never ran, no expired session would work, because expiry is enforced by the
// resolver's predicate. What it must not do is take a live row with it, or lock
// the table by deleting everything at once.
func TestTheSessionSweepIsBoundedAndOnlyTakesClosedWindows(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewSessionRepository(h.Pool)
	org := h.Org()
	userID := seedUser(h, org, "priya@example.test", nil)

	liveSecret, _ := insertSession(t, h, org, userID, 24*time.Hour)
	for range 3 {
		insertSession(t, h, org, userID, time.Hour)
	}

	// Nothing has closed yet.
	n, err := repo.DeleteExpired(h.Ctx, h.Now(), 100)
	require.NoError(t, err)
	require.Zero(t, n, "the sweep took a session whose window is still open")

	after := h.Now().Add(2 * time.Hour)

	// The batch is a ceiling, not a suggestion: an unbounded DELETE is a lock on
	// a table every authenticated request reads.
	n, err = repo.DeleteExpired(h.Ctx, after, 2)
	require.NoError(t, err)
	require.EqualValues(t, 2, n)

	n, err = repo.DeleteExpired(h.Ctx, after, 100)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	// The live one survived, and still resolves.
	_, err = repo.ResolveByHash(h.Ctx, digest(t, liveSecret), h.Now())
	require.NoError(t, err, "the sweep deleted a live session")

	var remaining int
	require.NoError(t, h.Pool.QueryRow(h.Ctx, `SELECT count(*) FROM sessions`).Scan(&remaining))
	require.Equal(t, 1, remaining)
}

// mintPAT writes a PAT through the real writer and returns its secret.
func mintPAT(
	t *testing.T, h *harness.H, org harness.Org, userID uuid.UUID, expiresAt *time.Time,
) (string, domain.APIToken) {
	t.Helper()

	secret := domain.SecretPrefixPAT + id.Token(32)
	prefix, err := domain.PrefixOfSecret(secret)
	require.NoError(t, err)

	token, err := domain.NewAPIToken(domain.NewAPITokenParams{
		ID: id.New(), OrgID: org.ID, UserID: userID, Kind: domain.TokenKindPAT,
		Name: "laptop", Hash: digest(t, secret), Prefix: prefix,
		ExpiresAt: expiresAt, CreatedAt: h.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, repository.NewAPITokenRepository(h.Pool).Insert(h.Ctx, org.Scope, token))
	return secret, token
}

// ⛔ TestResolveByPrefixReturnsOnlyLivePATs.
//
// The prefix lookup AUTHENTICATES NOTHING — the service decides with a
// constant-time comparison — but what it returns is the candidate set, and every
// row in it is a row that could be authenticated. `kind = 'pat'` is therefore
// load-bearing: it is the second of two independent refusals that keep an ingest
// token off the read API.
func TestResolveByPrefixReturnsOnlyLivePATs(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewAPITokenRepository(h.Pool)
	org := h.Org()
	userID := seedUser(h, org, "priya@example.test", nil)

	t.Run("a live PAT comes back with its subject", func(t *testing.T) {
		secret, token := mintPAT(t, h, org, userID, nil)

		got, err := repo.ResolveByPrefix(h.Ctx, token.Prefix, h.Now())
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, token.ID, got[0].Token.ID)
		require.Equal(t, digest(t, secret), got[0].Token.Hash,
			"the row holds the sha256 of the secret and nothing else")
		require.Equal(t, userID, got[0].Subject.UserID)
		require.Equal(t, org.Slug, got[0].Subject.OrgSlug)
	})

	t.Run("an ingest token is not a candidate for the API", func(t *testing.T) {
		cluster := h.Cluster(org)
		source := h.Source(org, cluster)

		secret := domain.SecretPrefixIngest + id.Token(32)
		prefix, err := domain.PrefixOfSecret(secret)
		require.NoError(t, err)
		require.Len(t, prefix.String(), domain.TokenPrefixLenIngest,
			"an ingest prefix is fifteen characters; assuming twelve is what broke POST /sources")

		token, err := domain.NewAPIToken(domain.NewAPITokenParams{
			ID: id.New(), OrgID: org.ID, Kind: domain.TokenKindIngest, Name: "ingest:prod",
			Hash: digest(t, secret), Prefix: prefix, SourceID: source.ID, CreatedAt: h.Now(),
		})
		require.NoError(t, err)
		require.NoError(t, repo.Insert(h.Ctx, org.Scope, token))

		got, err := repo.ResolveByPrefix(h.Ctx, prefix, h.Now())
		require.NoError(t, err)
		require.Empty(t, got, "`kind = 'pat'` is what keeps an ingest credential off the read API")
	})

	t.Run("a revoked PAT is not a candidate", func(t *testing.T) {
		_, token := mintPAT(t, h, org, userID, nil)

		found, err := repo.Revoke(h.Ctx, org.Scope, token.ID, h.Now())
		require.NoError(t, err)
		require.True(t, found)

		got, err := repo.ResolveByPrefix(h.Ctx, token.Prefix, h.Now())
		require.NoError(t, err)
		require.Empty(t, got)

		// Idempotent: the second revoke reports found without moving the timestamp.
		found, err = repo.Revoke(h.Ctx, org.Scope, token.ID, h.Now().Add(time.Hour))
		require.NoError(t, err)
		require.True(t, found, "revoking twice must succeed; the contract makes DELETE retryable")
	})

	t.Run("an expired PAT is not a candidate", func(t *testing.T) {
		expiry := h.Now().Add(time.Hour)
		_, token := mintPAT(t, h, org, userID, &expiry)

		got, err := repo.ResolveByPrefix(h.Ctx, token.Prefix, h.Now())
		require.NoError(t, err)
		require.Len(t, got, 1)

		got, err = repo.ResolveByPrefix(h.Ctx, token.Prefix, expiry)
		require.NoError(t, err)
		require.Empty(t, got, "`expires_at > $2`: the boundary belongs to the dead side")
	})

	t.Run("a disabled user's PAT is not a candidate", func(t *testing.T) {
		otherOrg := h.Org()
		otherUser := seedUser(h, otherOrg, "sam@example.test", nil)
		_, token := mintPAT(t, h, otherOrg, otherUser, nil)

		h.Exec(`UPDATE users SET disabled_at = $1 WHERE id = $2`, h.Now(), otherUser)

		got, err := repo.ResolveByPrefix(h.Ctx, token.Prefix, h.Now())
		require.NoError(t, err)
		require.Empty(t, got, "disabling a person must end their tokens, not only their sessions")
	})

	t.Run("a soft-deleted org's PAT is not a candidate", func(t *testing.T) {
		goneOrg := h.Org()
		goneUser := seedUser(h, goneOrg, "alex@example.test", nil)
		_, token := mintPAT(t, h, goneOrg, goneUser, nil)

		h.Exec(`UPDATE orgs SET deleted_at = $1 WHERE id = $2`, h.Now(), goneOrg.ID)

		got, err := repo.ResolveByPrefix(h.Ctx, token.Prefix, h.Now())
		require.NoError(t, err)
		require.Empty(t, got)
	})

	t.Run("revoking cannot reach into another org", func(t *testing.T) {
		_, token := mintPAT(t, h, org, userID, nil)
		stranger := h.Org()

		// ⚠️ PINNING ACTUAL BEHAVIOUR. The doc comment on Revoke describes the
		// `found` boolean as what distinguishes "already revoked" from "no such
		// token in your org", but a cross-org id updates nothing and then finds
		// nothing on the existence probe, so it comes back as an ERROR
		// (`pgx.ErrNoRows` → `errs.NotFound`) rather than as `(false, nil)`. Both
		// routes reach the same 404 in `service.RevokeToken`, so this is a
		// documentation drift and not a defect — but a caller switching on `found`
		// alone would be wrong, which is why it is asserted here.
		found, err := repo.Revoke(h.Ctx, stranger.Scope, token.ID, h.Now())
		require.Error(t, err)
		require.ErrorIs(t, err, errs.ErrNotFound,
			"a cross-org id must report not-found; a 403 would confirm the id exists somewhere")
		require.False(t, found)

		got, err := repo.ResolveByPrefix(h.Ctx, token.Prefix, h.Now())
		require.NoError(t, err)
		require.Len(t, got, 1, "another org's revoke ended a live credential")
	})
}

// TestATokenRowCannotClaimAnotherOrg — the scope is the authority, and a row
// claiming a different tenant must never reach the driver.
func TestATokenRowCannotClaimAnotherOrg(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org, stranger := h.Org(), h.Org()
	userID := seedUser(h, org, "priya@example.test", nil)

	secret := domain.SecretPrefixPAT + id.Token(32)
	prefix, err := domain.PrefixOfSecret(secret)
	require.NoError(t, err)
	token, err := domain.NewAPIToken(domain.NewAPITokenParams{
		ID: id.New(), OrgID: org.ID, UserID: userID, Kind: domain.TokenKindPAT,
		Name: "laptop", Hash: digest(t, secret), Prefix: prefix, CreatedAt: h.Now(),
	})
	require.NoError(t, err)

	err = repository.NewAPITokenRepository(h.Pool).Insert(h.Ctx, stranger.Scope, token)
	require.Error(t, err, "a row whose org_id disagrees with the scope must be refused")
	require.ErrorIs(t, err, errs.ErrInternal, "it is oto's bug, not the caller's")

	var n int
	require.NoError(t, h.Pool.QueryRow(h.Ctx, `SELECT count(*) FROM api_tokens`).Scan(&n))
	require.Zero(t, n, "a refused insert left a row behind")
}

// ---------------------------------------------------- the Slack resolver

// seedSlackIdentity writes an UNLINKED sighting of a Slack member, through the
// real writer.
//
// Unlinked is the normal state and a success in its own right: an ack from
// somebody who has never opened oto's settings still records, with the Slack
// handle as the actor label. `slack_identities_link_ck` makes (user_id,
// linked_at) all-or-nothing, which is why the builder cannot half-set it.
func seedSlackIdentity(t *testing.T, h *harness.H, org harness.Org, team, member, handle string) uuid.UUID {
	t.Helper()

	teamID, err := domain.NewSlackTeamID(team)
	require.NoError(t, err)
	memberID, err := domain.NewSlackUserID(member)
	require.NoError(t, err)

	si, err := domain.NewSlackIdentity(id.New(), org.ID, teamID, memberID, handle)
	require.NoError(t, err)

	stored, err := repository.NewSlackIdentityRepository(h.Pool).Upsert(h.Ctx, org.Scope, si, h.Now())
	require.NoError(t, err)
	return stored.ID
}

func slackIDs(t *testing.T, team, member string) (domain.SlackTeamID, domain.SlackUserID) {
	t.Helper()
	teamID, err := domain.NewSlackTeamID(team)
	require.NoError(t, err)
	memberID, err := domain.NewSlackUserID(member)
	require.NoError(t, err)
	return teamID, memberID
}

// ⭐ TestResolveBySlackUserIsOrgBlindAndRefusesAnAmbiguousMember.
//
// `slack_identities_uniq` is (org_id, team_id, slack_user_id): a workspace member
// is unique WITHIN an org and not across the deployment, because one Slack
// workspace connected to two oto tenants is representable. The interaction
// payload names a workspace and a member and never an org, so this query is what
// produces one — and `LIMIT 2` is what stops the planner's physical ordering from
// deciding which tenant a press is attributed to.
func TestResolveBySlackUserIsOrgBlindAndRefusesAnAmbiguousMember(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewSlackIdentityRepository(h.Pool)

	const team, member = "T9TK3CUKW", "U0BF2XQLM"
	teamID, memberID := slackIDs(t, team, member)

	orgA := h.Org()
	identityA := seedSlackIdentity(t, h, orgA, team, member, "priya")

	got, err := repo.ResolveBySlackUser(h.Ctx, teamID, memberID)
	require.NoError(t, err, "a single live row must resolve")
	require.Equal(t, identityA, got.ID)
	require.Equal(t, orgA.ID, got.OrgID, "the resolved row is what supplies the tenancy")
	require.False(t, got.Linked(), "an unlinked sighting is a success, not a failure")

	// The same workspace member in a second org. There is no honest way to pick.
	orgB := h.Org()
	seedSlackIdentity(t, h, orgB, team, member, "priya")

	_, err = repo.ResolveBySlackUser(h.Ctx, teamID, memberID)
	require.ErrorIs(t, err, errs.ErrNotFound,
		"a member sighted in two orgs must not be attributed to either of them")

	// A different member in the same workspace is unaffected: the pair is the key.
	otherTeamID, otherMemberID := slackIDs(t, team, "U0BF2XQLZ")
	seedSlackIdentity(t, h, orgA, team, "U0BF2XQLZ", "sam")
	got, err = repo.ResolveBySlackUser(h.Ctx, otherTeamID, otherMemberID)
	require.NoError(t, err)
	require.Equal(t, orgA.ID, got.OrgID)
}

// ⛔ TestResolveBySlackUserExcludesASoftDeletedOrg.
//
// This resolver had the SAME hole `resolveByEmailSQL` had, for the same reason
// and with the same two consequences, and it survived 7f8e710 because that
// commit's comment asserted there were three unscoped resolvers when there are
// five. It is LATENT rather than live — `Service.ResolveSlackActor` has no
// production caller today, and `app/adapters.go` deliberately wires the
// ORG-SCOPED `RecordSlackIdentity` instead — but "latent" is a fact about the
// wiring, not about the query, and it is one wiring change from being the path a
// Slack ack takes.
//
// Soft-deleting an org does not touch `slack_identities`: the FK is `ON DELETE
// CASCADE`, which fires for a hard DELETE and never for `deleted_at = now()`.
func TestResolveBySlackUserExcludesASoftDeletedOrg(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewSlackIdentityRepository(h.Pool)

	const team, member = "T9TK3CUKW", "U0BF2XQLM"
	teamID, memberID := slackIDs(t, team, member)

	dead := h.Org()
	deadIdentity := seedSlackIdentity(t, h, dead, team, member, "priya")

	got, err := repo.ResolveBySlackUser(h.Ctx, teamID, memberID)
	require.NoError(t, err)
	require.Equal(t, deadIdentity, got.ID)

	h.Exec(`UPDATE orgs SET deleted_at = $1 WHERE id = $2`, h.Now(), dead.ID)

	var survived bool
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT count(*) = 1 FROM slack_identities WHERE org_id = $1`, dead.ID).Scan(&survived))
	require.True(t, survived, "soft-deleting an org must NOT have cascaded to slack_identities; "+
		"the join in the resolver is the only thing that asks the question")

	_, err = repo.ResolveBySlackUser(h.Ctx, teamID, memberID)
	require.ErrorIs(t, err, errs.ErrNotFound,
		"a soft-deleted tenant's Slack identity must not resolve; what comes back is an org id "+
			"that a caller turns into a db.TenantScope")
}

// ⭐ TestASoftDeletedOrgDoesNotShadowALiveSlackIdentity is the user-visible half,
// and it is the same lockout as the email one: a dead row counted towards
// `LIMIT 2` refuses a LIVE tenant's member.
//
// The last leg re-asserts that two LIVE tenants sharing a workspace member are
// still ambiguous, so the join cannot be mistaken for relaxing `LIMIT 2`.
func TestASoftDeletedOrgDoesNotShadowALiveSlackIdentity(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewSlackIdentityRepository(h.Pool)

	const team, member = "T9TK3CUKW", "U0BF2XQLM"
	teamID, memberID := slackIDs(t, team, member)

	dead := h.Org()
	seedSlackIdentity(t, h, dead, team, member, "priya")
	h.Exec(`UPDATE orgs SET deleted_at = $1 WHERE id = $2`, h.Now(), dead.ID)

	live := h.Org()
	liveIdentity := seedSlackIdentity(t, h, live, team, member, "priya")

	got, err := repo.ResolveBySlackUser(h.Ctx, teamID, memberID)
	require.NoError(t, err,
		"a deleted tenant's row made a live tenant's member ambiguous and unattributable")
	require.Equal(t, liveIdentity, got.ID)
	require.Equal(t, live.ID, got.OrgID, "the LIVE tenant is the one the press must land in")

	// A second dead tenant changes nothing: they are not candidates, so they
	// cannot combine into an ambiguity either.
	alsoDead := h.Org()
	seedSlackIdentity(t, h, alsoDead, team, member, "priya")
	h.Exec(`UPDATE orgs SET deleted_at = $1 WHERE id = $2`, h.Now(), alsoDead.ID)

	got, err = repo.ResolveBySlackUser(h.Ctx, teamID, memberID)
	require.NoError(t, err, "two dead tenants are still nought candidates, not two")
	require.Equal(t, liveIdentity, got.ID)

	// ⚠️ And the guard is intact.
	secondLive := h.Org()
	seedSlackIdentity(t, h, secondLive, team, member, "priya")

	_, err = repo.ResolveBySlackUser(h.Ctx, teamID, memberID)
	require.ErrorIs(t, err, errs.ErrNotFound,
		"excluding dead tenants must not have relaxed LIMIT 2 for live ones")
}

// TestASessionRowCannotClaimAnotherOrg is the same guard on `sessions`.
func TestASessionRowCannotClaimAnotherOrg(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org, stranger := h.Org(), h.Org()
	userID := seedUser(h, org, "priya@example.test", nil)

	sess, err := domain.NewSession(id.New(), org.ID, userID, digest(t, id.Token(32)),
		"", h.Now(), time.Hour)
	require.NoError(t, err)

	err = repository.NewSessionRepository(h.Pool).Insert(h.Ctx, stranger.Scope, sess)
	require.Error(t, err)
	require.ErrorIs(t, err, errs.ErrInternal)

	var n int
	require.NoError(t, h.Pool.QueryRow(h.Ctx, `SELECT count(*) FROM sessions`).Scan(&n))
	require.Zero(t, n)
}
