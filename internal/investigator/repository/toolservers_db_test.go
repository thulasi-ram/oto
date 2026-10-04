package repository_test

// TOOLSERVERS AGAINST A REAL POSTGRES (migration 00093, git-bug 2e9a086): the token sealed
// in `channel_credentials` and unsealed only as a `tool_server_token`, the tenant
// predicate, the per-org name index, the CHECKs that keep a token off plaintext and a
// secret out of the URL, and a discovery that replaces the last list whole — or, when it
// fails, keeps it and says why.

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	channelrepo "github.com/thulasiram/oto/internal/channels/repository"
	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/investigator/repository"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/harness"
)

func toolServerDraft(t *testing.T, name, url, access string) domain.ToolServerDraft {
	t.Helper()
	limits, err := domain.NewCallLimits(0, 0)
	require.NoError(t, err)
	d, err := domain.NewToolServerDraft(name, url, "", access, "", limits)
	require.NoError(t, err)
	return d
}

func TestAToolServerTokenIsSealedAndOnlyOpensAsOne(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org := h.Org()
	ring := keyring(t)
	creds := channelrepo.NewCredentialRepository(h.Pool, ring, ring, h.Clock)
	servers := repository.NewToolServerRepository(h.Pool)
	keys := repository.NewKeyStore(h.Pool, ring)

	credID, err := creds.CreateCredential(h.Ctx, org.Scope, domain.ToolServerCredentialKind,
		map[string]string{domain.ToolServerCredentialValueKey: "tst-sealed"})
	require.NoError(t, err)
	ts, err := servers.Insert(h.Ctx, org.Scope, toolServerDraft(t, "k8s", "https://k8s-mcp.tools.svc/mcp", "read"), credID, h.Now())
	require.NoError(t, err)
	require.Equal(t, credID, ts.CredentialID)
	require.Equal(t, h.Now(), ts.CreatedAt, "created_at comes from the injected clock")
	require.Equal(t, domain.AccessRead, ts.Access)
	require.Equal(t, domain.TransportStreamableHTTP, ts.Transport)
	require.Equal(t, domain.DefaultResultBytes, ts.Limits.MaxResultBytes)
	require.True(t, ts.DiscoveredAt.IsZero())

	tok, err := keys.ResolveToolServerToken(h.Ctx, org.Scope, ts.CredentialID)
	require.NoError(t, err)
	require.Equal(t, "tst-sealed", tok)

	// ⛔ A model key behind the slot is refused by kind — and a ToolServer token behind
	// a model endpoint's slot likewise.
	modelKey, err := creds.CreateCredential(h.Ctx, org.Scope, domain.CredentialKind,
		map[string]string{domain.CredentialValueKey: "sk"})
	require.NoError(t, err)
	_, err = keys.ResolveToolServerToken(h.Ctx, org.Scope, modelKey)
	require.Equal(t, "tool_server_credential_kind", errs.CodeOf(err))
	_, err = keys.ResolveKey(h.Ctx, org.Scope, credID)
	require.Equal(t, "model_credential_kind", errs.CodeOf(err))

	_, err = keys.ResolveToolServerToken(h.Ctx, h.Org().Scope, credID)
	require.True(t, errs.IsKind(err, errs.KindNotFound), "got %v", err)
}

func TestToolServersAreReadPerOrgAndNamedOncePerOrg(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org, other := h.Org(), h.Org()
	servers := repository.NewToolServerRepository(h.Pool)

	vm, err := servers.Insert(h.Ctx, org.Scope, toolServerDraft(t, "vm", "http://vm-mcp.tools.svc:8080/mcp", "read"), uuid.Nil, h.Now())
	require.NoError(t, err)
	_, err = servers.Insert(h.Ctx, org.Scope, toolServerDraft(t, "k8s-write", "http://k8s-w.tools.svc/mcp", "write"), uuid.Nil, h.Now())
	require.NoError(t, err)

	list, err := servers.List(h.Ctx, org.Scope)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "k8s-write", list[0].Name, "listed by name")
	require.Equal(t, domain.AccessWrite, list[0].Access)

	byName, err := servers.ByNames(h.Ctx, org.Scope, []string{"vm", "nope"})
	require.NoError(t, err)
	require.Len(t, byName, 1)
	require.Equal(t, vm.ID, byName[0].ID)

	_, err = servers.Get(h.Ctx, other.Scope, vm.ID)
	require.True(t, errs.IsKind(err, errs.KindNotFound), "another org's ToolServer must 404, got %v", err)
	none, err := servers.ByNames(h.Ctx, other.Scope, []string{"vm"})
	require.NoError(t, err)
	require.Empty(t, none)

	_, err = servers.Insert(h.Ctx, org.Scope, toolServerDraft(t, "vm", "http://elsewhere/mcp", "read"), uuid.Nil, h.Now())
	require.Equal(t, "tool_servers_org_name_uniq", errs.CodeOf(err))
	_, err = servers.Insert(h.Ctx, other.Scope, toolServerDraft(t, "vm", "http://elsewhere/mcp", "read"), uuid.Nil, h.Now())
	require.NoError(t, err)
}

