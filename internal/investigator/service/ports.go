package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// The ports this module declares for itself (ADR 0002, CONTEXT.md §5.4).
// `internal/app` satisfies each one; nothing here names a concrete.

// ProviderStore is where model endpoints are kept, satisfied by
// `investigator/repository.ProviderRepository`.
type ProviderStore interface {
	Insert(ctx context.Context, s db.TenantScope, draft domain.ProviderDraft, credentialID uuid.UUID, at time.Time) (domain.ProviderConfig, error)
	Get(ctx context.Context, s db.TenantScope, providerID uuid.UUID) (domain.ProviderConfig, error)
	List(ctx context.Context, s db.TenantScope) ([]domain.ProviderConfig, error)
}

// CredentialWriter seals a secret into the one sealed-secret store and returns its id.
//
// ⭐ IT IS `channels/repository.CredentialRepository`'S PLAIN-TYPED FACE, the same one
// `sources/service` and `channels/api` declare: `uuid.UUID` and `map[string]string`,
// so one concrete satisfies every consumer with no adapter. It has no read method on
// purpose — the only reader of a model key is KeyResolver, at the moment of dialling.
type CredentialWriter interface {
	CreateCredential(ctx context.Context, s db.TenantScope, kind string, values map[string]string) (uuid.UUID, error)
}

// KeyResolver unseals a model endpoint's key, satisfied by
// `investigator/repository.KeyStore`. The string it returns is a secret.
type KeyResolver interface {
	ResolveKey(ctx context.Context, s db.TenantScope, credentialID uuid.UUID) (string, error)
}

// ModelDialer builds a domain.ModelProvider for one stored endpoint and its unsealed
// key, satisfied by `investigator/models/openaicompat.Dialer`. It is a port so this
// service names no adapter: the one that speaks a wire protocol is chosen in
// `internal/app`, with the guarded HTTP client it must dial through.
type ModelDialer interface {
	Dial(cfg domain.ProviderConfig, apiKey string) (domain.ModelProvider, error)
}

// TxRunner is the unit of work: an endpoint's row and its sealed key commit together.
type TxRunner interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// ---------------------------------------------------------------- Investigations
//
// The ports below carry git-bug 180a525: Investigators, their runs, and the oto
// history a run may read. ⛔ NONE OF THEM REACHES THE NOTIFICATION PATH. There is no
// port here onto `notification` or `channels`, and depguard's
// `investigator-never-reaches-the-notification-path` forbids the import: a Finding
// changes what people READ, never WHETHER they are told (ADR 0053 §2).

// InvestigatorStore is where Investigators and their versions are kept, satisfied by
// `investigator/repository.InvestigatorRepository`.
type InvestigatorStore interface {
	// Create writes an Investigator and its version 1 (the caller's transaction).
	Create(ctx context.Context, s db.TenantScope, d domain.InvestigatorDraft, model domain.ModelIdentity, at time.Time) (domain.Investigator, error)
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Investigator, error)
	// Lock reads an Investigator FOR UPDATE, so two writers cannot both mint version N+1.
	Lock(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Investigator, error)
	List(ctx context.Context, s db.TenantScope) ([]domain.Investigator, error)
	// Versions lists an Investigator's versions, newest first.
	Versions(ctx context.Context, s db.TenantScope, id uuid.UUID) ([]domain.Version, error)
	GetVersion(ctx context.Context, s db.TenantScope, versionID uuid.UUID) (domain.Version, error)
	// Update writes the mutable half: the kill switch, the budgets and the minimum
	// interval.
	Update(ctx context.Context, s db.TenantScope, id uuid.UUID, enabled bool, b domain.Budgets,
		interval time.Duration, incidents bool, at time.Time) error
	// AddVersion writes version `number`.
	AddVersion(ctx context.Context, s db.TenantScope, investigatorID uuid.UUID, number int,
		spec domain.VersionSpec, model domain.ModelIdentity, at time.Time) (domain.Version, error)
}

