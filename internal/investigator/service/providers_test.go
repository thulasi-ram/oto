package service

import (
	"context"
	"strings"
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

// memTx runs fn and records that it did; a failed fn is "rolled back" by the test
// asserting nothing was left behind in the stores.
type memTx struct{ ran int }

func (m *memTx) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	m.ran++
	return fn(ctx)
}

type recordingDialer struct {
	gotKey string
	skew   bool
}

func (d *recordingDialer) Dial(cfg domain.ProviderConfig, key string) (domain.ModelProvider, error) {
	d.gotKey = key
	id := cfg.Identity()
	if d.skew {
		id.Endpoint += "/"
	}
	return modelfake.NewWithIdentity(id), nil
}

type rig struct {
	svc   *Service
	store *memStore
	creds *memCreds
	tx    *memTx
	dial  *recordingDialer
	scope db.TenantScope
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{
		store: &memStore{rows: map[uuid.UUID]domain.ProviderConfig{}},
		creds: &memCreds{sealed: map[uuid.UUID]map[string]string{}, kinds: map[uuid.UUID]string{}},
		tx:    &memTx{},
		dial:  &recordingDialer{},
	}
	scope, err := db.NewTenantScope(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	r.scope = scope
	svc, err := New(Deps{Providers: r.store, Credentials: r.creds, Keys: r.creds, Dialer: r.dial, Tx: r.tx,
		Clock: clock.NewFake(time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))})
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
	full := Deps{Providers: r.store, Credentials: r.creds, Keys: r.creds, Dialer: r.dial, Tx: r.tx}
	for name, d := range map[string]Deps{
		"providers": {Credentials: full.Credentials, Keys: full.Keys, Dialer: full.Dialer, Tx: full.Tx},
		"creds":     {Providers: full.Providers, Keys: full.Keys, Dialer: full.Dialer, Tx: full.Tx},
		"keys":      {Providers: full.Providers, Credentials: full.Credentials, Dialer: full.Dialer, Tx: full.Tx},
		"dialer":    {Providers: full.Providers, Credentials: full.Credentials, Keys: full.Keys, Tx: full.Tx},
		"tx":        {Providers: full.Providers, Credentials: full.Credentials, Keys: full.Keys, Dialer: full.Dialer},
	} {
		if _, err := New(d); err == nil {
			t.Fatalf("missing %s accepted", name)
		}
	}
	if _, err := New(full); err != nil {
		t.Fatal(err)
	}

}
