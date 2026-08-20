package domain

import (
	"time"

	"github.com/google/uuid"
)

// This file is the READ MODEL a Notification is rendered from. It is DATA: no
// invariants, no constructors, no behaviour beyond a couple of convenience
// predicates. The invariants live on the entities these facts are projected
// from, in the modules that own them, and duplicating one here would give two
// answers to the same question.
//
// It exists because of C11: a delivery is rendered AT CLAIM TIME, not at enqueue
// time. What is queued is an INTENT; what is sent is the world as it is when the
// message actually goes out. That is why an alert which fired and resolved
// entirely inside a snooze window produces no stale card when the snooze ends,
// and it is why this type is a snapshot rather than a payload.

// SnapshotQuery asks for everything one delivery needs to render.
type SnapshotQuery struct {
	// CaseID is the firing episode being snapshotted, and it is the CONVERSATION.
	//
	// ⛔ IT WAS `GroupID uuid.UUID` REQUIRED PLUS `CaseID *uuid.UUID` OPTIONAL
	// (git-bug `7570090`). The Case was a narrowing of the group; it is now the
	// subject itself, so the two collapsed into one required field. `AlertID` stays
	// optional and still means what it meant.
	CaseID uuid.UUID
	// AlertID narrows the FOCUS: the one alert an ack, a re-fire or a rule change
	// is about.
	AlertID *uuid.UUID
	// ⛔ `MaxAlerts int` WAS HERE AND IS DELETED (git-bug `7570090`). It capped the
	// member list the snapshot query fetched: the renderer showed at most this many
	// instances inline and then said "and N more", and fetching more than it could
	// show was work nobody saw. A Case has exactly one Alert, so the list it capped
	// is at most one row and no statement passes it to a `LIMIT` any more.
	//
	// ⚠️ AN EARLIER NOTE ON THIS FIELD ARGUED IT SHOULD SURVIVE "because the view
	// service sets it from `MaxInstances`, which is a published channel setting;
	// retiring the setting is a config change, not a read-model one". THAT ARGUMENT
	// WAS WRONG ON ITS FACTS and is recorded here so it is not made again. The
	// published setting is `slack.config.max_instances`, and it reaches the card
	// through `RenderOptions.MaxInstances`, which is untouched and still bounds what
	// the Slack and webhook renderers print. `notification/service.ViewConfig.MaxInstances`
	// was a SEPARATE, UNWIRED copy — `container.go` never set it — so deleting this
	// field retires no setting and changes no card.
	// Reason is the fact about to be RENDERED, and it is the only thing that can
	// name the timeline entry which caused it: `acked` means
	// `case.acknowledged`, `comment` means `comment.added`. Without it a
	// read model can fill in the world but not the actor, because "who did this"
	// is a question about ONE event and the subject ids alone do not say which.
	//
	// EMPTY IS A REAL ANSWER: it means the caller is not rendering — the
	// evaluation path reads a snapshot to decide whether to send at all — and a
	// reader that is not rendering must not pay for the lookup.
	Reason Reason
}

// OrgFacts identifies the tenant.
type OrgFacts struct {
	ID   uuid.UUID
	Slug string
	Name string
}

