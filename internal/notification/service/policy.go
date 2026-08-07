package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// PolicyService answers ONE question: which destinations does this fact go to?
//
// ⛔ ROUTING IS SIGNAL → DESTINATION. A policy matches on the group's LABELS and
// on the §H.6 Reason, and it resolves to `channels`. It cannot reference a user,
// a team, a schedule, a rotation or a time of day, because none of those things
// exists in `domain.Policy` and adding one is the pull request that turns oto
// into an on-call product (SCOPE-BOUNDARY §5.3, FR-1, H-1).
//
// There is also no clock in the matching path, and that absence is load-bearing:
// a policy whose outcome depends on WHEN it is evaluated is a schedule.
type PolicyService struct {
	policies PolicyStore
	channels ChannelStore
}

// NewPolicyService builds the service.
func NewPolicyService(policies PolicyStore, channels ChannelStore) (*PolicyService, error) {
	if policies == nil || channels == nil {
		return nil, errs.New(errs.KindInternal, "policy_service_deps",
			"a policy store and a channel store are required")
	}
	return &PolicyService{policies: policies, channels: channels}, nil
}

// MatchRequest is one routing question.
type MatchRequest struct {
	Reason domain.Reason
	// Labels are the AlertGroup's `group_labels`. Group labels rather than an
	// individual alert's: the thing being routed is the GROUP's card, and routing
	// two members of one group to two different channels would split a single
	// conversation across two rooms.
	Labels map[string]string
}

// Match is a routing answer.
type Match struct {
	// Policy is the winning policy, or nil when nothing routed this fact.
	Policy *domain.Policy
	// Channels is every destination the winning policy names, live or not.
	Channels []domain.Channel
	// Live is the subset that may actually receive a delivery.
	Live []domain.Channel
}

// Routed reports whether a policy claimed this fact.
func (m Match) Routed() bool { return m.Policy != nil }

// Deliverable reports whether at least one destination can receive it.
func (m Match) Deliverable() bool { return len(m.Live) > 0 }

// Evaluate walks the live policies in priority order and STOPS AT THE FIRST
// MATCH.
//
// First-match rather than all-match is what the schema says — `notifications`
// carries a single `policy_id` — and it is the right rule for a human: an
// operator debugging "why did this go to #general?" has one policy to read, not
// a set union. It also makes priority mean something, which an all-match design
// would not.
//
// A fact that matches nothing is NOT an error and NOT a silent drop. It comes
// back with a nil Policy, and the caller records a Notification with
// `suppressed_reason = no_policy` — a row an operator can find and act on.
func (s *PolicyService) Evaluate(
	ctx context.Context, scope db.TenantScope, req MatchRequest,
) (Match, error) {
	if !req.Reason.Valid() {
		return Match{}, errs.Validation("unknown_reason", "unknown notification reason",
			errs.Violation{Field: "reason", Code: "enum", Message: string(req.Reason)})
	}

	policies, err := s.policies.ListLive(ctx, scope)
	if err != nil {
		return Match{}, err
	}

	for i := range policies {
		p := policies[i]
		if !p.Handles(req.Reason) {
			continue
		}
		ok, err := p.Matches(req.Labels)
		if err != nil {
			// A policy with a broken regex must not be able to stop evaluation for
			// every alert in the org. Skip it and let the next one be considered;
			// the validation error is already surfaced when the policy is saved.
			continue
		}
		if !ok {
			continue
		}

		all, err := s.channels.ListByIDs(ctx, scope, p.ChannelIDs)
		if err != nil {
			return Match{}, err
		}
		live := make([]domain.Channel, 0, len(all))
		for _, c := range all {
			if c.Live() {
				live = append(live, c)
			}
		}
		return Match{Policy: &p, Channels: all, Live: live}, nil
	}

	return Match{}, nil
}

// PreviewRequest is a DRY RUN of the routing decision.
//
// Exactly one of PolicyID or Policy may be set. Neither means "walk the live set
// as an evaluation would".
type PreviewRequest struct {
	Reason domain.Reason
	Labels map[string]string
	// PolicyID previews one stored policy in isolation, ignoring priority.
	PolicyID *uuid.UUID
	// Policy previews a CANDIDATE that has not been saved. This is the important
	// case: an operator should be able to find out that their new matcher routes
	// every critical to #random BEFORE it does.
	Policy *domain.Policy
}

// PreviewOutcome is one policy's verdict during a dry run.
type PreviewOutcome struct {
	PolicyID   uuid.UUID
	PolicyName string
	Priority   int
	Matched    bool
	// Verdict is a short, stable label: "matched", "reason_not_handled",
	// "matcher_failed", "matcher_invalid", "not_reached".
	Verdict string
	// FailedMatcher names the first matcher that did not hold, so the operator is
	// told WHICH clause is wrong rather than that the policy "did not match".
	FailedMatcher string
}

