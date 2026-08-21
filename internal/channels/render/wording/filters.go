package wording

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/osteele/liquid"
)

// humanise renders a duration the way an operator reads one: two units at most,
// largest first, never "0s" padding.
//
// ⚠️ IT IS A DELIBERATE TWIN OF slack.humanDuration AND MUST STAY ONE. That
// function is unexported and golden-tested inside its own package; this one serves
// every provider. TestHumaniseMatchesSlackWording pins the shared cases so the two
// cannot drift into telling the same operator two different firing durations for
// the same signal on two different channels.
func humanise(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Second:
		return "under a second"
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		m, s := int(d.Minutes()), int(d.Seconds())%60
		if s == 0 {
			return strconv.Itoa(m) + "m"
		}
		return strconv.Itoa(m) + "m " + strconv.Itoa(s) + "s"
	case d < 24*time.Hour:
		h, m := int(d.Hours()), int(d.Minutes())%60
		if m == 0 {
			return strconv.Itoa(h) + "h"
		}
		return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m"
	default:
		days, h := int(d.Hours())/24, int(d.Hours())%24
		if h == 0 {
			return strconv.Itoa(days) + "d"
		}
		return strconv.Itoa(days) + "d " + strconv.Itoa(h) + "h"
	}
}

// FilterNames is oto's entire curated filter set, and the list is the contract.
//
// ⛔ CURATED BY CONSTRUCTION, NOT BY SUBTRACTION. liquid.NewBasicEngine() ships
// ZERO filters (verified: all 25 probed report "undefined filter"), so this slice
// is not a subset of somebody else's set that must be re-audited on every upgrade —
// it is the whole surface. Adding a filter is a deliberate edit to this file.
//
// ⚠️ NO FILTER HERE EMITS A PROVIDER'S SYNTAX. `strike` does not write a tilde and
// `datetime` does not write Slack's <!date> token; both emit neutral marks that a
// Dialect spells. See dialect.go for why that is a correctness requirement rather
// than tidiness.
var FilterNames = []string{
	"default",
	"upper", "lower", "capitalise",
	"truncate_runes",
	"code", "strike", "bold", "italic",
	"datetime", "plural", "human_duration",
}

// registerFilters installs exactly FilterNames on e.
func registerFilters(e *liquid.Engine) {
	// default is load-bearing for TOTALITY: every field reference in a Wording is
	// meant to carry one, so a Stanza can never render empty and no zero-information
	// rule is violated by absence. An empty string is falsy, which matches.
	e.RegisterFilter("default", func(v, fallback any) any {
		if isBlank(v) {
			return fallback
		}
		return v
	})

	e.RegisterFilter("upper", func(v any) any { return strings.ToUpper(str(v)) })
	e.RegisterFilter("lower", func(v any) any { return strings.ToLower(str(v)) })
	e.RegisterFilter("capitalise", func(v any) any {
		s := str(v)
		if s == "" {
			return s
		}
		r, n := utf8.DecodeRuneInString(s)
		return strings.ToUpper(string(r)) + s[n:]
	})

	// truncate_runes counts RUNES, unlike the byte-budgeted sink it feeds. An author
	// asking for forty characters means forty characters; the sink's separate byte
	// ceiling is Slack's, and both apply.
	e.RegisterFilter("truncate_runes", func(v any, n int) any {
		s := str(v)
		if n <= 0 || utf8.RuneCountInString(s) <= n {
			return s
		}
		return string([]rune(s)[:n]) + "…"
	})

	e.RegisterFilter("code", wrap(markCodeOpen, markCodeClose))
	e.RegisterFilter("strike", wrap(markStrikeOpen, markStrikeClose))
	e.RegisterFilter("bold", wrap(markBoldOpen, markBoldClose))
	e.RegisterFilter("italic", wrap(markItalicOpen, markItalicClose))

	// datetime passes a time mark straight through: BuildInput already stamped both
	// the epoch and oto's UTC rendering into it, and the Dialect decides which the
	// provider can use. Applied to anything that is not a mark it is the identity,
	// so `{{ "n/a" | datetime }}` says "n/a" rather than erroring.
	e.RegisterFilter("datetime", func(v any) any { return str(v) })

	e.RegisterFilter("plural", func(v any, one, many string) any {
		n := toInt(v)
		if n == 1 {
			return "1 " + one
		}
		return strconv.Itoa(n) + " " + many
	})

	// human_duration takes SECONDS, because that is what a count of seconds looks
	// like coming out of a JSON payload, and returns oto's own phrasing.
	e.RegisterFilter("human_duration", func(v any) any {
		return humanise(time.Duration(toInt(v)) * time.Second)
	})
}

func wrap(open, shut rune) func(any) any {
	return func(v any) any {
		s := str(v)
		if strings.TrimSpace(s) == "" {
			// Marking nothing produces a pair of empty delimiters on the card —
			// "``" or "~~" — which reads as a rendering bug rather than an absent
			// value. An empty input stays empty and `default` can catch it.
			return ""
		}
		return string(open) + s + string(shut)
	}
}

func isBlank(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case bool:
		return !t
	}
	return false
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(t)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	}
	return ""
}

func toInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int32:
		return int(t)
	case int64:
		return int(t)
	case float64:
		return int(t)
	case float32:
		return int(t)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(t))
		return n
	}
	return 0
}
