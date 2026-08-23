package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/knadh/koanf/parsers/yaml"
	kenv "github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"

	"github.com/thulasiram/oto/internal/platform/ratelimit"
	"github.com/thulasiram/oto/internal/platform/tuning"
)

// EnvPrefix is the prefix every oto environment variable carries.
// OTO_HTTP_ADDR maps onto http.addr, OTO_DB_INGEST_MAX_CONNS onto db.ingest_max_conns.
const EnvPrefix = "OTO_"

// Config is the complete, typed configuration of an oto process.
// Every field has a default; nothing here may be read before Load has validated it.
type Config struct {
	Env     string `koanf:"env"      validate:"required,oneof=dev staging prod"`
	Service string `koanf:"service"  validate:"required"`
	Version string `koanf:"version"`
	// Commit and BuildDate are stamped by the linker (see the Dockerfile's -ldflags)
	// and passed through here so GET /api/v1/version can answer "which build is
	// this" without importing package main. Empty in a `go run` build.
	Commit    string `koanf:"commit"`
	BuildDate string `koanf:"build_date"`

	HTTP      HTTPConfig      `koanf:"http"`
	DB        DBConfig        `koanf:"db"`
	Log       LogConfig       `koanf:"log"`
	Telemetry TelemetryConfig `koanf:"telemetry"`
	Jobs      JobsConfig      `koanf:"jobs"`
	Ingest    IngestConfig    `koanf:"ingest"`
	Retention RetentionConfig `koanf:"retention"`
	Slack     SlackConfig     `koanf:"slack"`
	Security  SecurityConfig  `koanf:"security"`

	// tuning is the DECLARATIVE per-org tuning layer, harvested from `tuning.*`
	// and OTO_TUNING_* (see tuning.go). It is unexported, and that is load-bearing
	// twice over: koanf's structs provider and mapstructure both ignore it, so the
	// provenance recorded on every entry cannot be clobbered by a stray
	// `OTO_TUNING` of its own; and nothing can read it without going through
	// TuningEntries, which hands back a copy.
	//
	// There is no typed struct here on purpose. The closed key set, the value
	// types and the bounds live in `identity/domain`, and a second copy in
	// `platform` would be a second copy that can disagree.
	tuning []TuningEntry
}

// HTTPConfig configures the public HTTP surface.
type HTTPConfig struct {
	Addr            string        `koanf:"addr"             validate:"required"`
	ReadTimeout     time.Duration `koanf:"read_timeout"     validate:"gt=0"`
	WriteTimeout    time.Duration `koanf:"write_timeout"    validate:"gt=0"`
	IdleTimeout     time.Duration `koanf:"idle_timeout"     validate:"gt=0"`
	ShutdownTimeout time.Duration `koanf:"shutdown_timeout" validate:"gt=0"`
	// RequestTimeout bounds a single non-streaming request. SSE routes opt out.
	RequestTimeout time.Duration `koanf:"request_timeout" validate:"gt=0"`
	MaxBodyBytes   int64         `koanf:"max_body_bytes"  validate:"gt=0"`
	// CORSOrigins is the exact list of browser origins allowed to read
	// authenticated responses.
	//
	// ⛔ IT DEFAULTS TO EMPTY, WHICH DISABLES CORS. It used to default to
	// `http://localhost:5173` — the SolidJS dev server — while `AllowCredentials`
	// was true, so any production deployment that never set
	// OTO_HTTP_CORS_ORIGINS let a page served from a developer's laptop read
	// every authenticated response with the user's cookie attached. A convenience
	// default on a credentialed CORS policy is a hole with a friendly name; the
	// dev server sets the variable, like every other origin has to.
	CORSOrigins []string `koanf:"cors_origins"`
	// BaseURL is the absolute root of every oto link oto ever emits — the Slack
	// card's deep links and the webhook URL an operator pastes into Alertmanager.
	//
	// ⛔ IT CARRIED NO VALIDATION AT ALL AND THAT WAS A BUG (git-bug `3f5e952`).
	// `DBConfig.URL` six lines down has always been `validate:"required"`; this had
	// nothing, so an operator could set it empty — an env override, a config file, a
	// Helm value that renders blank — and oto would start cleanly with no working
	// link anywhere. Two surfaces degrade to the empty string DELIBERATELY and
	// silently: `notification/service.links` guards on `baseURL != ""` and leaves
	// every card's deep link empty, and `sources/api.webhookURL` returns "" for the
	// URL that makes ingestion work at all — so oto appears configured and receives
	// nothing, with the diagnostic pointing at Alertmanager.
	//
	// ⭐ `http_url` RATHER THAN `url`, AND THE DEFAULT IS KEPT. `url` would admit
	// `oto.example.com` with no scheme, which Slack will not linkify and which
	// produces a card whose link silently does nothing. Keeping the
	// `http://localhost:8080` default preserves the zero-config dev path; what is
	// refused is an explicitly WRONG value, which is the realistic failure.
	BaseURL string `koanf:"base_url" validate:"required,http_url"`
}

