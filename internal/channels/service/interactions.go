package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
	"github.com/thulasiram/oto/internal/platform/log"
)

// THE ACTION IDS OTO'S CARDS CARRY (§H.8, S9).
//
// They are a DURABLE CONTRACT with messages already sitting in Slack. A card
// posted last month still has last month's `action_id` on its button, and a
// rename here turns every one of those buttons into the silent no-op this whole
// file exists to abolish. Add ids; never rewrite one.
const (
	// ActionAcknowledge records that a human has SEEN the alerts in this group.
	//
	// ⛔ IT IS A RECEIPT AND NOTHING ELSE (§E.1.1). It does not change state, it
	// does not claim the alert, it does not say who will look at it, and no copy
	// anywhere in this file may suggest otherwise. oto has no axis on which an
	// alert belongs to a person, and the day this button implies one is the day
	// oto has become the product it exists not to be.
	ActionAcknowledge = "oto.ack"
	// ActionUnacknowledge drops an acknowledgement. The card renders it, and oto
	// answers it honestly — see applyUnacknowledge.
	ActionUnacknowledge = "oto.unack"
	// ActionNoopPrefix marks the URL buttons. Slack delivers an interaction for
	// every one of them and oto must acknowledge it; there is nothing to do
	// beyond that, and the explicit namespace is what says so out loud.
	ActionNoopPrefix = "oto.noop."
)

// interactionEnqueueTimeout bounds the ONE database write the HTTP path makes.
//
// ⛔ IT IS A SLACK CONSTRAINT, NOT A TASTE. Slack shows the user "This app is not
// responding" if the endpoint has not answered in three seconds. The transport
// answers 200 BEFORE this runs, so the budget here is not what the user waits
// on — it is what stops a wedged Postgres from holding an HTTP worker open
// indefinitely after the response has already gone out.
const interactionEnqueueTimeout = 5 * time.Second

// ------------------------------------------------------------------- ports

// Every interface in this file is a PORT DECLARED BY THE CONSUMER (CONTEXT.md
// §5.4). Two of them reach UPSTREAM — `grouping` acks the alerts, `identity`
// names the human — and they are expressed entirely in primitives for that
// reason: `channels` is the last module in the dependency direction (§I.1) and
// must not learn either module's types. `internal/app` does the adapting, which
// is the one place allowed to know both.

// SlackConversations resolves the TENANT from a Slack workspace and conversation.
//
// ⛔⛔ THIS IS THE HIGHEST-STAKES CALL IN THE FILE. An interaction payload names
// a team and a channel and never an org. Everything downstream runs under the
// db.TenantScope this produces, so an org resolved from anything the USER
// controls — a button value, a message id — would be a cross-tenant hole with a
// friendly name. It is resolved from the destination oto's own operator
// configured, and from nothing else.
type SlackConversations interface {
	ResolveSlackConversation(ctx context.Context, teamID, conversationID string) (SlackDestination, error)
}

// SlackDestination is the tenant and channel one conversation belongs to.
type SlackDestination struct {
	OrgID     uuid.UUID
	ChannelID uuid.UUID
}

// SlackActors maps a Slack workspace member onto an oto actor, WITHIN one org.
//
// ⛔ IT IS ORG-SCOPED, and that is a correctness property rather than a tidiness
// one. A Slack member linked to an oto user in org A must not be attributed in
// org B: resolving across orgs would let a press in one tenant's channel be
// recorded against another tenant's user.
//
// An UNLINKED member is a SUCCESS. `SlackActor.UserID` is zero, the Slack handle
// carries the timeline label, and the acknowledgement is recorded. Refusing one
// would silently lose every acknowledgement from anybody who has not linked
// their account — which is worse than the defect this file fixes.
type SlackActors interface {
	SlackActor(ctx context.Context, s db.TenantScope, teamID, slackUserID, handle string) (SlackActor, error)
}

// SlackActor is who pressed the button, in oto's own terms.
type SlackActor struct {
	// UserID is the linked oto user, zero when the member has not linked.
	UserID uuid.UUID
	// Label is the immutable display name recorded on the timeline.
	Label string
}

