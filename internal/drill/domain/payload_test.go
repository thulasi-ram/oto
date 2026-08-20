package domain_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/drill/domain"
	"github.com/thulasiram/oto/internal/ingestion/decode"
)

var (
	drillID = uuid.MustParse("018f0000-0000-7000-8000-0000000000aa")
	fixedAt = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
)

func mustBuild(t *testing.T, sev string) []byte {
	t.Helper()
	body, err := domain.BuildPayload(domain.PayloadInput{
		DrillID: drillID, ClusterKey: "prod-eu", Severity: sev, Now: fixedAt,
	})
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	return body
}

// ⭐⭐ THE LOAD-BEARING TEST OF THIS PACKAGE. A drill is only evidence if the
// payload it pushes survives oto's OWN inbound decoder and bounds — the same ones
// an Alertmanager's body meets. A synthetic payload that ingestion would reject
// turns every drill into a false alarm about the operator's configuration.
func TestPayloadDecodesThroughTheRealIngestDecoder(t *testing.T) {
	env, err := decode.Decode(mustBuild(t, ""))
	if err != nil {
		t.Fatalf("oto cannot decode its own synthetic payload: %v", err)
	}

	if env.Version != "4" {
		t.Errorf("version = %q, want the literal \"4\"", env.Version)
	}
	if env.Status != "firing" {
		t.Errorf("status = %q, want firing", env.Status)
	}
	if env.Receiver != domain.Receiver {
		t.Errorf("receiver = %q, want %q", env.Receiver, domain.Receiver)
	}
	if len(env.Alerts) != 1 {
		t.Fatalf("alerts = %d, want exactly 1 — a drill is one alert", len(env.Alerts))
	}

	a := env.Alerts[0]
	if got := a.Labels["alertname"]; got != domain.AlertName {
		t.Errorf("alertname = %q, want %q", got, domain.AlertName)
	}
	if got := a.Labels["severity"]; got != domain.DefaultSeverity {
		t.Errorf("severity = %q, want the default %q", got, domain.DefaultSeverity)
	}
	if got := a.Labels["cluster"]; got != "prod-eu" {
		t.Errorf("cluster = %q, want the source's cluster key", got)
	}
	if got := a.Labels[domain.DrillLabel]; got != drillID.String() {
		t.Errorf("%s = %q, want the drill id", domain.DrillLabel, got)
	}
	if a.Annotations["summary"] == "" || a.Annotations["description"] == "" {
		t.Error("a drill's card must carry both annotations, or it renders thinner than a real one")
	}

	// ⛔ The Go zero time is the LEGAL Alertmanager spelling of "no end time known
	// for this payload" (B10/B13). A drill must carry what a real firing alert
	// carries, and never a fabricated end.
	if !a.EndsAt.IsZero() {
		t.Errorf("endsAt = %v, want the zero time — a firing alert has no end", a.EndsAt)
	}
	if want := fixedAt.Add(-domain.FiringFor); !a.StartsAt.Equal(want) {
		t.Errorf("startsAt = %v, want %v so the card shows a real duration", a.StartsAt, want)
	}
	// ⛔ Deliberately empty: a drill matches no Prometheus rule, and inventing a
	// generatorURL would make the rule-snapshot stage report a lookup that never
	// happened.
	if a.GeneratorURL != "" {
		t.Errorf("generatorURL = %q, want empty", a.GeneratorURL)
	}
}

// The nonce is what every artefact query matches on. Two drills sharing one would
// make each report on the other's rows.
func TestPayloadNonceIsUniquePerDrill(t *testing.T) {
	other := uuid.MustParse("018f0000-0000-7000-8000-0000000000bb")
	a, err := domain.BuildPayload(domain.PayloadInput{
		DrillID: drillID, ClusterKey: "c", Now: fixedAt,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	b, err := domain.BuildPayload(domain.PayloadInput{
		DrillID: other, ClusterKey: "c", Now: fixedAt,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if string(a) == string(b) {
		t.Fatal("two drills produced identical payloads; their artefacts would be indistinguishable")
	}

	envA, _ := decode.Decode(a)
	envB, _ := decode.Decode(b)
	// ⚠️ THE ENVELOPE'S `groupLabels` MUST CARRY THE NONCE TOO, AND THE REASON HAS
	// CHANGED (git-bug `7570090`). It used to be load-bearing: both drills would
	// otherwise resolve to one §C.4 `group_key`, one generation and one Slack thread.
	// There is no group_key and no generation now — a conversation is the Case, and a
	// Case is per-alert, so the isolation comes from `labels` above rather than from
	// here. What is still asserted, and is why this is retargeted rather than deleted:
	// the envelope is a FAITHFUL FORGERY recorded verbatim in `ingest_batches.payload`,
	// so every copy of the nonce in it has to be this drill's — a body whose
	// `groupLabels` named another drill would make the raw batch on disk lie about
	// which drill wrote it.
	if envA.GroupLabels[domain.DrillLabel] == envB.GroupLabels[domain.DrillLabel] {
		t.Fatal("group labels do not carry the nonce; the raw batch on disk would not " +
			"identify the drill that wrote it")
	}
}

// Reproducible bytes: no clock read, no map-order surprise. A golden test
// elsewhere depends on this, and so does anyone debugging a drill from the raw
// batch on disk.
func TestPayloadIsDeterministic(t *testing.T) {
	for i := range 5 {
		if got, want := string(mustBuild(t, "critical")), string(mustBuild(t, "critical")); got != want {
			t.Fatalf("build %d differed from build 0 — the payload is not reproducible", i)
		}
	}
}

// ⛔ NOTHING IN THE BODY IS THE SYNTHETIC MARK. The mark is
// `ingest_batches.mode`, set by the endpoint that accepted the batch. If a future
// change smuggled a `synthetic: true` into the payload, every upstream on earth
// could set it and evict its own alerts from oto's statistics.
func TestPayloadCarriesNoSyntheticFlag(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal(mustBuild(t, ""), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, found := raw["synthetic"]; found {
		t.Fatal("the payload carries a `synthetic` field — the mark must be provenance, never payload")
	}
	if strings.Contains(strings.ToLower(string(mustBuild(t, ""))), `"synthetic"`) {
		t.Fatal("the payload mentions `synthetic`; a wire-settable mark is forgeable by any upstream")
	}
}

func TestPayloadRefusesIncompleteInput(t *testing.T) {
	cases := map[string]domain.PayloadInput{
		"no drill id":    {ClusterKey: "c", Now: fixedAt},
		"no cluster key": {DrillID: drillID, Now: fixedAt},
		"no clock":       {DrillID: drillID, ClusterKey: "c"},
		"severity too long": {
			DrillID: drillID, ClusterKey: "c", Now: fixedAt,
			Severity: strings.Repeat("x", domain.MaxSeverityBytes+1),
		},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := domain.BuildPayload(in); err == nil {
				t.Fatal("want an error, got none")
			}
		})
	}
}
