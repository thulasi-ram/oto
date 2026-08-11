package domain_test

import (
	"testing"
	"time"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
	lifecycle "github.com/thulasiram/oto/internal/alerts/service"
	grouping "github.com/thulasiram/oto/internal/grouping/domain"
	identity "github.com/thulasiram/oto/internal/identity/domain"
	sources "github.com/thulasiram/oto/internal/sources/domain"
)

// ⛔⛔ THIS FILE IS THE ARITHMETIC BEHIND FOUR DEFAULTS, EXECUTED.
//
// `refire_grace` and the flap thresholds used to be guesses — `docs/ORCHESTRATION.md`
// listed them as needing the owner, and the numbers had acquired bounds, a config
// layer and a page of guidance without anybody checking them against a real
// cluster. ADR 0026 replaced the guesses with a derivation from a measured corpus.
//
// A derivation written only in prose rots: the next person moves a constant, the
// prose still claims it was derived, and nothing notices. So the corpus lives here
// as constants with their sources, and every default is RECOMPUTED from it. Change
// a default without changing the reasoning and this file fails.
//
// ⚠️ These are test-only cross-package imports, the same idiom as
// `refire_grace_replay_test.go`: `identity/domain` is a settings vocabulary and
// must not depend on the lifecycle, the grouping engine or the source client at
// build time. The invariant lives in a test instead.

// ---------------------------------------------------------------- the corpus

// EcosystemGroupInterval is the `group_interval` essentially every real
// Alertmanager runs, and it is the one route timing the ecosystem does NOT
// override: Alertmanager's own `dispatch.DefaultRouteOpts`, kube-prometheus-stack's
// `values.yaml`, kube-prometheus's `alertmanager-secret.yaml`, OpenShift's
// cluster-monitoring-operator and Grafana Alerting's notification-policy defaults
// all ship `5m`. (`repeat_interval` moves constantly — the Kubernetes ecosystem
// converged on `12h` against upstream's `4h` — and `group_wait` moves on
// per-severity child routes. `group_interval` does not move.) See ADR 0026 §1.
const EcosystemGroupInterval = 5 * time.Minute

// ModalRuleFor is the `for:` of the modal real alerting rule. Across all 155
// alerting rules kube-prometheus-stack 88.2.0 ships — kubernetes-mixin, the
// node-exporter mixin, and the Prometheus/Alertmanager/etcd/kube-state-metrics
// mixins — `15m` is both the MODE (69 rules, 44.5%) and the MEDIAN; `15m`+`10m`+`5m`
// together are 75.5% of the corpus. oto's own `deploy/prometheus/oto-rules.yaml`
// agrees: `15m` is the mode of its non-instantaneous rules. See ADR 0026 §1.
const ModalRuleFor = 15 * time.Minute

// FastCaptureGroupInterval is the counter-case, and it is a REAL capture rather
// than an invention: `sources/client/alertmanager/testdata/compose_v0.28.1.yaml`,
// taken from a live `prom/alertmanager:v0.28.1`. Every bound below is checked
// against it as well as against the ecosystem value, because a bound that excludes
// a value a real cluster needs is as much a defect as one that admits a bad value.
const FastCaptureGroupInterval = 30 * time.Second

// observableCycle is how long one fire → resolve → fire cycle takes to be
// OBSERVED, and it yields exactly two counted transitions
// (`occurrence.opened|reopened` plus `occurrence.resolved|expired`).
//
// ⛔ THE `max` IS THE TERM THE OLD ARITHMETIC MISSED. A cycle pays the larger of
// two independent floors: the RULE floor (the condition must hold for `for:` all
// over again) and the TRANSPORT floor (Alertmanager will not send two
// notifications for one group closer together than `group_interval`, and a cycle
// needs two — one resolved, one firing). The old formula was
// `for + group_interval`, which claims a rule with no `for:` can cycle in one
// `group_interval`. It cannot; it needs two flushes. That missing term is why the
// shipped 5-in-30m flap default was unreachable even for the case it was
// supposedly written for.
func observableCycle(groupInterval, ruleFor time.Duration) time.Duration {
	if ruleFor > groupInterval {
		return groupInterval + ruleFor
	}
	return 2 * groupInterval
}

// observableTransitions is the ceiling on transitions oto can SEE in a window.
func observableTransitions(window, cycle time.Duration) int {
	return 2 * int(window/cycle)
}

// ---------------------------------------------------------- the upstream tie

