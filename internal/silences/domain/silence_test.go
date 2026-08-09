package domain

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/platform/errs"
)

var (
	silenceID = uuid.MustParse("018f3a4b-0000-7000-8000-000000000401")
	orgID     = uuid.MustParse("018f3a4b-0000-7000-8000-000000000402")
	sourceID  = uuid.MustParse("018f3a4b-0000-7000-8000-000000000403")

	t0 = time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
)

func requireKind(t *testing.T, err error, kind errs.Kind, code string) {
	t.Helper()
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, kind, e.Kind)
	assert.Equal(t, code, e.Code)
}

func mustMatcher(t *testing.T, name, value string, isRegex, isEqual bool) Matcher {
	t.Helper()
	m, err := NewMatcher(name, value, isRegex, isEqual)
	require.NoError(t, err)
	return m
}

func validParams(t *testing.T) Params {
	t.Helper()
	return Params{
		ID:              silenceID,
		OrgID:           orgID,
		SourceID:        sourceID,
		SourceSilenceID: "9d1e4c0a-1111-2222-3333-444455556666",
		Matchers:        []Matcher{mustMatcher(t, "alertname", "KubePodCrashLooping", false, true)},
		StartsAt:        t0,
		EndsAt:          t0.Add(2 * time.Hour),
		CreatedBy:       "ram@example.com",
		Comment:         "deploying a fix",
		State:           StateActive,
		SourceUpdatedAt: t0,
		MirroredAt:      t0.Add(time.Minute),
	}
}

// ------------------------------------------------------- ⛔ THE READ-ONLY MIRROR

// TestSilenceHasNoWritePath is SPEC R3 / CONTEXT.md §4: oto has NO WRITE PATH
// into your cluster. It cannot create, edit or expire a silence — only show you
// one. The mirror's method set is the enforcement point: every method is a
// value-receiver reader, and there is no mutator and no factory that mints a
// silence oto invented.
func TestSilenceHasNoWritePath(t *testing.T) {
	ty := reflect.TypeOf(Silence{})

	// Every exported method is a value-receiver accessor. A pointer-receiver
	// method would not appear in the value type's method set at all, so this also
	// proves there is no `*Silence` mutator reachable from a value.
	assert.Zero(t, reflect.TypeOf(&Silence{}).NumMethod()-ty.NumMethod(),
		"a pointer-receiver method on a read-only mirror is a write path")

	// The method set is enumerated rather than pattern-matched: adding ANY method
	// to a read-only mirror must be a deliberate act that fails this test first.
	// Every entry below is a reader.
	wantMethods := []string{
		"Annotations", "Comment", "CreatedBy", "Describe", "EndsAt", "ID",
		"Matchers", "Matches", "MirroredAt", "OrgID", "SourceID",
		"SourceSilenceID", "SourceUpdatedAt", "StartsAt", "State",
	}
	var gotMethods []string
	for i := range ty.NumMethod() {
		m := ty.Method(i)
		gotMethods = append(gotMethods, m.Name)
		assert.Equal(t, 1, m.Type.NumOut(),
			"Silence.%s returns more than one value — a reader returns one", m.Name)
	}
	assert.ElementsMatch(t, wantMethods, gotMethods,
		"a new method on the read-only mirror needs a SPEC R3 ruling first")

	// And every field is unexported, so nothing outside this package can assign
	// to a mirrored silence at all.
	sty := reflect.TypeOf(Silence{})
	for i := range sty.NumField() {
		assert.False(t, sty.Field(i).IsExported(),
			"Silence.%s is exported and therefore writable", sty.Field(i).Name)
	}
}

func TestSilenceStateIsMirroredNeverComputedFromTheClock(t *testing.T) {
	// ⛔ oto never computes the state from the clock: disagreeing with the system
	// that actually suppresses alerts would make this page a confident lie.
	p := validParams(t)
	p.StartsAt = t0.Add(-100 * 24 * time.Hour)
	p.EndsAt = t0.Add(-99 * 24 * time.Hour) // long past, by our clock
	p.State = StateActive                   // but upstream still says active

	s, err := New(p)
	require.NoError(t, err)
	assert.Equal(t, StateActive, s.State(),
		"a silence Alertmanager still calls `active` is active here")
	assert.True(t, s.State().IsLive())

	// There is no method that would recompute it.
	_, hasStateAt := reflect.TypeOf(Silence{}).MethodByName("StateAt")
	assert.False(t, hasStateAt)
	_, hasIsActiveAt := reflect.TypeOf(Silence{}).MethodByName("IsActiveAt")
	assert.False(t, hasIsActiveAt)
}

