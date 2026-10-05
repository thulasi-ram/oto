package repository

import (
	"context"
	"encoding/json"
	"errors"
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
       coalesce(n.finding, ''), coalesce(n.classification, ''), n.requested_by, n.requested_by_label,
       n.requested_at, n.not_before, n.started_at, n.ended_at,
       n.digest_window_start, n.digest_window_end
  FROM investigations n
  JOIN investigators i         ON i.id = n.investigator_id
  JOIN investigator_versions v ON v.id = n.investigator_version_id`

const insertInvestigationSQL = `
INSERT INTO investigations (id, org_id, subject_kind, subject_id, alert_key, investigator_id,
                            investigator_version_id, status, reason, reason_detail,
                            max_steps, max_tokens, max_wall_s, tokens_in, tokens_out, tool_calls,
                            requested_by, requested_by_label, requested_at, not_before, ended_at,
                            digest_window_start, digest_window_end)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 0, 0, 0, $14, $15, $16, $17, $18, $19, $20)`

// Insert writes a new run: `queued`, or `skipped` with its Ending and EndedAt set.
func (r *InvestigationRepository) Insert(ctx context.Context, s db.TenantScope, inv domain.Investigation) (domain.Investigation, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Investigation{}, err
	}
	nid := id.New()
	var reason, detail *string
	var ended, notBefore, windowStart, windowEnd *time.Time
	if !inv.DigestWindow.IsZero() {
		ws, we := inv.DigestWindow.Start.UTC(), inv.DigestWindow.End.UTC()
		windowStart, windowEnd = &ws, &we
	}
	if !inv.NotBefore.IsZero() {
		nb := inv.NotBefore.UTC()
		notBefore = &nb
	}
	if inv.Status.Terminal() {
		reason, detail = nullable(string(inv.Ending.Reason)), nullable(inv.Ending.Detail)
		at := inv.EndedAt.UTC()
		ended = &at
	}
	if _, err := r.db(ctx).Exec(ctx, insertInvestigationSQL,
		nid, s.OrgID(), string(inv.SubjectKind), inv.SubjectID, nullable(inv.AlertKey), inv.InvestigatorID,
		inv.VersionID, string(inv.Status), reason, detail,
		inv.Budgets.MaxSteps, inv.Budgets.MaxTokens, inv.Budgets.WallSeconds(),
		nullableID(inv.RequestedBy.UserID), inv.RequestedBy.Label, inv.RequestedAt.UTC(), notBefore, ended,
		windowStart, windowEnd); err != nil {
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

// Start moves a `queued` run to `running` — unless the org already has `maxRunning`
// running, in which case it stays queued and StartAtCapacity says so (ADR 0053 §6:
// "Concurrency — waits in the job queue; never dropped").
//
// ⭐ RACE-SAFE BY AN ADVISORY LOCK, AND ONLY INSIDE A TRANSACTION. The count and the
// UPDATE are two statements; under READ COMMITTED two workers would each count one
// free slot and both take it. So Start first takes the org's transaction-scoped
// advisory lock (LockNamespaceInvestigations, "runs/<org>"), and the lock is released
// by the COMMIT that makes this run's `running` visible to the next counter. Outside
// a transaction the lock would guard nothing, and db.AdvisoryXactLock refuses it.
func (r *InvestigationRepository) Start(
	ctx context.Context, s db.TenantScope, nid uuid.UUID, at time.Time, maxRunning int,
) (domain.StartOutcome, error) {
	if err := db.RequireScope(s); err != nil {
		return 0, err
	}
	if err := db.AdvisoryXactLock(ctx, r.db(ctx), orgRunsKey(s)); err != nil {
		return 0, errs.Internal("investigation_lock", err)
	}
	running, err := r.CountRunning(ctx, s)
	if err != nil {
		return 0, err
	}
	if running >= maxRunning {
		return domain.StartAtCapacity, nil
	}
	tag, err := r.db(ctx).Exec(ctx, `
