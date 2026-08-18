package api

// THE ALERTS TRANSPORT, CHECKED AGAINST THE CONTRACT ITSELF.
//
// ⭐ NOTHING HERE RE-STATES A RESPONSE SHAPE BY HAND. Every success body is
// handed to `schema.Assert`, which compiles the JSON Schema
// `api/openapi/openapi.yaml` declares for that operationId and that status and
// validates the bytes the handler actually wrote. A test that spelled the shape
// out a second time would be a second copy of the contract, and a second copy
// drifts exactly the way the DTOs did — which is how the conformance audit found
// a `delivery_summary` that was declared, never emitted, and passed every
// validator in the building.
//
// The fixtures come from world_test.go, which builds one tenant's alerts,
// cases, events, enrichments, notifications and snoozes through the real
// domain constructors, and which answers a NOT-FOUND for every id that tenant
// does not own — exactly as the repository does under `db.TenantScope`, where the
// predicate is `WHERE org_id = $scope AND id = $id` and another tenant's row does
// not come back forbidden, it does not come back at all.
//
// The properties this file protects:
//
//   - the {data,page,meta} envelope on every read, `meta.request_id` included;
//   - the three human verbs refuse a non-human principal (§E.1.1) — an ack
//     attributed to "system" is a receipt nobody signed;
//   - a snooze must END: neither spelling, or both, is a refusal with the field
//     named (§B.8.3), because an unexpiring snooze is a mute;
//   - a delivery roll-up that could not be read FAILS the request rather than
//     rendering an alert page that quietly claims silence it never checked;
//   - and, above all, that another tenant's id answers 404 on all fifteen
//     id-addressed operations — never 403, which would confirm the row exists.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// alertPath is the fixture alert's collection path, spelled once.
func alertPath(suffix string) string { return "/alerts/" + fxAlertID.String() + suffix }

// casePath is the fixture case's path.
func casePath(suffix string) string { return "/cases/" + fxCase.String() + suffix }

// sendJSON posts a body the CONTRACT would accept.
//
// ⭐ The fixture is validated against the request schema before it is sent, so a
// test can never pass on a body no real client would be allowed to send — which
// is the other half of the drift this suite exists to catch.
func sendJSON(t *testing.T, c *apitest.Client, op, method, target, body string) *apitest.Response {
	t.Helper()
	schema.AssertRequest(t, op, []byte(body))
	return c.Raw(method, target, apitest.ContentTypeJSON, body)
}

/* -------------------------------------------------------------------------- */
/* Reads                                                                      */
/* -------------------------------------------------------------------------- */

// TestGetAlertAnswersTheDetailTheContractDeclares.
//
// The promise: `GET /api/v1/alerts/{id}` answers an `AlertDetailDTO` — the alert,
// its current case, the snooze in force, the enrichment summary and the
// delivery roll-up — under BOTH spellings of the id, the UUID and the §C.2
// alert_key.
//
// What broke when it did not hold: the audit found `delivery_summary` declared on
// this schema and emitted by nothing. It was optional, so every validator passed,
// and an alert page rendered without it looked exactly like an alert nobody
// needed to be told about. The key spelling matters for a different reason: the
// key is the handle Slack buttons and pasted URLs carry, so a detail endpoint that
// only accepts a UUID turns every one of those into a dead link.
func TestGetAlertAnswersTheDetailTheContractDeclares(t *testing.T) {
	t.Parallel()

	for _, spelling := range []struct{ name, id string }{
		{"by uuid", fxAlertID.String()},
		{"by alert_key", fxAlertKey(t)},
	} {
		t.Run(spelling.name, func(t *testing.T) {
			t.Parallel()

			c, svc := newAlertsProbe(t)
			resp := c.GET("/alerts/"+spelling.id).MustStatus(t, http.StatusOK)
			schema.Assert(t, "getAlert", http.StatusOK, resp.Body())

			if ct := resp.Header("Content-Type"); ct != apitest.ContentTypeJSON {
				t.Fatalf("Content-Type = %q, want %q", ct, apitest.ContentTypeJSON)
			}
			// ⛔ The tenant came off the principal and nowhere else.
			if svc.lastScope.OrgID() != apitest.OrgID {
				t.Fatalf("the handler read as org %s, want the caller's %s",
					svc.lastScope.OrgID(), apitest.OrgID)
			}
			// The roll-up is read for every rendering, not only some of them.
			if svc.calls["DeliveryRollupForAlert"] != 1 {
				t.Fatalf("the delivery roll-up was read %d time(s), want 1",
					svc.calls["DeliveryRollupForAlert"])
			}
		})
	}
}

// ⭐ TestAnAlertPageThatCannotReadItsEnrichmentsFailsRatherThanRenderWithoutThem.
//
// The promise: when a sub-read the detail view depends on fails, the REQUEST
// fails. The handler does not drop the member and answer 200.
//
// What broke: a 200 missing a field a caller cannot distinguish from "there was
// nothing to say" is oto claiming silence it has not checked — the exact false
// negative the whole product exists to prevent.
func TestAnAlertPageThatCannotReadItsEnrichmentsFailsRatherThanRenderWithoutThem(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)
	svc.failEnrichments = errs.Internal("enrichment_read_failed", errs.ErrInternal)

	resp := c.GET(alertPath("")).MustStatus(t, http.StatusInternalServerError)
	schema.AssertProblem(t, "getAlert", http.StatusInternalServerError, resp.Body())
}

// TestListAlertCasesAnswersThePageTheContractDeclares.
//
// The promise: the episode list is a keyset page of `CaseDTO`, and it
// renders an OPEN, an ENDED and a SUPPRESSED episode with the same schema.
//
// What broke: `ended_at`, `resolve_reason` and `suppression_reason` only exist on
// some rows. A test over a single open case proves that `null` validates
// and nothing else.
func TestListAlertCasesAnswersThePageTheContractDeclares(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)
	resp := c.GET(alertPath("/cases")).MustStatus(t, http.StatusOK)
	schema.Assert(t, "listAlertCases", http.StatusOK, resp.Body())

	rows, ok := resp.JSON(t)["data"].([]any)
	if !ok || len(rows) != 3 {
		t.Fatalf("data = %v, want the open, ended and suppressed episodes: %s",
			resp.JSON(t)["data"], resp)
	}
	if svc.calls["Cases"] != 1 {
		t.Fatalf("the service was consulted %d time(s), want 1", svc.calls["Cases"])
	}
}

// TestListCasesAnswersThePageTheContractDeclares.
//
// The promise: `GET /api/v1/cases` is a keyset page of `CaseListItemDTO` — an
// episode plus the identity it belongs to — and it renders an OPEN, an ENDED and
// a SUPPRESSED episode under one schema.
//
// ⛔ WHAT IT REALLY GUARDS IS `alert`. The whole reason this endpoint is not
// `GET /alerts?ack=` is that an operator triages by name and severity, and those
// are columns of `alerts` rather than of the episode. The contract declares the
// reference REQUIRED, so a row that shipped without it would fail here rather
// than in a browser rendering a list of bare uuids.
func TestListCasesAnswersThePageTheContractDeclares(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)
	// `?state=open` is the live queue's spelling since ADR 0040 collapsed `?open=`
	// into it; the fake service ignores the filter and returns all three rows,
	// which is what lets one request exercise the whole schema.
	resp := c.GET("/cases?state=open&ack=unacked").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listCases", http.StatusOK, resp.Body())

	rows, ok := resp.JSON(t)["data"].([]any)
	if !ok || len(rows) != 3 {
		t.Fatalf("data = %v, want the open, ended and suppressed episodes: %s",
			resp.JSON(t)["data"], resp)
	}
	for i, row := range rows {
		r, _ := row.(map[string]any)
		if _, present := r["alert"]; !present {
			t.Fatalf("row %d carries no `alert`; the list would need one request per "+
				"row to be readable, which is the N+1 this shape exists to avoid", i)
		}
	}
	if svc.calls["ListCases"] != 1 {
		t.Fatalf("the service was consulted %d time(s), want 1", svc.calls["ListCases"])
	}
	// ⛔ The tenant came off the principal and nowhere else.
	if svc.lastScope.OrgID() != apitest.OrgID {
		t.Fatalf("the handler read as org %s, want the caller's %s",
			svc.lastScope.OrgID(), apitest.OrgID)
	}
}

