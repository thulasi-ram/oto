// Package wording turns a customer-authored Liquid template into the text of one
// Stanza of a notification, and bounds what that template can do.
//
// It implements ADR 0037. The short version of that ADR is the invariant this
// package exists to hold:
//
//	A Wording chooses WORDS. It never chooses structure, colour, mentions,
//	links, or destination.
//
// Everything here follows from taking that sentence literally across MORE THAN ONE
// provider. A Wording's output is text, and text is portable where layout is not —
// but only if the text carries no provider's markup. `*x*` is BOLD in Slack mrkdwn
// and ITALIC in Discord markdown, so a template that emits a literal asterisk does
// not degrade politely on a second provider, it renders the wrong emphasis and says
// nothing about having done so. So a Wording never emits markup at all: the curated
// filters emit NEUTRAL MARKS from the Unicode private-use area, and each provider's
// Dialect spells those marks in its own syntax on the way out (see dialect.go).
//
// The safety property is a function of the output type and the sink, not of the
// template language:
//
//   - Structure is unreachable because a Wording emits a string and Go builds every
//     block, assigns every block_id and owns the attachment, colour and emoji.
//   - Length is bounded because every Wording's output goes through the SAME
//     escape-then-truncate sink that already carries upstream annotation text.
//   - Emptiness is bounded because a Wording that renders empty is DISCARDED and
//     the built-in Go text is used instead, so no stanza can be blanked.
//   - Mentions are refused by the Dialect, per provider, because a broadcast ping
//     is spelled differently in each and only the provider knows its own spelling.
//
// Two engines, deliberately. Authoring is STRICT so a typo is refused while a human
// is present to be told; delivery is LAX so a missing field degrades one Stanza
// rather than killing a delivery. Refuse at write time, degrade at render time.
package template
