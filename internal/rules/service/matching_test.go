package service_test

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/rules/domain"
	"github.com/thulasiram/oto/internal/rules/service"
	sourcesdomain "github.com/thulasiram/oto/internal/sources/domain"
	"github.com/thulasiram/oto/internal/sources/rulematch"
)

// Where `match_confidence` and `candidate_count` actually come from.
//
// `rules` does not match anything. It RECORDS a verdict: `sources/rulematch`
// scores the candidate rules and hands back a Match, `internal/app` maps that
// onto a rules/domain.Recovery, and NewSnapshot copies the confidence and the
// count into the row. Testing the confidence with a stub lookup — which
// capture_test.go also does, for the storage half — proves the copy and nothing
// about the verdict.
//
// So these tests drive the REAL matcher over zero, one and several candidate
// rules, then push the result through the same mapping the composition root uses
// and into a Snapshot. What is being pinned is the end-to-end claim SPEC §D.6 and
// ADR 0009 make: "we guessed" is never stored as "we knew", and an `ambiguous`
// match survives all the way to the column the UI and the Slack card read.
//
// ⚠️ The mapping below is a COPY of `internal/app.recoveryOf`, which is
// unexported. That is the cost of the composition root being the only place
// allowed to know both types (CONTEXT.md §5.4). If the two ever disagree, the
// confidence stored will not be the confidence computed — which is precisely the
// failure this file exists to make loud.

func recoveryOf(m rulematch.Match) domain.Recovery {
	return domain.Recovery{
		Origin:               domain.Origin(m.Origin),
		Strategy:             domain.Strategy(m.Strategy),
		Confidence:           domain.Confidence(m.Confidence),
		CandidateCount:       m.CandidateCount,
		RuleName:             m.RuleName,
		RuleGroup:            m.RuleGroup,
		RuleFile:             m.RuleFile,
		Expr:                 m.Expr,
		ForSeconds:           m.ForSeconds,
		KeepFiringForSeconds: m.KeepFiringForSeconds,
		Labels:               m.Labels,
		Annotations:          m.Annotations,
		PrometheusURL:        m.PrometheusURL,
		Notes:                m.Notes,
	}
}

const promURL = "https://prom.internal"

// generatorURL builds the link Prometheus puts on an alert:
// `externalURL + strutil.TableLinkForExpression(expr)`.
func generatorURL(expr string) string {
	return promURL + "/graph?g0.expr=" + url.QueryEscape(expr) + "&g0.tab=1"
}

func firingAlert(labels map[string]string, generator string) rulematch.Alert {
	return rulematch.Alert{
		Labels:       labels,
		Annotations:  map[string]string{"summary": "checkout is unhappy"},
		GeneratorURL: generator,
	}
}

func group(name, file string, rules ...sourcesdomain.AlertingRule) sourcesdomain.RuleGroup {
	return sourcesdomain.RuleGroup{Name: name, File: file, Rules: rules}
}

func rule(name, query string, labels map[string]string) sourcesdomain.AlertingRule {
	return sourcesdomain.AlertingRule{
		Name:        name,
		Query:       query,
		Duration:    300,
		Labels:      labels,
		Annotations: map[string]string{"summary": "{{ $labels.service }} is unhappy"},
	}
}

