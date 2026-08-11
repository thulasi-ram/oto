package domain

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/validate"
)

// Identity-key shapes (SPEC §C). Each mirrors a DDL CHECK; the patterns
// themselves live once in platform/validate so the drift test has one place to
// look.
const (
	// AlertKeyPrefix marks an Alert identity key.
	AlertKeyPrefix = "ak_"
	// GroupKeyPrefix marks an AlertGroup identity key.
	GroupKeyPrefix = "gk_"

	// identityDigestBytes is the number of SHA-256 bytes an identity key keeps.
	// 128 bits, rendered as 26 base32hex characters.
	identityDigestBytes = 16

	// fingerprintHexLen is the width of Alertmanager's "%016x" fingerprint.
	fingerprintHexLen = 16
)

// base32hexLower is RFC 4648 base32hex without padding, lowercased. Its alphabet
// is 0-9a-v, which is exactly what the DDL CHECKs on alert_key and group_key
// accept, and it sorts in the same order as the bytes it encodes.
var base32hexLower = base32.HexEncoding.WithPadding(base32.NoPadding)

func encodeIdentity(digest []byte) string {
	return strings.ToLower(base32hexLower.EncodeToString(digest[:identityDigestBytes]))
}

// ClusterKey is the human-chosen identity of a Cluster — a logical failure
// domain. It participates in Alert identity (C.2), so its charset is
// load-bearing: `KubePodCrashLooping{namespace="prod"}` in `prod-eu` and in
// `prod-us` are DIFFERENT Alerts, because they have different blast radii.
type ClusterKey struct{ s string }

// NewClusterKey validates a cluster key against clusters_key_ck.
func NewClusterKey(s string) (ClusterKey, error) {
	if !validate.ClusterKeyRe.MatchString(s) {
		return ClusterKey{}, errs.Newf(errs.KindValidation, "cluster_key",
			"cluster_key must match %s", validate.PatternClusterKey)
	}
	return ClusterKey{s: s}, nil
}

// String renders the cluster key.
func (k ClusterKey) String() string { return k.s }

// IsZero reports whether the key is unset.
func (k ClusterKey) IsZero() bool { return k.s == "" }

// AlertKey is the identity of an Alert: the PRIMARY dedup key (SPEC §C.2).
//
// It is the SHA-256 of `(org_id, cluster_key, canonical labels minus the source's
// ignore_labels)`, truncated to 128 bits and rendered as `ak_` plus 26 lowercase
// base32hex characters — URL-safe and human-copyable. Dedup is enforced by the
// UNIQUE (org_id, alert_key) constraint, never by a read-then-write check.
type AlertKey struct{ s string }

// NewAlertKey parses an alert key, validating it against alerts_key_ck.
func NewAlertKey(s string) (AlertKey, error) {
	if !validate.AlertKeyRe.MatchString(s) {
		return AlertKey{}, errs.Newf(errs.KindValidation, "alert_key",
			"alert_key must match %s", validate.PatternAlertKey)
	}
	return AlertKey{s: s}, nil
}

// ComputeAlertKey derives the identity of an Alert (SPEC §C.2):
//
//	"ak_" || base32hexLower( sha256(
//	     field(org_id_bytes(16)) || field(cluster_key)
//	  || canon(labels, ignore) )[0:16] )
//
// where field(x) := uint32be(len(x)) || x, the framing writeField documents. The
// trailing canon() is written raw and needs no prefix: it is the remainder.
//
// Ignored labels are still stored on the Alert; they are merely not hashed.
// Changing a source's ignore_labels does NOT re-key existing Alerts — new
// identities are created, and that is documented behaviour.
func ComputeAlertKey(orgID uuid.UUID, clusterKey ClusterKey, ls LabelSet, ignore []string) AlertKey {
	h := sha256.New()
	writeField(h, orgID[:])
	writeField(h, []byte(clusterKey.s))
	_, _ = h.Write(ls.Canonical(ignore))
	return AlertKey{s: AlertKeyPrefix + encodeIdentity(h.Sum(nil))}
}

// String renders the alert key.
func (k AlertKey) String() string { return k.s }

