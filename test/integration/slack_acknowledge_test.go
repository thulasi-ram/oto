package integration

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/app"
	"github.com/thulasiram/oto/internal/platform/config"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/platform/jobs"
)

// ⭐⭐ THE TEST FOR THE DEFECT.
//
// Every alert card oto has ever posted carries an `Acknowledge` button. The
// interaction endpoint verified Slack's HMAC, answered 200, and threw the
// payload away — `Interactions` was `nil` in the composition root. A user
// pressed the button, Slack showed them a tick, and nothing happened anywhere.
// The only action oto asks of a human was a no-op that looked like it worked,
// which is worse than having no button at all: it teaches people that
// acknowledging is pointless, and oto's whole claim is that its record is true.
//
// This file drives the REAL route table, over a REAL Postgres, with correctly
// computed signatures, and asserts on the DATABASE — because "the handler
// returned 200" is precisely what the broken version also did.
//
// There is no signing secret and no Slack app available here, so the test
// supplies its own secret and computes the MAC itself. That proves everything
// except that Slack's own signature matches oto's arithmetic, which is the one
// thing only a live workspace can prove.

const ackSigningSecret = "8f742231b10e8888abcd99yyyzzz85a5" //nolint:gosec // a fixture, not a credential

// slackWorld is the seeded world: two tenants, each with a Slack channel in the
// same workspace, and one firing alert group apiece.
type slackWorld struct {
	alphaOrg     uuid.UUID
	alphaGroup   uuid.UUID
	alphaAlert   uuid.UUID
	alphaOcc     uuid.UUID
	alphaChannel string // Slack conversation id

	betaOrg     uuid.UUID
	betaGroup   uuid.UUID
	betaChannel string

	// linkedUser is an oto user in alpha whose Slack member id is linked.
	linkedUser  uuid.UUID
	linkedEmail string
}

const slackTeam = "T9TK3CUKW"

// TestSlackAcknowledgeButtonActuallyAcknowledges is the end-to-end proof.
func TestSlackAcknowledgeButtonActuallyAcknowledges(t *testing.T) {
	env := newEnvWith(t, func(c *config.Config) {
		// HTTP interactivity, with a signing secret — the only configuration in
		// which the endpoint is enabled at all.
		c.Slack.Enabled = true
		c.Slack.Mode = "http"
		c.Slack.SigningSecret = ackSigningSecret
	})
	w := seedSlackWorld(t, env)

	// ---- 1. THE PRESS --------------------------------------------------
	//
	// A linked member of org alpha presses Acknowledge on alpha's card, in
	// alpha's channel.
	status := env.pressSlackButton(t, slackPress{
		team:        slackTeam,
		channel:     w.alphaChannel,
		user:        "U0123456789",
		userName:    "ram",
		actionID:    "oto.ack",
		value:       w.alphaGroup.String(),
		signedAt:    time.Now(),
		secret:      ackSigningSecret,
		responseURL: "https://hooks.slack.com/actions/" + slackTeam + "/1/2",
	})
	if status != http.StatusOK {
		t.Fatalf("the interaction endpoint answered %d, want 200: Slack shows the user a "+
			"failure banner for anything else", status)
	}

	// ---- 2. THE JOB ----------------------------------------------------
	//
	// The endpoint's whole job is to enqueue inside three seconds. The worker
	// pool is off in this harness, so the row is still there to look at — which
	// is exactly the assertion that matters: the press became DURABLE WORK
	// rather than being dropped on the floor.
	if n := env.countJobs(t, jobs.KindSlackInteraction); n != 1 {
		t.Fatalf("the press enqueued %d %s jobs, want 1. A verified interaction that "+
			"enqueues nothing IS the defect", n, jobs.KindSlackInteraction)
	}

	// ---- 3. THE WORK ---------------------------------------------------
	//
	// Applied directly rather than through River, so the test is about the
	// acknowledgement and not about queue timing.
	env.applySlackJob(t, jobs.SlackInteractionArgs{
		ActionID:      "oto.ack",
		Value:         w.alphaGroup.String(),
		TeamID:        slackTeam,
		ChannelID:     w.alphaChannel,
		SlackUserID:   "U0123456789",
		SlackUserName: "ram",
	})

	// ---- 4. THE RECORD -------------------------------------------------
	ackState, ackedBy, ackedLabel := env.occurrenceAck(t, w.alphaOcc)
	if ackState != "acked" {
		t.Fatalf("the occurrence is %q after an Acknowledge press, want acked. "+
			"The button is still a no-op that looks like it worked", ackState)
	}
	// The member is LINKED, so the receipt names the oto user.
	if ackedBy == nil || *ackedBy != w.linkedUser {
		t.Fatalf("acked_by = %v, want the linked oto user %s", ackedBy, w.linkedUser)
	}
	if ackedLabel != w.linkedEmail {
		t.Fatalf("acked_by_label = %q, want %q", ackedLabel, w.linkedEmail)
	}

	// The alert PROJECTION moved too — that is what every list and every card
	// renders from.
	if got := env.alertAckState(t, w.alphaAlert); got != "acked" {
		t.Fatalf("alerts.ack_state = %q, want acked", got)
	}

	// ---- 5. THE TIMELINE -----------------------------------------------
	//
	// An acknowledgement that is not on the append-only timeline is not a fact
	// oto can be asked about later.
	kind, label := env.ackEvent(t, w.alphaAlert)
	if kind != "user" {
		t.Fatalf("the timeline records actor_kind = %q for a linked member, want user", kind)
	}
	if label != w.linkedEmail {
		t.Fatalf("the timeline records actor_label = %q, want %q", label, w.linkedEmail)
	}

	// ---- 6. CLOSING THE LOOP -------------------------------------------
	//
	// ⭐ The card is updated by the DISPATCH path, not by the interaction
	// handler, so that thread ordering, delivery idempotency and the per-channel
	// rate limit still apply. What the ack owes us is the `notify.evaluate` with
	// reason `acked`, enqueued in the same transaction as the receipt.
	if n := env.countNotifyEvaluate(t, "acked"); n < 1 {
		t.Fatal("the acknowledgement enqueued no notify.evaluate with reason=acked; " +
			"the card in Slack would never change and the user would see nothing happen")
	}
}

