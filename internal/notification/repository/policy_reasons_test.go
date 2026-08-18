package repository_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/platform/id"
)

// ⭐ THE BACKSTOP FOR `reasons`, WHICH FOR ONE RULE WAS NOT THERE.
//
// CONTEXT.md §5b binds a bound to the DTO tag, the domain constructor and the
// DDL CHECK, and requires all three. Uniqueness on `notification_policies.reasons`
// was written in the first only: `policies_reasons_ck` counted elements and
// scanned for NULLs and never compared one element to another, and
// `Policy.Validate` did the same. This file is migration 00046's half of the
// repair — the layer that holds when a writer arrives that does not pass through
// `httpx.Bind`, which is the only reason a CHECK exists at all.
//
// It is asserted against a REAL Postgres and with raw SQL on purpose. The
// repository deliberately re-proves nothing (`CreatePolicy`: *"the domain's own
// Validate has already run in the service layer"*), so a test that went through
// the repository would be testing the domain check twice and the constraint never.
//
// The layer-3 half lives in internal/notification/domain/policy_test.go.

// insertPolicy writes one policy row directly, naming only the columns that have
// no default: 00034 took the database's clock off this table, so both timestamps
// are the caller's.
func insertPolicy(t *testing.T, fx fixture, reasons []string) error {
	t.Helper()

	_, err := fx.h.Pool.Exec(fx.h.Ctx,
		`INSERT INTO notification_policies (id, org_id, name, reasons, channel_ids,
		     created_at, updated_at)
		 VALUES ($1, $2, $3, $4, ARRAY[$5::uuid], $6, $6)`,
		id.New(), fx.scope.OrgID(), "p-"+uuid.NewString()[:8], reasons, fx.channel, fx.h.Now())
	return err
}

// refusedBy asserts err is a 23514 raised by the named constraint. The name is
// the runtime contract (SPEC §L.9): it is what `errs.Error.Code` carries, so a
// row refused by some OTHER constraint would be a different failure wearing the
// same exit code.
func refusedBy(t *testing.T, err error, constraint string) {
	t.Helper()

	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "want a Postgres error, got %v", err)
	require.Equal(t, "23514", pgErr.Code, "want check_violation")
	require.Equal(t, constraint, pgErr.ConstraintName)
}

// TestThePolicyReasonsCheckRefusesABag is the assertion the ticket turns on: the
// column may not hold a repeated reason, whoever is writing.
func TestThePolicyReasonsCheckRefusesABag(t *testing.T) {
	t.Parallel()

	fx := newFixture(t)

	err := insertPolicy(t, fx, []string{"fired", "acked", "fired"})
	refusedBy(t, err, "policies_reasons_ck")

	// The other writer of the same column. A fix applied only to the INSERT path
	// would leave a policy one PATCH away from becoming a bag.
	require.NoError(t, insertPolicy(t, fx, []string{"fired", "acked"}))
	_, err = fx.h.Pool.Exec(fx.h.Ctx,
		`UPDATE notification_policies SET reasons = ARRAY['fired','fired']
		  WHERE org_id = $1`, fx.scope.OrgID())
	refusedBy(t, err, "policies_reasons_ck")
}

// The ceiling and the set rule are one statement: with duplicates refused, a
// list of one more than the vocabulary can only be the whole vocabulary plus a
// repeat, and the whole vocabulary once has to remain legal or the constraint
// outlaws a policy an operator is entitled to write.
//
// ⚠️ THE WHOLE-VOCABULARY POLICY BELOW CARRIES `digest` AND NO WINDOW, AND THAT MUST
// STAY LEGAL. Migration 00058's `policies_digest_reason_ck` binds the two in ONE
// direction only — a window implies the reason, never the reverse — precisely so that
// this row is still insertable. A `digest` with no window is inert, exactly like
// `refired`, which nothing has produced since ADR 0040.
func TestThePolicyReasonsCheckBoundsTheColumnAtTheEnum(t *testing.T) {
	t.Parallel()

	fx := newFixture(t)

	all := domain.AllReasons()
	require.Len(t, all, 19, "the ceiling in the CHECK is the size of this vocabulary")

	whole := make([]string, 0, len(all))
	for _, r := range all {
		whole = append(whole, string(r))
	}
	require.NoError(t, insertPolicy(t, fx, whole),
		"a policy reacting to every reason once is the largest legal policy and must store")

	refusedBy(t, insertPolicy(t, fx, append(whole, "fired")), "policies_reasons_ck")
}

// ⛔ THE EMPTY ARRAY, which the old predicate let through while reading
// `BETWEEN 1 AND 32`. `array_length(reasons, 1)` is NULL for `{}`, a CHECK that
// evaluates to NULL PASSES, and a policy that reacts to nothing was therefore
// storable by the same non-DTO path this migration is about. 00046 counts with
// `cardinality`, which returns 0 and refuses the row.
func TestThePolicyReasonsCheckRefusesAPolicyThatReactsToNothing(t *testing.T) {
	t.Parallel()

	fx := newFixture(t)
	refusedBy(t, insertPolicy(t, fx, []string{}), "policies_reasons_ck")
}

// The domain refuses a duplicate BEFORE the database has to, which is the whole
// point of restating a bound in three places: the operator gets a field-level
// 422 naming `reasons`, not a 23514 an operator must decode.
func TestTheDomainRefusesTheDuplicateBeforeTheCheckDoes(t *testing.T) {
	t.Parallel()

	fx := newFixture(t)

	draft := domain.PolicyDraft{
		Name:       "page-sre",
		Reasons:    []domain.Reason{domain.ReasonFired, domain.ReasonFired},
		ChannelIDs: []uuid.UUID{fx.channel},
	}
	// The service layer's gate, restated here against the same draft the
	// repository would otherwise write through untouched.
	p := domain.Policy{
		OrgID:      fx.scope.OrgID(),
		Name:       draft.Name,
		Priority:   domain.DefaultPolicyPriority,
		Enabled:    true,
		Reasons:    draft.Reasons,
		ChannelIDs: draft.ChannelIDs,
	}
	require.Error(t, p.Validate(), "Validate is layer 3 and must not defer this to the CHECK")

	// And the repository, which validates nothing, still cannot store it: the
	// backstop holds even when the gate above is bypassed entirely.
	_, err := repository.NewConfigRepository(fx.h.Pool, fx.h.Clock).
		CreatePolicy(fx.h.Ctx, fx.scope, draft)
	require.Error(t, err, "the CHECK is the backstop and it must catch what reaches it")
}
