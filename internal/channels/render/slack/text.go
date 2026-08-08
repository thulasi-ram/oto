package slack

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Slack's documented character limits (§H.7). They are constants here so that the
// renderer truncates against the same numbers Validate rejects against; a limit
// that lives in two places is a limit that will diverge.
const (
	maxSectionText  = 3000
	maxFieldText    = 2000
	maxFields       = 10
	maxContextItems = 10
	maxActionItems  = 25
	maxButtonText   = 75
	maxURL          = 3000
	maxButtonValue  = 2000
	maxID           = 255
	maxBlocks       = 50
	maxTopLevelText = 3000
	maxMetadata     = 8000
	maxPayloadBytes = 100000

	// otoTopLevelText is oto's own, tighter budget for the top-level text. Slack
	// documents no cap, but a push notification longer than this is not read.
	otoTopLevelText = 300

	// sectionTruncateAt is where a section is cut so that the ellipsis and the
	// "see full detail in oto" link still fit inside maxSectionText (§H.7).
	sectionTruncateAt = 2900
)

func lower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// escape neutralises the three characters Slack's mrkdwn parser treats as markup
// control characters. It is applied to every value that came from an upstream
// label or annotation: a label value containing "<!channel>" must not become a
// channel-wide ping.
func escape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// code renders s as inline code, escaped, with backticks stripped so a label
// value cannot break out of the span.
func code(s string) string {
	s = strings.ReplaceAll(escape(s), "`", "'")
	if s == "" {
		return ""
	}
	return "`" + s + "`"
}

// link renders an mrkdwn link. An empty url degrades to the bare label rather
// than emitting "<|label>", which Slack renders as literal garbage.
func link(url, label string) string {
	if url == "" {
		return escape(label)
	}
	return "<" + url + "|" + escape(label) + ">"
}

// truncateSection cuts text to Slack's section limit DELIBERATELY: a visible
// ellipsis plus a link to the full detail in oto.
//
// This is the rule that matters. A card that was silently truncated tells an
// operator a smaller truth than the one that exists, and they have no way to know.
// A card that says "…" and offers a link tells them exactly what happened.
func truncateSection(text, moreURL string) string {
	return truncateAt(text, sectionTruncateAt, maxSectionText, moreURL)
}

// truncateField cuts one section field, which has its own 2 000-char budget.
func truncateField(text, moreURL string) string {
	return truncateAt(text, maxFieldText-120, maxFieldText, moreURL)
}

func truncateAt(text string, cut, hard int, moreURL string) string {
	if len(text) <= hard {
		return text
	}
	suffix := "…"
	if moreURL != "" {
		suffix = "… " + link(moreURL, "see full detail in oto")
	}
	if cut > len(text) {
		cut = len(text)
	}
	head := text[:cut]
	// Never split a rune, and never split a link: cutting inside "<url|label>"
	// leaves a dangling "<" that Slack renders as raw text.
	for len(head) > 0 && !utf8.ValidString(head) {
		head = head[:len(head)-1]
	}
	if i := strings.LastIndex(head, "<"); i > strings.LastIndex(head, ">") {
		head = head[:i]
	}
	head = strings.TrimRight(head, " \t\n·•-")
	return head + suffix
}

// truncateRunes cuts a short string (a button label, a title) on a rune boundary
// with a visible ellipsis. There is no "view in oto" link here: these strings are
// too short to hang one off, and every card already carries the group link.
func truncateRunes(s string, maxLen int) string {
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	if maxLen <= 1 {
		return "…"
	}
	out := []rune(s)[:maxLen-1]
	return strings.TrimRight(string(out), " \t") + "…"
}

// slackDate renders a timestamp as Slack's <!date> token so every viewer sees it
// in their own timezone, with a UTC fallback for clients that cannot (S13).
func slackDate(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	u := t.UTC()
	return "<!date^" + strconv.FormatInt(u.Unix(), 10) + "^{time}|" + u.Format("15:04 MST") + ">"
}

