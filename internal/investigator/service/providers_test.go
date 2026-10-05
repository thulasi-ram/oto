package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/modelfake"
)

// These are the service's own rules, against in-memory ports. The SQL — the sealed
// row, the tenant predicate, the CHECKs — is `repository/providers_db_test.go`'s.

type memStore struct {
	rows map[uuid.UUID]domain.ProviderConfig
	fail error
}

func (m *memStore) Insert(_ context.Context, s db.TenantScope, d domain.ProviderDraft, cred uuid.UUID, at time.Time) (domain.ProviderConfig, error) {
	if m.fail != nil {
		return domain.ProviderConfig{}, m.fail
	}
	c := domain.ProviderConfig{ID: uuid.New(), OrgID: s.OrgID(), Name: d.Name, BaseURL: d.BaseURL,
		Model: d.Model, CredentialID: cred, CreatedAt: at, UpdatedAt: at}
	m.rows[c.ID] = c
	return c, nil
}

func (m *memStore) Get(_ context.Context, _ db.TenantScope, id uuid.UUID) (domain.ProviderConfig, error) {
	c, ok := m.rows[id]
	if !ok {
		return domain.ProviderConfig{}, errs.NotFound("model_provider_not_found", "no such model endpoint")
	}
	return c, nil
}

func (m *memStore) List(context.Context, db.TenantScope) ([]domain.ProviderConfig, error) {
	out := []domain.ProviderConfig{}
	for _, c := range m.rows {
		out = append(out, c)
	}
	return out, nil
}

type memCreds struct {
	sealed map[uuid.UUID]map[string]string
	kinds  map[uuid.UUID]string
}

func (m *memCreds) CreateCredential(_ context.Context, _ db.TenantScope, kind string, values map[string]string) (uuid.UUID, error) {
	id := uuid.New()
	m.sealed[id], m.kinds[id] = values, kind
	return id, nil
}

func (m *memCreds) ResolveKey(_ context.Context, _ db.TenantScope, id uuid.UUID) (string, error) {
	return m.sealed[id][domain.CredentialValueKey], nil
}

func (m *memCreds) ResolveToolServerToken(_ context.Context, _ db.TenantScope, id uuid.UUID) (string, error) {
	if m.kinds[id] != domain.ToolServerCredentialKind {
		return "", errs.New(errs.KindInternal, "tool_server_credential_kind", "wrong kind")
	}
	return m.sealed[id][domain.ToolServerCredentialValueKey], nil
}

// memTx runs fn and records that it did; a failed fn is "rolled back" by the test
// asserting nothing was left behind in the stores.
type memTx struct{ ran int }

func (m *memTx) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	m.ran++
	return fn(ctx)
}

// recordingDialer hands back a scripted model for every Dial, and keeps the last one
// so a test can read what the run asked it.
type recordingDialer struct {
	mu     sync.Mutex
	gotKey string
	skew   bool
	script []modelfake.Step
	last   *modelfake.Provider
	// byModel scripts an endpoint by its model name instead — the risk model's, which is
	// dialled beside the Investigation's — and dialled keeps each one it built.
	byModel map[string][]modelfake.Step
	dialled map[string][]*modelfake.Provider
}

func (d *recordingDialer) Dial(cfg domain.ProviderConfig, key string) (domain.ModelProvider, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := cfg.Identity()
	if steps, ok := d.byModel[cfg.Model]; ok {
		p := modelfake.NewWithIdentity(id, steps...)
		if d.dialled == nil {
			d.dialled = map[string][]*modelfake.Provider{}
		}
		d.dialled[cfg.Model] = append(d.dialled[cfg.Model], p)
		return p, nil
	}
	d.gotKey = key
	if d.skew {
		id.Endpoint += "/"
	}
	d.last = modelfake.NewWithIdentity(id, d.script...)
	return d.last, nil
}

func (d *recordingDialer) model() *modelfake.Provider {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.last
}

