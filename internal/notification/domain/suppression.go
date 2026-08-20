package domain

// SuppressedReason is why oto communicated nothing — the closed set of
// `notifications.suppressed_reason` (notifications_suppmap_ck, as widened by
// migration 00018, narrowed to SIX by migration 00059 and widened to SEVEN by
// migration 00073).
//
// This is OTO'S OWN notification-suppression vocabulary. It is NOT
// Alertmanager's `alert_cases.suppression_reason`, which mirrors the four
// upstream suppression reasons and nothing else. Conflating them would make oto
// report "Alertmanager is suppressing this" when the truth is "a human asked oto
// to be quiet" (§B.8.2).
//
// A suppressed Notification is a RECORDED FACT with a row, a reason and a place
// in the UI. It is never a silent drop: silent suppression is the single fastest
// way to destroy an operator's trust in an alerting tool (§B.6).
type SuppressedReason string

// The closed SuppressedReason set.
const (
	// SuppressedChannelDisabled: every destination the policy names is disabled
	// or deleted. Outranks everything because it explains the absence of a
	// destination, and no later test can even be evaluated without one.
	SuppressedChannelDisabled SuppressedReason = "channel_disabled"
	// SuppressedNoPolicy: nothing routed this fact anywhere.
	SuppressedNoPolicy SuppressedReason = "no_policy"
	// SuppressedSnoozed: a human asked oto to be quiet about this signal until a
	// fixed time (§B.8).
	SuppressedSnoozed SuppressedReason = "snoozed"
	// SuppressedThrottled: the policy's per-subject rate cap was reached.
	SuppressedThrottled SuppressedReason = "throttled"
	// SuppressedBelowThreshold: the policy's count condition (`count_min` over
	// `count_window_s`, migration 00072) is not met yet — fewer subjects of the
	// bound kind have been seen inside the sliding window than the operator asked
	// for.
	//
	// ⭐ IT IS `throttled`'S DUAL AND IT IS THE SAME POLICY COLUMNS WITH THE
	// OPPOSITE SENSE. `throttled` says "you have already been told this many times
	// in this window"; this says "this has not happened enough times in this window
	// yet". A policy may carry both and a single evaluation may be refused by
	// either.
	//
	// ⛔ IT IS NOT `flapping` COMING BACK, AND THE DIFFERENCE IS THE WHOLE REASON
	// THIS VALUE IS ADMISSIBLE. `flapping` and `storm` were deleted because they
	// were OTO'S OWN OPINION that a real firing was not worth mentioning, taken
	// against a threshold welded into Go (`DefaultFlapThreshold = 5` over
	// `DefaultFlapWindow = 7200 s`) that no operator could see or change. The
	// number here comes from a column an operator wrote, on a policy they can read
	// back, and the silence it produces is the one they asked for by name — which
	// is exactly the replacement git-bug `7570090` names for hardcoded flap
	// detection. Same shape of quiet; a different author.
	SuppressedBelowThreshold SuppressedReason = "below_threshold"
	// SuppressedVerbosity: every destination's verbosity dropped this fact.
	SuppressedVerbosity SuppressedReason = "verbosity"
	// SuppressedDuplicateRender: the rendered payload was byte-identical to what
	// the thread already shows, so the update would have been a no-op.
	SuppressedDuplicateRender SuppressedReason = "duplicate_render"
)

