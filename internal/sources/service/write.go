package service

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/platform/idempotency"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// ⭐⭐ THIS FILE IS THE MODULE'S WRITE PATH, AND IT IS HERE BECAUSE THE
// TRANSACTION BOUNDARY IS NOT AN HTTP CONCERN.
//
// Every one of these operations used to live in `internal/sources/api`: the
// handler opened the transaction, decided the order of three writes across two
// modules' tables, and made the business decision that a supplied credential
// rotates in place rather than being replaced. Anything that is not an HTTP
// request — a job, a CLI, a future gRPC surface, a test with no router — could not
// create a source at all without re-deriving that ordering, and the one handler
// that did re-derive it got it wrong and committed twice.
//
// The handler now binds, calls one method here, and maps the error.

// The two operationIds an `Idempotency-Key` is claimed under here. One key must
// not be replayable across two different operations, so the operation is part of
// the claim's identity, and these are the contract's own operationIds spelled
// once so a claim and the contract cannot drift.
var (
	opCreateSource            = idempotency.MustOperation("createSource")
	opRotateSourceIngestToken = idempotency.MustOperation("rotateSourceIngestToken")
	opCreateCluster           = idempotency.MustOperation("createCluster")
)

// The codes the write path mints. Each is a DEPLOYMENT fact — a collaborator this
// process was built without — except CodeCredentialEmpty, which is a caller one.
const (
	// CodeTokenIssuerUnavailable means no ingest-token issuer is wired, so no
	// source can be registered and none can rotate its credential.
	CodeTokenIssuerUnavailable = "sources_token_issuer_unavailable"
	// CodeCredentialStoreUnavailable means a credential was supplied and there is
	// nowhere to seal it.
	CodeCredentialStoreUnavailable = "sources_credential_store_unavailable"
	// CodeIdempotencyUnavailable means the caller sent an `Idempotency-Key` this
	// deployment cannot honour.
	CodeIdempotencyUnavailable = "idempotency_unavailable"
	// CodeCredentialEmpty means a credential was supplied carrying no values.
	CodeCredentialEmpty = "credential_empty"
)

// CredentialInput is secret material for one upstream, in PLAIN TYPES.
//
// ⛔ Values is secret. It arrives on a write-only DTO field, is handed straight to
// the sealer, and is never logged, echoed or retained. Kind `none` DETACHES: the
// existing row is left in place (other things may reference it) and the source
// stops pointing at it.
type CredentialInput struct {
	Kind   string
	Values map[string]string
}

// Idempotency is the caller's `Idempotency-Key` intent for one write.
//
// ⭐⭐ THE CLAIM IT ASKS FOR IS TAKEN INSIDE THE OPERATION'S OWN TRANSACTION, AND
// AFTER THE ACT IT GUARDS, so a key somebody already holds rolls that act back
// with it. That is the difference between "your retry was refused" and "your
// retry minted a second live credential and told you it was a duplicate".
type Idempotency struct {
	// Keyed reports that the caller sent a key at all. False means every field
	// below is ignored and no claim is taken.
	Keyed bool
	Key   idempotency.Key
	// Principal is who sent it. A key is a client's private handle on its own
	// retry, so a claim is keyed by the principal as well as the org: one org
	// member's key must never be able to refuse another's request.
	Principal authn.Principal
	// RequestHash is what "the same request" means for an operation that carries a
	// BODY, and is the sha256 of the bytes the caller actually sent. RotateIngestToken
	// ignores it and digests the source it is rotating instead — see there.
	RequestHash idempotency.RequestHash
}

// IssuedIngest is a source and the ingest credential it was just given.
//
// ⚠️ Secret IS THE ONLY TIME THE PLAINTEXT EXISTS ANYWHERE IN OTO. It is returned
// from here, rendered into one response body, and dropped. Only its sha256 is
// stored, so a lost token is replaced, never retrieved.
type IssuedIngest struct {
	Source domain.Source
	Secret string
	// Prefix is the secret's kind literal plus four characters, retained for
	// display so the token can be identified later without being recoverable.
	Prefix string
}

