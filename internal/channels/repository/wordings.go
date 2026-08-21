package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// wordingRow is the row model of `wordings`. Unexported, never leaves this
// package (three-model rule, CONTEXT.md §5.5).
type wordingRow struct {
	id        uuid.UUID
	orgID     uuid.UUID
	channelID *uuid.UUID
	stanza    string
	template  string
	matchers  []byte
	reasons   []string
	priority  int32
	enabled   bool
	createdAt time.Time
	updatedAt time.Time
	deletedAt *time.Time
}

// matcherJSON is the stored shape of one `when` term. It mirrors
// notification/repository's `matcherJSON` byte for byte so an operator reading
// either table sees the same three keys.
type matcherJSON struct {
	Name  string `json:"name"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

const wordingColumns = `
	w.id, w.org_id, w.channel_id, w.stanza, w.template, w.matchers, w.reasons,
	w.priority, w.enabled, w.created_at, w.updated_at, w.deleted_at`

const wordingFrom = `
  FROM wordings w`

func (r *wordingRow) scanDest() []any {
	return []any{
		&r.id, &r.orgID, &r.channelID, &r.stanza, &r.template, &r.matchers, &r.reasons,
		&r.priority, &r.enabled, &r.createdAt, &r.updatedAt, &r.deletedAt,
	}
}

// toDomain maps one row onto the entity, re-proving the closed vocabularies. A
// row that cannot become a Wording is a mapper bug and says so (§L.9(1)).
func (r *wordingRow) toDomain() (domain.Wording, error) {
	if !domain.StanzaTakesAWording(r.stanza) {
		return domain.Wording{}, errs.Internal("wording_row_invalid",
			fmt.Errorf("stored wording names stanza %q, which takes no wording", r.stanza))
	}
	var stored []matcherJSON
	if len(r.matchers) > 0 {
		if err := json.Unmarshal(r.matchers, &stored); err != nil {
			return domain.Wording{}, errs.Internal("wording_row_invalid",
				fmt.Errorf("stored wording matchers are not decodable: %w", err))
		}
	}
	matchers := make([]domain.Matcher, 0, len(stored))
	for _, m := range stored {
		op := domain.MatchOp(m.Op)
		if !op.Valid() {
			return domain.Wording{}, errs.Internal("wording_row_invalid",
				fmt.Errorf("stored wording matcher uses operator %q", m.Op))
		}
		matchers = append(matchers, domain.Matcher{Name: m.Name, Op: op, Value: m.Value})
	}
	return domain.Wording{
		ID: r.id, OrgID: r.orgID, ChannelID: r.channelID,
		Stanza: r.stanza, Template: r.template,
		Matchers: matchers, Reasons: r.reasons,
		Priority: int(r.priority), Enabled: r.enabled,
		CreatedAt: r.createdAt, UpdatedAt: r.updatedAt, DeletedAt: r.deletedAt,
	}, nil
}

func encodeMatchers(ms []domain.Matcher) ([]byte, error) {
	out := make([]matcherJSON, 0, len(ms))
	for _, m := range ms {
		out = append(out, matcherJSON{Name: m.Name, Op: string(m.Op), Value: m.Value})
	}
	return json.Marshal(out)
}

// maxResolvedWordings bounds what one delivery will read.
//
// ⛔ AN UNBOUNDED READ ON THE DELIVERY PATH IS A LATENCY BUG WAITING FOR ONE
// CUSTOMER. Only four stanzas are wordable and only the first match per stanza is
// used, so a tenant needs a handful of rows; the ceiling exists so that an org
// that accumulates thousands cannot make every card slower. It is deliberately far
// above any sane configuration, and the ORDER BY means the rows that would have
// won are the ones kept.
const maxResolvedWordings = 512

// WordingRepository reads and writes `wordings`.
type WordingRepository struct {
	q     db.Querier
	clock clock.Clock
}

// NewWordingRepository builds the repository. The clock is injected because the
// application owns time: migrations 00032 and 00033 removed every `DEFAULT now()`
// after the database's clock and the application's raced against one CHECK and
// produced a ~50%-reproducible 23514 on the first write after a create.
func NewWordingRepository(q db.Querier, clk clock.Clock) *WordingRepository {
	if clk == nil {
		clk = clock.New()
	}
	return &WordingRepository{q: q, clock: clk}
}

func (r *WordingRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// ------------------------------------------------------------------- reads

const resolveWordingsSQL = `SELECT ` + wordingColumns + wordingFrom + `
 WHERE w.org_id = $1
   AND w.enabled
   AND w.deleted_at IS NULL
   AND (w.channel_id = $2 OR w.channel_id IS NULL)
 ORDER BY w.channel_id NULLS LAST, w.priority, w.created_at, w.id
 LIMIT $3`

// Resolve returns every live Wording that could apply to one destination, already
// in precedence order: destination-specific first, then the org-wide house voice,
// and within each by priority LOWER FIRST (ADR 0049).
//
// ⭐ THE ORDER IS THE DECISION, AND IT IS MADE IN SQL RATHER THAN IN GO. The
// caller walks the list and takes the first row per Stanza whose `when` clause
// matches, so "which Wording won" is a property of this ORDER BY and of nothing
// else. `(w.channel_id IS NULL)` sorts false before true, which puts the
// destination's own rows ahead of the tenant's — a rule naming one destination is
// more specific than one naming a whole tenant.
//
// ⚠️ IT RETURNS EVERY STANZA'S CANDIDATES IN ONE QUERY, on purpose. A delivery
// needs at most four, and four round trips per delivery to save a few rows would
// be the wrong trade at the rate oto sends.
func (r *WordingRepository) Resolve(
	ctx context.Context, s db.TenantScope, channelID uuid.UUID,
) ([]domain.Wording, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, resolveWordingsSQL, s.OrgID(), channelID, maxResolvedWordings)
	if err != nil {
		return nil, mapErr(err, "wording_not_found", "resolve wordings")
	}
	defer rows.Close()

	var out []domain.Wording
	for rows.Next() {
		var row wordingRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, mapErr(err, "wording_not_found", "scan a wording")
		}
		w, err := row.toDomain()
		if err != nil {
			// ⛔ ONE UNREADABLE ROW DOES NOT BLANK A CARD. A Wording that cannot be
			// mapped is skipped and the built-in Go text is used for its Stanza,
			// which is the same degradation a failing template gets. Refusing the
			// whole delivery over a presentation row would invert the priority
			// between "the card reads oddly" and "the card never arrives".
			continue
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err, "wording_not_found", "resolve wordings")
	}
	return out, nil
}

const getWordingSQL = `SELECT ` + wordingColumns + wordingFrom + ` WHERE w.org_id = $1 AND w.id = $2`

// Get reads one Wording, deleted or not.
func (r *WordingRepository) Get(
	ctx context.Context, s db.TenantScope, wordingID uuid.UUID,
) (domain.Wording, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Wording{}, err
	}
	var row wordingRow
	if err := r.db(ctx).QueryRow(ctx, getWordingSQL, s.OrgID(), wordingID).Scan(row.scanDest()...); err != nil {
		if isNoRows(err) {
			return domain.Wording{}, errs.NotFound("wording_not_found", "no such wording")
		}
		return domain.Wording{}, mapErr(err, "wording_not_found", "read a wording")
	}
	return row.toDomain()
}

const listWordingsSQL = `SELECT ` + wordingColumns + wordingFrom + `
 WHERE w.org_id = $1
   AND ($2 OR w.deleted_at IS NULL)
   AND ($3::uuid IS NULL OR w.channel_id = $3)
   AND ($4::timestamptz IS NULL OR (w.created_at, w.id) < ($4, $5))
 ORDER BY w.created_at DESC, w.id DESC
 LIMIT $6`

// List returns a keyset page of Wordings, newest first. There is no OFFSET in this
// codebase (SPEC §E.1).
//
// A nil channelID lists every Wording in the org, org-wide rows included; a
// non-nil one lists that destination's own rows ONLY, without the org-wide
// fallback, because the settings screen for a channel is editing that channel's
// exceptions and showing the tenant's rows there would invite editing the wrong
// one. Resolve is what merges the two.
func (r *WordingRepository) List(
	ctx context.Context, s db.TenantScope, channelID *uuid.UUID, includeDeleted bool, p db.Keyset,
) ([]domain.Wording, db.Cursor, error) {
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

	rows, err := r.db(ctx).Query(ctx, listWordingsSQL,
		s.OrgID(), includeDeleted, channelID, afterAt, afterID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapErr(err, "wording_not_found", "list wordings")
	}
	defer rows.Close()

	out := make([]domain.Wording, 0, limit+1)
	for rows.Next() {
		var row wordingRow
		if err := rows.Scan(row.scanDest()...); err != nil {
			return nil, db.Cursor{}, mapErr(err, "wording_not_found", "scan a wording")
		}
		w, err := row.toDomain()
		if err != nil {
			return nil, db.Cursor{}, err
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, db.Cursor{}, mapErr(err, "wording_not_found", "list wordings")
	}

	// ⛔ THE HOUSE HELPERS, NOT A HAND-ROLLED SLICE. This trimmed the extra row and
	// built a bare Cursor{SortKey, ID}, leaving HasMore false and dropping the
	// cursor Hash — so `httpx.PageOf` emitted `has_more: false` and no
	// `next_cursor`, and every list was permanently stuck on page one. It read as
	// equivalent to channels.go and was not.
	page, hasMore := db.PageOf(out, limit)
	cur := db.Cursor{Hash: p.Cursor.Hash}
	if len(page) > 0 {
		last := page[len(page)-1]
		cur = db.NextCursor(last.CreatedAt, last.ID, p.Cursor.Hash, hasMore)
	}
	return page, cur, nil
}

// ------------------------------------------------------------------ writes

const createWordingSQL = `
INSERT INTO wordings AS w (id, org_id, channel_id, stanza, template, matchers, reasons,
                      priority, enabled, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)
RETURNING ` + wordingColumns

// Create inserts one Wording.
func (r *WordingRepository) Create(
	ctx context.Context, s db.TenantScope, n domain.NewWording,
) (domain.Wording, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Wording{}, err
	}
	matchers, err := encodeMatchers(n.Matchers)
	if err != nil {
		return domain.Wording{}, errs.Internal("wording_encode_failed", err)
	}
	// ⛔ THE FOREIGN KEY IS ORG-BLIND, SO THE ORG CHECK HAS TO BE HERE.
	// `wordings.channel_id REFERENCES channels(id)` proves the channel EXISTS, not
	// that it is this tenant's — a caller naming another org's channel id would get
	// a row accepted by the database. `Resolve` is org-filtered so nothing would
	// ever render, which makes it a silent no-op rather than a leak; a 404 at the
	// point of the mistake is a better answer than a wording that never fires and
	// never says why.
	if n.ChannelID != nil {
		var exists bool
		if err := r.db(ctx).QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM channels WHERE org_id = $1 AND id = $2)`,
			s.OrgID(), *n.ChannelID).Scan(&exists); err != nil {
			return domain.Wording{}, mapErr(err, "wording_not_found", "check a wording's channel")
		}
		if !exists {
			return domain.Wording{}, errs.NotFound("channel_not_found",
				"no such channel in this organisation")
		}
	}

	newID := n.ID
	if newID == uuid.Nil {
		newID = id.New()
	}
	reasons := n.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	// Both clock columns come from ONE read, so `updated_at >= created_at` cannot
	// fail on a row that was never updated.
	now := r.clock.Now().UTC()

	var row wordingRow
	err = r.db(ctx).QueryRow(ctx, createWordingSQL,
		newID, s.OrgID(), n.ChannelID, n.Stanza, n.Template, matchers, reasons,
		n.Priority, n.Enabled, now,
	).Scan(row.scanDest()...)
	if err != nil {
		return domain.Wording{}, mapErr(err, "wording_not_found", "create a wording")
	}
	return row.toDomain()
}

