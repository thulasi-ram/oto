package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
)

// ⚠️ WHAT THIS FILE DOES NOT PROVE. It pins the ARITHMETIC of the harness against
// fixtures the author wrote. It cannot prove the axes are the right ones — only a
// run against a real `ingest_batches` corpus can do that, which is what
// `-dsn` is for. A green test here means the tool would report the truth about a
// corpus; it says nothing about what that truth is.

func load(t *testing.T) []Batch {
	t.Helper()
	batches, err := loadFixtures("testdata")
	require.NoError(t, err)
	require.Len(t, batches, 5, "three fixture files, five stored bodies")
	return batches
}

// TestAnalyse_Fixtures pins every number the report prints, because a harness
// whose counts are approximately right is a harness that cannot settle an
// argument.
func TestAnalyse_Fixtures(t *testing.T) {
	rep := Analyse(load(t))

	assert.Equal(t, 5, rep.Batches)
	assert.Equal(t, 0, rep.BatchesSkipped)
	// Twelve alert rows across five bodies, but only ten distinct alerts: the
	// `continue:true` pair is delivered twice. An Alertmanager payload is
	// CUMULATIVE MEMBERSHIP, so counting rows would let one re-sending group
	// dominate its own statistics.
	assert.Equal(t, 12, rep.AlertRows)
	assert.Equal(t, 10, rep.DistinctAlerts)
	assert.Equal(t, 3, rep.TruncatedAlerts, "alerts Alertmanager says it dropped are reported, never inferred")

	assert.Equal(t, 5, rep.UpstreamGroups)
	assert.Equal(t, 5, rep.DerivedGroups)
	assert.Equal(t, []int{5, 2, 2, 2, 1}, rep.UpstreamSizes)
	assert.Equal(t, []int{3, 2, 2, 2, 1}, rep.DerivedSizes)
	assert.Equal(t, 1, rep.DerivedSingleton)

	// The over-split: `group_by: [cluster]` put five alerts of two alertnames in
	// one envelope, and the derived key makes three threads of them. Two of those
	// threads are new, and that is the cost this harness exists to price.
	assert.Equal(t, 1, rep.OverSplit)
	assert.Equal(t, 2, rep.ExtraThreads)
	assert.Equal(t, 3, rep.WorstSplit.Into)
	assert.Equal(t, "sre-slack", rep.WorstSplit.Receiver)

	// One merge of each kind: `continue:true` (identical groupLabels, receiver the
	// only difference) and `group_by: [alertname, severity]` (a partition on an
	// axis oto refuses).
	assert.Equal(t, 1, rep.OverMergeIntended)
	assert.Equal(t, 1, rep.OverMergeSuspect)
	assert.Equal(t, 2, rep.WorstMerge.From)

	assert.Equal(t, alerts.GroupSplitAxes, rep.Axes,
		"a report filed as evidence must name the rule it measured")
}

// TestAnalyse_SeverityAndPodNeverSplit is the same claim `keys_test.go` makes in
// the kernel, restated where the consequence is measurable: the fixtures contain
// four alerts that differ only in `severity`, `pod` or `instance`, and they must
// not add a single thread.
func TestAnalyse_SeverityAndPodNeverSplit(t *testing.T) {
	batches := load(t)
	rep := Analyse(batches)

	// Re-run with every non-axis label stripped. The report's partitions must be
	// identical; only the alert IDENTITIES collapse, because those labels are alert
	// identity even though they are not group identity.
	stripped := make([]Batch, 0, len(batches))
	for _, b := range batches {
		nb := b
		nb.Payload.Alerts = nil
		for _, a := range b.Payload.Alerts {
			keep := map[string]string{}
			for _, axis := range alerts.GroupSplitAxes {
				if v, ok := a.Labels[axis]; ok {
					keep[axis] = v
				}
			}
			na := a
			na.Labels = keep
			nb.Payload.Alerts = append(nb.Payload.Alerts, na)
		}
		stripped = append(stripped, nb)
	}
	got := Analyse(stripped)

	assert.Equal(t, rep.DerivedGroups, got.DerivedGroups)
	assert.Equal(t, rep.UpstreamGroups, got.UpstreamGroups)
	assert.Equal(t, rep.OverSplit, got.OverSplit)
	assert.Equal(t, rep.ExtraThreads, got.ExtraThreads)
	assert.Less(t, got.DistinctAlerts, rep.DistinctAlerts,
		"stripping pod/instance/severity must collapse ALERTS, which is what proves they were carried")
}

// TestAnalyse_SkipsWhatItCannotKey refuses to guess. A batch with no cluster key
// has no first axis, and a row this tool cannot key is a row it must not count —
// silently normalising it is how a validation harness starts agreeing with
// whatever it is validating.
func TestAnalyse_SkipsWhatItCannotKey(t *testing.T) {
	good := load(t)[0]

	noCluster := good
	noCluster.ClusterKey = ""
	noOrg := good
	noOrg.OrgID = uuid.Nil
	badCluster := good
	badCluster.ClusterKey = "Prod EU" // fails clusters_key_ck

	rep := Analyse([]Batch{good, noCluster, noOrg, badCluster})
	assert.Equal(t, 1, rep.Batches)
	assert.Equal(t, 3, rep.BatchesSkipped)

	// An alert whose LABELS fail their bounds is skipped without discarding the
	// batch: on the real path that alert is a recorded ingest rejection, and the
	// rest of the envelope is still observed.
	unnamed := good
	unnamed.Payload.Alerts = append([]struct {
		Labels map[string]string `json:"labels"`
	}{{Labels: map[string]string{"severity": "critical"}}}, good.Payload.Alerts...)
	rep = Analyse([]Batch{unnamed})
	assert.Equal(t, 1, rep.Batches)
	assert.Equal(t, 0, rep.BatchesSkipped)
	assert.Equal(t, 5, rep.AlertRows, "the alert with no alertname is skipped, the other five are not")
}

// TestReport_WriteNamesItsOwnLimits keeps the warning in the output. A report
// pasted into a ticket without it reads as a measurement of production.
func TestReport_WriteNamesItsOwnLimits(t *testing.T) {
	var buf bytes.Buffer
	Analyse(load(t)).Write(&buf)
	out := buf.String()

	assert.Contains(t, out, "AS-YET UNVALIDATED")
	assert.Contains(t, out, "alertname, namespace")
	assert.Contains(t, out, "extra threads")
	assert.True(t, strings.Contains(out, "truncated upstream"),
		"truncation is unseeable and the report must say so rather than imply completeness")
}

// TestReport_JSONRoundTrips keeps the machine-readable form usable as evidence.
func TestReport_JSONRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, Analyse(load(t)).WriteJSON(&buf))
	assert.Contains(t, buf.String(), `"extra_threads": 2`)
	assert.Contains(t, buf.String(), `"over_merge_suspect": 1`)
}
