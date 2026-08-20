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

// GroupKey is the durable identity of an AlertGroup (SPEC §C.4).
//
// ⭐ IT IS DERIVED FROM THE ALERT'S OWN LABELS AND FROM NOTHING UPSTREAM CHOSE.
// It used to hash `(org, source_id, receiver, Alertmanager's groupLabels)`, which
// put the identity of a Slack thread under the control of a file oto can neither
// read back nor validate: editing `group_by` in `alertmanager.yml` shifted the
// whole key space, and `continue: true` gave one alert two threads at once. An
// Alertmanager group is a declared notification-BATCHING boundary, never a claim
// that its members are related — see ADR 0038.
//
// The axes are now oto's own, fixed, and stated rather than configured:
// `(org, cluster, alertname, namespace-or-∅)`. Every one of them is present on
// EVERY alert on BOTH ingest paths, which is what makes the webhook path and the
// reconciler path agree — `GET /api/v2/alerts` returns no grouping at all, so
// under the old rule reconciler-sourced groups got an empty receiver and no group
// labels (`00008_grouping.sql`) and the two paths answered "which thread does
// this belong to" differently.
type GroupKey struct{ s string }

// NewGroupKey parses a group key.
func NewGroupKey(s string) (GroupKey, error) {
	if !validate.GroupKeyRe.MatchString(s) {
		return GroupKey{}, errs.Newf(errs.KindValidation, "group_key",
			"group_key must match %s", validate.PatternGroupKey)
	}
	return GroupKey{s: s}, nil
}

// GroupSplitAxes names the label axes an AlertGroup splits on, in canonical
// order. It exists so a reader, a test and the replay harness all quote the same
// list, and so that adding an axis is one edit in one place.
//
// ⛔ IT IS NOT CONFIGURABLE AND MUST NOT BECOME SO. A tunable split key
// reinvents `group_by` inside oto and re-inherits the problem the derivation was
// built to escape; SPEC's `correlation` charter already words the requirement as
// "machine-derived groupings… with a STATED algorithm" — stated, not configured.
var GroupSplitAxes = []string{LabelAlertName, LabelNamespace}

// SplitLabels projects an Alert's label set onto the axes an AlertGroup splits
// on. It is the label half of ComputeGroupKey's pre-image, and that is now the
// WHOLE of what it is: a projection on its way into a hash.
//
// ⛔ IT IS NO LONGER "WHAT IS STORED AS `alert_groups.group_labels`" (git-bug
// `7570090`, migration 00069). The column is gone and so is the table under it, so
// nothing persists this map — it is computed, canonicalised, hashed into a GroupKey
// and discarded. A reader looking for these axes in a row is looking for a table
// that was dropped.
//
// ⭐ THE ARGUMENT THE OLD SENTENCE CARRIED IS SETTLED, AND SETTLED HARDER THAN
// STORING THE MAP EVER SETTLED IT. It said this projection being stored is "what
// makes a notification policy matching `namespace` work", because every matcher in
// oto was fed the GROUP's labels — so a matcher on `namespace` matched nothing
// unless the operator happened to put `namespace` in `group_by`, and it failed
// quietly as a `no_policy` suppression rather than as an error. A policy is now
// matched against the ALERT's own label set, which means a matcher on ANY label
// works whether or not that label is one of these four axes. The same change
// corrected `drill/domain.suppressionDetail`'s sentence from "this group's labels"
// to "this alert's", for exactly this reason.
//
// ⭐ WHAT STILL READS THE AXES is `case_policy_config`, which keys the case
// retention window on `(namespace, alertname)` so that an operator learns ONE set
// of dimensions for grouping and for retention rather than two —
// `alerts/repository/casepolicy.go` cites this function by name for that. The list
// below is therefore still a live contract; it is a contract about a KEY and a
// partition, not about a column.
//
// # WHAT IS DELIBERATELY ABSENT
//
//   - `severity` — an escalation is the same problem getting worse, and a group's
//     severity is an AGGREGATE that only means something if `warning` and
//     `critical` live in one group.
//   - `pod` / `instance` — that is the thing being grouped. Splitting on it makes
//     every alert its own group and every group its own thread.
//   - `service` — omitted until evidence says otherwise (see the replay harness in
//     `tools/groupreplay`). Adding it later SPLITS existing groups, which is the
//     safe direction; removing an axis would MERGE them, and Slack threads cannot
//     be re-parented.
//   - `cluster` — it is an axis of the key, but it is not a LABEL. It is resolved
//     from the source's configuration, participates in Alert identity (§C.2) and
//     is first-class on the group as `cluster_id`/`cluster_key`. Writing it into
//     the group's labels would invent a label the upstream never sent.
//
// # AN ABSENT NAMESPACE IS ITS OWN PARTITION, NOT AN ERROR
//
// The label is simply omitted from the result, and canon()'s length prefixes make
// the omission injective: `{alertname:X}` and `{alertname:X, namespace:""}` are
// different byte strings and would be different groups. Prometheus treats an
// empty label value as equivalent to an absent one, and `alerts.namespace` stores
// NULL for both (`nilIfEmpty`), so an EMPTY namespace is folded onto absent here
// too — the group key and the promoted column must not disagree about what ∅ is.
func SplitLabels(ls LabelSet) Labels {
	// The alertname is present and non-empty by LabelSet's own invariant, so this
	// is total: there is no label set that has no group.
	m := map[string]string{LabelAlertName: ls.AlertName()}
	if ns := ls.Namespace(); ns != "" {
		m[LabelNamespace] = ns
	}
	// Constructed directly rather than through NewLabels: this is a SUBSET of a set
	// that has already passed every B3–B6 bound, so no bound can be broken by
	// taking two of its entries, and a constructor that can fail here would force
	// every call site to handle an error no input can produce.
	return Labels{m: m}
}

