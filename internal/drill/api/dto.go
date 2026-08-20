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
	//
	// ⛔ `GroupID *uuid.UUID` TAGGED `json:"group_id"` WAS HERE AND IS DELETED (git-bug
	// `7570090`). It was the deep link to `/groups/<id>`, and there is no such route
	// and no such row; `CaseID` is the link that replaced it, which `2c26520` had
	// already made the product's first destination.
	//
	// ⚠️ THE CONTRACT STILL HAS TO LOSE THE PROPERTY, AND NOT ONLY FOR TIDINESS.
	// `DeliveryDrillDTO` lists `group_id` as an OPTIONAL property with no
	// `additionalProperties: false`, so an ABSENT key is legal at RUNTIME and
	// `drills_contract_test.go` is green — but gate G1 (`test/contract/dto_schema_test.go`)
	// walks the Go struct against the schema and reports a declared property that no
	// Go field builds, which is exactly what this now is. `DrillDestinationDTO` was
	// the mirror image: `broadcast` was `required` under `additionalProperties: false`,
	// so THERE the contract had to move first. Two different failure modes, one edit
	// each, both in `api/openapi/openapi.yaml`.
	//
	// ⛔ `DrillStageName` OWES THE SAME DEBT. `TestContractEnumsMatchTheirDomainEnum`
	// holds the enum and `AllStages()` to the same set in the same order, and `group`
	// has left `AllStages()`, so the enum member has to go with it.
	AlertID        *uuid.UUID `json:"alert_id"`
	CaseID         *uuid.UUID `json:"case_id"`
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
	// ⛔ `Broadcast bool` TAGGED `json:"broadcast"` WAS HERE AND IS DELETED. It
	// reported whether the delivery went out as a channel-visible broadcast reply,
	// "anyway so an operator can see the decision was taken rather than skipped" — and
	// there is no decision left: the thread broadcast mechanism is removed,
	// `BroadcastPolicy.Warrants` had exactly one reachable Reason and the owner ruled
	// it goes, so the flag could only ever have rendered false.
	//
	// ⚠️ THE CONTRACT MOVED FIRST AND THAT IS WHY THE FIELD COULD GO. `broadcast` was a
	// `required` property of `DrillDestinationDTO` under `additionalProperties: false`,
	// so for a while the published schema demanded a byte the domain no longer had a
	// fact for. `api/openapi/openapi.yaml` has since dropped both the property and the
	// `required` member; with `additionalProperties: false` still set, EMITTING it is
	// now the violation, which `drills_contract_test.go` proves either way.
	//
	// ⭐ `mode` IS THE HONEST SUCCESSOR and it was always beside this. It carries the
	// provider's own word for what oto did — `post_root`, `update_root`, `thread_reply`
	// — instead of a derived boolean with one possible value.
	Error      *string `json:"error"`
	ErrorClass *string `json:"error_class"`
}
