package integration

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/app"
	"github.com/thulasiram/oto/internal/platform/id"
)

// ⭐⭐ THE TESTS THAT WOULD HAVE CAUGHT TWO FIELDS THAT WERE WRITTEN AND NEVER READ.
//
// Both defects here share one shape, and it is the shape that survives every
// build, every schema validator and every code review: a field that is DECLARED
// on the wire, OPTIONAL in the contract, and populated by nothing. Absence and
// "nothing to report" are then the same bytes, so no client can tell them apart
// and no test that asserts on values rather than on PRESENCE can fail.
//
//  1. `suppressed_by` was written to `alert_cases`, SELECTed, scanned into
//     the row struct — and never unmarshalled. `domain.Case` had no such
//     field, so the one column that answers "WHICH silence is muting this alert"
//     stopped at the repository. An operator saw `suppression_reason: silence`
//     and could not learn the silence id, which was sitting in the row.
//
//  2. `note` on unack and unsnooze was bound from the body, length-validated
//     against the contract's 2000-character ceiling, and then discarded by
//     `if _, err := optionalBody[...]`. A responder typing "un-acking, it's back"
//     got a 200 and no record anywhere. The validation made it worse: a 2001
//     character note was rejected, which is proof to the caller that the field
//     is live.
//
// Both assert against the real server over a real Postgres, because both bugs
// lived precisely in the seam between a layer that had the value and a layer
// that never asked for it.

// caseView is the slice of CaseDTO these tests read.
type caseView struct {
	ID                uuid.UUID `json:"id"`
	State             string    `json:"state"`
	SuppressionReason *string   `json:"suppression_reason"`
	SuppressedBy      *struct {
		SilencedBy  []string `json:"silenced_by"`
		InhibitedBy []string `json:"inhibited_by"`
		MutedBy     []string `json:"muted_by"`
	} `json:"suppressed_by"`
}

// timelineEvent is the slice of AlertEventDTO these tests read.
type timelineEvent struct {
	Type    string         `json:"type"`
	Summary string         `json:"summary"`
	Payload map[string]any `json:"payload"`
}

// TestSuppressedByNamesTheSilence.
func TestSuppressedByNamesTheSilence(t *testing.T) {
	env := newEnv(t)
	seed := seedSuppressed(t, env)

	// ---- 1. The suppressed episode names its witnesses --------------------
	var suppressed struct {
		Data caseView `json:"data"`
	}
	env.do(t, http.MethodGet, "/api/v1/cases/"+seed.suppressedCase.String(),
		seed.token, nil, http.StatusOK, &suppressed)

	got := suppressed.Data
	if got.SuppressionReason == nil || *got.SuppressionReason != "silence" {
		t.Fatalf("suppression_reason = %v, want silence", got.SuppressionReason)
	}
	if got.SuppressedBy == nil {
		t.Fatal("a silenced case carries no suppressed_by. The ids ARE in the row: " +
			"`alert_cases.suppressed_by` was written, selected and scanned, and then " +
			"never unmarshalled, so `suppression_reason: silence` could never be followed " +
			"to the silence doing the silencing")
	}
	if len(got.SuppressedBy.SilencedBy) != 1 || got.SuppressedBy.SilencedBy[0] != "b3d1f0aa-sil" {
		t.Fatalf("silenced_by = %v, want [b3d1f0aa-sil]", got.SuppressedBy.SilencedBy)
	}
	if len(got.SuppressedBy.InhibitedBy) != 1 || got.SuppressedBy.InhibitedBy[0] != "f00dcafe" {
		t.Fatalf("inhibited_by = %v, want [f00dcafe]. All THREE witnesses come off the same "+
			"Alertmanager status object; keeping only silences leaves an inhibited alert "+
			"exactly as unexplained as before", got.SuppressedBy.InhibitedBy)
	}

	// ---- 2. A firing episode names nobody, whatever the row says ----------
	//
	// ⛔ The seed deliberately leaves stale witnesses on an OPEN, unsuppressed row
	// — which
	// is what every row written before the persistence path learned to clear
	// them looks like. Reporting them would make oto say "silenced by <id>"
	// about an alert that is demonstrably firing, which is a worse failure than
	// saying nothing.
	var firing struct {
		Data caseView `json:"data"`
	}
	env.do(t, http.MethodGet, "/api/v1/cases/"+seed.firingCase.String(),
		seed.token, nil, http.StatusOK, &firing)

	// ⭐ `open` AND NO `suppression_reason` IS WHAT "firing" MEANS ON THE WIRE NOW
	// (ADR 0040). The DTO's `state` is the EPISODE's, `open | closed`; the pair of
	// fields below is the four-word reading, and it is exactly the pair the
	// `SuppressedBy` gate consults. Asserting both is what keeps this a test about
	// the witnesses rather than about the enum that carries them.
	if firing.Data.State != "open" {
		t.Fatalf("state = %q, want open", firing.Data.State)
	}
	if firing.Data.SuppressionReason != nil {
		t.Fatalf("suppression_reason = %v on the row that stands in for a firing episode; "+
			"with one set it would READ as suppressed and the gate below would be asserting "+
			"the opposite of what this test is for", firing.Data.SuppressionReason)
	}
	if firing.Data.SuppressedBy != nil {
		t.Fatalf("a firing case reports suppressed_by = %+v; witnesses are meaningful "+
			"in exactly the states `suppression_reason` is, and in no others",
			firing.Data.SuppressedBy)
	}
}