type rig struct {
	svc            *Service
	store          *memStore
	creds          *memCreds
	tx             *memTx
	dial           *recordingDialer
	scope          db.TenantScope
	clock          *clock.Fake
	investigators  *memInvestigators
	investigations *memInvestigations
	history        *memHistory
	incidents      *memIncidents
	digests        *memDigests
	declarer       *memDeclarer
	findings       *memFindings
	orgControls    *memControls
	classes        *memClasses
	queue          *memQueue
	toolServers    *memToolServers
	toolDialer     *switchDialer
	redaction      *memRedaction
	suggestions    *memSuggestions
	policies       *memPolicies
	memberships    *memMemberships
	approvers      *memApprovers
	remedies       *memRemedies
	remedyFacts    *memRemedyDeclarer
	remedyRisk     *memRemedyRisk
}

func (r *rig) deps() Deps {
	return Deps{Providers: r.store, Credentials: r.creds, Keys: r.creds, Dialer: r.dial, Tx: r.tx, Clock: r.clock,
		Investigators: r.investigators, Investigations: r.investigations,
		Cases: r.history, Incidents: r.incidents, Digests: r.digests, Declarer: r.declarer,
		Timeline: r.history, Rules: r.history, Findings: r.findings,
		OrgControls: r.orgControls, Classes: r.classes, Queue: r.queue,
		Limits:      Limits{ToolTimeout: 50 * time.Millisecond, MaxToolResult: 4096},
		ToolServers: r.toolServers, Tokens: r.creds, ToolDialer: r.toolDialer, Redaction: r.redaction,
		Suggestions: r.suggestions, Policies: r.policies, Memberships: r.memberships, Approvers: r.approvers,
		Remedies: r.remedies, RemedyDeclarer: r.remedyFacts, RemedyRisk: r.remedyRisk}
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{
		store:          &memStore{rows: map[uuid.UUID]domain.ProviderConfig{}},
		creds:          &memCreds{sealed: map[uuid.UUID]map[string]string{}, kinds: map[uuid.UUID]string{}},
		tx:             &memTx{},
		dial:           &recordingDialer{},
		clock:          clock.NewFake(time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)),
		investigators:  newMemInvestigators(),
		investigations: newMemInvestigations(),
		history:        &memHistory{cases: map[uuid.UUID]domain.CaseSubject{}},
		incidents:      newMemIncidents(),
		digests:        &memDigests{cases: map[uuid.UUID][]domain.DigestCase{}},
		declarer:       &memDeclarer{},
		findings:       &memFindings{},
		orgControls:    &memControls{on: true, dailyTokens: 2_000_000, concurrency: 2},
		classes:        &memClasses{},
		queue:          &memQueue{},
		toolServers:    newMemToolServers(),
		toolDialer:     &switchDialer{},
		redaction:      &memRedaction{},
		suggestions:    &memSuggestions{},
		policies:       &memPolicies{},
		memberships:    &memMemberships{},
		approvers:      &memApprovers{},
		remedies:       newMemRemedies(),
		remedyFacts:    &memRemedyDeclarer{},
		remedyRisk:     &memRemedyRisk{},
	}
	scope, err := db.NewTenantScope(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	r.scope = scope
	svc, err := New(r.deps())
	if err != nil {
		t.Fatal(err)
	}
	r.svc = svc
	return r
}

