package api

import (
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/drill/domain"
)

// drillDTO renders one drill and its staged result.
//
// ⭐ IT ALWAYS EMITS EVERY STAGE, including the ones that never started. A result
// screen that showed only what happened would make a chain that stopped at
// `policy` look like a chain with three stages, and "how far did it get" is half
// of what an operator is reading for.
func drillDTO(d domain.Drill, res domain.Result) DrillDTO {
	out := DrillDTO{
		ID:             d.ID,
		SourceID:       d.SourceID,
		Severity:       d.Severity,
		Status:         res.Status.String(),
		FailedStage:    stagePtr(res.FailedStage),
		Stages:         stageDTOs(res.Stages),
		Destinations:   destinationDTOs(res.Destinations),
		AlertID:        idPtr(d.AlertID),
		OccurrenceID:   idPtr(d.OccurrenceID),
		GroupID:        idPtr(d.GroupID),
		NotificationID: idPtr(d.NotificationID),
		BatchID:        idPtr(d.BatchID),
		StartedByLabel: d.StartedByLabel,
		StartedAt:      d.StartedAt.UTC(),
		DeadlineAt:     d.DeadlineAt.UTC(),
		FinishedAt:     timePtr(d.FinishedAt),
		DisposedAt:     timePtr(d.DisposedAt),
	}
	if out.Destinations == nil {
		out.Destinations = []DrillDestinationDTO{}
	}
	return out
}

func stageDTOs(in []domain.Stage) []DrillStageDTO {
	// A drill with no stages at all is not a thing this package can produce, but
	// an empty slice renders as `[]` and a nil one as `null`, and the contract
	// says this is always an array.
	out := make([]DrillStageDTO, 0, len(in))
	for _, st := range in {
		out = append(out, DrillStageDTO{
			Name:   st.Name.String(),
			Status: st.Status.String(),
			Detail: st.Detail,
			Facts:  st.Facts,
		})
	}
	return out
}

func destinationDTOs(in []domain.Destination) []DrillDestinationDTO {
	if len(in) == 0 {
		return nil
	}
	out := make([]DrillDestinationDTO, 0, len(in))
	for _, d := range in {
		out = append(out, DrillDestinationDTO{
			ChannelID:         d.ChannelID,
			ChannelName:       d.ChannelName,
			Status:            d.Status,
			Mode:              d.Mode,
			ThreadID:          strPtr(d.ThreadID),
			ProviderMessageID: strPtr(d.ProviderMessageID),
			Broadcast:         d.Broadcast,
			Error:             strPtr(d.Error),
			ErrorClass:        strPtr(d.ErrorClass),
		})
	}
	return out
}

func stagePtr(s domain.StageName) *string {
	if s == "" {
		return nil
	}
	v := s.String()
	return &v
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func idPtr(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}