// TestUnackAndUnsnoozeNotesReachTheTimeline drives the real endpoints.
func TestUnackAndUnsnoozeNotesReachTheTimeline(t *testing.T) {
	env := newEnv(t)
	seed := seedSuppressed(t, env)
	alert := "/api/v1/alerts/" + seed.alertID.String()
	// ⭐ ACK IS ADDRESSED BY CASE, NOT BY ALERT. A receipt is a fact about ONE
	// ephemeral firing episode, so the route says so; the alert is reached
	// through the case rather than the other way round. `firingCase` is
	// `alertID`'s open episode, which is what a human would be acknowledging.
	firingCase := "/api/v1/cases/" + seed.firingCase.String()

	// ---- 1. ack, then unack WITH a note ----------------------------------
	env.do(t, http.MethodPost, firingCase+"/ack", seed.token,
		map[string]any{"note": "looking at it"}, http.StatusOK, nil)
	env.do(t, http.MethodPost, firingCase+"/unack", seed.token,
		map[string]any{"note": "un-acking, it is back"}, http.StatusOK, nil)

	unacked := findEvent(t, env, seed, "case.unacknowledged")
	if unacked.Payload["reason"] != "manual" {
		t.Fatalf("unack reason = %v, want manual", unacked.Payload["reason"])
	}
	if note, _ := unacked.Payload["note"].(string); note != "un-acking, it is back" {
		t.Fatalf("the unack note never reached the timeline: payload = %+v.\n"+
			"The handler bound it, validated it against the contract's 2000-character "+
			"ceiling, and dropped it. `ack_note` is cleared by the withdrawal, so the "+
			"event payload is the only place this fact can live", unacked.Payload)
	}

	// ---- 2. snooze, then unsnooze WITH a note ----------------------------
	env.do(t, http.MethodPost, alert+"/snooze", seed.token,
		map[string]any{"duration_seconds": 3600, "note": "deploying a fix"}, http.StatusOK, nil)
	env.do(t, http.MethodPost, alert+"/unsnooze", seed.token,
		map[string]any{"note": "fix did not land, want the pages back"}, http.StatusOK, nil)

	unsnoozed := findEvent(t, env, seed, "alert.unsnoozed")
	if unsnoozed.Payload["reason"] != "manual" {
		t.Fatalf("unsnooze reason = %v, want manual", unsnoozed.Payload["reason"])
	}
	if note, _ := unsnoozed.Payload["note"].(string); note != "fix did not land, want the pages back" {
		t.Fatalf("the unsnooze note never reached the timeline: payload = %+v.\n"+
			"The contract calls it \"Optional note recorded with the wake-up\" and nothing "+
			"was recorded anywhere", unsnoozed.Payload)
	}

	// ⛔ The snooze's OWN note is untouched by the wake-up. They are two facts
	// about two moments, and letting the second overwrite the first would erase
	// why the quiet period was asked for.
	var snoozes struct {
		Data []struct {
			Note *string `json:"note"`
		} `json:"data"`
	}
	env.do(t, http.MethodGet, alert+"/snoozes", seed.token, nil, http.StatusOK, &snoozes)
	if len(snoozes.Data) != 1 || snoozes.Data[0].Note == nil || *snoozes.Data[0].Note != "deploying a fix" {
		t.Fatalf("the snooze's own note was clobbered by the wake-up: %+v", snoozes.Data)
	}
}

// findEvent reads the alert timeline and returns the newest event of one type.
func findEvent(t *testing.T, e *env, seed suppressedSeed, kind string) timelineEvent {
	t.Helper()

	var body struct {
		Data []timelineEvent `json:"data"`
	}
	e.do(t, http.MethodGet, "/api/v1/alerts/"+seed.alertID.String()+"/events?limit=100",
		seed.token, nil, http.StatusOK, &body)

	for _, ev := range body.Data {
		if ev.Type == kind {
			return ev
		}
	}
	types := make([]string, 0, len(body.Data))
	for _, ev := range body.Data {
		types = append(types, ev.Type)
	}
	t.Fatalf("no %s event on the timeline; saw %v", kind, types)
	return timelineEvent{}
}

// suppressedSeed is the world both tests run against.
type suppressedSeed struct {
	token string
	// alertID's open episode is `firing` and carries STALE witnesses on the row.
	alertID    uuid.UUID
	firingCase uuid.UUID
	// suppressedCase belongs to a second alert and is genuinely silenced.
	suppressedCase uuid.UUID
}

