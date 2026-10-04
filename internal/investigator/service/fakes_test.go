package service

// IN-MEMORY PORTS FOR THE INVESTIGATION TESTS. They keep the rules the SQL keeps where a
// test depends on them — a Step can only be appended, an ended run cannot be finished
// twice, a version number cannot be taken twice — so a loop that tried to rewrite its
// transcript fails here as it would against 00092's triggers. The SQL itself is
// `repository/investigations_db_test.go`'s.

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

type memInvestigators struct {
	mu       sync.Mutex
	rows     map[uuid.UUID]domain.Investigator
	versions map[uuid.UUID][]domain.Version // newest last
}

func newMemInvestigators() *memInvestigators {
	return &memInvestigators{rows: map[uuid.UUID]domain.Investigator{}, versions: map[uuid.UUID][]domain.Version{}}
}

func (m *memInvestigators) Create(_ context.Context, s db.TenantScope, d domain.InvestigatorDraft, model domain.ModelIdentity, at time.Time) (domain.Investigator, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.OrgID == s.OrgID() && r.Name == d.Name {
			return domain.Investigator{}, errs.Conflict("investigators_org_name_uniq", "taken")
		}
	}
	inv := domain.Investigator{ID: uuid.New(), OrgID: s.OrgID(), Name: d.Name, Enabled: d.Enabled,
		Budgets: d.Budgets, MinInterval: d.MinInterval, CreatedAt: at, UpdatedAt: at}
	m.rows[inv.ID] = inv
	m.addVersionLocked(inv.ID, 1, d.Spec, model, at)
	return m.getLocked(s, inv.ID)
}

func (m *memInvestigators) addVersionLocked(invID uuid.UUID, n int, spec domain.VersionSpec, model domain.ModelIdentity, at time.Time) domain.Version {
	v := domain.Version{ID: uuid.New(), InvestigatorID: invID, Number: n, ProviderID: spec.ProviderID,
		Model: model, Prompt: spec.Prompt, Tools: spec.Tools, CreatedAt: at}
	m.versions[invID] = append(m.versions[invID], v)
	return v
}

func (m *memInvestigators) getLocked(s db.TenantScope, id uuid.UUID) (domain.Investigator, error) {
	r, ok := m.rows[id]
	if !ok || r.OrgID != s.OrgID() {
		return domain.Investigator{}, errs.NotFound("investigator_not_found", "no such Investigator")
	}
	vs := m.versions[id]
	r.Current = vs[len(vs)-1]
	return r, nil
}

func (m *memInvestigators) Get(_ context.Context, s db.TenantScope, id uuid.UUID) (domain.Investigator, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getLocked(s, id)
}

func (m *memInvestigators) Lock(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.Investigator, error) {
	return m.Get(ctx, s, id)
}

func (m *memInvestigators) List(_ context.Context, s db.TenantScope) ([]domain.Investigator, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Investigator{}
	for id := range m.rows {
		if r, err := m.getLocked(s, id); err == nil {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b domain.Investigator) int {
		if a.Name < b.Name {
			return -1
		}
		return 1
	})
	return out, nil
}

func (m *memInvestigators) Versions(_ context.Context, s db.TenantScope, id uuid.UUID) ([]domain.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.getLocked(s, id); err != nil {
		return nil, err
	}
	out := slices.Clone(m.versions[id])
	slices.Reverse(out)
	return out, nil
}

func (m *memInvestigators) GetVersion(_ context.Context, s db.TenantScope, vid uuid.UUID) (domain.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for invID, vs := range m.versions {
		if m.rows[invID].OrgID != s.OrgID() {
			continue
		}
		for _, v := range vs {
			if v.ID == vid {
				return v, nil
			}
		}
	}
	return domain.Version{}, errs.NotFound("investigator_not_found", "no such version")
}

func (m *memInvestigators) Update(_ context.Context, s db.TenantScope, id uuid.UUID, enabled bool, b domain.Budgets, interval time.Duration, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok || r.OrgID != s.OrgID() {
		return errs.NotFound("investigator_not_found", "no such Investigator")
	}
	r.Enabled, r.Budgets, r.MinInterval, r.UpdatedAt = enabled, b, interval, at
	m.rows[id] = r
	return nil
}

func (m *memInvestigators) AddVersion(_ context.Context, s db.TenantScope, id uuid.UUID, n int, spec domain.VersionSpec, model domain.ModelIdentity, at time.Time) (domain.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[id]; !ok || r.OrgID != s.OrgID() {
		return domain.Version{}, errs.NotFound("investigator_not_found", "no such Investigator")
	}
	for _, v := range m.versions[id] {
		if v.Number == n {
			return domain.Version{}, errs.Conflict("investigator_versions_number_uniq", "taken")
		}
	}
	return m.addVersionLocked(id, n, spec, model, at), nil
}

