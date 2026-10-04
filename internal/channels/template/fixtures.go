package template

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
	Name string
	view *domain.NotificationView
	at   time.Time
	// Representative marks the ORDINARY cards. A template that renders empty on
	// one of these is refused at save time; rendering empty on a hostile fixture
	// is expected and is handled at delivery by falling back to oto's own card.
	//
	// ⚠️ THE DIGEST IS NOT REPRESENTATIVE, THOUGH IT IS AN ORDINARY MESSAGE. A
	// template's `source` reaches the ROOT card and its `reply_source` the thread
	// replies (ReplyFixtures); a digest is built by its own renderer and takes no
	// template in this version. The digest view
	// stays in the corpus as a ROBUSTNESS fixture, because a view with almost
	// every field absent is exactly what shakes out a template that assumed one.
	Representative bool
}

// Bind projects this fixture for one format.
//
// ⛔ IT CANNOT BE PRECOMPUTED, because the escaping is part of the binding and
// the escaping depends on the format: a card escapes every value for Markdown
// and a raw template must not. A fixture that memoised one format's bindings
// would validate `raw` templates against `card` values and pass things that
// break in production.
func (f Fixture) Bind(format Format) (Input, map[string]string) {
	return BuildInput(f.view, f.at, format)
}

var fixtureClock = time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC)

// Fixtures is the corpus. It is deliberately small and deliberately nasty.
func Fixtures() []Fixture {
	return []Fixture{
		{Name: "firing", Representative: true, view: firingView(), at: fixtureClock.Add(23 * time.Minute)},
		{Name: "resolved", Representative: true, view: resolvedView(), at: fixtureClock.Add(2 * time.Hour)},
		{Name: "digest", view: digestView(), at: fixtureClock},
		{Name: "empty-labels", view: emptyView(), at: fixtureClock},
		{Name: "oversized-annotation", view: oversizedView(), at: fixtureClock},
		{Name: "hostile-text", view: hostileView(), at: fixtureClock},
		{Name: "zero-value", view: &domain.NotificationView{}, at: time.Time{}},
	}
}

// ReplyFixtures is the corpus a template's REPLY body is previewed against: one
// fixture per reason a thread can be told about, each named for its reason.
//
// ⭐ NAMED FOR THE REASON BECAUSE THAT IS THE AUTHOR'S QUESTION. A reply body is
// one branch per reason, and "what does my body say for `acked`?" is answered by a
// row titled `acked` — including "nothing, so oto's own reply", which is the
// ordinary answer for every reason the body does not handle.
func ReplyFixtures() []Fixture {
	at := fixtureClock.Add(30 * time.Minute)
	snoozeUntil := fixtureClock.Add(4 * time.Hour)
	actor := &domain.ActorView{Label: "ram@example.com", Kind: "user"}

	reply := func(reason string, base func() *domain.NotificationView, edit func(*domain.NotificationView)) Fixture {
		v := base()
		v.Reason = reason
		if edit != nil {
			edit(v)
		}
		return Fixture{Name: reason, Representative: true, view: v, at: at}
	}
	return []Fixture{
		reply("all_resolved", resolvedView, nil),
		reply("acked", firingView, func(v *domain.NotificationView) { v.Actor = actor }),
		reply("unacked", firingView, func(v *domain.NotificationView) { v.Actor = actor }),
		reply("comment", firingView, func(v *domain.NotificationView) {
			v.Actor, v.Comment = actor, "rolling back the 14:02 deploy"
		}),
		reply("suppressed", firingView, nil),
		reply("unsuppressed", firingView, nil),
		reply("expired", resolvedView, nil),
		reply("snoozed", firingView, func(v *domain.NotificationView) {
			v.Actor, v.SnoozedUntil = actor, &snoozeUntil
		}),
		reply("unsnoozed", firingView, nil),
		reply("rule_changed", firingView, nil),
	}
}

func firingView() *domain.NotificationView {
	v := &domain.NotificationView{
		Org: domain.OrgRef{ID: "org", Slug: "acme", Name: "Acme"},
		// A real card carries deep links, so the corpus must too: a template that
		// says `[text]({{ links.group }})` has to be exercised with the link
		// PRESENT, or the save-time gate only ever proves the absent case.
		Links: domain.Links{
			Group:      "https://oto.example/g/g1",
			Alert:      "https://oto.example/a/a1",
			Timeline:   "https://oto.example/g/g1/timeline",
			Prometheus: "https://prom.example/graph?g0.expr=up",
			Runbook:    "https://wiki.example/runbooks/high-error-rate",
		},
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
		"forged":  "not code \ue000999fake\ue001",
		"bidi":    "safe\u202Etxet desrever\u202C",
		"control": "a\x00b\x07c",
	}
	v.Alerts[0].Labels = map[string]string{"weird key!@#": "v", "": "empty-key"}
	v.Actor = &domain.ActorView{Kind: "user", Label: "<!channel>"}
	v.Comment = "@everyone deploy now"
	return v
}

