package service_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
)

// The per-source ingest credential: the mint recipe, the mint-before-revoke
// ordering, and the paged revoke-except sweep. These moved here from the
// composition root, so this file is what stops the recipe drifting now that it
// lives beside the PAT mint it deliberately does NOT share code with.

// orderedTokens is an in-memory TokenStore that records the ORDER of its writes
// and serves List in SMALL pages regardless of the limit it is asked for.
//
// ⭐ THE OPS LOG IS THE POINT OF THE FAKE. Mint-before-revoke is an ordering
// fact — revoke-then-mint left a source with zero working credentials when the
// mint failed, and Alertmanager treats the resulting 401 as permanent — and an
// ordering fact is only observable where the calls land, in the order they
// landed.
//
// It is deliberately LEAKY like fakeTokens: List returns revoked rows too, so
// what the tests observe is the service's own narrowing, not the fake's.
type orderedTokens struct {
	rows     []domain.APIToken
	ops      []string // "insert <id>" / "revoke <id>", in call order
	pageSize int      // rows per List page; the service's limit is ignored
	lists    int
}

func (f *orderedTokens) Insert(_ context.Context, s db.TenantScope, t domain.APIToken) error {
	if t.OrgID != s.OrgID() {
		return fmt.Errorf("api_token_scope_mismatch")
	}
	f.rows = append(f.rows, t)
	f.ops = append(f.ops, "insert "+t.ID.String())
	return nil
}

func (f *orderedTokens) Revoke(_ context.Context, s db.TenantScope, tokenID uuid.UUID, at time.Time) (bool, error) {
	f.ops = append(f.ops, "revoke "+tokenID.String())
	for i, t := range f.rows {
		if t.ID != tokenID || t.OrgID != s.OrgID() {
			continue
		}
		if t.RevokedAt == nil {
			when := at
			f.rows[i].RevokedAt = &when
		}
		return true, nil
	}
	return false, nil
}

func (f *orderedTokens) List(
	_ context.Context, s db.TenantScope, kind domain.TokenKind, userID uuid.UUID, k db.Keyset,
) ([]domain.APIToken, db.Cursor, error) {
	f.lists++

	var matching []domain.APIToken
	for _, t := range f.rows {
		if t.OrgID == s.OrgID() && t.Kind == kind && (userID == uuid.Nil || t.UserID == userID) {
			matching = append(matching, t)
		}
	}

	start := 0
	if !k.Cursor.IsZero() {
		for i, t := range matching {
			if t.ID == k.Cursor.ID {
				start = i + 1
				break
			}
		}
	}
	end := min(start+f.pageSize, len(matching))
	page := matching[start:end]
	if end < len(matching) {
		// The cursor is opaque to the service; ID + a non-zero SortKey is enough.
		return page, db.Cursor{SortKey: epoch, ID: page[len(page)-1].ID, HasMore: true}, nil
	}
	return page, db.Cursor{}, nil
}

func (f *orderedTokens) Get(context.Context, db.TenantScope, uuid.UUID) (domain.APIToken, error) {
	panic("the ingest paths never read a single token back")
}

func (f *orderedTokens) TouchLastUsed(context.Context, db.TenantScope, uuid.UUID, time.Time) error {
	panic("an ingest token never authenticates through this service")
}

func (f *orderedTokens) ResolveByPrefix(context.Context, domain.TokenPrefix, time.Time) ([]domain.AuthenticatedToken, error) {
	panic("an ingest token never authenticates through this service")
}

// live returns the unrevoked rows scoped to one source.
func (f *orderedTokens) live(sourceID uuid.UUID) []domain.APIToken {
	var out []domain.APIToken
	for _, t := range f.rows {
		if t.SourceID == sourceID && t.RevokedAt == nil {
			out = append(out, t)
		}
	}
	return out
}

func newIngestFixture(t *testing.T, pageSize int) (*service.Service, *orderedTokens, db.TenantScope) {
	t.Helper()

	store := &orderedTokens{pageSize: pageSize}
	scope, err := db.NewTenantScope(id.New())
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	svc := service.New(service.Deps{Tokens: store, Clock: clock.NewFake(epoch)})
	return svc, store, scope
}

