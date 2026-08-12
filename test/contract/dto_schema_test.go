package contract

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	alertsapi "github.com/thulasiram/oto/internal/alerts/api"
	channelsapi "github.com/thulasiram/oto/internal/channels/api"
	drillapi "github.com/thulasiram/oto/internal/drill/api"
	enrichmentapi "github.com/thulasiram/oto/internal/enrichment/api"
	groupingapi "github.com/thulasiram/oto/internal/grouping/api"
	identityapi "github.com/thulasiram/oto/internal/identity/api"
	ingestionapi "github.com/thulasiram/oto/internal/ingestion/api"
	notificationapi "github.com/thulasiram/oto/internal/notification/api"
	rulesapi "github.com/thulasiram/oto/internal/rules/api"
	silencesapi "github.com/thulasiram/oto/internal/silences/api"
	sourcesapi "github.com/thulasiram/oto/internal/sources/api"
	statsapi "github.com/thulasiram/oto/internal/stats/api"
	streamingapi "github.com/thulasiram/oto/internal/streaming/api"
)

// ---------------------------------------------------------------------------
// Gate G1 of SPEC §L.8.1 — Go DTO → OpenAPI.
//
// The eight contract divergences the phase-4 audit found were all found by a
// human reading `api/openapi/openapi.yaml` beside `internal/*/api/dto.go`. This
// file is that human, mechanised: it REFLECTS over the real structs — not over
// a list of field names somebody maintains — and asserts that for every DTO
//
//	1. a component schema of the same name exists,
//	2. neither side has a property the other lacks,
//	3. each property's OpenAPI type is the one the Go type actually marshals to,
//	4. required-ness matches `omitempty` exactly, and
//	5. a `*T` field is declared nullable and a `T` field is not.
//
// The list below binds a Go type to a schema name. It is NOT a list of fields —
// Go reflection cannot enumerate the types in a package at run time, so the
// types must be named once. `TestEveryAPIStructIsBound` closes that loophole by
// PARSING `internal/*/api` and failing on any `*DTO`/`*Request`/`*Query` struct
// that is neither bound here nor listed, with a written reason, in
// `notInContract`. Adding a DTO and forgetting this file is therefore a
// failure, not a silent gap.
// ---------------------------------------------------------------------------

// binding ties one Go struct to one `components.schemas` entry.
type binding struct {
	// pkg is the internal domain the struct lives in, e.g. "alerts".
	pkg string
	// schema is the component schema name. It differs from the Go type name in
	// a handful of places and each difference is called out at the binding.
	schema string
	// v is a zero value of the struct; only its type is used.
	v any
}

