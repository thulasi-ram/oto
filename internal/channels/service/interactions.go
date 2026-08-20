package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
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
	// ActionAcknowledge records that a human has SEEN this alert.
	//
	// ⛔ IT USED TO SAY "the alerts in this group" AND THE BUTTON'S SUBJECT HAS
	// CHANGED UNDER IT (git-bug 7570090). A conversation holds exactly one Case,
	// which is one Alert's firing episode, so a press acknowledges ONE Case. The
	// action id itself must NOT change with it: cards posted before this change are
	// still sitting in Slack carrying `oto.ack`, and renaming it turns every one of
	// those buttons into the silent no-op this whole file exists to abolish. What
	// changed is what the button's VALUE names — see `Cases`.
	//
	// ⛔ IT IS A RECEIPT AND NOTHING ELSE (§E.1.1). It does not change state, it
	// does not claim the alert, it does not say who will look at it, and no copy
	// anywhere in this file may suggest otherwise. oto has no axis on which an
	// alert belongs to a person, and the day this button implies one is the day
	// oto has become the product it exists not to be.
	ActionAcknowledge = "oto.ack"
	// ActionUnacknowledge takes that receipt back, off the same Case and by the
	// same four steps — see applyUnacknowledge. It is the ONLY withdrawal this
	// surface has, because ack is the only thing it writes.
	ActionUnacknowledge = "oto.unack"
	// ActionSnooze asks oto to GO QUIET about one signal for a while (§B.8.3,
	// §B.8.6). It is the third verb this surface writes and the first that is not a
	// receipt: an ack is a statement about what a human has seen, a snooze is an
	// instruction about what OTO does next.
	//
	// ⭐ IT IS STILL NOT A CLAIM ABOUT A PERSON (§E.1.1). Snooze changes nothing in
	// the cluster, nothing about the alert's state, and nothing about who is
	// looking at it — the alert goes on firing, the card keeps its colour and its
	// `:rotating_light:`, and only oto's own narration stops (§B.8.1, §H.4). That
	// is why it may live on a chat card at all, where "resolve" and "assign" may
	// not: it is reversible, it expires by itself, and it is attributed.
	//
	// ⛔ IT IS THE ONE ACTION ON THIS SURFACE WHOSE PAYLOAD IS NOT A BARE ID, and
	// it is a MENU rather than a button for the reason §B.8.3 gives: there are five
	// presets and no free-text duration, because "there is no indefinite snooze".
	// The chosen option's value carries the preset token AND the alert, separated
	// by snoozeValueSeparator, and the token is looked up in a closed table rather
	// than parsed — see snoozeSubject.
	ActionSnooze = "oto.snooze"
	// ActionUnsnooze ends that quiet early, and it is what §B.8.6 means by "the
	// `Snooze` action BECOMES `:bell: Unsnooze`": the two never appear on one card,
	// because a card offering both would be asking the reader which of two
	// contradictory facts about oto is true.
	//
	// ⛔ ITS VALUE IS AN ALERT ID AND `oto.ack`'S IS A CASE ID. Both are bare
	// uuids, both are opaque, and they name DIFFERENT TABLES — a snooze is a fact
	// about the signal's notification behaviour and outlives any one episode
	// (§B.8.7), while a receipt is a fact about one episode. Swapping them would
	// resolve to nothing and be answered honestly, which is the good outcome; the
	// bad one is assuming they are interchangeable because they look alike.
	ActionUnsnooze = "oto.unsnooze"
	// ActionNoopPrefix marks the URL buttons. Slack delivers an interaction for
	// every one of them and oto must acknowledge it; there is nothing to do
	// beyond that, and the explicit namespace is what says so out loud.
	ActionNoopPrefix = "oto.noop."
)

// snoozeValueSeparator joins the preset token to the alert id in a snooze
// option's value.
//
// ⛔ THE VALUE IS STILL NOT TRUSTED AND STILL CARRIES NO STATE (S8). It is two
// SELECTORS and nothing else: which of the five offered durations was chosen, and
// which row to look up. Neither half is decoded into behaviour — the token is
// matched against `domain.SnoozeDuration`'s closed list and the id is resolved in
// oto's own database under the tenant scope — so a forged value can name a
// duration oto never offered or an alert in another org and be refused either way.
// ⭐ THE BYTE ITSELF IS `channels/domain`'s, not this file's, for the same reason
// the preset list is: the module that MINTS the value and the module that SPLITS it
// have to agree, and two spellings of one separator is a menu whose every option
// this handler refuses. See domain.SnoozeValueSeparator.
const snoozeValueSeparator = domain.SnoozeValueSeparator

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
// §5.4). Two of them reach UPSTREAM — `alerts` acks the Case, `identity`
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

