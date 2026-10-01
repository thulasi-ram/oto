package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/incidents/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// Service is the Incident's four human verbs — draw, add, remove, move — and its
// two reads (ADR 0052 §1–§4).
//
// ⛔ THERE IS NO FIFTH VERB, AND THE ABSENCE IS THE RULING. Nothing here sets an
// Incident's state (§3: it is read off the member Cases), resolves or closes one,
// names a lead, or records a severity a human chose: that is the RESPONSE, and it
// lives in the incident tool the Incident is declared to (§5). Nothing here deletes
// an Incident either — a story that turned out to be two stories is two moves, and
// both stay on the record.
//
// ⭐ EVERY WRITE IS ONE TRANSACTION THAT ALSO NARRATES. The membership row and the
// `incident.case_*` fact on the member Case's timeline commit together or not at
// all, so the Case's history can never claim a membership the Incident does not
// have, or miss one it does.
type Service struct {
	incidents Repository
	tx        TxRunner
	timeline  Timeline
	announcer Announcer
	clock     clock.Clock
}

// Deps are the Service's collaborators. Every one is required: an Incident write
// with no transaction, or with no timeline to narrate onto, is a write whose
// guarantees are gone, and failing at construction is cheaper than failing at
// 03:00.
type Deps struct {
	Incidents Repository
	Tx        TxRunner
	Timeline  Timeline
	// Announcer declares each Incident fact outbound (ADR 0052 §5). Required for the
	// reason Timeline is: a membership change oto told nobody about is an Incident
	// the incident tool never hears of, silently.
	Announcer Announcer
	Clock     clock.Clock
}

// New builds the Service.
func New(d Deps) (*Service, error) {
	switch {
	case d.Incidents == nil:
		return nil, errors.New("incidents: a repository is required")
	case d.Tx == nil:
		return nil, errors.New("incidents: a unit of work is required")
	case d.Timeline == nil:
		return nil, errors.New("incidents: a timeline is required; a membership change nobody " +
			"narrates is a Case history with a hole in it")
	case d.Announcer == nil:
		return nil, errors.New("incidents: an announcer is required; an Incident fact nobody " +
			"declares is one the incident tool never hears of")
	}
	if d.Clock == nil {
		d.Clock = clock.New()
	}
	return &Service{
		incidents: d.Incidents, tx: d.Tx, timeline: d.Timeline,
		announcer: d.Announcer, clock: d.Clock,
	}, nil
}

func (s *Service) now() time.Time { return s.clock.Now().UTC() }

// List returns a page of the org's Incidents, newest first, each with its derived
// state.
func (s *Service) List(ctx context.Context, scope db.TenantScope, p db.Keyset) ([]domain.Incident, db.Cursor, error) {
	return s.incidents.List(ctx, scope, p)
}

// Get returns one Incident by the number a human quotes, with every spell of
// every Case that has been in it.
func (s *Service) Get(ctx context.Context, scope db.TenantScope, number int64) (domain.Detail, error) {
	return s.incidents.Get(ctx, scope, number)
}

// GetByID is Get addressed by id, for the notification layer building an
// Incident fact's card at claim time (ADR 0052 §5).
func (s *Service) GetByID(ctx context.Context, scope db.TenantScope, id uuid.UUID) (domain.Detail, error) {
	return s.incidents.GetByID(ctx, scope, id)
}

// HoldingCase returns the Incident one Case is in NOW — none or exactly one,
// because a Case is in at most one (`incident_members_case_live_uniq`).
//
// ⭐ IT IS THE READ THAT LETS A SCREEN SAY "IT WILL BE MOVED" BEFORE ANYONE PRESSES
// ANYTHING (git-bug f89c9cc). Without it the only way a human learns a Case is
// already in a story is the `409 case_in_incident` on the draw, which is the
// refusal arriving where the question should have been answered. It reads the
// same membership the refusal reads, and like that read it decides nothing: the
// partial unique index is still what holds the line.
func (s *Service) HoldingCase(ctx context.Context, scope db.TenantScope, caseID uuid.UUID) ([]domain.Incident, error) {
	held, err := s.incidents.LiveMemberships(ctx, scope, []uuid.UUID{caseID})
	if err != nil {
		return nil, err
	}
	ref, ok := held[caseID]
	if !ok {
		return []domain.Incident{}, nil
	}
	d, err := s.incidents.GetByID(ctx, scope, ref.ID)
	if err != nil {
		return nil, err
	}
	return []domain.Incident{d.Incident}, nil
}

