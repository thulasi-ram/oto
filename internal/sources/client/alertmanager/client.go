package alertmanager

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpc"
	"github.com/thulasiram/oto/internal/sources/domain"
)

// ErrPrefix namespaces every error code this client produces, so that
// `alertmanager_unreachable` can never be confused with `prometheus_unreachable`
// after the error has crossed three service boundaries.
const ErrPrefix = "alertmanager"

// API v2 paths. There is no v1 and no v3: every v1 path has returned HTTP 410
// Gone since Alertmanager 0.27.0 (research A4), and the repository contains no
// api/v3 at all.
//
// Note the singular/plural asymmetry, which is the easiest thing in this API to
// get wrong: `/silences` lists and creates, `/silence/{id}` reads and expires.
const (
	// PathStatus is GET /api/v2/status.
	PathStatus = "/api/v2/status"
	// PathAlerts is GET /api/v2/alerts.
	PathAlerts = "/api/v2/alerts"
	// PathAlertGroups is GET /api/v2/alerts/groups.
	PathAlertGroups = "/api/v2/alerts/groups"
	// PathSilences is GET /api/v2/silences (PLURAL: list).
	PathSilences = "/api/v2/silences"
	// PathSilence is GET /api/v2/silence/{id} (SINGULAR: one silence).
	PathSilence = "/api/v2/silence/"
	// PathReceivers is GET /api/v2/receivers.
	PathReceivers = "/api/v2/receivers"
)

// Filter bounds. Alertmanager will happily accept an unbounded matcher list and
// then spend the request budget parsing it; oto will not send one.
const (
	// MaxFilters is the number of `filter=` matcher strings one request may carry.
	MaxFilters = 64
	// MaxFilterBytes bounds one matcher string.
	MaxFilterBytes = 4096
	// MaxSilenceIDBytes bounds a silence id before it is put in a path segment.
	MaxSilenceIDBytes = 256
)

// Config builds a Client for one AlertSource.
type Config struct {
	// BaseURL is alert_sources.base_url: absolute http(s), no trailing slash.
	BaseURL string
	// Auth is the resolved credential, or the zero value for an open endpoint.
	Auth httpc.Auth
	// TLS carries alert_sources.tls_skip_verify and any custom CA bundle.
	TLS httpc.TLSOptions
	// Timeout bounds one attempt.
	Timeout time.Duration
	// MaxResponseBytes caps a response body.
	MaxResponseBytes int64
	// Retry is the in-band retry policy; 5xx and network only.
	Retry httpc.Retry
	// UserAgent identifies oto upstream.
	UserAgent string
	// Clock is the time source. Never call time.Now in this package.
	Clock clock.Clock
	// Transport lets a test point the client at an httptest.Server.
	Transport http.RoundTripper
	// DialContext installs the SSRF guard's dialer.
	//
	// ⭐ It is the control, not the URL validation at configuration time: it sees
	// the address the socket actually connects to, so a DNS record re-pointed
	// between the check and the dial has nothing left to win.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// Sleep lets a test drive the retry backoff without real time passing.
	Sleep httpc.Sleeper
}

// Client is a read-only Alertmanager API v2 client.
//
// It is READ ONLY on purpose (R3). There is no Silence create, no Silence
// expire, and no postAlerts: oto has no write path into a cluster in v1, because
// a silence-write bug suppresses a real incident.
type Client struct {
	http *httpc.Client
	clk  clock.Clock
}

// Client implements the port exactly as SPEC §F.4 declares it.
var _ domain.AlertmanagerClient = (*Client)(nil)

// New builds a Client.
func New(cfg Config) (*Client, error) {
	clk := cfg.Clock
	if clk == nil {
		clk = clock.New()
	}
	hc, err := httpc.New(httpc.Config{
		BaseURL:          cfg.BaseURL,
		Auth:             cfg.Auth,
		TLS:              cfg.TLS,
		Timeout:          cfg.Timeout,
		MaxResponseBytes: cfg.MaxResponseBytes,
		Retry:            cfg.Retry,
		UserAgent:        cfg.UserAgent,
		ErrPrefix:        ErrPrefix,
		Clock:            clk,
		Transport:        cfg.Transport,
		DialContext:      cfg.DialContext,
		Sleep:            cfg.Sleep,
	})
	if err != nil {
		return nil, err
	}
	return &Client{http: hc, clk: clk}, nil
}