// DBConfig configures the two pgx pools mandated by SPEC §G.10.
// The ingest pool exists so that UI queries can never starve ingestion.
type DBConfig struct {
	URL string `koanf:"url" validate:"required"`

	MaxConns int32 `koanf:"max_conns" validate:"gte=4"`
	// IngestSharePercent is the share of MaxConns reserved for the ingest pool.
	IngestSharePercent int   `koanf:"ingest_share_percent" validate:"gt=0,lte=100"`
	IngestMinConns     int32 `koanf:"ingest_min_conns"     validate:"gte=1"`

	// The statement timeouts are the only per-pool budgets pgx can enforce for
	// us: they are set as `statement_timeout` runtime parameters on every
	// connection the pool opens (see platform/db.open).
	//
	// ⛔ THERE IS NO ACQUISITION TIMEOUT HERE, and there never was one that
	// worked. pgxpool has no such setting — `Acquire` waits on the caller's
	// context and nothing else — so a `db.ingest_acquire_timeout` key could only
	// ever have been read by whoever bounds the wait, which is the ingest
	// shedder. It lives there now, as `ingest.acquire_timeout`.
	IngestStatementTimeout  time.Duration `koanf:"ingest_statement_timeout"  validate:"gt=0"`
	GeneralStatementTimeout time.Duration `koanf:"general_statement_timeout" validate:"gt=0"`

	MaxConnLifetime time.Duration `koanf:"max_conn_lifetime" validate:"gt=0"`
	MaxConnIdleTime time.Duration `koanf:"max_conn_idle_time" validate:"gt=0"`
	ConnectTimeout  time.Duration `koanf:"connect_timeout"    validate:"gt=0"`
	// AutoMigrate runs goose up at boot. Off by default: migrations are an explicit step.
	AutoMigrate bool `koanf:"auto_migrate"`
}

// IngestPoolSize is the resolved max size of the ingest pool.
func (c DBConfig) IngestPoolSize() int32 {
	n := int32(int(c.MaxConns) * c.IngestSharePercent / 100)
	if n < c.IngestMinConns {
		n = c.IngestMinConns
	}
	if n > c.MaxConns-1 {
		n = c.MaxConns - 1
	}
	return n
}

// GeneralPoolSize is the resolved max size of the general pool: the remainder.
func (c DBConfig) GeneralPoolSize() int32 {
	n := c.MaxConns - c.IngestPoolSize()
	if n < 1 {
		n = 1
	}
	return n
}

// LogConfig configures log/slog.
type LogConfig struct {
	Level  string `koanf:"level"  validate:"required,oneof=debug info warn error"`
	Format string `koanf:"format" validate:"required,oneof=json text"`
	// ⛔ THERE IS NO PROCESS-WIDE `redact_labels` HERE. There used to be, and
	// `.env.example` promised it blanked label values before the raw payload was
	// persisted; nothing read it, so it blanked nothing and the promise was a
	// privacy claim oto did not keep.
	//
	// Redaction is per-source and it is real: `alert_sources.redact_labels` and
	// `redact_annotations` are glob patterns applied by `ingestion/decode.Redactor`
	// BEFORE `ingest_batches.payload` is written (§C.9.2, `ingestion/service.Accept`),
	// and again by the reconciler. Which labels carry customer identifiers is a
	// property of the upstream that emits them, so that is where the setting
	// belongs — and a second, process-wide list would be a second definition of
	// "sensitive" that can disagree with the first.
	//
	// SourceLocation adds the caller file:line to every record. Costly; off by default.
	SourceLocation bool `koanf:"source_location"`
}

