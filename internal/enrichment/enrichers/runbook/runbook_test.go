package runbook_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/enrichment/enrichers/runbook"
)

// runbook.link is the one enricher with NO UPSTREAM AT ALL. It reads a map,
// validates some strings and returns, which is why its declared timeout is 5 ms.
//
// That is a property worth pinning rather than assuming: a component that dialled
// arbitrary URLs out of an operator-supplied annotation would be an SSRF
// primitive with a friendly name. TestItNeverFetchesAnyURL below is the test
// that keeps it honest.

func subject(annotations map[string]string) *domain.Subject {
	return &domain.Subject{
		OrgID:       "0198c0de-0000-7000-8000-0000000000aa",
		SubjectKind: domain.SubjectOccurrence,
		SubjectID:   "0198c0de-0000-7000-8000-0000000000bb",
		Alert: domain.AlertSnapshot{
			ID:          "0198c0de-0000-7000-8000-0000000000cc",
			AlertName:   "HighErrorRate",
			Severity:    "critical",
			Namespace:   "payments",
			Labels:      map[string]string{"namespace": "payments", "service": "checkout"},
			Annotations: annotations,
		},
	}
}

func enrich(t *testing.T, e *runbook.Enricher, s *domain.Subject) domain.Result {
	t.Helper()
	res, err := e.Enrich(context.Background(), s)
	require.NoError(t, err, "this enricher has no error path: it does no I/O")
	return res
}

func payloadOf(t *testing.T, res domain.Result) runbook.Payload {
	t.Helper()
	p, ok := res.Payload.(runbook.Payload)
	require.True(t, ok, "the payload is the enricher's own typed struct")
	return p
}

// ------------------------------------------------------------------ the ports

func TestTheRegistryContractIsStable(t *testing.T) {
	t.Parallel()

	e := runbook.New(nil)

	// SPEC §F.3 calls this enricher `runbook`, but enrichments_name_ck requires a
	// DOTTED name, so a bare "runbook" is not storable. Renaming it silently
	// would orphan every stored row.
	assert.Equal(t, "runbook.link", e.Name())
	assert.True(t, domain.ValidEnricherName(e.Name()),
		"the registry id must satisfy enrichments_name_ck")
	assert.Equal(t, 1, e.Version())
	assert.Equal(t, domain.PhaseInline, e.Phase(),
		"it is free, and a runbook button on the first card beats one thirty seconds later")
	assert.Equal(t, 5*time.Millisecond, e.Timeout(),
		"5 ms is honest only while this enricher does no I/O")
}

func TestApplicableRequiresSomethingToRead(t *testing.T) {
	t.Parallel()

	bare := runbook.New(nil)
	assert.False(t, bare.Applicable(nil))
	assert.False(t, bare.Applicable(subject(nil)), "no annotations and no templates: nothing to do")
	assert.True(t, bare.Applicable(subject(map[string]string{"runbook_url": "https://wiki/x"})))

	withTemplates := runbook.New(runbook.StaticTemplates{"HighErrorRate": "https://wiki/{namespace}"})
	assert.True(t, withTemplates.Applicable(subject(nil)),
		"a template resolver alone is enough to be worth running")
}

// --------------------------------------------------------------------- found

func TestFoundLinksAreClassifiedAndHoisted(t *testing.T) {
	t.Parallel()

	res := enrich(t, runbook.New(nil), subject(map[string]string{
		"runbook_url":   "https://wiki.example/runbooks/high-error-rate",
		"dashboard_url": "https://grafana.example/d/abc",
		"logs_url":      "https://kibana.example/app/logs",
		"summary":       "the checkout error rate is above 5%",
	}))

	assert.Equal(t, domain.StatusOK, res.Status)
	p := payloadOf(t, res)

	assert.Equal(t, "https://wiki.example/runbooks/high-error-rate", p.Runbook,
		"the primary link is hoisted because the Slack card renders it as a button")
	require.Len(t, p.Links, 3, "a plain-prose annotation is not a link")
	assert.Empty(t, p.Rejected)

	kinds := make([]string, 0, len(p.Links))
	for _, l := range p.Links {
		kinds = append(kinds, l.Kind)
	}
	assert.Equal(t, []string{runbook.KindRunbook, runbook.KindDashboard, runbook.KindLogs}, kinds,
		"runbook, then dashboard, then logs: the order an operator reads them in")

	assert.Equal(t, "https://grafana.example/d/abc", p.Links[1].URL)
	assert.Equal(t, "dashboard_url", p.Links[1].Source, "the annotation name is provenance")
	assert.Empty(t, res.CacheKey, "a few string operations are cheaper than a cache round trip")
	assert.Zero(t, res.TTL)
}

