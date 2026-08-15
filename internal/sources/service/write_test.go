package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/idempotency"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// THE WRITE PATH, TESTED WHERE IT LIVES.
//
// Every assertion in this file used to be made through an httptest router in
// `internal/sources/api`, because that is where the transaction was. It moved
// (ticket 0869f21), so these moved with it — and that is the point: a job, a CLI
// or a future gRPC surface gets the same atomicity, the same ordering and the
// same `Idempotency-Key` semantics as the HTTP handler, and this file is what
// says so without a router anywhere in it.

/* -------------------------------------------------------------------------- */
/* 1. A source and its ingest token are ONE fact                              */
/* -------------------------------------------------------------------------- */

// ⭐ TestCreateCommitsTheSourceAndItsTokenTogether.
//
// The row and the credential used to be independent commits. A failure between
// them left an `alert_sources` row with no token: a source the settings screen
// shows as configured, whose webhook URL an operator has already pasted into
// `webhook_config`, and which answers 401 to every alert forever. Alertmanager
// never retries a 4xx, so those alerts are simply gone.
func TestCreateCommitsTheSourceAndItsTokenTogether(t *testing.T) {
	t.Parallel()

	svc, deps := newWriteService(t)
	issued, err := svc.Create(context.Background(), testScope(t), CreateCommand{
		Draft: testDraft(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if !deps.tx.committed {
		t.Fatal("the create did not run inside a unit of work")
	}
	if deps.repo.created != 1 || deps.tokens.issued != 1 {
		t.Fatalf("writes = %d source, %d token; want 1 and 1", deps.repo.created, deps.tokens.issued)
	}
	// ⛔ AND BOTH JOINED THE SAME ONE. `committed` alone stays true when one of the
	// two writes is moved back outside the closure, so each fake records whether
	// the context it was handed was the transaction's.
	if !deps.repo.createdInTx || !deps.tokens.issuedInTx {
		t.Fatalf("joined the unit of work: insert = %t, mint = %t",
			deps.repo.createdInTx, deps.tokens.issuedInTx)
	}
	if issued.Secret == "" || issued.Source.ID == uuid.Nil {
		t.Fatalf("the create returned no usable result: %+v", issued)
	}
}

// ⭐ TestCreateRollsBackWhenTheMintFails is the negative half, and it is the
// failure that produced the orphan rows.
func TestCreateRollsBackWhenTheMintFails(t *testing.T) {
	t.Parallel()

	svc, deps := newWriteService(t)
	deps.tokens.issueErr = errors.New("the identity store is having a bad day")

	if _, err := svc.Create(context.Background(), testScope(t), CreateCommand{Draft: testDraft()}); err == nil {
		t.Fatal("a failed mint returned success")
	}
	if deps.tx.committed {
		t.Fatal("the unit of work committed a source with no ingest credential")
	}
	if !deps.tx.rolledBack {
		t.Fatal("the unit of work neither committed nor rolled back")
	}
}

// TestCreateSealsTheCredentialInsideTheSameTransaction.
//
// The sealed secret is the third write of the three, and it is the FIRST to run:
// the source row has to be able to name it. A seal that committed on its own
// would leave an orphan credential every time the insert after it failed.
func TestCreateSealsTheCredentialInsideTheSameTransaction(t *testing.T) {
	t.Parallel()

	svc, deps := newWriteService(t)
	_, err := svc.Create(context.Background(), testScope(t), CreateCommand{
		Draft:      testDraft(),
		Credential: &CredentialInput{Kind: "bearer", Values: map[string]string{"token": "s3cret"}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if deps.sealer.created != 1 || !deps.sealer.createdInTx {
		t.Fatalf("seals = %d, joined the unit of work = %t", deps.sealer.created, deps.sealer.createdInTx)
	}
	if deps.repo.lastDraft.AuthCredentialID == nil ||
		*deps.repo.lastDraft.AuthCredentialID != deps.sealer.lastID {
		t.Fatalf("the source was inserted pointing at %v, not at the credential just sealed (%v)",
			deps.repo.lastDraft.AuthCredentialID, deps.sealer.lastID)
	}
}

// TestCreateRefusesACredentialWithNothingInIt keeps the caller-facing half of the
// credential rules where the sealing is.
func TestCreateRefusesACredentialWithNothingInIt(t *testing.T) {
	t.Parallel()

	svc, deps := newWriteService(t)
	_, err := svc.Create(context.Background(), testScope(t), CreateCommand{
		Draft:      testDraft(),
		Credential: &CredentialInput{Kind: "bearer"},
	})
	if errs.CodeOf(err) != CodeCredentialEmpty {
		t.Fatalf("code = %q, want %q", errs.CodeOf(err), CodeCredentialEmpty)
	}
	if deps.repo.created != 0 || deps.tokens.issued != 0 {
		t.Fatal("a refused credential still wrote a source or minted a token")
	}
}

// ⛔ TestCreateWithoutAnIssuerRefusesBeforeAnythingIsWritten.
//
// A deployment with no ingest-token issuer cannot register a source at all: a
// source that can never receive a webhook is not a source. `503` says so without
// inviting a retry of the same broken request, and it is raised BEFORE the
// transaction opens.
func TestCreateWithoutAnIssuerRefusesBeforeAnythingIsWritten(t *testing.T) {
	t.Parallel()

	svc, deps := newWriteService(t, func(o *Options) { o.Tokens = nil })
	_, err := svc.Create(context.Background(), testScope(t), CreateCommand{Draft: testDraft()})
	if errs.CodeOf(err) != CodeTokenIssuerUnavailable {
		t.Fatalf("code = %q, want %q", errs.CodeOf(err), CodeTokenIssuerUnavailable)
	}
	if deps.repo.created != 0 || deps.tx.committed || deps.tx.rolledBack {
		t.Fatal("the refusal still opened a unit of work or wrote a row")
	}
}

/* -------------------------------------------------------------------------- */
/* 2. The `Idempotency-Key` claim, inside the same transaction as the mint     */
/* -------------------------------------------------------------------------- */

// ⭐⭐ TestAClaimedKeyRollsTheWholeCreateBack.
//
// This is the guarantee the whole mechanism exists for. The claim runs AFTER the
// mint, inside the mint's transaction, so a key somebody already holds does not
// merely produce a 409 — it undoes the credential this call minted. Take the claim
// outside and the retry hands out a second live ingest token whose secret goes to
// a response the caller already gave up on.
func TestAClaimedKeyRollsTheWholeCreateBack(t *testing.T) {
	t.Parallel()

	svc, deps := newWriteService(t)
	scope := testScope(t)
	first := testIdempotency(t, "the-same-key", idempotency.HashRequest([]byte(`{"name":"prod-eu"}`)))

	if _, err := svc.Create(context.Background(), scope, CreateCommand{
		Draft: testDraft(), Idempotency: first,
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}

	if _, err := svc.Create(context.Background(), scope, CreateCommand{
		Draft: testDraft(), Idempotency: first,
	}); err == nil {
		t.Fatal("the same key created a second source")
	}
	// ⭐ THE ROLLBACK IS THE ASSERTION. The retry did reach the mint — it must, or
	// the claim would have nothing to guard — and what saves the caller is that the
	// refusal happened INSIDE that transaction, so the second source row and the
	// second credential go back with it. The fakes below count attempts and cannot
	// un-count them, which is exactly why this is the fact worth asserting.
	if !deps.tx.rolledBack {
		t.Fatal("the refused retry did not roll its transaction back; the source row and the " +
			"credential it minted would survive as a live token nobody was ever told about")
	}
}

// TestTheClaimIsTakenInsideTheTransaction is the structural half of the same
// promise: a claim taken on the pool cannot roll a mint back however it fails.
func TestTheClaimIsTakenInsideTheTransaction(t *testing.T) {
	t.Parallel()

	svc, deps := newWriteService(t)
	_, err := svc.Create(context.Background(), testScope(t), CreateCommand{
		Draft:       testDraft(),
		Idempotency: testIdempotency(t, "k1", idempotency.HashRequest(nil)),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !deps.claims.claimedInTx {
		t.Fatal("the claim did not join the mint's unit of work")
	}
	// AFTER the mint, because it records what the call created: the SOURCE id,
	// which is the id a caller who never received the token needs in order to find
	// it and rotate its credential.
	if deps.claims.last.CreatedRef == uuid.Nil {
		t.Fatal("the claim recorded no created_ref; a caller told 409 could not find its source")
	}
}

// ⛔ TestAKeyedRequestIsRefusedWhenTheDeploymentCannotHonourIt.
//
// The defect this closes was a header the contract promised and the server
// ignored. Ignoring it a second time because a collaborator is nil would
// reproduce it exactly, and the caller would have no way to tell a protected
// create from an unprotected one. The refusal lands before the mint.
func TestAKeyedRequestIsRefusedWhenTheDeploymentCannotHonourIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		drop func(*Options)
	}{
		{"no claim store", func(o *Options) { o.Claims = nil }},
		{"no unit of work", func(o *Options) { o.Tx = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc, deps := newWriteService(t, tc.drop)
			_, err := svc.Create(context.Background(), testScope(t), CreateCommand{
				Draft:       testDraft(),
				Idempotency: testIdempotency(t, "k1", idempotency.HashRequest(nil)),
			})
			if errs.CodeOf(err) != CodeIdempotencyUnavailable {
				t.Fatalf("code = %q, want %q", errs.CodeOf(err), CodeIdempotencyUnavailable)
			}
			if deps.tokens.issued != 0 || deps.repo.created != 0 {
				t.Fatal("the refusal still minted or wrote")
			}
		})
	}
}

// ⛔ TestAnUnkeyedRequestNeedsNoClaimStore. The header is optional, and a
// deployment without the store must still serve every caller that did not send
// one.
func TestAnUnkeyedRequestNeedsNoClaimStore(t *testing.T) {
	t.Parallel()

	svc, deps := newWriteService(t, func(o *Options) { o.Claims = nil })
	if _, err := svc.Create(context.Background(), testScope(t), CreateCommand{Draft: testDraft()}); err != nil {
		t.Fatalf("an unkeyed create was refused: %v", err)
	}
	if deps.repo.created != 1 {
		t.Fatalf("sources created = %d, want 1", deps.repo.created)
	}
}

/* -------------------------------------------------------------------------- */
/* 3. What an edit MEANS                                                      */
/* -------------------------------------------------------------------------- */

// ⭐ TestUpdateRotatesAnExistingCredentialInPlace.
//
// The source never spends a moment pointing at nothing, which is why the existing
// row is re-sealed rather than replaced. The rule used to live in an HTTP handler,
// where the only way to state it was to re-derive it.
func TestUpdateRotatesAnExistingCredentialInPlace(t *testing.T) {
	t.Parallel()

	existing := uuid.New()
	svc, deps := newWriteService(t)
	deps.repo.src.AuthCredentialID = &existing

	if _, err := svc.Update(context.Background(), testScope(t), deps.repo.src.ID, UpdateCommand{
		Credential: &CredentialInput{Kind: "basic", Values: map[string]string{"password": "p"}},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if deps.sealer.rotated != 1 || deps.sealer.created != 0 {
		t.Fatalf("rotations = %d, new seals = %d; a supplied credential re-seals in place",
			deps.sealer.rotated, deps.sealer.created)
	}
	if !deps.sealer.rotatedInTx {
		t.Fatal("the rotation did not join the update's unit of work")
	}
	got := deps.repo.lastPatch.AuthCredentialID
	if got == nil || *got == nil || **got != existing {
		t.Fatalf("the patch pointed the source at %v, not at the credential it already had", got)
	}
}

// TestUpdateSealsANewCredentialWhenTheSourceHadNone.
func TestUpdateSealsANewCredentialWhenTheSourceHadNone(t *testing.T) {
	t.Parallel()

	svc, deps := newWriteService(t)
	if _, err := svc.Update(context.Background(), testScope(t), deps.repo.src.ID, UpdateCommand{
		Credential: &CredentialInput{Kind: "bearer", Values: map[string]string{"token": "t"}},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if deps.sealer.created != 1 || deps.sealer.rotated != 0 {
		t.Fatalf("seals = %d, rotations = %d", deps.sealer.created, deps.sealer.rotated)
	}
	got := deps.repo.lastPatch.AuthCredentialID
	if got == nil || *got == nil || **got != deps.sealer.lastID {
		t.Fatalf("the patch did not name the credential just sealed: %v", got)
	}
}

// ⛔ TestKindNoneDetachesAndAnAbsentCredentialLeavesItAlone.
//
// These are DIFFERENT REQUESTS and collapsing them would silently unauthenticate
// a source that was only being renamed. `none` detaches — the row stays, other
// things may reference it, the source stops pointing at it — while an absent
// credential must not appear in the patch at all.
func TestKindNoneDetachesAndAnAbsentCredentialLeavesItAlone(t *testing.T) {
	t.Parallel()

	existing := uuid.New()
	name := "renamed"

	svc, deps := newWriteService(t)
	deps.repo.src.AuthCredentialID = &existing
	if _, err := svc.Update(context.Background(), testScope(t), deps.repo.src.ID, UpdateCommand{
		Credential: &CredentialInput{Kind: "none"},
	}); err != nil {
		t.Fatalf("detach: %v", err)
	}
	got := deps.repo.lastPatch.AuthCredentialID
	if got == nil || *got != nil {
		t.Fatalf("kind `none` should clear the credential, patch carried %v", got)
	}
	if deps.sealer.created != 0 || deps.sealer.rotated != 0 {
		t.Fatal("detaching a credential still touched the secret store")
	}

	svc2, deps2 := newWriteService(t)
	deps2.repo.src.AuthCredentialID = &existing
	if _, err := svc2.Update(context.Background(), testScope(t), deps2.repo.src.ID, UpdateCommand{
		Patch: domain.SourcePatch{Name: &name},
	}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if deps2.repo.lastPatch.AuthCredentialID != nil {
		t.Fatal("a rename carried a credential change the caller never asked for")
	}
}

/* -------------------------------------------------------------------------- */
/* 4. Retiring a source, and rotating its credential                          */
/* -------------------------------------------------------------------------- */

// ⭐ TestSoftDeleteRevokesInTheSameBreath.
//
// A soft delete that leaves a live ingest token is a soft delete in name only: the
// row is gone from every screen and the credential still authenticates, with
// nobody able to find it in order to revoke it. The order is deliberate too — the
// delete decides the answer, so an unknown id is a clean not-found with no write
// against another source's tokens.
func TestSoftDeleteRevokesInTheSameBreath(t *testing.T) {
	t.Parallel()

	svc, deps := newWriteService(t)
	if err := svc.SoftDelete(context.Background(), testScope(t), deps.repo.src.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deps.repo.deleted != 1 || deps.tokens.revoked != 1 {
		t.Fatalf("delete = %d rows, %d revocations", deps.repo.deleted, deps.tokens.revoked)
	}
	if !deps.tx.committed || !deps.repo.deletedInTx || !deps.tokens.revokedInTx {
		t.Fatalf("committed = %t, delete joined = %t, revoke joined = %t",
			deps.tx.committed, deps.repo.deletedInTx, deps.tokens.revokedInTx)
	}
}

// ⭐ TestSoftDeleteRollsBackWhenRevocationFails is the negative half: a failed
// revocation must take the soft delete down with it.
func TestSoftDeleteRollsBackWhenRevocationFails(t *testing.T) {
	t.Parallel()

	svc, deps := newWriteService(t)
	deps.tokens.revokeErr = errors.New("the identity store is having a bad day")

	if err := svc.SoftDelete(context.Background(), testScope(t), deps.repo.src.ID); err == nil {
		t.Fatal("a failed revocation reported success")
	}
	if deps.tx.committed || !deps.tx.rolledBack {
		t.Fatalf("committed = %t, rolled back = %t", deps.tx.committed, deps.tx.rolledBack)
	}
}

// ⛔ TestRotateNeverLeavesZeroWorkingTokens.
//
// The issuer revoked first and minted second. A mint that failed therefore revoked
// the source's only credential and left nothing behind it — and because
// Alertmanager treats 401 as permanent, the alerts sent afterwards were destroyed
// rather than delayed. The failure mode must be "nothing changed".
func TestRotateNeverLeavesZeroWorkingTokens(t *testing.T) {
	t.Parallel()

	svc, deps := newWriteService(t)
	deps.tokens.issueErr = errors.New("the mint failed")

	if _, err := svc.RotateIngestToken(
		context.Background(), testScope(t), deps.repo.src.ID, Idempotency{},
	); err == nil {
		t.Fatal("a failed rotation reported success")
	}
	if deps.tokens.revoked != 0 {
		t.Fatalf("a failed rotation revoked %d token(s); the source is now unreachable", deps.tokens.revoked)
	}
	if deps.tx.committed {
		t.Fatal("a failed rotation committed")
	}
}

// ⭐⭐ TestTheRotationDigestNamesTheSourceAndNotTheEmptyBody.
//
// A rotation declares no body, so a body digest is a CONSTANT and the source id is
// not in the claim tuple. One key would then make "rotate source A" and "rotate
// source B" digest identically, and the second would be refused as a REPLAY of a
// rotation that touched a different source entirely — a 409 telling the caller
// their request already succeeded when it never ran. Folding the target in makes
// them the different requests they are, while a true retry against the same source
// still digests identically and is still refused.
func TestTheRotationDigestNamesTheSourceAndNotTheEmptyBody(t *testing.T) {
	t.Parallel()

	scope := testScope(t)
	key := testIdempotency(t, "one-key", idempotency.RequestHash{})

	svcA, depsA := newWriteService(t)
	if _, err := svcA.RotateIngestToken(context.Background(), scope, depsA.repo.src.ID, key); err != nil {
		t.Fatalf("rotate A: %v", err)
	}
	svcB, depsB := newWriteService(t)
	if _, err := svcB.RotateIngestToken(context.Background(), scope, depsB.repo.src.ID, key); err != nil {
		t.Fatalf("rotate B: %v", err)
	}

	hashA, hashB := depsA.claims.last.RequestHash, depsB.claims.last.RequestHash
	if hashA == hashB {
		t.Fatal("two different sources digested identically; one key would refuse the second " +
			"as a replay of a rotation that destroyed a different source's credential")
	}
	if hashA == idempotency.HashRequest(nil) {
		t.Fatal("the digest is of the empty body, so every rotation in the org collides")
	}

	// And the retry it MUST refuse: the same key against the same source.
	if _, err := svcA.RotateIngestToken(context.Background(), scope, depsA.repo.src.ID, key); err == nil {
		t.Fatal("a retried rotation minted a second secret and revoked the one the caller holds")
	}
	if !depsA.tx.rolledBack {
		t.Fatal("the refused retry did not roll back; the source now has a token nobody was told about")
	}
}

// TestRotateWithoutAnIssuerIs503.
func TestRotateWithoutAnIssuerIs503(t *testing.T) {
	t.Parallel()

	svc, deps := newWriteService(t, func(o *Options) { o.Tokens = nil })
	_, err := svc.RotateIngestToken(context.Background(), testScope(t), deps.repo.src.ID, Idempotency{})
	if errs.CodeOf(err) != CodeTokenIssuerUnavailable {
		t.Fatalf("code = %q, want %q", errs.CodeOf(err), CodeTokenIssuerUnavailable)
	}
}

// TestWritesAgainstAForeignOrDeletedSourceAreNotFound keeps the tenant boundary
// asserted at the layer that resolves the row.
func TestWritesAgainstAForeignOrDeletedSourceAreNotFound(t *testing.T) {
	t.Parallel()

	svc, _ := newWriteService(t)
	stranger := uuid.New()

	if _, err := svc.RotateIngestToken(
		context.Background(), testScope(t), stranger, Idempotency{},
	); errs.CodeOf(err) != "not_found" {
		t.Fatalf("rotate: code = %q, want not_found", errs.CodeOf(err))
	}
	if _, err := svc.Update(context.Background(), testScope(t), stranger, UpdateCommand{
		Credential: &CredentialInput{Kind: "bearer", Values: map[string]string{"token": "t"}},
	}); errs.CodeOf(err) != "not_found" {
		t.Fatalf("update: code = %q, want not_found", errs.CodeOf(err))
	}
}

/* -------------------------------------------------------------------------- */
/* Harness                                                                    */
/* -------------------------------------------------------------------------- */

func testScope(t *testing.T) db.TenantScope {
	t.Helper()
	s, err := db.NewTenantScope(uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	return s
}

func testDraft() domain.SourceDraft {
	return domain.SourceDraft{
		ClusterID: uuid.New(),
		Name:      "prod-eu",
		Kind:      domain.KindAlertmanager,
		BaseURL:   "https://am.example.com",
	}
}

func testIdempotency(t *testing.T, key string, hash idempotency.RequestHash) Idempotency {
	t.Helper()
	k, err := idempotency.NewKey(key)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return Idempotency{
		Keyed: true,
		Key:   k,
		Principal: authn.Principal{
			Kind:   authn.KindSession,
			UserID: uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"),
		},
		RequestHash: hash,
	}
}

type writeDeps struct {
	repo   *writeRepo
	sealer *fakeSealer
	tokens *fakeTokens
	tx     *fakeTx
	claims *fakeClaims
}

// newWriteService builds the service over fakes, with every override applied to
// Options before construction so a test can express "this deployment was wired
// without X" the way the container would.
func newWriteService(t *testing.T, overrides ...func(*Options)) (*Service, *writeDeps) {
	t.Helper()

	deps := &writeDeps{
		repo:   &writeRepo{src: domain.Source{ID: uuid.New(), Name: "prod-eu", Kind: domain.KindAlertmanager}},
		sealer: &fakeSealer{},
		tokens: &fakeTokens{},
		tx:     &fakeTx{},
		claims: newFakeClaims(),
	}
	o := Options{
		Repo:    deps.repo,
		Clients: unusableClients{},
		Sealer:  deps.sealer,
		Tokens:  deps.tokens,
		Tx:      deps.tx,
		Claims:  deps.claims,
	}
	for _, f := range overrides {
		f(&o)
	}
	svc, err := New(o)
	if err != nil {
		t.Fatalf("build service: %v", err)
	}
	return svc, deps
}

// fakeTx records whether the unit of work committed or rolled back, and MARKS THE
// CONTEXT it hands down — mirroring the production runner, where a transaction
// travels in the context (`platform/db.FromContext`). A write that received an
// unmarked context is a write that ran on the pool and committed on its own, and
// `committed` alone cannot see that: it stays true when one of two writes is moved
// back outside the closure.
type fakeTx struct {
	committed  bool
	rolledBack bool
}

type fakeTxKey struct{}

func joinedUnitOfWork(ctx context.Context) bool { return ctx.Value(fakeTxKey{}) != nil }

func (f *fakeTx) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	// Mirror `db.TxOptions`, which returns `fn(ctx)` unchanged when the context is
	// already in a transaction: a nested call must not look like a second BEGIN.
	if joinedUnitOfWork(ctx) {
		return fn(ctx)
	}
	if err := fn(context.WithValue(ctx, fakeTxKey{}, struct{}{})); err != nil {
		f.rolledBack = true
		return err
	}
	f.committed = true
	return nil
}

// writeRepo owns EXACTLY ONE source, so every other id is not-found and the
// tenant probe is a fact about the service rather than about the fake.
type writeRepo struct {
	src domain.Source

	created     int
	deleted     int
	createdInTx bool
	deletedInTx bool
	lastDraft   domain.SourceDraft
	lastPatch   domain.SourcePatch
}

func (r *writeRepo) Get(_ context.Context, _ db.TenantScope, id uuid.UUID) (domain.Source, error) {
	if id != r.src.ID {
		return domain.Source{}, errs.NotFound("not_found", "no such source")
	}
	return r.src, nil
}

func (r *writeRepo) Create(
	ctx context.Context, s db.TenantScope, in domain.SourceDraft,
) (domain.Source, error) {
	r.created++
	r.createdInTx = joinedUnitOfWork(ctx)
	r.lastDraft = in
	return domain.Source{
		ID: uuid.New(), OrgID: s.OrgID(), ClusterID: in.ClusterID,
		Name: in.Name, Kind: in.Kind, BaseURL: in.BaseURL,
		AuthCredentialID: in.AuthCredentialID,
	}, nil
}

func (r *writeRepo) Update(
	_ context.Context, _ db.TenantScope, id uuid.UUID, p domain.SourcePatch,
) (domain.Source, error) {
	if id != r.src.ID {
		return domain.Source{}, errs.NotFound("not_found", "no such source")
	}
	r.lastPatch = p
	return r.src, nil
}

func (r *writeRepo) SoftDelete(ctx context.Context, _ db.TenantScope, id uuid.UUID) error {
	if id != r.src.ID {
		return errs.NotFound("not_found", "no such source")
	}
	r.deletedInTx = joinedUnitOfWork(ctx)
	r.deleted++
	return nil
}

func (r *writeRepo) List(
	context.Context, db.TenantScope, domain.SourceFilter, db.Keyset,
) ([]domain.Source, db.Cursor, error) {
	return nil, db.Cursor{}, nil
}

func (r *writeRepo) ListByIDs(context.Context, db.TenantScope, []uuid.UUID) ([]domain.Source, error) {
	return nil, nil
}

func (r *writeRepo) ListDue(context.Context, db.TenantScope, int) ([]domain.Source, error) {
	return nil, nil
}

func (r *writeRepo) GetHealth(
	_ context.Context, _ db.TenantScope, id uuid.UUID,
) (domain.SourceHealth, error) {
	return domain.SourceHealth{SourceID: id, Status: domain.HealthUnknown}, nil
}

func (r *writeRepo) HealthFor(
	context.Context, db.TenantScope, []uuid.UUID,
) (map[uuid.UUID]domain.SourceHealth, error) {
	return map[uuid.UUID]domain.SourceHealth{}, nil
}

func (r *writeRepo) SaveHealth(context.Context, db.TenantScope, domain.SourceHealth) error {
	return nil
}

func (r *writeRepo) ResolveOrg(context.Context, uuid.UUID) (uuid.UUID, error) {
	return r.src.OrgID, nil
}

type fakeSealer struct {
	created     int
	rotated     int
	createdInTx bool
	rotatedInTx bool
	lastID      uuid.UUID
}

func (f *fakeSealer) CreateCredential(
	ctx context.Context, _ db.TenantScope, _ string, _ map[string]string,
) (uuid.UUID, error) {
	f.created++
	f.createdInTx = joinedUnitOfWork(ctx)
	f.lastID = uuid.New()
	return f.lastID, nil
}

func (f *fakeSealer) RotateCredential(
	ctx context.Context, _ db.TenantScope, id uuid.UUID, _ string, _ map[string]string,
) error {
	f.rotated++
	f.rotatedInTx = joinedUnitOfWork(ctx)
	f.lastID = id
	return nil
}

// fakeTokens mints a REAL-SHAPED ingest secret, so a caller asserting on the
// fifteen-character prefix is asserting the production derivation.
type fakeTokens struct {
	issued      int
	revoked     int
	issuedInTx  bool
	revokedInTx bool
	issueErr    error
	revokeErr   error
}

func (f *fakeTokens) IssueIngestToken(
	ctx context.Context, _ db.TenantScope, _ uuid.UUID,
) (string, string, error) {
	if f.issueErr != nil {
		return "", "", f.issueErr
	}
	f.issued++
	f.issuedInTx = joinedUnitOfWork(ctx)
	secret := "oto_ingest_Ab3Dkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"
	return secret, secret[:15], nil
}

func (f *fakeTokens) RevokeIngestTokens(ctx context.Context, _ db.TenantScope, _ uuid.UUID) error {
	f.revokedInTx = joinedUnitOfWork(ctx)
	if f.revokeErr != nil {
		return f.revokeErr
	}
	f.revoked++
	return nil
}

// fakeClaims is the claim table's behaviour rather than its SQL: a key is held by
// exactly one (org, principal, operation, key) tuple, and a second attempt is a
// replay when the digest matches and a conflict when it does not.
type fakeClaims struct {
	held        map[string]idempotency.Claim
	claimedInTx bool
	last        idempotency.Claim
}

func newFakeClaims() *fakeClaims { return &fakeClaims{held: map[string]idempotency.Claim{}} }

func (f *fakeClaims) Claim(
	ctx context.Context, _ db.TenantScope, c idempotency.Claim,
) (idempotency.Result, error) {
	f.claimedInTx = joinedUnitOfWork(ctx)
	f.last = c
	k := c.OrgID.String() + "|" + c.PrincipalID.String() + "|" + c.Operation.String() + "|" + c.Key.String()
	incumbent, held := f.held[k]
	if !held {
		f.held[k] = c
		return idempotency.Result{Outcome: idempotency.Claimed, Existing: c}, nil
	}
	if incumbent.RequestHash == c.RequestHash {
		return idempotency.Result{Outcome: idempotency.Replayed, Existing: incumbent}, nil
	}
	return idempotency.Result{Outcome: idempotency.Conflicted, Existing: incumbent}, nil
}

// unusableClients satisfies ClientFactory for a service that never dials: the
// write path builds no outbound client, and a factory that could would be a
// factory a test might accidentally exercise.
type unusableClients struct{}

func (unusableClients) Alertmanager(domain.Source, domain.Credential) (AlertmanagerClient, error) {
	return nil, errors.New("no upstream in a write test")
}

func (unusableClients) Prometheus(
	domain.Source, domain.Credential, string,
) (PrometheusClient, error) {
	return nil, errors.New("no upstream in a write test")
}
