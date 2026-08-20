package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// AlertName is the `alertname` every drill carries.
//
// It is deliberately unmistakable in a Slack channel's history: an operator
// scrolling back six months has to be able to tell a rehearsal from an outage at
// a glance, and a card titled `OtoDeliveryDrill` does that without a legend.
const AlertName = "OtoDeliveryDrill"

// DrillLabel is the label carrying the drill's id.
//
// ⛔ IT IS NOT THE SYNTHETIC MARK. The mark is `ingest_batches.mode`, which no
// payload can set; this label is only a NONCE, so every artefact a drill created
// can be found again by `labels @> {"oto_drill": …}` against alerts_labels_gin —
// which keeps working whatever a source's `ignore_labels` does to `alert_key`.
// Anyone can forge this label; forging it buys nothing, because the aggregates
// key off the column and not off this.
const DrillLabel = "oto_drill"

// Receiver is the Alertmanager receiver name a drill claims.
//
// ⛔ IT SEPARATES NOTHING, AND NOTHING NEEDS IT TO. Until ADR 0038 the §C.4 key
// hashed `receiver` and Alertmanager's groupLabels, so this constant plus the
// per-drill nonce in `groupLabels` gave every drill its own generation. ADR 0038
// narrowed the key to `(org, cluster, alertname, namespace-or-∅)` — neither the
// receiver nor the nonce an axis — and git-bug `7570090` deleted the key, the
// generation and `alert_groups` outright. The value survives because a real
// Alertmanager payload carries a receiver name and this one is a faithful forgery.
//
// What still holds, and is the property that mattered: a drill CANNOT post into
// the thread of a real incident, because `AlertName` is oto's own and no real
// rule is called `OtoDeliveryDrill`.
//
// ⭐ AND THE HOLE ADR 0038 OPENED IS NOW CLOSED, WHICH IS WORTH RECORDING BECAUSE THE
// FIX CAME FROM ELSEWHERE. Between ADR 0038 and `7570090` two drills fired inside
// `group_close_delay` shared ONE generation and one thread, so disposing the first
// deleted the second's evidence out from under it, and `dispose.go` carried a
// `NOT EXISTS` guard for exactly that. A conversation now holds exactly one Case and
// a Case belongs to exactly one Alert, so the nonce that always separated the drills'
// ALERTS separates their conversations too — by identity rather than by a grouping
// axis invented for one caller, which is the thing ADR 0038 forbade and the reason
// this was never fixed here.
const Receiver = "oto-delivery-drill"

// FiringFor is how long a drill's alert claims to have been firing when it
// arrives. It is non-zero so the rendered card shows a real duration rather than
// "0s", which is the one value that looks like a bug.
const FiringFor = 12 * time.Minute

// DefaultSeverity is what a drill fires at when the caller does not choose.
//
// `warning` rather than `critical`: a drill should be able to reach a policy
// without being the loudest thing in the channel, and an operator who wants to
// test their critical route can ask for it.
const DefaultSeverity = "warning"

// MaxSeverityBytes bounds the caller-chosen severity, mirroring
// `alerts_sev_ck` and `drills_sev_ck`.
const MaxSeverityBytes = 64

// PayloadInput is everything the synthetic body is built from.
type PayloadInput struct {
	// DrillID is the nonce written into the `oto_drill` label.
	DrillID uuid.UUID
	// ClusterKey is the source's cluster identity, so the payload's `cluster`
	// label agrees with the identity oto will key the Alert on (§C.2).
	ClusterKey string
	// Severity is the raw label value, in the operator's own vocabulary (§L.4.2).
	Severity string
	// Now is oto's clock. Injected, never read, so the bytes are reproducible.
	Now time.Time
}

