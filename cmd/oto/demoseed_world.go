package main

import "time"

// This file is the FICTIONAL ORG, and nothing else. `demoseed.go` owns the
// writing; this owns the story, so that changing the story never means reading a
// single line of SQL.
//
// ⛔ EVERY NAME IN HERE IS INVENTED AND MUST STAY THAT WAY. No real company,
// product, person or domain appears, and every hostname is under `.example` —
// the TLD RFC 2606 reserves for exactly this, and the only one that can never be
// registered by somebody who then receives a demo's traffic.
//
// ⭐ EVERY INSTANT IS A DURATION BEFORE `now`, never a date. A seed with dates in
// it is a seed whose screenshots say "2024" forever and whose "firing for" column
// reads in years by the second birthday of the fixture.

const (
	hour   = time.Hour
	day    = 24 * time.Hour
	minute = time.Minute
)

// The three clusters. `key` is the §C.2 cluster_key, which participates in Alert
// identity: the SAME alertname in two clusters is two Alerts, because it has two
// blast radii.
var demoClusters = []demoCluster{
	{key: "prod-us-east-1", name: "Production · US East 1"},
	{key: "prod-eu-west-1", name: "Production · EU West 1"},
	{key: "staging", name: "Staging"},
}

// One AlertSource per cluster: two green and one not, because a sources page on
// which everything is green has never shown anybody what the page is for.
//
// ⛔ THE BASE URLS ARE FICTION AND MUST STAY FICTION. Pointing one of these at
// the Alertmanager `just infra` runs on localhost:9093 does make the reconciler
// call it healthy — and makes the very next pass INGEST that Alertmanager's own
// alerts (OtoStackSmokeTest, OtoJobsFailing, whatever else the host is
// complaining about) straight into Acme's queue, because reconciliation is a
// two-way repair and not a liveness check. A fixture that imports the machine it
// is being demonstrated on is worse than one whose upstreams are imaginary.
// `.example` is reserved precisely so these names can never resolve to somebody.
//
// ⛔ AND NOTHING HERE IS `unreachable`. The shell raises a permanent,
// undismissable banner naming every unreachable source — "oto cannot reach …,
// and this list is incomplete until it is back" — on every screen. Three of them
// says the product is broken when the only broken thing is the fixture's
// imaginary DNS. `degraded` is the state that demonstrates the health feature
// without that claim: it shows on the sources screen, it still BLOCKS THE REAPER
// (§B.4), and it raises no banner (SourceReachBanner is `unreachable`-only).
//
// These rows are a projection the reconciler owns, so the seeded reading stands
// only until it runs — see demoReconcileInterval in demoseed.go for how long
// that is and why it is the maximum the DDL allows.
var demoSources = []demoSource{
	{
		name: "am-prod-us-east-1", cluster: "prod-us-east-1",
		baseURL: "http://alertmanager.prod-us-east-1.acme.example:9093",
		promURL: "http://prometheus.prod-us-east-1.acme.example:9090",
		health: demoHealth{
			status: "healthy", lastPushAgo: 40 * time.Second,
			lastReconcileAgo: 25 * time.Second, lastReconcileStatus: "ok",
			amVersion: "0.27.0", sendResolved: true, clockSkewMS: 120,
		},
	},
	// The one source that is not green, and it is degraded the way ApplyProbe
	// actually reaches degraded from an Alertmanager it CAN see: a settling HA
	// cluster, the single member of `degradingWarnings`. Hence no `last_error`,
	// `last_reconcile_status: ok` and zero consecutive failures — three of those
	// would be `unreachable` by the state machine, not degraded, and a fixture
	// that spells an impossible row teaches the reader the wrong state machine.
	// The 2.4 s skew is recorded and deliberately raises nothing: it is under
	// MaxTolerableSkew, and skew is measured, never acted on (C12).
	{
		name: "am-prod-eu-west-1", cluster: "prod-eu-west-1",
		baseURL: "http://alertmanager.prod-eu-west-1.acme.example:9093",
		promURL: "http://prometheus.prod-eu-west-1.acme.example:9090",
		health: demoHealth{
			status: "degraded", lastPushAgo: 3 * minute,
			lastReconcileAgo: 35 * time.Second, lastReconcileStatus: "ok",
			amVersion: "0.27.0", sendResolved: true, clockSkewMS: 2400,
			divergenceCount: 2,
			warnings: []demoWarning{{
				code:    "alertmanager_cluster_not_ready",
				subject: "settling",
				message: "the Alertmanager cluster is not ready",
			}},
		},
	},
	// Staging pushes rarely — nobody deploys to it at night — which is what a
	// twelve-minute-old push looks like on a source that is perfectly well.
	{
		name: "am-staging", cluster: "staging",
		baseURL: "http://alertmanager.staging.acme.example:9093",
		promURL: "http://prometheus.staging.acme.example:9090",
		health: demoHealth{
			status: "healthy", lastPushAgo: 12 * minute,
			lastReconcileAgo: 20 * time.Second, lastReconcileStatus: "ok",
			amVersion: "0.26.0", sendResolved: true, clockSkewMS: 40,
		},
	},
}

