package api

// THE IDENTITY TRANSPORT, CHECKED AGAINST THE CONTRACT ITSELF.
//
// ⭐ NOTHING HERE RE-STATES A RESPONSE SHAPE BY HAND. Every success body is
// handed to `schema.Assert`, which compiles the JSON Schema
// `api/openapi/openapi.yaml` declares for that operationId and that status and
// validates the bytes the handler actually wrote; every refusal goes to
// `schema.AssertProblem`; every body this file SENDS goes to
// `schema.AssertRequest`, so a test cannot pass with a request no real client
// would be allowed to make. A test that spelled a shape out a second time would
// be a second copy of the contract, and a second copy drifts exactly the way the
// DTOs did.
//
// ⚠️ THIS FIXTURE DRIVES THE REAL `authn.Middleware`, unlike every other api
// package's. It has to: `/me` and `/org/settings` are behind `Require` and
// `/auth/logout`, `/api-tokens*` and `PATCH /org/settings` are behind
// `RequireSession`, and both re-authenticate the request rather than trusting a
// principal somebody put in the context. A fixture that injected a principal
// directly would test a route table that does not exist in the process.
//
// The properties this file protects, and what broke when each did not hold:
//
//   - the {data, meta} envelope with `meta.request_id` present — REQUIRED by the
//     contract, and `httpx.Meta` omits it when empty, so a response produced
//     outside the request-id middleware fails its own schema;
//   - the effective tuning is returned WITH its origins, its config keys and the
//     org overrides configuration is shadowing — an effective `600` that cannot
//     say whether the org chose it, the deployment forces it or oto ships it is
//     the version of configurability that is worse than none;
//   - the one-time token secret appears on the 201 and NOWHERE else — it is the
//     only response shape in oto that carries a credential;
//   - a token id in another tenant answers 404 and never 403 — a 403 confirms the
//     row exists somewhere, which turns the id space into a cross-tenant
//     existence oracle.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// The fixture ids are CONSTANTS, never freshly generated: a tenant-scoping
// failure has to be reproducible from the failure message, and the whole point of
// the probe is that the message names the exact id that leaked.
var (
	contractTokenID   = uuid.MustParse("77777777-7777-4777-8777-777777777777")
	contractSessionID = uuid.MustParse("99999999-9999-4999-8999-999999999999")
)

const (
	// contractCookieName is the contract's own session cookie.
	contractCookieName = "oto_session"
	// contractCookieValue is the session secret this fixture presents. It is NOT a
	// PAT: a `oto_pat_` value in the cookie jar is refused by the middleware, and
	// that refusal is auth_boundary_test.go's subject rather than this file's.
	contractCookieValue = "n7q2w9x4t6p8m3r5y1c0badefghijklmnopqrstu"

	// contractTokenSecret is the plaintext a freshly minted PAT carries, and it is
	// the string every "the secret is not here" assertion below searches for. It
	// satisfies the contract's own `^oto_pat_[A-Za-z0-9]+$` and its 20..200 length,
	// and its first twelve characters are contractTokenPrefix, exactly as
	// `domain.PrefixOfSecret` derives them.
	contractTokenSecret = "oto_pat_Ab3DE7fG2hJ9kL4mN1pQ6rS3tU8vW5xY"
	// contractTokenPrefix is the stored, displayable slice of that secret.
	contractTokenPrefix = "oto_pat_Ab3D"

	// contractSlackUserID is the linked Slack identity on the `/me` fixture.
	contractSlackUserID = "U7A2K9QLM"
)

// contractIdentityEpoch is when everything in this file happened.
var contractIdentityEpoch = time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

// The request bodies this file sends, as the bytes that go on the wire. They are
// literals rather than marshalled Go values so that `schema.AssertRequest` and the
// handler see BYTE-IDENTICAL input: a fixture validated in one shape and sent in
// another proves nothing about either.
const (
	// contractSettingsPatchBody raises the storm threshold and hands the flap
	// threshold back to oto's default.
	contractSettingsPatchBody = `{"storm_threshold":60,"reset":["flap_threshold"]}`
	// contractSettingsBadResetBody names a key that does not exist. It is a body
	// the CONTRACT permits — `reset` is an array of bounded strings, with no enum —
	// which is precisely why the server has to refuse it.
	contractSettingsBadResetBody = `{"reset":["refire_grace"]}`

	// contractCreateTokenBody mints a token that expires.
	contractCreateTokenBody = `{"name":"laptop CLI","expires_at":"2027-03-01T09:00:00.000Z"}`
	// contractCreateTokenBlankNameBody is a name the contract permits (minLength 1)
	// and `notblank` refuses: present is not the same as meaningful.
	contractCreateTokenBlankNameBody = `{"name":"   "}`
)

// ------------------------------------------------------------------- doubles

// contractSessionResolver admits exactly one session secret and nothing else.
//
// It is a resolver rather than an injected principal because the identity routes
// run the REAL middleware — see the file comment. It authenticates ONLY
// contractCookieValue, so "the caller presented a credential" is a fact this
// fixture can turn off.
type contractSessionResolver struct {
	mu      sync.Mutex
	bearers int
	cookies int
}

func (r *contractSessionResolver) ResolveBearer(context.Context, string) (authn.Principal, error) {
	r.mu.Lock()
	r.bearers++
	r.mu.Unlock()
	// ⛔ This fixture holds no PAT. A bearer credential reaching the resolver at
	// all would mean the session-only boundary let one through.
	return authn.Principal{}, authn.Unauthenticated()
}

