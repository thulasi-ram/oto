package integration

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/app"
	"github.com/thulasiram/oto/internal/platform/id"
)

// ⭐⭐ THE TEST THAT WOULD HAVE CAUGHT THE ORIGINAL BUG.
//
// `delivery_summary` was declared on four response schemas — the alert detail,
// the case detail, the group detail and the notification detail — and
// emitted by NONE of them. ⛔ IT IS THREE SCHEMAS NOW: the group detail left the
// contract with `alert_groups` (git-bug `7570090`), and A CONVERSATION IS A CASE,
// so the case detail is where a thread's roll-up is read. Because the field was optional, every schema
// validator passed and the absence was invisible: nothing in the build, the
// contract or the test suite could tell "this endpoint has no deliveries to
// report" from "this endpoint never computes the field".
//
// What that costs is the product's central claim. oto's silence is only worth
// anything if it can be told apart from failure, and a user looking at a quiet
// Slack channel needs to know whether nothing fired or whether four deliveries
// died on an expired token. From outside the database those are identical.
//
// So this test asserts PRESENCE FIRST, on all three, against a real Postgres with
// a fan-out in five different states — sent, skipped, dead, failed and pending —
// and only then checks the arithmetic.

// deliverySummary is the shape all four endpoints must carry.
type deliverySummary struct {
	Total          *int32  `json:"total"`
	Sent           *int32  `json:"sent"`
	Failed         *int32  `json:"failed"`
	Dead           *int32  `json:"dead"`
	Skipped        *int32  `json:"skipped"`
	Pending        *int32  `json:"pending"`
	LastErrorClass *string `json:"last_error_class"`
	LastSentAt     *string `json:"last_sent_at"`
}

// present reports whether every count the schema declares REQUIRED actually
// arrived. Pointers, so a missing key and a zero are distinguishable — which is
// the whole point of the exercise.
func (d deliverySummary) present() bool {
	return d.Total != nil && d.Sent != nil && d.Failed != nil &&
		d.Dead != nil && d.Skipped != nil && d.Pending != nil
}

// fanOut is the seeded world: one alert with one open Case, notified once,
// fanned out over five channels that ended in five different states.
//
// ⛔ `groupID` WAS A FIELD HERE AND IS DELETED (git-bug `7570090`). The Case IS
// the conversation, so `caseID` is the only conversation id the seed has.
type fanOut struct {
	token          string
	alertID        uuid.UUID
	caseID         uuid.UUID
	notificationID uuid.UUID
	// suppressedID is a second intent with NO deliveries at all. It is the case
	// an omitted field hid most damagingly: "oto formed this intent and told
	// nobody, and here is why".
	suppressedID uuid.UUID
}

