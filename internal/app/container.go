package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	alertsapi "github.com/thulasiram/oto/internal/alerts/api"
	alertsrepo "github.com/thulasiram/oto/internal/alerts/repository"
	alertsservice "github.com/thulasiram/oto/internal/alerts/service"
	channelsapi "github.com/thulasiram/oto/internal/channels/api"
	channelsregistry "github.com/thulasiram/oto/internal/channels/registry"
	channelsrepo "github.com/thulasiram/oto/internal/channels/repository"
	channelsservice "github.com/thulasiram/oto/internal/channels/service"
	enrichapi "github.com/thulasiram/oto/internal/enrichment/api"
	"github.com/thulasiram/oto/internal/enrichment/enrichers/alerthistory"
	"github.com/thulasiram/oto/internal/enrichment/enrichers/promrule"
	"github.com/thulasiram/oto/internal/enrichment/enrichers/relatedalerts"
	"github.com/thulasiram/oto/internal/enrichment/enrichers/runbook"
	enrichrepo "github.com/thulasiram/oto/internal/enrichment/repository"
	enrichservice "github.com/thulasiram/oto/internal/enrichment/service"
	groupingapi "github.com/thulasiram/oto/internal/grouping/api"
	groupingrepo "github.com/thulasiram/oto/internal/grouping/repository"
	groupingservice "github.com/thulasiram/oto/internal/grouping/service"
	identityapi "github.com/thulasiram/oto/internal/identity/api"
	identityrepo "github.com/thulasiram/oto/internal/identity/repository"
	identityservice "github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/ingestion"
	notifapi "github.com/thulasiram/oto/internal/notification/api"
	notifrepo "github.com/thulasiram/oto/internal/notification/repository"
	notifservice "github.com/thulasiram/oto/internal/notification/service"
	notifworker "github.com/thulasiram/oto/internal/notification/worker"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/config"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/jobs"
	"github.com/thulasiram/oto/internal/platform/secrets"
	"github.com/thulasiram/oto/internal/platform/telemetry"
	rulesapi "github.com/thulasiram/oto/internal/rules/api"
	rulesrepo "github.com/thulasiram/oto/internal/rules/repository"
	rulesservice "github.com/thulasiram/oto/internal/rules/service"
	silencesapi "github.com/thulasiram/oto/internal/silences/api"
	silencesrepo "github.com/thulasiram/oto/internal/silences/repository"
	silencesservice "github.com/thulasiram/oto/internal/silences/service"
	sourcesapi "github.com/thulasiram/oto/internal/sources/api"
	sourcesrepo "github.com/thulasiram/oto/internal/sources/repository"
	sourcesservice "github.com/thulasiram/oto/internal/sources/service"
	statsapi "github.com/thulasiram/oto/internal/stats/api"
	statsrepo "github.com/thulasiram/oto/internal/stats/repository"
	statsservice "github.com/thulasiram/oto/internal/stats/service"
	streamingapi "github.com/thulasiram/oto/internal/streaming/api"
	streamingrepo "github.com/thulasiram/oto/internal/streaming/repository"
	streamingservice "github.com/thulasiram/oto/internal/streaming/service"
)

