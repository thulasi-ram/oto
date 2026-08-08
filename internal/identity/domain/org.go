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
	ID        uuid.UUID
	Slug      string
	Name      string
	Settings  Settings
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
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
}

// The defaults of SPEC §D.1, restated as the values a brand-new org boots with.
// They are the numbers that decide how noisy Slack is; changing one is a product
// decision, not a tuning tweak.
const (
	DefaultRefireGrace        = 600 * time.Second
	DefaultResolveGrace       = 300 * time.Second
	DefaultGroupCloseDelay    = 300 * time.Second
	DefaultFlapThreshold      = 5
	DefaultFlapWindow         = 1800 * time.Second
	DefaultFlapDigestInterval = 900 * time.Second
	DefaultStormThreshold     = 25
	DefaultStormWindow        = 60 * time.Second
	DefaultStormCooldown      = 600 * time.Second
	DefaultRawRetention       = 14 * 24 * time.Hour
	DefaultEventRetention     = 13 * 30 * 24 * time.Hour
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