// TestDeliverySummaryIsEmittedByAllThreeDetailEndpoints.
//
// ⛔ IT WAS `...ByAllFourDetailEndpoints` AND THE FOURTH IS DELETED, NOT
// RETARGETED (git-bug `7570090`): `GET /alert-groups/{id}` left the contract with
// the entity, and `GET /cases/{id}` — probe 2 — already covers the conversation
// the group detail used to report on.
func TestDeliverySummaryIsEmittedByAllThreeDetailEndpoints(t *testing.T) {
	env := newEnv(t)
	seed := seedFanOut(t, env)

	// ---- 1. GET /alerts/{id} -------------------------------------------
	//
	// ⭐ THE ALERT ROLL-UP HAS TO REACH THROUGH ITS CASES, and the seed is built to
	// prove it: the `fired` intent below carries `alert_id NULL` and names the Case
	// as its subject, because that is what a `fired` notification actually looks
	// like once a conversation is a Case (git-bug `7570090`). A roll-up that
	// counted only `notifications.alert_id` reports zero here — a false silence on
	// the very page the field exists for — and the old fan-out through
	// `notifications.group_id` is gone, so `alert_cases` is the only path left.
	var alertBody struct {
		Data struct {
			DeliverySummary *deliverySummary `json:"delivery_summary"`
		} `json:"data"`
	}
	env.do(t, http.MethodGet, "/api/v1/alerts/"+seed.alertID.String(),
		seed.token, nil, http.StatusOK, &alertBody)
	assertFanOut(t, "GET /alerts/{id}", alertBody.Data.DeliverySummary)

	// ---- 2. GET /cases/{id} --------------------------------------
	var occBody struct {
		Data struct {
			DeliverySummary *deliverySummary `json:"delivery_summary"`
		} `json:"data"`
	}
	env.do(t, http.MethodGet, "/api/v1/cases/"+seed.caseID.String(),
		seed.token, nil, http.StatusOK, &occBody)
	assertFanOut(t, "GET /cases/{id}", occBody.Data.DeliverySummary)

	// ---- 3. GET /notifications/{id} ------------------------------------
	//
	// ⛔ `GET /alert-groups/{id}` WAS PROBE 3 AND IS DELETED (git-bug `7570090`).
	var notifBody struct {
		Data struct {
			DeliverySummary *deliverySummary `json:"delivery_summary"`
		} `json:"data"`
	}
	env.do(t, http.MethodGet, "/api/v1/notifications/"+seed.notificationID.String(),
		seed.token, nil, http.StatusOK, &notifBody)
	assertFanOut(t, "GET /notifications/{id}", notifBody.Data.DeliverySummary)

	// ---- 4. The intent nobody was told about ---------------------------
	//
	// ⛔ THE CASE THE OLD CODE DROPPED SILENTLY. `summarise` returned nil for an
	// empty fan-out and the field carried `omitempty`, so a SUPPRESSED
	// notification — the one whose whole story is "oto decided not to tell you"
	// — answered with no `delivery_summary` at all.
	var suppressed struct {
		Data struct {
			Status           string           `json:"status"`
			SuppressedReason *string          `json:"suppressed_reason"`
			DeliverySummary  *deliverySummary `json:"delivery_summary"`
		} `json:"data"`
	}
	env.do(t, http.MethodGet, "/api/v1/notifications/"+seed.suppressedID.String(),
		seed.token, nil, http.StatusOK, &suppressed)

	s := suppressed.Data.DeliverySummary
	if s == nil || !s.present() {
		t.Fatalf("a suppressed notification carries no delivery_summary (%+v). "+
			"An all-zero roll-up is an ANSWER — nobody was told — and omitting it makes "+
			"that indistinguishable from a server that never computed one", s)
	}
	if *s.Total != 0 || *s.Sent != 0 || *s.Dead != 0 || *s.Pending != 0 {
		t.Fatalf("a suppressed notification reports deliveries: %+v", s)
	}
	if suppressed.Data.SuppressedReason == nil {
		t.Fatal("the suppressed intent carries no reason; silence that cannot be explained " +
			"destroys trust (§B.6)")
	}
}

// assertFanOut checks presence first and arithmetic second.
//
// The seeded fan-out is five deliveries: one sent, one skipped, one dead, one
// failed and one pending. `skipped` counts BOTH separately and inside `sent`,
// exactly as the contract describes — a coalesced no-op update means the
// destination already shows this content, which is a healthy quiet thread rather
// than a failure.
func assertFanOut(t *testing.T, where string, got *deliverySummary) {
	t.Helper()

	if got == nil {
		t.Fatalf("%s omits delivery_summary entirely. The field is declared on this schema; "+
			"emitting nothing makes oto's silence indistinguishable from \"no alert\", which is "+
			"the exact failure it exists to prevent", where)
	}
	if !got.present() {
		t.Fatalf("%s emits a partial delivery_summary (%+v): every count the schema declares "+
			"required must be there, because a missing count reads as zero to a client", where, got)
	}

	want := map[string]int32{
		"total": 5, "sent": 2, "failed": 1, "dead": 1, "skipped": 1, "pending": 1,
	}
	actual := map[string]int32{
		"total": *got.Total, "sent": *got.Sent, "failed": *got.Failed,
		"dead": *got.Dead, "skipped": *got.Skipped, "pending": *got.Pending,
	}
	for k, v := range want {
		if actual[k] != v {
			t.Fatalf("%s: %s = %d, want %d (whole roll-up: %+v)", where, k, actual[k], v, actual)
		}
	}

	// ⭐ A NON-ZERO `dead` IS A PRODUCT SIGNAL, NOT A FOOTNOTE, and it is only
	// actionable with the class beside it: "one died" versus "one died because
	// the token expired" are different afternoons.
	if got.LastErrorClass == nil {
		t.Fatalf("%s reports a dead delivery with no last_error_class", where)
	}
	if *got.LastErrorClass != "auth_expired" {
		t.Fatalf("%s: last_error_class = %q, want auth_expired", where, *got.LastErrorClass)
	}
	if got.LastSentAt == nil {
		t.Fatalf("%s reports two sent deliveries and no last_sent_at", where)
	}
}

