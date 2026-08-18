package domain

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ⭐⭐ WHY THIS FILE EXISTS.
//
// Migration 00052 renamed the firing episode from Occurrence to Case and
// DELIBERATELY DID NOT REWRITE `alert_events`: the table is monthly-partitioned,
// append-only and retained thirteen months, so thirteen months of rows still
// spell the eight lifecycle facts `occurrence.*`. The whole of that decision
// rests on one map — `legacySpellings` — and on `NewEventType` canonicalising
// through it on the way in.
//
// ⛔ AND THAT MAP FAILS SILENTLY. Drop a row from it, or mistype one, and
// `go build` is clean, every existing test is green, and nothing goes wrong
// until a timeline containing the affected transition is read: `NewEventType`
// returns a validation error, `alerts/repository.eventFromRow` propagates it,
// and the timeline ERRORS RATHER THAN RENDERS — which is the exact outcome
// `retiredEventTypes`' neighbouring comment says the mechanism exists to
// prevent. The one assertion that existed before this file was a COUNT
// (`test/arch/eventtype_test.go`, `len(eventTypeValues()) == 44`), and a count
// survives dropping one spelling and adding a typo'd one.
//
// So every claim below is stated over the SET rather than over a restatement of
// the map: "every `case.*` type has its pre-rename spelling", not "these eight
// pairs". A restatement is a second copy of the bug.

// renamedPrefix and legacyPrefix are the two words for one subject. The eight
// renamed facts are exactly the members of `AllEventTypes` under the first.
const (
	renamedPrefix = "case."
	legacyPrefix  = "occurrence." // vocab:allow — the pre-rename spelling, in the test that proves it is still read.
)

// canonicalCaseTypes is the eight lifecycle values ADR 0036 renamed, read out of
// the closed set rather than listed here.
func canonicalCaseTypes() []EventType {
	var out []EventType
	for _, t := range AllEventTypes() {
		if strings.HasPrefix(t.String(), renamedPrefix) {
			out = append(out, t)
		}
	}
	return out
}

// TestLegacySpellingsCanonicaliseOnRead is the behaviour the ticket added:
// a pre-rename string arrives and the value that fact HAS NOW comes back.
func TestLegacySpellingsCanonicaliseOnRead(t *testing.T) {
	require.Len(t, legacySpellings, 8,
		"ADR 0036 renamed eight lifecycle facts; the translation table is that many rows or it is missing one")

	for legacy, want := range legacySpellings {
		t.Run(legacy, func(t *testing.T) {
			got, err := NewEventType(legacy)
			require.NoError(t, err,
				"a row written before ADR 0036 spells the fact this way and MUST still parse")
			assert.Equal(t, want, got)
			assert.False(t, got.IsZero())

			// ⛔ THE OLD WORD NEVER LEAVES THE PROCESS. A rename is not a
			// retirement: the client is told about `case.*` and only `case.*`, so
			// canonicalising to anything that still says the old word would put a
			// value on the wire that `components.schemas.AlertEventType` does not
			// list.
			assert.Equal(t, strings.TrimPrefix(legacy, legacyPrefix),
				strings.TrimPrefix(got.String(), renamedPrefix),
				"the fact is the same one; only the subject was renamed")
			assert.True(t, strings.HasPrefix(got.String(), renamedPrefix),
				"canonicalisation must land on the new word")
			assert.NotEqual(t, legacy, got.String())

			text, marshalErr := got.MarshalText()
			require.NoError(t, marshalErr)
			assert.Equal(t, got.String(), string(text),
				"every encoder sees the canonical value, not the persisted one")
		})
	}
}

