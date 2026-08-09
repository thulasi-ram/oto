package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// ------------------------------------------------------------------------ State

func TestNewState_ClosedSet(t *testing.T) {
	tests := []struct {
		in   string
		want State
		ok   bool
	}{
		{in: "firing", want: StateFiring, ok: true},
		{in: "suppressed", want: StateSuppressed, ok: true},
		{in: "resolved", want: StateResolved, ok: true},
		{in: "expired", want: StateExpired, ok: true},

		{in: ""},
		{in: "none"},
		{in: "Firing"},
		{in: "FIRING"},
		{in: " firing"},
		{in: "closed"},
		{in: "acked"},
		{in: "flapping"},
		// ⛔ CONTEXT.md §3: snooze is NOT a state. `State` must never gain one.
		{in: "snoozed"},
	}
	for _, tc := range tests {
		t.Run("state="+tc.in, func(t *testing.T) {
			got, err := NewState(tc.in)
			if tc.ok {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
				assert.Equal(t, tc.in, got.String())
				return
			}
			var e *errs.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, errs.KindValidation, e.Kind)
			assert.Equal(t, "enum", e.Code)
			assert.True(t, got.IsZero())
		})
	}
}

func TestState_Predicates(t *testing.T) {
	tests := []struct {
		state              State
		open, terminal, is bool
	}{
		{state: StateNone, open: false, terminal: false, is: true},
		{state: StateFiring, open: true},
		{state: StateSuppressed, open: true},
		{state: StateResolved, terminal: true},
		{state: StateExpired, terminal: true},
	}
	for _, tc := range tests {
		t.Run("state="+tc.state.String(), func(t *testing.T) {
			assert.Equal(t, tc.open, tc.state.IsOpen())
			assert.Equal(t, tc.terminal, tc.state.IsTerminal())
			assert.Equal(t, tc.is, tc.state.IsZero())
			assert.False(t, tc.state.IsOpen() && tc.state.IsTerminal(),
				"open and terminal are mutually exclusive")
		})
	}
}

// TestStates_ExpiredIsNotResolved is CONTEXT.md §3's headline rule as a type-level
// fact: they are two distinct values, and neither is a synonym for the other.
func TestStates_ExpiredIsNotResolved(t *testing.T) {
	assert.NotEqual(t, StateResolved, StateExpired)
	assert.NotEqual(t, StateResolved.String(), StateExpired.String())
	assert.True(t, StateResolved.IsTerminal())
	assert.True(t, StateExpired.IsTerminal())

	// And their resolve reasons are bound one-to-one, which is the mechanism.
	assert.NotEqual(t, ResolveUpstream, ResolveTimeout)
}

// ---------------------------------------------------------------------- AckState

func TestNewAckState_ClosedSet(t *testing.T) {
	tests := []struct {
		in   string
		want AckState
		ok   bool
	}{
		{in: "unacked", want: AckStateUnacked, ok: true},
		{in: "acked", want: AckStateAcked, ok: true},
		{in: ""},
		{in: "Acked"},
		{in: "ack"},
		{in: "assigned"},
		{in: "snoozed"},
	}
	for _, tc := range tests {
		t.Run("ack="+tc.in, func(t *testing.T) {
			got, err := NewAckState(tc.in)
			if tc.ok {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
				assert.Equal(t, tc.in, got.String())
				return
			}
			var e *errs.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, "enum", e.Code)
		})
	}

	assert.True(t, AckStateAcked.IsAcked())
	assert.False(t, AckStateUnacked.IsAcked())
	assert.False(t, AckState{}.IsAcked())
	assert.True(t, AckState{}.IsZero())
	assert.False(t, AckStateUnacked.IsZero())
}

// --------------------------------------------------------------------- Severity

