package harness_test

import (
	"encoding/json"
	"testing"
	"time"

	chdomain "github.com/thulasiram/oto/internal/channels/domain"
	slackprov "github.com/thulasiram/oto/internal/channels/providers/slack"
	webhookprov "github.com/thulasiram/oto/internal/channels/providers/webhook"
	sources "github.com/thulasiram/oto/internal/sources/domain"
	"github.com/thulasiram/oto/test/harness"
)

// ⭐ THIS FILE IS THE HARNESS'S OWN ACCEPTANCE TEST, and it exists for exactly
// the reason ADR 0021 §4 gives: a fake nothing exercises is indistinguishable
// from a fake that does not work. Four httptest servers speaking four upstream
// wire formats is precisely the shape of thing that compiles, reads correctly,
// and answers `{}` to every request forever.
//
// Every assertion below goes through oto's REAL client or REAL provider. None of
// them asserts on the fake's internals.

func TestMain(m *testing.M) { harness.Main(m) }

// TestPostgresAndBuildersProduceAUsableTenant proves the container, the
// migrations, the partitions and the FK graph all line up — and that the seeded
// rows are visible through the TenantScope, which is the only way a repository
// will ever read them.
func TestPostgresAndBuildersProduceAUsableTenant(t *testing.T) {
	t.Parallel()
	h := harness.New(t)

	org := h.Org()
	user := h.User(org)
	cluster := h.Cluster(org)
	source := h.Source(org, cluster)
	group := h.Group(org, source, cluster)
	alert := h.AlertWith(org, cluster, harness.DefaultLabels())
	ac := h.Case(alert, group)

	if !org.Scope.Valid() || org.Scope.OrgID() != org.ID {
		t.Fatalf("the scope does not authorise its own org: %v", org.Scope)
	}

	// The identity keys are COMPUTED, not invented: both CHECK constraints would
	// have rejected a hand-rolled string, and a string that happened to match
	// would still not agree with the ingest path.
	var storedAlertKey, storedGroupKey string
	if err := h.Pool.QueryRow(h.Ctx,
		`SELECT a.alert_key, g.group_key
		   FROM alerts a JOIN alert_groups g ON g.org_id = a.org_id
		  WHERE a.id = $1 AND g.id = $2`, alert.ID, group.ID).
		Scan(&storedAlertKey, &storedGroupKey); err != nil {
		t.Fatalf("read back the seeded rows: %v", err)
	}
	if storedAlertKey != harness.AlertKey(org.ID, cluster.Key, alert.Labels).String() {
		t.Fatalf("alert_key %q does not match the computed identity", storedAlertKey)
	}
	if storedGroupKey != group.Key.String() {
		t.Fatalf("group_key %q != %q", storedGroupKey, group.Key)
	}

	// The case is the alert's current one, which is the projection every
	// read path trusts.
	var current string
	if err := h.Pool.QueryRow(h.Ctx,
		`SELECT current_case_id FROM alerts WHERE id = $1`, alert.ID).Scan(&current); err != nil {
		t.Fatalf("read current_case_id: %v", err)
	}
	if current != ac.ID.String() {
		t.Fatalf("current_case_id = %s, want %s", current, ac.ID)
	}

	// alert_events is partitioned with no default partition. If
	// oto_partitions_manage did not run, this insert fails — and every timeline
	// append in every harness test would fail with it. It is stamped at h.Now(),
	// not now(), because that is the instant every test writes at: see
	// TestEpochHasAPartitionEverywhere below.
	h.Exec(`INSERT INTO alert_events (id, org_id, alert_id, case_id, type, actor_kind,
	                                  summary, occurred_at, recorded_at, payload)
	        VALUES (gen_random_uuid(), $1, $2, $3, 'alert.observed', 'ingest',
	                'observed by a harness test', $4, $4, '{}'::jsonb)`,
		org.ID, alert.ID, ac.ID, h.Now())

	var users int
	if err := h.Pool.QueryRow(h.Ctx,
		`SELECT count(*) FROM users WHERE org_id = $1 AND id = $2`, org.ID, user.ID).
		Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 {
		t.Fatalf("the seeded user is not in its org")
	}
}