// InvestigationStore is where runs and their transcripts are kept, satisfied by
// `investigator/repository.InvestigationRepository`.
type InvestigationStore interface {
	// Insert writes a new run, `queued` or — when its switch was off — `skipped`.
	Insert(ctx context.Context, s db.TenantScope, inv domain.Investigation) (domain.Investigation, error)
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Investigation, error)
	// ListBySubject is one subject's runs, latest first.
	ListBySubject(ctx context.Context, s db.TenantScope, kind domain.SubjectKind, subjectID uuid.UUID,
		p db.Keyset) ([]domain.Investigation, db.Cursor, error)
	// Start moves a `queued` run to `running` unless the org already has maxRunning
	// running — then it stays queued and says StartAtCapacity — or it was not queued
	// (another worker took it, or it already ended). It counts and starts under the
	// org's advisory lock, so it is called inside a transaction (ADR 0053 §6).
	Start(ctx context.Context, s db.TenantScope, id uuid.UUID, at time.Time, maxRunning int) (domain.StartOutcome, error)
	// CountRunning counts the org's `running` runs: a peek, not the decision.
	CountRunning(ctx context.Context, s db.TenantScope) (int, error)
	// SpentSince is the input + output tokens of every model turn the org's runs
	// recorded at or after `since`, and of every Remedy risk-model question asked about a
	// Remedy proposed since then (owner ruling 2026-10-05 on git-bug eb4f21b) — the day's
	// spend against its daily budget.
	SpentSince(ctx context.Context, s db.TenantScope, since time.Time) (int64, error)
	// LockSubjectRuns takes the (Investigator, subject) advisory lock and reads what
	// the minimum interval decides on. Inside a transaction.
	LockSubjectRuns(ctx context.Context, s db.TenantScope, investigatorID uuid.UUID, kind domain.SubjectKind,
		subjectID uuid.UUID) (domain.SubjectRuns, error)
	// Finish ends a run that has not ended, with its Finding and the class it was
	// given ("" for none). The row is frozen from then on.
	Finish(ctx context.Context, s db.TenantScope, id uuid.UUID, end domain.Ending, spent domain.Usage,
		toolCalls int, finding, classification string, at time.Time) error
	// SpentOn sums one run's Steps: the tokens of its model turns, and its Tool calls —
	// every call Step except those to the named answer-shaping Tools and those the step
	// budget refused unrun. A run's own counters are written only when it ends, so this
	// is how an ending that its worker never reached records what was spent (review A5).
	SpentOn(ctx context.Context, s db.TenantScope, id uuid.UUID, answerShaping []string) (domain.Usage, int, error)
	// AppendStep writes one transcript entry. There is no method that changes one.
	AppendStep(ctx context.Context, s db.TenantScope, investigationID uuid.UUID, step domain.Step) error
	Steps(ctx context.Context, s db.TenantScope, investigationID uuid.UUID) ([]domain.Step, error)
	// PriorFindings are earlier runs' Findings on the same alert_key, newest first.
	PriorFindings(ctx context.Context, s db.TenantScope, alertKey string, except uuid.UUID, limit int) ([]domain.PriorFinding, error)
	// SubjectFindings are earlier runs' Findings on any of the named subjects of one
	// kind, newest first: an Incident's own, or its member Cases'.
	SubjectFindings(ctx context.Context, s db.TenantScope, kind domain.SubjectKind, subjectIDs []uuid.UUID,
		except uuid.UUID, limit int) ([]domain.PriorFinding, error)
	// DigestRun is the run armed for one digest window — a policy and a window — or nil
	// (git-bug 3e96f5a). There is at most one.
	DigestRun(ctx context.Context, s db.TenantScope, policyID uuid.UUID, window domain.DigestWindow) (*domain.Investigation, error)
}

// ClassStore is where the org's Classification set is kept (ADR 0053 §5, git-bug
// 4298aa0), satisfied by `investigator/repository.ClassRepository`.
//
// ⛔ REPLACING THE SET TOUCHES NO FINDING. A Finding copied the NAME it was given onto
// its own row, so there is nothing here that could rewrite one — and nothing should.
type ClassStore interface {
	// ClassSet reads the set in the operator's order; empty when they wrote none.
	ClassSet(ctx context.Context, s db.TenantScope) (domain.ClassSet, error)
	// ReplaceClassSet writes the whole set, in the caller's transaction.
	ReplaceClassSet(ctx context.Context, s db.TenantScope, set domain.ClassSet, at time.Time) error
}

