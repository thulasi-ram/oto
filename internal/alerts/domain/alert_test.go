package domain

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/errs"
)

var clusterIDFix = uuid.MustParse("018f3a4b-0000-7000-8000-000000000201")

func validAlertParams(t *testing.T) AlertParams {
	t.Helper()
	ls := mustLabelSet(t, map[string]string{
		"alertname": "KubePodCrashLooping",
		"severity":  "critical",
		"namespace": "prod",
		"service":   "checkout",
	})
	ck := mustClusterKey(t, "prod-eu")
	ann, err := NewAnnotations(map[string]string{"summary": "pods are restarting"})
	require.NoError(t, err)

	return AlertParams{
		ID:                uuid.MustParse("018f3a4b-0000-7000-8000-000000000202"),
		OrgID:             orgA,
		ClusterID:         clusterIDFix,
		Key:               ComputeAlertKey(orgA, ck, ls, nil),
		Fingerprint:       ls.Fingerprint(),
		ClusterKey:        ck,
		Labels:            ls,
		Annotations:       ann,
		State:             StateFiring,
		FirstSeenAt:       t0,
		LastSeenAt:        t0,
		LastStateChangeAt: t0,
		TotalCases:        1,
	}
}

func TestNewAlert_RequiredFields(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*AlertParams)
		code string
	}{
		{name: "no id", mut: func(p *AlertParams) { p.ID = uuid.Nil }, code: "required"},
		{name: "no org", mut: func(p *AlertParams) { p.OrgID = uuid.Nil }, code: "required"},
		{name: "no cluster", mut: func(p *AlertParams) { p.ClusterID = uuid.Nil }, code: "required"},
		{name: "no alert key", mut: func(p *AlertParams) { p.Key = AlertKey{} }, code: "required"},
		{name: "no fingerprint", mut: func(p *AlertParams) { p.Fingerprint = SourceFingerprint{} }, code: "required"},
		{name: "no cluster key", mut: func(p *AlertParams) { p.ClusterKey = ClusterKey{} }, code: "required"},
		{name: "no labels", mut: func(p *AlertParams) { p.Labels = LabelSet{} }, code: "required"},
		{name: "no state", mut: func(p *AlertParams) { p.State = State{} }, code: "required"},
		{name: "no first_seen_at", mut: func(p *AlertParams) { p.FirstSeenAt = time.Time{} }, code: "required"},
		{
			name: "generator url over the bound",
			mut:  func(p *AlertParams) { p.GeneratorURL = strings.Repeat("u", MaxGeneratorURLBytes+1) },
			code: "max_length",
		},
		{name: "negative case count", mut: func(p *AlertParams) { p.TotalCases = -1 }, code: "min"},
		{name: "negative flap score", mut: func(p *AlertParams) { p.FlapScore = -0.1 }, code: "min"},
		{
			name: "last_seen_at before first_seen_at",
			mut:  func(p *AlertParams) { p.LastSeenAt = t0.Add(-time.Second) },
			code: "field_order",
		},
		{
			name: "last_state_change_at before first_seen_at",
			mut:  func(p *AlertParams) { p.LastStateChangeAt = t0.Add(-time.Second) },
			code: "field_order",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validAlertParams(t)
			tc.mut(&p)
			_, err := NewAlert(p)
			requireKind(t, err, errs.KindValidation, tc.code)
		})
	}
}

func TestNewAlert_HappyPath(t *testing.T) {
	p := validAlertParams(t)
	a, err := NewAlert(p)
	require.NoError(t, err)

	assert.Equal(t, "KubePodCrashLooping", a.AlertName())
	assert.Equal(t, SeverityCritical, a.Severity())
	assert.Equal(t, "prod", a.Namespace())
	assert.Equal(t, "checkout", a.Service())
	assert.Equal(t, StateFiring, a.State())
	assert.False(t, a.HasOpenCase())
	assert.False(t, a.IsFlapping())
	assert.Equal(t, p.Key, a.Key())
	assert.Equal(t, p.Fingerprint, a.Fingerprint())
	assert.Equal(t, p.ClusterKey, a.ClusterKey())
	assert.Equal(t, clusterIDFix, a.ClusterID())
}

