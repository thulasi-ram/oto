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
	identity "github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/tuning"
	sources "github.com/thulasiram/oto/internal/sources/domain"
)

// ⛔⛔ THIS FILE IS THE ARITHMETIC BEHIND THE SHIPPED TIMING DEFAULTS, EXECUTED.
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
// ⚠️ These are test-only cross-package imports, the same idiom
// `ingestion/domain/bounds_test.go` uses: `identity/domain` is a settings
// vocabulary and must not depend on the lifecycle or the source client at build
// time. (That file is what is left of `refire_grace_replay_test.go`, which used
// to be cited here and which git-bug 7287b28 deleted along with the two settings
// it tied together.)
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

// ------------------------------------------ refire_grace × close delay: DELETED

// ⛔⛔ THREE TESTS AND ~90 LINES OF DERIVATION STOOD HERE AND ALL THREE ARE
// DELETED WITH THEIR SUBJECTS (git-bug 7287b28, owner ruling of 2026-08-19):
//
//   - `TestRefireGraceReachesTheModalRealRule` — that 1200s reaches the modal
//     real rule's `for: 15m` plus one batching delay, and no further, because oto
//     prefers a loud fragmented thread to a silently merged one (ADR 0026 §5).
//   - `TestTheOldRefireGraceCouldNotReachTheModalRule` — that the old 600s was
//     the BOUND FLOOR presented as a recommendation, ADR 0026's central finding.
//   - `TestGroupCloseDelayDoesNotDefeatTheRefireGrace` — that the close delay is
//     at or above the grace, because a generation that closed first handed a
//     re-fire a new Slack root card anyway.
//
// ⭐ THE THIRD ONE IS WHY THIS BLOCK IS A COMMENT RATHER THAN A DELETION. It was
// the ONE test in this file asserting a relationship BETWEEN two defaults, and it
// existed because the shipped pair had once been wrong in exactly that way — 300s
// against 600s. It was also the only thing enforcing the "pin" that
// `platform/tuning` described as *"the whole point rather than a coincidence"*,
// and it enforced it one level too high: it compared the two DEFAULTS, so nothing
// stopped an operator's two SETTINGS from contradicting each other. That is the
// `4aea61e` shape, and both halves are now gone, which is the only thing that
// makes it moot. Deleting one and keeping the other would have left the trap
// armed with its tripwire removed.
//
// ⚠️ SO THE DERIVATION BELOW NOW COVERS TWO DEFAULTS, NOT FOUR, AND THE HEADER OF
// THIS FILE SAYS SO. The corpus constants are unchanged and still measured; what
// changed is that two of the four numbers they justified described mechanisms
// ADR 0040 and git-bug 7570090 deleted.

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

	// ⛔ THE SAME CHECK FOR `group_close_delay_s`'s FLOOR WAS HERE AND IS DELETED
	// WITH THE KEY (git-bug 7287b28). It made the same point from the other end —
	// that a 60s floor is below the ecosystem's 5m `group_interval` ON PURPOSE,
	// because the compose capture runs `group_interval: 30s` and a bound that
	// excluded it would exclude a value a real cluster needs. `flap_window_s`
	// above still carries that argument, so the reasoning did not leave with the
	// key.
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

