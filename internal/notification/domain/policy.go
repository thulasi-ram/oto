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
func (p Policy) Handles(r Reason) bool {
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

	v = append(v, p.validateDigest()...)

	if len(v) > 0 {
		return errs.Validation("policy_invalid", "the notification policy is not valid", v...)
	}
	return nil
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
		if !p.Handles(ReasonDigest) {
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
