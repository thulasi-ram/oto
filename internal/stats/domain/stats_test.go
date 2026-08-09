package domain

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// ------------------------------------------------- ⛔ NEVER PER-PERSON (SPEC R8)

// allStatsTypes is every value object this package exposes. New ones must be
// added here, which is the point: the person-scope test below sweeps this list.
func allStatsTypes() []any {
	return []any{
		AlertQuality{}, AlertCounts{}, GroupCounts{}, DeliveryCounts{},
		SourceCounts{}, ChannelCounts{}, Overview{},
	}
}

// TestNoAggregateIsKeyedByAPerson is the executable form of "there is no user
// field anywhere in this package and there is no way to add one".
//
// Per-person response-time metrics, leaderboards and per-individual aggregates
// are not merely omitted — they are UNREPRESENTABLE, and a feature that does not
// exist cannot be misused (CONTEXT.md §6, SCOPE-BOUNDARY).
func TestNoAggregateIsKeyedByAPerson(t *testing.T) {
	// vocab:allow the scope-boundary guard must name the words it forbids; this table IS the enforcement.
	banned := []string{
		"user", "person", "people", "individual", "member", "actor",
		"assignee", "assigned", "owner", "responder", "oncall", "on_call", // vocab:allow the scope-boundary guard must name the words it forbids; this table IS the enforcement.
		"rota", "engineer", "operator", "email", "handle", "login", // vocab:allow the scope-boundary guard must name the words it forbids; this table IS the enforcement.
		"ackedby", "acked_by", "resolvedby", "snoozedby", "createdby",
		"leaderboard", "ranking", "perperson", "who",
	}

	for _, v := range allStatsTypes() {
		ty := reflect.TypeOf(v)
		t.Run(ty.Name(), func(t *testing.T) {
			require.NotZero(t, ty.NumField())
			for i := range ty.NumField() {
				f := ty.Field(i)
				lower := strings.ToLower(f.Name)
				for _, b := range banned {
					assert.NotContains(t, lower, b,
						"%s.%s keys an aggregate on a person", ty.Name(), f.Name)
				}
				// Nothing may even be TYPED as a person: no UUID identity fields.
				assert.NotContains(t, strings.ToLower(f.Type.String()), "uuid",
					"%s.%s carries an identity", ty.Name(), f.Name)
			}
		})
	}
}

// TestAlertQualityIsKeyedByTheSignalOnly — the row is per alertname, per cluster.
func TestAlertQualityIsKeyedByTheSignalOnly(t *testing.T) {
	ty := reflect.TypeOf(AlertQuality{})

	for _, name := range []string{"AlertName", "ClusterKey"} {
		f, ok := ty.FieldByName(name)
		require.True(t, ok, "AlertQuality must be keyed by %s", name)
		assert.Equal(t, "string", f.Type.String())
	}

	// Everything else is a COUNT or a DURATION over signals — never a name.
	for i := range ty.NumField() {
		f := ty.Field(i)
		if f.Name == "AlertName" || f.Name == "ClusterKey" {
			continue
		}
		assert.Contains(t, []string{"int", "int64"}, f.Type.String(),
			"AlertQuality.%s is neither a count nor a duration", f.Name)
	}
}

// TestSortKeysAreSignalScoped — a sort key is a hygiene problem, never a person.
func TestSortKeysAreSignalScoped(t *testing.T) {
	for _, s := range []Sort{
		SortOccurrencesDesc, SortNotificationsDesc, SortAckRateAsc,
		SortFlapTransitionsDesc, SortFiringSecondsDesc,
	} {
		lower := strings.ToLower(s.String())
		// vocab:allow the scope-boundary guard must name the words it forbids; this table IS the enforcement.
		for _, banned := range []string{"user", "person", "acked_by", "assignee", "responder", "mtta", "mttr"} {
			assert.NotContains(t, lower, banned, "sort key %q", s)
		}
	}

	// The vocabulary is binding: it is FIRING DURATION, never MTTR.
	assert.Equal(t, Sort("-total_firing_seconds"), SortFiringSecondsDesc)
	_, ok := reflect.TypeOf(AlertQuality{}).FieldByName("TotalFiringSeconds")
	assert.True(t, ok)
}

// ------------------------------------------------------------------------ Sort

func TestNewSort(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Sort
		ok   bool
	}{
		{name: "empty falls back to the default", in: "", want: SortOccurrencesDesc, ok: true},
		{name: "noisiest rules", in: "-occurrences", want: SortOccurrencesDesc, ok: true},
		{name: "most interruptions", in: "-notifications", want: SortNotificationsDesc, ok: true},
		{name: "nobody ever acks these", in: "ack_rate", want: SortAckRateAsc, ok: true},
		{name: "unstable", in: "-flap_transitions", want: SortFlapTransitionsDesc, ok: true},
		{name: "firing longest", in: "-total_firing_seconds", want: SortFiringSecondsDesc, ok: true},

		{name: "ascending occurrences is not offered", in: "occurrences"},
		{name: "descending ack rate is not offered", in: "-ack_rate"},
		{name: "an arbitrary column would be an unindexed sort", in: "alertname"},
		{name: "sql injection", in: "1; DROP TABLE alerts"},
		{name: "a per-person sort does not exist", in: "-acked_by"},
		{name: "mttr does not exist", in: "-mttr"},
		{name: "case matters", in: "-Occurrences"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewSort(tc.in)
			if tc.ok {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
				assert.Equal(t, string(tc.want), got.String())
				return
			}
			var e *errs.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, errs.KindValidation, e.Kind)
			assert.Equal(t, "enum", e.Code)
			assert.Equal(t, Sort(""), got)
		})
	}
}

