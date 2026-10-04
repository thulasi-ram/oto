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
	// Update writes the mutable half: the kill switch and the budgets.
	Update(ctx context.Context, s db.TenantScope, id uuid.UUID, enabled bool, b domain.Budgets, at time.Time) error
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
	// Start moves a `queued` run to `running`, and reports false when it was not
	// queued — another worker took it, or it already ended.
	Start(ctx context.Context, s db.TenantScope, id uuid.UUID, at time.Time) (bool, error)
	// Finish ends a run that has not ended. The row is frozen from then on.
	Finish(ctx context.Context, s db.TenantScope, id uuid.UUID, end domain.Ending, spent domain.Usage,
		toolCalls int, finding string, at time.Time) error
	// AppendStep writes one transcript entry. There is no method that changes one.
	AppendStep(ctx context.Context, s db.TenantScope, investigationID uuid.UUID, step domain.Step) error
	Steps(ctx context.Context, s db.TenantScope, investigationID uuid.UUID) ([]domain.Step, error)
	// PriorFindings are earlier runs' Findings on the same alert_key, newest first.
	PriorFindings(ctx context.Context, s db.TenantScope, alertKey string, except uuid.UUID, limit int) ([]domain.PriorFinding, error)
}

// CaseReader reads the Case an Investigation is about, satisfied in `internal/app`
// over `alerts/service`. A Case this org does not have is KindNotFound.
type CaseReader interface {
	InvestigationCase(ctx context.Context, s db.TenantScope, caseID uuid.UUID) (domain.CaseSubject, error)
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
// Case, inside the caller's transaction, satisfied in `internal/app` over the
// enrichment store. ⛔ It enqueues nothing: publishing a Finding is not a reason to
// evaluate a notification.
type FindingPublisher interface {
	PublishFinding(ctx context.Context, s db.TenantScope, f domain.PublishedFinding) error
}

// OrgSwitch reads the org's Investigation kill switch (`investigations_enabled`,
// ADR 0053 §6), satisfied in `internal/app` over `identity/service`.
type OrgSwitch interface {
	InvestigationsEnabled(ctx context.Context, s db.TenantScope) (bool, error)
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
