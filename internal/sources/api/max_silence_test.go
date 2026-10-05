package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestMaxSilenceIsDefaultedOnOrOffAtCreate — ADR 0056 §3. `max_silence_seconds`
// is `integer | null` and the two absences mean different things: an omitted
// field takes the DDL's default of a day, and an explicit null turns the `silent`
// expiry off. A plain `*int32` could not tell them apart, which is why the field
// is a NullableInt32.
func TestMaxSilenceIsDefaultedOnOrOffAtCreate(t *testing.T) {
	t.Parallel()

	base := func(extra map[string]any) map[string]any {
		body := map[string]any{
			"name":       "prod-eu",
			"cluster_id": uuid.New().String(),
			"kind":       "alertmanager",
			"base_url":   "https://am.example.com",
		}
		for k, v := range extra {
			body[k] = v
		}
		return body
	}

	for _, tc := range []struct {
		name string
		body map[string]any
		want *time.Duration
	}{
		{"omitted takes a day", base(nil), ptrDuration(24 * time.Hour)},
		{"null turns it off", base(map[string]any{"max_silence_seconds": nil}), nil},
		{"a number sets it", base(map[string]any{"max_silence_seconds": 7200}), ptrDuration(2 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt, deps := newTestRouter(t)
			rec := doCreate(t, rt, tc.body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
			}
			got := deps.writes.lastCreate.Draft.MaxSilence
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("max_silence = %s, want off", *got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Fatalf("max_silence = %v, want %s", got, *tc.want)
			}
		})
	}
}

// TestMaxSilenceIsBoundedByName — `alert_sources_silence_ck` at layer 2. Below an
// hour is refused rather than expiring every live Case on the next tick, and the
// violation names the field the caller sent.
func TestMaxSilenceIsBoundedByName(t *testing.T) {
	t.Parallel()

	rt, deps := newTestRouter(t)
	rec := doCreate(t, rt, map[string]any{
		"name":                "prod-eu",
		"cluster_id":          uuid.New().String(),
		"kind":                "alertmanager",
		"base_url":            "https://am.example.com",
		"max_silence_seconds": 60,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "max_silence_seconds") {
		t.Fatalf("the violation must name the field: %s", rec.Body.String())
	}
	if deps.writes.created != 0 {
		t.Fatal("a refused create still reached the write path")
	}

	rec = doPatch(t, rt, uuid.New(), map[string]any{"max_silence_seconds": 2592001})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 above thirty days: %s", rec.Code, rec.Body.String())
	}
	if deps.writes.updated != 0 {
		t.Fatal("a refused patch still reached the write path")
	}
}

// TestMaxSilencePatchesLeaveTurnOffOrSet — on PATCH the absences mean LEAVE ALONE
// and OFF, and a body carrying only this field is not an empty patch.
func TestMaxSilencePatchesLeaveTurnOffOrSet(t *testing.T) {
	t.Parallel()

	rt, deps := newTestRouter(t)

	rec := doPatch(t, rt, uuid.New(), map[string]any{"max_silence_seconds": nil})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if p := deps.writes.lastUpdate.Patch.MaxSilence; p == nil || *p != nil {
		t.Fatalf("an explicit null must reach the patch as OFF, got %v", p)
	}

	rec = doPatch(t, rt, uuid.New(), map[string]any{"max_silence_seconds": 172800})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if p := deps.writes.lastUpdate.Patch.MaxSilence; p == nil || *p == nil || **p != 48*time.Hour {
		t.Fatalf("a number must reach the patch as that duration, got %v", p)
	}

	rec = doPatch(t, rt, uuid.New(), map[string]any{"name": "renamed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if p := deps.writes.lastUpdate.Patch.MaxSilence; p != nil {
		t.Fatal("a patch that does not name max_silence_seconds must leave it alone")
	}
}

func ptrDuration(d time.Duration) *time.Duration { return &d }