// AlertGroups is the fan-out of a human verb over one group generation.
//
// ⛔ There is exactly ONE verb here and there is no second one coming from this
// surface. A chat message is not a place to resolve, close, silence or assign
// anything: it has no confirmation, no audit trail of oto's own and no undo.
// Acknowledge is a fact ABOUT the alert; every other verb would be a claim about
// a person.
type AlertGroups interface {
	// GroupExists reports whether one generation is visible in this tenant. It is
	// the tenancy check made explicit: a group id from another org answers false
	// rather than silently acking nothing.
	GroupExists(ctx context.Context, s db.TenantScope, groupID uuid.UUID) (bool, error)
	// AcknowledgeGroup acks every OPEN member episode of one generation.
	AcknowledgeGroup(ctx context.Context, s db.TenantScope, groupID uuid.UUID,
		actorKind, actorID, actorLabel string) (GroupAckResult, error)
}

// GroupAckResult is what one fan-out did.
type GroupAckResult struct {
	// Members is how many currently-joined members the verb was offered to.
	Members int
	// Applied is how many accepted it.
	Applied int
	// SkippedCodes counts the refusals by stable errs code, which is the only
	// thing that can tell "somebody already acked this" from "it resolved while
	// you were reading it".
	SkippedCodes map[string]int
	// Unreached is how many currently-joined members the fan-out never offered
	// the verb to, because a group ack is bounded and a storm can be larger than
	// the bound.
	//
	// ⭐ IT IS HERE SO THE BUTTON CANNOT LOOK LIKE IT WORKED. That is this
	// surface's whole standard — see applyUnacknowledge — and an ack that
	// silently covered a tenth of a five-thousand-alert group would fail it more
	// quietly than the bug that standard was written for.
	Unreached int
}

// SlackNotice sends a message only the person who pressed the button can see.
//
// It posts to the interaction's `response_url`, which is Slack's own one-shot
// reply channel: it needs no token and no scope, which is why it is the only
// outbound call oto can make in answer to a button. Empty URL means the caller
// gets no reply, and that is a degradation, never a failure.
type SlackNotice interface {
	Ephemeral(ctx context.Context, responseURL, text string) error
}

// ------------------------------------------------------------------ service

// InteractionOptions are the InteractionService's dependencies.
type InteractionOptions struct {
	Conversations SlackConversations
	Actors        SlackActors
	Groups        AlertGroups
	Enqueuer      db.Enqueuer
	Notice        SlackNotice
	// Metrics is optional. A nil one costs the `oto_slack_unknown_action_total`
	// series and nothing else, which is the right trade for a test that does not
	// want a registry.
	Metrics *InteractionMetrics
	Clock   clock.Clock
	Logger  *slog.Logger
}

// InteractionService consumes verified Slack block actions.
//
// ⭐ IT IS SPLIT IN TWO ON PURPOSE, and the split is the three-second rule.
// `Handle` runs on the HTTP request and does exactly one durable thing: it
// enqueues. `Apply` runs on the queue and does the work — resolve the tenant,
// name the human, acknowledge, and tell the user when the action cannot apply.
// Every database read an acknowledgement needs is on the second half, where no
// Slack timer is running.
type InteractionService struct {
	conversations SlackConversations
	actors        SlackActors
	groups        AlertGroups
	enqueuer      db.Enqueuer
	notice        SlackNotice
	metrics       *InteractionMetrics
	clk           clock.Clock
	log           *slog.Logger
}

