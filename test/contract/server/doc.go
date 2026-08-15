// Package server is gate G2 of SPEC §L.8.1: the RUNNING SERVER against the
// OpenAPI contract.
//
// # What this gate asserts
//
// A real `internal/app.Container` is assembled over a real, fully migrated
// Postgres (test/harness), mounted on a real `httptest.Server`, and driven over
// real HTTP with a real credential minted by the real `oto bootstrap` path.
// Every declared operation in `api/openapi/openapi.yaml` is called, and for each
// response the gate asserts three things:
//
//  1. the status the server chose is one the contract declares for that
//     operation, AND the one this probe expected;
//  2. the Content-Type the server wrote is one the contract declares for that
//     operation and status;
//  3. the BYTES the handler wrote validate against the schema the contract
//     declares for that operation, status and media type.
//
// Point (3) is the whole gate. G1 (`test/contract/dto_schema_test.go`) proves
// the Go DTO STRUCTS agree with the contract; it cannot prove that the handler
// wrapping one in an envelope, or hand-rolling a map, or serving a 500 with a
// bare string, produces bytes the contract permits. Every finding in the
// conformance audit was of exactly that shape.
//
// # Why this is Go and not schemathesis
//
// SPEC §L.8.1 names `schemathesis`. This gate deviates, deliberately, and the
// argument is:
//
//   - schemathesis is Python. This repository's toolchain is Go and Node, and
//     CI installs exactly those two. A gate is only worth what it costs to keep
//     green, and a third runtime — a pinned interpreter, a lockfile, a
//     dependency tree nobody here reads — is a standing maintenance cost paid by
//     every contributor for one check.
//
//   - schemathesis needs a server it can reach, and oto's server is not a
//     process you can start with a flag. It needs a migrated Postgres, a
//     bootstrapped org, a credential, and fakes standing in for the Slack Web
//     API, Alertmanager v2 and outbound webhooks. That bootstrap already exists,
//     in Go, in `test/harness` (ADR 0021). Driving it from Python means either
//     exporting a bootstrap CLI or composing the whole world in CI — both of
//     which are more machinery than the gate they serve.
//
//   - The assertion itself needs no Python at all. OpenAPI 3.1 schemas ARE JSON
//     Schema 2020-12, and `test/contract/schema` already compiles the contract
//     as such — from the SAME embedded bytes the server publishes at
//     `GET /openapi.json`, so the gate can never pass against a copy on disk the
//     server does not serve.
//
// What is genuinely lost is schemathesis's FUZZED REQUEST GENERATION. Two things
// stand in for it. First, exhaustiveness is not optional here:
// TestEveryDeclaredOperationIsDriven fails when the contract declares an
// operation this file does not call, so an operation cannot be added without a
// probe. Second, the probe table includes the negative cases a generator would
// find on its own — no credential, another tenant's id, a malformed body, a body
// the contract forbids — because those responses are the ones whose shape drifts
// first and is noticed last.
//
// # Why its own package
//
// It is NOT in `test/contract` with G1. G1 is a pure reflection-and-YAML test
// that runs in milliseconds and needs no Docker; putting a testcontainers
// Postgres in the same package would make every G1 iteration pay for one.
package server
