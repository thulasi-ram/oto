package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/render/wording"
	"github.com/thulasiram/oto/internal/channels/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// DefaultWordingPriority is `wordings.priority`'s own DDL default, restated
// because the contract publishes it: a caller that omits `priority` lands beside
// every other unprioritised row rather than ahead of all of them at zero.
const DefaultWordingPriority = 100

// previewDialects are the spellings a preview shows, in the order it shows them.
//
// ⭐⭐ SHOWING TWO IS THE ENDPOINT'S WHOLE TEACHING JOB (ADR 0048). A filter emits
// a NEUTRAL mark and a Dialect spells it, so `{{ service | code }}` is a backtick
// on Slack and nothing at all on a webhook. An author shown one spelling
// concludes that markup is theirs to write; an author shown both cannot.
//
// ⚠️ IT IS A LITERAL SLICE AND ADR 0048 ALREADY CALLS THAT THE WRONG SHAPE — its
// owed work is a Dialect REGISTRY, so that a provider shipped without one refuses
// to construct. This list will be that registry's first caller. Until it exists,
// a Dialect added and forgotten is a Dialect this preview does not show, which is
// the smaller half of the same defect the ADR records against
// TestNoWordingCanEmitAnAudience.
var previewDialects = []wording.Dialect{wording.SlackDialect{}, wording.PlainDialect{}}

// listWordings serves GET /api/v1/wordings.
func (rt *Router) listWordings(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.wordings != nil, "channels_wording_store_unavailable",
		"the wording store is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	p := httpx.NewParams(r, "limit", "cursor", "channel_id", "include_deleted")
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	limit := p.Limit()
	var channelID *uuid.UUID
	if p.Has("channel_id") {
		id := p.UUID("channel_id")
		channelID = &id
	}
	includeDeleted := boolOr(p.Bool("include_deleted"), false)
	if err := p.Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// ⛔ BOTH FILTERS ARE IN THE HASH. A cursor minted over one destination's
	// exceptions and replayed against the whole tenant describes a position in a
	// sequence that no longer exists, and `include_deleted` changes the sequence
	// just as completely — without them the server would serve a page from the
	// middle of the wrong list and nothing would look wrong.
	cursor, err := httpx.DecodeCursor(p.Cursor(), httpx.FilterHash(
		"wordings",
		"channel_id="+uuidOrEmpty(channelID),
		"include_deleted="+strconv.FormatBool(includeDeleted),
	))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	rows, next, err := rt.wordings.List(r.Context(), scope, channelID, includeDeleted, httpx.Keyset(limit, cursor))
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	out := make([]WordingDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, wordingDTO(row))
	}
	httpx.List(w, r, out, httpx.PageOf(next, limit), started)
}

