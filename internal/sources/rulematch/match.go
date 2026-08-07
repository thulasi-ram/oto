package rulematch

import (
	"sort"
	"strings"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// LabelAlertName is the label that names the alerting rule.
const LabelAlertName = "alertname"

// MaxRuleNameBytes mirrors rule_snapshots_name_ck.
const MaxRuleNameBytes = 1024

// Strategy names how a rule was recovered. The caller records it alongside the
// snapshot so that "we guessed" is never indistinguishable from "we knew".
type Strategy string

// The recovery strategies, in SPEC's preference order.
const (
	// StrategyNone means nothing was recovered.
	StrategyNone Strategy = "none"
	// StrategyGeneratorURL is the primary path: decode g0.expr, zero API calls.
	StrategyGeneratorURL Strategy = "generator_url"
	// StrategyRulesAPI is the enrichment path: GET /api/v1/rules, matched on
	// alertname plus a non-templated label subset.
	StrategyRulesAPI Strategy = "rules_api"
)

// Origin mirrors rule_snapshots.origin (SPEC §D.6). The values are a closed set
// enforced by a DDL CHECK, so they are written here exactly once.
type Origin string

// The snapshot origins.
const (
	// OriginUnavailable means no expression was recovered at all. The DDL binds
	// this to an empty expr and vice versa (rule_snapshots_expr_ck).
	OriginUnavailable Origin = "unavailable"
	// OriginGeneratorURL means the expr came from g0.expr with no API call.
	OriginGeneratorURL Origin = "generator_url"
	// OriginPrometheusAPI means the rules API also supplied `for`,
	// `keep_firing_for` and the raw labels and annotations.
	OriginPrometheusAPI Origin = "prometheus_api"
)

// Confidence mirrors rule_snapshots.match_confidence.
type Confidence string

// The match confidences. rule_snapshots_conf_ck binds each to a CandidateCount,
// which Match.Validate re-checks before anything reaches the database.
const (
	// ConfidenceNone means no candidate matched; CandidateCount must be 0.
	ConfidenceNone Confidence = "none"
	// ConfidenceExact means one unambiguous candidate; CandidateCount must be 1.
	ConfidenceExact Confidence = "exact"
	// ConfidenceProbable means a clear winner among several; count >= 1.
	ConfidenceProbable Confidence = "probable"
	// ConfidenceAmbiguous means a tie oto refuses to break; count >= 2. It MUST
	// be surfaced in the UI and in Slack, never hidden (SPEC §D.6).
	ConfidenceAmbiguous Confidence = "ambiguous"
)

// Note codes are stable, greppable reasons attached to a Match. Each names one
// of the documented ambiguity pitfalls, so that an operator staring at an
// `ambiguous` snapshot is told WHY rather than left to guess.
const (
	// NoteNoAlertName means the alert has no alertname, so no rule lookup is
	// even possible (ingest bound B10 should have rejected it first).
	NoteNoAlertName = "no_alertname"
	// NoteDuplicateAlertName means several rules across groups or files share
	// this alertname. Nothing in Prometheus forbids it (pitfall 1).
	NoteDuplicateAlertName = "duplicate_alertname"
	// NoteTemplatedLabels means one or more rule labels contain `{{ … }}`, so
	// the rules API returns the raw template while the alert carries the
	// rendered value. Those labels are excluded from the subset test (pitfall 3).
	NoteTemplatedLabels = "templated_rule_labels"
	// NoteLabelMismatch means a non-templated rule label disagreed with the
	// alert. Prometheus alert_relabel_configs can rewrite any label on the way
	// out, including alertname, so this is a strong negative and not a veto
	// (pitfall 2).
	NoteLabelMismatch = "rule_label_mismatch"
	// NoteExprDisambiguated means the generatorURL expression narrowed several
	// same-named rules to one. This is the strongest signal available.
	NoteExprDisambiguated = "expr_disambiguated"
	// NoteExprDivergence means the rules API's query differs from the expression
	// the alert was actually generated from — a reload between fire and fetch,
	// or the wrong Prometheus (pitfall 5).
	NoteExprDivergence = "expr_divergence"
	// NoteRulesAPINoMatch means the rules API answered but knew no such rule, so
	// the match fell back to generatorURL alone.
	NoteRulesAPINoMatch = "rules_api_no_match"
	// NoteExternalLabels records that the subset test is deliberately
	// one-directional: global.external_labels are added to alerts on the way to
	// Alertmanager and are never on the rule (pitfall 4).
	NoteExternalLabels = "external_labels_ignored"
	// NoteNoRulesAPI means no Prometheus was configured or consulted.
	NoteNoRulesAPI = "rules_api_not_consulted"
)

// Alert is the minimum an alert must carry to have its rule recovered.
type Alert struct {
	// Labels are the RENDERED labels as Alertmanager holds them, including any
	// external labels Prometheus added on the way out.
	Labels map[string]string
	// Annotations are the rendered annotations.
	Annotations map[string]string
	// GeneratorURL is the primary strategy's whole input.
	GeneratorURL string
}

// AlertName returns the alertname label.
func (a Alert) AlertName() string { return a.Labels[LabelAlertName] }

// Input is one rule-recovery request.
type Input struct {
	// Alert is the alert whose rule is wanted.
	Alert Alert
	// Groups are rule groups already fetched from /api/v1/rules, or nil when the
	// API was not consulted (no prometheus_url, or the caller only wants the
	// zero-API-call path).
	Groups []domain.RuleGroup
	// PrometheusURL is the server Groups came from. It is REQUIRED whenever
	// Groups is non-empty and a prometheus_api origin is produced, because
	// rule_snapshots_promurl_ck rejects that origin without it.
	PrometheusURL string
}

// Match is a recovered rule definition plus its provenance. Every field maps
// onto a rule_snapshots column, so the caller records it verbatim.
type Match struct {
	Origin         Origin
	Strategy       Strategy
	Confidence     Confidence
	CandidateCount int

	RuleName  string
	RuleGroup string
	RuleFile  string

	Expr                 string
	ForSeconds           float64
	KeepFiringForSeconds float64
	Labels               map[string]string
	Annotations          map[string]string

	// PrometheusURL is set iff Origin is OriginPrometheusAPI.
	PrometheusURL string
	// ExternalURL is the origin Prometheus recovered from generatorURL, which is
	// how a federated deployment knows which server to ask next time.
	ExternalURL string
	// Notes are the stable Note* codes explaining the confidence.
	Notes []string
}

// Found reports whether anything usable was recovered.
func (m Match) Found() bool { return m.Origin != OriginUnavailable && m.Expr != "" }

// HasNote reports whether the match carries a note code.
func (m Match) HasNote(code string) bool {
	for _, n := range m.Notes {
		if n == code {
			return true
		}
	}
	return false
}

// Validate re-checks the invariants the DDL enforces, so that an impossible
// combination is caught here rather than as a 500 from a CHECK violation
// (SPEC §L.1: a CHECK reaching the HTTP layer means layers 1-3 have a hole).
func (m Match) Validate() error {
	switch {
	case (m.Origin == OriginUnavailable) != (strings.TrimSpace(m.Expr) == ""):
		return errs.New(errs.KindInternal, "rulematch_expr_origin_mismatch",
			"origin=unavailable and an empty expr must agree (rule_snapshots_expr_ck)")
	case len(m.Expr) > MaxExprBytes:
		return errs.Newf(errs.KindInternal, "rulematch_expr_too_large",
			"expr must be at most %d bytes", MaxExprBytes)
	case m.Origin == OriginPrometheusAPI && m.PrometheusURL == "":
		return errs.New(errs.KindInternal, "rulematch_missing_prometheus_url",
			"origin=prometheus_api requires a prometheus_url (rule_snapshots_promurl_ck)")
	case m.Origin != OriginUnavailable && (m.RuleName == "" || len(m.RuleName) > MaxRuleNameBytes):
		return errs.Newf(errs.KindInternal, "rulematch_invalid_rule_name",
			"rule_name must be 1..%d bytes (rule_snapshots_name_ck)", MaxRuleNameBytes)
	case m.ForSeconds < 0 || m.KeepFiringForSeconds < 0:
		return errs.New(errs.KindInternal, "rulematch_negative_duration",
			"for and keep_firing_for must be non-negative (rule_snapshots_for_ck)")
	}

	ok := (m.Confidence == ConfidenceNone && m.CandidateCount == 0) ||
		(m.Confidence == ConfidenceExact && m.CandidateCount == 1) ||
		(m.Confidence == ConfidenceProbable && m.CandidateCount >= 1) ||
		(m.Confidence == ConfidenceAmbiguous && m.CandidateCount >= 2)
	if !ok {
		return errs.Newf(errs.KindInternal, "rulematch_confidence_mismatch",
			"confidence=%s is not compatible with candidate_count=%d (rule_snapshots_conf_ck)",
			m.Confidence, m.CandidateCount)
	}
	return nil
}

// Candidate is one rule that could be the alert's origin, with its score.
type Candidate struct {
	Group string
	File  string
	Rule  domain.AlertingRule
	Score Score
}

// Score explains why a candidate ranked where it did. It is exported because
// "surface ambiguity rather than guessing" means the reasoning has to be
// inspectable, not just the verdict.
type Score struct {
	// ExprEqual means the rule's query equals the generatorURL expression.
	ExprEqual bool
	// MatchedLabels counts non-templated rule labels present and equal on the
	// alert.
	MatchedLabels int
	// MismatchedLabels counts non-templated rule labels present and different.
	MismatchedLabels int
	// MissingLabels counts non-templated rule labels absent from the alert.
	MissingLabels int
	// TemplatedLabels counts rule labels skipped because they are templates.
	TemplatedLabels int
	// AnnotationOverlap counts annotation KEYS the rule and the alert share.
	// Values are not compared: the rule holds a template, the alert holds its
	// rendering.
	AnnotationOverlap int
	// Total is the weighted sum the ranking uses.
	Total int
}

// Scoring weights. Expression equality dominates everything because it is the
// only signal that cannot be produced by coincidence.
const (
	weightExprEqual  = 1000
	weightLabelMatch = 5
	weightLabelMiss  = -8
	weightLabelAbsen = -1
	weightAnnotation = 1
)

// Candidates scores every rule in groups against the alert.
//
// The subset test is deliberately ONE-DIRECTIONAL — rule labels ⊆ alert labels,
// never the reverse — because Prometheus adds global.external_labels to alerts
// on the way to Alertmanager and those labels are never on the rule (pitfall 4).
// Templated rule label values are skipped entirely: the API returns the raw
// `{{ $labels.x }}` and the alert carries its rendering, so comparing them would
// fail every time templating is used (pitfall 3).
func Candidates(a Alert, groups []domain.RuleGroup, exprHint string) []Candidate {
	name := a.AlertName()
	if name == "" {
		return nil
	}
	hint := normaliseExpr(exprHint)

	var out []Candidate
	for _, g := range groups {
		for _, r := range g.Rules {
			if r.Name != name {
				continue
			}
			c := Candidate{Group: g.Name, File: g.File, Rule: r}
			c.Score = score(a, r, hint)
			out = append(out, c)
		}
	}

	// Deterministic order: best first, then by group and file so that two runs
	// over the same input never disagree about which of two tied rules is listed
	// first. Ambiguity must be reported, not resolved by map iteration order.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score.Total != out[j].Score.Total {
			return out[i].Score.Total > out[j].Score.Total
		}
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Group < out[j].Group
	})
	return out
}