// ⛔ TestTheCaseListRefusesAParameterItDoesNotServe.
//
// The promise: `?q=`, `?label[…]=` and `?snoozed=` are `400 unknown_parameter` on
// this endpoint, not silently ignored.
//
// What breaks otherwise: those three ARE served on `GET /alerts`, so a caller
// copying a filter bar across would get an unfiltered page of episodes that looks
// exactly like a filtered one. A triage queue that quietly widens is how an
// operator concludes there is nothing left to look at.
func TestTheCaseListRefusesAParameterItDoesNotServe(t *testing.T) {
	t.Parallel()

	for _, q := range []string{"q=oom", "label[team]=core", "snoozed=true", "sort=-started_at"} {
		t.Run(q, func(t *testing.T) {
			t.Parallel()

			c, svc := newAlertsProbe(t)
			resp := c.GET("/cases?"+q).MustStatus(t, http.StatusBadRequest)
			schema.AssertProblem(t, "listCases", http.StatusBadRequest, resp.Body())

			if len(svc.calls) != 0 {
				t.Fatalf("an unserved parameter reached the service: %v", svc.calls)
			}
		})
	}
}

// TestTheCaseListBindsItsCursorToItsFilter (§E.1).
//
// The promise: a cursor minted under one filter and replayed against another is
// refused. What it protects is a page served from the middle of a list that no
// longer exists, with nothing about the response looking wrong.
func TestTheCaseListBindsItsCursorToItsFilter(t *testing.T) {
	t.Parallel()

	c, _ := newAlertsProbe(t)
	first := c.GET("/cases?ack=unacked").MustStatus(t, http.StatusOK)
	page, _ := first.JSON(t)["page"].(map[string]any)
	next, _ := page["next_cursor"].(string)
	if next == "" {
		t.Fatal("the first page minted no cursor; there is nothing to replay")
	}

	resp := c.GET("/cases?ack=acked&cursor="+next).MustStatus(t, http.StatusBadRequest)
	if code := resp.Problem(t).Code; code != "cursor_filter_mismatch" {
		t.Fatalf("code = %q, want cursor_filter_mismatch", code)
	}
}

// TestListAlertEnrichmentsKeepsAFailedResultDistinguishableFromAMissingOne.
//
// The promise: every enrichment is stamped with its producer, version, phase,
// duration and cache provenance, and a FAILED one is recorded rather than
// discarded — that is what `status` is for.
//
// What broke: an enricher that silently vanished on failure is an enricher whose
// absence reads as "nothing to add". An operator cannot act on a difference they
// cannot see.
func TestListAlertEnrichmentsKeepsAFailedResultDistinguishableFromAMissingOne(t *testing.T) {
	t.Parallel()

	c, _ := newAlertsProbe(t)
	resp := c.GET(alertPath("/enrichments")).MustStatus(t, http.StatusOK)
	schema.Assert(t, "listAlertEnrichments", http.StatusOK, resp.Body())

	rows, ok := resp.JSON(t)["data"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("data = %v, want the successful and the failed enrichment: %s",
			resp.JSON(t)["data"], resp)
	}
	var statuses []string
	for _, row := range rows {
		r, _ := row.(map[string]any)
		s, _ := r["status"].(string)
		statuses = append(statuses, s)
	}
	if !contains(statuses, "failed") {
		t.Fatalf("statuses = %v; a failed enrichment must stay visible", statuses)
	}
}

// TestListAlertNotificationsSaysWhetherAnybodyWasActuallyTold.
//
// The promise: every notification row carries its delivery counts and, when it
// was suppressed, the REASON — in oto's own suppression vocabulary rather than
// Alertmanager's.
//
// What broke: the audit found a `NotificationReason` the server emitted and the
// contract rejected. A single boolean "notified" cannot distinguish "delivered",
// "suppressed because snoozed" and "every channel failed", and those are the three
// facts an operator is actually asking about.
func TestListAlertNotificationsSaysWhetherAnybodyWasActuallyTold(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)
	resp := c.GET(alertPath("/notifications")).MustStatus(t, http.StatusOK)
	schema.Assert(t, "listAlertNotifications", http.StatusOK, resp.Body())

	rows, ok := resp.JSON(t)["data"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("data = %v, want the delivered and the suppressed intent: %s",
			resp.JSON(t)["data"], resp)
	}
	if svc.calls["Notifications"] != 1 {
		t.Fatalf("the service was consulted %d time(s), want 1", svc.calls["Notifications"])
	}
}

// TestListAlertSnoozesRendersTheQuietPeriodAsHistoryRatherThanABoolean — §B.8.6.
//
// The promise: the per-alert snooze list carries the ACTIVE quiet period and the
// ones that have ended, each attributable and bounded.
//
// What broke: membership of a snooze modelled as a boolean cannot say who asked,
// why, or until when, and cannot be reviewed after the fact. That is what makes
// the difference between a snooze and a mute, and mutes are how channels die.
func TestListAlertSnoozesRendersTheQuietPeriodAsHistoryRatherThanABoolean(t *testing.T) {
	t.Parallel()

	c, _ := newAlertsProbe(t)
	resp := c.GET(alertPath("/snoozes")).MustStatus(t, http.StatusOK)
	schema.Assert(t, "listAlertSnoozes", http.StatusOK, resp.Body())

	rows, ok := resp.JSON(t)["data"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("data = %v, want the active snooze and the one somebody ended early: %s",
			resp.JSON(t)["data"], resp)
	}
	// The ended row is the only one that can carry an ended_reason, and it must.
	ended := 0
	for _, row := range rows {
		r, _ := row.(map[string]any)
		if r["ended_reason"] != nil {
			ended++
		}
	}
	if ended != 1 {
		t.Fatalf("%d row(s) carry an ended_reason, want exactly 1: %s", ended, resp)
	}
}

// ⭐ TestListActiveSnoozesStillListsASnoozeWhoseAlertCouldNotBeRead — §B.8.6.
//
// The promise: `GET /api/v1/snoozes` is the org-wide banner of everything oto is
// currently quiet about, and a row whose Alert could not be loaded is listed
// anyway with a null `alert`.
//
// What broke: dropping the row hides a quiet period, which is the exact failure
// this endpoint exists to prevent. A banner that omits a snooze is worse than no
// banner, because it asserts completeness it does not have.
func TestListActiveSnoozesStillListsASnoozeWhoseAlertCouldNotBeRead(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)
	resp := c.GET("/snoozes").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listActiveSnoozes", http.StatusOK, resp.Body())

	rows, ok := resp.JSON(t)["data"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("data = %v, want both active snoozes: %s", resp.JSON(t)["data"], resp)
	}
	nullAlert := 0
	for _, row := range rows {
		r, _ := row.(map[string]any)
		if r["alert"] == nil {
			nullAlert++
		}
	}
	if nullAlert != 1 {
		t.Fatalf("%d row(s) render a null alert, want exactly 1 — the snooze whose alert "+
			"was not in the batch must still be listed: %s", nullAlert, resp)
	}
	if svc.calls["ActiveSnoozes"] != 1 {
		t.Fatalf("the service was consulted %d time(s), want 1", svc.calls["ActiveSnoozes"])
	}
}

// TestGetCaseAnswersTheEpisodeDetailTheContractDeclares.
//
// The promise: `GET /api/v1/cases/{id}` answers the episode, the alert it
// belongs to, its enrichments and the episode-scoped delivery roll-up.
func TestGetCaseAnswersTheEpisodeDetailTheContractDeclares(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)
	resp := c.GET(casePath("")).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getCase", http.StatusOK, resp.Body())

	if svc.calls["DeliveryRollupForCase"] != 1 {
		t.Fatalf("the episode-scoped delivery roll-up was read %d time(s), want 1",
			svc.calls["DeliveryRollupForCase"])
	}
}