// TestAlert_CarriesNoAck is the §B.1 boundary this entity had to lose.
//
// ⛔ AN ACK IS A RECEIPT FOR ONE FIRING EPISODE. `case_ackorder_ck` says an ack
// cannot exist without an episode to belong to; projecting one onto the Alert,
// which outlives every episode it has, is how a March acknowledgement
// pre-acknowledges a September firing. The reflective assertion is the point:
// re-adding the field is what this refuses, not a particular value of it.
func TestAlert_CarriesNoAck(t *testing.T) {
	for _, name := range []string{"AckState", "Acked", "AckedAt", "AckedBy", "AckedByLabel"} {
		_, ok := reflect.TypeOf(AlertParams{}).FieldByName(name)
		assert.False(t, ok, "AlertParams must not carry %q: ack belongs to the episode", name)

		_, ok = reflect.TypeOf(AlertProjection{}).FieldByName(name)
		assert.False(t, ok, "AlertProjection must not carry %q: ack is not projected", name)

		_, ok = reflect.TypeOf(Alert{}).MethodByName(name)
		assert.False(t, ok, "Alert must not answer %q: ask the Case", name)
	}
}

func TestAlert_TerminalStatesAreLegalProjections(t *testing.T) {
	// An Alert survives resolution forever: the projection carries the most
	// recent case's state when none is open.
	for _, state := range []State{StateFiring, StateSuppressed, StateResolved, StateExpired} {
		p := validAlertParams(t)
		p.State = state
		a, err := NewAlert(p)
		require.NoError(t, err)
		assert.Equal(t, state, a.State())
	}
}

// TestAlert_SnoozeIsTheThirdOrthogonalAxis — CONTEXT.md §3 and §B.8: a snoozed
// alert is STILL FIRING and must still be rendered as firing.
//
// ⭐ WHAT THIS TEST ASSERTS CHANGED SHAPE, AND THE NEW SHAPE IS STRONGER. It used
// to build an Alert carrying a snooze timestamp and check that the timestamp had
// moved neither state nor severity. The Alert can no longer carry one at all —
// the quiet period lives on `alert_snoozes` — so orthogonality is now structural
// rather than something the entity has to be careful about, and what is left to
// prove is that neither the entity nor the state enum has any way to express it.
func TestAlert_SnoozeIsTheThirdOrthogonalAxis(t *testing.T) {
	p := validAlertParams(t)
	p.State = StateFiring

	a, err := NewAlert(p)
	require.NoError(t, err)

	assert.Equal(t, StateFiring, a.State(), "snooze cannot reach state")
	assert.Equal(t, SeverityCritical, a.Severity(), "a snoozed critical is still critical")

	// State cannot express snooze at all, which is the mechanism.
	_, err = NewState("snoozed")
	assert.Error(t, err)

	// Nor can the constructor input, the projection, or the entity: there is no
	// field to set and no accessor to read. `AlertParams` is the rehydration
	// shape, so a snooze column reappearing on `alerts` would have to appear here
	// first — which is what makes this reflective check the tripwire rather than a
	// restatement of the compiler's job.
	for _, shape := range []any{AlertParams{}, AlertProjection{}, Alert{}} {
		ty := reflect.TypeOf(shape)
		for i := range ty.NumField() {
			name := ty.Field(i).Name
			assert.NotContains(t, strings.ToLower(name), "snooz",
				"%s must carry no snooze field: the row is the record", ty.Name())
		}
	}
}

// TestAlert_Materially is §B.3 T2: only a MATERIAL change deserves an
// `alert.mutated` event, which is what keeps a five-second scrape interval from
// drowning the timeline.
func TestAlert_Materially(t *testing.T) {
	base := validAlertParams(t)
	base.GeneratorURL = "http://prom/graph?g0.expr=up"
	a, err := NewAlert(base)
	require.NoError(t, err)

	sameLabels := a.Labels()
	sameAnnotations := a.Annotations()

	otherAnnotations, err := NewAnnotations(map[string]string{"summary": "reworded"})
	require.NoError(t, err)
	emptyAnnotations, err := NewAnnotations(nil)
	require.NoError(t, err)
	warnLabels := mustLabelSet(t, map[string]string{
		"alertname": "KubePodCrashLooping",
		"severity":  "warning",
		"namespace": "prod",
		"service":   "checkout",
	})
	extraLabel := mustLabelSet(t, map[string]string{
		"alertname": "KubePodCrashLooping",
		"severity":  "critical",
		"namespace": "prod",
		"service":   "checkout",
		"pod":       "checkout-abc",
	})

	tests := []struct {
		name        string
		labels      LabelSet
		annotations Annotations
		url         string
		want        bool
	}{
		{name: "nothing changed", labels: sameLabels, annotations: sameAnnotations, url: base.GeneratorURL},
		{name: "severity changed", labels: warnLabels, annotations: sameAnnotations, url: base.GeneratorURL, want: true},
		{name: "an annotation was reworded", labels: sameLabels, annotations: otherAnnotations, url: base.GeneratorURL, want: true},
		{name: "an annotation was dropped", labels: sameLabels, annotations: emptyAnnotations, url: base.GeneratorURL, want: true},
		{name: "the generator url moved", labels: sameLabels, annotations: sameAnnotations, url: "http://prom/other", want: true},
		{
			name:   "a non-promoted label changed — NOT material here, because it is a different Alert entirely (ADR 0020)",
			labels: extraLabel, annotations: sameAnnotations, url: base.GeneratorURL,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, a.Materially(tc.labels, tc.annotations, tc.url))
		})
	}
}