// ----------------------------------------------------------------------- State

func TestNewState(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want State
		ok   bool
	}{
		{in: "active", want: StateActive, ok: true},
		{in: "pending", want: StatePending, ok: true},
		{in: "expired", want: StateExpired, ok: true},
		{in: ""},
		{in: "Active"},
		{in: "resolved"},
		{in: "firing"},
		{in: "deleted"},
		{in: "snoozed"},
	} {
		t.Run("state="+tc.in, func(t *testing.T) {
			got, err := NewState(tc.in)
			if tc.ok {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
				assert.Equal(t, tc.in, got.String())
				assert.False(t, got.IsZero())
				return
			}
			requireKind(t, err, errs.KindValidation, "enum")
			assert.True(t, got.IsZero())
		})
	}
}

func TestState_IsLive(t *testing.T) {
	assert.True(t, StateActive.IsLive(), "suppressing now")
	assert.True(t, StatePending.IsLive(), "about to")
	assert.False(t, StateExpired.IsLive())
	assert.False(t, State{}.IsLive())
}

// --------------------------------------------------------------------- Matcher

func TestNewMatcher(t *testing.T) {
	tests := []struct {
		name    string
		mName   string
		mValue  string
		wantErr string
	}{
		{name: "plain", mName: "alertname", mValue: "X"},
		{name: "underscore", mName: "_a0", mValue: "X"},
		{name: "empty value is legal", mName: "team", mValue: ""},
		{name: "at the value bound", mName: "team", mValue: strings.Repeat("v", MaxMatcherValueBytes)},

		{name: "bad name charset", mName: "bad-name", mValue: "X", wantErr: "labelname"},
		{name: "empty name", mName: "", mValue: "X", wantErr: "labelname"},
		{name: "leading digit", mName: "0bad", mValue: "X", wantErr: "labelname"},
		{name: "unicode name", mName: "日本語", mValue: "X", wantErr: "labelname"},
		{name: "value over the bound", mName: "team", mValue: strings.Repeat("v", MaxMatcherValueBytes+1), wantErr: "max_length"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewMatcher(tc.mName, tc.mValue, false, true)
			if tc.wantErr == "" {
				require.NoError(t, err)
				assert.Equal(t, tc.mName, m.Name)
				assert.Equal(t, tc.mValue, m.Value)
				return
			}
			requireKind(t, err, errs.KindValidation, tc.wantErr)
		})
	}
}

// TestMatcher_Op renders Alertmanager's `(isRegex, isEqual)` encoding as the four
// operators a human recognises.
func TestMatcher_Op(t *testing.T) {
	tests := []struct {
		isRegex, isEqual bool
		want             string
	}{
		{isRegex: false, isEqual: true, want: "="},
		{isRegex: false, isEqual: false, want: "!="},
		{isRegex: true, isEqual: true, want: "=~"},
		{isRegex: true, isEqual: false, want: "!~"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			m := mustMatcher(t, "job", "kubelet", tc.isRegex, tc.isEqual)
			assert.Equal(t, tc.want, m.Op())
		})
	}
}

