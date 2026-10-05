package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestASourcesClusterCannotBeChanged — owner ruling R3 (2026-10-05). A source's
// cluster is which Cases it speaks for: the reaper's §B.4 guard, the `silent`
// threshold and `source_removed` are all questions about the live sources on a
// Case's cluster. Moving a source would orphan one cluster's Cases — expired as
// `source_removed` a resolve grace later — and hand another cluster a witness to
// alerts it never carried.
//
// ⛔ THE REFUSAL IS BY NAME, exactly as `kind`'s is: the body decodes with
// DisallowUnknownFields and the schema is `additionalProperties: false`, so a
// caller still sending the field gets a violation that says `cluster_id`, not a
// 200 that quietly moved the source or quietly ignored the request.
func TestASourcesClusterCannotBeChanged(t *testing.T) {
	t.Parallel()

	for _, body := range []map[string]any{
		{"cluster_id": uuid.New().String()},
		{"cluster_id": uuid.New().String(), "name": "renamed-too"},
		{"kind": "grafana"},
	} {
		rt, deps := newTestRouter(t)

		rec := doPatch(t, rt, uuid.New(), body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("PATCH %v: status = %d, want 422 — cluster_id and kind are immutable: %s",
				body, rec.Code, rec.Body.String())
		}
		field := "cluster_id"
		if _, ok := body["kind"]; ok {
			field = "kind"
		}
		if !strings.Contains(rec.Body.String(), field) || !strings.Contains(rec.Body.String(), "unknown_field") {
			t.Fatalf("PATCH %v: the refusal must NAME %s as an unknown field: %s", body, field, rec.Body.String())
		}
		if deps.writes.updated != 0 {
			t.Fatalf("PATCH %v: a refused patch still reached the write path", body)
		}
	}
}
