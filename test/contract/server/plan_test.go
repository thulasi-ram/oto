package server

import (
	"net/http"
	"time"
)

/*
plan is the probe table: every route the contract declares, in the order the
gate drives them.

ORDER IS LOAD-BEARING. `createChannel` is what gives `getChannel` an id;
`createSource` is what gives the ingest webhook a token; the DELETEs come last
because a deleted row cannot be read. The table is therefore grouped by
lifecycle rather than alphabetically, and the groups are the sections below.

`{{name}}` is a fixture reference, expanded from the table `world.ids` at request
time — in the URL, in the headers and in the JSON body. It is deliberately NOT
the contract's own `{name}` syntax: `tmpl` carries the contract's spelling
verbatim, because that is the key the operation is resolved by, and a table that
rewrote it could drive a route the contract does not declare and never notice.
*/
func plan() []probe {
	// A `startsAt` the ingest bounds accept: the per-alert window is
	// `now - 365d … now + 24h` (B-bounds), evaluated against the SERVER's clock.
	firing := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)

	return []probe{
		/* ---------------------------------------------------------------- ops */
		// The three unversioned endpoints and the version stamp. They need no
		// credential and no fixtures, so they run first: if `/readyz` is not 200
		// the database is not up and every later failure is noise.
		{method: http.MethodGet, tmpl: "/healthz", auth: authNone, want: http.StatusOK},
		{method: http.MethodGet, tmpl: "/readyz", auth: authNone, want: http.StatusOK},
		{method: http.MethodGet, tmpl: "/metrics", auth: authNone, want: http.StatusOK},
		{method: http.MethodGet, tmpl: "/openapi.json", auth: authNone, want: http.StatusOK},
		{method: http.MethodGet, tmpl: "/api/v1/version", want: http.StatusOK},

		/* ----------------------------------------------------------- identity */
		{
			method: http.MethodPost, tmpl: "/api/v1/auth/login", auth: authNone,
			body:        map[string]any{"email": "ops@acme.example", "password": bootstrapPassword},
			want:        http.StatusOK,
			keepsCookie: true,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/auth/login", auth: authNone,
			body: map[string]any{"email": "ops@acme.example", "password": "not-the-password"},
			want: http.StatusUnauthorized,
			why:  "a wrong password must be an unspecific 401, and its BODY is what a client reads",
		},
		{method: http.MethodGet, tmpl: "/api/v1/me", want: http.StatusOK},
		{method: http.MethodGet, tmpl: "/api/v1/me", auth: authNone, want: http.StatusUnauthorized},
		{method: http.MethodGet, tmpl: "/api/v1/org/settings", want: http.StatusOK},
		// ⛔ SESSION, NOT TOKEN, for the four below. Settings and token management
		// are the identity router's session-only group: a PAT may not mint or
		// revoke another PAT, which is the only thing standing between a leaked
		// token and a permanent one. Presenting the PAT here answers 401, and that
		// 401 is a boundary this gate would otherwise never notice.
		{
			method: http.MethodPatch, tmpl: "/api/v1/org/settings", auth: authSession,
			body: map[string]any{"resolve_grace_s": 1800},
			want: http.StatusOK,
		},
		{method: http.MethodGet, tmpl: "/api/v1/api-tokens", auth: authSession, want: http.StatusOK},
		{
			method: http.MethodPost, tmpl: "/api/v1/api-tokens", auth: authSession,
			body:    map[string]any{"name": "gate-g2-minted"},
			want:    http.StatusCreated,
			capture: map[string][]string{"token": {"data", "token", "id"}},
		},
		// The self-service Slack link (git-bug a556a5c). The list answers 2xx, empty: nobody in
		// this world has pressed a Remedy button, so no code exists to link with. The three writes
		// are session-only and are driven to their typed refusals.
		{method: http.MethodGet, tmpl: "/api/v1/me/slack-identities", want: http.StatusOK},
		{
			method: http.MethodPost, tmpl: "/api/v1/me/slack-identities/preview", auth: authSession,
			body: map[string]any{"code": "ABCDE-FGHJK"},
			want: http.StatusUnprocessableEntity,
			why:  "a code exists only once a Slack member has pressed a Remedy button; this one names none",
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/me/slack-identities", auth: authSession,
			body: map[string]any{"code": "ABCDE-FGHJK"},
			want: http.StatusUnprocessableEntity,
			why:  "the same code, which names no live code, so nothing is linked",
		},
		{
			method: http.MethodDelete, tmpl: "/api/v1/me/slack-identities/{id}",
			url: "/api/v1/me/slack-identities/019fe2a1-5d1e-7c00-8000-00000000a556", auth: authSession,
			want: http.StatusNotFound,
			why:  "a Slack identity is linked to nobody in this world, so there is none of mine to unlink",
		},

		/* ------------------------------------------------------------ clusters */
		{method: http.MethodGet, tmpl: "/api/v1/clusters", want: http.StatusOK},
		{
			method: http.MethodPost, tmpl: "/api/v1/clusters",
			body:    map[string]any{"cluster_key": "staging", "display_name": "Staging"},
			want:    http.StatusCreated,
			capture: map[string][]string{"newcluster": {"data", "id"}},
		},
		{
			method: http.MethodPatch, tmpl: "/api/v1/clusters/{id}", url: "/api/v1/clusters/{{newcluster}}",
			body: map[string]any{"display_name": "Staging (eu)"},
			want: http.StatusOK,
		},

		/* ------------------------------------------------------------- sources */
		{method: http.MethodGet, tmpl: "/api/v1/sources", want: http.StatusOK},
		{
			method: http.MethodPost, tmpl: "/api/v1/sources",
			body: map[string]any{
				"name":       "gate-g2-source",
				"cluster_id": "{{newcluster}}",
				"kind":       "alertmanager",
				"base_url":   "{{alertmanager}}",
			},
			want: http.StatusCreated,
			capture: map[string][]string{
				"newsource":    {"data", "source", "id"},
				"ingest_token": {"data", "ingest_token"},
			},
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/sources",
			body: map[string]any{"name": "", "cluster_id": "{{newcluster}}",
				"kind": "alertmanager", "base_url": "{{alertmanager}}"},
			want: http.StatusUnprocessableEntity,
			why:  "a blank name is the commonest 422, and its violations[] is what highlights the control",
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/sources/{id}", url: "/api/v1/sources/{{newsource}}",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/sources/{id}", url: "/api/v1/sources/{{stranger}}",
			want: http.StatusNotFound,
			why:  "another tenant's id must be indistinguishable from one that never existed",
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/sources/{id}", url: "/api/v1/sources/{{newsource}}",
			auth: authNone, want: http.StatusUnauthorized,
		},
		{
			method: http.MethodPatch, tmpl: "/api/v1/sources/{id}", url: "/api/v1/sources/{{newsource}}",
			body: map[string]any{"reconcile_interval_seconds": 60},
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/sources/{id}/health", url: "/api/v1/sources/{{newsource}}/health",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/sources/{id}/rejections",
			url:  "/api/v1/sources/{{newsource}}/rejections?reason=undecodable",
			want: http.StatusOK,
			why: "the per-source rejection feed rides ingest_rejections_source_idx across every " +
				"retained daily partition; an empty page over a partitioned table is exactly the " +
				"query a unit test cannot exercise",
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/sources/{id}/failed-batches",
			url:  "/api/v1/sources/{{newsource}}/failed-batches",
			want: http.StatusOK,
			why:  "the same, for the batches that were accepted and never processed",
		},

		/* -------------------------------------------------------------- ingest */
		// Runs before rotate-token, because rotating invalidates the credential
		// createSource handed back.
		{
			method: http.MethodPost,
			tmpl:   "/api/v1/ingest/alertmanager/{source_id}",
			url:    "/api/v1/ingest/alertmanager/{{newsource}}",
			auth:   authIngest,
			body: map[string]any{
				"version":         "4",
				"groupKey":        `{}:{alertname="GateG2"}`,
				"truncatedAlerts": 0,
				"status":          "firing",
				"receiver":        "oto-webhook",
				"groupLabels":     map[string]any{"alertname": "GateG2"},
				"commonLabels":    map[string]any{"alertname": "GateG2", "severity": "critical"},
				"externalURL":     "http://alertmanager.invalid",
				"alerts": []any{map[string]any{
					"status":       "firing",
					"labels":       map[string]any{"alertname": "GateG2", "severity": "critical"},
					"annotations":  map[string]any{"summary": "gate G2 synthetic"},
					"startsAt":     firing,
					"endsAt":       "0001-01-01T00:00:00Z",
					"generatorURL": "http://prometheus.invalid/graph",
					"fingerprint":  "3f8c1a2b9d4e5f60",
				}},
			},
			want: http.StatusAccepted,
		},
		{
			method: http.MethodPost,
			tmpl:   "/api/v1/ingest/alertmanager/{source_id}",
			url:    "/api/v1/ingest/alertmanager/{{newsource}}",
			auth:   authNone,
			body:   map[string]any{"version": "4"},
			want:   http.StatusUnauthorized,
			why:    "⛔ never 429 and never a transient 4xx: Alertmanager deletes a 4xx notification permanently",
		},

		/* -------------------------------------- sources that talk to the world */
		{
			method: http.MethodPost, tmpl: "/api/v1/sources/{id}/test", url: "/api/v1/sources/{{newsource}}/test",
			want: http.StatusOK,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/sources/{id}/reconcile", url: "/api/v1/sources/{{newsource}}/reconcile",
			want: http.StatusOK,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/sources/{id}/rotate-token", url: "/api/v1/sources/{{newsource}}/rotate-token",
			want: http.StatusOK,
		},

		/* ------------------------------------------------------------ channels */
		{method: http.MethodGet, tmpl: "/api/v1/channel-types", want: http.StatusOK},
		{method: http.MethodGet, tmpl: "/api/v1/channels", want: http.StatusOK},

		// ⭐ THE CONNECTION IS CREATED BEFORE THE CHANNEL, BECAUSE THE CHANNEL
		// CANNOT EXIST WITHOUT ONE (ADR 0047). `channels.connection_id` is NOT
		// NULL, so this is not probe ordering for convenience — it is the order
		// the schema enforces, and the `{{connection}}` fixture below is what
		// `createChannel` spends.
		{method: http.MethodGet, tmpl: "/api/v1/channel-connections", want: http.StatusOK},
		// The payload-mapping catalog embedded in the binary (ADR 0055 §2). It reads no
		// row, so it needs no fixture; what it proves is that every embedded file
		// serialises to the contract's entry shape.
		{method: http.MethodGet, tmpl: "/api/v1/payload-mapping-catalog", want: http.StatusOK},
		{
			method: http.MethodPost, tmpl: "/api/v1/channel-connections",
			body: map[string]any{
				"type":   "webhook",
				"name":   "gate-g2-webhook-connection",
				"config": map[string]any{},
			},
			want:    http.StatusCreated,
			capture: map[string][]string{"connection": {"data", "id"}},
			why:     "a webhook connection may carry no credential at all; a slack one must have a bot token",
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/channel-connections/{id}",
			url:  "/api/v1/channel-connections/{{connection}}",
			want: http.StatusOK,
		},
		{
			method: http.MethodPatch, tmpl: "/api/v1/channel-connections/{id}",
			url:  "/api/v1/channel-connections/{{connection}}",
			body: map[string]any{"name": "gate-g2-webhook-connection-renamed"},
			want: http.StatusOK,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/channel-connections/{id}/mapping/test",
			url:  "/api/v1/channel-connections/{{connection}}/mapping/test",
			body: map[string]any{"channel_id": "00000000-0000-4000-8000-000000000001", "fact": "drawn"},
			want: http.StatusPreconditionFailed,
			why: "the fixture connection carries no payload mapping, and the tester refuses that " +
				"before it looks the channel up — so the 412 is reached without a channel, and " +
				"without sending anything to a tool this gate has no business reaching",
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/channel-connections/{id}/slack/resolve",
			url:  "/api/v1/channel-connections/{{connection}}/slack/resolve",
			body: map[string]any{"name": "sre-alerts"},
			want: http.StatusUnprocessableEntity,
			why: "the fixture connection is a WEBHOOK one, and only Slack can resolve a name to an id — " +
				"the registry refuses the provider rather than reaching for a network this gate has no " +
				"business depending on. Driving the 200 would need a live Slack workspace, which would " +
				"make this gate fail for a reason that is not about oto",
		},

		{
			method: http.MethodPost, tmpl: "/api/v1/channels",
			body: map[string]any{
				"type":          "webhook",
				"name":          "gate-g2-webhook",
				"connection_id": "{{connection}}",
				"config":        map[string]any{"url": "{{webhook}}"},
			},
			want:    http.StatusCreated,
			capture: map[string][]string{"channel": {"data", "id"}},
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/channels/{id}", url: "/api/v1/channels/{{channel}}",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/channels/{id}", url: "/api/v1/channels/{{stranger}}",
			want: http.StatusNotFound,
		},
		{
			method: http.MethodPatch, tmpl: "/api/v1/channels/{id}", url: "/api/v1/channels/{{channel}}",
			body: map[string]any{"verbosity": "status_changes"},
			want: http.StatusOK,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/channels/{id}/test", url: "/api/v1/channels/{{channel}}/test",
			want: http.StatusOK,
		},

		/* --------------------------------------------------- notification templates */
		{method: http.MethodGet, tmpl: "/api/v1/notification-templates", want: http.StatusOK},
		{
			method: http.MethodPost, tmpl: "/api/v1/notification-templates",
			body: map[string]any{
				"name":     "gate-g2-template",
				"provider": "slack",
				"format":   "text",
				"source":   "an alert is firing",
			},
			want:    http.StatusCreated,
			capture: map[string][]string{"template": {"data", "id"}},
			why: "saving RENDERS the template against the shipped fixture corpus, so a 201 here " +
				"proves the engine ran rather than that a row was inserted. " +
				"⚠️ THE SOURCE IS A BARE LITERAL AND HAS TO BE: this table expands `{{name}}` " +
				"from its own fixture map, which is Liquid's interpolation syntax character for " +
				"character, so a realistic template is eaten by the harness before the server " +
				"sees it. And `text` rather than `card` for the same reason — a card needs " +
				"`{{ actions }}` to avoid the warning, and the harness would eat that too. What " +
				"this gate owns is the response shape; interpolation is asserted in " +
				"internal/channels/api and in channels/template's own suite",
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/notification-templates/preview",
			body: map[string]any{
				"format": "text",
				"source": "an alert is firing",
			},
			want: http.StatusOK,
			why: "the preview writes nothing and answers 200 even for a template it would refuse; " +
				"422 is reserved for a malformed request",
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/notification-templates/{id}",
			url:  "/api/v1/notification-templates/{{template}}",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/notification-templates/{id}",
			url:  "/api/v1/notification-templates/{{stranger}}",
			want: http.StatusNotFound,
		},
		{
			method: http.MethodPatch, tmpl: "/api/v1/notification-templates/{id}",
			url:  "/api/v1/notification-templates/{{template}}",
			body: map[string]any{"enabled": false},
			want: http.StatusOK,
		},
		{
			method: http.MethodDelete, tmpl: "/api/v1/notification-templates/{id}",
			url:  "/api/v1/notification-templates/{{template}}",
			want: http.StatusNoContent,
		},

		/* ------------------------------------------------------------ policies */
		{method: http.MethodGet, tmpl: "/api/v1/notification-policies", want: http.StatusOK},
		{
			method: http.MethodPost, tmpl: "/api/v1/notification-policies",
			body: map[string]any{
				"name":        "gate-g2-policy",
				"reasons":     []any{"fired"},
				"channel_ids": []any{"{{channel}}"},
			},
			want:    http.StatusCreated,
			capture: map[string][]string{"policy": {"data", "id"}},
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/notification-policies/preview",
			// ⭐ THE SUBJECT IS THE CASE (git-bug `7570090`). It was `group_id` until
			// `alert_groups` was dropped; `PolicyPreviewRequest` is
			// `additionalProperties: false`, so the old key is now a 400 and this
			// probe would be asserting about a rejection rather than a dry run.
			body:   map[string]any{"case_id": "{{case}}", "reason": "fired"},
			header: idempotency("preview"),
			want:   http.StatusOK,
		},
		{
			method: http.MethodPatch, tmpl: "/api/v1/notification-policies/{id}", url: "/api/v1/notification-policies/{{policy}}",
			body: map[string]any{"priority": 200},
			want: http.StatusOK,
		},
		// Review D4: a policy's digest-window runs. This world asks for none, so the page
		// is empty — what is driven is the route, its 200 shape, and the 404 a stranger
		// policy gets through the same read a Suggestion's apply makes.
		{
			method: http.MethodGet, tmpl: "/api/v1/notification-policies/{id}/investigations",
			url: "/api/v1/notification-policies/{{policy}}/investigations", want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/notification-policies/{id}/investigations",
			url: "/api/v1/notification-policies/{{stranger}}/investigations", want: http.StatusNotFound,
		},

		/* -------------------------------------------------------------- alerts */
		{method: http.MethodGet, tmpl: "/api/v1/alerts", want: http.StatusOK},
		{method: http.MethodGet, tmpl: "/api/v1/alerts", auth: authNone, want: http.StatusUnauthorized},
		{
			method: http.MethodGet, tmpl: "/api/v1/alerts/rollups",
			url:  "/api/v1/alerts/rollups?group_by=alertname",
			want: http.StatusOK,
		},
		{method: http.MethodGet, tmpl: "/api/v1/alerts/{id}", url: "/api/v1/alerts/{{alert}}", want: http.StatusOK},
		{
			method: http.MethodGet, tmpl: "/api/v1/alerts/{id}", url: "/api/v1/alerts/{{stranger}}",
			want: http.StatusNotFound,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/alerts/{id}/cases", url: "/api/v1/alerts/{{alert}}/cases",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/alerts/{id}/events", url: "/api/v1/alerts/{{alert}}/events",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/alerts/{id}/enrichments", url: "/api/v1/alerts/{{alert}}/enrichments",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/alerts/{id}/rule", url: "/api/v1/alerts/{{alert}}/rule",
			want: http.StatusOK,
			why: "the fixture alert carries no rule provenance, and absence is not a client error — " +
				"`RuleHistoryDTO.current` is already `oneOf [RuleSnapshotDTO, null]`, so the 200 " +
				"spelling for \"oto captured no rule for this alert\" was always in the contract. " +
				"This asserted 422 until 015b25b: four of five real alerts hit it, and the detail " +
				"page painted \"Validation failed — a rule key's source id must be a UUID\" with a " +
				"Try again button that could never succeed",
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/alerts/{id}/notifications", url: "/api/v1/alerts/{{alert}}/notifications",
			want: http.StatusOK,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/alerts/{id}/comments", url: "/api/v1/alerts/{{alert}}/comments",
			body: map[string]any{"body": "tracking upstream"}, header: idempotency("comment"),
			want: http.StatusCreated,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/alerts/{id}/comments", url: "/api/v1/alerts/{{alert}}/comments",
			body: map[string]any{"body": ""}, header: idempotency("comment-blank"),
			want: http.StatusUnprocessableEntity,
			why:  "a blank comment is refused, and the refusal is a Problem a client renders",
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/alerts/{id}/snooze", url: "/api/v1/alerts/{{alert}}/snooze",
			body: map[string]any{"duration_seconds": 3600, "note": "deploy window"},
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/alerts/{id}/snoozes", url: "/api/v1/alerts/{{alert}}/snoozes",
			want: http.StatusOK,
		},
		{method: http.MethodGet, tmpl: "/api/v1/snoozes", want: http.StatusOK},
		{
			method: http.MethodPost, tmpl: "/api/v1/alerts/{id}/unsnooze", url: "/api/v1/alerts/{{alert}}/unsnooze",
			body: map[string]any{"note": "back on"},
			want: http.StatusOK,
		},
		// The BULK wake. It runs AFTER the single one deliberately: the fixture
		// alert is awake by now, so the account this drives is the SKIP path —
		// `not_snoozed` for an alert nobody snoozed and `alert_not_found` for the
		// stranger's id — and both are still a `200`, which is the shape most worth
		// validating. Waking the alert here instead would leave the probe above
		// asserting a `412`.
		{
			method: http.MethodPost, tmpl: "/api/v1/alerts/unsnooze",
			body: map[string]any{
				"alert_ids": []any{"{{alert}}", "{{stranger}}"},
				"note":      "wake the selection",
			},
			want: http.StatusOK,
			why:  "a bulk wake where nothing woke is an account, not a refusal",
		},

		/* --------------------------------------------------------- cases */
		{
			method: http.MethodGet, tmpl: "/api/v1/cases", want: http.StatusOK,
			why: "the ORG-WIDE case list, unfiltered: the shape a client lands on",
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/cases",
			url:  "/api/v1/cases?state=open&ack=unacked&limit=50",
			want: http.StatusOK,
			why: "the query the endpoint exists for. Since ADR 0040 collapsed `?open=` into " +
				"`?state=`, THIS is the parameter that has to reach case_ack_idx — a partial " +
				"index is matched against the query's own predicates and never against " +
				"case_terminal_ended, so `ListCases` emits the axis as the liveness literal " +
				"and only a real Parse proves the spliced statement still binds",
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/cases",
			url:  "/api/v1/cases?ack=unacked,acked&state=closed&severity=critical&cluster=staging&since=2020-01-01T00:00:00Z",
			want: http.StatusOK,
			why: "the MULTI-VALUE ack arm, the CLOSED half of the state axis and the " +
				"alert-side EXISTS. The single-value arm above is spelled " +
				"`ack_state = ($2)[1]` and this one `= ANY($2)`; only a real Parse against a " +
				"real Postgres proves both bind $2 and both plan — and 00054 RENUMBERED this " +
				"statement, so a stale `$3` here would be a Parse failure rather than a diff",
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/cases",
			url:  "/api/v1/cases?state=open,closed",
			want: http.StatusOK,
			why: "naming BOTH values of a two-value enum must be no constraint at all. " +
				"`ListCases` only splices a predicate for a single value; an edit that " +
				"emitted one per value would ask for `ended_at IS NULL AND IS NOT NULL` and " +
				"answer an empty page, which looks exactly like a quiet estate",
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/cases", url: "/api/v1/cases?ack=maybe",
			want: http.StatusUnprocessableEntity,
			why:  "an ack state outside the closed enum is a 422, and its violations[] is what highlights the control",
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/cases", url: "/api/v1/cases?state=firing",
			want: http.StatusUnprocessableEntity,
			why: "`firing` is the ALERT's word. It was a legal value of this parameter one " +
				"migration ago, so the 422 is what proves the collapse reached validation and " +
				"not only the SQL",
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/cases", url: "/api/v1/cases?open=true",
			want: http.StatusBadRequest,
			why: "the removed parameter. Unknown query parameters are REFUSED rather than " +
				"ignored (400 unknown_parameter), so a client still sending `open=true` is " +
				"told plainly instead of silently receiving closed episodes in its live queue",
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/cases/{id}", url: "/api/v1/cases/{{case}}",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/cases/{id}", url: "/api/v1/cases/{{stranger}}",
			want: http.StatusNotFound,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/cases/{id}/events", url: "/api/v1/cases/{{case}}/events",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/cases/{id}/rule", url: "/api/v1/cases/{{case}}/rule",
			want: http.StatusNotFound,
			why:  "no rule snapshot was ever captured for this episode, which is the ordinary answer for an alert that arrived without one",
		},
		// ⭐ ORDER MATTERS: ack runs before unack so the episode has a receipt to
		// take back, and both run before the GROUP ack below so the fan-out finds
		// its member unacked. `{id}` is a CASE id on both: a receipt is a fact
		// about one firing episode, and the alert-addressed spelling had to resolve
		// "whatever is open right now" to find its subject.
		{
			method: http.MethodPost, tmpl: "/api/v1/cases/{id}/ack", url: "/api/v1/cases/{{case}}/ack",
			body: map[string]any{"note": "seen, rolling back"}, header: idempotency("ack"),
			want: http.StatusOK,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/cases/{id}/unack", url: "/api/v1/cases/{{case}}/unack",
			body: map[string]any{"note": "still open"}, header: idempotency("unack"),
			want: http.StatusOK,
		},

		/* ----------------------------------------------------------- incidents */
		// ADR 0052: a human draws an Incident over Cases, and a Case belongs to at
		// most one. The order walks the membership lifecycle — draw, refuse a second
		// draw over the same Case, draw a second Incident over a second Case, refuse
		// adding that Case where it is not, move it, remove it, add it back — so
		// every refusal the at-most-one rule produces is observed on the wire, not
		// only every success. `{number}` is the per-org number the create captured.
		{method: http.MethodGet, tmpl: "/api/v1/incidents", want: http.StatusOK},
		{
			method: http.MethodPost, tmpl: "/api/v1/incidents",
			body:    map[string]any{"case_ids": []string{"{{case}}"}},
			want:    http.StatusCreated,
			capture: map[string][]string{"incident": {"data", "number"}},
		},
		// Which Incident a Case is in now — the read a screen asks before it offers
		// a move rather than a second draw (git-bug f89c9cc).
		{
			method: http.MethodGet, tmpl: "/api/v1/incidents", url: "/api/v1/incidents?case_id={{case}}",
			want: http.StatusOK,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/incidents",
			body: map[string]any{"case_ids": []string{"{{case}}"}},
			want: http.StatusConflict,
			why: "a Case belongs to at most one Incident: the second draw is refused by " +
				"incident_members_case_live_uniq and its detail points at the move",
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/incidents",
			body:    map[string]any{"case_ids": []string{"{{case2}}"}},
			want:    http.StatusCreated,
			capture: map[string][]string{"incident2": {"data", "number"}},
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/incidents/{number}", url: "/api/v1/incidents/{{incident}}",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/incidents/{number}", url: "/api/v1/incidents/999999",
			want: http.StatusNotFound,
			why:  "a number this org never drew is indistinguishable from one another org drew",
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/incidents/{number}/cases", url: "/api/v1/incidents/{{incident}}/cases",
			body: map[string]any{"case_id": "{{case2}}"},
			want: http.StatusConflict,
			why:  "the Case is in the second Incident, so adding it here is refused with a pointer to move",
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/incidents/{number}/cases/{case_id}/move",
			url:     "/api/v1/incidents/{{incident2}}/cases/{{case2}}/move",
			rawBody: `{"to_number": {{incident}}}`,
			want:    http.StatusOK,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/incidents/{number}/cases/{case_id}/remove",
			url:  "/api/v1/incidents/{{incident}}/cases/{{case2}}/remove",
			want: http.StatusOK,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/incidents/{number}/cases/{case_id}/remove",
			url:  "/api/v1/incidents/{{incident}}/cases/{{case2}}/remove",
			want: http.StatusNotFound,
			why:  "the membership is a tombstone now; a second removal finds no current member to remove",
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/incidents/{number}/cases", url: "/api/v1/incidents/{{incident2}}/cases",
			body: map[string]any{"case_id": "{{case2}}"},
			want: http.StatusOK,
		},

		/* --------------------------------------------------------- correlators */
		// ADR 0052 §2 (git-bug 61eeddf): the operator-written definitions that draw
		// Incidents. List, write, reorder by `priority`; the DELETE is in the
		// teardown group with the others.
		//
		// ⭐ ITS MATCHER NAMES AN `alertname` NO PROBE INGESTS. A Correlator acts on
		// every Case that opens after it is written, so one matching the gate's own
		// alerts would draw Incidents under the Incident probes above depending on
		// where in the table it ran.
		{method: http.MethodGet, tmpl: "/api/v1/correlators", want: http.StatusOK},
		{
			method: http.MethodPost, tmpl: "/api/v1/correlators",
			body: map[string]any{
				"name":                 "GateG2 storm",
				"matchers":             []map[string]string{{"name": "alertname", "op": "=", "value": "GateG2Correlator"}},
				"count_min":            5,
				"count_window_seconds": 600,
				"quiet_grace_seconds":  1800,
				// ADR 0052 §6. Harmless here for the matcher's reason above: no probe
				// ingests the alertname, so no Incident of it ever has a thread.
				"incidents_are_conversations": true,
			},
			want:    http.StatusCreated,
			capture: map[string][]string{"correlator": {"data", "id"}},
		},
		{
			method: http.MethodPatch, tmpl: "/api/v1/correlators/{id}", url: "/api/v1/correlators/{{correlator}}",
			body: map[string]any{"priority": 10},
			want: http.StatusOK,
		},

		/* ------------------------------------------------------- investigators */
		// ADR 0053 (git-bug 8f1f071, 180a525): a model endpoint, an Investigator that
		// dials it, and one Investigation requested against the fixture Case. The
		// order is the dependency order — the Investigator names the endpoint, the
		// request names the Investigator, the run read names the request's answer.
		//
		// ⭐ THE ENDPOINT CARRIES NO KEY, AND THAT IS THE CONTAINER'S SHAPE, NOT A
		// SHORTCUT. This world boots without `security.secret_key`, so there is no
		// keyring to seal one with; a keyless endpoint is legal (a self-hosted model on
		// the cluster network), and `has_key: false` is the half of the response a
		// keyed one would not show.
		//
		// ⭐ NOTHING IS DIALLED. Creating an endpoint stores a row, and the request
		// answers 202 with the run `queued`: this container enqueues jobs but works
		// none, so no model is reached and the `.invalid` host is never resolved.
		{method: http.MethodGet, tmpl: "/api/v1/model-providers", want: http.StatusOK},
		{
			method: http.MethodPost, tmpl: "/api/v1/model-providers",
			body: map[string]any{
				"name":     "gate-g2-endpoint",
				"base_url": "https://model.invalid/v1",
				"model":    "gate-g2-model",
			},
			want:    http.StatusCreated,
			capture: map[string][]string{"modelprovider": {"data", "id"}},
		},
		{method: http.MethodGet, tmpl: "/api/v1/investigators", want: http.StatusOK},
		{
			method: http.MethodPost, tmpl: "/api/v1/investigators",
			body: map[string]any{
				"name":              "gateg2",
				"model_provider_id": "{{modelprovider}}",
				"prompt":            "Read the Case.",
				"tools":             []any{"oto_case_timeline"},
			},
			want:    http.StatusCreated,
			capture: map[string][]string{"investigator": {"data", "id"}},
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/investigators",
			body: map[string]any{
				"name":              "gateg2wild",
				"model_provider_id": "{{modelprovider}}",
				"prompt":            "Read the Case.",
				"tools":             []any{"oto_*"},
			},
			want: http.StatusUnprocessableEntity,
			why:  "an allowlist names Tools exactly; a wildcard is refused, and its violations[] points at `tools`",
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/investigators/{id}", url: "/api/v1/investigators/{{investigator}}",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/investigators/{id}", url: "/api/v1/investigators/{{stranger}}",
			want: http.StatusNotFound,
		},
		{
			method: http.MethodPatch, tmpl: "/api/v1/investigators/{id}", url: "/api/v1/investigators/{{investigator}}",
			body: map[string]any{"prompt": "Read the Case, then its rule."},
			want: http.StatusOK,
			why:  "a changed prompt writes version 2, so the response's versions[] carries two entries to validate",
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/cases/{id}/investigations", url: "/api/v1/cases/{{case}}/investigations",
			body:    map[string]any{"investigator_id": "{{investigator}}"},
			want:    http.StatusAccepted,
			capture: map[string][]string{"investigation": {"data", "id"}},
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/cases/{id}/investigations", url: "/api/v1/cases/{{case}}/investigations",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/investigations/{id}", url: "/api/v1/investigations/{{investigation}}",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/investigations/{id}", url: "/api/v1/investigations/{{stranger}}",
			want: http.StatusNotFound,
		},
		// ADR 0053 §2 (git-bug 8327c00): a Finding's Suggestions. The run above is
		// recorded `queued` and never worked here, so it has no Finding and its list is
		// EMPTY — still a 2xx validated against SuggestionListResponse. Applying needs a
		// Suggestion, and only a run's Finding makes one, so apply is driven to its typed
		// 404: driven, not credited.
		{
			method: http.MethodGet, tmpl: "/api/v1/investigations/{id}/suggestions",
			url: "/api/v1/investigations/{{investigation}}/suggestions", want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/investigations/{id}/suggestions",
			url: "/api/v1/investigations/{{stranger}}/suggestions", want: http.StatusNotFound,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/suggestions/{id}/apply", url: "/api/v1/suggestions/{{stranger}}/apply",
			body: map[string]any{}, want: http.StatusNotFound,
			why: "a Suggestion is made only by a run's Finding, and this world works no runs; suggestion_not_found",
		},
		// ADR 0054 (git-bug 4148256): a Finding's Remedies. The run above has no Finding, so
		// its list is EMPTY — still a 2xx validated against RemedyListResponse. A Remedy is
		// made only by a run's Finding, so the read, the approve and the decline are driven
		// to their typed 404s: driven, not credited.
		{
			method: http.MethodGet, tmpl: "/api/v1/investigations/{id}/remedies",
			url: "/api/v1/investigations/{{investigation}}/remedies", want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/investigations/{id}/remedies",
			url: "/api/v1/investigations/{{stranger}}/remedies", want: http.StatusNotFound,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/remedies/{id}", url: "/api/v1/remedies/{{stranger}}",
			want: http.StatusNotFound,
			why:  "a Remedy is made only by a run's Finding, and this world works no runs; remedy_not_found",
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/remedies/{id}/approve", url: "/api/v1/remedies/{{stranger}}/approve",
			body: map[string]any{"arguments_sha256": "0000000000000000000000000000000000000000000000000000000000000000"}, want: http.StatusNotFound,
			why: "a Remedy is made only by a run's Finding, and this world works no runs; remedy_not_found",
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/remedies/{id}/decline", url: "/api/v1/remedies/{{stranger}}/decline",
			want: http.StatusNotFound,
			why:  "a Remedy is made only by a run's Finding, and this world works no runs; remedy_not_found",
		},
		// ADR 0053 §4 (git-bug 74ea849): an Incident investigated as a whole, asked
		// about by the number the incident probes above captured. Nothing is dialled,
		// for the Case request's reason: the run is recorded `queued` and its job is
		// never worked here.
		{
			method: http.MethodPost, tmpl: "/api/v1/incidents/{number}/investigations",
			url:  "/api/v1/incidents/{{incident}}/investigations",
			body: map[string]any{"investigator_id": "{{investigator}}"},
			want: http.StatusAccepted,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/incidents/{number}/investigations",
			url: "/api/v1/incidents/{{incident}}/investigations", want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/incidents/{number}/investigations",
			url: "/api/v1/incidents/999999/investigations", want: http.StatusNotFound,
		},
		// ADR 0053 §5 (git-bug 4298aa0): the org's Classification set. A fresh org reads
		// it EMPTY — oto ships no classes — then writes two, and `unclassified` is driven
		// to its 422 because it is reserved, not the operator's to write.
		{method: http.MethodGet, tmpl: "/api/v1/investigation-classes", want: http.StatusOK},
		{
			method: http.MethodPut, tmpl: "/api/v1/investigation-classes",
			body: map[string]any{"classes": []map[string]any{
				{"name": "deploy-regression", "description": "A change we shipped broke it."},
				{"name": "capacity"},
			}},
			want: http.StatusOK,
		},
		{
			method: http.MethodPut, tmpl: "/api/v1/investigation-classes",
			body: map[string]any{"classes": []map[string]any{{"name": "unclassified"}}},
			want: http.StatusUnprocessableEntity,
			why:  "unclassified is always admissible and is reserved; the set it was refused against stands",
		},
		// ADR 0054 §3 (git-bug eb4f21b): the org's Remedy risk rules. A fresh org reads them
		// EMPTY — oto ships no rule, and every Remedy needs two. ⛔ There is no write to probe:
		// `oto remedy-rules apply` writes them from the host shell (owner ruling 2026-10-05).
		{method: http.MethodGet, tmpl: "/api/v1/remedy-risk-rules", want: http.StatusOK},

		/* -------------------------------------------------------- tool servers */
		// ADR 0053 §3, 0054 §5 (git-bug 2e9a086): an operator's MCP server, configured,
		// read back, and asked for its Tools. In dependency order — every later probe
		// names the ToolServer the create captured.
		//
		// ⭐ IT CARRIES NO TOKEN, for the model endpoint's reason above: this world has
		// no keyring to seal one with, and a ToolServer that takes no token is legal.
		//
		// ⚠️ DISCOVERY IS DRIVEN TO ITS 502, AND THAT IS THE ONLY HONEST ANSWER HERE.
		// This world runs no MCP server, so asking `.invalid` for its Tools fails the
		// way an unreachable ToolServer does — recorded on the row as
		// `discovery_error`, answered as the contract's BadGateway Problem. The Tool
		// list read after it is the last GOOD list, which is empty and still a 200.
		{method: http.MethodGet, tmpl: "/api/v1/tool-servers", want: http.StatusOK},
		{
			method: http.MethodPost, tmpl: "/api/v1/tool-servers",
			body: map[string]any{
				"name":   "gate-g2-tools",
				"url":    "https://toolserver.invalid/mcp",
				"access": "read",
			},
			want:    http.StatusCreated,
			capture: map[string][]string{"toolserver": {"data", "id"}},
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/tool-servers/{id}", url: "/api/v1/tool-servers/{{toolserver}}",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/tool-servers/{id}", url: "/api/v1/tool-servers/{{stranger}}",
			want: http.StatusNotFound,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/tool-servers/{id}/discover", url: "/api/v1/tool-servers/{{toolserver}}/discover",
			want: http.StatusBadGateway,
			why:  "this world runs no MCP server; an unreachable ToolServer is a 502, and the failure is recorded on it",
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/tool-servers/{id}/tools", url: "/api/v1/tool-servers/{{toolserver}}/tools",
			want: http.StatusOK,
		},
		// ADR 0054 §4 (git-bug 47f67c8): who holds the Remedy approval grant. Read-only —
		// `oto grant` on the host is the only writer, so there is no write probe to make
		// and this world's read ToolServer lists none.
		{
			method: http.MethodGet, tmpl: "/api/v1/tool-servers/{id}/remedy-approvers",
			url: "/api/v1/tool-servers/{{toolserver}}/remedy-approvers", want: http.StatusOK,
		},

		/* ------------------------------------------------------- case policies */
		// The case RETENTION WINDOW W, per (namespace, alertname). The shape is
		// `/api/v1/clusters`'s and so is the probe order: list, create by the
		// immutable natural key, patch the one mutable value by the id the create
		// handed back. The DELETE is in the teardown group with the others.
		//
		// ⭐ THE `alertname` IS ONE NO OTHER PROBE INGESTS. W decides when a case
		// closes, so a window written here against `GateG2` would change what the
		// case probes above observe depending on where in the table it ran.
		{method: http.MethodGet, tmpl: "/api/v1/case-policies", want: http.StatusOK},
		{
			method: http.MethodPost, tmpl: "/api/v1/case-policies",
			body: map[string]any{
				"namespace":                "staging",
				"alertname":                "GateG2CasePolicy",
				"retention_window_seconds": 600,
			},
			want:    http.StatusCreated,
			capture: map[string][]string{"casepolicy": {"data", "id"}},
		},
		{
			method: http.MethodPatch, tmpl: "/api/v1/case-policies/{id}", url: "/api/v1/case-policies/{{casepolicy}}",
			body: map[string]any{"retention_window_seconds": 1200},
			want: http.StatusOK,
		},

		/* -------------------------------------------------------- alert groups */
		// ⛔ TEN PROBES WERE HERE AND ALL TEN ARE DELETED (git-bug `7570090`).
		// `/api/v1/alert-groups`, `/{id}`, `/{id}/alerts`, `/{id}/timeline` and the
		// six verbs (`ack`, `unack`, `comments`, `snooze`, `unsnooze`) left the
		// contract with the entity they addressed: `alert_groups` is dropped and a
		// conversation holds exactly one Case. They are DELETED rather than
		// retargeted at `/api/v1/cases/...`, because the Case surface has its own
		// probes in the case section above — retargeting would have duplicated them
		// under a heading naming a thing that no longer exists.
		//
		// ⚠️ The gate below is a TOTALITY check over the contract's paths, so a
		// probe left here for a removed path fails as an unknown template, and a
		// path added back without a probe fails as an uncovered one. Neither is a
		// thing this comment can drift out of agreement with.

		/* ------------------------------------------------------ rule snapshots */
		{
			method: http.MethodGet, tmpl: "/api/v1/rule-snapshots",
			url:  "/api/v1/rule-snapshots?source_id={{source}}&rule_name=HighErrorRate",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/rule-snapshots/batch",
			url:  "/api/v1/rule-snapshots/batch?id={{stranger}}",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/rule-snapshots/{id}", url: "/api/v1/rule-snapshots/{{stranger}}",
			want: http.StatusNotFound,
		},

		/* --------------------------------------- notifications and deliveries */
		{method: http.MethodGet, tmpl: "/api/v1/notifications", want: http.StatusOK},
		{
			method: http.MethodGet, tmpl: "/api/v1/notifications/{id}", url: "/api/v1/notifications/{{stranger}}",
			want: http.StatusNotFound,
		},
		{method: http.MethodGet, tmpl: "/api/v1/deliveries", want: http.StatusOK},
		{
			method: http.MethodGet, tmpl: "/api/v1/deliveries/{id}", url: "/api/v1/deliveries/{{stranger}}",
			want: http.StatusNotFound,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/deliveries/{id}/retry", url: "/api/v1/deliveries/{{stranger}}/retry",
			want: http.StatusNotFound,
		},

		/* ------------------------------------------------------------ silences */
		{method: http.MethodGet, tmpl: "/api/v1/silences", want: http.StatusOK},
		{
			method: http.MethodGet, tmpl: "/api/v1/silences/{id}", url: "/api/v1/silences/{{stranger}}",
			want: http.StatusNotFound,
		},

		/* ---------------------------------------------- labels and enrichments */
		{method: http.MethodGet, tmpl: "/api/v1/labels", want: http.StatusOK},
		{
			method: http.MethodGet, tmpl: "/api/v1/labels/{name}/values", url: "/api/v1/labels/severity/values",
			want: http.StatusOK,
		},
		{method: http.MethodGet, tmpl: "/api/v1/enrichers", want: http.StatusOK},

		/* -------------------------------------------------------------- drills */
		{
			method: http.MethodPost, tmpl: "/api/v1/drills",
			body:    map[string]any{"source_id": "{{newsource}}", "severity": "critical"},
			want:    http.StatusAccepted,
			capture: map[string][]string{"drill": {"data", "id"}},
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/drills", url: "/api/v1/drills?source_id={{newsource}}",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/drills",
			want: http.StatusBadRequest,
			why: "`source_id` is `required: true` on a QUERY string, so omitting it never formed a " +
				"valid request: that is the 400 malformed_request family, not a 422. This answered " +
				"an undeclared 422 with no violations[] until ee3ae9c, on the plainest request there is",
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/drills/{id}", url: "/api/v1/drills/{{drill}}",
			want: http.StatusOK,
		},
		{
			method: http.MethodDelete, tmpl: "/api/v1/drills/{id}", url: "/api/v1/drills/{{drill}}",
			want: http.StatusPreconditionFailed,
			why:  "this container ENQUEUES but does not WORK jobs, so the drill is still running and disposing of a running drill is refused",
		},

		/* --------------------------------------------------------------- stats */
		{method: http.MethodGet, tmpl: "/api/v1/stats/overview", want: http.StatusOK},
		{method: http.MethodGet, tmpl: "/api/v1/stats/alert-quality", want: http.StatusOK},

		/* -------------------------------------------------------------- stream */
		{
			method: http.MethodGet, tmpl: "/api/v1/stream",
			want: http.StatusOK, stream: true,
			header: map[string]string{"Accept": "text/event-stream"},
		},

		/* ------------------------------------------------------- slack callback */
		{
			method: http.MethodPost, tmpl: "/api/v1/integrations/slack/interactions",
			auth: authNone,
			header: map[string]string{
				"X-Slack-Request-Timestamp": "1",
				"X-Slack-Signature":         "v0=deadbeef",
				"Content-Type":              "application/x-www-form-urlencoded",
			},
			rawBody: "payload=%7B%7D",
			want:    http.StatusUnauthorized,
			why:     "an unsigned callback is refused; the SIGNED 200 needs a Slack channel and its signing secret",
		},

		/* ------------------------------------------------- teardown (DELETEs) */
		// Last, and in dependency order: a policy references a channel, so the
		// policy goes first. The case retention window goes ahead of all of them
		// because nothing points at it — removing the row simply restores W=0.
		{
			method: http.MethodDelete, tmpl: "/api/v1/case-policies/{id}",
			url:  "/api/v1/case-policies/{{casepolicy}}",
			want: http.StatusNoContent,
		},
		{
			method: http.MethodDelete, tmpl: "/api/v1/notification-policies/{id}",
			url:  "/api/v1/notification-policies/{{policy}}",
			want: http.StatusNoContent,
		},
		{
			method: http.MethodDelete, tmpl: "/api/v1/correlators/{id}", url: "/api/v1/correlators/{{correlator}}",
			want: http.StatusNoContent,
		},
		{
			method: http.MethodDelete, tmpl: "/api/v1/channels/{id}", url: "/api/v1/channels/{{channel}}",
			want: http.StatusNoContent,
		},
		// ⛔ AND ONLY NOW. `channels.connection_id` is ON DELETE RESTRICT, so this
		// probe is a 409 for as long as the channel above still stands — which is
		// the invariant, not an ordering nuisance.
		{
			method: http.MethodDelete, tmpl: "/api/v1/channel-connections/{id}",
			url:  "/api/v1/channel-connections/{{connection}}",
			want: http.StatusNoContent,
		},
		{
			method: http.MethodDelete, tmpl: "/api/v1/sources/{id}", url: "/api/v1/sources/{{newsource}}",
			want: http.StatusNoContent,
		},
		{
			method: http.MethodDelete, tmpl: "/api/v1/api-tokens/{id}", url: "/api/v1/api-tokens/{{token}}",
			auth: authSession, want: http.StatusNoContent,
		},
		// The session dies last, because it is the credential the logout probe
		// itself presents.
		{
			method: http.MethodPost, tmpl: "/api/v1/auth/logout", auth: authSession,
			want: http.StatusNoContent,
		},
	}
}
