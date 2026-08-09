// Payload cap: the Go bound and `ui_events_payload_ck` measured against a REAL
// Postgres.
//
// These tests exist because the cap used to be measured with two different rulers.
// `streaming/domain.NewAppend` bounded `len(payload)` — the RAW JSON TEXT — while
// `ui_events_payload_ck` bounded `pg_column_size(payload)` — the STORED JSONB. The
// second is not a function of the first, and the divergence went both ways:
//
//   - TOO LAX: a compact object of many small fields passed Go at 4096 raw bytes
//     and stored FAR larger, tripping the CHECK. `streaming/repository.mapErr`
//     turns a 23514 into `errs.KindInternal`, so that was a 500 on the SSE write
//     path — precisely what checking in Go was supposed to prevent.
//   - TOO STRICT: a whitespace-padded payload was refused by Go for bytes jsonb
//     discards.
//
// NEITHER HALF IS PROVABLE IN A PURE PACKAGE. `pg_column_size` is Postgres's
// answer and nothing else can give it, which is why the proof lives here, on the
// real schema, through the real repository, and actually inserts rows.

package integration

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/streaming/domain"
	"github.com/thulasiram/oto/internal/streaming/repository"
	"github.com/thulasiram/oto/test/harness"
)

// jsonKeyBytes is every byte a JSON string may carry unescaped, minus `"` and
// `\`: 0x20..0x7F. Control bytes below 0x20 must be escaped and would therefore
// cost MORE text per key, and anything above 0x7F is multi-byte UTF-8, likewise.
// So this is the cheapest alphabet there is, and it bounds how many one-character
// keys an object can have before it must start spending two.
func jsonKeyBytes() []string {
	out := make([]string, 0, 96)
	for c := 0x20; c <= 0x7F; c++ {
		if c == '"' || c == '\\' {
			continue
		}
		out = append(out, string(rune(c)))
	}
	return out
}

// worstCasePayload builds the compact JSON object of EXACTLY `size` bytes with the
// largest possible stored jsonb size.
//
// The maximisation is spelled out on domain.MaxStoredPayloadBytes. In short: jsonb
// spends 8 bytes of JEntry per PAIR plus 8 bytes for the smallest possible number
// (a jsonb number is a full `numeric`; `pg_column_size(1::numeric)` = 8) against
// one byte of text, so the worst object is the one with the MOST pairs and
// single-digit numeric values. Keys must be DISTINCT — jsonb keeps only the last
// of a duplicate key, so duplicates would make the row smaller — hence the
// shortest-first walk over jsonKeyBytes.
//
// The final value is then widened with extra digits to land on exactly `size`.
// That costs nothing stored: `numeric` packs four decimal digits per NumericDigit,
// so 1 and 9999 are both 8 bytes.
func worstCasePayload(t *testing.T, size int) []byte {
	t.Helper()

	alpha := jsonKeyBytes()
	keys := make([]string, 0, len(alpha)+len(alpha)*len(alpha))
	keys = append(keys, alpha...)
	for _, a := range alpha {
		for _, c := range alpha {
			keys = append(keys, a+c)
		}
	}

	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		piece := `"` + k + `":1`
		if i > 0 {
			piece = "," + piece
		}
		if b.Len()+len(piece)+1 > size {
			break
		}
		b.WriteString(piece)
	}
	// Widen the last value until the text lands exactly on the cap. `1` becomes
	// `1999…`, which is still one NumericDigit for the first four digits and stays
	// cheap thereafter, so this pads the TEXT without shrinking the overhead ratio.
	b.WriteString(strings.Repeat("9", size-b.Len()-1))
	b.WriteByte('}')

	out := []byte(b.String())
	require.Len(t, out, size, "the fixture must sit exactly on the requested size")
	return out
}

