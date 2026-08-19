package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"regexp"
	"sort"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// MaxPolicyGroupBy is `policies_group_by_ck`. Eight is well above any real
// collapse — Alertmanager's own `group_by` lists run to one to three labels in
// practice — and the bound is what stops a policy naming so many labels that
// every alert lands in a conversation of its own.
const MaxPolicyGroupBy = 8

// CollapseKeyPrefix marks a policy collapse key, so a key is never mistaken for
// the `gk_` group key it deliberately is not.
const CollapseKeyPrefix = "ck_"

// labelNameRe is Prometheus's own label-name grammar.
//
// ⛔ IT IS ENFORCED HERE RATHER THAN IN A CHECK, and 00063's header records why:
// "every element matches" cannot be written as a CHECK constraint without a
// subquery or a helper schema object. This is the write path, so a bad label name
// comes back as a field-level violation the settings form can point at instead of
// as a 23514 an operator has to decode.
var labelNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// CollapseKey is the delivery-time identity of a conversation under this policy:
// which facts share one message.
//
// ⭐ IT IS NOT `alert_groups.group_key` UNDER ANOTHER NAME, AND THE DIFFERENCE IS
// THE WHOLE POINT OF git-bug 7570090. The group key is oto's FIXED identity axes
// — `alertname`, and `namespace` when present — and it is IMMUTABLE for an alert's
// whole life, because alert identity IS the label set and a stored group row
// cannot be re-parented afterwards. This key is a DELIVERY-TIME COLLAPSE that an
// operator owns. Editing `group_by` changes which FUTURE deliveries share a
// conversation and moves nothing that already landed, which is exactly the
// property that makes it safe to make configurable at all — the objection ADR 0038
// recorded against a tunable split key was about re-parenting, and there is
// nothing here to re-parent.
//
// ⛔ AN EMPTY `group_by` RETURNS THE EMPTY STRING, NOT A HASH OF NOTHING. Empty is
// the default and means "this policy does not collapse", so the caller must be
// able to tell "no collapse configured" from "collapsed into one bucket" — a hash
// of the empty input would be a real, single, shared key and would silently merge
// every delivery the policy makes.
//
// A label the alert does not carry contributes its name and an empty value rather
// than being skipped, so `{node: ""}` and `{}` are different buckets. Skipping it
// would make an alert missing `node` collapse together with every other alert
// missing `node`, across unrelated services, which is the loudest possible failure
// of a grouping decision.
//
// The framing is `ComputeGroupKey`'s: every field is length-prefixed, so no pair
// of inputs can be concatenated into the same byte string.
func (p Policy) CollapseKey(labels map[string]string) string {
	if len(p.GroupBy) == 0 {
		return ""
	}

	// Sorted, so the key does not depend on the order an operator typed the
	// labels in. `group_by: [node, pod]` and `[pod, node]` are one policy.
	names := make([]string, len(p.GroupBy))
	copy(names, p.GroupBy)
	sort.Strings(names)

	h := sha256.New()
	writeCollapseField(h, p.ID[:])
	for _, n := range names {
		writeCollapseField(h, []byte(n))
		writeCollapseField(h, []byte(labels[n]))
	}
	return CollapseKeyPrefix + hex.EncodeToString(h.Sum(nil)[:16])
}

// writeCollapseField length-prefixes one field, so `("ab","c")` and `("a","bc")`
// cannot hash alike.
func writeCollapseField(h hash.Hash, b []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(b)
}

// validateGroupBy restates `policies_group_by_ck` and adds the per-element
// grammar the CHECK cannot express.
func (p Policy) validateGroupBy() []errs.Violation {
	if len(p.GroupBy) == 0 {
		return nil
	}
	var v []errs.Violation
	if len(p.GroupBy) > MaxPolicyGroupBy {
		v = append(v, errs.Violation{
			Field: "group_by", Code: "max_items",
			Message: "a policy collapses on at most 8 labels",
		})
	}
	seen := make(map[string]struct{}, len(p.GroupBy))
	for _, n := range p.GroupBy {
		if !labelNameRe.MatchString(n) {
			v = append(v, errs.Violation{
				Field: "group_by", Code: "label_name",
				Message: "a group_by entry must be a label name — a letter or underscore, " +
					"then letters, digits or underscores. A name no label set can contain " +
					"collapses nothing and matches nothing",
			})
			continue
		}
		if _, dup := seen[n]; dup {
			// Not merely untidy: a repeated label would be hashed twice and make a
			// key that no single-mention policy can reproduce, so two policies an
			// operator wrote as identical would collapse differently.
			v = append(v, errs.Violation{
				Field: "group_by", Code: "duplicate",
				Message: "a label may appear once in group_by",
			})
		}
		seen[n] = struct{}{}
	}
	return v
}
