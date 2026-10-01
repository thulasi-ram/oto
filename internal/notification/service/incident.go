package service

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/notification/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ADR 0052 §5: DECLARING AN INCIDENT IS A NOTIFICATION (git-bug aa6d18b).
//
// ⭐ AN INCIDENT FACT IS AN ORDINARY NOTIFICATION, ON PURPOSE. It gets the §C.7
// idempotency key, a `notifications` row, `notification_deliveries` with their own
// retry budget, the dead-letter, the delivery audit and the transactional outbox —
// everything that makes a Notification trustworthy — because those are what "every
// Incident goes to incident.io" needs and none of them is Incident-shaped. What this
// file adds is only what IS: the subject (the Incident, not a Case), the routing
// input (labels the whole story shares), and the destinations an Incident can reach
// today.
//
// ⛔ NOTHING HERE SENDS A COMMAND. The five Reasons are facts — drawn, a Case added
// or removed, quiet, active again — and none of them means resolve or close. An org
// that wants its incident tool to resolve on `quiet` writes that in the tool.

// IncidentReader is the port this module declares to read the Incident a fact is
// about. `internal/app` satisfies it over `incidents/service`; `notification` never
// imports `incidents` (CONTEXT.md §4).
type IncidentReader interface {
	Incident(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.IncidentFacts, error)
}

// IncidentIntent is one Incident fact, as `notify.incident` receives it.
type IncidentIntent struct {
	IncidentID uuid.UUID
	Reason     domain.Reason
	// OccasionID is WHICH TIME this fact happened, and for an Incident it is the
	// whole discriminator: an Incident has no `state_version`. Required.
	OccasionID uuid.UUID
}

// incidentSkipReason is the sentence a threaded destination's delivery is recorded
// as skipped with. It is a fact about the DESTINATION, never about the Incident.
const incidentSkipReason = "this channel threads its messages, and an Incident is not a " +
	"conversation on any threaded channel yet (ADR 0052 §6); the generic webhook carries Incident facts"

// EvaluateIncident is `notify.incident`: decide, record and fan out ONE Incident
// fact, in one transaction, with the delivery jobs enqueued inside it (ADR 0001).
//
// Order of operations, the same as `Evaluate`'s and for the same reasons: read the
// world once, route, record the intent suppressed or not — "oto decided not to tell
// you, and here is why" is the feature (§B.6) — and only then fan out.
//
// ⭐ AN ORG WITH NO INCIDENT POLICY SENDS NOTHING, AND SAYS SO. The intent is still
// recorded, as `no_policy`, exactly like any other fact nobody routed: oto drawing
// Incidents for analysis and conversation is ordinary (ADR 0052, Consequences), and
// declaring them is the narrowing a policy states.
func (s *NotificationService) EvaluateIncident(
	ctx context.Context, scope db.TenantScope, in IncidentIntent,
) (Result, error) {
	switch {
	case !in.Reason.Valid() || in.Reason.Subject() != domain.SubjectIncident:
		return Result{}, errs.Validation("unknown_reason", "not an Incident notification reason",
			errs.Violation{Field: "reason", Code: "enum", Message: string(in.Reason)})
	case in.IncidentID == uuid.Nil:
		return Result{}, errs.Validation("incident_required", "an Incident fact names its Incident",
			errs.Violation{Field: "incident_id", Code: "required", Message: "an incident id is required"})
	case in.OccasionID == uuid.Nil:
		// ⛔ REFUSED, NOT WARNED, UNLIKE `Evaluate`'s occasion-less snooze. There a
		// missing occasion costs only a SECOND announcement in one episode; here the
		// occasion is the key's only discriminator, so without it every `case_added`
		// on one Incident would collapse into the first one forever.
		return Result{}, errs.Validation("occasion_required", "an Incident fact names its occasion",
			errs.Violation{Field: "occasion_id", Code: "required", Message: "an occasion id is required"})
	case s.incidents == nil:
		return Result{}, errs.New(errs.KindInternal, "incident_reader_unwired",
			"the notification service has no Incident reader, so it cannot evaluate an Incident fact")
	}

	var out Result
	err := s.txr.InTx(ctx, func(ctx context.Context) error {
		var err error
		out, err = s.evaluateIncident(ctx, scope, in)
		return err
	})
	return out, err
}

