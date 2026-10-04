package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// InvestigationRepository is every statement against `investigations` and
// `investigation_steps` (migration 00092).
//
// ⛔⛔ A STEP IS ONLY EVER INSERTED. There is no UPDATE or DELETE of
// `investigation_steps` anywhere in this file — and if one were added,
// `investigation_steps_append_only` would refuse it at the row. An Investigation is
// written while it is `queued` or `running`, by Start and Finish, both of which name
// the status they move FROM in their predicate; once it has ended,
// `investigations_frozen` refuses any further UPDATE.
//
// ⛔ EVERY METHOD IS TENANT-SCOPED, AND `org_id` IS IN EVERY PREDICATE.
type InvestigationRepository struct {
	q db.Querier
}

// NewInvestigationRepository builds the repository over a fallback querier; a
// transaction travelling in the context wins over it.
func NewInvestigationRepository(q db.Querier) *InvestigationRepository {
	return &InvestigationRepository{q: q}
}

func (r *InvestigationRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

func mapInvestigationErr(err error, what string) error {
	return db.MapError(err, db.ErrorPolicy{
		NotFound:           "investigation_not_found",
		NotFoundMessage:    "no such Investigation",
		QueryFailed:        "investigator_query_failed",
		QueryFailedMessage: fmt.Sprintf("could not %s", what),
	})
}

// investigationSelect joins the version and the Investigator, which are immutable
// where an Investigation reads them: a version is never updated, and an Investigator
// is never renamed.
const investigationSelect = `
SELECT n.id, n.org_id, n.subject_kind, n.subject_id, coalesce(n.alert_key, ''),
       n.investigator_id, i.name, n.investigator_version_id, v.version, v.model_endpoint, v.model_name,
       n.status, coalesce(n.reason, ''), coalesce(n.reason_detail, ''),
       n.max_steps, n.max_tokens, n.max_wall_s, n.tokens_in, n.tokens_out, n.tool_calls,
       coalesce(n.finding, ''), n.requested_by, n.requested_by_label,
       n.requested_at, n.started_at, n.ended_at
  FROM investigations n
  JOIN investigators i         ON i.id = n.investigator_id
  JOIN investigator_versions v ON v.id = n.investigator_version_id`

const insertInvestigationSQL = `
INSERT INTO investigations (id, org_id, subject_kind, subject_id, alert_key, investigator_id,
                            investigator_version_id, status, reason, reason_detail,
                            max_steps, max_tokens, max_wall_s, tokens_in, tokens_out, tool_calls,
                            requested_by, requested_by_label, requested_at, ended_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 0, 0, 0, $14, $15, $16, $17)`

// Insert writes a new run: `queued`, or `skipped` with its Ending and EndedAt set.
func (r *InvestigationRepository) Insert(ctx context.Context, s db.TenantScope, inv domain.Investigation) (domain.Investigation, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Investigation{}, err
	}
	nid := id.New()
	var reason, detail *string
	var ended *time.Time
	if inv.Status.Terminal() {
		reason, detail = nullable(string(inv.Ending.Reason)), nullable(inv.Ending.Detail)
		at := inv.EndedAt.UTC()
		ended = &at
	}
	if _, err := r.db(ctx).Exec(ctx, insertInvestigationSQL,
		nid, s.OrgID(), string(inv.SubjectKind), inv.SubjectID, nullable(inv.AlertKey), inv.InvestigatorID,
		inv.VersionID, string(inv.Status), reason, detail,
		inv.Budgets.MaxSteps, inv.Budgets.MaxTokens, inv.Budgets.WallSeconds(),
		nullableID(inv.RequestedBy.UserID), inv.RequestedBy.Label, inv.RequestedAt.UTC(), ended); err != nil {
		return domain.Investigation{}, mapInvestigationErr(err, "record an Investigation")
	}
	return r.Get(ctx, s, nid)
}