// memInvestigations keeps runs and transcripts. ⛔ It has no way to change a Step.
type memInvestigations struct {
	mu        sync.Mutex
	rows      map[uuid.UUID]domain.Investigation
	steps     map[uuid.UUID][]domain.Step
	failSteps error
}

func newMemInvestigations() *memInvestigations {
	return &memInvestigations{rows: map[uuid.UUID]domain.Investigation{}, steps: map[uuid.UUID][]domain.Step{}}
}

func (m *memInvestigations) Insert(_ context.Context, s db.TenantScope, inv domain.Investigation) (domain.Investigation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv.ID, inv.OrgID = uuid.New(), s.OrgID()
	m.rows[inv.ID] = inv
	return inv, nil
}

func (m *memInvestigations) Get(_ context.Context, s db.TenantScope, id uuid.UUID) (domain.Investigation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok || r.OrgID != s.OrgID() {
		return domain.Investigation{}, errs.NotFound("investigation_not_found", "no such Investigation")
	}
	return r, nil
}

func (m *memInvestigations) ListBySubject(_ context.Context, s db.TenantScope, kind domain.SubjectKind, subjectID uuid.UUID, _ db.Keyset) ([]domain.Investigation, db.Cursor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Investigation{}
	for _, r := range m.rows {
		if r.OrgID == s.OrgID() && r.SubjectKind == kind && r.SubjectID == subjectID {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b domain.Investigation) int { return b.RequestedAt.Compare(a.RequestedAt) })
	return out, db.Cursor{}, nil
}

// Start counts and starts under one mutex, which is what the advisory lock is to the
// SQL: no second Start can count between this one's count and its write.
func (m *memInvestigations) Start(_ context.Context, s db.TenantScope, id uuid.UUID, at time.Time, maxRunning int) (domain.StartOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.countRunningLocked(s) >= maxRunning {
		return domain.StartAtCapacity, nil
	}
	r, ok := m.rows[id]
	if !ok || r.OrgID != s.OrgID() || r.Status != domain.StatusQueued {
		return domain.StartNotQueued, nil
	}
	r.Status, r.StartedAt = domain.StatusRunning, at
	m.rows[id] = r
	return domain.StartBegan, nil
}

func (m *memInvestigations) countRunningLocked(s db.TenantScope) int {
	n := 0
	for _, r := range m.rows {
		if r.OrgID == s.OrgID() && r.Status == domain.StatusRunning {
			n++
		}
	}
	return n
}

func (m *memInvestigations) CountRunning(_ context.Context, s db.TenantScope) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.countRunningLocked(s), nil
}

func (m *memInvestigations) SpentSince(_ context.Context, s db.TenantScope, since time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, steps := range m.steps {
		if m.rows[id].OrgID != s.OrgID() {
			continue
		}
		for _, st := range steps {
			if st.Kind == domain.StepModelTurn && !st.RecordedAt.Before(since) {
				n += st.Usage.Total()
			}
		}
	}
	return n, nil
}

func (m *memInvestigations) LockSubjectRuns(_ context.Context, s db.TenantScope, investigatorID uuid.UUID,
	kind domain.SubjectKind, subjectID uuid.UUID,
) (domain.SubjectRuns, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out domain.SubjectRuns
	for _, r := range m.rows {
		if r.OrgID != s.OrgID() || r.InvestigatorID != investigatorID || r.SubjectKind != kind || r.SubjectID != subjectID {
			continue
		}
		r := r
		switch {
		case r.Status == domain.StatusQueued:
			if out.Queued == nil || r.RequestedAt.After(out.Queued.RequestedAt) {
				out.Queued = &r
			}
		case r.Status != domain.StatusSkipped:
			if out.Last == nil || anchor(r).After(anchor(*out.Last)) {
				out.Last = &r
			}
		}
	}
	return out, nil
}

func anchor(r domain.Investigation) time.Time {
	if !r.StartedAt.IsZero() {
		return r.StartedAt
	}
	return r.RequestedAt
}