// GroupFacts is the CARD-LEVEL facts of one conversation — the thing that owns
// exactly one thread per destination.
//
// ⛔⛔ IT WAS ONE `alert_groups` GENERATION, AND `alert_groups` IS DELETED
// (git-bug `7570090`, migration `00069`). A conversation holds exactly ONE Case and
// a Case has exactly one Alert, so the Case is now the thing that owns the thread,
// and every field below is projected from the Case and that Alert by
// `repository.readConversation`. The invariant the type was named for is unchanged:
// it is the one row per card that the members hang off.
//
// ⭐ THE NAME IS RETAINED DELIBERATELY, AND IT IS NOW A LIE OF CONVENIENCE. Both
// `GroupFacts` and `Snapshot.Group` are read by the view service, the Slack
// renderer, the webhook renderer and the API DTOs, none of which this change owns;
// renaming the type here would be a mechanical edit across four packages that are
// mid-flight in the same stage, and it would hide the semantic changes below inside
// a diff nobody can review. The rename is worth doing and is not this change.
//
// ⛔ FIVE FIELDS ARE NOW PERMANENTLY EMPTY, AND EACH SAYS SO ON ITSELF:
// `GroupKey`, `Receiver`, `SourceGroupKey`, `NotificationReason` and
// `AlertmanagerURL`. They described the Alertmanager ROUTE that produced a
// generation — its key, its receiver, its wire groupKey, its last wire reason — or
// the source that route came from, and none of those questions has a row to be
// asked of any more. Every consumer already treats the empty string as an answer,
// which is why they are left at their zero value rather than deleted: deleting
// them is the same cross-package rename as above.
type GroupFacts struct {
	// ID is the CONVERSATION's id, which is now `alert_cases.id`.
	//
	// ⚠️ IT IS NO LONGER AN `alert_groups` ID, AND ONE READER STILL BELIEVES IT IS.
	// `service.links` builds `/groups/<id>` from it and argues at length that
	// `/cases/<id>` would be a detail page addressed by an id naming a different
	// table. That argument has inverted: the id names `alert_cases` now, so the
	// honest link is `/cases/` and the group screen it points at is being removed
	// from the web app in this same stage. Fixing the link belongs to the view
	// service, which owns the sentence.
	ID uuid.UUID
	// ⛔ GroupKey WAS `alert_groups.group_key` — the `gk_…` identity that was stable
	// across alertmanager.yml route edits (§C.4). PERMANENTLY EMPTY: it was the
	// route's identity, and a Case has an `alert_key`, which is a different thing
	// and is on `Alerts[0].AlertKey` where a reader can tell them apart.
	//
	//oto:retired the `alert_groups` row that was its only writer is deleted and
	// `groupFactsSQL` no longer selects it. The field is KEPT because it is still
	// READ, across four packages and out onto a published wire: the view service
	// copies it into `GroupView`, `channels/render/slack` renders it into the card
	// footer, and `channels/render/webhookjson` publishes it as `group_key` on the
	// outbound webhook envelope customers consume. Every one of those readers already
	// treats "" as an answer. Deleting the declaration is a four-package rename that
	// breaks an external payload, which is a product decision and not this cleanup.
	GroupKey string
	// Generation is `alert_cases.seq`: which run of this thing the reader is looking
	// at. The group's `generation` counted regenerations of a route's grouping; the
	// Case's `seq` counts firing episodes of an alert, and both answer "is this the
	// same run as last time" — which is the only question any reader asks of it (the
	// Slack renderer hashes it into the card nonce). Leaving it zero would tell every
	// renderer that every episode of an alert is the same run.
	Generation int
	// Title is `alerts.alertname`. The group's `title` was derived from the route's
	// group labels; with one alert per conversation the alert's own name IS the card
	// title, and `GroupLabels["alertname"]` — the fallback every renderer already
	// applies — now agrees with it by construction rather than by luck.
	Title string
	// ⛔ Receiver WAS `alert_groups.receiver`, the Alertmanager route receiver this
	// generation arrived through. PERMANENTLY EMPTY: it is a property of a route, and
	// oto no longer records the route.
	//
	//oto:retired `alert_groups.receiver` was its only writer and the table is gone.
	// KEPT for the same reason as `GroupKey` above: it is read by the view service,
	// the Slack renderer and the outbound `webhookjson` envelope, all of which
	// already draw "" as "not applicable". A route's receiver is answerable from
	// `ingest_batches.receiver` if it is ever wanted again, which is where the fact
	// actually lives.
	Receiver string
	// ⛔ SourceGroupKey WAS Alertmanager's own groupKey, DISPLAY ONLY and NEVER
	// PARSED because it is unescaped, unbounded, and changes on every
	// alertmanager.yml reload. PERMANENTLY EMPTY, and its hazards leave with it: the
	// value was only ever stored per generation.
	//
	//oto:retired `alert_groups.source_group_key` was its only writer. KEPT because it
	// is read by the view service and published as `source_group_key` on the outbound
	// webhook envelope, and because the hazard note above has to stay attached to the
	// declaration it guards — anything that refills this field must not parse it.
	// `ingest_batches.group_key` still stores Alertmanager's raw groupKey verbatim.
	SourceGroupKey string
	// GroupLabels is the one Alert's own labels.
	//
	// ⚠️ THIS IS A BEHAVIOURAL CHANGE AND NOT A RENAME. The group's `group_labels`
	// were the SUBSET Alertmanager grouped by; these are the alert's full set, which
	// is a superset. It matters because policy matching reads it: `notify.go` already
	// merges the focused alert's labels over the group's, so with one alert per
	// conversation the matcher now sees the same set whether or not the query named a
	// focus — which removes an inconsistency rather than adding one. A matcher on a
	// label the route did not group by will now match where it previously could not.
	// Leaving this empty was the alternative and it is worse: a conversation with no
	// focus would be matched against no labels at all, so policies would stop
	// selecting and oto would go quiet.
	GroupLabels map[string]string
	// State is `alert_cases.state`, still `open` or `closed`.
	State string
	// Severity is the one Alert's severity — RAW upstream, never normalised. The
	// group's was "max member severity", which over one member is the same value.
	Severity string
	// ClusterKey is `alerts.cluster_key`.
	//
	// ⚠️ IT USED TO BE PERMANENTLY EMPTY BY ACCIDENT, AND IS NOW FILLED. `alert_groups`
	// had a `cluster_id` and no `cluster_key`, so `groupFactsSQL` never selected one
	// and nothing ever scanned into this field. The webhook renderer's subtitle has a
	// live `on <cluster>` clause that has therefore never rendered; it will now.
	ClusterKey string
	// StateVersion is `alert_cases.state_version`, the episode's optimistic lock,
	// which increments on every state transition.
	//
	// ⭐ IT HAS A REAL SUCCESSOR AND THAT IS LOAD-BEARING, not tidiness. It is the
	// fallback `mint` uses for the §C.7 idempotency key when the payload pinned no
	// version, and its own comment explains why: a key derived from a CONSTANT is
	// identical for every evaluation of this subject forever, so the second fact
	// about it is swallowed as a duplicate of the first. `alert_groups.state_version`
	// and `alert_cases.state_version` were introduced for different purposes — a
	// notification key and a compare-and-set — and both increment on exactly the
	// events that make a new fact worth sending.
	StateVersion int

	// The six counts, projected over the ONE Alert this conversation's Case is
	// having: `TotalCount` is 1, exactly one of the first four is 1, and
	// `AckedCount` is 1 when that episode is acknowledged.
	//
	// ⛔ THEY WERE SIX STORED COLUMNS ON `alert_groups`, MAINTAINED BY A WRITER IN
	// ANOTHER MODULE. They are now derived in the same read that produces
	// `Snapshot.Alerts`, from the same row, which is what `AllResolved` below always
	// asked for: the counts cannot disagree with the card they are rendered on,
	// because there is no longer a copy of them to fall behind.
	FiringCount     int
	SuppressedCount int
	ResolvedCount   int
	ExpiredCount    int
	TotalCount      int
	AckedCount      int

	// ⛔⛔ `StormMode`, `StormSince` AND `StormCount` WERE HERE AND ARE DELETED WITH
	// THE COLUMNS THEY MIRRORED (migration 00059). `alert_groups.storm_mode` and
	// `storm_since` were LIVE STATE about a generation — "this one is collapsed
	// right now" — and nothing evaluates a storm any more, so no writer could ever
	// set them again. `ReasonStorm` and `SuppressedStorm` were briefly RETIRED on that
	// argument — a stored row still has to render — and then went the same way:
	// migration 00059 narrows `notifications_suppmap_ck` to six values and migration
	// 00060 narrows `notifications_reason_ck` to eighteen, and the database reset the
	// maintainer authorised means there is no row left to decode.
	//
	// ⚠️ `channels/domain.GroupView.StormMode` and `NotificationView.StormCount` are
	// what this used to feed, and both are deleted too: with no `storm` Reason there is
	// no stored notification for the Slack renderer to draw a storm card from.

	// ⛔ NotificationReason WAS `alert_groups.last_notification_reason` — the wire
	// value Alertmanager put on the most recent batch for this generation (§H.6),
	// Alertmanager's statement about its OWN delivery, kept verbatim and reconciled
	// against oto's transition-derived Reason at evaluation time. PERMANENTLY EMPTY:
	// the column was per generation and the table is gone.
	//
	// ⚠️ RECONCILIATION ITSELF SURVIVES AND STILL MATTERS, so nothing here is dead.
	// `ReconcileWithWire` takes this value AND `AllResolved()`, and it is the second
	// argument that carries the load-bearing case — `some_resolved` becoming
	// `all_resolved`, which is update-only becoming a thread reply that may be
	// broadcast. The wire half was already inert in practice: no writer in the tree
	// has ever set `last_notification_reason`, so this field has been the empty
	// string on every card oto has sent. It is worth knowing before anyone concludes
	// from the deletion that a behaviour changed.
	//
	//oto:retired `alert_groups.last_notification_reason` was the column it was
	// selected from and the table is gone. ⭐ IT IS ALSO THE ONE FINDING HERE THAT IS
	// NOT REALLY THIS TICKET'S: no writer in the tree has EVER set that column, so the
	// value has been the empty string on every card oto has sent, before and after.
	// What changed is only that the SELECT which laundered it into a "write" went
	// away. KEPT because `ReconcileWithWire` still reads it and reconciliation still
	// matters — the load-bearing half is `AllResolved()`, and the wire half degrading
	// to "" is the same behaviour it always had.
	NotificationReason string

	// FiringSince is the UPSTREAM start of this firing episode:
	// `least(source_starts_at, started_at)` on the Case.
	//
	// ⛔ IT WAS `min(least(…))` OVER THE GENERATION'S MEMBERS, AND A SEPARATE ROUND
	// TRIP THAT DEGRADED TO ZERO if the aggregate went wrong. One Case is one row and
	// both columns are NOT NULL, so there is no aggregate left to fail and it is read
	// with everything else. `started_at` is still in the `least` for the case whose
	// upstream sent no usable `startsAt`.
	//
	// ⛔ IT IS NOT `FirstSeenAt`. `FirstSeenAt` is when OTO first heard about the
	// episode, and the gap between the two is oto's own latency plus Alertmanager's
	// `group_wait` — twenty-one minutes in the first live run. A card that says
	// "Started 18:17" when the alert started at 17:56 has told an operator
	// something false about how long an outage has lasted, which is the one number
	// they act on at 03:00.
	FiringSince time.Time

	// AlertmanagerURL is an Alertmanager UI root oto CAN VOUCH FOR — the base the
	// Silence and "Open in Alertmanager" deep links are built from (§H.3, R3): oto
	// never writes a silence, it only shows you where to write one — or the empty
	// string, meaning there is nowhere to send anyone.
	//
	// ⛔⛔ IT IS PERMANENTLY EMPTY, AND THE ONE AFFORDANCE v1 OFFERS IS THEREFORE
	// OFF (git-bug `7570090`). `alert_groups.source_id` was the only path from
	// anything a card is about to `alert_sources`: neither `alerts` nor
	// `alert_cases` nor `clusters` carries a source, so with the group deleted there
	// is nothing to join and nothing to vouch for. Restoring the Silence deep link
	// needs a `source_id` on `alert_cases` — a schema change, deliberately not
	// smuggled into a deletion.
	//
	// ⛔ AND THE REASONING THAT MADE IT EMPTY-OR-NOTHING STILL DECIDES WHAT HAPPENS
	// NEXT, so it is kept rather than deleted with the join. It is NOT simply the
	// source's `base_url`, and filling it as if it were is the bug this field's name
	// once invited: `base_url` is the Alertmanager API ROOT, and only for a source of
	// kind `alertmanager` is that also the UI root. A grafana source's base_url
	// addresses an AM-compat API whose console keeps silences somewhere else
	// entirely, so `<base>/#/silences/new` is a 404 with a button on it. Whoever
	// restores this owes that check, exactly as `groupFactsSQL` used to make it.
	//
	// EMPTY IS A REAL ANSWER, not a missing one, and every consumer already draws
	// it as no link and no Silence button — which is why this degrades to a card
	// that offers nothing rather than to a card that offers a lie. A fabricated link
	// that 404s at 03:00 costs an operator the one affordance v1 offers them.
	//
	//oto:retired `alert_groups.source_id` was the only join from anything a card is
	// about to `alert_sources`, so with the group deleted nothing can vouch for a UI
	// root and the SILENCE DEEP LINK IS OFF — the one silence affordance v1 has (R3,
	// §H.3). The field is KEPT, and this is the case the marker exists for: the reader
	// is alive (`view.go` still builds `AlertmanagerSilenceNew` from it, and the Slack
	// renderer still draws the button when it is non-empty), so restoring the feature
	// is filling this field rather than rebuilding a path. The paragraphs above are
	// the specification of how to fill it honestly — a `source_id` on `alert_cases`,
	// and a `kind == alertmanager` check, because a grafana source's `base_url` makes
	// `<base>/#/silences/new` a 404 with a button on it — and they must stay attached
	// to the declaration they are about.
	AlertmanagerURL string

	// FirstSeenAt is `alert_cases.started_at`: when OTO first heard about this
	// episode. ⛔ IT IS NOT `alerts.first_seen_at`, which is when oto first heard
	// about the ALERT and may be months earlier — using it here would date this
	// firing to a firing that is over, which is the same defect `FiringSince`
	// describes with the numbers on it.
	FirstSeenAt time.Time
	// LastActivityAt is `alert_cases.last_observed_at`.
	LastActivityAt time.Time
	// ClosedAt is `alert_cases.ended_at`, so `Open()` reports whether the episode is
	// still running.
	ClosedAt *time.Time
}

