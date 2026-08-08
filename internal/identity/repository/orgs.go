package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// orgRow is the wire shape of one `orgs` row. It is UNEXPORTED, like every row
// model in oto: three model sets exist and the compiler must be able to tell
// them apart (CONTEXT.md §5.5). Nothing outside this package can name it, so no
// handler can accidentally render one.
type orgRow struct {
	id        uuid.UUID
	slug      string
	name      string
	settings  []byte
	createdAt time.Time
	updatedAt time.Time
	deletedAt *time.Time
}

// settingsJSON is the JSONB payload of `orgs.settings`, whose keys SPEC §D.1
// fixes. Every duration is stored in SECONDS and every retention in its own
// unit; the domain type is durations throughout, so this struct is the one place
// the unit conversion happens.
//
// The fields are pointers so that "absent" and "zero" stay distinguishable. A
// literal 0 in the JSONB would mean "somebody wrote 0", and
// domain.Settings.Normalise reads a non-positive value as unset — collapsing the
// two here would silently disable a damper for every org that never opened the
// settings screen.
type settingsJSON struct {
	RefireGraceS        *int `json:"refire_grace_s,omitempty"`
	ResolveGraceS       *int `json:"resolve_grace_s,omitempty"`
	GroupCloseDelayS    *int `json:"group_close_delay_s,omitempty"`
	FlapThreshold       *int `json:"flap_threshold,omitempty"`
	FlapWindowS         *int `json:"flap_window_s,omitempty"`
	FlapDigestIntervalS *int `json:"flap_digest_interval_s,omitempty"`
	StormThreshold      *int `json:"storm_threshold,omitempty"`
	StormWindowS        *int `json:"storm_window_s,omitempty"`
	StormCooldownS      *int `json:"storm_cooldown_s,omitempty"`
	RawRetentionDays    *int `json:"raw_retention_days,omitempty"`
	EventRetentionMonth *int `json:"event_retention_months,omitempty"`
}

func seconds(p *int) time.Duration {
	if p == nil {
		return 0
	}
	return time.Duration(*p) * time.Second
}

func count(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// toDomain maps the row onto the domain entity, explicitly and field by field.
//
// It does NOT call domain.NewOrg: the constructor's job is to reject input a
// user supplied, and a row that is already in Postgres has passed the CHECKs
// that constructor mirrors. Re-validating here would turn a legacy row into a
// 500 on read, which is the failure mode of validating the wrong direction.
func (r orgRow) toDomain() (domain.Org, error) {
	var s settingsJSON
	if len(r.settings) > 0 {
		if err := json.Unmarshal(r.settings, &s); err != nil {
			// A settings blob that will not parse is a mapper or migration bug,
			// never a caller's fault.
			return domain.Org{}, mapErr(err, "org_not_found", "org")
		}
	}

	return domain.Org{
		ID:   r.id,
		Slug: r.slug,
		Name: r.name,
		Settings: domain.Settings{
			RefireGrace:        seconds(s.RefireGraceS),
			ResolveGrace:       seconds(s.ResolveGraceS),
			GroupCloseDelay:    seconds(s.GroupCloseDelayS),
			FlapThreshold:      count(s.FlapThreshold),
			FlapWindow:         seconds(s.FlapWindowS),
			FlapDigestInterval: seconds(s.FlapDigestIntervalS),
			StormThreshold:     count(s.StormThreshold),
			StormWindow:        seconds(s.StormWindowS),
			StormCooldown:      seconds(s.StormCooldownS),
			RawRetention:       time.Duration(count(s.RawRetentionDays)) * 24 * time.Hour,
			// Months are not a duration Postgres or Go agrees on; §D.1 stores a
			// month count and oto reads it as 30 days, uniformly, everywhere.
			EventRetention: time.Duration(count(s.EventRetentionMonth)) * 30 * 24 * time.Hour,
		}.Normalise(),
		CreatedAt: r.createdAt.UTC(),
		UpdatedAt: r.updatedAt.UTC(),
		DeletedAt: r.deletedAt,
	}, nil
}

// OrgRepository reads the tenant root.
type OrgRepository struct {
	q db.Querier
}

// NewOrgRepository builds the repository over a fallback querier. A transaction
// travelling in the context wins over it (CONTEXT.md §5.9).
func NewOrgRepository(q db.Querier) *OrgRepository { return &OrgRepository{q: q} }

func (r *OrgRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const selectOrgSQL = `
SELECT o.id, o.slug, o.name, o.settings, o.created_at, o.updated_at, o.deleted_at
  FROM orgs o
 WHERE o.id = $1`

// Get returns the caller's own org.
//
// There is no id argument: the scope IS the identity of the row. A method that
// took both would be a method that could be called with a mismatched pair.
func (r *OrgRepository) Get(ctx context.Context, s db.TenantScope) (domain.Org, error) {
	var row orgRow
	err := r.db(ctx).QueryRow(ctx, selectOrgSQL, s.OrgID()).
		Scan(&row.id, &row.slug, &row.name, &row.settings, &row.createdAt, &row.updatedAt, &row.deletedAt)
	if err != nil {
		return domain.Org{}, mapErr(err, "org_not_found", "org")
	}
	return row.toDomain()
}
