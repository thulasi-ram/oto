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

	HTTP      HTTPConfig      `koanf:"http"`
	DB        DBConfig        `koanf:"db"`
	Log       LogConfig       `koanf:"log"`
	Telemetry TelemetryConfig `koanf:"telemetry"`
	Jobs      JobsConfig      `koanf:"jobs"`
	Ingest    IngestConfig    `koanf:"ingest"`
	Retention RetentionConfig `koanf:"retention"`
	Slack     SlackConfig     `koanf:"slack"`
	Security  SecurityConfig  `koanf:"security"`
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
	CORSOrigins    []string      `koanf:"cors_origins"`
	BaseURL        string        `koanf:"base_url"`
}

// DBConfig configures the two pgx pools mandated by SPEC §G.10.
// The ingest pool exists so that UI queries can never starve ingestion.
type DBConfig struct {
	URL string `koanf:"url" validate:"required"`

	MaxConns int32 `koanf:"max_conns" validate:"gte=4"`
	// IngestSharePercent is the share of MaxConns reserved for the ingest pool.
	IngestSharePercent int   `koanf:"ingest_share_percent" validate:"gt=0,lte=100"`
	IngestMinConns     int32 `koanf:"ingest_min_conns"     validate:"gte=1"`

	IngestStatementTimeout  time.Duration `koanf:"ingest_statement_timeout"  validate:"gt=0"`
	IngestAcquireTimeout    time.Duration `koanf:"ingest_acquire_timeout"    validate:"gt=0"`
	GeneralStatementTimeout time.Duration `koanf:"general_statement_timeout" validate:"gt=0"`
	GeneralAcquireTimeout   time.Duration `koanf:"general_acquire_timeout"   validate:"gt=0"`

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
	// RedactLabels are label keys whose values are redacted before persistence and logging.
	RedactLabels []string `koanf:"redact_labels"`
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
	// QueueIngest and QueueDefault are the worker counts per queue.
	QueueIngest   int           `koanf:"queue_ingest"   validate:"gte=0"`
	QueueDefault  int           `koanf:"queue_default"  validate:"gte=0"`
	QueueDelivery int           `koanf:"queue_delivery" validate:"gte=0"`
	FetchInterval time.Duration `koanf:"fetch_interval" validate:"gt=0"`
	JobTimeout    time.Duration `koanf:"job_timeout"    validate:"gt=0"`
	RescueAfter   time.Duration `koanf:"rescue_after"   validate:"gt=0"`
}

// IngestConfig configures the webhook accept path. SPEC §C.9 and §G.2.
type IngestConfig struct {
	MaxBatchBytes int64 `koanf:"max_batch_bytes" validate:"gt=0"`
	MaxAlerts     int   `koanf:"max_alerts"      validate:"gt=0"`
	MaxLabels     int   `koanf:"max_labels"      validate:"gt=0"`
	MaxLabelBytes int   `koanf:"max_label_bytes" validate:"gt=0"`
	// RetryAfter is the Retry-After sent with a 503. Never 429, never 4xx (C4).
	RetryAfter time.Duration `koanf:"retry_after" validate:"gt=0"`
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

// SecurityConfig holds the secret material the process needs at boot.
type SecurityConfig struct {
	// SecretKey is the base64 32-byte AES-256-GCM key used by platform/secrets.
	SecretKey     string        `koanf:"secret_key"`
	SessionTTL    time.Duration `koanf:"session_ttl"    validate:"gt=0"`
	SessionCookie string        `koanf:"session_cookie" validate:"required"`
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
			CORSOrigins:     []string{"http://localhost:5173"},
			BaseURL:         "http://localhost:8080",
		},
		DB: DBConfig{
			URL:                     "postgres://oto:oto@localhost:5432/oto?sslmode=disable",
			MaxConns:                20,
			IngestSharePercent:      25,
			IngestMinConns:          4,
			IngestStatementTimeout:  2 * time.Second,
			IngestAcquireTimeout:    500 * time.Millisecond,
			GeneralStatementTimeout: 15 * time.Second,
			GeneralAcquireTimeout:   5 * time.Second,
			MaxConnLifetime:         time.Hour,
			MaxConnIdleTime:         30 * time.Minute,
			ConnectTimeout:          5 * time.Second,
			AutoMigrate:             false,
		},
		Log: LogConfig{
			Level:        "info",
			Format:       "json",
			RedactLabels: []string{},
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
			Enabled:       true,
			QueueIngest:   10,
			QueueDefault:  5,
			QueueDelivery: 5,
			FetchInterval: time.Second,
			JobTimeout:    time.Minute,
			RescueAfter:   time.Hour,
		},
		Ingest: IngestConfig{
			MaxBatchBytes: 8 << 20,
			MaxAlerts:     5000,
			MaxLabels:     64,
			MaxLabelBytes: 2048,
			RetryAfter:    10 * time.Second,
		},
		Retention: RetentionConfig{
			RawPayloads: 14 * 24 * time.Hour,
			Events:      13 * 30 * 24 * time.Hour,
			UIEvents:    24 * time.Hour,
		},
		Slack: SlackConfig{
			Enabled: false,
			Mode:    "socket",
		},
		Security: SecurityConfig{
			SessionTTL:    30 * 24 * time.Hour,
			SessionCookie: "oto_session",
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

	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// sections are the top-level config keys. An env var is split on the first
// underscore that yields a known section; everything after it is one flat key.
var sections = []string{
	"http", "db", "log", "telemetry", "jobs", "ingest", "retention", "slack", "security",
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
	if cfg.Slack.Enabled && cfg.Slack.Mode == "http" && cfg.Slack.SigningSecret == "" {
		return errors.New("config: slack.signing_secret is required in http mode (an empty secret accepts forged requests)")
	}
	return nil
}

// IsProd reports whether this process is running in the production environment.
func (c Config) IsProd() bool { return c.Env == "prod" }
