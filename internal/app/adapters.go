package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	alertsdomain "github.com/thulasiram/oto/internal/alerts/domain"
	alertsservice "github.com/thulasiram/oto/internal/alerts/service"
	enrichdomain "github.com/thulasiram/oto/internal/enrichment/domain"
	enrichrepo "github.com/thulasiram/oto/internal/enrichment/repository"
	enrichservice "github.com/thulasiram/oto/internal/enrichment/service"
	groupingdomain "github.com/thulasiram/oto/internal/grouping/domain"
	groupingservice "github.com/thulasiram/oto/internal/grouping/service"
	identitydomain "github.com/thulasiram/oto/internal/identity/domain"
	identityrepo "github.com/thulasiram/oto/internal/identity/repository"
	identityservice "github.com/thulasiram/oto/internal/identity/service"
	notifrepo "github.com/thulasiram/oto/internal/notification/repository"
	notifservice "github.com/thulasiram/oto/internal/notification/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
	rulesdomain "github.com/thulasiram/oto/internal/rules/domain"
	rulesservice "github.com/thulasiram/oto/internal/rules/service"
	"github.com/thulasiram/oto/internal/sources/rulematch"
	sourcesservice "github.com/thulasiram/oto/internal/sources/service"
	streamingdomain "github.com/thulasiram/oto/internal/streaming/domain"
	streamingservice "github.com/thulasiram/oto/internal/streaming/service"
)

// THE ADAPTERS.
//
// Every type in this file exists because two modules must not know about each
// other, and the composition root is the ONE place that may know both. Each one
// is deliberately tiny: an adapter with logic in it is a module that ended up in
// the wrong package.
//
// Three of them are late-bound (`*Late` fields set after construction) because
// the dependency graph has genuine cycles at the SERVICE level even though it
// has none at the package level — `alerts` needs a group's state_version and
// `grouping` needs the alert timeline. The cycle is broken here, in the wiring,
// rather than by widening either module's imports.

// ---------------------------------------------------------------- streaming

// streamAppender adapts `streaming/service` onto the identical `StreamAppender`
// port that `alerts/service` and `grouping/service` each declare for themselves.
//
// The kind travels as a plain string across the port — depguard keeps
// `streaming/domain` out of both consumers — so this is where it becomes a
// typed, CHECK-constrained `ui_events.kind`. An unknown kind is REFUSED rather
// than coerced: a frame the client cannot decode is worse than no frame.
//
// It is called inside the caller's transaction, so the row, its NOTIFY and the
// state change they describe commit together (§E.4).
type streamAppender struct {
	svc *streamingservice.Service
}

func (a streamAppender) Append(
	ctx context.Context, s db.TenantScope, kind string, resourceID uuid.UUID, payload []byte,
) error {
	if a.svc == nil {
		return nil
	}
	_, err := a.svc.Append(ctx, s, streamingdomain.Kind(kind), resourceID, json.RawMessage(payload))
	return err
}

// ---------------------------------------------------------------- rules

// ruleLookup is `rules/service.RuleLookup` over `sources/service.ResolveRule`.
//
// It is the adapter the rules module documents in its own port comment: rules
// owns content-addressing and diffing, sources owns generatorURL decoding and
// the /api/v1/rules match, and neither may import the other. The two result
// shapes are field-for-field equivalent, so this is a translation and nothing
// more.
//
// ⛔ A lookup that recovers nothing returns a ZERO Recovery and a NIL ERROR.
// Failing to find a rule is the normal degraded path; returning an error here
// would produce no snapshot at all, which is precisely the outcome the design
// refuses — "we looked and could not see it" has to be a recorded fact.
type ruleLookup struct {
	sources *sourcesservice.Service
}

func (l ruleLookup) Lookup(
	ctx context.Context, s db.TenantScope, req rulesservice.LookupRequest,
) (rulesdomain.Recovery, error) {
	if l.sources == nil {
		return rulesdomain.Recovery{}, nil
	}

	m, err := l.sources.ResolveRule(ctx, s, req.SourceID, sourcesservice.RuleQuery{
		Labels:         req.Labels,
		Annotations:    req.Annotations,
		GeneratorURL:   req.GeneratorURL,
		SkipPrometheus: req.SkipUpstream,
		// A federated deployment's rule lives on the Prometheus named by the
		// alert's own generatorURL, not on the source's configured one.
		FollowGeneratorURL: !req.SkipUpstream,
	})
	if err != nil {
		// Degrade rather than fail: see the contract note above.
		return rulesdomain.Recovery{}, nil
	}
	return recoveryOf(m), nil
}

