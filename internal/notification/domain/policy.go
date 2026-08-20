package domain

import (
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// ⛔ BINDING (SCOPE-BOUNDARY §5.3, SPEC §G.9.1). A NotificationPolicy routes a
// FACT to a DESTINATION. It MUST NEVER gain `user_ids`, `team_ids`,
// `schedule_id`, `rotation`, `time_of_day`, `days_of_week`, `timezone`, or a
// second reminder stage.
//
// A policy that routes to a PERSON is a rota, and a rota is how oto stops being
// a flight recorder and starts being an on-call product (FR-1, H-1). The
// structural guarantee is this file: `ChannelIDs []uuid.UUID` references
// `channels` and there is no other target field, so the violation cannot be
// expressed without editing this struct — which is exactly the review moment the
// doctrine wants.

// Bounds mirrored from notification_policies' CHECK constraints. They are
// validated in Go as well as in the database so that a bad policy comes back as
// a field-level validation error rather than as a 23514 an operator must decode.
const (
	// MaxPolicyMatchers is policies_matchers_ck.
	MaxPolicyMatchers = 32
	// MaxPolicyReasons is policies_reasons_ck, and it is 17 because `reasons` is a
	// SET over the 17-value Reason enum.
	//
	// ⚠️ IT IS NOT A NUMBER TO BE CHOSEN. It is `len(AllReasons())`, asserted as
	// such by `TestTheReasonCeilingIsTheSizeOfTheReasonEnum`. It moved from 18 to 19
	// when migration 00058 added `digest`, back to 18 when 00060 deleted `storm`,
	// and to 17 when 00067 deleted `unacked_reminder` — the number follows the
	// vocabulary in both directions. The DDL ceiling and the DTO `max` tag carry the
	// same number for the same reason.
	//
	// It used to be 32 while the contract said 18, and that gap was not a wire
	// bound differing from a storage bound on purpose: it was the room duplicates
	// occupied. `uniqueItems: true` made a 19th element unreachable on the wire
	// while the column would happily hold `fired` thirty-two times, so 32 was the
	// only number that described what could actually be stored.
	//
	// Migration 00046 closed that — the CHECK now refuses a repeated element, and
	// Validate below refuses one too — so an array of DISTINCT values drawn from the
	// closed vocabulary cannot exceed its size, and a ceiling of 32 would be a number
	// no row could ever test. All three layers say the same number and all three say
	// set, which is what CONTEXT.md §5b asks of a bound.
	//
	// ⛔ IT WAS 17 AND IS NOW 15 (git-bug `7570090`, migration `00069`, which narrows
	// `policies_reasons_ck` to match). `new_alerts` and `some_resolved` left the
	// vocabulary: both assert a plurality and a conversation holds one Case. The
	// ceiling tracks the vocabulary BY CONSTRUCTION, so it moves whenever the
	// vocabulary does — which is the whole reason all three layers must agree.
	MaxPolicyReasons = 15
	// MaxPolicyChannels is policies_chan_ck.
	MaxPolicyChannels = 16
	// MinPolicyPriority and MaxPolicyPriority are policies_prio_ck. LOWER IS
	// EVALUATED FIRST.
	MinPolicyPriority = 0
	// MaxPolicyPriority is the upper bound of policies_prio_ck.
	MaxPolicyPriority = 10000
	// MaxPolicyNameLength is policies_name_ck.
	MaxPolicyNameLength = 120
)

// Reminder bounds from policies_reminder_ck (SPEC §G.9.1).
const (
	// MinUnackedReminderAfter is the floor: below a minute the reminder is noise.
	MinUnackedReminderAfter = 60 * time.Second
	// MaxUnackedReminderAfter is the ceiling: a day.
	MaxUnackedReminderAfter = 24 * time.Hour
)

// MatchOp is a label matcher operator, mirroring Alertmanager's four.
type MatchOp string

// The four operators.
const (
	// OpEqual is `=`.
	OpEqual MatchOp = "="
	// OpNotEqual is `!=`.
	OpNotEqual MatchOp = "!="
	// OpMatch is `=~`, a FULLY ANCHORED regular expression, as in Alertmanager.
	OpMatch MatchOp = "=~"
	// OpNotMatch is `!~`, the anchored negation.
	OpNotMatch MatchOp = "!~"
)

// Valid reports whether op is one of the four.
func (op MatchOp) Valid() bool {
	switch op {
	case OpEqual, OpNotEqual, OpMatch, OpNotMatch:
		return true
	default:
		return false
	}
}

// IsRegex reports whether op compiles its value.
func (op MatchOp) IsRegex() bool { return op == OpMatch || op == OpNotMatch }

// Matcher is one label predicate: `{"name":"severity","op":"=","value":"critical"}`.
//
// It matches LABELS ONLY. There is no matcher on a time of day, on a weekday, or
// on who is on call, and there never will be: a policy whose outcome depends on
// WHEN it is evaluated is a schedule (SCOPE-BOUNDARY §4.8).
type Matcher struct {
	Name  string
	Op    MatchOp
	Value string
}

// regexCache memoises anchored matcher regexes.
//
// Policy evaluation happens on every lifecycle transition, and the same handful
// of matchers are recompiled thousands of times an hour otherwise. The cache is
// bounded and dropped wholesale when it grows past the bound: a policy set large
// enough to overflow it is already pathological, and an unbounded cache keyed by
// user-supplied strings is a memory leak with a nicer name.
var (
	regexCacheMu sync.RWMutex
	regexCache   = map[string]*regexp.Regexp{}
)

const regexCacheMax = 1024

// anchoredRegex compiles value with Alertmanager's full-anchor semantics: `=~`
// means the WHOLE label value matches, never a substring. Getting this wrong
// makes `severity=~"crit"` silently match `critical-but-ignorable`.
func anchoredRegex(value string) (*regexp.Regexp, error) {
	regexCacheMu.RLock()
	re, ok := regexCache[value]
	regexCacheMu.RUnlock()
	if ok {
		return re, nil
	}

	re, err := regexp.Compile("^(?:" + value + ")$")
	if err != nil {
		return nil, errs.Validation("policy_matcher_regex",
			"a matcher regular expression did not compile",
			errs.Violation{Field: "matchers.value", Code: "regex", Message: err.Error()})
	}

	regexCacheMu.Lock()
	if len(regexCache) >= regexCacheMax {
		regexCache = map[string]*regexp.Regexp{}
	}
	regexCache[value] = re
	regexCacheMu.Unlock()

	return re, nil
}

// Validate checks one matcher.
func (m Matcher) Validate() error {
	switch {
	case strings.TrimSpace(m.Name) == "":
		return errs.Validation("policy_matcher_name", "a matcher needs a label name",
			errs.Violation{Field: "matchers.name", Code: "required", Message: "a label name is required"})
	case !m.Op.Valid():
		return errs.Validation("policy_matcher_op", "a matcher operator must be one of = != =~ !~",
			errs.Violation{Field: "matchers.op", Code: "enum", Message: "unsupported operator"})
	}
	if m.Op.IsRegex() {
		if _, err := anchoredRegex(m.Value); err != nil {
			return err
		}
	}
	return nil
}

// Matches evaluates the matcher against a label set.
//
// A MISSING LABEL IS AN EMPTY STRING, which is Alertmanager's rule and the only
// one that makes `!=` behave sanely: `team != "payments"` must be true for an
// alert that carries no `team` label at all.
func (m Matcher) Matches(labels map[string]string) (bool, error) {
	got := labels[m.Name]

	switch m.Op {
	case OpEqual:
		return got == m.Value, nil
	case OpNotEqual:
		return got != m.Value, nil
	case OpMatch, OpNotMatch:
		re, err := anchoredRegex(m.Value)
		if err != nil {
			return false, err
		}
		hit := re.MatchString(got)
		if m.Op == OpNotMatch {
			return !hit, nil
		}
		return hit, nil
	default:
		return false, errs.Validation("policy_matcher_op", "a matcher operator must be one of = != =~ !~",
			errs.Violation{Field: "matchers.op", Code: "enum", Message: "unsupported operator"})
	}
}

// Throttle is a policy's per-subject rate cap: at most Max notifications for one
// subject inside Window.
//
// Hitting it produces a SUPPRESSED NOTIFICATION with `suppressed_reason =
// throttled`, which is a visible UI state. It never silently drops.
type Throttle struct {
	Max    int
	Window time.Duration
}

// Enabled reports whether this throttle constrains anything. Both halves are
// required: a cap with no window, or a window with no cap, is a configuration
// mistake that must not silently mute a channel.
func (t Throttle) Enabled() bool { return t.Max > 0 && t.Window > 0 }

// ⭐⭐ THE COUNT CONDITION LIVES HERE, BESIDE `Throttle`, BECAUSE IT IS `Throttle`
// AND NOT `digest_floor` (git-bug `7570090`, stage 6, migration `00072`).
//
// It was declared in digest.go under a comment calling it "`digest_floor`
// generalised — the special case it was all along", and that framing was wrong on
// the only axis that matters: WHAT THE COMPARISON DOES. SPEC §H.6 says the digest
// floor "is NOT a damper and suppresses nothing" — a policy carrying a window sends
// its digest IN ADDITION to everything else it routes, and a window that does not
// clear its floor had nothing to send in the first place. This one SUPPRESSES A FACT
// OTO ALREADY HAS, and records `below_threshold` when it does. A suppressor is not
// the child of a thing that suppresses nothing.
//
// Its own comment enumerated four axes and every one of them names the throttle:
//
//   - the window is SLIDING, not tiled, and re-derived per evaluation, exactly as
//     `Throttle.Window` is;
//   - `DigestWindowAligned` is deliberately unconsulted, because nothing is
//     REPORTED about the span — the digest's divisor rule exists so two pods can
//     name the same span, and no such agreement is needed here;
//   - `Enabled` requires BOTH halves, which is `Throttle.Enabled`'s rule and the
//     opposite of `Digest.Enabled`'s;
//   - `Clears` of an unconfigured condition is TRUE, which is the opposite of
//     `Digest.Clears(0)`.
//
// What the two floors do share is one arithmetic — "did enough happen inside a span"
// — and that is a resemblance rather than a lineage. `Digest.Clears` is fourteen
// lines away in digest.go and neither calls the other.

// Count-over-window bounds, mirrored from policies_count_min_ck and
// policies_count_window_ck. Validated in Go as well as in the database so that a
// bad condition comes back as a field-level violation rather than as a 23514.
const (
	// MinCountThreshold IS TWO, AND ONE IS EXCLUDED FOR THE REASON ZERO IS
	// EXCLUDED FROM `digest_floor`: it would describe a behaviour that does not
	// exist.
	//
	// The fact being evaluated is itself inside the window — it just happened — so
	// a threshold of one is cleared by every fact unconditionally and states no
	// condition at all. An operator who writes `count_min: 1` has asked for
	// today's behaviour in a spelling that looks like a damper, which is the
	// `refire_grace_s` defect (a knob that clamps and validates while changing no
	// outcome) reintroduced one release after it was deleted.
	MinCountThreshold = 2
	// MaxCountThreshold is the ceiling policies_count_min_ck enforces. It is
	// MaxDigestFloor, because it counts the same objects over the same kind of
	// span and two different ceilings on one arithmetic would be a number to
	// reconcile rather than a bound to honour.
	MaxCountThreshold = MaxDigestFloor

	// MinCountWindow is ONE MINUTE, WHICH IS THE THROTTLE'S FLOOR AND NOT THE
	// DIGEST'S. `policies_throttle_ck`'s window is 60..86400 and
	// `policies_digest_window_ck`'s is 300..86400, and the difference is not
	// arbitrary: a digest window shorter than five minutes is the per-event stream
	// it exists to replace wearing a delay, whereas a count condition sends the
	// event itself and its window only decides how far back the counting reaches.
	// "five of these in the last ninety seconds" is a question somebody genuinely
	// asks about a crash loop.
	MinCountWindow = time.Minute
	// MaxCountWindow is one day, as it is for every other span a policy may hold.
	MaxCountWindow = 24 * time.Hour
)

// CountOverWindow is a policy's floor on its own recent history: "stay silent
// until at least Min facts about my bound subject kind have happened inside
// Window".
//
// ⭐ IT IS `Throttle`'S DUAL, AND THE TWO ARE THE SAME TWO FIELDS. A throttle is a
// CEILING — at most Max inside Window, and exceeding it suppresses. This is a
// FLOOR — at least Min inside Window, and falling short of it suppresses. Neither
// is expressible as the other and an operator wants both for the same alert:
// "tell me once the pod has restarted five times this hour, and then no more than
// twice an hour after that" is one policy with a floor and a ceiling, not two
// policies.
//
// ⛔⛔ THE UNIT IS THE CASE AND ONLY THE CASE, WHICH IS A NARROWING OF WHAT 00072
// ADVERTISED AND IS WRITTEN DOWN HERE BECAUSE THE ALTERNATIVE WAS A PERMANENT MUTE.
// `policies_count_subject_ck` requires the condition to name exactly one
// `subject_kind`, and `policies_count_case_ck` now requires that kind to be `case`.
// The two rejected bindings failed for different reasons:
//
//   - `{alert}` COUNTED THE WRONG THING AND ALWAYS WILL. The numerator is
//     `count(DISTINCT subject_id)`, and an alert-subject row's `subject_id` is the
//     alert IDENTITY — one value, stable across every firing it ever has. So the
//     count of an alert re-firing five times is ONE, the caller's `+1` makes it
//     one, and "tell me once the pod has restarted five times this hour" — the
//     very sentence migration 00072 advertises — could never be met by the facts
//     it describes. What that binding actually counted was OTHER alert identities
//     the same policy had recorded facts about, which is a question nobody asked.
//     Counting occurrences instead would mean counting oto's own rows about one
//     identity (`count(*)` over acked, enriched, snoozed, …), which is a
//     throttle's numerator wearing a floor's clothes.
//   - `{digest}` READ THE KNOB WITH NOTHING. A digest is minted by the tick in
//     `notification/service/digest.go`, which evaluates `digest_floor` over its own
//     tiled window and never reaches `Evaluate`'s suppressors — so `count_min` on a
//     digest-bound policy decided nothing at all, which is exactly the
//     `refire_grace_s` defect migration 00071 deleted two settings for.
//
// `{case}` is the binding the advertised behaviour actually needs and the only one
// where the arithmetic is honest: five firings of one alert are five Cases, five
// distinct `subject_id`s, and a count of five.
//
// ⛔ ITS WINDOW IS A SLIDING LOOKBACK AND NOT A TILED ONE, WHICH IS WHY THERE IS
// NO ALIGNMENT RULE HERE. `Digest.Window` tiles the UTC day so that every boundary
// is a wall-clock boundary and no two pods disagree about which window a fact fell
// in — a digest is a REPORT ABOUT A SPAN, so the span has to be an object both
// pods can name. This window is `[TakenAt - Window, TakenAt]`, re-derived at every
// evaluation from the instant of the fact being evaluated, exactly as the
// throttle's is. Nothing is reported about the span, so nothing needs to agree on
// where it starts, and `DigestWindowAligned` is deliberately NOT consulted: a
// divisor rule here would refuse ninety seconds for a reason that does not apply.
//
// ⛔ AND IT IS NOT A SCHEDULE. The window says how far back to COUNT. It can never
// become a predicate on the wall clock — no timezone, no time of day, no weekday
// (SCOPE-BOUNDARY §4.8), which is the rule the binding block at the top of this
// file states for every clock a policy holds.
type CountOverWindow struct {
	// Min is `count_min`: how many facts must have happened. Zero means this
	// policy carries no count condition, which is what every row written before
	// migration 00072 says and what every policy that does not ask for one
	// continues to say.
	Min int
	// Window is `count_window_s`: how far back the counting reaches from the
	// instant of the fact being evaluated.
	Window time.Duration
}

// Enabled reports whether this condition constrains anything.
//
// ⚠️ BOTH HALVES ARE REQUIRED, AND THE RULE IS `Throttle.Enabled`'S RATHER THAN
// `Digest.Enabled`'S. A digest's floor is optional because its WINDOW alone is a
// complete instruction — "summarise every ten minutes" sends whenever the window
// was not empty. Neither half of a count condition means anything alone: a floor
// with no span is a threshold over unbounded history, and a span with no floor
// counts something and then does nothing with the number. `policies_count_pair_ck`
// refuses a row carrying one, and this method refuses to act on one that somehow
// exists.
func (c CountOverWindow) Enabled() bool { return c.Min > 0 && c.Window > 0 }

// Clears reports whether a count is enough for this policy to speak.
//
// ⛔ A DISABLED CONDITION CLEARS EVERYTHING, WHICH IS THE OPPOSITE OF
// `Digest.Clears` AND IS DELIBERATE. `Digest.Clears(0)` is false because an empty
// window has nothing to summarise, so silence withholds nothing. Here the fact
// exists and is waiting to be sent, so the question is "is there a reason NOT to
// send it" — and a policy with no count condition has no such reason. Returning
// false for the unconfigured case would make the default state total silence,
// which is the `no_policy` suppression this whole axis has to avoid becoming.
//
// It therefore needs no `Enabled` guard at the call site, which is the shape that
// keeps the caller from getting the default backwards.
func (c CountOverWindow) Clears(count int) bool {
	if !c.Enabled() {
		return true
	}
	return count >= c.Min
}

// MaxPolicySubjectKinds is policies_subjkinds_ck, and it is 3 because
// `subject_kinds` is a SET over the three-value SubjectKind vocabulary.
//
// ⚠️ IT IS NOT A NUMBER TO BE CHOSEN, exactly as MaxPolicyReasons is not: it is
// `len(AllSubjectKinds())`, asserted as such by
// `TestTheSubjectKindCeilingIsTheSizeOfTheSubjectKindEnum`. It was 4 for the
// duration this column did not exist — `alert_group` was a kind until migration
// `00069` deleted it — and it will move again in either direction the vocabulary
// does. All three layers (this constant, the DTO `max` tag, the contract's
// `maxItems`) carry the same number for the same reason (CONTEXT.md §5b).
//
// ⭐ THE DDL DOES NOT SPELL IT AND DOES NOT NEED TO. `policies_subjkinds_ck`
// enforces containment in the vocabulary plus set-ness, and a SET drawn from a
// three-value vocabulary cannot exceed three by construction — so a cardinality
// arm would be a number no row could ever test, which is the defect 00046 removed
// from `policies_reasons_ck` when it stopped saying 32.
const MaxPolicySubjectKinds = 3

// SubjectBinding is WHICH ALTITUDE A POLICY IS ABOUT — the subset of
// `notifications.subject_kind` it claims, as `notification_policies.subject_kinds`
// (migration `00072`, git-bug `7570090` done-when 8).
//
// EMPTY MEANS EVERY KIND, and that is the shipped default and the state of every
// row written before 00072. A policy that declares nothing behaves exactly as it
// did, which is what makes this axis monotone: the only new outcomes are
// narrowings an operator asked for by name.
//
// ⭐⭐ WHAT IT ADDS THAT `reasons` DOES NOT, STATED PLAINLY BECAUSE THE OVERLAP IS
// REAL AND HIDING IT WOULD BE WORSE. `Reason.Subject()` is TOTAL, so as a routing
// filter this is derivable: `subject_kinds: [case]` selects exactly the Reasons a
// hand-narrowed `reasons` list could have selected. The binding is not here to add
// reachable routings. It is here for two things a `reasons` list cannot do:
//
//   - IT IS THE COUNT'S UNIT. `CountOverWindow` (declared beside `Throttle` above)
//     is a floor on how many facts happened, and a count is meaningless without a
//     unit —
//     `policies_count_subject_ck` therefore requires a count condition to name
//     EXACTLY ONE kind. `digest_floor` counts Cases because migration 00058 chose
//     Cases and wrote the choice into a comment; this is that choice becoming a
//     column. Summing an alert-subject fact and a case-subject fact into one
//     number would be adding identities to episodes.
//   - IT IS A DECLARATION AN OPERATOR CAN READ. Deriving "this policy is about
//     firing episodes" from a fifteen-element Reason list requires knowing
//     `reasonSubjects` by heart. `subject_kinds: [case]` says it once, and the
//     coherence check in `validateSubjects` then refuses the combination that would
//     otherwise be silent — a binding that admits none of the declared Reasons,
//     which routes nothing and mints a `no_policy` suppression per fact forever.
//
// ⛔ IT IS AN AXIS ON THE EXISTING GATE AND NOT A SECOND GATE. `Handles` is where
// it is consulted, because "is this Reason mine" and "is this altitude mine" are
// one question asked of one policy at one moment. Adding a parallel `Binds` call
// beside every `Handles` call in the service would have been a second mechanism
// with a second set of call sites to forget — which is exactly what done-when 8
// refuses.
type SubjectBinding []SubjectKind

// Unrestricted reports whether this binding narrows nothing — the default.
func (b SubjectBinding) Unrestricted() bool { return len(b) == 0 }

// Binds reports whether k is an altitude this policy claims.
//
// ⚠️ AN EMPTY BINDING BINDS EVERYTHING, AND THE DIRECTION MATTERS MORE THAN IT
// LOOKS. Getting it backwards makes an unconfigured policy claim nothing, and the
// failure mode on this path is a `no_policy` SUPPRESSION — a filter that silently
// deletes notifications rather than an error anybody sees. Stage 1 of this ticket
// (`d76ee0d`) was landed under the same rule for the same reason.
func (b SubjectBinding) Binds(k SubjectKind) bool {
	if b.Unrestricted() {
		return true
	}
	for _, have := range b {
		if have == k {
			return true
		}
	}
	return false
}

// Sole returns the one kind this binding names, and false if it names none or
// several.
//
// It is `policies_count_subject_ck` in Go: a count condition needs a unit, and a
// binding of two kinds supplies two. The empty binding returns false rather than
// picking a default, because "every kind" is precisely the answer that has no unit.
func (b SubjectBinding) Sole() (SubjectKind, bool) {
	if len(b) != 1 {
		return "", false
	}
	return b[0], true
}

// Policy is one routing rule: which facts, matching which labels, go to which
// Channels.
type Policy struct {
	ID    uuid.UUID
	OrgID uuid.UUID
	Name  string
	// Priority orders evaluation, LOWER FIRST. The first policy that matches
	// wins and no other policy is consulted — which is why `notifications` carries
	// a single `policy_id` rather than a join table.
	Priority int
	Enabled  bool

	Matchers []Matcher
	// Reasons is which §H.6 Reason values this policy reacts to.
	Reasons []Reason
	// ChannelIDs is the fan-out, 1..16 destinations. It references `channels` and
	// NOTHING ELSE (see the binding block at the top of this file).
	ChannelIDs []uuid.UUID
	Throttle   Throttle

	// Subjects is `subject_kinds`: which altitude of fact this policy is about
	// (migration `00072`). The zero value — an empty binding — is every kind, which
	// is what every policy written before 00072 says. See SubjectBinding.
	Subjects SubjectBinding

	// Count is `count_min` and `count_window_s`: stay silent until at least this
	// many facts about my bound subject kind have happened inside this window. The
	// zero value means no condition, which is the shipped default.
	//
	// ⭐ IT IS THE FLOOR TO `Throttle`'S CEILING AND THE SAME TWO FIELDS. A policy
	// may carry both, and the pair is one sentence an operator writes once: "tell
	// me once this has happened five times in an hour, then no more than twice an
	// hour after that." `CountOverWindow` is declared beside `Throttle` above, and
	// the header there records why it is not `digest_floor`'s child.
	//
	// ⛔ IT REQUIRES `Subjects` TO BE EXACTLY `{case}` (`policies_count_case_ck`).
	// A count needs a unit, and the other two bindings are a permanent mute and an
	// inert knob respectively — see CountOverWindow.
	//
	// ⛔ ITS WINDOW IS NOT A SCHEDULE either, and the binding block at the top of
	// this file governs it as it governs `Digest`.
	Count CountOverWindow

	// ⛔ `UnackedReminderAfter` WAS HERE AND IS DELETED (git-bug bd0fb1d, migration
	// 00068). It was oto's one reminder stage — how long a signal could go
	// unacknowledged before a single `unacked_reminder` was sent to this policy's
	// own channels. The owner withdrew the feature: oto sends nothing unprompted.
	//
	// The field carried a BINDING refusal — one stage, forever, never a slice, never
	// a target other than ChannelIDs (SPEC §G.9.1) — and that refusal does not go
	// away with it. It gets stronger: there is now no reminder stage at all, so
	// there is no scalar for a second element to be appended to. Re-adding one is
	// re-adding the feature, and needs an ADR that argues against FR-1 by name.

	// Digest is `digest_window_s` and `digest_floor`: summarise what matched me over
	// a window, and stay silent unless enough happened. The zero value means no
	// digest, which is what every policy written before migration 00058 says and
	// what every policy that does not ask for one continues to say.
	//
	// ⛔ IT IS NOT A SCHEDULE OF WHEN OTO MAY SPEAK (see digest.go's binding block
	// and the one at the top of this file). The window selects which FACTS a summary
	// covers; it is read by the digest tick and by nothing on the evaluation path.
	// An alert-based or case-based policy gains no window: its facts fire
	// immediately, every time, because their noise is a signal to fix the Prometheus
	// rule and oto does not decide to be quiet about a firing.
	Digest Digest

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// Live reports whether this policy participates in evaluation.
func (p Policy) Live() bool { return p.Enabled && p.DeletedAt == nil }

// Handles reports whether this policy reacts to r.
//
// ⭐ IT ASKS TWO QUESTIONS AND HAS DONE SINCE MIGRATION `00072`: is this Reason in
// my list, and is the ALTITUDE it is about one I claim. The subject-kind binding is
// folded in here rather than checked beside every call because that is the whole of
// what done-when 8's "further axes on the existing machinery, not a second
// mechanism" asks for — there are four `Handles` call sites and a parallel gate
// would have meant remembering all four, forever, including the ones added later.
//
// ⚠️ THE ONE COST, RECORDED RATHER THAN DISCOVERED. `PolicyService.Preview`
// reports the verdict `reason_not_handled` when this returns false, so a policy
// refused by its BINDING is explained as if its `reasons` list were the problem.
// The routing outcome is right and the sentence is coarse. Splitting them needs a
// `subject_not_bound` verdict, which is a new value in a published contract enum
// and a change in `notification/service` — out of this stage's scope, and worth
// less than shipping the axis.
func (p Policy) Handles(r Reason) bool {
	if !p.Subjects.Binds(r.Subject()) {
		return false
	}
	for _, k := range p.Reasons {
		if k == r {
			return true
		}
	}
	return false
}

// Digests reports whether this policy sends a periodic digest — a window AND the
// Reason that routes it.
//
// ⚠️ BOTH HALVES, AND THE SECOND ONE IS NOT A FORMALITY. `policies_digest_reason_ck`
// makes the window imply the Reason in the database, so a stored policy cannot
// disagree; this method asks anyway, because a Policy value can also be a
// PREVIEW candidate that was never stored, and a digest minted for a policy whose
// `reasons` omit `digest` would be recorded and immediately suppressed as
// `no_policy`, once per window, forever. `firstMatchingPolicy` in reminder.go
// checks the same coherence for the same reason.
func (p Policy) Digests() bool { return p.Digest.Enabled() && p.Handles(ReasonDigest) }

// Matches reports whether every matcher holds against the group's labels.
//
// Matchers are ANDed. There is no OR and no nesting: a policy an operator cannot
// read at 3am is a policy that will route the wrong alert to the wrong channel.
func (p Policy) Matches(labels map[string]string) (bool, error) {
	for _, m := range p.Matchers {
		ok, err := m.Matches(labels)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// Validate enforces every bound the DDL enforces, plus the closed vocabularies.
func (p Policy) Validate() error {
	var v []errs.Violation

	if n := len(strings.TrimSpace(p.Name)); n < 1 || n > MaxPolicyNameLength {
		v = append(v, errs.Violation{
			Field: "name", Code: "length",
			Message: "a policy name is 1 to 120 characters",
		})
	}
	if p.Priority < MinPolicyPriority || p.Priority > MaxPolicyPriority {
		v = append(v, errs.Violation{
			Field: "priority", Code: "range",
			Message: "priority is 0 to 10000, lower evaluated first",
		})
	}
	if len(p.Matchers) > MaxPolicyMatchers {
		v = append(v, errs.Violation{
			Field: "matchers", Code: "max_items",
			Message: "at most 32 matchers",
		})
	}
	for i, m := range p.Matchers {
		if err := m.Validate(); err != nil {
			v = append(v, errs.ViolationsOf(err)...)
			_ = i
		}
	}

	switch {
	case len(p.Reasons) < 1:
		v = append(v, errs.Violation{
			Field: "reasons", Code: "required",
			Message: "a policy must react to at least one reason",
		})
	case len(p.Reasons) > MaxPolicyReasons:
		v = append(v, errs.Violation{
			Field: "reasons", Code: "max_items", Message: "at most 18 reasons",
		})
	}
	// ⭐ `reasons` IS A SET, and this loop is the layer that says so where it
	// counts. The `unique` tag on both request DTOs used to be the only place the
	// rule existed, so a duplicate was refused for an HTTP body and storable by
	// anything else — while the contract publishes `uniqueItems: true` on the
	// RESPONSE, which makes a stored duplicate a row oto's own generated client
	// rejects when it is read back. The code is `duplicate_items` because that is
	// what layer 1's `unique` tag emits (platform/validate), and one rule that
	// answers with two codes depending on which layer caught it is the drift §5b
	// exists to prevent. Reported once per repeated value: a list of six `fired`s
	// is one mistake, not five.
	seen := make(map[Reason]bool, len(p.Reasons))
	duplicated := make(map[Reason]bool)
	for _, r := range p.Reasons {
		if !r.Valid() {
			v = append(v, errs.Violation{
				Field: "reasons", Code: "enum",
				Message: "unknown notification reason " + string(r),
			})
		}
		if seen[r] && !duplicated[r] {
			duplicated[r] = true
			v = append(v, errs.Violation{
				Field: "reasons", Code: "duplicate_items",
				Message: "reasons must not contain duplicates: " + string(r) + " is listed twice",
			})
		}
		seen[r] = true
	}

	switch {
	case len(p.ChannelIDs) < 1:
		v = append(v, errs.Violation{
			Field: "channel_ids", Code: "required",
			Message: "a policy must name at least one destination channel",
		})
	case len(p.ChannelIDs) > MaxPolicyChannels:
		v = append(v, errs.Violation{
			Field: "channel_ids", Code: "max_items", Message: "at most 16 channels",
		})
	}
	for _, id := range p.ChannelIDs {
		if id == uuid.Nil {
			v = append(v, errs.Violation{
				Field: "channel_ids", Code: "required", Message: "a channel id may not be empty",
			})
		}
	}

	if (p.Throttle.Max > 0) != (p.Throttle.Window > 0) {
		v = append(v, errs.Violation{
			Field: "throttle", Code: "incomplete",
			Message: "a throttle needs both max and window_seconds, or neither",
		})
	}

	v = append(v, p.validateSubjects()...)
	v = append(v, p.validateDigest()...)
	v = append(v, p.validateCount()...)

	if len(v) > 0 {
		return errs.Validation("policy_invalid", "the notification policy is not valid", v...)
	}
	return nil
}

// validateSubjects restates policies_subjkinds_ck in Go and adds the one rule the
// database cannot hold — that the binding must admit something.
//
// ⛔ THE COHERENCE RULE IS NOT A CHECK CONSTRAINT AND CANNOT BE, WHICH IS THE SAME
// DIVISION 00063 DREW FOR A LABEL-NAME GRAMMAR. It needs `Reason.Subject()`, a
// fifteen-entry Go map, and the alternatives are both worse than the rule they buy:
// a CHECK may not contain a subquery, so the map would have to be transcribed into
// the DDL as a literal — a second copy of the allocation, updated by hand, in the
// one place nothing tests. So the vocabulary and the set-ness are the database's
// and the coherence is this function's, and the cost is stated rather than hidden:
// a row written around the service can carry a binding that routes nothing. It
// suppresses; it corrupts nothing.
//
// ⚠️ AN INERT POLICY IS THE FAILURE THIS EXISTS TO CATCH, and it is the same
// failure `policies_digest_reason_ck` exists to catch one axis over. A binding
// admitting none of the declared Reasons' altitudes routes nothing at all — and it
// does not error, it mints a `no_policy` SUPPRESSION per fact, forever, on a policy
// whose settings screen looks configured. That is silent suppression (SPEC §B.6),
// and refusing it at the door is the only place it is visible.
func (p Policy) validateSubjects() []errs.Violation {
	var v []errs.Violation
	if p.Subjects.Unrestricted() {
		// Every kind. There is nothing to check and nothing to be incoherent with.
		return nil
	}

	if len(p.Subjects) > MaxPolicySubjectKinds {
		v = append(v, errs.Violation{
			Field: "subject_kinds", Code: "max_items",
			Message: "at most 3 subject kinds, which is the whole vocabulary",
		})
	}

	// Vocabulary and set-ness, reported once per repeated value for the reason
	// `reasons` reports once per repeated value: a list naming `case` three times is
	// one mistake, not two.
	seen := make(map[SubjectKind]bool, len(p.Subjects))
	duplicated := make(map[SubjectKind]bool)
	for _, k := range p.Subjects {
		if !k.Valid() {
			v = append(v, errs.Violation{
				Field: "subject_kinds", Code: "enum",
				Message: "unknown notification subject kind " + string(k),
			})
		}
		if seen[k] && !duplicated[k] {
			duplicated[k] = true
			v = append(v, errs.Violation{
				Field: "subject_kinds", Code: "duplicate_items",
				Message: "subject_kinds must not contain duplicates: " + string(k) + " is listed twice",
			})
		}
		seen[k] = true
	}

	// The coherence rule. It is checked against the DECLARED Reasons rather than
	// against the whole vocabulary, because the question is whether THIS policy can
	// ever route anything — not whether the binding is satisfiable in principle.
	if len(p.Reasons) > 0 {
		routes := false
		for _, r := range p.Reasons {
			if r.Valid() && p.Subjects.Binds(r.Subject()) {
				routes = true
				break
			}
		}
		if !routes {
			v = append(v, errs.Violation{
				Field: "subject_kinds", Code: "incoherent",
				Message: "this subject binding admits none of the policy's reasons, so the policy " +
					"would route nothing and record a no_policy suppression for every fact it saw — " +
					"name the subject kind its reasons are about, or leave subject_kinds empty for all of them",
			})
		}
	}

	return v
}

// validateCount restates policies_count_min_ck, policies_count_window_ck,
// policies_count_pair_ck and policies_count_subject_ck in Go, so that a bad
// condition comes back as a field-level violation the settings form can point at
// rather than as a 23514 an operator has to decode (CONTEXT.md §5b).
//
// The field paths are the JSON names and never the column names, for the reason
// `validateDigest` gives: `count_window_s` is a spelling no client has ever been
// sent.
func (p Policy) validateCount() []errs.Violation {
	var v []errs.Violation

	if p.Count.Min != 0 &&
		(p.Count.Min < MinCountThreshold || p.Count.Min > MaxCountThreshold) {
		v = append(v, errs.Violation{
			Field: "count_min", Code: "range",
			Message: "the count threshold is 2 to 10000, or unset — a threshold of 1 is cleared " +
				"by the fact being evaluated and so states no condition at all",
		})
	}
	if p.Count.Window != 0 &&
		(p.Count.Window < MinCountWindow || p.Count.Window > MaxCountWindow) {
		v = append(v, errs.Violation{
			Field: "count_window_seconds", Code: "range",
			Message: "the count window is 60 to 86400 seconds, or unset",
		})
	}

	// Both halves or neither. Unlike the digest's pair rule this is symmetric —
	// see CountOverWindow.Enabled for why neither half means anything alone — so
	// it is reported against whichever half is missing, which is the one the
	// operator has to fill in.
	switch {
	case p.Count.Min > 0 && p.Count.Window == 0:
		v = append(v, errs.Violation{
			Field: "count_window_seconds", Code: "incomplete",
			Message: "a count threshold needs a window: a threshold over unbounded history " +
				"is not something anything can evaluate",
		})
	case p.Count.Window > 0 && p.Count.Min == 0:
		v = append(v, errs.Violation{
			Field: "count_min", Code: "incomplete",
			Message: "a count window needs a threshold: counting facts over a span and then " +
				"comparing the number against nothing is not a condition",
		})
	}

	// ⭐ THE UNIT RULE, AND IT IS WHY THE TWO AXES LANDED IN ONE MIGRATION. A count
	// is a number of somethings, and the something is the policy's bound subject
	// kind. An unrestricted binding supplies no unit and a two-kind binding supplies
	// two, so both are refused for the count — and only for the count. A policy with
	// no count condition needs no unit and keeps every binding this file admits.
	//
	// ⛔⛔ AND THE ONE KIND MUST BE `case`, WHICH IS NARROWER THAN 00072'S OWN
	// ADVERTISEMENT AND IS REFUSED HERE RATHER THAN HONOURED WRONGLY. Both other
	// bindings pass the cardinality rule and neither can decide anything an operator
	// would recognise:
	//
	//   - `{alert}` IS A PERMANENT MUTE. The numerator is `count(DISTINCT
	//     subject_id)` and an alert-subject row's `subject_id` is the alert
	//     IDENTITY, one value across every firing — so five re-fires count ONE, the
	//     evaluator's `+1` makes one, and `1 < count_min` suppresses every
	//     notification the policy would ever route, forever, as `below_threshold`.
	//   - `{digest}` IS AN INERT KNOB. Digests are minted by the tick against
	//     `digest_floor` and never pass through the suppressors, so `count_min` on
	//     that binding is read by nothing at all — the `refire_grace_s` shape
	//     migration 00071 deleted two settings for.
	//
	// Refusing beats reinterpreting: making `{alert}` count OCCURRENCES would change
	// what an operator observes into something no column says, and `{case}` already
	// expresses the ticket's own example exactly — five firings of one alert are five
	// Cases and therefore five distinct subjects. See CountOverWindow.
	if p.Count.Enabled() {
		switch kind, ok := p.Subjects.Sole(); {
		case !ok:
			v = append(v, errs.Violation{
				Field: "subject_kinds", Code: "required",
				Message: "a count condition must name exactly one subject kind, because a count " +
					"needs a unit: counting alert-subject facts and case-subject facts into one " +
					"number would be adding identities to episodes",
			})
		case kind != SubjectCase:
			v = append(v, errs.Violation{
				Field: "subject_kinds", Code: "unsupported",
				Message: "a count condition counts Cases, so subject_kinds must be exactly " +
					"[\"case\"]: an alert's subject is its identity and does not change when it " +
					"fires again, so counting alert-subject facts would never reach a threshold " +
					"above one and would mute this policy permanently — and a digest is minted " +
					"against digest_floor by the digest tick, which never reads count_min at all",
			})
		}
	}

	return v
}

// ValidateExplicit catches what a PATCH can state but a MERGED policy can no
// longer distinguish.
//
// ⛔ THE MERGED VIEW CANNOT SEE AN EXPLICIT ZERO FLOOR. `Digest.Floor` uses zero to
// mean "no floor", so `{"digest_floor": 0}` and `{"digest_floor": null}` both fold
// to `Floor = 0` and both look unset to Validate. The repository does NOT treat
// them alike: a null writes SQL NULL, and an explicit zero writes a literal 0,
// which `policies_digest_floor_ck` (NULL, or 1..10000) refuses as a 23514 -- a 500
// carrying a constraint name, with no field path for the settings form to point at.
// That is the exact failure validateDigest exists to prevent, so the check has to
// happen where the distinction still exists: on the patch, before the fold.
//
// Only the LOWER bound needs restating. Any floor above zero survives the fold
// intact and validateDigest sees it, so the ceiling is already covered there.
func (p PolicyPatch) ValidateExplicit() error {
	var v []errs.Violation
	if p.DigestFloor != nil {
		if f := *p.DigestFloor; f != nil && *f < MinDigestFloor {
			v = append(v, errs.Violation{
				// The JSON name, for the reason given on unacked_reminder_after_seconds.
				Field: "digest_floor", Code: "range",
				Message: "the digest floor is 1 to 10000, or unset — " +
					"zero would ask for a digest of an empty window, which never sends",
			})
		}
	}
	// ⭐ THE COUNT CONDITION HAS THE SAME HOLE IN BOTH HALVES, and it is wider than
	// the digest's. `Digest.Floor` uses zero for "unset" and so does `Count.Min`,
	// but `Count.Window` uses zero for "unset" too — so `{"count_min": 1}`,
	// `{"count_min": 0}` and `{"count_window_seconds": 30}` all fold to something
	// `validateCount` reads as absent, and the repository writes the literal the
	// operator sent. `policies_count_min_ck` (NULL, or 2..10000) and
	// `policies_count_window_ck` (NULL, or 60..86400) then refuse the row as a 23514
	// with no field path for the form to point at.
	//
	// Only the LOWER bounds need restating, for the reason given above: any value
	// above the floor survives the fold intact and `validateCount` sees it.
	if p.CountMin != nil {
		if n := *p.CountMin; n != nil && *n < MinCountThreshold {
			v = append(v, errs.Violation{
				Field: "count_min", Code: "range",
				Message: "the count threshold is 2 to 10000, or unset — a threshold of 1 is cleared " +
					"by the fact being evaluated and so states no condition at all",
			})
		}
	}
	if p.CountWindow != nil {
		if w := *p.CountWindow; w != nil && *w < MinCountWindow {
			v = append(v, errs.Violation{
				Field: "count_window_seconds", Code: "range",
				Message: "the count window is 60 to 86400 seconds, or unset",
			})
		}
	}
	if len(v) > 0 {
		return errs.Validation("policy_invalid", "the notification policy is not valid", v...)
	}
	return nil
}

// validateDigest restates policies_digest_window_ck, policies_digest_floor_ck,
// policies_digest_pair_ck and policies_digest_reason_ck in Go, so that a bad digest
// comes back as a field-level violation the settings form can point at rather than
// as a 23514 an operator has to decode (CONTEXT.md §5b).
//
// The field paths are the JSON names, never the column names: a violation path is
// what a client maps onto a control, and `digest_window_s` is a spelling no client
// has ever been sent.
func (p Policy) validateDigest() []errs.Violation {
	var v []errs.Violation

	if p.Digest.Window != 0 {
		switch {
		case p.Digest.Window < MinDigestWindow || p.Digest.Window > MaxDigestWindow:
			v = append(v, errs.Violation{
				Field: "digest_window_seconds", Code: "range",
				Message: "the digest window is 300 to 86400 seconds, or unset",
			})
		case !DigestWindowAligned(p.Digest.Window):
			// The divisor rule, and the message says WHY rather than just refusing:
			// "not a divisor of 86400" is meaningless without the reason, which is that
			// every boundary has to be a wall-clock boundary in UTC.
			v = append(v, errs.Violation{
				Field: "digest_window_seconds", Code: "alignment",
				Message: "the digest window must divide the day evenly (300, 600, 900, 1800, 3600, 7200, …) " +
					"so that every window boundary is a wall-clock boundary in UTC",
			})
		}
		// ⛔ TWO SEPARATE REFUSALS, BECAUSE `Handles` ASKS TWO QUESTIONS AND THIS USED
		// TO BLAME THE WRONG ONE. Since migration 00072 `Handles` is false either
		// because `reasons` omits `digest` or because `subject_kinds` does not bind the
		// `digest` altitude, and reporting both against `reasons` sent an operator to
		// edit a list that was already correct. The outcome is identical and the
		// SENTENCE has to name the control that is wrong — the same defect
		// `Policy.Handles`'s own ⚠️ block records for `Preview`'s coarse verdict,
		// fixed here because a validation violation carries a field path a form points
		// at.
		if !p.Subjects.Binds(SubjectDigest) {
			v = append(v, errs.Violation{
				Field: "subject_kinds", Code: "incoherent",
				Message: "a policy with a digest window must bind the `digest` subject kind, " +
					"or leave subject_kinds empty for all of them: its digests are minted at " +
					"the `digest` altitude, so a binding that omits it routes none of them — " +
					"the digest tick warns once per tick forever and the reconciler skips the " +
					"policy, hiding the gap that results",
			})
		} else if !p.Handles(ReasonDigest) {
			v = append(v, errs.Violation{
				Field: "reasons", Code: "required",
				Message: "a policy with a digest window must also react to the `digest` reason, " +
					"or its digests would be suppressed as no_policy once per window",
			})
		}
	}

	switch {
	// Zero is UNSET here, not a floor -- the same shape as UnackedReminderAfter
	// above. Stating the bound as MinDigestFloor rather than as a bare `< 0` is
	// what makes this the admissible set `policies_digest_floor_ck` enforces
	// (NULL, or 1..10000) instead of a wider one that happens to share its
	// rejections for negatives.
	case p.Digest.Floor != 0 &&
		(p.Digest.Floor < MinDigestFloor || p.Digest.Floor > MaxDigestFloor):
		v = append(v, errs.Violation{
			Field: "digest_floor", Code: "range",
			Message: "the digest floor is 1 to 10000, or unset",
		})
	case p.Digest.Floor > 0 && p.Digest.Window == 0:
		v = append(v, errs.Violation{
			Field: "digest_floor", Code: "incomplete",
			Message: "a digest floor needs a digest window: a threshold over an unbounded span " +
				"is not something anything can evaluate",
		})
	}

	return v
}
