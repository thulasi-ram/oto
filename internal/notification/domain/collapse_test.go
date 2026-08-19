package domain_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// violatesField reports whether err carries any violation on field, whatever its
// code. The shared `violation` helper matches field AND code; here the code is
// the thing under test (`label_name` vs `duplicate` vs `max_items`), so pinning
// it would make each case assert its own implementation rather than the rule.
func violatesField(err error, field string) bool {
	for _, v := range errs.ViolationsOf(err) {
		if v.Field == field {
			return true
		}
	}
	return false
}

func policyGroupedBy(labels ...string) domain.Policy {
	return domain.Policy{
		ID:      uuid.MustParse("018f3a4b-0000-7000-8000-0000000000c1"),
		GroupBy: labels,
	}
}

// TestTwoAlertsSharingTheGroupByLabelCollapseTogether is the case git-bug
// 7570090 names as impossible today, asserted at the collapse key.
//
// ⛔ SCOPE, STATED SO THIS IS NOT READ AS MORE THAN IT IS. This asserts the KEY,
// not the conversation. A conversation is still identified through
// `alert_groups.id` inside `notifications.subject_id`; moving it onto
// `(conversation_kind, conversation_id)` is stage 3. When that lands, "same key"
// becomes "one conversation" and the assertion moves up a layer.
//
// The pair below differ on `alertname` — oto's own fixed identity axis, so they
// are DIFFERENT alerts with different group keys and, today, different threads.
// Under `group_by: [node]` they are one delivery, which is precisely the decision
// a stored group row computed from fixed axes cannot express.
func TestTwoAlertsSharingTheGroupByLabelCollapseTogether(t *testing.T) {
	t.Parallel()

	p := policyGroupedBy("node")

	diskFull := map[string]string{"alertname": "DiskFull", "namespace": "prod", "node": "worker-7"}
	memHigh := map[string]string{"alertname": "MemoryHigh", "namespace": "prod", "node": "worker-7"}
	elsewhere := map[string]string{"alertname": "DiskFull", "namespace": "prod", "node": "worker-9"}

	if got, want := p.CollapseKey(memHigh), p.CollapseKey(diskFull); got != want {
		t.Errorf("two alerts on the same node collapsed apart:\n  %s\n  %s\n"+
			"They differ on `alertname`, which is exactly why a group row keyed on "+
			"oto's fixed axes cannot put them in one conversation", got, want)
	}
	if p.CollapseKey(elsewhere) == p.CollapseKey(diskFull) {
		t.Error("the same alertname on a DIFFERENT node collapsed together — the " +
			"collapse would then ignore the one label the policy names")
	}
}

// TestAnEmptyGroupByIsNotAHashOfNothing guards the distinction the default rests
// on.
//
// Empty means "this policy does not collapse", and it is the shipped default. If
// it returned a hash, that hash would be a real, single, shared key — and every
// delivery the policy ever made would silently merge into one conversation. The
// failure would look like working code.
func TestAnEmptyGroupByIsNotAHashOfNothing(t *testing.T) {
	t.Parallel()

	p := domain.Policy{ID: uuid.MustParse("018f3a4b-0000-7000-8000-0000000000c2")}
	if got := p.CollapseKey(map[string]string{"node": "worker-7"}); got != "" {
		t.Errorf("an empty group_by produced %q, want the empty string", got)
	}
}

// TestAMissingGroupByLabelIsItsOwnBucket is the failure mode a "skip it" reading
// would produce.
//
// If a label the alert does not carry were skipped, every alert missing `node` —
// across unrelated services — would collapse into ONE conversation. That is the
// loudest possible way for a grouping decision to be wrong, so a missing label
// contributes an empty value and buckets separately.
func TestAMissingGroupByLabelIsItsOwnBucket(t *testing.T) {
	t.Parallel()

	p := policyGroupedBy("node")
	withNode := p.CollapseKey(map[string]string{"node": "worker-7"})
	without := p.CollapseKey(map[string]string{"alertname": "DiskFull"})
	otherWithout := p.CollapseKey(map[string]string{"alertname": "MemoryHigh"})

	if without == withNode {
		t.Error("an alert with no `node` collapsed with one that has it")
	}
	if without != otherWithout {
		t.Error("two alerts both missing `node` produced different keys — they are " +
			"the same bucket, and the bucket is 'no node'")
	}
	if !strings.HasPrefix(without, domain.CollapseKeyPrefix) {
		t.Errorf("key %q lacks the %q prefix that keeps it distinguishable from a "+
			"`gk_` group key", without, domain.CollapseKeyPrefix)
	}
}

