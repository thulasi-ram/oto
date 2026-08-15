package promrule_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/enrichment/enrichers/promrule"
	"github.com/thulasiram/oto/internal/enrichment/service"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpc"
	"github.com/thulasiram/oto/internal/platform/id"
	rulesdomain "github.com/thulasiram/oto/internal/rules/domain"
	rulesservice "github.com/thulasiram/oto/internal/rules/service"
	"github.com/thulasiram/oto/internal/sources/client/prometheus"
	"github.com/thulasiram/oto/test/harness"
)

// prom.rule is the reason this module exists: every other alerting product shows
// you the alert, this one shows you the RULE THAT PRODUCED IT, as it was written
// at the moment it fired.
//
// The Snapshotter below is a test double for the `rules` service, but its
// upstream half is REAL: it drives oto's own Prometheus client against an
// httptest server, so "not found", "the server 500ed", "the server refused",
// "nothing is listening" and "the budget expired mid-request" are genuine HTTP
// outcomes rather than an interface returning a hand-written error.

var baseTime = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

const alertName = "HighErrorRate"

var (
	sourceID     = id.New()
	alertID      = id.New()
	occurrenceID = id.New()
	snapshotID   = id.New()
)

// ------------------------------------------------------- the rules-service double

// snapshotter is `promrule.Snapshotter`, implemented over the real Prometheus
// client. It does what `rules/service.Capture` does — recover, content-address,
// degrade — without needing a database for it.
type snapshotter struct {
	client *prometheus.Client

	// The three facts the enricher reads but does not compute.
	drifted             bool
	previousFingerprint string

	calls  int
	sawReq rulesservice.CaptureRequest
}

func (s *snapshotter) Capture(
	ctx context.Context, scope db.TenantScope, req rulesservice.CaptureRequest,
) (rulesservice.Capture, error) {
	s.calls++
	s.sawReq = req

	name := req.Labels["alertname"]
	groups, err := s.client.Rules(ctx, []string{name})
	if err != nil {
		// A storage or transport failure is one of the two things `rules`
		// returns an error for. Everything else degrades below.
		return rulesservice.Capture{}, err
	}

	rec := rulesdomain.Recovery{
		Origin:     rulesdomain.OriginUnavailable,
		Strategy:   rulesdomain.StrategyNone,
		Confidence: rulesdomain.ConfidenceNone,
		Notes:      []string{},
	}
	key := rulesdomain.Key{SourceID: req.SourceID.String(), Name: name}

	count := 0
	for _, g := range groups {
		for _, r := range g.Rules {
			if r.Name != name {
				continue
			}
			count++
			if count > 1 {
				continue
			}
			key.File, key.Group = g.File, g.Name
			rec = rulesdomain.Recovery{
				Origin:               rulesdomain.OriginPrometheusAPI,
				Strategy:             rulesdomain.StrategyRulesAPI,
				Confidence:           rulesdomain.ConfidenceExact,
				CandidateCount:       1,
				RuleName:             r.Name,
				RuleGroup:            g.Name,
				RuleFile:             g.File,
				Expr:                 r.Query,
				ForSeconds:           r.Duration,
				KeepFiringForSeconds: r.KeepFiringFor,
				Labels:               r.Labels,
				Annotations:          r.Annotations,
				PrometheusURL:        s.client.BaseURL(),
				Notes:                []string{},
			}
		}
	}
	switch {
	case count == 0:
		// "oto looked and could not see it" is a legitimate result, not an error.
		rec.Notes = []string{"rule_not_found"}
	case count > 1:
		// SPEC §D.6: an ambiguous match MUST reach the UI and the Slack card.
		rec.Confidence, rec.CandidateCount = rulesdomain.ConfidenceAmbiguous, count
		rec.Notes = []string{"duplicate_alertname"}
	}

	snap := rulesdomain.NewSnapshot(scope.OrgID().String(), key, rec, baseTime)
	snap.ID = snapshotID.String()

	return rulesservice.Capture{
		Snapshot:            snap,
		Recovery:            rec,
		NewVersion:          true,
		Drifted:             s.drifted,
		PreviousFingerprint: s.previousFingerprint,
		Warnings:            append([]string{}, rec.Notes...),
	}, nil
}