// BuildPayload renders the Alertmanager v4 webhook body a drill pushes.
//
// ⭐⭐ IT IS A REAL PAYLOAD, NOT A STUB, and every field is populated for a
// reason: a sparse body would sail through bounds a real one has to survive, and
// would render a sparse card that passes outbound validation a real card might
// not. A drill that passed on an easier input than the real thing would be worse
// than no drill at all.
//
// ⛔ NOTHING HERE IS THE SYNTHETIC MARK. The body is indistinguishable, to every
// line of code below the ingest endpoint, from something an Alertmanager could
// have sent — which is exactly why it is evidence. The mark travels beside it, on
// the batch.
//
// It is a pure function of its input: no clock read, no randomness, no I/O, so a
// golden test can pin the exact bytes.
func BuildPayload(in PayloadInput) ([]byte, error) {
	if in.DrillID == uuid.Nil {
		return nil, errs.New(errs.KindValidation, "required", "a drill payload needs its drill id")
	}
	if in.ClusterKey == "" {
		return nil, errs.New(errs.KindValidation, "required", "a drill payload needs a cluster key")
	}
	if in.Now.IsZero() {
		return nil, errs.New(errs.KindValidation, "required", "a drill payload needs oto's clock")
	}
	severity := in.Severity
	if severity == "" {
		severity = DefaultSeverity
	}
	if len(severity) > MaxSeverityBytes {
		return nil, errs.Newf(errs.KindValidation, "max_length",
			"severity must have at most %d characters", MaxSeverityBytes)
	}

	now := in.Now.UTC()
	startsAt := now.Add(-FiringFor)
	nonce := in.DrillID.String()

	labels := map[string]string{
		"alertname":  AlertName,
		"severity":   severity,
		"cluster":    in.ClusterKey,
		"namespace":  "oto",
		"service":    "oto",
		"instance":   "oto-delivery-drill:9100",
		"job":        "oto",
		DrillLabel:   nonce,
		"oto_origin": "delivery-drill",
	}

	// ⛔ oto DOES NOT READ THESE. The envelope carries `groupLabels` because a
	// drill's payload must be byte-shaped like Alertmanager's, and this is what a
	// real webhook would put here — but oto has ignored the envelope's since ADR
	// 0038. They are kept so the drill remains a faithful forgery of an Alertmanager
	// delivery, and they are recorded verbatim in `ingest_batches.payload` like any
	// other body.
	//
	// ⚠️ `groupLabels` IS ALERTMANAGER'S WORD AND NOT OTO'S ANY MORE (git-bug
	// `7570090`). oto has no AlertGroup, so there is nothing here for a group of oto's
	// to be labelled by; what this map is, and all it ever was on the wire, is the
	// upstream's own statement of which labels it collapsed on.
	//
	// The trade-off they used to encode is now settled elsewhere and in oto's
	// favour: a policy matching `namespace` matches a drill exactly when it would
	// match a real alert, because a policy is matched against the ALERT's own label
	// set — no longer against `SplitLabels` of a generation, and never "whatever the
	// operator put in `group_by`". The result screen still names the policy that
	// matched.
	groupLabels := map[string]string{
		"alertname": AlertName,
		"severity":  severity,
		"cluster":   in.ClusterKey,
		"namespace": "oto",
		DrillLabel:  nonce,
	}

	annotations := map[string]string{
		"summary": "Delivery drill from oto. Nothing is wrong.",
		"description": "This alert was manufactured by oto and pushed through the same ingest " +
			"endpoint, the same state machine, the same Case, the same notification policy and " +
			"the same delivery path a real alert takes. Seeing this card means every one of those " +
			"stages works. It is excluded from all alert statistics and is deleted automatically.",
	}

	// ⛔ BUILT AS MAPS, NOT AS A TAGGED STRUCT. `json:"…"` tags are forbidden in
	// `domain` (§L.4.1, CONTEXT.md §5.2) precisely because they are what quietly
	// turns a domain type into a DTO, and the rule holds even when the wire
	// format belongs to a foreign system. `encoding/json` itself is permitted
	// here — it does no I/O — and marshalling a map emits keys in sorted order,
	// so the bytes stay reproducible for a golden test.
	//
	// Reusing `ingestion/decode.Envelope` was the other option and is worse: that
	// type is a LENIENT DECODER for bytes oto did not write, and pointing the
	// producer at it would let a change made for a foreign upstream's benefit
	// silently reshape what oto sends itself.
	env := map[string]any{
		"version": "4",
		"groupKey": `{}/{` + DrillLabel + `="` + nonce + `"}:{alertname="` +
			AlertName + `"}`,
		"truncatedAlerts":     0,
		"status":              "firing",
		"receiver":            Receiver,
		"groupLabels":         groupLabels,
		"commonLabels":        labels,
		"commonAnnotations":   annotations,
		"notification_reason": "first notification",
		"alerts": []any{map[string]any{
			"status":      "firing",
			"labels":      labels,
			"annotations": annotations,
			"startsAt":    startsAt.Format(time.RFC3339Nano),
			// The Go zero time is the LEGAL Alertmanager spelling of "no end time
			// known for this payload" (B10/B13). It is what a real firing alert
			// carries, so it is what a drill carries.
			"endsAt": time.Time{}.Format(time.RFC3339Nano),
			// ⛔ DELIBERATELY EMPTY. `generatorURL` is how oto reaches a rule
			// definition with no API call, and a drill corresponds to no rule in
			// anybody's Prometheus. Inventing a URL would make the rule-snapshot
			// stage report a lookup that never happened.
			"generatorURL": "",
		}},
	}

	body, err := json.Marshal(env)
	if err != nil {
		return nil, errs.Internal("drill_payload_encode_failed", err)
	}
	return body, nil
}
