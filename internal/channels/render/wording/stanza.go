package wording

// StanzaID names one unit of a rendered message.
//
// The set is SPEC §H.7's block budget — "8 base blocks (title, body, fields,
// members, trail, rule, actions, footer)" — which had no collective noun before
// ADR 0037 gave it one. The names are kept identical to the SPEC's so the
// vocabulary does not fork.
type StanzaID string

const (
	StanzaTitle   StanzaID = "title"
	StanzaBody    StanzaID = "body"
	StanzaFields  StanzaID = "fields"
	StanzaMembers StanzaID = "members"
	StanzaTrail   StanzaID = "trail"
	StanzaRule    StanzaID = "rule"
	// StanzaActions is DECLARED AND REFUSED. See Wordable.
	StanzaActions StanzaID = "actions"
	StanzaFooter  StanzaID = "footer"
)

// AllStanzas is every name in SPEC §H.7's budget, in render order.
//
// ⚠️ RENDER ORDER IS NOT SHED ORDER. SPEC §H.7's list is a drop-order-on-overflow,
// and this one is the order a reader sees. They were one dial and that made a
// user-settable order silently decide what gets dropped; ADR 0037 leaves ordering
// undecided precisely so this distinction can exist first.
var AllStanzas = []StanzaID{
	StanzaTitle, StanzaBody, StanzaFields, StanzaMembers,
	StanzaTrail, StanzaRule, StanzaActions, StanzaFooter,
}

// Wordable reports whether a Wording may target this Stanza.
//
// ⛔ `actions` IS THE ONE THAT CANNOT BE, AND IT IS LISTED RATHER THAN OMITTED.
// Every other Stanza resolves to one text string with one insertion point. An
// actions block resolves to a list of Action structs, where the visible label is
// bound to a stable `action_id` — "oto.ack", "oto.snooze" — that an interaction
// handler matches on and an operator learns to recognise. Re-wording a button is
// therefore not a wording change, it is an interaction change, and oto has already
// shipped two defects from a button whose label promised something its id could not
// deliver (git-bug 85da108, ccad583).
//
// It stays IN the enum because SPEC §H.7 names eight and a seven-name enum would
// fork the vocabulary; it is refused HERE, once, with a sentence, because a stanza
// that is silently absent from a menu teaches nobody why.
func (s StanzaID) Wordable() bool {
	return s.Valid() && s != StanzaActions
}

// Valid reports whether s is one of the eight §H.7 names.
func (s StanzaID) Valid() bool {
	for _, k := range AllStanzas {
		if k == s {
			return true
		}
	}
	return false
}

// RefusalReason explains, to the person who typed it, why this Stanza takes no
// Wording. It returns "" for a Stanza that does.
func (s StanzaID) RefusalReason() string {
	switch {
	case !s.Valid():
		return "not a stanza of an oto notification"
	case s == StanzaActions:
		return "the actions row carries interactive buttons whose labels are bound to " +
			"their action ids, so its text is structure rather than wording"
	default:
		return ""
	}
}

func (s StanzaID) String() string { return string(s) }
