package service_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/id"
)

// Session lifetime, revocation and the sweep.
//
// ⭐ EVERY EXPIRY ASSERTION HERE IS AGAINST A STORE THAT HAS ALREADY FORGOTTEN
// TO FILTER. `fakeSessions.ResolveByHash` returns the row whatever the time is,
// which is exactly the mistake the production SQL does not make — and that is
// the point: what these tests pin is the SECOND enforcement,
// `domain.Session.Live`, asked again by `ResolveSession` on whatever came back.
// If that check were deleted, production would still look fine until the day the
// SQL predicate was edited. The SQL half is pinned against a real database in
// `identity/repository/auth_resolvers_test.go`.
//
// Nothing here sleeps. The service reads `platform/clock`, and the fixture hands
// it a `clock.Fake`.

// loginSession logs the fixture's user in and returns the cookie value.
func loginSession(t *testing.T, f *authFixture) (string, uuid.UUID) {
	t.Helper()
	res, err := f.svc.Login(ctx(), loginCmd("priya@example.test", testPassword))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return res.Session.Secret, res.Session.Session.ID
}

// TestASessionResolvesForItsWholeLifetimeAndNotAMomentLonger.
func TestASessionResolvesForItsWholeLifetimeAndNotAMomentLonger(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	secret, sessionID := loginSession(t, f)

	p, err := f.svc.ResolveSession(ctx(), secret)
	if err != nil {
		t.Fatalf("a fresh session did not resolve: %v", err)
	}
	if p.Kind != authn.KindSession || p.SessionID != sessionID || p.UserID != f.user.ID {
		t.Fatalf("the resolved principal is wrong: %+v", p)
	}
	if p.OrgSlug != "acme" || p.OrgName != "Acme" {
		t.Fatal("the joined subject did not reach the principal")
	}

	// One second before the window closes: still live.
	f.clk.Advance(testSessionTTL - time.Second)
	if _, err := f.svc.ResolveSession(ctx(), secret); err != nil {
		t.Fatalf("the session died before its expiry: %v", err)
	}

	// ⛔ At the expiry instant itself. `Live` is `!now.Before(ExpiresAt)`, so the
	// boundary belongs to the dead side — a session is not live at the moment it
	// expires.
	f.clk.Advance(time.Second)
	_, err = f.svc.ResolveSession(ctx(), secret)
	requireUnauthorized(t, err, "unauthenticated")

	f.clk.Advance(365 * 24 * time.Hour)
	_, err = f.svc.ResolveSession(ctx(), secret)
	requireUnauthorized(t, err, "unauthenticated")
}

// ⛔ TestAZeroExpiryReadsAsExpiredNotEternal is the fail-closed asymmetry, and it
// is the one that matters most: a row a mapper filled in wrongly, or a struct
// nobody passed through NewSession, must lock everybody out rather than mint one
// session that never ends.
func TestAZeroExpiryReadsAsExpiredNotEternal(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	secret := id.Token(32)

	f.sessions.put(domain.AuthenticatedSession{
		Session: domain.Session{
			ID:        id.New(),
			OrgID:     f.org,
			UserID:    f.user.ID,
			Hash:      digestOf(t, secret),
			CreatedAt: epoch,
			// ExpiresAt deliberately left zero.
		},
		Subject: f.sessions.subjectForRows,
	})

	_, err := f.svc.ResolveSession(ctx(), secret)
	requireUnauthorized(t, err, "unauthenticated")
}

// TestLogoutRevokesServerSideAndTheCookieStopsWorking.
func TestLogoutRevokesServerSideAndTheCookieStopsWorking(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	secret, sessionID := loginSession(t, f)

	p, err := f.svc.ResolveSession(ctx(), secret)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	f.clk.Advance(time.Hour)
	if err := f.svc.ExpireSession(ctx(), f.scope, p); err != nil {
		t.Fatalf("logout failed: %v", err)
	}
	if len(f.sessions.revoked) != 1 || f.sessions.revoked[0] != sessionID {
		t.Fatalf("logout revoked %v, want the caller's own session", f.sessions.revoked)
	}
	if !f.sessions.revokedAt.Equal(epoch.Add(time.Hour)) {
		t.Fatalf("revoked_at = %s; it must come from the injected clock", f.sessions.revokedAt)
	}

	// The cookie the browser still holds is now worth nothing.
	_, err = f.svc.ResolveSession(ctx(), secret)
	requireUnauthorized(t, err, "unauthenticated")

	// ⚠️ IDEMPOTENT. A client that cannot log out because it is already logged out
	// has been handed a problem it cannot act on.
	if err := f.svc.ExpireSession(ctx(), f.scope, p); err != nil {
		t.Fatalf("a second logout failed: %v", err)
	}
}

// TestLogoutIsANoOpForACredentialThatIsNotASession — a PAT-authenticated caller
// reaches here with a zero SessionID. The middleware refuses it first; this is
// the second refusal, and it must not revoke something arbitrary.
func TestLogoutIsANoOpForACredentialThatIsNotASession(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	_, _ = loginSession(t, f)

	pat := authn.Principal{Kind: authn.KindPAT, OrgID: f.org, UserID: f.user.ID, TokenID: id.New()}
	if err := f.svc.ExpireSession(ctx(), f.scope, pat); err != nil {
		t.Fatalf("logout for a PAT principal failed: %v", err)
	}
	if len(f.sessions.revoked) != 0 {
		t.Fatalf("a PAT logout revoked %v", f.sessions.revoked)
	}
}

