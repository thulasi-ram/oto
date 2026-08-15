package service_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// The in-memory doubles the authentication tests in this package run against.
//
// ⭐ THE STORES ARE DELIBERATELY LEAKY. Production's SQL already excludes revoked
// rows, expired rows, disabled users and soft-deleted orgs — see
// `resolveSessionSQL` and `resolveByPrefixSQL`. These doubles exclude NOTHING and
// hand back whatever they hold, because the property under test here is the
// SECOND refusal: `domain.Session.Live`, `domain.APIToken.Usable` and
// `domain.Subject.Valid`, asked again by the service on whatever came back. A
// double that filtered would make every one of those assertions vacuous and would
// pass just as happily if the service's own check were deleted.
//
// The predicates in the SQL are pinned separately, against a real database, in
// `identity/repository/auth_resolvers_test.go`.

// epoch is a fixed, far-from-any-boundary instant, so a test that advances the
// clock states its expectation as arithmetic rather than as "roughly now". It
// matches `test/harness.Epoch` without importing Docker into a unit test.
var epoch = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// testSessionTTL is short and round so expiry arithmetic reads at a glance.
const testSessionTTL = 24 * time.Hour

// testPassword is the one correct password in these tests. It is long enough for
// the REAL argon2id hasher's bounds, which the timing test uses.
const testPassword = "correct-horse-battery-staple"

// ------------------------------------------------------------------- hashers

// cheapEncoding is an argon2id-SHAPED encoding that costs nothing to verify.
// `domain.NewPasswordHash` requires the `$argon2id$` prefix and nothing more, so
// this is storable exactly where a real hash is.
func cheapEncoding(password string) string { return domain.PasswordHashPrefix + "plain$" + password }

// corruptEncoding is a stored hash oto cannot parse. It stands for a corrupted
// row or an encoding written by a different hasher; it is NOT a wrong password,
// and the login path must not conflate the two.
const corruptEncoding = domain.PasswordHashPrefix + "unreadable"

// cheapHasher is a substitute for argon2id that spends no memory and no time.
//
// It exists because a service test that verified real argon2id would pay 19 MiB
// and two passes per case. `authn.PasswordHasher` is a port precisely so this is
// possible — and note that DummyVerify is ON THE PORT, so a login path that
// skipped it would not compile against this type either.
type cheapHasher struct{}

func (cheapHasher) Hash(password string) (string, error) { return cheapEncoding(password), nil }

func (cheapHasher) Verify(encoded, password string) (bool, error) {
	if encoded == corruptEncoding {
		// Production returns KindInternal here, never "wrong password": an
		// unparseable stored hash is oto's bug and answering 401 would hide it.
		return false, errs.Wrap(errors.New("unreadable encoding"), errs.KindInternal,
			"password_hash_unreadable", "an internal error occurred")
	}
	return encoded == cheapEncoding(password), nil
}

func (cheapHasher) DummyVerify(string) {}

// countingHasher wraps any hasher and counts the argon2id evaluations the login
// path asks for, split by which door they came through.
//
// ⭐ THIS COUNTER IS HOW THE ENUMERATION DEFENCE IS ASSERTED. "The unknown-user
// path costs the same as the known-user path" is a statement about work
// performed, and work performed is observable here exactly, on every machine,
// with no stopwatch and no flake.
type countingHasher struct {
	inner authn.PasswordHasher

	mu      sync.Mutex
	verify  int
	dummy   int
	secrets []string
}

func newCountingHasher(inner authn.PasswordHasher) *countingHasher {
	return &countingHasher{inner: inner}
}

func (h *countingHasher) Hash(password string) (string, error) { return h.inner.Hash(password) }

func (h *countingHasher) Verify(encoded, password string) (bool, error) {
	h.mu.Lock()
	h.verify++
	h.secrets = append(h.secrets, password)
	h.mu.Unlock()
	return h.inner.Verify(encoded, password)
}

func (h *countingHasher) DummyVerify(password string) {
	h.mu.Lock()
	h.dummy++
	h.secrets = append(h.secrets, password)
	h.mu.Unlock()
	h.inner.DummyVerify(password)
}

// counts reports (real verifications, dummy verifications).
func (h *countingHasher) counts() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.verify, h.dummy
}

