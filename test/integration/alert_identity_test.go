package integration

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/app"
)

// ⭐ THIS FILE ANSWERS ONE QUESTION AGAINST A REAL DATABASE: is `severity` part
// of an Alert's identity?
//
// It exists because a feature was built on the assumption that it is NOT. ADR
// 0020 proposed broadcasting a `warning` → `critical` transition, and a
// `severity_raised` notification Reason plus a migration were written to express
// it. Both assumed that one Alert can be observed at two severities over its
// lifetime — that a severity change is an UPDATE of an existing row.
//
// In Prometheus `severity` is an ordinary LABEL, and SPEC §C.2 hashes the
// canonical label set minus the source's `ignore_labels` into `alert_key`. So
// unless `severity` is in that ignore set, two observations differing only in
// severity are two DIFFERENT Alerts with two different identities, two rows and
// two threads — and "this alert got worse" is a transition oto can never see.
//
// The ignore set is a per-source column with a DDL default, so the question is
// answerable only against a real schema. That is what this test does, and it is
// kept permanently: if someone ever adds `severity` to the shipped default, the
// whole of §C.2's identity story changes and this test is where they find out.
func TestSeverityIsPartOfAlertIdentity(t *testing.T) {
	env := newEnv(t)

	boot, err := app.Bootstrap(env.ctx, env.pool, app.BootstrapRequest{
		OrgSlug: "ident", Email: "ops@ident.example", Password: "correct-horse-battery-staple",
	}, time.Now())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	var cluster struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	env.do(t, http.MethodPost, "/api/v1/clusters", boot.Token, map[string]any{
		"cluster_key": "prod-eu", "display_name": "Production EU",
	}, http.StatusCreated, &cluster)

	// A source created the way every install creates one: no `ignore_labels` of
	// its own, so the column takes the DDL default. THAT DEFAULT IS THE POLICY.
	var created struct {
		Data struct {
			Source struct {
				ID           string   `json:"id"`
				IgnoreLabels []string `json:"ignore_labels"`
			} `json:"source"`
		} `json:"data"`
	}
	env.do(t, http.MethodPost, "/api/v1/sources", boot.Token, map[string]any{
		"name": "am-eu-1", "cluster_id": cluster.Data.ID,
		"kind": "alertmanager", "base_url": "https://alertmanager.ident.example",
	}, http.StatusCreated, &created)

	// The policy as the DATABASE holds it, not as Go remembers it.
	var stored []string
	if err := env.pool.QueryRow(env.ctx,
		`SELECT ignore_labels FROM alert_sources WHERE id = $1`,
		created.Data.Source.ID).Scan(&stored); err != nil {
		t.Fatalf("read ignore_labels: %v", err)
	}
	if slices.Contains(stored, domain.LabelSeverity) {
		t.Fatalf("the shipped ignore-label policy now contains %q (%v) — "+
			"alert identity has changed meaning and §C.2 needs re-reading",
			domain.LabelSeverity, stored)
	}

	// ⭐ THE ASSERTION. Two observations of the same rule, on the same instance,
	// differing in exactly one label value: `severity`.
	orgID := boot.OrgID
	clusterKey, err := domain.NewClusterKey("prod-eu")
	if err != nil {
		t.Fatalf("cluster key: %v", err)
	}

	labelSet := func(severity string) domain.LabelSet {
		ls, lerr := domain.NewLabelSet(map[string]string{
			"alertname": "HighErrorRate",
			"instance":  "web-1",
			"severity":  severity,
		})
		if lerr != nil {
			t.Fatalf("label set: %v", lerr)
		}
		return ls
	}

	warn := domain.ComputeAlertKey(orgID, clusterKey, labelSet("warning"), stored)
	crit := domain.ComputeAlertKey(orgID, clusterKey, labelSet("critical"), stored)

	if warn.String() == crit.String() {
		t.Fatalf("severity is ignored by the identity hash: both observations are %s. "+
			"A severity rise would then be an UPDATE of one Alert and a "+
			"`severity_raised` transition would be reachable", warn)
	}

	// The consequence, spelled out so the reason this matters survives the test
	// name: `alerts` is keyed UNIQUE (org_id, alert_key), so these two are two
	// rows. There is no row that was `warning` and is now `critical`, and
	// therefore no severity-rise transition for oto to observe or announce.
	t.Logf("warning=%s critical=%s — different identities, so a severity rise is "+
		"a NEW Alert, never a change to an existing one", warn, crit)
}