// ComputeGroupKey derives the durable group identity (SPEC §C.4):
//
//	"gk_" || base32hexLower( sha256(
//	     field(org_id_bytes(16)) || field(cluster_key)
//	  || canon(SplitLabels(labels), {}) )[0:16] )
//
// where field(x) := uint32be(len(x)) || x, the framing writeField documents. The
// trailing canon() is written raw and needs no prefix: it is the remainder.
//
// ⭐ IT IS THE SAME SHAPE AS ComputeAlertKey ON PURPOSE. Both hash
// `(org, cluster_key, some canonical projection of the alert's labels)`, because
// a group is now a COARSENING of alert identity rather than a mirror of something
// upstream. That is what makes the split key IMMUTABLE for an alert's whole life:
// alert identity IS the label set, so changing any label makes a different Alert
// with its own cases, and a label-based rule can therefore never move an existing
// alert between threads. The residual risk is choosing too FINELY up front, not
// re-parenting — and re-parenting is the one thing Slack cannot do.
//
// ⚠️ DROPPING `receiver` MERGES ROUTES THAT DELIBERATELY SEPARATED THE SAME
// ALERTS. Two receivers fed by `continue: true` used to produce two groups and
// two threads for one alert; they now produce one. `cluster_key` is what must
// distinguish alerts that genuinely belong in different conversations — which it
// should be anyway, since alert identity is already `(org, cluster)`. ADR 0038
// records the trade.
//
// ⚠️ THE AXES ARE AS-YET UNVALIDATED AGAINST PRODUCTION PAYLOADS. See
// `tools/groupreplay`.
//
// ⚠️ AND `tools/groupreplay` IS NOW THE ONLY THING THAT CALLS THIS. Every other
// caller went with `alert_groups` (git-bug `7570090`): nothing in oto derives a
// group key at request time any more, because a Case is the conversation and there
// is no generation to key. That harness is not dead code — it is the instrument
// that would answer the ⚠️ above, from a corpus of stored `ingest_batches`.
//
// ⛔ DO NOT CONFUSE THIS WITH THE `group_key` INGESTION STILL HANDLES. That one is
// ALERTMANAGER'S OWN `groupKey`, a raw string off the envelope stored verbatim on
// `ingest_batches.group_key` for observability and never parsed. It is a different
// value with the same name, it needs no hash, and it does not keep this function
// alive.
//
// see that call: `tools/lintreach/main.go:484` skips every package under
// `<module>/tools/` so the gate does not gate itself. So the finding is a blind
// spot and not a fact — deleting this would break `go build ./...`. It is
// `reachable-ok` rather than `retired` for exactly that reason: `retired` would
// claim no route exists, and one does.
//
//oto:reachable-ok `tools/groupreplay/replay.go:236` calls it, and this gate cannot
func ComputeGroupKey(orgID uuid.UUID, clusterKey ClusterKey, labels LabelSet) GroupKey {
	h := sha256.New()
	writeField(h, orgID[:])
	writeField(h, []byte(clusterKey.s))
	_, _ = h.Write(SplitLabels(labels).Canonical(nil))
	return GroupKey{s: GroupKeyPrefix + encodeIdentity(h.Sum(nil))}
}