// Cases is one human verb over ONE Case.
//
// ⛔⛔ IT WAS `AlertGroups`, A FAN-OUT OVER ONE GROUP GENERATION, AND THAT SHAPE IS
// DELETED (git-bug 7570090). `GroupExists`, `AcknowledgeGroup` and
// `UnacknowledgeGroup` each took a group uuid and the two verbs returned a
// `GroupAckResult` — members offered, members applied, members refused by code,
// members never reached because a group could be larger than `domain.FanOutLimit`.
// AlertGroups are gone: a conversation holds exactly one Case, a Case is one
// Alert's firing episode, and one is not a fan-out. There is no ceiling to exceed,
// no partial press to report and no per-member account to keep.
//
// ⭐ THE BUTTON STILL WORKS AND ITS PAYLOAD STILL CARRIES ONE OPAQUE UUID. What
// changed is which table that uuid names: it is a CASE id now, and the renderer
// mints it. `alerts/service` already spoke this shape before this change did —
// `AcknowledgeAs`/`UnacknowledgeAs` take a case id and say why in as many words:
// "the episode is what makes an alert a member of a generation, so the id was being
// read, discarded, and looked up again from the alert one layer down".
//
// ⛔ There are exactly TWO verbs here, and they are ONE VERB AND ITS UNDO. A chat
// message is not a place to resolve, close, silence or assign anything: it has no
// confirmation, no audit trail of oto's own and no undo. Acknowledge is a fact
// ABOUT the alert, which is precisely why it may be withdrawn — the withdrawal
// says "that receipt was wrong", and nothing about a person. Every other verb
// would be a claim about one.
//
// ⛔ THE VERBS RETURN ONLY AN ERROR, AND THE REFUSAL IS IN ITS CODE. A press that
// cannot apply is not a failure to retry — it is an outcome to tell the human
// about — and `alerts/domain` already names each outcome with a stable
// `errs.KindPrecondition` code: `already_acked`, `not_acked`, `no_open_case`. Those
// three codes are the whole vocabulary this surface's copy is keyed on, and they
// are the same three the HTTP contract answers 412 with. A port that invented its
// own would be a second spelling of the same refusal.
type Cases interface {
	// CaseExists reports whether one Case is visible in this tenant. It is the
	// tenancy check made explicit: a case id from another org answers false rather
	// than silently acking nothing.
	CaseExists(ctx context.Context, s db.TenantScope, caseID uuid.UUID) (bool, error)
	// AcknowledgeCase records the receipt on one OPEN Case. A refusal comes back as
	// `errs.KindPrecondition` carrying `already_acked` or `no_open_case`.
	AcknowledgeCase(ctx context.Context, s db.TenantScope, caseID uuid.UUID,
		actorKind, actorID, actorLabel string) error
	// UnacknowledgeCase withdraws the receipt from one OPEN Case. Its refusals are
	// `not_acked` or `no_open_case`, and they are NOT interchangeable with the ack's
	// — see unackRefusalText.
	UnacknowledgeCase(ctx context.Context, s db.TenantScope, caseID uuid.UUID,
		actorKind, actorID, actorLabel string) error
}

