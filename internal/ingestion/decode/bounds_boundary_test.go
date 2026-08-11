package decode

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
)

// ⭐ THIS FILE IS THE ON/OVER PAIR FOR EVERY BOUND OF SPEC §L.3.2 THAT CAN BE
// DECIDED WITHOUT A DATABASE — B2 through B16, and the B19/B8 interaction.
//
// It exists because the bounds were named constants that nothing asserted, and a
// conformance audit marked layer 2 "PASS" on their EXISTENCE. A constant that is
// never exercised is a comment: the value can be right and the comparison off by
// one, and either direction is a product defect that only shows up in a customer's
// incident.
//
//	one over the bound, rejected when it should be accepted  -> a lost alert.
//	   Alertmanager sees the 202, deletes the notification, and never retries the
//	   alert oto declined for no reason (C4, ADR 0007).
//	one under the bound, accepted when it should be rejected  -> the process is
//	   not protected, and the value lands at layer 6 where a CHECK violation is a
//	   500 in the middle of an incident.
//
// So every case below states a payload sitting EXACTLY ON a limit and one sitting
// EXACTLY ONE UNIT OVER it, and asserts the verdict of both. The bounds are
// INCLUSIVE throughout — `> Max…`, never `>= Max…` — and these tests are what
// makes that a fact rather than a reading.
//
// B1 (body size) and B17 (chunking) are not here: the first is enforced by the
// transport and the second by the process transaction, so both are asserted end to
// end in `test/integration/ingest_bounds_test.go` where the status code and the
// `ingest_rejections` row are observable.

// canonOverheadPerLabel is what one label costs B6 on top of its own bytes: a
// 4-byte big-endian length prefix for the name and one for the value (SPEC §C.1).
//
// It is restated here rather than imported because `alerts/domain` keeps it
// unexported. TestNormalise_B6LabelSetSizeOnTheBound pins this copy to the
// kernel's own arithmetic through LabelSet.SerialisedSize, so the two cannot drift
// silently.
const canonOverheadPerLabel = 8

// canonSize is the quantity B6 caps: the length of the §C.1 canonical
// serialisation of a label map.
func canonSize(m map[string]string) int {
	n := 0
	for name, value := range m {
		n += len(name) + len(value) + canonOverheadPerLabel
	}
	return n
}

// labelsOfCanonSize builds a legal label map whose canonical serialisation is
// EXACTLY target bytes, so a B6 test can sit on the bound instead of near it.
//
// It has to spread the bytes over several labels because B5 caps any single value
// at 4 096 and B6's cap is 16 384: no one label can reach the bound on its own,
// which is precisely the case a naive test would miss.
func labelsOfCanonSize(t *testing.T, target int) map[string]string {
	t.Helper()

	// `alertname` is mandatory (B10) and costs 9 + 1 + 8 bytes.
	m := map[string]string{alerts.LabelAlertName: "X"}
	remaining := target - canonSize(m)

	for i := 0; remaining > 0; i++ {
		name := fmt.Sprintf("pad_%02d", i)
		overhead := len(name) + canonOverheadPerLabel
		require.Greater(t, remaining, overhead,
			"the residue is too small to spend on another label; adjust the target")

		chunk := min(remaining-overhead, alerts.MaxLabelValueBytes)
		m[name] = strings.Repeat("v", chunk)
		remaining -= chunk + overhead
	}

	require.Equal(t, target, canonSize(m))
	require.LessOrEqual(t, len(m), alerts.MaxLabels, "the fixture must not trip B3 instead of B6")
	return m
}

// nestedObject renders a body whose maximum JSON nesting depth is exactly depth.
// `{}` is depth 1, which is what CheckDepth counts.
func nestedObject(depth int) []byte {
	return []byte(strings.Repeat(`{"a":`, depth-1) + `{}` + strings.Repeat(`}`, depth-1))
}

// ------------------------------------------------------------- B2, B15 (batch)