// String renders the group key.
func (k GroupKey) String() string { return k.s }

// IsZero reports whether the key is unset.
func (k GroupKey) IsZero() bool { return k.s == "" }

// RuleFingerprint is the content address of one Prometheus alerting-rule
// definition (SPEC §C.6). Rule drift — "the newest snapshot for this RuleKey has
// a different fingerprint than the one bound to the previous case" — is the
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
//	   || field(subject_id_bytes(16)) || field(reason) || itoa(state_version)
//	   || field(occasion_id_bytes(16))   -- ONLY when the occasion is non-nil ) )
//
// where field(x) := uint32be(len(x)) || x, the framing writeField documents.
// `subject_kind` and `reason` are closed internal enums, so this key was never
// forgeable through a field's content; it is framed this way because one §C key
// with a different framing from its neighbours is a trap, not a saving.
//
// # THE OCCASION, AND WHY IT IS CONDITIONAL
//
// `occasionID` is WHICH TIME THIS REASON HAPPENED, when the four columns above
// cannot tell two happenings apart. It is `uuid.Nil` for every Reason whose facts
// are already distinguished by `state_version`, and `uuid.Nil` writes NOTHING: the
// pre-image is then byte-for-byte the one this function has always computed, which
// is why no stored key moves and the golden vector in keys_test still holds.
//
// ⭐ IT EXISTS BECAUSE `state_version` IS NOT ALWAYS A DISCRIMINATOR. It is
// `alert_cases.state_version`, the CASE's optimistic lock, and it advances on a
// case STATE TRANSITION. A snooze is not one — `StartSnooze` takes an Alert, and
// §B.8 is emphatic that snooze is neither a state nor a suppression_reason — so a
// second snooze on the same alert produced a byte-identical key and
// `notifications_idem_uniq` swallowed the announcement. The operator's snooze went
// from 1h to 4h and nobody was told, which is the silence §B.6 forbids. The
// occasion is the `alert_snoozes.id` there: one key per snooze row, by
// construction, minted before the row so the row, the event, the claim and the key
// all name the same snooze.
//
// ⚠️ THE CONDITIONAL WRITE IS WHAT KEEPS THE PRE-IMAGE UNIQUELY DECODABLE, and it
// is not the tail rule being broken. §C.0's tail is `itoa(state_version)`, written
// raw because it is the remainder; anything after it must be self-delimiting from
// the digits, and `field(uuid)` is: it is a FIXED 20 bytes whose first byte is
// `0x00` (the high byte of the length 16), and a decimal string can contain no
// `0x00`. So the first `0x00` after the reason field is the boundary, always, and
// no (state_version, occasion) pair can be re-split into a different pair. This is
// the one place a §C key writes a field after its tail, and the reason it is safe
// is the reason it must stay a fixed-width uuid rather than becoming a free string.
//
// # THIS IS THE ONLY IMPLEMENTATION OF §C.7
//
// It takes strings rather than notification/domain's SubjectKind and Reason for
// the one reason the kernel takes nothing from another domain: it may not import
// one. notification/domain.IdempotencyKey is a three-line adapter that owns those
// closed enums and calls this — it is what notify.go calls, and it is why this
// function is reachable at all. Until then §C.7 had two implementations and the
// kernel's, the one a reader would assume canonical, was the dead one.
func ComputeIdempotencyKey(
	orgID uuid.UUID, subjectKind string, subjectID uuid.UUID,
	reason string, stateVersion int, occasionID uuid.UUID,
) IdempotencyKey {
	h := sha256.New()
	writeField(h, orgID[:])
	writeField(h, []byte(subjectKind))
	writeField(h, subjectID[:])
	writeField(h, []byte(reason))
	_, _ = h.Write([]byte(strconv.Itoa(stateVersion)))
	if occasionID != uuid.Nil {
		writeField(h, occasionID[:])
	}
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
// It did NOT hold for `expr`, which is free-form PromQL out of Prometheus, nor —
// while §C.4 still hashed it — for `receiver`, free-form text out of the
// operator's alertmanager.yml: receiver "a" with groupLabels {b:"1"} and receiver
// "a\x00\x00\x00\x00\x01b…" with no groupLabels were one pre-image and one
// GroupKey.
//
// `receiver` left §C.4 with ADR 0038 and no §C key hashes operator free text any
// more, but the framing STAYS. It is not a cost worth reclaiming: `expr` still
// needs it, an identity function that is injective only until someone adds a
// free-text field is a trap, and re-framing would re-key every Alert.
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
