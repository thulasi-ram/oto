package domain

import (
	"sort"
	"time"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// The per-org tuning surface: the keys, their server-side bounds, and the
// distinction between "this org chose this" and "this is what oto ships with".
//
// ⭐ THE TWO RULES THIS FILE EXISTS FOR:
//
//  1. **Bounds are enforced HERE, on the server, not in the settings form.** A UI
//     is a convenience for the common case; it is not a boundary. `refire_grace`
//     of zero would put a Slack thread on every transition of every alert, and
//     the request that sets it will arrive from `curl` long before it arrives
//     from a form. Every bound below is checked on the WRITE path and clamped on
//     the READ path.
//
//  2. **An effective value is useless without its origin.** The worst version of
//     configurability is a screen showing `600` with no way to tell whether the
//     org chose 600 or is simply running the shipped default — because those two
//     answers behave identically today and diverge the moment the default moves.
//     `SettingsPatch` keeps the org's OWN writes, so `Origin` can answer.
//
// ⚠️ Every bound here is one of THREE copies of the same rule (CONTEXT.md R9):
// this table, the OpenAPI schema in `api/openapi/openapi.yaml`, and — for
// `unacked_reminder_after_s` — the `policies_reminder_ck` DDL CHECK. Changing one
// without the others is what turns a 422 into a 500.

// SettingKey is the closed set of org-tunable keys, spelled exactly as
// `orgs.settings` stores them.
type SettingKey string

// The tunable keys.
const (
	KeyRefireGrace         SettingKey = "refire_grace_s"
	KeyResolveGrace        SettingKey = "resolve_grace_s"
	KeyGroupCloseDelay     SettingKey = "group_close_delay_s"
	KeyFlapThreshold       SettingKey = "flap_threshold"
	KeyFlapWindow          SettingKey = "flap_window_s"
	KeyFlapDigestInterval  SettingKey = "flap_digest_interval_s"
	KeyStormThreshold      SettingKey = "storm_threshold"
	KeyStormWindow         SettingKey = "storm_window_s"
	KeyStormCooldown       SettingKey = "storm_cooldown_s"
	KeyRawRetention        SettingKey = "raw_retention_days"
	KeyEventRetention      SettingKey = "event_retention_months"
	KeyUnackedReminder     SettingKey = "unacked_reminder_after_s"
	KeyDefaultVerbosity    SettingKey = "default_verbosity"
	KeyBroadcastOnResolved SettingKey = "broadcast_on_resolved"

	// KeyUnackedReminderMention is WHO the one unacked reminder addresses:
	// none | here | channel | list. DEFAULT `none`, on evidence — see mention.go.
	KeyUnackedReminderMention SettingKey = "unacked_reminder_mention"
	// KeyUnackedReminderMentionList is the explicit audience for mode `list`.
	KeyUnackedReminderMentionList SettingKey = "unacked_reminder_mention_list"
	// KeyUnackedReminderMentionMinSeverity gates mentions on severity. DEFAULT
	// `critical`: `@here` on every unacked warning is how a channel learns to mute
	// oto, and a muted channel hides the real incident.
	KeyUnackedReminderMentionMinSeverity SettingKey = "unacked_reminder_mention_min_severity"
)

// Origin says where an effective value came from.
//
// It is a two-value enum on purpose. "Inherited from a parent org" and "set by an
// operator override" are answers to a question oto does not have — there is one
// tenant level and one shipped default, and inventing a third origin would be
// inventing a hierarchy.
type Origin string

// The origins.
const (
	// OriginDefault means oto's shipped value is in force; this org wrote nothing.
	OriginDefault Origin = "default"
	// OriginOrg means this org wrote this value and it overrides the default.
	OriginOrg Origin = "org"
)

// Bound is the server-side range of one integer knob, with the reason the range
// is what it is. The reason is a field rather than a comment because it is
// rendered into the 422 an out-of-range write gets back: a caller told only
// "invalid" tries a different wrong number.
type Bound struct {
	Min, Max int
	Why      string
}

// Contains reports whether v is inside the bound.
func (b Bound) Contains(v int) bool { return v >= b.Min && v <= b.Max }

// Clamp forces v into the bound. It is the READ-path counterpart to the
// write-path rejection: a row written before a bound existed, or by a migration,
// must never produce a pathological runtime value just because it is already in
// the database. Rejecting on read would turn a legacy row into a 500 on every
// alert, which is the failure mode of validating the wrong direction.
func (b Bound) Clamp(v int) int {
	switch {
	case v < b.Min:
		return b.Min
	case v > b.Max:
		return b.Max
	default:
		return v
	}
}

// MinRefireGraceSeconds is `refire_grace_s`'s floor, and it is DERIVED rather
// than chosen: it is twice the §C.5 ingest replay window
// (`ingestion/domain.DedupTTL`, 5 minutes).
//
// Twice, not once, so that the reachable band is as wide as the window it has to
// clear. At the floor a re-fire between 5 and 10 minutes after a resolve is both
// visible to ingest and inside the grace, which is the T8 case; below the floor
// that band is empty and `refire_grace` is a control with no effect.
//
// ⛔ It is not imported from `ingestion` — a settings vocabulary must not depend
// on the ingest path — so the two numbers are tied by a test that imports both
// (`TestTheReplayWindowIsStrictlyInsideRefireGrace`) rather than by a compile
// error. Change one and that test tells you about the other.
const MinRefireGraceSeconds = 600

// settingBounds is the table. Every integer key has an entry; a key with no entry
// is a key nothing can validate, so `Validate` treats a miss as a bug rather than
// as permission.
var settingBounds = map[SettingKey]Bound{
	// ⛔ THE FLOOR IS `2 × ingest_dedup`'s REPLAY WINDOW, AND THE COUPLING IS THE
	// POINT OF THE NUMBER.
	//
	// §C.5 suppresses a replayed batch — an HA sibling, a retry — for
	// `ingestion/domain.DedupTTL`. A re-fire whose alert set is unchanged produces
	// a byte-identical dedup key, so a `refire_grace` at or below that window makes
	// T8 UNREACHABLE: every re-fire oto can still observe is, by arithmetic,
	// already outside the grace and opens a new generation and a new Slack root —
	// precisely the wall of near-identical messages oto exists to prevent, produced
	// by a setting that looks like it should have prevented it.
	//
	// They WERE equal (both ten minutes) and the first live tester had to alter the
	// alert set to exercise re-fire at all. `MinRefireGrace` is derived from the
	// replay window rather than picked, so raising one without the other cannot
	// silently re-close the gap; `TestTheReplayWindowIsStrictlyInsideRefireGrace`
	// pins the relationship. Zero remains forbidden outright: it is a Slack thread
	// per transition. See docs/setup/tuning.md.
	//
	// ⭐ THE FLOOR IS NOT THE DEFAULT, AND IT USED TO BE. `DefaultRefireGrace` sat
	// exactly ON this floor, which meant oto shipped the lowest value it was
	// willing to accept and called it a recommendation. It is now 1200 (ADR 0026),
	// derived from the RULES rather than from the transport: the grace clock starts
	// at the upstream `ended_at`, so a re-fire must pay the rule's whole `for:`
	// dwell before oto can see it, and the modal real rule's `for:` is 15m. This
	// bound is untouched — it is a statement about what oto can physically observe,
	// and a real cluster with a 30s `group_interval` still cannot use a grace below
	// it, because the §C.5 replay window is a property of Alertmanager's retry
	// budget rather than of anybody's route timing.
	KeyRefireGrace: {Min: MinRefireGraceSeconds, Max: 86400,
		Why: "seconds, 600..86400: the floor is twice the §C.5 ingest replay window, because a re-fire inside that window is dropped as a duplicate delivery and the grace can never be reached; above a day two separate incidents merge into one case and the history lies"},
	KeyResolveGrace: {Min: 60, Max: 86400,
		Why: "seconds, 60..86400: must exceed the EndsAt lease Prometheus refreshes (typically 3-4 minutes) or one missed scrape looks like an expiry"},
	// The SECOND relationship, and the one the shipped defaults used to break:
	// keep it at or above `refire_grace`. Closing a generation freezes its thread,
	// so a re-fire that oto classified as "the same problem coming back" still gets
	// a brand-new root card when the generation closed first — the grace reopens
	// the case and buys nothing visible. It is not enforced as a cross-key
	// bound because a cross-key bound would reject a legal partial PATCH that
	// merely arrives in the wrong order; the settings screen warns instead.
	KeyGroupCloseDelay: {Min: 60, Max: 86400,
		Why: "seconds, 60..86400: keep at or above group_interval, or a generation closes between two batches of the same incident, and at or above refire_grace, or a re-fire inside the grace finds a closed generation and gets a new root message anyway"},

	// Below 3, one ordinary rolling deploy looks like flapping and a healthy
	// alert is labelled noisy in the UI. Above 100 the threshold is unreachable
	// for any real rule and the damper is dead code that looks configured.
	KeyFlapThreshold: {Min: 3, Max: 100,
		Why: "transitions, 3..100: below 3 a single deploy is mislabelled as flapping; above 100 the damper can never engage"},
	// ⚠️ 300 IS KEPT DELIBERATELY, THOUGH IT IS INERT AT THE COMMONEST
	// `group_interval`. One observable fire→resolve→fire cycle costs
	// `group_interval + max(group_interval, for)` and yields two counted
	// transitions, so at the ecosystem's 5m `group_interval` a 300s window can hold
	// none of them. Raising the floor to match would be the wrong fix: the compose
	// capture in `sources/client/alertmanager/testdata` runs `group_interval: 30s`,
	// where a 300s window holds five whole cycles and is exactly right. A bound
	// that excluded it would exclude a value a real cluster needs. The arithmetic
	// belongs in the settings screen, which knows that cluster's own numbers; this
	// table only knows what is universally impossible.
	KeyFlapWindow: {Min: 300, Max: 86400,
		Why: "seconds, 300..86400: a window shorter than one group_interval cannot contain two transitions oto is able to observe. Whether it is long ENOUGH depends on your group_interval and your rules' for:, which this bound cannot see — the settings screen does that arithmetic against your own Alertmanager"},
	KeyFlapDigestInterval: {Min: 60, Max: 86400,
		Why: "seconds, 60..86400: a digest more often than once a minute is not a digest"},

	// A storm threshold of 1 collapses every group on its second member: permanent
	// storm mode, which is silence wearing a damper's name.
	KeyStormThreshold: {Min: 2, Max: 10000,
		Why: "distinct alerts, 2..10000: a threshold of 1 puts every group in permanent storm mode and suppresses every per-alert reply forever"},
	KeyStormWindow: {Min: 10, Max: 3600,
		Why: "seconds, 10..3600: must exceed group_wait or a burst Alertmanager is still batching does not look like a burst; longer than an hour is not a burst at all"},
	KeyStormCooldown: {Min: 60, Max: 86400,
		Why: "seconds, 60..86400: below a minute storm mode flickers on and off across consecutive Alertmanager batches"},

	// ⛔ RETENTION IS THE ONLY SETTING PAIR HERE WHOSE WRONG VALUE IS
	// UNRECOVERABLE. Every other knob above changes when something fires; these two
	// decide when a partition is DROPPED, and a dropped partition is gone. Both
	// bounds are therefore documented by what is LOST at the boundary, not by what
	// is stored — an operator lowering a number needs to know what stops being
	// answerable. ADR 0024 is the full ledger.
	//
	// The 30-day shipped default is DERIVED: it is the `alert_event_keys`
	// idempotency horizon, past which a stored batch can no longer be replayed
	// without duplicating the timeline (§C.8, §D.4, SPEC acceptance criterion 36).
	KeyRawRetention: {Min: 1, Max: 365,
		Why: "days, 1..365: raw payloads age out by dropping whole daily partitions. The shipped 30 is the alert_event_keys idempotency horizon — a batch older than that cannot be replayed after a parser fix without appending the timeline twice. Below it you lose the ability to reproduce an ingestion bug from the bytes that caused it; nothing an alert page shows depends on this"},
	// 13 months is a CEILING, not a preference: ADR 0014 puts one org's pessimistic
	// ceiling at 10M events/month and names 50–100M rows as where Postgres-only
	// hurts, so 13 months is the longest default that stays inside it.
	KeyEventRetention: {Min: 1, Max: 120,
		Why: "months, 1..120: dropping a monthly partition of alert_events destroys the instant-by-instant timeline for that month — every human comment, every unack note, the ordered narrative and the actor on each transition. What survives is the projection: the alert, every episode with its ack and its outcome, the rule text, and who was told on which channel. 13 is the longest default that keeps one org inside ADR 0014's scale envelope; raise it to 120 if you must keep timelines for years, and expect ADR 0014's revisit triggers"},

	// ⚠️ THIS RANGE IS `policies_reminder_ck`, VERBATIM. The org-level value is the
	// default a policy inherits when it sets none, so a value this table accepted
	// and that CHECK rejected would be a 23514 at reminder time.
	KeyUnackedReminder: {Min: 60, Max: 86400,
		Why: "seconds, 60..86400: mirrors policies_reminder_ck exactly, because this value becomes a policy's own when the policy sets none"},
}

// Bounds returns the bound for an integer key.
func Bounds(k SettingKey) (Bound, bool) {
	b, ok := settingBounds[k]
	return b, ok
}

// IntKeys returns the integer-valued keys in a stable order, so a caller
// rendering the bounds table gets the same order twice.
func IntKeys() []SettingKey {
	out := make([]SettingKey, 0, len(settingBounds))
	for k := range settingBounds {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// DefaultChannelVerbosity is the verbosity a Channel falls back to when it names
// none and the org has not chosen otherwise. It mirrors `channels_verbosity_ck`'s
// default and `notification/domain.VerbosityStatusChanges`.
const DefaultChannelVerbosity = "status_changes"

// channelVerbosities is `channels_verbosity_ck`, duplicated here rather than
// imported: `identity` owns the tenant and must not depend on `notification`. If
// the two ever disagree the write is rejected here, which is the safe direction.
var channelVerbosities = map[string]bool{
	"all": true, "status_changes": true, "firing_and_resolved": true, "firing_only": true,
}

// SettingsPatch is what THIS ORG WROTE, and only that.
//
// Every field is a pointer, and the pointer is the whole design: nil means "this
// org never wrote this key", which is a different fact from "this org wrote the
// same number the default happens to be". Collapsing the two loses the origin,
// and the origin is half of what makes a settings screen trustworthy — it is the
// difference between a value that will follow oto's default when oto's default
// moves and one that will not.
//
// It is also the exact shape of the `orgs.settings` JSONB, which is why a partial
// write is expressible at all: PATCHing one key rewrites one key.
type SettingsPatch struct {
	RefireGraceS        *int
	ResolveGraceS       *int
	GroupCloseDelayS    *int
	FlapThreshold       *int
	FlapWindowS         *int
	FlapDigestIntervalS *int
	StormThreshold      *int
	StormWindowS        *int
	StormCooldownS      *int
	RawRetentionDays    *int
	EventRetentionMonth *int
	// UnackedReminderAfterS is the org DEFAULT a notification policy inherits when
	// its own `unacked_reminder_after_s` is NULL. nil here leaves today's behaviour
	// exactly as it was — a policy with no reminder delay has no reminder.
	//
	// ⛔ ONE STAGE, FOREVER (§G.9.1). This is a scalar because the concept is a
	// scalar. It must never become an array, a ladder, or a second delay.
	UnackedReminderAfterS *int
	// DefaultVerbosity is the org's fallback for a Channel that names no verbosity.
	DefaultVerbosity *string
	// BroadcastOnResolved is ADR 0020's one configurable broadcast, default off.
	BroadcastOnResolved *bool

	// UnackedReminderMention is who the reminder addresses. See mention.go for
	// why the default is `none` and why that is a research result rather than a
	// preference.
	//
	// ⛔ NOT A ROTA (§4.8). The three fields below are a fixed audience and a
	// severity gate. None of them may ever become time-aware.
	UnackedReminderMention *string
	// UnackedReminderMentionList is the explicit audience for mode `list`.
	UnackedReminderMentionList *[]string
	// UnackedReminderMentionMinSeverity is the severity floor for attaching one.
	UnackedReminderMentionMinSeverity *string
}

// intPtr returns the address of the field named by k, or nil for a key that is
// not integer-valued.
//
// One switch serves validation, merging, clearing and origin reporting, so the
// fourteen keys are enumerated ONCE. A per-key copy of each of those loops is how
// a table like this acquires a key that can be written but not validated.
func (p *SettingsPatch) intPtr(k SettingKey) **int {
	switch k {
	case KeyRefireGrace:
		return &p.RefireGraceS
	case KeyResolveGrace:
		return &p.ResolveGraceS
	case KeyGroupCloseDelay:
		return &p.GroupCloseDelayS
	case KeyFlapThreshold:
		return &p.FlapThreshold
	case KeyFlapWindow:
		return &p.FlapWindowS
	case KeyFlapDigestInterval:
		return &p.FlapDigestIntervalS
	case KeyStormThreshold:
		return &p.StormThreshold
	case KeyStormWindow:
		return &p.StormWindowS
	case KeyStormCooldown:
		return &p.StormCooldownS
	case KeyRawRetention:
		return &p.RawRetentionDays
	case KeyEventRetention:
		return &p.EventRetentionMonth
	case KeyUnackedReminder:
		return &p.UnackedReminderAfterS
	case KeyDefaultVerbosity, KeyBroadcastOnResolved,
		KeyUnackedReminderMention, KeyUnackedReminderMentionList,
		KeyUnackedReminderMentionMinSeverity:
		return nil
	default:
		return nil
	}
}

// Validate rejects every out-of-range write, key by key, with the field name and
// the reason the range exists.
//
// ⛔ THIS IS THE SERVER-SIDE BOUND, and it is the only one that counts. The UI
// bound is a courtesy; this is the boundary. A key the org did not write is not
// validated, because not writing a key is always legal.
func (p SettingsPatch) Validate() error {
	var v []errs.Violation

	for _, k := range IntKeys() {
		ptr := (&p).intPtr(k)
		if ptr == nil || *ptr == nil {
			continue
		}
		b := settingBounds[k]
		if !b.Contains(**ptr) {
			v = append(v, errs.Violation{
				Field: string(k), Code: "out_of_range", Message: b.Why,
			})
		}
	}

	if p.DefaultVerbosity != nil && !channelVerbosities[*p.DefaultVerbosity] {
		v = append(v, errs.Violation{
			Field: string(KeyDefaultVerbosity), Code: "invalid_enum",
			Message: "one of all, status_changes, firing_and_resolved, firing_only",
		})
	}

	if p.UnackedReminderMention != nil && !ValidMentionMode(*p.UnackedReminderMention) {
		v = append(v, errs.Violation{
			Field: string(KeyUnackedReminderMention), Code: "invalid_enum",
			Message: "one of none, here, channel, list. `here` and `channel` are believed NOT to notify from inside a thread reply, which is where oto puts the reminder; an explicit list is the only form Slack documents as notifying",
		})
	}
	if p.UnackedReminderMentionList != nil {
		list := *p.UnackedReminderMentionList
		if len(list) > MaxReminderMentions {
			v = append(v, errs.Violation{
				Field: string(KeyUnackedReminderMentionList), Code: "max_items",
				Message: "at most 10 individuals or usergroups: a reminder that notifies more than that is a page, and oto pages nobody",
			})
		}
		for _, m := range list {
			if !ValidMentionToken(m) {
				v = append(v, errs.Violation{
					Field: string(KeyUnackedReminderMentionList), Code: "pattern",
					Message: "each entry is a Slack user `<@U...>` or usergroup `<!subteam^S...>`; @here and @channel are modes, not list members",
				})
				break
			}
		}
	}
	if p.UnackedReminderMentionMinSeverity != nil &&
		!ValidMentionMinSeverity(*p.UnackedReminderMentionMinSeverity) {
		v = append(v, errs.Violation{
			Field: string(KeyUnackedReminderMentionMinSeverity), Code: "invalid_enum",
			Message: "one of critical, warning, info",
		})
	}

	if len(v) > 0 {
		return errs.Validation("invalid_org_settings",
			"one or more settings are outside the range oto will accept", v...)
	}
	return nil
}

// Merge applies a partial write onto the stored patch and returns the result.
//
// A nil field in `next` leaves the stored value alone; a set field replaces it.
// There is deliberately NO "write null to reset": a reset is `Clear`, which names
// the keys it is clearing, because a JSON body cannot distinguish "I omitted this"
// from "I meant null" without a tri-state on every field — and a settings API
// where an omitted key silently reverts to the default is an API that reverts
// nine settings every time somebody changes one.
func (p SettingsPatch) Merge(next SettingsPatch) SettingsPatch {
	out := p
	for _, k := range IntKeys() {
		src, dst := (&next).intPtr(k), (&out).intPtr(k)
		if src == nil || dst == nil || *src == nil {
			continue
		}
		v := **src
		*dst = &v
	}
	if next.DefaultVerbosity != nil {
		v := *next.DefaultVerbosity
		out.DefaultVerbosity = &v
	}
	if next.BroadcastOnResolved != nil {
		v := *next.BroadcastOnResolved
		out.BroadcastOnResolved = &v
	}
	if next.UnackedReminderMention != nil {
		v := *next.UnackedReminderMention
		out.UnackedReminderMention = &v
	}
	if next.UnackedReminderMentionList != nil {
		v := append([]string(nil), *next.UnackedReminderMentionList...)
		out.UnackedReminderMentionList = &v
	}
	if next.UnackedReminderMentionMinSeverity != nil {
		v := *next.UnackedReminderMentionMinSeverity
		out.UnackedReminderMentionMinSeverity = &v
	}
	return out
}

// Clear drops the named keys, returning each to oto's shipped default. It is how
// an operator undoes an override, and after it `Origin` reports `default` again.
func (p SettingsPatch) Clear(keys ...SettingKey) SettingsPatch {
	out := p
	for _, k := range keys {
		if dst := (&out).intPtr(k); dst != nil {
			*dst = nil
			continue
		}
		switch k {
		case KeyDefaultVerbosity:
			out.DefaultVerbosity = nil
		case KeyBroadcastOnResolved:
			out.BroadcastOnResolved = nil
		case KeyUnackedReminderMention:
			out.UnackedReminderMention = nil
		case KeyUnackedReminderMentionList:
			out.UnackedReminderMentionList = nil
		case KeyUnackedReminderMentionMinSeverity:
			out.UnackedReminderMentionMinSeverity = nil
		}
	}
	return out
}

// Origin reports whether the effective value of k comes from this org or from
// oto's shipped default.
func (p SettingsPatch) Origin(k SettingKey) Origin {
	if ptr := (&p).intPtr(k); ptr != nil {
		if *ptr != nil {
			return OriginOrg
		}
		return OriginDefault
	}
	switch k {
	case KeyDefaultVerbosity:
		if p.DefaultVerbosity != nil {
			return OriginOrg
		}
	case KeyBroadcastOnResolved:
		if p.BroadcastOnResolved != nil {
			return OriginOrg
		}
	case KeyUnackedReminderMention:
		if p.UnackedReminderMention != nil {
			return OriginOrg
		}
	case KeyUnackedReminderMentionList:
		if p.UnackedReminderMentionList != nil {
			return OriginOrg
		}
	case KeyUnackedReminderMentionMinSeverity:
		if p.UnackedReminderMentionMinSeverity != nil {
			return OriginOrg
		}
	}
	return OriginDefault
}

// only returns a patch carrying just this key's value, or an empty patch when
// the key is not set. It is how Org.Shadowed assembles "the overrides that are
// not in force" one key at a time, without a second enumeration of the key set.
func (p SettingsPatch) only(k SettingKey) SettingsPatch {
	var out SettingsPatch
	if src := (&p).intPtr(k); src != nil {
		if *src != nil {
			v := **src
			*(&out).intPtr(k) = &v
		}
		return out
	}
	switch k {
	case KeyDefaultVerbosity:
		out.DefaultVerbosity = p.DefaultVerbosity
	case KeyBroadcastOnResolved:
		out.BroadcastOnResolved = p.BroadcastOnResolved
	case KeyUnackedReminderMention:
		out.UnackedReminderMention = p.UnackedReminderMention
	case KeyUnackedReminderMentionList:
		out.UnackedReminderMentionList = p.UnackedReminderMentionList
	case KeyUnackedReminderMentionMinSeverity:
		out.UnackedReminderMentionMinSeverity = p.UnackedReminderMentionMinSeverity
	}
	return out
}

// Overridden returns the keys this org has written, in a stable order.
func (p SettingsPatch) Overridden() []SettingKey {
	out := make([]SettingKey, 0, len(settingBounds)+2)
	for _, k := range AllSettingKeys() {
		if p.Origin(k) == OriginOrg {
			out = append(out, k)
		}
	}
	return out
}

// AllSettingKeys is the closed key set in a stable order.
func AllSettingKeys() []SettingKey {
	out := IntKeys()
	return append(out, KeyDefaultVerbosity, KeyBroadcastOnResolved,
		KeyUnackedReminderMention, KeyUnackedReminderMentionList,
		KeyUnackedReminderMentionMinSeverity)
}

// Settings folds the org's overrides onto oto's defaults and CLAMPS the result.
//
// Clamping rather than rejecting is the read-path rule stated on Bound.Clamp: a
// value already in the database — written before a bound existed, or by a
// migration — must not be able to produce a pathological runtime value, and must
// also not be able to fail an alert. The write path is where an operator finds
// out their number was refused.
func (p SettingsPatch) Settings() Settings {
	d := DefaultSettings()
	s := d

	pick := func(k SettingKey, def int) int {
		ptr := (&p).intPtr(k)
		if ptr == nil || *ptr == nil {
			return def
		}
		return settingBounds[k].Clamp(**ptr)
	}

	s.RefireGrace = time.Duration(pick(KeyRefireGrace, int(d.RefireGrace/time.Second))) * time.Second
	s.ResolveGrace = time.Duration(pick(KeyResolveGrace, int(d.ResolveGrace/time.Second))) * time.Second
	s.GroupCloseDelay = time.Duration(pick(KeyGroupCloseDelay, int(d.GroupCloseDelay/time.Second))) * time.Second
	s.FlapThreshold = pick(KeyFlapThreshold, d.FlapThreshold)
	s.FlapWindow = time.Duration(pick(KeyFlapWindow, int(d.FlapWindow/time.Second))) * time.Second
	s.FlapDigestInterval = time.Duration(pick(KeyFlapDigestInterval, int(d.FlapDigestInterval/time.Second))) * time.Second
	s.StormThreshold = pick(KeyStormThreshold, d.StormThreshold)
	s.StormWindow = time.Duration(pick(KeyStormWindow, int(d.StormWindow/time.Second))) * time.Second
	s.StormCooldown = time.Duration(pick(KeyStormCooldown, int(d.StormCooldown/time.Second))) * time.Second
	s.RawRetention = time.Duration(pick(KeyRawRetention, int(d.RawRetention/(24*time.Hour)))) * 24 * time.Hour
	// §D.1 stores a month count and oto reads a month as 30 days, uniformly.
	s.EventRetention = time.Duration(pick(KeyEventRetention, int(d.EventRetention/(30*24*time.Hour)))) * 30 * 24 * time.Hour

	// The reminder default is the one knob whose absence is meaningful: zero means
	// "this org sets no default, so a policy with no delay of its own still has no
	// reminder". Preserving that is what keeps this addition from silently turning
	// reminders on for every install that upgrades.
	if p.UnackedReminderAfterS != nil {
		s.UnackedReminderAfter = time.Duration(
			settingBounds[KeyUnackedReminder].Clamp(*p.UnackedReminderAfterS)) * time.Second
	}

	s.DefaultVerbosity = DefaultChannelVerbosity
	if p.DefaultVerbosity != nil && channelVerbosities[*p.DefaultVerbosity] {
		s.DefaultVerbosity = *p.DefaultVerbosity
	}

	if p.BroadcastOnResolved != nil {
		s.BroadcastOnResolved = *p.BroadcastOnResolved
	}

	// The mention surface, CLAMPED on read like everything else: a stored value
	// outside the vocabulary falls back to the shipped default rather than failing
	// a reminder. `none` and `critical` are the safe fallbacks in both directions —
	// the quiet one and the narrow one.
	s.UnackedReminderMention = MentionNone
	if p.UnackedReminderMention != nil && ValidMentionMode(*p.UnackedReminderMention) {
		s.UnackedReminderMention = *p.UnackedReminderMention
	}
	s.UnackedReminderMentionList = nil
	if p.UnackedReminderMentionList != nil {
		list := *p.UnackedReminderMentionList
		if len(list) > MaxReminderMentions {
			list = list[:MaxReminderMentions]
		}
		for _, m := range list {
			if ValidMentionToken(m) {
				s.UnackedReminderMentionList = append(s.UnackedReminderMentionList, m)
			}
		}
	}
	s.UnackedReminderMentionMinSeverity = MentionSeverityCritical
	if p.UnackedReminderMentionMinSeverity != nil &&
		ValidMentionMinSeverity(*p.UnackedReminderMentionMinSeverity) {
		s.UnackedReminderMentionMinSeverity = *p.UnackedReminderMentionMinSeverity
	}

	return s
}

// EffectiveInt returns the effective value of an integer key alongside its
// origin. It is what the API renders, and it is deliberately ONE call: a shape
// that returned the value and made the caller ask separately for the origin is a
// shape where somebody renders the value and forgets the origin.
func (p SettingsPatch) EffectiveInt(k SettingKey) (int, Origin, bool) {
	s := p.Settings()
	var v int
	switch k {
	case KeyRefireGrace:
		v = int(s.RefireGrace / time.Second)
	case KeyResolveGrace:
		v = int(s.ResolveGrace / time.Second)
	case KeyGroupCloseDelay:
		v = int(s.GroupCloseDelay / time.Second)
	case KeyFlapThreshold:
		v = s.FlapThreshold
	case KeyFlapWindow:
		v = int(s.FlapWindow / time.Second)
	case KeyFlapDigestInterval:
		v = int(s.FlapDigestInterval / time.Second)
	case KeyStormThreshold:
		v = s.StormThreshold
	case KeyStormWindow:
		v = int(s.StormWindow / time.Second)
	case KeyStormCooldown:
		v = int(s.StormCooldown / time.Second)
	case KeyRawRetention:
		v = int(s.RawRetention / (24 * time.Hour))
	case KeyEventRetention:
		v = int(s.EventRetention / (30 * 24 * time.Hour))
	case KeyUnackedReminder:
		v = int(s.UnackedReminderAfter / time.Second)
	default:
		return 0, OriginDefault, false
	}
	return v, p.Origin(k), true
}
