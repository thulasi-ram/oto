package rulematch

import "strings"

// Resolve recovers the originating rule, applying the SPEC's preference order.
//
// The order is not "try the cheap one first". It is a correctness ordering
// (SPEC §F.4, research A7):
//
//  1. generatorURL's g0.expr is the expression AS EVALUATED. It cannot be
//     ambiguous, it costs no API call, and it is right even when several
//     Prometheis feed one Alertmanager. It is the primary source of `expr`.
//  2. /api/v1/rules is the ENRICHMENT path. It is the only source of `for`,
//     `keep_firing_for` and the rule's raw labels and annotations — and it is
//     the ambiguous one, because alertname is not unique, can be rewritten by
//     relabeling, and its labels arrive untemplated.
//
// When both succeed the result is origin=prometheus_api (it is strictly more
// information), the generatorURL expression is used to DISAMBIGUATE between
// same-named rules, and a divergence between the two expressions is recorded as
// a note rather than silently resolved.
//
// Resolve performs no I/O. The caller fetches Groups; that keeps the preference
// order testable without a Prometheus and keeps this package pure.
func Resolve(in Input) Match {
	gen, genErr := ParseGeneratorURL(in.Alert.GeneratorURL)
	hasGen := genErr == nil && gen.Expr != ""

	name := in.Alert.AlertName()
	if name == "" {
		m := Match{
			Origin:     OriginUnavailable,
			Strategy:   StrategyNone,
			Confidence: ConfidenceNone,
			Notes:      []string{NoteNoAlertName},
		}
		if hasGen {
			m.ExternalURL = gen.ExternalURL
		}
		return m
	}
	if len(name) > MaxRuleNameBytes {
		name = name[:MaxRuleNameBytes]
	}

	// --- strategy 2 input -------------------------------------------------
	if len(in.Groups) == 0 {
		return fromGenerator(gen, hasGen, name, NoteNoRulesAPI)
	}

	cands := Candidates(in.Alert, in.Groups, gen.Expr)
	if len(cands) == 0 {
		return fromGenerator(gen, hasGen, name, NoteRulesAPINoMatch)
	}

	notes := []string{NoteExternalLabels}
	total := len(cands)
	if total > 1 {
		notes = append(notes, NoteDuplicateAlertName)
	}

	// Expression equality is a HARD FILTER, not a tiebreak: if the alert told us
	// which expression fired and exactly one rule carries it, that rule IS the
	// rule, whatever its labels say.
	chosen := cands
	if hasGen {
		var exact []Candidate
		for _, c := range cands {
			if c.Score.ExprEqual {
				exact = append(exact, c)
			}
		}
		switch {
		case len(exact) == 1:
			chosen = exact
			total = 1
			if len(cands) > 1 {
				notes = append(notes, NoteExprDisambiguated)
			}
		case len(exact) > 1:
			chosen = exact
		}
	}

	best := chosen[0]
	tied := 0
	for _, c := range chosen {
		if c.Score.Total == best.Score.Total {
			tied++
		}
	}

	conf := ConfidenceProbable
	switch {
	case tied > 1:
		conf = ConfidenceAmbiguous
		if total < 2 {
			total = tied
		}
	case total == 1 && best.Score.MismatchedLabels == 0:
		conf = ConfidenceExact
	}
	if best.Score.MismatchedLabels > 0 {
		notes = append(notes, NoteLabelMismatch)
		if conf == ConfidenceExact {
			conf = ConfidenceProbable
		}
	}
	if best.Score.TemplatedLabels > 0 {
		notes = append(notes, NoteTemplatedLabels)
	}

	m := Match{
		Origin:               OriginPrometheusAPI,
		Strategy:             StrategyRulesAPI,
		Confidence:           conf,
		CandidateCount:       total,
		RuleName:             clampName(best.Rule.Name),
		RuleGroup:            best.Group,
		RuleFile:             best.File,
		Expr:                 clampExpr(best.Rule.Query),
		ForSeconds:           nonNegative(best.Rule.Duration),
		KeepFiringForSeconds: nonNegative(best.Rule.KeepFiringFor),
		Labels:               best.Rule.Labels,
		Annotations:          best.Rule.Annotations,
		PrometheusURL:        in.PrometheusURL,
		Notes:                notes,
	}
	if hasGen {
		m.ExternalURL = gen.ExternalURL
		if normaliseExpr(gen.Expr) != normaliseExpr(best.Rule.Query) {
			m.Notes = append(m.Notes, NoteExprDivergence)
		}
	}

	// rule_snapshots_promurl_ck: origin=prometheus_api without a prometheus_url
	// is not storable. The generatorURL's externalURL IS the server that
	// evaluated the rule, so it is the correct value to fall back to; without
	// even that, the API result has no provenance and is dropped rather than
	// recorded with a fabricated one.
	if m.PrometheusURL == "" {
		if !hasGen || gen.ExternalURL == "" {
			return fromGenerator(gen, hasGen, name, NoteRulesAPINoMatch)
		}
		m.PrometheusURL = gen.ExternalURL
	}
	// rule_snapshots_expr_ck: a non-unavailable origin needs a non-empty expr.
	if strings.TrimSpace(m.Expr) == "" {
		if hasGen {
			m.Expr = clampExpr(gen.Expr)
			m.Origin = OriginPrometheusAPI
		} else {
			return Match{
				Origin: OriginUnavailable, Strategy: StrategyNone,
				Confidence: ConfidenceNone, RuleName: "", Notes: m.Notes,
			}
		}
	}
	return m
}

// fromGenerator builds the primary-path-only result.
func fromGenerator(gen GeneratorURL, ok bool, name string, note string) Match {
	if !ok {
		return Match{
			Origin:     OriginUnavailable,
			Strategy:   StrategyNone,
			Confidence: ConfidenceNone,
			Notes:      []string{note},
		}
	}
	return Match{
		Origin:     OriginGeneratorURL,
		Strategy:   StrategyGeneratorURL,
		Confidence: ConfidenceExact,
		// The expression is not a guess between candidates; it is the one the
		// alert itself reported. Exactly one candidate, and it is right.
		CandidateCount: 1,
		RuleName:       name,
		Expr:           clampExpr(gen.Expr),
		ExternalURL:    gen.ExternalURL,
		Notes:          []string{note},
	}
}

// clampName enforces rule_snapshots_name_ck.
func clampName(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > MaxRuleNameBytes {
		return s[:MaxRuleNameBytes]
	}
	return s
}

// clampExpr enforces rule_snapshots_exprlen_ck.
func clampExpr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > MaxExprBytes {
		return s[:MaxExprBytes]
	}
	return s
}

// nonNegative enforces rule_snapshots_for_ck. Prometheus reports these as float
// SECONDS (600 means `for: 10m`); a negative value is impossible and would be a
// CHECK violation, so it is clamped rather than propagated.
func nonNegative(f float64) float64 {
	if f < 0 {
		return 0
	}
	return f
}
