package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/contract/apitest"
	"github.com/thulasiram/oto/test/contract/schema"
)

// The Wordings half of the Channels tag, asserted against the ONE contract.
//
// A Wording is customer-authored Liquid that writes the TEXT of one Stanza
// (ADR 0037). The promises being protected, in order of how much a break would
// cost:
//
//  1. THE SAVE-TIME GATE EXECUTES. An unknown filter is a render-time error in
//     Liquid, so a create that only parsed would accept `{{ x | no_filter }}`
//     and discover it at 03:00 on a real card. Nothing here re-implements that
//     gate — `service.ValidateWording` owns it, and these tests prove the
//     handler asks.
//  2. A REFUSED STANZA IS REFUSED WITH A SENTENCE. `fields`, `members`, `trail`
//     and `actions` take no Wording, and the 422 says which kind of structure
//     each one is. A generic `must be one of: …` teaches nobody why.
//  3. THE PREVIEW SHOWS TWO SPELLINGS. A filter emits a neutral mark and a
//     Dialect spells it (ADR 0048), so the same template is backticked on Slack
//     and bare for a webhook. An author shown one spelling concludes that markup
//     is theirs to write.
//  4. Another tenant's id is a 404, and another tenant's CHANNEL id is a 422 —
//     `wordings.channel_id` references `channels(id)` with no org term, so the
//     foreign key cannot hold that line.

/* -------------------------------------------------------------------------- */
/* Fakes                                                                      */
/* -------------------------------------------------------------------------- */

// wordStore is a WordingStore keyed by id which answers NOT FOUND for any row
// whose OrgID is not the caller's.
//
// That is the honest shape of the real query: every statement in
// `repository/wordings.go` is `WHERE org_id = $1 AND …`, so a stranger's id
// genuinely returns zero rows. A fake that returned the row and left the handler
// to notice would be testing a guard the production code does not have.
type wordStore struct {
	byID map[uuid.UUID]domain.Wording
	// created and patched record the writes, so a refused request can be proved
	// to have written nothing.
	created  []domain.NewWording
	patched  []domain.WordingPatch
	deleted  []uuid.UUID
	listPage []domain.Wording
	// listChannel and listDeleted record the filter the handler passed down,
	// because a filter that is accepted on the wire and dropped before the query
	// returns the wrong page wearing the right shape.
	listChannel *uuid.UUID
	listDeleted bool
}

func (f *wordStore) Get(_ context.Context, s db.TenantScope, id uuid.UUID) (domain.Wording, error) {
	row, ok := f.byID[id]
	if !ok || row.OrgID != s.OrgID() {
		return domain.Wording{}, errs.NotFound("wording_not_found", "no such wording")
	}
	return row, nil
}

func (f *wordStore) List(
	_ context.Context, s db.TenantScope, channelID *uuid.UUID, includeDeleted bool, _ db.Keyset,
) ([]domain.Wording, db.Cursor, error) {
	f.listChannel, f.listDeleted = channelID, includeDeleted
	out := make([]domain.Wording, 0, len(f.listPage))
	for _, row := range f.listPage {
		if row.OrgID != s.OrgID() {
			continue
		}
		if !includeDeleted && row.DeletedAt != nil {
			continue
		}
		out = append(out, row)
	}
	return out, db.Cursor{}, nil
}

func (f *wordStore) Create(
	_ context.Context, s db.TenantScope, n domain.NewWording,
) (domain.Wording, error) {
	f.created = append(f.created, n)
	return domain.Wording{
		ID: n.ID, OrgID: s.OrgID(), ChannelID: n.ChannelID,
		Stanza: n.Stanza, Template: n.Template,
		Matchers: n.Matchers, Reasons: n.Reasons,
		Priority: n.Priority, Enabled: n.Enabled,
		CreatedAt: chanNow, UpdatedAt: chanNow,
	}, nil
}