// ---------------------------------------------------------------- Suggestions
//
// The ports below carry git-bug 8327c00: a Finding's Suggestions, and the two ORDINARY
// edits applying one performs. ⛔ None of them writes during a run: an Investigation reads a
// policy or an Incident to check what it proposes, and never changes either — the write
// methods are called only from ApplySuggestion, on a human's request.

// SuggestionStore is where a Finding's Suggestions are kept, satisfied by
// `investigator/repository.SuggestionRepository`.
type SuggestionStore interface {
	// InsertSuggestions writes one run's Suggestions in the caller's transaction — the one
	// that records its Finding.
	InsertSuggestions(ctx context.Context, s db.TenantScope, investigationID uuid.UUID, drafts []domain.SuggestionDraft,
		at, lapsesAt time.Time) error
	// ListSuggestions reads one Investigation's SHOWN Suggestions: applied, or not lapsed
	// at `now`. A lapsed one is not read.
	ListSuggestions(ctx context.Context, s db.TenantScope, investigationID uuid.UUID, now time.Time) ([]domain.Suggestion, error)
	// LockSuggestion reads one FOR UPDATE, lapsed or not, inside a transaction.
	LockSuggestion(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Suggestion, error)
	// MarkApplied records who applied it and when, once.
	MarkApplied(ctx context.Context, s db.TenantScope, id uuid.UUID, by domain.Requester, at time.Time) error
}

// PolicyEditor reads notification policies as a Suggestion names them, and makes the one
// edit a count-condition Suggestion asks for, satisfied in `internal/app` over
// `notification/service.PolicyWriter`.
//
// ⭐⭐ ApplyCountCondition IS THE ORDINARY EDIT. It goes through the same service method a
// human's `PATCH /notification-policies/{id}` does — the merged policy validated, a deleted
// one refused, the same repository write — so a policy a Suggestion changed is
// indistinguishable from one a human changed by hand, which is ADR 0044 §3's whole test:
// the number is the operator's because a human applied it.
//
// ⛔ `investigator` NEVER IMPORTS `notification` (depguard
// `investigator-never-reaches-the-notification-path`): the policy arrives as
// domain.PolicyTarget, and nothing here can evaluate, send or hold a notification.
type PolicyEditor interface {
	// SuggestionPolicy reads one live policy; a deleted one, or one this org does not have,
	// is KindNotFound.
	SuggestionPolicy(ctx context.Context, s db.TenantScope, policyID uuid.UUID) (domain.PolicyTarget, error)
	// LockSuggestionPolicy is SuggestionPolicy holding the policy's row lock for the caller's
	// transaction (judgment 2, E6): an applied count Suggestion compares and writes under it,
	// so a hand edit cannot commit between the stale check and the edit.
	LockSuggestionPolicy(ctx context.Context, s db.TenantScope, policyID uuid.UUID) (domain.PolicyTarget, error)
	// SuggestionPolicies reads the org's live policies, in evaluation order.
	SuggestionPolicies(ctx context.Context, s db.TenantScope) ([]domain.PolicyTarget, error)
	// ApplyCountCondition sets the policy's count_min and count_window_seconds through the
	// ordinary policy edit, in the caller's transaction.
	ApplyCountCondition(ctx context.Context, s db.TenantScope, policyID uuid.UUID, countMin int, window time.Duration) error
}

// MembershipEditor makes the one edit a membership Suggestion asks for — add, or move —
// satisfied in `internal/app` over `incidents/service`'s own Add and Move, the verbs a human's
// request goes through, with the applier as the actor and the Investigation as provenance on
// the Case's timeline fact. In the caller's transaction.
type MembershipEditor interface {
	ApplySuggestedMembership(ctx context.Context, s db.TenantScope, m domain.AppliedMembership) error
}

// CaseReader reads the Case an Investigation is about, satisfied in `internal/app`
// over `alerts/service`. A Case this org does not have is KindNotFound.
type CaseReader interface {
	InvestigationCase(ctx context.Context, s db.TenantScope, caseID uuid.UUID) (domain.CaseSubject, error)
}

