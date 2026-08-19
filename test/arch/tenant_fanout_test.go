package arch

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/thulasiram/oto/internal/platform/jobs"
)

// ⭐⭐ WHY THIS FILE EXISTS.
//
// `jobs.TenantFanOut` closed a defect the compiler cannot see coming back: a
// per-tenant periodic that loops every tenant inside ONE execution compiles,
// works on the three-org install it was written against, and becomes an outage
// as a linear function of the customer count — one fixed timeout silently
// divided by every tenant, one tenant's failure a log line inside everybody
// else's sweep. `notify.unacked_reminder` was exactly that shape for months
// after the other five periodics were converted, because nothing failed when it
// stayed behind. This gate is what fails.
//
// ⚠️ THAT KIND NO LONGER EXISTS — git-bug bd0fb1d deleted the reminder outright —
// and the file is deliberately still written around it. The defect was real, the
// planted `legacySweepArgs` below is still byte-for-byte its pre-conversion shape,
// and a gate explained by the case that motivated it survives the case going away.
//
// ⛔ THE DISCOVERY ENUMERATES THE SEAM, NEVER A HAND-LIST OF KINDS. A hand-list
// of per-tenant periodics inside the gate is the defect the gate exists to kill:
// the next periodic would be added to `jobs.Handlers` and not to the list, and
// the gate would wave through the very kind it was built for. So the walk reads
// `jobs.Handlers` — the complete set of job kinds oto registers (registry.go
// calls it THE SEAM) — takes each handler's args type by reflection, and calls
// the args' own InsertOpts: a uniqueness window (`ByPeriod > 0`, the shape
// `periodicOpts` pins on every scheduled kind) is what makes a kind periodic.
// The only list this file keeps is the OPPOSITE one — the periodics that are
// genuinely NOT per-tenant, each with its reason — so a new periodic fails
// CLOSED: it either embeds the fan-out half or argues its way onto that list.

// notPerTenantPeriodic names the periodic kinds that have NO per-tenant unit of
// work to fan out, and why. Everything periodic and not in here must embed
// jobs.TenantFanOut.
//
// ⭐ ADDING A LINE HERE IS THE DECISION, NOT THE PAPERWORK. An entry says "this
// sweep has no tenant to loop over", which is an architectural claim about the
// tables it touches — state the reason, and expect the stale-entry check below
// to delete the line the day the claim stops being true.
var notPerTenantPeriodic = map[string]string{
	jobs.KindPartitionsManage: "a partition is a property of the TABLE, not of a row, and the window it " +
		"drops at is a REDUCE over every tenant's settings (identity/service.MaxRetention) rather than a " +
		"map — there is no per-tenant unit of work to enqueue (app.managePartitions's ⛔⛔ argues this in full)",
	jobs.KindCacheExpire: "a bounded eviction over `enrichment_cache`, which ages out by cache key and " +
		"has no tenant to loop over",
	jobs.KindSilencesSync: "its payload names a SOURCE, and the source names the tenant; the tenant walk " +
		"that reaches it is `source.reconcile`'s own fan-out, which enqueues one of these per due source",
}