// binder is `alert_occurrences.rule_snapshot_id`, and its failure mode.
type binder struct {
	err   error
	calls int
	sawID uuid.UUID
}

func (b *binder) BindRuleSnapshot(
	_ context.Context, _ db.TenantScope, _, snapID uuid.UUID,
) error {
	b.calls++
	b.sawID = snapID
	return b.err
}

// -------------------------------------------------------------------- fixtures

func subject() *domain.Subject {
	return &domain.Subject{
		OrgID:       id.NewString(),
		SubjectKind: domain.SubjectOccurrence,
		SubjectID:   occurrenceID.String(),
		Alert: domain.AlertSnapshot{
			ID:           alertID.String(),
			AlertKey:     "ak_abcdefghijklmnopqrstuvwxyz",
			AlertName:    alertName,
			Severity:     "critical",
			Labels:       map[string]string{"alertname": alertName, "severity": "critical"},
			Annotations:  map[string]string{"summary": "errors are up"},
			GeneratorURL: "https://prom.example/graph?g0.expr=rate(errors[5m])",
		},
		Occurrence: domain.OccurrenceSnapshot{ID: occurrenceID.String(), StartedAt: baseTime},
		Source:     domain.SourceRef{ID: sourceID.String(), Kind: "alertmanager"},
	}
}

func scoped(t *testing.T) context.Context {
	t.Helper()
	s, err := db.NewTenantScope(id.New())
	require.NoError(t, err)
	return service.WithScope(context.Background(), s)
}

// clientAt builds oto's real Prometheus client against an arbitrary base URL.
// One attempt, no retry backoff: a test must never wait on a sleeper.
func clientAt(t *testing.T, baseURL string) *prometheus.Client {
	t.Helper()
	c, err := prometheus.New(prometheus.Config{
		BaseURL:   strings.TrimSuffix(baseURL, "/"),
		Clock:     clock.NewFake(baseTime),
		UserAgent: "oto-test",
	})
	require.NoError(t, err)
	return c
}

func payloadOf(t *testing.T, res domain.Result) promrule.Payload {
	t.Helper()
	p, ok := res.Payload.(promrule.Payload)
	require.True(t, ok, "the payload is the enricher's own typed struct")
	return p
}

// firing is one alerting rule as Prometheus puts it on the wire. Duration is
// FLOAT SECONDS: 600 means `for: 10m`.
func firing() harness.PromRuleGroup {
	return harness.PromRuleGroup{
		Name: "checkout",
		File: "/etc/prometheus/rules/checkout.yml",
		Rules: []harness.PromRule{
			harness.AlertingRule(alertName, `rate(http_errors_total[5m]) > 0.05`, 600),
		},
	}
}

// ------------------------------------------------------------------ the ports

func TestTheRegistryContractIsStable(t *testing.T) {
	t.Parallel()

	e := promrule.New(&snapshotter{}, nil)

	assert.Equal(t, "prom.rule", e.Name())
	assert.True(t, domain.ValidEnricherName(e.Name()),
		"the dot is mandatory: enrichments_name_ck rejects a bare word")
	assert.Equal(t, 1, e.Version())
	assert.Equal(t, domain.PhaseInline, e.Phase(),
		"the rule is the single most useful piece of context on the FIRST card")
	assert.Equal(t, 800*time.Millisecond, e.Timeout())
	assert.Equal(t, 5*time.Minute, promrule.CacheTTL,
		"five minutes bounds how long oto can be wrong about a rule change to under one repeat interval")
	assert.Less(t, e.Timeout(), domain.InlineBudget,
		"one enricher must fit inside the ceiling it shares")
}

func TestApplicableRequiresAnAlertname(t *testing.T) {
	t.Parallel()

	e := promrule.New(&snapshotter{}, nil)

	assert.True(t, e.Applicable(subject()))
	assert.False(t, e.Applicable(nil))

	// The alertname IS the rule name; a lookup by anything else would be a guess.
	nameless := subject()
	nameless.Alert.AlertName = ""
	assert.False(t, e.Applicable(nameless))
}

