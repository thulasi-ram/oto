package repository

import (
	"encoding/json"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/drill/domain"
)

// The frozen outcome's ON-DISK SHAPE, owned here and not by the domain.
//
// ⛔ IT IS NOT `domain.Result` MARSHALLED DIRECTLY, and the difference is not
// pedantry. `delivery_drills.outcome` is a durable format: rows written by
// release N are read by release N+2. A domain type carries no `json` tags
// (CONTEXT.md §5.2), so marshalling it would key the stored bytes off Go FIELD
// NAMES — and a rename that the compiler is perfectly happy with would silently
// stop every historical drill from decoding. Tags belong where the persistence
// contract is, which is here.
type outcomeWire struct {
	Status       string            `json:"status"`
	FailedStage  string            `json:"failed_stage,omitempty"`
	Stages       []stageWire       `json:"stages"`
	Destinations []destinationWire `json:"destinations,omitempty"`
}

type stageWire struct {
	Name   string            `json:"name"`
	Status string            `json:"status"`
	Detail string            `json:"detail,omitempty"`
	Facts  map[string]string `json:"facts,omitempty"`
}

type destinationWire struct {
	ChannelID         uuid.UUID `json:"channel_id"`
	ChannelName       string    `json:"channel_name"`
	Status            string    `json:"status"`
	Mode              string    `json:"mode"`
	ThreadID          string    `json:"thread_id,omitempty"`
	ProviderMessageID string    `json:"provider_message_id,omitempty"`
	Broadcast         bool      `json:"broadcast"`
	Error             string    `json:"error,omitempty"`
	ErrorClass        string    `json:"error_class,omitempty"`
}

func encodeOutcome(res domain.Result) ([]byte, error) {
	w := outcomeWire{
		Status:      res.Status.String(),
		FailedStage: res.FailedStage.String(),
		Stages:      make([]stageWire, 0, len(res.Stages)),
	}
	for _, st := range res.Stages {
		w.Stages = append(w.Stages, stageWire{
			Name:   st.Name.String(),
			Status: st.Status.String(),
			Detail: st.Detail,
			Facts:  st.Facts,
		})
	}
	for _, d := range res.Destinations {
		w.Destinations = append(w.Destinations, destinationWire{
			ChannelID:         d.ChannelID,
			ChannelName:       d.ChannelName,
			Status:            d.Status,
			Mode:              d.Mode,
			ThreadID:          d.ThreadID,
			ProviderMessageID: d.ProviderMessageID,
			Broadcast:         d.Broadcast,
			Error:             d.Error,
			ErrorClass:        d.ErrorClass,
		})
	}
	return json.Marshal(w)
}

func decodeOutcome(b []byte) (domain.Result, error) {
	var w outcomeWire
	if err := json.Unmarshal(b, &w); err != nil {
		return domain.Result{}, err
	}
	// The stored verdict is re-proven rather than trusted. These bytes were
	// written by an older release; a status this one does not recognise is a
	// migration problem worth surfacing, not a string to render at an operator.
	status, err := domain.NewStatus(w.Status)
	if err != nil {
		return domain.Result{}, err
	}
	res := domain.Result{
		Status:      status,
		FailedStage: domain.StageName(w.FailedStage),
		Stages:      make([]domain.Stage, 0, len(w.Stages)),
	}
	for _, st := range w.Stages {
		res.Stages = append(res.Stages, domain.Stage{
			Name:   domain.StageName(st.Name),
			Status: domain.StageStatus(st.Status),
			Detail: st.Detail,
			Facts:  st.Facts,
		})
	}
	for _, d := range w.Destinations {
		res.Destinations = append(res.Destinations, domain.Destination{
			ChannelID:         d.ChannelID,
			ChannelName:       d.ChannelName,
			Status:            d.Status,
			Mode:              d.Mode,
			ThreadID:          d.ThreadID,
			ProviderMessageID: d.ProviderMessageID,
			Broadcast:         d.Broadcast,
			Error:             d.Error,
			ErrorClass:        d.ErrorClass,
		})
	}
	return res, nil
}
