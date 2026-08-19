package repository

import (
	"context"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// CasePolicyRepository is the SQL over `case_policy_config` — per
// (namespace, alertname) shaping of the CASE itself, migration 00057.
//
// ⭐⭐ WHAT THE TABLE IS FOR. Today it carries one knob: W, the CASE RETENTION
// WINDOW. A case whose alert has resolved stays OPEN for W and closes only once
// the alert has stayed resolved for W, so a re-fire inside W lands in the
// still-open episode rather than opening the next `seq`. That is how an alert
// flapping six times in ten minutes produces ONE case, one notification and one
// thread reply — the noise never exists, instead of existing and being withheld at
// delivery, which is what §B.6 refuses.
//
// ⭐ WHY THE AXES ARE (namespace, alertname). They are ADR 0038's own axes —
// `SplitLabels(ls) = {alertname} ∪ {namespace}`, `alerts/domain/keys.go` — so an
// operator learns ONE set of dimensions for grouping and for retention rather than
// two. It is coarser than `group_key` by exactly one axis, `cluster_key`: one
// window governs the same alertname in every cluster of an org. Splitting later is
// the safe direction (ADR 0038); starting split would make an operator write the
// same number once per cluster to get the obvious behaviour.
//
// ⛔ AN ABSENT NAMESPACE IS THE EMPTY STRING HERE AND NOWHERE ELSE IN OTO.
// `alerts.namespace` is NULL for both absent and empty, because Prometheus treats
// the two as equivalent and `SplitLabels` omits the axis for both — so they are
// already ONE partition, and this table simply has to spell that partition with a
// value a UNIQUE index can compare. A NULL would let one org hold two
// contradictory windows for one alertname, since two NULLs are not equal under
// `case_policy_axes_uniq`. Every caller normalises through `NormaliseNamespace`.
//
// ⭐ IT IS READ-ONLY. There is deliberately no writer here: the settings surface
// that creates these rows is a separate concern from the ingest path that reads
// them, exactly as `notification/repository`'s PolicyRepository (read) is separate
// from its ConfigRepository (write), so the evaluator cannot rewrite the rule it
// is evaluating.
type CasePolicyRepository struct{ q db.Querier }

// NewCasePolicyRepository builds the repository over a fallback querier.
func NewCasePolicyRepository(q db.Querier) *CasePolicyRepository {
	return &CasePolicyRepository{q: q}
}

func (r *CasePolicyRepository) db(ctx context.Context) db.Querier {
	return db.FromContext(ctx, r.q)
}

// NormaliseNamespace maps a namespace label onto the partition this table keys on:
// absent, empty and whitespace-only all become "".
//
// It is exported because the CALLER holds the alert, and the alert is where a
// namespace can be absent. Normalising at the boundary rather than inside the
// query keeps the two spellings from reaching SQL at all.
//
// ⭐ IT DELEGATES RATHER THAN TRIMMING AGAIN. The settings surface writes rows
// through `domain.NormaliseNamespace`; two independent trims over one UNIQUE index
// is the shape where the partition a write lands in and the partition this lookup
// probes drift apart WITHOUT ANYTHING FAILING — the window would simply never
// apply, which is the quiet degradation §B.6 refuses.
func NormaliseNamespace(ns string) string { return domain.NormaliseNamespace(ns) }

const retentionWindowSQL = `
SELECT retention_window_s
  FROM case_policy_config
 WHERE org_id = $1 AND namespace = $2 AND alertname = $3`

// RetentionWindow reads W for one (namespace, alertname).
//
// ⭐⭐ NO ROW MEANS ZERO, AND ZERO MEANS TODAY'S BEHAVIOUR. The table starts empty
// and stays empty until an operator configures something, so this returns 0 for
// every alert on every deployment that has not opted in — and 0 makes the §B.3 T5
// arm take no deferral branch at all, which is the pre-00057 close path unedited.
// A missing row is therefore NOT an error and not a distinguishable state: there is
// nothing a caller could do differently, and an `(value, found, error)` triple here
// would invite one to invent a fallback that is not the shipped default.
//
// ⛔ AN UNREADABLE ROW IS NOT SILENTLY ZERO. A query failure is returned, because
// "the operator asked for a ten-minute window and got today's noise because a
// SELECT failed" is exactly the kind of quiet degradation an alerting product
// cannot afford. The caller decides — the ingest path treats it as a batch error,
// which is the loud direction.
func (r *CasePolicyRepository) RetentionWindow(
	ctx context.Context, s db.TenantScope, namespace, alertname string,
) (time.Duration, error) {
	if err := db.RequireScope(s); err != nil {
		return 0, err
	}
	name := strings.TrimSpace(alertname)
	if name == "" {
		// Every Alert has an alertname (§C.2) and every group key hashes it (ADR
		// 0038), so an empty one is a programming error rather than a lookup miss.
		return 0, errs.Internal("case_policy_alertname_missing",
			errsMissing("alertname is required"))
	}

	var seconds int32
	err := r.db(ctx).QueryRow(ctx, retentionWindowSQL,
		s.OrgID(), NormaliseNamespace(namespace), name).Scan(&seconds)
	if err != nil {
		if isNoRows(err) {
			return 0, nil
		}
		return 0, mapErr(err, "read case retention window")
	}
	if seconds <= 0 {
		// `case_policy_window_ck` already refuses a negative, and a stored 0 is the
		// same instruction as no row at all. Collapsing them here means one answer
		// reaches the machine, so "W is off" cannot arrive in two spellings.
		return 0, nil
	}
	return time.Duration(seconds) * time.Second, nil
}
