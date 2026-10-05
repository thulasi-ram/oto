package service

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/sources/client/prometheus"
	"github.com/thulasiram/oto/internal/sources/domain"
	"github.com/thulasiram/oto/internal/sources/rulematch"
)

// FOLLOWING A VMALERT generatorURL, git-bug 766709c.
//
// vmalert's default link names the vmalert that evaluated the rule and carries
// no expression. ParseGeneratorURL still refuses it — no expression, no parse —
// so the FollowGeneratorURL branch used to derive no override and query nothing.
// These tests drive ResolveRule end to end against oto's real Prometheus client
// and a vmalert-shaped server; only the factory and the repository are fakes.

// overrideFactory records every override the service asked for, then builds the
// real client exactly as ruleSourceFactory does.
type overrideFactory struct {
	ruleSourceFactory
	overrides []string
}

func (f *overrideFactory) Prometheus(src domain.Source, cred domain.Credential, override string) (PrometheusClient, error) {
	f.overrides = append(f.overrides, override)
	return f.ruleSourceFactory.Prometheus(src, cred, override)
}

// vmalertRule is the one rule the vmalert-shaped server holds, spelled the way
// vmalert spells it.
const vmalertRule = `{"status":"success","data":{"groups":[{"name":"node","file":"/etc/vmalert/node.yml","interval":30,
	"id":"1036955090143761274","rules":[{"type":"alerting","name":"InstanceDown","query":"up == 0",
	"duration":300,"keep_firing_for":60,"labels":{"severity":"page"},
	"id":"1074584496268461589","group_id":"1036955090143761274"}]}]}}`