// BaseURL is the canonical Alertmanager root this client talks to.
func (c *Client) BaseURL() string { return c.http.BaseURL() }

// Status is one parsed GET /api/v2/status, with everything oto learned from it.
type Status struct {
	// AM is the port-shaped view (SPEC §F.4).
	AM domain.AMStatus
	// ClusterName is the gossip cluster name, "" when clustering is disabled.
	ClusterName string
	// Peers are the HA peers, which are also the reason ingest is at-least-once.
	Peers []Peer
	// Receivers are the receiver names declared in the running config.
	Receivers []string
	// ConfigParsed reports whether `config.original` yielded ResolveTimeout and
	// SendResolved. When false, SendResolved is nil and MUST be treated as
	// "unknown" (the NULL in source_health.send_resolved), never as false.
	ConfigParsed bool
	// ConfigError is why parsing failed, for a source_health warning.
	ConfigError string
	// Latency is how long the probe took, on oto's clock.
	Latency time.Duration
}

// Peer is one Alertmanager HA peer.
type Peer struct {
	Name    string
	Address string
}

// ServerTimeUnknown reports that the upstream advertised no usable clock, so no
// skew sample can be taken from this probe. A zero ServerTime must never be
// subtracted from oto's clock: it would report a skew of two thousand years.
func (s Status) ServerTimeUnknown() bool { return s.AM.ServerTime.IsZero() }

// Status reads GET /api/v2/status.
//
// It is how oto learns the version (which gates notification_reason handling at
// AM >= 0.32.0), the effective send_resolved per receiver (C15), and the
// upstream clock for skew measurement (C12).
func (c *Client) Status(ctx context.Context) (domain.AMStatus, error) {
	s, err := c.StatusDetail(ctx)
	return s.AM, err
}

// StatusDetail reads GET /api/v2/status and returns everything, including the
// config-parse outcome that the port's AMStatus has no room for.
func (c *Client) StatusDetail(ctx context.Context) (Status, error) {
	started := c.clk.Now()

	var w wireStatus
	resp, err := c.http.GetJSON(ctx, PathStatus, nil, &w)
	if err != nil {
		return Status{}, err
	}

	out := Status{
		ClusterName: w.Cluster.Name,
		AM: domain.AMStatus{
			Version:       strings.TrimSpace(w.VersionInfo.Version),
			Uptime:        deref(w.Uptime),
			ClusterStatus: w.Cluster.Status,
			ClusterPeers:  len(w.Cluster.Peers),
			// ServerTime comes from the HTTP Date header: the v2 status body
			// carries no "now". Date has one-second granularity, which is ample
			// for an EWMA of skew and useless for anything finer.
			ServerTime:     resp.Date,
			ResolveTimeout: DefaultResolveTimeout,
		},
		Latency: c.clk.Since(started),
	}
	for _, p := range w.Cluster.Peers {
		out.Peers = append(out.Peers, Peer(p))
	}

	// A version string that is absent AND a cluster block that is absent means
	// this is almost certainly not an Alertmanager. Say so in the taxonomy
	// rather than returning a hollow success that a probe will report as green.
	if out.AM.Version == "" && w.Cluster.Status == "" && w.Config.Original == "" {
		return out, c.http.Errorf(httpc.CodeMalformedResponse,
			errs.New(errs.KindUpstreamDown, "", "no versionInfo, cluster or config in the status response"))
	}

	cfg, cfgErr := parseConfig(w.Config.Original)
	if cfgErr != nil {
		out.ConfigError = cfgErr.Error()
		return out, nil
	}
	out.ConfigParsed = true
	out.Receivers = cfg.Receivers
	out.AM.ResolveTimeout = cfg.ResolveTimeout
	out.AM.SendResolved = cfg.SendResolved
	return out, nil
}

