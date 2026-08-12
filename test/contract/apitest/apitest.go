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
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/platform/httpx/middleware"
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

// Anonymous returns a copy of c that presents no principal at all.
func (c *Client) Anonymous() *Client {
	cp := *c
	cp.anonymous = true
	return &cp
}

// Do runs one request.
func (c *Client) Do(req *http.Request) *Response {
	if !c.anonymous {
		req = req.WithContext(authn.Into(req.Context(), c.p))
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
