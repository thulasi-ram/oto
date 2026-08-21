package service_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	chdomain "github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
)

// TestARenderFailureCountsItselfOut is finding 7, stated as a test.
//
// A renderer that cannot build a legal payload used to die in perfect silence.
// The delivery row was marked `dead` correctly, and then the job returned nil and
// reported SUCCESS — so `oto_jobs_dead_total` never moved, the dead-letter never
// saw the payload, and the SPEC's metrics table was naming a counter that cannot
// fire here. The only externally visible symptom was that an alert stopped
// arriving, which is exactly the symptom oto exists to prevent.
//
// The fix is NOT to make the job fail. Handing the error out through
// `outcome.retry` classifies retryable, and the woken job would find the delivery
// already resolved and exit quietly — one extra wake-up and still no dead-letter.
// So this test pins the alarm that was actually built: a named counter and an
// error log, next to a dead row that still carries the offending bytes.
func TestARenderFailureCountsItselfOut(t *testing.T) {
	t.Parallel()

	tgt := &target{caps: chdomain.Capability(domain.CapThreading | domain.CapAmend)}
	h := newDispatchRig(t, tgt)
	ctx := t.Context()

	// The shape a real renderer refuses with: bytes plus a terminal error naming
	// the check that rejected them (§L.6).
	h.renderer.fail(&chdomain.Error{
		Class: chdomain.ClassConfigInvalid, Provider: "webhook",
		Code: "invalid_blocks", Cause: nil,
	})

	notificationID := h.evaluate(t, domain.ReasonFired, 1)
	rows := h.rowsFor(t, notificationID)
	require.Len(t, rows, 1)

	// The job SUCCEEDS. That is the deliberate half of the design, not an
	// oversight: the job was asked to resolve a delivery and it did.
	require.NoError(t, h.dispatcher.Dispatch(ctx, h.fx.scope, rows[0].ID))

	dead, err := h.deliveries.Get(ctx, h.fx.scope, rows[0].ID)
	require.NoError(t, err)
	require.Equal(t, domain.DeliveryDead, dead.Status,
		"a render failure is deterministic; retrying it would re-fail identically")
	require.Equal(t, domain.ClassConfigInvalid, dead.ErrorClass)
	require.NotEmpty(t, dead.Rendered,
		"§L.6: the bytes that failed are the only way to debug the death from the row")

	require.Equal(t, 0, tgt.delivers, "nothing may reach a destination unrendered")

	// The counter is the alarm. Its labels are the three bounded facts an operator
	// needs to find the bug: whose renderer, on which provider, in which mode.
	require.Equal(t, 1.0, counterValue(t,
		h.metrics.RenderInvalid.WithLabelValues("webhook", "webhook.json", "post_root")),
		"a render bug that increments nothing is a render bug nobody sees")

	// And the destination is flagged, so the channel screen says why it went quiet.
	channels := repository.NewChannelRepository(h.fx.pool)
	c, err := channels.Get(ctx, h.fx.scope, h.fx.channel.ID)
	require.NoError(t, err)
	require.Equal(t, domain.HealthConfigInvalid, c.HealthStatus)
}

// counterValue reads one counter without a registry.
//
// `testutil.ToFloat64` would say this in one line and is not worth promoting
// `prometheus/common` to a direct dependency of the whole module for.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, c.Write(&m))
	return m.GetCounter().GetValue()
}
