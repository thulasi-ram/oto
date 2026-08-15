// Package apitest drives one domain router with net/http/httptest.
//
// The handlers are what the conformance audit found every one of its C-findings
// in, and the reason they were never checked is that checking them looked
// expensive: a database, a composition root, a server. None of that is true. A
// domain router mounts onto a bare chi tree, its service ports are interfaces
// the api package declares for itself, and one request is a `httptest.NewRequest`
// away.
//
// What this package supplies is the small amount of REALISM that a bare
// `httptest.NewRequest` lacks and that the contract depends on:
//
//   - the request-id middleware, because `meta.request_id` is a REQUIRED member
//     of every success envelope and `httpx.Meta` omits it when empty;
//   - an authenticated principal in the context, because every handler resolves
//     its tenant from one and there is no other sanctioned path to a
//     db.TenantScope;
//   - two org ids that are not each other, so that "another tenant's id" is a
//     value a table-driven test can pass rather than a scenario it must set up.
package apitest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/platform/httpx/middleware"
	"github.com/thulasiram/oto/test/contract/schema"
)

// The two tenants every test in this suite is written against.
//
// ⛔ They are CONSTANTS and never freshly generated: a tenant-scoping test whose
// "other org" changes per run cannot be reproduced from a failure message, and
// the whole point of the probe is that a failure names the exact id that leaked.
var (
	// OrgID is the caller's tenant.
	OrgID = uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	// OtherOrgID is somebody else's tenant. Nothing owned by it may ever be
	// visible to a caller scoped to OrgID, and asking for one of its ids must
	// answer 404 — never 403, which would confirm the row exists.
	OtherOrgID = uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	// UserID is the human behind the session.
	UserID = uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	// StrangerID is an id that belongs to OtherOrgID. It is the value the
	// tenant-scoping probe puts in the path.
	StrangerID = uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
)

// Member is the least-privileged caller every authenticated endpoint accepts:
// a browser session belonging to a human in OrgID. v1 has no roles, so this is
// also the MOST privileged caller — which is exactly why the tenant boundary is
// the only boundary there is, and why it has to hold.
func Member() authn.Principal {
	return authn.Principal{
		Kind:        authn.KindSession,
		OrgID:       OrgID,
		UserID:      UserID,
		DisplayName: "Ada Lovelace",
		Email:       "ada@example.com",
		OrgSlug:     "acme",
		OrgName:     "Acme",
		SessionID:   uuid.MustParse("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"),
	}
}

// MemberOf is Member scoped to a different tenant, for the handful of tests
// that need to prove a row IS visible to its owner.
func MemberOf(org uuid.UUID) authn.Principal {
	p := Member()
	p.OrgID = org
	return p
}

// Machine is a non-human principal — a PAT is human, a system credential is not.
// The human verbs (ack, comment, snooze) refuse it with a 403, and that refusal
// is a branch worth a test: an acknowledgement attributed to "system" is a
// receipt nobody signed.
func Machine() authn.Principal {
	return authn.Principal{Kind: authn.KindSystem, OrgID: OrgID}
}

// Mounter is what every domain router in this repo satisfies.
type Mounter interface{ Mount(chi.Router) }

// Handler wraps a domain router in the middleware the real server runs, rooted
// where internal/app roots it.
//
// The request-id middleware is not optional decoration: `Meta.request_id` is
// required by the contract and `httpx.Meta` marshals it with `omitempty`, so a
// response produced without it is a response that fails its own schema. Running
// the middleware here is what makes the schema assertion honest.
func Handler(rt Mounter) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	rt.Mount(r)
	return r
}

// Client drives one router as one principal.
type Client struct {
	h http.Handler
	p authn.Principal
	// anonymous suppresses the principal entirely, for the unauthenticated
	// surfaces (/healthz, /version, login).
	anonymous bool
	// cookies ride on every request, for the one router that mounts the REAL
	// session middleware and therefore authenticates from the wire rather than
	// from the context principal.
	cookies []http.Cookie
}

// New builds a client for rt, calling as an org member.
func New(rt Mounter) *Client { return &Client{h: Handler(rt), p: Member()} }

// NewHandler builds a client over an already-assembled handler.
func NewHandler(h http.Handler) *Client { return &Client{h: h, p: Member()} }

// As returns a copy of c calling as p.
func (c *Client) As(p authn.Principal) *Client {
	cp := *c
	cp.p = p
	cp.anonymous = false
	return &cp
}

