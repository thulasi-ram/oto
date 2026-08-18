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
// ⭐ AND THE LIVENESS CLAUSE IS ALREADY HERE. `o.state = 'firing'` is stricter
// than "still a member": `case_terminal_ended` makes membership `ended_at IS NULL`,
// which is `state IN ('firing','suppressed')`, and a suppressed episode is not one
// a reminder should be minted for. Nothing was lost when the join table went.
const unackedGroupsSQL = `
SELECT g.id, g.state_version, g.group_labels, min(o.started_at) AS unacked_since
  FROM alert_groups g
  JOIN alert_cases o ON o.group_id = g.id AND o.org_id = g.org_id
 WHERE g.org_id = $1
   AND g.closed_at IS NULL
   AND o.state = 'firing'
   AND o.ack_state = 'unacked'
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
