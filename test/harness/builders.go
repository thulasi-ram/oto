package harness

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
)

// The builders below seed the FK graph a test needs and nothing else. They write
// SQL directly, and that is deliberate: `test/harness` cannot import a service
// package without creating an import cycle for the in-package tests of that very
// service (`package service` importing a package that imports `service`), and a
// fixture is not the thing under test. Anything that IS under test — the ingest
// path, the lifecycle machine, the dispatcher — must still be driven through the
// real service, which is what ADR 0021 means by "real collaborators".
//
// Every builder mints UUIDv7 through platform/id, because time-ordered ids are
// what the index locality of this schema assumes.

// Org is one seeded tenant, with the TenantScope every repository method wants.
type Org struct {
	// ID is orgs.id.
	ID uuid.UUID
	// Slug is the URL-safe handle, unique per database.
	Slug string
	// Name is the display name.
	Name string
	// Scope is the proof of authentication into this org. It is the one thing a
	// test cannot construct by hand: TenantScope's field is unexported and
	// db.NewTenantScope is the only door.
	Scope db.TenantScope
}

// Org seeds a fresh tenant with a random slug.
func (h *H) Org() Org { return h.OrgNamed(uniqueSlug("org")) }

// OrgNamed seeds a tenant with an explicit slug, for a test that asserts on it.
func (h *H) OrgNamed(slug string) Org {
	h.T.Helper()
	org := Org{ID: id.New(), Slug: slug, Name: "Org " + slug}
	// `created_at`/`updated_at` are NAMED and take the harness clock, like every
	// other timestamp a builder writes. 00033 removed the database default from
	// this table for the same reason 00032 removed it from `channels`: `orgs`
	// timestamps come from the application, so a seed that let `now()` fill them
	// would be seeding a row the product itself cannot write.
	h.Exec(`INSERT INTO orgs (id, slug, name, created_at, updated_at) VALUES ($1, $2, $3, $4, $4)`,
		org.ID, org.Slug, org.Name, h.Now())
	org.Scope = Scope(h.T, org.ID)
	return org
}

// Scope builds the TenantScope for an org id.
//
// It is exported because it is the only sanctioned construction path outside
// platform/authn, and every repository call in every test needs one.
func Scope(t *testing.T, orgID uuid.UUID) db.TenantScope {
	t.Helper()
	s, err := db.NewTenantScope(orgID)
	if err != nil {
		t.Fatalf("harness: tenant scope: %v", err)
	}
	return s
}

// User is one seeded human principal.
type User struct {
	// ID is users.id.
	ID uuid.UUID
	// OrgID is the tenant this user belongs to.
	OrgID uuid.UUID
	// Email is the login identity.
	Email string
	// DisplayName is what the timeline shows as the actor.
	DisplayName string
}

// User seeds a member of org. The password hash is left NULL, which the schema
// documents as "password login disabled" — a builder must never mint a
// credential a test did not ask for.
func (h *H) User(org Org) User {
	h.T.Helper()
	u := User{
		ID:          id.New(),
		OrgID:       org.ID,
		Email:       uniqueSlug("user") + "@example.test",
		DisplayName: "Test User",
	}
	// `created_at`/`updated_at` are NAMED and take the harness clock. 00034 removed
	// this table's DEFAULT now() for the reason 00032 removed `channels`': the row
	// is written by the application, so a seed that let `now()` fill it would be
	// seeding a row the product itself cannot write.
	h.Exec(`INSERT INTO users (id, org_id, email, display_name, created_at, updated_at)
	        VALUES ($1, $2, $3, $4, $5, $5)`,
		u.ID, u.OrgID, u.Email, u.DisplayName, h.Now())
	return u
}

// Cluster is one seeded identity and failure domain.
type Cluster struct {
	// ID is clusters.id.
	ID uuid.UUID
	// OrgID is the tenant.
	OrgID uuid.UUID
	// Key is the cluster_key that PARTICIPATES IN ALERT IDENTITY (SPEC §C.2).
	Key alerts.ClusterKey
}

