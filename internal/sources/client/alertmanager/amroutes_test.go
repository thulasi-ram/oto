package alertmanager

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/sources/domain"
)

// The route-tree resolver: which routes deliver where, with what timings, under
// what matchers.
//
// ⭐⭐ WHY EVERY CASE HERE IS ABOUT A SET RATHER THAN A NUMBER. The three
// Alertmanager timings are per-route and inherited, so the numbers governing the
// alerts oto is sent are the ones on the route(s) delivering to oto's receiver —
// and `continue: true` means there can be several, with different numbers. A
// resolver that returned one triple would be untestable in the only way that
// matters: it could be confidently wrong and no assertion here would catch it.
//
// Each case below pins the WHOLE list, in evaluation order, because the ordering
// and the omissions are the semantics:
//
//   - a route with a matcher-less child NEVER appears (it cannot deliver);
//   - a route after a matcher-less sibling appears MARKED unreachable;
//   - a timing appears with the DEPTH that stated it, so inheritance is visible.

// summary renders one resolved route as a single comparable line:
//
//	receiver | {matcher path} | gw=<value>@<depth> gi=… ri=… | flags
//
// A timing stated nowhere on the path renders as `-`, which is NOT zero and NOT
// Alertmanager's default: it is "no route on this path says", and what that
// implies is decided later, at the API boundary.
func summary(r domain.ReceiverRoute) string {
	var b strings.Builder
	b.WriteString(r.Receiver)
	b.WriteString(" | ")
	b.WriteString(r.Label())
	b.WriteString(" | gw=")
	b.WriteString(timingText(r.GroupWait))
	b.WriteString(" gi=")
	b.WriteString(timingText(r.GroupInterval))
	b.WriteString(" ri=")
	b.WriteString(timingText(r.RepeatInterval))
	if r.GroupByAll {
		b.WriteString(" by=...")
	} else if len(r.GroupBy) > 0 {
		b.WriteString(" by=" + strings.Join(r.GroupBy, ","))
	}
	if r.Shadowed {
		b.WriteString(" UNREACHABLE")
	}
	return b.String()
}

func timingText(t domain.InheritedTiming) string {
	if !t.Stated() {
		return "-"
	}
	return compactDur(*t.Value) + "@" + strconv.Itoa(t.FromDepth)
}

// compactDur renders a duration the way an alertmanager.yml states it, so a case
// below reads as the configuration it came from. time.Duration.String() spells 4h
// as "4h0m0s", which turns every expectation into a transcription exercise and
// hides the one character that matters when a case fails.
func compactDur(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	case d%time.Second == 0:
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return d.String()
}

// resolveFrom parses a whole alertmanager.yml and returns the resolution, so the
// cases below read as configuration rather than as parser internals.
func resolveFrom(t *testing.T, yaml string) domain.RouteResolution {
	t.Helper()
	cfg, err := parseConfig(yaml)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg.Routes
}

func assertRoutes(t *testing.T, got []domain.ReceiverRoute, want []string) {
	t.Helper()
	lines := make([]string, 0, len(got))
	for _, r := range got {
		lines = append(lines, summary(r))
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d routes, want %d\n got: %s\nwant: %s",
			len(lines), len(want), strings.Join(lines, "\n      "), strings.Join(want, "\n      "))
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("route %d:\n got %q\nwant %q", i, lines[i], want[i])
		}
	}
}

