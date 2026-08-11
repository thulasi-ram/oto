package domain_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ddlReasons is `ingest_rejections_reason_ck` as the DDL spells it, in the DDL's
// own order (00006, widened by 00035).
//
// ⛔ THIS IS THE SECOND OF THREE COPIES AND IT IS DELIBERATE. The enum is a
// three-way contract — the Go constants, this CHECK, and the `reason` label of
// `oto_ingest_rejected_total` — and there is no generator between them. Writing
// the DDL list out here is what makes a missing migration a FAILING TEST instead
// of a 23514 on the rejection write, i.e. an alert lost while recording that an
// alert was lost. If this list changes, `db/migrations` changes in the same
// commit.
var ddlReasons = []domain.Reason{
	"too_many_labels",
	"label_value_too_large",
	"label_name_too_large",
	"labelset_too_large",
	"too_many_annotations",
	"annotation_too_large",
	"annotation_unstorable",
	"missing_alertname",
	"invalid_label_name",
	"invalid_label_value",
	"timestamp_out_of_window",
	"too_many_alerts",
	"body_too_large",
	"undecodable",
	"unknown_source",
}

func TestReasonSetMatchesTheDDLCheck(t *testing.T) {
	for _, r := range ddlReasons {
		assert.True(t, r.Valid(), "the DDL admits %q but the Go enum does not", r)
	}

	// And nothing beyond it: a Go member with no DDL member is the failure mode
	// that writes a row Postgres will refuse.
	known := make(map[domain.Reason]struct{}, len(ddlReasons))
	for _, r := range ddlReasons {
		known[r] = struct{}{}
	}
	for _, r := range []domain.Reason{
		domain.ReasonTooManyLabels,
		domain.ReasonLabelValueTooLarge,
		domain.ReasonLabelNameTooLarge,
		domain.ReasonLabelSetTooLarge,
		domain.ReasonTooManyAnnotations,
		domain.ReasonAnnotationTooLarge,
		domain.ReasonAnnotationUnstorable,
		domain.ReasonMissingAlertname,
		domain.ReasonInvalidLabelName,
		domain.ReasonInvalidLabelValue,
		domain.ReasonTimestampOutOfWindow,
		domain.ReasonTooManyAlerts,
		domain.ReasonBodyTooLarge,
		domain.ReasonUndecodable,
		domain.ReasonUnknownSource,
	} {
		assert.Contains(t, known, r, "%q has no member in ingest_rejections_reason_ck", r)
	}

	assert.False(t, domain.Reason("nonsense").Valid())
	assert.False(t, domain.Reason("").Valid())
}

// TestReasonFromError_NamesWhatWasActuallyWrong is the regression for the defect
// that added `invalid_label_value` and `annotation_unstorable`.
//
// ⭐ EVERY CASE HERE ASSERTS "NOT undecodable" ON PURPOSE. The mapping has a
// fallback, and the fallback is a sentence about the BODY — "these bytes are not
// an Alertmanager webhook payload". Applied to an alert whose payload decoded
// perfectly and whose only fault was one unwritable label value, it sent an
// operator hunting for malformed JSON that did not exist. `ingest_rejections` is
// the only place a rejected alert survives (§C.9.1); the reason column is the
// whole diagnosis.
func TestReasonFromError_NamesWhatWasActuallyWrong(t *testing.T) {
	tests := []struct {
		name string
		mint func() error
		want domain.Reason
	}{
		{
			name: "a NUL in a label value",
			mint: func() error {
				_, err := alerts.NewLabelSet(map[string]string{"alertname": "X", "pod": "a\x00b"})
				return err
			},
			want: domain.ReasonInvalidLabelValue,
		},
		{
			name: "invalid UTF-8 in a label value",
			mint: func() error {
				_, err := alerts.NewLabelSet(map[string]string{"alertname": "X", "pod": "\xff"})
				return err
			},
			want: domain.ReasonInvalidLabelValue,
		},
		{
			name: "a NUL in an annotation value",
			mint: func() error {
				_, err := alerts.NewAnnotations(map[string]string{"summary": "a\x00b"})
				return err
			},
			want: domain.ReasonAnnotationUnstorable,
		},
		{
			name: "invalid UTF-8 in an annotation name",
			mint: func() error {
				_, err := alerts.NewAnnotations(map[string]string{"sum\xffmary": "v"})
				return err
			},
			want: domain.ReasonAnnotationUnstorable,
		},
		{
			// The bounds that already worked, kept here so a refactor of the mapping
			// cannot quietly narrow it to the two new members.
			name: "an unstorable label NAME is a name problem, not a value problem",
			mint: func() error {
				_, err := alerts.NewLabelSet(map[string]string{"alertname": "X", "a\xffb": "v"})
				return err
			},
			want: domain.ReasonInvalidLabelName,
		},
		{
			name: "a missing alertname",
			mint: func() error {
				_, err := alerts.NewLabelSet(map[string]string{"pod": "p1"})
				return err
			},
			want: domain.ReasonMissingAlertname,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.mint()
			require.Error(t, err)

			got := domain.ReasonFromError(err)
			assert.Equal(t, tc.want, got)
			assert.NotEqual(t, domain.ReasonUndecodable, got,
				"a decodable payload with one bad field must never be reported as undecodable")
			assert.True(t, got.Valid(), "the reason must be writable to ingest_rejections")
		})
	}
}

// TestReasonFromError_UnknownCodeStillFallsBack. The fallback is not being
// removed — it is the honest answer for a code nobody has thought about. It is
// only wrong when it stands in for a bound oto deliberately added.
func TestReasonFromError_UnknownCodeStillFallsBack(t *testing.T) {
	assert.Equal(t, domain.ReasonUndecodable,
		domain.ReasonFromError(errs.New(errs.KindValidation, "something_new", "no member yet")))
	assert.Equal(t, domain.ReasonUndecodable,
		domain.ReasonFromError(errors.New("a bare error carries no code at all")))
}
