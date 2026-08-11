package promrule

import (
	"context"
	"time"

	"github.com/google/uuid"

	enrichdomain "github.com/thulasiram/oto/internal/enrichment/domain"
	enrichservice "github.com/thulasiram/oto/internal/enrichment/service"
	"github.com/thulasiram/oto/internal/platform/db"
	rulesservice "github.com/thulasiram/oto/internal/rules/service"
)

// Name is the registry id. The dot is mandatory: enrichments_name_ck rejects a
// bare word.
const Name = "prom.rule"

// Version is bumped when the payload shape or the capture semantics change.
// Bumping it invalidates every cached result and re-runs on the next occurrence
// (SPEC §F.3).
const Version = 1

// Timeout is the per-call ceiling from SPEC §F.3.
//
// 800 ms is generous for a generatorURL decode (which is string parsing) and
// tight for a /api/v1/rules round trip (which is a network call to somebody
// else's Prometheus). That asymmetry is intentional: the primary path always
// fits, and the enrichment path is allowed to lose the race.
const Timeout = 800 * time.Millisecond

// CacheTTL is how long a captured rule stays fresh in the shared cache.
//
// Five minutes is a deliberate compromise. Rules change on a deploy cadence
// measured in hours, so a longer TTL would still be correct almost always — but
// "almost always" is the wrong posture for the one feature whose entire claim
// is that it tells you when the rule CHANGED. Five minutes bounds how long oto
// can be wrong about that to less than one Alertmanager repeat interval.
const CacheTTL = 5 * time.Minute

// Snapshotter is the narrow port onto the `rules` module.
//
// The enricher does not know how a rule is recovered, whether Prometheus was
// consulted, or how a fingerprint is computed. It knows only that it can ask
// for a capture and will always get an answer — including the answer "the rule
// could not be recovered", which is a legitimate result and not an error.
type Snapshotter interface {
	Capture(ctx context.Context, s db.TenantScope, req rulesservice.CaptureRequest) (rulesservice.Capture, error)
}

// OccurrenceBinder writes the captured snapshot onto the occurrence.
//
// `alert_occurrences.rule_snapshot_id` exists for exactly this (SPEC §D.6), and
// it is what makes "show me the rule as it was when THIS fired" a single join
// rather than a time-travel query. Optional: a nil binder means the snapshot is
// still captured, versioned and reported, it is merely not pinned to the row.
type OccurrenceBinder interface {
	BindRuleSnapshot(ctx context.Context, s db.TenantScope, occurrenceID, snapshotID uuid.UUID) error
}

// Payload is the enricher's typed output, as stored in `enrichments.payload`.
//
// It is a projection, not the snapshot: the full definition lives in
// `rule_snapshots` and is fetched by id when the panel is opened. What travels
// with the alert is what a human reads in the first three seconds.
type Payload struct {
	SnapshotID  string `json:"snapshot_id"`
	Fingerprint string `json:"fingerprint"`

	RuleName  string `json:"rule_name"`
	RuleGroup string `json:"rule_group,omitempty"`
	RuleFile  string `json:"rule_file,omitempty"`

	Expr                 string  `json:"expr,omitempty"`
	ForSeconds           float64 `json:"for_seconds"`
	KeepFiringForSeconds float64 `json:"keep_firing_for_seconds"`

	// Origin, Confidence and CandidateCount are the provenance. An `ambiguous`
	// confidence MUST reach the UI and the Slack card (SPEC §D.6) — hiding it
	// would turn "we picked one of three rules with this name" into "here is
	// your rule", which is the single most misleading thing this module could
	// do.
	Origin         string `json:"origin"`
	Confidence     string `json:"match_confidence"`
	CandidateCount int    `json:"candidate_count"`
	Available      bool   `json:"available"`

	// Drifted reports that the rule was EDITED between the previous capture and
	// this one — SPEC §C.6's definition, decided by rules/domain.Drifted over
	// what the two captures both observed, so that an outage or a change of
	// recovery path in between is not rendered as somebody editing the rule.
	// PreviousFingerprint is what to diff against: the last capture that held a
	// definition, which an `unavailable` one in between does not displace.
	Drifted             bool   `json:"drifted"`
	NewVersion          bool   `json:"new_version"`
	PreviousFingerprint string `json:"previous_fingerprint,omitempty"`

	// Notes are the recovery's stable reason codes, e.g. duplicate_alertname.
	Notes []string `json:"notes,omitempty"`
}

// Enricher captures the alerting rule behind an occurrence.
//
// It is the reason this module exists. Every other alerting product shows you
// the alert; this one shows you the RULE THAT PRODUCED IT, as it was written at
// the moment it fired, and tells you when somebody has since changed the
// threshold.
type Enricher struct {
	rules  Snapshotter
	binder OccurrenceBinder
}

// Enricher satisfies the port.
var _ enrichdomain.Enricher = (*Enricher)(nil)

// New builds the enricher. A nil binder disables occurrence binding.
func New(rules Snapshotter, binder OccurrenceBinder) *Enricher {
	return &Enricher{rules: rules, binder: binder}
}

// Name is the stable registry id.
func (*Enricher) Name() string { return Name }