// TestNewSeverity_IsStrict is the first half of §L.4.2: strict in the domain, for
// API input and config — anything outside the closed set is rejected.
func TestNewSeverity_IsStrict(t *testing.T) {
	tests := []struct {
		in   string
		want Severity
		ok   bool
	}{
		{in: "critical", want: SeverityCritical, ok: true},
		{in: "page", want: SeverityPage, ok: true},
		{in: "warning", want: SeverityWarning, ok: true},
		{in: "info", want: SeverityInfo, ok: true},
		{in: "none", want: SeverityNone, ok: true},
		{in: "unknown", want: SeverityUnknown, ok: true},

		{in: ""},
		{in: "Critical"},
		{in: "CRITICAL"},
		{in: " critical"},
		{in: "critical "},
		{in: "crit"},  // an ALIAS is not a member of the closed set
		{in: "p1"},    // ditto
		{in: "sev1"},  // ditto
		{in: "warn"},  // ditto
		{in: "fatal"}, // ditto
		{in: "nonsense"},
	}
	for _, tc := range tests {
		t.Run("severity="+tc.in, func(t *testing.T) {
			got, err := NewSeverity(tc.in)
			if tc.ok {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
				return
			}
			var e *errs.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, errs.KindValidation, e.Kind)
			assert.Equal(t, "enum", e.Code)
			assert.True(t, got.IsZero())
		})
	}
}

// TestSeverityFromLabel_AliasTable is §L.4.2's table, verbatim from the doc
// comment. `page` and `none` keep their own values rather than collapsing.
func TestSeverityFromLabel_AliasTable(t *testing.T) {
	tests := []struct {
		in   string
		want Severity
	}{
		{in: "critical", want: SeverityCritical},
		{in: "crit", want: SeverityCritical},
		{in: "fatal", want: SeverityCritical},
		{in: "p1", want: SeverityCritical},
		{in: "sev1", want: SeverityCritical},

		{in: "page", want: SeverityPage},

		{in: "warning", want: SeverityWarning},
		{in: "warn", want: SeverityWarning},
		{in: "p2", want: SeverityWarning},
		{in: "sev2", want: SeverityWarning},

		{in: "info", want: SeverityInfo},
		{in: "informational", want: SeverityInfo},
		{in: "p3", want: SeverityInfo},
		{in: "p4", want: SeverityInfo},
		{in: "p5", want: SeverityInfo},

		{in: "none", want: SeverityNone},

		// Case and surrounding whitespace are normalised before the lookup.
		{in: "CRITICAL", want: SeverityCritical},
		{in: "  Warning \t", want: SeverityWarning},
		{in: "P1", want: SeverityCritical},

		// Everything else, including absent.
		{in: "", want: SeverityUnknown},
		{in: "   ", want: SeverityUnknown},
		{in: "unknown", want: SeverityUnknown},
		{in: "sev3", want: SeverityUnknown},
		{in: "p0", want: SeverityUnknown},
		{in: "disaster", want: SeverityUnknown},
	}
	for _, tc := range tests {
		t.Run("label="+tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, SeverityFromLabel(tc.in))
		})
	}
}