// TestAnIngestTokenIsMintedByItsOwnRecipe pins the shape that is deliberately
// NOT the PAT's: kind-relative prefix, fixed name convention, source scope, no
// user — and, like every credential here, a stored sha256 and never the secret.
func TestAnIngestTokenIsMintedByItsOwnRecipe(t *testing.T) {
	t.Parallel()

	svc, store, scope := newIngestFixture(t, 200)
	sourceID := id.New()

	secret, prefix, err := svc.IssueIngestToken(ctx(), scope, sourceID)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	if !strings.HasPrefix(secret, domain.SecretPrefixIngest) {
		t.Fatalf("secret %q does not announce its kind", secret[:min(15, len(secret))])
	}
	if got := len(secret) - len(domain.SecretPrefixIngest); got < 32 {
		t.Fatalf("the random half is %d characters; 32 bytes of entropy cannot fit", got)
	}
	// ⚠️ The split is KIND-RELATIVE: fifteen characters, not a PAT's twelve. The
	// fixed-twelve bug failed api_tokens_prefix_ck on every single call and made
	// `POST /api/v1/sources` return 422 for the life of the product.
	if len(prefix) != domain.TokenPrefixLenIngest {
		t.Fatalf("prefix %q is %d characters, want the kind-relative %d",
			prefix, len(prefix), domain.TokenPrefixLenIngest)
	}
	if prefix != secret[:domain.TokenPrefixLenIngest] {
		t.Fatalf("the returned prefix %q is not the head of the secret", prefix)
	}

	rows := store.live(sourceID)
	if len(rows) != 1 {
		t.Fatalf("live tokens for the source = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Kind != domain.TokenKindIngest || row.OrgID != scope.OrgID() {
		t.Fatalf("the row names the wrong credential: %+v", row)
	}
	if row.UserID != uuid.Nil {
		t.Fatal("an ingest token was given a user; it belongs to a source and nobody")
	}
	if row.SourceID != sourceID {
		t.Fatalf("source_id = %s, want %s", row.SourceID, sourceID)
	}
	if want := "ingest:" + sourceID.String(); row.Name != want {
		t.Fatalf("name = %q, want the convention %q", row.Name, want)
	}
	if row.Hash != digestOf(t, secret) {
		t.Fatal("the stored hash is not the sha256 of the issued secret")
	}
	if rendered := fmt.Sprintf("%+v", row); strings.Contains(rendered, secret) {
		t.Fatalf("the plaintext is reachable by formatting the row: %s", rendered)
	}
}

// TestARotationMintsBeforeItRevokes is the production incident, pinned as an
// ordering: the first write of a rotation is the INSERT, the old token is
// revoked after it, and the token just minted is excluded from its own sweep.
func TestARotationMintsBeforeItRevokes(t *testing.T) {
	t.Parallel()

	svc, store, scope := newIngestFixture(t, 200)
	sourceID := id.New()

	if _, _, err := svc.IssueIngestToken(ctx(), scope, sourceID); err != nil {
		t.Fatalf("first mint: %v", err)
	}
	old := store.live(sourceID)[0]

	store.ops = nil
	secret, _, err := svc.IssueIngestToken(ctx(), scope, sourceID)
	if err != nil {
		t.Fatalf("rotation: %v", err)
	}

	if len(store.ops) == 0 || !strings.HasPrefix(store.ops[0], "insert ") {
		t.Fatalf("the rotation's first write is %v, want the mint — revoke-first left a "+
			"source with zero working credentials when the mint failed, and Alertmanager "+
			"treats the resulting 401 as permanent", store.ops)
	}
	wantRevoke := "revoke " + old.ID.String()
	found := false
	for _, op := range store.ops[1:] {
		if op == wantRevoke {
			found = true
		}
	}
	if !found {
		t.Fatalf("the old token was not revoked after the mint: %v", store.ops)
	}

	live := store.live(sourceID)
	if len(live) != 1 {
		t.Fatalf("live tokens after a rotation = %d, want exactly the new one", len(live))
	}
	if live[0].Hash != digestOf(t, secret) {
		t.Fatal("the surviving token is not the one whose secret was returned — the sweep " +
			"revoked the credential it was called to mint")
	}
}

// TestRevokeExceptWalksEveryPage seeds more live tokens than one List page
// serves, so a sweep that trusted its first page would leave working
// credentials behind on a source that was just told everything is revoked.
func TestRevokeExceptWalksEveryPage(t *testing.T) {
	t.Parallel()

	svc, store, scope := newIngestFixture(t, 2)
	sourceID, otherSource := id.New(), id.New()

	// Five live tokens on the source — three pages at the fake's size — plus one
	// on ANOTHER source that must survive the sweep untouched.
	for range 5 {
		if _, _, err := svc.IssueIngestToken(ctx(), scope, sourceID); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if _, _, err := svc.IssueIngestToken(ctx(), scope, otherSource); err != nil {
		t.Fatalf("seed other source: %v", err)
	}
	// Rotation revokes as it mints, so re-arm the seeds as live rows directly.
	for i := range store.rows {
		store.rows[i].RevokedAt = nil
	}

	store.lists = 0
	if err := svc.RevokeIngestTokens(ctx(), scope, sourceID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if got := store.live(sourceID); len(got) != 0 {
		t.Fatalf("%d live token(s) survived past the first page; a sweep that trusts one "+
			"page leaves working credentials on a source the caller was told is closed", len(got))
	}
	if got := store.live(otherSource); len(got) != 1 {
		t.Fatalf("the sweep crossed sources: %d live token(s) remain on the other source, want 1", len(got))
	}
	if store.lists < 3 {
		t.Fatalf("List was called %d time(s) over three pages of tokens; the sweep did not "+
			"follow its cursor to the end", store.lists)
	}
}