// TestEpochHasAPartitionEverywhere is git-bug 6547228's regression test, and it
// is a test about the CALENDAR: it passes today and it has to go on passing on
// the first of every month after today.
//
// All four of these are PARTITION BY RANGE with no default partition, and the
// partition manager builds its window around the database's `now()`. Epoch does
// not move, so without the second call in `migrateTemplate` the window walks off
// it and a row a harness test writes into one of these tables fails with a bare
// 23514. Five tests worked around that by deriving their own `now`; none of them
// needs to any more, and the way to keep it that way is here.
func TestEpochHasAPartitionEverywhere(t *testing.T) {
	t.Parallel()
	h := harness.New(t)

	for _, table := range []struct {
		parent, grain string
		// callerStamped is whether a Go caller supplies the partition key. Where it
		// does, a missing partition is a failing test; where it does not, a missing
		// partition is only a missing precaution.
		callerStamped bool
	}{
		{"alert_events", "month", true},
		{"ingest_batches", "day", true},
		{"ingest_rejections", "day", true},
		// `ui_events.at` is DEFAULT now() and every writer omits the column, so
		// nothing is ever stamped at Epoch here. The partition is kept for the day
		// one supplies `at`, and this row asserts the harness still creates it.
		{"ui_events", "hour", false},
	} {
		// to_regclass answers NULL for a partition that was never created, which is
		// exactly the state that produces the 23514.
		var name *string
		if err := h.Pool.QueryRow(h.Ctx,
			`SELECT to_regclass('public.' || oto_partition_name($1, $2, $3))::text`,
			table.parent, table.grain, harness.Epoch).Scan(&name); err != nil {
			t.Fatalf("look up %s's partition for Epoch: %v", table.parent, err)
		}
		if name == nil {
			if table.callerStamped {
				t.Fatalf("%s has no %s partition covering harness.Epoch (%s): a row stamped "+
					"at h.Now() would fail with 'no partition of relation'",
					table.parent, table.grain, harness.Epoch)
			}
			t.Fatalf("%s has no %s partition covering harness.Epoch (%s): no writer stamps "+
				"`at` today, so nothing fails yet — but epochPartitionsSQL stopped "+
				"creating it and the first writer that does will",
				table.parent, table.grain, harness.Epoch)
		}
	}
}

// TestEachTestGetsItsOwnDatabase is the isolation proof. Two tests seeding the
// same slug can only both succeed if `orgs_slug_uniq` is being enforced in two
// different databases.
func TestEachTestGetsItsOwnDatabase(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	h.OrgNamed("shared-slug-collision")

	var orgs int
	if err := h.Pool.QueryRow(h.Ctx, `SELECT count(*) FROM orgs`).Scan(&orgs); err != nil {
		t.Fatalf("count orgs: %v", err)
	}
	if orgs != 1 {
		t.Fatalf("this test's database holds %d orgs; it is not isolated", orgs)
	}
}

func TestEachTestGetsItsOwnDatabaseToo(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	h.OrgNamed("shared-slug-collision")

	var orgs int
	if err := h.Pool.QueryRow(h.Ctx, `SELECT count(*) FROM orgs`).Scan(&orgs); err != nil {
		t.Fatalf("count orgs: %v", err)
	}
	if orgs != 1 {
		t.Fatalf("this test's database holds %d orgs; it is not isolated", orgs)
	}
}

