---
title: 0011 — Validation is seven layers with two opposite trust models
---
**Status:** Accepted · 2026-08-08

## Context
"Validate the input" is one sentence covering at least seven genuinely different problems in oto,
and the two most important of them want **opposite** behaviour.

An API request body is written by an authenticated user who can read the error and fix it.
Rejecting it loudly with a precise field path is the correct, kind response.

An Alertmanager webhook body is written by software oto does not control, over a protocol whose
published documentation is incomplete (`routeLabels` is on the wire and absent from the docs), and
whose retry classifier **permanently deletes** any notification answered with a 4xx. Applying
API-style strictness there — `DisallowUnknownFields`, reject-on-first-violation, 400 on a bad
label — silently destroys alerts during the exact window when the customer's cluster is on fire.

A single shared "validation layer" is therefore not a simplification; it is a defect.

Separately: bounds get written down three times (a struct tag, a domain constructor, a DDL CHECK)
and drift. When they drift, a 422 becomes a 500 — the user is told "internal error" for something
they could have fixed.

## Decision
Seven layers, each with a named library, a named trust model and a named failure shape (SPEC §L).

1. **API DTOs** — `go-playground/validator/v10`, invoked only through `httpx.Bind[T]` so no
   handler can forget. `RegisterTagNameFunc` makes every reported path a **JSON** name.
   `DisallowUnknownFields` is ON. Result: 422 + RFC 9457 `violations[]` of `{field, code, message}`,
   with a closed tag→code map.
2. **Untrusted inbound** — hand-written bounds (B1–B17, every value literal). Lenient decode,
   hard bounds, **per-alert** rejection into `ingest_rejections`, and **still 202**. The only
   permitted 4xx are 401, 413 and undecodable-400, all genuinely permanent. Redaction runs
   *before* the raw persist.
3. **Domain invariants** — value objects with unexported fields and `New…() (T, error)`
   constructors; a single total `Transition` function is the only way an occurrence changes state.
   There is no optional `Validate()` anywhere. Illegal states are unrepresentable.
4. **Provider config** — `santhosh-tekuri/jsonschema/v6`, draft 2020-12, compiled from `embed.FS`
   at boot. **The same bytes** validate server-side and render the settings form, so there is one
   source of truth and the UI needs no code when a provider is added.
5. **Outbound render** — 18 explicit Block Kit checks before any Slack call. A violation is a
   `dead` delivery with the offending payload persisted, never a truncated send.
6. **Persistence** — CHECK/NOT NULL/UNIQUE/FK as the last line of defence. A CHECK violation
   reaching HTTP is a **500 and an alert**, because it means layers 1–3 have a hole.
7. **Frontend** — `valibot` (tree-shakeable, ~10× smaller than zod), used for forms *and* for
   parsing API responses. Responses parse `looseObject` (additive server changes must not break a
   deployed UI); forms parse `strictObject`.

Drift is closed by four CI gates: Go DTOs → OpenAPI, server → OpenAPI (schemathesis), OpenAPI →
TS types, OpenAPI → valibot — plus `TestValidatorMatchesDDL`, which asserts every custom-rule
regex is byte-identical to its DDL CHECK.

The repository layer **never** validates a business rule (that is the service's job; duplicating
it produces two subtly different rulebooks) but **does** reject malformed row models and is the
single place SQLSTATEs become `errs.Kind`.

## Consequences
- A bound now lives in three files, and changing one without the others fails CI. That friction is
  the feature: it is what stops a 422 silently becoming a 500.
- Error responses are uniform enough that the SolidJS form layer maps `violations[].field` onto
  controls mechanically, with no per-endpoint code.
- The ingest path has a property test (`TestIngestNever4xx`) and a fuzz target, because "we
  returned 400 once" is a data-loss incident, not a bug report.
- Seven layers is more machinery than a small service needs. It is justified here because oto's
  entire premise is "never lies", and because half the input is hostile by construction.

## Alternatives rejected
- **One shared validation layer:** would apply user-input strictness to Alertmanager payloads and
  delete alerts. Non-viable.
- **`ozzo-validation` / hand-rolled:** nicer for conditional rules, but far less ecosystem support
  and no `RegisterTagNameFunc` equivalent, which is what makes JSON field paths free.
- **`zod` on the frontend:** ubiquitous, but 3–4× the bundle for identical ergonomics on a
  dashboard where the alert table is already the bundle budget.
- **Generating Go DTOs from OpenAPI (`oapi-codegen`):** would remove gate G1, but reintroduces the
  codegen coordination dependency between independent implementation agents that C19/C20 removed.
  A drift *test* is cheaper than a drift *generator* here.
- **Validating only at the edge:** the classic mistake. It makes every internal caller a trusted
  caller, and workers (reconciler, reaper, notifier) are internal callers that write domain state.
