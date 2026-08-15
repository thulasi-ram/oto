package decode_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/ingestion/decode"
	"github.com/thulasiram/oto/test/harness"
)

func TestMain(m *testing.M) { harness.Main(m) }

// This file is about ONE defect with TWO faces, and both are a split between two
// representations of the same request.
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
// ⛔ THE KILLER IS AN ESCAPE SEQUENCE, NOT A RAW BYTE, and that distinction is
// why the first face was reported twice and reproduced never. A raw 0x00 inside a
// JSON string is INVALID JSON: `json.Unmarshal` refuses it at the door and the
// request is rejected cleanly, with a row to show for it. The six-character
// ESCAPE that denotes the same code point is VALID JSON, decodes without
// complaint, survives the verbatim passthrough untouched, and is then refused by
// Postgres — which cannot represent U+0000 in `text` or `jsonb` at all.
//
// ⛔ AND THE ESCAPE IS NOT ONLY U+0000. That was the hole the first fix left
// open. An UNPAIRED SURROGATE ESCAPE — a high half with no low half after it, or
// a low half with no high half before it — is likewise valid JSON per RFC 8259,
// is valid UTF-8 as raw bytes, carries no NUL, and is likewise refused by
// Postgres, with SQLSTATE 22P02 instead of 22P05. It is not exotic: ES2019
// well-formed `JSON.stringify` emits lone surrogates literally rather than
// failing, and so do Python with `surrogateescape` and Jackson. Any producer that
// is not Go can send one.
//
// The failure that follows either face is the worst shape available: the INSERT
// that fails is the very row whose job is to record the failure, so the accept
// returns 503 with no `ingest_rejections` row, and Alertmanager retries the same
// poisoned body forever.
//
// ⛔ THE PRE-SCAN'S COMPLETENESS IS THE CONTRACT. Go's decoder already folds a
// lone surrogate to U+FFFD, so `PersistedPayload`'s rewriting slow path repairs
// every case in the table below all by itself. The only way any of them reaches
// Postgres is `hasUnstorableBytes` failing to route the body to that path. That
// is why this test is a TABLE and not one case: it was one case, and that is
// precisely why the surrogate half survived a fix, a comment and a closed ticket.

// escape spells one `\uXXXX` JSON escape.
//
// ⛔ EVERY ESCAPE IN THIS FILE IS BUILT FROM BYTES, NEVER WRITTEN LITERALLY, and
// the reason is not style. Typed out in Go source, the backslash-u form is one
// editor, one formatter, one linter or one copy-paste away from becoming the code
// point it merely DENOTES — a completely different input that takes a completely
// different path and is already handled. Conflating the two is exactly how the
// NUL face of this defect was reported twice and reproduced never, and it
// misfired again while this very file was being written. The four hex digits are
// safe as a plain literal; the two characters in front of them are not.
func escape(hexDigits string) string {
	return string(append([]byte{'\\', 'u'}, hexDigits...))
}

// escapedBackslash is the two characters that denote ONE literal backslash.
var escapedBackslash = string([]byte{'\\', '\\'})

var (
	// nulEscape denotes U+0000. Postgres cannot store it in text or jsonb.
	nulEscape = escape("0000")

	// The surrogate halves, in both hex spellings RFC 8259 permits, because
	// Postgres refuses both spellings and a scan that folds only one is not a
	// scan.
	highUpper = escape("D800")
	highLower = escape("d800")
	lowUpper  = escape("DC00")
	lowLower  = escape("dc00")

	// A WELL-FORMED PAIR: U+1F600 GRINNING FACE, the way every non-Go producer
	// that escapes non-BMP text writes it. This one is storable and must keep the
	// verbatim fast path.
	pairUpper = escape("D83D") + escape("DE00")
	pairLower = escape("d83d") + escape("de00")
)

// annotationBody puts the fragment in an ANNOTATION VALUE, which is where free
// prose from a rule author reaches oto — a `summary:` templated from a metric
// label that happened to carry something Postgres will not take.
func annotationBody(fragment string) []byte {
	return []byte(`{"version":"4","groupKey":"{}:{alertname=\"DiskFull\"}",` +
		`"status":"firing","receiver":"oto","alerts":[{"status":"firing",` +
		`"labels":{"alertname":"DiskFull","severity":"critical"},` +
		`"annotations":{"summary":"disk at 91% ` + fragment + `"},` +
		`"startsAt":"2026-08-09T00:00:00Z"}]}`)
}

// keyBody puts the fragment in an annotation NAME. A key is a JSON string like
// any other, so Postgres refuses it for the same reason — and a scan that only
// looked at values would miss it.
func keyBody(fragment string) []byte {
	return []byte(`{"version":"4","status":"firing","receiver":"oto",` +
		`"alerts":[{"status":"firing","labels":{"alertname":"DiskFull"},` +
		`"annotations":{"summary` + fragment + `":"disk at 91%"}}]}`)
}