// Snoozes is oto's own quiet, over ONE ALERT.
//
// ⛔ ITS SUBJECT IS THE ALERT AND NOT THE CASE, WHICH MAKES IT THE ONLY PORT ON
// THIS SURFACE THAT IS NOT CASE-SHAPED. A snooze is "a fact about a signal's
// notification behaviour" (§B.8.7): it is scoped to an `alert_key`, it survives the
// episode that provoked it, and `alerts/service`'s own verbs — `SnoozeAs`,
// `UnsnoozeAs` — take an alert id for exactly that reason. Reshaping this port
// around the Case to match its neighbour would have meant resolving case → alert
// somewhere, and the somewhere would have been a layer with no business knowing
// that a Case has exactly one Alert.
//
// ⛔ THERE IS NO NOTE, AND THAT IS DELIBERATE RATHER THAN UNFINISHED. `snooze(note)`
// exists in the domain and in the API because a form has somewhere to type; a
// two-tap menu on a card does not, and an empty note is more honest than a
// fabricated one. The attribution is not lost — `actorLabel` is recorded on the
// `alert_snoozes` row and in the `alert.snoozed` event either way, so the timeline
// still says who went quiet and until when.
//
// ⛔ NO EXPLICIT TENANCY PROBE, UNLIKE `Cases`. The reason `CaseExists` is a
// separate call is that a scoped ack against another org's Case writes NOTHING and
// says nothing; these two verbs both begin by READING the alert under the scope, so
// an alert this tenant cannot see comes back as `errs.KindNotFound` on its own and
// the human gets a sentence. Adding a probe would be a second round trip to
// re-answer a question the verb already answers.
//
// ⛔ THE VERBS RETURN ONLY AN ERROR, and the refusal is in its code — the same
// contract `Cases` states and for the same reason. `not_snoozed` is the one
// precondition an unsnooze can earn; a snooze has none, because it is orthogonal to
// state and supersedes its own incumbent rather than refusing (§B.8.3).
type Snoozes interface {
	// SnoozeAlert makes oto quiet about one alert until `until`. An `until` outside
	// the domain's 5-minute…30-day window is a validation error, which is why the
	// caller derives it from a preset and never from the payload.
	SnoozeAlert(ctx context.Context, s db.TenantScope, alertID uuid.UUID,
		actorKind, actorID, actorLabel string, until time.Time) error
	// UnsnoozeAlert ends the quiet early, with `ended_reason='manual'`. It refuses
	// with `errs.KindPrecondition` carrying `not_snoozed` when there is no quiet to
	// end — which is the double-click, and is an outcome rather than a fault.
	UnsnoozeAlert(ctx context.Context, s db.TenantScope, alertID uuid.UUID,
		actorKind, actorID, actorLabel string) error
}

// ⛔⛔ `GroupAckResult` WAS HERE AND IS DELETED IN FULL (git-bug 7570090). It held
// `Members`, `Applied`, `SkippedCodes` and `Unreached` — the account of one fan-out
// over one group generation — and it is gone because there is no fan-out left. One
// conversation, one Case, one Alert's episode: the verb either applied or it was
// refused, and the refusal has a code.
//
// ⭐ THE STANDARD IT ENFORCED IS THE PART THAT MUST NOT GO WITH IT, because it was
// won the hard way. `Unreached` existed "SO THE BUTTON CANNOT LOOK LIKE IT WORKED":
// a group above `domain.FanOutLimit` whose oldest 500 members were already
// acknowledged applied nothing, fell through to "Already acknowledged" and left
// 4 500 alerts nobody had seen. The lesson is that a press must never be answered
// with a sentence about a bigger set than it actually touched. A one-Case press
// cannot break that rule by arithmetic — its set is exactly one — so the rule now
// holds by construction rather than by a counter. If anything on this surface ever
// acts on more than one Case again, this comment is the reason it needs an account
// before it needs copy.

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
	Cases         Cases
	// Snoozes is OPTIONAL, and a nil one is a deployment that has not wired oto's
	// quiet yet rather than a card without a Snooze menu — the renderer is a pure
	// function of the view and cannot see this field. A press therefore has to be
	// answered by a sentence saying so, which `applySnooze` does; the one thing it
	// must never be answered with is the silence this file exists to abolish.
	Snoozes  Snoozes
	Enqueuer db.Enqueuer
	Notice   SlackNotice
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
	cases         Cases
	snoozes       Snoozes
	enqueuer      db.Enqueuer
	notice        SlackNotice
	metrics       *InteractionMetrics
	clk           clock.Clock
	log           *slog.Logger
}

