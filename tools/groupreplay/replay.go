// Package main is `groupreplay`, the harness that decides whether the derived
// group key's axes are the right ones.
//
// ⚠️⚠️ THE AXES ARE AS-YET UNVALIDATED AGAINST PRODUCTION PAYLOADS. ADR 0038
// changed `group_key` from `H(org, source_id, receiver, Alertmanager's
// groupLabels)` to `H(org, cluster_key, alertname, namespace-or-∅)` on a design
// argument. That argument has NOT been checked against a real corpus. Everything
// this tool has been run against so far is `testdata/`, which is fixtures the
// author wrote — fixtures cannot falsify a choice of axes, they can only show
// that the arithmetic is what it claims to be. Until someone points `-dsn` at a
// database holding real `ingest_batches`, "the axes are right" is an opinion.
//
// # WHAT IT MEASURES
//
// The derived key is computable from `ingest_batches.payload`, which is retained
// `orgs.settings.raw_retention_days` — default 30 since 00036_retention_defaults,
// bounds 1..365 (`internal/identity/domain/settings.go`). ⚠️ DO NOT READ THE 30 AS
// DERIVED from the `alert_event_keys` idempotency horizon — that derivation is
// exactly what git-bug `747bde8` is open to retract, because `oto replay` gates on
// supersession rather than on age. For this harness the number means one thing
// only: how far back a stored corpus reaches. The 14 that
// 00006's original COMMENT names was superseded and is not the retention of any
// live database. Each stored body is an
// Alertmanager webhook envelope: it carries the receiver and the groupLabels that
// produced ONE upstream notification group, and the alerts that were in it. So a
// stored corpus contains both sides of the comparison — what Alertmanager
// actually grouped, and what oto's rule WOULD have grouped — and the interesting
// numbers are the two ways they disagree:
//
//   - OVER-SPLIT: one upstream group's alerts land in several derived groups.
//     Each extra one is an extra chat thread. This is the failure mode the axes
//     risk, because splitting is the direction the rule can go.
//   - OVER-MERGE: alerts from several upstream groups land in one derived group.
//     Mostly this is intended — it is what dropping `receiver` and `source_id`
//     was FOR — so it is reported split by cause: same alertname across
//     receivers (intended), versus genuinely different upstream label sets
//     collapsing (worth a look).
//
// # WHAT IT CANNOT MEASURE
//
// Nothing here says whether a thread was USEFUL. It counts partitions. A rule
// that produced one thread per alert would score perfectly on over-merge and be
// useless; that is why the size distribution is printed next to the counts and
// why `Singletons` is called out.
//
// It also cannot see alerts Alertmanager never sent: `max_alerts` truncation is
// reported only as a count (`truncatedAlerts`), and MuteStage drops muted alerts
// before the webhook. Both make the corpus a lower bound on real volume.
//
// # USAGE
//
//	go run ./tools/groupreplay                       # the synthetic fixtures
//	go run ./tools/groupreplay -dsn "$OTO_DB_URL"    # a real corpus
//	go run ./tools/groupreplay -dsn "$OTO_DB_URL" -since 168h -limit 50000
//	go run ./tools/groupreplay -json                 # machine-readable
package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
)

// Batch is one stored `ingest_batches` row, reduced to what the key needs.
//
// `ClusterKey` is joined from `alert_sources → clusters` and is not on the batch
// row itself: it is an axis of the derived key, and reading it from a label would
// be exactly the mistake the derivation was written to avoid.
type Batch struct {
	ID         uuid.UUID `json:"id"`
	OrgID      uuid.UUID `json:"org_id"`
	SourceID   uuid.UUID `json:"source_id"`
	ClusterKey string    `json:"cluster_key"`
	// Payload is the raw redacted webhook body, exactly as `ingest_batches.payload`
	// holds it.
	Payload Envelope `json:"payload"`
}