// TestAlertmanagerFakeSpeaksV2ToTheRealClient drives the fake through oto's own
// client, so a wire-format mistake in the fake fails here rather than in a
// reconciler test that will blame the reconciler.
func TestAlertmanagerFakeSpeaksV2ToTheRealClient(t *testing.T) {
	t.Parallel()

	am := harness.NewAlertmanager(t)
	clk := harness.Epoch
	am.SetVersion("0.28.1")
	am.SetAlerts(
		harness.FiringAlert(map[string]string{"alertname": "HighErrorRate"}, clk),
		harness.SuppressedAlert(map[string]string{"alertname": "Noisy"}, clk, "sil-1"),
	)
	am.SetSilences(harness.ActiveSilence("sil-1", clk, clk.Add(time.Hour),
		map[string]string{"alertname": "Noisy"}))

	client := am.Client(nil)
	ctx := t.Context()

	status, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Version != "0.28.1" {
		t.Fatalf("version = %q", status.Version)
	}
	// The route timings come out of `config.original`, parsed by the real
	// config parser — which is the only reason provenance is observable at all.
	if status.RouteTimings.GroupWait == nil || status.RouteTimings.GroupWait.String() != "30s" {
		t.Fatalf("group_wait was not read off the config: %+v", status.RouteTimings)
	}
	if status.ServerTime.IsZero() {
		t.Fatal("no Date header; the clock-skew signal is unavailable")
	}

	got, err := client.Alerts(ctx, sources.AlertFilter{})
	if err != nil {
		t.Fatalf("alerts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d alerts, want 2", len(got))
	}
	// ⭐ Suppression survives the round trip. It is the ONE fact only the
	// reconciler can see, and a fake that flattened it would make every
	// suppression test vacuous.
	if got[1].Status.State != "suppressed" || len(got[1].Status.SilencedBy) != 1 {
		t.Fatalf("suppression was lost: %+v", got[1].Status)
	}

	if _, err := client.Silences(ctx, sources.SilenceFilter{}); err != nil {
		t.Fatalf("silences: %v", err)
	}

	am.FailWith(503)
	if _, err := client.Status(ctx); err == nil {
		t.Fatal("a 503 upstream produced no error")
	}

	if len(am.Requests()) == 0 {
		t.Fatal("the fake recorded no requests")
	}
}

// TestPrometheusFakeSpeaksV1ToTheRealClient covers the v1 envelope, including
// the 200-that-is-a-refusal.
func TestPrometheusFakeSpeaksV1ToTheRealClient(t *testing.T) {
	t.Parallel()

	prom := harness.NewPrometheus(t)
	prom.SetBuildInfo(harness.PromBuildInfo{Version: "3.1.0"})
	prom.SetRuleGroups(harness.PromRuleGroup{
		Name: "errors", File: "/etc/rules.yml", Interval: 30,
		Rules: []harness.PromRule{
			harness.AlertingRule("HighErrorRate", `rate(errors[5m]) > 0.05`, 600),
			// A recording rule must be filtered out by the client, not by the fake.
			{Type: "recording", Name: "job:errors:rate5m", Query: "rate(errors[5m])"},
		},
	})

	client := prom.Client(nil)
	ctx := t.Context()

	bi, err := client.BuildInfo(ctx)
	if err != nil {
		t.Fatalf("buildinfo: %v", err)
	}
	if bi.Version != "3.1.0" {
		t.Fatalf("version = %q", bi.Version)
	}

	groups, err := client.Rules(ctx, []string{"HighErrorRate"})
	if err != nil {
		t.Fatalf("rules: %v", err)
	}
	if len(groups) != 1 || len(groups[0].Rules) != 1 {
		t.Fatalf("recording rules were not filtered: %+v", groups)
	}
	// FLOAT SECONDS. 600 is `for: 10m`.
	if groups[0].Rules[0].Duration != 600 {
		t.Fatalf("duration = %v, want 600 seconds", groups[0].Rules[0].Duration)
	}

	// A 200 carrying status="error" is a REFUSAL and must not look like success.
	prom.RefuseWith("rule_name[] is not supported")
	if _, err := client.Rules(ctx, nil); err == nil {
		t.Fatal("a refusing Prometheus produced no error")
	}
}

