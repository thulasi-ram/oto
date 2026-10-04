package service

// THE ORG'S CLASSIFICATION SET (ADR 0053 §5; git-bug 4298aa0). An operator writes the
// closed vocabulary a Finding is classified in; oto ships none. The set is read whole
// and replaced whole — it is one vocabulary, and a partial edit of a vocabulary is a
// second vocabulary nobody wrote.
//
// ⛔ REPLACING IT REWRITES NO FINDING. Every Finding copied the name it was given onto
// its own (frozen) row; a renamed or removed class changes what the NEXT run is offered
// and nothing any earlier run said.

import (
	"context"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// ClassSet reads the org's Classification set, in the operator's order.
func (s *Service) ClassSet(ctx context.Context, scope db.TenantScope) (domain.ClassSet, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.ClassSet{}, err
	}
	return s.classes.ClassSet(ctx, scope)
}

// ReplaceClassSet writes the org's whole Classification set — an empty one included,
// which is how an operator stops Findings being classified — and returns it as stored.
func (s *Service) ReplaceClassSet(ctx context.Context, scope db.TenantScope, set domain.ClassSet) (domain.ClassSet, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.ClassSet{}, err
	}
	at := s.now()
	var out domain.ClassSet
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		if err := s.classes.ReplaceClassSet(ctx, scope, set, at); err != nil {
			return err
		}
		read, err := s.classes.ClassSet(ctx, scope)
		out = read
		return err
	})
	if err != nil {
		return domain.ClassSet{}, err
	}
	return out, nil
}
