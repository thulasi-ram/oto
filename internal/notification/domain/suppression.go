package domain

// SuppressedReason is why oto communicated nothing — the closed set of
// `notifications.suppressed_reason` (notifications_suppmap_ck, as widened by
// migration 00018 and narrowed to SIX by migration 00059).
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
	// SuppressedVerbosity: every destination's verbosity dropped this fact.
	SuppressedVerbosity SuppressedReason = "verbosity"
	// SuppressedDuplicateRender: the rendered payload was byte-identical to what
	// the thread already shows, so the update would have been a no-op.
	SuppressedDuplicateRender SuppressedReason = "duplicate_render"
)

// suppressorOrder is the §B.8.2 precedence chain, BINDING AND IN THIS ORDER.
//
// When several suppressors apply, the FIRST MATCH wins and is the one recorded.
// The order is not arbitrary and is not a performance decision:
//
//	channel_disabled  there is nowhere to send; nothing else can even be asked
//	no_policy         nothing routed it; still nowhere to send
//	snoozed           a DELIBERATE HUMAN ACT, and therefore the most actionable
//	                  explanation a reader can be given. It outranked every
//	                  automatic damper below it for that reason alone.
//	throttled         a policy rate cap
//	verbosity         a per-destination volume preference
//	duplicate_render  nothing changed; the cheapest and least interesting answer
//
// ⭐⭐ TWO RANKS WERE DELETED FROM BETWEEN `snoozed` AND `throttled`, AND THE CHAIN
// IS WORTH READING TWICE FOR WHAT IS LEFT. `storm` and `flapping` were the only
// values that were ever OTO'S OWN OPINION about a signal — oto judging a real
// firing not worth mentioning, which is the one suppression an operator cannot
// distinguish from a signal that never fired. Every value that remains is either
// the ABSENCE OF A DESTINATION (`channel_disabled`, `no_policy`), A HUMAN'S
// EXPLICIT REQUEST (`snoozed`, `verbosity`), THE WORLD'S CONSTRAINT (`throttled`),
// or NOTHING TO SAY (`duplicate_render`). Not one of them is a judgement, and
// nothing may add one back.
//
// ⛔ THEY ARE DELETED RATHER THAN RETIRED, WHICH IS A CHANGE OF MIND THIS FILE
// USED TO ARGUE THE OTHER WAY. The retirement bargain — keep the value declared so
// a DECODER meeting an older row can render it — only buys something when such a
// row can exist. Migration 00059 narrowed `notifications_suppmap_ck` to six and
// migration 00060 narrows `notifications_reason_ck` the same way, and neither
// performs an `UPDATE`: a database holding either value REFUSES the migration
// outright. The maintainer has answered that with `just db-reset` on the only
// database that exists, so there is no row left to decode and no binary left to
// have written one. A value kept for a reader that cannot exist is not caution, it
// is a vocabulary entry the next person has to rule out.
var suppressorOrder = []SuppressedReason{
	SuppressedChannelDisabled,
	SuppressedNoPolicy,
	SuppressedSnoozed,
	SuppressedThrottled,
	SuppressedVerbosity,
	SuppressedDuplicateRender,
}

// SuppressorOrder returns the §B.8.2 precedence chain. The slice is freshly
// built so the order cannot be mutated by a caller.
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