// ConversationFor answers the notification layer's one question about a Case at
// the moment it evaluates a fact about it (ADR 0052 §6): is this Case in an
// Incident that is a CONVERSATION — drawn by a Correlator that says its Incidents
// are — and if so, which one?
//
// ⭐ THE BOUNDARY IS THE MEMBERSHIP ROW'S COMMIT, AND NOTHING WAITS ON IT. A fact
// evaluated before the Case joined reads no row and lands in the Case's own
// thread; one evaluated after reads the row and lands in the Incident's. Nothing
// here holds a fact back for a Correlator that has not run yet, and nothing moves a
// fact that has already been placed.
//
// ⭐ A HUMAN'S MEMBERSHIP COUNTS THE SAME AS A CORRELATOR'S. A Case a human added
// to, or moved into, a Correlator-drawn conversation is a member like any other,
// and its later facts follow it there; a Case moved OUT follows the move. What
// decides is the Incident's author, never the membership's.
func (s *Service) ConversationFor(ctx context.Context, scope db.TenantScope, caseID uuid.UUID) (domain.Ref, bool, error) {
	return s.incidents.ConversationHolding(ctx, scope, caseID)
}

// Draw is a human drawing one Incident over one or more Cases (ADR 0052 §2).
//
// ⭐ ALL OR NOTHING. A Case the org does not have, or one already in another
// Incident, refuses the whole draw: an Incident drawn over "the Cases that
// happened to be free" is not the story the human asked for. The refusal for the
// second names the holding Incident and the move — the read below exists only to
// write that sentence; the partial unique index is what actually holds the line.
//
// ⚠️ A RETRY IS SAFE WITHOUT AN IDEMPOTENCY CLAIM, and that is a consequence of
// the rule rather than an oversight: a second draw over the same Cases meets the
// first draw's memberships and is refused with a pointer to the Incident the
// first attempt drew.
func (s *Service) Draw(
	ctx context.Context, scope db.TenantScope, caseIDs []uuid.UUID, by domain.Attribution,
) (domain.Detail, error) {
	if !by.IsHuman() {
		return domain.Detail{}, errs.New(errs.KindInternal, "incident_draw_not_human",
			"this path draws for a human; a Correlator draws through its own")
	}
	ids, err := distinctCases(caseIDs)
	if err != nil {
		return domain.Detail{}, err
	}

	var drawn domain.Ref
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		cases, err := s.resolveCases(ctx, scope, ids)
		if err != nil {
			return err
		}
		live, err := s.incidents.LiveMemberships(ctx, scope, ids)
		if err != nil {
			return err
		}
		for _, c := range cases {
			if holder, ok := live[c.ID]; ok {
				return domain.CaseInIncident(c, holder)
			}
		}

		at := s.now()
		drawn, err = s.incidents.Insert(ctx, scope, at, by)
		if err != nil {
			return err
		}
		// ⭐ ONE FACT FOR THE DRAW, NOT ONE PER CASE. The Incident coming into being
		// is the thing the incident tool is told; the Cases it was drawn over are on
		// the card. Its initial state is not a separate fact either: an Incident
		// drawn over closed Cases is born quiet and never went quiet.
		if err := s.announce(ctx, scope, drawn.ID, domain.FactDrawn); err != nil {
			return err
		}
		for _, c := range cases {
			if err := s.incidents.AddMember(ctx, scope, drawn.ID, c.ID, at, by); err != nil {
				return err
			}
			if err := s.timeline.RecordIncidentFact(ctx, scope, CaseFact{
				Type:    kernel.EventIncidentCaseAdded,
				CaseID:  c.ID,
				AlertID: c.AlertID,
				Summary: fmt.Sprintf("Drawn into Incident #%d by %s", drawn.Number, who(by)),
				Payload: map[string]any{
					"incident_id":     drawn.ID.String(),
					"incident_number": drawn.Number,
					"drawn":           true,
				},
				By: by,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return domain.Detail{}, err
	}
	return s.incidents.Get(ctx, scope, drawn.Number)
}

// Add is a human adding one Case to an Incident (ADR 0052 §4).
func (s *Service) Add(
	ctx context.Context, scope db.TenantScope, number int64, caseID uuid.UUID, by domain.Attribution,
) (domain.Detail, error) {
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		in, err := s.incidents.Ref(ctx, scope, number)
		if err != nil {
			return err
		}
		cases, err := s.resolveCases(ctx, scope, []uuid.UUID{caseID})
		if err != nil {
			return err
		}
		c := cases[0]
		live, err := s.incidents.LiveMemberships(ctx, scope, []uuid.UUID{caseID})
		if err != nil {
			return err
		}
		if holder, ok := live[caseID]; ok {
			if holder.ID == in.ID {
				return domain.AlreadyAMember(c, in)
			}
			return domain.CaseInIncident(c, holder)
		}

		before, err := s.lockAndCount(ctx, scope, in.ID)
		if err != nil {
			return err
		}
		if err := s.incidents.AddMember(ctx, scope, in.ID, caseID, s.now(), by); err != nil {
			return err
		}
		if err := s.announceMembership(ctx, scope, in.ID, domain.FactCaseAdded, before); err != nil {
			return err
		}
		return s.timeline.RecordIncidentFact(ctx, scope, CaseFact{
			Type:    kernel.EventIncidentCaseAdded,
			CaseID:  c.ID,
			AlertID: c.AlertID,
			Summary: fmt.Sprintf("Added to Incident #%d by %s", in.Number, who(by)),
			Payload: map[string]any{
				"incident_id":     in.ID.String(),
				"incident_number": in.Number,
				"drawn":           false,
			},
			By: by,
		})
	})
	if err != nil {
		return domain.Detail{}, err
	}
	return s.incidents.Get(ctx, scope, number)
}