func (f *wordStore) Update(
	_ context.Context, s db.TenantScope, id uuid.UUID, p domain.WordingPatch,
) (domain.Wording, error) {
	row, ok := f.byID[id]
	if !ok || row.OrgID != s.OrgID() {
		return domain.Wording{}, errs.NotFound("wording_not_found", "no such wording")
	}
	f.patched = append(f.patched, p)
	if p.Template != nil {
		row.Template = *p.Template
	}
	if p.Priority != nil {
		row.Priority = *p.Priority
	}
	if p.Enabled != nil {
		row.Enabled = *p.Enabled
	}
	row.UpdatedAt = chanNow
	return row, nil
}

func (f *wordStore) Delete(_ context.Context, s db.TenantScope, id uuid.UUID) error {
	row, ok := f.byID[id]
	if !ok || row.OrgID != s.OrgID() {
		return errs.NotFound("wording_not_found", "no such wording")
	}
	f.deleted = append(f.deleted, id)
	return nil
}

/* -------------------------------------------------------------------------- */
/* Harness                                                                    */
/* -------------------------------------------------------------------------- */

// wordMine is a Wording owned by apitest.OrgID.
var wordMine = uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317497001")

// wordGone is a soft-deleted Wording owned by apitest.OrgID. It exists because
// `include_deleted` is the only way to reach it and a filter with nothing behind
// it proves nothing.
var wordGone = uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317497002")

// wordTemplate is the sentence ADR 0037 is justified by: oto's own facts, in
// prose, which Prometheus cannot write because Prometheus does not know any of
// them. `| code` is what makes the two spellings differ.
const wordTemplate = `{{ alert.name | code }} is {{ alert.severity | default: "unknown" }}`

func wordingFixture(id, org uuid.UUID, channel *uuid.UUID) domain.Wording {
	return domain.Wording{
		ID: id, OrgID: org, ChannelID: channel,
		Stanza:   "body",
		Template: wordTemplate,
		Matchers: []domain.Matcher{{Name: "service", Op: domain.MatchEq, Value: "checkout"}},
		Reasons:  []string{"fired"},
		Priority: 100,
		Enabled:  true,

		CreatedAt: chanNow.Add(-24 * time.Hour),
		UpdatedAt: chanNow.Add(-time.Hour),
	}
}

// wordWorld is one wired router plus the two stores the wording endpoints touch.
type wordWorld struct {
	wordings *wordStore
	channels *chanStore
	client   *apitest.Client
}

// newWordWorld wires the router with one Wording owned by apitest.OrgID, one
// owned by apitest.OtherOrgID and one soft-deleted, over a channel store holding
// a destination for each tenant.
//
// The stranger rows EXIST in both stores. That matters: a probe against an id
// that is simply absent proves nothing about tenancy, because "no such row" and
// "not yours" would be the same answer for the wrong reason.
func newWordWorld(t *testing.T) *wordWorld {
	t.Helper()

	deletedAt := chanNow.Add(-time.Hour)
	mine := wordingFixture(wordMine, apitest.OrgID, &chanMine)
	gone := wordingFixture(wordGone, apitest.OrgID, nil)
	gone.Enabled, gone.DeletedAt = false, &deletedAt
	stranger := wordingFixture(apitest.StrangerID, apitest.OtherOrgID, nil)

	w := &wordWorld{
		wordings: &wordStore{
			byID: map[uuid.UUID]domain.Wording{
				wordMine:           mine,
				wordGone:           gone,
				apitest.StrangerID: stranger,
			},
			listPage: []domain.Wording{mine, gone, stranger},
		},
		channels: &chanStore{
			byID: map[uuid.UUID]domain.Instance{
				chanMine:           channelFixture(chanMine, apitest.OrgID),
				apitest.StrangerID: channelFixture(apitest.StrangerID, apitest.OtherOrgID),
			},
		},
	}
	rt := NewRouter(Options{
		Registry: &chanRegistry{descriptors: []domain.Descriptor{slackDescriptor(), webhookDescriptor()}},
		Channels: w.channels,
		Wordings: w.wordings,
		Clock:    clock.NewFake(chanNow),
	})
	w.client = apitest.New(rt)
	return w
}