// Container holds every long-lived dependency, wired by explicit constructors.
// There is no codegen and no runtime DI container: the dependency graph of oto is
// meant to be readable top to bottom in this one file.
//
// ⭐ THE TWO POOLS ARE THE LOAD-BEARING DECISION (SPEC §G.10). They are not an
// optimisation; they are the reason a dashboard query cannot delay a webhook.
// Every construction below states which pool it takes and why:
//
//	Pools.Ingest   25 % of connections · 2 s statement timeout · 500 ms acquire.
//	               ONLY the webhook accept path and the `ingest` queue workers.
//	               `internal/ingestion` takes *db.Pools rather than a loose pool
//	               precisely so that this split cannot be handed over by accident.
//	Pools.General  everything else: the UI, the settings API, the notification
//	               and lifecycle workers, River's own bookkeeping, and the
//	               LISTEN connection. A slow query here can never hold an ingest
//	               connection, because it cannot reach one.
//
// The alerts repositories are built over General as their FALLBACK querier only.
// On the ingest path they are called inside ingestion's transaction, which
// travels in ctx and comes from the ingest pool, so `ObserveBatch` runs entirely
// on Ingest without either module naming the other's pool.
type Container struct {
	Config    config.Config
	Logger    *slog.Logger
	Clock     clock.Clock
	Pools     *db.Pools
	Telemetry *telemetry.Telemetry

	// Keyring is THE credential keyring (SPEC §D.8). One per process: two would
	// mean two key-rotation stories for rows in one table.
	Keyring *secrets.Keyring

	// Jobs is both the db.Enqueuer every service writes through and, when
	// WorkersEnabled, the worker runtime. Registry is what it was built over.
	Jobs     *jobs.Client
	Registry *jobs.Registry
	// WorkersEnabled reports whether this process works jobs as well as
	// enqueueing them.
	WorkersEnabled bool

	// --- services -------------------------------------------------------

	Identity   *identityservice.Service
	Auth       *authn.Middleware
	Sources    *sourcesservice.Service
	Rules      *rulesservice.Service
	Alerts     *alertsservice.Service
	Grouping   *groupingservice.Service
	Enrichment *enrichservice.Service
	Silences   *silencesservice.Service
	Stats      *statsservice.Service
	Ingestion  *ingestion.Module

	Streaming       *streamingservice.Service
	StreamHub       *streamingservice.Hub
	StreamBridge    *streamingservice.Bridge
	ChannelRegistry *channelsregistry.Registry
	ChannelTester   *channelsservice.Tester

	Notify          *notifservice.NotificationService
	Dispatch        *notifservice.DispatchService
	Policies        *notifservice.PolicyService
	Views           *notifservice.ViewService
	Reminders       *notifservice.ReminderService
	NotifyHistory   *notifservice.HistoryService
	NotifyWorkers   *notifworker.Workers
	NotifyScopes    *notifrepo.ScopeResolver
	notifConfigRepo *notifrepo.ConfigRepository

	// --- routers --------------------------------------------------------

	routers routerSet

	// --- shutdown -------------------------------------------------------

	stopBridge context.CancelFunc
	bridgeDone chan struct{}

	// orgs enumerates tenants for the periodic sweeps.
	orgs orgLister
	// enqueuer is the late-bound outbox handed to every service before the job
	// client exists. See lateEnqueuer.
	enqueuer *lateEnqueuer
}

// routerSet is every domain's HTTP surface, held so routes.go can mount them.
type routerSet struct {
	identity  *identityapi.Router
	alerts    *alertsapi.Router
	grouping  *groupingapi.Router
	rules     *rulesapi.Router
	sources   *sourcesapi.Router
	channels  *channelsapi.Router
	notifs    *notifapi.Router
	silences  *silencesapi.Router
	stats     *statsapi.Router
	enrichers *enrichapi.Router
	streaming *streamingapi.Router
	ingestion *ingestion.Module
}

// Options are what a process hands the composition root.
type Options struct {
	Config    config.Config
	Logger    *slog.Logger
	Pools     *db.Pools
	Telemetry *telemetry.Telemetry
	Clock     clock.Clock
	// RunWorkers makes this process WORK jobs, not merely enqueue them. An API
	// pod sets it false: enqueueing is required everywhere, working is not.
	RunWorkers bool
	// HTTPClient is the outbound client handed to the Slack provider.
	HTTPClient *http.Client
}