// TestSeverityFromLabel_IsTotal is the second half of §L.4.2. It CANNOT fail: it
// has no error return at all, and every input — empty, garbage, enormous, binary
// — must land inside the closed set. A severity oto has never seen must never
// cost an alert (§L.3).
func TestSeverityFromLabel_IsTotal(t *testing.T) {
	closed := map[Severity]struct{}{
		SeverityCritical: {}, SeverityPage: {}, SeverityWarning: {},
		SeverityInfo: {}, SeverityNone: {}, SeverityUnknown: {},
	}

	inputs := []string{
		"",
		" ",
		"\x00",
		"\x00\x01\x02\xff",
		"\n\t\r",
		"critical\x00",
		"日本語",
		"☃",
		"NULL",
		"null",
		"undefined",
		"-1",
		"0",
		"9999999999999999999999",
		"{\"json\": true}",
		"'; DROP TABLE alerts; --",
		"<script>alert(1)</script>",
		strings.Repeat("a", 1),
		strings.Repeat("a", 256),
		strings.Repeat("a", 100_000),
		strings.Repeat("critical", 10_000),
		strings.Repeat(" ", 10_000) + "critical" + strings.Repeat(" ", 10_000),
		strings.Repeat("日", 50_000),
	}

	for i, in := range inputs {
		t.Run("input#"+string(rune('a'+i%26)), func(t *testing.T) {
			var got Severity
			require.NotPanics(t, func() { got = SeverityFromLabel(in) })
			_, ok := closed[got]
			assert.True(t, ok, "SeverityFromLabel returned a value outside the closed set: %q", got)
			assert.False(t, got.IsZero(), "the total constructor never returns the zero Severity")

			// Whatever it returned is something the STRICT constructor accepts:
			// the lenient half can never produce a value the strict half rejects.
			round, err := NewSeverity(got.String())
			require.NoError(t, err)
			assert.Equal(t, got, round)
		})
	}

	// The huge-input case that would have been rejected by the strict half is
	// mapped, not rejected — and the mapping is stable.
	huge := strings.Repeat(" ", 5000) + "CRITICAL" + strings.Repeat(" ", 5000)
	assert.Equal(t, SeverityCritical, SeverityFromLabel(huge))
	_, err := NewSeverity(huge)
	assert.Error(t, err, "the same string is rejected by the strict constructor: two trust models")
}

func TestNewRawSeverity_StoresTheUsersOwnVocabulary(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantNil bool
		want    string
		wantErr bool
	}{
		{name: "blank is absent", in: "", wantNil: true},
		{name: "whitespace is absent", in: "  \t ", wantNil: true},
		{name: "sev1 survives verbatim", in: "sev1", want: "sev1"},
		{name: "P1 survives verbatim, uncased", in: "P1", want: "P1"},
		{name: "page survives verbatim", in: "page", want: "page"},
		{name: "an unknown vocabulary survives", in: "🔥 on fire", want: "🔥 on fire"},
		{name: "at the bound", in: strings.Repeat("a", MaxRawSeverityBytes), want: strings.Repeat("a", MaxRawSeverityBytes)},
		{name: "over the bound", in: strings.Repeat("a", MaxRawSeverityBytes+1), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewRawSeverity(tc.in)
			if tc.wantErr {
				var e *errs.Error
				require.ErrorAs(t, err, &e)
				assert.Equal(t, "max_length", e.Code)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			if tc.wantNil {
				assert.Nil(t, got, "an absent label is a nil pointer: alerts_sev_ck's floor is one character")
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tc.want, *got, "the raw label is NOT normalised at write time (§L.4.2)")
		})
	}
}

func TestSeverity_HasNoRankOrOrdering(t *testing.T) {
	// ⛔ enums.go documents at length why there is no Rank and no Raised. This
	// test is the executable form of that ruling: if either is ever added back
	// for the `severity_raised` purpose, ADR 0020 must be revisited first.
	//
	// Severity is a plain comparable value object with exactly two behaviours.
	assert.Equal(t, "critical", SeverityCritical.String())
	assert.True(t, Severity{}.IsZero())
	assert.False(t, SeverityUnknown.IsZero())
}

// ------------------------------------------------------------ SuppressionReason

