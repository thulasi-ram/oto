package domain

// THE CORRELATOR (ADR 0052 §2, §4; git-bug 61eeddf): an operator-written
// definition that draws Incidents — matchers over Cases, optionally a count over a
// window — so that "why is this Case in this Incident?" always has an answer
// somebody can read back. It is the machine author; the human is the other one,
// and a model is not a third (§2).
//
// ⛔ IT IS NOT A "RULE". In oto a rule is the Prometheus alerting rule
// (CONTEXT.md §3), and a Correlator is configuration an operator writes in oto.
//
// ⭐ ITS PREDICATE IS A NOTIFICATION POLICY'S, NOT A LOOKALIKE. `Matchers` is the
// kernel's `[]Matcher` — the grammar, the anchoring and the regex cache a policy
// evaluates with (`alerts/domain/matcher.go`, moved there for this reader) — and
// `Count` is 00072's count condition with 00072's bounds. `Priority` orders the
// walk as a policy's does: LOWER FIRST, the first whose matchers hold claims the
// Case, and no later one is consulted.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Bounds mirroring migration 00085's CHECKs, the request DTO tags and the
// contract — R9's three copies. Each is the notification policy's number for the
// same column, because each IS the same column.
const (
	// MaxCorrelatorNameLength is `correlators_name_ck`.
	MaxCorrelatorNameLength = 120
	// MaxCorrelatorMatchers is `correlators_matchers_ck` (`policies_matchers_ck`).
	MaxCorrelatorMatchers = 32
	// MinCorrelatorPriority and MaxCorrelatorPriority are `correlators_prio_ck`.
	// LOWER IS EVALUATED FIRST.
	MinCorrelatorPriority = 0
	// MaxCorrelatorPriority is the upper bound of `correlators_prio_ck`.
	MaxCorrelatorPriority = 10000
	// DefaultCorrelatorPriority is the column default, the policy's 100.
	DefaultCorrelatorPriority = 100

	// MinCountThreshold IS TWO for 00072's reason: the Case being evaluated is
	// itself inside the window, so a threshold of one is cleared by every Case and
	// states no condition — that Correlator is spelled by leaving the count off.
	MinCountThreshold = 2
	// MaxCountThreshold is `correlators_count_min_ck`.
	MaxCountThreshold = 10000
	// MinCountWindow is the throttle's floor, as it is for a policy's count.
	MinCountWindow = time.Minute
	// MaxCountWindow is one day, as it is for every span oto lets an operator hold.
	MaxCountWindow = 24 * time.Hour

	// MinQuietGrace and MaxQuietGrace are `correlators_quiet_grace_ck` (migration
	// 00086): the count window's bounds. Under a minute is "only while active"
	// with extra steps; over a day is not a story anybody would recognise.
	MinQuietGrace = time.Minute
	// MaxQuietGrace is the upper bound of `correlators_quiet_grace_ck`.
	MaxQuietGrace = 24 * time.Hour
)

// Count is a Correlator's floor on DRAWING: "draw only once at least Min Cases I
// claimed opened inside Window".
//
// ⭐ IT IS 00072's `CountOverWindow`, SPELLED HERE BECAUSE THIS PACKAGE MAY NOT
// IMPORT THAT ONE (domain-must-be-pure). Same two fields, same bounds, same rule
// that both halves are required, same sliding lookback re-derived from the instant
// of the thing being evaluated — here the Case's `started_at`, so a job that runs
// late counts the window the Case actually opened in, not the one the worker woke
// up in.
//
// ⛔ IT GATES A DRAW AND NEVER A JOIN. A Case matching while this Correlator's
// Incident is active joins it whatever the count (§4): the count answers "is this
// a story yet?", and once the story exists that question is settled.
type Count struct {
	// Min is `count_min`. Zero means no condition: every matching Case draws or
	// joins.
	Min int
	// Window is `count_window_s`.
	Window time.Duration
}

// Enabled reports whether this condition constrains anything. Both halves are
// required (`correlators_count_pair_ck`), `Throttle.Enabled`'s rule.
func (c Count) Enabled() bool { return c.Min > 0 && c.Window > 0 }

