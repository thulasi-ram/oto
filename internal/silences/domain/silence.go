package domain

// THE READ-ONLY MIRROR of an Alertmanager silence.
//
// ⛔ THE PRODUCT RULING (SPEC R3, CONTEXT.md §4): oto has NO WRITE PATH into your
// cluster. It cannot create, edit or expire a silence — only show you one. A
// silence write path is safety-critical, because a bug in one suppresses a real
// incident, and oto has to earn it first. There is deliberately no constructor
// here that mints a silence oto invented; every value object below exists to
// carry what the sync job read from upstream.

import (
	"maps"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/validate"
)

// Bounds mirroring `silences`' CHECK constraints (db/migrations/00012_silences.sql).
const (
	// MaxSourceSilenceIDBytes mirrors silences_srcid_ck.
	MaxSourceSilenceIDBytes = 128
	// MaxCreatedByBytes mirrors the `created_by` bound in openapi.yaml.
	MaxCreatedByBytes = 256
	// MaxCommentBytes mirrors the `comment` bound in openapi.yaml.
	MaxCommentBytes = 4096
	// MaxMatchers mirrors silences_match_ck's upper half and the contract.
	MaxMatchers = 64
	// MaxMatcherValueBytes mirrors the matcher value bound.
	MaxMatcherValueBytes = 4096
)

// State is the silence lifecycle as UPSTREAM reports it.
//
// ⛔ oto never computes it from the clock. The mirror mirrors: a silence
// Alertmanager still calls `active` is active here, even if our clock disagrees
// about its end time, because disagreeing with the system that actually
// suppresses alerts would make this page a confident lie.
type State struct{ s string }

// The three silence states (openapi.yaml `SilenceState`, silences_state_ck).
var (
	// StateActive means upstream is suppressing on this silence now.
	StateActive = State{"active"}
	// StatePending means it has not started yet.
	StatePending = State{"pending"}
	// StateExpired means it has lapsed.
	StateExpired = State{"expired"}
)

// NewState parses a mirrored state.
func NewState(s string) (State, error) {
	switch s {
	case StateActive.s, StatePending.s, StateExpired.s:
		return State{s}, nil
	default:
		return State{}, errs.Newf(errs.KindValidation, "enum",
			"silence state must be one of: active, pending, expired (got %q)", s)
	}
}

// String renders the state.
func (s State) String() string { return s.s }

// IsZero reports whether the state is unset.
func (s State) IsZero() bool { return s.s == "" }

// IsLive reports whether the silence is suppressing or about to.
func (s State) IsLive() bool { return s == StateActive || s == StatePending }

// Matcher is one Alertmanager silence matcher, mirrored verbatim.
//
// The four operators are encoded upstream as `(isRegex, isEqual)`: `=` is
// (false,true), `!=` is (false,false), `=~` is (true,true), `!~` is (true,false).
// `Op` is the same thing rendered for a human.
type Matcher struct {
	Name    string
	Value   string
	IsRegex bool
	IsEqual bool
}

// NewMatcher builds a mirrored matcher, refusing one oto could not have read.
func NewMatcher(name, value string, isRegex, isEqual bool) (Matcher, error) {
	if !validate.LabelNameRe.MatchString(name) {
		return Matcher{}, errs.New(errs.KindValidation, "labelname",
			"matcher name must be a valid Prometheus label name")
	}
	if len(value) > MaxMatcherValueBytes {
		return Matcher{}, errs.Newf(errs.KindValidation, "max_length",
			"matcher value must have at most %d characters", MaxMatcherValueBytes)
	}
	return Matcher{Name: name, Value: value, IsRegex: isRegex, IsEqual: isEqual}, nil
}

// Op renders the matcher as one of the four operators.
func (m Matcher) Op() string {
	switch {
	case m.IsRegex && m.IsEqual:
		return "=~"
	case m.IsRegex:
		return "!~"
	case m.IsEqual:
		return "="
	default:
		return "!="
	}
}

// Matches reports whether one label set satisfies this matcher.
//
// ⚠️ A regex matcher is answered by string equality here, NOT by evaluating the
// pattern: this is used only to explain "which alerts does oto believe this
// silence covers", and a wrong answer from a half-implemented regex engine would
// be worse than an under-reported one. Alertmanager remains the authority on what
// is actually suppressed.
func (m Matcher) Matches(labels map[string]string) bool {
	v, present := labels[m.Name]
	if m.IsRegex {
		return present && m.IsEqual == (v == m.Value)
	}
	if m.IsEqual {
		return present && v == m.Value
	}
	return !present || v != m.Value
}

// Silence is a read-only mirror of one Alertmanager silence.
//
// Every field is unexported and reachable only through the constructor, so a
// mirror row that upstream could not have produced — no matchers, an end before
// its start, a blank creator — cannot be built at all.
type Silence struct {
	id              uuid.UUID
	orgID           uuid.UUID
	sourceID        uuid.UUID
	sourceSilenceID string
	matchers        []Matcher
	startsAt        time.Time
	endsAt          time.Time
	createdBy       string
	comment         string
	annotations     map[string]string
	state           State
	sourceUpdatedAt time.Time
	mirroredAt      time.Time
}

// Params is the constructor input, and also the rehydration shape: the
// repository maps a row into it and the constructor re-proves every invariant.
type Params struct {
	ID              uuid.UUID
	OrgID           uuid.UUID
	SourceID        uuid.UUID
	SourceSilenceID string
	Matchers        []Matcher
	StartsAt        time.Time
	EndsAt          time.Time
	CreatedBy       string
	Comment         string
	Annotations     map[string]string
	State           State
	SourceUpdatedAt time.Time
	MirroredAt      time.Time
}