// NewInteractionService builds the consumer.
func NewInteractionService(o InteractionOptions) (*InteractionService, error) {
	if o.Conversations == nil || o.Cases == nil || o.Enqueuer == nil {
		return nil, errs.New(errs.KindInternal, "slack_interactions_deps",
			"a conversation resolver, a case action port and an enqueuer are required")
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
		cases:         o.Cases,
		snoozes:       o.Snoozes,
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
// read the acknowledgement needs — the org, the Case, the actor — is deferred to
// `Apply`, because the sum of them is not reliably under three
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
		// ⛔ THE FOUR WRITING ACTIONS SHARE ONE ARM AND ONE ENQUEUE, and none of
		// them reads the database here — see the note on `Handle`. `a.value()` is
		// what makes the arm shared at all: `oto.snooze` is a MENU, so Slack sends
		// its answer under `selected_option.value` where the three buttons send
		// theirs under `value`, and reading only the latter would enqueue every
		// snooze press with an empty subject.
		case id == ActionAcknowledge, id == ActionUnacknowledge,
			id == ActionSnooze, id == ActionUnsnooze:
			reqs = append(reqs, db.JobRequest{
				Args: jobs.SlackInteractionArgs{
					ActionID:      id,
					Value:         a.value(),
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
// an alert that resolved, a Case that does not exist, a conversation oto has no
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
		return s.applyUnacknowledge(ctx, logger, scope, args)
	case ActionSnooze:
		return s.applySnooze(ctx, logger, scope, args)
	case ActionUnsnooze:
		return s.applyUnsnooze(ctx, logger, scope, args)
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
	//
	// ⛔ IT NAMES A CASE, NOT A GROUP GENERATION (git-bug 7570090). A card rendered
	// before this change carries a GROUP uuid, which resolves to no Case and is
	// answered by the honest "oto can no longer find that alert" below rather than by
	// a silent nothing. That is the correct outcome for a stale button and it is why
	// the tenancy check is a lookup rather than a scope predicate.
	caseID, err := uuid.Parse(args.Value)
	if err != nil || caseID == uuid.Nil {
		logger.Warn("channels: an Acknowledge button carried a value that is not an id")
		s.tell(ctx, args, "oto could not read that button. It may have been posted by an older version — "+
			"open the alert in oto and acknowledge it there.")
		return nil
	}
	logger = logger.With(slog.String("case_id", caseID.String()))

	// ⛔⛔ THE TENANCY CHECK, MADE EXPLICIT. Every call below is already scoped, so
	// a Case belonging to another org would simply write nothing — a silent
	// nothing, which is the exact failure mode being fixed. Asking first turns it
	// into an honest answer AND into something a test can assert on.
	exists, err := s.cases.CaseExists(ctx, scope, caseID)
	if err != nil {
		return err
	}
	if !exists {
		logger.Warn("channels: a Slack interaction named a case that is not in this tenant")
		s.tell(ctx, args, "oto can no longer find that alert from this channel. "+
			"It may have been removed, or it belongs to a different oto organisation.")
		return nil
	}

	// ---- 3. THE HUMAN --------------------------------------------------
	kind, actorID, label := s.actor(ctx, scope, args)

	// ---- 4. THE RECEIPT ------------------------------------------------
	//
	// ⭐ NOTHING IS POSTED FROM HERE. The acknowledgement enqueued a
	// `notify.evaluate` inside its own transaction, and the card is updated by the
	// dispatch path — which is what keeps thread ordering, delivery idempotency and
	// the per-channel rate limit in force (§G.5, §G.7, §H.9). A `chat.update`
	// issued from this worker would bypass all three and race the very
	// notification it duplicates.
	//
	// ⭐ AND A SUCCESSFUL PRESS SAYS NOTHING IN SLACK, for the same reason: the CARD
	// is the feedback. An ephemeral "done" would be oto talking about itself.
	switch err := s.cases.AcknowledgeCase(ctx, scope, caseID, kind, actorID, label); {
	case err == nil:
		logger.Info("channels: acknowledged from Slack")
		return nil
	case errs.IsKind(err, errs.KindNotFound):
		// It existed a moment ago and does not now. A race, not a fault.
		s.tell(ctx, args, "That alert is no longer available.")
		return nil
	case errs.IsKind(err, errs.KindPrecondition):
		// ⛔ THIS IS AN OUTCOME, NOT A FAILURE. The alert was already acknowledged,
		// or its episode ended while the human was deciding. Retrying either one
		// twelve times would re-run the verb all night to redeliver a sentence.
		code := errs.CodeOf(err)
		logger.Info("channels: a Slack acknowledgement applied to nothing",
			slog.String("refusal", code))
		s.tell(ctx, args, ackRefusalText(code))
		return nil
	default:
		return err
	}
}

// applyUnacknowledge is applyAcknowledge READ BACKWARDS, and the symmetry is the
// design rather than a coincidence of implementation.
//
// ⭐ THE FOUR NUMBERED STEPS ARE THE SAME FOUR, IN THE SAME ORDER, FOR THE SAME
// REASONS. Subject, tenancy, human, receipt — every one of them argued above and
// none of them re-argued here. `alerts.Unacknowledge` writes the same row the ack
// does, under the same scope, and refuses with the same closed set of codes; there
// is no set-level ack column for either verb to touch and there will not be one,
// so "un-acknowledged" means, and only means, that this Case carries no receipt.
//
// ⛔ WHAT IS NOT SHARED IS THE COPY, and that is the only reason this surface
// needed anything at all once the domain verb existed. "Somebody already acked
// this" and "there was no receipt here to take back" are different afternoons in
// the mirror exactly as they are in the original, and a withdrawal that answered
// both with the ack's sentences would be describing the wrong nothing. See
// unackRefusalText.
//
// ⛔ IT WITHDRAWS A RECEIPT AND NOTHING ELSE (§E.1.1). It does not reopen an
// alert, it does not re-notify, it does not hand the alert back to anybody — there
// is nobody to hand it to — and no sentence below may suggest otherwise.
func (s *InteractionService) applyUnacknowledge(
	ctx context.Context, logger *slog.Logger, scope db.TenantScope, args jobs.SlackInteractionArgs,
) error {
	// ---- 2. THE SUBJECT ------------------------------------------------
	caseID, err := uuid.Parse(args.Value)
	if err != nil || caseID == uuid.Nil {
		logger.Warn("channels: an Un-acknowledge button carried a value that is not an id")
		s.tell(ctx, args, "oto could not read that button. It may have been posted by an older version — "+
			"open the alert in oto and withdraw the acknowledgement there.")
		return nil
	}
	logger = logger.With(slog.String("case_id", caseID.String()))

	// ⛔⛔ THE TENANCY CHECK, MADE EXPLICIT — for the reason given on the ack. A
	// scoped withdrawal against another org's Case would write nothing, which is the
	// silent nothing this whole file exists to abolish.
	exists, err := s.cases.CaseExists(ctx, scope, caseID)
	if err != nil {
		return err
	}
	if !exists {
		logger.Warn("channels: a Slack interaction named a case that is not in this tenant")
		s.tell(ctx, args, "oto can no longer find that alert from this channel. "+
			"It may have been removed, or it belongs to a different oto organisation.")
		return nil
	}

	// ---- 3. THE HUMAN --------------------------------------------------
	//
	// ⚠️ IT IS WHOEVER PRESSED, AND IT NEED NOT BE WHOEVER ACKED. oto has no axis
	// on which a receipt belongs to the person who wrote it, so there is nothing
	// here to check permission against; the withdrawal is recorded against the
	// presser, which is what oto actually observed.
	kind, actorID, label := s.actor(ctx, scope, args)

	// ---- 4. THE WITHDRAWAL ---------------------------------------------
	//
	// ⭐ NOTHING IS POSTED FROM HERE, for the reason given on the ack: the card is
	// updated by the dispatch path, which is the only place thread ordering,
	// delivery idempotency and the per-channel rate limit hold (§G.5, §G.7, §H.9).
	switch err := s.cases.UnacknowledgeCase(ctx, scope, caseID, kind, actorID, label); {
	case err == nil:
		logger.Info("channels: un-acknowledged from Slack")
		return nil
	case errs.IsKind(err, errs.KindNotFound):
		s.tell(ctx, args, "That alert is no longer available.")
		return nil
	case errs.IsKind(err, errs.KindPrecondition):
		code := errs.CodeOf(err)
		logger.Info("channels: a Slack un-acknowledgement applied to nothing",
			slog.String("refusal", code))
		s.tell(ctx, args, unackRefusalText(code))
		return nil
	default:
		return err
	}
}

// applySnooze is the first verb on this surface that changes what OTO DOES rather
// than what oto has recorded, and every one of the four numbered steps means
// something slightly different because of it.
//
// ⭐ THE SUBJECT IS AN ALERT AND THE STEP HAS A SECOND HALF. A snooze press answers
// a question — "for how long?" — so its payload names a duration as well as a row,
// and the duration is the half an attacker would want. It is never taken from the
// payload as a number: the token is looked up in `channels/domain`'s closed preset
// table and an unknown one is refused, which keeps S8 true of a value that is no
// longer a bare uuid. See snoozeSubject.
//
// ⭐ THE TENANCY CHECK IS THE VERB ITSELF, unlike the ack's explicit `CaseExists`.
// `alerts/service.Snooze` opens by reading the alert under this scope, so an alert
// belonging to another org comes back `errs.KindNotFound` rather than writing a
// silent nothing — the failure mode `CaseExists` exists to prevent cannot occur
// here, and asking anyway would be a second round trip for the same answer.
//
// ⛔ NOTHING IS POSTED FROM HERE, for the reason both acks give: the snooze enqueues
// its own `notify.evaluate(reason=snoozed)` inside its transaction, and §B.8.4 makes
// `snoozed` exempt from the very suppression it creates precisely so the channel is
// TOLD it is going quiet. The card and its thread reply come from the dispatch path,
// where thread ordering, delivery idempotency and the per-channel rate limit hold
// (§G.5, §G.7, §H.9).
func (s *InteractionService) applySnooze(
	ctx context.Context, logger *slog.Logger, scope db.TenantScope, args jobs.SlackInteractionArgs,
) error {
	// ---- 2. THE SUBJECT ------------------------------------------------
	alertID, quiet, ok := snoozeSubject(args.Value)
	if !ok {
		logger.Warn("channels: a Snooze menu carried a value oto did not mint")
		s.tell(ctx, args, "oto could not read that choice. It may have been posted by an older version — "+
			"open the alert in oto and snooze it there.")
		return nil
	}
	logger = logger.With(
		slog.String("alert_id", alertID.String()),
		slog.String("snooze_for", quiet.String()))

	// ⚠️ AN UNWIRED PORT IS ANSWERED WITH A SENTENCE, NEVER WITH NOTHING. The
	// renderer is a pure function of the view and cannot know whether this
	// deployment injected a snooze port, so the menu is on the card either way; the
	// only thing this branch can still control is whether the human finds out.
	if s.snoozes == nil {
		logger.Warn("channels: a Snooze press arrived at a deployment with no snooze port")
		s.tell(ctx, args, "oto cannot snooze from Slack in this deployment yet. "+
			"Open the alert in oto and snooze it there.")
		return nil
	}

	// ---- 3. THE HUMAN --------------------------------------------------
	//
	// A snooze is ALWAYS attributed — the domain refuses a non-human actor — and
	// the label is what the card's own `*Notifications*` field prints back:
	// ":zzz: Snoozed by <@U…> until …" (§B.8.6). An unlinked Slack member still
	// snoozes, recorded as `actor_kind = 'slack'`; see actor.
	kind, actorID, label := s.actor(ctx, scope, args)

	// ---- 4. THE QUIET --------------------------------------------------
	//
	// ⭐ THE CLOCK STARTS WHEN OTO ACTS, NOT WHEN THE CARD WAS DRAWN. The preset is
	// a LENGTH and the absolute moment is derived here, on the worker, because that
	// is the last point before the write: a card rendered an hour ago would
	// otherwise have baked in an expiry that was already in the past, and §B.8.3's
	// 5-minute minimum would refuse the press with a validation error the human
	// could do nothing about. A job retry re-anchors it, which is the same reading —
	// "quiet for thirty minutes from when oto went quiet".
	until := s.clk.Now().Add(quiet)
	switch err := s.snoozes.SnoozeAlert(ctx, scope, alertID, kind, actorID, label, until); {
	case err == nil:
		logger.Info("channels: snoozed from Slack")
		return nil
	case errs.IsKind(err, errs.KindNotFound):
		s.tell(ctx, args, "oto can no longer find that alert from this channel. "+
			"It may have been removed, or it belongs to a different oto organisation.")
		return nil
	case errs.IsKind(err, errs.KindPrecondition), errs.IsKind(err, errs.KindValidation):
		// ⛔ AN OUTCOME, NOT A FAILURE — the same rule the two acks are under. A
		// snooze earns no precondition today (it supersedes its own incumbent rather
		// than refusing, §B.8.3), and the arm is here anyway because a code this
		// surface has never seen must still produce a sentence.
		code := errs.CodeOf(err)
		logger.Info("channels: a Slack snooze applied to nothing",
			slog.String("refusal", code))
		s.tell(ctx, args, snoozeRefusalText(code))
		return nil
	default:
		return err
	}
}

// applyUnsnooze ends the quiet early, and it is applySnooze with the question
// removed: there is exactly one way to stop being quiet, so there is no preset to
// decode and the value is a bare alert id again.
//
// ⛔ IT IS NOT AN ACK AND IT IS NOT A RESOLVE. Waking oto up says "start telling me
// about this again" and nothing whatever about the alert, which is still firing,
// still the same colour, and still carrying whatever receipt it had before somebody
// went quiet about it (§B.8.1).
func (s *InteractionService) applyUnsnooze(
	ctx context.Context, logger *slog.Logger, scope db.TenantScope, args jobs.SlackInteractionArgs,
) error {
	// ---- 2. THE SUBJECT ------------------------------------------------
	alertID, err := uuid.Parse(args.Value)
	if err != nil || alertID == uuid.Nil {
		logger.Warn("channels: an Unsnooze button carried a value that is not an id")
		s.tell(ctx, args, "oto could not read that button. It may have been posted by an older version — "+
			"open the alert in oto and wake it there.")
		return nil
	}
	logger = logger.With(slog.String("alert_id", alertID.String()))

	if s.snoozes == nil {
		logger.Warn("channels: an Unsnooze press arrived at a deployment with no snooze port")
		s.tell(ctx, args, "oto cannot end a snooze from Slack in this deployment yet. "+
			"Open the alert in oto and wake it there.")
		return nil
	}

	// ---- 3. THE HUMAN --------------------------------------------------
	//
	// ⚠️ IT IS WHOEVER PRESSED, AND IT NEED NOT BE WHOEVER SNOOZED — the same
	// reading the withdrawal of a receipt is under. oto has no axis on which a
	// snooze belongs to the person who started it, so there is nothing here to check
	// a permission against, and a quiet nobody can end is a mute by another name.
	kind, actorID, label := s.actor(ctx, scope, args)

	// ---- 4. THE WAKE-UP ------------------------------------------------
	switch err := s.snoozes.UnsnoozeAlert(ctx, scope, alertID, kind, actorID, label); {
	case err == nil:
		logger.Info("channels: unsnoozed from Slack")
		return nil
	case errs.IsKind(err, errs.KindNotFound):
		s.tell(ctx, args, "That alert is no longer available.")
		return nil
	case errs.IsKind(err, errs.KindPrecondition):
		code := errs.CodeOf(err)
		logger.Info("channels: a Slack unsnooze applied to nothing",
			slog.String("refusal", code))
		s.tell(ctx, args, unsnoozeRefusalText(code))
		return nil
	default:
		return err
	}
}

// snoozeSubject splits one snooze option's value into the alert it names and the
// duration oto offered for it.
//
// ⛔ THE DURATION IS LOOKED UP, NEVER PARSED (S8, §B.8.3). `domain.SnoozeDuration`
// matches the token against the five presets the card actually offered; anything
// else — including a perfectly well-formed `720h` — answers false and the press is
// refused. A `time.ParseDuration` here would have let four edited characters buy a
// month of silence on somebody else's alert, and §B.8.3's "there is no indefinite
// snooze" would have become advice.
//
// ⭐ IT REFUSES A THIRD FIELD RATHER THAN IGNORING ONE. `SplitN(…, 2)` would read
// `30m|<uuid>|anything` as a valid press and silently drop the tail; a value oto did
// not mint is not a value to interpret charitably.
func snoozeSubject(value string) (uuid.UUID, time.Duration, bool) {
	token, rest, found := strings.Cut(strings.TrimSpace(value), snoozeValueSeparator)
	if !found || strings.Contains(rest, snoozeValueSeparator) {
		return uuid.Nil, 0, false
	}
	quiet, ok := domain.SnoozeDuration(token)
	if !ok {
		return uuid.Nil, 0, false
	}
	alertID, err := uuid.Parse(rest)
	if err != nil || alertID == uuid.Nil {
		return uuid.Nil, 0, false
	}
	return alertID, quiet, true
}

// snoozeRefusalText says which kind of nothing a snooze met.
//
// ⛔ IT HAS NO NAMED CODE TODAY AND STILL EXISTS, which is the opposite of dead
// code: §B.8.3 makes a snooze unrefusable by design — it is orthogonal to state, so
// a resolved alert can be snoozed, and an existing quiet is SUPERSEDED rather than
// rejected — so the only reachable arm is the default. The default is the whole
// point. Every other verb on this surface learned its refusal codes after a human
// had already been answered with silence, and a press this surface cannot explain
// must still get a sentence (§H.8).
func snoozeRefusalText(code string) string {
	switch code {
	case "no_open_case":
		// Not reachable through the domain today, and it would still be the wrong
		// sentence to leave unwritten: an alert whose episode has ended is one oto
		// has nothing left to say about, which is what the human was asking for.
		return "There is nothing to quieten — this alert has already resolved or expired."
	default:
		return "oto could not snooze this alert. Open it in oto to see why."
	}
}

// unsnoozeRefusalText is snoozeRefusalText in the mirror, and unlike its twin it
// has a real code to key on.
//
// ⛔ `not_snoozed` IS THE DOUBLE-CLICK AND MUST NOT READ AS A FAULT. Two people
// looking at the same card both press Unsnooze; the second one gets this, and the
// state they wanted is the state that holds. Telling them something went wrong
// would be describing a success.
func unsnoozeRefusalText(code string) string {
	switch code {
	case "not_snoozed":
		return "oto is not quiet about this alert — there is no snooze here to end. " +
			"Ending a snooze only starts the notifications again; it does not change the alert's state."
	case "no_open_case":
		return "There is nothing to wake — this alert has already resolved or expired."
	default:
		return "There is no snooze here to end."
	}
}

// ⛔⛔ `partialAckText` AND `partialUnackText` WERE HERE AND ARE DELETED (git-bug
// 7570090). Each said "this group is larger than one acknowledgement covers: N of
// its alerts were not reached", and each was reached whenever `GroupAckResult.
// Unreached > 0`. A press now touches exactly one Case, so there is no ceiling to
// exceed and no remainder to report.
//
// ⭐ THE TWO THINGS THEY GOT RIGHT, RECORDED BECAUSE THEY WERE BOTH BUG FIXES.
// First: neither said "press again", because a second press walks the same rows,
// finds them acknowledged and applies nothing — telling somebody in a storm to
// press again would have been worse than saying nothing. Second, and it is the
// subtler one: the OUTSTANDING COUNT MEANT THE OPPOSITE THING ON THE TWO PATHS. An
// ack that did not reach a member left it UNACKNOWLEDGED; a withdrawal that did not
// reach one left it ACKNOWLEDGED — a receipt standing on an alert nobody had looked
// at since. Borrowing the ack's last sentence for the withdrawal would have been
// precisely backwards. That asymmetry survives in the two refusal-copy functions
// below, which is where it still applies.

// ackRefusalText says WHICH kind of nothing happened.
//
// "Already acknowledged" and "it resolved while you were reading it" are different
// afternoons, and a button that answers both with the same shrug is only marginally
// better than one that says nothing at all.
//
// ⛔ IT IS KEYED ON `alerts/domain`'s OWN PRECONDITION CODES AND INVENTS NOTHING.
// `already_acked` and `no_open_case` are the two the ack can earn, they are the
// same two the HTTP contract answers 412 with, and the `default` arm exists because
// a code this surface has never seen must still produce a sentence — a press
// answered with silence is the defect this whole file was written against.
//
// ⛔ EVERY SENTENCE IS ABOUT ONE ALERT, and that is now true by construction rather
// than by a reach check. See the `GroupAckResult` tombstone for what used to make
// it a live hazard.
func ackRefusalText(code string) string {
	switch code {
	case "already_acked":
		// Somebody got here first — possibly the same person, twice. Either way
		// the fact is already on the timeline and nothing needs to change.
		return "Already acknowledged. An acknowledgement records that somebody has seen this; " +
			"it does not change the alert's state."
	case "no_open_case":
		return "There is nothing to acknowledge — this alert has already resolved or expired."
	default:
		return "There is nothing to acknowledge — this alert is no longer in a state that admits one."
	}
}

// unackRefusalText is ackRefusalText in the mirror: it says WHICH kind of nothing
// happened, keyed on `not_acked` where that one keys on `already_acked`.
//
// ⛔ THE TWO CODES ARE NOT INTERCHANGEABLE AND NEITHER ARE THESE SENTENCES.
// `not_acked` is an OPEN alert carrying no receipt — nothing to take back, and
// quite possibly because somebody took it back a second earlier. `no_open_case` is
// an episode that has already ended, which is the same "nothing" the ack reports
// and the one the contract answers `412` to. Telling a human the alert resolved
// when in fact it is still firing unacknowledged is the worst sentence this file
// could say, and it is the whole reason this function exists rather than the
// withdrawal borrowing the ack's copy.
func unackRefusalText(code string) string {
	switch code {
	case "not_acked":
		// The double-click, and "somebody got here first". Either way the alert
		// carries no receipt now, which is what the press was asking for.
		return "Not acknowledged — there is no receipt here to take back. Withdrawing an " +
			"acknowledgement removes the record that somebody has seen this; it does not " +
			"change the alert's state."
	case "no_open_case":
		return "There is nothing to withdraw — this alert has already resolved or expired."
	default:
		return "There is nothing to withdraw — this alert is no longer in a state that admits a receipt."
	}
}

// ⛔ `GroupAckResult.Skipped()` WAS HERE AND IS DELETED WITH THE STRUCT (git-bug
// 7570090). It totalled the members that refused the verb, which is how the copy
// above used to tell "all of them refused for one reason" from "some of them did".
// One Case refuses for exactly one reason or not at all, so the code IS the total.

// actor names the human, in the two forms the timeline accepts.
//
// ⛔ A SLACK MEMBER WITH NO OTO ACCOUNT STILL ACKS, and is recorded as
// `actor_kind = 'slack'` — the kind `alerts/domain` declares for exactly this
// person: "a human acting through a Slack interaction". Their id is the Slack
// member id, their label is the handle they had at press time, and
// `alert_cases.acked_by` stays NULL because there is no oto user to point
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