func TestTheSchemaRefusesATokenOverPlaintextAndASecretInTheURL(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org := h.Org()
	ring := keyring(t)
	creds := channelrepo.NewCredentialRepository(h.Pool, ring, ring, h.Clock)
	credID, err := creds.CreateCredential(h.Ctx, org.Scope, domain.ToolServerCredentialKind,
		map[string]string{domain.ToolServerCredentialValueKey: "tst"})
	require.NoError(t, err)
	servers := repository.NewToolServerRepository(h.Pool)

	// Built by hand, past the domain constructor, to prove each CHECK holds on its own.
	plain := toolServerDraft(t, "plain", "http://k8s-mcp:8080/mcp", "read")
	_, err = servers.Insert(h.Ctx, org.Scope, plain, credID, h.Now())
	require.Equal(t, "tool_servers_token_tls_ck", errs.CodeOf(err))

	for _, url := range []string{"https://u:p@k8s/mcp", "https://k8s/mcp?api_key=x"} {
		d := toolServerDraft(t, "bad", "https://k8s/mcp", "read")
		d.URL = url
		_, err = servers.Insert(h.Ctx, org.Scope, d, uuid.Nil, h.Now())
		require.Equal(t, "tool_servers_url_ck", errs.CodeOf(err), url)
	}
	d := toolServerDraft(t, "x", "https://k8s/mcp", "read")
	d.Access = "admin"
	_, err = servers.Insert(h.Ctx, org.Scope, d, uuid.Nil, h.Now())
	require.Equal(t, "tool_servers_access_ck", errs.CodeOf(err))
}

func TestADiscoveryReplacesTheListAndAFailureKeepsIt(t *testing.T) {
	t.Parallel()
	h := harness.New(t)
	org := h.Org()
	servers := repository.NewToolServerRepository(h.Pool)
	ts, err := servers.Insert(h.Ctx, org.Scope, toolServerDraft(t, "k8s", "http://k8s-mcp/mcp", "read"), uuid.Nil, h.Now())
	require.NoError(t, err)

	ro := true
	pods, err := domain.NewDiscoveredTool("pods_list", "List pods.", json.RawMessage(`{"type":"object"}`), &ro)
	require.NoError(t, err)
	bad, err := domain.NewDiscoveredTool("logs.query", "", json.RawMessage(`"no"`), nil)
	require.NoError(t, err)
	require.NoError(t, servers.ReplaceTools(h.Ctx, org.Scope, ts.ID, []domain.DiscoveredTool{pods, bad}, h.Now()))

	tools, err := servers.Tools(h.Ctx, org.Scope, ts.ID)
	require.NoError(t, err)
	require.Len(t, tools, 2)
	require.Equal(t, "logs.query", tools[0].Name)
	require.Nil(t, tools[0].InputSchema, "an unusable schema is stored as NULL, not dropped with its Tool")
	require.NotNil(t, tools[1].ReadOnlyHint)
	require.JSONEq(t, `{"type":"object"}`, string(tools[1].InputSchema))

	require.NoError(t, servers.RecordDiscoveryFailure(h.Ctx, org.Scope, ts.ID, "the ToolServer could not be reached", h.Now()))
	got, err := servers.Get(h.Ctx, org.Scope, ts.ID)
	require.NoError(t, err)
	require.False(t, got.DiscoveredAt.IsZero())
	require.Equal(t, "the ToolServer could not be reached", got.DiscoveryError)
	kept, err := servers.Tools(h.Ctx, org.Scope, ts.ID)
	require.NoError(t, err)
	require.Len(t, kept, 2, "a failed discovery keeps the last good list")

	require.NoError(t, servers.ReplaceTools(h.Ctx, org.Scope, ts.ID, []domain.DiscoveredTool{pods}, h.Now()))
	got, err = servers.Get(h.Ctx, org.Scope, ts.ID)
	require.NoError(t, err)
	require.Empty(t, got.DiscoveryError, "a success forgets the last failure")
	again, err := servers.Tools(h.Ctx, org.Scope, ts.ID)
	require.NoError(t, err)
	require.Len(t, again, 1)

	other, err := servers.Tools(h.Ctx, h.Org().Scope, ts.ID)
	require.NoError(t, err)
	require.Empty(t, other)
}
