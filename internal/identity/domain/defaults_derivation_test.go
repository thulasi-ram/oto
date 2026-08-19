package domain_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
	lifecycle "github.com/thulasiram/oto/internal/alerts/service"
	grouping "github.com/thulasiram/oto/internal/grouping/domain"
	identity "github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/tuning"
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
// build time.
//
// ⭐ WHAT CHANGED, AND WHY THE LAST SECTION OF THIS FILE IS NOW A TRIPWIRE RATHER
// THAN A LOAD-BEARING WALL. The four packages that need a shipped default used to
// each spell it out, and this file's agreement check was the ONLY thing holding
// them together — delete it and four packages could silently disagree, which is a
// note in a test file standing in for a mechanism. The numbers now live once, in
// `platform/tuning`, and every other declaration is a REFERENCE to that constant,
// so the compiler holds them together and the arithmetic below is what this file
// is for. The agreement check is kept anyway, one layer down: it fires if someone
// types a literal back in.

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
// (`case.opened|reopened` plus `case.resolved|expired`).
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

// ⭐ THE HEADLINE RESULT. `refire_grace`'s clock starts at the case's
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
// Reopening a case only avoids a new Slack root message while the group
// GENERATION is still open: a closed generation is never rejoined, and the next
// observation opens N+1, which is a new thread with a brand-new root
// (ADR 0005, §B.5). oto shipped
// `group_close_delay: 300s` against `refire_grace: 600s`, so the generation closed
// five minutes into a ten-minute grace and the whole second half of the grace
// bought a case reopen that posted a new card anyway. oto's own tuning page
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
		if !b.Contains(v) {
			t.Errorf("%s ships %d, outside its own bound %d..%d — a fresh install fails its own "+
				"validation", k, v, b.Min, b.Max)
		}
	}
}

// ------------------------------------------------------------- the one home

// ⛔ FOUR PACKAGES NEED THESE NUMBERS AND EXACTLY ONE PLACE WRITES THEM.
// `identity/domain` owns the tenant's tuning; the lifecycle machine, the grouping
// engine and the alerts service each keep a fallback for the case where no
// SettingsReader is wired. A fallback that disagrees is a silent second product:
// an org whose settings failed to load would run the OLD arithmetic and nobody
// would be told, which is exactly what ADR 0026 caused when it moved three
// defaults and two copies were missed.
//
// None of those packages may import each other — CONTEXT.md §5.4, enforced by
// depguard — so the numbers moved DOWN to `platform/tuning`, the layer everything
// may import and that may import nothing, and each package now declares its name
// as a reference to that constant. This test therefore checks something the
// compiler mostly checks already; it is kept because the one thing the compiler
// cannot object to is somebody replacing a reference with a literal that happens
// to agree today. See TestNoPackageSpellsATuningDefaultAsALiteral below, which
// objects to that in the syntax, and this one, which objects to it in the value.
func TestEveryMirroredDefaultAgreesWithIdentity(t *testing.T) {
	t.Parallel()

	// ⭐ `refire_grace` IS NOT CHECKED AGAINST THE ALERTS SIDE ANY MORE, AND THE
	// ABSENCE IS THE POINT. ADR 0040 retired T8, so the lifecycle machine no longer
	// measures a re-fire against a grace window and neither `alerts/domain` nor
	// `alerts/service` keeps a fallback for one. The SETTING survives untouched — an
	// org still tunes `refire_grace_s` and `identity/domain` still ships the default
	// the derivation at the top of this file computes — so the agreement that still
	// has two sides is identity's with `platform/tuning`, asserted at the foot of
	// this test. There is no alerts-side copy left that could drift from it.
	if alerts.DefaultResolveGrace != identity.DefaultResolveGrace {
		t.Errorf("alerts/domain.DefaultResolveGrace = %s, identity/domain says %s",
			alerts.DefaultResolveGrace, identity.DefaultResolveGrace)
	}
	if grouping.DefaultGroupCloseDelay != identity.DefaultGroupCloseDelay {
		t.Errorf("grouping/domain.DefaultGroupCloseDelay = %s, identity/domain says %s",
			grouping.DefaultGroupCloseDelay, identity.DefaultGroupCloseDelay)
	}
	// ⛔ THE THREE STORM MIRRORS WERE HERE AND BOTH SIDES ARE NOW GONE. `grouping/domain`
	// stopped declaring `DefaultStormThreshold`, `DefaultStormWindow` and
	// `DefaultStormCooldown` when storm damping was removed; `identity/domain` and
	// `platform/tuning` dropped theirs with the three settings keys (migration 00059),
	// because a shipped default for a key nothing admits is a number no install can
	// ever hold.

	svc := lifecycle.DefaultSettings()
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

	// And the home itself is the home: identity does not merely AGREE with
	// platform/tuning, it IS it. A `!=` here is unreachable while the declaration
	// is `= tuning.DefaultRefireGrace`, and reachable the moment it is not.
	if identity.DefaultRefireGrace != tuning.DefaultRefireGrace {
		t.Errorf("identity/domain.DefaultRefireGrace = %s but platform/tuning says %s: the "+
			"settings vocabulary has stopped naming the one home",
			identity.DefaultRefireGrace, tuning.DefaultRefireGrace)
	}
	if identity.DefaultGroupCloseDelay != tuning.DefaultGroupCloseDelay {
		t.Errorf("identity/domain.DefaultGroupCloseDelay = %s but platform/tuning says %s",
			identity.DefaultGroupCloseDelay, tuning.DefaultGroupCloseDelay)
	}
	if identity.DefaultFlapWindow != tuning.DefaultFlapWindow {
		t.Errorf("identity/domain.DefaultFlapWindow = %s but platform/tuning says %s",
			identity.DefaultFlapWindow, tuning.DefaultFlapWindow)
	}
}

