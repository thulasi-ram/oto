---
title: oto
---
**The alert history your Prometheus stack does not keep.**

oto sits behind an Alertmanager you already run and records what happens to every alert:
when it first appeared, every episode since, *what the rule said at that moment*, who was
told on which channel in which thread, who acknowledged it, and how it ended — as one
replayable timeline, in a web UI and in Slack.

Self-hosted, MIT, Postgres-only. It is a flight recorder, not an incident manager.

> **Status: pre-release.** No version has been tagged yet. The schema is stable enough to
> run against and migrations are expand/contract-only, but nothing here is 1.0.

---

## Why this exists

Ask your current stack a question about last Tuesday.

- **Alertmanager keeps no history.** It is a silence console with a live view. Once an
  alert resolves, the fact that it fired — and what the rule looked like when it did — is
  gone.
- **Grafana's alert history only covers Grafana-managed rules.** If your rules live in
  Prometheus files, you get nothing.
- **The Slack thread is the real record, and it is unqueryable.** "How many times has this
  fired this quarter?" "Who acked it?" "Was the rule edited between episodes?" — all of
  that is sitting in scrollback, unaddressable.
- **Rules mutate silently.** An alert that fires today with a different `for:` than it had
  last month has a history that quietly lies to you.

And the tools that would fix it are the wrong shape: Robusta's UI is SaaS-or-enterprise,
Keep is incident-first and generic, and Grafana OnCall OSS was archived in March 2026.

oto answers those four questions. It reads from your Alertmanager and writes nothing back to
your cluster.

## How it works, in five points

1. **Point one Alertmanager at oto.** One webhook receiver, one per-source ingest token.
   Nothing else in your cluster changes; oto has no write path into it.
2. **oto gives every distinct label set a permanent identity** — an **Alert**. It is created
   the first time it is seen and it is never deleted, so "has this happened before?" has an
   answer.
3. **Every firing episode is an AlertCase.** Separate, sequential, terminal. Ack one and you
   have acknowledged exactly that episode, not the alert forever.
4. **At the moment it fires, oto captures the rule.** Content-addressed, stored per episode.
   When the rule changes between episodes, `rule_changed` is a first-class fact, and the
   timeline shows the drift. Grafana, Robusta and Keep all keep the alert; none of them keep
   the rule text per episode.
5. **Slack is a real surface, not a notification sink.** One thread per episode, the root
   card updated in place, ack/snooze/comment from the message — and every one of those
   actions lands in the same timeline the UI reads.