// TestSlackAcknowledgeCannotCrossTenants is the highest-stakes property.
//
// ⛔⛔ The interaction arrives with a Slack team id and a channel, never an org.
// The tenant is resolved from the CHANNEL — a destination oto's own operator
// configured — and everything downstream runs under that scope. Here a press in
// ALPHA's channel names BETA's group id inside an otherwise perfectly authentic,
// correctly signed envelope. It must change nothing.
func TestSlackAcknowledgeCannotCrossTenants(t *testing.T) {
	env := newEnvWith(t, func(c *config.Config) {
		c.Slack.Enabled = true
		c.Slack.Mode = "http"
		c.Slack.SigningSecret = ackSigningSecret
	})
	w := seedSlackWorld(t, env)

	env.applySlackJob(t, jobs.SlackInteractionArgs{
		ActionID:    "oto.ack",
		Value:       w.betaGroup.String(), // ⛔ another tenant's group
		TeamID:      slackTeam,
		ChannelID:   w.alphaChannel, // ⛔ resolved to alpha
		SlackUserID: "U0123456789",
	})

	if n := env.ackedOccurrences(t, w.betaOrg); n != 0 {
		t.Fatalf("%d of org beta's occurrences were acknowledged from org alpha's Slack channel. "+
			"An interaction from one workspace must never reach another tenant's alerts", n)
	}
	if n := env.ackedOccurrences(t, w.alphaOrg); n != 0 {
		t.Fatalf("%d of org alpha's occurrences were acknowledged by a press naming beta's group", n)
	}
}

