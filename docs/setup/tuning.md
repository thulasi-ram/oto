# Tuning oto

oto ships with defaults that are safe for a stock Alertmanager. They are not right for every install,
because **the correct value for almost every knob below is a function of your own `alertmanager.yml`
and your own alerting rules.** A setting that is sensible against `group_interval: 5m` can be
inert — or actively harmful — against `group_interval: 30s`.

This page explains what each knob does, both ways it can be wrong, and how to derive a value from
configuration you already have.

> **Status:** these values are currently org-level settings (`orgs.settings`, SPEC §D.1) with the
> defaults below. Making them editable per org in the UI is planned work, not shipped. Nothing here
> describes a feature you can change from the web UI today.

---

## First: the three Alertmanager numbers everything depends on

Read these out of your `alertmanager.yml` before touching anything in oto. They are per-route, so read
the route your oto receiver is attached to, including anything inherited from the parent route.

```yaml
route:
  group_by: [alertname, cluster, namespace]
  group_wait:      30s   # delay before the FIRST notification for a new group
  group_interval:  5m    # minimum gap before an update for a group that CHANGED
  repeat_interval: 4h    # gap before re-sending an UNCHANGED group
```

Those are Alertmanager's own defaults (`dispatch/route.go`). What they mean for oto:

- **`group_wait` is a floor on alert→Slack latency** that oto cannot improve. With the default there is
  an inherent ~30s delay before oto learns about a brand-new group. Also, and importantly for the flap
  knobs: *"If an alert is resolved before `group_wait` has elapsed, no notification will be sent"* —
  **the fastest flaps are invisible to oto entirely.** oto cannot damp, count or report what it is never
  told about.
- **`group_interval` is the clock rate of oto's whole view of the world.** oto does not learn about a
  change to an existing group faster than this. Your Slack card can be up to `group_interval` stale, by
  construction. **Every oto duration below should be read as a multiple of `group_interval`, not as an
  absolute time.**
- **`repeat_interval` is what produces `notification_reason: "repeat interval elapsed"`**, which oto
  maps to an **update-only** delivery — it never posts a new message for a nag. This is the single
  largest noise reduction oto provides and it needs no tuning. Note the consequence though: with the
  default, an unacknowledged critical is re-sent by Alertmanager only every **4 hours**, which is why
  oto runs its own unacked-reminder clock rather than relying on `repeat_interval`.

And one number from your **rules**, not your Alertmanager config:

```yaml
- alert: HighErrorRate
  expr: ...
  for: 10m    # <- this one
```

`for:` is the dwell time before Prometheus considers the rule firing. **It is the hard floor on how
fast an alert can possibly oscillate**, and it is what makes the flap thresholds either meaningful or
dead code. If your rules use `for:` durations that vary from `0s` to `1h`, no single global flap
threshold is correct for all of them; tune for the rules that actually misbehave.

---

## `refire_grace` — default **600s (10 minutes)**

**What it does.** An alert resolves, then the same `alert_key` fires again. `refire_grace` decides
whether that is *the same problem coming back* or *a new problem*:

- **Within the grace window** → the existing occurrence **reopens** (`reopen_count += 1`), oto reuses
  the existing Slack thread, and the card updates in place with a `refired` reply. **No new root
  message.**
- **After the grace window** → a **new occurrence** opens. If the alert group had already closed, that
  means a **new AlertGroup generation**, and a new AlertGroup generation means a **brand-new Slack root
  message and a brand-new thread.**

So `refire_grace` is, in practice, the knob that decides how many Slack threads a recurring problem
generates.

**Too short → thread fragmentation.** This is the failure mode to actually worry about, and the reason
is arithmetic, not taste:

> **If `refire_grace` is shorter than `group_interval`, every single re-fire opens a new Slack thread.**

Alertmanager will not tell oto that a group changed sooner than `group_interval` after the last
notification. So the resolve notification and the subsequent re-fire notification are separated by *at
least* `group_interval` on the wire. If `refire_grace` is 2 minutes and `group_interval` is 5 minutes,
the grace window has always expired by the time oto is even capable of hearing about the re-fire. The
window is unreachable. Every re-fire is classified as a new occurrence, opens a new generation, and
posts a new root card — and you get exactly the wall of near-identical Slack messages oto exists to
prevent, with a `refire_grace` setting that looks like it should have prevented it.

