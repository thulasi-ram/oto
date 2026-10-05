package service

// A DIGEST CARRIES A FINDING THAT WAS READY WHEN ITS WINDOW CLOSED, AND NEVER WAITS FOR
// ONE THAT WAS NOT (ADR 0053 §2, §4; git-bug 3e96f5a).
//
// Two halves, on two clocks, and neither waits for the other:
//
//   - ARMING (ArmDigestInvestigations, the `investigations.digest` tick, once a minute per
//     tenant). For every live digest policy that names an Investigator, the window open
//     now gets ONE run, recorded and enqueued once its lead has begun —
//     `now >= window_end - DigestLead(window, wall budget)` — so a run that keeps to its
//     own wall-time budget has ended before the window closes.
//   - CARRYING (DigestFinding, read by the digest tick at the send). The run for the
//     window that just closed is read ONCE; if it has ended with a Finding
//     (domain.Investigation.CarriedByDigest) the digest carries a copy of it, and
//     otherwise the digest goes out with its built-in body. There is no second read.
//
// ⛔⛔ NOTHING HERE DECIDES WHETHER A DIGEST IS SENT (ADR 0053 §2). The digest tick sends
// a window that cleared its policy's floor whether or not a run was armed, ran, failed,
// was skipped, or is still running; this module is asked one question at the send and
// its answer changes what the digest SAYS. A Finding that ends after the send is kept on
// its run and is posted nowhere — the run never reaches the digest; the digest reads the
// run.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ArmDigestInvestigations is the `investigations.digest` tick for one org: it arms the
// run for every summarised digest window whose lead has begun and that has none yet,
// and returns how many it armed.
//
// ⭐ ONE POLICY'S FAILURE DOES NOT COST THE OTHERS THEIR RUN, for the reason the digest
// tick gives (`notification/service.SweepOrg`): each policy is its own subscription. The
// first error is returned after every policy has been tried, so the tick is retried and
// a window still inside its lead is armed then.
//
// ⭐ A SWITCH THAT IS OFF IS AN UNSUBSCRIPTION, NOT A CONTROL HIT (owner ruling O1,
// 2026-10-05), as for an Incident (IncidentChanged): neither a switched-off Investigator
// nor a switched-off org is asked, and no row is written — a `skipped` row per window, 288
// a day for a five-minute window, would be noise, and the switch is itself the readable
// record. The org's daily budget IS recorded, as `skipped`/`budget`, like any request's.
func (s *Service) ArmDigestInvestigations(ctx context.Context, scope db.TenantScope) (int, error) {
	if err := db.RequireScope(scope); err != nil {
		return 0, err
	}
	now := s.now()
	due, err := s.digests.SummarisedDigests(ctx, scope, now)
	if err != nil {
		return 0, err
	}
	if len(due) == 0 {
		return 0, nil
	}
	if controls, err := s.orgControls.InvestigationControls(ctx, scope); err != nil {
		return 0, err
	} else if !controls.Enabled {
		return 0, nil
	}
	var (
		armed  int
		failed error
	)
	for _, d := range due {
		ok, err := s.armDigest(ctx, scope, d, now)
		if err != nil {
			if failed == nil {
				failed = err
			}
			continue
		}
		if ok {
			armed++
		}
	}
	return armed, failed
}

