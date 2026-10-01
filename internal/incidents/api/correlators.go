package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	kernel "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/incidents/domain"
	"github.com/thulasiram/oto/internal/incidents/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// The wire shapes and handlers of `/api/v1/correlators*` (ADR 0052 §2, git-bug
// 61eeddf; api/openapi/openapi.yaml, tag Incidents). Gate G1 diffs every struct
// here against its component schema.
//
// ⭐ A CORRELATOR'S BODY IS A NOTIFICATION POLICY'S FIRST HALF ON THE WIRE TOO.
// `matchers` reuses the contract's `MatcherDTO` component — one schema, two Go
// mirrors, because an api package may not import another module's — and
// `count_min` / `count_window_seconds` are spelled, bounded and nullable exactly as
// on `UpdatePolicyRequest`. An operator who can write one can write the other.
//
// ⛔ NOTHING HERE IS CALLED A RULE. In oto that word is the Prometheus alerting
// rule.

// MatcherDTO renders the contract's `MatcherDTO` — the notification policy's
// matcher, in ADR 0017's grammar.
type MatcherDTO struct {
	Name  string `json:"name"  validate:"required,labelname,max=1024"`
	Op    string `json:"op"    validate:"required,matcherop"`
	Value string `json:"value" validate:"max=4096"`
}

// CorrelatorDTO renders `CorrelatorDTO`: one operator-written Correlator.
type CorrelatorDTO struct {
	ID                 uuid.UUID    `json:"id"`
	Name               string       `json:"name"`
	Priority           int          `json:"priority"`
	Enabled            bool         `json:"enabled"`
	Matchers           []MatcherDTO `json:"matchers"`
	CountMin           *int         `json:"count_min"`
	CountWindowSeconds *int         `json:"count_window_seconds"`
	// QuietGraceSeconds is `quiet_grace_s` (migration 00086); null joins only
	// while the Correlator's Incident is active.
	QuietGraceSeconds *int `json:"quiet_grace_seconds"`
	// IncidentsAreConversations is `incidents_are_conversations` (migration
	// 00087, ADR 0052 §6): later facts about this Correlator's member Cases post
	// into the Incident's thread instead of each Case's own.
	IncidentsAreConversations bool      `json:"incidents_are_conversations"`
	CreatedAt                 time.Time `json:"created_at"`
	UpdatedAt                 time.Time `json:"updated_at"`
}

// CreateCorrelatorRequest is the body of `POST /api/v1/correlators`.
//
// The tags carry the RANGES of migration 00085's CHECKs and nothing else; the
// symmetric pair rule is cross-field and lives in `domain.Correlator.Validate`, the
// division the policy's count condition makes.
type CreateCorrelatorRequest struct {
	Name               string       `json:"name"                           validate:"required,notblank,min=1,max=120"`
	Priority           *int32       `json:"priority,omitempty"             validate:"omitempty,min=0,max=10000"`
	Enabled            *bool        `json:"enabled,omitempty"`
	Matchers           []MatcherDTO `json:"matchers,omitempty"             validate:"omitempty,max=32,dive"`
	CountMin           *int32       `json:"count_min,omitempty"            validate:"omitempty,min=2,max=10000"`
	CountWindowSeconds *int32       `json:"count_window_seconds,omitempty" validate:"omitempty,min=60,max=86400"`
	QuietGraceSeconds  *int32       `json:"quiet_grace_seconds,omitempty"  validate:"omitempty,min=60,max=86400"`
	// IncidentsAreConversations defaults to false: one conversation per Case
	// (ADR 0045) until an operator asks for the narrowing by name.
	IncidentsAreConversations *bool `json:"incidents_are_conversations,omitempty"`
}

