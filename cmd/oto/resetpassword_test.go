package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	identityrepo "github.com/thulasiram/oto/internal/identity/repository"
	identityservice "github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/test/harness"
)

// `oto reset-password` is THE ONLY RECOVERY FOR A LOCKED-OUT USER short of
// destroying the database. It rewrites a live credential outside any session,
// so what these tests pin down is narrower and sharper than "it runs":
// the OLD password must stop working, the NEW one must work, every session
// this user held must die with it, and a wrong org or address must change
// nothing at all.

// seedUser bootstraps one org and user through the real install path, so the
// account this test resets is the same shape a real one would be.
func seedUser(t *testing.T, h *harness.H, orgSlug, email, password string) {
	t.Helper()
	t.Setenv("OTO_BOOTSTRAP_PASSWORD", password)
	_, err := capture(t, func() error {
		return bootstrapCommand(h.Ctx, h.DSN, []string{
			"--org-slug", orgSlug, "--email", email,
		})
	})
	require.NoError(t, err)
}

func newIdentityService(h *harness.H) *identityservice.Service {
	return identityservice.New(identityservice.Deps{
		Orgs:     identityrepo.NewOrgRepository(h.Pool, h.Clock),
		Users:    identityrepo.NewUserRepository(h.Pool),
		Tokens:   identityrepo.NewAPITokenRepository(h.Pool),
		Sessions: identityrepo.NewSessionRepository(h.Pool),
		Slack:    identityrepo.NewSlackIdentityRepository(h.Pool),
		Hasher:   authn.NewPasswordHasher(),
		Clock:    h.Clock,
	})
}

// ⭐ TestResetPasswordChangesTheCredentialAndRevokesEverySession.
//
// One test, because "the old password is dead", "the new one works" and "the
// session minted under the old password died with it" are three facts about
// the SAME reset — asserting them separately would let a fix for one silently
// break another.
func TestResetPasswordChangesTheCredentialAndRevokesEverySession(t *testing.T) {
	h := harness.New(t)
	const oldPassword = "correct-horse-battery-staple"
	const newPassword = "a-completely-different-passphrase"
	seedUser(t, h, "acme", "operator@example.test", oldPassword)

	svc := newIdentityService(h)
	login, err := svc.Login(h.Ctx, identityservice.LoginCommand{
		Email: "operator@example.test", Password: oldPassword, UserAgent: "test",
	})
	require.NoError(t, err, "the seeded account must be able to log in before the reset")

	// The live session cookie a reset must kill.
	_, err = svc.ResolveSession(h.Ctx, login.Session.Secret)
	require.NoError(t, err, "the session must be live before the reset")

	t.Setenv("OTO_RESET_PASSWORD", newPassword)
	out, err := capture(t, func() error {
		return resetPasswordCommand(h.Ctx, h.DSN, []string{
			"--org-slug", "acme", "--email", "operator@example.test",
		})
	})
	require.NoError(t, err)
	require.Contains(t, out, "password changed")
	require.NotContains(t, out, newPassword, "the new password reached stdout")

	// The old password is dead.
	_, err = svc.Login(h.Ctx, identityservice.LoginCommand{
		Email: "operator@example.test", Password: oldPassword,
	})
	require.Error(t, err, "the reset left the old password working")

	// The new one works.
	newLogin, err := svc.Login(h.Ctx, identityservice.LoginCommand{
		Email: "operator@example.test", Password: newPassword, UserAgent: "test",
	})
	require.NoError(t, err, "the password the reset set does not log in")
	require.Equal(t, login.Principal.OrgID, newLogin.Principal.OrgID)

	// The session minted BEFORE the reset no longer resolves.
	_, err = svc.ResolveSession(h.Ctx, login.Session.Secret)
	require.Error(t, err, "a session from before the reset survived it")
}

func TestResetPasswordRefusesAnUnknownOrgOrUser(t *testing.T) {
	h := harness.New(t)
	seedUser(t, h, "acme", "operator@example.test", "correct-horse-battery-staple")

	t.Setenv("OTO_RESET_PASSWORD", "a-completely-different-passphrase")

	_, err := capture(t, func() error {
		return resetPasswordCommand(h.Ctx, h.DSN, []string{
			"--org-slug", "no-such-org", "--email", "operator@example.test",
		})
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no-such-org")

	_, err = capture(t, func() error {
		return resetPasswordCommand(h.Ctx, h.DSN, []string{
			"--org-slug", "acme", "--email", "stranger@example.test",
		})
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "stranger@example.test")

	// Neither refusal touched the real account's password.
	svc := newIdentityService(h)
	_, err = svc.Login(h.Ctx, identityservice.LoginCommand{
		Email: "operator@example.test", Password: "correct-horse-battery-staple",
	})
	require.NoError(t, err, "a refused reset changed the real user's password")
}

func TestResetPasswordRefusesWithoutThePasswordEnvironmentVariable(t *testing.T) {
	h := harness.New(t)
	seedUser(t, h, "acme", "operator@example.test", "correct-horse-battery-staple")
	t.Setenv("OTO_RESET_PASSWORD", "")

	_, err := capture(t, func() error {
		return resetPasswordCommand(h.Ctx, h.DSN, []string{
			"--org-slug", "acme", "--email", "operator@example.test",
		})
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "OTO_RESET_PASSWORD")

	// ...and there is no flag that would have worked either.
	_, err = capture(t, func() error {
		return resetPasswordCommand(h.Ctx, h.DSN, []string{
			"--org-slug", "acme", "--email", "operator@example.test",
			"--password", "correct-horse-battery-staple",
		})
	})
	require.Error(t, err, "--password was accepted; a password in argv is readable from ps")
}

func TestResetPasswordRefusesAPasswordShorterThanTheFloor(t *testing.T) {
	h := harness.New(t)
	seedUser(t, h, "acme", "operator@example.test", "correct-horse-battery-staple")
	t.Setenv("OTO_RESET_PASSWORD", "short")

	_, err := capture(t, func() error {
		return resetPasswordCommand(h.Ctx, h.DSN, []string{
			"--org-slug", "acme", "--email", "operator@example.test",
		})
	})
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "password")

	svc := newIdentityService(h)
	_, err = svc.Login(h.Ctx, identityservice.LoginCommand{
		Email: "operator@example.test", Password: "correct-horse-battery-staple",
	})
	require.NoError(t, err, "a refused reset changed the real user's password")
}
