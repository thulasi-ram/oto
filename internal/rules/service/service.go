package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/rules/domain"
)

// Error codes this service mints.
const (
	// CodeMissingRepository means the service was constructed without storage.
	CodeMissingRepository = "rules_missing_repository"
	// CodeNoAlertName means the capture request carried no alertname, so there
	// is no rule key to capture under.
	CodeNoAlertName = "rules_no_alertname"
	// CodeNoSource means the capture request named no AlertSource.
	CodeNoSource = "rules_no_source"
	// CodeSnapshotNotFound means no such snapshot in the caller's org.
	CodeSnapshotNotFound = "rules_snapshot_not_found"
	// CodeUnknownVersion means a diff asked for a version number the history
	// does not have.
	CodeUnknownVersion = "rules_unknown_version"
)

// The alert timeline types this service appends (SPEC §D.4.1). They are a
// CLOSED enum; implementers must not invent more.
const (
	// EventSnapshotCaptured records that a rule definition was stored.
	EventSnapshotCaptured = "rule.snapshot_captured"
	// EventDefinitionChanged records drift: the rule is not what it was.
	EventDefinitionChanged = "rule.definition_changed"
	// EventLookupFailed records that the rule could not be recovered at all.
	EventLookupFailed = "rule.lookup_failed"
)

// DefaultHistoryLimit bounds a history read. A rule with more than this many
// distinct texts is pathological, and an unbounded query on a hot path is worse
// than a truncated answer.
const DefaultHistoryLimit = 200

// LabelAlertName is the label that names the alerting rule.
const LabelAlertName = "alertname"

// Options are the Service's dependencies. Everything is a port, so the whole
// service runs against fakes with no Postgres and no Prometheus.
type Options struct {
	Repo   SnapshotRepository
	Lookup RuleLookup
	Events EventRecorder
	Clock  clock.Clock
	Logger *slog.Logger
}

// Service captures, versions and diffs alerting-rule definitions.
//
// It is the module SPEC §I.1 calls the differentiator: every other alerting
// tool shows you the alert, and oto shows you THE RULE AS IT WAS WHEN THE ALERT
// FIRED, plus how it has been edited since.
type Service struct {
	repo   SnapshotRepository
	lookup RuleLookup
	events EventRecorder
	clk    clock.Clock
	log    *slog.Logger
}

// New builds the Service.
func New(o Options) (*Service, error) {
	if o.Repo == nil {
		return nil, errs.New(errs.KindInternal, CodeMissingRepository, "a snapshot repository is required")
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.New()
	}
	lg := o.Logger
	if lg == nil {
		lg = slog.Default()
	}
	return &Service{repo: o.Repo, lookup: o.Lookup, events: o.Events, clk: clk, log: lg}, nil
}

// CaptureRequest asks for one alert's rule to be captured at fire time.
type CaptureRequest struct {
	// SourceID is the AlertSource the alert arrived from. Required.
	SourceID uuid.UUID
	// AlertID and OccurrenceID scope the timeline events. Optional.
	AlertID      uuid.UUID
	OccurrenceID uuid.UUID
	// Labels are the alert's rendered labels; alertname is required.
	Labels map[string]string
	// Annotations are the alert's rendered annotations.
	Annotations map[string]string
	// GeneratorURL is the primary recovery path's whole input.
	GeneratorURL string
	// SkipUpstream forces the zero-API-call path.
	SkipUpstream bool
}

// Capture is the outcome of one capture, including the facts that make it
// interesting: whether this is a rule oto had not seen in this form before, and
// whether it differs from the version the previous fire was bound to.
type Capture struct {
	// Snapshot is the stored row. It is ALWAYS present on a nil error, even
	// when nothing could be recovered — in that case Origin is `unavailable`
	// and Available() is false.
	Snapshot domain.Snapshot
	// Recovery is what the upstream lookup returned, before storage.
	Recovery domain.Recovery
	// NewVersion reports that this exact rule text had not been stored before.
	NewVersion bool
	// Drifted reports that the rule differs from the previous capture for this
	// rule key: SPEC §C.6's definition of drift.
	Drifted bool
	// PreviousFingerprint is the content address this rule had before, empty on
	// a first capture.
	PreviousFingerprint string
	// Warnings are the lookup's stable note codes plus any degradation this
	// service applied. They are shown, never swallowed.
	Warnings []string
}

// Recovered reports whether the capture holds an actual rule definition.
func (c Capture) Recovered() bool { return c.Snapshot.Available() }