// TestAnEmptyCookieIsRefusedWithoutALookup — no credential is not a credential,
// and it must not cost a database round trip on every anonymous request.
func TestAnEmptyCookieIsRefusedWithoutALookup(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)

	_, err := f.svc.ResolveSession(ctx(), "")
	requireUnauthorized(t, err, "unauthenticated")
	if _, lookups := f.sessions.counts(); lookups != 0 {
		t.Fatalf("an empty cookie caused %d store lookup(s)", lookups)
	}
}

// TestAForgedCookieIsRefused. The value is 256 bits of randomness and only its
// sha256 is stored, so guessing is not a strategy — but the refusal has to be
// the same unspecific one, and it must not resolve to anybody.
func TestAForgedCookieIsRefused(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	genuine, _ := loginSession(t, f)

	for _, forged := range []string{
		id.Token(32),
		genuine + "x",
		genuine[:len(genuine)-1],
		"oto_pat_" + id.Token(32),
	} {
		_, err := f.svc.ResolveSession(ctx(), forged)
		requireUnauthorized(t, err, "unauthenticated")
	}
	// The genuine one still works: the forgeries did not disturb it.
	if _, err := f.svc.ResolveSession(ctx(), genuine); err != nil {
		t.Fatalf("the real cookie stopped working: %v", err)
	}
}

// ⛔ TestASessionWithNoValidSubjectIsRefused. The resolver's joins already drop a
// disabled user and a soft-deleted org; this is the service asking again, and it
// is what keeps a half-filled row from minting a principal with no tenant.
func TestASessionWithNoValidSubjectIsRefused(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	secret := id.Token(32)

	f.sessions.put(domain.AuthenticatedSession{
		Session: domain.Session{
			ID:        id.New(),
			OrgID:     f.org,
			UserID:    f.user.ID,
			Hash:      digestOf(t, secret),
			CreatedAt: epoch,
			ExpiresAt: epoch.Add(testSessionTTL),
		},
		// A subject with no user: what a join that lost its `users` row would
		// produce.
		Subject: domain.Subject{OrgID: f.org, OrgSlug: "acme"},
	})

	_, err := f.svc.ResolveSession(ctx(), secret)
	requireUnauthorized(t, err, "unauthenticated")
}

// TestARevokedSessionRowIsRefusedEvenIfTheStoreReturnsIt is the other half of
// "fails closed twice over".
func TestARevokedSessionRowIsRefusedEvenIfTheStoreReturnsIt(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	secret := id.Token(32)
	when := epoch.Add(-time.Minute)

	f.sessions.put(domain.AuthenticatedSession{
		Session: domain.Session{
			ID:        id.New(),
			OrgID:     f.org,
			UserID:    f.user.ID,
			Hash:      digestOf(t, secret),
			CreatedAt: epoch.Add(-time.Hour),
			ExpiresAt: epoch.Add(testSessionTTL),
			RevokedAt: &when,
		},
		Subject: f.sessions.subjectForRows,
	})

	_, err := f.svc.ResolveSession(ctx(), secret)
	requireUnauthorized(t, err, "unauthenticated")
}

// TestExpireAllSessionsEndsEveryLiveSessionAUserHolds — what a password change
// and an account disable both need.
func TestExpireAllSessionsEndsEveryLiveSessionAUserHolds(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	f.clk.Advance(3 * time.Hour)

	if err := f.svc.ExpireAllSessions(ctx(), f.scope, f.user.ID); err != nil {
		t.Fatalf("expire all: %v", err)
	}
	if len(f.sessions.revokedAllFor) != 1 || f.sessions.revokedAllFor[0] != f.user.ID {
		t.Fatalf("revoked sessions for %v, want the one user", f.sessions.revokedAllFor)
	}
	if !f.sessions.revokedAt.Equal(epoch.Add(3 * time.Hour)) {
		t.Fatalf("revoked_at = %s; it must come from the injected clock", f.sessions.revokedAt)
	}
}

// ⚠️ TestTheSweepIsHygieneAndNotEnforcement.
//
// The sweep deletes rows whose window has closed. It must run on the injected
// clock, and — the part worth pinning — an expired session must ALREADY be
// unusable before the sweep has run at all. A security property that depends on
// a cron is a security property that stops holding the first time the cron does.
func TestTheSweepIsHygieneAndNotEnforcement(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	secret, _ := loginSession(t, f)

	f.clk.Advance(testSessionTTL + time.Hour)

	// Nothing has swept yet, and the credential is already dead.
	_, err := f.svc.ResolveSession(ctx(), secret)
	requireUnauthorized(t, err, "unauthenticated")
	if f.sessions.sweptCalls != 0 {
		t.Fatal("the sweep ran on its own; expiry must not depend on it")
	}

	n, err := f.svc.SweepExpiredSessions(ctx(), 250)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d rows, want 1", n)
	}
	if !f.sessions.sweptBefore.Equal(epoch.Add(testSessionTTL + time.Hour)) {
		t.Fatalf("the sweep cut at %s; it must use the injected clock", f.sessions.sweptBefore)
	}
	if f.sessions.sweptBatch != 250 {
		t.Fatalf("batch = %d, want the caller's 250 — an unbounded DELETE is a lock on the table",
			f.sessions.sweptBatch)
	}
}

// TestTheSweepLeavesLiveSessionsAlone. A sweep that took a live row with it is an
// outage for whoever was holding it.
func TestTheSweepLeavesLiveSessionsAlone(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	secret, _ := loginSession(t, f)

	f.clk.Advance(time.Hour)
	n, err := f.svc.SweepExpiredSessions(ctx(), 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("the sweep deleted %d live session(s)", n)
	}
	if _, err := f.svc.ResolveSession(ctx(), secret); err != nil {
		t.Fatalf("a live session was swept away: %v", err)
	}
}
