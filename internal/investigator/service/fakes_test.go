package service

// IN-MEMORY PORTS FOR THE INVESTIGATION TESTS. They keep the rules the SQL keeps where a
// test depends on them — a Step can only be appended, an ended run cannot be finished
// twice, a version number cannot be taken twice — so a loop that tried to rewrite its
// transcript fails here as it would against 00096's triggers. The SQL itself is
// `repository/investigations_db_test.go`'s.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
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
		Budgets: d.Budgets, MinInterval: d.MinInterval, InvestigatesIncidents: d.InvestigatesIncidents,
		CreatedAt: at, UpdatedAt: at}
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

func (m *memInvestigators) Update(_ context.Context, s db.TenantScope, id uuid.UUID, enabled bool, b domain.Budgets,
	interval time.Duration, incidents bool, at time.Time,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok || r.OrgID != s.OrgID() {
		return errs.NotFound("investigator_not_found", "no such Investigator")
	}
	r.Enabled, r.Budgets, r.MinInterval, r.InvestigatesIncidents, r.UpdatedAt = enabled, b, interval, incidents, at
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
	// remedies, when wired, is where the Remedy risk questions' tokens are: SpentSince sums
	// them as the repository does (owner ruling 2026-10-05 on git-bug eb4f21b).
	remedies *memRemedies
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
	if m.remedies != nil {
		m.remedies.mu.Lock()
		for _, r := range m.remedies.rows {
			if r.OrgID == s.OrgID() && !r.ProposedAt.Before(since) {
				n += r.Risk.ModelTokens
			}
		}
		m.remedies.mu.Unlock()
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

func (m *memInvestigations) Finish(_ context.Context, s db.TenantScope, id uuid.UUID, end domain.Ending, spent domain.Usage, calls int, finding, class string, at time.Time) error {
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
	if class != "" && finding == "" {
		// `investigations_class_ck`, said in memory: a class belongs to a Finding.
		return errs.Newf(errs.KindInternal, "investigations_class_ck", "classified %q with no Finding", class)
	}
	r.Status, r.Ending, r.Spent, r.ToolCalls, r.Finding, r.EndedAt = end.Status, end, spent, calls, finding, at
	r.Classification = class
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

func (m *memInvestigations) SpentOn(_ context.Context, _ db.TenantScope, id uuid.UUID, answerShaping []string) (domain.Usage, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var (
		u     domain.Usage
		calls int
	)
	for _, st := range m.steps[id] {
		switch {
		case st.Kind == domain.StepModelTurn:
			u = u.Add(st.Usage)
		case slices.Contains(answerShaping, st.Call.Name),
			st.Outcome == domain.OutcomeRefused && strings.HasPrefix(st.Result, "not run:"):
		default:
			calls++
		}
	}
	return u, calls, nil
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
			out = append(out, domain.PriorFinding{InvestigationID: r.ID, SubjectKind: r.SubjectKind, SubjectID: r.SubjectID,
				InvestigatorName: r.InvestigatorName, VersionNumber: r.VersionNumber, Status: r.Status,
				Finding: r.Finding, Classification: r.Classification, EndedAt: r.EndedAt})
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memInvestigations) SubjectFindings(_ context.Context, s db.TenantScope, kind domain.SubjectKind,
	ids []uuid.UUID, except uuid.UUID, limit int,
) ([]domain.PriorFinding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.PriorFinding{}
	for _, r := range m.rows {
		if r.OrgID == s.OrgID() && r.SubjectKind == kind && slices.Contains(ids, r.SubjectID) &&
			r.ID != except && r.Finding != "" {
			out = append(out, domain.PriorFinding{InvestigationID: r.ID, SubjectKind: r.SubjectKind, SubjectID: r.SubjectID,
				InvestigatorName: r.InvestigatorName, VersionNumber: r.VersionNumber, Status: r.Status,
				Finding: r.Finding, Classification: r.Classification, EndedAt: r.EndedAt})
		}
	}
	slices.SortFunc(out, func(a, b domain.PriorFinding) int { return b.EndedAt.Compare(a.EndedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// memIncidents is the org's Incidents and which Incident each Case is in.
type memIncidents struct {
	mu        sync.Mutex
	incidents map[uuid.UUID]domain.IncidentSubject
	holding   map[uuid.UUID]uuid.UUID // case → incident
	// reads counts InvestigationIncident calls, so a test can say the Incident was never
	// read.
	reads int
}

func newMemIncidents() *memIncidents {
	return &memIncidents{incidents: map[uuid.UUID]domain.IncidentSubject{}, holding: map[uuid.UUID]uuid.UUID{}}
}

func (m *memIncidents) InvestigationIncident(_ context.Context, _ db.TenantScope, id uuid.UUID) (domain.IncidentSubject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	i, ok := m.incidents[id]
	if !ok {
		return domain.IncidentSubject{}, errs.NotFound("incident_not_found", "no such incident")
	}
	return i, nil
}

func (m *memIncidents) InvestigationIncidentNumbered(_ context.Context, _ db.TenantScope, n int64) (domain.IncidentSubject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, i := range m.incidents {
		if i.Number == n {
			return i, nil
		}
	}
	return domain.IncidentSubject{}, errs.NotFound("incident_not_found", "no such incident")
}

func (m *memIncidents) HoldingIncident(_ context.Context, _ db.TenantScope, caseID uuid.UUID) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.holding[caseID], nil
}

// memDeclarer records each Incident Finding declared outbound — the one output a run
// has beyond its rows and its Enrichment, and only for an Incident.
type memDeclarer struct {
	mu       sync.Mutex
	declared [][2]uuid.UUID // (incident, investigation)
}

func (m *memDeclarer) DeclareIncidentFinding(_ context.Context, _ db.TenantScope, incidentID, investigationID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.declared = append(m.declared, [2]uuid.UUID{incidentID, investigationID})
	return nil
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
	// err, when set, is what reading them answers: a database that cannot.
	err error
}

func (m *memControls) InvestigationControls(context.Context, db.TenantScope) (domain.OrgControls, error) {
	if m.err != nil {
		return domain.OrgControls{}, m.err
	}
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

// memClasses is the org's Classification set. ⛔ Like the table, it knows nothing about
// the runs: replacing the set cannot reach a Finding.
type memClasses struct {
	mu  sync.Mutex
	set domain.ClassSet
}

func (m *memClasses) ClassSet(context.Context, db.TenantScope) (domain.ClassSet, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.set, nil
}

func (m *memClasses) ReplaceClassSet(_ context.Context, _ db.TenantScope, set domain.ClassSet, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.set = set
	return nil
}

// memSuggestions is the Suggestion table: inserted with a Finding, read while shown,
// applied once. ⛔ Like the table, it has no method that declines one.
type memSuggestions struct {
	mu   sync.Mutex
	rows []domain.Suggestion
}

func (m *memSuggestions) InsertSuggestions(_ context.Context, s db.TenantScope, investigationID uuid.UUID,
	drafts []domain.SuggestionDraft, at, lapsesAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range drafts {
		m.rows = append(m.rows, domain.Suggestion{ID: uuid.New(), OrgID: s.OrgID(), InvestigationID: investigationID,
			Kind: d.Kind, Count: d.Count, Membership: d.Membership, Why: d.Why, ProposedAt: at, LapsesAt: lapsesAt})
	}
	return nil
}

func (m *memSuggestions) ListSuggestions(_ context.Context, s db.TenantScope, investigationID uuid.UUID, now time.Time) ([]domain.Suggestion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Suggestion{}
	for _, r := range m.rows {
		if r.OrgID == s.OrgID() && r.InvestigationID == investigationID && (!r.AppliedAt.IsZero() || now.Before(r.LapsesAt)) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memSuggestions) LockSuggestion(_ context.Context, s db.TenantScope, id uuid.UUID) (domain.Suggestion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.OrgID == s.OrgID() && r.ID == id {
			return r, nil
		}
	}
	return domain.Suggestion{}, errs.NotFound("suggestion_not_found", "no such Suggestion")
}

func (m *memSuggestions) MarkApplied(_ context.Context, s db.TenantScope, id uuid.UUID, by domain.Requester, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, r := range m.rows {
		if r.OrgID == s.OrgID() && r.ID == id {
			if !r.AppliedAt.IsZero() {
				return errs.Conflict("suggestion_already_applied", "applied meanwhile")
			}
			m.rows[i].AppliedAt, m.rows[i].AppliedBy = at, by
			return nil
		}
	}
	return errs.NotFound("suggestion_not_found", "no such Suggestion")
}

// countEdit is one ordinary policy edit a Suggestion made.
type countEdit struct {
	policyID uuid.UUID
	min      int
	window   time.Duration
}

// memPolicies is the org's notification policies as a Suggestion reads them, and a
// record of every count-condition edit — the only write an applied Suggestion makes.
type memPolicies struct {
	mu       sync.Mutex
	policies []domain.PolicyTarget
	edits    []countEdit
}

func (m *memPolicies) SuggestionPolicy(_ context.Context, _ db.TenantScope, id uuid.UUID) (domain.PolicyTarget, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.policies {
		if p.ID == id {
			return p, nil
		}
	}
	return domain.PolicyTarget{}, errs.NotFound("policy_not_found", "no such notification policy")
}

func (m *memPolicies) SuggestionPolicies(context.Context, db.TenantScope) ([]domain.PolicyTarget, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]domain.PolicyTarget(nil), m.policies...), nil
}

func (m *memPolicies) ApplyCountCondition(_ context.Context, _ db.TenantScope, id uuid.UUID, n int, w time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.edits = append(m.edits, countEdit{policyID: id, min: n, window: w})
	for i, p := range m.policies {
		if p.ID == id {
			m.policies[i].CountMin, m.policies[i].CountWindow = n, w
		}
	}
	return nil
}

// memMemberships records every ordinary membership edit an applied Suggestion made.
type memMemberships struct {
	mu    sync.Mutex
	edits []domain.AppliedMembership
}

func (m *memMemberships) ApplySuggestedMembership(_ context.Context, _ db.TenantScope, a domain.AppliedMembership) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.edits = append(m.edits, a)
	return nil
}

// DigestRun is the window's run, or nil.
func (m *memInvestigations) DigestRun(_ context.Context, s db.TenantScope, policyID uuid.UUID, w domain.DigestWindow) (*domain.Investigation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.OrgID == s.OrgID() && r.SubjectKind == domain.SubjectDigest && r.SubjectID == policyID &&
			r.DigestWindow == w {
			out := r
			return &out, nil
		}
	}
	return nil, nil
}

// memDigests is the DigestReader: the summarised policies and each window's Cases.
type memDigests struct {
	mu       sync.Mutex
	policies []domain.SummarisedDigest
	cases    map[uuid.UUID][]domain.DigestCase
	reads    int
}

func (m *memDigests) SummarisedDigests(context.Context, db.TenantScope, time.Time) ([]domain.SummarisedDigest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.policies), nil
}

func (m *memDigests) InvestigationDigest(_ context.Context, _ db.TenantScope, policyID uuid.UUID, w domain.DigestWindow) (domain.DigestSubject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	for _, p := range m.policies {
		if p.PolicyID == policyID {
			return domain.DigestSubject{PolicyID: policyID, PolicyName: p.PolicyName, Window: w,
				Cases: slices.Clone(m.cases[policyID])}, nil
		}
	}
	return domain.DigestSubject{}, errs.NotFound("policy_not_found", "no such policy")
}

// memApprovers is RemedyApprovers over a map of ToolServer -> grants (git-bug 47f67c8).
// Read-only, like the port.
type memApprovers struct {
	mu   sync.Mutex
	rows map[uuid.UUID][]domain.RemedyApprover
}

func (m *memApprovers) RemedyApprovers(_ context.Context, _ db.TenantScope, toolServerID uuid.UUID) ([]domain.RemedyApprover, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]domain.RemedyApprover{}, m.rows[toolServerID]...), nil
}

func (m *memApprovers) RequireRemedyApprover(_ context.Context, _ db.TenantScope, toolServerID, userID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.rows[toolServerID] {
		if a.UserID == userID && a.Counts {
			return nil
		}
	}
	return errs.Forbidden("remedy_approver_required", "no grant")
}
