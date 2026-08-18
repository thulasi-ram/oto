package scope

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/thulasiram/oto/test/harness"
)

// ---------------------------------------------------------------------------
// ADR 0041 — THE ALERT/CASE ALLOCATION, ENFORCED AGAINST THE LIVE SCHEMA.
//
// ⭐ THE RULE THIS ASSERTS IS ONE SENTENCE: is this fact true of the signal
// across ALL its firings, or only of this ONE firing? Across → `alerts`. This
// one → `alert_cases`. For a verb the same question reads "does it still mean
// something after this firing ends?" — snooze does, so it is the Alert's; an
// acknowledgement does not, so it is the Case's.
//
// ⭐ IT IS THE SIBLING OF forbidden_columns_test.go, NOT A COPY OF IT. That gate
// asks WHOSE fact a column is — a signal's or a human's — and the answer is the
// scope boundary. This one asks WHICH ROW a fact belongs on, and the answer is
// the allocation. They share the introspection helpers in that file on purpose:
// both are claims about a database that has had every migration applied, and
// neither may be satisfiable by reading the migration text.
//
// The three failures that pass `tools/lintvocab`, pass a diff review, and fail
// here are the ones that file already enumerates: DDL that never went through
// `db/migrations/`, an expand/contract migration whose contract half was written
// and never deployed, and a `-- +goose Down` that restores what the Up removed.
// A fourth is specific to this gate: an enum widened by an `ALTER TABLE … DROP
// CONSTRAINT … ADD CONSTRAINT` in a later migration, which leaves the original
// migration's text — the text a reviewer greps — perfectly correct.
//
// ⚠️ WHAT IT DOES NOT COVER, stated so nobody mistakes it for total: it reads
// column NAMES and CHECK constraint MEMBERS. A correctly named column holding
// the wrong fact — `alerts.last_seen_at` written from one episode's clock — is
// invisible here and belongs to a behavioural test. This gate stops the
// allocation errors that arrive as SCHEMA, because those are the ones that
// acquire rows before anybody notices.
// ---------------------------------------------------------------------------

// ackStem matches the acknowledgement family. `(un)?ack` with a word boundary on
// the left catches `ack_state`, `acked_by`, `acked_at`, `acked_by_label`,
// `ack_note` and `unacked_*`, which is every spelling migration 00049 removed
// from `alerts`.
//
// ⛔ It is deliberately NOT the bare substring `ack`, which would match
// `feedback_url`, `slack_channel_id` and `rollback_at` — all legitimate names on
// other tables, and a gate that cries wolf is a gate somebody deletes.
var ackStem = regexp.MustCompile(`(^|_)(un)?ack`)

// snoozeStem matches the snooze family. A snooze is a ROW in `alert_snoozes`
// with its own start, end, actor and note; a column anywhere here would be a
// second and lossier spelling of it.
var snoozeStem = regexp.MustCompile(`snooz`)

// forbiddenStem is one allocation claim: this family of columns may not appear
// on this table, and here is the sentence that says why.
type forbiddenStem struct {
	table  string
	stem   *regexp.Regexp
	family string
	why    string
}

// allocationRules are ADR 0041 §6 claims 1 and 2.
//
// ⭐ THERE IS NO `ack` RULE FOR `alert_cases`, AND ITS ABSENCE IS THE POINT. The
// receipt belongs there; `acked_by` is the column the scope boundary explicitly
// permits (forbidden_columns_test.go says so too). A gate that forbade the
// family everywhere would be asserting "oto does not do acknowledgement", which
// is a different and false claim.
var allocationRules = []forbiddenStem{
	{
		table:  "alerts",
		stem:   ackStem,
		family: "acknowledgement",
		why: "an acknowledgement is a receipt for ONE firing (ADR 0041 §1, ADR 0040 §6, " +
			"migration 00049). On the identity it silently survives a re-fire, so an alert " +
			"nobody has looked at reads as seen. It belongs on `alert_cases`.",
	},
	{
		table:  "alerts",
		stem:   snoozeStem,
		family: "snooze",
		why: "a snooze is a row in `alert_snoozes` with its own start, end, actor and note " +
			"(SPEC §B.8). A column here would be a second, lossy spelling of a lifecycle " +
			"that already has a home.",
	},
	{
		table:  "alert_cases",
		stem:   snoozeStem,
		family: "snooze",
		why: "snooze is oto being quiet about a SIGNAL until a fixed time; it outlives the " +
			"firing that provoked it (ADR 0041 §1). On an episode it would also have to be " +
			"copied forward to the next one, which is the allocation error in its purest form.",
	},
}