// work is the total number of argon2id evaluations this login cost. It is the
// number a stopwatch on the wire would be measuring.
func (h *countingHasher) work() int {
	v, d := h.counts()
	return v + d
}

// ---------------------------------------------------------------- user store

// fakeUsers is an in-memory UserReader.
//
// `ResolveByEmail` reproduces the repository's contract rather than its SQL: it
// answers NotFound both for an address nobody has and for one that exists in two
// orgs (the `LIMIT 2` case), because those are the two answers the service has to
// treat identically. The SQL itself is pinned against Postgres in
// `identity/repository/auth_resolvers_test.go`.
type fakeUsers struct {
	byEmail   map[string]domain.User
	byID      map[uuid.UUID]domain.User
	ambiguous map[string]bool

	mu       sync.Mutex
	resolves int
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{
		byEmail:   map[string]domain.User{},
		byID:      map[uuid.UUID]domain.User{},
		ambiguous: map[string]bool{},
	}
}

func (f *fakeUsers) add(u domain.User) {
	f.byEmail[u.Email.String()] = u
	f.byID[u.ID] = u
}

func (f *fakeUsers) ResolveByEmail(_ context.Context, email domain.Email) (domain.User, error) {
	f.mu.Lock()
	f.resolves++
	f.mu.Unlock()

	if f.ambiguous[email.String()] {
		return domain.User{}, errs.NotFound("user_not_found", "no such user")
	}
	u, ok := f.byEmail[email.String()]
	if !ok {
		return domain.User{}, errs.NotFound("user_not_found", "no such user")
	}
	return u, nil
}

func (f *fakeUsers) Get(_ context.Context, _ db.TenantScope, userID uuid.UUID) (domain.User, error) {
	u, ok := f.byID[userID]
	if !ok {
		return domain.User{}, errs.NotFound("user_not_found", "no such user")
	}
	return u, nil
}

func (f *fakeUsers) GetByEmail(_ context.Context, _ db.TenantScope, email domain.Email) (domain.User, error) {
	u, ok := f.byEmail[email.String()]
	if !ok {
		return domain.User{}, errs.NotFound("user_not_found", "no such user")
	}
	return u, nil
}

func (f *fakeUsers) ListMembers(context.Context, db.TenantScope, db.Keyset) ([]domain.User, db.Cursor, error) {
	return nil, db.Cursor{}, nil
}

// ------------------------------------------------------------- session store

// fakeSessions is an in-memory, deliberately LEAKY SessionStore: see the note at
// the top of this file.
type fakeSessions struct {
	mu sync.Mutex

	rows     map[domain.TokenHash]domain.AuthenticatedSession
	inserted []domain.Session
	lookups  int

	revoked        []uuid.UUID
	revokedAt      time.Time
	revokedAllFor  []uuid.UUID
	sweptBefore    time.Time
	sweptBatch     int
	sweptCalls     int
	insertErr      error
	subjectForRows domain.Subject
}

func newFakeSessions(subject domain.Subject) *fakeSessions {
	return &fakeSessions{
		rows:           map[domain.TokenHash]domain.AuthenticatedSession{},
		subjectForRows: subject,
	}
}

func (f *fakeSessions) Insert(_ context.Context, s db.TenantScope, sess domain.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return f.insertErr
	}
	if sess.OrgID != s.OrgID() {
		return errs.Internal("session_scope_mismatch", nil)
	}
	f.inserted = append(f.inserted, sess)
	f.rows[sess.Hash] = domain.AuthenticatedSession{Session: sess, Subject: f.subjectForRows}
	return nil
}

func (f *fakeSessions) ResolveByHash(
	_ context.Context, hash domain.TokenHash, _ time.Time,
) (domain.AuthenticatedSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	hit, ok := f.rows[hash]
	if !ok {
		return domain.AuthenticatedSession{}, errs.NotFound("session_not_found", "no such session")
	}
	return hit, nil
}

func (f *fakeSessions) Revoke(_ context.Context, s db.TenantScope, sessionID uuid.UUID, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, sessionID)
	f.revokedAt = at
	for h, row := range f.rows {
		if row.Session.ID == sessionID && row.Session.OrgID == s.OrgID() && row.Session.RevokedAt == nil {
			when := at
			row.Session.RevokedAt = &when
			f.rows[h] = row
		}
	}
	return nil
}

