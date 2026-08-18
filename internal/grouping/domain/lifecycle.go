package domain

import (
	"time"

	"github.com/thulasiram/oto/internal/platform/tuning"
)

// The group lifecycle clock — the one number a generation's own timing needs.
//
// ⭐⭐ THIS FILE WAS `damping.go` AND IT HELD SPEC §B.6's GROUP-LEVEL DAMPER. Storm
// collapse lived here: `StormPolicy`, `DefaultStormPolicy`, `StormAction`,
// `StormDecision`, `EvaluateStorm` and `ApplyStorm`, plus the three `storm_*`
// defaults. All of it is deleted, and what is left is `group_close_delay_s`, which
// was only ever a lodger in `StormPolicy` because it is the same KIND of number.
//
// ⛔⛔ WHY STORM WENT, AND WHY THE ANSWER IS NOT "TUNE IT LOWER". Storm collapse
// counted DISTINCT alerts joining one generation inside a window and, above the
// threshold, held the generation to ONE root card with a count and a link while
// dropping every per-alert thread reply. The comment at the top of this file used to
// insist that this was "a VISIBLE UI STATE, NEVER SILENT SUPPRESSION" — and at the
// group's own altitude that was true, because the collapse posted, recorded and
// displayed itself. It was not true at the altitude that matters: the thirty-nine
// replies that never landed left NO trace an operator could read, so a suppressed
// notification was indistinguishable from a signal that never fired, which is the
// one failure §B.6 says an alerting product cannot be forgiven for.
//
// ⭐⭐ THE DEEPER FAULT IS THAT THE DEFENCE HAD NO OBJECT. A storm is many DIFFERENT
// alerts arriving together. The thing that owns many different alerts is an INCIDENT
// — `correlation`, DEFERRED-POST-V1 — and because that object does not exist yet,
// storm detection had nowhere to put what it detected. So it put it in the
// NOTIFICATION LAYER, which is precisely how a detector became a damper. Removing it
// now and reintroducing it beside incidents is cheaper than carrying a half-feature
// that pretends the object exists. Flooding a channel with two hundred real firings
// is a TRUTHFUL report that something is badly wrong; oto does not decide otherwise
// on its own judgement.
//
// ⭐ AND NOTHING SURVIVES ANY MORE. `alert_groups.storm_mode`,
// `alert_groups.storm_since`, `groups_storm_ck` and `channels.storm_notice_at` are
// dropped by migration 00059; `Group` no longer hydrates or exposes a storm pair;
// `?storm=` is off the contract and the group DTO carries neither field; and the
// three `storm_*` org settings are gone from `identity/domain`, from the settings API
// and from `platform/tuning`. The deferral was real — deleting a settings key makes
// `identity/domain/declarative.go` REFUSE AT BOOT, and two of the three were
// documented Helm values — and it was spent by a fact rather than a workaround: no
// oto database and no Helm release exists outside a development laptop, so there is
// no operator to CrashLoop. `group.storm_started` and `group.storm_ended` are DELETED
// rather than retired: they were kept one cut longer because `alert_events` could still
// hold rows spelling them, and migration 00060 narrows `ev_type_ck` to refuse the
// spellings outright, with the authorised database reset behind it.
//
// The ALERT-level damper is a separate story with the same ending, and it has now
// reached it: `alerts.flap_score` and `alerts.is_flapping` are RETIRED IN PLACE. The
// digest left the notification layer with migration 00057 because the case retention
// window removes the noise at FORMATION instead of at delivery — and that is what
// blinded the score, which counted lifecycle events a damped flap never appends. The
// detector, its job and its two timeline events are gone; the columns keep their last
// value and stay readable (ADR 0041, Amendment 1).

// DefaultGroupCloseDelay is how long a generation with no live member stays open
// before it closes and freezes its thread.
//
// ⛔ IT IS A REFERENCE TO `platform/tuning`, NOT A LITERAL. It was a literal with a
// ⚠️ comment saying it MIRRORS `identity/domain` — and a comment is not a mechanism:
// ADR 0026 moved three of oto's defaults in one change and two mirrored copies were
// missed. `grouping/domain` may not import `identity/domain` (CONTEXT.md §5.4,
// enforced by depguard), so the number lives one layer below both, where every module
// may name it and nothing may name a module. The derivation is stated there.
//
// It is pinned EQUAL to `refire_grace`: closing this generation is what makes the
// next fire post a brand-new Slack root, so a close delay shorter than the re-fire
// grace hands a re-fire that oto classified as "the same problem coming back" a new
// card anyway. It was 5m against a 10m grace, and the mismatch defeated the grace.
// See ADR 0026.
const DefaultGroupCloseDelay = tuning.DefaultGroupCloseDelay

// LifecyclePolicy is the tuning of one org's generation lifecycle.
//
// ⭐ IT IS ONE FIELD, AND THE SHAPE IS KEPT ON PURPOSE. It was `StormPolicy` with
// four; a bare `time.Duration` parameter would read as an anonymous number at every
// call site, and the next generation-level clock — if there ever is one — belongs
// beside this one rather than in a second port.
type LifecyclePolicy struct {
	// CloseDelay is `group_close_delay_s`.
	CloseDelay time.Duration
}

// DefaultLifecyclePolicy is §D.1's default.
func DefaultLifecyclePolicy() LifecyclePolicy {
	return LifecyclePolicy{CloseDelay: DefaultGroupCloseDelay}
}

// Normalise fills a zero field from the default, so a partially-configured org can
// never produce a zero close delay and freeze every generation's thread on the tick
// after it loses its last live member.
func (p LifecyclePolicy) Normalise() LifecyclePolicy {
	if p.CloseDelay <= 0 {
		p.CloseDelay = DefaultLifecyclePolicy().CloseDelay
	}
	return p
}