// ⭐ TestAnEpisodePageThatCannotReadItsDeliveryRollupFails.
//
// The promise: an absent enrichment is a missing nicety and is swallowed; an
// absent DELIVERY ROLL-UP is not, and fails the request.
//
// What broke: the two are deliberately different. A caller cannot distinguish "no
// deliveries" from "we could not look", so answering 200 with a zeroed roll-up
// would be oto asserting a silence it never verified.
func TestAnEpisodePageThatCannotReadItsDeliveryRollupFails(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)
	svc.failCaseRollup = errs.Internal("delivery_rollup_failed", errs.ErrInternal)

	resp := c.GET(casePath("")).MustStatus(t, http.StatusInternalServerError)
	schema.AssertProblem(t, "getCase", http.StatusInternalServerError, resp.Body())
}

// ⭐ TestTheTwoTimelinesDefaultToTheOrderTheirQuestionIsAskedIn.
//
// The promise: `/alerts/{id}/events` defaults to `desc` and
// `/cases/{id}/events` to `asc`, and both answer a page of `AlertEventDTO`
// ordered by oto's OWN clock.
//
// What broke when it did not hold: they answer different questions — "what has
// this rule ever done" reads newest first, "what happened during this outage"
// reads in the order it happened — and a shared default would make one of the two
// read backwards. Ordering by the upstream's `occurred_at` would be worse still: a
// skewed Alertmanager could then make a timeline render out of order, which is how
// an incident review reaches the wrong conclusion.
func TestTheTwoTimelinesDefaultToTheOrderTheirQuestionIsAskedIn(t *testing.T) {
	t.Parallel()

	cases := []struct {
		op, path, defaultOrder string
	}{
		{"listAlertEvents", alertPath("/events"), "desc"},
		{"listCaseEvents", casePath("/events"), "asc"},
	}

	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			t.Parallel()

			c, svc := newAlertsProbe(t)
			// The repository answers a keyset page NEWEST-FIRST, so the fake hands
			// one back that way. Without this the fixture is already ascending and
			// the two directions coincide — a test that could not tell an
			// unimplemented `order` from a working one.
			reverseFixtureEvents(svc)

			byDefault := c.GET(tc.path).MustStatus(t, http.StatusOK)
			schema.Assert(t, tc.op, http.StatusOK, byDefault.Body())

			asc := c.GET(tc.path+"?order=asc").MustStatus(t, http.StatusOK)
			schema.Assert(t, tc.op, http.StatusOK, asc.Body())
			desc := c.GET(tc.path+"?order=desc").MustStatus(t, http.StatusOK)

			ascIDs, descIDs := eventIDs(t, asc), eventIDs(t, desc)
			if len(ascIDs) != 3 {
				t.Fatalf("data carries %d events, want the three fixtures: %s", len(ascIDs), asc)
			}
			if sameOrder(ascIDs, descIDs) {
				t.Fatalf("%s answered the same sequence for asc and desc; one direction "+
					"is not implemented: %s", tc.op, asc)
			}
			// ⭐ Ordered by oto's OWN clock. The UI displays `occurred_at`; it never
			// sorts by it, because a skewed upstream would then be able to make a
			// timeline render out of order.
			if !ascendingBy(t, asc, "recorded_at") {
				t.Fatalf("?order=asc is not ascending by recorded_at: %s", asc)
			}

			want := ascIDs
			if tc.defaultOrder == "desc" {
				want = descIDs
			}
			if !sameOrder(eventIDs(t, byDefault), want) {
				t.Fatalf("%s defaulted to the wrong direction; it answers a different "+
					"question from its sibling and must default to %s: %s",
					tc.op, tc.defaultOrder, byDefault)
			}
		})
	}
}

// TestAnUnknownEventTypeIsRefusedWithTheFieldNamed.
//
// The promise: `?type=` outside the closed event vocabulary is a 422 naming
// `type`, not an empty page.
//
// What broke: an empty timeline reads as "nothing happened", which on an incident
// review is the most misleading answer the API can give.
func TestAnUnknownEventTypeIsRefusedWithTheFieldNamed(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)
	resp := c.GET(alertPath("/events?type=alert.exploded")).
		MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "listAlertEvents", http.StatusUnprocessableEntity, resp.Body())

	p := resp.MustViolate(t, "type")
	if p.Violations[0].Code != "enum" {
		t.Fatalf("violations[type].code = %q, want enum", p.Violations[0].Code)
	}
	if svc.calls["AlertTimeline"] != 0 {
		t.Fatal("an unknown event type still reached the service")
	}
}

// TestListLabelValuesAnswersTheTypeaheadTheContractDeclares.
//
// The promise: `GET /api/v1/labels/{name}/values` answers the values of ONE label
// with their counts, and forwards the label name and the prefix to the service
// rather than filtering in the handler.
func TestListLabelValuesAnswersTheTypeaheadTheContractDeclares(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)
	resp := c.GET("/labels/namespace/values?q=pay&limit=25").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listLabelValues", http.StatusOK, resp.Body())

	if svc.lastLabelName != "namespace" {
		t.Fatalf("the service was asked for label %q, want namespace", svc.lastLabelName)
	}
	if svc.lastLabelPrefix != "pay" || svc.lastLimit != 25 {
		t.Fatalf("prefix = %q, limit = %d; the typeahead must push both down to the query",
			svc.lastLabelPrefix, svc.lastLimit)
	}
}

// TestALabelNameThatIsNotOneIsRefusedWithTheFieldNamed.
//
// The promise: a path segment that cannot be a Prometheus label name is a 422
// whose `violations[]` names `name` with a machine code.
//
// What broke: a name that reaches the query is a name the database has to reject,
// and the caller learns only that something went wrong somewhere.
func TestALabelNameThatIsNotOneIsRefusedWithTheFieldNamed(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)
	resp := c.GET("/labels/1nvalid/values").MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "listLabelValues", http.StatusUnprocessableEntity, resp.Body())

	p := resp.MustViolate(t, "name")
	if p.Code != "validation_failed" {
		t.Fatalf("code = %q, want validation_failed", p.Code)
	}
	if svc.calls["LabelValues"] != 0 {
		t.Fatal("a refused label name still reached the service")
	}
}

// TestAnUnknownQueryParameterIsRefusedRatherThanIgnored — SPEC §E.3.
//
// The promise: `400 unknown_parameter`, with `violations[]` naming the parameter.
//
// What broke: a silently dropped `?state=firing` on a snooze list returns the
// unfiltered page and looks exactly like a filtered one, which is how the UI and
// the API stop agreeing without anybody noticing.
func TestAnUnknownQueryParameterIsRefusedRatherThanIgnored(t *testing.T) {
	t.Parallel()

	apitest.AssertUnknownQueryParamRefused(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		c, svc := newAlertsProbe(t)
		return c, func(t *testing.T, _ apitest.Route, _ *apitest.Response) {
			if svc.calls["ActiveSnoozes"] != 0 {
				t.Fatal("a refused query still reached the service")
			}
		}
	}, []apitest.Route{
		{Op: "listActiveSnoozes", Method: http.MethodGet, Path: "/snoozes?state=firing"},
	})
}

/* -------------------------------------------------------------------------- */
/* The human verbs                                                            */
/* -------------------------------------------------------------------------- */

// humanVerb is one of the five write operations a person can perform.
// They are tabled because they share every property that matters — attribution,
// tenancy, and the shape of their answer — and five hand-written copies is how one
// of them quietly acquires a different rule.
//
// ⭐ `target` IS THE WHOLE PATH AND NOT A SUFFIX, BECAUSE THE FIVE NO LONGER SHARE
// A SUBJECT. Ack and unack are addressed by CASE id: a receipt is a fact about one
// firing episode and is stored on `alert_cases`. Comment and the two snooze verbs
// are addressed by ALERT id: a snooze is a row in `alert_snoozes` keyed by
// `alert_id` and outlives every episode, and a comment annotates the identity's
// timeline. A shared `alertPath` prefix would have hidden that split.
type humanVerb struct {
	op     string
	target string
	body   string
	status int
}

func humanVerbs() []humanVerb {
	return []humanVerb{
		{"ackCase", casePath("/ack"), `{"note":"Known deploy, rolling back"}`, http.StatusOK},
		{"unackCase", casePath("/unack"), `{"note":"not the deploy after all"}`, http.StatusOK},
		{"commentOnAlert", alertPath("/comments"), `{"body":"Confirmed upstream provider incident."}`, http.StatusCreated},
		{"snoozeAlert", alertPath("/snooze"), `{"duration_seconds":14400,"note":"deploy window"}`, http.StatusOK},
		{"unsnoozeAlert", alertPath("/unsnooze"), `{"note":"deploy finished early"}`, http.StatusOK},
	}
}