// Get reads one run.
func (r *InvestigationRepository) Get(ctx context.Context, s db.TenantScope, nid uuid.UUID) (domain.Investigation, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Investigation{}, err
	}
	if err := db.RequireID("investigation_id", nid); err != nil {
		return domain.Investigation{}, err
	}
	row := r.db(ctx).QueryRow(ctx, investigationSelect+` WHERE n.org_id = $1 AND n.id = $2`, s.OrgID(), nid)
	out, err := scanInvestigation(row)
	if err != nil {
		return domain.Investigation{}, mapInvestigationErr(err, "read an Investigation")
	}
	return out, nil
}

// ListBySubject reads one subject's runs, latest first, served by
// `investigations_subject_idx`.
func (r *InvestigationRepository) ListBySubject(
	ctx context.Context, s db.TenantScope, kind domain.SubjectKind, subjectID uuid.UUID, p db.Keyset,
) ([]domain.Investigation, db.Cursor, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, db.Cursor{}, err
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 25
	}
	var (
		afterAt *time.Time
		afterID *uuid.UUID
	)
	if !p.Cursor.IsZero() {
		at, cid := p.Cursor.SortKey.UTC(), p.Cursor.ID
		afterAt, afterID = &at, &cid
	}
	rows, err := r.db(ctx).Query(ctx, investigationSelect+`
 WHERE n.org_id = $1 AND n.subject_kind = $2 AND n.subject_id = $3
   AND ($4::timestamptz IS NULL OR (n.requested_at, n.id) < ($4, $5::uuid))
 ORDER BY n.requested_at DESC, n.id DESC
 LIMIT $6`, s.OrgID(), string(kind), subjectID, afterAt, afterID, limit+1)
	if err != nil {
		return nil, db.Cursor{}, mapInvestigationErr(err, "list Investigations")
	}
	defer rows.Close()
	out := make([]domain.Investigation, 0, limit+1)
	for rows.Next() {
		inv, err := scanInvestigation(rows)
		if err != nil {
			return nil, db.Cursor{}, mapInvestigationErr(err, "list Investigations")
		}
		out = append(out, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, db.Cursor{}, mapInvestigationErr(err, "list Investigations")
	}
	page, hasMore := db.PageOf(out, limit)
	cursor := db.Cursor{Hash: p.Cursor.Hash}
	if hasMore {
		last := page[len(page)-1]
		cursor = db.Cursor{SortKey: last.RequestedAt, ID: last.ID, Hash: p.Cursor.Hash, HasMore: true}
	}
	return page, cursor, nil
}

// Start moves a `queued` run to `running`. False means it was not queued.
func (r *InvestigationRepository) Start(ctx context.Context, s db.TenantScope, nid uuid.UUID, at time.Time) (bool, error) {
	if err := db.RequireScope(s); err != nil {
		return false, err
	}
	tag, err := r.db(ctx).Exec(ctx, `
UPDATE investigations SET status = 'running', started_at = $3
 WHERE org_id = $1 AND id = $2 AND status = 'queued'`, s.OrgID(), nid, at.UTC())
	if err != nil {
		return false, mapInvestigationErr(err, "start an Investigation")
	}
	return tag.RowsAffected() == 1, nil
}

// Finish ends a run that has not ended. A run that never started (`queued`) gets
// `started_at` now — unless it is `skipped`, which never starts by definition
// (`investigations_started_ck`).
func (r *InvestigationRepository) Finish(
	ctx context.Context, s db.TenantScope, nid uuid.UUID, end domain.Ending, spent domain.Usage,
	toolCalls int, finding string, at time.Time,
) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	if !end.Status.Terminal() {
		return errs.Newf(errs.KindInternal, "investigation_not_ended", "a run cannot be finished as %s", end.Status)
	}
	tag, err := r.db(ctx).Exec(ctx, `
UPDATE investigations
   SET status = $3, reason = $4, reason_detail = $5,
       tokens_in = $6, tokens_out = $7, tool_calls = $8, finding = $9,
       started_at = CASE WHEN $3 = 'skipped' THEN NULL ELSE coalesce(started_at, $10) END,
       ended_at = $10
 WHERE org_id = $1 AND id = $2 AND status IN ('queued','running')`,
		s.OrgID(), nid, string(end.Status), nullable(string(end.Reason)), nullable(end.Detail),
		spent.InputTokens, spent.OutputTokens, toolCalls, nullable(finding), at.UTC())
	if err != nil {
		return mapInvestigationErr(err, "end an Investigation")
	}
	if tag.RowsAffected() == 0 {
		return errs.Conflict("investigation_already_ended", "this Investigation has already ended")
	}
	return nil
}