// TestCacheSeedFixesTheQuestionAndNotTheOccurrence. A cache keyed by occurrence
// would never hit.
func TestCacheSeedFixesTheQuestionAndNotTheOccurrence(t *testing.T) {
	t.Parallel()

	e := promrule.New(&snapshotter{}, nil)

	other := subject()
	other.Occurrence.ID = id.NewString()
	other.SubjectID = other.Occurrence.ID
	assert.Equal(t, e.CacheSeed(subject()), e.CacheSeed(other),
		"a second fire of the same alert asks the same question")

	moved := subject()
	moved.Alert.GeneratorURL = "https://prom.example/graph?g0.expr=something_else"
	assert.NotEqual(t, e.CacheSeed(subject()), e.CacheSeed(moved),
		"the generatorURL is the primary path's whole input, so it is part of the question")

	assert.Empty(t, e.CacheSeed(nil))
}

// --------------------------------------------------------------------- found

// TestAFoundRuleIsCapturedPinnedAndProjected drives the whole happy path over
// real HTTP.
func TestAFoundRuleIsCapturedPinnedAndProjected(t *testing.T) {
	t.Parallel()

	prom := harness.NewPrometheus(t)
	prom.SetRuleGroups(firing())

	snaps := &snapshotter{client: prom.Client(clock.NewFake(baseTime))}
	bind := &binder{}

	res, err := promrule.New(snaps, bind).Enrich(scoped(t), subject())
	require.NoError(t, err)

	assert.Equal(t, domain.StatusOK, res.Status)
	assert.Equal(t, promrule.CacheTTL, res.TTL)
	assert.Empty(t, res.Warnings)

	p := payloadOf(t, res)
	assert.True(t, p.Available)
	assert.Equal(t, `rate(http_errors_total[5m]) > 0.05`, p.Expr,
		"the expression is what a human reads in the first three seconds")
	assert.InDelta(t, 600, p.ForSeconds, 1e-9, "float SECONDS: 600 means for: 10m")
	assert.Equal(t, alertName, p.RuleName)
	assert.Equal(t, "checkout", p.RuleGroup)
	assert.Equal(t, "/etc/prometheus/rules/checkout.yml", p.RuleFile)
	assert.Equal(t, snapshotID.String(), p.SnapshotID)
	assert.Regexp(t, `^[0-9a-f]{64}$`, p.Fingerprint, "the content address of the definition (§C.6)")
	assert.Equal(t, string(rulesdomain.OriginPrometheusAPI), p.Origin, "provenance, always")
	assert.Equal(t, string(rulesdomain.ConfidenceExact), p.Confidence)
	assert.Equal(t, 1, p.CandidateCount)
	assert.False(t, p.Drifted)

	// The snapshot is pinned to the occurrence, which is what makes "show me the
	// rule as it was when THIS fired" a single join.
	assert.Equal(t, 1, bind.calls)
	assert.Equal(t, snapshotID, bind.sawID)

	// The request really went out, filtered rather than paged.
	requests := prom.Requests()
	require.Len(t, requests, 1)
	assert.Contains(t, requests[0], "rule_name%5B%5D="+alertName)
	assert.Contains(t, requests[0], "exclude_alerts=true")

	// And the capture was asked the right question.
	assert.Equal(t, sourceID, snaps.sawReq.SourceID)
	assert.Equal(t, alertID, snaps.sawReq.AlertID)
	assert.Equal(t, occurrenceID, snaps.sawReq.OccurrenceID)
	assert.Equal(t, subject().Alert.GeneratorURL, snaps.sawReq.GeneratorURL)
}

