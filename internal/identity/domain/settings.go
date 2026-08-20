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
//     is a convenience for the common case; it is not a boundary. A
//     `resolve_grace` of zero would let one missed Prometheus scrape look like an
//     expiry, and the request that sets it will arrive from `curl` long before it
//     arrives from a form. Every bound below is checked on the WRITE path and
//     clamped on the READ path.
//
//  2. **An effective value is useless without its origin.** The worst version of
//     configurability is a screen showing `600` with no way to tell whether the
//     org chose 600 or is simply running the shipped default — because those two
//     answers behave identically today and diverge the moment the default moves.
//     `SettingsPatch` keeps the org's OWN writes, so `Origin` can answer.
//
// ⚠️ Every bound here is one of TWO copies of the same rule (CONTEXT.md R9):
// this table and the OpenAPI schema in `api/openapi/openapi.yaml`. There used to
// be a third — `policies_reminder_ck`, the DDL CHECK on
// `unacked_reminder_after_s` — and it went with the reminder (git-bug bd0fb1d,
// migration 00068). No surviving key here is also a column, so no key here has a
// DDL copy. Changing one of the two without the other is what turns a 422 into a
// 500.

// SettingKey is the closed set of org-tunable keys, spelled exactly as
// `orgs.settings` stores them.
type SettingKey string

