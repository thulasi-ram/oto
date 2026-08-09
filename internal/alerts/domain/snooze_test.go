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

var snoozeIDFix = uuid.MustParse("018f3a4b-0000-7000-8000-000000000301")

func validSnoozeParams(t *testing.T) SnoozeParams {
	t.Helper()
	ls := mustLabelSet(t, map[string]string{"alertname": "X"})
	return SnoozeParams{
		ID:             snoozeIDFix,
		OrgID:          orgA,
		AlertID:        alertID,
		AlertKey:       ComputeAlertKey(orgA, mustClusterKey(t, "prod-eu"), ls, nil),
		SnoozedAt:      t0,
		SnoozedUntil:   t0.Add(time.Hour),
		SnoozedBy:      uuid.New(),
		SnoozedByLabel: "Ram",
	}
}

// ------------------------------------------------- the three things snooze is NOT

// TestSnoozeIsNotAState is snooze.go's ⛔ note 1 and CONTEXT.md §3.
func TestSnoozeIsNotAState(t *testing.T) {
	_, err := NewState("snoozed")
	requireKind(t, err, errs.KindValidation, "enum")
	_, err = NewState("snooze")
	require.Error(t, err)

	for _, s := range allStates() {
		assert.NotEqual(t, "snoozed", s.String())
	}
	// Nothing in the transition table can produce or consume it.
	for _, tr := range allTriggers() {
		assert.NotContains(t, []string{"snooze", "snoozed"}, tr.String())
	}
}

// TestSnoozeIsNotASuppressionReason is snooze.go's ⛔ note 2:
// `alert_occurrences.suppression_reason` mirrors ALERTMANAGER'S FOUR REASONS and
// nothing else. Adding `snoozed` would make oto report "Alertmanager is
// suppressing this" when the truth is "a human asked oto to be quiet".
func TestSnoozeIsNotASuppressionReason(t *testing.T) {
	_, err := NewSuppressionReason(SuppressorSnoozed)
	requireKind(t, err, errs.KindValidation, "enum")

	for _, r := range []SuppressionReason{
		SuppressionSilence, SuppressionInhibition,
		SuppressionMuteTimeInterval, SuppressionActiveTimeInterval,
	} {
		assert.NotEqual(t, SuppressorSnoozed, r.String())
	}

	// Snooze records itself as a NOTIFICATION suppressed_reason instead.
	assert.Contains(t, SuppressorPrecedence(), SuppressorSnoozed)
}

// TestSnoozeAndSilenceAreRepresentableSimultaneously — both facts are about two
// different systems and neither overrides the other.
func TestSnoozeAndSilenceAreRepresentableSimultaneously(t *testing.T) {
	p := validAlertParams(t)
	p.State = StateSuppressed
	p.SnoozedUntil = t0.Add(time.Hour)
	a, err := NewAlert(p)
	require.NoError(t, err)

	assert.Equal(t, StateSuppressed, a.State(), "Alertmanager is suppressing this")
	assert.True(t, a.IsSnoozedAt(t0), "and oto is separately being quiet about it")
	assert.Equal(t, SeverityCritical, a.Severity(), "neither touches severity")
}

// ------------------------------------------------------------------- constructor