// TestMatcher_Matches pins the deliberately conservative semantics.
//
// ⚠️ A regex matcher is answered by STRING EQUALITY, not by evaluating the
// pattern: this only ever explains "which alerts does oto BELIEVE this silence
// covers", and Alertmanager remains the authority on what is actually
// suppressed. A wrong answer from a half-implemented regex engine would be worse
// than an under-reported one.
func TestMatcher_Matches(t *testing.T) {
	labels := map[string]string{"job": "kubelet", "team": ""}

	tests := []struct {
		name             string
		mName, mValue    string
		isRegex, isEqual bool
		want             bool
	}{
		{name: "= hit", mName: "job", mValue: "kubelet", isEqual: true, want: true},
		{name: "= miss", mName: "job", mValue: "kubeproxy", isEqual: true},
		{name: "= on an absent label", mName: "pod", mValue: "p1", isEqual: true},
		{name: "= on an empty value", mName: "team", mValue: "", isEqual: true, want: true},

		{name: "!= differs", mName: "job", mValue: "kubeproxy", want: true},
		{name: "!= equals", mName: "job", mValue: "kubelet"},
		{name: "!= on an absent label matches", mName: "pod", mValue: "p1", want: true},

		{name: "=~ is string equality, not a pattern", mName: "job", mValue: "kubelet", isRegex: true, isEqual: true, want: true},
		{name: "=~ does NOT evaluate the pattern", mName: "job", mValue: "kube.*", isRegex: true, isEqual: true},
		{name: "=~ on an absent label", mName: "pod", mValue: "p.*", isRegex: true, isEqual: true},

		{name: "!~ differs", mName: "job", mValue: "kube.*", isRegex: true, want: true},
		{name: "!~ equals", mName: "job", mValue: "kubelet", isRegex: true},
		{name: "!~ on an absent label under-reports rather than guesses", mName: "pod", mValue: "p.*", isRegex: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := mustMatcher(t, tc.mName, tc.mValue, tc.isRegex, tc.isEqual)
			assert.Equal(t, tc.want, m.Matches(labels))
		})
	}
}

// ---------------------------------------------------------------- constructor

func TestNew_RefusesASilenceUpstreamCouldNotHaveProduced(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Params)
		code string
	}{
		{name: "no id", mut: func(p *Params) { p.ID = uuid.Nil }, code: "required"},
		{name: "no org", mut: func(p *Params) { p.OrgID = uuid.Nil }, code: "required"},
		{name: "no source", mut: func(p *Params) { p.SourceID = uuid.Nil }, code: "required"},

		{name: "no upstream silence id", mut: func(p *Params) { p.SourceSilenceID = "" }, code: "max_length"},
		{name: "blank upstream silence id", mut: func(p *Params) { p.SourceSilenceID = "   " }, code: "max_length"},
		{name: "upstream silence id over the bound", mut: func(p *Params) { p.SourceSilenceID = strings.Repeat("s", MaxSourceSilenceIDBytes+1) }, code: "max_length"},

		{name: "no matchers", mut: func(p *Params) { p.Matchers = nil }, code: "min_items"},
		{name: "empty matcher slice", mut: func(p *Params) { p.Matchers = []Matcher{} }, code: "min_items"},

		{name: "no starts_at", mut: func(p *Params) { p.StartsAt = time.Time{} }, code: "required"},
		{name: "no ends_at", mut: func(p *Params) { p.EndsAt = time.Time{} }, code: "required"},
		{name: "ends before starts", mut: func(p *Params) { p.EndsAt = p.StartsAt.Add(-time.Second) }, code: "field_order"},
		{name: "zero-length window", mut: func(p *Params) { p.EndsAt = p.StartsAt }, code: "field_order"},

		{name: "no creator", mut: func(p *Params) { p.CreatedBy = "" }, code: "max_length"},
		{name: "blank creator", mut: func(p *Params) { p.CreatedBy = "  \t" }, code: "max_length"},
		{name: "creator over the bound", mut: func(p *Params) { p.CreatedBy = strings.Repeat("c", MaxCreatedByBytes+1) }, code: "max_length"},
		{name: "comment over the bound", mut: func(p *Params) { p.Comment = strings.Repeat("c", MaxCommentBytes+1) }, code: "max_length"},

		{name: "no state", mut: func(p *Params) { p.State = State{} }, code: "required"},
		{name: "no mirrored_at", mut: func(p *Params) { p.MirroredAt = time.Time{} }, code: "required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validParams(t)
			tc.mut(&p)
			_, err := New(p)
			requireKind(t, err, errs.KindValidation, tc.code)
		})
	}

	t.Run("too many matchers", func(t *testing.T) {
		p := validParams(t)
		for len(p.Matchers) <= MaxMatchers {
			p.Matchers = append(p.Matchers, mustMatcher(t, "job", "kubelet", false, true))
		}
		_, err := New(p)
		requireKind(t, err, errs.KindValidation, "max_items")
	})

	t.Run("exactly at the matcher bound", func(t *testing.T) {
		p := validParams(t)
		p.Matchers = nil
		for range MaxMatchers {
			p.Matchers = append(p.Matchers, mustMatcher(t, "job", "kubelet", false, true))
		}
		_, err := New(p)
		require.NoError(t, err)
	})
}

