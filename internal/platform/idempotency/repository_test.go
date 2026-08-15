package idempotency_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/platform/idempotency"
	"github.com/thulasiram/oto/test/harness"
)

func TestMain(m *testing.M) { harness.Main(m) }

// These tests run against the real Postgres because every property under test is
// a property of the SCHEMA — which tuple is unique, and what a second INSERT into
// it does. A fake store would assert the shape of its own map.

var (
	opCreate = idempotency.MustOperation("createApiToken")
	opRevoke = idempotency.MustOperation("revokeApiToken")
)

// fixture is one seeded tenant with a principal in it.
type fixture struct {
	h    *harness.H
	repo *idempotency.Repository
	org  harness.Org
	user harness.User
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	h := harness.New(t)
	org := h.Org()
	return fixture{h: h, repo: idempotency.NewRepository(h.Pool), org: org, user: h.User(org)}
}

// claimOf builds a claim for this fixture's tenant and principal.
func (f fixture) claimOf(t *testing.T, op idempotency.Operation, key string, body []byte, ref uuid.UUID) idempotency.Claim {
	t.Helper()
	k, err := idempotency.NewKey(key)
	require.NoError(t, err)
	return idempotency.Claim{
		OrgID:       f.org.ID,
		PrincipalID: f.user.ID,
		Operation:   op,
		Key:         k,
		RequestHash: idempotency.HashRequest(body),
		CreatedRef:  ref,
		ClaimedAt:   f.h.Now(),
	}
}

// claim runs one Claim inside a transaction, which is the only way it may be
// called.
func (f fixture) claim(t *testing.T, c idempotency.Claim) idempotency.Result {
	t.Helper()
	res, err := f.tryClaim(t, c)
	require.NoError(t, err)
	return res
}

func (f fixture) tryClaim(t *testing.T, c idempotency.Claim) (idempotency.Result, error) {
	t.Helper()
	scope := harness.Scope(t, c.OrgID)
	var res idempotency.Result
	err := db.Tx(f.h.Ctx, f.h.Pool, func(ctx context.Context) error {
		var cerr error
		res, cerr = f.repo.Claim(ctx, scope, c)
		return cerr
	})
	return res, err
}

// TestAFreshKeyIsClaimed is the ordinary first call: nobody holds the key, so
// this caller does, and it is the one allowed to mint.
func TestAFreshKeyIsClaimed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	token := id.New()
	res := f.claim(t, f.claimOf(t, opCreate, "key-fresh", []byte(`{"name":"ci"}`), token))

	require.Equal(t, idempotency.Claimed, res.Outcome)
	require.True(t, res.Fresh())
	require.Equal(t, token, res.Existing.CreatedRef)
}

// TestTheSameKeyAndBodyIsAReplayThatNamesWhatTheFirstCallCreated is the defect
// the whole ticket is about, from the caller's side.
//
// ⭐ THE `CreatedRef` IS THE POINT. oto cannot show the secret twice — it never
// stored one — so the honest answer to a retry is "your first attempt worked, and
// this is the id of what it made". A caller that never received the secret can
// revoke exactly that id. Returning nothing would leave them unable to tell a
// success from a failure, which is the state the bug left them in.
func TestTheSameKeyAndBodyIsAReplayThatNamesWhatTheFirstCallCreated(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	body := []byte(`{"name":"ci"}`)
	first := id.New()
	require.Equal(t, idempotency.Claimed, f.claim(t, f.claimOf(t, opCreate, "key-replay", body, first)).Outcome)

	// The retry arrives having minted nothing of its own yet: the reference it
	// offers is a SECOND token id, which must be discarded in favour of the
	// incumbent's.
	res := f.claim(t, f.claimOf(t, opCreate, "key-replay", body, id.New()))

	require.Equal(t, idempotency.Replayed, res.Outcome)
	require.False(t, res.Fresh())
	require.Equal(t, first, res.Existing.CreatedRef,
		"a replay must name what the FIRST call created; naming the retry's own id would send the "+
			"caller to revoke a credential that does not exist and leave the real one live")
}