// Remove is a human taking one Case out of an Incident. The membership is
// tombstoned, never deleted, and the Case itself is untouched.
func (s *Service) Remove(
	ctx context.Context, scope db.TenantScope, number int64, caseID uuid.UUID, by domain.Attribution,
) (domain.Detail, error) {
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		in, err := s.incidents.Ref(ctx, scope, number)
		if err != nil {
			return err
		}
		c, err := s.memberCase(ctx, scope, caseID)
		if err != nil {
			return err
		}
		before, err := s.lockAndCount(ctx, scope, in.ID)
		if err != nil {
			return err
		}
		if err := s.incidents.RemoveMember(ctx, scope, in.ID, caseID, s.now(), by, uuid.Nil); err != nil {
			return err
		}
		if err := s.announceMembership(ctx, scope, in.ID, domain.FactCaseRemoved, before); err != nil {
			return err
		}
		return s.timeline.RecordIncidentFact(ctx, scope, CaseFact{
			Type:    kernel.EventIncidentCaseRemoved,
			CaseID:  c.ID,
			AlertID: c.AlertID,
			Summary: fmt.Sprintf("Removed from Incident #%d by %s", in.Number, who(by)),
			Payload: map[string]any{
				"incident_id":     in.ID.String(),
				"incident_number": in.Number,
			},
			By: by,
		})
	})
	if err != nil {
		return domain.Detail{}, err
	}
	return s.incidents.Get(ctx, scope, number)
}