func TestAnUnrecognisedAnnotationHoldingAnAbsoluteURLIsStillReported(t *testing.T) {
	t.Parallel()

	res := enrich(t, runbook.New(nil), subject(map[string]string{
		"incident_channel": "https://slack.example/archives/C123",
	}))

	p := payloadOf(t, res)
	require.Len(t, p.Links, 1)
	assert.Equal(t, runbook.KindOther, p.Links[0].Kind,
		"a link oto has no name for is reported, never guessed at")
	assert.Empty(t, p.Runbook, "and it is emphatically not promoted to the runbook button")
}

// TestGuessingIsRefused: `url` is not `runbook_url`. Guessing would put a wrong
// link in front of somebody at 3am.
func TestGuessingIsRefused(t *testing.T) {
	t.Parallel()

	res := enrich(t, runbook.New(nil), subject(map[string]string{
		"url": "https://example.test/something",
	}))

	p := payloadOf(t, res)
	require.Len(t, p.Links, 1)
	assert.Equal(t, runbook.KindOther, p.Links[0].Kind)
	assert.Empty(t, p.Runbook)
}

func TestTheOutputIsDeterministicWhateverTheMapOrder(t *testing.T) {
	t.Parallel()

	annotations := map[string]string{
		"runbook":       "https://wiki.example/b",
		"runbook_url":   "https://wiki.example/a",
		"grafana":       "https://grafana.example/2",
		"dashboard_url": "https://grafana.example/1",
		"loki_url":      "https://loki.example/1",
	}

	first := payloadOf(t, enrich(t, runbook.New(nil), subject(annotations)))
	for i := 0; i < 20; i++ {
		again := payloadOf(t, enrich(t, runbook.New(nil), subject(annotations)))
		require.Equal(t, first, again,
			"two runs over the same alert must render identically or the card churns")
	}
	// KIND FIRST, THEN THE ANNOTATION NAME. Both runbook annotations sort ahead
	// of both dashboard ones, and only inside a kind does the source name break
	// the tie — `dashboard_url` before `grafana`, `runbook` before `runbook_url`.
	urls := make([]string, 0, len(first.Links))
	for _, l := range first.Links {
		urls = append(urls, l.URL)
	}
	assert.Equal(t, []string{
		"https://wiki.example/b",    // runbook, source "runbook"
		"https://wiki.example/a",    // runbook, source "runbook_url"
		"https://grafana.example/1", // dashboard, source "dashboard_url"
		"https://grafana.example/2", // dashboard, source "grafana"
		"https://loki.example/1",    // logs, source "loki_url"
	}, urls)
}

func TestDuplicateURLsUnderDifferentAnnotationsAreReportedOnce(t *testing.T) {
	t.Parallel()

	res := enrich(t, runbook.New(nil), subject(map[string]string{
		"runbook_url": "https://wiki.example/a",
		"playbook":    "https://wiki.example/a",
	}))

	p := payloadOf(t, res)
	assert.Len(t, p.Links, 1, "the same URL twice is one link")
}

// ----------------------------------------------------------------- not found

func TestNoLinksIsSkippedRatherThanAnEmptyOK(t *testing.T) {
	t.Parallel()

	res := enrich(t, runbook.New(nil), subject(map[string]string{
		"summary":     "the checkout error rate is above 5%",
		"description": "see the wiki",
	}))

	assert.Equal(t, domain.StatusSkipped, res.Status,
		"an empty link list is a declined enrichment, not a successful empty one")
	p := payloadOf(t, res)
	assert.Empty(t, p.Links)
	assert.Empty(t, p.Runbook)
	assert.Empty(t, p.Rejected)
}

func TestANilSubjectIsSkipped(t *testing.T) {
	t.Parallel()

	res, err := runbook.New(nil).Enrich(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusSkipped, res.Status)
}