// TestApplyBatchBounds_B2AlertsPerBatch is the batch cap, and it is the one bound
// whose over-the-line behaviour is TRUNCATE rather than reject.
//
// Nine thousand nine hundred and ninety-nine of ten thousand and one alerts
// recorded beats zero recorded, so the batch survives; the excess is counted into
// `ingest_rejections` so the loss is visible rather than inferred.
func TestApplyBatchBounds_B2AlertsPerBatch(t *testing.T) {
	t.Run("exactly on the bound is accepted whole", func(t *testing.T) {
		env := Envelope{Alerts: make([]Alert, domain.MaxAlertsPerBatch)}

		out := ApplyBatchBounds(&env)

		assert.Zero(t, out.Dropped, "10 000 is legal; B2 is inclusive")
		assert.Empty(t, out.Notes, "nothing was lost, so nothing is recorded")
		assert.Len(t, env.Alerts, domain.MaxAlertsPerBatch)
	})

	t.Run("one over the bound truncates and records", func(t *testing.T) {
		env := Envelope{Alerts: make([]Alert, domain.MaxAlertsPerBatch+1)}

		out := ApplyBatchBounds(&env)

		assert.Equal(t, 1, out.Dropped, "exactly the excess is dropped, not the batch")
		assert.Len(t, env.Alerts, domain.MaxAlertsPerBatch,
			"the batch is truncated to the cap, and `ingest_batches_count_ck` is that same number")
		assert.Equal(t, []domain.Reason{domain.ReasonTooManyAlerts}, reasons(out.Notes))
	})
}

// TestApplyBatchBounds_B15ReceiverAndGroupKey.
//
// Both are display strings oto stores for observability and hashes into no
// identity — AM's `groupKey` is opaque and MUST NOT be parsed (C3) — so the
// over-the-bound verdict is truncate-and-keep with NO rejection reason at all.
// §L.3.2's B15 row assigns none, and `ingest_rejections_reason_ck` is a closed
// enum: inventing a member for a shortened display string would be a migration.
func TestApplyBatchBounds_B15ReceiverAndGroupKey(t *testing.T) {
	t.Run("exactly on the bound is untouched", func(t *testing.T) {
		receiver := strings.Repeat("r", domain.MaxReceiverBytes)
		groupKey := strings.Repeat("g", domain.MaxGroupKeyBytes)
		env := Envelope{Receiver: receiver, GroupKey: groupKey}

		out := ApplyBatchBounds(&env)

		assert.Equal(t, receiver, env.Receiver)
		assert.Equal(t, groupKey, env.GroupKey)
		assert.Empty(t, out.Notes)
	})

	t.Run("one over the bound is truncated, and the batch is kept", func(t *testing.T) {
		env := Envelope{
			Receiver: strings.Repeat("r", domain.MaxReceiverBytes+1),
			GroupKey: strings.Repeat("g", domain.MaxGroupKeyBytes+1),
		}

		out := ApplyBatchBounds(&env)

		assert.Len(t, env.Receiver, domain.MaxReceiverBytes)
		assert.Len(t, env.GroupKey, domain.MaxGroupKeyBytes)
		assert.Empty(t, out.Notes,
			"B15 assigns no rejection reason; a shortened display string is not a dropped observation")
		assert.Zero(t, out.Dropped, "no alert was lost")
	})
}

// ------------------------------------------------------- B3-B6, B9-B11 (labels)

// TestNormalise_B3LabelsPerAlert. Over the cap THAT ALERT is dropped and the rest
// of the batch proceeds — the asymmetry that makes a 202 defensible.
func TestNormalise_B3LabelsPerAlert(t *testing.T) {
	labelsOf := func(n int) map[string]string {
		m := map[string]string{alerts.LabelAlertName: "X"}
		for i := len(m); i < n; i++ {
			m[fmt.Sprintf("l%d", i)] = "v"
		}
		return m
	}

	t.Run("exactly on the bound is accepted", func(t *testing.T) {
		in := labelsOf(alerts.MaxLabels)
		require.Len(t, in, alerts.MaxLabels)

		n, err := normalise(t, wireAlert(in, nil))

		require.NoError(t, err, "64 labels is legal; B3 is inclusive")
		assert.Equal(t, alerts.MaxLabels, n.Labels.Len())
		assert.Empty(t, n.Notes)
	})

	t.Run("one over the bound rejects that alert", func(t *testing.T) {
		in := labelsOf(alerts.MaxLabels + 1)
		require.Len(t, in, alerts.MaxLabels+1)

		_, err := normalise(t, wireAlert(in, nil))

		require.Error(t, err)
		assert.Equal(t, domain.ReasonTooManyLabels, domain.ReasonFromError(err))
	})
}

