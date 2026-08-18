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
	// StormCount is greater than zero when the group is in storm mode. Storm mode
	// is a VISIBLE state, never silent suppression.
	StormCount int
	RenderedAt time.Time
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
	StormMode       bool
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
// `fired`, `acked`, `unacked`, `suppressed`, `unsuppressed`, `resolved`,
// `expired`, `refired`, `storm`. It is deliberately NOT the raw
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
	// "oto.noop.silence". Every URL button still delivers an interaction payload
	// oto must acknowledge (§H.8).
	ID    string
	Label string
	Style string // "" | "primary" | "danger"
	// URL set makes this a link action.
	URL string
	// Value is an OPAQUE ID ONLY. Never a payload. Never trusted.
	Value   string
	Confirm bool
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
