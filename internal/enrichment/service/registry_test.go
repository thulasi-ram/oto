package service_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/enrichment/service"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// The registry is the boot-time set of Enrichers, and every rejection below is
// deliberately a BOOT failure rather than a 3am surprise: enrichments_name_ck
// would refuse the row anyway, and the first fire of a new alert is the worst
// possible moment to discover it.

func TestNewRegistryRejectsEnrichersTheSchemaWouldRefuse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []domain.Enricher
		code string
	}{
		{
			name: "a nil enricher",
			in:   []domain.Enricher{nil},
			code: "enrichment_nil_enricher",
		},
		{
			// The DOT IS MANDATORY. This is the reason the runbook enricher is
			// registered as `runbook.link` and not as SPEC §F.3's `runbook`.
			name: "an undotted name",
			in:   []domain.Enricher{&stubEnricher{name: "runbook"}},
			code: "enrichment_bad_enricher_name",
		},
		{
			name: "an uppercase name",
			in:   []domain.Enricher{&stubEnricher{name: "Prom.Rule"}},
			code: "enrichment_bad_enricher_name",
		},
		{
			name: "an empty name",
			in:   []domain.Enricher{&stubEnricher{name: ""}},
			code: "enrichment_bad_enricher_name",
		},
		{
			name: "a version below one",
			in:   []domain.Enricher{&stubEnricher{name: "test.alpha", version: -1}},
			code: "enrichment_bad_enricher_version",
		},
		{
			name: "an unknown phase",
			in:   []domain.Enricher{&stubEnricher{name: "test.alpha", phase: domain.Phase(9)}},
			code: "enrichment_bad_enricher_phase",
		},
		{
			name: "the same name twice",
			in: []domain.Enricher{
				&stubEnricher{name: "test.alpha"},
				&stubEnricher{name: "test.alpha"},
			},
			code: "enrichment_duplicate_enricher",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg, err := service.NewRegistry(tc.in...)
			require.Error(t, err, "the registry must refuse this at boot")
			assert.Nil(t, reg)
			assert.Equal(t, tc.code, errs.CodeOf(err))
			assert.Equal(t, errs.KindInternal, errs.KindOf(err),
				"a misregistered enricher is a wiring bug, not a user's input")
		})
	}
}

func TestEmptyRegistryIsLegal(t *testing.T) {
	t.Parallel()

	// A deployment with enrichment switched off is a supported deployment.
	reg, err := service.NewRegistry()
	require.NoError(t, err)
	assert.Empty(t, reg.All())
	assert.Empty(t, reg.Select(domain.PhaseInline, nil))
}

// TestRegistryOrderIsDeterministic pins (phase, name), independent of the order
// the enrichers were wired in.
//
// This is not tidiness. A pipeline whose result ordering depends on
// registration order produces Slack cards that reshuffle between two identical
// alerts and diffs nobody can golden-test.
func TestRegistryOrderIsDeterministic(t *testing.T) {
	t.Parallel()

	reg, err := service.NewRegistry(
		&stubEnricher{name: "zulu.async", phase: domain.PhaseAsync},
		&stubEnricher{name: "alpha.async", phase: domain.PhaseAsync},
		&stubEnricher{name: "zulu.inline", phase: domain.PhaseInline},
		&stubEnricher{name: "alpha.inline", phase: domain.PhaseInline},
	)
	require.NoError(t, err)

	names := make([]string, 0, 4)
	for _, e := range reg.All() {
		names = append(names, e.Name())
	}
	assert.Equal(t,
		[]string{"alpha.inline", "zulu.inline", "alpha.async", "zulu.async"},
		names,
		"inline before async, then lexical by name")

	// All returns a copy: a caller that sorts the slice must not reorder the
	// registry for everybody else.
	got := reg.All()
	got[0], got[3] = got[3], got[0]
	assert.Equal(t, "alpha.inline", reg.All()[0].Name(), "All must hand back a copy")
}

// TestRegistryAnswersHonestlyAboutAnUnknownEnricherID is the "unknown id" case:
// every lookup must say no rather than invent a zero value.
func TestRegistryAnswersHonestlyAboutAnUnknownEnricherID(t *testing.T) {
	t.Parallel()

	known := &stubEnricher{name: "test.alpha"}
	reg, err := service.NewRegistry(known)
	require.NoError(t, err)

	e, ok := reg.Get("test.nosuch")
	assert.False(t, ok, "an unregistered id is not registered")
	assert.Nil(t, e, "and there is no zero-value enricher to hand back")

	assert.False(t, reg.Enabled("test.nosuch"), "an enricher that does not exist will not run")
	assert.True(t, reg.Enabled("test.alpha"))

	// Selecting an unknown name selects nothing. It is NOT an error: a job
	// payload minted by an older pod may name an enricher this build has
	// dropped, and parking the phase over it would cost the whole run.
	assert.Empty(t, reg.Select(domain.PhaseInline, []string{"test.nosuch"}),
		"an unknown name resolves to no enricher")

	// The known one still runs alongside the unknown one.
	sel := reg.Select(domain.PhaseInline, []string{"test.nosuch", "test.alpha"})
	require.Len(t, sel, 1)
	assert.Equal(t, "test.alpha", sel[0].Name())
}