// TestTheCollapseKeyDoesNotDependOnTheOrderAnOperatorTyped — `[node, pod]` and
// `[pod, node]` are one policy, so they must not produce two conversations.
func TestTheCollapseKeyDoesNotDependOnTheOrderAnOperatorTyped(t *testing.T) {
	t.Parallel()

	labels := map[string]string{"node": "worker-7", "pod": "api-abc"}
	if a, b := policyGroupedBy("node", "pod"), policyGroupedBy("pod", "node"); a.CollapseKey(labels) != b.CollapseKey(labels) {
		t.Error("group_by order changed the collapse key, so the same policy written " +
			"two ways would split one conversation in two")
	}
}

// TestTheCollapseKeyIsUnambiguouslyFramed — the length-prefix framing
// `ComputeGroupKey` uses, asserted rather than assumed.
//
// Without it, `{node: "ab", pod: "c"}` and `{node: "a", pod: "bc"}` concatenate to
// the same bytes and two different collapses hash alike.
func TestTheCollapseKeyIsUnambiguouslyFramed(t *testing.T) {
	t.Parallel()

	p := policyGroupedBy("node", "pod")
	if p.CollapseKey(map[string]string{"node": "ab", "pod": "c"}) ==
		p.CollapseKey(map[string]string{"node": "a", "pod": "bc"}) {
		t.Error("two different label sets hashed alike — the fields are not framed, " +
			"so a value can be split across a boundary and collide")
	}
}

// TestTwoPoliciesDoNotShareAConversationByGroupingAlike — the policy's own id is
// in the key, so two policies that both group by `node` still deliver separately.
// They may name different channels, and a shared key would cross-post.
func TestTwoPoliciesDoNotShareAConversationByGroupingAlike(t *testing.T) {
	t.Parallel()

	a := policyGroupedBy("node")
	b := domain.Policy{
		ID:      uuid.MustParse("018f3a4b-0000-7000-8000-0000000000c9"),
		GroupBy: []string{"node"},
	}
	labels := map[string]string{"node": "worker-7"}
	if a.CollapseKey(labels) == b.CollapseKey(labels) {
		t.Error("two policies grouping by the same label produced one key — they may " +
			"name different channels, so the conversation would cross-post")
	}
}

// TestGroupByIsValidatedOnTheWritePath — 00063's CHECK bounds cardinality only,
// deliberately, so the per-element grammar has to be proved here or it is proved
// nowhere.
func TestGroupByIsValidatedOnTheWritePath(t *testing.T) {
	t.Parallel()

	base := func(gb []string) domain.Policy {
		return domain.Policy{
			Name: "n", Priority: 100, GroupBy: gb,
			Reasons:    []domain.Reason{domain.ReasonFired},
			ChannelIDs: []uuid.UUID{uuid.New()},
		}
	}

	for _, tc := range []struct {
		name string
		gb   []string
		ok   bool
	}{
		{"empty is the default and is legal", nil, true},
		{"a plain label name", []string{"node"}, true},
		{"underscores and digits", []string{"_x9", "a_b_c"}, true},
		{"a dash is not a label name", []string{"my-label"}, false},
		{"a leading digit is not a label name", []string{"9lives"}, false},
		{"a dotted name is not a label name", []string{"a.b"}, false},
		{"the empty string is not a label name", []string{""}, false},
		{"a repeat would hash twice", []string{"node", "node"}, false},
		{"nine exceeds the bound", []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := base(tc.gb).Validate()
			bad := violatesField(err, "group_by")
			if tc.ok && bad {
				t.Errorf("group_by %v was refused: %v", tc.gb, err)
			}
			if !tc.ok && !bad {
				t.Errorf("group_by %v was accepted, so it reaches the database "+
					"as a name no label set can contain", tc.gb)
			}
		})
	}
}