// IncidentReader reads the Incident an Investigation is about, and the one a Case is
// in, satisfied in `internal/app` over `incidents/service` (git-bug 74ea849). An
// Incident this org does not have is KindNotFound.
//
// ⛔ `investigator` NEVER IMPORTS `incidents` (CONTEXT.md §4): the Incident arrives as
// the copy in domain/subject.go, and what an Investigation can see of it is exactly
// what is spelled there.
type IncidentReader interface {
	InvestigationIncident(ctx context.Context, s db.TenantScope, incidentID uuid.UUID) (domain.IncidentSubject, error)
	// InvestigationIncidentNumbered is the same read addressed by the number a human
	// quotes, which is how the API names an Incident.
	InvestigationIncidentNumbered(ctx context.Context, s db.TenantScope, number int64) (domain.IncidentSubject, error)
	// HoldingIncident is the Incident a Case is a CURRENT member of, or uuid.Nil — at
	// most one, because a Case is in at most one (ADR 0052 §4).
	HoldingIncident(ctx context.Context, s db.TenantScope, caseID uuid.UUID) (uuid.UUID, error)
}

// FindingDeclarer declares an Incident's new Finding outbound as the Incident fact
// `finding` (ADR 0052 §5: oto sends "drawn, member added or removed, quiet, active
// again, new Finding"), INSIDE the caller's transaction, so the Finding and its
// declaration are one fact or neither. Satisfied in `internal/app` by enqueueing
// `notify.incident`, keyed on the Investigation as its occasion.
//
// ⭐ IT IS A FACT, NOT A DECISION ABOUT ANY SIGNAL (ADR 0053 §2). A Finding is never an
// input to whether a notification about a Case or an Alert is sent, held or
// suppressed; this declares that the Incident has a new Finding, and whether any
// policy routes that anywhere is the notification layer's question alone. A Case's
// Finding is declared nowhere.
type FindingDeclarer interface {
	DeclareIncidentFinding(ctx context.Context, s db.TenantScope, incidentID, investigationID uuid.UUID) error
}

// DigestReader reads the digest windows an Investigation summarises, satisfied in
// `internal/app` over `notification`'s policies and its digest store (git-bug 3e96f5a).
//
// ⛔ IT READS, AND IT IS THE WHOLE OF WHAT THIS MODULE KNOWS ABOUT A DIGEST. `investigator`
// never imports `notification` (depguard `investigator-never-reaches-the-notification-
// path`): the window arrives as domain.DigestWindow, computed by the policy's own window
// arithmetic, and the Cases as the copies in domain/digest.go. Nothing here can send,
// hold or amend a digest — the digest tick reads a Finding off this module through
// DigestFinding, at the send, and never waits for one.
type DigestReader interface {
	// SummarisedDigests lists the org's live digest policies that name an Investigator,
	// each with its window open at `now`.
	SummarisedDigests(ctx context.Context, s db.TenantScope, now time.Time) ([]domain.SummarisedDigest, error)
	// InvestigationDigest reads one window's Cases as the policy's matchers select them,
	// so far. A policy this org no longer has, or that no longer sends a digest, is
	// KindNotFound.
	InvestigationDigest(ctx context.Context, s db.TenantScope, policyID uuid.UUID, window domain.DigestWindow) (domain.DigestSubject, error)
}

// TimelineReader reads a Case's timeline, oldest first, at most `limit` entries —
// the most recent ones when there are more.
type TimelineReader interface {
	CaseTimeline(ctx context.Context, s db.TenantScope, caseID uuid.UUID, limit int) ([]domain.TimelineEntry, error)
}

// RuleReader reads a rule snapshot, satisfied in `internal/app` over `rules/service`.
type RuleReader interface {
	RuleAtFire(ctx context.Context, s db.TenantScope, snapshotID uuid.UUID) (domain.RuleAtFire, error)
}

// FindingPublisher stores a Finding as the Enrichment `investigator.<name>` on its
// subject — the Case or the Incident — inside the caller's transaction, satisfied in
// `internal/app` over the enrichment store. ⛔ It enqueues nothing: publishing a
// Finding is not a reason to evaluate a notification. (An Incident's Finding is also
// DECLARED, through FindingDeclarer — a separate port, so the publish stays inert.)
type FindingPublisher interface {
	PublishFinding(ctx context.Context, s db.TenantScope, f domain.PublishedFinding) error
}

