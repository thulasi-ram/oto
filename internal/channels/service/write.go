package service

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/platform/idempotency"
)

// ⭐⭐ THIS FILE IS WHY `testChannel` NO LONGER SENDS A SECOND REAL MESSAGE INTO
// A CUSTOMER'S SLACK WORKSPACE.
//
// `POST /channels/{id}/test` called the tester unconditionally on every request,
// with no dedup of any kind, while the contract's `Idempotency-Key` promised
// callers that a replay "returns the original result rather than acting twice"
// (ticket a6cc834). It was the sharpest case in that ticket: every retry —
// deliberate, or a client library's own — is a message a real person sees in a
// real room. The UI did not even send a key, so the endpoint was unfixed at both
// ends.
//
// ⛔ IT IS A SERVICE AND NOT A HANDLER, because the claim has to join a
// transaction and the transaction boundary is not an HTTP concern — the same
// ruling that moved `createSource` and `createCluster` off their routers.
// `channels/api`'s note that its CRUD is "genuinely CRUD" held right up until the
// moment one of those writes needed to commit beside something else.

// The operationIds a key is claimed under here, spelled once from the contract so
// a claim and the contract cannot drift.
var (
	opCreateChannel = idempotency.MustOperation("createChannel")
	opTestChannel   = idempotency.MustOperation("testChannel")
)

// The codes this write path mints. Each is a DEPLOYMENT fact — a collaborator
// this process was built without.
const (
	// CodeIdempotencyUnavailable means the caller sent an `Idempotency-Key` this
	// deployment cannot honour.
	CodeIdempotencyUnavailable = "idempotency_unavailable"
	// CodeChannelStoreUnavailable means there is nowhere to write a channel.
	CodeChannelStoreUnavailable = "channels_store_unavailable"
	// CodeTesterUnavailable means channel testing is not configured here.
	CodeTesterUnavailable = "channels_tester_unavailable"
)

// Claims is the `Idempotency-Key` claim store, satisfied by
// `*platform/idempotency.Repository`.
type Claims interface {
	Claim(ctx context.Context, s db.TenantScope, c idempotency.Claim) (idempotency.Result, error)
}

// UnitOfWork runs a create and the claim that guards it as ONE commit.
type UnitOfWork interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// ChannelWriter inserts a destination, satisfied by
// `*channels/repository.ChannelRepository`.
//
// ⛔ It takes the id inside NewInstance. The claim must name what the create made,
// and it must do so BEFORE the insert: a retry that inserted first would meet
// `channels_name_uniq` and be answered with a name conflict rather than with the
// channel the caller already has.
type ChannelWriter interface {
	Create(ctx context.Context, s db.TenantScope, in domain.NewInstance) (domain.Instance, error)
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Instance, error)
}

// ChannelTester sends one synthetic card, satisfied by `*Tester` in this package.
type ChannelTester interface {
	Test(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.TestResult, error)
}

// Compile-time proof that the tester satisfies the port this file declares.
var _ ChannelTester = (*Tester)(nil)

// Idempotency is the caller's `Idempotency-Key` intent for one write.
type Idempotency struct {
	// Keyed reports that the caller sent a key at all. False means every field
	// below is ignored and the write behaves exactly as it did before, which is
	// what keeps the header optional.
	Keyed bool
	Key   idempotency.Key
	// Principal is who sent it. A key is a client's private handle on its own
	// retry, so one org member's key must never refuse another's request.
	Principal authn.Principal
	// RequestHash is what "the same request" means: the sha256 of the body for a
	// create, and of the CHANNEL for a test — see WriterOptions.
	RequestHash idempotency.RequestHash
}

// WriterOptions are the Writer's dependencies.
type WriterOptions struct {
	Store  ChannelWriter
	Tester ChannelTester
	Tx     UnitOfWork
	Claims Claims
	Clock  clock.Clock
}

// Writer owns the two channel operations that have a side effect worth claiming:
// registering a destination, and sending a real message to one.
type Writer struct {
	store  ChannelWriter
	tester ChannelTester
	tx     UnitOfWork
	claims Claims
	clk    clock.Clock
}

// NewWriter builds the writer.
func NewWriter(o WriterOptions) (*Writer, error) {
	if o.Store == nil {
		return nil, errs.New(errs.KindInternal, "channels_writer_deps",
			"a channel store is required")
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.New()
	}
	return &Writer{store: o.Store, tester: o.Tester, tx: o.Tx, claims: o.Claims, clk: clk}, nil
}