const updateWordingSQL = `
UPDATE wordings w SET
  template   = COALESCE($3, w.template),
  matchers   = COALESCE($4, w.matchers),
  reasons    = COALESCE($5, w.reasons),
  priority   = COALESCE($6, w.priority),
  enabled    = COALESCE($7, w.enabled),
  updated_at = GREATEST(w.updated_at, $8)
WHERE w.org_id = $1 AND w.id = $2 AND w.deleted_at IS NULL
RETURNING ` + wordingColumns

// Update applies a partial patch.
//
// `updated_at = GREATEST(...)` keeps the column monotonic across pods whose clocks
// disagree, which is the same guard every other table here uses and the reason
// six tables stopped 500ing when a lagging pod wrote second.
//
// ⛔ THE STANZA IS NOT PATCHABLE. Moving a Wording from `body` to `title` is not an
// edit of that Wording, it is a different Wording — the read set differs, the
// budget differs, and the row's history would claim it had always been the new
// one. Delete and create.
func (r *WordingRepository) Update(
	ctx context.Context, s db.TenantScope, wordingID uuid.UUID, p domain.WordingPatch,
) (domain.Wording, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Wording{}, err
	}
	var matchers any
	if p.Matchers != nil {
		raw, err := encodeMatchers(*p.Matchers)
		if err != nil {
			return domain.Wording{}, errs.Internal("wording_encode_failed", err)
		}
		matchers = raw
	}
	var reasons any
	if p.Reasons != nil {
		v := *p.Reasons
		if v == nil {
			v = []string{}
		}
		reasons = v
	}

	var row wordingRow
	err := r.db(ctx).QueryRow(ctx, updateWordingSQL,
		s.OrgID(), wordingID, p.Template, matchers, reasons, p.Priority, p.Enabled,
		r.clock.Now().UTC(),
	).Scan(row.scanDest()...)
	if err != nil {
		if isNoRows(err) {
			return domain.Wording{}, errs.NotFound("wording_not_found", "no such wording")
		}
		return domain.Wording{}, mapErr(err, "wording_not_found", "update a wording")
	}
	return row.toDomain()
}

const deleteWordingSQL = `
UPDATE wordings w SET enabled = FALSE, deleted_at = $3, updated_at = GREATEST(w.updated_at, $3)
WHERE w.org_id = $1 AND w.id = $2 AND w.deleted_at IS NULL`

// Delete soft-deletes one Wording.
//
// Soft, like every other delete here: a delivery's persisted wording set names the
// rows that produced a card, and a hard delete would make an old card's provenance
// unreadable.
func (r *WordingRepository) Delete(
	ctx context.Context, s db.TenantScope, wordingID uuid.UUID,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	tag, err := r.db(ctx).Exec(ctx, deleteWordingSQL, s.OrgID(), wordingID, r.clock.Now().UTC())
	if err != nil {
		return mapErr(err, "wording_not_found", "delete a wording")
	}
	if tag.RowsAffected() == 0 {
		return errs.NotFound("wording_not_found", "no such wording")
	}
	return nil
}