// bindings is the DTO ↔ schema map.
//
// Several schemas are bound MORE THAN ONCE, because more than one domain
// declares its own Go struct for the same wire shape (`AlertDTO` in both
// `alerts` and `grouping`, `SnoozeRequest` in both, and so on). Every copy is
// checked against the one contract, which also makes the copies agree with each
// other — the failure mode this catches is a field added to one and not the
// twin.
var bindings = []binding{
	// ------------------------------------------------------------- alerts
	{"alerts", "AckRequest", alertsapi.AckRequest{}},
	{"alerts", "UnackRequest", alertsapi.UnackRequest{}},
	{"alerts", "CommentRequest", alertsapi.CommentRequest{}},
	{"alerts", "SnoozeRequest", alertsapi.SnoozeRequest{}},
	{"alerts", "UnsnoozeRequest", alertsapi.UnsnoozeRequest{}},
	{"alerts", "AlertDTO", alertsapi.AlertDTO{}},
	{"alerts", "AlertDetailDTO", alertsapi.AlertDetailDTO{}},
	{"alerts", "AlertRefDTO", alertsapi.AlertRefDTO{}},
	{"alerts", "AlertEventDTO", alertsapi.AlertEventDTO{}},
	{"alerts", "AlertRollupDTO", alertsapi.AlertRollupDTO{}},
	{"alerts", "OccurrenceDTO", alertsapi.OccurrenceDTO{}},
	{"alerts", "OccurrenceDetailDTO", alertsapi.OccurrenceDetailDTO{}},
	{"alerts", "EnrichmentDTO", alertsapi.EnrichmentDTO{}},
	{"alerts", "EnrichmentSummaryDTO", alertsapi.EnrichmentSummaryDTO{}},
	{"alerts", "DeliverySummaryDTO", alertsapi.DeliverySummaryDTO{}},
	{"alerts", "NotificationDTO", alertsapi.NotificationDTO{}},
	{"alerts", "SnoozeDTO", alertsapi.SnoozeDTO{}},
	{"alerts", "SnoozeHistoryDTO", alertsapi.SnoozeHistoryDTO{}},
	{"alerts", "ActiveSnoozeDTO", alertsapi.ActiveSnoozeDTO{}},
	{"alerts", "LabelNameDTO", alertsapi.LabelNameDTO{}},
	{"alerts", "LabelValueDTO", alertsapi.LabelValueDTO{}},

	// ----------------------------------------------------------- grouping
	{"grouping", "GroupDTO", groupingapi.GroupDTO{}},
	{"grouping", "GroupDetailDTO", groupingapi.GroupDetailDTO{}},
	{"grouping", "AlertDTO", groupingapi.AlertDTO{}},
	{"grouping", "AlertRefDTO", groupingapi.AlertRefDTO{}},
	{"grouping", "AlertEventDTO", groupingapi.AlertEventDTO{}},
	{"grouping", "DeliverySummaryDTO", groupingapi.DeliverySummaryDTO{}},
	{"grouping", "SnoozeDTO", groupingapi.SnoozeDTO{}},
	{"grouping", "AckRequest", groupingapi.AckRequest{}},
	{"grouping", "CommentRequest", groupingapi.CommentRequest{}},
	{"grouping", "SnoozeRequest", groupingapi.SnoozeRequest{}},
	{"grouping", "UnsnoozeRequest", groupingapi.UnsnoozeRequest{}},

	// ------------------------------------------------------------ sources
	{"sources", "ClusterDTO", sourcesapi.ClusterDTO{}},
	{"sources", "CreateClusterRequest", sourcesapi.CreateClusterRequest{}},
	{"sources", "UpdateClusterRequest", sourcesapi.UpdateClusterRequest{}},
	{"sources", "SourceDTO", sourcesapi.SourceDTO{}},
	{"sources", "SourceCreatedDTO", sourcesapi.SourceCreatedDTO{}},
	{"sources", "SourceHealthDTO", sourcesapi.SourceHealthDTO{}},
	{"sources", "SourceTestDTO", sourcesapi.SourceTestDTO{}},
	{"sources", "ReconcileResultDTO", sourcesapi.ReconcileResultDTO{}},
	{"sources", "CreateSourceRequest", sourcesapi.CreateSourceRequest{}},
	{"sources", "UpdateSourceRequest", sourcesapi.UpdateSourceRequest{}},
	{"sources", "RouteTimingsDTO", sourcesapi.RouteTimingsDTO{}},
	{"sources", "RouteTimingDTO", sourcesapi.RouteTimingDTO{}},
	{"sources", "RouteStepDTO", sourcesapi.RouteStepDTO{}},
	{"sources", "ReceiverRouteDTO", sourcesapi.ReceiverRouteDTO{}},
	{"sources", "InheritedTimingDTO", sourcesapi.InheritedTimingDTO{}},
	// The contract spells this one without the `DTO` suffix.
	{"sources", "CredentialInput", sourcesapi.CredentialInputDTO{}},

	// ----------------------------------------------------------- channels
	{"channels", "ChannelDTO", channelsapi.ChannelDTO{}},
	{"channels", "ChannelTypeDTO", channelsapi.ChannelTypeDTO{}},
	{"channels", "ChannelTestDTO", channelsapi.ChannelTestDTO{}},
	{"channels", "CreateChannelRequest", channelsapi.CreateChannelRequest{}},
	{"channels", "UpdateChannelRequest", channelsapi.UpdateChannelRequest{}},
	{"channels", "CredentialInput", channelsapi.CredentialInputDTO{}},

	// ------------------------------------------------------- notification
	{"notification", "PolicyDTO", notificationapi.PolicyDTO{}},
	{"notification", "CreatePolicyRequest", notificationapi.CreatePolicyRequest{}},
	{"notification", "UpdatePolicyRequest", notificationapi.UpdatePolicyRequest{}},
	{"notification", "MatcherDTO", notificationapi.MatcherDTO{}},
	{"notification", "ThrottleDTO", notificationapi.ThrottleDTO{}},
	{"notification", "NotificationDTO", notificationapi.NotificationDTO{}},
	{"notification", "NotificationDetailDTO", notificationapi.NotificationDetailDTO{}},
	{"notification", "DeliveryDTO", notificationapi.DeliveryDTO{}},
	{"notification", "DeliveryDetailDTO", notificationapi.DeliveryDetailDTO{}},
	{"notification", "DeliverySummaryDTO", notificationapi.DeliverySummaryDTO{}},
	{"notification", "PolicyPreviewRequest", notificationapi.PolicyPreviewRequest{}},
	{"notification", "PolicyPreviewDTO", notificationapi.PolicyPreviewDTO{}},

	// ----------------------------------------------------------- identity
	{"identity", "MeDTO", identityapi.MeDTO{}},
	{"identity", "UserDTO", identityapi.UserDTO{}},
	{"identity", "OrgDTO", identityapi.OrgDTO{}},
	{"identity", "OrgSettingsDTO", identityapi.OrgSettingsDTO{}},
	{"identity", "OrgSettingsViewDTO", identityapi.OrgSettingsViewDTO{}},
	{"identity", "OrgSettingsPatchDTO", identityapi.OrgSettingsPatchDTO{}},
	{"identity", "SettingBoundDTO", identityapi.SettingBoundDTO{}},
	{"identity", "UpdateOrgSettingsRequest", identityapi.UpdateOrgSettingsRequest{}},
	{"identity", "LoginRequest", identityapi.LoginRequest{}},
	{"identity", "CreateTokenRequest", identityapi.CreateTokenRequest{}},
	// The contract spells the initialism `Api`, the Go style guide spells it
	// `API`; the JSON is identical and the difference is only in the type name.
	{"identity", "ApiTokenDTO", identityapi.APITokenDTO{}},
	{"identity", "ApiTokenCreatedDTO", identityapi.APITokenCreatedDTO{}},

	// ------------------------------------------------------------ silences
	{"silences", "SilenceDTO", silencesapi.SilenceDTO{}},
	{"silences", "SilenceDetailDTO", silencesapi.SilenceDetailDTO{}},
	{"silences", "SilenceMatcherDTO", silencesapi.SilenceMatcherDTO{}},
	{"silences", "AlertRefDTO", silencesapi.AlertRefDTO{}},

	// --------------------------------------------------------------- rules
	{"rules", "RuleSnapshotDTO", rulesapi.RuleSnapshotDTO{}},
	{"rules", "RuleHistoryDTO", rulesapi.RuleHistoryDTO{}},
	{"rules", "RuleChangeDTO", rulesapi.RuleChangeDTO{}},
	// `RuleExprDiffDTO` is a oneOf of three verdict variants and one Go struct
	// produces all three, so there is no schema of that name to diff against.
	// It is bound to the WIDEST branch — `numbers_moved` is the only one that
	// carries `numbers`, so it is the only branch that names every key the Go
	// struct can emit. Binding to a narrower branch would report `numbers` as a
	// property the contract does not declare, which is the union's whole point
	// and not a divergence. The narrowing itself — that `numbers` may travel
	// under `numbers_moved` and nothing else — is enforced by the contract on
	// the client and by `changeDTO` on the server; G2 sees the real bytes.
	{"rules", "RuleExprNumbersMovedDTO", rulesapi.RuleExprDiffDTO{}},
	{"rules", "RuleExprNumberChangeDTO", rulesapi.RuleExprNumberChangeDTO{}},

	// --------------------------------------------------------------- stats
	{"stats", "StatsOverviewDTO", statsapi.StatsOverviewDTO{}},
	{"stats", "AlertQualityDTO", statsapi.AlertQualityDTO{}},

	// ----------------------------------------------------------- ingestion
	{"ingestion", "IngestAcceptedDTO", ingestionapi.IngestAcceptedDTO{}},

	// -------------------------------------------------------- enrichment
	{"enrichment", "EnricherDTO", enrichmentapi.EnricherDTO{}},

	// ------------------------------------------------------------- drill
	// The contract prefixes this family `Delivery`, because "drill" alone is
	// ambiguous in an alerting product.
	{"drill", "DeliveryDrillDTO", drillapi.DrillDTO{}},
	{"drill", "DrillStageDTO", drillapi.DrillStageDTO{}},
	{"drill", "DrillDestinationDTO", drillapi.DrillDestinationDTO{}},
	{"drill", "StartDrillRequest", drillapi.StartDrillRequest{}},

	// ---------------------------------------------------------- streaming
	{"streaming", "StreamFrame", streamingapi.StreamFrameDTO{}},
}

