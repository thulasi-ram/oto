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
	// MaxPolicyReasons is policies_reasons_ck.
	MaxPolicyReasons = 32
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

	// UnackedReminderAfter is `unacked_reminder_after_s`: how long a signal may
	// go unacknowledged before ONE Reason=unacked_reminder notification is sent to
	// the channels this policy already names. Zero disables it.
	//
	// ⛔ IT IS A SCALAR. ONE STAGE, FOREVER (SPEC §G.9.1, BINDING, PERMANENT). It
	// must never become a slice, a ladder, or a target other than ChannelIDs. The
	// moment it is an array, oto is an on-call product and FR-1 has been crossed.
	UnackedReminderAfter time.Duration

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

// RemindsOnUnacked reports whether this policy asks for the one reminder stage.
func (p Policy) RemindsOnUnacked() bool { return p.UnackedReminderAfter > 0 }

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
			Field: "reasons", Code: "max_items", Message: "at most 32 reasons",
		})
	}
	for _, r := range p.Reasons {
		if !r.Valid() {
			v = append(v, errs.Violation{
				Field: "reasons", Code: "enum",
				Message: "unknown notification reason " + string(r),
			})
		}
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

	if p.UnackedReminderAfter != 0 &&
		(p.UnackedReminderAfter < MinUnackedReminderAfter || p.UnackedReminderAfter > MaxUnackedReminderAfter) {
		v = append(v, errs.Violation{
			// The JSON name, not the column name. A violation path is what the
			// settings form maps onto a control (CONTEXT.md §5b, SPEC §L.8.2), and
			// `unacked_reminder_after_s` is a field no client has ever been sent.
			Field: "unacked_reminder_after_seconds", Code: "range",
			Message: "the unacked reminder delay is 60 to 86400 seconds, or unset",
		})
	}

	if len(v) > 0 {
		return errs.Validation("policy_invalid", "the notification policy is not valid", v...)
	}
	return nil
}
