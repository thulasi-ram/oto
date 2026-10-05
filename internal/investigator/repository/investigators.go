package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// InvestigatorRepository is every statement against `investigators` and
// `investigator_versions` (migration 00096).
//
// ⛔ A VERSION IS NEVER UPDATED. There is no UPDATE of `investigator_versions` in this
// file: a change to what produces a Finding is an INSERT of version N+1, and the
// Findings that name version N keep naming what produced them.
//
// ⛔ EVERY METHOD IS TENANT-SCOPED, AND `org_id` IS IN EVERY PREDICATE, so another
// org's Investigator is the same answer as one that never existed: 404.
type InvestigatorRepository struct {
	q db.Querier
}

// NewInvestigatorRepository builds the repository over a fallback querier; a
// transaction travelling in the context wins over it.
func NewInvestigatorRepository(q db.Querier) *InvestigatorRepository {
	return &InvestigatorRepository{q: q}
}

func (r *InvestigatorRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

func mapInvestigatorErr(err error, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           "investigator_not_found",
		NotFoundMessage:    "no such Investigator",
		QueryFailed:        "investigator_query_failed",
		QueryFailedMessage: fmt.Sprintf("could not %s", what),
	})
}

// investigatorSelect reads an Investigator joined to its CURRENT version — the one
// with the highest number, which is the one a new run pins.
const investigatorSelect = `
SELECT i.id, i.org_id, i.name, i.enabled, i.max_steps, i.max_tokens, i.max_wall_s, i.min_interval_s,
       i.investigates_incidents, i.created_at, i.updated_at,
       v.id, v.version, v.model_provider_id, v.model_endpoint, v.model_name, v.prompt,
       v.tool_allowlist, v.created_at
  FROM investigators i
  JOIN LATERAL (
        SELECT * FROM investigator_versions v
         WHERE v.org_id = i.org_id AND v.investigator_id = i.id
         ORDER BY v.version DESC LIMIT 1) v ON true`

const insertInvestigatorSQL = `
INSERT INTO investigators (id, org_id, name, enabled, max_steps, max_tokens, max_wall_s, min_interval_s,
                           investigates_incidents, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)`

const insertVersionSQL = `
INSERT INTO investigator_versions (id, org_id, investigator_id, version, model_provider_id,
                                   model_endpoint, model_name, prompt, tool_allowlist, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

// Create writes an Investigator and its version 1. Run it inside the caller's
// transaction: the two rows are one fact.
func (r *InvestigatorRepository) Create(
	ctx context.Context, s db.TenantScope, d domain.InvestigatorDraft, model domain.ModelIdentity, at time.Time,
) (domain.Investigator, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Investigator{}, err
	}
	invID := id.New()
	if _, err := r.db(ctx).Exec(ctx, insertInvestigatorSQL, invID, s.OrgID(), d.Name, d.Enabled,
		d.Budgets.MaxSteps, d.Budgets.MaxTokens, d.Budgets.WallSeconds(), intervalSeconds(d.MinInterval),
		d.InvestigatesIncidents, at.UTC()); err != nil {
		return domain.Investigator{}, mapInvestigatorErr(err, "store an Investigator")
	}
	if _, err := r.AddVersion(ctx, s, invID, 1, d.Spec, model, at); err != nil {
		return domain.Investigator{}, err
	}
	return r.Get(ctx, s, invID)
}

// AddVersion writes version `number`. A number already taken is a 409 naming
// `investigator_versions_number_uniq` — two writers raced, and Lock is what stops that.
func (r *InvestigatorRepository) AddVersion(
	ctx context.Context, s db.TenantScope, investigatorID uuid.UUID, number int,
	spec domain.VersionSpec, model domain.ModelIdentity, at time.Time,
) (domain.Version, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Version{}, err
	}
	vid := id.New()
	if _, err := r.db(ctx).Exec(ctx, insertVersionSQL, vid, s.OrgID(), investigatorID, number,
		spec.ProviderID, model.Endpoint, model.Model, spec.Prompt, spec.Tools.Names(), at.UTC()); err != nil {
		return domain.Version{}, mapInvestigatorErr(err, "store an Investigator version")
	}
	return r.GetVersion(ctx, s, vid)
}

// Get reads one Investigator with its current version.
func (r *InvestigatorRepository) Get(ctx context.Context, s db.TenantScope, invID uuid.UUID) (domain.Investigator, error) {
	return r.getOne(ctx, s, invID, "")
}

// Lock reads one Investigator FOR UPDATE of its own row, inside the caller's
// transaction, so two changes cannot both mint the same next version.
func (r *InvestigatorRepository) Lock(ctx context.Context, s db.TenantScope, invID uuid.UUID) (domain.Investigator, error) {
	return r.getOne(ctx, s, invID, " FOR UPDATE OF i")
}

func (r *InvestigatorRepository) getOne(ctx context.Context, s db.TenantScope, invID uuid.UUID, suffix string) (domain.Investigator, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Investigator{}, err
	}
	if err := db.RequireID("investigator_id", invID); err != nil {
		return domain.Investigator{}, err
	}
	row := r.db(ctx).QueryRow(ctx, investigatorSelect+` WHERE i.org_id = $1 AND i.id = $2`+suffix, s.OrgID(), invID)
	out, err := scanInvestigator(row)
	if err != nil {
		return domain.Investigator{}, mapInvestigatorErr(err, "read an Investigator")
	}
	return out, nil
}

// MaxListedInvestigators bounds List: an org writes a handful.
const MaxListedInvestigators = 200

// List reads an org's Investigators by name, served by `investigators_org_name_uniq`.
func (r *InvestigatorRepository) List(ctx context.Context, s db.TenantScope) ([]domain.Investigator, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, investigatorSelect+` WHERE i.org_id = $1 ORDER BY i.name LIMIT $2`,
		s.OrgID(), MaxListedInvestigators)
	if err != nil {
		return nil, mapInvestigatorErr(err, "list Investigators")
	}
	defer rows.Close()
	out := []domain.Investigator{}
	for rows.Next() {
		inv, err := scanInvestigator(rows)
		if err != nil {
			return nil, mapInvestigatorErr(err, "list Investigators")
		}
		out = append(out, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, mapInvestigatorErr(err, "list Investigators")
	}
	return out, nil
}

// Update writes the mutable half: the kill switch, the budgets, the minimum interval
// and whether Incidents start runs of it.
func (r *InvestigatorRepository) Update(
	ctx context.Context, s db.TenantScope, invID uuid.UUID, enabled bool, b domain.Budgets, interval time.Duration,
	incidents bool, at time.Time,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	tag, err := r.db(ctx).Exec(ctx, `
