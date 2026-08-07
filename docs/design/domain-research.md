# Domain Research: Prometheus Alertmanager + Slack

Research date: **2026-08-07**.
Primary sources pinned to **Alertmanager `main` @ 2026-08-07** (latest tagged release **0.33.1**, 2026-07-04) and **Prometheus `main`**.

Every non-obvious claim below carries a source. Anything I could not verify is marked **UNVERIFIED**.

---

# A. Prometheus Alertmanager

## A0. Version landscape

| Release | Date |
|---|---|
| 0.33.1 | 2026-07-04 |
| 0.33.0 | 2026-06-12 |
| 0.32.0 | 2026-04-08 |
| 0.31.0 | 2026-02-02 |
| 0.30.0 | 2025-12-15 |
| 0.29.0 | 2025-11-01 |
| 0.28.0 | 2025-01-15 |
| 0.27.0 | 2024-02-28 |

Source: [CHANGELOG.md](https://github.com/prometheus/alertmanager/blob/main/CHANGELOG.md)

---

## A1. Webhook receiver JSON payload schema

### The documented shape

Source: [docs/configuration.md `<webhook_config>`](https://github.com/prometheus/alertmanager/blob/main/docs/configuration.md) / [prometheus.io webhook_config](https://prometheus.io/docs/alerting/latest/configuration/#webhook_config)

```
{
  "version": "4",
  "groupKey": <string>,              // key identifying the group of alerts (e.g. to deduplicate)
  "truncatedAlerts": <int>,          // how many alerts have been truncated due to "max_alerts"
  "status": "<resolved|firing>",
  "receiver": <string>,
  "groupLabels": <object>,
  "commonLabels": <object>,
  "commonAnnotations": <object>,
  "externalURL": <string>,           // backlink to the Alertmanager.
  "notification_reason": <string>,   // string represent the reason this notification was generated
  "alerts": [
    {
      "status": "<resolved|firing>",
      "labels": <object>,
      "annotations": <object>,
      "startsAt": "<rfc3339>",
      "endsAt": "<rfc3339>",
      "generatorURL": <string>,      // identifies the entity that caused the alert
      "fingerprint": <string>        // fingerprint to identify the alert
    },
    ...
  ]
}
```

### The actual wire shape (from source) — includes an UNDOCUMENTED field

The payload is `notify/webhook.Message` embedding `template.Data`:

```go
// notify/webhook/webhook.go
type Message struct {
	*template.Data
	Version         string `json:"version"`
	GroupKey        string `json:"groupKey"`
	TruncatedAlerts uint64 `json:"truncatedAlerts"`
}
```

```go
// template/template.go
type Data struct {
	Receiver string `json:"receiver"`
	Status   string `json:"status"`
	Alerts   Alerts `json:"alerts"`

	NotificationReason string `json:"notification_reason"`

	GroupLabels       KV `json:"groupLabels"`
	CommonLabels      KV `json:"commonLabels"`
	CommonAnnotations KV `json:"commonAnnotations"`
	RouteLabels       KV `json:"routeLabels"`   // <-- NOT in the published docs

	ExternalURL string `json:"externalURL"`
}

type Alert struct {
	Status       string    `json:"status"`
	Labels       KV        `json:"labels"`
	Annotations  KV        `json:"annotations"`
	StartsAt     time.Time `json:"startsAt"`
	EndsAt       time.Time `json:"endsAt"`
	GeneratorURL string    `json:"generatorURL"`
	Fingerprint  string    `json:"fingerprint"`
}
```

Sources: [webhook.go](https://github.com/prometheus/alertmanager/blob/main/notify/webhook/webhook.go), [template.go](https://github.com/prometheus/alertmanager/blob/main/template/template.go)

> **Finding:** `routeLabels` is emitted on the wire but is absent from the published webhook docs. It is populated from the route's `labels:` config (a 0.33-era feature; see A1.1). Do not rely on the docs alone — decode leniently.

### Literal example payload

```json
{
  "version": "4",
  "groupKey": "{}/{severity=\"critical\"}:{alertname=\"HighErrorRate\", cluster=\"prod-eu\"}",
  "truncatedAlerts": 0,
  "status": "firing",
  "receiver": "oto-webhook",
  "notification_reason": "new alerts added",
  "groupLabels": {
    "alertname": "HighErrorRate",
    "cluster": "prod-eu"
  },
  "commonLabels": {
    "alertname": "HighErrorRate",
    "cluster": "prod-eu",
    "severity": "critical",
    "team": "payments"
  },
  "commonAnnotations": {
    "summary": "Error rate above 5% for 10m",
    "runbook_url": "https://runbooks.example.com/HighErrorRate"
  },
  "routeLabels": {},
  "externalURL": "https://alertmanager.example.com",
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "HighErrorRate",
        "cluster": "prod-eu",
        "severity": "critical",
        "team": "payments",
        "instance": "api-7f9c-2x4k:9100",
        "job": "api"
      },
      "annotations": {
        "summary": "Error rate above 5% for 10m",
        "description": "api-7f9c-2x4k error rate is 12.4%",
        "runbook_url": "https://runbooks.example.com/HighErrorRate"
      },
      "startsAt": "2026-08-07T09:14:22.114Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "generatorURL": "https://prometheus.example.com/graph?g0.expr=...&g0.tab=1",
      "fingerprint": "3f8c1a2b9d4e5f60"
    },
    {
      "status": "resolved",
      "labels": {
        "alertname": "HighErrorRate",
        "cluster": "prod-eu",
        "severity": "critical",
        "team": "payments",
        "instance": "api-3b1d-9p2m:9100",
        "job": "api"
      },
      "annotations": {
        "summary": "Error rate above 5% for 10m",
        "description": "api-3b1d-9p2m error rate is 0.2%",
        "runbook_url": "https://runbooks.example.com/HighErrorRate"
      },
      "startsAt": "2026-08-07T08:51:02.007Z",
      "endsAt": "2026-08-07T09:20:12.443Z",
      "generatorURL": "https://prometheus.example.com/graph?g0.expr=...&g0.tab=1",
      "fingerprint": "aa07c4e51b3d9f22"
    }
  ]
}
```

Notes on the literal shape:

- `version` is the **literal string `"4"`**, hardcoded. It has not changed in years and is not a useful feature-detection signal.
- `truncatedAlerts` is a **number** (`uint64`), despite the docs writing `<int>` in a string-looking position.
- `endsAt` for a firing alert is the **Go zero time** `"0001-01-01T00:00:00Z"` when unset, *not* `null` and not omitted. Guard for this.
- A single payload can mix `firing` and `resolved` alerts in `alerts[]`. The top-level `status` is `resolved` only when **all** alerts in the group are resolved.
- `groupLabels`, `commonLabels`, `commonAnnotations`, `routeLabels` are `KV` = `map[string]string`. All values are strings.

### `notification_reason` values

Added by *"[FEATURE] Add the reason for notifying in dedup stage. #4971"* in **0.32.0 (2026-04-08)**. The webhook docs omitted it until it was fixed in **0.32.3 / 0.33.1 (2026-07-04)** — *"[BUGFIX] doc: fix missing `notification_reason` field in webhook documentation (#5329)"*.

Exact strings ([notify/notify.go](https://github.com/prometheus/alertmanager/blob/main/notify/notify.go)):

| Constant | Wire string |
|---|---|
| `ReasonDoNotNotify` | `"none"` |
| `ReasonFirstNotification` | `"first notification"` |
| `ReasonNewAlertsInGroup` | `"new alerts added"` |
| `ReasonNewResolvedAlerts` | `"some alerts resolved"` |
| `ReasonAllAlertsResolved` | `"all alerts resolved"` |
| `ReasonRepeatIntervalElapsed` | `"repeat interval elapsed"` |
| `ReasonUnknown` | `"unknown"` |

> **This is a big deal for us.** `notification_reason` tells us *why* Alertmanager re-sent a group. `"repeat interval elapsed"` means "nothing changed, this is a nag" → we should **update the existing Slack message, not post a new one**. `"first notification"` → post new. `"new alerts added"` / `"some alerts resolved"` → update + optionally thread a delta. Only available on AM ≥ 0.32.0; treat empty string as unknown and fall back to comparing fingerprint sets.

### `max_alerts` / truncation

```go
func truncateAlerts(maxAlerts uint64, alerts []*types.Alert) ([]*types.Alert, uint64) {
	if maxAlerts != 0 && uint64(len(alerts)) > maxAlerts {
		return alerts[:maxAlerts], uint64(len(alerts)) - maxAlerts
	}
	return alerts, 0
}
```

Default `max_alerts: 0` = unlimited. Truncation takes the **first N in slice order**, which is not a meaningful priority order. If the operator sets `max_alerts`, `truncatedAlerts > 0` and we have permanently lost those alert bodies — we can only render "+N more". Recommend documenting that operators leave `max_alerts: 0` and let us paginate in Slack.

### Custom `payload` escape hatch

`webhook_config` supports a `payload:` map of Go templates that **replaces** the entire default body. The docs carry an explicit "USE AT YOUR OWN RISK … THE ALERTMANAGER TEAM WILL NOT PROVIDE ANY SUPPORT" warning. Our ingest should assume the standard shape but fail soft.

---

## A2. Alert fingerprint

### How it is computed

`Alert.Fingerprint` in the webhook payload is `model.LabelSet.Fingerprint()` from `prometheus/common`:

```go
// prometheus/common/model/signature.go
const SeparatorByte byte = 255

func labelSetToFingerprint(ls LabelSet) Fingerprint {
	if len(ls) == 0 {
		return Fingerprint(emptyLabelSignature)
	}
	labelNames := make(LabelNames, 0, len(ls))
	for labelName := range ls {
		labelNames = append(labelNames, labelName)
	}
	sort.Sort(labelNames)

	sum := hashNew()
	for _, labelName := range labelNames {
		sum = hashAdd(sum, string(labelName))
		sum = hashAddByte(sum, SeparatorByte)
		sum = hashAdd(sum, string(ls[labelName]))
		sum = hashAddByte(sum, SeparatorByte)
	}
	return Fingerprint(sum)
}
```

```go
// prometheus/common/model/fingerprinting.go
type Fingerprint uint64

func (f Fingerprint) String() string {
	return fmt.Sprintf("%016x", uint64(f))
}
```

Sources: [signature.go](https://github.com/prometheus/common/blob/main/model/signature.go), [fingerprinting.go](https://github.com/prometheus/common/blob/main/model/fingerprinting.go)

**Algorithm summary:**
1. Sort label names lexicographically.
2. For each: `hash(name)`, `hash(0xFF)`, `hash(value)`, `hash(0xFF)`.
3. Hash function is **FNV-1a 64-bit** (`hashNew`/`hashAdd`; the file comment states *"For our purposes, FNV-1A 64-bit is used"*).
4. Render as **exactly 16 lowercase hex chars**, zero-padded (`%016x`).

**It is a pure function of the full label set** — including `alertname`, `severity`, `instance`, and every other label. It is **fully reproducible in our own Go code** by importing `github.com/prometheus/common/model` and calling `LabelSet.Fingerprint().String()`. We do not need to trust the incoming value; we can recompute and verify.

### Is it always present?

- Added to template data by *"[CHANGE] Add Fingerprint to template data. #1945"* in **Alertmanager 0.19.0 (2019-09-03)** ([CHANGELOG](https://github.com/prometheus/alertmanager/blob/main/CHANGELOG.md)).
- Earlier, *"[ENHANCEMENT] Expose the alert's 'fingerprint' (unique identifier) through API (#786)"* exposed it on the API only.
- Since it is a non-pointer `string` field with no `omitempty`, on **≥ 0.19.0 the key is always present** in the JSON. On < 0.19.0 it is absent entirely.

**Design call:** require AM ≥ 0.19.0 (released 2019 — safe), but recompute the fingerprint ourselves from `labels` anyway so we are immune to the field being missing, and so it stays a *derived*, not *supplied*, key.

### Distinct from Alertmanager's internal dedup hash

Watch out: Alertmanager's **notification log** dedup uses a *different* hash on the *same* input:

```go
// notify/notify.go
func hashAlert(a *alert.Alert) uint64 {
	const sep = '\xff'
	// ... sort label names, append name, sep, value, sep ...
	hash := xxhash.Sum64(b)
	return hash
}
```

Same byte layout, but **xxhash64**, not FNV-1a, and it is never exposed on the wire. Do not confuse the two.

---

## A3. `groupKey` format and stability

```go
// dispatch/dispatch.go
func (ag *aggrGroup) GroupKey() string {
	return fmt.Sprintf("%s:%s", ag.routeKey, ag.labels)
}
```

Source: [dispatch.go](https://github.com/prometheus/alertmanager/blob/main/dispatch/dispatch.go)

- `routeKey` is `Route.Key()`, which walks the route tree from the root joining ancestor keys with `/`. Each segment is the route's matcher set.
- `ag.labels` is a `model.LabelSet`, whose `String()` renders as `{name="value", name2="value2"}` with **label names sorted**.

Concrete example:

```
{}/{severity="critical"}:{alertname="HighErrorRate", cluster="prod-eu"}
```

### Is it safe as a durable key? **No — use it as a hint, not a primary key.**

Reasons:

1. **It embeds the route path.** Any edit to `alertmanager.yml` that changes the matcher set of the matched route (or inserts/removes an ancestor route) changes `routeKey`, and therefore changes `groupKey` for alerts that are otherwise identical. Config reloads are routine.
2. **It embeds the rendered group label set**, so it changes if `group_by` changes.
3. **It is unbounded in length** — a deep route tree with many matchers plus a wide `group_by` produces long strings. Alertmanager itself hashes it for internal storage: `GroupId: notify.Key(ag.GroupKey()).Hash()` (dispatch.go:980), which is a hint that the raw string is not meant as a compact identity.
4. It is not escaped, so a label *value* containing `:` or `}` makes naive parsing wrong. Treat it as an opaque blob; do not parse it.

**Recommended durable key for oto:** a hash over `(receiver, sorted groupLabels)` — stable across route-config edits — with `groupKey` stored alongside as observability metadata. Fall back to the alert `fingerprint` set for per-alert identity. This is what lets us keep the same Slack thread across an Alertmanager config reload.

---

## A4. Alertmanager API v2

### Base path and spec

- Base path: **`/api/v2/`** (`basePath: /api/v2/` in the spec).
- OpenAPI spec (Swagger 2.0): **https://github.com/prometheus/alertmanager/blob/main/api/v2/openapi.yaml**
- Generated Go client is vendored in-tree at `api/v2/client` (go-swagger generated) — importable as `github.com/prometheus/alertmanager/api/v2/client`.

### Is there a v3? **No.**

The repo contains only `api/v2` (plus `api/metrics`, `api/status`). There is no `api/v3` directory and no v3 references in `api/api.go`. **v2 is current.**

### v1 is removed (not merely deprecated)

```go
// api/v1_deprecation_router.go
"The Alertmanager v1 API was deprecated in version 0.16.0 and is removed as of
 version 0.27.0 - please use the equivalent route in the v2 API"
// ... w.WriteHeader(410)
```

All v1 paths return **HTTP 410 Gone** with `{"status":"deprecated","error":"..."}` since **0.27.0**. Do not write any v1 code.

### Endpoint table

| Method | Path | operationId | Key query params |
|---|---|---|---|
| GET | `/api/v2/status` | `getStatus` | — |
| GET | `/api/v2/receivers` | `getReceivers` | — |
| GET | `/api/v2/alerts` | `getAlerts` | `active` (bool, default **true**), `silenced` (bool, default **true**), `inhibited` (bool, default **true**), `unprocessed` (bool, default **true**), `filter` (array of matcher strings, repeatable), `receiver` (string, regex) |
| POST | `/api/v2/alerts` | `postAlerts` | body: array of `postableAlert` |
| GET | `/api/v2/alerts/groups` | `getAlertGroups` | `active`, `silenced`, `inhibited`, **`muted`** (all bool, default true), `filter` (array), `receiver` (string) |
| GET | `/api/v2/silences` | `getSilences` | `filter` (array), **`active`** (bool, default true), **`expired`** (bool, default true), **`pending`** (bool, default true) |
| POST | `/api/v2/silences` | `postSilences` | body: `postableSilence` |
| GET | `/api/v2/silence/{silenceID}` | `getSilence` | — |
| DELETE | `/api/v2/silence/{silenceID}` | `deleteSilence` | — |

Source: [api/v2/openapi.yaml](https://github.com/prometheus/alertmanager/blob/main/api/v2/openapi.yaml)

> Note the singular/plural asymmetry: **`/silences`** (list, create) vs **`/silence/{id}`** (get, expire). Easy to get wrong.
> Note `active`/`expired`/`pending` on `GET /silences` are newer state filters (the older API only had `filter`). **UNVERIFIED**: exact release that added them.

### Key response models

`gettableAlert` = `alert` (`labels` required, `generatorURL`) plus, all **required**: `receivers[]`, `fingerprint`, `startsAt`, `updatedAt`, `endsAt`, `annotations`, `status`.

`alertStatus`:
```text
{
  "state": "unprocessed | active | suppressed",
  "silencedBy": ["<silenceID>", ...],
  "inhibitedBy": ["<fingerprint>", ...],
  "mutedBy": ["<time-interval-name>", ...]
}
```
All four fields are **required** (arrays are present but may be empty). Note the API has a single `suppressed` state; *why* it is suppressed is in the three arrays.

`alertGroup`: `{ labels, routeLabels, receiver, alerts[] }` — all required. Confirms `routeLabels` is a first-class concept in current versions.

`silence` (postable body): `matchers` (required), `startsAt`, `endsAt`, `createdBy`, `comment` all **required**; `annotations` optional (silence annotations were added in 0.32.0, *"[FEATURE] Add silence annotations. #4965"*).

`gettableSilence` adds required `id`, `status`, `updatedAt`. `silenceStatus.state` ∈ `expired | active | pending`.

`matcher`: `{ name, value, isRegex (required), isEqual (default true) }`. The `isRegex`/`isEqual` pair encodes all four operators: `=` (`isRegex:false,isEqual:true`), `!=` (`false,false`), `=~` (`true,true`), `!~` (`true,false`).

### Silence create / update / expire semantics

```go
// api/v2/api.go
func (api *API) deleteSilenceHandler(...) {
	sid := params.SilenceID.String()
	if err := api.silences.Expire(ctx, sid); err != nil { ... }
}
```

- **DELETE `/silence/{id}` does not delete — it *expires*.** It sets `endsAt` to now; the silence remains queryable with `state: "expired"`. Returns 200, or 404 if unknown.
- **POST `/silences` is create-or-update.** `postableSilence` = `silence` + optional `id`. Supplying an existing `id` updates that silence; omitting it creates a new one. Response is `{"silenceID": "<string>"}` — the operation is described in the spec as *"Create / update silence response"*.
- Server-side validation (400 on failure): `startsAt` must be strictly before `endsAt`; `endsAt` must not be in the past.
- POST can return **404** if the supplied `id` does not exist.

**Design call:** our "Silence" button should POST a new silence and persist the returned `silenceID` on the Slack message so the "Unsilence" button can DELETE it. Because expire is idempotent-ish and 404s on unknown IDs, treat 404 on unsilence as success.

### Multiple matcher sets

*"[FEATURE] Add support for multiple matcher set silences. #4957"* landed in **0.32.0**. **UNVERIFIED**: how this is represented in the v2 wire model (the `matchers` field in `openapi.yaml` is still a flat `matchers` array) — likely gated behind a feature flag or a newer spec revision. Verify against the target AM version before relying on it.

---

## A5. Resolve semantics — `resolve_timeout` / `EndsAt`

There are **two independent timeout mechanisms**. Confusing them is the classic bug.

### 1. Prometheus side: `EndsAt` is a lease, refreshed on every send

```go
// prometheus/rules/manager.go — SendAlerts
if !alert.ResolvedAt.IsZero() {
	a.EndsAt = alert.ResolvedAt
} else {
	a.EndsAt = alert.ValidUntil
}
```

```go
// prometheus/rules/alerting.go — sendAlerts
if alert.needsSending(ts, resendDelay) {
	alert.LastSentAt = ts
	// Allow for two Eval or Alertmanager send failures.
	delta := max(interval, resendDelay)
	alert.ValidUntil = ts.Add(4 * delta)
	...
}
```

Sources: [rules/manager.go](https://github.com/prometheus/prometheus/blob/main/rules/manager.go), [rules/alerting.go](https://github.com/prometheus/prometheus/blob/main/rules/alerting.go)

- `--rules.alert.resend-delay` default **`1m`** ([cmd/prometheus/main.go](https://github.com/prometheus/prometheus/blob/main/cmd/prometheus/main.go)).
- So for a **firing** alert, `EndsAt = now + 4 * max(group_evaluation_interval, 1m)` → **at least 4 minutes in the future**, refreshed roughly every minute.
- For a **genuinely resolved** alert, Prometheus sets `EndsAt = ResolvedAt` — a timestamp **in the past or present**.
- Prometheus keeps sending resolved alerts for a retention window after resolution, explicitly so the resolve is not lost:

```go
// rules/alerting.go
// 1. It allows for Prometheus to be more resilient to network issues that
//    would otherwise prevent a resolved alert from being reported as resolved
//    to Alertmanager.
// 2. It helps reduce the chance of resolved notifications being lost if
//    Alertmanager crashes or restarts between receiving the resolved alert
//    from Prometheus and sending the resolved notification.
if a.State == StatePending || (!a.ResolvedAt.IsZero() && ts.Sub(a.ResolvedAt) > resolvedRetention) {
	delete(r.active, fp)
}
```

### 2. Alertmanager side: `resolve_timeout` only applies when `EndsAt` is absent

```go
// api/v2/api.go — postAlertsHandler
resolveTimeout := time.Duration(api.alertmanagerConfig.Global.ResolveTimeout)
...
// If no end time is defined, set a timeout after which an alert
// is marked resolved if it is not updated.
if alert.EndsAt.IsZero() {
	alert.Timeout = true
	alert.EndsAt = now.Add(resolveTimeout)
}
```

`global.resolve_timeout` default is **5m** (`config/config.go`: `ResolveTimeout: model.Duration(5 * time.Minute)`).

> **Key insight:** when alerts come from **Prometheus**, `EndsAt` is always set, so `resolve_timeout` is **never used**. It only matters for alerts pushed directly to `POST /api/v2/alerts` by third parties. A lot of documentation gets this wrong.

### How do *we* tell "resolved" from "stale"?

The webhook gives us both signals:

| Signal | Meaning |
|---|---|
| alert `status == "resolved"` | Authoritative. Alertmanager decided this alert is resolved. |
| `endsAt` in the **past** | Consistent with resolved. |
| `endsAt` in the **future** (≈ now + 4m) | Still firing; this is the lease, not a prediction. |
| `endsAt == "0001-01-01T00:00:00Z"` | Zero value — no end time known. |
| top-level `status == "resolved"` | **All** alerts in the group are resolved. |
| `notification_reason == "all alerts resolved"` | Explicit resolve notification. |
| `notification_reason == "some alerts resolved"` | Partial resolve within the group. |

**Rule for oto:** trust per-alert `status`, never infer resolution from `endsAt` arithmetic. Separately run a **staleness sweeper**: if we have not seen a firing notification for a group in `> repeat_interval + slack`, mark it *stale/unknown* in the UI rather than *resolved* — because a silenced or inhibited alert also stops arriving (see A6), and so does a dead Alertmanager.

### `send_resolved`

`send_resolved` defaults to **`true`** for webhooks. If an operator sets it `false`, we will **never** see resolve notifications, and we must not leave Slack cards firing forever. Detect this by querying `GET /api/v2/status` (which returns the effective `config`) and warn loudly, or reconcile via `GET /api/v2/alerts`.

Notably, the retry stage still logs resolved alerts to the nflog even when `send_resolved: false`:

```go
// notify/retry_stage.go
// If we shouldn't send notifications for resolved alerts, but there are only
// resolved alerts, report them all as successfully notified (we still want the
// notification log to log them for the next run of DedupStage).
```

---

## A6. Inhibition and silence — what a webhook receiver sees

### **Suppressed alerts are never delivered to a webhook. At all.**

The notification pipeline runs `MuteStage` **before** `RetryStage`. `MuteStage` *drops* muted alerts from the slice that continues down the pipeline:

```go
// notify/mute.go
func (n *MuteStage) Exec(ctx context.Context, logger *slog.Logger, alerts ...*alert.Alert) (context.Context, []*alert.Alert, error) {
	var (
		filtered []*alert.Alert
		muted    []*alert.Alert
	)
	for _, a := range alerts {
		// Do not send the alert if muted.
		if n.muter.Mutes(ctx, a.Labels) {
			muted = append(muted, a)
		} else {
			filtered = append(filtered, a)
		}
	}
	...
	return ctx, filtered, nil   // <-- muted alerts are gone
}
```

Source: [notify/mute.go](https://github.com/prometheus/alertmanager/blob/main/notify/mute.go)

Four suppression reasons exist, all handled by the same stage:

```go
const (
	SuppressedReasonSilence            = "silence"
	SuppressedReasonInhibition         = "inhibition"
	SuppressedReasonMuteTimeInterval   = "mute_time_interval"
	SuppressedReasonActiveTimeInterval = "active_time_interval"
)
```

Consequences for oto — these are important:

1. **A silenced alert looks identical to a resolved-and-gone alert from the webhook's perspective: silence.** We get nothing. No "suppressed" status, no notification, no field. The webhook `alerts[].status` enum is **only** `firing | resolved` — there is no `suppressed` value on the wire.
2. If a whole group becomes silenced, we simply stop receiving notifications for it. We will *not* get a `status: resolved`.
3. Therefore, **if the user silences an alert from our Slack card, we must update the card ourselves** — Alertmanager will not tell us. Persist "we created silence X for group Y" locally.
4. To detect suppression we created ourselves *or* someone created in the Alertmanager UI, we must **poll `GET /api/v2/alerts?silenced=true&inhibited=true`** and read `status.state == "suppressed"` plus `status.silencedBy[] / inhibitedBy[] / mutedBy[]`. This is the *only* way to distinguish "silenced" from "resolved" from "Alertmanager is down".
5. Observability: the current metric is **`alertmanager_notifications_suppressed_total{reason=...}`** — verified in [notify/metrics.go](https://github.com/prometheus/alertmanager/blob/main/notify/metrics.go):

   ```go
   Namespace: "alertmanager",
   Name:      "notifications_suppressed_total",
   Help:      "The total number of notifications suppressed for being silenced, inhibited, outside of active time intervals or within muted time intervals.",
   ```

   Beware of stale docs and blog posts: it was introduced (#3565, 0.27.0 era) as **`alertmanager_alerts_supressed_total`** — different noun *and* a typo (one `p`). It has since been renamed. If our docs or dashboards reference this metric, use the current name and note the rename for operators on older versions.

> **Architecture consequence:** a webhook-only design cannot render silence state. oto needs the **API v2 reconciler loop** as a first-class component, not an optional extra.

---

## A7. Fetching the originating alert rule definition

Alertmanager does **not** know about rules. It only sees label sets. The rule definition lives in **Prometheus**, via `GET /api/v1/rules`.

### Endpoint

`GET <prometheus>/api/v1/rules`

Query parameters ([Prometheus HTTP API docs](https://prometheus.io/docs/prometheus/latest/querying/api/#rules)):

| Param | Type | Notes |
|---|---|---|
| `type` | `alert` \| `record` | Filter by rule type. Use `type=alert`. |
| `rule_name[]` | string, repeatable | Return rules with the given name(s). **This is our primary filter — pass `alertname`.** |
| `rule_group[]` | string, repeatable | Filter by group name. |
| `file[]` | string, repeatable | Filter by rule file path. |
| `exclude_alerts` | bool | Return rule definitions only, omitting active alert instances. **Use this — it dramatically shrinks the response.** |
| `match[]` | label selector, repeatable | Return rules whose labels match the selector. |
| `group_limit` | number | Enables pagination. |
| `group_next_token` | string | Continuation token. |

### Response shape

```go
// prometheus/web/api/v1/api.go
type AlertingRule struct {
	// State can be "pending", "firing", "inactive".
	State          string           `json:"state"`
	Name           string           `json:"name"`
	Query          string           `json:"query"`
	Duration       float64          `json:"duration"`        // the `for:` clause, in SECONDS
	KeepFiringFor  float64          `json:"keepFiringFor"`   // the `keep_firing_for:` clause, in SECONDS
	Labels         labels.Labels    `json:"labels"`
	Annotations    labels.Labels    `json:"annotations"`
	Alerts         []*Alert         `json:"alerts"`
	Health         rules.RuleHealth `json:"health"`
	LastError      string           `json:"lastError,omitempty"`
	EvaluationTime float64          `json:"evaluationTime"`
	LastEvaluation time.Time        `json:"lastEvaluation"`
	// Type of an alertingRule is always "alerting".
	Type string `json:"type"`
}

type Alert struct {
	Labels          labels.Labels `json:"labels"`
	Annotations     labels.Labels `json:"annotations"`
	State           string        `json:"state"`
	ActiveAt        *time.Time    `json:"activeAt,omitempty"`
	KeepFiringSince *time.Time    `json:"keepFiringSince,omitempty"`
	Value           string        `json:"value"`
}
```

Source: [web/api/v1/api.go](https://github.com/prometheus/prometheus/blob/main/web/api/v1/api.go)

Documented example ([prometheus.io](https://prometheus.io/docs/prometheus/latest/querying/api/#rules)):

```json
{
    "data": {
        "groups": [
            {
                "rules": [
                    {
                        "alerts": [
                            {
                                "activeAt": "2018-07-04T20:27:12.60602144+02:00",
                                "annotations": { "summary": "High request latency" },
                                "labels": {
                                    "alertname": "HighRequestLatency",
                                    "severity": "page"
                                },
                                "state": "firing",
                                "value": "1e+00"
                            }
                        ],
                        "annotations": { "summary": "High request latency" },
                        "duration": 600,
                        "health": "ok",
                        "labels": { "severity": "page" },
                        "name": "HighRequestLatency",
                        "query": "job:request_latency_seconds:mean5m{job=\"myjob\"} > 0.5",
                        "type": "alerting"
                    }
                ],
                "file": "/rules.yaml",
                "interval": 60,
                "limit": 0,
                "name": "example"
            }
        ]
    },
    "status": "success"
}
```

Note the doc example predates `keepFiringFor`; the Go struct is authoritative — `keepFiringFor` **is** returned (no `omitempty`), as `0` when unset.

### Matching an alert instance back to its rule — and the pitfalls

Naive approach: `alertname` label → `rule.name`. That mostly works, but:

**Pitfall 1 — `alertname` is not unique.** Nothing stops two rule groups, or two files, from defining `HighErrorRate`. `rule_name[]` can return several rules. Disambiguate by checking that the rule's **static `labels`** are a subset of the alert's labels, and that `annotations` keys match. Even then it can be ambiguous.

**Pitfall 2 — `alertname` can be overwritten.** An Alertmanager route or a Prometheus relabeling step can rewrite `alertname`. Also, `alert_relabel_configs` in `prometheus.yml` runs before alerts are sent to Alertmanager and can mutate any label, including `alertname`.

**Pitfall 3 — the rule's labels are templated.** Rule `labels:` values may contain `{{ $labels.x }}` templates. The `/api/v1/rules` response returns the **raw, untemplated** rule definition, but the alert instance carries the **rendered** values. So a naive "rule labels ⊆ alert labels" subset check fails whenever templating is used. Compare only on non-templated (no `{{`) label values.

**Pitfall 4 — `externalLabels` are added on the way out.** Prometheus adds `global.external_labels` to alerts sent to Alertmanager. Those labels will be on the alert but never on the rule. Subset checks must be one-directional: rule labels ⊆ alert labels, never the reverse.

**Pitfall 5 — multiple Prometheus servers.** In a federated / sharded setup, several Prometheis feed one Alertmanager. `/api/v1/rules` on the wrong server returns nothing. **Use `generatorURL` to identify the origin server**: it is `externalURL + "/graph?g0.expr=..."`, built in `SendAlerts` (`GeneratorURL: externalURL + strutil.TableLinkForExpression(expr)`). Parse its scheme+host to pick which Prometheus to query, and parse `g0.expr` to recover the PromQL expression **directly, with zero API calls**.

> **Best approach for oto (recommended):**
> 1. **Primary:** parse `generatorURL`. `g0.expr` is URL-encoded PromQL — decode it and you have the expression *as evaluated*, no rule lookup, no ambiguity, and it works even for multi-Prometheus setups. This is by far the most robust path for showing "the query".
> 2. **Enrichment:** for `for:` / `keep_firing_for:` / raw rule labels+annotations, call `GET /api/v1/rules?type=alert&rule_name[]=<alertname>&exclude_alerts=true` on the Prometheus identified in step 1, and cache aggressively (rule definitions change rarely; cache for minutes, invalidate on config reload).
> 3. Handle N>1 matches by scoring on non-templated label subset + annotation key overlap, and surface ambiguity rather than guessing.

**Pitfall 6 — `duration` and `keepFiringFor` are float seconds**, not Go duration strings and not milliseconds. `600` means `for: 10m`.

**Pitfall 7 — pagination is not consistent.** The docs state: *"there are no guarantees regarding the consistency of the response if the rule groups are being modified during the pagination process."* Prefer `rule_name[]` filtering over paging through everything.

---

## A8. Clustering — can the same alert be delivered more than once?

### **Yes. Alertmanager guarantees at-least-once, not exactly-once.**

### How dedup works

1. **Prometheus fans out to every Alertmanager.** The README is explicit:
   > *"Important: Do not load balance traffic between Prometheus and its Alertmanagers, but instead point Prometheus to a list of all Alertmanagers. The Alertmanager implementation expects all alerts to be sent to all Alertmanagers to ensure high availability."*

   So **every** peer independently has the alert and independently wants to notify.

2. **`ClusterWaitStage` staggers peers by position.** Each peer waits `position * --cluster.peer-timeout` (default **15s**) before notifying:

   ```go
   // notify/cluster_stages.go
   func (ws *ClusterWaitStage) Exec(...) {
       select {
       case <-time.After(ws.wait()):
       case <-ctx.Done():
           return ctx, nil, ctx.Err()
       }
       return ctx, alerts, nil
   }
   ```
   `waitFunc = clusterWait(peer, opts.PeerTimeout)` → peer 0 waits 0s, peer 1 waits 15s, peer 2 waits 30s.

3. **The notification log (nflog) is gossiped.** After peer 0 notifies, it records the notification and gossips that record. When peer 1's wait expires, `DedupStage` sees peer 0's nflog entry and suppresses.

4. **`ClusterGossipSettleStage`** blocks the pipeline until gossip has settled after startup (`--cluster.settle-timeout`, default = `--cluster.pushpull-interval` = **1m**), so a freshly-started peer does not re-notify everything.

Sources: [notify/cluster_stages.go](https://github.com/prometheus/alertmanager/blob/main/notify/cluster_stages.go), [app/app.go](https://github.com/prometheus/alertmanager/blob/main/app/app.go), [README.md](https://github.com/prometheus/alertmanager/blob/main/README.md)

### Why duplicates still happen

- **The dedup is time-based and gossip-based, both best-effort.** If peer 0's notification takes longer than 15s, or the gossip does not converge within 15s (network partition, GC pause, `--cluster.gossip-interval` default 200ms but pushpull is 1m), peer 1 will notify too.
- **A network partition guarantees duplicates by design** — that is the whole point of the HA model. It prefers duplicate notifications over lost notifications.
- **Retries produce duplicates.** If our endpoint returns 200 but the response is lost, or we time out after having processed the request, Alertmanager retries (see A9).
- **Restarts.** nflog is snapshotted to disk, but a crash between notifying and snapshotting loses the record.

### **Design mandate for oto: idempotent ingest.**

The webhook handler **must** be idempotent. Recommended dedup key:

```
sha256(groupKey_normalized || receiver || sorted(fingerprint:status for each alert) || notification_reason)
```

with a short-TTL (say 60s, ≥ `n_peers * peer_timeout` = 45s for a 3-node cluster) seen-set. Reject/no-op duplicates and still return 200. Do **not** dedupe purely on `groupKey` — legitimate repeat notifications share it.

Also: because the webhook can be retried, treat "post to Slack" as the non-idempotent step and guard it behind the dedup check, not the other way round.

---

## A9. HTTP response contract and retry behaviour

### What Alertmanager expects back

From [webhook.go](https://github.com/prometheus/alertmanager/blob/main/notify/webhook/webhook.go):

> *"Webhooks are assumed to respond with 2xx response codes on a successful request and 5xx response codes are assumed to be recoverable."*

The actual classifier ([notify/util.go](https://github.com/prometheus/alertmanager/blob/main/notify/util.go)):

```go
func (r *Retrier) Check(statusCode int, body io.Reader) (bool, error) {
	if statusCode/100 == 2 {
		return false, nil
	}
	retry := statusCode/100 == 5 || slices.Contains(r.RetryCodes, statusCode)
	s := fmt.Sprintf("unexpected status code %v", statusCode)
	...
	return retry, errors.New(s)
}
```

| Status | Behaviour |
|---|---|
| **2xx** | Success. Notification recorded in nflog. Body ignored. |
| **5xx** | Failure, **retried**. |
| **3xx** | Failure, **not retried** (unless followed by the HTTP client). |
| **4xx** | Failure, **not retried** — permanently dropped. |
| `RetryCodes` | Per-integration extra retriable codes. **The webhook integration sets none** — no default `RetryCodes` for webhooks, so 429 from *our* endpoint is **not** retried. |

> **Critical:** returning **429 or 4xx means the notification is silently and permanently lost.** If oto is overloaded or rate-limited by Slack, we must return **503**, not 429, to get a retry. Never return 4xx for a transient condition. Never return 2xx for a payload we failed to persist.
>
> Also: the response **body is ignored on 2xx**. There is no way to signal anything back to Alertmanager. Ack fast, process async — but only after durably persisting the payload, because a 2xx is a promise.

### Retry loop

```go
// notify/retry_stage.go
// backoff/v5's ExponentialBackOff never returns Stop from NextBackOff, so
// the ticker retries indefinitely until the context is canceled.
b := backoff.NewExponentialBackOff()
tick := backoff.NewTicker(b)
```

- Uses `github.com/cenkalti/backoff/v5` with **default** `ExponentialBackOff` settings (v5 defaults: initial interval 500ms, multiplier 1.5, randomization factor 0.5, max interval 60s — **UNVERIFIED** against the exact pinned v5 version in `go.mod`).
- **It retries indefinitely.** There is no max-attempts cap. The loop terminates only when the context is cancelled.

### The retry deadline (this is what actually bounds retries)

```go
// dispatch/dispatch.go — aggrGroup.run
ctx, cancel := context.WithTimeout(ag.ctx, ag.timeout(ag.opts.GroupInterval))
```

```go
// app/app.go
waitFunc := func() time.Duration { return 0 }
if peer != nil {
	waitFunc = clusterWait(peer, opts.PeerTimeout)
}
timeoutFunc := func(d time.Duration) time.Duration {
	if d < notify.MinTimeout {
		d = notify.MinTimeout
	}
	return d + waitFunc()
}
```

```go
// notify/notify.go
const MinTimeout = 10 * time.Second
```

**Total retry budget = `max(group_interval, 10s) + (peer_position × cluster.peer-timeout)`.**

With the common defaults (`group_interval: 5m`, single node): **5 minutes** of retries. On a 3-node cluster the third peer gets `5m + 30s`.

When the deadline expires the failure is recorded as `contextDeadlineExceeded` on `alertmanager_notifications_failed_total{reason=...}` and **the notification is dropped** — the group will be retried from scratch only at the next `group_interval` tick (and `DedupStage` may then suppress it, since nothing changed... unless `repeat_interval` has elapsed).

### Per-request timeout

`webhook_config.timeout`, default **`0s` = no timeout**.

> The docs warn: *"NOTE: This will have no effect if set higher than the group_interval."* — because the outer context deadline dominates.

**Design call:** oto's webhook handler must respond **well under 10 seconds** (the floor of the retry budget) even when `group_interval` is small. Target p99 < 1s: validate → dedup-check → durably enqueue → 200. All Slack I/O happens asynchronously.

### Failure-reason metric labels (useful for operators debugging oto)

`alertmanager_notifications_failed_total{reason=...}`. Values include `clientError`, `serverError`, `contextCanceled`, `contextDeadlineExceeded`, and — **new in `main` / unreleased at time of writing** — `authError` (HTTP 401/403) and `rateLimited` (HTTP 429) split out of the generic `clientError`:

> *"[CHANGE] notify: The `reason` label on `alertmanager_notifications_failed_total` now distinguishes `authError` (HTTP 401/403) and `rateLimited` (HTTP 429) from the generic `clientError`."* — CHANGELOG, `main / (unreleased)`

---

## A10. Route timing defaults (the cadence we must design around)

```go
// dispatch/route.go
GroupWait:         30 * time.Second,
GroupInterval:     5 * time.Minute,
RepeatInterval:    4 * time.Hour,
```

Source: [dispatch/route.go](https://github.com/prometheus/alertmanager/blob/main/dispatch/route.go)

| Setting | Default | Meaning for us |
|---|---|---|
| `group_wait` | **30s** | Delay before the *first* notification for a brand-new group. So there is an inherent ~30s floor on alert→Slack latency that we cannot improve. Set expectations accordingly. |
| `group_interval` | **5m** | Minimum gap before sending an update for a group that has *changed*. Our card can therefore go stale by up to 5 minutes. Also = our retry budget (§A9). |
| `repeat_interval` | **4h** | Gap before re-sending an *unchanged* group. This is what produces `notification_reason: "repeat interval elapsed"`. |

**Implication:** with defaults, an unacknowledged critical alert only re-pings every **4 hours**. That is far too slow for on-call escalation, and it is why every serious tool implements its own escalation timer rather than relying on `repeat_interval`. oto should track "unacked for > N minutes" itself and escalate (thread reply with `reply_broadcast`, or a page) on its own clock, independent of Alertmanager.

Also from the docs: *"If an alert is resolved before `group_wait` has elapsed, no notification will be sent"* — so **short-lived flapping alerts are invisible to us entirely**. We cannot report on them, and should not claim to.

---

## A11. Bonus: Grafana Alerting sends a compatible superset

Many teams run **Grafana Unified Alerting** instead of (or alongside) Alertmanager. Its webhook contact point emits the **same `version: "4"` envelope** with the standard fields (`receiver`, `status`, `alerts`, `groupLabels`, `commonLabels`, `commonAnnotations`, `externalURL`, `version`, `groupKey`, `truncatedAlerts`, per-alert `fingerprint`), **plus** Grafana-specific extras:

| Extra field | Scope | Use |
|---|---|---|
| `orgId` | top-level | Grafana org — useful as a tenant discriminator |
| `title`, `message` | top-level | template-rendered strings |
| `state` | top-level | `"alerting"` or `"ok"` — **note: different vocabulary from Alertmanager's `firing`/`resolved`** |
| `silenceURL` | per-alert | direct deep link to silence the rule — **we can render this as a button with zero API work** |
| `dashboardURL` | per-alert | present when the Dashboard UID annotation exists |
| `panelURL` | per-alert | present when the Panel ID annotation exists |
| `imageURL` | per-alert | **screenshot of the triggering panel** — a Block Kit `image` block straight into the card |

Source: [Grafana webhook notifier docs](https://grafana.com/docs/grafana/latest/alerting/configure-notifications/manage-contact-points/integrations/webhook-notifier/)

**UNVERIFIED**: whether `valueString` / `values` are present in the current schema (they appear in older Grafana docs).

**Design call:** decode into a struct with the Alertmanager fields required and the Grafana fields optional. Costs almost nothing and roughly doubles the addressable install base. `imageURL` in particular is a *large* visual win that pure-Alertmanager users cannot get. Map `state: "alerting"|"ok"` onto our internal firing/resolved.

---

## A-Summary: what this means for oto's architecture

| Constraint | Consequence |
|---|---|
| Webhook must answer 2xx in < 10s | Ack-and-enqueue. Never do Slack I/O inline. |
| 4xx/429 = permanent loss; only 5xx retried | Return **503** for backpressure. Never 429, never 4xx for transient errors. |
| Retries are unbounded but capped by `max(group_interval,10s) + peer_wait` | ~5 min budget by default. |
| At-least-once delivery (HA gossip + retries) | Idempotent ingest with a dedup key + ~60s TTL is mandatory. |
| Suppressed alerts are dropped before the webhook | Silence/inhibit state is **invisible** via webhook. Requires an API v2 reconciler. |
| `groupKey` embeds route config | Not a durable key. Derive our own from (receiver, groupLabels). |
| `fingerprint` = FNV-1a 64 over sorted labels, 16 hex | Recomputable locally; use as per-alert identity. |
| `notification_reason` (AM ≥ 0.32.0) | Drives post-vs-update decision in Slack. |
| `resolve_timeout` is irrelevant for Prometheus-sourced alerts | Trust `status`, not `endsAt`. |
| API v1 returns 410 since 0.27.0; no v3 exists | Target v2 only. |
| Rule lookup is ambiguous by `alertname` | Prefer parsing `generatorURL`'s `g0.expr`. |

---

# B. Slack

## B1. Block Kit — block types, limits, and severity color

### Block count per surface

> *"You can include up to **50 blocks** in each message, and **100 blocks** in modals or Home tabs."*
> — [Block Kit overview](https://docs.slack.dev/block-kit/)

### Current block type inventory (21 types)

Source: [Blocks reference](https://docs.slack.dev/reference/block-kit/blocks/)

| Block | Description (verbatim) |
|---|---|
| Actions | "Holds multiple interactive elements." |
| **Alert** | "Displays alerts, warnings, and informational messages." |
| Card | "Displays content in a card." |
| Carousel | "Displays related card blocks in a horizontally-scrolling container." |
| Container | "A general-purpose wrapper for grouping child blocks together, with a configurable size." |
| Context | "Provides contextual info, which can include both images and text." |
| Context actions | "Displays actions as contextual info, which can include both feedback buttons and icon buttons." |
| Data table | "Displays rich tables that support pagination, sorting, filtering, and interactivity." |
| Data visualization | "Displays data visually in pie, bar, area, or line chart formats." |
| Divider | "Visually separates pieces of info inside of a message." |
| File | "Displays info about remote files." |
| Header | "Displays a larger-sized text." |
| Image | "Displays an image." |
| Input | "Collects information from users via elements." |
| Markdown | "Displays formatted markdown." |
| Plan | "Displays a collection of related tasks." |
| Rich text | "Displays formatted, structured representation of text." |
| Section | "Displays text, possibly alongside elements." |
| Table | "Displays structured information in a table." |
| Task card | "Displays a single task, representing a single action." |
| Video | "Displays an embedded video player." |

> ### ⚠️ TRAP: the `alert` block is **not** for our alerts.
> Despite the name, the Alert block is **modals-only**:
> > *"Alert blocks are currently only supported in modals."*
> > — [Alert block reference](https://docs.slack.dev/reference/block-kit/blocks/alert-block/)
>
> Its fields: `type: "alert"`, `text` (plain_text or mrkdwn, **max 200 chars**), `level` ∈ `default | info | warning | error | success`, `block_id`.
>
> It is genuinely useful for the **"Confirm silence"** or **"Escalate"** modal we open from a button, but it **cannot** be used in the channel alert card. Do not design around it.

### Assessment of the newer block types for an alert card

| Block | Verdict | Evidence |
|---|---|---|
| **Alert** | ❌ Unusable in messages | *"Alert blocks are currently only supported in modals."* `text` max 200 chars. |
| **Card** | ⚠️ Too small | Fields: `hero_image`, `icon`/`slack_icon`, `title` (150), `subtitle` (150), **`body` max 200 chars**, `subtext` (200), **`actions` max 3 buttons**. *"At least one of `hero_image`, `title`, `actions`, or `body` is required."* A 200-char body cannot hold an alert description. Possible fit for a compact multi-alert digest, not the main card. — [card-block](https://docs.slack.dev/reference/block-kit/blocks/card-block/) |
| **Context actions** | ➖ Wrong purpose | Holds *"feedback buttons and icon buttons"*, max 5 items. Aimed at 👍/👎 feedback affordances, not alert actions. — [context-actions-block](https://docs.slack.dev/reference/block-kit/blocks/context-actions-block/) |
| **Table** | ✅ Viable for label dumps | Max **100 rows**, **20 cells/row**, **20 column settings**, **10,000 chars across all cells**. Docs describe apps that *"programmatically publish messages that include a table."* Good for an expanded "all labels" or "all instances" view. — [table-block](https://docs.slack.dev/reference/block-kit/blocks/table-block/) |
| Data table / Data visualization / Carousel / Container / Plan / Task card | ⚠️ **UNVERIFIED** surface support | The blocks index does not state per-surface availability, and several of these are clearly aimed at newer AI/agent surfaces. Verify each individually before use. |

**Recommendation: build the v1 card from the classic, universally-supported set** — `header`, `section` (with `fields` and `accessory`), `context`, `actions`, `divider`, `rich_text` — wrapped in a color attachment. That combination is guaranteed to render everywhere including mobile and older clients. Treat `table` as a v2 enhancement for the expanded view.

### Character and item limits

| Block / element | Limit | Source |
|---|---|---|
| `section.text` | **3,000 chars** | [section-block](https://docs.slack.dev/reference/block-kit/blocks/section-block/) |
| `section.fields` | **max 10 items, 2,000 chars each** | same |
| `block_id` (all blocks) | **255 chars** | same |
| `header.text` | **150 chars**, must be `plain_text` | [header-block](https://docs.slack.dev/reference/block-kit/blocks/header-block/) |
| `context.elements` | **max 10 items** | [context-block](https://docs.slack.dev/reference/block-kit/blocks/context-block/) |
| `context` per-text char limit | not documented — **UNVERIFIED** | same |
| `actions.elements` | **max 25 elements** | [actions-block](https://docs.slack.dev/reference/block-kit/blocks/actions-block/) |
| `button.text` | **75 chars** (docs warn display "may truncate with ~30 characters") | [button-element](https://docs.slack.dev/reference/block-kit/block-elements/button-element/) |
| `button.url` | **3,000 chars** | same |
| `button.value` | **2,000 chars** | same |
| `action_id` | **255 chars** | same |
| `button.style` | `primary` (green), `danger` (red), or omitted | same |
| `image.image_url` | **3,000 chars** | [image-block](https://docs.slack.dev/reference/block-kit/blocks/image-block/) |
| `image.alt_text` | **2,000 chars** | same |
| `image.title` | **2,000 chars**, `plain_text` | same |
| `markdown` block | **12,000 chars cumulative across all markdown blocks in one payload**; `block_id` is not retained | [markdown-block](https://docs.slack.dev/reference/block-kit/blocks/markdown-block/) |
| `table` block | **max 100 rows**, **max 20 cells per row**, **max 20 column settings**, **10,000 chars across all cells** | [table-block](https://docs.slack.dev/reference/block-kit/blocks/table-block/) |

The section-block docs also note: *"`block_id` should be unique for each message and each iteration of a message. If a message is updated, use a new `block_id`."* — relevant to our update flow (we re-render cards on state change).

### Attachments and the severity color bar

**Status: legacy, but NOT deprecated, and still the only way to get a color bar.**

Slack's own page is now filed under `/legacy/`:

> *"This feature is a **legacy part of messaging functionality** for Slack apps. We recommend you stick with layout blocks."*
> — [Legacy secondary message attachments](https://docs.slack.dev/legacy/legacy-messaging/legacy-secondary-message-attachments/)

And the migration guide warns content in attachments *"may be wrapped, truncated, or hidden behind a 'show more' option by Slack clients"* and that attachments *"may change in the future in ways that reduce their visibility or utility."*

**But** — the decisive quote:

> *"The **color parameter currently does not have a block alternative**, so if you want to retain the color bar, you can use the `blocks` parameter within an attachment."*
> — [Attachments to blocks migration guide](https://api.slack.com/messaging/attachments-to-blocks)

### ✅ Recommended 2025/2026 pattern for severity color

```text
{
  "channel": "C0123456789",
  "text": "[CRITICAL] HighErrorRate firing on prod-eu",
  "attachments": [
    {
      "color": "#D93025",
      "blocks": [ /* ... the entire Block Kit card ... */ ]
    }
  ]
}
```

- `color` accepts `good` (green), `warning` (yellow), `danger` (red), or a hex like `#439FE0`.
- Exactly **one** attachment wrapping **all** blocks. The legacy docs say *"never ever (ever!) send more than 20 attachments"*; `chat.postMessage` hard-caps at **100** (`too_many_attachments`).
- **No 2025/2026 sunset for attachments was found.** **UNVERIFIED**: any announced removal date — none exists as of research date.

**Alternative if you want to avoid attachments entirely:** convey severity with a leading emoji in the `header` (🔴 / 🟠 / 🟡 / 🟢) plus a `context` line. This is what several tools do (see B8). The pragmatic answer is **do both** — emoji for scannability, color bar for the peripheral-vision cue.

**Proposed severity palette for oto:**

| Severity | Emoji | Hex |
|---|---|---|
| critical | 🔴 | `#D93025` |
| warning | 🟠 | `#E8710A` |
| info | 🔵 | `#1A73E8` |
| resolved | ✅ | `#188038` |
| acknowledged | 👀 | `#F9AB00` |
| silenced | 🔇 | `#80868B` |

### Sizing our card against the limits

A realistic alert card is ~10–15 blocks. The **50-block ceiling is the real constraint when a group contains many alerts.** With ~3 blocks per alert instance, we can render ~12 instances before hitting the wall. Plan: render the top N (default 5–10), then a `+ N more` button that opens a modal (100 blocks) or posts a thread reply. Also watch `msg_blocks_too_long` / `invalid_blocks` errors.

---

## B2. `chat.postMessage` threading

Source: [chat.postMessage](https://docs.slack.dev/reference/methods/chat.postMessage/)

- **`thread_ts`**: *"Provide a `thread_ts` value for the posted message to act as a reply to a parent message."* Critically: *"**Avoid using a reply's `ts` value; use its parent instead.**"*
- **`reply_broadcast`** (bool, default `false`): with `thread_ts`, *"indicates whether reply should be made visible to everyone in the channel or conversation."* Slack advises using it **sparingly**. It produces a message with subtype [`thread_broadcast`](https://docs.slack.dev/reference/events/message/thread_broadcast/).
- **Response**: `ok`, `channel` (the channel ID actually posted to — may differ from what you passed if you passed a name), `ts`, and `message` (the full server-parsed message object).

### Persisting and reusing the parent `ts`

- **`ts` is only unique per channel.** The durable handle is the pair **`(channel_id, ts)`** — always store both. Use the `channel` value **returned by the API**, not the one you sent.
- `ts` is a string like `"1548261231.000200"`. **Store it as a string, never as a float** — the fractional part is a 6-digit sequence counter, and float round-tripping will corrupt it. This is the single most common Slack integration bug.
- **Distinguishing parent from reply**: a message is a **parent** when `thread_ts == ts`; a **reply** when they differ. Presence of `thread_ts` at all means it is in a thread. — [Retrieving messages](https://docs.slack.dev/messaging/retrieving-messages/)
- Store `(alert_group_key → channel_id, parent_ts, last_rendered_hash)` in our DB, keyed by our own durable group key (see A3), not Alertmanager's `groupKey`.

### What if the parent is deleted?

- Slack documents the delete side: when a message with threaded replies is deleted, *"those replies will be left in the conversation, with a placeholder informing viewers about the deleted message."* And *"a parent message object will retain a `thread_ts` value, even if all its replies have been deleted."* — [Modifying messages](https://docs.slack.dev/messaging/modifying-messages/)
- **What `chat.postMessage` does with a `thread_ts` pointing at a deleted parent is UNVERIFIED** — Slack does not document it, and notably **`thread_not_found` is NOT in the `chat.postMessage` error table**. Threading-related errors that *are* listed: `cannot_reply_to_message`, `restricted_action_thread_locked`.
- **Design call:** treat any non-`ok` response to a threaded post as "parent is gone." Clear the stored `parent_ts`, post a fresh top-level card, and store the new `ts`. Same for `chat.update` returning `message_not_found`.

---

## B3. `chat.update` semantics and limits

Source: [chat.update](https://docs.slack.dev/reference/methods/chat.update/)

**Required args:** `token`, `channel` (channel **ID**; DM IDs start with `D`), `ts`.
**Updatable:** `text`, `blocks`, `attachments`, `markdown_text`, `metadata`, `file_ids`, `link_names`, `parse`, `reply_broadcast`.

### ⚠️ Content-replacement footguns

- Passing `text` **without** `blocks` **deletes the previous blocks.**
- To keep blocks while updating, **omit `text`.**
- Omit `attachments` to retain the previous attachments — **but** since our card lives *inside* an attachment (for the color bar), a state change from firing→resolved means resending the whole `attachments` array with the new `color`. **The color bar can be updated.**

### What cannot be updated

- Only messages posted by the authenticated user / app. Bot users must set `as_user: true` when editing their own messages.
- **Ephemeral messages cannot be updated at all.**
- **You cannot move a message** — `channel` identifies the target, it is not a new destination. — [Modifying messages](https://docs.slack.dev/messaging/modifying-messages/)

### Is there an age limit? Effectively no — but plan for `edit_window_closed`

- **No API-level time limit on editing is documented** anywhere in the method reference or the Modifying Messages guide.
- The error `edit_window_closed` exists and means *"The message cannot be edited due to the **team message edit settings**"* — i.e. a **workspace admin setting**, not a platform-wide TTL.
- **UNVERIFIED** in the strict sense: whether bot messages are updatable literally indefinitely. The documented constraint is workspace edit settings, not message age.
- **Design call:** we can update an alert card for the whole lifetime of an incident (hours/days). But handle `edit_window_closed` and `message_not_found` as terminal: fall back to posting a new message (optionally threaded under the old one) and re-point our stored `ts`.

### Relevant error codes

`message_not_found`, `cant_update_message`, `edit_window_closed`, `channel_not_found`, `invalid_blocks`, `invalid_blocks_format`, `block_mismatch`, `msg_too_long`, `no_text`, `update_failed`, `too_many_attachments`, `metadata_too_large`, `markdown_text_conflict`, `is_inactive`, `ratelimited`, `token_expired`, `token_revoked`, `missing_scope`, `no_permission`, `as_user_not_supported`, `message_limit_exceeded`.

---

## B4. Rate limits

Source: [Web API rate limits](https://docs.slack.dev/apis/web-api/rate-limits/)

Rate limits are applied **per method, per app, per workspace**.

### Tier definitions

| Tier | Limit | Notes |
|---|---|---|
| Tier 1 | **1+ per minute** | minimal burst tolerance |
| Tier 2 | **20+ per minute** | occasional bursts |
| Tier 3 | **50+ per minute** | typical for paginating collection methods |
| Tier 4 | **100+ per minute** | generous burst behaviour |
| Special | varies | e.g. `chat.postMessage` |

### Per-method tiers

| Method | Tier | Notes |
|---|---|---|
| **`chat.postMessage`** | **Special** | *"generally allows posting **one message per second per channel**"*, plus a workspace-wide limit of *"several hundred messages per minute"* with generous burst behaviour |
| **`chat.update`** | **Tier 3 (50+/min)** | |
| `conversations.history` | Tier 3 (50+/min) | `limit` default 100, max 999 |
| `conversations.replies` | Tier 3 (50+/min) | |
| `conversations.list` | Tier 2 (20+/min) | `limit` < 1000; Slack recommends ≤ 200/page |

> **The binding constraint for oto is `chat.postMessage`: ~1 msg/sec/channel.** An alert storm that fans out to one channel will queue. Design: a **per-channel token-bucket** at 1 msg/s, plus coalescing — prefer `chat.update` on an existing card (Tier 3, 50/min, and not per-channel-limited) over posting a new one. This is an independent, strong argument for the update-in-place card model.

### The May 2025 non-Marketplace rate limit change

Sources: [changelog 2025-05-29](https://docs.slack.dev/changelog/2025/05/29/rate-limit-changes-for-non-marketplace-apps/), [clarification 2025-06-03](https://docs.slack.dev/changelog/2025/06/03/rate-limits-clarity/)

**Affected methods: ONLY `conversations.history` and `conversations.replies`.**

New limits for affected apps:
- **1 request per minute** (Tier 3 → effectively Tier 1)
- **`limit` default AND max both reduced to 15 objects**

**Who is affected:** *"commercially distributed apps that are **not approved for the Slack Marketplace**"* — i.e. "unlisted" apps distributed outside the Marketplace.

**Who is NOT affected — the key line for a self-hosted internal tool:**

> *"Any **internal customer-built apps** will maintain their existing rate limits and will not be subject to the new posted limits."*
> — [clarification 2025-06-03](https://docs.slack.dev/changelog/2025/06/03/rate-limits-clarity/)

Marketplace-approved apps are also unaffected.

**Dates:** May 29, 2025 — new limits effective immediately for newly-created non-Marketplace apps and new installs of existing unlisted apps. June 30, 2025 — updated API ToS for apps created before May 29, 2025. A grace period for **existing installations** of unlisted apps was reportedly set to Sept 2, 2025 and later extended to Mar 3, 2026 — **PARTIALLY VERIFIED**; neither date appears in the current changelog page text (they come from FAQ search summaries). Both dates are now in the past, so assume enforcement is live.

**Consequence for oto:**
- Deployed as an **internal, single-workspace, non-distributed app → exempt.** Keep Tier 3.
- If oto is ever **sold/distributed to customers outside the Marketplace**, `conversations.history`/`replies` collapse to 1 req/min @ 15 objects.
- **Architectural mandate either way: never depend on reading Slack to reconstruct our own state.** Our DB is the source of truth for `(channel, ts, thread_ts, card state)`. Reading threads back must be, at most, a diagnostic nicety.

### Rate limit response

- HTTP **429 Too Many Requests**
- **`Retry-After` header** with seconds to wait (e.g. `Retry-After: 30`)
- JSON error `ratelimited`

Also note: **Events API delivery is capped at 30,000 deliveries per workspace per app per 60 minutes** ([Events API](https://docs.slack.dev/apis/events-api/)).

---

## B5. Interactivity

### `block_actions` payload

Source: [block_actions payload](https://docs.slack.dev/reference/interaction-payloads/block_actions-payload/)

Top-level properties: `type`, `trigger_id`, `user`, `team`, `container`, `api_app_id`, `actions`, `token`, `hash`, `function_data`, `interactivity`, `bot_access_token`, `enterprise`, `channel`, `message`, `view`, `state`, `response_url`.

Verbatim example from the docs:

```json
{
    "type": "block_actions",
    "team": { "id": "T9TK3CUKW", "domain": "example" },
    "user": { "id": "UA8RXUSPL", "username": "jtorrance", "team_id": "T9TK3CUKW" },
    "api_app_id": "AABA1ABCD",
    "token": "9s8d9as89d8as9d8as989",
    "container": {
        "type": "message_attachment",
        "message_ts": "1548261231.000200",
        "attachment_id": 1,
        "channel_id": "CBR2V3XEX",
        "is_ephemeral": false,
        "is_app_unfurl": false
    },
    "trigger_id": "12321423423.333649436676.d8c1bb837935619ccad0f624c448ffb3",
    "channel": { "id": "CBR2V3XEX", "name": "review-updates" },
    "message": {
        "bot_id": "BAH5CA16Z", "type": "message",
        "text": "This content can't be displayed.",
        "user": "UAJ2RU415", "ts": "1548261231.000200"
    },
    "response_url": "https://hooks.slack.com/actions/AABA1ABCD/1232321423432/D09sSasdasdAS9091209",
    "actions": [
        {
            "action_id": "WaXA",
            "block_id": "=qXel",
            "text": { "type": "plain_text", "text": "View", "emoji": true },
            "value": "click_me_123",
            "type": "button",
            "action_ts": "1548426417.840180"
        }
    ]
}
```

Key mechanics:
- The payload arrives **form-encoded** as a single `payload` parameter — you must `ParseForm()` then JSON-decode `payload`, **but you need the raw body first for signature verification** (see below). Read the body once into a buffer.
- The `token` field is **deprecated** — never use it for auth. Use signing secrets.
- `message.ts` / `container.message_ts` is what you feed to `chat.update`.
- `actions[].value` (2,000 chars) carries our per-alert state. Put an **opaque ID**, not a serialized blob — look it up in our DB. Never trust `value` as authoritative.

### The 3-second acknowledgement

> *"your app must reply to the HTTP POST request with an HTTP 200 OK response. This must be sent **within 3 seconds** of receiving the payload."*
> — [Handling user interaction](https://docs.slack.dev/interactivity/handling-user-interaction/)

In Go: verify signature → write 200 → hand off to a worker goroutine/queue. Everything else is async.

Related: **`trigger_id` expires in 3 seconds and may only be used once** — so `views.open` must happen synchronously in the handler, before the ack, or immediately after.

### `response_url` vs `chat.update`

| | `response_url` | `chat.update` |
|---|---|---|
| Lifetime | **30 minutes, max 5 uses** | unlimited |
| Permissions | *"bypasses channel posting permissions"* — works even without channel scopes | needs `chat:write` in the channel |
| Ephemeral messages | **only** way to update them | cannot touch them |
| Payload options | `response_type` (`in_channel` / ephemeral), `replace_original`, `delete_original` (must be sole attribute), `thread_ts` | full message rewrite |

**Design call:** use `response_url` with `replace_original: true` for the **immediate optimistic UI response** to a button click (fast, permission-free, within the 30-min window). Use `chat.update` for **all subsequent long-lived state changes** (a resolve arriving 6 hours later). Do not build the long-lived path on `response_url` — the 30-min/5-use cap will bite.

### Request signature verification

Source: [Verifying requests from Slack](https://docs.slack.dev/authentication/verifying-requests-from-slack/)

1. Read headers **`X-Slack-Signature`** and **`X-Slack-Request-Timestamp`** (case-insensitive).
2. **Reject if the timestamp is more than 5 minutes old** (replay protection).
3. Build basestring: **`v0:<timestamp>:<raw_request_body>`** — colon-joined, using the **raw, unparsed** body bytes.
4. HMAC-SHA256 over the basestring, keyed by the **signing secret**; hex-encode; prefix **`v0=`**.
5. Compare with a **constant-time** compare — *"use an hmac `compare` function instead of directly comparing the signatures for equality."* In Go: `hmac.Equal`.

```go
// Sketch — read the body ONCE, verify, then re-parse.
body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
ts := r.Header.Get("X-Slack-Request-Timestamp")
// reject if |now - ts| > 5*time.Minute
mac := hmac.New(sha256.New, []byte(signingSecret))
fmt.Fprintf(mac, "v0:%s:%s", ts, body)
expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Slack-Signature"))) { /* 401 */ }
```

`slack-go` provides `slack.NewSecretsVerifier` which implements this.

---

## B6. Socket Mode vs HTTP Events API

Sources: [HTTP vs Socket Mode](https://docs.slack.dev/apis/events-api/comparing-http-socket-mode/), [Using Socket Mode](https://docs.slack.dev/apis/events-api/using-socket-mode/), [Events API](https://docs.slack.dev/apis/events-api/)

**Slack's own recommendation:**
> *"we recommend using **HTTP for production applications**."*
> *"we recommend using Socket Mode when developing your app and using it locally. Once deployed and published for use in a team setting, we recommend using HTTP request URLs."*

| | Socket Mode | HTTP Events API |
|---|---|---|
| Transport | WebSocket, bidirectional, realtime | Request/response |
| Public endpoint | **Not required** | **Required** (public HTTPS Request URL) |
| Concurrency | **max 10 concurrent WebSocket connections per app** | scales horizontally |
| Scaling | stateful → *"more difficult to scale"* | *"scales well horizontally"* |
| Reliability | long-lived connections inherently less reliable | short-lived connections *"inherently more reliable"* |
| Slack Marketplace | ❌ *"Apps using Socket Mode are **not currently allowed** in the public Slack Marketplace."* HTTP is *"a **requirement**"* for Marketplace submission | ✅ required |
| Signature verification | **not needed** — *"you're receiving the events over a pre-authenticated WebSocket"* | required |
| Ack mechanism | ack each event with its **`envelope_id`** over the socket | HTTP 200 within 3s |
| Firewall / on-prem | ✅ ideal — *"if you're building an on-premise integration, have no ability to receive external HTTP requests"* | ❌ blocked by corporate firewalls |

Socket Mode apps **can** still be distributed org-wide via enterprise deployment options — just not the public Marketplace. The 10-connection allowance is deliberate: graceful restarts, load balancing under high event throughput, temporary redundancy — which gives us **zero-downtime deploys** of a Go service.

**HTTP retry behaviour (relevant to idempotency):** Slack retries failed deliveries **up to 3 times with exponential backoff** — nearly immediate, then after 1 minute, then after 5 minutes — with `x-slack-retry-num` and `x-slack-retry-reason` headers. If **>95% of deliveries fail in a 60-minute window** (SSL errors, >3s responses, >2 redirects, non-2xx), Slack **temporarily disables your event subscriptions** and emails the app owner.

### Recommendation for oto

**Support both; default to Socket Mode for self-hosted.**

- The overwhelming majority of self-hosted installs sit behind a firewall with no inbound HTTPS. Socket Mode is the difference between "works in 5 minutes" and "please ask your network team for an ingress rule + TLS cert." That onboarding delta dominates.
- Socket Mode also removes an entire class of bugs: no public endpoint to secure, no signature verification path, no Slack-side auto-disable when we deploy.
- Offer HTTP mode as a config flag for installs that already have ingress, want horizontal scale beyond 10 connections, or intend to list on the Marketplace.
- **Build the interaction handler transport-agnostically** — one `func handleBlockActions(payload slack.InteractionCallback) error`, with two thin adapters. `slack-go` makes this straightforward since both paths yield the same `InteractionCallback` type.
- Note the ack semantics differ: Socket Mode requires acking by `envelope_id`; HTTP requires a 200 within 3s. Both are "ack fast, work async."


## B7. Go SDK: `github.com/slack-go/slack`

### Verdict: actively maintained, and the only realistic choice.

### There is no official Slack Go SDK, and no `bolt-go`

- Slack's [tools page](https://docs.slack.dev/tools/) lists **Bolt for Java, JavaScript, and Python**, and SDKs for **Deno, Java, Node, Python**. **No Go.**
- The `slackapi` GitHub org contains `node-slack-sdk`, `python-slack-sdk`, `java-slack-sdk`, `bolt-js`, `bolt-python`, `hubot-slack` — **no Go repo at all**.
- `bolt-go` **does not exist**. Confirmed against both the tools page and the org listing.
- Slack's [community tools page](https://docs.slack.dev/tools/community-tools) does not list `slack-go` either — so it is unofficial *and* unlisted.
- `nlopes/slack` (the original) was **archived 2025-06-28**; `slack-go/slack` is the continuation.

### Maintenance health (verified against the repo and GitHub API on 2026-08-07)

| Metric | Value |
|---|---|
| Latest release | **v0.27.0, 2026-06-27** |
| Last push / HEAD commit | **2026-08-03** |
| Stars | ~4,954 |
| Open issues | **3** (see caveat) |
| Commits, last 26 weeks | **123** (last 52 weeks: 213) |
| Archived | No |
| License | BSD-2-Clause |
| Imported by | 3,000+ packages (pkg.go.dev) |

> **Caveat on "3 open issues":** the repo runs `actions/stale` with `days-before-stale: 45` / `days-before-close: 10` for issues *and* PRs. The low count reflects aggressive auto-closing, not an absence of gaps.

### Why it revived — worth knowing

Norberto Lopes created the library in 2015, stepped away for years, and **returned to maintenance in January 2025** — explicitly because he joined **incident.io**, which uses the library, and found it struggling. Source: [nlopes.dev](https://nlopes.dev/writing/back-to-maintaining-slack-go).

### ⚠️ Release cadence and stability risk

- 2025: v0.16.0 (Feb) → v0.17.0 (May) → v0.17.3 (Jul 4) → **~6.5 months with no releases**
- 2026: v0.18.0 (Feb 21) → v0.19.0 → v0.20.0 → v0.21.x → v0.22.0 → v0.23.x → v0.24.0 → v0.25.0 → v0.26.0 → **v0.27.0 (Jun 27)**

**Ten minor versions in ~4 months of 2026.** The README warns:

> *"There is currently no major version released. Therefore, minor version releases may include backward incompatible changes."*

v0.21.0's notes cite "multiple breaking changes" including cursor-based pagination changes and API removals.

> ### 🔒 Security floor: use **≥ v0.23.1**
> v0.23.1 (2026-05-10) is a **security patch**: `NewSecretsVerifier` now rejects **empty signing secrets** *"to avoid accepting forged request signatures when applications are misconfigured."* The release calls this an intentional breaking change *"so that there are no footguns."* An earlier version with an accidentally-empty `SLACK_SIGNING_SECRET` env var would **accept forged requests**. — [v0.23.1 release](https://github.com/slack-go/slack/releases/tag/v0.23.1)

**Policy for oto:** pin an exact version in `go.mod`, floor at v0.23.1, and read the changelog on every bump. Do not use `@latest` in CI.

### Feature coverage — everything we need is present

| Need | Coverage |
|---|---|
| **Block Kit** | `block.go` defines **21** `MessageBlockType` constants: `section, divider, image, actions, context, context_actions, file, input, header, rich_text, call, video, markdown, table, data_table, data_visualization, task_card, plan, alert, card, carousel`. Confirmed by the file listing: `block_alert.go`, `block_card.go`, `block_carousel.go`, `block_context_actions.go`, `block_data_table.go`, `block_data_visualization.go`, `block_plan.go`, `block_table.go`, `block_task_card.go` all exist. **It tracks Slack's newest blocks closely.** |
| **Socket Mode** | `slack-go/slack/socketmode` — `Client`, `New()`, `Run()`/`RunContext()`, `Ack()`/`AckCtx()`, `Open()`, plus the higher-level `SocketmodeHandler` with `Handle(EventType, fn)` / `RunEventLoop()`. Event types: `EventTypeEventsAPI`, `EventTypeInteractive`, `EventTypeSlashCommand`, `EventTypeConnecting/Connected/Disconnect`, `EventTypeInvalidAuth`. README calls `SocketmodeHandler` "experimental" but recommends Socket Mode over RTM. |
| **Interactions** | `InteractionCallback` with `ActionCallback` (`BlockActions` + `AttachmentActions`), `ResponseURL`, `Message`, `Container` (carries `message_ts`, `thread_ts`, `channel_id`, `is_ephemeral`), `TriggerID`, `View`. 11 `InteractionType` constants. |
| **Signature verification** | `NewSecretsVerifier(header, secret)` + `Write(body)` + `Ensure()` in `security.go`. HMAC-SHA256 over `v0:timestamp:body`, constant-time `hmac.Equal`, rejects timestamps > 5 min old, returns `ErrInvalidConfiguration` on empty secret. **Exactly implements the documented spec — do not hand-roll this.** |
| **chat.update / threading** | `UpdateMessage(channelID, timestamp string, ...MsgOption)` + `UpdateMessageContext`; `PostMessage`, `PostEphemeral`, `ScheduleMessage`, `DeleteMessage`. Options: `MsgOptionBlocks`, `MsgOptionAttachments`, `MsgOptionTS` (threading), `MsgOptionBroadcast`, `MsgOptionUpdate`, `MsgOptionReplaceOriginal`, `MsgOptionResponseURL`, `MsgOptionMetadata`. |
| **Color bar** | `Attachment` has both `Color string` and `Blocks Blocks` (`json:"blocks,omitempty"`) — the attachment-wrapping-blocks pattern is directly supported. |
| **Reactions** | `AddReaction(name, ItemRef)`, `RemoveReaction`, `GetReactions`, `ListReactions` (+ `Context` variants) — useful for lightweight state signalling. |
| **Rate limiting** | `OptionRetry()` enables automatic retry **for 429 only** (*"5xx and connection errors are not retried"*); `OptionRetryConfig()` for broader strategies. 429s surface as `*RateLimitedError{RetryAfter time.Duration}` with `Retryable() bool`. |

### Known gaps (none blocking)

1. **`container` block is missing** — [issue #1575](https://github.com/slack-go/slack/issues/1575) (2026-07-17). Slack shipped `container` on 2026-06-29: a collapsible wrapper with title/subtitle/icon, max 10 child blocks, width `narrow|standard|wide|full`, and `default_collapsed`. **This is the one missing block genuinely relevant to alert cards** — a collapsed "all labels" section would be ideal. Workaround: thread reply or modal.
2. **[#1571](https://github.com/slack-go/slack/issues/1571)** (2026-07-10) — `PostMessage`/`SendMessage` discard the `chat.postMessage` response's `message` object, so **you cannot read back `thread_ts`** and detect a silently un-threaded reply. Relevant to our deleted-parent handling (§B2): we cannot cheaply confirm the reply actually threaded.
3. **[#1577](https://github.com/slack-go/slack/issues/1577)** (2026-07-25) — `action_token` parsed from `assistant_thread` but Slack sends it top-level; silent parse failure. Not relevant to us.

**Bottom line: use `slack-go/slack`, pin ≥ v0.23.1, expect breaking changes on minor bumps, and budget for periodic dependency maintenance.**

---

## B8. How best-in-class tools build their Slack alert cards

Two of the reference tools are open source, so the values below are **exact**, not inferred.

### Grafana OnCall — the best open reference; read this one

Source: [`slack_renderer.py`](https://github.com/grafana/oncall/blob/dev/engine/apps/alerts/incident_appearance/renderers/slack_renderer.py), [`alertmanager.py`](https://github.com/grafana/oncall/blob/dev/engine/config_integrations/alertmanager.py)

**Architecture: Block Kit blocks for content; attachments used purely as colored containers for state + buttons.**

Layout:
1. `section`/`mrkdwn` — title
2. `section`/`mrkdwn` — body, truncated with `"... Message has been trimmed. Check the whole content in Web"`
3. If >1 alert in the group, a `context` block: `:package: Showing the last alert only out of {alerts_count} total. Visit <{web_link}|the plugin page> to see them all.`

Header format:
```
*<{grafana_oncall_link}|#{alert_group_id} {web_title}>* via {integration_name} (*<{source_link}|source>*)
```

Body template — the concrete field pattern to copy:
```
Severity: {{ severity }} {{ severity_emoji }}     # :rotating_light: critical, :warning: warning, :question: other
Status:   {{ status }} {{ status_emoji }}         # :fire: firing, :white_check_mark: resolved
Firing alerts – {{ payload.numFiring }}
Resolved alerts – {{ payload.numResolved }}
<{{ annotations.runbook_url }}|:book: Runbook:link:>
<{{ annotations.runbook_url_internal }}|:closed_book: Runbook (internal):link:>
GroupLabels: / CommonLabels: / Annotations:   (as "- k: v" bullets)
```

> ### 🎨 **Attachment color encodes alert-group STATE, not severity.** Exact hex:
> ```
> firing       #a30200
> silenced     #dddddd
> acknowledged #daa038
> resolved     #2eb886
> ```
> This is the best-tested palette in the open-source landscape. **Adopt it.** Severity goes in the emoji; state goes in the color bar. That split is the key insight — it means the color always answers "do I need to act?", which is the question a human scanning a channel is actually asking.

**Button state machine** (single `actions` block, elements swap by state):

| State | Buttons |
|---|---|
| Firing, unacked | `Acknowledge`, `Resolve`, `Silence` (static_select with duration options) / `Unsilence`, `Responders`, `Attach to …`, `Add Resolution notes` / `Resolution notes [n]`, `:fire: Declare incident` (URL), `:blue_book: Show Timeline` |
| Acknowledged | `:fire: Declare incident`, `Unacknowledge`, `Resolve`, `Silence`/`Unsilence`, `Responders`, `Attach to …`, resolution notes, `Show Timeline` |
| Resolved | `Unresolve`, resolution notes, `Show Timeline` |

Two details worth stealing:
- The resolution-notes button is `style: "primary"` and its **label carries a count** — `Add Resolution notes` at 0, `Resolution notes [3]` otherwise.
- Button `value` is a JSON blob `{organization_id, alert_group_ppk, …}` so the handler needs no `ts` → state lookup.
- **Silence is a `static_select` with duration options**, not a fixed-duration button. Much better UX than our draft's "Silence 1h".

State text is appended as its **own attachment above the buttons**: `"Resolved by @x"` / `"Acknowledged by @x"`.

**Update model: `chat.update` in place** ([tasks.py](https://github.com/grafana/oncall/blob/dev/engine/apps/slack/tasks.py)):
```python
slack_client.chat_update(channel=..., ts=slack_message.slack_id,
                         attachments=alert_group.render_slack_attachments(),
                         blocks=alert_group.render_slack_blocks())
```
wrapped in `SlackAPIRatelimitError` handling. Threads are used **only** for notes/follow-ups via `chat_postMessage(thread_ts=...)`.

### Grafana Alerting's own Slack contact point — the anti-pattern we exist to fix

Source: [grafana/alerting `slack.go`](https://github.com/grafana/alerting/blob/main/receivers/slack/v1/slack.go)

**Attachments only, no blocks**, with an in-code TODO admitting it:
```go
// TODO: We should use the Block Kit API instead
Attachments: []attachment{{ Color: ..., Title: title, Fallback: title,
    Footer: footer, FooterIcon: ..., Ts: now, TitleLink: ruleURL,
    Text: ..., Fields: nil /* TODO. Should be a config. */ }},
```
`Fields` is hard-coded `nil` — **no fields grid at all**. The docs confirm: *"you cannot modify its visual appearance with custom blocks."*

Colors: firing `#D63232`, resolved `#36a64f`. Header: `[FIRING:2] HighLatency checkout production` — no emoji. **Zero buttons.** Silence/Dashboard/Panel/Source appear as bare URLs in the body text. **Resolution posts a new message rather than editing.**

### Alertmanager's own Slack integration — also legacy, also buttonless

Defaults ([default.tmpl](https://github.com/prometheus/alertmanager/blob/main/template/default.tmpl), [slack_config](https://prometheus.io/docs/alerting/latest/configuration/#slack_config)):
```
slack.default.color:     {{ if eq .Status "firing" }}danger{{ else }}good{{ end }}
slack.default.title:     [{{ .Status | toUpper }}{{ if eq .Status "firing" }}:{{ .Alerts.Firing | len }}{{ end }}] {{ .GroupLabels.SortedPairs.Values | join " " }} (...)
slack.default.titlelink: {{ .ExternalURL }}/#/alerts?receiver={{ .Receiver | urlquery }}
mrkdwn_in:      ["fallback", "pretext", "text"]
send_resolved:  false     # note: FALSE by default for slack_config, unlike webhook_config
update_message: false
```

> **Block Kit support has been an open feature request since 2020** — [alertmanager#2217](https://github.com/prometheus/alertmanager/issues/2217), still open, 24 comments. This is the market gap oto fills.

Community best-practice templates converge on four link buttons — **Runbook 📗, Query 🔍, Dashboard 📈, Silence 🔕** — with colors firing+critical=`danger`, firing+warning=`warning`, firing+other=`#439FE0`, resolved=`good`; severity emoji `:fire:` critical / `:warning:` warning. The Silence button is a deep link:
```
{{ .ExternalURL }}/#/silences/new?filter=%7B...url-encoded matchers...%7D
```
Sources: [Monzo's templates](https://gist.github.com/milesbxf/e2744fc90e9c41b47aa47925f8ff6512), [hodovi.cc](https://hodovi.cc/blog/creating-awesome-alertmanager-templates-for-slack/).

> **Cheap win:** we can render a "Silence" **URL button** using that deep link with zero API calls and zero state — a great fallback when the user has not configured API credentials for us.

### Sentry — pure blocks, no color bar, emoji-only severity

Exact `LEVEL_TO_COLOR` ([types.py](https://github.com/getsentry/sentry/blob/master/src/sentry/integrations/messaging/types.py)):
```
_actioned_issue    #EDEEEF   (grey — after someone acts)
_incident_resolved #4DC771
debug   #FBE14F     error #E03E2F     fatal #FA4747
info    #2788CE     warning #FFC227
```
Exact `LEVEL_TO_EMOJI` ([types.py](https://github.com/getsentry/sentry/blob/master/src/sentry/integrations/slack/message_builder/types.py)):
```
_incident_resolved :green_circle:
debug :bug:    error :red_circle:    fatal :red_circle:
info :large_blue_circle:             warning :large_yellow_circle:
ACTION_EMOJI = :white_circle:   # replaces the level emoji once actioned
```

> **Key finding: Sentry issue alerts set no attachment color at all.** Severity is conveyed **purely by the leading emoji**. Only *metric alert incidents* set a color. So the "no attachments" path is viable at the highest tier of the market.

Block order: title `section` → culprit `context` → description `section` (triple-backtick quoted) → tags `section` → group-context `context` → action text → `actions` → notes → footer `context` → chart image.

> **Title is a `section`, NOT a `header` block:** `:red_circle: *<link|TypeError: bar>*`
>
> Rationale: `header` is `plain_text` only — **no bold, no links**. Using a `section` buys the deep link back. This is a better choice than a `header` block for an alert card, and it is what Sentry, OnCall, and Alertmanager all do.

Fallback `text` is `[project-slug] Title: description`.

Context field labels: `Events`, `Users Affected`, `State`, `First Seen`, `Approx. Start Time` — rendered `Key: *value*` with **three-space** separators. Footer uses **four-space** separators: `Project: <url|slug>    Alert: <url|Rule Name>    Short ID: BAR-1`.

> **Noise-reduction trick worth copying:** if `State == "New"` or `event_count <= 1`, Sentry **deletes** the Events and Users Affected fields entirely, and skips the backend query for the user count. **Don't render a count of 1.** Generalize: suppress any field whose value carries no information.

Buttons — **max 3, state-dependent**: Unresolved → `Resolve` · `Archive` · `Select Assignee…` (an `external_select`, not a button); Resolved → `Unresolve` · `Archive` · assignee; Ignored → `Resolve` · `Mark as Ongoing` · assignee.

Updates: **in place** via `replace_original: True` on the `response_url`; the emoji flips to `:white_circle:` and an "Issue resolved by @user" block is appended. Errors go back **ephemeral**. Metric alerts thread: *"a follow-up alert appearing as a threaded reply for future status changes."*

State routing: `block_id` carries JSON — `{"issue": 123, "rule": 1}`.

**A complete literal Block Kit payload is published in Sentry's test suite**: [`build_test_message_blocks`](https://github.com/getsentry/sentry/blob/master/tests/sentry/integrations/slack/test_message_builder.py).

### PagerDuty — the two-tier button model

Cards *"continuously update as the incident progresses"* — in-place editing.

**4 visible buttons: `Acknowledge`, `Resolve`, `Join Dedicated Channel`, `Run a Workflow`. 13 more behind a "More actions…" dropdown**: Add a Note, Add Responder, View Details, Add Task, View Tasks, Assign Roles, Change Priority, Reassign, Escalate, Create Channel, New Status Update, Change Type, Run an Action.

Admin-tunable knobs that amount to a noise-control API — **we should ship equivalents**:
- **Thread incident updates** — *"Toggle on to add all updates to a thread and reduce clutter"*
- **Verbosity presets**: Highlights Only (default for incident channels) / Status Updates Only (default for notification channels) / No Updates / All Updates / Custom
- **Display emojis in incident notifications** — *"display emojis next to fields"*, switchable off
- **Hide Create/Join Incident Channel Button**
- Channel naming: `#inc-{{incident.incident_number}}`

Fields include Priority (P1–P5) and Urgency (high/low). Exact color hex: **UNVERIFIED** — PagerDuty publishes no Block Kit JSON. Sources: [Slack user guide](https://support.pagerduty.com/main/docs/slack-user-guide), [integration guide](https://support.pagerduty.com/main/docs/slack-integration-guide).

### Opsgenie — clearest published color-bar convention

> **"Green means closed, red means open, and blue means acknowledged."**
> — [Atlassian docs](https://support.atlassian.com/opsgenie/docs/integrate-opsgenie-with-slack-app/)

Colors update dynamically, and related alerts recolor too. Buttons: Acknowledge, Unacknowledge, Close, Snooze, Assign, Take Ownership, Add Note — and *"buttons are changed accordingly"* by state. Exact hex: **UNVERIFIED**.

### incident.io

Heavily Block Kit — *"over 70 different Slack modals and almost as many messages"* — but publishes **no JSON**.

Alert message actions: **Declare incident / Join incident, Go to source, Silence, Resolve, Go to dashboard** ([docs](https://docs.incident.io/alerts/slack-channels)).

> **Notable: there is no `Acknowledge` on alert messages.** Ack belongs to *escalations*, and an "escalate to next level" button appears **only after** you have acknowledged. This is a deliberate separation of "the alert" from "the human response to the alert" — worth considering for our data model.

Messages are template-driven under Settings → Slack Messages: customizable **Title** (with dynamic status that auto-updates), Description toggle, and **Attributes — *"choose which alert attributes to display and in what order"*, with per-attribute custom emoji** ([message customization](https://docs.incident.io/alerts/message-customization)).

With grouping enabled: *"the first alert posts as a regular message and subsequent alerts turn it into a group message summarizing the number of alerts and their status"* — edit-in-place with a count. Color bar / hex / default attribute list: **UNVERIFIED**.

### Rootly — status emoji, no color bar, and a genuinely novel idea

Status emoji in the message, **not** a color bar: **🔴 Started, 🟠 Mitigated, 🟢 Resolved, 🔀 In triage** (hex **UNVERIFIED**).

> **Steal this:** *"When the status or severity of an incident are updated, the previous state will be shown in **strikethrough** text (e.g. ~Started~ → Mitigated)."*
> — [Rootly changelog](https://rootly.com/changelog/redesigned-incident-announcement-update-slack-blocks)
>
> This solves a real problem with edit-in-place: a reader who saw the card an hour ago cannot tell what changed. Strikethrough-the-old-value gives you the history *inside* the current render, for free, with no extra blocks.

Announcement fields: title, Slack channel, severity, environment, status emoji. Buttons: Update Summary, Manage Incident Roles, Update Incident, Leave Feedback, Manage Action Items, Add PagerDuty Responders, Add Opsgenie Responders.

Update model is **both**: the announcement block auto-updates in place, *and* update blocks are posted into its thread. Workflow message options are explicitly `Pin to Channel` / `Send as Ephemeral` / `Thread under parent` / `Update Parent Message` / `Broadcast Thread Reply`.

### FireHydrant — the cleanest published field list

> **Severity, Title, Started, Resolved, Opened By, Resolved By, Assigned Roles and People, links (channel / meeting bridge / ticket), buttons to Command Center and Internal Status Page** — and *"The message will also automatically update in place when these details update."*
> — [Runbook step: notify Slack channel](https://docs.firehydrant.com/docs/runbook-step-notify-slack-channel)

Custom messages support mrkdwn + template vars with a **3,000-character limit** (matching the `section` limit). Signals alerts arrive as **DMs** with acknowledge / escalate / dismiss / open-incident buttons; **escalations are added as threaded replies** to the alert message. Also has emoji-reaction quick-declare: react with a configured emoji and the bot replies with a button to open an incident.

### Datadog — the outlier: essentially one button

Per-channel content toggles: Message, Snapshot (graph image), Related tags, Recipient handles, Monitor actions. Terraform: `message`, `snapshot`, `tags`, `notified`, `mute_buttons`.

**The only action is Mute.** No ack/resolve/assign on monitor alerts.

Titles prefix state: `Triggered:` / `Recovered:` (third-party verified, not officially documented). Priorities P1–P5, overridable inline with `{{override_priority 'Pi'}}`. Conditional body sections: `{{#is_alert}}`, `{{#is_warning}}`, `{{#is_recovery}}`, `{{#is_no_data}}`, `{{#is_alert_to_warning}}`. Data vars: `{{value}}`, `{{threshold}}`, `{{first_triggered_at}}`, `{{triggered_duration_sec}}`.

> **Datadog's prescribed message body is the best published alert-copy template found** ([notification message best practices](https://docs.datadoghq.com/monitors/guide/notification-message-best-practices/)):
> 1. **What's happening** — value + threshold
> 2. **Impact** — customer-facing consequence
> 3. **Why** — potential root causes
> 4. **How to troubleshoot** — runbook link
> 5. **Related links**
>
> And: the title should state *"the failure mode(s) or the diverging metrics [and] what resource is affected."*
>
> **oto should ship this as the default annotation template we recommend to users**, and lint alerts that lack a `runbook_url`.

Datadog Incident Management puts **severity and status in the channel title bar**, not repeated per message.

---

## B8b. Published guidance on alert message design

### Slack's own design docs

[App design concepts](https://docs.slack.dev/concepts/app-design/):
- DO *"allow the installer to easily set preferences"* for message rate, channel, and notification types.
- DON'T use `@channel`/`@here`/`@everyone` without explicit permission — **exception: critical outages needing immediate action.**
- DO offer **digest options** instead of per-event alerts.
- DO segment message types into relevant channels; DON'T default to `#general`.
- DO focus on **the action the receiver most likely wants to take**.

[Designing with Block Kit](https://docs.slack.dev/concepts/designing-with-block-kit/):
- *"Visually rich messages… take up a lot of space on a person's screen."*
- **"Use more than just color to convey meaning."** Target 4.5:1 contrast; test light and dark mode.
- *"Always pair emojis with text"*, place them at sentence ends, and **"Do not use an emoji as a control."**
- *"Context blocks are great for storing helpful information that isn't primary content."*
- *"Give buttons brief, repetitive labels to avoid truncation."*
- Use **an overflow menu to store lesser-used options.**
- After the flow completes, **condense the message and remove buttons.**

> ### ♿ Accessibility — the most important single fact in this section
> **Screen readers read the top-level `text` field and NOT the content of interior blocks.**
> So either put everything a user needs into `text`, or omit `text` entirely when using blocks.
>
> Combined with `text` also being the **push notification**, the **sidebar preview**, and the **search result snippet**, this makes `text` the highest-leverage string in the whole product. It deserves as much design attention as the blocks.

[Messaging](https://docs.slack.dev/messaging/): *"Ephemeral messages should only ever be sent in response to some user action, they must never be unexpected or unsolicited."* → **never deliver an alert ephemerally**; use ephemeral only for button errors/confirmations.

### incident.io — SRE alerting best practices

Source: [incident.io blog](https://incident.io/blog/sre-alerting-best-practices)

- **The filter test:** *"if the on-call engineer cannot take a specific action to resolve it, the alert should not exist."*
- Teams receive *"over 2,000 alerts weekly"* with *"around 3% needing immediate action."*
- **Benchmark: below 20% alert-to-actionable-incident conversion = noise problem; target 30–50%.** — a metric oto could actually compute and surface.
- Audit each alert: real/imminent user impact? runbook or clear action? ignored more than twice in 30 days?
- *"A database going down fires alerts for every dependent service within seconds. Without deduplication, your on-call engineer wakes up to 40 pages from one incident."*
- P0 = *"Production down, revenue impacted, or customer data at risk."*

### Knock — Slack notification best practices

Sources: [best practices](https://knock.app/manuals/slack-notifications/best-practices-for-slack-notifications), [design guide](https://knock.app/blog/the-guide-to-designing-slack-notifications)

- **Batch**: instead of three "Server N restarted" messages → *"3 servers restarted: server-1, server-2, server-3."*
- **Thread**: parent per event, replies for updates — *"The channel will see one message, and users who want details can expand the thread."* Use `reply_broadcast: true` for urgent updates. **Start a fresh parent when a thread exceeds 20–30 replies.**
- *"A great Slack message uses its header block the same way a great email uses its subject line."*
- Button color: only default/`primary`/`danger` — *"save green and red for clear confirm/accept or cancel/reject use cases."* Use emoji in default-style buttons to differentiate navigation actions.
- Dividers separate items in digests; *"if you're just sending single block messages with text, dividers are probably overkill."*

---

## B8c. Cross-tool synthesis — where the good ones converge

1. **One leading emoji encodes severity, paired with text.** Never color alone — Slack's own accessibility guidance requires it, and colorblind users need it.
2. **The color bar is optional and forces an attachment wrapper.** Accept the legacy/truncation risk, or skip it and lead with emoji as Sentry does.
3. **Color encodes STATE, not severity**, in every tool that uses it well. OnCall's palette is the best-tested.
4. **Title as a `section` with a bold link, not a `header` block** — headers are `plain_text` only, so a header costs you the deep link.
5. **3–4 visible buttons, everything else in an overflow.** PagerDuty: 4 visible / 13 hidden. Sentry: 3.
6. **Buttons are state-dependent.** Resolve↔Unresolve, Ack↔Unack, Silence↔Unsilence. Never render a no-op action.
7. **Edit in place with `chat.update` for state changes**; thread only for a *stream* of ongoing updates (notes, escalations, screenshots). Make it a per-channel toggle, as PagerDuty does.
8. **Metadata in `context` blocks**, `Key: *value*` with wide separators — not sections.
9. **Suppress low-information fields** (Sentry drops counts at ≤1).
10. **Runbook is a first-class link/button**, not buried in annotation text — exactly where Grafana Alerting and Alertmanager fail.
11. **Encode routing state in `block_id` / `value` as JSON** so handlers need no `ts` → state lookup. (Ours should be an **opaque ID**, not a payload — see §B5.)
12. **Always set a meaningful top-level `text`.**
13. **Show a group count when collapsing**: OnCall's `context` line and incident.io's group summary both show "N total" and render only the latest.


---

## B9. Concrete Block Kit alert card for oto

This design is derived from the constraints in §B1–B5 and the cross-tool synthesis in §B8c. Every choice below is traceable to a verified fact.

### Design decisions and their justification

| Decision | Justification |
|---|---|
| **Title is a `section` with a bold link — NOT a `header` block** | `header` is `plain_text` only: no bold, no links (§B1). Sentry, OnCall, and Alertmanager all use a section. A header costs us the deep link for no gain. |
| **Severity → leading emoji. State → attachment `color`.** | Slack: *"Use more than just color to convey meaning."* OnCall's proven split. The color then always answers "do I need to act?" |
| **Wrap all blocks in ONE attachment with `color`** | Only way to get the bar; *"the color parameter currently does not have a block alternative"* (§B1). Accept the legacy risk. |
| **Top-level `text` is written deliberately** | It is the push notification, sidebar preview, search snippet, **and the only thing screen readers read** (§B8b). Highest-leverage string in the product. |
| **3 primary buttons + an `overflow` menu** | Slack: *"use an overflow menu to store lesser-used options"* and *"give buttons brief, repetitive labels to avoid truncation."* PagerDuty 4/13, Sentry 3. |
| **Buttons are state-dependent; never render a no-op** | Universal across OnCall / Opsgenie / Sentry (§B8c #6). |
| **Silence is a `static_select` with durations, not a fixed button** | Copied from OnCall. Avoids an extra modal round-trip for the common case. |
| **`chat.update` in place; thread only for streams of updates** | `chat.update` is Tier 3 (50/min); `chat.postMessage` is ~1/sec/channel (§B4). Updating is both nicer *and* cheaper. |
| **Strikethrough the previous state on change** | Rootly's trick — gives change history inside the current render at zero block cost. |
| **Suppress zero-information fields** | Sentry drops counts ≤ 1. Don't render "1 instance" or an empty team. |
| **`<!date^…>` for all timestamps** | Renders in each viewer's timezone; `{ago}` stays correct without re-rendering. |
| **`unfurl_links: false`, `unfurl_media: false`** | Runbook/Prometheus links would otherwise generate unfurl cards that bury the alert. |
| **`value` carries an opaque ID, not a payload** | OnCall/Sentry stuff JSON in there; we should not — `value` is user-visible and un-authenticated (§B5). Look state up in our DB. |

### State palette (adopted from Grafana OnCall, verified hex)

| State | Emoji | `color` |
|---|---|---|
| Firing | 🔴 (critical) / 🟠 (warning) / 🔵 (info) | `#a30200` |
| Acknowledged | 👀 | `#daa038` |
| Silenced | 🔇 | `#dddddd` |
| Resolved | ✅ | `#2eb886` |

Severity emoji follows OnCall/Sentry: `:rotating_light:` or `:red_circle:` critical, `:warning:` / `:large_yellow_circle:` warning, `:large_blue_circle:` info.

### Block budget

Title `section` 1 + body `section` 1 + fields `section` 1 + instances `section` 1 + query `context` 1 + `actions` 1 + footer `context` 1 = **7 blocks**.
Well under the 50-block cap, leaving room for optional per-instance rows. Render **10** instances inline at most; beyond that, a "Show all N" button opening a modal (100-block budget).

### Firing card — literal JSON

```json
{
  "channel": "C0123456789",
  "text": ":rotating_light: [FIRING] HighErrorRate — 3 of 12 api instances in prod-eu above 5% errors. Severity critical, team payments, firing since 09:14 UTC. Runbook: https://runbooks.example.com/HighErrorRate",
  "unfurl_links": false,
  "unfurl_media": false,
  "attachments": [
    {
      "color": "#a30200",
      "fallback": "[FIRING] HighErrorRate on prod-eu",
      "blocks": [
        {
          "type": "section",
          "text": {
            "type": "mrkdwn",
            "text": ":rotating_light: *<https://oto.example.com/g/01J9XQ2K7M3T|HighErrorRate>*  ·  `prod-eu`\n_Error rate above 5% for 10m_"
          }
        },
        {
          "type": "section",
          "text": {
            "type": "mrkdwn",
            "text": "3 of 12 `api` instances are returning >5% of requests as 5xx. Checkout and payment confirmation are affected."
          }
        },
        {
          "type": "section",
          "fields": [
            { "type": "mrkdwn", "text": "*Status*\n:fire: Firing" },
            { "type": "mrkdwn", "text": "*Severity*\n:rotating_light: critical" },
            { "type": "mrkdwn", "text": "*Service*\n`api`" },
            { "type": "mrkdwn", "text": "*Team*\n<!subteam^SAZ94GDB8>" },
            { "type": "mrkdwn", "text": "*Started*\n<!date^1786439662^{time}|09:14 UTC>" },
            { "type": "mrkdwn", "text": "*Firing for*\n21m" }
          ]
        },
        {
          "type": "section",
          "text": {
            "type": "mrkdwn",
            "text": "*Affected instances*\n• `api-7f9c-2x4k` — 12.4%\n• `api-3b1d-9p2m` — 8.1%\n• `api-0c5e-7q1n` — 6.7%"
          }
        },
        {
          "type": "context",
          "elements": [
            {
              "type": "mrkdwn",
              "text": ":mag: `sum(rate(http_requests_total{job=\"api\",code=~\"5..\"}[5m])) / sum(rate(http_requests_total{job=\"api\"}[5m])) > 0.05`   `for: 10m`"
            }
          ]
        },
        {
          "type": "actions",
          "block_id": "oto_actions",
          "elements": [
            {
              "type": "button",
              "text": { "type": "plain_text", "text": ":eyes: Acknowledge", "emoji": true },
              "style": "primary",
              "action_id": "oto.ack",
              "value": "01J9XQ2K7M3T"
            },
            {
              "type": "static_select",
              "action_id": "oto.silence",
              "placeholder": { "type": "plain_text", "text": ":mute: Silence for…", "emoji": true },
              "options": [
                { "text": { "type": "plain_text", "text": "30 minutes" }, "value": "01J9XQ2K7M3T|30m" },
                { "text": { "type": "plain_text", "text": "1 hour" },     "value": "01J9XQ2K7M3T|1h" },
                { "text": { "type": "plain_text", "text": "4 hours" },    "value": "01J9XQ2K7M3T|4h" },
                { "text": { "type": "plain_text", "text": "24 hours" },   "value": "01J9XQ2K7M3T|24h" },
                { "text": { "type": "plain_text", "text": "Until resolved" }, "value": "01J9XQ2K7M3T|resolved" }
              ]
            },
            {
              "type": "button",
              "text": { "type": "plain_text", "text": ":book: Runbook", "emoji": true },
              "action_id": "oto.noop.runbook",
              "url": "https://runbooks.example.com/HighErrorRate"
            },
            {
              "type": "overflow",
              "action_id": "oto.more",
              "options": [
                { "text": { "type": "plain_text", "text": ":chart_with_upwards_trend: Open in Prometheus" }, "url": "https://prometheus.example.com/graph?g0.expr=..." },
                { "text": { "type": "plain_text", "text": ":bell: Open in Alertmanager" },  "url": "https://alertmanager.example.com/#/alerts?filter=%7Balertname%3D%22HighErrorRate%22%7D" },
                { "text": { "type": "plain_text", "text": ":busts_in_silhouette: Assign…" },  "value": "assign|01J9XQ2K7M3T" },
                { "text": { "type": "plain_text", "text": ":fire: Declare incident" },        "value": "incident|01J9XQ2K7M3T" },
                { "text": { "type": "plain_text", "text": ":white_check_mark: Force resolve" },"value": "resolve|01J9XQ2K7M3T" },
                { "text": { "type": "plain_text", "text": ":label: Show all labels" },        "value": "labels|01J9XQ2K7M3T" },
                { "text": { "type": "plain_text", "text": ":blue_book: Show timeline" },      "value": "timeline|01J9XQ2K7M3T" }
              ]
            }
          ]
        },
        {
          "type": "context",
          "elements": [
            {
              "type": "mrkdwn",
              "text": "oto   ·   `3f8c1a2b9d4e5f60`   ·   receiver `oto-webhook`   ·   _new alerts added_   ·   updated <!date^1786440012^{time}|09:20 UTC>"
            }
          ]
        }
      ]
    }
  ]
}
```

### Notes on the JSON above

- **The `text` field is a full sentence, not a slug.** It carries alertname, scope, severity, team, start time, and the runbook URL — because it is the push notification *and* the screen-reader content (§B8b).
- **Every `url` button still fires an interaction payload you must ack**:
  > *"If you're using `url`, you'll still receive an interaction payload and will need to send an acknowledgement response."* — [button element](https://docs.slack.dev/reference/block-kit/block-elements/button-element/)

  Hence the `oto.noop.runbook` naming convention: the handler needs an explicit no-op branch returning 200 for `oto.noop.*`. Forgetting this produces "This app is not responding" toasts. **Overflow options with a `url` behave the same way** — `oto.more` must also ack even when the chosen option was a link.
- `style: "primary"` is used on exactly **one** button, per Slack's guidance that primary *"should only be used for one button within a set"* and `danger` *"even more sparingly"*. Destructive actions (Force resolve) live in the overflow behind a confirm dialog rather than being styled `danger` inline.
- **UNVERIFIED**: whether `{ago}` composes with other tokens in one `<!date^…>` (docs show `{date} at {time}` composing; `{ago}` is only shown alone). The card above avoids the question by using `{time}` in the date tag and computing "Firing for 21m" server-side. Re-render on update to keep it fresh.

### Acknowledged state — via `chat.update`, same `ts`

- `color` → `#daa038`
- leading emoji → `:eyes:`
- Status field → `"*Status*\n:eyes: ~Firing~ → Acked by <@UA8RXUSPL>"` (Rootly's strikethrough trick)
- primary button → `{ "text": ":arrow_uturn_left: Un-acknowledge", "action_id": "oto.unack" }`, **no** `style`
- footer context appends `· acked <!date^…^{time}|…>`

### Resolved state — via `chat.update`, same `ts`

- `color` → `#2eb886`, emoji → `:white_check_mark:`
- title line → `":white_check_mark: *<link|HighErrorRate>* · `prod-eu` — Resolved"`
- Status → `"*Status*\n:white_check_mark: ~Firing~ → Resolved"`; add `*Duration*\n21m 10s`
- **Drop the instances section and the Affected-instances field** — zero information once resolved (Sentry's suppression rule)
- Actions collapse to `Show timeline` and `Declare incident` — Slack: *"condense the message and remove buttons"* after the flow completes

### Silenced state

`color` → `#dddddd`, emoji → `:mute:`, Status → `":mute: ~Firing~ → Silenced by <@U…> until <!date^…>"`, and the Silence select flips to an **Unsilence** button carrying the Alertmanager `silenceID`.

> Because Alertmanager stops delivering silenced alerts entirely (§A6), **this state is only ever set by us** — from our own silence action or from the API v2 reconciler polling `status.silencedBy`. There is no webhook that will tell us.

### Update-vs-post policy, driven by `notification_reason`

| `notification_reason` (AM ≥ 0.32.0) | Action |
|---|---|
| `first notification` | `chat.postMessage`; store `(channel, ts)`. |
| `new alerts added` | `chat.update` the card **and** post a thread reply "+2 instances now firing". |
| `some alerts resolved` | `chat.update` only — no thread noise. |
| `all alerts resolved` | `chat.update` to resolved state; optional thread reply with duration. |
| `repeat interval elapsed` | **`chat.update` only. Never post a new message.** |
| *(empty — AM < 0.32.0)* | Fall back to diffing the fingerprint set against stored state. |

> **`repeat interval elapsed` → update-only is the single biggest noise reduction available to us**, and it is the exact behaviour Grafana Alerting and stock Alertmanager get wrong (both repost). It is also the cheapest: `chat.update` is Tier 3 (50/min) while `chat.postMessage` is ~1/sec/channel.

**Escalation** for a genuinely unacknowledged critical: post a **threaded reply with `reply_broadcast: true`** rather than a new top-level card — it surfaces in-channel while keeping the thread as the single record. Slack advises using `reply_broadcast` *"sparingly"*, so gate it on severity + unacked duration. Note that Alertmanager's own `repeat_interval` default of **4h** (§A10) is far too slow for this — oto must run its own escalation clock.

**Thread hygiene:** per Knock's guidance, **start a fresh parent card when a thread exceeds 20–30 replies.**

### Configuration knobs to ship (modelled on PagerDuty)

- **Thread updates vs. update-in-place only** — per channel
- **Verbosity**: All updates / Status changes only / Firing + resolved only / Firing only
- **Show emoji next to field labels** — on/off
- **Severity → channel routing**
- **Which attributes to display and in what order** (incident.io's model), with per-attribute emoji

---

## B-Summary: Slack constraints that shape oto

| Constraint | Consequence |
|---|---|
| 50 blocks/message, 3000 chars/section, 150 chars/header (plain_text only) | Title must be a `section`, not a `header`. Cap inline instances at ~10. |
| No Block Kit equivalent of the color bar | Wrap blocks in one attachment. Attachments are legacy but not deprecated. |
| `chat.postMessage` ≈ **1 msg/sec/channel**; `chat.update` Tier 3 (50/min) | Update-in-place is both better UX and 50× cheaper. Per-channel token bucket. |
| 429 + `Retry-After` on rate limit | Honour the header. `slack-go`'s `OptionRetry()` covers 429 only. |
| May 2025 limits hit only `conversations.history`/`replies`, and **internal apps are exempt** | Never depend on reading Slack back. Our DB is the source of truth. |
| 3-second interaction ack; `trigger_id` expires in 3s | Verify → 200 → async. Open modals synchronously. |
| `response_url`: 30 min / 5 uses | Optimistic UI only. Long-lived state changes use `chat.update`. |
| Signature: `v0:ts:body` HMAC-SHA256, 5-min replay window, constant-time compare | Use `slack.NewSecretsVerifier` (≥ v0.23.1 — earlier versions accept an empty secret). |
| Socket Mode: no public endpoint, max 10 connections, barred from Marketplace | Default for self-hosted; HTTP as a flag. |
| `slack-go` has no major version; 10 minors in 4 months | Pin exactly; floor at v0.23.1. |
| `container` block unsupported by `slack-go` | No collapsible sections yet; use modals/threads for overflow detail. |
| Screen readers read only top-level `text` | `text` must be a complete sentence, not a slug. |
| URL buttons still deliver an interaction payload | Explicit no-op ack branch, or users see "app is not responding". |
