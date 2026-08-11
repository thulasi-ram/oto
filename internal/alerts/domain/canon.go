package domain

// CanonMap is the SPEC §C.1 canonical serialisation of a label map that has NOT
// been through NewLabels — names sorted ascending by byte order, each name and
// each value written as `uint32be(len) || bytes`, nothing escaped.
//
// # WHY A LENIENT DOOR EXISTS AT ALL
//
// Every other §C key hashes labels oto itself accepted at the boundary, so every
// other pre-image is built from a constructed Labels and NewLabels is the only
// way in. §C.6 is the exception, and it is a real one rather than a shortcut: a
// `rule_fingerprint` is computed over a rule definition RECOVERED from
// Prometheus's `/api/v1/rules` or from a `generatorURL`, and those labels are
// Prometheus's data, not oto's. They have never passed NewLabels, they need not
// satisfy oto's label-name charset, and they are not stored as an Alert's labels.
//
// Refusing to canonicalise them is not available: a rule oto cannot fingerprint
// is a rule oto cannot report drift on, and drift detection is the product. So
// the kernel takes the lenient input rather than making the caller re-implement
// the format — which is exactly what `rules/domain.Canon` used to be, a second
// spelling of §C.1 that agreed with this one by luck until a cross-check test was
// written.
//
// # WHY THIS RETURNS BYTES AND NOT A Labels
//
// The obvious alternative — an unchecked `Labels` constructor — is refused
// deliberately. A Labels is the substrate of `alert_key` (§C.2) and `group_key`
// (§C.4), and a lenient constructor for it would be a hole straight through
// layer 3: an unbounded, uncharsetted label set would become an Alert identity
// with nothing between it and the digest. Bytes cannot be mistaken for a
// validated value object, and they are all §C.6 needs.
//
// The bounds NewLabels enforces (count, name charset, value length, NUL,
// serialised size) therefore do NOT apply here. That is the point, and it is
// safe because the result is hashed and discarded rather than stored: the rule's
// labels reach Postgres through `rule_snapshots.labels`, whose own DDL bounds
// them.
//
// The uint32 length conversion is inherited from appendCanonField. A rule label
// would have to exceed 4 GiB to overflow it, which the HTTP response carrying it
// cannot reach; this is the same exposure `rules/domain.Canon` has always had,
// not a new one.
//
// It MUST agree byte for byte with Labels.Canonical(nil) wherever both are
// defined — TestCanonMapAgreesWithLabelsCanonical is what keeps that true — and
// it does so by construction: it IS Labels.canonical, reached without the
// constructor.
func CanonMap(m map[string]string) []byte { return Labels{m: m}.canonical() }