// TestTheSameKeyWithADifferentBodyIsAConflict is the contract's own wording:
// replaying a key with a different body is a 409. The two calls cannot both be
// the request the key names.
func TestTheSameKeyWithADifferentBodyIsAConflict(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	first := id.New()
	require.Equal(t, idempotency.Claimed,
		f.claim(t, f.claimOf(t, opCreate, "key-conflict", []byte(`{"name":"ci"}`), first)).Outcome)

	res := f.claim(t, f.claimOf(t, opCreate, "key-conflict", []byte(`{"name":"prod"}`), id.New()))

	require.Equal(t, idempotency.Conflicted, res.Outcome)
	require.False(t, res.Fresh())
	require.Equal(t, first, res.Existing.CreatedRef)
}

// TestABodylessOperationReplaysAgainstItself covers `revokeApiToken`, which sends
// no body at all. HashRequest(nil) is the digest of the empty string rather than
// a zero value, so two bodyless retries compare EQUAL and the second is a replay
// — an operation with no body must still be claimable.
func TestABodylessOperationReplaysAgainstItself(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	require.Equal(t, idempotency.Claimed,
		f.claim(t, f.claimOf(t, opRevoke, "key-bodyless", nil, uuid.Nil)).Outcome)

	res := f.claim(t, f.claimOf(t, opRevoke, "key-bodyless", nil, uuid.Nil))

	require.Equal(t, idempotency.Replayed, res.Outcome)
	require.Equal(t, uuid.Nil, res.Existing.CreatedRef,
		"a revoke creates nothing, so its claim references nothing")
}

// TestABodylessOperationDistinguishesItsTarget is why a bodyless operation
// digests the resource it acts on rather than its (absent) body.
//
// ⛔ WITHOUT IT ONE KEY REFUSES A GENUINELY DIFFERENT REQUEST. `HashRequest(nil)`
// is a CONSTANT — every bodyless request has the same digest — and the path `{id}`
// is not in the claim tuple, so revoking token A and then token B under one key
// would be answered "that request already succeeded", naming nothing, about a
// request the caller never made. The two calls destroy different credentials and
// are not the same request.
func TestABodylessOperationDistinguishesItsTarget(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tokenA, tokenB := id.New(), id.New()

	first := f.claimOf(t, opRevoke, "one-gesture", nil, uuid.Nil)
	first.RequestHash = idempotency.HashTargetedRequest(tokenA, nil)
	require.Equal(t, idempotency.Claimed, f.claim(t, first).Outcome)

	// A DIFFERENT target under the same key is a different request, and is told so.
	other := first
	other.RequestHash = idempotency.HashTargetedRequest(tokenB, nil)
	require.Equal(t, idempotency.Conflicted, f.claim(t, other).Outcome,
		"two different targets under one key are two different requests; answering the second as a "+
			"replay would tell the caller that revoking B had already succeeded when only A was ever "+
			"revoked")

	// And the SAME target still replays, which is what the key is for.
	require.Equal(t, idempotency.Replayed, f.claim(t, first).Outcome,
		"a true retry — same key, same target — must still be recognised, or folding the target in "+
			"would have bought correctness by making the header useless")
}

// TestOneTenantsKeyDoesNotBlockAnothers is the tenancy property. Idempotency keys
// are client-chosen strings and clients pick from small alphabets; if the tuple
// did not lead with org_id, one tenant's key would refuse another tenant's create
// and, worse, hand back a reference to a row in somebody else's org.
func TestOneTenantsKeyDoesNotBlockAnothers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	other := f.h.Org()
	otherUser := f.h.User(other)

	mine := f.claimOf(t, opCreate, "shared-key", []byte(`{"name":"ci"}`), id.New())
	require.Equal(t, idempotency.Claimed, f.claim(t, mine).Outcome)

	theirs := mine
	theirs.OrgID = other.ID
	theirs.PrincipalID = otherUser.ID
	theirs.CreatedRef = id.New()

	res := f.claim(t, theirs)
	require.Equal(t, idempotency.Claimed, res.Outcome,
		"a key claimed in one tenant must be free in another")
	require.Equal(t, theirs.CreatedRef, res.Existing.CreatedRef)
}

// TestOnePrincipalsKeyDoesNotBlockAnothers is why `principal_id` is in the tuple.
//
// ⛔ A KEY IS A CLIENT'S PRIVATE HANDLE ON ITS OWN RETRY. Shared per-org, one
// member's key would silently refuse another member's create — a denial of
// service any org member could aim at any other — and the refusal would double as
// an oracle telling them a key they guessed was in use.
func TestOnePrincipalsKeyDoesNotBlockAnothers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	mine := f.claimOf(t, opCreate, "same-key", []byte(`{"name":"ci"}`), id.New())
	require.Equal(t, idempotency.Claimed, f.claim(t, mine).Outcome)

	colleague := f.h.User(f.org)
	theirs := mine
	theirs.PrincipalID = colleague.ID
	theirs.CreatedRef = id.New()

	res := f.claim(t, theirs)
	require.Equal(t, idempotency.Claimed, res.Outcome,
		"one org member's key must not be able to block another's")
	require.Equal(t, theirs.CreatedRef, res.Existing.CreatedRef)
}

