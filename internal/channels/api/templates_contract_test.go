package api

import (
	"context"
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

// The NotificationTemplates half of the Channels tag, asserted against the ONE
// contract.
//
// The promises being protected, in order of how much a break would cost:
//
//  1. THE SAVE-TIME GATE EXECUTES. An unknown filter is a render-time error in
//     Liquid, so a create that only parsed would accept `{{ x | no_filter }}`
//     and discover it at 03:00 on a real card. Nothing here re-implements that
//     gate — `service.ValidateTemplate` owns it — and these prove the handler
//     asks.
//  2. A MISSING ACTION ROW IS A WARNING AND NOT A REFUSAL. The operator may ship a
//     card with no Acknowledge button. oto's job is to make sure they know they
//     did, and then to get out of the way.
//  3. THE PREVIEW SHOWS TWO SPELLINGS. One Markdown document compiles to Slack's
//     `*bold*` and to a webhook consumer's plain words; an author shown one
//     spelling concludes that markup is theirs to write.
//  4. Another tenant's id is a 404 — never 403, never their row, never a 500.

/* -------------------------------------------------------------------------- */
/* Fake                                                                       */
/* -------------------------------------------------------------------------- */

// tmplStore is a TemplateStore keyed by id which answers NOT FOUND for any row
// whose OrgID is not the caller's.
//
// That is the honest shape of the real query: every statement in
// `repository/templates.go` is `WHERE org_id = $1 AND …`, so a stranger's id
// genuinely returns zero rows. A fake that returned the row and left the handler
// to notice would be testing a guard the production code does not have.
type tmplStore struct {
	byID map[uuid.UUID]domain.NotificationTemplate
	// created and patched record the writes, so a refused request can be proved
	// to have written nothing.
	created     []domain.NewNotificationTemplate
	patched     []domain.NotificationTemplatePatch
	deleted     []uuid.UUID
	listPage    []domain.NotificationTemplate
	listDeleted bool
}

func (f *tmplStore) Get(_ context.Context, s db.TenantScope, id uuid.UUID) (domain.NotificationTemplate, error) {
	row, ok := f.byID[id]
	if !ok || row.OrgID != s.OrgID() {
		return domain.NotificationTemplate{}, errs.NotFound("template_not_found", "no such template")
	}
	return row, nil
}

func (f *tmplStore) List(
	_ context.Context, s db.TenantScope, includeDeleted bool, _ db.Keyset,
) ([]domain.NotificationTemplate, db.Cursor, error) {
	f.listDeleted = includeDeleted
	out := make([]domain.NotificationTemplate, 0, len(f.listPage))
	for _, row := range f.listPage {
		if row.OrgID != s.OrgID() {
			continue
		}
		if row.DeletedAt != nil && !includeDeleted {
			continue
		}
		out = append(out, row)
	}
	return out, db.Cursor{}, nil
}

func (f *tmplStore) Create(
	_ context.Context, s db.TenantScope, n domain.NewNotificationTemplate,
) (domain.NotificationTemplate, error) {
	f.created = append(f.created, n)
	return domain.NotificationTemplate{
		ID: n.ID, OrgID: s.OrgID(), Name: n.Name,
		Provider: n.Provider, Format: n.Format, Source: n.Source,
		Version: 1, Enabled: n.Enabled,
		CreatedAt: chanNow, UpdatedAt: chanNow,
	}, nil
}

func (f *tmplStore) Update(
	_ context.Context, s db.TenantScope, id uuid.UUID, p domain.NotificationTemplatePatch,
) (domain.NotificationTemplate, error) {
	row, ok := f.byID[id]
	if !ok || row.OrgID != s.OrgID() {
		return domain.NotificationTemplate{}, errs.NotFound("template_not_found", "no such template")
	}
	f.patched = append(f.patched, p)
	if p.Source != nil {
		row.Source = *p.Source
		row.Version++
	}
	if p.Name != nil {
		row.Name = *p.Name
	}
	if p.Enabled != nil {
		row.Enabled = *p.Enabled
	}
	row.UpdatedAt = chanNow
	return row, nil
}

func (f *tmplStore) Delete(_ context.Context, s db.TenantScope, id uuid.UUID) error {
	row, ok := f.byID[id]
	if !ok || row.OrgID != s.OrgID() {
		return errs.NotFound("template_not_found", "no such template")
	}
	f.deleted = append(f.deleted, id)
	return nil
}

/* -------------------------------------------------------------------------- */
/* Harness                                                                    */
/* -------------------------------------------------------------------------- */

var (
	tmplMine = uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317497001")
	// tmplGone is soft-deleted. `include_deleted` is the only way to reach it, and
	// a filter with nothing behind it proves nothing.
	tmplGone = uuid.MustParse("019fe297-d84f-7599-b5b2-1f2317497002")
)

// tmplSource is the sentence this feature is justified by: oto's own facts, in
// prose, which Prometheus cannot write because Prometheus does not know any of
// them. `| code` is what makes the two spellings differ.
const tmplSource = "# {{ alert.name | code }}\n\nfiring {{ group.firing_for }}, " +
	"{{ alert.total_cases }} times\n\n{{ actions }}"

func templateFixture(id, org uuid.UUID) domain.NotificationTemplate {
	return domain.NotificationTemplate{
		ID: id, OrgID: org,
		Name:     "house voice " + id.String()[:8],
		Provider: "slack", Format: "card", Source: tmplSource,
		Version: 1, Enabled: true,
		CreatedAt: chanNow.Add(-24 * time.Hour),
		UpdatedAt: chanNow.Add(-time.Hour),
	}
}

type tmplWorld struct {
	templates *tmplStore
	client    *apitest.Client
}

// newTmplWorld wires the router with one template owned by apitest.OrgID, one
// soft-deleted, and one owned by apitest.OtherOrgID.
//
// The stranger row EXISTS. That matters: a probe against an id that is simply
// absent proves nothing about tenancy, because "no such row" and "not yours"
// would be the same answer for the wrong reason.
func newTmplWorld(t *testing.T) *tmplWorld {
	t.Helper()

	deletedAt := chanNow.Add(-time.Hour)
	mine := templateFixture(tmplMine, apitest.OrgID)
	gone := templateFixture(tmplGone, apitest.OrgID)
	gone.Enabled, gone.DeletedAt = false, &deletedAt
	stranger := templateFixture(apitest.StrangerID, apitest.OtherOrgID)

	w := &tmplWorld{templates: &tmplStore{
		byID: map[uuid.UUID]domain.NotificationTemplate{
			tmplMine: mine, tmplGone: gone, apitest.StrangerID: stranger,
		},
		listPage: []domain.NotificationTemplate{mine, gone, stranger},
	}}
	rt := NewRouter(Options{
		Registry:  &chanRegistry{descriptors: []domain.Descriptor{slackDescriptor(), webhookDescriptor()}},
		Templates: w.templates,
		Clock:     clock.NewFake(chanNow),
	})
	w.client = apitest.New(rt)
	return w
}

/* -------------------------------------------------------------------------- */
/* Happy paths — one per operation, asserted against the contract             */
/* -------------------------------------------------------------------------- */

func TestListNotificationTemplatesAnswersThisTenantsTemplates(t *testing.T) {
	t.Parallel()

	w := newTmplWorld(t)
	resp := w.client.GET("/notification-templates").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listNotificationTemplates", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("the page carries %d templates, want 1 — one belongs to %s and one is deleted",
			len(data), apitest.OtherOrgID)
	}
}

