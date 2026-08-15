package jobs

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// THE PER-TENANT FAN-OUT.
//
// ⛔ THE DEFECT THIS CLOSES IS THAT A PER-ROW BOUND DOES NOT BOUND TENANTS. Every
// periodic sweep in oto is per-tenant, because every repository method takes a
// `db.TenantScope` by construction. Each of them bounded the rows it touched for
// ONE tenant — `sweepLimit`, 500 — and then looped every tenant inside a SINGLE
// job execution under a SINGLE fixed timeout. `occurrence.reap` had two minutes
// for the whole customer base; `stats.rollup` had five. Adding a tenant added
// fixed work to every tick inside a budget nobody widened, so the thing that
// eventually blew the timeout was the variable nobody had bounded: how many
// customers there are. A sweep that is not bounded is a sweep that becomes an
// outage the first time somebody has a bad night, and tenant count is exactly as
// unbounded as row count.
//
// ⭐ THE SHAPE IS `source.reconcile`'s, DELIBERATELY, AND IT IS THE ONLY ONE HERE.
// That job's payload already has two shapes under one kind: no source id means
// "this is the fan-out tick, expand it", a source id means "this is one pass".
// Copying it costs no new job kind, no new queue, no new retry policy and no new
// metric — the kind is what all four are keyed on, and the kind does not change.
// What changes is that the tick now does O(1) work per tenant (one row in a batch
// insert) and each tenant's real work gets a WHOLE execution timeout of its own,
// instead of a share of one.
//
// ⚠️ IT WAS NEVER A COPY OF `Reconciler.FanOut` — IT IS THE OTHER WAY ROUND NOW.
// That fan-out is the precedent for the two-shape PAYLOAD and was the precedent
// for nothing else: it looped `Scopes()` synchronously inside one execution, which
// is the very thing this file exists to stop doing. It has since been converted
// onto this one, and is the only caller here whose per-tenant job is itself a
// fan-out — one tenant's due sources, bounded by `sources/service.FanOutLimit`,
// enqueuing work keyed on a SOURCE id. That inner level is its own and is not
// modelled here; what it borrows is the tenant walk above it.

// TenantFanOut is the two-shape payload every per-tenant periodic carries. Embed
// it beside Payload in the args struct.
//
//   - Both fields zero → THE FAN-OUT TICK. Expand it into one job per tenant.
//   - OrgID set → ONE TENANT'S PASS. Do that tenant's work and nothing else.
//   - After set → A CONTINUATION of the fan-out tick, resuming the tenant walk
//     after that id. See FanOutTenants for why a continuation exists at all.
//
// ⛔ NEITHER FIELD IS AUTHORITY. A job row is data, and data that decided its own
// authorisation would undo the tenancy boundary — `occurrenceScopes` says the
// same thing about the one other place a scope is produced rather than passed.
// The org id here is a HINT about which tenant to look up; the `orgs` table is
// what answers whether that tenant exists and is still live. `TenantScoper` is
// where that lookup is required, and a handler that builds a scope straight from
// the payload instead has re-opened the hole.
//
// ⚠️ THE FIELDS ARE NOT `omitempty` AND MUST NOT BECOME SO. `uuid.UUID` is
// `[16]byte`, and `encoding/json` never omits an array, so the tag would be a
// comment that lies. They are also what River's ByArgs uniqueness hashes: a
// distinct org id is a distinct unique key, which is what gives each tenant its
// own once-per-period slot instead of all of them sharing one.
//
// Payload version stays 1 across this addition, per the rule Payload states: an
// ABSENT field decodes to the zero value, which is the fan-out tick, which is
// what a pre-existing queued row meant. Adding a field is safe; changing the
// meaning of one is not.
type TenantFanOut struct {
	// OrgID names the one tenant this execution is for. Nil means the fan-out.
	OrgID uuid.UUID `json:"org_id"`
	// After is the keyset cursor a continuation resumes the tenant walk from.
	// Meaningless unless OrgID is nil.
	After uuid.UUID `json:"after"`
}

// IsFanOut reports whether this payload is the fan-out tick rather than one
// tenant's pass.
func (t TenantFanOut) IsFanOut() bool { return t.OrgID == uuid.Nil }

