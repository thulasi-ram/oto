package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/platform/ratelimit"
)

// The transport half of the authentication surface: which credential is admitted
// on which route, and what bounds the one endpoint that runs argon2id for
// anybody who can reach it.
//
// ⭐ THE RESOLVER IN THESE TESTS ANSWERS "YES" TO EVERYTHING. That is deliberate.
// The properties under test are refusals the middleware makes BEFORE any lookup —
// an ingest token anywhere on the API, a PAT on a session-only route, a
// non-bearer Authorization header — and a resolver that could say no would let
// those tests pass for the wrong reason. Every one of them therefore asserts the
// refusal AND that the resolver was never consulted.
//
// Nothing here sleeps: the limiter runs on a `clock.Fake`.

const (
	testCookieName = "oto_session"
	testPATSecret  = domain.SecretPrefixPAT + "Ab3Dxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	testIngestSec  = domain.SecretPrefixIngest + "Wx9Zyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy"
	testCookieVal  = "svqk3m9x7t2p8w4r6y5n1c0badefghijklmnopqrs"
)

var testEpoch = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// ------------------------------------------------------------------- doubles

// spyResolver authenticates ANYTHING it is handed, and counts which door the
// credential came through.
type spyResolver struct {
	mu       sync.Mutex
	bearer   []string
	sessions []string

	// deny makes the resolver refuse, for the tests that need the failure path.
	deny bool
}

func (s *spyResolver) ResolveBearer(_ context.Context, secret string) (authn.Principal, error) {
	s.mu.Lock()
	s.bearer = append(s.bearer, secret)
	s.mu.Unlock()
	if s.deny {
		return authn.Principal{}, authn.Unauthenticated()
	}
	return authn.Principal{
		Kind: authn.KindPAT, OrgID: testOrgID, UserID: testUserID, TokenID: id.New(),
	}, nil
}

func (s *spyResolver) ResolveSession(_ context.Context, secret string) (authn.Principal, error) {
	s.mu.Lock()
	s.sessions = append(s.sessions, secret)
	s.mu.Unlock()
	if s.deny {
		return authn.Principal{}, authn.Unauthenticated()
	}
	return authn.Principal{
		Kind: authn.KindSession, OrgID: testOrgID, UserID: testUserID, SessionID: id.New(),
	}, nil
}

func (s *spyResolver) counts() (bearer, session int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bearer), len(s.sessions)
}

var (
	testOrgID  = uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	testUserID = uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
)

// spyService is the identity service the router calls into.
type spyService struct {
	mu sync.Mutex

	logins     int
	loginErr   error
	loggedOut  int
	sessionTTL time.Duration
}

func (s *spyService) Login(_ context.Context, _ service.LoginCommand) (service.LoginResult, error) {
	s.mu.Lock()
	s.logins++
	err := s.loginErr
	s.mu.Unlock()
	if err != nil {
		return service.LoginResult{}, err
	}

	sessionID := id.New()
	expires := testEpoch.Add(s.ttl())
	return service.LoginResult{
		Principal: authn.Principal{
			Kind: authn.KindSession, OrgID: testOrgID, UserID: testUserID,
			SessionID: sessionID, ExpiresAt: expires,
		},
		Session: service.IssuedSession{
			Session: domain.Session{
				ID: sessionID, OrgID: testOrgID, UserID: testUserID,
				CreatedAt: testEpoch, ExpiresAt: expires,
			},
			Secret: testCookieVal,
		},
	}, nil
}

func (s *spyService) ttl() time.Duration {
	if s.sessionTTL <= 0 {
		return 24 * time.Hour
	}
	return s.sessionTTL
}

func (s *spyService) loginCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins
}

func (s *spyService) ExpireSession(context.Context, db.TenantScope, authn.Principal) error {
	s.mu.Lock()
	s.loggedOut++
	s.mu.Unlock()
	return nil
}

func (s *spyService) Me(_ context.Context, _ db.TenantScope, p authn.Principal) (service.MeView, error) {
	email, err := domain.NewEmail("priya@example.test")
	if err != nil {
		return service.MeView{}, err
	}
	user := domain.User{ID: testUserID, OrgID: testOrgID, Email: email, DisplayName: "Priya"}
	return service.MeView{
		Principal: p,
		Org:       domain.Org{ID: testOrgID, Slug: "acme", Name: "Acme", Settings: domain.DefaultSettings()},
		User:      &user,
	}, nil
}