// CreateCommand registers one upstream.
type CreateCommand struct {
	// Draft is everything about the source itself. Its AuthCredentialID is
	// IGNORED and overwritten: the credential id is produced by sealing Credential
	// inside this call's transaction, and a caller that could assert one would be
	// asserting a row it never wrote.
	Draft domain.SourceDraft
	// Credential is the optional upstream secret. Nil, empty or kind `none` means
	// the upstream is unauthenticated.
	Credential  *CredentialInput
	Idempotency Idempotency
}

// UpdateCommand edits one upstream.
type UpdateCommand struct {
	// Patch is every field except the credential. AuthCredentialID is IGNORED and
	// set from Credential, because whether a supplied secret rotates in place or
	// becomes a new row is this service's decision and not the caller's.
	Patch domain.SourcePatch
	// Credential nil means LEAVE THE CREDENTIAL ALONE, which is a different
	// request from kind `none` — that one detaches it.
	Credential *CredentialInput
}

// Create registers a source, seals its credential and mints its ingest token.
//
// ⭐ THE SOURCE AND ITS CREDENTIAL COMMIT TOGETHER OR NOT AT ALL. They used to be
// three independent commits — seal the credential, insert the row, mint the token
// — and when the mint failed the row stayed. The result was a source that the
// settings screen shows as configured, whose webhook URL an operator has already
// pasted into `webhook_config`, and which answers 401 to every alert forever.
// Alertmanager does not retry a 4xx, so those alerts are simply gone.
//
// ⭐⭐ A RETRY CARRYING THE SAME `Idempotency-Key` IS REFUSED, NOT REPEATED,
// because this hands out a plaintext ingest token exactly once. The claim is taken
// in the same transaction as the mint, so a key somebody already holds rolls the
// whole create back and the caller is told the id of the source its first attempt
// made.
func (s *Service) Create(
	ctx context.Context, scope db.TenantScope, cmd CreateCommand,
) (IssuedIngest, error) {
	// ⭐⭐ BOTH PRECONDITIONS ARE CHECKED BEFORE ANYTHING IS MINTED. A claim with no
	// transaction to join is refused by `idempotency.Repository.Claim` — but that
	// refusal arrives AFTER the mint, and with no transaction there is nothing to
	// roll the mint back. Demanding the collaborators up front costs a caller one
	// `503`; discovering the gap late costs a source its only credential.
	if err := s.requireIssuer(); err != nil {
		return IssuedIngest{}, err
	}
	if err := s.requireClaims(cmd.Idempotency); err != nil {
		return IssuedIngest{}, err
	}

	var out IssuedIngest
	err := s.inTx(ctx, func(ctx context.Context) error {
		credentialID, err := s.sealCredential(ctx, scope, cmd.Credential)
		if err != nil {
			return err
		}
		draft := cmd.Draft
		draft.AuthCredentialID = credentialID

		created, err := s.repo.Create(ctx, scope, draft)
		if err != nil {
			return err
		}
		secret, prefix, err := s.tokens.IssueIngestToken(ctx, scope, created.ID)
		if err != nil {
			return err
		}
		if cmd.Idempotency.Keyed {
			// AFTER the mint, because the claim records what the call created — the
			// SOURCE id, which is the id a caller who never received the ingest token
			// needs in order to find it and rotate its credential.
			if err := s.claim(ctx, scope, cmd.Idempotency,
				opCreateSource, cmd.Idempotency.RequestHash, created.ID); err != nil {
				return err
			}
		}
		out = IssuedIngest{Source: created, Secret: secret, Prefix: prefix}
		return nil
	})
	if err != nil {
		return IssuedIngest{}, err
	}
	return out, nil
}

// Update applies a partial change to one source.
//
// ⭐ A SUPPLIED CREDENTIAL ROTATES THE EXISTING SECRET IN PLACE when there is one,
// so the source never spends a moment pointing at nothing. That is a rule about
// what an edit MEANS, which is why it lives here and not in whichever transport
// happened to ask: the read of the current credential id and the write that
// replaces it are one transaction, and a rotation that failed leaves the source
// pointing where it already did.
func (s *Service) Update(
	ctx context.Context, scope db.TenantScope, id uuid.UUID, cmd UpdateCommand,
) (domain.Source, error) {
	var out domain.Source
	err := s.inTx(ctx, func(ctx context.Context) error {
		patch := cmd.Patch
		if cmd.Credential != nil {
			existing, err := s.source(ctx, scope, id)
			if err != nil {
				return err
			}
			newID, err := s.rotateCredential(ctx, scope, existing.AuthCredentialID, cmd.Credential)
			if err != nil {
				return err
			}
			patch.AuthCredentialID = &newID
		}
		updated, err := s.repo.Update(ctx, scope, id, patch)
		if err != nil {
			return err
		}
		out = updated
		return nil
	})
	if err != nil {
		return domain.Source{}, err
	}
	return out, nil
}

