package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// Reason is the closed set of `ingest_rejections.reason` (ingest_rejections_reason_ck).
//
// It is simultaneously three contracts: the DDL CHECK, the `reason` label of
// `oto_ingest_rejected_total`, and the string the per-source rejection feed
// renders. Adding a member requires a migration in the same commit.
//
// Writing one of these rows is what makes it legitimate to answer 202 for a
// partially bad payload. oto never silently drops (§C.9.1).
type Reason string

// The rejection reasons, in the order the DDL lists them.
const (
	// ReasonTooManyLabels is B3: more than MaxLabelsPerAlert labels on one alert.
	ReasonTooManyLabels Reason = "too_many_labels"
	// ReasonLabelValueTooLarge is B5 and B11: a label value, or `alertname`, over cap.
	ReasonLabelValueTooLarge Reason = "label_value_too_large"
	// ReasonLabelNameTooLarge is B4.
	ReasonLabelNameTooLarge Reason = "label_name_too_large"
	// ReasonLabelSetTooLarge is B6: the whole serialised set over cap.
	ReasonLabelSetTooLarge Reason = "labelset_too_large"
	// ReasonTooManyAnnotations is B7. The alert is KEPT; the excess is dropped.
	ReasonTooManyAnnotations Reason = "too_many_annotations"
	// ReasonAnnotationTooLarge is B8. The alert is KEPT; the value is truncated.
	ReasonAnnotationTooLarge Reason = "annotation_too_large"
	// ReasonAnnotationUnstorable is B19: an annotation name or value carrying
	// U+0000 or invalid UTF-8, which Postgres cannot hold in `jsonb`.
	//
	// The alert is KEPT and so is the annotation: the unstorable code points of a
	// VALUE are replaced with U+FFFD. An annotation whose NAME is unstorable is
	// dropped, because a name is a key and rewriting it would silently merge two
	// annotations into one. Annotations are prose, never identity (§C.9.3), so
	// mutating one is honest where mutating a label value would not be — see
	// ReasonInvalidLabelValue for the other half of the same rule.
	ReasonAnnotationUnstorable Reason = "annotation_unstorable"
	// ReasonMissingAlertname is B10.
	ReasonMissingAlertname Reason = "missing_alertname"
	// ReasonInvalidLabelName is B9.
	ReasonInvalidLabelName Reason = "invalid_label_name"
	// ReasonInvalidLabelValue is B18: a label value Postgres cannot store — a
	// U+0000, which `text` cannot hold at all, or a byte sequence that is not valid
	// UTF-8. THAT ALERT is rejected.
	//
	// It exists because the alternative was a lie. The bound is real — such a value
	// fails at layer 6, the INSERT, whatever oto does with it — but with no member
	// of this enum to carry it, ReasonFromError fell through to `undecodable`, and
	// an operator reading the rejection feed went looking for malformed JSON that
	// was never there. The payload decoded perfectly; one label value was
	// unwritable, and oto knows which one.
	//
	// It REJECTS rather than sanitises because a label value is part of alert
	// IDENTITY: replacing a byte would change which Alert this is and file the
	// observation under a key the upstream never sent.
	ReasonInvalidLabelValue Reason = "invalid_label_value"
	// ReasonTimestampOutOfWindow is B12 and B13. B12 drops the alert; B13 clamps
	// and keeps it. Both record this reason, because both are the same fact about
	// the upstream: its clock disagrees with ours by more than we will model.
	ReasonTimestampOutOfWindow Reason = "timestamp_out_of_window"
	// ReasonTooManyAlerts is B2: the batch was truncated to MaxAlertsPerBatch.
	ReasonTooManyAlerts Reason = "too_many_alerts"
	// ReasonBodyTooLarge is B1. The only rejection that answers 413.
	ReasonBodyTooLarge Reason = "body_too_large"
	// ReasonUndecodable is B16 and a body that is not the webhook envelope at all —
	// a custom `payload:` template can emit any shape. The only rejection that
	// answers 400.
	ReasonUndecodable Reason = "undecodable"
	// ReasonUnknownSource is a batch for a source oto cannot serve: soft deleted,
	// or with `push_enabled = false`. Recorded rather than refused, so an operator
	// sees what arrived instead of wondering where it went.
	ReasonUnknownSource Reason = "unknown_source"
)

// String renders the reason.
func (r Reason) String() string { return string(r) }