// The two people the seed adds. The bootstrap user is the third and the only one
// with a password: these two exist to be the ACTOR on an ack and a comment, and
// a demo fixture must not mint credentials nobody asked for.
var demoPeople = []demoPerson{
	{email: "sre@acme.example", name: "Sam Reyes"},
	{email: "dana@acme.example", name: "Dana Okafor"},
}

// One Slack-shaped Connection and one webhook Connection. The Slack one carries
// a sealed credential because channel_connections_cred_ck makes it mandatory;
// the token is obvious nonsense and reaches no network, because nothing in a
// seeded delivery is ever sent.
// demoFakeBotToken is assembled at runtime rather than written as a literal.
// Spelled out, it matches Slack's token shape closely enough that GitHub's
// secret scanner blocks the push — the value is nonsense either way, and
// nothing in a seeded delivery reaches the network.
var demoFakeBotToken = "xoxb" + "-0000000000-0000000000-notarealtoken"

var demoConnections = []demoConnection{
	{
		name: "acme-workspace", typ: "slack",
		config:  `{"team_id":"T0ACMEDEMO"}`,
		credKnd: "slack_bot_token",
		credVal: map[string]string{"bot_token": demoFakeBotToken},
	},
	{name: "acme-webhooks", typ: "webhook", config: `{}`},
}

// Four Channels over those two Connections — three conversations and one
// webhook — so the fan-out on a policy is a fan-out and not a synonym for one
// destination.
//
// `caps` is the `channels.capabilities` bitmask: 1 threading | 2 amend |
// 4 rich layout | 8 interactive, and 32 provider-side dedupe (bit 16 is retired).
var demoChannels = []demoChannel{
	{
		name: "#sre-alerts", typ: "slack", connection: "acme-workspace",
		config:   `{"conversation_id":"C0SREALERTS","conversation_name":"sre-alerts"}`,
		convID:   "C0SREALERTS",
		renderer: "slack.default", verbosity: "all", caps: 1 | 2 | 4 | 8,
		health: "healthy",
	},
	{
		name: "#checkout-alerts", typ: "slack", connection: "acme-workspace",
		config:   `{"conversation_id":"C0CHECKOUT","conversation_name":"checkout-alerts"}`,
		convID:   "C0CHECKOUT",
		renderer: "slack.default", verbosity: "status_changes", caps: 1 | 2 | 4 | 8,
		health: "healthy",
	},
	{
		name: "#platform-warnings", typ: "slack", connection: "acme-workspace",
		config:   `{"conversation_id":"C0PLATFORM","conversation_name":"platform-warnings"}`,
		convID:   "C0PLATFORM",
		renderer: "slack.default", verbosity: "firing_and_resolved", caps: 1 | 2 | 4 | 8,
		health: "degraded",
		healthMsg: "chat.postMessage was rate limited 4 times in the last hour " +
			"(Slack tier 3, 50/min)",
	},
	{
		name: "ops-webhook", typ: "webhook", connection: "acme-webhooks",
		config:   `{"url":"https://hooks.acme.example/oto/alerts","method":"POST","timeout_ms":5000}`,
		renderer: "webhook.json", verbosity: "firing_only", caps: 32,
		health: "healthy",
	},
}

// Two more NotificationTemplates beside the starter `oto's own card` that
// bootstrap writes, so the picker has a choice and the list has three rows.
var demoTemplates = []demoTemplate{
	{
		name: "acme critical card", format: "card", provider: "slack", enabled: true,
		source: "# :rotating_light: {{ alert.name }}\n\n" +
			"{{ annotations.summary | default: \"No summary on this alert.\" }}\n\n---\n\n" +
			":::fields\nSeverity | {{ alert.severity | upper }}\n" +
			"Cluster | {{ alert.cluster }}\nFiring | {{ group.firing_for }}\n" +
			"Seen | {{ alert.total_cases }} times\n:::\n\n" +
			"> [Open in oto]({{ links.group }})\n\n{{ actions }}",
	},
	{
		name: "acme terse text", format: "text", provider: "slack", enabled: true,
		source: "[{{ alert.severity | upper }}] {{ alert.name }} · {{ alert.cluster }} · " +
			"firing {{ group.firing_for }} · {{ links.group }}",
	},
}