// SoftDelete retires a source and REVOKES its ingest token, so a source that has
// been deleted cannot still be pushed to. ALERT HISTORY IS RETAINED: deleting a
// source must never erase the record of what it once reported.
//
// ⭐ THE DELETION AND THE REVOCATION COMMIT TOGETHER OR NOT AT ALL, for the same
// reason Create does: they were two independent commits, and a failure in the
// second left a source that the settings screen shows as gone while its ingest
// token stayed live and usable — a soft delete in name only, and a credential
// nobody can see in order to revoke it.
//
// THE ORDER IS DELIBERATE. The soft delete goes first because it is the call that
// decides the answer: it reports not-found for an id that does not exist or is
// already deleted, and running it first keeps that 404 free of any write against
// another source's tokens. It also takes `alert_sources` before `api_tokens`,
// which is the lock order Create uses.
func (s *Service) SoftDelete(ctx context.Context, scope db.TenantScope, id uuid.UUID) error {
	return s.inTx(ctx, func(ctx context.Context) error {
		if err := s.repo.SoftDelete(ctx, scope, id); err != nil {
			return err
		}
		if s.tokens == nil {
			// A deployment with no issuer minted no ingest token either, so there is
			// nothing this delete failed to revoke.
			return nil
		}
		return s.tokens.RevokeIngestTokens(ctx, scope, id)
	})
}

// RotateIngestToken replaces one source's ingest credential.
//
// The new secret is returned EXACTLY ONCE and the old one stops working
// immediately. Between rotation and reconfiguration the old token is rejected with
// `401`, which Alertmanager treats as PERMANENT, so notifications sent in that
// window are lost — which is why the contract tells the operator to update the
// receiver promptly and why nothing here delays the revocation to be kind.
//
// ⛔ THE ONE THING IT MUST NEVER DO IS LEAVE ZERO WORKING TOKENS. The issuer used
// to revoke first and mint second; a mint that failed therefore revoked the
// source's only credential and left nothing in its place, and because Alertmanager
// never retries a 401 the alerts sent afterwards were destroyed rather than
// delayed — the precise failure ADR 0007 exists to prevent. The whole rotation is
// one transaction and mints before it revokes, so the failure mode is "nothing
// changed" instead of "nothing works".
//
// ⭐⭐ A RETRY CARRYING THE SAME `Idempotency-Key` IS REFUSED, NOT REPEATED, and it
// matters more here than on a create: a rotation does not merely mint a second
// secret, it REVOKES the one it minted a moment ago, so an automatic retry after a
// dropped response destroys the credential the caller may actually be holding and
// hands the replacement to a response that never arrived.
func (s *Service) RotateIngestToken(
	ctx context.Context, scope db.TenantScope, id uuid.UUID, idem Idempotency,
) (IssuedIngest, error) {
	if err := s.requireIssuer(); err != nil {
		return IssuedIngest{}, err
	}
	if err := s.requireClaims(idem); err != nil {
		return IssuedIngest{}, err
	}

	var out IssuedIngest
	err := s.inTx(ctx, func(ctx context.Context) error {
		found, err := s.source(ctx, scope, id)
		if err != nil {
			return err
		}
		secret, prefix, err := s.tokens.IssueIngestToken(ctx, scope, found.ID)
		if err != nil {
			return err
		}
		if idem.Keyed {
			// The claim carries no `created_ref`: the issuer port returns a secret and
			// a display prefix, never the row id, and there is no version of it that
			// should return the credential itself.
			//
			// ⭐ THE DIGEST IS OF THE SOURCE BEING ROTATED, not of the empty body this
			// operation declares. A bodyless request digests to a CONSTANT and the
			// source id is not in the claim tuple, so one key would make "rotate source
			// A" and "rotate source B" indistinguishable and refuse the second as a
			// replay of a rotation that touched a different source entirely. Folded in,
			// the two are the different requests they are, and a true retry against the
			// same source still digests identically and still replays.
			if err := s.claim(ctx, scope, idem, opRotateSourceIngestToken,
				idempotency.HashTargetedRequest(found.ID, nil), uuid.Nil); err != nil {
				return err
			}
		}
		out = IssuedIngest{Source: found, Secret: secret, Prefix: prefix}
		return nil
	})
	if err != nil {
		return IssuedIngest{}, err
	}
	return out, nil
}

