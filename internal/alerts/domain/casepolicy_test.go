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

// These are the tests for one row of `case_policy_config` (migration 00057): its
// AXES — (namespace, alertname), ADR 0038's own — and the bounds on W.
//
// ⛔ EVERY NUMBER ASSERTED HERE IS A DDL CHECK. `case_policy_window_ck` is
// `BETWEEN 0 AND 86400`, `case_policy_name_ck` is `length(alertname) BETWEEN 1 AND
// 1024`, `case_policy_ns_ck` is `length(namespace) <= 1024`. A validator looser
// than its CHECK turns a 422 into a 500; one tighter rejects rows the database
// would have taken. These tests are the only place the two readings are compared.

// -------------------------------------------------- the absent-namespace partition

// TestTheAbsentNamespaceIsTheEmptyStringPartition is the sentinel migration 00057
// spells out and oto spells nowhere else.
//
// ⭐⭐ WHY IT IS THE EMPTY STRING AND NOT NULL. `case_policy_axes_uniq` is a UNIQUE index and
// two NULLs are not equal under one, so a NULL namespace would let a single org
// hold two contradictory windows for the same alertname. ADR 0038 already folds
// EMPTY onto ABSENT — `alerts.namespace` is NULL for both because Prometheus
// treats them as equivalent — so the sentinel loses nothing: they were already one
// partition and this simply gives that partition a value an index can compare.
//
// ⭐ AND IT IS ONE FUNCTION. `repository.NormaliseNamespace` delegates here rather
// than trimming again, because two independent trims over one UNIQUE index is the
// shape where the partition a settings write lands in and the partition the ingest
// lookup probes drift apart WITHOUT ANYTHING FAILING — the window would simply
// never apply.
func TestTheAbsentNamespaceIsTheEmptyStringPartition(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "absent", in: "", want: ""},
		{name: "spaces only", in: "   ", want: ""},
		{name: "tabs and newlines only", in: "\t\n ", want: ""},
		{name: "a namespace", in: "production", want: "production"},
		{name: "a padded namespace", in: "  production\t", want: "production"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, NormaliseNamespace(tc.in))
		})
	}
}

// TestARowForTheAbsentNamespaceIsLegalAndNormalisedOnTheWayIn — the absent
// namespace is a PARTITION and not a missing value, so a draft that names it is
// valid, and `Normalised` is what makes the write land in the partition the ingest
// lookup will probe.
func TestARowForTheAbsentNamespaceIsLegalAndNormalisedOnTheWayIn(t *testing.T) {
	d := CasePolicyDraft{
		Namespace:       "   ",
		Alertname:       "  HighErrorRate  ",
		RetentionWindow: 10 * time.Minute,
	}
	require.NoError(t, d.Validate(), "the '' partition is a legal row, not a missing axis")

	n := d.Normalised()
	assert.Equal(t, "", n.Namespace, "whitespace folds onto the absent-namespace partition")
	assert.Equal(t, "HighErrorRate", n.Alertname, "the axis is stored trimmed")
	assert.Equal(t, 10*time.Minute, n.RetentionWindow)
	require.NoError(t, n.Validate())
}

// --------------------------------------------------------------- the axes exist

// TestACasePolicyRowAlwaysNamesAnAlertname is `case_policy_name_ck`, and it is why
// there is no org-wide default row: a row with no alertname WOULD be one, and this
// table deliberately does not offer one — a default lives in code, at 0, where it
// cannot be half-configured.
func TestACasePolicyRowAlwaysNamesAnAlertname(t *testing.T) {
	tests := []struct {
		name      string
		alertname string
		namespace string
		wantField string
	}{
		{name: "an alertname and a namespace", alertname: "HighErrorRate", namespace: "prod"},
		{name: "an alertname in the absent-namespace partition", alertname: "HighErrorRate"},
		{
			name:      "the shortest legal alertname",
			alertname: "a",
		},
		{
			name:      "the longest legal alertname",
			alertname: strings.Repeat("a", MaxCasePolicyAlertnameBytes),
		},
		{
			name:      "the longest legal namespace",
			alertname: "HighErrorRate",
			namespace: strings.Repeat("n", MaxCasePolicyNamespaceBytes),
		},
		{name: "no alertname", wantField: "alertname"},
		{name: "a whitespace-only alertname", alertname: "   ", wantField: "alertname"},
		{
			name:      "an alertname one byte over the CHECK",
			alertname: strings.Repeat("a", MaxCasePolicyAlertnameBytes+1),
			wantField: "alertname",
		},
		{
			name:      "a namespace one byte over the CHECK",
			alertname: "HighErrorRate",
			namespace: strings.Repeat("n", MaxCasePolicyNamespaceBytes+1),
			wantField: "namespace",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CasePolicyDraft{
				Namespace: tc.namespace, Alertname: tc.alertname,
			}.Validate()
			if tc.wantField == "" {
				require.NoError(t, err)
				return
			}
			requireViolation(t, err, tc.wantField, "length")
		})
	}
}