func TestCreateSealsTheKeyAsAModelKeyAndReturnsOnlyItsID(t *testing.T) {
	r := newRig(t)
	cfg, err := r.svc.CreateProvider(context.Background(), r.scope, domain.ProviderDraft{
		Name: "gateway", BaseURL: "https://GW.test/v1/", Model: "m", APIKey: "sk-live-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasKey() || r.creds.kinds[cfg.CredentialID] != domain.CredentialKind ||
		r.creds.sealed[cfg.CredentialID][domain.CredentialValueKey] != "sk-live-1" {
		t.Fatalf("key not sealed as a model key: %+v", r.creds)
	}
	if cfg.BaseURL != "https://gw.test/v1" || cfg.CreatedAt.IsZero() || r.tx.ran != 1 {
		t.Fatalf("cfg = %+v, tx ran %d", cfg, r.tx.ran)
	}
	// ⛔ The config is safe to print whole.
	if s := strings.Join([]string{cfg.Name, cfg.BaseURL, cfg.Model, cfg.CredentialID.String()}, " "); strings.Contains(s, "sk-live") {
		t.Fatal("the key reached the config")
	}
}

func TestAKeylessEndpointSealsNothing(t *testing.T) {
	r := newRig(t)
	cfg, err := r.svc.CreateProvider(context.Background(), r.scope, domain.ProviderDraft{
		Name: "local", BaseURL: "http://vllm.models.svc:8000/v1", Model: "m",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasKey() || len(r.creds.sealed) != 0 {
		t.Fatalf("a keyless endpoint sealed something: %+v", r.creds.sealed)
	}
}

func TestAnInvalidDraftTouchesNothing(t *testing.T) {
	r := newRig(t)
	_, err := r.svc.CreateProvider(context.Background(), r.scope, domain.ProviderDraft{
		Name: "plain", BaseURL: "http://litellm:4000", Model: "m", APIKey: "sk",
	})
	if !errs.IsKind(err, errs.KindValidation) {
		t.Fatalf("err = %v, want validation", err)
	}
	if r.tx.ran != 0 || len(r.creds.sealed) != 0 {
		t.Fatal("an invalid draft reached the unit of work")
	}
}

func TestAStorageFailureIsReturned(t *testing.T) {
	r := newRig(t)
	r.store.fail = errs.Conflict("model_providers_org_name_uniq", "taken")
	_, err := r.svc.CreateProvider(context.Background(), r.scope, domain.ProviderDraft{
		Name: "gateway", BaseURL: "https://gw.test", Model: "m", APIKey: "sk",
	})
	if !errs.IsKind(err, errs.KindConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenUnsealsStraightIntoTheAdapterAndPinsTheStoredIdentity(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	cfg, err := r.svc.CreateProvider(ctx, r.scope, domain.ProviderDraft{
		Name: "gateway", BaseURL: "https://gw.test/v1", Model: "m", APIKey: "sk-live-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := r.svc.OpenProvider(ctx, r.scope, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.dial.gotKey != "sk-live-2" || p.Identity() != cfg.Identity() {
		t.Fatalf("dialled with %q, identity %+v", r.dial.gotKey, p.Identity())
	}

	r.dial.skew = true
	if _, err := r.svc.OpenProvider(ctx, r.scope, cfg.ID); errs.CodeOf(err) != "model_identity_mismatch" {
		t.Fatalf("err = %v, want model_identity_mismatch", err)
	}

	if _, err := r.svc.OpenProvider(ctx, r.scope, uuid.New()); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestNewRequiresEveryPort(t *testing.T) {
	r := newRig(t)
	for name, drop := range map[string]func(*Deps){
		"providers":      func(d *Deps) { d.Providers = nil },
		"creds":          func(d *Deps) { d.Credentials = nil },
		"keys":           func(d *Deps) { d.Keys = nil },
		"dialer":         func(d *Deps) { d.Dialer = nil },
		"tx":             func(d *Deps) { d.Tx = nil },
		"investigators":  func(d *Deps) { d.Investigators = nil },
		"investigations": func(d *Deps) { d.Investigations = nil },
		"cases":          func(d *Deps) { d.Cases = nil },
		"digests":        func(d *Deps) { d.Digests = nil },
		"timeline":       func(d *Deps) { d.Timeline = nil },
		"rules":          func(d *Deps) { d.Rules = nil },
		"findings":       func(d *Deps) { d.Findings = nil },
		"org controls":   func(d *Deps) { d.OrgControls = nil },
		"classes":        func(d *Deps) { d.Classes = nil },
		"queue":          func(d *Deps) { d.Queue = nil },
		"tool servers":   func(d *Deps) { d.ToolServers = nil },
		"tokens":         func(d *Deps) { d.Tokens = nil },
		"tool dialer":    func(d *Deps) { d.ToolDialer = nil },
		"redaction":      func(d *Deps) { d.Redaction = nil },
		"remedy risk":    func(d *Deps) { d.RemedyRisk = nil },
	} {
		d := r.deps()
		drop(&d)
		if _, err := New(d); err == nil {
			t.Fatalf("missing %s accepted", name)
		}
	}
	if _, err := New(r.deps()); err != nil {
		t.Fatal(err)
	}
}
