package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// CasePolicy is one row of `case_policy_config` (migration 00057): the per
// (namespace, alertname) shaping of the Case itself. Today it carries exactly one
// knob — W, the CASE RETENTION WINDOW.
//
// ⭐⭐ W MOVES *WHEN* A CASE CLOSES AND NOTHING ELSE. A case whose alert has
// resolved stays OPEN for W and closes only once the alert has stayed resolved for
// W, so a re-fire inside W is an ordinary repeat observation landing in the
// still-open episode rather than the next `seq`. Six flaps become ONE case, one
// notification and one thread reply — the noise never exists, instead of existing
// and being withheld at delivery, which is the distinction §B.6 refuses to blur.
// See `Case.resolvePendingAt` for the mechanism.
//
// ⛔ IT IS A DELAYED CLOSE AND NEVER A REOPEN (ADR 0040). Nothing configurable
// here can resurrect a closed episode.
//
// ⭐ WHY THE AXES ARE (namespace, alertname) AND WHY THERE IS NO WILDCARD ROW.
// They are ADR 0038's own axes, so an operator learns ONE set of dimensions for
// grouping and for retention rather than two. There is deliberately no org-wide
// row and no wildcard: the default lives in code as 0, where it cannot be
// half-configured, and an ABSENT ROW *IS* the default. A wildcard would introduce
// precedence — "which row wins" — which is the question this table exists not to
// have.
//
// ⛔ THE ABSENT NAMESPACE IS THE EMPTY STRING, HERE AND NOWHERE ELSE IN OTO.
// `alerts.namespace` is NULL for both absent and empty because Prometheus treats
// the two as equivalent, so they are already ONE partition and this table simply
// spells that partition with a value a UNIQUE index can compare. Everything that
// reads or writes it goes through NormaliseNamespace.
type CasePolicy struct {
	ID    uuid.UUID
	OrgID uuid.UUID

	// Namespace is the ADR 0038 axis. The empty string is the absent-namespace
	// partition, not a missing value.
	Namespace string
	// Alertname is mandatory, because it is mandatory on every Alert (§C.2) and on
	// every group key (ADR 0038).
	Alertname string

	// RetentionWindow is W. Zero is the shipped default and is byte-for-byte the
	// pre-00057 behaviour: the §B.3 close path takes no deferral branch at all.
	RetentionWindow time.Duration

	CreatedAt time.Time
	UpdatedAt time.Time
}

// The bounds of one `case_policy_config` row.
//
// ⛔ EVERY NUMBER HERE IS A DDL CHECK, VERBATIM (SPEC R9, §L.2.4 P-10). They are
// declared once, in this block, and the HTTP layer states the same range in its
// `validate` tag and in `api/openapi/openapi.yaml` only because layer 1 has to
// reject a bad body before the domain sees it — no handler re-derives them, and no
// handler adds a bound of its own. A validator looser than its CHECK turns a 422
// into a 500; one tighter rejects rows the database would have taken.
const (
	// MinCaseRetentionWindowSeconds is 0 — `case_policy_window_ck`'s floor — and 0
	// is TODAY'S BEHAVIOUR rather than an approximation of it. A stored 0 and an
	// absent row are the same instruction, which is why a create carrying 0 is
	// legal: an operator pinning "no window, on purpose" for one alertname is
	// making a statement, and the table is where statements live.
	MinCaseRetentionWindowSeconds = 0
	// MaxCaseRetentionWindowSeconds is one day — `case_policy_window_ck`'s ceiling.
	// A longer window keeps an episode open across a whole shift's worth of
	// unrelated firings, which stops being noise reduction and starts being one case
	// that means nothing.
	MaxCaseRetentionWindowSeconds = 86400

	// MaxCasePolicyAlertnameBytes mirrors `case_policy_name_ck`, which mirrors
	// `alerts_name_ck`.
	MaxCasePolicyAlertnameBytes = 1024
	// MaxCasePolicyNamespaceBytes mirrors `case_policy_ns_ck`. There is no floor:
	// the empty string is the absent-namespace partition.
	MaxCasePolicyNamespaceBytes = 1024
)

// MinCaseRetentionWindow and MaxCaseRetentionWindow are the same two bounds as
// Durations, for the callers that hold W as one.
const (
	MinCaseRetentionWindow = MinCaseRetentionWindowSeconds * time.Second
	MaxCaseRetentionWindow = MaxCaseRetentionWindowSeconds * time.Second
)

// NormaliseNamespace maps a namespace label onto the partition `case_policy_config`
// keys on: absent, empty and whitespace-only all become "".
//
// ⭐ IT IS THE ONE DEFINITION. The ingest-path reader
// (`repository.NormaliseNamespace`) delegates here rather than trimming again, so
// the partition a settings write lands in and the partition an alert looks up
// cannot come apart — which is the only way this table can be wrong without
// anything failing.
func NormaliseNamespace(ns string) string { return strings.TrimSpace(ns) }