func (r *contractSessionResolver) ResolveSession(_ context.Context, secret string) (authn.Principal, error) {
	r.mu.Lock()
	r.cookies++
	r.mu.Unlock()
	if secret != contractCookieValue {
		return authn.Principal{}, authn.Unauthenticated()
	}
	return authn.Principal{
		Kind:        authn.KindSession,
		OrgID:       apitest.OrgID,
		UserID:      apitest.UserID,
		DisplayName: "Priya Raman",
		Email:       "priya@example.com",
		OrgSlug:     "acme-observability",
		OrgName:     "Acme Observability",
		SessionID:   contractSessionID,
		ExpiresAt:   contractIdentityEpoch.Add(24 * time.Hour),
	}, nil
}

// contractIdentityService owns exactly one org and exactly one token, and answers
// 404 for every other token id. That is what makes the tenant probe honest: the
// handler cannot pass by accident, because the only path to a 204 is the id this
// tenant owns.
//
// ⛔ ITS REVOKE PATH IS THE REPOSITORY'S BEHAVIOUR, NOT A CONVENIENCE. The real
// query runs `WHERE org_id = $scope AND id = $id` under `db.TenantScope`, so a
// token belonging to another org matches no row and the service raises
// `errs.NotFound`. Answering 403 here would be a fixture that could never
// reproduce the leak this probe exists to catch.
type contractIdentityService struct {
	mu sync.Mutex

	org   domain.Org
	decl  domain.Declarative
	user  domain.User
	token domain.APIToken

	// issued is the command the last create carried, so a test can prove the
	// request reached the service unmangled.
	issued service.CreateTokenCommand
	// patched and cleared are the last write's parsed patch and reset list.
	patched domain.SettingsPatch
	cleared []domain.SettingKey

	calls identityCalls
}

// identityCalls is the fake's call ledger. It is a comparable struct on purpose,
// so "the service was never reached" is one assertion rather than seven.
type identityCalls struct {
	me, getOrg, update, list, issue, revoke, expire int
}

func (s *contractIdentityService) Login(context.Context, service.LoginCommand) (service.LoginResult, error) {
	// Login has its own suite (auth_boundary_test.go) and no route in this file
	// reaches it. Failing loudly beats returning a plausible session.
	return service.LoginResult{}, errs.Internal("login_not_driven_by_this_file", nil)
}

func (s *contractIdentityService) ExpireSession(context.Context, db.TenantScope, authn.Principal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls.expire++
	return nil
}

func (s *contractIdentityService) Me(
	_ context.Context, _ db.TenantScope, p authn.Principal,
) (service.MeView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls.me++
	user := s.user
	return service.MeView{
		Principal:   p,
		Org:         s.org,
		User:        &user,
		SlackUserID: contractSlackUserID,
	}, nil
}

func (s *contractIdentityService) GetOrg(context.Context, db.TenantScope) (domain.Org, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls.getOrg++
	return s.org, nil
}

func (s *contractIdentityService) UpdateOrgSettings(
	_ context.Context, _ db.TenantScope, p domain.SettingsPatch, reset []domain.SettingKey,
) (domain.Org, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls.update++
	s.patched, s.cleared = p, reset

	// The real service merges onto what the org already wrote, clears the named
	// keys and recomputes the effective settings under the declarative layer. Doing
	// the same here is what makes the 200 body describe the write rather than the
	// state before it.
	s.org.Overrides = s.org.Overrides.Merge(p).Clear(reset...)
	s.org = s.org.WithDeclarative(s.decl)
	return s.org, nil
}

func (s *contractIdentityService) ListTokens(
	context.Context, db.TenantScope, authn.Principal, db.Keyset,
) ([]domain.APIToken, db.Cursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls.list++
	return []domain.APIToken{s.token}, db.Cursor{}, nil
}

func (s *contractIdentityService) IssueToken(
	_ context.Context, _ db.TenantScope, _ authn.Principal, cmd service.CreateTokenCommand,
) (service.IssuedToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls.issue++
	s.issued = cmd
	return service.IssuedToken{Token: s.token, Secret: contractTokenSecret}, nil
}

func (s *contractIdentityService) RevokeToken(_ context.Context, _ db.TenantScope, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls.revoke++
	if id != s.token.ID {
		// ⛔ Anything this tenant does not own is a 404, never a 403 and never
		// somebody else's row.
		return errs.NotFound("not_found", "no such resource")
	}
	return nil
}