UPDATE investigations SET status = 'running', started_at = $3
 WHERE org_id = $1 AND id = $2 AND status = 'queued'`, s.OrgID(), nid, at.UTC())
	if err != nil {
		return 0, mapInvestigationErr(err, "start an Investigation")
	}
	if tag.RowsAffected() != 1 {
		return domain.StartNotQueued, nil
	}
	return domain.StartBegan, nil
}

// CountRunning counts the org's `running` runs, served by `investigations_running_idx`.
// Read alone it is a peek — Start is the count that decides.
func (r *InvestigationRepository) CountRunning(ctx context.Context, s db.TenantScope) (int, error) {
	if err := db.RequireScope(s); err != nil {
		return 0, err
	}
	var n int
	if err := r.db(ctx).QueryRow(ctx,
		`SELECT count(*) FROM investigations WHERE org_id = $1 AND status = 'running'`, s.OrgID()).Scan(&n); err != nil {
		return 0, mapInvestigationErr(err, "count running Investigations")
	}
	return n, nil
}

// SpentSince sums the input + output tokens of every model turn the org's runs have
// recorded at or after `since`, served by `investigation_steps_spend_idx`.
//
// ⭐ STEPS, NOT RUNS: a turn's tokens are on its Step the moment it happens, so a run
// still going is counted, and a run that crossed midnight counts in each day it spent
// in. A run's own tokens_in / tokens_out are written only when it ends.
func (r *InvestigationRepository) SpentSince(ctx context.Context, s db.TenantScope, since time.Time) (int64, error) {
	if err := db.RequireScope(s); err != nil {
		return 0, err
	}
	var n int64
	if err := r.db(ctx).QueryRow(ctx, `
SELECT coalesce(sum(tokens_in + tokens_out), 0)::bigint
  FROM investigation_steps
 WHERE org_id = $1 AND kind = 'model_turn' AND recorded_at >= $2`, s.OrgID(), since.UTC()).Scan(&n); err != nil {
		return 0, mapInvestigationErr(err, "sum the day's Investigation tokens")
	}
	return n, nil
}

// SpentOn sums one run's Steps: the input and output tokens of its model turns, and its
// Tool calls — every call Step but those to the named answer-shaping Tools and those the
// step budget refused unrun (the loop writes those as "not run: …"). A run's own
// tokens_in / tokens_out / tool_calls are written only when it ends, so an ending its
// worker never reached — `interrupted`, or abandoned by its job — is recorded with this.
func (r *InvestigationRepository) SpentOn(
	ctx context.Context, s db.TenantScope, nid uuid.UUID, answerShaping []string,
) (domain.Usage, int, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.Usage{}, 0, err
	}
	if answerShaping == nil {
		answerShaping = []string{}
	}
	var (
		u     domain.Usage
		calls int
	)
	if err := r.db(ctx).QueryRow(ctx, `
