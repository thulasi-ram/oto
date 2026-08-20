package arch

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ⭐⭐ WHY THIS FILE EXISTS.
//
// CONTEXT.md §4 draws the dependency direction between modules, and for a long
// time it claimed to be enforced by "depguard + an arch test". There was no arch
// test, and depguard cannot do the job on its own: every
// `<module>-must-not-reach-into-other-domains` rule re-allows *every* other
// module's `/service` package, so those rules constrain LAYERING (you may only
// reach `<other>/service`, never its api/repository/domain) and say nothing at
// all about which way an edge points. The one directional depguard rule is
// `dependency-direction-alerts-and-ingestion`. Everything else was unenforced,
// and §4's diagram had drifted: it drew `alerts ──► grouping` while the code
// imports the other way round.
//
// ⛔ BOTH MODULES IN THAT ANECDOTE ARE NOT BOTH STILL HERE: `grouping` was deleted
// with `alert_groups` (git-bug `7570090`). The drift it records is the reason this
// file exists and is kept for that reason; the examples below name live modules.
//
// ⛔ THIS IS THE ONLY GATE ON DIRECTION — which is not the same as being the only
// gate an import meets. depguard is the gate on LAYERING: which package inside
// another module you may name (only its `/service`), and that `platform` may name no
// domain at all. This file is the gate on DIRECTION: which module may compile-depend
// on which, that nothing imports `internal/app`, and that the resulting graph has no
// cycle in it. Three cases, and they are the whole relationship:
//
//   - depguard alone — the wrong package along a DECLARED edge. `silences` importing
//     `alerts/repository`: §4 draws silences ──► alerts, so this file waves it
//     through and only `just lint` objects.
//   - this file alone — the right package in an UNDECLARED direction. `alerts`
//     importing `silences/service`: every `<module>-must-not-reach-into-other-domains`
//     rule re-allows every other module's `/service`, so depguard waves it through
//     and only this file objects. `internal/app` is the same shape and worse: no
//     depguard rule denies it outside `platform`/`pkg`.
//   - BOTH, deliberately — the wrong package along an UNDECLARED edge. A new
//     `notification` import of `silences/repository` trips depguard's layering rule
//     AND this file's undeclared-edge check. That double coverage is not redundancy
//     being tolerated; it is two independent questions happening to have the same
//     answer, and either gate alone would still leave the other case open.
//
// ⚠️ COMPILE-TIME EDGES ONLY. Ports (an interface the consumer declares and
// `internal/app/container.go` satisfies) and River job enqueues (a string in
// `internal/platform/jobs/kinds.go`) cross module boundaries without producing an
// import, so nothing here can see them — and nothing else can either. §4 draws
// them separately for that reason.

const modulePrefix = "github.com/thulasiram/oto/internal/"

// edge is one sanctioned compile-time dependency from one module to another.
//
// ⭐ ADDING A LINE HERE IS THE DECISION, NOT THE PAPERWORK. The list below IS
// CONTEXT.md §4's first diagram. A new module edge is an architectural change:
// state the reason in `why`, update §4 in the same commit, and expect the cycle
// check below to refuse the ones that would invert the graph.
type edge struct {
	from string
	to   string
	why  string
}

var allowedEdges = []edge{
	// ⛔ `grouping ──► alerts` WAS THE FIRST EDGE HERE AND IT IS DELETED (git-bug
	// `7570090`). `internal/grouping` is gone with `alert_groups`: there is no
	// §C.4 group resolution to read the Alert it is grouping, so the edge has no
	// `from` left. TestNoStaleDeclaredEdge is what forces this deletion rather than
	// leaving it as a standing permission nobody can exercise.
	//
	// ⚠️ CONTEXT.md §4's first diagram IS this list, so the diagram has to lose the
	// same edge. This gate reads the list, not the document, and cannot say whether
	// that happened.
	{
		from: "rules", to: "alerts",
		why: "a rule snapshot is captured at fire time against an Alert; `rules/api` reads " +
			"`alerts/service` to resolve the subject it is narrating.",
	},
	{
		from: "silences", to: "alerts",
		why: "the mirror answers \"which alerts is this silence muting\", which is a read of " +
			"`alerts/service`. It is READ-ONLY in both directions: oto has no silence write " +
			"path (R3).",
	},
	{
		from: "sources", to: "alerts",
		why: "`sources/service/reconcile.go` IS the §G.8 reconciler — SPEC §I.2 draws it under " +
			"`ingestion`, it is implemented here — and it hands its Observations to " +
			"`alerts/service.ObserveBatch`, the same single write path the webhook uses (C18).",
	},
	{
		from: "enrichment", to: "rules",
		why: "`enrichment/enrichers/promrule` recovers the rule definition behind an alert and " +
			"reads `rules/service` to do it.",
	},
}

