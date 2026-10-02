package repository_test

// MODEL ENDPOINTS AGAINST A REAL POSTGRES (migration 00091, git-bug 8f1f071). Every
// claim here is a claim about SQL as much as Go: the key sealed in `channel_credentials`
// and unsealed only as a `model_api_key`, the tenant predicate, the per-org name index,
// and the CHECK that a key never rides plaintext.

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	channelrepo "github.com/thulasiram/oto/internal/channels/repository"
	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/investigator/repository"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/secrets"
	"github.com/thulasiram/oto/test/harness"
)

func keyring(t *testing.T) *secrets.Keyring {
	t.Helper()
	k, err := secrets.NewKeyring(map[int][]byte{1: bytes.Repeat([]byte{7}, secrets.KeyBytes)}, 1)
	require.NoError(t, err)
	return k
}

func draft(name, base string) domain.ProviderDraft {
	d, err := domain.ProviderDraft{Name: name, BaseURL: base, Model: "gateway-model"}.Normalize()
	if err != nil {
		panic(err)
	}
	return d
}

func TestAModelKeyIsSealedAndOnlyOpensAsAModelKey(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org := h.Org()
	ring := keyring(t)
	creds := channelrepo.NewCredentialRepository(h.Pool, ring, ring, h.Clock)
	providers := repository.NewProviderRepository(h.Pool)
	keys := repository.NewKeyStore(h.Pool, ring)

	credID, err := creds.CreateCredential(h.Ctx, org.Scope, domain.CredentialKind,
		map[string]string{domain.CredentialValueKey: "sk-live-sealed"})
	require.NoError(t, err)
	p, err := providers.Insert(h.Ctx, org.Scope, draft("gateway", "https://gw.example.test/v1"), credID, h.Now())
	require.NoError(t, err)
	require.Equal(t, credID, p.CredentialID)
	require.Equal(t, h.Now(), p.CreatedAt, "created_at comes from the injected clock")
	require.Equal(t, p.CreatedAt, p.UpdatedAt)

	key, err := keys.ResolveKey(h.Ctx, org.Scope, p.CredentialID)
	require.NoError(t, err)
	require.Equal(t, "sk-live-sealed", key)

	// ⛔ A Slack token behind the slot is refused by kind, before any unseal.
	slack, err := creds.CreateCredential(h.Ctx, org.Scope, "slack_bot_token", map[string]string{"token": "xoxb-1"})
	require.NoError(t, err)
	_, err = keys.ResolveKey(h.Ctx, org.Scope, slack)
	require.Equal(t, "model_credential_kind", errs.CodeOf(err))

	// Another org's key is the same answer as one that never existed.
	_, err = keys.ResolveKey(h.Ctx, h.Org().Scope, p.CredentialID)
	require.True(t, errs.IsKind(err, errs.KindNotFound), "got %v", err)
}

func TestModelEndpointsAreReadPerOrgAndNamedOncePerOrg(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org, other := h.Org(), h.Org()
	providers := repository.NewProviderRepository(h.Pool)

	b, err := providers.Insert(h.Ctx, org.Scope, draft("b-local", "http://vllm.models.svc:8000/v1"), uuid.Nil, h.Now())
	require.NoError(t, err)
	require.False(t, b.HasKey())
	_, err = providers.Insert(h.Ctx, org.Scope, draft("a-gateway", "https://gw.example.test/v1"), uuid.Nil, h.Now())
	require.NoError(t, err)

	list, err := providers.List(h.Ctx, org.Scope)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "a-gateway", list[0].Name, "listed by name")

	got, err := providers.Get(h.Ctx, org.Scope, b.ID)
	require.NoError(t, err)
	require.Equal(t, b.Identity(), got.Identity())

	_, err = providers.Get(h.Ctx, other.Scope, b.ID)
	require.True(t, errs.IsKind(err, errs.KindNotFound), "another org's endpoint must 404, got %v", err)
	otherList, err := providers.List(h.Ctx, other.Scope)
	require.NoError(t, err)
	require.Empty(t, otherList)

	// The name is unique within an org, and only within one.
	_, err = providers.Insert(h.Ctx, org.Scope, draft("b-local", "https://elsewhere.test"), uuid.Nil, h.Now())
	require.True(t, errs.IsKind(err, errs.KindConflict), "got %v", err)
	require.Equal(t, "model_providers_org_name_uniq", errs.CodeOf(err))
	_, err = providers.Insert(h.Ctx, other.Scope, draft("b-local", "https://elsewhere.test"), uuid.Nil, h.Now())
	require.NoError(t, err)
}

func TestTheSchemaRefusesAKeyOverPlaintext(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org := h.Org()
	ring := keyring(t)
	creds := channelrepo.NewCredentialRepository(h.Pool, ring, ring, h.Clock)
	credID, err := creds.CreateCredential(h.Ctx, org.Scope, domain.CredentialKind,
		map[string]string{domain.CredentialValueKey: "sk"})
	require.NoError(t, err)

	// Built by hand, past domain.KeyNeedsHTTPS, to prove the CHECK holds on its own.
	plain := domain.ProviderDraft{Name: "plain", BaseURL: "http://litellm.svc:4000", Model: "m"}
	_, err = repository.NewProviderRepository(h.Pool).Insert(h.Ctx, org.Scope, plain, credID, h.Now())
	require.Error(t, err)
	require.Equal(t, "model_providers_key_tls_ck", errs.CodeOf(err))
}
