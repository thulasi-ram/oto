package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// SourceConfigRepository reads the five per-source facts the ingest path needs
// from `alert_sources` and `clusters`.
//
// ⚠️ LAYERING NOTE. Those two tables belong to the `sources` module, and the
// long-term shape is an adapter in `internal/app` over `sources/service` — the
// port `service.SourceConfigs` exists precisely so that swap is a one-line change
// with no caller touched. This read-only implementation exists because the ingest
// path cannot start without redaction patterns, `sources` has no Postgres
// repository yet, and the alternative is an endpoint that returns 503 forever.
//
// It is deliberately narrow: SELECT only, five columns plus the cluster key, org
// scoped, and no write path of any kind.
type SourceConfigRepository struct {
	q db.Querier
}

// NewSourceConfigRepository builds the repository over the ingest pool, so that a
// slow dashboard query on the general pool can never delay a webhook (§G.10).
func NewSourceConfigRepository(q db.Querier) *SourceConfigRepository {
	return &SourceConfigRepository{q: q}
}

func (r *SourceConfigRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const sourceConfigSQL = `
SELECT s.id, s.org_id, s.cluster_id, c.cluster_key,
       s.inject_labels, s.ignore_labels, s.redact_labels, s.redact_annotations,
       s.push_enabled, s.deleted_at
  FROM alert_sources s
  JOIN clusters c ON c.id = s.cluster_id AND c.org_id = s.org_id
 WHERE s.org_id = $1 AND s.id = $2`

// Config loads one source's ingest-relevant configuration.
func (r *SourceConfigRepository) Config(ctx context.Context, s db.TenantScope, sourceID uuid.UUID) (domain.SourceConfig, error) {
	var (
		out          domain.SourceConfig
		clusterKey   string
		injectLabels []byte
		deletedAt    *time.Time
	)

	err := r.db(ctx).QueryRow(ctx, sourceConfigSQL, s.OrgID(), sourceID).Scan(
		&out.ID, &out.OrgID, &out.ClusterID, &clusterKey,
		&injectLabels, &out.IgnoreLabels, &out.RedactLabels, &out.RedactAnnotations,
		&out.PushEnabled, &deletedAt,
	)
	if err != nil {
		return domain.SourceConfig{}, mapErr(err, "load the source configuration")
	}

	key, err := alerts.NewClusterKey(clusterKey)
	if err != nil {
		// clusters_key_ck should have made this unrepresentable, so it is oto's bug
		// rather than the caller's — and it is fatal to identity, because
		// cluster_key is hashed into every alert_key (C.2).
		return domain.SourceConfig{}, errs.Wrap(err, errs.KindInternal, "clusters_key_ck",
			"this source's cluster key is not usable for alert identity")
	}
	out.ClusterKey = key
	out.DeletedAt = deletedAt

	if len(injectLabels) > 0 {
		if err := json.Unmarshal(injectLabels, &out.InjectLabels); err != nil {
			// Degrade rather than fail: injecting nothing loses context, failing here
			// loses the alert. The mismatch is visible as an identity that lacks the
			// injected labels, which is recoverable; a dropped webhook is not.
			out.InjectLabels = nil
		}
	}
	return out, nil
}
