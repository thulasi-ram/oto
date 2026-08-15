package schema

import (
	"errors"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The tests in this file guard the GUARD.
//
// Every other test in the api packages delegates its shape assertion to this
// package. If the loader silently resolved nothing — a mistyped pointer, a
// `$ref` that stopped resolving, a schema that failed to compile and was
// skipped — those assertions would all still pass, and the suite would be a
// very convincing way of checking nothing at all. That is the exact failure
// mode the conformance audit found in the API itself, and it would be
// embarrassing to reproduce it here.

// ⭐ TestEveryDeclaredResponseSchemaCompiles walks the whole contract and
// compiles every response schema it declares.
//
// It is the cheapest possible guarantee that `schema.Assert` can never fail
// open: if a pointer does not resolve or a schema does not compile, it fails
// HERE, once, with the operation and status named — rather than in whichever
// handler test happened to reach it first, or nowhere at all.
func TestEveryDeclaredResponseSchemaCompiles(t *testing.T) {
	t.Parallel()

	ops := Operations(t)
	if len(ops) == 0 {
		t.Fatal("the contract declares no operations; the loader resolved nothing")
	}

	var bodies, bodiless int
	for _, op := range ops {
		for _, status := range op.Statuses {
			jsonErr := probe(op.ID, status, MediaTypeJSON)
			problemErr := probe(op.ID, status, "application/problem+json")

			switch {
			case jsonErr == nil || problemErr == nil:
				bodies++
			case errors.Is(jsonErr, ErrNoBody) && errors.Is(problemErr, ErrNoBody):
				// A 204, or a response whose body is not JSON (SSE, the
				// Prometheus text exposition). Nothing to compile.
				bodiless++
			default:
				t.Errorf("%s %d (%s %s): the contract declares a body that will not compile:\n  json:    %v\n  problem: %v",
					op.ID, status, op.Method, op.Path, jsonErr, problemErr)
			}
		}
	}
	t.Logf("%d operations, %d compilable response bodies, %d bodiless responses", len(ops), bodies, bodiless)
}

func probe(op string, status int, mediaType string) error {
	_, err := Response(op, status, mediaType)
	return err
}

// TestEveryOperationHasAtLeastOneSuccessAndOneRefusal.
//
// An operation that declares only a 200 is an operation whose failure modes
// nobody wrote down, which means a client has nothing to branch on and a
// handler has no agreed way to say no.
func TestEveryOperationHasAtLeastOneSuccessAndOneRefusal(t *testing.T) {
	t.Parallel()

	// The unauthenticated ops probes are the deliberate exception: `/healthz`
	// is either answering or the process is gone, and `/metrics` and
	// `/openapi.json` are static. There is nothing for them to refuse.
	alwaysAnswers := map[string]string{
		"getLiveness":        "a liveness probe that can fail is a restart loop",
		"getMetrics":         "static exposition; there is no request to refuse",
		"getOpenapiDocument": "static document; there is no request to refuse",
		"getVersion":         "answers before anyone has logged in, so it cannot 401",
	}

	for _, op := range Operations(t) {
		if op.SuccessStatus() == 0 {
			t.Errorf("%s (%s %s) declares no 2xx", op.ID, op.Method, op.Path)
		}
		if _, exempt := alwaysAnswers[op.ID]; exempt {
			continue
		}
		if !hasRefusal(op) {
			t.Errorf("%s (%s %s) declares no 4xx or 5xx: %v", op.ID, op.Method, op.Path, op.Statuses)
		}
	}
}

func hasRefusal(op Operation) bool {
	for _, s := range op.Statuses {
		if s >= 400 {
			return true
		}
	}
	return false
}

// ⛔ TestTheValidatorActuallyRejects.
//
// A validator that accepts everything passes every test in this repository and
// protects nothing. These are the three drifts the conformance audit actually
// found, replayed as bodies, and each one must be REFUSED.
func TestTheValidatorActuallyRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		op     string
		status int
		body   string
		want   string
	}{
		{
			// The audit's own C-finding: `getVersion` answered a bare object.
			name:   "a response with no {data,meta} envelope",
			op:     "getVersion",
			status: 200,
			body:   `{"version":"1.0.0","commit":"abc","schema_version":"00011"}`,
			want:   "missing properties",
		},
		{
			name:   "an envelope missing a required member of its data",
			op:     "getVersion",
			status: 200,
			body:   `{"data":{"version":"1.0.0"},"meta":{"request_id":"01JD8Z2K7M3TQ9"}}`,
			want:   "missing propert",
		},
		{
			// `format:` is asserted, so a timestamp that is not one is caught.
			// A DTO that started emitting unix seconds would otherwise pass as
			// "a string" forever.
			name:   "a timestamp that is not a timestamp",
			op:     "getVersion",
			status: 200,
			body:   `{"data":{"version":"1.0.0","commit":"abc","schema_version":"00011","built_at":"yesterday"},"meta":{"request_id":"01JD8Z2K7M3TQ9"}}`,
			want:   "date-time",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sch, err := Response(tc.op, tc.status, MediaTypeJSON)
			if err != nil {
				t.Fatalf("resolve %s %d: %v", tc.op, tc.status, err)
			}
			v, err := unmarshal(tc.body)
			if err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			err = sch.Validate(v)
			if err == nil {
				t.Fatalf("the validator ACCEPTED %s; it protects nothing", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal does not mention %q, so a failure would not explain itself:\n%v", tc.want, err)
			}
		})
	}
}

// TestTheValidatorAcceptsAWellFormedBody is the other half: a validator that
// rejects everything is just as useless as one that accepts everything.
func TestTheValidatorAcceptsAWellFormedBody(t *testing.T) {
	t.Parallel()

	sch, err := Response("getVersion", 200, MediaTypeJSON)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	v, err := unmarshal(`{"data":{"version":"1.0.0","commit":"9f2c1a7b","schema_version":"00011",` +
		`"go_version":"go1.24.0","built_at":"2026-03-01T12:00:00Z"},` +
		`"meta":{"request_id":"01JD8Z2K7M3TQ9","elapsed_ms":14}}`)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if err := sch.Validate(v); err != nil {
		t.Fatalf("a well-formed VersionResponse was refused:\n%v", err)
	}
}

// TestAnUnknownOperationIsAnError, so that a test naming an operationId the
// contract no longer declares fails loudly instead of asserting nothing.
func TestAnUnknownOperationIsAnError(t *testing.T) {
	t.Parallel()

	if _, err := Lookup("thisOperationWasRenamedTwoQuartersAgo"); err == nil {
		t.Fatal("an unknown operationId resolved; a stale test would silently assert nothing")
	}
	if _, err := Response("getSource", 418, MediaTypeJSON); err == nil {
		t.Fatal("an undeclared status resolved")
	}
}

// TestOperationsAreDeterministic, because a table-driven suite over the whole
// API must produce the same subtests every run or a flake cannot be bisected.
func TestOperationsAreDeterministic(t *testing.T) {
	t.Parallel()

	first := Operations(t)
	second := Operations(t)
	if len(first) != len(second) {
		t.Fatalf("two reads gave %d and %d operations", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("operation %d is %q then %q; the order is not stable", i, first[i].ID, second[i].ID)
		}
	}
}

func unmarshal(s string) (any, error) {
	return jsonschema.UnmarshalJSON(strings.NewReader(s))
}