// TestNormalise_B4LabelNameLength.
func TestNormalise_B4LabelNameLength(t *testing.T) {
	t.Run("exactly on the bound is accepted", func(t *testing.T) {
		name := strings.Repeat("a", alerts.MaxLabelNameBytes)

		n, err := normalise(t, wireAlert(map[string]string{
			alerts.LabelAlertName: "X", name: "v",
		}, nil))

		require.NoError(t, err, "a 1024-byte label name is legal; B4 is inclusive")
		_, ok := n.Labels.Get(name)
		assert.True(t, ok, "the label survives verbatim, name included")
	})

	t.Run("one over the bound rejects that alert", func(t *testing.T) {
		name := strings.Repeat("a", alerts.MaxLabelNameBytes+1)

		_, err := normalise(t, wireAlert(map[string]string{
			alerts.LabelAlertName: "X", name: "v",
		}, nil))

		require.Error(t, err)
		assert.Equal(t, domain.ReasonLabelNameTooLarge, domain.ReasonFromError(err),
			"the length bound is checked BEFORE the charset, so an over-long but "+
				"well-formed name reports its length and not `invalid_label_name`")
	})
}

// TestNormalise_B5LabelValueLength.
func TestNormalise_B5LabelValueLength(t *testing.T) {
	t.Run("exactly on the bound is accepted", func(t *testing.T) {
		value := strings.Repeat("v", alerts.MaxLabelValueBytes)

		n, err := normalise(t, wireAlert(map[string]string{
			alerts.LabelAlertName: "X", "pod": value,
		}, nil))

		require.NoError(t, err, "a 4096-byte label value is legal; B5 is inclusive")
		got, ok := n.Labels.Get("pod")
		require.True(t, ok)
		assert.Equal(t, value, got, "a label value is identity and is never truncated")
	})

	t.Run("one over the bound rejects that alert", func(t *testing.T) {
		_, err := normalise(t, wireAlert(map[string]string{
			alerts.LabelAlertName: "X",
			"pod":                 strings.Repeat("v", alerts.MaxLabelValueBytes+1),
		}, nil))

		require.Error(t, err)
		assert.Equal(t, domain.ReasonLabelValueTooLarge, domain.ReasonFromError(err))
	})
}

// TestNormalise_B6LabelSetSizeOnTheBound is the total serialised set, and it is
// the bound most likely to be got wrong: the quantity capped is not the sum of the
// names and values but the CANONICAL SERIALISATION, which adds a 4-byte length
// prefix to each of them (§C.1). Counting the payload bytes alone would leave the
// bound eight bytes per label too generous.
func TestNormalise_B6LabelSetSizeOnTheBound(t *testing.T) {
	in := labelsOfCanonSize(t, alerts.MaxLabelSetBytes)

	n, err := normalise(t, wireAlert(in, nil))

	require.NoError(t, err, "a serialisation of exactly 16384 bytes is legal; B6 is inclusive")
	assert.Equal(t, alerts.MaxLabelSetBytes, n.Labels.SerialisedSize(),
		"⭐ this pins the local overhead constant to the kernel's own arithmetic: "+
			"if either changes without the other, the fixture stops sitting on the bound")
}

// TestNormalise_B6LabelSetSizeOneOver.
func TestNormalise_B6LabelSetSizeOneOver(t *testing.T) {
	in := labelsOfCanonSize(t, alerts.MaxLabelSetBytes+1)

	_, err := normalise(t, wireAlert(in, nil))

	require.Error(t, err)
	assert.Equal(t, domain.ReasonLabelSetTooLarge, domain.ReasonFromError(err))
}