/* -------------------------------------------------------------------------- */
/* Happy paths — one per operation, asserted against the contract             */
/* -------------------------------------------------------------------------- */

// TestListWordingsAnswersAPageOfThisTenantsTemplates.
func TestListWordingsAnswersAPageOfThisTenantsTemplates(t *testing.T) {
	t.Parallel()

	w := newWordWorld(t)
	resp := w.client.GET("/wordings").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listWordings", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("the page carries %d wordings, want 1 — one belongs to %s and one is deleted",
			len(data), apitest.OtherOrgID)
	}
}

// TestListWordingsPassesBothFiltersDownToTheQuery.
//
// A filter accepted on the wire and dropped before the query returns the wrong
// page wearing the right shape, which is the failure §E.3 exists to prevent one
// layer up. `include_deleted` is the sharper of the two: without it the deleted
// row is invisible, so a handler that ignored the parameter would look correct
// on the default list and wrong only where somebody was auditing a past card.
func TestListWordingsPassesBothFiltersDownToTheQuery(t *testing.T) {
	t.Parallel()

	w := newWordWorld(t)
	resp := w.client.GET("/wordings?channel_id="+chanMine.String()+"&include_deleted=true").
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "listWordings", http.StatusOK, resp.Body())

	if w.wordings.listChannel == nil || *w.wordings.listChannel != chanMine {
		t.Fatalf("channel_id reached the store as %v, want %s", w.wordings.listChannel, chanMine)
	}
	if !w.wordings.listDeleted {
		t.Fatal("include_deleted=true reached the store as false; the deleted row would be unreachable")
	}
	if data, _ := resp.JSON(t)["data"].([]any); len(data) != 2 {
		t.Fatalf("the page carries %d wordings, want 2 — the deleted one was asked for", len(data))
	}
}

// TestCreatingAWordingRendersItBeforeSavingIt.
//
// The promise: saving EXECUTES the template. `service.ValidateWording` renders it
// against the whole fixture corpus, because an unknown filter is a render-time
// error in Liquid and a parse would miss it.
func TestCreatingAWordingRendersItBeforeSavingIt(t *testing.T) {
	t.Parallel()

	w := newWordWorld(t)
	body := map[string]any{
		"stanza":     "body",
		"template":   wordTemplate,
		"channel_id": chanMine.String(),
		"matchers":   []any{map[string]any{"name": "service", "op": "=", "value": "checkout"}},
		"reasons":    []any{"fired"},
	}
	// The fixture is proved to be one a real client could have sent, so this test
	// cannot pass with a request the contract forbids.
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	schema.AssertRequest(t, "createWording", raw)

	resp := w.client.POST(t, "/wordings", body).MustStatus(t, http.StatusCreated)
	schema.Assert(t, "createWording", http.StatusCreated, resp.Body())

	if len(w.wordings.created) != 1 {
		t.Fatalf("the store saw %d creates, want 1", len(w.wordings.created))
	}
	got := w.wordings.created[0]
	if got.ID == uuid.Nil {
		t.Fatal("the create command carries no id; oto mints ids in Go so a row's id is known before the INSERT")
	}
	// The DDL's own defaults, restated by the mapper because the contract
	// publishes them.
	if got.Priority != DefaultWordingPriority || !got.Enabled {
		t.Fatalf("priority = %d, enabled = %v; want the documented %d and true",
			got.Priority, got.Enabled, DefaultWordingPriority)
	}
}

