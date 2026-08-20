package domain

import "time"

// NotificationView is the channel-agnostic read model a Renderer turns into a
// message. It is built ONCE PER DELIVERY, AT CLAIM TIME (C11), so that a queued
// notification reflects the world as it is when it is sent rather than when it
// was enqueued. It is denormalised on purpose: renderers must never query.
type NotificationView struct {
	Org    OrgRef
	Reason string // notification.Reason, §H.6
	Group  GroupView
	// Alerts are the group's members, newest first, already capped by
	// RenderOptions.MaxInstances.
	Alerts []AlertView
	// Focus is set when the fact is about ONE alert: an ack, a re-fire, a rule change.
	Focus       *AlertView
	Case        *CaseView
	Rule        *RuleView
	RuleChange  *RuleChangeView
	Enrichments map[string]EnrichmentView // keyed by enricher name
	// Digest is set when this view is a PERIODIC SUMMARY rather than a fact about a
	// signal, and it is non-nil on exactly those views. A renderer branches on it
	// before it branches on anything else, because none of the fields above mean
	// anything on one: there is no group, no member list, no case, no rule and no
	// state to colour.
	//
	// ⛔ IT EXISTS BECAUSE THE HEADLINE USED TO RIDE `Group.Title` (git-bug
	// `78388fb`). `notification/service.ViewService.digest` built one sentence with
	// `DigestHeadline` and put it in the group's name slot, so a renderer that had
	// never heard of a digest drew `*<sentence>* — <status>` and was accidentally
	// truthful. Its own comment called that "a floor, not a design", and the floor
	// had a shape a defect could grow in: `Group.Title` held something that was not
	// a group's title and `Group.State` said "open" about a view with no group, so
	// every GENERIC reader of either field — a fallback derivation, an audit surface,
	// a second renderer — read a digest's prose as a group's name and was right by
	// accident.
	//
	// ⭐ IT CARRIES FACTS AND NOT PROSE, WHICH IS THE WHOLE POINT. A count and a span
	// can be laid out as fields, translated, re-ordered, or drawn as a chart by a
	// renderer that wants to; a sentence can only be printed. §F.1's seam says the
	// view contains no block, no colour and no provider name — a pre-composed
	// headline is the same category of leak one level up, a LAYOUT decision made in
	// the module that has no channel.
	Digest *DigestView
	// Actor is who did it, for human-caused reasons.
	Actor   *ActorView
	Comment string
	Actions []Action
	Links   Links
	// Previous carries the state the card showed before this delivery, for the
	// strikethrough trick (§H.4).
	Previous *PreviousState
	// Trail is the group's state history, oldest first — the card's receipt.
	//
	// ⭐ IT IS WHAT STOPS `chat.update` ERASING THE STORY (ADR 0008, §H.4). The
	// root card is the current state and the thread is the history, which is right
	// for a reader who is IN the thread. In the channel the update is silent and
	// destructive: a firing card becomes a resolved card with no notification and
	// no trace, and somebody scrolling past cannot tell that anything happened.
	Trail []TrailEntry
	// Notifications is how many notifications oto sent about this group. It is on
	// the receipt because "how loud was this?" is a question about oto's own
	// behaviour that only oto can answer.
	Notifications int
	// SnoozedUntil is when oto's own quiet on this signal runs out, and nil when
	// oto is not being quiet about it. It is the ONE fact a snooze card cannot
	// infer: who asked is `Actor` and what they wrote is `Comment` — both already
	// here, both read the same gated way `ackedBy` and `suppressionNote` read them
	// — but "until when" lives nowhere a renderer can reach, and a `snoozed` card
	// that cannot say it is a card that says "Snoozed" and stops.
	//
	// ⛔ IT IS THE THIRD ORTHOGONAL AXIS AND IT NEVER TOUCHES THE COLOUR (§B.8.1,
	// §B.8.6). A snoozed firing critical is still a firing critical: it stays
	// `#a30200` / `:rotating_light:`, its Status field still follows `case.state`,
	// and `DeriveCardState` neither reads this field nor may ever learn to.
	// "Colouring a snoozed critical calm would be the exact lie §E.1.1 exists to
	// prevent" (§H.4). This field exists so the card can SAY oto has gone quiet,
	// never so the card can LOOK quiet.
	//
	// ⛔ IT IS ALSO NOT `CaseView.SuppressionReason`, which is Alertmanager's
	// silence and explicitly "NOT" oto's (`notification/domain/suppression.go`).
	// A snooze changes nothing in the cluster, so `CardSuppressed` never fires for
	// one and its grey must never be borrowed to draw one.
	//
	// ⚠️ A NON-NIL VALUE IS NOT THE SAME AS "QUIET" — the snooze row is live until
	// the 60-second expiry sweep ends it, so its clock may already have run out.
	// The renderer draws what it is given; it does not re-decide whether oto is
	// speaking, because that decision was already made upstream at claim time.
	SnoozedUntil *time.Time
	RenderedAt   time.Time
}