// TestLegacySpellingsAreTotalOverTheRenamedFacts is the assertion a count cannot
// make: not "there are eight" but "there is one for each, and it is the right
// one".
//
// ⭐ IT READS THE MAP FROM BOTH ENDS. Dropping `occurrence.unacknowledged`
// fails the first half; adding `occurrence.unaknowledged` in its place fails the
// second. A test that only counted, or only walked the map, would pass on the
// pair of edits together — which is exactly how this kind of table decays.
func TestLegacySpellingsAreTotalOverTheRenamedFacts(t *testing.T) {
	canonical := canonicalCaseTypes()
	require.Len(t, canonical, 8,
		"the eight renamed lifecycle facts are T1, T7, T3, T4, T5, T6, T9 and T10; "+
			"a ninth `case.*` type needs a row in legacySpellings or an argument for why not")

	t.Run("every renamed fact has its pre-rename spelling", func(t *testing.T) {
		for _, ty := range canonical {
			want := legacyPrefix + strings.TrimPrefix(ty.String(), renamedPrefix)

			got, err := NewEventType(want)
			require.NoError(t, err,
				"%q is on disk in thirteen months of partitions and no longer parses — "+
					"reading that timeline errors instead of rendering", want)
			assert.Equal(t, ty, got)

			assert.Contains(t, ty.PersistedSpellings(), want,
				"a SQL predicate built from %s would silently stop matching every row "+
					"written before ADR 0036", ty)
		}
	})

	t.Run("every legacy spelling names a renamed fact", func(t *testing.T) {
		for legacy, ty := range legacySpellings {
			assert.True(t, strings.HasPrefix(legacy, legacyPrefix),
				"%q is not a pre-rename spelling of anything", legacy)
			assert.Equal(t, renamedPrefix+strings.TrimPrefix(legacy, legacyPrefix), ty.String(),
				"%q maps to %s, which is not the same fact under the new word — "+
					"one of the two is mistyped", legacy, ty)
			assert.Contains(t, eventTypes, ty.String(),
				"a legacy spelling must canonicalise onto a member of the closed set")
		}
	})

	t.Run("the map is injective", func(t *testing.T) {
		seen := map[string]string{}
		for legacy, ty := range legacySpellings {
			if prev, dup := seen[ty.String()]; dup {
				t.Errorf("%q and %q both canonicalise to %s, so one renamed fact has lost its "+
					"spelling and another has two", prev, legacy, ty)
			}
			seen[ty.String()] = legacy
		}
	})

	t.Run("no legacy spelling collides with a canonical value", func(t *testing.T) {
		// If it did, `NewEventType`'s closed-set branch would answer first and the
		// translation would never run — a dead row nobody could see was dead.
		for legacy := range legacySpellings {
			assert.NotContains(t, eventTypes, legacy,
				"%q is both a persisted-only spelling and a live contract value", legacy)
		}
	})
}

// TestCanonicalValuesNeverRoundTripToALegacySpelling is the other direction, and
// it is the half that protects the CONTRACT rather than the read.
func TestCanonicalValuesNeverRoundTripToALegacySpelling(t *testing.T) {
	legacy := map[string]struct{}{}
	for s := range legacySpellings {
		legacy[s] = struct{}{}
	}

	for _, ty := range AllEventTypes() {
		s := ty.String()
		assert.NotContains(t, legacy, s,
			"%s is on the wire contract and must not also be a pre-rename spelling", s)
		assert.False(t, strings.HasPrefix(s, legacyPrefix),
			"`AllEventTypes` is what generated clients accept and it lists the new word only")

		// Re-parsing a canonical value is a fixed point: the translation table is
		// consulted only when the closed set does not answer.
		got, err := NewEventType(s)
		require.NoError(t, err)
		assert.Equal(t, ty, got)
		assert.Equal(t, s, got.String())
	}

	for legacySpelling := range legacySpellings {
		canonical, err := NewEventType(legacySpelling)
		require.NoError(t, err)
		again, err := NewEventType(canonical.String())
		require.NoError(t, err)
		assert.Equal(t, canonical, again,
			"canonicalising twice must be the same as canonicalising once")
		assert.NotEqual(t, legacySpelling, again.String())
	}
}