// StartedAt is the instant the card means by "Started": upstream's own
// `startsAt` when oto has one, and oto's first sighting when it does not.
//
// The fallback is honest rather than convenient — an episode with no recorded
// upstream start really is only known to oto from when oto saw it — and it is one
// function so that every renderer, the API and the UI answer the question the same
// way. It is unreachable while both of the Case's clock columns are NOT NULL, and
// it stays because this type is also filled by whatever implements
// `service.SnapshotSource` next.
func (g GroupFacts) StartedAt() time.Time {
	if !g.FiringSince.IsZero() {
		return g.FiringSince
	}
	return g.FirstSeenAt
}

// AllResolved reports whether this conversation's one Alert has stopped firing
// and stopped being suppressed, by resolving rather than by expiring.
//
// It is the fact behind §H.6's `all alerts resolved` row, and oto derives it
// from its OWN record rather than trusting the wire value alone: the counts are a
// projection of the case oto has recorded, and they cannot disagree with the card
// they render — which is now true structurally, since the same read produces both.
//
// ⛔ THE EXPRESSION IS UNCHANGED OVER ONE MEMBER, AND THAT IS THE POINT OF
// LEAVING IT ALONE. `Total > 0 && Firing == 0 && Suppressed == 0 && Resolved > 0`
// reads, one member wide, as "the episode ended and it ended by resolving" — an
// expired episode still answers false, exactly as an expired-only generation did.
// Replacing it with a direct state test would be the same answer written twice, and
// the two copies could disagree.
func (g GroupFacts) AllResolved() bool {
	return g.TotalCount > 0 && g.FiringCount == 0 && g.SuppressedCount == 0 &&
		g.ResolvedCount > 0 && g.ResolvedCount+g.ExpiredCount >= g.TotalCount
}

