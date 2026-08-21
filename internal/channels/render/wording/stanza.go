package wording

// StanzaID names one unit of a rendered message.
//
// The set is SPEC §H.7's block budget — "8 base blocks (title, body, fields,
// members, trail, rule, actions, footer)" — which had no collective noun before
// ADR 0037 gave it one. The names are kept identical to the SPEC's so the
// vocabulary does not fork.
type StanzaID string

const (
	StanzaTitle StanzaID = "title"
	StanzaBody  StanzaID = "body"
	// StanzaFields is DECLARED AND REFUSED. See Wordable.
	StanzaFields StanzaID = "fields"
	// StanzaMembers and StanzaTrail are DECLARED AND REFUSED. See Wordable.
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
//
// ⛔ `fields` IS THE SECOND, AND FOR A DIFFERENT REASON. Every wordable Stanza
// renders to ONE section text, so substituting a string for it is exactly the
// swap ADR 0037 describes. The fields stanza does not: it renders to a GRID of up
// to ten `Text` objects, each with its own 2 000-byte budget, in a binding §H.7
// order whose tail is what gets shed when the budget runs out. A one-line Wording
// there would not re-word the grid, it would REPLACE the grid with a paragraph —
// which is a structural change wearing a wording's clothes, and the one thing
// this feature promised not to be. Per-field wordings are a coherent future
// feature and a different shape; this is not a cheap version of it.
//
// ⛔ `members` AND `trail` ARE REFUSED BY ADR 0037'S OWN CEILING. Both render a
// SEQUENCE — the member instances, the state transitions — and the ADR refuses
// enumeration outright: "Without {% for %}, a Wording cannot iterate the member
// list. 'N of M instances' stays Go's. This is a real ceiling and it is accepted
// deliberately." A single-line Wording could only replace the whole sequence with
// a sentence that cannot name its own contents, which loses the information the
// stanza exists to carry. They are listed here rather than quietly accepted so
// that the ceiling is visible at the place somebody would try to cross it.
//
// ⚠️ FOUR OF THE EIGHT §H.7 STANZAS ARE THEREFORE WORDABLE: title, body, rule and
// footer — the ones that are PROSE. This is narrower than the design note's
// "Wordings across all eight Stanzas", which asserted the number without checking
// which stanzas are prose and which are grids, sequences or buttons. The gap the
// feature was justified by is fully inside the four: "Firing 20 minutes, 4th time
// this week, still unacked" is a title or a body.
func (s StanzaID) Wordable() bool {
	switch s {
	case StanzaTitle, StanzaBody, StanzaRule, StanzaFooter:
		return true
	default:
		return false
	}
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
	case s == StanzaMembers:
		return "the member list is an enumeration, and a wording cannot iterate: " +
			"replacing it with one line would drop the instances it exists to name"
	case s == StanzaTrail:
		return "the state trail is an enumeration, and a wording cannot iterate: " +
			"replacing it with one line would drop the transitions it exists to record"
	case s == StanzaFields:
		return "the fields grid is up to ten separately-budgeted cells in a binding " +
			"order that decides what is shed on overflow, so one line of prose would " +
			"replace the grid rather than re-word it"
	default:
		return ""
	}
}

func (s StanzaID) String() string { return string(s) }