// TenantFanOutLimit is how many tenants ONE fan-out execution may enqueue for.
//
// It is 500 because `sweepLimit` and `orgPageSize` are 500, and it is the same
// number for the same reason they are: a bound nobody reaches in practice is
// still the bound that keeps a bad night from becoming an outage. It bounds the
// batch insert and the memory the tick holds, and it is deliberately far above
// any tenant count oto is designed for — reaching it is a fact worth logging,
// not a routine outcome.
//
// ⛔ IT DEFERS, IT NEVER DROPS. See FanOutTenants: a truncated page queues a
// continuation carrying the cursor, so the tenants past the ceiling are reached
// by the next execution rather than starved forever. A ceiling that silently
// stopped at the first 500 tenants sorted by id would be worse than the unbounded
// loop it replaced — the loop at least visited everybody.
//
// ⛔ AND THAT PROMISE HOLDS ONLY WHILE THE PAGER CAN SERVE THIS NUMBER. "Deferred,
// never dropped" is inferred from ONE observation — a page that came back full —
// so a pager that returned fewer ids than were asked for, for any reason other
// than reaching the end of the table, would make this comment false without
// touching it: no page is ever full, no continuation is ever queued, and every
// tenant past the pager's own smaller number is starved permanently. It is not a
// hazard left to review. `app.orgPageSize` IS this constant rather than a second
// copy of 500, and `orgLister.ScopePage` fails loudly rather than serve a smaller
// page than it was asked for.
const TenantFanOutLimit = 500

// TenantPager reads ONE bounded, keyset-ordered page of live tenants.
//
// It returns bare ids rather than scopes on purpose: the fan-out only needs
// something to put in a payload, and the scope is built at execution time by
// TenantScoper against the table. Producing a scope here and trusting it later
// would be the payload-as-authority mistake with extra steps.
type TenantPager interface {
	// ScopePage returns at most limit live org ids strictly after `after`, in
	// ascending id order.
	//
	// ⛔ A SHORT PAGE MEANS THE END OF THE TABLE AND MAY MEAN NOTHING ELSE. It is
	// the only signal FanOutTenants has, and it decides whether the walk
	// continues. An implementation that clamps `limit` down to a ceiling of its
	// own says "no more tenants" about tenants that exist, and they are then
	// never swept again. An implementation that cannot serve the limit must
	// return an error, not a smaller page.
	ScopePage(ctx context.Context, after uuid.UUID, limit int) ([]uuid.UUID, error)
}

// TenantScoper turns the org id a payload NAMES into the scope the table
// AUTHORISES.
//
// ⛔ A soft-deleted tenant must resolve to KindNotFound, not to a usable scope.
// The fan-out reads live tenants, but a tenant can depart between the tick and
// the pass, and that window is the whole reason this is a lookup rather than a
// cast: sweeping a departed tenant is work producing alerts, reminders and flap
// scores nobody will ever read, which is exactly what be3d314 removed from the
// list side.
type TenantScoper interface {
	LiveScope(ctx context.Context, orgID uuid.UUID) (db.TenantScope, error)
}

// Tenants is both halves, which is what a per-tenant periodic needs: the pager
// for its fan-out shape and the scoper for its per-tenant shape.
type Tenants interface {
	TenantPager
	TenantScoper
}

// CodeFanOutUnwired is the failure of a periodic that cannot reach the tenant
// list or the queue. It is INTERNAL and loud: a fan-out that quietly enqueued
// nothing is a sweep that has stopped, and a sweep that has stopped looks exactly
// like a system with nothing to do.
const CodeFanOutUnwired = "tenant_fanout_unwired"

// CodeFanOutFailed is a fan-out whose batch insert did not land.
const CodeFanOutFailed = "tenant_fanout_failed"

// FanOutOutcome is what one fan-out execution did, for the caller's log line.
type FanOutOutcome struct {
	// Enqueued is how many per-tenant jobs were inserted, excluding any
	// continuation.
	Enqueued int
	// Deferred reports that the page came back full, so a continuation was queued
	// and more tenants remain. It is a normal outcome, never an error — but it is
	// one an operator must be able to find afterwards.
	Deferred bool
	// Cursor is the last org id this execution reached. Zero when it reached none.
	Cursor uuid.UUID
}

