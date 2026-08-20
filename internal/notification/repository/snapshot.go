package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
)

// ⛔ `DefaultMaxAlerts` WAS HERE AND IS DELETED (git-bug `7570090`). It was "how
// many member instances a card shows inline before it says 'and N more' (§H.3)",
// and it capped `memberAlertsSQL`. A conversation now holds exactly ONE Case and a
// Case has exactly ONE Alert, so the relation it capped is at most one row and
// there is no page for a default page size to be about. `SnapshotQuery.MaxAlerts`
// survives — the view service still sets it — but nothing here reads it any more,
// and its own doc comment says so.

// SnapshotRepository reads the notification READ MODEL.
//
// It is a read model and nothing else: no invariant is enforced here and no row
// it touches is written here. That is what makes it safe for this module to
// project from tables another module owns — the state machine, the projections
// and the events all still have exactly one writer.
//
// It is also the DEFAULT IMPLEMENTATION of the narrow port
// `service.SnapshotSource`. Once `alerts/service` publishes an equivalent, the
// wiring swaps one constructor and this file becomes dead code; until then the
// notification module is not blocked on another module's schedule.
type SnapshotRepository struct {
	q   db.Querier
	clk clock.Clock
}

// NewSnapshotRepository builds the read model over a fallback querier.
func NewSnapshotRepository(q db.Querier, clk clock.Clock) *SnapshotRepository {
	if clk == nil {
		clk = clock.New()
	}
	return &SnapshotRepository{q: q, clk: clk}
}

