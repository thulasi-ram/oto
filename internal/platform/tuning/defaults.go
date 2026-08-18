package tuning

import "time"

// ⛔⛔ THIS FILE IS THE ONLY PLACE IN THE TREE WHERE THESE NUMBERS ARE WRITTEN.
//
// They used to be written in four packages at once — `identity/domain` as the
// stated source, `alerts/domain`, `grouping/domain` and `alerts/service` as
// "mirrors" — and each mirror carried a ⚠️ comment asking the next person to
// remember. ADR 0026 then moved three of them in one change (`refire_grace`
// 600→1200, `group_close_delay` 300→1200, `flap_window` 1800→7200) and two of the
// mirrored copies were missed; only a test caught it. That failure mode is not
// loud. A missed mirror does not crash and does not log: it is the fallback an
// org gets when its settings row fails to load, so exactly the tenant already
// having a bad day runs the OLD arithmetic while the rest of the system runs the
// new one, and nobody is told.
//
// So the mirrors are gone. Every package that needs a shipped default now names a
// constant declared HERE, and a divergence has to be typed on purpose rather than
// arrived at by forgetting.
//
// ⭐ WHY THIS LIVES UNDER `platform` AND NOT IN ANY DOMAIN.
//
// The obvious home is `identity/domain`: §D.1 tuning is a property of an Org, and
// identity owns the keys, the bounds and the provenance. It cannot BE the home,
// because CONTEXT.md §5.4 forbids one domain importing another's internals and
// `.golangci.yml` enforces it — `alerts`, `grouping` and `identity` deny each
// other's package trees outright, and the one sanctioned cross-domain `domain`
// import (RULE K, `internal/alerts/domain`) is the ALERT IDENTITY kernel: value
// objects, not tenant settings. Putting the tenant's tuning in the alert kernel
// would make identity — the module that validates and serves these numbers — the
// downstream mirror of the module that merely consumes them.
//
// `platform` is the layer every module may already import and that may import no
// module (rule 7, `platform-must-not-import-domains`). A leaf of derived numbers
// with no behaviour therefore couples the four packages in the ONE direction the
// architecture already permits, and the cycle the constraint exists to prevent is
// mechanically impossible: this package importing any domain is a lint failure.
// It also reaches the fifth copy — `platform/config`'s retention floor — which no
// home inside a domain ever could, since platform may not import a domain at all.
//
// ⛔ WHAT MAY AND MAY NOT LIVE HERE. Constants only, and only ones that ARE
// SHARED. This is not a bag for identity's whole settings vocabulary: the
// defaults only identity reads (`unacked_reminder_*`, the mention policy, the
// channel verbosity) stay in `identity/domain` with the types they belong to,
// because moving them here would drag domain types into platform and invert the
// direction this package exists to protect. Nothing here has behaviour, imports
// anything but `time`, or knows what an Org is.
//
// Every number below is DERIVED, and the derivation is executed rather than
// described: `identity/domain/defaults_derivation_test.go` recomputes each one
// from the measured rule corpus of ADR 0026 and fails if a constant is moved
// without the reasoning moving with it. The prose here says what the number is;
// that file is what keeps it true. See also docs/setup/tuning.md.

// The §D.1 lifecycle and damping defaults.
const (
	// DefaultRefireGrace is `for + group_interval` for the MODAL real rule:
	// 15m + 5m = 20m. It is the smallest value at which oto's re-fire reopen path
	// (T8) is reachable for the commonest rule shape in the wild.
	//
	// ⛔ IT WAS 600s AND 600s IS UNREACHABLE FOR 76% OF REAL RULES. The clock
	// starts at the case's `ended_at`, which T5 takes from the UPSTREAM
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
	DefaultRefireGrace = 1200 * time.Second
	// DefaultResolveGrace is how long past `source_ends_at` the reaper waits
	// before a case may expire (§B.4).
	DefaultResolveGrace = 300 * time.Second
	// DefaultGroupCloseDelay is how long a generation with no live member stays
	// open before it closes and freezes its thread. It EQUALS DefaultRefireGrace,
	// and the equality is the whole point rather than a coincidence.
	//
	// ⛔ IT WAS 300s WHILE `refire_grace` WAS 600s, WHICH DEFEATED `refire_grace`.
	// Reopening a case only avoids a new Slack root message if the group
	// GENERATION is still open — a closed generation freezes its thread and the
	// next observation opens generation N+1 with a brand-new root (§B.5). With the
	// old pair the generation closed 5 minutes after the resolve and the grace ran
	// for 10, so the whole second half of the grace bought a case reopen
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
	// DefaultStormThreshold is how many DISTINCT alerts must join one generation
	// inside DefaultStormWindow before it collapses to a single message.
	DefaultStormThreshold = 25
	// DefaultStormWindow is the window those joins are counted over.
	DefaultStormWindow = 60 * time.Second
	// DefaultStormCooldown is how long a generation must go WITHOUT a new member
	// before storm mode ends.
	DefaultStormCooldown = 600 * time.Second
)

// The §D.11 retention defaults (ADR 0024). They are per-org settings AND the
// floor `partitions.manage` starts from, which is why `platform/config` names
// them too — the partition dropper takes the MAXIMUM of the configured floor and
// every org's setting, so the two must start from the same number or a fresh
// install drops a partition an org's own retention still claims.
const (
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
)