// The corpus above is only worth anything if oto agrees about what Alertmanager's
// defaults are. `sources/domain` holds oto's copy of them and serves them to the
// tuning screen with `default_applies` provenance, so if that copy ever moves, the
// derivation moved with it and this file must be re-run.
func TestTheDerivationUsesTheSameAlertmanagerDefaultsOtoServes(t *testing.T) {
	t.Parallel()

	if sources.DefaultGroupInterval != EcosystemGroupInterval {
		t.Fatalf("oto tells operators Alertmanager's default group_interval is %s, but every "+
			"default in ADR 0026 is derived against %s. One of the two is now wrong",
			sources.DefaultGroupInterval, EcosystemGroupInterval)
	}
}

// ------------------------------------------------------------- refire_grace

// ⭐ THE HEADLINE RESULT. `refire_grace`'s clock starts at the occurrence's
// `ended_at`, which T5 takes from the UPSTREAM `EndsAt` — when Prometheus stopped
// considering the rule firing, not when oto heard about it. So a re-fire must pay
// the rule's whole `for:` dwell again INSIDE the grace window, and Alertmanager
// then batches the notification on top of that.
func TestRefireGraceReachesTheModalRealRule(t *testing.T) {
	t.Parallel()

	// The typical re-fire: the `for:` dwell, plus up to one batching delay.
	typical := ModalRuleFor + EcosystemGroupInterval

	if identity.DefaultRefireGrace < typical {
		t.Fatalf("DefaultRefireGrace = %s, but the modal real rule (for: %s) re-fires into "+
			"oto's view at %s. Every re-fire of 44.5%% of real rules would open a new episode, "+
			"a new generation and a brand-new Slack root card — which is the wall of messages "+
			"oto exists to prevent, produced by a setting that looks like it prevented it",
			identity.DefaultRefireGrace, ModalRuleFor, typical)
	}

	// ⛔ AND NOT MUCH MORE THAN THAT. `refire_grace` too high folds a genuine
	// re-fire into a stale thread, which is the shape of a MISSED PAGE — the worst
	// failure oto can have — while too low merely fragments threads, and every one
	// of those fragments is a LOUD new root card. oto prefers the loud error
	// (ADR 0026 §5), so the default is the SMALLEST value that reaches the mode,
	// not the largest that would cover the `for: 1h` tail.
	if identity.DefaultRefireGrace > typical+EcosystemGroupInterval {
		t.Fatalf("DefaultRefireGrace = %s, more than one batching delay past what the modal "+
			"rule needs (%s). oto prefers the loud error: a grace this wide merges genuinely "+
			"separate incidents for the 76%% of rules that never needed it", identity.DefaultRefireGrace, typical)
	}
}

// The old default is pinned as a REGRESSION, not as history: 600s is still a legal
// value an operator can write, and this states exactly what writing it costs.
func TestTheOldRefireGraceCouldNotReachTheModalRule(t *testing.T) {
	t.Parallel()

	const wasShipped = 600 * time.Second

	if wasShipped >= ModalRuleFor {
		t.Fatalf("the corpus or the arithmetic moved: %s no longer fails to reach a for: of %s, "+
			"so ADR 0026's central finding needs re-deriving", wasShipped, ModalRuleFor)
	}
	// It could not even reach the BEST case — a re-fire whose condition returned
	// the instant the alert resolved and whose notification was flushed at once.
	if bound, ok := identity.Bounds(identity.KeyRefireGrace); ok {
		if int(wasShipped/time.Second) != bound.Min {
			t.Fatalf("the old default was the BOUND FLOOR (%ds) presented as a recommendation; "+
				"that is the smell ADR 0026 removed, and the floor has since moved to %d",
				int(wasShipped/time.Second), bound.Min)
		}
	}
}

// --------------------------------------------------- refire_grace × close delay