// Preview is the full dry-run answer.
type Preview struct {
	// Outcomes is every policy considered, in evaluation order, including the
	// ones after the winner, marked "not_reached". Seeing what the first match
	// SHADOWED is usually the actual question being asked.
	Outcomes []PreviewOutcome
	Matched  *domain.Policy
	// Destinations are the resolved channels of the winner.
	Destinations []PreviewDestination
	// Suppressed is the reason nothing would be sent, or "" if something would.
	// It is computed from the routing layer only: a dry run does NOT consult a
	// snooze, a storm or a throttle, because those are properties of a MOMENT and
	// a preview that changed its answer between two clicks would be worse than no
	// preview at all.
	Suppressed domain.SuppressedReason
}

// PreviewDestination is what one channel would receive.
type PreviewDestination struct {
	ChannelID   uuid.UUID
	ChannelName string
	ChannelType domain.ChannelType
	Live        bool
	// Modes is the §H.6 decision for this destination.
	Modes []domain.Mode
	// ReplyDropReason explains a dropped thread reply: "verbosity",
	// "thread_updates", "no_threading".
	ReplyDropReason string
}

// Preview runs the routing decision WITHOUT creating anything.
//
// It writes no row, enqueues no job and opens no destination. That is the whole
// contract: an operator has to be able to ask "where would this go?" of a
// production system, repeatedly, at no cost and with no risk of a message
// appearing in a channel.
func (s *PolicyService) Preview(
	ctx context.Context, scope db.TenantScope, req PreviewRequest,
) (Preview, error) {
	if !req.Reason.Valid() {
		return Preview{}, errs.Validation("unknown_reason", "unknown notification reason",
			errs.Violation{Field: "reason", Code: "enum", Message: string(req.Reason)})
	}

	candidates, err := s.previewCandidates(ctx, scope, req)
	if err != nil {
		return Preview{}, err
	}

	var out Preview
	for i := range candidates {
		p := candidates[i]
		outcome := PreviewOutcome{
			PolicyID: p.ID, PolicyName: p.Name, Priority: p.Priority,
		}

		switch {
		case out.Matched != nil:
			outcome.Verdict = "not_reached"
		case !p.Handles(req.Reason):
			outcome.Verdict = "reason_not_handled"
		default:
			failed, invalid, matched := evaluateMatchers(p, req.Labels)
			switch {
			case invalid:
				outcome.Verdict, outcome.FailedMatcher = "matcher_invalid", failed
			case !matched:
				outcome.Verdict, outcome.FailedMatcher = "matcher_failed", failed
			default:
				outcome.Verdict, outcome.Matched = "matched", true
				winner := p
				out.Matched = &winner
			}
		}
		out.Outcomes = append(out.Outcomes, outcome)
	}

	if out.Matched == nil {
		out.Suppressed = domain.SuppressedNoPolicy
		return out, nil
	}

	channels, err := s.channels.ListByIDs(ctx, scope, out.Matched.ChannelIDs)
	if err != nil {
		return Preview{}, err
	}

	anyLive, anyDelivery := false, false
	for _, c := range channels {
		plan := domain.PlanFor(domain.PlanInput{
			Reason:        req.Reason,
			Verbosity:     c.EffectiveVerbosity(),
			ThreadUpdates: c.ThreadUpdates,
			Capabilities:  c.Capabilities,
			// A preview describes the FIRST notification to a destination, so it
			// assumes no thread yet and no damper engaged. Anything else would make
			// the answer depend on live state the operator cannot see.
		})
		if c.Live() {
			anyLive = true
			anyDelivery = anyDelivery || !plan.Empty()
		}
		out.Destinations = append(out.Destinations, PreviewDestination{
			ChannelID:       c.ID,
			ChannelName:     c.Name,
			ChannelType:     c.Type,
			Live:            c.Live(),
			Modes:           plan.Modes,
			ReplyDropReason: plan.ReplyDropReason,
		})
	}

	switch {
	case !anyLive:
		out.Suppressed = domain.SuppressedChannelDisabled
	case !anyDelivery:
		out.Suppressed = domain.SuppressedVerbosity
	}
	return out, nil
}

// previewCandidates resolves which policies a dry run should consider.
func (s *PolicyService) previewCandidates(
	ctx context.Context, scope db.TenantScope, req PreviewRequest,
) ([]domain.Policy, error) {
	switch {
	case req.Policy != nil:
		p := *req.Policy
		p.OrgID = scope.OrgID()
		if err := p.Validate(); err != nil {
			return nil, err
		}
		return []domain.Policy{p}, nil

	case req.PolicyID != nil:
		p, err := s.policies.Get(ctx, scope, *req.PolicyID)
		if err != nil {
			return nil, err
		}
		return []domain.Policy{p}, nil

	default:
		return s.policies.ListLive(ctx, scope)
	}
}

// evaluateMatchers runs a policy's matchers and reports the FIRST one that
// failed, so a preview can point at a clause instead of shrugging.
func evaluateMatchers(p domain.Policy, labels map[string]string) (failed string, invalid, matched bool) {
	for _, m := range p.Matchers {
		ok, err := m.Matches(labels)
		if err != nil {
			return m.Name, true, false
		}
		if !ok {
			return m.Name, false, false
		}
	}
	return "", false, true
}