// FanOutTenants expands one fan-out tick into one job per live tenant, at most
// TenantFanOutLimit of them, and queues a continuation when it stopped at the
// ceiling.
//
// `build` is called once per job and receives the payload half; the caller
// returns its own args struct with that half embedded. One closure rather than
// two because the continuation is the SAME args type with a different half of the
// same field pair set, and a caller that could get those two out of step is a
// caller that could enqueue a tick as a tenant pass.
//
// ⭐ THE CONTINUATION IS WHY THIS IS A CEILING AND NOT A CLIFF. The obvious
// bounded fan-out — take the first 500 tenants and log the rest as unreached —
// starves every tenant past the boundary FOREVER, because the walk restarts at
// the same place on every tick. `grouping`'s member ceiling can report an
// unreached remainder and stop, because a human is holding the request and can
// press again; nobody is holding a periodic tick. So the remainder is not
// reported and abandoned, it is CARRIED: the continuation is an ordinary queued
// job with the cursor in its payload, which makes the deferred work durable
// (it survives the leader moving to another pod, which an in-memory cursor would
// not), visible in `river_job` like everything else, and self-terminating — the
// chain ends at the first short page.
//
// ⛔ THE CHAIN CANNOT SPIN. A continuation is queued only when the page came back
// FULL, and a full page always advances the cursor because the walk is strictly
// `id > $1`. An empty or short page is terminal. That leaves no arrangement of
// the tenant table that produces a continuation which asks for the same page
// again.
//
// ⚠️ ONE BATCH INSERT, NOT ONE INSERT PER TENANT. The point of the exercise is
// that the tick's own cost stops being proportional to tenant count in ROUND
// TRIPS; 500 individual inserts would have moved the problem rather than solved
// it.
func FanOutTenants(
	ctx context.Context,
	kind string,
	enq db.Enqueuer,
	tenants TenantPager,
	log *slog.Logger,
	after uuid.UUID,
	build func(TenantFanOut) db.JobArgs,
) (FanOutOutcome, error) {
	if enq == nil || tenants == nil || build == nil {
		return FanOutOutcome{}, errs.New(errs.KindInternal, CodeFanOutUnwired,
			kind+" has no tenant list or no queue, so it cannot fan out")
	}
	if log == nil {
		log = slog.Default()
	}

	orgIDs, err := tenants.ScopePage(ctx, after, TenantFanOutLimit)
	if err != nil {
		return FanOutOutcome{}, err
	}
	if len(orgIDs) == 0 {
		return FanOutOutcome{}, nil
	}

	reqs := make([]db.JobRequest, 0, len(orgIDs)+1)
	for _, orgID := range orgIDs {
		reqs = append(reqs, db.JobRequest{Args: build(TenantFanOut{OrgID: orgID})})
	}

	out := FanOutOutcome{Enqueued: len(orgIDs), Cursor: orgIDs[len(orgIDs)-1]}
	if len(orgIDs) >= TenantFanOutLimit {
		out.Deferred = true
		reqs = append(reqs, db.JobRequest{Args: build(TenantFanOut{After: out.Cursor})})
	}

	if _, err := enq.EnqueueMany(ctx, reqs); err != nil {
		return FanOutOutcome{}, errs.Wrap(err, errs.KindUnavailable, CodeFanOutFailed,
			kind+" could not queue its per-tenant passes")
	}

	if out.Deferred {
		// A truncated fan-out is a normal, bounded outcome and never an error —
		// but nothing else is going to say that this tick did not reach everybody,
		// and the continuation that fixes it is one more row in a table with
		// millions of them.
		log.WarnContext(ctx, "jobs: tenant fan-out stopped at the ceiling and queued a continuation",
			slog.String("kind", kind),
			slog.Int("limit", TenantFanOutLimit),
			slog.Int("enqueued", out.Enqueued),
			slog.String("resume_after", out.Cursor.String()))
	}
	return out, nil
}

// ForTenant is the other half of a two-shape handler: resolve the payload's org
// id against the table and run fn under the scope that lookup produced.
//
// ⛔ A TENANT THAT HAS DEPARTED IS NOT A JOB FAILURE. The org was live when the
// fan-out read it and is gone now; there is nothing to sweep and nothing to
// retry, so this returns nil rather than burning the periodic retry budget on a
// row that is never coming back. `source.reconcile` answers a deleted source the
// same way for the same reason.
func ForTenant(
	ctx context.Context,
	kind string,
	tenants TenantScoper,
	orgID uuid.UUID,
	fn func(context.Context, db.TenantScope) error,
) error {
	if tenants == nil {
		return errs.New(errs.KindInternal, CodeFanOutUnwired,
			kind+" cannot resolve the tenant its payload names")
	}
	scope, err := tenants.LiveScope(ctx, orgID)
	if err != nil {
		if errs.IsKind(err, errs.KindNotFound) {
			return nil
		}
		return err
	}
	return fn(ctx, scope)
}
