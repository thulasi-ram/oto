package decode_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/ingestion/decode"
	"github.com/thulasiram/oto/test/harness"
)

func TestMain(m *testing.M) { harness.Main(m) }

// This file is about ONE defect, and it is a split between two representations
// of the same request.
//
// `ApplyBatchBounds` sanitises the decoded ENVELOPE: B19 in `bounds.go` drops
// unstorable annotation names and replaces unstorable values with U+FFFD, using
// `alerts.UnstorableReason` / `alerts.SanitiseText`. But the envelope is not what
// is stored. `accept.go` persists `PersistedPayload(cmd.Body, …)` — the RAW body
// — and `PersistedPayload` returns those bytes VERBATIM whenever there is nothing
// to redact and nothing to truncate, which is the overwhelmingly common case.
// So B19 cleans a copy that is thrown away, and the dirty original is what lands
// in `ingest_batches.payload JSONB NOT NULL` (00006_ingestion.sql).
//
// ⛔ THE KILLER IS THE ESCAPE SEQUENCE, NOT A RAW BYTE, and that distinction is
// why this was reported twice and reproduced never. A raw 0x00 inside a JSON
// string is INVALID JSON: `json.Unmarshal` refuses it at the door and the request
// is rejected cleanly, with a row to show for it. The six-character ESCAPE that
// denotes the same code point is VALID JSON, decodes without complaint, survives
// the verbatim passthrough untouched, and is then refused by Postgres — which
// cannot represent U+0000 in `text` or `jsonb` at all.
//
// The failure that follows is the worst shape available: the INSERT that fails is
// the very row whose job is to record the failure, so the accept returns 503 with
// no `ingest_rejections` row, and Alertmanager retries the same poisoned body
// forever.

// nulEscape is the six ASCII characters backslash, u, zero, zero, zero, zero.
//
// ⛔ IT IS SPELLED AS BYTES ON PURPOSE. Written literally it is one editor, one
// linter or one copy-paste away from becoming the actual NUL it merely denotes —
// which is a different input entirely, takes a different path, and is already
// handled correctly. Conflating the two is precisely how this defect survived
// being reported twice.
var nulEscape = string([]byte{'\\', 'u', '0', '0', '0', '0'})

// amBody is a body Alertmanager can really send. The escape sits in an ANNOTATION
// value, which is where free prose from a rule author reaches oto — a `summary:`
// templated from a metric label that happened to carry a NUL.
func amBody() []byte {
	return []byte(`{"version":"4","groupKey":"{}:{alertname=\"DiskFull\"}",` +
		`"status":"firing","receiver":"oto","alerts":[{"status":"firing",` +
		`"labels":{"alertname":"DiskFull","severity":"critical"},` +
		`"annotations":{"summary":"disk at 91% ` + nulEscape + `"},` +
		`"startsAt":"2026-08-09T00:00:00Z"}]}`)
}

// TestPersistedPayloadReturnsCleanBodiesVerbatim guards the contract the fix must
// NOT trade away.
//
// The verbatim passthrough is deliberate (§C.9.2, and the ⭐ on PersistedPayload):
// re-encoding every body would silently drop the Grafana and custom-template
// fields that `Envelope` does not model, from the one artefact that exists to
// answer "what actually arrived". The storability pass is allowed to rewrite a
// poisoned body; it is not allowed to start rewriting clean ones.
func TestPersistedPayloadReturnsCleanBodiesVerbatim(t *testing.T) {
	t.Parallel()

	clean := []byte(`{"version":"4","status":"firing","orgId":7,"title":"a Grafana field oto does not model",` +
		`"alerts":[{"status":"firing","labels":{"alertname":"DiskFull"},` +
		`"annotations":{"summary":"disk at 91%"}}]}`)

	payload, err := decode.PersistedPayload(clean, nil, 0)
	require.NoError(t, err)

	assert.Equal(t, string(clean), string(payload),
		"a body with nothing to redact, truncate or sanitise must come back byte for byte")
}

// TestPersistedPayloadSanitisesTheEscape is the other half of the fix: the
// poisoned body IS rewritten, and the escape is gone from the bytes that reach
// storage.
func TestPersistedPayloadSanitisesTheEscape(t *testing.T) {
	t.Parallel()

	payload, err := decode.PersistedPayload(amBody(), nil, 0)
	require.NoError(t, err, "a NUL escape is valid JSON, so nothing here should refuse it")

	assert.NotContains(t, string(payload), nulEscape,
		"the escape must not survive into the bytes handed to a JSONB column")
	assert.Contains(t, string(payload), "disk at 91%",
		"sanitising replaces the offending code point, it does not discard the prose around it")
	assert.Contains(t, string(payload), "DiskFull",
		"and it leaves the rest of the document intact")
}

// TestPersistedPayloadIsStorableAsJSONB is the defect.
//
// It asserts the property that actually matters and that nothing currently
// enforces: whatever `PersistedPayload` returns must be something Postgres can
// store, because the caller's next move is to write it to a JSONB column inside
// the transaction that would otherwise record the rejection.
//
// The probe is the bare cast rather than an INSERT into `ingest_batches`, because
// the cast IS the operation that fails — an INSERT would need an org, a source and
// a scope to reach the same error, and would prove nothing extra.
func TestPersistedPayloadIsStorableAsJSONB(t *testing.T) {
	t.Parallel()

	h := harness.New(t)

	payload, err := decode.PersistedPayload(amBody(), nil, 0)
	require.NoError(t, err)

	var round []byte
	err = h.Pool.QueryRow(h.Ctx, `SELECT $1::jsonb`, []byte(payload)).Scan(&round)
	require.NoError(t, err,
		"PersistedPayload returned bytes Postgres cannot store: the accept INSERT fails, "+
			"the 503 makes Alertmanager retry the same body forever, and the row that would "+
			"have recorded the rejection is the row that failed")

	assert.NotContains(t, string(round), "\x00",
		"nothing that reaches storage may carry a NUL")
}