// TestNormalise_B9LabelNameCharset. B9 has no numeric bound, so its "boundary" is
// the first and subsequent character classes of `^[a-zA-Z_][a-zA-Z0-9_]*$`.
//
// The last case is the §L.3.2a corollary in test form: an unstorable label NAME
// needs no storability rule of its own, because the charset already admits no NUL
// and no non-ASCII byte.
func TestNormalise_B9LabelNameCharset(t *testing.T) {
	accepted := []string{"_", "a", "A9", "_9", "a_b_9", strings.Repeat("z", 100)}
	for _, name := range accepted {
		t.Run("accepted/"+name, func(t *testing.T) {
			_, err := normalise(t, wireAlert(map[string]string{
				alerts.LabelAlertName: "X", name: "v",
			}, nil))
			assert.NoError(t, err)
		})
	}

	rejected := map[string]string{
		"a leading digit":  "9a",
		"a hyphen":         "a-b",
		"a dot":            "a.b",
		"an empty name":    "",
		"a non-ascii byte": "ä",
		"a NUL":            "a\x00b",
	}
	for what, name := range rejected {
		t.Run("rejected/"+what, func(t *testing.T) {
			_, err := normalise(t, wireAlert(map[string]string{
				alerts.LabelAlertName: "X", name: "v",
			}, nil))

			require.Error(t, err)
			assert.Equal(t, domain.ReasonInvalidLabelName, domain.ReasonFromError(err))
		})
	}
}

// TestNormalise_B10AlertnameIsRequired. `alertname` is the one label oto refuses
// to live without: it is what a human recognises the Alert by and what every rule
// lookup keys on.
func TestNormalise_B10AlertnameIsRequired(t *testing.T) {
	t.Run("present and non-empty is accepted", func(t *testing.T) {
		n, err := normalise(t, wireAlert(map[string]string{alerts.LabelAlertName: "X"}, nil))
		require.NoError(t, err)
		assert.Equal(t, "X", n.Labels.AlertName())
	})

	for what, in := range map[string]map[string]string{
		"absent":     {"severity": "critical"},
		"empty":      {alerts.LabelAlertName: ""},
		"whitespace": {alerts.LabelAlertName: "   "},
	} {
		t.Run(what+" rejects that alert", func(t *testing.T) {
			_, err := normalise(t, wireAlert(in, nil))

			require.Error(t, err)
			assert.Equal(t, domain.ReasonMissingAlertname, domain.ReasonFromError(err))
		})
	}
}

// TestNormalise_B11AlertnameLength.
//
// ⚠️ B11's reason is `label_value_too_large`, NOT a member of its own. §L.3.2
// assigns that string deliberately — `alertname` is a label value like any other —
// and the closed enum has no `alertname_too_large`. A test that expected a
// dedicated reason would be asserting a member that cannot be written.
func TestNormalise_B11AlertnameLength(t *testing.T) {
	t.Run("exactly on the bound is accepted", func(t *testing.T) {
		name := strings.Repeat("A", alerts.MaxAlertNameBytes)

		n, err := normalise(t, wireAlert(map[string]string{alerts.LabelAlertName: name}, nil))

		require.NoError(t, err, "a 1024-byte alertname is legal; B11 is inclusive")
		assert.Equal(t, name, n.Labels.AlertName())
	})

	t.Run("one over the bound rejects that alert", func(t *testing.T) {
		name := strings.Repeat("A", alerts.MaxAlertNameBytes+1)
		require.LessOrEqual(t, len(name), alerts.MaxLabelValueBytes,
			"the fixture must trip B11 and not B5, or it proves nothing about alertname")

		_, err := normalise(t, wireAlert(map[string]string{alerts.LabelAlertName: name}, nil))

		require.Error(t, err)
		assert.Equal(t, domain.ReasonLabelValueTooLarge, domain.ReasonFromError(err))
	})
}

// -------------------------------------------------- B7, B8, B19 (annotations)