// TestTheSameKeyOnTwoOperationsIsTwoClaims is why `operation` is in the tuple.
//
// oto's own frontend mints ONE key per user gesture. Without the operation, a
// gesture that creates and then revokes would find its revoke refused by its
// create, and be told about a resource from the wrong endpoint.
func TestTheSameKeyOnTwoOperationsIsTwoClaims(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	body := []byte(`{"name":"ci"}`)
	created := f.claimOf(t, opCreate, "one-gesture", body, id.New())
	require.Equal(t, idempotency.Claimed, f.claim(t, created).Outcome)

	revoked := f.claimOf(t, opRevoke, "one-gesture", body, uuid.Nil)
	require.Equal(t, idempotency.Claimed, f.claim(t, revoked).Outcome,
		"two different operations are never the same request, however equal their keys and bodies")

	// And each operation still replays against ITSELF.
	require.Equal(t, idempotency.Replayed, f.claim(t, created).Outcome)
	require.Equal(t, idempotency.Replayed, f.claim(t, revoked).Outcome)
}

// TestARolledBackCallerLeavesNoKeyHeld is the transaction property, and it is the
// reason Claim participates in the caller's unit of work rather than opening its
// own.
//
// ⭐⭐ PASS 2 MINTS AND CLAIMS TOGETHER. If the claim committed independently, a
// mint that failed afterwards would leave a key held by a credential that does
// not exist, and the caller's retry would be told to go and revoke an id that was
// never written. Rolling back together is what makes "one credential per key" a
// property of the database rather than of handler ordering.
func TestARolledBackCallerLeavesNoKeyHeld(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	c := f.claimOf(t, opCreate, "key-rollback", []byte(`{"name":"ci"}`), id.New())
	scope := harness.Scope(t, c.OrgID)
	boom := errors.New("the mint failed after the claim")

	err := db.Tx(f.h.Ctx, f.h.Pool, func(ctx context.Context) error {
		res, cerr := f.repo.Claim(ctx, scope, c)
		require.NoError(t, cerr)
		require.Equal(t, idempotency.Claimed, res.Outcome)
		return boom
	})
	require.ErrorIs(t, err, boom)

	var held int
	require.NoError(t, f.h.Pool.QueryRow(f.h.Ctx,
		`SELECT count(*) FROM idempotency_claims WHERE org_id = $1`, f.org.ID).Scan(&held))
	require.Zero(t, held, "the claim must die with the transaction that made it")

	// And the key is genuinely free again, not merely absent.
	require.Equal(t, idempotency.Claimed, f.claim(t, c).Outcome)
}

// TestClaimingOutsideATransactionIsRefused pins the refusal itself. A caller that
// forgot the transaction would get a claim that commits on its own, which is the
// half-done state above with nothing to notice it.
func TestClaimingOutsideATransactionIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	c := f.claimOf(t, opCreate, "key-no-tx", []byte(`{}`), id.New())
	_, err := f.repo.Claim(f.h.Ctx, harness.Scope(t, c.OrgID), c)
	require.Error(t, err)

	var written int
	require.NoError(t, f.h.Pool.QueryRow(f.h.Ctx,
		`SELECT count(*) FROM idempotency_claims WHERE org_id = $1`, f.org.ID).Scan(&written))
	require.Zero(t, written)
}

// TestAClaimForAnotherOrgThanTheScopeIsRefused is CONTEXT.md §5.6: the scope is
// the authority, and a row claiming a different org is a service bug that must
// never reach the driver.
func TestAClaimForAnotherOrgThanTheScopeIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	c := f.claimOf(t, opCreate, "key-wrong-org", []byte(`{}`), id.New())
	elsewhere := harness.Scope(t, id.New())

	err := db.Tx(f.h.Ctx, f.h.Pool, func(ctx context.Context) error {
		_, cerr := f.repo.Claim(ctx, elsewhere, c)
		return cerr
	})
	require.Error(t, err)
}

