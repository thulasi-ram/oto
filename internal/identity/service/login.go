package service

import (
	"context"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/authn"
)

// LoginCommand is a local password login. v1 has no SSO and no OIDC.
type LoginCommand struct {
	Email    string
	Password string
	// UserAgent is captured on the session row for the "your sessions" screen.
	// It is NEVER used for authorisation.
	UserAgent string
}

// LoginResult is a successful login: the principal, and the cookie value that
// will address its session.
type LoginResult struct {
	Principal authn.Principal
	Session   IssuedSession
}

// Login verifies a password and mints a session.
//
// ⭐ EVERY FAILURE PATH IS INDISTINGUISHABLE, in the answer AND in the timing.
//
// The answer: a malformed address, an unknown address, an address that exists in
// two orgs, a disabled user, a user with no password set and a wrong password
// all return the same `invalid_credentials` 401. The contract requires it — "a
// failed login returns an unspecific 401 that does not reveal whether the
// account exists" — and a code path that reveals it once reveals it forever.
//
// The timing: every one of those paths runs DummyVerify, which spends the same
// argon2id evaluation a real verification would. Without it the endpoint answers
// "no such user" in microseconds and "wrong password" in tens of milliseconds,
// and no amount of careful wording in the body closes a difference a stopwatch
// can read. This is the reason the hasher is on the interface rather than only
// on the concrete type: a login path that skipped the dummy would not compile
// against a substitute hasher that had no such method.
//
// ⚠️ THE PASSWORD IS NEVER LOGGED, and neither is the address on a failure — a
// log line naming the address of every failed attempt is a list of valid-looking
// addresses, which is the enumeration this function otherwise prevents.
func (s *Service) Login(ctx context.Context, cmd LoginCommand) (LoginResult, error) {
	email, err := domain.NewEmail(cmd.Email)
	if err != nil {
		s.hasher.DummyVerify(cmd.Password)
		return LoginResult{}, invalidCredentials()
	}

	user, err := s.users.ResolveByEmail(ctx, email)
	if err != nil {
		s.hasher.DummyVerify(cmd.Password)
		return LoginResult{}, invalidCredentials()
	}
	if !user.CanPasswordLogin() {
		// Disabled, or SSO/Slack-only. Same cost, same answer.
		s.hasher.DummyVerify(cmd.Password)
		return LoginResult{}, invalidCredentials()
	}

	ok, err := s.hasher.Verify(user.PasswordHash.Encoded(), cmd.Password)
	if err != nil {
		// An UNPARSEABLE STORED HASH IS NOT A FAILED LOGIN. It is oto's bug or a
		// corrupted row, and answering "wrong password" would hide it from the
		// operator forever while locking one user out permanently. The error is
		// KindInternal and carries no detail to the caller.
		s.log.ErrorContext(ctx, "identity: stored password hash is unreadable",
			"user_id", user.ID, "org_id", user.OrgID, "error", err.Error())
		return LoginResult{}, err
	}
	if !ok {
		return LoginResult{}, invalidCredentials()
	}

	// Authenticated. The Principal is built FIRST and the scope derived from it,
	// because authn.Principal.Scope is the only sanctioned path to a
	// db.TenantScope (CONTEXT.md §5.6) — including here, where the caller and the
	// authenticator are the same function.
	p := authn.Principal{
		Kind:        authn.KindSession,
		OrgID:       user.OrgID,
		UserID:      user.ID,
		DisplayName: user.DisplayName,
		Email:       user.Email.String(),
	}
	scope, err := p.Scope()
	if err != nil {
		return LoginResult{}, err
	}

	issued, err := s.issueSession(ctx, scope, user.ID, cmd.UserAgent)
	if err != nil {
		return LoginResult{}, err
	}

	p.SessionID = issued.Session.ID
	p.ExpiresAt = issued.Session.ExpiresAt
	return LoginResult{Principal: p, Session: issued}, nil
}