// TestGetWordingServesOneTemplateIncludingADeletedOne.
//
// ⭐ A soft-deleted row is SERVED here, unlike getChannel's. A delivery records
// the wordings that produced its card, so "why did my card read like that" has to
// stay answerable after the Wording is gone — which is the same argument that
// made the delete soft in the first place.
func TestGetWordingServesOneTemplateIncludingADeletedOne(t *testing.T) {
	t.Parallel()

	w := newWordWorld(t)
	resp := w.client.GET("/wordings/"+wordMine.String()).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getWording", http.StatusOK, resp.Body())

	gone := w.client.GET("/wordings/"+wordGone.String()).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getWording", http.StatusOK, gone.Body())
	if gone.JSON(t)["data"].(map[string]any)["deleted_at"] == nil {
		t.Fatal("the deleted row came back with a null deleted_at; nothing would tell a reader it is gone")
	}
}

// TestPatchingAWordingCannotMoveItToAnotherStanza.
//
// ⛔ THE REQUEST HAS NO `stanza` FIELD AND THAT IS THE WHOLE TEST. Moving a
// Wording from `body` to `title` is not an edit of that Wording, it is a
// different Wording — the read set differs, the budget differs, and the row's
// history would claim it had always been the new one. `additionalProperties:
// false` and `DisallowUnknownFields` are the two halves of saying so.
func TestPatchingAWordingCannotMoveItToAnotherStanza(t *testing.T) {
	t.Parallel()

	w := newWordWorld(t)
	ok := w.client.PATCH(t, "/wordings/"+wordMine.String(),
		map[string]any{"template": `{{ alert.name | bold }} is up`}).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "updateWording", http.StatusOK, ok.Body())

	// `DisallowUnknownFields` is the binder's half and `additionalProperties:
	// false` is the contract's; the request struct having no such field is the
	// third. All three have to agree, or one of them is decoration.
	moved := w.client.PATCH(t, "/wordings/"+wordMine.String(),
		map[string]any{"stanza": "title"}).
		MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "updateWording", http.StatusUnprocessableEntity, moved.Body())
	if v := violationFor(t, moved, "stanza"); v.Code != "unknown_field" {
		t.Fatalf("code = %q, want unknown_field", v.Code)
	}

	if len(w.wordings.patched) != 1 {
		t.Fatalf("the store saw %d patches, want 1 — the stanza move must have written nothing",
			len(w.wordings.patched))
	}
	if w.wordings.byID[wordMine].Stanza != "body" {
		t.Fatalf("the stored stanza is now %q", w.wordings.byID[wordMine].Stanza)
	}
}

// TestDeletingAWordingIsA204WithNothingInIt.
func TestDeletingAWordingIsA204WithNothingInIt(t *testing.T) {
	t.Parallel()

	w := newWordWorld(t)
	resp := w.client.DELETE("/wordings/"+wordMine.String()).MustStatus(t, http.StatusNoContent)
	schema.AssertNoBody(t, "deleteWording", http.StatusNoContent, resp.Body())

	if len(w.wordings.deleted) != 1 || w.wordings.deleted[0] != wordMine {
		t.Fatalf("the store saw deletes %v, want exactly [%s]", w.wordings.deleted, wordMine)
	}
}

/* -------------------------------------------------------------------------- */
/* The preview — the feature's authoring story (ADR 0037 names it)            */
/* -------------------------------------------------------------------------- */