// Alerts reads GET /api/v2/alerts.
//
// THIS IS THE ONLY WAY TO OBSERVE SUPPRESSION (C1). Alertmanager's MuteStage
// drops silenced and inhibited alerts before the notification pipeline reaches
// any webhook, so `status.silencedBy` / `inhibitedBy` / `mutedBy` exist nowhere
// on the push path. A webhook-only oto could never render silence state.
func (c *Client) Alerts(ctx context.Context, f domain.AlertFilter) ([]domain.GettableAlert, error) {
	q, err := c.alertQuery(f)
	if err != nil {
		return nil, err
	}

	var w []wireAlert
	if _, err := c.http.GetJSON(ctx, PathAlerts, q, &w); err != nil {
		return nil, err
	}
	out := make([]domain.GettableAlert, 0, len(w))
	for _, a := range w {
		out = append(out, a.toAlert())
	}
	return out, nil
}

// AlertGroupFilter selects which alert groups to fetch. Note the fourth boolean:
// GET /api/v2/alerts/groups takes `muted`, which GET /api/v2/alerts does not.
type AlertGroupFilter struct {
	Active, Silenced, Inhibited, Muted bool
	Filter                             []string
	Receiver                           string
}

// AlertGroup is one Alertmanager notification group as the v2 API returns it.
//
// It is NOT oto's AlertGroup. Alertmanager's grouping is derived from the route
// tree and changes on every alertmanager.yml reload, which is exactly why C3
// makes oto's own group_key = H(org, source, receiver, sorted groupLabels) the
// durable identity and keeps AM's groupKey only as an observability hint.
type AlertGroup struct {
	// Labels are the group labels — the durable part.
	Labels map[string]string
	// RouteLabels come from the matched route. They are first-class in the v2
	// model and absent from the published webhook docs.
	RouteLabels map[string]string
	// Receiver is the receiver name this group routed to.
	Receiver string
	// Alerts are the group's members.
	Alerts []domain.GettableAlert
}

// AlertGroups reads GET /api/v2/alerts/groups.
func (c *Client) AlertGroups(ctx context.Context, f AlertGroupFilter) ([]AlertGroup, error) {
	q := url.Values{}
	// All four default to TRUE upstream. An all-false filter is the zero value,
	// not a request for nothing, so it means "the API default" (see alertQuery).
	if f.Active || f.Silenced || f.Inhibited || f.Muted {
		q.Set("active", strconv.FormatBool(f.Active))
		q.Set("silenced", strconv.FormatBool(f.Silenced))
		q.Set("inhibited", strconv.FormatBool(f.Inhibited))
		q.Set("muted", strconv.FormatBool(f.Muted))
	}
	if err := addFilters(q, f.Filter); err != nil {
		return nil, err
	}
	if f.Receiver != "" {
		q.Set("receiver", f.Receiver)
	}

	var w []wireAlertGroup
	if _, err := c.http.GetJSON(ctx, PathAlertGroups, q, &w); err != nil {
		return nil, err
	}
	out := make([]AlertGroup, 0, len(w))
	for _, g := range w {
		grp := AlertGroup{
			Labels:      copyMap(g.Labels),
			RouteLabels: copyMap(g.RouteLabels),
			Receiver:    g.Receiver.Name,
			Alerts:      make([]domain.GettableAlert, 0, len(g.Alerts)),
		}
		for _, a := range g.Alerts {
			grp.Alerts = append(grp.Alerts, a.toAlert())
		}
		out = append(out, grp)
	}
	return out, nil
}