// TestEveryHumanVerbAnswersTheShapeTheContractDeclares.
//
// The promise: ack and unack answer the case, a comment answers 201 with
// the appended event, and the two snooze verbs answer the ALERT DETAIL — the same
// schema `GET /alerts/{id}` returns, delivery roll-up included.
//
// What broke: a field present on one rendering of a schema and absent on another
// is precisely the drift the hand-copied DTO layer exists to prevent. The snooze
// verbs re-read the alert rather than describing what the request hoped for, so
// the response is the row as it now stands.
func TestEveryHumanVerbAnswersTheShapeTheContractDeclares(t *testing.T) {
	t.Parallel()

	for _, v := range humanVerbs() {
		t.Run(v.op, func(t *testing.T) {
			t.Parallel()

			c, svc := newAlertsProbe(t)
			resp := sendJSON(t, c, v.op, http.MethodPost, v.target, v.body).
				MustStatus(t, v.status)
			schema.Assert(t, v.op, v.status, resp.Body())

			// ⛔ THE ACTION IS ATTRIBUTED TO THE PERSON WHO TOOK IT. An
			// acknowledgement nobody signed is a receipt nobody signed.
			if svc.lastActor.ID() != apitest.UserID.String() {
				t.Fatalf("the action was attributed to %q, want the caller %s",
					svc.lastActor.ID(), apitest.UserID)
			}
			if !svc.lastActor.Kind().IsHuman() {
				t.Fatalf("the action was attributed to a %s actor; the human verbs require a human",
					svc.lastActor.Kind())
			}
		})
	}
}

// TestTheAckVerbsAcceptNoBodyAtAll.
//
// The promise: `note` is optional, so an argument-free verb sent with NO BODY —
// which is what `POST /alerts/{id}/ack` looks like from a chat button — is a 200
// and not a 400.
//
// What broke: an optional body decoded as a required one turns the simplest call
// in the API into a refusal, and the caller has no field to fix.
func TestTheAckVerbsAcceptNoBodyAtAll(t *testing.T) {
	t.Parallel()

	for _, v := range []struct{ op, target string }{
		{"ackCase", casePath("/ack")},
		{"unackCase", casePath("/unack")},
		{"unsnoozeAlert", alertPath("/unsnooze")},
	} {
		t.Run(v.op, func(t *testing.T) {
			t.Parallel()

			c, _ := newAlertsProbe(t)
			resp := c.POST(t, v.target, nil).MustStatus(t, http.StatusOK)
			schema.Assert(t, v.op, http.StatusOK, resp.Body())
		})
	}
}

// ⛔ TestANonHumanPrincipalCannotSignAHumanVerb — §E.1.1.
//
// The promise: a system credential is refused with 403 on every verb that records
// WHO did something, and the service is never reached.
//
// What broke: acknowledgement identity is stored because it is operationally
// necessary. An ack attributed to "system" tells the next responder that somebody
// is looking at the incident when nobody is.
func TestANonHumanPrincipalCannotSignAHumanVerb(t *testing.T) {
	t.Parallel()

	for _, v := range humanVerbs() {
		t.Run(v.op, func(t *testing.T) {
			t.Parallel()

			c, svc := newAlertsProbe(t)
			resp := c.As(apitest.Machine()).
				Raw(http.MethodPost, v.target, apitest.ContentTypeJSON, v.body).
				MustStatus(t, http.StatusForbidden)
			schema.AssertProblem(t, v.op, http.StatusForbidden, resp.Body())

			if len(svc.calls) != 0 {
				t.Fatalf("a machine principal reached the service: %v", svc.calls)
			}
		})
	}
}

// ⛔ TestAVerbTheAlertIsInTheWrongStateForIsA412AndNotA409.
//
// The promise: acking a case that has already ended, or waking an alert
// nobody put to sleep, is a PRECONDITION failure. The request is well-formed and
// the caller is entitled to make it; the entity is simply in the wrong state.
//
// What broke: a 409 says "somebody else changed this, try again", and a client
// that believes it will retry forever against a state that is never coming back.
// The distinction is the difference between a retry and a refresh.
func TestAVerbTheAlertIsInTheWrongStateForIsA412AndNotA409(t *testing.T) {
	t.Parallel()

	for _, v := range humanVerbs() {
		if !schema.Op(t, v.op).Declares(http.StatusPreconditionFailed) {
			// A comment is legal whatever state the alert is in, and the contract
			// says so by declaring no 412 for it.
			continue
		}
		t.Run(v.op, func(t *testing.T) {
			t.Parallel()

			c, svc := newAlertsProbe(t)
			svc.failVerb = errs.Precondition("case_ended",
				"this case has already ended")

			resp := c.Raw(http.MethodPost, v.target, apitest.ContentTypeJSON, v.body).
				MustStatus(t, http.StatusPreconditionFailed)
			schema.AssertProblem(t, v.op, http.StatusPreconditionFailed, resp.Body())

			if code := resp.Problem(t).Code; code != "case_ended" {
				t.Fatalf("code = %q; the service's own machine code must survive the "+
					"trip through the problem writer", code)
			}
		})
	}
}

// ⛔ TestASnoozeMustEnd — §B.8.3.
//
// The promise: EXACTLY ONE of `until` and `duration_seconds`, never both and never
// neither, and the refusal names the field with a machine code.
//
// What broke: there is no indefinite snooze. An unexpiring quiet period is a mute
// with no indicator light, and a caller that sends neither spelling has asked for
// exactly that — so the refusal has to say which field to supply rather than
// failing somewhere inside the domain with prose.
func TestASnoozeMustEnd(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		body  string
		field string
		code  string
	}{
		{
			name:  "neither spelling",
			body:  `{"note":"just for a bit"}`,
			field: "until",
			code:  "required_without",
		},
		{
			name:  "both spellings",
			body:  `{"until":"2026-08-11T18:00:00Z","duration_seconds":3600}`,
			field: "until",
			code:  "excluded_with",
		},
		{
			name:  "a window shorter than the floor",
			body:  `{"duration_seconds":60}`,
			field: "duration_seconds",
			code:  "min",
		},
		{
			name:  "a window longer than the ceiling",
			body:  `{"duration_seconds":5184000}`,
			field: "duration_seconds",
			code:  "max",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, svc := newAlertsProbe(t)
			resp := c.Raw(http.MethodPost, alertPath("/snooze"), apitest.ContentTypeJSON, tc.body).
				MustStatus(t, http.StatusUnprocessableEntity)
			schema.AssertProblem(t, "snoozeAlert", http.StatusUnprocessableEntity, resp.Body())

			p := resp.MustViolate(t, tc.field)
			for _, v := range p.Violations {
				if v.Field == tc.field && v.Code != tc.code {
					t.Fatalf("violations[%s].code = %q, want %q", tc.field, v.Code, tc.code)
				}
			}
			if svc.calls["Snooze"] != 0 {
				t.Fatal("a refused snooze still reached the service")
			}
		})
	}
}

/* -------------------------------------------------------------------------- */
/* The bulk wake                                                              */
/* -------------------------------------------------------------------------- */

// unsnoozeAlertsPath is the BULK wake. It has no `{id}`: its subjects are named in
// the body, which is the whole point of the endpoint.
const unsnoozeAlertsPath = "/alerts/unsnooze"

