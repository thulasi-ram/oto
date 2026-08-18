package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/grouping/domain"
	"github.com/thulasiram/oto/internal/grouping/repository"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ⭐ WHAT RESOLVE OWES THE REST OF THE PRODUCT (ticket bc691fa, ADR 0038).
//
// Two things, and the second is the one that used to be wrong quietly:
//
//  1. the §C.4 key is `(org, cluster, alertname, namespace-or-∅)`, derived from
//     the alert's own labels;
//  2. the generation's `group_labels` are THOSE SAME AXES — because every matcher
//     in oto is fed the group's labels, so a policy matching `namespace` used to
//     match nothing unless the operator had put `namespace` in `group_by`, and it
//     failed as a `no_policy` suppression rather than as an error.

func mustLabelSet(t *testing.T, in map[string]string) kernel.LabelSet {
	t.Helper()
	ls, err := kernel.NewLabelSet(in)
	if err != nil {
		t.Fatalf("NewLabelSet: %v", err)
	}
	return ls
}

func mustClusterKey(t *testing.T, s string) kernel.ClusterKey {
	t.Helper()
	k, err := kernel.NewClusterKey(s)
	if err != nil {
		t.Fatalf("NewClusterKey: %v", err)
	}
	return k
}

// capturingGroups is fakeGroups that remembers what it was asked to open, and
// answers "no open generation" so Resolve always takes the opening path.
type capturingGroups struct {
	*fakeGroups
	lookedUpKey string
	opened      repository.NewGeneration
}

func (c *capturingGroups) GetOpenByKey(
	_ context.Context, _ db.TenantScope, key string,
) (domain.Group, bool, error) {
	c.lookedUpKey = key
	return domain.Group{}, false, nil
}

func (c *capturingGroups) OpenGeneration(
	_ context.Context, _ db.TenantScope, in repository.NewGeneration,
) (domain.Group, error) {
	c.opened = in
	return c.group, nil
}

func newResolveHarness(t *testing.T) (*joinHarness, *capturingGroups) {
	t.Helper()
	h := newJoinHarness(t)
	capture := &capturingGroups{fakeGroups: h.groups}
	svc, err := New(Deps{
		Groups:   capture,
		Members:  h.members,
		Tx:       h.tx,
		Events:   h.events,
		Stream:   h.stream,
		Settings: h.settings,
		Enqueuer: h.enqueuer,
		Clock:    clock.NewFake(h.at),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.svc = svc
	return h, capture
}

func resolveRequest(t *testing.T, labels map[string]string) ResolveRequest {
	t.Helper()
	return ResolveRequest{
		SourceID:   uuid.New(),
		ClusterID:  uuid.New(),
		ClusterKey: mustClusterKey(t, "prod-eu"),
		Labels:     mustLabelSet(t, labels),
		// The provenance an Alertmanager webhook carries. NONE of it may reach the
		// key or the group's labels.
		Receiver:       "sre-slack",
		SourceGroupKey: `{}/{team="sre"}:{cluster="prod-eu"}`,
		At:             time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}
}

func TestResolve_KeyIsTheDerivedOne(t *testing.T) {
	h, groups := newResolveHarness(t)
	in := resolveRequest(t, map[string]string{
		"alertname": "KubePodCrashLooping",
		"namespace": "payments",
		"severity":  "critical",
		"pod":       "checkout-7f9c-2x4k",
	})

	if _, err := h.svc.Resolve(h.ctx, h.scope, in); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	want := kernel.ComputeGroupKey(h.scope.OrgID(), in.ClusterKey, in.Labels).String()
	if groups.lookedUpKey != want {
		t.Errorf("looked up %q, want the derived key %q", groups.lookedUpKey, want)
	}
	if groups.opened.GroupKey != want {
		t.Errorf("opened generation under %q, want %q", groups.opened.GroupKey, want)
	}

	// ⭐ THE MATCHER FIX. The group's labels are oto's axes, not Alertmanager's
	// groupLabels — which this request does not even carry any more.
	got := groups.opened.GroupLabels
	if len(got) != 2 || got["alertname"] != "KubePodCrashLooping" || got["namespace"] != "payments" {
		t.Errorf("group_labels = %v, want exactly the split axes", got)
	}
	if _, split := got["severity"]; split {
		t.Error("severity reached the group's labels; it is not an axis and must not become one")
	}

	// The title is the alert's NAME, not a serialised map: both axes are elided by
	// the card's own layout, so nothing is left to append.
	if groups.opened.Title != "KubePodCrashLooping" {
		t.Errorf("title = %q, want the alertname alone", groups.opened.Title)
	}

	// Provenance is still recorded — it is just not identity.
	if groups.opened.Receiver != "sre-slack" || groups.opened.SourceGroupKey != in.SourceGroupKey {
		t.Error("the Alertmanager provenance was dropped; it is not identity, but it is still evidence")
	}
}

// TestResolve_AbsentNamespaceIsItsOwnPartition — and is not an error, which is
// the half a nullable column keeps getting wrong.
func TestResolve_AbsentNamespaceIsItsOwnPartition(t *testing.T) {
	h, groups := newResolveHarness(t)
	in := resolveRequest(t, map[string]string{"alertname": "NodeDiskPressure", "instance": "10.0.0.4:9100"})

	if _, err := h.svc.Resolve(h.ctx, h.scope, in); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := groups.opened.GroupLabels; len(got) != 1 || got["alertname"] != "NodeDiskPressure" {
		t.Errorf("group_labels = %v, want {alertname} with no namespace key at all", got)
	}
	if _, present := groups.opened.GroupLabels["namespace"]; present {
		t.Error("∅ was written as a present key; it is the ABSENCE of one, which is what canon() makes injective")
	}

	withNS := resolveRequest(t, map[string]string{"alertname": "NodeDiskPressure", "namespace": "infra"})
	if kernel.ComputeGroupKey(h.scope.OrgID(), withNS.ClusterKey, withNS.Labels) ==
		kernel.ComputeGroupKey(h.scope.OrgID(), in.ClusterKey, in.Labels) {
		t.Error("an absent namespace shares a group with a present one")
	}
}

// TestResolve_RefusesWhatItCannotKey. Both inputs are guaranteed by §C.2 — an
// Observation without them could not have produced an `alert_key` either — so the
// failure is a layer-3 invariant, and it must be a VALIDATION error because the
// orchestrator degrades on exactly that kind: the alert is recorded without a
// group rather than the whole batch being retried forever.
func TestResolve_RefusesWhatItCannotKey(t *testing.T) {
	for name, mutate := range map[string]func(*ResolveRequest){
		"no cluster key": func(r *ResolveRequest) { r.ClusterKey = kernel.ClusterKey{} },
		"no labels":      func(r *ResolveRequest) { r.Labels = kernel.LabelSet{} },
		"no source":      func(r *ResolveRequest) { r.SourceID = uuid.Nil },
	} {
		t.Run(name, func(t *testing.T) {
			h, groups := newResolveHarness(t)
			in := resolveRequest(t, map[string]string{"alertname": "X"})
			mutate(&in)

			_, err := h.svc.Resolve(h.ctx, h.scope, in)
			if !errs.IsKind(err, errs.KindValidation) {
				t.Fatalf("err = %v, want a validation error the orchestrator can degrade on", err)
			}
			if groups.opened.GroupKey != "" {
				t.Error("a generation was opened for an observation with no computable key")
			}
		})
	}
}