func TestNew_HappyPath(t *testing.T) {
	p := validParams(t)
	s, err := New(p)
	require.NoError(t, err)

	assert.Equal(t, silenceID, s.ID())
	assert.Equal(t, orgID, s.OrgID())
	assert.Equal(t, sourceID, s.SourceID())
	assert.Equal(t, p.SourceSilenceID, s.SourceSilenceID())
	assert.Equal(t, "ram@example.com", s.CreatedBy())
	assert.Equal(t, "deploying a fix", s.Comment())
	assert.Equal(t, StateActive, s.State())
	assert.Len(t, s.Matchers(), 1)
	assert.NotNil(t, s.Annotations(), "an absent annotation map reads as empty, never nil")

	// Every timestamp is normalised to UTC.
	assert.Equal(t, time.UTC, s.StartsAt().Location())
	assert.Equal(t, time.UTC, s.EndsAt().Location())
	assert.Equal(t, time.UTC, s.SourceUpdatedAt().Location())
	assert.Equal(t, time.UTC, s.MirroredAt().Location())
}

func TestNew_TrimsTheMirroredIdentifiers(t *testing.T) {
	p := validParams(t)
	p.SourceSilenceID = "  sil-1  "
	p.CreatedBy = "  ram  "
	s, err := New(p)
	require.NoError(t, err)
	assert.Equal(t, "sil-1", s.SourceSilenceID())
	assert.Equal(t, "ram", s.CreatedBy())
}

func TestNew_SourceUpdatedAtMayBeUnknown(t *testing.T) {
	p := validParams(t)
	p.SourceUpdatedAt = time.Time{}
	s, err := New(p)
	require.NoError(t, err)
	assert.True(t, s.SourceUpdatedAt().IsZero(), "zero when upstream did not report one")
}

func TestSilence_MatchersAreCopiedOut(t *testing.T) {
	p := validParams(t)
	original := p.Matchers[0]
	s, err := New(p)
	require.NoError(t, err)

	// The caller's slice cannot reach back into the mirror.
	p.Matchers[0] = mustMatcher(t, "job", "tampered", false, true)
	assert.Equal(t, original, s.Matchers()[0])

	// Nor can the slice the accessor hands out.
	out := s.Matchers()
	out[0] = mustMatcher(t, "job", "tampered", false, true)
	assert.Equal(t, original, s.Matchers()[0])
}

// ⛔ BUG. `Silence.Annotations()` returns the INTERNAL map, and `New` stores the
// caller's map by reference rather than cloning it. Either handle lets a caller
// mutate a row of the READ-ONLY MIRROR in place, after every invariant has been
// proved.
//
// It contradicts the type's own doc comment — "Every field is unexported and
// reachable only through the constructor" — and it is inconsistent with
// `Matchers()` twenty lines above, which copies on both sides for exactly this
// reason. `internal/alerts/domain`'s equivalent (`Annotations.Map()`) uses
// `maps.Clone`.
//
// Severity is low today (no current caller mutates it) and the fix is a
// `maps.Clone` in `New` and in the accessor.
func TestSilence_AnnotationsMustNotBeMutable_BUG(t *testing.T) {
	// Regression: Annotations() handed out the internal map and New() aliased the
	// caller.s, so a read-only mirror was editable in place.

	p := validParams(t)
	p.Annotations = map[string]string{"grafana_folder": "prod"}
	s, err := New(p)
	require.NoError(t, err)

	// The caller's map must not reach back in.
	p.Annotations["grafana_folder"] = "tampered"
	assert.Equal(t, "prod", s.Annotations()["grafana_folder"])

	// Nor may the accessor's result.
	out := s.Annotations()
	out["grafana_folder"] = "tampered"
	out["injected"] = "yes"
	assert.Equal(t, "prod", s.Annotations()["grafana_folder"])
	assert.NotContains(t, s.Annotations(), "injected")
}

// -------------------------------------------------------------------- Matches

