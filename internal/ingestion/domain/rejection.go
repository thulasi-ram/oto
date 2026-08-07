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
	// ReasonMissingAlertname is B10.
	ReasonMissingAlertname Reason = "missing_alertname"
	// ReasonInvalidLabelName is B9.
	ReasonInvalidLabelName Reason = "invalid_label_name"
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
	ReasonMissingAlertname:     {},
	ReasonInvalidLabelName:     {},
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