// TelemetryConfig configures OpenTelemetry and the Prometheus registry.
type TelemetryConfig struct {
	MetricsEnabled  bool    `koanf:"metrics_enabled"`
	MetricsPath     string  `koanf:"metrics_path"     validate:"required,startswith=/"`
	TracingEnabled  bool    `koanf:"tracing_enabled"`
	OTLPEndpoint    string  `koanf:"otlp_endpoint"`
	OTLPInsecure    bool    `koanf:"otlp_insecure"`
	TraceSampleRate float64 `koanf:"trace_sample_rate" validate:"gte=0,lte=1"`
}

// JobsConfig configures the river job queues.
type JobsConfig struct {
	Enabled bool `koanf:"enabled"`
	// QueueIngest, QueueDefault and QueueDelivery are OVERRIDES, not defaults.
	// ZERO MEANS UNSET, and unset falls through to `jobs.DefaultQueueWorkers` — the
	// SPEC §G.3 table, which is where the reasoning for each width lives. Setting
	// one here departs from the published number, so a deployment that sets them is
	// answering for its own widths.
	QueueIngest   int `koanf:"queue_ingest"   validate:"gte=0"`
	QueueDefault  int `koanf:"queue_default"  validate:"gte=0"`
	QueueDelivery int `koanf:"queue_delivery" validate:"gte=0"`

	// QueueReconcile is the `reconcile` queue's worker count, and it gets a knob of
	// its own rather than riding `queue_default` because it is the ONE queue whose
	// width is a TENANT-COUNT CEILING rather than a throughput preference.
	//
	// The `reconcile` queue's per-source passes are network-bound calls to somebody
	// else's Alertmanager, and the fan-out tick offers a fresh round of them every
	// 30 s. Below the width SPEC §G.3.1's arithmetic requires for a given tenant
	// count, `source.reconcile` does not merely run slower — it falls permanently
	// behind its own schedule, and sources are discovered as due later and later.
	// The number is published with its arithmetic in SPEC §G.3.1; this is the knob
	// that lets an operator move it without a rebuild.
	QueueReconcile int `koanf:"queue_reconcile" validate:"gte=0"`

	FetchInterval time.Duration `koanf:"fetch_interval" validate:"gt=0"`
	JobTimeout    time.Duration `koanf:"job_timeout"    validate:"gt=0"`
	RescueAfter   time.Duration `koanf:"rescue_after"   validate:"gt=0"`
}

// IngestConfig configures the webhook accept path. SPEC §C.9 and §G.2.
//
// ⛔ THE B1-B19 BOUNDS ARE NOT HERE AND ARE NOT CONFIGURABLE. `max_batch_bytes`,
// `max_alerts`, `max_labels` and `max_label_bytes` were declared here, published
// in `.env.example`, validated at boot — and read by nothing, while the enforced
// limits sat in `ingestion/domain.bounds` as constants. `OTO_INGEST_MAX_ALERTS`
// even advertised 5000 against an enforced truncation point of 10000, so an
// operator lowering a cap mid-incident changed nothing and was told nothing.
//
// They are constants because each is BOUND TO SOMETHING ELSE THAT CANNOT MOVE
// WITH AN ENVIRONMENT VARIABLE: `MaxAlertsPerBatch` is `ingest_batches_count_ck`,
// `MaxBodyBytes` is `ingest_batches_bytes_ck`, and `MaxLabelsPerAlert` is
// `alerts/domain.MaxLabels`, which the LabelSet constructor and a DDL CHECK both
// enforce. A knob that can disagree with the CHECK it is supposed to describe
// turns a rejected alert into a 500.
type IngestConfig struct {
	// RetryAfter is the Retry-After sent with a 503. Never 429, never 4xx (C4).
	RetryAfter time.Duration `koanf:"retry_after" validate:"gt=0"`
	// AcquireTimeout is how long a webhook may wait for an ingest slot before it
	// is shed with a 503 (§G.10). It is the ingest pool's acquisition budget, and
	// the shedder is what enforces it — pgxpool has no acquire timeout of its
	// own, which is why this is `ingest.acquire_timeout` and not a `db.*` key.
	//
	// Lower it and oto gives up on a queued webhook sooner, spending less of
	// Alertmanager's ~5-minute retry budget on waiting; raise it and oto holds
	// the upstream's connection open for longer before answering.
	AcquireTimeout time.Duration `koanf:"acquire_timeout" validate:"gt=0"`
}

// RetentionConfig configures the partition drop schedule.
type RetentionConfig struct {
	RawPayloads time.Duration `koanf:"raw_payloads" validate:"gt=0"`
	Events      time.Duration `koanf:"events"       validate:"gt=0"`
	UIEvents    time.Duration `koanf:"ui_events"    validate:"gt=0"`
}

