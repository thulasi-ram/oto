package app

import (
	"context"
	"sync/atomic"
	"time"

	channelsrepo "github.com/thulasiram/oto/internal/channels/repository"
	notifservice "github.com/thulasiram/oto/internal/notification/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/secrets"
	sourcesrepo "github.com/thulasiram/oto/internal/sources/repository"
)

// lateEnqueuer is the transactional outbox, handed out before it exists.
//
// The job client cannot be built until every handler is, and every handler needs
// the services, and every service needs an enqueuer. That is a genuine cycle in
// construction order and not in dependency direction, so it is broken with one
// atomic pointer set exactly once at the end of wiring.
//
// ⛔ It NEVER silently drops. A job enqueued before the client exists — which can
// only happen if a service is used during construction — returns an
// `unavailable` error, because on the ingest path that becomes a 503 with a
// Retry-After and the upstream keeps the alert. Swallowing it would turn "202
// Accepted is a promise" into a lie.
type lateEnqueuer struct {
	inner atomic.Pointer[db.Enqueuer]
}

func (e *lateEnqueuer) set(q db.Enqueuer) { e.inner.Store(&q) }

func (e *lateEnqueuer) get() (db.Enqueuer, error) {
	if p := e.inner.Load(); p != nil && *p != nil {
		return *p, nil
	}
	return nil, errs.Unavailable("queue_unavailable",
		"the job queue is not started yet; retry", 0)
}

// Enqueue inserts one job, joining the caller's transaction when ctx carries one.
func (e *lateEnqueuer) Enqueue(ctx context.Context, args db.JobArgs, opts ...db.JobOption) (db.EnqueueResult, error) {
	q, err := e.get()
	if err != nil {
		return db.EnqueueResult{}, err
	}
	return q.Enqueue(ctx, args, opts...)
}

// EnqueueMany inserts a batch in one round trip. A 200-alert webhook must not
// become 200 inserts.
func (e *lateEnqueuer) EnqueueMany(ctx context.Context, reqs []db.JobRequest) ([]db.EnqueueResult, error) {
	q, err := e.get()
	if err != nil {
		return nil, err
	}
	return q.EnqueueMany(ctx, reqs)
}

// keyringSealer and the three unsealer adapters hand the ONE platform keyring to
// the consumer-declared ports without any module naming `platform/secrets`
// itself.
//
// A nil keyring stays nil rather than becoming a no-op sealer. The repositories
// already refuse to store or read a credential without one — "this deployment
// has no credential keyring configured" — and a no-op would be the difference
// between a deployment that cannot configure a channel and one that stores a bot
// token in the clear.
//
// ⭐ EACH RETURNS THE PORT TYPE, NOT `*secrets.Keyring`, AND THAT IS THE WHOLE
// POINT. Returning the concrete stored a TYPED NIL in the interface field: an
// interface value holding a `(*secrets.Keyring)(nil)` is NOT `== nil`, so the
// `if r.open == nil` guard in every credential repository never fired, and the
// promised "fails loudly at the repository" became `Keyring.Unseal` dereferencing
// `k.aeads` on a nil receiver — a panic, recovered as a 500, on a path whose
// whole design is to fail closed. Converting to the interface HERE, where the
// nil is still visible as a nil, is what makes the guards downstream true.
func keyringSealer(k *secrets.Keyring) channelsrepo.Sealer {
	if k == nil {
		return nil
	}
	return k
}

func channelsUnsealer(k *secrets.Keyring) channelsrepo.Unsealer {
	if k == nil {
		return nil
	}
	return k
}

func sourcesUnsealer(k *secrets.Keyring) sourcesrepo.Unsealer {
	if k == nil {
		return nil
	}
	return k
}

func dispatchUnsealer(k *secrets.Keyring) notifservice.CredentialUnsealer {
	if k == nil {
		return nil
	}
	return k
}

// retentionCeiling is the ONE read `foldRetention` makes: the widest retention
// window any live tenant has asked for, evaluated by `identity/service.MaxRetention`
// — inside identity, where the declarative overlay is applied, because the
// effective retention exists only on the far side of `Org.WithDeclarative`. An
// aggregate this package wrote over the raw `orgs.settings` column would skip
// that overlay and compute a maximum over numbers nobody is using.
//
// ⛔ IT IS A PORT BECAUSE IT IS ITS FAILURE THAT HAS TO BE PINNED, not because
// anything here needs a second implementation. The concrete is
// `identity.Service`, which cannot be asked to return an error from a test.
// `effectiveRetention`'s rule is that EVERY failure widens the window,
// retention is the one setting pair whose wrong value is unrecoverable, and a
// rule about failures that no test can reach is a comment: both of the fold's
// old failure paths were in fact narrowing when the ports were introduced.
type retentionCeiling interface {
	MaxRetention(ctx context.Context) (raw, event time.Duration, err error)
}

// ⭐ THE PORT-DRIFT ASSERTIONS.
//
// Every one of these says: this concrete satisfies that consumer-declared port.
// They exist so a signature change breaks the BUILD rather than surfacing as a
// nil interface at boot or, worse, as a method that is never called. In a
// composition root the alternative to a compile error is a runtime panic in
// production, which is not an alternative.
//
// The identity assertions are the ones that module explicitly asked for; the
// rest follow the same rule for the same reason.
var (
	// platform/secrets satisfies every sealed-secret port in the system. This is
	// what makes "one keyring for the whole process" checkable.
	_ channelsrepo.Sealer             = (*secrets.Keyring)(nil)
	_ channelsrepo.Unsealer           = (*secrets.Keyring)(nil)
	_ sourcesrepo.Unsealer            = (*secrets.Keyring)(nil)
	_ notifservice.CredentialUnsealer = (*secrets.Keyring)(nil)

	// The late-bound outbox is a db.Enqueuer like any other.
	_ db.Enqueuer = (*lateEnqueuer)(nil)
)