Routing is by label matcher to a channel, never to a person — see
[What oto is not](#what-oto-is-not).

## What it looks like

The screens below are a demo org (`Acme Corp`) with generated data — see
[Try it with demo data](#try-it-with-demo-data).

**The queue.** One row per firing episode, not per alert. Ack receipts carry who and when.

![The case queue](/oto/assets/screenshots/cases.png)

**One episode, end to end.** Firing duration, the rule as it read at fire time, the full
append-only timeline, and every delivery that went out.

![A single case in detail](/oto/assets/screenshots/case-detail.png)

**Rule drift.** The same alert, two episodes, two different rules — which is the whole
reason this project exists.

![Alert detail showing rule drift](/oto/assets/screenshots/alert-detail.png)

**The delivery log.** Who was told, where, and whether it actually arrived. Failures are
visible rather than swallowed.

![The notification activity log](/oto/assets/screenshots/notification-activity.png)

## Quick start

Prerequisites: Go 1.26, Node 24, Docker, [`just`](https://github.com/casey/just).

```bash
cp .env.example .env   # the defaults already match docker-compose.yml
just setup             # go mod download, npm install, dev tooling
just infra             # postgres 17 + alertmanager + prometheus, waits for health
just migrate           # apply migrations (this runs `oto migrate`, not raw goose)

# The first org, the first user, the first token. There is no signup route and no
# org-creation API on purpose, so a freshly migrated database has no credential that
# can log in. This subcommand is the only way to make one, and it refuses to run twice.
OTO_BOOTSTRAP_PASSWORD='a long passphrase' \
  go run ./cmd/oto bootstrap --org-slug acme --email you@example.com

just up                # API on :8080, UI on :5173
```

Then open <http://localhost:5173> and log in with that email and passphrase.

| | |
|---|---|
| UI | <http://localhost:5173> — the Vite dev server, with hot reload |
| API | <http://localhost:8080> — `/healthz`, `/readyz`, `/metrics`, `/api/v1/…` |
| Alertmanager | <http://localhost:9093> |
| Prometheus | <http://localhost:9090> — evaluates `deploy/prometheus/oto-rules.yaml` |

`just health` curls `/healthz` then `/readyz`; `/readyz` touches the database, so a green
there proves Postgres and the migrations too. `just` on its own lists every recipe.

**In a deployment there is no second port.** The released image serves the UI from the same
origin as the API, at `/` — the SPA calls relative paths (`/api/v1/…`) and has no
base-URL setting, so same-origin is the only arrangement that works. `:5173` above is the
dev server and exists only so that hot reload does. A binary you built yourself with
`go build` carries no UI until `just ui-build` has written `web/dist`; it says so at `/`
rather than 404ing, and `oto version` reports `ui: absent` or `ui: embedded (N files)`.

`bootstrap` prints a personal access token **once** — only its sha256 is stored. Losing it
is not losing API access: **Settings → Access tokens** mints more. Minting always requires a
signed-in session, so a token can never mint or enumerate its siblings.

### Try it with demo data

A freshly migrated database is empty, and an empty oto is a set of placeholder sentences.
To see the product with something in it:

```bash
just setup                                          # once per checkout
OTO_BOOTSTRAP_PASSWORD='a long passphrase' just demo
just up
```

`just demo` runs infra, migrations, bootstrap and the seeder in order, then prints the
credentials. Log in as `ops@acme.example` with that passphrase.

It seeds a fictional org: three clusters, five services, 21 alert identities, 36 episodes
across every state, acks with actors and notes, 94 deliveries of which some failed and two
are dead-lettered, three message templates, a snoozed alert — and one alert whose rule
changed between episodes, so the drift view has something real to show.

The seeder refuses to run against a production environment, and refuses to run twice. If
your database already has data, `just reset` first — note that this destroys the dev volume.

The demo's alert sources point at fictional hosts, so after roughly three hours oto probes
them, correctly concludes it cannot reach them, and says so in a source-health banner. That
is the health check working, not the demo breaking. `just reset && just demo` gives you a
clean window again.

### Seeing a real alert arrive

`just infra` also starts a Prometheus whose first rule, `OtoStackSmokeTest`, is deliberately
always true with a 15 s `for:` — so within a minute <http://localhost:9093> shows a card
without you having to break anything. That proves Prometheus → Alertmanager.

The last hop needs two values that cannot exist before you do: the ingest route is per
source (`/api/v1/ingest/alertmanager/{source_id}`) and authenticates with that source's own
token. Create a source in the UI, paste its `webhook_url` and `ingest_token` — returned
exactly once — into `deploy/alertmanager/alertmanager.yml`, then
`docker compose restart alertmanager`. That file's header comment has the exact call. Until
you do, the receiver points at an all-zero uuid that 404s on purpose, rather than at a URL
that looks plausible and silently is not.

That same Prometheus is a legitimate `prometheus_url` for the source, which is what lets oto
capture what a rule said at the moment it fired ([ADR 0009](/oto/adr/0009-rule-snapshot-versioning-at-fire-time/)).

## Concepts

Six nouns carry the whole model. [`docs/concepts.md`](/oto/concepts/) is the full
reference; this is the shape.

| | |
|---|---|
| **Alert** | The identity of a label set within an org and cluster. Created on first sight, never deleted. |
| **AlertCase** | One contiguous firing episode, `(alert_id, seq)`. The thing you ack. |
| **AlertEvent** | One immutable fact at one instant. Append-only; current state is a projection of these. |
| **RuleSnapshot** | The alerting rule, content-addressed, as it read at fire time. |
| **NotificationPolicy** | Label matchers → up to 16 **Channels**. Evaluated in priority order, *lowest first*; first match wins. Targets destinations, never people. |
| **Conversation** | What decides which facts share a message. One per Case, bound to one thread per Channel. |

```
Prometheus rule fires
        │
        ▼
   Alertmanager ──── webhook + per-source ingest token ───┐
   (silenced alerts are dropped here)                     │
                                                          ▼
                        POST /api/v1/ingest/alertmanager/{source_id}
                        one transaction: durably record the raw batch and
                        enqueue the work. Slack is never on this path.
                                                          │
                                                          ▼
 Alert              the identity of a label set within (org, cluster).
   │                Born the first time it is seen. Never deleted.
   │
   └── AlertCase    one firing episode, (alert_id, seq). The thing you ack.
          │         open ──▶ closed. Terminal: a re-fire opens seq+1 and
          │         never revives the one before it.
          │
          ├── AlertEvent    one immutable fact at one instant. The timeline.
          │                 Current state is a projection of these, never
          │                 an overwrite.
          ├── RuleSnapshot  the rule, content-addressed, as it read at fire
          │                 time. Nothing else in this space keeps it.
          └── Enrichment    a typed, provenanced result from one Enricher.
                            │
       a transition happens │ and carries exactly one Reason
       (fired, acked, snoozed, rule_changed, … 15 in total)
                            ▼
 NotificationPolicy   label matchers, evaluated in priority order, LOWEST
                      FIRST, first match wins. Routes to 1..16 Channels —
                      never to a person.
                            │
                            ▼
 Notification         ONE idempotent intent to communicate that one fact
                            │
             fan out, one NotificationDelivery per Channel
                   ┌────────┴────────┐
                   ▼                 ▼
             #sre-alerts      #platform-warnings
                   └────────┬────────┘
                            ▼
 Conversation         one per Case, bound to one ChannelThread per Channel.
                      The root card is UPDATED IN PLACE; replies are gated
                      by that Channel's verbosity.
                            │
                            ▼
 A human acks, snoozes or comments
                            │
                            └──▶ appends another AlertEvent
                                 → which mints the next Notification
                                 → which updates that same root card
                                 (and the UI, live, over SSE)
```

An **Alert** is `firing`, `suppressed`, `resolved` or `expired` — and `expired` is not
`resolved`. Ack state and snooze are two orthogonal axes on top of that. A human may write
ack state and nothing else: there is no Resolve, Close, Merge, Dismiss or Assign, because
a human does not get to overwrite what a signal said.

## What oto is not

Not an incident manager, an on-call or paging system, a workflow engine, a rule editor, an
AI investigator, or anything with a write path into your cluster.

This is enforced rather than promised. `tools/lintvocab` fails CI on thirteen banned concept
words and six forbidden column names, and `notification_policies` has a `ChannelIDs` column
and no other target field — "route this to a person" cannot be expressed without editing the
struct. The reasoning is in
[ADR 0013](/oto/adr/0013-alert-first-scope-boundary/) and
[`docs/design/SCOPE-BOUNDARY.md`](/oto/design/scope-boundary/), which adjudicates 32
feature requests one by one.

## Layout

```
cmd/oto/           the binary: server, worker, operational subcommands
internal/app/      explicit constructor wiring; the dependency graph is readable
internal/platform/ config, log, telemetry, db, httpx, jobs, errs, id, clock
internal/<domain>/ api / service / repository / domain — see CONTEXT.md
db/migrations/     goose SQL, expand/contract only
api/openapi/       the contract; the TS client and validators are generated from it
web/               Vite 6 + SolidJS + TypeScript strict + Tailwind v4
deploy/            alertmanager and prometheus configs, the Slack app manifest
deploy/helm/oto/   API + worker Deployments, `oto migrate` as a pre-upgrade hook.
                   Postgres is EXTERNAL — no subchart, by decision (ADR 0014)
docs/              concepts, ADRs, the binding spec, setup guides, metric runbooks
test/harness/      real Postgres via testcontainers; fakes for the four true externals
test/contract/     the drift gates between Go, OpenAPI and the TS client
test/load/         burst cases behind `//go:build load`; CI does not run them
tools/lintvocab/   the scope-boundary linter
```

Stack: Go 1.26 (chi, pgx, River for jobs), Postgres 17 as the only datastore, SolidJS on the
front end, Slack as the primary channel. No analytical store, no stream processor, no
Postgres subchart — see ADRs [0001](/oto/adr/0001-postgres-sole-datastore-river-job-queue/) and
[0014](/oto/adr/0014-postgres-only-no-analytical-store/).

## Where to read next

| | |
|---|---|
| [`docs/concepts.md`](/oto/concepts/) | The domain model in full: every noun, the state machines, worked examples. **Start here.** |
| [`CONTEXT.md`](/oto/architecture/) | Implementer's map: domain language, module map, layering rules. |
| [`CONTRIBUTING.md`](/oto/contributing/) | Setup, the task runner, what each gate catches, how to read a red one. |
| [`docs/setup/configuration.md`](/oto/setup/configuration/) | Every environment variable, its default, and whether it is required. |
| [`docs/setup/slack.md`](/oto/setup/slack/) | Connecting a workspace, and what to do about every Slack error oto classifies. |
| [`docs/setup/tuning.md`](/oto/setup/tuning/) | `resolve_grace`, flap thresholds, retention windows, and how to derive each from your own rules. |
| [`docs/runbooks/`](/oto/runbooks/) | One page per `oto_*` metric: what it counts, what a sustained value means, what to do. |
| [`docs/releasing.md`](/oto/releasing/) | Cutting a tag, what the pipeline publishes to GHCR, and the one step it cannot do for you. |
| [`docs/design/SPEC.md`](/oto/design/spec/) | The binding specification. It changes only through an ADR in the same commit. |
| `docs/adr/` | Every decision and what superseded it — 50 of them, starting with [ADR 0001](/oto/adr/0001-postgres-sole-datastore-river-job-queue/). |

## Licence

MIT — see [`LICENSE`](LICENSE) and [ADR 0019](/oto/adr/0019-mit-licence/). No `ee/`
directory, no feature behind a licence key, no CLA.

Issues live in the repository itself via [`git-bug`](https://github.com/git-bug/git-bug):
`git-bug bug` lists them, `git-bug bug new` files one.
