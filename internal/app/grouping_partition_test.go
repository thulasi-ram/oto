package app

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	alertsdomain "github.com/thulasiram/oto/internal/alerts/domain"
)

var (
	testSourceA  = uuid.MustParse("018f3a4b-0000-7000-8000-0000000000a1")
	testSourceB  = uuid.MustParse("018f3a4b-0000-7000-8000-0000000000a2")
	testClusterA = uuid.MustParse("018f3a4b-0000-7000-8000-0000000000c1")
)

func labelSet(t *testing.T, in map[string]string) alertsdomain.LabelSet {
	t.Helper()
	ls, err := alertsdomain.NewLabelSet(in)
	require.NoError(t, err)
	return ls
}

func clusterKey(t *testing.T, s string) alertsdomain.ClusterKey {
	t.Helper()
	k, err := alertsdomain.NewClusterKey(s)
	require.NoError(t, err)
	return k
}

// ⭐ THE DEFECT THIS FILE EXISTS FOR (ticket bc691fa, ADR 0038): "two ingest
// paths, two different answers to which thread this belongs to".
//
// `partitionByGroup` is the composition root's copy of the §C.4 identity — it
// decides which observations are resolved together and which `part[0]` speaks for
// them — so it is the one place where a divergence between the webhook path and
// the reconciler path would be invisible to `keys_test.go` and fatal in
// production.

// TestPartitionByGroup_IngestAndReconcilerAgree builds the SAME alert as each of
// the two producers builds it and asserts they partition together.
//
// The two Observations differ in every field the old key hashed: the webhook one
// carries a receiver, an Alertmanager groupKey and a source id; the reconciler
// one carries none of them and, being an HA replica, a different source id
// again. Under the old rule those were three different groups and three threads.
func TestPartitionByGroup_IngestAndReconcilerAgree(t *testing.T) {
	labels := map[string]string{
		"alertname": "KubePodCrashLooping",
		"namespace": "payments",
		"severity":  "critical",
		"pod":       "checkout-7f9c-2x4k",
	}

	fromWebhook := alertsdomain.Observation{
		Source:         alertsdomain.ObservedByIngest,
		SourceID:       testSourceA,
		ClusterID:      testClusterA,
		ClusterKey:     clusterKey(t, "prod-eu"),
		Labels:         labelSet(t, labels),
		Receiver:       "sre-slack",
		SourceGroupKey: `{}/{team="sre"}:{cluster="prod-eu"}`,
	}
	fromReconciler := alertsdomain.Observation{
		Source:     alertsdomain.ObservedByReconciler,
		SourceID:   testSourceB, // the OTHER replica of the same Alertmanager
		ClusterID:  testClusterA,
		ClusterKey: clusterKey(t, "prod-eu"),
		Labels:     labelSet(t, labels),
		// No receiver and no source group key: GET /api/v2/alerts returns neither.
	}

	assert.Equal(t, groupingKey(fromWebhook), groupingKey(fromReconciler),
		"the same alert must resolve to one group whichever path observed it")

	parts := partitionByGroup([]alertsdomain.Observation{fromWebhook, fromReconciler})
	require.Len(t, parts, 1, "one alert, one generation, one thread")
	assert.Len(t, parts[0], 2)
}

// TestPartitionByGroup_SplitsOnTheAxesAndNothingElse pins the whole rule at the
// level where it costs chat threads.
func TestPartitionByGroup_SplitsOnTheAxesAndNothingElse(t *testing.T) {
	obs := func(cluster string, labels map[string]string) alertsdomain.Observation {
		return alertsdomain.Observation{
			SourceID:   testSourceA,
			ClusterID:  testClusterA,
			ClusterKey: clusterKey(t, cluster),
			Labels:     labelSet(t, labels),
		}
	}

	// ⛔ ONE ENVELOPE IS NO LONGER ONE PARTITION. This is a `group_by: [cluster]`
	// webhook: Alertmanager sent one notification group and oto now makes three
	// generations of it — two alertnames, one of them in two namespaces.
	coarse := []alertsdomain.Observation{
		obs("prod-eu", map[string]string{"alertname": "KubePodCrashLooping", "namespace": "payments", "pod": "a-1"}),
		obs("prod-eu", map[string]string{"alertname": "KubePodCrashLooping", "namespace": "payments", "pod": "a-2", "severity": "warning"}),
		obs("prod-eu", map[string]string{"alertname": "KubePodCrashLooping", "namespace": "search", "pod": "b-1"}),
		obs("prod-eu", map[string]string{"alertname": "NodeDiskPressure", "instance": "10.0.0.4:9100"}),
	}
	parts := partitionByGroup(coarse)
	require.Len(t, parts, 3)
	assert.Len(t, parts[0], 2, "same alertname, same namespace, different pod and severity")
	assert.Len(t, parts[1], 1, "a second namespace is a second partition")
	assert.Len(t, parts[2], 1)

	// A different cluster is always a different group: `cluster` is the first axis
	// and it is what must now distinguish alerts that two receivers used to.
	crossCluster := partitionByGroup([]alertsdomain.Observation{
		obs("prod-eu", map[string]string{"alertname": "X", "namespace": "n"}),
		obs("prod-us", map[string]string{"alertname": "X", "namespace": "n"}),
	})
	assert.Len(t, crossCluster, 2)

	// An absent namespace is ITS OWN partition, not a match for every namespace and
	// not an error.
	nsAbsent := partitionByGroup([]alertsdomain.Observation{
		obs("prod-eu", map[string]string{"alertname": "X"}),
		obs("prod-eu", map[string]string{"alertname": "X", "namespace": "n"}),
		obs("prod-eu", map[string]string{"alertname": "X", "namespace": ""}),
	})
	require.Len(t, nsAbsent, 2)
	assert.Len(t, nsAbsent[0], 2, "an EMPTY namespace joins the absent partition, as alerts.namespace stores NULL for both")
	assert.Len(t, nsAbsent[1], 1)
}

