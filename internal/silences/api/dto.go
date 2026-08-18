package api

import (
	"time"

	"github.com/google/uuid"
)

// The wire DTOs of the Silences tag. Every json tag is byte-identical to
// api/openapi/openapi.yaml.

// SilenceMatcherDTO renders `SilenceMatcherDTO`.
//
// The four operators are encoded as `(is_regex, is_equal)` exactly as
// Alertmanager encodes them — `=` is (false,true), `!=` is (false,false), `=~` is
// (true,true), `!~` is (true,false) — and `op` is the same thing rendered for
// convenience. Both are emitted because the pair is what upstream stores and the
// operator is what a human reads.
type SilenceMatcherDTO struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"is_regex"`
	IsEqual bool   `json:"is_equal"`
	Op      string `json:"op,omitempty"`
}

// SilenceDTO renders `SilenceDTO`: a READ-ONLY mirror of an Alertmanager silence.
//
// oto cannot create, edit or expire one — only show you one. `alertmanager_url`
// is v1's only silence affordance.
type SilenceDTO struct {
	ID              uuid.UUID           `json:"id"`
	SourceID        uuid.UUID           `json:"source_id"`
	SourceSilenceID string              `json:"source_silence_id"`
	Matchers        []SilenceMatcherDTO `json:"matchers"`
	StartsAt        time.Time           `json:"starts_at"`
	EndsAt          time.Time           `json:"ends_at"`
	CreatedBy       string              `json:"created_by"`
	Comment         string              `json:"comment"`
	Annotations     map[string]string   `json:"annotations,omitempty"`
	State           string              `json:"state"`
	SourceUpdatedAt *time.Time          `json:"source_updated_at"`
	MirroredAt      time.Time           `json:"mirrored_at"`
	AlertmanagerURL *string             `json:"alertmanager_url"`
}

// SilenceDetailDTO renders `SilenceDetailDTO`.
//
// `matched_alerts` is oto's BELIEF about coverage, computed from the mirrored
// matchers. Alertmanager remains the authority on what is actually suppressed.
type SilenceDetailDTO struct {
	SilenceDTO
	MatchedAlerts []AlertRefDTO `json:"matched_alerts"`
	MatchedCount  int32         `json:"matched_count"`
}

// AlertRefDTO renders `AlertRefDTO`: a compact Alert reference.
type AlertRefDTO struct {
	ID         uuid.UUID `json:"id"`
	AlertKey   string    `json:"alert_key"`
	AlertName  string    `json:"alertname"`
	Severity   *string   `json:"severity"`
	Namespace  *string   `json:"namespace"`
	ClusterKey string    `json:"cluster_key"`
	State      string    `json:"state"`
}

// ListSilencesQuery is the validated form of the `listSilences` query string.
type ListSilencesQuery struct {
	State     []string `json:"state"      validate:"omitempty,max=3,unique,dive,oneof=active pending expired"`
	SourceID  string   `json:"source_id"  validate:"omitempty,uuid"`
	CreatedBy string   `json:"created_by" validate:"omitempty,max=256"`
	Q         string   `json:"q"          validate:"omitempty,max=200"`
	Limit     int      `json:"limit"      validate:"min=1,max=200"`
	Cursor    string   `json:"cursor"     validate:"omitempty,cursor"`
}
