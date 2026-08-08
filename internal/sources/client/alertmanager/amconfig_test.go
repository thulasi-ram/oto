package alertmanager

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

// The route-timing parser: what oto reads out of an Alertmanager's OWN config so
// that nobody has to type `group_wait` into a form again.
//
// ⭐⭐ THE TWO FIXTURES BELOW ARE REAL. They are `config.original` as captured
// verbatim from `prom/alertmanager:v0.28.1` — one from the repository's own
// `docker compose` Alertmanager, one from a container started on a minimal config
// with a child route. They are not hand-written approximations of what
// Alertmanager might emit, and that distinction is the entire reason the second
// one exists: it proves that ALERTMANAGER OMITS AN UNSET TIMING rather than
// publishing its documented default, which is what makes `unknown` a real,
// common state rather than a theoretical one.
//
// `TestAgainstARealAlertmanager` at the bottom runs the same assertions against a
// live server when one is pointed at it.

func mustFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

func seconds(d *time.Duration) string {
	if d == nil {
		return "unknown"
	}
	return d.String()
}

// TestTheComposeAlertmanagerReportsItsThreeTimings.
//
// deploy/alertmanager/alertmanager.yml states all three, so all three are
// observed, and there are no child routes to qualify them.
func TestTheComposeAlertmanagerReportsItsThreeTimings(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig(mustFixture(t, "compose_v0.28.1.yaml"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	tm := cfg.Timings
	if tm.GroupWait == nil || *tm.GroupWait != 10*time.Second {
		t.Fatalf("group_wait = %s, want 10s", seconds(tm.GroupWait))
	}
	if tm.GroupInterval == nil || *tm.GroupInterval != 30*time.Second {
		t.Fatalf("group_interval = %s, want 30s", seconds(tm.GroupInterval))
	}
	if tm.RepeatInterval == nil || *tm.RepeatInterval != 4*time.Hour {
		t.Fatalf("repeat_interval = %s, want 4h", seconds(tm.RepeatInterval))
	}
	if tm.ChildRoutes != 0 || tm.ChildrenWithTimings != 0 {
		t.Fatalf("child routes %d/%d, want none", tm.ChildrenWithTimings, tm.ChildRoutes)
	}
	if !tm.Known() {
		t.Fatal("Known() is false for a config that states all three")
	}

	// The pre-existing parse must be untouched by the addition.
	if cfg.ResolveTimeout != 5*time.Minute {
		t.Fatalf("resolve_timeout = %v", cfg.ResolveTimeout)
	}
	if sends, ok := cfg.SendResolved["oto"]; !ok || !sends {
		t.Fatalf("send_resolved for `oto` = %v/%v", sends, ok)
	}
}

// TestAnUnsetTimingIsUnknownAndIsNeverTheDocumentedDefault.
//
// ⛔ THIS IS THE LOAD-BEARING TEST OF THE WHOLE FEATURE. The fixture's top-level
// route states NONE of the three, and Alertmanager does not fill them in: they
// are `omitempty` pointers, and the 30s / 5m / 4h defaults are applied later, in
// `dispatch.NewRoute`, where `GET /api/v2/status` cannot see them. If oto ever
// substitutes the documented values here, it prints a confident number it never
// observed — and the entire purpose of these three is to tell an operator when
// one of their knobs can never fire.
func TestAnUnsetTimingIsUnknownAndIsNeverTheDocumentedDefault(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig(mustFixture(t, "minimal_child_routes_v0.28.1.yaml"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	tm := cfg.Timings
	if tm.GroupWait != nil {
		t.Fatalf("group_wait = %s, want unknown: Alertmanager did not publish one, and "+
			"30s would be a number oto invented", seconds(tm.GroupWait))
	}
	if tm.GroupInterval != nil {
		t.Fatalf("group_interval = %s, want unknown", seconds(tm.GroupInterval))
	}
	if tm.RepeatInterval != nil {
		t.Fatalf("repeat_interval = %s, want unknown", seconds(tm.RepeatInterval))
	}
	if tm.Known() {
		t.Fatal("Known() is true when nothing was observed")
	}

	// The child route DOES state two of them, and that is the caveat this count
	// exists to surface: the top-level answer does not govern everything.
	if tm.ChildRoutes != 1 {
		t.Fatalf("child routes = %d, want 1", tm.ChildRoutes)
	}
	if tm.ChildrenWithTimings != 1 {
		t.Fatalf("children with timings = %d, want 1", tm.ChildrenWithTimings)
	}
}

// TestChildRoutesAreCountedToTheirFullDepth. A timing three levels down still
// governs whatever matches it, so it still qualifies the top-level answer.
func TestChildRoutesAreCountedToTheirFullDepth(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig(`
route:
  receiver: root
  group_wait: 30s
  routes:
    - receiver: a
      routes:
        - receiver: b
          group_interval: 1m
        - receiver: c
    - receiver: d
      repeat_interval: 12h
receivers:
  - name: root
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tm := cfg.Timings
	if tm.ChildRoutes != 4 {
		t.Fatalf("child routes = %d, want 4 across two levels", tm.ChildRoutes)
	}
	if tm.ChildrenWithTimings != 2 {
		t.Fatalf("children with timings = %d, want 2", tm.ChildrenWithTimings)
	}
	if tm.GroupWait == nil || *tm.GroupWait != 30*time.Second {
		t.Fatalf("the top-level group_wait was lost: %s", seconds(tm.GroupWait))
	}
}

// TestAZeroTimingIsObservedRatherThanUnknown. `group_wait: 0s` is a real setting
// — "notify at once" — and collapsing it into "unknown" would hide a
// configuration choice that has a large effect on how oto sees flaps.
func TestAZeroTimingIsObservedRatherThanUnknown(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig("route:\n  receiver: r\n  group_wait: 0s\nreceivers:\n  - name: r\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Timings.GroupWait == nil {
		t.Fatal("group_wait: 0s was reported as unknown")
	}
	if *cfg.Timings.GroupWait != 0 {
		t.Fatalf("group_wait = %v, want 0", *cfg.Timings.GroupWait)
	}
}

// TestAnUnparseableTimingIsUnknownRatherThanZero. A future or malformed duration
// must not become a number; it must become an honest gap.
func TestAnUnparseableTimingIsUnknownRatherThanZero(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig("route:\n  receiver: r\n  group_interval: soon\nreceivers:\n  - name: r\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Timings.GroupInterval != nil {
		t.Fatalf("group_interval = %s, want unknown", seconds(cfg.Timings.GroupInterval))
	}
}

// TestPromDurationsAlertmanagerAcceptsAreParsed. `4h`, `1d` and `1h30m` are all
// legal `model.Duration`s and none of them is a `time.ParseDuration` input.
func TestPromDurationsAlertmanagerAcceptsAreParsed(t *testing.T) {
	t.Parallel()

	cases := map[string]time.Duration{
		"30s":   30 * time.Second,
		"5m":    5 * time.Minute,
		"4h":    4 * time.Hour,
		"1h30m": 90 * time.Minute,
		"1d":    24 * time.Hour,
		"500ms": 500 * time.Millisecond,
		"1w":    7 * 24 * time.Hour,
	}
	for in, want := range cases {
		cfg, err := parseConfig("route:\n  receiver: r\n  group_wait: " + in + "\nreceivers:\n  - name: r\n")
		if err != nil {
			t.Fatalf("%s: parse: %v", in, err)
		}
		if cfg.Timings.GroupWait == nil || *cfg.Timings.GroupWait != want {
			t.Fatalf("%s parsed as %s, want %v", in, seconds(cfg.Timings.GroupWait), want)
		}
	}
}

// TestAgainstARealAlertmanager runs the parser over a LIVE server's own
// `config.original`.
//
// ⭐ WHY IT IS NOT ONLY A FIXTURE. A fixture proves oto agrees with a string
// somebody once captured; this proves oto agrees with the program. Point it at
// the repository's compose Alertmanager:
//
//	docker compose up -d --wait alertmanager
//	OTO_TEST_ALERTMANAGER_URL=http://localhost:9093 go test ./internal/sources/client/alertmanager/...
//
// It SKIPS rather than fails when no URL is set, because `go test ./...` must not
// require a container to be up.
func TestAgainstARealAlertmanager(t *testing.T) {
	base := os.Getenv("OTO_TEST_ALERTMANAGER_URL")
	if base == "" {
		t.Skip("set OTO_TEST_ALERTMANAGER_URL to run against a live Alertmanager")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+PathStatus, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", base+PathStatus, err)
	}
	defer resp.Body.Close() //nolint:errcheck // a test read

	var w wireStatus
	if err := json.NewDecoder(resp.Body).Decode(&w); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if w.Config.Original == "" {
		t.Fatal("the live Alertmanager returned no config.original")
	}

	cfg, err := parseConfig(w.Config.Original)
	if err != nil {
		t.Fatalf("parse the live config: %v", err)
	}
	t.Logf("alertmanager %s: group_wait=%s group_interval=%s repeat_interval=%s child_routes=%d/%d",
		w.VersionInfo.Version, seconds(cfg.Timings.GroupWait), seconds(cfg.Timings.GroupInterval),
		seconds(cfg.Timings.RepeatInterval), cfg.Timings.ChildrenWithTimings, cfg.Timings.ChildRoutes)

	// The repository's own compose Alertmanager states all three, so a live run
	// against it is expected to observe all three. Any OTHER Alertmanager may
	// legitimately publish none — which is exactly the `unknown` case — so the
	// assertion is only made when the receiver names it.
	if _, isCompose := cfg.SendResolved["oto"]; !isCompose {
		return
	}
	if cfg.Timings.GroupWait == nil || cfg.Timings.GroupInterval == nil || cfg.Timings.RepeatInterval == nil {
		t.Fatalf("the compose Alertmanager states all three and oto read %s / %s / %s",
			seconds(cfg.Timings.GroupWait), seconds(cfg.Timings.GroupInterval),
			seconds(cfg.Timings.RepeatInterval))
	}
}
