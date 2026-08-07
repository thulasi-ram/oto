package domain

import (
	"time"

	"github.com/google/uuid"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
)

// SourceConfig is the slice of an AlertSource the ingest path actually needs.
//
// It is deliberately NOT `sources/domain.Source`. Ingestion depends on five
// facts — where to redact, what to inject, what not to hash, which cluster the
// alerts belong to, and whether pushes are accepted — and depending on the whole
// sources entity for that would put every future field of that entity on the
// hot path. The port that produces this is declared by ingestion (§F.5 rule 4).
type SourceConfig struct {
	ID        uuid.UUID
	OrgID     uuid.UUID
	ClusterID uuid.UUID
	// ClusterKey participates in Alert identity (C.2): the same label set in
	// prod-eu and prod-us are DIFFERENT Alerts, because they have different blast
	// radii.
	ClusterKey alerts.ClusterKey

	// InjectLabels are merged into every observation BEFORE alert_key is computed,
	// which is how one Alertmanager can serve several logical clusters. An
	// injected label loses to a label the upstream actually sent: oto adds
	// context, it does not overwrite evidence.
	InjectLabels map[string]string
	// IgnoreLabels are excluded from the alert_key hash (§C.1) and STILL STORED.
	IgnoreLabels []string
	// RedactLabels are glob patterns applied to label VALUES before the raw batch
	// is persisted (§C.9.2), so a secret never lands on disk.
	RedactLabels []string
	// RedactAnnotations is RedactLabels for annotation values.
	RedactAnnotations []string

	// PushEnabled gates the webhook endpoint for this source.
	PushEnabled bool
	// DeletedAt is non-nil for a soft-deleted source.
	DeletedAt *time.Time
}

// AcceptsPush reports whether a webhook batch for this source may be processed.
// A source that says no still gets its batch RECORDED as an `unknown_source`
// rejection rather than refused, because a 4xx would delete the notification
// permanently and an operator toggling a flag should not lose evidence.
func (c SourceConfig) AcceptsPush() bool { return c.PushEnabled && c.DeletedAt == nil }

// IngestToken is a resolved, non-revoked `api_tokens` row of kind `ingest`.
//
// An ingest token is scoped to EXACTLY ONE source (api_tokens_ingest_scope) and
// can never read an alert, list a source or reach any other endpoint. That
// narrowness is the whole security model of a credential that lives in an
// `alertmanager.yml` on every cluster.
type IngestToken struct {
	ID       uuid.UUID
	OrgID    uuid.UUID
	SourceID uuid.UUID
}