func (s *contractIdentityService) counts() identityCalls {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// ------------------------------------------------------------------ fixtures

// contractOrg builds a tenant whose tuning exercises all three origins at once.
//
// ⭐ THE RICHNESS IS THE TEST. `OrgSettingsViewDTO` requires `settings`,
// `origins`, `config_keys`, `shadowed` and `bounds`, and four of those five are
// empty on an org that never configured anything — so a fixture of defaults would
// only prove that the empty case validates. This org:
//
//   - WROTE `refire_grace_s`, `storm_threshold`, `flap_threshold` and the whole
//     mention surface, so `origins` reports `org` and the enums are populated;
//   - runs under a DEPLOYMENT that forces `refire_grace_s` and
//     `raw_retention_days`, so `origins` reports `config`, `config_keys` names the
//     env var and the file key, and `shadowed` carries the 1800 the org wrote and
//     is not getting.
func contractOrg(t *testing.T) (domain.Org, domain.Declarative) {
	t.Helper()

	org, err := domain.NewOrg(apitest.OrgID, "acme-observability", "Acme Observability",
		domain.DefaultSettings())
	if err != nil {
		t.Fatalf("build the org: %v", err)
	}

	mentions := []string{"<@U7A2K9QLM>", "<!subteam^SA1B2C3D4>"}
	org.Overrides = domain.SettingsPatch{
		RefireGraceS:                      intPtrOf(1800),
		StormThreshold:                    intPtrOf(40),
		FlapThreshold:                     intPtrOf(8),
		UnackedReminderAfterS:             intPtrOf(900),
		DefaultVerbosity:                  strPtrOf("all"),
		BroadcastOnResolved:               boolPtrOf(true),
		UnackedReminderMention:            strPtrOf("list"),
		UnackedReminderMentionList:        &mentions,
		UnackedReminderMentionMinSeverity: strPtrOf("warning"),
	}
	if err := org.Overrides.Validate(); err != nil {
		t.Fatalf("the fixture's own overrides are outside the bounds the server enforces: %v", err)
	}

	decl, err := domain.NewDeclarative([]domain.DeclaredEntry{
		{Key: string(domain.KeyRefireGrace), ConfigKey: "OTO_TUNING_REFIRE_GRACE_S", Value: 2400},
		{Key: string(domain.KeyRawRetention), ConfigKey: "tuning.raw_retention_days", Value: 45},
	})
	if err != nil {
		t.Fatalf("build the declarative layer: %v", err)
	}

	return org.WithDeclarative(decl), decl
}

// contractUser builds the human behind the session, through the real constructor
// so `email` and `display_name` are values the schema's `format: email` and length
// bounds actually accept.
func contractUser(t *testing.T) domain.User {
	t.Helper()

	email, err := domain.NewEmail("priya@example.com")
	if err != nil {
		t.Fatalf("build the email: %v", err)
	}
	u, err := domain.NewUser(apitest.UserID, apitest.OrgID, email, "Priya Raman", domain.PasswordHash{})
	if err != nil {
		t.Fatalf("build the user: %v", err)
	}
	u.CreatedAt = contractIdentityEpoch
	return u
}

// contractToken builds the one PAT this tenant owns, through `domain.NewAPIToken`
// so every `api_tokens_*` invariant the repository would enforce holds here too —
// including the one the DDL cannot express, that the prefix announces the row's
// kind.
func contractToken(t *testing.T) domain.APIToken {
	t.Helper()

	var digest [domain.TokenHashBytes]byte
	for i := range digest {
		digest[i] = byte(i + 1)
	}
	hash, err := domain.NewTokenHash(digest[:])
	if err != nil {
		t.Fatalf("build the token hash: %v", err)
	}

	// ⭐ The prefix is DERIVED from the secret rather than typed twice, which is
	// what makes "the four displayed characters belong to this credential" a fact
	// rather than a coincidence of two literals.
	prefix, err := domain.PrefixOfSecret(contractTokenSecret)
	if err != nil {
		t.Fatalf("derive the token prefix: %v", err)
	}
	if prefix.String() != contractTokenPrefix {
		t.Fatalf("PrefixOfSecret = %q, want %q", prefix.String(), contractTokenPrefix)
	}

	expires := contractIdentityEpoch.Add(180 * 24 * time.Hour)
	tok, err := domain.NewAPIToken(domain.NewAPITokenParams{
		ID:        contractTokenID,
		OrgID:     apitest.OrgID,
		UserID:    apitest.UserID,
		Kind:      domain.TokenKindPAT,
		Name:      "laptop CLI",
		Hash:      hash,
		Prefix:    prefix,
		ExpiresAt: &expires,
		CreatedAt: contractIdentityEpoch,
	})
	if err != nil {
		t.Fatalf("build the token: %v", err)
	}

	lastUsed := contractIdentityEpoch.Add(36 * time.Hour)
	tok.LastUsedAt = &lastUsed
	return tok
}

type identityFixture struct {
	svc      *contractIdentityService
	resolver *contractSessionResolver
	c        *apitest.Client
}

// newIdentityFixture mounts the identity router behind the request-id middleware
// AND the real authenticator.
//
// The clock is the REAL one on purpose: `meta.elapsed_ms` is derived from
// `time.Since(started)` and the contract types it as a non-negative int32, so a
// fake epoch would make every success body report an elapsed time of months.
func newIdentityFixture(t *testing.T) *identityFixture {
	t.Helper()

	org, decl := contractOrg(t)
	svc := &contractIdentityService{
		org:   org,
		decl:  decl,
		user:  contractUser(t),
		token: contractToken(t),
	}
	resolver := &contractSessionResolver{}

	rt := NewRouter(Options{
		Service: svc,
		Auth:    authn.NewMiddleware(resolver, contractCookieName),
		Cookie:  DefaultCookieConfig(contractCookieName),
		Clock:   clock.New(),
	})
	return &identityFixture{svc: svc, resolver: resolver, c: apitest.New(rt)}
}

// signedIn sends one request carrying the session cookie.
//
// raw is the body VERBATIM, so the bytes `schema.AssertRequest` validated are the
// bytes the handler decodes.
func (f *identityFixture) signedIn(method, target string, raw []byte) *apitest.Response {
	return f.c.Do(f.request(method, target, raw, true))
}

// anonymous sends the same request with no credential at all.
func (f *identityFixture) anonymous(method, target string) *apitest.Response {
	return f.c.Anonymous().Do(f.request(method, target, nil, false))
}

func (f *identityFixture) request(method, target string, raw []byte, withCookie bool) *http.Request {
	var body io.Reader
	if raw != nil {
		body = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, body)
	if raw != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if withCookie {
		req.AddCookie(&http.Cookie{Name: contractCookieName, Value: contractCookieValue})
	}
	return req
}

// ------------------------------------------------------------- happy paths

// TestGetCurrentPrincipalAnswersTheShapeTheContractDeclares.
//
// The promise: `GET /api/v1/me` returns {data, meta} where data is a `MeDTO` —
// who am I, which org am I in, and how is that org tuned — with
// `meta.request_id` present.
//
// What broke when it did not hold: the audit found operations answering an
// envelope missing `meta` and DTOs missing required members, and a client cannot
// tell which one it is looking at until it crashes on the difference. The settings
// block is what lets the UI explain oto's damping decisions rather than presenting
// them as inexplicable silence, so a `MeDTO` without it is a UI that cannot
// account for itself.
func TestGetCurrentPrincipalAnswersTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	f := newIdentityFixture(t)
	resp := f.signedIn(http.MethodGet, "/me", nil).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getCurrentPrincipal", http.StatusOK, resp.Body())

	if ct := resp.Header("Content-Type"); ct != apitest.ContentTypeJSON {
		t.Fatalf("Content-Type = %q, want %q", ct, apitest.ContentTypeJSON)
	}
	if got := f.svc.counts().me; got != 1 {
		t.Fatalf("the service was consulted %d time(s), want 1", got)
	}

	// ⛔ No credential material anywhere in the body. The session secret is a
	// bearer handle in everything but name, and a response carrying one puts it in
	// every proxy log between here and the browser.
	body := string(resp.Body())
	for _, secret := range []string{contractCookieValue, contractTokenSecret} {
		if strings.Contains(body, secret) {
			t.Fatalf("the /me body carries a credential: %s", resp)
		}
	}
}

