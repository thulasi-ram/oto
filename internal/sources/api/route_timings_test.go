package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/sources/domain"
)

// The wire half of the three provenance states.
//
// A client renders whatever this mapper emits, so the guarantee that matters is
// on the JSON: a `default_applies` field must be indistinguishable from
// `observed` in NOTHING, and an `unknown` field must carry no number at all.

func timings(t *testing.T, h domain.SourceHealth) map[string]any {
	t.Helper()

	raw, err := json.Marshal(healthDTO(h))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out struct {
		RouteTimings map[string]any `json:"route_timings"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.RouteTimings == nil {
		t.Fatal("route_timings is absent from the wire; it is required and always present, " +
			"because `unknown` is a state the object carries rather than a reason to omit it")
	}
	return out.RouteTimings
}

func field(t *testing.T, tm map[string]any, name string) map[string]any {
	t.Helper()

	f, ok := tm[name].(map[string]any)
	if !ok {
		t.Fatalf("%s is %T on the wire, want an object with a provenance", name, tm[name])
	}
	return f
}

func dur(v time.Duration) *time.Duration { return &v }

// TestASourceNobodyHasProbedIsUnknownOnTheWire.
func TestASourceNobodyHasProbedIsUnknownOnTheWire(t *testing.T) {
	t.Parallel()

	tm := timings(t, domain.SourceHealth{Status: domain.HealthUnknown})

	for _, name := range []string{"group_wait", "group_interval", "repeat_interval"} {
		f := field(t, tm, name)
		if f["provenance"] != string(domain.TimingUnknown) {
			t.Fatalf("%s provenance = %v, want unknown", name, f["provenance"])
		}
		if f["value_ms"] != nil {
			t.Fatalf("%s carries %v for a config oto has never read", name, f["value_ms"])
		}
	}
	if tm["observed_at"] != nil {
		t.Fatalf("observed_at = %v when nothing was ever observed", tm["observed_at"])
	}
	if tm["defaults_from_version"] != nil {
		t.Fatalf("defaults attributed to %v when nothing defaulted", tm["defaults_from_version"])
	}
}

// TestAStockAlertmanagerReportsDefaultAppliesRatherThanUnknown.
//
// ⭐ THE CASE THE FEATURE EXISTS FOR. `alertmanager.yml` states none of the
// three, oto parsed it successfully, and every field is therefore governed by
// Alertmanager's documented default. The number is present so the guidance can do
// arithmetic; the label is present so the guidance can say whose number it is.
func TestAStockAlertmanagerReportsDefaultAppliesRatherThanUnknown(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	tm := timings(t, domain.SourceHealth{
		Status:         domain.HealthHealthy,
		AMVersion:      "0.28.1",
		RouteTimings:   domain.RouteTimings{ChildRoutes: 1, ChildrenWithTimings: 1},
		RouteTimingsAt: &at,
	})

	want := map[string]float64{
		"group_wait":      float64(domain.DefaultGroupWait.Milliseconds()),
		"group_interval":  float64(domain.DefaultGroupInterval.Milliseconds()),
		"repeat_interval": float64(domain.DefaultRepeatInterval.Milliseconds()),
	}
	for name, ms := range want {
		f := field(t, tm, name)
		if f["provenance"] != string(domain.TimingDefaultApplies) {
			t.Fatalf("%s provenance = %v, want default_applies — this is a stock config and "+
				"reporting it as unknown is what made the tuning screen useless", name, f["provenance"])
		}
		if f["value_ms"] != ms {
			t.Fatalf("%s value_ms = %v, want %v", name, f["value_ms"], ms)
		}
	}
	if tm["defaults_from_version"] != "0.28.1" {
		t.Fatalf("defaults_from_version = %v, want the version the source reported",
			tm["defaults_from_version"])
	}
	if tm["defaults_verified"] != true {
		t.Fatalf("defaults_verified = %v for the release oto pinned them to", tm["defaults_verified"])
	}
	if tm["observed_at"] == nil {
		t.Fatal("observed_at is null although oto DID read this configuration; that null is " +
			"exactly what separates default_applies from unknown")
	}
	if tm["child_routes_with_timings"] != float64(1) {
		t.Fatalf("child_routes_with_timings = %v, want 1 — the v1 caveat must survive",
			tm["child_routes_with_timings"])
	}
}

// TestAnObservedValueIsNeverLabelledAsADefault.
func TestAnObservedValueIsNeverLabelledAsADefault(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	tm := timings(t, domain.SourceHealth{
		Status:    domain.HealthHealthy,
		AMVersion: "0.28.1",
		RouteTimings: domain.RouteTimings{
			GroupWait:      dur(10 * time.Second),
			GroupInterval:  dur(30 * time.Second),
			RepeatInterval: dur(4 * time.Hour),
		},
		RouteTimingsAt: &at,
	})

	for name, ms := range map[string]float64{
		"group_wait": 10_000, "group_interval": 30_000, "repeat_interval": 14_400_000,
	} {
		f := field(t, tm, name)
		if f["provenance"] != string(domain.TimingObserved) {
			t.Fatalf("%s provenance = %v, want observed", name, f["provenance"])
		}
		if f["value_ms"] != ms {
			t.Fatalf("%s value_ms = %v, want %v", name, f["value_ms"], ms)
		}
	}
	if tm["defaults_from_version"] != nil {
		t.Fatalf("defaults_from_version = %v when every field was observed",
			tm["defaults_from_version"])
	}
}

// TestAnUnverifiedVersionStillGetsNumbersAndSaysSo. Withholding the defaults from
// a newer Alertmanager would put that operator back on `unknown` for no gain; the
// honest form is to answer and qualify.
func TestAnUnverifiedVersionStillGetsNumbersAndSaysSo(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	tm := timings(t, domain.SourceHealth{
		Status:         domain.HealthHealthy,
		AMVersion:      "0.41.0",
		RouteTimingsAt: &at,
	})

	f := field(t, tm, "group_interval")
	if f["value_ms"] != float64(domain.DefaultGroupInterval.Milliseconds()) {
		t.Fatalf("group_interval = %v; an unverified version must still get the best answer available",
			f["value_ms"])
	}
	if tm["defaults_verified"] != false {
		t.Fatal("a release newer than the one oto checked was reported as verified")
	}
	if tm["defaults_from_version"] != "0.41.0" {
		t.Fatalf("defaults_from_version = %v, want the source's own version",
			tm["defaults_from_version"])
	}
}