func (s *spyService) GetOrg(context.Context, db.TenantScope) (domain.Org, error) {
	return domain.Org{ID: testOrgID, Slug: "acme", Name: "Acme", Settings: domain.DefaultSettings()}, nil
}

func (s *spyService) UpdateOrgSettings(
	context.Context, db.TenantScope, domain.SettingsPatch, []domain.SettingKey,
) (domain.Org, error) {
	return s.GetOrg(context.Background(), db.TenantScope{})
}

func (s *spyService) ListTokens(
	context.Context, db.TenantScope, authn.Principal, db.Keyset,
) ([]domain.APIToken, db.Cursor, error) {
	return nil, db.Cursor{}, nil
}

func (s *spyService) IssueToken(
	context.Context, db.TenantScope, authn.Principal, service.CreateTokenCommand,
) (service.IssuedToken, error) {
	return service.IssuedToken{}, errs.Internal("not_reached_in_these_tests", nil)
}

func (s *spyService) RevokeToken(context.Context, db.TenantScope, uuid.UUID) error { return nil }

// closedGate is a LoginGate with nothing free, for the shedding test.
type closedGate struct{}

func (closedGate) Acquire() bool { return false }
func (closedGate) Release()      {}

// ------------------------------------------------------------------- fixture

type apiFixture struct {
	router   http.Handler
	svc      *spyService
	resolver *spyResolver
	limiter  *ratelimit.Limiter
	clk      *clock.Fake
}

type apiOptions struct {
	limiter *ratelimit.Limiter
	gate    LoginGate
}

func newAPIFixture(t *testing.T, o apiOptions) *apiFixture {
	t.Helper()

	clk := clock.NewFake(testEpoch)
	svc := &spyService{}
	resolver := &spyResolver{}

	rt := NewRouter(Options{
		Service: svc,
		Auth:    authn.NewMiddleware(resolver, testCookieName),
		Cookie:  DefaultCookieConfig(testCookieName),
		Limiter: limiterOrNil(o.limiter),
		Gate:    o.gate,
		Clock:   clk,
	})

	r := chi.NewRouter()
	rt.Mount(r)

	return &apiFixture{router: r, svc: svc, resolver: resolver, limiter: o.limiter, clk: clk}
}

// limiterOrNil keeps a nil *Limiter out of the non-nil interface value that a
// plain assignment would produce — the router checks `rt.limiter != nil`.
func limiterOrNil(l *ratelimit.Limiter) LoginLimiter {
	if l == nil {
		return nil
	}
	return l
}