// Move is a human moving one Case from one Incident to another, in ONE
// transaction: the tombstone here and the membership there are written together,
// both attributed to the same human, so no reader ever sees the Case in neither
// Incident or in both (ADR 0052 §4).
//
// ⭐ THE SOURCE IS NAMED, NOT LOOKED UP. The caller says which Incident the Case is
// leaving, and the tombstone's `removed_at IS NULL` guard is what checks it: a Case
// moved or removed meanwhile is a 404 here rather than a move from somewhere it no
// longer is. It returns the DESTINATION, which is the Incident the caller now
// cares about.
func (s *Service) Move(
	ctx context.Context, scope db.TenantScope, from, to int64, caseID uuid.UUID, by domain.Attribution,
) (domain.Detail, error) {
	if from == to {
		return domain.Detail{}, domain.MoveToSelf()
	}
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		src, err := s.incidents.Ref(ctx, scope, from)
		if err != nil {
			return err
		}
		dst, err := s.incidents.Ref(ctx, scope, to)
		if err != nil {
			return err
		}
		c, err := s.memberCase(ctx, scope, caseID)
		if err != nil {
			return err
		}
		// Both Incidents are locked in ONE call, in id order, so a move from A to B
		// and a concurrent move from B to A queue rather than deadlock.
		if err := s.incidents.Lock(ctx, scope, []uuid.UUID{src.ID, dst.ID}); err != nil {
			return err
		}
		srcBefore, err := s.incidents.OpenMembers(ctx, scope, src.ID)
		if err != nil {
			return err
		}
		dstBefore, err := s.incidents.OpenMembers(ctx, scope, dst.ID)
		if err != nil {
			return err
		}
		at := s.now()
		if err := s.incidents.RemoveMember(ctx, scope, src.ID, caseID, at, by, dst.ID); err != nil {
			return err
		}
		if err := s.incidents.AddMember(ctx, scope, dst.ID, caseID, at, by); err != nil {
			return err
		}
		// A move is TWO facts outbound, one per Incident, because each Incident is
		// declared to the incident tool on its own: the one left behind lost a Case
		// (and may have gone quiet), the one joined gained it (and may be active
		// again). On the Case's own timeline it is ONE fact, below.
		if err := s.announceMembership(ctx, scope, src.ID, domain.FactCaseRemoved, srcBefore); err != nil {
			return err
		}
		if err := s.announceMembership(ctx, scope, dst.ID, domain.FactCaseAdded, dstBefore); err != nil {
			return err
		}
		return s.timeline.RecordIncidentFact(ctx, scope, CaseFact{
			Type:    kernel.EventIncidentCaseMoved,
			CaseID:  c.ID,
			AlertID: c.AlertID,
			Summary: fmt.Sprintf("Moved from Incident #%d to #%d by %s", src.Number, dst.Number, who(by)),
			Payload: map[string]any{
				"from_incident_id": src.ID.String(),
				"from_number":      src.Number,
				"to_incident_id":   dst.ID.String(),
				"to_number":        dst.Number,
			},
			By: by,
		})
	})
	if err != nil {
		return domain.Detail{}, err
	}
	return s.incidents.Get(ctx, scope, to)
}

// CasesEnded is the Case-ending observer (`alerts/service.CaseEndings`): it runs
// INSIDE the transaction that closed the Cases and announces `quiet` for every
// Incident that closing left with no open member (ADR 0052 §3, §5).
//
// ⭐ "WENT QUIET" IS DECIDED HERE AND NOWHERE ELSE FOR A CLOSE, because a Case
// closing is the only event that can quiet an Incident without a human touching
// it — Cases are strictly terminal, so nothing can make a member open again. Every
// Incident holding one of these Cases had that Case open a moment ago, so it was
// active; if no current member is open now, it just went quiet. The row lock makes
// two concurrent closes of an Incident's last two open Cases announce it once, not
// zero times.
//
// It writes nothing to `incidents` or `incident_members`: the state is derived, so
// there is nothing to write. It only declares.
func (s *Service) CasesEnded(ctx context.Context, scope db.TenantScope, caseIDs []uuid.UUID) error {
	if len(caseIDs) == 0 {
		return nil
	}
	held, err := s.incidents.Holding(ctx, scope, caseIDs)
	if err != nil || len(held) == 0 {
		return err
	}
	if err := s.incidents.Lock(ctx, scope, held); err != nil {
		return err
	}
	for _, id := range held {
		open, err := s.incidents.OpenMembers(ctx, scope, id)
		if err != nil {
			return err
		}
		if open > 0 {
			continue
		}
		if err := s.announce(ctx, scope, id, domain.FactQuiet); err != nil {
			return err
		}
	}
	return nil
}

