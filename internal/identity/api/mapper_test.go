package api

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/authn"
)

// TestToMeDTOCarriesTrigramAvailableIntoSearchPartialMatchEnabled proves the
// one new piece of §C's wiring that lives entirely in this package: the
// process-lifetime `pg_trgm` bool arrives on MeView and must survive the
// explicit field-by-field mapping into MeDTO.Search — the three-model rule
// (ADR 0002) means this is a manual copy, not something the compiler enforces,
// so it earns its own assertion independent of the HTTP-level contract test.
func TestToMeDTOCarriesTrigramAvailableIntoSearchPartialMatchEnabled(t *testing.T) {
	t.Parallel()

	org, err := domain.NewOrg(uuid.New(), "acme", "Acme", domain.DefaultSettings())
	require.NoError(t, err)

	for _, tc := range []struct {
		name             string
		trigramAvailable bool
	}{
		{"enabled", true},
		{"disabled", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			view := service.MeView{
				Principal:        authn.Principal{Kind: authn.KindSession, OrgID: org.ID},
				Org:              org,
				TrigramAvailable: tc.trigramAvailable,
			}
			dto := toMeDTO(view)
			require.Equal(t, tc.trigramAvailable, dto.Search.PartialMatchEnabled)
		})
	}
}
