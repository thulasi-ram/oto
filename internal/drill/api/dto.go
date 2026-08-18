package api

import (
	"time"

	"github.com/google/uuid"
)

// StartDrillRequest is the body of `POST /api/v1/drills`.
//
// It is deliberately almost empty: the point of a drill is that it takes the
// route a real alert takes, and every knob added here is a way for the drill to
// stop resembling one.
type StartDrillRequest struct {
	// SourceID is the AlertSource whose delivery path is being proved. The drill's
	// payload is accepted AS IF it had arrived on that source's webhook, so it
	// picks up that source's cluster identity, its `inject_labels`, its
	// `ignore_labels` and its redaction — which is most of what makes the answer
	// mean anything.
	SourceID uuid.UUID `json:"source_id" validate:"required"`
	// Severity is the raw label value the synthetic alert fires at. It is the ONE
	// knob, because severity is what a notification policy most often matches on:
	// an operator whose only policy routes `severity=critical` would otherwise get
	// `no_policy` from every drill and conclude oto was broken.
	Severity string `json:"severity" validate:"omitempty,max=64"`
}

// DrillDTO renders one delivery drill and its staged result.
type DrillDTO struct {
	ID       uuid.UUID `json:"id"`
	SourceID uuid.UUID `json:"source_id"`
	Severity string    `json:"severity"`
	// Status is running | passed | failed | timed_out.
	Status string `json:"status"`
	// FailedStage names the FIRST stage that failed, or null. This single field is
	// the operator-facing point of the whole feature: not "it did not work" but
	// "the policy matched nothing".
	FailedStage *string `json:"failed_stage"`

	Stages       []DrillStageDTO       `json:"stages"`
	Destinations []DrillDestinationDTO `json:"destinations"`

	// The artefacts the drill created, for deep links. Null until the stage that
	// produces each one has run.
	AlertID        *uuid.UUID `json:"alert_id"`
	CaseID         *uuid.UUID `json:"case_id"`
	GroupID        *uuid.UUID `json:"group_id"`
	NotificationID *uuid.UUID `json:"notification_id"`
	BatchID        *uuid.UUID `json:"batch_id"`

	StartedByLabel string     `json:"started_by_label"`
	StartedAt      time.Time  `json:"started_at"`
	DeadlineAt     time.Time  `json:"deadline_at"`
	FinishedAt     *time.Time `json:"finished_at"`
	// DisposedAt is when the synthetic signal rows were deleted. A non-null value
	// with a non-null `finished_at` is the normal, healthy end state of a drill:
	// the receipt survives, the fake alert does not.
	DisposedAt *time.Time `json:"disposed_at"`
}

// DrillStageDTO is one link of the chain.
type DrillStageDTO struct {
	Name string `json:"name"`
	// Status is pending | passed | failed | skipped.
	Status string `json:"status"`
	Detail string `json:"detail"`
	// Facts are the small pieces of evidence beside the stage — an alert_key, a
	// Slack ts, a policy name. Keys are stable and snake_case.
	Facts map[string]string `json:"facts,omitempty"`
}

// DrillDestinationDTO is one channel the card reached.
type DrillDestinationDTO struct {
	ChannelID         uuid.UUID `json:"channel_id"`
	ChannelName       string    `json:"channel_name"`
	Status            string    `json:"status"`
	Mode              string    `json:"mode"`
	ThreadID          *string   `json:"thread_id"`
	ProviderMessageID *string   `json:"provider_message_id"`
	// Broadcast records whether this delivery went out as a channel-visible
	// broadcast reply. On a drill it is expected to be false — a first
	// notification posts a root — and it is reported anyway so an operator can see
	// the decision was taken rather than skipped.
	Broadcast  bool    `json:"broadcast"`
	Error      *string `json:"error"`
	ErrorClass *string `json:"error_class"`
}
