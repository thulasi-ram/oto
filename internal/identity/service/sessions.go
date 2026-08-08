package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
)

// IssuedSession is a new browser session and the cookie value that addresses it.
//
// ⚠️ Secret IS THE COOKIE VALUE. It exists for exactly as long as it takes the
// api layer to write a Set-Cookie header. Only its sha256 is stored, so a
// database disclosure does not yield a live session.
type IssuedSession struct {
	Session domain.Session
	Secret  string
}

// issueSession mints a session for an already-authenticated user.
//
// It is unexported: there is exactly one way to obtain a session in oto, and it
// is Login. An exported minting function is a function somebody will call from a
// path that has not verified a password.
func (s *Service) issueSession(
	ctx context.Context, scope db.TenantScope, userID uuid.UUID, userAgent string,
) (IssuedSession, error) {
	secret := id.Token(SecretEntropyBytes)
	hash, err := digest(secret)
	if err != nil {
		return IssuedSession{}, err
	}

	sess, err := domain.NewSession(
		id.New(), scope.OrgID(), userID, hash, userAgent, s.clk.Now(), s.sessionTTL,
	)
	if err != nil {
		return IssuedSession{}, err
	}
	if err := s.sessions.Insert(ctx, scope, sess); err != nil {
		return IssuedSession{}, err
	}

	// The session id is NEVER logged in full: it selects exactly one live session
	// and correlates a shipped log stream with a specific human.
	s.log.InfoContext(ctx, "identity: session issued",
		"session", authn.RedactID(sess.ID), "user_id", userID, "org_id", scope.OrgID())

	return IssuedSession{Session: sess, Secret: secret}, nil
}

// ResolveSession verifies a session cookie and mints a Principal.
//
// ⭐ EXPIRY IS ENFORCED SERVER-SIDE AND FAILS CLOSED, twice over. The SQL will
// not return a revoked or expired row, and domain.Session.Live is asked again on
// whatever does come back — including the case the SQL cannot see, a zero
// ExpiresAt, which Live reads as EXPIRED rather than as eternal. Nothing here
// trusts the cookie's own opinion about when it stops working, because the
// cookie carries no such opinion: expiry is a column, not a claim.
func (s *Service) ResolveSession(ctx context.Context, secret string) (authn.Principal, error) {
	if secret == "" {
		return authn.Principal{}, unauthenticated()
	}
	hash, err := digest(secret)
	if err != nil {
		return authn.Principal{}, unauthenticated()
	}

	now := s.clk.Now()
	hit, err := s.sessions.ResolveByHash(ctx, hash, now)
	if err != nil {
		return authn.Principal{}, unauthenticated()
	}
	if !hit.Session.Live(now) || !hit.Subject.Valid() {
		return authn.Principal{}, unauthenticated()
	}

	return authn.Principal{
		Kind:        authn.KindSession,
		OrgID:       hit.Subject.OrgID,
		UserID:      hit.Subject.UserID,
		DisplayName: hit.Subject.DisplayName,
		Email:       hit.Subject.Email.String(),
		OrgSlug:     hit.Subject.OrgSlug,
		OrgName:     hit.Subject.OrgName,
		SessionID:   hit.Session.ID,
		ExpiresAt:   hit.Session.ExpiresAt,
	}, nil
}

// ExpireSession revokes the session behind the calling principal.
//
// It is IDEMPOTENT and never fails for a session that is already gone: logout
// must always succeed, because a client that cannot log out because it is
// already logged out has been handed a problem it cannot act on.
//
// A PAT-authenticated caller reaches here with a zero SessionID and is a no-op.
// The contract puts logout behind the session cookie alone, so that path is
// already refused at the middleware; this is the second refusal.
func (s *Service) ExpireSession(ctx context.Context, scope db.TenantScope, p authn.Principal) error {
	if p.SessionID == uuid.Nil {
		return nil
	}
	if err := s.sessions.Revoke(ctx, scope, p.SessionID, s.clk.Now()); err != nil {
		return err
	}
	s.log.InfoContext(ctx, "identity: session revoked",
		"session", authn.RedactID(p.SessionID), "org_id", scope.OrgID())
	return nil
}

// ExpireAllSessions ends every live session a user holds. It is what a password
// change and an account disable both need, and it is why those two operations
// will not each grow their own version of this loop.
func (s *Service) ExpireAllSessions(ctx context.Context, scope db.TenantScope, userID uuid.UUID) error {
	return s.sessions.RevokeAllForUser(ctx, scope, userID, s.clk.Now())
}

// SweepExpiredSessions deletes rows whose window has closed.
//
// ⚠️ HYGIENE, NOT ENFORCEMENT. Expiry is already enforced by the resolver's SQL
// predicate and by domain.Session.Live; this only reclaims table space. If this
// sweep never ran, no expired session would work — which is the correct
// relationship between a cron and a security property.
func (s *Service) SweepExpiredSessions(ctx context.Context, batch int) (int64, error) {
	return s.sessions.DeleteExpired(ctx, s.clk.Now(), batch)
}