// ⛔⛔ THE DEFECT THE TWO DEFAULTS HAD *BETWEEN* THEM, AND THE ONE A PER-KNOB TEST
// WOULD NEVER HAVE CAUGHT.
//
// Reopening an occurrence only avoids a new Slack root message while the group
// GENERATION is still open: closing a generation freezes its thread and the next
// observation opens N+1 with a brand-new root (ADR 0005, §B.5). oto shipped
// `group_close_delay: 300s` against `refire_grace: 600s`, so the generation closed
// five minutes into a ten-minute grace and the whole second half of the grace
// bought an occurrence reopen that posted a new card anyway. oto's own tuning page
// already stated the rule; its own defaults broke it.
func TestGroupCloseDelayDoesNotDefeatTheRefireGrace(t *testing.T) {
	t.Parallel()

	if identity.DefaultGroupCloseDelay < identity.DefaultRefireGrace {
		t.Fatalf("group_close_delay %s < refire_grace %s: a re-fire oto classified as the same "+
			"problem coming back finds a CLOSED generation and gets a brand-new Slack root card "+
			"anyway, which is the entire thing the grace exists to prevent",
			identity.DefaultGroupCloseDelay, identity.DefaultRefireGrace)
	}

	// Equality is SAFE rather than racy, and the reason is that the two clocks start
	// at different moments: this one runs from the group's last ACTIVITY (the
	// resolve as oto observed it) while the grace runs from the upstream `ended_at`,
	// which is the same instant or earlier. The generation therefore always closes
	// at or after the grace expires, never before.
	if identity.DefaultGroupCloseDelay != identity.DefaultRefireGrace {
		t.Logf("group_close_delay (%s) exceeds refire_grace (%s); legal, and wider than ADR 0026 "+
			"derived — check the ADR still describes what ships",
			identity.DefaultGroupCloseDelay, identity.DefaultRefireGrace)
	}

	// It must also clear `group_interval`, or a generation closes between two
	// batches of ONE incident and the second half arrives as a new group.
	if identity.DefaultGroupCloseDelay < EcosystemGroupInterval {
		t.Fatalf("group_close_delay %s is below the ecosystem group_interval %s: a generation "+
			"can close between two Alertmanager batches of the same incident",
			identity.DefaultGroupCloseDelay, EcosystemGroupInterval)
	}
}

// ------------------------------------------------------------- flap damping

// ⭐ THE SECOND HEADLINE RESULT: `5`-in-`30m` was unreachable for EVERY rule shape
// in the corpus, including rules with no `for:` at all, because the transport floor
// caps a 30-minute window at six observable transitions whatever the rules do.
func TestFlapThresholdIsReachableForTheModalRealRule(t *testing.T) {
	t.Parallel()

	cycle := observableCycle(EcosystemGroupInterval, ModalRuleFor)
	ceiling := observableTransitions(identity.DefaultFlapWindow, cycle)

	if identity.DefaultFlapThreshold > ceiling {
		t.Fatalf("flap_threshold %d exceeds the observable ceiling of %d "+
			"(for: %s, group_interval: %s, window %s → a %s cycle). The damper can NEVER engage: "+
			"it is dead code that looks configured, right up until somebody asks why a wildly "+
			"oscillating alert was never marked as flapping",
			identity.DefaultFlapThreshold, ceiling, ModalRuleFor,
			EcosystemGroupInterval, identity.DefaultFlapWindow, cycle)
	}

	// "Roughly half the ceiling" is the rule docs/setup/tuning.md states. At or
	// below half leaves room for the ordinary case to stay under it.
	if identity.DefaultFlapThreshold > ceiling/2 {
		t.Fatalf("flap_threshold %d is above half the observable ceiling of %d: reachable, but "+
			"only by an alert oscillating at close to the physical maximum, which is not what "+
			"'flapping' should mean", identity.DefaultFlapThreshold, ceiling)
	}

	// And not so far below it that one rolling deploy is mislabelled. The bound's
	// own floor of 3 is the hard version of this; here we check the default did not
	// drift down toward it.
	if identity.DefaultFlapThreshold < 3 {
		t.Fatalf("flap_threshold %d: below 3 a single rolling deploy is labelled as flapping "+
			"and a healthy alert is displayed as noisy", identity.DefaultFlapThreshold)
	}
}