// IsZero reports whether the key is unset.
func (k AlertKey) IsZero() bool { return k.s == "" }

// SourceFingerprint is Alertmanager's own fingerprint of a label set, recomputed
// by oto rather than trusted from the wire (SPEC §C.3).
//
// It is the join key for `/api/v2/alerts` reconciliation and for debugging
// against upstream. It is NEVER the product identity: that is AlertKey.
type SourceFingerprint struct{ s string }

// NewSourceFingerprint parses a fingerprint, validating it against
// alerts_srcfp_ck.
func NewSourceFingerprint(s string) (SourceFingerprint, error) {
	if !validate.SourceFingerprintRe.MatchString(s) {
		return SourceFingerprint{}, errs.Newf(errs.KindValidation, "source_fingerprint",
			"source_fingerprint must match %s", validate.PatternSourceFingerprint)
	}
	return SourceFingerprint{s: s}, nil
}

// ComputeSourceFingerprint recomputes Alertmanager's FNV-1a 64 fingerprint over
// the FULL label set (C.3).
func ComputeSourceFingerprint(ls LabelSet) SourceFingerprint { return ls.Fingerprint() }

// String renders the fingerprint as 16 lowercase hex characters.
func (f SourceFingerprint) String() string { return f.s }

// IsZero reports whether the fingerprint is unset.
func (f SourceFingerprint) IsZero() bool { return f.s == "" }

// GroupKey is the durable identity of an Alertmanager notification group
// (SPEC §C.4).
//
// It is stable across `alertmanager.yml` route edits, which is exactly what
// Alertmanager's own `groupKey` is not: AM's value embeds route config and
// changes on reload. AM's value is stored verbatim as `source_group_key` for
// observability and MUST NOT be parsed — it is unescaped and unbounded (C3).
type GroupKey struct{ s string }

// NewGroupKey parses a group key.
func NewGroupKey(s string) (GroupKey, error) {
	if !validate.GroupKeyRe.MatchString(s) {
		return GroupKey{}, errs.Newf(errs.KindValidation, "group_key",
			"group_key must match %s", validate.PatternGroupKey)
	}
	return GroupKey{s: s}, nil
}

// ComputeGroupKey derives the durable group identity (SPEC §C.4):
//
//	"gk_" || base32hexLower( sha256(
//	     field(org_id_bytes(16)) || field(source_id_bytes(16))
//	  || field(receiver) || canon(groupLabels, {}) )[0:16] )
//
// where field(x) := uint32be(len(x)) || x, the framing writeField documents.
//
// `receiver` is free-form text out of the operator's alertmanager.yml and is the
// reason this key is length-prefixed rather than NUL-separated: under the old
// framing a receiver carrying a NUL could forge the leading bytes of the group
// labels and merge two unrelated notification groups into one.
//
// For a reconciler-sourced observation with no groupLabels, receiver is "" and
// groupLabels is the Alertmanager alert group's own labels.
func ComputeGroupKey(orgID, sourceID uuid.UUID, receiver string, groupLabels Labels) GroupKey {
	h := sha256.New()
	writeField(h, orgID[:])
	writeField(h, sourceID[:])
	writeField(h, []byte(receiver))
	_, _ = h.Write(groupLabels.Canonical(nil))
	return GroupKey{s: GroupKeyPrefix + encodeIdentity(h.Sum(nil))}
}

// String renders the group key.
func (k GroupKey) String() string { return k.s }

// IsZero reports whether the key is unset.
func (k GroupKey) IsZero() bool { return k.s == "" }

// RuleFingerprint is the content address of one Prometheus alerting-rule
// definition (SPEC §C.6). Rule drift — "the newest snapshot for this RuleKey has
// a different fingerprint than the one bound to the previous occurrence" — is the
// headline differentiator, and this is what makes it decidable.
type RuleFingerprint struct{ s string }

// NewRuleFingerprint parses a rule fingerprint.
func NewRuleFingerprint(s string) (RuleFingerprint, error) {
	if !validate.SHA256HexRe.MatchString(s) {
		return RuleFingerprint{}, errs.Newf(errs.KindValidation, "rule_fingerprint",
			"rule_fingerprint must match %s", validate.PatternSHA256Hex)
	}
	return RuleFingerprint{s: s}, nil
}