// NewInteractionService builds the consumer.
func NewInteractionService(o InteractionOptions) (*InteractionService, error) {
	if o.Conversations == nil || o.Groups == nil || o.Enqueuer == nil {
		return nil, errs.New(errs.KindInternal, "slack_interactions_deps",
			"a conversation resolver, a group action port and an enqueuer are required")
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.New()
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &InteractionService{
		conversations: o.Conversations,
		actors:        o.Actors,
		groups:        o.Groups,
		enqueuer:      o.Enqueuer,
		notice:        o.Notice,
		metrics:       o.Metrics,
		clk:           clk,
		log:           logger,
	}, nil
}

// Handle takes one verified interaction off the HTTP request.
//
// ⛔ IT DOES NOT ACKNOWLEDGE ANYTHING, AND IT MUST NOT LEARN TO. Every database
// read the acknowledgement needs — the org, the group, its members, the actor —
// is deferred to `Apply`, because the sum of them is not reliably under three
// seconds on a busy night and Slack's failure banner is the thing this whole
// change exists to prevent.
//
// A payload it cannot act on is NOT an error the caller should retry: the
// transport has already answered 200 and Slack will not send it again. It is
// logged and swallowed, and the return value is reserved for the one failure
// that is genuinely oto's — the enqueue itself.
func (s *InteractionService) Handle(ctx context.Context, payload json.RawMessage) error {
	logger := log.From(ctx)

	env, err := parseSlackEnvelope(payload)
	switch {
	case errors.Is(err, errUnsupportedInteraction):
		logger.Info("channels: ignored a Slack interaction oto does not serve",
			slog.String("type", env.Type))
		return nil
	case err != nil:
		logger.Warn("channels: unreadable Slack interaction envelope",
			slog.String("error", err.Error()))
		return nil
	}

	if len(env.Actions) == 0 {
		logger.Warn("channels: a Slack block action carried no actions")
		return nil
	}

	// ⭐ THE DISPATCH SEAM. One switch, keyed on the action id, and every branch
	// says what it does — including the branches that deliberately do nothing.
	// A default that silently returns nil is how this endpoint became a no-op in
	// the first place, so the default LOGS.
	reqs := make([]db.JobRequest, 0, len(env.Actions))
	for _, a := range env.Actions {
		id := strings.TrimSpace(a.ActionID)
		switch {
		case id == ActionAcknowledge, id == ActionUnacknowledge:
			reqs = append(reqs, db.JobRequest{
				Args: jobs.SlackInteractionArgs{
					ActionID:      id,
					Value:         strings.TrimSpace(a.Value),
					TeamID:        env.Team.ID,
					ChannelID:     env.Channel.ID,
					SlackUserID:   env.User.ID,
					SlackUserName: env.User.handle(),
					MessageTS:     env.messageTS(),
					ResponseURL:   env.ResponseURL,
				},
				// A byte-identical replay inside Slack's own five-minute window
				// collapses onto the job already in flight. It is a CONVENIENCE,
				// not the correctness mechanism: the acknowledgement itself is
				// idempotent in the domain, which is what actually makes a
				// double-click, a replay and a job retry converge (§G.5).
				Opts: []db.JobOption{db.WithUniquePeriod(slackReplayWindow)},
			})
		case strings.HasPrefix(id, ActionNoopPrefix):
			// A URL button. Slack delivered an interaction oto is REQUIRED to
			// acknowledge and there is nothing else to do; the explicit namespace
			// is what makes that a decision rather than an oversight (S9).
		default:
			// ⛔ THE OUTCOME IS RECORDED, NOT MERELY LOGGED (§H.8). The response is
			// already a 200 and has to be — Slack disables an app's event
			// subscriptions when more than 95 % of deliveries fail in a 60-minute
			// window — so a counter is the only thing that turns "a human pressed a
			// button oto could not route" into something an operator can see
			// without grepping logs. There is no rejection table this belongs in:
			// `ingest_rejections` is the ingest path's, keyed by a NOT NULL
			// `source_id` under a closed reason enum bound to the §C.9.1 bounds
			// checks, and a Slack press has no source.
			s.metrics.unknownAction()
			logger.Warn("channels: a Slack interaction named an unknown action",
				slog.String("action_id", id))
		}
	}
	if len(reqs) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, interactionEnqueueTimeout)
	defer cancel()
	if _, err := s.enqueuer.EnqueueMany(ctx, reqs); err != nil {
		return err
	}
	return nil
}

// slackReplayWindow mirrors the transport's replay window. It is restated here
// rather than imported because `api` may not be imported by `service`, and both
// numbers mean the same thing: the age at which Slack itself stops considering a
// captured request live.
const slackReplayWindow = 5 * time.Minute