// UpdateCorrelatorRequest is the body of `PATCH /api/v1/correlators/{id}`.
//
// ⭐ `priority` IS HOW AN OPERATOR REORDERS. The walk is priority-ordered, LOWER
// FIRST, exactly as notification policies are, so moving a Correlator ahead of
// another is a PATCH of one number — the same gesture the policy list has always
// taken.
//
// The count condition is nullable for the policy's reason: an explicit `null`
// TURNS IT OFF, which is a different request from omitting the field. Clearing one
// half alone is refused, because `correlators_count_pair_ck` is symmetric.
type UpdateCorrelatorRequest struct {
	Name               *string       `json:"name,omitempty"     validate:"omitempty,notblank,min=1,max=120"`
	Priority           *int32        `json:"priority,omitempty" validate:"omitempty,min=0,max=10000"`
	Enabled            *bool         `json:"enabled,omitempty"`
	Matchers           *[]MatcherDTO `json:"matchers,omitempty" validate:"omitempty,max=32,dive"`
	CountMin           NullableInt32 `json:"count_min,omitempty"`
	CountWindowSeconds NullableInt32 `json:"count_window_seconds,omitempty"`
	// QuietGraceSeconds is nullable for the count's reason: an explicit `null`
	// CLEARS the grace — join only while active — which is a different request
	// from omitting it. Its range is checked by the domain, as the count's is.
	QuietGraceSeconds NullableInt32 `json:"quiet_grace_seconds,omitempty"`
	// IncidentsAreConversations redirects only facts evaluated after the PATCH
	// commits; nothing already posted moves, in either direction.
	IncidentsAreConversations *bool `json:"incidents_are_conversations,omitempty"`
}

// NullableInt32 is a contract field typed as `integer | null`, where an explicit
// `null` means CLEAR and an omitted field means leave it alone —
// `notification/api.NullableInt32`'s shape, mirrored because an api package may
// not import another module's.
type NullableInt32 struct {
	Set   bool
	Value *int32
}

// UnmarshalJSON records presence as well as value.
func (n *NullableInt32) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(b) == "null" {
		n.Value = nil
		return nil
	}
	var v int32
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n.Value = &v
	return nil
}

// MarshalJSON renders the field back, for symmetry.
func (n NullableInt32) MarshalJSON() ([]byte, error) {
	if !n.Set || n.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*n.Value)
}

// ------------------------------------------------------------------- mapping

func correlatorDTO(c domain.Correlator) CorrelatorDTO {
	ms := make([]MatcherDTO, 0, len(c.Matchers))
	for _, m := range c.Matchers {
		ms = append(ms, MatcherDTO{Name: m.Name, Op: string(m.Op), Value: m.Value})
	}
	out := CorrelatorDTO{
		ID: c.ID, Name: c.Name, Priority: c.Priority, Enabled: c.Enabled, Matchers: ms,
		CreatedAt: c.CreatedAt.UTC(), UpdatedAt: c.UpdatedAt.UTC(),
	}
	out.IncidentsAreConversations = c.Conversations
	if c.Count.Enabled() {
		m, w := c.Count.Min, int(c.Count.Window/time.Second)
		out.CountMin, out.CountWindowSeconds = &m, &w
	}
	if c.QuietGrace > 0 {
		g := int(c.QuietGrace / time.Second)
		out.QuietGraceSeconds = &g
	}
	return out
}

func toMatchers(in []MatcherDTO) []kernel.Matcher {
	out := make([]kernel.Matcher, 0, len(in))
	for _, m := range in {
		out = append(out, kernel.Matcher{Name: m.Name, Op: kernel.MatchOp(m.Op), Value: m.Value})
	}
	return out
}

func countOf(minimum, windowSeconds *int32) domain.Count {
	var c domain.Count
	if minimum != nil {
		c.Min = int(*minimum)
	}
	if windowSeconds != nil {
		c.Window = time.Duration(*windowSeconds) * time.Second
	}
	return c
}