// TestNewSuppressionReason_MirrorsAlertmanagerAndNothingElse is CONTEXT.md §3:
// `suppression_reason` mirrors Alertmanager's FOUR reasons and nothing else.
// `snoozed` in particular is not one of them — putting it here would make oto
// report "Alertmanager is suppressing this" when a human asked oto to be quiet.
func TestNewSuppressionReason_MirrorsAlertmanagerAndNothingElse(t *testing.T) {
	accepted := []string{"silence", "inhibition", "mute_time_interval", "active_time_interval"}
	for _, in := range accepted {
		t.Run("accepts "+in, func(t *testing.T) {
			got, err := NewSuppressionReason(in)
			require.NoError(t, err)
			assert.Equal(t, in, got.String())
			assert.False(t, got.IsZero())
		})
	}

	rejected := []string{
		"",
		"snoozed", // ⛔ the whole point
		"snooze",
		"storm",
		"flapping",
		"throttled",
		"verbosity",
		"duplicate_render",
		"channel_disabled",
		"Silence",
		"muted",
	}
	for _, in := range rejected {
		t.Run("rejects "+in, func(t *testing.T) {
			got, err := NewSuppressionReason(in)
			var e *errs.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, errs.KindValidation, e.Kind)
			assert.Equal(t, "enum", e.Code)
			assert.True(t, got.IsZero())
		})
	}

	// Every oto-side notification suppressor is rejected by this constructor —
	// the two vocabularies must not leak into one another.
	for _, s := range SuppressorPrecedence() {
		_, err := NewSuppressionReason(s)
		assert.Error(t, err, "%q is a notification suppressor, never an occurrence suppression_reason", s)
	}
}

func TestNewResolveReason(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want ResolveReason
		ok   bool
	}{
		{in: "upstream", want: ResolveUpstream, ok: true},
		{in: "timeout", want: ResolveTimeout, ok: true},
		{in: ""},
		{in: "manual"},
		{in: "user"},
		{in: "closed"},
		{in: "resolved"},
	} {
		t.Run("reason="+tc.in, func(t *testing.T) {
			got, err := NewResolveReason(tc.in)
			if tc.ok {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
				return
			}
			var e *errs.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, "enum", e.Code)
			assert.True(t, got.IsZero())
		})
	}
}

// --------------------------------------------------------------------- ActorKind

func TestNewActorKind(t *testing.T) {
	accepted := map[string]ActorKind{
		"system":     ActorSystem,
		"ingest":     ActorIngest,
		"reconciler": ActorReconciler,
		"reaper":     ActorReaper,
		"enricher":   ActorEnricher,
		"notifier":   ActorNotifier,
		"user":       ActorUser,
		"slack":      ActorSlack,
	}
	for in, want := range accepted {
		t.Run("accepts "+in, func(t *testing.T) {
			got, err := NewActorKind(in)
			require.NoError(t, err)
			assert.Equal(t, want, got)
			assert.Equal(t, in, got.String())
		})
	}
	for _, in := range []string{"", "User", "admin", "webhook", "bot"} {
		t.Run("rejects "+in, func(t *testing.T) {
			_, err := NewActorKind(in)
			var e *errs.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, "enum", e.Code)
		})
	}
}

func TestActorKind_IsHuman(t *testing.T) {
	human := []ActorKind{ActorUser, ActorSlack}
	machine := []ActorKind{ActorSystem, ActorIngest, ActorReconciler, ActorReaper, ActorEnricher, ActorNotifier}

	for _, k := range human {
		assert.True(t, k.IsHuman(), "%s", k)
	}
	for _, k := range machine {
		assert.False(t, k.IsHuman(), "%s", k)
	}
	assert.False(t, ActorKind{}.IsHuman())
	assert.True(t, ActorKind{}.IsZero())
}

func TestNewActor(t *testing.T) {
	tests := []struct {
		name    string
		kind    ActorKind
		id      string
		label   string
		wantErr bool
	}{
		{name: "system needs neither", kind: ActorSystem},
		{name: "reaper needs neither", kind: ActorReaper},
		{name: "user with both", kind: ActorUser, id: "u1", label: "Ram"},
		{name: "slack with both", kind: ActorSlack, id: "U123", label: "ram"},

		{name: "zero kind", wantErr: true},
		{name: "user without id", kind: ActorUser, label: "Ram", wantErr: true},
		{name: "user without label", kind: ActorUser, id: "u1", wantErr: true},
		{name: "user with blank id", kind: ActorUser, id: "  ", label: "Ram", wantErr: true},
		{name: "user with blank label", kind: ActorUser, id: "u1", label: " \t", wantErr: true},
		{name: "slack without id", kind: ActorSlack, label: "ram", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := NewActor(tc.kind, tc.id, tc.label)
			if tc.wantErr {
				var e *errs.Error
				require.ErrorAs(t, err, &e)
				assert.Equal(t, errs.KindValidation, e.Kind)
				assert.Equal(t, "required", e.Code)
				assert.True(t, a.IsZero())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.kind, a.Kind())
			assert.Equal(t, tc.id, a.ID())
			assert.Equal(t, tc.label, a.Label())
			assert.False(t, a.IsZero())
		})
	}
}