// OrgRef identifies the tenant a notification belongs to.
type OrgRef struct{ ID, Slug, Name string }

// GroupView is one generation of one Alertmanager notification group — the thing
// that owns exactly one Slack thread.
type GroupView struct {
	ID, GroupKey    string
	Generation      int
	Title           string
	Receiver        string
	GroupLabels     map[string]string
	State           string // open | closed
	Severity        string
	FiringCount     int
	SuppressedCount int
	ResolvedCount   int
	ExpiredCount    int
	TotalCount      int
	AckedCount      int
	// StartedAt is when the SIGNAL started, taken from upstream's own `startsAt`.
	// FirstSeenAt is when OTO first heard about it. They are different facts and
	// the card must not present the second as the first: the gap is oto's latency
	// plus Alertmanager's `group_wait`, and it was twenty-one minutes in the first
	// live run.
	StartedAt      time.Time
	FirstSeenAt    time.Time
	LastActivityAt time.Time
	// SourceGroupKey is Alertmanager's own groupKey. DISPLAY ONLY, NEVER PARSED:
	// it is unescaped, unbounded, and changes on every alertmanager.yml reload (C3).
	SourceGroupKey string
	ClusterKey     string
}

// DigestView is what a digest has INSTEAD OF a GroupView: a count and the span it
// was counted over.
//
// A digest is one summary per window per policy, replacing per-event traffic. It is
// not a Case, it is not a transition, and it names no signal — so it borrows none of
// the Case-shaped fields above, and a renderer must not reach for them.
//
// ⚠️ THE SPAN IS HALF-OPEN — `[CoveredFrom, CoveredTo)` — AND A CARD MUST NOT PRINT
// IT AS IF IT WERE CLOSED. `notifications_digcover_ck` enforces
// `covered_from <= window_start < covered_to`, and `digest_covered_to`'s own column
// comment names it "the EXCLUSIVE end". So the honest rendering is "from X up to Y",
// never "from X to Y": the Case that opened at exactly `CoveredTo` belongs to the
// NEXT digest, and a card claiming otherwise would double-count it in the reader's
// head every window.
type DigestView struct {
	// Count is how many Cases OPENED inside the span. It is the number the digest
	// asserts and the number the policy's floor was compared against, read off the
	// stored row rather than recomputed: the window is closed so there is no newer
	// truth, and `alert_cases` is reapable, so a recomputed count would shrink as the
	// episodes aged out (migration 00058).
	//
	// It is at least 1 on anything oto sends — `notifications_digest_ck` requires
	// `digest_count >= 1` on every digest row, and a window below its policy's floor
	// writes no row at all.
	Count int
	// CoveredFrom is the INCLUSIVE start of the span, at or before the window's own
	// start. It reaches back when the digest swept up a Case whose transaction
	// committed too late for the previous window's read, so the honest sentence is
	// "since the last digest, plus stragglers" (migration 00070).
	//
	// CoveredTo is the EXCLUSIVE end: the window's end, and therefore the instant
	// coverage reached.
	//
	// ⛔ BOTH ARE ZERO ON A DIGEST WRITTEN BEFORE MIGRATION 00070, AND A RENDERER MUST
	// DRAW THAT ABSENCE RATHER THAN FILL IT. The only way to invent the span is
	// `window_start + the policy's CURRENT digest_window_s`, which is exactly the
	// inference git-bug `342e071` is about: an operator who narrows a window would
	// retroactively change the span every card oto has ever drawn claims to cover. A
	// card that does not know its span says so.
	CoveredFrom, CoveredTo time.Time
}

// AlertView is one Alert as a renderer sees it.
type AlertView struct {
	ID, AlertKey, SourceFingerprint                     string
	AlertName, Severity, Namespace, Service, ClusterKey string
	Labels, Annotations                                 map[string]string
	GeneratorURL                                        string
	State, AckState                                     string
	FirstSeenAt, LastSeenAt                             time.Time
	TotalCases                                          int
	IsFlapping                                          bool
	Value                                               *float64
}