// sharedTuningDefaults are the constant names that MUST NOT be spelled as a value
// anywhere but `platform/tuning`. They are the ones more than one package needs;
// a default only its own package reads (`DefaultUnackedReminderAfter`, the
// mention policy) is not here and is correctly a literal where it lives.
var sharedTuningDefaults = map[string]bool{
	"DefaultRefireGrace":     true,
	"DefaultResolveGrace":    true,
	"DefaultGroupCloseDelay": true,
	"DefaultFlapThreshold":   true,
	"DefaultFlapWindow":      true,
	"DefaultRawRetention":    true,
	"DefaultEventRetention":  true,
}

// ⛔⛔ THE GUARD THAT SURVIVES A FUTURE EDITOR, AND THE ONE THE OLD VERSION OF THIS
// FILE COULD NOT BE.
//
// Value agreement is not the property that was missing. Four packages agreeing
// TODAY was always true; what was missing was any reason they had to agree
// TOMORROW, because each one spelled its own number and the only tie between them
// was the test above. The property that actually holds them together is
// syntactic: outside `platform/tuning`, every one of these names is declared as a
// reference to `tuning.<Name>` and never as a literal. That is what makes a
// divergence a thing you have to do on purpose — and this test is what notices
// when somebody does it by accident, which is how it happened the first time.
//
// The check reads source rather than values on purpose: `DefaultFlapWindow = 2 *
// time.Hour` in `alerts/service` agreed with identity for a year and was still
// the defect.
func TestNoPackageSpellsATuningDefaultAsALiteral(t *testing.T) {
	t.Parallel()

	// Relative to internal/identity/domain, which is where this test file lives.
	// A package that grows a copy and is not listed here is not caught, so the
	// list is every package the numbers are known to have reached.
	dirs := []string{
		".",
		"../../alerts/domain",
		"../../alerts/service",
		"../../grouping/domain",
	}

	fset := token.NewFileSet()
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			// Production files only: a test may legitimately write the literal —
			// `damping_test.go` PINS 20m against the constant, which is the check that
			// says what the shipped number is.
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parsing %s: %v", path, err)
			}
			for _, decl := range file.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, ident := range vs.Names {
						if !sharedTuningDefaults[ident.Name] || i >= len(vs.Values) {
							continue
						}
						if referencesTuning(vs.Values[i]) {
							continue
						}
						t.Errorf("%s declares %s with a value of its own instead of `tuning.%s`. "+
							"That is the defect ADR 0026 shipped: the copy agrees until the day "+
							"somebody moves the original, and then an org whose settings failed to "+
							"load runs the old arithmetic and is told nothing",
							fset.Position(ident.Pos()), ident.Name, ident.Name)
					}
				}
			}
		}
	}
}

// referencesTuning reports whether an expression is rooted in the `tuning`
// package. It accepts arithmetic — `2 * tuning.DefaultFlapWindow` is a derived
// value that still moves with its source, which is the property being protected —
// and rejects anything that names no constant from the one home.
func referencesTuning(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		id, ok := v.X.(*ast.Ident)
		return ok && id.Name == "tuning"
	case *ast.BinaryExpr:
		return referencesTuning(v.X) || referencesTuning(v.Y)
	case *ast.ParenExpr:
		return referencesTuning(v.X)
	case *ast.CallExpr:
		for _, a := range v.Args {
			if referencesTuning(a) {
				return true
			}
		}
		return false
	default:
		return false
	}
}