// queryObjects lists the `*Query` structs, which are bound to operation
// PARAMETERS rather than to a schema and are checked by
// TestQueryParamsMatchContract. Membership here is verified: an entry that
// queryBindings does not cover is a failure.
var queryObjects = map[string]bool{
	"alerts.ListAlertsQuery":     true,
	"alerts.ListRollupsQuery":    true,
	"alerts.TimelineQuery":       true,
	"alerts.LabelQuery":          true,
	"grouping.ListGroupsQuery":   true,
	"grouping.TimelineQuery":     true,
	"silences.ListSilencesQuery": true,
	"rules.ListSnapshotsQuery":   true,
	"rules.HistoryQuery":         true,
	"rules.BatchSnapshotsQuery":  true,
	"stats.OverviewQuery":        true,
	"stats.AlertQualityQuery":    true,
	"identity.PageQuery":         true,
}

// nestedOnly lists the DTOs the contract inlines inside a parent schema instead
// of naming in `components.schemas`. They ARE checked by G1 — the comparer
// recurses into them — but they have no schema of their own to bind to.
//
// The claim "it is reached from a bound parent" is not taken on trust: the
// comparer records every Go type it walks, and an entry here that the walk did
// not reach is a failure. Otherwise this map would be the escape hatch that
// quietly empties the gate.
var nestedOnly = map[string]string{
	"alerts.SuppressedByDTO":              "inlined in OccurrenceDTO.suppressed_by",
	"notification.PolicyPreviewResultDTO": "inlined in PolicyPreviewDTO",
	"rules.RuleKeyDTO":                    "inlined in the rules snapshot schemas",
	"sources.HealthWarningDTO":            "inlined in SourceHealthDTO.warnings[]",
	"stats.AlertStateCountsDTO":           "inlined in StatsOverviewDTO.alerts",
	"stats.GroupCountsDTO":                "inlined in StatsOverviewDTO.groups",
	"stats.DeliveryCountsDTO":             "inlined in StatsOverviewDTO.deliveries",
	"stats.SourceCountsDTO":               "inlined in StatsOverviewDTO.sources",
	"stats.ChannelCountsDTO":              "inlined in StatsOverviewDTO.channels",
	"stats.WindowDTO":                     "inlined in StatsOverviewDTO.window",
}

// unbuiltProperties is the known debt of gate G1, in the shape this repository
// already uses for `tools/lintvocab/baseline.txt` and
// `tools/lintreach/baseline.txt`: a written, shrink-only list of divergences
// that exist at the moment the gate was built.
//
// Every line is a property the CONTRACT declares and no Go field produces — a
// promise the server does not keep. None of them is required, so no client can
// be relying on it today; all of them are reported in full in the issue that
// created this gate. They are listed rather than deleted from `openapi.yaml`
// because removing a promised expansion is a product decision, not a test's.
//
// The list can only shrink. An entry that stops matching FAILS, so paying the
// debt is enforced and a stale exemption cannot quietly cover the next hole.
//
// The key is `<domain>.<GoType>.<property>`, because two domains declare their
// own Go struct for the same schema and only one of them may be short.
var unbuiltProperties = map[string]string{
	// The `include=`-style expansions that were specified and never built. Each
	// is `X | null` in the contract and no mapper ever sets it.
	"alerts.AlertDetailDTO.source":                "SourceRefDTO expansion never built",
	"alerts.AlertDetailDTO.group":                 "GroupRefDTO expansion never built",
	"alerts.OccurrenceDetailDTO.group":            "GroupRefDTO expansion never built",
	"alerts.OccurrenceDetailDTO.rule":             "rule expansion never built",
	"grouping.GroupDetailDTO.source":              "SourceRefDTO expansion never built",
	"silences.SilenceDetailDTO.source":            "SourceRefDTO expansion never built",
	"notification.NotificationDetailDTO.alert":    "AlertRefDTO expansion never built",
	"notification.NotificationDetailDTO.group":    "GroupRefDTO expansion never built",
	"notification.DeliveryDetailDTO.notification": "NotificationDTO expansion never built",

	// `listAlertGroupAlerts` serves the same `AlertDTO` schema from its own,
	// shorter Go struct. These three are `include=`-gated on the alerts list and
	// that endpoint has no `include=`, so their absence is correct there.
	"grouping.AlertDTO.current_occurrence": "listAlertGroupAlerts has no include= parameter",
	"grouping.AlertDTO.enrichments":        "listAlertGroupAlerts has no include= parameter",
	"grouping.AlertDTO.rule":               "listAlertGroupAlerts has no include= parameter",
}

// unenforceableRequired lists REQUEST properties the contract marks `required`
// and that no `validate` tag can enforce, because the Go field is a non-pointer
// scalar: `encoding/json` decodes an absent key and an explicit zero value into
// the same bytes, so `validate:"required"` would refuse a LEGAL value as the
// price of refusing an absent one.
//
// It is the same shape as `unbuiltProperties` — written, keyed
// `<domain>.<GoType>.<property>`, and verified: an entry the walk does not find
// divergent FAILS, so a tag added later cannot leave a stale exemption behind.
//
// ⛔ IT IS NOT A PLACE TO PUT A FORGOTTEN `required`. It is only for a property
// whose ZERO VALUE IS ITSELF MEANINGFUL, which is the entire reason the tag
// cannot be added. Each entry has to say what that meaning is.
var unenforceableRequired = map[string]string{
	// `severity=""` is a legal Alertmanager matcher and means "the alert carries
	// no severity label" — domain.Matcher.Matches reads a missing label as the
	// empty string on purpose, and the policy editor's matcher box can express
	// it. `validate:"required"` would refuse it, so the server cannot tell an
	// omitted `value` from an explicit empty one, and the contract's `required`
	// stands as the obligation on the CLIENT that it is: every generated client
	// sends the key.
	"notification.MatcherDTO.value": "the empty string is a legal matcher value, " +
		"so `required` would outlaw it to catch an absent key it cannot see",
}

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