// Envelope is the part of the Alertmanager v4 body this tool reads.
//
// It is a SEPARATE decode from `ingestion/decode.Envelope` on purpose. That type
// is the product's ingest contract and is bound by a lenient-decoding policy that
// exists to protect the alert; this one is an analysis input, and a field it
// cannot read is a row this tool should skip loudly rather than normalise.
type Envelope struct {
	Receiver    string            `json:"receiver"`
	GroupKey    string            `json:"groupKey"`
	GroupLabels map[string]string `json:"groupLabels"`
	Alerts      []struct {
		Labels map[string]string `json:"labels"`
	} `json:"alerts"`
	TruncatedAlerts int `json:"truncatedAlerts"`
}

// Report is the whole verdict. Every count is over DISTINCT alerts, never over
// payload rows: a group that re-sends every 4 hours would otherwise dominate its
// own statistics, since an Alertmanager payload is cumulative membership and not
// a delta.
type Report struct {
	Batches          int `json:"batches"`
	BatchesSkipped   int `json:"batches_skipped"`
	AlertRows        int `json:"alert_rows"`
	DistinctAlerts   int `json:"distinct_alerts"`
	TruncatedAlerts  int `json:"truncated_alerts_reported"`
	UpstreamGroups   int `json:"upstream_groups"`
	DerivedGroups    int `json:"derived_groups"`
	DerivedSingleton int `json:"derived_singleton_groups"`

	// UpstreamSizes and DerivedSizes are distinct-alert counts per group, sorted
	// descending. Printed rather than summarised because a mean over a
	// long-tailed distribution says nothing an operator can act on.
	UpstreamSizes []int `json:"upstream_sizes"`
	DerivedSizes  []int `json:"derived_sizes"`

	// OverSplit is the count of upstream groups whose alerts landed in more than
	// one derived group, and ExtraThreads is how many EXTRA threads that costs —
	// the sum of (derived groups for this upstream group − 1). ExtraThreads is the
	// number that matters: one upstream group splitting into eleven is worse than
	// eleven splitting into two each.
	OverSplit    int `json:"over_split_groups"`
	ExtraThreads int `json:"extra_threads"`
	// WorstSplit names the upstream group that split the most.
	WorstSplit SplitDetail `json:"worst_split"`

	// OverMergeIntended is derived groups that absorbed several upstream groups
	// whose `groupLabels` were IDENTICAL — so the only thing separating them was
	// `receiver` or `source_id`. That is `continue:true` double-threading and HA
	// replicas, and collapsing them is exactly what ADR 0038 was for.
	OverMergeIntended int `json:"over_merge_intended"`
	// OverMergeSuspect is derived groups that absorbed upstream groups with
	// DIFFERENT `groupLabels` — the operator partitioned on something oto's axes
	// ignore, `group_by: [alertname, severity]` being the common shape. It is the
	// number to argue about: every one of these is a distinction a human wrote
	// down and oto is now discarding.
	//
	// ⛔ It is NOT computed from the derived group's alertnames. Every alert in a
	// derived group shares an alertname BY CONSTRUCTION — it is an axis — so that
	// version of this metric could only ever report zero.
	OverMergeSuspect int `json:"over_merge_suspect"`
	// WorstMerge names the derived group that absorbed the most upstream groups.
	WorstMerge MergeDetail `json:"worst_merge"`

	// Axes is the axis list the run used, echoed so a report filed as evidence
	// cannot be misread as evidence for a different rule.
	Axes []string `json:"axes"`
}

// SplitDetail is one upstream group and what the rule did to it.
type SplitDetail struct {
	UpstreamGroupKey string   `json:"upstream_group_key"`
	Receiver         string   `json:"receiver"`
	Into             int      `json:"into"`
	Sample           []string `json:"sample_axes"`
}

// MergeDetail is one derived group and what it absorbed.
type MergeDetail struct {
	Axes string `json:"axes"`
	From int    `json:"from"`
	// Samples are the distinct upstream `groupLabels` sets that ended up in this
	// one derived group. One entry means the merge was over `receiver`/`source_id`
	// alone.
	Samples []string `json:"sample_upstream_group_labels"`
}

