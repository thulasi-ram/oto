package repository_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/repository"
	"github.com/thulasiram/oto/test/harness"
)

// Migration 00090 (ADR 0055 §2, git-bug 2205620): a webhook connection carries a
// payload mapping and, in a third slot, the sealed secrets it names. What is read
// back is the mapping and the names — never a secret, so a stored or exported
// mapping holds none.

// TestAMappingAndItsSecretNamesRoundTripWithoutTheSecret stores a mapping and its
// secrets, reads the connection back, and finds the names and not the value
// anywhere in what a reader gets.
func TestAMappingAndItsSecretNamesRoundTripWithoutTheSecret(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	creds := repository.NewCredentialRepository(h.Pool, jsonSealer{}, jsonSealer{}, h.Clock)
	conns := repository.NewConnectionRepository(h.Pool, h.Clock)

	const secret = "R0UTING-KEY-VALUE"
	sealed, err := creds.Create(h.Ctx, org.Scope, domain.MappingSecretsKind, map[string]string{"routing_key": secret})
	require.NoError(t, err)

	mapping := json.RawMessage(`{"body": "{\"routing_key\": \"{{ secrets.routing_key }}\"}"}`)
	conn, err := conns.Create(h.Ctx, org.Scope, domain.NewConnection{
		Type: domain.TypeWebhook, Name: "pagerduty", Config: json.RawMessage(`{}`),
		PayloadMapping: mapping, MappingCredentialID: &sealed.ID, MappingSecretNames: []string{"routing_key"},
	})
	require.NoError(t, err)

	require.JSONEq(t, string(mapping), string(conn.PayloadMapping))
	require.Equal(t, []string{"routing_key"}, conn.MappingSecretNames)
	require.Equal(t, &sealed.ID, conn.MappingCredentialID)

	read, err := json.Marshal(conn)
	require.NoError(t, err)
	require.False(t, strings.Contains(string(read), secret),
		"⛔ the connection as read carries the secret: %s", read)

	// The secrets unseal for the send path, under their names.
	kind, values, err := creds.Resolve(h.Ctx, org.Scope, sealed.ID)
	require.NoError(t, err)
	require.Equal(t, domain.MappingSecretsKind, kind)
	require.Equal(t, map[string]string{"routing_key": secret}, values)

	// Removing the mapping and detaching the slot clears both, and the names go
	// with the slot they describe.
	none := json.RawMessage(nil)
	cleared, err := conns.Update(h.Ctx, org.Scope, conn.ID, domain.ConnectionPatch{
		PayloadMapping: &none,
		MappingSecrets: &domain.MappingSecretsSlot{},
	})
	require.NoError(t, err)
	require.Nil(t, cleared.PayloadMapping)
	require.Nil(t, cleared.MappingCredentialID)
	require.Empty(t, cleared.MappingSecretNames)
}

// TestOnlyAWebhookCarriesAMapping is channel_connections_mapping_ck.
func TestOnlyAWebhookCarriesAMapping(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	conn := h.SlackConnection(org, "T0MAPPING")
	_, err := h.Pool.Exec(h.Ctx,
		`UPDATE channel_connections SET payload_mapping = '{"body":"{}"}'::jsonb WHERE id = $1`, conn.ID)
	require.Error(t, err, "a slack connection took a payload mapping")
	require.Contains(t, err.Error(), "channel_connections_mapping_ck")
}
