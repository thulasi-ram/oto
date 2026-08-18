package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/thulasiram/oto/internal/grouping/domain"
)

// ⛔⛔ THIS FILE WAS `damping_test.go` AND MOST OF IT IS DELETED WITH THE CODE IT
// COVERED. `TestDefaultStormPolicy`, `TestStormPolicyNormalise`,
// `TestStormPolicyNormaliseIsIdempotent`, `TestEvaluateStormEntersOnlyAboveTheThreshold`,
// `TestEvaluateStormEchoesThePolicyInForce`, `TestEvaluateStormNormalisesNowToUTC`,
// `TestEvaluateStormEndsOnlyAfterCooldownWithoutANewMember`,
// `TestEvaluateStormCountsDistinctJoins`, `TestEvaluateStormOnAClosedGeneration`,
// `TestEvaluateStormDoesNotRestartAnActiveStorm` and `TestApplyStorm` all asserted the
// behaviour of a damper that no longer exists: storm collapse is removed, not disabled.
// See the tombstone at the top of `lifecycle.go`. Keeping them green against a
// re-implementation would be the fastest way for the damper to come back.
//
// What survives is the one number that was only ever a lodger in `StormPolicy`.

func TestDefaultLifecyclePolicy(t *testing.T) {
	t.Parallel()

	assert.Equal(t, domain.DefaultGroupCloseDelay, domain.DefaultLifecyclePolicy().CloseDelay)

	// ⛔ 20m, not the 5m this assertion once transcribed. Closing a generation freezes
	// its Slack thread, so the next observation opens N+1 with a brand-new root card
	// (ADR 0005, §B.5), and `identity/domain.DefaultGroupCloseDelay` is pinned EQUAL to
	// `refire_grace` by ADR 0026. At 5m against a 10m grace the generation closed
	// halfway through the grace and the whole second half bought a re-fire that posted a
	// new card anyway — the mismatch defeated the grace. Equality is safe rather than
	// racy because this clock runs from the group's last ACTIVITY (the resolve as oto
	// observed it) and the grace runs from the upstream `ended_at`, which is the same
	// instant or earlier.
	//
	// If this fails, the fix is almost certainly in `identity/domain` and not here;
	// `identity/domain/defaults_derivation_test.go` is what keeps the two in step.
	assert.Equal(t, 20*time.Minute, domain.DefaultGroupCloseDelay)
}

// TestLifecyclePolicyNormaliseFillsAZeroCloseDelay: a partially-configured org must not
// be able to produce a zero close delay, which would freeze every generation's thread on
// the tick after it lost its last live member.
func TestLifecyclePolicyNormaliseFillsAZeroCloseDelay(t *testing.T) {
	t.Parallel()

	assert.Equal(t, domain.DefaultGroupCloseDelay,
		domain.LifecyclePolicy{}.Normalise().CloseDelay)
	assert.Equal(t, domain.DefaultGroupCloseDelay,
		domain.LifecyclePolicy{CloseDelay: -time.Second}.Normalise().CloseDelay)

	// A configured value is left alone, and normalising is idempotent.
	set := domain.LifecyclePolicy{CloseDelay: 90 * time.Second}
	assert.Equal(t, set, set.Normalise())
	assert.Equal(t, set.Normalise(), set.Normalise().Normalise())
}