**Too long → history that lies.** Two genuinely separate incidents six hours apart get recorded as one
occurrence that "reopened", sharing one thread and one duration. Your alert history under-counts
occurrences, the duration statistics are meaningless (one occurrence with a six-hour gap in the middle),
and a Slack thread from this morning grows a reply about this evening's outage.

**How to choose.** Start from `group_interval` and give it real headroom:

```
refire_grace  ≥  2 × group_interval          (hard floor; below group_interval it does nothing at all)
refire_grace  ≈  3 × group_interval          (a reasonable default)
```

| Your `group_interval` | Sane `refire_grace` |
|---|---|
| `30s` | `5m` (the default is fine, and generous) |
| `5m` *(Alertmanager default)* | **`10m`–`15m`** — the shipped default of 10m is exactly `2 ×` |
| `15m` | `30m`–`45m` |

Then sanity-check the top end against how long your incidents actually last. If a typical incident is
resolved and genuinely gone in under ten minutes, a ten-minute grace window will merge distinct
incidents; shorten it toward `2 × group_interval` and accept a few more threads.

---

## Flap damping — `flap_threshold` **5**, `flap_window` **1800s (30m)**, `flap_digest_interval` **900s (15m)**

**What it does.** `flap_score` is an EWMA of state transitions per hour. When an alert exceeds
`flap_threshold` transitions within `flap_window`, oto marks it flapping and changes behaviour: the
alert still opens and closes occurrences normally, and the card still updates, but **thread replies stop
and are replaced by one coalesced digest reply per `flap_digest_interval`**. Flapping is a visible state
in the UI, never a silent one.

**Too high (or unreachable) → the damper never engages.** You keep every individual transition reply for
an alert that is oscillating, which is the noisiest possible outcome and the exact thing the feature
exists to stop.

**Too low → real transitions get collapsed into a digest.** An alert that legitimately fired, resolved
and fired again during a rolling deploy gets marked as flapping, and the second firing is folded into a
15-minute digest instead of being announced. You find out late. Worse, "flapping" is displayed in the
UI, so a healthy alert is mislabelled as noisy and someone eventually deletes a useful rule because of
it.

### The `for:` trap — why the default is unreachable for many rules

> **A threshold of 5 transitions in 30 minutes is unreachable for a rule with `for: 10m`.**

Work it through. Every `resolved → firing` edge requires the rule's condition to hold continuously for
`for:` first. With `for: 10m`, a 30-minute window can physically contain **at most 3** firing edges —
and only if resolution and re-detection were instantaneous, which they are not. Add that oto only
observes a state change when Alertmanager sends one, no faster than `group_interval` (5m), and the
observable ceiling in a 30-minute window drops to about **6 notifications total**, of which only some
are transitions.

The practical ceiling on transitions oto can *see* in a window `W` is roughly:

```
max_observable_transitions  ≈  2 × W / (for + group_interval)
```

For `for: 10m`, `group_interval: 5m`, `W = 30m`: `2 × 30 / 15` = **4**. The threshold is 5. **The
damper can never fire.** It is not conservative, it is dead code — and it will look correctly configured
right up until someone asks why a wildly oscillating alert was never marked as flapping.

**How to choose.** Compute the ceiling for the rules you care about, then set the threshold at roughly
half of it:

```
flap_threshold  ≈  W / (for + group_interval)          # ≈ half the observable ceiling
flap_threshold  ≥  3                                   # below this, one deploy looks like flapping
```

| Rule `for:` | `group_interval` | Ceiling in 30m | Workable `flap_threshold` |
|---|---|---|---|
| `0s` / `30s` | `30s` | ~30+ | `5` (the default is right; this is the case it was written for) |
| `2m` | `5m` | ~8 | `4` |
| `10m` | `5m` | ~4 | **raise `flap_window` instead** — see below |
| `30m`+ | any | ~1 | flap damping is meaningless; leave it, it will never trigger |

For long-`for:` rules, do not lower the threshold to 2 — two transitions is a normal deploy, and you
would be labelling healthy alerts as flapping. **Widen the window instead.** With `for: 10m`, a window
of `3h` and a threshold of `5` describes something genuinely pathological, and the threshold becomes
reachable:

```
flap_window  ≈  flap_threshold × (for + group_interval) × 2
```