// Anonymous returns a copy of c that presents no principal at all — no
// context principal and no cookie, because a session cookie IS a credential.
func (c *Client) Anonymous() *Client {
	cp := *c
	cp.anonymous = true
	return &cp
}

// WithCookie returns a copy of c that presents the cookie on every request —
// how a suite whose router mounts the real session middleware signs its
// client in, since that middleware authenticates from the wire and not from
// the context principal.
func (c *Client) WithCookie(name, value string) *Client {
	cp := *c
	cp.cookies = append(append([]http.Cookie{}, c.cookies...), http.Cookie{Name: name, Value: value})
	return &cp
}

// Do runs one request.
func (c *Client) Do(req *http.Request) *Response {
	if !c.anonymous {
		req = req.WithContext(authn.Into(req.Context(), c.p))
		for i := range c.cookies {
			req.AddCookie(&c.cookies[i])
		}
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return &Response{rec: rec, req: req}
}

// GET runs a GET.
func (c *Client) GET(target string) *Response {
	return c.Do(httptest.NewRequest(http.MethodGet, target, nil))
}

// DELETE runs a DELETE.
func (c *Client) DELETE(target string) *Response {
	return c.Do(httptest.NewRequest(http.MethodDelete, target, nil))
}

// POST runs a POST with a JSON body. A nil body sends no body at all, which is
// what the argument-free verbs (ack, unack, retry) look like on the wire.
func (c *Client) POST(t testing.TB, target string, body any) *Response {
	t.Helper()
	return c.withBody(t, http.MethodPost, target, body)
}

// PATCH runs a PATCH with a JSON body.
func (c *Client) PATCH(t testing.TB, target string, body any) *Response {
	t.Helper()
	return c.withBody(t, http.MethodPatch, target, body)
}

// PUT runs a PUT with a JSON body.
func (c *Client) PUT(t testing.TB, target string, body any) *Response {
	t.Helper()
	return c.withBody(t, http.MethodPut, target, body)
}

// Raw sends a body verbatim, for the malformed-input branches that a Go value
// cannot express.
func (c *Client) Raw(method, target, contentType, body string) *Response {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return c.Do(req)
}

func (c *Client) withBody(t testing.TB, method, target string, body any) *Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		r = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.Do(req)
}

// Response is one recorded exchange.
type Response struct {
	rec *httptest.ResponseRecorder
	req *http.Request
}

// Code is the status.
func (r *Response) Code() int { return r.rec.Code }

// Body is the raw response bytes.
func (r *Response) Body() []byte { return r.rec.Body.Bytes() }

// Header returns one response header.
func (r *Response) Header(name string) string { return r.rec.Header().Get(name) }

// Recorder exposes the recorder for the few assertions that need it.
func (r *Response) Recorder() *httptest.ResponseRecorder { return r.rec }

// String renders the exchange for a failure message.
func (r *Response) String() string {
	return r.req.Method + " " + r.req.URL.String() + " -> " + r.rec.Result().Status + "\n" + r.rec.Body.String()
}

// MustStatus fails unless the status is want.
func (r *Response) MustStatus(t testing.TB, want int) *Response {
	t.Helper()
	if r.rec.Code != want {
		t.Fatalf("status = %d, want %d\n%s", r.rec.Code, want, r)
	}
	return r
}

// JSON decodes the body into a generic tree.
func (r *Response) JSON(t testing.TB) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(r.rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("the response body is not a JSON object: %v\n%s", err, r)
	}
	return out
}

// Problem decodes the body as an RFC 9457 problem.
func (r *Response) Problem(t testing.TB) Problem {
	t.Helper()
	var p Problem
	if err := json.Unmarshal(r.rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("the response body is not a problem document: %v\n%s", err, r)
	}
	return p
}

// Problem is the caller's view of RFC 9457 + violations[].
type Problem struct {
	Type       string      `json:"type"`
	Title      string      `json:"title"`
	Status     int         `json:"status"`
	Detail     string      `json:"detail"`
	Instance   string      `json:"instance"`
	Code       string      `json:"code"`
	RequestID  string      `json:"request_id"`
	Violations []Violation `json:"violations"`
	RetryAfter int         `json:"retry_after_seconds"`
}

// Violation is one entry of violations[].
type Violation struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Fields lists the violated field names.
func (p Problem) Fields() []string {
	out := make([]string, 0, len(p.Violations))
	for _, v := range p.Violations {
		out = append(out, v.Field)
	}
	return out
}