// seedFanOut writes one alert, one open Case — which IS the conversation — one
// notification with five deliveries in five states, and one suppressed intent
// with none.
//
// It writes SQL directly on purpose: this test is about what the READ path
// emits, and driving the write path would make a failure here ambiguous between
// "the roll-up is wrong" and "the notify worker did something else".
func seedFanOut(t *testing.T, e *env) fanOut {
	t.Helper()

	boot, err := app.Bootstrap(e.ctx, e.pool, app.BootstrapRequest{
		OrgSlug: "fanout", OrgName: "Fan Out", Email: "ops@fanout.example",
		DisplayName: "Ops", Password: "correct-horse-battery-staple", TokenName: "bootstrap",
	}, time.Now())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	var orgID uuid.UUID
	if err := e.pool.QueryRow(e.ctx,
		`SELECT id FROM orgs WHERE slug = 'fanout'`).Scan(&orgID); err != nil {
		t.Fatalf("read org: %v", err)
	}

	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	out := fanOut{
		token:          boot.Token,
		alertID:        id.New(),
		caseID:         id.New(),
		notificationID: id.New(),
		suppressedID:   id.New(),
	}
	clusterID, sourceID, credentialID := id.New(), id.New(), id.New()
	connectionID := id.New()

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

	// `current_case_id` is filled in after the case exists:
	// `alerts_current_case_fk` points forward, and the projection is written by
	// the same transaction that opens the episode in the real write path.
	exec(`INSERT INTO alerts (id, org_id, cluster_id, alert_key, source_fingerprint, alertname,
	         severity, cluster_key, labels, state,
	         first_seen_at, last_seen_at, last_state_change_at, total_cases)
	      VALUES ($1,$2,$3,'ak_0123456789abcdefghijklmnop','3f8c1a2b9d4e5f60','HighErrorRate',
	         'critical','prod','{"alertname":"HighErrorRate"}'::jsonb,'firing',$4,$4,$4,1)`,
		out.alertID, orgID, clusterID, now)

	// ⛔ AN `alert_groups` GENERATION WAS SEEDED HERE AND IS DELETED (git-bug
	// `7570090`). The episode below is the conversation; there is no generation to
	// open and no membership to record.
	exec(`WITH allocated AS (
	        INSERT INTO org_case_numbers (org_id, next_number) VALUES ($2, 2)
	        ON CONFLICT (org_id) DO UPDATE
	                SET next_number = org_case_numbers.next_number + 1
	          RETURNING next_number - 1 AS number
	      )
	      INSERT INTO alert_cases (id, org_id, alert_id, seq, number, state,
	         started_at, last_observed_at, source_starts_at)
	      SELECT $1,$2,$3,1,(SELECT number FROM allocated),'open',$4,$4,$4`,
		out.caseID, orgID, out.alertID, now)

	exec(`UPDATE alerts SET current_case_id = $2 WHERE id = $1`,
		out.alertID, out.caseID)

	// A Slack channel MUST carry a credential (`channels_cred_ck`) and the sealed
	// blob has a 29-byte floor — a 12-byte nonce, a 16-byte tag and at least one
	// byte of plaintext. The seed satisfies the real constraint rather than
	// relaxing it; nothing ever decrypts this row.
	// `created_at` is NAMED: 00033 removed the database default, so this table's
	// timestamps come from the application like `channels`' do.
	exec(`INSERT INTO channel_credentials (id, org_id, kind, sealed, key_version, created_at)
	      VALUES ($1,$2,'slack_bot_token', decode(repeat('00', 32), 'hex'), 1, $3)`,
		credentialID, orgID, now)

	// ⛔ THE CREDENTIAL HANGS OFF A CONNECTION NOW, NOT OFF EACH CHANNEL (ADR 0047,
	// migration 00075). That is exactly the shape this fixture wants anyway: the
	// seven channels below are seven destinations in ONE Slack workspace, which is
	// the case that used to mean the same sealed token pasted seven times.
	// `channels.connection_id` is NOT NULL, so this row is a prerequisite and not
	// extra scenery.
	exec(`INSERT INTO channel_connections (id, org_id, type, name, config, credential_id,
	         created_at, updated_at)
	      VALUES ($1,$2,'slack',$3,'{"team_id":"T9TK3CUKW"}'::jsonb,$4,$5,$5)`,
		connectionID, orgID, "workspace-"+orgID.String()[:8], credentialID, now)

	// ⛔ The intent is CASE-scoped, with alert_id NULL — which is what a `fired`
	// notification actually looks like now that a conversation is a Case (git-bug
	// `7570090`). An alert roll-up that only counted `notifications.alert_id` would
	// report zero for this, and zero here is the false silence the whole feature
	// exists to prevent. `notifications_subject_ck`'s `case` arm demands
	// `case_id IS NOT NULL AND subject_id = case_id`, so the subject and the
	// conversation are the same id twice and that is not a redundancy: subject is
	// what the fact is ABOUT, conversation is where it LANDS.
	exec(`INSERT INTO notifications (id, org_id, subject_kind, subject_id, case_id, conversation_kind, conversation_id, reason,
	         state_version, idempotency_key, status, created_at, updated_at)
	      VALUES ($1,$2,'case',$3,$3,'case',$3,'fired',1,
	         '0000000000000000000000000000000000000000000000000000000000000001','partial',$4,$4)`,
		out.notificationID, orgID, out.caseID, now)

	exec(`INSERT INTO notifications (id, org_id, subject_kind, subject_id, conversation_kind, conversation_id, alert_id,
	         case_id, reason, state_version, idempotency_key, status, suppressed_reason,
	         created_at, updated_at)
	      VALUES ($1,$2,'case',$3,'case',$3,$4,$3,'acked',1,
	         '0000000000000000000000000000000000000000000000000000000000000002','suppressed',
	         'throttled',$5,$5)`,
		out.suppressedID, orgID, out.caseID, out.alertID, now)

	// Five channels, because `deliveries_fanout_uniq` is (notification, channel,
	// mode) and five states need five destinations to be expressible at once.
	type seedDelivery struct {
		status     string
		errorClass string
		sentAt     *time.Time
	}
	sentAt := now.Add(time.Minute)
	// The order is the `updated_at` order, and it is chosen: the `dead` row moves
	// LAST, so `last_error_class` has to report the terminal failure rather than
	// the retryable one that happened first. "The token expired" and "a request
	// timed out and will be retried" are different afternoons.
	deliveries := []seedDelivery{
		{status: "sent", sentAt: &sentAt},
		{status: "skipped"},
		{status: "failed", errorClass: "retryable"},
		{status: "dead", errorClass: "auth_expired"},
		{status: "pending"},
	}
	for i, d := range deliveries {
		channelID := id.New()
		// Named by index, not by a uuid prefix: ids here are UUIDv7 and their
		// leading bytes are a millisecond timestamp, so five rows minted in one
		// loop share a prefix and collide on `channels_name_uniq`.
		// `created_at`/`updated_at` are named because 00032 removed the database
		// default: `channels` timestamps come from the application, never from now().
		exec(`INSERT INTO channels (id, org_id, type, name, config, connection_id,
		         created_at, updated_at)
		      VALUES ($1,$2,'slack',$3,'{"channel":"#sre"}'::jsonb,$4,$5,$5)`,
			channelID, orgID, "sre-"+d.status, connectionID, now)

		var (
			providerMessageID *string
			errText           *string
			errClass          *string
			// `deliveries_err_ck` requires both halves on failed and dead;
			// `deliveries_sent_ck` requires a provider handle and a timestamp on
			// sent. The seed satisfies the real constraints rather than relaxing
			// them, because a roll-up over rows the schema would reject proves
			// nothing.
			updatedAt = now.Add(time.Duration(i) * time.Second)
		)
		if d.errorClass != "" {
			msg := "seeded " + d.status
			errText, errClass = &msg, &d.errorClass
		}
		if d.status == "sent" {
			ts := "1712345678.000100"
			providerMessageID = &ts
		}
		exec(`INSERT INTO notification_deliveries (id, org_id, notification_id, channel_id, mode,
		         status, attempts, provider_message_id, error, error_class, sent_at,
		         created_at, updated_at)
		      VALUES ($1,$2,$3,$4,'post_root',$5,0,$6,$7,$8,$9,$10,$11)`,
			id.New(), orgID, out.notificationID, channelID, d.status,
			providerMessageID, errText, errClass, d.sentAt, now, updatedAt)
	}

	// ⛔ THE DECOY. A second alert with its own Case — its own CONVERSATION — its
	// own notification and its own two deliveries, which the alert above has
	// nothing to do with. Every roll-up above must still report five. Without it, a
	// query that forgot its subject predicate — or scoped only by org — would pass
	// every other assertion in this file.
	//
	// ⚠️ IT IS A SECOND ALERT AND NOT A SECOND CASE OF THE SAME ONE, deliberately:
	// `case_one_open_idx` admits one open Case per Alert, and a decoy the schema
	// refuses is a decoy that silently stops decoying.
	decoyAlert, decoyCase, decoyNotification := id.New(), id.New(), id.New()
	exec(`INSERT INTO alerts (id, org_id, cluster_id, alert_key, source_fingerprint, alertname,
	         severity, cluster_key, labels, state,
	         first_seen_at, last_seen_at, last_state_change_at, total_cases)
	      VALUES ($1,$2,$3,'ak_abcdefghijklmnopqrstuv0123','9a8b7c6d5e4f3021','Unrelated',
	         'warning','prod','{"alertname":"Unrelated"}'::jsonb,'firing',$4,$4,$4,1)`,
		decoyAlert, orgID, clusterID, now)
	exec(`WITH allocated AS (
	        INSERT INTO org_case_numbers (org_id, next_number) VALUES ($2, 2)
	        ON CONFLICT (org_id) DO UPDATE
	                SET next_number = org_case_numbers.next_number + 1
	          RETURNING next_number - 1 AS number
	      )
	      INSERT INTO alert_cases (id, org_id, alert_id, seq, number, state,
	         started_at, last_observed_at, source_starts_at)
	      SELECT $1,$2,$3,1,(SELECT number FROM allocated),'open',$4,$4,$4`,
		decoyCase, orgID, decoyAlert, now)
	exec(`UPDATE alerts SET current_case_id = $2 WHERE id = $1`, decoyAlert, decoyCase)
	exec(`INSERT INTO notifications (id, org_id, subject_kind, subject_id, case_id, conversation_kind, conversation_id, reason,
	         state_version, idempotency_key, status, created_at, updated_at)
	      VALUES ($1,$2,'case',$3,$3,'case',$3,'fired',1,
	         '0000000000000000000000000000000000000000000000000000000000000003','delivered',$4,$4)`,
		decoyNotification, orgID, decoyCase, now)
	for i := range 2 {
		channelID := id.New()
		exec(`INSERT INTO channels (id, org_id, type, name, config, connection_id,
		         created_at, updated_at)
		      VALUES ($1,$2,'slack',$3,'{"channel":"#other"}'::jsonb,$4,$5,$5)`,
			channelID, orgID, "other-"+string(rune('a'+i)), connectionID, now)
		exec(`INSERT INTO notification_deliveries (id, org_id, notification_id, channel_id, mode,
		         status, attempts, provider_message_id, sent_at, created_at, updated_at)
		      VALUES ($1,$2,$3,$4,'post_root','sent',1,'1712345678.000900',$5,$5,$5)`,
			id.New(), orgID, decoyNotification, channelID, now.Add(time.Hour))
	}

	return out
}
