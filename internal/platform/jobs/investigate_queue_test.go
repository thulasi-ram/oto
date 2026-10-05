package jobs_test

import (
	"testing"

	"github.com/riverqueue/river/rivertype"

	"github.com/thulasiram/oto/internal/platform/config"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// TestAnInvestigationNeverSharesAQueueWithTheNotificationPath is git-bug 180a525's
// "runs async and never touches the notification path", stated where the queue is
// decided (ADR 0053 §3).
//
// An Investigation is a chain of paid model calls that may run for minutes. On
// `notify` or `deliver_*` it would sit in the slots a notification waits for; on
// `enrich` it would sit in the slots the inline enrichment pass — which releases a
// Case's FIRST notification early — waits for. On `investigate` it can delay only
// another Investigation.
func TestAnInvestigationNeverSharesAQueueWithTheNotificationPath(t *testing.T) {
	t.Parallel()

	run := jobs.InvestigationsRunArgs{}.InsertOpts().Queue
	if run != jobs.QueueInvestigate {
		t.Fatalf("investigations.run rides %q, want %q", run, jobs.QueueInvestigate)
	}
	for _, q := range []string{
		jobs.NotifyEvaluateArgs{}.InsertOpts().Queue,
		jobs.NotifyIncidentArgs{}.InsertOpts().Queue,
		jobs.EnrichRunArgs{}.InsertOpts().Queue,
		jobs.QueueDeliverSlack, jobs.QueueDeliverWebhook,
	} {
		if run == q {
			t.Fatalf("investigations.run rides %q, a queue the notification path waits on", q)
		}
	}
}

// TestTheInvestigateKindIsRegisteredWithARunsWorthOfTimeout pins the worker side: the
// kind is registered on its own queue, and its timeout outlasts the largest wall-time
// budget an Investigator may set, so the budget — not the job timeout — is what ends
// a long run and gets to record how it ended.
func TestTheInvestigateKindIsRegisteredWithARunsWorthOfTimeout(t *testing.T) {
	t.Parallel()

	r := jobs.NewRegistry(nil)
	if err := jobs.RegisterAll(r, jobs.Handlers{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, s := range r.Specs() {
		if s.Kind != jobs.KindInvestigationsRun {
			continue
		}
		if s.Queue != jobs.QueueInvestigate {
			t.Fatalf("%s is registered on %q, want %q", s.Kind, s.Queue, jobs.QueueInvestigate)
		}
		if s.Timeout != jobs.InvestigationJobTimeout {
			t.Fatalf("%s times out at %s, want %s", s.Kind, s.Timeout, jobs.InvestigationJobTimeout)
		}
		return
	}
	t.Fatalf("%s is not registered", jobs.KindInvestigationsRun)
}

// TestAnIncidentTriggerNeverWaitsOnARunOrANotification — git-bug 74ea849. The job a
// membership change enqueues to start Investigations is lifecycle work: never on
// `investigate`, where it would wait behind minutes-long runs for a slot, and never on
// `notify`, where it would sit in front of the facts it was enqueued beside.
func TestAnIncidentTriggerNeverWaitsOnARunOrANotification(t *testing.T) {
	t.Parallel()

	q := jobs.InvestigationsIncidentArgs{}.InsertOpts().Queue
	if q != jobs.QueueLifecycle {
		t.Fatalf("investigations.incident rides %q, want %q", q, jobs.QueueLifecycle)
	}
	r := jobs.NewRegistry(nil)
	if err := jobs.RegisterAll(r, jobs.Handlers{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, s := range r.Specs() {
		if s.Kind == jobs.KindInvestigationsIncident {
			if s.Queue != jobs.QueueLifecycle {
				t.Fatalf("%s is registered on %q, want %q", s.Kind, s.Queue, jobs.QueueLifecycle)
			}
			return
		}
	}
	t.Fatalf("%s is not registered", jobs.KindInvestigationsIncident)
}

// TestAnIncidentTriggerWaitsBehindTheDigestAndFoldsABurst — review B1: the trigger rides
// `lifecycle` beside `notify.digest`, so it is BACKGROUND (behind the digest tick) and
// unique by args while one is waiting or running, so a Correlator storm folds into it.
func TestAnIncidentTriggerWaitsBehindTheDigestAndFoldsABurst(t *testing.T) {
	t.Parallel()

	o := jobs.InvestigationsIncidentArgs{}.InsertOpts()
	if o.Priority != jobs.PriorityBackground || o.Priority <= (jobs.NotifyDigestArgs{}).InsertOpts().Priority {
		t.Fatalf("priority %d, want background, behind notify.digest", o.Priority)
	}
	if !o.UniqueOpts.ByArgs {
		t.Fatal("not unique by args: a burst of membership changes would queue one trigger each")
	}
	states := map[rivertype.JobState]bool{}
	for _, s := range o.UniqueOpts.ByState {
		states[s] = true
	}
	for _, s := range []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending,
		rivertype.JobStateScheduled, rivertype.JobStateRunning} {
		if !states[s] {
			t.Fatalf("unique states %v lack %s", o.UniqueOpts.ByState, s)
		}
	}
	if states[rivertype.JobStateCompleted] || states[rivertype.JobStateDiscarded] || states[rivertype.JobStateCancelled] {
		t.Fatalf("unique states %v would swallow a later change's trigger", o.UniqueOpts.ByState)
	}
}

// TestTheInvestigateQueueHasAKnob — review A10: the deployment-wide width defaults to 8
// and `jobs.queue_investigate` moves it.
func TestTheInvestigateQueueHasAKnob(t *testing.T) {
	t.Parallel()

	if got := jobs.FromPlatformConfig(config.JobsConfig{}).Queues[jobs.QueueInvestigate]; got != 8 {
		t.Fatalf("default investigate width = %d, want 8", got)
	}
	if got := jobs.FromPlatformConfig(config.JobsConfig{QueueInvestigate: 5}).Queues[jobs.QueueInvestigate]; got != 5 {
		t.Fatalf("queue_investigate 5 gave %d", got)
	}
}