// Open reports whether this firing episode is still live.
func (g GroupFacts) Open() bool { return g.ClosedAt == nil }

// AlertFacts is one Alert as a card sees it.
type AlertFacts struct {
	ID                uuid.UUID
	AlertKey          string
	SourceFingerprint string
	AlertName         string
	// Severity is the RAW upstream label value, never normalised: users filter on
	// their own vocabulary and normalising here would destroy it.
	Severity     string
	Namespace    string
	Service      string
	ClusterKey   string
	Labels       map[string]string
	Annotations  map[string]string
	GeneratorURL string

	State string
	// AckState is the ack of THIS ALERT'S CASE — the conversation's own Case in
	// `Alerts`, the alert's current case on the focus — and never a
	// column on `alerts`. An ack is a statement about ONE firing episode: a
	// projection onto the Alert keeps asserting it after that episode has closed,
	// which is how a September firing arrives pre-acknowledged because somebody
	// acked in March. It reads `unacked` when the case cannot be resolved, which
	// is the only safe default: over-reporting an unacked alert costs a glance,
	// under-reporting one costs the alert.
	AckState string
	// SnoozedUntil is `alert_snoozes.snoozed_until` of the row that is currently
	// in force for this Alert, and nil when there is none.
	//
	// ⭐ IT IS READ FROM THE SNOOZE ROW, NEVER FROM A COLUMN ON `alerts`. Whether
	// oto speaks is decided from this field, and deciding it from a denormalised
	// mirror meant the notification path never touched the record that knows who
	// asked for quiet, what they wrote, or how the quiet period ended. The join
	// that fills this is in `alertFactsColumns`, shared by both statements that
	// build an `AlertFacts`, so those three facts are one column-list entry away
	// whenever a surface needs to say them.
	//
	// It is still the THIRD ORTHOGONAL AXIS: not a state, never touching severity,
	// and a snoozed critical is still rendered as a firing critical.
	//
	// ⚠️ A NON-NIL VALUE IS NOT THE SAME AS "QUIET". The row is live until the
	// 60-second expiry sweep ends it, so its clock may already have run out. Ask
	// `Snoozed(now)`, which is why that predicate takes an instant.
	SnoozedUntil *time.Time

	FirstSeenAt time.Time
	LastSeenAt  time.Time
	TotalCases  int
	IsFlapping  bool
	FlapScore   float64
	Value       *float64
}