// exemptReason names the cross-module imports that are deliberately NOT module
// dependencies, and returns "" for everything else.
//
// Both are already enforced by depguard (RULE K and RULE V in `.golangci.yml`);
// they are repeated here only so this test does not have to pretend they are
// ordinary edges — folding them into allowedEdges would make `alerts` look like
// the sink of six dependencies rather than the module that depends on nothing.
func exemptReason(from, pkg string) string {
	switch {
	case underPackage(pkg, "alerts/domain"):
		// RULE K — the shared domain kernel (§5.2b). Every module may import it; it
		// imports no other domain.
		return "RULE K: alerts/domain is the shared domain kernel"
	case from == "notification" && underPackage(pkg, "channels/domain"):
		// RULE V — the channel-agnostic view (§F.2). notification BUILDS
		// `channels/domain.NotificationView` and hands it to a Renderer whose
		// concrete it never names; `channels/service` is injected.
		return "RULE V: notification builds channels/domain.NotificationView"
	}
	return ""
}

func underPackage(pkg, prefix string) bool {
	return pkg == prefix || strings.HasPrefix(pkg, prefix+"/")
}

const (
	// compositionRoot is `internal/app`, the module nothing may import.
	compositionRoot = "app"
	// infraModule is `internal/platform`, the module everything may import.
	infraModule = "platform"
)

// skippedAsSource are the two trees whose OUTBOUND imports are outside the module
// graph by design. ⚠️ IT IS READ ON THE `from` SIDE ONLY, and that asymmetry is the
// whole point of it.
//
// `platform` is infrastructure, not a domain: `platform-must-not-import-domains`
// already forbids it every edge, and every module imports it, so it is skipped as a
// target too (see scanImports).
//
// `app` is the composition root — THE one place allowed to know every concrete — so
// every edge OUT of it is expected. An edge INTO it is not an undeclared edge to be
// argued about; it is a hard failure with no version that gets sanctioned. See
// compositionRootViolations.
//
// ⛔ AN EARLIER VERSION OF THIS COMMENT CLAIMED an edge into `app` "would be caught as
// an undeclared edge on the importing side". It would not have been. The target skip
// dropped it there too, and no depguard rule denies `internal/app` outside
// `platform`/`pkg` — so the single worst edge in the repo, the one that closes a cycle
// through every module at once, was the one edge nothing looked at.
var skippedAsSource = map[string]bool{infraModule: true, compositionRoot: true}

// observedEdge is one cross-module import found in the tree.
type observedEdge struct {
	from string
	pkg  string // the imported package, module-relative: "alerts/service"
	file string
}

func TestNoUndeclaredModuleEdge(t *testing.T) {
	allowed := make(map[string]edge, len(allowedEdges))
	for _, e := range allowedEdges {
		allowed[e.from+" -> "+e.to] = e
	}

	for _, obs := range scanImports(t) {
		if exemptReason(obs.from, obs.pkg) != "" {
			continue
		}
		to := moduleOf(obs.pkg)
		if to == compositionRoot {
			// TestNothingImportsTheCompositionRoot owns this one. Reporting it here
			// as well would offer the fix that must never be taken — "add the edge to
			// allowedEdges" — for the one edge no reason can sanction.
			continue
		}
		if _, ok := allowed[obs.from+" -> "+to]; ok {
			continue
		}
		t.Errorf(""+
			"undeclared module edge %s ──► %s\n"+
			"  %s imports %s%s\n"+
			"CONTEXT.md §4 does not draw this edge. Either reach %s through a port this "+
			"module declares and internal/app/container.go satisfies, or add the edge to "+
			"allowedEdges in this file WITH ITS REASON and redraw §4 in the same commit.",
			obs.from, to, obs.file, modulePrefix, obs.pkg, to)
	}
}

// TestNoStaleDeclaredEdge is the half that keeps §4 honest in the other
// direction. An edge that has been removed from the code but left in the list
// re-authorises itself the day somebody adds it back, silently — which is the
// exact failure mode that produced this ticket.
func TestNoStaleDeclaredEdge(t *testing.T) {
	seen := make(map[string]bool)
	for _, obs := range scanImports(t) {
		if exemptReason(obs.from, obs.pkg) != "" {
			continue
		}
		seen[obs.from+" -> "+moduleOf(obs.pkg)] = true
	}

	for _, e := range allowedEdges {
		if !seen[e.from+" -> "+e.to] {
			t.Errorf(""+
				"declared module edge %s ──► %s no longer exists in the code\n"+
				"delete it from allowedEdges and from CONTEXT.md §4: a declaration nothing "+
				"backs is a standing permission nobody argued for.",
				e.from, e.to)
		}
	}
}

