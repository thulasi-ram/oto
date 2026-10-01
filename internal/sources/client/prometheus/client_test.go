package prometheus_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/sources/client/prometheus"
)

// rulesServer answers GET /api/v1/rules with body and nothing else.
func rulesServer(t *testing.T, body string) *prometheus.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != prometheus.PathRules {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := prometheus.New(prometheus.Config{
		BaseURL:   srv.URL,
		Clock:     clock.NewFake(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)),
		UserAgent: "oto-test",
	})
	require.NoError(t, err)
	return c
}

// The two servers' answers for the SAME rule — `for: 10m`, `keep_firing_for: 5m`
// — trimmed to what oto reads. They differ in one key's spelling, and that one
// key is the whole bug.
const (
	prometheusRules = `{"status":"success","data":{"groups":[{"name":"g","file":"/etc/rules.yml","interval":30,
		"rules":[{"type":"alerting","name":"HighLatency","query":"latency > 1","duration":600,
		"keepFiringFor":300,"labels":{"severity":"page"},"health":"ok","state":"inactive"}]}]}}`

	vmalertRules = `{"status":"success","data":{"groups":[{"name":"g","file":"/etc/rules.yml","interval":30,
		"id":"1036955090143761274","type":"prometheus",
		"rules":[{"type":"alerting","name":"HighLatency","query":"latency > 1","duration":600,
		"keep_firing_for":300,"labels":{"severity":"page"},"health":"ok","state":"inactive",
		"id":"1074584496268461589","group_id":"1036955090143761274"}]}]}}`

	noKeepFiringFor = `{"status":"success","data":{"groups":[{"name":"g","file":"f","interval":30,
		"rules":[{"type":"alerting","name":"HighLatency","query":"latency > 1","duration":600}]}]}}`
)

// TestKeepFiringForIsReadInEitherSpelling is git-bug 5a79ff5. Before it, the
// vmalert fixture decoded keep_firing_for as 0, and that 0 went into the
// RuleSnapshot and its fingerprint as a statement about the rule.
func TestKeepFiringForIsReadInEitherSpelling(t *testing.T) {
	cases := []struct {
		name string
		body string
		want float64
	}{
		{"prometheus spells it keepFiringFor", prometheusRules, 300},
		{"vmalert spells it keep_firing_for", vmalertRules, 300},
		{"a rule without it is 0 on either server", noKeepFiringFor, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			groups, err := rulesServer(t, c.body).Rules(context.Background(), []string{"HighLatency"})
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Rules, 1)
			r := groups[0].Rules[0]
			require.Equal(t, c.want, r.KeepFiringFor)
			require.Equal(t, 600.0, r.Duration, "the field both servers spell alike is untouched")
		})
	}
}

// TestTheTwoServersDescribeTheSameRuleIdentically is the property the bug
// broke: the same rule served by Prometheus and by vmalert must decode to the
// same AlertingRule, or the two snapshots fingerprint differently for no
// reason a rule author wrote.
func TestTheTwoServersDescribeTheSameRuleIdentically(t *testing.T) {
	prom, err := rulesServer(t, prometheusRules).Rules(context.Background(), nil)
	require.NoError(t, err)
	vm, err := rulesServer(t, vmalertRules).Rules(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, prom, vm)
}

// TestCamelCaseWinsWhenBothArePresent pins the tie-break. No real server sends
// both today; one that did would be speaking Prometheus's API, which is the one
// oto was written against.
func TestCamelCaseWinsWhenBothArePresent(t *testing.T) {
	body := `{"status":"success","data":{"groups":[{"name":"g","file":"f","interval":30,
		"rules":[{"type":"alerting","name":"A","query":"up == 0","duration":0,
		"keepFiringFor":120,"keep_firing_for":300}]}]}}`
	groups, err := rulesServer(t, body).Rules(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 120.0, groups[0].Rules[0].KeepFiringFor)
}