// stateEnum is ADR 0041 §6 claims 3 and 4: a `state` CHECK whose members are
// exactly these, with these ones named as the forbidden leak.
type stateEnum struct {
	table      string
	constraint string
	want       []string
	forbidden  map[string]string
}

var stateEnums = []stateEnum{
	{
		table:      "alerts",
		constraint: "alerts_state_ck",
		want:       []string{"expired", "firing", "resolved"},
		forbidden: map[string]string{
			"suppressed": "suppression is an ORTHOGONAL AXIS (ADR 0041 §4): `suppression_reason` " +
				"and `suppressed_by` say whether Alertmanager is delivering the signal, and the " +
				"signal goes on firing underneath. As a state value it occupies the slot `firing` " +
				"needs, so every `state = 'firing'` reader under-counts by exactly the alerts " +
				"somebody has silenced.",
			"snoozed": "snooze is oto's own quiet button and is never a state — " +
				"`internal/alerts/domain/snooze.go:25-32` has refused this since 00017. An alert " +
				"can be firing AND acked AND snoozed at once, and all three are displayed.",
		},
	},
	{
		table:      "alert_cases",
		constraint: "case_state_ck",
		want:       []string{"closed", "open"},
		forbidden: map[string]string{
			"firing": "an episode's own state is whether it is RUNNING (ADR 0040). `firing` was " +
				"just \"not ended\", which `ended_at` and `case_terminal_ended` already said.",
			"suppressed": "a silence mutes a LABEL SET, not one firing of it (ADR 0040 §2). The " +
				"episode's record of having been muted is `suppression_reason`, which is a " +
				"column beside `state`, not a value inside it (ADR 0041 §5).",
			"snoozed": "snooze is neither a state nor an episode's fact (ADR 0041 §1).",
		},
	},
}

// enumMember pulls the quoted members out of a rendered CHECK constraint.
// Postgres renders `state IN ('a','b')` as `state = ANY (ARRAY['a'::text,
// 'b'::text])`, so the members are exactly the `'…'::text` literals.
var enumMember = regexp.MustCompile(`'([a-z_]+)'::text`)

// constraintDef asks the catalogue for the RENDERED definition of one named
// constraint. Reading `pg_get_constraintdef` rather than the migration file is
// the whole point: a later migration that dropped and re-added the constraint
// leaves the original file's text correct and the database changed.
func constraintDef(ctx context.Context, q querier, table, name string) (string, bool, error) {
	rows, err := q.Query(ctx, `
		SELECT pg_get_constraintdef(c.oid)
		  FROM pg_constraint c
		 WHERE c.conrelid = $1::regclass
		   AND c.conname  = $2`, table, name)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()

	if !rows.Next() {
		return "", false, rows.Err()
	}
	var def string
	if err := rows.Scan(&def); err != nil {
		return "", false, err
	}
	return def, true, rows.Err()
}

