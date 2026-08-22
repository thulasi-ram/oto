package app

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	alertsapi "github.com/thulasiram/oto/internal/alerts/api"
	alertsdomain "github.com/thulasiram/oto/internal/alerts/domain"
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
	identityapi "github.com/thulasiram/oto/internal/identity/api"
	identitydomain "github.com/thulasiram/oto/internal/identity/domain"
	identityrepo "github.com/thulasiram/oto/internal/identity/repository"
	identityservice "github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/ingestion"
	ingestionservice "github.com/thulasiram/oto/internal/ingestion/service"
	notifapi "github.com/thulasiram/oto/internal/notification/api"
	notifrepo "github.com/thulasiram/oto/internal/notification/repository"
	notifservice "github.com/thulasiram/oto/internal/notification/service"
	notifworker "github.com/thulasiram/oto/internal/notification/worker"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/config"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/idempotency"
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

	// Idempotency claims a client-supplied `Idempotency-Key` so a retried mutation
	// cannot act twice (SPEC §E.1). It is a PLATFORM store rather than one
	// domain's, because the header is a transport mechanism 28 operations across
	// nine modules declare — `identity` and `sources` are merely the first two
	// callers, being the two that mint credentials. It is held on the container so
	// `retention.prune` can sweep it beside the other unpartitioned siblings.
	Idempotency *idempotency.Repository

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
	ChannelWrites   *channelsservice.Writer
	// SlackInteractions consumes verified Slack block actions (§H.8). It is
	// reachable from TWO places on purpose — the HTTP endpoint enqueues through
	// it, the `slack.interaction` worker applies through it — which is what keeps
	// the three-second rule and the work on opposite sides of one type.
	SlackInteractions *channelsservice.InteractionService

	PolicyWrites    *notifservice.PolicyWriter
	Notify          *notifservice.NotificationService
	Dispatch        *notifservice.DispatchService
	Policies        *notifservice.PolicyService
	Views           *notifservice.ViewService
	Digests         *notifservice.DigestService
	NotifyHistory   *notifservice.HistoryService
	NotifyWorkers   *notifworker.Workers
	NotifyScopes    *notifrepo.ScopeResolver
	notifConfigRepo *notifrepo.ConfigRepository
	// templates is held on the container because it is read at BOTH ends of the
	// feature — the authoring API and the delivery-time resolver — and those are
	// wired by two different methods.
	templates *channelsrepo.TemplateRepository

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

	// ---- capability detection: pg_trgm ------------------------------------
	//
	// ONE process-lifetime check, cached as a bool and threaded into every
	// consumer below. It is computed here, before either consumer exists,
	// specifically so that `identity` and `alerts` never need to know about
	// each other to agree on the answer — see db.TrigramAvailable's doc
	// comment for why oto never enables the extension itself.
	//
	// A read error here is NOT fatal: the capability is optional by design, and
	// a transient failure to read `pg_extension` should degrade to "not
	// available" rather than take the whole process down over a feature every
	// alert search can run without.
	trigramAvailable, err := db.TrigramAvailable(ctx, general)
	if err != nil {
		logger.Warn("db: could not determine pg_trgm availability; alert search will not offer partial alertname matching",
			"error", err.Error())
		trigramAvailable = false
	}

	// ---- the idempotency claim store -------------------------------------
	//
	// Built before every domain, because it belongs to none of them: it is the
	// store behind the `Idempotency-Key` header, which is a property of how a
	// request is RETRIED and not a fact about any entity. Its rows are swept by
	// `retention.prune`.
	c.Idempotency = idempotency.NewRepository(general)

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
	// ⭐ `createApiToken` HAD NO TRANSACTION AT ALL. It minted a credential and
	// returned its plaintext in one unguarded commit, so there was nowhere for the
	// idempotency claim that guards it to join. The two are one unit of work now.
	identityTx := identityrepo.NewTxRunner(general)
	c.Identity = identityservice.New(identityservice.Deps{
		Orgs:     identityrepo.NewOrgRepository(general, clk),
		Users:    identityrepo.NewUserRepository(general),
		Tokens:   tokenRepo,
		Sessions: identityrepo.NewSessionRepository(general),
		Slack:    identityrepo.NewSlackIdentityRepository(general),
		// The same runner the identity API uses: it is what makes the ingest-token
		// rotation's mint and revocation ONE commit (IssueIngestToken).
		Tx:          identityTx,
		Clock:       clk,
		Logger:      logger,
		SessionTTL:  o.Config.Security.SessionTTL,
		Declarative: declarative,
		// Same process-lifetime bool `alerts` gets below. identity surfaces it on
		// `GET /api/v1/me` (§C, MeDTO.Search) so the UI can offer alertname
		// substring search precisely when it will actually match — without
		// `internal/identity` importing `internal/alerts` to find out (ADR 0002).
		TrigramAvailable: trigramAvailable,
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
	connectionRepo := channelsrepo.NewConnectionRepository(general, clk)
	// ⭐ ONE WORDING REPOSITORY FOR TWO READERS, and they read it at opposite ends
	// of the product. `buildRouters` gives it to the authoring API; the
	// notification service resolves against it at claim time. It was constructed
	// inline at the second site alone while the first did not exist; two
	// constructions would be two connection-pool users where one will do, and — the
	// part that actually bites — two places to change when the constructor gains an
	// argument.
	templateRepo := channelsrepo.NewTemplateRepository(general, clk)
	c.templates = templateRepo
	credentialRepo := channelsrepo.NewCredentialRepository(general, keyringSealer(c.Keyring), channelsUnsealer(c.Keyring), clk)
	tester, err := channelsservice.NewTester(channelsservice.TesterOptions{
		Store:       channelRepo,
		Connections: connectionRepo,
		Creds:       credentialRepo,
		Registry:    c.ChannelRegistry,
		Clock:       clk,
		BaseURL:     o.Config.HTTP.BaseURL,
	})
	if err != nil {
		return nil, err
	}

	// ⭐ THE SETTINGS-TIME SLACK NAME↔ID LOOKUP. Unlike the tester, this never
	// mints a delivery Channel — it only unseals a connection's credential long
	// enough to ask the provider's optional ConversationResolver capability.
	channelResolver, err := channelsservice.NewResolver(channelsservice.ResolverOptions{
		Store:    connectionRepo,
		Creds:    credentialRepo,
		Registry: c.ChannelRegistry,
	})
	if err != nil {
		return nil, err
	}

	// `createChannel` and `testChannel` move behind the service so an
	// `Idempotency-Key` claim can join the transaction of the act it guards. The
	// tester is handed over rather than kept beside it: a retried `testChannel`
	// that took its claim outside the send would still make the second real send
	// the claim exists to prevent.
	c.ChannelWrites, err = channelsservice.NewWriter(channelsservice.WriterOptions{
		Store:  channelRepo,
		Tester: tester,
		Tx:     channelsrepo.NewTxRunner(general),
		Claims: c.Idempotency,
		Clock:  clk,
	})
	if err != nil {
		return nil, err
	}

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
		// ---- the write half (ticket 0869f21) ------------------------------
		//
		// These four used to be injected into `sources/api`, which meant the
		// transaction boundary, the ordering of writes across three tables and the
		// `Idempotency-Key` claim were reachable only through an HTTP request. They
		// belong to the service that performs the operation.
		//
		// Sealer is the CHANNELS credential repository: one sealed-secret store
		// serves both modules, and `sources` reaches it through a plain-typed port
		// rather than an import.
		Sealer: credentialRepo,
		// Tokens is the identity service itself: the ingest mint is credential
		// lifecycle and lives beside the PAT mint it mirrors
		// (identity/service.IssueIngestToken). Its mint-before-revoke transaction
		// is identity's own runner, and it JOINS the unit of work this service
		// opens around a create or a rotation — the transaction travels in the
		// context, so the source row, its credential and its token still commit
		// together.
		Tokens: c.Identity,
		// One transaction for the source row, its credential and its ingest token.
		// They used to be independent commits, and a source without its token can
		// never receive a webhook.
		Tx: sourceTx,
		// `rotateSourceIngestToken` is `createApiToken`'s defect twin: it hands out
		// a secret exactly once AND revokes the previous one, so a retry destroyed
		// the credential the caller was still holding. The claim joins the
		// rotation's own transaction.
		Claims: c.Idempotency,
		// `createCluster` is the last write that went API→repository directly, so
		// there was no transaction for its claim to join. The registry comes here
		// for that; `sources/api` keeps the same repository for the reads.
		Clusters: clusterRepo,
	})
	if err != nil {
		return nil, err
	}

	// ---- rules: content-addressed snapshots, plus the lookup adapter -----
	//
	// `Events` is LATE-BOUND for the same reason `grouping`'s ports are: the alert
	// timeline belongs to a service built further down this function, and rules is
	// built first because it depends on nothing. The holder is filled the moment
	// `c.Alerts` exists; until then it answers nil, which is the port's documented
	// "captured but not narrated" degradation and is what the unit tests run
	// against anyway.
	timeline := &timelineRecorder{}
	c.Rules, err = rulesservice.New(rulesservice.Options{
		Repo:   rulesrepo.NewSnapshotRepository(general),
		Lookup: ruleLookup{sources: c.Sources},
		Events: timeline,
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
	alertRepo := alertsrepo.NewAlertRepository(general, clk, trigramAvailable)
	caseRepo := alertsrepo.NewCaseRepository(general)
	// `case_policy_config` — the case retention window W (migration 00057). Wiring
	// it changes nothing on its own: the table starts empty and an absent row means
	// W=0, which is the close-on-resolve behaviour oto has always had.
	casePolicyRepo := alertsrepo.NewCasePolicyRepository(general)
	// The same table read the other way round: `CasePolicies` is the lifecycle's
	// read of W at transition time, `casePolicyConfigRepo` is the settings surface
	// behind /api/v1/case-policies. Unwired, those four routes answer 503.
	casePolicyConfigRepo := alertsrepo.NewCasePolicyConfigRepository(general, clk)
	eventRepo := alertsrepo.NewEventRepository(general, clk)
	snoozeRepo := alertsrepo.NewSnoozeRepository(general, clk)
	enrichmentRepo := enrichrepo.NewEnrichmentRepository(general).WithLogger(logger)

	notificationsPort := &notificationReader{}

	c.Alerts, err = alertsservice.New(alertsservice.Deps{
		Alerts:           alertRepo,
		Cases:            caseRepo,
		Events:           eventRepo,
		Snoozes:          snoozeRepo,
		Tx:               alertsrepo.NewTxRunner(general),
		AlertLister:      alertRepo,
		AlertBatch:       alertRepo,
		OccBatch:         caseRepo,
		OccSources:       caseRepo,
		CasePolicies:     casePolicyRepo,
		CasePolicyConfig: casePolicyConfigRepo,
		SnoozeHistory:    snoozeRepo,
		Enqueuer:         c.enqueuer,
		Stream:           stream,
		Health:           sourceHealth{svc: c.Sources},
		Settings:         settings,
		Enrichments:      enrichmentReader{repo: enrichmentRepo},
		Notifications:    notificationsPort,
		// `commentOnAlert` and `snoozeAlert` take their claim inside the same
		// transaction as the write, on the store every other guarded operation
		// claims in. A comment is the one action a retry duplicates VISIBLY —
		// the same sentence twice on an incident timeline.
		Claims: c.Idempotency,
		Clock:  clk,
		Logger: logger,
	})
	if err != nil {
		return nil, err
	}
	// The timeline exists now, so the two narrators that were built before it can
	// reach it. Every `rule.*` and `enrichment.*` row in `alert_events` is written
	// through this one line (§D.4.1, T11 and T12).
	timeline.svc = c.Alerts

	// ⛔ THE `grouping` MODULE WAS CONSTRUCTED HERE AND IS DELETED IN FULL (git-bug
	// `7570090`): "durable generations, membership, group lifecycle", its two
	// repositories, its own tx runner, and the `groupVersionsPort` late-binding that
	// existed only to break the `alerts` ⇄ `grouping` package cycle. A Case is the
	// conversation, so there is no generation to open, no membership to record and
	// no cycle left to break.

	// ---- enrichment: the budgeted, provenanced pipeline ------------------
	alertReadModel := enrichrepo.NewAlertReadModel(general)
	enricherRegistry, err := enrichservice.NewRegistry(
		promrule.New(c.Rules, caseRepo),
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
			alerts:  c.Alerts,
			sources: c.Sources,
			occSrc:  &caseSourceReader{resolver: caseRepo},
		},
		Notifier: enrichservice.NewQueueNotifier(c.enqueuer),
		Events:   timeline,
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
	observer := alertObserver{svc: c.Alerts}
	c.Ingestion, err = ingestion.New(ingestion.Deps{
		Pools:    o.Pools,
		Enqueuer: c.enqueuer,
		Config:   o.Config.Ingest,
		Alerts:   observer,
		// The READ side of the same boundary, for `oto replay` alone. It is built
		// over the two alerts repositories on the GENERAL pool — the pool they were
		// constructed on — because the supersession check is a human at a terminal
		// asking a question, and spending an ingest connection on it would put an
		// operator's recovery command in the way of the webhook path it is trying
		// to recover.
		AlertStates: alertStates{alerts: alertRepo, cases: caseRepo},
		Clock:       clk,
		Logger:      logger,
		Registry:    reg,
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
		Cases:         slackCaseActions{alerts: c.Alerts},
		// ⭐ THE SNOOZE MENU'S OTHER HALF (§B.8.6, git-bug `0a8ca4a`). The port is
		// nil-tolerant on purpose — a press at an unwired deployment is answered with
		// an ephemeral saying so rather than with silence — and a nil here is
		// therefore not a crash, which is exactly what made it possible to ship the
		// handlers and the menu with nothing behind them. Wiring it is what turns
		// "oto cannot snooze from Slack in this deployment yet" back into a snooze.
		Snoozes:  slackSnoozeActions{alerts: c.Alerts},
		Labels:   slackLabelReads{alerts: c.Alerts},
		Enqueuer: c.enqueuer,
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

	c.buildRouters(channelRepo, connectionRepo, credentialRepo, channelResolver, clusterRepo, identityTx, enricherRegistry, clk)
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
	digestRepo := notifrepo.NewDigestRepository(general)
	txRunner := notifrepo.NewTxRunner(general)

	c.NotifyScopes = notifrepo.NewScopeResolver(general)
	c.notifConfigRepo = notifrepo.NewConfigRepository(general, clk)

	// The same `orgs.settings` adapter the alerts and grouping modules read their
	// tuning through. One adapter means one answer: the group close delay that
	// ends a generation and the broadcast policy that decides how loudly a
	// transition lands cannot come from two different reads of the same row.
	settings := orgSettings{svc: c.Identity}

	var err error
	if c.Policies, err = notifservice.NewPolicyService(policyRepo, channelRepo); err != nil {
		return err
	}
	// `createPolicy` moves behind the service for the claim, on the SAME unit of
	// work the dispatch path uses: the claim and the insert commit together or
	// neither does.
	if c.PolicyWrites, err = notifservice.NewPolicyWriter(notifservice.PolicyWriterOptions{
		Store:  c.notifConfigRepo,
		Tx:     txRunner,
		Claims: c.Idempotency,
		Clock:  clk,
	}); err != nil {
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
		// `orgs.settings` on every evaluation — the same adapter the alerts and
		// grouping lifecycle ports use.
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
		BaseURL:  c.Config.HTTP.BaseURL,
		Clock:    clk,
		Logger:   logger,
		// ⭐ BOTH OF THESE ARE ALERTS, NOT STATISTICS, AND BOTH ARE INVISIBLE
		// FAILURES OTHERWISE. `oto_delivery_claim_lost_total`: a message that
		// exists in somebody's channel with no `sent` row behind it.
		// `oto_render_invalid_total`: a card oto could not build, whose delivery
		// dies while the job reports success, so no queue metric ever moves. Left
		// unregistered they are counters nothing could scrape, which is the same
		// as not having them.
		Metrics: notifservice.NewMetrics(reg),
		// The customer's per-Stanza templates (ADR 0037, ADR 0049). Resolution
		// happens at claim time, on the notification side, so the renderer stays a
		// pure function of (NotificationView, RenderOptions) and its golden files
		// keep meaning something. A deployment whose policies name no template
		// resolves none and every card reads in oto's own voice.
		Templates: channelsservice.NewTemplates(c.templates),
	}); err != nil {
		return err
	}

	// The digest tick (migration 00058). It takes the notification service for its
	// FAN-OUT and for nothing else: a digest is not routed (its policy is the input),
	// not snoozed (a snooze is keyed by `alert_key` and a digest names no alert) and
	// not throttled by a group cap (it lands on no group's thread) — see
	// DigestService.emit. It takes no settings reader, deliberately: there is no
	// org-level digest default and there must not be one, because a window is a
	// per-policy subscription rather than a volume dial.
	if c.Digests, err = notifservice.NewDigestService(notifservice.DigestConfig{
		Policies: policyRepo,
		Digests:  digestRepo,
		Notifier: c.Notify,
		Clock:    clk,
		Logger:   logger,
	}); err != nil {
		return err
	}

	if c.NotifyHistory, err = notifservice.NewHistoryService(notificationRepo); err != nil {
		return err
	}

	c.NotifyWorkers, err = notifworker.New(notifworker.Config{
		Scopes:   c.NotifyScopes,
		Notifier: c.Notify,
		Dispatch: c.Dispatch,
		Digests:  c.Digests,
		Logger:   logger,
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
//
// ⭐ IT TAKES THE COLLABORATORS THE ROUTERS ACTUALLY BIND. The sources API used
// to be handed `*SourceRepository`, `*TxRunner` and the API-token repository so
// it could write through them; since ticket 0869f21 it binds the `sources`
// service facade (`c.Sources`) instead, which owns the transaction and the
// `Idempotency-Key` claim taken inside it. The parameters outlived their last
// reader and are gone: an unused dependency in a wiring signature reads as a
// relationship that exists.
func (c *Container) buildRouters(
	channelRepo *channelsrepo.ChannelRepository,
	connectionRepo *channelsrepo.ConnectionRepository,
	credentialRepo *channelsrepo.CredentialRepository,
	channelResolver *channelsservice.Resolver,
	clusterRepo *sourcesrepo.ClusterRepository,
	identityTx *identityrepo.TxRunner,
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
			// One transaction for the minted credential and the `Idempotency-Key`
			// claim that guards it, and the store that claim is taken in. Together
			// they are what stops a retried create — a dropped response, a proxy
			// timeout, a double-clicked button — from handing out a second live
			// token whose secret nobody ever receives.
			Tx:     identityTx,
			Claims: c.Idempotency,
			Clock:  clk,
		}),
		alerts: alertsapi.NewRouter(c.Alerts, clk),
		// ⛔ THE `grouping` ROUTER WAS HERE AND IS DELETED (git-bug `7570090`), and
		// with it the nine `/api/v1/alert-groups*` paths and the
		// `groupDeliveryRollups` adapter that answered `delivery_summary` on the
		// group card — "was anybody told about this generation, and did it land".
		rules: rulesapi.NewRouter(c.Rules, c.Alerts, clk),
		sources: sourcesapi.NewRouter(sourcesapi.Options{
			Sources: c.Sources,
			// ⭐ THE WRITE FACADE, NOT THE REPOSITORY. `sources/service` owns the
			// create/update/delete/rotate path, the transaction around it and the
			// `Idempotency-Key` claim taken inside it, so this router binds a
			// request, calls one method and maps the error (ticket 0869f21).
			Registry: c.Sources,
			Clusters: clusterRepo,
			// `createCluster` went API→repository directly, so there was no
			// transaction for an `Idempotency-Key` claim to join. It goes through the
			// same service as every other write now; the repository above stays for
			// the reads.
			ClusterWrites: c.Sources,
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
			// Configuration-time SSRF feedback. The DIALER is the control; this is
			// so an operator who pastes a metadata-service URL sees a 422 naming the
			// field rather than a probe that mysteriously returns someone else's data.
			Guard:            c.NetGuard,
			AllowInsecureTLS: c.Config.Security.AllowInsecureTLS,
			Clock:            clk,
			BaseURL:          c.Config.HTTP.BaseURL,
		}),
		channels: channelsapi.NewRouter(channelsapi.Options{
			Registry:    c.ChannelRegistry,
			Channels:    channelRepo,
			Connections: connectionRepo,
			Creds:       credentialRepo,
			Writes:      c.ChannelWrites,
			Resolver:    channelResolver,
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
			// The authoring half of ADR 0037: the six /wordings routes, including
			// the preview that renders a candidate template against the shipped
			// fixture corpus in every Dialect and saves nothing.
			Templates: c.templates,
			Clock:     clk,
		}),
		notifs: notifapi.NewRouter(notifapi.Options{
			Policies: c.notifConfigRepo,
			// The service, not the repository: `createPolicy` takes its
			// `Idempotency-Key` claim inside the same transaction as the insert, so
			// a retry is answered with the original policy instead of a `409` from
			// `policies_name_uniq` that names nothing.
			PolicyWrites:  c.PolicyWrites,
			Audit:         c.notifConfigRepo,
			Notifications: notifrepo.NewNotificationRepository(c.Pools.General),
			Deliveries:    notifrepo.NewDeliveryRepository(c.Pools.General),
			Preview:       c.Policies,
			Views:         c.Views,
			Renderers:     c.ChannelRegistry,
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

// alertStates is `ingestion/service.AlertStateReader`: the READ half of the
// ingestion↔alerts boundary, and the only thing `oto replay` needs from the
// alerts module.
//
// ⭐ IT LIVES HERE, WITH alertObserver, FOR THE SAME REASON. `ingestion` may not
// import `alerts/repository`, and the one `alerts/domain` type it may name is
// `Observation` (§C.1, CONTEXT.md §5.2b). So the composition root translates: two
// existing reads in, one flat ingestion-owned struct out. No new SQL — the
// batched shapes were already there for the T2 material-change probe and for
// §G.4's "a 200-alert payload must not become 200 round trips".
//
// ⛔ IT ANSWERS ONLY WHAT THE REFUSAL NEEDS, and the temptation to widen it
// should be refused. A replay asks one question — has this alert moved since the
// batch was received — and every extra field handed across this boundary is a
// field the ingest path can start branching on.
type alertStates struct {
	alerts *alertsrepo.AlertRepository
	cases  *alertsrepo.CaseRepository
}

func (a alertStates) StatesByAlertKey(
	ctx context.Context, s db.TenantScope, alertKeys []string,
) (map[string]ingestionservice.AlertState, error) {
	out := make(map[string]ingestionservice.AlertState, len(alertKeys))
	if len(alertKeys) == 0 {
		return out, nil
	}

	found, err := a.alerts.GetByAlertKeys(ctx, s, alertKeys)
	if err != nil {
		return nil, err
	}

	ids := make([]uuid.UUID, 0, len(found))
	byID := make(map[uuid.UUID]alertsdomain.Alert, len(found))
	for _, al := range found {
		ids = append(ids, al.ID())
		byID[al.ID()] = al
	}
	latest, err := a.cases.LatestByAlerts(ctx, s, ids)
	if err != nil {
		return nil, err
	}

	for _, key := range alertKeys {
		al, ok := found[key]
		if !ok {
			// An alert this batch names that oto has never seen. Nothing can have
			// overtaken it, so it is reported as absent rather than omitted — the
			// caller's denominator is the batch, not the database.
			out[key] = ingestionservice.AlertState{}
			continue
		}

		st := ingestionservice.AlertState{
			Exists:   true,
			Identity: alertIdentity(al),
		}
		ac, ok := latest[al.ID()]
		if !ok {
			// An alert row with no episode yet. There is no timeline to duplicate.
			out[key] = st
			continue
		}

		st.State = ac.AlertState().String()
		st.Terminal = ac.State().IsClosed()
		st.SourceUpdatedAt = ac.SourceUpdatedAt()
		// ⛔ THE LATER OF THE TWO, AND `ended_at` IS WHY. Reaper expiry closes an
		// case without any upstream saying so and never touches
		// `source_updated_at`, so reporting only the latter would print "last moved"
		// as the day the alert last fired for an episode that closed a week later.
		st.MovedAt = ac.SourceUpdatedAt()
		if ac.EndedAt().After(st.MovedAt) {
			st.MovedAt = ac.EndedAt()
		}
		out[key] = st
	}
	return out, nil
}

// alertIdentity renders an alert the way an operator recognises it — the name
// plus the one label that says WHICH one — for the refusal message and nothing
// else.
//
// It is `service` before `namespace` because that is the order the alert list
// shows them in, and it degrades to the bare alertname rather than to an empty
// brace pair: an alert carrying neither is still identified by its key on the
// same line.
func alertIdentity(a alertsdomain.Alert) string {
	switch {
	case a.Service() != "":
		return a.AlertName() + "{service=" + a.Service() + "}"
	case a.Namespace() != "":
		return a.AlertName() + "{namespace=" + a.Namespace() + "}"
	default:
		return a.AlertName()
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
