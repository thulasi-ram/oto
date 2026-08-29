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
// same workspace, and one firing alert with one open Case apiece.
//
// ⛔ `alphaGroup` AND `betaGroup` WERE FIELDS HERE AND ARE DELETED (git-bug
// `7570090`). A CARD'S BUTTON CARRIES A CASE ID: `alert_groups` is dropped, the
// conversation IS the Case, and `interactions.go` parses `args.Value` as a
// `alert_cases.id` and hands it to `CaseExists`/`AcknowledgeCase`. A press
// carrying a generation id would now fail to resolve, which is a DIFFERENT
// failure from the no-op this file exists to prevent — and would pass the
// cross-tenant test below for the wrong reason.
type slackWorld struct {
	alphaOrg     uuid.UUID
	alphaAlert   uuid.UUID
	alphaCase    uuid.UUID
	alphaChannel string // Slack conversation id

	betaOrg     uuid.UUID
	betaCase    uuid.UUID
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
		value:       w.alphaCase.String(),
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
		Value:         w.alphaCase.String(),
		TeamID:        slackTeam,
		ChannelID:     w.alphaChannel,
		SlackUserID:   "U0123456789",
		SlackUserName: "ram",
	})

	// ---- 4. THE RECORD -------------------------------------------------
	ackState, ackedBy, ackedLabel := env.caseAck(t, w.alphaCase)
	if ackState != "acked" {
		t.Fatalf("the case is %q after an Acknowledge press, want acked. "+
			"The button is still a no-op that looks like it worked", ackState)
	}
	// The member is LINKED, so the receipt names the oto user.
	if ackedBy == nil || *ackedBy != w.linkedUser {
		t.Fatalf("acked_by = %v, want the linked oto user %s", ackedBy, w.linkedUser)
	}
	if ackedLabel != w.linkedEmail {
		t.Fatalf("acked_by_label = %q, want %q", ackedLabel, w.linkedEmail)
	}

	// ⛔ AND THE ALERT ROW DID NOT MOVE, BECAUSE IT HAS NOWHERE TO MOVE TO.
	// `alerts` carries no ack column: a receipt for one firing must not outlive
	// that firing. What every list and every card renders from is this alert's
	// CURRENT EPISODE, which is the row asserted above.
	if col := env.alertsHasColumn(t, "ack_state"); col {
		t.Fatal("`alerts` has an ack_state column again. An ack is a statement about " +
			"ONE firing episode; projected onto the Alert it keeps asserting itself " +
			"after that episode has closed, which is how a firing months later arrives " +
			"pre-acknowledged and never reaches anybody.")
	}
	if got := env.currentCaseAckState(t, w.alphaAlert); got != "acked" {
		t.Fatalf("the alert's current episode is %q, want acked", got)
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
// ALPHA's channel names BETA's case id inside an otherwise perfectly authentic,
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
		Value:       w.betaCase.String(), // ⛔ another tenant's Case
		TeamID:      slackTeam,
		ChannelID:   w.alphaChannel, // ⛔ resolved to alpha
		SlackUserID: "U0123456789",
	})

	if n := env.ackedCases(t, w.betaOrg); n != 0 {
		t.Fatalf("%d of org beta's cases were acknowledged from org alpha's Slack channel. "+
			"An interaction from one workspace must never reach another tenant's alerts", n)
	}
	if n := env.ackedCases(t, w.alphaOrg); n != 0 {
		t.Fatalf("%d of org alpha's cases were acknowledged by a press naming beta's case", n)
	}
}

