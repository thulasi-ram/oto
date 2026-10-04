package repository_test

// THE CLASSIFICATION SET AND A FINDING'S CLASS AGAINST A REAL POSTGRES (migration 00096,
// git-bug 4298aa0). What the SQL holds on its own: a fresh org has no classes, the set
// is replaced whole in its operator's order, `unclassified` and a name outside the
// alphabet cannot be a row, a class needs a Finding — and replacing the set leaves every
// Finding's class exactly as it was given.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/investigator/repository"
	"github.com/thulasiram/oto/internal/platform/db"
)

func TestTheClassSetIsReplacedWholeAndRewritesNoFinding(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	classes := repository.NewClassRepository(w.h.Pool)
	replace := func(scope db.TenantScope, cs ...domain.Class) {
		t.Helper()
		set, err := domain.NewClassSet(cs)
		require.NoError(t, err)
		require.NoError(t, db.NewTxRunner(w.h.Pool).InTx(w.h.Ctx, func(ctx context.Context) error {
			return classes.ReplaceClassSet(ctx, scope, set, w.h.Now())
		}))
	}

	// ⭐ oto ships no classes: a fresh org reads an empty set, and nothing seeded one.
	set, err := classes.ClassSet(w.h.Ctx, w.scope)
	require.NoError(t, err)
	require.True(t, set.Empty())

	replace(w.scope, domain.Class{Name: "deploy-regression", Description: "A change we shipped broke it."},
		domain.Class{Name: "capacity"})
	set, err = classes.ClassSet(w.h.Ctx, w.scope)
	require.NoError(t, err)
	require.Equal(t, []string{"deploy-regression", "capacity", domain.Unclassified}, set.Answers())

	other, err := classes.ClassSet(w.h.Ctx, w.h.Org().Scope)
	require.NoError(t, err)
	require.True(t, other.Empty(), "another org reads none of them")

	// A run's Finding is classified in the set as it stood.
	run := w.queued(t, "k", w.h.Now())
	w.start(t, run.ID, w.h.Now(), 2)
	require.NoError(t, w.runs.Finish(w.h.Ctx, w.scope, run.ID, domain.Completed(), domain.Usage{}, 0,
		"The deploy.", "deploy-regression", w.h.Now()))

	// The operator renames the class, then empties the set.
	replace(w.scope, domain.Class{Name: "change-failure"})
	set, err = classes.ClassSet(w.h.Ctx, w.scope)
	require.NoError(t, err)
	require.Equal(t, []string{"change-failure", domain.Unclassified}, set.Answers())
	replace(w.scope)

	// ⛔ The Finding keeps the class it was given.
	got, err := w.runs.Get(w.h.Ctx, w.scope, run.ID)
	require.NoError(t, err)
	require.Equal(t, "deploy-regression", got.Classification)
	prior, err := w.runs.PriorFindings(w.h.Ctx, w.scope, "k", uuid.Nil, 5)
	require.NoError(t, err)
	require.Len(t, prior, 1)
	require.Equal(t, "deploy-regression", prior[0].Classification)

	// ⛔ The CHECKs: `unclassified` is never a row, a name keeps its alphabet.
	for _, name := range []string{"unclassified", "Noise", "a b"} {
		_, err := w.h.Pool.Exec(w.h.Ctx, `INSERT INTO investigation_classes (org_id, name, description, position, created_at)
		  VALUES ($1, $2, '', 0, $3)`, w.scope.OrgID(), name, w.h.Now())
		require.Error(t, err, "investigation_classes admitted %q", name)
	}
}

func TestAClassificationBelongsToAFinding(t *testing.T) {
	t.Parallel()
	w := newWorld(t)

	// No Finding, no class: `investigations_class_ck`.
	run := w.queued(t, "k", w.h.Now())
	w.start(t, run.ID, w.h.Now(), 2)
	err := w.runs.Finish(w.h.Ctx, w.scope, run.ID, domain.EndedBy(domain.ReasonModelError, "502"), domain.Usage{}, 0,
		"", domain.Unclassified, w.h.Now())
	require.Error(t, err)

	// No set offered: NULL, read back as none — never as `unclassified`.
	plain := w.queued(t, "k", w.h.Now())
	w.start(t, plain.ID, w.h.Now(), 2)
	require.NoError(t, w.runs.Finish(w.h.Ctx, w.scope, plain.ID, domain.Completed(), domain.Usage{}, 0,
		"The deploy.", "", w.h.Now()))
	got, err := w.runs.Get(w.h.Ctx, w.scope, plain.ID)
	require.NoError(t, err)
	require.Equal(t, "", got.Classification)
	var isNull bool
	require.NoError(t, w.h.Pool.QueryRow(w.h.Ctx,
		`SELECT classification IS NULL FROM investigations WHERE id = $1`, plain.ID).Scan(&isNull))
	require.True(t, isNull)
}
