package domain

// THE ORG-WIDE CONTROLS AND THE MINIMUM INTERVAL (ADR 0053 §6; git-bug bf172fe).
//
// §6 names three controls beyond the per-run budgets, and each one's breach is a
// different verb — which is the whole design of this file:
//
//	Daily token budget   org            → SKIPPED, on the record, never queued
//	Concurrency          org            → WAITS, queued; never dropped
//	Minimum interval     each Investigator, per subject → COALESCES into one run
//
// ⭐ "EVERY CONTROL IS A NUMBER AN OPERATOR CAN READ BACK, AND HITTING ONE IS RECORDED,
// NEVER SILENT." The budget and the concurrency are org settings with bounds and an
// origin (`identity/domain`); the interval is a column on the Investigator. A budget
// skip is a row with status `skipped` and reason `budget`; a wait is a row that stays
// `queued`; a coalesced trigger is the one queued run every trigger inside the interval
// resolves to, and a deferred one carries the time it may start (NotBefore).

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// OrgControls are the org's §6 controls as read back from its settings: the kill
// switch, the daily token budget and the concurrency. Built by `internal/app` from
// `identity`'s effective settings, which bound both numbers.
type OrgControls struct {
	// Enabled is `investigations_enabled`, the org's kill switch.
	Enabled bool
	// DailyTokens is `investigation_daily_tokens`: input + output tokens per UTC day.
	DailyTokens int64
	// Concurrency is `investigation_concurrency`: the most runs `running` at once.
	Concurrency int
}