func TestRegistrySelectFiltersByPhaseWhenGivenNoNames(t *testing.T) {
	t.Parallel()

	inline := &stubEnricher{name: "test.inline", phase: domain.PhaseInline}
	async := &stubEnricher{name: "test.async", phase: domain.PhaseAsync}
	reg, err := service.NewRegistry(inline, async)
	require.NoError(t, err)

	sel := reg.Select(domain.PhaseInline, nil)
	require.Len(t, sel, 1)
	assert.Equal(t, "test.inline", sel[0].Name())

	sel = reg.Select(domain.PhaseAsync, nil)
	require.Len(t, sel, 1)
	assert.Equal(t, "test.async", sel[0].Name())
}

// TestRegistrySelectByNameIgnoresTheDeclaredPhase is how an inline enricher
// that blew the budget is re-run in the async pass without having to lie about
// its own phase.
func TestRegistrySelectByNameIgnoresTheDeclaredPhase(t *testing.T) {
	t.Parallel()

	inline := &stubEnricher{name: "test.inline", phase: domain.PhaseInline}
	reg, err := service.NewRegistry(inline)
	require.NoError(t, err)

	sel := reg.Select(domain.PhaseAsync, []string{"test.inline"})
	require.Len(t, sel, 1, "a named inline enricher must be selectable in the async pass")
	assert.Equal(t, domain.PhaseInline, sel[0].Phase(), "and it does not have to lie about its phase")
}

func TestRegistrySelectTrimsTheNamesItIsGiven(t *testing.T) {
	t.Parallel()

	reg, err := service.NewRegistry(&stubEnricher{name: "test.alpha"})
	require.NoError(t, err)

	sel := reg.Select(domain.PhaseInline, []string{"  test.alpha\t"})
	require.Len(t, sel, 1, "a job payload with stray whitespace still names a real enricher")
}

// TestDisabledEnricherIsAbsentRatherThanFailed pins the runtime kill switch.
//
// Every enricher is independently disableable BY DESIGN: they are the part of
// oto most likely to misbehave against one customer's Prometheus, and the answer
// to "your rule lookup is hammering our API" has to be a config change, not a
// redeploy. A disabled enricher is ABSENT — nothing in the UI claims it failed.
func TestDisabledEnricherIsAbsentRatherThanFailed(t *testing.T) {
	t.Parallel()

	alpha := &stubEnricher{name: "test.alpha"}
	bravo := &stubEnricher{name: "test.bravo"}
	reg, err := service.NewRegistry(alpha, bravo)
	require.NoError(t, err)

	reg.Disable("test.alpha")

	assert.False(t, reg.Enabled("test.alpha"))
	assert.True(t, reg.Enabled("test.bravo"))

	sel := reg.Select(domain.PhaseInline, nil)
	require.Len(t, sel, 1)
	assert.Equal(t, "test.bravo", sel[0].Name(), "a disabled enricher is not dispatched")

	// Naming it explicitly does not resurrect it: the kill switch outranks the
	// job payload, which is the whole point of being able to turn it off.
	assert.Empty(t, reg.Select(domain.PhaseInline, []string{"test.alpha"}),
		"an explicit name must not defeat the kill switch")

	// It is still REGISTERED, so `GET /api/v1/enrichers` can report it as
	// present-and-disabled rather than pretending it was never wired.
	_, ok := reg.Get("test.alpha")
	assert.True(t, ok, "a disabled enricher is still discoverable")
	assert.Len(t, reg.All(), 2)
}

func TestDisableIgnoresBlankNames(t *testing.T) {
	t.Parallel()

	reg, err := service.NewRegistry(&stubEnricher{name: "test.alpha"})
	require.NoError(t, err)

	reg.Disable("", "   ")
	assert.True(t, reg.Enabled("test.alpha"), "blank names must not disable anything")
	assert.Len(t, reg.Select(domain.PhaseInline, nil), 1)
}

// TestDisabledEnricherIsNotRunByThePipeline is the same rule one layer up: a
// registry-level kill switch that the pipeline ignored would be a kill switch
// in name only.
func TestDisabledEnricherIsNotRunByThePipeline(t *testing.T) {
	t.Parallel()

	alpha := &stubEnricher{name: "test.alpha"}
	bravo := &stubEnricher{name: "test.bravo"}
	e := newEnv(t, nil, alpha, bravo)
	e.svc.Registry().Disable("test.alpha")

	out := e.run(t, domain.PhaseInline)

	require.Len(t, out.Results, 1)
	assert.Equal(t, "test.bravo", out.Results[0].Enricher())
	assert.Zero(t, alpha.callCount(), "a disabled enricher is not called")
	assert.NotContains(t, byName(out.Results), "test.alpha",
		"and it produces no row, so nothing in the UI claims it failed")
}

// TestEnricherTimeoutIsBoundedByThePhaseBudget covers the unexported timeoutOf
// through its only observable effect: an enricher that declares a timeout wider
// than the whole phase cannot spend everyone else's time.
func TestEnricherTimeoutIsBoundedByThePhaseBudget(t *testing.T) {
	t.Parallel()

	greedy := &stubEnricher{
		name:    "test.greedy",
		timeout: time.Hour,
		fn:      blocking(),
	}
	polite := &stubEnricher{name: "test.polite"}

	// A budget that is already spent by the time the goroutines start: the
	// greedy enricher's hour-long timeout must not survive it.
	e := newEnv(t, func(o *service.Options) { o.InlineBudget = time.Nanosecond }, greedy, polite)

	out := e.run(t, domain.PhaseInline)
	got := byName(out.Results)

	require.Contains(t, got, "test.greedy")
	assert.Equal(t, domain.StatusTimeout, got["test.greedy"].Status(),
		"an enricher may not declare its way past the phase ceiling")
	assert.Contains(t, got["test.greedy"].ErrorText(), "budget")
}
