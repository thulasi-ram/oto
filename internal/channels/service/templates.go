package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/template"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// TemplateStore is the read side this service needs. Declared here, satisfied by
// channels/repository — the service names the port, the repository satisfies it.
type TemplateStore interface {
	// ForPolicy reads the live NotificationTemplate a policy names, or nothing.
	ForPolicy(ctx context.Context, s db.TenantScope, policyID uuid.UUID) (domain.NotificationTemplate, bool, error)
}

// Templates reads the NotificationTemplate a delivery was routed with.
type Templates struct {
	store TemplateStore
}

// NewTemplates builds the resolver.
func NewTemplates(store TemplateStore) *Templates { return &Templates{store: store} }

// For returns the template the given policy names, or nil.
//
// ⭐ IT IS A FOREIGN KEY READ AND NOT A SEARCH, WHICH IS THE POINT OF MOVING
// SELECTION ONTO THE POLICY. The predecessor walked an ordered candidate list
// evaluating a second matcher vocabulary with a second precedence rule, and an
// operator had to hold both in their head to predict a card. A policy already
// decided who gets notified; it now also says what that looks like.
//
// ⛔ IT SWALLOWS ITS ERRORS ON PURPOSE. A presentation lookup must never be able
// to fail a delivery: an unreadable store yields no template, the renderer builds
// oto's own card, and the alert goes out reading in the default voice. A card
// that reads plainly is a small loss; a card that does not arrive because a
// formatting table was unavailable is not.
func (t *Templates) For(ctx context.Context, s db.TenantScope, policyID uuid.UUID) *domain.TemplateRef {
	if t == nil || t.store == nil {
		return nil
	}
	row, ok, err := t.store.ForPolicy(ctx, s, policyID)
	if err != nil || !ok || !row.Live() {
		return nil
	}
	return &domain.TemplateRef{
		ID:      row.ID.String(),
		Version: row.Version,
		Format:  row.Format,
		Source:  row.Source,
	}
}

// ValidateTemplate is the SAVE-TIME gate, in full: the domain's rules and the
// engine's, in the order an author would want to hear them.
//
// ⛔ IT RENDERS, BECAUSE PARSING PROVES ALMOST NOTHING. `{{ x | no_such_filter }}`
// parses cleanly in Liquid and fails at render, so a save that only parsed would
// accept it and discover it at 03:00 on a real card. The engine executes the
// template against the whole fixture corpus, including the hostile cases.
//
// ⚠️ WARNINGS COME BACK AS VIOLATIONS TOO, AND THE CALLER MUST TELL THEM APART.
// `template.Blocking` is that test. The one warning today is a card with no
// `{{ actions }}`: the operator is allowed to ship a card with no Acknowledge
// button, and oto's job is to make sure they know they did.
func ValidateTemplate(name, provider, format, source string) []errs.Violation {
	var out []errs.Violation
	if err := domain.ValidateNotificationTemplate(name, provider, format, source); err != nil {
		return []errs.Violation{{Field: "source", Code: "invalid", Message: err.Error()}}
	}
	for _, p := range template.Validate(template.Format(format), source) {
		v := errs.Violation{Field: "source", Code: string(p.Kind), Message: p.Message}
		if p.Fixture != "" {
			v.Message = p.Message + " (on the " + p.Fixture + " example)"
		}
		out = append(out, v)
	}
	return out
}

// TemplateWarningsOnly reports whether a gate verdict blocks a save.
func TemplateWarningsOnly(vs []errs.Violation) bool {
	for _, v := range vs {
		if v.Code != string(template.ProblemWarning) {
			return false
		}
	}
	return true
}