**`flap_digest_interval` (default 15m)** is how often a flapping alert is allowed one summary reply.
Keep it **at or above `group_interval`** — a digest interval shorter than `group_interval` cannot
produce more digests, it just adds jitter to when they land. `2 × group_interval` up to `4 ×` is the
useful range. Set it too high and the digest arrives long after anyone cared; too low and the digest is
not a digest.

---

## Storm collapse — `storm_threshold` **25**, `storm_window` **60s**, `storm_cooldown` **600s (10m)**

**What it does.** If more than `storm_threshold` distinct alerts join **one AlertGroup generation**
within `storm_window`, the group enters storm mode: oto posts or updates exactly **one** root message
carrying a count and a link, and **suppresses every per-alert thread reply**. Storm mode ends after
`storm_cooldown` with no new members. Like flapping, it is a visible state.

**Too high → the flood you built this for.** A node pool dies, 200 pods alert, and oto faithfully posts
200 thread replies. Emitting 1,000 Slack messages is strictly worse than emitting zero, and you will
also hit `chat.postMessage`'s ~1 message/second/channel limit and spend twenty minutes delivering a
backlog nobody will read.

**Too low → oto goes quiet exactly when you need detail.** Storm mode suppresses per-alert replies. A
threshold of 5 means a routine deploy touching six services collapses into "6 alerts, see them all in
oto" — and the operator has to leave Slack to find out *which* six. Storm mode is a defence against a
flood, not a summarisation strategy.

### `group_by` decides whether these knobs do anything at all

This is the one that catches people. `storm_threshold` counts alerts joining **one group**, and your
`group_by` decides how many alerts can ever be in a group:

```yaml
group_by: [alertname, cluster, namespace]   # coarse — one group can hold hundreds. Storm mode works.
group_by: [alertname, instance, pod]        # fine — one group holds ~1 alert. Storm mode can NEVER fire.
group_by: [...]                             # every label — one group per alert. Same.
```

**If your `group_by` includes a high-cardinality label (`instance`, `pod`, `container`), storm collapse
is unreachable at any threshold**, because no group ever accumulates 25 members. You will get one root
card per alert instead, which is a flood by a different route. The fix is in `alertmanager.yml`, not in
oto: group on the labels that describe the *problem*, not the ones that describe the *instance*.

**How to choose.**

- **`storm_threshold`** — set it above your largest *normal* group and below your smallest *abnormal*
  one. A useful proxy: the number of replicas in your largest deployment. If your biggest service runs
  40 pods and a bad deploy alerts on all of them, `25` is right. If your groups never exceed 8 members
  even in an outage, lower it to `10`; if you routinely see 60-member groups during ordinary deploys,
  raise it, or you will lose per-alert detail every Tuesday.
- **`storm_window`** — must be **longer than `group_wait`**, or a burst that Alertmanager is still
  batching will not look like a burst to oto. With `group_wait: 30s`, the shipped `60s` is a sensible
  `2 ×`. If you have raised `group_wait` to `2m` to reduce noise, raise `storm_window` to `4m` with it,
  or storm mode will trigger inconsistently depending on where the burst falls relative to the batch
  boundary.
- **`storm_cooldown`** — how long the group must be quiet before normal per-alert behaviour resumes.
  Keep it **at or above `group_interval`**, otherwise storm mode will flicker on and off across
  consecutive batches. The default `10m` is `2 ×` the Alertmanager default.

---

## Broadcast — which transitions surface in the channel

**What it does.** oto's primary verb is `chat.update`: it edits the existing alert card in place instead
of posting a new message. That is what keeps a channel readable — but **`chat.update` is completely
silent.** No notification, no unread, nothing rises in the channel. A card can go from `warning` to
`critical` and everyone can miss it. Thread replies have the same problem: they notify thread
participants and nobody else.