// TestPreviewSpellsTheSameSentenceTwice.
//
// ⭐⭐ THIS IS ADR 0048 IN ONE ASSERTION. `{{ alert.name | code }}` emits a
// NEUTRAL mark, not a backtick: the Slack Dialect spells it with one and the
// plain Dialect drops it while keeping every word. If both spellings ever came
// back identical, either a filter has started writing a provider's punctuation —
// which renders as the WRONG EMPHASIS on the next provider, silently, with no
// error to anyone — or the preview has stopped spelling at all.
func TestPreviewSpellsTheSameSentenceTwice(t *testing.T) {
	t.Parallel()

	w := newWordWorld(t)
	body := map[string]any{"stanza": "body", "template": wordTemplate}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	schema.AssertRequest(t, "previewWording", raw)

	resp := w.client.POST(t, "/wordings/preview", body).MustStatus(t, http.StatusOK)
	schema.Assert(t, "previewWording", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	if problems, _ := data["problems"].([]any); len(problems) != 0 {
		t.Fatalf("an honest template was refused: %#v", problems)
	}

	spellings := spellingsOf(t, data, "firing")
	slack, plain := spellings["slack"], spellings["plain"]
	if !strings.Contains(slack, "`HighErrorRate`") {
		t.Fatalf("the slack spelling is %q, and mrkdwn's code span is one backtick", slack)
	}
	if strings.Contains(plain, "`") {
		t.Fatalf("the plain spelling is %q; a webhook consumer receives words, not another product's punctuation", plain)
	}
	if !strings.Contains(plain, "HighErrorRate") {
		t.Fatalf("the plain spelling is %q; dropping a mark must keep the word it wrapped", plain)
	}
	if slack == plain {
		t.Fatalf("both dialects spelled %q — a filter is writing punctuation a Dialect should own", slack)
	}
}

// TestPreviewCoversTheWholeShippedCorpus.
//
// The nasty fixtures are the point. A template that reads beautifully on a rich
// firing card frequently breaks on the sparse ones — a resolved card with no rule
// snapshot, a digest with no group, an alert with no labels — and those are
// exactly the cards an operator is reading when something is wrong.
func TestPreviewCoversTheWholeShippedCorpus(t *testing.T) {
	t.Parallel()

	w := newWordWorld(t)
	resp := w.client.POST(t, "/wordings/preview",
		map[string]any{"stanza": "title", "template": wordTemplate}).
		MustStatus(t, http.StatusOK)
	schema.Assert(t, "previewWording", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	renderings, _ := data["renderings"].([]any)
	seen := map[string]bool{}
	for _, r := range renderings {
		m, _ := r.(map[string]any)
		name, _ := m["fixture"].(string)
		seen[name] = true
		if sp, _ := m["spellings"].([]any); len(sp) != 2 {
			t.Fatalf("fixture %q carries %d spellings, want slack and plain", name, len(sp))
		}
	}
	for _, want := range []string{
		"firing", "resolved", "digest", "empty-labels",
		"oversized-annotation", "hostile-text", "zero-value",
	} {
		if !seen[want] {
			t.Errorf("the preview skipped the %q fixture", want)
		}
	}
}

// TestPreviewOfABrokenTemplateIsA200WithTheProblemsListed.
//
// ⛔ A preview that 422'd would tell an author strictly less than one that shows
// them what broke. The refusal and the output belong in the same round trip,
// because the fix is usually visible only when both are on screen.
func TestPreviewOfABrokenTemplateIsA200WithTheProblemsListed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		template string
		wantCode string
	}{
		{
			name:     "an unknown filter, which only RENDERING can find",
			template: `{{ alert.name | no_such_filter }}`,
			wantCode: "render",
		},
		{
			name:     "a misspelled field, against the maximal view",
			template: `{{ alert.nmae | default: "-" }}`,
			wantCode: "unknown_field",
		},
		{
			// Liquid does NOT fail on this: it treats the unclosed run as literal
			// text and prints `{{ alert.name` onto the card, which is worse than an
			// error because nothing anywhere reports it.
			name:     "an unclosed expression, which liquid would print rather than refuse",
			template: `{{ alert.name`,
			wantCode: "parse",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			w := newWordWorld(t)
			resp := w.client.POST(t, "/wordings/preview",
				map[string]any{"stanza": "body", "template": tc.template}).
				MustStatus(t, http.StatusOK)
			schema.Assert(t, "previewWording", http.StatusOK, resp.Body())

			data, _ := resp.JSON(t)["data"].(map[string]any)
			if !hasProblem(data, tc.wantCode) {
				t.Fatalf("problems = %#v, want one with code %q", data["problems"], tc.wantCode)
			}
		})
	}
}