func (f *fakeSessions) RevokeAllForUser(_ context.Context, _ db.TenantScope, userID uuid.UUID, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokedAllFor = append(f.revokedAllFor, userID)
	f.revokedAt = at
	return nil
}

func (f *fakeSessions) DeleteExpired(_ context.Context, before time.Time, batch int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweptCalls++
	f.sweptBefore, f.sweptBatch = before, batch

	var n int64
	for h, row := range f.rows {
		if row.Session.ExpiresAt.Before(before) {
			delete(f.rows, h)
			n++
		}
	}
	return n, nil
}

// put installs a session row directly, for a test that needs a shape Login would
// never mint — a zero expiry, a revoked row, a subject with no user.
func (f *fakeSessions) put(row domain.AuthenticatedSession) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[row.Session.Hash] = row
}

func (f *fakeSessions) counts() (inserted, lookups int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inserted), f.lookups
}

// --------------------------------------------------------------- token store

// fakeTokens is an in-memory, deliberately LEAKY TokenStore: see the note at the
// top of this file. `ResolveByPrefix` filters by prefix ALONE — not by kind, not
// by revocation, not by expiry — so the service's own refusals are what the
// tests observe.
type fakeTokens struct {
	mu sync.Mutex

	rows      []domain.AuthenticatedToken
	inserted  []domain.APIToken
	resolves  int
	touched   []uuid.UUID
	touchErr  error
	insertErr error

	subjectForRows domain.Subject
}

func newFakeTokens(subject domain.Subject) *fakeTokens {
	return &fakeTokens{subjectForRows: subject}
}

func (f *fakeTokens) Insert(_ context.Context, s db.TenantScope, t domain.APIToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return f.insertErr
	}
	if t.OrgID != s.OrgID() {
		return errs.Internal("api_token_scope_mismatch", nil)
	}
	f.inserted = append(f.inserted, t)
	f.rows = append(f.rows, domain.AuthenticatedToken{Token: t, Subject: f.subjectForRows})
	return nil
}

func (f *fakeTokens) Get(_ context.Context, s db.TenantScope, tokenID uuid.UUID) (domain.APIToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.Token.ID == tokenID && row.Token.OrgID == s.OrgID() {
			return row.Token, nil
		}
	}
	return domain.APIToken{}, errs.NotFound("token_not_found", "no such token")
}

func (f *fakeTokens) List(
	_ context.Context, s db.TenantScope, kind domain.TokenKind, userID uuid.UUID, _ db.Keyset,
) ([]domain.APIToken, db.Cursor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.APIToken
	for _, row := range f.rows {
		t := row.Token
		if t.OrgID == s.OrgID() && t.Kind == kind && (userID == uuid.Nil || t.UserID == userID) {
			out = append(out, t)
		}
	}
	return out, db.Cursor{}, nil
}

func (f *fakeTokens) Revoke(_ context.Context, s db.TenantScope, tokenID uuid.UUID, at time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, row := range f.rows {
		if row.Token.ID != tokenID || row.Token.OrgID != s.OrgID() {
			continue
		}
		if row.Token.RevokedAt == nil {
			when := at
			f.rows[i].Token.RevokedAt = &when
		}
		return true, nil
	}
	// No such token in this org. The service turns this into a 404 — never a 403,
	// which would confirm the id exists somewhere else.
	return false, nil
}

func (f *fakeTokens) TouchLastUsed(_ context.Context, _ db.TenantScope, tokenID uuid.UUID, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, tokenID)
	return f.touchErr
}

func (f *fakeTokens) ResolveByPrefix(
	_ context.Context, prefix domain.TokenPrefix, _ time.Time,
) ([]domain.AuthenticatedToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves++

	var out []domain.AuthenticatedToken
	for _, row := range f.rows {
		if row.Token.Prefix.String() == prefix.String() {
			out = append(out, row)
		}
	}
	return out, nil
}

// put installs a token row directly, for a shape IssueToken would never mint.
func (f *fakeTokens) put(row domain.AuthenticatedToken) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, row)
}

func (f *fakeTokens) counts() (inserted, resolves int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inserted), f.resolves
}

// ------------------------------------------------------------------ fixture