// Apply performs one interaction. It is the body of the `slack.interaction` job.
//
// ⛔ ITS ERROR RETURN IS FOR TRANSIENT FAILURE ONLY. A press that CANNOT apply —
// an alert that resolved, a group that does not exist, a conversation oto has no
// channel for — is not a job to retry twelve times; it is an outcome to tell the
// user about. Those paths send an ephemeral notice and return nil. What does
// return an error is a database that is down, because that genuinely is worth
// retrying.
func (s *InteractionService) Apply(ctx context.Context, args jobs.SlackInteractionArgs) error {
	logger := s.log.With(
		slog.String("action_id", args.ActionID),
		slog.String("slack_team_id", args.TeamID),
		slog.String("slack_channel_id", args.ChannelID))

	// ---- 1. THE TENANT -------------------------------------------------
	//
	// ⛔ THIS LINE IS WHERE THE TENANCY COMES FROM, and it is the last moment
	// anything can question it. `db.NewTenantScope` below turns the org id into
	// PROOF OF AUTHENTICATION — a TenantScope's field is unexported and every
	// repository method downstream takes one on trust — so a scope cannot
	// re-check what produced it, and nothing after this point asks whether the
	// org is still alive. `resolveSlackConversationSQL` therefore asks, in SQL,
	// with an INNER `orgs … deleted_at IS NULL` join, exactly as the four
	// resolvers in `identity/repository` do; the roll-call of all five and the
	// test that keeps it complete are documented on `resolveByEmailSQL`.
	//
	// The tests for this half are necessarily in `channels/repository`: the fake
	// below cannot have an opinion about a soft-deleted org, because the question
	// is a predicate and not a branch.
	dest, err := s.conversations.ResolveSlackConversation(ctx, args.TeamID, args.ChannelID)
	if err != nil {
		if errs.IsKind(err, errs.KindNotFound) {
			logger.Warn("channels: a Slack interaction named a conversation oto has no channel for")
			s.tell(ctx, args, "oto has no channel configured for this conversation, so it cannot record anything here. "+
				"Ask whoever set oto up to point a channel at this conversation.")
			return nil
		}
		return err
	}
	scope, err := db.NewTenantScope(dest.OrgID)
	if err != nil {
		return errs.Internal("slack_interaction_scope", err)
	}
	logger = logger.With(slog.String("org_id", dest.OrgID.String()))

	switch args.ActionID {
	case ActionAcknowledge:
		return s.applyAcknowledge(ctx, logger, scope, args)
	case ActionUnacknowledge:
		return s.applyUnacknowledge(ctx, args)
	default:
		// Reachable only across a deploy: `Handle` enqueues nothing it cannot
		// route, so a job carrying an unserved action is one an OLDER binary
		// enqueued and a newer one drained. It counts on the same series as the
		// transport's own unknown branch, because from an operator's chair it is
		// the same fact — a press oto could not act on.
		s.metrics.unknownAction()
		logger.Warn("channels: the interaction worker met an action it does not serve")
		return nil
	}
}