// Snoozed reports whether this alert's notifications are quiet as of now.
func (a AlertFacts) Snoozed(now time.Time) bool {
	return a.SnoozedUntil != nil && a.SnoozedUntil.After(now)
}

// CaseFacts is one firing episode.
type CaseFacts struct {
	ID                uuid.UUID
	Seq               int
	State             string
	AckState          string
	SuppressionReason string
	ResolveReason     string
	StartedAt         time.Time
	EndedAt           *time.Time
	AckedByLabel      string
	AckedAt           *time.Time
	AckNote           string
}

// Duration is how long the episode has run, or ran.
func (o CaseFacts) Duration(now time.Time) time.Duration {
	if o.EndedAt != nil {
		return o.EndedAt.Sub(o.StartedAt)
	}
	return now.Sub(o.StartedAt)
}

// RuleFacts is what the alerting rule said when the case fired. Capturing
// it is oto's defensible differentiator: every other tool shows you the alert,
// and none of them shows you that somebody lowered the threshold last Tuesday.
type RuleFacts struct {
	SnapshotID      uuid.UUID
	Fingerprint     string
	File            string
	Group           string
	Name            string
	Expr            string
	For             time.Duration
	KeepFiringFor   time.Duration
	Labels          map[string]string
	Annotations     map[string]string
	Origin          string
	MatchConfidence string
	CapturedAt      time.Time
}