// TestBoundAnnotations_B7AnnotationsPerAlert. Over the cap the EXCESS is dropped
// and the alert is KEPT: an annotation is display text and never identity (§C.9.3),
// so no amount of annotation abuse justifies losing the signal underneath it.
func TestBoundAnnotations_B7AnnotationsPerAlert(t *testing.T) {
	annotationsOf := func(n int) map[string]string {
		m := make(map[string]string, n)
		for i := range n {
			m[fmt.Sprintf("a%03d", i)] = "v"
		}
		return m
	}

	t.Run("exactly on the bound keeps every annotation and records nothing", func(t *testing.T) {
		in := annotationsOf(alerts.MaxAnnotations)

		n, err := normalise(t, wireAlert(nil, in))

		require.NoError(t, err)
		assert.Len(t, n.Annotations, alerts.MaxAnnotations, "32 annotations is legal; B7 is inclusive")
		assert.Empty(t, n.Notes, "nothing was dropped, so nothing is recorded")
	})

	t.Run("one over the bound drops the excess and keeps the alert", func(t *testing.T) {
		in := annotationsOf(alerts.MaxAnnotations + 1)

		n, err := normalise(t, wireAlert(nil, in))

		require.NoError(t, err, "⭐ B7 NEVER costs the alert")
		assert.Len(t, n.Annotations, alerts.MaxAnnotations)
		assert.Equal(t, []domain.Reason{domain.ReasonTooManyAnnotations}, reasons(n.Notes))
	})
}

// TestBoundAnnotations_B8AnnotationValueLength. Over the cap the VALUE is
// truncated with a marker and the alert is kept.
func TestBoundAnnotations_B8AnnotationValueLength(t *testing.T) {
	t.Run("exactly on the bound is untouched", func(t *testing.T) {
		value := strings.Repeat("v", alerts.MaxAnnotationValueBytes)

		n, err := normalise(t, wireAlert(nil, map[string]string{"summary": value}))

		require.NoError(t, err)
		assert.Equal(t, value, n.Annotations["summary"],
			"a 16384-byte annotation is legal; B8 is inclusive")
		assert.Empty(t, n.Notes)
	})

	t.Run("one over the bound truncates to the cap INCLUDING the marker", func(t *testing.T) {
		value := strings.Repeat("v", alerts.MaxAnnotationValueBytes+1)

		n, err := normalise(t, wireAlert(nil, map[string]string{"summary": value}))

		require.NoError(t, err, "B8 never costs the alert")
		got := n.Annotations["summary"]
		assert.Len(t, got, alerts.MaxAnnotationValueBytes,
			"⛔ the marker is INSIDE the cap. A truncation to the cap PLUS a marker "+
				"would leave layer 2 handing layer 6 a value that trips alerts_annotations_ck")
		assert.True(t, strings.HasSuffix(got, TruncationMarker),
			"the shortening is visible to a human reading the annotation")
		assert.Equal(t, []domain.Reason{domain.ReasonAnnotationTooLarge}, reasons(n.Notes))
	})
}

// TestBoundAnnotations_B19GrowthAcrossTheB8Bound is the ORDERING between B19 and
// B8, expressed as an on/over pair, and it is the one place where two bounds
// interact arithmetically.
//
// Sanitisation replaces a one-byte unstorable with a THREE-byte U+FFFD, so a value
// that is under the B8 cap on the wire can be over it after B19 has run. The
// boundary is therefore not `len(wire) > 16384` but `len(sanitised) > 16384`:
//
//	16382 wire bytes carrying one bad byte -> 16384 sanitised, exactly on B8
//	16383 wire bytes carrying one bad byte -> 16385 sanitised, one over B8
//
// Running B8 first would put both on the accepted side and hand layer 6 a value
// 16 385 bytes long.
func TestBoundAnnotations_B19GrowthAcrossTheB8Bound(t *testing.T) {
	// One trailing 0xff, which sanitises from one byte to three.
	oneBadByte := func(n int) string { return strings.Repeat("a", n-1) + "\xff" }

	t.Run("sanitising lands exactly on the B8 bound: no truncation", func(t *testing.T) {
		wire := oneBadByte(alerts.MaxAnnotationValueBytes - 2)

		n, err := normalise(t, wireAlert(nil, map[string]string{"summary": wire}))

		require.NoError(t, err)
		got := n.Annotations["summary"]
		assert.Len(t, got, alerts.MaxAnnotationValueBytes)
		assert.False(t, strings.HasSuffix(got, TruncationMarker),
			"nothing was over the cap, so nothing is truncated")
		assert.Equal(t, []domain.Reason{domain.ReasonAnnotationUnstorable}, reasons(n.Notes),
			"only the substitution is recorded")
	})

	t.Run("sanitising lands one over the B8 bound: truncated too", func(t *testing.T) {
		wire := oneBadByte(alerts.MaxAnnotationValueBytes - 1)

		n, err := normalise(t, wireAlert(nil, map[string]string{"summary": wire}))

		require.NoError(t, err)
		got := n.Annotations["summary"]
		assert.Len(t, got, alerts.MaxAnnotationValueBytes,
			"⛔ what leaves layer 2 satisfies B8, sanitisation included")
		assert.Empty(t, alerts.UnstorableReason(got))
		assert.ElementsMatch(t,
			[]domain.Reason{domain.ReasonAnnotationUnstorable, domain.ReasonAnnotationTooLarge},
			reasons(n.Notes),
			"both bounds fired and both are recorded separately")
	})
}

