package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Origin says how a rule definition was obtained. It mirrors
// `rule_snapshots.origin`, whose CHECK is the authority on the value set.
//
// The strings are duplicated here rather than imported from
// internal/sources/rulematch because a domain package may not reach into
// another module (CONTEXT.md §5.4, enforced by depguard). The Recovery struct
// is the seam: `internal/app` maps one onto the other in a dozen lines.
type Origin string

// The snapshot origins.
const (
	// OriginUnavailable means no expression was recovered at all. This is a
	// NORMAL outcome, not an error: a Prometheus behind a firewall and an alert
	// with no generatorURL are both ordinary, and a snapshot that honestly says
	// "unavailable" is worth more than no row at all.
	OriginUnavailable Origin = "unavailable"
	// OriginGeneratorURL means the expr came from g0.expr with no API call.
	OriginGeneratorURL Origin = "generator_url"
	// OriginPrometheusAPI means /api/v1/rules also supplied `for`,
	// `keep_firing_for` and the rule's raw labels and annotations.
	OriginPrometheusAPI Origin = "prometheus_api"
)

// Valid reports whether o is one of the three storable origins.
func (o Origin) Valid() bool {
	switch o {
	case OriginUnavailable, OriginGeneratorURL, OriginPrometheusAPI:
		return true
	default:
		return false
	}
}

// Confidence mirrors `rule_snapshots.match_confidence`. It is stored alongside
// every snapshot so that "we guessed" is never indistinguishable from "we knew".
type Confidence string

// The match confidences. Each is locked to a CandidateCount by
// rule_snapshots_conf_ck, which Snapshot.Validate re-checks before any write.
const (
	// ConfidenceNone means nothing matched; CandidateCount must be 0.
	ConfidenceNone Confidence = "none"
	// ConfidenceExact means one unambiguous candidate; CandidateCount must be 1.
	ConfidenceExact Confidence = "exact"
	// ConfidenceProbable means a clear winner among several; count >= 1.
	ConfidenceProbable Confidence = "probable"
	// ConfidenceAmbiguous means a tie oto refuses to break; count >= 2. It MUST
	// be surfaced in the UI and in Slack, never hidden (SPEC §D.6).
	ConfidenceAmbiguous Confidence = "ambiguous"
)

// Valid reports whether c is one of the four storable confidences.
func (c Confidence) Valid() bool {
	switch c {
	case ConfidenceNone, ConfidenceExact, ConfidenceProbable, ConfidenceAmbiguous:
		return true
	default:
		return false
	}
}

// Strategy names the recovery path that produced a snapshot. It is provenance,
// not a column: it is carried on the Recovery and recorded in the capture's
// warnings and in the `rule.snapshot_captured` event payload.
type Strategy string

// The recovery strategies, in SPEC §F.4's preference order.
const (
	// StrategyNone means nothing was recovered.
	StrategyNone Strategy = "none"
	// StrategyGeneratorURL is the primary path: decode g0.expr, zero API calls.
	StrategyGeneratorURL Strategy = "generator_url"
	// StrategyRulesAPI is the enrichment path: GET /api/v1/rules.
	StrategyRulesAPI Strategy = "rules_api"
)

// Size bounds mirroring the CHECK constraints on `rule_snapshots`. They are
// applied in Go so that an oversized expression is clamped at the boundary
// rather than arriving as a 500 from a constraint violation (SPEC §L.1).
const (
	// MaxExprBytes mirrors rule_snapshots_exprlen_ck.
	MaxExprBytes = 65536
	// MaxRuleNameBytes mirrors rule_snapshots_name_ck.
	MaxRuleNameBytes = 1024
)

// Key is the identity of a rule across time: `rule_key` in SPEC §C.6.
//
// It is NOT the content address. Two snapshots sharing a Key and differing in
// Fingerprint are the same rule, edited — which is precisely the fact the whole
// module exists to report.
type Key struct {
	// SourceID is the AlertSource whose Prometheus owns the rule.
	SourceID string
	// File and Group are empty when the origin could not supply them
	// (generatorURL knows the expression but not where it is written down).
	File  string
	Group string
	// Name equals the alertname.
	Name string
}

// IsZero reports whether the key names nothing.
func (k Key) IsZero() bool { return k.SourceID == "" && k.Name == "" }

