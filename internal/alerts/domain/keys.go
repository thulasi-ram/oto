package domain

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

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
//	     org_id_bytes(16) || 0x00 || cluster_key || 0x00
//	  || canon(labels, ignore) )[0:16] )
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
//	     org_id_bytes(16) || 0x00 || source_id_bytes(16) || 0x00
//	  || receiver || 0x00 || canon(groupLabels, {}) )[0:16] )
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
//	hex( sha256( expr || 0x00 || for_seconds || 0x00 || keep_firing_for_seconds
//	   || 0x00 || canon(rule_labels, {}) || 0x00 || canon(rule_annotations, {}) ) )
//
// Durations are rendered as whole seconds in base 10, matching Prometheus's own
// wire form, where `for: 10m` is the number 600.
func ComputeRuleFingerprint(expr string, forDur, keepFiringFor time.Duration, labels Labels, annotations Annotations) RuleFingerprint {
	h := sha256.New()
	writeField(h, []byte(expr))
	writeField(h, []byte(strconv.FormatInt(int64(forDur/time.Second), 10)))
	writeField(h, []byte(strconv.FormatInt(int64(keepFiringFor/time.Second), 10)))
	writeField(h, labels.Canonical(nil))
	_, _ = h.Write(annotations.Canonical())
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
//	hex( sha256( org_id_bytes(16) || 0x00 || subject_kind || 0x00
//	   || subject_id_bytes(16) || 0x00 || reason || 0x00 || itoa(state_version) ) )
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

// writeField writes one NUL-terminated field of a digest pre-image. Every §C key
// separates its fields with 0x00 so that two different field splits can never
// produce the same byte string.
func writeField(h interface{ Write([]byte) (int, error) }, b []byte) {
	_, _ = h.Write(b)
	_, _ = h.Write([]byte{0x00})
}
