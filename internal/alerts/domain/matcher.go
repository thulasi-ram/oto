package domain

import (
	"regexp"
	"strings"
	"sync"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// ⭐⭐ THE ONE LABEL-MATCHER GRAMMAR OPERATORS WRITE, AND IT LIVES IN THE KERNEL SO
// THAT IT STAYS ONE (ADR 0017, ADR 0052 §2).
//
// It was declared in `notification/domain` beside the policy that was, for a long
// time, its only reader. A Correlator is the second: ADR 0052 §2 has it written as
// "matchers over Cases", in the grammar an operator already writes for a policy,
// and `incidents` may import neither `notification` (CONTEXT.md §4 draws no such
// edge, and `test/arch` refuses one) nor anything but the kernel from its domain
// package (`domain-must-be-pure`). The choice was a second grammar in `incidents`
// — a second regex cache, a second anchoring rule, a second answer to "does `!=`
// match a missing label" — or this one, moved to the layer every module may
// import (RULE K). A second copy of a predicate language is exactly what ADR 0017
// and migration 00076's header refuse, so it moved.
//
// `notification/domain` keeps every name it had as an ALIAS (`type Matcher =
// kernel.Matcher`), so no policy, template or test changed a character, and the
// error codes below are the ones that package always emitted — renaming a code
// is a contract change and moving a file is not.
//
// It matches LABELS ONLY. There is no matcher on a time of day, on a weekday, or
// on who is on call, and there never will be: a predicate whose outcome depends on
// WHEN it is evaluated is a schedule (SCOPE-BOUNDARY §4.8), whether a policy or a
// Correlator holds it.

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
type Matcher struct {
	Name  string
	Op    MatchOp
	Value string
}

// regexCache memoises anchored matcher regexes.
//
// Policy evaluation happens on every lifecycle transition and Correlator
// evaluation on every Case open, and the same handful of matchers are recompiled
// thousands of times an hour otherwise. The cache is bounded and dropped
// wholesale when it grows past the bound: a matcher set large enough to overflow
// it is already pathological, and an unbounded cache keyed by user-supplied
// strings is a memory leak with a nicer name. One cache for both readers, because
// they compile the same strings.
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
//
// ⚠️ THE CODES SAY `policy_matcher_*` FOR A CORRELATOR'S MATCHER TOO, and that is
// the cost of not renaming a code the notification contract already emits. Both
// callers fold the error into a field-level violation under their own outer code
// (`policy_invalid`, `correlator_invalid`) through `errs.ViolationsOf`, so the
// inner code is never what a client branches on.
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

// MatchAll reports whether EVERY matcher holds against labels. An empty list
// matches everything.
//
// Matchers are ANDed. There is no OR and no nesting: a predicate an operator
// cannot read at 3am is one that will put the wrong alert in the wrong place,
// whether the place is a channel or an Incident.
func MatchAll(ms []Matcher, labels map[string]string) (bool, error) {
	for _, m := range ms {
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
