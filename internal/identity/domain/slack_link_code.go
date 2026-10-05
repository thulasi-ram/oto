package domain

// A SLACK MEMBER LINKS THEMSELVES TO AN oto USER WITH A CODE oto SHOWED ONLY THEM (git-bug a556a5c,
// the ruling of 2026-10-05: SELF-SERVICE IN THE UI, not a host-shell command).
//
// ⭐⭐ A LINK DECIDES WHOSE APPROVAL A SLACK CLICK COUNTS AS. A Remedy's double approval (ADR 0054
// §4) counts DIFFERENT oto users, and a Slack press is applied as the user its member is linked to —
// so a wrong link lets one person's clicks count as another's approval. Everything in this file and
// the path that uses it fails closed.
//
// ⭐ THE FLOW STARTS IN SLACK BECAUSE ONLY SLACK CAN PROVE A SLACK IDENTITY. oto verifies the HMAC on
// every interaction payload; when an UNLINKED member presses a Remedy button, the ephemeral reply —
// seen only by that member — carries a code bound to the verified `(org, team_id, user_id)`. The
// member then signs in to oto and enters it on their own Account page, which only ever links the
// SIGNED-IN user. oto therefore learns both halves from the side that can prove each: Slack proves
// the member, oto's session proves the person.
//
// ⛔ THE CODE IS A CREDENTIAL. Anybody who enters your code in THEIR oto session makes YOUR Slack
// clicks count as THEM. The mitigations are the confirmation screen (which names the Slack member and
// workspace before anything is written), the ten-minute life, single use, the recorded fact of every
// link and unlink, and the refusal to move a link that already names another real person.