// TestLogoutAnswers204WithNoBodyAfterRevokingServerSide.
//
// The promise: `POST /api/v1/auth/logout` revokes the session SERVER-SIDE and
// then clears the cookie, and the contract declares no body for its 204.
//
// What broke: a logout that only clears the cookie leaves a live credential in
// the session table — a client that ignores the header, or a copy of the cookie
// taken earlier, still authenticates. Doing it in the other order is the whole
// difference between "logged out" and "the browser forgot".
func TestLogoutAnswers204WithNoBodyAfterRevokingServerSide(t *testing.T) {
	t.Parallel()

	f := newIdentityFixture(t)
	resp := f.signedIn(http.MethodPost, "/auth/logout", nil).MustStatus(t, http.StatusNoContent)
	schema.AssertNoBody(t, "logout", http.StatusNoContent, resp.Body())

	if got := f.svc.counts().expire; got != 1 {
		t.Fatalf("the session was revoked %d time(s) server-side, want 1", got)
	}
	cookies := resp.Recorder().Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != "" || cookies[0].MaxAge >= 0 {
		t.Fatalf("the cookie was not cleared: %+v", cookies)
	}
}

// ⭐ TestGetOrgSettingsAnswersTheEffectiveTuningWithItsOriginsAndItsShadowedOverrides.
//
// The promise: `GET /api/v1/org/settings` returns the effective value, the origin
// of every key, the config key behind every `config` origin, the org overrides
// configuration is shadowing, and the server-side bounds with their reasons.
//
// What broke: returning the effective value alone is the version of
// configurability that is worse than none. A screen showing `2400` cannot say
// whether this org chose it, the deployment forces it, or oto ships it, and those
// three behave identically today and diverge the moment anything moves. A badge
// reading "managed by configuration" with no key beside it turns a five-second
// edit into an archaeology exercise; a hidden shadowed override is a number in
// Postgres nobody can see that takes effect the moment a config key is deleted.
func TestGetOrgSettingsAnswersTheEffectiveTuningWithItsOriginsAndItsShadowedOverrides(t *testing.T) {
	t.Parallel()

	f := newIdentityFixture(t)
	resp := f.signedIn(http.MethodGet, "/org/settings", nil).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getOrgSettings", http.StatusOK, resp.Body())

	data := dataObject(t, resp)
	settings := childObject(t, resp, data, "settings")
	origins := childObject(t, resp, data, "origins")
	configKeys := childObject(t, resp, data, "config_keys")
	shadowed := childObject(t, resp, data, "shadowed")
	bounds := childObject(t, resp, data, "bounds")

	// Configuration wins over the org's own 1800, and says so.
	if got := numberAt(t, settings, "refire_grace_s"); got != 2400 {
		t.Fatalf("effective refire_grace_s = %v, want the deployment's 2400", got)
	}
	if got, _ := origins["refire_grace_s"].(string); got != "config" {
		t.Fatalf("origins[refire_grace_s] = %q, want config", got)
	}
	if got, _ := configKeys["refire_grace_s"].(string); got != "OTO_TUNING_REFIRE_GRACE_S" {
		t.Fatalf("config_keys[refire_grace_s] = %q; a `config` badge with no key is a wall", got)
	}
	// ⭐ The override that is NOT in force is still visible, and it is the 1800.
	if got := numberAt(t, shadowed, "refire_grace_s"); got != 1800 {
		t.Fatalf("shadowed refire_grace_s = %v, want the org's own 1800", got)
	}

	// A key this org wrote and nothing is forcing reports `org`; one nobody touched
	// reports `default`. Both answers have to be distinguishable or the origin is
	// decoration.
	if got, _ := origins["flap_threshold"].(string); got != "org" {
		t.Fatalf("origins[flap_threshold] = %q, want org", got)
	}
	if got, _ := origins["resolve_grace_s"].(string); got != "default" {
		t.Fatalf("origins[resolve_grace_s] = %q, want default", got)
	}
	// A key configuration manages but the org never wrote is not shadowing anything.
	if _, present := shadowed["raw_retention_days"]; present {
		t.Fatalf("shadowed names raw_retention_days, which this org never overrode: %s", resp)
	}

	// The bounds carry their REASON, because a caller told only "invalid" tries a
	// different wrong number.
	bound := childObject(t, resp, bounds, "refire_grace_s")
	if why, _ := bound["why"].(string); strings.TrimSpace(why) == "" {
		t.Fatalf("bounds[refire_grace_s].why is empty; the form cannot explain the range: %s", resp)
	}
	if got := f.svc.counts().getOrg; got != 1 {
		t.Fatalf("the service was consulted %d time(s), want 1", got)
	}
}

