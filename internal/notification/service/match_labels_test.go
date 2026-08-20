package service

import (
	"testing"

	"github.com/thulasiram/oto/internal/notification/domain"
)

// TestTheMatcherSeesTheFocusedAlertsOwnLabels is git-bug 7570090 stage 1, and it
// OUTLIVED THE REASON IT WAS FILED FOR.
//
// It was landed as the prerequisite for a `group_by` column on
// `notification_policies`: a policy cannot collapse on `node` while the matcher is
// handed only the group's two axes. That column landed in 00063 and migration
// 00065 took it away again — the owner ruled one Case per conversation, so there
// is no collapse left to configure.
//
// ⭐ THE PROPERTY IS WORTH KEEPING ON ITS OWN, WHICH IS WHY THIS DID NOT GO WITH
// IT. Evaluating a policy against `alertname` and `namespace` alone is the ADR
// 0038 failure mode in a second place: a policy written against any other label
// matched nothing and failed as a `no_policy` suppression rather than as an error.
// A filter that silently deletes notifications is wrong whether or not anything
// downstream ever groups by the labels it can now see.
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

		// A label off the two axes, reachable only through the focus. This is the
		// case a policy matching `node` needs and the old input could not serve.
		if got["node"] != "worker-7" {
			t.Errorf("node = %q, want worker-7 — a policy cannot MATCH on a label the "+
				"matcher was never handed, and a policy that matches nothing suppresses "+
				"as `no_policy` rather than failing (ADR 0038)", got["node"])
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

	t.Run("a reason with no focus is matched on the conversation's labels alone", func(t *testing.T) {
		t.Parallel()

		// ⛔ THE SUB-TEST WAS NAMED FOR A "group-scoped reason" AND CITED `new_alerts`,
		// WHICH IS DELETED (git-bug `7570090`). The shape it covers survives and is now
		// the COMMON one rather than a group-level special case: `AlertID` is the FOCUS
		// and is optional, so `all_resolved`, `repeat` and `expired` all arrive without
		// one. With no focus to merge over, the conversation's own labels are the whole
		// matcher input.
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