// Cluster seeds a cluster named "prod".
func (h *H) Cluster(org Org) Cluster { return h.ClusterNamed(org, "prod") }

// ClusterNamed seeds a cluster with an explicit cluster_key.
func (h *H) ClusterNamed(org Org, key string) Cluster {
	h.T.Helper()
	c := Cluster{ID: id.New(), OrgID: org.ID, Key: ClusterKey(h.T, key)}
	// `created_at`/`updated_at` are NAMED and take the harness clock; 00034 removed
	// this table's DEFAULT now(). See `User` above.
	h.Exec(`INSERT INTO clusters (id, org_id, cluster_key, display_name, created_at, updated_at)
	        VALUES ($1, $2, $3, $4, $5, $5)`,
		c.ID, c.OrgID, key, key, h.Now())
	return c
}

// Source is one seeded AlertSource.
type Source struct {
	// ID is alert_sources.id.
	ID uuid.UUID
	// OrgID is the tenant.
	OrgID uuid.UUID
	// ClusterID is the cluster this source observes.
	ClusterID uuid.UUID
	// Name is unique per org.
	Name string
	// Kind is `alertmanager` or `grafana` (alert_sources_kind_ck). It decides what
	// BaseURL actually addresses, so a test about a deep link has to state it.
	Kind string
	// BaseURL is the Alertmanager API root, with no trailing slash. For a
	// `grafana` source it is the AM-compat prefix, which is NOT a UI root.
	BaseURL string
}

// Source seeds an alertmanager-kind source pointed at an unreachable URL.
//
// Point it at an Alertmanager fake with SourceAt when the test actually probes
// it; the default is deliberately not routable so that an accidental outbound
// call fails instead of escaping.
func (h *H) Source(org Org, cl Cluster) Source {
	return h.SourceAt(org, cl, "https://am.invalid.example")
}

// SourceAt seeds an alertmanager-kind source whose base_url is baseURL —
// normally `fake.URL()` from an Alertmanager fake.
func (h *H) SourceAt(org Org, cl Cluster, baseURL string) Source {
	return h.SourceOfKind(org, cl, "alertmanager", baseURL)
}

// GrafanaSourceAt seeds a `grafana`-kind source at baseURL.
//
// Same table, same column, a DIFFERENT claim about what that URL addresses: a
// grafana base_url is an Alertmanager-COMPATIBLE API prefix, and Grafana's own
// console is not behind it (SPEC §L.3). Behaviour turns on that difference — the
// per-silence deep link, the card's Silence button — and a suite that can only
// seed alertmanagers cannot show that it does.
func (h *H) GrafanaSourceAt(org Org, cl Cluster, baseURL string) Source {
	return h.SourceOfKind(org, cl, "grafana", baseURL)
}

// SourceOfKind seeds a source of an explicit kind. `alert_sources_kind_ck`
// rejects anything but `alertmanager` and `grafana`, so a typo fails here rather
// than quietly seeding a source no code path handles.
func (h *H) SourceOfKind(org Org, cl Cluster, kind, baseURL string) Source {
	h.T.Helper()
	s := Source{
		ID:        id.New(),
		OrgID:     org.ID,
		ClusterID: cl.ID,
		Name:      uniqueSlug("src"),
		Kind:      kind,
		BaseURL:   baseURL,
	}
	// `created_at`/`updated_at` are NAMED and take the harness clock; 00034 removed
	// this table's DEFAULT now(). See `User` above.
	h.Exec(`INSERT INTO alert_sources (id, org_id, cluster_id, name, kind, base_url,
	                                   created_at, updated_at)
	        VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`,
		s.ID, s.OrgID, s.ClusterID, s.Name, s.Kind, s.BaseURL, h.Now())
	return s
}

