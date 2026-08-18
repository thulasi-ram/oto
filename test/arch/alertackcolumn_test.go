package arch

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ⭐⭐ WHY THIS FILE EXISTS.
//
// `alerts.ack_state` projected the current case's acknowledgement onto an
// entity that OUTLIVES the firing it referred to. An ack is a statement about ONE
// episode — `case_ackorder_ck CHECK (acked_at IS NULL OR acked_at >= started_at)`
// says so in the schema — and the projection was the only place that contradicted
// it:
//
//	10:00  fires    -> case 7 opens, a human acks -> ack_state = 'acked'
//	10:05  resolves -> case 7 closes
//	10:05..14:00    alerts.state     = 'resolved'   honest, that IS its state
//	                alerts.ack_state = 'acked'      a claim about a closed episode
//
// That window is the mechanism by which a September firing arrives
// pre-acknowledged because somebody acked in March: it never enters anyone's
// queue and nobody looks at it. The list of unacknowledged things is the surface
// people work from, and its trustworthiness is the whole value of ack.
//
// ⛔ SO THE COLUMN IS GONE, AND THIS REFUSES ITS RETURN. The pressure to put it
// back is real and arrives dressed as performance every time — "the list has to
// join to show ack", "one denormalised letter costs nothing". It cost the
// product's most-used filter its truthfulness, and no index on `alerts` ever
// covered it, so the filter it existed to serve was a heap scan that got worse as
// the table grew.
//
// ⚠️ WHAT THIS IS AND IS NOT. It is a STATIC read of `db/migrations`: it replays
// the Up half of every migration in order and asks what columns `alerts` has at
// the end, so it catches the migration nobody noticed, and it needs no database.
// It does NOT see a column added outside `db/migrations` — a hotfix `ALTER
// TABLE`, an un-deployed contract half, a rolled-back `Down`. That is
// `test/scope/forbidden_columns_test.go`'s job (it asks the live schema), and
// `test/integration/slack_acknowledge_test.go` asserts the same absence against a
// real database on the ack path itself. Three gates, three failure modes, said
// here so nobody mistakes any one of them for total.
//
// The replay itself — `replayColumns` — lives in `snoozecolumn_test.go`, which
// needed the identical fold for the identical reason one axis over.

// ackColumn is what an acknowledgement column looks like however it is spelled.
//
// ⭐ IT IS A STEM, NOT THE NAME. The way this column comes back is not as
// itself: it comes back as `acked`, `last_acked_at`, `ack_by`, `is_acknowledged`
// — a smaller, more reasonable-looking version of exactly the same untrue claim.
// Matching `ack` is what makes the rename stop being an escape route.
var ackColumn = regexp.MustCompile(`ack`)

// TestAlertsHasNoAckColumn is the gate.
func TestAlertsHasNoAckColumn(t *testing.T) {
	cols := replayColumns(t, "alerts")

	// ⛔ AN EMPTY ANSWER IS NOT A PASS. Misspell the table, rename it, break the
	// `CREATE TABLE` body scan, and the predicate below reports nothing — green
	// forever, over a schema it never read.
	if len(cols) < 10 {
		t.Fatalf("the migration replay found %d column(s) on `alerts`, which is not a table "+
			"this schema has: %v\n\nThe gate examined nothing. Either `alerts` was renamed "+
			"and this file was not updated with it, or the replay has stopped reading "+
			"CREATE TABLE bodies.", len(cols), sortedColumns(cols))
	}
	for _, want := range []string{"id", "org_id", "state", "current_case_id"} {
		if !cols[want] {
			t.Fatalf("the replay lost `alerts.%s`, so it is not reading the real schema", want)
		}
	}

	for _, c := range sortedColumns(cols) {
		if ackColumn.MatchString(c) {
			t.Error(ackColumnFailure("alerts", c))
		}
	}
}

// TestAckStaysOnTheCase is the other half, and it is not decoration: a
// green "no ack on `alerts`" is also what you get by deleting acknowledgement
// from the product entirely. The receipt has to still be somewhere, and there is
// exactly one somewhere it can be.
//
// ⭐ IT IS ALSO THE PROOF THE GATE HAS TEETH. `db/migrations` is clean, so
// `TestAlertsHasNoAckColumn` passes whether the predicate works, is inverted, or
// matches nothing at all. Running the SAME predicate over the table where ack
// legitimately lives must flag all five of its columns — against the real
// migration text, through the real replay.
func TestAckStaysOnTheCase(t *testing.T) {
	cols := replayColumns(t, "alert_cases")

	want := []string{"ack_note", "ack_state", "acked_at", "acked_by", "acked_by_label"}
	for _, w := range want {
		if !cols[w] {
			t.Errorf("`alert_cases` has lost `%s`.\n\nAck did not move off the Alert to "+
				"nowhere — it moved to the EPISODE, the only subject about which an "+
				"acknowledgement stays true. Losing it there is the same defect as keeping it "+
				"on `alerts`, arrived at from the other side.", w)
		}
	}

	var flagged []string
	for _, c := range sortedColumns(cols) {
		if ackColumn.MatchString(c) {
			flagged = append(flagged, c)
		}
	}
	if strings.Join(flagged, ",") != strings.Join(want, ",") {
		t.Errorf("the ack predicate flagged %v on `alert_cases`, want exactly %v\n\n"+
			"This is the gate's own teeth check: if the predicate cannot recognise the five "+
			"columns that ARE an acknowledgement, its silence about `alerts` means nothing.",
			flagged, want)
	}
}

// TestAckColumnFailureSaysWhy. A gate that fires without an argument gets the
// column renamed until it stops firing, which is how `ack_state` would come back
// as `acked` inside a week.
func TestAckColumnFailureSaysWhy(t *testing.T) {
	msg := ackColumnFailure("alerts", "last_acked_at")
	for _, want := range []string{
		"alerts.last_acked_at",
		"alert_cases",
		"ONE FIRING EPISODE",
		"case_ack_idx",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the failure message does not mention %q:\n%s", want, msg)
		}
	}
}

func ackColumnFailure(table, column string) string {
	return fmt.Sprintf(
		"the schema carries an acknowledgement column on `%[1]s`: %[1]s.%[2]s\n\n"+
			"⛔ AN ACK IS A STATEMENT ABOUT ONE FIRING EPISODE. `%[1]s` is the identity of a "+
			"label set and outlives every firing it has, so a receipt stored here keeps "+
			"asserting itself after its subject has ended — and a firing months later arrives "+
			"pre-acknowledged, never reaching anybody.\n\n"+
			"`case_ackorder_ck CHECK (acked_at IS NULL OR acked_at >= started_at)` already says "+
			"an ack cannot exist without an episode. Put it on `alert_cases`, where "+
			"`case_ack_idx (org_id, ack_state, started_at DESC) WHERE ended_at IS NULL` is the "+
			"index that answers ack questions — no index on `%[1]s` ever covered one, which is "+
			"why the `?ack=` filter this column served was a heap scan.\n\n"+
			"`state` is projected onto `%[1]s` and this is not, and the difference is not "+
			"taste: `state` still has an honest answer when nothing is firing. Ack has none.",
		table, column)
}

func sortedColumns(cols map[string]bool) []string {
	out := make([]string, 0, len(cols))
	for c := range cols {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