func (m *memInvestigations) Finish(_ context.Context, s db.TenantScope, id uuid.UUID, end domain.Ending, spent domain.Usage, calls int, finding string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok || r.OrgID != s.OrgID() {
		return errs.NotFound("investigation_not_found", "no such Investigation")
	}
	if r.Status.Terminal() {
		// `investigations_frozen`, said in memory.
		return errs.Conflict("investigation_already_ended", "frozen")
	}
	r.Status, r.Ending, r.Spent, r.ToolCalls, r.Finding, r.EndedAt = end.Status, end, spent, calls, finding, at
	if end.Status != domain.StatusSkipped && r.StartedAt.IsZero() {
		r.StartedAt = at
	}
	m.rows[id] = r
	return nil
}

func (m *memInvestigations) AppendStep(_ context.Context, _ db.TenantScope, id uuid.UUID, st domain.Step) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failSteps != nil {
		return m.failSteps
	}
	if r := m.rows[id]; r.Status != domain.StatusRunning {
		return errs.Newf(errs.KindInternal, "step_on_idle_run", "a Step was appended to a %s run", r.Status)
	}
	if n := len(m.steps[id]); st.Seq != n+1 {
		return errs.Newf(errs.KindInternal, "step_seq", "Step %d appended after %d", st.Seq, n)
	}
	m.steps[id] = append(m.steps[id], st)
	return nil
}

func (m *memInvestigations) Steps(_ context.Context, _ db.TenantScope, id uuid.UUID) ([]domain.Step, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.steps[id]), nil
}

func (m *memInvestigations) PriorFindings(_ context.Context, s db.TenantScope, key string, except uuid.UUID, limit int) ([]domain.PriorFinding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.PriorFinding{}
	for _, r := range m.rows {
		if r.OrgID == s.OrgID() && r.AlertKey == key && r.ID != except && r.Finding != "" {
			out = append(out, domain.PriorFinding{InvestigationID: r.ID, SubjectID: r.SubjectID,
				InvestigatorName: r.InvestigatorName, VersionNumber: r.VersionNumber, Status: r.Status,
				Finding: r.Finding, EndedAt: r.EndedAt})
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// memHistory is the Case, its timeline and its rule.
type memHistory struct {
	cases    map[uuid.UUID]domain.CaseSubject
	timeline []domain.TimelineEntry
	rule     domain.RuleAtFire
}

func (h *memHistory) InvestigationCase(_ context.Context, _ db.TenantScope, id uuid.UUID) (domain.CaseSubject, error) {
	c, ok := h.cases[id]
	if !ok {
		return domain.CaseSubject{}, errs.NotFound("case_not_found", "no such case")
	}
	return c, nil
}

func (h *memHistory) CaseTimeline(_ context.Context, _ db.TenantScope, _ uuid.UUID, limit int) ([]domain.TimelineEntry, error) {
	if len(h.timeline) > limit {
		return h.timeline[len(h.timeline)-limit:], nil
	}
	return h.timeline, nil
}

func (h *memHistory) RuleAtFire(context.Context, db.TenantScope, uuid.UUID) (domain.RuleAtFire, error) {
	return h.rule, nil
}

// memFindings records what was published. It is the whole of what a run hands the
// enrichment store — and the test of "never touches the notification path" is that
// this, and the job queue below, are the only outputs a run has.
type memFindings struct {
	mu        sync.Mutex
	published []domain.PublishedFinding
}

func (m *memFindings) PublishFinding(_ context.Context, _ db.TenantScope, f domain.PublishedFinding) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.published = append(m.published, f)
	return nil
}

// memControls is the org's §6 controls as a test sets them.
type memControls struct {
	on          bool
	dailyTokens int64
	concurrency int
}

func (m *memControls) InvestigationControls(context.Context, db.TenantScope) (domain.OrgControls, error) {
	return domain.OrgControls{Enabled: m.on, DailyTokens: m.dailyTokens, Concurrency: m.concurrency}, nil
}

type memQueue struct {
	mu   sync.Mutex
	jobs []db.JobArgs
	// opts are each job's applied options, by index — when it was scheduled for.
	opts []db.JobOptions
}

func (m *memQueue) Enqueue(_ context.Context, args db.JobArgs, opts ...db.JobOption) (db.EnqueueResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs = append(m.jobs, args)
	m.opts = append(m.opts, db.ApplyJobOptions(db.JobOptions{}, opts...))
	return db.EnqueueResult{}, nil
}

// funcTool is a Tool a test writes inline.
type funcTool struct {
	name string
	fn   func(ctx context.Context) (string, error)
}

func (t funcTool) Schema() domain.ToolSchema { return mustSchema(t.name, "a test Tool", "") }

func (t funcTool) Call(ctx context.Context, _ db.TenantScope, _ RunSubject, _ json.RawMessage) (string, error) {
	return t.fn(ctx)
}
