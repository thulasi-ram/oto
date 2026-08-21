package service

import (
	"context"
	"regexp"
	"sync"

	"github.com/google/uuid"
	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/render/wording"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// WordingStore is the read side this service needs. Declared here, satisfied by
// channels/repository — the service names the port, the repository satisfies it.
type WordingStore interface {
	Resolve(ctx context.Context, s db.TenantScope, channelID uuid.UUID) ([]domain.Wording, error)
}

// Wordings resolves which customer template, if any, writes each Stanza of one
// delivery.
type Wordings struct {
	store WordingStore
}

// NewWordings builds the resolver.
func NewWordings(store WordingStore) *Wordings { return &Wordings{store: store} }

// For returns the winning template per Stanza for one destination and one view.
//
// ⭐ EXACTLY ONE WORDING WINS PER STANZA, AND THERE IS NOTHING TO MERGE. The store
// returns candidates already in precedence order — this destination's own rows
// before the org-wide house voice, and within each by priority LOWER FIRST — so
// this walks once and takes the first whose `when` clause matches. That is the
// same "first match wins and no other is consulted" sentence routing already uses,
// and using a second, different resolution rule for presentation is how an
// operator learns to distrust both (ADR 0049).
//
// ⛔ A FAILURE HERE IS NEVER A FAILED DELIVERY. If the store cannot be read, the
// caller gets no wordings and an empty map, and every Stanza falls back to oto's
// own Go text. A card that reads in oto's default voice is a small loss; a card
// that does not arrive because a presentation table was unavailable is not.
func (w *Wordings) For(
	ctx context.Context, s db.TenantScope, channelID uuid.UUID, v *domain.NotificationView,
) map[string]string {
	if w == nil || w.store == nil || v == nil || channelID == uuid.Nil {
		return nil
	}
	candidates, err := w.store.Resolve(ctx, s, channelID)
	if err != nil || len(candidates) == 0 {
		return nil
	}

	labels := effectiveLabels(v)
	out := make(map[string]string, len(domain.WordableStanzas))
	for _, c := range candidates {
		if _, taken := out[c.Stanza]; taken {
			continue // an earlier, more specific or higher-priority row already won
		}
		if !c.Live() || !domain.StanzaTakesAWording(c.Stanza) {
			continue
		}
		if !matches(c, v.Reason, labels) {
			continue
		}
		out[c.Stanza] = c.Template
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// effectiveLabels is what a `when` clause matches against.
//
// The focused alert's own labels win over the group's, because a Wording written
// for `service="checkout"` means the alert in front of the reader, and a group
// label is the coarser fact. Both are present so a clause can name either.
func effectiveLabels(v *domain.NotificationView) map[string]string {
	out := make(map[string]string, len(v.Group.GroupLabels)+8)
	for k, val := range v.Group.GroupLabels {
		out[k] = val
	}
	focus := v.Focus
	if focus == nil && len(v.Alerts) > 0 {
		focus = &v.Alerts[0]
	}
	if focus != nil {
		for k, val := range focus.Labels {
			out[k] = val
		}
		// The four oto names an operator will reach for without thinking of them as
		// labels. They are added only when the signal did not carry a real label of
		// the same name, so upstream always wins its own vocabulary.
		for k, val := range map[string]string{
			"alertname": focus.AlertName, "severity": focus.Severity,
			"service": focus.Service, "namespace": focus.Namespace,
		} {
			if _, ok := out[k]; !ok && val != "" {
				out[k] = val
			}
		}
	}
	return out
}

// matches reports whether a Wording's `when` clause selects this delivery.
//
// An EMPTY clause matches everything, which is what makes one org-wide row the
// natural way to set a house voice. Both halves must hold: reasons narrow WHICH
// FACT, matchers narrow WHICH SIGNAL.
func matches(c domain.Wording, reason string, labels map[string]string) bool {
	if len(c.Reasons) > 0 {
		found := false
		for _, r := range c.Reasons {
			if r == reason {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, m := range c.Matchers {
		if !matchOne(m, labels[m.Name]) {
			return false
		}
	}
	return true
}

func matchOne(m domain.Matcher, value string) bool {
	switch m.Op {
	case domain.MatchEq:
		return value == m.Value
	case domain.MatchNotEq:
		return value != m.Value
	case domain.MatchRe:
		re, err := anchored(m.Value)
		return err == nil && re.MatchString(value)
	case domain.MatchNotRe:
		re, err := anchored(m.Value)
		// ⛔ AN UNCOMPILABLE PATTERN DOES NOT MATCH, EVEN NEGATED. Returning true
		// here would make a broken `!~` silently select every delivery, which is
		// the loudest possible failure for the quietest possible mistake.
		return err == nil && !re.MatchString(value)
	}
	return false
}

var (
	reCacheMu sync.RWMutex
	reCache   = map[string]*regexp.Regexp{}
)

// anchored compiles a matcher pattern as FULLY ANCHORED, which is what
// Alertmanager's `=~` means and therefore what an operator expects. An unanchored
// `=~ "prod"` matching "not-production" would be a trap.
func anchored(pattern string) (*regexp.Regexp, error) {
	reCacheMu.RLock()
	re, ok := reCache[pattern]
	reCacheMu.RUnlock()
	if ok {
		return re, nil
	}
	re, err := regexp.Compile(`\A(?:` + pattern + `)\z`)
	if err != nil {
		return nil, err
	}
	reCacheMu.Lock()
	reCache[pattern] = re
	reCacheMu.Unlock()
	return re, nil
}

// ValidateWording is the full save-time gate: the domain's shape rules, then the
// template engine's, which must RENDER to find out.
//
// ⚠️ RENDERING IS NOT OPTIONAL. An unknown filter is a render-time error in Liquid,
// not a parse-time one, so a save that only parsed would accept
// `{{ x | no_such_filter }}` and discover it on a real card at 03:00.
func ValidateWording(stanza, template string, matchers []domain.Matcher, reasons []string, priority int) []errs.Violation {
	v := domain.ValidateWording(stanza, template, matchers, reasons, priority)
	if len(v) > 0 {
		// The engine cannot say anything useful about a template aimed at a stanza
		// that takes none, or one over the size ceiling.
		return v
	}
	for _, p := range wording.Validate(wording.StanzaID(stanza), template) {
		v = append(v, errs.Violation{
			Field:   "template",
			Code:    string(p.Kind),
			Message: problemMessage(p),
		})
	}
	return v
}

func problemMessage(p wording.Problem) string {
	if p.Fixture == "" {
		return p.Message
	}
	// Naming the fixture is the difference between "it broke" and "it breaks on a
	// digest", which is the sentence that tells an author what to write a default
	// for.
	return p.Message + " (on a " + p.Fixture + " notification)"
}