// applyAcknowledge is the whole point of the endpoint.
func (s *InteractionService) applyAcknowledge(
	ctx context.Context, logger *slog.Logger, scope db.TenantScope, args jobs.SlackInteractionArgs,
) error {
	// ---- 2. THE SUBJECT ------------------------------------------------
	// The button's value is an OPAQUE UUID and is resolved server-side (S8). A
	// value that is not a uuid is a card oto did not render, or a forgery inside
	// an authentic envelope; either way there is nothing to look up.
	groupID, err := uuid.Parse(args.Value)
	if err != nil || groupID == uuid.Nil {
		logger.Warn("channels: an Acknowledge button carried a value that is not an id")
		s.tell(ctx, args, "oto could not read that button. It may have been posted by an older version — "+
			"open the alert in oto and acknowledge it there.")
		return nil
	}
	logger = logger.With(slog.String("group_id", groupID.String()))

	// ⛔⛔ THE TENANCY CHECK, MADE EXPLICIT. Every call below is already scoped,
	// so a group belonging to another org would simply find no members and ack
	// nothing — a silent nothing, which is the exact failure mode being fixed.
	// Asking first turns it into an honest answer AND into something a test can
	// assert on.
	exists, err := s.groups.GroupExists(ctx, scope, groupID)
	if err != nil {
		return err
	}
	if !exists {
		logger.Warn("channels: a Slack interaction named a group that is not in this tenant")
		s.tell(ctx, args, "oto can no longer find that alert group from this channel. "+
			"It may have been removed, or it belongs to a different oto organisation.")
		return nil
	}

	// ---- 3. THE HUMAN --------------------------------------------------
	kind, actorID, label := s.actor(ctx, scope, args)

	// ---- 4. THE RECEIPT ------------------------------------------------
	res, err := s.groups.AcknowledgeGroup(ctx, scope, groupID, kind, actorID, label)
	if err != nil {
		if errs.IsKind(err, errs.KindNotFound) {
			// It existed a moment ago and does not now. A race, not a fault.
			s.tell(ctx, args, "That alert group is no longer available.")
			return nil
		}
		return err
	}

	// ⭐ NOTHING IS POSTED FROM HERE. The acknowledgement enqueued a
	// `notify.evaluate` inside its own transaction, and the card is updated by the
	// dispatch path — which is what keeps thread ordering, delivery idempotency and
	// the per-channel rate limit in force (§G.5, §G.7, §H.9). A `chat.update`
	// issued from this worker would bypass all three and race the very
	// notification it duplicates.
	if res.Applied > 0 {
		logger.Info("channels: acknowledged from Slack",
			slog.Int("members", res.Members), slog.Int("applied", res.Applied),
			slog.Int("unreached", res.Unreached))
	} else {
		logger.Info("channels: a Slack acknowledgement applied to nothing",
			slog.Int("members", res.Members), slog.Any("skipped", res.SkippedCodes),
			slog.Int("unreached", res.Unreached))
	}

	// ⛔ REACH IS ANSWERED FIRST, AND IT IS ANSWERED HOWEVER MANY THE PRESS
	// APPLIED TO. This used to be nested inside `Applied > 0`, which made the
	// warning unreachable in the exact case it was written for: a group above
	// domain.FanOutLimit whose oldest 500 members are ALREADY acknowledged applies
	// nothing, and fell through to "Already acknowledged" while thousands of its
	// alerts behind the ceiling were not. That is the failure this whole surface's
	// standard is against — see applyUnacknowledge — told from the other side: not
	// a button that pretends it worked, but one that pretends there is nothing
	// left to do. An unreached member is an unacknowledged alert whatever the
	// members in front of it answered.
	switch {
	case res.Unreached > 0:
		// The card will show what was acked and would otherwise read as the whole
		// group. Only the person who pressed is told, because this is about their
		// press and not a fact about the alerts.
		s.tell(ctx, args, partialAckText(res))
	case res.Applied == 0:
		s.tell(ctx, args, nothingAppliedText(res))
	}
	return nil
}

// applyUnacknowledge answers the Un-acknowledge button HONESTLY.
//
// ⛔ It is deliberately not implemented, and it deliberately does not pretend to
// be. `grouping` has no un-acknowledge fan-out — the group verbs are ack,
// comment and snooze — so wiring this button would mean inventing a fourth, and
// that is a scope decision, not a Slack one. Until it is made, the button says
// where the action does exist. The one thing it must never do is what the
// Acknowledge button used to: look like it worked.
func (s *InteractionService) applyUnacknowledge(ctx context.Context, args jobs.SlackInteractionArgs) error {
	s.tell(ctx, args, "Removing an acknowledgement is not available from Slack yet. "+
		"Open the alert group in oto to un-acknowledge it.")
	return nil
}

// partialAckText says that the press did not cover the group.
//
// ⛔ IT PROMISES NOTHING IT CANNOT DO. It does not say "press again": a second
// press walks the same members in the same order, finds them acknowledged and
// applies nothing, so telling somebody to press again during a storm would be
// worse than saying nothing at all. It reports the size of what is left and
// stops there.
//
// ⚠️ IT ALSO ANSWERS THE PRESS THAT APPLIED TO NOTHING. That is the case the
// ceiling actually produces after the first press: the oldest 500 members are
// already acknowledged, so the second press concludes on 500 refusals and never
// sees the thousands behind them. "Acknowledged 0 alerts" would be a strange
// sentence, so it is not said — but the count of what is still outstanding is,
// because that is the only number the person pressing needs.
func partialAckText(res GroupAckResult) string {
	if res.Applied == 0 {
		return fmt.Sprintf(
			"The alerts this acknowledgement reached were already acknowledged or have resolved. "+
				"This group is larger than one acknowledgement covers: %d of its alerts were "+
				"not reached and are still unacknowledged.",
			res.Unreached)
	}
	return fmt.Sprintf(
		"Acknowledged %d alerts. This group is larger than one acknowledgement covers: "+
			"%d of its alerts were not reached and are still unacknowledged.",
		res.Applied, res.Unreached)
}