// TestSlackAcknowledgeAttributesAnUnlinkedMemberHonestly.
//
// ⛔ A Slack member with no oto account STILL ACKS. Refusing would silently lose
// every acknowledgement from anybody who has not linked an account, which is a
// worse failure than the one being fixed. What is recorded is the truth about
// what happened: `actor_kind = 'slack'`, the Slack member id, the handle they
// had at press time, and `acked_by` NULL because there is no oto user to name.
func TestSlackAcknowledgeAttributesAnUnlinkedMemberHonestly(t *testing.T) {
	env := newEnvWith(t, func(c *config.Config) {
		c.Slack.Enabled = true
		c.Slack.Mode = "http"
		c.Slack.SigningSecret = ackSigningSecret
	})
	w := seedSlackWorld(t, env)

	env.applySlackJob(t, jobs.SlackInteractionArgs{
		ActionID:      "oto.ack",
		Value:         w.alphaGroup.String(),
		TeamID:        slackTeam,
		ChannelID:     w.alphaChannel,
		SlackUserID:   "U0000NOBODY", // never linked, never seen before
		SlackUserName: "newcomer",
	})

	ackState, ackedBy, label := env.occurrenceAck(t, w.alphaOcc)
	if ackState != "acked" {
		t.Fatalf("an unlinked Slack member's press left the occurrence %q; the acknowledgement was lost", ackState)
	}
	if ackedBy != nil {
		t.Fatalf("acked_by = %v for an unlinked member; oto invented an oto user", *ackedBy)
	}
	if label != "@newcomer" {
		t.Fatalf("acked_by_label = %q, want @newcomer: the display name is what makes the timeline readable", label)
	}

	kind, actorLabel := env.ackEvent(t, w.alphaAlert)
	if kind != "slack" {
		t.Fatalf("actor_kind = %q, want slack for a member with no oto account", kind)
	}
	if actorLabel != "@newcomer" {
		t.Fatalf("the timeline records %q, want @newcomer", actorLabel)
	}

	// ⭐ The sighting is RECORDED, which is what `slack_identities` is for: the
	// row is what a settings screen later offers to link, so an install where
	// nobody has linked anything still accumulates the identities that make
	// linking a one-click job.
	if !env.slackIdentityExists(t, w.alphaOrg, slackTeam, "U0000NOBODY") {
		t.Fatal("the press recorded no slack_identities row; linking this member later " +
			"would mean typing their member id by hand")
	}
}

// TestSlackAcknowledgeIsIdempotent covers the double-click and the replay.
//
// Both converge on the same row, because the idempotency lives in the DOMAIN —
// `Occurrence.Acknowledge` refuses an already-acked episode — rather than in the
// transport. A second press does not move `acked_at`, does not re-attribute the
// receipt, and does not append a second event.
func TestSlackAcknowledgeIsIdempotent(t *testing.T) {
	env := newEnvWith(t, func(c *config.Config) {
		c.Slack.Enabled = true
		c.Slack.Mode = "http"
		c.Slack.SigningSecret = ackSigningSecret
	})
	w := seedSlackWorld(t, env)

	press := jobs.SlackInteractionArgs{
		ActionID:      "oto.ack",
		Value:         w.alphaGroup.String(),
		TeamID:        slackTeam,
		ChannelID:     w.alphaChannel,
		SlackUserID:   "U0123456789",
		SlackUserName: "ram",
	}
	env.applySlackJob(t, press)
	_, _, firstLabel := env.occurrenceAck(t, w.alphaOcc)
	firstAt := env.occurrenceAckedAt(t, w.alphaOcc)

	// The same press again — a double-click, a Slack redelivery, or a job retry.
	env.applySlackJob(t, press)
	// And somebody ELSE, arriving second.
	second := press
	second.SlackUserID = "U0000OTHER"
	second.SlackUserName = "other"
	env.applySlackJob(t, second)

	_, _, label := env.occurrenceAck(t, w.alphaOcc)
	if label != firstLabel {
		t.Fatalf("a second press re-attributed the acknowledgement from %q to %q; "+
			"the first human to see it is the fact on the record", firstLabel, label)
	}
	if at := env.occurrenceAckedAt(t, w.alphaOcc); !at.Equal(firstAt) {
		t.Fatalf("a second press moved acked_at from %s to %s", firstAt, at)
	}
	if n := env.countAckEvents(t, w.alphaAlert); n != 1 {
		t.Fatalf("the timeline carries %d occurrence.acknowledged events after three presses, want 1", n)
	}
}

