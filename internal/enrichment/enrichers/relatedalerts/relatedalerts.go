package relatedalerts

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/enrichment/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
)

// Name is the registry id.
const Name = "alert.related"

// Version is bumped when the payload shape or the relation rules change.
const Version = 1

// Timeout is the per-call ceiling. It is looser than the inline enrichers'
// because this one runs after the notification has already gone out, so the
// only thing its latency costs is how soon the card is amended.
const Timeout = 2 * time.Second

// CacheTTL is short: "what else is firing" is a statement about right now.
const CacheTTL = 30 * time.Second

// Window is how far either side of this case's start another alert must
// have been firing to count as related.
//
// One hour, and it is a WINDOW rather than a causal claim. oto states that
// these alerts overlapped in time and in one label dimension. It does not say
// they share a cause, because it does not know — machine-derived correlation
// with a stated algorithm is the deferred `correlation` module, and inventing
// a weaker version of it here under a friendlier name would be exactly the
// scope creep the boundary document exists to prevent.
const Window = time.Hour

// MaxPerRelation caps how many alerts are reported per relation, and MaxTotal
// caps the payload. A storm produces thousands of related alerts and a card
// listing them is unreadable; a count plus the newest few is not.
const (
	MaxPerRelation = 5
	MaxTotal       = 15
)

// Relation names why two alerts are considered related. The set is CLOSED and
// each member is a plain, checkable statement about labels — never a heuristic.
//
// ⛔ `RelationGroup = "same_group"` WAS THE FIRST AND STRONGEST MEMBER AND IS
// DELETED (git-bug `7570090`). It meant "both were in the same AlertGroup
// generation: routed together by Alertmanager, the strongest signal available and
// the only one oto did not invent" — and every word of that stayed true right up
// to the point where there is no generation to be in. `alert_cases.group_id` is
// the column the relation was computed from and 00069 drops it, so this is not a
// tidy-up: the SQL would have raised a 42703 on every run of this enricher.
//
// ⚠️ THE ENRICHER IS GENUINELY WEAKER NOW AND PRETENDING OTHERWISE WOULD BE THE
// REAL DEFECT. What is left, `same_alertname` and `same_namespace`, are both oto's
// own label inferences; the one relation that carried an EXTERNAL routing decision
// is gone, and nothing in v1 replaces it. Restoring a signal of that strength is
// the deferred `correlation` module's job, not this enricher's — see the ⛔ above
// `Enricher` on why this file refuses to guess at shared causes.
const (
	// RelationAlertName means the same rule fired for different label sets —
	// the same problem in several places.
	RelationAlertName = "same_alertname"
	// RelationNamespace means different rules fired in the same namespace —
	// several problems in one place.
	RelationNamespace = "same_namespace"
)

// Related is one other alert that was firing nearby.
type Related struct {
	Relation  string    `json:"relation"`
	AlertID   string    `json:"alert_id"`
	AlertKey  string    `json:"alert_key"`
	AlertName string    `json:"alertname"`
	Severity  string    `json:"severity,omitempty"`
	Namespace string    `json:"namespace,omitempty"`
	Service   string    `json:"service,omitempty"`
	State     string    `json:"state"`
	CaseID    string    `json:"case_id"`
	StartedAt time.Time `json:"started_at"`
}

// Payload is the enricher's typed output.
type Payload struct {
	WindowSeconds int `json:"window_s"`
	// Counts are the TOTAL number of related alerts per relation, before the
	// per-relation cap. The count is the honest number; the list is a sample.
	Counts map[string]int `json:"counts"`
	// Alerts is the reported sample, deterministically ordered.
	Alerts []Related `json:"alerts"`
	// Truncated reports that more were found than are listed.
	Truncated bool `json:"truncated"`
}

// Query is what the store is asked for.
type Query struct {
	// AlertID identifies the subject, which is excluded from its own results.
	AlertID uuid.UUID
	// ⛔ `CaseID uuid.UUID` WAS HERE AND IS DELETED (git-bug `7570090`). Its doc
	// read: "the same_group relation is resolved FROM CaseID by the store: the group
	// a fire belongs to is a fact the store can join to, and passing it in would
	// mean every caller had to know it first." That was its ONLY job — the
	// self-exclusion is and always was by AlertID — so when the relation went with
	// the group the field had no remaining reader.
	//
	// ⚠️ IT IS DELETED RATHER THAN LEFT AS AN IGNORED FIELD BECAUSE THE STORE CANNOT
	// SIMPLY STOP BINDING IT. It was `$2` in `relatedAlertsSQL`, and a bound
	// parameter that no expression mentions makes Postgres refuse the whole
	// statement with 42P18. Keeping the field would have meant keeping a lie in the
	// argument list; see the ⚠️ above that query.
	//
	// AlertName and Namespace scope the label relations; empty skips them.
	AlertName string
	Namespace string
	// From and To bound the window on alert_cases.started_at.
	From time.Time
	To   time.Time
	// Limit bounds each relation's result set.
	Limit int
}

