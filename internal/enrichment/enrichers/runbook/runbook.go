package runbook

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/platform/validate"
)

// Name is the registry id.
//
// SPEC §F.3 calls this enricher `runbook`, but enrichments_name_ck requires a
// DOTTED name (`^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)+$`), so a bare "runbook" is
// not storable. `runbook.link` is the same enricher under a name the schema
// will actually accept.
const Name = "runbook.link"

// Version is bumped when the payload shape or the normalisation rules change.
const Version = 1

// Timeout is the per-call ceiling from SPEC §F.3.
//
// 5 ms, because this enricher does no I/O whatsoever. It reads a map, validates
// some strings and returns. If it ever needs more than 5 ms, something has been
// added to it that does not belong in it.
const Timeout = 5 * time.Millisecond

// MaxURLBytes bounds a stored link. Anything longer is a payload, not a link.
const MaxURLBytes = 2048

// MaxLinks bounds how many links one alert may contribute. An annotation set
// full of URLs is a templating accident, not context.
const MaxLinks = 16

// Link kinds, in the order they are reported.
const (
	// KindRunbook is the operator's procedure for this alert.
	KindRunbook = "runbook"
	// KindDashboard is a Grafana or similar dashboard.
	KindDashboard = "dashboard"
	// KindLogs is a log search.
	KindLogs = "logs"
	// KindOther is an absolute URL in an annotation oto has no name for.
	KindOther = "other"
)

// runbookKeys, dashboardKeys and logKeys are the annotation names oto
// recognises, lowercased. The lists are deliberately short and closed: guessing
// that `url` means "runbook" would put a wrong link in front of somebody at
// 3am, and a link nobody recognises is still reported, as KindOther.
var (
	runbookKeys   = []string{"runbook_url", "runbook", "runbookurl", "playbook_url", "playbook"}
	dashboardKeys = []string{"dashboard_url", "dashboarduid", "dashboard", "grafana_url", "grafana"}
	logKeys       = []string{"logs_url", "log_url", "logs", "kibana_url", "loki_url"}
)

// Link is one validated, normalised link.
type Link struct {
	// Kind is runbook | dashboard | logs | other.
	Kind string `json:"kind"`
	// Source is the annotation name it came from, or "org_template".
	Source string `json:"source"`
	// URL is absolute, http(s), credential-free and length-bounded.
	URL string `json:"url"`
}

// Payload is the enricher's typed output.
type Payload struct {
	// Runbook is the primary link, hoisted because it is the one the Slack card
	// renders as a button (SPEC §F.2 Links.Runbook).
	Runbook string `json:"runbook,omitempty"`
	// Links are every recognised link, deterministically ordered.
	Links []Link `json:"links"`
	// Rejected names annotations that looked like links but were not usable, so
	// that a typo in an annotation is visible rather than silently dropped.
	Rejected []string `json:"rejected,omitempty"`
}

// TemplateResolver supplies the org-level `alertname → url` fallback of SPEC
// §F.3, for the very common case of a team whose alerts have no runbook_url
// annotation but whose wiki is laid out predictably.
//
// It is a port rather than a map so the templates can come from org settings
// without this package learning what an org is.
type TemplateResolver interface {
	// RunbookTemplate returns the template for an alertname, or "" for none.
	// Placeholders are `{label}` and are substituted from the alert's labels.
	RunbookTemplate(alertName string) string
}

// StaticTemplates is a TemplateResolver over a fixed map, which is what the
// config file produces and what a test wants.
type StaticTemplates map[string]string

// RunbookTemplate implements TemplateResolver.
func (t StaticTemplates) RunbookTemplate(alertName string) string { return t[alertName] }

// Enricher extracts and normalises the links an alert already carries.
//
// It NEVER FETCHES A URL. Not to validate it, not to follow a redirect, not to
// check it is alive. oto is an alerting product handling operator-supplied
// strings; a component that dials arbitrary URLs out of an annotation is an
// SSRF primitive with a friendly name, and the value of knowing that a runbook
// returns 200 does not come close to justifying it.
type Enricher struct {
	templates TemplateResolver
}

// Enricher satisfies the port.
var _ domain.Enricher = (*Enricher)(nil)

// New builds the enricher. A nil resolver disables the org-template fallback.
func New(templates TemplateResolver) *Enricher { return &Enricher{templates: templates} }

// Name is the stable registry id.
func (*Enricher) Name() string { return Name }

// Version is the payload/semantics version.
func (*Enricher) Version() int { return Version }