// HasField reports whether violations[] names field.
func (p Problem) HasField(field string) bool {
	for _, v := range p.Violations {
		if v.Field == field {
			return true
		}
	}
	return false
}

// MustViolate asserts that the problem names field in violations[] with a
// machine code — the three things §L.2.2 promises a client and the only three a
// form can act on.
//
// It says nothing about the STATUS: violations[] name a member of the request,
// not a status code (§L.1), so a 400 that can say which query parameter was
// wrong carries them exactly as a 422 does. The caller asserts the status.
func (r *Response) MustViolate(t testing.TB, field string) Problem {
	t.Helper()
	p := r.Problem(t)
	if len(p.Violations) == 0 {
		t.Fatalf("the refusal carries no violations[]; a client can only show the prose\n%s", r)
	}
	if !p.HasField(field) {
		t.Fatalf("violations[] names %v, want an entry for %q\n%s", p.Fields(), field, r)
	}
	for _, v := range p.Violations {
		if v.Field == field && v.Code == "" {
			t.Fatalf("the violation for %q carries no code; a form cannot branch on prose\n%s", field, r)
		}
	}
	return p
}

// ContentTypeJSON is what a body-bearing success must be labelled.
const ContentTypeJSON = httpx.ContentTypeJSON

// ContentTypeProblem is what a refusal must be labelled (RFC 9457 §3).
const ContentTypeProblem = httpx.ContentTypeProblem

/* -------------------------------------------------------------------------- */
/* The cross-cutting probes                                                   */
/* -------------------------------------------------------------------------- */

// Route is one operation of a module's surface, spelled the way its contract
// suite addresses it on the wire: the operationId the contract declares, the
// method, the concrete path, and the JSON body verbatim — empty for the
// body-less operations, which are sent the way a chat button sends them: no
// body, and no header claiming there is one.
//
// The route TABLES stay in the module suites, because a module's table is the
// readable statement of what it serves; only the probes every module owes —
// the same 401 on every route, a stranger's id answering 404, an unknown
// query parameter refused — are executed here, once.
type Route struct {
	Op     string
	Method string
	Path   string
	Body   string
	// Name labels the subtest when one operation is probed under several
	// spellings (a stranger's id, `banana`, the nil uuid). Empty means Op
	// labels it.
	Name string
}

func (r Route) label() string {
	if r.Name != "" {
		return r.Name
	}
	return r.Op
}

// send drives the route through Raw so the bytes on the wire are exactly the
// bytes the table declares.
func (r Route) send(c *Client) *Response {
	return c.Raw(r.Method, r.Path, contentTypeFor(r.Body), r.Body)
}

// contentTypeFor labels a body only when there is one.
func contentTypeFor(body string) string {
	if body == "" {
		return ""
	}
	return ContentTypeJSON
}

// RouteCheck is a module's own assertion on top of a shared probe's refusal —
// typically that the service behind the transport saw no call. It receives
// the route so a module can branch on which spelling was probed.
type RouteCheck func(t *testing.T, r Route, resp *Response)

// WorldFactory builds a fresh world for ONE probe: the client that drives it,
// and the module's RouteCheck (nil when the shared assertions are the whole
// story). Every route gets its own world so the subtests can run in parallel
// and a leak in one cannot pollute the next.
type WorldFactory func(t *testing.T) (*Client, RouteCheck)

// AssertUnauthenticated proves that with no principal there is no tenant, so
// there is nothing to read and nobody to attribute a write to: every route
// answers the contract's own 401 problem with `code: unauthenticated` — one
// code, one shape — before anything behind the transport is reached.
func AssertUnauthenticated(t *testing.T, newWorld WorldFactory, routes []Route) {
	t.Helper()
	for _, r := range routes {
		t.Run(r.label(), func(t *testing.T) {
			t.Parallel()

			c, check := newWorld(t)
			resp := r.send(c.Anonymous())
			requireStatus(t, r, resp, http.StatusUnauthorized)
			schema.AssertProblem(t, r.Op, http.StatusUnauthorized, resp.Body())

			if code := resp.Problem(t).Code; code != "unauthenticated" {
				t.Fatalf("%s %s (%s): code = %q, want unauthenticated",
					r.Method, r.Path, r.Op, code)
			}
			requireProblemContentType(t, r, resp)
			if check != nil {
				check(t, r, resp)
			}
		})
	}
}