// ⛔ THREE PACKAGES NEED THESE NUMBERS AND EXACTLY ONE PLACE WRITES THEM.
// `identity/domain` owns the tenant's tuning; the lifecycle machine and the alerts
// service each keep a fallback for the case where no SettingsReader is wired. A fallback that disagrees is a silent second product:
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

	// ⭐ `refire_grace` IS NOT CHECKED HERE ANY MORE BECAUSE IT NO LONGER EXISTS
	// (git-bug 7287b28), AND THE TWO-STAGE WAY IT LEFT IS THE FAILURE MODE THIS
	// WHOLE SECTION IS A TRIPWIRE FOR, SO IT IS RECORDED RATHER THAN DELETED.
	//
	// It stopped being checked against the ALERTS side first: ADR 0040 retired T8,
	// so the lifecycle machine no longer measured a re-fire against a grace window
	// and neither `alerts/domain` nor `alerts/service` kept a fallback for one. The
	// SETTING survived that, untouched — an org still tuned `refire_grace_s` and
	// identity still shipped a default derived from the corpus — which is exactly
	// the state the owner then ruled on: a knob with a derivation, a bound and no
	// reader. The mirror going was the early symptom; the key going is the fix.
	//
	// The agreement that still has two sides is identity's with `platform/tuning`,
	// asserted at the foot of this test.
	if alerts.DefaultResolveGrace != identity.DefaultResolveGrace {
		t.Errorf("alerts/domain.DefaultResolveGrace = %s, identity/domain says %s",
			alerts.DefaultResolveGrace, identity.DefaultResolveGrace)
	}
	// ⛔ THE `group_close_delay` MIRROR WAS HERE AND ONE SIDE OF IT NO LONGER EXISTS.
	// It compared `grouping/domain.DefaultGroupCloseDelay` against identity's, and
	// `internal/grouping` is gone as a package — the engine folded into case
	// formation. The agreement that still has two sides is identity's with
	// `platform/tuning`, asserted at the foot of this test, and that is the one that
	// matters: identity does not mirror the number, it names the one home.
	//
	// ⚠️ THIS FILE DID NOT COMPILE UNTIL THIS COMMENT REPLACED THAT CHECK. The import
	// outlived the package it named, so `internal/identity/domain`'s external test
	// package had been unbuildable — a mirror check that cannot run is worse than a
	// deleted one, because the file still reads as if it were guarding something.
	//
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
	// is `= tuning.DefaultResolveGrace`, and reachable the moment it is not.
	// ⛔ THE `DefaultRefireGrace` AND `DefaultGroupCloseDelay` CHECKS WERE HERE AND
	// BOTH SIDES OF EACH ARE NOW GONE (git-bug 7287b28): neither `identity/domain`
	// nor `platform/tuning` declares either constant.
	if identity.DefaultResolveGrace != tuning.DefaultResolveGrace {
		t.Errorf("identity/domain.DefaultResolveGrace = %s but platform/tuning says %s: the "+
			"settings vocabulary has stopped naming the one home",
			identity.DefaultResolveGrace, tuning.DefaultResolveGrace)
	}
	if identity.DefaultFlapWindow != tuning.DefaultFlapWindow {
		t.Errorf("identity/domain.DefaultFlapWindow = %s but platform/tuning says %s",
			identity.DefaultFlapWindow, tuning.DefaultFlapWindow)
	}
}

// sharedTuningDefaults are the constant names that MUST NOT be spelled as a value
// anywhere but `platform/tuning`. They are the ones more than one package needs;
// a default only its own package reads is not here and is correctly a literal
// where it lives — `DefaultChannelVerbosity` is identity's, and nothing else needs
// it. ⛔ THIS SENTENCE USED TO CITE `DefaultUnackedReminderAfter` AND THE MENTION
// POLICY, both deleted with the reminder (git-bug bd0fb1d).
// ⛔ `DefaultRefireGrace` AND `DefaultGroupCloseDelay` WERE THE FIRST AND THIRD
// ENTRIES AND BOTH ARE DELETED (git-bug 7287b28). A name here that no package
// declares is worse than useless: it makes the guard below look like it is
// watching something.
var sharedTuningDefaults = map[string]bool{
	"DefaultResolveGrace":   true,
	"DefaultFlapThreshold":  true,
	"DefaultFlapWindow":     true,
	"DefaultRawRetention":   true,
	"DefaultEventRetention": true,
}

// ⛔⛔ THE GUARD THAT SURVIVES A FUTURE EDITOR, AND THE ONE THE OLD VERSION OF THIS
// FILE COULD NOT BE.
//
// Value agreement is not the property that was missing. Every package agreeing
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
		// ⛔ `../../grouping/domain` WAS LISTED HERE. The package is gone, and
		// `os.ReadDir` on it was a `t.Fatalf` — this scan could not have passed even
		// if the file had compiled.
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