// View is the fixture's NotificationView. It is exposed for the payload-mapping
// gate, which renders the oto.notification.v1 envelope FROM it rather than binding it
// the way a template does (ADR 0055 §2); a caller must not mutate it.
func (f Fixture) View() *domain.NotificationView { return f.view }

// At is the instant the fixture is rendered at.
func (f Fixture) At() time.Time { return f.at }

// MappingFixtures is the corpus a webhook Connection's payload mapping is checked
// against before it may be saved (ADR 0055 §2, git-bug 2205620): ONE REPRESENTATIVE
// VIEW FOR EVERY FACT an envelope can carry, named for that fact, and then the hostile
// and zero-value shapes of a Case fact and of an Incident fact.
//
// ⭐ EVERY FACT, BECAUSE A MAPPING CANNOT DECLINE ONE. ADR 0055 grants a mapping no
// way to say "not this fact", so a body that only works for `drawn` is a body that
// kills every `fired` delivery on that Connection — and the place to find that out is
// the save, with the fact named, not the first page at 03:00.
//
// ⭐ THE HOSTILE SHAPES ARE WHAT PROVE THE ESCAPE. A label holding `"`, `\`, a newline
// and `</script>` must land as JSON string content in every body; a mapping that
// renders valid JSON for `firing` and broken JSON for a quote in a label is refused.
func MappingFixtures() []Fixture {
	at := fixtureClock.Add(30 * time.Minute)
	actor := &domain.ActorView{Label: "ram@example.com", Kind: "user"}
	snoozeUntil := fixtureClock.Add(4 * time.Hour)

	fact := func(reason string, base func() *domain.NotificationView, edit func(*domain.NotificationView)) Fixture {
		v := base()
		v.Reason = reason
		if edit != nil {
			edit(v)
		}
		return Fixture{Name: reason, Representative: true, view: v, at: at}
	}
	withActor := func(v *domain.NotificationView) { v.Actor = actor }
	incident := func(reason string) func() *domain.NotificationView {
		return func() *domain.NotificationView { return incidentFixtureView(reason) }
	}

	out := []Fixture{
		fact("fired", firingView, nil),
		fact("all_resolved", resolvedView, nil),
		fact("repeat", firingView, nil),
		fact("suppressed", firingView, nil),
		fact("unsuppressed", firingView, nil),
		fact("expired", resolvedView, nil),
		fact("refired", firingView, nil),
		fact("acked", firingView, withActor),
		fact("unacked", firingView, withActor),
		fact("snoozed", firingView, func(v *domain.NotificationView) {
			v.Actor, v.SnoozedUntil = actor, &snoozeUntil
		}),
		fact("unsnoozed", firingView, nil),
		fact("enriched", firingView, nil),
		fact("rule_changed", firingView, nil),
		fact("comment", firingView, func(v *domain.NotificationView) {
			v.Actor, v.Comment = actor, "rolling back the 14:02 deploy"
		}),
		fact("digest", digestView, nil),
		fact("drawn", incident("drawn"), nil),
		fact("case_added", incident("case_added"), nil),
		fact("case_removed", incident("case_removed"), nil),
		fact("quiet", incident("quiet"), func(v *domain.NotificationView) {
			v.Incident.State = "quiet"
			for i := range v.Incident.Members {
				v.Incident.Members[i].CaseState = "closed"
			}
		}),
		fact("active_again", incident("active_again"), nil),
		// The sixth Incident fact (ADR 0053 §4, git-bug 74ea849): an Investigation of
		// the Incident reached a new Finding, which the card carries.
		fact("finding", incident("finding"), func(v *domain.NotificationView) {
			v.Incident.Finding = &domain.IncidentFindingView{
				InvestigationID: "0199a1b2-c3d4-7e5f-8a9b-00000000f1d1", Investigator: "firstlook", Version: 2,
				Summary:     "The checkout deploy doubled the error rate; the crash loop began two minutes later.",
				ConcludedAt: fixtureClock.Add(25 * time.Minute),
			}
		}),
		// The six Remedy facts (ADR 0054 §2, git-bug 4148256): each carries the Remedy
		// transition it declares.
		fact("remedy_proposed", incident("remedy_proposed"), withRemedy("proposed", "investigator", "")),
		fact("remedy_approved", incident("remedy_approved"), withRemedy("approved", "user", "")),
		fact("remedy_declined", incident("remedy_declined"), withRemedy("declined", "user", "")),
		fact("remedy_expired", incident("remedy_expired"), withRemedy("expired", "system", "")),
		fact("remedy_executed", incident("remedy_executed"), withRemedy("executed", "system", "")),
		fact("remedy_failed", incident("remedy_failed"), withRemedy("failed", "system", "tool_error")),
	}

	hostileCase := hostileView()
	hostileCase.Alerts[0].Labels = map[string]string{
		"alertname": "quote\" backslash\\ newline\n </script> \u2028sep\u2029",
		"severity":  "critical",
	}
	// Hostile annotation KEYS, which a mapping binds as well as values: an alert's
	// annotation names are as writable as its labels, and `{{ alert.annotations }}`
	// renders them. One tries to close the string and add a command field; the other
	// spells a secret reference out of the sentinel separators.
	hostileCase.Alerts[0].Annotations["\",\"event_action\":\"resolve\",\"x\":\""] = "v"
	hostileCase.Alerts[0].Annotations["\u2028routing_key\u2029"] = "v"
	hostileIncident := incidentFixtureView("drawn")
	hostileIncident.Incident.DrawnBy.Label = "</script>\"\\\n<!channel>"
	hostileIncident.Incident.Members[0].AlertName = "a\"b\\c\nd\u2028e"
	hostileIncident.Incident.Members[0].Labels = map[string]string{"service": "{\"forged\":true}"}

	return append(out,
		Fixture{Name: "fired: hostile-text", view: hostileCase, at: at},
		Fixture{Name: "fired: empty-labels", view: emptyView(), at: fixtureClock},
		Fixture{Name: "fired: zero-value", view: &domain.NotificationView{Reason: "fired"}, at: time.Time{}},
		Fixture{Name: "drawn: hostile-text", view: hostileIncident, at: at},
		Fixture{Name: "drawn: zero-value", view: &domain.NotificationView{
			Reason: "drawn", Incident: &domain.IncidentView{},
		}, at: time.Time{}},
	)
}