// TestUpdateOrgSettingsStoresThePartialWriteAndReturnsTheNewView.
//
// The promise: `PATCH /api/v1/org/settings` is a PARTIAL write — an omitted key
// is left alone, and a key named in `reset` returns to oto's shipped default and
// reports origin `default` again. The 200 is the same `OrgSettingsViewResponse`
// the read returns, so a client never has to re-read to find out what it did.
//
// What broke: a settings API where an omitted key silently reverts to the default
// reverts nine settings every time somebody changes one.
func TestUpdateOrgSettingsStoresThePartialWriteAndReturnsTheNewView(t *testing.T) {
	t.Parallel()

	raw := []byte(contractSettingsPatchBody)
	schema.AssertRequest(t, "updateOrgSettings", raw)

	f := newIdentityFixture(t)
	resp := f.signedIn(http.MethodPatch, "/org/settings", raw).MustStatus(t, http.StatusOK)
	schema.Assert(t, "updateOrgSettings", http.StatusOK, resp.Body())

	// The service saw the write as a patch plus a reset list, not as a whole
	// settings object.
	f.svc.mu.Lock()
	patched, cleared := f.svc.patched, f.svc.cleared
	f.svc.mu.Unlock()

	if patched.StormThreshold == nil || *patched.StormThreshold != 60 {
		t.Fatalf("the service received storm_threshold %v, want 60", patched.StormThreshold)
	}
	if patched.ResolveGraceS != nil {
		t.Fatalf("an omitted key arrived as a write (%v); omission means leave it alone",
			*patched.ResolveGraceS)
	}
	if len(cleared) != 1 || cleared[0] != domain.KeyFlapThreshold {
		t.Fatalf("reset = %v, want [flap_threshold]", cleared)
	}

	data := dataObject(t, resp)
	settings := childObject(t, resp, data, "settings")
	origins := childObject(t, resp, data, "origins")

	if got := numberAt(t, settings, "storm_threshold"); got != 60 {
		t.Fatalf("storm_threshold = %v after the write, want 60", got)
	}
	if got, _ := origins["storm_threshold"].(string); got != "org" {
		t.Fatalf("origins[storm_threshold] = %q after the org wrote it, want org", got)
	}
	// ⛔ The reset key is back to oto's default, in the value AND in the origin. An
	// origin that still said `org` would be a screen that cannot tell an operator
	// their reset happened.
	if got := numberAt(t, settings, "flap_threshold"); got != float64(domain.DefaultFlapThreshold) {
		t.Fatalf("flap_threshold = %v after a reset, want oto's default %d",
			got, domain.DefaultFlapThreshold)
	}
	if got, _ := origins["flap_threshold"].(string); got != "default" {
		t.Fatalf("origins[flap_threshold] = %q after a reset, want default", got)
	}
}