// -------------------------------------------------------- B12, B13 (the clock)

// TestNormalise_B12StartsAtWindow is the sanity window on `startsAt`, and it is
// the one per-alert bound whose violation is a DROP rather than a repair.
//
// It has to be: `startsAt` is the untrusted value that steers partition routing,
// and an alert claiming 2087 would demand a partition sixty years out (§C12).
//
// The clock is `platform/clock`'s fake. A wall-clock `time.Now()` here would make
// a 365-day window depend on the instant the suite ran, and the failure mode of
// that is a test that passes for a year.
func TestNormalise_B12StartsAtWindow(t *testing.T) {
	clk := clock.NewFake(fixedNow)
	now := clk.Now()

	at := func(startsAt time.Time) (Normalised, error) {
		return Normalise(Alert{
			Status:   StatusFiring,
			Labels:   map[string]string{alerts.LabelAlertName: "X"},
			StartsAt: startsAt,
		}, AlertOptions{Now: now})
	}

	accepted := map[string]time.Time{
		"exactly on the floor, now-365d":  now.Add(-domain.MaxStartsAtPast),
		"one nanosecond inside the floor": now.Add(-domain.MaxStartsAtPast).Add(time.Nanosecond),
		"now":                             now,
		"exactly on the ceiling, now+24h": now.Add(domain.MaxStartsAtFuture),
	}
	for what, startsAt := range accepted {
		t.Run("accepted/"+what, func(t *testing.T) {
			n, err := at(startsAt)

			require.NoError(t, err, "the window is INCLUSIVE at both ends")
			assert.True(t, n.StartsAt.Equal(startsAt), "the timestamp is stored as sent")
			assert.Equal(t, time.UTC, n.StartsAt.Location(), "normalised to UTC")
		})
	}

	rejected := map[string]time.Time{
		"one nanosecond before the floor":     now.Add(-domain.MaxStartsAtPast).Add(-time.Nanosecond),
		"one nanosecond past the ceiling":     now.Add(domain.MaxStartsAtFuture).Add(time.Nanosecond),
		"a broken clock claiming sixty years": now.AddDate(60, 0, 0),
	}
	for what, startsAt := range rejected {
		t.Run("rejected/"+what, func(t *testing.T) {
			_, err := at(startsAt)

			require.Error(t, err, "B12 drops THAT ALERT; the batch is still 202")
			assert.Equal(t, domain.ReasonTimestampOutOfWindow, domain.ReasonFromError(err))
		})
	}
}