// RuleChangeFacts is the diff between this case's rule and the previous
// one's.
type RuleChangeFacts struct {
	PreviousSnapshotID  uuid.UUID
	PreviousFingerprint string
	PreviousCapturedAt  time.Time
	ExprChanged         bool
	PreviousExpr        string
	NewExpr             string
	ForChanged          bool
	PreviousFor         time.Duration
	NewFor              time.Duration
	// LabelDiff maps a name to [old, new]; "" means the label was absent.
	LabelDiff      map[string][2]string
	AnnotationDiff map[string][2]string
}

// EnrichmentFacts is one Enricher's provenanced result.
type EnrichmentFacts struct {
	Enricher   string
	Status     string
	Payload    map[string]any
	Warnings   []string
	Error      string
	ComputedAt time.Time
}

// ActorFacts is who caused the fact being communicated. ACTOR, NEVER SUBJECT: a
// person appears on a notification as metadata about an action, never as its
// topic.
//
// It is read from the ONE event that IS the fact, never assembled from a user
// row: `alert_events.actor_label` is frozen at write time precisely so that a
// renamed or deleted user does not rewrite what a card said (§D.4). So there is
// no id to resolve here and no directory lookup on the delivery path — the name
// was already decided, by the module that owns the verb, at the instant the
// human acted.
type ActorFacts struct {
	// Kind is `alert_events.actor_kind`: system | ingest | reconciler | reaper |
	// enricher | notifier | user | slack. The last two are the human ones, and
	// only they are guaranteed an id AND a label (ev_actor_ck) — so an actor with
	// no label is one of oto's own machines, which is a DIFFERENT answer from no
	// actor at all.
	Kind  string
	ID    string
	Label string
}