func TestNewSnooze_Bounds(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*SnoozeParams)
		code string
	}{
		{name: "no id", mut: func(p *SnoozeParams) { p.ID = uuid.Nil }, code: "required"},
		{name: "no org", mut: func(p *SnoozeParams) { p.OrgID = uuid.Nil }, code: "required"},
		{name: "no alert", mut: func(p *SnoozeParams) { p.AlertID = uuid.Nil }, code: "required"},
		{name: "no alert key", mut: func(p *SnoozeParams) { p.AlertKey = AlertKey{} }, code: "required"},
		{name: "no snoozed_at", mut: func(p *SnoozeParams) { p.SnoozedAt = time.Time{} }, code: "required"},

		// ⛔ THERE IS NO INDEFINITE SNOOZE.
		{name: "no snoozed_until", mut: func(p *SnoozeParams) { p.SnoozedUntil = time.Time{} }, code: "required"},
		{name: "until before at", mut: func(p *SnoozeParams) { p.SnoozedUntil = p.SnoozedAt.Add(-time.Second) }, code: "field_order"},
		{name: "zero-length window", mut: func(p *SnoozeParams) { p.SnoozedUntil = p.SnoozedAt }, code: "field_order"},
		{name: "under the minimum", mut: func(p *SnoozeParams) { p.SnoozedUntil = p.SnoozedAt.Add(MinSnoozeDuration - time.Second) }, code: "min"},
		{name: "over the maximum", mut: func(p *SnoozeParams) { p.SnoozedUntil = p.SnoozedAt.Add(MaxSnoozeDuration + time.Second) }, code: "max"},

		// A snooze is ALWAYS attributed and visible.
		{name: "no label", mut: func(p *SnoozeParams) { p.SnoozedByLabel = "" }, code: "not_blank"},
		{name: "blank label", mut: func(p *SnoozeParams) { p.SnoozedByLabel = "  \t" }, code: "not_blank"},
		{name: "label over the bound", mut: func(p *SnoozeParams) { p.SnoozedByLabel = strings.Repeat("l", MaxSnoozeLabelBytes+1) }, code: "max_length"},
		{name: "note over the bound", mut: func(p *SnoozeParams) { p.Note = strings.Repeat("n", MaxSnoozeNoteBytes+1) }, code: "max_length"},

		// ended_at and ended_reason are all-or-nothing.
		{name: "ended_at without a reason", mut: func(p *SnoozeParams) { p.EndedAt = p.SnoozedAt.Add(time.Minute) }, code: "field_order"},
		{name: "reason without an ended_at", mut: func(p *SnoozeParams) { p.EndedReason = SnoozeEndedManual }, code: "field_order"},
		{
			name: "ended before it started",
			mut: func(p *SnoozeParams) {
				p.EndedAt = p.SnoozedAt.Add(-time.Second)
				p.EndedReason = SnoozeEndedManual
			},
			code: "field_order",
		},
		{name: "ended_by without a label", mut: func(p *SnoozeParams) { p.EndedBy = uuid.New() }, code: "required"},
		{name: "ended_by_label over the bound", mut: func(p *SnoozeParams) { p.EndedByLabel = strings.Repeat("l", MaxSnoozeLabelBytes+1) }, code: "max_length"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validSnoozeParams(t)
			tc.mut(&p)
			_, err := NewSnooze(p)
			requireKind(t, err, errs.KindValidation, tc.code)
		})
	}

	t.Run("at the exact bounds", func(t *testing.T) {
		for _, d := range []time.Duration{MinSnoozeDuration, MaxSnoozeDuration} {
			p := validSnoozeParams(t)
			p.SnoozedUntil = p.SnoozedAt.Add(d)
			s, err := NewSnooze(p)
			require.NoError(t, err)
			assert.Equal(t, d, s.Duration())
		}
	})

	t.Run("happy path", func(t *testing.T) {
		p := validSnoozeParams(t)
		p.Note = "deploying a fix"
		s, err := NewSnooze(p)
		require.NoError(t, err)

		assert.Equal(t, snoozeIDFix, s.ID())
		assert.Equal(t, orgA, s.OrgID())
		assert.Equal(t, alertID, s.AlertID())
		assert.Equal(t, p.AlertKey, s.AlertKey())
		assert.Equal(t, "Ram", s.SnoozedByLabel())
		assert.Equal(t, "deploying a fix", s.Note())
		assert.Equal(t, time.Hour, s.Duration())
		assert.True(t, s.IsOpen())
		assert.True(t, s.EndedReason().IsZero())
		assert.Equal(t, time.UTC, s.SnoozedAt().Location())
		assert.Equal(t, time.UTC, s.SnoozedUntil().Location())
	})

	t.Run("a deleted user leaves the label behind", func(t *testing.T) {
		p := validSnoozeParams(t)
		p.SnoozedBy = uuid.Nil
		s, err := NewSnooze(p)
		require.NoError(t, err, "ON DELETE SET NULL: the LABEL is what history reads from")
		assert.Equal(t, uuid.Nil, s.SnoozedBy())
		assert.Equal(t, "Ram", s.SnoozedByLabel())
	})
}

func TestSnoozePresets_AreAllWithinTheBoundsAndNoneIsIndefinite(t *testing.T) {
	presets := SnoozePresets()
	require.NotEmpty(t, presets)

	for _, d := range presets {
		assert.GreaterOrEqual(t, d, MinSnoozeDuration, "preset %s", d)
		assert.LessOrEqual(t, d, MaxSnoozeDuration, "preset %s", d)
		assert.Positive(t, d, "there is deliberately no 'indefinite' entry")
	}

	// Ascending, and every one is constructible.
	for i := 1; i < len(presets); i++ {
		assert.Greater(t, presets[i], presets[i-1])
	}
	for _, d := range presets {
		p := validSnoozeParams(t)
		p.SnoozedUntil = p.SnoozedAt.Add(d)
		_, err := NewSnooze(p)
		require.NoError(t, err, "preset %s must satisfy the domain bounds", d)
	}

	assert.Equal(t, 30*24*time.Hour, MaxSnoozeDuration, "an unexpiring snooze is a mute")
	assert.Equal(t, 5*time.Minute, MinSnoozeDuration)
}