// recoveryOf maps a rulematch.Match onto rules/domain.Recovery.
func recoveryOf(m rulematch.Match) rulesdomain.Recovery {
	return rulesdomain.Recovery{
		Origin:               rulesdomain.Origin(m.Origin),
		Strategy:             rulesdomain.Strategy(m.Strategy),
		Confidence:           rulesdomain.Confidence(m.Confidence),
		CandidateCount:       m.CandidateCount,
		RuleName:             m.RuleName,
		RuleGroup:            m.RuleGroup,
		RuleFile:             m.RuleFile,
		Expr:                 m.Expr,
		ForSeconds:           m.ForSeconds,
		KeepFiringForSeconds: m.KeepFiringForSeconds,
		Labels:               m.Labels,
		Annotations:          m.Annotations,
		PrometheusURL:        m.PrometheusURL,
		Notes:                m.Notes,
	}
}

// ---------------------------------------------------------------- notification

// notificationReader is `alerts/service.NotificationReader` over
// `notification/service.HistoryService`.
//
// ⛔ SPEC §I.1 binds `alerts` never to import `notification`. This struct copy is
// the entire cost of that rule, and it is worth paying: it is what lets oto run
// with notifications wired out completely.
//
// A SUPPRESSED NOTIFICATION IS IN THIS LIST. "oto decided not to tell you, and
// here is why" is the answer an operator needs; an empty page is not.
type notificationReader struct {
	svc *notifservice.HistoryService
}

func (r notificationReader) ListForAlert(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID, p db.Keyset,
) ([]alertsservice.NotificationSummary, db.Cursor, error) {
	if r.svc == nil {
		return nil, db.Cursor{}, nil
	}
	rows, cursor, err := r.svc.ListForAlert(ctx, s, alertID, p)
	if err != nil {
		return nil, db.Cursor{}, err
	}

	out := make([]alertsservice.NotificationSummary, 0, len(rows))
	for _, n := range rows {
		out = append(out, alertsservice.NotificationSummary{
			ID:               n.ID,
			GroupID:          n.GroupID,
			AlertID:          n.AlertID,
			OccurrenceID:     n.OccurrenceID,
			Reason:           n.Reason,
			Status:           n.Status,
			SuppressedReason: n.SuppressedReason,
			StateVersion:     n.StateVersion,
			CreatedAt:        n.CreatedAt,
			DeliveriesTotal:  n.DeliveriesTotal,
			DeliveriesSent:   n.DeliveriesSent,
			DeliveriesFailed: n.DeliveriesFailed,
			DeliveriesDead:   n.DeliveriesDead,
			// PolicyID, UpdatedAt, DeliveriesSkipped and DeliveriesPending are
			// ABSENT rather than substituted: the v1 read model does not track
			// them, and a projection that reports a value it did not read is
			// worse than one that reports nothing.
		})
	}
	return out, cursor, nil
}

// subjectResolver is `notification/api.SubjectResolver`: it maps an alert or an
// occurrence onto the group generation whose card would carry the fact.
//
// Routing is about the GROUP — routing two members of one group to two channels
// would split one conversation across two rooms — so a preview asked about an
// alert has to resolve that alert's group first.
type subjectResolver struct {
	alerts   *alertsservice.Service
	grouping *groupingservice.Service
}

func (r subjectResolver) GroupIDForAlert(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID,
) (uuid.UUID, error) {
	if r.grouping == nil {
		return uuid.Nil, errs.Unavailable("grouping_unavailable",
			"group resolution is not wired in this deployment", 0)
	}
	members, err := r.grouping.GroupsForAlert(ctx, s, alertID, 1)
	if err != nil {
		return uuid.Nil, err
	}
	if len(members) == 0 {
		return uuid.Nil, errs.NotFound("group_not_found", "this alert is not in any group")
	}
	return members[0].GroupID(), nil
}

func (r subjectResolver) GroupIDForOccurrence(
	ctx context.Context, s db.TenantScope, occurrenceID uuid.UUID,
) (uuid.UUID, error) {
	if r.alerts == nil {
		return uuid.Nil, errs.Unavailable("alerts_unavailable",
			"occurrence resolution is not wired in this deployment", 0)
	}
	occ, err := r.alerts.GetOccurrence(ctx, s, occurrenceID)
	if err != nil {
		return uuid.Nil, err
	}
	if occ.GroupID() == uuid.Nil {
		return uuid.Nil, errs.NotFound("group_not_found", "this occurrence is not in any group")
	}
	return occ.GroupID(), nil
}

