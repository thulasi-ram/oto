package app

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	alertsapi "github.com/thulasiram/oto/internal/alerts/api"
	alertsrepo "github.com/thulasiram/oto/internal/alerts/repository"
	alertsservice "github.com/thulasiram/oto/internal/alerts/service"
	channelsapi "github.com/thulasiram/oto/internal/channels/api"
	slackprovider "github.com/thulasiram/oto/internal/channels/providers/slack"
	channelsregistry "github.com/thulasiram/oto/internal/channels/registry"
	channelsrepo "github.com/thulasiram/oto/internal/channels/repository"
	channelsservice "github.com/thulasiram/oto/internal/channels/service"
	drillapi "github.com/thulasiram/oto/internal/drill/api"
	drillrepo "github.com/thulasiram/oto/internal/drill/repository"
	drillservice "github.com/thulasiram/oto/internal/drill/service"
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
	identitydomain "github.com/thulasiram/oto/internal/identity/domain"
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
	"github.com/thulasiram/oto/internal/platform/netguard"
	"github.com/thulasiram/oto/internal/platform/ratelimit"
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

// declarativeTuning resolves this process's declarative tuning layer.
//
// It is the ONE place `platform/config`'s TuningEntry and `identity/domain`'s
// DeclaredEntry meet, and that is why it lives in the composition root: the
// loader must not know what a tuning key means, and the domain must not know
// where configuration comes from. The mapping is four lines, and paying four
// lines here is what keeps both of those true.
func declarativeTuning(cfg config.Config) (identitydomain.Declarative, error) {
	entries := cfg.TuningEntries()
	declared := make([]identitydomain.DeclaredEntry, 0, len(entries))
	for _, e := range entries {
		declared = append(declared, identitydomain.DeclaredEntry{
			Key: e.Key, ConfigKey: e.ConfigKey, Value: e.Value,
		})
	}
	return identitydomain.NewDeclarative(declared)
}

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

	// NetGuard is THE SSRF control (§C1/§C3). One per process, installed as the
	// dialer of every outbound HTTP client that talks to a configured URL, so
	// "which addresses may this deployment reach" has exactly one answer.
	NetGuard *netguard.Guard

	// LoginLimiter and LoginGate bound `POST /auth/login`: the first by rate per
	// client address, the second by concurrent argon2id evaluations, which is what
	// actually bounds the 19 MiB-per-verification memory cost.
	LoginLimiter *ratelimit.Limiter
	LoginGate    *ratelimit.Gate

	// Jobs is both the db.Enqueuer every service writes through and, when
	// WorkersEnabled, the worker runtime. Registry is what it was built over.
	Jobs     *jobs.Client
	Registry *jobs.Registry
	// WorkersEnabled reports whether this process works jobs as well as
	// enqueueing them.
	WorkersEnabled bool

	// --- services -------------------------------------------------------

	Identity *identityservice.Service
	Auth     *authn.Middleware
	Sources  *sourcesservice.Service
	// Reconciler is `source.reconcile` (SPEC §G.8, ADR 0006) — MANDATORY. It is a
	// separate field from Sources because it is built LATER: it drives the alerts
	// state machine, and `sources` is constructed before `alerts`.
	Reconciler *sourcesservice.Reconciler
	Rules      *rulesservice.Service
	Alerts     *alertsservice.Service
	Grouping   *groupingservice.Service
	Enrichment *enrichservice.Service
	Silences   *silencesservice.Service
	Stats      *statsservice.Service
	Ingestion  *ingestion.Module
	// Drills runs delivery drills: one synthetic alert pushed through the REAL
	// pipeline. It is built AFTER ingestion because it drives ingestion — through
	// the same `Accept` the webhook handler calls, which is the whole point.
	Drills *drillservice.Service

	Streaming       *streamingservice.Service
	StreamHub       *streamingservice.Hub
	StreamBridge    *streamingservice.Bridge
	ChannelRegistry *channelsregistry.Registry
	ChannelTester   *channelsservice.Tester
	// SlackInteractions consumes verified Slack block actions (§H.8). It is
	// reachable from TWO places on purpose — the HTTP endpoint enqueues through
	// it, the `slack.interaction` worker applies through it — which is what keeps
	// the three-second rule and the work on opposite sides of one type.
	SlackInteractions *channelsservice.InteractionService

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
	// streamMetrics is the ONE registered streaming collector set. It is held on
	// the container because the hub, the bridge and the SSE handler are built in
	// three different places and must all increment the SAME collectors —
	// building a second set is not a duplicate metric, it is a metric nothing
	// scrapes.
	streamMetrics *streamingservice.Metrics
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
	drills    *drillapi.Router
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

	// ---- the login limits ------------------------------------------------
	//
	// ⛔ THEY ARE A DENIAL-OF-SERVICE CONTROL, not only an authentication one.
	// `identity/service.Login` runs argon2id at 19 MiB on EVERY path — including
	// the DummyVerify that makes an unknown address cost the same as a known one —
	// so an unauthenticated caller allocates 19 MiB per in-flight request. The
	// limiter bounds the rate per client address; the gate bounds how many of
	// those evaluations are resident at once, which is the number that decides
	// whether this endpoint can exhaust the pod.
	c.LoginLimiter = ratelimit.New(ratelimit.Config{
		Burst:  o.Config.Security.LoginRateBurst,
		Refill: o.Config.Security.LoginRateRefill,
		Clock:  clk,
	})
	c.LoginGate = ratelimit.NewGate(o.Config.Security.LoginMaxConcurrent)

	// ---- identity, and the one authenticator in the process --------------
	//
	// ⭐ THE DECLARATIVE TUNING LAYER IS RESOLVED HERE AND FAILS THE BOOT. An
	// unknown key, an unparseable value or one outside its bound stops the process
	// with the config key named, rather than starting a pod whose values file
	// contains a line that silently does nothing (identity/domain.Declarative).
	declarative, err := declarativeTuning(o.Config)
	if err != nil {
		return nil, err
	}
	tokenRepo := identityrepo.NewAPITokenRepository(general)
	c.Identity = identityservice.New(identityservice.Deps{
		Orgs:        identityrepo.NewOrgRepository(general, clk),
		Users:       identityrepo.NewUserRepository(general),
		Tokens:      tokenRepo,
		Sessions:    identityrepo.NewSessionRepository(general),
		Slack:       identityrepo.NewSlackIdentityRepository(general),
		Clock:       clk,
		Logger:      logger,
		SessionTTL:  o.Config.Security.SessionTTL,
		Declarative: declarative,
	})
	if !declarative.Empty() {
		keys := declarative.Keys()
		names := make([]string, 0, len(keys))
		for _, k := range keys {
			names = append(names, string(k)+"="+declarative.ConfigKey(k))
		}
		logger.Info("identity: tuning keys are set by this deployment's configuration and cannot be changed over the API",
			"keys", strings.Join(names, " "))
	}
	c.Auth = authn.NewMiddleware(c.Identity, o.Config.Security.SessionCookie)
	settings := orgSettings{svc: c.Identity}

	// ---- streaming: the durable log, the hub, the LISTEN/NOTIFY bridge ---
	streamMetrics := streamingservice.NewMetrics(reg)
	c.streamMetrics = streamMetrics
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
		// The webhook provider builds a `netguard` guard from this SAME
		// deployment-level switch and installs it as the DIALER on every channel's
		// transport, exactly as the Alertmanager and Prometheus clients do. One
		// decision for the operator, one SSRF control for the process.
		AllowPrivateWebhookTargets: o.Config.Security.AllowPrivateTargets,
		// And the webhook provider gates `config.insecure_skip_verify` on the SAME
		// switch that gates `alert_sources.tls_skip_verify`. Both are "which
		// certificates does this deployment trust", and both were tenant-writable.
		AllowInsecureWebhookTLS: o.Config.Security.AllowInsecureTLS,
	})
	channelRepo := channelsrepo.NewChannelRepository(general, clk)
	credentialRepo := channelsrepo.NewCredentialRepository(general, keyringSealer(c.Keyring), channelsUnsealer(c.Keyring), clk)
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

	// ---- the SSRF guard --------------------------------------------------
	//
	// ⭐ ONE GUARD FOR THE PROCESS, INSTALLED AS A DIALER. Every URL oto dials
	// outbound is operator- or tenant-supplied, and oto dials it from inside the
	// operator's network. The guard is built here so that "which addresses may
	// this deployment reach" is answered in exactly one place, and it is handed to
	// the client factory as `Dial` rather than run as a pre-flight check because
	// only the dialer sees the address the socket actually connects to.
	//
	// `AllowPrivateTargets` is DEPLOYMENT-LEVEL and default closed. It is read from
	// config and never from a row: a per-tenant version of this switch would be a
	// per-tenant grant to read the host's metadata service.
	c.NetGuard = netguard.New(netguard.Options{
		AllowPrivate: o.Config.Security.AllowPrivateTargets,
		Code:         "source_target_not_permitted",
		Field:        "base_url",
	})
	if c.NetGuard.AllowsPrivate() {
		logger.Warn("security.allow_private_targets is ON: oto will dial private, loopback and link-local addresses on behalf of any tenant")
	}

	// ---- sources: the upstream registry and the outbound clients ---------
	sourceRepo := sourcesrepo.NewSourceRepository(general, clk)
	clusterRepo := sourcesrepo.NewClusterRepository(general, clk)
	sourceTx := sourcesrepo.NewTxRunner(general)
	clientFactory := sourcesservice.NewClientFactory(clk)
	clientFactory.Dial = c.NetGuard.DialContext
	clientFactory.AllowInsecureTLS = o.Config.Security.AllowInsecureTLS
	c.Sources, err = sourcesservice.New(sourcesservice.Options{
		Repo:    sourceRepo,
		Creds:   sourcesrepo.NewCredentialStore(general, sourcesUnsealer(c.Keyring)),
		Clients: clientFactory,
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
	enrichmentRepo := enrichrepo.NewEnrichmentRepository(general).WithLogger(logger)

	groupVersionsPort := &groupVersions{}
	notificationsPort := &notificationReader{}

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
	notificationsPort.svc = c.NotifyHistory

	// ---- silences and stats ---------------------------------------------
	//
	// ⛔ `Mirror` IS NOT A WRITE PATH INTO YOUR CLUSTER (R3). It is the write half
	// of oto's own `silences` table, reachable from the `silences.sync` job and
	// from no HTTP route. `Sources` is the read port it copies from.
	silenceRepo := silencesrepo.NewSilenceRepository(general)
	c.Silences, err = silencesservice.New(silencesservice.Deps{
		Silences: silenceRepo,
		Alerts:   c.Alerts,
		Sources:  silenceSource{svc: c.Sources},
		Mirror:   silenceRepo,
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
	//
	// The orchestrator is hoisted into a variable because it has TWO producers:
	// the webhook path below and the reconciler after it. That is the point of
	// C18 — one write path into `alerts`, two things that feed it.
	observer := alertObserver{svc: c.Alerts, grouping: c.Grouping, log: logger}
	c.Ingestion, err = ingestion.New(ingestion.Deps{
		Pools:    o.Pools,
		Enqueuer: c.enqueuer,
		Config:   o.Config.Ingest,
		Alerts:   observer,
		Clock:    clk,
		Logger:   logger,
		Registry: reg,
	})
	if err != nil {
		return nil, err
	}

	// ---- the reconciler: MANDATORY (ADR 0006) ----------------------------
	//
	// ⭐ It is built HERE and not beside `c.Sources`, because it drives the alerts
	// state machine and `sources` is constructed before `alerts`. Alertmanager's
	// MuteStage drops suppressed alerts before any webhook fires, so this is the
	// ONLY thing in oto that can ever observe `suppressed` — a nil here is not a
	// missing feature, it is an unreachable alert state.
	//
	// It takes the SAME `observer` the webhook path takes, deliberately: a
	// reconciler-recovered alert joins a group generation and earns a notification
	// exactly as a pushed one does.
	c.Reconciler, err = sourcesservice.NewReconciler(sourcesservice.ReconcilerOptions{
		Sources:  c.Sources,
		Alerts:   observer,
		Read:     c.Alerts,
		Clusters: clusterRepo,
		Orgs:     c.orgs,
		Enqueuer: c.enqueuer,
		Clock:    clk,
		Logger:   logger,
	})
	if err != nil {
		return nil, err
	}

	// ---- inbound Slack: the Acknowledge button (§H.8) --------------------
	//
	// ⭐ IT IS BUILT BEFORE THE QUEUE because the queue registers its handler, and
	// AFTER grouping and identity because it calls both. Nothing about it is
	// optional in a deployment that renders the button: a card carrying an
	// Acknowledge nobody consumes is worse than a card with no button at all.
	c.SlackInteractions, err = channelsservice.NewInteractionService(channelsservice.InteractionOptions{
		Conversations: slackConversations{channels: channelRepo},
		Actors:        slackActors{identity: c.Identity},
		Groups:        slackGroupActions{grouping: c.Grouping},
		Enqueuer:      c.enqueuer,
		// The ephemeral reply goes to Slack's own `response_url`, which needs no
		// token and no scope — which is why oto can tell a user "that already
		// resolved" without asking the operator for anything the manifest does
		// not already request.
		Notice: slackprovider.NewNotice(o.HTTPClient),
		// ⭐ `oto_slack_unknown_action_total` (§H.8). An unroutable action id is
		// answered 200 like every other interaction — Slack disables an app's
		// subscriptions when deliveries fail — so this counter is the ONLY
		// evidence that a human pressed a button oto could not route. Left
		// unregistered it would be the same silent success this endpoint was
		// fixed for, one layer down.
		Metrics: channelsservice.NewInteractionMetrics(reg),
		Clock:   clk,
		Logger:  logger,
	})
	if err != nil {
		return nil, err
	}

	// ---- delivery drills -------------------------------------------------
	//
	// ⭐ AFTER INGESTION, AND THAT ORDER IS THE FEATURE. A drill drives the real
	// pipeline by handing a manufactured payload to the SAME
	// `ingestion/service.Service` the webhook handler uses; there is no second
	// entry point, and if this were ever wired to anything else a passing drill
	// would stop being evidence.
	c.Drills, err = drillservice.New(drillservice.Options{
		Store:   drillrepo.NewDrillRepository(general),
		Ingest:  drillIngest{svc: c.Ingestion.Service},
		Sources: drillSources{sources: c.Sources, clusters: clusterRepo},
		Clock:   clk,
		Logger:  logger,
	})
	if err != nil {
		return nil, err
	}

	// ---- the queue, last, because it needs every handler -----------------
	if err := c.buildJobs(ctx, general, reg, clk, logger); err != nil {
		return nil, err
	}
	c.enqueuer.set(c.Jobs)

	c.buildRouters(channelRepo, credentialRepo, tokenRepo, sourceRepo, clusterRepo, sourceTx, enricherRegistry, clk)
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

	// The same `orgs.settings` adapter the alerts and grouping modules read their
	// tuning through. One adapter means one answer: the storm threshold that
	// collapses a group and the broadcast policy that decides whether the collapse
	// is announced cannot come from two different reads of the same row.
	settings := orgSettings{svc: c.Identity}

	var err error
	if c.Policies, err = notifservice.NewPolicyService(policyRepo, channelRepo); err != nil {
		return err
	}

	// The snapshot is read AT CLAIM TIME, always (§C11). A cached one would make
	// a card describe a world the alert has already left.
	//
	// This is the repository itself, not a port wrapper: `notifservice.SnapshotSource`
	// has exactly one implementation and `assertions.go` pins it. Should
	// `alerts/service` ever publish an equivalent, the swap is this one constructor.
	snapshots := notifrepo.NewSnapshotRepository(general, clk)

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
		Channels:      channelRepo,
		// ADR 0020's broadcast policy and the org's fallback verbosity, read from
		// `orgs.settings` on every evaluation — the same adapter the lifecycle and
		// storm ports use.
		Settings: settings,
		Clock:    clk,
		Logger:   logger,
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
		Unsealer:      dispatchUnsealer(c.Keyring),
		Gates: notifrepo.NewOrderingGates(notifrepo.GatesConfig{
			Pool:       general,
			Clock:      clk,
			Logger:     logger,
			Registerer: reg,
		}),
		Enqueuer: c.enqueuer,
		// The unacked reminder's mention audience, read from `orgs.settings` at
		// claim time (ADR 0020). It is the ONLY setting the dispatch path reads.
		Settings: settings,
		BaseURL:  c.Config.HTTP.BaseURL,
		Clock:    clk,
		Logger:   logger,
		// ⭐ `oto_delivery_claim_lost_total` is an ALERT, not a statistic: every
		// increment is a message that exists in somebody's channel with no `sent`
		// row behind it. Left unregistered it was a counter nothing could scrape,
		// which is the same as not having it.
		Metrics: notifservice.NewMetrics(reg),
	}); err != nil {
		return err
	}

	if c.Reminders, err = notifservice.NewReminderService(notifservice.ReminderConfig{
		Policies:  policyRepo,
		Reminders: reminderRepo,
		Notifier:  c.Notify,
		// The org's fallback reminder delay, for a policy that names none.
		Settings: settings,
		Clock:    clk,
		Logger:   logger,
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
		// The per-source schedule cannot live in `platform/jobs`: its payload names
		// a source, so the fan-out needs the source list. See addSourcePeriodic.
		c.addSourcePeriodic(registry)
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
	sourceTx *sourcesrepo.TxRunner,
	enricherRegistry *enrichservice.Registry,
	clk clock.Clock,
) {
	c.routers = routerSet{
		identity: identityapi.NewRouter(identityapi.Options{
			Service: c.Identity,
			Auth:    c.Auth,
			Cookie:  identityapi.DefaultCookieConfig(c.Config.Security.SessionCookie),
			Limiter: c.LoginLimiter,
			Gate:    c.LoginGate,
			Clock:   clk,
		}),
		alerts: alertsapi.NewRouter(c.Alerts, clk),
		// The third argument is `delivery_summary` on the group card: was anybody
		// told about this generation, and did it land. It is late enough in the
		// build that `c.NotifyHistory` is real; when the notification module is
		// absent the adapter answers all-zero rather than nil.
		grouping: groupingapi.NewRouter(c.Grouping, c.Alerts,
			groupDeliveryRollups{svc: c.NotifyHistory}, clk),
		rules: rulesapi.NewRouter(c.Rules, c.Alerts, clk),
		sources: sourcesapi.NewRouter(sourcesapi.Options{
			Sources:  c.Sources,
			Registry: sourceRepo,
			Clusters: clusterRepo,
			Creds:    credentialRepo,
			Tokens:   ingestTokenIssuer{tokens: tokenRepo, tx: sourceTx, clk: clk},
			// `POST /sources/{id}/reconcile` forces one pass now (§G.8). It answers
			// 200 with `ok:false` for an upstream that is down, because "the source
			// is unreachable" is a RESULT the operator asked for — and because the
			// same pass has already recorded the failure in `source_health`, where
			// three of them block the reaper (§B.4).
			Reconcile: c.Reconciler,
			// `GET /sources/{id}/rejections` and `/failed-batches`: the read half
			// of ingestion, which was written from the first migration and could
			// not be read back from anywhere but `psql`. It is the SAME
			// `ingestion/service.Service` the webhook handler writes through.
			Feeds: ingestFeeds{svc: c.Ingestion.Service},
			// One transaction for the source row and its ingest credential. They
			// used to be independent commits, and a source without its token can
			// never receive a webhook.
			Tx: sourceTx,
			// Configuration-time SSRF feedback. The DIALER is the control; this is
			// so an operator who pastes a metadata-service URL sees a 422 naming the
			// field rather than a probe that mysteriously returns someone else's data.
			Guard:            c.NetGuard,
			AllowInsecureTLS: c.Config.Security.AllowInsecureTLS,
			Clock:            clk,
			BaseURL:          c.Config.HTTP.BaseURL,
		}),
		channels: channelsapi.NewRouter(channelsapi.Options{
			Registry: c.ChannelRegistry,
			Channels: channelRepo,
			Creds:    credentialRepo,
			Tester:   c.ChannelTester,
			// ⭐ THE ACKNOWLEDGE BUTTON, WIRED. It was `nil` here for the whole of
			// the product's life, and the comment that stood in its place claimed
			// the endpoint "answers 503"; it did not — it verified the HMAC and
			// answered 200 with an empty body, so every press showed the user a
			// tick and did nothing at all. The only action oto asks of a human was
			// a no-op that looked like it worked, which is worse than no button:
			// it teaches people that acknowledging is pointless.
			//
			// The consumer enqueues and returns. Everything an acknowledgement
			// actually costs happens on `slack.interaction`, off the request.
			Interactions:  c.SlackInteractions,
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
		silences:  silencesapi.NewRouter(c.Silences, silenceBaseURLs{svc: c.Sources}, clk),
		stats:     statsapi.NewRouter(c.Stats, clk),
		drills:    drillRouter(c.Drills, clk),
		enrichers: enrichapi.NewRouter(enricherRegistry, clk),
		streaming: streamingapi.NewRouter(c.Streaming, c.StreamHub,
			streamingapi.ScopeResolverFunc(func(ctx context.Context) (db.TenantScope, error) {
				_, s, err := authn.Scope(ctx)
				return s, err
				// ⭐ THE REGISTERED SET, not a fresh one. The SSE handler is the
				// only place `oto_stream_resync_total{reason=...}` is incremented for
				// a client-driven resync; handed its own unregistered collectors,
				// every one of those increments was invisible to /metrics.
			}), clk, c.Logger, c.streamMetrics),
		ingestion: c.Ingestion,
	}
}

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

// drillRouter builds the delivery-drill surface, or nothing when drills are not
// wired. A nil router is skipped by mountDomains, so a deployment without them
// answers 404 rather than panicking on the first request.
func drillRouter(svc *drillservice.Service, clk clock.Clock) *drillapi.Router {
	if svc == nil {
		return nil
	}
	return drillapi.NewRouter(svc, clk)
}