// CasePolicyDraft creates one row.
//
// It is not a `CasePolicy` because the id and the timestamps are server-owned, and
// a create that accepted them would let a caller assert them.
type CasePolicyDraft struct {
	// ID lets the caller NAME the row before it exists, which is what lets an
	// `Idempotency-Key` claim record the id of what a create made in the SAME
	// transaction as the insert. Zero means the repository mints one.
	ID              uuid.UUID
	Namespace       string
	Alertname       string
	RetentionWindow time.Duration
}

// Validate enforces every bound the DDL enforces, on the row the write would
// produce.
//
// The violation paths are the JSON names, never the column names: a violation path
// is what a settings form maps onto a control (CONTEXT.md §5b, SPEC §L.8.2), and
// `retention_window_s` is a spelling no client has ever been sent.
func (d CasePolicyDraft) Validate() error {
	v := violateCasePolicy(d.Namespace, d.Alertname, d.RetentionWindow, true)
	if len(v) > 0 {
		return errs.Validation("case_policy_invalid",
			"the case retention policy is not valid", v...)
	}
	return nil
}

// Normalised returns the draft with its namespace folded onto the partition it
// will be stored in, so the caller writes the same value the reader looks up.
func (d CasePolicyDraft) Normalised() CasePolicyDraft {
	d.Namespace = NormaliseNamespace(d.Namespace)
	d.Alertname = strings.TrimSpace(d.Alertname)
	return d
}

// CasePolicyPatch is the partial update.
//
// ⛔ IT CARRIES NO `Namespace` AND NO `Alertname`, AND MUST NEVER GAIN EITHER.
// The two together are the row's identity under `case_policy_axes_uniq`; moving a
// window from one pair to another is deleting one rule and writing a second, and a
// PATCH that could do it silently would let an operator believe a window applies
// to an alertname it no longer names. The immutability is expressed as an ABSENT
// FIELD rather than as a runtime check, because a field that cannot be sent cannot
// be sent by accident.
type CasePolicyPatch struct {
	RetentionWindow *time.Duration
}

// IsEmpty reports whether the patch would change nothing.
func (p CasePolicyPatch) IsEmpty() bool { return p.RetentionWindow == nil }

// Validate proves the patch against the row it lands on.
func (p CasePolicyPatch) Validate(existing CasePolicy) error {
	merged := existing
	if p.RetentionWindow != nil {
		merged.RetentionWindow = *p.RetentionWindow
	}
	v := violateCasePolicy(merged.Namespace, merged.Alertname, merged.RetentionWindow, false)
	if len(v) > 0 {
		return errs.Validation("case_policy_invalid",
			"the case retention policy is not valid", v...)
	}
	return nil
}

// violateCasePolicy is the ONE reading of the three CHECKs. Both Validate methods
// route through it so that a create and a patch cannot answer differently about
// the same row.
//
// `checkAxes` is false for a patch, whose axes came out of the database and were
// therefore already proved by the CHECKs on the way in; re-reporting them would
// point a form at two controls the request does not contain.
func violateCasePolicy(
	namespace, alertname string, window time.Duration, checkAxes bool,
) []errs.Violation {
	var v []errs.Violation

	if checkAxes {
		if n := len(strings.TrimSpace(alertname)); n < 1 || n > MaxCasePolicyAlertnameBytes {
			v = append(v, errs.Violation{
				Field: "alertname", Code: "length",
				Message: "an alertname is 1 to 1024 characters: a row with no alertname would be " +
					"an org-wide default, which this table deliberately does not offer",
			})
		}
		if len(NormaliseNamespace(namespace)) > MaxCasePolicyNamespaceBytes {
			v = append(v, errs.Violation{
				Field: "namespace", Code: "length",
				Message: "a namespace is at most 1024 characters; the empty string is the " +
					"absent-namespace partition",
			})
		}
	}

	switch {
	case window%time.Second != 0:
		// The column is `INT` seconds, so a fractional duration would be truncated
		// into a different rule than the one somebody wrote.
		v = append(v, errs.Violation{
			Field: "retention_window_seconds", Code: "range",
			Message: "the case retention window is a whole number of seconds",
		})
	case window < MinCaseRetentionWindow || window > MaxCaseRetentionWindow:
		v = append(v, errs.Violation{
			Field: "retention_window_seconds", Code: "range",
			Message: "the case retention window is 0 to 86400 seconds; 0 means the case closes " +
				"on the resolve, which is what oto did before this table existed",
		})
	}
	return v
}
