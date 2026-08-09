package api

import (
	"time"

	"github.com/google/uuid"
)

// The wire DTOs of the Rules tag, plus the two rule reads the contract files
// under Alerts and Occurrences (`getAlertRuleHistory`, `getOccurrenceRule`).
//
// They live here because this is the only package permitted to name
// `rules/domain` (CONTEXT.md §5.4). Every json tag is byte-identical to
// api/openapi/openapi.yaml.

// RuleSnapshotDTO renders `RuleSnapshotDTO` — **the differentiator**.
//
// Because a snapshot is captured at fire time rather than read live, an episode
// from six weeks ago still reports the threshold that was actually in force then,
// even if the rule has since been changed or deleted outright.
type RuleSnapshotDTO struct {
	ID                   uuid.UUID         `json:"id"`
	SourceID             uuid.UUID         `json:"source_id"`
	RuleFingerprint      string            `json:"rule_fingerprint"`
	RuleFile             string            `json:"rule_file"`
	RuleGroup            string            `json:"rule_group"`
	RuleName             string            `json:"rule_name"`
	Expr                 string            `json:"expr"`
	ForSeconds           float64           `json:"for_seconds"`
	KeepFiringForSeconds float64           `json:"keep_firing_for_seconds"`
	RuleLabels           map[string]string `json:"rule_labels"`
	RuleAnnotations      map[string]string `json:"rule_annotations"`
	Origin               string            `json:"origin"`
	PrometheusURL        *string           `json:"prometheus_url"`
	MatchConfidence      string            `json:"match_confidence"`
	CandidateCount       int32             `json:"candidate_count"`
	CapturedAt           time.Time         `json:"captured_at"`
}

// RuleChangeDTO renders `RuleChangeDTO`: the structured diff between the snapshot
// bound to this occurrence and the one bound to the previous occurrence.
//
// A threshold changing under you is never noise, which is why this is delivered
// to chat regardless of channel verbosity.
type RuleChangeDTO struct {
	PreviousSnapshotID  uuid.UUID            `json:"previous_snapshot_id"`
	PreviousFingerprint string               `json:"previous_fingerprint"`
	PreviousCapturedAt  time.Time            `json:"previous_captured_at"`
	ExprChanged         bool                 `json:"expr_changed"`
	PreviousExpr        *string              `json:"previous_expr"`
	NewExpr             *string              `json:"new_expr"`
	ExprDiff            *RuleExprDiffDTO     `json:"expr_diff"`
	ForChanged          bool                 `json:"for_changed"`
	PreviousForSeconds  *float64             `json:"previous_for_seconds"`
	NewForSeconds       *float64             `json:"new_for_seconds"`
	LabelDiff           map[string][2]string `json:"label_diff,omitempty"`
	AnnotationDiff      map[string][2]string `json:"annotation_diff,omitempty"`
}

// RuleExprDiffDTO renders `RuleExprDiffDTO`: what oto established about HOW the
// expression changed.
//
// On the wire this is a closed union of three variants discriminated by
// `verdict`, and `numbers` exists on the `numbers_moved` variant alone. One Go
// struct produces all three because a Go struct can only ever emit the shape
// changeDTO builds — `Numbers` is `omitempty` and is populated under exactly one
// branch, so no marshalled payload carries a threshold claim oto did not vouch
// for. The contract is what enforces that on the client; this file is what
// enforces it on the server.
//
// A `numbers_moved` verdict with no `numbers` therefore travels as `{"verdict":
// "numbers_moved"}`, and that is the contract's reformat case: same expression,
// same numbers, different whitespace.
//
// `nil` for the whole DTO means the expression did not change. That is a
// statement, not an absence, which is why it is not an empty verdict string.
type RuleExprDiffDTO struct {
	Verdict string                    `json:"verdict"`
	Numbers []RuleExprNumberChangeDTO `json:"numbers,omitempty"`
}

// RuleExprNumberChangeDTO renders `RuleExprNumberChangeDTO`: one numeric
// literal that moved.
//
// `Index` is the literal's ordinal among the literals oto vouched for, and it is
// meaningful on both sides at once precisely because `numbers_moved` means the
// two expressions are congruent.
type RuleExprNumberChangeDTO struct {
	Index         int32   `json:"index"`
	PreviousValue float64 `json:"previous_value"`
	NewValue      float64 `json:"new_value"`
}

// RuleKeyDTO renders the identity across which drift is detected.
//
// It is `(source_id, rule_file, rule_group, rule_name)` and NOT the alert name
// alone: two files can define the same one, which is exactly the ambiguity
// `match_confidence` exists to report.
type RuleKeyDTO struct {
	SourceID  uuid.UUID `json:"source_id"`
	RuleFile  string    `json:"rule_file,omitempty"`
	RuleGroup string    `json:"rule_group,omitempty"`
	RuleName  string    `json:"rule_name"`
}

// RuleHistoryDTO renders `RuleHistoryDTO`.
type RuleHistoryDTO struct {
	RuleKey  RuleKeyDTO        `json:"rule_key"`
	Current  *RuleSnapshotDTO  `json:"current"`
	Change   *RuleChangeDTO    `json:"change"`
	Versions []RuleSnapshotDTO `json:"versions"`
}

// ------------------------------------------------------------- query objects

// ListSnapshotsQuery is the validated form of the `listRuleSnapshots` query.
//
// `source_id` and `rule_name` are REQUIRED because a bare alert name is not an
// identity.
type ListSnapshotsQuery struct {
	SourceID  string `json:"source_id" validate:"required,uuid"`
	RuleName  string `json:"rule_name" validate:"required,min=1,max=1024"`
	RuleGroup string `json:"rule_group" validate:"omitempty,max=4096"`
	RuleFile  string `json:"rule_file"  validate:"omitempty,max=4096"`
	Limit     int    `json:"limit"      validate:"min=1,max=200"`
	Cursor    string `json:"cursor"     validate:"omitempty,cursor"`
}

// HistoryQuery is the validated form of the `getAlertRuleHistory` query.
type HistoryQuery struct {
	Limit int `json:"limit" validate:"min=1,max=200"`
}