// seedSuppressed writes two alerts: one firing (with stale witnesses left on the
// row on purpose) and one suppressed by a silence and an inhibition.
//
// It writes SQL directly, like seedFanOut, because these tests are about what
// the READ path emits; driving the reconciler would make a failure ambiguous
// between "the read path drops the column" and "the reconciler wrote nothing".
func seedSuppressed(t *testing.T, e *env) suppressedSeed {
	t.Helper()

	boot, err := app.Bootstrap(e.ctx, e.pool, app.BootstrapRequest{
		OrgSlug: "witness", OrgName: "Witness", Email: "ops@witness.example",
		DisplayName: "Ops", Password: "correct-horse-battery-staple", TokenName: "bootstrap",
	}, time.Now())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	var orgID uuid.UUID
	if err := e.pool.QueryRow(e.ctx,
		`SELECT id FROM orgs WHERE slug = 'witness'`).Scan(&orgID); err != nil {
		t.Fatalf("read org: %v", err)
	}

	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	out := suppressedSeed{
		token:          boot.Token,
		alertID:        id.New(),
		firingCase:     id.New(),
		suppressedCase: id.New(),
	}
	clusterID, sourceID := id.New(), id.New()
	silencedAlert := id.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", sql[:min(60, len(sql))], err)
		}
	}

	// `created_at`/`updated_at` are NAMED on both: 00034 removed their DEFAULT
	// now(), because these tables' timestamps come from the application.
	exec(`INSERT INTO clusters (id, org_id, cluster_key, display_name, created_at, updated_at)
	      VALUES ($1,$2,'prod','prod',$3,$3)`, clusterID, orgID, now)
	exec(`INSERT INTO alert_sources (id, org_id, cluster_id, name, kind, base_url,
	         created_at, updated_at)
	      VALUES ($1,$2,$3,'am','alertmanager','http://am.test',$4,$4)`,
		sourceID, orgID, clusterID, now)

	exec(`INSERT INTO alerts (id, org_id, cluster_id, alert_key, source_fingerprint, alertname,
	         severity, cluster_key, labels, state,
	         first_seen_at, last_seen_at, last_state_change_at, total_cases)
	      VALUES ($1,$2,$3,'ak_0123456789abcdefghijklmnop','3f8c1a2b9d4e5f60','HighErrorRate',
	         'critical','prod','{"alertname":"HighErrorRate"}'::jsonb,'firing',$4,$4,$4,1)`,
		out.alertID, orgID, clusterID, now)

	// ⭐ ADR 0041: a silenced alert is `state = 'firing'` PLUS the suppression axis.
	// `suppressed` is not a value this column can hold any more — it occupied the
	// slot `firing` needed — so the silence is seeded where it now lives.
	exec(`INSERT INTO alerts (id, org_id, cluster_id, alert_key, source_fingerprint, alertname,
	         severity, cluster_key, labels, state, suppression_reason, suppressed_by,
	         first_seen_at, last_seen_at, last_state_change_at, total_cases)
	      VALUES ($1,$2,$3,'ak_abcdefghijklmnopqrstuv0123','a1b2c3d4e5f60789','DiskFilling',
	         'warning','prod','{"alertname":"DiskFilling"}'::jsonb,'firing','silence',
	         '{"silencedBy":["sil-1"],"inhibitedBy":[],"mutedBy":[]}'::jsonb,$4,$4,$4,1)`,
		silencedAlert, orgID, clusterID, now)

	// ⛔ STALE WITNESSES ON A FIRING ROW. This is what every case written
	// before the persistence path learned to clear the column looks like, and
	// the read path must refuse to report them.
	exec(`WITH allocated AS (
	        INSERT INTO org_case_numbers (org_id, next_number) VALUES ($2, 2)
	        ON CONFLICT (org_id) DO UPDATE
	                SET next_number = org_case_numbers.next_number + 1
	          RETURNING next_number - 1 AS number
	      )
	      INSERT INTO alert_cases (id, org_id, alert_id, seq, number, state, suppressed_by,
	         started_at, last_observed_at, source_starts_at)
	      SELECT $1,$2,$3,1,(SELECT number FROM allocated),'open',
	         '{"silencedBy":["stale-sil"],"inhibitedBy":[],"mutedBy":[]}'::jsonb,
	         $4,$4,$4`,
		out.firingCase, orgID, out.alertID, now)

	exec(`WITH allocated AS (
	        INSERT INTO org_case_numbers (org_id, next_number) VALUES ($2, 2)
	        ON CONFLICT (org_id) DO UPDATE
	                SET next_number = org_case_numbers.next_number + 1
	          RETURNING next_number - 1 AS number
	      )
	      INSERT INTO alert_cases (id, org_id, alert_id, seq, number, state, suppression_reason,
	         suppressed_by, suppress_count, started_at, last_observed_at, source_starts_at)
	      SELECT $1,$2,$3,1,(SELECT number FROM allocated),'open','silence',
	         '{"silencedBy":["b3d1f0aa-sil"],"inhibitedBy":["f00dcafe"],"mutedBy":[]}'::jsonb,
	         1,$4,$4,$4`,
		out.suppressedCase, orgID, silencedAlert, now)

	exec(`UPDATE alerts SET current_case_id = $2 WHERE id = $1`,
		out.alertID, out.firingCase)
	exec(`UPDATE alerts SET current_case_id = $2 WHERE id = $1`,
		silencedAlert, out.suppressedCase)

	return out
}