// TestTheRouteTreeIsResolvedWithAlertmanagersOwnSemantics.
func TestTheRouteTreeIsResolvedWithAlertmanagersOwnSemantics(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		yaml string
		want []string
	}{
		{
			// The stock shape: one route, no children, all three stated. The
			// top-level route IS a delivering route and must appear as one.
			name: "a flat config is one route",
			yaml: `
route:
  receiver: oto
  group_by: [alertname, namespace]
  group_wait: 10s
  group_interval: 30s
  repeat_interval: 4h
receivers:
  - name: oto
`,
			want: []string{`oto | {} | gw=10s@0 gi=30s@0 ri=4h@0 by=alertname,namespace`},
		},
		{
			// ⛔ THE CASE THE OLD PARSER GOT WRONG. The top-level route states
			// group_interval: 5m and the child overrides it with 1m; the child is
			// where the alerts go, so 5m was advice about a cluster that does not
			// exist. group_wait is stated only at depth 0, so the child inherits
			// it and the DEPTH says so.
			name: "a child overrides one timing and inherits the rest",
			yaml: `
route:
  receiver: fallback
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h
  routes:
    - receiver: oto
      matchers:
        - team="sre"
      group_interval: 1m
receivers:
  - name: fallback
  - name: oto
`,
			want: []string{
				`fallback | {} | gw=30s@0 gi=5m@0 ri=4h@0`,
				`oto | {} > {team="sre"} | gw=30s@0 gi=1m@1 ri=4h@0`,
			},
		},
		{
			// ⭐ THE REASON THE ANSWER IS A SET. `continue: true` does not stop
			// evaluation, so a `severity=critical, team=sre` alert reaches the
			// pager route AND the sre route. Both deliver to `oto`, and they
			// disagree about group_interval. There is no single number.
			name: "continue lets two routes reach one receiver with different timings",
			yaml: `
route:
  receiver: fallback
  group_wait: 30s
  group_interval: 5m
  routes:
    - receiver: oto
      matchers:
        - severity="critical"
      continue: true
      group_interval: 30s
    - receiver: oto
      matchers:
        - team="sre"
      group_interval: 10m
receivers:
  - name: fallback
  - name: oto
`,
			want: []string{
				`fallback | {} | gw=30s@0 gi=5m@0 ri=-`,
				`oto | {} > {severity="critical"} | gw=30s@0 gi=30s@1 ri=-`,
				`oto | {} > {team="sre"} | gw=30s@0 gi=10m@1 ri=-`,
			},
		},
		{
			// A matcher-less child takes everything its parent gets, so the
			// PARENT stops being a delivery point entirely — `dispatch.Route.Match`
			// falls back to the node only when no child matched — and every
			// sibling after it is unreachable at any label set.
			name: "a matcher-less child consumes the parent and shadows its siblings",
			yaml: `
route:
  receiver: fallback
  group_wait: 30s
  routes:
    - receiver: catchall
    - receiver: never
      matchers:
        - team="sre"
receivers:
  - name: fallback
`,
			want: []string{
				`catchall | {} > {} | gw=30s@0 gi=- ri=-`,
				`never | {} > {team="sre"} | gw=30s@0 gi=- ri=- UNREACHABLE`,
			},
		},
		{
			// The same child WITH `continue: true` still consumes the parent —
			// Match appends its result, so the parent is never the answer — but it
			// does NOT stop evaluation, so the sibling after it is reachable.
			// Getting this pair the same way round would either invent a route or
			// delete a real one.
			name: "a matcher-less child with continue does not shadow its siblings",
			yaml: `
route:
  receiver: fallback
  group_wait: 30s
  routes:
    - receiver: mirror
      continue: true
    - receiver: oto
      matchers:
        - team="sre"
receivers:
  - name: fallback
`,
			want: []string{
				`mirror | {} > {} | gw=30s@0 gi=- ri=-`,
				`oto | {} > {team="sre"} | gw=30s@0 gi=- ri=-`,
			},
		},
		{
			// `match` and `match_re` are deprecated upstream and still route
			// production traffic. Reading only `matchers` would report this route
			// as matcher-less, which would shadow its sibling and claim a route
			// that fires constantly can never fire.
			name: "the deprecated match spellings are read and normalised",
			yaml: `
route:
  receiver: fallback
  routes:
    - receiver: oto
      match:
        severity: critical
        team: sre
      match_re:
        env: prod|staging
    - receiver: other
      matchers:
        - team="db"
receivers:
  - name: fallback
`,
			want: []string{
				`fallback | {} | gw=- gi=- ri=-`,
				`oto | {} > {env=~"prod|staging",severity="critical",team="sre"} | gw=- gi=- ri=-`,
				`other | {} > {team="db"} | gw=- gi=- ri=-`,
			},
		},
		{
			// `group_by` inherits like everything else, and `...` is its own
			// state: grouping by every label means no group ever accumulates a
			// second member, so storm collapse is unreachable at any threshold.
			// No timing captures that, which is why it travels beside them.
			name: "group_by inherits and the ... form is preserved",
			yaml: `
route:
  receiver: fallback
  group_by: [alertname, cluster]
  routes:
    - receiver: inherits
      matchers:
        - team="a"
    - receiver: everything
      matchers:
        - team="b"
      group_by: ["..."]
receivers:
  - name: fallback
`,
			want: []string{
				`fallback | {} | gw=- gi=- ri=- by=alertname,cluster`,
				`inherits | {} > {team="a"} | gw=- gi=- ri=- by=alertname,cluster`,
				`everything | {} > {team="b"} | gw=- gi=- ri=- by=...`,
			},
		},
		{
			// The receiver inherits too, so a child that names none delivers to
			// its parent's — which is how a whole subtree ends up pointed at oto
			// without the word appearing anywhere below the root.
			name: "the receiver inherits down the tree",
			yaml: `
route:
  receiver: oto
  group_interval: 5m
  routes:
    - matchers:
        - team="sre"
      group_interval: 1m
      routes:
        - matchers:
            - severity="critical"
          group_wait: 0s
receivers:
  - name: oto
`,
			want: []string{
				`oto | {} | gw=- gi=5m@0 ri=-`,
				`oto | {} > {team="sre"} | gw=- gi=1m@1 ri=-`,
				`oto | {} > {team="sre"} > {severity="critical"} | gw=0s@2 gi=1m@1 ri=-`,
			},
		},
		{
			// A config with no `route:` block routes nothing anywhere. There is no
			// answer to give, and an empty list is how that is said — never a
			// fabricated top-level route standing in for one.
			name: "a config with no route block resolves to nothing",
			yaml: `
receivers:
  - name: oto
`,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertRoutes(t, resolveFrom(t, tc.yaml).Routes, tc.want)
		})
	}
}