SELECT coalesce(sum(tokens_in) FILTER (WHERE kind = 'model_turn'), 0)::bigint,
       coalesce(sum(tokens_out) FILTER (WHERE kind = 'model_turn'), 0)::bigint,
       count(*) FILTER (WHERE kind = 'tool_call' AND NOT (tool_name = ANY($3))
                          AND NOT (outcome = 'refused' AND result LIKE 'not run:%'))::int
  FROM investigation_steps
 WHERE org_id = $1 AND investigation_id = $2`, s.OrgID(), nid, answerShaping).Scan(
		&u.InputTokens, &u.OutputTokens, &calls); err != nil {
		return domain.Usage{}, 0, mapInvestigationErr(err, "sum an Investigation's Steps")
	}
	return u, calls, nil
}

// LockSubjectRuns takes the advisory lock for one (Investigator, subject) and reads
// what the minimum interval decides on: the latest `queued` run, and the latest run
// that neither waits nor skipped. Inside a transaction only, for Start's reason: two
// membership changes at once must not both find nothing queued and both insert.
func (r *InvestigationRepository) LockSubjectRuns(
	ctx context.Context, s db.TenantScope, investigatorID uuid.UUID, kind domain.SubjectKind, subjectID uuid.UUID,
) (domain.SubjectRuns, error) {
	if err := db.RequireScope(s); err != nil {
		return domain.SubjectRuns{}, err
	}
	key := db.AdvisoryKey(db.LockNamespaceInvestigations,
		"subject/"+investigatorID.String()+"/"+string(kind)+"/"+subjectID.String())
	if err := db.AdvisoryXactLock(ctx, r.db(ctx), key); err != nil {
		return domain.SubjectRuns{}, errs.Internal("investigation_lock", err)
	}
	const where = ` WHERE n.org_id = $1 AND n.investigator_id = $2 AND n.subject_kind = $3 AND n.subject_id = $4`
	var out domain.SubjectRuns
	queued, err := r.oneOrNone(ctx, investigationSelect+where+` AND n.status = 'queued'
 ORDER BY n.requested_at DESC, n.id DESC LIMIT 1`, s.OrgID(), investigatorID, string(kind), subjectID)
	if err != nil {
		return domain.SubjectRuns{}, err
	}
	last, err := r.oneOrNone(ctx, investigationSelect+where+` AND n.status NOT IN ('queued','skipped')
 ORDER BY coalesce(n.started_at, n.requested_at) DESC, n.id DESC LIMIT 1`, s.OrgID(), investigatorID, string(kind), subjectID)
	if err != nil {
		return domain.SubjectRuns{}, err
	}
	out.Queued, out.Last = queued, last
	return out, nil
}

func (r *InvestigationRepository) oneOrNone(ctx context.Context, sql string, args ...any) (*domain.Investigation, error) {
	inv, err := scanInvestigation(r.db(ctx).QueryRow(ctx, sql, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // "no such run" is an answer here, not a failure
	}
	if err != nil {
		return nil, mapInvestigationErr(err, "read a subject's Investigations")
	}
	return &inv, nil
}

// DigestRun reads the run armed for one digest window — the policy and the window — or
// nil when none was (git-bug 3e96f5a). There is at most one, by
// `investigations_digest_window_uniq`, which also serves this read.
//
// ⭐ IT IS BOTH SIDES' ONE QUESTION: the arming tick asks it to arm a window once, and
// the digest tick asks it at the send and takes whatever the run is at that moment.
func (r *InvestigationRepository) DigestRun(
	ctx context.Context, s db.TenantScope, policyID uuid.UUID, window domain.DigestWindow,
) (*domain.Investigation, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	return r.oneOrNone(ctx, investigationSelect+`
 WHERE n.org_id = $1 AND n.subject_kind = 'digest' AND n.subject_id = $2
   AND n.digest_window_start = $3 AND n.digest_window_end = $4`,
		s.OrgID(), policyID, window.Start.UTC(), window.End.UTC())
}

// orgRunsKey is the advisory key Start serialises an org's starts on.
func orgRunsKey(s db.TenantScope) int64 {
	return db.AdvisoryKey(db.LockNamespaceInvestigations, "runs/"+s.OrgID().String())
}

// Finish ends a run that has not ended. A run that never started (`queued`) gets
// `started_at` now — unless it is `skipped`, which never starts by definition
// (`investigations_started_ck`). `classification` is the class the Finding was given,
// "" for none (`investigations_class_ck` refuses one without a Finding).
func (r *InvestigationRepository) Finish(
	ctx context.Context, s db.TenantScope, nid uuid.UUID, end domain.Ending, spent domain.Usage,
	toolCalls int, finding, classification string, at time.Time,
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
       tokens_in = $6, tokens_out = $7, tool_calls = $8, finding = $9, classification = $11,
       started_at = CASE WHEN $3 = 'skipped' THEN NULL ELSE coalesce(started_at, $10) END,
       ended_at = $10
 WHERE org_id = $1 AND id = $2 AND status IN ('queued','running')`,
		s.OrgID(), nid, string(end.Status), nullable(string(end.Reason)), nullable(end.Detail),
		spent.InputTokens, spent.OutputTokens, toolCalls, nullable(finding), at.UTC(), nullable(classification))
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

// MaxSteps bounds one transcript read. It is a ceiling with room, not a page size, and
// the arithmetic that keeps every transcript under it is (review A11):
//
//   - Tool calls that count are at most domain.MaxStepBudget (100) — a refused
//     answer-shaping call past its cap counts too;
//   - answer-shaping calls that do not count are at most domain.MaxFreeCallsPerRun (50);
//   - every turn but the last makes at least one call, so turns are at most 151.
//
// So a run records at most 2×(100+50)+1 = 301 Steps, plus the calls of the one turn the
// step budget stopped mid-way, which domain.MaxTurnOutputTokens bounds. 1000 is that with
// room; a run never reaches it, so no read is ever silently cut.
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