// --------------------------------------------------- rejection, not silence

// TestAnUnusableLinkIsRejectedLoudly. A typo in an annotation is a real,
// fixable problem in somebody's rule file; reporting it beats hiding it.
func TestAnUnusableLinkIsRejectedLoudly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
	}{
		{name: "a javascript: scheme", url: "javascript:alert(1)"},
		{name: "a file: scheme", url: "file:///etc/passwd"},
		{name: "a scheme-relative URL", url: "//wiki.example/runbook"},
		{name: "a relative path", url: "/runbooks/high-error-rate"},
		{name: "embedded credentials", url: "https://user:hunter2@wiki.example/runbook"},
		{name: "no host", url: "https://"},
		{name: "not a URL at all", url: "see the wiki"},
		{name: "longer than the column allows", url: "https://wiki.example/" + strings.Repeat("a", runbook.MaxURLBytes)},

		// An unfilled org-template placeholder, pasted into an annotation by hand.
		// A literal brace is not a legal URI character and `url.URL.String` would
		// launder it into `%7B`, so it is refused before it can become a button.
		// See TestAnUnresolvedPlaceholderIsRejectedRatherThanShippedAsAButton.
		{name: "an unresolved template placeholder", url: "https://wiki.example/org/{region}/runbook"},

		// ⚠️ WHAT IS NOT IN THIS TABLE MATTERS AS MUCH AS WHAT IS. A query string,
		// a fragment, a trailing slash and an uppercase scheme were all rejected
		// here once, because `normalise` reused `validate.IsAbsoluteHTTPURL` — the
		// `alert_sources.base_url` predicate — on an operator's link. They are all
		// legal links and are now pinned as ACCEPTED by
		// TestALinkKeepsItsQueryFragmentAndTrailingSlash and
		// TestAnUppercaseSchemeIsAcceptedAndLowercased.
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res := enrich(t, runbook.New(nil), subject(map[string]string{"runbook_url": tc.url}))

			// `partial`, NOT `skipped`: the enricher looked, found a broken
			// annotation and named it. `skipped` means "Applicable said no", and
			// Succeeded()/Reusable are both false for it, so filing a real finding
			// under it would record this run identically to one that never ran.
			// See TestARejectedLinkIsReportedAsPartial.
			assert.Equal(t, domain.StatusPartial, res.Status)
			assert.Contains(t, res.Warnings, "unusable_link_annotations")
			p := payloadOf(t, res)
			assert.Equal(t, []string{"runbook_url"}, p.Rejected,
				"the annotation is named so somebody can fix it")
			assert.Empty(t, p.Runbook, "and nothing unusable reaches the button")
			assert.Empty(t, p.Links)
		})
	}
}

// TestACredentialInAURLIsACredentialInASlackMessage is the rejection that is
// about disclosure rather than about correctness, so it gets its own name.
func TestACredentialInAURLIsACredentialInASlackMessage(t *testing.T) {
	t.Parallel()

	res := enrich(t, runbook.New(nil), subject(map[string]string{
		"runbook_url": "https://svc:s3cr3t@wiki.example/runbook",
	}))

	p := payloadOf(t, res)
	for _, l := range p.Links {
		assert.NotContains(t, l.URL, "s3cr3t")
	}
	assert.NotContains(t, p.Runbook, "s3cr3t")
	assert.Equal(t, []string{"runbook_url"}, p.Rejected)
}

func TestSchemeAndHostAreLowercasedButThePathIsNot(t *testing.T) {
	t.Parallel()

	// The scheme is spelt in lowercase here so this case pins the HOST rule on its
	// own; TestAnUppercaseSchemeIsAcceptedAndLowercased pins the scheme. Note that
	// `normalise`'s `u.Scheme = strings.ToLower(u.Scheme)` is belt and braces
	// either way — `url.Parse` has already lowercased the scheme by the time it
	// runs — whereas the host lowercasing is the line doing real work.
	res := enrich(t, runbook.New(nil), subject(map[string]string{
		"runbook_url": "https://Wiki.Example/Runbooks/HighErrorRate",
	}))

	p := payloadOf(t, res)
	require.Len(t, p.Links, 1)
	assert.Equal(t, "https://wiki.example/Runbooks/HighErrorRate", p.Links[0].URL,
		"the host is case-insensitive; the path is not, and is left alone")
}