func (r CreateCorrelatorRequest) toDraft() domain.CorrelatorDraft {
	d := domain.CorrelatorDraft{
		Name: r.Name, Enabled: r.Enabled,
		Matchers: toMatchers(r.Matchers),
		Count:    countOf(r.CountMin, r.CountWindowSeconds),
	}
	if r.QuietGraceSeconds != nil {
		d.QuietGrace = time.Duration(*r.QuietGraceSeconds) * time.Second
	}
	if r.IncidentsAreConversations != nil {
		d.Conversations = *r.IncidentsAreConversations
	}
	if r.Priority != nil {
		p := int(*r.Priority)
		d.Priority = &p
	}
	return d
}

// toPatch turns the body into a patch. The count halves are merged against the
// stored condition by the service: a PATCH naming only `count_window_seconds`
// keeps the stored `count_min`, so the patch carries each half's intent and
// `mergeCount` below resolves it against the row.
func (r UpdateCorrelatorRequest) toPatch() (domain.CorrelatorPatch, countPatch) {
	var p domain.CorrelatorPatch
	p.Name = r.Name
	p.Enabled = r.Enabled
	p.Conversations = r.IncidentsAreConversations
	if r.Priority != nil {
		v := int(*r.Priority)
		p.Priority = &v
	}
	if r.Matchers != nil {
		ms := toMatchers(*r.Matchers)
		p.Matchers = &ms
	}
	if r.QuietGraceSeconds.Set {
		var g time.Duration
		if r.QuietGraceSeconds.Value != nil {
			g = time.Duration(*r.QuietGraceSeconds.Value) * time.Second
		}
		p.QuietGrace = &g
	}
	return p, countPatch{min: r.CountMin, window: r.CountWindowSeconds}
}

// countPatch is the two nullable halves of a count condition as the body said
// them.
type countPatch struct {
	min, window NullableInt32
}

func (c countPatch) set() bool { return c.min.Set || c.window.Set }

// merge applies the halves the body named onto the stored condition; a half the
// body did not name keeps its stored value.
func (c countPatch) merge(stored domain.Count) domain.Count {
	out := stored
	if c.min.Set {
		out.Min = 0
		if c.min.Value != nil {
			out.Min = int(*c.min.Value)
		}
	}
	if c.window.Set {
		out.Window = 0
		if c.window.Value != nil {
			out.Window = time.Duration(*c.window.Value) * time.Second
		}
	}
	return out
}

// ------------------------------------------------------------------- the router

// CorrelatorService is the port this layer declares for the Correlator routes,
// satisfied by *service.Correlators.
type CorrelatorService interface {
	List(ctx context.Context, s db.TenantScope) ([]domain.Correlator, error)
	Create(ctx context.Context, s db.TenantScope, d domain.CorrelatorDraft) (domain.Correlator, error)
	Update(ctx context.Context, s db.TenantScope, id uuid.UUID, p domain.CorrelatorPatch) (domain.Correlator, error)
	Delete(ctx context.Context, s db.TenantScope, id uuid.UUID) error
	Get(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Correlator, error)
}

// Compile-time proof that the service satisfies the port this layer declares.
var _ CorrelatorService = (*service.Correlators)(nil)

// CorrelatorRouter serves `/api/v1/correlators`.
//
// ⭐ IT IS ITS OWN ROUTER RATHER THAN MORE ROUTES ON Router, because its service is
// its own type (`service.Correlators`) and a nil one — a deployment that wired
// Incidents and not Correlators — then mounts nothing rather than half a surface.
type CorrelatorRouter struct {
	svc CorrelatorService
	clk clock.Clock
}

// NewCorrelatorRouter builds the Correlator HTTP surface.
func NewCorrelatorRouter(svc CorrelatorService, clk clock.Clock) *CorrelatorRouter {
	if clk == nil {
		clk = clock.New()
	}
	return &CorrelatorRouter{svc: svc, clk: clk}
}

// maxCorrelatorsListed is the page the unpaged list reports. The list is every
// live Correlator in the org — hand-written configuration, a handful per org — and
// it is never cut: `has_more` is always false.
const maxCorrelatorsListed = 200