// incidentFixtureView is an Incident fact the way `ViewService.incidentCard` builds
// one: a Reason, an IncidentView and a render time, and nothing else (ADR 0052 §5).
func incidentFixtureView(reason string) *domain.NotificationView {
	drawn := fixtureClock.Add(-20 * time.Minute)
	return &domain.NotificationView{
		Org:    domain.OrgRef{ID: "org", Slug: "acme", Name: "Acme"},
		Reason: reason,
		Incident: &domain.IncidentView{
			ID:      "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b",
			Number:  4,
			State:   "active",
			DrawnAt: drawn,
			DrawnBy: domain.IncidentAuthorView{Label: "Priya R."},
			Link:    "https://oto.example/incidents/4",
			Members: []domain.IncidentMemberView{
				{
					CaseID: "0199a1b2-0000-7000-8000-000000000412", CaseNumber: 412, CaseState: "open",
					AlertID: "a1", AlertName: "HighErrorRate",
					Labels:  map[string]string{"alertname": "HighErrorRate", "severity": "critical"},
					AddedAt: drawn, AddedBy: domain.IncidentAuthorView{Label: "Priya R."},
					Link: "https://oto.example/cases/412",
				},
				{
					CaseID: "0199a1b2-0000-7000-8000-000000000409", CaseNumber: 409, CaseState: "closed",
					AlertID: "a3", AlertName: "DiskFull",
					Labels:  map[string]string{"alertname": "DiskFull"},
					AddedAt: drawn, AddedBy: domain.IncidentAuthorView{Label: "Priya R."},
					RemovedAt: drawn.Add(10 * time.Minute), RemovedByLabel: "Sam K.", MovedToNumber: 7,
				},
			},
		},
		RenderedAt: fixtureClock.Add(30 * time.Minute),
	}
}

// withRemedy dresses an Incident fixture with the Remedy transition a `remedy_*` fact carries:
// a rollout restart on the checkout Deployment, approved by two people where it got that far.
func withRemedy(state, actorKind, failure string) func(v *domain.NotificationView) {
	return func(v *domain.NotificationView) {
		r := &domain.IncidentRemedyView{
			RemedyID: "0199a1b2-c3d4-7e5f-8a9b-00000000e1d1", InvestigationID: "0199a1b2-c3d4-7e5f-8a9b-00000000f1d1",
			State: state, ToolServer: "k8s-write", Tool: "rollout_restart",
			Arguments:       `{"namespace":"checkout","deployment":"api"}`,
			ArgumentsSHA256: "524bbf6f79faeb40f6b7bf21913d076e37ad70c570aef46870fa570113e95198",
			Target:          "Deployment checkout/api",
			Description:     "The 14:02 deploy left the api pods crash-looping; a restart picks up the reverted config.",
			ProposedBy:      "Investigator firstlook v2", RequiredApprovals: 2,
			Approvals: []domain.IncidentRemedyApprovalView{},
			ActorKind: actorKind, ActorLabel: "Investigator firstlook v2",
			At: fixtureClock.Add(30 * time.Minute), ExpiresAt: fixtureClock.Add(90 * time.Minute),
			FailureReason: failure,
		}
		if actorKind != "investigator" {
			r.ActorLabel = "Grace Hopper"
			if actorKind == "system" {
				r.ActorLabel = "oto"
			}
		}
		if state != "proposed" && state != "declined" && state != "expired" {
			r.Approvals = []domain.IncidentRemedyApprovalView{
				{Label: "Grace Hopper", ApprovedAt: fixtureClock.Add(31 * time.Minute)},
				{Label: "Ada Lovelace", ApprovedAt: fixtureClock.Add(32 * time.Minute)},
			}
		}
		if failure != "" {
			r.Detail = "the ToolServer answered: deployments.apps \"api\" is forbidden"
		}
		v.Incident.Remedy = r
	}
}
