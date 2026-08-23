# Configuration reference

Every setting oto reads from its environment is listed here, with the default the process actually
boots with. The authority for a default is `Default()` in
`internal/platform/config/config.go:304-406`, not `.env.example`; where the two disagree, this page
follows the code and says so under [Where `.env.example` and the code
disagree](#where-envexample-and-the-code-disagree).

## The minimum viable .env

```bash
cp .env.example .env
just up
```

That is the whole first-run configuration. Every value in `.env.example` is a default that already
works against `docker-compose.yml`, and nothing in it has to be edited to get a booting API, a
migrated database and a UI.

The file must **exist**, even if empty: the `justfile` sets `dotenv-load`, so `just` fails to run a
recipe when `.env` is absent. Two settings are worth knowing about before the first run:

- `OTO_BOOTSTRAP_PASSWORD` is not in `.env.example` and has no default. `oto bootstrap` exits
  without it.
- `OTO_SECURITY_SECRET_KEY` is empty in `.env.example` and that is tolerated — the process boots and
  warns. It has to be set before any channel can be configured.

## How a value is resolved

`Load` binds three layers, later winning over earlier (`config.go:411-456`):

1. `Default()` — every field has one.
2. An optional YAML file, passed as `-config <path>` or `OTO_CONFIG=<path>`. `OTO_CONFIG` is read
   directly with `os.Getenv` (`cmd/oto/main.go:78`) and is not itself an `OTO_<SECTION>_<KEY>` name.
3. `OTO_*` environment variables.

The result is validated before it is returned, so a bad value fails the **boot** rather than being
ignored at runtime.

A variable name is `OTO_` + a section + `_` + the rest of the key, flattened. The sections are
`http`, `db`, `log`, `telemetry`, `jobs`, `ingest`, `retention`, `slack`, `security` and `tuning`
(`config.go:461-469`), so `OTO_DB_INGEST_SHARE_PERCENT` becomes `db.ingest_share_percent` and
`OTO_HTTP_BASE_URL` becomes `http.base_url`. A name with no known section prefix stays a top-level
key, which is how `OTO_ENV` and `OTO_SERVICE` work.

**Any value containing a comma is split into a list** (`config.go:428-434`). That is what makes
`OTO_HTTP_CORS_ORIGINS` a list of origins; it also means a scalar value must not contain a comma.

Durations are Go duration strings (`500ms`, `2s`, `15m`, `720h`). Booleans are `true`/`false`.

## Process identity

| Variable | Type | Default | Required | Controls |
|---|---|---|---|---|
| `OTO_ENV` | `dev` \| `staging` \| `prod` | `dev` | has a default | The deployment environment. `prod` is what `Config.IsProd()` reports. |
| `OTO_SERVICE` | string | `oto` | has a default | The service name carried on logs and telemetry. |
| `OTO_VERSION` | string | `dev` | no | Reported by `GET /api/v1/version`. Normally stamped by the linker at build time rather than set here. |
| `OTO_CONFIG` | path | none | no | A YAML config file to load between the defaults and the environment. Equivalent to `-config`. |

## Server and HTTP

| Variable | Type | Default | Required | Controls |
|---|---|---|---|---|
| `OTO_HTTP_ADDR` | string | `:8080` | has a default | The listen address. |
| `OTO_HTTP_BASE_URL` | URL | `http://localhost:8080` | has a default | The absolute root of every link oto emits — Slack card deep links and the webhook URL an operator pastes into Alertmanager. Validated as an `http_url`, and boot is refused if it contains `<`, `>`, `|` or whitespace, because those are Slack mrkdwn control characters and the card would ship silently wrong (`config.go:520-526`). |
| `OTO_HTTP_CORS_ORIGINS` | comma-separated list | **empty — CORS disabled** | no | The exact browser origins allowed to read authenticated responses. `*` is refused at boot: oto sends credentials, and no browser honours a wildcard with those. The Vite dev server proxies instead, so this is a dev fallback only. |
| `OTO_HTTP_READ_TIMEOUT` | duration | `15s` | has a default | Request read deadline. |
| `OTO_HTTP_WRITE_TIMEOUT` | duration | `30s` | has a default | Response write deadline. |
| `OTO_HTTP_IDLE_TIMEOUT` | duration | `120s` | has a default | Keep-alive idle deadline. |
| `OTO_HTTP_SHUTDOWN_TIMEOUT` | duration | `20s` | has a default | How long a graceful shutdown waits for in-flight requests. |
| `OTO_HTTP_REQUEST_TIMEOUT` | duration | `30s` | has a default | Bounds a single non-streaming request. SSE routes opt out. |
| `OTO_HTTP_MAX_BODY_BYTES` | bytes | `16777216` (16 MiB) | has a default | The generic request body cap. The ingest path has its own, lower, non-configurable bound. |

## Database

| Variable | Type | Default | Required | Controls |
|---|---|---|---|---|
| `OTO_DB_URL` | connection string | `postgres://oto:oto@localhost:5432/oto?sslmode=disable` | **has a working default** | The Postgres connection. The default already matches `docker-compose.yml`, so a local checkout needs no change here. Set it for anything else. |
| `OTO_DB_MAX_CONNS` | int, ≥ 4 | `20` | has a default | Total connections across both pools. |
| `OTO_DB_INGEST_SHARE_PERCENT` | int, 1–100 | `25` | has a default | The share of `max_conns` carved out for the ingest pool, so UI queries can never starve ingestion (SPEC §G.10). Boot fails if the split leaves the general pool empty. |
| `OTO_DB_INGEST_MIN_CONNS` | int, ≥ 1 | `4` | has a default | A floor under the ingest pool, applied after the percentage. |
| `OTO_DB_INGEST_STATEMENT_TIMEOUT` | duration | `2s` | has a default | `statement_timeout` set on every ingest-pool connection. |
| `OTO_DB_GENERAL_STATEMENT_TIMEOUT` | duration | `15s` | has a default | `statement_timeout` on every general-pool connection. |
| `OTO_DB_MAX_CONN_LIFETIME` | duration | `1h` | has a default | How long a pooled connection may live. |
| `OTO_DB_MAX_CONN_IDLE_TIME` | duration | `30m` | has a default | How long an idle connection is kept. |
| `OTO_DB_CONNECT_TIMEOUT` | duration | `5s` | has a default | Dial timeout for a new connection. |
| `OTO_DB_AUTO_MIGRATE` | bool | `false` | has a default | Run migrations at boot. Off deliberately: migrations are an explicit step (`just migrate`). |

There is no acquisition timeout under `db.*`, and there never was one that worked — pgxpool has
none. The wait is bounded by the ingest shedder, as `OTO_INGEST_ACQUIRE_TIMEOUT`.

## Security and credential sealing

| Variable | Type | Default | Required | Controls |
|---|---|---|---|---|
| `OTO_SECURITY_SECRET_KEY` | base64 of 32 bytes | **none** | see below | The AES-256-GCM key behind `platform/secrets`. Generate with `openssl rand -base64 32`. |
| `OTO_BOOTSTRAP_PASSWORD` | string | **none** | **yes, for `oto bootstrap`** | The first user's password. Read only from the environment, never a flag, so it cannot leak through `ps` or shell history (`cmd/oto/bootstrap.go:60-62`). Missing or empty and `oto bootstrap` exits with an error naming this variable. It is not a config key and appears in no YAML file. |
| `OTO_SECURITY_SESSION_TTL` | duration | `720h` (30 days) | has a default | Session lifetime. |
| `OTO_SECURITY_SESSION_COOKIE` | string | `oto_session` | has a default | The session cookie name. |
| `OTO_SECURITY_ALLOW_PRIVATE_TARGETS` | bool | `false` | has a default | Opens the SSRF guard (`platform/netguard`) for the whole process. With it on, oto will dial `10.0.0.0/8`, `127.0.0.1` and `169.254.169.254` on behalf of **any** tenant that configures a source or webhook pointing there, so it belongs only on a single-tenant, self-hosted install. |
| `OTO_SECURITY_ALLOW_INSECURE_TLS` | bool | `false` | has a default | Lets `alert_sources.tls_skip_verify` and a webhook channel's `insecure_skip_verify` take effect. Both are tenant-writable; whether an unverified certificate is acceptable is a statement about the operator's network, so it is decided here. With this false, both flags are refused at validation. |
| `OTO_SECURITY_LOGIN_RATE_BURST` | int, > 0 | `5` | has a default | Login attempts a fresh client address may make back to back. |
| `OTO_SECURITY_LOGIN_RATE_REFILL` | duration | `12s` | has a default | How long one login token takes to come back. |
| `OTO_SECURITY_LOGIN_MAX_CONCURRENT` | int, > 0 | `8` | has a default | How many argon2id verifications may run at once. argon2id costs 19 MiB per verification and runs on every login path, including unknown addresses, so this bounds memory as well as brute force. |

### `OTO_SECURITY_SECRET_KEY` in detail

It has **no default**, and an empty value is **tolerated**. A deployment with no secret key boots
without a keyring rather than with a fabricated one, and logs:

```
security.secret_key is not set: credential sealing is disabled and channels cannot be configured
```

See `internal/app/container.go:316-323`. Every credential read then fails loudly at the repository,
which is the correct blast radius — nothing is silently stored in the clear. In practice: you can
skip it for a first look at the product, and you must set it before configuring any channel.

```bash
openssl rand -base64 32
```

The value must decode to exactly 32 bytes; anything else fails the boot rather than the first read.

## Ingest

| Variable | Type | Default | Required | Controls |
|---|---|---|---|---|
| `OTO_INGEST_RETRY_AFTER` | duration | `10s` | has a default | The `Retry-After` sent with a 503. The ingest path answers 503 for anything transient, never 429 and never 4xx — a 4xx makes Alertmanager delete the alert permanently. |
| `OTO_INGEST_ACQUIRE_TIMEOUT` | duration | `500ms` | has a default | How long a webhook may wait for an ingest slot before it is shed with a 503. Lower it and oto gives up on a queued webhook sooner, spending less of Alertmanager's retry budget on waiting; raise it and oto holds the upstream's connection open for longer. |

**The payload bounds are not configurable.** An 8 MiB body, 10,000 alerts per batch and 64 labels
per alert are constants in `internal/ingestion/domain/bounds.go`, each bound to a DDL `CHECK` or to
the domain `LabelSet` constructor. A variable that disagreed with the `CHECK` it describes would
turn a rejected alert into a 500. `OTO_INGEST_MAX_ALERTS` and its siblings were declared, published
and read by nothing; they are gone.

## Slack and providers

| Variable | Type | Default | Required | Controls |
|---|---|---|---|---|
| `OTO_SLACK_ENABLED` | bool | `false` | has a default | Whether the Slack transport is constructed at all. |
| `OTO_SLACK_MODE` | `socket` \| `http` | `socket` | has a default | The interaction transport. **Socket Mode is not implemented**: `socket` disables the interactions endpoint without enabling anything in its place, so a deployment left at the default renders an Acknowledge button that nothing is listening for. Use `http`. |
| `OTO_SLACK_SIGNING_SECRET` | string | empty | **yes when enabled and `mode=http`** | The only thing authenticating `POST /api/v1/integrations/slack/interactions`. Boot fails with an empty value in http mode, because an empty secret accepts forged requests (`config.go:527-529`). |
| `OTO_SLACK_APP_TOKEN` | string | empty | no | The Socket Mode app token. Read by the config, unused while Socket Mode is unimplemented. |

Workspace credentials — the bot token and the workspace itself — are **not** environment variables.
They are per-org records, configured through the API and sealed with `OTO_SECURITY_SECRET_KEY`. See
[slack.md](slack.md) for the credentials and the app manifest, and
[slack-live-verification.md](slack-live-verification.md) for what to check against a real workspace.

The generic-webhook provider has no variables of its own. Its two trust decisions come from the
security section: `OTO_SECURITY_ALLOW_PRIVATE_TARGETS` and `OTO_SECURITY_ALLOW_INSECURE_TLS`.

## Notifications and enrichment

Neither has environment variables. Notification policies, channels and notification templates are
tenant data, created through the API and stored in Postgres; enrichment is configured by which
enrichers are registered in `internal/app/container.go` and by the per-phase budgets in
`internal/enrichment/service/registry.go`. What an operator can set from the environment about
either is the per-org tuning below — chiefly `default_verbosity`, which decides how much a channel
is told.

## Worker and River queues

| Variable | Type | Default | Required | Controls |
|---|---|---|---|---|
| `OTO_JOBS_ENABLED` | bool | `true` | has a default | Whether this process runs the River worker. |
| `OTO_JOBS_FETCH_INTERVAL` | duration | `1s` | has a default | How often the worker polls for jobs. |
| `OTO_JOBS_JOB_TIMEOUT` | duration | `1m` | has a default | Per-job deadline. |
| `OTO_JOBS_RESCUE_AFTER` | duration | `1h` | has a default | How long a stuck job waits before being rescued. |
| `OTO_JOBS_QUEUE_INGEST` | int, ≥ 0 | `0` = unset | no | Overrides the `ingest` queue width. |
| `OTO_JOBS_QUEUE_DEFAULT` | int, ≥ 0 | `0` = unset | no | Overrides the default queue width. |
| `OTO_JOBS_QUEUE_DELIVERY` | int, ≥ 0 | `0` = unset | no | Overrides the delivery queue width. |
| `OTO_JOBS_QUEUE_RECONCILE` | int, ≥ 0 | `0` = unset | no | Overrides the `reconcile` queue width. |

**Zero means unset, not "no workers".** Each width is applied only when it is greater than zero, so
leaving all four alone is what lets `jobs.DefaultQueueWorkers()` — the SPEC §G.3 table — be the
default: `ingest` 16, `enrich` 8, `notify` 8, `deliver_slack` 4, `deliver_webhook` 8, `reconcile` 8,
`lifecycle` 4, `maintenance` 1. Setting one departs from the published number, and the deployment
answers for its own width.

`reconcile` is the one to think about before the others: its 8 is a **supported tenant count**
(roughly 120), not a throughput preference. Below the width SPEC §G.3.1's arithmetic requires,
`source.reconcile` falls permanently behind its own 30-second cadence rather than merely running
slower.

## Observability

| Variable | Type | Default | Required | Controls |
|---|---|---|---|---|
| `OTO_LOG_LEVEL` | `debug` \| `info` \| `warn` \| `error` | `info` | has a default | `log/slog` level. |
| `OTO_LOG_FORMAT` | `json` \| `text` | **`json`** | has a default | Log encoding. |
| `OTO_LOG_SOURCE_LOCATION` | bool | `false` | has a default | Adds the caller `file:line` to every record. Costly. |
| `OTO_TELEMETRY_METRICS_ENABLED` | bool | `true` | has a default | Serve the Prometheus registry. |
| `OTO_TELEMETRY_METRICS_PATH` | path, must start with `/` | `/metrics` | has a default | Where the registry is served. |
| `OTO_TELEMETRY_TRACING_ENABLED` | bool | `false` | has a default | Export OpenTelemetry traces. |
| `OTO_TELEMETRY_OTLP_ENDPOINT` | host:port | `localhost:4317` | has a default | The OTLP collector. |
| `OTO_TELEMETRY_OTLP_INSECURE` | bool | **`true`** | has a default | Send OTLP without TLS. On by default, which suits a sidecar collector and not a remote one. |
| `OTO_TELEMETRY_TRACE_SAMPLE_RATE` | float, 0–1 | `0.1` | has a default | Trace sampling ratio. |

There is **no** process-wide `OTO_LOG_REDACT_LABELS`. Redaction is per source: set `redact_labels`
and `redact_annotations` on the alert source (`POST`/`PATCH /api/v1/sources`). They are glob
patterns applied to label and annotation **values** before `ingest_batches.payload` is written, so a
matched value never reaches disk.

The metric names oto exports each have a runbook in [`../runbooks/`](../runbooks/README.md).

## Retention

**Destructive.** These are the only settings in oto that delete anything. Partitions are dropped whole, with no
undo and no export (ADR 0024).

| Variable | Type | Default | Required | Controls |
|---|---|---|---|---|
| `OTO_RETENTION_RAW_PAYLOADS` | duration | `720h` (30 days) | has a default | How long raw webhook bodies are kept. This is the depth of the rejections and failed-batch feeds, and the window in which a stored batch can be replayed. No alert page is served from here. |
| `OTO_RETENTION_EVENTS` | duration | `9360h` (13 months) | has a default | How long the alert timeline is kept. Dropping a month destroys every human comment and unack note in it — those live nowhere else. The alert, its cases, the acks, the rule text and the delivery record are never reaped. |
| `OTO_RETENTION_UI_EVENTS` | duration | `24h` | has a default | How long the `ui_events` stream buffer that backs SSE resume is kept. |

These are a **floor**, not the last word: a partition holds every tenant's rows, so
`partitions.manage` drops at the maximum of these and every org's own `orgs.settings` value.

## Declarative tuning

The seven per-org tuning keys can be pinned from the environment as `OTO_TUNING_<KEY>`. Precedence
is: shipped default → org override in Postgres → **these**, highest.

Pinning a key **takes the setting away from the UI**. The settings API reports its origin as
`config`, names the variable so an operator knows where to change it, and refuses a `PATCH` on that
key with 409 rather than accepting a value that would revert on the next deploy. An override the org
already wrote is kept, reported as `shadowed`, and takes effect again the moment the line is
removed. Use these when oto is deployed from Helm values and the repository is meant to be the
truth; otherwise leave them unset and let each org tune itself.

| Variable | Setting key | Bounds |
|---|---|---|
| `OTO_TUNING_RESOLVE_GRACE_S` | `resolve_grace_s` | 60–86400 seconds |
| `OTO_TUNING_FLAP_THRESHOLD` | `flap_threshold` | 3–100 |
| `OTO_TUNING_FLAP_WINDOW_S` | `flap_window_s` | 300–86400 seconds |
| `OTO_TUNING_FLAP_DIGEST_INTERVAL_S` | `flap_digest_interval_s` | 60–86400 seconds |
| `OTO_TUNING_RAW_RETENTION_DAYS` | `raw_retention_days` | 1–365 |
| `OTO_TUNING_EVENT_RETENTION_MONTHS` | `event_retention_months` | 1–120 |
| `OTO_TUNING_DEFAULT_VERBOSITY` | `default_verbosity` | `all` \| `status_changes` \| `firing_and_resolved` \| `firing_only` |

That is the whole key set — it is exactly `identity/domain.AllSettingKeys()`, and that list is the
authority. A name outside it does not add a setting; it fails the boot with `unknown_key`. A value
outside the bounds fails the boot too, rather than being quietly clamped.

The three flap keys are **permanently inert**: they are accepted, validated and stored, and they
decide nothing, because flap damping was retired (ADR 0042 Amendment 3, SPEC §B.3). They keep their
names on the standing rule that deleting a settings key is a contract change of its own.

Derived defaults, the corpus each number was read off, and the retention windows in operational
terms are in [tuning.md](tuning.md); they are not repeated here.

## Local dev Alertmanager wiring

These three are **not read by oto**. They exist because the compose Alertmanager has to be told
where to POST and with what token, and Alertmanager expands no environment variables — a literal
`$VAR` in its config is parsed as a URL and fails at load. `just am-wire` reads them and renders
`deploy/alertmanager/local/{webhook_url,ingest_token}`, which the receiver picks up through
`url_file` and `credentials_file`. It runs automatically before `just infra`.

| Variable | Default | Controls |
|---|---|---|
| `OTO_AM_LOCAL_WEBHOOK` | the all-zero uuid ingest URL | Where the dev Alertmanager posts. `localhost` is rewritten to `host.docker.internal`, because Alertmanager dials from inside a container. |
| `OTO_AM_LOCAL_INGEST_TOKEN` | a placeholder that 404s loudly | The bearer token that receiver sends. |
| `OTO_AM_LOCAL_PAT` | none | A personal access token for poking the API by hand (`just stream`, curl). |

Both of the first two come from creating a source; the token is returned exactly once. Left unset,
the receiver points at the all-zero uuid, which 404s loudly rather than posting to a route that does
not exist.

## Where `.env.example` and the code disagree

`.env.example` is a curated starting point, not a generated one. Five differences are worth knowing.

| What | `.env.example` | The code |
|---|---|---|
| Log format | `OTO_LOG_FORMAT=text` (line 27) | `Format: "json"` (`config.go:334`). Copying the example gives text logs; an unset variable gives JSON. |
| CORS origins | `OTO_HTTP_CORS_ORIGINS=http://localhost:5173` (line 15) | `CORSOrigins: []string{}` (`config.go:317`) — the shipped default disables CORS entirely. The example's own comment says so; the line is a dev convenience that a production `.env` must remove. |
| `OTO_RETENTION_UI_EVENTS` | absent | Exists, default `24h` (`config.go:388`). |
| `OTO_BOOTSTRAP_PASSWORD` | absent | Required by `oto bootstrap` (`cmd/oto/bootstrap.go:60-62`). |
| Whole sections | no `OTO_JOBS_*`, no `OTO_HTTP_*` timeouts, no `OTO_DB_*` connection lifetimes, no `OTO_SECURITY_SESSION_*`, no `OTO_TELEMETRY_METRICS_PATH` / `_OTLP_INSECURE` / `_TRACE_SAMPLE_RATE`, no `OTO_SLACK_APP_TOKEN` | All exist with the defaults tabulated above. |

Two further names appear in source comments and are read by nothing.
`OTO_ALLOW_PRIVATE_WEBHOOK_TARGETS` is named in `internal/channels/providers/webhook/provider.go:57`
and `internal/channels/registry/registry.go:42`; the value actually wired there is
`OTO_SECURITY_ALLOW_PRIVATE_TARGETS` (`internal/app/container.go:407`). `config.go:30` refers to
"the Makefile's LDFLAGS" for the linker-stamped build fields.

## Related pages

- [tuning.md](tuning.md) — the per-org settings in operational terms, the corpus the defaults were
  derived from, and the retention windows.
- [slack.md](slack.md) — connecting a workspace: the three credentials, the app manifest, and every
  Slack error oto classifies.
- [slack-live-verification.md](slack-live-verification.md) — the checklist for a real workspace.
- [`../runbooks/README.md`](../runbooks/README.md) — one runbook per exported metric.