// CreateChannel registers a destination.
//
// ⭐⭐ A RETRY CARRYING THE SAME KEY IS ANSWERED WITH THE ORIGINAL CHANNEL. This
// endpoint declared the header and read it nowhere; it was safe only by accident,
// because `channels_name_uniq (org_id, name)` refused a second create under the
// same name with a `409` that named nothing — so a client that never received its
// response could not learn the id of what it had already made, and an operator
// retrying under a different name got a genuine duplicate.
//
// ⛔ THE CONFIG IS NOT VALIDATED HERE. Layer 4 is the caller's (§L.5): the
// provider registry lives with the HTTP layer that holds it, and re-validating
// would mean this package deciding what a valid Slack config is on top of the
// registry that already knows.
func (w *Writer) CreateChannel(
	ctx context.Context, scope db.TenantScope, in domain.NewInstance, idem Idempotency,
) (domain.Instance, error) {
	if err := w.requireStore(); err != nil {
		return domain.Instance{}, err
	}
	if err := w.requireClaims(idem); err != nil {
		return domain.Instance{}, err
	}

	var (
		out      domain.Instance
		replayOf uuid.UUID
	)
	// Named before it exists, so the claim below can record it.
	if in.ID == uuid.Nil {
		in.ID = id.New()
	}
	err := w.inTx(ctx, func(ctx context.Context) error {
		if idem.Keyed {
			replayed, ref, err := w.claim(ctx, scope, idem, opCreateChannel, in.ID)
			if err != nil {
				return err
			}
			if replayed {
				replayOf = ref
				return errReplay
			}
		}
		created, err := w.store.Create(ctx, scope, in)
		if err != nil {
			return err
		}
		out = created
		return nil
	})
	if errors.Is(err, errReplay) {
		// Read OUTSIDE the rolled-back transaction, so what comes back is the row
		// the FIRST attempt committed.
		return w.store.Get(ctx, scope, replayOf)
	}
	if err != nil {
		return domain.Instance{}, err
	}
	return out, nil
}

// TestChannel sends one synthetic card, at most once per key.
//
// ⭐⭐ THE CLAIM IS TAKEN BEFORE THE SEND AND IN ITS OWN TRANSACTION, AND THAT IS
// DELIBERATE. Every other claim in oto commits with the act it guards, so a lost
// race rolls the act back. There is nothing to roll back here: the act is a
// message in somebody else's Slack workspace, and no transaction can un-send it.
// Holding a database transaction open across a fifteen-second outbound call to
// somebody else's API would also be its own outage. So the ordering is inverted
// on purpose — CLAIM, THEN SEND — and the failure mode is chosen to match: a
// process that dies between the two leaves a claimed key and no message, which
// costs the operator one press with a fresh key. The other ordering costs a
// customer's on-call channel a second real alert card.
//
// ⭐ A REPLAY ANSWERS FROM THE DESTINATION'S RECORDED HEALTH. The tester writes
// what every attempt learned onto the channel row, so the first attempt's verdict
// is still there to be read — which is the "original result" the contract
// promises, rather than a stored response body the claim table deliberately
// cannot hold.
func (w *Writer) TestChannel(
	ctx context.Context, scope db.TenantScope, channelID uuid.UUID, idem Idempotency,
) (domain.TestResult, error) {
	if w.tester == nil {
		return domain.TestResult{}, errs.Unavailable(CodeTesterUnavailable,
			"channel testing is not configured in this deployment", 0)
	}
	if err := w.requireClaims(idem); err != nil {
		return domain.TestResult{}, err
	}

	if idem.Keyed {
		replayed := false
		err := w.inTx(ctx, func(ctx context.Context) error {
			// CreatedRef is Nil: a test creates no row. The replay is served from
			// the destination's health rather than from anything the claim names.
			was, _, err := w.claim(ctx, scope, idem, opTestChannel, uuid.Nil)
			replayed = was
			return err
		})
		if err != nil {
			return domain.TestResult{}, err
		}
		if replayed {
			return w.lastTestResult(ctx, scope, channelID)
		}
	}
	return w.tester.Test(ctx, scope, channelID)
}