// errLostTheRace is what a losing claimant returns so its own mint rolls back
// with the claim it did not get — exactly what `idempotency.Reuse` does to a
// handler's transaction.
var errLostTheRace = errors.New("the key was already claimed")

// mintToken writes an `api_tokens` row inside the caller's transaction: the act a
// claim exists to guard, doing what a real mint does — one row, committed with the
// claim or rolled back with it.
func (f fixture) mintToken(ctx context.Context, tokenID uuid.UUID) error {
	// `api_tokens_hash_ck` wants exactly 32 bytes, and the SECRET ITSELF IS NEVER
	// WRITTEN anywhere — here as in production, the row holds a digest and nothing
	// else, which is why a replay can never be answered with the secret.
	digest := sha256.Sum256([]byte(tokenID.String()))
	_, err := db.FromContext(ctx, f.h.Pool).Exec(ctx,
		`INSERT INTO api_tokens (id, org_id, user_id, kind, name, token_hash, prefix, created_at)
		 VALUES ($1, $2, $3, 'pat', $4, $5, 'oto_pat_AbCd', $6)`,
		tokenID, f.org.ID, f.user.ID, "race-"+tokenID.String()[:8], digest[:], f.h.Now())
	return err
}

func (f fixture) tokenExists(t *testing.T, tokenID uuid.UUID) bool {
	t.Helper()
	var n int
	require.NoError(t, f.h.Pool.QueryRow(f.h.Ctx,
		`SELECT count(*) FROM api_tokens WHERE id = $1`, tokenID).Scan(&n))
	return n == 1
}

// TestTwoSimultaneousTransactionsOnOneKeyLeaveExactlyOneClaimant is THE property
// the whole design rests on, and it is the one no other case here reaches: every
// test above claims a key after the previous claim has committed, which exercises
// the ordinary retry and never the race.
//
// ⭐⭐ THE RACE IS THE REASON `Claim` IS TWO STATEMENTS AND NOT ONE CTE. Under one
// snapshot the loser's INSERT waits on the winner's uncommitted row and its outer
// SELECT — pinned to a snapshot taken BEFORE the winner committed — then sees
// NOTHING, which `ingest_dedup` reads as "we won". Here "we won" means MINT A
// SECOND CREDENTIAL. The separate read takes a fresh snapshot under READ
// COMMITTED and therefore sees exactly the row it lost to. That argument is
// written in `Claim`'s doc comment; this is where it is checked against a real
// Postgres rather than believed.
//
// ⭐ IT IS COORDINATED WITH CHANNELS AND NOT WITH SLEEPS. Both claimants report
// that their transaction is OPEN and their mint is written, and only then are
// both released to claim — so the overlap is a fact of the schedule rather than a
// hope about timing, and the test neither flakes on a slow machine nor passes
// vacuously on a fast one by letting the first claimant commit before the second
// begins.
func TestTwoSimultaneousTransactionsOnOneKeyLeaveExactlyOneClaimant(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	scope := harness.Scope(t, f.org.ID)
	// Built once, on the test's own goroutine: the two claimants differ ONLY in the
	// credential each of them minted, which is precisely the shape of two retries
	// of one request arriving at once.
	shared := f.claimOf(t, opCreate, "key-race", []byte(`{"name":"ci"}`), uuid.Nil)

	type attempt struct {
		token uuid.UUID
		res   idempotency.Result
		err   error
	}

	const claimants = 2
	var (
		open    = make(chan struct{}, claimants) // "my transaction is open and my mint is written"
		release = make(chan struct{})            // closed once BOTH are, so neither wins by arriving early
		done    = make(chan attempt, claimants)
	)

	for range claimants {
		go func() {
			a := attempt{token: id.New()}
			a.err = db.Tx(f.h.Ctx, f.h.Pool, func(ctx context.Context) error {
				if merr := f.mintToken(ctx, a.token); merr != nil {
					return merr
				}
				open <- struct{}{}
				<-release

				mine := shared
				mine.CreatedRef = a.token

				var cerr error
				a.res, cerr = f.repo.Claim(ctx, scope, mine)
				if cerr != nil {
					return cerr
				}
				if !a.res.Fresh() {
					// What a handler does with a key it did not get: refuse, and take
					// the mint down with the transaction.
					return errLostTheRace
				}
				return nil
			})
			done <- a
		}()
	}

	// ⛔ EVERY WAIT IS BOUNDED. The loser blocks on the winner's uncommitted row,
	// which is the intended behaviour and is indistinguishable from a deadlock at
	// the exit code — so "no deadlock" is asserted as a deadline rather than
	// assumed. Postgres would break a genuine cycle itself with a 40P01, which
	// `mapErr` turns into an error, and that error would fail the assertions below.
	const budget = 30 * time.Second
	for range claimants {
		select {
		case <-open:
		case <-time.After(budget):
			t.Fatal("a claimant never reached its claim; both transactions must be open before either " +
				"claims, or this test is not exercising the race at all")
		}
	}
	close(release)

	attempts := make([]attempt, 0, claimants)
	for range claimants {
		select {
		case a := <-done:
			attempts = append(attempts, a)
		case <-time.After(budget):
			t.Fatal("two simultaneous claims on one key did not both finish: the loser waits on the " +
				"winner's uncommitted row, and a wait that never ends is a deadlock — which for a " +
				"credential endpoint means every retry hangs until the statement timeout")
		}
	}

	var winner, loser attempt
	var claimed int
	for _, a := range attempts {
		if a.res.Outcome == idempotency.Claimed {
			claimed++
			winner = a
			continue
		}
		loser = a
	}
	require.Equal(t, 1, claimed,
		"exactly one of two simultaneous transactions may claim one key: two winners is two live "+
			"credentials for one key, which is the entire defect, and zero is a key nobody may spend")

	require.NoError(t, winner.err, "the claimant that won must commit")
	require.ErrorIs(t, loser.err, errLostTheRace)
	require.Equal(t, idempotency.Replayed, loser.res.Outcome,
		"the loser must OBSERVE the winner's row rather than find nothing: a claim that reads its "+
			"own pre-race snapshot sees no incumbent and concludes it won")
	require.Equal(t, winner.token, loser.res.Existing.CreatedRef,
		"the loser must be told what the WINNER created; naming its own rolled-back id would send "+
			"the caller to revoke a credential that does not exist and leave the live one alone")

	// The database agrees: one claim, pointing at the credential that survived.
	var ref uuid.UUID
	require.NoError(t, f.h.Pool.QueryRow(f.h.Ctx,
		`SELECT created_ref FROM idempotency_claims
		  WHERE org_id = $1 AND principal_id = $2 AND operation = $3 AND idempotency_key = 'key-race'`,
		f.org.ID, f.user.ID, opCreate.String()).Scan(&ref))
	require.Equal(t, winner.token, ref)

	// ⭐⭐ AND THE LOSER'S MINT IS GONE. This is what the shared transaction buys:
	// the losing request DID insert a token before it discovered the key was taken,
	// and if that insert had committed the org would own a live credential whose
	// secret went to a response that answered `409`.
	require.True(t, f.tokenExists(t, winner.token), "the winning claimant's credential must survive")
	require.False(t, f.tokenExists(t, loser.token),
		"the losing claimant's credential must roll back with its claim, or the race mints exactly "+
			"the orphaned live token this table exists to prevent")
}

