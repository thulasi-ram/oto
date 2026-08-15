package service_test

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// The login surface. This is the only thing standing between the internet and
// every tenant's alert history, and until this file existed none of it was tested.
//
// Every "rejected" case below asserts THREE things, because a 401 alone is not
// the property:
//
//  1. the answer is the same unspecific `invalid_credentials` 401;
//  2. the COST is the same — one argon2id evaluation, whichever path was taken;
//  3. NOTHING WAS WRITTEN. A failed login that left a session row behind would be
//     the same class of defect as the `TokenPrefixLen` orphan rows.

// TestTheRightPasswordMintsExactlyOneSession is the happy path, asserted down to
// what landed in the store.
func TestTheRightPasswordMintsExactlyOneSession(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)

	res, err := f.svc.Login(ctx(), loginCmd("priya@example.test", testPassword))
	if err != nil {
		t.Fatalf("the correct password was refused: %v", err)
	}

	if res.Principal.Kind != authn.KindSession {
		t.Fatalf("principal kind = %q, want session", res.Principal.Kind)
	}
	if res.Principal.OrgID != f.org || res.Principal.UserID != f.user.ID {
		t.Fatalf("principal names the wrong subject: org=%s user=%s", res.Principal.OrgID, res.Principal.UserID)
	}
	if res.Principal.SessionID != res.Session.Session.ID {
		t.Fatal("the principal and the minted session disagree about which session this is")
	}
	if !res.Principal.ExpiresAt.Equal(res.Session.Session.ExpiresAt) {
		t.Fatal("the principal's expiry is not the row's expiry; the cookie and the column would disagree")
	}

	inserted, _ := f.sessions.counts()
	if inserted != 1 {
		t.Fatalf("sessions inserted = %d, want exactly 1", inserted)
	}
	sess := f.sessions.inserted[0]
	if sess.OrgID != f.org || sess.UserID != f.user.ID {
		t.Fatal("the session row was written into the wrong tenant")
	}
	if got, want := sess.ExpiresAt, epoch.Add(testSessionTTL); !got.Equal(want) {
		t.Fatalf("expires_at = %s, want %s — the TTL comes from the injected clock", got, want)
	}
	if sess.UserAgent != "Mozilla/5.0 (test)" {
		t.Fatalf("user_agent = %q; it is stored for the sessions screen", sess.UserAgent)
	}

	// ⛔ THE SECRET IS NOT IN THE ROW. Only its sha256 is, which is what makes a
	// database disclosure not a session disclosure.
	if res.Session.Secret == "" {
		t.Fatal("no cookie value was returned; nothing could address this session")
	}
	if sess.Hash != digestOf(t, res.Session.Secret) {
		t.Fatal("the stored hash is not the sha256 of the returned secret")
	}
	if strings.Contains(sess.UserAgent, res.Session.Secret) || sess.Hash.String() != "[redacted]" {
		t.Fatal("the session secret is reachable from the stored row")
	}

	// One real verification, and NO dummy: the known-good path does exactly the
	// work the unknown path is padded to.
	if v, d := f.hasher.counts(); v != 1 || d != 0 {
		t.Fatalf("argon2id evaluations: verify=%d dummy=%d, want 1 and 0", v, d)
	}
}

// TestTheWrongPasswordIsRefusedAndWritesNothing.
func TestTheWrongPasswordIsRefusedAndWritesNothing(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)

	_, err := f.svc.Login(ctx(), loginCmd("priya@example.test", "not-the-password"))
	requireUnauthorized(t, err, "invalid_credentials")

	if inserted, _ := f.sessions.counts(); inserted != 0 {
		t.Fatalf("a failed login left %d session row(s) behind", inserted)
	}
	if v, d := f.hasher.counts(); v != 1 || d != 0 {
		t.Fatalf("argon2id evaluations: verify=%d dummy=%d, want 1 and 0", v, d)
	}
}

