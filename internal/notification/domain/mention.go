package domain

// The unacked-reminder mention audience (ADR 0020, SPEC §G.9.1).
//
// ⭐ WHY A MENTION IS PART OF THE BROADCAST DECISION AT ALL. The reminder is the
// one thing oto says whose purpose is to reach somebody who has NOT engaged.
// `reply_broadcast` gets a REFERENCE to it into the channel; it does not get a
// notification onto anybody's phone. Slack's rules, verified:
//
//   - `@here` / `@channel` / `@everyone`: "These mentions won't notify people
//     when their notifications are paused or when they're used in threads."
//     (https://slack.com/help/articles/202009646-Notify-a-channel-or-workspace)
//     A broadcasting reply IS a thread reply. Slack documents no exception for
//     `reply_broadcast`, so on the evidence these two modes are SILENT NO-OPS in
//     the position oto uses them, and the default is `none` because of it.
//   - `<@U…>`: "the mentioned user will also be notified about the reference".
//   - `<!subteam^S…>`: "Slack will notify each user in the group about the
//     mention". (https://docs.slack.dev/messaging/formatting-message-text)
//     Neither carries a thread carve-out, so an explicit list is the only form
//     known to work.
//
// ⛔⛔ THE MENTION GOES IN THE TOP-LEVEL `text`, NEVER INSIDE A BLOCK. SPEC §H.1
// S3 puts all of oto's blocks inside one attachment (it is the only way to get a
// colour bar), and Slack's `thread_broadcast` reference "cannot contain
// attachments or message buttons". A mention buried in a block therefore does not
// even APPEAR in the channel copy, let alone notify. This is a direct consequence
// of the stripping behaviour ADR 0020 records, and it is why MentionPolicy hands
// back a list for the `text` rather than a rendered block.
//
// ⛔ A LIST IS NOT A ROTA (SCOPE-BOUNDARY §4.8, ADR 0013). Fixed ids, chosen once,
// in configuration. There is no time field on this struct and there must never be
// one: a mention audience that varies with the hour is a schedule, and a schedule
// is the thing oto is defined by not being.

// MentionPolicy is the org's answer to "who does the unacked reminder address".
type MentionPolicy struct {
	// Mode is `none` | `here` | `channel` | `list`. The empty string is `none`.
	Mode string
	// List is the explicit audience for mode `list`: Slack user and usergroup ids
	// in Slack's own wire form. It is ignored by every other mode.
	List []string
	// MinSeverity is the severity class at or above which a mention is attached at
	// all: `critical` (the default), `warning` or `info`.
	MinSeverity string
}

// The mode values, duplicated from `identity/domain` rather than imported: the
// notification module must not depend on the tenant module, and the value crosses
// the boundary as a string exactly like `verbosity` does. If the two ever
// disagree, an unknown mode resolves to "address nobody", which is the safe
// direction.
const (
	mentionNone    = "none"
	mentionHere    = "here"
	mentionChannel = "channel"
	mentionList    = "list"
)

// Slack's wire spellings for the two special mentions.
const (
	// SlackMentionHere is `@here`.
	SlackMentionHere = "<!here>"
	// SlackMentionChannel is `@channel`.
	SlackMentionChannel = "<!channel>"
)

// MaxMentions caps the rendered audience. It mirrors the settings-side cap and
// exists again here because this is the last gate before the string reaches a
// message: a reminder that notifies forty people is a page (ADR 0013).
const MaxMentions = 10

// severityRank orders the §H.2 severity classes. `page` and `critical` share a
// rank because §H.2 renders them identically — they are two spellings of one
// severity. Anything oto has never seen is OFF the scale, not at the bottom of
// it, and the second result says so.
func severityRank(s string) (int, bool) {
	switch s {
	case "critical", "page":
		return 3, true
	case "warning":
		return 2, true
	case "info", "none":
		return 1, true
	default:
		return 0, false
	}
}

// Audience is who this reminder addresses, given the severity of the thing being
// reminded about. An empty result means nobody, which is the default.
//
// ⛔ THE SEVERITY GATE FAILS CLOSED, AND THAT IS THE POINT. A severity oto cannot
// rank — a label it has never seen, or an absent one — produces NO mention, at
// every setting. The alternative is that a typo'd `severity:` label pings ten
// people, and the channel that learns to mute oto because of it is a channel that
// will also miss the real incident. `@here` on every unacked warning is how that
// happens, and the default floor of `critical` is what stops it.
func (p MentionPolicy) Audience(severity string) []string {
	mode := p.Mode
	if mode == "" {
		mode = mentionNone
	}
	if mode == mentionNone {
		return nil
	}

	floor := p.MinSeverity
	if floor == "" {
		floor = "critical"
	}
	want, wantOK := severityRank(floor)
	got, gotOK := severityRank(severity)
	if !wantOK {
		// An unreadable floor is treated as the strictest one, never as "no gate".
		want, wantOK = severityRank("critical")
	}
	if !wantOK || !gotOK || got < want {
		return nil
	}

	switch mode {
	case mentionHere:
		return []string{SlackMentionHere}
	case mentionChannel:
		return []string{SlackMentionChannel}
	case mentionList:
		if len(p.List) == 0 {
			return nil
		}
		out := p.List
		if len(out) > MaxMentions {
			out = out[:MaxMentions]
		}
		return append([]string(nil), out...)
	default:
		// An unknown mode addresses nobody. A vocabulary mismatch between the
		// tenant module and this one must not be able to invent a channel-wide
		// ping.
		return nil
	}
}