// OrgControls reads the org's ADR 0053 §6 controls — the kill switch
// (`investigations_enabled`), the daily token budget (`investigation_daily_tokens`)
// and the concurrency (`investigation_concurrency`) — as their effective values,
// satisfied in `internal/app` over `identity/service`.
type OrgControls interface {
	InvestigationControls(ctx context.Context, s db.TenantScope) (domain.OrgControls, error)
}

// JobQueue enqueues `investigations.run` inside the caller's transaction, so a run
// row and the job that runs it commit together. `db.Enqueuer` satisfies it.
type JobQueue interface {
	Enqueue(ctx context.Context, args db.JobArgs, opts ...db.JobOption) (db.EnqueueResult, error)
}

// ---------------------------------------------------------------- ToolServers
//
// The ports below carry git-bug 2e9a086: an operator's MCP servers, the Tools they list,
// and the org's redaction rules a Tool's result passes through. ⛔ None of them holds a
// cluster credential — the one secret is the ToolServer's own access token, sealed like
// a model key (ADR 0016, 0054 §5).

// ToolServerStore is where ToolServers and the Tools they listed are kept, satisfied by
// `investigator/repository.ToolServerRepository`.
type ToolServerStore interface {
	Insert(ctx context.Context, s db.TenantScope, draft domain.ToolServerDraft, credentialID uuid.UUID, at time.Time) (domain.ToolServerConfig, error)
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.ToolServerConfig, error)
	List(ctx context.Context, s db.TenantScope) ([]domain.ToolServerConfig, error)
	// ByNames reads the org's ToolServers with these names; an unknown name is absent.
	ByNames(ctx context.Context, s db.TenantScope, names []string) ([]domain.ToolServerConfig, error)
	// ReplaceTools records a successful discovery (the caller's transaction).
	ReplaceTools(ctx context.Context, s db.TenantScope, id uuid.UUID, tools []domain.DiscoveredTool, at time.Time) error
	// RecordDiscoveryFailure records a failed one, keeping the last good list.
	RecordDiscoveryFailure(ctx context.Context, s db.TenantScope, id uuid.UUID, reason string, at time.Time) error
	// Tools reads what a ToolServer listed at its last successful discovery, by name.
	Tools(ctx context.Context, s db.TenantScope, id uuid.UUID) ([]domain.DiscoveredTool, error)
}

// TokenResolver unseals a ToolServer's access token, satisfied by
// `investigator/repository.KeyStore`. The string it returns is a secret.
type TokenResolver interface {
	ResolveToolServerToken(ctx context.Context, s db.TenantScope, credentialID uuid.UUID) (string, error)
}

// ToolServerDialer opens a session with one ToolServer, satisfied by
// `investigator/toolservers/mcpclient.Dialer`. It is a port so this service names no
// SDK: the adapter is chosen in `internal/app`, with the guarded HTTP client it must
// dial through.
type ToolServerDialer interface {
	Connect(ctx context.Context, cfg domain.ToolServerConfig, token string) (domain.ToolServerClient, error)
}

// RedactionRules hands a run the org's ingest redaction rules as a ResultRedactor,
// satisfied in `internal/app` over `sources/service` and `ingestion/decode`'s matcher —
// the same patterns and the same replacement value ingest applies (git-bug 2e9a086).
type RedactionRules interface {
	ToolResultRedactor(ctx context.Context, s db.TenantScope) (domain.ResultRedactor, error)
}

// RemedyApprovers is the Remedy approval grant (ADR 0054 §4, git-bug 47f67c8), satisfied in
// `internal/app` over `identity/service`, which owns `remedy_approver_grants`.
//
// ⛔⛔ READ-ONLY, AND IT MUST STAY SO. A grant is given and taken by `oto grant` / `oto
// revoke` from the host shell, never from inside oto: a write method here would be one
// handler away from a route that lets one holder mint a second approver, which defeats
// double approval. test/scope/remedy_approver_routes_test.go walks the mounted routes to
// hold that.
type RemedyApprovers interface {
	// RemedyApprovers lists every grant on one ToolServer, disabled holders included.
	RemedyApprovers(ctx context.Context, s db.TenantScope, toolServerID uuid.UUID) ([]domain.RemedyApprover, error)
	// RequireRemedyApprover is nil when the user holds a grant that counts on the
	// ToolServer, and otherwise the typed 403 `remedy_approver_required`. ⭐ The Remedy
	// approval path (git-bug 4148256) calls this once per approval, with the approving
	// human's user id; double approval calls it for each of two DIFFERENT users.
	RequireRemedyApprover(ctx context.Context, s db.TenantScope, toolServerID, userID uuid.UUID) error
}