// Phase is inline: it is free, and a runbook button on the first card is worth
// more than a runbook button on an amendment thirty seconds later.
func (*Enricher) Phase() domain.Phase { return domain.PhaseInline }

// Timeout is the per-call ceiling.
func (*Enricher) Timeout() time.Duration { return Timeout }

// Applicable is true whenever there is anything at all to read.
func (e *Enricher) Applicable(s *domain.Subject) bool {
	if s == nil {
		return false
	}
	return len(s.Alert.Annotations) > 0 || e.templates != nil
}

// Enrich reads the annotations and returns validated links.
//
// It is a PURE FUNCTION of the Subject and the injected templates: no clock, no
// database, no network. That is what makes it trivially fakeable and what makes
// its 5 ms timeout honest.
func (e *Enricher) Enrich(_ context.Context, s *domain.Subject) (domain.Result, error) {
	if s == nil {
		return domain.Result{Status: domain.StatusSkipped}, nil
	}

	var (
		links    []Link
		rejected []string
		seen     = map[string]bool{}
	)

	add := func(kind, source, raw string) {
		clean, ok := normalise(raw)
		if !ok {
			rejected = append(rejected, source)
			return
		}
		if seen[clean] || len(links) >= MaxLinks {
			return
		}
		seen[clean] = true
		links = append(links, Link{Kind: kind, Source: source, URL: clean})
	}

	// Annotation names are iterated in sorted order, never in map order: two
	// runs over the same alert must produce the same list in the same order or
	// the rendered card churns.
	names := make([]string, 0, len(s.Alert.Annotations))
	for k := range s.Alert.Annotations {
		names = append(names, k)
	}
	sort.Strings(names)

	for _, name := range names {
		value := strings.TrimSpace(s.Alert.Annotations[name])
		if value == "" {
			continue
		}
		lower := strings.ToLower(name)
		switch {
		case matches(lower, runbookKeys):
			add(KindRunbook, name, value)
		case matches(lower, dashboardKeys):
			add(KindDashboard, name, value)
		case matches(lower, logKeys):
			add(KindLogs, name, value)
		case looksLikeURL(value):
			add(KindOther, name, value)
		}
	}

	// The org template is a FALLBACK, not an override: an alert that names its
	// own runbook always wins, because whoever wrote the annotation knew more
	// about this alert than whoever wrote the org-wide pattern.
	if !hasKind(links, KindRunbook) && e.templates != nil && s.Alert.AlertName != "" {
		if tmpl := strings.TrimSpace(e.templates.RunbookTemplate(s.Alert.AlertName)); tmpl != "" {
			add(KindRunbook, "org_template", expand(tmpl, s.Alert.Labels))
		}
	}

	sort.SliceStable(links, func(i, j int) bool {
		if links[i].Kind != links[j].Kind {
			return kindRank(links[i].Kind) < kindRank(links[j].Kind)
		}
		return links[i].Source < links[j].Source
	})

	payload := Payload{Links: links, Rejected: rejected}
	for _, l := range links {
		if l.Kind == KindRunbook {
			payload.Runbook = l.URL
			break
		}
	}

	// ⛔ THE ORDER OF THESE TWO BRANCHES IS LOAD-BEARING, and it used to be the
	// other way round: `len(links) == 0` overwrote the `partial` unconditionally,
	// which is exactly the case the report exists for — the alert's ONLY link
	// annotation was the broken one, so of course the list is empty.
	//
	// `skipped` is documented as "Applicable said no". Applicable said yes here:
	// the enricher looked, found a broken annotation and named it. Filing that as
	// `skipped` is not cosmetic — Status.Succeeded() and Enrichment.Reusable are
	// both false for it, so a run that produced a real finding is recorded
	// identically to one that never ran, and the finding never reaches anyone who
	// could fix the rule file. Both sibling enrichers use the other convention:
	// alerthistory and promrule report `partial` with a warning whenever they
	// looked and produced less than they wanted.
	status := domain.StatusOK
	var warnings []string
	if len(links) == 0 {
		// Nothing found and nothing wrong: a declined enrichment, not a failed one.
		status = domain.StatusSkipped
	}
	if len(rejected) > 0 {
		// A malformed runbook_url is a real, fixable problem in somebody's rule
		// file. Reporting it is more useful than hiding it.
		status = domain.StatusPartial
		warnings = append(warnings, "unusable_link_annotations")
	}

	return domain.Result{
		Status:   status,
		Payload:  payload,
		Warnings: warnings,
		// Deliberately uncached: the computation is a few string operations, so
		// a cache round trip would cost more than the work it saves.
		CacheKey: "",
	}, nil
}