// TestAnAmbiguousMatchIsSurfacedAndNeverHidden.
//
// Hiding it would turn "we picked one of three rules with this name" into "here
// is your rule", which is the single most misleading thing this module could do.
func TestAnAmbiguousMatchIsSurfacedAndNeverHidden(t *testing.T) {
	t.Parallel()

	prom := harness.NewPrometheus(t)
	prom.SetRuleGroups(
		firing(),
		harness.PromRuleGroup{
			Name: "checkout-canary",
			File: "/etc/prometheus/rules/canary.yml",
			Rules: []harness.PromRule{
				harness.AlertingRule(alertName, `rate(http_errors_total[1m]) > 0.10`, 60),
			},
		},
	)

	res, err := promrule.New(&snapshotter{client: prom.Client(clock.NewFake(baseTime))}, nil).
		Enrich(scoped(t), subject())
	require.NoError(t, err)

	assert.Equal(t, domain.StatusOK, res.Status)
	assert.Contains(t, res.Warnings, "ambiguous_rule_match")
	assert.Contains(t, res.Warnings, "duplicate_alertname", "the reason code travels with it")

	p := payloadOf(t, res)
	assert.Equal(t, string(rulesdomain.ConfidenceAmbiguous), p.Confidence)
	assert.Equal(t, 2, p.CandidateCount, "the honest count, not the one it picked")
	assert.True(t, p.Available)
}

// TestADriftedRuleIsNeverServedFromACachePredatingTheChange.
func TestADriftedRuleIsNeverServedFromACachePredatingTheChange(t *testing.T) {
	t.Parallel()

	prom := harness.NewPrometheus(t)
	prom.SetRuleGroups(firing())

	snaps := &snapshotter{
		client:              prom.Client(clock.NewFake(baseTime)),
		drifted:             true,
		previousFingerprint: strings.Repeat("a", 64),
	}

	res, err := promrule.New(snaps, nil).Enrich(scoped(t), subject())
	require.NoError(t, err)

	assert.Equal(t, domain.StatusOK, res.Status)
	assert.Contains(t, res.Warnings, "rule_definition_changed")
	assert.Zero(t, res.TTL,
		"the one feature whose entire claim is that it tells you when the rule CHANGED may not cache across the change")

	p := payloadOf(t, res)
	assert.True(t, p.Drifted)
	assert.Equal(t, strings.Repeat("a", 64), p.PreviousFingerprint, "what to diff against")
	assert.True(t, p.NewVersion)
}

// TestAFailedPinIsAWarningAndNotAFailure. A capture that is stored but not
// pinned still answers "what did this rule say"; a capture refused because the
// pin failed answers nothing.
func TestAFailedPinIsAWarningAndNotAFailure(t *testing.T) {
	t.Parallel()

	prom := harness.NewPrometheus(t)
	prom.SetRuleGroups(firing())

	bind := &binder{err: errs.New(errs.KindInternal, "alerts_write_failed", "the row is locked")}

	res, err := promrule.New(&snapshotter{client: prom.Client(clock.NewFake(baseTime))}, bind).
		Enrich(scoped(t), subject())
	require.NoError(t, err)

	assert.Equal(t, domain.StatusOK, res.Status)
	assert.Contains(t, res.Warnings, "snapshot_not_bound")
	assert.True(t, payloadOf(t, res).Available, "the rule itself still reaches the card")
	assert.Equal(t, 1, bind.calls)
}

func TestANilBinderSimplyDoesNotPin(t *testing.T) {
	t.Parallel()

	prom := harness.NewPrometheus(t)
	prom.SetRuleGroups(firing())

	res, err := promrule.New(&snapshotter{client: prom.Client(clock.NewFake(baseTime))}, nil).
		Enrich(scoped(t), subject())
	require.NoError(t, err)

	assert.Equal(t, domain.StatusOK, res.Status)
	assert.True(t, payloadOf(t, res).Available)
}

// ----------------------------------------------------------------- not found

