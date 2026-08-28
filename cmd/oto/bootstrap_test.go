package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/app"
	identityrepo "github.com/thulasiram/oto/internal/identity/repository"
	identityservice "github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/test/harness"
)

// `oto bootstrap` is THE INSTALL PATH. v1 has no org-creation API and no signup,
// so a migrated database has no credential that can reach it until this has run —
// and this subcommand had no test at all, which for the one command that creates
// the deployment's first full-access credential is the wrong place to be.
//
// These tests drive the SUBCOMMAND, not `app.Bootstrap` underneath it: the flag
// parsing, the refusal to take a password from a flag, and the printed secret are
// each a place a mistake would be invisible to a test of the function alone.
//
// ⚠️ Every failing case asserts that NOTHING WAS WRITTEN. A half-bootstrapped
// deployment — an org with no user, a user with no token — is one that neither
// works nor can be bootstrapped again.

func TestMain(m *testing.M) { harness.Main(m) }

const bootstrapPassword = "correct-horse-battery-staple"

// capture runs fn with os.Stdout redirected, and returns what it printed. The
// token is printed to stdout ONCE and never logged, so this is the only way to
// get at the credential the command mints.
func capture(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	r, w, err := os.Pipe()
	require.NoError(t, err)

	saved := os.Stdout
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	callErr := fn()

	os.Stdout = saved
	require.NoError(t, w.Close())
	out := <-done
	require.NoError(t, r.Close())
	return out, callErr
}

// counts is the whole state this command creates.
func counts(t *testing.T, h *harness.H) (orgs, users, tokens int) {
	t.Helper()
	require.NoError(t, h.Pool.QueryRow(h.Ctx, `SELECT count(*) FROM orgs`).Scan(&orgs))
	require.NoError(t, h.Pool.QueryRow(h.Ctx, `SELECT count(*) FROM users`).Scan(&users))
	require.NoError(t, h.Pool.QueryRow(h.Ctx, `SELECT count(*) FROM api_tokens`).Scan(&tokens))
	return orgs, users, tokens
}

