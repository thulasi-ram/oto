package service

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
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
	CodeIdempotencyUnavailable = idempotency.CodeUnavailable
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

// Idempotency is the caller's `Idempotency-Key` intent for one write — the
// platform's own Intent under the name this module's ports cross it as. What
// "the same request" means here: the sha256 of the body for a create, and of
// the CHANNEL for a test. The operation is this service's fact and is filled at
// the claim site, not by the transport.
type Idempotency = idempotency.Intent

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
	if err := idempotency.Require(idem, w.claims, w.tx); err != nil {
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
	// ⭐ NEITHER RESPONSE HERE CARRIES A SECRET, so the policy is Replay: the
	// honest answer to a retry is the channel that already exists or the verdict
	// already reached — which is what the header's own description promises.
	idem.Operation = opCreateChannel
	err := w.inTx(ctx, func(ctx context.Context) error {
		if idem.Keyed {
			ref, err := idempotency.Resolve(ctx, w.claims, scope, idem,
				idempotency.Replay, in.ID, w.clk.Now().UTC())
			if err != nil {
				replayOf = ref
				return err
			}
		}
		created, err := w.store.Create(ctx, scope, in)
		if err != nil {
			return err
		}
		out = created
		return nil
	})
	if errors.Is(err, idempotency.ErrReplay) {
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
	if err := idempotency.Require(idem, w.claims, w.tx); err != nil {
		return domain.TestResult{}, err
	}

	if idem.Keyed {
		idem.Operation = opTestChannel
		err := w.inTx(ctx, func(ctx context.Context) error {
			// CreatedRef is Nil: a test creates no row. The replay is served from
			// the destination's health rather than from anything the claim names —
			// which is exactly the case Resolve's reconciled edge permits.
			_, err := idempotency.Resolve(ctx, w.claims, scope, idem,
				idempotency.Replay, uuid.Nil, w.clk.Now().UTC())
			return err
		})
		if errors.Is(err, idempotency.ErrReplay) {
			return w.lastTestResult(ctx, scope, channelID)
		}
		if err != nil {
			return domain.TestResult{}, err
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