// Analyse computes the report. It is pure: no clock, no database, no network, so
// the fixtures and a production corpus go through exactly the same arithmetic.
//
// A batch that cannot produce a key is SKIPPED and counted, never guessed at. The
// axes come from `alerts.SplitLabels`, which requires a valid `LabelSet` — the
// same constructor the ingest path uses — so an alert this tool refuses is an
// alert oto would have rejected too.
func Analyse(batches []Batch) Report {
	rep := Report{Axes: append([]string(nil), alerts.GroupSplitAxes...)}

	// distinct alerts, keyed by the identity the product uses.
	seenAlert := map[string]struct{}{}
	// derived group -> set of distinct alerts.
	derived := map[string]map[string]struct{}{}
	// derived group -> a readable rendering of its axes.
	derivedAxes := map[string]string{}
	// upstream group -> set of distinct alerts.
	upstream := map[string]map[string]struct{}{}
	upstreamMeta := map[string]SplitDetail{}
	// upstream group -> set of derived groups its alerts landed in.
	splitInto := map[string]map[string]struct{}{}
	// derived group -> set of upstream groups it absorbed, and the DISTINCT
	// groupLabels those groups carried.
	mergedFrom := map[string]map[string]struct{}{}
	mergedLabels := map[string]map[string]struct{}{}

	for _, b := range batches {
		clusterKey, err := alerts.NewClusterKey(b.ClusterKey)
		if err != nil || b.OrgID == uuid.Nil {
			rep.BatchesSkipped++
			continue
		}
		rep.Batches++
		rep.TruncatedAlerts += b.Payload.TruncatedAlerts

		// The upstream partition is (source, receiver, groupLabels) — the OLD key's
		// inputs, which is what makes this a comparison and not a description. AM's
		// own `groupKey` is not used: it embeds the route path and changes on reload,
		// so two payloads of one group across a config edit would count as two.
		glRendered := renderLabels(b.Payload.GroupLabels)
		ug := b.SourceID.String() + "\x00" + b.Payload.Receiver + "\x00" + glRendered
		if _, ok := upstream[ug]; !ok {
			upstream[ug] = map[string]struct{}{}
			splitInto[ug] = map[string]struct{}{}
			upstreamMeta[ug] = SplitDetail{
				UpstreamGroupKey: b.Payload.GroupKey,
				Receiver:         b.Payload.Receiver,
			}
		}

		for _, a := range b.Payload.Alerts {
			ls, err := alerts.NewLabelSet(a.Labels)
			if err != nil {
				// Not counted as a skipped batch: the batch was readable, this one
				// alert would have been a recorded ingest rejection.
				continue
			}
			rep.AlertRows++

			alertID := alerts.ComputeAlertKey(b.OrgID, clusterKey, ls, nil).String()
			seenAlert[alertID] = struct{}{}

			dg := alerts.ComputeGroupKey(b.OrgID, clusterKey, ls).String()
			axes := clusterKey.String() + " " + renderLabels(alerts.SplitLabels(ls).Map())
			derivedAxes[dg] = axes
			if derived[dg] == nil {
				derived[dg] = map[string]struct{}{}
				mergedFrom[dg] = map[string]struct{}{}
				mergedLabels[dg] = map[string]struct{}{}
			}
			derived[dg][alertID] = struct{}{}
			upstream[ug][alertID] = struct{}{}
			splitInto[ug][dg] = struct{}{}
			mergedFrom[dg][ug] = struct{}{}
			mergedLabels[dg][glRendered] = struct{}{}

			if m := upstreamMeta[ug]; len(m.Sample) < 3 && !slices.Contains(m.Sample, axes) {
				m.Sample = append(m.Sample, axes)
				upstreamMeta[ug] = m
			}
		}
	}

	rep.DistinctAlerts = len(seenAlert)
	rep.UpstreamGroups = len(upstream)
	rep.DerivedGroups = len(derived)

	for _, members := range upstream {
		rep.UpstreamSizes = append(rep.UpstreamSizes, len(members))
	}
	for _, members := range derived {
		rep.DerivedSizes = append(rep.DerivedSizes, len(members))
		if len(members) == 1 {
			rep.DerivedSingleton++
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(rep.UpstreamSizes)))
	sort.Sort(sort.Reverse(sort.IntSlice(rep.DerivedSizes)))

	for ug, into := range splitInto {
		if len(into) <= 1 {
			continue
		}
		rep.OverSplit++
		rep.ExtraThreads += len(into) - 1
		if len(into) > rep.WorstSplit.Into {
			d := upstreamMeta[ug]
			d.Into = len(into)
			rep.WorstSplit = d
		}
	}

	for dg, from := range mergedFrom {
		if len(from) <= 1 {
			continue
		}
		labels := slices.Sorted(mapKeys(mergedLabels[dg]))
		if len(labels) == 1 {
			rep.OverMergeIntended++
		} else {
			rep.OverMergeSuspect++
		}
		if len(from) > rep.WorstMerge.From {
			rep.WorstMerge = MergeDetail{
				Axes:    derivedAxes[dg],
				From:    len(from),
				Samples: labels[:min(len(labels), 3)],
			}
		}
	}
	return rep
}