// createWording serves POST /api/v1/wordings.
//
// ⛔ NO `Idempotency-Key` HANDLING, for the reason createConnection states: a
// Wording is settings, and a retried create leaves a duplicate row an operator
// can see and delete — not a second message a human has already read. The one
// unrepeatable act in this module remains `testChannel`.
//
// ⭐ THE GATE IS `service.ValidateWording` AND IT MUST RENDER TO RUN. An unknown
// filter is a render-time error in Liquid, not a parse-time one, so a save that
// only parsed would accept `{{ x | no_such_filter }}` and discover it at 03:00 on
// a real card.
func (rt *Router) createWording(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, err := scopeOf(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.requireWordingWriteDeps(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto, err := httpx.Bind[CreateWordingRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	matchers := domainMatchers(dto.Matchers)
	if err := refuseWording(service.ValidateWording(
		dto.Stanza, dto.Template, matchers, dto.Reasons, intOr(dto.Priority, DefaultWordingPriority),
	)); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.checkWordingChannel(r.Context(), scope, dto.ChannelID); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	row, err := rt.wordings.Create(r.Context(), scope, dto.toNewWording())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusCreated, wordingDTO(row), started)
}

// getWording serves GET /api/v1/wordings/{id}.
//
// A soft-deleted row is served rather than hidden, unlike getChannel's. A
// delivery's persisted wording set names the rows that produced a card, so
// "why did my card read like that" has to stay answerable after the Wording is
// gone — which is the same argument that made the delete soft.
func (rt *Router) getWording(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.wordings != nil, "channels_wording_store_unavailable",
		"the wording store is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	row, err := rt.wordings.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, wordingDTO(row), started)
}

// updateWording serves PATCH /api/v1/wordings/{id}.
//
// ⛔ THE STANZA IS NOT PATCHABLE and the request has no field for it. Moving a
// Wording from `body` to `title` is not an edit of that Wording, it is a
// different Wording — the read set differs, the budget differs, and the row's
// history would claim it had always been the new one.
//
// A supplied `template` is re-validated against the STORED stanza, because that
// is the stanza it will render into. Validating against anything the request
// could name is what the missing field prevents.
func (rt *Router) updateWording(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := rt.requireWordingWriteDeps(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto, err := httpx.Bind[UpdateWordingRequest](w, r)
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

	existing, err := rt.wordings.Get(r.Context(), scope, id)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if existing.DeletedAt != nil {
		httpx.WriteProblem(w, r, errs.NotFound("wording_deleted", "this wording has been deleted"))
		return
	}

	// The patch is validated as the WHOLE ROW IT WOULD PRODUCE, not as the fields
	// it carries. A template is refused or accepted on the stanza it lands in, and
	// a clause is bounded by its total size — so validating the delta alone would
	// let two individually-legal patches build an illegal row.
	merged := patchedWording(existing, dto)
	if err := refuseWording(service.ValidateWording(
		merged.Stanza, merged.Template, merged.Matchers, merged.Reasons, merged.Priority,
	)); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	row, err := rt.wordings.Update(r.Context(), scope, id, dto.toPatch())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.Data(w, r, http.StatusOK, wordingDTO(row), started)
}

// deleteWording serves DELETE /api/v1/wordings/{id}.
//
// Soft, like every other delete here, and for a reason particular to this table:
// a delivery's persisted wording set names the rows that produced a card, and a
// hard delete would make an old card's provenance unreadable. There is no `409`
// counterpart to deleteChannel's — nothing references a Wording, and a card that
// loses its Wording simply reads in oto's own voice again.
func (rt *Router) deleteWording(w http.ResponseWriter, r *http.Request) {
	scope, id, err := rt.subject(r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := requireDependency(rt.wordings != nil, "channels_wording_store_unavailable",
		"the wording store is not configured in this deployment"); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	if err := rt.wordings.Delete(r.Context(), scope, id); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusNoContent, nil)
}

// previewWording serves POST /api/v1/wordings/preview.
//
// ⭐⭐ THIS IS THE FEATURE'S AUTHORING STORY, AND ADR 0037 NAMES IT. It writes
// nothing, takes no `Idempotency-Key`, and answers with the same template
// rendered against the whole shipped corpus — the ordinary cards and the nasty
// ones — in every Dialect oto can spell.
//
// ⛔ A TEMPLATE THAT FAILS VALIDATION IS STILL A `200`. A preview that refused
// would tell an author less than one that shows them what broke: the refusal and
// the output belong in the same round trip, because the fix is usually visible
// only when both are on screen. `422` is reserved for a malformed REQUEST — an
// absent stanza, an absent template, a source over the one-line ceiling — which
// is a question the endpoint cannot answer rather than an answer it does not
// like.
func (rt *Router) previewWording(w http.ResponseWriter, r *http.Request) {
	started := rt.now()

	if _, err := scopeOf(r); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}
	if err := httpx.NewParams(r).Err(); err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	dto, err := httpx.Bind[PreviewWordingRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	// The `when` clause is not previewed, so the vocabulary bounds are given
	// values that pass: what is under test here is the template.
	problems := service.ValidateWording(dto.Stanza, dto.Template, nil, nil, DefaultWordingPriority)
	// ⛔ THE ONE PROBLEM THAT IS A `422` RATHER THAN A RESULT. A source over the
	// ceiling is not a template that renders badly, it is a body this endpoint
	// declines to read — and rendering two kilobytes of it back seven times over
	// would answer a mistake with a wall.
	if v, ok := overCeiling(problems); ok {
		httpx.WriteProblem(w, r, errs.Validation("validation_failed",
			"1 field failed validation.", v))
		return
	}

	out := WordingPreviewDTO{
		Stanza:     dto.Stanza,
		Template:   dto.Template,
		Problems:   wordingProblems(problems),
		Renderings: []WordingRenderingDTO{},
	}
	// A template that does not compile has nothing to show, and `problems` already
	// says why. Compile's own refusals are a subset of Validate's, so this branch
	// can never be silent.
	compiled, err := wording.Compile(wording.StanzaID(dto.Stanza), dto.Template)
	if err == nil {
		out.Template = compiled.Source
		out.Renderings = renderFixtures(compiled)
	}
	httpx.Data(w, r, http.StatusOK, out, started)
}

// ------------------------------------------------------------------- helpers

// renderFixtures renders one compiled template against the whole shipped corpus,
// once per Dialect.
//
// ⚠️ A RENDER FAILURE IS RECORDED, NOT PROPAGATED. At delivery a failing Stanza
// falls back to oto's own Go text rather than killing the card, so a preview that
// stopped at the first bad fixture would describe a behaviour oto does not have —
// and would hide the six fixtures that worked.
func renderFixtures(compiled *wording.Wording) []WordingRenderingDTO {
	fixtures := wording.Fixtures()
	out := make([]WordingRenderingDTO, 0, len(fixtures))
	for _, f := range fixtures {
		spellings := make([]WordingSpellingDTO, 0, len(previewDialects))
		for _, d := range previewDialects {
			text, err := compiled.Render(f.Input, d)
			s := WordingSpellingDTO{Dialect: d.Name(), Text: text}
			if err != nil {
				s.Error = optionalString(err.Error())
			}
			spellings = append(spellings, s)
		}
		out = append(out, WordingRenderingDTO{
			Fixture:        f.Name,
			Representative: f.Representative,
			Spellings:      spellings,
		})
	}
	return out
}

// wordingProblems maps the save-time gate's verdict onto the wire.
func wordingProblems(vs []errs.Violation) []WordingProblemDTO {
	out := make([]WordingProblemDTO, 0, len(vs))
	for _, v := range vs {
		out = append(out, WordingProblemDTO{Field: v.Field, Code: v.Code, Message: v.Message})
	}
	return out
}

// refuseWording turns the save-time gate's verdict into a `422`, or nil.
//
// It is the same mapping `validateConfig` performs for a provider schema failure:
// the violations arrive already carrying the field a form control is bound to, so
// nothing here re-words them.
func refuseWording(vs []errs.Violation) error {
	if len(vs) == 0 {
		return nil
	}
	noun := "fields"
	if len(vs) == 1 {
		noun = "field"
	}
	return errs.Validation("validation_failed",
		strconv.Itoa(len(vs))+" "+noun+" failed validation.", vs...)
}

// overCeiling finds the one violation that means "this body is too big to
// answer", as opposed to "this template is wrong".
func overCeiling(vs []errs.Violation) (errs.Violation, bool) {
	for _, v := range vs {
		if v.Field == "template" && v.Code == string(wording.ProblemTooLong) {
			return v, true
		}
	}
	return errs.Violation{}, false
}

// patchedWording is the row an update WOULD produce, for the gate to judge.
func patchedWording(existing domain.Wording, p UpdateWordingRequest) domain.Wording {
	out := existing
	if p.Template != nil {
		out.Template = *p.Template
	}
	if p.Matchers != nil {
		out.Matchers = domainMatchers(*p.Matchers)
	}
	if p.Reasons != nil {
		out.Reasons = *p.Reasons
	}
	if p.Priority != nil {
		out.Priority = *p.Priority
	}
	return out
}

// checkWordingChannel refuses a Wording bound to a destination this tenant does
// not own, or one that has been deleted.
//
// ⛔ THIS IS A CROSS-TABLE INVARIANT NO CHECK CONSTRAINT CAN SEE, the same shape
// as checkConnectionType's. `wordings.channel_id` references `channels(id)`
// ALONE, with no org term, so another tenant's channel id satisfies the foreign
// key perfectly. The row would then be inert — `Resolve` scopes by org, so it
// could never spell a word on anybody's card — and inert is exactly the problem:
// an operator would see their Wording saved, see it never apply, and have nothing
// to read that explained it.
func (rt *Router) checkWordingChannel(
	ctx context.Context, scope db.TenantScope, channelID *uuid.UUID,
) error {
	if channelID == nil {
		return nil // the org-wide house voice names no destination
	}
	inst, err := rt.channels.Get(ctx, scope, *channelID)
	if err != nil {
		if errs.IsKind(err, errs.KindNotFound) {
			return errs.Validation("validation_failed", "1 field failed validation.",
				errs.Violation{
					Field: "channel_id", Code: "not_found",
					Message: "no such channel in this organisation",
				})
		}
		return err
	}
	if inst.Deleted() {
		return errs.Validation("validation_failed", "1 field failed validation.",
			errs.Violation{
				Field: "channel_id", Code: "deleted",
				Message: "this channel has been deleted",
			})
	}
	return nil
}

func (rt *Router) requireWordingWriteDeps() error {
	if err := requireDependency(rt.wordings != nil, "channels_wording_store_unavailable",
		"the wording store is not configured in this deployment"); err != nil {
		return err
	}
	return requireDependency(rt.channels != nil, "channels_store_unavailable",
		"the channel store is not configured in this deployment")
}

// uuidOrEmpty renders an optional id for a filter hash. The empty string is the
// honest spelling of "no filter": it is not a UUID any caller can send, so it
// cannot collide with one.
func uuidOrEmpty(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}
