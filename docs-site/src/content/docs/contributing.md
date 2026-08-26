---
title: Contributing to oto
---
oto is a flight recorder for alerts. Read [CONTEXT.md](/oto/architecture/) first: it carries the domain
language, the module map and the layering rules, and most refused changes are refused for
contradicting one of the three. [docs/design/SPEC.md](/oto/design/SPEC/) is binding and meant to
be implemented literally; it changes only through an ADR in the same commit.

## Ground rules

- **MIT licence** — see [LICENSE](LICENSE) and [ADR 0019](/oto/adr/0019-mit-licence/). No `ee/`
  directory, nothing behind a licence key, and **no CLA**: a contribution needs no paperwork.
- **Claims are backed or removed.** An unverified count, or a comment describing deleted machinery
  in the present tense, is treated as a defect in its own right.

## Filing and finding work

Work is tracked in the repository with [`git-bug`](https://github.com/git-bug/git-bug), not GitHub
Issues, so the tickets travel with the clone.

```bash
git bug bug                 # list every ticket: id, state, title
git bug bug show <id>
git bug bug new -t "<title>" -F body.md
```

House convention, consistent across the tickets and the commit log: **a title is a declarative
statement of the defect, never an imperative** — "`deleteSource` commits SoftDelete and
RevokeIngestTokens as two transactions", not "Fix deleteSource". Bodies use six bold lead-in
sections, in order — `**What is wrong.**`, `**The demonstration.**`, `**Why it matters.**`,
`**Where.**`, `**Scope.** small|medium|large`, `**Done when.**` — as prose rather than bullets, with
concrete counts and `file:line` throughout, quoting the CONTEXT.md or SPEC rule verbatim with its §
number when a finding violates one. Calibrate on a recent ticket; the oldest use only four sections.
Note that `-t` is silently ignored when `-F` is also passed: the title becomes the body's first
paragraph, so verify with `git bug bug show` and repair with `git bug title edit`.

## Environment setup

| Tool | Version | Pinned in |
|---|---|---|
| Go | 1.26 | [`go.mod`](go.mod) line 3, and `GO_VERSION` in [`ci.yml`](.github/workflows/ci.yml) |
| Node | 24 | `NODE_VERSION` in `.github/workflows/ci.yml` |
| Postgres | 17 | `postgres:17-alpine` in [`docker-compose.yml`](docker-compose.yml) |
| golangci-lint | v2.12.2 in CI; `@latest` from `just setup` | `.github/workflows/ci.yml`, [`justfile`](justfile) |
| kubeconform | v0.7.0 | `justfile` and `ci.yml`, identically |
| helm | v4.1.1 | `justfile` and `ci.yml`, identically |
| `just` | any recent release | not pinned |

A Docker daemon is required — the suite starts real Postgres containers. Docker Desktop and
[colima](https://github.com/abiosoft/colima) both work; colima needs no setup, because the recipes
detect its socket.

```bash
cp .env.example .env   # first: the justfile sets dotenv-load, so no recipe runs without it
just setup     # go mod download, npm install, then golangci-lint + kubeconform + helm
just up        # infra, migrations, API+worker on :8080, UI on :5173
```

`just setup` puts all three binaries in `$(go env GOPATH)/bin` — no sudo, no Homebrew, the same
lines on macOS and Linux. helm is **installed**, not warned about: `just ci` depends on
`just helm-check`, so a tool the setup recipe could only print a sentence about is a fresh clone
whose first `just ci` fails and reads as a broken repository. `.env` must exist, because the
justfile sets `dotenv-load`; `cp .env.example .env` is enough to boot, and every variable is
documented in [docs/setup/configuration.md](/oto/setup/configuration/).

## The task runner

`just` is the only task runner ([ADR 0021](/oto/adr/0021-correctness-and-testing-strategy/): a
second one is a second answer to what green means, and the two drifted). `just` with no arguments
lists every recipe with its group.

| Run | |
|---|---|
| `just up` | infra, migrations, `go run ./cmd/oto` and the Vite dev server, in one foreground process |
| `just infra` / `just down` | compose Postgres, Alertmanager, Prometheus, blocking until healthy / stop them, keeping the volume |
| `just api` / `just worker` | API and worker in one process / the worker alone |
| `just ui` / `just docs` | Vite dev server (proxies `/api`, `/healthz`) / the Starlight docs site on `/oto/` |
| `just docs-check` | build the docs site and fail on any internal link that does not resolve |
| `just logs [service]` | tail container logs |
| `just health` / `just metrics [filter]` | `/healthz` and `/readyz` / oto's own Prometheus metrics |
| `just fire-alert <source-id> [token] [status]` | POST a synthetic Alertmanager webhook at the ingest endpoint |
| `just stream <token>` | watch the SSE stream |

| Database | |
|---|---|
| `just migrate` | every pending migration: goose schema, **then River's own tables** |
| `just status` / `just new-migration <name>` | what has been applied / create an empty goose pair |
| `just rollback` / `just migrate-roundtrip` | down exactly one / up-down-up from empty, proving every Down |
| `just reset` / `just psql` | destroy the volume and rebuild, irreversibly / psql on the dev database |

| Gates and tooling | |
|---|---|
| `just fmt` | `go fmt ./...` and `go mod tidy` |
| `just lint` | gofmt check, `go build`, `go vet`, then golangci-lint by absolute path |
| `just test` | `go test -race -count=1 ./...`, with the colima socket detected first |
| `just generate` | regenerate the TS client and the valibot validators from the OpenAPI contract |
| `just dto-check` `just server-check` `just generate-check` `just validators-check` | gates G1–G4 |
| `just lint-vocabulary` / `just lint-reachability` | the two scope gates |
| `just helm-check` | `helm lint`, `helm template` over the value combinations that matter, `kubeconform` over every rendered manifest |
| `just ui-build` / `just ui-test` | `tsc --noEmit && vite build` / vitest |
| `just ci` | every gate above, as one local sequence ([`justfile`](justfile) line 440) |

| Release | |
|---|---|
| `just release <vX.Y.Z>` | eight checks, then an annotated tag. Cuts locally; never pushes |
| `just release-watch <vX.Y.Z>` | follow that tag's pipeline run, exiting non-zero if it failed |

Pushing the tag is what publishes, and it is typed by hand on purpose.
[`docs/releasing.md`](/oto/releasing/) is the whole procedure: what the eight checks are for, what
the pipeline puts in GHCR, and the one step it cannot do for you.

## The gate set

`just ci` and `.github/workflows/ci.yml` are kept in step deliberately: a contributor's green must
be CI's green. Each gate catches a class the others structurally cannot.

| Gate | Catches what nothing else does |
|---|---|
| `just lint` | gofmt drift, `go vet`, and the **depguard layering contract** in [`.golangci.yml`](.golangci.yml): `api-must-not-import-repository`, `repository-must-not-import-api`, `domain-must-be-pure`, `platform-must-not-import-domains`, `slack-sdk-is-provider-only`, and one `<module>-must-not-reach-into-other-domains` rule per module. It also fails when golangci-lint is **absent** — the wrapper on PATH prints "No issues found" and exits 0 when the binary is not installed |
| `just test` | behaviour, over real Postgres. It also carries `test/arch/arch_test.go`, the only gate on module *direction* (depguard's cross-domain rules are symmetric: they express layering, never direction or acyclicity), and `test/arch/sqltables_test.go`, the only gate on a module reaching across a boundary through SQL table names rather than an import |
| **G1** — Go DTO → OpenAPI (`just dto-check`) | a `*DTO`/`*Request`/`*Query` struct whose json tags, required/optional split, types, enums, nullability or `validate` bounds disagree with [`api/openapi/openapi.yaml`](api/openapi/openapi.yaml). Pure reflection over YAML: no Docker, about a second |
| **G2** — running server → OpenAPI (`just server-check`) | a handler that writes bytes the contract does not describe. Assembles the real container over a real migrated Postgres, calls every declared operation over HTTP, validates status, Content-Type and body against the schema declared for it. Needs Docker |
| **G3** — OpenAPI → TS client (`just generate-check`) | a stale checked-in `web/src/api/generated/schema.d.ts`. G3 protects the *types*, which vanish at build time |
| **G4** — OpenAPI → valibot (`just validators-check`) | a stale `web/src/api/generated/validators.ts`. G4 protects the *runtime* check — the only thing that can notice a server whose body stopped matching the contract |
| `tsc --noEmit` (inside `just ui-build`) | what the generated client cannot: a component reading a field G3 correctly says does not exist |
| `just lint-vocabulary` | the scope boundary drifting in *language* before it drifts in schema (see below) |
| `just lint-reachability` | declarations wired to nothing — a zero-write or zero-read struct field, a json tag that does not round-trip with the contract, an exported `New*` with no caller outside its own file. Debt lives in [`tools/lintreach/baseline.txt`](tools/lintreach/baseline.txt) and can only shrink; `//oto:reachable-ok <reason>` is the escape hatch and an unused one is an error |
| `just helm-check` | a chart that renders as YAML but not as Kubernetes, and a `fail` guard rail whose `.Values` path was renamed out from under it. `helm lint` alone is not the gate: it exits 0 on this chart at defaults, which `helm template` refuses to render. `kubeconform` is what rejects `apiVersion: v1beta9` and a `Deployment` with no `selector` |

**What runs in what order.** CI does not order these gates. `.github/workflows/ci.yml` declares nine
jobs — `go-lint`, `go-build`, `go-test`, `contract-dto`, `contract-server`, `ui`, `vocabulary`,
`helm`, `reachability` — and runs them in parallel; the only dependency anywhere in the file is
`needs: [go-build]` on `go-test` and on `contract-server`. Locally, `just ci` is a single sequence,
verbatim from `justfile` line 440:

```
lint lint-vocabulary lint-reachability helm-check dto-check server-check generate-check validators-check test ui-build ui-test
```

Recommended order when running gates by hand rather than through `just ci`: `just lint` first —
fastest, and its message points straight at the fix — then the four drift gates, seconds each and
Docker-free except G2, then `just test`. A contract failure buried behind a full
`go test -race ./...` is a contract failure found ten minutes late. This is a suggestion for a
contributor's own loop, not a description of CI.

## Testing

Use `just test`. **Never bare `go test ./...`.**

The suite is testcontainers-backed against real Postgres, because mocked databases lie about SQL
semantics. Under colima, testcontainers cannot find the daemon unless told where the socket is, and
colima leaves `DOCKER_HOST` unset. The recipe detects `~/.colima/default/docker.sock`, exports
`DOCKER_HOST` and `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE`, and announces that it did — Docker
Desktop and a pre-set `DOCKER_HOST` are both left alone. Without that, around 300 database-backed
tests fail on a container that never started, deep in the suite, reading like broken code rather
than a missing environment variable. To narrow a run, keep that environment and pass the package or
test yourself:

```bash
DOCKER_HOST="unix://$HOME/.colima/default/docker.sock" \
TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock \
  go test -race -count=1 -run TestName ./internal/alerts/service/
```

`test/load/` sits behind `//go:build load`, so neither `just test` nor CI compiles it in:
`go test -tags load ./test/load/`. A green CI says nothing about those.

### The two Slack golden corpora

There are **two** checked-in corpora of Slack output, with **two different update flags**;
regenerating one leaves the other red.

| Corpus | Regenerate with |
|---|---|
| `internal/channels/render/slack/testdata/*.golden.json` — the renderer's unit fixtures | `go test ./internal/channels/render/slack/ -update` |
| `test/fixtures/slack/*.message.json` + `*.blockkit.json` — the paste-into-Block-Kit-Builder corpus | `go test ./test/harness/ -update-slack-goldens` |

A rendering change touches both. `*.message.json` is the wire truth; `*.blockkit.json` is that
card's blocks lifted out of its attachment, because Block Kit Builder cannot render attachments. The
generic-webhook renderer has a third corpus on its own `-update` flag.

## Interpreting a red gate

- **`just lint-vocabulary` is green on a clean tree, and that is the bar.** Anything it prints is
  new. If the word is genuinely unavoidable — frozen migration history, a foreign system's wire key
  — put `vocab:allow <reason>` on the line or within the two lines above it, so the exemption
  arrives with a reason attached and `-v` can list it.
- **`just test` exits 1 for one failure and for ten.** Compare failure **names** against the
  baseline on `main`, never exit codes. A change that fixes two tests and breaks one is
  indistinguishable from a change that broke nothing, if all you read is the exit code.
- **golangci-lint ignores git worktrees.** It lints the main tree regardless of which worktree you
  invoke it from, so `just ci` cannot gate work isolated in a worktree. Serialize the gates: bring
  the change into the main tree, then run them.
- **A missing tool must fail, not skip.** A check that passes because its checker is absent is worse
  than no check; `just lint` and `deploy/helm/check.sh` both exit non-zero and name `just setup`.

## Database changes

Migrations are goose `.sql` files in `db/migrations/`, and there are **80** of them.

- **Expand/contract only.** Never a destructive migration in one release; assume N and N+1 run
  simultaneously.
- **A migration is never rewritten in place.** Retiring something means a *new* pair — see commit
  `f196060`, which retired a set of columns in new migrations `00078`/`00079` rather than by editing
  `00076`/`00077`. A shipped migration has already run somewhere.
- **Apply with `just migrate`, not raw goose.** It runs `go run ./cmd/oto migrate`, which applies the
  goose schema **and then River's own queue tables**; goose alone leaves River's tables absent and
  the worker dies at boot.
- `just migrate-roundtrip` proves every Down. A `-- +goose Down` must restore the world as it was,
  banned vocabulary included — which is why the vocabulary linter exempts Down sections.
- Constraint and index names are a runtime contract: §L.9 returns them as `errs.Error.Code` on
  23505/23503. `<table>_<purpose>_ck|_uniq|_idx`, no `ck_`/`ix_`/`uq_` prefixes. Every composite
  index starts with `org_id`, every repository method takes `(ctx, db.TenantScope, …)`, and there is
  no `OFFSET` here: list methods take `db.Keyset` and return `db.Cursor`.

## Generated code

Generated from [`api/openapi/openapi.yaml`](api/openapi/openapi.yaml) and **checked in**:
`web/src/api/generated/schema.d.ts` (the TypeScript client, via `openapi-typescript`) and
`web/src/api/generated/validators.ts` (one valibot schema per component schema, via
`web/scripts/gen-validators.mjs`). No hand-written valibot schema may describe an API response — a
form schema must `v.pipe` into the generated request schema. Regenerate with **`just generate`** and
**commit the output**: G3 and G4 regenerate both and fail on any diff, so an uncommitted
regeneration is a red CI. `web/src/components/ensoPaths.ts` is also generated, by `just enso-trace`,
which is deliberately outside `just ci`.

## Architecture rules a change will be refused for

### Module layering

Every domain under `internal/<domain>/` has four layers — `api/`, `service/`, `repository/`,
`domain/` — and these rules are mechanically enforced (CONTEXT.md §5):

1. `api` must not import `repository`; `repository` must not import `api`.
2. `domain` imports no `pgx`, `database/sql`, `net/http`, `os`, `time.Now` (use `platform/clock`),
   `slack-go` or `client-go`. `encoding/json` **is** permitted — it does no I/O. `json:"…"` struct
   tags in `domain` are **forbidden**: a tag is what quietly turns a domain type into a DTO.
3. `internal/alerts/domain` is the shared domain kernel and the only sanctioned cross-domain
   `domain` import. `notification` may also import `channels/domain`, view types only. There are
   exactly two such grants, and a third needs an ADR.
4. `internal/platform/tuning` is the one home of the shipped §D.1 tuning defaults — constants only,
   importing nothing but `time`. `platform` may import no domain at all.
5. `service` imports `domain` and the ports it declares itself; concretes are injected by
   `internal/app/container.go`, the one place allowed to know every module.
6. Cross-domain calls are **service → service**, through an interface declared by the **consumer**.
   Never repository → repository.
7. Three model sets the compiler can tell apart: `api.XxxDTO` ← `domain.Xxx` ←
   `repository.xxxRow` (unexported). **No DTO may embed a domain type or a row type.**
8. Transactions travel in `ctx` (`db.FromContext`). There are no `WithTx(tx)` variants.

`alerts` imports no other module: everything it needs from `enrichment`, `notification` and
`streaming` is a port it declares in `alerts/service/deps.go`. That is what lets oto run with
notifications entirely disabled.

### The banned vocabulary

The scope boundary always drifts in *language* before it drifts in schema: a word arrives, then the
concept, then the column. **AC-49** is the acceptance criterion in
[SCOPE-BOUNDARY.md](/oto/design/SCOPE-BOUNDARY/), restated as SPEC §P-18, that bans a fixed list
of words and a fixed list of person-subject column names across `internal/`, `web/src/` and
`db/migrations/` and requires a lint rule to enforce it. [`tools/lintvocab`](tools/lintvocab/) is
that rule — nineteen patterns.

The shape of the rule: 13 banned concept words and 6 forbidden person-subject column names, matched
on stems rather than exact spellings, so an inflection is the same violation as the root. The
enumeration itself is stated in exactly one place —
[docs/concepts.md § 6, The boundary](/oto/concepts/#6-the-boundary) — and is not repeated here,
because three copies of a list drift. A word can be renamed; a column has rows, which is why the two
halves are separate lists rather than one.

Comments are stripped before matching, because this codebase's comments name the banned words
constantly and every such mention forbids the concept. What is scanned is what the product *is*:
identifiers, literals and DDL — including SQL string literals, since a `COMMENT ON COLUMN` ships to
every operator. `vocab:allow <reason>` on the line, or within the two lines above it, suppresses
one; the reason is printed by `-v`. Known debt lives in
[`tools/lintvocab/baseline.txt`](tools/lintvocab/baseline.txt), a violation outside it fails
immediately, and a baseline line matching nothing is itself an error.

Paging, routing an alert to a person, multi-stage notification ladders, status pages, retrospectives
and per-person response-time metrics are **permanently out of scope**. Adding one needs an ADR
arguing against FR-1 by name.

## Commit and change conventions

Conventional-commit type, optional scope, `!` for a breaking change, then a **lowercase descriptive
clause stating what is now true** — not an imperative, not a summary of the diff. The subject reads
as a sentence somebody could disagree with:

```
fix(db)!: retire wordings in NEW migrations 00078/00079, not by rewriting 00076/00077
feat(web): the retry button moves to the log, and the log can afford to offer it
fix(config): `http.base_url` was the only unvalidated dependency of two operator-facing surfaces
```

Scopes in the log: `web`, `db`, `api`, `slack`, `notification`, `channels`, `identity`, `config`,
`contract`, `harness`, `templates`, `adr`; `docs:` and `feat:` also appear unscoped. The `afk:`
prefix marks an unattended agent run and is not a conventional-commit type. In the body, name the
git-bug id and the migration number when either is involved, state the measurement rather than the
intent, and say what was *deleted* as plainly as what was added. An ADR changes in the same commit
as the SPEC text it amends. Before opening a change, run `just ci`, compare the failure names
against the `main` baseline above, and say which gates you ran and which you could not.

## Documentation

[`docs-site/scripts/sync-docs.mjs`](docs-site/scripts/sync-docs.mjs) copies `README.md`,
`CONTEXT.md`, this file, `docs/concepts.md`, `docs/releasing.md`, `docs/ORCHESTRATION.md` and
everything under `docs/adr/`, `docs/design/`, `docs/setup/` and `docs/runbooks/` into an
Astro/Starlight site, adding the frontmatter Starlight needs and rewriting `*.md` links to clean
URLs. It runs on `npm run dev` and `npm run build` inside
`docs-site/` (`just docs`). Two things follow from that:

- **The site is built, link-checked and deployed by CI.**
  [`.github/workflows/pages.yml`](.github/workflows/pages.yml) publishes it to
  <https://thulasi-ram.github.io/oto> on every push to `main` that touches a source doc. It builds
  from `docs/` rather than from the committed mirror, so a missed sync deploys the right page — but
  re-sync anyway, because the mirror is what makes a docs change reviewable. `just docs-check` is
  the same build and the same link gate, locally.
- **It is served from a path, not a domain root.** `docs-site/site.config.mjs` holds `base` and is
  read by both the Astro config and the sync script, because Astro prefixes only the URLs it
  generates and never an href written into Markdown. Setting `base` in the Astro config alone gives
  a site whose nav works and whose in-content links all 404.
- **The generated pages under `docs-site/src/content/docs/` are committed**, so they can go stale
  against their sources. The sync script also removes `adr/`, `design/`, `setup/` and `runbooks/`
  before regenerating, which destroys any page in those four directories with no source under
  `docs/`.

A file added under `docs/adr/`, `docs/design/`, `docs/setup/` or `docs/runbooks/` is picked up by
the directory listing and needs nothing else. A new top-level source — this document, `README.md`,
`CONTEXT.md`, `docs/releasing.md` — has to be added to `SOURCES` in the script **and** to the
Overview group in [`docs-site/astro.config.mjs`](docs-site/astro.config.mjs), or it syncs to a route
with no way to navigate to it.