// TestDTOSchemaDrift is gate G1 (SPEC §L.8.1, §L.10). It is the test the SPEC
// names `TestDTOSchemaDrift` and the file it names `dto_schema_test.go`.
func TestDTOSchemaDrift(t *testing.T) {
	f, _ := runG1(t)
	f.report(t, "gate G1: Go DTOs and api/openapi/openapi.yaml disagree")
}

// runG1 walks every binding and returns both the divergences and the set of Go
// types the walk actually reached, as `<pkg>.<TypeName>`.
func runG1(t *testing.T) (*failures, map[string]bool) {
	t.Helper()
	d := loadDoc(t)
	f := &failures{}
	visited := map[string]bool{}
	unbuiltSeen := map[string]bool{}
	unenforceableSeen := map[string]bool{}

	for _, b := range bindings {
		if _, ok := d.schema(b.schema); !ok {
			f.addf("%s.%s: no `components.schemas.%s` in the contract",
				b.pkg, reflect.TypeOf(b.v).Name(), b.schema)
			continue
		}
		c := &comparer{doc: d, f: f, seen: map[string]bool{}, visited: visited,
			unbuiltSeen: unbuiltSeen, unenforceableSeen: unenforceableSeen,
			request: isRequestSchema(b.schema, reflect.TypeOf(b.v).Name())}
		// Entered through a `$ref` rather than through the resolved node, so
		// that `flat.origin` — and therefore every message — names the schema.
		c.compare(b.pkg+"."+b.schema, reflect.TypeOf(b.v),
			map[string]any{"$ref": "#/components/schemas/" + b.schema})
	}
	for key := range unbuiltProperties {
		if !unbuiltSeen[key] {
			f.addf("unbuiltProperties lists %s, but the walk did not find it missing — "+
				"either it is now built, or the contract dropped it. A stale exemption is "+
				"the next hole", key)
		}
	}
	for key := range unenforceableRequired {
		if !unenforceableSeen[key] {
			f.addf("unenforceableRequired lists %s, but the walk found nothing to excuse — "+
				"either the Go field now validates it, or the contract stopped requiring it, "+
				"and in both cases the exemption is now a hole", key)
		}
	}
	return f, visited
}

// isRequestSchema reports whether a binding describes a body the server
// DECODES rather than one it encodes. `*Request` covers all but one; the
// exception is `OrgSettingsPatchDTO`, a PATCH body whose name predates the
// convention.
func isRequestSchema(schema, goName string) bool {
	return strings.HasSuffix(schema, "Request") ||
		strings.HasSuffix(goName, "Request") ||
		goName == "OrgSettingsPatchDTO" ||
		goName == "CredentialInputDTO"
}

// domainKey renders a reflected type as `<domain>.<TypeName>`, which is how
// every list in this file names a struct. Every api package is literally
// `package api`, so `reflect.Type.String()` alone cannot tell `alerts.AlertDTO`
// from `grouping.AlertDTO`.
func domainKey(t reflect.Type) string {
	path := t.PkgPath()
	path = strings.TrimSuffix(path, "/api")
	i := strings.LastIndex(path, "/")
	return path[i+1:] + "." + t.Name()
}