func TestGetNotificationTemplateServesASoftDeletedRow(t *testing.T) {
	t.Parallel()

	// A delivery records the template id that produced it, so a deleted row has to
	// stay readable or "why did my card read like that" stops being answerable.
	w := newTmplWorld(t)
	resp := w.client.GET("/notification-templates/"+tmplGone.String()).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getNotificationTemplate", http.StatusOK, resp.Body())
}

func TestCreateNotificationTemplateSavesAWholeMessage(t *testing.T) {
	t.Parallel()

	w := newTmplWorld(t)
	resp := w.client.POST(t, "/notification-templates", map[string]any{
		"name": "house voice", "provider": "slack", "format": "card", "source": tmplSource,
	}).MustStatus(t, http.StatusCreated)
	schema.Assert(t, "createNotificationTemplate", http.StatusCreated, resp.Body())

	if len(w.templates.created) != 1 {
		t.Fatalf("the handler wrote %d rows, want 1", len(w.templates.created))
	}
	if got := w.templates.created[0].Enabled; !got {
		t.Error("`enabled` was omitted and did not default to true; a template nobody enabled is a picker entry that does nothing")
	}
}

// ⛔ THE GATE MUST RENDER TO CATCH THIS. `no_such_filter` PARSES cleanly in
// Liquid and fails only when executed, so a handler that merely parsed would
// accept this and discover it on a real card.
func TestCreateNotificationTemplateRefusesAFilterThatOnlyFailsWhenRun(t *testing.T) {
	t.Parallel()

	w := newTmplWorld(t)
	resp := w.client.POST(t, "/notification-templates", map[string]any{
		"name": "broken", "provider": "slack", "format": "card",
		"source": "# {{ alert.name | no_such_filter }}",
	}).MustStatus(t, http.StatusUnprocessableEntity)
	// No schema.Assert here: a 422 is `application/problem+json`, whose shape the
	// envelope gate already proves for every declared refusal in the contract.
	_ = resp
	if len(w.templates.created) != 0 {
		t.Fatal("a refused template was written anyway")
	}
}

