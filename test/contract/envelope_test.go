package contract

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/thulasiram/oto/test/contract/schema"
)

// The envelope, asserted once for the whole API.
//
// The conformance audit's first C-finding was `getVersion` answering a bare
// object where every other endpoint answers `{data, meta}`. It got there
// because the envelope was a CONVENTION — written in the SPEC, applied by hand,
// checked by nobody — and a convention applied by hand is one that will be
// forgotten by exactly one endpoint, which is all it takes: the generated
// TypeScript client reads `.data`, so the one endpoint that forgets is the one
// that breaks the page.
//
// These tests do not look at any handler. They assert that the CONTRACT still
// says the same thing about every versioned operation, so a new endpoint
// specified without an envelope is caught while it is still a diff.

// envelopeExempt are the operations that deliberately answer something else,
// each for a reason that is about the consumer rather than about convenience.
var envelopeExempt = map[string]string{
	"getLiveness":             "an unversioned kubelet probe; the consumer is a kubelet, not the generated client",
	"getReadiness":            "an unversioned kubelet probe, same reason, and its body must stay greppable by an operator",
	"getMetrics":              "Prometheus text exposition, not JSON at all",
	"getOpenapiDocument":      "the OpenAPI document itself; wrapping it would make it not be one",
	"streamEvents":            "text/event-stream; the envelope is per-event, on the SSE frame",
	"receiveSlackInteraction": "Slack's own callback contract decides this body, not oto's",
}

// ⭐ TestEverySuccessfulResponseIsAnEnvelope.
func TestEverySuccessfulResponseIsAnEnvelope(t *testing.T) {
	t.Parallel()

	empty, err := jsonschema.UnmarshalJSON(strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("decode the empty object: %v", err)
	}

	var bare, checked []string
	for _, op := range schema.Operations(t) {
		if _, ok := envelopeExempt[op.ID]; ok {
			continue
		}
		for _, status := range op.Statuses {
			if status < 200 || status >= 300 {
				continue
			}
			sch, err := schema.Response(op.ID, status, schema.MediaTypeJSON)
			if errors.Is(err, schema.ErrNoBody) {
				// A 204. There is nothing to wrap.
				continue
			}
			if err != nil {
				t.Errorf("%s %d: %v", op.ID, status, err)
				continue
			}
			checked = append(checked, op.ID)

			// `{}` must be refused, and the refusal must name BOTH members. That
			// is the cheapest way to read "required: [data, meta]" out of a
			// schema without re-implementing $ref resolution here.
			verr := sch.Validate(empty)
			if verr == nil {
				bare = append(bare, op.ID+" ("+op.Method+" "+op.Path+"): accepts {}, so nothing is required")
				continue
			}
			msg := verr.Error()
			if !strings.Contains(msg, "'data'") || !strings.Contains(msg, "'meta'") {
				bare = append(bare, op.ID+" ("+op.Method+" "+op.Path+"): does not require both data and meta")
			}
		}
	}

	if len(checked) == 0 {
		t.Fatal("no successful JSON response was checked; the contract did not load")
	}
	sort.Strings(bare)
	if len(bare) > 0 {
		t.Errorf("%d successful response(s) are not `{data, meta}` envelopes:\n  %s\n\n"+
			"Every versioned response is an envelope (SPEC §E.1). The generated TypeScript client\n"+
			"reads `.data`; an endpoint that answers a bare object is one the client cannot read.\n"+
			"If an operation genuinely must answer something else, add it to envelopeExempt WITH THE REASON.",
			len(bare), strings.Join(bare, "\n  "))
	}
	t.Logf("%d successful JSON responses checked, %d exempt", len(checked), len(envelopeExempt))
}

// TestTheEnvelopeExemptionListNamesRealOperations keeps the exemption list from
// outliving the operations it excuses.
func TestTheEnvelopeExemptionListNamesRealOperations(t *testing.T) {
	t.Parallel()

	for id, why := range envelopeExempt {
		if _, err := schema.Lookup(id); err != nil {
			t.Errorf("envelopeExempt names %q, which the contract does not declare", id)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("envelopeExempt[%q] has no reason", id)
		}
	}
}

// ⛔ TestEveryRefusalIsAProblemDocument.
//
// One error shape, everywhere. A client that has to guess whether a 4xx carries
// a problem document or some endpoint's own error object cannot render a
// consistent failure — and `violations[]` is the only thing a form can turn
// into a field-level message, so an endpoint that invents its own shape makes
// its own form worse.
func TestEveryRefusalIsAProblemDocument(t *testing.T) {
	t.Parallel()

	// The readiness probe's 503 is a PROBE RESULT, not a refusal of a request:
	// it carries the same ReadyDTO the 200 does, because a kubelet and an
	// operator both need to read WHICH dependency failed out of the same shape.
	// Rendering it as a problem document would make "not ready" and "your
	// request was wrong" the same shape, which they are not.
	refusalExempt := map[string]bool{"getReadiness": true}

	var wrong []string
	var checked int
	for _, op := range schema.Operations(t) {
		if refusalExempt[op.ID] {
			continue
		}
		for _, status := range op.Statuses {
			if status < 400 {
				continue
			}
			_, err := schema.Response(op.ID, status, "application/problem+json")
			if err == nil {
				checked++
				continue
			}
			if errors.Is(err, schema.ErrNoBody) {
				wrong = append(wrong, op.ID+" "+itoa(status)+": declares a refusal with no problem+json body")
				continue
			}
			wrong = append(wrong, op.ID+" "+itoa(status)+": "+err.Error())
		}
	}

	if checked == 0 {
		t.Fatal("no refusal was checked; the contract did not load")
	}
	sort.Strings(wrong)
	if len(wrong) > 0 {
		t.Errorf("%d refusal(s) are not RFC 9457 problem documents:\n  %s",
			len(wrong), strings.Join(wrong, "\n  "))
	}
	t.Logf("%d declared refusals, every one a problem+json", checked)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