// CaseView is one firing episode as a renderer sees it.
//
// ⭐ `State` IS THE FOUR-WORD §B.2 READING AND NOT `alert_cases.state`. A card
// says "firing" or "resolved" about an episode and always has; the column behind
// it holds `open | closed` since ADR 0040, and `notification/repository`
// recomposes the word before a renderer ever sees it. Renderers are unchanged by
// that migration, deliberately — the frozen `oto.notification.v1` envelope could
// not have absorbed it.
//
// ⛔ THERE IS NO `ReopenCount`. A Case is strictly terminal, so the number could
// only ever be zero.
type CaseView struct {
	ID                                                string
	Seq                                               int
	State, AckState, SuppressionReason, ResolveReason string
	StartedAt                                         time.Time
	EndedAt                                           *time.Time
	Duration                                          time.Duration
	AckedByLabel                                      string
	AckedAt                                           *time.Time
	AckNote                                           string
}

// RuleView is what the alerting rule said at the moment the case fired.
// Capturing this is the defensible differentiator (R6).
type RuleView struct {
	SnapshotID, Fingerprint string
	File, Group, Name       string
	Expr                    string
	For, KeepFiringFor      time.Duration
	Labels, Annotations     map[string]string
	Origin, MatchConfidence string
	CapturedAt              time.Time
}

// RuleChangeView is the headline differentiator's payload: what changed in the
// rule definition between this case and the previous one.
type RuleChangeView struct {
	PreviousSnapshotID    string
	PreviousFingerprint   string
	PreviousCapturedAt    time.Time
	ExprChanged           bool
	PreviousExpr, NewExpr string
	ForChanged            bool
	PreviousFor, NewFor   time.Duration
	// LabelDiff maps a name to [old, new]; "" means the label was absent.
	LabelDiff      map[string][2]string
	AnnotationDiff map[string][2]string
}

// EnrichmentView is one Enricher's provenanced result.
type EnrichmentView struct {
	Enricher   string
	Status     string
	Payload    map[string]any
	Warnings   []string
	Error      string
	ComputedAt time.Time
}

// ActorView is who caused the fact being communicated.
type ActorView struct{ Kind, ID, Label string }

// PreviousState is the state the card showed before this delivery.
type PreviousState struct {
	State, AckState string
}

// TrailEntry is one transition on the card's state trail.
//
// Kind is a small closed vocabulary the renderer maps to an emoji and a verb —
// `fired`, `acked`, `unacked`, `suppressed`, `unsuppressed`, `snoozed`,
// `unsnoozed`, `resolved`, `expired`, `refired`. It is deliberately NOT the raw
// `alert_events.type`: the renderer must not learn another module's enum, and a
// type it does not recognise is dropped rather than printed raw.
type TrailEntry struct {
	Kind  string
	At    time.Time
	Actor string
}

// Action is one interactive affordance on a card.
type Action struct {
	// ID is the stable action id: "oto.ack", "oto.unack", "oto.noop.runbook",
	// "oto.noop.silence", "oto.snooze", "oto.unsnooze". Every URL button still
	// delivers an interaction payload oto must acknowledge (§H.8).
	ID    string
	Label string
	Style string // "" | "primary" | "danger"
	// URL set makes this a link action.
	URL string
	// Value is an OPAQUE ID ONLY. Never a payload. Never trusted.
	Value   string
	Confirm bool
	// Options turn this action into a MENU rather than a button, and they exist
	// because §B.8.3's snooze is the first affordance on the card that asks the
	// human a QUESTION — "for how long?" — rather than recording a single fact.
	//
	// ⛔ IT IS STILL CHANNEL-AGNOSTIC, which is the whole reason it is a list of
	// label/value pairs and not a Block Kit element. `notification/service` decides
	// that a snooze offers exactly the five §B.8.3 presets; each renderer decides
	// what a five-way choice LOOKS like — Slack draws a select, and a renderer with
	// no menu of its own is free to draw five buttons or nothing at all. A view that
	// named `static_select` here would have made the seam a Slack seam.
	//
	// ⭐ ONE ACTION ID COVERS EVERY OPTION, exactly as Slack's own select does: the
	// id says WHICH question was answered and the chosen option's `Value` says WHAT
	// the answer was. The handler re-reads that answer against its own closed table
	// (`channels/service`, snoozePresets) rather than trusting it, which is what
	// keeps S8 true of a value that is no longer a bare id.
	Options []ActionOption
}