// ⭐⭐ TestTheBulkWakeAnswersAnAccountAndNotACount.
//
// The promise: `POST /api/v1/alerts/unsnooze` answers `200` with one entry per
// requested id, in request order, and `woken + skipped == requested ==
// results.length`.
//
// What broke: a bulk action that answers a bare number cannot tell the operator
// which rows it left behind. "3 of 5" is the answer a person needs, and the two
// missing rows have different explanations — one was already awake, one is not in
// this org — which no count can carry.
func TestTheBulkWakeAnswersAnAccountAndNotACount(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)
	body := `{"alert_ids":["` + fxAlertID.String() + `","` + apitest.StrangerID.String() + `"],` +
		`"note":"deploy finished early"}`

	resp := sendJSON(t, c, "unsnoozeAlerts", http.MethodPost, unsnoozeAlertsPath, body).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "unsnoozeAlerts", http.StatusOK, resp.Body())

	data, ok := resp.JSON(t)["data"].(map[string]any)
	if !ok {
		t.Fatalf("the response carries no data object:\n%s", resp.Body())
	}
	for field, want := range map[string]float64{"requested": 2, "woken": 1, "skipped": 1} {
		if got, _ := data[field].(float64); got != want {
			t.Errorf("data.%s = %v, want %v", field, data[field], want)
		}
	}

	rows, ok := data["results"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("results is not a list of two outcomes:\n%s", resp.Body())
	}
	// ⛔ ORDER IS PART OF THE CONTRACT. A surface renders the account against the
	// rows the operator ticked, and an account it has to re-join by id is one that
	// will be re-joined wrongly.
	first, _ := rows[0].(map[string]any)
	second, _ := rows[1].(map[string]any)
	if id, _ := first["alert_id"].(string); id != fxAlertID.String() {
		t.Errorf("results[0].alert_id = %v, want the first id the request named", first["alert_id"])
	}
	if got, _ := first["outcome"].(string); got != "woken" {
		t.Errorf("results[0].outcome = %v, want woken", first["outcome"])
	}
	if first["reason"] != nil {
		t.Errorf("results[0].reason = %v; a wake has nothing to explain", first["reason"])
	}

	// ⛔⛔ ANOTHER TENANT'S ID IS A SKIP AND NOT A LEAK. It is reported exactly as an
	// id belonging to nobody would be — `alert_not_found` — because any other
	// treatment is an existence oracle, and this endpoint would let one be walked a
	// hundred ids per request.
	if got, _ := second["outcome"].(string); got != "skipped" {
		t.Errorf("results[1].outcome = %v, want skipped", second["outcome"])
	}
	if got, _ := second["reason"].(string); got != "alert_not_found" {
		t.Errorf("results[1].reason = %v, want alert_not_found", second["reason"])
	}

	if svc.calls["UnsnoozeMany"] != 1 {
		t.Fatalf("the service was called %d time(s); one request is one fan-out",
			svc.calls["UnsnoozeMany"])
	}
	if svc.lastActor.ID() != apitest.UserID.String() || !svc.lastActor.Kind().IsHuman() {
		t.Fatalf("the wake was attributed to %q (%s), want the human caller %s",
			svc.lastActor.ID(), svc.lastActor.Kind(), apitest.UserID)
	}
	// The note rides along to every wake-up: the fan-out is of the primitive, note
	// and all, exactly as the group unsnooze does it.
	if svc.lastUnsnoozeNote != "deploy finished early" {
		t.Errorf("the note reached the service as %q", svc.lastUnsnoozeNote)
	}
}

// ⭐ TestABulkWakeThatWokeNothingIsStillA200.
//
// The promise: every id already awake is a `200` whose account says so — never the
// `412 not_snoozed` the single-alert route answers.
//
// What broke: the single route addresses ONE entity and has nothing to report but
// its state, so a `412` is the whole answer there. Here there is always an account,
// and "all of them were already awake" is a complete and correct one. A `412` would
// also destroy the account, which is the only record of what the press did.
func TestABulkWakeThatWokeNothingIsStillA200(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)
	svc.failVerb = errs.Precondition("not_snoozed", "this alert is not snoozed")

	body := `{"alert_ids":["` + fxAlertID.String() + `"]}`
	resp := sendJSON(t, c, "unsnoozeAlerts", http.MethodPost, unsnoozeAlertsPath, body).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "unsnoozeAlerts", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	if got, _ := data["woken"].(float64); got != 0 {
		t.Errorf("woken = %v, want 0", data["woken"])
	}
	if got, _ := data["skipped"].(float64); got != 1 {
		t.Errorf("skipped = %v, want 1", data["skipped"])
	}
	rows, _ := data["results"].([]any)
	if len(rows) != 1 {
		t.Fatalf("results should still name the id that was asked about:\n%s", resp.Body())
	}
	row, _ := rows[0].(map[string]any)
	if got, _ := row["reason"].(string); got != "not_snoozed" {
		t.Errorf("results[0].reason = %v, want not_snoozed — a skip must say WHICH skip", row["reason"])
	}
}

// ⛔⛔ TestTheBulkWakeRefusesAListItCannotBound.
//
// The promise: `alert_ids` is required, must name at least one alert, may not
// repeat one, and is capped at MaxUnsnoozeAlertIDs. Each refusal is a `422` naming
// `alert_ids`, which is what every other bounded list in this API answers.
//
// What broke: the cap is the only thing standing between one press and an unbounded
// number of write transactions inside one request — and the REQUIRED-ness is what
// stops `{}` meaning "wake everything", which is the reading this endpoint exists
// to refuse.
func TestTheBulkWakeRefusesAListItCannotBound(t *testing.T) {
	t.Parallel()

	overCap := make([]string, MaxUnsnoozeAlertIDs+1)
	for i := range overCap {
		overCap[i] = `"` + uuid.New().String() + `"`
	}

	cases := []struct{ name, body, code string }{
		{"no list at all", `{"note":"wake up"}`, "required"},
		{"an empty list", `{"alert_ids":[]}`, "min_items"},
		{
			"the same alert twice",
			`{"alert_ids":["` + fxAlertID.String() + `","` + fxAlertID.String() + `"]}`,
			"duplicate_items",
		},
		{"one past the cap", `{"alert_ids":[` + strings.Join(overCap, ",") + `]}`, "max_items"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, svc := newAlertsProbe(t)
			resp := c.Raw(http.MethodPost, unsnoozeAlertsPath, apitest.ContentTypeJSON, tc.body).
				MustStatus(t, http.StatusUnprocessableEntity)
			schema.AssertProblem(t, "unsnoozeAlerts", http.StatusUnprocessableEntity, resp.Body())

			p := resp.MustViolate(t, "alert_ids")
			for _, v := range p.Violations {
				if v.Field == "alert_ids" && v.Code != tc.code {
					t.Fatalf("violations[alert_ids].code = %q, want %q", v.Code, tc.code)
				}
			}
			// ⛔ A REFUSED LIST NEVER REACHES THE SERVICE. An over-cap list that got
			// through would be the unbounded fan-out the cap exists to prevent, and it
			// would have written half of it before anybody noticed.
			if svc.calls["UnsnoozeMany"] != 0 {
				t.Fatal("a refused bulk wake still reached the service")
			}
		})
	}
}

// ⛔ TestANonHumanPrincipalCannotWakeAlertsInBulk — §E.1.1.
//
// The promise: a machine credential is a `403` and the service is never reached,
// exactly as it is on the five single-subject human verbs.
//
// What broke: every wake-up this endpoint performs enqueues a notification and
// appends an attributed `alert.unsnoozed` event. An unattributed bulk wake is a
// hundred timeline entries signed by nobody.
func TestANonHumanPrincipalCannotWakeAlertsInBulk(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)
	body := `{"alert_ids":["` + fxAlertID.String() + `"]}`

	resp := c.As(apitest.Machine()).
		Raw(http.MethodPost, unsnoozeAlertsPath, apitest.ContentTypeJSON, body).
		MustStatus(t, http.StatusForbidden)
	schema.AssertProblem(t, "unsnoozeAlerts", http.StatusForbidden, resp.Body())

	if len(svc.calls) != 0 {
		t.Fatalf("a machine principal reached the service: %v", svc.calls)
	}
}

// ⭐ TestTheBulkWakeIsNotShadowedByTheSingleAlertRoute.
//
// The promise: `/alerts/unsnooze` reaches the bulk handler and `/alerts/{id}/unsnooze`
// still reaches the single one.
//
// What broke: the bulk path's first segment after `/alerts` is a STATIC segment
// sitting where `{id}` also matches. If the trie resolved it as an alert id the
// endpoint would answer `400 invalid_uuid` for the literal word `unsnooze` — a
// route that exists, is documented, and cannot be called.
func TestTheBulkWakeIsNotShadowedByTheSingleAlertRoute(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)

	sendJSON(t, c, "unsnoozeAlerts", http.MethodPost, unsnoozeAlertsPath,
		`{"alert_ids":["`+fxAlertID.String()+`"]}`).MustStatus(t, http.StatusOK)
	c.POST(t, alertPath("/unsnooze"), nil).MustStatus(t, http.StatusOK)

	if svc.calls["UnsnoozeMany"] != 1 || svc.calls["Unsnooze"] != 1 {
		t.Fatalf("the two routes did not land on their own handlers: %v", svc.calls)
	}
}