// New assembles the container.
//
// Ownership of Pools and Telemetry passes to it: Close releases both.
//
// A nil Pools is NOT fatal. `/healthz` must answer while Postgres is down so a
// liveness probe does not kill every pod during a database outage; `/readyz`
// reports the truth. In that state the container carries no services and the
// versioned API is not mounted.
func New(ctx context.Context, o Options) (*Container, error) {
	clk := o.Clock
	if clk == nil {
		clk = clock.New()
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}

	c := &Container{
		Config:         o.Config,
		Logger:         logger,
		Clock:          clk,
		Pools:          o.Pools,
		Telemetry:      o.Telemetry,
		WorkersEnabled: o.RunWorkers && o.Config.Jobs.Enabled,
	}
	if o.Pools == nil {
		return c, nil
	}

	var reg prometheus.Registerer
	if o.Telemetry != nil {
		reg = o.Telemetry.Registry
	}

	general := o.Pools.General
	c.orgs = orgLister{pool: general}
	c.enqueuer = &lateEnqueuer{}

	// ---- the keyring ---------------------------------------------------
	// A deployment with no `security.secret_key` boots WITHOUT a keyring rather
	// than with a fabricated one. Every credential read then fails loudly at the
	// repository, which is the correct blast radius: a channel cannot be
	// configured, and nothing is silently stored in the clear.
	if key := o.Config.Security.SecretKey; key != "" {
		kr, err := secrets.NewKeyringFromBase64(key)
		if err != nil {
			return nil, err
		}
		c.Keyring = kr
	} else {
		logger.Warn("security.secret_key is not set: credential sealing is disabled and channels cannot be configured")
	}

	// ---- identity, and the one authenticator in the process --------------
	tokenRepo := identityrepo.NewAPITokenRepository(general)
	c.Identity = identityservice.New(identityservice.Deps{
		Orgs:       identityrepo.NewOrgRepository(general),
		Users:      identityrepo.NewUserRepository(general),
		Tokens:     tokenRepo,
		Sessions:   identityrepo.NewSessionRepository(general),
		Slack:      identityrepo.NewSlackIdentityRepository(general),
		Clock:      clk,
		Logger:     logger,
		SessionTTL: o.Config.Security.SessionTTL,
	})
	c.Auth = authn.NewMiddleware(c.Identity, o.Config.Security.SessionCookie)
	settings := orgSettings{svc: c.Identity}

	// ---- streaming: the durable log, the hub, the LISTEN/NOTIFY bridge ---
	streamMetrics := streamingservice.NewMetrics(reg)
	c.Streaming = streamingservice.NewService(streamingrepo.NewEventRepository(general), clk, logger)
	c.StreamHub = streamingservice.NewHub(streamingservice.HubConfig{
		Logger:  logger,
		Metrics: hubMetrics(streamMetrics),
	})
	c.StreamBridge = newBridge(o.Pools, c.StreamHub, streamMetrics, clk, logger)
	stream := streamAppender{svc: c.Streaming}

	// ---- channels: the provider registry, the destinations, the secrets ---
	c.ChannelRegistry = channelsregistry.Default(channelsregistry.Config{
		Clock:      clk,
		HTTPClient: o.HTTPClient,
	})
	channelRepo := channelsrepo.NewChannelRepository(general, clk)
	credentialRepo := channelsrepo.NewCredentialRepository(general, keyringSealer(c.Keyring), keyringUnsealer(c.Keyring), clk)
	tester, err := channelsservice.NewTester(channelsservice.TesterOptions{
		Store:    channelRepo,
		Creds:    credentialRepo,
		Registry: c.ChannelRegistry,
		Clock:    clk,
		BaseURL:  o.Config.HTTP.BaseURL,
	})
	if err != nil {
		return nil, err
	}
	c.ChannelTester = tester

	// ---- sources: the upstream registry and the outbound clients ---------
	sourceRepo := sourcesrepo.NewSourceRepository(general, clk)
	clusterRepo := sourcesrepo.NewClusterRepository(general, clk)
	c.Sources, err = sourcesservice.New(sourcesservice.Options{
		Repo:    sourceRepo,
		Creds:   sourcesrepo.NewCredentialStore(general, keyringUnsealer(c.Keyring)),
		Clients: sourcesservice.NewClientFactory(clk),
		Clock:   clk,
		Logger:  logger,
	})
	if err != nil {
		return nil, err
	}

	// ---- rules: content-addressed snapshots, plus the lookup adapter -----
	c.Rules, err = rulesservice.New(rulesservice.Options{
		Repo:   rulesrepo.NewSnapshotRepository(general),
		Lookup: ruleLookup{sources: c.Sources},
		Clock:  clk,
		Logger: logger,
	})
	if err != nil {
		return nil, err
	}

	// ---- alerts: the heart -----------------------------------------------
	//
	// Two of its ports are LATE-BOUND because the service graph has cycles the
	// package graph does not: `grouping` needs the alert timeline, and `alerts`
	// needs a group's state_version. Both holders answer safely until filled.
	alertRepo := alertsrepo.NewAlertRepository(general, clk)
	occurrenceRepo := alertsrepo.NewOccurrenceRepository(general)
	eventRepo := alertsrepo.NewEventRepository(general, clk)
	snoozeRepo := alertsrepo.NewSnoozeRepository(general, clk)
	enrichmentRepo := enrichrepo.NewEnrichmentRepository(general)

	groupVersionsPort := &groupVersions{}
	notificationsPort := &lateNotificationReader{}

	c.Alerts, err = alertsservice.New(alertsservice.Deps{
		Alerts:        alertRepo,
		Occurrences:   occurrenceRepo,
		Events:        eventRepo,
		Snoozes:       snoozeRepo,
		Tx:            alertsrepo.NewTxRunner(general),
		AlertLister:   alertRepo,
		AlertBatch:    alertRepo,
		SnoozeProj:    alertRepo,
		OccBatch:      occurrenceRepo,
		OccSources:    occurrenceRepo,
		EventCounts:   eventRepo,
		SnoozeHistory: snoozeRepo,
		Enqueuer:      c.enqueuer,
		Stream:        stream,
		Health:        sourceHealth{svc: c.Sources},
		Settings:      settings,
		GroupVersions: groupVersionsPort,
		Enrichments:   enrichmentReader{repo: enrichmentRepo},
		Notifications: notificationsPort,
		Clock:         clk,
		Logger:        logger,
	})
	if err != nil {
		return nil, err
	}

	// ---- grouping: durable generations, membership, storm damping --------
	c.Grouping, err = groupingservice.New(groupingservice.Deps{
		Groups:   groupingrepo.NewGroupRepository(general, clk),
		Members:  groupingrepo.NewMemberRepository(general, clk),
		Tx:       groupingrepo.NewTxRunner(general),
		Events:   c.Alerts,
		Timeline: c.Alerts,
		Actions:  c.Alerts,
		Stream:   stream,
		Enqueuer: c.enqueuer,
		Settings: settings,
		Clock:    clk,
		Logger:   logger,
	})
	if err != nil {
		return nil, err
	}
	groupVersionsPort.svc = c.Grouping

	// ---- enrichment: the budgeted, provenanced pipeline ------------------
	alertReadModel := enrichrepo.NewAlertReadModel(general)
	enricherRegistry, err := enrichservice.NewRegistry(
		promrule.New(c.Rules, occurrenceRepo),
		runbook.New(runbook.StaticTemplates{}),
		alerthistory.New(alertReadModel, clk),
		relatedalerts.New(alertReadModel, clk),
	)
	if err != nil {
		return nil, err
	}
	c.Enrichment, err = enrichservice.New(enrichservice.Options{
		Registry: enricherRegistry,
		Repo:     enrichmentRepo,
		Cache:    enrichrepo.NewCacheRepository(general),
		Subjects: subjectLoader{
			alerts:   c.Alerts,
			grouping: c.Grouping,
			sources:  c.Sources,
			occSrc:   &occurrenceSourceReader{resolver: occurrenceRepo},
		},
		Notifier: enrichservice.NewQueueNotifier(c.enqueuer),
		Enqueuer: c.enqueuer,
		Clock:    clk,
		Logger:   logger,
	})
	if err != nil {
		return nil, err
	}

	// ---- notification: policy, intents, fan-out, ordering ----------------
	if err := c.buildNotification(general, reg, clk, logger); err != nil {
		return nil, err
	}
	notificationsPort.inner = notificationReader{svc: c.NotifyHistory}

	// ---- silences and stats ---------------------------------------------
	c.Silences, err = silencesservice.New(silencesservice.Deps{
		Silences: silencesrepo.NewSilenceRepository(general),
		Alerts:   c.Alerts,
		Clock:    clk,
		Logger:   logger,
	})
	if err != nil {
		return nil, err
	}
	c.Stats, err = statsservice.New(statsservice.Deps{
		Repo:  statsrepo.NewStatsRepository(general),
		Clock: clk,
	})
	if err != nil {
		return nil, err
	}

	// ---- ingestion: THE ONLY MODULE ON THE INGEST POOL -------------------
	c.Ingestion, err = ingestion.New(ingestion.Deps{
		Pools:    o.Pools,
		Enqueuer: c.enqueuer,
		Config:   o.Config.Ingest,
		Alerts:   alertObserver{svc: c.Alerts},
		Clock:    clk,
		Logger:   logger,
		Registry: reg,
	})
	if err != nil {
		return nil, err
	}

	// ---- the queue, last, because it needs every handler -----------------
	if err := c.buildJobs(ctx, general, reg, clk, logger); err != nil {
		return nil, err
	}
	c.enqueuer.set(c.Jobs)

	c.buildRouters(channelRepo, credentialRepo, tokenRepo, sourceRepo, clusterRepo, enricherRegistry, clk)
	return c, nil
}