// Recovery is a rule definition as recovered from an upstream, before it is
// content-addressed and stored. It is the module's INPUT port shape: the
// `sources` module resolves the rule, `internal/app` maps its result onto this,
// and nothing in `rules` needs to know how the lookup was done.
type Recovery struct {
	Origin     Origin
	Strategy   Strategy
	Confidence Confidence
	// CandidateCount is how many rules matched. 0 for none, 1 for exact,
	// >= 2 for ambiguous.
	CandidateCount int

	RuleName  string
	RuleGroup string
	RuleFile  string

	Expr                 string
	ForSeconds           float64
	KeepFiringForSeconds float64
	Labels               map[string]string
	Annotations          map[string]string

	// PrometheusURL is required when Origin is OriginPrometheusAPI.
	PrometheusURL string
	// Notes are stable, greppable reason codes explaining the confidence.
	Notes []string
}

// Recovered reports whether anything usable came back. A false here is the
// normal degraded path, not a failure: the caller stores an `unavailable`
// snapshot so that "we looked and could not see it" is a recorded fact.
func (r Recovery) Recovered() bool {
	return r.Origin != OriginUnavailable && strings.TrimSpace(r.Expr) != ""
}

// Snapshot is a content-addressed capture of one alerting rule at one instant:
// what the rule SAID when the alert fired.
//
// Rows are immutable and deduplicated by (org, source, fingerprint), so a rule
// captured on every fire costs one row until its text changes. That is what
// makes the history a list of EDITS rather than a list of fires.
type Snapshot struct {
	ID    string
	OrgID string
	Key   Key

	// Fingerprint is the content address: sha256 over the definition (§C.6).
	Fingerprint string

	Expr                 string
	ForSeconds           float64
	KeepFiringForSeconds float64
	Labels               map[string]string
	Annotations          map[string]string

	Origin         Origin
	PrometheusURL  string
	Confidence     Confidence
	CandidateCount int
	CapturedAt     time.Time
}

// Available reports whether the snapshot carries a rule definition.
func (s Snapshot) Available() bool { return s.Origin != OriginUnavailable }

// Ambiguous reports whether the match must be surfaced as uncertain. SPEC §D.6
// requires this to reach the UI and the Slack card; it is never hidden.
func (s Snapshot) Ambiguous() bool { return s.Confidence == ConfidenceAmbiguous }

// NewSnapshot builds an unsaved Snapshot from a Recovery, clamping every field
// the DDL bounds and computing the content address.
//
// A Recovery that recovered nothing becomes a well-formed `unavailable`
// snapshot rather than an error: degrading gracefully is the specified
// behaviour, and a snapshot that records the failure is what lets the UI say
// "the rule could not be recovered" instead of showing an empty panel.
func NewSnapshot(orgID string, key Key, r Recovery, capturedAt time.Time) Snapshot {
	s := Snapshot{
		OrgID:          orgID,
		Key:            key,
		Expr:           clamp(r.Expr, MaxExprBytes),
		ForSeconds:     nonNegative(r.ForSeconds),
		Origin:         r.Origin,
		PrometheusURL:  r.PrometheusURL,
		Confidence:     r.Confidence,
		CandidateCount: r.CandidateCount,
		CapturedAt:     capturedAt.UTC(),
	}
	s.KeepFiringForSeconds = nonNegative(r.KeepFiringForSeconds)
	s.Labels = copyMap(r.Labels)
	s.Annotations = copyMap(r.Annotations)

	if s.Key.Name == "" {
		s.Key.Name = clamp(r.RuleName, MaxRuleNameBytes)
	}
	if s.Key.Group == "" {
		s.Key.Group = r.RuleGroup
	}
	if s.Key.File == "" {
		s.Key.File = r.RuleFile
	}
	s.Key.Name = clamp(s.Key.Name, MaxRuleNameBytes)

	// rule_snapshots_expr_ck binds `origin = unavailable` to an empty expr in
	// BOTH directions. Reconcile the pair here rather than letting a CHECK
	// decide it at 3am.
	if s.Expr == "" {
		s.Origin = OriginUnavailable
		s.PrometheusURL = ""
		s.ForSeconds, s.KeepFiringForSeconds = 0, 0
	} else if s.Origin == OriginUnavailable {
		s.Origin = OriginGeneratorURL
	}
	if s.Origin == OriginUnavailable {
		s.Confidence, s.CandidateCount = ConfidenceNone, 0
	}

	s.Fingerprint = Fingerprint(s.Expr, s.ForSeconds, s.KeepFiringForSeconds, s.Labels, s.Annotations)
	return s
}