// TestACommentMustCarryABody.
//
// The promise: a blank comment is a 422 naming `body`; a request with no body at
// all is a 400 `empty_body`. Both are declared by the contract, and they are
// different failures: one is a well-formed request that breaks a rule, the other
// is not a request at all.
//
// What broke: a comment is an event and cannot be edited or deleted — the timeline
// IS the record — so an empty one is a permanent blank line in an incident's
// history.
func TestACommentMustCarryABody(t *testing.T) {
	t.Parallel()

	t.Run("blank after trimming", func(t *testing.T) {
		t.Parallel()

		c, svc := newAlertsProbe(t)
		resp := c.Raw(http.MethodPost, alertPath("/comments"), apitest.ContentTypeJSON, `{"body":"   "}`).
			MustStatus(t, http.StatusUnprocessableEntity)
		schema.AssertProblem(t, "commentOnAlert", http.StatusUnprocessableEntity, resp.Body())

		resp.MustViolate(t, "body")
		if svc.calls["Comment"] != 0 {
			t.Fatal("a blank comment still reached the service")
		}
	})

	t.Run("no body at all", func(t *testing.T) {
		t.Parallel()

		c, svc := newAlertsProbe(t)
		resp := c.POST(t, alertPath("/comments"), nil).MustStatus(t, http.StatusBadRequest)
		schema.AssertProblem(t, "commentOnAlert", http.StatusBadRequest, resp.Body())

		if code := resp.Problem(t).Code; code != "empty_body" {
			t.Fatalf("code = %q, want empty_body", code)
		}
		if svc.calls["Comment"] != 0 {
			t.Fatal("a bodiless comment still reached the service")
		}
	})
}

/* -------------------------------------------------------------------------- */
/* The case retention window (case_policy_config, migration 00057)            */
/* -------------------------------------------------------------------------- */

// TestListCasePoliciesAnswersThePageTheContractDeclares.
//
// The promise: `GET /api/v1/case-policies` is a keyset page of `CasePolicyDTO`,
// and the EMPTY namespace survives serialisation as `""`.
//
// ⛔ WHAT IT REALLY GUARDS IS THE EMPTY STRING. `""` is the absent-namespace
// partition — the one every alert with no `namespace` label falls into — and it is
// the only namespace most deployments will ever key on. A DTO with `omitempty` on
// that field, or a client that read a missing key as "unknown", would make the most
// commonly used rule in the collection invisible on the page that lists it.
func TestListCasePoliciesAnswersThePageTheContractDeclares(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)
	resp := c.GET("/case-policies").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listCasePolicies", http.StatusOK, resp.Body())

	rows, ok := resp.JSON(t)["data"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("data = %v, want the two fixture rules: %s", resp.JSON(t)["data"], resp)
	}
	second, _ := rows[1].(map[string]any)
	ns, present := second["namespace"]
	if !present {
		t.Fatal("the absent-namespace rule shipped without a `namespace` key; \"\" is a " +
			"partition, not a missing value")
	}
	if ns != "" {
		t.Fatalf("namespace = %v, want the empty string", ns)
	}
	if w := second["retention_window_seconds"]; w != float64(0) {
		t.Fatalf("retention_window_seconds = %v, want 0 — a stored zero is a decision and "+
			"must not be omitted", w)
	}
	if svc.calls["CasePolicies"] != 1 {
		t.Fatalf("the service was consulted %d time(s), want 1", svc.calls["CasePolicies"])
	}
	// ⛔ The tenant came off the principal and nowhere else.
	if svc.lastScope.OrgID() != apitest.OrgID {
		t.Fatalf("the handler read as org %s, want the caller's %s",
			svc.lastScope.OrgID(), apitest.OrgID)
	}
}

// TestCreateCasePolicyStoresTheWindowTheCallerAsked.
//
// The promise: `POST /api/v1/case-policies` answers 201 with the stored rule, and
// the seconds on the wire arrive at the domain as a Duration.
//
// ⭐ W=0 IS A LEGAL CREATE and is asserted here on purpose. A stored 0 and an
// absent row are the same instruction to the §B.3 machine, so it would be easy to
// "helpfully" refuse it — but recording "this alertname gets no window,
// deliberately" is a decision, and refusing it would leave the absence of a row
// ambiguous between "not decided" and "decided to be zero".
func TestCreateCasePolicyStoresTheWindowTheCallerAsked(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
		want time.Duration
		ns   string
	}{
		{"a ten minute window", `{"namespace":"production","alertname":"HighErrorRate","retention_window_seconds":600}`, 10 * time.Minute, "production"},
		{"the absent namespace", `{"alertname":"HighErrorRate","retention_window_seconds":600}`, 10 * time.Minute, ""},
		{"an explicit zero", `{"alertname":"HighErrorRate","retention_window_seconds":0}`, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, svc := newAlertsProbe(t)
			resp := sendJSON(t, c, "createCasePolicy", http.MethodPost, "/case-policies", tc.body).
				MustStatus(t, http.StatusCreated)
			schema.Assert(t, "createCasePolicy", http.StatusCreated, resp.Body())

			if got := svc.lastCasePolicyDraft.RetentionWindow; got != tc.want {
				t.Fatalf("the service was asked for %s, want %s", got, tc.want)
			}
			if got := svc.lastCasePolicyDraft.Namespace; got != tc.ns {
				t.Fatalf("namespace reached the service as %q, want %q", got, tc.ns)
			}
		})
	}
}

// ⛔ TestACasePolicyWindowOutsideItsBoundIsRefusedWithTheFieldNamed.
//
// The promise: a window above 86400 or below 0 is a 422 naming
// `retention_window_seconds`, and the service is never reached.
//
// What breaks otherwise: `case_policy_window_ck` catches it as a 23514, which
// surfaces as a 500 — a server error for a request the operator could have fixed,
// with no field for the settings form to point at.
func TestACasePolicyWindowOutsideItsBoundIsRefusedWithTheFieldNamed(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		`{"alertname":"HighErrorRate","retention_window_seconds":86401}`,
		`{"alertname":"HighErrorRate","retention_window_seconds":-1}`,
	} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()

			c, svc := newAlertsProbe(t)
			resp := c.Raw(http.MethodPost, "/case-policies", apitest.ContentTypeJSON, body).
				MustStatus(t, http.StatusUnprocessableEntity)
			schema.AssertProblem(t, "createCasePolicy", http.StatusUnprocessableEntity, resp.Body())

			resp.MustViolate(t, "retention_window_seconds")
			if len(svc.calls) != 0 {
				t.Fatalf("an out-of-range window reached the service: %v", svc.calls)
			}
		})
	}
}

// ⛔ TestACasePolicyCannotMoveItsOwnAxes.
//
// The promise: `namespace` and `alertname` are unknown properties on the PATCH
// body, so sending one is a 400 rather than a silent no-op.
//
// What breaks otherwise: the pair is the rule's identity under
// `case_policy_axes_uniq`. A PATCH that accepted and ignored an `alertname` would
// let an operator believe a window had been moved to a different alert — and the
// alert they meant to quieten would go on producing six cases per flap while the
// screen said otherwise.
func TestACasePolicyCannotMoveItsOwnAxes(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		`{"alertname":"SomethingElse"}`,
		`{"namespace":"staging","retention_window_seconds":600}`,
	} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()

			c, svc := newAlertsProbe(t)
			resp := c.Raw(http.MethodPatch, "/case-policies/"+fxCasePolicyID.String(),
				apitest.ContentTypeJSON, body).MustStatus(t, http.StatusBadRequest)
			schema.AssertProblem(t, "updateCasePolicy", http.StatusBadRequest, resp.Body())

			if len(svc.calls) != 0 {
				t.Fatalf("a request naming an immutable axis reached the service: %v", svc.calls)
			}
		})
	}
}