func resolveAgainst(t *testing.T, src domain.Source, q RuleQuery) (rulematch.Match, *overrideFactory) {
	t.Helper()
	f := &overrideFactory{}
	s := &Service{
		repo:    &writeRepo{src: src},
		clients: f,
		clk:     clock.NewFake(probeNow),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	m, err := s.ResolveRule(context.Background(), db.TenantScope{}, src.ID, q)
	if err != nil {
		t.Fatalf("ResolveRule: %v", err)
	}
	return m, f
}

func TestFollowingAVmalertLinkQueriesTheVmalertRoot(t *testing.T) {
	var srv ruleServer
	root := srv.start(t, map[string]string{prometheus.PathRules: vmalertRule})
	link := root + "/vmalert/alert?group_id=1036955090143761274&alert_id=1074584496268461589"

	// No prometheus_url: the generatorURL is the ONLY way to find the vmalert.
	src := domain.Source{ID: uuid.New()}
	m, f := resolveAgainst(t, src, RuleQuery{
		Labels:             map[string]string{"alertname": "InstanceDown", "severity": "page"},
		GeneratorURL:       link,
		FollowGeneratorURL: true,
	})

	if len(f.overrides) != 1 || f.overrides[0] != root {
		t.Fatalf("overrides = %q, want exactly the vmalert root %q", f.overrides, root)
	}
	if !srv.asked(prometheus.PathRules) {
		t.Fatal("the vmalert root's rules API was never queried")
	}
	if m.Origin != rulematch.OriginPrometheusAPI || m.PrometheusURL != root {
		t.Errorf("origin=%q url=%q; want the rule from %s's rules API", m.Origin, m.PrometheusURL, root)
	}
	if m.Expr != "up == 0" || m.ForSeconds != 300 || m.KeepFiringForSeconds != 60 {
		t.Errorf("expr=%q for=%v keep_firing_for=%v; want the vmalert rule as written",
			m.Expr, m.ForSeconds, m.KeepFiringForSeconds)
	}
}

// TestNotFollowingQueriesNothingNew: following is a flag on the query, and
// without it a source with no prometheus_url asks no server at all, vmalert link
// or not. (internal/app's rule lookup sets the flag whenever upstream calls are
// allowed, so in a running oto this is the SkipUpstream path.)
func TestNotFollowingQueriesNothingNew(t *testing.T) {
	var srv ruleServer
	root := srv.start(t, map[string]string{prometheus.PathRules: vmalertRule})

	src := domain.Source{ID: uuid.New()}
	_, f := resolveAgainst(t, src, RuleQuery{
		Labels:       map[string]string{"alertname": "InstanceDown"},
		GeneratorURL: root + "/vmalert/alert?group_id=1&alert_id=2",
	})

	if len(f.overrides) != 0 || srv.asked(prometheus.PathRules) {
		t.Fatalf("FollowGeneratorURL unset, yet a rules API was queried (overrides %q)", f.overrides)
	}
}

// vmEmptyRules is what VictoriaMetrics itself answers on /api/v1/rules when it
// is not proxying to a vmalert: a stub with no groups, so Grafana does not error.
const vmEmptyRules = `{"status":"success","data":{"groups":[]}}`

// TestAFollowedURLThatMissesFallsBackToTheConfiguredOne is the regression the
// vmui parse would otherwise have introduced. The recommended vmui link names
// the VictoriaMetrics UI, not the vmalert; once it parsed, following it sent the
// lookup there and never to the vmalert configured as prometheus_url.
func TestAFollowedURLThatMissesFallsBackToTheConfiguredOne(t *testing.T) {
	var ui, vmalert ruleServer
	uiRoot := ui.start(t, map[string]string{prometheus.PathRules: vmEmptyRules})
	vmalertRoot := vmalert.start(t, map[string]string{prometheus.PathRules: vmalertRule})

	src := domain.Source{ID: uuid.New(), PrometheusURL: vmalertRoot}
	m, f := resolveAgainst(t, src, RuleQuery{
		Labels:             map[string]string{"alertname": "InstanceDown", "severity": "page"},
		GeneratorURL:       uiRoot + "/vmui/#/?g0.expr=up%20%3D%3D%200",
		FollowGeneratorURL: true,
	})

	if len(f.overrides) != 2 || f.overrides[0] != uiRoot || f.overrides[1] != "" {
		t.Fatalf("overrides = %q, want the followed root and then the configured URL", f.overrides)
	}
	if m.Origin != rulematch.OriginPrometheusAPI || m.PrometheusURL != vmalertRoot {
		t.Fatalf("origin=%q url=%q; want the rule from the configured vmalert %s", m.Origin, m.PrometheusURL, vmalertRoot)
	}
	if m.ForSeconds != 300 || m.KeepFiringForSeconds != 60 {
		t.Errorf("for=%v keep_firing_for=%v; the configured vmalert's definition was not used", m.ForSeconds, m.KeepFiringForSeconds)
	}
}

// TestAFollowedURLThatHitsIsNotSecondGuessed: in the federated case following
// exists for, the followed server holds the rule and the configured one is
// never asked.
func TestAFollowedURLThatHitsIsNotSecondGuessed(t *testing.T) {
	var followed, configured ruleServer
	followedRoot := followed.start(t, map[string]string{prometheus.PathRules: vmalertRule})
	configuredRoot := configured.start(t, map[string]string{prometheus.PathRules: vmEmptyRules})

	src := domain.Source{ID: uuid.New(), PrometheusURL: configuredRoot}
	m, f := resolveAgainst(t, src, RuleQuery{
		Labels:             map[string]string{"alertname": "InstanceDown", "severity": "page"},
		GeneratorURL:       followedRoot + "/graph?g0.expr=up+%3D%3D+0&g0.tab=1",
		FollowGeneratorURL: true,
	})

	if len(f.overrides) != 1 || configured.asked(prometheus.PathRules) {
		t.Fatalf("the configured URL was asked although the followed one held the rule (overrides %q)", f.overrides)
	}
	if m.PrometheusURL != followedRoot {
		t.Errorf("url=%q, want the followed %s", m.PrometheusURL, followedRoot)
	}
}

// otherRule is a server that ignored rule_name[] and answered with a rule that is
// not the one asked for.
const otherRule = `{"status":"success","data":{"groups":[{"name":"disk","file":"/etc/vmalert/disk.yml","interval":30,
	"rules":[{"type":"alerting","name":"DiskFull","query":"disk_free < 0.1","duration":60}]}]}}`

// TestAFollowedAnswerWithoutTheRuleIsAMiss: a non-empty answer is not a hit until
// the named rule is in it. A followed server that ignores the filter and answers
// with other rules would otherwise stop the lookup short of the configured one.
func TestAFollowedAnswerWithoutTheRuleIsAMiss(t *testing.T) {
	var followed, configured ruleServer
	followedRoot := followed.start(t, map[string]string{prometheus.PathRules: otherRule})
	configuredRoot := configured.start(t, map[string]string{prometheus.PathRules: vmalertRule})

	src := domain.Source{ID: uuid.New(), PrometheusURL: configuredRoot}
	m, f := resolveAgainst(t, src, RuleQuery{
		Labels:             map[string]string{"alertname": "InstanceDown", "severity": "page"},
		GeneratorURL:       followedRoot + "/graph?g0.expr=up+%3D%3D+0&g0.tab=1",
		FollowGeneratorURL: true,
	})

	if len(f.overrides) != 2 || f.overrides[0] != followedRoot || f.overrides[1] != "" {
		t.Fatalf("overrides = %q, want the followed root and then the configured URL", f.overrides)
	}
	if m.PrometheusURL != configuredRoot || m.ForSeconds != 300 {
		t.Errorf("url=%q for=%v; want InstanceDown from the configured %s", m.PrometheusURL, m.ForSeconds, configuredRoot)
	}
}

func TestHoldsRulesNamesTheRule(t *testing.T) {
	groups := []domain.RuleGroup{{Rules: []domain.AlertingRule{{Name: "DiskFull"}}}}
	if holdsRules(groups, "InstanceDown") {
		t.Error("a group holding only DiskFull held InstanceDown")
	}
	if !holdsRules(groups, "DiskFull") {
		t.Error("a group holding DiskFull did not hold it")
	}
	if !holdsRules(groups, "") {
		t.Error("with no name, any rule is a hit")
	}
	if holdsRules([]domain.RuleGroup{{Name: "empty"}}, "") {
		t.Error("a group with no rules is a miss, whatever the group count")
	}
}

// TestAVmalertRootAsksTheConfiguredURLFirst: the root is what vmalert says about
// itself, the configured prometheus_url is what the operator gave oto — so with
// one configured, the root is only the fallback.
func TestAVmalertRootAsksTheConfiguredURLFirst(t *testing.T) {
	var root, configured ruleServer
	vmalertRoot := root.start(t, map[string]string{prometheus.PathRules: vmalertRule})
	configuredRoot := configured.start(t, map[string]string{prometheus.PathRules: vmalertRule})
	link := vmalertRoot + "/vmalert/alert?group_id=1036955090143761274&alert_id=1074584496268461589"

	src := domain.Source{ID: uuid.New(), PrometheusURL: configuredRoot}
	m, f := resolveAgainst(t, src, RuleQuery{
		Labels:             map[string]string{"alertname": "InstanceDown", "severity": "page"},
		GeneratorURL:       link,
		FollowGeneratorURL: true,
	})

	if len(f.overrides) != 1 || f.overrides[0] != "" || root.asked(prometheus.PathRules) {
		t.Fatalf("overrides = %q; the configured URL held the rule, so the vmalert root must not be asked", f.overrides)
	}
	if m.PrometheusURL != configuredRoot {
		t.Errorf("url=%q, want the configured %s", m.PrometheusURL, configuredRoot)
	}
}

// TestAVmalertRootIsTheFallbackWhenTheConfiguredURLMisses: the root is still
// followed, after the configured URL answered without the rule.
func TestAVmalertRootIsTheFallbackWhenTheConfiguredURLMisses(t *testing.T) {
	var root, configured ruleServer
	vmalertRoot := root.start(t, map[string]string{prometheus.PathRules: vmalertRule})
	configuredRoot := configured.start(t, map[string]string{prometheus.PathRules: vmEmptyRules})

	src := domain.Source{ID: uuid.New(), PrometheusURL: configuredRoot}
	m, f := resolveAgainst(t, src, RuleQuery{
		Labels:             map[string]string{"alertname": "InstanceDown", "severity": "page"},
		GeneratorURL:       vmalertRoot + "/vmalert/alert?group_id=1&alert_id=2",
		FollowGeneratorURL: true,
	})

	if len(f.overrides) != 2 || f.overrides[0] != "" || f.overrides[1] != vmalertRoot {
		t.Fatalf("overrides = %q, want the configured URL and then the vmalert root", f.overrides)
	}
	if m.PrometheusURL != vmalertRoot || m.ForSeconds != 300 {
		t.Errorf("url=%q for=%v; want the rule from the vmalert root %s", m.PrometheusURL, m.ForSeconds, vmalertRoot)
	}
}
