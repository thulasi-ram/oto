package api

import (
	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/rules/domain"
)

// The domain → DTO mappers. Every field is copied by hand.

func snapshotDTO(s domain.Snapshot) RuleSnapshotDTO {
	return RuleSnapshotDTO{
		ID:                   parseUUID(s.ID),
		SourceID:             parseUUID(s.Key.SourceID),
		RuleFingerprint:      s.Fingerprint,
		RuleFile:             s.Key.File,
		RuleGroup:            s.Key.Group,
		RuleName:             s.Key.Name,
		Expr:                 s.Expr,
		ForSeconds:           s.ForSeconds,
		KeepFiringForSeconds: s.KeepFiringForSeconds,
		RuleLabels:           emptyMap(s.Labels),
		RuleAnnotations:      emptyMap(s.Annotations),
		Origin:               string(s.Origin),
		PrometheusURL:        strPtr(s.PrometheusURL),
		MatchConfidence:      string(s.Confidence),
		CandidateCount:       int32(s.CandidateCount),
		CapturedAt:           s.CapturedAt.UTC(),
	}
}

func keyDTO(k domain.Key) RuleKeyDTO {
	return RuleKeyDTO{
		SourceID:  parseUUID(k.SourceID),
		RuleFile:  k.File,
		RuleGroup: k.Group,
		RuleName:  k.Name,
	}
}

// changeDTO renders a diff.
//
// `label_diff` and `annotation_diff` are `name → [old, new]`; an empty string on
// one side means the entry was absent there, which is how the contract encodes an
// addition or a removal without a third shape.
func changeDTO(d domain.Diff) RuleChangeDTO {
	out := RuleChangeDTO{
		PreviousSnapshotID:  parseUUID(d.From.ID),
		PreviousFingerprint: d.From.Fingerprint,
		PreviousCapturedAt:  d.From.CapturedAt.UTC(),
		ExprChanged:         d.ExprChanged,
		ForChanged:          d.ForChanged,
	}
	if d.ExprChanged {
		out.PreviousExpr = strPtr(d.From.Expr)
		out.NewExpr = strPtr(d.To.Expr)
		out.ExprDiff = exprDiffDTO(d)
	}
	if d.ForChanged {
		from, to := d.From.ForSeconds, d.To.ForSeconds
		out.PreviousForSeconds = &from
		out.NewForSeconds = &to
	}
	if len(d.Labels) > 0 {
		out.LabelDiff = mapChanges(d.Labels)
	}
	if len(d.Annotations) > 0 {
		out.AnnotationDiff = mapChanges(d.Annotations)
	}
	return out
}

// exprDiffDTO renders the expression verdict, and is the server half of the
// contract's "no threshold claim without a verdict that vouches for one".
//
// The branch is on `ExprStructural`, which is the domain's own answer to "is a
// numeric narrative on offer at all?" — it is true for BOTH no-claim verdicts.
// Branching on it rather than on `ExprVerdict != ExprNumbersMoved` means a
// fourth verdict added to the domain tomorrow arrives here as "no numbers"
// rather than as a silently empty `numbers_moved`. `ExprVerdict` then says which
// no-claim verdict it was, because "the shape moved" and "oto will not say what
// moved" are different facts and the UI renders them differently.
//
// A nil return means the expression did not change: `ExprNotCompared` has no
// wire spelling, by design.
func exprDiffDTO(d domain.Diff) *RuleExprDiffDTO {
	if d.ExprVerdict == domain.ExprNotCompared {
		return nil
	}
	if d.ExprStructural {
		return &RuleExprDiffDTO{Verdict: string(d.ExprVerdict)}
	}

	// No numbers under `numbers_moved` is not a missing answer: it is the
	// contract's "the two expressions are the same expression with the same
	// numbers in it", i.e. a reformat. The verdict already carries that; the
	// empty list is what it looks like.
	var numbers []RuleExprNumberChangeDTO
	for _, n := range d.ExprNumbers {
		numbers = append(numbers, RuleExprNumberChangeDTO{
			Index:         int32(n.Index),
			PreviousValue: n.Old,
			NewValue:      n.New,
		})
	}
	return &RuleExprDiffDTO{Verdict: string(d.ExprVerdict), Numbers: numbers}
}

func mapChanges(cs []domain.MapChange) map[string][2]string {
	out := make(map[string][2]string, len(cs))
	for _, c := range cs {
		out[c.Name] = [2]string{c.Old, c.New}
	}
	return out
}

// ------------------------------------------------------------------ helpers

// parseUUID renders a domain id, which travels as a string because the rules
// domain is storage-agnostic. A malformed one becomes the nil UUID rather than an
// error: the row exists, and refusing to render it would hide a real snapshot
// behind an id-formatting complaint.
func parseUUID(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil
	}
	return id
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func emptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
