package domain

import "time"

// Damping is SPEC §B.6 — oto's automatic defences against its own noise.
//
// ⭐ THE RULE THAT GOVERNS EVERY LINE IN THIS FILE:
//
//	Flapping and storm mode are VISIBLE UI STATES, NEVER SILENT SUPPRESSION.
//
// Damping never hides an alert, never changes its state, never changes its
// severity and never stops an AlertEvent being written. Occurrences still open
// and close normally; the ONLY thing that changes is how loudly oto talks about
// them, and the fact that it went quieter is itself displayed and recorded on the
// timeline. Silence destroys trust, so oto's quiet is always accounted for.
//
// Storm collapse is the GROUP-level damper and lives here. Flap damping is the
// ALERT-level damper: `alerts.flap_score` is an EWMA of state transitions per
// hour, it is scored by the `flap.score` job in the alerts module, and it is a
// DERIVED SIGNAL — never a state (§B.1).

// The §D.1 storm defaults. An org may tune them; it may not turn the visibility
// off, because there is nothing to turn off — the state is the message.
const (
	// DefaultStormThreshold is how many DISTINCT alerts must join one generation
	// inside the window before it collapses to a single message.
	DefaultStormThreshold = 25
	// DefaultStormWindow is the window those joins are counted over.
	DefaultStormWindow = 60 * time.Second
	// DefaultStormCooldown is how long a generation must go WITHOUT a new member
	// before storm mode ends.
	DefaultStormCooldown = 10 * time.Minute
	// DefaultGroupCloseDelay is how long a generation with no live member stays
	// open before it closes and freezes its thread.
	DefaultGroupCloseDelay = 5 * time.Minute
)

// StormPolicy is the tuning of storm collapse for one org.
type StormPolicy struct {
	Threshold int
	Window    time.Duration
	Cooldown  time.Duration
	// CloseDelay is group_close_delay_s. It lives here because it is the same
	// kind of number — a clock the group lifecycle reads — and putting it
	// anywhere else means two places to look.
	CloseDelay time.Duration
}

// DefaultStormPolicy is §D.1's defaults. Storm collapse is ON BY DEFAULT: the
// product's promise is to be quiet by default and unmistakable when it matters,
// and a 400-alert node failure that posts 400 messages is neither.
func DefaultStormPolicy() StormPolicy {
	return StormPolicy{
		Threshold:  DefaultStormThreshold,
		Window:     DefaultStormWindow,
		Cooldown:   DefaultStormCooldown,
		CloseDelay: DefaultGroupCloseDelay,
	}
}

// Normalise fills any zero field from the defaults, so a partially-configured org
// can never produce a zero threshold and collapse every group on its first
// member.
func (p StormPolicy) Normalise() StormPolicy {
	d := DefaultStormPolicy()
	if p.Threshold <= 0 {
		p.Threshold = d.Threshold
	}
	if p.Window <= 0 {
		p.Window = d.Window
	}
	if p.Cooldown <= 0 {
		p.Cooldown = d.Cooldown
	}
	if p.CloseDelay <= 0 {
		p.CloseDelay = d.CloseDelay
	}
	return p
}

// StormAction is what a storm evaluation decided.
type StormAction int

const (
	// StormUnchanged means the generation stays as it is.
	StormUnchanged StormAction = iota
	// StormStart means the generation enters storm mode and one
	// `group.storm_started` event is appended.
	StormStart
	// StormEnd means the cooldown elapsed with no new member and one
	// `group.storm_ended` event is appended.
	StormEnd
)

// StormDecision is the outcome of one evaluation, carrying the evidence that
// justified it so the event payload can explain itself to a human.
type StormDecision struct {
	Action StormAction
	// DistinctJoins is how many distinct alerts joined inside the window. It is
	// the number the UI shows next to "storm".
	DistinctJoins int
	// Since is when storm mode began, for a start; the existing storm_since for
	// an end.
	Since time.Time
	// Threshold and Window are echoed so the timeline records the policy that was
	// in force, not the policy that happens to be configured when somebody reads
	// it later.
	Threshold int
	Window    time.Duration
}

// EvaluateStorm decides whether a generation enters or leaves storm mode (§B.6).
//
//	More than `threshold` DISTINCT alerts joining one generation inside
//	`window` collapses the group: it posts and updates exactly ONE root
//	message with a count and a link, and suppresses per-alert thread replies.
//	Storm mode ends after `cooldown` without a new member.
//
// It counts DISTINCT ALERTS, not observations: one flapping alert re-firing forty
// times in a minute is a flap and is damped by flap_score, while forty different
// alerts arriving in a minute is a storm. Conflating the two would collapse a
// group because ONE alert was noisy, hiding thirty-nine quiet ones.
//
// The instant is a PARAMETER and there is no clock in this package.
func EvaluateStorm(g Group, distinctJoins int, lastJoinAt, now time.Time, p StormPolicy) StormDecision {
	p = p.Normalise()
	now = now.UTC()
	d := StormDecision{
		Action:        StormUnchanged,
		DistinctJoins: distinctJoins,
		Threshold:     p.Threshold,
		Window:        p.Window,
		Since:         g.StormSince(),
	}

	if !g.IsOpen() {
		// A closed generation has no thread to collapse and no members to come.
		if g.StormMode() {
			d.Action = StormEnd
		}
		return d
	}

	if !g.StormMode() {
		if distinctJoins > p.Threshold {
			d.Action = StormStart
			d.Since = now
		}
		return d
	}

	// Storm mode ends after the cooldown WITHOUT a new member — not after a fixed
	// duration. A storm that is still growing is still a storm.
	reference := lastJoinAt
	if reference.IsZero() {
		reference = g.StormSince()
	}
	if !reference.IsZero() && !now.Before(reference.UTC().Add(p.Cooldown)) {
		d.Action = StormEnd
	}
	return d
}

// ApplyStorm folds a decision onto the generation.
//
// Entering or leaving storm mode is a MATERIAL change: it moves `state_version`,
// which is hashed into notification.idempotency_key (§C.7), so the message that
// announces the storm is a new intent rather than a duplicate of the last one.
func ApplyStorm(g Group, d StormDecision) (Group, bool) {
	switch d.Action {
	case StormStart:
		if g.stormMode {
			return g, false
		}
		next := g
		next.stormMode = true
		next.stormSince = d.Since.UTC()
		next.stateVersion = g.stateVersion + 1
		return next, true
	case StormEnd:
		if !g.stormMode {
			return g, false
		}
		next := g
		next.stormMode = false
		next.stormSince = time.Time{}
		next.stateVersion = g.stateVersion + 1
		return next, true
	case StormUnchanged:
		return g, false
	default:
		return g, false
	}
}