// TestNormalise_B13EndsAtWindow is `endsAt`, and its verdict is the OPPOSITE of
// B12's on purpose: a violation CLAMPS to `startsAt` and KEEPS the alert.
//
// §B.3.2 is explicit that a backward-skewed upstream clock must never abort an
// ingest. The skew is measured and surfaced; it is never a reason to lose an alert.
// `endsAt` also steers no partition — `received_at` does — so there is nothing to
// protect by dropping.
func TestNormalise_B13EndsAtWindow(t *testing.T) {
	clk := clock.NewFake(fixedNow)
	now := clk.Now()
	startsAt := now.Add(-time.Minute)

	at := func(endsAt time.Time) (Normalised, error) {
		return Normalise(Alert{
			Status:   StatusFiring,
			Labels:   map[string]string{alerts.LabelAlertName: "X"},
			StartsAt: startsAt,
			EndsAt:   endsAt,
		}, AlertOptions{Now: now})
	}

	kept := map[string]time.Time{
		"the Go zero time, which means `no end time known`": {},
		"exactly equal to startsAt":                         startsAt,
		"exactly on the ceiling, now+365d":                  now.Add(domain.MaxEndsAtFuture),
		"one nanosecond inside the ceiling":                 now.Add(domain.MaxEndsAtFuture).Add(-time.Nanosecond),
	}
	for what, endsAt := range kept {
		t.Run("unclamped/"+what, func(t *testing.T) {
			n, err := at(endsAt)

			require.NoError(t, err)
			assert.False(t, n.Clamped, "the window is INCLUSIVE at the ceiling")
			assert.True(t, n.EndsAt.Equal(endsAt), "the timestamp is stored as sent")
			assert.Empty(t, reasons(n.Notes))
		})
	}

	clamped := map[string]time.Time{
		"one nanosecond past the ceiling": now.Add(domain.MaxEndsAtFuture).Add(time.Nanosecond),
		"one nanosecond before startsAt":  startsAt.Add(-time.Nanosecond),
	}
	for what, endsAt := range clamped {
		t.Run("clamped/"+what, func(t *testing.T) {
			n, err := at(endsAt)

			require.NoError(t, err, "⭐ B13 NEVER costs the alert")
			assert.True(t, n.Clamped)
			assert.True(t, n.EndsAt.Equal(startsAt), "clamped to startsAt, not to the ceiling")
			assert.Equal(t, []domain.Reason{domain.ReasonTimestampOutOfWindow}, reasons(n.Notes),
				"the skew is recorded under the same reason as B12: the upstream clock "+
					"disagrees with ours by more than oto will model")
		})
	}
}

// ---------------------------------------------------------------------- B14

// TestNormalise_B14GeneratorURLLength. `generatorURL` is a deep link and never
// identity, so a shortened one costs a click and nothing else — truncate, keep the
// alert, and record NOTHING. §L.3.2's B14 row assigns no reason, and the reason
// enum is closed.
func TestNormalise_B14GeneratorURLLength(t *testing.T) {
	t.Run("exactly on the bound is untouched", func(t *testing.T) {
		url := strings.Repeat("u", alerts.MaxGeneratorURLBytes)
		a := wireAlert(nil, nil)
		a.GeneratorURL = url

		n, err := normalise(t, a)

		require.NoError(t, err)
		assert.Equal(t, url, n.GeneratorURL, "8192 bytes is legal; B14 is inclusive")
		assert.Empty(t, n.Notes)
	})

	t.Run("one over the bound is truncated and the alert kept", func(t *testing.T) {
		a := wireAlert(nil, nil)
		a.GeneratorURL = strings.Repeat("u", alerts.MaxGeneratorURLBytes+1)

		n, err := normalise(t, a)

		require.NoError(t, err)
		assert.Len(t, n.GeneratorURL, alerts.MaxGeneratorURLBytes)
		assert.Empty(t, n.Notes, "B14 assigns no rejection reason")
	})
}

// ---------------------------------------------------------------------- B16