// ⭐ THE OPERATOR MAY SHIP A CARD WITH NO ACKNOWLEDGE BUTTON. It is a degraded card
// and not a lost alert — `POST /api/v1/cases/{id}/ack` reaches the same service
// method the button does — so oto says so and saves it.
func TestATemplateWithNoActionRowIsSavedAndWarnedAbout(t *testing.T) {
	t.Parallel()

	w := newTmplWorld(t)
	w.client.POST(t, "/notification-templates", map[string]any{
		"name": "quiet", "provider": "slack", "format": "card",
		"source": "# {{ alert.name }}\n\nno buttons",
	}).MustStatus(t, http.StatusCreated)

	if len(w.templates.created) != 1 {
		t.Fatal("a card with no `{{ actions }}` was refused; that is the operator's choice to make")
	}

	// And the preview says so, which is where somebody can still change their mind.
	resp := w.client.POST(t, "/notification-templates/preview", map[string]any{
		"format": "card", "source": "# {{ alert.name }}\n\nno buttons",
	}).MustStatus(t, http.StatusOK)
	body := resp.Body()
	if !strings.Contains(string(body), "\"warning\"") {
		t.Fatalf("the preview did not warn about the missing action row: %s", body)
	}
	if !strings.Contains(string(body), "Acknowledge") {
		t.Errorf("the warning does not name what is lost: %s", body)
	}
}

func TestUpdateNotificationTemplateJudgesTheWholeRow(t *testing.T) {
	t.Parallel()

	w := newTmplWorld(t)
	resp := w.client.PATCH(t, "/notification-templates/"+tmplMine.String(), map[string]any{
		"source": "# {{ alert.name }}\n\nrewritten\n\n{{ actions }}",
	}).MustStatus(t, http.StatusOK)
	schema.Assert(t, "updateNotificationTemplate", http.StatusOK, resp.Body())

	if got, _ := resp.JSON(t)["data"].(map[string]any); got["version"] != float64(2) {
		t.Errorf("editing the source did not bump the version: %v", got["version"])
	}
}