func TestEveryPerTenantPeriodicCarriesTheFanOutHalf(t *testing.T) {
	seen := make(map[string]bool, len(notPerTenantPeriodic))
	periodics := 0

	for _, argsT := range registeredJobArgs(t) {
		zero := reflect.New(argsT).Elem().Interface()
		args, ok := zero.(river.JobArgs)
		if !ok {
			t.Fatalf("%s does not implement river.JobArgs by value; the discovery cannot read its kind", argsT)
		}
		if periodicWindow(zero) <= 0 {
			// Not periodic: either event-driven (no uniqueness window at all) or a
			// kind whose schedule some other gate owns. A PERIODIC without a window
			// would be two leader-elected pods inserting the same tick, which is why
			// every scheduled kind pins one through periodicOpts — the window is the
			// honest mechanical signal for "this runs on a clock".
			continue
		}
		periodics++

		kind := args.Kind()
		if reason, exempt := notPerTenantPeriodic[kind]; exempt {
			seen[kind] = true
			if embedsTenantFanOut(argsT) {
				t.Errorf(""+
					"periodic job %q embeds jobs.TenantFanOut but is listed in notPerTenantPeriodic\n"+
					"  (its entry claims: %s)\n"+
					"an exemption over a kind that now carries the fan-out half is a standing "+
					"permission nobody argued for — delete the entry.",
					kind, reason)
			}
			continue
		}
		if msg := fanOutViolation(kind, argsT); msg != "" {
			t.Error(msg)
		}
	}

	if periodics == 0 {
		t.Fatal("found no periodic job kinds at all — the discovery is broken, not the schedule")
	}
	for kind := range notPerTenantPeriodic {
		if !seen[kind] {
			t.Errorf(""+
				"notPerTenantPeriodic names %q, which is not a registered periodic job kind\n"+
				"delete the entry: an exemption nothing backs re-authorises itself the day "+
				"a kind by that name comes back.",
				kind)
		}
	}
}

// legacySweepArgs is the shape `notify.unacked_reminder` had before it was
// converted (the kind itself is gone — bd0fb1d — and this shape outlives it): a periodic uniqueness window, a Payload, and no fan-out half — the
// all-tenants loop hid in its handler where no gate could see it, and the args
// were the one place it showed.
type legacySweepArgs struct {
	jobs.Payload
}

func (legacySweepArgs) Kind() string { return "arch.legacy_sweep" }

func (legacySweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByQueue: true, ByPeriod: time.Minute},
	}
}

// TestPerTenantPeriodicGateFires plants the pre-conversion shape and checks the
// real predicates report it.
//
// ⭐ IT IS THE ONLY PROOF THE GATE HAS TEETH. Every registered periodic either
// embeds TenantFanOut or sits on the exempt list today, so the walk above passes
// whether the checks work, are inverted, or are deleted outright — which is
// exactly how a gate rots. The planted type is byte-for-byte the reminder's old
// args, so this is also the record that the gate would have caught the last
// synchronous sweep before it was converted.
func TestPerTenantPeriodicGateFires(t *testing.T) {
	if periodicWindow(legacySweepArgs{}) <= 0 {
		t.Fatal("the discovery does not classify the planted legacy sweep as periodic, so the gate never looks at it")
	}

	msg := fanOutViolation("arch.legacy_sweep", reflect.TypeOf(legacySweepArgs{}))
	if msg == "" {
		t.Fatal("the gate waved through a periodic with no fan-out half — the exact shape it exists to refuse")
	}
	// The message has to survive too: a gate that fires without saying what the
	// embedding buys gets pacified with a suppression instead of a conversion.
	for _, want := range []string{
		"jobs.TenantFanOut",
		"WHOLE execution budget",
		"dead-letters",
		"notPerTenantPeriodic",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure message does not mention %q:\n%s", want, msg)
		}
	}

	// The digest tick is the negative half: a real registered per-tenant periodic
	// whose shape SATISFIES the gate, so the two halves together prove the gate
	// discriminates rather than just always firing.
	//
	// ⚠️ IT USED TO BE `notify.unacked_reminder`, which was the kind this whole file
	// was built for. git-bug bd0fb1d deleted that kind, so the negative half moved to
	// the digest — the other per-tenant periodic of exactly the same shape. The
	// planted `legacySweepArgs` above is STILL byte-for-byte the reminder's
	// pre-conversion args, and that is the half worth keeping: it is the record of
	// the defect, and it does not depend on the kind still existing.
	if v := fanOutViolation(jobs.KindNotifyDigest, reflect.TypeOf(jobs.NotifyDigestArgs{})); v != "" {
		t.Errorf("the digest tick's args do not satisfy the gate:\n%s", v)
	}
}

