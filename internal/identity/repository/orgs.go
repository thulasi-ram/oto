package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
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
	q     db.Querier
	clock clock.Clock
}

// NewOrgRepository builds the repository over a fallback querier. A transaction
// travelling in the context wins over it (CONTEXT.md §5.9).
//
// ⭐ THE CLOCK IS A CONSTRUCTOR ARGUMENT BECAUSE THIS TABLE'S TIME IS THE
// APPLICATION'S. `orgs_time_ck` is `updated_at >= created_at`, and the row is
// CREATED by `internal/app.Bootstrap` from the Go clock. If the only other
// writer — UpdateSettings — stamped `now()`, the two columns would come from two
// different machines, and a Go clock running AHEAD of Postgres would turn the
// first settings change after a fresh install into a 23514 (00033).
func NewOrgRepository(q db.Querier, clk clock.Clock) *OrgRepository {
	if clk == nil {
		clk = clock.New()
	}
	return &OrgRepository{q: q, clock: clk}
}

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

// listLiveOrgsSQL reads ONE keyset page of live orgs, walking the primary key.
//
// `id` is the primary key and a UUIDv7, so `id > $1 ORDER BY id` is a cursor
// that can neither skip nor repeat a tenant when one is created or soft-deleted
// mid-walk. There is no OFFSET here for the reason `db.Keyset` gives for the
// whole codebase.
//
// ⛔ A SOFT-DELETED TENANT IS NOT LISTED. The one caller is the retention fold,
// and a departed tenant's settings widening the whole deployment's partition
// window would be that tenant configuring disk it no longer pays for.
const listLiveOrgsSQL = `
SELECT o.id, o.slug, o.name, o.settings, o.created_at, o.updated_at, o.deleted_at
  FROM orgs o
 WHERE o.deleted_at IS NULL AND o.id > $1
 ORDER BY o.id
 LIMIT $2`

// ListLive reads one keyset page of live orgs, for `identity/service.MaxRetention`.
//
// ⚠️ It is unscoped — it walks the tenant table itself — and it AUTHORISES
// NOTHING: the rows feed a maximum over `orgs.settings`, and a maximum is a
// number, not a scope. Every other read in this file stays behind a
// TenantScope.
func (r *OrgRepository) ListLive(ctx context.Context, after uuid.UUID, limit int) ([]domain.Org, error) {
	rows, err := r.db(ctx).Query(ctx, listLiveOrgsSQL, after, pageLimit(limit))
	if err != nil {
		return nil, mapErr(err, "org_not_found", "org")
	}
	defer rows.Close()

	var out []domain.Org
	for rows.Next() {
		var row orgRow
		if err := rows.Scan(&row.id, &row.slug, &row.name, &row.settings,
			&row.createdAt, &row.updatedAt, &row.deletedAt); err != nil {
			return nil, mapErr(err, "org_not_found", "org")
		}
		org, err := row.toDomain()
		if err != nil {
			return nil, err
		}
		out = append(out, org)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "org_not_found", "org")
	}
	return out, nil
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
//
// ⭐ `updated_at` IS THE INJECTED GO CLOCK, AND IT USED TO BE `now()`. That was
// the MIRROR of the defect 00032 fixed on `channels`, and it was live: the org
// row is written by `internal/app.Bootstrap` with `created_at` from the GO
// clock, while this statement took the DATABASE's. `orgs_time_ck` is
// `updated_at >= created_at`, so an app server whose clock ran AHEAD of Postgres
// failed the first settings write after a fresh install with a 23514 —
// `internal_error/orgs_time_ck`, a 500 on the very first thing a new operator
// does. One clock now stamps both columns (00033).
//
// ⭐ GREATEST KEEPS IT MONOTONIC, for the half the clock alone does not fix.
// "The application owns time" is not "one clock": oto runs N pods with N clocks,
// and the pod serving a settings PATCH is rarely the pod that bootstrapped the
// deployment. A few milliseconds of lag between them writes an `updated_at`
// BELOW `created_at` and reproduces the identical 23514. GREATEST makes the
// check unfalsifiable while leaving the value app-owned; it is the same idiom,
// for the same reason, as `channels` and as OrderingStore.Advance.
const updateSettingsSQL = `
UPDATE orgs
   SET settings   = $2,
       updated_at = GREATEST(updated_at, $3)
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
	err = r.db(ctx).QueryRow(ctx, updateSettingsSQL, s.OrgID(), blob, r.clock.Now().UTC()).
		Scan(&row.id, &row.slug, &row.name, &row.settings, &row.createdAt, &row.updatedAt, &row.deletedAt)
	if err != nil {
		return domain.Org{}, mapErr(err, "org_not_found", "org")
	}
	return row.toDomain()
}