// Fingerprint is `rule_fingerprint` (SPEC §C.6): the content address of a rule
// definition.
//
//	sha256( expr 0x00 for_seconds 0x00 keep_firing_for_seconds 0x00
//	        canon(rule_labels) 0x00 canon(rule_annotations) )
//
// Durations are rendered with strconv.FormatFloat(f, 'f', -1, 64), the shortest
// representation that round-trips, so 600 and 600.0 are the same rule.
func Fingerprint(expr string, forSeconds, keepFiringForSeconds float64, labels, annotations map[string]string) string {
	h := sha256.New()
	write := func(s string) { _, _ = h.Write([]byte(s)) }
	sep := func() { _, _ = h.Write([]byte{0x00}) }

	write(expr)
	sep()
	write(strconv.FormatFloat(forSeconds, 'f', -1, 64))
	sep()
	write(strconv.FormatFloat(keepFiringForSeconds, 'f', -1, 64))
	sep()
	write(Canon(labels))
	sep()
	write(Canon(annotations))

	return hex.EncodeToString(h.Sum(nil))
}

// Canon is the canonical label serialisation of SPEC §C.1, with no ignore set:
// names sorted ascending by byte order, `name 0x01 value 0x02` per entry.
//
// Names and values are used verbatim (UTF-8, no case folding). This function is
// pure and does no I/O, which is what lets a fingerprint be recomputed anywhere
// and compared byte for byte.
func Canon(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(0x01)
		b.WriteString(m[n])
		b.WriteByte(0x02)
	}
	return b.String()
}

// FingerprintPattern is the shape rule_snapshots_fp_ck enforces: 64 lowercase
// hex characters.
const FingerprintPattern = `^[0-9a-f]{64}$`

// Validate re-checks every invariant the DDL enforces, so an impossible
// combination is caught in Go rather than as a 500 from a CHECK violation
// (SPEC §L.1: a CHECK reaching the HTTP layer means layers 1-3 have a hole).
func (s Snapshot) Validate() error {
	switch {
	case s.OrgID == "":
		return errInvalid("rules_snapshot_no_org", "a snapshot must be org-scoped")
	case s.Key.SourceID == "":
		return errInvalid("rules_snapshot_no_source", "a snapshot must name its alert source")
	case !s.Origin.Valid():
		return errInvalid("rules_snapshot_bad_origin", "origin must be one of prometheus_api, generator_url, unavailable")
	case !s.Confidence.Valid():
		return errInvalid("rules_snapshot_bad_confidence", "match_confidence must be one of exact, probable, ambiguous, none")
	case !isHex64(s.Fingerprint):
		return errInvalid("rules_snapshot_bad_fingerprint", "rule_fingerprint must be 64 lowercase hex characters")
	case len(strings.TrimSpace(s.Key.Name)) == 0 || len(s.Key.Name) > MaxRuleNameBytes:
		return errInvalid("rules_snapshot_bad_name", "rule_name must be 1..1024 bytes")
	case (s.Origin == OriginUnavailable) != (strings.TrimSpace(s.Expr) == ""):
		return errInvalid("rules_snapshot_expr_origin_mismatch", "origin=unavailable and an empty expr must agree")
	case len(s.Expr) > MaxExprBytes:
		return errInvalid("rules_snapshot_expr_too_large", "expr must be at most 65536 bytes")
	case s.ForSeconds < 0 || s.KeepFiringForSeconds < 0:
		return errInvalid("rules_snapshot_negative_duration", "for and keep_firing_for must be non-negative")
	case s.Origin == OriginPrometheusAPI && s.PrometheusURL == "":
		return errInvalid("rules_snapshot_missing_prometheus_url", "origin=prometheus_api requires a prometheus_url")
	case s.CandidateCount < 0:
		return errInvalid("rules_snapshot_negative_candidates", "candidate_count must be non-negative")
	}

	ok := (s.Confidence == ConfidenceNone && s.CandidateCount == 0) ||
		(s.Confidence == ConfidenceExact && s.CandidateCount == 1) ||
		(s.Confidence == ConfidenceProbable && s.CandidateCount >= 1) ||
		(s.Confidence == ConfidenceAmbiguous && s.CandidateCount >= 2)
	if !ok {
		return errInvalid("rules_snapshot_confidence_mismatch",
			"match_confidence and candidate_count disagree (rule_snapshots_conf_ck)")
	}
	return nil
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func clamp(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) > limit {
		return s[:limit]
	}
	return s
}

func nonNegative(f float64) float64 {
	if f < 0 || f != f { // NaN is not storable in a DOUBLE PRECISION CHECK
		return 0
	}
	return f
}

func copyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