// ------------------------------------------------------------ the clock accessors

func TestSnooze_ClockPredicates(t *testing.T) {
	p := validSnoozeParams(t)
	until := p.SnoozedUntil
	open, err := NewSnooze(p)
	require.NoError(t, err)

	p.EndedAt = t0.Add(10 * time.Minute)
	p.EndedReason = SnoozeEndedManual
	p.EndedBy = uuid.New()
	p.EndedByLabel = "Ram"
	ended, err := NewSnooze(p)
	require.NoError(t, err)

	tests := []struct {
		name             string
		s                Snooze
		now              time.Time
		isOpen           bool
		active, expired  bool
		wantRemainingSec float64
	}{
		{name: "open and inside the window", s: open, now: t0.Add(30 * time.Minute), isOpen: true, active: true, wantRemainingSec: 1800},
		{name: "open, exactly at the wake-up", s: open, now: until, isOpen: true, expired: true},
		{name: "open, past the wake-up: the expire job has not swept it", s: open, now: until.Add(time.Hour), isOpen: true, expired: true},
		{name: "already ended", s: ended, now: t0.Add(time.Minute)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.isOpen, tc.s.IsOpen(), "IsOpen is a fact about the ROW, not the clock")
			assert.Equal(t, tc.active, tc.s.IsActiveAt(tc.now))
			assert.Equal(t, tc.expired, tc.s.HasExpiredAt(tc.now))
			assert.InDelta(t, tc.wantRemainingSec, tc.s.RemainingAt(tc.now).Seconds(), 1e-6)
			assert.GreaterOrEqual(t, tc.s.RemainingAt(tc.now), time.Duration(0), "floored at zero")
		})
	}
}

// -------------------------------------------------------------------- StartSnooze

func TestStartSnooze(t *testing.T) {
	a, err := NewAlert(validAlertParams(t))
	require.NoError(t, err)
	userID := uuid.New()

	s, events, err := StartSnooze(a, SnoozeCommand{
		ID:      snoozeIDFix,
		Actor:   humanActor(t, userID.String(), "Ram"),
		At:      at(t, t0, t0),
		Until:   t0.Add(4 * time.Hour),
		Note:    "deploy in flight",
		EventID: eventIDFix,
	})
	require.NoError(t, err)

	assert.Equal(t, a.OrgID(), s.OrgID())
	assert.Equal(t, a.ID(), s.AlertID())
	assert.Equal(t, a.Key(), s.AlertKey())
	assert.Equal(t, userID, s.SnoozedBy())
	assert.Equal(t, "Ram", s.SnoozedByLabel())
	assert.Equal(t, 4*time.Hour, s.Duration())

	require.Len(t, events, 1)
	ev := events[0]
	assert.Equal(t, EventAlertSnoozed, ev.Type())
	assert.Equal(t, a.ID(), ev.AlertID())
	assert.Equal(t, uuid.Nil, ev.OccurrenceID(), "a snooze is a fact about the ALERT, not one episode")
	assert.Equal(t, "Notifications snoozed by Ram", ev.Summary(),
		"a snooze that does not announce itself is the silent suppression §B.6 forbids")
	assert.Equal(t, "snooze:"+snoozeIDFix.String()+":started", ev.DedupeKey())

	payload := ev.Payload()
	assert.Equal(t, snoozeIDFix.String(), payload["snooze_id"])
	assert.Equal(t, t0.Add(4*time.Hour).Format(time.RFC3339Nano), payload["until"])
	assert.Equal(t, "deploy in flight", payload["note"])
	assert.Equal(t, int64(4*3600), payload["duration_seconds"])
}

