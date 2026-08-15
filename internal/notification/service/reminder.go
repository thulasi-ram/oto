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
	settings  SettingsReader
	batch     int
	clk       clock.Clock
	log       *slog.Logger
}

// ReminderConfig configures the sweep.
type ReminderConfig struct {
	Policies  PolicyStore
	Reminders ReminderStore
	Notifier  *NotificationService
	// Settings reads the org's default reminder delay. OPTIONAL: nil means only a
	// policy's own `unacked_reminder_after_s` can produce a reminder, which is
	// exactly the behaviour that shipped.
	Settings SettingsReader
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
		settings: cfg.Settings, batch: cfg.Batch, clk: cfg.Clock, log: cfg.Logger,
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

// SweepOrg reminds one tenant's channels about signals nobody has acknowledged.
//
// It is one tenant's WHOLE tick, deliberately: the worker fans the periodic out
// into one of these per live tenant (jobs.TenantFanOut), so there is no
// all-tenants loop here for one org's broken policy to hide inside — a tenant
// that fails fails alone, on its own retry budget, and stops nobody else.
func (s *ReminderService) SweepOrg(ctx context.Context, scope db.TenantScope) (int, error) {
	// The org's fallback delay, for policies that name none of their own. It is
	// read fresh on every tick — sixty seconds is the longest a change to it can
	// take to bind, and there is no cache in front of it to make that longer.
	orgDefault := s.orgReminderDefault(ctx, scope)

	policies, err := s.policies.ListWithUnackedReminder(ctx, scope, orgDefault)
	if err != nil {
		return 0, err
	}
	if len(policies) == 0 {
		return 0, nil
	}
	if orgDefault != nil {
		// The SQL admitted these rows on the strength of the org default; give
		// them that value, so the per-policy comparison below is asking about the
		// delay that is actually in force. A policy with its own delay keeps it.
		fallback := time.Duration(*orgDefault) * time.Second
		for i := range policies {
			if policies[i].UnackedReminderAfter <= 0 {
				policies[i].UnackedReminderAfter = fallback
			}
		}
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

// orgReminderDefault reads the org's fallback reminder delay in SECONDS, or nil
// when the org sets none.
//
// ⛔ IT NEVER FAILS THE SWEEP. A settings lookup that could stop a reminder would
// turn an unreadable settings row into silence on an unacknowledged critical,
// which is the one outcome this service exists to prevent. On any error the
// answer is nil — the pre-existing behaviour, where only a policy's own delay
// produces a reminder.
func (s *ReminderService) orgReminderDefault(ctx context.Context, scope db.TenantScope) *int {
	if s.settings == nil {
		return nil
	}
	def, err := s.settings.NotificationDefaults(ctx, scope)
	if err != nil {
		s.log.WarnContext(ctx, "notification: could not read the org reminder default",
			"org_id", scope.OrgID(), "error", err.Error())
		return nil
	}
	if def.UnackedReminderAfter <= 0 {
		return nil
	}
	secs := int(def.UnackedReminderAfter / time.Second)
	return &secs
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