// SlackConfig configures the Slack provider transport. Socket Mode is the default (C13).
type SlackConfig struct {
	Enabled       bool   `koanf:"enabled"`
	Mode          string `koanf:"mode" validate:"required,oneof=socket http"`
	AppToken      string `koanf:"app_token"`
	SigningSecret string `koanf:"signing_secret"`
}

// SecurityConfig holds the secret material and the deployment-level trust
// decisions the process needs at boot.
//
// ⛔ EVERY SWITCH BELOW IS DEPLOYMENT-LEVEL AND NONE OF THEM IS PER-TENANT. Each
// says something about the network oto runs in or the certificates it trusts,
// which is knowledge an operator has and an org member does not. A tenant-visible
// field that granted any of them would be a field that lets one customer read the
// host's metadata service.
type SecurityConfig struct {
	// SecretKey is the base64 32-byte AES-256-GCM key used by platform/secrets.
	SecretKey     string        `koanf:"secret_key"`
	SessionTTL    time.Duration `koanf:"session_ttl"    validate:"gt=0"`
	SessionCookie string        `koanf:"session_cookie" validate:"required"`

	// AllowPrivateTargets opens the SSRF guard (`platform/netguard`) for a
	// self-hosted install whose Alertmanager, Prometheus or webhook receiver
	// genuinely sits on a private network.
	//
	// ⛔ DEFAULT CLOSED, and it opens the guard for the WHOLE PROCESS. With it on,
	// oto will dial 10.0.0.0/8, 127.0.0.1 and 169.254.169.254 on behalf of any
	// tenant, so it belongs only on a single-tenant install.
	// OTO_SECURITY_ALLOW_PRIVATE_TARGETS.
	AllowPrivateTargets bool `koanf:"allow_private_targets"`

	// AllowInsecureTLS lets `alert_sources.tls_skip_verify` and a webhook channel's
	// `config.insecure_skip_verify` actually take effect.
	//
	// ⛔ DEFAULT CLOSED. Both are tenant-writable — through `POST /api/v1/sources`
	// and `POST /api/v1/channels` — and honouring either unconditionally let any
	// org member turn off certificate verification for an outbound connection made
	// by oto's own process. On the channel side that traffic is the notification
	// itself: labels, annotations and the rule expression, i.e. a description of
	// what is broken inside the customer's infrastructure. Whether an unverified
	// certificate is acceptable is a statement about the operator's network. With
	// this false, both flags are refused at validation rather than silently
	// dropped. OTO_SECURITY_ALLOW_INSECURE_TLS.
	AllowInsecureTLS bool `koanf:"allow_insecure_tls"`

	// LoginRateBurst and LoginRateRefill bound `POST /api/v1/auth/login` per
	// client address; LoginMaxConcurrent bounds how many argon2id verifications
	// run at once. See `platform/ratelimit` for why the second is the one that
	// bounds memory.
	LoginRateBurst     int           `koanf:"login_rate_burst"     validate:"gt=0"`
	LoginRateRefill    time.Duration `koanf:"login_rate_refill"    validate:"gt=0"`
	LoginMaxConcurrent int           `koanf:"login_max_concurrent" validate:"gt=0"`
}

