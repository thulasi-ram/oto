# oto

**The alert history layer your Prometheus stack does not have — self-hosted, with a UI, without
adopting an AIOps platform or a paid SaaS.** For every alert that has ever fired, oto shows: when
it first appeared, every episode since, *what the rule said at that moment*, who was told, on
which channel, in which thread, who acknowledged it, and how it ended — as one continuous,
replayable timeline. Alertmanager holds no history; Grafana's alert history only covers
Grafana-managed rules; Robusta's UI is SaaS-or-enterprise; Grafana OnCall OSS was archived in
March 2026. oto fills that vacuum: a self-hostable, OSS, Slack-first alert layer over an existing
Prometheus/Alertmanager stack. It is not an incident manager, an on-call/paging system, a
workflow engine, a rule editor, an AI investigator, or anything with a write path into your
cluster.

## Running it

Prerequisites: Go 1.26, Node 24, Docker.

```bash
cp .env.example .env      # defaults already match docker-compose.yml
make db-up                # postgres:17 + alertmanager, waits for health
make migrate-up           # apply migrations
make dev                  # API on :8080
make ui-dev               # UI on :5173, proxying /api and /healthz to :8080
```

Then open <http://localhost:5173>. The landing page calls `/healthz` through the dev proxy, which
is the quickest way to confirm the front end and the back end are talking.

| | |
|---|---|
| API | <http://localhost:8080> — `/healthz`, `/readyz`, `/metrics`, `/api/v1/…` |
| UI | <http://localhost:5173> |
| Alertmanager | <http://localhost:9093> — routes every alert to oto on the host |

`make help` lists every target. The ones you will use most:

```
make build          build ./bin/oto
make test           go test -race ./...   (integration tests need Docker)
make lint           golangci-lint, including the depguard layering rules
make ci             fmt + lint + build + test + ui-build
make migrate-status which migrations have been applied
make db-reset       destroy the dev volume and start clean
```

## Layout

```
cmd/oto/          the binary: server, worker, operational subcommands
internal/app/     explicit constructor wiring; the dependency graph is readable
internal/platform config, log, telemetry, db (two pools + tx), httpx, jobs, errs, id, clock
internal/<domain> api / service / repository / domain — see CONTEXT.md §5
pkg/alertkey/     canonical label serialisation and the identity keys
db/migrations/    goose SQL, expand/contract only
web/              Vite 6 + SolidJS + TypeScript strict + Tailwind v4
deploy/           helm chart, compose, alertmanager and prometheus config
test/             fixtures, integration, contract, load, harness
```

The layering rules are mechanically enforced by `depguard` in `.golangci.yml`: `api` cannot import
`repository`, `repository` cannot import `api`, `domain` packages import no I/O at all, and no
domain can reach into another domain's internals. If `make lint` rejects an import, the import is
wrong — not the rule.

## Where to read next

- `CONTEXT.md` — the map: domain language, module map, layering rules, conventions. **Read first.**
- `docs/design/SPEC.md` — binding specification. Implement it literally.
- `docs/design/domain-research.md` — verified ground truth about Alertmanager and Slack.
- `docs/adr/` — amendments. The SPEC changes only through an ADR in the same commit.

Issue tracking lives in the repository itself via [`git-bug`](https://github.com/git-bug/git-bug):
`git-bug bug` lists, `git-bug bug new` files.