// compositionRootViolations picks out the observed edges that point INTO
// `internal/app`.
//
// It is its own pass rather than a case inside TestNoUndeclaredModuleEdge because
// the two failures have different remedies. An undeclared edge has a legitimate
// answer — argue for it, add it to allowedEdges, redraw §4 — and this one has none,
// so it must never be reported by the test whose message offers that fix.
//
// ⚠️ exemptReason is deliberately not consulted: RULE K and RULE V name
// `alerts/domain` and `channels/domain`, neither of which lives under `app`, and no
// third exemption may be invented here.
func compositionRootViolations(observed []observedEdge) []observedEdge {
	var out []observedEdge
	for _, obs := range observed {
		if moduleOf(obs.pkg) == compositionRoot {
			out = append(out, obs)
		}
	}
	return out
}

// compositionRootFailure is the message, built apart from the test that prints it so
// TestCompositionRootGateFires can assert the gate says WHY and not merely that it
// fired.
func compositionRootFailure(obs observedEdge) string {
	return fmt.Sprintf(""+
		"%s imports the composition root\n"+
		"  %s imports %s%s\n"+
		"`internal/app` IS IMPORT-ONLY-OUTWARD. It constructs every concrete in the repo and "+
		"satisfies every port, so it already depends on every module; an import pointing back at "+
		"it closes a cycle through every one of them at once, and no module can then be "+
		"reasoned about, tested or switched off alone.\n"+
		"There is no line to add to allowedEdges for this: the edge is not undeclared, it is "+
		"unsanctionable. Take what you need as a port THIS module declares and "+
		"internal/app/container.go satisfies, or move the shared thing down into "+
		"internal/platform, which everything may import.",
		obs.from, obs.file, modulePrefix, obs.pkg)
}

// TestNothingImportsTheCompositionRoot is the second of the three things this file
// gates, and the one nothing else in the repo covers at all.
//
// ⛔ depguard DOES NOT COVER IT. The twelve
// `<module>-must-not-reach-into-other-domains` rules are `list-mode: lax` and not one
// of them denies `github.com/thulasiram/oto/internal/app`; the only rule that does is
// `platform-must-not-import-domains`, whose `files:` scope is `internal/platform/**`
// and `pkg/**`. An import of `internal/app` from any domain module passes `just lint`
// clean.
func TestNothingImportsTheCompositionRoot(t *testing.T) {
	for _, obs := range compositionRootViolations(scanImports(t)) {
		t.Error(compositionRootFailure(obs))
	}
}

// TestCompositionRootGateFires plants the edge that must never exist and checks the
// real walk reports it.
//
// ⭐ IT IS THE ONLY PROOF THE GATE HAS TEETH. No production file imports
// `internal/app`, so TestNothingImportsTheCompositionRoot passes today whether the
// check works, is inverted, or is deleted outright — which is exactly how the hole it
// closes survived: an earlier scanImports dropped every edge whose target was `app`
// before any check ran, and all three tests still passed.
//
// It plants a tree and points scanImportsIn at it rather than hand-building
// []observedEdge, so the walk, the module-skip logic and the message are all the
// production ones. The four files cover the four answers the walk has to get right.
func TestCompositionRootGateFires(t *testing.T) {
	root := t.TempDir()
	write := func(rel string, imports ...string) {
		t.Helper()
		var b strings.Builder
		b.WriteString("package p\n\nimport (\n")
		for _, imp := range imports {
			fmt.Fprintf(&b, "\t_ %q\n", modulePrefix+imp)
		}
		b.WriteString(")\n")

		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("planting %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatalf("planting %s: %v", rel, err)
		}
	}

	// THE VIOLATION: a domain module reaching back into the wiring layer.
	write("notification/service/render.go", "app/container")
	// OUT of app, which is what app is for — skipped as a source, so invisible here.
	write("app/container.go", "alerts/service")
	// A declared edge, to prove the gate is not just flagging every cross-module import.
	write("silences/service/mirror.go", "alerts/service")
	// NOT `app`: a module whose name merely starts with it. moduleOf splits on the
	// first `/` for this reason, and a prefix test written as strings.HasPrefix would
	// report this file and be wrong.
	write("rules/api/narrate.go", "appliance/service")

	observed := scanImportsIn(t, root)
	violations := compositionRootViolations(observed)

	if len(violations) != 1 {
		t.Fatalf("planted exactly one import of the composition root, gate found %d: %v",
			len(violations), violations)
	}
	got := violations[0]
	if got.from != "notification" || got.pkg != "app/container" {
		t.Errorf("gate reported the wrong edge: from=%q pkg=%q, want from=%q pkg=%q",
			got.from, got.pkg, "notification", "app/container")
	}
	if !strings.Contains(got.file, "notification/service/render.go") {
		t.Errorf("gate reported the wrong file: %q", got.file)
	}

	// The message has to survive too: a gate that fires without saying why gets the
	// edge deleted at random until it stops firing.
	msg := compositionRootFailure(got)
	for _, want := range []string{
		"notification/service/render.go",
		modulePrefix + "app/container",
		"IMPORT-ONLY-OUTWARD",
		"no line to add to allowedEdges",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure message does not mention %q:\n%s", want, msg)
		}
	}

	// The other three files are the negative half, and they are the half that would
	// catch an over-broad fix.
	for _, obs := range observed {
		if obs.from == compositionRoot {
			t.Errorf("app's own outbound imports must stay outside the graph, got %v", obs)
		}
	}
	if len(observed) != 3 {
		t.Errorf("expected 3 observed edges (app-import, silences──►alerts, rules──►appliance), got %d: %v",
			len(observed), observed)
	}
}