// New builds a mirrored silence, enforcing every §D.9 invariant.
func New(p Params) (Silence, error) {
	switch {
	case p.ID == uuid.Nil:
		return Silence{}, errs.New(errs.KindValidation, "required", "silence id is required")
	case p.OrgID == uuid.Nil:
		return Silence{}, errs.New(errs.KindValidation, "required", "org_id is required")
	case p.SourceID == uuid.Nil:
		return Silence{}, errs.New(errs.KindValidation, "required", "source_id is required")
	}

	srcID := strings.TrimSpace(p.SourceSilenceID)
	if srcID == "" || len(srcID) > MaxSourceSilenceIDBytes {
		return Silence{}, errs.Newf(errs.KindValidation, "max_length",
			"source_silence_id must be 1..%d characters", MaxSourceSilenceIDBytes)
	}
	if len(p.Matchers) == 0 {
		return Silence{}, errs.New(errs.KindValidation, "min_items",
			"a silence must carry at least one matcher")
	}
	if len(p.Matchers) > MaxMatchers {
		return Silence{}, errs.Newf(errs.KindValidation, "max_items",
			"a silence must carry at most %d matchers", MaxMatchers)
	}
	if p.StartsAt.IsZero() || p.EndsAt.IsZero() {
		return Silence{}, errs.New(errs.KindValidation, "required",
			"starts_at and ends_at are required")
	}
	if !p.EndsAt.After(p.StartsAt) {
		return Silence{}, errs.New(errs.KindValidation, "field_order",
			"ends_at must be after starts_at")
	}
	createdBy := strings.TrimSpace(p.CreatedBy)
	if createdBy == "" || len(createdBy) > MaxCreatedByBytes {
		return Silence{}, errs.Newf(errs.KindValidation, "max_length",
			"created_by must be 1..%d characters", MaxCreatedByBytes)
	}
	if len(p.Comment) > MaxCommentBytes {
		return Silence{}, errs.Newf(errs.KindValidation, "max_length",
			"comment must have at most %d characters", MaxCommentBytes)
	}
	if p.State.IsZero() {
		return Silence{}, errs.New(errs.KindValidation, "required", "silence state is required")
	}
	if p.MirroredAt.IsZero() {
		return Silence{}, errs.New(errs.KindValidation, "required", "mirrored_at is required")
	}

	// Cloned on ingress as well as egress: storing the caller's map by reference
	// leaves the constructed Silence aliased to something the caller still holds.
	annotations := maps.Clone(p.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}

	return Silence{
		id:              p.ID,
		orgID:           p.OrgID,
		sourceID:        p.SourceID,
		sourceSilenceID: srcID,
		matchers:        append([]Matcher(nil), p.Matchers...),
		startsAt:        p.StartsAt.UTC(),
		endsAt:          p.EndsAt.UTC(),
		createdBy:       createdBy,
		comment:         p.Comment,
		annotations:     annotations,
		state:           p.State,
		sourceUpdatedAt: utcOrZero(p.SourceUpdatedAt),
		mirroredAt:      p.MirroredAt.UTC(),
	}, nil
}

// ID is oto's own row id.
func (s Silence) ID() uuid.UUID { return s.id }

// OrgID is the tenant.
func (s Silence) OrgID() uuid.UUID { return s.orgID }

// SourceID names the AlertSource this silence was mirrored from.
func (s Silence) SourceID() uuid.UUID { return s.sourceID }

// SourceSilenceID is Alertmanager's own silence id, for cross-referencing and
// deep links.
func (s Silence) SourceSilenceID() string { return s.sourceSilenceID }

// Matchers are the mirrored matchers, at least one.
func (s Silence) Matchers() []Matcher { return append([]Matcher(nil), s.matchers...) }

// StartsAt is upstream's start.
func (s Silence) StartsAt() time.Time { return s.startsAt }

// EndsAt is upstream's end, strictly after StartsAt.
func (s Silence) EndsAt() time.Time { return s.endsAt }

// CreatedBy is whoever created the silence in Alertmanager. It is mirrored
// verbatim and is NOT an oto user.
func (s Silence) CreatedBy() string { return s.createdBy }

// Comment is the silence justification, shown wherever oto explains why
// something is quiet.
func (s Silence) Comment() string { return s.comment }

// Annotations are the Alertmanager >= 0.32.0 silence annotations.
//
// The clone is what makes "reachable only through the constructor" true: handing
// back the internal map let a caller edit a mirrored silence in place, and a
// mirror that can be edited locally is no longer a mirror. Matchers() clones on
// both sides for the same reason.
func (s Silence) Annotations() map[string]string { return maps.Clone(s.annotations) }

// State is the mirrored lifecycle state.
func (s Silence) State() State { return s.state }

// SourceUpdatedAt is upstream's updatedAt, used to detect a changed silence
// between syncs. Zero when upstream did not report one.
func (s Silence) SourceUpdatedAt() time.Time { return s.sourceUpdatedAt }

// MirroredAt is oto's clock at the last successful sync of this row — the
// staleness indicator the UI shows.
func (s Silence) MirroredAt() time.Time { return s.mirroredAt }

// Matches reports whether every matcher holds for one label set.
func (s Silence) Matches(labels map[string]string) bool {
	for _, m := range s.matchers {
		if !m.Matches(labels) {
			return false
		}
	}
	return true
}

// Describe renders the matchers in Alertmanager's own syntax, which is what the
// free-text search reads and what a human recognises.
func (s Silence) Describe() string {
	parts := make([]string, 0, len(s.matchers))
	for _, m := range s.matchers {
		parts = append(parts, m.Name+m.Op()+`"`+m.Value+`"`)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func utcOrZero(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}
