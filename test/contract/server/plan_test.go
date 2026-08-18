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
			body: map[string]any{"refire_grace_s": 1800},
			want: http.StatusOK,
		},
		{method: http.MethodGet, tmpl: "/api/v1/api-tokens", auth: authSession, want: http.StatusOK},
		{
			method: http.MethodPost, tmpl: "/api/v1/api-tokens", auth: authSession,
			body:    map[string]any{"name": "gate-g2-minted"},
			want:    http.StatusCreated,
			capture: map[string][]string{"token": {"data", "token", "id"}},
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
		{
			method: http.MethodPost, tmpl: "/api/v1/channels",
			body: map[string]any{
				"type":   "webhook",
				"name":   "gate-g2-webhook",
				"config": map[string]any{"url": "{{webhook}}"},
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
			body:   map[string]any{"group_id": "{{group}}", "reason": "fired"},
			header: idempotency("preview"),
			want:   http.StatusOK,
		},
		{
			method: http.MethodPatch, tmpl: "/api/v1/notification-policies/{id}", url: "/api/v1/notification-policies/{{policy}}",
			body: map[string]any{"priority": 200},
			want: http.StatusOK,
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
			method: http.MethodPost, tmpl: "/api/v1/alerts/{id}/ack", url: "/api/v1/alerts/{{alert}}/ack",
			body: map[string]any{"note": "seen, rolling back"}, header: idempotency("ack"),
			want: http.StatusOK,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/alerts/{id}/unack", url: "/api/v1/alerts/{{alert}}/unack",
			body: map[string]any{"note": "still open"}, header: idempotency("unack"),
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

		/* --------------------------------------------------------- cases */
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

		/* -------------------------------------------------------- alert groups */
		{method: http.MethodGet, tmpl: "/api/v1/alert-groups", want: http.StatusOK},
		{
			method: http.MethodGet, tmpl: "/api/v1/alert-groups/{id}", url: "/api/v1/alert-groups/{{group}}",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/alert-groups/{id}", url: "/api/v1/alert-groups/{{stranger}}",
			want: http.StatusNotFound,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/alert-groups/{id}/alerts", url: "/api/v1/alert-groups/{{group}}/alerts",
			want: http.StatusOK,
		},
		{
			method: http.MethodGet, tmpl: "/api/v1/alert-groups/{id}/timeline", url: "/api/v1/alert-groups/{{group}}/timeline",
			want: http.StatusOK,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/alert-groups/{id}/ack", url: "/api/v1/alert-groups/{{group}}/ack",
			body: map[string]any{"note": "on it"}, header: idempotency("group-ack"),
			want: http.StatusOK,
		},
		// ⭐ ORDER MATTERS: this runs AFTER the ack above, so the generation has a
		// receipt to take back and the withdrawal is a 200 rather than the 412 a
		// never-acked group would earn. The ack surface moved onto the Case detail
		// and a case you can ack but never un-ack is a trap, which is why this verb
		// exists at all.
		{
			method: http.MethodPost, tmpl: "/api/v1/alert-groups/{id}/unack", url: "/api/v1/alert-groups/{{group}}/unack",
			body: map[string]any{"note": "handing back"}, header: idempotency("group-unack"),
			want: http.StatusOK,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/alert-groups/{id}/comments", url: "/api/v1/alert-groups/{{group}}/comments",
			body: map[string]any{"body": "group note"}, header: idempotency("group-comment"),
			want: http.StatusCreated,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/alert-groups/{id}/snooze", url: "/api/v1/alert-groups/{{group}}/snooze",
			body: map[string]any{"duration_seconds": 1800},
			want: http.StatusOK,
		},
		{
			method: http.MethodPost, tmpl: "/api/v1/alert-groups/{id}/unsnooze", url: "/api/v1/alert-groups/{{group}}/unsnooze",
			body: map[string]any{},
			want: http.StatusOK,
		},

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
		// policy goes first.
		{
			method: http.MethodDelete, tmpl: "/api/v1/notification-policies/{id}",
			url:  "/api/v1/notification-policies/{{policy}}",
			want: http.StatusNoContent,
		},
		{
			method: http.MethodDelete, tmpl: "/api/v1/channels/{id}", url: "/api/v1/channels/{{channel}}",
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