// Silences reads GET /api/v2/silences (PLURAL).
//
// READ ONLY (R3). oto mirrors silences so the UI can explain why an alert went
// quiet; the "Silence" affordance in Slack is a deep link into the Alertmanager
// UI, not an API call.
func (c *Client) Silences(ctx context.Context, f domain.SilenceFilter) ([]domain.GettableSilence, error) {
	q := url.Values{}
	// active / expired / pending all default TRUE upstream, and are a newer
	// addition than `filter` itself: an older Alertmanager ignores them rather
	// than failing, which is the failure mode oto can live with.
	if f.Active || f.Expired || f.Pending {
		q.Set("active", strconv.FormatBool(f.Active))
		q.Set("expired", strconv.FormatBool(f.Expired))
		q.Set("pending", strconv.FormatBool(f.Pending))
	}
	if err := addFilters(q, f.Filter); err != nil {
		return nil, err
	}

	var w []wireSilence
	if _, err := c.http.GetJSON(ctx, PathSilences, q, &w); err != nil {
		return nil, err
	}
	out := make([]domain.GettableSilence, 0, len(w))
	for _, s := range w {
		out = append(out, s.toSilence())
	}
	return out, nil
}

// Silence reads GET /api/v2/silence/{id} (SINGULAR).
//
// A silence that has been "deleted" upstream is still returned here with
// state="expired": DELETE /silence/{id} expires rather than deletes. An unknown
// id is a genuine 404 and surfaces as errs.KindNotFound.
func (c *Client) Silence(ctx context.Context, id string) (domain.GettableSilence, error) {
	id = strings.TrimSpace(id)
	switch {
	case id == "":
		return domain.GettableSilence{}, errs.New(errs.KindValidation,
			ErrPrefix+"_invalid_silence_id", "a silence id is required")
	case len(id) > MaxSilenceIDBytes:
		return domain.GettableSilence{}, errs.Newf(errs.KindValidation,
			ErrPrefix+"_invalid_silence_id", "a silence id must be at most %d bytes", MaxSilenceIDBytes)
	}

	var w wireSilence
	if _, err := c.http.GetJSON(ctx, PathSilence+url.PathEscape(id), nil, &w); err != nil {
		return domain.GettableSilence{}, err
	}
	return w.toSilence(), nil
}

// Receivers reads GET /api/v2/receivers, the cheap cross-check against the
// receiver names parsed out of the status config.
func (c *Client) Receivers(ctx context.Context) ([]string, error) {
	var w []wireReceiver
	if _, err := c.http.GetJSON(ctx, PathReceivers, nil, &w); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(w))
	for _, r := range w {
		out = append(out, r.Name)
	}
	return out, nil
}

// alertQuery builds the query string for GET /api/v2/alerts.
func (c *Client) alertQuery(f domain.AlertFilter) (url.Values, error) {
	q := url.Values{}
	// All four booleans default to TRUE in the Alertmanager API, so the Go zero
	// value of AlertFilter would, sent literally, ask for NOTHING. An all-false
	// filter therefore means "the API default": the alternative is a reconciler
	// that silently returns an empty world and expires every open occurrence.
	if f.Active || f.Silenced || f.Inhibited || f.Unprocessed {
		q.Set("active", strconv.FormatBool(f.Active))
		q.Set("silenced", strconv.FormatBool(f.Silenced))
		q.Set("inhibited", strconv.FormatBool(f.Inhibited))
		q.Set("unprocessed", strconv.FormatBool(f.Unprocessed))
	}
	if err := addFilters(q, f.Filter); err != nil {
		return nil, err
	}
	if f.Receiver != "" {
		q.Set("receiver", f.Receiver)
	}
	return q, nil
}

// addFilters appends the repeatable `filter=` matcher strings, bounded.
func addFilters(q url.Values, filters []string) error {
	if len(filters) > MaxFilters {
		return errs.Newf(errs.KindValidation, ErrPrefix+"_too_many_filters",
			"at most %d filters may be sent in one request", MaxFilters)
	}
	for _, f := range filters {
		if len(f) > MaxFilterBytes {
			return errs.Newf(errs.KindValidation, ErrPrefix+"_filter_too_large",
				"a filter must be at most %d bytes", MaxFilterBytes)
		}
		if f != "" {
			q.Add("filter", f)
		}
	}
	return nil
}