// reasonSet is the membership test behind ReasonFromCode. A map rather than a
// switch so that the DDL enum and this list are trivially diffable by eye.
var reasonSet = map[Reason]struct{}{
	ReasonTooManyLabels:        {},
	ReasonLabelValueTooLarge:   {},
	ReasonLabelNameTooLarge:    {},
	ReasonLabelSetTooLarge:     {},
	ReasonTooManyAnnotations:   {},
	ReasonAnnotationTooLarge:   {},
	ReasonAnnotationUnstorable: {},
	ReasonMissingAlertname:     {},
	ReasonInvalidLabelName:     {},
	ReasonInvalidLabelValue:    {},
	ReasonTimestampOutOfWindow: {},
	ReasonTooManyAlerts:        {},
	ReasonBodyTooLarge:         {},
	ReasonUndecodable:          {},
	ReasonUnknownSource:        {},
}

// Valid reports whether r is a member of ingest_rejections_reason_ck.
func (r Reason) Valid() bool {
	_, ok := reasonSet[r]
	return ok
}

// ReasonFromError maps a shared-kernel construction failure onto a Reason.
//
// `alerts/domain` deliberately mints its validation codes as the SAME strings
// this enum uses (`too_many_labels`, `invalid_label_name`, …), precisely so that
// layer 2 can persist a rejection without re-deriving why the constructor said
// no. An unrecognised code falls back to ReasonUndecodable, which is the honest
// answer: we could not turn these bytes into an alert and we do not know why.
//
// ⛔ THAT FALLBACK IS A TRAP AND IT HAS SPRUNG ONCE. `undecodable` means "these
// bytes are not a webhook payload", and it sends an operator hunting for
// malformed JSON. When the kernel grew `invalid_label_value` without a member
// here, a perfectly well-formed payload carrying one unwritable label value was
// filed as undecodable — true about the outcome, false about the cause, and
// `ingest_rejections` is the ONLY place a rejected alert survives (§C.9.1).
// Minting a validation code in `alerts/domain` without adding the matching member
// AND its migration is therefore a bug: the fallback is for codes nobody has
// thought about, never for a bound oto deliberately added.
func ReasonFromError(err error) Reason {
	if r := Reason(errs.CodeOf(err)); r.Valid() {
		return r
	}
	return ReasonUndecodable
}

// Rejection is one observation oto refused to normalise, with the offending
// element kept (`ingest_rejections`).
//
// BatchID is nil when no batch row exists — an undecodable body, an oversized
// body, or an unknown source. Everything else carries the batch it came from.
type Rejection struct {
	ID       uuid.UUID
	OrgID    uuid.UUID
	SourceID uuid.UUID
	BatchID  *uuid.UUID
	// ReceivedAt is oto's clock at accept time and the PARTITION KEY. A rejection
	// always shares its batch's received_at, so the two land in the same daily
	// partition and an operator's "what happened at 02:14" query touches one.
	ReceivedAt time.Time
	Reason     Reason
	// Detail is human-readable specifics: which label exceeded which cap. Never a
	// secret — it is written after redaction.
	Detail string
	// Raw is the rejected element itself, POST-REDACTION, so an operator can see
	// exactly what arrived. Never empty: the column is NOT NULL and the whole
	// point of the table is the evidence.
	Raw json.RawMessage
}

// RejectionFilter narrows the per-source rejection feed.
//
// SourceID is REQUIRED and is not a convenience: `ingest_rejections_source_idx`
// is `(org_id, source_id, received_at DESC)`, so a feed without a source is a
// query with no index to ride. The screen is per-source anyway (§C.9.1).
type RejectionFilter struct {
	SourceID uuid.UUID
	// Reasons is an OR over the closed enum. Empty means every reason, which is
	// the default the screen opens with — an operator asking "why did my alert
	// never appear" does not yet know which bound it hit.
	Reasons []Reason
}

// RejectionEntry is one row of that feed, and it is a READ model rather than
// `Rejection` with extra fields.
//
// The difference is `Labels` versus `Raw`. The write side owns the whole
// evidence document; the feed owns the one question an operator is asking —
// WHICH ALERT was refused — and that is the label set, lifted out of `raw`. A
// list that shipped every `raw` document would ship the batch's worth of
// evidence to render a table of reasons.
//
// Labels is EMPTY, never absent, for the rejections that have no alert to name.
// A body oto could not decode, a body over the size cap and an unknown source
// are recorded against the source with no element to point at (§L.3.2), and a
// batch-level rejection like B2's truncation is about the payload rather than
// about any one alert in it. For all of those, `Reason` and `Detail` are the
// whole answer.
type RejectionEntry struct {
	ID         uuid.UUID
	SourceID   uuid.UUID
	BatchID    *uuid.UUID
	ReceivedAt time.Time
	Reason     Reason
	// Detail is the human-readable specifics: which label exceeded which cap.
	Detail string
	// Labels is the rejected alert's label set as it was WRITTEN — that is,
	// already redacted per `alert_sources.redact_labels`. A matched value reads
	// `[redacted]` here because it reads `[redacted]` on disk; this feed does not
	// have the plaintext and must never grow a way to get it.
	Labels map[string]string
}
