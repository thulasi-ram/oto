package api

import (
	"net/http"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
)

// The org-settings surface: the read that says WHAT IS IN FORCE AND WHY, and the
// write that was missing.
//
// ⭐ THE READ RETURNS FIVE THINGS, AND ALL FIVE ARE THE FEATURE:
//
//	settings    — the EFFECTIVE value, the one the hot path is using right now;
//	origins     — `default`, `org` or `config`, per key;
//	config_keys — for a `config` key, WHICH env var or file key set it;
//	shadowed    — the org override a `config` key is overriding, still stored;
//	bounds      — the server-side range, per key, and the reason for it.
//
// The last two arrived with the declarative layer, and they exist for the same
// reason as the first three. A badge reading "managed by configuration" with no
// key beside it tells an operator only that they cannot fix it here; a shadowed
// override that is hidden rather than shown is a number sitting in Postgres that
// nobody can see and that will take effect the moment a config key is deleted.
//
// Returning the effective value alone is the version of configurability that is
// worse than none: a screen showing `600` cannot tell an operator whether their
// org chose 600 or is riding oto's default, and those two answers behave
// identically today and diverge the moment the default moves. Returning the
// bounds alongside means the form is generated from the same table the server
// rejects with, so a UI cannot offer a value the API will refuse.

// OrgSettingsViewDTO is the body of `GET /api/v1/org/settings`.
type OrgSettingsViewDTO struct {
	// Settings are the effective values — overrides folded onto oto's defaults,
	// declarative configuration folded over both, and the result clamped to its
	// bounds. This is what oto is actually doing.
	Settings OrgSettingsDTO `json:"settings"`
	// Origins says, per key, whether the effective value is oto's shipped default
	// (`default`), this org's own (`org`), or this deployment's configuration
	// (`config`).
	Origins map[string]string `json:"origins"`
	// ConfigKeys names, for every key whose origin is `config`, the environment
	// variable or file key that set it.
	//
	// ⭐ WITHOUT IT THE BADGE IS A WALL. "Managed by configuration" tells an
	// operator they cannot change the value here and nothing about where they
	// can, which turns a five-second edit into an archaeology exercise across a
	// Helm chart, a values file and a Deployment's env block. Keys with any other
	// origin are ABSENT, not empty — a present key means "this is where to go".
	ConfigKeys map[string]string `json:"config_keys"`
	// Shadowed is this org's own overrides that are NOT in force, because
	// configuration is forcing something else. Only shadowed keys are present.
	//
	// ⭐ IT IS THE OTHER HALF OF THE SAME ANSWER. "You have an override of 900s,
	// but configuration is forcing 600s" is actionable; showing only the 600
	// leaves a stored 900 invisible, still in the database, and ready to take
	// effect the moment somebody deletes the config key.
	Shadowed OrgSettingsPatchDTO `json:"shadowed"`
	// Bounds is the server-side range of every integer key, with the reason.
	Bounds map[string]SettingBoundDTO `json:"bounds"`
}

// OrgSettingsPatchDTO is a PARTIAL settings body: every field is optional and an
// absent field means "this key is not set".
//
// It is the shape of what an org WROTE rather than of what is in force, which is
// why it cannot reuse OrgSettingsDTO: that type renders a complete, effective
// tuning, and a complete tuning cannot express "this org set two keys and nothing
// else".
type OrgSettingsPatchDTO struct {
	RefireGraceS        *int `json:"refire_grace_s,omitempty"`
	ResolveGraceS       *int `json:"resolve_grace_s,omitempty"`
	GroupCloseDelayS    *int `json:"group_close_delay_s,omitempty"`
	FlapThreshold       *int `json:"flap_threshold,omitempty"`
	FlapWindowS         *int `json:"flap_window_s,omitempty"`
	FlapDigestIntervalS *int `json:"flap_digest_interval_s,omitempty"`
	RawRetentionDays    *int `json:"raw_retention_days,omitempty"`
	EventRetentionMonth *int `json:"event_retention_months,omitempty"`

	UnackedReminderAfterS *int    `json:"unacked_reminder_after_s,omitempty"`
	DefaultVerbosity      *string `json:"default_verbosity,omitempty"`
	BroadcastOnResolved   *bool   `json:"broadcast_on_resolved,omitempty"`

	UnackedReminderMention            *string   `json:"unacked_reminder_mention,omitempty"`
	UnackedReminderMentionList        *[]string `json:"unacked_reminder_mention_list,omitempty"`
	UnackedReminderMentionMinSeverity *string   `json:"unacked_reminder_mention_min_severity,omitempty"`
}