// lastTestResult reconstructs the verdict the first attempt recorded.
//
// ⚠️ IT REPORTS HEALTH, WHICH IS WHAT A TEST WRITES AND ALL A TEST WRITES. The
// provider message ids of the original send are not recoverable and are left
// null rather than invented: a replayed permalink pointing at nothing would be
// worse than no permalink at all.
func (w *Writer) lastTestResult(
	ctx context.Context, scope db.TenantScope, channelID uuid.UUID,
) (domain.TestResult, error) {
	if err := w.requireStore(); err != nil {
		return domain.TestResult{}, err
	}
	inst, err := w.store.Get(ctx, scope, channelID)
	if err != nil {
		return domain.TestResult{}, err
	}
	out := domain.TestResult{
		OK:    inst.Health == domain.InstanceHealthy,
		Error: inst.HealthError,
	}
	if inst.HealthCheckedAt != nil {
		out.CheckedAt = *inst.HealthCheckedAt
	} else {
		out.CheckedAt = w.clk.Now().UTC()
	}
	if !out.OK {
		out.ErrorClass = replayedErrorClass(inst.Health)
		if out.Error == "" {
			out.Error = "the first attempt under this Idempotency-Key did not reach the destination"
		}
	}
	return out, nil
}

// replayedErrorClass maps the banner a failed test raised back onto the class the
// original response carried. `unknown` health means the first attempt recorded
// nothing conclusive, which is a retryable answer and never a permanent one.
func replayedErrorClass(h domain.InstanceHealth) domain.ErrorClass {
	switch h {
	case domain.InstanceAuthFailed:
		return domain.ClassAuthExpired
	case domain.InstanceConfigInvalid:
		return domain.ClassConfigInvalid
	case domain.InstanceDegraded:
		return domain.ClassPermanent
	default:
		return domain.ClassRetryable
	}
}

// ------------------------------------------------------------------- helpers

func (w *Writer) inTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if w.tx == nil {
		return fn(ctx)
	}
	return w.tx.InTx(ctx, fn)
}

func (w *Writer) requireStore() error {
	if w.store != nil {
		return nil
	}
	return errs.Unavailable(CodeChannelStoreUnavailable,
		"the channel store is not configured in this deployment", 0)
}

// requireClaims refuses a KEYED request this deployment cannot honour.
//
// ⛔ IT IS REFUSED, NOT SERVED UNGUARDED. The defect this closes was a header the
// contract promised and the server ignored; ignoring it a second time because a
// collaborator is nil would send the second real message anyway, and the caller
// would have no way to tell a protected test from an unprotected one.
func (w *Writer) requireClaims(idem Idempotency) error {
	if !idem.Keyed {
		return nil
	}
	if w.claims != nil && w.tx != nil {
		return nil
	}
	return errs.Unavailable(CodeIdempotencyUnavailable,
		"this deployment cannot honour Idempotency-Key", 0)
}

// claim takes the caller's key for op and reports whether the first attempt
// already did this, naming what it made.
//
// ⭐ IT REPLAYS RATHER THAN REFUSING. Neither of these responses carries a
// secret, so the honest answer to a retry is the channel that already exists or
// the verdict already reached — which is what the header's own description
// promises. A key held for a DIFFERENT body is still a `409`: that is not a
// retry, it is a second request wearing the first one's name.
func (w *Writer) claim(
	ctx context.Context, scope db.TenantScope, idem Idempotency,
	op idempotency.Operation, created uuid.UUID,
) (replayed bool, ref uuid.UUID, err error) {
	res, err := w.claims.Claim(ctx, scope, idempotency.Claim{
		OrgID:       scope.OrgID(),
		PrincipalID: idem.Principal.UserID,
		Operation:   op,
		Key:         idem.Key,
		RequestHash: idem.RequestHash,
		CreatedRef:  created,
		ClaimedAt:   w.clk.Now().UTC(),
	})
	if err != nil {
		return false, uuid.Nil, err
	}
	if res.Fresh() {
		return false, uuid.Nil, nil
	}
	if res.Outcome == idempotency.Conflicted {
		return false, uuid.Nil, idempotency.Reuse(res)
	}
	if created != uuid.Nil && res.Existing.CreatedRef == uuid.Nil {
		// A create whose replay names nothing cannot be served as one. Refusing
		// carries the same `idempotency_key_reuse` code, so the client still learns
		// that its first attempt succeeded.
		return false, uuid.Nil, idempotency.Reuse(res)
	}
	return true, res.Existing.CreatedRef, nil
}

// errReplay carries a replayed claim out of its own transaction so the create
// beside it rolls back. It never reaches a caller.
var errReplay = errors.New("this idempotency key was already claimed")
