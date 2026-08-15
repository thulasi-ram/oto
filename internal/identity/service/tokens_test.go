package service_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// Personal access tokens: mint, verify, revoke — and every way a credential that
// is not a live PAT has to be refused.
//
// As in sessions_test.go, `fakeTokens.ResolveByPrefix` filters by PREFIX ALONE.
// Production's SQL also excludes `kind <> 'pat'`, revoked rows and expired rows;
// here nothing does, so what these tests observe is the service's own
// `domain.APIToken.Usable` and the constant-time digest comparison. The SQL
// predicates are pinned separately in
// `identity/repository/auth_resolvers_test.go`.

// sessionPrincipal is a signed-in human — the only principal allowed to mint a
// token.
func (f *authFixture) sessionPrincipal() authn.Principal {
	return authn.Principal{
		Kind: authn.KindSession, OrgID: f.org, UserID: f.user.ID,
		DisplayName: f.user.DisplayName, Email: f.user.Email.String(),
		SessionID: id.New(),
	}
}

// ⭐ TestAPATIsMintedVerifiedAndRevoked is the whole life of a token, in one test,
// because the interesting assertions are about what survives each step.
func TestAPATIsMintedVerifiedAndRevoked(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	p := f.sessionPrincipal()

	issued, err := f.svc.IssueToken(ctx(), f.scope, p, service.CreateTokenCommand{Name: "laptop"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// --- what was minted
	if !strings.HasPrefix(issued.Secret, domain.SecretPrefixPAT) {
		t.Fatalf("secret %q does not announce its kind", issued.Secret[:min(12, len(issued.Secret))])
	}
	if got := len(issued.Secret) - len(domain.SecretPrefixPAT); got < 32 {
		t.Fatalf("the random half is %d characters; 32 bytes of entropy cannot fit", got)
	}
	if issued.Token.Prefix.String() != issued.Secret[:domain.TokenPrefixLenPAT] {
		t.Fatalf("the stored prefix %q is not the head of the secret", issued.Token.Prefix)
	}
	if issued.Token.Kind != domain.TokenKindPAT || issued.Token.OrgID != f.org || issued.Token.UserID != f.user.ID {
		t.Fatalf("the row names the wrong credential: %+v", issued.Token)
	}
	if issued.Token.SourceID != uuid.Nil {
		t.Fatal("a PAT was scoped to a source; api_tokens_ingest_scope says only an ingest token is")
	}

	// ⛔ THE SECRET IS NOT IN THE ROW, and there is no field it could be in. The
	// stored value is its sha256, and the type's String() is a redaction — so a
	// row that reaches a log line is not a credential that reaches a log line.
	inserted, _ := f.tokens.counts()
	if inserted != 1 {
		t.Fatalf("tokens inserted = %d, want 1", inserted)
	}
	row := f.tokens.inserted[0]
	if row.Hash != digestOf(t, issued.Secret) {
		t.Fatal("the stored hash is not the sha256 of the issued secret")
	}
	if rendered := fmt.Sprintf("%+v", row); strings.Contains(rendered, issued.Secret) {
		t.Fatalf("the plaintext is reachable by formatting the row: %s", rendered)
	}

	// --- verification
	got, err := f.svc.ResolveBearer(ctx(), issued.Secret)
	if err != nil {
		t.Fatalf("a freshly minted token did not authenticate: %v", err)
	}
	if got.Kind != authn.KindPAT || got.OrgID != f.org || got.UserID != f.user.ID {
		t.Fatalf("the resolved principal is wrong: %+v", got)
	}
	if got.TokenID != issued.Token.ID {
		t.Fatal("the principal names a different token than the one presented")
	}
	if got.SessionID != uuid.Nil {
		t.Fatal("a PAT principal carries a session id; logout would then have something to revoke")
	}

	// --- revocation
	f.clk.Advance(time.Hour)
	if err := f.svc.RevokeToken(ctx(), f.scope, issued.Token.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_, err = f.svc.ResolveBearer(ctx(), issued.Secret)
	requireUnauthorized(t, err, "unauthenticated")

	// ⚠️ IDEMPOTENT, and the timestamp does not move: it records when the
	// credential stopped working.
	if err := f.svc.RevokeToken(ctx(), f.scope, issued.Token.ID); err != nil {
		t.Fatalf("a second revoke failed: %v", err)
	}
	revokedAt := f.tokens.rows[0].Token.RevokedAt
	if revokedAt == nil || !revokedAt.Equal(epoch.Add(time.Hour)) {
		t.Fatalf("revoked_at = %v, want the first revocation's instant from the injected clock", revokedAt)
	}
}

// ⛔ TestAnIngestTokenIsRefusedOnTheAPIWithoutALookup is SPEC §G.2's narrowness,
// asserted where it is cheapest to get wrong.
//
// An ingest token is scoped to exactly one AlertSource and can read NOTHING. The
// middleware refuses it before this is ever called; this is the second refusal,
// and it happens on the string alone — no database round trip, so an attacker
// cannot use the API as a lookup amplifier for guessed ingest secrets.
func TestAnIngestTokenIsRefusedOnTheAPIWithoutALookup(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	ingest := domain.SecretPrefixIngest + id.Token(32)

	// Even one that really exists in the table.
	prefix, err := domain.PrefixOfSecret(ingest)
	if err != nil {
		t.Fatalf("prefix: %v", err)
	}
	token, err := domain.NewAPIToken(domain.NewAPITokenParams{
		ID: id.New(), OrgID: f.org, Kind: domain.TokenKindIngest, Name: "ingest:prod",
		Hash: digestOf(t, ingest), Prefix: prefix, SourceID: id.New(), CreatedAt: epoch,
	})
	if err != nil {
		t.Fatalf("ingest token: %v", err)
	}
	f.tokens.put(domain.AuthenticatedToken{Token: token, Subject: f.tokens.subjectForRows})

	_, err = f.svc.ResolveBearer(ctx(), ingest)
	requireUnauthorized(t, err, "unauthenticated")

	if _, resolves := f.tokens.counts(); resolves != 0 {
		t.Fatalf("an ingest secret caused %d database lookup(s); it must be refused on its prefix alone", resolves)
	}
}

// TestSomethingThatIsNotABearerSecretIsRefusedWithoutALookup — a session cookie
// value, an empty string, a JWT-shaped thing, a bare prefix.
func TestSomethingThatIsNotABearerSecretIsRefusedWithoutALookup(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	cookie, _ := loginSession(t, f)

	for _, presented := range []string{
		"",
		cookie, // ⭐ a live session cookie is NOT a bearer token
		"Bearer " + domain.SecretPrefixPAT + id.Token(32),
		"eyJhbGciOiJIUzI1NiJ9.e30.x",
		domain.SecretPrefixPAT, // the literal with no random half
		"oto_pat",              // one character short of the literal
	} {
		_, err := f.svc.ResolveBearer(ctx(), presented)
		requireUnauthorized(t, err, "unauthenticated")
	}
	if _, resolves := f.tokens.counts(); resolves != 0 {
		t.Fatalf("%d lookup(s) happened for credentials that are not PATs", resolves)
	}
}

// ⭐ TestTheDigestDecidesAndNotThePrefix. The prefix selects candidates; the
// constant-time comparison of the full sha256 is what authenticates.
//
// Two tokens deliberately SHARE a display prefix here, which the four random
// characters make possible in the wild. Presenting the second one's secret must
// authenticate the second one — not the first row the store happened to return —
// and presenting a third secret with the same prefix must authenticate nobody.
func TestTheDigestDecidesAndNotThePrefix(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	const shared = "Ab3D"

	mint := func(userID uuid.UUID) (string, domain.APIToken) {
		t.Helper()
		secret := domain.SecretPrefixPAT + shared + id.Token(32)
		prefix, err := domain.PrefixOfSecret(secret)
		if err != nil {
			t.Fatalf("prefix: %v", err)
		}
		token, err := domain.NewAPIToken(domain.NewAPITokenParams{
			ID: id.New(), OrgID: f.org, UserID: userID, Kind: domain.TokenKindPAT,
			Name: "colliding", Hash: digestOf(t, secret), Prefix: prefix, CreatedAt: epoch,
		})
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		return secret, token
	}

	firstSecret, first := mint(f.user.ID)
	secondSecret, second := mint(f.user.ID)
	f.tokens.put(domain.AuthenticatedToken{Token: first, Subject: f.tokens.subjectForRows})
	f.tokens.put(domain.AuthenticatedToken{Token: second, Subject: f.tokens.subjectForRows})

	if first.Prefix.String() != second.Prefix.String() {
		t.Fatal("the fixture failed to produce a prefix collision")
	}

	p, err := f.svc.ResolveBearer(ctx(), secondSecret)
	if err != nil {
		t.Fatalf("the second colliding token did not authenticate: %v", err)
	}
	if p.TokenID != second.ID {
		t.Fatalf("the wrong candidate authenticated: %s, want %s — the prefix decided, not the digest",
			p.TokenID, second.ID)
	}

	p, err = f.svc.ResolveBearer(ctx(), firstSecret)
	if err != nil || p.TokenID != first.ID {
		t.Fatalf("the first colliding token did not authenticate as itself: %v %s", err, p.TokenID)
	}

	// A third secret sharing the prefix and nothing else.
	_, err = f.svc.ResolveBearer(ctx(), domain.SecretPrefixPAT+shared+id.Token(32))
	requireUnauthorized(t, err, "unauthenticated")
}

// ⛔ TestAnExpiredPATFailsClosedEvenIfTheStoreReturnsIt.
func TestAnExpiredPATFailsClosedEvenIfTheStoreReturnsIt(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	p := f.sessionPrincipal()
	expiry := epoch.Add(2 * time.Hour)

	issued, err := f.svc.IssueToken(ctx(), f.scope, p,
		service.CreateTokenCommand{Name: "short-lived", ExpiresAt: &expiry})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	f.clk.Advance(time.Hour)
	if _, err := f.svc.ResolveBearer(ctx(), issued.Secret); err != nil {
		t.Fatalf("the token died before its expiry: %v", err)
	}

	// At the expiry instant. `Expired` is `!now.Before(ExpiresAt)`: the boundary
	// belongs to the dead side.
	f.clk.Advance(time.Hour)
	_, err = f.svc.ResolveBearer(ctx(), issued.Secret)
	requireUnauthorized(t, err, "unauthenticated")
}

// ⛔ TestATokenWithAnExpiryInThePastIsRefusedAndNothingIsWritten.
//
// The orphan-row assertion is the point. `TokenPrefixLen` shipped as one wrong
// constant, `POST /api/v1/sources` returned 422 forever, and the failed mints
// left rows behind. A refused mint must leave the table exactly as it found it.
func TestATokenWithAnExpiryInThePastIsRefusedAndNothingIsWritten(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{"in the past", epoch.Add(-time.Second)},
		{"exactly now", epoch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newAuthFixture(t)
			at := tc.at

			_, err := f.svc.IssueToken(ctx(), f.scope, f.sessionPrincipal(),
				service.CreateTokenCommand{Name: "dead on arrival", ExpiresAt: &at})
			if err == nil {
				t.Fatal("a token that expires at or before its own creation was minted")
			}
			if !errs.IsKind(err, errs.KindValidation) {
				t.Fatalf("kind = %s, want validation_failed", errs.KindOf(err))
			}
			if errs.CodeOf(err) != "invalid_token_expiry" {
				t.Fatalf("code = %q, want invalid_token_expiry", errs.CodeOf(err))
			}
			v := errs.ViolationsOf(err)
			if len(v) != 1 || v[0].Field != "expires_at" {
				t.Fatalf("violations = %+v, want one naming expires_at", v)
			}
			if inserted, _ := f.tokens.counts(); inserted != 0 {
				t.Fatalf("a refused mint left %d row(s) behind", inserted)
			}
		})
	}
}

// ⛔ TestOnlyASignedInUserCanMintOrListTokens. There is no such thing as an
// org-owned PAT: a credential that could mint one would be a credential that
// reproduces itself.
func TestOnlyASignedInUserCanMintOrListTokens(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	// An ingest principal: a tenant, but no human.
	headless := authn.Principal{Kind: authn.KindIngest, OrgID: f.org, SourceID: id.New()}

	_, err := f.svc.IssueToken(ctx(), f.scope, headless, service.CreateTokenCommand{Name: "self-replicating"})
	if err == nil {
		t.Fatal("a credential with no user behind it minted a personal access token")
	}
	if !errs.IsKind(err, errs.KindForbidden) || errs.CodeOf(err) != "token_requires_user" {
		t.Fatalf("error = %v, want forbidden/token_requires_user", err)
	}
	if inserted, _ := f.tokens.counts(); inserted != 0 {
		t.Fatalf("a refused mint wrote %d row(s)", inserted)
	}

	if _, _, err := f.svc.ListTokens(ctx(), f.scope, headless, dbKeyset()); err == nil {
		t.Fatal("a credential with no user behind it listed tokens")
	}
}

// TestListTokensIsNarrowedToTheCallersOwnTokens — not an authorisation rule (v1
// has no RBAC), a privacy one: Priya's laptop token has no business in Sam's
// settings screen.
func TestListTokensIsNarrowedToTheCallersOwnTokens(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	mine := f.sessionPrincipal()

	if _, err := f.svc.IssueToken(ctx(), f.scope, mine, service.CreateTokenCommand{Name: "mine"}); err != nil {
		t.Fatalf("mint: %v", err)
	}

	// Somebody else in the same org.
	other := f.addUser(t, "sam@example.test", testPassword, nil)
	theirs := authn.Principal{Kind: authn.KindSession, OrgID: f.org, UserID: other.ID}
	if _, err := f.svc.IssueToken(ctx(), f.scope, theirs, service.CreateTokenCommand{Name: "theirs"}); err != nil {
		t.Fatalf("mint: %v", err)
	}

	list, _, err := f.svc.ListTokens(ctx(), f.scope, mine, dbKeyset())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Name != "mine" {
		t.Fatalf("the list returned %d token(s) %v; it must carry only the caller's", len(list), list)
	}
}

// TestRevokingATokenThatIsNotInThisOrgIsNotFound — never forbidden. A 403 would
// confirm the id exists somewhere.
func TestRevokingATokenThatIsNotInThisOrgIsNotFound(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)

	err := f.svc.RevokeToken(ctx(), f.scope, id.New())
	if err == nil {
		t.Fatal("revoking an id that does not exist here succeeded")
	}
	if !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("kind = %s, want not_found — a 403 would confirm the id exists elsewhere", errs.KindOf(err))
	}
	if errs.CodeOf(err) != "token_not_found" {
		t.Fatalf("code = %q, want token_not_found", errs.CodeOf(err))
	}
}