// toOrgSettingsPatchDTO renders what an org wrote, field for field. It defaults
// nothing: an absent pointer stays absent, because the whole content of this
// type is which keys are set.
func toOrgSettingsPatchDTO(p domain.SettingsPatch) OrgSettingsPatchDTO {
	return OrgSettingsPatchDTO{
		RefireGraceS:          p.RefireGraceS,
		ResolveGraceS:         p.ResolveGraceS,
		GroupCloseDelayS:      p.GroupCloseDelayS,
		FlapThreshold:         p.FlapThreshold,
		FlapWindowS:           p.FlapWindowS,
		FlapDigestIntervalS:   p.FlapDigestIntervalS,
		RawRetentionDays:      p.RawRetentionDays,
		EventRetentionMonth:   p.EventRetentionMonth,
		UnackedReminderAfterS: p.UnackedReminderAfterS,
		DefaultVerbosity:      p.DefaultVerbosity,
		BroadcastOnResolved:   p.BroadcastOnResolved,

		UnackedReminderMention:            p.UnackedReminderMention,
		UnackedReminderMentionList:        p.UnackedReminderMentionList,
		UnackedReminderMentionMinSeverity: p.UnackedReminderMentionMinSeverity,
	}
}

// SettingBoundDTO is one knob's accepted range and the argument for it.
//
// `Why` is rendered into the 422 an out-of-range write receives, and it is
// returned here so the form can say the same thing BEFORE the write. A caller
// told only "invalid" tries a different wrong number.
type SettingBoundDTO struct {
	Min int    `json:"min"`
	Max int    `json:"max"`
	Why string `json:"why"`
}

// UpdateOrgSettingsRequest is the body of `PATCH /api/v1/org/settings`.
//
// ⚠️ EVERY FIELD IS A POINTER AND OMISSION MEANS "LEAVE IT ALONE". A settings
// API where an omitted key silently reverts to the default is an API that reverts
// nine settings every time somebody changes one. Returning a key to oto's default
// is done by NAMING it in `reset`, which is explicit and cannot happen by
// accident.
//
// There are no `validate` tags on the numbers on purpose: the range lives in
// `domain.Bounds`, which the service checks against the MERGED state. A tag here
// would be a second copy that could disagree, and R9 wants three copies that
// agree — the domain table, the OpenAPI schema and the DDL — not four.
type UpdateOrgSettingsRequest struct {
	RefireGraceS        *int `json:"refire_grace_s,omitempty"`
	ResolveGraceS       *int `json:"resolve_grace_s,omitempty"`
	GroupCloseDelayS    *int `json:"group_close_delay_s,omitempty"`
	FlapThreshold       *int `json:"flap_threshold,omitempty"`
	FlapWindowS         *int `json:"flap_window_s,omitempty"`
	FlapDigestIntervalS *int `json:"flap_digest_interval_s,omitempty"`
	RawRetentionDays    *int `json:"raw_retention_days,omitempty"`
	EventRetentionMonth *int `json:"event_retention_months,omitempty"`

	// UnackedReminderAfterS is the org default a notification policy inherits when
	// it names no delay of its own. ⛔ ONE STAGE, FOREVER (§G.9.1): a scalar, never
	// an array, and never a target other than the policy's own channel_ids.
	UnackedReminderAfterS *int `json:"unacked_reminder_after_s,omitempty"`
	// DefaultVerbosity is the fallback for a Channel that names no verbosity.
	DefaultVerbosity *string `json:"default_verbosity,omitempty"`
	// BroadcastOnResolved is ADR 0020's one configurable broadcast. Default off:
	// a broadcast cannot be un-sent, and on a busy channel this doubles traffic
	// for the least urgent fact oto has.
	BroadcastOnResolved *bool `json:"broadcast_on_resolved,omitempty"`

	// UnackedReminderMention is who the one unacked reminder addresses:
	// none | here | channel | list. DEFAULT `none`.
	//
	// ⚠️ `here` AND `channel` MAY DO NOTHING. Slack's own help says @here and
	// @channel "won't notify people ... when they're used in threads", and oto's
	// reminder is a thread reply that broadcasts — `reply_broadcast` moves where a
	// reference appears, and Slack documents no exception to the thread rule. An
	// explicit list of individuals and usergroups is the only form Slack documents
	// as notifying from that position.
	//
	// ⛔ THE LIST IS NOT A ROTA (§4.8, ADR 0013). It is a fixed audience chosen
	// once. It must never become time-aware and must never gain a second stage.
	UnackedReminderMention *string `json:"unacked_reminder_mention,omitempty"`
	// UnackedReminderMentionList is the explicit audience for mode `list`.
	UnackedReminderMentionList *[]string `json:"unacked_reminder_mention_list,omitempty"`
	// UnackedReminderMentionMinSeverity is the severity floor for attaching a
	// mention at all. DEFAULT `critical`: `@here` on every unacked warning is how
	// a channel learns to mute oto, and a muted channel hides the real incident.
	UnackedReminderMentionMinSeverity *string `json:"unacked_reminder_mention_min_severity,omitempty"`

	// Reset names keys to return to oto's shipped default. After a reset the key's
	// origin reports `default` again.
	Reset []string `json:"reset,omitempty" validate:"omitempty,max=32,dive,max=64"`
}