// Mount registers the Correlator routes onto r, rooted at /api/v1.
//
// The shape is `/notification-policies`': list, create, patch, delete. There is no
// separate reorder route, because order IS `priority` and a PATCH sets it.
func (rt *CorrelatorRouter) Mount(r chi.Router) {
	r.Route("/correlators", func(r chi.Router) {
		r.Get("/", rt.listCorrelators)
		r.Post("/", rt.createCorrelator)
		r.Patch("/{id}", rt.updateCorrelator)
		r.Delete("/{id}", rt.deleteCorrelator)
	})
}

func (rt *CorrelatorRouter) now() time.Time { return rt.clk.Now().UTC() }

// listCorrelators serves GET /api/v1/correlators — every live Correlator, in the
// order the evaluator walks them.
func (rt *CorrelatorRouter) listCorrelators(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	ks, err := rt.svc.List(r.Context(), scope)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	out := make([]CorrelatorDTO, 0, len(ks))
	for _, k := range ks {
		out = append(out, correlatorDTO(k))
	}
	httpx.List(w, r, out, httpx.PageOf(db.Cursor{}, maxCorrelatorsListed), started)
}

// createCorrelator serves POST /api/v1/correlators.
//
// ⚠️ NO IDEMPOTENCY CLAIM: a retried create meets `correlators_name_uniq` and is a
// 409 naming it, which is the policy create's behaviour before it gained a claim.
func (rt *CorrelatorRouter) createCorrelator(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[CreateCorrelatorRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	k, err := rt.svc.Create(r.Context(), scope, dto.toDraft())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusCreated, correlatorDTO(k), started)
}

// updateCorrelator serves PATCH /api/v1/correlators/{id}.
func (rt *CorrelatorRouter) updateCorrelator(w http.ResponseWriter, r *http.Request) {
	started := rt.now()
	scope, id, err := correlatorSubject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	dto, err := httpx.Bind[UpdateCorrelatorRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	patch, count := dto.toPatch()
	if count.set() {
		// The halves are merged against the stored condition here, at the edge,
		// because "omitted" and "null" are wire distinctions the domain patch does
		// not carry. The service re-reads the row under its lock and validates the
		// MERGED Correlator, so a stale read here can only produce a 422, never a
		// row that breaks `correlators_count_pair_ck`.
		stored, err := rt.svc.Get(r.Context(), scope, id)
		if err != nil {
			httpx.WriteProblem(w, r, err)
			return
		}
		merged := count.merge(stored.Count)
		patch.Count = &merged
	}
	if patch.IsEmpty() {
		httpx.WriteProblem(w, r, errs.Validation("validation_failed",
			"supply at least one field to change",
			errs.Violation{Field: "", Code: "min_properties", Message: "at least one property is required"}))
		return
	}
	k, err := rt.svc.Update(r.Context(), scope, id, patch)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, correlatorDTO(k), started)
}

// deleteCorrelator serves DELETE /api/v1/correlators/{id}: the Correlator draws
// nothing from now on, and the Incidents it drew keep naming it.
func (rt *CorrelatorRouter) deleteCorrelator(w http.ResponseWriter, r *http.Request) {
	scope, id, err := correlatorSubject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.svc.Delete(r.Context(), scope, id); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusNoContent, nil)
}

// correlatorSubject resolves the tenant and the `{id}` path parameter, and rejects
// unknown query parameters. A malformed id is the not-found answer, for
// `PathUUID`'s reason.
func correlatorSubject(r *http.Request) (db.TenantScope, uuid.UUID, error) {
	scope, err := scopeOf(r)
	if err != nil {
		return db.TenantScope{}, uuid.Nil, err
	}
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		return db.TenantScope{}, uuid.Nil, domain.CorrelatorNotFound()
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		return db.TenantScope{}, uuid.Nil, err
	}
	return scope, id, nil
}