// Default returns the configuration oto boots with when nothing is supplied.
func Default() Config {
	return Config{
		Env:     "dev",
		Service: "oto",
		Version: "dev",
		HTTP: HTTPConfig{
			Addr:            ":8080",
			ReadTimeout:     15 * time.Second,
			WriteTimeout:    30 * time.Second,
			IdleTimeout:     120 * time.Second,
			ShutdownTimeout: 20 * time.Second,
			RequestTimeout:  30 * time.Second,
			MaxBodyBytes:    16 << 20,
			CORSOrigins:     []string{},
			BaseURL:         "http://localhost:8080",
		},
		DB: DBConfig{
			URL:                     "postgres://oto:oto@localhost:5432/oto?sslmode=disable",
			MaxConns:                20,
			IngestSharePercent:      25,
			IngestMinConns:          4,
			IngestStatementTimeout:  2 * time.Second,
			GeneralStatementTimeout: 15 * time.Second,
			MaxConnLifetime:         time.Hour,
			MaxConnIdleTime:         30 * time.Minute,
			ConnectTimeout:          5 * time.Second,
			AutoMigrate:             false,
		},
		Log: LogConfig{
			Level:  "info",
			Format: "json",
		},
		Telemetry: TelemetryConfig{
			MetricsEnabled:  true,
			MetricsPath:     "/metrics",
			TracingEnabled:  false,
			OTLPEndpoint:    "localhost:4317",
			OTLPInsecure:    true,
			TraceSampleRate: 0.1,
		},
		Jobs: JobsConfig{
			Enabled: true,
			// ⛔ THE FOUR QUEUE WIDTHS ARE DELIBERATELY ABSENT, i.e. zero, AND ZERO
			// MEANS "UNSET" RATHER THAN "NO WORKERS". `jobs.FromPlatformConfig`
			// applies each field only when it is > 0, so leaving them unset is what
			// lets `jobs.DefaultQueueWorkers` — the SPEC §G.3 table — be the default.
			//
			// They used to be set here (ingest 10, default 5, delivery 5), and that
			// made the fall-through unreachable: SIX OF EIGHT queues ran a width the
			// published table does not name, in every deployment that did not set the
			// environment. Two of the six moved against the rationale that justifies
			// them — `deliver_slack` widened 4 -> 5, when it is the narrowest ON
			// PURPOSE because Slack allows roughly one message per second per channel
			// and extra workers buy 429s — and `ingest` narrowed 16 -> 10, when it is
			// the widest on purpose so a webhook batch never queues behind anything.
			//
			// ⭐ ONE TABLE DECIDES A DEFAULT QUEUE WIDTH, AND IT IS THE ONE THAT
			// CARRIES THE REASONING. §G.3.1 publishes arithmetic and invites an
			// operator to re-derive `W` for their own upstreams; that invitation is
			// only honest if the `W` in the table is the `W` in the process. These
			// fields remain OVERRIDES — set `jobs.queue_*` to depart from the table.
			// `TestConfigDefaultsProduceTheSpecQueueWidths` holds the two together.
			FetchInterval: time.Second,
			JobTimeout:    time.Minute,
			RescueAfter:   time.Hour,
		},
		Ingest: IngestConfig{
			RetryAfter:     10 * time.Second,
			AcquireTimeout: 500 * time.Millisecond,
		},
		// The FLOOR the partition dropper starts from, not the last word: §D.11
		// retention is a table-level property, so `partitions.manage` takes the
		// MAXIMUM of this and every org's `orgs.settings` value (ADR 0024).
		//
		// ⛔ THESE ARE THE SAME NUMBERS `identity/domain` SERVES, so they are the
		// same CONSTANTS now. They used to be literals under a comment saying they
		// "mirror identity/domain.DefaultRawRetention and DefaultEventRetention and
		// must move with them" — and platform may never import a domain (rule 7),
		// so there was no way to say it in code until the numbers moved down to
		// `platform/tuning`. A floor that drifted BELOW an org's own retention would
		// drop a partition that org still expects to be able to read.
		Retention: RetentionConfig{
			RawPayloads: tuning.DefaultRawRetention,
			Events:      tuning.DefaultEventRetention,
			UIEvents:    24 * time.Hour,
		},
		Slack: SlackConfig{
			Enabled: false,
			Mode:    "socket",
		},
		Security: SecurityConfig{
			SessionTTL:    30 * 24 * time.Hour,
			SessionCookie: "oto_session",
			// Default closed, both of them. A guard that has to be turned ON is a
			// guard an operator has thought about; one that has to be turned off is
			// one nobody ever notices was never on.
			AllowPrivateTargets: false,
			AllowInsecureTLS:    false,
			LoginRateBurst:      ratelimit.DefaultBurst,
			LoginRateRefill:     ratelimit.DefaultRefill,
			LoginMaxConcurrent:  ratelimit.DefaultConcurrency,
		},
	}
}

// Load resolves configuration in the binding order: defaults, then the optional YAML
// file at path, then OTO_* environment variables. The result is validated before return.
func Load(path string) (Config, error) {
	k := koanf.New(".")

	if err := k.Load(structs.Provider(Default(), "koanf"), nil); err != nil {
		return Config{}, fmt.Errorf("config: load defaults: %w", err)
	}

	if path != "" {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return Config{}, fmt.Errorf("config: load file %q: %w", path, err)
		}
	}

	envProvider := kenv.Provider(".", kenv.Opt{
		Prefix: EnvPrefix,
		TransformFunc: func(key, value string) (string, any) {
			key = strings.ToLower(strings.TrimPrefix(key, EnvPrefix))
			key = envKeyToPath(key)
			if strings.Contains(value, ",") {
				return key, strings.Split(value, ",")
			}
			return key, value
		},
	})
	if err := k.Load(envProvider, nil); err != nil {
		return Config{}, fmt.Errorf("config: load env: %w", err)
	}

	cfg := Default()
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return Config{}, fmt.Errorf("config: unmarshal: %w", err)
	}

	// The declarative tuning layer is harvested separately, because the merged
	// koanf above cannot say WHICH provider set a key and "managed by
	// configuration" without a key to go and edit is a wall, not an answer.
	tuning, err := loadTuning(path)
	if err != nil {
		return Config{}, err
	}
	cfg.tuning = tuning

	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// sections are the top-level config keys. An env var is split on the first