// suppressorOrder is the precedence chain: when several suppressors apply, the FIRST
// MATCH wins and is the one recorded.
//
// ⚠️ SIX OF THE SEVEN RANKS ARE SPEC §B.8.2's AND THE SEVENTH CAME FROM ADR 0044 §5,
// WHICH IS SAID PLAINLY BECAUSE BORROWED AUTHORITY IS WORSE THAN NONE. §B.8.2 as
// originally written fixed `channel_disabled → no_policy → snoozed → throttled →
// verbosity → duplicate_render` — six values, and `below_threshold` was not among
// them, because the value did not exist when that section was written. Its rank below
// was an IMPLEMENTATION DECISION MADE HERE AND IN MIGRATION 00073, and the owner
// ratified it on 2026-08-20 (ADR 0044 §5), which is where its authority now lives —
// not in this comment, and not in the §B.8.2 line that records it. The order of the
// other six is unchanged and is the spec's.
//
// The reasoning, the spec's for six and ADR 0044 §5's for the seventh:
//
//	channel_disabled  there is nowhere to send; nothing else can even be asked
//	no_policy         nothing routed it; still nowhere to send
//	snoozed           a DELIBERATE HUMAN ACT, and therefore the most actionable
//	                  explanation a reader can be given. It outranked every
//	                  automatic damper below it for that reason alone.
//	throttled         a policy rate cap — the CEILING was hit
//	below_threshold   a policy count condition — the FLOOR is not reached yet
//	verbosity         a per-destination volume preference
//	duplicate_render  nothing changed; the cheapest and least interesting answer
//
// ⭐ `below_threshold` SITS DIRECTLY BELOW `throttled`, RATIFIED BY ADR 0044 §5
// (owner, 2026-08-20). That ADR is the authority for this one rank; SPEC §B.8.2
// records it and this slice implements it. The order shipped as a PROPOSAL -- the
// question "does a ceiling outrank a floor" existed nowhere before this axis did --
// and the history is kept here because a reader who finds only the ruling cannot
// tell a considered answer from an inherited one. The argument, now ruled: the two
// are the same two policy columns read with opposite senses, so they belong adjacent
// and above `verbosity`, which is a property of a DESTINATION rather than of the
// policy. The ceiling is ranked
// first of the two because a spent cap is the ACTIVE fact — oto has been speaking
// about this conversation and stopped, against a number the operator has already
// been hit by — whereas an unmet floor is the ordinary RESTING state of every
// policy that carries one: for most of its window a count condition is unmet by
// design. A resting state that outranked an active damper would mask it on every
// policy carrying both, which is the same argument that puts `verbosity` below
// `throttled`.
//
// ⭐⭐ TWO RANKS WERE DELETED FROM BETWEEN `snoozed` AND `throttled`, AND THE CHAIN
// IS WORTH READING TWICE FOR WHAT IS LEFT. `storm` and `flapping` were the only
// values that were ever OTO'S OWN OPINION about a signal — oto judging a real
// firing not worth mentioning, which is the one suppression an operator cannot
// distinguish from a signal that never fired. Every value that remains is either
// the ABSENCE OF A DESTINATION (`channel_disabled`, `no_policy`), A HUMAN'S
// EXPLICIT REQUEST (`snoozed`, `verbosity`, `throttled`, `below_threshold`), or
// NOTHING TO SAY (`duplicate_render`). Not one of them is a judgement, and
// nothing may add one back.
//
// ⚠️ `below_threshold` JOINED THE SECOND GROUP AND NOT A NEW ONE, WHICH IS THE
// TEST IT HAD TO PASS. It produces the same silence `flapping` produced — "this
// keeps happening, say nothing yet" — so the only thing separating it from the
// value this file spent two paragraphs deleting is WHERE THE NUMBER COMES FROM. It
// comes from `notification_policies.count_min`, which an operator wrote, can read
// back and can delete. `flapping`'s came from a constant in Go. A damper whose
// threshold is the operator's is their request; one whose threshold is ours is our
// opinion, and that is the line this vocabulary is held to.
//
// ⛔ THEY ARE DELETED RATHER THAN RETIRED, WHICH IS A CHANGE OF MIND THIS FILE
// USED TO ARGUE THE OTHER WAY. The retirement bargain — keep the value declared so
// a DECODER meeting an older row can render it — only buys something when such a
// row can exist. Migration 00059 narrowed `notifications_suppmap_ck` to six and
// migration 00060 narrows `notifications_reason_ck` the same way, and neither
// performs an `UPDATE`: a database holding either value REFUSES the migration
// outright. The maintainer has answered that with `just reset` on the only
// database that exists, so there is no row left to decode and no binary left to
// have written one. A value kept for a reader that cannot exist is not caution, it
// is a vocabulary entry the next person has to rule out.
var suppressorOrder = []SuppressedReason{
	SuppressedChannelDisabled,
	SuppressedNoPolicy,
	SuppressedSnoozed,
	SuppressedThrottled,
	SuppressedBelowThreshold,
	SuppressedVerbosity,
	SuppressedDuplicateRender,
}

// SuppressorOrder returns the precedence chain — §B.8.2's six ranks plus the
// `below_threshold` rank this release proposes; see suppressorOrder. The slice is
// freshly built so the order cannot be mutated by a caller.
func SuppressorOrder() []SuppressedReason {
	out := make([]SuppressedReason, len(suppressorOrder))
	copy(out, suppressorOrder)
	return out
}

// Valid reports whether s is in the closed set.
func (s SuppressedReason) Valid() bool {
	for _, k := range suppressorOrder {
		if k == s {
			return true
		}
	}
	return false
}

// String renders the reason as stored.
func (s SuppressedReason) String() string { return string(s) }

// Rank is s's position in the precedence chain, lower first. An unknown reason
// sorts last rather than first: an unrecognised suppressor must never be allowed
// to mask a known one.
func (s SuppressedReason) Rank() int {
	for i, k := range suppressorOrder {
		if k == s {
			return i
		}
	}
	return len(suppressorOrder)
}

// Suppressors is the set of suppressors that applied to one evaluation.
//
// It is a SET rather than a first-writer-wins field on purpose: the evaluator
// discovers them in the order its data arrives, which is not the order §B.8.2
// requires, and a field would therefore record whichever one happened to be
// tested first.
type Suppressors struct {
	seen map[SuppressedReason]bool
}

// Add records that a suppressor applied. Adding an unknown reason is ignored
// rather than stored: it would fail notifications_suppmap_ck at insert time, and
// a constraint violation is a much worse way to learn about a typo than nothing
// happening.
func (s *Suppressors) Add(r SuppressedReason) {
	if !r.Valid() {
		return
	}
	if s.seen == nil {
		s.seen = make(map[SuppressedReason]bool, 4)
	}
	s.seen[r] = true
}

// AddIf records r when cond holds. It exists so an evaluator reads as a list of
// conditions rather than a ladder of ifs.
func (s *Suppressors) AddIf(cond bool, r SuppressedReason) {
	if cond {
		s.Add(r)
	}
}

// Any reports whether anything suppressed this evaluation.
func (s *Suppressors) Any() bool { return len(s.seen) > 0 }

// Has reports whether r is among the suppressors that applied.
func (s *Suppressors) Has(r SuppressedReason) bool { return s.seen[r] }

// Winner returns the reason to record: the FIRST MATCH in the §B.8.2 order.
// The second result is false when nothing suppressed this evaluation.
func (s *Suppressors) Winner() (SuppressedReason, bool) {
	for _, r := range suppressorOrder {
		if s.seen[r] {
			return r, true
		}
	}
	return "", false
}

// All returns every suppressor that applied, in precedence order. The winner is
// what the row records; the full set is what an operator asking "why was I not
// told?" deserves to see in the UI.
func (s *Suppressors) All() []SuppressedReason {
	if len(s.seen) == 0 {
		return nil
	}
	out := make([]SuppressedReason, 0, len(s.seen))
	for _, r := range suppressorOrder {
		if s.seen[r] {
			out = append(out, r)
		}
	}
	return out
}