// Clears reports whether n Cases in the window are enough to draw. A disabled
// condition clears everything — a Correlator with no count draws on its first
// match — which is `CountOverWindow.Clears`' direction for the same reason: the
// default must never be "nothing ever happens".
func (c Count) Clears(n int) bool {
	if !c.Enabled() {
		return true
	}
	return n >= c.Min
}

// Correlator is one operator-written definition, as stored.
type Correlator struct {
	ID       uuid.UUID
	Name     string
	Priority int
	Enabled  bool
	// Matchers are ANDed; an empty list matches every Case.
	Matchers []kernel.Matcher
	Count    Count
	// QuietGrace is `quiet_grace_s` (ADR 0052 §4, migration 00086): how long after
	// this Correlator's latest Incident went quiet a matching Case still joins it.
	// Zero joins only while it is active.
	QuietGrace time.Duration
	// Conversations is `incidents_are_conversations` (ADR 0052 §6, migration
	// 00087): the operator says the Incidents this Correlator draws are
	// CONVERSATIONS, so a fact about a member Case evaluated after its membership
	// committed posts into the Incident's thread instead of the Case's own.
	//
	// ⭐ IT IS READ AT DELIVERY, NEVER STAMPED AT DRAW. Nothing about an Incident
	// records that it "became" a conversation: the notification layer asks, for
	// each fact as it evaluates it, whether the Case is in an Incident whose
	// Correlator says so. Turning it on redirects later facts; turning it off sends
	// later facts back to each Case's own thread; neither moves anything already
	// posted.
	Conversations bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Joins reports whether a Case of this Correlator that opened at startedAt joins
// the Correlator's latest Incident rather than drawing a new one (ADR 0052 §4).
//
// ⭐ WHILE ACTIVE, ALWAYS; WHILE QUIET, ONLY INSIDE THE GRACE. The grace is
// measured from the instant the Incident went quiet to the instant the new Case
// OPENED — not to when the job ran — so a late worker cannot turn a re-fire inside
// the grace into a new story. The boundary is inclusive: a re-fire exactly
// `quiet_grace_s` after quiet still joins.
//
// ⛔ A FLAPPING ALERT IS WHY THIS EXISTS. Without a grace, a disk alert that fires
// every twenty minutes is quiet between firings, and every firing draws a fresh
// Incident — each declared outbound as its own external incident.
func (c Correlator) Joins(latest CorrelatorIncident, startedAt time.Time) bool {
	if latest.State() == StateActive {
		return true
	}
	if c.QuietGrace <= 0 || latest.QuietSince.IsZero() {
		return false
	}
	return startedAt.Sub(latest.QuietSince) <= c.QuietGrace
}

// Matches reports whether every matcher holds against a Case's labels.
func (c Correlator) Matches(labels map[string]string) (bool, error) {
	return kernel.MatchAll(c.Matchers, labels)
}

// Validate enforces every bound the DDL enforces, as field-level violations so an
// operator meets the sentence rather than a 23514.
func (c Correlator) Validate() error {
	var v []errs.Violation

	if n := len(strings.TrimSpace(c.Name)); n < 1 || n > MaxCorrelatorNameLength {
		v = append(v, errs.Violation{
			Field: "name", Code: "length",
			Message: fmt.Sprintf("a Correlator name is 1 to %d characters", MaxCorrelatorNameLength),
		})
	}
	if c.Priority < MinCorrelatorPriority || c.Priority > MaxCorrelatorPriority {
		v = append(v, errs.Violation{
			Field: "priority", Code: "range",
			Message: "priority is 0 to 10000, lower evaluated first",
		})
	}
	if len(c.Matchers) > MaxCorrelatorMatchers {
		v = append(v, errs.Violation{
			Field: "matchers", Code: "max_items",
			Message: fmt.Sprintf("at most %d matchers", MaxCorrelatorMatchers),
		})
	}
	for _, m := range c.Matchers {
		if err := m.Validate(); err != nil {
			v = append(v, errs.ViolationsOf(err)...)
		}
	}
	v = append(v, c.Count.violations()...)
	if c.QuietGrace != 0 &&
		(c.QuietGrace < MinQuietGrace || c.QuietGrace > MaxQuietGrace || c.QuietGrace%time.Second != 0) {
		v = append(v, errs.Violation{
			Field: "quiet_grace_seconds", Code: "range",
			Message: "quiet_grace_seconds is 60 to 86400, or absent to join only while active",
		})
	}

	if len(v) > 0 {
		return errs.Validation("correlator_invalid", "the Correlator is not valid", v...)
	}
	return nil
}

// violations restates `correlators_count_*_ck`. The fields are the wire's names.
func (c Count) violations() []errs.Violation {
	var v []errs.Violation
	if (c.Min > 0) != (c.Window > 0) {
		v = append(v, errs.Violation{
			Field: "count_min", Code: "incomplete",
			Message: "a count condition needs both count_min and count_window_seconds, or neither",
		})
		return v
	}
	if !c.Enabled() {
		return nil
	}
	if c.Min < MinCountThreshold || c.Min > MaxCountThreshold {
		v = append(v, errs.Violation{
			Field: "count_min", Code: "range",
			Message: fmt.Sprintf("count_min is %d to %d: one would be cleared by every Case", MinCountThreshold, MaxCountThreshold),
		})
	}
	if c.Window < MinCountWindow || c.Window > MaxCountWindow || c.Window%time.Second != 0 {
		v = append(v, errs.Violation{
			Field: "count_window_seconds", Code: "range",
			Message: "count_window_seconds is 60 to 86400",
		})
	}
	return v
}

// CorrelatorDraft is a Correlator being created: everything an operator writes,
// with the two defaulted columns optional.
type CorrelatorDraft struct {
	Name       string
	Priority   *int
	Enabled    *bool
	Matchers   []kernel.Matcher
	Count      Count
	QuietGrace time.Duration
	// Conversations defaults to false: N alerts, N conversations (ADR 0045) until
	// an operator says otherwise, by name.
	Conversations bool
}

// Correlator is the draft as the row it would become, for validation.
func (d CorrelatorDraft) Correlator() Correlator {
	c := Correlator{
		Name: d.Name, Priority: DefaultCorrelatorPriority, Enabled: true,
		Matchers: d.Matchers, Count: d.Count, QuietGrace: d.QuietGrace,
		Conversations: d.Conversations,
	}
	if d.Priority != nil {
		c.Priority = *d.Priority
	}
	if d.Enabled != nil {
		c.Enabled = *d.Enabled
	}
	return c
}

// CorrelatorPatch is a partial update. A nil field is left alone; `Count` set to
// a pointer at the zero Count CLEARS the condition.
type CorrelatorPatch struct {
	Name     *string
	Priority *int
	Enabled  *bool
	Matchers *[]kernel.Matcher
	Count    *Count
	// QuietGrace set to a pointer at zero CLEARS the grace.
	QuietGrace *time.Duration
	// Conversations turns ADR 0052 §6 on or off for this Correlator's Incidents.
	// It redirects only facts evaluated after it commits.
	Conversations *bool
}

// IsEmpty reports whether the patch changes nothing.
func (p CorrelatorPatch) IsEmpty() bool {
	return p.Name == nil && p.Priority == nil && p.Enabled == nil && p.Matchers == nil &&
		p.Count == nil && p.QuietGrace == nil && p.Conversations == nil
}

// Apply returns c with the patch merged in. It is what validation runs against:
// a patch is only valid in the context of the row it lands on.
func (p CorrelatorPatch) Apply(c Correlator) Correlator {
	if p.Name != nil {
		c.Name = *p.Name
	}
	if p.Priority != nil {
		c.Priority = *p.Priority
	}
	if p.Enabled != nil {
		c.Enabled = *p.Enabled
	}
	if p.Matchers != nil {
		c.Matchers = *p.Matchers
	}
	if p.Count != nil {
		c.Count = *p.Count
	}
	if p.QuietGrace != nil {
		c.QuietGrace = *p.QuietGrace
	}
	if p.Conversations != nil {
		c.Conversations = *p.Conversations
	}
	return c
}

// ByCorrelator builds the attribution of a Correlator's decision (§2). The id is
// the whole answer to "why"; the name is read from the Correlator when a sentence
// needs it, because the row stores only the id.
func ByCorrelator(id uuid.UUID) Attribution {
	return Attribution{correlatorID: id}
}

// CorrelatorNotFound is the one answer for a Correlator id this org does not have,
// including a deleted one and another org's.
func CorrelatorNotFound() error {
	return errs.NotFound("correlator_not_found", "no such Correlator")
}

// CorrelationCase is what the evaluator needs to know about the Case it is
// deciding: who it is, when it opened, what it is labelled, and whether a human
// has already decided where it belongs.
type CorrelationCase struct {
	CaseRef
	StartedAt time.Time
	Labels    map[string]string
	// InIncident is set when the Case is a CURRENT member of any Incident: every
	// Correlator skips it (§4, at most one Incident per Case).
	InIncident bool
	// EverRemoved is set when a human has ever removed or moved the Case out of an
	// Incident: no Correlator may put it back (§4), and the tombstone is how that is
	// known.
	EverRemoved bool
	// Synthetic is set for a delivery drill's Case. A drill proves a notification
	// arrives; it must never draw an Incident, which would be declared outbound as
	// a real one.
	Synthetic bool
}

// Claimable reports whether a Correlator may act on this Case at all.
func (c CorrelationCase) Claimable() bool { return !c.InIncident && !c.EverRemoved && !c.Synthetic }

// CorrelatorIncident is a Correlator's most recent Incident as the evaluator reads
// it: which one, and how many of its current members are open — the count its
// derived state is read off (ADR 0052 §3).
type CorrelatorIncident struct {
	Ref
	OpenMembers int
	// QuietSince is when the Incident went quiet, READ rather than stored (ADR 0052
	// §3): the latest `ended_at` among its current members, or the latest removal
	// if a human's removal is what quieted it. Meaningful only while quiet.
	QuietSince time.Time
}

// State is the Incident's derived state.
func (i CorrelatorIncident) State() State { return StateOf(i.OpenMembers) }

// CaseAt is a claimed Case with the instant it opened — the unit a count condition
// counts and the coordinate its window slides along.
type CaseAt struct {
	CaseRef
	StartedAt time.Time
}

// Span answers a count condition for one Case: is there a window of length
// c.Window, CONTAINING the anchor, that holds at least c.Min Cases (the anchor
// included)? If so it returns the Cases of the fullest such window, in start order
// — the Cases an Incident is drawn over — and true.
//
// ⭐⭐ THE WINDOW SLIDES AROUND THE ANCHOR, NOT BACK FROM IT, AND THAT IS WHAT MAKES
// THE ANSWER INDEPENDENT OF JOB ORDER. 00072's policy count looks back from the
// fact being evaluated, which is right for a notification because each fact is
// judged on its own. Here five Cases opening inside one second are five
// `incidents.correlate` jobs on four workers, and the queue does not promise they
// run in start order: a lookback `[start - W, start]` judged on the Case that
// STARTED last but RAN first sees nothing, and the Case that ran last sees every
// Case but the one that started after it — so "≥5 within 600 s" would wait for a
// sixth that may never come. Asking "is there any W-long span through this Case
// holding Min of them" gives the same verdict whichever job runs last, which is the
// one that finds the others already recorded.
//
// others are the candidates the caller read from `[anchor - W, anchor + W]`; any
// span containing the anchor lies inside it. Two pointers over the sorted starts,
// so a storm's thousand candidates cost a thousand steps, not a million.
func (c Count) Span(anchor CaseAt, others []CaseAt) ([]CaseAt, bool) {
	if !c.Enabled() {
		return []CaseAt{anchor}, true
	}
	all := make([]CaseAt, 0, len(others)+1)
	all = append(all, others...)
	all = append(all, anchor)
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].StartedAt.Equal(all[j].StartedAt) {
			return all[i].StartedAt.Before(all[j].StartedAt)
		}
		return all[i].ID.String() < all[j].ID.String()
	})

	bestLo, bestHi := -1, -1
	hi := 0
	for lo := range all {
		start := all[lo].StartedAt
		if start.After(anchor.StartedAt) {
			break // every later window starts after the anchor and cannot contain it
		}
		if anchor.StartedAt.Sub(start) > c.Window {
			continue // a window opening here closes before the anchor
		}
		if hi < lo {
			hi = lo
		}
		for hi+1 < len(all) && !all[hi+1].StartedAt.After(start.Add(c.Window)) {
			hi++
		}
		if bestLo < 0 || hi-lo > bestHi-bestLo {
			bestLo, bestHi = lo, hi
		}
	}
	if bestLo < 0 || !c.Clears(bestHi-bestLo+1) {
		return nil, false
	}
	return all[bestLo : bestHi+1], true
}
