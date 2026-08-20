//go:build load

package load

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/app"
	slackprov "github.com/thulasiram/oto/internal/channels/providers/slack"
	notifdomain "github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/config"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/platform/secrets"
	"github.com/thulasiram/oto/internal/platform/telemetry"
	"github.com/thulasiram/oto/test/harness"
)

// TestMain hands the whole package the harness's ONE Postgres. Startup is lazy,
// so a `-run` selector that matches nothing never pays for Docker.
func TestMain(m *testing.M) { harness.Main(m) }

// conversation is the Slack channel id every load case delivers into. It is ONE
// conversation on purpose: "a burst of thousands of alerts adds nothing to the
// channel beyond one root card per group" is only a falsifiable claim when many
// busy groups share a channel.
const conversation = "C000000LOAD"

// botToken must have the `xoxb-` shape or the conformance fake answers
// `invalid_auth` — which is the fake enforcing Slack's own contract, not a
// fixture detail to work around.
const botToken = "xoxb-oto-load-test"

// env is one load case's whole world: its own database, the real container with
// workers running, the real router over HTTP, and a conforming fake Slack.
//
// ⭐ NOTHING BELOW THE HTTP BOUNDARY IS FAKED EXCEPT SLACK. ADR 0021 permits a
// double only at an adapter port that leaves the process, and the point of a load
// case is to measure the real path: the real shedder, the real ingest pool with
// its 2 s statement timeout, real River workers, the real ordering gate on real
// advisory locks.
type env struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	srv   *httptest.Server
	slack *harness.SlackConformance
	c     *app.Container
	tel   *telemetry.Telemetry

	orgID     uuid.UUID
	sourceID  uuid.UUID
	channelID uuid.UUID
	token     string
	client    *http.Client
}