// TestSlackInteractionSignatureIsEnforcedByTheRealEndpoint.
//
// The unit tests prove the algorithm; this proves it is actually MOUNTED, in
// front of the real consumer, with nothing in the middleware chain reading the
// body first. A forged press must enqueue nothing at all.
func TestSlackInteractionSignatureIsEnforcedByTheRealEndpoint(t *testing.T) {
	env := newEnvWith(t, func(c *config.Config) {
		c.Slack.Enabled = true
		c.Slack.Mode = "http"
		c.Slack.SigningSecret = ackSigningSecret
	})
	w := seedSlackWorld(t, env)

	base := slackPress{
		team: slackTeam, channel: w.alphaChannel, user: "U0123456789", userName: "ram",
		actionID: "oto.ack", value: w.alphaGroup.String(),
		signedAt: time.Now(), secret: ackSigningSecret,
	}

	forged := base
	forged.secret = "the-wrong-secret"
	if s := env.pressSlackButton(t, forged); s != http.StatusUnauthorized {
		t.Fatalf("a press signed with the wrong secret answered %d, want 401", s)
	}

	stale := base
	stale.signedAt = time.Now().Add(-10 * time.Minute)
	if s := env.pressSlackButton(t, stale); s != http.StatusUnauthorized {
		t.Fatalf("a press signed ten minutes ago answered %d, want 401", s)
	}

	tampered := base
	tampered.tamper = true
	if s := env.pressSlackButton(t, tampered); s != http.StatusUnauthorized {
		t.Fatalf("a press whose body was edited after signing answered %d, want 401", s)
	}

	if n := env.countJobs(t, jobs.KindSlackInteraction); n != 0 {
		t.Fatalf("%d jobs were enqueued by requests that failed verification", n)
	}

	// The genuine article still works, which is what proves the three refusals
	// above were about the signature and not about the harness.
	if s := env.pressSlackButton(t, base); s != http.StatusOK {
		t.Fatalf("a correctly signed press answered %d, want 200", s)
	}
	if n := env.countJobs(t, jobs.KindSlackInteraction); n != 1 {
		t.Fatalf("a correctly signed press enqueued %d jobs, want 1", n)
	}
}

// ------------------------------------------------------------------ harness

type slackPress struct {
	team        string
	channel     string
	user        string
	userName    string
	actionID    string
	value       string
	responseURL string
	signedAt    time.Time
	secret      string
	// tamper edits the body AFTER it is signed.
	tamper bool
}

// pressSlackButton posts one signed interaction at the real endpoint.
func (e *env) pressSlackButton(t *testing.T, p slackPress) int {
	t.Helper()

	payload := fmt.Sprintf(`{"type":"block_actions",`+
		`"team":{"id":%q},"user":{"id":%q,"username":%q},"channel":{"id":%q},`+
		`"container":{"message_ts":"1712345678.000100"},"response_url":%q,`+
		`"actions":[{"action_id":%q,"type":"button","value":%q}]}`,
		p.team, p.user, p.userName, p.channel, p.responseURL, p.actionID, p.value)

	body := url.Values{"payload": {payload}}.Encode()
	stamp := strconv.FormatInt(p.signedAt.Unix(), 10)

	mac := hmac.New(sha256.New, []byte(p.secret))
	mac.Write([]byte("v0:" + stamp + ":" + body))
	signature := "v0=" + hex.EncodeToString(mac.Sum(nil))

	if p.tamper {
		// One byte of the SIGNED body, changed after the fact. This is the shape
		// of an attack that swaps a button's value inside an authentic envelope.
		body = strings.Replace(body, "block_actions", "block_actionX", 1)
	}

	req, err := http.NewRequestWithContext(e.ctx, http.MethodPost,
		e.server.URL+"/api/v1/integrations/slack/interactions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", signature)

	resp, err := e.server.Client().Do(req)
	if err != nil {
		t.Fatalf("post interaction: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	// The consumer runs after the response is flushed, so the enqueue may land a
	// moment after the status does.
	e.waitForJob(t, jobs.KindSlackInteraction)
	return resp.StatusCode
}

// waitForJob gives the post-response dispatch a bounded moment to land.
func (e *env) waitForJob(t *testing.T, kind string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e.countJobs(t, kind) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// applySlackJob runs the worker body directly.
func (e *env) applySlackJob(t *testing.T, args jobs.SlackInteractionArgs) {
	t.Helper()
	if e.container.SlackInteractions == nil {
		t.Fatal("the container built no Slack interaction consumer: the Acknowledge button " +
			"is wired to nothing, which is the defect itself")
	}
	if err := e.container.SlackInteractions.Apply(e.ctx, args); err != nil {
		t.Fatalf("apply slack interaction: %v", err)
	}
}

func (e *env) countJobs(t *testing.T, kind string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM river_job WHERE kind = $1`, kind).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}

func (e *env) countNotifyEvaluate(t *testing.T, reason string) int {
	t.Helper()
	var n int
	err := e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM river_job WHERE kind = $1 AND args->>'reason' = $2`,
		jobs.KindNotifyEvaluate, reason).Scan(&n)
	if err != nil {
		t.Fatalf("count notify jobs: %v", err)
	}
	return n
}

func (e *env) occurrenceAck(t *testing.T, occurrenceID uuid.UUID) (state string, by *uuid.UUID, label string) {
	t.Helper()
	var lbl *string
	err := e.pool.QueryRow(e.ctx,
		`SELECT ack_state, acked_by, acked_by_label FROM alert_occurrences WHERE id = $1`,
		occurrenceID).Scan(&state, &by, &lbl)
	if err != nil {
		t.Fatalf("read occurrence ack: %v", err)
	}
	if lbl != nil {
		label = *lbl
	}
	return state, by, label
}

func (e *env) occurrenceAckedAt(t *testing.T, occurrenceID uuid.UUID) time.Time {
	t.Helper()
	var at *time.Time
	if err := e.pool.QueryRow(e.ctx,
		`SELECT acked_at FROM alert_occurrences WHERE id = $1`, occurrenceID).Scan(&at); err != nil {
		t.Fatalf("read acked_at: %v", err)
	}
	if at == nil {
		t.Fatal("acked_at is NULL")
	}
	return *at
}

func (e *env) alertAckState(t *testing.T, alertID uuid.UUID) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx,
		`SELECT ack_state FROM alerts WHERE id = $1`, alertID).Scan(&s); err != nil {
		t.Fatalf("read alert ack_state: %v", err)
	}
	return s
}