func (s *NotificationService) evaluateIncident(
	ctx context.Context, scope db.TenantScope, in IncidentIntent,
) (Result, error) {
	now := s.clk.Now().UTC()

	facts, err := s.incidents.Incident(ctx, scope, in.IncidentID)
	if err != nil {
		return Result{}, err
	}

	n := domain.Notification{
		ID:          uuid.New(),
		OrgID:       scope.OrgID(),
		SubjectKind: domain.SubjectIncident,
		SubjectID:   facts.ID,
		// The Incident's own conversation (migration 00084). On the destinations an
		// Incident fact reaches today — those that keep no thread — that is simply
		// the destination; see `incidentSkipReason` for the others.
		ConversationKind: domain.ConversationIncident,
		ConversationID:   facts.ID,
		Reason:           in.Reason,
		// An Incident has no compare-and-set to version. The occasion is the key's
		// discriminator, and the version is the constant `notifications_sver_ck`
		// admits.
		StateVersion: 1,
		Status:       domain.StatusPending,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	n.IdempotencyKey = domain.IdempotencyKey(
		scope.OrgID(), n.SubjectKind, n.SubjectID, n.Reason, n.StateVersion, in.OccasionID)

	match, err := s.policies.Evaluate(ctx, scope, MatchRequest{
		Reason: in.Reason,
		Labels: facts.MatchLabels(),
	})
	if err != nil {
		return Result{}, err
	}
	if !match.Routed() {
		return s.record(ctx, scope, n, domain.SuppressedNoPolicy, nil, now)
	}
	id := match.Policy.ID
	n.PolicyID = &id
	if !match.Deliverable() {
		return s.record(ctx, scope, n, domain.SuppressedChannelDisabled, nil, now)
	}

	stored, created, err := s.notifications.Insert(ctx, scope, n)
	if err != nil {
		return Result{}, err
	}
	if !created && stored.Status == domain.StatusSuppressed {
		return Result{Notification: stored, Suppressed: stored.SuppressedReason}, nil
	}

	count, err := s.incidentFanOut(ctx, scope, stored, match.Live, now)
	if err != nil {
		return Result{}, err
	}
	if err := s.notifications.SetStatus(ctx, scope, stored.ID, domain.StatusDispatched, now); err != nil {
		return Result{}, err
	}
	stored.Status = domain.StatusDispatched
	// No `AppendNotificationCreated`: an Incident fact names no alert and no case, so
	// it belongs on no signal's timeline (`subjectOfEvent`), exactly as a digest does.
	// The member Cases already carry the `incident.case_*` facts themselves.
	return Result{Notification: stored, Created: created, Deliveries: count}, nil
}

// incidentFanOut splits the live destinations by what they can carry.
//
// A destination that keeps no thread — the generic webhook — gets one `post_root`
// through the ordinary `fanOut`, with its sequence-free delivery and its job
// enqueued in this transaction. A THREADED destination gets a delivery row recorded
// as SKIPPED, with the sentence that says why, and no job: an Incident is not a
// conversation on a threaded channel yet, and the honest record of "oto decided not
// to post this here" is a skipped delivery, not an absent one (§B.6).
func (s *NotificationService) incidentFanOut(
	ctx context.Context, scope db.TenantScope, n domain.Notification,
	live []domain.Channel, now time.Time,
) (int, error) {
	channels := append([]domain.Channel(nil), live...)
	sort.Slice(channels, func(i, j int) bool { return channels[i].ID.String() < channels[j].ID.String() })

	var plain []destination
	created := 0
	for _, c := range channels {
		if !needsThread(c) {
			plain = append(plain, destination{channel: c})
			continue
		}
		row, madeNew, err := s.deliveries.Create(ctx, scope, repository.NewDelivery{
			ID:             uuid.New(),
			NotificationID: n.ID,
			ChannelID:      c.ID,
			Mode:           domain.ModePostRoot,
			CreatedAt:      now,
		})
		if err != nil {
			return created, err
		}
		if !madeNew {
			continue
		}
		created++
		if err := s.deliveries.MarkSkipped(ctx, scope, row.ID, incidentSkipReason, now); err != nil {
			return created, err
		}
	}
	made, err := s.fanOut(ctx, scope, n, plain, now)
	return created + made, err
}

// incident projects an Incident fact's card from the Incident as it is NOW (C11).
//
// ⭐ IT NAMES NO SIGNAL AND OFFERS NO ACTION. Every action on a card acts on a
// signal — acknowledge this case, snooze this alert — and an Incident fact is about
// the story; a button would have to pick one of its Cases. What it carries instead
// is the membership, each Case with its labels and its own link, so an incident
// tool receiving this can show what the story is made of.
func (v *ViewService) incident(ctx context.Context, scope db.TenantScope, n domain.Notification) (*NotificationView, error) {
	if v.incidents == nil {
		return nil, errs.New(errs.KindInternal, "incident_reader_unwired",
			"the view service has no Incident reader, so it cannot build an Incident card")
	}
	f, err := v.incidents.Incident(ctx, scope, n.SubjectID)
	if err != nil {
		return nil, err
	}

	state := "quiet"
	if f.Active {
		state = "active"
	}
	iv := &IncidentView{
		ID:      f.ID.String(),
		Number:  f.Number,
		State:   state,
		DrawnAt: f.DrawnAt.UTC(),
		DrawnBy: incidentAuthor(f.DrawnByLabel, f.DrawnByCorrelator),
		Members: make([]IncidentMemberView, 0, len(f.Members)),
	}
	if v.baseURL != "" {
		iv.Link = v.baseURL + "/incidents/" + strconv.FormatInt(f.Number, 10)
	}
	for _, m := range f.Members {
		caseState := "closed"
		if m.CaseOpen {
			caseState = "open"
		}
		mv := IncidentMemberView{
			CaseID:         m.CaseID.String(),
			CaseNumber:     m.CaseNumber,
			CaseState:      caseState,
			AlertID:        m.AlertID.String(),
			AlertName:      m.Alertname,
			Labels:         m.Labels,
			AddedAt:        m.AddedAt.UTC(),
			AddedBy:        incidentAuthor(m.AddedByLabel, m.AddedByCorrelator),
			RemovedByLabel: m.RemovedByLabel,
			MovedToNumber:  m.MovedToNumber,
		}
		if !m.Current() {
			mv.RemovedAt = m.RemovedAt.UTC()
		}
		if v.baseURL != "" {
			mv.Link = v.baseURL + "/cases/" + m.CaseID.String()
		}
		iv.Members = append(iv.Members, mv)
	}
	return &NotificationView{
		Reason:     string(n.Reason),
		Incident:   iv,
		RenderedAt: v.clk.Now().UTC(),
	}, nil
}

func incidentAuthor(label string, correlator uuid.UUID) IncidentAuthorView {
	if correlator != uuid.Nil {
		return IncidentAuthorView{CorrelatorID: correlator.String()}
	}
	return IncidentAuthorView{Label: label}
}