// Group is one seeded AlertGroup generation. One generation owns exactly one
// Slack thread, which is why the generation is part of the identity.
type Group struct {
	// ID is alert_groups.id.
	ID uuid.UUID
	// OrgID is the tenant.
	OrgID uuid.UUID
	// SourceID is the source the group was routed from.
	SourceID uuid.UUID
	// ClusterID is the cluster.
	ClusterID uuid.UUID
	// Key is the DURABLE group identity (SPEC §C.4) — stable across
	// alertmanager.yml route edits, which AM's own groupKey is not.
	Key alerts.GroupKey
	// Generation is 1 for a freshly built group.
	Generation int
	// Labels are the group labels the key was derived from.
	Labels map[string]string
}

// Group seeds an open group for a default alert label set.
func (h *H) Group(org Org, src Source, cl Cluster) Group {
	return h.GroupWith(org, src, cl, map[string]string{
		"alertname": "HarnessAlert",
		"namespace": "harness",
		"severity":  "critical",
	})
}

// GroupWith seeds an open group for one ALERT'S label set. The group_key is
// COMPUTED, never invented: `groups_key_ck` accepts only `gk_` plus 26
// base32hex characters, and a hand-rolled string that happens to match is a
// group whose identity does not agree with the ingest path's.
//
// ⭐ The argument is the alert's labels, not Alertmanager's groupLabels. Since
// ADR 0038 the group key is `(org, cluster, alertname, namespace-or-∅)` and the
// group's own `group_labels` are `SplitLabels` of the same set, so seeding a
// group means naming an alert — which is what a group has always meant.
func (h *H) GroupWith(org Org, src Source, cl Cluster, alertLabels map[string]string) Group {
	h.T.Helper()

	labels, err := alerts.NewLabelSet(alertLabels)
	if err != nil {
		h.T.Fatalf("harness: group labels: %v", err)
	}
	g := Group{
		ID:         id.New(),
		OrgID:      org.ID,
		SourceID:   src.ID,
		ClusterID:  cl.ID,
		Key:        alerts.ComputeGroupKey(org.ID, cl.Key, labels),
		Generation: 1,
		Labels:     alerts.SplitLabels(labels).Map(),
	}
	now := h.Now()
	h.Exec(`INSERT INTO alert_groups
	          (id, org_id, source_id, cluster_id, group_key, generation, title, state,
	           group_labels, first_seen_at, last_activity_at)
	        VALUES ($1, $2, $3, $4, $5, $6, $7, 'open', $8::jsonb, $9, $9)`,
		g.ID, g.OrgID, g.SourceID, g.ClusterID, g.Key.String(), g.Generation,
		"A group", jsonObject(h.T, g.Labels), now)
	return g
}

// Alert is one seeded Alert — the durable identity, not the episode.
type Alert struct {
	// ID is alerts.id.
	ID uuid.UUID
	// OrgID is the tenant.
	OrgID uuid.UUID
	// ClusterID is the cluster.
	ClusterID uuid.UUID
	// Key is the alert_key (SPEC §C.2). Computed, never invented.
	Key alerts.AlertKey
	// Labels is the full label set.
	Labels alerts.LabelSet
}

// DefaultLabels is the label set the builders use when a test does not care
// what the alert is about, only that it exists.
func DefaultLabels() map[string]string {
	return map[string]string{
		"alertname": "HighErrorRate",
		"severity":  "critical",
		"service":   "checkout",
	}
}

// Alert seeds a firing Alert over DefaultLabels.
func (h *H) Alert(org Org, cl Cluster) Alert {
	return h.AlertWith(org, cl, DefaultLabels())
}