// amBody is the original reproduction: a body Alertmanager can really send, with
// the NUL escape in an annotation value.
func amBody() []byte { return annotationBody(nulEscape) }

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

// storabilityCase is one input and the three things that must be true of it.
type storabilityCase struct {
	name string
	body []byte

	// rawSQLSTATE is Postgres's verdict on the RAW body, asserted so that every
	// row of this table is proved to be a real hazard rather than a guess about
	// one. "" means Postgres accepts the raw bytes; anything else is the SQLSTATE
	// it refuses them with, and all three that appear here — 22P05, 22P02, 22021
	// — are exactly the three `service.isUnstorableBytes` recognises, which is the
	// backstop this scan exists to keep unreached.
	rawSQLSTATE string

	// wantVerbatim: PersistedPayload must return the input byte for byte. TRUE is
	// the assertion that keeps the fast path honest; FALSE is the assertion that
	// the body was routed to the rewriting slow path.
	wantVerbatim bool

	// wantErr: PersistedPayload must refuse the body outright. Only for input
	// that is not valid JSON, where refusing at the door IS the clean outcome —
	// the caller turns it into a 4xx with a recorded reason, which is the whole
	// point of not letting Postgres be the one to notice.
	wantErr bool
}

func storabilityCases() []storabilityCase {
	return []storabilityCase{
		{
			name:         "clean body, including fields oto does not model",
			body:         []byte(`{"version":"4","status":"firing","orgId":7,"alerts":[]}`),
			rawSQLSTATE:  "",
			wantVerbatim: true,
		},
		{
			name:         "every other escape JSON has, none of which is a hazard",
			body:         annotationBody(escapedBackslash + `n\t\"` + escape("00e9")),
			rawSQLSTATE:  "",
			wantVerbatim: true,
		},

		// ── U+0000, the face that was already closed ──────────────────────────
		{
			name:        "NUL escape in a value",
			body:        amBody(),
			rawSQLSTATE: "22P05",
		},
		{
			name:        "NUL escape in a key",
			body:        keyBody(nulEscape),
			rawSQLSTATE: "22P05",
		},
		{
			name: "raw NUL byte",
			body: annotationBody(string([]byte{0x00})),
			// 22021 character_not_in_repertoire, not 22P05: Postgres never gets as
			// far as parsing the JSON, because a UTF8 database will not accept a
			// NUL in a text parameter at all.
			rawSQLSTATE: "22021",
			// A raw NUL inside a JSON string is not JSON at all. Refusing here is
			// the clean rejection with a row to show for it — the outcome the
			// escape denies us and this whole file exists to restore.
			wantErr: true,
		},
		{
			name: "invalid UTF-8",
			body: annotationBody(string([]byte{0xFF})),
			// Unlike the raw NUL this one IS repaired rather than refused: Go's
			// decoder folds an invalid byte to U+FFFD instead of erroring, so the
			// slow path produces a storable document.
			rawSQLSTATE: "22021",
		},

		// ── Unpaired surrogates, the face the first fix left open ─────────────
		{
			name:        "high surrogate alone, upper-case hex",
			body:        annotationBody(highUpper),
			rawSQLSTATE: "22P02",
		},
		{
			name:        "high surrogate alone, lower-case hex",
			body:        annotationBody(highLower),
			rawSQLSTATE: "22P02",
		},
		{
			name:        "low surrogate alone, upper-case hex",
			body:        annotationBody(lowUpper),
			rawSQLSTATE: "22P02",
		},
		{
			name:        "low surrogate alone, lower-case hex",
			body:        annotationBody(lowLower),
			rawSQLSTATE: "22P02",
		},
		{
			name:        "two high surrogates",
			body:        annotationBody(highUpper + highUpper),
			rawSQLSTATE: "22P02",
		},
		{
			name:        "low surrogate before high surrogate, the pair backwards",
			body:        annotationBody(lowUpper + highUpper),
			rawSQLSTATE: "22P02",
		},
		{
			name:        "unpaired surrogate in a key",
			body:        keyBody(highUpper),
			rawSQLSTATE: "22P02",
		},
		{
			name:        "a valid pair followed by a stray low half",
			body:        annotationBody(pairUpper + lowUpper),
			rawSQLSTATE: "22P02",
		},
		{
			name: "high half whose low half is literal text, not an escape",
			// The low half here is preceded by an EVEN run of backslashes, so it
			// denotes six characters of prose and pairs with nothing. The high half
			// is alone, and a scan that matched bytes without counting backslashes
			// would call this a pair and wave it through.
			body:        annotationBody(highUpper + escapedBackslash + `uDC00`),
			rawSQLSTATE: "22P02",
		},
		{
			name:        "the two halves in different strings, adjacent in the byte stream",
			body:        []byte(`{"version":"4","a":"` + highUpper + `","b":"` + lowUpper + `"}`),
			rawSQLSTATE: "22P02",
		},

		// ── Valid pairs: storable, and the fast path must survive them ────────
		{
			name:         "well-formed surrogate pair in a value, upper-case hex",
			body:         annotationBody(pairUpper),
			rawSQLSTATE:  "",
			wantVerbatim: true,
		},
		{
			name:         "well-formed surrogate pair in a value, lower-case hex",
			body:         annotationBody(pairLower),
			rawSQLSTATE:  "",
			wantVerbatim: true,
		},
		{
			name:         "well-formed surrogate pair in a key",
			body:         keyBody(pairUpper),
			rawSQLSTATE:  "",
			wantVerbatim: true,
		},
		{
			name:         "two well-formed pairs back to back",
			body:         annotationBody(pairUpper + pairLower),
			rawSQLSTATE:  "",
			wantVerbatim: true,
		},

		// ── The twin: an escaped backslash is not an escape ───────────────────
		{
			name: "literal text that merely spells the NUL escape",
			// An escaped backslash followed by the plain letters u0000. It denotes
			// no NUL, Postgres takes it happily, and the old substring scan
			// false-positived on it and threw away the verbatim guarantee for
			// nothing.
			body:         annotationBody(escapedBackslash + `u0000`),
			rawSQLSTATE:  "",
			wantVerbatim: true,
		},
		{
			name:         "literal text that merely spells a surrogate escape",
			body:         annotationBody(escapedBackslash + `uD800`),
			rawSQLSTATE:  "",
			wantVerbatim: true,
		},
		{
			name: "a real escape behind an escaped backslash",
			// Three backslashes: the first two are one literal backslash, the third
			// escapes what follows. Parity has to be counted, not guessed.
			body:        annotationBody(escapedBackslash + highUpper),
			rawSQLSTATE: "22P02",
		},
	}
}