// TestPartitionByGroup_PreservesFirstSeenOrder keeps the property the original
// function was written for: two runs of the same batch must resolve their groups
// in the same order, or two ingest workers racing one payload can deadlock on the
// `alert_groups` rows in opposite orders.
func TestPartitionByGroup_PreservesFirstSeenOrder(t *testing.T) {
	obs := func(name string) alertsdomain.Observation {
		return alertsdomain.Observation{
			SourceID:   testSourceA,
			ClusterID:  testClusterA,
			ClusterKey: clusterKey(t, "prod-eu"),
			Labels:     labelSet(t, map[string]string{"alertname": name}),
		}
	}
	in := []alertsdomain.Observation{obs("C"), obs("A"), obs("C"), obs("B"), obs("A")}

	for range 20 {
		parts := partitionByGroup(in)
		require.Len(t, parts, 3)
		assert.Equal(t, "C", parts[0][0].Labels.AlertName())
		assert.Equal(t, "A", parts[1][0].Labels.AlertName())
		assert.Equal(t, "B", parts[2][0].Labels.AlertName())
		assert.Len(t, parts[0], 2)
	}

	assert.Empty(t, partitionByGroup(nil))
	assert.Len(t, partitionByGroup(in[:1]), 1)
}

// TestGroupingKey_IsInjective is the partition's own §C.1 obligation. The key is
// rendered rather than hashed (org_id is not on an Observation), so its
// injectivity is this function's responsibility and not SHA-256's: two alerts
// that smear into one string are two incidents in one chat thread.
func TestGroupingKey_IsInjective(t *testing.T) {
	obs := func(cluster string, labels map[string]string) alertsdomain.Observation {
		return alertsdomain.Observation{
			ClusterKey: clusterKey(t, cluster),
			Labels:     labelSet(t, labels),
		}
	}
	cases := map[string]alertsdomain.Observation{
		"a/X":         obs("a", map[string]string{"alertname": "X"}),
		"a/X+n":       obs("a", map[string]string{"alertname": "X", "namespace": "n"}),
		"a/X+nn":      obs("a", map[string]string{"alertname": "X", "namespace": "nn"}),
		"a/Xn":        obs("a", map[string]string{"alertname": "Xn"}),
		"a/Xn+empty":  obs("a", map[string]string{"alertname": "Xn", "namespace": ""}),
		"ab/X":        obs("ab", map[string]string{"alertname": "X"}),
		"a-b/X":       obs("a-b", map[string]string{"alertname": "X"}),
		"ab/X+n":      obs("ab", map[string]string{"alertname": "X", "namespace": "n"}),
		"a/X-sep-n":   obs("a", map[string]string{"alertname": "X\x01n"}),
		"prod-eu/X+n": obs("prod-eu", map[string]string{"alertname": "X", "namespace": "n"}),
	}
	// "a/Xn+empty" is the ONE deliberate duplicate: an empty namespace is the
	// absent partition, so it must key exactly as "a/Xn" does.
	seen := map[string]string{}
	for name, o := range cases {
		k := groupingKey(o)
		if prev, dup := seen[k]; dup {
			assert.ElementsMatch(t, []string{"a/Xn", "a/Xn+empty"}, []string{prev, name},
				"the only permitted collision is empty-namespace onto absent-namespace")
			continue
		}
		seen[k] = name
	}
	assert.Len(t, seen, len(cases)-1)
}
