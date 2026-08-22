package api

import (
	"net/http"
	"strconv"

	"github.com/thulasiram/oto/internal/channels/service"
	"github.com/thulasiram/oto/internal/channels/template"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// previewDialects are the spellings a preview shows, in the order it shows them.
//
// ⭐⭐ SHOWING TWO IS THE ENDPOINT'S WHOLE TEACHING JOB. One Markdown document
// compiles to Slack's `*bold*` and to a webhook consumer's plain words, and an
// author shown one spelling concludes that markup is theirs to write. It is also
// the only way the portability claim is visible rather than asserted.
//
// ⚠️ IT IS A LITERAL SLICE AND THAT IS THE WRONG SHAPE. Its owed work is a Dialect
// REGISTRY, so that a provider shipped without one refuses to construct. Until it
// exists, a Dialect added and forgotten is a Dialect this preview does not show.
var previewDialects = []template.Dialect{template.SlackDialect{}, template.PlainDialect{}}

// listTemplates serves GET /api/v1/notification-templates.
func (rt *Router) listTemplates(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.requireTemplateStore(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	p := httpx.NewParams(r, "limit", "cursor", "include_deleted")
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	limit := p.Limit()
	includeDeleted := boolOr(p.Bool("include_deleted"), false)
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// The filter is in the hash: `include_deleted` changes the sequence completely,
	// so a cursor minted over one and replayed against the other describes a
	// position that no longer exists — and nothing would look wrong.
	cursor, err := httpx.DecodeCursor(p.Cursor(), httpx.FilterHash(
		"notification_templates",
		"include_deleted="+strconv.FormatBool(includeDeleted),
	))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	rows, next, err := rt.templates.List(r.Context(), scope, includeDeleted, httpx.Keyset(limit, cursor))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]NotificationTemplateDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, notificationTemplateDTO(row))
	}
	httpx.List(w, r, out, httpx.PageOf(next, limit), started)
}