// newEnv builds the world. tweak adjusts the Config before the container is
// built, which is how the shedder case shrinks the ingest pool.
func newEnv(t *testing.T, tweak func(*config.Config)) *env {
	t.Helper()

	h := harness.New(t)
	slack := harness.NewSlackConformance(t, conversation)

	// The keyring key is generated per test and put in the Config, so the sealed
	// credential this file writes and the keyring the dispatcher unseals with are
	// provably the same key rather than the same literal.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("load: keyring key: %v", err)
	}
	encodedKey := base64.StdEncoding.EncodeToString(key)

	cfg := config.Default()
	cfg.DB.URL = h.DSN
	cfg.HTTP.BaseURL = "http://oto.load.test"
	cfg.Telemetry.MetricsEnabled = false
	cfg.Security.SecretKey = encodedKey
	cfg.Jobs.Enabled = true
	if tweak != nil {
		tweak(&cfg)
	}

	tel, err := telemetry.Setup(h.Ctx, cfg)
	if err != nil {
		t.Fatalf("load: telemetry: %v", err)
	}

	pools, err := db.Open(h.Ctx, cfg.DB)
	if err != nil {
		t.Fatalf("load: pools: %v", err)
	}

	c, err := app.New(h.Ctx, app.Options{
		Config:    cfg,
		Logger:    loadLogger(),
		Pools:     pools,
		Telemetry: tel,
		// ⛔ THE REAL CLOCK, not the harness FakeClock. Every partition on this
		// schema is cut around `now()` and every rate bucket, retry delay and River
		// snooze is a real duration; a clock pinned at the harness Epoch would route
		// `ingest_batches` into a partition that does not exist and would freeze
		// every grace window at zero. A load case measures wall time by definition.
		Clock: clock.New(),
		// This process WORKS jobs. Without it the ingest path would answer 202 and
		// nothing would ever be delivered, which is the one shape a load case may
		// not have.
		RunWorkers: true,
		HTTPClient: slackClient(t, slack),
	})
	if err != nil {
		t.Fatalf("load: container: %v", err)
	}
	if err := c.Start(h.Ctx); err != nil {
		t.Fatalf("load: start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	srv := httptest.NewServer(c.Router())
	t.Cleanup(srv.Close)

	e := &env{
		t: t, ctx: h.Ctx, pool: h.Pool, srv: srv, slack: slack, c: c, tel: tel,
		client: &http.Client{Timeout: 60 * time.Second},
	}
	e.seed(encodedKey)
	return e
}

// loadLogger is silent by default and loud on request. A load case that printed
// one line per delivery would bury its own numbers.
func loadLogger() *slog.Logger {
	level, ok := map[string]slog.Level{
		"debug": slog.LevelDebug, "info": slog.LevelInfo,
		"warn": slog.LevelWarn, "error": slog.LevelError,
	}[os.Getenv("OTO_LOAD_LOG")]
	if !ok {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// ------------------------------------------------------------ Slack redirection

// slackClient is the outbound client the container hands the Slack provider.
//
// ⭐ THE REDIRECT IS AT THE TRANSPORT, NOT AT THE SDK. The container builds the
// provider itself and there is no seam to inject `slack.OptionAPIURL` through, so
// the double is installed one layer lower: every request bound for `slack.com` is
// re-pointed at the conformance server with its PATH UNTOUCHED, which means the
// real slack-go client, the real form encoding and the real `ts`-as-a-string
// discipline are all still in the path — exactly what
// `harness.SlackConformance` exists to judge.
func slackClient(t *testing.T, s *harness.SlackConformance) *http.Client {
	t.Helper()
	target, err := url.Parse(s.APIURL())
	if err != nil {
		t.Fatalf("load: slack fake url: %v", err)
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &slackRedirect{target: target, base: http.DefaultTransport},
	}
}

type slackRedirect struct {
	target *url.URL
	base   http.RoundTripper
}

func (t *slackRedirect) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "slack.com" {
		return t.base.RoundTrip(r)
	}
	clone := r.Clone(r.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	clone.Host = ""
	return t.base.RoundTrip(clone)
}

// ------------------------------------------------------------------- seeding

// seed writes the FK graph one burst needs: a tenant, a cluster, a push-enabled
// source with a live ingest token, a Slack destination with a really-sealed bot
// token, and one policy that routes every §H.6 Reason to it.
//
// It is SQL rather than the settings API because the API surface is not what is
// under test here and a fixture must not become a second thing that can fail.
// Everything that IS under test — accept, process, group, notify, dispatch — is
// driven through the real HTTP route and the real workers.
func (e *env) seed(encodedKey string) {
	e.t.Helper()
	now := time.Now().UTC()

	e.orgID = id.New()
	e.exec(`INSERT INTO orgs (id, slug, name, settings, created_at, updated_at)
	        VALUES ($1, $2, 'Load', $3::jsonb, $4, $4)`,
		e.orgID, "load-"+uuid.NewString()[:8],
		// ⭐ NO SETTINGS OVERRIDE. This used to pin `storm_cooldown_s` at 1800 so the
		// once-per-channel storm-notice latch could not re-arm mid-run. ADR 0042
		// deleted the key along with the latch, and no remaining tuning key changes
		// what these cases assert: nothing here resolves, expires or flaps, so the
		// grace windows are never consulted and the shipped defaults are the honest
		// configuration to measure against.
		`{}`, now)

	clusterID := id.New()
	e.exec(`INSERT INTO clusters (id, org_id, cluster_key, display_name, created_at, updated_at)
	        VALUES ($1, $2, 'prod', 'prod', $3, $3)`, clusterID, e.orgID, now)

	e.sourceID = id.New()
	// ⚠️ RECONCILE CANNOT BE TURNED OFF (ADR 0006, migration 00038 dropped the
	// column), so the fan-out will poll this unroutable Alertmanager on start and
	// once per interval. The interval is therefore pinned at its 3600 s ceiling:
	// the failures are harmless — a failed probe writes `source_health` and
	// produces no observation — but they are not what is being measured, and one
	// per thirty seconds would be noise in every latency figure below.
	e.exec(`INSERT INTO alert_sources (id, org_id, cluster_id, name, kind, base_url,
	          push_enabled, reconcile_interval_s, created_at, updated_at)
	        VALUES ($1, $2, $3, $4, 'alertmanager', 'https://am.invalid.example',
	                true, 3600, $5, $5)`,
		e.sourceID, e.orgID, clusterID, "load-"+uuid.NewString()[:8], now)

	// The ingest credential, hashed exactly as `api/auth.go` hashes the presented
	// bearer: sha256 of the secret, never the secret.
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		e.t.Fatalf("load: token bytes: %v", err)
	}
	e.token = "oto_ingest_" + hex.EncodeToString(raw)
	digest := sha256.Sum256([]byte(e.token))
	e.exec(`INSERT INTO api_tokens (id, org_id, kind, name, token_hash, prefix, source_id, created_at)
	        VALUES ($1, $2, 'ingest', 'load', $3, $4, $5, $6)`,
		id.New(), e.orgID, digest[:], e.token[:15], e.sourceID, now)

	// The Slack destination. The blob is SEALED WITH THE PROCESS KEYRING, not
	// stubbed: the dispatcher unseals it for real, so a credential-handling
	// regression fails here rather than in production.
	keyring, err := secrets.NewKeyringFromBase64(encodedKey)
	if err != nil {
		e.t.Fatalf("load: keyring: %v", err)
	}
	kind, values := e.slack.Credential(botToken)
	sealed, version, err := keyring.Seal(e.ctx, kind, values)
	if err != nil {
		e.t.Fatalf("load: seal credential: %v", err)
	}
	credID := id.New()
	e.exec(`INSERT INTO channel_credentials (id, org_id, kind, sealed, key_version, created_at)
	        VALUES ($1, $2, $3, $4, $5, $6)`,
		credID, e.orgID, kind, sealed, version, now)

	// The capability mask is taken from the PROVIDER'S OWN DESCRIPTOR rather than
	// written as a number. A literal here would be a second copy of a persisted
	// wire contract, and the two would drift the first time a bit was added.
	caps := int64(slackprov.NewProvider(slackprov.Options{}).Descriptor().Capabilities)

	e.channelID = id.New()
	e.exec(`INSERT INTO channels (id, org_id, type, name, config, credential_id, capabilities,
	          renderer, verbosity, thread_updates, enabled, created_at, updated_at)
	        VALUES ($1, $2, 'slack', $3, $4::jsonb, $5, $6, 'default', 'all', true, true, $7, $7)`,
		e.channelID, e.orgID, "load-alerts", []byte(e.slack.Config(conversation)),
		credID, caps, now)

	// One policy, no matchers, every Reason. A load case must not be able to lose
	// a delivery to a routing decision it did not intend to make.
	reasons := make([]string, 0, len(notifdomain.AllReasons()))
	for _, r := range notifdomain.AllReasons() {
		reasons = append(reasons, string(r))
	}
	e.exec(`INSERT INTO notification_policies (id, org_id, name, priority, enabled, matchers,
	          reasons, channel_ids, throttle, created_at, updated_at)
	        VALUES ($1, $2, 'load', 100, true, '[]'::jsonb, $3, $4, '{}'::jsonb, $5, $5)`,
		id.New(), e.orgID, reasons, []uuid.UUID{e.channelID}, now)
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("load: seed: %v\nSQL: %s", err, sql)
	}
}

func (e *env) queryInt(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("load: query: %v\nSQL: %s", err, sql)
	}
	return n
}