func (r *SnapshotRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

const orgFactsSQL = `SELECT id, slug, name FROM orgs WHERE id = $1`

// caseStateSQL is `Case.AlertState` written in SQL: the four §B.2 words a card
// renders, recomposed from the two columns that still carry them.
//
// ⭐ THE CARD'S VOCABULARY DID NOT CHANGE WHEN THE COLUMN DID. `alert_cases.state`
// is `open | closed` since migration 00054, but a Slack card and the frozen
// `oto.notification.v1` webhook both say `firing` / `suppressed` / `resolved` /
// `expired` about an episode, and both must go on saying it. So the reading is
// derived at the edge of the read rather than stored: the ALERT answers the open
// half (an open case IS its alert's current one, by case_one_open_idx) and
// `resolve_reason` answers the closed half, which is the column that always
// carried resolved-apart-from-expired.
//
// ⛔ IT IS A JOIN TO `alerts` AND NOT A READ OF `o.suppression_reason`: nothing
// CHECKS that column against a state any more, and a card is the last place to
// trust an unenforced invariant. The argument was `grouping/repository`'s, made at
// length on `memberRollupSQL`; that module is deleted (git-bug `7570090`) and the
// reasoning outlived it, so it is stated here rather than cited from a file that
// no longer exists.
// ⭐ THE `suppressed` ARM IS FIRST AND IT READS THE ALERT'S AXIS, NOT ITS STATE
// (ADR 0041). `alerts.state` no longer holds `suppressed` — it holds `firing`
// while a silence is in force, so that every COUNT of firing alerts includes the
// silenced ones. This expression is a DISPLAY reading for one card, which is the
// one place the four-value word is still what a human wants, so it recomposes it
// from the two axes rather than from a column that stopped carrying it.
const caseStateSQL = `CASE WHEN o.state = 'open' AND a.suppression_reason IS NOT NULL THEN 'suppressed'
                           WHEN o.state = 'open' THEN a.state
                           WHEN o.resolve_reason = 'timeout' THEN 'expired'
                           ELSE 'resolved' END`

// alertFactsColumns is the ONE column list `scanAlertFacts` reads, named once
// because two statements fill an `AlertFacts` and a positional scan cannot notice
// that they have drifted apart.
//
// ⭐ IT TAKES THE ACK FROM THE CASE, NEVER FROM A COLUMN ON `alerts`. `alerts`
// has no ack column at all: an ack is a statement about ONE firing episode, and a
// projection onto the Alert keeps asserting it after that episode has closed —
// which is how a September firing arrives pre-acknowledged because somebody acked
// in March. Nobody looks at an acked alert, so that defect costs the alert.
//
// ⭐⭐ THE SNOOZE COMES FROM `alert_snoozes`, AND THE JOIN THAT FETCHES IT IS THE
// POINT OF THE SECOND HALF. Whether oto speaks about this conversation is decided
// from `SnoozedMemberCount` and `FocusSnoozed`; deciding it from a denormalised
// `alerts.snoozed_until` meant the answer came from a bare timestamp maintained by
// a write path in another module, and a bare timestamp cannot name the person who
// asked for quiet, what they wrote, or how the quiet period ended. The
// authoritative row carries all three, one column-list entry away from here.
//
// It costs approximately nothing. `alert_snoozes_active_idx` is
// UNIQUE (alert_id) WHERE ended_at IS NULL, so the relation this LEFT JOIN builds
// from is the snoozes CURRENTLY IN FORCE across the whole tenant — dozens of rows.
// The uniqueness is also what makes it safe: at most one live snooze per alert
// means the join cannot duplicate the row it is attached to.
//
// LEFT, so an awake alert is still returned. `snoozed_until` arrives NULL, which
// is exactly what `AlertFacts.Snoozed` reads as "not quiet".
//
// ⚠️ `coalesce(o.ack_state,'unacked')` RATHER THAN `o.ack_state`, EVEN WHERE THE
// JOIN TO THE CASE IS INNER AND THE COLUMN CANNOT BE NULL. One list serves both
// statements only if it is legal in the one where the case is reached by a LEFT
// JOIN (`focusAlertSQL`, whose focus may be an alert that has never fired), and
// `unacked` is the only safe default there: over-reporting an unacked alert costs
// a glance, under-reporting one costs the alert.
const alertFactsColumns = `
       a.id, a.alert_key, a.source_fingerprint, a.alertname,
       coalesce(a.severity,''), coalesce(a.namespace,''), coalesce(a.service,''),
       a.cluster_key, a.labels, a.annotations, coalesce(a.generator_url,''),
       a.state, coalesce(o.ack_state,'unacked'), z.snoozed_until,
       a.first_seen_at, a.last_seen_at,
       a.total_cases, a.is_flapping, a.flap_score`

// conversationFactsSQL reads the CARD-LEVEL facts of one conversation: the Case
// the delivery is about, and the one Alert that Case is having.
//
// ⛔⛔ IT WAS `groupFactsSQL` PLUS `groupFiringSinceSQL` PLUS `memberAlertsSQL`
// PLUS `memberCountsSQL` — FOUR STATEMENTS OVER `alert_groups` AND ITS MEMBERS —
// AND ALL FOUR ARE DELETED (git-bug `7570090`, migration `00069`). `alert_groups`
// is gone, so the title, the severity, the six counts and the upstream start have
// no row to come from and are projected from the Case and its Alert instead. The
// four collapsed into one statement because the four cardinalities they existed to
// keep apart collapsed first: a conversation holds exactly ONE Case, a Case has
// exactly ONE Alert, and a snooze is unique per alert, so this is one row joined to
// one row joined to at most one row.
//
// ⭐ THAT COLLAPSE IS WHAT MAKES THE COUNTS AND THE MEMBER LIST UNABLE TO
// DISAGREE, and `GroupFacts.AllResolved`'s doc already asked for exactly that: the
// counts are a projection of what oto has recorded, and they must not contradict
// the card they are rendered on. They used to be six stored columns on
// `alert_groups`, maintained by a writer in another module, read here and trusted;
// they are now derived in `readConversation` from the very row that produced the
// Alert beside them. There is no longer a copy to fall behind.
//
// ⛔ `memberCountsSQL` WAS A SECOND ROUND TRIP FOR ONE REASON — "count over EVERY
// current member, not over the capped page `memberAlertsSQL` returns, because the
// suppression decision must not depend on how many instances the renderer happens
// to show" — AND THAT REASON IS NOW STRUCTURAL RATHER THAN PAID FOR. `MaxAlerts`
// cannot truncate a relation of at most one row, so the count and the list are the
// same fact and are read once. The guarantee is unchanged; only its cost is.
//
// ⛔ THE `alert_sources` JOIN IS GONE WITH THE TABLE THAT CARRIED `source_id`, AND
// `GroupFacts.AlertmanagerURL` IS THEREFORE ALWAYS EMPTY. See that field's own
// comment: the kind predicate this query used to apply (`alertmanager` yes, all
// others the empty string) was the whole point of it, and neither `alerts` nor
// `alert_cases` nor `clusters` has a `source_id` for it to apply to. Empty is a
// real answer here, not a missing one, and `service.links` already draws it as no
// link and no Silence button — so the read degrades to the answer it was already
// designed to give for a Grafana source, rather than to a fabricated URL. Restoring
// the affordance needs a column on `alert_cases`, which is a schema change and is
// not this one.
const conversationFactsSQL = `
SELECT ` + alertFactsColumns + `,
       o.seq, o.state, o.state_version, ` + caseStateSQL + `,
       least(o.source_starts_at, o.started_at), o.started_at,
       o.last_observed_at, o.ended_at
  FROM alert_cases o
  JOIN alerts a ON a.id = o.alert_id AND a.org_id = o.org_id
  LEFT JOIN alert_snoozes z
         ON z.alert_id = a.id AND z.org_id = a.org_id AND z.ended_at IS NULL
 WHERE o.org_id = $1 AND o.id = $2`

// focusAlertSQL reads the one Alert the intent is about, with its ack taken from
// the case it is currently having. The focus is reached by `alert_id` rather than
// through the conversation's Case, so the case is the CURRENT case — the same
// episode `include=current_case` expands on the alert list, and the only episode
// about which an ack still says anything true.
//
// ⭐ IT IS STILL A SEPARATE READ FROM `conversationFactsSQL`, AND UNDER ONE-CASE
// THAT IS NO LONGER OBVIOUS, SO: the focus is NOT always the conversation's alert.
// `AlertID` narrows the focus and a caller may name an alert that is not this
// Case's — the policy preview takes a Case and, optionally, an alert — and a card
// that silently rendered the Case's own alert under the heading of the one it was
// asked about would be a lie the reader cannot see. The two reads also answer at
// different `ended_at`: this one has no membership predicate at all, which is what
// makes the focus correct for an alert whose episode has since closed.
//
// Its snooze comes from `alert_snoozes` for the same reason the conversation's
// does, and it MATTERS MORE HERE: a fact about one alert is decided by that
// alert's own snooze (§B.8.1), so this join is the whole input to
// `Snapshot.FocusSnoozed` and therefore to whether oto says anything at all.
const focusAlertSQL = `
SELECT ` + alertFactsColumns + `
  FROM alerts a
  LEFT JOIN alert_cases o ON o.id = a.current_case_id
  LEFT JOIN alert_snoozes z
         ON z.alert_id = a.id AND z.org_id = a.org_id AND z.ended_at IS NULL
 WHERE a.org_id = $1 AND a.id = $2`

const caseByIDSQL = `
SELECT o.id, o.alert_id, o.seq, ` + caseStateSQL + `, coalesce(o.suppression_reason,''),
       coalesce(o.resolve_reason,''), o.started_at, o.ended_at,
       o.ack_state, coalesce(o.acked_by_label,''), o.acked_at, coalesce(o.ack_note,''),
       o.rule_snapshot_id
  FROM alert_cases o
  JOIN alerts a ON a.id = o.alert_id AND a.org_id = o.org_id
 WHERE o.org_id = $1 AND o.id = $2`

// ⛔ `currentCaseSQL` WAS HERE AND IS DELETED (git-bug `7570090`). It read
// `alerts.current_case_id` and was `readCase`'s SECOND ARM, for a query that named
// an alert but no episode. `SnapshotQuery.CaseID` is now REQUIRED — a conversation
// IS a Case — so there is no such query left to serve, and the arm was not merely
// unreachable but actively wrong to keep: the current case of the focus alert is
// frequently NOT the Case this conversation is about, and answering "which episode
// is this card about" with it would render one episode's ack, rule and enrichments
// under another episode's heading. `focusAlertSQL` above still reaches through
// `current_case_id`, deliberately and for a different question — what the FOCUS
// alert's ack says right now — and the difference between those two questions is
// the whole reason this arm is gone rather than repointed.

const ruleSnapshotSQL = `
SELECT id, rule_fingerprint, rule_file, rule_group, rule_name, expr,
       for_seconds, keep_firing_for_seconds, rule_labels, rule_annotations,
       origin, match_confidence, captured_at
  FROM rule_snapshots
 WHERE org_id = $1 AND id = $2`

const previousRuleSnapshotSQL = `
SELECT rs.id, rs.rule_fingerprint, rs.rule_file, rs.rule_group, rs.rule_name,
       rs.expr, rs.for_seconds, rs.keep_firing_for_seconds, rs.rule_labels,
       rs.rule_annotations, rs.origin, rs.match_confidence, rs.captured_at
  FROM alert_cases o
  JOIN rule_snapshots rs ON rs.id = o.rule_snapshot_id
 WHERE o.org_id = $1 AND o.alert_id = $2 AND o.seq < $3
   AND o.rule_snapshot_id IS NOT NULL
 ORDER BY o.seq DESC
 LIMIT 1`

// MaxTrailEntries bounds the state trail rendered on a card (§H.4).
//
// A long-lived flapping alert would otherwise grow the trail without bound and
// blow the section budget; the renderer elides the middle rather than the end,
// because the first transition and the last are the two a reader needs.
const MaxTrailEntries = 12

// caseTrailSQL reads the Case's own state history.
//
// ⛔ IT WAS `groupTrailSQL`, KEYED ON `group_id`, AND IT CARRIED TWO MORE TYPES
// (git-bug `7570090`). `group.opened` and `group.closed` were oto's own bookkeeping
// about a generation, and they leave this list for a reason that has nothing to do
// with the entity being deleted: an event about a group names `group_id` and NOT
// `case_id`, so on this key they are two more values in a predicate that cannot
// match a row. `ev_case_idx` (org_id, case_id, recorded_at DESC, id DESC) serves
// this exactly, which `ev_group_idx` did for the old key.
//
// ⚠️ THE TWO TYPES ARE NOT DEAD VOCABULARY, so do not "finish the job" by deleting
// them from the kernel enum on the strength of this line. Migration `00069` keeps
// `alert_events.group_id` and `ev_subject_ck` exactly as they are — READABLE,
// UNWRITABLE, the 00051/00054 bargain — because a thirteen-month retention of
// append-only history is a record of what happened and not a mirror of what exists.
// Those rows still validate and still render in the timeline. They are simply not
// this card's receipt.
//
// The type list is CLOSED and short on purpose: this is the card's receipt, not
// the timeline. `alert.mutated`, the enrichment events and the notification
// events are all real history and all belong in oto's timeline view; putting
// them here would turn a four-line trail into a scrollback and teach people to
// ignore it.
//
// Ordered by `recorded_at` — oto's clock, which is the causal order — and
// DISPLAYED by `occurred_at`, which is upstream's. Conflating the two is how a
// skewed cluster gets a trail that reads backwards.
//
// ⛔ THE SIXTEEN VALUES BELOW ARE A COPY OF THE KERNEL'S ENUM, IN SQL, AND THEY
// CANNOT BE ANYTHING ELSE. They are part of a predicate Postgres plans — the walk
// is `ev_case_idx` plus this filter — not of the parameter list, so there is no
// `EventType` to bind here the way `readCause` binds one. What that costs is
// exactly the hazard `causeEventTypes` below describes: rename a value in SPEC
// §D.4.1, miss this line, and the trail silently goes empty, because a read that
// returns no rows is indistinguishable from a Case that never changed state.
// `test/arch.TestEventTypeSQLNamesLiveValues` is the thing that notices — this
// package is registered in `eventTypeSQLSites`, and the gate reads INSIDE this
// literal and fails if any value in it has left the enum.
const caseTrailSQL = `
SELECT type, occurred_at, coalesce(actor_label,'')
  FROM alert_events
 WHERE org_id = $1 AND case_id = $2
   AND type IN ('case.opened','case.reopened','case.suppressed',
                'case.unsuppressed','case.resolved','case.expired',
                'case.acknowledged','case.unacknowledged',
                -- vocab:allow -- pre-ADR-0036 spellings of the same eight facts, still on disk for thirteen months (alerts/domain.legacySpellings, migration 00052). Dropping them here would empty the state trail of any Case that opened before the rename.
                'occurrence.opened','occurrence.reopened','occurrence.suppressed','occurrence.unsuppressed',
                'occurrence.resolved','occurrence.expired','occurrence.acknowledged','occurrence.unacknowledged')
 ORDER BY recorded_at DESC, id DESC
 LIMIT $3`

// causeEventTypes maps a Reason onto the `alert_events` type whose row IS that
// fact — the one entry on the timeline that knows who caused it and, for a
// comment, what they said.
//
// ⛔ IT IS FOUR ENTRIES AND NOT THE WHOLE REASON ENUM, and the absences are the
// design. A reason with no entry costs NOTHING — no row, no round trip — and
// `fired`, `repeat`, `all_resolved` and the rest are caused by the world rather
// than by anybody, so there is no name to fetch and no sentence on the card that
// would carry one. What is here is exactly the set the renderer attributes:
// §E.1.1's human verbs as a card renders them (an acknowledgement, its withdrawal,
// a comment) plus the silence, which is attributed too and whose answer is that no
// human at oto caused it.
//
// ⛔ `snoozed` AND `unsnoozed` ARE DELIBERATELY ABSENT. A snooze is a fact about
// oto's own notifications, the Slack renderer has no `snoozed` card at all, and
// a query whose result nothing renders is a query nobody should pay for.
// ⚠️ THE VALUES ARE THE KERNEL'S ENUM, NOT FOUR MORE STRING LITERALS. They used to
// be spelled out here, and a typo in one produced a query that silently matched
// nothing — worse than a bad write, because a read that returns no rows looks
// exactly like a fact that never happened. Binding the value gates the TYPO: a
// constant that has left the enum is a compile error.
//
// ⛔ IT DOES NOT GATE THE RENAME, AND AN EARLIER VERSION OF THIS COMMENT CLAIMED
// THE HAZARD WAS "GONE RATHER THAN GATED". Three of these four values were renamed
// by ADR 0036, and 00052 deliberately did NOT rewrite `alert_events` — so an ack
// written before the rename is on disk as `occurrence.acknowledged` and a predicate
// built from `.String()` alone matches ZERO of them, for the whole thirteen-month
// retention. `readCause` therefore binds `PersistedSpellings()` and the statement
// says `type = ANY($3)`; `internal/alerts/domain.EventType` states the rule ("USE
// IT FOR EVERY PREDICATE ON alert_events.type, never String()") and this is a
// predicate, whether the value arrives as a literal or as a parameter.
// `test/arch.TestEventTypeSQLNamesLiveValues` cannot see this one: it reads SQL
// string literals, and a bound parameter has none.
//
// ⚠️ IT IS NOT TRUE OF THIS WHOLE FILE, AND AN EARLIER VERSION OF THIS COMMENT
// IMPLIED IT WAS. `caseTrailSQL`, forty lines up, still spells sixteen of these
// values out — it must, because they sit in a predicate rather than a parameter —
// so `case.acknowledged` IS still written in Go in this package. What
// changed is that the copy is now registered and checked: see that statement's own
// comment and `test/arch.TestEventTypeSQLNamesLiveValues`.
var causeEventTypes = map[domain.Reason]kernel.EventType{
	domain.ReasonAcked:      kernel.EventCaseAcknowledged,
	domain.ReasonUnacked:    kernel.EventCaseUnacknowledged,
	domain.ReasonComment:    kernel.EventCommentAdded,
	domain.ReasonSuppressed: kernel.EventCaseSuppressed,
}

// causeByCaseSQL reads the ONE event that caused the fact being rendered: the
// newest of its type against the Case the notification is about.
//
// ⛔⛔ IT WAS THREE STATEMENTS — BY CASE, BY ALERT, BY GROUP — SELECTED BY WHICH
// ID THE QUERY NAMED, AND IT IS NOW ONE (git-bug `7570090`). THE DEFECT THE ARMS
// EXISTED TO PREVENT HAS NOT GONE AWAY; the narrowest subject just became the only
// subject. The group arm returned whichever MEMBER of the generation was acted on
// last, so a card about Ada's alert rendered "Acknowledged by grace" — and for
// `comment` it dragged the sibling's WORDS across too. `alert_groups` is deleted
// and there are no siblings, so that arm has no question to answer.
//
// ⛔ AND THE ALERT ARM IS DELETED FOR THE SAME REASON ONE LEVEL DOWN, WHICH IS
// WHY IT MUST NOT COME BACK AS A FALLBACK. `alert_id` is not narrower than a Case,
// it is WIDER: an Alert has many Cases over its life, so the newest ack for the
// alert may belong to March while this card is about September's firing. That is
// the same "somebody else's receipt on this card" defect, and it is the one
// `AlertFacts.AckState` is written to avoid.
//
// ⭐ IT IS ALSO REACHABLE FOR ALL FOUR REASONS, WHICH IS WHY NO FALLBACK IS
// NEEDED. `acked`, `unacked` and `suppressed` are case events and carry
// `case_id` by construction. `comment` is the one that looks doubtful —
// `alerts/service.Comment` sets `params.CaseID` only when the alert has an open
// case — but it enqueues the notification inside the same `if hasOpen`, so a
// `comment` intent exists exactly when the event it names carries the id this
// statement keys on. A comment left on an alert with no open case appends an event
// and sends nothing, which is a decision made in `alerts`, not a gap here.
//
// It rides `ev_case_idx` (org_id, case_id, recorded_at DESC, id DESC) as a LIMIT 1
// walk backwards from the newest row.
//
// `payload->>'body'` is the comment's text and is NULL for the other three
// types, which is why one statement serves all four: the body column is simply
// empty for a fact that has no text.
//
// ⛔ `type = ANY($3)` AND NOT `type = $3`. $3 is every string the column may hold
// for this fact — `PersistedSpellings()`, canonical plus any pre-ADR-0036 spelling
// — because 00052 left thirteen months of `occurrence.*` rows on disk. With the
// equality, an ack from before the rename returned `pgx.ErrNoRows`, `readCause`
// gave up silently, and the card rendered an acknowledgement with NO ACKER NAME,
// indistinguishable from an ack nobody made. `ANY` over a one-to-two element array
// is still an index range per element on `ev_case_idx`, so the LIMIT 1 walk
// backwards from the newest row is unchanged.
const causeByCaseSQL = `
SELECT actor_kind, coalesce(actor_id,''), coalesce(actor_label,''),
       coalesce(payload->>'body','')
  FROM alert_events
 WHERE org_id = $1 AND case_id = $2 AND type = ANY($3)
 ORDER BY recorded_at DESC, id DESC
 LIMIT 1`

// caseNotificationsSQL counts what oto has SAID in this conversation.
//
// "How loud was this?" is a question about oto's own behaviour, and oto is the
// only thing that can answer it — it belongs on the receipt beside how long the
// outage lasted. Suppressed intents are excluded: they are notifications oto
// decided NOT to send, and counting them would report noise that never happened.
//
// ⭐ IT KEYS ON THE CONVERSATION, THE DELIVERY TARGET, AND NOT ON THE SUBJECT, AND
// THAT DISTINCTION IS THE WHOLE POINT — it survived the container being deleted
// unchanged. "How loud was this thread" must count the acks, snoozes and comments
// that were POSTED INTO it, and since migration 00056 those rows declare `case` or
// `alert` as their SUBJECT. A subject-keyed count would still be wrong today for
// exactly the reason it was wrong then: an `alert`-subject row about this Case's
// own alert carries `subject_id = <alert id>`, which no predicate on the Case's id
// matches, so the number a user reads would be smaller than what they watched
// arrive.
//
// ⛔ IT WAS `WHERE group_id = $2`, AND `notifications.group_id` IS DELETED WITH
// `alert_groups` (git-bug `7570090`, migration `00069`). The replacement is the
// pair migration 00064 introduced for precisely this succession:
// `(conversation_kind, conversation_id)`. `notification/repository.Insert` already
// stopped writing `group_id`, so this count would have started shrinking toward
// zero on its own — a silent receipt, not an error.
//
// ⚠️ THE KIND IS BOUND, NOT SPELLED. `domain.ConversationCase` is the vocabulary's
// owner; a literal `'case'` here is a second copy that a rename cannot reach, and
// the failure mode is a count that quietly returns zero forever.
//
// ⚠️ `notif_conversation_idx (org_id, conversation_id, created_at DESC)` SERVES
// THIS, and migration `00069` created it in the same change that dropped
// `notif_group_idx` — whose leading column no longer exists. The index does NOT
// carry `conversation_kind`, so the kind arrives as a recheck on the rows the index
// already narrowed to one conversation. Keeping the kind in the predicate anyway is
// deliberate: `conversation_id` is a bare UUID with no FK and holds an
// `alert_cases.id` or a `notification_policies.id` depending on the kind, so a
// predicate that omits it is one collision away from counting somebody else's
// digest. The count is unbounded in time and reads the whole conversation's range,
// which is why `created_at` comes last in the index and this query names no window.
const caseNotificationsSQL = `
SELECT count(*)
  FROM notifications
 WHERE org_id = $1 AND conversation_kind = $2 AND conversation_id = $3
   AND status <> 'suppressed'`

const enrichmentsSQL = `
SELECT enricher, status, payload, warnings, coalesce(error,''), computed_at
  FROM enrichments
 WHERE org_id = $1 AND subject_kind = 'case' AND subject_id = $2
 ORDER BY enricher ASC`

// Snapshot builds the whole read model for one delivery, AT CLAIM TIME.
//
// ⛔ IT WAS EIGHT READS AND IS NOW SIX (git-bug `7570090`). `readGroup` and
// `readMembers` were two questions about two entities with two cardinalities;
// a conversation holds exactly one Case and a Case has exactly one Alert, so they
// are one question about one row and `readConversation` is both of them.
//
// It is still several round trips on purpose rather than one heroic join, and the
// reason CHANGED with the shape: the cardinalities no longer differ, so what keeps
// these apart is that each answers a different question with a different
// DEGRADATION POLICY. The conversation must exist or there is nothing to render;
// the rule and the enrichments may be absent; the trail, the actor and the
// notification count degrade to empty rather than failing a delivery. Folding them
// together would force one policy on all of them. Delivery is not the hot path —
// ingestion is — and this code is read far more often than it runs.
func (r *SnapshotRepository) Snapshot(
	ctx context.Context, s db.TenantScope, q domain.SnapshotQuery,
) (domain.Snapshot, error) {
	now := r.clk.Now().UTC()
	snap := domain.Snapshot{TakenAt: now, SnoozedAlerts: map[uuid.UUID]time.Time{}}

	if err := r.readOrg(ctx, s, &snap); err != nil {
		return domain.Snapshot{}, err
	}
	if err := r.readConversation(ctx, s, q.CaseID, now, &snap); err != nil {
		return domain.Snapshot{}, err
	}
	if err := r.readFocus(ctx, s, q, now, &snap); err != nil {
		return domain.Snapshot{}, err
	}
	if err := r.readCase(ctx, s, q, &snap); err != nil {
		return domain.Snapshot{}, err
	}
	r.readTrail(ctx, s, q.CaseID, &snap)
	r.readCause(ctx, s, q, &snap)
	r.readNotificationCount(ctx, s, q.CaseID, &snap)
	return snap, nil
}

// readCause loads WHO caused the fact being rendered, and what they said.
//
// ⭐ IT READS THE RECORD RATHER THAN CARRYING A COPY, and that is the whole
// choice. The actor and the comment body are already written, exactly once, by
// the module that owns the human verb — in the same transaction that enqueued
// this notification, so they are on disk before any delivery can claim it — and
// `alert_events.actor_label` is denormalised there precisely so a renamed or
// deleted user never rewrites what a card said. Copying either onto the
// notification row would be a second answer to the same question, and two
// answers can disagree.
//
// ⛔ ONE ROUND TRIP, AND ONLY FOR THE REASONS SOMEBODY CAUSES. A reason with no
// entry in `causeEventTypes` returns before touching the database, so the common
// delivery path — fired, repeat, resolved, enriched — pays nothing at all. It is
// not an N+1 either: a snapshot is built once per delivery (C11), and this is one
// indexed `LIMIT 1` beside the round trips already above it.
//
// ⛔ IT NO LONGER NARROWS, AND `readCase` NO LONGER NARROWS EITHER. The two used
// to select between a case arm and an alert arm and the comment here said "they
// must not drift"; both are now single-armed on `q.CaseID`, so the rule is kept by
// there being nothing left to choose. The reasoning that made drift dangerous is on
// `causeByCaseSQL`, where it still applies to the arms that were removed.
//
// A failure DEGRADES to no actor, exactly like the trail: a card that cannot
// name the acker is a small loss, and a card that never renders is an alert
// nobody sees.
func (r *SnapshotRepository) readCause(
	ctx context.Context, s db.TenantScope, q domain.SnapshotQuery, snap *domain.Snapshot,
) {
	eventType, ok := causeEventTypes[q.Reason]
	if !ok {
		return
	}

	var (
		actor domain.ActorFacts
		body  string
	)
	// ⛔ `PersistedSpellings()`, never `String()`. See `causeByCaseSQL`: three of the
	// four types in `causeEventTypes` were renamed by ADR 0036, 00052 left the old
	// rows spelled the old way, and a predicate over the canonical value alone is
	// valid SQL that stops matching everything written before the rename.
	if err := r.db(ctx).QueryRow(ctx, causeByCaseSQL, s.OrgID(), q.CaseID,
		eventType.PersistedSpellings()).
		Scan(&actor.Kind, &actor.ID, &actor.Label, &body); err != nil {
		return
	}
	snap.Actor = &actor
	snap.Comment = body
}

// readNotificationCount records how many notifications oto has sent in this
// conversation. It degrades to zero, which the renderer suppresses (S11).
func (r *SnapshotRepository) readNotificationCount(
	ctx context.Context, s db.TenantScope, caseID uuid.UUID, snap *domain.Snapshot,
) {
	var n int
	if err := r.db(ctx).QueryRow(ctx, caseNotificationsSQL,
		s.OrgID(), string(domain.ConversationCase), caseID).Scan(&n); err == nil {
		snap.NotificationCount = n
	}
}

// readTrail loads the Case's state history for the card's receipt (§H.4).
//
// A failure DEGRADES to no trail rather than failing the snapshot. The trail is
// what makes a resolved card legible; a card with no trail is a regression, and a
// card that never renders is an alert nobody sees.
func (r *SnapshotRepository) readTrail(
	ctx context.Context, s db.TenantScope, caseID uuid.UUID, snap *domain.Snapshot,
) {
	rows, err := r.db(ctx).Query(ctx, caseTrailSQL, s.OrgID(), caseID, MaxTrailEntries)
	if err != nil {
		return
	}
	defer rows.Close()

	var out []domain.TransitionFact
	for rows.Next() {
		var f domain.TransitionFact
		if err := rows.Scan(&f.Type, &f.At, &f.ActorLabel); err != nil {
			return
		}
		out = append(out, f)
	}
	if rows.Err() != nil {
		return
	}
	// The query reads newest-first so the LIMIT keeps the RECENT end; the card
	// reads oldest-first because that is the order the story happened in.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	snap.Trail = out
}

func (r *SnapshotRepository) readOrg(
	ctx context.Context, s db.TenantScope, snap *domain.Snapshot,
) error {
	err := r.db(ctx).QueryRow(ctx, orgFactsSQL, s.OrgID()).
		Scan(&snap.Org.ID, &snap.Org.Slug, &snap.Org.Name)
	return mapErr(err, "org_not_found", "org")
}

// readConversation fills the card-level facts, the one-element member list and
// the member counts from ONE row: the Case, its Alert, and any snooze in force.
//
// ⛔ IT WAS `readGroup` PLUS `readMembers` (git-bug `7570090`), AND THE FAILURE
// CODE MOVED WITH IT. A missing `alert_groups` row failed the snapshot as
// `group_not_found`; a missing Case fails it as `case_not_found`. It still FAILS
// rather than degrades, and that is deliberate and unchanged in spirit: the
// conversation is the subject, and a card rendered without its subject would be a
// blank message sent to a channel. `service.ViewService` documents a nil id coming
// back as `group_not_found` and retrying twelve times; the retry behaviour is the
// same, the code a client reads is not, and that comment is the view service's to
// correct.
//
// ⭐ THE MEMBER LIST IS ONE ELEMENT, NOT ZERO, AND THAT IS A CONTRACT WITH FILES
// THIS ONE DOES NOT OWN. Three readers depend on the slice existing:
// `service.ViewService.alerts` builds the card's instance list from it,
// `service.links` falls back to `snap.Alerts[0]` for the runbook, Prometheus and
// Grafana links whenever there is no focus, and `Snapshot.AnyFlapping` walks it.
// The counts matter more still: `notify.go` decides quiet with
// `AllMembersSnoozed()` whenever `Focus` is nil, and that predicate reads
// `MemberCount > 0` — so a `MemberCount` of zero would not merely render a thinner
// card, it would turn every snooze into a no-op on the one path that has no focus
// to fall back on. Collapsing the list to nothing and asking the renderer to read
// `Focus` instead would have moved that decision into three files owned by three
// other people, in the same change that deleted the entity underneath them.
//
// ⛔ SO `MaxAlerts` IS INERT AND ITS PARAMETER IS GONE, BUT `Alerts` KEEPS ITS
// SHAPE. The renderer's "and N more" clause reads `TotalCount - shown`, which is
// now 1 - 1 = 0 on a live card and says nothing, which is correct.
//
// ⭐ AND THE LIST IS EMPTY ONCE THE EPISODE HAS ENDED, WHICH IS NOT AN OVERSIGHT.
// `memberAlertsSQL` carried `o.ended_at IS NULL` as its membership predicate — the
// card lists what is CURRENTLY firing, not what ever fired — so a group whose
// members had all resolved rendered no instances. Keeping that predicate keeps a
// terminal card byte-identical in shape to today's. Dropping it would look like an
// improvement (a resolved card would gain its runbook link) and would silently
// change a suppression decision: a resolved Case whose alert still has a live
// snooze would report `MemberCount == SnoozedMemberCount == 1`, `AllMembersSnoozed`
// would become true, and the resolution notification — the one message that closes
// the loop for whoever was woken up — would be suppressed. That is a product
// decision with a much longer argument than a read model gets to make.
func (r *SnapshotRepository) readConversation(
	ctx context.Context, s db.TenantScope, caseID uuid.UUID, now time.Time,
	snap *domain.Snapshot,
) error {
	var (
		a                  domain.AlertFacts
		labels, annotation []byte
		g                  = &snap.Group
		display            string
		firingSince        time.Time
		endedAt            *time.Time
	)
	err := r.db(ctx).QueryRow(ctx, conversationFactsSQL, s.OrgID(), caseID).Scan(
		&a.ID, &a.AlertKey, &a.SourceFingerprint, &a.AlertName, &a.Severity,
		&a.Namespace, &a.Service, &a.ClusterKey, &labels, &annotation,
		&a.GeneratorURL, &a.State, &a.AckState, &a.SnoozedUntil,
		&a.FirstSeenAt, &a.LastSeenAt, &a.TotalCases, &a.IsFlapping, &a.FlapScore,
		&g.Generation, &g.State, &g.StateVersion, &display,
		&firingSince, &g.FirstSeenAt, &g.LastActivityAt, &endedAt,
	)
	if err != nil {
		return mapErr(err, "case_not_found", "case")
	}
	a.Labels = decodeStringMap(labels)
	a.Annotations = decodeStringMap(annotation)

	// ⭐ THE CARD-LEVEL FACTS ARE THE CASE'S AND THE ALERT'S, NOT A GROUP ROW'S.
	// `GroupFacts` documents each of these mappings on the field itself, including
	// the five that are now permanently empty because nothing in the schema can
	// answer them any more.
	g.ID = caseID
	g.Title = a.AlertName
	g.Severity = a.Severity
	g.ClusterKey = a.ClusterKey
	g.GroupLabels = a.Labels
	g.FiringSince = firingSince.UTC()
	g.ClosedAt = endedAt

	// The six counts, projected over the ONE Alert this Case is having. `display`
	// is `caseStateSQL`'s four-value reading, so exactly one of the first four is
	// 1 and the other three are 0 — which is what keeps `AllResolved()` answering
	// the same question it always did, one member wide.
	switch display {
	case "firing":
		g.FiringCount = 1
	case "suppressed":
		g.SuppressedCount = 1
	case "resolved":
		g.ResolvedCount = 1
	case "expired":
		g.ExpiredCount = 1
	}
	g.TotalCount = 1
	if a.AckState == "acked" {
		g.AckedCount = 1
	}

	// Membership, and the clock guard on the snooze. `ended_at IS NULL` is the
	// predicate `memberAlertsSQL` applied in SQL; it is applied here instead
	// because this statement must return the row either way — the card-level facts
	// above are needed for a terminal card too.
	if endedAt != nil {
		return nil
	}
	snap.Alerts = append(snap.Alerts, a)
	snap.MemberCount = 1
	// ⚠️ `After(now)` AND NOT MERELY `ended_at IS NULL`. The snooze row is live
	// until the 60-second expiry sweep ends it, so for up to a minute its clock has
	// run out while the column still says otherwise. Counting such a row would hold
	// a card back for a quiet period that has already finished. This is the same
	// guard `readFocus` applies, and the two must give the same answer because
	// `notify.go` chooses between them on whether a focus exists.
	if a.Snoozed(now) {
		snap.SnoozedAlerts[a.ID] = *a.SnoozedUntil
		snap.SnoozedMemberCount = 1
	}
	return nil
}

func (r *SnapshotRepository) readFocus(
	ctx context.Context, s db.TenantScope, q domain.SnapshotQuery, now time.Time,
	snap *domain.Snapshot,
) error {
	if q.AlertID == nil {
		return nil
	}
	// ⛔ THE FOCUS IS STILL OPTIONAL, AND FILLING IT FROM THE CASE'S OWN ALERT
	// WOULD NOT BE A TIDY-UP. A Case has exactly one Alert, so this read is
	// derivable — and `Focus != nil` is a SIGNAL, not a convenience: `notify.go`
	// switches its quiet test on it and `ViewService` adds a whole focus section to
	// the card when it is set. Making it unconditional would put a focus block on
	// every card oto sends, which is a rendering decision this file does not get to
	// take. `AlertID` narrows the focus, exactly as `SnapshotQuery` says.
	//
	// Reading it separately is also what makes the card correct for an alert that
	// is not this Case's own, which is precisely when a wrong card is most
	// misleading.
	a, err := scanAlertFacts(r.db(ctx).QueryRow(ctx, focusAlertSQL, s.OrgID(), *q.AlertID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return mapErr(err, "alert_not_found", "focus alert")
	}
	snap.Focus = &a
	// ⚠️ THE `After(now)` GUARD IS THE SAME ONE `readConversation` APPLIES, and
	// until this change only one of the two had it. `alert_snoozes.ended_at IS NULL`
	// means "not yet swept", not "still in force": the expiry job runs every 60
	// seconds, so for up to a minute a row is live and its clock has run out. A
	// map entry written without this guard would report the focus as quiet after
	// oto should already be speaking again.
	if a.Snoozed(now) {
		snap.SnoozedAlerts[a.ID] = *a.SnoozedUntil
	}
	return nil
}

// readCase loads the firing episode the card is about.
//
// ⛔ IT WAS TWO ARMS, BY CASE AND BY ALERT, AND IS NOW ONE (git-bug `7570090`).
// See `currentCaseSQL`'s epitaph above: `CaseID` is required, so "which episode is
// this card about" has one answer, and the alert's CURRENT episode is frequently
// not it.
//
// It still degrades to no Case on `ErrNoRows` rather than failing. That arm is
// unreachable while `readConversation` runs first over the same row and fails
// hard on the same absence — it is kept because the two statements are separate
// reads and a degradation policy is not something to remove on the grounds that
// nothing currently exercises it.
func (r *SnapshotRepository) readCase(
	ctx context.Context, s db.TenantScope, q domain.SnapshotQuery, snap *domain.Snapshot,
) error {
	var (
		ac       domain.CaseFacts
		alertID  uuid.UUID
		ruleSnap *uuid.UUID
	)
	err := r.db(ctx).QueryRow(ctx, caseByIDSQL, s.OrgID(), q.CaseID).Scan(
		&ac.ID, &alertID, &ac.Seq, &ac.State, &ac.SuppressionReason,
		&ac.ResolveReason, &ac.StartedAt, &ac.EndedAt,
		&ac.AckState, &ac.AckedByLabel, &ac.AckedAt, &ac.AckNote, &ruleSnap,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return mapErr(err, "case_not_found", "case")
	}
	snap.Case = &ac

	if err := r.readEnrichments(ctx, s, ac.ID, snap); err != nil {
		return err
	}
	if ruleSnap == nil {
		return nil
	}
	return r.readRule(ctx, s, *ruleSnap, alertID, ac.Seq, snap)
}

func (r *SnapshotRepository) readRule(
	ctx context.Context, s db.TenantScope,
	snapshotID, alertID uuid.UUID, seq int, snap *domain.Snapshot,
) error {
	current, err := scanRuleFacts(r.db(ctx).QueryRow(ctx, ruleSnapshotSQL, s.OrgID(), snapshotID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return mapErr(err, "rule_snapshot_not_found", "rule snapshot")
	}
	snap.Rule = &current

	previous, err := scanRuleFacts(r.db(ctx).QueryRow(ctx, previousRuleSnapshotSQL,
		s.OrgID(), alertID, seq))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return mapErr(err, "rule_snapshot_not_found", "previous rule snapshot")
	}
	if previous.Fingerprint == current.Fingerprint {
		// Same content address, same rule. `rule_fingerprint` is a content address
		// (§C.6) precisely so this comparison is exact rather than heuristic.
		return nil
	}
	snap.RuleChange = diffRules(previous, current)
	return nil
}

func (r *SnapshotRepository) readEnrichments(
	ctx context.Context, s db.TenantScope, caseID uuid.UUID, snap *domain.Snapshot,
) error {
	rows, err := r.db(ctx).Query(ctx, enrichmentsSQL, s.OrgID(), caseID)
	if err != nil {
		return mapErr(err, "enrichment_not_found", "list enrichments")
	}
	defer rows.Close()

	for rows.Next() {
		var (
			e       domain.EnrichmentFacts
			payload []byte
		)
		if err := rows.Scan(&e.Enricher, &e.Status, &payload, &e.Warnings,
			&e.Error, &e.ComputedAt); err != nil {
			return mapErr(err, "enrichment_not_found", "scan an enrichment")
		}
		if len(payload) > 0 {
			_ = json.Unmarshal(payload, &e.Payload)
		}
		if snap.Enrichments == nil {
			snap.Enrichments = map[string]domain.EnrichmentFacts{}
		}
		snap.Enrichments[e.Enricher] = e
	}
	return mapErr(rows.Err(), "enrichment_not_found", "read enrichments")
}

func scanAlertFacts(row pgx.Row) (domain.AlertFacts, error) {
	var (
		a                  domain.AlertFacts
		labels, annotation []byte
	)
	err := row.Scan(
		&a.ID, &a.AlertKey, &a.SourceFingerprint, &a.AlertName, &a.Severity,
		&a.Namespace, &a.Service, &a.ClusterKey, &labels, &annotation,
		&a.GeneratorURL, &a.State, &a.AckState, &a.SnoozedUntil,
		&a.FirstSeenAt, &a.LastSeenAt, &a.TotalCases, &a.IsFlapping, &a.FlapScore,
	)
	if err != nil {
		return domain.AlertFacts{}, err
	}
	a.Labels = decodeStringMap(labels)
	a.Annotations = decodeStringMap(annotation)
	return a, nil
}

func scanRuleFacts(row pgx.Row) (domain.RuleFacts, error) {
	var (
		f                   domain.RuleFacts
		forS, keepS         float64
		labels, annotations []byte
	)
	err := row.Scan(
		&f.SnapshotID, &f.Fingerprint, &f.File, &f.Group, &f.Name, &f.Expr,
		&forS, &keepS, &labels, &annotations, &f.Origin, &f.MatchConfidence, &f.CapturedAt,
	)
	if err != nil {
		return domain.RuleFacts{}, err
	}
	f.For = time.Duration(forS * float64(time.Second))
	f.KeepFiringFor = time.Duration(keepS * float64(time.Second))
	f.Labels = decodeStringMap(labels)
	f.Annotations = decodeStringMap(annotations)
	return f, nil
}

// diffRules is the headline differentiator's payload: what actually changed in
// the rule between two cases of the same alert.
//
// It compares the DEFINITION, not the rendered text: an expression that was
// reformatted is not a change an operator needs woken up about, and one whose
// threshold moved from 0.05 to 0.03 is.
func diffRules(previous, current domain.RuleFacts) *domain.RuleChangeFacts {
	d := &domain.RuleChangeFacts{
		PreviousSnapshotID:  previous.SnapshotID,
		PreviousFingerprint: previous.Fingerprint,
		PreviousCapturedAt:  previous.CapturedAt,
		ExprChanged:         previous.Expr != current.Expr,
		PreviousExpr:        previous.Expr,
		NewExpr:             current.Expr,
		ForChanged:          previous.For != current.For,
		PreviousFor:         previous.For,
		NewFor:              current.For,
		LabelDiff:           diffMaps(previous.Labels, current.Labels),
		AnnotationDiff:      diffMaps(previous.Annotations, current.Annotations),
	}
	return d
}

func diffMaps(before, after map[string]string) map[string][2]string {
	out := map[string][2]string{}
	for k, v := range before {
		if av, ok := after[k]; !ok || av != v {
			out[k] = [2]string{v, after[k]}
		}
	}
	for k, v := range after {
		if _, ok := before[k]; !ok {
			out[k] = [2]string{"", v}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// decodeStringMap turns a JSONB object into a label map. A decode failure yields
// an empty map rather than an error: a card missing one label is far better than
// a card that never renders, and the raw row is still on disk for anyone asking
// why.
func decodeStringMap(b []byte) map[string]string {
	if len(b) == 0 {
		return map[string]string{}
	}
	out := map[string]string{}
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]string{}
	}
	return out
}