func matches(lower string, keys []string) bool {
	for _, k := range keys {
		if lower == k {
			return true
		}
	}
	return false
}

func hasKind(links []Link, kind string) bool {
	for _, l := range links {
		if l.Kind == kind {
			return true
		}
	}
	return false
}

func kindRank(kind string) int {
	switch kind {
	case KindRunbook:
		return 0
	case KindDashboard:
		return 1
	case KindLogs:
		return 2
	default:
		return 3
	}
}

// looksLikeURL is the cheap pre-filter that decides whether an annotation oto
// has no name for is worth treating as a link at all.
//
// ⭐ THE COMPARISON IS CASE-INSENSITIVE. RFC 3986 §3.1 makes a scheme
// case-insensitive, so `HTTPS://wiki.example/runbook` is the same link as the
// lowercase spelling. This used to be a case-sensitive HasPrefix, which meant an
// annotation written by somebody whose editor capitalised the scheme was filed
// as "unusable" and the operator was blamed for a good link.
func looksLikeURL(v string) bool {
	lower := strings.ToLower(v)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// normalise validates and canonicalises one link.
//
// The rules are conservative on purpose. A link oto shows is a link somebody
// will click while under pressure, so anything ambiguous is rejected rather
// than repaired:
//
//   - absolute http(s) only — no scheme-relative, no javascript:, no file:;
//   - the scheme is matched case-insensitively (RFC 3986 §3.1) and lowercased;
//   - no embedded credentials, which would print a secret into Slack;
//   - no unresolved `{placeholder}`, see below;
//   - length-bounded;
//   - the fragment and query are preserved verbatim, because a dashboard link
//     without its time range is a link to the wrong thing.
//
// ⛔ THE PREDICATE IS validate.IsOperatorLinkURL, NEVER validate.IsAbsoluteHTTPURL.
// The latter is the `alert_sources.base_url` rule: a base URL is a PREFIX oto
// concatenates API paths onto, so it refuses a query string, a fragment and a
// trailing slash. Pointed at an operator's link it refused
// `?from=now-1h&to=now`, `#step-2` and `https://wiki.example/runbooks/` — most
// of the real annotations there are — while the list above promised the exact
// opposite. The two predicates guard different columns and must stay apart.
func normalise(raw string) (string, bool) {
	v := strings.TrimSpace(raw)
	v = strings.Trim(v, "<>\"'")
	if v == "" || len(v) > MaxURLBytes {
		return "", false
	}
	if !looksLikeURL(v) {
		return "", false
	}
	if strings.ContainsAny(v, "{}") {
		// ⛔ AN UNRESOLVED ORG-TEMPLATE PLACEHOLDER DIES HERE, and it has to die
		// here rather than later: `url.URL.String` percent-encodes a literal brace
		// into `%7B`/`%7D`, which turns `https://wiki.example/org/{region}/runbook`
		// into a URL that VALIDATES PERFECTLY and 404s on somebody's wiki. Getting
		// hoisted into Payload.Runbook, it becomes the button on the Slack card
		// (SPEC §F.2) — a field that looks populated and is wrong, shown to a human
		// deciding whether to act at 3am. That is worse than no button.
		//
		// A brace is unambiguous evidence of an unfilled placeholder: `expand`
		// substitutes every label through url.PathEscape, which encodes `{` and `}`
		// to `%7B`/`%7D`, so a label value legitimately containing a brace never
		// reaches this check as a literal one. (An unencoded brace is not a legal
		// URI character under RFC 3986 anyway, so a hand-written annotation
		// carrying one is also correctly refused.)
		return "", false
	}
	if !validate.IsOperatorLinkURL(v) {
		return "", false
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return "", false
	}
	if u.User != nil {
		// A credential in a URL is a credential in a Slack message.
		return "", false
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u.String(), true
}

// expand substitutes `{label}` placeholders from the alert's labels.
//
// An unresolved placeholder leaves the template unexpanded, which then fails
// normalisation and is reported as rejected — better than emitting a link with
// a literal `{namespace}` in the path and letting somebody click it. That is
// enforced by the brace check in `normalise`; it is stated here because this is
// the function that can leave a brace behind, and the two halves of the rule
// have to be read together.
func expand(tmpl string, labels map[string]string) string {
	if !strings.Contains(tmpl, "{") {
		return tmpl
	}
	out := tmpl
	names := make([]string, 0, len(labels))
	for k := range labels {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		out = strings.ReplaceAll(out, "{"+k+"}", url.PathEscape(labels[k]))
	}
	return out
}