// storedSize is Postgres's own verdict on a payload: pg_column_size of the jsonb,
// which is the quantity ui_events_payload_ck bounds.
func storedSize(t *testing.T, h *harness.H, payload []byte) int {
	t.Helper()

	var n int
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT pg_column_size($1::jsonb)`, string(payload)).Scan(&n))
	return n
}

// TestWorstCaseJSONBOverheadStoresUnderTheDDLCap is the arithmetic on
// domain.MaxStoredPayloadBytes, checked rather than asserted.
//
// It is the test that would have caught the original bug: it constructs the
// LARGEST-STORING object the Go cap admits and asks Postgres what it costs. The
// answer is 10680 bytes for 4096 bytes of text — 2.6x — which is over the old 4096
// CHECK by a factor of two and a half, and comfortably under the 16384 that 00031
// installed.
func TestWorstCaseJSONBOverheadStoresUnderTheDDLCap(t *testing.T) {
	t.Parallel()
	h := harness.New(t)

	payload := worstCasePayload(t, domain.MaxPayloadBytes)

	// Go accepts it: it is a compact JSON object of exactly MaxPayloadBytes.
	ev, err := domain.NewAppend(domain.KindAlertUpserted, uuid.New(), payload)
	require.NoError(t, err, "a compact object at exactly the cap is legal")
	require.Len(t, ev.Payload, domain.MaxPayloadBytes, "already compact: nothing to strip")

	stored := storedSize(t, h, payload)
	t.Logf("compact text %d bytes → stored jsonb %d bytes (%.2fx)",
		len(payload), stored, float64(stored)/float64(len(payload)))

	// THE BUG, RESTATED AS AN ASSERTION. This is the object that passed the Go
	// check and then violated the 4096 CHECK.
	assert.Greater(t, stored, 4096,
		"the old CHECK bounded pg_column_size at 4096; this payload passed Go and stored larger — that was the 23514/500")

	// THE FIX. The DDL bound now clears the worst case with room to spare.
	assert.LessOrEqual(t, stored, domain.MaxStoredPayloadBytes,
		"the DDL backstop must exceed the worst-case stored size of anything Go accepts")

	// Pin the measured figure. The arithmetic on domain.MaxStoredPayloadBytes
	// predicts 10680; a Postgres that disagreed would invalidate the headroom
	// calculation, and that must fail here rather than in production.
	assert.Equal(t, 10680, stored,
		"jsonb overhead changed; re-derive the headroom on domain.MaxStoredPayloadBytes")
}

// TestUIEventPayloadAtTheGoCapStores is the storage proof: the payload shape that
// used to produce a 23514 now INSERTS.
//
// It goes through the real repository rather than a hand-written INSERT, so the
// CHECK is met on the actual write path, and it asserts on the row read back —
// what was stored, not merely that nothing errored.
func TestUIEventPayloadAtTheGoCapStores(t *testing.T) {
	t.Parallel()
	h := harness.New(t)

	orgID := uuid.New()
	scope, err := db.NewTenantScope(orgID)
	require.NoError(t, err)
	repo := repository.NewEventRepository(h.Pool)

	t.Run("many small fields at exactly the Go cap", func(t *testing.T) {
		payload := worstCasePayload(t, domain.MaxPayloadBytes)
		require.Greater(t, storedSize(t, h, payload), 4096,
			"this fixture must be one the OLD constraint would have rejected, or it proves nothing")

		ev, err := domain.NewAppend(domain.KindAlertUpserted, uuid.New(), payload)
		require.NoError(t, err)

		got, err := repo.Append(h.Ctx, scope, ev)
		require.NoError(t, err,
			"a payload the domain accepts must survive ui_events_payload_ck; a 23514 here is a 500 on the SSE write path")
		assert.Positive(t, got.Seq)
		assert.JSONEq(t, string(payload), string(got.Payload),
			"the row read back is the payload that was validated")
	})

	t.Run("whitespace-padded payload is accepted and stored compact", func(t *testing.T) {
		// Over the Go cap as raw text, trivial as jsonb. This used to be refused
		// outright; it must now store, and store WITHOUT the padding.
		padded := []byte(`{"alert_id":"` + uuid.New().String() + `"` +
			strings.Repeat(" ", domain.MaxPayloadBytes) + `}`)
		require.Greater(t, len(padded), domain.MaxPayloadBytes)

		ev, err := domain.NewAppend(domain.KindOccurrenceUpserted, uuid.New(), padded)
		require.NoError(t, err, "whitespace is insignificant to JSON and free in jsonb")

		got, err := repo.Append(h.Ctx, scope, ev)
		require.NoError(t, err)
		assert.NotContains(t, string(got.Payload), "  ",
			"the compacted form is what was measured and therefore what must be stored")
		assert.Less(t, storedSize(t, h, got.Payload), 128,
			"the row Postgres actually holds is tiny — refusing it was a rejection with no cause")
	})
}

// TestUIEventsPayloadCheckIsStillABackstop — relaxing the bound must not have
// disabled it. The CHECK is what catches a writer that bypassed NewAppend, and
// both of its clauses must still bite.
func TestUIEventsPayloadCheckIsStillABackstop(t *testing.T) {
	t.Parallel()
	h := harness.New(t)

	insert := func(payload string) error {
		_, err := h.Pool.Exec(h.Ctx,
			`INSERT INTO ui_events (org_id, kind, resource, resource_id, payload)
			 VALUES ($1, 'alert.upserted', 'alert', $2, $3::jsonb)`,
			uuid.New(), uuid.New(), payload)
		return err
	}

	// Over the new bound: rejected. 8192 bytes of this shape stores at roughly
	// 2.6x, i.e. ~21 kB, which clears 16384 without sitting on the boundary.
	over := worstCasePayload(t, 8192)
	require.Greater(t, storedSize(t, h, over), domain.MaxStoredPayloadBytes,
		"the fixture must exceed the DDL bound as STORED bytes")
	err := insert(string(over))
	require.Error(t, err, "the backstop must still reject an oversized envelope")
	assert.Contains(t, err.Error(), "ui_events_payload_ck",
		"the constraint NAME is a runtime contract (CONTEXT.md §6, SPEC §L.9) and must survive the migration")

	// Not an object: still rejected. Relaxing the size clause must not have
	// loosened the shape clause.
	err = insert(`[1,2,3]`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ui_events_payload_ck")

	// And the ordinary envelope still stores.
	require.NoError(t, insert(`{"state":"firing"}`))
}

// TestUIEventsPayloadCheckAppliesToEveryPartition — `ui_events` is PARTITION BY
// RANGE (at) with hourly partitions, and 00031 names only the PARENT.
//
// That is correct but it is not obvious, so it is asserted rather than assumed: a
// CHECK on a partitioned table is copied to every partition, DROP on the parent
// removes the copies and ADD on the parent reinstalls them. A migration that had
// touched only the parent's own catalog row would leave the partitions — which is
// where the rows actually live, and therefore where the constraint actually fires
// — still carrying the old 4096 bound.
func TestUIEventsPayloadCheckAppliesToEveryPartition(t *testing.T) {
	t.Parallel()
	h := harness.New(t)

	type row struct {
		table string
		def   string
		local bool
		inh   int
	}

	load := func() []row {
		rows, err := h.Pool.Query(h.Ctx, `
			SELECT c.conrelid::regclass::text,
			       pg_get_constraintdef(c.oid),
			       c.conislocal,
			       c.coninhcount
			  FROM pg_constraint c
			 WHERE c.conname = 'ui_events_payload_ck'
			 ORDER BY 1`)
		require.NoError(t, err)
		defer rows.Close()

		var out []row
		for rows.Next() {
			var r row
			require.NoError(t, rows.Scan(&r.table, &r.def, &r.local, &r.inh))
			out = append(out, r)
		}
		require.NoError(t, rows.Err())
		return out
	}

	got := load()
	require.NotEmpty(t, got, "ui_events_payload_ck must exist")

	var parents, partitions int
	for _, r := range got {
		assert.Contains(t, r.def, "16384", "%s still carries the OLD bound", r.table)
		assert.Contains(t, r.def, "jsonb_typeof", "%s lost the shape clause", r.table)

		if r.table == "ui_events" {
			parents++
			assert.True(t, r.local, "the parent owns the constraint")
			assert.Zero(t, r.inh, "the parent inherits from nobody")
			continue
		}
		partitions++
		assert.False(t, r.local, "%s must INHERIT the constraint, not own a copy", r.table)
		assert.Equal(t, 1, r.inh, "%s must be attached to exactly one parent", r.table)
	}
	assert.Equal(t, 1, parents)

	// The harness's template runs oto_partitions_manage(), so the clone already has
	// the current hour plus the ones ahead of it.
	var want int
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT count(*) FROM pg_inherits WHERE inhparent = 'ui_events'::regclass`).Scan(&want))
	require.Positive(t, want, "ui_events must actually be partitioned, or this test proves nothing")
	assert.Equal(t, want, partitions, "every existing partition must carry the relaxed bound")

	// A partition created AFTER the migration must inherit it too — the partition
	// manager runs hourly forever, so this is the case that outlives the release.
	h.Exec(`CREATE TABLE ui_events_p_probe PARTITION OF ui_events
	          FOR VALUES FROM ('2099-01-01 00:00:00+00') TO ('2099-01-01 01:00:00+00')`)

	after := load()
	assert.Len(t, after, len(got)+1, "the new partition must have picked the constraint up")
	for _, r := range after {
		if r.table == "ui_events_p_probe" {
			assert.Contains(t, r.def, "16384",
				"a partition created after the migration must inherit the relaxed bound")
		}
	}
}
