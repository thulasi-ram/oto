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
		// The binding is not a pointer and has no default to apply: an empty binding
		// IS the column default, so materialising is a copy.
		Subjects: d.Subjects,
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
	// The digest's four rules — range, the divisor rule, the pair rule and the
	// reason rule — are all `Policy.Validate`'s, so materialising the two halves is
	// the whole of this layer's job. A hand-written range check here would be the
	// second copy that drifts from `policies_digest_window_ck`.
	if d.DigestWindow != nil {
		p.Digest.Window = *d.DigestWindow
	}
	if d.DigestFloor != nil {
		p.Digest.Floor = *d.DigestFloor
	}
	// The count condition's four rules — the two ranges, the symmetric pair rule and
	// the unit rule that ties it to a one-element binding — are all
	// `Policy.Validate`'s, so materialising the two halves is the whole of this
	// layer's job. A hand-written range check here would be the second copy that
	// drifts from `policies_count_min_ck`.
	if d.CountMin != nil {
		p.Count.Min = *d.CountMin
	}
	if d.CountWindow != nil {
		p.Count.Window = *d.CountWindow
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
	// Run FIRST, because the fold below is lossy: it collapses an explicit
	// `digest_floor: 0` onto the same `Floor = 0` that means "unset", and the
	// merged policy can no longer tell the two apart. See PolicyPatch.ValidateExplicit.
	if err := p.ValidateExplicit(); err != nil {
		return err
	}
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
	// ⭐ THE DIGEST IS WHY MERGING RATHER THAN VALIDATING THE PATCH MATTERS MOST.
	// `policies_digest_reason_ck` ties the window to the `digest` Reason and
	// `policies_digest_pair_ck` ties the floor to the window, so `{"digest_floor":
	// 5}` and `{"reasons": ["fired"]}` are each valid alone and each fatal against
	// the wrong stored row. Only the merged policy knows which.
	if p.DigestWindow != nil {
		if v := *p.DigestWindow; v != nil {
			merged.Digest.Window = *v
		} else {
			merged.Digest.Window = 0
		}
	}
	if p.DigestFloor != nil {
		if v := *p.DigestFloor; v != nil {
			merged.Digest.Floor = *v
		} else {
			merged.Digest.Floor = 0
		}
	}
	// ⭐ AND THE SUBJECT BINDING IS THE STRONGEST CASE YET FOR MERGING RATHER THAN
	// VALIDATING THE PATCH. It is cross-checked against BOTH of the other two axes:
	// `validateSubjects` refuses a binding that admits none of the policy's Reasons,
	// and `policies_count_subject_ck` refuses a count condition without exactly one
	// kind. So `{"subject_kinds": ["alert"]}` alone is valid, and it is fatal against
	// a stored policy whose reasons are all case-scoped, and fatal again against one
	// carrying a count condition — three requests that are each individually
	// well-formed and only the merged row knows which of them is being made.
	if p.Subjects != nil {
		merged.Subjects = *p.Subjects
	}
	if p.CountMin != nil {
		if v := *p.CountMin; v != nil {
			merged.Count.Min = *v
		} else {
			merged.Count.Min = 0
		}
	}
	if p.CountWindow != nil {
		if v := *p.CountWindow; v != nil {
			merged.Count.Window = *v
		} else {
			merged.Count.Window = 0
		}
	}
	return merged.Validate()
}