// ⭐ TestEveryFailurePathSpendsTheSameArgon2idWork is the user-enumeration
// defence, asserted STRUCTURALLY rather than with a stopwatch.
//
// The property the code claims is "every failure path costs the same wall-clock
// time", and the mechanism is `DummyVerify` — one argon2id evaluation burned on
// a path that has no stored hash to verify against. A wall-clock assertion of
// that is a flaky test that the first person it fails on will delete; the thing
// that is actually true on every machine is that EACH PATH PERFORMS EXACTLY ONE
// KDF EVALUATION. That is what is asserted here, per path, by counting through
// the `authn.PasswordHasher` seam.
//
// A statistical timing check over the REAL hasher lives in
// TestTheUnknownUserPathReallyRunsArgon2id below; it is one-sided and generously
// toleranced, for the reason given there.
func TestEveryFailurePathSpendsTheSameArgon2idWork(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		email    string
		password string
		setup    func(t *testing.T, f *authFixture)
		why      string
	}{
		{
			name:     "unknown address",
			email:    "nobody@example.test",
			password: testPassword,
			why:      "the address exists nowhere; without DummyVerify this answers in microseconds",
		},
		{
			name:     "malformed address",
			email:    "not-an-address",
			password: testPassword,
			why:      "rejected before any lookup, and still charged the same KDF",
		},
		{
			name:     "address in two orgs",
			email:    "shared@example.test",
			password: testPassword,
			setup: func(_ *testing.T, f *authFixture) {
				// What `ResolveByEmail`'s LIMIT 2 reports for an ambiguous address:
				// there is no honest way to pick an org, so it is a 401 like any other.
				f.users.ambiguous["shared@example.test"] = true
			},
			why: "an ambiguous address must not reveal that it exists twice",
		},
		{
			name:     "disabled user",
			email:    "disabled@example.test",
			password: testPassword,
			setup: func(t *testing.T, f *authFixture) {
				when := epoch.Add(-time.Hour)
				f.addUser(t, "disabled@example.test", testPassword, func(u *domain.User) {
					u.DisabledAt = &when
				})
			},
			why: "a disabled account must be indistinguishable from a nonexistent one",
		},
		{
			name:     "sso-only user with no password",
			email:    "sso@example.test",
			password: testPassword,
			setup: func(t *testing.T, f *authFixture) {
				f.addUser(t, "sso@example.test", "", nil)
			},
			why: "password_hash IS NULL is 'password login disabled', not 'no password required'",
		},
		{
			name:     "wrong password for a real user",
			email:    "priya@example.test",
			password: "not-the-password",
			why:      "the baseline every other path has to cost the same as",
		},
	}

	var messages []string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthFixture(t)
			if tc.setup != nil {
				tc.setup(t, f)
			}

			_, err := f.svc.Login(ctx(), loginCmd(tc.email, tc.password))
			requireUnauthorized(t, err, "invalid_credentials")

			// ⭐ ONE argon2id evaluation, whichever door the request went through.
			if got := f.hasher.work(); got != 1 {
				t.Fatalf("this path spent %d argon2id evaluation(s), want exactly 1 — %s", got, tc.why)
			}
			// The submitted password is what was hashed, not a constant: a dummy
			// that hashed "" would be a shorter, cheaper input on every attempt.
			if len(f.hasher.secrets) != 1 || f.hasher.secrets[0] != tc.password {
				t.Fatalf("the KDF was fed %v, want the submitted password", f.hasher.secrets)
			}
			if inserted, _ := f.sessions.counts(); inserted != 0 {
				t.Fatalf("a refused login wrote %d session row(s)", inserted)
			}
			messages = append(messages, err.Error())
		})
	}

	// The ANSWER is uniform too: one message, one code, for all six paths.
	sort.Strings(messages)
	if len(messages) > 0 && messages[0] != messages[len(messages)-1] {
		t.Fatalf("the failure paths do not answer identically: %q vs %q",
			messages[0], messages[len(messages)-1])
	}
}

