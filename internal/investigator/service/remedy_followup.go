package service

// AN EXECUTED REMEDY IS FOLLOWED BY AN INVESTIGATION OF ITS INCIDENT (ADR 0054 §6; git-bug
// a53c8b0). "An executed Remedy triggers a follow-up Investigation of its Incident, so the
// timeline shows whether it helped."
//
// ⭐⭐ RAISED IN THE TRANSACTION THAT RECORDS `executed`, AND ONLY THERE. The run's row and its
// `investigations.run` job are written beside the transition (the queue's insert joins the
// transaction in the context), so the Remedy is recorded executed AND its follow-up is asked
// for, or neither. A redelivered `remedies.execute` finds the Remedy no longer `executing` and
// records nothing — so it raises nothing: one execution, one follow-up. A Remedy that FAILED
// raises nothing: nothing changed that a look could judge, and a failure is already a fact on
// the Incident. Nothing here is on the notification path: the Finding the follow-up reaches is
// published and declared like any other (`finish`), and what a policy does with it is the
// notification layer's question.
//
// ⭐ THE INVESTIGATOR THAT PROPOSED IT RUNS IT. It chose the command, it holds the write Tools'
// descriptions and the read Tools that can see what the command touched, and its earlier
// Finding is the "before" its next one is read against — the same author on both sides of the
// change. It is asked whether or not it is opted into Incidents: that opt-in governs which
// Investigators an Incident's OWN facts (drawn, membership) wake, and this fact is the Remedy's.
//
// ⭐ "ITS INCIDENT" IS THE ONE ITS FACTS ARE DECLARED TO (remedyIncident): the Incident it was
// proposed about, or the Incident that holds the Case it was proposed about. A Remedy on a Case
// no Incident holds is followed up on THAT CASE — the Remedy's own subject, whose timeline is
// where "did it help" is read; recording nothing would leave exactly the gap this closes.
//
// ⭐ EVERY §6 CONTROL APPLIES, THROUGH THE ONE DOOR EVERY TRIGGER USES (request): a spent daily
// budget records the follow-up `skipped`/`budget`; the concurrency makes it wait; and it
// coalesces under the Investigator's minimum interval like a membership change
// (domain.TriggerRemedyExecuted). ⛔ A SWITCHED-OFF INVESTIGATOR OR ORG IS UNSUBSCRIBED (owner
// ruling O1): no row, as for an Incident's automatic triggers.

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// followUpExecutedRemedy asks for the follow-up Investigation of a Remedy just recorded
// `executed`, in the caller's transaction. Anything it cannot follow up — its run, its
// Investigator or its subject gone, a switch off — is nothing to do, never a failure: the
// Remedy's own record must not be lost to its follow-up.
func (s *Service) followUpExecutedRemedy(ctx context.Context, scope db.TenantScope, r domain.Remedy) error {
	if r.State != domain.RemedyExecuted {
		return nil
	}
	proposing, err := s.investigations.Get(ctx, scope, r.InvestigationID)
	if errs.IsKind(err, errs.KindNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	investigator, err := s.investigators.Get(ctx, scope, proposing.InvestigatorID)
	if errs.IsKind(err, errs.KindNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	controls, err := s.orgControls.InvestigationControls(ctx, scope)
	if err != nil {
		return err
	}
	if !controls.Enabled || !investigator.Enabled {
		return nil // ⛔ unsubscribed (owner ruling O1): no row.
	}

	incidentID, err := s.remedyIncident(ctx, scope, r)
	if err != nil {
		return err
	}
	switch {
	case incidentID != uuid.Nil:
		incident, err := s.incidents.InvestigationIncident(ctx, scope, incidentID)
		if errs.IsKind(err, errs.KindNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		by, err := domain.NewRequester(uuid.Nil, fmt.Sprintf("oto: a Remedy on Incident #%d was executed", incident.Number))
		if err != nil {
			return err
		}
		_, err = s.request(ctx, scope, incidentRef(incident), investigator.ID, by, domain.TriggerRemedyExecuted)
		return err
	case r.SubjectKind == domain.SubjectCase:
		by, err := domain.NewRequester(uuid.Nil, "oto: a Remedy on this Case was executed")
		if err != nil {
			return err
		}
		_, _, err = s.requestCase(ctx, scope, r.SubjectID, investigator.ID, by, domain.TriggerRemedyExecuted)
		if errs.IsKind(err, errs.KindNotFound) {
			return nil
		}
		return err
	default:
		return nil
	}
}