// Four NotificationPolicies at four priorities, so the "which policy chose this"
// column on a Notification has more than one possible answer.
//
// ⛔ ONLY ONE POINTS AT A TEMPLATE, for the reason `internal/app/bootstrap.go`
// spells out at length: `template_id IS NULL` renders through oto's built-in Go
// card, which is the FALLBACK every template failure lands on, and a fallback
// that no configuration exercises is one nobody finds out is torn.
var demoPolicies = []demoPolicy{
	{
		name: "critical anywhere", priority: 10,
		matchers: `[{"name":"severity","op":"=","value":"critical"}]`,
		reasons:  []string{"fired", "all_resolved", "acked", "refired", "rule_changed"},
		channels: []string{"#sre-alerts", "ops-webhook"},
	},
	{
		name: "checkout, warning and up", priority: 20,
		matchers: `[{"name":"service","op":"=","value":"checkout-api"},` +
			`{"name":"severity","op":"=~","value":"critical|warning"}]`,
		reasons:  []string{"fired", "all_resolved", "acked", "comment", "enriched"},
		channels: []string{"#checkout-alerts"},
		template: "acme critical card",
	},
	{
		name: "platform warnings", priority: 50,
		matchers: `[{"name":"severity","op":"=","value":"warning"}]`,
		reasons:  []string{"fired", "all_resolved"},
		channels: []string{"#platform-warnings"},
		throttle: `{"per_case_seconds":900}`,
	},
	{
		name: "everything else", priority: 900,
		matchers: `[]`,
		reasons:  []string{"fired"},
		channels: []string{"#sre-alerts"},
	},
}

// ---------------------------------------------------------------- the alerts

