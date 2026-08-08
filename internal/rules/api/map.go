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
