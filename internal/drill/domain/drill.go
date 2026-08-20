package domain

import (
	"time"

	"github.com/google/uuid"
)

// Deadline is how long a drill waits before it declares itself timed out.
//
// It is derived rather than picked. The chain a drill drives is four River jobs
// deep — `ingest.process_batch`, `enrich.run`, `notify.evaluate`,
// `deliver.dispatch` — and the only one with a deliberate delay is the
// pre-notification budget (`jobs.PreNotificationBudget`, 2 s). Everything else is
// a queue hop plus one Slack round trip. Ninety seconds is roughly an order of
// magnitude more than the happy path needs, which is the right slack for a
// diagnostic: long enough that a busy queue does not produce a false alarm, short
// enough that an operator waiting at a screen gets an answer rather than a
// spinner.
//
// ⛔ It is a DEADLINE, not a timeout on any call. Nothing is cancelled when it
// passes; the pipeline keeps going and may well succeed. What changes is only
// what the drill is willing to claim.
const Deadline = 90 * time.Second

// DisposeAfter is how long a settled drill's synthetic signal rows survive before
// `retention.prune` deletes them.
//
// ⭐ WHY NOT IMMEDIATELY. An operator who ran a drill, read the result and then
// wanted to click through to the alert row, the Case timeline or the delivery
// record would find nothing. A day is long enough for that and short enough that
// nobody's alert list ever has a drill in it — which it would not anyway, since
// every list excludes them.
//
// ⭐ WHY THIS DOES NOT CONTRADICT ADR 0024. That ADR promises the signal tables
// are never reaped, and the promise is about the record of an UPSTREAM event.
// A drill records none: nothing fired, no cluster was involved, oto manufactured
// every byte. Its rows are pruned BY ROW, joining `ingest_dedup`, `sessions` and
// `enrichment_cache` — the three side tables ADR 0024 already lists as pruned
// that way — and no partition is dropped. The drill's own receipt row survives,
// with its frozen outcome, exactly as ADR 0024 designed `retention_exports`.
const DisposeAfter = 24 * time.Hour

// Drill is one delivery drill (`delivery_drills`).
//
// It is DATA, in the mould of `alerts/domain.AlertUpsert`: its invariants live in
// the DDL and in the service that mints it, and duplicating them here would give
// two answers to one question. The interesting logic in this package is
// `BuildPayload` and `Observe`, both pure functions.
type Drill struct {
	ID       uuid.UUID
	SourceID uuid.UUID
	// Label is the `oto_drill` nonce. It equals ID.String() and is stored
	// separately because it is what every artefact query matches on.
	Label    string
	Severity string

	BatchID uuid.UUID
	// The disposal manifest, filled in as stages discover each artefact.
	//
	// ⛔ `GroupID uuid.UUID` WAS HERE AND IS DELETED (git-bug `7570090`). It named the
	// `alert_groups` row disposal had to delete and the subject the thread was keyed
	// by, and there is no such row. `CaseID` carries both jobs now: it is the
	// conversation, so it is what the thread and the notifications are addressed by,
	// and it needs no delete of its own because the `alerts` delete CASCADEs to it.
	//
	// ⚠️ `delivery_drills.group_id` STILL EXISTS AND NOTHING READS OR WRITES IT. It
	// has no FK (00039), so dropping `alert_groups` leaves it holding stale uuids
	// rather than failing; the column is another agent's migration to drop.
	AlertID        uuid.UUID
	CaseID         uuid.UUID
	NotificationID uuid.UUID

	// Outcome is the frozen staged result, present exactly when the drill has
	// settled. While it is nil, every read recomputes from the live rows.
	//
	// ⛔ THE VERDICT LIVES HERE AND NOWHERE ELSE ON THIS TYPE. `status` and
	// `failed_stage` are stored as their own columns because they are what an
	// operator greps a database for — but a second copy on the entity would be a
	// second answer to "did it pass", and the two would disagree the first time
	// somebody forgot to set one.
	Outcome *Result

	// StartedByLabel is past-tense attribution, frozen at write time so the
	// receipt stays readable after the user is gone. The user id behind it is
	// stored (ON DELETE SET NULL) and deliberately never read back: nothing in oto
	// derives a per-person metric, and a field nobody can read cannot start.
	StartedByLabel string

	StartedAt  time.Time
	DeadlineAt time.Time
	FinishedAt time.Time
	DisposedAt time.Time
}

// Settled reports whether the verdict is frozen.
func (d Drill) Settled() bool { return !d.FinishedAt.IsZero() }

// Disposed reports whether the synthetic signal rows have been deleted.
func (d Drill) Disposed() bool { return !d.DisposedAt.IsZero() }

// NewDrill is the repository input for minting one.
type NewDrill struct {
	ID             uuid.UUID
	SourceID       uuid.UUID
	Severity       string
	StartedBy      uuid.UUID
	StartedByLabel string
	StartedAt      time.Time
	DeadlineAt     time.Time
}