// armDigest arms one window's run if it is due and has none.
func (s *Service) armDigest(ctx context.Context, scope db.TenantScope, d domain.SummarisedDigest, now time.Time) (bool, error) {
	investigator, err := s.investigators.Get(ctx, scope, d.InvestigatorID)
	if errs.IsKind(err, errs.KindNotFound) {
		return false, nil // gone between the two reads; the policy's column clears with it
	}
	if err != nil {
		return false, err
	}
	if !investigator.Enabled {
		return false, nil
	}
	if !d.Window.Armable(now, domain.DigestLead(d.Window.Length(), investigator.Budgets.MaxWall)) {
		return false, nil
	}
	existing, err := s.investigations.DigestRun(ctx, scope, d.PolicyID, d.Window)
	if err != nil {
		return false, err
	}
	if existing != nil {
		return false, nil // armed already: one run per window, whatever became of it
	}
	by, err := domain.NewRequester(uuid.Nil, digestTriggerLabel(d))
	if err != nil {
		return false, err
	}
	_, err = s.request(ctx, scope, subjectRef{kind: domain.SubjectDigest, id: d.PolicyID, window: d.Window},
		investigator.ID, by, domain.TriggerDigestWindow)
	if errs.IsKind(err, errs.KindConflict) {
		// Another tick armed it between the read above and this insert:
		// `investigations_digest_window_uniq` is the arbiter, and its answer is "armed".
		return false, nil
	}
	return err == nil, err
}

// digestTriggerLabel is who "asked" for a digest window's run: oto, and why.
func digestTriggerLabel(d domain.SummarisedDigest) string {
	return fmt.Sprintf("oto: the digest window of policy %s closes at %s",
		d.PolicyName, d.Window.End.UTC().Format(time.RFC3339))
}

// DigestFinding is the run a digest sent NOW may carry for one window, and false when it
// may carry none — no run was armed, or the run is queued, running, failed or skipped.
//
// ⛔ IT IS ASKED ONCE, AT THE SEND, AND IT NEVER WAITS. The caller is the digest tick, and
// the answer is whatever the run is at that instant (domain.Investigation.CarriedByDigest).
func (s *Service) DigestFinding(
	ctx context.Context, scope db.TenantScope, policyID uuid.UUID, window domain.DigestWindow,
) (domain.Investigation, bool, error) {
	if err := db.RequireScope(scope); err != nil {
		return domain.Investigation{}, false, err
	}
	run, err := s.investigations.DigestRun(ctx, scope, policyID, window)
	if err != nil || run == nil {
		return domain.Investigation{}, false, err
	}
	if !run.CarriedByDigest() {
		return domain.Investigation{}, false, nil
	}
	return *run, true, nil
}

// digestJSON is a digest window as the run reads it.
func digestJSON(d domain.DigestSubject) map[string]any {
	type digestCase struct {
		CaseID    string            `json:"case_id"`
		Alertname string            `json:"alertname"`
		Labels    map[string]string `json:"labels"`
		StartedAt any               `json:"started_at"`
	}
	cases := make([]digestCase, 0, len(d.Cases))
	for _, c := range d.Cases {
		cases = append(cases, digestCase{CaseID: c.CaseID.String(), Alertname: c.Alertname,
			Labels: c.Labels, StartedAt: timeOrNil(c.StartedAt)})
	}
	return map[string]any{
		"policy_id":      d.PolicyID.String(),
		"policy_name":    d.PolicyName,
		"window_start":   timeOrNil(d.Window.Start),
		"window_end":     timeOrNil(d.Window.End),
		"cases":          cases,
		"cases_unlisted": d.Unlisted,
	}
}

// renderDigestSubject is the user message for a digest window: what the summary is for,
// and the window's Cases as oto recorded them when the run began.
func renderDigestSubject(d domain.DigestSubject) string {
	b, err := json.Marshal(digestJSON(d))
	if err != nil {
		b = []byte(`{}`)
	}
	return fmt.Sprintf("Summarise the digest window of notification policy %q, from %s up to %s (UTC, the end "+
		"exclusive). Your Finding becomes the body of the digest oto sends when the window closes, if you have "+
		"finished by then; if you have not, the digest is sent without it. The window may still be open, so "+
		"Cases can open in it after this message: %s reads it again. These are the Cases the policy selected so "+
		"far:\n%s", d.PolicyName, d.Window.Start.UTC().Format(time.RFC3339), d.Window.End.UTC().Format(time.RFC3339),
		ToolDigestCases, b)
}