// ⚠️ TestCreateApiTokenReturnsThePlaintextSecretExactlyOnceAndNowhereElse.
//
// The promise: the 201 carries the token AND its secret, and it is the ONLY
// response in oto that ever will. Only a sha256 is stored, so a lost token is
// replaced rather than recovered.
//
// What broke: a secret that leaks into a second response leaks into every proxy
// log, every browser history entry and every screenshot of a token list. This
// asserts both halves — that it IS there once on the create, and that the list and
// `/me` cannot be made to show it.
func TestCreateApiTokenReturnsThePlaintextSecretExactlyOnceAndNowhereElse(t *testing.T) {
	t.Parallel()

	raw := []byte(contractCreateTokenBody)
	schema.AssertRequest(t, "createApiToken", raw)

	f := newIdentityFixture(t)
	resp := f.signedIn(http.MethodPost, "/api-tokens", raw).MustStatus(t, http.StatusCreated)
	schema.Assert(t, "createApiToken", http.StatusCreated, resp.Body())

	created := string(resp.Body())
	if n := strings.Count(created, contractTokenSecret); n != 1 {
		t.Fatalf("the secret appears %d time(s) in the 201; it is disclosed exactly once: %s", n, resp)
	}

	// It is on the response, and it is NOT on the token object beside it —
	// `APITokenDTO` has no field that could hold one.
	data := dataObject(t, resp)
	if got, _ := data["secret"].(string); got != contractTokenSecret {
		t.Fatalf("data.secret = %q, want the minted secret: %s", got, resp)
	}
	token := childObject(t, resp, data, "token")
	tokenJSON, err := json.Marshal(token)
	if err != nil {
		t.Fatalf("re-encode the token: %v", err)
	}
	if strings.Contains(string(tokenJSON), contractTokenSecret) {
		t.Fatalf("the token object carries the plaintext: %s", tokenJSON)
	}

	// The request reached the service unmangled: the name as typed, and an expiry
	// normalised to UTC.
	f.svc.mu.Lock()
	issued := f.svc.issued
	f.svc.mu.Unlock()
	if issued.Name != "laptop CLI" {
		t.Fatalf("the service received name %q, want %q", issued.Name, "laptop CLI")
	}
	if issued.ExpiresAt == nil || issued.ExpiresAt.Location() != time.UTC {
		t.Fatalf("the service received expires_at %v; it must arrive as UTC", issued.ExpiresAt)
	}

	// ⛔ And nowhere else. Both of the other bodies that mention this token are
	// searched for the plaintext.
	list := f.signedIn(http.MethodGet, "/api-tokens", nil).MustStatus(t, http.StatusOK)
	schema.Assert(t, "listApiTokens", http.StatusOK, list.Body())
	me := f.signedIn(http.MethodGet, "/me", nil).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getCurrentPrincipal", http.StatusOK, me.Body())

	for _, other := range []*apitest.Response{list, me} {
		if strings.Contains(string(other.Body()), contractTokenSecret) {
			t.Fatalf("a second response echoes the one-time secret: %s", other)
		}
	}
	// The list still identifies the credential, by its public prefix. That is the
	// whole reason the column exists.
	if !strings.Contains(string(list.Body()), contractTokenPrefix) {
		t.Fatalf("the token list shows no prefix, so two tokens cannot be told apart: %s", list)
	}
}

// TestRevokeApiTokenAnswers204WithNoBody.
//
// The promise: `DELETE /api/v1/api-tokens/{id}` answers 204 and the contract
// declares no body for it.
//
// What broke: a 204 that writes bytes is a response no HTTP client agrees about —
// some surface the body, some discard it, and a schema that says "no body" is the
// only thing that notices.
func TestRevokeApiTokenAnswers204WithNoBody(t *testing.T) {
	t.Parallel()

	f := newIdentityFixture(t)
	resp := f.signedIn(http.MethodDelete, "/api-tokens/"+contractTokenID.String(), nil).
		MustStatus(t, http.StatusNoContent)
	schema.AssertNoBody(t, "revokeApiToken", http.StatusNoContent, resp.Body())

	if got := f.svc.counts().revoke; got != 1 {
		t.Fatalf("the service was asked to revoke %d time(s), want 1", got)
	}
}

// --------------------------------------------------------------- boundaries

// ⛔ TestAnApiTokenOutsideTheCallersTenantIsANotFound.
//
// The promise: every id-addressed operation answers 404 for an id it does not own
// — never 403, never another tenant's row, never a 500.
//
// What broke: a 403 confirms the id exists somewhere, which turns the id space
// into a cross-tenant existence oracle. v1's only cause of 403 is cross-org
// access, which is precisely the case this must not distinguish from "no such
// thing". The repository runs `WHERE org_id = $scope AND id = $id`, so a stranger's
// token matches no row; this proves the transport surfaces that as a 404 and says
// nothing about the owning org while doing it.
func TestAnApiTokenOutsideTheCallersTenantIsANotFound(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
		// reached says whether the id was well-formed enough to be looked up at all.
		reached bool
	}{
		{
			name:    "an id owned by another org",
			path:    "/api-tokens/" + apitest.StrangerID.String(),
			reached: true,
		},
		{
			name: "an id that is not a uuid at all",
			path: "/api-tokens/banana",
		},
		{
			name: "the nil uuid",
			path: "/api-tokens/00000000-0000-0000-0000-000000000000",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newIdentityFixture(t)
			resp := f.signedIn(http.MethodDelete, tc.path, nil).MustStatus(t, http.StatusNotFound)
			schema.AssertProblem(t, "revokeApiToken", http.StatusNotFound, resp.Body())

			if code := resp.Problem(t).Code; code != "not_found" {
				t.Fatalf("code = %q, want not_found", code)
			}
			if ct := resp.Header("Content-Type"); !strings.Contains(ct, "problem+json") {
				t.Fatalf("Content-Type = %q, want application/problem+json", ct)
			}
			// ⚠️ The refusal says nothing about the other tenant.
			body := string(resp.Body())
			for _, leak := range []string{apitest.OtherOrgID.String(), contractTokenID.String()} {
				if strings.Contains(body, leak) {
					t.Fatalf("the 404 names %s: %s", leak, resp)
				}
			}

			want := 0
			if tc.reached {
				want = 1
			}
			if got := f.svc.counts().revoke; got != want {
				t.Fatalf("the service saw %d revoke call(s), want %d", got, want)
			}
		})
	}
}

