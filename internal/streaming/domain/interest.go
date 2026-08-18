package domain

import (
	"strings"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// The `resources` query values of SPEC §E.4. They are PLURAL and do not equal the
// Resource enum: the API talks about collections a client is displaying, the
// schema talks about the row that changed. Mapping between them here is the only
// place the two vocabularies meet.
const (
	SelectorAlerts     = "alerts"
	SelectorGroups     = "groups"
	SelectorCases      = "cases"
	SelectorEvents     = "events"
	SelectorDeliveries = "deliveries"
	SelectorSources    = "sources"
)

var selectorResource = map[string]Resource{
	SelectorAlerts:     ResourceAlert,
	SelectorGroups:     ResourceGroup,
	SelectorCases:      ResourceCase,
	SelectorEvents:     ResourceAlertEvent,
	SelectorDeliveries: ResourceDelivery,
	SelectorSources:    ResourceSource,
}

// Interest is one subscriber's declared filter.
//
// It can only ever NARROW. Org scoping is applied separately and server-side from
// the authenticated principal, and is not represented here at all — precisely so
// that no combination of query parameters can widen it.
type Interest struct {
	resources map[Resource]struct{}
	alertID   *uuid.UUID
	groupID   *uuid.UUID
}

// ParseInterest builds an Interest from the query parameters of GET /api/v1/stream.
//
// An empty or absent `resources` list means all of them: an EventSource opened
// with no filter is the common case and must not be a silent no-op stream.
func ParseInterest(resources []string, alertID, groupID *uuid.UUID) (Interest, error) {
	in := Interest{alertID: alertID, groupID: groupID}

	var set map[Resource]struct{}
	for _, raw := range resources {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			res, ok := selectorResource[part]
			if !ok {
				return Interest{}, errs.Validation("stream_resource_invalid",
					"unknown stream resource "+part).WithViolations(errs.Violation{
					Field:   "resources",
					Code:    "oneof",
					Message: part + " is not one of alerts, groups, cases, events, deliveries, sources",
				})
			}
			if set == nil {
				set = map[Resource]struct{}{}
			}
			set[res] = struct{}{}
		}
	}
	in.resources = set
	return in, nil
}

// AllResources reports whether this Interest declines to filter by resource.
func (i Interest) AllResources() bool { return len(i.resources) == 0 }

// Matches reports whether e should be delivered to this subscriber.
//
// Narrowing by alert or group is an OR across the two ids rather than an AND:
// a client watching one group wants that group's own frames AND the frames of the
// alerts inside it, and requiring both would deliver almost nothing.
func (i Interest) Matches(e Event) bool {
	if !i.AllResources() {
		if _, ok := i.resources[e.Resource]; !ok {
			return false
		}
	}

	if i.alertID == nil && i.groupID == nil {
		return true
	}
	if i.alertID != nil && e.AlertID != nil && *i.alertID == *e.AlertID {
		return true
	}
	if i.groupID != nil && e.GroupID != nil && *i.groupID == *e.GroupID {
		return true
	}
	return false
}