// TestPruneDeletesOnlyClaimsPastTheWindow is the sweep `retention.prune` runs.
//
// ⛔ THE HORIZON IS THE CONTRACT'S PROMISE, so the assertion that matters is the
// SURVIVOR: deleting a claim that is still inside `RetentionWindow` re-opens the
// hole the table closes, and it does so silently — the next retry simply mints a
// second credential.
func TestPruneDeletesOnlyClaimsPastTheWindow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	stale := f.claimOf(t, opCreate, "key-stale", []byte(`{}`), id.New())
	stale.ClaimedAt = f.h.Now().Add(-idempotency.RetentionWindow).Add(-time.Minute)
	require.Equal(t, idempotency.Claimed, f.claim(t, stale).Outcome)

	fresh := f.claimOf(t, opCreate, "key-live", []byte(`{}`), id.New())
	fresh.ClaimedAt = f.h.Now().Add(-idempotency.RetentionWindow).Add(time.Minute)
	require.Equal(t, idempotency.Claimed, f.claim(t, fresh).Outcome)

	deleted, err := f.repo.Prune(f.h.Ctx, f.h.Now().Add(-idempotency.RetentionWindow))
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)

	// The stale key is free again — which is what expiry MEANS — and the live one
	// still holds.
	require.Equal(t, idempotency.Claimed, f.claim(t, f.claimOf(t, opCreate, "key-stale", []byte(`{}`), id.New())).Outcome)
	require.Equal(t, idempotency.Replayed, f.claim(t, fresh).Outcome)
}