import (
	"crypto/sha256"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

const (
	// SlackLinkCodeLength is how many Crockford base32 characters a code has: 10 × 5 = 50 bits of
	// crypto/rand, comfortably above the ≥ 40 the design asks for. It is short because a person
	// types it; it is safe because it lives ten minutes, is single-use and wrong guesses are
	// limited per user — at five wrong guesses per fifteen minutes, guessing ANY live code is
	// odds of roughly (live codes) × 5 / 2^50 per window.
	SlackLinkCodeLength = 10
	// SlackLinkCodeTTL is how long a code lives, on the APPLICATION clock (CONTEXT.md §5.2).
	SlackLinkCodeTTL = 10 * time.Minute
	// SlackLinkCodeMaxPresentations is the PER-CODE limit: how many times one live code may be
	// presented (previewed or confirmed) before it is dead. A person needs two — a preview and a
	// confirm — and a few reloads; a code passed around is burned rather than probed.
	SlackLinkCodeMaxPresentations = 5
	// SlackLinkWrongAttemptLimit is the PER-USER limit: wrong, used or expired codes one signed-in
	// user may present inside SlackLinkWrongAttemptWindow before every further attempt is a 429.
	SlackLinkWrongAttemptLimit = 5
	// SlackLinkWrongAttemptWindow is the window SlackLinkWrongAttemptLimit counts over.
	SlackLinkWrongAttemptWindow = 15 * time.Minute
)

// slackLinkAlphabet is Crockford's base32: no I, L, O or U, so nothing a person reads off a
// screen can be mistyped as something else.
const slackLinkAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// SlackLinkCodeInvalidCode is the ONE refusal a presented code gets when it does not name a live
// code: wrong, already used, expired, burned by too many presentations or from another org. One
// code and one sentence for all five, because a distinction a caller can observe is an oracle it
// can query (the same reasoning as `service.unauthenticated`).
const SlackLinkCodeInvalidCode = "slack_link_code_invalid"

// SlackLinkCodeInvalid is that refusal.
func SlackLinkCodeInvalid() error {
	return errs.Validation(SlackLinkCodeInvalidCode,
		"that link code is wrong, already used or expired; press the Remedy button in Slack again for a new one")
}

// SlackIdentityLinkedElsewhereCode is the refusal of a link whose Slack member already resolves
// to ANOTHER real oto user.
const SlackIdentityLinkedElsewhereCode = "slack_identity_linked_elsewhere"

// SlackIdentityLinkedElsewhere is a 409: ⛔ a link to another real person is NEVER moved, by this
// path or any other. The person who holds it unlinks it from their own Account page first.
func SlackIdentityLinkedElsewhere() error {
	return errs.Conflict(SlackIdentityLinkedElsewhereCode,
		"this Slack account is already linked to another oto user; they must unlink it from their own Account page first")
}

// SlackLinkAttemptsExhaustedCode is the per-user 429.
const SlackLinkAttemptsExhaustedCode = "slack_link_attempts_exhausted"

// SlackLinkCode is a code as a person types it, normalised. If you can construct one, it is ten
// characters of the alphabet — which says nothing about whether any live code has it.
type SlackLinkCode struct{ v string }

// NewSlackLinkCode mints a code from r (crypto/rand in production).
//
// Each character is drawn from one byte by REJECTION, never by `b % 32`: 256 is a multiple of 32,
// so the modulo would be unbiased here too, but the rejection form states the property instead of
// relying on an arithmetic coincidence a future alphabet change would quietly break.
func NewSlackLinkCode(r io.Reader) (SlackLinkCode, error) {
	n := len(slackLinkAlphabet)
	limit := 256 - 256%n
	out := make([]byte, 0, SlackLinkCodeLength)
	buf := make([]byte, SlackLinkCodeLength*2)
	for len(out) < SlackLinkCodeLength {
		if _, err := io.ReadFull(r, buf); err != nil {
			return SlackLinkCode{}, errs.Internal("slack_link_code_entropy", err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, slackLinkAlphabet[int(b)%n])
			if len(out) == SlackLinkCodeLength {
				break
			}
		}
	}
	return SlackLinkCode{v: string(out)}, nil
}

// ParseSlackLinkCode reads a code a person typed: case, spaces and hyphens are ignored, and the
// letters Crockford says are misreadings are read as the digit meant (I and L as 1, O as 0).
// Anything else that is not exactly ten alphabet characters is SlackLinkCodeInvalid — the same
// refusal as a wrong code, and counted as one.
func ParseSlackLinkCode(raw string) (SlackLinkCode, error) {
	var b strings.Builder
	for _, r := range strings.ToUpper(raw) {
		switch r {
		case ' ', '-', '\t':
			continue
		case 'I', 'L':
			r = '1'
		case 'O':
			r = '0'
		}
		if r > 127 || !strings.ContainsRune(slackLinkAlphabet, r) {
			return SlackLinkCode{}, SlackLinkCodeInvalid()
		}
		b.WriteRune(r)
		if b.Len() > SlackLinkCodeLength {
			return SlackLinkCode{}, SlackLinkCodeInvalid()
		}
	}
	if b.Len() != SlackLinkCodeLength {
		return SlackLinkCode{}, SlackLinkCodeInvalid()
	}
	return SlackLinkCode{v: b.String()}, nil
}

// Display is the code as Slack shows it: two groups of five, `ABCDE-FGHJK`.
func (c SlackLinkCode) Display() string {
	if len(c.v) != SlackLinkCodeLength {
		return ""
	}
	return c.v[:5] + "-" + c.v[5:]
}

// Hash is the sha256 of the normalised code — the ONLY form oto stores. sha256 rather than
// argon2id for `service.digest`'s reason, scaled down: the input is 50 bits of crypto/rand that
// lives ten minutes and dies on first use, so a work factor would tax every presentation and
// protect nothing that outlives the code. The lookup is by hash, so no comparison in oto ever
// runs over the plaintext.
func (c SlackLinkCode) Hash() (TokenHash, error) {
	if len(c.v) != SlackLinkCodeLength {
		return TokenHash{}, SlackLinkCodeInvalid()
	}
	sum := sha256.Sum256([]byte(c.v))
	return NewTokenHash(sum[:])
}

// String redacts: a code is a credential for its ten minutes and has no business in a log line.
func (c SlackLinkCode) String() string { return "[redacted]" }

// SlackLinkChange is what one recorded fact did to a link.
type SlackLinkChange string

const (
	// SlackLinkLinked: a member was linked to the signed-in user by a code.
	SlackLinkLinked SlackLinkChange = "linked"
	// SlackLinkUnlinked: the signed-in user unlinked a member linked to them.
	SlackLinkUnlinked SlackLinkChange = "unlinked"
)

// SlackLinkFact is the recorded fact of one link or unlink (`slack_identity_links`).
type SlackLinkFact struct {
	ID         uuid.UUID
	OrgID      uuid.UUID
	IdentityID uuid.UUID
	// TeamID and SlackUserID are copied so the fact reads without a join.
	TeamID      SlackTeamID
	SlackUserID SlackUserID
	Change      SlackLinkChange
	// UserID is who the member now resolves to (linked) or no longer does (unlinked).
	UserID uuid.UUID
	// DisplacedUserID is the SHADOW member a link retired; zero when none was displaced.
	DisplacedUserID uuid.UUID
	// ActorID is the signed-in user who did it. On the self-service path it is always UserID.
	ActorID uuid.UUID
	At      time.Time
}

// NewSlackLinkFact builds a fact, refusing one that names nobody.
func NewSlackLinkFact(id uuid.UUID, si SlackIdentity, change SlackLinkChange, userID, displaced, actor uuid.UUID, at time.Time) (SlackLinkFact, error) {
	if id == uuid.Nil || si.ID == uuid.Nil || si.OrgID == uuid.Nil || userID == uuid.Nil || actor == uuid.Nil || at.IsZero() {
		return SlackLinkFact{}, errs.Validation("invalid_slack_link_fact", "a link fact names the identity, the user, the actor and a time")
	}
	if change != SlackLinkLinked && change != SlackLinkUnlinked {
		return SlackLinkFact{}, errs.Validation("invalid_slack_link_fact", "a link fact is linked or unlinked")
	}
	if change == SlackLinkUnlinked && displaced != uuid.Nil {
		return SlackLinkFact{}, errs.Validation("invalid_slack_link_fact", "an unlink displaces nobody")
	}
	return SlackLinkFact{
		ID: id, OrgID: si.OrgID, IdentityID: si.ID, TeamID: si.TeamID, SlackUserID: si.SlackUserID,
		Change: change, UserID: userID, DisplacedUserID: displaced, ActorID: actor, At: at.UTC(),
	}, nil
}