// buildNotification wires the notification module. It is its own method because
// it has eight collaborators and inlining it would bury the shape of the graph.
func (c *Container) buildNotification(
	general *pgxpool.Pool, reg prometheus.Registerer, clk clock.Clock, logger *slog.Logger,
) error {
	policyRepo := notifrepo.NewPolicyRepository(general)
	channelRepo := notifrepo.NewChannelRepository(general)
	notificationRepo := notifrepo.NewNotificationRepository(general)
	deliveryRepo := notifrepo.NewDeliveryRepository(general)
	threadRepo := notifrepo.NewThreadRepository(general)
	eventRepo := notifrepo.NewEventRepository(general, clk)
	reminderRepo := notifrepo.NewReminderRepository(general)
	txRunner := notifrepo.NewTxRunner(general)

	c.NotifyScopes = notifrepo.NewScopeResolver(general)
	c.notifConfigRepo = notifrepo.NewConfigRepository(general, clk)

	var err error
	if c.Policies, err = notifservice.NewPolicyService(policyRepo, channelRepo); err != nil {
		return err
	}

	// The snapshot is read AT CLAIM TIME, always (§C11). A cached one would make
	// a card describe a world the alert has already left.
	snapshots := snapshotSource(notifrepo.NewSnapshotRepository(general, clk))

	if c.Views, err = notifservice.NewViewService(notifservice.ViewConfig{
		Snapshots: snapshots,
		BaseURL:   c.Config.HTTP.BaseURL,
		Clock:     clk,
	}); err != nil {
		return err
	}

	if c.Notify, err = notifservice.NewNotificationService(notifservice.NotificationConfig{
		Tx:            txRunner,
		Policies:      c.Policies,
		Notifications: notificationRepo,
		Deliveries:    deliveryRepo,
		Threads:       threadRepo,
		Snapshots:     snapshots,
		Events:        eventRepo,
		Enqueuer:      c.enqueuer,
		Clock:         clk,
		Logger:        logger,
	}); err != nil {
		return err
	}

	if c.Dispatch, err = notifservice.NewDispatchService(notifservice.DispatchConfig{
		Tx:            txRunner,
		Notifications: notificationRepo,
		Deliveries:    deliveryRepo,
		Threads:       threadRepo,
		Channels:      channelRepo,
		Events:        eventRepo,
		Views:         c.Views,
		Registry:      c.ChannelRegistry,
		Unsealer:      keyringUnsealer(c.Keyring),
		Gates: notifrepo.NewOrderingGates(notifrepo.GatesConfig{
			Pool:       general,
			Clock:      clk,
			Logger:     logger,
			Registerer: reg,
		}),
		Enqueuer: c.enqueuer,
		BaseURL:  c.Config.HTTP.BaseURL,
		Clock:    clk,
		Logger:   logger,
	}); err != nil {
		return err
	}

	if c.Reminders, err = notifservice.NewReminderService(notifservice.ReminderConfig{
		Policies:  policyRepo,
		Reminders: reminderRepo,
		Notifier:  c.Notify,
		Clock:     clk,
		Logger:    logger,
	}); err != nil {
		return err
	}

	if c.NotifyHistory, err = notifservice.NewHistoryService(notificationRepo); err != nil {
		return err
	}

	c.NotifyWorkers, err = notifworker.New(notifworker.Config{
		Scopes:    c.NotifyScopes,
		Notifier:  c.Notify,
		Dispatch:  c.Dispatch,
		Reminders: c.Reminders,
		Logger:    logger,
	})
	return err
}