func (f *apiFixture) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// postLogin sends a well-formed login body from one client address.
func (f *apiFixture) postLogin(from string) *httptest.ResponseRecorder {
	body := `{"email":"priya@example.test","password":"correct-horse-battery-staple"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if from != "" {
		req.RemoteAddr = from
	}
	return f.do(req)
}

// problemCode reads the stable machine code out of a problem+json body.
func problemCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "problem+json") {
		t.Fatalf("Content-Type = %q, want application/problem+json: %s", ct, rec.Body.String())
	}
	var p errs.ProblemDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v (%s)", err, rec.Body.String())
	}
	return p.Code
}

// requireUnauthenticated asserts the ONE answer every credential failure gives.
func requireUnauthenticated(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	if code := problemCode(t, rec); code != "unauthenticated" {
		t.Fatalf("code = %q, want unauthenticated", code)
	}
	if h := rec.Header().Get("Set-Cookie"); h != "" {
		t.Fatalf("a refused request was handed a cookie: %q", h)
	}
}

// ------------------------------------------------------------- rate limiting

// ⭐ TestTheLoginLimiterTripsAndRecovers.
//
// Both halves matter. A limiter that never trips leaves the most expensive
// unauthenticated endpoint in the process — argon2id at 19 MiB on EVERY path,
// including the dummy — open to anybody who can reach it. A limiter that never
// lets a legitimate user back in is its own outage, and is the reason the second
// half of this test exists at all.
func TestTheLoginLimiterTripsAndRecovers(t *testing.T) {
	t.Parallel()

	clk := clock.NewFake(testEpoch)
	f := newAPIFixture(t, apiOptions{
		limiter: ratelimit.New(ratelimit.Config{Burst: 3, Refill: 12 * time.Second, Clock: clk}),
	})
	f.svc.loginErr = errs.Unauthorized("invalid_credentials", "email or password is incorrect")

	for i := range 3 {
		rec := f.postLogin("")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401 inside the burst", i+1, rec.Code)
		}
	}
	if got := f.svc.loginCount(); got != 3 {
		t.Fatalf("the service saw %d attempts, want 3", got)
	}

	// ⛔ Tripped. The fourth attempt never reaches the password verifier.
	rec := f.postLogin("")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 once the burst is spent", rec.Code)
	}
	if code := problemCode(t, rec); code != "too_many_login_attempts" {
		t.Fatalf("code = %q, want too_many_login_attempts", code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After = %q; a refusal must carry a delay the client can act on", ra)
	}
	if got := f.svc.loginCount(); got != 3 {
		t.Fatalf("a rate-limited request still ran a password verification (%d calls)", got)
	}
	// ⚠️ The refusal says nothing about accounts. A limiter that answered
	// differently for a known and an unknown address would reintroduce the
	// enumeration oracle the uniform 401 exists to close.
	if body := rec.Body.String(); strings.Contains(body, "priya@example.test") {
		t.Fatalf("the 429 echoes the submitted address: %s", body)
	}

	// Another client is unaffected: one noisy address must not lock out the org.
	if other := f.postLogin("198.51.100.7:5555"); other.Code != http.StatusUnauthorized {
		t.Fatalf("a different client got %d; the budget is per address", other.Code)
	}

	// ⭐ RECOVERY. One refill interval later there is one token again.
	clk.Advance(12 * time.Second)
	rec = f.postLogin("")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d after a refill interval; a limiter that never reopens is an outage", rec.Code)
	}
	if got := f.svc.loginCount(); got != 5 {
		t.Fatalf("the service saw %d attempts, want 5 after recovery", got)
	}

	// ...and exactly one: the refill is a drip, not a reset.
	if rec := f.postLogin(""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d; one interval buys one attempt", rec.Code)
	}
}

// ⭐ TestASuccessfulLoginClearsTheBucket — somebody who mistyped their password
// three times and then got it right must not be locked out by their own typos.
func TestASuccessfulLoginClearsTheBucket(t *testing.T) {
	t.Parallel()

	clk := clock.NewFake(testEpoch)
	f := newAPIFixture(t, apiOptions{
		limiter: ratelimit.New(ratelimit.Config{Burst: 4, Refill: time.Minute, Clock: clk}),
	})

	f.svc.loginErr = errs.Unauthorized("invalid_credentials", "email or password is incorrect")
	for range 3 {
		if rec := f.postLogin(""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	}

	// The fourth attempt is the last in the burst, and it works.
	f.svc.loginErr = nil
	if rec := f.postLogin(""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// Without the Reset the bucket would be empty and this would be a 429.
	f.svc.loginErr = errs.Unauthorized("invalid_credentials", "email or password is incorrect")
	for i := range 4 {
		if rec := f.postLogin(""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d after a successful login: status = %d, want 401 — "+
				"a success must clear the caller's bucket", i+1, rec.Code)
		}
	}
}

// TestTheLimiterIsCheckedBeforeTheBodyIsBound — a refused caller must not get to
// allocate, and must not be able to tell a rate limit from a bad body.
func TestTheLimiterIsCheckedBeforeTheBodyIsBound(t *testing.T) {
	t.Parallel()

	clk := clock.NewFake(testEpoch)
	f := newAPIFixture(t, apiOptions{
		limiter: ratelimit.New(ratelimit.Config{Burst: 1, Refill: time.Minute, Clock: clk}),
	})
	f.svc.loginErr = errs.Unauthorized("invalid_credentials", "email or password is incorrect")

	if rec := f.postLogin(""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := f.do(req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: the limit is checked before the body is read", rec.Code)
	}
}

// ⭐ TestTheConcurrencyGateShedsRatherThanQueues. The limiter bounds the RATE per
// address; the gate bounds the resident memory of everything in flight, which is
// what argon2id at 19 MiB per concurrent evaluation actually costs.
func TestTheConcurrencyGateShedsRatherThanQueues(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t, apiOptions{gate: closedGate{}})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- f.postLogin("") }()

	select {
	case rec := <-done:
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429 when the gate is full", rec.Code)
		}
		if code := problemCode(t, rec); code != "login_busy" {
			t.Fatalf("code = %q, want login_busy", code)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Fatal("a shed request must carry a Retry-After")
		}
		if got := f.svc.loginCount(); got != 0 {
			t.Fatalf("a shed request still ran %d password verification(s)", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the login handler blocked on a full gate; it must shed immediately")
	}
}

// ------------------------------------------------------------------- cookies

// ⛔ TestAFailedLoginSetsNoCookie. A Set-Cookie on a failure hands out a working
// credential with a response that says the login did not happen.
func TestAFailedLoginSetsNoCookie(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t, apiOptions{})
	f.svc.loginErr = errs.Unauthorized("invalid_credentials", "email or password is incorrect")

	rec := f.postLogin("")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if code := problemCode(t, rec); code != "invalid_credentials" {
		t.Fatalf("code = %q, want invalid_credentials", code)
	}
	if h := rec.Header().Get("Set-Cookie"); h != "" {
		t.Fatalf("a failed login set a cookie: %q", h)
	}
	if body := rec.Body.String(); strings.Contains(body, "priya@example.test") {
		t.Fatalf("the 401 echoes the address back: %s", body)
	}
}

// ⭐ TestASuccessfulLoginSetsTheHardenedCookie. Each flag is load-bearing:
// HttpOnly (one XSS is not a stolen session), Secure (never in clear text),
// SameSite=Lax (a cross-site POST cannot ack, snooze or mint), and an expiry
// taken from the SERVER's row so the browser and the database cannot disagree.
func TestASuccessfulLoginSetsTheHardenedCookie(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t, apiOptions{})

	rec := f.postLogin("")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies set = %d, want exactly 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != testCookieName || c.Value != testCookieVal {
		t.Fatalf("cookie %s=%q is not the session secret", c.Name, c.Value)
	}
	if !c.HttpOnly {
		t.Fatal("the session cookie is readable from JavaScript")
	}
	if !c.Secure {
		t.Fatal("the session cookie may travel in clear text")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Fatalf("Path = %q, want /", c.Path)
	}
	if want := int((24 * time.Hour).Seconds()); c.MaxAge != want {
		t.Fatalf("Max-Age = %d, want %d — it comes from the minted row, not from a constant",
			c.MaxAge, want)
	}

	// ⛔ The secret is in the header and NOWHERE ELSE. A response body carrying it
	// would put it in every proxy log between here and the browser.
	if strings.Contains(rec.Body.String(), testCookieVal) {
		t.Fatalf("the session secret is in the response body: %s", rec.Body.String())
	}
}

// TestLogoutRevokesServerSideThenClearsTheCookie — a client that ignores the
// header still holds a credential that resolves to nothing.
func TestLogoutRevokesServerSideThenClearsTheCookie(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t, apiOptions{})

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: testCookieName, Value: testCookieVal})
	rec := f.do(req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if f.svc.loggedOut != 1 {
		t.Fatalf("the session was revoked %d time(s) server-side", f.svc.loggedOut)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != "" || cookies[0].MaxAge >= 0 {
		t.Fatalf("the cookie was not cleared: %+v", cookies)
	}
}

// --------------------------------------------------------- credential kinds

// sessionOnlyRoutes are the contract's `security: [sessionCookie]` group. A token
// must not be able to enumerate, mint or revoke its own siblings, and must not be
// able to retune the org whose alerts it can read.
func sessionOnlyRoutes() []struct{ method, path string } {
	return []struct{ method, path string }{
		{http.MethodPost, "/auth/logout"},
		{http.MethodGet, "/api-tokens"},
		{http.MethodPost, "/api-tokens"},
		{http.MethodDelete, "/api-tokens/" + uuid.NewString()},
		{http.MethodPatch, "/org/settings"},
	}
}

// sharedRoutes accept either credential kind.
func sharedRoutes() []struct{ method, path string } {
	return []struct{ method, path string }{
		{http.MethodGet, "/me"},
		{http.MethodGet, "/org/settings"},
	}
}

// ⛔ TestAPATIsRefusedOnEverySessionOnlyRouteWithoutALookup is v1's single
// privilege boundary, and it is a boundary between CREDENTIAL KINDS rather than
// between roles.
func TestAPATIsRefusedOnEverySessionOnlyRouteWithoutALookup(t *testing.T) {
	t.Parallel()

	for _, route := range sessionOnlyRoutes() {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t, apiOptions{})
			req := httptest.NewRequest(route.method, route.path, strings.NewReader(`{"name":"x"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testPATSecret)
			rec := f.do(req)

			requireUnauthenticated(t, rec)

			// ⭐ Refused on the header alone. The resolver — which in this fixture
			// authenticates anything — was never asked.
			if bearer, session := f.resolver.counts(); bearer != 0 || session != 0 {
				t.Fatalf("the credential store was consulted (%d bearer, %d session lookups) "+
					"for a credential kind this route does not accept", bearer, session)
			}
		})
	}
}

