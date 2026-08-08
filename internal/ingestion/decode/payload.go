package decode

import "time"

// Envelope is the Alertmanager v4 webhook body as ACTUALLY EMITTED ON THE WIRE,
// which is not the same thing as the documented body.
//
// Decoding policy (§L.3.1), and every clause of it is load-bearing:
//
//   - LENIENT. No DisallowUnknownFields, ever. Alertmanager emits `routeLabels`,
//     which is absent from the published documentation. Grafana Unified Alerting
//     emits a superset (`orgId`, `title`, `message`, `state`, `silenceURL`,
//     `dashboardURL`, `panelURL`, `imageURL`). A custom `payload:` template can
//     emit any shape at all. Rejecting an unknown field would break on the next
//     Alertmanager release AND DELETE ALERTS, because the only way to signal a
//     rejection is a 4xx and a 4xx is permanent loss.
//   - `version` is the hardcoded literal "4". It is NOT feature detection: do not
//     branch on it, do not reject another value.
//   - `truncatedAlerts` is a NUMBER, despite the upstream docs rendering it in a
//     string-looking position.
//
// Unknown fields are simply dropped from this struct; the RAW body is what is
// persisted, so nothing is actually lost by not modelling them.
//
// ⛔ `externalURL` IS NOT MODELLED, and its absence is a decision. It is the
// Alertmanager UI root, it arrives free on every notification, and it was
// decoded here and read by nothing — but the trustworthy spelling of that same
// fact is `alert_sources.base_url`, which an operator configured, which the SSRF
// guard vetted, and which is already what builds the Silence link on a Slack
// card. Deep-linking a human from oto's UI to a URL supplied by the body of an
// inbound webhook is a redirect an upstream gets to choose. It stays in the
// persisted raw payload, where it is evidence rather than a link.
type Envelope struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts"`
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	// RouteLabels is on the wire and absent from the docs. Its existence is
	// precisely why this decoder is lenient, so it is modelled explicitly — as
	// evidence, not because oto reads it.
	RouteLabels map[string]string `json:"routeLabels"`
	// NotificationReason is Alertmanager >= 0.32.0 (C5). One of `none`,
	// `first notification`, `new alerts added`, `some alerts resolved`,
	// `all alerts resolved`, `repeat interval elapsed`, `unknown`.
	//
	// It is why oto is quieter than stock Alertmanager: `repeat interval elapsed`
	// means nothing changed, so the existing message is UPDATED rather than a new
	// one posted. Empty on older Alertmanager, where the fallback is diffing
	// fingerprint sets — never guessing.
	NotificationReason string `json:"notification_reason"`

	Alerts []Alert `json:"alerts"`
}

// Alert is one element of the envelope's `alerts[]`.
//
// `status` is only ever `firing` or `resolved` here. `suppressed` is NOT
// observable on a webhook: Alertmanager's MuteStage drops muted alerts before the
// webhook is called, so absence means nothing and MUST NOT be read as suppression
// (C1). Only the reconciler can witness that.
type Alert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	// EndsAt may be the Go ZERO TIME "0001-01-01T00:00:00Z" — not null, not
	// omitted. Zero means "no end time known for this payload"; it never means
	// year 1, and it never means "forget the end time you already had".
	EndsAt       time.Time `json:"endsAt"`
	GeneratorURL string    `json:"generatorURL"`
	// Fingerprint is Alertmanager's FNV-1a 64, present since 0.19.0. oto
	// RECOMPUTES it and stores its own (C10); this field is kept only so a
	// mismatch can be measured.
	Fingerprint string `json:"fingerprint"`
	// Value is Prometheus's sample value where the upstream chose to send one. It
	// is a pointer because zero and absent are different facts.
	Value *float64 `json:"value"`
}

// The per-alert wire statuses. A webhook never carries anything else.
const (
	// StatusFiring is an active alert.
	StatusFiring = "firing"
	// StatusResolved is an explicit upstream resolution. It is the ONLY thing that
	// may produce `resolved` in oto — absence produces `expired` (C2).
	StatusResolved = "resolved"
)

// NormaliseStatus maps a wire status onto the two values oto will act on.
//
// Anything unrecognised becomes `firing`. That asymmetry is deliberate: treating
// an unknown status as resolved would fabricate a resolution, and oto never
// claims resolved when it means anything else (C2). Over-reporting a firing alert
// is recoverable by the reconciler; a fabricated resolution is not.
func NormaliseStatus(s string) string {
	if s == StatusResolved {
		return StatusResolved
	}
	return StatusFiring
}