// ⭐ TestBootstrapCreatesAWorkingInstallAndThenRefusesToRunAgain.
//
// One test, because the second run's assertions are only meaningful against the
// state the first run left — and the property is not "the second run errors", it
// is "the second run errors AND changes nothing".
func TestBootstrapCreatesAWorkingInstallAndThenRefusesToRunAgain(t *testing.T) {
	h := harness.New(t)
	t.Setenv("OTO_BOOTSTRAP_PASSWORD", bootstrapPassword)

	out, err := capture(t, func() error {
		return bootstrapCommand(h.Ctx, h.DSN, []string{
			"--org-slug", "acme",
			"--org-name", "Acme Inc",
			"--email", "operator@example.test",
			"--name", "Operator",
			"--token-name", "bootstrap",
		})
	})
	require.NoError(t, err, "the documented install path failed")

	orgs, users, tokens := counts(t, h)
	require.Equal(t, 1, orgs)
	require.Equal(t, 1, users)
	require.Equal(t, 1, tokens)

	// ⛔ THE SECRET IS PRINTED ONCE, TO STDOUT, AND NEVER TO THE LOG. Fish it out
	// of what the operator would have seen.
	secret := patFrom(t, out)
	require.Contains(t, out, "token_prefix "+secret[:12],
		"the printed prefix must identify the printed token")

	// The credential this command mints actually works. Without this assertion the
	// command could print a plausible token that authenticates nothing, which is
	// exactly the failure a fresh install cannot diagnose.
	svc := identityservice.New(identityservice.Deps{
		Orgs:     identityrepo.NewOrgRepository(h.Pool, h.Clock),
		Users:    identityrepo.NewUserRepository(h.Pool),
		Tokens:   identityrepo.NewAPITokenRepository(h.Pool),
		Sessions: identityrepo.NewSessionRepository(h.Pool),
		Slack:    identityrepo.NewSlackIdentityRepository(h.Pool),
		Hasher:   authn.NewPasswordHasher(),
		Clock:    h.Clock,
	})

	p, err := svc.ResolveBearer(h.Ctx, secret)
	require.NoError(t, err, "the token the install path printed does not authenticate")
	require.Equal(t, authn.KindPAT, p.Kind)
	require.Equal(t, "acme", p.OrgSlug)
	require.Equal(t, "operator@example.test", p.Email)

	// ...and so does the password, through the real argon2id verifier.
	login, err := svc.Login(h.Ctx, identityservice.LoginCommand{
		Email: "operator@example.test", Password: bootstrapPassword, UserAgent: "test",
	})
	require.NoError(t, err, "the password the install path set does not log in")
	require.Equal(t, p.OrgID, login.Principal.OrgID)

	// A wrong password still fails, which is the other half of "the hash is real".
	_, err = svc.Login(h.Ctx, identityservice.LoginCommand{
		Email: "operator@example.test", Password: "not-the-password",
	})
	require.Error(t, err)

	// --- the second run

	var hashBefore string
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT password_hash FROM users WHERE email = 'operator@example.test'`).Scan(&hashBefore))

	t.Setenv("OTO_BOOTSTRAP_PASSWORD", "a-completely-different-password")
	out, err = capture(t, func() error {
		return bootstrapCommand(h.Ctx, h.DSN, []string{
			"--org-slug", "attacker",
			"--email", "attacker@example.test",
		})
	})

	require.Error(t, err, "bootstrap ran twice")
	require.ErrorIs(t, err, app.ErrAlreadyBootstrapped,
		"the refusal must be the explicit one, not a constraint violation from three layers down")
	require.NotContains(t, out, "oto_pat_", "the refused run still printed a credential")

	// ⛔ NOTHING MOVED. Refusing is the only answer that is not either a silent
	// no-op the operator misreads as success, or a password reset on an existing
	// account — which is a takeover primitive.
	orgs, users, tokens = counts(t, h)
	require.Equal(t, 1, orgs, "the refused run created an org")
	require.Equal(t, 1, users, "the refused run created a user")
	require.Equal(t, 1, tokens, "the refused run minted a token")

	var hashAfter string
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT password_hash FROM users WHERE email = 'operator@example.test'`).Scan(&hashAfter))
	require.Equal(t, hashBefore, hashAfter, "a second bootstrap reset the first user's password")

	var strangers int
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT count(*) FROM orgs WHERE slug = 'attacker'`).Scan(&strangers))
	require.Zero(t, strangers)

	// The original credential still works after the refused run.
	_, err = svc.ResolveBearer(h.Ctx, secret)
	require.NoError(t, err, "the refused run disturbed the live credential")
}

// ⭐ TestBootstrapIfNeededIsIdempotentWithoutTouchingTheExistingOrg.
//
// `--if-needed` exists for a hook that re-runs. Argo CD maps helm's post-install
// hook onto PostSync and runs it on every sync, so the ordinary refusal — exit
// non-zero — makes every sync after the first report a failed hook for doing
// exactly what it should. The chart's Job passes this flag for that reason.
//
// ⛔ THE FLAG CHANGES THE EXIT CODE AND NOTHING ELSE. What must NOT change is
// everything the refusal was protecting: no second org, no password reset on the
// existing account, and no credential printed to a log somebody is now shipping.
func TestBootstrapIfNeededIsIdempotentWithoutTouchingTheExistingOrg(t *testing.T) {
	h := harness.New(t)
	t.Setenv("OTO_BOOTSTRAP_PASSWORD", bootstrapPassword)

	out, err := capture(t, func() error {
		return bootstrapCommand(h.Ctx, h.DSN, []string{
			"--org-slug", "acme", "--email", "operator@example.test", "--if-needed",
		})
	})
	require.NoError(t, err)
	require.Contains(t, out, "oto_pat_", "the first run must still print the token")

	var hashBefore string
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT password_hash FROM users WHERE email = 'operator@example.test'`).Scan(&hashBefore))

	// The re-run: a different org, a different address, a different password —
	// the shape a re-synced hook would arrive in if any of the values had been
	// edited between syncs.
	t.Setenv("OTO_BOOTSTRAP_PASSWORD", "a-completely-different-password")
	out, err = capture(t, func() error {
		return bootstrapCommand(h.Ctx, h.DSN, []string{
			"--org-slug", "attacker", "--email", "attacker@example.test", "--if-needed",
		})
	})
	require.NoError(t, err, "--if-needed still exited non-zero; every re-sync reports a failed hook")
	require.Contains(t, out, "already has an org", "an exit 0 that says nothing is indistinguishable from having run")
	require.NotContains(t, out, "oto_pat_", "the no-op run printed a credential")

	orgs, users, tokens := counts(t, h)
	require.Equal(t, 1, orgs, "the no-op run created an org")
	require.Equal(t, 1, users, "the no-op run created a user")
	require.Equal(t, 1, tokens, "the no-op run minted a token")

	var hashAfter string
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT password_hash FROM users WHERE email = 'operator@example.test'`).Scan(&hashAfter))
	require.Equal(t, hashBefore, hashAfter,
		"--if-needed reset the first user's password, which is a takeover primitive")

	// ⛔ AND IT IS NOT A BLANKET "NOTHING FAILS". The flag forgives one error;
	// a missing password is still a refusal.
	t.Setenv("OTO_BOOTSTRAP_PASSWORD", "")
	_, err = capture(t, func() error {
		return bootstrapCommand(h.Ctx, h.DSN, []string{
			"--org-slug", "acme", "--email", "operator@example.test", "--if-needed",
		})
	})
	require.Error(t, err, "--if-needed swallowed an unrelated failure")
	require.Contains(t, err.Error(), "OTO_BOOTSTRAP_PASSWORD")
}

// ⛔ TestBootstrapRefusesWithoutThePasswordEnvironmentVariable.
//
// The password is read from OTO_BOOTSTRAP_PASSWORD and not from a flag, because a
// flag value lands in shell history and in `ps`, where every other user on the box
// can read it. This pins that there is no flag path at all.
func TestBootstrapRefusesWithoutThePasswordEnvironmentVariable(t *testing.T) {
	h := harness.New(t)
	t.Setenv("OTO_BOOTSTRAP_PASSWORD", "")

	_, err := capture(t, func() error {
		return bootstrapCommand(h.Ctx, h.DSN, []string{
			"--org-slug", "acme", "--email", "operator@example.test",
		})
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "OTO_BOOTSTRAP_PASSWORD")

	orgs, users, tokens := counts(t, h)
	require.Zero(t, orgs+users+tokens, "a refused bootstrap wrote to the database")

	// ...and there is no flag that would have worked either.
	_, err = capture(t, func() error {
		return bootstrapCommand(h.Ctx, h.DSN, []string{
			"--org-slug", "acme", "--email", "operator@example.test",
			"--password", bootstrapPassword,
		})
	})
	require.Error(t, err, "--password was accepted; a password in argv is readable from ps")
}

// TestBootstrapRefusesAnUnusableRequestBeforeItWritesAnything. Each of these is a
// value the DDL would also refuse — the point is that it is refused HERE, with a
// message that says what to type, rather than as a 23514 naming a constraint.
func TestBootstrapRefusesAnUnusableRequestBeforeItWritesAnything(t *testing.T) {
	cases := []struct {
		name string
		args []string
		pass string
		want string
	}{
		{
			name: "no org slug",
			args: []string{"--email", "operator@example.test"},
			pass: bootstrapPassword,
			want: "org-slug",
		},
		{
			name: "no email",
			args: []string{"--org-slug", "acme"},
			pass: bootstrapPassword,
			want: "email",
		},
		{
			name: "an address that is not one",
			args: []string{"--org-slug", "acme", "--email", "operator"},
			pass: bootstrapPassword,
			want: "email",
		},
		{
			name: "a slug the DDL would refuse",
			args: []string{"--org-slug", "Not A Slug", "--email", "operator@example.test"},
			pass: bootstrapPassword,
			want: "org",
		},
		{
			name: "a password shorter than the floor",
			args: []string{"--org-slug", "acme", "--email", "operator@example.test"},
			// ⚠️ Deliberately longer than the interactive minimum: this credential
			// can configure every source in the org and is typed once.
			pass: "short",
			want: "password",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := harness.New(t)
			t.Setenv("OTO_BOOTSTRAP_PASSWORD", tc.pass)

			out, err := capture(t, func() error { return bootstrapCommand(h.Ctx, h.DSN, tc.args) })
			require.Error(t, err)
			require.Contains(t, strings.ToLower(err.Error()), tc.want,
				"the message must say what to type instead")
			require.NotContains(t, out, "oto_pat_", "a refused run printed a credential")
			require.NotContains(t, err.Error(), tc.pass, "the password reached an error message")

			orgs, users, tokens := counts(t, h)
			require.Zero(t, orgs+users+tokens, "a refused bootstrap wrote to the database")
		})
	}
}

// patFrom pulls the printed secret out of the command's stdout.
func patFrom(t *testing.T, out string) string {
	t.Helper()
	for _, field := range strings.Fields(out) {
		if strings.HasPrefix(field, "oto_pat_") && len(field) > 40 {
			return field
		}
	}
	t.Fatalf("no API token in the command's output:\n%s", out)
	return ""
}