func (e *env) ackEvent(t *testing.T, alertID uuid.UUID) (actorKind, actorLabel string) {
	t.Helper()
	var lbl *string
	err := e.pool.QueryRow(e.ctx,
		`SELECT actor_kind, actor_label FROM alert_events
		  WHERE alert_id = $1 AND type = 'occurrence.acknowledged'
		  ORDER BY recorded_at DESC LIMIT 1`, alertID).Scan(&actorKind, &lbl)
	if err != nil {
		t.Fatalf("read ack event (the timeline has no acknowledgement on it): %v", err)
	}
	if lbl != nil {
		actorLabel = *lbl
	}
	return actorKind, actorLabel
}

func (e *env) countAckEvents(t *testing.T, alertID uuid.UUID) int {
	t.Helper()
	var n int
	err := e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM alert_events WHERE alert_id = $1 AND type = 'occurrence.acknowledged'`,
		alertID).Scan(&n)
	if err != nil {
		t.Fatalf("count ack events: %v", err)
	}
	return n
}

func (e *env) ackedOccurrences(t *testing.T, orgID uuid.UUID) int {
	t.Helper()
	var n int
	err := e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM alert_occurrences WHERE org_id = $1 AND ack_state = 'acked'`,
		orgID).Scan(&n)
	if err != nil {
		t.Fatalf("count acked occurrences: %v", err)
	}
	return n
}