// ⭐ TestTheUnknownUserPathReallyRunsArgon2id is the other half of the
// enumeration defence: the counter above proves DummyVerify is CALLED, and this
// proves DummyVerify is not a stub that returns immediately.
//
// ⚠️ IT IS A ONE-SIDED FLOOR, NOT AN EQUALITY. A test that asserted "the unknown
// path takes within 10% of the known path" is a coin flip on a loaded CI box and
// would be deleted the first week. What cannot happen by accident is an argon2id
// evaluation at 19 MiB finishing in under a millisecond, so the floor below
// separates "real KDF" from "no KDF at all" — which is exactly the regression
// that would reopen the oracle — with roughly four orders of magnitude of room.
//
// It also compares the two paths' medians with a deliberately loose 10x
// tolerance. A path that skipped the KDF entirely is ~1000x faster, so that
// bound catches the real defect while leaving room for any scheduler noise a
// shared machine can produce.
func TestTheUnknownUserPathReallyRunsArgon2id(t *testing.T) {
	if testing.Short() {
		t.Skip("argon2id at 19 MiB is too expensive for -short")
	}
	// Deliberately NOT parallel: the medians below are cheap to keep meaningful
	// if this test is not competing with the rest of the package for cores.

	f := newAuthFixtureWith(t, authn.NewPasswordHasher())

	const samples = 5
	unknown := make([]time.Duration, 0, samples)
	known := make([]time.Duration, 0, samples)

	for range samples {
		start := time.Now()
		_, err := f.svc.Login(ctx(), loginCmd("nobody@example.test", testPassword))
		unknown = append(unknown, time.Since(start))
		requireUnauthorized(t, err, "invalid_credentials")

		start = time.Now()
		_, err = f.svc.Login(ctx(), loginCmd("priya@example.test", "not-the-password"))
		known = append(known, time.Since(start))
		requireUnauthorized(t, err, "invalid_credentials")
	}

	// Every one of those attempts cost exactly one evaluation, half of them dummy.
	if v, d := f.hasher.counts(); v != samples || d != samples {
		t.Fatalf("argon2id evaluations: verify=%d dummy=%d, want %d each", v, d, samples)
	}

	medianUnknown, medianKnown := median(unknown), median(known)
	if medianUnknown < time.Millisecond {
		t.Fatalf("the unknown-address login took %v; argon2id at 19 MiB cannot finish that fast, "+
			"so DummyVerify is not doing the work that hides which addresses exist", medianUnknown)
	}
	if medianUnknown*10 < medianKnown || medianKnown*10 < medianUnknown {
		t.Fatalf("unknown-address %v vs wrong-password %v differ by more than 10x; "+
			"that gap is a user-enumeration oracle a stopwatch can read", medianUnknown, medianKnown)
	}
}

