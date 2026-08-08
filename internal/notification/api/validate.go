package api

import (
	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// validateDraft proves a new policy against the DOMAIN's invariants before it is
// written.
//
// Layer 1 (the DTO tags) has already checked the bounds it can see. This is
// layer 3: it compiles every matcher regex, checks the reason vocabulary and the
// throttle's two-halves rule, and is the reason a bad policy comes back as a
// field-level 422 rather than as a 23514 — a 500 that tells the operator nothing
// about which control is wrong.
func validateDraft(scope db.TenantScope, d domain.PolicyDraft) error {
	return materialise(scope, d).Validate()
}

// materialise folds a draft onto the Policy the write would produce, applying
// the same defaults the repository does. Validating the materialised row rather
// than the draft is what makes the check honest: the DDL sees the row, not the
// request.
func materialise(scope db.TenantScope, d domain.PolicyDraft) domain.Policy {
	p := domain.Policy{
		OrgID:      scope.OrgID(),
		Name:       d.Name,
		Priority:   domain.DefaultPolicyPriority,
		Enabled:    true,
		Matchers:   d.Matchers,
		Reasons:    d.Reasons,
		ChannelIDs: d.ChannelIDs,
	}
	if d.Priority != nil {
		p.Priority = *d.Priority
	}
	if d.Enabled != nil {
		p.Enabled = *d.Enabled
	}
	if d.Throttle != nil {
		p.Throttle = *d.Throttle
	}
	if d.UnackedReminderAfter != nil {
		p.UnackedReminderAfter = *d.UnackedReminderAfter
	}
	return p
}

// validateMerged proves a patch against the row it lands on.
//
// A patch cannot be validated in isolation: `{"channel_ids": []}` is invalid, but
// so is a patch that leaves a policy with no reasons, and only the merged result
// knows which. Applying the patch to a copy of the stored policy and validating
// THAT is the only check that answers the question the database will ask.
func validateMerged(existing domain.Policy, p domain.PolicyPatch) error {
	merged := existing
	if p.Name != nil {
		merged.Name = *p.Name
	}
	if p.Priority != nil {
		merged.Priority = *p.Priority
	}
	if p.Enabled != nil {
		merged.Enabled = *p.Enabled
	}
	if p.Matchers != nil {
		merged.Matchers = *p.Matchers
	}
	if p.Reasons != nil {
		merged.Reasons = *p.Reasons
	}
	if p.ChannelIDs != nil {
		merged.ChannelIDs = *p.ChannelIDs
	}
	if p.Throttle != nil {
		if v := *p.Throttle; v != nil {
			merged.Throttle = *v
		} else {
			merged.Throttle = domain.Throttle{}
		}
	}
	if p.UnackedReminderAfter != nil {
		if v := *p.UnackedReminderAfter; v != nil {
			merged.UnackedReminderAfter = *v
		} else {
			merged.UnackedReminderAfter = 0
		}
	}
	return merged.Validate()
}
