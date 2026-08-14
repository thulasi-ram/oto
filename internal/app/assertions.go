package app

import (
	alertsservice "github.com/thulasiram/oto/internal/alerts/service"
	channelsapi "github.com/thulasiram/oto/internal/channels/api"
	slackprovider "github.com/thulasiram/oto/internal/channels/providers/slack"
	channelsregistry "github.com/thulasiram/oto/internal/channels/registry"
	channelsrepo "github.com/thulasiram/oto/internal/channels/repository"
	channelsservice "github.com/thulasiram/oto/internal/channels/service"
	enrichservice "github.com/thulasiram/oto/internal/enrichment/service"
	enrichworker "github.com/thulasiram/oto/internal/enrichment/worker"
	groupingservice "github.com/thulasiram/oto/internal/grouping/service"
	identityapi "github.com/thulasiram/oto/internal/identity/api"
	identityrepo "github.com/thulasiram/oto/internal/identity/repository"
	identityservice "github.com/thulasiram/oto/internal/identity/service"
	ingestservice "github.com/thulasiram/oto/internal/ingestion/service"
	notifapi "github.com/thulasiram/oto/internal/notification/api"
	notifrepo "github.com/thulasiram/oto/internal/notification/repository"
	notifservice "github.com/thulasiram/oto/internal/notification/service"
	notifworker "github.com/thulasiram/oto/internal/notification/worker"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/idempotency"
	"github.com/thulasiram/oto/internal/platform/jobs"
	"github.com/thulasiram/oto/internal/platform/secrets"
	rulesservice "github.com/thulasiram/oto/internal/rules/service"
	silencesservice "github.com/thulasiram/oto/internal/silences/service"
	sourcesapi "github.com/thulasiram/oto/internal/sources/api"
	sourcesrepo "github.com/thulasiram/oto/internal/sources/repository"
	sourcesservice "github.com/thulasiram/oto/internal/sources/service"
	statsworker "github.com/thulasiram/oto/internal/stats/worker"
)