// TestARuleThatCannotBeRecoveredIsRecordedNotSilent.
//
// The operator learns "the rule could not be recovered", which is a fact, rather
// than seeing an empty panel and wondering whether oto simply did not try.
func TestARuleThatCannotBeRecoveredIsRecordedNotSilent(t *testing.T) {
	t.Parallel()

	prom := harness.NewPrometheus(t)
	prom.SetRuleGroups(harness.PromRuleGroup{
		Name:  "other",
		File:  "/etc/prometheus/rules/other.yml",
		Rules: []harness.PromRule{harness.AlertingRule("SomethingElse", "up == 0", 60)},
	})

	bind := &binder{}
	res, err := promrule.New(&snapshotter{client: prom.Client(clock.NewFake(baseTime))}, bind).
		Enrich(scoped(t), subject())
	require.NoError(t, err, "a rule that cannot be recovered is NOT an error")

	assert.Equal(t, domain.StatusPartial, res.Status)
	assert.Contains(t, res.Warnings, "rule_unavailable")
	assert.Contains(t, res.Warnings, "rule_not_found")
	assert.Zero(t, res.TTL,
		"not cached: the next fire should try again rather than inherit a \"we could not see it\"")

	p := payloadOf(t, res)
	assert.False(t, p.Available, "the payload states plainly that there is no rule here")
	assert.Empty(t, p.Expr, "and it invents nothing to fill the panel with")
	assert.Equal(t, string(rulesdomain.OriginUnavailable), p.Origin)
	assert.Equal(t, string(rulesdomain.ConfidenceNone), p.Confidence)
	assert.Zero(t, p.CandidateCount)
	assert.Zero(t, p.ForSeconds)

	assert.Zero(t, bind.calls,
		"an unavailable snapshot is not pinned to the occurrence: there is nothing to pin")
}

// TestAnAlertWithNoSourceIsSkippedNotFailed.
func TestAnAlertWithNoSourceIsSkippedNotFailed(t *testing.T) {
	t.Parallel()

	snaps := &snapshotter{}
	s := subject()
	s.Source.ID = ""

	res, err := promrule.New(snaps, nil).Enrich(scoped(t), s)
	require.NoError(t, err)

	assert.Equal(t, domain.StatusSkipped, res.Status)
	assert.Equal(t, []string{"no_alert_source"}, res.Warnings)
	assert.Nil(t, res.Payload)
	assert.Zero(t, snaps.calls, "no source means nothing to ask")
}

// ------------------------------------------------------------ upstream error

// TestAnUpstreamServerErrorIsAnErrorAndNothingElse. A 500 from somebody else's
// Prometheus is a real HTTP outcome, and what must not come back with it is a
// half-filled Payload that renders as a rule.
func TestAnUpstreamServerErrorIsAnErrorAndNothingElse(t *testing.T) {
	t.Parallel()

	prom := harness.NewPrometheus(t)
	prom.SetRuleGroups(firing())
	prom.FailWith(http.StatusInternalServerError)

	bind := &binder{}
	res, err := promrule.New(&snapshotter{client: prom.Client(clock.NewFake(baseTime))}, bind).
		Enrich(scoped(t), subject())

	require.Error(t, err)
	assert.Equal(t, domain.Result{}, res,
		"an error carries the ZERO result: nothing to render, nothing stale, nothing wrong")
	assert.Zero(t, bind.calls, "and nothing is pinned to the occurrence")
	assert.Equal(t, errs.KindUpstreamDown, errs.KindOf(err),
		"a Prometheus that is down is an upstream failure, not oto's")
}

// TestAnUpstreamRefusalIsAnErrorAndNothingElse: Prometheus REFUSING is a 200
// carrying `{"status":"error"}`. It really does answer this way, and the
// distinction from "down" is carried in oto's error taxonomy.
func TestAnUpstreamRefusalIsAnErrorAndNothingElse(t *testing.T) {
	t.Parallel()

	prom := harness.NewPrometheus(t)
	prom.RefuseWith("bad_data: parse error at char 3")

	res, err := promrule.New(&snapshotter{client: prom.Client(clock.NewFake(baseTime))}, nil).
		Enrich(scoped(t), subject())

	require.Error(t, err)
	assert.Equal(t, domain.Result{}, res)
	assert.Equal(t, "prometheus_api_error", errs.CodeOf(err),
		"a refusal gets its own code: \"your rule_name filter was rejected\" must stay "+
			"separable from \"the process is down\"")
	assert.Equal(t, errs.KindUpstreamDown, errs.KindOf(err))
}