// TransitionFact is one entry of the Case's state trail — a §B.3 edge that a
// human reading the card at 03:00 would want to see happened.
//
// ⭐ IT EXISTS BECAUSE `chat.update` IS BOTH SILENT AND DESTRUCTIVE. ADR 0008
// makes the root card the CURRENT state and the thread the history, which is
// exactly right if you are in the thread. In the channel, a firing card mutates
// into a resolved one with no notification and no trace: somebody scrolling past
// sees a calm green card and cannot tell that anything ever happened, when it
// fired, or for how long. The owner's words on watching it: "it means something
// happened and we don't know."
//
// The trail is the fix that keeps `chat.update` as the primary verb. The card
// stops being a live gauge that forgets, and becomes a live gauge that keeps its
// receipt.
type TransitionFact struct {
	// Type is the `alert_events.type` value, e.g. `case.resolved`.
	Type string
	// At is the UPSTREAM clock (`occurred_at`), which is what a human reads.
	At time.Time
	// ActorLabel is who caused it, when a human did. ACTOR, NEVER SUBJECT.
	ActorLabel string
}

// Snapshot is the whole read model for one delivery.
type Snapshot struct {
	Org OrgFacts
	// Group is the card-level facts of the conversation. See `GroupFacts`: it is
	// projected from the Case and its one Alert now, and the name is retained on
	// purpose.
	Group GroupFacts
	// Alerts is the conversation's member list, which is AT MOST ONE ELEMENT — the
	// Case's one Alert while the episode is live, and empty once it has ended.
	//
	// ⛔ IT IS A SLICE AND NOT AN `*AlertFacts`, DELIBERATELY (git-bug `7570090`).
	// A Case has exactly one Alert, so the shape is now over-general — and it is a
	// CONTRACT with three readers this change does not own: the view service builds
	// the card's instance list from it, `service.links` falls back to `Alerts[0]` for
	// the runbook and Prometheus links when there is no focus, and `AnyFlapping`
	// below walks it. Collapsing it to a pointer would move a rendering decision into
	// three other people's files in the same change that deleted the container.
	// `MaxAlerts` can no longer truncate it, so "already capped" is now structural.
	Alerts []AlertFacts
	Focus  *AlertFacts
	// Trail is the Case's state history, oldest first, already capped.
	Trail []TransitionFact
	// NotificationCount is how many non-suppressed notifications oto has sent in
	// this CONVERSATION — keyed on the delivery target, never on the subject, for
	// the reason `caseNotificationsSQL` gives at length. It is a fact about OTO's
	// behaviour, and the receipt on a terminal card is the right place to answer
	// "how loud was this?".
	NotificationCount int
	// Case is the focused firing episode, when there is one.
	Case       *CaseFacts
	Rule       *RuleFacts
	RuleChange *RuleChangeFacts
	// Enrichments are keyed by enricher name.
	Enrichments map[string]EnrichmentFacts
	Actor       *ActorFacts
	Comment     string
	// SnoozedAlerts is the subset of the RENDERED alerts whose notifications are
	// currently quiet, with the time each wakes up. It drives the card's
	// `*Notifications*` field.
	SnoozedAlerts map[uuid.UUID]time.Time
	// MemberCount and SnoozedMemberCount are 0 or 1: one while the Case is live,
	// zero once it has ended, and `SnoozedMemberCount` is 1 only when that Alert's
	// snooze is GENUINELY in force at `TakenAt`.
	//
	// ⭐ THEY WERE COUNTED BY A SECOND QUERY so that they covered EVERY current
	// member rather than the capped `Alerts` page — the suppression decision must not
	// depend on how many instances the renderer happens to show. That guarantee is
	// unchanged and is now STRUCTURAL rather than bought with a round trip: a cap
	// cannot truncate a relation of at most one row, so the count and the list are
	// the same fact, read once, and cannot disagree.
	MemberCount        int
	SnoozedMemberCount int
	// TakenAt is when this snapshot was read. It becomes the card's "updated"
	// timestamp, so it must be the read time and not the enqueue time.
	TakenAt time.Time
}

