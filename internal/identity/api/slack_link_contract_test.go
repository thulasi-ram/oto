package api

// THE SELF-SERVICE SLACK LINK OVER HTTP (git-bug a556a5c): four operations under `/me`, the three
// writes session-only, NONE taking a user id. These run the real authenticator behind the contract
// fixture's session resolver, and a SlackLinks double that records which principal each call was
// made as — the property under test is that it is always the signed-in one.

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

var linkIdentityID = uuid.MustParse("019fe2a1-5d1e-7c00-8000-00000000a556")

type linkCall struct {
	op   string
	p    authn.Principal
	code string
	id   uuid.UUID
}

// contractSlackLinks answers as the service would for ONE identity the signed-in user may link
// or holds. "WRONG" is a wrong code, "TAKEN" a member another real user holds, "SLOW" a user
// whose wrong-code budget is spent.
type contractSlackLinks struct {
	mu    sync.Mutex
	calls []linkCall
}

func (c *contractSlackLinks) record(l linkCall) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, l)
}

func (c *contractSlackLinks) seen() []linkCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]linkCall(nil), c.calls...)
}

func contractSlackIdentity(t testing.TB, userID uuid.UUID) domain.SlackIdentity {
	team, err := domain.NewSlackTeamID("T9TK3CUKW")
	if err != nil {
		t.Fatal(err)
	}
	member, err := domain.NewSlackUserID("U0123456789")
	if err != nil {
		t.Fatal(err)
	}
	si, err := domain.NewSlackIdentity(linkIdentityID, apitest.OrgID, team, member, "priya")
	if err != nil {
		t.Fatal(err)
	}
	if userID != uuid.Nil {
		si, err = si.Link(userID, contractIdentityEpoch)
		if err != nil {
			t.Fatal(err)
		}
	}
	return si
}

func refusalFor(code string) error {
	switch code {
	case "WRONG":
		return domain.SlackLinkCodeInvalid()
	case "TAKEN":
		return domain.SlackIdentityLinkedElsewhere()
	case "SLOW":
		return errs.RateLimited(domain.SlackLinkAttemptsExhaustedCode, "too many wrong link codes", 15*time.Minute)
	}
	return nil
}

func (c *contractSlackLinks) ListMySlackIdentities(_ context.Context, _ db.TenantScope, p authn.Principal) ([]domain.SlackIdentity, error) {
	c.record(linkCall{op: "list", p: p})
	return []domain.SlackIdentity{contractSlackIdentity(nil, p.UserID)}, nil
}

func (c *contractSlackLinks) PreviewSlackLink(_ context.Context, _ db.TenantScope, p authn.Principal, code string) (service.SlackLinkPreview, error) {
	c.record(linkCall{op: "preview", p: p, code: code})
	if err := refusalFor(code); err != nil {
		return service.SlackLinkPreview{}, err
	}
	return service.SlackLinkPreview{
		Identity: contractSlackIdentity(nil, uuid.Nil), ExpiresAt: contractIdentityEpoch.Add(10 * time.Minute),
	}, nil
}

func (c *contractSlackLinks) ConfirmSlackLink(_ context.Context, _ db.TenantScope, p authn.Principal, code string) (domain.SlackIdentity, error) {
	c.record(linkCall{op: "confirm", p: p, code: code})
	if err := refusalFor(code); err != nil {
		return domain.SlackIdentity{}, err
	}
	return contractSlackIdentity(nil, p.UserID), nil
}

// UnlinkSlackIdentity mirrors `unlinkSlackIdentitySQL`: only the one identity this user holds.
func (c *contractSlackLinks) UnlinkSlackIdentity(_ context.Context, _ db.TenantScope, p authn.Principal, id uuid.UUID) error {
	c.record(linkCall{op: "unlink", p: p, id: id})
	if id != linkIdentityID {
		return errs.NotFound("slack_identity_not_found", "no such slack identity")
	}
	return nil
}

func newSlackLinkFixture(t *testing.T) (*contractSlackLinks, *apitest.Client) {
	t.Helper()
	links := &contractSlackLinks{}
	rt := NewRouter(Options{
		Auth:       authn.NewMiddleware(&contractSessionResolver{}, contractCookieName),
		Cookie:     DefaultCookieConfig(contractCookieName),
		SlackLinks: links,
		Clock:      clock.New(),
	})
	return links, apitest.New(rt).WithCookie(contractCookieName, contractCookieValue)
}

// requireSignedInOnly asserts every call the service saw was made as the signed-in session.
func requireSignedInOnly(t *testing.T, calls []linkCall) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("the service was never asked")
	}
	for _, c := range calls {
		if c.p.UserID != apitest.UserID || c.p.Kind != authn.KindSession || c.p.OrgID != apitest.OrgID {
			t.Fatalf("%s was made as %+v, not the signed-in session", c.op, c.p)
		}
	}
}

func TestListMySlackIdentitiesAnswersTheContractShape(t *testing.T) {
	t.Parallel()
	links, c := newSlackLinkFixture(t)
	resp := c.GET("/me/slack-identities").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listMySlackIdentities", http.StatusOK, resp.Body())
	requireSignedInOnly(t, links.seen())
}

