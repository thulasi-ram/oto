package api

import (
	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/ingestion/service"
)

// IngestAcceptedDTO is the webhook receipt (openapi `IngestAcceptedDTO`).
//
// ⚠️ ALERTMANAGER IGNORES THIS BODY ON 2xx. There is no back-channel: nothing
// here can tell the upstream anything, and no field may ever become load-bearing
// for it. It exists for humans, for `curl`, and for oto's own contract tests.
//
// It carries no `validate` tags because it is a RESPONSE. The tag rules of §L.2
// bind inbound DTOs; a response is oto's own output and is correct by
// construction.
type IngestAcceptedDTO struct {
	// BatchID is the durable handle. On a duplicate it is the ORIGINAL batch's id,
	// so every replay of the same payload gets the same answer.
	BatchID uuid.UUID `json:"batch_id"`
	// AlertCount is how many alerts were accepted for processing, 0..10000.
	AlertCount int `json:"alert_count"`
	// Duplicate is true when `ingest_dedup` collapsed this onto an earlier batch.
	// TRUE IS A NORMAL OUTCOME, not a warning: an HA Alertmanager pair is
	// at-least-once by design and a network partition guarantees duplicates.
	Duplicate bool `json:"duplicate"`
	// TruncatedAlerts is how many alert bodies were dropped — by the upstream's own
	// `max_alerts` and by oto's B2 cap. Both are permanent losses of the bodies;
	// oto can only report "+N more".
	TruncatedAlerts int `json:"truncated_alerts"`
	// RejectedAlerts is how many elements failed a bound and were recorded as
	// rejections. The rest of the batch was still processed and this is still 202.
	RejectedAlerts int `json:"rejected_alerts"`
}

// acceptedDTO maps the service result onto the wire shape.
func acceptedDTO(r service.AcceptResult) IngestAcceptedDTO {
	return IngestAcceptedDTO{
		BatchID:         r.BatchID,
		AlertCount:      r.AlertCount,
		Duplicate:       r.Duplicate,
		TruncatedAlerts: r.TruncatedAlerts,
		RejectedAlerts:  r.RejectedAlerts,
	}
}