// TestWhichReceiverIsOtosOwn.
//
// ⛔⛔ THIS IS AN INFERENCE AND THE TEST EXISTS TO KEEP IT ONE. oto's ingest path
// is `/api/v1/ingest/alertmanager/{source_id}`, so the webhook URL in an
// operator's config contains the id of the source oto is probing and would
// identify oto's receiver exactly. `webhook_config.url` is a `SecretURL`, and
// `config.original` is the marshalled config, so it arrives as `<secret>` —
// verified by `testdata/compose_v0.28.1.yaml`, a real capture. So the rule is
// narrow and its basis travels with the answer.
func TestWhichReceiverIsOtosOwn(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		yaml       string
		wantName   string
		wantBasis  domain.ReceiverBasis
		wantCandis []string
	}{
		{
			name: "exactly one webhook receiver is oto's",
			yaml: `
route:
  receiver: oto
receivers:
  - name: oto
    webhook_configs:
      - url: <secret>
  - name: mail
    email_configs:
      - to: a@example.com
`,
			wantName:   "oto",
			wantBasis:  domain.ReceiverSoleWebhook,
			wantCandis: []string{"oto"},
		},
		{
			// Two webhook receivers and no way to tell them apart. Picking one
			// would be a coin toss presented as a reading — the exact failure the
			// hand-typed tuning form was replaced for.
			name: "several webhook receivers are ambiguous, with both named",
			yaml: `
route:
  receiver: a
receivers:
  - name: a
    webhook_configs:
      - url: <secret>
  - name: b
    webhook_configs:
      - url: <secret>
`,
			wantName:   "",
			wantBasis:  domain.ReceiverAmbiguous,
			wantCandis: []string{"a", "b"},
		},
		{
			// A real finding about a source, not a parser shortfall: nothing in
			// this configuration can push to oto at all.
			name: "no webhook receiver at all is its own answer",
			yaml: `
route:
  receiver: mail
receivers:
  - name: mail
    email_configs:
      - to: a@example.com
`,
			wantName:  "",
			wantBasis: domain.ReceiverNoWebhook,
		},
		{
			// An empty `webhook_configs:` list is not a webhook receiver. It
			// declares an integration and configures none, so it delivers nothing.
			name: "an empty webhook_configs list is not a candidate",
			yaml: `
route:
  receiver: a
receivers:
  - name: a
    webhook_configs: []
`,
			wantName:  "",
			wantBasis: domain.ReceiverNoWebhook,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := resolveFrom(t, tc.yaml)
			if res.Receiver != tc.wantName {
				t.Errorf("receiver = %q, want %q", res.Receiver, tc.wantName)
			}
			if res.Basis != tc.wantBasis {
				t.Errorf("basis = %q, want %q", res.Basis, tc.wantBasis)
			}
			if strings.Join(res.WebhookReceivers, ",") != strings.Join(tc.wantCandis, ",") {
				t.Errorf("candidates = %v, want %v", res.WebhookReceivers, tc.wantCandis)
			}
		})
	}
}