// The tunable keys.
const (
	KeyResolveGrace       SettingKey = "resolve_grace_s"
	KeyFlapThreshold      SettingKey = "flap_threshold"
	KeyFlapWindow         SettingKey = "flap_window_s"
	KeyFlapDigestInterval SettingKey = "flap_digest_interval_s"
	KeyRawRetention       SettingKey = "raw_retention_days"
	KeyEventRetention     SettingKey = "event_retention_months"
	KeyDefaultVerbosity   SettingKey = "default_verbosity"

	// ⛔⛔ `refire_grace_s` AND `group_close_delay_s` WERE HERE AND BOTH ARE DELETED
	// (git-bug 7287b28, migration 00071). They were org-facing, bounds-validated,
	// patchable, origin-reporting settings that DECIDED NOTHING:
	//
	//   - `refire_grace_s` picked T8 over T7 until ADR 0040 retired T8. Every live
	//     reference had become settings plumbing — `routingCommand` passes
	//     `ResolveGrace`, and there were zero reads in `alerts/`, `notification/`,
	//     `ingestion/` or `app/`.
	//   - `group_close_delay_s` closed an `alert_groups` GENERATION. Generations
	//     are gone (git-bug 7570090) and so is its one consumer,
	//     `app/adapters.go`'s `LifecyclePolicy{CloseDelay}`.
	//
	// ⭐ THIS IS `tools/lintreach`'s DEFECT CLASS, IN THE ONE PLACE LINTREACH
	// CANNOT LOOK. It walks Go declarations for a field with no reader; a settings
	// key's only reader IS its own CRUD, so the round trip looks perfectly wired
	// while nothing downstream consults the number. The two keys therefore had to
	// be found by reading, and the tuning derivation had become an argument about
	// the correct value of a number nothing read.
	//
	// DELETED, NOT RETIRED, on the same standing ruling as the reminder's five
	// keys (bd0fb1d) and storm's three: oto is unreleased, `git tag` is empty, and
	// a knob that clamps, validates and reports an origin while changing no
	// outcome is a vocabulary entry the next person has to rule out. No ghosts at
	// launch. `NewDeclarative` now refuses a values file naming either with
	// `unknown_key`, which is the loud failure rather than a regression.
	//
	// ⛔ `broadcast_on_resolved` WAS HERE AND IS DELETED (git-bug 7570090). It was
	// ADR 0020's ONE configurable broadcast, and it governed nothing once Slack
	// thread-broadcast was removed outright: with no `reply_broadcast` call left in
	// the notifier there is no broadcast for an org to opt into. A boolean that
	// cannot change an outcome is not a safe default, it is a lie in the settings
	// screen. `default_verbosity` is now the ONLY non-integer setting key.
	//
	// ⛔ FIVE KEYS WERE HERE AND ARE DELETED (git-bug bd0fb1d, migration 00068):
	// `unacked_reminder_after_s` and the three `unacked_reminder_mention*` keys.
	// The owner withdrew the unacked reminder — oto sends nothing unprompted — and
	// ruled the mention goes with it, because a mention was never a property of
	// Slack delivery in general: it was the AUDIENCE HALF of that one fact and had
	// no other producer.
	//
	// ⭐ THE MENTION KEYS EXISTED TO HOLD A LINE, AND DELETING THEM HOLDS IT
	// HARDER. SCOPE-BOUNDARY §5.2 restricted the audience to usergroups and
	// `!here`/`!channel` and dropped individual `<@U…>` mentions, because naming an
	// individual names a RESPONDER and oto must never know who is on call (H-1,
	// FR-1). With no mention surface there is nowhere left to cross that line.
	//
	// `orgs.settings` is one JSONB document, so 00068 deletes the keys from every
	// row rather than dropping a column, and `NewDeclarative` refuses a config
	// still naming one with `unknown_key`.
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

// ⛔⛔ `MinRefireGraceSeconds` WAS HERE AND IS DELETED WITH THE KEY IT FLOORED
// (git-bug 7287b28). It was `2 × ingestion/domain.DedupTTL`, and the pair was
// tied by `TestTheReplayWindowIsStrictlyInsideRefireGrace` rather than by a
// compile error, because a settings vocabulary must not import the ingest path.
//
// ⚠️ THE DERIVATION RAN THIS WAY ROUND, AND THAT IS WHY DELETING IT COSTS THE
// INGEST BOUND NOTHING. `DedupTTL` was NEVER computed from this constant: it is a
// TRANSPORT window, justified by Alertmanager's own three-peer gossip settling
// time and its retry backoff ceiling, and this floor was derived FROM it so the
// grace could not be configured underneath it. The dependent half is the half
// being deleted. `DedupTTL`'s own floor is asserted on its own terms by
// `ingestion/domain.TestTheReplayWindowStillCoversHAAndRetries`, which is the
// surviving half of the file that used to hold both.

// settingBounds is the table. Every integer key has an entry; a key with no entry
// is a key nothing can validate, so `Validate` treats a miss as a bug rather than
// as permission.
var settingBounds = map[SettingKey]Bound{
	// ⛔⛔ `refire_grace_s` AND `group_close_delay_s` HAD THE TWO ENTRIES ABOVE THIS
	// ONE AND BOTH ARE DELETED (git-bug 7287b28). Between them they carried the
	// longest bound rationale in the table — a floor derived from the §C.5 replay
	// window, and a cross-key rule keeping the close delay at or above the grace —
	// and every clause of it argued about the correct value of a number nothing
	// read. A bound is only worth stating for a key that changes an outcome.
	KeyResolveGrace: {Min: 60, Max: 86400,
		Why: "seconds, 60..86400: must exceed the EndsAt lease Prometheus refreshes (typically 3-4 minutes) or one missed scrape looks like an expiry"},

	// ⛔ THE THREE FLAP KEYS ARE PERMANENTLY INERT, AND THE DECISION IS RECORDED
	// ONCE — ADR 0042 Amendment 3, restated in SPEC §B.3. They STAY under their
	// own names with these bounds, on the standing rule that deleting a settings
	// key is a contract change of its own.
	//
	// Do not re-argue it here. The state that amendment ends is four files each
	// observing that the keys decide nothing and none of them being the decision;
	// a fifth observation is the thing it was written to stop.
	//
	// ⚠️ AND THE COMPARISON THAT USED TO SIT IN THIS PARAGRAPH IS GONE WITH ITS
	// SUBJECT. It read: what differs from `refire_grace` is *"that one still PINS
	// two numbers and these pin nothing"*. `refire_grace` pinned
	// `group_close_delay` and the ingest replay floor, and git-bug 7287b28 deleted
	// all three, so the distinction no longer separates anything. These keys are
	// now the ONLY inert ones left, and they are inert by a recorded ruling rather
	// than by attrition — which is the whole difference.
	//
	// The bounds below are still ENFORCED, so they still have to be right: a write
	// outside them is refused whatever the mechanism does. Their arithmetic is the
	// retired detector's, kept because it is what put each bound where it is.
	KeyFlapThreshold: {Min: 3, Max: 100,
		Why: "transitions, 3..100. ⚠️ Retired: no value changes what is delivered (ADR 0042 Amendment 3). The bound is the detector's own arithmetic — below 3 a single deploy would have been mislabelled as flapping, above 100 the threshold was unreachable for any real rule"},
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
		Why: "seconds, 300..86400. ⚠️ Retired: no value changes what is delivered (ADR 0042 Amendment 3). The bound is the detector's own arithmetic — a window shorter than one group_interval could not contain two transitions oto was able to observe"},
	KeyFlapDigestInterval: {Min: 60, Max: 86400,
		Why: "seconds, 60..86400. ⚠️ Retired: no value changes what is delivered (ADR 0042 Amendment 3). There is no flap digest to pace — a digest more often than once a minute would not have been a digest"},

	// ⛔⛔ `storm_threshold`, `storm_window_s` AND `storm_cooldown_s` WERE HERE AND
	// ARE DELETED. They were kept, INERT and bounded, for exactly one reason: this
	// table is what `declarative.go` validates against, `NewDeclarative` REFUSES AT
	// BOOT on a key it does not know (SPEC §H.13), and two of the three were
	// documented Helm values — so an operator who had tuned storm would have
	// CrashLooped on the next `helm upgrade`. That was the whole of the deferral,
	// and it is spent: no oto database and no Helm release exists outside a
	// development laptop, `git tag` is empty, and `release.yml` publishes only on a
	// `v*.*.*` tag with no `latest`. There is no values file in the world carrying
	// one of these keys.
	//
	// ⭐ AND THE BOOT REFUSAL IS THE RIGHT FAILURE, NOT A REGRESSION. A values file
	// that still says `tuning.storm_threshold: 40` now fails at
	// `NewDeclarative` with `unknown_key`, naming the config key that stated it and
	// listing every key that IS one. That is louder and more useful than the
	// alternative this table was protecting — a knob an operator can still set, that
	// still clamps, that still reports an origin, and that decides nothing.

	// ⛔ RETENTION IS THE ONLY SETTING PAIR HERE WHOSE WRONG VALUE IS
	// UNRECOVERABLE. Every other knob above changes when something fires; these two
	// decide when a partition is DROPPED, and a dropped partition is gone. Both
	// bounds are therefore documented by what is LOST at the boundary, not by what
	// is stored — an operator lowering a number needs to know what stops being
	// answerable. ADR 0024 is the full ledger.
	//
	// The 30-day shipped default is CHOSEN, not derived. It was documented as the
	// `alert_event_keys` idempotency horizon until `oto replay` moved its gate from
	// age to supersession (`ingestion/service.supersededBy` takes no age argument),
	// and nothing derives it now: it is the depth of the two raw feeds and the window
	// a replay can be attempted in at all (ADR 0024, Amendment 4).
	KeyRawRetention: {Min: 1, Max: 365,
		Why: "days, 1..365: raw payloads age out by dropping whole daily partitions. The shipped 30 is chosen, not derived — a replay is refused because the alerts a batch would touch have moved on since it arrived, never because the batch is old — so this window is the depth of the rejections and failed-batch feeds, which take no date range, and the window in which a stored batch can be replayed at all. Below it you lose the ability to reproduce an ingestion bug from the bytes that caused it; nothing an alert page shows depends on this"},
	// 13 months is a CEILING, not a preference: ADR 0014 puts one org's pessimistic
	// ceiling at 10M events/month and names 50–100M rows as where Postgres-only
	// hurts, so 13 months is the longest default that stays inside it.
	KeyEventRetention: {Min: 1, Max: 120,
		Why: "months, 1..120: dropping a monthly partition of alert_events destroys the instant-by-instant timeline for that month — every human comment, every unack note, the ordered narrative and the actor on each transition. What survives is the projection: the alert, every episode with its ack and its outcome, the rule text, and who was told on which channel. 13 is the longest default that keeps one org inside ADR 0014's scale envelope; raise it to 120 if you must keep timelines for years, and expect ADR 0014's revisit triggers"},
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
	ResolveGraceS       *int
	FlapThreshold       *int
	FlapWindowS         *int
	FlapDigestIntervalS *int
	RawRetentionDays    *int
	EventRetentionMonth *int
	// DefaultVerbosity is the org's fallback for a Channel that names no verbosity.
	DefaultVerbosity *string

	// ⛔⛔ `RefireGraceS` AND `GroupCloseDelayS` WERE HERE AND BOTH ARE DELETED
	// (git-bug 7287b28). See the key block above for why neither decided anything.
	//
	// ⛔ `BroadcastOnResolved` WAS HERE AND IS DELETED (git-bug 7570090), with the
	// broadcast it configured. See the key block above.
	//
	// ⛔ FOUR REMINDER FIELDS WERE HERE AND ARE DELETED (git-bug bd0fb1d):
	// `UnackedReminderAfterS` and the three `UnackedReminderMention*`. See the key
	// block above for why the mention went with the delay rather than after it.
}