// ⚠️ TestLastUsedBookkeepingCannotFailARequest. `last_used_at` is an operator
// convenience, not an audit record: a request must not 500 because a bookkeeping
// UPDATE could not get a row lock.
func TestLastUsedBookkeepingCannotFailARequest(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	issued, err := f.svc.IssueToken(ctx(), f.scope, f.sessionPrincipal(),
		service.CreateTokenCommand{Name: "laptop"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	f.tokens.touchErr = errs.Internal("row_locked", nil)
	if _, err := f.svc.ResolveBearer(ctx(), issued.Secret); err != nil {
		t.Fatalf("authentication failed because of a bookkeeping write: %v", err)
	}
	if len(f.tokens.touched) != 1 {
		t.Fatalf("last_used_at was touched %d time(s), want 1", len(f.tokens.touched))
	}
}

// TestLastUsedIsNotWrittenOnEveryRequest — an UPDATE on the read path of every
// PAT-authenticated call is contention on one row, for five-minute resolution
// nobody needs.
func TestLastUsedIsNotWrittenOnEveryRequest(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	issued, err := f.svc.IssueToken(ctx(), f.scope, f.sessionPrincipal(),
		service.CreateTokenCommand{Name: "laptop"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// The first call has no `last_used_at` to compare against and writes one.
	if _, err := f.svc.ResolveBearer(ctx(), issued.Secret); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Give the row the timestamp the real store would now hold.
	now := f.clk.Now()
	f.tokens.rows[0].Token.LastUsedAt = &now

	if _, err := f.svc.ResolveBearer(ctx(), issued.Secret); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(f.tokens.touched) != 1 {
		t.Fatalf("last_used_at was written %d times inside one TouchInterval", len(f.tokens.touched))
	}

	f.clk.Advance(service.TouchInterval + time.Second)
	if _, err := f.svc.ResolveBearer(ctx(), issued.Secret); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(f.tokens.touched) != 2 {
		t.Fatalf("last_used_at was not refreshed after TouchInterval (%d writes)", len(f.tokens.touched))
	}
}

// TestEveryMintedSecretIsDifferent. 256 bits from crypto/rand — the assertion is
// cheap and the failure it catches (a seeded or reused generator) is total.
func TestEveryMintedSecretIsDifferent(t *testing.T) {
	t.Parallel()

	f := newAuthFixture(t)
	seen := map[string]bool{}
	for i := range 16 {
		issued, err := f.svc.IssueToken(ctx(), f.scope, f.sessionPrincipal(),
			service.CreateTokenCommand{Name: fmt.Sprintf("token-%d", i)})
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if seen[issued.Secret] {
			t.Fatal("a minted secret repeated")
		}
		seen[issued.Secret] = true
	}
}