// ------------------------------------------------------------- the bounds on W

// TestTheRetentionWindowBoundsAreTheDDLCheckVerbatim is `case_policy_window_ck`:
// 0 to 86400 seconds, whole seconds only.
//
// ⭐ 0 IS LEGAL AND MEANS TODAY'S BEHAVIOUR. A stored 0 and an absent row are the
// same instruction, which is why a create carrying 0 is accepted rather than
// rejected as a no-op: an operator pinning "no window, on purpose" for one
// alertname is making a statement.
//
// ⭐ 86400 IS THE CEILING because a longer window keeps an episode open across a
// whole shift's worth of unrelated firings, which stops being noise reduction and
// starts being one case that means nothing.
//
// ⛔ AND A FRACTIONAL WINDOW IS REFUSED RATHER THAN TRUNCATED. The column is `INT`
// seconds, so 90.5s would be stored as a different rule from the one somebody
// wrote.
func TestTheRetentionWindowBoundsAreTheDDLCheckVerbatim(t *testing.T) {
	assert.Equal(t, 0, MinCaseRetentionWindowSeconds)
	assert.Equal(t, 86400, MaxCaseRetentionWindowSeconds)
	assert.Equal(t, time.Duration(0), MinCaseRetentionWindow)
	assert.Equal(t, 24*time.Hour, MaxCaseRetentionWindow, "the ceiling is one day")

	tests := []struct {
		name    string
		window  time.Duration
		wantErr bool
	}{
		{name: "the floor — today's behaviour", window: 0},
		{name: "one second", window: time.Second},
		{name: "a working window", window: 10 * time.Minute},
		{name: "the ceiling", window: MaxCaseRetentionWindow},
		{name: "one second over the ceiling", window: MaxCaseRetentionWindow + time.Second, wantErr: true},
		{name: "one second under the floor", window: -time.Second, wantErr: true},
		{name: "a whole day and a bit", window: 25 * time.Hour, wantErr: true},
		{name: "a fractional second", window: 1500 * time.Millisecond, wantErr: true},
		{name: "sub-second", window: 500 * time.Millisecond, wantErr: true},
		{name: "a negative fraction", window: -500 * time.Millisecond, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CasePolicyDraft{
				Alertname: "HighErrorRate", RetentionWindow: tc.window,
			}.Validate()
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			requireViolation(t, err, "retention_window_seconds", "range")
		})
	}
}

// TestTheCasePolicyViolationPathIsTheClientsSpelling — a violation path is what a
// settings form maps onto a control (SPEC §L.8.2), so it is the JSON name and
// never the column name. `retention_window_s` is a spelling no client has ever
// been sent.
func TestTheCasePolicyViolationPathIsTheClientsSpelling(t *testing.T) {
	err := CasePolicyDraft{Alertname: "HighErrorRate", RetentionWindow: time.Hour * 48}.Validate()
	require.Error(t, err)

	var e *errs.Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, errs.KindValidation, e.Kind)
	assert.Equal(t, "case_policy_invalid", e.Code)

	for _, v := range errs.ViolationsOf(err) {
		assert.NotEqual(t, "retention_window_s", v.Field, "the column name is not a client's word")
		assert.NotContains(t, v.Field, "/", "this row's fields are flat")
	}
}

// -------------------------------------------------------------------- the patch

