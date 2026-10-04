package app

// THE INVESTIGATOR'S SEAMS (ADR 0053 §3, git-bug 180a525). `investigator` imports no
// other module (CONTEXT.md §4): it declares what it reads — a Case, its timeline, the
// rule at fire time, the org's kill switch — and where it writes its Finding, and the
// adapters below satisfy those ports from the modules that own each thing.
//
// ⛔⛔ NONE OF THEM TOUCHES THE NOTIFICATION PATH. The Finding is written to the
// enrichment store and NOTHING is enqueued after it — not `notify.evaluate`, not an
// `enriched` amendment. ADR 0053 §2: a Finding changes what people read, never whether
// they are told, and git-bug 180a525's "never touches the notification path" is held
// here by there being no call to make.

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	alertsservice "github.com/thulasiram/oto/internal/alerts/service"
	enrichdomain "github.com/thulasiram/oto/internal/enrichment/domain"
	enrichrepo "github.com/thulasiram/oto/internal/enrichment/repository"
	identityservice "github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/ingestion/decode"
	investigatordomain "github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/jobs"
	rulesservice "github.com/thulasiram/oto/internal/rules/service"
	sourcesdomain "github.com/thulasiram/oto/internal/sources/domain"
	sourcesservice "github.com/thulasiram/oto/internal/sources/service"
)

// investigationCases is `investigator/service.CaseReader` and `TimelineReader` over
// `alerts/service`.
type investigationCases struct {
	alerts *alertsservice.Service
}

func (a investigationCases) InvestigationCase(
	ctx context.Context, s db.TenantScope, caseID uuid.UUID,
) (investigatordomain.CaseSubject, error) {
	c, err := a.alerts.GetCase(ctx, s, caseID)
	if err != nil {
		return investigatordomain.CaseSubject{}, err
	}
	alert, err := a.alerts.GetAlert(ctx, s, c.AlertID())
	if err != nil {
		return investigatordomain.CaseSubject{}, err
	}
	return investigatordomain.CaseSubject{
		CaseID:         c.ID(),
		Number:         c.Number(),
		AlertID:        alert.ID(),
		AlertKey:       alert.Key().String(),
		Alertname:      alert.AlertName(),
		Labels:         alert.Labels().Map(),
		Annotations:    alert.Annotations().Map(),
		State:          c.State().String(),
		StartedAt:      c.StartedAt(),
		EndedAt:        c.EndedAt(),
		RuleSnapshotID: c.RuleSnapshotID(),
	}, nil
}

// timelineSlack reaches back before a Case's start, because the event that opened it
// is recorded at — not after — the instant it started, and a clock a few seconds
// apart must not leave it out.
const timelineSlack = time.Hour

func (a investigationCases) CaseTimeline(
	ctx context.Context, s db.TenantScope, caseID uuid.UUID, limit int,
) ([]investigatordomain.TimelineEntry, error) {
	c, err := a.alerts.GetCase(ctx, s, caseID)
	if err != nil {
		return nil, err
	}
	// The read is newest first; the most recent `limit` entries are what a run is
	// told, turned oldest first so it reads as a story.
	page, err := a.alerts.CaseTimeline(ctx, s, caseID,
		db.TimeWindow{From: c.StartedAt().Add(-timelineSlack)}, db.Keyset{Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]investigatordomain.TimelineEntry, 0, len(page.Events))
	for _, e := range page.Events {
		out = append(out, investigatordomain.TimelineEntry{
			At: e.OccurredAt(), Type: e.Type().String(), Actor: e.Actor().Label(), Summary: e.Summary(),
		})
	}
	slices.Reverse(out)
	return out, nil
}

// investigationRules is `investigator/service.RuleReader` over `rules/service`.
type investigationRules struct {
	rules *rulesservice.Service
}

func (a investigationRules) RuleAtFire(
	ctx context.Context, s db.TenantScope, snapshotID uuid.UUID,
) (investigatordomain.RuleAtFire, error) {
	snap, err := a.rules.Get(ctx, s, snapshotID)
	if err != nil {
		return investigatordomain.RuleAtFire{}, err
	}
	return investigatordomain.RuleAtFire{
		CapturedAt:  snap.CapturedAt,
		Name:        snap.Key.Name,
		Group:       snap.Key.Group,
		Expr:        snap.Expr,
		For:         time.Duration(snap.ForSeconds * float64(time.Second)),
		Labels:      snap.Labels,
		Annotations: snap.Annotations,
		Origin:      string(snap.Origin),
		Confidence:  string(snap.Confidence),
		Available:   snap.Available(),
	}, nil
}

// findingPublisher is `investigator/service.FindingPublisher`: a Finding is stored as
// the Enrichment `investigator.<name>` on its Case (ADR 0053 §3), through the same
// repository and the same constructor every enricher's result goes through — so the
// cards, the API and the stream read it with nothing new.
//
// ⭐ ONE CURRENT FINDING PER INVESTIGATOR PER CASE. `enrichments_subject_uniq` is
// (subject_kind, subject_id, enricher), so a later run's Finding REPLACES the earlier
// one — "the latest is shown" (§4) — while every run's own Finding stays on its
// Investigation row. The Enrichment's version is the Investigator version, so it names
// what produced it.
type findingPublisher struct {
	repo *enrichrepo.EnrichmentRepository
}