// TestMatchConfidenceOverZeroOneAndSeveralCandidates walks the whole ladder.
//
// Read the `count` column against SPEC's rule_snapshots_conf_ck: none↔0,
// exact↔1, probable↔≥1, ambiguous↔≥2. The pairs are not decoration — the DDL
// enforces them, so a matcher that produced `exact` with three candidates would
// take the capture down with a CHECK violation rather than merely be wrong.
func TestMatchConfidenceOverZeroOneAndSeveralCandidates(t *testing.T) {
	t.Parallel()

	alertLabels := map[string]string{
		"alertname": "HighErrorRate",
		"severity":  "critical",
		"service":   "checkout",
		// An external label Prometheus added on the way out. It is on the alert
		// and never on the rule, which is why the subset test is one-directional.
		"cluster": "prod-eu",
	}
	const expr = `rate(errors_total[5m]) > 0.05`

	cases := []struct {
		name       string
		groups     []sourcesdomain.RuleGroup
		generator  string
		confidence domain.Confidence
		count      int
		origin     domain.Origin
		notes      []string
	}{
		{
			name:       "ZERO candidates and no generatorURL: nothing was recovered",
			groups:     []sourcesdomain.RuleGroup{group("other", "other.yml", rule("SomethingElse", expr, nil))},
			confidence: domain.ConfidenceNone,
			count:      0,
			origin:     domain.OriginUnavailable,
			notes:      []string{rulematch.NoteRulesAPINoMatch},
		},
		{
			name:       "ZERO candidates but the alert told us the expression itself",
			groups:     []sourcesdomain.RuleGroup{group("other", "other.yml", rule("SomethingElse", expr, nil))},
			generator:  generatorURL(expr),
			confidence: domain.ConfidenceExact,
			// The expression is not a guess between candidates; it is the one the
			// alert reported. Exactly one candidate, and it is right.
			count:  1,
			origin: domain.OriginGeneratorURL,
			notes:  []string{rulematch.NoteRulesAPINoMatch},
		},
		{
			name:       "the rules API was never consulted",
			groups:     nil,
			generator:  generatorURL(expr),
			confidence: domain.ConfidenceExact,
			count:      1,
			origin:     domain.OriginGeneratorURL,
			notes:      []string{rulematch.NoteNoRulesAPI},
		},
		{
			name: "ONE candidate whose non-templated labels are a subset of the alert's",
			groups: []sourcesdomain.RuleGroup{
				group("checkout", "checkout.yml",
					rule("HighErrorRate", expr, map[string]string{"severity": "critical"})),
			},
			confidence: domain.ConfidenceExact,
			count:      1,
			origin:     domain.OriginPrometheusAPI,
			notes:      []string{rulematch.NoteExternalLabels},
		},
		{
			name: "ONE candidate whose label disagrees: a strong negative, not a veto",
			groups: []sourcesdomain.RuleGroup{
				group("checkout", "checkout.yml",
					rule("HighErrorRate", expr, map[string]string{"severity": "warning"})),
			},
			// alert_relabel_configs can rewrite any label on the way out, so a
			// mismatch downgrades the claim rather than discarding the match.
			confidence: domain.ConfidenceProbable,
			count:      1,
			origin:     domain.OriginPrometheusAPI,
			notes:      []string{rulematch.NoteLabelMismatch},
		},
		{
			name: "SEVERAL candidates with a clear winner on labels",
			groups: []sourcesdomain.RuleGroup{
				group("checkout", "a-checkout.yml",
					rule("HighErrorRate", expr, map[string]string{"severity": "critical"})),
				group("payments", "b-payments.yml",
					rule("HighErrorRate", expr, map[string]string{"severity": "warning"})),
			},
			confidence: domain.ConfidenceProbable,
			// BOTH are counted. "We chose one of two" is the fact being recorded.
			count:  2,
			origin: domain.OriginPrometheusAPI,
			notes:  []string{rulematch.NoteDuplicateAlertName},
		},
		{
			name: "SEVERAL candidates that TIE: oto refuses to break it",
			groups: []sourcesdomain.RuleGroup{
				group("checkout", "a-checkout.yml",
					rule("HighErrorRate", expr, map[string]string{"severity": "critical"})),
				group("payments", "b-payments.yml",
					rule("HighErrorRate", expr, map[string]string{"severity": "critical"})),
				group("search", "c-search.yml",
					rule("HighErrorRate", expr, map[string]string{"severity": "critical"})),
			},
			confidence: domain.ConfidenceAmbiguous,
			count:      3,
			origin:     domain.OriginPrometheusAPI,
			notes:      []string{rulematch.NoteDuplicateAlertName},
		},
		{
			name: "SEVERAL candidates, and the generatorURL narrows them to one",
			groups: []sourcesdomain.RuleGroup{
				group("checkout", "a-checkout.yml",
					rule("HighErrorRate", expr, map[string]string{"severity": "critical"})),
				group("payments", "b-payments.yml",
					rule("HighErrorRate", `rate(payment_errors_total[5m]) > 0.01`,
						map[string]string{"severity": "critical"})),
			},
			generator: generatorURL(expr),
			// Expression equality is a HARD FILTER, not a tiebreak: the alert told
			// us which expression fired, so the count collapses to one and the
			// match is exact rather than ambiguous.
			confidence: domain.ConfidenceExact,
			count:      1,
			origin:     domain.OriginPrometheusAPI,
			notes:      []string{rulematch.NoteExprDisambiguated},
		},
		{
			name: "a templated rule label is skipped rather than compared",
			groups: []sourcesdomain.RuleGroup{
				group("checkout", "checkout.yml",
					rule("HighErrorRate", expr, map[string]string{
						"severity": "critical",
						// The API returns the raw template; the alert carries its
						// rendering. Comparing them would fail every time.
						"pod": "{{ $labels.pod }}",
					})),
			},
			confidence: domain.ConfidenceExact,
			count:      1,
			origin:     domain.OriginPrometheusAPI,
			notes:      []string{rulematch.NoteTemplatedLabels},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := rulematch.Resolve(rulematch.Input{
				Alert:         firingAlert(alertLabels, tc.generator),
				Groups:        tc.groups,
				PrometheusURL: promURL,
			})
			require.NoError(t, m.Validate(), "the matcher must not emit a combination the DDL refuses")

			assert.Equal(t, rulematch.Confidence(tc.confidence), m.Confidence)
			assert.Equal(t, tc.count, m.CandidateCount)
			assert.Equal(t, rulematch.Origin(tc.origin), m.Origin)
			for _, n := range tc.notes {
				assert.Truef(t, m.HasNote(n), "expected note %q, got %v", n, m.Notes)
			}

			// ...and now the same verdict, all the way into the row.
			r := newRig(t, func(service.LookupRequest) (domain.Recovery, error) {
				return recoveryOf(m), nil
			})
			c := r.capture(t, id.New(), id.New())

			assert.Equal(t, tc.confidence, c.Snapshot.Confidence,
				"the stored match_confidence must be the one the matcher computed")
			assert.Equal(t, tc.count, c.Snapshot.CandidateCount)
			assert.Equal(t, tc.origin, c.Snapshot.Origin)
			require.NoError(t, c.Snapshot.Validate(),
				"confidence and candidate_count must satisfy rule_snapshots_conf_ck")

			// An ambiguous match is never hidden (SPEC §D.6): the predicate the UI
			// and the Slack card branch on has to be true.
			assert.Equal(t, tc.confidence == domain.ConfidenceAmbiguous, c.Snapshot.Ambiguous())
			for _, n := range tc.notes {
				assert.Contains(t, c.Warnings, n, "the reason codes must survive into the capture")
			}
		})
	}
}