// ---------------------------------------------------------------- enrichment

// subjectLoader is `enrichment/service.SubjectLoader`.
//
// It denormalises one occurrence into the frozen Subject an Enricher sees, plus
// the notification coordinates the async phase must quote back. Every read goes
// through a SERVICE — never another module's repository — which is why this
// lives here and not in `enrichment/repository`.
type subjectLoader struct {
	alerts   *alertsservice.Service
	grouping *groupingservice.Service
	sources  *sourcesservice.Service
	occSrc   *occurrenceSourceReader
}

func (l subjectLoader) LoadSubject(
	ctx context.Context, s db.TenantScope, occurrenceID uuid.UUID,
) (enrichservice.Loaded, error) {
	if l.alerts == nil {
		return enrichservice.Loaded{}, errs.Unavailable("alerts_unavailable",
			"the alerts service is not wired in this deployment", 0)
	}

	occ, err := l.alerts.GetOccurrence(ctx, s, occurrenceID)
	if err != nil {
		return enrichservice.Loaded{}, err
	}
	detail, err := l.alerts.Get(ctx, s, occ.AlertID())
	if err != nil {
		return enrichservice.Loaded{}, err
	}
	alert := detail.Alert

	out := enrichservice.Loaded{
		AlertID: alert.ID(),
		GroupID: occ.GroupID(),
		Subject: enrichdomain.Subject{
			OrgID:       s.OrgID().String(),
			SubjectKind: "occurrence",
			SubjectID:   occurrenceID.String(),
			Alert: enrichdomain.AlertSnapshot{
				ID:                alert.ID().String(),
				AlertKey:          alert.Key().String(),
				SourceFingerprint: alert.Fingerprint().String(),
				AlertName:         alert.AlertName(),
				Severity:          alert.Severity().String(),
				Namespace:         alert.Namespace(),
				Service:           alert.Service(),
				ClusterKey:        alert.ClusterKey().String(),
				Labels:            alert.Labels().Map(),
				Annotations:       alert.Annotations().Map(),
				GeneratorURL:      alert.GeneratorURL(),
			},
			Occurrence: enrichdomain.OccurrenceSnapshot{
				ID:             occ.ID().String(),
				Seq:            occ.Seq(),
				State:          occ.State().String(),
				StartedAt:      occ.StartedAt(),
				SourceStartsAt: occ.SourceStartsAt(),
			},
		},
	}

	// The group's state_version pins a late enrichment to the group state it was
	// minted against (§C.7), which is what stops an amended card resending an old
	// one. No group means nothing to amend, and that is not an error.
	if out.GroupID != uuid.Nil && l.grouping != nil {
		if v, verr := l.grouping.StateVersion(ctx, s, out.GroupID); verr == nil {
			out.StateVersion = v
		}
	}

	// The source is only needed by enrichers that call upstream. Failing to
	// resolve it must not fail the run: a missing SourceRef degrades promrule to
	// its generatorURL-only path, which is the documented behaviour.
	if l.occSrc != nil {
		if sourceID, ok := l.occSrc.SourceID(ctx, s, occurrenceID); ok {
			out.SourceID = sourceID
			out.Subject.Source.ID = sourceID.String()
			if l.sources != nil {
				if src, serr := l.sources.Get(ctx, s, sourceID); serr == nil {
					out.Subject.Source.ClusterID = src.ClusterID.String()
					out.Subject.Source.ClusterKey = alert.ClusterKey().String()
					out.Subject.Source.BaseURL = src.BaseURL
					out.Subject.Source.PrometheusURL = src.PrometheusURL
					out.Subject.Source.Kind = string(src.Kind)
				}
			}
		}
	}
	return out, nil
}

// occurrenceSourceReader answers "which AlertSource did this episode come from",
// through the batch port `alerts/service` already declares for the reaper guard.
type occurrenceSourceReader struct {
	resolver alertsservice.OccurrenceSourceResolver
}

func (r *occurrenceSourceReader) SourceID(
	ctx context.Context, s db.TenantScope, occurrenceID uuid.UUID,
) (uuid.UUID, bool) {
	if r == nil || r.resolver == nil {
		return uuid.Nil, false
	}
	m, err := r.resolver.SourceIDs(ctx, s, []uuid.UUID{occurrenceID})
	if err != nil {
		return uuid.Nil, false
	}
	src, ok := m[occurrenceID]
	return src, ok && src != uuid.Nil
}

