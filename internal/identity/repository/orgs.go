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

	// The keys added when the tuning surface became editable. They are
	// `omitempty` like the rest, so an org that has never opened the settings
	// screen still stores `{}` and still reports every value as a DEFAULT.
	UnackedReminderAfterS *int    `json:"unacked_reminder_after_s,omitempty"`
	DefaultVerbosity      *string `json:"default_verbosity,omitempty"`
	BroadcastOnResolved   *bool   `json:"broadcast_on_resolved,omitempty"`

	// The unacked-reminder mention surface (ADR 0020). `omitempty` like the rest,
	// so an org that never opened the screen stores nothing and reports every one
	// of these as a DEFAULT — which for the mode is `none`, deliberately.
	UnackedReminderMention            *string   `json:"unacked_reminder_mention,omitempty"`
	UnackedReminderMentionList        *[]string `json:"unacked_reminder_mention_list,omitempty"`
	UnackedReminderMentionMinSeverity *string   `json:"unacked_reminder_mention_min_severity,omitempty"`
}

// toPatch maps the stored blob onto the domain's override record. It is a
// straight field-for-field copy and it must stay one: this is the boundary where
// "absent" is still absent, and any defaulting done here would destroy the origin
// before the domain ever saw it.
func (s settingsJSON) toPatch() domain.SettingsPatch {
	return domain.SettingsPatch{
		RefireGraceS:          s.RefireGraceS,
		ResolveGraceS:         s.ResolveGraceS,
		GroupCloseDelayS:      s.GroupCloseDelayS,
		FlapThreshold:         s.FlapThreshold,
		FlapWindowS:           s.FlapWindowS,
		FlapDigestIntervalS:   s.FlapDigestIntervalS,
		StormThreshold:        s.StormThreshold,
		StormWindowS:          s.StormWindowS,
		StormCooldownS:        s.StormCooldownS,
		RawRetentionDays:      s.RawRetentionDays,
		EventRetentionMonth:   s.EventRetentionMonth,
		UnackedReminderAfterS: s.UnackedReminderAfterS,
		DefaultVerbosity:      s.DefaultVerbosity,
		BroadcastOnResolved:   s.BroadcastOnResolved,

		UnackedReminderMention:            s.UnackedReminderMention,
		UnackedReminderMentionList:        s.UnackedReminderMentionList,
		UnackedReminderMentionMinSeverity: s.UnackedReminderMentionMinSeverity,
	}
}

// fromPatch is toPatch's inverse, for the write path.
func fromPatch(p domain.SettingsPatch) settingsJSON {
	return settingsJSON{
		RefireGraceS:          p.RefireGraceS,
		ResolveGraceS:         p.ResolveGraceS,
		GroupCloseDelayS:      p.GroupCloseDelayS,
		FlapThreshold:         p.FlapThreshold,
		FlapWindowS:           p.FlapWindowS,
		FlapDigestIntervalS:   p.FlapDigestIntervalS,
		StormThreshold:        p.StormThreshold,
		StormWindowS:          p.StormWindowS,
		StormCooldownS:        p.StormCooldownS,
		RawRetentionDays:      p.RawRetentionDays,
		EventRetentionMonth:   p.EventRetentionMonth,
		UnackedReminderAfterS: p.UnackedReminderAfterS,
		DefaultVerbosity:      p.DefaultVerbosity,
		BroadcastOnResolved:   p.BroadcastOnResolved,

		UnackedReminderMention:            p.UnackedReminderMention,
		UnackedReminderMentionList:        p.UnackedReminderMentionList,
		UnackedReminderMentionMinSeverity: p.UnackedReminderMentionMinSeverity,
	}
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

	// The patch is carried through UNCHANGED and the effective settings are
	// DERIVED from it, rather than the two being built independently. One source
	// means the origin the API reports and the value the hot path uses can never
	// describe different worlds — which is the whole failure this reporting exists
	// to prevent.
	patch := s.toPatch()

	return domain.Org{
		ID:        r.id,
		Slug:      r.slug,
		Name:      r.name,
		Settings:  patch.Settings(),
		Overrides: patch,
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

// updateSettingsSQL replaces the whole blob and returns the row it wrote.
//
// ⚠️ IT DOES NOT `jsonb_set` KEY BY KEY, and that is deliberate. The merge is
// done in the domain, on a patch the service has already validated, so there is
// exactly one place that decides what a partial write means. A SQL-side merge
// would be a second such place — one that no test can reach without a database
// and that no bound is checked by.
//
// `updated_at` moves on every write. It is what tells a second pod's next read
// that its numbers are stale, and it is why no cache is needed to make a change
// take effect.
const updateSettingsSQL = `
UPDATE orgs
   SET settings   = $2,
       updated_at = now()
 WHERE id = $1
   AND deleted_at IS NULL
RETURNING id, slug, name, settings, created_at, updated_at, deleted_at`

// UpdateSettings writes this org's overrides and returns the org as stored.
//
// It takes the WHOLE patch, already merged and already validated by the service.
// A repository that accepted a partial patch would have to decide what a missing
// key means, and that decision belongs to the domain.
func (r *OrgRepository) UpdateSettings(
	ctx context.Context, s db.TenantScope, p domain.SettingsPatch,
) (domain.Org, error) {
	blob, err := json.Marshal(fromPatch(p))
	if err != nil {
		// A patch that will not marshal is a mapper bug, never a caller's fault.
		return domain.Org{}, mapErr(err, "org_settings_invalid", "org")
	}

	var row orgRow
	err = r.db(ctx).QueryRow(ctx, updateSettingsSQL, s.OrgID(), blob).
		Scan(&row.id, &row.slug, &row.name, &row.settings, &row.createdAt, &row.updatedAt, &row.deletedAt)
	if err != nil {
		return domain.Org{}, mapErr(err, "org_not_found", "org")
	}
	return row.toDomain()
}