// intPtr returns the address of the field named by k, or nil for a key that is
// not integer-valued.
//
// One switch serves validation, merging, clearing and origin reporting, so the
// seven keys are enumerated ONCE. A per-key copy of each of those loops is how
// a table like this acquires a key that can be written but not validated.
func (p *SettingsPatch) intPtr(k SettingKey) **int {
	switch k {
	case KeyResolveGrace:
		return &p.ResolveGraceS
	case KeyFlapThreshold:
		return &p.FlapThreshold
	case KeyFlapWindow:
		return &p.FlapWindowS
	case KeyFlapDigestInterval:
		return &p.FlapDigestIntervalS
	case KeyRawRetention:
		return &p.RawRetentionDays
	case KeyEventRetention:
		return &p.EventRetentionMonth
	case KeyDefaultVerbosity:
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
// seven settings every time somebody changes one.
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
	}
	return out
}

// Overridden returns the keys this org has written, in a stable order.
func (p SettingsPatch) Overridden() []SettingKey {
	out := make([]SettingKey, 0, len(settingBounds)+1)
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
	return append(out, KeyDefaultVerbosity)
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

	s.ResolveGrace = time.Duration(pick(KeyResolveGrace, int(d.ResolveGrace/time.Second))) * time.Second
	s.FlapThreshold = pick(KeyFlapThreshold, d.FlapThreshold)
	s.FlapWindow = time.Duration(pick(KeyFlapWindow, int(d.FlapWindow/time.Second))) * time.Second
	s.FlapDigestInterval = time.Duration(pick(KeyFlapDigestInterval, int(d.FlapDigestInterval/time.Second))) * time.Second
	s.RawRetention = time.Duration(pick(KeyRawRetention, int(d.RawRetention/(24*time.Hour)))) * 24 * time.Hour
	// §D.1 stores a month count and oto reads a month as 30 days, uniformly.
	s.EventRetention = time.Duration(pick(KeyEventRetention, int(d.EventRetention/(30*24*time.Hour)))) * 30 * 24 * time.Hour

	s.DefaultVerbosity = DefaultChannelVerbosity
	if p.DefaultVerbosity != nil && channelVerbosities[*p.DefaultVerbosity] {
		s.DefaultVerbosity = *p.DefaultVerbosity
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
	case KeyResolveGrace:
		v = int(s.ResolveGrace / time.Second)
	case KeyFlapThreshold:
		v = s.FlapThreshold
	case KeyFlapWindow:
		v = int(s.FlapWindow / time.Second)
	case KeyFlapDigestInterval:
		v = int(s.FlapDigestInterval / time.Second)
	case KeyRawRetention:
		v = int(s.RawRetention / (24 * time.Hour))
	case KeyEventRetention:
		v = int(s.EventRetention / (30 * 24 * time.Hour))
	default:
		return 0, OriginDefault, false
	}
	return v, p.Origin(k), true
}