// TestPreviewWritesNothing, which is the one property that makes it safe to call
// on every keystroke.
func TestPreviewWritesNothing(t *testing.T) {
	t.Parallel()

	w := newWordWorld(t)
	w.client.POST(t, "/wordings/preview",
		map[string]any{"stanza": "body", "template": wordTemplate}).
		MustStatus(t, http.StatusOK)

	if len(w.wordings.created)+len(w.wordings.patched)+len(w.wordings.deleted) != 0 {
		t.Fatalf("the preview wrote: %d creates, %d patches, %d deletes",
			len(w.wordings.created), len(w.wordings.patched), len(w.wordings.deleted))
	}
}

// TestPreviewOfAnOversizedSourceIsA422.
//
// The one problem that is a malformed REQUEST rather than a result: a source over
// the one-line-of-prose ceiling is a body this endpoint declines to read, and
// rendering two kilobytes of it back fourteen times would answer a mistake with a
// wall.
func TestPreviewOfAnOversizedSourceIsA422(t *testing.T) {
	t.Parallel()

	w := newWordWorld(t)
	resp := w.client.POST(t, "/wordings/preview", map[string]any{
		"stanza":   "body",
		"template": strings.Repeat("a", domain.MaxWordingTemplate+1),
	}).MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "previewWording", http.StatusUnprocessableEntity, resp.Body())

	if v := violationFor(t, resp, "template"); v.Code != "too_long" {
		t.Fatalf("code = %q, want too_long", v.Code)
	}
}

/* -------------------------------------------------------------------------- */
/* Refusals                                                                   */
/* -------------------------------------------------------------------------- */

// TestARefusedStanzaIsA422SayingWhichKindOfStructureItIs.
//
// ⛔ THE MESSAGE IS THE FEATURE. All eight SPEC §H.7 names are accepted by the
// binder precisely so that the four which take no Wording can be refused with a
// sentence. A `oneof` tag on the DTO would answer `must be one of: title, body,
// rule, footer`, which is the same refusal with the reason deleted — and the
// reason is different for each of the four.
func TestARefusedStanzaIsA422SayingWhichKindOfStructureItIs(t *testing.T) {
	t.Parallel()

	for stanza, wants := range map[string]string{
		"fields":  "grid",
		"members": "enumeration",
		"trail":   "enumeration",
		"actions": "action ids",
	} {
		t.Run(stanza, func(t *testing.T) {
			t.Parallel()

			w := newWordWorld(t)
			resp := w.client.POST(t, "/wordings", map[string]any{
				"stanza": stanza, "template": wordTemplate,
			}).MustStatus(t, http.StatusUnprocessableEntity)
			schema.AssertProblem(t, "createWording", http.StatusUnprocessableEntity, resp.Body())

			v := violationFor(t, resp, "stanza")
			if v.Code != "unsupported_stanza" {
				t.Fatalf("code = %q, want unsupported_stanza", v.Code)
			}
			if !strings.Contains(v.Message, wants) {
				t.Fatalf("the refusal of %q says %q, and does not mention %q — a stanza refused "+
					"without its reason teaches nobody why", stanza, v.Message, wants)
			}
			if len(w.wordings.created) != 0 {
				t.Fatal("a refused stanza was written anyway")
			}
		})
	}
}

// TestATemplateTheEngineRefusesIsA422OnTheTemplateField.
//
// ⭐ THE UNKNOWN FILTER IS THE ONE THAT MATTERS. It parses cleanly and fails at
// render, so a save that only parsed would accept it and the card would lose that
// Stanza in the middle of the night, quietly, with the operator none the wiser.
func TestATemplateTheEngineRefusesIsA422OnTheTemplateField(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		template string
		wantCode string
	}{
		{"an unknown filter", `{{ alert.name | no_such_filter }}`, "render"},
		{"a misspelled field", `{{ alert.nmae | default: "-" }}`, "unknown_field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			w := newWordWorld(t)
			resp := w.client.POST(t, "/wordings", map[string]any{
				"stanza": "body", "template": tc.template,
			}).MustStatus(t, http.StatusUnprocessableEntity)
			schema.AssertProblem(t, "createWording", http.StatusUnprocessableEntity, resp.Body())

			if v := violationFor(t, resp, "template"); v.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", v.Code, tc.wantCode)
			}
			if len(w.wordings.created) != 0 {
				t.Fatal("a template the engine refuses was written anyway")
			}
		})
	}
}

