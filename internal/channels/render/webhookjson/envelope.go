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

	Org Org `json:"org"`
	// Group is a POINTER so that a digest can decline to assert one.
	//
	// ⛔ THE KEY IS NOT MOVED, RENAMED OR DROPPED, WHICH IS WHAT §H.10 FREEZES. Every
	// envelope that has a group still carries `group` with exactly the same members in
	// exactly the same order — a non-nil pointer marshals identically to the value it
	// replaced, and the three checked-in fixtures are byte-for-byte unchanged. What
	// changed is the ONE class of envelope that never had a group: a digest.
	//
	// ⭐ AND FOR THAT CLASS THE VALUE WAS A FABRICATION, NOT A FIELD. A digest view is
	// built by `notification/service.ViewService.digest`, which carries no group at all
	// (git-bug `78388fb`), so a zero `GroupView` mapped to `state: ""`, every count at
	// `0`, and `first_seen_at`/`last_activity_at` at `0001-01-01T00:00:00Z` — an object
	// that reads as a real, empty, ancient group to any consumer that does not know a
	// digest when it sees one. `total_count: 0` on a message asserting three new cases
	// is a positive false statement, and CONTEXT.md §3's rule against those does not
	// stop at the Slack renderer. An absent key a consumer must branch on is a smaller
	// claim than a present object that is wrong.
	Group *Group `json:"group,omitempty"`
	// Digest is the periodic summary's facts, and it is non-nil on exactly the
	// envelopes where `Group` is nil. The two are alternatives, never companions: a
	// consumer reads `digest` to know this message summarises a WINDOW rather than
	// reporting a fact about a signal, and every Case-shaped key below is absent on one.
	Digest *Digest `json:"digest,omitempty"`
	// Alerts is `[]` and never `null` on a digest: a digest names no signal, and an
	// empty list is the truthful rendering of "nothing here to enumerate". The key has
	// no `omitempty` under v1 and does not get one.
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

// Digest is one closed window summarised: a count and the span it was counted over.
//
// ⚠️⚠️ THE SPAN IS HALF-OPEN — `[covered_from, covered_to)` — AND A CONSUMER THAT
// TREATS IT AS CLOSED WILL DOUBLE-COUNT ONE CASE PER WINDOW. `covered_to` is the
// EXCLUSIVE end: it is enforced as such by `notifications_digcover_ck`
// (`covered_from <= window_start < covered_to`), it is named "the EXCLUSIVE end" by
// `digest_covered_to`'s own column comment, and the Slack card therefore prints it as
// "Up to" and never "To". The same convention is asserted here rather than re-decided,
// because two channels describing one stored pair two different ways is worse than
// either convention would have been alone. Consecutive digests from one policy ABUT;
// they do not overlap, and the Case that opened at exactly `covered_to` is in the next
// one.
//
// ⛔ THE THREE SPAN FIELDS ARE PRESENT OR ABSENT TOGETHER, AND ABSENT IS A REAL
// ANSWER. They are `nil` on a digest written before migration 00070, which did not
// store the span. Filling them in is not available: the only inference is the window
// start plus the policy's window AS IT IS TODAY, and an operator who has since
// narrowed `digest_window_s` would be told every digest oto ever sent covered a span
// none of them did (git-bug `342e071`). A consumer that sees `count` without a span
// knows the count and does not know the window, which is exactly what oto knows.
type Digest struct {
	// Count is how many Cases OPENED inside the span, read off the stored row rather
	// than recomputed — the window is closed, so there is no newer truth, and
	// `alert_cases` is reapable, so a recomputed count would shrink as episodes aged
	// out (migration 00058). It is at least 1 on anything oto sends
	// (`notifications_digest_ck`), so it is never a zero dressed up as news.
	Count int `json:"count"`
	// CoveredFrom is the INCLUSIVE start, at or before the window's own start: it
	// reaches back for Cases whose transaction committed too late for the previous
	// window's read, so the honest sentence is "since the last digest, plus
	// stragglers".
	CoveredFrom *time.Time `json:"covered_from,omitempty"`
	// CoveredTo is the EXCLUSIVE end. See the half-open warning above.
	CoveredTo *time.Time `json:"covered_to,omitempty"`
	// SpanSeconds is `covered_to - covered_from`, emitted beside the two ends for the
	// same reason `Case.duration_seconds` is emitted beside `started_at`/`ended_at`:
	// this envelope's convention is that a consumer never has to do date arithmetic to
	// get a length. It is SUBTRACTED FROM THE TWO ENDS AND NEVER READ FROM A POLICY,
	// which is the whole point of storing both — the length cannot be falsified by a
	// later edit to the policy's window, and it is usually LONGER than that window
	// because of the straggler lookback above.
	SpanSeconds *float64 `json:"span_seconds,omitempty"`
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