// AlertWith seeds a firing Alert over explicit labels.
//
// ⚠️ This writes the row directly and therefore does NOT run the lifecycle
// machine. Use it for a test that needs an Alert to exist; drive
// `alerts/service.ObserveBatch` for a test that is ABOUT how an Alert comes to
// exist.
func (h *H) AlertWith(org Org, cl Cluster, kv map[string]string) Alert {
	h.T.Helper()

	labels := Labels(h.T, kv)
	a := Alert{
		ID:        id.New(),
		OrgID:     org.ID,
		ClusterID: cl.ID,
		Key:       alerts.ComputeAlertKey(org.ID, cl.Key, labels, nil),
		Labels:    labels,
	}
	severity, _ := labels.Get("severity")
	now := h.Now()
	h.Exec(`INSERT INTO alerts
	          (id, org_id, cluster_id, alert_key, source_fingerprint, alertname, severity,
	           cluster_key, labels, state, first_seen_at, last_seen_at, last_state_change_at,
	           total_cases)
	        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, 'firing', $10, $10, $10, 1)`,
		a.ID, a.OrgID, a.ClusterID, a.Key.String(),
		alerts.ComputeSourceFingerprint(labels).String(),
		labels.AlertName(), nullable(severity), cl.Key.String(),
		jsonObject(h.T, labels.Map()), now)
	return a
}

// Case is one seeded AlertCase — the episode the state machine runs
// on.
type Case struct {
	// ID is alert_cases.id.
	ID uuid.UUID
	// OrgID is the tenant.
	OrgID uuid.UUID
	// AlertID is the Alert this episode belongs to.
	AlertID uuid.UUID
	// GroupID is the group generation, or uuid.Nil when ungrouped.
	GroupID uuid.UUID
	// Seq is 1, 2, 3... per alert.
	Seq int
}

// Case seeds an open, firing case of a and points the Alert's
// current_case_id at it, which is the projection the read path uses.
func (h *H) Case(a Alert, g Group) Case {
	h.T.Helper()

	o := Case{ID: id.New(), OrgID: a.OrgID, AlertID: a.ID, GroupID: g.ID, Seq: 1}
	now := h.Now()
	h.Exec(`INSERT INTO alert_cases
	          (id, org_id, alert_id, group_id, seq, state, started_at, last_observed_at,
	           source_starts_at, source_updated_at)
	        VALUES ($1, $2, $3, $4, $5, 'open', $6, $6, $6, $6)`,
		o.ID, o.OrgID, o.AlertID, nullableUUID(o.GroupID), o.Seq, now)
	h.Exec(`UPDATE alerts SET current_case_id = $1 WHERE id = $2`, o.ID, a.ID)
	return o
}

// ------------------------------------------------------- domain value helpers

// Labels builds a LabelSet or fails the test. A LabelSet is validated at
// construction (§L.3), so a builder that swallowed the error would hand back a
// zero value and fail somewhere unrelated.
func Labels(t *testing.T, kv map[string]string) alerts.LabelSet {
	t.Helper()
	ls, err := alerts.NewLabelSet(kv)
	if err != nil {
		t.Fatalf("harness: label set: %v", err)
	}
	return ls
}

// ClusterKey builds a ClusterKey or fails the test.
func ClusterKey(t *testing.T, s string) alerts.ClusterKey {
	t.Helper()
	k, err := alerts.NewClusterKey(s)
	if err != nil {
		t.Fatalf("harness: cluster key: %v", err)
	}
	return k
}

// AlertKey computes the alert identity the ingest path would compute for the
// same inputs.
func AlertKey(orgID uuid.UUID, cl alerts.ClusterKey, labels alerts.LabelSet) alerts.AlertKey {
	return alerts.ComputeAlertKey(orgID, cl, labels, nil)
}

// ---------------------------------------------------------------------- misc

// uniqueSlug returns a slug that satisfies orgs_slug_ck and is unique per run.
func uniqueSlug(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, uuid.NewString()[:8])
}

func jsonObject(t *testing.T, kv map[string]string) []byte {
	t.Helper()
	if kv == nil {
		kv = map[string]string{}
	}
	raw, err := json.Marshal(kv)
	if err != nil {
		t.Fatalf("harness: encode labels: %v", err)
	}
	return raw
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableUUID(u uuid.UUID) any {
	if u == uuid.Nil {
		return nil
	}
	return u
}
