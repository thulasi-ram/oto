package db

import (
	"errors"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// This file pins the §L.9 SQLSTATE table itself — not one repository's use of it.
//
// It exists because the table was copied nine times and four of the copies had
// drifted, and a test that lived beside one copy could only ever have caught that
// copy. What follows asserts the whole chain a caller actually experiences —
// SQLSTATE → errs.Kind → HTTP status, plus the retry guidance and the machine
// code — against SPEC §L.9 transcribed by hand below. If a row here and a row in
// the SPEC disagree, one of them is wrong and the disagreement is the finding.
//
// It is an in-package test on purpose: `TestTheTableHasExactlyTheL9Rows` has to
// see `sqlStates` to prove no row was added that the SPEC does not sanction, and
// a tenth mapping cannot hide behind an exported surface that still looks right.

// l9Row is one row of the §L.9 table as the SPEC writes it, plus the HTTP status
// §L.1 gives that Kind. The status is spelled out rather than derived because the
// status is the promise the caller receives: 503 says "come back", 500 says
// "something is broken". Deriving it would only re-assert errs.HTTPStatus.
type l9Row struct {
	sqlstate string
	kind     errs.Kind
	status   int
	retry    bool
	after    time.Duration
}

// theL9Table is SPEC §L.9(2), row for row, in the SPEC's order.
//
//	| 23505 unique_violation on a user-supplied key | KindConflict + constraint name as Code |
//	| 23505 on an oto-computed key                  | KindInternal (see the policy tests)    |
//	| 23503 foreign_key_violation                   | KindConflict, Code = constraint name   |
//	| 23514 check_violation                         | KindInternal, Code = constraint name   |
//	| 23502 not_null_violation                      | KindInternal                           |
//	| 40001 / 40P01                                 | KindConflict, Retryable: true          |
//	| 57014 query_canceled (statement_timeout)      | KindUnavailable, Retryable: true       |
//	| 53300 too_many_connections                    | KindUnavailable, Retryable: true       |
//
// §L.9 says "Retryable: true" for the last three and names no delay; the delays
// asserted here are `retryAfterOverload`, which is what the majority of the nine
// copies already used and what `platform/httpx` would otherwise default to.
var theL9Table = []l9Row{
	{sqlstate: "23505", kind: errs.KindConflict, status: http.StatusConflict},
	{sqlstate: "23503", kind: errs.KindConflict, status: http.StatusConflict},
	{sqlstate: "23514", kind: errs.KindInternal, status: http.StatusInternalServerError},
	{sqlstate: "23502", kind: errs.KindInternal, status: http.StatusInternalServerError},
	{sqlstate: "40001", kind: errs.KindConflict, status: http.StatusConflict, retry: true, after: 0},
	{sqlstate: "40P01", kind: errs.KindConflict, status: http.StatusConflict, retry: true, after: 0},
	{sqlstate: "57014", kind: errs.KindUnavailable, status: http.StatusServiceUnavailable, retry: true, after: time.Second},
	{sqlstate: "53300", kind: errs.KindUnavailable, status: http.StatusServiceUnavailable, retry: true, after: time.Second},
}

// generalPolicy is what every repository but ingestion and idempotency passes.
var generalPolicy = ErrorPolicy{
	NotFound:           "widget_not_found",
	NotFoundMessage:    "no such widget",
	QueryFailed:        "widget_query_failed",
	QueryFailedMessage: "could not read the widget",
}

// pgErr builds the error pgx hands a repository for a given SQLSTATE.
func pgErr(sqlstate, constraint string) error {
	return &pgconn.PgError{Code: sqlstate, ConstraintName: constraint, Message: "server-side detail"}
}

// ⭐ TestTheL9TableIsTranslatedExactlyOnce walks every row of §L.9 through
// MapError and asserts the Kind, the HTTP status the caller sees, the machine
// code and the retry guidance.
func TestTheL9TableIsTranslatedExactlyOnce(t *testing.T) {
	for _, row := range theL9Table {
		t.Run(row.sqlstate, func(t *testing.T) {
			err := MapError(pgErr(row.sqlstate, "widgets_name_uniq"), generalPolicy)

			if got := errs.KindOf(err); got != row.kind {
				t.Errorf("SQLSTATE %s maps to kind %q, but §L.9 says %q", row.sqlstate, got, row.kind)
			}
			if got := errs.HTTPStatus(errs.KindOf(err)); got != row.status {
				t.Errorf("SQLSTATE %s answers HTTP %d, but §L.9 + §L.1 say %d", row.sqlstate, got, row.status)
			}
			// CONTEXT.md §6: constraint names are a RUNTIME CONTRACT. Losing the name
			// is how an API ends up saying "conflict" where it could say "that name is
			// taken", and two of the nine copies had already lost it.
			if got := errs.CodeOf(err); got != "widgets_name_uniq" {
				t.Errorf("SQLSTATE %s returns code %q; the violated constraint's name is the contract", row.sqlstate, got)
			}
			after, retryable := errs.RetryAfterOf(err)
			if retryable != row.retry {
				t.Errorf("SQLSTATE %s is retryable=%v, want %v", row.sqlstate, retryable, row.retry)
			}
			if retryable && after != row.after {
				t.Errorf("SQLSTATE %s advertises Retry-After %v, want %v", row.sqlstate, after, row.after)
			}
		})
	}
}

// ⭐ TestAStatementTimeoutIs503AndNotA500 is the row that cost this ticket.
//
// `platform/db.open` writes `statement_timeout` as a runtime parameter on EVERY
// connection of BOTH pools, so `57014` is reachable from every module, not just
// the ingest path. Three of the nine copies — notification, streaming, drill —
// had no row for it and let it fall through to KindInternal. §L.1 is explicit
// about which promise overload gets: 503 is oto's own backpressure and carries a
// Retry-After; 500 says something is broken and pages somebody.
//
// It holds under every policy, because no policy has a reason to call a busy
// database a defect.
func TestAStatementTimeoutIs503AndNotA500(t *testing.T) {
	for name, p := range map[string]ErrorPolicy{
		"general":      generalPolicy,
		"computedKeys": {NotFound: "x", QueryFailed: "y", ComputedKeys: true},
		"never4xx":     {NotFound: "x", QueryFailed: "y", Never4xx: true},
	} {
		t.Run(name, func(t *testing.T) {
			err := MapError(pgErr("57014", ""), p)
			if !errs.IsKind(err, errs.KindUnavailable) {
				t.Fatalf("a statement timeout is %q; §L.9 says KindUnavailable", errs.KindOf(err))
			}
			if got := errs.HTTPStatus(errs.KindOf(err)); got != http.StatusServiceUnavailable {
				t.Fatalf("a statement timeout answers HTTP %d, want 503", got)
			}
			after, retryable := errs.RetryAfterOf(err)
			if !retryable || after <= 0 {
				t.Fatalf("a statement timeout carries retryable=%v after=%v; a 503 without retry "+
					"guidance tells a client nothing it can act on", retryable, after)
			}
		})
	}
}

// ⛔ TestTheIngestPathIsNeverA4xx is C4, the single most consequential rule in
// oto. Alertmanager retries 5xx and only 5xx; a 4xx — 409 included — makes it
// delete the notification permanently and silently.
func TestTheIngestPathIsNeverA4xx(t *testing.T) {
	ingest := ErrorPolicy{
		NotFound:           "ingest_not_found",
		NotFoundMessage:    "no such ingest row",
		QueryFailed:        "ingest_query_failed",
		QueryFailedMessage: "could not read the batch",
		Never4xx:           true,
	}

	// ⛔ The two 5xx are not interchangeable here. A CONSTRAINT VIOLATION is a
	// defect in oto's own statement and stays 500 so it pages; everything else is
	// a database that could not answer for a batch already on disk, and 503 with a
	// retry hint is the answer this path owes Alertmanager. That is the split
	// `ingestion`'s own copy of the table had — it spelled the three constraint
	// rows and let all else fall through to KindUnavailable — and `23502` is the
	// row that makes the difference visible.
	want := map[string]errs.Kind{
		"23505": errs.KindInternal,    // §L.9 row 2: the key is oto's, not the caller's
		"23503": errs.KindInternal,    // a dangling reference on this path is an oto bug
		"23514": errs.KindInternal,    // a CHECK oto wrote and oto broke
		"23502": errs.KindUnavailable, // a mapper bug the retry may well survive
		"40001": errs.KindUnavailable, // still transient, but 503 rather than 409
		"40P01": errs.KindUnavailable, //
		"57014": errs.KindUnavailable, // as everywhere
		"53300": errs.KindUnavailable, // as everywhere
	}
	for sqlstate, kind := range want {
		err := MapError(pgErr(sqlstate, "ingest_batches_pkey"), ingest)
		if got := errs.KindOf(err); got != kind {
			t.Errorf("on the ingest path SQLSTATE %s maps to %q, want %q", sqlstate, got, kind)
		}
		if status := errs.HTTPStatus(errs.KindOf(err)); status >= 400 && status < 500 {
			t.Errorf("SQLSTATE %s answers HTTP %d on the ingest path; C4 forbids every 4xx, "+
				"because Alertmanager deletes an alert it saw one for", sqlstate, status)
		}
	}

	// Anything with no §L.9 row is transient by assumption here, not a defect.
	if !errs.IsKind(MapError(errors.New("connection reset"), ingest), errs.KindUnavailable) {
		t.Error("an unclassified failure on the ingest path must be 503, not 500")
	}
}

// TestAComputedKeyCollisionIsInternal pins §L.9's second row: a 23505 on a key
// oto computed is the idempotency mechanism, swallowed by ON CONFLICT, so one
// reaching Go is an oto bug rather than a 409 the caller can act on.
func TestAComputedKeyCollisionIsInternal(t *testing.T) {
	p := ErrorPolicy{NotFound: "x", QueryFailed: "y", ComputedKeys: true}

	for _, sqlstate := range []string{"23505", "23503"} {
		if !errs.IsKind(MapError(pgErr(sqlstate, "idempotency_claims_pkey"), p), errs.KindInternal) {
			t.Errorf("SQLSTATE %s on an oto-computed key must be internal (§L.9 row 2)", sqlstate)
		}
	}
	// A serialization failure is NOT about a key, so ComputedKeys does not touch
	// it: the caller re-reads and retries, which is a 409 exactly as §L.9 says.
	if !errs.IsKind(MapError(pgErr("40001", ""), p), errs.KindConflict) {
		t.Error("ComputedKeys must not move 40001: it is transient, not a key collision")
	}
}

// TestTheTableHasExactlyTheL9Rows is the half of this file that stops a tenth
// copy from reappearing as an EXTRA ROW rather than a second function. A row that
// is not in the SPEC is a mapping nobody agreed to; a row in the SPEC that is not
// here is a SQLSTATE falling through to 500.
func TestTheTableHasExactlyTheL9Rows(t *testing.T) {
	want := make([]string, 0, len(theL9Table))
	for _, row := range theL9Table {
		want = append(want, row.sqlstate)
	}
	got := make([]string, 0, len(sqlStates))
	for code := range sqlStates {
		got = append(got, code)
	}
	sort.Strings(want)
	sort.Strings(got)

	if len(got) != len(want) {
		t.Fatalf("the table has %d rows, §L.9 has %d: %v vs %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the table maps %v; §L.9 maps %v", got, want)
		}
	}
}