// ComputeRuleFingerprint content-addresses a rule definition (SPEC §C.6):
//
//	hex( sha256( field(expr) || field(for_seconds) || field(keep_firing_for_seconds)
//	   || field(canon(rule_labels, {})) || canon(rule_annotations, {}) ) )
//
// where field(x) := uint32be(len(x)) || x, the framing writeField documents.
// `expr` is free-form PromQL out of Prometheus and may carry any byte, which is
// why the fields are length-prefixed and not NUL-separated.
//
// # THIS IS THE ONLY IMPLEMENTATION OF §C.6
//
// There were two, over different input shapes, and they agreed by luck until
// c133981 cross-checked them — over the inputs they could BOTH express. Outside
// that overlap they disagreed: this one truncated `for` to whole seconds and the
// live one rendered it as a float, so `for: 1s500ms` had two content addresses
// and only one of them was ever stored. `rules/domain.Fingerprint` now calls
// this, and this renders durations the way the stored values were computed.
//
// # THE INPUT SHAPE IS RAW MAPS, DELIBERATELY
//
// Every other §C key takes constructed value objects because it hashes labels
// oto accepted at its own boundary. A rule's labels arrive from Prometheus and
// have never passed NewLabels — see CanonMap, which is the sanctioned door for
// exactly that and the reason this function needs no second implementation.
//
// Durations are seconds, rendered with strconv.FormatFloat(f, 'f', -1, 64) — the
// shortest form that round-trips — so 600 and 600.0 are one rule and `for: 500ms`
// is addressable. Prometheus's `/api/v1/rules` reports `duration` as a float
// number of seconds, so this is its wire form and not a re-encoding of it.
func ComputeRuleFingerprint(expr string, forSeconds, keepFiringForSeconds float64, labels, annotations map[string]string) RuleFingerprint {
	h := sha256.New()
	writeField(h, []byte(expr))
	writeField(h, []byte(strconv.FormatFloat(forSeconds, 'f', -1, 64)))
	writeField(h, []byte(strconv.FormatFloat(keepFiringForSeconds, 'f', -1, 64)))
	writeField(h, CanonMap(labels))
	_, _ = h.Write(CanonMap(annotations))
	return RuleFingerprint{s: hex.EncodeToString(h.Sum(nil))}
}

// String renders the rule fingerprint as 64 lowercase hex characters.
func (f RuleFingerprint) String() string { return f.s }

// IsZero reports whether the fingerprint is unset.
func (f RuleFingerprint) IsZero() bool { return f.s == "" }

// IdempotencyKey identifies one Notification exactly once (SPEC §C.7).
//
// "all_resolved at state_version 7" can exist exactly once, because the key
// hashes the subject and the version together and the DB holds
// UNIQUE (org_id, idempotency_key).
type IdempotencyKey struct{ s string }

// NewIdempotencyKey parses an idempotency key.
func NewIdempotencyKey(s string) (IdempotencyKey, error) {
	if !validate.SHA256HexRe.MatchString(s) {
		return IdempotencyKey{}, errs.Newf(errs.KindValidation, "idempotency_key",
			"idempotency_key must match %s", validate.PatternSHA256Hex)
	}
	return IdempotencyKey{s: s}, nil
}

// ComputeIdempotencyKey derives a Notification's idempotency key (SPEC §C.7):
//
//	hex( sha256( field(org_id_bytes(16)) || field(subject_kind)
//	   || field(subject_id_bytes(16)) || field(reason) || itoa(state_version) ) )
//
// where field(x) := uint32be(len(x)) || x, the framing writeField documents.
// `subject_kind` and `reason` are closed internal enums, so this key was never
// forgeable through a field's content; it is framed this way because one §C key
// with a different framing from its neighbours is a trap, not a saving.
//
// # THIS IS THE ONLY IMPLEMENTATION OF §C.7
//
// It takes strings rather than notification/domain's SubjectKind and Reason for
// the one reason the kernel takes nothing from another domain: it may not import
// one. notification/domain.IdempotencyKey is a three-line adapter that owns those
// closed enums and calls this — it is what notify.go calls, and it is why this
// function is reachable at all. Until then §C.7 had two implementations and the
// kernel's, the one a reader would assume canonical, was the dead one.
func ComputeIdempotencyKey(orgID uuid.UUID, subjectKind string, subjectID uuid.UUID, reason string, stateVersion int) IdempotencyKey {
	h := sha256.New()
	writeField(h, orgID[:])
	writeField(h, []byte(subjectKind))
	writeField(h, subjectID[:])
	writeField(h, []byte(reason))
	_, _ = h.Write([]byte(strconv.Itoa(stateVersion)))
	return IdempotencyKey{s: hex.EncodeToString(h.Sum(nil))}
}

