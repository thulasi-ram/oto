package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ⛔ BINDING, PERMANENT (SPEC §G.9.1, SCOPE-BOUNDARY §4.7).
//
// THERE IS EXACTLY ONE STAGE, FOREVER.
//
// `unacked_reminder_after_s` is a SCALAR. This service must never gain a stage
// index, a second threshold, a per-stage target, or any awareness of who is on
// call. The reminder is triggered by THE SIGNAL'S OWN UNACKED DURATION and is
// delivered to THE CHANNELS THE POLICY ALREADY ROUTES TO. It is a fact about the
// signal, not a routing decision about a human.
//
// The path from here to PagerDuty is four small pull requests — an array of
// thresholds, per-stage targets, targets that are people, a rota to resolve the
// person — and each one looks reasonable on its own. That is precisely why the
// refusal is written down at the top of the file that would have to change.
// Widening this needs an ADR that argues against FR-1 by name.

// ReminderService is oto's own clock for unacknowledged signals.
//
// It exists because Alertmanager's `repeat_interval` defaults to FOUR HOURS,
// which is far too slow for an unacknowledged critical, and because "nobody has
// looked at this yet" is a fact about the signal that no upstream system tracks.
type ReminderService struct {
	policies  PolicyStore
	reminders ReminderStore
	notifier  *NotificationService
	batch     int
	clk       clock.Clock
	log       *slog.Logger
}

// ReminderConfig configures the sweep.
type ReminderConfig struct {
	Policies  PolicyStore
	Reminders ReminderStore
	Notifier  *NotificationService
	// Batch caps how many groups one tick considers per org. Zero means 200.
	Batch  int
	Clock  clock.Clock
	Logger *slog.Logger
}

// NewReminderService builds the service.
func NewReminderService(cfg ReminderConfig) (*ReminderService, error) {
	if cfg.Policies == nil || cfg.Reminders == nil || cfg.Notifier == nil {
		return nil, errs.New(errs.KindInternal, "reminder_service_deps",
			"the reminder service needs a policy store, a reminder store and the notification service")
	}
	s := &ReminderService{
		policies: cfg.Policies, reminders: cfg.Reminders, notifier: cfg.Notifier,
		batch: cfg.Batch, clk: cfg.Clock, log: cfg.Logger,
	}
	if s.batch <= 0 {
		s.batch = 200
	}
	if s.clk == nil {
		s.clk = clock.New()
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// Sweep runs one tick across every tenant.
//
// One org's broken policy must not stop the others being reminded, so a failure
// is logged and the sweep continues. The tick repeats in sixty seconds anyway,
// which makes "carry on" strictly better than "abort": aborting would silently
// convert one org's problem into every org's silence.
func (s *ReminderService) Sweep(ctx context.Context) (int, error) {
	orgs, err := s.reminders.ListOrgIDs(ctx)
	if err != nil {
		return 0, err
	}

	total := 0
	for _, orgID := range orgs {
		scope, err := db.NewTenantScope(orgID)
		if err != nil {
			continue
		}
		n, err := s.SweepOrg(ctx, scope)
		if err != nil {
			s.log.ErrorContext(ctx, "notification: the unacked reminder sweep failed for one org",
				"org_id", orgID, "error", err.Error())
			continue
		}
		total += n
	}
	return total, nil
}

// SweepOrg reminds one tenant's channels about signals nobody has acknowledged.
func (s *ReminderService) SweepOrg(ctx context.Context, scope db.TenantScope) (int, error) {
	policies, err := s.policies.ListWithUnackedReminder(ctx, scope)
	if err != nil {
		return 0, err
	}
	if len(policies) == 0 {
		return 0, nil
	}

	// One query serves every policy: the candidate set is "unacknowledged for
	// longer than the SHORTEST threshold anybody asked for", and the per-policy
	// comparison happens below. Running one query per policy would scan the same
	// rows N times to answer the same question.
	shortest := shortestThreshold(policies)
	now := s.clk.Now().UTC()

	groups, err := s.reminders.ListUnackedGroups(ctx, scope, now.Add(-shortest), s.batch)
	if err != nil {
		return 0, err
	}

	sent := 0
	for _, g := range groups {
		policy, ok := firstMatchingPolicy(policies, g.GroupLabels, g.UnackedFor(now))
		if !ok {
			continue
		}
		_ = policy

		// The reminder goes through the ordinary evaluation path. It is not a
		// special send: it is routed by policy, suppressed by a snooze, recorded as
		// a Notification and fanned out like every other fact. A reminder that
		// bypassed suppression would be a reminder that ignored somebody who had
		// explicitly asked for quiet.
		res, err := s.notifier.Evaluate(ctx, scope, Intent{
			GroupID:      g.GroupID,
			Reason:       domain.ReasonUnackedReminder,
			StateVersion: g.StateVersion,
		})
		if err != nil {
			s.log.ErrorContext(ctx, "notification: could not send an unacked reminder",
				"group_id", g.GroupID, "error", err.Error())
			continue
		}
		if res.Created {
			sent++
		}
	}
	return sent, nil
}

// shortestThreshold is the earliest any policy would want to be reminded.
func shortestThreshold(policies []domain.Policy) time.Duration {
	shortest := domain.MaxUnackedReminderAfter
	for _, p := range policies {
		if p.RemindsOnUnacked() && p.UnackedReminderAfter < shortest {
			shortest = p.UnackedReminderAfter
		}
	}
	return shortest
}

// firstMatchingPolicy walks the policies in evaluation order and returns the
// first whose matchers hold AND whose threshold has actually elapsed.
//
// Both conditions, in that order, on the same policy. Taking the threshold from
// one policy and the routing from another would produce a reminder that no
// single rule in the system asked for.
func firstMatchingPolicy(
	policies []domain.Policy, labels map[string]string, unackedFor time.Duration,
) (domain.Policy, bool) {
	for _, p := range policies {
		if !p.RemindsOnUnacked() || unackedFor < p.UnackedReminderAfter {
			continue
		}
		if !p.Handles(domain.ReasonUnackedReminder) {
			// The policy asks for a reminder but does not list the reason, so the
			// evaluation path would suppress it as `no_policy`. Skipping here keeps
			// the two decisions consistent instead of minting an intent that is
			// certain to be suppressed.
			continue
		}
		ok, err := p.Matches(labels)
		if err != nil || !ok {
			continue
		}
		return p, true
	}
	return domain.Policy{}, false
}
