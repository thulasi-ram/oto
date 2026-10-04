package domain

// A DIGEST WINDOW AS AN INVESTIGATION SUBJECT (ADR 0053 §2, §4; git-bug 3e96f5a).
//
// "Subjects are case | incident | digest | policy"; a digest's Finding becomes its body
// for the windows a policy asked for. The policy asks by naming an Investigator
// (`notification_policies.digest_investigator_id`), and that Investigator's run for a
// window is armed AHEAD of the window's close, by DigestLead.
//
// ⛔⛔ THE DIGEST NEVER WAITS ON THE RUN. The digest tick sends a closed window when it
// closes, with whatever usable Finding exists for it AT THAT MOMENT (CarriedByDigest) and
// the built-in body otherwise — the run queued, running, failed, skipped, or not armed
// at all. Nothing holds a digest for a run, retries one, or amends one when a Finding
// arrives late: a late Finding stays on its run and is posted nowhere. And a Finding is
// never an input to WHETHER a digest is sent (ADR 0053 §2) — only to what it says.

import (
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// DigestWindow is one closed-to-be digest window: `[Start, End)`, aligned in UTC by
// the notification policy's own window arithmetic, which `internal/app` hands over —
// this module does not re-derive a boundary the digest tick owns.
type DigestWindow struct {
	Start time.Time
	End   time.Time
}

// NewDigestWindow builds a window, refusing one that is empty or reversed
// (`investigations_digest_window_ck`).
func NewDigestWindow(start, end time.Time) (DigestWindow, error) {
	if start.IsZero() || !end.After(start) {
		return DigestWindow{}, errs.Newf(errs.KindInternal, "digest_window_invalid",
			"a digest window runs forward: [%s, %s) is not one", start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
	}
	return DigestWindow{Start: start.UTC(), End: end.UTC()}, nil
}

// IsZero reports whether this is no window at all — every non-digest subject.
func (w DigestWindow) IsZero() bool { return w.Start.IsZero() && w.End.IsZero() }

// Length is the window's span.
func (w DigestWindow) Length() time.Duration { return w.End.Sub(w.Start) }

// DigestLeadSlack is what DigestLead adds to an Investigator's wall-time budget: one
// minute for the once-a-minute `investigations.digest` tick to notice the window is due,
// and one for the run's job to be picked up and begin.
const DigestLeadSlack = 2 * time.Minute

// DigestLead is how long before a window's close its run is armed.
//
// ⭐ THE INVESTIGATOR'S OWN WALL-TIME BUDGET SETS IT, because that budget is the longest
// the run can take: a run armed `wall + slack` ahead of the close ends — completed, or
// exhausted with a partial Finding — before the window closes, unless it waited on the
// org's concurrency. Arming it any earlier would summarise less of the window for no
// gain; arming it later would let a well-behaved run miss the send.
//
// ⚠️ CAPPED AT HALF THE WINDOW. A five-minute window with a twenty-minute budget would
// otherwise arm its run before the window began, summarising a window with nothing in it
// yet. Under the cap a run may be too slow for its window; then the digest is sent on
// time with the built-in body, which is the binding rule working, not a failure.
func DigestLead(window, wall time.Duration) time.Duration {
	lead := wall + DigestLeadSlack
	if half := window / 2; lead > half {
		lead = half
	}
	if lead < 0 {
		return 0
	}
	return lead
}

// ArmsAt is the instant a window's run is armed, given its lead.
func (w DigestWindow) ArmsAt(lead time.Duration) time.Time { return w.End.Add(-lead) }

// Armable reports whether, at `now`, a window's run should be armed: its lead has
// begun and the window has not closed. A window already closed is the digest tick's,
// and a run armed for it now could only produce a Finding nothing will carry.
func (w DigestWindow) Armable(now time.Time, lead time.Duration) bool {
	return !now.Before(w.ArmsAt(lead)) && now.Before(w.End)
}

// SummarisedDigest is one notification policy that asked for its digest to be
// summarised, and the window of it open now: what the arming tick decides on. Built in
// `internal/app` from the notification policy, whose window arithmetic it uses.
type SummarisedDigest struct {
	PolicyID   uuid.UUID
	PolicyName string
	// InvestigatorID is the Investigator the policy named.
	InvestigatorID uuid.UUID
	Window         DigestWindow
}

// DigestSubject is a digest window as an Investigation is told about it: the policy,
// the window, and the Cases its matchers selected that opened inside the window so far.
//
// ⛔ A COPY, NOT THE NOTIFICATION MODULE'S TYPES (see subject.go's header): `investigator`
// imports no other module, and what a run can see is exactly what is spelled here.
type DigestSubject struct {
	PolicyID   uuid.UUID
	PolicyName string
	Window     DigestWindow
	// Cases are the window's Cases so far, oldest first, at most MaxDigestCases.
	Cases []DigestCase
	// Unlisted is how many more there were.
	Unlisted int
}

// MaxDigestCases bounds how many of a window's Cases a run is handed at once; the rest
// are counted (DigestSubject.Unlisted).
const MaxDigestCases = 200

// DigestCase is one Case a digest window selected, as the digest itself counts it: the
// episode, when it opened, and the labels the policy's matchers were evaluated against
// (`alertname`, and `namespace` when the alert has one — ADR 0038's axes).
type DigestCase struct {
	CaseID    uuid.UUID
	Alertname string
	Labels    map[string]string
	StartedAt time.Time
}

// CarriedByDigest reports whether this run's Finding is one a digest sent NOW may carry:
// a digest run that has ended with a Finding — `completed`, or `exhausted` with the
// partial Finding it reached (ADR 0053 §6 keeps it, and the card says "partial").
//
// ⛔ EVERY OTHER STATE IS THE BUILT-IN BODY, AND NONE OF THEM IS WAITED FOR: `queued` and
// `running` have nothing yet, `failed` and `skipped` will never have anything. The caller
// asks once, at the send, and takes the answer.
func (i Investigation) CarriedByDigest() bool {
	if i.SubjectKind != SubjectDigest || i.Finding == "" {
		return false
	}
	return i.Status == StatusCompleted || i.Status == StatusExhausted
}
