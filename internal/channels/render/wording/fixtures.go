package wording

import (
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// A Fixture is one NotificationView shape a Wording must survive.
//
// ⭐ SAVING RENDERS AGAINST ALL OF THEM, WHICH IS THE POINT. Liquid reports an
// unknown filter at RENDER time, not parse time, so a save that only parsed would
// accept `{{ x | no_such_filter }}` and discover it at 03:00 on a real card. And a
// template that works on a rich firing notification frequently breaks on the sparse
// ones — a resolved card with no rule, a digest with no group, an alert with no
// labels — which are exactly the cards an operator is reading when something is
// wrong.
type Fixture struct {
	Name  string
	Input StanzaInput
	// Representative marks the ORDINARY cards. A Wording that renders empty on one
	// of these is refused at save time; rendering empty on a hostile fixture is
	// expected and is handled at delivery by falling back to oto's own text.
	Representative bool
}

var fixtureClock = time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC)

// Fixtures is the corpus. It is deliberately small and deliberately nasty.
func Fixtures() []Fixture {
	return []Fixture{
		{Name: "firing", Representative: true, Input: BuildInput(firingView(), fixtureClock.Add(23*time.Minute))},
		{Name: "resolved", Representative: true, Input: BuildInput(resolvedView(), fixtureClock.Add(2*time.Hour))},
		{Name: "digest", Representative: true, Input: BuildInput(digestView(), fixtureClock)},
		{Name: "empty-labels", Input: BuildInput(emptyView(), fixtureClock)},
		{Name: "oversized-annotation", Input: BuildInput(oversizedView(), fixtureClock)},
		{Name: "hostile-text", Input: BuildInput(hostileView(), fixtureClock)},
		{Name: "zero-value", Input: BuildInput(&domain.NotificationView{}, time.Time{})},
	}
}

func firingView() *domain.NotificationView {
	v := &domain.NotificationView{
		Org:    domain.OrgRef{ID: "org", Slug: "acme", Name: "Acme"},
		Reason: "fired",
		Group: domain.GroupView{
			ID: "g1", GroupKey: "k", Generation: 1, Title: "HighErrorRate",
			Receiver: "platform", State: "open", Severity: "critical",
			GroupLabels:    map[string]string{"service": "checkout", "env": "prod"},
			FiringCount:    3,
			TotalCount:     4,
			AckedCount:     0,
			StartedAt:      fixtureClock,
			FirstSeenAt:    fixtureClock.Add(time.Minute),
			LastActivityAt: fixtureClock.Add(20 * time.Minute),
			ClusterKey:     "eu-west-1",
		},
		Alerts: []domain.AlertView{{
			ID: "a1", AlertName: "HighErrorRate", Severity: "critical",
			Service: "checkout", Namespace: "payments", ClusterKey: "eu-west-1",
			Labels:      map[string]string{"service": "checkout", "pod": "checkout-7f9"},
			Annotations: map[string]string{"summary": "Error rate above 5%", "runbook_url": "https://rb/x"},
			State:       "firing", AckState: "unacked",
			FirstSeenAt: fixtureClock, LastSeenAt: fixtureClock.Add(20 * time.Minute),
			TotalCases: 4, IsFlapping: true,
		}},
		Case: &domain.CaseView{
			ID: "c1", Seq: 4, State: "firing", AckState: "unacked",
			StartedAt: fixtureClock, Duration: 23 * time.Minute,
		},
		Rule: &domain.RuleView{
			SnapshotID: "s1", Name: "HighErrorRate", File: "rules.yml", Group: "http",
			Expr: `rate(errors[5m]) > 0.05`, For: 5 * time.Minute,
			Origin: "prometheus", MatchConfidence: "exact", CapturedAt: fixtureClock,
		},
		Enrichments: map[string]domain.EnrichmentView{
			"alert.history": {
				Enricher: "alert.history", Status: "ok", ComputedAt: fixtureClock,
				Payload: map[string]any{
					"cases_7d": 4, "flap_score": 0.62, "median_firing_seconds": 1380,
					// A nested value: dropped rather than stringified, because a
					// loop-free language could not read it and a Go map's print
					// order is not deterministic.
					"by_day": map[string]any{"mon": 2},
				},
			},
		},
		Trail:         []domain.TrailEntry{{Kind: "fired", At: fixtureClock}, {Kind: "refired", At: fixtureClock.Add(time.Hour)}},
		Links:         domain.Links{},
		Notifications: 2,
		RenderedAt:    fixtureClock.Add(23 * time.Minute),
	}
	return v
}

func resolvedView() *domain.NotificationView {
	v := firingView()
	v.Reason = "all_resolved"
	v.Group.State = "closed"
	v.Group.FiringCount, v.Group.ResolvedCount = 0, 4
	ended := fixtureClock.Add(2 * time.Hour)
	v.Case = &domain.CaseView{
		ID: "c1", Seq: 4, State: "resolved", AckState: "acked",
		StartedAt: fixtureClock, EndedAt: &ended, Duration: 2 * time.Hour,
		AckedByLabel: "on the platform channel", AckedAt: &ended, ResolveReason: "upstream",
	}
	// A terminal card with NO rule snapshot is the case SPEC §H.4 cares most about:
	// the card becomes the only remaining record exactly when it has least to say.
	v.Rule = nil
	v.Enrichments = nil
	return v
}

func digestView() *domain.NotificationView {
	return &domain.NotificationView{
		Org:    domain.OrgRef{ID: "org", Slug: "acme", Name: "Acme"},
		Reason: "digest",
		// ⚠️ A ZERO SPAN, ON PURPOSE. A digest written before migration 00070 has
		// no covered_from/covered_to, and a Wording must be able to say so rather
		// than print two zero timestamps.
		Digest:     &domain.DigestView{Count: 17},
		RenderedAt: fixtureClock,
	}
}

func emptyView() *domain.NotificationView {
	return &domain.NotificationView{
		Org:        domain.OrgRef{ID: "org", Slug: "acme", Name: "Acme"},
		Reason:     "fired",
		Group:      domain.GroupView{ID: "g", State: "open", StartedAt: fixtureClock},
		Alerts:     []domain.AlertView{{ID: "a"}},
		RenderedAt: fixtureClock,
	}
}

func oversizedView() *domain.NotificationView {
	v := firingView()
	big := make([]byte, 12000)
	for i := range big {
		big[i] = 'x'
	}
	v.Alerts[0].Annotations = map[string]string{"summary": string(big)}
	return v
}

// hostileView carries the strings an attacker would try, so a Wording that passes
// validation has been rendered against them at least once.
func hostileView() *domain.NotificationView {
	v := firingView()
	v.Alerts[0].Annotations = map[string]string{
		"summary": "<!channel> <!here> <@U024BE7LH> <!subteam^SAZ94GDB8> @everyone @here",
		// A forged mark: if sanitise ever stops running, this is the fixture that
		// notices, because the private-use codepoints would reach a Dialect and be
		// spelled as real markup.
		"forged":  "not code 999fake",
		"bidi":    "safe‮txet desrever‬",
		"control": "a\x00b\x07c",
	}
	v.Alerts[0].Labels = map[string]string{"weird key!@#": "v", "": "empty-key"}
	v.Actor = &domain.ActorView{Kind: "user", Label: "<!channel>"}
	v.Comment = "@everyone deploy now"
	return v
}
