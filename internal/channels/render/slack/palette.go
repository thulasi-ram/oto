package slack

import "github.com/thulasiram/oto/internal/channels/domain"

// CardState is what the card's colour says. It is NOT AlertCase.State: a
// group has many members, and the card must answer one question — "do I need to
// act?" — with one colour (S4).
type CardState string

// The card states. Their hexes are Grafana OnCall's palette, the best-tested open
// alert palette that exists (§H.2). `expired` has no upstream precedent and is an
// oto original.
//
// ⛔ THERE IS NO `storm` STATE. Storm damping is removed (ADR 0042) and migration
// 00059 dropped `alert_groups.storm_mode`, so no group can enter it and the purple
// it spent is retired with it. The hex is deliberately NOT written here: the palette
// gate reads every colour literal out of this file and fails on one §H.2 does not
// list, which is what makes the two agree.
// A card state is a LIVE reading of a group; it is not a stored value
// with a row to render, which is why it goes where `notifications.reason = 'storm'`
// stays.
//
// This palette is a SEPARATE, UNCHANGED system from the oto UI tokens. Do not
// harmonise them: different substrate, different contrast contract. A renderer
// must never read a --oto-* token (CONTEXT.md §5c).
const (
	// CardFiring means at least one member is firing and unacknowledged.
	CardFiring CardState = "firing"
	// CardAcknowledged means somebody took it. It is still firing.
	CardAcknowledged CardState = "acknowledged"
	// CardSuppressed means an Alertmanager silence covers it. Only the reconciler
	// can produce this state (C1).
	CardSuppressed CardState = "suppressed"
	// CardResolved means an explicit upstream status="resolved" arrived.
	CardResolved CardState = "resolved"
	// CardExpired means oto stopped hearing about it. It is NEVER a resolution.
	CardExpired CardState = "expired"
)

// Colour is the attachment colour for a card state (§H.2).
func (s CardState) Colour() string {
	switch s {
	case CardFiring:
		return "#a30200"
	case CardAcknowledged:
		return "#daa038"
	case CardSuppressed:
		return "#dddddd"
	case CardResolved:
		return "#2eb886"
	case CardExpired:
		return "#6b6b6b"
	default:
		return "#6b6b6b"
	}
}

// Emoji is the state emoji shown in the Status field (§H.2).
func (s CardState) Emoji() string {
	switch s {
	case CardFiring:
		return ":fire:"
	case CardAcknowledged:
		return ":eyes:"
	case CardSuppressed:
		return ":mute:"
	case CardResolved:
		return ":white_check_mark:"
	case CardExpired:
		return ":grey_question:"
	default:
		return ":grey_question:"
	}
}

// Label is the human word for the state, used in the Status field and in the
// top-level text. "Expired" must read as "we lost sight", never as "resolved".
func (s CardState) Label() string {
	switch s {
	case CardFiring:
		return "Firing"
	case CardAcknowledged:
		return "Acked"
	case CardSuppressed:
		return "Silenced"
	case CardResolved:
		return "Resolved"
	case CardExpired:
		return "Expired"
	default:
		return "Unknown"
	}
}

// Banner is the bracketed word in the top-level text: "[FIRING] HighErrorRate …".
func (s CardState) Banner() string {
	switch s {
	case CardFiring:
		return "FIRING"
	case CardAcknowledged:
		return "ACKED"
	case CardSuppressed:
		return "SILENCED"
	case CardResolved:
		return "RESOLVED"
	case CardExpired:
		return "EXPIRED"
	default:
		return "UNKNOWN"
	}
}

// IsTerminal reports whether the card has stopped being actionable. A terminal
// card sheds its members section, its rule context and its buttons (§H.4): once
// the flow has completed, every one of those is zero information (S11).
func (s CardState) IsTerminal() bool { return s == CardResolved || s == CardExpired }

// SeverityEmoji maps a severity label to its leading emoji (§H.2).
//
// Severity travels as an emoji and never as colour, because colour is spent on
// state and because Slack's accessibility guidance requires more than one channel.
func SeverityEmoji(severity string) string {
	switch normaliseSeverity(severity) {
	case "critical":
		return ":rotating_light:"
	case "warning":
		return ":warning:"
	case "info":
		return ":large_blue_circle:"
	default:
		return ":white_circle:"
	}
}

func normaliseSeverity(s string) string {
	switch lower(s) {
	case "critical", "page":
		return "critical"
	case "warning", "warn":
		return "warning"
	case "info", "informational", "none", "":
		return "info"
	default:
		return "other"
	}
}

// DeriveCardState computes the one colour a group gets from its member counts.
//
// The ordering is a product decision, not an implementation detail:
//
//   - anything still firing and unacknowledged outranks everything else, because
//     that is the only state that means "act now";
//   - expired outranks resolved when both are present, because a group that
//     contains alerts we lost sight of has NOT resolved, and oto never fabricates
//     a resolution (CONTEXT.md §3).
func DeriveCardState(g domain.GroupView) CardState {
	switch {
	case g.FiringCount > 0 && g.AckedCount < g.FiringCount:
		return CardFiring
	case g.FiringCount > 0:
		return CardAcknowledged
	case g.SuppressedCount > 0:
		return CardSuppressed
	case g.ExpiredCount > 0:
		return CardExpired
	case g.ResolvedCount > 0:
		return CardResolved
	default:
		return CardExpired
	}
}