// TestEveryAPIStructIsBound parses `internal/*/api` and fails on any exported
// `*DTO`, `*Request` or `*Query` struct that TestDTOSchemaDrift does not check.
// Without this, G1 degrades into exactly the hand-maintained list it replaces:
// a new DTO would simply not be covered and nothing would say so.
func TestEveryAPIStructIsBound(t *testing.T) {
	bound := map[string]bool{}
	for _, b := range bindings {
		bound[b.pkg+"."+reflect.TypeOf(b.v).Name()] = true
	}
	for _, qb := range queryBindings {
		bound[qb.pkg+"."+reflect.TypeOf(qb.v).Name()] = true
	}
	_, visited := runG1(t)

	root := moduleRoot(t)
	dirs, err := filepath.Glob(filepath.Join(root, "internal", "*", "api"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(dirs) == 0 {
		t.Fatal("found no internal/*/api packages — the scan is looking in the wrong place")
	}

	var f failures
	used := map[string]bool{}
	for _, dir := range dirs {
		pkg := filepath.Base(filepath.Dir(dir))
		for _, name := range apiStructNames(t, dir) {
			key := pkg + "." + name
			if bound[key] {
				used[key] = true
				continue
			}
			if queryObjects[key] {
				used[key] = true
				continue
			}
			if why, ok := nestedOnly[key]; ok {
				used[key] = true
				if !visited[key] {
					f.addf("nestedOnly claims %s is %q, but the G1 walk never reached that "+
						"Go type — either the contract stopped inlining it or nothing "+
						"references it, and in both cases the exemption is now a hole", key, why)
				}
				continue
			}
			f.addf("%s is an exported %s struct in internal/%s/api with no binding in "+
				"test/contract/dto_schema_test.go and no entry in queryObjects or "+
				"nestedOnly — gate G1 does not check it", key, suffixOf(name), pkg)
		}
	}
	for key := range nestedOnly {
		if !used[key] {
			f.addf("nestedOnly lists %s, which no longer exists — a stale exemption "+
				"hides the next struct that takes its name", key)
		}
	}
	for key := range queryObjects {
		if !used[key] {
			f.addf("queryObjects lists %s, which no longer exists", key)
		}
		if !bound[key] {
			f.addf("queryObjects lists %s but queryBindings does not bind it to an "+
				"operation, so no parameter is ever compared for it", key)
		}
	}
	for _, b := range bindings {
		key := b.pkg + "." + reflect.TypeOf(b.v).Name()
		if !used[key] {
			f.addf("bindings names %s, which the source scan did not find", key)
		}
	}

	f.report(t, "gate G1: coverage")
}

func suffixOf(name string) string {
	for _, s := range []string{"DTO", "Request", "Query"} {
		if strings.HasSuffix(name, s) {
			return s
		}
	}
	return "API"
}

// apiStructNames returns the exported struct type names in one package that end
// in DTO, Request or Query, excluding test files.
func apiStructNames(t *testing.T, dir string) []string {
	t.Helper()
	// The files are listed and parsed one by one rather than with
	// `parser.ParseDir`, which is deprecated: it ignores build tags when it groups
	// files into packages. Every name that reaches `out` has to come from a real
	// declaration anyway, so the grouping is not something this helper needs.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
		}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || !ts.Name.IsExported() {
					continue
				}
				if _, ok := ts.Type.(*ast.StructType); !ok {
					continue
				}
				n := ts.Name.Name
				if strings.HasSuffix(n, "DTO") || strings.HasSuffix(n, "Request") ||
					strings.HasSuffix(n, "Query") {
					out = append(out, n)
				}
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Reflection ↔ schema comparison
// ---------------------------------------------------------------------------

var (
	timeType    = reflect.TypeOf(time.Time{})
	uuidType    = reflect.TypeOf(uuid.UUID{})
	rawJSONType = reflect.TypeOf(json.RawMessage{})
)

type comparer struct {
	doc *doc
	f   *failures
	// seen guards against the cycles a self-referencing schema creates
	// (GroupDetailDTO contains AlertRefDTO contains …).
	seen map[string]bool
	// visited records every Go struct type the walk reached, so that a
	// `nestedOnly` exemption can be verified rather than believed.
	visited map[string]bool
	// request switches checkRequired between the decode rule and the encode
	// rule. See the comment there.
	request bool
	// unbuiltSeen records which unbuiltProperties entries were actually hit,
	// so a stale one is an error rather than a free pass.
	unbuiltSeen map[string]bool
	// unenforceableSeen does the same for unenforceableRequired.
	unenforceableSeen map[string]bool
}

// compare walks a Go struct type against a schema node.
func (c *comparer) compare(path string, gt reflect.Type, node map[string]any) {
	for gt.Kind() == reflect.Pointer {
		gt = gt.Elem()
	}
	if c.visited != nil && gt.Kind() == reflect.Struct && strings.HasSuffix(gt.PkgPath(), "/api") {
		c.visited[domainKey(gt)] = true
	}
	key := path + "|" + gt.String() + "|" + refName(node)
	if c.seen[key] {
		return
	}
	c.seen[key] = true

	fl := c.doc.flatten(node)
	if len(fl.variants) > 0 {
		// A union: the Go side is one struct, so the best available check is
		// that the struct matches at least one branch. Rather than guess, the
		// gate reports the union and moves on — silently passing a union would
		// be worse, and silently failing it would be wrong.
		c.f.addf("%s: the contract declares a oneOf/anyOf union of %d branches; "+
			"G1 cannot diff a single Go struct against a union — split the schema "+
			"or bind the Go type to one branch", path, len(fl.variants))
		return
	}
	if !fl.isObject() {
		c.f.addf("%s: Go declares a struct but the contract declares %s", path, fl.typeNames())
		return
	}

	// The label a divergence is reported under is the COMPONENT SCHEMA, not the
	// route that reached it, because that is where the fix goes. `path` is kept
	// for the nodes the contract inlines and never names.
	label := fl.origin
	if label == "" {
		label = path
	}

	goFields := jsonFields(gt)

	for _, name := range sortedKeys(goFields) {
		gf := goFields[name]
		prop, ok := fl.props[name]
		if !ok {
			c.f.addf("%s.%s: the Go DTO serialises this key; the contract declares no such property "+
				"(%s.%s)", label, name, goTypeLabel(gt), gf.sf.Name)
			continue
		}
		c.checkRequired(label, goTypeLabel(gt), name, gf, fl, prop)
		c.compareField(label+"."+name, gf, prop)
	}

	for _, name := range sortedKeys(fl.props) {
		if _, ok := goFields[name]; !ok {
			key := goTypeLabel(gt) + "." + name
			if _, ok := unbuiltProperties[key]; ok {
				c.unbuiltSeen[key] = true
				continue
			}
			req := "optional"
			if fl.required[name] {
				req = "REQUIRED"
			}
			c.f.addf("%s.%s: the contract declares this property (%s); no Go json tag on %s "+
				"produces it, so the server never sends it", label, name, req, goTypeLabel(gt))
		}
	}
}

// goTypeLabel renders a Go type as `<domain>.<Name>`. Every api package is
// literally `package api`, so `reflect.Type.String()` alone says `api.AlertDTO`
// for two different structs.
func goTypeLabel(t reflect.Type) string {
	if strings.HasSuffix(t.PkgPath(), "/api") {
		return domainKey(t)
	}
	return t.String()
}

// checkRequired asserts that the contract's `required` list says the same thing
// about one property that the Go struct does.
//
// The two directions are NOT symmetric, and conflating them was the first thing
// this gate got wrong:
//
//   - A REQUEST is decoded. Whether a key may be absent is decided by the
//     `validate` tag, because that is the layer that rejects the body — a
//     request field without `omitempty` in its `validate` tag is one the server
//     refuses to do without, whatever the json tag says about encoding.
//
//   - A RESPONSE is encoded. Whether a key is present is decided by the json
//     tag alone: no `omitempty` means the key is on the wire every time.
//
// One convention of this contract is honoured rather than fought: a NULLABLE
// response property is consistently left out of `required` (see `SourceDTO`,
// where `prometheus_url` and `health` are both `null`-able and neither is
// required). Both readings are safe for a client — the key is always there
// carrying `null`, and a client that treats "absent" as "null" is right either
// way — so demanding the other convention would have produced three hundred
// findings that all said the same thing about a decision somebody made on
// purpose. The check still bites where it matters: a NON-nullable response
// field missing from `required`, or an `omitempty` field listed in it.
func (c *comparer) checkRequired(path, goType, name string, gf goField, fl flat, prop map[string]any) {
	gotRequired := fl.required[name]

	if c.request {
		wantRequired := validateRequires(gf.sf.Tag.Get("validate"))
		switch {
		case wantRequired && !gotRequired:
			c.f.addf("%s.%s: the Go request struct validates this field as `required`, "+
				"so the server rejects a body without it, but the contract does not list "+
				"it in `required`", path, name)
		case !wantRequired && gotRequired:
			key := goType + "." + name
			if _, ok := unenforceableRequired[key]; ok {
				if c.unenforceableSeen != nil {
					c.unenforceableSeen[key] = true
				}
				return
			}
			c.f.addf("%s.%s: the contract lists this field as `required`, but the Go "+
				"request struct accepts a body without it (`validate:%q`)",
				path, name, gf.sf.Tag.Get("validate"))
		}
		return
	}

	switch {
	case !gf.omitempty && !gotRequired:
		if c.doc.flatten(prop).nullable {
			return // the contract's nullable-is-not-required convention
		}
		c.f.addf("%s.%s: Go has no `omitempty` and the contract does not permit null, "+
			"so the key is on the wire every time, but the contract does not list it "+
			"in `required`", path, name)
	case gf.omitempty && gotRequired:
		c.f.addf("%s.%s: Go has `omitempty`, so the server omits the key when the value "+
			"is empty, but the contract lists it in `required`", path, name)
	}
}

// validateRequires reports whether a `validate` tag makes the field mandatory.
func validateRequires(tag string) bool {
	for _, rule := range strings.Split(tag, ",") {
		name, _, _ := strings.Cut(rule, "=")
		if name == "required" {
			return true
		}
	}
	return false
}

// goField is one reflected struct field that reaches the wire.
type goField struct {
	sf        reflect.StructField
	omitempty bool
	// t is the field type with the outermost pointer stripped.
	t      reflect.Type
	isPtr  bool
	string bool // `json:",string"`
}

// jsonFields returns the wire keys a struct produces, following embedded
// structs the way encoding/json promotes them.
func jsonFields(t reflect.Type) map[string]goField {
	out := map[string]goField{}
	collectJSONFields(t, out)
	return out
}

func collectJSONFields(t reflect.Type, out map[string]goField) {
	if t.Kind() != reflect.Struct {
		return
	}
	for i := range t.NumField() {
		sf := t.Field(i)
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if sf.Anonymous && name == "" {
			et := sf.Type
			for et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct && et != timeType {
				collectJSONFields(et, out)
				continue
			}
		}
		if !sf.IsExported() {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		ft := sf.Type
		isPtr := ft.Kind() == reflect.Pointer
		if isPtr {
			ft = ft.Elem()
		}
		out[name] = goField{
			sf:        sf,
			omitempty: strings.Contains(","+opts+",", ",omitempty,"),
			t:         ft,
			isPtr:     isPtr,
			string:    strings.Contains(","+opts+",", ",string,"),
		}
	}
}

// compareField checks one property's type, nullability and, for composites,
// its element type.
func (c *comparer) compareField(path string, gf goField, node map[string]any) {
	fl := c.doc.flatten(node)

	// A type with its own MarshalJSON decides its wire shape in code
	// reflection cannot read — `sources.NullableString` reflects as
	// `{Set bool; Value string}` and marshals as `string|null`. Judging it by
	// its fields would report three divergences that are not there, so G1 says
	// only what it can prove: the contract must permit null, because the whole
	// reason these types exist is to tell an explicit `null` from an absent
	// key. Their VALUE shape is left to gate G2, which sees the real bytes.
	if isOpaqueJSON(gf.t) {
		if !fl.nullable && len(fl.variants) == 0 {
			c.f.addf("%s: Go declares %s, whose MarshalJSON emits `null` for an unset "+
				"value, but the contract declares %s and does not permit null",
				path, gf.t.String(), fl.typeNames())
		}
		return
	}

	// A `*T` field marshals to `null` when nil, so unless the key is omitted
	// entirely the contract must say so. This is the check that catches the
	// most common real divergence: a pointer added to a DTO and the contract
	// left saying `type: string`.
	if gf.isPtr && !gf.omitempty && !fl.nullable && len(fl.variants) == 0 {
		c.f.addf("%s: Go declares *%s without `omitempty`, so the wire value is `null` "+
			"when unset, but the contract declares %s and does not permit null",
			path, gf.t.String(), fl.typeNames())
	}
	// The reverse only means something for a RESPONSE. On a request body,
	// `encoding/json` decodes `null` into a non-pointer field by leaving it at
	// its zero value and returning no error, so a contract that permits null
	// there is describing something the server genuinely tolerates.
	if !c.request && !gf.isPtr && fl.nullable && !isNilable(gf.t) {
		c.f.addf("%s: the contract permits null, but Go declares a non-pointer %s "+
			"which can never marshal to null", path, gf.t.String())
	}

	if len(fl.variants) > 0 {
		return // union; reported at the struct level when it is the whole node
	}

	want, ok := openAPITypeOf(gf.t)
	if !ok {
		return // json.RawMessage / any — the contract may say anything
	}
	if gf.string {
		want = "string" // `json:",string"` boxes a number in a string
	}
	if len(fl.types) > 0 && !fl.types[want] {
		c.f.addf("%s: Go %s marshals to `%s`, the contract declares `%s`",
			path, gf.t.String(), want, fl.typeNames())
		return
	}
	if len(fl.types) == 0 && !fl.hasProps {
		// An untyped node constrains nothing; `Uuid` and friends always carry a
		// type, so this is a hole in the contract rather than deliberate width.
		c.f.addf("%s: the contract node declares no `type` at all, so it constrains "+
			"nothing; Go marshals `%s`", path, want)
		return
	}

	switch gf.t.Kind() {
	case reflect.Slice, reflect.Array:
		if gf.t == uuidType {
			return
		}
		if fl.items == nil {
			c.f.addf("%s: the contract declares an array with no `items`", path)
			return
		}
		c.compareElem(path+"[]", gf.t.Elem(), fl.items)
	case reflect.Map:
		if fl.addl == nil && !fl.hasProps {
			return // a free-form object; the Go map agrees
		}
		if fl.addl != nil {
			c.compareElem(path+"{}", gf.t.Elem(), fl.addl)
		}
	case reflect.Struct:
		if gf.t == timeType {
			return
		}
		c.compare(path, gf.t, node)
	default:
	}
}

// compareElem checks an array element or a map value.
func (c *comparer) compareElem(path string, gt reflect.Type, node map[string]any) {
	isPtr := gt.Kind() == reflect.Pointer
	if isPtr {
		gt = gt.Elem()
	}
	c.compareField(path, goField{t: gt, isPtr: isPtr, omitempty: true}, node)
}

var jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()

// isOpaqueJSON reports whether a type overrides its own JSON encoding, making
// its struct fields a lie about its wire shape. `time.Time` and `uuid.UUID` do
// too, but their wire shape is known and handled by openAPITypeOf.
func isOpaqueJSON(t reflect.Type) bool {
	if t == timeType || t == uuidType || t == rawJSONType {
		return false
	}
	return t.Implements(jsonMarshalerType) || reflect.PointerTo(t).Implements(jsonMarshalerType)
}

// isNilable reports whether a non-pointer Go type can still marshal to `null`.
func isNilable(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Slice, reflect.Map, reflect.Interface:
		return t != uuidType
	default:
		return t == rawJSONType
	}
}

// openAPITypeOf maps a Go type onto the OpenAPI type it marshals to. The second
// result is false for types whose JSON shape is unconstrained.
func openAPITypeOf(t reflect.Type) (string, bool) {
	switch t {
	case timeType:
		return "string", true
	case uuidType:
		return "string", true
	case rawJSONType:
		return "", false
	}
	switch t.Kind() {
	case reflect.String:
		return "string", true
	case reflect.Bool:
		return "boolean", true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer", true
	case reflect.Float32, reflect.Float64:
		return "number", true
	case reflect.Slice, reflect.Array:
		return "array", true
	case reflect.Map, reflect.Struct:
		return "object", true
	case reflect.Interface:
		return "", false
	default:
		return "", false
	}
}

// ---------------------------------------------------------------------------
// Query objects → operation parameters
// ---------------------------------------------------------------------------

// queryBinding ties a `*Query` struct to EVERY operation whose query string it
// is the validated form of.
//
// It is a list and not one id because several of these structs serve more than
// one route — `alerts.TimelineQuery` is the event-list query of both
// `listAlertEvents` and `listOccurrenceEvents` — and checking only one of them
// is how the other quietly acquires a parameter nobody reads.
type queryBinding struct {
	pkg   string
	opIDs []string
	v     any
	// ignore lists json keys the contract has no parameter for, with a reason.
	ignore map[string]string
	// deepObject lists parameters the handler reads from the RAW query string
	// rather than through a struct field, because their wire form is
	// `name[key]=value` and no single json tag can name it. Every entry is
	// verified: the contract must really declare it `style: deepObject`, the
	// parameter must still exist, and the struct must still not have a field
	// for it, so this cannot become the place a forgotten filter hides.
	deepObject map[string]string
}

// labelSelectorParam is the reason `label` is read outside the query struct on
// both alert reads.
const labelSelectorParam = "`label[k]=v` is a deepObject: the wire keys are " +
	"`label[team]` and `label[!tier]`, never `label`. parseListAlerts reads them " +
	"out of the raw parameters via filter.ParseLabelParams and compiles them into " +
	"the ADR 0017 selector AST, and `listAlertsParams` admits them by the `label[` " +
	"prefix, so the filter is applied — it just cannot be a struct field"

var queryBindings = []queryBinding{
	{pkg: "alerts", opIDs: []string{"listAlerts"}, v: alertsapi.ListAlertsQuery{},
		deepObject: map[string]string{"label": labelSelectorParam}},
	{pkg: "alerts", opIDs: []string{"listAlertRollups"}, v: alertsapi.ListRollupsQuery{},
		deepObject: map[string]string{"label": labelSelectorParam}},
	{pkg: "alerts", opIDs: []string{"listAlertEvents", "listOccurrenceEvents"},
		v: alertsapi.TimelineQuery{}},
	{pkg: "alerts", opIDs: []string{"listLabelNames"}, v: alertsapi.LabelQuery{}},
	{pkg: "grouping", opIDs: []string{"listAlertGroups"}, v: groupingapi.ListGroupsQuery{}},
	{pkg: "grouping", opIDs: []string{"getAlertGroupTimeline"}, v: groupingapi.TimelineQuery{}},
	{pkg: "silences", opIDs: []string{"listSilences"}, v: silencesapi.ListSilencesQuery{}},
	{pkg: "rules", opIDs: []string{"listRuleSnapshots"}, v: rulesapi.ListSnapshotsQuery{}},
	{pkg: "rules", opIDs: []string{"getAlertRuleHistory"}, v: rulesapi.HistoryQuery{}},
	{pkg: "rules", opIDs: []string{"batchGetRuleSnapshots"}, v: rulesapi.BatchSnapshotsQuery{}},
	{pkg: "stats", opIDs: []string{"getStatsOverview"}, v: statsapi.OverviewQuery{}},
	{pkg: "stats", opIDs: []string{"getAlertQualityStats"}, v: statsapi.AlertQualityQuery{}},
	{pkg: "identity", opIDs: []string{"listApiTokens"}, v: identityapi.PageQuery{}},
}

// TestQueryParamsMatchContract is the half of G1 that covers `*Query` structs.
// A query object is not a schema — it is the validated form of an operation's
// query string — so it is diffed against that operation's `parameters` instead.
func TestQueryParamsMatchContract(t *testing.T) {
	d := loadDoc(t)
	ops := d.operations()
	var f failures

	for _, qb := range queryBindings {
		gt := reflect.TypeOf(qb.v)
		fields := jsonFields(gt)

		for _, opID := range qb.opIDs {
			op, ok := ops[opID]
			if !ok {
				f.addf("%s.%s: the contract declares no operation `%s`",
					qb.pkg, gt.Name(), opID)
				continue
			}
			declared := op.queryParams(d)

			for _, name := range sortedKeys(fields) {
				p, ok := declared[name]
				if !ok {
					if _, skip := qb.ignore[name]; skip {
						continue
					}
					f.addf("%s.%s: %s reads query parameter `%s`; operation `%s` declares no "+
						"such parameter", qb.pkg, gt.Name(), gt.Name(), name, opID)
					continue
				}
				schema, _ := p["schema"].(map[string]any)
				if schema == nil {
					schema = map[string]any{}
				}
				c := &comparer{doc: d, f: &f, seen: map[string]bool{}}
				c.compareField(fmt.Sprintf("%s(%s)?%s", opID, gt.Name(), name),
					queryField(fields[name]), schema)
			}
			for _, name := range sortedKeys(declared) {
				if _, ok := fields[name]; ok {
					continue
				}
				if _, raw := qb.deepObject[name]; raw {
					// The claim is checked, not believed: only a `deepObject`
					// parameter is unreachable from a json tag, so an exemption
					// on any other style is itself a failure.
					if style, _ := declared[name]["style"].(string); style != "deepObject" {
						f.addf("%s: `%s` is exempted as read-from-raw-parameters, but the "+
							"contract declares `style: %q`, not `deepObject` — a parameter a "+
							"struct field COULD name must be named by one", opID, name, style)
					}
					continue
				}
				f.addf("%s: the contract declares query parameter `%s`; %s.%s has no field "+
					"that reads it, so setting it does nothing",
					opID, name, qb.pkg, gt.Name())
			}
			for _, name := range sortedKeys(qb.deepObject) {
				if _, ok := declared[name]; !ok {
					f.addf("%s: the deepObject exemption names `%s`, which the operation no "+
						"longer declares — a stale exemption hides the next one", opID, name)
				}
				if _, ok := fields[name]; ok {
					f.addf("%s: the deepObject exemption names `%s`, but %s.%s now has a field "+
						"for it, so the exemption is checking nothing",
						opID, name, qb.pkg, gt.Name())
				}
			}
		}
	}

	f.report(t, "gate G1: query objects and the contract's operation parameters disagree")
}

// TestEnumFilterCeilingsMatchTheirEnum asserts that an array query parameter
// whose elements are a closed enum is bounded at exactly the size of that enum.
//
// A filter ceiling BELOW the enum it filters is a bound that can only ever be
// wrong: "give me everything" is the one request every such filter must be able
// to express, and the caller is refused by its OWN generated client, before a
// server that would have answered it ever sees the request. `?type=` on the
// three timelines spent two releases in that state, because `maxItems` is
// written next to the `$ref` and the enum grows somewhere else entirely —
// nothing tied the two numbers together. This is that tie.
//
// A ceiling ABOVE the enum is harmless but is still a failure here, because
// `uniqueItems: true` makes it unreachable: it can only mean the enum shrank
// and the bound did not, and a bound nobody maintains is the one that falls
// behind next.
//
// Parameters whose items are NOT enum-backed are skipped DELIBERATELY. A
// free-text filter — `cluster`, `namespace`, `alertname`, `severity` — is
// bounded by how many values a query string may sensibly carry, which is a
// judgement about request size and has no enum to be equal to. Asserting
// anything about those numbers here would be inventing a rule, not enforcing
// one.
func TestEnumFilterCeilingsMatchTheirEnum(t *testing.T) {
	d := loadDoc(t)
	ops := d.operations()
	var f failures
	checked := 0

	for _, opID := range sortedKeys(ops) {
		params := ops[opID].queryParams(d)
		for _, name := range sortedKeys(params) {
			node, _ := params[name]["schema"].(map[string]any)
			if node == nil {
				continue
			}
			fl := d.flatten(node)
			if !fl.types["array"] || fl.items == nil {
				continue
			}
			if elem := d.flatten(fl.items); len(elem.enum) > 0 {
				checked++
				switch {
				case fl.maxItems == nil:
					f.addf("%s: query parameter `%s` is an array of %s, a closed enum of %d "+
						"values, and declares no `maxItems` — every other array parameter in "+
						"this contract carries one", opID, name, enumOrigin(elem, fl.items), len(elem.enum))
				case *fl.maxItems != len(elem.enum):
					f.addf("%s: query parameter `%s` declares `maxItems: %d` but its items are "+
						"%s, a closed enum of %d values — a caller asking for every value it is "+
						"allowed to name writes a request its own client refuses to send. Set "+
						"`maxItems: %d`", opID, name, *fl.maxItems, enumOrigin(elem, fl.items),
						len(elem.enum), len(elem.enum))
				}
			}
		}
	}

	if checked == 0 {
		t.Fatal("found no enum-backed array query parameter at all — the walk is looking in " +
			"the wrong place and this gate is asserting nothing")
	}
	f.report(t, "gate G1: an enum-backed filter is bounded at something other than its enum")
}

// enumOrigin names the enum a failure is about: the component schema when the
// items are a `$ref`, and the enum's own values when the contract spells them
// inline and there is no name to give.
func enumOrigin(elem flat, items map[string]any) string {
	if name := refName(items); name != "" {
		return "`" + name + "`"
	}
	vals := make([]string, 0, len(elem.enum))
	for _, v := range elem.enum {
		vals = append(vals, fmt.Sprint(v))
	}
	return "the inline enum [" + strings.Join(vals, " ") + "]"
}

// queryField relaxes required-ness for a query parameter: a query object always
// has every field present in Go, and absence is expressed by the zero value,
// not by `omitempty`.
func queryField(gf goField) goField {
	gf.omitempty = true
	return gf
}

// operation is one path+method from the contract.
type operation struct {
	method   string
	path     string
	node     map[string]any
	pathItem map[string]any
}

// operations indexes every operation by its operationId.
func (d *doc) operations() map[string]operation {
	out := map[string]operation{}
	for path, raw := range d.paths {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, m := range []string{"get", "post", "put", "patch", "delete", "head", "options"} {
			node, ok := item[m].(map[string]any)
			if !ok {
				continue
			}
			id, _ := node["operationId"].(string)
			if id == "" {
				continue
			}
			out[id] = operation{method: m, path: path, node: node, pathItem: item}
		}
	}
	return out
}

// queryParams returns the operation's `in: query` parameters — its own and the
// ones inherited from the path item — as name → PARAMETER node.
//
// It is the whole parameter and not just its schema because `style` decides
// what a name means on the wire: a `deepObject` arrives as `label[team]=core`
// and is a different thing from a parameter literally called `label`.
func (o operation) queryParams(d *doc) map[string]map[string]any {
	out := map[string]map[string]any{}
	add := func(list any) {
		items, ok := list.([]any)
		if !ok {
			return
		}
		for _, raw := range items {
			p, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			p = d.derefParameter(p)
			if in, _ := p["in"].(string); in != "query" {
				continue
			}
			name, _ := p["name"].(string)
			if name == "" {
				continue
			}
			out[name] = p
		}
	}
	add(o.pathItem["parameters"])
	add(o.node["parameters"])
	return out
}

// derefParameter resolves `$ref: '#/components/parameters/X'`.
func (d *doc) derefParameter(p map[string]any) map[string]any {
	ref, ok := p["$ref"].(string)
	if !ok {
		return p
	}
	name := strings.TrimPrefix(ref, "#/components/parameters/")
	if name == ref {
		return p
	}
	comps, _ := d.root["components"].(map[string]any)
	params, _ := comps["parameters"].(map[string]any)
	if target, ok := params[name].(map[string]any); ok {
		return target
	}
	return p
}