// DayStart is the UTC midnight that began the day t is in: the moment the daily
// token budget last reset (§6: "Resets at UTC midnight").
func DayStart(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// BudgetSpent reports why a new run may not start because the day's budget is spent
// — a sentence for the run's reason_detail — or "" when there is budget left.
//
// ⭐ THE TEST IS "HAS THE DAY'S RECORDED SPEND REACHED THE BUDGET", NOT "WOULD THIS RUN
// FIT". A run's cost is unknown until it ends, so admitting on a projection would be a
// guess; what is known is every token every Step has already recorded today. A run
// already going is bounded by its own per-run token budget, so the day can overrun by
// at most what the runs in flight still had left — and the next run asked for is
// skipped, on the record.
func (c OrgControls) BudgetSpent(spentToday int64, now time.Time) string {
	if spentToday < c.DailyTokens {
		return ""
	}
	reset := DayStart(now).Add(24 * time.Hour)
	return fmt.Sprintf("this org has spent %d of its %d daily Investigation tokens (investigation_daily_tokens) since 00:00 UTC; "+
		"nothing new starts until it resets at %s", spentToday, c.DailyTokens, reset.Format(time.RFC3339))
}

// AtCapacity reports whether `running` runs fill the org's concurrency.
func (c OrgControls) AtCapacity(running int) bool { return running >= c.Concurrency }

// Bounds of an Investigator's minimum interval (`investigators_interval_ck`).
const (
	// MinIntervalSeconds is zero: an Investigator may run on every trigger. Zero is a
	// number an operator reads back, not a sentinel.
	MinIntervalSeconds = 0
	// MaxIntervalSeconds is a day: past it, "minimum interval" is a schedule, and a
	// subject whose membership keeps changing for a day deserves a human's look.
	MaxIntervalSeconds = 86400
	// DefaultIntervalSeconds is ten minutes: two `group_interval`s at Alertmanager's
	// shipped 5m, so one batch of membership churn is one follow-up run, not one per
	// notification Alertmanager sends about it.
	DefaultIntervalSeconds = 600
)

// NewMinInterval builds an Investigator's minimum interval between runs on one
// subject, inside `investigators_interval_ck`.
func NewMinInterval(seconds int) (time.Duration, error) {
	if seconds < MinIntervalSeconds || seconds > MaxIntervalSeconds {
		const msg = "0 to 86400 seconds"
		return 0, errs.Validation("investigator_interval_invalid",
			"an Investigator's minimum interval is outside the range oto will accept",
			errs.Violation{Field: "min_interval_seconds", Code: "out_of_range", Message: msg})
	}
	return time.Duration(seconds) * time.Second, nil
}

// DefaultMinInterval is the interval an Investigator gets when its writer names none.
func DefaultMinInterval() time.Duration { return DefaultIntervalSeconds * time.Second }

// Trigger is what asked for an Investigation (ADR 0053 §4).
type Trigger string

// The triggers the minimum interval distinguishes.
const (
	// TriggerHuman is "a human asks". ⭐ IT IS NOT HELD TO THE INTERVAL: §6 scopes
	// coalescing to membership-change triggers, and §4 lists a human's request as a
	// trigger of its own. A person who presses "ask again" gets a run — bounded by
	// the kill switch, the daily budget and the concurrency like every other.
	TriggerHuman Trigger = "human"
	// TriggerDrawn is "an Incident is drawn" (§4): the first look at a new story. It
	// happens once per Incident, so it is not held to the interval either — but a
	// redelivered trigger finds the run the first delivery made and resolves to it,
	// rather than paying for the same first look twice (git-bug 74ea849).
	TriggerDrawn Trigger = "drawn"
	// TriggerMembership is "its membership changes" (§4) — the automatic trigger the
	// interval exists for (§6: "Membership-change triggers inside the interval
	// coalesce into one run"). Raised by the Incident domain when a Case joins or
	// leaves (git-bug 74ea849). Going quiet is not a membership change and raises
	// nothing (§4: "Not on quiet").
	TriggerMembership Trigger = "membership"
)

// Automatic reports whether the trigger is one oto raised rather than a person.
func (t Trigger) Automatic() bool { return t != TriggerHuman }

// CoveredByIncident reports whether a trigger on a CASE is answered by the Incident
// the Case is in rather than by a run of its own (ADR 0053 §4: "A Case already in an
// Incident gets no Investigation of its own automatically — the Incident's covers
// it"). `holding` is the Incident the Case is a current member of, or uuid.Nil.
//
// ⭐ A HUMAN IS NEVER COVERED. "Automatically" is the ADR's own word: a person who
// asks about one Case of a storm gets a run about that Case.
func CoveredByIncident(trigger Trigger, holding uuid.UUID) bool {
	return trigger.Automatic() && holding != uuid.Nil
}

// SubjectRuns is what the interval reads about one (Investigator, subject): the run
// waiting to start, if any, and the latest run that did not skip.
type SubjectRuns struct {
	// Queued is the latest `queued` run, or nil.
	Queued *Investigation
	// Last is the latest run that is not `queued` and not `skipped` — the last one
	// that began (or ended trying to), or nil. A skipped run never ran, so it opens no
	// interval.
	Last *Investigation
}

// Admission is what a trigger becomes: onto an existing run, or a new one that may
// start now or not before a time.
type Admission struct {
	// Onto is the run this trigger resolved to — the queued one it coalesced into, or
	// for a redelivered draw the one already made — or uuid.Nil for a new run.
	Onto uuid.UUID
	// NotBefore is when a new run may start; zero for "now".
	NotBefore time.Time
}

// Coalesced reports whether the trigger resolved to an existing run.
func (a Admission) Coalesced() bool { return a.Onto != uuid.Nil }

// Admit decides what a trigger becomes under an Investigator's minimum interval.
//
//   - A human's request is a new run, now. (See TriggerHuman.)
//   - A draw resolves to the run already made for this subject — queued or begun —
//     and is otherwise a new run, now. (See TriggerDrawn.)
//   - A membership change while a run of the same Investigator on the same subject is
//     still `queued` coalesces into it: that run has not started, so it will read the
//     subject as it stands when it does — the change is already in what it will see.
//   - Otherwise, inside the interval since the last run began, it is a new run that
//     may not start before the interval elapses. Every further change before then
//     finds THAT run queued and coalesces into it, so a burst of churn is one
//     follow-up, run once the interval is up — never dropped, never one per change.
//   - Past the interval, or with none, it is a new run, now.
//
// The anchor is when the last run STARTED, not when it was asked for: a run that
// waited on the concurrency for five minutes saw the subject as it was five minutes
// later, and the interval is between the things the runs saw.
func Admit(trigger Trigger, interval time.Duration, runs SubjectRuns, now time.Time) Admission {
	switch trigger {
	case TriggerMembership:
	case TriggerDrawn:
		switch {
		case runs.Queued != nil:
			return Admission{Onto: runs.Queued.ID}
		case runs.Last != nil:
			return Admission{Onto: runs.Last.ID}
		default:
			return Admission{}
		}
	default:
		return Admission{}
	}
	if runs.Queued != nil {
		return Admission{Onto: runs.Queued.ID}
	}
	if interval <= 0 || runs.Last == nil {
		return Admission{}
	}
	anchor := runs.Last.StartedAt
	if anchor.IsZero() {
		anchor = runs.Last.RequestedAt
	}
	if next := anchor.Add(interval); now.Before(next) {
		return Admission{NotBefore: next.UTC()}
	}
	return Admission{}
}

// StartOutcome is what an attempt to start a queued run came to under the org's
// concurrency.
type StartOutcome int

// The three outcomes of a start.
const (
	// StartBegan: the run moved from `queued` to `running`.
	StartBegan StartOutcome = iota + 1
	// StartNotQueued: it was not queued — another worker took it, or it ended.
	StartNotQueued
	// StartAtCapacity: the org already has its concurrency's worth running, so it
	// stays `queued` and its job waits (§6: "Waits in the job queue; never dropped").
	StartAtCapacity
)