// TestUpdateOrgSettingsRefusesAResetNamingAKeyThatDoesNotExist.
//
// The promise: `422 invalid_org_settings`, with `violations[]` naming `reset` and
// carrying a machine code a form can branch on.
//
// What broke: a typo'd key that is silently dropped is a reset the operator
// believes happened and did not — they walk away thinking `refire_grace_s` is back
// to oto's default while their old override is still in force. A refusal carrying
// only prose is barely better: a form with nothing to highlight sends the caller
// back to guess a different wrong value.
func TestUpdateOrgSettingsRefusesAResetNamingAKeyThatDoesNotExist(t *testing.T) {
	t.Parallel()

	raw := []byte(contractSettingsBadResetBody)
	// ⭐ The CONTRACT permits this body — `reset` is an array of bounded strings
	// with no enum — so the refusal is genuinely the server's to make.
	schema.AssertRequest(t, "updateOrgSettings", raw)

	f := newIdentityFixture(t)
	resp := f.signedIn(http.MethodPatch, "/org/settings", raw).
		MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "updateOrgSettings", http.StatusUnprocessableEntity, resp.Body())

	p := resp.MustViolate(t, "reset")
	if p.Code != "invalid_org_settings" {
		t.Fatalf("code = %q, want invalid_org_settings", p.Code)
	}
	if !strings.Contains(violationMessage(t, p, "reset"), "refire_grace") {
		t.Fatalf("the violation does not name the key that was rejected: %+v", p.Violations)
	}
	if got := f.svc.counts().update; got != 0 {
		t.Fatalf("a refused write still reached the service (%d call(s))", got)
	}
}

// TestCreateApiTokenRefusesANameThatIsOnlyWhitespace.
//
// The promise: `422 validation_failed`, with `violations[]` naming `name` and
// carrying the `not_blank` code.
//
// What broke: present is not the same as meaningful. A token called "   " is a row
// in the list an operator cannot tell from any other, on a screen whose entire
// purpose is telling credentials apart before revoking one — and the contract's
// `minLength: 1` cannot catch it, which is exactly why the server checks.
func TestCreateApiTokenRefusesANameThatIsOnlyWhitespace(t *testing.T) {
	t.Parallel()

	raw := []byte(contractCreateTokenBlankNameBody)
	// The contract permits three spaces; `notblank` is oto's own rule on top.
	schema.AssertRequest(t, "createApiToken", raw)

	f := newIdentityFixture(t)
	resp := f.signedIn(http.MethodPost, "/api-tokens", raw).
		MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "createApiToken", http.StatusUnprocessableEntity, resp.Body())

	p := resp.MustViolate(t, "name")
	if p.Status != http.StatusUnprocessableEntity {
		t.Fatalf("the problem says %d, the status line said 422", p.Status)
	}
	if code := violationCode(t, p, "name"); code != "not_blank" {
		t.Fatalf("violations[name].code = %q, want not_blank", code)
	}
	if got := f.svc.counts().issue; got != 0 {
		t.Fatalf("a refused request still minted %d token(s)", got)
	}
}

// TestAnUnauthenticatedCallerGetsTheContractsProblemOnEveryIdentityRoute.
//
// The promise: with no credential there is no tenant, so there is nothing to read
// and nothing to write — one code, one shape, on every route.
//
// What broke: each of these handlers re-derives its scope from `authn.Scope`
// rather than trusting that a middleware ran, because "the middleware guarantees
// it" stops being true the first time a route is mounted somewhere else. This
// asserts the 401 the CONTRACT declares for each operation, not merely that the
// status was 401.
func TestAnUnauthenticatedCallerGetsTheContractsProblemOnEveryIdentityRoute(t *testing.T) {
	t.Parallel()

	routes := []struct {
		op     string
		method string
		path   string
	}{
		{"getCurrentPrincipal", http.MethodGet, "/me"},
		{"getOrgSettings", http.MethodGet, "/org/settings"},
		{"updateOrgSettings", http.MethodPatch, "/org/settings"},
		{"logout", http.MethodPost, "/auth/logout"},
		{"listApiTokens", http.MethodGet, "/api-tokens"},
		{"createApiToken", http.MethodPost, "/api-tokens"},
		{"revokeApiToken", http.MethodDelete, "/api-tokens/" + contractTokenID.String()},
	}

	for _, route := range routes {
		t.Run(route.op, func(t *testing.T) {
			t.Parallel()

			f := newIdentityFixture(t)
			resp := f.anonymous(route.method, route.path).MustStatus(t, http.StatusUnauthorized)
			schema.AssertProblem(t, route.op, http.StatusUnauthorized, resp.Body())

			if code := resp.Problem(t).Code; code != "unauthenticated" {
				t.Fatalf("code = %q, want unauthenticated", code)
			}
			if h := resp.Header("Set-Cookie"); h != "" {
				t.Fatalf("a refused request was handed a cookie: %q", h)
			}
			if got := f.svc.counts(); got != (identityCalls{}) {
				t.Fatalf("an unauthenticated request reached the service: %+v", got)
			}
		})
	}
}

