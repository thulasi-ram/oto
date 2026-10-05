package service

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// TestEveryTestIncidentFactNamesItsOwnIncident — a receiver orders an Incident's
// facts by (id, sequence) and may drop one at or below the highest it has seen for
// that Incident. Every test fact is sequence 1, so if two tests named the same
// Incident the second would be dropped as a replay of the first. Each test send is
// its own one-fact story: a fresh Incident id, sequence 1, every time.
func TestEveryTestIncidentFactNamesItsOwnIncident(t *testing.T) {
	t.Parallel()

	inst := domain.Instance{ID: uuid.New(), OrgID: uuid.New(), Name: "incident-tool"}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

	seen := map[string]bool{}
	for _, fact := range []string{"drawn", "drawn", "case_added", "quiet"} {
		v := SyntheticFactView(inst, now, "https://oto.example", fact)
		if v.Incident == nil {
			t.Fatalf("%s: an Incident fact rendered no Incident", fact)
		}
		if v.Incident.Sequence != 1 {
			t.Fatalf("%s: sequence = %d, want 1 — a test is the only fact of its story",
				fact, v.Incident.Sequence)
		}
		if _, err := uuid.Parse(v.Incident.ID); err != nil {
			t.Fatalf("%s: incident id %q is not a uuid: %v", fact, v.Incident.ID, err)
		}
		if seen[v.Incident.ID] {
			t.Fatalf("%s: incident id %s was already sent by an earlier test — a receiver "+
				"keyed on (id, sequence) would drop this one", fact, v.Incident.ID)
		}
		seen[v.Incident.ID] = true
	}
}