// AllMembersSnoozed reports whether every member alert is currently quiet, which
// over one Alert means: that Alert is quiet, and the Case is still live.
//
// This is the CONVERSATION-level snooze test — the one `notify.go` uses when the
// query named no focus — and the conservative direction still matters, one level
// down from where it was written. It used to say: a group with ONE awake member is
// not snoozed, because silencing the whole card because most of it is quiet would
// hide the one alert nobody asked to be quiet about. There are no siblings left to
// hide behind, so the same conservatism now shows up in WHICH ROWS COUNT: a snooze
// whose `ended_at` is set, or whose clock has run out while the 60-second sweep has
// not been round, does not make this true. A card held back on a quiet period that
// has already finished is the §B.8.3 failure with one member instead of five.
//
// ⚠️ `MemberCount > 0` IS LOAD-BEARING AND IS NOT A NIL GUARD. It is why a
// terminal Case is never "all snoozed": the member list is empty once the episode
// ends, so the resolution notification goes out even when the alert still has a live
// snooze on it. Reporting true there would suppress the one message that closes the
// loop for whoever was woken up.
func (s Snapshot) AllMembersSnoozed() bool {
	return s.MemberCount > 0 && s.SnoozedMemberCount >= s.MemberCount
}

// FocusSnoozed reports whether the ONE alert this fact is about is quiet.
//
// Snooze is scoped to an `alert_key` (§B.8.1), so when a fact is about one alert
// that alert's own snooze decides — never the conversation's. The two agree
// whenever the focus IS the Case's own alert, which is the common case and not a
// reason to collapse them: `AlertID` may name an alert this Case is not about.
func (s Snapshot) FocusSnoozed(now time.Time) bool {
	if s.Focus == nil {
		return false
	}
	if s.Focus.Snoozed(now) {
		return true
	}
	until, ok := s.SnoozedAlerts[s.Focus.ID]
	return ok && until.After(now)
}

// AnyFlapping reports whether any member is flapping.
func (s Snapshot) AnyFlapping() bool {
	if s.Focus != nil {
		return s.Focus.IsFlapping
	}
	for _, a := range s.Alerts {
		if a.IsFlapping {
			return true
		}
	}
	return false
}
