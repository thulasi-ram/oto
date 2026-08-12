package db

import (
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// The §L.9 SQLSTATE table, and the ONE home of it.
//
// SPEC §L.9(2) says it in one line — "Translate SQLSTATEs into `errs.Kind` in
// exactly one helper" — and CONTEXT.md §5b says the repository "is the single
// place SQLSTATEs become `errs.Kind`". Neither sentence survived contact: the
// table was spelled nine times, once per repository, each named `mapErr`, and
// four of those spellings had already drifted. Five implemented the whole table;
// two implemented three rows and let a `57014` fall through to `KindInternal`,
// which is a 500 where §L.9 says 503; one implemented no row at all; and a tenth
// copy in `platform/idempotency` had drifted a different way again. A statement
// timeout is not hypothetical in any of them — `pool.go` sets `statement_timeout`
// as a runtime parameter on EVERY connection of both pools, so a slow Postgres
// produces `57014` in every module.
//
// A 500 and a 503 are different promises. §L.1's taxonomy is explicit about which
// one overload gets: 503 is oto's own backpressure, it carries `Retry-After`, and
// it tells a caller to come back. A 500 says something is broken and pages
// somebody. Three modules turned a busy database into a defect, and lost the
// retryability with it.
//
// It lives in `platform/db` rather than `platform/errs` because the translation
// needs `pgconn.PgError`, and `errs` is imported by every `domain` package, which
// CONTEXT.md §5.2 requires to be pure Go with no pgx. `platform/db` already owns
// the pools that produce these SQLSTATEs and already imports pgx, and depguard's
// `platform-must-not-import-domains` is satisfied because nothing here names a
// domain: what is module-specific — the not-found code, the query-failed code,
// the ingest path's never-a-4xx rule — arrives through ErrorPolicy from the
// caller.
//
// The constraint NAME travels out as `errs.Error.Code` on every row that has one,
// because CONTEXT.md §6 makes constraint and index names a runtime contract:
// `alert_sources_name_uniq` is what lets an API say "that name is taken" rather
// than "conflict". On the rows where Postgres names none, the code is the one the
// CALLER supplies through ErrorPolicy.Codes — `identity_serialization_failure`,
// `ingest_check_violation` — because those strings were published, and a client
// branching on `code` does not care that oto refactored its repositories.
//
// ⚠️ THE MESSAGES ARE NOW THE TABLE'S, AND THAT IS A VISIBLE CHANGE. Nine copies
// spelled nine slightly different sentences for the same condition — "the row
// already exists", "that value is already in use", "ui event already exists" —
// and `23505` and `23503` are KindConflict, so those sentences are RENDERED on a
// 409 rather than dropped as 5xx detail. `alerts` and `grouping` are the two
// modules a caller can see this in; both now say what §L.9's row says. A message
// that differs per module is drift waiting to happen rather than a fact about the
// module, which is the whole reason this file exists — but it is a change to what
// a 409 body reads, and it is stated here rather than discovered.

// retryAfterOverload is the pacing hint on an overload row. §L.9 says `Retryable:
// true` for `40001`, `57014` and `53300` but names no delay; one second is what
// five of the nine copies already used, and `platform/httpx` supplies its own
// default when a 503 carries none.
const retryAfterOverload = time.Second

// sqlStateRow is one row of the §L.9 table.
//
// transient distinguishes a conflict oto can retry into success (a serialization
// failure) from one that describes the state of the data (a duplicate key). The
// two are the same Kind in §L.9 and different Kinds on the ingest path, which is
// the only reason the field exists.
//
// namesConstraint records the rows Postgres attaches a constraint name to. It is
// documentation of a Postgres fact — see constraintCode — and it is also how the
// ingest path tells the two shapes of 5xx apart: a constraint violation there is
// a defect in oto's own statements, and anything else is a database that could
// not answer, which C4 makes backpressure rather than a defect.
type sqlStateRow struct {
	kind            errs.Kind
	message         string
	transient       bool
	namesConstraint bool
	retryAfter      time.Duration
}

// sqlStates is SPEC §L.9's table, transcribed. Changing a row here changes it
// everywhere, which is the entire point; `TestSQLStateTableMatchesL9` in
// sqlstate_test.go pins it against the SPEC so a drift fails loudly.
var sqlStates = map[string]sqlStateRow{
	// unique_violation on a user-supplied key — a duplicate name or slug. On an
	// oto-computed key it is the idempotency mechanism working and is swallowed by
	// ON CONFLICT; one that reaches Go there is KindInternal, which is what
	// ErrorPolicy.ComputedKeys says.
	"23505": {kind: errs.KindConflict, message: "that value is already in use", namesConstraint: true},
	// foreign_key_violation — the referenced row is missing or still in use.
	"23503": {kind: errs.KindConflict, message: "the row references something that is missing or still in use", namesConstraint: true},
	// check_violation — a hole in layers 1–3 (§L.0). Never the caller's fault.
	"23514": {kind: errs.KindInternal, message: "a row violated a database constraint", namesConstraint: true},
	// not_null_violation — a mapper bug.
	"23502": {kind: errs.KindInternal, message: "a required column was null"},
	// serialization_failure / deadlock_detected — transient; the caller re-reads.
	"40001": {kind: errs.KindConflict, message: "the transaction conflicted; retry", transient: true},
	"40P01": {kind: errs.KindConflict, message: "the transaction conflicted; retry", transient: true},
	// query_canceled — the statement_timeout every pool sets. Overload, not defect.
	"57014": {kind: errs.KindUnavailable, message: "the query exceeded its time budget", transient: true, retryAfter: retryAfterOverload},
	// too_many_connections / pool acquire timeout — overload.
	"53300": {kind: errs.KindUnavailable, message: "the database is at capacity", transient: true, retryAfter: retryAfterOverload},
}

// internalMessage replaces a row's message when a policy downgrades it to a 5xx.
// "that value is already in use" describes something a caller could fix, and a
// 500 is never that. It is not rendered either way — `errs.Problem` drops the
// detail of every 5xx — but it is what an operator reads in the log line.
const internalMessage = "an internal error occurred"

// ErrorPolicy is everything a repository contributes to the §L.9 translation:
// the two codes only it can name, and — for the ingest path alone — the rule that
// no error it produces may be a 4xx.
//
// It deliberately cannot add, remove or re-map a row. A module with an opinion
// about what `23514` means is a module that has re-grown the ninth copy.
type ErrorPolicy struct {
	// NotFound is the errs code for pgx.ErrNoRows — `user_not_found`,
	// `token_not_found` — so a 404 is diagnosable without telling the caller
	// anything it could not infer from the URL it asked for.
	NotFound string
	// NotFoundMessage is the human message on that 404. "no such row" when empty.
	NotFoundMessage string
	// QueryFailed is the errs code for a database error with no §L.9 row.
	QueryFailed string
	// QueryFailedMessage is the message on that error. Never rendered (5xx), and
	// never allowed to carry a pgx type, a row struct or a SQL string (§L.9(3)).
	QueryFailedMessage string

	// Codes is the module's own error code per SQLSTATE, used ONLY where Postgres
	// names no constraint — `identity_serialization_failure` for a `40001`,
	// `ingest_check_violation` for a `23514` on an unnamed constraint. A named
	// constraint always wins, in every module, because CONTEXT.md §6 makes the
	// name the contract and §L.9 returns it as the Code.
	//
	// It exists because those strings are ALREADY A PUBLISHED CONTRACT. Five
	// modules minted their own — identity, streaming, notification, ingestion and
	// idempotency — and a client branching on `code` does not care that oto
	// collapsed ten copies of a table into one; `sqlstate_40001` where
	// `identity_serialization_failure` used to be is a broken client, not a
	// refactor. The other five modules already emitted `sqlstate_<code>` and pass
	// nothing here.
	//
	// It is a CODE map and nothing else: a policy still cannot add, remove or
	// re-map a row, so no module can regrow an opinion about what a SQLSTATE
	// MEANS — only about what it has always called it.
	Codes map[string]string

	// ComputedKeys says every unique and foreign key reachable on this path is one
	// oto computed itself, not one a caller supplied. §L.9 row 2: a `23505` on an
	// oto-computed key is swallowed by ON CONFLICT, so one that reaches Go means
	// the statement drifted from the constraint — an oto bug, never a 409 the
	// caller can act on. `platform/idempotency` and `ingestion` are the two.
	ComputedKeys bool

	// Never4xx says NO error from this path may be a 4xx, which is C4, the single
	// most consequential rule in oto: Alertmanager retries 5xx and only 5xx, and a
	// 409 makes it delete the notification permanently and silently during exactly
	// the window the customer's cluster is on fire. It implies ComputedKeys, and it
	// additionally turns a transient conflict into 503 and makes the fallthrough
	// `KindUnavailable`. `ingestion/repository` is the only holder.
	//
	// It also keeps the ingest path's 5xx split where the module's own copy had it:
	// the three CONSTRAINT VIOLATIONS are 500, because a statement that violates
	// oto's own constraint is a defect an operator must see, and every other 5xx —
	// `23502` included — is 503 with a retry hint, because the alert is on the
	// wire and the only useful answer is "come back". Alertmanager retries both,
	// but only one of them carries the pacing hint and only one of them pages.
	Never4xx bool
}

// MapError is the §L.9 translation. It is the single place in oto where a
// SQLSTATE becomes an errs.Kind.
//
// A nil error maps to a nil error, so it is safe in a one-line return.
func MapError(err error, p ErrorPolicy) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		msg := p.NotFoundMessage
		if msg == "" {
			msg = "no such row"
		}
		return errs.NotFound(p.NotFound, msg)
	}

	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		if row, ok := sqlStates[pg.Code]; ok {
			row = row.under(p)
			e := errs.Wrap(err, row.kind, constraintCode(pg, p), row.message)
			if row.transient {
				e = e.WithRetryAfter(row.retryAfter)
			}
			return e
		}
	}

	kind := errs.KindInternal
	if p.Never4xx {
		kind = errs.KindUnavailable
	}
	return errs.Wrap(err, kind, p.QueryFailed, p.QueryFailedMessage)
}

