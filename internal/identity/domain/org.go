package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/tuning"
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
	// RefireGrace is the window a re-fire is treated as "the same problem coming
	// back" in.
	//
	// ⚠️ IT NO LONGER DECIDES A TRANSITION (ADR 0040). It used to pick T8 over T7 —
	// a re-fire inside the window reopened the closed episode and kept its
	// acknowledgement — and a Case is strictly terminal now, so every re-fire opens
	// the next `seq` unacked. The setting is retained because `GroupCloseDelay` is
	// pinned at or above it and `MinRefireGraceSeconds` derives the ingest replay
	// floor from it; whether it should be renamed or removed is undecided, and
	// removing a settings key is a contract change of its own.
	RefireGrace time.Duration
	// ResolveGrace is how long past `source_ends_at` the reaper waits before an
	// case may expire (§B.4).
	ResolveGrace time.Duration
	// GroupCloseDelay is how long an idle generation is held open before closing.
	// A closed generation is never rejoined: the next observation opens N+1, and a
	// new generation is a new thread.
	GroupCloseDelay time.Duration

	// FlapThreshold is the transition count above which an Alert is MARKED
	// flapping. Marking is the whole point: flapping is a VISIBLE state, never
	// silent suppression (§B.6).
	FlapThreshold int
	// FlapWindow is the window FlapThreshold is counted over.
	FlapWindow time.Duration
	// FlapDigestInterval is how often a flapping alert's card is refreshed.
	FlapDigestInterval time.Duration

	// ⛔⛔ `StormThreshold`, `StormWindow` AND `StormCooldown` WERE HERE AND ARE
	// DELETED WITH THEIR SettingKeys (see settings.go). They were inert — storm
	// damping is removed, so no module read them to decide anything and the only
	// thing that still touched them was the settings screen, which rendered three
	// numbers that changed nothing.

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
// ⛔⛔ THE SHARED ONES ARE NOT WRITTEN HERE ANY MORE, AND THAT IS THE POINT.
//
// `identity/domain` owns the tenant's tuning — the keys, the bounds, the
// provenance and this struct — but it is not the only package that needs the
// SHIPPED number. `alerts/domain`, `grouping/domain` and `alerts/service` each
// keep a fallback for the case where no SettingsReader is wired, and CONTEXT.md
// §5.4 forbids any of them importing this package: a domain reaches another
// domain only through `<other>/service`, and `.golangci.yml` enforces it. So they
// used to COPY the numbers, with a ⚠️ comment in each copy asking the next person
// to remember, and when ADR 0026 moved three of them at once two copies were
// missed and only a test noticed.
//
// The numbers therefore live in `platform/tuning`, the layer every module may
// import and that may import none, and every SHARED default below is a REFERENCE
// to that one home rather than a literal. Two packages can no longer disagree
// about a value neither of them writes. The names stay here because this is where
// the settings vocabulary lives and where every caller already looks for them.
//
// The defaults NOBODY ELSE READS keep their values here, where the types they use
// also live: `unacked_reminder_*`, the mention policy and the channel verbosity
// are identity's alone, and moving them down would drag domain types into
// platform and invert the direction the split exists to protect.
//
// ⭐ THE FOUR TIMING DEFAULTS ARE DERIVED FROM A MEASURED CORPUS, NOT CHOSEN
// (ADR 0026). The two numbers everything hangs off are:
//
//   - `group_interval: 5m` — Alertmanager's own `dispatch.DefaultRouteOpts`, and
//     the value shipped unchanged by kube-prometheus-stack, kube-prometheus,
//     OpenShift's cluster-monitoring-operator and Grafana Alerting. It is the one
//     Alertmanager number the ecosystem does NOT override.
//   - `for: 15m` — the MODE and the MEDIAN of the 155 alerting rules
//     kube-prometheus-stack 88.2.0 ships (69 of 155, 44.5%). `15m`, `10m` and
//     `5m` together are 75.5% of every rule in that corpus.
//
// Each derivation is stated where the constant now lives, restated in
// docs/setup/tuning.md, and RECOMPUTED by `defaults_derivation_test.go` so a
// default cannot drift away from the arithmetic that produced it.
const (
	// DefaultRefireGrace is the default width of that window. Since ADR 0040 no
	// transition consults it; see the field comment on Settings.RefireGrace.
	DefaultRefireGrace = tuning.DefaultRefireGrace
	// DefaultResolveGrace is how long past `source_ends_at` the reaper waits
	// before a case may expire (§B.4).
	DefaultResolveGrace = tuning.DefaultResolveGrace
	// DefaultGroupCloseDelay is pinned EQUAL to DefaultRefireGrace, and the
	// equality is the whole point rather than a coincidence: a generation that
	// closes first hands a re-fire oto classified as "the same problem coming
	// back" a brand-new Slack root anyway. See the derivation in platform/tuning.
	DefaultGroupCloseDelay = tuning.DefaultGroupCloseDelay
	// DefaultFlapThreshold is the transition count above which an Alert is marked
	// flapping — a VISIBLE state, never silent suppression (§B.6).
	DefaultFlapThreshold = tuning.DefaultFlapThreshold
	// DefaultFlapWindow is the window DefaultFlapThreshold is counted over. It is
	// 2h and not 30m because the transport floor capped a 30-minute window at six
	// observable transitions, which made a threshold of 5 unreachable for every
	// rule shape in the corpus.
	DefaultFlapWindow = tuning.DefaultFlapWindow
	// DefaultFlapDigestInterval is how often a flapping alert's digest is posted.
	DefaultFlapDigestInterval = tuning.DefaultFlapDigestInterval
	// ⛔ `DefaultStormThreshold`, `DefaultStormWindow` AND `DefaultStormCooldown`
	// WERE HERE. They mirrored `platform/tuning`, which no longer declares them: a
	// default for a setting that does not exist is a number nothing can apply.
	// DefaultRawRetention is 30 DAYS and it is CHOSEN, not derived: `oto replay`
	// gates on supersession rather than on age, so nothing derives this number
	// (ADR 0024, Amendment 4). It is the depth of the rejections and failed-batch
	// feeds, which take no time window, and the window a replay can be attempted in
	// at all; it costs 51 MB at 1 000 alert firings a day.
	DefaultRawRetention = tuning.DefaultRawRetention
	// DefaultEventRetention is 13 MONTHS, and the reason is a CEILING rather than
	// a preference: it is the longest window that keeps one org inside ADR 0014's
	// own scale envelope of 50–100M rows.
	DefaultEventRetention = tuning.DefaultEventRetention
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
// oto DEFAULTS TO QUIET: grouping is on, flap SCORING is on (its damper moved to case
// formation), and storm collapse is gone entirely —
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
