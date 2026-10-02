package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/channels/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/mappings"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// TestTheMappingCatalogListsEveryEmbeddedFileWhole (ADR 0055 §2, git-bug 2b5eecc).
//
// The promise: Settings → Connections imports a catalog entry by COPYING its
// `mapping` into a connection, so the list carries each mapping whole, in the
// shape a connection stores — and names the secrets an importer must seal without
// carrying a value for any of them.
func TestTheMappingCatalogListsEveryEmbeddedFileWhole(t *testing.T) {
	t.Parallel()

	catalog, err := service.LoadCatalog(mappings.FS)
	require.NoError(t, err)
	client := apitest.New(NewRouter(Options{Catalog: catalog, Clock: clock.NewFake(chanNow)}))

	resp := client.GET("/payload-mapping-catalog").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listPayloadMappingCatalog", http.StatusOK, resp.Body())

	body := resp.JSON(t)
	data, ok := body["data"].([]any)
	if !ok || len(data) != len(catalog) {
		t.Fatalf("data is %#v, want the %d embedded catalog files", body["data"], len(catalog))
	}
	byID := map[string]map[string]any{}
	for _, d := range data {
		e, _ := d.(map[string]any)
		id, _ := e["id"].(string)
		byID[id] = e
	}
	pd, ok := byID["pagerduty"]
	if !ok {
		t.Fatalf("the catalog lists no pagerduty entry: %v", byID)
	}
	mapping, _ := pd["mapping"].(map[string]any)
	bodySrc, _ := mapping["body"].(string)
	if !strings.Contains(bodySrc, "{{ secrets.routing_key }}") {
		t.Fatalf("the pagerduty mapping does not read its key as a secret reference: %q", bodySrc)
	}
	secrets, _ := pd["secrets"].([]any)
	if len(secrets) != 1 || secrets[0] != "routing_key" {
		t.Fatalf("secrets is %#v, want the one name an importer must seal", pd["secrets"])
	}
	if _, ok := byID["incident-io"]; !ok {
		t.Fatalf("the catalog lists no incident-io entry: %v", byID)
	}
}

// TestAnEmptyCatalogIsAnEmptyListNotA503: a deployment wired without the catalog
// still maps whatever its operators write, so the list is empty rather than
// unavailable.
func TestAnEmptyCatalogIsAnEmptyListNotA503(t *testing.T) {
	t.Parallel()

	client := apitest.New(NewRouter(Options{Clock: clock.NewFake(chanNow)}))
	resp := client.GET("/payload-mapping-catalog").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listPayloadMappingCatalog", http.StatusOK, resp.Body())
	data, ok := resp.JSON(t)["data"].([]any)
	if !ok || len(data) != 0 {
		t.Fatalf("data is %#v, want []", data)
	}
}