func TestSystemActor(t *testing.T) {
	a, err := SystemActor(ActorReaper)
	require.NoError(t, err)
	assert.Equal(t, ActorReaper, a.Kind())
	assert.Empty(t, a.ID())
	assert.Empty(t, a.Label())

	_, err = SystemActor(ActorUser)
	assert.Error(t, err, "a human actor cannot be minted without an identity")
	_, err = SystemActor(ActorKind{})
	assert.Error(t, err)
}

// -------------------------------------------------------------- ObservationTime

func TestNewObservationTime(t *testing.T) {
	occurred := time.Date(2026, 8, 9, 10, 0, 0, 0, time.FixedZone("IST", 5*3600+1800))
	recorded := time.Date(2026, 8, 9, 4, 30, 5, 0, time.UTC)

	at, err := NewObservationTime(occurred, recorded)
	require.NoError(t, err)

	assert.Equal(t, time.UTC, at.OccurredAt().Location(), "domain timestamps are normalised to UTC")
	assert.Equal(t, time.UTC, at.RecordedAt().Location())
	assert.True(t, at.OccurredAt().Equal(occurred))
	assert.Equal(t, 5*time.Second, at.Skew(), "skew is recorded_at - occurred_at")
	assert.False(t, at.IsZero())

	// A backward-skewed upstream clock is MEASURED, never rejected (C12).
	future := recorded.Add(time.Hour)
	at, err = NewObservationTime(future, recorded)
	require.NoError(t, err)
	assert.Equal(t, -time.Hour, at.Skew())

	for _, tc := range []struct{ occurred, recorded time.Time }{
		{recorded: recorded},
		{occurred: occurred},
		{},
	} {
		_, err := NewObservationTime(tc.occurred, tc.recorded)
		var e *errs.Error
		require.ErrorAs(t, err, &e)
		assert.Equal(t, "required", e.Code)
	}

	assert.True(t, ObservationTime{}.IsZero())
}

// ------------------------------------------------------------------- TimeWindow

func TestNewTimeWindow(t *testing.T) {
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	w, err := NewTimeWindow(from, to)
	require.NoError(t, err)
	assert.Equal(t, from, w.From())
	assert.Equal(t, to, w.To())
	assert.Equal(t, 24*time.Hour, w.Duration())
	assert.False(t, w.IsZero())

	// Half-open: [from, to).
	assert.True(t, w.Contains(from))
	assert.True(t, w.Contains(from.Add(time.Hour)))
	assert.False(t, w.Contains(to))
	assert.False(t, w.Contains(from.Add(-time.Nanosecond)))
	assert.False(t, w.Contains(to.Add(time.Nanosecond)))

	for _, tc := range []struct {
		name       string
		from, to   time.Time
		wantCode   string
		wantErrMsg bool
	}{
		{name: "unbounded start", to: to, wantCode: "required"},
		{name: "inverted", from: to, to: from, wantCode: "field_order"},
		{name: "empty range", from: from, to: from, wantCode: "field_order"},
		{name: "unbounded end", from: from, wantCode: "field_order"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewTimeWindow(tc.from, tc.to)
			var e *errs.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, tc.wantCode, e.Code)
			assert.True(t, got.IsZero())
		})
	}
}

func TestRequireID(t *testing.T) {
	assert.NoError(t, requireID("x", uuid.New()))
	err := requireID("org_id", uuid.Nil)
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, errs.KindValidation, e.Kind)
	assert.Equal(t, "required", e.Code)
	assert.Contains(t, e.Message, "org_id")
}