func TestSurroundingPunctuationIsTrimmed(t *testing.T) {
	t.Parallel()

	res := enrich(t, runbook.New(nil), subject(map[string]string{
		"runbook_url": " <https://wiki.example/runbook> ",
	}))

	p := payloadOf(t, res)
	require.Len(t, p.Links, 1)
	assert.Equal(t, "https://wiki.example/runbook", p.Links[0].URL)
}

func TestAnAnnotationSetFullOfURLsIsCapped(t *testing.T) {
	t.Parallel()

	annotations := map[string]string{}
	for i := 0; i < runbook.MaxLinks*3; i++ {
		annotations["link_"+string(rune('a'+i%26))+string(rune('a'+i/26))] =
			"https://example.test/" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}

	res := enrich(t, runbook.New(nil), subject(annotations))
	p := payloadOf(t, res)
	assert.LessOrEqual(t, len(p.Links), runbook.MaxLinks,
		"an annotation set full of URLs is a templating accident, not context")
}

// ------------------------------------------------------------ org templates

func TestTheOrgTemplateIsAFallbackAndNotAnOverride(t *testing.T) {
	t.Parallel()

	e := runbook.New(runbook.StaticTemplates{
		"HighErrorRate": "https://wiki.example/org/{namespace}",
	})

	// Whoever wrote the annotation knew more about this alert than whoever wrote
	// the org-wide pattern.
	res := enrich(t, e, subject(map[string]string{"runbook_url": "https://wiki.example/mine"}))
	p := payloadOf(t, res)
	assert.Equal(t, "https://wiki.example/mine", p.Runbook)
	assert.Len(t, p.Links, 1, "the template is not added alongside")
}

func TestTheOrgTemplateFillsTheGapAndExpandsLabels(t *testing.T) {
	t.Parallel()

	e := runbook.New(runbook.StaticTemplates{
		"HighErrorRate": "https://wiki.example/org/{namespace}/{service}",
	})

	res := enrich(t, e, subject(nil))
	p := payloadOf(t, res)

	require.Len(t, p.Links, 1)
	assert.Equal(t, "https://wiki.example/org/payments/checkout", p.Runbook)
	assert.Equal(t, "org_template", p.Links[0].Source,
		"the source says where the link came from, so an operator can find it")
}

func TestATemplateWithNoPlaceholdersIsUsedVerbatim(t *testing.T) {
	t.Parallel()

	e := runbook.New(runbook.StaticTemplates{"HighErrorRate": "https://wiki.example/org/runbook"})

	p := payloadOf(t, enrich(t, e, subject(nil)))
	assert.Equal(t, "https://wiki.example/org/runbook", p.Runbook)
}

func TestALabelValueInATemplateIsPathEscaped(t *testing.T) {
	t.Parallel()

	s := subject(nil)
	s.Alert.Labels = map[string]string{"namespace": "team payments/eu"}
	e := runbook.New(runbook.StaticTemplates{"HighErrorRate": "https://wiki.example/{namespace}"})

	p := payloadOf(t, enrich(t, e, s))
	require.Len(t, p.Links, 1)
	assert.NotContains(t, p.Links[0].URL, " ", "a label value is data, not URL structure")
	assert.Contains(t, p.Links[0].URL, "%20")
}

// -------------------------------------------- the four rules that were defects

