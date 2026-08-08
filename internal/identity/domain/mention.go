package domain

import "regexp"

// The unacked-reminder mention vocabulary (ADR 0020, SPEC §G.9.1).
//
// ⭐ WHY THIS IS A SETTING AT ALL. The unacked reminder is the one thing oto says
// whose entire purpose is to reach somebody who has NOT engaged. It is delivered
// as a thread reply that broadcasts, and a thread reply notifies only people
// already in the thread — which is exactly the wrong audience. A mention is the
// only mechanism Slack offers for reaching past that.
//
// ⛔⛔ THE DEFAULT IS `none`, AND THE REASON IS RESEARCH, NOT TIMIDITY. Slack's
// own documentation says of `@here`, `@channel` and `@everyone`: "These mentions
// won't notify people when their notifications are paused or when they're used in
// threads."
// (https://slack.com/help/articles/202009646-Notify-a-channel-or-workspace)
// A broadcasting reply IS a thread reply — `reply_broadcast` changes where a
// REFERENCE to it appears, and Slack documents no exception to the thread rule
// for it. So `here` and `channel` are, on the evidence, SILENT NO-OPS in the one
// position oto would use them. Defaulting to a setting that does nothing is worse
// than having no default: it manufactures the belief that somebody was told.
//
// Individuals and usergroups are different, and this is the documented
// difference: for `<@U…>` Slack says "the mentioned user will also be notified
// about the reference", and for `<!subteam^S…>` "Slack will notify each user in
// the group about the mention", with no thread carve-out either time
// (https://docs.slack.dev/messaging/formatting-message-text). An explicit list is
// therefore the ONLY form of this setting known to work.
//
// ⛔ A LIST IS NOT A ROTA (SCOPE-BOUNDARY §4.8, ADR 0013). It is a fixed audience
// an operator chose once, in configuration. It must never become time-aware, must
// never acquire a second stage, and must never be derived from a schedule. oto
// does not know who is on call and will never pretend to. The structural
// guarantee is that this file holds a `[]string` of opaque Slack ids and there is
// no other field to hang a time on.

// MentionMode is the closed set of `unacked_reminder_mention` values.
type MentionMode = string

// The mention modes.
const (
	// MentionNone addresses nobody. THE DEFAULT.
	MentionNone MentionMode = "none"
	// MentionHere is `@here`. Believed to be a no-op in a thread reply; kept
	// because it is expressible in Slack and an operator may know something about
	// their workspace that the documentation does not say.
	MentionHere MentionMode = "here"
	// MentionChannel is `@channel`. Same caveat as MentionHere.
	MentionChannel MentionMode = "channel"
	// MentionList addresses the explicit individuals and usergroups in
	// `unacked_reminder_mention_list`. The only form Slack documents as notifying
	// from inside a thread.
	MentionList MentionMode = "list"
)

// MaxReminderMentions caps the explicit list. Ten is the same cap the Slack
// channel schema used, and it is a cap rather than a courtesy: a reminder that
// notifies forty people is a page, and oto pages nobody (ADR 0013).
const MaxReminderMentions = 10

// mentionModes is the closed set.
var mentionModes = map[string]bool{
	MentionNone: true, MentionHere: true, MentionChannel: true, MentionList: true,
}

// ValidMentionMode reports whether m is one of the four.
func ValidMentionMode(m string) bool { return mentionModes[m] }

// mentionTokenRe is a Slack user or usergroup mention, already in Slack's wire
// form. `@here` and `@channel` are NOT accepted here: they are modes, not list
// members, because a list that could contain `@channel` would let a five-person
// list quietly become a channel-wide ping.
var mentionTokenRe = regexp.MustCompile(`^(<@[UW][A-Z0-9]{2,}>|<!subteam\^S[A-Z0-9]{2,}>)$`)

// ValidMentionToken reports whether s is a permitted list member.
func ValidMentionToken(s string) bool { return mentionTokenRe.MatchString(s) }

// MentionMinSeverity is the closed set of `unacked_reminder_mention_min_severity`
// values: the severity class at or above which a mention is attached at all.
type MentionMinSeverity = string

// The gate values, mirroring §H.2's severity classes. `page` is not listed
// because §H.2 renders it identically to `critical` — they are two spellings of
// one severity, and offering both would invite an operator to pick the one their
// rules do not use.
const (
	// MentionSeverityCritical attaches a mention to critical (and `page`) only.
	// THE DEFAULT.
	MentionSeverityCritical MentionMinSeverity = "critical"
	// MentionSeverityWarning attaches it to warning and above.
	MentionSeverityWarning MentionMinSeverity = "warning"
	// MentionSeverityInfo attaches it to everything oto can rank.
	MentionSeverityInfo MentionMinSeverity = "info"
)

var mentionSeverities = map[string]bool{
	MentionSeverityCritical: true,
	MentionSeverityWarning:  true,
	MentionSeverityInfo:     true,
}

// ValidMentionMinSeverity reports whether s is one of the three.
func ValidMentionMinSeverity(s string) bool { return mentionSeverities[s] }