// TestUpdateCasePolicyChangesTheWindowAndNothingElse, and TestDeleteCasePolicy
// proves the removal is a 204 with no body.
//
// ⭐ REMOVING THE ROW RESTORES W=0, which is the close-on-resolve behaviour oto
// had before migration 00057 — not a broken state and not a disabled feature.
func TestUpdateAndDeleteCasePolicy(t *testing.T) {
	t.Parallel()

	t.Run("updateCasePolicy", func(t *testing.T) {
		t.Parallel()

		c, svc := newAlertsProbe(t)
		resp := sendJSON(t, c, "updateCasePolicy", http.MethodPatch,
			"/case-policies/"+fxCasePolicyID.String(), `{"retention_window_seconds":1200}`).
			MustStatus(t, http.StatusOK)
		schema.Assert(t, "updateCasePolicy", http.StatusOK, resp.Body())

		if svc.lastCasePolicyPatch.RetentionWindow == nil {
			t.Fatal("the patch reached the service with no window")
		} else if got := *svc.lastCasePolicyPatch.RetentionWindow; got != 20*time.Minute {
			t.Fatalf("the service was asked for %s, want 20m", got)
		}
	})

	t.Run("an empty patch", func(t *testing.T) {
		t.Parallel()

		c, svc := newAlertsProbe(t)
		resp := c.Raw(http.MethodPatch, "/case-policies/"+fxCasePolicyID.String(),
			apitest.ContentTypeJSON, `{}`).MustStatus(t, http.StatusUnprocessableEntity)
		schema.AssertProblem(t, "updateCasePolicy", http.StatusUnprocessableEntity, resp.Body())

		if len(svc.calls) != 0 {
			t.Fatalf("an empty patch reached the service: %v", svc.calls)
		}
	})

	t.Run("deleteCasePolicy", func(t *testing.T) {
		t.Parallel()

		c, svc := newAlertsProbe(t)
		resp := c.DELETE("/case-policies/"+fxCasePolicyID.String()).
			MustStatus(t, http.StatusNoContent)
		schema.AssertNoBody(t, "deleteCasePolicy", http.StatusNoContent, resp.Body())

		if svc.calls["DeleteCasePolicy"] != 1 {
			t.Fatalf("the service was consulted %d time(s), want 1",
				svc.calls["DeleteCasePolicy"])
		}
	})
}

/* -------------------------------------------------------------------------- */
/* The tenant boundary                                                        */
/* -------------------------------------------------------------------------- */

// idAddressedOperations is every operation in this package that takes a resource
// id — which is every operation a missing `WHERE org_id = $1` could leak through.
//
// It is a TABLE rather than fifteen hand-written tests for one reason: the probe
// is the highest-value assertion in this suite and the one most easily forgotten,
// and a table makes forgetting one visible.
func idAddressedOperations(id string) []apitest.Route {
	return []apitest.Route{
		{Op: "getAlert", Method: http.MethodGet, Path: "/alerts/" + id},
		{Op: "listAlertCases", Method: http.MethodGet, Path: "/alerts/" + id + "/cases"},
		{Op: "listAlertEvents", Method: http.MethodGet, Path: "/alerts/" + id + "/events"},
		{Op: "listAlertEnrichments", Method: http.MethodGet, Path: "/alerts/" + id + "/enrichments"},
		{Op: "listAlertNotifications", Method: http.MethodGet, Path: "/alerts/" + id + "/notifications"},
		{Op: "listAlertSnoozes", Method: http.MethodGet, Path: "/alerts/" + id + "/snoozes"},
		{Op: "commentOnAlert", Method: http.MethodPost, Path: "/alerts/" + id + "/comments", Body: `{"body":"who owns this?"}`},
		{Op: "snoozeAlert", Method: http.MethodPost, Path: "/alerts/" + id + "/snooze", Body: `{"duration_seconds":3600}`},
		{Op: "unsnoozeAlert", Method: http.MethodPost, Path: "/alerts/" + id + "/unsnooze", Body: `{"note":"awake"}`},
		{Op: "getCase", Method: http.MethodGet, Path: "/cases/" + id},
		{Op: "listCaseEvents", Method: http.MethodGet, Path: "/cases/" + id + "/events"},
		{Op: "ackCase", Method: http.MethodPost, Path: "/cases/" + id + "/ack", Body: `{"note":"looking"}`},
		{Op: "unackCase", Method: http.MethodPost, Path: "/cases/" + id + "/unack", Body: `{"note":"handing back"}`},
		// The CASE RETENTION WINDOW rules. A config row is as tenant-owned as an
		// alert is: another org's retention window must be a 404 and never a 403,
		// or the id space becomes an existence oracle over somebody else's
		// alertnames — which is a leak of what they monitor, not merely of a uuid.
		{Op: "updateCasePolicy", Method: http.MethodPatch, Path: "/case-policies/" + id, Body: `{"retention_window_seconds":600}`},
		{Op: "deleteCasePolicy", Method: http.MethodDelete, Path: "/case-policies/" + id},
	}
}

// ⛔ TestAnIdOutsideTheCallersTenantIsANotFoundOnEveryOperation.
//
// The promise: every id-addressed operation answers 404 for an id the caller's org
// does not own — never 403, never another tenant's data, never a 500 — and the
// refusal says nothing about who does own it.
//
// What broke: a 403 confirms the id exists somewhere, which turns the id space
// into a cross-tenant existence oracle. v1's only cause of 403 is cross-org
// access, which is exactly the case this must NOT distinguish from "no such
// thing". This is the only assertion standing between a missing `WHERE org_id =
// $1` and one customer reading another's alerts.
func TestAnIdOutsideTheCallersTenantIsANotFoundOnEveryOperation(t *testing.T) {
	t.Parallel()

	apitest.AssertCrossTenant404(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		c, _ := newAlertsProbe(t)
		return c, nil
	}, idAddressedOperations(apitest.StrangerID.String()))
}

// TestAnIdThatIsNotAUUIDIsTheSameNotFound.
//
// The promise: `/cases/banana` and `/cases/00000000-…` answer the same
// 404 as a stranger's id.
//
// What broke: telling an unauthenticated scanner that a segment was "well-formed
// but not a UUID" is a distinction with no value to a legitimate caller and real
// value to somebody enumerating.
func TestAnIdThatIsNotAUUIDIsTheSameNotFound(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"banana", "00000000-0000-0000-0000-000000000000"} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()

			c, _ := newAlertsProbe(t)
			resp := c.GET("/cases/"+id).MustStatus(t, http.StatusNotFound)
			schema.AssertProblem(t, "getCase", http.StatusNotFound, resp.Body())
		})
	}
}

// TestAnUnauthenticatedCallerGetsTheSame401OnEveryRoute.
//
// The promise: with no principal in the context there is no tenant, so there is
// nothing to read and nobody to attribute a write to — one code, one shape, on
// every route.
//
// What broke: a handler that resolved its scope from anything other than
// `authn.Scope` would serve rows to a caller who proved nothing. Every handler
// re-derives it rather than trusting that a middleware ran, because "the
// middleware guarantees it" stops being true the first time a route is mounted
// somewhere else.
func TestAnUnauthenticatedCallerGetsTheSame401OnEveryRoute(t *testing.T) {
	t.Parallel()

	apitest.AssertUnauthenticated(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		c, svc := newAlertsProbe(t)
		return c, func(t *testing.T, _ apitest.Route, _ *apitest.Response) {
			if len(svc.calls) != 0 {
				t.Fatalf("an unauthenticated request reached the service: %v", svc.calls)
			}
		}
	}, idAddressedOperations(fxAlertID.String()))
}

// TestTheDeclaredAlertOperationsAreTheOnesThisPackageServes guards the failure
// mode that made this suite necessary: an operation whose declared statuses and
// whose tested statuses were never compared, because the list of what to test was
// kept by hand.
func TestTheDeclaredAlertOperationsAreTheOnesThisPackageServes(t *testing.T) {
	t.Parallel()

	// Every id-addressed operation must declare the 404 the tenant probe asserts.
	// An operation that declared none would make the probe above assert a status
	// no client is allowed to expect.
	for _, tc := range idAddressedOperations("x") {
		if op := schema.Op(t, tc.Op); !op.Declares(http.StatusNotFound) {
			t.Fatalf("%s declares no 404, but a cross-tenant id must produce one", tc.Op)
		}
	}
	for _, v := range humanVerbs() {
		op := schema.Op(t, v.op)
		if op.SuccessStatus() != v.status {
			t.Fatalf("%s declares success %d, and this file asserts %d",
				v.op, op.SuccessStatus(), v.status)
		}
		// ⛔ The 403 is the non-human refusal, and it is the ONLY 403 v1 has
		// besides cross-org access.
		if !op.Declares(http.StatusForbidden) {
			t.Fatalf("%s declares no 403, but a machine principal must be refused with one", v.op)
		}
	}
	// ⛔ There is no Resolve, no Close and no Dismiss, and there never will be:
	// oto does not own the signal, so it cannot end one (§E.1.1).
	for _, id := range []string{"resolveAlert", "closeAlert", "dismissAlert", "deleteAlert"} {
		if _, err := schema.Lookup(id); err == nil {
			t.Fatalf("the contract has grown a %q operation; oto does not own the signal "+
				"and cannot end one", id)
		}
	}
}