// TestAPatchIsValidatedAsTheWholeRowItWouldProduce.
//
// A patch carries no stanza, so a template arriving alone still has to be judged
// on the stanza it will land in. Validating the delta by itself would accept a
// template that cannot render where it is going.
func TestAPatchIsValidatedAsTheWholeRowItWouldProduce(t *testing.T) {
	t.Parallel()

	w := newWordWorld(t)
	resp := w.client.PATCH(t, "/wordings/"+wordMine.String(),
		map[string]any{"template": `{{ alert.name | no_such_filter }}`}).
		MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "updateWording", http.StatusUnprocessableEntity, resp.Body())

	if v := violationFor(t, resp, "template"); v.Code != "render" {
		t.Fatalf("code = %q, want render", v.Code)
	}
	if len(w.wordings.patched) != 0 {
		t.Fatal("a template the engine refuses was patched in anyway")
	}
}

// TestAnUpdateThatAsksForNothingIsRefusedOnAWording.
func TestAnUpdateThatAsksForNothingIsRefusedOnAWording(t *testing.T) {
	t.Parallel()

	w := newWordWorld(t)
	resp := w.client.PATCH(t, "/wordings/"+wordMine.String(), map[string]any{}).
		MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "updateWording", http.StatusUnprocessableEntity, resp.Body())

	if len(w.wordings.patched) != 0 {
		t.Fatal("an empty patch reached the store")
	}
}

// TestAWordingBoundToAnotherTenantsChannelIsRefused.
//
// ⛔ THE FOREIGN KEY CANNOT HOLD THIS LINE. `wordings.channel_id` references
// `channels(id)` ALONE, with no org term, so another tenant's channel id
// satisfies it perfectly. The row would then be inert — `Resolve` scopes by org,
// so it could never spell a word on anybody's card — and inert is exactly the
// problem: the operator would see it saved, see it never apply, and have nothing
// to read that explained the difference.
func TestAWordingBoundToAnotherTenantsChannelIsRefused(t *testing.T) {
	t.Parallel()

	w := newWordWorld(t)
	resp := w.client.POST(t, "/wordings", map[string]any{
		"stanza": "body", "template": wordTemplate,
		"channel_id": apitest.StrangerID.String(),
	}).MustStatus(t, http.StatusUnprocessableEntity)
	schema.AssertProblem(t, "createWording", http.StatusUnprocessableEntity, resp.Body())

	if v := violationFor(t, resp, "channel_id"); v.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", v.Code)
	}
	if strings.Contains(string(resp.Body()), apitest.OtherOrgID.String()) {
		t.Fatalf("the refusal names the owning org:\n%s", resp)
	}
	if len(w.wordings.created) != 0 {
		t.Fatal("a wording bound to another tenant's channel was written anyway")
	}
}

/* -------------------------------------------------------------------------- */
/* The cross-cutting probes                                                   */
/* -------------------------------------------------------------------------- */

// TestAnotherTenantsWordingIdIsAlwaysA404.
//
// The stranger row is REAL and owned by apitest.OtherOrgID, so a handler that
// forgot its scope would answer 200 with somebody else's prose.
func TestAnotherTenantsWordingIdIsAlwaysA404(t *testing.T) {
	t.Parallel()

	stranger := "/wordings/" + apitest.StrangerID.String()
	routes := []apitest.Route{
		{Op: "getWording", Method: http.MethodGet, Path: stranger},
		{Op: "updateWording", Method: http.MethodPatch, Path: stranger,
			Body: `{"template":"{{ alert.name }}"}`},
		{Op: "deleteWording", Method: http.MethodDelete, Path: stranger},
	}
	apitest.AssertCrossTenant404(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		w := newWordWorld(t)
		return w.client, func(t *testing.T, _ apitest.Route, _ *apitest.Response) {
			if len(w.wordings.patched)+len(w.wordings.deleted) != 0 {
				t.Fatal("a refused cross-tenant request still reached the store")
			}
		}
	}, routes)
}