// authFixture is one test's identity service and everything behind it.
type authFixture struct {
	svc      *service.Service
	users    *fakeUsers
	sessions *fakeSessions
	tokens   *fakeTokens
	hasher   *countingHasher
	clk      *clock.Fake
	logs     *bytes.Buffer

	// user is the one live, password-capable member of org.
	user  domain.User
	org   uuid.UUID
	scope db.TenantScope
}

// newAuthFixture builds the service with the cheap hasher.
func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	return newAuthFixtureWith(t, cheapHasher{})
}

// newAuthFixtureWith builds the service over an explicit hasher, so one test can
// pay for REAL argon2id where that is the point.
func newAuthFixtureWith(t *testing.T, inner authn.PasswordHasher) *authFixture {
	t.Helper()

	orgID, userID := id.New(), id.New()
	email, err := domain.NewEmail("priya@example.test")
	if err != nil {
		t.Fatalf("email: %v", err)
	}
	encoded, err := inner.Hash(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	hash, err := domain.NewPasswordHash(encoded)
	if err != nil {
		t.Fatalf("password hash: %v", err)
	}
	user, err := domain.NewUser(userID, orgID, email, "Priya", hash)
	if err != nil {
		t.Fatalf("user: %v", err)
	}

	subject := domain.Subject{
		OrgID: orgID, OrgSlug: "acme", OrgName: "Acme",
		UserID: userID, Email: email, DisplayName: "Priya",
	}

	f := &authFixture{
		users:    newFakeUsers(),
		sessions: newFakeSessions(subject),
		tokens:   newFakeTokens(subject),
		hasher:   newCountingHasher(inner),
		clk:      clock.NewFake(epoch),
		logs:     &bytes.Buffer{},
		user:     user,
		org:      orgID,
	}
	f.users.add(user)

	scope, err := db.NewTenantScope(orgID)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	f.scope = scope

	f.svc = service.New(service.Deps{
		Users:      f.users,
		Sessions:   f.sessions,
		Tokens:     f.tokens,
		Hasher:     f.hasher,
		Clock:      f.clk,
		Logger:     slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		SessionTTL: testSessionTTL,
	})
	return f
}

// addUser seeds another member, in another org, with its own password.
func (f *authFixture) addUser(t *testing.T, address, password string, mutate func(*domain.User)) domain.User {
	t.Helper()

	email, err := domain.NewEmail(address)
	if err != nil {
		t.Fatalf("email: %v", err)
	}
	hash := domain.NoPassword()
	if password != "" {
		encoded, herr := f.hasher.Hash(password)
		if herr != nil {
			t.Fatalf("hash: %v", herr)
		}
		hash, herr = domain.NewPasswordHash(encoded)
		if herr != nil {
			t.Fatalf("password hash: %v", herr)
		}
	}
	u, err := domain.NewUser(id.New(), f.org, email, "Member", hash)
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if mutate != nil {
		mutate(&u)
	}
	f.users.add(u)
	return u
}

// login is the command every test in this package sends.
func loginCmd(email, password string) service.LoginCommand {
	return service.LoginCommand{Email: email, Password: password, UserAgent: "Mozilla/5.0 (test)"}
}

// ---------------------------------------------------------------- assertions

// requireUnauthorized asserts the error is the ONE answer every credential
// failure gives, with the expected stable code.
func requireUnauthorized(t *testing.T, err error, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the call succeeded; it must fail with %s", wantCode)
	}
	if !errs.IsKind(err, errs.KindUnauthorized) {
		t.Fatalf("kind = %s, want unauthenticated (%v)", errs.KindOf(err), err)
	}
	if got := errs.CodeOf(err); got != wantCode {
		t.Fatalf("code = %q, want %q (%v)", got, wantCode, err)
	}
}

// digestOf is the transformation `service.digest` performs, restated here so a
// test can prove the row holds the sha256 of the secret and not the secret.
func digestOf(t *testing.T, secret string) domain.TokenHash {
	t.Helper()
	sum := sha256.Sum256([]byte(secret))
	h, err := domain.NewTokenHash(sum[:])
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return h
}

// ctx is the background context every one of these tests runs on.
func ctx() context.Context { return context.Background() }

// dbKeyset is the first page of anything.
func dbKeyset() db.Keyset { return db.Keyset{Limit: 50} }