// Capture recovers, content-addresses and stores the rule behind one alert.
//
// It is written to DEGRADE, never to fail. A Prometheus that is down, an alert
// with no generatorURL, a rule that has been deleted since it fired — all of
// them produce a stored `unavailable` snapshot with the reason recorded, and a
// nil error. The only errors this method returns are a malformed request and a
// storage failure, because those are the two cases where pretending would put a
// lie in the database.
func (s *Service) Capture(ctx context.Context, scope db.TenantScope, req CaptureRequest) (Capture, error) {
	if req.SourceID == uuid.Nil {
		return Capture{}, errs.New(errs.KindValidation, CodeNoSource,
			"a rule capture must name the alert source it came from")
	}
	name := strings.TrimSpace(req.Labels[LabelAlertName])
	if name == "" {
		return Capture{}, errs.New(errs.KindValidation, CodeNoAlertName,
			"a rule capture requires an alertname label")
	}

	rec := s.lookupRule(ctx, scope, req)

	key := domain.Key{
		SourceID: req.SourceID.String(),
		File:     rec.RuleFile,
		Group:    rec.RuleGroup,
		Name:     name,
	}
	snap := domain.NewSnapshot(scope.OrgID().String(), key, rec, s.clk.Now())
	if err := snap.Validate(); err != nil {
		// An invalid snapshot would become a CHECK violation and therefore a
		// 500. Degrade to a well-formed `unavailable` row instead: a snapshot
		// that says "we could not see it" is honest, a fabricated one is not.
		s.log.ErrorContext(ctx, "rules: snapshot failed its own invariants, degrading to unavailable",
			"source_id", req.SourceID, "alertname", name, "error", err)
		rec = domain.Recovery{
			Origin:     domain.OriginUnavailable,
			Strategy:   domain.StrategyNone,
			Confidence: domain.ConfidenceNone,
			Notes:      append(append([]string{}, rec.Notes...), "snapshot_invariant_violation"),
		}
		snap = domain.NewSnapshot(scope.OrgID().String(), domain.Key{
			SourceID: req.SourceID.String(), Name: name,
		}, rec, s.clk.Now())
		if err := snap.Validate(); err != nil {
			return Capture{}, err
		}
	}

	// The previous capture is read BEFORE the write so that "the newest
	// snapshot for this rule key" means the one the last fire saw, not the one
	// this fire just created.
	previous, hadPrevious, err := s.repo.Latest(ctx, scope, snap.Key)
	if err != nil {
		return Capture{}, err
	}

	stored, inserted, err := s.repo.Upsert(ctx, scope, snap)
	if err != nil {
		return Capture{}, err
	}

	out := Capture{
		Snapshot:   stored,
		Recovery:   rec,
		NewVersion: inserted,
		Warnings:   append([]string{}, rec.Notes...),
	}
	if hadPrevious {
		out.PreviousFingerprint = previous.Fingerprint
		out.Drifted = previous.Fingerprint != stored.Fingerprint
	}

	s.narrate(ctx, scope, req, out)
	return out, nil
}

// lookupRule performs the upstream lookup, absorbing every failure.
func (s *Service) lookupRule(ctx context.Context, scope db.TenantScope, req CaptureRequest) domain.Recovery {
	if s.lookup == nil {
		return domain.Recovery{
			Origin:     domain.OriginUnavailable,
			Strategy:   domain.StrategyNone,
			Confidence: domain.ConfidenceNone,
			Notes:      []string{"rule_lookup_not_configured"},
		}
	}

	rec, err := s.lookup.Lookup(ctx, scope, LookupRequest{
		SourceID:     req.SourceID,
		Labels:       req.Labels,
		Annotations:  req.Annotations,
		GeneratorURL: req.GeneratorURL,
		SkipUpstream: req.SkipUpstream,
	})
	if err != nil {
		s.log.WarnContext(ctx, "rules: rule lookup failed, capturing as unavailable",
			"source_id", req.SourceID, "code", errs.CodeOf(err), "error", err)
		return domain.Recovery{
			Origin:     domain.OriginUnavailable,
			Strategy:   domain.StrategyNone,
			Confidence: domain.ConfidenceNone,
			Notes:      []string{"rule_lookup_failed"},
		}
	}
	return rec
}

