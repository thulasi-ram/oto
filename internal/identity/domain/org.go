package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/validate"
)

// Org is the tenant boundary. Every row in oto belongs to exactly one, directly
// or transitively (SPEC §D.1).
type Org struct {
	ID   uuid.UUID
	Slug string
	Name string
	// Settings are the EFFECTIVE values: this org's overrides folded onto oto's
	// shipped defaults and clamped to their bounds. Everything on the hot path
	// reads these and never reasons about where a number came from.
	Settings Settings
	// Overrides are what this org actually WROTE, and nothing else. It is what
	// lets the API say "600, and you chose it" rather than just "600" — see
	// SettingsPatch. A caller that renders Settings without consulting this is
	// showing a number nobody can act on.
	//
	// ⚠️ AN OVERRIDE HERE MAY NOT BE THE VALUE IN FORCE. Declarative wins over it,
	// and the override is deliberately kept anyway — see Declarative and
	// Shadowed.
	Overrides SettingsPatch
	// Declarative is what the DEPLOYMENT states, and it beats both the override
	// and the shipped default. It is not per-tenant and is not stored in Postgres:
	// it comes from this process's own configuration and is identical for every
	// org this process serves.
	Declarative Declarative
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// WithDeclarative overlays the deployment's declarative tuning and RECOMPUTES the
// effective settings.
//
// ⭐ IT IS THE ONLY WAY THE TWO CAN BE ATTACHED, and the recompute is why. An Org
// whose Declarative said one thing while its Settings still held the pre-overlay
// number would be an Org that reports an origin for a value it is not using —
// precisely the divergence this whole surface exists to make impossible.
func (o Org) WithDeclarative(d Declarative) Org {
	o.Declarative = d
	// Declarative merges OVER the org's own, because Merge takes every non-nil
	// field from its argument. That single call is the precedence rule.
	o.Settings = o.Overrides.Merge(d.Patch()).Settings()
	return o
}

// Origin reports where this org's effective value for k comes from: `config` when
// the deployment forces it, `org` when this tenant wrote it, `default` otherwise.
func (o Org) Origin(k SettingKey) Origin {
	if o.Declarative.Manages(k) {
		return OriginConfig
	}
	return o.Overrides.Origin(k)
}

// ConfigKey returns the config key forcing k, or "" when nothing is.
func (o Org) ConfigKey(k SettingKey) string { return o.Declarative.ConfigKey(k) }

// Shadowed is the org's own overrides that are NOT in force, because
// configuration is forcing something else.
//
// ⭐ IT IS RETURNED SO THE OPERATOR CAN SEE BOTH NUMBERS. "You have an override of
// 900s, but configuration is forcing 600s" is an answer somebody can act on;
// showing only the 600 leaves the 900 sitting in the database, invisible, waiting
// to take effect the moment the config key is removed.
func (o Org) Shadowed() SettingsPatch {
	var out SettingsPatch
	for _, k := range o.Declarative.Keys() {
		if o.Overrides.Origin(k) != OriginOrg {
			continue
		}
		out = out.Merge(o.Overrides.only(k))
	}
	return out
}

// Settings is one org's tuning of the lifecycle machine, the dampers and
// retention (`orgs.settings`, SPEC §D.1).
//
// Every field is a duration or a count the SPEC names. NOTHING here changes what
// a transition MEANS — only when it fires. A setting that could change the
// meaning of `resolved` would let an operator configure oto into lying.
//
// The values are stored as seconds in JSONB and are typed as durations here, so
// that no caller has to remember which unit a particular key was written in.
type Settings struct {
	// RefireGrace decides T8 from T7: a re-fire inside the window reopens the
	// existing occurrence, one after it opens a new episode (§B.5).
	RefireGrace time.Duration
	// ResolveGrace is how long past `source_ends_at` the reaper waits before an
	// occurrence may expire (§B.4).
	ResolveGrace time.Duration
	// GroupCloseDelay is how long an idle generation is held open before closing,
	// which freezes its Slack thread.
	GroupCloseDelay time.Duration

	// FlapThreshold is the transition count above which an Alert is MARKED
	// flapping. Marking is the whole point: flapping is a VISIBLE state, never
	// silent suppression (§B.6).
	FlapThreshold int
	// FlapWindow is the window FlapThreshold is counted over.
	FlapWindow time.Duration
	// FlapDigestInterval is how often a flapping alert's card is refreshed.
	FlapDigestInterval time.Duration

	// StormThreshold is the distinct-join count that collapses a group's card.
	StormThreshold int
	// StormWindow is the window StormThreshold is counted over.
	StormWindow time.Duration
	// StormCooldown is how long a group stays collapsed after it stops storming.
	StormCooldown time.Duration

	// RawRetention is how long raw ingested payloads are kept. They age out by
	// dropping whole partitions, never by deleting rows.
	RawRetention time.Duration
	// EventRetention is how long `alert_events` are kept.
	EventRetention time.Duration

	// UnackedReminderAfter is the org DEFAULT a notification policy inherits when
	// its own `unacked_reminder_after_s` is NULL. Zero means the org sets no
	// default, which is what shipped: a policy with no delay of its own has no
	// reminder.
	//
	// ⛔ ONE STAGE, FOREVER (§G.9.1). A scalar, never an array, never a ladder,
	// and never a target other than the policy's own channel_ids.
	UnackedReminderAfter time.Duration

	// DefaultVerbosity is the fallback for a Channel that names no verbosity. It
	// is a `channels_verbosity_ck` value, held as a string because `identity` owns
	// the tenant and must not depend on `notification`.
	DefaultVerbosity string

	// BroadcastOnResolved is ADR 0020's ONE configurable broadcast. Default off:
	// closure is welcome, and on a busy channel it doubles traffic for the least
	// urgent fact oto has — nobody was ever woken because a resolve arrived
	// quietly. Every other broadcasting transition is fixed by policy, because a
	// broadcast cannot be un-sent and the set is a product decision, not a dial.
	BroadcastOnResolved bool

	// UnackedReminderMention is WHO the one unacked reminder addresses:
	// none | here | channel | list (mention.go). DEFAULT `none`.
	//
	// ⛔ NOT A ROTA, EVER (§4.8, ADR 0013). A fixed audience chosen once, in
	// configuration. No time of day, no weekday, no schedule, no second stage.
	UnackedReminderMention string
	// UnackedReminderMentionList is the explicit audience for mode `list`: Slack
	// user and usergroup ids, at most MaxReminderMentions of them.
	UnackedReminderMentionList []string
	// UnackedReminderMentionMinSeverity is the severity class at or above which a
	// mention is attached at all. DEFAULT `critical` — `@here` on every unacked
	// warning is how a channel gets muted, and a muted channel hides the real
	// incident.
	UnackedReminderMentionMinSeverity string
}

// The defaults of SPEC §D.1, restated as the values a brand-new org boots with.
// They are the numbers that decide how noisy Slack is; changing one is a product
// decision, not a tuning tweak.
//
// ⭐ THE FOUR TIMING DEFAULTS BELOW ARE DERIVED FROM A MEASURED CORPUS, NOT
// CHOSEN (ADR 0026). The two numbers everything hangs off are:
//
//   - `group_interval: 5m` — Alertmanager's own `dispatch.DefaultRouteOpts`, and
//     the value shipped unchanged by kube-prometheus-stack, kube-prometheus,
//     OpenShift's cluster-monitoring-operator and Grafana Alerting. It is the one
//     Alertmanager number the ecosystem does NOT override.
//   - `for: 15m` — the MODE and the MEDIAN of the 155 alerting rules
//     kube-prometheus-stack 88.2.0 ships (69 of 155, 44.5%). `15m`, `10m` and
//     `5m` together are 75.5% of every rule in that corpus.
//
// Every derivation below is stated in docs/setup/tuning.md against those numbers
// and their sources, and `defaults_derivation_test.go` recomputes it so a
// default cannot drift away from the arithmetic that produced it.
const (
	// DefaultRefireGrace is `for + group_interval` for the MODAL real rule:
	// 15m + 5m = 20m. It is the smallest value at which oto's re-fire reopen path
	// (T8) is reachable for the commonest rule shape in the wild.
	//
	// ⛔ IT WAS 600s AND 600s IS UNREACHABLE FOR 76% OF REAL RULES. The clock
	// starts at the occurrence's `ended_at`, which T5 takes from the UPSTREAM
	// `EndsAt` — the moment Prometheus stopped considering the rule firing, not
	// the moment oto heard about it. For the same alert to fire again its
	// condition must hold for `for:` all over again, and Alertmanager then batches
	// the notification. So the earliest re-fire oto can OBSERVE lands at
	// `ended_at + for`, and typically at `ended_at + for + group_interval`. With
	// `for: 15m` that is 15–20 minutes, and a 10-minute grace has always expired:
	// every re-fire opened a new episode, a new generation and a new Slack root
	// card, with a setting on the screen that looked like it should have stopped
	// it. 600s covered rules up to `for: 5m` — 24% of the corpus. 1200s covers
	// every rule up to `for: 15m` — 86.5%.
	DefaultRefireGrace  = 1200 * time.Second
	DefaultResolveGrace = 300 * time.Second
	// DefaultGroupCloseDelay EQUALS DefaultRefireGrace, and the equality is the
	// whole point rather than a coincidence.
	//
	// ⛔ IT WAS 300s WHILE `refire_grace` WAS 600s, WHICH DEFEATED `refire_grace`.
	// Reopening an occurrence only avoids a new Slack root message if the group
	// GENERATION is still open — a closed generation freezes its thread and the
	// next observation opens generation N+1 with a brand-new root (§B.5). With the
	// old pair the generation closed 5 minutes after the resolve and the grace ran
	// for 10, so the whole second half of the grace bought an occurrence reopen
	// that still posted a new card. oto's own tuning guidance already said "keep
	// group_close_delay at or above refire_grace"; the shipped defaults broke it.
	//
	// Equal is safe rather than racy because the two clocks start at different
	// moments: this one runs from the group's last ACTIVITY (the resolve as oto
	// observed it) while `refire_grace` runs from the upstream `ended_at`, which is
	// the same instant or earlier. So the group always closes at or after the grace
	// expires, never before.
	DefaultGroupCloseDelay = 1200 * time.Second
	// DefaultFlapThreshold SURVIVED the derivation unchanged: at the window below
	// it sits at 42% of the observable ceiling, which is the "roughly half the
	// ceiling" rule docs/setup/tuning.md states, and it stays above the floor of 3
	// that keeps one ordinary rolling deploy from being labelled as flapping.
	DefaultFlapThreshold = 5
	// DefaultFlapWindow is `flap_threshold × cycle` for the modal rule, rounded up
	// to a round number: 5 × (15m + 5m) = 100m → 2h.
	//
	// ⛔ IT WAS 1800s, AND 5-IN-30m WAS UNREACHABLE FOR EVERY RULE SHAPE IN THE
	// CORPUS — the damper was dead code that looked configured. Alertmanager will
	// not report a changed group sooner than `group_interval` after the last
	// notification, so one observable fire→resolve→fire cycle costs
	// `group_interval + max(group_interval, for)` and yields exactly TWO counted
	// transitions. At `group_interval: 5m` that is a 10-minute cycle even for a
	// rule with no `for:` at all, so a 30-minute window holds at most 6 observable
	// transitions — and 20 minutes, hence 2 transitions, for the modal `for: 15m`
	// rule. A threshold of 5 could never be crossed.
	//
	// Rounding UP is the safe direction: a wider window makes the threshold more
	// reachable, and a window that is too wide fails visibly (a stale "flapping"
	// badge) and self-heals within one 5-minute `flap.score` tick, whereas a window
	// that is too narrow fails invisibly, as silence where a damper should be.
	DefaultFlapWindow = 7200 * time.Second
	// DefaultFlapDigestInterval SURVIVED unchanged: the rule is "at or above
	// group_interval, usefully 2×–4×", and 15m is 3 × the 5m the ecosystem runs.
	DefaultFlapDigestInterval = 900 * time.Second
	DefaultStormThreshold     = 25
	DefaultStormWindow        = 60 * time.Second
	DefaultStormCooldown      = 600 * time.Second
	// DefaultRawRetention is 30 DAYS and it is DERIVED, not chosen: it is the
	// `alert_event_keys` idempotency horizon (SPEC §D.4, `created_at < now() - 30
	// days`).
	//
	// The one stated requirement a stored raw payload exists to serve is SPEC
	// acceptance criterion 36 — "replaying a stored `ingest_batch` after a parser
	// fix reproduces the same state without duplicate Slack messages". That replay
	// is idempotent only while the batch's event dedupe keys are still in
	// `alert_event_keys`. Past that horizon a replay APPENDS the timeline a second
	// time, so a payload kept longer is a payload that can no longer be used for the
	// thing it is kept for. Keeping raw bytes beyond the window in which they are
	// safely replayable buys storage and no capability.
	//
	// ⛔ THE TWO NUMBERS ARE COUPLED. If the `alert_event_keys` pruner ever moves
	// off 30 days, this moves with it — the same relationship `MinRefireGraceSeconds`
	// has with `ingestion/domain.DedupTTL`. See ADR 0024.
	//
	// It was 14 days, which was traceable to nothing. See ADR 0024 for the measured
	// storage this costs: at 10 000 alert firings a day the extra sixteen days is
	// about 270 MB.
	DefaultRawRetention = 30 * 24 * time.Hour
	// DefaultEventRetention is 13 MONTHS, and the reason is a CEILING rather than a
	// preference: it is the longest window that keeps a single org inside ADR 0014's
	// own scale envelope.
	//
	// ADR 0014 puts the pessimistic ceiling at ~10M events per month for one org and
	// names 50–100M rows as where Postgres-only starts to hurt. 13 × 10M = 130M rows
	// — measured at ~752 bytes per row all-in, about 98 GB — which is the top of that
	// band. A longer default would ship every install past the point ADR 0014 says to
	// revisit the datastore.
	//
	// The thirteenth month is what makes a year-on-year comparison land, but that
	// comparison is served by `alert_quality_daily`, which is NEVER reaped. What this
	// window actually bounds is the instant-by-instant TIMELINE — see ADR 0024 for
	// exactly what is lost when a partition is dropped, and note that human comments
	// live nowhere else.
	DefaultEventRetention = 13 * 30 * 24 * time.Hour
	// DefaultUnackedReminderAfter is ZERO, and the zero is the decision: oto ships
	// no org-level reminder default, so a notification policy that names no delay
	// still produces no reminder. Anything else would turn reminders on for every
	// install that merely upgrades.
	DefaultUnackedReminderAfter = 0 * time.Second
	// DefaultBroadcastOnResolved is off (ADR 0020).
	DefaultBroadcastOnResolved = false
	// DefaultUnackedReminderMention is `none`, and the default is a RESEARCH
	// RESULT: Slack documents that @here and @channel do not notify when used in
	// threads, and oto's reminder is a thread reply. Shipping `here` by default
	// would ship a setting that silently does nothing. See mention.go.
	DefaultUnackedReminderMention = MentionNone
	// DefaultUnackedReminderMentionMinSeverity is `critical` only.
	DefaultUnackedReminderMentionMinSeverity = MentionSeverityCritical
)

// DefaultSettings is the tuning an org has until somebody changes it.
//
// oto DEFAULTS TO QUIET: grouping, flap damping and storm collapse are all on,
// and every damping decision is a visible UI state rather than a silent drop
// (CONTEXT.md §6).
func DefaultSettings() Settings {
	return Settings{
		RefireGrace:        DefaultRefireGrace,
		ResolveGrace:       DefaultResolveGrace,
		GroupCloseDelay:    DefaultGroupCloseDelay,
		FlapThreshold:      DefaultFlapThreshold,
		FlapWindow:         DefaultFlapWindow,
		FlapDigestInterval: DefaultFlapDigestInterval,
		StormThreshold:     DefaultStormThreshold,
		StormWindow:        DefaultStormWindow,
		StormCooldown:      DefaultStormCooldown,
		RawRetention:       DefaultRawRetention,
		EventRetention:     DefaultEventRetention,

		UnackedReminderAfter: DefaultUnackedReminderAfter,
		DefaultVerbosity:     DefaultChannelVerbosity,
		BroadcastOnResolved:  DefaultBroadcastOnResolved,

		UnackedReminderMention:            DefaultUnackedReminderMention,
		UnackedReminderMentionMinSeverity: DefaultUnackedReminderMentionMinSeverity,
	}
}

// Normalise replaces any non-positive value with its default.
//
// A zero in `orgs.settings` means "this key was never written", not "disable the
// damper". Reading it as zero would turn an unconfigured org into one with a flap
// window of nothing, which silently disables §B.6 for exactly the tenants that
// never touched the settings screen.
func (s Settings) Normalise() Settings {
	d := DefaultSettings()
	if s.RefireGrace <= 0 {
		s.RefireGrace = d.RefireGrace
	}
	if s.ResolveGrace <= 0 {
		s.ResolveGrace = d.ResolveGrace
	}
	if s.GroupCloseDelay <= 0 {
		s.GroupCloseDelay = d.GroupCloseDelay
	}
	if s.FlapThreshold <= 0 {
		s.FlapThreshold = d.FlapThreshold
	}
	if s.FlapWindow <= 0 {
		s.FlapWindow = d.FlapWindow
	}
	if s.FlapDigestInterval <= 0 {
		s.FlapDigestInterval = d.FlapDigestInterval
	}
	if s.StormThreshold <= 0 {
		s.StormThreshold = d.StormThreshold
	}
	if s.StormWindow <= 0 {
		s.StormWindow = d.StormWindow
	}
	if s.StormCooldown <= 0 {
		s.StormCooldown = d.StormCooldown
	}
	if s.RawRetention <= 0 {
		s.RawRetention = d.RawRetention
	}
	if s.EventRetention <= 0 {
		s.EventRetention = d.EventRetention
	}
	if !channelVerbosities[s.DefaultVerbosity] {
		s.DefaultVerbosity = d.DefaultVerbosity
	}
	if !ValidMentionMode(s.UnackedReminderMention) {
		s.UnackedReminderMention = d.UnackedReminderMention
	}
	if !ValidMentionMinSeverity(s.UnackedReminderMentionMinSeverity) {
		s.UnackedReminderMentionMinSeverity = d.UnackedReminderMentionMinSeverity
	}
	// UnackedReminderAfter is deliberately NOT defaulted here: zero is a meaningful
	// value for it — "this org sets no reminder default" — and rewriting it to a
	// default would give every policy a reminder nobody asked for. Its BOUND is
	// still enforced, on the write path and on SettingsPatch.Settings.
	return s
}

// MaxOrgNameBytes mirrors orgs_name_ck.
const MaxOrgNameBytes = 200

// NewOrg builds an Org, enforcing the invariants `orgs_slug_ck` and
// `orgs_name_ck` also enforce. If you can construct it, it is valid: there is no
// optional Validate() in this package.
func NewOrg(id uuid.UUID, slug, name string, settings Settings) (Org, error) {
	slug = strings.TrimSpace(strings.ToLower(slug))
	name = strings.TrimSpace(name)

	if !validate.OrgSlugRe.MatchString(slug) {
		return Org{}, errs.Validation("invalid_org_slug", "slug must match "+validate.PatternOrgSlug)
	}
	if l := len(name); l < 1 || l > MaxOrgNameBytes {
		return Org{}, errs.Validation("invalid_org_name", "name must be 1..200 characters")
	}
	return Org{ID: id, Slug: slug, Name: name, Settings: settings.Normalise()}, nil
}

// Live reports whether the org has not been soft deleted.
func (o Org) Live() bool { return o.DeletedAt == nil }
