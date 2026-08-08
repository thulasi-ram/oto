package service

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/sources/client/alertmanager"
	"github.com/thulasiram/oto/internal/sources/domain"
	"github.com/thulasiram/oto/internal/sources/rulematch"
)

// Error codes this service mints itself, on top of the namespaced transport
// codes that alertmanager/prometheus produce.
const (
	// CodeSourceNotFound means no such source in the caller's org.
	CodeSourceNotFound = "sources_not_found"
	// CodeSourceDeleted means the source is soft deleted.
	CodeSourceDeleted = "sources_deleted"
	// CodeCredentialFailed means the sealed credential could not be unsealed.
	CodeCredentialFailed = "sources_credential_unavailable"
	// CodeClientFailed means the source's configuration cannot produce a client
	// — a base URL that no longer normalises, an unusable CA bundle.
	CodeClientFailed = "sources_client_unavailable"
	// CodeNoPrometheus means prometheus_url is empty, so rule definitions are
	// limited to what generatorURL carries.
	CodeNoPrometheus = "sources_prometheus_not_configured"
)

// Options are the Service's dependencies. Everything is a port, so the whole
// service is exercisable with fakes and an httptest.Server.
type Options struct {
	Repo    SourceRepository
	Creds   CredentialStore
	Clients ClientFactory
	Clock   clock.Clock
	Logger  *slog.Logger
}

// Service composes the outbound clients with the source registry.
//
// It is the read side of the sources module: probe a source, report its health,
// and serve the Alertmanager and Prometheus reads that the reconciler and the
// enrichment pipeline call. There is no write path into a cluster (R3), so there
// is no method here that mutates anything upstream.
type Service struct {
	repo    SourceRepository
	creds   CredentialStore
	clients ClientFactory
	clk     clock.Clock
	log     *slog.Logger
}

// New builds the Service.
func New(o Options) (*Service, error) {
	switch {
	case o.Repo == nil:
		return nil, errs.New(errs.KindInternal, "sources_missing_repository", "a source repository is required")
	case o.Clients == nil:
		return nil, errs.New(errs.KindInternal, "sources_missing_client_factory", "a client factory is required")
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.New()
	}
	lg := o.Logger
	if lg == nil {
		lg = slog.Default()
	}
	return &Service{repo: o.Repo, creds: o.Creds, clients: o.Clients, clk: clk, log: lg}, nil
}

// Get returns one source.
func (s *Service) Get(ctx context.Context, scope db.TenantScope, id uuid.UUID) (domain.Source, error) {
	return s.source(ctx, scope, id)
}

// OrgForSource resolves the tenant that owns a source, for a worker whose job
// payload names a source and no org (§G.3).
//
// It is the only method here that BUILDS a scope rather than being handed one,
// and it is deliberately narrow: one id in, one scope out, and every subsequent
// call the worker makes is bound by it.
func (s *Service) OrgForSource(ctx context.Context, sourceID uuid.UUID) (db.TenantScope, error) {
	orgID, err := s.repo.ResolveOrg(ctx, sourceID)
	if err != nil {
		return db.TenantScope{}, err
	}
	scope, err := db.NewTenantScope(orgID)
	if err != nil {
		return db.TenantScope{}, errs.Wrap(err, errs.KindInternal, CodeSourceNotFound,
			"this source names an org that does not exist")
	}
	return scope, nil
}

// List returns a keyset page of sources.
func (s *Service) List(ctx context.Context, scope db.TenantScope, f domain.SourceFilter, p db.Keyset) ([]domain.Source, db.Cursor, error) {
	return s.repo.List(ctx, scope, f, p)
}

// Health returns the liveness projection for one source.
//
// Anything other than healthy BLOCKS the reaper (SPEC §B.4). Callers must read
// this before expiring anything: losing sight of an alert is not the alert
// resolving, and a dead Alertmanager looks exactly like a quiet one.
func (s *Service) Health(ctx context.Context, scope db.TenantScope, id uuid.UUID) (domain.SourceHealth, error) {
	if _, err := s.source(ctx, scope, id); err != nil {
		return domain.SourceHealth{}, err
	}
	return s.repo.GetHealth(ctx, scope, id)
}

