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
//
// ⚠️ ONE DECLARATION, AND IT IS NOT A DECISION. An INCIDENT's new Finding is declared
// outbound as the Incident fact `finding` (ADR 0052 §5, git-bug 74ea849) through
// findingDeclarer below — a separate port, so the publish above stays inert. It says
// the Incident has a new Finding; it holds, suppresses or adds nothing about any
// Case's or Alert's own notifications.

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	alertsservice "github.com/thulasiram/oto/internal/alerts/service"
	enrichdomain "github.com/thulasiram/oto/internal/enrichment/domain"
	enrichrepo "github.com/thulasiram/oto/internal/enrichment/repository"
	identityservice "github.com/thulasiram/oto/internal/identity/service"
	incidentsdomain "github.com/thulasiram/oto/internal/incidents/domain"
	incidentsservice "github.com/thulasiram/oto/internal/incidents/service"
	"github.com/thulasiram/oto/internal/ingestion/decode"
	investigatordomain "github.com/thulasiram/oto/internal/investigator/domain"
	investigatorservice "github.com/thulasiram/oto/internal/investigator/service"
	notifdomain "github.com/thulasiram/oto/internal/notification/domain"
	notifrepo "github.com/thulasiram/oto/internal/notification/repository"
	notifservice "github.com/thulasiram/oto/internal/notification/service"
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

// investigationIncidents is `investigator/service.IncidentReader` over
// `incidents/service` (git-bug 74ea849): the Incident a run is about, as the copy the
// investigator module declares, and the Incident a Case is in.
type investigationIncidents struct {
	incidents *incidentsservice.Service
}

func (a investigationIncidents) InvestigationIncident(
	ctx context.Context, s db.TenantScope, incidentID uuid.UUID,
) (investigatordomain.IncidentSubject, error) {
	d, err := a.incidents.GetByID(ctx, s, incidentID)
	if err != nil {
		return investigatordomain.IncidentSubject{}, err
	}
	return incidentSubject(d), nil
}

func (a investigationIncidents) InvestigationIncidentNumbered(
	ctx context.Context, s db.TenantScope, number int64,
) (investigatordomain.IncidentSubject, error) {
	d, err := a.incidents.Get(ctx, s, number)
	if err != nil {
		return investigatordomain.IncidentSubject{}, err
	}
	return incidentSubject(d), nil
}

func (a investigationIncidents) HoldingIncident(ctx context.Context, s db.TenantScope, caseID uuid.UUID) (uuid.UUID, error) {
	held, err := a.incidents.HoldingCase(ctx, s, caseID)
	if err != nil || len(held) == 0 {
		return uuid.Nil, err
	}
	return held[0].ID, nil
}

// incidentSubject is the struct copy the boundary costs.
func incidentSubject(d incidentsdomain.Detail) investigatordomain.IncidentSubject {
	drawnBy := d.DrawnBy.Label()
	if !d.DrawnBy.IsHuman() {
		drawnBy = "a Correlator (" + d.DrawnBy.CorrelatorID().String() + ")"
	}
	out := investigatordomain.IncidentSubject{
		IncidentID: d.ID,
		Number:     d.Number,
		Active:     d.OpenMemberCount > 0,
		DrawnAt:    d.DrawnAt,
		DrawnBy:    drawnBy,
		Members:    make([]investigatordomain.IncidentMember, 0, len(d.Members)),
	}
	for _, m := range d.Members {
		out.Members = append(out.Members, investigatordomain.IncidentMember{
			CaseID:     m.CaseID,
			CaseNumber: m.CaseNumber,
			Alertname:  m.Alertname,
			Labels:     m.Labels,
			State:      m.CaseState.String(),
			AddedAt:    m.AddedAt,
			RemovedAt:  m.RemovedAt,
		})
	}
	return out
}

