package api

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/streaming/domain"
)

// StreamFrameDTO is the `data:` line of one SSE frame. It is the wire contract of
// openapi `StreamFrame` and must not drift from it.
//
// `resource` and `id` are pointers because they are absent for `resync`, which is
// a statement about the stream rather than about any resource. `data` is raw:
// the payload was validated on the way into `ui_events` and re-marshalling it
// here would only risk changing it.
type StreamFrameDTO struct {
	Seq      int64           `json:"seq"`
	Kind     string          `json:"kind"`
	Resource *string         `json:"resource"`
	ID       *uuid.UUID      `json:"id"`
	OrgID    uuid.UUID       `json:"org_id"`
	At       time.Time       `json:"at"`
	Data     json.RawMessage `json:"data"`
}

// toFrameDTO maps a durable event onto its wire frame.
func toFrameDTO(e domain.Event) StreamFrameDTO {
	resource := string(e.Resource)
	id := e.ResourceID
	return StreamFrameDTO{
		Seq:      e.Seq,
		Kind:     string(e.Kind),
		Resource: &resource,
		ID:       &id,
		OrgID:    e.OrgID,
		At:       e.At,
		Data:     e.Payload,
	}
}

// resyncDataDTO is the payload of a `resync` frame (openapi `ResyncData`).
type resyncDataDTO struct {
	Reason string `json:"reason"`
}

// resyncFrame builds a resync frame.
//
// It carries the connection's current watermark as `seq` so the frame is
// well-formed and the client's Last-Event-ID does not move backwards. `resource`
// and `id` stay null: there is no resource this is about.
func resyncFrame(orgID uuid.UUID, seq int64, at time.Time, reason domain.ResyncReason) (StreamFrameDTO, error) {
	data, err := json.Marshal(resyncDataDTO{Reason: string(reason)})
	if err != nil {
		return StreamFrameDTO{}, err
	}
	return StreamFrameDTO{
		Seq:   seq,
		Kind:  string(domain.KindResync),
		OrgID: orgID,
		At:    at,
		Data:  data,
	}, nil
}