// TestSlackAcknowledgeAttributesAnUnlinkedMemberToItsShadow.
//
// ⛔ A Slack member with no oto account STILL ACKS. Refusing would silently lose
// every acknowledgement from anybody who has not linked an account, which is a
// worse failure than the one being fixed. That half has never changed.
//
// ⚠️ WHAT CHANGED IS WHO IS NAMED, AND IT REVERSES WHAT THIS TEST USED TO ASSERT
// (owner ruling 2026-08-20, git-bug `a74d6b2`, migration `00074`). It read:
// "`acked_by` NULL because there is no oto user to name", and it failed the build
// if oto "invented an oto user". oto now DOES mint one — a SHADOW member, on first
// press — because `idempotency_claims.principal_id` is NOT NULL, so without a
// principal a Slack redelivery took no claim and wrote the press twice.
//
// The honesty this test is named for is therefore relocated, not dropped: the
// shadow carries NO email (oto never reads Slack back, C9, so it cannot know one
// and does not fabricate one), cannot log in, and `acked_by_label` is still the
// handle the member had at press time — which is what makes the timeline readable
// and is asserted below unchanged.
//
// ⭐ NOTE FOR ANYONE RESTORING THE OLD ASSERTION: attribution never needed the row.
// `actor_kind = 'slack'` plus the member id and handle already told the truth, which
// is why the previous stance was defensible. The row exists for the CLAIM. If a
// future change gives Slack presses a principal without a `users` row, this test
// should go back to asserting NULL.
func TestSlackAcknowledgeAttributesAnUnlinkedMemberToItsShadow(t *testing.T) {
	env := newEnvWith(t, func(c *config.Config) {
		c.Slack.Enabled = true
		c.Slack.Mode = "http"
		c.Slack.SigningSecret = ackSigningSecret
	})
	w := seedSlackWorld(t, env)

	env.applySlackJob(t, jobs.SlackInteractionArgs{
		ActionID:      "oto.ack",
		Value:         w.alphaCase.String(),
		TeamID:        slackTeam,
		ChannelID:     w.alphaChannel,
		SlackUserID:   "U0000NOBODY", // never linked, never seen before
		SlackUserName: "newcomer",
	})

	ackState, ackedBy, label := env.caseAck(t, w.alphaCase)
	if ackState != "acked" {
		t.Fatalf("an unlinked Slack member's press left the case %q; the acknowledgement was lost", ackState)
	}
	if ackedBy == nil {
		t.Fatal("acked_by is NULL for an unlinked member. Since migration 00074 the press " +
			"mints a shadow member and names it, because a claim needs a principal and " +
			"`idempotency_claims.principal_id` is NOT NULL — a NULL here means the shadow " +
			"was not created, so a Slack redelivery of this press would write the ack twice")
	}
	if email, canLogin := env.userEmailAndLogin(t, *ackedBy); email != nil || canLogin {
		t.Fatalf("the shadow named by acked_by has email=%v canLogin=%v; want a NULL email and "+
			"no login. oto never reads Slack back (C9), so it cannot know an address, and by "+
			"ruling it holds none rather than a fabricated one", email, canLogin)
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
// `Case.Acknowledge` refuses an already-acked episode — rather than in the
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
		Value:         w.alphaCase.String(),
		TeamID:        slackTeam,
		ChannelID:     w.alphaChannel,
		SlackUserID:   "U0123456789",
		SlackUserName: "ram",
	}
	env.applySlackJob(t, press)
	_, _, firstLabel := env.caseAck(t, w.alphaCase)
	firstAt := env.caseAckedAt(t, w.alphaCase)

	// The same press again — a double-click, a Slack redelivery, or a job retry.
	env.applySlackJob(t, press)
	// And somebody ELSE, arriving second.
	second := press
	second.SlackUserID = "U0000OTHER"
	second.SlackUserName = "other"
	env.applySlackJob(t, second)

	_, _, label := env.caseAck(t, w.alphaCase)
	if label != firstLabel {
		t.Fatalf("a second press re-attributed the acknowledgement from %q to %q; "+
			"the first human to see it is the fact on the record", firstLabel, label)
	}
	if at := env.caseAckedAt(t, w.alphaCase); !at.Equal(firstAt) {
		t.Fatalf("a second press moved acked_at from %s to %s", firstAt, at)
	}
	if n := env.countAckEvents(t, w.alphaAlert); n != 1 {
		t.Fatalf("the timeline carries %d case.acknowledged events after three presses, want 1", n)
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
		actionID: "oto.ack", value: w.alphaCase.String(),
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

func (e *env) caseAck(t *testing.T, caseID uuid.UUID) (state string, by *uuid.UUID, label string) {
	t.Helper()
	var lbl *string
	err := e.pool.QueryRow(e.ctx,
		`SELECT ack_state, acked_by, acked_by_label FROM alert_cases WHERE id = $1`,
		caseID).Scan(&state, &by, &lbl)
	if err != nil {
		t.Fatalf("read case ack: %v", err)
	}
	if lbl != nil {
		label = *lbl
	}
	return state, by, label
}

// userEmailAndLogin reads the two facts that make a shadow member a shadow: it has
// no address, and it cannot be logged into. Read from the DB rather than through the
// domain so the test pins what is STORED — a shadow whose email column held a
// synthetic address would satisfy any Go-level `IsShadow()` that keyed off something
// else, and the ruling this guards is specifically about the column.
func (e *env) userEmailAndLogin(t *testing.T, userID uuid.UUID) (email *string, canLogin bool) {
	t.Helper()
	var hash *string
	err := e.pool.QueryRow(e.ctx,
		`SELECT email, password_hash FROM users WHERE id = $1`,
		userID).Scan(&email, &hash)
	if err != nil {
		t.Fatalf("read user %s: %v", userID, err)
	}
	return email, hash != nil
}

func (e *env) caseAckedAt(t *testing.T, caseID uuid.UUID) time.Time {
	t.Helper()
	var at *time.Time
	if err := e.pool.QueryRow(e.ctx,
		`SELECT acked_at FROM alert_cases WHERE id = $1`, caseID).Scan(&at); err != nil {
		t.Fatalf("read acked_at: %v", err)
	}
	if at == nil {
		t.Fatal("acked_at is NULL")
	}
	return *at
}

// currentCaseAckState is what a list row and a card actually read: the
// ack of the episode the Alert is having, reached through `current_case_id`.
func (e *env) currentCaseAckState(t *testing.T, alertID uuid.UUID) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, `
		SELECT o.ack_state
		  FROM alerts a
		  JOIN alert_cases o ON o.id = a.current_case_id
		 WHERE a.id = $1`, alertID).Scan(&s); err != nil {
		t.Fatalf("read the current episode's ack_state: %v", err)
	}
	return s
}