// under applies a policy's exceptions to a row. No policy weakens a row into a
// 4xx; the only movement is toward a stronger answer or, on the ingest path,
// sideways between two 5xx.
func (r sqlStateRow) under(p ErrorPolicy) sqlStateRow {
	if r.kind != errs.KindConflict {
		// ⛔ THE INGEST PATH'S 5xx SPLIT. `ingestion`'s own copy of the table spelled
		// exactly three rows — the constraint violations — and let EVERYTHING ELSE
		// fall through to `KindUnavailable`; `23502` was on the everything-else side,
		// so a mapper bug there answered 503 with a retry hint rather than 500. That
		// is the right split and not an accident of which rows got written: a
		// constraint violation is a defect in oto's own statement and belongs in a
		// pager, while a NOT NULL that arrived from a redaction the ingest path
		// applied is a batch that a fixed oto can accept on the retry Alertmanager
		// is about to make. Without this the shared table's `KindInternal` would
		// have quietly moved `23502` from 503 to 500 on the one path where the
		// difference is the SPEC's most consequential rule.
		if p.Never4xx && r.kind == errs.KindInternal && !r.namesConstraint {
			r.kind = errs.KindUnavailable
			r.transient = true
			r.retryAfter = retryAfterOverload
		}
		return r
	}
	switch {
	case r.transient && p.Never4xx:
		// A serialization failure on the ingest path is still transient, and 503 is
		// how C4 says "come back" without a 4xx.
		r.kind = errs.KindUnavailable
		r.retryAfter = retryAfterOverload
	case !r.transient && (p.ComputedKeys || p.Never4xx):
		// §L.9 row 2: a duplicate or a dangling reference on a key oto computed is
		// an oto bug, and the caller has nothing to fix.
		r.kind = errs.KindInternal
		r.message = internalMessage
		r.transient = false
	}
	return r
}

// constraintCode returns the violated constraint's name, which §L.9 makes the
// machine-readable error code. Postgres names one on `23505`, `23503` and
// `23514` — the three rows flagged `namesConstraint` — and on nothing else: a
// `23502` names a COLUMN, and a `40001` names nothing at all.
//
// §L.9 is silent about the code on those rows, so the module's own published
// name is used when it has one and `sqlstate_<code>` when it does not. Five of
// the ten copies minted names and five produced `sqlstate_<code>`; both are kept,
// per module, because both were already on the wire.
func constraintCode(pg *pgconn.PgError, p ErrorPolicy) string {
	if pg.ConstraintName != "" {
		return pg.ConstraintName
	}
	if code, ok := p.Codes[pg.Code]; ok && code != "" {
		return code
	}
	return "sqlstate_" + pg.Code
}