func TestNewSort_DefaultIsTheNoisiestRules(t *testing.T) {
	got, err := NewSort("")
	require.NoError(t, err)
	assert.Equal(t, SortOccurrencesDesc, got,
		"the best alert is the one that no longer exists, so the noisiest come first")
}

// ---------------------------------------------------------------- AlertQuality

func TestAlertQuality_AckRate(t *testing.T) {
	tests := []struct {
		name string
		q    AlertQuality
		want float32
	}{
		{name: "never acked — the rule whose best future is deletion", q: AlertQuality{Occurrences: 47}},
		{name: "always acked", q: AlertQuality{Occurrences: 4, AckedOccurrences: 4}, want: 1},
		{name: "half", q: AlertQuality{Occurrences: 4, AckedOccurrences: 2}, want: 0.5},
		{name: "no occurrences at all", q: AlertQuality{AckedOccurrences: 3}},
		{name: "a negative count cannot divide", q: AlertQuality{Occurrences: -1, AckedOccurrences: 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.InDelta(t, tc.want, tc.q.AckRate(), 1e-6)
		})
	}
}

func TestAlertQuality_FlapScore(t *testing.T) {
	tests := []struct {
		name string
		q    AlertQuality
		want float32
	}{
		{name: "stable", q: AlertQuality{Occurrences: 10}},
		{name: "one transition each", q: AlertQuality{Occurrences: 10, FlapTransitions: 10}, want: 1},
		{name: "very noisy", q: AlertQuality{Occurrences: 2, FlapTransitions: 9}, want: 4.5},
		{name: "no occurrences", q: AlertQuality{FlapTransitions: 5}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.InDelta(t, tc.want, tc.q.FlapScore(), 1e-6)
		})
	}
}

func TestAlertQuality_SortValue(t *testing.T) {
	q := AlertQuality{
		AlertName:          "KubePodCrashLooping",
		ClusterKey:         "prod-eu",
		Occurrences:        47,
		Notifications:      52,
		AckedOccurrences:   4,
		FlapTransitions:    9,
		TotalFiringSeconds: 86_400,
	}

	tests := []struct {
		sort Sort
		want float64
	}{
		{sort: SortOccurrencesDesc, want: 47},
		{sort: SortNotificationsDesc, want: 52},
		{sort: SortAckRateAsc, want: float64(float32(4) / float32(47))},
		{sort: SortFlapTransitionsDesc, want: 9},
		{sort: SortFiringSecondsDesc, want: 86_400},
		{sort: Sort("unknown"), want: 47}, // the default axis
		{sort: Sort(""), want: 47},
	}
	for _, tc := range tests {
		t.Run(string(tc.sort), func(t *testing.T) {
			assert.InDelta(t, tc.want, q.SortValue(tc.sort), 1e-6)
		})
	}
}

// TestAlertQuality_AnswersTheReportsHeadlineSentence — "this rule fired 47 times
// this month, cost 47 notifications, and was acknowledged 0 times".
func TestAlertQuality_AnswersTheReportsHeadlineSentence(t *testing.T) {
	q := AlertQuality{
		AlertName:     "KubePodCrashLooping",
		ClusterKey:    "prod-eu",
		Occurrences:   47,
		Notifications: 47,
	}
	assert.Zero(t, q.AckRate())
	assert.Equal(t, 47, q.Occurrences)
	assert.Equal(t, 47, q.Notifications)
	assert.Equal(t, float64(47), q.SortValue(SortOccurrencesDesc))
}

// TestExpiredIsCountedApartFromResolved — CONTEXT.md §3 and §6: losing sight of
// an alert is not the alert going away, and summing the two into one "closed"
// bucket is exactly the lie oto exists to prevent.
func TestExpiredIsCountedApartFromResolved(t *testing.T) {
	quality := reflect.TypeOf(AlertQuality{})
	autoResolved, ok := quality.FieldByName("AutoResolved")
	require.True(t, ok)
	expired, ok := quality.FieldByName("Expired")
	require.True(t, ok)
	assert.NotEqual(t, autoResolved.Index, expired.Index, "two separate fields")

	counts := reflect.TypeOf(AlertCounts{})
	for _, name := range []string{"Firing", "Suppressed", "Resolved", "Expired", "Acked", "Unacked", "Flapping"} {
		_, ok := counts.FieldByName(name)
		assert.True(t, ok, "AlertCounts must carry %s", name)
	}

	// No ALERT-scoped type merges them into one bucket. (GroupCounts.Closed is a
	// legitimate generation fact and is deliberately not swept here.)
	for _, v := range []any{AlertQuality{}, AlertCounts{}} {
		ty := reflect.TypeOf(v)
		for _, merged := range []string{"Closed", "Ended", "Done", "Finished", "Terminal", "Inactive"} {
			_, found := ty.FieldByName(merged)
			assert.False(t, found, "%s must not merge the terminal states into %q", ty.Name(), merged)
		}
	}

	// A worked example: the two are read separately, and there is no derived
	// "closed" total on the type to read them back out of.
	q := AlertQuality{Occurrences: 10, AutoResolved: 6, Expired: 4}
	assert.Equal(t, 6, q.AutoResolved)
	assert.Equal(t, 4, q.Expired)
	assert.NotContains(t, methodNames(reflect.TypeOf(q)), "Closed")
	assert.NotContains(t, methodNames(reflect.TypeOf(q)), "Resolved")
}