// lockAndCount takes one Incident's row lock and reads its open-member count — the
// "before" of a membership change, read where no concurrent change can move it.
func (s *Service) lockAndCount(ctx context.Context, scope db.TenantScope, incidentID uuid.UUID) (int, error) {
	if err := s.incidents.Lock(ctx, scope, []uuid.UUID{incidentID}); err != nil {
		return 0, err
	}
	return s.incidents.OpenMembers(ctx, scope, incidentID)
}

// announceMembership declares a membership fact and, if the change moved the
// Incident's derived state, the state fact after it — read under the lock the
// caller already holds.
func (s *Service) announceMembership(
	ctx context.Context, scope db.TenantScope, incidentID uuid.UUID, fact domain.Fact, openBefore int,
) error {
	openAfter, err := s.incidents.OpenMembers(ctx, scope, incidentID)
	if err != nil {
		return err
	}
	if err := s.announce(ctx, scope, incidentID, fact); err != nil {
		return err
	}
	if state, moved := domain.Transition(openBefore, openAfter); moved {
		return s.announce(ctx, scope, incidentID, state)
	}
	return nil
}

// announce declares one fact, minting its occasion here — in the transaction that
// made it true — so a redelivered job is the same fact and a second happening never
// is.
func (s *Service) announce(ctx context.Context, scope db.TenantScope, incidentID uuid.UUID, fact domain.Fact) error {
	return s.announcer.Announce(ctx, scope, []Announcement{{
		IncidentID: incidentID, Fact: fact, Occasion: id.New(),
	}})
}

// resolveCases reads the named Cases inside the org, in the order asked, and
// refuses the request if any is missing — another org's Case included, which is
// the same 404 as one that never existed — or is a delivery drill's.
func (s *Service) resolveCases(ctx context.Context, scope db.TenantScope, ids []uuid.UUID) ([]domain.CaseRef, error) {
	found, err := s.incidents.Cases(ctx, scope, ids)
	if err != nil {
		return nil, err
	}
	out := make([]domain.CaseRef, 0, len(ids))
	for _, id := range ids {
		c, ok := found[id]
		if !ok {
			return nil, domain.CaseNotFound()
		}
		if c.Synthetic {
			return nil, domain.SyntheticCase(c)
		}
		out = append(out, c)
	}
	return out, nil
}

// memberCase is resolveCases for the remove and move paths, where the subject is
// an existing membership: a Case this org does not have cannot be a member of
// anything here, so it gets the member's 404 rather than a different one that
// would tell a caller more than the path asked.
func (s *Service) memberCase(ctx context.Context, scope db.TenantScope, caseID uuid.UUID) (domain.CaseRef, error) {
	found, err := s.incidents.Cases(ctx, scope, []uuid.UUID{caseID})
	if err != nil {
		return domain.CaseRef{}, err
	}
	c, ok := found[caseID]
	if !ok {
		return domain.CaseRef{}, domain.MemberNotFound()
	}
	return c, nil
}

// distinctCases refuses an empty draw and an oversized one, and folds repeats:
// an Incident is a SET of Cases, so naming one twice names it once.
func distinctCases(ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, errs.Validation("validation_failed", "an Incident is drawn over at least one Case",
			errs.Violation{Field: "case_ids", Code: "min_items", Message: "name at least one Case"})
	}
	seen := make(map[uuid.UUID]bool, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id == uuid.Nil || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) > domain.MaxCasesPerDraw {
		return nil, errs.Validation("validation_failed", "too many Cases for one draw",
			errs.Violation{Field: "case_ids", Code: "max_items",
				Message: fmt.Sprintf("name at most %d Cases", domain.MaxCasesPerDraw)})
	}
	if len(out) == 0 {
		return nil, domain.CaseNotFound()
	}
	return out, nil
}

// who renders the actor for a timeline sentence.
func who(a domain.Attribution) string {
	if a.IsHuman() {
		return a.Label()
	}
	return "a Correlator"
}