// buildJobs registers every handler and builds the client.
//
// ⭐ River's own bookkeeping runs on the GENERAL pool, deliberately (§G.10):
// queue traffic is not domain traffic, and giving it the small pool would let
// queue polling starve the very path that pool exists to protect.
func (c *Container) buildJobs(
	ctx context.Context, general *pgxpool.Pool, reg prometheus.Registerer,
	clk clock.Clock, logger *slog.Logger,
) error {
	_ = ctx

	registry := jobs.NewRegistry(&jobs.Runtime{
		Logger:  logger.With(slog.String("component", "jobs")),
		Clock:   clk,
		Metrics: jobs.NewMetrics(reg),
	})
	if err := jobs.RegisterAll(registry, c.handlers()); err != nil {
		return err
	}
	if c.WorkersEnabled {
		jobs.AddDefaultPeriodic(registry, clk)
	}
	c.Registry = registry

	cfg := jobs.FromPlatformConfig(c.Config.Jobs)
	cfg.Pool = general
	cfg.Registry = registry
	cfg.Logger = logger
	cfg.Clock = clk
	if !c.WorkersEnabled {
		// An INSERT-ONLY client: the shape an API pod wants, where enqueueing is
		// required and working jobs is not.
		cfg.Queues = nil
		cfg.QueueDepthInterval = 0
	}

	client, err := jobs.New(cfg)
	if err != nil {
		return err
	}
	c.Jobs = client
	return nil
}