// TestReachingOneReceiverIsASetThatCanDisagree.
//
// ⛔ THE WHOLE POINT. Two routes reach `oto` with different `group_interval`s, so
// there is no single triple to argue from and `Governing` must refuse to produce
// one. A resolver that returned the first match would pass every other test in
// this file and give confident, wrong tuning advice forever.
func TestReachingOneReceiverIsASetThatCanDisagree(t *testing.T) {
	t.Parallel()

	res := resolveFrom(t, `
route:
  receiver: fallback
  group_wait: 30s
  group_interval: 5m
  routes:
    - receiver: oto
      matchers:
        - severity="critical"
      continue: true
      group_interval: 30s
    - receiver: oto
      matchers:
        - team="sre"
receivers:
  - name: fallback
  - name: oto
    webhook_configs:
      - url: <secret>
`)

	reaching := res.ForOto()
	if len(reaching) != 2 {
		t.Fatalf("routes reaching oto = %d, want 2", len(reaching))
	}
	if domain.Agree(reaching) {
		t.Fatal("two routes state 30s and 5m and were reported as agreeing")
	}
	if _, ok := domain.Governing(reaching); ok {
		t.Fatal("Governing produced a single answer for a set that disagrees; " +
			"picking one is the failure this whole feature exists to remove")
	}

	// The same tree with the disagreement removed DOES have one answer: agreement
	// is about the numbers, not about the number of routes.
	agreeing := []domain.ReceiverRoute{reaching[0], reaching[0]}
	gov, ok := domain.Governing(agreeing)
	if !ok || gov.GroupInterval.Value == nil || *gov.GroupInterval.Value != 30*time.Second {
		t.Fatalf("two identical routes did not resolve to their own value: %+v", gov)
	}
}

// TestASingleReachingRouteIsTheAnswer. The commonest real install: one webhook
// receiver, one route to it, and the numbers on THAT route rather than the root's.
func TestASingleReachingRouteIsTheAnswer(t *testing.T) {
	t.Parallel()

	res := resolveFrom(t, `
route:
  receiver: fallback
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h
  routes:
    - receiver: oto
      matchers:
        - team="sre"
      group_interval: 1m
receivers:
  - name: fallback
  - name: oto
    webhook_configs:
      - url: <secret>
`)

	gov, ok := domain.Governing(res.ForOto())
	if !ok {
		t.Fatal("one route reaches oto and no governing answer was produced")
	}
	if gov.GroupInterval.Value == nil || *gov.GroupInterval.Value != time.Minute {
		t.Fatalf("group_interval = %v, want the route's own 1m and not the root's 5m",
			gov.GroupInterval.Value)
	}
	if !gov.GroupInterval.Own(len(gov.Path)) {
		t.Error("group_interval was reported as inherited when the route states it")
	}
	// group_wait is stated only at the root, so it is inherited — the same
	// number, a different line in the operator's file.
	if gov.GroupWait.Value == nil || *gov.GroupWait.Value != 30*time.Second {
		t.Fatalf("group_wait = %v, want 30s inherited from the root", gov.GroupWait.Value)
	}
	if gov.GroupWait.Own(len(gov.Path)) {
		t.Error("group_wait was reported as this route's own when only the root states it")
	}
	if gov.GroupWait.FromDepth != 0 {
		t.Errorf("group_wait came from depth %d, want 0", gov.GroupWait.FromDepth)
	}
}

// TestTheRealNestedFixtureResolves runs the whole thing over a realistic
// multi-team tree: inheritance across two levels, `continue: true`, both matcher
// spellings, a parent consumed by its catch-all child, and an unreachable route.
func TestTheRealNestedFixtureResolves(t *testing.T) {
	t.Parallel()

	res := resolveFrom(t, mustFixture(t, "nested_team_routes.yaml"))

	assertRoutes(t, res.Routes, []string{
		`fallback | {} | gw=30s@0 gi=- ri=4h@0 by=alertname,cluster`,
		`pager | {} > {severity="page"} | gw=30s@0 gi=30s@1 ri=4h@0 by=alertname,cluster`,
		// `platform` itself is ABSENT: its matcher-less child consumes everything
		// that reaches it, so it is never a delivery point.
		`audit | {} > {team="platform"} > {kind="audit"} | gw=30s@0 gi=2m@1 ri=24h@2 by=alertname,cluster`,
		`db-team | {} > {team="platform"} > {component="database",env=~"prod|staging"} | gw=10s@2 gi=2m@1 ri=4h@0 by=alertname,cluster`,
		`platform-catchall | {} > {team="platform"} > {} | gw=30s@0 gi=2m@1 ri=4h@0 by=...`,
		`never-fires | {} > {team="platform"} > {alertname="Whatever"} | gw=30s@0 gi=2m@1 ri=4h@0 by=alertname,cluster UNREACHABLE`,
	})

	// Two receivers carry a webhook and Alertmanager redacted both URLs, so oto
	// must decline to name one.
	if res.Basis != domain.ReceiverAmbiguous {
		t.Errorf("basis = %q, want ambiguous", res.Basis)
	}
	if got := strings.Join(res.WebhookReceivers, ","); got != "pager,platform" {
		t.Errorf("candidates = %q, want pager,platform", got)
	}
	if len(res.ForOto()) != 0 {
		t.Error("routes were claimed for oto while the receiver is ambiguous")
	}

	// The deprecated spelling is read AND labelled: oto renders the current one
	// and says where it came from rather than rewriting the operator's file.
	dbRoute := res.Routes[3]
	if !dbRoute.Path[len(dbRoute.Path)-1].Deprecated {
		t.Error("the match/match_re route was not flagged as using the deprecated spelling")
	}
	if !res.Routes[1].Path[1].Continue {
		t.Error("continue: true was lost on the pager route")
	}
}