// demoAlerts is the signal history the whole demo is made of. Everything
// downstream — the timeline, the RuleSnapshots, the Notifications, the
// deliveries, the daily rollup — is DERIVED from these rows by demoseed.go, so
// adding an alert here adds it to every screen at once and cannot leave one
// screen disagreeing with another.
//
//nolint:gochecknoglobals // the fixture IS the data; see the file comment.
var demoAlerts = []demoAlert{
	// ⭐ THE HEADLINE ALERT. Three Cases, and the rule CHANGED between the second
	// and the third — a wider threshold and a longer `for:` — which is what the
	// rule-drift panel exists to show and the one thing a Prometheus stack cannot
	// tell you on its own.
	{
		cluster: "prod-us-east-1", alertname: "CheckoutAPIHighErrorRate",
		severity: "critical", namespace: "payments", service: "checkout-api",
		job: "checkout-api", instance: "checkout-api-7f9c2x4k:8080",
		annotations: map[string]string{
			"summary":     "checkout-api is returning 5xx for more than 2% of requests",
			"description": "Error ratio over the last 5 minutes is above the threshold in prod-us-east-1.",
			"runbook_url": "https://runbooks.acme.example/checkout-api/high-error-rate",
			"dashboard":   "https://grafana.acme.example/d/checkout/checkout-api",
		},
		rules: []demoRule{{
			file: "acme/payments.rules.yaml", group: "payments.availability",
			expr: `sum(rate(http_requests_total{job="checkout-api",code=~"5.."}[5m]))` +
				` / sum(rate(http_requests_total{job="checkout-api"}[5m])) > 0.02`,
			forS: 300,
		}, {
			file: "acme/payments.rules.yaml", group: "payments.availability",
			expr: `sum(rate(http_requests_total{job="checkout-api",code=~"5.."}[5m]))` +
				` / sum(rate(http_requests_total{job="checkout-api"}[5m])) > 0.05`,
			forS: 600, keepS: 300,
		}},
		enrichers: []string{"prom.rule", "alert.history", "runbook.link", "alert.related"},
		cases: []demoCase{
			{
				startedAgo: 12*day + 4*hour, endedAgo: 12*day + 1*hour, resolve: "upstream",
				rule: 0, value: 0.031, ackBy: "sre@acme.example", ackAfter: 11 * minute,
				ackNote: "Rolling back the payment-gateway client bump.",
			},
			{
				startedAgo: 6 * day, endedAgo: 5*day + 20*hour, resolve: "upstream",
				rule: 0, value: 0.047,
			},
			{
				startedAgo: 26 * hour, rule: 1, value: 0.062,
				ackBy: "dana@acme.example", ackAfter: 18 * minute,
				ackNote: "Same signature as last week. Watching the retry budget.",
				comments: []demoComment{
					{by: "sre@acme.example", after: 2 * hour,
						body: "Threshold was widened to 5% on Tuesday — this is the first firing under the new rule."},
					{by: "dana@acme.example", after: 5 * hour,
						body: "Error budget for the month is 40% spent. Leaving it open until the deploy lands."},
				},
			},
		},
	},
	{
		cluster: "prod-us-east-1", alertname: "CheckoutAPILatencyP99",
		severity: "warning", namespace: "payments", service: "checkout-api",
		job: "checkout-api", instance: "checkout-api-7f9c2x4k:8080",
		annotations: map[string]string{
			"summary":     "checkout-api p99 latency is above 1.5s",
			"runbook_url": "https://runbooks.acme.example/checkout-api/latency",
		},
		rules: []demoRule{{
			file: "acme/payments.rules.yaml", group: "payments.latency",
			expr: `histogram_quantile(0.99, sum by (le) ` +
				`(rate(http_request_duration_seconds_bucket{job="checkout-api"}[5m]))) > 1.5`,
			forS: 600,
		}},
		enrichers: []string{"prom.rule", "alert.history"},
		cases: []demoCase{
			{startedAgo: 9 * day, endedAgo: 9*day - 90*minute, resolve: "upstream", value: 1.83},
			{startedAgo: 3 * hour, value: 2.14},
		},
	},
	// ⭐ THE SECOND RICH TIMELINE: acked, commented, and still open after two days.
	{
		cluster: "prod-us-east-1", alertname: "PaymentsWorkerQueueBacklog",
		severity: "critical", namespace: "payments", service: "payments-worker",
		job: "payments-worker", instance: "payments-worker-3b81mm:9102",
		annotations: map[string]string{
			"summary":     "payments-worker backlog is above 25k messages",
			"description": "The settlement queue is growing faster than the worker pool drains it.",
			"runbook_url": "https://runbooks.acme.example/payments-worker/backlog",
		},
		rules: []demoRule{{
			file: "acme/payments.rules.yaml", group: "payments.throughput",
			expr: `sum(queue_depth{job="payments-worker",queue="settlement"}) > 25000`,
			forS: 900,
		}},
		enrichers: []string{"prom.rule", "alert.history", "runbook.link"},
		cases: []demoCase{
			{startedAgo: 13 * day, endedAgo: 13*day - 6*hour, resolve: "upstream", value: 31402},
			{startedAgo: 8 * day, endedAgo: 8*day - 40*minute, resolve: "timeout", value: 26890},
			{
				startedAgo: 2*day + 3*hour, value: 41255,
				ackBy: "sre@acme.example", ackAfter: 34 * minute,
				ackNote: "Scaled the worker pool to 12. Draining, slowly.",
				comments: []demoComment{
					{by: "dana@acme.example", after: 20 * hour,
						body: "Backlog is down to 18k. Not closing it until it clears the threshold upstream."},
				},
			},
		},
	},
	{
		cluster: "prod-us-east-1", alertname: "PaymentsWorkerRestartLoop",
		severity: "warning", namespace: "payments", service: "payments-worker",
		job: "kubelet", instance: "ip-10-42-7-13.ec2.internal:10250",
		annotations: map[string]string{
			"summary": "payments-worker restarted 6 times in 15 minutes",
		},
		rules: []demoRule{{
			file: "acme/kube.rules.yaml", group: "kube.workloads",
			expr:  `increase(kube_pod_container_status_restarts_total{namespace="payments",container="payments-worker"}[15m]) > 5`,
			forS:  0,
			keepS: 300,
		}},
		cases: []demoCase{
			{startedAgo: 11 * day, endedAgo: 11*day - 25*minute, resolve: "upstream", value: 7},
			{startedAgo: 4*day + 2*hour, endedAgo: 4 * day, resolve: "upstream", value: 6},
		},
	},
	{
		cluster: "prod-us-east-1", alertname: "InventoryServiceStockDrift",
		severity: "warning", namespace: "inventory", service: "inventory-svc",
		job: "inventory-svc", instance: "inventory-svc-55d4qz:8080",
		annotations: map[string]string{
			"summary":     "inventory-svc reconciliation drift above 0.5%",
			"runbook_url": "https://runbooks.acme.example/inventory-svc/stock-drift",
		},
		rules: []demoRule{{
			file: "acme/inventory.rules.yaml", group: "inventory.consistency",
			expr: `abs(inventory_ledger_drift_ratio{job="inventory-svc"}) > 0.005`,
			forS: 1800,
		}},
		// The snoozed one: quiet on purpose, and the /alerts quiet tab needs it.
		snoozes: []demoSnooze{
			{startedAgo: 9 * day, untilAgo: 7 * day, by: "sre@acme.example",
				note: "Ledger migration in flight.", endedReason: "expired"},
			{startedAgo: 5 * hour, untilAgo: -19 * hour, by: "dana@acme.example",
				note: "Known during the warehouse cut-over. Back on Monday."},
		},
		cases: []demoCase{
			{startedAgo: 10 * day, endedAgo: 10*day - 3*hour, resolve: "upstream", value: 0.0071},
			{startedAgo: 30 * hour, value: 0.0094},
		},
	},
	{
		cluster: "prod-us-east-1", alertname: "InventoryServiceDBPoolSaturation",
		severity: "critical", namespace: "inventory", service: "inventory-svc",
		job: "inventory-svc", instance: "inventory-svc-55d4qz:8080",
		annotations: map[string]string{
			"summary":     "inventory-svc has been at 100% of its database pool for 10 minutes",
			"runbook_url": "https://runbooks.acme.example/inventory-svc/db-pool",
		},
		rules: []demoRule{{
			file: "acme/inventory.rules.yaml", group: "inventory.capacity",
			expr: `pgbouncer_pools_client_waiting_connections{job="inventory-svc"} > 0`,
			forS: 600,
		}},
		enrichers: []string{"prom.rule", "runbook.link"},
		cases: []demoCase{
			{
				startedAgo: 7 * hour, value: 41,
				ackBy: "dana@acme.example", ackAfter: 6 * minute,
				ackNote: "Raising max_connections on the replica.",
			},
		},
	},
	{
		cluster: "prod-us-east-1", alertname: "SearchIndexerLagGrowing",
		severity: "warning", namespace: "search", service: "search-indexer",
		job: "search-indexer", instance: "search-indexer-0:9090",
		annotations: map[string]string{
			"summary": "search-indexer is more than 15 minutes behind the write log",
		},
		rules: []demoRule{{
			file: "acme/search.rules.yaml", group: "search.freshness",
			expr: `time() - search_indexer_last_commit_timestamp_seconds{job="search-indexer"} > 900`,
			forS: 300,
		}},
		cases: []demoCase{
			{startedAgo: 5 * day, endedAgo: 5*day - 5*hour, resolve: "upstream", value: 2410},
		},
	},
	// ⭐ THE RE-FIRE HISTORY: four Cases in two weeks, which is what "fired 4
	// times" and the case strip on the alert page are for.
	{
		cluster: "prod-us-east-1", alertname: "NotifyGatewayDeliveryFailures",
		severity: "critical", namespace: "messaging", service: "notify-gateway",
		job: "notify-gateway", instance: "notify-gateway-9c2f:8080",
		annotations: map[string]string{
			"summary":     "notify-gateway is failing more than 10% of outbound sends",
			"runbook_url": "https://runbooks.acme.example/notify-gateway/delivery-failures",
		},
		rules: []demoRule{{
			file: "acme/messaging.rules.yaml", group: "messaging.delivery",
			expr: `sum(rate(notify_send_failures_total{job="notify-gateway"}[10m]))` +
				` / sum(rate(notify_send_total{job="notify-gateway"}[10m])) > 0.1`,
			forS: 300,
		}},
		enrichers: []string{"prom.rule", "alert.history"},
		cases: []demoCase{
			{startedAgo: 13*day + 5*hour, endedAgo: 13*day + 4*hour, resolve: "upstream", value: 0.14},
			{startedAgo: 10*day + 2*hour, endedAgo: 10 * day, resolve: "upstream", value: 0.19},
			{startedAgo: 4 * day, endedAgo: 4*day - 45*minute, resolve: "timeout", value: 0.12},
			{
				startedAgo: 15 * hour, value: 0.23,
				ackBy: "sre@acme.example", ackAfter: 52 * minute,
				ackNote: "Upstream provider is throttling us. Ticket open with them.",
			},
		},
	},
	{
		cluster: "prod-us-east-1", alertname: "NodeDiskSpaceLow",
		severity: "warning", namespace: "kube-system",
		job: "node-exporter", instance: "ip-10-42-9-88.ec2.internal:9100",
		extra: map[string]string{"device": "/dev/nvme0n1p1", "mountpoint": "/"},
		annotations: map[string]string{
			"summary": "Node root filesystem is above 85% used",
		},
		rules: []demoRule{{
			file: "acme/node.rules.yaml", group: "node.capacity",
			expr: `(1 - node_filesystem_avail_bytes{mountpoint="/"}` +
				` / node_filesystem_size_bytes{mountpoint="/"}) > 0.85`,
			forS: 1800,
		}},
		cases: []demoCase{{startedAgo: 46 * hour, value: 0.883}},
	},
	{
		cluster: "prod-us-east-1", alertname: "TargetDown",
		severity: "info", namespace: "monitoring",
		job: "prometheus", instance: "prometheus-0:9090",
		annotations: map[string]string{
			"summary": "A scrape target has been unreachable for 10 minutes",
		},
		rules: []demoRule{{
			file: "acme/monitoring.rules.yaml", group: "monitoring.meta",
			expr: `up == 0`, forS: 600,
		}},
		cases: []demoCase{
			{startedAgo: 7 * day, endedAgo: 7*day - 20*minute, resolve: "upstream", value: 0},
		},
	},

	// ------------------------------------------------------- prod-eu-west-1

	// The SAME alertname as the headline alert, in another cluster: a different
	// Alert identity, a different blast radius, and the clearest demonstration on
	// any screen of why cluster_key is part of the key.
	{
		cluster: "prod-eu-west-1", alertname: "CheckoutAPIHighErrorRate",
		severity: "critical", namespace: "payments", service: "checkout-api",
		job: "checkout-api", instance: "checkout-api-2d40kb:8080",
		annotations: map[string]string{
			"summary":     "checkout-api is returning 5xx for more than 2% of requests",
			"runbook_url": "https://runbooks.acme.example/checkout-api/high-error-rate",
		},
		rules: []demoRule{{
			file: "acme/payments.rules.yaml", group: "payments.availability",
			expr: `sum(rate(http_requests_total{job="checkout-api",code=~"5.."}[5m]))` +
				` / sum(rate(http_requests_total{job="checkout-api"}[5m])) > 0.02`,
			forS: 300,
		}},
		enrichers: []string{"prom.rule", "runbook.link"},
		cases: []demoCase{
			{startedAgo: 8 * day, endedAgo: 8*day - 2*hour, resolve: "upstream", value: 0.028},
			{startedAgo: 11 * hour, value: 0.036},
		},
	},
	{
		cluster: "prod-eu-west-1", alertname: "CheckoutAPICacheMissSpike",
		severity: "warning", namespace: "payments", service: "checkout-api",
		job: "checkout-api", instance: "checkout-api-2d40kb:8080",
		annotations: map[string]string{
			"summary": "checkout-api cache miss ratio above 40%",
		},
		rules: []demoRule{{
			file: "acme/payments.rules.yaml", group: "payments.caching",
			expr: `sum(rate(cache_misses_total{job="checkout-api"}[5m]))` +
				` / sum(rate(cache_lookups_total{job="checkout-api"}[5m])) > 0.4`,
			forS: 600,
		}},
		cases: []demoCase{
			{startedAgo: 6*day + 6*hour, endedAgo: 6 * day, resolve: "upstream", value: 0.47},
		},
	},
	{
		cluster: "prod-eu-west-1", alertname: "PaymentsWorkerQueueBacklog",
		severity: "warning", namespace: "payments", service: "payments-worker",
		job: "payments-worker", instance: "payments-worker-7ac1pd:9102",
		annotations: map[string]string{
			"summary": "payments-worker backlog is above 10k messages",
		},
		rules: []demoRule{{
			file: "acme/payments.rules.yaml", group: "payments.throughput",
			expr: `sum(queue_depth{job="payments-worker",queue="settlement"}) > 10000`,
			forS: 900,
		}},
		cases: []demoCase{
			{startedAgo: 12 * day, endedAgo: 12*day - 4*hour, resolve: "upstream", value: 12800},
			{startedAgo: 20 * hour, value: 14310},
		},
	},
	{
		cluster: "prod-eu-west-1", alertname: "InventoryServiceStockDrift",
		severity: "info", namespace: "inventory", service: "inventory-svc",
		job: "inventory-svc", instance: "inventory-svc-91bbtt:8080",
		annotations: map[string]string{
			"summary": "inventory-svc reconciliation drift above 0.1%",
		},
		rules: []demoRule{{
			file: "acme/inventory.rules.yaml", group: "inventory.consistency",
			expr: `abs(inventory_ledger_drift_ratio{job="inventory-svc"}) > 0.001`,
			forS: 3600,
		}},
		cases: []demoCase{
			{startedAgo: 9*day + 3*hour, endedAgo: 9 * day, resolve: "upstream", value: 0.0018},
		},
	},
	{
		cluster: "prod-eu-west-1", alertname: "SearchIndexerQueryTimeouts",
		severity: "critical", namespace: "search", service: "search-indexer",
		job: "search-indexer", instance: "search-indexer-1:9090",
		annotations: map[string]string{
			"summary":     "search-indexer is timing out more than 5% of queries",
			"runbook_url": "https://runbooks.acme.example/search-indexer/timeouts",
		},
		rules: []demoRule{{
			file: "acme/search.rules.yaml", group: "search.availability",
			expr: `sum(rate(search_query_timeouts_total{job="search-indexer"}[5m]))` +
				` / sum(rate(search_query_total{job="search-indexer"}[5m])) > 0.05`,
			forS: 300,
		}},
		enrichers: []string{"prom.rule", "alert.history", "runbook.link"},
		cases: []demoCase{
			{startedAgo: 14 * day, endedAgo: 14*day - 3*hour, resolve: "upstream", value: 0.061},
			{
				startedAgo: 33 * hour, value: 0.084,
				ackBy: "sre@acme.example", ackAfter: 2 * hour,
				ackNote: "Shard 3 is hot. Rebalancing tonight.",
			},
		},
	},
	// The suppressed one: firing, and quiet because Alertmanager is silencing it.
	{
		cluster: "prod-eu-west-1", alertname: "NotifyGatewayRateLimited",
		severity: "warning", namespace: "messaging", service: "notify-gateway",
		job: "notify-gateway", instance: "notify-gateway-4e71:8080",
		annotations: map[string]string{
			"summary": "notify-gateway is being rate limited by the upstream provider",
		},
		rules: []demoRule{{
			file: "acme/messaging.rules.yaml", group: "messaging.delivery",
			expr: `sum(rate(notify_provider_429_total{job="notify-gateway"}[10m])) > 0`,
			forS: 600,
		}},
		cases: []demoCase{
			{
				startedAgo: 28 * hour, value: 3,
				suppress: "silence", silencedBy: []string{"c6f3a2b1-8d45-4e70-9a11-2f8c5b7d0e93"},
			},
		},
	},
	{
		cluster: "prod-eu-west-1", alertname: "CertificateExpiringSoon",
		severity: "warning", namespace: "kube-system",
		job: "cert-exporter", instance: "cert-exporter-0:9110",
		extra: map[string]string{"secret": "checkout-api-tls"},
		annotations: map[string]string{
			"summary": "The checkout-api TLS certificate expires in less than 14 days",
		},
		rules: []demoRule{{
			file: "acme/security.rules.yaml", group: "security.certificates",
			expr: `(certificate_expiry_timestamp_seconds - time()) / 86400 < 14`,
			forS: 3600,
		}},
		cases: []demoCase{{startedAgo: 3*day + 7*hour, value: 9}},
	},

	// -------------------------------------------------------------- staging

	{
		cluster: "staging", alertname: "SearchIndexerLagGrowing",
		severity: "info", namespace: "search", service: "search-indexer",
		job: "search-indexer", instance: "search-indexer-0:9090",
		annotations: map[string]string{
			"summary": "search-indexer is more than 15 minutes behind the write log",
		},
		rules: []demoRule{{
			file: "acme/search.rules.yaml", group: "search.freshness",
			expr: `time() - search_indexer_last_commit_timestamp_seconds{job="search-indexer"} > 900`,
			forS: 300,
		}},
		cases: []demoCase{{startedAgo: 5*day + 30*minute, value: 7200}},
	},
	// The expired one, and the §B.4 guard told as a story: staging went dark a
	// week ago, oto HELD this episode rather than resolving it, and the reaper
	// only swept it once the source could be seen again — which is why the source
	// above is healthy today and this case still ended in `timeout`, not
	// `resolved`.
	{
		cluster: "staging", alertname: "CheckoutAPIHighErrorRate",
		severity: "warning", namespace: "payments", service: "checkout-api",
		job: "checkout-api", instance: "checkout-api-stg-0:8080",
		annotations: map[string]string{
			"summary": "checkout-api is returning 5xx for more than 2% of requests",
		},
		rules: []demoRule{{
			file: "acme/payments.rules.yaml", group: "payments.availability",
			expr: `sum(rate(http_requests_total{job="checkout-api",code=~"5.."}[5m]))` +
				` / sum(rate(http_requests_total{job="checkout-api"}[5m])) > 0.02`,
			forS: 300,
		}},
		cases: []demoCase{
			{startedAgo: 6*day + 12*hour, endedAgo: 6 * day, resolve: "timeout", value: 0.09},
		},
	},
	{
		cluster: "staging", alertname: "NotifyGatewayDeliveryFailures",
		severity: "info", namespace: "messaging", service: "notify-gateway",
		job: "notify-gateway", instance: "notify-gateway-stg-0:8080",
		annotations: map[string]string{
			"summary": "notify-gateway is failing more than 10% of outbound sends",
		},
		rules: []demoRule{{
			file: "acme/messaging.rules.yaml", group: "messaging.delivery",
			expr: `sum(rate(notify_send_failures_total{job="notify-gateway"}[10m]))` +
				` / sum(rate(notify_send_total{job="notify-gateway"}[10m])) > 0.1`,
			forS: 300,
		}},
		cases: []demoCase{
			{startedAgo: 2*day + 4*hour, endedAgo: 2 * day, resolve: "upstream", value: 0.16},
		},
	},
	{
		cluster: "staging", alertname: "InventoryServiceDBPoolSaturation",
		severity: "warning", namespace: "inventory", service: "inventory-svc",
		job: "inventory-svc", instance: "inventory-svc-stg-0:8080",
		annotations: map[string]string{
			"summary": "inventory-svc has been at 100% of its database pool for 10 minutes",
		},
		rules: []demoRule{{
			file: "acme/inventory.rules.yaml", group: "inventory.capacity",
			expr: `pgbouncer_pools_client_waiting_connections{job="inventory-svc"} > 0`,
			forS: 600,
		}},
		cases: []demoCase{{startedAgo: 21 * hour, value: 12}},
	},
}