// buildRouters constructs every domain's HTTP surface.
func (c *Container) buildRouters(
	channelRepo *channelsrepo.ChannelRepository,
	credentialRepo *channelsrepo.CredentialRepository,
	tokenRepo *identityrepo.APITokenRepository,
	sourceRepo *sourcesrepo.SourceRepository,
	clusterRepo *sourcesrepo.ClusterRepository,
	enricherRegistry *enrichservice.Registry,
	clk clock.Clock,
) {
	c.routers = routerSet{
		identity: identityapi.NewRouter(c.Identity, c.Auth,
			identityapi.DefaultCookieConfig(c.Config.Security.SessionCookie), clk),
		alerts:   alertsapi.NewRouter(c.Alerts, clk),
		grouping: groupingapi.NewRouter(c.Grouping, c.Alerts, clk),
		rules:    rulesapi.NewRouter(c.Rules, c.Alerts, clk),
		sources: sourcesapi.NewRouter(sourcesapi.Options{
			Sources:  c.Sources,
			Registry: sourceRepo,
			Clusters: clusterRepo,
			Creds:    credentialRepo,
			Tokens:   ingestTokenIssuer{tokens: tokenRepo, clk: clk},
			// ⛔ Reconcile is deliberately NIL. `source.reconcile` (§G.8) has no
			// implementation in this tree — no package produces Observations from
			// the Alertmanager v2 API — and `sources/api` already answers 503 for
			// a missing collaborator. Faking it would mean answering 200 for a
			// pass that never ran, which is the one thing a reconciler must never
			// do: its divergence count is the canary for every correctness bug in
			// the system.
			Reconcile: nil,
			Clock:     clk,
			BaseURL:   c.Config.HTTP.BaseURL,
		}),
		channels: channelsapi.NewRouter(channelsapi.Options{
			Registry: c.ChannelRegistry,
			Channels: channelRepo,
			Creds:    credentialRepo,
			Tester:   c.ChannelTester,
			// ⛔ Interactions is NIL: no package consumes a verified Slack
			// block-action payload yet. The endpoint verifies its HMAC and then
			// answers 503 rather than acknowledging work nobody will do.
			Interactions:  nil,
			SigningSecret: c.Config.Slack.SigningSecret,
			Clock:         clk,
		}),
		notifs: notifapi.NewRouter(notifapi.Options{
			Policies:      c.notifConfigRepo,
			Audit:         c.notifConfigRepo,
			Notifications: notifrepo.NewNotificationRepository(c.Pools.General),
			Deliveries:    notifrepo.NewDeliveryRepository(c.Pools.General),
			Preview:       c.Policies,
			Views:         c.Views,
			Renderers:     c.ChannelRegistry,
			Subjects:      subjectResolver{alerts: c.Alerts, grouping: c.Grouping},
			Enqueuer:      c.enqueuer,
			Clock:         clk,
			BaseURL:       c.Config.HTTP.BaseURL,
		}),
		silences:  silencesapi.NewRouter(c.Silences, c.alertmanagerURL(), clk),
		stats:     statsapi.NewRouter(c.Stats, clk),
		enrichers: enrichapi.NewRouter(enricherRegistry, clk),
		streaming: streamingapi.NewRouter(c.Streaming, c.StreamHub,
			streamingapi.ScopeResolverFunc(func(ctx context.Context) (db.TenantScope, error) {
				_, s, err := authn.Scope(ctx)
				return s, err
			}), clk, c.Logger, streamingservice.NewMetrics(nil)),
		ingestion: c.Ingestion,
	}
}