// TestAnUnresolvedPlaceholderIsRejectedRatherThanShippedAsAButton.
//
// `expand`'s contract says an unresolved placeholder "leaves the template
// unexpanded, which then fails normalisation and is reported as rejected —
// better than emitting a link with a literal `{namespace}` in the path and
// letting somebody click it" (runbook.go). It once did not.
// `validate.IsAbsoluteHTTPURL` accepted `{` and `}` in a path, `url.URL.String`
// percent-encoded them, and `https://wiki.example/org/%7Bregion%7D/runbook` was
// HOISTED INTO Payload.Runbook, which SPEC §F.2 renders as the button on the
// Slack card.
//
// That is the failure mode the enrichment pipeline is otherwise careful to
// avoid: not a missing field, but a field that LOOKS POPULATED AND IS WRONG. The
// operator at 3am clicks a button that 404s on their wiki, and nothing anywhere
// says the template could not be filled in. `normalise` now refuses a literal
// brace outright — see the ⛔ comment there for why a brace is unambiguous
// evidence of an unfilled placeholder rather than of escaped label data.
func TestAnUnresolvedPlaceholderIsRejectedRatherThanShippedAsAButton(t *testing.T) {
	t.Parallel()

	e := runbook.New(runbook.StaticTemplates{
		"HighErrorRate": "https://wiki.example/org/{region}/runbook",
	})

	res := enrich(t, e, subject(nil))
	p := payloadOf(t, res)

	assert.Empty(t, p.Runbook, "an unfillable template must not become a button")
	assert.NotContains(t, p.Runbook, "%7B")
	assert.Equal(t, domain.StatusPartial, res.Status)
	assert.Equal(t, []string{"org_template"}, p.Rejected)
}

// TestALinkKeepsItsQueryFragmentAndTrailingSlash.
//
// normalise used to document the opposite of what it did:
//
//	"the fragment and the query are preserved verbatim, because a dashboard link
//	 without its time range is a link to the wrong thing"
//
// while delegating to `validate.IsAbsoluteHTTPURL`, the predicate for
// `alert_sources.base_url` — a PREFIX oto concatenates API paths onto. That
// predicate deliberately refuses a query string, a fragment and a trailing
// slash, which is correct FOR A BASE URL. Reused here it refused the three
// commonest shapes an operator's runbook annotation actually takes, so the card
// lost its runbook button for a large fraction of real alerts and the payload
// blamed the operator's annotation for a bound belonging to a different column.
//
// `normalise` now uses `validate.IsOperatorLinkURL`, a predicate that exists
// precisely so these two columns cannot be conflated again.
func TestALinkKeepsItsQueryFragmentAndTrailingSlash(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"https://grafana.example/d/abc?from=now-1h&to=now",
		"https://wiki.example/runbook#step-2",
		"https://wiki.example/runbooks/",
	} {
		res := enrich(t, runbook.New(nil), subject(map[string]string{"runbook_url": raw}))
		p := payloadOf(t, res)
		assert.Equal(t, raw, p.Runbook, "a query, a fragment and a trailing slash are all legal in a link")
		assert.Empty(t, p.Rejected)
	}
}

// TestARejectedLinkIsReportedAsPartial.
//
// Enrich's own comment says a malformed annotation is "a real, fixable problem
// in somebody's rule file. Reporting it is more useful than hiding it", and it
// sets `partial` plus the `unusable_link_annotations` warning to do so. Two
// lines later it used to overwrite that unconditionally:
//
//	if len(links) == 0 {
//	    status = domain.StatusSkipped
//	}
//
// and the list is always empty in exactly the case the report is FOR: the
// alert's only link annotation was the broken one.
//
// The consequence was not cosmetic. `StatusSkipped` is documented as "Applicable
// said no", `Status.Succeeded()` is false for it and `Enrichment.Reusable` is
// false for it, so a run that DID look and DID find a broken annotation was
// filed under the same status as one that never ran. Every sibling enricher
// uses the other convention: alerthistory and promrule report `partial` with a
// warning whenever they looked and produced less than they wanted.
//
// The two branches are now ordered so a rejection wins over an empty list.
func TestARejectedLinkIsReportedAsPartial(t *testing.T) {
	t.Parallel()

	res := enrich(t, runbook.New(nil), subject(map[string]string{
		"runbook_url": "see the wiki",
	}))

	assert.Equal(t, domain.StatusPartial, res.Status,
		"the enricher looked, found a broken annotation and named it: that is a partial answer, not a declined one")
	assert.Contains(t, res.Warnings, "unusable_link_annotations")
	assert.Equal(t, []string{"runbook_url"}, payloadOf(t, res).Rejected)
}