// TestTheCapturedFixturesStillResolve keeps the two REAL captures honest against
// the new walk: whatever else changes, these are what a live Alertmanager emits.
func TestTheCapturedFixturesStillResolve(t *testing.T) {
	t.Parallel()

	// The compose Alertmanager: one receiver, one webhook, one route. This is the
	// shape SPEC §J.1 tells every operator to deploy, and it is the case that
	// must be answered exactly rather than hedged.
	compose := resolveFrom(t, mustFixture(t, "compose_v0.28.1.yaml"))
	assertRoutes(t, compose.Routes, []string{
		`oto | {} | gw=10s@0 gi=30s@0 ri=4h@0 by=alertname,namespace`,
	})
	if compose.Basis != domain.ReceiverSoleWebhook || compose.Receiver != "oto" {
		t.Fatalf("compose receiver = %q (%s), want oto via sole_webhook",
			compose.Receiver, compose.Basis)
	}
	gov, ok := domain.Governing(compose.ForOto())
	if !ok || gov.GroupInterval.Value == nil || *gov.GroupInterval.Value != 30*time.Second {
		t.Fatalf("the compose route did not govern: %+v", gov)
	}

	// The minimal capture: a child route that DOES state timings, over a root
	// that states none. It proves the walk carries an absence down — `gi=-` on
	// both routes is "no route on this path says", never 5m.
	minimal := resolveFrom(t, mustFixture(t, "minimal_child_routes_v0.28.1.yaml"))
	assertRoutes(t, minimal.Routes, []string{
		`oto | {} | gw=- gi=- ri=-`,
		`oto | {} > {severity="critical"} | gw=5s@1 gi=- ri=1h@1`,
	})
	// ⚠️ ITS RECEIVER DECLARES NO INTEGRATION AT ALL, so oto cannot be fed by it
	// and must not claim the routes below it.
	if minimal.Basis != domain.ReceiverNoWebhook {
		t.Errorf("minimal basis = %q, want no_webhook", minimal.Basis)
	}
	if len(minimal.ForOto()) != 0 {
		t.Error("routes were claimed for oto on a config with no webhook receiver")
	}
}

// TestTheWalkIsBoundedByItsCaps. The tree is untrusted input of unbounded size
// and the result is stored per source and rendered on a settings screen. Both
// caps must DROP VISIBLY rather than truncate silently — a list that is quietly
// short is a list an operator reads as complete.
func TestTheWalkIsBoundedByItsCaps(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	b.WriteString("route:\n  receiver: oto\n  routes:\n")
	for i := range domain.MaxResolvedRoutes + 10 {
		b.WriteString("    - receiver: r\n      matchers:\n        - i=\"" + strconv.Itoa(i) + "\"\n")
	}
	b.WriteString("receivers:\n  - name: oto\n")

	res := resolveFrom(t, b.String())
	if len(res.Routes) != domain.MaxResolvedRoutes {
		t.Fatalf("routes = %d, want the cap of %d", len(res.Routes), domain.MaxResolvedRoutes)
	}
	if res.Dropped == 0 {
		t.Fatal("routes were discarded and Dropped stayed 0, so the list reads as complete")
	}

	// Depth: a tree deeper than MaxRouteDepth stops being walked, and the cut is
	// counted rather than hidden.
	var deep strings.Builder
	deep.WriteString("route:\n  receiver: oto\n")
	indent := "  "
	for range MaxRouteDepth + 4 {
		deep.WriteString(indent + "routes:\n" + indent + "  - matchers: [a=\"b\"]\n")
		indent += "    "
	}
	deep.WriteString("receivers:\n  - name: oto\n")

	nested := resolveFrom(t, deep.String())
	for _, r := range nested.Routes {
		if len(r.Path) > MaxRouteDepth {
			t.Fatalf("a path of %d routes escaped the depth cap of %d", len(r.Path), MaxRouteDepth)
		}
	}
	if nested.Dropped == 0 {
		t.Fatal("the depth cap cut the tree and Dropped stayed 0")
	}
}