// TestASessionCookieIsAcceptedOnTheSessionOnlyRoutes is the other half: the
// boundary refuses the wrong kind without refusing the right one.
func TestASessionCookieIsAcceptedOnTheSessionOnlyRoutes(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t, apiOptions{})
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: testCookieName, Value: testCookieVal})
	rec := f.do(req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: a session cookie is exactly what this route wants", rec.Code)
	}
	if bearer, session := f.resolver.counts(); bearer != 0 || session != 1 {
		t.Fatalf("resolver calls: %d bearer, %d session; want 0 and 1", bearer, session)
	}
}

// TestBothKindsAreAcceptedOnTheSharedRoutes — the boundary is not "refuse
// everything", which is the other way to pass the tests above.
func TestBothKindsAreAcceptedOnTheSharedRoutes(t *testing.T) {
	t.Parallel()

	for _, route := range sharedRoutes() {
		t.Run(route.path, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t, apiOptions{})

			withPAT := httptest.NewRequest(route.method, route.path, nil)
			withPAT.Header.Set("Authorization", "Bearer "+testPATSecret)
			if rec := f.do(withPAT); rec.Code != http.StatusOK {
				t.Fatalf("a PAT got %d on %s: %s", rec.Code, route.path, rec.Body.String())
			}

			withCookie := httptest.NewRequest(route.method, route.path, nil)
			withCookie.AddCookie(&http.Cookie{Name: testCookieName, Value: testCookieVal})
			if rec := f.do(withCookie); rec.Code != http.StatusOK {
				t.Fatalf("a session got %d on %s: %s", rec.Code, route.path, rec.Body.String())
			}

			if bearer, session := f.resolver.counts(); bearer != 1 || session != 1 {
				t.Fatalf("resolver calls: %d bearer, %d session; want 1 each", bearer, session)
			}
		})
	}
}

