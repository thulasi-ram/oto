package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/incidents/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Correlators is the machine author of Incidents (ADR 0052 §2, §4; git-bug
// 61eeddf): the operator's CRUD over Correlators, and the evaluator that runs one
// Case through them.
//
// ⭐ IT IS A SECOND TYPE BESIDE Service AND NOT MORE METHODS ON IT, and it borrows
// Service's machinery rather than copying it. A Correlator's draw and join are the
// human's draw and add with a different author: the same row lock before the
// count, the same membership row through the same repository, the same
// `announceMembership` deciding `quiet`/`active_again`, the same timeline port. So
// it holds the *Service and calls those, and the one place either author's
// membership change can be got wrong stays one place.
//
// ⛔ IT NEVER RUNS ON THE INGEST TRANSACTION (CONTEXT.md commitment 2). `Correlate`
// is called by the `incidents.correlate` job, which a Case opening enqueues; the
// job runs on the `lifecycle` queue, so it can neither block nor delay the Case's
// own `notify.evaluate` — a Correlator that fails, or is slow, or is wrong, leaves
// every notification exactly as it would have been without it.
type Correlators struct {
	inc   *Service
	store CorrelatorStore
}

// NewCorrelators builds the Correlator path over an Incident service.
func NewCorrelators(inc *Service, store CorrelatorStore) (*Correlators, error) {
	switch {
	case inc == nil:
		return nil, errors.New("incidents: correlators need the incident service whose membership verbs they reuse")
	case store == nil:
		return nil, errors.New("incidents: correlators need a store")
	}
	return &Correlators{inc: inc, store: store}, nil
}

// ------------------------------------------------------------------ the CRUD

// List returns every live Correlator, disabled ones included, in evaluation order —
// the order the settings page shows and the evaluator walks.
func (c *Correlators) List(ctx context.Context, scope db.TenantScope) ([]domain.Correlator, error) {
	return c.store.List(ctx, scope)
}

// Get reads one live Correlator.
func (c *Correlators) Get(ctx context.Context, scope db.TenantScope, id uuid.UUID) (domain.Correlator, error) {
	return c.store.Get(ctx, scope, id)
}

// Create writes one Correlator after validating every bound the DDL holds.
func (c *Correlators) Create(ctx context.Context, scope db.TenantScope, d domain.CorrelatorDraft) (domain.Correlator, error) {
	k := d.Correlator()
	if err := k.Validate(); err != nil {
		return domain.Correlator{}, err
	}
	return c.store.Create(ctx, scope, k, c.inc.now())
}

// Update merges a patch into the stored Correlator and validates the RESULT,
// under the Correlator's row lock — the lock a decision takes — so an edit never
// lands between an evaluation's read of the Correlator and its write.
func (c *Correlators) Update(
	ctx context.Context, scope db.TenantScope, id uuid.UUID, p domain.CorrelatorPatch,
) (domain.Correlator, error) {
	if p.IsEmpty() {
		return domain.Correlator{}, errs.Validation("validation_failed", "supply at least one field to change",
			errs.Violation{Code: "min_properties", Message: "at least one property is required"})
	}
	var out domain.Correlator
	err := c.inc.tx.InTx(ctx, func(ctx context.Context) error {
		existing, err := c.store.Lock(ctx, scope, id)
		if err != nil {
			return err
		}
		merged := p.Apply(existing)
		if err := merged.Validate(); err != nil {
			return err
		}
		out, err = c.store.Update(ctx, scope, merged, c.inc.now())
		return err
	})
	return out, err
}

// Delete retires a Correlator. Every Incident it drew, and every membership it
// added, keeps naming it (migration 00085): the answer to "why is this here?" does
// not expire with its author.
func (c *Correlators) Delete(ctx context.Context, scope db.TenantScope, id uuid.UUID) error {
	return c.store.Delete(ctx, scope, id, c.inc.now())
}

// ------------------------------------------------------------- the evaluator

// Verdict is what one evaluation decided, for the job's log line and for tests.
// It is never stored: what a Correlator did is on the membership rows and the
// timeline, and what it declined to do is, correctly, nothing at all.
type Verdict string

// The verdicts.
const (
	// VerdictUnclaimable: the Case is already in an Incident, or a human once took
	// it out of one. Every Correlator skips it (ADR 0052 §4).
	VerdictUnclaimable Verdict = "unclaimable"
	// VerdictNoMatch: no live Correlator's matchers hold.
	VerdictNoMatch Verdict = "no_match"
	// VerdictBelowCount: the first matching Correlator claimed the Case, has no
	// Incident it may join, and its count condition is not met yet. The claim is
	// recorded, so a later Case can clear the count with this one in it.
	VerdictBelowCount Verdict = "below_count"
	// VerdictDrew: the Correlator drew a new Incident.
	VerdictDrew Verdict = "drew"
	// VerdictJoined: the Case joined the Correlator's latest Incident.
	VerdictJoined Verdict = "joined"
)

// Correlation is one evaluation's outcome.
type Correlation struct {
	Verdict Verdict
	// CorrelatorID is the Correlator that claimed the Case, for every verdict but
	// the first two.
	CorrelatorID uuid.UUID
	// Incident is the Incident drawn or joined.
	Incident domain.Ref
}

// Correlate runs one freshly opened Case through the org's Correlators (ADR 0052
// §2, §4). One transaction: the claim, the membership rows, the timeline facts and
// the outbound Incident facts commit together or not at all, so a retried job
// re-decides from a clean slate rather than from half of a decision.
//
// ⭐ THE ORDER OF QUESTIONS IS THE SPECIFICATION:
//
//  1. Is the Case claimable at all? A Case already in an Incident is skipped by
//     every Correlator — at most one Incident per Case — and a Case a human ever
//     removed or moved is never put back by a machine.
//  2. Which Correlator? The first, in the operator's order, whose matchers hold.
//     Only that one: a second matching Correlator does nothing, even when the first
//     then declines to draw because its count is not met (policy semantics).
//  3. May it join? Only the latest Incident THAT CORRELATOR drew, while it is
//     active or within the Correlator's `quiet_grace` after it went quiet
//     (`domain.Correlator.Joins`). A human-drawn Incident is never a candidate, so
//     it never grows by itself.
//  4. Else, may it draw? With no count condition, yes, over this Case. With one,
//     only if a window of the Correlator's length through this Case holds at least
//     `count_min` of its free claimed Cases — and then over all of them at once.
//
// The Correlator's row is locked between 2 and 3, which serialises its decisions:
// the fifth Case of a storm, arriving on a second worker, waits for the fourth's
// transaction and then counts it.
func (c *Correlators) Correlate(ctx context.Context, scope db.TenantScope, caseID uuid.UUID) (Correlation, error) {
	var out Correlation
	err := c.inc.tx.InTx(ctx, func(ctx context.Context) error {
		out = Correlation{}
		cs, err := c.store.CorrelationCase(ctx, scope, caseID)
		if err != nil {
			return err
		}
		if !cs.Claimable() {
			out.Verdict = VerdictUnclaimable
			return nil
		}

		live, err := c.store.Live(ctx, scope)
		if err != nil {
			return err
		}
		first, ok, err := firstMatch(live, cs.Labels)
		if err != nil {
			return err
		}
		if !ok {
			out.Verdict = VerdictNoMatch
			return nil
		}

		k, err := c.store.Lock(ctx, scope, first.ID)
		if errs.IsKind(err, errs.KindNotFound) {
			// Deleted between the walk and the lock. Its successor in the order was
			// never asked, and asking it now would decide on a configuration the
			// operator was in the middle of changing; the claim is simply not made.
			out.Verdict = VerdictNoMatch
			return nil
		}
		if err != nil {
			return err
		}
		// ⚠️ RE-READ UNDER THE LOCK. A human may have drawn, added or removed this
		// Case while the walk ran, and the lock serialises Correlators, not humans.
		// The partial unique index would refuse a second membership anyway, but only
		// by failing the job; reading again turns the common race into a skip.
		if cs, err = c.store.CorrelationCase(ctx, scope, caseID); err != nil {
			return err
		}
		if !cs.Claimable() {
			out.Verdict = VerdictUnclaimable
			return nil
		}

		at := c.inc.now()
		if err := c.store.RecordMatch(ctx, scope, k.ID, cs, at); err != nil {
			return err
		}
		out.CorrelatorID = k.ID

		latest, found, err := c.store.LatestDrawn(ctx, scope, k.ID)
		if err != nil {
			return err
		}
		// ⚠️ LOCK THE CANDIDATE, THEN READ IT AGAIN. The read above ran unlocked, and
		// whether it may join turns on its open count and when it went quiet — both
		// moved by a human's remove or a Case closing, which the Correlator's lock does
		// not serialise. Under the Incident's lock nothing can move them, so the second
		// read is the one Joins decides on; if it no longer joins, this Case draws.
		// Case → Correlator → Incident, the order every path takes.
		if found {
			if err := c.inc.incidents.Lock(ctx, scope, []uuid.UUID{latest.ID}); err != nil {
				return err
			}
			if latest, found, err = c.store.LatestDrawn(ctx, scope, k.ID); err != nil {
				return err
			}
		}
		if found && k.Joins(latest, cs.StartedAt) {
			if err := c.join(ctx, scope, k, latest.Ref, cs.CaseRef, at); err != nil {
				return err
			}
			out.Verdict, out.Incident = VerdictJoined, latest.Ref
			return nil
		}

		anchor := domain.CaseAt{CaseRef: cs.CaseRef, StartedAt: cs.StartedAt}
		var others []domain.CaseAt
		if k.Count.Enabled() {
			others, err = c.store.WindowCandidates(ctx, scope, k.ID,
				cs.StartedAt.Add(-k.Count.Window), cs.StartedAt.Add(k.Count.Window), cs.ID)
			if err != nil {
				return err
			}
		}
		span, clears := k.Count.Span(anchor, others)
		if !clears {
			out.Verdict = VerdictBelowCount
			return nil
		}
		drawn, err := c.draw(ctx, scope, k, span, at)
		if err != nil {
			return err
		}
		out.Verdict, out.Incident = VerdictDrew, drawn
		return nil
	})
	if err != nil {
		return Correlation{}, err
	}
	return out, nil
}

// firstMatch is the walk: the first Correlator, in the order given, whose matchers
// hold. A matcher that fails to evaluate — a regex the write path validated and
// the row somehow still carries broken — fails the evaluation rather than being
// read as "no match": skipping it would hand the Case to the next Correlator in
// the order, which is a decision the operator did not write.
func firstMatch(live []domain.Correlator, labels map[string]string) (domain.Correlator, bool, error) {
	for _, k := range live {
		ok, err := k.Matches(labels)
		if err != nil {
			return domain.Correlator{}, false, err
		}
		if ok {
			return k, true, nil
		}
	}
	return domain.Correlator{}, false, nil
}

// draw is a Correlator drawing one Incident over the Cases its count condition
// gathered — Service.Draw with a different author and no refusal path, because
// every Case here was read free inside this transaction and the partial unique
// index still holds the line if a human won a race since.
func (c *Correlators) draw(
	ctx context.Context, scope db.TenantScope, k domain.Correlator, cases []domain.CaseAt, at time.Time,
) (domain.Ref, error) {
	by := domain.ByCorrelator(k.ID)
	drawn, err := c.inc.incidents.Insert(ctx, scope, at, by)
	if err != nil {
		return domain.Ref{}, err
	}
	// One fact for the draw, as for a human's: the Cases are on the card.
	if err := c.inc.announce(ctx, scope, drawn.ID, domain.FactDrawn); err != nil {
		return domain.Ref{}, err
	}
	for _, cs := range cases {
		if err := c.inc.incidents.AddMember(ctx, scope, drawn.ID, cs.ID, at, by); err != nil {
			return domain.Ref{}, err
		}
		if err := c.inc.timeline.RecordIncidentFact(ctx, scope, CaseFact{
			Type:    kernel.EventIncidentCaseAdded,
			CaseID:  cs.ID,
			AlertID: cs.AlertID,
			Summary: fmt.Sprintf("Drawn into Incident #%d by Correlator %q", drawn.Number, k.Name),
			Payload: map[string]any{
				"incident_id":     drawn.ID.String(),
				"incident_number": drawn.Number,
				"drawn":           true,
				"correlator_id":   k.ID.String(),
			},
			By:             by,
			CorrelatorName: k.Name,
		}); err != nil {
			return domain.Ref{}, err
		}
	}
	return drawn, nil
}

// join is a Correlator adding one Case to an Incident it drew — Service.Add with a
// different author: the Incident's row lock before the count, then the membership,
// then `case_added` and, if the Incident was quiet — a re-fire inside the grace —
// `active_again`.
func (c *Correlators) join(
	ctx context.Context, scope db.TenantScope, k domain.Correlator, in domain.Ref, cs domain.CaseRef, at time.Time,
) error {
	by := domain.ByCorrelator(k.ID)
	before, err := c.inc.lockAndCount(ctx, scope, in.ID)
	if err != nil {
		return err
	}
	if err := c.inc.incidents.AddMember(ctx, scope, in.ID, cs.ID, at, by); err != nil {
		return err
	}
	if err := c.inc.announceMembership(ctx, scope, in.ID, domain.FactCaseAdded, before); err != nil {
		return err
	}
	return c.inc.timeline.RecordIncidentFact(ctx, scope, CaseFact{
		Type:    kernel.EventIncidentCaseAdded,
		CaseID:  cs.ID,
		AlertID: cs.AlertID,
		Summary: fmt.Sprintf("Added to Incident #%d by Correlator %q", in.Number, k.Name),
		Payload: map[string]any{
			"incident_id":     in.ID.String(),
			"incident_number": in.Number,
			"drawn":           false,
			"correlator_id":   k.ID.String(),
		},
		By:             by,
		CorrelatorName: k.Name,
	})
}
