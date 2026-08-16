package repository_test

// These tests are against the REAL Postgres, for the same reason
// labels_plan_test.go is: what is under test is a property of the SQL text the
// repository builds — which query-building function `@@` runs against, and
// whether `alertname ILIKE` is even in the WHERE clause — and only a real
// planner and a real GIN/pg_trgm index can answer that honestly. TestMain lives
// in prune_test.go; this file adds no second one.

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/alerts/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/test/harness"
)

// setAnnotations writes alerts.annotations directly, because
// harness.AlertWith seeds only the promoted labels and leaves annotations at
// its `{}` default. `?q=` also searches summary/description, so a test about
// the text-search clause needs a row that actually has some.
func setAnnotations(h *harness.H, alertID uuid.UUID, kv map[string]string) {
	h.T.Helper()
	b, err := json.Marshal(kv)
	require.NoError(h.T, err)
	h.Exec(`UPDATE alerts SET annotations = $1::jsonb WHERE id = $2`, b, alertID)
}

// names extracts alertname for a stable, order-independent comparison: the
// point of every assertion below is WHICH rows matched, not what order the
// keyset returned them in.
func names(alerts []domain.Alert) []string {
	out := make([]string, len(alerts))
	for i, a := range alerts {
		out[i] = a.AlertName()
	}
	sort.Strings(out)
	return out
}

// TestWebsearchToTsqueryUpgradesQuerySemantics is part A: the same
// alerts_text_idx-mirroring left-hand side, but websearch_to_tsquery's richer
// grammar — negation, phrase search, OR — where plainto_tsquery could only ever
// AND bare words together.
func TestWebsearchToTsqueryUpgradesQuerySemantics(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	cl := h.Cluster(org)
	repo := repository.NewAlertRepository(h.Pool, h.Clock, false)

	checkout := h.AlertWith(org, cl, map[string]string{"alertname": "CheckoutErrorRateHigh"})
	setAnnotations(h, checkout.ID, map[string]string{
		"summary":     "checkout error budget burned",
		"description": "the checkout service is returning 5xx at an elevated rate",
	})
	payment := h.AlertWith(org, cl, map[string]string{"alertname": "PaymentLatencyHigh"})
	setAnnotations(h, payment.ID, map[string]string{
		"summary":     "payment latency budget burned",
		"description": "p99 latency on the payment service has crossed its budget",
	})
	disk := h.AlertWith(org, cl, map[string]string{"alertname": "DiskSpaceLow"})
	setAnnotations(h, disk.ID, map[string]string{
		"summary":     "disk space running out",
		"description": "root volume utilisation is above threshold",
	})

	t.Run("bare word still matches, same as plainto_tsquery would", func(t *testing.T) {
		got, _, err := repo.List(t.Context(), org.Scope, domain.AlertFilter{Query: "checkout"}, db.Keyset{})
		require.NoError(t, err)
		require.Equal(t, []string{"CheckoutErrorRateHigh"}, names(got))
	})

	t.Run("phrase search matches only the adjacent words", func(t *testing.T) {
		got, _, err := repo.List(t.Context(), org.Scope,
			domain.AlertFilter{Query: `"budget burned"`}, db.Keyset{})
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"CheckoutErrorRateHigh", "PaymentLatencyHigh"}, names(got))

		// "burned budget" is the same two lexemes in the other order: not adjacent
		// in any of the seeded text, so the phrase must NOT match.
		got, _, err = repo.List(t.Context(), org.Scope,
			domain.AlertFilter{Query: `"burned budget"`}, db.Keyset{})
		require.NoError(t, err)
		require.Empty(t, got)
	})

	t.Run("negation excludes a word plainto_tsquery could never exclude", func(t *testing.T) {
		got, _, err := repo.List(t.Context(), org.Scope,
			domain.AlertFilter{Query: "budget -checkout"}, db.Keyset{})
		require.NoError(t, err)
		require.Equal(t, []string{"PaymentLatencyHigh"}, names(got))
	})

	t.Run("OR unions two otherwise-unrelated matches", func(t *testing.T) {
		got, _, err := repo.List(t.Context(), org.Scope,
			domain.AlertFilter{Query: "disk OR payment"}, db.Keyset{})
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"DiskSpaceLow", "PaymentLatencyHigh"}, names(got))
	})
}

// TestTrigramUnavailableOnlyMatchesFullText is part B's negative branch: absent
// the capability, `?q=` cannot find a substring inside a compound alertname —
// exactly the gap trigram closes, proven by showing it stays open here.
func TestTrigramUnavailableOnlyMatchesFullText(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	cl := h.Cluster(org)
	// trigramAvailable=false: the constructor arg the vast majority of
	// deployments run with, since oto never enables pg_trgm itself.
	repo := repository.NewAlertRepository(h.Pool, h.Clock, false)

	h.AlertWith(org, cl, map[string]string{"alertname": "CheckoutErrorRateHigh"})

	// "error" is a lexeme-internal substring of the one-lexeme alertname
	// `CheckoutErrorRateHigh` and appears nowhere else on the row (annotations
	// are left at their `{}` default) — this is precisely the case the runbook
	// and the ticket describe as unreachable by tsquery alone.
	got, _, err := repo.List(t.Context(), org.Scope, domain.AlertFilter{Query: "error"}, db.Keyset{})
	require.NoError(t, err)
	require.Empty(t, got, "tsquery alone must not substring-match inside a compound alertname")
}

// TestTrigramAvailableClosesTheSubstringGap is part B's positive branch. It
// enables pg_trgm the same way the runbook tells an operator to (this test
// does it directly; docs/runbooks/alert-search-partial-match.md is the
// human-facing copy) and proves the OR branch in applyAlertFilter finds what
// full-text search structurally cannot.
func TestTrigramAvailableClosesTheSubstringGap(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	// Never oto's own migration path — see internal/platform/db/capabilities.go
	// and the runbook. A test enabling it directly is standing in for the
	// operator's one-time, self-service step.
	h.Exec(`CREATE EXTENSION IF NOT EXISTS pg_trgm`)

	org := h.Org()
	cl := h.Cluster(org)
	repo := repository.NewAlertRepository(h.Pool, h.Clock, true)

	h.AlertWith(org, cl, map[string]string{"alertname": "CheckoutErrorRateHigh"})
	quota := h.AlertWith(org, cl, map[string]string{"alertname": "QuotaExceeded"})
	// "payment" appears only in the summary, nowhere in any seeded alertname, so
	// a match on it can ONLY come through the tsquery side of the OR, never the
	// ILIKE side.
	setAnnotations(h, quota.ID, map[string]string{"summary": "customer payment failed to process"})

	// The substring case: "error" is inside CheckoutErrorRateHigh's one lexeme
	// and appears in no annotation, so only the ILIKE branch can find it.
	got, _, err := repo.List(t.Context(), org.Scope, domain.AlertFilter{Query: "error"}, db.Keyset{})
	require.NoError(t, err)
	require.Equal(t, []string{"CheckoutErrorRateHigh"}, names(got))

	// The full-text case: the OR still carries websearch_to_tsquery, so enabling
	// trigram never narrows what oto already matched — it only adds the
	// substring case alongside it.
	got, _, err = repo.List(t.Context(), org.Scope, domain.AlertFilter{Query: "payment"}, db.Keyset{})
	require.NoError(t, err)
	require.Equal(t, []string{"QuotaExceeded"}, names(got))
}