// ActionOption is one choice inside a menu-shaped Action.
//
// ⛔ `Value` IS NOT A BARE ID, AND IT IS STILL NOT TRUSTED. A button's value is
// one opaque uuid (S8) because a button asks nothing; a menu option has to say
// which of the offered choices was taken, so its value is a SELECTOR — a short
// token oto minted, sent, and will look up in its own closed table when the click
// comes back. Nothing is decoded FROM it: an unrecognised token is refused, which
// is the same posture as an unparseable uuid on a button.
type ActionOption struct {
	Label string
	Value string
}

// SnoozeValueSeparator joins a preset token to the alert id inside one snooze
// option's value.
//
// ⛔ IT LIVES BESIDE THE PRESETS BECAUSE THE TWO ENDS MUST AGREE. `notification/
// service` mints the value and `channels/service` splits it, and a `|` written out
// twice is a menu whose every option the handler refuses. It is a `|` because that
// byte cannot occur in a uuid or in a preset token, so the split needs no escaping
// and cannot be forged by a value that contains one.
const SnoozeValueSeparator = "|"

// SnoozePreset is one of the durations a card may offer to go quiet for.
//
// ⛔ THE FIVE ARE BINDING AND THERE IS NO SIXTH, LEAST OF ALL "INDEFINITELY"
// (§B.8.3). "There is no indefinite snooze — an unexpiring snooze is a mute, and
// mutes are how channels die." Every preset here is also inside the domain's own
// 5-minute…30-day bounds, so a press can never be refused by the window check the
// API shares with `alert_snoozes_min_ck` / `_max_ck`; a preset added outside them
// would fail at the write, on the operator's card, at 03:00.
type SnoozePreset struct {
	// Token is what travels in the option's value and comes back on the press. It
	// is short because an option's `value` is a far shorter field than a button's,
	// and stable because a card posted last month still carries last month's token
	// — the same durability argument the action ids themselves are under (§H.8).
	Token string
	// Label is what the human reads in the menu.
	Label string
	// For is how long the quiet lasts, measured from the moment oto acts on the
	// press and never from the moment the card was rendered.
	For time.Duration
}

// snoozePresets is the closed list, in the order §B.8.3 writes it.
//
// It lives in `channels/domain` rather than beside either user because it has TWO,
// on opposite sides of the seam: `notification/service` builds the menu's options
// from the tokens and labels, and `channels/service` turns a token that comes back
// into a duration. Two copies of five tokens is two chances for the menu to offer a
// choice the handler cannot decode — which presents as a button that does nothing,
// the one defect the interaction surface is under standing orders to never ship.
var snoozePresets = []SnoozePreset{
	{Token: "30m", Label: "30 minutes", For: 30 * time.Minute},
	{Token: "1h", Label: "1 hour", For: time.Hour},
	{Token: "4h", Label: "4 hours", For: 4 * time.Hour},
	{Token: "24h", Label: "24 hours", For: 24 * time.Hour},
	{Token: "7d", Label: "7 days", For: 7 * 24 * time.Hour},
}

// SnoozePresets is the list a card offers, in §B.8.3's order.
//
// It returns a COPY. The slice is package state that two modules read on every
// render, and a caller that sorted or truncated the original in place would change
// what every card in every org offers.
func SnoozePresets() []SnoozePreset {
	out := make([]SnoozePreset, len(snoozePresets))
	copy(out, snoozePresets)
	return out
}

// SnoozeDuration decodes one token back into the duration oto offered.
//
// ⛔ THIS IS A LOOKUP AND NOT A PARSE, AND THE DIFFERENCE IS THE WHOLE POINT (S8).
// The token arrives from a Slack payload, which is to say from the network; a
// `time.ParseDuration` here would let a press ask for eleven weeks of silence on an
// alert by editing four characters. An unknown token answers false and the surface
// refuses the press, exactly as an unparseable uuid on a button does.
func SnoozeDuration(token string) (time.Duration, bool) {
	for _, p := range snoozePresets {
		if p.Token == token {
			return p.For, true
		}
	}
	return 0, false
}

// Links are the deep links a card offers.
type Links struct {
	Group, Alert, Timeline   string // oto deep links
	Prometheus, Alertmanager string
	// AlertmanagerSilenceNew is a deep link into the Alertmanager UI. It is v1's
	// ONLY silence affordance: oto has no write path into the cluster (R3).
	//
	// It and `Alertmanager` are EMPTY unless the card's source is an Alertmanager
	// whose console oto can address — both are Alertmanager's own `/#/…` URL
	// shapes, and a Grafana-backed source does not serve them. A renderer draws
	// the empty string as no button, which is the intended card.
	AlertmanagerSilenceNew                       string
	Runbook                                      string
	GrafanaDashboard, GrafanaPanel, GrafanaImage string // Grafana-sourced only
}