// alertmanagerURL is the Alertmanager UI root used for the per-silence deep
// link. v1 has no write path into a cluster, so the link is the ONLY silence
// affordance and an empty value renders `null` rather than a guess.
func (c *Container) alertmanagerURL() string { return "" }

// newBridge builds the LISTEN/NOTIFY bridge over the general pool.
//
// The listening connection is acquired from GENERAL and never returned: a
// connection holding LISTEN cannot serve queries, and taking it from Ingest
// would permanently spend one of the connections reserved for the webhook.
func newBridge(
	pools *db.Pools, hub *streamingservice.Hub, m *streamingservice.Metrics,
	clk clock.Clock, logger *slog.Logger,
) *streamingservice.Bridge {
	bridge := &streamingservice.Bridge{}
	bridge = streamingservice.NewBridge(streamingservice.BridgeConfig{
		Repo:      streamingrepo.NewEventRepository(pools.General),
		Publisher: hub,
		Listener: db.NewListener(pools.General, db.EventsChannel, db.ListenerOptions{
			Logger: logger,
			// Every notification issued while the socket was down is gone
			// forever, so a reconnect runs a full catch-up immediately rather
			// than waiting for the poll to notice.
			OnConnect: func(ctx context.Context) { bridge.OnListenerReconnect(ctx) },
		}),
		Clock:   clk,
		Logger:  logger,
		Metrics: bridgeMetrics(m),
	})
	return bridge
}