// ------------------------------------------------------------------ clusters

// CodeClustersUnavailable means no cluster registry is wired, so no
// identity/failure domain can be registered here.
const CodeClustersUnavailable = "clusters_unavailable"

// CreateCluster registers an identity/failure domain.
//
// ⛔ `cluster_key` participates in ALERT IDENTITY (§C.2) and cannot be changed
// afterwards: the same label set in two clusters is two different alerts, which
// is correct because they have different blast radii. Choosing it is therefore a
// decision, and this is the only path that lets anybody make it.
//
// ⭐⭐ IT IS HERE, AND NOT IN A HANDLER, BECAUSE THE TRANSACTION BOUNDARY IS NOT
// AN HTTP CONCERN. `createCluster` went api → repository directly, so the
// `Idempotency-Key` claim had nowhere to be taken that was inside the insert's
// transaction. This is the same move `createSource` made and for the same reason.
//
// ⭐⭐ A RETRY CARRYING THE SAME KEY IS REPLAYED, NOT REFUSED, AND THAT IS THE
// DIFFERENCE BETWEEN THIS AND `Create` ABOVE. A source's `201` hands out a
// plaintext ingest token that genuinely cannot be produced twice, so a held key
// there is a `409` naming what already exists. A cluster's `201` carries no
// secret, so the honest answer to a retry is the cluster the first attempt made —
// which is what the header's own description promised all along. Before this,
// `clusters_key_uniq` answered a same-body retry with a duplicate-key `409` that
// named nothing, so a client that lost its response could not even learn the id
// of what it had already created.
//
// ⭐ THE CLAIM IS TAKEN BEFORE THE INSERT, which is why the id is minted here. A
// claim taken afterwards would never be reached: the retry's INSERT hits
// `clusters_key_uniq` first and fails with the very conflict the claim exists to
// replace.
func (s *Service) CreateCluster(
	ctx context.Context, scope db.TenantScope, key, displayName string, idem Idempotency,
) (domain.Cluster, error) {
	if s.clusters == nil {
		return domain.Cluster{}, errs.Unavailable(CodeClustersUnavailable,
			"the cluster registry is not configured in this deployment", 0)
	}
	if err := s.requireClaims(idem); err != nil {
		return domain.Cluster{}, err
	}

	var (
		out      domain.Cluster
		replayOf uuid.UUID
	)
	clusterID := id.New()
	err := s.inTx(ctx, func(ctx context.Context) error {
		if idem.Keyed {
			res, err := s.claims.Claim(ctx, scope, idempotency.Claim{
				OrgID:       scope.OrgID(),
				PrincipalID: idem.Principal.UserID,
				Operation:   opCreateCluster,
				Key:         idem.Key,
				RequestHash: idem.RequestHash,
				CreatedRef:  clusterID,
				ClaimedAt:   s.clk.Now().UTC(),
			})
			if err != nil {
				return err
			}
			if res.Outcome == idempotency.Conflicted {
				// One key, two different bodies. That is not a retry, it is a second
				// request wearing the first one's name, and the contract has always
				// said so with a `409`.
				return idempotency.Reuse(res)
			}
			if !res.Fresh() {
				if res.Existing.CreatedRef == uuid.Nil {
					return idempotency.Reuse(res)
				}
				replayOf = res.Existing.CreatedRef
				return errReplayCluster
			}
		}
		created, err := s.clusters.Create(ctx, scope, clusterID, key, displayName)
		if err != nil {
			return err
		}
		out = created
		return nil
	})
	if errors.Is(err, errReplayCluster) {
		// Read OUTSIDE the rolled-back transaction, so what comes back is the row
		// the first attempt committed and nothing this one attempted.
		return s.clusters.Get(ctx, scope, replayOf)
	}
	if err != nil {
		return domain.Cluster{}, err
	}
	return out, nil
}

// errReplayCluster carries a replayed claim out of its own transaction so the
// insert beside it rolls back. It never reaches a caller.
var errReplayCluster = errors.New("this idempotency key already registered a cluster")

