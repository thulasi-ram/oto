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
// occurrences, events, enrichments, notifications and snoozes through the real
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
//   - and, above all, that another tenant's id answers 404 on all thirteen
//     id-addressed operations — never 403, which would confirm the row exists.

import (
	"net/http"
	"testing"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// alertPath is the fixture alert's collection path, spelled once.
func alertPath(suffix string) string { return "/alerts/" + fxAlertID.String() + suffix }

// occurrencePath is the fixture occurrence's path.
func occurrencePath(suffix string) string { return "/occurrences/" + fxOccurrence.String() + suffix }

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
// its current occurrence, the snooze in force, the enrichment summary and the
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

// TestListAlertOccurrencesAnswersThePageTheContractDeclares.
//
// The promise: the episode list is a keyset page of `OccurrenceDTO`, and it
// renders an OPEN, an ENDED and a SUPPRESSED episode with the same schema.
//
// What broke: `ended_at`, `resolve_reason` and `suppression_reason` only exist on
// some rows. A test over a single open occurrence proves that `null` validates
// and nothing else.
func TestListAlertOccurrencesAnswersThePageTheContractDeclares(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)
	resp := c.GET(alertPath("/occurrences")).MustStatus(t, http.StatusOK)
	schema.Assert(t, "listAlertOccurrences", http.StatusOK, resp.Body())

	rows, ok := resp.JSON(t)["data"].([]any)
	if !ok || len(rows) != 3 {
		t.Fatalf("data = %v, want the open, ended and suppressed episodes: %s",
			resp.JSON(t)["data"], resp)
	}
	if svc.calls["Occurrences"] != 1 {
		t.Fatalf("the service was consulted %d time(s), want 1", svc.calls["Occurrences"])
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

// TestGetOccurrenceAnswersTheEpisodeDetailTheContractDeclares.
//
// The promise: `GET /api/v1/occurrences/{id}` answers the episode, the alert it
// belongs to, its enrichments and the episode-scoped delivery roll-up.
func TestGetOccurrenceAnswersTheEpisodeDetailTheContractDeclares(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)
	resp := c.GET(occurrencePath("")).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getOccurrence", http.StatusOK, resp.Body())

	if svc.calls["DeliveryRollupForOccurrence"] != 1 {
		t.Fatalf("the episode-scoped delivery roll-up was read %d time(s), want 1",
			svc.calls["DeliveryRollupForOccurrence"])
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
	svc.failOccurrenceRollup = errs.Internal("delivery_rollup_failed", errs.ErrInternal)

	resp := c.GET(occurrencePath("")).MustStatus(t, http.StatusInternalServerError)
	schema.AssertProblem(t, "getOccurrence", http.StatusInternalServerError, resp.Body())
}

// ⭐ TestTheTwoTimelinesDefaultToTheOrderTheirQuestionIsAskedIn.
//
// The promise: `/alerts/{id}/events` defaults to `desc` and
// `/occurrences/{id}/events` to `asc`, and both answer a page of `AlertEventDTO`
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
		{"listOccurrenceEvents", occurrencePath("/events"), "asc"},
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

// humanVerb is one of the five write operations a person can perform on an alert.
// They are tabled because they share every property that matters — attribution,
// tenancy, and the shape of their answer — and five hand-written copies is how one
// of them quietly acquires a different rule.
type humanVerb struct {
	op     string
	path   string
	body   string
	status int
}

func humanVerbs() []humanVerb {
	return []humanVerb{
		{"ackAlert", "/ack", `{"note":"Known deploy, rolling back"}`, http.StatusOK},
		{"unackAlert", "/unack", `{"note":"not the deploy after all"}`, http.StatusOK},
		{"commentOnAlert", "/comments", `{"body":"Confirmed upstream provider incident."}`, http.StatusCreated},
		{"snoozeAlert", "/snooze", `{"duration_seconds":14400,"note":"deploy window"}`, http.StatusOK},
		{"unsnoozeAlert", "/unsnooze", `{"note":"deploy finished early"}`, http.StatusOK},
	}
}

// TestEveryHumanVerbAnswersTheShapeTheContractDeclares.
//
// The promise: ack and unack answer the occurrence, a comment answers 201 with
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
			resp := sendJSON(t, c, v.op, http.MethodPost, alertPath(v.path), v.body).
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

	for _, v := range []struct{ op, path string }{
		{"ackAlert", "/ack"},
		{"unackAlert", "/unack"},
		{"unsnoozeAlert", "/unsnooze"},
	} {
		t.Run(v.op, func(t *testing.T) {
			t.Parallel()

			c, _ := newAlertsProbe(t)
			resp := c.POST(t, alertPath(v.path), nil).MustStatus(t, http.StatusOK)
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
				Raw(http.MethodPost, alertPath(v.path), apitest.ContentTypeJSON, v.body).
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
// The promise: acking an occurrence that has already ended, or waking an alert
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
			svc.failVerb = errs.Precondition("occurrence_ended",
				"this occurrence has already ended")

			resp := c.Raw(http.MethodPost, alertPath(v.path), apitest.ContentTypeJSON, v.body).
				MustStatus(t, http.StatusPreconditionFailed)
			schema.AssertProblem(t, v.op, http.StatusPreconditionFailed, resp.Body())

			if code := resp.Problem(t).Code; code != "occurrence_ended" {
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
/* The tenant boundary                                                        */
/* -------------------------------------------------------------------------- */

// idAddressedOperations is every operation in this package that takes a resource
// id — which is every operation a missing `WHERE org_id = $1` could leak through.
//
// It is a TABLE rather than thirteen hand-written tests for one reason: the probe
// is the highest-value assertion in this suite and the one most easily forgotten,
// and a table makes forgetting one visible.
func idAddressedOperations(id string) []apitest.Route {
	return []apitest.Route{
		{Op: "getAlert", Method: http.MethodGet, Path: "/alerts/" + id},
		{Op: "listAlertOccurrences", Method: http.MethodGet, Path: "/alerts/" + id + "/occurrences"},
		{Op: "listAlertEvents", Method: http.MethodGet, Path: "/alerts/" + id + "/events"},
		{Op: "listAlertEnrichments", Method: http.MethodGet, Path: "/alerts/" + id + "/enrichments"},
		{Op: "listAlertNotifications", Method: http.MethodGet, Path: "/alerts/" + id + "/notifications"},
		{Op: "listAlertSnoozes", Method: http.MethodGet, Path: "/alerts/" + id + "/snoozes"},
		{Op: "ackAlert", Method: http.MethodPost, Path: "/alerts/" + id + "/ack", Body: `{"note":"looking"}`},
		{Op: "unackAlert", Method: http.MethodPost, Path: "/alerts/" + id + "/unack", Body: `{"note":"handing back"}`},
		{Op: "commentOnAlert", Method: http.MethodPost, Path: "/alerts/" + id + "/comments", Body: `{"body":"who owns this?"}`},
		{Op: "snoozeAlert", Method: http.MethodPost, Path: "/alerts/" + id + "/snooze", Body: `{"duration_seconds":3600}`},
		{Op: "unsnoozeAlert", Method: http.MethodPost, Path: "/alerts/" + id + "/unsnooze", Body: `{"note":"awake"}`},
		{Op: "getOccurrence", Method: http.MethodGet, Path: "/occurrences/" + id},
		{Op: "listOccurrenceEvents", Method: http.MethodGet, Path: "/occurrences/" + id + "/events"},
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
// The promise: `/occurrences/banana` and `/occurrences/00000000-…` answer the same
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
			resp := c.GET("/occurrences/"+id).MustStatus(t, http.StatusNotFound)
			schema.AssertProblem(t, "getOccurrence", http.StatusNotFound, resp.Body())
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