UPDATE investigators SET enabled = $3, max_steps = $4, max_tokens = $5, max_wall_s = $6, min_interval_s = $7,
                         investigates_incidents = $8, updated_at = $9
 WHERE org_id = $1 AND id = $2`, s.OrgID(), invID, enabled, b.MaxSteps, b.MaxTokens, b.WallSeconds(), intervalSeconds(interval),
		incidents, at.UTC())
	if err != nil {
		return mapInvestigatorErr(err, "update an Investigator")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("investigator_not_found", "no such Investigator")
	}
	return nil
}

const versionColumns = `id, investigator_id, version, model_provider_id, model_endpoint, model_name, prompt, tool_allowlist, created_at`

// MaxListedVersions bounds Versions. A version is a deliberate edit; a hundred is a
// history no settings screen shows in one go.
const MaxListedVersions = 100

// Versions lists an Investigator's versions, newest first.
func (r *InvestigatorRepository) Versions(ctx context.Context, s db.TenantScope, invID uuid.UUID) ([]domain.Version, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, `SELECT `+versionColumns+` FROM investigator_versions
 WHERE org_id = $1 AND investigator_id = $2 ORDER BY version DESC LIMIT $3`, s.OrgID(), invID, MaxListedVersions)
	if err != nil {
		return nil, mapInvestigatorErr(err, "list Investigator versions")
	}
	defer rows.Close()
	out := []domain.Version{}
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, mapInvestigatorErr(err, "list Investigator versions")
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, mapInvestigatorErr(err, "list Investigator versions")
	}
	return out, nil
}

// GetVersion reads one version.
func (r *InvestigatorRepository) GetVersion(ctx context.Context, s db.TenantScope, versionID uuid.UUID) (domain.Version, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Version{}, err
	}
	row := r.db(ctx).QueryRow(ctx, `SELECT `+versionColumns+` FROM investigator_versions
 WHERE org_id = $1 AND id = $2`, s.OrgID(), versionID)
	v, err := scanVersion(row)
	if err != nil {
		return domain.Version{}, mapInvestigatorErr(err, "read an Investigator version")
	}
	return v, nil
}

func scanVersion(row pgx.Row) (domain.Version, error) {
	var (
		v     domain.Version
		tools []string
	)
	if err := row.Scan(&v.ID, &v.InvestigatorID, &v.Number, &v.ProviderID, &v.Model.Endpoint, &v.Model.Model,
		&v.Prompt, &tools, &v.CreatedAt); err != nil {
		return domain.Version{}, err
	}
	allow, err := domain.NewAllowlist(tools)
	if err != nil {
		return domain.Version{}, errs.Internal("investigator_version_corrupt", err)
	}
	v.Tools = allow
	v.CreatedAt = v.CreatedAt.UTC()
	return v, nil
}

func scanInvestigator(row pgx.Row) (domain.Investigator, error) {
	var (
		out       domain.Investigator
		maxSteps  int
		maxTokens int64
		maxWall   int
		interval  int
		tools     []string
	)
	v := &out.Current
	if err := row.Scan(&out.ID, &out.OrgID, &out.Name, &out.Enabled, &maxSteps, &maxTokens, &maxWall, &interval,
		&out.InvestigatesIncidents, &out.CreatedAt, &out.UpdatedAt,
		&v.ID, &v.Number, &v.ProviderID, &v.Model.Endpoint, &v.Model.Model, &v.Prompt, &tools, &v.CreatedAt); err != nil {
		return domain.Investigator{}, err
	}
	b, err := domain.NewBudgets(maxSteps, maxTokens, maxWall)
	if err != nil {
		return domain.Investigator{}, errs.Internal("investigator_corrupt", err)
	}
	allow, err := domain.NewAllowlist(tools)
	if err != nil {
		return domain.Investigator{}, errs.Internal("investigator_version_corrupt", err)
	}
	minInterval, err := domain.NewMinInterval(interval)
	if err != nil {
		return domain.Investigator{}, errs.Internal("investigator_corrupt", err)
	}
	out.Budgets, out.MinInterval, v.Tools, v.InvestigatorID = b, minInterval, allow, out.ID
	out.CreatedAt, out.UpdatedAt, v.CreatedAt = out.CreatedAt.UTC(), out.UpdatedAt.UTC(), v.CreatedAt.UTC()
	return out, nil
}

// intervalSeconds is a minimum interval as `investigators.min_interval_s` stores it.
func intervalSeconds(d time.Duration) int { return int(d / time.Second) }
