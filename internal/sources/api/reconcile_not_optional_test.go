package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// ⭐⭐ TestReconcileCannotBeSwitchedOff is ADR 0006 made mechanical.
//
// THE DEFECT IT PINS. `alert_sources.reconcile_enabled` was a settable contract
// field, and `PATCH /api/v1/sources/{id} {"reconcile_enabled": false}` returned
// 200 and persisted. The reconciler is the ONLY producer of `suppressed` — nothing
// silenced ever reaches a webhook, because Alertmanager's MuteStage drops it — AND
// the only thing that refreshes `source_health`, whose two writers are the
// reconcile pass and the manual probe. So a source with that flag off kept a
// FROZEN `healthy` verdict, the §B.4 reaper guard went on trusting it, and an
// alert a colleague had merely silenced stopped arriving, aged past
// `resolve_grace`, and was ended as `expired` / `resolve_reason='timeout'`. One
// PATCH, by any org member, and the timeline records an ending that never
// happened.
//
// ⛔ THE REFUSAL IS BY NAME, AND THAT IS DELIBERATE. The body decodes with
// DisallowUnknownFields and the schema is `additionalProperties: false`, so a
// caller whose runbook or Terraform still carries the field gets a violation that
// says `reconcile_enabled`, not a 200 that quietly ignored it. Silently dropping
// it would leave somebody believing they had turned reconciliation off — the same
// lie, told to the operator instead of the database.
func TestReconcileCannotBeSwitchedOff(t *testing.T) {
	t.Parallel()

	rt, deps := newTestRouter(t)

	rec := doPatch(t, rt, uuid.New(), map[string]any{"reconcile_enabled": false})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; the reconciler is not optional (ADR 0006): %s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reconcile_enabled") {
		t.Fatalf("the refusal must NAME the field so a stale runbook finds out: %s", rec.Body.String())
	}
	if deps.writes.updated != 0 {
		t.Fatal("a refused patch still reached the write path")
	}
}

// The create path is the other door onto the same column, and it has to be shut
// too: a source registered with reconciliation off would never once be polled, so
// it would sit at health `unknown` — which blocks the reaper — while looking
// configured on the settings screen.
func TestReconcileCannotBeSwitchedOffAtCreate(t *testing.T) {
	t.Parallel()

	rt, deps := newTestRouter(t)

	rec := doCreate(t, rt, map[string]any{
		"name":              "prod-eu",
		"cluster_id":        uuid.New().String(),
		"kind":              "alertmanager",
		"base_url":          "https://am.example.com",
		"reconcile_enabled": false,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reconcile_enabled") {
		t.Fatalf("the violation should name the field: %s", rec.Body.String())
	}
	if deps.writes.created != 0 {
		t.Fatalf("a refused create still reached the write path %d time(s)", deps.writes.created)
	}
}

// ⭐ THE OTHER HALF OF THE DECISION, AND THE REASON IT IS SURVIVABLE. Removing the
// on/off switch would be a hardship if it also removed the ability to poll a slow,
// distant or rate-limited Alertmanager gently. It does not: `reconcile_interval_s`
// spans 10 s to an hour and is untouched. "How often" stays a choice; "whether"
// never was one.
func TestReconcileIntervalStaysTunable(t *testing.T) {
	t.Parallel()

	rt, _ := newTestRouter(t)

	rec := doPatch(t, rt, uuid.New(), map[string]any{"reconcile_interval_seconds": 3600})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; the interval must remain tunable: %s",
			rec.Code, rec.Body.String())
	}

	// And its bounds still bite, so "tunable" cannot become "off by another name":
	// there is no value of this field that means never.
	rec = doPatch(t, rt, uuid.New(), map[string]any{"reconcile_interval_seconds": 0})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for a zero interval: %s", rec.Code, rec.Body.String())
	}
}

// A source on the wire no longer advertises a switch that does not exist. A client
// that still renders one would be offering an operator a control that cannot be
// set, which is how the field survived the last review.
func TestSourceResponseCarriesNoReconcileToggle(t *testing.T) {
	t.Parallel()

	rt, _ := newTestRouter(t)
	rec := doCreate(t, rt, map[string]any{
		"name":       "prod-eu",
		"cluster_id": uuid.New().String(),
		"kind":       "alertmanager",
		"base_url":   "https://am.example.com",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Data struct {
			Source map[string]json.RawMessage `json:"source"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if _, present := body.Data.Source["reconcile_enabled"]; present {
		t.Fatal("SourceDTO still carries reconcile_enabled; the contract and ADR 0006 disagree again")
	}
	if _, present := body.Data.Source["reconcile_interval_seconds"]; !present {
		t.Fatal("the interval — the one reconciliation setting there is — is missing from the response")
	}
}

func doPatch(
	t *testing.T, rt *Router, id uuid.UUID, body map[string]any,
) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/sources/"+id.String(), strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	return serve(rt, req)
}