// narrate appends the timeline events for one capture. It never fails the
// capture: a timeline is a record of what happened, and failing the thing that
// happened because it could not be written down is backwards.
func (s *Service) narrate(ctx context.Context, scope db.TenantScope, req CaptureRequest, c Capture) {
	if s.events == nil {
		return
	}
	if req.AlertID == uuid.Nil && req.OccurrenceID == uuid.Nil {
		return
	}

	snapID := uuid.Nil
	if id, err := uuid.Parse(c.Snapshot.ID); err == nil {
		snapID = id
	}

	emit := func(typ, summary string, payload map[string]any, dedupe string) {
		if err := s.events.RecordRuleEvent(ctx, scope, RuleEvent{
			Type:         typ,
			AlertID:      req.AlertID,
			OccurrenceID: req.OccurrenceID,
			SnapshotID:   snapID,
			Summary:      clampSummary(summary),
			Payload:      payload,
			DedupeKey:    dedupe,
		}); err != nil {
			s.log.WarnContext(ctx, "rules: could not record rule event",
				"type", typ, "error", err)
		}
	}

	base := map[string]any{
		"rule_name":       c.Snapshot.Key.Name,
		"rule_group":      c.Snapshot.Key.Group,
		"rule_file":       c.Snapshot.Key.File,
		"origin":          string(c.Snapshot.Origin),
		"confidence":      string(c.Snapshot.Confidence),
		"candidate_count": c.Snapshot.CandidateCount,
		"strategy":        string(c.Recovery.Strategy),
		"notes":           c.Warnings,
	}

	if !c.Recovered() {
		emit(EventLookupFailed,
			fmt.Sprintf("rule %q could not be recovered", c.Snapshot.Key.Name),
			base, dedupeKey("rule_lookup_failed", req.OccurrenceID, c.Snapshot.Fingerprint))
		return
	}

	emit(EventSnapshotCaptured,
		fmt.Sprintf("captured rule %q (%s, %s match)",
			c.Snapshot.Key.Name, c.Snapshot.Origin, c.Snapshot.Confidence),
		base, dedupeKey("rule_captured", req.OccurrenceID, c.Snapshot.Fingerprint))

	if c.Drifted {
		drift := make(map[string]any, len(base)+2)
		for k, v := range base {
			drift[k] = v
		}
		drift["previous_fingerprint"] = c.PreviousFingerprint
		drift["fingerprint"] = c.Snapshot.Fingerprint
		emit(EventDefinitionChanged,
			fmt.Sprintf("rule %q changed since the previous fire", c.Snapshot.Key.Name),
			drift, dedupeKey("rule_changed", req.OccurrenceID, c.Snapshot.Fingerprint))
	}
}

func dedupeKey(prefix string, occurrenceID uuid.UUID, fingerprint string) string {
	fp := fingerprint
	if len(fp) > 16 {
		fp = fp[:16]
	}
	return fmt.Sprintf("%s:%s:%s", prefix, occurrenceID, fp)
}

// clampSummary enforces ev_summary_ck (1..500 bytes).
func clampSummary(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "rule capture"
	}
	if len(s) > 500 {
		return s[:500]
	}
	return s
}

// Get returns one stored snapshot.
func (s *Service) Get(ctx context.Context, scope db.TenantScope, id uuid.UUID) (domain.Snapshot, error) {
	return s.repo.Get(ctx, scope, id)
}

// GetMany returns the stored snapshots for a set of ids, in one round trip.
//
// ⭐ THIS IS WHAT THE ALERT LIST READS. `include=rule` gives the list one
// snapshot id per row and nothing else, because `alerts/api` may not name this
// module's types (CONTEXT.md §5.4); asking `/alerts/{id}/rule` per row is the
// N+1 that kept `expr` off the busiest screen in the product. One call with the
// page's ids answers all of them — and usually with far fewer rows than ids,
// since a rule that has not changed is ONE content-addressed snapshot however
// many alerts fired under it (ADR 0025).
//
// ⛔ An unknown id is silently absent, never a 404. A single stale id must not
// be able to blank the rule column of an entire page.
func (s *Service) GetMany(ctx context.Context, scope db.TenantScope, ids []uuid.UUID) ([]domain.Snapshot, error) {
	if len(ids) == 0 {
		return []domain.Snapshot{}, nil
	}
	// Duplicates are the NORMAL case here, not a caller error: fifty alerts
	// firing under one unchanged rule send fifty copies of one id. They are
	// collapsed before the query so the batch costs what the distinct set costs.
	seen := make(map[uuid.UUID]struct{}, len(ids))
	unique := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id == uuid.Nil {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return []domain.Snapshot{}, nil
	}
	return s.repo.GetMany(ctx, scope, unique)
}

// Latest returns the newest capture for a rule key. The bool is false when the
// rule has never been captured, which is a state and not an error.
func (s *Service) Latest(ctx context.Context, scope db.TenantScope, key domain.Key) (domain.Snapshot, bool, error) {
	return s.repo.Latest(ctx, scope, key)
}

// Addressable reports whether a RuleKey can name rows in the snapshot store.
//
// ⭐ IT IS THE DIFFERENCE BETWEEN "A KEY THAT FINDS NOTHING" AND "NOT A KEY AT
// ALL", and the two must not be confused. `rule_snapshots` is indexed on
// `(org_id, source_id, rule_name, …)`, so a key with no AlertSource behind it
// cannot be turned into a predicate: the repository refuses it as a VALIDATION
// error, which is exactly right when a caller typed the source id and exactly
// wrong when oto derived the key itself and had nothing to derive it from.
//
// That second case is ordinary and expected. `rules/api.keyOf` builds a key
// from an alert's `alertname` alone when no snapshot was ever bound to the
// episode — a Grafana-sourced alert, a hand-fired one, an Alertmanager whose
// Prometheus is unreachable. Asking such a key for its history is not a client
// mistake; the honest answer is "oto has captured nothing here", and this
// predicate is what lets callers say that instead of raising a 422.
//
// The nil UUID is not addressable either. It parses, so the repository would
// happily build a predicate from it, but no AlertSource is ever the zero id —
// a query under it is a round trip that cannot match.
func Addressable(key domain.Key) bool {
	if strings.TrimSpace(key.Name) == "" {
		return false
	}
	id, err := uuid.Parse(key.SourceID)
	return err == nil && id != uuid.Nil
}