// TestStartSnooze_DoesNotTouchTheSignal — §B.8: it changes nothing in the
// cluster, nothing upstream, and nothing about the signal's state.
func TestStartSnooze_DoesNotTouchTheSignal(t *testing.T) {
	p := validAlertParams(t)
	p.AckState = AckStateAcked
	a, err := NewAlert(p)
	require.NoError(t, err)

	before := a
	_, _, err = StartSnooze(a, SnoozeCommand{
		ID:      snoozeIDFix,
		Actor:   humanActor(t, uuid.New().String(), "Ram"),
		At:      at(t, t0, t0),
		Until:   t0.Add(time.Hour),
		EventID: eventIDFix,
	})
	require.NoError(t, err)

	assert.Equal(t, before, a, "StartSnooze has no code path by which it could alter the Alert")
	assert.Equal(t, StateFiring, a.State())
	assert.Equal(t, AckStateAcked, a.AckState())
	assert.Equal(t, SeverityCritical, a.Severity())
	assert.InDelta(t, before.FlapScore(), a.FlapScore(), 1e-6)
}

func TestStartSnooze_Rejects(t *testing.T) {
	a, err := NewAlert(validAlertParams(t))
	require.NoError(t, err)

	good := SnoozeCommand{
		ID:      snoozeIDFix,
		Actor:   humanActor(t, uuid.New().String(), "Ram"),
		At:      at(t, t0, t0),
		Until:   t0.Add(time.Hour),
		EventID: eventIDFix,
	}

	tests := []struct {
		name string
		mut  func(*SnoozeCommand)
		code string
	}{
		{name: "no actor", mut: func(c *SnoozeCommand) { c.Actor = Actor{} }, code: "required"},
		{name: "a machine cannot snooze", mut: func(c *SnoozeCommand) { c.Actor = actor(t, ActorSystem) }, code: "required"},
		{name: "the reaper cannot snooze", mut: func(c *SnoozeCommand) { c.Actor = actor(t, ActorReaper) }, code: "required"},
		{name: "no observation time", mut: func(c *SnoozeCommand) { c.At = ObservationTime{} }, code: "required"},
		{name: "no wake-up time", mut: func(c *SnoozeCommand) { c.Until = time.Time{} }, code: "required"},
		{name: "too short", mut: func(c *SnoozeCommand) { c.Until = t0.Add(time.Minute) }, code: "min"},
		{name: "too long", mut: func(c *SnoozeCommand) { c.Until = t0.Add(MaxSnoozeDuration + time.Hour) }, code: "max"},
		{name: "note over the bound", mut: func(c *SnoozeCommand) { c.Note = strings.Repeat("n", MaxSnoozeNoteBytes+1) }, code: "max_length"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := good
			tc.mut(&cmd)
			_, _, err := StartSnooze(a, cmd)
			requireKind(t, err, errs.KindValidation, tc.code)
		})
	}
}

func TestStartSnooze_SlackActorIDIsNotAUserFK(t *testing.T) {
	a, err := NewAlert(validAlertParams(t))
	require.NoError(t, err)
	slack, err := NewActor(ActorSlack, "U012ABCDEF", "ram")
	require.NoError(t, err)

	s, events, err := StartSnooze(a, SnoozeCommand{
		ID: snoozeIDFix, Actor: slack, At: at(t, t0, t0),
		Until: t0.Add(time.Hour), EventID: eventIDFix,
	})
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, s.SnoozedBy(), "a Slack user id is not an oto user FK")
	assert.Equal(t, "ram", s.SnoozedByLabel())
	assert.Equal(t, "U012ABCDEF", events[0].Actor().ID())
}

// ---------------------------------------------------------------------- Snooze.End