// underscore that yields a known section; everything after it is one flat key.
var sections = []string{
	"http", "db", "log", "telemetry", "jobs", "ingest", "retention", "slack", "security",
	// `tuning` carries no field on Config: it is harvested by loadTuning and typed
	// by identity/domain. It is named here anyway so that OTO_TUNING_RESOLVE_GRACE_S
	// resolves to `tuning.resolve_grace_s` in the merged instance too, and a values
	// file and an env var describe the same path everywhere in the process.
	TuningSection,
}

func envKeyToPath(key string) string {
	for _, s := range sections {
		if strings.HasPrefix(key, s+"_") {
			return s + "." + strings.TrimPrefix(key, s+"_")
		}
	}
	return key
}

var validate = validator.New(validator.WithRequiredStructEnabled())

// Validate reports every constraint the Config violates.
func Validate(cfg Config) error {
	if err := validate.Struct(cfg); err != nil {
		var verrs validator.ValidationErrors
		if errors.As(err, &verrs) {
			msgs := make([]string, 0, len(verrs))
			for _, fe := range verrs {
				msgs = append(msgs, fmt.Sprintf("%s: failed %q", fe.Namespace(), fe.Tag()))
			}
			return fmt.Errorf("config: invalid: %s", strings.Join(msgs, "; "))
		}
		return fmt.Errorf("config: invalid: %w", err)
	}
	if cfg.DB.IngestPoolSize() >= cfg.DB.MaxConns {
		return errors.New("config: db.ingest_share_percent leaves no connections for the general pool")
	}
	// A credentialed CORS policy with a wildcard origin is not a policy. The
	// browser refuses the combination anyway; refusing it at boot means an
	// operator learns from a startup error rather than from a support ticket.
	for _, o := range cfg.HTTP.CORSOrigins {
		if strings.TrimSpace(o) == "*" {
			return errors.New("config: http.cors_origins may not contain '*': oto sends credentials, and a wildcard origin with credentials is refused by every browser")
		}
	}
	// ⛔ THE CHARACTERS THAT SILENTLY CORRUPT A SLACK LINK, REFUSED AT BOOT.
	//
	// `render/slack.link` builds `<url|label>` and escapes the LABEL, never the URL —
	// correctly, because the Alertmanager silence deep link is legitimately
	// percent-encoded and escaping would mangle it. So every mrkdwn metacharacter in
	// `base_url` reaches the payload verbatim: a `|` gives the tag two separators and
	// Slack takes the first, so the label becomes part of the URL; a `>` closes the
	// tag early and the label falls out as literal text; a newline sits raw inside
	// the span. `http_url` above does not reliably reject these — Go's URL parser is
	// permissive about host characters — so they are named here, in the CORS check's
	// own shape: a rule that needs a sentence gets one.
	//
	// ⚠️ The failure this prevents is INVISIBLE, which is why it is worth a boot
	// check rather than a runtime guard: nothing errors, nothing logs, and the card
	// still ships — just wrong, for every reader.
	if i := strings.IndexAny(cfg.HTTP.BaseURL, "<>|\n\r\t "); i >= 0 {
		return fmt.Errorf(
			"config: http.base_url contains %q at byte %d: oto renders it into Slack "+
				"mrkdwn as <url|label>, where < > and | are control characters and "+
				"whitespace is not legal in a URL — the card would ship silently wrong",
			cfg.HTTP.BaseURL[i], i)
	}
	if cfg.Slack.Enabled && cfg.Slack.Mode == "http" && cfg.Slack.SigningSecret == "" {
		return errors.New("config: slack.signing_secret is required in http mode (an empty secret accepts forged requests)")
	}
	return nil
}

// IsProd reports whether this process is running in the production environment.
func (c Config) IsProd() bool { return c.Env == "prod" }
