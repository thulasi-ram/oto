package domain

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// ------------------------------------------------------------------ SuppressedBy

func TestSuppressedBy_MarshalsAlertmanagersCamelCase(t *testing.T) {
	// The foreign system's key spelling is not ours to change, and `json` tags on
	// a domain type are forbidden (§L.4.1) — hence the shadow struct.
	in := SuppressedBy{
		SilencedBy:  []string{"sil-1", "sil-2"},
		InhibitedBy: []string{"inh-1"},
		MutedBy:     []string{"mute-1"},
	}

	b, err := json.Marshal(in)
	require.NoError(t, err)
	assert.JSONEq(t,
		`{"silencedBy":["sil-1","sil-2"],"inhibitedBy":["inh-1"],"mutedBy":["mute-1"]}`,
		string(b))

	var out SuppressedBy
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, in, out)
}

func TestSuppressedBy_RoundTripsThroughTheZeroValue(t *testing.T) {
	b, err := json.Marshal(SuppressedBy{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"silencedBy":null,"inhibitedBy":null,"mutedBy":null}`, string(b))

	var out SuppressedBy
	require.NoError(t, json.Unmarshal(b, &out))
	assert.True(t, out.IsZero())
}

func TestSuppressedBy_IsZero(t *testing.T) {
	assert.True(t, SuppressedBy{}.IsZero())
	assert.True(t, SuppressedBy{SilencedBy: []string{}}.IsZero())
	assert.False(t, SuppressedBy{SilencedBy: []string{"a"}}.IsZero())
	assert.False(t, SuppressedBy{InhibitedBy: []string{"a"}}.IsZero())
	assert.False(t, SuppressedBy{MutedBy: []string{"a"}}.IsZero())
}

func TestSuppressedBy_UnmarshalRejectsMalformedJSON(t *testing.T) {
	var out SuppressedBy
	assert.Error(t, out.UnmarshalJSON([]byte(`not json`)))
}

// ------------------------------------------------------------- ObservationSource

func TestObservationSource_TwoProducersOnly(t *testing.T) {
	assert.Equal(t, ObservationSource("ingest"), ObservedByIngest)
	assert.Equal(t, ObservationSource("reconciler"), ObservedByReconciler)
	assert.NotEqual(t, ObservedByIngest, ObservedByReconciler)
}

func TestTransitionKind_IsNotAState(t *testing.T) {
	// "IT IS NOT A STATE, and conflating the two is how a state machine acquires
	// a fifth state nobody meant to add."
	kinds := []TransitionKind{
		TransitionObserve, TransitionSuppress, TransitionUnsuppress,
		TransitionResolve, TransitionExpire,
	}
	for _, k := range kinds {
		_, err := NewState(string(k))
		assert.Error(t, err, "%q must not parse as a State", k)
		_, err = NewCaseState(string(k))
		assert.Error(t, err, "%q must not parse as a CaseState either", k)
	}
	// Five, not six: TransitionReopen went with T8 (ADR 0040).
	assert.Len(t, kinds, 5)
}

// ----------------------------------------------------------- the compare-and-set

// TestPreconditionFor_IsTheOnlyWayToBuildOne — ⭐⭐ this is the mechanism that
// stops a resolution being fabricated under READ COMMITTED.
func TestPreconditionFor_IsTheOnlyWayToBuildOne(t *testing.T) {
	o := caseIn(t, StateFiring)
	pre := PreconditionFor(o)

	assert.Equal(t, o.StateVersion(), pre.StateVersion)
	assert.NotZero(t, pre.StateVersion,
		"zero means never bound to a pre-image at all, and the repository refuses it")

	// It is ONE field, and that is the point: a single version cannot be
	// half-asserted the way a multi-column pre-image could.
	assert.Equal(t, 1, reflect.TypeOf(TransitionPrecondition{}).NumField())
}

func TestApply_BeforeIsTheExactPreImageTheVerdictUsed(t *testing.T) {
	o := caseIn(t, StateFiring, func(p *CaseParams) {
		p.StateVersion = 11
		p.SourceEndsAt = t0.Add(time.Minute)
	})
	when := t0.Add(time.Hour)

	res, err := Apply(o, TransitionCommand{
		Trigger: TriggerReap, Actor: actor(t, ActorReaper),
		At: at(t, when, when), EventID: eventIDFix, SourceHealthy: true,
	})
	require.NoError(t, err)

	assert.Equal(t, o, res.Before)
	assert.Equal(t, 11, PreconditionFor(res.Before).StateVersion,
		"the guard cannot name a row other than the one the decision was made from")
	assert.Equal(t, 11, res.Case.StateVersion(),
		"the machine never invents a version: a version the domain invented would guard nothing")
}

// --------------------------------------------------------------------- RollupKey

func TestNewRollupKey(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want RollupKey
		ok   bool
	}{
		{in: "alertname", want: RollupByAlertName, ok: true},
		{in: "namespace", want: RollupByNamespace, ok: true},
		{in: "fingerprint", want: RollupByFingerprint, ok: true},
		{in: ""},
		{in: "severity"},
		{in: "service"},
		{in: "cluster"},
		{in: "labels"},
		{in: "Alertname"},
	} {
		t.Run("group_by="+tc.in, func(t *testing.T) {
			got, err := NewRollupKey(tc.in)
			if tc.ok {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
				assert.Equal(t, tc.in, got.String())
				assert.False(t, got.IsZero())
				return
			}
			requireKind(t, err, errs.KindValidation, "enum")
			assert.True(t, got.IsZero(),
				"the set is closed because each member must be answerable from an index (ADR 0017)")
		})
	}
}

// TestAlertRollup_RollupState — a bucket is as alive as its liveliest member, and
// `expired` outranks `resolved` because "we stopped hearing about this" is an
// open question and "the upstream said it ended" is a closed one.
func TestAlertRollup_RollupState(t *testing.T) {
	tests := []struct {
		name string
		r    AlertRollup
		want State
	}{
		// ⭐ ADR 0041: `Suppressed` IS A SUBSET OF `Firing`, so the liveliest-member
		// test is `Firing > Suppressed` — "at least one live member is audible" —
		// rather than the `Suppressed > 0` it replaced, which is now unreachable.
		{name: "an audible live member wins", r: AlertRollup{Firing: 10, Suppressed: 9, Resolved: 9, Expired: 9}, want: StateFiring},
		{name: "every live member silenced reads suppressed", r: AlertRollup{Firing: 9, Suppressed: 9, Resolved: 9, Expired: 9}, want: StateSuppressed},
		{name: "one silenced member beats both terminals", r: AlertRollup{Firing: 1, Suppressed: 1, Resolved: 9, Expired: 9}, want: StateSuppressed},
		{name: "⭐ expired outranks resolved", r: AlertRollup{Resolved: 99, Expired: 1}, want: StateExpired},
		{name: "all resolved", r: AlertRollup{Resolved: 3}, want: StateResolved},
		{name: "the unreachable empty bucket falls back conservatively", r: AlertRollup{}, want: StateResolved},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.r.RollupState())
		})
	}
}

// TestAlertRollup_CountsOnlyPropertiesOfTheAlert is the shape argument the
// dropped counter failed.
//
// ⭐ EVERY COUNTER ON A BUCKET IS A FACT ABOUT THE ALERT.
// `Firing`/`Suppressed`/`Resolved`/`Expired` are its current episode's state,
// projected onto it because `state` still has an honest answer when nothing is
// firing; `Flapping` is a derived alert-scoped signal. `Acked` was the one that
// was not: it counted receipts given for individual firings, so a bucket
// reporting "12 acknowledged" was counting acknowledgements of firings that had
// ended. It is gone, with `Unacked()` — which was only ever `Total - Acked` —
// and with the column both read.
func TestAlertRollup_CountsOnlyPropertiesOfTheAlert(t *testing.T) {
	ty := reflect.TypeOf(AlertRollup{})
	for _, name := range []string{"Acked", "Unacked", "AckState"} {
		_, ok := ty.FieldByName(name)
		assert.False(t, ok, "AlertRollup must not carry %q: ack is a fact about one episode", name)

		_, ok = ty.MethodByName(name)
		assert.False(t, ok, "AlertRollup must not answer %q", name)
	}
}

// TestAlertRollup_KeepsResolvedAndExpiredApart — collapsing them would hide the
// more interesting of the two.
func TestAlertRollup_KeepsResolvedAndExpiredApart(t *testing.T) {
	ty := reflect.TypeOf(AlertRollup{})
	for _, name := range []string{"Firing", "Suppressed", "Resolved", "Expired", "Flapping"} {
		_, ok := ty.FieldByName(name)
		assert.True(t, ok, "AlertRollup must count %s separately", name)
	}
	// `Acked` and `Snoozed` are DELIBERATELY absent, and the absence is the point.
	// Within this very roll-up they were the only two counters that were not
	// properties of the alert: ack is a receipt against the CASE (ADR 0036, and
	// `alerts.ack_state` is dropped in 00049) and a snooze is a row in
	// `alert_snoozes` the alert does not carry (00048). A counter that reappears
	// here is a column creeping back onto `alerts`.
	for _, name := range []string{"Closed", "Ended", "Done", "Terminal", "Acked", "Snoozed"} {
		_, ok := ty.FieldByName(name)
		assert.False(t, ok, "AlertRollup must not merge the terminal states into %q", name)
	}
}

// TestAlertFilter_DefaultNeverHidesSnoozedAlerts — §B.8.6: nil means INCLUDE
// BOTH, and nil is the default. Hiding them is how an incident is lost.
func TestAlertFilter_DefaultNeverHidesSnoozedAlerts(t *testing.T) {
	var f AlertFilter
	assert.Nil(t, f.Snoozed, "the zero filter includes snoozed alerts")
	assert.Nil(t, f.Flapping)
	assert.Empty(t, f.States, "nil or empty means no constraint on this dimension")
}

// ------------------------------------------------------------ domain layering

// TestDomainTypesCarryNoJSONTags — §L.4.1/§P-21b: `json:"…"` struct tags in
// `domain` are forbidden, because tags are what would quietly turn a domain type
// into a DTO. (`encoding/json` itself is permitted; it does no I/O.)
func TestDomainTypesCarryNoJSONTags(t *testing.T) {
	types := []any{
		Alert{}, AlertParams{}, AlertProjection{}, AlertFilter{}, AlertUpsert{},
		AlertUpsertResult{}, AlertRollup{}, Case{}, CaseParams{},
		OpenCaseParams{}, OpenCase{}, Event{}, EventParams{},
		Snooze{}, SnoozeParams{}, SnoozeCommand{}, UnsnoozeCommand{},
		SnoozeRequest{}, SnoozeEnd{}, Observation{}, SuppressedBy{},
		Transition{}, TransitionPrecondition{}, TransitionCommand{},
		TransitionResult{}, AckChange{}, AckCommand{}, LabelCount{},
		Labels{}, LabelSet{}, Annotations{}, Label{}, Actor{},
		ObservationTime{}, TimeWindow{},
	}
	for _, v := range types {
		ty := reflect.TypeOf(v)
		t.Run(ty.Name(), func(t *testing.T) {
			for i := range ty.NumField() {
				f := ty.Field(i)
				assert.Empty(t, f.Tag.Get("json"),
					"%s.%s carries a json tag", ty.Name(), f.Name)
				assert.Empty(t, f.Tag.Get("validate"),
					"%s.%s carries a validate tag", ty.Name(), f.Name)
				assert.Empty(t, f.Tag.Get("db"),
					"%s.%s carries a db tag", ty.Name(), f.Name)
			}
		})
	}
}

// TestNoPersonReferenceOnASignal — CONTEXT.md §1b, the first of the four doors.
// `acked_by` is past-tense attribution and is the ONLY exception.
func TestNoPersonReferenceOnASignal(t *testing.T) {
	// vocab:allow the scope-boundary guard must name the words it forbids; this table IS the enforcement.
	banned := []string{
		"assignedto", "assignee", "ownerid", "owner", "watchers", "watcher",
		"subscriber", "incidentid", "ticketid", "sladueat", "responder", // vocab:allow the scope-boundary guard must name the words it forbids; this table IS the enforcement.
		"oncall", "rota", "scheduleid", "userids", "timeofday", "escalation", // vocab:allow the scope-boundary guard must name the words it forbids; this table IS the enforcement.
	}

	for _, v := range []any{Alert{}, AlertParams{}, Case{}, CaseParams{}, AlertRollup{}, AlertProjection{}} {
		ty := reflect.TypeOf(v)
		t.Run(ty.Name(), func(t *testing.T) {
			for i := range ty.NumField() {
				name := strings.ToLower(ty.Field(i).Name)
				for _, b := range banned {
					assert.NotContains(t, name, b,
						"%s.%s is a person-reference column on a signal row", ty.Name(), ty.Field(i).Name)
				}
			}
		})
	}

	// The one permitted exception is present and is past-tense.
	_, ok := reflect.TypeOf(CaseParams{}).FieldByName("AckedBy")
	assert.True(t, ok, "acked_by IS stored: it is operationally necessary")
}

// TestNoHumanWritesASignalsState — CONTEXT.md §1b, third door. `ack_state` is the
// only state axis a human may write, and the machine enforces it by actor.
func TestNoHumanWritesASignalsState(t *testing.T) {
	for _, from := range []State{StateFiring, StateSuppressed, StateResolved, StateExpired} {
		for _, tr := range allTriggers() {
			for _, kind := range []ActorKind{ActorUser, ActorSlack} {
				o := caseIn(t, from, func(p *CaseParams) {
					p.SourceEndsAt = t0.Add(time.Minute)
				})
				when := t0.Add(time.Hour)
				_, err := Apply(o, TransitionCommand{
					Trigger:           tr,
					Actor:             actorOfKind(t, kind),
					At:                at(t, when, when),
					EventID:           eventIDFix,
					SuppressionReason: SuppressionSilence,
					SourceHealthy:     true,
				})
				require.Error(t, err, "%s drove %s from %s", kind, tr, from)
			}
		}
	}
}

// --------------------------------------------------------------- small helpers

func TestDerefHelpers(t *testing.T) {
	assert.Equal(t, uuid.Nil, derefID(nil))
	id := uuid.New()
	assert.Equal(t, id, derefID(&id))

	assert.True(t, derefTime(nil).IsZero())
	assert.Equal(t, t0, derefTime(&t0))

	assert.True(t, utcOrZero(time.Time{}).IsZero())
	ist := time.FixedZone("IST", 5*3600+1800)
	assert.Equal(t, time.UTC, utcOrZero(t0.In(ist)).Location())
}

func TestLabelCount(t *testing.T) {
	// The count is not decoration: a filter bar offering a label matching nothing
	// wastes the one minute of an incident that matters most.
	lc := LabelCount{Value: "namespace", Count: 12}
	assert.Equal(t, "namespace", lc.Value)
	assert.Equal(t, 12, lc.Count)
}