// TestAnUppercaseSchemeIsAcceptedAndLowercased.
//
// `normalise` documents itself as accepting "absolute http(s) only" and ends
// with `u.Scheme = strings.ToLower(u.Scheme)`, which says plainly that the
// author expected a mixed-case scheme to be normalised. It never was:
//
//   - `looksLikeURL` tested `strings.HasPrefix(v, "https://")`, case-sensitively;
//   - `validate.HTTPURLRe` is `^https?://[^\s]+$`, with no `(?i)` — it mirrors a
//     Postgres CHECK on `alert_sources.base_url`, where case-sensitivity is
//     correct and deliberate, so it could not be loosened;
//   - and `net/url.Parse` lowercases the scheme itself, so the ToLower line
//     could never have fired even if the string got that far.
//
// RFC 3986 §3.1 makes a scheme case-insensitive, so `HTTPS://wiki/runbook` is a
// perfectly good link that this enricher filed as "unusable" while blaming the
// operator for it. Same root cause as
// TestALinkKeepsItsQueryFragmentAndTrailingSlash — a base-URL predicate reused
// on an operator's link — plus one case in runbook's own `looksLikeURL`. Both
// are fixed: `looksLikeURL` folds case, and `validate.IsOperatorLinkURL` carries
// the `(?i)` that HTTPURLRe may not.
func TestAnUppercaseSchemeIsAcceptedAndLowercased(t *testing.T) {
	t.Parallel()

	res := enrich(t, runbook.New(nil), subject(map[string]string{
		"runbook_url": "HTTPS://Wiki.Example/Runbooks/HighErrorRate",
	}))

	p := payloadOf(t, res)
	require.Len(t, p.Links, 1)
	assert.Equal(t, "https://wiki.example/Runbooks/HighErrorRate", p.Links[0].URL,
		"a scheme is case-insensitive (RFC 3986 §3.1); the path is not")
	assert.Empty(t, p.Rejected)
}

func TestATemplateForAnotherAlertIsNotApplied(t *testing.T) {
	t.Parallel()

	e := runbook.New(runbook.StaticTemplates{"SomethingElse": "https://wiki.example/other"})

	res := enrich(t, e, subject(nil))
	assert.Equal(t, domain.StatusSkipped, res.Status)
	assert.Empty(t, payloadOf(t, res).Links)
}

// -------------------------------------------- there is no upstream, and no clock

// TestItNeverFetchesAnyURL is the security property, stated as a test.
//
// The annotations below point at a REAL, running HTTP server that counts every
// request it receives. A single hit here would mean oto had grown an SSRF
// primitive reachable by anyone who can write an annotation into a rule file.
func TestItNeverFetchesAnyURL(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)

	res := enrich(t, runbook.New(runbook.StaticTemplates{"HighErrorRate": srv.URL + "/org"}),
		subject(map[string]string{
			"runbook_url":   srv.URL + "/runbook",
			"dashboard_url": srv.URL + "/dashboard",
			"logs_url":      srv.URL + "/logs",
			"other_url":     srv.URL + "/other",
		}))

	assert.Equal(t, domain.StatusOK, res.Status)
	assert.NotEmpty(t, payloadOf(t, res).Runbook, "the link is reported")
	assert.Zero(t, hits.Load(),
		"it NEVER FETCHES A URL: not to validate it, not to follow a redirect, not to check it is alive")
}

// TestAnExpiredContextChangesNothing. There is no upstream to time out against,
// so the enricher's answer must not depend on the deadline it was handed —
// anything else would be a latent I/O path.
func TestAnExpiredContextChangesNothing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	require.Error(t, ctx.Err(), "the budget is already gone")

	e := runbook.New(nil)
	s := subject(map[string]string{"runbook_url": "https://wiki.example/runbook"})

	withBudget := enrich(t, e, s)
	withoutBudget, err := e.Enrich(ctx, s)
	require.NoError(t, err, "a pure function cannot time out")
	assert.Equal(t, withBudget, withoutBudget)
}

// TestTheSubjectIsNotMutated. The pipeline hands each enricher its own copy, but
// an enricher that edits the alert it was given would still corrupt the payload
// it later reports.
func TestTheSubjectIsNotMutated(t *testing.T) {
	t.Parallel()

	s := subject(map[string]string{"runbook_url": " <https://Wiki.Example/runbook> "})
	enrich(t, runbook.New(nil), s)

	assert.Equal(t, " <https://Wiki.Example/runbook> ", s.Alert.Annotations["runbook_url"],
		"normalisation happens on the way out, never in place")
	assert.Equal(t, map[string]string{"namespace": "payments", "service": "checkout"}, s.Alert.Labels)
}