// enrichmentReader is `alerts/service.EnrichmentReader` over the enrichment
// repository's own read model.
//
// A FAILED enrichment and a MISSING one stay distinguishable — that is what
// Status is for, and collapsing them here would hide the exact case the
// provenance exists to record.
type enrichmentReader struct {
	repo *enrichrepo.EnrichmentRepository
}

func (r enrichmentReader) ListForAlert(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID, occurrenceID *uuid.UUID,
) ([]alertsservice.EnrichmentSummary, error) {
	if r.repo == nil {
		return nil, nil
	}

	kind, subject := "alert", alertID
	if occurrenceID != nil && *occurrenceID != uuid.Nil {
		kind, subject = "occurrence", *occurrenceID
	}

	rows, err := r.repo.ListBySubject(ctx, s, kind, subject.String())
	if err != nil {
		return nil, err
	}

	out := make([]alertsservice.EnrichmentSummary, 0, len(rows))
	for _, e := range rows {
		summary := alertsservice.EnrichmentSummary{
			SubjectKind:     e.SubjectKind,
			SubjectID:       subject,
			Enricher:        e.Enricher,
			EnricherVersion: e.Version,
			Phase:           int(e.Phase),
			Status:          string(e.Status),
			Payload:         payloadMap(e.Payload),
			Warnings:        e.Warnings,
			Error:           e.Error,
			DurationMS:      int(e.Duration / time.Millisecond),
			FromCache:       e.FromCache,
			ComputedAt:      e.ComputedAt,
		}
		if parsed, perr := uuid.Parse(e.ID); perr == nil {
			summary.ID = parsed
		}
		if !e.ExpiresAt.IsZero() {
			expires := e.ExpiresAt
			summary.ExpiresAt = &expires
		}
		out = append(out, summary)
	}
	return out, nil
}

