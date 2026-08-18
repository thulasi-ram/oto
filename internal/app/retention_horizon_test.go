package app

import (
	"testing"
	"time"

	alertsdomain "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/tuning"
)

// TestTheDedupeKeyHorizonIsAFloorThatOnlyWidens pins the rule `retention.prune`
// sweeps `alert_event_keys` by, and it is the whole of the decision this job
// takes.
//
// ⭐⭐ THE TWO DIRECTIONS ARE TWO DIFFERENT SILENT FAILURES, and each half of the
// fold exists to stop one of them:
//
//   - TOO SHORT AND ACCEPTANCE CRITERION 36 BREAKS WITHOUT A SOUND. A stored
//     `ingest_batch` replays idempotently only while its event keys are still
//     claimed. `partitions.manage` keeps every raw partition to the LONGEST
//     window any tenant asked for (ADR 0024), so an org on 365 days holds
//     replayable payloads for a year — and if its keys went at 30 days, every
//     replay of an 11-month-old batch would append the timeline a second time and
//     report success, because zero rows written is documented as the idempotency
//     mechanism working.
//   - AND YET IT MAY NEVER GO BELOW 30. The reachable way down is NOT
//     `raw_retention_days`: `effectiveRetention` seeds the fold with
//     `Config.Retention.RawPayloads` and only ever widens it, so a per-org setting
//     of 1 day never reaches this fold at all. What reaches it is a deployment
//     setting `OTO_RETENTION_RAW_PAYLOADS` below 720h — that knob has no lower
//     bound but `validate:"gt=0"`. An operator who wants a day of payloads on the
//     whole install must not thereby unclaim the keys of episodes that are still
//     open, whose transitions the reconciler re-applies and which nothing but this
//     table dedupes. Thirty days is the number the schema comment, SPEC §D.4 and
//     `tuning.DefaultRawRetention` have all named since 00007.
func TestTheDedupeKeyHorizonIsAFloorThatOnlyWidens(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		rawDays int
		want    time.Duration
		why     string
	}{
		{"a deployment configured under the floor cannot shrink the horizon", 1, alertsdomain.DedupeKeyRetention,
			"an install keeping one day of payloads still writes keys for episodes that stay open " +
				"far longer, and the reconciler re-applies their transitions"},
		{"a deployment on the shipped default", 30, 31 * 24 * time.Hour,
			"the shipped raw retention IS this horizon plus the daily partition grain: a day-D " +
				"payload partition is dropped only once D+31d has passed, so its keys must reach " +
				"there too"},
		{"a tenant that keeps payloads longer widens it", 365, 366 * 24 * time.Hour,
			"a payload that is still replayable must still have its keys, or its replay appends " +
				"the timeline twice and says nothing"},
		{"an unreadable settings fold cannot narrow it", 0, alertsdomain.DedupeKeyRetention,
			"every failure in effectiveRetention widens the window; a zero arriving here must " +
				"keep a key rather than delete one"},
	} {
		if got := dedupeKeyHorizonOf(tc.rawDays); got != tc.want {
			t.Errorf("%s: dedupeKeyHorizonOf(%d) = %s, want %s — %s",
				tc.name, tc.rawDays, got, tc.want, tc.why)
		}
	}
}

// TestAKeyOutlivesThePartitionHoldingItsPayload is the acceptance criterion 36
// case the arithmetic above exists to serve, stated as the two jobs' clocks
// rather than as a duration.
//
// ⭐⭐ THE TWO SWEEPS ROUND THE SAME WINDOW IN OPPOSITE DIRECTIONS, and that is
// the whole bug this pins. `oto_drop_partitions_before` (00005_partitions.sql)
// keeps a DAILY partition until its WHOLE range is past `now - rawDays` — it
// skips any child whose upper bound is still in the future — so a payload that
// landed at ANY time on day D is readable until D+rawDays+1 00:00. The key sweep
// has no grain: it deletes at exactly `created_at + horizon`. A horizon of
// `rawDays` days therefore kills the key first, by up to a full day, and in that
// window a batch left `partial` is still offered by the failed-batch feed while
// the rows that make its replay idempotent are gone. Replaying it appends every
// event a second time, fires duplicate Slack, and reports success.
//
// So: for every arrival instant within a partition, the key must still be claimed
// at the moment that partition is finally dropped.
func TestAKeyOutlivesThePartitionHoldingItsPayload(t *testing.T) {
	t.Parallel()

	// Day D, and the instant `partitions.manage` may first drop it: the child
	// covers [D, D+1) and is dropped only once D+1 <= now-rawDays.
	dayD := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	for _, rawDays := range []int{1, 30, 90, 365} {
		horizon := dedupeKeyHorizonOf(rawDays)
		partitionDroppedAt := dayD.AddDate(0, 0, rawDays+1)

		// Every arrival inside day D, including the two edges and the 00:05
		// batch from the ticket.
		for _, arrival := range []time.Duration{
			0, 5 * time.Minute, 12 * time.Hour, 24*time.Hour - time.Nanosecond,
		} {
			createdAt := dayD.Add(arrival)
			keySweptAt := createdAt.Add(horizon)
			if keySweptAt.Before(partitionDroppedAt) {
				t.Errorf("raw_retention_days=%d: a batch received %s is still readable until %s "+
					"(its day-D partition is dropped only then), but its dedupe key is swept at %s "+
					"— %s earlier. Replaying that batch in the gap appends the timeline a SECOND "+
					"time and reports success, which is exactly the SPEC acceptance criterion 36 "+
					"hazard the pruner must not open. The horizon must cover the partition grain",
					rawDays, createdAt, partitionDroppedAt, keySweptAt,
					partitionDroppedAt.Sub(keySweptAt))
			}
		}
	}
}

// TestTheShippedKeyHorizonCoversTheShippedRawWindow pins the ONE direction that
// still has to hold: keys outlive the bytes they guard.
//
// ⛔ NEITHER NUMBER DERIVES THE OTHER, AND THAT IS THE CORRECTION. `oto replay`
// gates on supersession — `supersededBy` in `ingestion/service/replay.go` takes no
// age argument — so `tuning.DefaultRawRetention` is 30 days as a CHOSEN number
// (ADR 0024, Amendment 4: the depth of the rejections and failed-batch feeds,
// which take no time window, plus the window a replay can be attempted in at all,
// at a measured 51 MB per 1 000 daily firings), and `DedupeKeyRetention` is 30
// days for the reconciler, which re-applies transitions that nothing but
// `alert_event_keys` dedupes. They are two independent numbers that happen to be
// equal.
//
// What this test forbids is the ORDERING going wrong: a raw window wider than the
// key horizon would leave a readable payload whose dedupe keys had already been
// swept, and replaying it appends the timeline a second time while reporting
// success. `dedupeKeyHorizonOf` widens with the raw window, so at the shipped
// defaults the equality is exactly what puts the horizon at its floor — move
// either constant and re-derive the other from the two reasons above rather than
// from each other.
func TestTheShippedKeyHorizonCoversTheShippedRawWindow(t *testing.T) {
	t.Parallel()

	if tuning.DefaultRawRetention != alertsdomain.DedupeKeyRetention {
		t.Fatalf("DefaultRawRetention is %s and DedupeKeyRetention is %s — the shipped raw window "+
			"must never exceed the key horizon, or a payload that is still readable has lost the "+
			"keys that make its replay idempotent (ADR 0024, Amendment 4). They are independent "+
			"numbers that are equal today: neither is derived from the other, so changing one is a "+
			"decision about the other, not a consequence of it",
			tuning.DefaultRawRetention, alertsdomain.DedupeKeyRetention)
	}
}
