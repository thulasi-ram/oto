package service

import (
	"context"
	"crypto/subtle"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// CreateTokenCommand mints a personal access token.
type CreateTokenCommand struct {
	Name string
	// ExpiresAt is optional. Nil means the token does not expire.
	ExpiresAt *time.Time
}

// IssuedToken is a newly minted token and its secret.
//
// ⚠️ Secret IS THE ONLY TIME THE PLAINTEXT EXISTS ANYWHERE IN OTO. It is
// returned from IssueToken, rendered into one response body, and dropped. It is
// not stored, not logged, and not recoverable — a lost token is replaced, never
// retrieved (contract: `APITokenCreatedDTO`).
type IssuedToken struct {
	Token  domain.APIToken
	Secret string
}

// IssueToken mints a PAT for the calling user.
//
// The secret is `oto_pat_` plus 256 bits of crypto/rand; the row stores its
// sha256 and the display prefix. There is no field on
// domain.APIToken that could hold the plaintext, so "the secret is never stored"
// is a property of the types rather than a habit of this function.
func (s *Service) IssueToken(
	ctx context.Context, scope db.TenantScope, p authn.Principal, cmd CreateTokenCommand,
) (IssuedToken, error) {
	if p.UserID == uuid.Nil {
		// A PAT belongs to a user (api_tokens_pat_user). There is no such thing as
		// an org-owned PAT, and minting one on behalf of a token would let a
		// credential reproduce itself.
		return IssuedToken{}, errs.Forbidden("token_requires_user", "only a signed-in user can create a token")
	}

	now := s.clk.Now()
	if cmd.ExpiresAt != nil && !cmd.ExpiresAt.After(now) {
		return IssuedToken{}, errs.Validation("invalid_token_expiry", "expires_at must be in the future").
			WithViolations(errs.Violation{
				Field: "expires_at", Code: "gt", Message: "expires_at must be in the future",
			})
	}

	secret := domain.SecretPrefixPAT + id.Token(SecretEntropyBytes)
	hash, err := digest(secret)
	if err != nil {
		return IssuedToken{}, errs.Internal("token_hash_failed", err)
	}
	prefix, err := domain.PrefixOfSecret(secret)
	if err != nil {
		return IssuedToken{}, errs.Internal("token_prefix_failed", err)
	}

	token, err := domain.NewAPIToken(domain.NewAPITokenParams{
		ID:        id.New(),
		OrgID:     scope.OrgID(),
		UserID:    p.UserID,
		Kind:      domain.TokenKindPAT,
		Name:      cmd.Name,
		Hash:      hash,
		Prefix:    prefix,
		ExpiresAt: cmd.ExpiresAt,
		CreatedAt: now,
	})
	if err != nil {
		return IssuedToken{}, err
	}

	if err := s.tokens.Insert(ctx, scope, token); err != nil {
		return IssuedToken{}, err
	}

	// The prefix, never the secret. This is the record that a credential was
	// minted; it identifies which one without being one.
	s.log.InfoContext(ctx, "identity: personal access token issued",
		"token_prefix", token.Prefix.String(),
		"user_id", p.UserID,
		"org_id", scope.OrgID(),
	)

	return IssuedToken{Token: token, Secret: secret}, nil
}

// ListTokens pages the caller's live personal access tokens.
//
// It is narrowed to the CALLING USER's tokens rather than the org's. v1 has no
// RBAC — a member can do anything within their org (R2) — but a token list is
// not an authorisation question: showing Priya's laptop token in Sam's settings
// screen is a privacy failure and a support burden, not a permission model. The
// tokens remain revocable by anybody in the org through their id.
func (s *Service) ListTokens(
	ctx context.Context, scope db.TenantScope, p authn.Principal, k db.Keyset,
) ([]domain.APIToken, db.Cursor, error) {
	if p.UserID == uuid.Nil {
		return nil, db.Cursor{}, errs.Forbidden("token_requires_user", "only a signed-in user can list tokens")
	}
	return s.tokens.List(ctx, scope, domain.TokenKindPAT, p.UserID, k)
}

// RevokeToken revokes a token within the caller's org.
//
// It is IDEMPOTENT: revoking an already-revoked token succeeds and does not move
// the revocation timestamp, because that timestamp is when the credential
// stopped working. A DELETE that answered 404 the second time would make the
// contract's "revocation takes effect within 60 seconds" impossible to retry
// safely.
//
// A token belonging to another org is reported as not found, never as forbidden:
// a 403 would confirm the id exists somewhere.
func (s *Service) RevokeToken(ctx context.Context, scope db.TenantScope, tokenID uuid.UUID) error {
	found, err := s.tokens.Revoke(ctx, scope, tokenID, s.clk.Now())
	if err != nil {
		return err
	}
	if !found {
		return errs.NotFound("token_not_found", "no such token")
	}
	return nil
}

// GetToken returns one token within the caller's org, without its secret —
// there is no secret to return.
func (s *Service) GetToken(ctx context.Context, scope db.TenantScope, tokenID uuid.UUID) (domain.APIToken, error) {
	return s.tokens.Get(ctx, scope, tokenID)
}

// ResolveBearer verifies an `Authorization: Bearer oto_pat_…` credential and
// mints the Principal every other module reads out of the request context.
//
// ⭐ THE VERIFICATION, in the order it must happen:
//
//  1. The prefix must announce a PAT. An `oto_ingest_…` is refused here and
//     again by the middleware before this is ever called.
//  2. The public display prefix selects a small candidate set from the database.
//     That lookup decides nothing.
//  3. The presented secret's sha256 is compared against each candidate with
//     crypto/subtle.ConstantTimeCompare. THIS is what authenticates.
//  4. Usable() re-checks revocation and expiry, which the SQL already excluded.
//
// EVERY candidate is compared even after a match is found. Returning early would
// make the response time a function of the matching row's position in the
// result set, which over enough probes is a side channel into the table's
// physical order.
func (s *Service) ResolveBearer(ctx context.Context, secret string) (authn.Principal, error) {
	kind, ok := domain.KindOfSecret(secret)
	if !ok || kind != domain.TokenKindPAT {
		return authn.Principal{}, unauthenticated()
	}

	prefix, err := domain.PrefixOfSecret(secret)
	if err != nil {
		return authn.Principal{}, unauthenticated()
	}
	presented, err := digest(secret)
	if err != nil {
		return authn.Principal{}, unauthenticated()
	}

	now := s.clk.Now()
	candidates, err := s.tokens.ResolveByPrefix(ctx, prefix, now)
	if err != nil || len(candidates) == 0 {
		return authn.Principal{}, unauthenticated()
	}

	matched, unmatched := -1, 1
	for i, c := range candidates {
		eq := subtle.ConstantTimeCompare(presented.Bytes(), c.Token.Hash.Bytes())
		// Branch-free "keep the first match": `unmatched` is cleared by the first
		// hit and never set again, so the loop body executes identically for every
		// candidate whatever the input was.
		keep := eq & unmatched
		matched = subtle.ConstantTimeSelect(keep, i, matched)
		unmatched &^= keep
	}
	if matched < 0 {
		return authn.Principal{}, unauthenticated()
	}

	hit := candidates[matched]
	if !hit.Token.Usable(now) || !hit.Subject.Valid() {
		return authn.Principal{}, unauthenticated()
	}

	p := authn.Principal{
		Kind:        authn.KindPAT,
		OrgID:       hit.Subject.OrgID,
		UserID:      hit.Subject.UserID,
		DisplayName: hit.Subject.DisplayName,
		Email:       hit.Subject.Email.String(),
		OrgSlug:     hit.Subject.OrgSlug,
		OrgName:     hit.Subject.OrgName,
		TokenID:     hit.Token.ID,
	}
	if hit.Token.ExpiresAt != nil {
		p.ExpiresAt = *hit.Token.ExpiresAt
	}

	s.touch(ctx, p, hit.Token, now)
	return p, nil
}

// touch refreshes `last_used_at`, at most once per TouchInterval.
//
// A failure here is DELIBERATELY SWALLOWED at debug level: a request must not
// fail because a bookkeeping UPDATE could not get a row lock, and a warn on
// every contended write would make the log useless during exactly the traffic
// that causes it.
func (s *Service) touch(ctx context.Context, p authn.Principal, t domain.APIToken, now time.Time) {
	if t.LastUsedAt != nil && now.Sub(*t.LastUsedAt) < TouchInterval {
		return
	}
	scope, err := p.Scope()
	if err != nil {
		return
	}
	if err := s.tokens.TouchLastUsed(ctx, scope, t.ID, now); err != nil {
		s.log.DebugContext(ctx, "identity: could not record token use",
			"token_prefix", t.Prefix.String(), "error", err.Error())
	}
}