// stepCall is a Tool call as `investigation_steps.tool_calls` stores it.
type stepCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// AppendStep writes one transcript entry. The only write to `investigation_steps`.
func (r *InvestigationRepository) AppendStep(ctx context.Context, s db.TenantScope, nid uuid.UUID, st domain.Step) error {
	if err := db.RequireScope(s); err != nil {
		return err
	}
	var (
		text, finish, callID, tool, args, outcome, result *string
		calls                                             []byte
		tokIn, tokOut                                     *int64
	)
	switch st.Kind {
	case domain.StepModelTurn:
		cs := make([]stepCall, 0, len(st.Calls))
		for _, c := range st.Calls {
			cs = append(cs, stepCall{ID: c.ID, Name: c.Name, Arguments: c.Arguments})
		}
		b, err := json.Marshal(cs)
		if err != nil {
			return errs.Internal("investigation_step_encode", err)
		}
		calls = b
		text, finish = nullable(st.Text), nullable(string(st.Finish))
		in, out := st.Usage.InputTokens, st.Usage.OutputTokens
		tokIn, tokOut = &in, &out
	case domain.StepToolCall:
		callID, tool, args = &st.Call.ID, &st.Call.Name, &st.Call.Arguments
		o := string(st.Outcome)
		outcome, result = &o, &st.Result
	default:
		return errs.Newf(errs.KindInternal, "investigation_step_kind", "unknown Step kind %q", st.Kind)
	}
	_, err := r.db(ctx).Exec(ctx, `
INSERT INTO investigation_steps (id, org_id, investigation_id, seq, kind, text, tool_calls, finish_reason,
                                 tokens_in, tokens_out, call_id, tool_name, arguments, outcome, result,
                                 duration_ms, recorded_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		id.New(), s.OrgID(), nid, st.Seq, string(st.Kind), text, calls, finish, tokIn, tokOut,
		callID, tool, args, outcome, result, st.Duration.Milliseconds(), st.RecordedAt.UTC())
	if err != nil {
		return mapInvestigationErr(err, "record a Step")
	}
	return nil
}

// MaxSteps bounds one transcript read. A run's step budget is at most 100 Tool calls,
// each with its turn; this is that ceiling with room, not a page size.
const MaxSteps = 1000

// Steps reads a run's transcript, in order.
func (r *InvestigationRepository) Steps(ctx context.Context, s db.TenantScope, nid uuid.UUID) ([]domain.Step, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, `
SELECT id, seq, kind, coalesce(text, ''), tool_calls, coalesce(finish_reason, ''),
       coalesce(tokens_in, 0), coalesce(tokens_out, 0), coalesce(call_id, ''), coalesce(tool_name, ''),
       coalesce(arguments, ''), coalesce(outcome, ''), coalesce(result, ''), duration_ms, recorded_at
  FROM investigation_steps
 WHERE org_id = $1 AND investigation_id = $2
 ORDER BY seq LIMIT $3`, s.OrgID(), nid, MaxSteps)
	if err != nil {
		return nil, mapInvestigationErr(err, "read a transcript")
	}
	defer rows.Close()
	out := []domain.Step{}
	for rows.Next() {
		var (
			st              domain.Step
			kind, finish, o string
			calls           []byte
			durMS           int64
		)
		if err := rows.Scan(&st.ID, &st.Seq, &kind, &st.Text, &calls, &finish,
			&st.Usage.InputTokens, &st.Usage.OutputTokens, &st.Call.ID, &st.Call.Name,
			&st.Call.Arguments, &o, &st.Result, &durMS, &st.RecordedAt); err != nil {
			return nil, mapInvestigationErr(err, "read a transcript")
		}
		st.Kind, st.Finish, st.Outcome = domain.StepKind(kind), domain.FinishReason(finish), domain.ToolOutcome(o)
		st.Duration, st.RecordedAt = time.Duration(durMS)*time.Millisecond, st.RecordedAt.UTC()
		if len(calls) > 0 {
			var cs []stepCall
			if err := json.Unmarshal(calls, &cs); err != nil {
				return nil, errs.Internal("investigation_step_corrupt", err)
			}
			for _, c := range cs {
				st.Calls = append(st.Calls, domain.ToolCall{ID: c.ID, Name: c.Name, Arguments: c.Arguments})
			}
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, mapInvestigationErr(err, "read a transcript")
	}
	return out, nil
}

// PriorFindings reads earlier runs' Findings on the same alert_key, newest first,
// served by `investigations_alert_key_idx`.
func (r *InvestigationRepository) PriorFindings(
	ctx context.Context, s db.TenantScope, alertKey string, except uuid.UUID, limit int,
) ([]domain.PriorFinding, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	rows, err := r.db(ctx).Query(ctx, `
SELECT n.id, n.subject_id, i.name, v.version, n.status, n.finding, n.ended_at
  FROM investigations n
  JOIN investigators i         ON i.id = n.investigator_id
  JOIN investigator_versions v ON v.id = n.investigator_version_id
 WHERE n.org_id = $1 AND n.alert_key = $2 AND n.finding IS NOT NULL AND n.id <> $3
 ORDER BY n.ended_at DESC, n.id DESC
 LIMIT $4`, s.OrgID(), alertKey, except, limit)
	if err != nil {
		return nil, mapInvestigationErr(err, "read prior Findings")
	}
	defer rows.Close()
	out := []domain.PriorFinding{}
	for rows.Next() {
		var (
			p      domain.PriorFinding
			status string
		)
		if err := rows.Scan(&p.InvestigationID, &p.SubjectID, &p.InvestigatorName, &p.VersionNumber,
			&status, &p.Finding, &p.EndedAt); err != nil {
			return nil, mapInvestigationErr(err, "read prior Findings")
		}
		st, err := domain.ParseStatus(status)
		if err != nil {
			return nil, err
		}
		p.Status, p.EndedAt = st, p.EndedAt.UTC()
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, mapInvestigationErr(err, "read prior Findings")
	}
	return out, nil
}

func scanInvestigation(row pgx.Row) (domain.Investigation, error) {
	var (
		out                       domain.Investigation
		kind, status, reason, det string
		maxSteps, maxWall         int
		maxTokens                 int64
		requestedBy               *uuid.UUID
		started, ended            *time.Time
	)
	if err := row.Scan(&out.ID, &out.OrgID, &kind, &out.SubjectID, &out.AlertKey,
		&out.InvestigatorID, &out.InvestigatorName, &out.VersionID, &out.VersionNumber,
		&out.Model.Endpoint, &out.Model.Model,
		&status, &reason, &det, &maxSteps, &maxTokens, &maxWall,
		&out.Spent.InputTokens, &out.Spent.OutputTokens, &out.ToolCalls,
		&out.Finding, &requestedBy, &out.RequestedBy.Label,
		&out.RequestedAt, &started, &ended); err != nil {
		return domain.Investigation{}, err
	}
	sk, err := domain.ParseSubjectKind(kind)
	if err != nil {
		return domain.Investigation{}, err
	}
	st, err := domain.ParseStatus(status)
	if err != nil {
		return domain.Investigation{}, err
	}
	b, err := domain.NewBudgets(maxSteps, maxTokens, maxWall)
	if err != nil {
		return domain.Investigation{}, errs.Internal("investigation_corrupt", err)
	}
	out.SubjectKind, out.Status, out.Budgets = sk, st, b
	if st.Terminal() {
		end, err := domain.RestoreEnding(st, reason, det)
		if err != nil {
			return domain.Investigation{}, err
		}
		out.Ending = end
	}
	if requestedBy != nil {
		out.RequestedBy.UserID = *requestedBy
	}
	out.RequestedAt = out.RequestedAt.UTC()
	if started != nil {
		out.StartedAt = started.UTC()
	}
	if ended != nil {
		out.EndedAt = ended.UTC()
	}
	return out, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullableID(u uuid.UUID) *uuid.UUID {
	if u == uuid.Nil {
		return nil
	}
	return &u
}
