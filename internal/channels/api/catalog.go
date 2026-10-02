package api

import (
	"net/http"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// listMappingCatalog serves GET /api/v1/payload-mapping-catalog (ADR 0055 §2,
// git-bug 2b5eecc).
//
// It lists the catalog embedded in this binary — `mappings/*.yaml` — each entry
// with the mapping document whole, so Settings → Connections can import one by
// COPYING it into a webhook connection's own `payload_mapping`. There is no import
// endpoint, on purpose: an import is an ordinary connection update, through the
// same save-time gate as any mapping, and a copy that nothing on the connection
// links back to cannot be changed by a later catalog.
//
// ⛔ NOTHING HERE IS A SECRET. A catalog entry names the secrets an importer must
// seal (`secrets`) and carries none of them; the operator supplies the values on
// the connection.
//
// It is unpaginated, like `/channel-types`: the catalog is fixed at build time.
func (rt *Router) listMappingCatalog(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	if _, err := scopeOf(r); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]PayloadMappingCatalogEntryDTO, 0, len(rt.catalog))
	for _, e := range rt.catalog {
		out = append(out, catalogEntryDTO(e))
	}
	httpx.Data(w, r, http.StatusOK, out, started)
}

func catalogEntryDTO(e domain.CatalogMapping) PayloadMappingCatalogEntryDTO {
	commands := make([]PayloadMappingCommandDTO, 0, len(e.Commands))
	for _, c := range e.Commands {
		commands = append(commands, PayloadMappingCommandDTO{
			Field:     c.Field,
			Forbidden: append(make([]string, 0, len(c.Forbidden)), c.Forbidden...),
		})
	}
	return PayloadMappingCatalogEntryDTO{
		ID:        e.ID,
		Vendor:    e.Vendor,
		Title:     e.Title,
		Summary:   e.Summary,
		Docs:      append(make([]string, 0, len(e.Docs)), e.Docs...),
		CheckedOn: e.CheckedOn,
		Setup:     append(make([]string, 0, len(e.Setup)), e.Setup...),
		Commands:  commands,
		Secrets:   append(make([]string, 0, len(e.Secrets)), e.Secrets...),
		Mapping:   e.Mapping,
	}
}