// createTemplate serves POST /api/v1/notification-templates.
//
// ⛔ NO `Idempotency-Key` HANDLING, for the reason createConnection states: a
// template is settings, and a retried create leaves a duplicate row an operator
// can see and delete — not a second message a human has already read.
//
// ⭐ THE GATE IS `service.ValidateTemplate` AND IT MUST RENDER TO RUN. An unknown
// filter is a render-time error in Liquid, not a parse-time one, so a save that
// only parsed would accept `{{ x | no_such_filter }}` and discover it at 03:00 on
// a real card.
func (rt *Router) createTemplate(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.requireTemplateStore(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto, err := httpx.Bind[CreateNotificationTemplateRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := refuseTemplate(service.ValidateTemplate(dto.Name, dto.Provider, dto.Format, dto.Source)); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	row, err := rt.templates.Create(r.Context(), scope, dto.toNewNotificationTemplate())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusCreated, notificationTemplateDTO(row), started)
}

// getTemplate serves GET /api/v1/notification-templates/{id}.
//
// A soft-deleted row is served rather than hidden. A delivery records the template
// id that produced it, so "why did my card read like that" has to stay answerable
// after the template is gone — the same argument that made the delete soft.
func (rt *Router) getTemplate(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.requireTemplateStore(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	row, err := rt.templates.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, notificationTemplateDTO(row), started)
}

// updateTemplate serves PATCH /api/v1/notification-templates/{id}.
func (rt *Router) updateTemplate(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.requireTemplateStore(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto, err := httpx.Bind[UpdateNotificationTemplateRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if dto.IsEmpty() {
		httpx.WriteProblem(w, r, errs.Validation("validation_failed",
			"supply at least one field to change",
			errs.Violation{Field: "", Code: "min_properties", Message: "at least one property is required"}))
		return
	}

	existing, err := rt.templates.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if existing.DeletedAt != nil {
		httpx.WriteProblem(w, r, errs.NotFound("template_deleted", "this template has been deleted"))
		return
	}

	merged := patchedTemplate(existing, dto)
	if err := refuseTemplate(service.ValidateTemplate(
		merged.Name, merged.Provider, merged.Format, merged.Source,
	)); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	row, err := rt.templates.Update(r.Context(), scope, id, dto.toPatch())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, notificationTemplateDTO(row), started)
}

// deleteTemplate serves DELETE /api/v1/notification-templates/{id}.
func (rt *Router) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.requireTemplateStore(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.templates.Delete(r.Context(), scope, id); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusNoContent, nil)
}

// previewTemplate serves POST /api/v1/notification-templates/preview.
//
// ⭐ IT ANSWERS 200 WITH PROBLEMS RATHER THAN 422, AND THAT IS THE POINT. An
// author mid-edit has a broken template most of the time; answering their
// keystroke with an error status and no rendering would make the editor useless
// exactly when it is most needed. The problems and the renderings come back
// together, so the screen can show what works beside what does not.
func (rt *Router) previewTemplate(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	if _, err := scopeOf(r); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto, err := httpx.Bind[PreviewNotificationTemplateRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	format := template.Format(dto.Format)
	problems := template.Validate(format, dto.Source)

	// ⛔ THE ONE PROBLEM THAT IS A `422` RATHER THAN A RESULT. A source over the
	// ceiling is not a template that renders badly, it is a body this endpoint
	// declines to read — and rendering sixteen kilobytes of it back seven times
	// over would answer a mistake with a wall.
	for _, p := range problems {
		if p.Kind == template.ProblemTooLong {
			httpx.WriteProblem(w, r, errs.Validation("validation_failed", "1 field failed validation.",
				errs.Violation{Field: "source", Code: string(p.Kind), Message: p.Message}))
			return
		}
	}

	out := TemplatePreviewDTO{
		Format:     dto.Format,
		Source:     dto.Source,
		Problems:   templateProblems(problems),
		Renderings: []TemplateRenderingDTO{},
	}
	// A template that does not compile has nothing to show, and `problems` already
	// says why. Compile's refusals are a subset of Validate's, so this branch can
	// never be silent.
	if compiled, err := template.Compile(format, dto.Source); err == nil {
		out.Source = compiled.Source
		out.Renderings = renderFixtures(compiled, format)
	}
	httpx.Data(w, r, http.StatusOK, out, started)
}

// ------------------------------------------------------------------- helpers

// renderFixtures renders one compiled template against the whole shipped corpus,
// once per Dialect.
//
// ⚠️ A RENDER FAILURE IS RECORDED, NOT PROPAGATED. At delivery a failing template
// falls back to oto's own card rather than killing the message, so a preview that
// stopped at the first bad fixture would describe a behaviour oto does not have —
// and would hide the six fixtures that worked.
func renderFixtures(compiled *template.Template, format template.Format) []TemplateRenderingDTO {
	fixtures := template.Fixtures()
	out := make([]TemplateRenderingDTO, 0, len(fixtures))
	for _, f := range fixtures {
		in, links := f.Bind(format)
		row := TemplateRenderingDTO{
			Fixture:        f.Name,
			Representative: f.Representative,
			Spellings:      make([]TemplateSpellingDTO, 0, len(previewDialects)),
		}

		var doc *template.Document
		var docProblems []template.Problem
		if format == template.FormatCard {
			doc, docProblems = compiled.RenderCard(in, links)
			row.HasActions = doc != nil && doc.HasActions
		}

		for _, d := range previewDialects {
			s := TemplateSpellingDTO{Dialect: d.Name()}
			switch format {
			case template.FormatCard:
				if doc != nil {
					s.Text = doc.Spelled(d)
				} else if len(docProblems) > 0 {
					s.Error = docProblems[0].Message
				}
			case template.FormatText:
				text, err := compiled.RenderText(in, d, links)
				s.Text = text
				if err != nil {
					s.Error = err.Error()
				}
			case template.FormatRaw:
				// ⚠️ RAW IS SLACK'S BLOCK KIT AND HAS NO SECOND SPELLING. Showing the
				// same JSON twice under two dialect headings would assert a
				// portability this format does not have — which is the one thing an
				// author choosing it most needs to understand.
				if _, isSlack := d.(template.SlackDialect); !isSlack {
					continue
				}
				raw, err := compiled.RenderRaw(in)
				s.Text = string(raw)
				if err != nil {
					s.Error = err.Error()
				}
			}
			row.Spellings = append(row.Spellings, s)
		}
		out = append(out, row)
	}
	return out
}

// templateProblems maps the save-time gate's verdict onto the wire.
func templateProblems(ps []template.Problem) []TemplateProblemDTO {
	out := make([]TemplateProblemDTO, 0, len(ps))
	for _, p := range ps {
		out = append(out, TemplateProblemDTO{
			Kind: string(p.Kind), Field: "source", Message: p.Message, Fixture: p.Fixture,
		})
	}
	return out
}

// refuseTemplate turns the save-time gate's verdict into a `422`, or nil.
//
// ⛔ A WARNING IS NOT A REFUSAL. A card with no `{{ actions }}` carries no
// Acknowledge button, and the operator is allowed to ship that — an alert stays
// acknowledgeable from the console and from `POST /api/v1/cases/{id}/ack`. It
// comes back in the preview and it does not stop a save.
func refuseTemplate(vs []errs.Violation) error {
	blocking := make([]errs.Violation, 0, len(vs))
	for _, v := range vs {
		if v.Code != string(template.ProblemWarning) {
			blocking = append(blocking, v)
		}
	}
	if len(blocking) == 0 {
		return nil
	}
	noun := "fields"
	if len(blocking) == 1 {
		noun = "field"
	}
	return errs.Validation("validation_failed",
		strconv.Itoa(len(blocking))+" "+noun+" failed validation.", blocking...)
}

func (rt *Router) requireTemplateStore() error {
	return requireDependency(rt.templates != nil, "channels_template_store_unavailable",
		"the notification template store is not configured in this deployment")
}