// TestModuleGraphIsAcyclic refuses an allow-list that has a cycle in it.
//
// ⭐ IT IS THE TEETH. Without it, the cheapest way past TestNoUndeclaredModuleEdge
// is to add the offending line to allowedEdges — which is how a `notification ──►
// alerts` against the standing `alerts`-side edge would come back. A cycle between
// two modules means neither can be reasoned about,
// tested or disabled alone, and running oto with notifications entirely switched
// off — how the first correctness tests run — stops being possible.
func TestModuleGraphIsAcyclic(t *testing.T) {
	adj := make(map[string][]string)
	for _, e := range allowedEdges {
		adj[e.from] = append(adj[e.from], e.to)
	}

	const (
		white = 0 // unvisited
		grey  = 1 // on the current path
		black = 2 // finished
	)
	colour := make(map[string]int)

	var path []string
	var walk func(string) []string
	walk = func(n string) []string {
		colour[n] = grey
		path = append(path, n)
		for _, next := range adj[n] {
			switch colour[next] {
			case grey:
				return append(append([]string{}, path...), next)
			case white:
				if cycle := walk(next); cycle != nil {
					return cycle
				}
			}
		}
		path = path[:len(path)-1]
		colour[n] = black
		return nil
	}

	starts := make([]string, 0, len(adj))
	for n := range adj {
		starts = append(starts, n)
	}
	sort.Strings(starts)

	for _, n := range starts {
		if colour[n] != white {
			continue
		}
		path = nil
		if cycle := walk(n); cycle != nil {
			t.Fatalf("allowedEdges contains a cycle: %s", strings.Join(cycle, " ──► "))
		}
	}
}

// scanImports reads every non-test Go file under internal/ and returns the
// cross-module imports it finds.
//
// ⚠️ NON-TEST FILES ONLY, which is the same boundary `.golangci.yml`'s exclusions
// draw for depguard: the arch rules describe production code. Test files DO cross
// module lines — `identity/repository`'s clock test imports `internal/app` — and
// forbidding that is a different decision needing its own argument.
//
// It parses rather than shelling out to `go list` so the gate does not need a
// build cache, a module download or a working toolchain path to run.
func scanImports(t *testing.T) []observedEdge {
	t.Helper()
	return scanImportsIn(t, filepath.Join(repoRoot(t), "internal"))
}

// scanImportsIn is scanImports with the tree named explicitly, so a test can point
// the real walk at a planted one instead of asserting on a hand-made []observedEdge.
func scanImportsIn(t *testing.T, root string) []observedEdge {
	t.Helper()

	fset := token.NewFileSet()
	var out []observedEdge

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		from := moduleOf(filepath.ToSlash(rel))
		if skippedAsSource[from] {
			return nil
		}

		f, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(p, modulePrefix) {
				continue
			}
			pkg := strings.TrimPrefix(p, modulePrefix)
			to := moduleOf(pkg)
			// ⚠️ THE TARGET SKIP IS NOT skippedAsSource, AND MUST NOT BECOME IT.
			// `platform` is skipped as a target because every module imports it and
			// none of those edges is a module dependency. `compositionRoot` is
			// deliberately absent: an import of `internal/app` is precisely the edge
			// that has to survive the walk to be reported.
			if to == from || to == infraModule {
				continue
			}
			out = append(out, observedEdge{
				from: from,
				pkg:  pkg,
				file: filepath.ToSlash(filepath.Join(filepath.Base(root), rel)),
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatal("found no cross-module imports at all — the scan is broken, not the graph")
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].pkg < out[j].pkg
	})
	return out
}

// moduleOf takes the first path element: "alerts/service/deps.go" -> "alerts".
func moduleOf(p string) string {
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return p
}

func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}