func (e *env) slackIdentityExists(t *testing.T, orgID uuid.UUID, team, member string) bool {
	t.Helper()
	var n int
	err := e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM slack_identities
		  WHERE org_id = $1 AND team_id = $2 AND slack_user_id = $3`,
		orgID, team, member).Scan(&n)
	if err != nil {
		t.Fatalf("read slack identity: %v", err)
	}
	return n > 0
}

// seedSlackWorld writes two tenants that share one Slack workspace.
//
// ⭐ THE TWO-TENANT SHAPE IS THE POINT. One workspace connected to two oto orgs
// is representable, and it is the configuration in which a cross-tenant hole
// would actually be reachable. A single-org fixture would pass whether or not
// the tenancy check existed.
func seedSlackWorld(t *testing.T, e *env) slackWorld {
	t.Helper()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", sql[:min(70, len(sql))], err)
		}
	}

	now := time.Now().UTC().Add(-time.Hour)
	w := slackWorld{
		alphaChannel: "C000000ALPHA",
		betaChannel:  "C0000000BETA",
		linkedEmail:  "ram@alpha.example",
	}

	boot, err := app.Bootstrap(e.ctx, e.pool, app.BootstrapRequest{
		OrgSlug: "alpha", OrgName: "Alpha", Email: w.linkedEmail,
		DisplayName: "Ram", Password: "correct-horse-battery-staple", TokenName: "bootstrap",
	}, time.Now())
	if err != nil {
		t.Fatalf("bootstrap alpha: %v", err)
	}
	w.alphaOrg, w.linkedUser = boot.OrgID, boot.UserID

	// ⛔ THE SECOND TENANT IS WRITTEN DIRECTLY. `Bootstrap` refuses to run twice —
	// it is the first-org command, not a factory — and a second tenant is exactly
	// what makes the cross-tenant case reachable, so it is inserted as a row.
	w.betaOrg = id.New()
	exec(`INSERT INTO orgs (id, slug, name) VALUES ($1,'beta','Beta')`, w.betaOrg)

	// The LINK. `ram` in the Slack workspace is `ram@alpha.example` in oto —
	// but only inside org alpha.
	exec(`INSERT INTO slack_identities (id, org_id, team_id, slack_user_id, slack_handle, user_id, linked_at)
	      VALUES ($1,$2,$3,'U0123456789','ram',$4,$5)`,
		id.New(), w.alphaOrg, slackTeam, w.linkedUser, now)

	// `alerts_key_ck` and `groups_key_ck` are `^(ak|gk)_[0-9a-v]{26}$` — a
	// Crockford-base32 digest, 26 characters, no letters past `v`. The seed
	// satisfies the real constraint rather than relaxing it.
	seedOrg := func(orgID uuid.UUID, slug, keySuffix, conversation string) (groupID, alertID, occID uuid.UUID) {
		clusterID, sourceID, credID := id.New(), id.New(), id.New()
		groupID, alertID, occID = id.New(), id.New(), id.New()

		exec(`INSERT INTO clusters (id, org_id, cluster_key, display_name) VALUES ($1,$2,'prod','prod')`,
			clusterID, orgID)
		exec(`INSERT INTO alert_sources (id, org_id, cluster_id, name, kind, base_url)
		      VALUES ($1,$2,$3,'am','alertmanager','http://am.test')`, sourceID, orgID, clusterID)

		exec(`INSERT INTO alerts (id, org_id, cluster_id, alert_key, source_fingerprint, alertname,
		         severity, cluster_key, labels, state,
		         first_seen_at, last_seen_at, last_state_change_at, total_occurrences)
		      VALUES ($1,$2,$3,$4,'3f8c1a2b9d4e5f60','HighErrorRate',
		         'critical','prod','{"alertname":"HighErrorRate"}'::jsonb,'firing',$5,$5,$5,1)`,
			alertID, orgID, clusterID, "ak_"+keySuffix, now)

		exec(`INSERT INTO alert_groups (id, org_id, source_id, cluster_id, group_key, title, state,
		         state_version, total_count, firing_count, first_seen_at, last_activity_at)
		      VALUES ($1,$2,$3,$4,$5,'HighErrorRate · prod','open',1,1,1,$6,$6)`,
			groupID, orgID, sourceID, clusterID, "gk_"+keySuffix, now)

		exec(`INSERT INTO alert_occurrences (id, org_id, alert_id, group_id, seq, state,
		         started_at, last_observed_at, source_starts_at)
		      VALUES ($1,$2,$3,$4,1,'firing',$5,$5,$5)`, occID, orgID, alertID, groupID, now)

		exec(`UPDATE alerts SET current_occurrence_id = $2 WHERE id = $1`, alertID, occID)
		exec(`INSERT INTO alert_group_members (group_id, occurrence_id, org_id, alert_id, joined_at)
		      VALUES ($1,$2,$3,$4,$5)`, groupID, occID, orgID, alertID, now)

		// The Slack destination. `channels_cred_ck` requires a credential on a
		// slack channel, and the sealed blob has a 29-byte floor; the seed
		// satisfies the real constraint rather than relaxing it.
		exec(`INSERT INTO channel_credentials (id, org_id, kind, sealed, key_version)
		      VALUES ($1,$2,'slack_bot_token', decode(repeat('00', 32), 'hex'), 1)`, credID, orgID)
		// `created_at` and `updated_at` are NAMED: 00032 removed the database
		// default so that no row on this table can take the database's clock while
		// its `updated_at` writers take the application's.
		exec(`INSERT INTO channels (id, org_id, type, name, config, credential_id,
		         created_at, updated_at)
		      VALUES ($1,$2,'slack',$3,$4::jsonb,$5,$6,$6)`,
			id.New(), orgID, slug+"-alerts",
			fmt.Sprintf(`{"team_id":%q,"conversation_id":%q,"conversation_name":"%s-alerts"}`,
				slackTeam, conversation, slug),
			credID, now)

		return groupID, alertID, occID
	}

	w.alphaGroup, w.alphaAlert, w.alphaOcc = seedOrg(w.alphaOrg, "alpha", "0a0123456789abcdefghijklmn", w.alphaChannel)
	w.betaGroup, _, _ = seedOrg(w.betaOrg, "beta", "0b0123456789abcdefghijklmn", w.betaChannel)
	return w
}
