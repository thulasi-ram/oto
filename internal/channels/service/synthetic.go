package service

import (
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// SyntheticAlertName is the alertname the test card carries. It is deliberately
// obvious in a channel's history: an operator scrolling back six months must be
// able to tell a drill from an outage at a glance.
const SyntheticAlertName = "OtoChannelTest"

// SyntheticView builds the NotificationView the test card renders from.
//
// It is populated ENOUGH TO EXERCISE THE REAL RENDERER: a group with a title and
// counts, one member alert with labels and annotations, a case, a rule
// snapshot, the standard actions and the deep links. A sparse view would render a
// sparse card, which would pass validation that a real card might not — and the
// whole value of this endpoint is that a pass means the real path works.
//
// It is a pure function of (instance, now, baseURL): no I/O, no clock read, no
// randomness, so the golden output is stable and a test can pin it.
func SyntheticView(inst domain.Instance, now time.Time, baseURL string) *domain.NotificationView {
	base := strings.TrimRight(baseURL, "/")
	started := now.Add(-12 * time.Minute)

	labels := map[string]string{
		"alertname": SyntheticAlertName,
		"severity":  "warning",
		"cluster":   "oto-selftest",
		"namespace": "oto",
		"service":   "oto",
		"instance":  "oto-selftest-0:9100",
		"job":       "oto",
	}
	annotations := map[string]string{
		"summary": "This is a test card from oto. Nothing is wrong.",
		"description": "Sent by the channel test button. It was rendered by the same renderer, " +
			"validated by the same outbound checks and delivered over the same transport a real " +
			"alert uses, so seeing it means the real path works.",
	}

	alert := domain.AlertView{
		ID:                "00000000-0000-7000-8000-000000000001",
		AlertKey:          "otoselftest0000000000000000000000000000000000000000000000000000",
		SourceFingerprint: "0000000000000000",
		AlertName:         SyntheticAlertName,
		Severity:          "warning",
		Namespace:         "oto",
		Service:           "oto",
		ClusterKey:        "oto-selftest",
		Labels:            labels,
		Annotations:       annotations,
		State:             "firing",
		AckState:          "unacked",
		FirstSeenAt:       started,
		LastSeenAt:        now,
		TotalCases:        1,
	}

	view := &domain.NotificationView{
		Org:    domain.OrgRef{ID: inst.OrgID.String(), Slug: "oto", Name: "oto"},
		Reason: "fired",
		Group: domain.GroupView{
			ID:         "00000000-0000-7000-8000-000000000002",
			GroupKey:   "otoselftest",
			Generation: 1,
			Title:      SyntheticAlertName + " — channel test",
			Receiver:   "oto-webhook",
			GroupLabels: map[string]string{
				"alertname": SyntheticAlertName,
				"cluster":   "oto-selftest",
			},
			State:          "open",
			Severity:       "warning",
			FiringCount:    1,
			TotalCount:     1,
			FirstSeenAt:    started,
			LastActivityAt: now,
			SourceGroupKey: "{}:{alertname=\"" + SyntheticAlertName + "\"}",
			ClusterKey:     "oto-selftest",
		},
		Alerts: []domain.AlertView{alert},
		Focus:  &alert,
		Case: &domain.CaseView{
			ID:        "00000000-0000-7000-8000-000000000003",
			Seq:       1,
			State:     "firing",
			AckState:  "unacked",
			StartedAt: started,
			Duration:  now.Sub(started),
		},
		Rule: &domain.RuleView{
			SnapshotID:  "00000000-0000-7000-8000-000000000004",
			Fingerprint: strings.Repeat("0", 64),
			File:        "oto/selftest.yml",
			Group:       "oto",
			Name:        SyntheticAlertName,
			Expr:        "vector(1)",
			For:         5 * time.Minute,
			Labels:      map[string]string{"severity": "warning"},
			Annotations: map[string]string{"summary": annotations["summary"]},
			// A synthetic snapshot is not a recovered one. It says `unavailable` so
			// that nobody reading a channel's history mistakes it for evidence that
			// oto reached a Prometheus.
			Origin:          "unavailable",
			MatchConfidence: "none",
			CapturedAt:      started,
		},
		Actions:    syntheticActions(inst),
		Links:      syntheticLinks(base),
		RenderedAt: now,
	}
	return view
}

// syntheticActions builds the buttons the real card carries, so the test
// exercises the interactive path's payload size and layout too.
//
// Every button's `value` is an OPAQUE ID and never a payload (§H.8). These ones
// name a CASE that does not exist, which is exactly right: an interaction from a
// test card resolves to nothing and is acknowledged as a no-op.
//
// ⛔ IT USED TO SAY "a group that does not exist" (git-bug 7570090). AlertGroups
// are gone and `oto.ack` now carries a Case id — see `service.Cases` — so the
// sentence had to move even though the bytes did not. A synthetic id that names
// nothing in either vocabulary is the one value that is safe here whatever the
// button's subject is.
func syntheticActions(inst domain.Instance) []domain.Action {
	if inst.Capabilities&domain.CapInteractive == 0 {
		return nil
	}
	return []domain.Action{
		{
			ID:    "oto.ack",
			Label: "Acknowledge",
			Style: "primary",
			Value: "00000000-0000-7000-8000-000000000003",
		},
		{
			ID:    "oto.noop.runbook",
			Label: "Runbook",
			URL:   "https://github.com/otohq/oto",
			Value: "00000000-0000-7000-8000-000000000003",
		},
	}
}

// syntheticLinks builds the deep links. When no base URL is configured they are
// left empty rather than pointing at a placeholder host: a dead link on a test
// card is a support ticket.
func syntheticLinks(base string) domain.Links {
	if base == "" {
		return domain.Links{}
	}
	// `/groups/`, the same path `notification/service.links` mints for a real card:
	// a test card whose links pointed somewhere a real card never does would be
	// testing a shape the product does not build.
	return domain.Links{
		Group:    base + "/groups/00000000-0000-7000-8000-000000000002",
		Alert:    base + "/alerts/00000000-0000-7000-8000-000000000001",
		Timeline: base + "/groups/00000000-0000-7000-8000-000000000002/timeline",
	}
}

// SyntheticFactView is SyntheticView told as one chosen fact, for a payload
// mapping's test send (ADR 0055 §2). A mapping renders a different body per fact, so
// the operator picks which one to send — an Incident fact among them — and whatever
// the incident tool opens is unmistakably `OtoChannelTest`.
//
// A Case fact is the synthetic card with its reason changed; `digest` is a window of
// one; an Incident fact is a synthetic Incident whose one member is the synthetic
// Case, shaped the way `ViewService.incidentCard` builds one (a Reason, an
// IncidentView and a render time, and nothing else).
func SyntheticFactView(inst domain.Instance, now time.Time, baseURL, fact string) *domain.NotificationView {
	v := SyntheticView(inst, now, baseURL)
	v.Reason = fact
	switch {
	case fact == "digest":
		from := now.Add(-time.Hour)
		return &domain.NotificationView{
			Org: v.Org, Reason: fact, RenderedAt: now,
			Digest: &domain.DigestView{Count: 1, CoveredFrom: from, CoveredTo: now},
		}
	case domain.IncidentFact(fact):
		base := strings.TrimRight(baseURL, "/")
		drawn := now.Add(-10 * time.Minute)
		state, caseState := "active", "open"
		if fact == "quiet" {
			state, caseState = "quiet", "closed"
		}
		member := domain.IncidentMemberView{
			CaseID: v.Case.ID, CaseNumber: 1, CaseState: caseState,
			AlertID: v.Alerts[0].ID, AlertName: SyntheticAlertName,
			Labels:  v.Alerts[0].Labels,
			AddedAt: drawn, AddedBy: domain.IncidentAuthorView{Label: "oto channel test"},
		}
		if fact == "case_removed" {
			member.RemovedAt, member.RemovedByLabel = now, "oto channel test"
		}
		incident := &domain.IncidentView{
			ID:     "00000000-0000-7000-8000-000000000005",
			Number: 1,
			// ⚠️ EVERY TEST FACT IS SEQUENCE 1, whichever one the operator sends. A
			// test is one fact about a story that has no other, and a receiver that
			// drops a fact BELOW the highest it has seen must not start dropping the
			// second test it is sent because the first was numbered higher.
			Sequence: 1,
			State:    state,
			DrawnAt:  drawn,
			DrawnBy:  domain.IncidentAuthorView{Label: "oto channel test"},
			Members:  []domain.IncidentMemberView{member},
		}
		if base != "" {
			incident.Link = base + "/incidents/1"
		}
		return &domain.NotificationView{Org: v.Org, Reason: fact, Incident: incident, RenderedAt: now}
	}
	return v
}