// TestPersistedPayloadIsStorableAsJSONB is the defect, as a table.
//
// It asserts the property that actually matters and that nothing else enforces:
// whatever `PersistedPayload` returns must be something Postgres can store,
// because the caller's next move is to write it to a JSONB column inside the
// transaction that would otherwise record the rejection.
//
// The probe is a bare cast rather than an INSERT into `ingest_batches`, because
// the cast IS the operation that fails — an INSERT would need an org, a source
// and a scope to reach the same error, and would prove nothing extra.
//
// Each row is proved twice against the real Postgres: once on the RAW body, to
// show the hazard is real and not a story about one, and once on the output, to
// show the hazard is gone.
func TestPersistedPayloadIsStorableAsJSONB(t *testing.T) {
	t.Parallel()

	h := harness.New(t)

	for _, tc := range storabilityCases() {
		t.Run(tc.name, func(t *testing.T) {
			// 1. What does Postgres make of the body as it arrived? This is the
			//    half that makes the table a proof: a case whose raw form Postgres
			//    would have accepted anyway is not guarding anything.
			assert.Equal(t, tc.rawSQLSTATE, jsonbVerdict(t, h, tc.body),
				"Postgres's verdict on the RAW body changed; the table is now describing "+
					"a hazard that no longer exists, or missing one that now does")

			payload, err := decode.PersistedPayload(tc.body, nil, 0)
			if tc.wantErr {
				require.Error(t, err,
					"input that is not JSON must be refused here, where the rejection can be "+
						"recorded, and not by the INSERT whose job is to record it")
				return
			}
			require.NoError(t, err)

			// 2. Verbatim or not, deliberately. Both directions are contracts: a
			//    clean body must not be re-encoded, and a poisoned one must be.
			if tc.wantVerbatim {
				require.Equal(t, string(tc.body), string(payload),
					"this body is storable as it stands, so re-encoding it would silently drop "+
						"the Grafana and custom-template fields Envelope does not model")
			} else {
				require.NotEqual(t, string(tc.body), string(payload),
					"this body is NOT storable, so returning it verbatim hands Postgres bytes "+
						"it will refuse inside the transaction that would record the refusal")
			}

			// 3. And the thing that actually matters.
			assert.Empty(t, jsonbVerdict(t, h, payload),
				"PersistedPayload returned bytes Postgres cannot store: the accept INSERT "+
					"fails, the 503 makes Alertmanager retry the same body forever, and the "+
					"row that would have recorded the rejection is the row that failed")
		})
	}
}

// jsonbVerdict reports "" when Postgres accepts body as jsonb, and the SQLSTATE
// it refuses the body with otherwise.
func jsonbVerdict(t *testing.T, h *harness.H, body []byte) string {
	t.Helper()

	var round []byte
	err := h.Pool.QueryRow(h.Ctx, `SELECT $1::jsonb`, body).Scan(&round)
	if err == nil {
		assert.NotContains(t, string(round), "\x00",
			"nothing that reaches storage may carry a NUL")
		return ""
	}

	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	// Not a verdict at all — a dead connection, a cancelled context, a driver
	// that would not transmit. Surfaced rather than folded into a sentinel,
	// because a table row that silently means "we never asked Postgres" is
	// exactly the kind of unproved case this file exists to eliminate.
	return "no verdict, the query itself failed: " + err.Error()
}