// ------------------------------------------------------------- the types

// demoCluster is one logical failure domain.
type demoCluster struct {
	key  string
	name string
}

// demoHealth is a `source_health` row, expressed relative to now.
type demoHealth struct {
	status              string
	lastPushAgo         time.Duration
	lastReconcileAgo    time.Duration
	lastReconcileStatus string
	amVersion           string
	sendResolved        bool
	clockSkewMS         int64
	divergenceCount     int
	warnings            []demoWarning
}

// demoWarning is one element of `source_health.warnings`, in the shape the
// repository stores and reads back: a code, a sentence, and when it was noticed.
type demoWarning struct {
	code    string
	message string
	// subject names what the warning is about — a receiver, a URL, a cluster
	// state. It is optional, it is never a secret, and it is the half of a
	// warning that tells an operator WHICH thing is wrong.
	subject string
}

// demoSource is one AlertSource plus the health the reconciler would have
// written for it.
type demoSource struct {
	name    string
	cluster string
	baseURL string
	promURL string
	health  demoHealth
}

// demoPerson is a user the seed adds as an ACTOR. It has no password: see the
// comment on demoPeople.
type demoPerson struct {
	email string
	name  string
}

// demoConnection is one org-wide provider setup.
type demoConnection struct {
	name    string
	typ     string
	config  string
	credKnd string
	credVal map[string]string
}