// ------------------------------------------------------------- the wire payload

type wireAlert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	GeneratorURL string            `json:"generatorURL"`
}

type wireBatch struct {
	Version            string            `json:"version"`
	GroupKey           string            `json:"groupKey"`
	TruncatedAlerts    int               `json:"truncatedAlerts"`
	Status             string            `json:"status"`
	Receiver           string            `json:"receiver"`
	GroupLabels        map[string]string `json:"groupLabels"`
	CommonLabels       map[string]string `json:"commonLabels"`
	CommonAnnotations  map[string]string `json:"commonAnnotations"`
	ExternalURL        string            `json:"externalURL"`
	NotificationReason string            `json:"notification_reason"`
	Alerts             []wireAlert       `json:"alerts"`
}

// batchSpec names one Alertmanager notification to build.
type batchSpec struct {
	// Group is the value of the `alertname` group label.
	//
	// ⛔ IT NO LONGER DECIDES A CONVERSATION (git-bug `7570090`). It used to be the
	// §C.4 group identity — one Group was one AlertGroup generation and one Slack
	// thread. `alert_groups` is dropped and a conversation is a Case, so this is now
	// only the alertname every alert in the batch carries, and every one of those
	// alerts opens its own conversation. The field keeps its name because it is
	// still literally the Alertmanager GROUP LABEL on the wire.
	Group string
	// Wave distinguishes successive batches for one group, so each carries alerts
	// oto has never seen. Repeating an alert set would recompute the SAME §C.5
	// dedup key and be answered 202-duplicate inside the five-minute replay
	// window, which measures the dedup table rather than the ingest path.
	Wave int
	// Alerts is how many alerts this batch carries.
	Alerts int
	// Reason is Alertmanager's `notification_reason`.
	Reason string
}

// body renders one Alertmanager v4 webhook body.
func (s batchSpec) body() []byte {
	now := time.Now().UTC().Format(time.RFC3339)
	alerts := make([]wireAlert, 0, s.Alerts)
	for i := 0; i < s.Alerts; i++ {
		instance := fmt.Sprintf("%s-w%d-i%04d", s.Group, s.Wave, i)
		alerts = append(alerts, wireAlert{
			Status: "firing",
			Labels: map[string]string{
				"alertname": s.Group,
				"severity":  "critical",
				"service":   "checkout",
				"instance":  instance,
			},
			Annotations: map[string]string{
				"summary":     "error rate is high on " + instance,
				"description": "the load case is pushing a burst through the real path",
			},
			StartsAt:     now,
			GeneratorURL: "http://prometheus.load.test/graph",
		})
	}
	raw, err := json.Marshal(wireBatch{
		Version:            "4",
		GroupKey:           fmt.Sprintf(`{}:{alertname="%s"}`, s.Group),
		Status:             "firing",
		Receiver:           "oto",
		GroupLabels:        map[string]string{"alertname": s.Group},
		CommonLabels:       map[string]string{"alertname": s.Group, "severity": "critical"},
		CommonAnnotations:  map[string]string{},
		ExternalURL:        "http://alertmanager.load.test",
		NotificationReason: s.Reason,
		Alerts:             alerts,
	})
	if err != nil {
		panic("load: encode batch: " + err.Error())
	}
	return raw
}