func TestSnooze_End(t *testing.T) {
	openSnooze := func(t *testing.T) Snooze {
		s, err := NewSnooze(validSnoozeParams(t))
		require.NoError(t, err)
		return s
	}

	t.Run("expired is the system's, and only the system's", func(t *testing.T) {
		s := openSnooze(t)
		endAt := t0.Add(time.Hour)

		next, events, err := s.End(UnsnoozeCommand{
			Actor: actor(t, ActorSystem), At: at(t, endAt, endAt),
			Reason: SnoozeEndedExpired, EventID: eventIDFix,
		})
		require.NoError(t, err)
		assert.Equal(t, SnoozeEndedExpired, next.EndedReason())
		assert.Equal(t, endAt, next.EndedAt())
		assert.False(t, next.IsOpen())
		assert.Equal(t, uuid.Nil, next.EndedBy())
		assert.Equal(t, EventAlertUnsnoozed, events[0].Type())
		assert.Equal(t, "Snooze expired: notifications resumed", events[0].Summary())
		assert.Equal(t, "snooze:"+snoozeIDFix.String()+":ended", events[0].DedupeKey())
		assert.Equal(t, "expired", events[0].Payload()["reason"])

		_, _, err = s.End(UnsnoozeCommand{
			Actor: humanActor(t, uuid.New().String(), "Ram"), At: at(t, endAt, endAt),
			Reason: SnoozeEndedExpired, EventID: eventIDFix,
		})
		requireKind(t, err, errs.KindInternal, "wrong_actor")
	})

	t.Run("manual and superseded require a human", func(t *testing.T) {
		for _, reason := range []SnoozeEndReason{SnoozeEndedManual, SnoozeEndedSuperseded} {
			s := openSnooze(t)
			userID := uuid.New()

			next, events, err := s.End(UnsnoozeCommand{
				Actor: humanActor(t, userID.String(), "Ram"), At: at(t, t0.Add(time.Minute), t0.Add(time.Minute)),
				Reason: reason, EventID: eventIDFix,
			})
			require.NoError(t, err)
			assert.Equal(t, reason, next.EndedReason())
			assert.Equal(t, userID, next.EndedBy())
			assert.Equal(t, "Ram", next.EndedByLabel())
			assert.Equal(t, reason.String(), events[0].Payload()["reason"])

			_, _, err = s.End(UnsnoozeCommand{
				Actor: actor(t, ActorSystem), At: at(t, t0, t0),
				Reason: reason, EventID: eventIDFix,
			})
			requireKind(t, err, errs.KindInternal, "wrong_actor")
		}
	})

	t.Run("the wake-up note lands on the timeline, not on the snooze row", func(t *testing.T) {
		p := validSnoozeParams(t)
		p.Note = "why the quiet was asked for"
		s, err := NewSnooze(p)
		require.NoError(t, err)

		next, events, err := s.End(UnsnoozeCommand{
			Actor: humanActor(t, uuid.New().String(), "Ram"), At: at(t, t0.Add(time.Minute), t0.Add(time.Minute)),
			Reason: SnoozeEndedManual, EventID: eventIDFix, Note: "fix is out",
		})
		require.NoError(t, err)
		assert.Equal(t, "why the quiet was asked for", next.Note(),
			"overwriting it would rewrite the reason the quiet period was asked for")
		assert.Equal(t, "fix is out", events[0].Payload()["note"])
	})

	t.Run("rejects", func(t *testing.T) {
		endedParams := validSnoozeParams(t)
		endedParams.EndedAt = t0.Add(time.Minute)
		endedParams.EndedReason = SnoozeEndedManual
		endedParams.EndedBy = uuid.New()
		endedParams.EndedByLabel = "Ram"
		alreadyEnded, err := NewSnooze(endedParams)
		require.NoError(t, err)

		tests := []struct {
			name string
			s    Snooze
			mut  func(*UnsnoozeCommand)
			kind errs.Kind
			code string
		}{
			{name: "no actor", s: openSnooze(t), mut: func(c *UnsnoozeCommand) { c.Actor = Actor{} }, kind: errs.KindValidation, code: "required"},
			{name: "no time", s: openSnooze(t), mut: func(c *UnsnoozeCommand) { c.At = ObservationTime{} }, kind: errs.KindValidation, code: "required"},
			{name: "no reason", s: openSnooze(t), mut: func(c *UnsnoozeCommand) { c.Reason = SnoozeEndReason{} }, kind: errs.KindValidation, code: "required"},
			{name: "already ended", s: alreadyEnded, kind: errs.KindPrecondition, code: "snooze_already_ended"},
			{name: "note over the bound", s: openSnooze(t), mut: func(c *UnsnoozeCommand) { c.Note = strings.Repeat("n", MaxSnoozeNoteBytes+1) }, kind: errs.KindValidation, code: "max_length"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				cmd := UnsnoozeCommand{
					Actor: humanActor(t, uuid.New().String(), "Ram"), At: at(t, t0.Add(time.Minute), t0.Add(time.Minute)),
					Reason: SnoozeEndedManual, EventID: eventIDFix,
				}
				if tc.mut != nil {
					tc.mut(&cmd)
				}
				_, _, err := tc.s.End(cmd)
				requireKind(t, err, tc.kind, tc.code)
			})
		}
	})

	t.Run("ended_at is clamped to snoozed_at", func(t *testing.T) {
		s := openSnooze(t)
		early := t0.Add(-time.Hour)
		next, _, err := s.End(UnsnoozeCommand{
			Actor: humanActor(t, uuid.New().String(), "Ram"), At: at(t, early, early),
			Reason: SnoozeEndedManual, EventID: eventIDFix,
		})
		require.NoError(t, err)
		assert.Equal(t, t0, next.EndedAt())
	})
}

