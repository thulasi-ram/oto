package jobs_test

import (
	"testing"

	"github.com/thulasiram/oto/internal/platform/jobs"
)

// TestCorrelationNeverSharesAQueueWithANotification is git-bug 61eeddf's "an
// evaluation failure never blocks or delays a notification", stated where it is
// decided.
//
// A Correlator runs as `incidents.correlate`, a job a Case opening enqueues beside
// its own `notify.evaluate`. If the two shared a queue, a storm's Correlator
// evaluations — or one Correlator stuck retrying — would sit in the very worker
// slots the `fired` evaluations are waiting for, and a notification would be
// delayed by a feature that was promised never to touch one. Different queues make
// that impossible rather than unlikely.
func TestCorrelationNeverSharesAQueueWithANotification(t *testing.T) {
	correlate := jobs.IncidentsCorrelateArgs{}.InsertOpts().Queue
	for _, notify := range []string{
		jobs.NotifyEvaluateArgs{}.InsertOpts().Queue,
		jobs.NotifyIncidentArgs{}.InsertOpts().Queue,
	} {
		if correlate == notify {
			t.Fatalf("incidents.correlate rides %q, the queue a notification is evaluated on", correlate)
		}
	}
	if correlate != jobs.QueueLifecycle {
		t.Fatalf("incidents.correlate rides %q, want %q", correlate, jobs.QueueLifecycle)
	}
}

// TestTheCorrelateKindIsRegisteredOnTheLifecycleQueue pins the WORKER side of the
// same rule: the kind has a registration — so a Case's correlate job is never an
// unknown kind retried to its ceiling — and the spec names the queue the producer
// inserts on.
func TestTheCorrelateKindIsRegisteredOnTheLifecycleQueue(t *testing.T) {
	r := jobs.NewRegistry(nil)
	if err := jobs.RegisterAll(r, jobs.Handlers{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, s := range r.Specs() {
		if s.Kind != jobs.KindIncidentsCorrelate {
			continue
		}
		if s.Queue != jobs.QueueLifecycle {
			t.Fatalf("%s is registered on %q, want %q", s.Kind, s.Queue, jobs.QueueLifecycle)
		}
		return
	}
	t.Fatalf("%s is not registered", jobs.KindIncidentsCorrelate)
}
