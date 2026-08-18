package jobs_test

import (
	"testing"

	"github.com/thulasiram/oto/internal/platform/config"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// TestConfigDefaultsProduceTheSpecQueueWidths is git-bug 39e48e2.
//
// There were two default tables. `jobs.DefaultQueueWorkers` carried the SPEC §G.3
// numbers AND the reasoning for each — why `deliver_slack` is narrowest, why
// `maintenance` is one — and `config.Default()` carried the numbers that actually
// ran, with reasoning for one of them. `FromPlatformConfig` applies a config field
// only when it is > 0, a fall-through written so an unset knob lands on the SPEC
// number; `config.Default()` left none of them unset, so nothing fell through and
// six of eight queues ran a width the published table does not name.
//
// ⛔ THE FAILURE WAS SILENT IN BOTH DIRECTIONS. No boot log named the divergence,
// and `jobs.queue_ingest` is missing from no environment because it was never
// unset — so the SPEC table read as a statement of fact rather than as one of two
// competing defaults, and §G.3.1's invitation to re-derive `W` was an invitation
// to compute against a process nobody runs.
//
// This is the tie, so the two cannot drift apart again in silence.
func TestConfigDefaultsProduceTheSpecQueueWidths(t *testing.T) {
	t.Parallel()

	got := jobs.FromPlatformConfig(config.Default().Jobs).Queues
	want := jobs.DefaultQueueWorkers()

	if len(got) != len(want) {
		t.Fatalf("a default process runs %d queues and the SPEC table names %d", len(got), len(want))
	}
	for q, w := range want {
		if got[q] != w {
			t.Errorf("queue %q: a default process runs %d workers, SPEC §G.3 publishes %d.\n"+
				"Either config.Default() has started setting jobs.queue_* again — in which "+
				"case the published table describes no process anybody runs — or the table "+
				"moved without the config's fall-through following it.", q, got[q], w)
		}
	}
}

// TestTheSpecQueueWidthsAreTheNumbersSpecPublishes pins the map itself against the
// §G.3 table as written, so a change to one is a visible change to both. The
// literals are duplicated ON PURPOSE: a test that recomputed them from the same
// map it is checking would assert nothing.
//
// ⛔ `reconcile`'s 8 IS A SUPPORTED TENANT COUNT, NOT A THROUGHPUT PREFERENCE
// (SPEC §G.3.1). Lowering it lowers how many tenants the deployment supports, so
// it is the one row here that may not be nudged to match its neighbours.
func TestTheSpecQueueWidthsAreTheNumbersSpecPublishes(t *testing.T) {
	t.Parallel()

	spec := map[string]int{
		jobs.QueueIngest:         16,
		jobs.QueueEnrich:         8,
		jobs.QueueNotify:         8,
		jobs.QueueDeliverSlack:   4,
		jobs.QueueDeliverWebhook: 8,
		jobs.QueueReconcile:      8,
		jobs.QueueLifecycle:      4,
		jobs.QueueMaintenance:    1,
	}
	got := jobs.DefaultQueueWorkers()

	for q, w := range spec {
		if got[q] != w {
			t.Errorf("DefaultQueueWorkers has %q at %d, SPEC §G.3 publishes %d — "+
				"move the table and the map together, with the rationale", q, got[q], w)
		}
	}
	for q := range got {
		if _, ok := spec[q]; !ok {
			t.Errorf("DefaultQueueWorkers names queue %q, which SPEC §G.3 does not", q)
		}
	}
}
