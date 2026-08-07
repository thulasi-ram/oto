package domain

import (
	"cmp"
	"hash/fnv"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/validate"
)

// Cardinality bounds (SPEC §C.9.1 and §L.3.2 B3–B11). A bound declared here is
// the same bound the DTO `validate` tag and the DDL `CHECK` declare; R9 makes
// them identical by rule, and the drift test makes them identical in fact.
const (
	// MaxLabels is the number of labels one Alert may carry (B3).
	MaxLabels = 64
	// MaxLabelNameBytes bounds a label name (B4).
	MaxLabelNameBytes = 1024
	// MaxLabelValueBytes bounds a label value (B5).
	MaxLabelValueBytes = 4096
	// MaxLabelSetBytes bounds the canonical serialisation of a whole label set (B6).
	MaxLabelSetBytes = 16384
	// MaxAlertNameBytes bounds the `alertname` label (B11, alerts_name_ck).
	MaxAlertNameBytes = 1024
	// MaxAnnotations is the number of annotations one Alert may carry (B7).
	MaxAnnotations = 32
	// MaxAnnotationValueBytes bounds one annotation value (B8).
	MaxAnnotationValueBytes = 16384
)

// LabelAlertName is the label that names the alerting rule. It is required on
// every Alert label set and is never dropped by Without.
const LabelAlertName = "alertname"

// Promoted labels. These five are extracted into btree-indexed columns; every
// other label lives in JSONB behind a GIN index (SPEC §C.9.3).
const (
	// LabelSeverity carries the alert's severity class.
	LabelSeverity = "severity"
	// LabelNamespace carries the Kubernetes namespace, when there is one.
	LabelNamespace = "namespace"
	// LabelService carries the owning service.
	LabelService = "service"
)

// Canonical serialisation separators (SPEC §C.1). They are part of every identity
// key oto computes and must never change: changing one re-keys every Alert.
const (
	canonNameSep  = 0x01
	canonLabelSep = 0x02
)

// Label is one name/value pair of a label set. It is an output projection: a
// Label can only be observed by way of a Labels or LabelSet, both of which have
// already enforced the charset and the bounds.
type Label struct {
	Name  string
	Value string
}

// Labels is a bounded, name-validated set of Prometheus labels.
//
// It is the substrate of every identity key in oto: `alert_key` hashes it (C.2),
// `source_fingerprint` hashes it (C.3), `group_key` hashes an Alertmanager
// groupLabels set (C.4) and `rule_fingerprint` hashes a rule's labels (C.6).
// The map is unexported and never handed out, so a constructed Labels is
// immutable and always within bounds.
type Labels struct{ m map[string]string }

// NewLabels validates and canonicalises an arbitrary label map.
//
// Invariants: at most MaxLabels entries; every name matches the Prometheus label
// name charset; every name at most MaxLabelNameBytes and every value at most
// MaxLabelValueBytes; canonical serialisation at most MaxLabelSetBytes.
//
// The error codes are the same strings the ingest path records in
// `ingest_rejections.reason` (§L.3.2), so layer 2 can persist a rejection without
// re-deriving why.
func NewLabels(in map[string]string) (Labels, error) {
	if len(in) > MaxLabels {
		return Labels{}, errs.Newf(errs.KindValidation, "too_many_labels",
			"a label set may carry at most %d labels, got %d", MaxLabels, len(in))
	}

	m := make(map[string]string, len(in))
	size := 0
	for name, value := range in {
		if len(name) > MaxLabelNameBytes {
			return Labels{}, errs.Newf(errs.KindValidation, "label_name_too_large",
				"label name exceeds %d bytes", MaxLabelNameBytes)
		}
		if !validate.LabelNameRe.MatchString(name) {
			return Labels{}, errs.Newf(errs.KindValidation, "invalid_label_name",
				"label name %q must match %s", name, validate.PatternLabelName)
		}
		if len(value) > MaxLabelValueBytes {
			return Labels{}, errs.Newf(errs.KindValidation, "label_value_too_large",
				"label %q value exceeds %d bytes", name, MaxLabelValueBytes)
		}
		size += len(name) + len(value) + 2 // the two canonical separators
		m[name] = value
	}
	if size > MaxLabelSetBytes {
		return Labels{}, errs.Newf(errs.KindValidation, "labelset_too_large",
			"serialised label set exceeds %d bytes", MaxLabelSetBytes)
	}
	return Labels{m: m}, nil
}