// TestWhatEachRepositoryStillOwns pins the three things a policy may say — and,
// by omission, everything it may not.
func TestWhatEachRepositoryStillOwns(t *testing.T) {
	// The not-found code, so a 404 names what was missing.
	err := MapError(pgx.ErrNoRows, generalPolicy)
	if !errs.IsKind(err, errs.KindNotFound) || errs.CodeOf(err) != "widget_not_found" {
		t.Errorf("pgx.ErrNoRows mapped to %q/%q, want not_found/widget_not_found",
			errs.KindOf(err), errs.CodeOf(err))
	}

	// The query-failed code, for a database error §L.9 has no row for.
	err = MapError(errors.New("dial tcp: connection refused"), generalPolicy)
	if !errs.IsKind(err, errs.KindInternal) || errs.CodeOf(err) != "widget_query_failed" {
		t.Errorf("an unclassified failure mapped to %q/%q, want internal_error/widget_query_failed",
			errs.KindOf(err), errs.CodeOf(err))
	}

	// A SQLSTATE outside §L.9 is not a row: it takes the fallthrough.
	err = MapError(pgErr("42P01", ""), generalPolicy) // undefined_table
	if !errs.IsKind(err, errs.KindInternal) || errs.CodeOf(err) != "widget_query_failed" {
		t.Errorf("an unlisted SQLSTATE mapped to %q/%q; only §L.9's rows are translated",
			errs.KindOf(err), errs.CodeOf(err))
	}

	// §L.9 is silent about the Code when Postgres names no constraint — which is
	// every 23502, since it names a column instead. `sqlstate_<code>` is what the
	// majority of the nine copies produced and what is kept.
	if got := errs.CodeOf(MapError(pgErr("23502", ""), generalPolicy)); got != "sqlstate_23502" {
		t.Errorf("an unnamed constraint yields code %q, want sqlstate_23502", got)
	}

	// A nil error is not an error. Two of the nine copies would have returned a
	// non-nil `error` interface wrapping a nil *errs.Error here.
	if got := MapError(nil, generalPolicy); got != nil {
		t.Errorf("MapError(nil) = %v, want nil", got)
	}
}

// TestNo5xxLeaksAServerSideDetail is §L.9(3): a message must be safe to render.
// The pgx message never becomes the errs message; it survives only as the cause,
// which `errs.Problem` drops on every 5xx and which only a log line sees.
func TestNo5xxLeaksAServerSideDetail(t *testing.T) {
	for _, row := range theL9Table {
		err := MapError(pgErr(row.sqlstate, "widgets_name_uniq"), generalPolicy)
		e, ok := errs.As(err)
		if !ok {
			t.Fatalf("SQLSTATE %s did not produce an *errs.Error", row.sqlstate)
		}
		if e.Message == "server-side detail" || e.Message == "" {
			t.Errorf("SQLSTATE %s renders %q; §L.9(3) requires a message safe to show", row.sqlstate, e.Message)
		}
		var pg *pgconn.PgError
		if !errors.As(err, &pg) {
			t.Errorf("SQLSTATE %s dropped its cause; the operator reads it from the log", row.sqlstate)
		}
	}
}