// History returns the rule's numbered edit history, oldest first.
//
// The versions are the DISTINCT TEXTS the rule has had, not the fires: a rule
// that fired ten thousand times unchanged has exactly one version, and a rule
// whose threshold was doubled last Tuesday has two.
//
// ⛔ AN UNADDRESSABLE KEY IS AN EMPTY HISTORY, NEVER AN ERROR. See Addressable:
// the key oto derives from an alert with no captured snapshot names no source,
// and pushing it down to the repository turns "we captured nothing" into "your
// request was invalid" — a 422 on the one screen the product exists for.
func (s *Service) History(ctx context.Context, scope db.TenantScope, key domain.Key) (domain.History, error) {
	if !Addressable(key) {
		return domain.NewHistory(key, nil), nil
	}
	snaps, err := s.repo.ListByKey(ctx, scope, key, DefaultHistoryLimit)
	if err != nil {
		return domain.History{}, err
	}
	return domain.NewHistory(key, snaps), nil
}

// SnapshotPage is one keyset page of a rule's capture history.
type SnapshotPage struct {
	Snapshots []domain.Snapshot
	Cursor    db.Cursor
}

// ListSnapshots serves `GET /api/v1/rule-snapshots` — the version history for
// one RuleKey, newest first, KEYSET PAGINATED.
//
// ⭐ It is not History(). History rebuilds the NUMBERED edit history, which only
// exists relative to the whole set — version 1 is the oldest capture — and is
// therefore capped at DefaultHistoryLimit and returned whole. Serving the list
// endpoint from it meant `next_cursor` was structurally always null and a rule
// captured more than two hundred times had history the API could not reach.
func (s *Service) ListSnapshots(
	ctx context.Context, scope db.TenantScope, key domain.Key, p db.Keyset,
) (SnapshotPage, error) {
	snaps, cur, err := s.repo.ListPage(ctx, scope, key, p)
	if err != nil {
		return SnapshotPage{}, err
	}
	return SnapshotPage{Snapshots: snaps, Cursor: cur}, nil
}

// DiffVersions compares two numbered versions of one rule.
//
// This is the product's headline read: "the rule as it was when this fired, and
// how the threshold has drifted since".
func (s *Service) DiffVersions(ctx context.Context, scope db.TenantScope, key domain.Key, from, to int) (domain.Diff, error) {
	h, err := s.History(ctx, scope, key)
	if err != nil {
		return domain.Diff{}, err
	}
	d, ok := h.DiffVersions(from, to)
	if !ok {
		return domain.Diff{}, errs.Newf(errs.KindNotFound, CodeUnknownVersion,
			"rule %q has %d versions; %d..%d is out of range", key.Name, h.Len(), from, to)
	}
	return d, nil
}

// DiffSince compares the version an occurrence was bound to against the newest
// one, which is what the alert card needs to say "this rule has changed since
// this alert last fired".
//
// The bool is false when there is nothing to compare: no history, or the bound
// fingerprint is the newest one.
func (s *Service) DiffSince(ctx context.Context, scope db.TenantScope, key domain.Key, boundFingerprint string) (domain.Diff, bool, error) {
	h, err := s.History(ctx, scope, key)
	if err != nil {
		return domain.Diff{}, false, err
	}
	bound, ok := h.ByFingerprint(boundFingerprint)
	if !ok {
		return domain.Diff{}, false, nil
	}
	latest, ok := h.Latest()
	if !ok || latest.Number == bound.Number {
		return domain.Diff{}, false, nil
	}
	return domain.Compare(bound.Snapshot, latest.Snapshot), true, nil
}

// DiffSnapshots compares two snapshots by id, oldest capture first.
func (s *Service) DiffSnapshots(ctx context.Context, scope db.TenantScope, a, b uuid.UUID) (domain.Diff, error) {
	first, err := s.repo.Get(ctx, scope, a)
	if err != nil {
		return domain.Diff{}, err
	}
	second, err := s.repo.Get(ctx, scope, b)
	if err != nil {
		return domain.Diff{}, err
	}
	if second.CapturedAt.Before(first.CapturedAt) {
		first, second = second, first
	}
	return domain.Compare(first, second), nil
}