// Version is the payload/semantics version.
func (*Enricher) Version() int { return Version }

// Phase is inline: the rule is the single most useful piece of context on the
// first card, so it is worth spending pre-notification budget on.
func (*Enricher) Phase() enrichdomain.Phase { return enrichdomain.PhaseInline }

// Timeout is the per-call ceiling.
func (*Enricher) Timeout() time.Duration { return Timeout }

// Applicable requires an alertname. Without one there is no rule to look up:
// the alertname IS the rule name, and a lookup by anything else would be a
// guess.
func (*Enricher) Applicable(s *enrichdomain.Subject) bool {
	return s != nil && s.Alert.AlertName != ""
}

// CacheSeed lets the pipeline skip the whole capture when the same alert
// identity was resolved moments ago.
//
// The seed is the alert key plus the generatorURL, because those two together
// determine the answer: the key fixes which rule is being asked about, and the
// generatorURL is the primary path's entire input. It deliberately does NOT
// include the occurrence id — a cache keyed by occurrence would never hit.
func (*Enricher) CacheSeed(s *enrichdomain.Subject) string {
	if s == nil {
		return ""
	}
	return s.Alert.AlertKey + "\x00" + s.Alert.AlertName + "\x00" + s.Alert.GeneratorURL
}

// Enrich captures, versions and binds the rule.
//
// A rule that cannot be recovered is NOT an error. The `rules` service stores
// an `unavailable` snapshot recording that oto looked and could not see it, and
// this returns StatusPartial with a warning: the operator learns "the rule
// could not be recovered", which is a fact, rather than seeing an empty panel
// and wondering whether oto simply did not try.
func (e *Enricher) Enrich(ctx context.Context, s *enrichdomain.Subject) (enrichdomain.Result, error) {
	scope, err := enrichservice.ScopeFrom(ctx)
	if err != nil {
		return enrichdomain.Result{}, err
	}
	sourceID, err := uuid.Parse(s.Source.ID)
	if err != nil {
		return enrichdomain.Result{
			Status:   enrichdomain.StatusSkipped,
			Warnings: []string{"no_alert_source"},
		}, nil
	}
	occurrenceID, _ := uuid.Parse(s.Occurrence.ID)

	capture, err := e.rules.Capture(ctx, scope, rulesservice.CaptureRequest{
		SourceID:     sourceID,
		AlertID:      parseOrNil(s.Alert.ID),
		OccurrenceID: occurrenceID,
		Labels:       s.Alert.Labels,
		Annotations:  s.Alert.Annotations,
		GeneratorURL: s.Alert.GeneratorURL,
	})
	if err != nil {
		return enrichdomain.Result{}, err
	}

	payload := toPayload(capture)

	// The binding is best-effort on purpose. A capture that is stored but not
	// pinned still answers "what did this rule say"; a capture refused because
	// the pin failed answers nothing.
	warnings := append([]string{}, capture.Warnings...)
	if e.binder != nil && occurrenceID != uuid.Nil && capture.Snapshot.Available() {
		if snapID, perr := uuid.Parse(capture.Snapshot.ID); perr == nil {
			if berr := e.binder.BindRuleSnapshot(ctx, scope, occurrenceID, snapID); berr != nil {
				warnings = append(warnings, "snapshot_not_bound")
			}
		}
	}

	res := enrichdomain.Result{
		Status:   enrichdomain.StatusOK,
		Payload:  payload,
		Warnings: warnings,
		CacheKey: "",
		TTL:      CacheTTL,
	}
	if !capture.Recovered() {
		// Recorded, not silent, and not cached: the next fire should try again
		// rather than inherit a "we could not see it" for five minutes.
		res.Status = enrichdomain.StatusPartial
		res.TTL = 0
		res.Warnings = append(res.Warnings, "rule_unavailable")
	}
	if capture.Snapshot.Ambiguous() {
		res.Warnings = append(res.Warnings, "ambiguous_rule_match")
	}
	if capture.Drifted {
		res.Warnings = append(res.Warnings, "rule_definition_changed")
		// A drifted rule must not be served from a cache that predates the
		// change, so this result is recomputed next time.
		res.TTL = 0
	}
	return res, nil
}

func toPayload(c rulesservice.Capture) Payload {
	snap := c.Snapshot
	return Payload{
		SnapshotID:           snap.ID,
		Fingerprint:          snap.Fingerprint,
		RuleName:             snap.Key.Name,
		RuleGroup:            snap.Key.Group,
		RuleFile:             snap.Key.File,
		Expr:                 snap.Expr,
		ForSeconds:           snap.ForSeconds,
		KeepFiringForSeconds: snap.KeepFiringForSeconds,
		Origin:               string(snap.Origin),
		Confidence:           string(snap.Confidence),
		CandidateCount:       snap.CandidateCount,
		Available:            snap.Available(),
		Drifted:              c.Drifted,
		NewVersion:           c.NewVersion,
		PreviousFingerprint:  c.PreviousFingerprint,
		Notes:                c.Warnings,
	}
}

func parseOrNil(s string) uuid.UUID {
	v, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil
	}
	return v
}