// TestDecode_B16NestingDepth is the decoder's nesting ceiling, and the depth check
// runs BEFORE unmarshalling rather than after — `encoding/json` will happily
// recurse through a hundred thousand levels of `[[[[…]]]]` and exhaust the
// goroutine stack before any post-hoc check could run.
//
// B16 is one of only two bounds that answer a 4xx (400), and it is defensible for
// exactly one reason: the same bytes could never decode on a retry, so no retry is
// being denied. The row still goes to `ingest_rejections` — asserted end to end in
// test/integration/ingest_bounds_test.go.
func TestDecode_B16NestingDepth(t *testing.T) {
	t.Run("exactly on the bound decodes", func(t *testing.T) {
		_, err := Decode(nestedObject(domain.MaxJSONDepth))

		require.NoError(t, err, "depth 32 is legal; B16 is inclusive")
	})

	t.Run("one over the bound is undecodable", func(t *testing.T) {
		_, err := Decode(nestedObject(domain.MaxJSONDepth + 1))

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTooDeep)
		assert.Equal(t, domain.ReasonUndecodable, domain.ReasonFromError(err))
	})

	t.Run("arrays count toward the same depth as objects", func(t *testing.T) {
		// 31 objects wrapping one array is 32; wrapping two is 33. A depth counter
		// that tracked only `{` would let an array bomb straight through.
		onTheBound := []byte(strings.Repeat(`{"a":`, domain.MaxJSONDepth-1) + `[]` +
			strings.Repeat(`}`, domain.MaxJSONDepth-1))
		require.NoError(t, CheckDepth(onTheBound, domain.MaxJSONDepth))

		oneOver := []byte(strings.Repeat(`{"a":`, domain.MaxJSONDepth-1) + `[[]]` +
			strings.Repeat(`}`, domain.MaxJSONDepth-1))
		assert.ErrorIs(t, CheckDepth(oneOver, domain.MaxJSONDepth), ErrTooDeep)
	})

	t.Run("a real envelope nested past the bound still fails", func(t *testing.T) {
		// The realistic shape: a custom `payload:` template emitting a deeply nested
		// object where a label value belongs.
		deep := strings.Repeat(`{"a":`, domain.MaxJSONDepth) + `{}` +
			strings.Repeat(`}`, domain.MaxJSONDepth)
		body := []byte(`{"version":"4","alerts":[{"labels":{"alertname":"X"},"x":` + deep + `}]}`)

		_, err := Decode(body)

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTooDeep)
	})
}

// TestDecode_ABodyThatIsNotAnEnvelopeFailsSoft is the other half of the
// `undecodable` reason, and it is why B16's row in §L.3.2 is not only about depth:
// a custom `payload:` template is Alertmanager's unsupported escape hatch and can
// emit any shape at all.
//
// Each of these is RECORDED and then answered 400 — never silently discarded.
func TestDecode_ABodyThatIsNotAnEnvelopeFailsSoft(t *testing.T) {
	for what, body := range map[string]string{
		"an empty body":     ``,
		"a bare scalar":     `42`,
		"a bare string":     `"firing"`,
		"a top-level array": `[{"version":"4"}]`,
		"trailing content":  `{"version":"4"} {"version":"4"}`,
		"truncated json":    `{"version":"4","alerts":[`,
		"a null":            `null`,
	} {
		t.Run(what, func(t *testing.T) {
			_, err := Decode([]byte(body))

			require.Error(t, err)
			assert.Equal(t, domain.ReasonUndecodable, domain.ReasonFromError(err),
				"the body was not a webhook payload, which is exactly what `undecodable` means")
		})
	}
}

// TestDecode_UnknownFieldsAreNotABound guards the boundary from the other side.
//
// ⛔ THE DECODER IS LENIENT AND MUST STAY LENIENT (§L.3.1). Alertmanager emits
// `routeLabels`, which is absent from the published documentation, and Grafana
// Unified Alerting emits a superset. Rejecting an unknown field would break on the
// next Alertmanager release AND DELETE ALERTS, because the only way to signal a
// rejection is a 4xx and a 4xx is permanent loss.
func TestDecode_UnknownFieldsAreNotABound(t *testing.T) {
	body := []byte(`{"version":"4","status":"firing","orgId":7,"title":"t","state":"alerting",
	                 "routeLabels":{"team":"core"},"imageURL":"https://example.test/i.png",
	                 "alerts":[{"status":"firing","labels":{"alertname":"X"},"wat":{"deep":true}}]}`)

	env, err := Decode(body)

	require.NoError(t, err, "an unknown field is not a bound and must never be one")
	assert.Len(t, env.Alerts, 1)
	assert.Equal(t, map[string]string{"team": "core"}, env.RouteLabels)
}