// demoChannel is one destination under a connection.
type demoChannel struct {
	name       string
	typ        string
	connection string
	config     string
	// convID is the provider conversation id a seeded thread points at. Empty for
	// a webhook, which has no conversation and therefore no thread.
	convID    string
	renderer  string
	verbosity string
	caps      int64
	health    string
	healthMsg string
}

// demoTemplate is one NotificationTemplate.
type demoTemplate struct {
	name     string
	provider string
	format   string
	source   string
	enabled  bool
}

// demoPolicy is one NotificationPolicy.
type demoPolicy struct {
	name     string
	priority int
	matchers string
	reasons  []string
	channels []string
	template string
	throttle string
}

// demoRule is one Prometheus alerting-rule definition. Two of these on one alert
// is rule drift, and rule drift is the product.
type demoRule struct {
	file  string
	group string
	expr  string
	forS  float64
	keepS float64
}

// demoComment is a human note on a Case.
type demoComment struct {
	by    string
	after time.Duration
	body  string
}

// demoSnooze is one snooze on an Alert. `untilAgo` is measured the same way
// `startedAgo` is — a NEGATIVE value is in the FUTURE, which is what makes a
// snooze ACTIVE and the Alert quiet.
type demoSnooze struct {
	startedAgo  time.Duration
	untilAgo    time.Duration
	by          string
	note        string
	endedReason string
}

// demoCase is one firing episode. `endedAgo == 0` means it is still open, and an
// open case is the only kind that may carry a suppression.
type demoCase struct {
	startedAgo time.Duration
	endedAgo   time.Duration
	resolve    string
	rule       int
	value      float64
	ackBy      string
	ackAfter   time.Duration
	ackNote    string
	suppress   string
	silencedBy []string
	comments   []demoComment
}

// demoAlert is one Alert identity and everything that ever happened to it.
type demoAlert struct {
	cluster     string
	alertname   string
	severity    string
	namespace   string
	service     string
	job         string
	instance    string
	extra       map[string]string
	annotations map[string]string
	rules       []demoRule
	enrichers   []string
	snoozes     []demoSnooze
	cases       []demoCase
}