// registeredJobArgs reads every args type off `jobs.Handlers`, the seam that IS
// the set of kinds RegisterAll registers. A field that is not a
// `jobs.Handler[T]` fails loudly: a discovery that skipped what it could not
// read would exempt kinds by accident, silently.
func registeredJobArgs(t *testing.T) []reflect.Type {
	t.Helper()

	seam := reflect.TypeOf(jobs.Handlers{})
	out := make([]reflect.Type, 0, seam.NumField())
	for i := 0; i < seam.NumField(); i++ {
		f := seam.Field(i)
		// jobs.Handler[T] is `func(context.Context, *jobs.Job[T]) error`; the args
		// type is Job[T]'s Args field.
		if f.Type.Kind() != reflect.Func || f.Type.NumIn() != 2 || f.Type.In(1).Kind() != reflect.Pointer {
			t.Fatalf("jobs.Handlers.%s is not a jobs.Handler[T] (%s); the discovery reads the seam and must not skip a kind it cannot parse", f.Name, f.Type)
		}
		job := f.Type.In(1).Elem()
		args, ok := job.FieldByName("Args")
		if !ok {
			t.Fatalf("jobs.Handlers.%s: %s carries no Args field; the discovery cannot see its payload type", f.Name, job)
		}
		out = append(out, args.Type)
	}
	return out
}

// periodicWindow is the discovery's one predicate: the uniqueness period the
// args pin on themselves, zero when they pin none. Every scheduled kind in oto
// gets its window from `periodicOpts`, so a positive window is what "periodic"
// mechanically means here.
func periodicWindow(zero any) time.Duration {
	opts, ok := zero.(interface{ InsertOpts() river.InsertOpts })
	if !ok {
		return 0
	}
	return opts.InsertOpts().UniqueOpts.ByPeriod
}

var tenantFanOutType = reflect.TypeOf(jobs.TenantFanOut{})

// embedsTenantFanOut reports whether argsT carries the fan-out half the way the
// dispatch pattern reads it: EMBEDDED, so IsFanOut, OrgID and After are promoted
// and River's ByArgs uniqueness hashes them into each tenant's own slot.
func embedsTenantFanOut(argsT reflect.Type) bool {
	for i := 0; i < argsT.NumField(); i++ {
		f := argsT.Field(i)
		if f.Anonymous && f.Type == tenantFanOutType {
			return true
		}
	}
	return false
}

// fanOutViolation returns "" when the periodic args type carries the fan-out
// half, or the argued failure otherwise. It is built apart from the test that
// prints it so TestPerTenantPeriodicGateFires can assert the gate says WHY and
// not merely that it fired.
func fanOutViolation(kind string, argsT reflect.Type) string {
	if embedsTenantFanOut(argsT) {
		return ""
	}
	return fmt.Sprintf(""+
		"periodic job %q (%s) does not embed jobs.TenantFanOut\n"+
		"A per-tenant periodic without the fan-out half loops every tenant inside ONE "+
		"execution: the kind's fixed timeout is silently divided by the customer count, "+
		"one tenant's failure is a swallowed log line inside everybody else's sweep, and "+
		"a departed tenant is swept from payload data instead of being resolved against "+
		"the live-org table. Embedding TenantFanOut buys the opposite — the tick only "+
		"ENQUEUES, each tenant's pass gets the kind's WHOLE execution budget, retries on "+
		"its own periodic ladder and dead-letters under its own payload, and "+
		"jobs.ForTenant answers a departed tenant with nil instead of work nobody will read.\n"+
		"Either embed jobs.TenantFanOut beside Payload and dispatch through "+
		"jobs.FanOutTenants / jobs.ForTenant (read `case.reap` or `stats.rollup` "+
		"end-to-end first), or — if this kind genuinely has no per-tenant unit of work — "+
		"add it to notPerTenantPeriodic in this file WITH ITS REASON.",
		kind, argsT)
}