// ---------------------------------------------------------------- Remedies
//
// The ports below carry git-bug 4148256: a Finding's Remedies, their approvals and their
// transitions, and the declaration of each transition outbound. ⛔ None of them reaches a
// ToolServer's write Tool during a run: the Investigator only NAMES one; executing an approved
// Remedy is a separate job (ADR 0054 §5).

// RemedyStore is where Remedies, their approvals and their transitions are kept, satisfied by
// `investigator/repository.RemedyRepository`.
type RemedyStore interface {
	// InsertRemedy writes one proposed Remedy and its proposal transition, in the caller's
	// transaction — the one that records its Finding.
	InsertRemedy(ctx context.Context, s db.TenantScope, r domain.Remedy, proposal domain.RemedyTransition) error
	// ListRemedies reads one Investigation's Remedies in the order proposed, each with its
	// approvals and transitions.
	ListRemedies(ctx context.Context, s db.TenantScope, investigationID uuid.UUID) ([]domain.Remedy, error)
	// GetRemedy reads one, with its approvals and transitions.
	GetRemedy(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Remedy, error)
	// LockRemedy reads one FOR UPDATE, inside a transaction.
	LockRemedy(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Remedy, error)
	// AddApproval records one human's approval; a second by the same user is refused
	// `remedy_already_approved`.
	AddApproval(ctx context.Context, s db.TenantScope, remedyID uuid.UUID, a domain.RemedyApproval) error
	// Transition moves a Remedy from t.From to t.To and records t, refusing `remedy_moved` when
	// it is no longer in t.From. expiresAt, when set, re-stamps the deadline; result, when set,
	// is what the write Tool answered.
	Transition(ctx context.Context, s db.TenantScope, remedyID uuid.UUID, t domain.RemedyTransition,
		expiresAt time.Time, result string) error
	// PastDeadline lists the Remedies still proposed or approved whose window passed at `now`.
	PastDeadline(ctx context.Context, s db.TenantScope, now time.Time, limit int) ([]uuid.UUID, error)
	// OutcomeOverdue lists the Remedies `executing` since at or before `claimedBy` — claimed,
	// and no answer recorded.
	OutcomeOverdue(ctx context.Context, s db.TenantScope, claimedBy time.Time, limit int) ([]uuid.UUID, error)
}

// RemedyDeclarer declares one Remedy transition outbound as an Incident fact — `remedy_proposed`,
// `remedy_approved`, `remedy_declined`, `remedy_expired`, `remedy_executed`, `remedy_failed` —
// INSIDE the caller's transaction, so the transition and its declaration are one fact or
// neither (ADR 0052 §5, ADR 0054 §2). Satisfied in `internal/app` by enqueueing
// `notify.incident`, keyed on the transition as its occasion and carrying the snapshot.
//
// ⭐ A REMEDY ON A CASE IN NO INCIDENT HAS NO OUTBOUND TARGET, and is declared nowhere: the
// transition row records that (`declared_incident_id` NULL), and nothing invents a target.
type RemedyDeclarer interface {
	DeclareRemedy(ctx context.Context, s db.TenantScope, incidentID uuid.UUID, fact domain.RemedyFact) error
}

// RemedyRiskStore is an org's Remedy risk rules and its risk model (ADR 0054 §3, git-bug
// eb4f21b), satisfied by `investigator/repository.RemedyRiskRepository`.
//
// ⛔ READ-ONLY, ON PURPOSE (owner ruling 2026-10-05). `oto remedy-rules apply` is the only
// writer, in internal/app; a write method here would be one handler away from a route.
type RemedyRiskStore interface {
	// RemedyRisk reads the rules in the operator's order, the risk model, and who last wrote
	// them; no rules and no model for an org that never wrote any.
	RemedyRisk(ctx context.Context, s db.TenantScope) (domain.RemedyRiskSettings, error)
}