func (p findingPublisher) PublishFinding(ctx context.Context, s db.TenantScope, f investigatordomain.PublishedFinding) error {
	status := enrichdomain.StatusOK
	if f.Partial {
		status = enrichdomain.StatusPartial
	}
	var warnings []string
	if f.Partial {
		warnings = []string{"partial: the Investigation ended at its " + string(f.Reason) + " before it concluded"}
	}
	e, err := enrichdomain.NewEnrichment(enrichdomain.EnrichmentParams{
		OrgID:       s.OrgID().String(),
		SubjectKind: enrichdomain.SubjectCase,
		SubjectID:   f.CaseID.String(),
		Enricher:    f.Enricher,
		Version:     f.Version,
		Phase:       enrichdomain.PhaseAsync,
		Status:      status,
		Payload: map[string]any{
			"investigation_id":        f.InvestigationID.String(),
			"investigator_version_id": f.VersionID.String(),
			"model":                   map[string]string{"endpoint": f.Model.Endpoint, "name": f.Model.Model},
			"status":                  string(f.Status),
			"reason":                  string(f.Reason),
			"partial":                 f.Partial,
			"summary":                 f.Summary,
			"tokens_in":               f.Spent.InputTokens,
			"tokens_out":              f.Spent.OutputTokens,
			"tool_calls":              f.ToolCalls,
		},
		Warnings:   warnings,
		Duration:   f.EndedAt.Sub(f.StartedAt),
		ComputedAt: f.EndedAt,
	})
	if err != nil {
		return err
	}
	return p.repo.UpsertMany(ctx, s, []enrichdomain.Enrichment{e})
}

// investigationSwitch is `investigator/service.OrgSwitch` over the org's settings,
// declarative overlay included (`identity/service.GetOrg`).
//
// ⛔ AN UNREADABLE SETTING IS AN ERROR, NOT A DEFAULT. The notification adapters fall
// back to shipped defaults because a settings lookup must never stop a notification;
// this is the opposite case — a kill switch that cannot be read must not be read as
// "on". The request fails, and a job retries until it can tell.
type investigationSwitch struct {
	identity *identityservice.Service
}

func (a investigationSwitch) InvestigationsEnabled(ctx context.Context, s db.TenantScope) (bool, error) {
	org, err := a.identity.GetOrg(ctx, s)
	if err != nil {
		return false, err
	}
	return org.Settings.InvestigationsEnabled, nil
}

// toolResultRedaction is `investigator/service.RedactionRules` (git-bug 2e9a086): the
// org's ingest redaction rules — every source's `redact_labels` and `redact_annotations`,
// deleted sources included — as ingest's own matcher and replacement value, so a
// ToolServer's result is redacted in the dialect an operator already wrote.
//
// ⭐ EVERY SOURCE, NOT ONE. A Tool reads the cluster, not a source; a name an operator
// called sensitive on any source of this org is sensitive in what any ToolServer says.
// A deleted source's rules count too: the data it described has not gone anywhere.
//
// ⛔ AN UNREADABLE RULE SET IS AN ERROR, NOT AN EMPTY ONE. The run's job retries rather
// than recording a result unredacted.
type toolResultRedaction struct {
	sources *sourcesservice.Service
}

func (a toolResultRedaction) ToolResultRedactor(ctx context.Context, s db.TenantScope) (investigatordomain.ResultRedactor, error) {
	var labels, annotations []string
	page := db.Keyset{Limit: db.MaxPageLimit}
	for {
		srcs, next, err := a.sources.List(ctx, s, sourcesdomain.SourceFilter{IncludeDeleted: true}, page)
		if err != nil {
			return investigatordomain.ResultRedactor{}, err
		}
		for _, src := range srcs {
			labels = append(labels, src.RedactLabels...)
			annotations = append(annotations, src.RedactAnnotations...)
		}
		if !next.HasMore {
			break
		}
		page.Cursor = next
	}
	r := decode.NewRedactor(labels, annotations)
	if !r.Enabled() {
		return investigatordomain.ResultRedactor{}, nil
	}
	return investigatordomain.NewResultRedactor(r.MatchesName, decode.RedactedValue), nil
}

// runInvestigation is `investigations.run` (ADR 0053 §3): one queued run, in its org.
//
// ⭐ A RUN WHOSE ORG IS GONE IS DONE, NOT RETRIED — `jobs.ForTenant` answers a departed
// tenant with nil, as every per-tenant job does.
func (c *Container) runInvestigation(ctx context.Context, job *jobs.Job[jobs.InvestigationsRunArgs]) error {
	if c.Investigator == nil {
		return jobs.ErrNotImplemented(jobs.KindInvestigationsRun)
	}
	return jobs.ForTenant(ctx, jobs.KindInvestigationsRun, c.orgs, job.Args.OrgID,
		func(ctx context.Context, scope db.TenantScope) error {
			err := c.Investigator.RunInvestigation(ctx, scope, job.Args.InvestigationID)
			if errs.IsKind(err, errs.KindValidation) {
				return jobs.Permanent(err)
			}
			return err
		})
}