func TestSilence_MatchesIsTheConjunctionOfEveryMatcher(t *testing.T) {
	p := validParams(t)
	p.Matchers = []Matcher{
		mustMatcher(t, "alertname", "KubePodCrashLooping", false, true),
		mustMatcher(t, "namespace", "prod", false, true),
	}
	s, err := New(p)
	require.NoError(t, err)

	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{
			name:   "both hold",
			labels: map[string]string{"alertname": "KubePodCrashLooping", "namespace": "prod", "pod": "p1"},
			want:   true,
		},
		{name: "one holds", labels: map[string]string{"alertname": "KubePodCrashLooping", "namespace": "staging"}},
		{name: "neither holds", labels: map[string]string{"alertname": "Other"}},
		{name: "no labels at all", labels: map[string]string{}},
		{name: "nil labels", labels: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, s.Matches(tc.labels))
		})
	}
}

// -------------------------------------------------------------------- Describe

func TestSilence_DescribeRendersAlertmanagersOwnSyntax(t *testing.T) {
	p := validParams(t)
	p.Matchers = []Matcher{
		mustMatcher(t, "alertname", "KubePodCrashLooping", false, true),
		mustMatcher(t, "namespace", "prod", false, false),
		mustMatcher(t, "job", "kube.*", true, true),
		mustMatcher(t, "team", "canary", true, false),
	}
	s, err := New(p)
	require.NoError(t, err)

	assert.Equal(t,
		`{alertname="KubePodCrashLooping", namespace!="prod", job=~"kube.*", team!~"canary"}`,
		s.Describe(),
		"this is what the free-text search reads and what a human recognises")
}

func TestSilence_DescribeSingleMatcher(t *testing.T) {
	s, err := New(validParams(t))
	require.NoError(t, err)
	assert.Equal(t, `{alertname="KubePodCrashLooping"}`, s.Describe())
}

// ---------------------------------------------------------------------- Filter

// TestFilterLivesInTheDomainSoTheAPINeedNotImportStorage — CONTEXT.md §5.1
// forbids an api package from reaching into a repository.
func TestFilter_IsPlainCompiledQueryData(t *testing.T) {
	f := Filter{
		States:     []string{StateActive.String(), StatePending.String()},
		SourceID:   sourceID,
		CreatedBy:  "ram@example.com",
		Query:      "deploy",
		FilterHash: "abc",
	}
	assert.Equal(t, []string{"active", "pending"}, f.States)
	assert.Equal(t, sourceID, f.SourceID)
	// Every field round-trips as it was set: a Filter is compiled query data and
	// nothing normalises, trims or hashes it on the way in.
	assert.Equal(t, "ram@example.com", f.CreatedBy)
	assert.Equal(t, "deploy", f.Query)
	assert.Equal(t, "abc", f.FilterHash)

	// The zero filter constrains nothing.
	var zero Filter
	assert.Empty(t, zero.States, "empty means all three states")
	assert.Equal(t, uuid.Nil, zero.SourceID, "the nil UUID means all sources")
	assert.Empty(t, zero.CreatedBy)
	assert.Empty(t, zero.Query)

	// It carries no person reference of oto's own: `created_by` is whoever created
	// the silence in ALERTMANAGER, and is NOT an oto user.
	ty := reflect.TypeOf(Filter{})
	for i := range ty.NumField() {
		name := strings.ToLower(ty.Field(i).Name)
		// vocab:allow the scope-boundary guard must name the words it forbids; this table IS the enforcement.
		for _, banned := range []string{"userid", "assignee", "owner", "responder", "oncall", "rota"} {
			assert.NotContains(t, name, banned)
		}
	}
}

func TestBoundsMirrorTheDDL(t *testing.T) {
	// A bound lives in three places and they must be identical (R9). These are
	// the domain half; TestValidatorMatchesDDL owns the other two.
	assert.Equal(t, 128, MaxSourceSilenceIDBytes)
	assert.Equal(t, 256, MaxCreatedByBytes)
	assert.Equal(t, 4096, MaxCommentBytes)
	assert.Equal(t, 64, MaxMatchers)
	assert.Equal(t, 4096, MaxMatcherValueBytes)
}

func TestUTCOrZero(t *testing.T) {
	assert.True(t, utcOrZero(time.Time{}).IsZero())
	ist := time.FixedZone("IST", 5*3600+1800)
	got := utcOrZero(t0.In(ist))
	assert.Equal(t, time.UTC, got.Location())
	assert.True(t, got.Equal(t0))
}