// TestSlackFakeDriveTheRealProvider covers the three methods oto is allowed to
// call, through the real slack-go client and the real provider.
func TestSlackFakeDriveTheRealProvider(t *testing.T) {
	t.Parallel()

	fake := harness.NewSlack(t)
	provider := fake.Provider(slackprov.Options{})
	kind, values := fake.Credential("xoxb-test-token")
	cred := chdomain.Credential{Kind: kind, Values: values}
	ctx := t.Context()

	identity, err := provider.VerifyCredential(ctx, cred)
	if err != nil {
		t.Fatalf("auth.test: %v", err)
	}
	if identity.TeamID != fake.TeamID() {
		t.Fatalf("team = %q, want %q", identity.TeamID, fake.TeamID())
	}

	channel, err := provider.Open(ctx, chdomain.ChannelConfig{
		Raw: fake.Config("C0123456789"),
	}, cred)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = channel.Close() })

	msg := chdomain.RenderedMessage{
		Fallback: "HighErrorRate is firing on checkout.",
		Payload: json.RawMessage(`{"text":"HighErrorRate is firing on checkout.",
			"attachments":[{"color":"#d5382a","fallback":"HighErrorRate is firing",
			"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"*HighErrorRate*"}}]}]}`),
	}

	res, err := channel.Deliver(ctx, chdomain.DeliverRequest{
		Message: msg, Mode: chdomain.ModePostRoot,
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	// ⭐ The ts is a STRING with a six-digit tail, and the conversation id comes
	// from the RESPONSE. Both are the durable handle.
	if res.Ref.MessageID == "" || res.Ref.ConversationID != "C0123456789" {
		t.Fatalf("bad message ref: %+v", res.Ref)
	}

	posts := fake.Posts()
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want 1", len(posts))
	}
	// The top-level text is the push notification and the only thing a screen
	// reader reads. It must survive to Slack verbatim.
	if posts[0].Text != "HighErrorRate is firing on checkout." {
		t.Fatalf("text was not delivered verbatim: %q", posts[0].Text)
	}
	if len(posts[0].Attachments) == 0 {
		t.Fatal("the rendered blocks never reached Slack")
	}

	// chat.update in place is PRIMARY (ADR 0008).
	if _, err := channel.Amend(ctx, res.Ref, msg); err != nil {
		t.Fatalf("update: %v", err)
	}
	updates := fake.Updates()
	if len(updates) != 1 || updates[0].TS != res.Ref.MessageID {
		t.Fatalf("the amend did not address the root ts: %+v", updates)
	}

	// A Slack error is HTTP 200 with ok:false, and §H.9 classifies it.
	fake.FailNext("channel_not_found")
	if _, err := channel.Deliver(ctx, chdomain.DeliverRequest{
		Message: msg, Mode: chdomain.ModePostRoot,
	}); err == nil {
		t.Fatal("channel_not_found produced no error")
	}
}

// TestWebhookFakeReceivesARealDelivery proves the outbound webhook path,
// including that the SSRF guard is what has to be opened for loopback.
func TestWebhookFakeReceivesARealDelivery(t *testing.T) {
	t.Parallel()

	fake := harness.NewWebhook(t)
	provider := fake.Provider(webhookprov.Options{})
	ctx := t.Context()

	cfg := chdomain.ChannelConfig{
		Raw: json.RawMessage(`{"url":"` + fake.URL() + `"}`),
	}
	cred := chdomain.Credential{
		Kind:   webhookprov.CredBearer,
		Values: map[string]string{"token": "s3cret"},
	}

	if err := provider.ValidateConfig(ctx, cfg.Raw); err != nil {
		t.Fatalf("validate: %v", err)
	}

	channel, err := provider.Open(ctx, cfg, cred)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = channel.Close() })

	if _, err := channel.Deliver(ctx, chdomain.DeliverRequest{
		Mode: chdomain.ModePostRoot,
		Message: chdomain.RenderedMessage{
			Fallback: "HighErrorRate is firing.",
			Payload:  json.RawMessage(`{"alert":"HighErrorRate"}`),
		},
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if fake.Count() != 1 {
		t.Fatalf("the receiver got %d deliveries, want 1", fake.Count())
	}
	got := fake.Requests()[0]
	// The sealed credential travelled on the transport, never through Config.
	if got.Header.Get("Authorization") != "Bearer s3cret" {
		t.Fatalf("the credential did not reach the wire: %q", got.Header.Get("Authorization"))
	}
	if len(got.Body) == 0 {
		t.Fatal("the rendered payload never reached the receiver")
	}

	// A 4xx is a delivery failure the caller must see, not a silent success.
	fake.RespondWith(400, `{"error":"nope"}`)
	if _, err := channel.Deliver(ctx, chdomain.DeliverRequest{
		Mode:    chdomain.ModePostRoot,
		Message: chdomain.RenderedMessage{Payload: json.RawMessage(`{}`)},
	}); err == nil {
		t.Fatal("a 400 from the receiver produced no error")
	}
}