// AssertCrossTenant404 proves the tenant boundary on every id-addressed
// route: an id the caller's org does not own answers 404 — never 403, which
// confirms the row exists somewhere and turns the id space into a
// cross-tenant existence oracle; never 200 with somebody else's data; never a
// 500, which is a boundary held by accident — and the refusal says nothing
// about who does own it. v1 has no roles, so this boundary is the only
// boundary there is.
func AssertCrossTenant404(t *testing.T, newWorld WorldFactory, routes []Route) {
	t.Helper()
	for _, r := range routes {
		t.Run(r.label(), func(t *testing.T) {
			t.Parallel()

			c, check := newWorld(t)
			resp := r.send(c)
			requireStatus(t, r, resp, http.StatusNotFound)
			schema.AssertProblem(t, r.Op, http.StatusNotFound, resp.Body())
			requireProblemContentType(t, r, resp)

			// ⚠️ The refusal names neither the other tenant nor anything it owns.
			if strings.Contains(string(resp.Body()), OtherOrgID.String()) {
				t.Fatalf("%s %s (%s): the 404 names the owning org\n%s",
					r.Method, r.Path, r.Op, resp)
			}
			if check != nil {
				check(t, r, resp)
			}
		})
	}
}

// AssertUnknownQueryParamRefused is SPEC §E.3: a query parameter the
// operation does not declare is `400 unknown_parameter` with `violations[]`
// naming the parameter — never silently dropped, because a silently ignored
// filter returns the wrong page wearing the right shape. Each route's Path
// carries the offending parameter itself (the typo is part of the scenario),
// and the helper reads its name back out of the query string.
func AssertUnknownQueryParamRefused(t *testing.T, newWorld WorldFactory, routes []Route) {
	t.Helper()
	for _, r := range routes {
		t.Run(r.label(), func(t *testing.T) {
			t.Parallel()

			// The 400 must be DECLARED, not merely produced: §E.3 makes it
			// reachable with any unknown parameter, and an undeclared status is
			// one no generated client has a branch for (git-bug ee3ae9c).
			if !schema.Op(t, r.Op).Declares(http.StatusBadRequest) {
				t.Fatalf("%s declares no 400, and §E.3 makes one reachable with any "+
					"unknown query parameter", r.Op)
			}

			param := soleQueryParam(t, r)
			c, check := newWorld(t)
			resp := r.send(c)
			requireStatus(t, r, resp, http.StatusBadRequest)
			schema.AssertProblem(t, r.Op, http.StatusBadRequest, resp.Body())

			p := resp.MustViolate(t, param)
			if p.Code != "unknown_parameter" {
				t.Fatalf("%s %s (%s): code = %q, want unknown_parameter",
					r.Method, r.Path, r.Op, p.Code)
			}
			if check != nil {
				check(t, r, resp)
			}
		})
	}
}

// requireStatus is MustStatus with the operation named: a shared executor
// that fails without saying WHICH route failed would be a regression on the
// hand-rolled loops it replaced.
func requireStatus(t *testing.T, r Route, resp *Response, want int) {
	t.Helper()
	if resp.Code() != want {
		t.Fatalf("%s: status = %d, want %d\n%s", r.Op, resp.Code(), want, resp)
	}
}

// requireProblemContentType holds RFC 9457 §3: a refusal is labelled
// application/problem+json, or a generated client will not parse it as one.
func requireProblemContentType(t *testing.T, r Route, resp *Response) {
	t.Helper()
	if ct := resp.Header("Content-Type"); !strings.Contains(ct, "problem+json") {
		t.Fatalf("%s %s (%s): Content-Type = %q, want application/problem+json",
			r.Method, r.Path, r.Op, ct)
	}
}

// soleQueryParam reads the offending parameter's name out of the route's own
// query string, and insists there is exactly one: a probe carrying two could
// not say which of them the refusal must name.
func soleQueryParam(t *testing.T, r Route) string {
	t.Helper()
	u, err := url.Parse(r.Path)
	if err != nil {
		t.Fatalf("%s: the path does not parse: %v", r.Op, err)
	}
	q := u.Query()
	if len(q) != 1 {
		t.Fatalf("%s: the path carries %d query parameters, want exactly the unknown one",
			r.Op, len(q))
	}
	for name := range q {
		return name
	}
	return ""
}