// priorFindingSelect is one ended run that reached a Finding, as a later run reads it.
const priorFindingSelect = `
SELECT n.id, n.subject_kind, n.subject_id, i.name, v.version, n.status, n.finding,
       coalesce(n.classification, ''), n.ended_at
  FROM investigations n
  JOIN investigators i         ON i.id = n.investigator_id
  JOIN investigator_versions v ON v.id = n.investigator_version_id`

// PriorFindings reads earlier runs' Findings on the same alert_key, newest first,
// served by `investigations_alert_key_idx`.
func (r *InvestigationRepository) PriorFindings(
	ctx context.Context, s db.TenantScope, alertKey string, except uuid.UUID, limit int,
) ([]domain.PriorFinding, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	return r.priorFindings(ctx, priorFindingSelect+`
 WHERE n.org_id = $1 AND n.alert_key = $2 AND n.finding IS NOT NULL AND n.id <> $3
 ORDER BY n.ended_at DESC, n.id DESC
 LIMIT $4`, s.OrgID(), alertKey, except, limit)
}

// SubjectFindings reads earlier runs' Findings on any of the named subjects of one
// kind, newest first, served by `investigations_subject_idx`: an Incident's own
// earlier Findings, and its member Cases' (ADR 0053 §3, §4; git-bug 74ea849).
func (r *InvestigationRepository) SubjectFindings(
	ctx context.Context, s db.TenantScope, kind domain.SubjectKind, subjectIDs []uuid.UUID, except uuid.UUID, limit int,
) ([]domain.PriorFinding, error) {
	if err := db.RequireScope(s); err != nil {
		return nil, err
	}
	if len(subjectIDs) == 0 {
		return []domain.PriorFinding{}, nil
	}
	return r.priorFindings(ctx, priorFindingSelect+`
 WHERE n.org_id = $1 AND n.subject_kind = $2 AND n.subject_id = ANY($3::uuid[])
   AND n.finding IS NOT NULL AND n.id <> $4
 ORDER BY n.ended_at DESC, n.id DESC
 LIMIT $5`, s.OrgID(), string(kind), subjectIDs, except, limit)
}

func (r *InvestigationRepository) priorFindings(ctx context.Context, sql string, args ...any) ([]domain.PriorFinding, error) {
	rows, err := r.db(ctx).Query(ctx, sql, args...)
	if err != nil {
		return nil, mapInvestigationErr(err, "read prior Findings")
	}
	defer rows.Close()
	out := []domain.PriorFinding{}
	for rows.Next() {
		var (
			p            domain.PriorFinding
			kind, status string
		)
		if err := rows.Scan(&p.InvestigationID, &kind, &p.SubjectID, &p.InvestigatorName, &p.VersionNumber,
			&status, &p.Finding, &p.Classification, &p.EndedAt); err != nil {
			return nil, mapInvestigationErr(err, "read prior Findings")
		}
		sk, err := domain.ParseSubjectKind(kind)
		if err != nil {
			return nil, err
		}
		st, err := domain.ParseStatus(status)
		if err != nil {
			return nil, err
		}
		p.SubjectKind, p.Status, p.EndedAt = sk, st, p.EndedAt.UTC()
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
		notBefore, started, ended *time.Time
		windowStart, windowEnd    *time.Time
	)
	if err := row.Scan(&out.ID, &out.OrgID, &kind, &out.SubjectID, &out.AlertKey,
		&out.InvestigatorID, &out.InvestigatorName, &out.VersionID, &out.VersionNumber,
		&out.Model.Endpoint, &out.Model.Model,
		&status, &reason, &det, &maxSteps, &maxTokens, &maxWall,
		&out.Spent.InputTokens, &out.Spent.OutputTokens, &out.ToolCalls,
		&out.Finding, &out.Classification, &requestedBy, &out.RequestedBy.Label,
		&out.RequestedAt, &notBefore, &started, &ended, &windowStart, &windowEnd); err != nil {
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
	if out.Classification, err = domain.RestoreClassification(out.Classification); err != nil {
		return domain.Investigation{}, err
	}
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
	if notBefore != nil {
		out.NotBefore = notBefore.UTC()
	}
	if started != nil {
		out.StartedAt = started.UTC()
	}
	if ended != nil {
		out.EndedAt = ended.UTC()
	}
	if windowStart != nil && windowEnd != nil {
		w, err := domain.NewDigestWindow(*windowStart, *windowEnd)
		if err != nil {
			return domain.Investigation{}, err
		}
		out.DigestWindow = w
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