// ------------------------------------------------------------------- helpers

// inTx runs fn in one transaction when a unit of work is wired, and inline
// otherwise.
func (s *Service) inTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if s.tx == nil {
		return fn(ctx)
	}
	return s.tx.InTx(ctx, fn)
}

// claim takes the caller's key for op, and turns a key somebody already holds into
// the contract's `409`.
//
// The hash is passed in because what "the same request" means differs by
// operation: a create is identified by its body, a rotation by the source whose
// credential it replaces.
func (s *Service) claim(
	ctx context.Context, scope db.TenantScope, idem Idempotency,
	op idempotency.Operation, hash idempotency.RequestHash, created uuid.UUID,
) error {
	res, err := s.claims.Claim(ctx, scope, idempotency.Claim{
		OrgID:       scope.OrgID(),
		PrincipalID: idem.Principal.UserID,
		Operation:   op,
		Key:         idem.Key,
		RequestHash: hash,
		CreatedRef:  created,
		ClaimedAt:   s.clk.Now().UTC(),
	})
	if err != nil {
		return err
	}
	if !res.Fresh() {
		return idempotency.Reuse(res)
	}
	return nil
}

// requireIssuer refuses an operation that would mint a token there is nowhere to
// mint. A deployment wired without an issuer is a misconfiguration, not a caller
// error, and `503` is the one status that says so without inviting a retry of the
// same broken request as a 500 would.
func (s *Service) requireIssuer() error {
	if s.tokens != nil {
		return nil
	}
	return errs.Unavailable(CodeTokenIssuerUnavailable,
		"ingest tokens cannot be minted in this deployment", 0)
}

// requireClaims refuses a KEYED request this deployment cannot honour.
//
// ⛔ IT IS REFUSED, NOT SERVED UNGUARDED. The defect this closes was a header the
// contract promised and the server ignored; ignoring it a second time because a
// collaborator is nil would reproduce it exactly, and the caller would have no way
// to tell a protected create from an unprotected one.
func (s *Service) requireClaims(idem Idempotency) error {
	if !idem.Keyed {
		return nil
	}
	if s.claims != nil && s.tx != nil {
		return nil
	}
	return errs.Unavailable(CodeIdempotencyUnavailable,
		"this deployment cannot honour Idempotency-Key", 0)
}

// sealCredential stores a supplied credential and returns its id, or nil.
//
// ⛔ The plaintext values travel from the caller straight into the sealer and are
// referenced nowhere else. Nothing in this file logs them.
func (s *Service) sealCredential(
	ctx context.Context, scope db.TenantScope, in *CredentialInput,
) (*uuid.UUID, error) {
	if in == nil || in.Kind == "" || in.Kind == "none" {
		return nil, nil
	}
	if err := s.requireSealer(len(in.Values)); err != nil {
		return nil, err
	}
	id, err := s.sealer.CreateCredential(ctx, scope, in.Kind, in.Values)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// rotateCredential re-seals an existing credential in place, or seals a new one
// when the source had none.
func (s *Service) rotateCredential(
	ctx context.Context, scope db.TenantScope, existing *uuid.UUID, in *CredentialInput,
) (*uuid.UUID, error) {
	if in == nil {
		return existing, nil
	}
	if in.Kind == "none" {
		// Detaching is expressed by supplying kind `none`: the row is left in place
		// (other things may reference it) and the source stops pointing at it.
		return nil, nil
	}
	if existing == nil {
		return s.sealCredential(ctx, scope, in)
	}
	if err := s.requireSealer(len(in.Values)); err != nil {
		return nil, err
	}
	if err := s.sealer.RotateCredential(ctx, scope, *existing, in.Kind, in.Values); err != nil {
		return nil, err
	}
	return existing, nil
}

// requireSealer refuses a credential this deployment cannot store, and one that
// carries nothing to store.
func (s *Service) requireSealer(values int) error {
	if s.sealer == nil {
		return errs.Unavailable(CodeCredentialStoreUnavailable,
			"credentials cannot be sealed in this deployment", 0)
	}
	if values == 0 {
		return errs.Validation(CodeCredentialEmpty, "a credential must carry at least one value",
			errs.Violation{Field: "credential/values", Code: "required", Message: "at least one value is required"})
	}
	return nil
}