// The window is the knob that was wrong, and it is derived from the threshold
// rather than the other way round: for long-`for:` rules you widen the window, you
// do not lower the threshold, because two transitions is a normal deploy.
func TestFlapWindowIsDerivedFromTheThresholdAndTheModalCycle(t *testing.T) {
	t.Parallel()

	cycle := observableCycle(EcosystemGroupInterval, ModalRuleFor)
	need := time.Duration(identity.DefaultFlapThreshold) * cycle

	if identity.DefaultFlapWindow < need {
		t.Fatalf("flap_window %s is below flap_threshold × cycle (%d × %s = %s), so the shipped "+
			"threshold sits above half the ceiling", identity.DefaultFlapWindow,
			identity.DefaultFlapThreshold, cycle, need)
	}

	// ⭐ ROUNDING UP IS DELIBERATE AND ASYMMETRIC. A window that is too wide fails
	// VISIBLY — a stale "flapping" badge on an alert that has settled — and
	// self-heals within one 5-minute `flap.score` tick, because the window is
	// rolling. A window that is too narrow fails INVISIBLY, as silence where a
	// damper should have been. But not unboundedly: past twice the requirement an
	// alert that misbehaved this morning is still marking one that settled hours ago.
	if identity.DefaultFlapWindow > 2*need {
		t.Fatalf("flap_window %s is more than twice what the derivation requires (%s): an alert "+
			"that misbehaved long ago keeps counting, and something long since fixed stays marked "+
			"as flapping in the UI", identity.DefaultFlapWindow, need)
	}
}

// The regression pin for the window, stating the defect in the terms it was found.
func TestTheOldFlapWindowWasUnreachableForEveryRuleShape(t *testing.T) {
	t.Parallel()

	const wasShipped = 30 * time.Minute

	// The BEST case in the whole corpus: a rule with no `for:` at all, oscillating
	// as fast as Alertmanager will report it. Even here the transport floor binds.
	fastest := observableCycle(EcosystemGroupInterval, 0)
	ceiling := observableTransitions(wasShipped, fastest)

	if ceiling >= 2*identity.DefaultFlapThreshold {
		t.Fatalf("a %s window now yields a ceiling of %d for a rule with no for:, which would "+
			"make the old default reachable after all. ADR 0026's second finding needs re-deriving",
			wasShipped, ceiling)
	}
}

// `flap_digest_interval` SURVIVED the derivation. A default that survives contact
// is a result, and pinning it stops it drifting on the next pass.
func TestFlapDigestIntervalSurvivesUnchanged(t *testing.T) {
	t.Parallel()

	// At or above group_interval: below it a digest cannot be produced more often,
	// it just adds jitter to when one lands.
	if identity.DefaultFlapDigestInterval < EcosystemGroupInterval {
		t.Fatalf("flap_digest_interval %s is below group_interval %s: it cannot produce more "+
			"digests, only less predictable ones",
			identity.DefaultFlapDigestInterval, EcosystemGroupInterval)
	}
	// 2×–4× is the stated useful band; too high and the digest arrives long after
	// anyone cared, too low and the digest is not a digest.
	if identity.DefaultFlapDigestInterval > 4*EcosystemGroupInterval {
		t.Fatalf("flap_digest_interval %s is more than 4 × group_interval %s: the summary lands "+
			"long after anyone cared, and a flapping alert is effectively silent until it does",
			identity.DefaultFlapDigestInterval, EcosystemGroupInterval)
	}
}

// ------------------------------------------------------------------- bounds

// A bound that excludes a value a real cluster needs is as much a defect as one
// that admits a pathological value. Every bound was re-checked against the fast
// capture as well as the ecosystem value, and NONE moved — this is why.
func TestTheBoundsStillAdmitTheOneRealFastCapture(t *testing.T) {
	t.Parallel()

	// At `group_interval: 30s` the observable cycle for an instantaneous rule is a
	// minute, so a 300-second flap window holds five whole cycles and is exactly
	// right. It is INERT at the ecosystem's 5m — and raising the floor to match
	// would have excluded the only real capture in this repo.
	fastCycle := observableCycle(FastCaptureGroupInterval, 0)
	bound, ok := identity.Bounds(identity.KeyFlapWindow)
	if !ok {
		t.Fatal("flap_window_s has no bound; every integer setting must have one")
	}
	floor := time.Duration(bound.Min) * time.Second
	if got := observableTransitions(floor, fastCycle); got < 2*identity.DefaultFlapThreshold {
		t.Fatalf("the flap_window floor of %s yields only %d observable transitions even at the "+
			"fast capture's group_interval of %s: the floor is now inert for EVERY real cluster "+
			"and should be raised", floor, got, FastCaptureGroupInterval)
	}

	// The group_close_delay floor is likewise below the ecosystem group_interval on
	// purpose: it is correct at 30s.
	cd, ok := identity.Bounds(identity.KeyGroupCloseDelay)
	if !ok {
		t.Fatal("group_close_delay_s has no bound")
	}
	if time.Duration(cd.Min)*time.Second < FastCaptureGroupInterval {
		t.Fatalf("the group_close_delay floor of %ds is below even the fast capture's "+
			"group_interval of %s, so it admits a value no cluster can use",
			cd.Min, FastCaptureGroupInterval)
	}
}