`reply_broadcast` (Slack's *"Also send to #channel"*) is the antidote. The thread stays the record; the
one change that matters surfaces once in the channel. See
[ADR 0020](../adr/0020-broadcast-the-transitions-that-must-be-seen.md) for the full reasoning.

**Broadcast by default:**

| Transition | Why |
|---|---|
| Severity increase (`warning` → `critical`) | Otherwise a silent edit changes the card from amber to red and tells nobody. |
| Re-fire within `refire_grace` | The one re-fire case that produces no new root message, so it is the one that is otherwise invisible. (A re-fire *after* the grace window already posts a new card — see `refire_grace` above.) |
| Storm detected | oto has just started suppressing individual notifications. People must be told the tool changed behaviour, or the quiet that follows is indistinguishable from nothing happening. |
| Unacked reminder | Its entire purpose is to be seen. |

**Never broadcast:** acknowledged, comment, enriched, snoozed. Each is a fact about the *response*,
addressed to people already in the thread. Broadcasting an ack would double the channel traffic of every
well-handled alert.

**Configurable, default off:** resolved. Closure is welcome, and on a quiet channel you may well want
it. On a busy channel it doubles traffic for the least urgent fact oto has — nobody was ever paged
because a resolve arrived quietly.

**Two things to know before you turn any of this up.**

1. **A broadcast cannot be undone.** Slack offers no un-broadcast. The bar is *"would an on-call
   engineer be angry to have missed this?"*, not *"is this interesting?"* A channel that learns to
   scroll past oto's broadcasts has lost the only mechanism oto has for genuine urgency.
2. **A broadcast is a `chat.postMessage`**, so it spends the ~1 message/second/channel budget — unlike
   an update, which is Tier 3 and effectively free. This is why broadcasts are damped during a storm:
   one broadcast per interesting transition, during the exact event that generates hundreds of them, is
   a self-inflicted flood. In storm mode exactly one broadcast is permitted — the storm announcement
   itself.

**Interaction with `verbosity`.** Broadcast is decided per *transition*, then modulated by the
destination channel's `verbosity` and `thread_updates` settings. A channel that has opted out of thread
replies does not receive louder ones. The single exception is the unacked reminder, which is always
broadcast — a reminder nobody sees is not a reminder.

---

## The rest of the org settings

| Key | Default | What it does | Relationship to your config |
|---|---|---|---|
| `resolve_grace_s` | `300` (5m) | How long past an alert's `EndsAt` lease oto waits before the reaper marks the occurrence **`expired`**. | Prometheus refreshes `EndsAt` on every send; the lease is typically `4 × scrape_interval` or `evaluation_interval` (commonly 3–4 minutes). Set `resolve_grace` **above** that lease, or a single missed scrape looks like an expiry. `5m` covers the usual case. |
| `group_close_delay_s` | `300` (5m) | How long an AlertGroup stays `open` after its last member stops firing, before it closes. Closing a group is what makes the *next* fire open a new generation — and therefore a new Slack root message. | Keep it **at or above `group_interval`**, and consider aligning it with `refire_grace`. If `group_close_delay` is much shorter than `refire_grace`, a re-fire inside the grace window still finds a closed group. |
| `raw_retention_days` | `14` | How long raw webhook payloads are kept before their partition is dropped. | Storage, not behaviour. Raise it if you debug ingestion often. |
| `event_retention_months` | `13` | How long the event timeline is kept. | Thirteen months so year-on-year comparisons work. |

**`expired` is not `resolved`, and the distinction is load-bearing.** `expired` means *oto stopped
hearing about this* — Prometheus or Alertmanager went away. `resolved` requires an explicit
`status="resolved"` observation. The reaper will never expire an occurrence while the alert source is
unhealthy; it holds the occurrence in place and raises a `source.unreachable` banner instead. Do not
tune `resolve_grace_s` down to make expiries arrive faster. Losing sight of an alert is not the same as
the alert resolving, and a fast, wrong expiry is worse than a slow, correct one.

---

## A worked starting point

If your Alertmanager is stock (`group_wait: 30s`, `group_interval: 5m`, `repeat_interval: 4h`) and your
rules mostly use `for: 5m`–`10m`:

```jsonc
{
  "refire_grace_s":        900,    // 3 × group_interval — comfortably reachable
  "resolve_grace_s":       300,    // default; above a typical EndsAt lease
  "group_close_delay_s":   300,    // = group_interval

  "flap_threshold":          5,    // keep the default...
  "flap_window_s":       10800,    // ...but widen to 3h so `for: 10m` rules can reach it
  "flap_digest_interval_s": 900,   // 3 × group_interval

  "storm_threshold":        25,    // assumes coarse group_by; check yours
  "storm_window_s":         60,    // 2 × group_wait
  "storm_cooldown_s":      600     // 2 × group_interval
}
```

**Before adopting any of this, check one thing:** does your `group_by` contain `instance`, `pod` or
another per-replica label? If it does, storm collapse cannot engage at any threshold, and the fix is in
`alertmanager.yml` rather than here.