// enumMembers is the predicate, separate from the query so the planted-violation
// test can feed it a constraint it rewrote inside a transaction.
func enumMembers(def string) []string {
	seen := map[string]struct{}{}
	for _, m := range enumMember.FindAllStringSubmatch(def, -1) {
		seen[m[1]] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// misallocatedColumns applies one rule to an introspected schema.
func misallocatedColumns(cols []column, rule forbiddenStem) []column {
	var bad []column
	for _, c := range cols {
		if c.table == rule.table && rule.stem.MatchString(c.name) {
			bad = append(bad, c)
		}
	}
	return bad
}

// allocationFailure is the message a misallocated column earns. It names the
// rule, the sentence, and the table the column should have gone to — because a
// gate that only says "no" gets the column renamed until it stops saying it.
func allocationFailure(rule forbiddenStem, bad []column) string {
	names := make([]string, 0, len(bad))
	for _, c := range bad {
		names = append(names, c.String())
	}
	sort.Strings(names)

	other := "alert_cases"
	if rule.table == "alert_cases" {
		other = "alerts"
	}
	return fmt.Sprintf(
		"the live schema carries %d %s column(s) on %q: %s\n\n"+
			"ADR 0041 — the rule that decides every column on these two tables:\n\n"+
			"    Is this fact true of the signal across ALL its firings, or only of this ONE firing?\n"+
			"    Across -> alerts.  This one -> alert_cases.\n"+
			"    For a verb: does it still mean something after this firing ends?\n\n"+
			"%s\n\n"+
			"If the fact genuinely belongs here, that is an amendment to ADR 0041 — not a column. "+
			"If it belongs on %s, move it there. If it has its own lifecycle, it wants a table.",
		len(bad), rule.family, rule.table, strings.Join(names, ", "), rule.why, other)
}

// enumFailure is the message a leaked enum member earns.
func enumFailure(e stateEnum, def string, got []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s admits %v, want exactly %v.\n\nrendered definition:\n    %s\n",
		e.constraint, got, e.want, def)

	for _, m := range got {
		if why, forbidden := e.forbidden[m]; forbidden {
			fmt.Fprintf(&b, "\n%q is forbidden by ADR 0041: %s\n", m, why)
		}
	}
	fmt.Fprintf(&b, "\nA state column holds ONE axis. Anything that can be true AT THE SAME TIME "+
		"as one of %v is a different axis and needs its own column.\n", e.want)
	return b.String()
}

// TestAllocationRulesHoldInTheLiveSchema is ADR 0041 §6 claims 1 and 2.
func TestAllocationRulesHoldInTheLiveSchema(t *testing.T) {
	h := harness.New(t)

	cols, err := introspect(h.Ctx, h.Pool, scopedTables)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	// The same anti-vacuity guard the sibling gate uses: an empty result is not a
	// pass, it is proof the gate examined nothing.
	assertEveryScopedTableAnswered(t, cols)

	for _, rule := range allocationRules {
		if bad := misallocatedColumns(cols, rule); len(bad) > 0 {
			t.Error(allocationFailure(rule, bad))
		}
	}
}

// TestStateColumnsHoldOneAxisEach is ADR 0041 §6 claims 3 and 4.
func TestStateColumnsHoldOneAxisEach(t *testing.T) {
	h := harness.New(t)

	for _, e := range stateEnums {
		def, found, err := constraintDef(h.Ctx, h.Pool, e.table, e.constraint)
		if err != nil {
			t.Fatalf("read %s: %v", e.constraint, err)
		}
		// ⛔ ANTI-VACUITY. A constraint that is not there produces no members and
		// no complaint, which would make this gate green on a schema that had
		// dropped the CHECK entirely — the one change it most needs to catch.
		if !found {
			t.Fatalf("%s does not exist on %q. ADR 0041 §6 is asserted by asking the "+
				"database what the constraint SAYS, so a missing constraint means the enum "+
				"is unbounded, not that it is correct.", e.constraint, e.table)
		}
		if got := enumMembers(def); strings.Join(got, ",") != strings.Join(e.want, ",") {
			t.Error(enumFailure(e, def, got))
		}
	}
}

// TestAllocationGateFires plants every violation and checks the real
// introspection reports exactly them.
//
// ⭐ IT IS THE ONLY PROOF THE GATE HAS TEETH. No migration adds an ack column to
// `alerts` or puts `suppressed` back in either enum, so both gates above pass
// whether their queries are correct, match the wrong catalogue, or return
// nothing at all.
//
// The plant is DDL inside a transaction that is rolled back. DDL is transactional
// in Postgres and both `information_schema` and `pg_constraint` are ordinary
// catalogue reads, so the uncommitted schema is visible to the SAME queries the
// gates run — and invisible to every other session, and gone at rollback.
func TestAllocationGateFires(t *testing.T) {
	h := harness.New(t)

	tx, err := h.Pool.Begin(h.Ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(h.Ctx) }()

	// THE COLUMN VIOLATIONS, one per rule and spelled the way each would really
	// arrive: `acked_at` is the column 00049 removed from `alerts`; `snoozed_until`
	// is the shortcut somebody takes to avoid a join to `alert_snoozes`; and the
	// same shortcut copied onto the episode.
	for _, ddl := range []string{
		`ALTER TABLE alerts      ADD COLUMN acked_at      TIMESTAMPTZ`,
		`ALTER TABLE alerts      ADD COLUMN snoozed_until TIMESTAMPTZ`,
		`ALTER TABLE alert_cases ADD COLUMN snoozed_until TIMESTAMPTZ`,
	} {
		if _, err := tx.Exec(h.Ctx, ddl); err != nil {
			t.Fatalf("planting %q: %v", ddl, err)
		}
	}
	// NOT violations, and they are here so the gate cannot pass by flagging every
	// column it has not seen before: `acked_by` is the receipt on the episode
	// where it belongs, and `feedback_url` proves `ackStem` is a stem and not the
	// substring `ack`.
	for _, ddl := range []string{
		`ALTER TABLE alert_cases ADD COLUMN acked_by_deputy TEXT`,
		`ALTER TABLE alerts      ADD COLUMN feedback_url    TEXT`,
	} {
		if _, err := tx.Exec(h.Ctx, ddl); err != nil {
			t.Fatalf("planting the control column %q: %v", ddl, err)
		}
	}

	cols, err := introspect(h.Ctx, tx, scopedTables)
	if err != nil {
		t.Fatalf("introspect in transaction: %v", err)
	}
	assertEveryScopedTableAnswered(t, cols)

	var got []string
	for _, rule := range allocationRules {
		for _, c := range misallocatedColumns(cols, rule) {
			got = append(got, rule.family+":"+c.String())
		}
	}
	sort.Strings(got)
	want := []string{
		"acknowledgement:alerts.acked_at",
		"snooze:alert_cases.snoozed_until",
		"snooze:alerts.snoozed_until",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("planted exactly %v, gate reported %v", want, got)
	}

	// THE ENUM VIOLATIONS. Each state CHECK is dropped and re-added one member
	// wider, which is exactly how an enum grows in practice — inside a later
	// migration, leaving the original migration's text untouched and correct.
	for _, ddl := range []string{
		`ALTER TABLE alerts DROP CONSTRAINT alerts_state_ck`,
		`ALTER TABLE alerts ADD CONSTRAINT alerts_state_ck
		   CHECK (state IN ('firing','suppressed','resolved','expired'))`,
		`ALTER TABLE alert_cases DROP CONSTRAINT case_state_ck`,
		`ALTER TABLE alert_cases ADD CONSTRAINT case_state_ck
		   CHECK (state IN ('open','closed','suppressed'))`,
	} {
		if _, err := tx.Exec(h.Ctx, ddl); err != nil {
			t.Fatalf("planting %q: %v", ddl, err)
		}
	}

	for _, e := range stateEnums {
		def, found, err := constraintDef(h.Ctx, tx, e.table, e.constraint)
		if err != nil {
			t.Fatalf("read %s in transaction: %v", e.constraint, err)
		}
		if !found {
			t.Fatalf("%s vanished after being re-added", e.constraint)
		}
		members := enumMembers(def)
		if strings.Join(members, ",") == strings.Join(e.want, ",") {
			t.Fatalf("%s still reads %v after `suppressed` was planted in it; the gate is "+
				"reading something other than the live constraint", e.constraint, members)
		}
		msg := enumFailure(e, def, members)
		if !strings.Contains(msg, "suppressed") || !strings.Contains(msg, "ADR 0041") {
			t.Errorf("the failure message for %s does not explain the leak:\n%s", e.constraint, msg)
		}
	}

	// And the rollback has to be real: both gates must be reading the database
	// rather than remembering what they were told.
	if err := tx.Rollback(h.Ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	after, err := introspect(h.Ctx, h.Pool, scopedTables)
	if err != nil {
		t.Fatalf("introspect after rollback: %v", err)
	}
	for _, rule := range allocationRules {
		if bad := misallocatedColumns(after, rule); len(bad) > 0 {
			t.Fatalf("the planted columns outlived their transaction: %v", bad)
		}
	}
}
