package jobs_test

import (
	"testing"

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