func TestDeleteNotificationTemplateIsSoft(t *testing.T) {
	t.Parallel()

	w := newTmplWorld(t)
	w.client.DELETE("/notification-templates/"+tmplMine.String()).MustStatus(t, http.StatusNoContent)
	if len(w.templates.deleted) != 1 {
		t.Fatalf("the handler deleted %d rows, want 1", len(w.templates.deleted))
	}
}

// ⭐⭐ THE PREVIEW'S WHOLE TEACHING JOB. One document, two providers, and they
// must not read the same — if they ever do, the Dialect layer has gone inert and
// the portability claim is an assertion rather than a demonstration.
func TestPreviewNotificationTemplateShowsBothSpellingsOfOneDocument(t *testing.T) {
	t.Parallel()

	w := newTmplWorld(t)
	resp := w.client.POST(t, "/notification-templates/preview", map[string]any{
		"format": "card", "source": tmplSource,
	}).MustStatus(t, http.StatusOK)
	schema.Assert(t, "previewNotificationTemplate", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	renderings, _ := data["renderings"].([]any)
	if len(renderings) == 0 {
		t.Fatalf("the preview rendered no fixtures at all: %s", resp.Body())
	}
	first, _ := renderings[0].(map[string]any)
	spellings, _ := first["spellings"].([]any)
	if len(spellings) != 2 {
		t.Fatalf("the preview showed %d spellings, want 2 — an author shown one concludes markup is theirs to write", len(spellings))
	}
	a, _ := spellings[0].(map[string]any)
	b, _ := spellings[1].(map[string]any)
	if a["text"] == b["text"] {
		t.Errorf("both providers spelled the card identically; the dialect layer is inert:\n%v", a["text"])
	}
}

// ⛔ THE PREVIEW ANSWERS 200 WITH PROBLEMS, NOT 422. An author mid-edit has a
// broken template most of the time, and answering their keystroke with an error
// status and no rendering would make the editor useless when it is most needed.
func TestPreviewNotificationTemplateAnswersABrokenTemplateWithProblemsAnd200(t *testing.T) {
	t.Parallel()

	w := newTmplWorld(t)
	resp := w.client.POST(t, "/notification-templates/preview", map[string]any{
		"format": "card", "source": "| a | b |\n|---|---|",
	}).MustStatus(t, http.StatusOK)
	schema.Assert(t, "previewNotificationTemplate", http.StatusOK, resp.Body())

	if !strings.Contains(string(resp.Body()), "tables are not supported") {
		t.Errorf("the refusal does not say what is wrong or what to do instead: %s", resp.Body())
	}
}

/* -------------------------------------------------------------------------- */
/* Tenancy — another org's id is a 404, never a 403 and never their row       */
/* -------------------------------------------------------------------------- */

func TestNotificationTemplateEndpointsRefuseAnotherTenantsID(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		op   string
		call func(*apitest.Client) *apitest.Response
	}{
		{"getNotificationTemplate", func(c *apitest.Client) *apitest.Response {
			return c.GET("/notification-templates/" + apitest.StrangerID.String())
		}},
		{"updateNotificationTemplate", func(c *apitest.Client) *apitest.Response {
			return c.PATCH(t, "/notification-templates/"+apitest.StrangerID.String(),
				map[string]any{"enabled": false})
		}},
		{"deleteNotificationTemplate", func(c *apitest.Client) *apitest.Response {
			return c.DELETE("/notification-templates/" + apitest.StrangerID.String())
		}},
	} {
		t.Run(tc.op, func(t *testing.T) {
			t.Parallel()
			w := newTmplWorld(t)
			tc.call(w.client).MustStatus(t, http.StatusNotFound)
			if len(w.templates.patched) != 0 || len(w.templates.deleted) != 0 {
				t.Fatal("a stranger's request reached the store")
			}
		})
	}
}
