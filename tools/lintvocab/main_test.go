package main

import (
	"os"
	"path/filepath"
	"testing"
)

// scan writes src to a file named name in a fresh directory and runs the real
// per-file scan over it — the same strip, Down-section and marker handling the
// gate runs over the repository — returning the rule names that FAILED.
func scan(t *testing.T, name, src string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	bad, _, err := scanFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range bad {
		out = append(out, f.rule)
	}
	return out
}

func fired(rules []string, name string) bool {
	for _, r := range rules {
		if r == name {
			return true
		}
	}
	return false
}

// TestIncidentIDIsBannedOnASignalRowAndNowhereElse is ADR 0052's lint choice,
// pinned in both directions. The two halves are the whole point: a rule that
// only fails the bad migration is the old blanket ban, which refused the
// Incident membership table before it had a row; a rule that only passes the
// good one has stopped guarding SCOPE-BOUNDARY §5.6's door.
func TestIncidentIDIsBannedOnASignalRowAndNowhereElse(t *testing.T) {
	fail := []struct{ name, src string }{
		{"ALTER TABLE adds it to alert_cases", `-- +goose Up
ALTER TABLE alert_cases
  ADD COLUMN incident_id UUID REFERENCES incidents(id);
`},
		{"CREATE TABLE puts it on alerts", `-- +goose Up
CREATE TABLE IF NOT EXISTS alerts (
  id          UUID PRIMARY KEY,
  incident_id UUID
);
`},
		{"ALTER TABLE ONLY on notifications", `-- +goose Up
ALTER TABLE ONLY public.notifications ADD COLUMN incident_id UUID;
`},
		{"a quoted schema and table", `-- +goose Up
ALTER TABLE "public"."alerts" ADD COLUMN incident_id uuid;
`},
		{"an index on notification_deliveries", `-- +goose Up
CREATE INDEX deliveries_incident_idx ON notification_deliveries (incident_id);
`},
		{"a qualified reference to alert_cases.incident_id", `-- +goose Up
COMMENT ON COLUMN alert_cases.incident_id IS 'which incident';
`},
		{"a quoted qualified reference", `-- +goose Up
SELECT "alerts"."incident_id" FROM alerts;
`},
	}
	for _, c := range fail {
		t.Run("fails/"+c.name, func(t *testing.T) {
			if got := scan(t, "00999_x.sql", c.src); !fired(got, "incident_id") {
				t.Errorf("incident_id on a signal row passed the gate; findings: %v", got)
			}
		})
	}

	pass := []struct{ name, src string }{
		{"the Incident membership table keys on it", `-- +goose Up
CREATE TABLE incident_members (
  incident_id UUID NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
  case_id     UUID NOT NULL REFERENCES alert_cases(id),
  PRIMARY KEY (incident_id, case_id)
);
CREATE INDEX incident_members_case_idx ON incident_members (case_id, incident_id);
`},
		{"a later ALTER on the membership table, after a signal-table statement", `-- +goose Up
ALTER TABLE alert_cases ADD COLUMN number BIGINT;
ALTER TABLE incident_members ADD COLUMN drawn_by_correlator UUID;
ALTER TABLE incident_members ADD CONSTRAINT members_incident_fk FOREIGN KEY (incident_id) REFERENCES incidents(id);
`},
		{"a read that joins a signal table to the membership table", `-- +goose Up
SELECT c.id, m.incident_id
  FROM alert_cases c
  JOIN incident_members m ON m.case_id = c.id;
`},
		{"a goose Down restoring the old world", `-- +goose Up
SELECT 1;
-- +goose Down
ALTER TABLE alert_cases ADD COLUMN incident_id UUID;
`},
	}
	for _, c := range pass {
		t.Run("passes/"+c.name, func(t *testing.T) {
			if got := scan(t, "00999_x.sql", c.src); fired(got, "incident_id") {
				t.Errorf("incident_id off a signal row failed the gate; findings: %v", got)
			}
		})
	}
}

// TestIncidentIDInGoIsScopedByTheRawString covers where this repository's SQL
// lives outside db/migrations: Go raw strings. The raw string is a statement
// boundary, so DDL in one is judged by its own head, and a struct tag naming
// the column is no statement at all.
func TestIncidentIDInGoIsScopedByTheRawString(t *testing.T) {
	bad := "package x\n\nconst q = `ALTER TABLE alert_cases ADD COLUMN incident_id UUID`\n"
	if got := scan(t, "x.go", bad); !fired(got, "incident_id") {
		t.Errorf("DDL in a Go raw string putting incident_id on alert_cases passed; findings: %v", got)
	}

	good := "package x\n\n" +
		"const q = `ALTER TABLE alert_cases ADD COLUMN number BIGINT`\n\n" +
		"type Member struct {\n\tIncidentID string `db:\"incident_id\"`\n}\n\n" +
		"const r = `INSERT INTO incident_members (incident_id, case_id) VALUES ($1, $2)`\n"
	if got := scan(t, "x.go", good); fired(got, "incident_id") {
		t.Errorf("a membership struct tag or insert failed the gate; findings: %v", got)
	}
}

// TestTheBareWordIsNotBanned pins the other half of ADR 0052's lint line: the
// noun is admitted. It was never in `banned` — only in the prose bans and AC-49's
// grep — and this keeps a well-meant "restore" from re-adding it here.
func TestTheBareWordIsNotBanned(t *testing.T) {
	src := "package incident\n\ntype Incident struct{ ID string }\n\nconst kind = \"incident\"\n"
	if got := scan(t, "incident.go", src); len(got) != 0 {
		t.Errorf("the noun Incident failed the gate; findings: %v", got)
	}
}

// TestTheOtherColumnRulesStayUnscoped is the guard on the narrowing: scoping
// was granted to one column for one ADR, and `ticket_id` on a table nobody has
// heard of is still a violation.
func TestTheOtherColumnRulesStayUnscoped(t *testing.T) {
	src := "-- +goose Up\nCREATE TABLE anything (ticket_id TEXT);\n"
	if got := scan(t, "00999_x.sql", src); !fired(got, "ticket_id") {
		t.Errorf("ticket_id off a signal row passed; only incident_id is scoped. findings: %v", got)
	}
}