func TestPreviewSlackLinkNamesTheMemberWithoutLinking(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"code":"abcde-fghjk"}`)
	schema.AssertRequest(t, "previewSlackLink", raw)

	links, c := newSlackLinkFixture(t)
	resp := c.Raw(http.MethodPost, "/me/slack-identities/preview", apitest.ContentTypeJSON, string(raw)).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "previewSlackLink", http.StatusOK, resp.Body())
	body := string(resp.Body())
	for _, want := range []string{`"team_id":"T9TK3CUKW"`, `"slack_user_id":"U0123456789"`, `"already_yours":false`} {
		if !strings.Contains(body, want) {
			t.Fatalf("the preview does not say %s: %s", want, body)
		}
	}
	calls := links.seen()
	requireSignedInOnly(t, calls)
	if len(calls) != 1 || calls[0].op != "preview" || calls[0].code != "abcde-fghjk" {
		t.Fatalf("calls = %+v; a preview must not confirm", calls)
	}
}

func TestLinkSlackIdentityLinksTheSignedInUser(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"code":"ABCDE-FGHJK"}`)
	schema.AssertRequest(t, "linkSlackIdentity", raw)

	links, c := newSlackLinkFixture(t)
	resp := c.Raw(http.MethodPost, "/me/slack-identities", apitest.ContentTypeJSON, string(raw)).
		MustStatus(t, http.StatusCreated)
	schema.Assert(t, "linkSlackIdentity", http.StatusCreated, resp.Body())
	requireSignedInOnly(t, links.seen())
}

func TestUnlinkSlackIdentityAnswers204(t *testing.T) {
	t.Parallel()
	links, c := newSlackLinkFixture(t)
	c.DELETE("/me/slack-identities/"+linkIdentityID.String()).MustStatus(t, http.StatusNoContent)
	requireSignedInOnly(t, links.seen())
}

// ⛔ A body naming somebody else is refused before the service is asked — the request DTO has no
// such field, and an unknown member is not silently dropped.
func TestNoRouteTakesAUserID(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/me/slack-identities", "/me/slack-identities/preview"} {
		links, c := newSlackLinkFixture(t)
		body := `{"code":"ABCDE-FGHJK","user_id":"` + apitest.StrangerID.String() + `"}`
		resp := c.Raw(http.MethodPost, path, apitest.ContentTypeJSON, body)
		if resp.Code() < 400 || resp.Code() >= 500 {
			t.Fatalf("%s accepted a user id: %s", path, resp)
		}
		if n := len(links.seen()); n != 0 {
			t.Fatalf("%s reached the service %d time(s) with a user id in the body", path, n)
		}
	}
	// And no operation's request schema has anywhere to put one.
	for _, op := range []string{"previewSlackLink", "linkSlackIdentity"} {
		sch, err := schema.RequestBody(op)
		if err != nil {
			t.Fatal(err)
		}
		v, err := jsonschema.UnmarshalJSON(strings.NewReader(`{"code":"ABCDE-FGHJK","user_id":"` + apitest.StrangerID.String() + `"}`))
		if err != nil {
			t.Fatal(err)
		}
		if sch.Validate(v) == nil {
			t.Fatalf("the contract lets %s carry a user id", op)
		}
	}
}

func TestTheRefusalsAreTyped(t *testing.T) {
	t.Parallel()
	for code, want := range map[string]struct {
		status int
		code   string
	}{
		"WRONG": {http.StatusUnprocessableEntity, domain.SlackLinkCodeInvalidCode},
		"TAKEN": {http.StatusConflict, domain.SlackIdentityLinkedElsewhereCode},
		"SLOW":  {http.StatusTooManyRequests, domain.SlackLinkAttemptsExhaustedCode},
	} {
		for _, op := range []struct{ id, path string }{
			{"previewSlackLink", "/me/slack-identities/preview"},
			{"linkSlackIdentity", "/me/slack-identities"},
		} {
			_, c := newSlackLinkFixture(t)
			resp := c.Raw(http.MethodPost, op.path, apitest.ContentTypeJSON, `{"code":"`+code+`"}`)
			if resp.Code() != want.status {
				t.Fatalf("%s %s: status %d, want %d: %s", op.id, code, resp.Code(), want.status, resp)
			}
			schema.AssertProblem(t, op.id, want.status, resp.Body())
			if got := resp.Problem(t).Code; got != want.code {
				t.Fatalf("%s %s: code %q, want %q", op.id, code, got, want.code)
			}
			if want.status == http.StatusTooManyRequests && resp.Header("Retry-After") == "" {
				t.Fatalf("%s: a 429 without Retry-After", op.id)
			}
		}
	}
}

// ⛔ A Slack identity that is not the caller's — another org's, somebody else's — is a 404.
func TestUnlinkingASlackIdentityThatIsNotYoursIsANotFound(t *testing.T) {
	t.Parallel()
	routes := []apitest.Route{
		{Name: "an identity owned by another org", Op: "unlinkSlackIdentity",
			Method: http.MethodDelete, Path: "/me/slack-identities/" + apitest.StrangerID.String()},
		{Name: "an identity of another user in this org", Op: "unlinkSlackIdentity",
			Method: http.MethodDelete, Path: "/me/slack-identities/019fe2a1-5d1e-7c00-8000-0000000b0b0b"},
	}
	apitest.AssertCrossTenant404(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		links, c := newSlackLinkFixture(t)
		return c, func(t *testing.T, _ apitest.Route, _ *apitest.Response) {
			requireSignedInOnly(t, links.seen())
		}
	}, routes)
}

func TestAnUnwiredDeploymentAnswers503(t *testing.T) {
	t.Parallel()
	rt := NewRouter(Options{
		Auth:   authn.NewMiddleware(&contractSessionResolver{}, contractCookieName),
		Cookie: DefaultCookieConfig(contractCookieName),
		Clock:  clock.New(),
	})
	c := apitest.New(rt).WithCookie(contractCookieName, contractCookieValue)
	resp := c.Raw(http.MethodPost, "/me/slack-identities", apitest.ContentTypeJSON, `{"code":"ABCDE-FGHJK"}`)
	if resp.Code() != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", resp.Code(), resp)
	}
	schema.AssertProblem(t, "linkSlackIdentity", http.StatusServiceUnavailable, resp.Body())
}