func TestAlert_MateriallyComparesTheSeverityCLASSNotTheRawLabel(t *testing.T) {
	// Normalisation happens at render time; `p1` and `sev1` are both Critical, so
	// they are not a material change to one another. (In practice they are also
	// two different Alerts, because severity participates in alert_key.)
	p := validAlertParams(t)
	p.Labels = mustLabelSet(t, map[string]string{"alertname": "X", "severity": "p1"})
	a, err := NewAlert(p)
	require.NoError(t, err)

	sev1 := mustLabelSet(t, map[string]string{"alertname": "X", "severity": "sev1"})
	assert.False(t, a.Materially(sev1, a.Annotations(), ""))

	warn := mustLabelSet(t, map[string]string{"alertname": "X", "severity": "warn"})
	assert.True(t, a.Materially(warn, a.Annotations(), ""))
}

func TestAlert_Project(t *testing.T) {
	a, err := NewAlert(validAlertParams(t))
	require.NoError(t, err)

	ac := caseID
	next, err := a.Project(AlertProjection{
		State:             StateResolved,
		CurrentCaseID:     &ac,
		LastSeenAt:        t0.Add(time.Hour),
		LastStateChangeAt: t0.Add(time.Hour),
		TotalCases:        9,
	})
	require.NoError(t, err)

	assert.Equal(t, StateResolved, next.State())
	assert.Equal(t, ac, next.CurrentCaseID())
	assert.True(t, next.HasOpenCase())
	assert.Equal(t, 9, next.TotalCases())

	// Identity and first sighting never move.
	assert.Equal(t, a.ID(), next.ID())
	assert.Equal(t, a.Key(), next.Key())
	assert.Equal(t, a.FirstSeenAt(), next.FirstSeenAt())
	assert.Equal(t, StateFiring, a.State(), "the receiver is untouched")

	// Nil pointers mean "no current case" and "awake".
	cleared, err := next.Project(AlertProjection{
		State:             StateExpired,
		LastSeenAt:        t0.Add(time.Hour),
		LastStateChangeAt: t0.Add(time.Hour),
		TotalCases:        9,
	})
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, cleared.CurrentCaseID())
	assert.False(t, cleared.HasOpenCase())
}

func TestAlert_ProjectReProvesInvariants(t *testing.T) {
	a, err := NewAlert(validAlertParams(t))
	require.NoError(t, err)

	_, err = a.Project(AlertProjection{
		State:             State{},
		LastSeenAt:        t0,
		LastStateChangeAt: t0,
	})
	requireKind(t, err, errs.KindValidation, "required")

	_, err = a.Project(AlertProjection{
		State:             StateFiring,
		LastSeenAt:        t0.Add(-time.Hour),
		LastStateChangeAt: t0,
	})
	requireKind(t, err, errs.KindValidation, "field_order")
}

func TestAlert_WithFlap(t *testing.T) {
	a, err := NewAlert(validAlertParams(t))
	require.NoError(t, err)

	flapping, err := a.WithFlap(4.5, true)
	require.NoError(t, err)
	assert.InDelta(t, 4.5, flapping.FlapScore(), 1e-6)
	assert.True(t, flapping.IsFlapping(), "flapping is a VISIBLE state; damping is never silent")
	assert.Equal(t, StateFiring, flapping.State(), "flapping is a derived signal, never a state")
	assert.False(t, a.IsFlapping(), "the receiver is untouched")

	_, err = a.WithFlap(-0.1, false)
	requireKind(t, err, errs.KindValidation, "min")
}

func TestAlert_LabelsIncludeTheIgnoredOnes(t *testing.T) {
	// "Ignored labels are stored; they are merely not hashed."
	ls := mustLabelSet(t, map[string]string{"alertname": "X", "pod": "p1"})
	ck := mustClusterKey(t, "prod-eu")

	p := validAlertParams(t)
	p.Labels = ls
	p.Key = ComputeAlertKey(orgA, ck, ls, []string{"pod"})
	p.Fingerprint = ls.Fingerprint()
	p.ClusterKey = ck

	a, err := NewAlert(p)
	require.NoError(t, err)

	pod, ok := a.Labels().Get("pod")
	assert.True(t, ok)
	assert.Equal(t, "p1", pod)
	assert.Equal(t, 2, a.Labels().Len())
}