// score weighs one candidate.
func score(a Alert, r domain.AlertingRule, exprHint string) Score {
	var s Score
	if exprHint != "" && normaliseExpr(r.Query) == exprHint {
		s.ExprEqual = true
	}
	for k, v := range r.Labels {
		if isTemplated(v) {
			s.TemplatedLabels++
			continue
		}
		av, present := a.Labels[k]
		switch {
		case !present:
			s.MissingLabels++
		case av == v:
			s.MatchedLabels++
		default:
			s.MismatchedLabels++
		}
	}
	for k := range r.Annotations {
		if _, ok := a.Annotations[k]; ok {
			s.AnnotationOverlap++
		}
	}

	s.Total = s.MatchedLabels*weightLabelMatch +
		s.MismatchedLabels*weightLabelMiss +
		s.MissingLabels*weightLabelAbsen +
		s.AnnotationOverlap*weightAnnotation
	if s.ExprEqual {
		s.Total += weightExprEqual
	}
	return s
}

// isTemplated reports whether a rule label value is a Go template rather than a
// literal.
func isTemplated(v string) bool {
	return strings.Contains(v, "{{")
}

// normaliseExpr collapses whitespace so that a reformatted-but-identical
// expression still compares equal. It does NOT parse PromQL: oto has no
// business having a PromQL parser, and a textual comparison that is occasionally
// too strict is far safer than a semantic one that is occasionally too loose.
func normaliseExpr(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