/* -------------------------------------------------------------------------- */
/* Small helpers                                                              */
/* -------------------------------------------------------------------------- */

// reverseFixtureEvents makes the fake answer newest-first, which is the order the
// keyset read actually returns.
func reverseFixtureEvents(svc *fakeAlertsService) {
	evs := svc.timelineRes.Events
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
}

// eventIDs reads the id of every row of a timeline page, which is the only thing
// an ordering assertion needs.
func eventIDs(t *testing.T, resp *apitest.Response) []string {
	t.Helper()
	rows, ok := resp.JSON(t)["data"].([]any)
	if !ok {
		t.Fatalf("the timeline body has no data array: %s", resp)
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		r, ok := row.(map[string]any)
		if !ok {
			t.Fatalf("a timeline row is not an object: %v", row)
		}
		id, _ := r["id"].(string)
		out = append(out, id)
	}
	return out
}

// ascendingBy reports whether the page is non-decreasing in the named RFC 3339
// member. String comparison is sound here because every timestamp is UTC with a
// `Z` suffix, which is exactly what makes RFC 3339 lexicographically ordered.
func ascendingBy(t *testing.T, resp *apitest.Response, member string) bool {
	t.Helper()
	rows, _ := resp.JSON(t)["data"].([]any)
	prev := ""
	for _, row := range rows {
		r, _ := row.(map[string]any)
		v, _ := r[member].(string)
		if v == "" {
			t.Fatalf("a timeline row carries no %s: %v", member, row)
		}
		if v < prev {
			return false
		}
		prev = v
	}
	return true
}

func sameOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

/* -------------------------------------------------------------------------- */
/* Ack is a fact about an episode, and this transport says so                 */
/* -------------------------------------------------------------------------- */

// TestTheAlertListRefusesAnAckFilterRatherThanIgnoringIt — §E.3, and the reason
// `alerts.ack_state` is gone.
//
// The promise: `?ack=` is not a parameter of `listAlerts` or `listAlertRollups`
// any more, so it is `400 unknown_parameter` with `ack` named in `violations[]`.
//
// ⭐ REFUSAL, NOT SILENT REMOVAL, IS THE WHOLE POINT. A dropped `?ack=unacked`
// returns the unfiltered page and looks exactly like a filtered one — an operator
// reading "47 unacknowledged" would be reading 47 alerts, full stop. §E.3 already
// requires this of every unknown parameter; what it buys HERE is that a client
// still sending the old filter finds out at once instead of triaging a list that
// silently stopped narrowing.
//
// ⛔ AND THE FILTER IS GONE FOR A REASON, not merely moved. It read
// `alerts.ack_state`, a projection of the CURRENT episode's receipt onto an
// entity that outlives every episode it has: between a resolution and the next
// firing the column asserted `acked` about a firing that had ended, so
// `?ack=unacked` hid an alert nobody had ever looked at. The ack facet is served
// where its subject still exists — the group list's own `?ack=`, which reads each
// member's episode, and `include=current_case` on this list.
func TestTheAlertListRefusesAnAckFilterRatherThanIgnoringIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, target string }{
		{"list", "/alerts?ack=unacked"},
		{"roll-up", "/alerts/rollups?group_by=alertname&ack=acked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, svc := newAlertsProbe(t)
			resp := c.GET(tc.target).MustStatus(t, http.StatusBadRequest)

			p := resp.MustViolate(t, "ack")
			if p.Code != "unknown_parameter" {
				t.Fatalf("code = %q, want unknown_parameter\n%s", p.Code, resp)
			}
			if svc.calls["List"] != 0 || svc.calls["Rollups"] != 0 {
				t.Fatal("a refused ack filter still reached the service, so the page it " +
					"would have served was the unfiltered one")
			}
		})
	}
}

// TestNoAlertShapedResponseCarriesAnAckState is the read-side half.
//
// The promise: `ack_state` appears on an episode and nowhere else. `AlertDTO`,
// `AlertDetailDTO`, `AlertRefDTO` and every roll-up bucket are silent about ack;
// `CaseDTO` — including the one embedded as `current_case` — carries
// it, with its actor and its timestamp.
//
// ⭐ IT WALKS THE BYTES THE HANDLER WROTE rather than the structs, because the
// failure this guards against is a field re-added at the DTO layer for a screen
// that "just needs the letter". The schema assertions elsewhere in this file
// cannot catch that: an ADDITIVE property is not a schema violation.
func TestNoAlertShapedResponseCarriesAnAckState(t *testing.T) {
	t.Parallel()

	c, _ := newAlertsProbe(t)

	// The detail, which is the shape with the most places to hide one.
	detail := c.GET(alertPath("")).MustStatus(t, http.StatusOK).JSON(t)
	alert, ok := detail["data"].(map[string]any)
	if !ok {
		t.Fatalf("data is not an object: %v", detail["data"])
	}
	if _, present := alert["ack_state"]; present {
		t.Error("`AlertDTO` carries `ack_state` again.\n\n" +
			"An acknowledgement is a receipt for ONE firing episode and stops being true " +
			"when that episode ends; an Alert outlives every episode it has. Read it from " +
			"`current_case`, which the list already expands for exactly this.")
	}

	// …and the episode inside it still has one, or ack has been deleted rather
	// than moved.
	ac, ok := alert["current_case"].(map[string]any)
	if !ok {
		t.Fatalf("current_case is not an object: %v", alert["current_case"])
	}
	if _, present := ac["ack_state"]; !present {
		t.Error("`CaseDTO.ack_state` is missing. Ack moved to the episode; it did " +
			"not leave the product.")
	}

	// The roll-up: every counter on a bucket is a property of the Alert, and the
	// ack pair was the one that was not.
	buckets := c.GET("/alerts/rollups?group_by=alertname").
		MustStatus(t, http.StatusOK).JSON(t)["data"].([]any)
	if len(buckets) == 0 {
		t.Fatal("the roll-up answered no buckets, so this assertion checked nothing")
	}
	for _, b := range buckets {
		bucket, isObj := b.(map[string]any)
		if !isObj {
			t.Fatalf("a bucket is not an object: %v", b)
		}
		// ⭐ ALL THREE REMOVED COUNTERS, NOT TWO. The generated client would notice
		// any of them reappearing, but this is the gate that states the REASON, and
		// a reason nobody wrote down is the one that gets argued away.
		for _, banned := range []struct{ key, why string }{
			{"acked_count", "An ack is a property of one of the Alert's FIRINGS, so this " +
				"number counted receipts for episodes that had ended. The acked count is a " +
				"case-surface number."},
			{"unacked_count", "Same defect, arrived at from the other side: it counted " +
				"alerts as unacknowledged on the strength of a column that outlived the " +
				"firing it described."},
			{"snoozed_count", "The roll-up shares the alert list's filter, and the list is " +
				"now on one tab or the other — so this would read 0 on the main tab and " +
				"`total_count` on Quiet. That is a restatement of which tab the caller is " +
				"on, not a fact about the bucket."},
		} {
			if _, present := bucket[banned.key]; present {
				t.Errorf("a roll-up bucket carries `%s` again.\n\n"+
					"`firing_count`/`suppressed_count`/`resolved_count`/`expired_count` are "+
					"the current episode's state and `flapping_count` is alert-scoped — every "+
					"counter here is a property of the Alert, answerable from `alerts` alone.\n\n"+
					"%s", banned.key, banned.why)
			}
		}
	}
}