// payloadMap renders an enricher's typed payload as the generic object the
// alerts read model carries. A payload that will not round-trip is reported as
// an empty object rather than dropped: the enrichment RAN, and the row saying so
// is the provenance.
func payloadMap(v any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// ---------------------------------------------------------------- sources

// sourceHealth is ⭐ THE REAPER GUARD (§B.4), wired.
//
//	Losing sight of an alert is NOT the same as the alert resolving.
//
// When the sources service is absent, or cannot answer, this returns FALSE and
// the sweep HOLDS every candidate. The reaper defaults to silence, never to a
// fabricated ending.
type sourceHealth struct {
	svc *sourcesservice.Service
}

func (h sourceHealth) Healthy(ctx context.Context, s db.TenantScope, sourceID uuid.UUID) (bool, error) {
	if h.svc == nil {
		return false, nil
	}
	health, err := h.svc.Health(ctx, s, sourceID)
	if err != nil {
		return false, err
	}
	return !health.BlocksReaper(), nil
}

// ingestTokenIssuer is `sources/api.IngestTokenIssuer`.
//
// The token lives in `api_tokens`, which is the identity module's table, and
// `identity/service` deliberately mints PATs only — an ingest credential is
// scoped to one AlertSource and belongs to no user, so it cannot go through the
// PAT path. This is that mint, at the seam.
//
// ⛔ THE SECRET IS RETURNED EXACTLY ONCE. Only its sha256 is stored, and there is
// no method here that reads one back because there is nothing to read.
type ingestTokenIssuer struct {
	tokens *identityrepo.APITokenRepository
	clk    interface{ Now() time.Time }
}

func (i ingestTokenIssuer) IssueIngestToken(
	ctx context.Context, s db.TenantScope, sourceID uuid.UUID,
) (string, string, error) {
	if i.tokens == nil {
		return "", "", errs.Unavailable("identity_unavailable",
			"the identity store is not wired in this deployment", 0)
	}
	// Rotation is revoke-then-mint, in that order: a window with two live tokens
	// is a window in which a leaked one still works.
	if err := i.RevokeIngestTokens(ctx, s, sourceID); err != nil {
		return "", "", err
	}

	now := i.clk.Now().UTC()
	secret := identitydomain.SecretPrefixIngest + id.Token(identityservice.SecretEntropyBytes)
	sum := sha256.Sum256([]byte(secret))
	hash, err := identitydomain.NewTokenHash(sum[:])
	if err != nil {
		return "", "", err
	}
	prefix, err := identitydomain.PrefixOfSecret(secret)
	if err != nil {
		return "", "", err
	}

	token, err := identitydomain.NewAPIToken(identitydomain.NewAPITokenParams{
		ID:        id.New(),
		OrgID:     s.OrgID(),
		Kind:      identitydomain.TokenKindIngest,
		Name:      "ingest:" + sourceID.String(),
		Hash:      hash,
		Prefix:    prefix,
		SourceID:  sourceID,
		CreatedAt: now,
	})
	if err != nil {
		return "", "", err
	}
	if err := i.tokens.Insert(ctx, s, token); err != nil {
		return "", "", err
	}
	return secret, token.Prefix.String(), nil
}

func (i ingestTokenIssuer) RevokeIngestTokens(
	ctx context.Context, s db.TenantScope, sourceID uuid.UUID,
) error {
	if i.tokens == nil {
		return errs.Unavailable("identity_unavailable",
			"the identity store is not wired in this deployment", 0)
	}
	now := i.clk.Now().UTC()

	// An ingest token belongs to no user, so the org's ingest tokens are listed
	// and narrowed by source here. There is at most a handful per source.
	page := db.Keyset{Limit: 200}
	for {
		tokens, cursor, err := i.tokens.List(ctx, s, identitydomain.TokenKindIngest, uuid.Nil, page)
		if err != nil {
			return err
		}
		for _, t := range tokens {
			if t.SourceID != sourceID {
				continue
			}
			if _, err := i.tokens.Revoke(ctx, s, t.ID, now); err != nil {
				return err
			}
		}
		if !cursor.HasMore || cursor.IsZero() {
			return nil
		}
		page.Cursor = cursor
	}
}

// ---------------------------------------------------------------- org settings

// orgSettings serves the two SettingsReader ports — `alerts/service.Lifecycle`
// and `grouping/service.Storm` — from `orgs.settings`, which `identity` owns.
//
// A settings lookup MUST NEVER be able to stop an alert being recorded, so every
// failure falls back to the §D.1 defaults rather than propagating.
type orgSettings struct {
	svc *identityservice.Service
}

func (o orgSettings) Lifecycle(ctx context.Context, s db.TenantScope) (alertsservice.Settings, error) {
	if o.svc == nil {
		return alertsservice.DefaultSettings(), nil
	}
	org, err := o.svc.GetOrg(ctx, s)
	if err != nil {
		return alertsservice.DefaultSettings(), nil
	}
	cfg := org.Settings.Normalise()
	return alertsservice.Settings{
		RefireGrace:   cfg.RefireGrace,
		ResolveGrace:  cfg.ResolveGrace,
		FlapThreshold: cfg.FlapThreshold,
		FlapWindow:    cfg.FlapWindow,
	}, nil
}

func (o orgSettings) Storm(ctx context.Context, s db.TenantScope) (groupingdomain.StormPolicy, error) {
	if o.svc == nil {
		return groupingdomain.DefaultStormPolicy(), nil
	}
	org, err := o.svc.GetOrg(ctx, s)
	if err != nil {
		return groupingdomain.DefaultStormPolicy(), nil
	}
	cfg := org.Settings.Normalise()
	return groupingdomain.StormPolicy{
		Threshold:  cfg.StormThreshold,
		Window:     cfg.StormWindow,
		Cooldown:   cfg.StormCooldown,
		CloseDelay: cfg.GroupCloseDelay,
	}.Normalise(), nil
}

// ---------------------------------------------------------------- late binding

// groupVersions is `alerts/service.GroupVersionReader`, late-bound.
//
// `alerts` is constructed before `grouping` (grouping needs the alert timeline
// and the member actions), but `alerts` needs a group's `state_version` to mint
// a notify job's idempotency key (§C.7). The service-level cycle is broken here
// rather than by widening either module.
//
// Version 0 is the documented degraded answer: the notify worker resolves the
// real version at evaluation time.
type groupVersions struct {
	svc *groupingservice.Service
}

func (g *groupVersions) StateVersion(ctx context.Context, s db.TenantScope, groupID uuid.UUID) (int, error) {
	if g == nil || g.svc == nil {
		return 0, nil
	}
	return g.svc.StateVersion(ctx, s, groupID)
}

// lateNotificationReader is `alerts/service.NotificationReader`, late-bound for
// the same reason: `notification` is constructed after `alerts`.
type lateNotificationReader struct {
	inner notificationReader
}

func (l *lateNotificationReader) ListForAlert(
	ctx context.Context, s db.TenantScope, alertID uuid.UUID, p db.Keyset,
) ([]alertsservice.NotificationSummary, db.Cursor, error) {
	if l == nil {
		return nil, db.Cursor{}, nil
	}
	return l.inner.ListForAlert(ctx, s, alertID, p)
}

// ---------------------------------------------------------------- job scopes

// occurrenceScopes resolves the tenant that owns an occurrence, for the
// `enrich.run` worker.
//
// ⚠ It is one of the few unscoped queries in the process, and it is unscoped
// because it is the query that PRODUCES the scope: `EnrichRunArgs` names an
// occurrence and no org (§G.3). A worker must not take the tenant on trust from
// a job payload — a job row is data, and data that decided its own authorisation
// would undo the tenancy boundary — so the org comes from the SUBJECT.
type occurrenceScopes struct {
	pool *pgxpool.Pool
}

const orgOfOccurrenceSQL = `SELECT org_id FROM alert_occurrences WHERE id = $1`

func (r occurrenceScopes) ScopeForOccurrence(
	ctx context.Context, occurrenceID uuid.UUID,
) (db.TenantScope, error) {
	var orgID uuid.UUID
	if err := r.pool.QueryRow(ctx, orgOfOccurrenceSQL, occurrenceID).Scan(&orgID); err != nil {
		return db.TenantScope{}, errs.NotFound("occurrence_not_found", "no such occurrence")
	}
	return db.NewTenantScope(orgID)
}

// orgLister enumerates every tenant, for the periodic sweeps that are global.
//
// The sweeps run per org because every repository method takes a TenantScope, by
// construction. `notification/repository.ReminderRepository` already publishes
// exactly this query for its own sweep; this is the same list for the reaper,
// the group close and the flap score.
type orgLister struct {
	pool *pgxpool.Pool
}

const listOrgIDsSQL = `SELECT id FROM orgs ORDER BY id`

func (l orgLister) Scopes(ctx context.Context) ([]db.TenantScope, error) {
	rows, err := l.pool.Query(ctx, listOrgIDsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []db.TenantScope
	for rows.Next() {
		var orgID uuid.UUID
		if err := rows.Scan(&orgID); err != nil {
			return nil, err
		}
		scope, err := db.NewTenantScope(orgID)
		if err != nil {
			continue
		}
		out = append(out, scope)
	}
	return out, rows.Err()
}

// snapshotSource is the concrete `notification/service.SnapshotSource`.
//
// The notification module ships its own read model for this port and documents
// the swap: "once `alerts/service` publishes an equivalent, the wiring swaps one
// constructor". Today that constructor is here, in one place, which is the whole
// point of naming it.
func snapshotSource(repo *notifrepo.SnapshotRepository) notifservice.SnapshotSource { return repo }

// ---------------------------------------------------------------- ingestion

// alertObserver is `ingestion/service.AlertObserver` — THE ONLY WRITE PATH INTO
// `alerts` (§G.4, C18) — over `alerts/service.ObserveBatch`.
//
// It exists only because the two signatures differ by an options argument and a
// result shape: ingestion counts what was applied and inspects nothing else, by
// contract. The narrowness is deliberate on both sides.
//
// ⚠ GROUPING IS NOT RESOLVED HERE, AND IT CANNOT BE YET.
// `ObserveOptions.GroupID` names the AlertGroup generation an observation joins,
// and the §C.4 key needs the Alertmanager RECEIVER and the GROUP LABELS —
// neither of which `alerts/domain.Observation` carries. Until that port widens,
// every observation lands with no group, which means `notifications.group_id`
// can never be filled and `notify.evaluate` is never enqueued. That is the one
// gap between this wiring and an end-to-end alert, and it is left visible rather
// than papered over with a fabricated group: a group minted from a guess would
// own a Slack thread, and the wrong thread is worse than none.
type alertObserver struct {
	svc *alertsservice.Service
}

func (o alertObserver) ObserveBatch(
	ctx context.Context, s db.TenantScope, obs []alertsdomain.Observation,
) (int, error) {
	res, err := o.svc.ObserveBatch(ctx, s, obs, alertsservice.ObserveOptions{})
	if err != nil {
		return 0, err
	}
	return len(res.Outcomes), nil
}