// ---------------------------------------------------------------- the roll-ups

// TestDeliveryCounts_DeadIsAProductSignal — oto's silence must never be
// indistinguishable from "no alert fired".
func TestDeliveryCounts_DeadIsAFirstClassField(t *testing.T) {
	ty := reflect.TypeOf(DeliveryCounts{})
	for _, name := range []string{"Sent", "Failed", "Dead", "Skipped", "Pending", "Ambiguous"} {
		_, ok := ty.FieldByName(name)
		assert.True(t, ok, "DeliveryCounts must carry %s", name)
	}

	d := DeliveryCounts{Sent: 3, Dead: 1}
	assert.NotZero(t, d.Dead, "a non-zero Dead is a product signal, not a footnote")
}

func TestSourceCounts_GatesTheReaper(t *testing.T) {
	ty := reflect.TypeOf(SourceCounts{})
	for _, name := range []string{"Healthy", "Degraded", "Unreachable", "Unknown", "MaxClockSkewMS", "TotalDivergence"} {
		_, ok := ty.FieldByName(name)
		assert.True(t, ok, "SourceCounts must carry %s", name)
	}

	// Skew is a measured quantity, surfaced rather than rejected (C12).
	f, _ := ty.FieldByName("MaxClockSkewMS")
	assert.Equal(t, "int64", f.Type.String())
}

func TestOverview_IsTheWholeDashboardAndNothingPersonal(t *testing.T) {
	var o Overview

	// Each half is present.
	assert.IsType(t, AlertCounts{}, o.Alerts)
	assert.IsType(t, GroupCounts{}, o.Groups)
	assert.IsType(t, DeliveryCounts{}, o.Deliveries)
	assert.IsType(t, SourceCounts{}, o.Sources)
	assert.IsType(t, ChannelCounts{}, o.Channels)

	// And exactly those five: a sixth half would be where a person snuck in.
	assert.Equal(t, 5, reflect.TypeOf(Overview{}).NumField())

	o = Overview{
		Alerts:     AlertCounts{Firing: 2, Suppressed: 1, Resolved: 8, Expired: 3, Acked: 1, Unacked: 2, Flapping: 1},
		Groups:     GroupCounts{Open: 1, Closed: 4, Storm: 0},
		Deliveries: DeliveryCounts{Sent: 10, Failed: 1, Dead: 1},
		Sources:    SourceCounts{Healthy: 1, MaxClockSkewMS: 1200},
		Channels:   ChannelCounts{Healthy: 2},
	}
	assert.Equal(t, 8, o.Alerts.Resolved)
	assert.Equal(t, 3, o.Alerts.Expired)
	assert.NotEqual(t, o.Alerts.Resolved, o.Alerts.Expired)
}

func TestCountTypesAreAllPlainCounters(t *testing.T) {
	for _, v := range []any{AlertCounts{}, GroupCounts{}, DeliveryCounts{}, ChannelCounts{}} {
		ty := reflect.TypeOf(v)
		t.Run(ty.Name(), func(t *testing.T) {
			for i := range ty.NumField() {
				assert.Equal(t, "int", ty.Field(i).Type.String(),
					"%s.%s is not a plain counter", ty.Name(), ty.Field(i).Name)
			}
		})
	}
}

func TestStatsTypesCarryNoDTOTags(t *testing.T) {
	// §L.4.1: `json:"…"` struct tags in `domain` are forbidden; tags are what
	// would quietly turn a domain type into a DTO.
	for _, v := range allStatsTypes() {
		ty := reflect.TypeOf(v)
		t.Run(ty.Name(), func(t *testing.T) {
			for i := range ty.NumField() {
				f := ty.Field(i)
				assert.Empty(t, f.Tag.Get("json"), "%s.%s", ty.Name(), f.Name)
				assert.Empty(t, f.Tag.Get("validate"), "%s.%s", ty.Name(), f.Name)
				assert.Empty(t, f.Tag.Get("db"), "%s.%s", ty.Name(), f.Name)
			}
		})
	}
}

func methodNames(ty reflect.Type) []string {
	out := make([]string, 0, ty.NumMethod())
	for i := range ty.NumMethod() {
		out = append(out, ty.Method(i).Name)
	}
	return out
}