// TestPersistedSpellingsIsWhatAPredicateNeeds. `PersistedSpellings` exists for
// one purpose — building `type IN (…)` — so it is tested against that purpose:
// canonical first, every on-disk spelling present, one element when there was no
// rename, and a fresh slice each call.
func TestPersistedSpellingsIsWhatAPredicateNeeds(t *testing.T) {
	t.Run("a renamed fact expands to both spellings, canonical first", func(t *testing.T) {
		for _, ty := range canonicalCaseTypes() {
			got := ty.PersistedSpellings()
			require.Len(t, got, 2, "%s has one pre-rename spelling and one canonical value", ty)
			assert.Equal(t, ty.String(), got[0],
				"canonical first: callers that take the head of this slice must get the live value")
			assert.Equal(t, legacyPrefix+strings.TrimPrefix(ty.String(), renamedPrefix), got[1])
		}
	})

	t.Run("a fact that was never renamed expands to itself", func(t *testing.T) {
		// ⛔ INCLUDING THE RETIRED ONES. `group.member_joined` is unwritable and
		// still readable, and a predicate over it must name exactly the one string
		// the column holds.
		for _, ty := range AllEventTypes() {
			if strings.HasPrefix(ty.String(), renamedPrefix) {
				continue
			}
			assert.Equal(t, []string{ty.String()}, ty.PersistedSpellings(),
				"%s was not renamed, so widening its predicate would widen it to nothing", ty)
		}
	})

	t.Run("every spelling is one the column may hold", func(t *testing.T) {
		persisted := map[string]struct{}{}
		for _, s := range AllPersistedEventTypes() {
			persisted[s] = struct{}{}
		}
		for _, ty := range AllEventTypes() {
			for _, s := range ty.PersistedSpellings() {
				assert.Contains(t, persisted, s,
					"%q would go into a SQL predicate but is not a value the column can hold",
					s)
			}
		}
	})

	t.Run("the caller gets a copy", func(t *testing.T) {
		// The slice is handed straight into query building; a caller that sorted or
		// appended to it in place would rewrite the shared table for the process.
		first := EventCaseOpened.PersistedSpellings()
		require.Len(t, first, 2)
		first[0] = "mutated"
		first[1] = "mutated"

		second := EventCaseOpened.PersistedSpellings()
		assert.Equal(t, EventCaseOpened.String(), second[0])
		assert.Equal(t, legacyPrefix+"opened", second[1])
	})
}

// TestAllPersistedEventTypesCoversTheColumn. This is the set `test/arch`'s
// event-type gate judges every SQL literal against, so its own totality is
// load-bearing: a value missing from here is a value that gate would call
// invented.
func TestAllPersistedEventTypesCoversTheColumn(t *testing.T) {
	got := AllPersistedEventTypes()

	t.Run("it is the contract set plus the pre-rename spellings", func(t *testing.T) {
		want := make([]string, 0, len(AllEventTypes())+len(legacySpellings))
		for _, ty := range AllEventTypes() {
			want = append(want, ty.String())
		}
		for legacy := range legacySpellings {
			want = append(want, legacy)
		}
		sort.Strings(want)
		assert.Equal(t, want, got)
	})

	t.Run("it is sorted and free of duplicates", func(t *testing.T) {
		assert.True(t, sort.StringsAreSorted(got),
			"callers build `IN (…)` lists from this; a stable order keeps the SQL stable")
		seen := map[string]struct{}{}
		for _, s := range got {
			_, dup := seen[s]
			assert.False(t, dup, "duplicate %q", s)
			seen[s] = struct{}{}
		}
	})

	t.Run("every member parses", func(t *testing.T) {
		// ⭐ THE WHOLE POINT. This set is defined as "what may be read off disk",
		// so a member `NewEventType` rejects is a row oto cannot render.
		for _, s := range got {
			ty, err := NewEventType(s)
			require.NoError(t, err, "%q is claimed to be readable and is not", s)
			assert.False(t, ty.IsZero())
		}
	})

	t.Run("it is strictly larger than the wire set", func(t *testing.T) {
		assert.Len(t, got, len(AllEventTypes())+len(legacySpellings))
		assert.Greater(t, len(got), len(AllEventTypes()),
			"if these two were equal the pre-rename spellings would have been lost")

		wire := map[string]struct{}{}
		for _, ty := range AllEventTypes() {
			wire[ty.String()] = struct{}{}
		}
		var extra []string
		for _, s := range got {
			if _, ok := wire[s]; !ok {
				extra = append(extra, s)
			}
		}
		require.Len(t, extra, 8)
		for _, s := range extra {
			assert.True(t, strings.HasPrefix(s, legacyPrefix),
				"the only thing the persisted set may add to the wire set is a pre-rename "+
					"spelling; %q is something else", s)
		}
	})

	t.Run("the retired types are in it", func(t *testing.T) {
		// Retired is not deleted: `alert_events` still contains these two, so a
		// predicate over the column has to be able to name them.
		for _, ty := range []EventType{EventGroupMemberJoined, EventGroupMemberLeft} {
			assert.Contains(t, got, ty.String())
			assert.True(t, ty.Retired())
		}
	})
}