// alertsHasColumn asks the LIVE schema, not the migration files. A column that
// came back by any route — a hotfix ALTER, an un-deployed contract half, a
// rolled-back Down — has rows, and the rows are what a migration cannot take back.
func (e *env) alertsHasColumn(t *testing.T, name string) bool {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'alerts' AND column_name = $1`,
		name).Scan(&n); err != nil {
		t.Fatalf("introspect alerts.%s: %v", name, err)
	}
	return n > 0
}

func (e *env) ackEvent(t *testing.T, alertID uuid.UUID) (actorKind, actorLabel string) {
	t.Helper()
	var lbl *string
	err := e.pool.QueryRow(e.ctx,
		`SELECT actor_kind, actor_label FROM alert_events
		  WHERE alert_id = $1 AND type = 'case.acknowledged'
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
		`SELECT count(*) FROM alert_events WHERE alert_id = $1 AND type = 'case.acknowledged'`,
		alertID).Scan(&n)
	if err != nil {
		t.Fatalf("count ack events: %v", err)
	}
	return n
}

func (e *env) ackedCases(t *testing.T, orgID uuid.UUID) int {
	t.Helper()
	var n int
	err := e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM alert_cases WHERE org_id = $1 AND ack_state = 'acked'`,
		orgID).Scan(&n)
	if err != nil {
		t.Fatalf("count acked cases: %v", err)
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
	// `created_at`/`updated_at` are NAMED: 00033 removed the database default so
	// that no `orgs` row can take the database's clock while `UpdateSettings`
	// takes the application's.
	exec(`INSERT INTO orgs (id, slug, name, created_at, updated_at)
	      VALUES ($1,'beta','Beta',$2,$2)`, w.betaOrg, now)

	// The LINK. `ram` in the Slack workspace is `ram@alpha.example` in oto —
	// but only inside org alpha.
	// `created_at` is NAMED: 00034 removed this table's DEFAULT now(), so the first
	// sighting and the link are both stamped by the application.
	exec(`INSERT INTO slack_identities (id, org_id, team_id, slack_user_id, slack_handle,
	         user_id, linked_at, created_at)
	      VALUES ($1,$2,$3,'U0123456789','ram',$4,$5,$5)`,
		id.New(), w.alphaOrg, slackTeam, w.linkedUser, now)

	// `alerts_key_ck` is `^ak_[0-9a-v]{26}$` — a Crockford-base32 digest, 26
	// characters, no letters past `v`. The seed satisfies the real constraint
	// rather than relaxing it. (`groups_key_ck` was the other half of this
	// sentence and went with `alert_groups`, git-bug `7570090`.)
	seedOrg := func(orgID uuid.UUID, slug, keySuffix, conversation string) (alertID, caseID uuid.UUID) {
		clusterID, sourceID, credID := id.New(), id.New(), id.New()
		alertID, caseID = id.New(), id.New()

		// `created_at`/`updated_at` are NAMED on both: 00034 removed their DEFAULT
		// now() so that nothing on these tables can take the database's clock while
		// their `updated_at` writers take the application's.
		exec(`INSERT INTO clusters (id, org_id, cluster_key, display_name, created_at, updated_at)
		      VALUES ($1,$2,'prod','prod',$3,$3)`, clusterID, orgID, now)
		exec(`INSERT INTO alert_sources (id, org_id, cluster_id, name, kind, base_url,
		         created_at, updated_at)
		      VALUES ($1,$2,$3,'am','alertmanager','http://am.test',$4,$4)`,
			sourceID, orgID, clusterID, now)

		exec(`INSERT INTO alerts (id, org_id, cluster_id, alert_key, source_fingerprint, alertname,
		         severity, cluster_key, labels, state,
		         first_seen_at, last_seen_at, last_state_change_at, total_cases)
		      VALUES ($1,$2,$3,$4,'3f8c1a2b9d4e5f60','HighErrorRate',
		         'critical','prod','{"alertname":"HighErrorRate"}'::jsonb,'firing',$5,$5,$5,1)`,
			alertID, orgID, clusterID, "ak_"+keySuffix, now)

		// ⛔ AN `alert_groups` GENERATION WAS SEEDED HERE AND IS DELETED (git-bug
		// `7570090`). The open Case below is what the card is about and what its
		// button carries; there is no generation to open and no membership to record.
		// `number` comes off the org's counter (00081), never a literal: the case
		// this seeds shares an org with every case the code under test opens, and
		// two of them naming 1 collide on `case_number_uniq`.
		exec(`WITH allocated AS (
		        INSERT INTO org_case_numbers (org_id, next_number) VALUES ($2, 2)
		        ON CONFLICT (org_id) DO UPDATE
		                SET next_number = org_case_numbers.next_number + 1
		          RETURNING next_number - 1 AS number
		      )
		      INSERT INTO alert_cases (id, org_id, alert_id, seq, number, state,
		         started_at, last_observed_at, source_starts_at)
		      SELECT $1,$2,$3,1,(SELECT number FROM allocated),'open',$4,$4,$4`,
			caseID, orgID, alertID, now)

		exec(`UPDATE alerts SET current_case_id = $2 WHERE id = $1`, alertID, caseID)

		// The Slack workspace's shared setup. `channel_connections_cred_ck` requires
		// a credential on a slack CONNECTION — that constraint moved off `channels`
		// in 00075 — and the sealed blob has a 29-byte floor; the seed satisfies the
		// real constraint rather than relaxing it.
		// `created_at` is NAMED for the reason 00033 gives: this table's clock is
		// the application's, and `rotated_at` is compared against this value.
		exec(`INSERT INTO channel_credentials (id, org_id, kind, sealed, key_version, created_at)
		      VALUES ($1,$2,'slack_bot_token', decode(repeat('00', 32), 'hex'), 1, $3)`,
			credID, orgID, now)
		// ⛔ `team_id` MOVED HERE AND IS NOT ON THE CHANNEL ANY MORE (ADR 0047). The
		// workspace is a property of the connection every destination under it shares;
		// `schema.json` is `additionalProperties: false`, so leaving it on the channel
		// config below would be a `config_invalid` refusal rather than a stale field.
		connID := id.New()
		exec(`INSERT INTO channel_connections (id, org_id, type, name, config, credential_id,
		         created_at, updated_at)
		      VALUES ($1,$2,'slack',$3,$4::jsonb,$5,$6,$6)`,
			connID, orgID, slug+"-workspace",
			fmt.Sprintf(`{"team_id":%q}`, slackTeam), credID, now)
		// `created_at` and `updated_at` are NAMED: 00032 removed the database
		// default so that no row on this table can take the database's clock while
		// its `updated_at` writers take the application's.
		exec(`INSERT INTO channels (id, org_id, type, name, config, connection_id,
		         created_at, updated_at)
		      VALUES ($1,$2,'slack',$3,$4::jsonb,$5,$6,$6)`,
			id.New(), orgID, slug+"-alerts",
			fmt.Sprintf(`{"conversation_id":%q,"conversation_name":"%s-alerts"}`,
				conversation, slug),
			connID, now)

		return alertID, caseID
	}

	w.alphaAlert, w.alphaCase = seedOrg(w.alphaOrg, "alpha", "0a0123456789abcdefghijklmn", w.alphaChannel)
	_, w.betaCase = seedOrg(w.betaOrg, "beta", "0b0123456789abcdefghijklmn", w.betaChannel)
	return w
}