// slackDateTime is slackDate with the day, for a timestamp that may be old enough
// that a bare time is ambiguous.
func slackDateTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	u := t.UTC()
	return "<!date^" + strconv.FormatInt(u.Unix(), 10) + "^{date_short_pretty} {time}|" +
		u.Format("2006-01-02 15:04 MST") + ">"
}

// plainClock is the literal UTC time used in the top-level text. The <!date>
// token does not render in a push notification, and the push notification is the
// thing an operator actually sees first (S5).
func plainClock(t time.Time) string {
	if t.IsZero() {
		return "an unknown time"
	}
	return t.UTC().Format("15:04 MST")
}

// humanDuration renders a duration the way an operator reads one: two units at
// most, largest first, never "0s" padding. Durations are computed server-side and
// re-rendered on every update (S13).
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Second:
		return "under a second"
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		if s == 0 {
			return strconv.Itoa(m) + "m"
		}
		return strconv.Itoa(m) + "m " + strconv.Itoa(s) + "s"
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return strconv.Itoa(h) + "h"
		}
		return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m"
	default:
		days := int(d.Hours()) / 24
		h := int(d.Hours()) % 24
		if h == 0 {
			return strconv.Itoa(days) + "d"
		}
		return strconv.Itoa(days) + "d " + strconv.Itoa(h) + "h"
	}
}

// plural renders "1 instance" / "3 instances" without the "(s)" that makes a
// product feel unfinished.
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// strike renders the §H.4 strikethrough trick: "~Firing~ → Resolved". A reader
// who saw the card an hour ago can tell what changed, at zero block cost.
func strike(previous, current string) string {
	if previous == "" || previous == current {
		return current
	}
	return "~" + escape(previous) + "~ → " + current
}

// joinNonEmpty joins the parts that carry information and drops the ones that do
// not, so a card never shows a separator with nothing on either side (S11).
func joinNonEmpty(sep string, parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

// number renders a metric value compactly. It is rendered as-is: oto does not
// know the unit, and inventing one would be a lie.
func number(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	s := strconv.FormatFloat(f, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// shortKey is the first 16 characters of a content hash, with no ellipsis: a
// truncated hash is understood as a prefix, and an ellipsis on one would read as
// a value that got cut off by accident.
func shortKey(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16]
}

// blockID builds the per-render block id required by S12. The nonce is derived
// from the view, not from a clock or a random source, so the renderer stays a
// pure function and its golden files stay stable.
func blockID(name, nonce string) string {
	id := "oto_" + name + "_" + nonce
	if len(id) > maxID {
		id = id[:maxID]
	}
	return id
}

// mentionList renders the resolved mention audience for an unacked reminder.
//
// ⛔⛔ ITS ONLY CALLER PUTS THE RESULT IN THE TOP-LEVEL `text`, AND THAT IS
// CORRECTNESS, NOT STYLE (ADR 0020). §H.1 S3 puts every oto block inside one
// attachment, and Slack's in-channel `thread_broadcast` reference "cannot contain
// attachments or message buttons" — so a mention rendered into a block is not
// merely un-notifying in the channel, it is not THERE. The top-level text is very
// nearly all a channel reader sees, and it is the only position a mention can
// occupy and still mean something.
//
// The tokens arrive in Slack's own wire form, already resolved and already gated
// on severity by the org's mention policy. Anything that is not a recognised
// mention shape is DROPPED rather than passed through: this string goes into a
// message, and an unvalidated fragment there is an injection surface.
//
// This list is NOT a rota and must never become time-aware (§G.9.1, §4.8). It is
// a fixed audience the operator chose once, in configuration, and oto has no
// concept of who is on call.
func mentionList(mentions []string) string {
	out := make([]string, 0, len(mentions))
	for _, m := range mentions {
		if mentionTokenRe.MatchString(m) {
			out = append(out, m)
		}
	}
	return strings.Join(out, " ")
}

// mentionTokenRe is the closed set of mention shapes this renderer will emit: a
// Slack user, a usergroup, `@here` or `@channel`. Nothing else reaches a message.
var mentionTokenRe = regexp.MustCompile(
	`^(<@[UW][A-Z0-9]{2,}>|<!subteam\^S[A-Z0-9]{2,}>|<!here>|<!channel>)$`)