// String renders the idempotency key.
func (k IdempotencyKey) String() string { return k.s }

// IsZero reports whether the key is unset.
func (k IdempotencyKey) IsZero() bool { return k.s == "" }

// SlackTS is a Slack message timestamp — a FOREIGN SYSTEM'S PRIMARY KEY.
//
// It is a STRING, always, never a float. Parsing it as a number loses precision
// and silently breaks every thread pointer oto owns.
type SlackTS struct{ s string }

// NewSlackTS parses a Slack timestamp.
func NewSlackTS(s string) (SlackTS, error) {
	if !validate.SlackTSRe.MatchString(s) {
		return SlackTS{}, errs.Newf(errs.KindValidation, "slack_ts",
			"slack ts must match %s", validate.PatternSlackTS)
	}
	return SlackTS{s: s}, nil
}

// String renders the timestamp verbatim, exactly as Slack sent it.
func (t SlackTS) String() string { return t.s }

// IsZero reports whether the timestamp is unset.
func (t SlackTS) IsZero() bool { return t.s == "" }

// writeField writes one length-prefixed field of a digest pre-image: a 4-byte
// big-endian byte count, then the bytes verbatim. Nothing is escaped and no byte
// is reserved, because the framing is carried by the prefix and not by the
// content. It is appendCanonField's framing (SPEC §C.1) applied one layer up, to
// the FIELDS of a §C key rather than to the names and values inside a label set.
//
// # WHY LENGTH PREFIXES AND NOT A NUL TERMINATOR
//
// A §C pre-image must be INJECTIVE for the same reason canon(labels) must be: the
// digest IS an identity. Two pre-images that collide are one AlertGroup, one rule
// content address, or one notification that a second real event can never be
// minted for.
//
// The framing this replaced was `field 0x00`, and NUL termination is injective
// only under an assumption no call site could enforce: that no field CONTAINS a
// NUL. That held for the fixed-width UUIDs, for cluster_key (whose charset
// excludes 0x00) and for the closed internal enums `subject_kind` and `reason`.
// It did NOT hold for `receiver`, which is free-form text out of the operator's
// alertmanager.yml, nor for `expr`, which is free-form PromQL out of Prometheus.
// Receiver "a" with groupLabels {b:"1"} and receiver "a\x00\x00\x00\x00\x01b…"
// with no groupLabels were one pre-image and one GroupKey.
//
// A length prefix removes the assumption instead of relying on it. Injectivity
// holds by decodability: a reader takes 4 bytes as a count n, then exactly n bytes
// as the field, and repeats — so a pre-image decodes to exactly one field tuple,
// whatever bytes the fields carry.
//
// # THE TRAILING FIELD
//
// Every §C key writes its LAST field raw, with no prefix — canon(labels) for C.2
// and C.4, canon(annotations) for C.6, itoa(state_version) for C.7. That is still
// unambiguous: it is by definition whatever remains once the prefixed fields have
// been read, so the decode above stays total. Where the tail is itself a canonical
// serialisation it is self-delimiting in turn, so the nesting introduces no second
// ambiguity: canon's own length prefixes decide where each name and value ends.
//
// The uint32 conversion is safe by construction. Every field is a 16-byte UUID, a
// bounded value object, a rendered integer, or a canonical serialisation capped by
// B6 at MaxLabelSetBytes — all far below 2^32.
func writeField(h interface{ Write([]byte) (int, error) }, b []byte) {
	var n [canonLenBytes]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(b)
}