// TestEveryWordingRouteIsA401WithoutAPrincipal.
func TestEveryWordingRouteIsA401WithoutAPrincipal(t *testing.T) {
	t.Parallel()

	one := "/wordings/" + wordMine.String()
	routes := []apitest.Route{
		{Op: "listWordings", Method: http.MethodGet, Path: "/wordings"},
		{Op: "createWording", Method: http.MethodPost, Path: "/wordings",
			Body: `{"stanza":"body","template":"{{ alert.name }}"}`},
		{Op: "previewWording", Method: http.MethodPost, Path: "/wordings/preview",
			Body: `{"stanza":"body","template":"{{ alert.name }}"}`},
		{Op: "getWording", Method: http.MethodGet, Path: one},
		{Op: "updateWording", Method: http.MethodPatch, Path: one,
			Body: `{"template":"{{ alert.name }}"}`},
		{Op: "deleteWording", Method: http.MethodDelete, Path: one},
	}
	apitest.AssertUnauthenticated(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		w := newWordWorld(t)
		return w.client, func(t *testing.T, _ apitest.Route, _ *apitest.Response) {
			if len(w.wordings.created)+len(w.wordings.patched)+len(w.wordings.deleted) != 0 {
				t.Fatal("an unauthenticated request reached the store")
			}
		}
	}, routes)
}

// TestAnUnknownQueryParameterOnTheWordingListIsRefused (SPEC §E.3). A typo'd
// `?chanel_id=` that is silently ignored returns the whole tenant's list wearing
// the shape of one channel's exceptions.
func TestAnUnknownQueryParameterOnTheWordingListIsRefused(t *testing.T) {
	t.Parallel()

	routes := []apitest.Route{
		{Op: "listWordings", Method: http.MethodGet, Path: "/wordings?chanel_id=" + chanMine.String()},
	}
	apitest.AssertUnknownQueryParamRefused(t, func(t *testing.T) (*apitest.Client, apitest.RouteCheck) {
		return newWordWorld(t).client, nil
	}, routes)
}

/* -------------------------------------------------------------------------- */
/* Helpers                                                                    */
/* -------------------------------------------------------------------------- */

// violationFor is MustViolate plus the entry it found: the shared helper proves a
// field is named and carries a code, and every caller here goes on to assert
// WHICH code, because a wording refusal is only useful if it says which rule.
func violationFor(t *testing.T, resp *apitest.Response, field string) apitest.Violation {
	t.Helper()

	p := resp.MustViolate(t, field)
	for _, v := range p.Violations {
		if v.Field == field {
			return v
		}
	}
	return apitest.Violation{}
}

// spellingsOf reads one fixture's dialect→text map out of a preview body.
func spellingsOf(t *testing.T, data map[string]any, fixture string) map[string]string {
	t.Helper()

	renderings, _ := data["renderings"].([]any)
	for _, r := range renderings {
		m, _ := r.(map[string]any)
		if name, _ := m["fixture"].(string); name != fixture {
			continue
		}
		out := map[string]string{}
		sp, _ := m["spellings"].([]any)
		for _, s := range sp {
			e, _ := s.(map[string]any)
			dialect, _ := e["dialect"].(string)
			text, _ := e["text"].(string)
			out[dialect] = text
		}
		return out
	}
	t.Fatalf("the preview carries no %q fixture: %#v", fixture, data["renderings"])
	return nil
}

// hasProblem reports whether a preview body carries a problem with this code.
func hasProblem(data map[string]any, code string) bool {
	problems, _ := data["problems"].([]any)
	for _, p := range problems {
		m, _ := p.(map[string]any)
		if got, _ := m["code"].(string); got == code {
			return true
		}
	}
	return false
}