// Alerts reads the source's current world from GET /api/v2/alerts.
//
// This is the ONLY way oto can observe suppression (C1). MuteStage drops
// silenced and inhibited alerts before any webhook sees them, so `suppressed` is
// producible here and nowhere else.
func (s *Service) Alerts(ctx context.Context, scope db.TenantScope, id uuid.UUID, f domain.AlertFilter) ([]domain.GettableAlert, error) {
	c, err := s.alertmanager(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	return c.Alerts(ctx, f)
}

// AlertGroups reads GET /api/v2/alerts/groups.
//
// The returned groups are Alertmanager's, not oto's. AM's groupKey embeds the
// route path and changes on every config reload, so it is an observability hint
// and MUST NOT be parsed or used as a key (C3).
func (s *Service) AlertGroups(ctx context.Context, scope db.TenantScope, id uuid.UUID, f alertmanager.AlertGroupFilter) ([]alertmanager.AlertGroup, error) {
	c, err := s.alertmanager(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	return c.AlertGroups(ctx, f)
}

// Silences reads the source's silences. READ ONLY (R3).
func (s *Service) Silences(ctx context.Context, scope db.TenantScope, id uuid.UUID, f domain.SilenceFilter) ([]domain.GettableSilence, error) {
	c, err := s.alertmanager(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	return c.Silences(ctx, f)
}

// Silence reads one silence by id.
//
// A silence "deleted" through the Alertmanager UI is still here with
// state="expired": DELETE /silence/{id} expires rather than deletes. Only an
// unknown id is a 404.
func (s *Service) Silence(ctx context.Context, scope db.TenantScope, id uuid.UUID, silenceID string) (domain.GettableSilence, error) {
	c, err := s.alertmanager(ctx, scope, id)
	if err != nil {
		return domain.GettableSilence{}, err
	}
	return c.Silence(ctx, silenceID)
}

// Status reads GET /api/v2/status without touching source_health. Probe is what
// the sources screen calls; this is for a caller that wants the raw answer.
func (s *Service) Status(ctx context.Context, scope db.TenantScope, id uuid.UUID) (alertmanager.Status, error) {
	c, err := s.alertmanager(ctx, scope, id)
	if err != nil {
		return alertmanager.Status{}, err
	}
	return c.StatusDetail(ctx)
}

// Rules fetches alerting-rule definitions from the source's Prometheus.
func (s *Service) Rules(ctx context.Context, scope db.TenantScope, id uuid.UUID, names []string) ([]domain.RuleGroup, error) {
	src, err := s.source(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if !src.HasPrometheus() {
		return nil, errs.New(errs.KindValidation, CodeNoPrometheus,
			"this source has no Prometheus URL, so rule definitions cannot be fetched")
	}
	c, err := s.prometheus(ctx, scope, src, "")
	if err != nil {
		return nil, err
	}
	return c.Rules(ctx, names)
}

// RuleQuery asks for one alert's originating rule.
type RuleQuery struct {
	// Labels are the alert's rendered labels; alertname is required.
	Labels map[string]string
	// Annotations are the alert's rendered annotations.
	Annotations map[string]string
	// GeneratorURL is the primary strategy's input.
	GeneratorURL string
	// SkipPrometheus forces the zero-API-call path. The enrichment pipeline sets
	// it when its budget is spent.
	SkipPrometheus bool
	// FollowGeneratorURL lets the lookup query the Prometheus named by the
	// alert's own generatorURL rather than the source's configured one. In a
	// federated setup that is the difference between finding the rule and
	// finding nothing (research A7 pitfall 5).
	FollowGeneratorURL bool
}

// ResolveRule recovers the originating rule for one alert.
//
// It applies the SPEC's preference order: decode g0.expr from generatorURL
// first (zero API calls, unambiguous), then enrich with /api/v1/rules for `for`,
// `keep_firing_for` and the raw labels and annotations.
//
// A Prometheus failure NEVER fails this call. Rule enrichment is best-effort by
// design — the alert is already real, and degrading to the generatorURL
// expression with a recorded note is strictly better than losing the snapshot.
// The returned Match carries Origin, Strategy, Confidence and CandidateCount for
// the caller to persist verbatim onto rule_snapshots.
func (s *Service) ResolveRule(ctx context.Context, scope db.TenantScope, id uuid.UUID, q RuleQuery) (rulematch.Match, error) {
	src, err := s.source(ctx, scope, id)
	if err != nil {
		return rulematch.Match{}, err
	}

	in := rulematch.Input{
		Alert: rulematch.Alert{
			Labels:       q.Labels,
			Annotations:  q.Annotations,
			GeneratorURL: q.GeneratorURL,
		},
	}

	if !q.SkipPrometheus {
		override := ""
		if q.FollowGeneratorURL {
			if gen, gerr := rulematch.ParseGeneratorURL(q.GeneratorURL); gerr == nil {
				override = gen.ExternalURL
			}
		}
		if src.HasPrometheus() || override != "" {
			groups, purl, ferr := s.fetchRules(ctx, scope, src, override, q.Labels[rulematch.LabelAlertName])
			if ferr != nil {
				s.log.WarnContext(ctx, "sources: rule lookup degraded to generatorURL",
					"source_id", src.ID, "code", errs.CodeOf(ferr), "error", ferr)
			} else {
				in.Groups = groups
				in.PrometheusURL = purl
			}
		}
	}

	m := rulematch.Resolve(in)
	if err := m.Validate(); err != nil {
		// An invalid Match would become a CHECK violation and therefore a 500.
		// Degrade to "unavailable" instead: no snapshot is better than a bad one.
		s.log.ErrorContext(ctx, "sources: rule match failed its own invariants",
			"source_id", src.ID, "error", err)
		return rulematch.Match{
			Origin:     rulematch.OriginUnavailable,
			Strategy:   rulematch.StrategyNone,
			Confidence: rulematch.ConfidenceNone,
			Notes:      m.Notes,
		}, nil
	}
	return m, nil
}

// fetchRules pulls the candidate rule groups for one alertname.
func (s *Service) fetchRules(ctx context.Context, scope db.TenantScope, src domain.Source, override, alertName string) ([]domain.RuleGroup, string, error) {
	c, err := s.prometheus(ctx, scope, src, override)
	if err != nil {
		return nil, "", err
	}
	var names []string
	if alertName != "" {
		names = []string{alertName}
	}
	groups, err := c.Rules(ctx, names)
	if err != nil {
		return nil, "", err
	}
	url := override
	if url == "" {
		url = src.PrometheusURL
	}
	return groups, url, nil
}

// source loads and guards one source.
func (s *Service) source(ctx context.Context, scope db.TenantScope, id uuid.UUID) (domain.Source, error) {
	src, err := s.repo.Get(ctx, scope, id)
	if err != nil {
		return domain.Source{}, err
	}
	if src.Deleted() {
		return domain.Source{}, errs.New(errs.KindNotFound, CodeSourceDeleted, "this source has been deleted")
	}
	return src, nil
}

// alertmanager builds a client for one source.
func (s *Service) alertmanager(ctx context.Context, scope db.TenantScope, id uuid.UUID) (AlertmanagerClient, error) {
	src, err := s.source(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	cred, err := s.credential(ctx, scope, src)
	if err != nil {
		return nil, err
	}
	c, err := s.clients.Alertmanager(src, cred)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindValidation, CodeClientFailed,
			"this source's connection settings are not usable")
	}
	return c, nil
}

// prometheus builds a Prometheus client for one source.
func (s *Service) prometheus(ctx context.Context, scope db.TenantScope, src domain.Source, override string) (PrometheusClient, error) {
	cred, err := s.credential(ctx, scope, src)
	if err != nil {
		return nil, err
	}
	c, err := s.clients.Prometheus(src, cred, override)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindValidation, CodeClientFailed,
			"this source's Prometheus settings are not usable")
	}
	return c, nil
}

// credential unseals the source's outbound credential, if it has one.
func (s *Service) credential(ctx context.Context, scope db.TenantScope, src domain.Source) (domain.Credential, error) {
	if src.AuthCredentialID == nil || s.creds == nil {
		return domain.Credential{Kind: domain.AuthNone}, nil
	}
	cred, err := s.creds.Resolve(ctx, scope, *src.AuthCredentialID)
	if err != nil {
		return domain.Credential{}, errs.Wrap(err, errs.KindInternal, CodeCredentialFailed,
			"this source's stored credential could not be read")
	}
	return cred, nil
}