func median(d []time.Duration) time.Duration {
	c := append([]time.Duration(nil), d...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

// ⛔ TestAnUnreadableStoredHashIsNotAFailedLogin. A corrupted or foreign-format
// hash is oto's bug, and answering "wrong password" would hide it from the
// operator forever while locking exactly one person out permanently.
func TestAnUnreadableStoredHashIsNotAFailedLogin(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	broken, err := domain.NewPasswordHash(corruptEncoding)
	if err != nil {
		t.Fatalf("password hash: %v", err)
	}
	f.addUser(t, "corrupt@example.test", "", func(u *domain.User) { u.PasswordHash = broken })

	_, err = f.svc.Login(ctx(), loginCmd("corrupt@example.test", testPassword))
	if err == nil {
		t.Fatal("an unreadable stored hash authenticated somebody")
	}
	if !errs.IsKind(err, errs.KindInternal) {
		t.Fatalf("kind = %s, want internal_error: a bad row is not a bad password", errs.KindOf(err))
	}
	if errs.CodeOf(err) == "invalid_credentials" {
		t.Fatal("an oto bug was reported to the caller as their mistake")
	}
	if inserted, _ := f.sessions.counts(); inserted != 0 {
		t.Fatalf("a failed login wrote %d session row(s)", inserted)
	}
}

// ⚠️ TestLoginNeverLogsTheAddressOrThePassword is §L3 on the log stream.
//
// A record of every failed attempt WITH its address is a list of addresses worth
// attacking — the enumeration the uniform 401 exists to prevent, reintroduced
// through the log sink. The password is worse and needs no explanation.
func TestLoginNeverLogsTheAddressOrThePassword(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)

	// Every path, including the one that succeeds.
	_, _ = f.svc.Login(ctx(), loginCmd("nobody@example.test", "hunter2-hunter2"))
	_, _ = f.svc.Login(ctx(), loginCmd("priya@example.test", "hunter2-hunter2"))
	res, err := f.svc.Login(ctx(), loginCmd("priya@example.test", testPassword))
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	logs := f.logs.String()
	for _, forbidden := range []string{
		"hunter2-hunter2", testPassword, "nobody@example.test", "priya@example.test",
		res.Session.Secret,
	} {
		if strings.Contains(logs, forbidden) {
			t.Fatalf("the log stream contains %q:\n%s", forbidden, logs)
		}
	}
	// The session id is redacted to a stub rather than logged whole: it selects
	// exactly one live session and correlates a log stream with a person.
	if strings.Contains(logs, res.Session.Session.ID.String()) {
		t.Fatalf("the session id was logged in full:\n%s", logs)
	}
	if !strings.Contains(logs, "session issued") {
		t.Fatalf("the successful login was not recorded at all:\n%s", logs)
	}
}

// TestLoginRefusesToMintASessionWhenTheStoreFails — the credential is only
// returned once everything that could fail has succeeded.
func TestLoginRefusesToMintASessionWhenTheStoreFails(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	f.sessions.insertErr = errs.Internal("sessions_unavailable", nil)

	res, err := f.svc.Login(ctx(), loginCmd("priya@example.test", testPassword))
	if err == nil {
		t.Fatal("a session that could not be stored was still handed to the caller")
	}
	if res.Session.Secret != "" {
		t.Fatal("a cookie value was returned for a session that does not exist")
	}
	if res.Principal.Valid() {
		t.Fatal("a usable principal was returned by a failed login")
	}
}

// TestAnEmptyPasswordIsRefusedLikeAnyOther pins the case a validator would
// normally have caught first: the service is a boundary of its own, and `curl`
// reaching it directly must get the same answer as the form.
func TestAnEmptyPasswordIsRefusedLikeAnyOther(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)

	_, err := f.svc.Login(ctx(), loginCmd("priya@example.test", ""))
	requireUnauthorized(t, err, "invalid_credentials")
	if inserted, _ := f.sessions.counts(); inserted != 0 {
		t.Fatal("an empty password minted a session")
	}
	if got := f.hasher.work(); got != 1 {
		t.Fatalf("argon2id evaluations = %d, want 1", got)
	}
}

// TestTheSessionIsScopedToTheResolvedOrg — the login request carries no org, so
// the resolved user's tenant is the only one it may land in. A session written
// into the wrong tenant is a cross-tenant login.
func TestTheSessionIsScopedToTheResolvedOrg(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)

	res, err := f.svc.Login(ctx(), loginCmd("priya@example.test", testPassword))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.Principal.OrgID == uuid.Nil {
		t.Fatal("the principal names no tenant")
	}
	if f.sessions.inserted[0].OrgID != res.Principal.OrgID {
		t.Fatal("the session row and the principal disagree about the tenant")
	}
	if _, err := res.Principal.Scope(); err != nil {
		t.Fatalf("the minted principal yields no tenant scope: %v", err)
	}
}
