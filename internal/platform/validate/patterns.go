package validate

import "regexp"

// The canonical pattern set.
//
// SPEC §L.2.4 and R9 bind a bound to THREE places — the DTO `validate` tag, the
// domain constructor and the DDL `CHECK` — and require them to be identical.
// Every regex that has a DDL counterpart is therefore declared exactly once,
// here, as an exported constant naming the constraint it mirrors. The drift test
// (`TestValidatorMatchesDDL`) parses `db/migrations/` and compares against these
// strings byte for byte, so they must never be edited without editing the
// migration in the same commit.
//
// Layer 1 (this package) and layer 3 (`internal/<domain>/domain`) both read these
// constants. Drift between layer 1 and layer 6 is the classic way a 500 replaces
// a 422.
const (
	// PatternLabelName is a Prometheus label name (SPEC bound B9). It is NOT a
	// Kubernetes label key. Labels live in JSONB, so this one has no DDL
	// counterpart; it is the charset the ingest normaliser and the domain
	// LabelSet both enforce.
	PatternLabelName = `^[a-zA-Z_][a-zA-Z0-9_]*$`

	// PatternClusterKey mirrors clusters_key_ck. The cluster key participates in
	// Alert identity (C.2), so its charset is load-bearing.
	PatternClusterKey = `^[a-z0-9][a-z0-9._-]{0,62}$`

	// PatternOrgSlug mirrors orgs_slug_ck.
	PatternOrgSlug = `^[a-z0-9][a-z0-9-]{1,62}$`

	// PatternAlertKey mirrors alerts_key_ck: "ak_" plus 26 lowercase base32hex
	// characters over the first 128 bits of the C.2 digest.
	PatternAlertKey = `^ak_[0-9a-v]{26}$`

	// PatternGroupKey mirrors alert_groups' group key check: "gk_" plus 26
	// lowercase base32hex characters (C.4).
	PatternGroupKey = `^gk_[0-9a-v]{26}$`

	// PatternSourceFingerprint mirrors alerts_srcfp_ck: Alertmanager's FNV-1a 64
	// fingerprint rendered "%016x" (C.3).
	PatternSourceFingerprint = `^[0-9a-f]{16}$`

	// PatternSHA256Hex is a full SHA-256 digest in lowercase hex. It is the shape
	// of rule_fingerprint (C.6), notification.idempotency_key (C.7) and
	// batch_dedup_key (C.5).
	PatternSHA256Hex = `^[0-9a-f]{64}$`

	// PatternSlackTS is a Slack message timestamp — a FOREIGN SYSTEM'S PRIMARY
	// KEY. Always a string, never a float (S7).
	PatternSlackTS = `^[0-9]{10}\.[0-9]{6}$`

	// PatternSlackTeamID mirrors slack_identities_team_ck.
	PatternSlackTeamID = `^T[A-Z0-9]{2,}$`

	// PatternSlackUserID mirrors slack_identities_user_ck.
	PatternSlackUserID = `^[UW][A-Z0-9]{2,}$`

	// PatternEventType mirrors ev_type_ck: the "<subject>.<fact>" shape of the
	// closed alert_events.type enum (§D.4.1).
	PatternEventType = `^[a-z_]+\.[a-z_]+$`

	// PatternHTTPURL mirrors the regex half of alert_sources_base_ck and
	// alert_sources_prom_ck. The DDL additionally rejects a trailing slash on
	// those two columns; that is a column rule, not a URL rule.
	PatternHTTPURL = `^https?://[^[:space:]]+$`
)

// Compiled forms of the canonical patterns. Go's regexp does not understand the
// POSIX class syntax Postgres uses inside a bracket expression, so PatternHTTPURL
// is translated — the STRING is what the drift test compares, the compiled form
// is only how Go evaluates it.
var (
	// LabelNameRe evaluates PatternLabelName.
	LabelNameRe = regexp.MustCompile(PatternLabelName)
	// ClusterKeyRe evaluates PatternClusterKey.
	ClusterKeyRe = regexp.MustCompile(PatternClusterKey)
	// OrgSlugRe evaluates PatternOrgSlug.
	OrgSlugRe = regexp.MustCompile(PatternOrgSlug)
	// AlertKeyRe evaluates PatternAlertKey.
	AlertKeyRe = regexp.MustCompile(PatternAlertKey)
	// GroupKeyRe evaluates PatternGroupKey.
	GroupKeyRe = regexp.MustCompile(PatternGroupKey)
	// SourceFingerprintRe evaluates PatternSourceFingerprint.
	SourceFingerprintRe = regexp.MustCompile(PatternSourceFingerprint)
	// SHA256HexRe evaluates PatternSHA256Hex.
	SHA256HexRe = regexp.MustCompile(PatternSHA256Hex)
	// SlackTSRe evaluates PatternSlackTS.
	SlackTSRe = regexp.MustCompile(PatternSlackTS)
	// SlackTeamIDRe evaluates PatternSlackTeamID.
	SlackTeamIDRe = regexp.MustCompile(PatternSlackTeamID)
	// SlackUserIDRe evaluates PatternSlackUserID.
	SlackUserIDRe = regexp.MustCompile(PatternSlackUserID)
	// EventTypeRe evaluates PatternEventType.
	EventTypeRe = regexp.MustCompile(PatternEventType)
	// HTTPURLRe evaluates PatternHTTPURL, with Postgres's [:space:] class
	// rewritten in Go's syntax.
	HTTPURLRe = regexp.MustCompile(`^https?://[^\s]+$`)
)