// nothingAppliedText says WHICH kind of nothing happened.
//
// "Already acknowledged" and "it resolved while you were reading it" are
// different afternoons, and a button that answers both with the same shrug is
// only marginally better than one that says nothing at all.
//
// ⚠️ EVERY SENTENCE HERE IS ABOUT THE WHOLE GROUP, so it is only reached when
// the fan-out reached the whole group. An incomplete one is answered by
// partialAckText instead: "already acknowledged" said of the 500 members a
// bounded press concluded on is not a statement about the 4 500 behind them, and
// said to somebody in a storm it is the exact opposite of one.
func nothingAppliedText(res GroupAckResult) string {
	switch {
	case res.Members == 0:
		return "There is nothing to acknowledge — this alert group has no live alerts left."
	case res.SkippedCodes["already_acked"] == res.Skipped() && res.Skipped() > 0:
		// Somebody got here first — possibly the same person, twice. Either way
		// the fact is already on the timeline and nothing needs to change.
		return "Already acknowledged. An acknowledgement records that somebody has seen this; " +
			"it does not change the alert's state."
	case res.SkippedCodes["already_acked"] > 0:
		return "Nothing to do: part of this group was already acknowledged and the rest has resolved or expired."
	default:
		return "There is nothing to acknowledge — every alert in this group has already resolved or expired."
	}
}

// Skipped is the total number of members that refused the verb.
func (r GroupAckResult) Skipped() int {
	n := 0
	for _, c := range r.SkippedCodes {
		n += c
	}
	return n
}

// actor names the human, in the two forms the timeline accepts.
//
// ⛔ A SLACK MEMBER WITH NO OTO ACCOUNT STILL ACKS, and is recorded as
// `actor_kind = 'slack'` — the kind `alerts/domain` declares for exactly this
// person: "a human acting through a Slack interaction". Their id is the Slack
// member id, their label is the handle they had at press time, and
// `alert_occurrences.acked_by` stays NULL because there is no oto user to point
// at. Every one of those is the truth about what happened.
//
// ⛔ NOTHING HERE IS AN ACCOUNT, AN INVITATION OR A CLAIM. Recording who pressed
// a button is operationally necessary (R8) and is the end of it: oto grows no
// per-person metric from it, and the label is denormalised precisely so that it
// describes an event rather than tracking a person.
func (s *InteractionService) actor(
	ctx context.Context, scope db.TenantScope, args jobs.SlackInteractionArgs,
) (kind, id, label string) {
	unlinked := func() (string, string, string) {
		l := args.SlackUserName
		if l == "" {
			return "slack", args.SlackUserID, args.SlackUserID
		}
		return "slack", args.SlackUserID, "@" + l
	}

	if s.actors == nil {
		return unlinked()
	}
	a, err := s.actors.SlackActor(ctx, scope, args.TeamID, args.SlackUserID, args.SlackUserName)
	if err != nil {
		// A directory lookup that fails must never cost an acknowledgement. The
		// press is recorded against the Slack member, which is what oto actually
		// observed.
		s.log.WarnContext(ctx, "channels: could not resolve a Slack member to an oto user",
			slog.String("error", err.Error()))
		return unlinked()
	}
	if a.UserID == uuid.Nil || a.Label == "" {
		return unlinked()
	}
	return "user", a.UserID.String(), a.Label
}

// tell sends the person who pressed the button a message only they can see.
//
// A failure to deliver it is logged and dropped: the alert-side outcome has
// already been decided, and turning "oto could not reach hooks.slack.com" into a
// job retry would re-run the acknowledgement to redeliver a notice.
func (s *InteractionService) tell(ctx context.Context, args jobs.SlackInteractionArgs, text string) {
	if s.notice == nil || args.ResponseURL == "" {
		return
	}
	if err := s.notice.Ephemeral(ctx, args.ResponseURL, text); err != nil {
		s.log.WarnContext(ctx, "channels: could not answer a Slack interaction",
			slog.String("error", err.Error()))
	}
}
