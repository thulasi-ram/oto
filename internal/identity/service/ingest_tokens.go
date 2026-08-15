package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// The per-source ingest credential (SPEC §G.2).
//
// An ingest token lives in `api_tokens` like a PAT, and the two mints are
// DELIBERATELY SEPARATE OPERATIONS: they are similar, not the same. An ingest
// token is scoped to exactly one AlertSource and belongs to NO user, so it
// cannot go through the PAT path — different secret prefix, different kind and
// therefore a different kind-relative display prefix, a fixed name convention
// instead of a caller-chosen name, no expiry, and a rotation that revokes what
// came before. What the two share are the primitives every credential of this
// table is made of: SecretEntropyBytes, digest, PrefixOfSecret. Merging the
// recipes would make one mint's rule the other's bug.
//
// ⛔ THE SECRET IS RETURNED EXACTLY ONCE. Only its sha256 is stored, and there
// is no method here that reads one back because there is nothing to read.

// inTx runs fn in one transaction when a runner is wired, inline otherwise.
func (s *Service) inTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if s.tx == nil {
		return fn(ctx)
	}
	return s.tx.InTx(ctx, fn)
}

// IssueIngestToken mints a fresh ingest token for the source, revokes every
// one that came before it, and returns the plaintext secret and its display
// prefix. It satisfies `sources/service.IngestTokens`, wired in internal/app.
func (s *Service) IssueIngestToken(
	ctx context.Context, scope db.TenantScope, sourceID uuid.UUID,
) (string, string, error) {
	// ⭐ MINT FIRST, REVOKE SECOND, BOTH IN ONE TRANSACTION.
	//
	// It used to revoke first and mint second, reasoning that a window with two
	// live tokens is a window in which a leaked one still works. That reasoning is
	// right about the window and wrong about the failure: revoke-then-mint means a
	// mint that fails for ANY reason leaves the source with ZERO working
	// credentials, and because Alertmanager treats 401 as permanent and never
	// retries it, every alert sent afterwards is destroyed rather than delayed.
	// That is precisely the silent loss ADR 0007 exists to prevent, and it is
	// exactly what happened: one probe of `rotate-token` against the prefix bug
	// revoked every live ingest token in the org and left nothing behind it.
	//
	// This order has no such failure. The two writes commit together, so an
	// observer inside the transaction is the only thing that ever sees both
	// tokens live, and a failure anywhere rolls the whole rotation back to "the
	// old token still works". The atomic window is a transaction, not a race.
	var (
		secret string
		prefix string
	)
	err := s.inTx(ctx, func(ctx context.Context) error {
		s2, minted, err := s.mintIngestToken(ctx, scope, sourceID)
		if err != nil {
			return err
		}
		// Revoking by id EXCLUDES the token just minted, so the new credential is
		// never revoked by the sweep that clears the old ones.
		if err := s.revokeIngestExcept(ctx, scope, sourceID, minted.ID); err != nil {
			return err
		}
		secret, prefix = s2, minted.Prefix.String()
		return nil
	})
	if err != nil {
		return "", "", err
	}
	return secret, prefix, nil
}

// mintIngestToken inserts one fresh ingest token and returns its plaintext
// secret alongside the stored row.
func (s *Service) mintIngestToken(
	ctx context.Context, scope db.TenantScope, sourceID uuid.UUID,
) (string, domain.APIToken, error) {
	now := s.clk.Now().UTC()
	secret := domain.SecretPrefixIngest + id.Token(SecretEntropyBytes)
	hash, err := digest(secret)
	if err != nil {
		return "", domain.APIToken{}, errs.Internal("token_hash_failed", err)
	}
	// ⚠️ The split is KIND-RELATIVE. `oto_ingest_` is eleven characters, so this
	// prefix is fifteen and not the twelve a PAT's is; a fixed twelve produced
	// `oto_ingest_X` and failed api_tokens_prefix_ck on every single call, which
	// is what made `POST /api/v1/sources` return 422 for the life of the product.
	prefix, err := domain.PrefixOfSecret(secret)
	if err != nil {
		return "", domain.APIToken{}, errs.Internal("token_prefix_failed", err)
	}

	token, err := domain.NewAPIToken(domain.NewAPITokenParams{
		ID:        id.New(),
		OrgID:     scope.OrgID(),
		Kind:      domain.TokenKindIngest,
		Name:      "ingest:" + sourceID.String(),
		Hash:      hash,
		Prefix:    prefix,
		SourceID:  sourceID,
		CreatedAt: now,
	})
	if err != nil {
		return "", domain.APIToken{}, err
	}
	if err := s.tokens.Insert(ctx, scope, token); err != nil {
		return "", domain.APIToken{}, err
	}

	// The prefix, never the secret — the same rule as the PAT mint's log line:
	// this is the record that a credential was minted, and it identifies which
	// one without being one.
	s.log.InfoContext(ctx, "identity: ingest token issued",
		"token_prefix", token.Prefix.String(),
		"source_id", sourceID,
		"org_id", scope.OrgID(),
	)
	return secret, token, nil
}

// RevokeIngestTokens revokes every live ingest token scoped to the source.
// Deleting a source that could still be pushed to would be a soft delete in
// name only, which is why the sources module calls this beside its own delete.
func (s *Service) RevokeIngestTokens(
	ctx context.Context, scope db.TenantScope, sourceID uuid.UUID,
) error {
	return s.revokeIngestExcept(ctx, scope, sourceID, uuid.Nil)
}

// revokeIngestExcept revokes every live ingest token for the source except
// `keep`.
//
// The exclusion is what lets a rotation mint before it revokes: without it the
// sweep would immediately revoke the token it was called to replace.
func (s *Service) revokeIngestExcept(
	ctx context.Context, scope db.TenantScope, sourceID, keep uuid.UUID,
) error {
	now := s.clk.Now().UTC()

	// An ingest token belongs to no user, so the org's ingest tokens are listed
	// and narrowed by source here. There is at most a handful per source.
	page := db.Keyset{Limit: 200}
	for {
		tokens, cursor, err := s.tokens.List(ctx, scope, domain.TokenKindIngest, uuid.Nil, page)
		if err != nil {
			return err
		}
		for _, t := range tokens {
			if t.SourceID != sourceID || (keep != uuid.Nil && t.ID == keep) {
				continue
			}
			if _, err := s.tokens.Revoke(ctx, scope, t.ID, now); err != nil {
				return err
			}
		}
		if !cursor.HasMore || cursor.IsZero() {
			return nil
		}
		page.Cursor = cursor
	}
}