// TestTheCasePolicyPatchCannotMoveARowBetweenAxes states the immutability as a
// property of the TYPE rather than as a runtime check, which is how the
// implementation states it: a field that cannot be sent cannot be sent by
// accident.
//
// ⛔ (namespace, alertname) IS THE ROW'S IDENTITY under `case_policy_axes_uniq`.
// Moving a window from one pair to another is deleting one rule and writing a
// second, and a PATCH that could do it silently would let an operator believe a
// window applies to an alertname it no longer names.
func TestTheCasePolicyPatchCannotMoveARowBetweenAxes(t *testing.T) {
	typ := reflect.TypeOf(CasePolicyPatch{})
	require.Equal(t, 1, typ.NumField(),
		"the patch carries exactly one knob; a second field is a new decision")
	assert.Equal(t, "RetentionWindow", typ.Field(0).Name)

	for _, forbidden := range []string{"Namespace", "Alertname", "OrgID", "ID"} {
		_, found := typ.FieldByName(forbidden)
		assert.False(t, found, "CasePolicyPatch must never gain %s", forbidden)
	}
}

// TestAnEmptyCasePolicyPatchChangesNothing — the absent field is the whole of the
// partial update, so "nothing to do" is answerable without touching the row.
func TestAnEmptyCasePolicyPatchChangesNothing(t *testing.T) {
	assert.True(t, CasePolicyPatch{}.IsEmpty())

	w := 5 * time.Minute
	assert.False(t, CasePolicyPatch{RetentionWindow: &w}.IsEmpty())

	zero := time.Duration(0)
	assert.False(t, CasePolicyPatch{RetentionWindow: &zero}.IsEmpty(),
		"narrowing a window to 0 is a change, not an absence")
}

// TestACasePolicyPatchIsProvedAgainstTheRowItLandsOn — the patch is validated as
// the MERGED row, so the bounds cannot be escaped by sending only half of one.
//
// ⭐ AND IT REPORTS NO AXIS VIOLATION, deliberately: the axes came out of the
// database and were proved by the CHECKs on the way in, so re-reporting them would
// point a form at two controls the request does not contain.
func TestACasePolicyPatchIsProvedAgainstTheRowItLandsOn(t *testing.T) {
	existing := CasePolicy{
		ID:              uuid.MustParse("018f3a4b-0000-7000-8000-0000000001c1"),
		Namespace:       "",
		Alertname:       "HighErrorRate",
		RetentionWindow: 10 * time.Minute,
	}

	t.Run("a window inside the CHECK is taken", func(t *testing.T) {
		w := MaxCaseRetentionWindow
		require.NoError(t, CasePolicyPatch{RetentionWindow: &w}.Validate(existing))
	})

	t.Run("a window outside the CHECK is refused", func(t *testing.T) {
		w := MaxCaseRetentionWindow + time.Second
		requireViolation(t, CasePolicyPatch{RetentionWindow: &w}.Validate(existing),
			"retention_window_seconds", "range")
	})

	t.Run("narrowing to zero is legal — it is today's behaviour", func(t *testing.T) {
		w := time.Duration(0)
		require.NoError(t, CasePolicyPatch{RetentionWindow: &w}.Validate(existing))
	})

	t.Run("an empty patch validates against the row untouched", func(t *testing.T) {
		require.NoError(t, CasePolicyPatch{}.Validate(existing))
	})

	t.Run("the axes are not re-reported, even when the stored row could not be written today",
		func(t *testing.T) {
			// A row whose axes would fail `case_policy_name_ck` cannot exist, but the
			// point is WHICH controls a violation names: a patch names the one field it
			// carries and never a field the request does not contain.
			odd := existing
			odd.Alertname = ""
			w := 30 * time.Second
			require.NoError(t, CasePolicyPatch{RetentionWindow: &w}.Validate(odd))
		})
}

// requireViolation asserts that err is the case-policy validation refusal and that
// it names exactly the field and code given.
func requireViolation(t *testing.T, err error, field, code string) {
	t.Helper()
	requireKind(t, err, errs.KindValidation, "case_policy_invalid")

	vs := errs.ViolationsOf(err)
	require.Len(t, vs, 1, "one broken bound is one violation")
	assert.Equal(t, field, vs[0].Field)
	assert.Equal(t, code, vs[0].Code)
	assert.NotEmpty(t, vs[0].Message, "a violation message is rendered to a human")
}
