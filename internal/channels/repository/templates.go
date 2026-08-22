package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// templateRow is the row model of `notification_templates`. Unexported, never
// leaves this package (three-model rule, CONTEXT.md §5.5).
type templateRow struct {
	id        uuid.UUID
	orgID     uuid.UUID
	name      string
	provider  string
	format    string
	source    string
	version   int32
	enabled   bool
	createdAt time.Time
	updatedAt time.Time
	deletedAt *time.Time
}

func (r *templateRow) scanDest() []any {
	return []any{
		&r.id, &r.orgID, &r.name, &r.provider, &r.format, &r.source,
		&r.version, &r.enabled, &r.createdAt, &r.updatedAt, &r.deletedAt,
	}
}

func (r templateRow) toDomain() (domain.NotificationTemplate, error) {
	return domain.NotificationTemplate{
		ID: r.id, OrgID: r.orgID, Name: r.name,
		Provider: r.provider, Format: r.format, Source: r.source,
		Version: int(r.version), Enabled: r.enabled,
		CreatedAt: r.createdAt.UTC(), UpdatedAt: r.updatedAt.UTC(),
		DeletedAt: r.deletedAt,
	}, nil
}

const templateColumns = ` t.id, t.org_id, t.name, t.provider, t.format, t.source,
       t.version, t.enabled, t.created_at, t.updated_at, t.deleted_at`

const templateFrom = ` FROM notification_templates t`

// TemplateRepository is the persistence side of NotificationTemplate.
type TemplateRepository struct {
	q     db.Querier
	clock clock.Clock
}

// NewTemplateRepository builds the repository.
func NewTemplateRepository(q db.Querier, clk clock.Clock) *TemplateRepository {
	if clk == nil {
		clk = clock.New()
	}
	return &TemplateRepository{q: q, clock: clk}
}

func (r *TemplateRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// ------------------------------------------------------------------- reads

const getTemplateSQL = `SELECT ` + templateColumns + templateFrom + `
 WHERE t.org_id = $1 AND t.id = $2`

// Get reads one template, deleted rows included.
//
// A soft-deleted row is served rather than hidden. A delivery records the
// template id that produced it, so "why did my card read like that" has to stay
// answerable after the template is gone — which is the same argument that made
// the delete soft in the first place.
func (r *TemplateRepository) Get(
	ctx context.Context, s db.TenantScope, templateID uuid.UUID,
) (domain.NotificationTemplate, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.NotificationTemplate{}, err
	}
	var row templateRow
	if err := r.db(ctx).QueryRow(ctx, getTemplateSQL, s.OrgID(), templateID).Scan(row.scanDest()...); err != nil {
		if isNoRows(err) {
			return domain.NotificationTemplate{}, errs.NotFound("template_not_found", "no such template")
		}
		return domain.NotificationTemplate{}, mapErr(err, "template_not_found", "read a template")
	}
	return row.toDomain()
}

const forPolicySQL = `SELECT ` + templateColumns + `
  FROM notification_policies p
  JOIN notification_templates t ON t.id = p.template_id
 WHERE p.org_id = $1 AND p.id = $2
   AND t.org_id = $1
   AND t.enabled AND t.deleted_at IS NULL`

// ForPolicy reads the live template a policy names.
//
// ⭐ IT IS A JOIN AND NOT A SEARCH, WHICH IS THE POINT OF THE PIVOT. The
// predecessor walked an ordered candidate list evaluating a second matcher
// vocabulary with a second precedence rule. Selection now rides the routing
// decision that has already been made, so "which template won" has exactly one
// answer and it is a foreign key.
//
// ⛔ `t.org_id = $1` IS NOT REDUNDANT BESIDE THE POLICY'S SCOPE. The foreign key
// on `template_id` references `notification_templates(id)` ALONE, with no org
// term, so a row written by any route that bypassed the service could point at
// another tenant's prose. The join term is the floor under that.
func (r *TemplateRepository) ForPolicy(
	ctx context.Context, s db.TenantScope, policyID uuid.UUID,
) (domain.NotificationTemplate, bool, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.NotificationTemplate{}, false, err
	}
	var row templateRow
	err := r.db(ctx).QueryRow(ctx, forPolicySQL, s.OrgID(), policyID).Scan(row.scanDest()...)
	if err != nil {
		if isNoRows(err) {
			// The policy has no template, or it is disabled or deleted. All three
			// mean the same thing to a delivery: oto's own card.
			return domain.NotificationTemplate{}, false, nil
		}
		return domain.NotificationTemplate{}, false, mapErr(err, "template_not_found", "read a policy's template")
	}
	t, err := row.toDomain()
	return t, err == nil, err
}

const listTemplatesSQL = `SELECT ` + templateColumns + templateFrom + `
 WHERE t.org_id = $1
   AND ($2 OR t.deleted_at IS NULL)
   AND ($3::timestamptz IS NULL OR (t.created_at, t.id) < ($3, $4))
 ORDER BY t.created_at DESC, t.id DESC
 LIMIT $5`

