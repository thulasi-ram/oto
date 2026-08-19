package service

import (
	"testing"

	"github.com/thulasiram/oto/internal/notification/domain"
)

// TestTheMatcherSeesTheFocusedAlertsOwnLabels is git-bug 7570090's declared
// prerequisite, landed ahead of the change that needs it.
//
// `alert_groups` is to be replaced by a `group_by` column on
// `notification_policies`, with the collapse key computed at delivery. That is
// impossible while a policy is evaluated against the group's two axes —
// `alertname`, and `namespace` when the alert has one — because a policy cannot
// group by a label the matcher was never handed. Landing `group_by` on the old
// input would ship a knob over labels nothing can see.
//
// ⭐ THE PROPERTY THAT MATTERS IS MONOTONICITY, and it is asserted here rather
// than argued in a comment. The focus's labels are a SUPERSET of the group's for
// that alert, because the group axes are derived from them — so every matcher
// that matched before still matches, and the only new outcomes are matches that
// were previously impossible to express. That is what makes this safe to land on
// a path whose failure mode is a silently deleted notification: a policy that
// stopped matching would suppress as `no_policy` rather than error.
func TestTheMatcherSeesTheFocusedAlertsOwnLabels(t *testing.T) {
	t.Parallel()

	group := map[string]string{"alertname": "PodRestart", "namespace": "prod"}

	t.Run("a non-axis label is reachable only through the focus", func(t *testing.T) {
		t.Parallel()

		snap := domain.Snapshot{
			Group: domain.GroupFacts{GroupLabels: group},
			Focus: &domain.AlertFacts{Labels: map[string]string{
				"alertname": "PodRestart",
				"namespace": "prod",
				"node":      "worker-7",
				"pod":       "api-abc123",
			}},
		}
		got := matchLabels(snap)

		// The case `group_by: [node]` needs and the old input could not serve.
		if got["node"] != "worker-7" {
			t.Errorf("node = %q, want worker-7 — a policy cannot group by a label the "+
				"matcher was never handed, which is why this is a prerequisite", got["node"])
		}
		if got["pod"] != "api-abc123" {
			t.Errorf("pod = %q, want api-abc123", got["pod"])
		}
	})

	t.Run("every group axis survives, so no policy stops matching", func(t *testing.T) {
		t.Parallel()

		// ⛔ THE REGRESSION THIS GUARDS. A policy written against the group's axes
		// predates this change and must be unaffected by it. If widening the input
		// could drop an axis, the failure would be a `no_policy` SUPPRESSION — a
		// filter that silently deletes notifications — not an error anyone sees.
		snap := domain.Snapshot{
			Group: domain.GroupFacts{GroupLabels: group},
			Focus: &domain.AlertFacts{Labels: map[string]string{"node": "worker-7"}},
		}
		got := matchLabels(snap)
		for k, v := range group {
			if got[k] != v {
				t.Errorf("group axis %q = %q, want %q — widening the matcher's input "+
					"must never narrow it", k, got[k], v)
			}
		}
	})

	t.Run("a group-scoped reason carries no focus and is unchanged", func(t *testing.T) {
		t.Parallel()

		// `all_resolved` and `new_alerts` are about the generation, not one member,
		// so there is no focused alert and the group's own labels are the whole
		// input — exactly as before this change.
		snap := domain.Snapshot{Group: domain.GroupFacts{GroupLabels: group}}
		got := matchLabels(snap)
		if len(got) != len(group) {
			t.Errorf("a focus-less intent was matched against %d labels, want the "+
				"group's %d", len(got), len(group))
		}
	})

	t.Run("an empty focus label set falls back rather than emptying the input", func(t *testing.T) {
		t.Parallel()

		snap := domain.Snapshot{
			Group: domain.GroupFacts{GroupLabels: group},
			Focus: &domain.AlertFacts{},
		}
		if got := matchLabels(snap); len(got) != len(group) {
			t.Errorf("a focus with no labels produced %d labels, want the group's %d — "+
				"an empty focus must not be able to empty the matcher's input", len(got), len(group))
		}
	})
}
