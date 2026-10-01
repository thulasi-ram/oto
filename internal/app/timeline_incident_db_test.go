package app

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	alertsrepo "github.com/thulasiram/oto/internal/alerts/repository"
	alertsservice "github.com/thulasiram/oto/internal/alerts/service"
	incidentsdomain "github.com/thulasiram/oto/internal/incidents/domain"
	incidentsrepo "github.com/thulasiram/oto/internal/incidents/repository"
	incidentsservice "github.com/thulasiram/oto/internal/incidents/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/test/harness"
)

// ⭐⭐ THE INCIDENT FACTS LAND ON THE MEMBER CASE'S OWN TIMELINE (ADR 0052,
// git-bug b2672a1).
//
// `incidents/service` narrates every membership change through a port, and its own
// tests use a recorder for that port — which is exactly the arrangement that let
// five §D.4.1 types go unwritten for months (see timeline_events_db_test.go). So
// this drives the REAL seam the container wires: the incidents service over its
// real repository, the production `timelineRecorder`, the real
// `AppendTimelineEvent`, and the rows read back out of `alert_events`.

type incidentTimelineRig struct {
	h      *harness.H
	org    harness.Org
	cl     harness.Cluster
	svc    *incidentsservice.Service
	alerts *alertsservice.Service
	alice  incidentsdomain.Attribution
}

func newIncidentTimelineRig(t *testing.T) *incidentTimelineRig {
	t.Helper()
	h := harness.New(t)
	org := h.Org()

	alerts, err := alertsservice.New(alertsservice.Deps{
		Alerts:     alertsrepo.NewAlertRepository(h.Pool, h.Clock, false),
		Cases:      alertsrepo.NewCaseRepository(h.Pool),
		Events:     alertsrepo.NewEventRepository(h.Pool, h.Clock),
		Snoozes:    alertsrepo.NewSnoozeRepository(h.Pool, h.Clock),
		Tx:         alertsrepo.NewTxRunner(h.Pool),
		AlertBatch: alertsrepo.NewAlertRepository(h.Pool, h.Clock, false),
		OccBatch:   alertsrepo.NewCaseRepository(h.Pool),
		Clock:      h.Clock,
		Logger:     quietLogger(),
	})
	require.NoError(t, err)

	// ⭐ THE PRODUCTION ADAPTER, BOUND EXACTLY AS THE CONTAINER BINDS IT.
	recorder := &timelineRecorder{}
	recorder.svc = alerts

	svc, err := incidentsservice.New(incidentsservice.Deps{
		Incidents: incidentsrepo.NewIncidentRepository(h.Pool),
		Tx:        incidentsrepo.NewTxRunner(h.Pool),
		Timeline:  recorder,
		Clock:     h.Clock,
	})
	require.NoError(t, err)

	user := h.User(org)
	alice, err := incidentsdomain.Human(user.ID, "alice")
	require.NoError(t, err)
	return &incidentTimelineRig{h: h, org: org, cl: h.Cluster(org), svc: svc, alerts: alerts, alice: alice}
}

func (r *incidentTimelineRig) openCase(t *testing.T, alertname string) harness.Case {
	t.Helper()
	return r.h.Case(r.h.AlertWith(r.org, r.cl, map[string]string{
		"alertname": alertname, "severity": "critical", "service": "checkout",
	}))
}

// incidentRows reads the case's `incident.*` rows straight out of the table.
func (r *incidentTimelineRig) incidentRows(t *testing.T, caseID uuid.UUID) []struct{ typ, summary, kind, label string } {
	t.Helper()
	rows, err := r.h.Pool.Query(context.Background(), `
		SELECT type, summary, actor_kind, COALESCE(actor_label, '')
		  FROM alert_events
		 WHERE org_id = $1 AND case_id = $2 AND type LIKE 'incident.%'
		 ORDER BY recorded_at, id`, r.org.ID, caseID)
	require.NoError(t, err)
	defer rows.Close()
	var out []struct{ typ, summary, kind, label string }
	for rows.Next() {
		var row struct{ typ, summary, kind, label string }
		require.NoError(t, rows.Scan(&row.typ, &row.summary, &row.kind, &row.label))
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestEveryIncidentMembershipChangeIsOnTheMemberCasesTimeline(t *testing.T) {
	t.Parallel()
	r := newIncidentTimelineRig(t)
	ctx := context.Background()

	moving, staying := r.openCase(t, "HighErrorRate"), r.openCase(t, "KubePodCrashLooping")
	first, err := r.svc.Draw(ctx, r.org.Scope, []uuid.UUID{moving.ID, staying.ID}, r.alice)
	require.NoError(t, err)
	second, err := r.svc.Draw(ctx, r.org.Scope, []uuid.UUID{r.openCase(t, "DiskFull").ID}, r.alice)
	require.NoError(t, err)

	_, err = r.svc.Move(ctx, r.org.Scope, first.Number, second.Number, moving.ID, r.alice)
	require.NoError(t, err)
	_, err = r.svc.Remove(ctx, r.org.Scope, first.Number, staying.ID, r.alice)
	require.NoError(t, err)

	got := r.incidentRows(t, moving.ID)
	require.Len(t, got, 2, "drawn, then moved — one row per decision")
	assert.Equal(t, "incident.case_added", got[0].typ)
	assert.Equal(t, "Drawn into Incident #1 by alice", got[0].summary)
	assert.Equal(t, "incident.case_moved", got[1].typ)
	assert.Equal(t, "Moved from Incident #1 to #2 by alice", got[1].summary)
	for _, row := range got {
		assert.Equal(t, "user", row.kind, "a human decided, so a human is the actor")
		assert.Equal(t, "alice", row.label)
	}

	got = r.incidentRows(t, staying.ID)
	require.Len(t, got, 2)
	assert.Equal(t, "incident.case_removed", got[1].typ)
	assert.Equal(t, "Removed from Incident #1 by alice", got[1].summary)

	// ⭐ AND THE CASE TIMELINE THE API SERVES READS THEM BACK — the closed enum on
	// the read path accepts what the write path wrote.
	window := db.TimeWindow{From: r.h.Now().Add(-time.Hour), To: r.h.Now().Add(time.Hour)}
	page, err := r.alerts.CaseTimeline(ctx, r.org.Scope, moving.ID, window, db.Keyset{Limit: 50})
	require.NoError(t, err)
	var types []string
	for _, ev := range page.Events {
		types = append(types, ev.Type().String())
	}
	assert.Contains(t, types, "incident.case_added")
	assert.Contains(t, types, "incident.case_moved")
}
