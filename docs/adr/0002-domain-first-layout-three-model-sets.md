# 0002 — `internal/<domain>/{api,service,repository,domain}` with three model sets

**Status:** Accepted · 2026-08-07

## Context
The owner mandated a domain-first layout with API → Service → Repository layering and three
distinct model sets (DTO / domain / row), written as `src/<domain>/…`. Go has no `src/`
convention, and layering rules that are not mechanically enforced decay within a quarter.

Five independent implementation agents will each build one module. Their code must compile
together and must not grow accidental cross-domain coupling.

## Decision
Layout is `internal/<domain>/{api,service,repository,domain}`. The innermost package is named
`domain`, not `model`. Shared importable primitives live in `pkg/`.

Every domain object exists in exactly three shapes, and the compiler can tell them apart:

```
HTTP/JSON  -> api.XxxDTO           (json + validate tags, versioned)
business   -> domain.Xxx           (pure Go, invariants, no I/O library imports)
storage    -> repository.xxxRow    (unexported, nullable types)
```

Hard rules, enforced by `golangci-lint` + `depguard` + an arch test:
- `<d>/api` MUST NOT import `<d>/repository`; `<d>/repository` MUST NOT import `<d>/api`.
- `<d>/domain` imports neither, and imports no I/O library at all (no `pgx`, no `net/http`).
- `<d>/service` imports `domain` and **port interfaces declared by itself**; concretes are injected.
- Cross-domain calls are **service → service**, through an interface declared by the *consumer*.
  Never repository → repository.
- Every repository method takes `ctx` then a `db.TenantScope` whose only constructor requires an
  authenticated principal.

## Consequences
- `internal/` gives compiler-enforced encapsulation — nothing outside the module can import it,
  which is exactly the boundary discipline the owner asked for. This is a **naming deviation from
  the literal `src/`**, flagged for the owner.
- Five agents can work in parallel: each owns one directory, and the ports in SPEC §F are the
  only contract between them.
- Some mapping boilerplate is unavoidable. It is the price of the API not leaking the schema and
  of the schema not leaking into the API. sqlc/ORM entities would quietly become *the* model and
  the three-model rule would die.

## Alternatives rejected
- **Literal `src/<domain>/…`:** identical shape, no compiler-enforced encapsulation.
- **Layer-first (`handlers/`, `services/`, `models/`):** every feature touches every directory;
  boundaries are conventions, not walls.
- **`ent` or `gorm`:** the generated entity becomes the domain model and the three-model rule
  collapses. `ent`'s graph traversal is superb and not what this codebase needs.
