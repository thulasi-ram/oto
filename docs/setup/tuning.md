# Tuning oto

oto's defaults are **derived from a measured corpus** — the Alertmanager route timings the ecosystem
actually ships, and the `for:` durations real rule packs actually use — rather than chosen. Both
tables, with their sources, are in *[The numbers oto's defaults are derived from](#the-numbers-otos-defaults-are-derived-from)*
below, and the decision is [ADR 0026](../adr/0026-tuning-defaults-derived-from-a-real-rule-corpus.md).

They are still not right for every install, because **the correct value for almost every knob below is
a function of your own `alertmanager.yml` and your own alerting rules.** A setting that is sensible
against `group_interval: 5m` can be inert — or actively harmful — against `group_interval: 30s`.

This page explains what each knob does, both ways it can be wrong, and how to derive a value from
configuration you already have.

> **Status:** these are org-level settings (`orgs.settings`, SPEC §D.1) with the defaults below, and
> they are **editable per org over the API**:
>
> - `GET /api/v1/org/settings` returns each **effective** value, its **origin** (`default`, `org` or
>   `config`), the **config key** behind any `config` origin, any **shadowed** override, and the
>   **bounds** the server will accept, with the reason for each.
> - `PATCH /api/v1/org/settings` writes them. It is a partial write — an omitted key is left alone —
>   and `"reset": ["refire_grace_s"]` returns a key to oto's default.
>
> **The bounds below are enforced server-side, not by the form**, and they are checked against the
> merged state. A change takes effect on the **next evaluation**: nothing caches these numbers, so
> there is no restart and no window in which one pod runs the old tuning and another the new.
>
> Values already stored outside a bound are **clamped on read** rather than rejected, so a row written
> before a bound existed can never fail an alert.

---

## Setting these from configuration (Helm, a values file, environment variables)

**If you run oto under IaC, put the tuning in your configuration and it wins.** Precedence is:

```
shipped default   →   org override (Postgres)   →   declarative configuration   (highest)
```

Declarative wins because that is the GitOps expectation: if it is in the values file, the values file is
the truth. The failure this removes is the silent one — somebody edits a number in the UI, the next
deploy reverts it, and nobody can work out why the setting changed back, because nothing anywhere said
the deployment had an opinion.

Every key on this page can be set either way. In a config file:

```yaml
tuning:
  refire_grace_s: 1500
  group_close_delay_s: 900
  default_verbosity: firing_only
  broadcast_on_resolved: true
```

or as environment variables — the key, upper-cased, behind `OTO_TUNING_`:

```
OTO_TUNING_REFIRE_GRACE_S=1500
OTO_TUNING_FLAP_WINDOW_S=10800
OTO_TUNING_DEFAULT_VERBOSITY=firing_only
OTO_TUNING_UNACKED_REMINDER_MENTION_LIST=<@U012AB3CD>,<!subteam^S01ABCDEF>
```

An environment variable beats a file key for the same setting, and the API reports whichever one is
actually in force.

**What you get, and what you give up:**

- `GET /api/v1/org/settings` reports the key's origin as **`config`** and names the exact key in
  `config_keys` — `OTO_TUNING_REFIRE_GRACE_S` or `tuning.refire_grace_s`. A "managed by configuration"
  badge with no key beside it would just be a wall; this is where you go to change it.
- **`PATCH` on that key is refused with `409`**, and the problem names the config key. It is not accepted
  and quietly reverted, because that is precisely the mystery this exists to end.
- **An org override you already had is kept and shown.** If your org wrote `900` and configuration now
  forces `600`, the response reports the effective `600` with origin `config` **and** the shadowed `900`
  under `shadowed`. Delete the config key and the `900` takes effect again — nothing was destroyed on
  your behalf, and nothing is hidden.
- **The bounds still apply.** Configuration is authoritative about *which* value is in force, not about
  which values are legal: `refire_grace_s: 0` is refused from a values file exactly as it is from `curl`.
- **A bad key fails the boot**, with the config key named. An unknown key, an unparseable value or one
  outside its bound stops the process rather than starting a pod whose values file contains a line that
  silently does nothing.

---

## First: the three Alertmanager numbers everything depends on

**oto reads these for you, per source.** `GET /api/v1/sources` returns them under
`health.route_timings`, taken from the running configuration each Alertmanager publishes at
`/api/v2/status`, refreshed on every reconcile pass, with the time they were last observed. They are
never typed in and never stored in a browser — the previous design asked an operator to enter them and
kept the answer in `localStorage`, which meant two people could open the same screen and be given
contradictory guidance about the same cluster.

Three things to know about what you will see there:

- **Every field carries its own `provenance`, and there are three of them.** Alertmanager omits any of
  the three that your config does not set, and applies its own `30s` / `5m` / `4h` defaults later, in
  `dispatch.NewRoute`, somewhere the status endpoint cannot see. So a stock `alertmanager.yml` publishes
  none of them — which is why the answer is not a nullable number:

  | `provenance` | means | what you do about it |
  | --- | --- | --- |
  | `observed` | the value is stated in your configuration | change it in `alertmanager.yml` |
  | `default_applies` | your configuration states nothing, so Alertmanager's documented default governs | there is no line to change; state one explicitly, or move the oto knob |
  | `unknown` | oto could not read or parse the configuration at all | fix the source; every verdict that depends on this number is withheld |

  A `default_applies` field carries the number as well as the label, and the guidance uses it: the
  arithmetic is exactly as valid as for an observed value — a 2m re-fire grace is just as unreachable
  under a defaulted 5m `group_interval` as under a configured one — and the wording says whose number it
  is arguing from, because that is what changes your next move. It is **never** rendered as though oto
  had observed it. `defaults_from_version` names the Alertmanager release the defaults are attributed
  to, and `defaults_verified` is false when your source is newer than the release oto last checked those
  upstream constants against.
- **oto resolves the ROUTE TREE, not just the root.** All three settings are per-route and inherited, so
  the numbers that govern the alerts *oto* is sent are the ones on the route delivering to oto's own
  receiver — which on any Alertmanager that overrides anything is not the top-level route. oto walks the
  whole tree with Alertmanager's own semantics and serves the result:

  | field | is |
  | --- | --- |
  | `route` | `oto_receiver` when the three numbers are those of the route(s) reaching oto's receiver; `top_level` when they are the fallback |
  | `receiver` / `receiver_basis` | which receiver oto believes is its own, and **how it decided** |
  | `routes` | every *delivering* route in the tree, with its receiver, its matcher path, its inherited timings and whether it reaches oto |
  | `routes_agree` | false when several routes reach oto and state **different** timings |
  | `child_routes` / `child_routes_with_timings` | the same shape in two numbers, unchanged |

  What "delivering" means is Alertmanager's own rule from `dispatch.Route.Match`: a route is the answer
  for an alert only when **no child of it matched**. So a route with a matcher-less child never delivers
  anything itself, and a matcher-less child without `continue: true` makes every sibling after it
  unreachable — oto marks those, because a route that can never fire is a real misconfiguration.

- **⚠️ The answer can be a SET, and oto will not collapse it.** Alertmanager evaluates a route's children
  in order and stops at the first match *unless* that child sets `continue: true`, so **several routes
  can deliver to one receiver with different timings.** When that happens `routes_agree` is false, `route`
  falls back to `top_level`, and the screen says *"these two routes reach oto and disagree"* rather than
  picking one. Picking one — first match, slowest, an average — would be a number oto invented, which is
  the exact failure the hand-typed form was replaced for.

- **⚠️ Which receiver is oto's own is an INFERENCE, and it is labelled as one.** oto's ingest path is
  `/api/v1/ingest/alertmanager/{source_id}`, so your webhook URL literally contains the id of the source
  oto is probing — it would identify oto's receiver exactly. oto never sees it: `webhook_config.url` is a
  `SecretURL`, and `config.original` is the *marshalled* config, so it arrives as the literal string
  `<secret>`. So `receiver_basis` is one of:

  | `receiver_basis` | means | what oto does |
  | --- | --- | --- |
  | `sole_webhook` | exactly one receiver has a webhook integration | that receiver is oto's; this is the shape the setup guide produces (one webhook receiver is the *entire* Alertmanager change oto requires) |
  | `ambiguous` | several receivers have webhook integrations | shows **every** route, claims none, and names the candidates in `webhook_receivers` |
  | `no_webhook` | no receiver has a webhook integration | nothing here can push to oto — a real finding about the source |
  | `unknown` | the configuration has never been read | every verdict that depends on these numbers is withheld |

- **⛔ Nothing in oto evaluates a matcher against a label set, and nothing will.** Deciding which route a
  *particular* alert takes needs that alert's labels and would be a second, invisible implementation of
  somebody else's routing engine. Everything above is **structural** — derived from the shape of the tree
  and therefore true for every alert. Where structure cannot decide, the answer stays a set and is shown
  as one.
- **`observed_at` is not `updated_at`.** It moves only when the numbers beside it were genuinely read, so
  a stale reading looks stale.

The rest of this section is what those three numbers mean. If you are reading `alertmanager.yml` by hand
rather than the screen, read the route your oto receiver is attached to, including everything inherited
from its parents — that is the route oto resolves for you.

```yaml
route:
  group_by: [alertname, cluster, namespace]
  group_wait:      30s   # delay before the FIRST notification for a new group
  group_interval:  5m    # minimum gap before an update for a group that CHANGED
  repeat_interval: 4h    # gap before re-sending an UNCHANGED group
```

Those are Alertmanager's own defaults (`dispatch/route.go`). What they mean for oto:

- **`group_wait` is a floor on alert→Slack latency** that oto cannot improve. With the default there is
  an inherent ~30s delay before oto learns about a brand-new group. Also, and importantly for the case
  retention window W: *"If an alert is resolved before `group_wait` has elapsed, no notification will be
  sent"* — **the fastest flaps are invisible to oto entirely.** oto cannot damp, count or report what it
  is never told about.
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
  for: 15m    # <- this one
```

`for:` is the dwell time before Prometheus considers the rule firing. **It is the hard floor on how
fast an alert can possibly oscillate**, and it is what makes both `group_close_delay` (with the
`refire_grace` it is pinned to) and the case retention window W either meaningful or dead code. If your rules use `for:` durations that vary from `0s` to
`1h`, no single global value is correct for all of them; tune for the rules that actually misbehave.

---

## The numbers oto's defaults are derived from

**Every default on this page is derived from the two tables below rather than from a plausible
guess**, and if your own numbers differ from them, your own numbers win. The full decision, including
what was rejected and how to overturn it, is [ADR 0026](../adr/0026-tuning-defaults-derived-from-a-real-rule-corpus.md).

### 1. What the ecosystem actually sets in `alertmanager.yml`

| Source | `group_wait` | `group_interval` | `repeat_interval` | `group_by` |
|---|---|---|---|---|
| Alertmanager `dispatch.DefaultRouteOpts` ([source](https://github.com/prometheus/alertmanager/blob/main/dispatch/route.go)) | `30s` | `5m` | `4h` | *(empty)* |
| Alertmanager docs example ([source](https://prometheus.io/docs/alerting/latest/configuration/)) | `30s` | `5m` | `4h` | `[cluster, alertname]` |
| Alertmanager `doc/examples/simple.yml` | `30s` | `5m` | `3h` | `[alertname, cluster, service]` |
| **kube-prometheus-stack** `values.yaml` | `30s` | `5m` | **`12h`** | `[namespace]` |
| **kube-prometheus** `alertmanager-secret.yaml` | `30s` | `5m` | **`12h`** | `[namespace]` |
| **OpenShift** cluster-monitoring-operator | `30s` | `5m` | **`12h`** | `[namespace]` |
| **Grafana Alerting** notification-policy defaults | `30s` | `5m` | `4h` | — |
| oto's own dev compose (`sources/client/alertmanager/testdata/compose_v0.28.1.yaml`, a real capture) | `10s` | **`30s`** | `4h` | `[alertname, namespace]` |

**The finding that matters: `group_interval: 5m` is the one Alertmanager number the ecosystem does
not override.** `repeat_interval` moves constantly — the whole Kubernetes ecosystem independently
converged on `12h`, hands-on operator configs cluster at `1h`–`3h` — and `group_wait` moves on
per-severity child routes. `group_interval` does not move. Every oto default below is therefore
derived against `group_interval: 5m`, and the only real capture in this repo that disagrees (`30s`)
is used as the counter-case for every bound.

### 2. What `for:` real rule packs actually use

All 155 alerting rules shipped by **kube-prometheus-stack 88.2.0** (appVersion `v0.93.0`) — which is
kubernetes-mixin, the node-exporter mixin, and the Prometheus/Alertmanager/etcd/kube-state-metrics
mixins, i.e. the rules a large share of real clusters run unmodified:

| `for:` | rules | share | | `for:` | rules | share |
|---|---|---|---|---|---|---|
| *none* | 10 | 6.5 % | | **15m** | **69** | **44.5 %** |
| `0s` | 1 | 0.6 % | | `20m` | 3 | 1.9 % |
| `1m` | 3 | 1.9 % | | `30m` | 3 | 1.9 % |
| `2m` | 2 | 1.3 % | | `1h` | 12 | 7.7 % |
| `3m` | 1 | 0.6 % | | `3h` | 1 | 0.6 % |
| `5m` | 20 | 12.9 % | | `4h` | 2 | 1.3 % |
| `10m` | 28 | 18.1 % | | | | |

- **Mode and median are both `15m`.** `15m` + `10m` + `5m` is **75.5 %** of every rule in the corpus.
- Sub-5-minute `for:` is rare — 7 rules of 155 — and is concentrated in capacity cliffs
  (`KubePersistentVolumeFillingUp` critical, `1m`) and SLO burn-rate alerts.
- Longest in the corpus: `PrometheusTSDBCompactionsFailing`, `4h`.
- Independent corroboration from inside this repo: **oto's own rule pack**
  (`deploy/prometheus/oto-rules.yaml`, 23 alerts) has `15m` as the mode of its non-instantaneous
  rules (7 of 16), a median of `10m`, and a spread of `0m`/`15s`/`2m`/`5m`/`10m`/`15m`/`30m`.
- The Prometheus docs have **no** recommendation for `for:` at all: `prometheus.io/docs/practices/alerting/`
  names no duration, and the `5m`/`10m` in the configuration docs are illustrative examples. The
  de-facto standard is set by the mixins, not by upstream.

Sources: the rendered chart rules under
`prometheus-community/helm-charts/charts/kube-prometheus-stack/templates/prometheus/rules-1.14/`,
cross-checked against `kubernetes-monitoring/kubernetes-mixin` and
`prometheus/node_exporter/docs/node-mixin`. ⚠️ The mixin's `.libsonnet` files are *not* a reliable
place to read a number — several alerts take `for:` from a config variable — and
`kubernetes-mixin/prometheus_alerts.yaml` does not exist. Read the rendered chart YAML.

---

## `refire_grace` — default **1200s (20 minutes)**, accepted range **600–86400**

> ⚠️ **This setting no longer decides anything about a case.**
> [ADR 0040](../adr/0040-a-case-is-open-or-closed-and-never-reopened.md) made an AlertCase strictly
> terminal: **every** re-fire opens the next episode, unacknowledged, whatever the clock says. The
> setting survives for two reasons and no others — `group_close_delay` ships pinned at or above it,
> guarded by `TestGroupCloseDelayDoesNotDefeatTheRefireGrace`, and this setting's own floor of 600s is
> twice the 5-minute §C.5 ingest replay window (`MinRefireGraceSeconds = 2 × DedupTTL`, derived from
> that window rather than chosen). ADR 0040 §6 decided the rest: the key stays, under its own name, in
> `orgs.settings`, with its bounds unchanged, and renaming, re-homing and removal are all refused —
> deleting a settings key is a contract change of its own (SPEC §D.1), and the question this setting
> used to answer, *how long a gap a re-fire may leave before it counts as a separate episode*, belongs
> at case formation rather than at the lifecycle boundary. Read this section as *the number
> `group_close_delay` is tied to*; `group_close_delay` is what still has an effect you can see.
>
> **This default was also 600s and that was wrong.** Against the corpus above, a 10-minute value was
> unreachable for 76 % of real rules — see *The arithmetic against real numbers*, below. It moved to
> 1200s in [ADR 0026](../adr/0026-tuning-defaults-derived-from-a-real-rule-corpus.md), and
> `group_close_delay` moved with it, because on its own the change would have bought nothing. The
> arithmetic that produced 1200s is unchanged and still binding — it now reaches the number that
> matters through the pin rather than directly.

**What happens on a re-fire, now.** An alert resolves, then the same `alert_key` fires again:

- **A NEW AlertCase opens**, at `seq + 1`, **`unacked`**. Always. The closed episode is untouched, its
  acknowledgement does not carry over, and your alert history counts both firings, because they were
  both firings. *An acknowledgement is a receipt for one firing, and the second firing is not the one
  that was signed for.*
- **Whether Slack gets a new root message is decided entirely by `group_close_delay`.** If the
  AlertGroup generation is still open, the new case joins it: `chat.update` on the existing card, in
  the existing thread. If the generation has closed, the next observation opens generation N+1 with a
  **brand-new root card and a brand-new thread.**

So the knob that decides how many Slack threads a recurring problem generates is
**`group_close_delay`** — and since `group_close_delay` is pinned at or above `refire_grace`, setting
this one too low still fragments your threads. That is the failure mode to worry about, and the reason
is arithmetic, not taste:

> **If this window is shorter than `group_interval`, every single re-fire opens a new Slack thread.**

Alertmanager will not tell oto that a group changed sooner than `group_interval` after the last
notification. So the resolve notification and the subsequent re-fire notification are separated by *at
least* `group_interval` on the wire. If the window is 2 minutes and `group_interval` is 5 minutes, it
has always expired by the time oto is even capable of hearing about the re-fire — and since
`group_close_delay` is pinned to it, the generation has always closed by then too. Every re-fire opens
a new generation and posts a new root card, and you get exactly the wall of near-identical Slack
messages oto exists to prevent, from a setting that looks like it should have prevented it.

**Too long → a thread that outlives its subject.** Two genuinely separate incidents six hours apart
join one AlertGroup generation, share one Slack thread, and a thread from this morning grows a reply
about this evening's outage. ⭐ **Your history no longer lies about this, and that is ADR 0040's
doing**: both firings are their own case, with their own duration, `seq` apart. The cost of too long
is now confined to the notification surface — which message a fact lands on — and never reaches the
record.

### The arithmetic against real numbers

**The clock does not start when oto hears about the resolve. It starts when *Prometheus* stopped
considering the rule to be firing.** T5 sets the case's `ended_at` from the upstream `EndsAt`, not
from the observation time — deliberately, because that is when the problem actually ended. Everything
below follows from that one fact, and it is what the old default missed. (`group_close_delay`'s own
clock starts later, at the group's last *activity*; §*`group_close_delay` must be at least
`refire_grace`* below is why equal is safe rather than racy.)

For the same alert to fire again, its condition must hold continuously for the rule's `for:` all over
again, starting no earlier than `ended_at`. Alertmanager then batches the resulting notification. So:

```
earliest re-fire oto can OBSERVE  =  ended_at + for                       (best case)
typical                           =  ended_at + for + group_interval      (a full batching delay)
```

Run that against the two tables above — `group_interval: 5m`, and the corpus's `for:` distribution:

| Rule `for:` | share of corpus | earliest observable re-fire | typical | reachable at **600s**? | at **1200s**? |
|---|---|---|---|---|---|
| *none* / `0s` | 7.1 % | ~0 | `5m` | ✅ | ✅ |
| `1m`–`3m` | 3.9 % | 1–3m | 6–8m | ✅ | ✅ |
| `5m` | 12.9 % | `5m` | `10m` | ⚠️ exactly on the boundary | ✅ |
| **`10m`** | **18.1 %** | `10m` | `15m` | ❌ **only in the best case** | ✅ |
| **`15m`** *(mode)* | **44.5 %** | `15m` | `20m` | ❌ **never** | ⚠️ on the boundary |
| `20m`+ | 13.4 % | ≥20m | ≥25m | ❌ | ❌ |

**Cumulatively: 600s was reachable for rules up to `for: 5m` — 24 % of the corpus. 1200s is reachable
for every rule up to `for: 15m` — 86.5 %.** ("Reachable" now means *the generation is still open when
the re-fire lands*, which is `group_close_delay`'s question; the pin is what makes the same table
answer it.) The default is `for + group_interval` for the
modal rule: `15m + 5m = 20m`. That is the *smallest* value that reaches the commonest rule shape in
the wild, and picking the smallest such value is deliberate — see *Which error oto prefers*, below.

A `for: 15m` rule that also pays a full 5-minute batching delay lands exactly on the 20-minute
boundary. That case is left to the operator rather than absorbed into the default: raise your own
`refire_grace_s` to `1500` if your rules are long and your batching is slow. The screen will tell you.

**How to choose your own.** Two constraints, and the binding one is whichever is larger:

```
refire_grace  ≥  2 × group_interval                 (transport floor; below group_interval it does nothing at all)
refire_grace  ≥  (typical for:) + group_interval    (rule floor; THIS is usually the binding one)
```

| `group_interval` | Typical rule `for:` | Sane `refire_grace` |
|---|---|---|
| `5m` *(the ecosystem default)* | `15m` *(the corpus mode)* | **`20m`** — the shipped default |
| `5m` | `5m` | `10m`–`15m` |
| `5m` | `1h` *(node-exporter-style predictive rules)* | `65m`+, or accept that a re-fire opens a new thread |
| `30s` *(oto's dev compose)* | `15m` | `~16m`; the transport floor is irrelevant here, the rules bind |
| `15m` | `5m` | `30m`–`45m` — here the transport floor binds |

**The server refuses anything below 600s or above 86400s, and the floor is *derived*, not chosen.**
Note that **the floor is no longer the default**: it used to be, which meant oto shipped the lowest
value it was willing to accept and presented it as a recommendation.

> **`refire_grace` must be at least twice oto's ingest replay window (5 minutes), so the floor is 600s.**

oto suppresses a *replayed* webhook batch — an HA Alertmanager sibling, a retry after a 5xx — using a
content-addressed dedup key over `(source, groupKey, receiver, notification_reason, {fingerprint:status})`
for 5 minutes. A re-fire whose alert set has not changed produces the **same key**. So a value at or
below the replay window is unreachable for a second, sharper reason than `group_interval`:

- a re-fire inside the window is also inside the replay window, and is dropped at ingest before the
  state machine can see it at all;
- a re-fire the replay window lets through is, by the same arithmetic, already outside the window.

The two used to be **exactly equal** (both 10 minutes), which made the whole band unreachable by
construction — the first live verification run had to alter the alert set, changing the dedup key, to
exercise a re-fire at all. Doubling gives every legal configuration a band at least 5 minutes wide in
which a re-fire is both *observable* and *inside the window*. Zero is a Slack thread per transition.

⚠️ **What an empty band costs is smaller than it was, and the floor is kept anyway.** Since ADR 0040
the band selects no transition — every re-fire opens a new episode — so an empty band is a control
with no effect rather than an edge nobody could reach. The arithmetic stays because it is what stops
the two numbers being edited into contradiction, and because this setting's own future is undecided.
The ceiling is the thread-merge failure: beyond a day, two separate incidents share one Slack thread.

Then sanity-check the top end against how long your incidents actually last. If a typical incident is
resolved and genuinely gone in under ten minutes, a ten-minute window will hold the generation open
across distinct incidents; shorten it toward `2 × group_interval` and accept a few more threads.

### ⛔ `group_close_delay` must be at least `refire_grace`, or none of the above is true

A re-fire avoids a new Slack root message **only if the group generation is still open.** A closed
generation is never rejoined: the next observation opens generation N+1, and a new generation is a
new thread with a brand-new root card (§B.5). So a `group_close_delay` shorter than `refire_grace` gives you a setting on the screen
that promises to hold the thread together for twenty minutes and a generation that let go after five.

**oto used to ship exactly that**: `group_close_delay` 300s against a `refire_grace` of 600s, so the
entire second half of the grace was decorative. Both are now `1200s`. Equal is safe rather than racy,
because the two clocks start at different moments: `group_close_delay` runs from the group's last
*activity* — the resolve as oto observed it — while `refire_grace` runs from the upstream `ended_at`,
which is the same instant or earlier. The generation therefore always closes at or after the grace
expires, never before.

It is **not** enforced as a server-side bound, because a cross-key bound would reject a legal partial
`PATCH` that merely arrived in the wrong order. The settings screen warns instead.

### Which error oto prefers, and why this default is the *smallest* one that works

The two failures are not symmetric, and the choice between them is the whole product:

- **Too short → thread fragmentation.** Noisy, annoying, and every single message is a **loud,
  visible new root card.** Nobody misses anything. What you lose is readability.
- **Too long → a genuine re-fire folded into a stale thread.** The episode is recorded correctly
  either way; what is at risk is whether anybody *reads* it, which is the worst failure oto can have.

**oto prefers the loud error, and this default is chosen accordingly**: `1200s` is the *smallest*
value that reaches the modal rule, not the largest value that would cover every rule. Covering the
`for: 1h` tail would have meant 65 minutes, and 65 minutes holds one thread open across genuinely
separate incidents for the 76 % of rules that do not need it.

One thing makes the "too long" failure less bad than it sounds, and one thing makes it worse:

1. ~~**Flapping still engages.**~~ **NO LONGER TRUE — flap detection is RETIRED** (see below). An
   alert that fires and resolves repeatedly is no longer marked flapping, so a window that is folding
   too much is visible in the CASE list — one long episode with many observations — rather than in a
   badge.
2. **⚠️ A re-fire into a still-open generation is no longer announced loudly, and you should know
   that.** It used to arrive as the `refired` reason, which ADR 0020 granted an irreversible
   `reply_broadcast` on exactly this reasoning: *the thread said resolved and people stopped following
   it.* Since ADR 0040 retired the edge behind it, **nothing produces `refired`** — a re-fire is
   `fired`, widened to `new_alerts` where Alertmanager's own `notification_reason` says so — and
   `fired` is a card update, not a channel broadcast. So the longer this window, the more re-fires
   land as an in-place update on a thread whose last visible word was *resolved*. **If your channels
   run quieter than the default `status_changes`, do not raise this above the derived value.** (The
   older form of this warning — that `verbosity` dropped the `refired` reply at `firing_only` and
   `firing_and_resolved`, recorded as an open defect in ADR 0026 — is moot for the plainest possible
   reason: there is no `refired` reply to drop.)

---

## ⛔ Flap damping is RETIRED — `flap_threshold`, `flap_window`, `flap_digest_interval` now decide NOTHING

> **Do not tune these three. They are inert.** The keys still exist in `orgs.settings` and the
> settings API still accepts them, so a values file that sets them keeps working — but nothing in oto
> reads them any more. Everything from here to the end of this section is kept for the record of how
> the numbers were derived; none of it describes current behaviour.

**What replaced it.** Flap noise is damped at **case formation** by the *case retention window* W
(migration `00057`): a case whose alert has resolved stays OPEN for W, and a re-fire inside W lands in
that still-open case instead of opening the next one. One case across the flap, one root card, one
thread reply. Nothing is withheld at delivery, and no notification is suppressed for flapping.

**Why the detector went with it.** `flap_score` was an EWMA over lifecycle transitions, and W removes
exactly the transitions it counted: a damped flap appends neither `case.opened` nor `case.resolved`, so
the score read *below* `flap_threshold` precisely when an alert was flapping hardest. A detector that
reports false while the thing is happening is worse than no detector, so `alerts.flap_score` and
`alerts.is_flapping` are **retired in place** — the values already stored stay visible and readable,
and nothing writes them again. See **ADR 0041**, Amendment 1, and SPEC §B.6.2.

<details>
<summary>Historical: how the flap defaults were derived (ADR 0026)</summary>

> **`flap_window` was 1800s and it was wrong. `flap_threshold` was 5 and it survived.** Against the
> corpus above, `5`-in-`30m` was unreachable for *every* rule shape — the damper was dead code that
> looked configured. See *The `for:` trap*, below, and
> [ADR 0026](../adr/0026-tuning-defaults-derived-from-a-real-rule-corpus.md).

**What it did.** `flap_score` was an EWMA of state transitions per hour. When an alert exceeded
`flap_threshold` transitions within `flap_window`, oto marked it flapping and changed behaviour: the
alert still opened and closed cases normally, and the card still updated, but **thread replies stopped
and were replaced by one coalesced digest reply per `flap_digest_interval`**. Flapping was a visible
state in the UI, never a silent one.

**Too high (or unreachable) → the damper never engaged.** You kept every individual transition reply for
an alert that was oscillating, which is the noisiest possible outcome and the exact thing the feature
existed to stop.

**Too low → real transitions got collapsed into a digest.** An alert that legitimately fired, resolved
and fired again during a rolling deploy got marked as flapping, and the second firing was folded into a
15-minute digest instead of being announced. You found out late. Worse, "flapping" was displayed in the
UI, so a healthy alert was mislabelled as noisy and someone eventually deleted a useful rule because of
it.

### The `for:` trap — why the 30-minute window made this dead code

> **A threshold of 5 transitions in 30 minutes was unreachable for every rule shape in the corpus,
> including rules with no `for:` at all.**

What oto counts is lifecycle events on the alert — `case.opened`, `case.resolved`, `case.expired` and
the suppression pair — so **one full fire → resolve → fire cycle contributes exactly two.** (The
counting query also names the retired `case.reopened`, in both its spellings, so that a window
reaching back before ADR 0040 still reports the alert as restlessly as it actually was. Nothing
appends that type any more.) The question is therefore how fast a cycle can
physically be *observed*, and there are two independent floors:

- **The rule floor.** A `resolved → firing` edge needs the condition to hold continuously for `for:`
  first, starting no earlier than the resolve.
- **The transport floor.** Alertmanager will not send two notifications for the same group closer
  together than `group_interval`, and a cycle needs two of them — one saying resolved, one saying
  firing.

```
observable cycle  ≈  group_interval + max(group_interval, for)
observable transitions in a window W  ≈  2 × ⌊ W / cycle ⌋
```

Note the `max(...)`: **the transport floor binds even when `for:` is zero.** That is the term the
older arithmetic on this page was missing, and it is why the old default failed even in the case it
was supposedly written for. Run it at `group_interval: 5m`:

| Rule `for:` | share of corpus | cycle | transitions in **30m** | transitions in **2h** |
|---|---|---|---|---|
| *none* / `0s` | 7.1 % | `10m` | **6** | 24 |
| `5m` | 12.9 % | `10m` | **6** | 24 |
| `10m` | 18.1 % | `15m` | **4** | 16 |
| **`15m`** *(mode)* | **44.5 %** | `20m` | **2** | **12** |
| `30m` | 1.9 % | `35m` | 0 | 6 |
| `1h` | 7.7 % | `65m` | 0 | 3 |

**A threshold of 5 needs a ceiling of at least 10 to sit at "roughly half", and a 30-minute window
never produced one.** Even at the physical maximum — an alert with no `for:` oscillating as fast as
Alertmanager will report it — a 30-minute window tops out at 6 transitions. The damper could not
engage for any rule in the corpus. It was not conservative, it was dead code, and it looked correctly
configured right up until somebody asked why a wildly oscillating alert was never marked as flapping.

**How the current defaults are derived.** Set the threshold at roughly half the ceiling, then solve
the window for it:

```
flap_threshold  ≈  ⌊ W / cycle ⌋                       # ≈ half the observable ceiling
flap_threshold  ≥  3                                   # below this, one deploy looks like flapping
flap_window     ≈  flap_threshold × cycle              # the same identity, solved for W
```

For the modal rule — `for: 15m`, `group_interval: 5m`, so a `20m` cycle — a threshold of `5` needs
`5 × 20m = 100m`, **rounded up to 2 hours.** At that window the modal rule's ceiling is 12 and the
threshold sits at 42 % of it, comfortably inside the "roughly half" rule; every shorter `for:` in the
corpus has a larger ceiling still.

Rounding **up** is deliberate. A window that is too wide fails *visibly* — a stale "flapping" badge on
an alert that had settled — and self-healed within one 5-minute `flap.score` tick, because the window
is rolling. A window that is too narrow fails *invisibly*, as silence where a damper should have been.

**`flap_threshold` stays 5, and that is a result rather than an omission.** It is above the floor of 3
that keeps one ordinary rolling deploy from being mislabelled, and at 42 % of the modal ceiling it is
not near the top either. **The server refuses a threshold below 3** for that reason, and refuses one
above 100 because at that point the damper is unreachable for any real rule.

**On the `flap_window` floor of 300s.** It is *inert* at `group_interval: 5m` — a 300-second window
cannot hold even one 10-minute cycle — and it is kept anyway, on purpose. The only real Alertmanager
capture in this repo runs `group_interval: 30s`, where the cycle is 60 seconds, a 300-second window
holds five whole cycles, and `300` is exactly right. **A bound that excludes a value a real cluster
needs is as much a defect as a bound that admits a bad one**, so the floor stays where it is and the
arithmetic lives in the settings screen, which knows your `group_interval` and can do it properly.

For long-`for:` rules, do not lower the threshold to 2 — two transitions is a normal deploy, and you
would be labelling healthy alerts as flapping. **Widen the window instead.** For `for: 1h` rules
(7.7 % of the corpus, mostly node-exporter's predictive disk-fill alerts) flap damping is close to
meaningless at any window you would want to run; leave it and let it never trigger.

**`flap_digest_interval` (default 15m) survived unchanged.** It is how often a flapping alert is
allowed one summary reply. Keep it **at or above `group_interval`** — a digest interval shorter than
`group_interval` cannot produce more digests, it just adds jitter to when they land. `2 ×` up to `4 ×`
`group_interval` is the useful range, and 15m is exactly `3 ×` the 5m the ecosystem runs. Set it too
high and the digest arrives long after anyone cared; too low and the digest is not a digest.

</details>

---

## Broadcast — which transitions surface in the channel

**What it does.** oto's primary verb is `chat.update`: it edits the existing alert card in place instead
of posting a new message. That is what keeps a channel readable — but **`chat.update` is completely
silent.** No notification, no unread, nothing rises in the channel. Thread replies have the same
problem: they notify thread participants and nobody else.

`reply_broadcast` (Slack's *"Also send to #channel"*) is the antidote. The thread stays the record; the
one change that matters surfaces once in the channel. See
[ADR 0020](../adr/0020-broadcast-the-transitions-that-must-be-seen.md) for the full reasoning.

**Broadcast by default — two transitions, and the test is "is the quiet form *invisible*?"**

| Transition | Why the quiet form is invisible |
|---|---|
| Unacked reminder | Its purpose is to reach somebody who has **not** engaged. In-thread it reaches only the already-engaged, which is the wrong audience. |
| Re-fire (`refired`) ⚠️ **unreachable** | The thread said *resolved* and people stopped following it. This was the one re-fire that produced no new root message. **Nothing produces `refired` any more** — ADR 0040 retired the edge behind it, so a re-fire is `fired` (or `new_alerts`) and is not broadcast. The policy is intact and the value stays declared; it simply has no producer. A re-fire that finds a **closed** generation still posts a new root card, which was always loud. |

**There is no severity-increase broadcast, and there cannot be.** `severity` is a Prometheus label and
labels are hashed into an Alert's identity, so `warning` and `critical` versions of one rule are two
different Alerts with two different threads — no card ever goes from amber to red. See
[ADR 0020, Amendment 2](../adr/0020-broadcast-the-transitions-that-must-be-seen.md).

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
   an update, which is Tier 3 and effectively free. Two hundred pods alerting at once therefore spend
   two hundred seconds of that budget if every one of them broadcasts. **oto damps none of this for
   you** — storm collapse was removed ([ADR 0042](../adr/0042-storm-damping-is-removed.md)) and nothing
   replaced it at the notification layer. The two default broadcasts are the whole of the defence, and
   `broadcast_on_resolved` is the only one you can add.

**Interaction with `verbosity`.** Broadcast is decided per *transition*, then modulated by the
destination channel's `verbosity` and `thread_updates` settings. A channel that has opted out of thread
replies does not receive louder ones.

⛔ **There used to be one exception — the unacked reminder, always broadcast, because a reminder
nobody sees is not a reminder. oto no longer sends it at all** (git-bug `bd0fb1d`): nothing is
delivered that nobody asked for. `refired` is now the only reason that always broadcasts.

---

## The rest of the org settings

| Key | Default | What it does | Relationship to your config |
|---|---|---|---|
| `resolve_grace_s` | `300` (5m) | How long past an alert's `EndsAt` lease oto waits before the reaper marks the case **`expired`**. | Prometheus refreshes `EndsAt` on every send; the lease is typically `4 × scrape_interval` or `evaluation_interval` (commonly 3–4 minutes). Set `resolve_grace` **above** that lease, or a single missed scrape looks like an expiry. `5m` covers the usual case. |
| `group_close_delay_s` | `1200` (20m) | How long an AlertGroup stays `open` after its last member stops firing, before it closes. Closing a group is what makes the *next* fire open a new generation — and therefore a new Slack root message. | Keep it **at or above `group_interval`**, and **at or above `refire_grace`** — that second one is not a suggestion. **This is the setting that decides whether a re-fire is loud**: since ADR 0040 the episode is always a new case, so the only remaining question is whether its generation is still open, and this key is the whole of that question. It shipped as `300` against a `600` grace, which defeated half of it; both are now `1200`. See `refire_grace`, above. |
| `default_verbosity` | `status_changes` | The fallback for a Channel that names no verbosity of its own. A channel's own setting always wins — an org default can never make a quiet channel loud. | Set it to `firing_only` if most of your channels want the quietest setting and you would rather not repeat yourself. |
| `broadcast_on_resolved` | `false` | Whether `all_resolved` is broadcast into the channel rather than posted quietly in the thread. | See **Broadcast**, above. It is the only broadcast that is configurable, because a broadcast cannot be un-sent. |
| `raw_retention_days` | `30` | How long raw webhook payloads are kept before their partition is dropped, permanently. | Nothing in `alertmanager.yml` bears on it. The thirty is **chosen**, not derived: a replay is refused when the alerts a batch would touch have moved on, never because the batch is old, so this is the depth of the rejection and failed-batch feeds and the window a replay can be attempted in. See **Retention**, below. |
| `event_retention_months` | `13` | How long the instant-by-instant timeline is kept before the month is dropped, permanently. | The only setting on this page that destroys something oto cannot rebuild. See **Retention**, below, before you lower it. |

**`expired` is not `resolved`, and the distinction is load-bearing.** `expired` means *oto stopped
hearing about this* — Prometheus or Alertmanager went away. `resolved` requires an explicit
`status="resolved"` observation. The reaper will never expire a case while the alert source is
unhealthy; it holds the case in place and raises a `source.unreachable` banner instead. Do not
tune `resolve_grace_s` down to make expiries arrive faster. Losing sight of an alert is not the same as
the alert resolving, and a fast, wrong expiry is worse than a slow, correct one.

---

## Retention

**These are the only two settings in oto that delete anything.** Everything else on this page changes
*when* something fires. These decide how far back you can still look, they work by dropping whole
partitions, and a dropped partition is gone — there is no undo, no soft delete and, today, no export.
Read this section before you lower either number. The full reasoning and the measurements behind the
defaults are in [ADR 0024](../adr/0024-retention-defaults-and-cold-storage.md).

### What retention never touches

Start here, because it is the part most people get wrong. **Retention deletes the narrative, never the
record.** No setting on this page, at any value, deletes:

- the **alert** itself, its full label set, its annotations, when it was first seen and last seen, how
  many times it has fired, and the last flap verdict oto recorded before flap detection was retired;
- **every firing episode** — when it started, when and how it ended, whether that end was `resolved`
  or `expired`, whether it was suppressed and by what, who acknowledged it, when, and their ack note;
- **what the rule said at the moment it fired** — the rule snapshot;
- **who was told**, on which channel, in which thread, on which attempt, and whether it landed or died;
- the **daily alert-hygiene rollups**, which is what year-on-year comparisons are actually built from.

Those tables have no reaper at all. Every clause of oto's own claim — *"for every alert that has ever
fired: when it first appeared, every episode since, what the rule said at that moment, who was told, on
which channel, in which thread, who acknowledged it, and how it ended"* — is served by one of them and
survives both boundaries indefinitely.

### `raw_retention_days` (default 30) — what you lose

The raw webhook bodies Alertmanager sent, and the record of which elements of them oto rejected.

After the boundary you **can no longer**: see the exact bytes that produced an alert, inspect a
rejection feed older than the window, or replay a stored batch through a fixed parser. You **can still**
answer every question in the list above — none of it is served from here. Two screens do read these
tables: `GET /api/v1/sources/{id}/rejections` and `GET /api/v1/sources/{id}/failed-batches`, neither of
which takes a date range, because an operator asking *"why did my alert never appear?"* does not know
when it went missing. **This setting is the depth of both feeds.**

Thirty days is **chosen**, not derived. Age is not what makes a replay unsafe: `oto replay` refuses a
batch whose alerts have moved on since it arrived — a later batch already wrote to the case, or the case
closed while this batch still says firing — and it allows one at any age the bytes survive. So what the
thirty buys is the depth of those two feeds and the window in which a replay can be attempted at all;
past the boundary there is nothing to replay and no flag that changes it. **Lower it** if the payloads
are sensitive in your environment and you want them gone sooner; the cost is reproducibility and feed
depth, not history. Raising it buys more of both, and disk.

At a thousand alert firings a day this window costs about 50 MB; at ten thousand, about 500 MB.

### `event_retention_months` (default 13) — what you lose

The instant-by-instant timeline: the ordered transitions with an actor on each, the delivery attempts,
the enrichment markers — and, the one that matters, **every human comment and every unack
note. Those live nowhere else in oto and cannot be reconstructed from anything.** When the month is
dropped, the writing goes with it.

Concretely, `GET /alerts/{id}/events`, `GET /cases/{id}/events` and the group timeline stop
returning anything older than the window. The alert page, the episode list, the acks, the rule text and
the delivery record are all unaffected.

Thirteen months is the longest default that keeps a single org inside oto's one-Postgres design
([ADR 0014](../adr/0014-postgres-only-no-analytical-store.md)): at that design's own pessimistic
ceiling it is about 130 million rows and 98 GB. Year-on-year comparison — the reason 13 was originally
given — is served by the daily rollups, which are never deleted, so it works at any setting here.

**If you need years of timeline for an audit, raise this — the bound goes to 120 months (10 years) and
that is a supported setting, not a code change.** At high alert volume expect to cross ADR 0014's
revisit triggers well before ten years; that is a known consequence, not a surprise.

### There is no cold-storage export yet

A month that ages out is **not archived anywhere**. The export is designed — a pre-drop hook writing
one gzipped JSONL file per partition to a mounted directory, with a receipt table that blocks the drop
until the file exists — and it is **not built**. It is specified in ADR 0024 so it can be picked up as
work. Until it lands, the only way to keep timeline is to raise `event_retention_months`, and the only
way to keep a copy outside oto is your Postgres backup.

### On a multi-tenant deployment, retention is a floor

A partition holds every tenant's rows, so oto cannot drop one org's data and keep another's from the
same partition. `partitions.manage` therefore drops at the **maximum** window any org has configured.
An org that raises its retention gets everything it asked for. An org that lowers it may find its rows
survive longer than its setting says, because a shorter window is never allowed to delete a longer
org's data. Raising costs disk; lowering is never honoured at the expense of another tenant.

---

## A worked starting point

**If you run kube-prometheus-stack, kube-prometheus or OpenShift monitoring more or less as shipped,
change nothing — oto's defaults *are* the derivation for that case**, and this block is what they are:

```jsonc
{
  "refire_grace_s":       1200,    // for: 15m (corpus mode) + group_interval 5m
  "resolve_grace_s":       300,    // above a typical EndsAt lease of 3-4 minutes
  "group_close_delay_s":  1200     // = refire_grace; THIS is the one that decides new-thread-or-not

  // ⛔ The three flap keys are NOT here on purpose: flap detection is RETIRED and
  // they decide nothing. Setting them is harmless and has no effect.
}
```

**Two places where you should deviate, and the deviation is not small:**

- **Rules with a long `for:`.** If your alerting is dominated by node-exporter-style predictive rules
  (`for: 1h`), the observable cycle is 65 minutes: `refire_grace` — and with it `group_close_delay_s`,
  which is the one that keeps the thread — would need to be ~`4000`. (Flap damping used to need a
  matching window here; it is retired, so there is nothing to widen.)
- **A fast `group_interval`.** oto's own dev compose runs `group_interval: 30s`, where the cycle for
  an instantaneous rule is 60 seconds. There, the *rules* bind rather than the transport, and
  `refire_grace_s` can come down a long way — no lower than `600`, which is a transport limit and not
  a preference.

**And check one thing before adopting any of it:** does your `group_by` contain `instance`, `pod` or
another per-replica label? If it does, every alert lands in a group of one, so every alert gets its own
Slack root card and none of the grouping keys above changes anything — a flood by a different route,
and the fix is in `alertmanager.yml` rather than here. The Kubernetes ecosystem's
`group_by: [namespace]` is coarse and groups usefully; a `group_by` assembled by hand often is not.