func TestNewSnoozeEndReason(t *testing.T) {
	for _, in := range []string{"expired", "manual", "superseded"} {
		got, err := NewSnoozeEndReason(in)
		require.NoError(t, err)
		assert.Equal(t, in, got.String())
		assert.False(t, got.IsZero())
	}
	for _, in := range []string{"", "cancelled", "timeout", "upstream", "Expired"} {
		_, err := NewSnoozeEndReason(in)
		requireKind(t, err, errs.KindValidation, "enum")
	}
	assert.True(t, SnoozeEndReason{}.IsZero())
}

// ------------------------------------------------- notification suppressors §B.8.2

func TestSuppressorPrecedence_IsTheFixedB82Order(t *testing.T) {
	assert.Equal(t, []string{
		"channel_disabled",
		"no_policy",
		"snoozed",
		"storm",
		"flapping",
		"throttled",
		"verbosity",
		"duplicate_render",
	}, SuppressorPrecedence())

	// Snooze outranks the automatic dampers, and sits below the two that mean the
	// message had nowhere to go at all.
	order := map[string]int{}
	for i, r := range SuppressorPrecedence() {
		order[r] = i
	}
	assert.Less(t, order[SuppressorChannelDisabled], order[SuppressorSnoozed])
	assert.Less(t, order[SuppressorNoPolicy], order[SuppressorSnoozed])
	assert.Less(t, order[SuppressorSnoozed], order[SuppressorStorm])
	assert.Less(t, order[SuppressorSnoozed], order[SuppressorFlapping])
	assert.Less(t, order[SuppressorSnoozed], order[SuppressorThrottled])

	// The returned slice is a fresh one; a caller cannot re-order the SPEC.
	got := SuppressorPrecedence()
	got[0] = "tampered"
	assert.Equal(t, SuppressorChannelDisabled, SuppressorPrecedence()[0])
}

func TestFirstSuppressor(t *testing.T) {
	tests := []struct {
		name    string
		applies map[string]bool
		want    string
	}{
		{name: "nothing applies: deliver it", applies: nil},
		{name: "empty map: deliver it", applies: map[string]bool{}},
		{name: "all false: deliver it", applies: map[string]bool{SuppressorSnoozed: false}},
		{name: "one applies", applies: map[string]bool{SuppressorFlapping: true}, want: SuppressorFlapping},
		{
			name:    "snooze beats the automatic dampers: it tells a user what to DO",
			applies: map[string]bool{SuppressorSnoozed: true, SuppressorFlapping: true, SuppressorStorm: true},
			want:    SuppressorSnoozed,
		},
		{
			name:    "no destination at all beats snooze",
			applies: map[string]bool{SuppressorSnoozed: true, SuppressorNoPolicy: true},
			want:    SuppressorNoPolicy,
		},
		{
			name:    "a disabled channel beats everything",
			applies: map[string]bool{SuppressorChannelDisabled: true, SuppressorNoPolicy: true, SuppressorSnoozed: true},
			want:    SuppressorChannelDisabled,
		},
		{
			name:    "an unknown reason is ignored",
			applies: map[string]bool{"invented": true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, FirstSuppressor(tc.applies))
		})
	}
}

// TestSnoozeSuppresses_ExemptsItsOwnAnnouncements — §B.8.4. A snooze must be able
// to announce its own beginning and end or it becomes the silent suppression
// §B.6 forbids.
func TestSnoozeSuppresses_ExemptsItsOwnAnnouncements(t *testing.T) {
	assert.False(t, SnoozeSuppresses(NotifyReasonSnoozed))
	assert.False(t, SnoozeSuppresses(NotifyReasonUnsnoozed))
	assert.Equal(t, "snoozed", NotifyReasonSnoozed)
	assert.Equal(t, "unsnoozed", NotifyReasonUnsnoozed)

	// Everything else is suppressed, INCLUDING rule_changed — a partial mute is a
	// confusing mute.
	for _, reason := range []string{
		"firing", "resolved", "expired", "acked", "unacked",
		"rule_changed", "all_resolved", "storm", "repeat", "", "anything_at_all",
	} {
		assert.True(t, SnoozeSuppresses(reason), "reason=%q", reason)
	}
}
