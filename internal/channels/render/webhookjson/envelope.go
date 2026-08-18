package webhookjson

import (
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// Schema is the envelope's version tag. It is the contract: a consumer that
// checks this one string knows exactly which fields it can rely on.
//
// It is versioned in the payload rather than in the URL because the URL belongs
// to the operator, not to oto. When the shape changes incompatibly, this becomes
// oto.notification.v2 and both are emitted for a release.
const Schema = "oto.notification.v1"

// Envelope is the generic webhook payload (§H.10).
//
// It is a faithful, flat projection of the NotificationView and NOTHING ELSE.
// There is no Slack vocabulary here — no blocks, no colour, no thread, no
// mrkdwn — because the generic webhook exists to prove the Channel abstraction
// holds. If this file ever needs a Slack affordance, the abstraction is wrong and
// the SPEC changes first (R5).
type Envelope struct {
	Schema string `json:"schema"`
	Reason string `json:"reason"`
	Mode   string `json:"mode"`
	// Continued marks a root that REPLACES a card this destination already had for
	// this generation (§H.9), so a consumer can tell a recovery from a new incident.
	Continued   bool      `json:"continued,omitempty"`
	DeliveredAt time.Time `json:"delivered_at"`

	Org    Org     `json:"org"`
	Group  Group   `json:"group"`
	Alerts []Alert `json:"alerts"`
	Focus  *Alert  `json:"focus,omitempty"`
	// The key is `occurrence`, and the Go name is Case, ON PURPOSE. This envelope is
	// frozen at oto.notification.v1 (§H.10, SCOPE-BOUNDARY H-2) and ADR 0036's
	// consequences do not name this surface. A wire key is a promise to a consumer
	// oto cannot survey; it becomes `case` at oto.notification.v2, with dual-emit,
	// under its own ticket — not as fallout from an internal rename.
	Case        *Case                 `json:"occurrence,omitempty"` // vocab:allow — a FROZEN EXTERNAL WIRE SPELLING under oto.notification.v1, not internal vocabulary; the Go name is Case and only a v2 bump may move the key.
	Rule        *Rule                 `json:"rule,omitempty"`
	RuleChange  *RuleChange           `json:"rule_change,omitempty"`
	Enrichments map[string]Enrichment `json:"enrichments,omitempty"`
	Actor       *Actor                `json:"actor,omitempty"`
	Comment     string                `json:"comment,omitempty"`
	Actions     []Action              `json:"actions,omitempty"`
	Links       map[string]string     `json:"links,omitempty"`
	Previous    map[string]string     `json:"previous,omitempty"`
	Summary     string                `json:"summary"`
}

// Org identifies the tenant.
type Org struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// Group is one generation of one notification group.
type Group struct {
	ID              string            `json:"id"`
	GroupKey        string            `json:"group_key"`
	Generation      int               `json:"generation"`
	Title           string            `json:"title"`
	Receiver        string            `json:"receiver"`
	GroupLabels     map[string]string `json:"group_labels,omitempty"`
	State           string            `json:"state"`
	Severity        string            `json:"severity,omitempty"`
	FiringCount     int               `json:"firing_count"`
	SuppressedCount int               `json:"suppressed_count"`
	ResolvedCount   int               `json:"resolved_count"`
	ExpiredCount    int               `json:"expired_count"`
	TotalCount      int               `json:"total_count"`
	AckedCount      int               `json:"acked_count"`
	FirstSeenAt     time.Time         `json:"first_seen_at"`
	LastActivityAt  time.Time         `json:"last_activity_at"`
	ClusterKey      string            `json:"cluster_key,omitempty"`
	// SourceGroupKey is Alertmanager's own groupKey. It is DISPLAY ONLY and is
	// never parsed: it is unescaped, unbounded, and changes on every
	// alertmanager.yml reload (C3). A consumer must not key on it either.
	SourceGroupKey string `json:"source_group_key,omitempty"`
}

// Alert is one Alert as a consumer sees it.
type Alert struct {
	ID                string            `json:"id"`
	AlertKey          string            `json:"alert_key"`
	SourceFingerprint string            `json:"source_fingerprint,omitempty"`
	AlertName         string            `json:"alert_name"`
	Severity          string            `json:"severity,omitempty"`
	Namespace         string            `json:"namespace,omitempty"`
	Service           string            `json:"service,omitempty"`
	ClusterKey        string            `json:"cluster_key,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	GeneratorURL      string            `json:"generator_url,omitempty"`
	State             string            `json:"state"`
	AckState          string            `json:"ack_state"`
	FirstSeenAt       time.Time         `json:"first_seen_at"`
	LastSeenAt        time.Time         `json:"last_seen_at"`
	// vocab:allow — a FROZEN EXTERNAL WIRE SPELLING under oto.notification.v1, not internal vocabulary; the Go name is TotalCases and only a v2 bump may move the key (§H.10).
	TotalCases int      `json:"total_occurrences"`
	IsFlapping bool     `json:"is_flapping"`
	Value      *float64 `json:"value,omitempty"`
}

// Case is one firing episode.
type Case struct {
	ID                string     `json:"id"`
	Seq               int        `json:"seq"`
	State             string     `json:"state"`
	AckState          string     `json:"ack_state"`
	SuppressionReason string     `json:"suppression_reason,omitempty"`
	ResolveReason     string     `json:"resolve_reason,omitempty"`
	StartedAt         time.Time  `json:"started_at"`
	EndedAt           *time.Time `json:"ended_at,omitempty"`
	DurationSeconds   float64    `json:"duration_seconds"`
	// ⛔ FROZEN AT ZERO, NOT REMOVED. ADR 0040 made a Case strictly terminal, so
	// nothing can ever count a reopen again — but this envelope is frozen at
	// oto.notification.v1 (§H.10, SCOPE-BOUNDARY H-2) and DELETING a key is as
	// breaking as renaming one. It goes at oto.notification.v2, with dual-emit,
	// under its own ticket — the same argument that keeps the `occurrence` key
	// above spelled the way it is.
	ReopenCount  int        `json:"reopen_count"`
	AckedByLabel string     `json:"acked_by_label,omitempty"`
	AckedAt      *time.Time `json:"acked_at,omitempty"`
	AckNote      string     `json:"ack_note,omitempty"`
}

// Rule is what the alerting rule said when the case fired.
type Rule struct {
	SnapshotID          string            `json:"snapshot_id"`
	Fingerprint         string            `json:"fingerprint"`
	File                string            `json:"file,omitempty"`
	Group               string            `json:"group,omitempty"`
	Name                string            `json:"name,omitempty"`
	Expr                string            `json:"expr,omitempty"`
	ForSeconds          float64           `json:"for_seconds"`
	KeepFiringForSecond float64           `json:"keep_firing_for_seconds"`
	Labels              map[string]string `json:"labels,omitempty"`
	Annotations         map[string]string `json:"annotations,omitempty"`
	Origin              string            `json:"origin,omitempty"`
	MatchConfidence     string            `json:"match_confidence,omitempty"`
	CapturedAt          time.Time         `json:"captured_at"`
}

// RuleChange is what changed in the rule between cases.
type RuleChange struct {
	PreviousSnapshotID  string               `json:"previous_snapshot_id"`
	PreviousFingerprint string               `json:"previous_fingerprint"`
	PreviousCapturedAt  time.Time            `json:"previous_captured_at"`
	ExprChanged         bool                 `json:"expr_changed"`
	PreviousExpr        string               `json:"previous_expr,omitempty"`
	NewExpr             string               `json:"new_expr,omitempty"`
	ForChanged          bool                 `json:"for_changed"`
	PreviousForSeconds  float64              `json:"previous_for_seconds"`
	NewForSeconds       float64              `json:"new_for_seconds"`
	LabelDiff           map[string][2]string `json:"label_diff,omitempty"`
	AnnotationDiff      map[string][2]string `json:"annotation_diff,omitempty"`
}

// Enrichment is one enricher's provenanced result.
type Enrichment struct {
	Enricher   string         `json:"enricher"`
	Status     string         `json:"status"`
	Payload    map[string]any `json:"payload,omitempty"`
	Warnings   []string       `json:"warnings,omitempty"`
	Error      string         `json:"error,omitempty"`
	ComputedAt time.Time      `json:"computed_at"`
}

// Actor is who caused the fact. It is metadata on a signal, never a subject in
// its own right (R11).
type Actor struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Action is one affordance, rendered as a link.
//
// A non-interactive channel gets links, never buttons — that degradation is the
// dispatch service's capability negotiation (§H.10), and this struct is simply
// what a link looks like.
type Action struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	URL   string `json:"url,omitempty"`
}

func mapAlert(a domain.AlertView) Alert {
	return Alert{
		ID:                a.ID,
		AlertKey:          a.AlertKey,
		SourceFingerprint: a.SourceFingerprint,
		AlertName:         a.AlertName,
		Severity:          a.Severity,
		Namespace:         a.Namespace,
		Service:           a.Service,
		ClusterKey:        a.ClusterKey,
		Labels:            a.Labels,
		Annotations:       a.Annotations,
		GeneratorURL:      a.GeneratorURL,
		State:             a.State,
		AckState:          a.AckState,
		FirstSeenAt:       utc(a.FirstSeenAt),
		LastSeenAt:        utc(a.LastSeenAt),
		TotalCases:        a.TotalCases,
		IsFlapping:        a.IsFlapping,
		Value:             a.Value,
	}
}

func utc(t time.Time) time.Time { return t.UTC() }