// TestAnUpstreamThatIsNotListeningIsAnErrorAndNothingElse.
func TestAnUpstreamThatIsNotListeningIsAnErrorAndNothingElse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening on that port any more

	res, err := promrule.New(&snapshotter{client: clientAt(t, url)}, nil).Enrich(scoped(t), subject())

	require.Error(t, err)
	assert.Equal(t, domain.Result{}, res)
	assert.True(t, httpc.IsUnreachable(err))
}

// TestAnEnricherWithoutAScopeFailsRatherThanCapturingUnscoped.
func TestAnEnricherWithoutAScopeFailsRatherThanCapturingUnscoped(t *testing.T) {
	t.Parallel()

	snaps := &snapshotter{}

	res, err := promrule.New(snaps, nil).Enrich(context.Background(), subject())

	require.Error(t, err)
	assert.Equal(t, "enrichment_no_tenant_scope", errs.CodeOf(err))
	assert.Equal(t, domain.Result{}, res)
	assert.Zero(t, snaps.calls)
}

// ------------------------------------------------------------------ timeout

// TestAnAlreadySpentBudgetProducesNoResultAtAll.
//
// The deadline is set in the past, so this costs zero wall time and still
// exercises the real transport: the request is built, the round trip is refused
// by the context, and the client classifies it as a timeout.
func TestAnAlreadySpentBudgetProducesNoResultAtAll(t *testing.T) {
	t.Parallel()

	prom := harness.NewPrometheus(t)
	prom.SetRuleGroups(firing())

	ctx, cancel := context.WithDeadline(scoped(t), baseTime.Add(-time.Hour))
	defer cancel()

	bind := &binder{}
	res, err := promrule.New(&snapshotter{client: prom.Client(clock.NewFake(baseTime))}, bind).
		Enrich(ctx, subject())

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"the pipeline reads this to record `timeout` rather than `failed`, and they earn different remedies")
	assert.True(t, httpc.HasCode(err, httpc.CodeTimeout))
	assert.Equal(t, domain.Result{}, res, "no rule, no stale rule, no half a rule")
	assert.Zero(t, bind.calls)
	assert.Empty(t, prom.Requests(), "an expired budget does not even reach the upstream")
}

// TestAnInFlightRequestIsAbandonedWhenTheBudgetIsWithdrawn is the other half:
// the upstream ANSWERED the connection and then went quiet.
//
// The handler blocks until its own request context is done, and the test
// withdraws the budget the instant the server confirms it is inside the handler.
// There is no sleep and no timing assumption anywhere in it.
func TestAnInFlightRequestIsAbandonedWhenTheBudgetIsWithdrawn(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(scoped(t))
	defer cancel()
	go func() {
		<-entered
		cancel()
	}()

	bind := &binder{}
	res, err := promrule.New(&snapshotter{client: clientAt(t, srv.URL)}, bind).Enrich(ctx, subject())

	require.Error(t, err, "a wedged Prometheus must not wedge the enricher")
	assert.True(t, errors.Is(err, context.Canceled))
	assert.Equal(t, domain.Result{}, res)
	assert.Zero(t, bind.calls)
}

// TestAMalformedUpstreamAnswerIsAnErrorAndNothingElse: a proxy serving HTML is
// not a Prometheus, and it must not be decoded into an empty rule.
func TestAMalformedUpstreamAnswerIsAnErrorAndNothingElse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>corporate proxy login</body></html>"))
	}))
	t.Cleanup(srv.Close)

	res, err := promrule.New(&snapshotter{client: clientAt(t, srv.URL)}, nil).Enrich(scoped(t), subject())

	require.Error(t, err)
	assert.Equal(t, domain.Result{}, res)
	assert.True(t, httpc.IsMalformed(err),
		"a malformed source is NOT a down source: the operator must fix the URL, not restart the service")
}