// ⭐ THE PORT-DRIFT WALL.
//
// Every line here says: THIS concrete satisfies THAT consumer-declared port.
//
// They exist because the composition root is the one place in oto where a
// signature mismatch does NOT produce a compile error on its own — a port is
// satisfied structurally, and a struct field typed as an interface accepts
// whatever happens to fit. Without these, a method renamed on one side of a seam
// surfaces as a nil interface at boot, or as a capability that silently stopped
// being used, and both are discovered in production.
//
// The identity assertions are the ones that module asked for by name. The rest
// follow because the argument is identical for all of them: a drift should break
// the BUILD.
var (
	// --- identity: the five stores, and the resolver every route depends on ---
	_ identityservice.OrgReader          = (*identityrepo.OrgRepository)(nil)
	_ identityservice.UserReader         = (*identityrepo.UserRepository)(nil)
	_ identityservice.TokenStore         = (*identityrepo.APITokenRepository)(nil)
	_ identityservice.SessionStore       = (*identityrepo.SessionRepository)(nil)
	_ identityservice.SlackIdentityStore = (*identityrepo.SlackIdentityRepository)(nil)
	// If this one stops compiling, EVERY authenticated route is affected: the
	// middleware and the thing that mints principals have drifted.
	_ authn.Resolver = (*identityservice.Service)(nil)

	// --- the cross-module ports this package writes adapters for -------------
	_ alertsservice.StreamAppender       = streamAppender{}
	_ groupingservice.StreamAppender     = streamAppender{}
	_ alertsservice.NotificationReader   = (*notificationReader)(nil)
	_ alertsservice.GroupVersionReader   = (*groupVersions)(nil)
	_ alertsservice.EnrichmentReader     = enrichmentReader{}
	_ alertsservice.SourceHealth         = sourceHealth{}
	_ alertsservice.SettingsReader       = orgSettings{}
	_ groupingservice.SettingsReader     = orgSettings{}
	_ notifservice.SettingsReader        = orgSettings{}
	_ rulesservice.RuleLookup            = ruleLookup{}
	_ enrichservice.SubjectLoader        = subjectLoader{}
	_ enrichworker.ScopeResolver         = occurrenceScopes{}
	_ ingestservice.AlertObserver        = alertObserver{}
	_ sourcesservice.IngestTokens        = ingestTokenIssuer{}
	_ notifapi.SubjectResolver           = subjectResolver{}
	_ notifworker.ScopeResolver          = (*notifrepo.ScopeResolver)(nil)
	_ notifservice.CredentialUnsealer    = (*secrets.Keyring)(nil)
	_ channelsservice.CredentialResolver = (*channelsrepo.CredentialRepository)(nil)

	// --- grouping reaches the shared kernel through `alerts/service` ----------
	_ groupingservice.EventAppender  = (*alertsservice.Service)(nil)
	_ groupingservice.TimelineReader = (*alertsservice.Service)(nil)
	_ groupingservice.MemberActions  = (*alertsservice.Service)(nil)

	// --- silences reads alerts through a consumer-declared port --------------
	_ silencesservice.AlertLister = (*alertsservice.Service)(nil)

	// --- the channels registry is three different narrow views ---------------
	_ channelsapi.ProviderRegistry    = (*channelsregistry.Registry)(nil)
	_ channelsservice.Registry        = (*channelsregistry.Registry)(nil)
	_ notifservice.ChannelRegistry    = (*channelsregistry.Registry)(nil)
	_ notifapi.RendererSource         = (*channelsregistry.Registry)(nil)
	_ channelsapi.CredentialWriter    = (*channelsrepo.CredentialRepository)(nil)
	_ sourcesservice.CredentialSealer = (*channelsrepo.CredentialRepository)(nil)
	_ channelsapi.ChannelStore        = (*channelsrepo.ChannelRepository)(nil)
	_ channelsservice.InstanceStore   = (*channelsrepo.ChannelRepository)(nil)

	// --- the Acknowledge button: four seams, all of them load-bearing --------
	//
	// The first is the one that was NIL for the product's whole life. If it stops
	// compiling, every Acknowledge button in every Slack workspace goes back to
	// showing a tick and doing nothing, and nothing else in the build would say so.
	_ channelsapi.SlackInteractions      = (*channelsservice.InteractionService)(nil)
	_ channelsservice.SlackNotice        = (*slackprovider.Notice)(nil)
	_ channelsservice.SlackActors        = slackActors{}
	_ channelsservice.AlertGroups        = slackGroupActions{}
	_ channelsservice.SlackConversations = slackConversations{}

	// --- sources: both halves of the module are the SERVICE ------------------
	//
	// ⭐ THE WRITE SIDE USED TO BE THE REPOSITORY HERE (ticket 0869f21), which is
	// what put the transaction boundary, the three-table ordering and the
	// credential-rotation rule inside an HTTP handler. `sources/service` owns them
	// now, and these two lines are what would break if either half drifted.
	_ sourcesapi.SourceReader         = (*sourcesservice.Service)(nil)
	_ sourcesapi.SourceRegistry       = (*sourcesservice.Service)(nil)
	_ sourcesservice.SourceRepository = (*sourcesrepo.SourceRepository)(nil)
	_ sourcesapi.ClusterRegistry      = (*sourcesrepo.ClusterRepository)(nil)

	// --- the tenant list every per-tenant periodic fans out over -------------
	//
	// ⛔ THE TWO HALVES MUST STAY TOGETHER OR THE SWEEPS GO QUIET. `ScopePage`
	// feeds the fan-out and `LiveScope` is what refuses to build a scope from a
	// job payload alone; a drift on either is a periodic that enqueues nothing, or
	// one that sweeps a tenant the list deliberately skips. Neither shows up as a
	// compile error at the call site, because both travel as interfaces.
	_ jobs.Tenants                = orgLister{}
	_ statsworker.TenantLister    = orgLister{}
	_ sourcesservice.TenantLister = orgLister{}

	// --- notification: the settings half and the evaluation half -------------
	_ notifapi.PolicyStore        = (*notifrepo.ConfigRepository)(nil)
	_ notifapi.AuditStore         = (*notifrepo.ConfigRepository)(nil)
	_ notifapi.NotificationReader = (*notifrepo.NotificationRepository)(nil)
	_ notifapi.DeliveryReader     = (*notifrepo.DeliveryRepository)(nil)
	_ notifapi.Requeuer           = (*lateEnqueuer)(nil)
	_ notifservice.PolicyStore    = (*notifrepo.PolicyRepository)(nil)
	_ notifservice.ChannelStore   = (*notifrepo.ChannelRepository)(nil)
	_ notifservice.EventSink      = (*notifrepo.EventRepository)(nil)
	_ notifservice.ReminderStore  = (*notifrepo.ReminderRepository)(nil)
	_ notifservice.HistoryStore   = (*notifrepo.NotificationRepository)(nil)
	_ notifservice.GateFactory    = (*notifrepo.OrderingGates)(nil)
	_ notifservice.SnapshotSource = (*notifrepo.SnapshotRepository)(nil)

	// --- `Idempotency-Key`: one store, read by two transports ----------------
	//
	// The header was declared on 28 operations and read by NONE of them. These two
	// lines are what make "the credential endpoints honour it" a compile-time fact
	// rather than a wiring the container could quietly drop again.
	_ identityapi.IdempotencyClaims    = (*idempotency.Repository)(nil)
	_ sourcesservice.IdempotencyClaims = (*idempotency.Repository)(nil)
	_ identityapi.UnitOfWork           = (*identityrepo.TxRunner)(nil)
	_ sourcesservice.UnitOfWork        = (*sourcesrepo.TxRunner)(nil)
)