// ⛔ TestBUG_ARetriedCreateApiTokenWithTheSameIdempotencyKeyMintsASecondCredential.
//
// The contract declares `Idempotency-Key` on `createApiToken` and on
// `revokeApiToken`, and states what it means: "Replaying the same key with the
// same body within the retention window returns the original result rather than
// acting twice; replaying it with a *different* body is a `409`."
//
// Nothing in oto reads the header. `internal/platform/httpx/middleware` allows it
// through CORS and that is the only mention of it in the process, so a client that
// retries a create — a dropped response, a proxy timeout, a user double-clicking
// "generate" — gets a SECOND live credential it never learns the secret of, and no
// way to tell that from the first attempt having worked. That is the exact failure
// an idempotency key exists to prevent, on the one endpoint in oto that hands out
// credentials.
func TestBUG_ARetriedCreateApiTokenWithTheSameIdempotencyKeyMintsASecondCredential(t *testing.T) {
	t.Skip("BUG: Idempotency-Key is declared on createApiToken and revokeApiToken " +
		"(api/openapi/openapi.yaml, parameters/IdempotencyKeyHeader) and is read by nothing " +
		"(identity/api/tokens.go createAPIToken, revokeAPIToken); a retried create must replay the " +
		"original 201 rather than minting a second credential, and the same key with a different body " +
		"must be a 409.")

	raw := []byte(contractCreateTokenBody)
	schema.AssertRequest(t, "createApiToken", raw)

	f := newIdentityFixture(t)
	const key = "01JD8Z2K7M3TQ9YB4V6H0XW5RE"

	send := func() *apitest.Response {
		req := f.request(http.MethodPost, "/api-tokens", raw, true)
		req.Header.Set("Idempotency-Key", key)
		return f.c.Do(req)
	}

	first := send().MustStatus(t, http.StatusCreated)
	schema.Assert(t, "createApiToken", http.StatusCreated, first.Body())
	second := send().MustStatus(t, http.StatusCreated)
	schema.Assert(t, "createApiToken", http.StatusCreated, second.Body())

	if got := f.svc.counts().issue; got != 1 {
		t.Fatalf("the same Idempotency-Key minted %d credential(s), want 1", got)
	}
}

// TestTheDeclaredIdentityOperationsAreTheOnesThisPackageServes guards the failure
// mode that made the coverage ratchet necessary: an operation declared in the
// contract that no test ever names, because the list of what to test was kept by
// hand.
func TestTheDeclaredIdentityOperationsAreTheOnesThisPackageServes(t *testing.T) {
	t.Parallel()

	for id, want := range map[string]int{
		"getCurrentPrincipal": http.StatusOK,
		"getOrgSettings":      http.StatusOK,
		"updateOrgSettings":   http.StatusOK,
		"listApiTokens":       http.StatusOK,
		"createApiToken":      http.StatusCreated,
		"logout":              http.StatusNoContent,
		"revokeApiToken":      http.StatusNoContent,
	} {
		op := schema.Op(t, id)
		if op.SuccessStatus() != want {
			t.Fatalf("%s declares success %d, and this file asserts %d", id, op.SuccessStatus(), want)
		}
		if !op.Declares(http.StatusUnauthorized) {
			t.Fatalf("%s declares no 401, but every identity route can produce one", id)
		}
	}

	// ⛔ Only ONE operation in this package addresses a row by id, and it is the
	// one the tenant probe above covers. A second would need its own.
	for _, id := range []string{"getCurrentPrincipal", "getOrgSettings", "updateOrgSettings",
		"listApiTokens", "createApiToken", "logout"} {
		if op := schema.Op(t, id); op.HasPathParam("id") {
			t.Fatalf("%s has grown an {id}; it needs a tenant-scoping probe", id)
		}
	}
	if !schema.Op(t, "revokeApiToken").HasPathParam("id") {
		t.Fatal("revokeApiToken no longer takes an {id}; the tenant probe above is testing nothing")
	}
}

// ------------------------------------------------------------------ helpers

func dataObject(t *testing.T, resp *apitest.Response) map[string]any {
	t.Helper()

	data, ok := resp.JSON(t)["data"].(map[string]any)
	if !ok {
		t.Fatalf("the body carries no data object: %s", resp)
	}
	return data
}

func childObject(t *testing.T, resp *apitest.Response, parent map[string]any, name string) map[string]any {
	t.Helper()

	child, ok := parent[name].(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object: %s", name, resp)
	}
	return child
}

func numberAt(t *testing.T, obj map[string]any, name string) float64 {
	t.Helper()

	v, ok := obj[name].(float64)
	if !ok {
		t.Fatalf("%s = %v, want a number", name, obj[name])
	}
	return v
}

func violationCode(t *testing.T, p apitest.Problem, field string) string {
	t.Helper()

	for _, v := range p.Violations {
		if v.Field == field {
			return v.Code
		}
	}
	t.Fatalf("violations[] names %v, want an entry for %q", p.Fields(), field)
	return ""
}

func violationMessage(t *testing.T, p apitest.Problem, field string) string {
	t.Helper()

	for _, v := range p.Violations {
		if v.Field == field {
			return v.Message
		}
	}
	t.Fatalf("violations[] names %v, want an entry for %q", p.Fields(), field)
	return ""
}

func intPtrOf(v int) *int       { return &v }
func strPtrOf(v string) *string { return &v }
func boolPtrOf(v bool) *bool    { return &v }