// Every shipped default must be inside its own bound. A default the server would
// refuse is a product that fails its own validation on a fresh install.
func TestEveryShippedDefaultIsInsideItsOwnBound(t *testing.T) {
	t.Parallel()

	var p identity.SettingsPatch
	for _, k := range identity.IntKeys() {
		v, _, ok := p.EffectiveInt(k)
		if !ok {
			t.Errorf("%s has no effective value", k)
			continue
		}
		b, ok := identity.Bounds(k)
		if !ok {
			t.Errorf("%s has no bound", k)
			continue
		}
		// `unacked_reminder_after_s` ships as 0 meaning "the org sets no default",
		// which is a legal READ value below a write bound that starts at 60.
		if k == identity.KeyUnackedReminder && v == 0 {
			continue
		}
		if !b.Contains(v) {
			t.Errorf("%s ships %d, outside its own bound %d..%d — a fresh install fails its own "+
				"validation", k, v, b.Min, b.Max)
		}
	}
}

// ------------------------------------------------------- the mirrored copies

// ⛔ FOUR PACKAGES CARRY THESE NUMBERS AND ONLY ONE OF THEM IS THE SOURCE.
// `identity/domain` owns the tenant's tuning; the lifecycle machine, the grouping
// engine and the alerts service each keep a fallback for the case where no
// SettingsReader is wired. A fallback that disagrees is a silent second product:
// an org whose settings failed to load would run the OLD arithmetic and nobody
// would be told. This is the only thing keeping them together.
func TestEveryMirroredDefaultAgreesWithIdentity(t *testing.T) {
	t.Parallel()

	if alerts.DefaultRefireGrace != identity.DefaultRefireGrace {
		t.Errorf("alerts/domain.DefaultRefireGrace = %s, identity/domain says %s",
			alerts.DefaultRefireGrace, identity.DefaultRefireGrace)
	}
	if alerts.DefaultResolveGrace != identity.DefaultResolveGrace {
		t.Errorf("alerts/domain.DefaultResolveGrace = %s, identity/domain says %s",
			alerts.DefaultResolveGrace, identity.DefaultResolveGrace)
	}
	if grouping.DefaultGroupCloseDelay != identity.DefaultGroupCloseDelay {
		t.Errorf("grouping/domain.DefaultGroupCloseDelay = %s, identity/domain says %s",
			grouping.DefaultGroupCloseDelay, identity.DefaultGroupCloseDelay)
	}
	if grouping.DefaultStormThreshold != identity.DefaultStormThreshold {
		t.Errorf("grouping/domain.DefaultStormThreshold = %d, identity/domain says %d",
			grouping.DefaultStormThreshold, identity.DefaultStormThreshold)
	}
	if grouping.DefaultStormWindow != identity.DefaultStormWindow {
		t.Errorf("grouping/domain.DefaultStormWindow = %s, identity/domain says %s",
			grouping.DefaultStormWindow, identity.DefaultStormWindow)
	}
	if grouping.DefaultStormCooldown != identity.DefaultStormCooldown {
		t.Errorf("grouping/domain.DefaultStormCooldown = %s, identity/domain says %s",
			grouping.DefaultStormCooldown, identity.DefaultStormCooldown)
	}

	svc := lifecycle.DefaultSettings()
	if svc.RefireGrace != identity.DefaultRefireGrace {
		t.Errorf("alerts/service.DefaultSettings().RefireGrace = %s, identity/domain says %s",
			svc.RefireGrace, identity.DefaultRefireGrace)
	}
	if svc.ResolveGrace != identity.DefaultResolveGrace {
		t.Errorf("alerts/service.DefaultSettings().ResolveGrace = %s, identity/domain says %s",
			svc.ResolveGrace, identity.DefaultResolveGrace)
	}
	if svc.FlapThreshold != identity.DefaultFlapThreshold {
		t.Errorf("alerts/service.DefaultSettings().FlapThreshold = %d, identity/domain says %d",
			svc.FlapThreshold, identity.DefaultFlapThreshold)
	}
	if svc.FlapWindow != identity.DefaultFlapWindow {
		t.Errorf("alerts/service.DefaultSettings().FlapWindow = %s, identity/domain says %s",
			svc.FlapWindow, identity.DefaultFlapWindow)
	}
}