// Start begins everything with a lifetime: the worker runtime and the streaming
// bridge. It is separate from New so a one-shot command can build the graph
// without starting a single goroutine.
func (c *Container) Start(ctx context.Context) error {
	if c == nil || c.Pools == nil {
		return nil
	}

	if c.StreamBridge != nil {
		bridgeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		c.stopBridge = cancel
		c.bridgeDone = make(chan struct{})
		go func() {
			defer close(c.bridgeDone)
			if err := c.StreamBridge.Run(bridgeCtx); err != nil {
				c.Logger.Error("streaming: bridge stopped", slog.String("error", err.Error()))
			}
		}()
	}

	if c.Jobs != nil && c.WorkersEnabled {
		if err := c.Jobs.Start(ctx); err != nil {
			return err
		}
		c.Logger.Info("worker: queues registered",
			slog.Any("queues", jobs.AllQueues()),
			slog.Int("handlers", len(c.Registry.Specs())))
		for _, spec := range c.Registry.Specs() {
			c.Logger.Info("worker: handler registered",
				slog.String("kind", spec.Kind), slog.String("queue", spec.Queue))
		}
	}
	return nil
}

// Close releases everything the container owns, in reverse construction order.
//
// ⭐ THE ORDER IS THE POINT. The worker pool is DRAINED FIRST, then the bridge,
// then the pools. Closing a pool under a running job turns a graceful shutdown
// into a burst of connection errors and leaves jobs in `running` until the
// rescuer notices — which is minutes of an alert sitting undelivered for no
// reason at all.
func (c *Container) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}

	var firstErr error
	if c.Jobs != nil && c.WorkersEnabled {
		if err := c.Jobs.Stop(ctx); err != nil {
			firstErr = err
		}
	}

	if c.stopBridge != nil {
		c.stopBridge()
		select {
		case <-c.bridgeDone:
		case <-time.After(5 * time.Second):
			c.Logger.Warn("streaming: bridge did not stop in time")
		}
	}
	if c.StreamHub != nil {
		c.StreamHub.Shutdown()
	}

	if c.Telemetry != nil {
		if err := c.Telemetry.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	c.Pools.Close()
	return firstErr
}

// hubMetrics adapts the streaming registry onto the hub's function surface, so
// the hub depends on no metrics library and reaches for no global registry.
func hubMetrics(m *streamingservice.Metrics) streamingservice.HubMetrics {
	if m == nil {
		return streamingservice.HubMetrics{}
	}
	return streamingservice.HubMetrics{
		Connections:     func(d float64) { m.Connections.Add(d) },
		Published:       func(n int) { m.Published.Add(float64(n)) },
		Dropped:         func(n int) { m.Dropped.Add(float64(n)) },
		Resync:          func(reason string) { m.Resyncs.WithLabelValues(reason).Inc() },
		CoalesceSkipped: func(n int) { m.Coalesced.Add(float64(n)) },
	}
}

// bridgeMetrics does the same for the bridge.
func bridgeMetrics(m *streamingservice.Metrics) streamingservice.BridgeMetrics {
	if m == nil {
		return streamingservice.BridgeMetrics{}
	}
	return streamingservice.BridgeMetrics{
		NotifyReceived:  m.NotifyReceived.Inc,
		NotifyMalformed: m.NotifyMalformed.Inc,
		Reconnects:      m.Reconnects.Inc,
		Fetched:         func(n int) { m.Fetched.Add(float64(n)) },
		PollRecovered:   func(n int) { m.PollRecovered.Add(float64(n)) },
		FetchErrors:     m.FetchErrors.Inc,
	}
}