// List returns a keyset page of templates, newest first. There is no OFFSET in
// this codebase (SPEC §E.1).
func (r *TemplateRepository) List(
	ctx context.Context, s db.TenantScope, includeDeleted bool, p db.Keyset,
) ([]domain.NotificationTemplate, db.Cursor, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, db.Cursor{}, err
	}
	limit := db.ClampLimit(p.Limit)

	var (
		afterAt *time.Time
		afterID uuid.UUID
	)
	if !p.Cursor.IsZero() {
		at := p.Cursor.SortKey.UTC()
		afterAt, afterID = &at, p.Cursor.ID
	}

	rows, err := r.db(ctx).Query(ctx, listTemplatesSQL, s.OrgID(), includeDeleted, afterAt, afterID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "template_not_found", "list templates")
	}
	defer rows.Close()

	out := make([]domain.NotificationTemplate, 0, limit+1)
	for rows.Next() {
		var row templateRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, db.Cursor{}, mapErr(err, "template_not_found", "scan a template")
		}
		t, err := row.toDomain()
		if err != nil {
			return nil, db.Cursor{}, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, db.Cursor{}, mapErr(err, "template_not_found", "list templates")
	}

	// The house helpers, not a hand-rolled slice: a bare Cursor{SortKey, ID} leaves
	// HasMore false and drops the Hash, which pins every list on page one.
	page, hasMore := db.PageOf(out, limit)
	cur := db.Cursor{Hash: p.Cursor.Hash}
	if len(page) > 0 {
		last := page[len(page)-1]
		cur = db.NextCursor(last.CreatedAt, last.ID, p.Cursor.Hash, hasMore)
	}
	return page, cur, nil
}

// ------------------------------------------------------------------ writes

const createTemplateSQL = `
INSERT INTO notification_templates AS t
  (id, org_id, name, provider, format, source, version, enabled, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, 1, $7, $8, $8)
RETURNING ` + templateColumns

// Create writes a new template at version 1.
func (r *TemplateRepository) Create(
	ctx context.Context, s db.TenantScope, n domain.NewNotificationTemplate,
) (domain.NotificationTemplate, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.NotificationTemplate{}, err
	}
	now := r.clock.Now().UTC()
	var row templateRow
	err := r.db(ctx).QueryRow(ctx, createTemplateSQL,
		n.ID, s.OrgID(), n.Name, n.Provider, n.Format, n.Source, n.Enabled, now,
	).Scan(row.scanDest()...)
	if err != nil {
		return domain.NotificationTemplate{}, mapErr(err, "template_not_created", "create a template")
	}
	return row.toDomain()
}

const updateTemplateSQL = `
UPDATE notification_templates t SET
  name     = COALESCE($3, t.name),
  provider = COALESCE($4, t.provider),
  format   = COALESCE($5, t.format),
  source   = COALESCE($6, t.source),
  enabled  = COALESCE($7, t.enabled),
  -- ⭐ THE VERSION BUMPS ONLY WHEN WHAT IT ATTRIBUTES CHANGES. A delivery row
  -- records (template_id, version) so a card can be traced to a revision. Renaming
  -- a template or disabling it does not change a single byte any past card
  -- rendered, so bumping then would make the attribution lie in the other
  -- direction: two versions that produced identical output.
  version = t.version + CASE
    WHEN ($6 IS NOT NULL AND $6 <> t.source) OR ($5 IS NOT NULL AND $5 <> t.format)
    THEN 1 ELSE 0 END,
  updated_at = GREATEST(t.updated_at, $8)
WHERE t.org_id = $1 AND t.id = $2 AND t.deleted_at IS NULL
RETURNING ` + templateColumns

// Update applies a partial patch.
//
// `updated_at = GREATEST(...)` keeps the column monotonic across pods whose clocks
// disagree, which is the same guard every other table here uses and the reason six
// tables stopped 500ing when a lagging pod wrote second.
func (r *TemplateRepository) Update(
	ctx context.Context, s db.TenantScope, templateID uuid.UUID, p domain.NotificationTemplatePatch,
) (domain.NotificationTemplate, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.NotificationTemplate{}, err
	}
	var row templateRow
	err := r.db(ctx).QueryRow(ctx, updateTemplateSQL,
		s.OrgID(), templateID, p.Name, p.Provider, p.Format, p.Source, p.Enabled,
		r.clock.Now().UTC(),
	).Scan(row.scanDest()...)
	if err != nil {
		if isNoRows(err) {
			return domain.NotificationTemplate{}, errs.NotFound("template_not_found", "no such template")
		}
		return domain.NotificationTemplate{}, mapErr(err, "template_not_found", "update a template")
	}
	return row.toDomain()
}

const deleteTemplateSQL = `
UPDATE notification_templates t SET enabled = FALSE, deleted_at = $3, updated_at = GREATEST(t.updated_at, $3)
WHERE t.org_id = $1 AND t.id = $2 AND t.deleted_at IS NULL`

// Delete soft-deletes a template.
//
// ⛔ SOFT, BECAUSE A DELIVERY POINTS AT IT. Every card this template ever rendered
// carries its id, and "why did my card read like that" has to stay answerable.
// The policies naming it fall back to oto's built-in card, by the ON DELETE SET
// NULL on `notification_policies.template_id` — which this does not trigger, so
// the policy keeps pointing at a row that `ForPolicy` will no longer return. That
// is the intended reading: the policy remembers what it was told, and the delivery
// gets the default voice.
func (r *TemplateRepository) Delete(ctx context.Context, s db.TenantScope, templateID uuid.UUID) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	tag, err := r.db(ctx).Exec(ctx, deleteTemplateSQL, s.OrgID(), templateID, r.clock.Now().UTC())
	if err != nil {
		return mapErr(err, "template_not_found", "delete a template")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("template_not_found", "no such template")
	}
	return nil
}