// Store is the narrow read port this enricher needs.
type Store interface {
	// RelatedAlerts returns the alerts matching each relation, plus the total
	// count per relation before truncation.
	RelatedAlerts(ctx context.Context, s db.TenantScope, q Query) ([]Related, map[string]int, error)
}

// Enricher reports what else was firing nearby.
type Enricher struct {
	store  Store
	clk    clock.Clock
	window time.Duration
}

// Enricher satisfies the port.
var _ domain.Enricher = (*Enricher)(nil)

// New builds the enricher.
func New(store Store, clk clock.Clock) *Enricher {
	if clk == nil {
		clk = clock.New()
	}
	return &Enricher{store: store, clk: clk, window: Window}
}

// WithWindow overrides the correlation window.
func (e *Enricher) WithWindow(d time.Duration) *Enricher {
	if d > 0 {
		e.window = d
	}
	return e
}

// Name is the stable registry id.
func (*Enricher) Name() string { return Name }

// Version is the payload/semantics version.
func (*Enricher) Version() int { return Version }

// Phase is ASYNC, and that is a deliberate ranking rather than an accident of
// cost. This query is a three-way scan over a hot table; the rule definition
// and the alert's own history are both more useful on the first card, so they
// get the pre-notification budget and this one arrives with the amendment.
func (*Enricher) Phase() domain.Phase { return domain.PhaseAsync }

// Timeout is the per-call ceiling.
func (*Enricher) Timeout() time.Duration { return Timeout }

// Applicable requires an alert identity and at least one relation to search on.
func (e *Enricher) Applicable(s *domain.Subject) bool {
	if e.store == nil || s == nil || s.Alert.ID == "" {
		return false
	}
	return s.Alert.AlertName != "" || s.Alert.Namespace != ""
}

// CacheSeed keys on the alert plus the window bucket.
//
// The bucket is the case's start truncated to the window, NOT the current
// time: two cases of the same alert minutes apart genuinely have
// different neighbourhoods, and a seed built from `now` would make the cache
// either useless or wrong depending on which way it rounded.
func (e *Enricher) CacheSeed(s *domain.Subject) string {
	if s == nil || s.Alert.ID == "" {
		return ""
	}
	bucket := s.Case.StartedAt.Truncate(e.window).Unix()
	return s.Alert.ID + "\x00" + strconv.FormatInt(bucket, 10)
}

// Enrich looks up the neighbourhood.
func (e *Enricher) Enrich(ctx context.Context, s *domain.Subject) (domain.Result, error) {
	scope, err := service.ScopeFrom(ctx)
	if err != nil {
		return domain.Result{}, err
	}
	alertID, err := uuid.Parse(s.Alert.ID)
	if err != nil {
		return domain.Result{Status: domain.StatusSkipped, Warnings: []string{"no_alert_id"}}, nil
	}

	anchor := s.Case.StartedAt
	if anchor.IsZero() {
		anchor = e.clk.Now()
	}
	found, counts, err := e.store.RelatedAlerts(ctx, scope, Query{
		AlertID:   alertID,
		AlertName: s.Alert.AlertName,
		Namespace: s.Alert.Namespace,
		From:      anchor.Add(-e.window),
		To:        anchor.Add(e.window),
		Limit:     MaxPerRelation,
	})
	if err != nil {
		return domain.Result{}, err
	}

	// Deterministic order: strongest relation first, then newest, then id. Two
	// runs over the same neighbourhood must render identically or the amended
	// card churns for no reason.
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].Relation != found[j].Relation {
			return relationRank(found[i].Relation) < relationRank(found[j].Relation)
		}
		if !found[i].StartedAt.Equal(found[j].StartedAt) {
			return found[i].StartedAt.After(found[j].StartedAt)
		}
		return found[i].AlertID < found[j].AlertID
	})

	truncated := false
	if len(found) > MaxTotal {
		found = found[:MaxTotal]
		truncated = true
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	if total > len(found) {
		truncated = true
	}
	if counts == nil {
		counts = map[string]int{}
	}

	payload := Payload{
		WindowSeconds: int(e.window / time.Second),
		Counts:        counts,
		Alerts:        found,
		Truncated:     truncated,
	}

	status := domain.StatusOK
	if len(found) == 0 {
		// Nothing nearby is a genuine, useful answer — "this is isolated" — but
		// it is not context worth amending a card for, so it is recorded as
		// skipped and contributes nothing to the coalesced reply.
		status = domain.StatusSkipped
	}

	return domain.Result{
		Status:  status,
		Payload: payload,
		TTL:     CacheTTL,
	}, nil
}

// relationRank orders the relations strongest-first for the rendered sample.
//
// ⛔ RANK 0 WAS `RelationGroup` (git-bug `7570090`). The remaining ranks are NOT
// renumbered down to 0 and 1 on purpose: the numbers are only ever compared to
// each other, never stored or rendered, and leaving the gap keeps the ordering
// claim honest — alertname was always the SECOND-strongest signal and still is.
func relationRank(r string) int {
	switch r {
	case RelationAlertName:
		return 1
	case RelationNamespace:
		return 2
	default:
		return 3
	}
}
