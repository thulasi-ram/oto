package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// UnackedGroup is one candidate for oto's ONE reminder stage.
//
// ⛔ It carries a group, a duration and the labels a policy matches on. It
// carries NO PERSON, no rota position and no stage index, and it never will
// (§G.9.1, SCOPE-BOUNDARY §5.2). The reminder is a fact about how long a SIGNAL
// has gone unacknowledged, delivered to the channels its policy already names.
// The moment this struct grows a "who" field, oto is an on-call product.
type UnackedGroup struct {
	GroupID      uuid.UUID
	StateVersion int
	GroupLabels  map[string]string
	// UnackedSince is the start of the OLDEST member case that is still
	// firing and still unacknowledged.
	UnackedSince time.Time
}

// UnackedFor is how long this group has been unacknowledged as of now.
func (g UnackedGroup) UnackedFor(now time.Time) time.Duration {
	return now.Sub(g.UnackedSince)
}

// ReminderRepository serves the `notify.unacked_reminder` sweep.
type ReminderRepository struct {
	q db.Querier
}

// NewReminderRepository builds the repository over a fallback querier.
func NewReminderRepository(q db.Querier) *ReminderRepository { return &ReminderRepository{q: q} }

func (r *ReminderRepository) db(ctx context.Context) db.Querier { return db.FromContext(ctx, r.q) }

// unackedGroupsSQL joins the generation to its members through
// `alert_cases.group_id`, which since migration 00051 IS the membership —
// there is no join table and no `left_at IS NULL` to carry.
//
// ⭐ AND THE LIVENESS CLAUSE IS ALREADY HERE, IN TWO PARTS SINCE ADR 0040.
// `o.state = 'open'` is membership — `case_terminal_ended` makes it identical to
// `ended_at IS NULL` — and `a.state = 'firing'` is the STRICTER half: a suppressed
// episode is still a live member and is not one a reminder should be minted for.
//
// ⛔ THE SECOND HALF HAS TO COME FROM THE ALERT NOW, AND THE JOIN IS A PRIMARY-KEY
// PROBE. `alert_cases.state` is `open | closed` (migration 00054), so `firing`
// apart from `suppressed` is a fact about the label set rather than the episode.
// The join is safe on exactly the rows this asks about: an OPEN case is its
// alert's current one (case_one_open_idx is UNIQUE (alert_id) WHERE ended_at IS
// NULL), so `a.state` IS this episode's state, and `o.state = 'open'` is what
// keeps every closed episode of a re-fired alert out of the answer.
const unackedGroupsSQL = `
SELECT g.id, g.state_version, g.group_labels, min(o.started_at) AS unacked_since
  FROM alert_groups g
  JOIN alert_cases o ON o.group_id = g.id AND o.org_id = g.org_id
  JOIN alerts a      ON a.id = o.alert_id AND a.org_id = o.org_id
 WHERE g.org_id = $1
   AND g.closed_at IS NULL
   AND o.state = 'open'
   AND a.state = 'firing'
   -- THE SILENCED ONES ARE STILL EXCLUDED, AND THIS IS NOW SAID OUT LOUD.
   -- Before ADR 0041, a.state = 'firing' excluded them as a SIDE EFFECT of
   -- 'suppressed' occupying the same column; the reminder's intent was always
   -- "nag about what somebody is still being paged for", and an operator who has
   -- silenced an alert has already answered the reminder. Every other reader of
   -- this column wanted the opposite and got the same accident, which is why the
   -- axis exists -- so the one place that genuinely means "firing and audible"
   -- has to write it down.
   AND a.suppression_reason IS NULL
   -- ⛔ AND A CASE HOLDING AN UPSTREAM RESOLVE IS NOT ONE TO NAG ABOUT EITHER.
   -- Since migration 00057 a resolve inside the retention window W does NOT close
   -- the episode: the close is HELD on resolve_pending_at and the case stays
   -- open, so a re-fire inside W lands in it instead of minting the next seq.
   --
   -- ⚠️ THE STATE COLUMN CANNOT SEE THAT, AND THAT IS WHY THIS LINE EXISTS. The
   -- deferral is written as a transition with From == To
   -- (internal/alerts/service/lifecycle.go), so alerts.state STAYS 'firing' for
   -- the whole window and a.state = 'firing' above cannot tell "still burning"
   -- apart from "resolved, close held". Without this predicate the held-open case
   -- keeps minting unacked reminders about an alert upstream has ALREADY called
   -- resolved -- exactly the noise W exists to remove, re-made by the one sweep
   -- that never heard about W. Case.ClosePending() is this same question in the
   -- domain, and case_pending_pair_ck makes the two pending columns agree, so
   -- naming the indexed one names both.
   AND o.resolve_pending_at IS NULL
   AND o.ack_state = 'unacked'
   -- ⛔ THE LITERAL 'alert_group' IS LOAD-BEARING AND IT IS TIED TO ONE ALLOCATION.
   -- This NOT EXISTS is the once-per-generation LATCH, and it can only find the
   -- reminder it is latching on if the reminder recorded the same subject it looks
   -- for. The unacked_reminder reason is allocated SubjectAlertGroup in
   -- internal/notification/domain/reason.go, so the pair written by notify.go is
   -- (alert_group, group_id) and that is what is probed here.
   --
   -- ⚠️ MOVING unacked_reminder TO ANOTHER SUBJECT WOULD SILENTLY UNLATCH IT.
   -- Nothing would fail: this clause would simply stop matching the row it wrote,
   -- every sweep would find the group again, and one reminder stage (§G.9.1) would
   -- become a nag every sweep interval -- the ladder oto refuses, built by accident.
   -- If that allocation ever changes, this literal changes with it, in the same
   -- commit. Correct as written; do not "generalise" it.
   AND NOT EXISTS (
         SELECT 1 FROM notifications n
          WHERE n.org_id = g.org_id
            AND n.subject_kind = 'alert_group'
            AND n.subject_id = g.id
            AND n.reason = $4)
 GROUP BY g.id, g.state_version, g.group_labels
HAVING min(o.started_at) <= $2
 ORDER BY unacked_since ASC
 LIMIT $3`

// ListUnackedGroups returns open groups whose oldest firing, unacknowledged
// member started before `before`.
//
// `before` is `now - the SHORTEST threshold any live policy asks for`, so one
// query serves every policy and the service does the per-policy comparison. The
// alternative — one query per policy — would scan the same rows N times to
// answer the same question.
//
// The NOT EXISTS is what makes the reminder fire AT MOST ONCE PER GROUP
// GENERATION (§G.9). It is a query, not a flag, because a flag would have to be
// cleared by somebody and the somebody is always missing.
func (r *ReminderRepository) ListUnackedGroups(
	ctx context.Context, s db.TenantScope, before time.Time, limit int,
) ([]UnackedGroup, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.db(ctx).Query(ctx, unackedGroupsSQL,
		s.OrgID(), before, limit, string(domain.ReasonUnackedReminder))
	if err != nil {
		return nil, mapErr(err, "group_not_found", "list unacknowledged groups")
	}
	defer rows.Close()

	out := make([]UnackedGroup, 0, 16)
	for rows.Next() {
		var (
			g      UnackedGroup
			labels []byte
		)
		if err := rows.Scan(&g.GroupID, &g.StateVersion, &labels, &g.UnackedSince); err != nil {
			return nil, mapErr(err, "group_not_found", "scan an unacknowledged group")
		}
		g.GroupLabels = decodeStringMap(labels)
		out = append(out, g)
	}
	return out, mapErr(rows.Err(), "group_not_found", "read unacknowledged groups")
}