// ⛔ TestAnIngestTokenIsRefusedOnEveryAPIRoute is SPEC §G.2: an ingest token is
// scoped to one AlertSource and can read NOTHING. It is refused on the string,
// before any lookup — the ingest endpoint has its own authenticator, its own
// pool and its own 401, and is not mounted behind this middleware at all.
func TestAnIngestTokenIsRefusedOnEveryAPIRoute(t *testing.T) {
	t.Parallel()

	routes := append(sharedRoutes(), sessionOnlyRoutes()...)
	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t, apiOptions{})
			req := httptest.NewRequest(route.method, route.path, strings.NewReader(`{"name":"x"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testIngestSec)
			rec := f.do(req)

			requireUnauthenticated(t, rec)
			if bearer, session := f.resolver.counts(); bearer != 0 || session != 0 {
				t.Fatalf("an ingest token caused %d bearer and %d session lookup(s); "+
					"it must be refused on its prefix alone", bearer, session)
			}
		})
	}
}

// ⭐ TestACredentialPresentedThroughTheWrongDoorIsRefused is the "and vice versa"
// case: the two credential kinds are not interchangeable transports.
func TestACredentialPresentedThroughTheWrongDoorIsRefused(t *testing.T) {
	t.Parallel()

	t.Run("a session secret in the Authorization header", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t, apiOptions{})
		req := httptest.NewRequest(http.MethodGet, "/me", nil)
		req.Header.Set("Authorization", "Bearer "+testCookieVal)
		rec := f.do(req)

		requireUnauthenticated(t, rec)
		// It does not announce `oto_pat_`, so it is refused before any lookup —
		// and, crucially, it is NOT retried as a cookie.
		if bearer, session := f.resolver.counts(); bearer != 0 || session != 0 {
			t.Fatalf("a session secret sent as a bearer caused %d bearer and %d session lookup(s)",
				bearer, session)
		}
	})

	t.Run("a PAT in the session cookie", func(t *testing.T) {
		t.Parallel()

		f := newAPIFixture(t, apiOptions{})
		f.resolver.deny = true // the session resolver will not find a PAT's digest

		req := httptest.NewRequest(http.MethodGet, "/me", nil)
		req.AddCookie(&http.Cookie{Name: testCookieName, Value: testPATSecret})
		rec := f.do(req)

		requireUnauthenticated(t, rec)
		// The cookie is resolved AS A COOKIE. A middleware that sniffed the value
		// and rerouted it to the bearer resolver would let a PAT reach the
		// session-only routes through the cookie jar.
		if bearer, session := f.resolver.counts(); bearer != 0 || session != 1 {
			t.Fatalf("resolver calls: %d bearer, %d session; a cookie must be resolved as a cookie",
				bearer, session)
		}
	})
}

// ⛔ TestANonBearerAuthorizationHeaderDoesNotFallThroughToTheCookie.
//
// A present-but-unreadable credential must not be treated as an absent one. If it
// were, a request carrying `Authorization: Basic …` alongside a stale cookie
// would authenticate as the COOKIE's owner — somebody the header did not name.
func TestANonBearerAuthorizationHeaderDoesNotFallThroughToTheCookie(t *testing.T) {
	t.Parallel()

	for _, header := range []string{
		"Basic cHJpeWE6aHVudGVyMg==",
		"Bearer",
		"Token " + testPATSecret,
		"bearer", // the prefix alone, no secret
	} {
		t.Run(header, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t, apiOptions{})
			req := httptest.NewRequest(http.MethodGet, "/me", nil)
			req.Header.Set("Authorization", header)
			req.AddCookie(&http.Cookie{Name: testCookieName, Value: testCookieVal})
			rec := f.do(req)

			requireUnauthenticated(t, rec)
			if _, session := f.resolver.counts(); session != 0 {
				t.Fatal("the cookie was used to authenticate a request that carried an " +
					"Authorization header naming somebody else")
			}
		})
	}
}

// TestNoCredentialAtAllIsTheSame401 — one code, one message, no hint about which
// check said no.
func TestNoCredentialAtAllIsTheSame401(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t, apiOptions{})

	for _, route := range append(sharedRoutes(), sessionOnlyRoutes()...) {
		req := httptest.NewRequest(route.method, route.path, strings.NewReader(`{"name":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		requireUnauthenticated(t, f.do(req))
	}

	// An empty cookie is no cookie.
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.AddCookie(&http.Cookie{Name: testCookieName, Value: ""})
	requireUnauthenticated(t, f.do(req))

	if bearer, session := f.resolver.counts(); bearer != 0 || session != 0 {
		t.Fatalf("a request with no credential caused %d bearer and %d session lookup(s)", bearer, session)
	}
}

// TestARejectedSessionCookieIsNotRetriedAsABearer pins the resolver failure path:
// when the store says no, the answer is the same 401 and nothing else is tried.
func TestARejectedSessionCookieIsNotRetriedAsABearer(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t, apiOptions{})
	f.resolver.deny = true

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.AddCookie(&http.Cookie{Name: testCookieName, Value: testCookieVal})
	requireUnauthenticated(t, f.do(req))

	if bearer, session := f.resolver.counts(); bearer != 0 || session != 1 {
		t.Fatalf("resolver calls: %d bearer, %d session; want 0 and 1", bearer, session)
	}
}