// TestMatchedRuleCarriesTheEnrichmentOnlyFields: `for`, `keep_firing_for` and
// the rule's RAW labels and annotations exist only on the /api/v1/rules path.
// They are also three of the five fields the fingerprint is computed over, so a
// generator_url capture and a prometheus_api capture of the same rule are
// legitimately different content — which is why Diff carries OriginChanged.
func TestMatchedRuleCarriesTheEnrichmentOnlyFields(t *testing.T) {
	t.Parallel()

	const expr = `rate(errors_total[5m]) > 0.05`
	matched := rule("HighErrorRate", expr, map[string]string{"severity": "critical"})
	// `for: 1s500ms` and `keep_firing_for: 500ms` — the fractional values that
	// used to be truncated to whole seconds before the two §C.6 implementations
	// were collapsed onto one.
	matched.Duration = 1.5
	matched.KeepFiringFor = 0.5

	viaAPI := rulematch.Resolve(rulematch.Input{
		Alert:         firingAlert(map[string]string{"alertname": "HighErrorRate", "severity": "critical"}, generatorURL(expr)),
		Groups:        []sourcesdomain.RuleGroup{group("checkout", "checkout.yml", matched)},
		PrometheusURL: promURL,
	})
	viaGenerator := rulematch.Resolve(rulematch.Input{
		Alert: firingAlert(map[string]string{"alertname": "HighErrorRate", "severity": "critical"}, generatorURL(expr)),
	})

	require.Equal(t, rulematch.OriginPrometheusAPI, viaAPI.Origin)
	require.Equal(t, rulematch.OriginGeneratorURL, viaGenerator.Origin)
	assert.Equal(t, 1.5, viaAPI.ForSeconds)
	assert.Equal(t, 0.0, viaGenerator.ForSeconds, "generatorURL knows the expression and nothing else")
	assert.Equal(t, "checkout", viaAPI.RuleGroup)
	assert.Empty(t, viaGenerator.RuleGroup, "and it does not know where the rule is written down")

	apiRig := newRig(t, func(service.LookupRequest) (domain.Recovery, error) { return recoveryOf(viaAPI), nil })
	api := apiRig.capture(t, id.New(), id.New())

	genRig := newRig(t, func(service.LookupRequest) (domain.Recovery, error) { return recoveryOf(viaGenerator), nil })
	gen := genRig.capture(t, id.New(), id.New())

	assert.NotEqual(t, api.Snapshot.Fingerprint, gen.Snapshot.Fingerprint,
		"the two paths recovered different amounts of the rule, so they are different content")
	assert.Equal(t, 1.5, api.Snapshot.ForSeconds,
		"a fractional `for` must survive into the row; truncating it here is how sub-second edits went invisible")
	assert.Equal(t, 0.5, api.Snapshot.KeepFiringForSeconds)
}