// Len is the number of labels.
func (l Labels) Len() int { return len(l.m) }

// IsZero reports whether this is the zero Labels, which carries nothing.
func (l Labels) IsZero() bool { return len(l.m) == 0 }

// Get returns the value of one label.
func (l Labels) Get(name string) (string, bool) {
	v, ok := l.m[name]
	return v, ok
}

// Names returns the label names in ascending byte order.
func (l Labels) Names() []string {
	names := slices.Collect(maps.Keys(l.m))
	slices.Sort(names)
	return names
}

// Sorted returns every label in ascending byte order of name. This deterministic
// order is the input to every hash oto computes (SPEC §C.1).
func (l Labels) Sorted() []Label {
	out := make([]Label, 0, len(l.m))
	for name, value := range l.m {
		out = append(out, Label{Name: name, Value: value})
	}
	slices.SortFunc(out, func(a, b Label) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// Map returns a copy of the labels. It is a copy on purpose: the caller may do
// what it likes with it without reaching back into the value object.
func (l Labels) Map() map[string]string { return maps.Clone(l.m) }

// Without returns the labels with the named ones removed. Removing labels can
// never break an invariant, so this is total.
func (l Labels) Without(names []string) Labels {
	if len(names) == 0 || len(l.m) == 0 {
		return l
	}
	drop := make(map[string]struct{}, len(names))
	for _, n := range names {
		drop[n] = struct{}{}
	}
	m := make(map[string]string, len(l.m))
	for name, value := range l.m {
		if _, skip := drop[name]; skip {
			continue
		}
		m[name] = value
	}
	return Labels{m: m}
}

// Canonical renders the canonical serialisation of SPEC §C.1:
//
//	for each (name, value) sorted by name ASC in byte order, name NOT in ignore:
//	    write(name); write(0x01); write(value); write(0x02)
//
// Names and values are used verbatim: UTF-8, no case folding. This byte string is
// hashed by alert_key, group_key and rule_fingerprint, so it is frozen.
func (l Labels) Canonical(ignore []string) []byte {
	return l.Without(ignore).canonical()
}

func (l Labels) canonical() []byte {
	sorted := l.Sorted()
	size := 0
	for _, lb := range sorted {
		size += len(lb.Name) + len(lb.Value) + 2
	}
	buf := make([]byte, 0, size)
	for _, lb := range sorted {
		buf = append(buf, lb.Name...)
		buf = append(buf, canonNameSep)
		buf = append(buf, lb.Value...)
		buf = append(buf, canonLabelSep)
	}
	return buf
}

// SerialisedSize is the length of the canonical serialisation over the whole set.
// It is the quantity bound B6 caps.
func (l Labels) SerialisedSize() int {
	size := 0
	for name, value := range l.m {
		size += len(name) + len(value) + 2
	}
	return size
}

// Fingerprint recomputes Alertmanager's own fingerprint locally (SPEC §C.3).
//
// It reproduces prometheus/common/model.LabelSet.Fingerprint().String() exactly:
// FNV-1a 64 over the labels sorted by name, writing name || 0xFF || value || 0xFF
// for each, rendered "%016x". It is computed over the FULL label set — nothing is
// ignored — and is the join key for API v2 reconciliation, never the product
// identity (C10). oto recomputes rather than trusting the wire value; a mismatch
// is recorded, never fatal.
func (l Labels) Fingerprint() SourceFingerprint {
	h := fnv.New64a()
	for _, lb := range l.Sorted() {
		_, _ = h.Write([]byte(lb.Name))
		_, _ = h.Write([]byte{fingerprintSep})
		_, _ = h.Write([]byte(lb.Value))
		_, _ = h.Write([]byte{fingerprintSep})
	}
	s := strconv.FormatUint(h.Sum64(), 16)
	return SourceFingerprint{s: strings.Repeat("0", fingerprintHexLen-len(s)) + s}
}

// fingerprintSep is prometheus/common/model.SeparatorByte.
const fingerprintSep = 0xFF

// LabelSet is the identity-bearing label set of an Alert.
//
// It is Labels that additionally carry a non-empty `alertname` — the one label
// oto refuses to live without, because it is what a human recognises the Alert by
// and what every rule lookup keys on. Two label sets that differ only in an
// ignored label are the SAME Alert (C.2); that is decided by Canonical, not here.
type LabelSet struct{ l Labels }

// NewLabelSet validates an Alert's label set: every Labels invariant, plus a
// present, non-empty `alertname` of at most MaxAlertNameBytes.
func NewLabelSet(in map[string]string) (LabelSet, error) {
	l, err := NewLabels(in)
	if err != nil {
		return LabelSet{}, err
	}
	name, ok := l.Get(LabelAlertName)
	if !ok || strings.TrimSpace(name) == "" {
		return LabelSet{}, errs.New(errs.KindValidation, "missing_alertname",
			"a label set must carry a non-empty alertname")
	}
	if len(name) > MaxAlertNameBytes {
		return LabelSet{}, errs.Newf(errs.KindValidation, "label_value_too_large",
			"alertname exceeds %d bytes", MaxAlertNameBytes)
	}
	return LabelSet{l: l}, nil
}

// AlertName is the `alertname` label. It is always present and non-empty.
func (s LabelSet) AlertName() string {
	name, _ := s.l.Get(LabelAlertName)
	return name
}

// Labels exposes the underlying bounded label set.
func (s LabelSet) Labels() Labels { return s.l }

// Len is the number of labels.
func (s LabelSet) Len() int { return s.l.Len() }

// IsZero reports whether this is the zero LabelSet.
func (s LabelSet) IsZero() bool { return s.l.IsZero() }

// Get returns the value of one label.
func (s LabelSet) Get(name string) (string, bool) { return s.l.Get(name) }

// Sorted returns every label in ascending byte order of name.
func (s LabelSet) Sorted() []Label { return s.l.Sorted() }

// Map returns a copy of the labels.
func (s LabelSet) Map() map[string]string { return s.l.Map() }

// Without returns the label set with the named labels removed. `alertname` is
// never removed: it is identity-bearing, and a LabelSet without one cannot exist.
func (s LabelSet) Without(names []string) LabelSet {
	keep := make([]string, 0, len(names))
	for _, n := range names {
		if n != LabelAlertName {
			keep = append(keep, n)
		}
	}
	return LabelSet{l: s.l.Without(keep)}
}

// Canonical renders the SPEC §C.1 serialisation, skipping the ignored names.
func (s LabelSet) Canonical(ignore []string) []byte { return s.Without(ignore).l.canonical() }

// SerialisedSize is the length of the canonical serialisation of the whole set.
func (s LabelSet) SerialisedSize() int { return s.l.SerialisedSize() }

// Fingerprint recomputes Alertmanager's fingerprint over the FULL label set (C.3).
func (s LabelSet) Fingerprint() SourceFingerprint { return s.l.Fingerprint() }

// Severity reads the promoted `severity` label, mapped onto the closed class set.
// An absent or unrecognised label is SeverityUnknown, never an error: an upstream
// label is untrusted input and rejecting it would lose the alert (§L.3).
func (s LabelSet) Severity() Severity { return SeverityFromLabel(mustGet(s, LabelSeverity)) }

// Namespace reads the promoted `namespace` label.
func (s LabelSet) Namespace() string { return mustGet(s, LabelNamespace) }

// Service reads the promoted `service` label.
func (s LabelSet) Service() string { return mustGet(s, LabelService) }

func mustGet(s LabelSet, name string) string {
	v, _ := s.Get(name)
	return v
}

// Annotations is an Alert's bounded annotation map.
//
// Annotations are display text — summary, description, runbook_url — and are
// deliberately NOT part of any identity: an operator rewording a description must
// not create a new Alert. They are bounded because a hostile or broken upstream
// would otherwise write unbounded bytes into oto's hot path (§L.3.2 B7–B8).
type Annotations struct{ m map[string]string }

// Annotation names oto reads by name.
const (
	// AnnotationSummary is the one-line human summary rendered in the timeline.
	AnnotationSummary = "summary"
	// AnnotationDescription is the long-form human description.
	AnnotationDescription = "description"
	// AnnotationRunbookURL is the runbook link the `runbook` Enricher resolves.
	AnnotationRunbookURL = "runbook_url"
)

// NewAnnotations validates an annotation map: at most MaxAnnotations entries,
// every name at most MaxLabelNameBytes, every value at most
// MaxAnnotationValueBytes.
//
// The name charset is deliberately NOT constrained. §L.3.2 bounds annotation
// count and value length and nothing else, and Grafana's Unified Alerting
// superset is free to send names oto has never seen.
func NewAnnotations(in map[string]string) (Annotations, error) {
	if len(in) > MaxAnnotations {
		return Annotations{}, errs.Newf(errs.KindValidation, "too_many_annotations",
			"an alert may carry at most %d annotations, got %d", MaxAnnotations, len(in))
	}
	m := make(map[string]string, len(in))
	for name, value := range in {
		if len(name) > MaxLabelNameBytes {
			return Annotations{}, errs.Newf(errs.KindValidation, "annotation_name_too_large",
				"annotation name exceeds %d bytes", MaxLabelNameBytes)
		}
		if len(value) > MaxAnnotationValueBytes {
			return Annotations{}, errs.Newf(errs.KindValidation, "annotation_too_large",
				"annotation %q exceeds %d bytes", name, MaxAnnotationValueBytes)
		}
		m[name] = value
	}
	return Annotations{m: m}, nil
}

// Len is the number of annotations.
func (a Annotations) Len() int { return len(a.m) }

// Get returns one annotation.
func (a Annotations) Get(name string) (string, bool) {
	v, ok := a.m[name]
	return v, ok
}

// Map returns a copy of the annotations.
func (a Annotations) Map() map[string]string { return maps.Clone(a.m) }

// Sorted returns every annotation in ascending byte order of name — the order
// rule_fingerprint hashes them in (C.6).
func (a Annotations) Sorted() []Label {
	out := make([]Label, 0, len(a.m))
	for name, value := range a.m {
		out = append(out, Label{Name: name, Value: value})
	}
	slices.SortFunc(out, func(x, y Label) int { return cmp.Compare(x.Name, y.Name) })
	return out
}

// Canonical renders annotations in the SPEC §C.1 serialisation, which is how
// rule_fingerprint content-addresses a rule's annotations (C.6).
func (a Annotations) Canonical() []byte {
	sorted := a.Sorted()
	buf := make([]byte, 0, 64)
	for _, an := range sorted {
		buf = append(buf, an.Name...)
		buf = append(buf, canonNameSep)
		buf = append(buf, an.Value...)
		buf = append(buf, canonLabelSep)
	}
	return buf
}

// Equal reports whether two annotation sets carry exactly the same pairs. A
// change here is one of the material changes that makes a repeat observation emit
// `alert.mutated` rather than nothing (§B.3 T2).
func (a Annotations) Equal(other Annotations) bool { return maps.Equal(a.m, other.m) }