// renderLabels is a stable, injective rendering used only for grouping keys
// INSIDE this tool. It is deliberately not `canon`: these strings are printed in
// the report, and a length-prefixed binary pre-image is not readable.
func renderLabels(m map[string]string) string {
	names := slices.Sorted(mapKeys(m))
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%q=%q", n, m[n]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// Write renders the report for a human. The distribution is printed as
// percentiles plus the head of the sorted sizes, because the head is where an
// axis choice goes wrong: one 4 000-member group is a thread nobody can read, and
// a mean cannot show it.
func (r Report) Write(w io.Writer) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format+"\n", args...) }

	p("groupreplay — the axes are %s", strings.Join(r.Axes, ", "))
	p("⚠️  AS-YET UNVALIDATED against production payloads unless -dsn named a real corpus.")
	p("")
	p("corpus")
	p("  batches read            %d (%d skipped: no cluster key or no org)", r.Batches, r.BatchesSkipped)
	p("  alert rows              %d", r.AlertRows)
	p("  distinct alerts         %d", r.DistinctAlerts)
	p("  truncated upstream      %d (alerts Alertmanager says it dropped; unseeable)", r.TruncatedAlerts)
	p("")
	p("partitions")
	p("  upstream groups         %d   sizes %s", r.UpstreamGroups, describe(r.UpstreamSizes))
	p("  derived groups          %d   sizes %s", r.DerivedGroups, describe(r.DerivedSizes))
	p("  derived singletons      %d (%s of derived groups) — a thread of one",
		r.DerivedSingleton, pct(r.DerivedSingleton, r.DerivedGroups))
	p("")
	p("disagreement")
	p("  over-split groups       %d (%s of upstream groups)", r.OverSplit, pct(r.OverSplit, r.UpstreamGroups))
	p("  extra threads           %d", r.ExtraThreads)
	if r.WorstSplit.Into > 0 {
		p("    worst: receiver %q split into %d — e.g. %s",
			r.WorstSplit.Receiver, r.WorstSplit.Into, strings.Join(r.WorstSplit.Sample, " | "))
	}
	p("  over-merge, intended    %d (same groupLabels across receivers/replicas — the point of ADR 0038)",
		r.OverMergeIntended)
	p("  over-merge, suspect     %d (upstream partitioned on something oto's axes ignore)", r.OverMergeSuspect)
	if r.WorstMerge.From > 0 {
		p("    worst: %s absorbed %d upstream groups — e.g. %s",
			r.WorstMerge.Axes, r.WorstMerge.From, strings.Join(r.WorstMerge.Samples, " | "))
	}
}

// WriteJSON renders the report for a machine.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func describe(sizes []int) string {
	if len(sizes) == 0 {
		return "(none)"
	}
	sorted := slices.SortedFunc(slices.Values(sizes), func(a, b int) int { return cmp.Compare(a, b) })
	q := func(f float64) int {
		i := int(f * float64(len(sorted)-1))
		return sorted[i]
	}
	head := sizes
	if len(head) > 5 {
		head = head[:5]
	}
	return fmt.Sprintf("p50=%d p90=%d p99=%d max=%d head=%v", q(0.50), q(0.90), q(0.99), sorted[len(sorted)-1], head)
}

func pct(n, of int) string {
	if of == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(of))
}