// findingDeclarer is `investigator/service.FindingDeclarer` over the outbox: an
// Incident's new Finding is one `notify.incident` job with the Reason `finding`, keyed
// on the Investigation as its occasion, enqueued in the transaction that recorded the
// Finding (ADR 0052 §5, git-bug 74ea849). A redelivered run's ending is the same
// occasion and the same key; a second run is a second Finding and a second fact.
//
// ⛔ WHETHER IT GOES ANYWHERE IS A POLICY'S QUESTION. An org with no policy naming
// `finding` records the intent `no_policy` and sends nothing.
type findingDeclarer struct {
	enq db.Enqueuer
}

func (d findingDeclarer) DeclareIncidentFinding(ctx context.Context, _ db.TenantScope, incidentID, investigationID uuid.UUID) error {
	_, err := d.enq.Enqueue(ctx, jobs.NotifyIncidentArgs{
		IncidentID: incidentID,
		Reason:     string(notifdomain.ReasonFinding),
		OccasionID: investigationID,
	})
	return err
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
// the Enrichment `investigator.<name>` on its subject — the Case, or the Incident
// (ADR 0053 §3, §4) — through the same
// repository and the same constructor every enricher's result goes through — so the
// cards, the API and the stream read it with nothing new.
//
// ⭐ ONE CURRENT FINDING PER INVESTIGATOR PER SUBJECT. `enrichments_subject_uniq` is
// (subject_kind, subject_id, enricher), so a later run's Finding REPLACES the earlier
// one — "the latest is shown" (§4) — while every run's own Finding stays on its
// Investigation row. The Enrichment's version is the Investigator version, so it names
// what produced it.
type findingPublisher struct {
	repo *enrichrepo.EnrichmentRepository
}

func (p findingPublisher) PublishFinding(ctx context.Context, s db.TenantScope, f investigatordomain.PublishedFinding) error {
	subjectKind := enrichdomain.SubjectCase
	if f.SubjectKind == investigatordomain.SubjectIncident {
		// ⭐ ON THE INCIDENT, NOT ON ANY OF ITS CASES (migration 00095): the run looked
		// at the story, and its Finding is about the story.
		subjectKind = enrichdomain.SubjectIncident
	}
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
		SubjectKind: subjectKind,
		SubjectID:   f.SubjectID.String(),
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
			// ⭐ THE CLASS TRAVELS WITH THE FINDING (ADR 0053 §5): one of the operator's
			// classes or `unclassified`, and null when the org had none — so a Case's
			// webhook envelope carries it in `enrichments` exactly as the card's API does.
			"classification": nullableClass(f.Classification),
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

// nullableClass is a Finding's classification as JSON: null when none was asked for,
// never "" — an empty string would read as a class with no name.
func nullableClass(c string) any {
	if c == "" {
		return nil
	}
	return c
}

// investigationControls is `investigator/service.OrgControls` over the org's settings,
// declarative overlay included (`identity/service.GetOrg`): the kill switch, the daily
// token budget and the concurrency (ADR 0053 §6), as their effective — bounded and
// clamped — values.
//
// ⛔ AN UNREADABLE SETTING IS AN ERROR, NOT A DEFAULT. The notification adapters fall
// back to shipped defaults because a settings lookup must never stop a notification;
// this is the opposite case — a kill switch or a budget that cannot be read must not be
// read as "on" or "unspent". The request fails, and a job retries until it can tell.
type investigationControls struct {
	identity *identityservice.Service
}

func (a investigationControls) InvestigationControls(ctx context.Context, s db.TenantScope) (investigatordomain.OrgControls, error) {
	org, err := a.identity.GetOrg(ctx, s)
	if err != nil {
		return investigatordomain.OrgControls{}, err
	}
	return investigatordomain.OrgControls{
		Enabled:     org.Settings.InvestigationsEnabled,
		DailyTokens: int64(org.Settings.InvestigationDailyTokens),
		Concurrency: org.Settings.InvestigationConcurrency,
	}, nil
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

// triggerIncidentInvestigations is `investigations.incident` (ADR 0053 §4, git-bug
// 74ea849): one Incident fact, turned into the runs of the Investigators that
// investigate Incidents. A trigger this release does not know is a bug in the producer
// and is not retried; an org that is gone is done, as for every per-tenant job.
func (c *Container) triggerIncidentInvestigations(ctx context.Context, job *jobs.Job[jobs.InvestigationsIncidentArgs]) error {
	if c.Investigator == nil {
		return jobs.ErrNotImplemented(jobs.KindInvestigationsIncident)
	}
	return jobs.ForTenant(ctx, jobs.KindInvestigationsIncident, c.orgs, job.Args.OrgID,
		func(ctx context.Context, scope db.TenantScope) error {
			_, err := c.Investigator.IncidentChanged(ctx, scope, job.Args.IncidentID,
				investigatordomain.Trigger(job.Args.Trigger))
			if errs.IsKind(err, errs.KindValidation) {
				return jobs.Permanent(err)
			}
			return err
		})
}

// ---------------------------------------------------------------- Suggestions
//
// git-bug 8327c00: a Finding's Suggestions are applied by a human through the ORDINARY
// edit. These two adapters are where "ordinary" is made literal — each calls the very
// service method a human's own request calls, and nothing else.

// policyReads is the half of the notification settings store a Suggestion reads policies
// through, satisfied by `*notification/repository.ConfigRepository`.
type policyReads interface {
	ListPolicies(ctx context.Context, s db.TenantScope, p db.Keyset) ([]notifdomain.Policy, db.Cursor, error)
	GetPolicy(ctx context.Context, s db.TenantScope, id uuid.UUID) (notifdomain.Policy, error)
}

// policyEdits is the policy edit a human's `PATCH /notification-policies/{id}` goes
// through, satisfied by `*notification/service.PolicyWriter`.
type policyEdits interface {
	UpdatePolicy(ctx context.Context, s db.TenantScope, id uuid.UUID, p notifdomain.PolicyPatch) (notifdomain.Policy, error)
}

// suggestionPolicies is `investigator/service.PolicyEditor`: notification policies as a
// Suggestion names them, and the count-condition edit.
//
// ⭐⭐ THE EDIT IS `PolicyWriter.UpdatePolicy`, THE PATCH'S OWN. The patch it builds is the
// one `{"count_min": n, "count_window_seconds": w}` binds to, so the merged validation, the
// refusal of a deleted policy and the write are the hand edit's, byte for byte. It decides
// nothing about any notification: a count condition is a column an operator can read back
// and clear with one PATCH (ADR 0044 §3), and a human pressed apply.
type suggestionPolicies struct {
	reads  policyReads
	writes policyEdits
}

func (a suggestionPolicies) SuggestionPolicy(ctx context.Context, s db.TenantScope, policyID uuid.UUID) (investigatordomain.PolicyTarget, error) {
	p, err := a.reads.GetPolicy(ctx, s, policyID)
	if err != nil {
		return investigatordomain.PolicyTarget{}, err
	}
	if p.DeletedAt != nil {
		return investigatordomain.PolicyTarget{}, errs.NotFound("policy_deleted", "this policy has been deleted")
	}
	return policyTarget(p), nil
}

func (a suggestionPolicies) SuggestionPolicies(ctx context.Context, s db.TenantScope) ([]investigatordomain.PolicyTarget, error) {
	var out []investigatordomain.PolicyTarget
	page := db.Keyset{Limit: db.MaxPageLimit}
	for {
		ps, next, err := a.reads.ListPolicies(ctx, s, page)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			out = append(out, policyTarget(p))
		}
		if !next.HasMore {
			return out, nil
		}
		page.Cursor = next
	}
}

func (a suggestionPolicies) ApplyCountCondition(
	ctx context.Context, s db.TenantScope, policyID uuid.UUID, countMin int, window time.Duration,
) error {
	n, w := &countMin, &window
	_, err := a.writes.UpdatePolicy(ctx, s, policyID, notifdomain.PolicyPatch{CountMin: &n, CountWindow: &w})
	return err
}

// policyTarget is the struct copy the boundary costs.
func policyTarget(p notifdomain.Policy) investigatordomain.PolicyTarget {
	kinds := make([]string, 0, len(p.Subjects))
	for _, k := range p.Subjects {
		kinds = append(kinds, string(k))
	}
	return investigatordomain.PolicyTarget{ID: p.ID, Name: p.Name, SubjectKinds: kinds,
		CountMin: p.Count.Min, CountWindow: p.Count.Window}
}

// suggestedMemberships is `investigator/service.MembershipEditor` over `incidents/service`:
// a membership Suggestion applied is the Incident service's own Add — or its Move, when the
// Case is in another Incident — attributed to the human who applied it, with the
// Investigation as provenance on the Case's timeline fact (`suggested_by_investigation_id`).
// The verbs, the locks, the outbound facts and the at-most-one rule are the hand edit's.
type suggestedMemberships struct {
	incidents *incidentsservice.Service
}

func (a suggestedMemberships) ApplySuggestedMembership(
	ctx context.Context, s db.TenantScope, m investigatordomain.AppliedMembership,
) error {
	human, err := incidentsdomain.Human(m.By.UserID, m.By.Label)
	if err != nil {
		return err
	}
	by, err := human.Suggested(m.SuggestedBy)
	if err != nil {
		return err
	}
	if m.FromNumber != 0 {
		_, err = a.incidents.Move(ctx, s, m.FromNumber, m.IncidentNumber, m.CaseID, by)
		return err
	}
	_, err = a.incidents.Add(ctx, s, m.IncidentNumber, m.CaseID, by)
	return err
}

// Compile-time proof that the PATCH's service satisfies the edit port the adapter holds.
var _ policyEdits = (*notifservice.PolicyWriter)(nil)

// ---------------------------------------------------------------- digest windows
//
// git-bug 3e96f5a: a digest carries a Finding that was ready when its window closed, and
// never waits for one that was not (ADR 0053 §4). Two adapters, one each way, and neither
// lets one module wait on the other.

// digestPolicies is the half of the notification policy store the digest adapters read,
// satisfied by `*notification/repository.PolicyRepository`.
type digestPolicies interface {
	ListWithDigest(ctx context.Context, s db.TenantScope) ([]notifdomain.Policy, error)
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (notifdomain.Policy, error)
}

// digestCases is the digest store's span read, satisfied by
// `*notification/repository.DigestRepository`.
type digestCases interface {
	Cases(ctx context.Context, s db.TenantScope, from, to time.Time, limit int) ([]notifrepo.DigestCase, error)
}

// digestCaseReadLimit bounds one window's read, as the digest tick bounds its own.
const digestCaseReadLimit = 5000

// investigationDigests is `investigator/service.DigestReader` over notification's
// policies and its digest store.
//
// ⭐ THE WINDOW IS THE POLICY'S OWN ARITHMETIC (`notification/domain.Digest.WindowStart`)
// and THE SELECTION IS THE POLICY'S OWN MATCHERS (`Policy.Matches`) — the very two the
// digest tick uses — so the run summarises the set the digest counts, not a lookalike.
// ⛔ IT READS AND NOTHING ELSE: there is no method here that sends, holds or marks.
type investigationDigests struct {
	policies digestPolicies
	cases    digestCases
}

func (a investigationDigests) SummarisedDigests(
	ctx context.Context, s db.TenantScope, now time.Time,
) ([]investigatordomain.SummarisedDigest, error) {
	policies, err := a.policies.ListWithDigest(ctx, s)
	if err != nil {
		return nil, err
	}
	out := make([]investigatordomain.SummarisedDigest, 0, len(policies))
	for _, p := range policies {
		if !p.Digests() || p.Digest.InvestigatorID == uuid.Nil {
			continue
		}
		start := p.Digest.WindowStart(now)
		w, err := investigatordomain.NewDigestWindow(start, p.Digest.WindowEnd(start))
		if err != nil {
			return nil, err
		}
		out = append(out, investigatordomain.SummarisedDigest{
			PolicyID: p.ID, PolicyName: p.Name, InvestigatorID: p.Digest.InvestigatorID, Window: w,
		})
	}
	return out, nil
}

func (a investigationDigests) InvestigationDigest(
	ctx context.Context, s db.TenantScope, policyID uuid.UUID, window investigatordomain.DigestWindow,
) (investigatordomain.DigestSubject, error) {
	p, err := a.policies.Get(ctx, s, policyID)
	if err != nil {
		return investigatordomain.DigestSubject{}, err
	}
	if !p.Live() || !p.Digests() {
		return investigatordomain.DigestSubject{}, errs.NotFound("policy_not_found",
			"the notification policy no longer sends a digest")
	}
	rows, err := a.cases.Cases(ctx, s, window.Start, window.End, digestCaseReadLimit)
	if err != nil {
		return investigatordomain.DigestSubject{}, err
	}
	out := investigatordomain.DigestSubject{PolicyID: p.ID, PolicyName: p.Name, Window: window}
	for _, c := range rows {
		// A matcher that cannot be evaluated selects nothing, as in the digest's own fold.
		if ok, err := p.Matches(c.Labels); err != nil || !ok {
			continue
		}
		if len(out.Cases) >= investigatordomain.MaxDigestCases {
			out.Unlisted++
			continue
		}
		out.Cases = append(out.Cases, investigatordomain.DigestCase{
			CaseID: c.ID, Alertname: c.Labels["alertname"], Labels: c.Labels, StartedAt: c.StartedAt,
		})
	}
	return out, nil
}

// digestFindings is `notification/service.DigestFindings` over `investigator/service`:
// the Finding a digest sent now may carry for one window.
//
// ⚠️ LATE-BOUND, like `incidentFacts`: notification is built before investigator. An
// unfilled holder answers "no Finding" — the built-in body — rather than an error,
// because before the investigator exists nothing can have summarised anything, and a
// digest must never be held for a summary.
type digestFindings struct {
	investigations *investigatorservice.Service
}

func (r *digestFindings) DigestFinding(
	ctx context.Context, s db.TenantScope, policyID uuid.UUID, start, end time.Time,
) (notifdomain.DigestFinding, bool, error) {
	if r.investigations == nil {
		return notifdomain.DigestFinding{}, false, nil
	}
	w, err := investigatordomain.NewDigestWindow(start, end)
	if err != nil {
		return notifdomain.DigestFinding{}, false, err
	}
	run, ok, err := r.investigations.DigestFinding(ctx, s, policyID, w)
	if err != nil || !ok {
		return notifdomain.DigestFinding{}, false, err
	}
	return notifdomain.DigestFinding{
		InvestigationID: run.ID,
		Investigator:    run.InvestigatorName,
		Version:         run.VersionNumber,
		Summary:         run.Finding,
		Classification:  run.Classification,
		Partial:         run.Partial(),
		ConcludedAt:     run.EndedAt,
	}, true, nil
}

// armDigestInvestigations is `investigations.digest` (git-bug 3e96f5a): the per-tenant
// tick that arms the run for every summarised digest window whose lead has begun. It
// records runs and calls no model, and the digest tick never waits for it.
func (c *Container) armDigestInvestigations(ctx context.Context, job *jobs.Job[jobs.InvestigationsDigestArgs]) error {
	if c.Investigator == nil {
		return jobs.ErrNotImplemented(jobs.KindInvestigationsDigest)
	}
	return c.perTenantSweep(ctx, jobs.KindInvestigationsDigest, job.Args.TenantFanOut,
		func(f jobs.TenantFanOut) db.JobArgs { return jobs.InvestigationsDigestArgs{TenantFanOut: f} },
		func(ctx context.Context, scope db.TenantScope) error {
			_, err := c.Investigator.ArmDigestInvestigations(ctx, scope)
			return err
		})
}
