package repository

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/alerts/domain"
)

// ⭐⭐ WHY THIS FILE EXISTS.
//
// `alert_events` is append-only, monthly-partitioned and retained thirteen
// months, and TWO separate decisions left strings on disk that the current
// vocabulary does not use:
//
//   - migration 00051 RETIRED `group.member_joined` and `group.member_left` —
//     the fact stopped existing, the rows did not;
//   - migration 00052 RENAMED the firing episode and deliberately did NOT rewrite
//     the eight `occurrence.*` lifecycle values, because an UPDATE across
//     thirteen months of partitions is the deploy that is down.
//
// Both decisions rest on the same claim: `eventRow.toDomain` can still read
// those rows. `toDomain` is the ONE place a persisted `alert_events.type`
// becomes a `domain.EventType` — every timeline in the product comes through it
// — and its failure mode is not a wrong answer, it is
// `errs.Internal("event_type_invalid")`, which renders as a timeline that ERRORS
// INSTEAD OF RENDERING.
//
// ⛔ AND NOTHING ELSE WOULD CATCH IT. Dropping a row from
// `domain.legacySpellings`, or removing a retired value from the closed set,
// leaves `go build` clean and every write path green: the strings only arrive
// from a database, and the only test that touched them counted them.
//
// This is the read half. The write half — that the retired pair is REFUSED by
// `alerts/service.AppendTimelineEvent` — is in `seam_retired_test.go`, and the
// two together are the asymmetry: readable forever, writable never.

// historyRow is one `alert_events` row as it sits on disk, with the type left as
// the caller's string. Everything else is the minimum a valid row carries.
func historyRow(typ string) *eventRow {
	alertID := uuid.New()
	at := time.Date(2025, 2, 14, 9, 30, 0, 0, time.UTC)
	return &eventRow{
		id:         uuid.New(),
		orgID:      uuid.New(),
		alertID:    &alertID,
		typ:        typ,
		occurredAt: at,
		recordedAt: at,
		actorKind:  domain.ActorSystem.String(),
		summary:    "a fact recorded before the rename",
	}
}

// TestHistoricalRowsWithRetiredTypesStillDeserialise. Retired is not deleted.
func TestHistoricalRowsWithRetiredTypesStillDeserialise(t *testing.T) {
	t.Parallel()

	for _, want := range []domain.EventType{
		domain.EventGroupMemberJoined,
		domain.EventGroupMemberLeft,
	} {
		t.Run(want.String(), func(t *testing.T) {
			ev, err := historyRow(want.String()).toDomain()
			if err != nil {
				t.Fatalf("a row carrying %s no longer reads: %v\n\n"+
					"`alert_events` still contains these. Removing the value from the closed "+
					"set does not remove the rows — it turns every timeline holding one into "+
					"an error response.", want, err)
			}
			if ev.Type() != want {
				t.Errorf("type = %s, want %s", ev.Type(), want)
			}
			if !ev.Type().Retired() {
				t.Errorf("%s read back without reporting itself retired, so the one thing "+
					"stopping it being written again does not apply to it", want)
			}
		})
	}
}

// TestHistoricalRowsWithPreRenameSpellingsCanonicaliseOnRead is migration 00052's
// entire read-side promise, exercised through the real mapper rather than through
// the map it consults.
//
// ⭐ THE ROW IS THE OLD WORD AND THE VALUE IS THE NEW ONE. That is what lets
// `alert_events` keep thirteen months of `occurrence.*` on disk while the API,
// the generated client and the UI know only `case.*`.
func TestHistoricalRowsWithPreRenameSpellingsCanonicaliseOnRead(t *testing.T) {
	t.Parallel()

	// Read out of the closed set rather than listed, so a renamed fact that lost
	// its pre-rename spelling fails here instead of being quietly untested.
	var renamed []domain.EventType
	for _, ty := range domain.AllEventTypes() {
		if strings.HasPrefix(ty.String(), "case.") {
			renamed = append(renamed, ty)
		}
	}
	if len(renamed) != 8 {
		t.Fatalf("found %d `case.*` types, want the 8 ADR 0036 renamed", len(renamed))
	}

	for _, want := range renamed {
		// vocab:allow — the pre-rename spelling, reconstructed as the DATA a row holds.
		persisted := "occurrence." + strings.TrimPrefix(want.String(), "case.")

		t.Run(persisted, func(t *testing.T) {
			ev, err := historyRow(persisted).toDomain()
			if err != nil {
				t.Fatalf("a row written before ADR 0036 no longer reads: %v\n\n"+
					"Migration 00052 left these eight strings on disk on purpose — rewriting "+
					"them would rewrite every lifecycle row oto has inside one goose "+
					"transaction. The read is the whole of the other side of that bargain.",
					err)
			}
			if ev.Type() != want {
				t.Fatalf("type = %s, want %s — the row canonicalises to the wrong fact",
					ev.Type(), want)
			}
			if ev.Type().String() == persisted {
				t.Errorf("the pre-rename spelling reached a caller. `AllEventTypes` and " +
					"therefore `components.schemas.AlertEventType` list only the new word, so " +
					"this value cannot be described on the wire.")
			}
		})
	}
}

// TestUnknownEventTypesAreStillRefusedOnRead is the control. Everything above
// asserts that a string the current vocabulary does not use is accepted, and a
// mapper that accepted ANY string would satisfy all of it while turning the
// closed enum into an open one from the database side.
func TestUnknownEventTypesAreStillRefusedOnRead(t *testing.T) {
	t.Parallel()

	for _, typ := range []string{
		"",
		"case.invented",
		"group.member_rejoined",
		// vocab:allow — a plausible mistyping of a real persisted spelling; it must NOT be read as one.
		"occurrence.unaknowledged",
		"alert.created.extra",
	} {
		t.Run(typ, func(t *testing.T) {
			if _, err := historyRow(typ).toDomain(); err == nil {
				t.Errorf("%q was accepted as an alert_events.type. The set is closed; a mapper "+
					"that admits anything makes the database the vocabulary.", typ)
			}
		})
	}
}