// toDomain splits the request into the patch to merge and the keys to clear.
//
// An unknown name in `reset` is REJECTED rather than ignored: a typo'd key that
// is silently dropped is a reset the operator believes happened and did not.
func (r UpdateOrgSettingsRequest) toDomain() (domain.SettingsPatch, []domain.SettingKey, error) {
	patch := domain.SettingsPatch{
		RefireGraceS:          r.RefireGraceS,
		ResolveGraceS:         r.ResolveGraceS,
		GroupCloseDelayS:      r.GroupCloseDelayS,
		FlapThreshold:         r.FlapThreshold,
		FlapWindowS:           r.FlapWindowS,
		FlapDigestIntervalS:   r.FlapDigestIntervalS,
		RawRetentionDays:      r.RawRetentionDays,
		EventRetentionMonth:   r.EventRetentionMonth,
		UnackedReminderAfterS: r.UnackedReminderAfterS,
		DefaultVerbosity:      r.DefaultVerbosity,
		BroadcastOnResolved:   r.BroadcastOnResolved,

		UnackedReminderMention:            r.UnackedReminderMention,
		UnackedReminderMentionList:        r.UnackedReminderMentionList,
		UnackedReminderMentionMinSeverity: r.UnackedReminderMentionMinSeverity,
	}

	known := make(map[string]domain.SettingKey, len(domain.AllSettingKeys()))
	for _, k := range domain.AllSettingKeys() {
		known[string(k)] = k
	}

	reset := make([]domain.SettingKey, 0, len(r.Reset))
	var bad []errs.Violation
	for _, name := range r.Reset {
		k, ok := known[name]
		if !ok {
			bad = append(bad, errs.Violation{
				Field: "reset", Code: "unknown_key",
				Message: name + " is not a settings key",
			})
			continue
		}
		reset = append(reset, k)
	}
	if len(bad) > 0 {
		return domain.SettingsPatch{}, nil, errs.Validation("invalid_org_settings",
			"reset names a key that does not exist", bad...)
	}
	return patch, reset, nil
}

// toOrgSettingsViewDTO renders the effective settings, their origins and the
// bounds. It takes the whole Org because the effective value and its origin must
// come from ONE read — assembling them from two would let the API report an
// origin for a value it is not showing.
func toOrgSettingsViewDTO(o domain.Org) OrgSettingsViewDTO {
	origins := make(map[string]string, len(domain.AllSettingKeys()))
	configKeys := map[string]string{}
	for _, k := range domain.AllSettingKeys() {
		origins[string(k)] = string(o.Origin(k))
		if ck := o.ConfigKey(k); ck != "" {
			configKeys[string(k)] = ck
		}
	}

	bounds := make(map[string]SettingBoundDTO, len(domain.IntKeys()))
	for _, k := range domain.IntKeys() {
		b, ok := domain.Bounds(k)
		if !ok {
			continue
		}
		bounds[string(k)] = SettingBoundDTO{Min: b.Min, Max: b.Max, Why: b.Why}
	}

	return OrgSettingsViewDTO{
		Settings:   toOrgSettingsDTO(o.Settings),
		Origins:    origins,
		ConfigKeys: configKeys,
		Shadowed:   toOrgSettingsPatchDTO(o.Shadowed()),
		Bounds:     bounds,
	}
}

// getOrgSettings is `GET /api/v1/org/settings` — operationId `getOrgSettings`.
func (rt *Router) getOrgSettings(w http.ResponseWriter, r *http.Request) {
	started := rt.clk.Now()

	_, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	org, err := rt.svc.GetOrg(r.Context(), scope)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	httpx.Data(w, r, http.StatusOK, toOrgSettingsViewDTO(org), started)
}

// updateOrgSettings is `PATCH /api/v1/org/settings` — operationId
// `updateOrgSettings`.
//
// ⛔ THE BOUNDS ARE THE SERVER'S, NOT THE FORM'S. The service validates the
// MERGED state, so a write cannot slip a value past by relying on a key it did
// not send, and a `refire_grace_s` of 0 is refused here whatever the UI would
// have allowed — that value is a Slack thread per transition.
//
// ⛔ A KEY THE DEPLOYMENT'S CONFIGURATION MANAGES IS REFUSED WITH 409, and the
// problem's violations name the config key that owns it. Accepting the write
// would store a number that is never in force and that reverts, visibly, on the
// next deploy — the exact mystery the declarative layer exists to end.
//
// It takes effect immediately and everywhere: nothing between this write and the
// notify worker's next evaluation holds a copy of these numbers, so there is no
// restart, no cache flush and no window in which one pod is running the old
// tuning and another the new.
func (rt *Router) updateOrgSettings(w http.ResponseWriter, r *http.Request) {
	started := rt.clk.Now()

	_, scope, err := authn.Scope(r.Context())
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	req, err := httpx.Bind[UpdateOrgSettingsRequest](w, r)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	patch, reset, err := req.toDomain()
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	org, err := rt.svc.UpdateOrgSettings(r.Context(), scope, patch, reset)
	if err != nil {
		httpx.WriteProblem(w, r, err)
		return
	}

	httpx.Data(w, r, http.StatusOK, toOrgSettingsViewDTO(org), started)
}
