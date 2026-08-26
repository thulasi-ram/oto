---
title: 0026 — `refire_grace` and the flap thresholds, derived from a real rule corpus
---
> ⛔ **SUPERSEDED IN PART — BOTH SETTINGS THIS ADR RAISED ARE DELETED.** `refire_grace_s` and
> `group_close_delay_s` no longer exist: git-bug `7287b28`, migration `00071`. ADR 0040 retired the
> transition `refire_grace` selected, and git-bug `7570090` / migration `00069` deleted the
> `alert_groups` generation that `group_close_delay` timed, so neither had a reader left outside its
> own CRUD. The flap thresholds are unaffected by that deletion and remain inert under ADR 0042
> Amendment 3.
>
> ⭐ **THE DERIVATION IS NOT SUPERSEDED AND IS WHY THIS DOCUMENT IS STILL WORTH READING.** The two
> measured tables — `group_interval: 5m` as the one Alertmanager number the ecosystem does not
> override, and `for: 15m` as the mode *and* median of kube-prometheus-stack 88.2.0's 155 rules — are
> the corpus every later window is still sized against, including the case retention window W and a
> notification policy's digest window. The three arithmetic defects found here are also still the best
> account of how a plausible-looking default can be unreachable in practice.
>
> ⚠️ **AND ONE CLAIM IN §4 WAS ALWAYS UNENFORCED**, which the deletion makes moot rather than fixes:
> *"`group_close_delay ≥ refire_grace` is safe at equality"* was two independent `1200 * time.Second`
> literals with a comment insisting the equality was the whole point, and the only check compared the
> two DEFAULTS — so two operator-written settings could contradict each other freely. Deleting both is
> what closed it. Deleting one would have left the trap armed with its tripwire removed.

**Status:** Accepted · 2026-08-09
**Decided WITHOUT the owner.** See *How to overturn this*, below — it is designed to be cheap.
**Relates to:** [0020](/oto/adr/0020-broadcast-the-transitions-that-must-be-seen/) (why a re-fire inside the
grace is broadcast at all, and the defect this ADR found in that guarantee),
[0005](/oto/adr/0005-durable-group-key-owns-the-slack-thread/) (why a closed generation means a new thread),
[0024](/oto/adr/0024-retention-defaults-and-cold-storage/) (the same "decided without the owner, derived not
chosen" shape)
**Amends:** SPEC §B.5 (`refire_grace` default), §B.6 (`flap_window` default), §D.1 (`orgs.settings`
defaults)
**Resolves:** git-bug `42b1e57` — *"refire_grace and the flap thresholds have never met a real
alertmanager.yml"*, and `docs/ORCHESTRATION.md` open question 4.

## Context

Four of oto's tuning defaults were guesses: `refire_grace` 600s, `group_close_delay` 300s,
`flap_threshold` 5, `flap_window` 1800s. `docs/ORCHESTRATION.md` listed them as needing the owner —
*"the knobs that decide how noisy Slack is. Should be validated against a real `alertmanager.yml`
before they harden."* They had since acquired server-enforced bounds, per-org overrides, a
declarative config layer, a tuning screen that reads route timings off the live cluster, and a page of
arithmetic in `docs/setup/tuning.md` — all of it hanging off numbers nobody had checked.

The charge is exact and it is correct. What follows is the evidence, then what the arithmetic does to
each default when you run it against real numbers instead of plausible ones.

### 1. The evidence base

Two measured tables. Nothing below is a "typical cluster"; every row has a source.

**What the ecosystem sets in `alertmanager.yml`:**

| Source | `group_wait` | `group_interval` | `repeat_interval` | `group_by` |
|---|---|---|---|---|
| Alertmanager `dispatch.DefaultRouteOpts` (`v0.33.1`, `main`) | `30s` | `5m` | `4h` | *(empty)* |
| Alertmanager `docs/configuration.md` reference | `30s` | `5m` | `4h` | — |
| Alertmanager `doc/examples/simple.yml` | `30s` | `5m` | `3h` | `[alertname, cluster, service]` |
| kube-prometheus-stack `values.yaml` | `30s` | `5m` | `12h` | `[namespace]` |
| kube-prometheus `alertmanager-secret.yaml` | `30s` | `5m` | `12h` | `[namespace]` |
| OpenShift cluster-monitoring-operator | `30s` | `5m` | `12h` | `[namespace]` |
| Grafana Alerting notification-policy defaults | `30s` | `5m` | `4h` | — |
| **oto's own compose capture** (`sources/client/alertmanager/testdata/compose_v0.28.1.yaml`) | `10s` | **`30s`** | `4h` | `[alertname, namespace]` |

Plus a spread of published operator configs deviating to `group_wait` `10s`–`1m`, `group_interval`
`30s`–`1h`, `repeat_interval` `10m`–`12h`.

> **`group_interval: 5m` is the one Alertmanager number the ecosystem does not override.**
> `repeat_interval` moves constantly and in both directions — the entire Kubernetes ecosystem
> independently converged on `12h`, hands-on operator configs cluster at `1h`–`3h`. `group_wait` moves
> on per-severity child routes. `group_interval` sits at `5m` in every one of the platform defaults
> above. **It is therefore the number every derivation here is run against**, with the `30s` capture
> as the standing counter-case for every bound.

**What `for:` real rule packs use** — all 155 alerting rules in kube-prometheus-stack 88.2.0
(appVersion `v0.93.0`): kubernetes-mixin, the node-exporter mixin, and the
Prometheus/Alertmanager/etcd/kube-state-metrics mixins.

| `for:` | rules | share | `for:` | rules | share |
|---|---|---|---|---|---|
| *none* | 10 | 6.5 % | **15m** | **69** | **44.5 %** |
| `0s` | 1 | 0.6 % | `20m` | 3 | 1.9 % |
| `1m` | 3 | 1.9 % | `30m` | 3 | 1.9 % |
| `2m` | 2 | 1.3 % | `1h` | 12 | 7.7 % |
| `3m` | 1 | 0.6 % | `3h` | 1 | 0.6 % |
| `5m` | 20 | 12.9 % | `4h` | 2 | 1.3 % |
| `10m` | 28 | 18.1 % | | | |

> **`15m` is both the mode and the median. `15m` + `10m` + `5m` is 75.5 % of the corpus.**

Corroborated from inside this repo: `deploy/prometheus/oto-rules.yaml`, oto's own 23-alert rule pack,
has `15m` as the mode of its 16 non-instantaneous rules and a median of `10m`.

Two negative results worth recording, because they are what makes the corpus the authority:
`prometheus.io/docs/practices/alerting/` gives **no** guidance on `for:` and names no duration, and
`kubernetes-mixin/prometheus_alerts.yaml` does not exist — the mixin does not commit a rendered file,
and several of its `.libsonnet` alerts take `for:` from a config variable, so the rendered chart YAML
is the only reliable place to read a number.

### 2. What the arithmetic does to `refire_grace`

**The finding is a piece of code, not an opinion.** `lifecycle.go`'s T5 sets the occurrence's
`ended_at` from `cmd.At.OccurredAt()` — the **upstream** `EndsAt`, the moment Prometheus stopped
considering the rule firing, not the moment oto heard about it. `withinRefireGrace` measures from
there. So a re-fire has to pay the rule's whole `for:` dwell all over again *inside* the grace window,
and Alertmanager then batches the notification on top:

```
earliest re-fire oto can OBSERVE  =  ended_at + for                     (best case)
typical                           =  ended_at + for + group_interval
```

| Rule `for:` | share | typical observed re-fire | reachable at **600s**? | at **1200s**? |
|---|---|---|---|---|
| *none* / `0s` / `1m`–`3m` | 11.0 % | ≤ 8m | ✅ | ✅ |
| `5m` | 12.9 % | `10m` | ⚠️ exactly on the boundary | ✅ |
| `10m` | 18.1 % | `15m` | ❌ best case only | ✅ |
| **`15m`** *(mode)* | **44.5 %** | `20m` | ❌ **never** | ⚠️ on the boundary |
| `20m`+ | 13.4 % | ≥ 25m | ❌ | ❌ |

**600s was reachable for 24 % of the corpus. 1200s is reachable for 86.5 %.**

### 3. What the arithmetic does to the flap thresholds — and the term everybody missed

`StateChangeCounts` counts lifecycle events (`occurrence.opened|reopened|resolved|expired` and the
suppression pair), so **one full fire → resolve → fire cycle contributes exactly two.** A cycle pays
the larger of two independent floors:

- the **rule** floor — the condition must hold for `for:` again;
- the **transport** floor — Alertmanager will not send two notifications for one group closer together
  than `group_interval`, and a cycle needs two of them, one resolved and one firing.

```
observable cycle  =  group_interval + max(group_interval, for)
transitions in W  ≈  2 × ⌊ W / cycle ⌋
```

`docs/setup/tuning.md` and the tuning screen both carried `2 × W / (for + group_interval)`, which
**omits the transport floor** and therefore claims a rule with no `for:` can complete a cycle in one
`group_interval`. It cannot; it needs two flushes. With the term restored, at `group_interval: 5m`:

| Rule `for:` | cycle | transitions in **30m** | in **2h** |
|---|---|---|---|
| *none* / `0s` / `5m` | `10m` | **6** | 24 |
| `10m` | `15m` | **4** | 16 |
| **`15m`** *(mode)* | `20m` | **2** | **12** |
| `1h` | `65m` | 0 | 3 |

> **A threshold of 5 needs a ceiling of ≥ 10 to sit at "roughly half", and a 30-minute window never
> produced one for any rule shape in the corpus — including rules with no `for:` at all.** The damper
> was dead code that looked configured.

### 4. The defect the two defaults had *between* them

`refire_grace` avoids a new Slack root message only while the group **generation** is still open; a
closed generation is never rejoined and the next observation opens N+1, which is a new thread with a
new root (ADR 0005, §B.5). oto shipped `group_close_delay: 300s` against `refire_grace: 600s`, so the generation closed
five minutes into a ten-minute grace and **the entire second half of the grace bought an occurrence
reopen that posted a new card anyway.** oto's own tuning page already stated the rule — *"keep
`group_close_delay` at or above `refire_grace`"* — and its own shipped defaults broke it. Its worked
example broke it harder (`refire_grace: 900`, `group_close_delay: 300`).

### 5. Which error oto prefers — and it is not the one this ADR was expected to pick

The two failures are not symmetric:

- **grace too short** → thread fragmentation. Noisy and annoying, and **every message is a loud,
  visible new root card**. Nobody misses anything.
- **grace too long** → a genuine re-fire folded into a stale thread. That is the shape of a **missed
  page**, the worst failure oto can have.

**oto prefers the loud error.** So the number picked is the *smallest* value that reaches the modal
rule, not the largest that would cover every rule: covering the `for: 1h` tail would have meant a
65-minute grace and would have merged genuinely separate incidents for the 76 % of rules that do not
need it. `1200` is `for + group_interval` at the mode, and a `for: 15m` rule that also pays a full
batching delay is deliberately left on the boundary for the operator to raise.

Two things soften the "too long" failure, and one thing sharpens it:

1. A re-fire inside the grace is **`reply_broadcast`ed into the channel** — it is one of only two
   transitions ADR 0020 grants an irreversible channel post, on exactly this reasoning.
2. Repeated reopens still cross `flap_threshold`, so an over-folding grace is **visible** in the UI.
3. **⚠️ Open defect, found here and deliberately NOT fixed here.** ADR 0020 grants `refired` a
   broadcast because its quiet form is *invisible*. §H.6's verbosity table then drops the `refired`
   reply entirely at `firing_and_resolved` and `firing_only`, and `refired` is **not** in
   `ungatedReplies`. **On a channel quieter than the default `status_changes`, a re-fire inside the
   grace is silent** — and the longer the grace, the more re-fires fall into that hole. The two
   documents contradict each other. Resolving it means deciding whether `firing_only` may delete a
   transition ADR 0020 called unmissable, which is a product decision and not a tuning one. It is
   written into `docs/setup/tuning.md` as a warning and left for the owner.

## Decision

| Key | Was | Now | Derivation |
|---|---|---|---|
| `refire_grace_s` | `600` | **`1200`** | `for` (15m, corpus mode) `+ group_interval` (5m, ecosystem constant) |
| `group_close_delay_s` | `300` | **`1200`** | `= refire_grace_s`, or the grace posts a new root card anyway |
| `flap_threshold` | `5` | **`5` — unchanged** | 42 % of the modal rule's 2-hour ceiling of 12; above the floor of 3 that keeps one rolling deploy from being mislabelled |
| `flap_window_s` | `1800` | **`7200`** | `flap_threshold × cycle` = `5 × 20m` = 100m, **rounded up** to 2h |
| `flap_digest_interval_s` | `900` | **`900` — unchanged** | `3 × group_interval`, inside the stated 2×–4× band |

Rounding the flap window **up** is deliberate and asymmetric: a window that is too wide fails
*visibly*, as a stale "flapping" badge that self-heals within one 5-minute `flap.score` tick because
the window is rolling; a window that is too narrow fails *invisibly*, as silence where a damper should
have been.

**`group_close_delay ≥ refire_grace` is safe at equality** rather than racy, because the two clocks
start at different moments: the close delay runs from the group's last *activity* (the resolve as oto
observed it) while the grace runs from the upstream `ended_at`, which is the same instant or earlier.
The generation therefore always closes at or after the grace expires. It is **not** enforced as a
cross-key bound, because a cross-key bound would reject a legal partial `PATCH` that merely arrived in
the wrong order; the settings screen warns instead.

### Every bound was re-checked and NONE moved

A bound that excludes a value a real cluster needs is as much a defect as one that admits a bad value.

| Bound | Verdict |
|---|---|
| `refire_grace_s` min `600` | **Kept.** It is `2 × DedupTTL`, a property of Alertmanager's retry budget rather than of anybody's route timing, so even the `30s`-`group_interval` capture cannot use less. What changed is that **the default no longer sits on the floor** — oto used to ship the lowest value it was willing to accept and present it as a recommendation. |
| `refire_grace_s` max `86400` | **Kept.** Beyond a day two separate incidents merge into one occurrence. |
| `flap_window_s` min `300` | **Kept, knowing it is inert at `group_interval: 5m`.** The compose capture runs `group_interval: 30s`, where the cycle is 60s, a 300s window holds five whole cycles and `300` is exactly right. Raising the floor would have excluded the only real capture in this repo. The arithmetic belongs in the settings screen, which knows the cluster's own numbers. |
| `flap_threshold` min `3` / max `100` | **Kept.** `3` is the rolling-deploy floor. `100` is reachable at a fast `group_interval` (a 24-hour window at `30s` has a ceiling near 2 880), so it is a sanity cap rather than a correctness bound, and it does not exclude anything real. |
| `group_close_delay_s` min `60` | **Kept.** Below `group_interval` at `5m`, but correct at `30s`. Same reasoning as the flap floor. |

### The guidance moved with the numbers

`docs/setup/tuning.md` now derives every one of these from the two tables in §1 with their sources,
instead of from a plausible guess, and carries the corpus itself so the next person can check the
arithmetic rather than trust it. The settings screen's `ASSUMED_RULE_FOR_S` moved from `300` to `900`
— it previously asserted *"five minutes is the commonest `for:` in the wild"*, which the corpus
falsifies — and both flap formulas gained the missing transport term. The `refire_grace` verdict
previously compared the value only against `group_interval`, which is the smaller floor for 82 % of
real rules, and so reported the old default as comfortably fine.

## Consequences

- **A brand-new org is quieter and its threads are stickier.** A recurring problem that used to
  produce a new Slack root card on every re-fire now reopens one thread for up to twenty minutes.
- **Flap damping engages for the first time.** It could not previously engage at all at
  `group_interval: 5m`. Installs will now see the "flapping" state and the digest behaviour, which
  they have never seen, and some will read it as a regression. It is the feature working.
- **A group's Slack thread stays live for 20 minutes after everything resolves**, so a new alert in
  that group joins the existing card rather than opening a new one. On a coarse `group_by` such as
  the ecosystem's `[namespace]`, a busy namespace's generation may rarely close and its thread may
  grow long. That risk existed at `300s` too for any namespace busier than one alert per five minutes;
  this widens it. If long threads become the complaint, `group_close_delay` is the knob, and lowering
  it costs exactly the re-fire reuse §4 describes.
- **Existing orgs with an override are untouched** — an override is stored, and `Origin` reports it as
  `org`. Only orgs on `default` move, which is the intended blast radius and is why origin exists.
- **oto's own dev compose is unaffected in kind but not in degree.** At `group_interval: 30s` the
  transport floor is irrelevant and the rules bind, so a 2-hour flap window is generous there; a
  developer chasing flap behaviour locally should lower it.

## How to overturn this

**This was decided without the owner, on the two tables in §1 and nothing else.** No customer was
asked and no production deployment was measured. The corpus is what a large share of clusters *ship*,
which is not the same as what they *run* — anyone who has overridden `for:` on their alerts, or who
runs a `group_interval` other than `5m`, is outside it. **If the owner has one real customer's
`alertmanager.yml` and rule files, that beats all of it**, and the arithmetic in
`docs/setup/tuning.md` is written to be re-run against them in about ten minutes.

Reversal is cheap, in ascending order of cost:

- **The numbers** are Go constants in `platform/tuning/defaults.go` and are written nowhere else.
  Changing them changes nothing else, and no migration is involved because a default is not stored.

  ⛔ **They were mirrored, and this ADR is why they are not any more.** The five constants lived in
  `identity/domain/org.go` and were COPIED into `alerts/domain/lifecycle.go`,
  `grouping/domain/damping.go` and `alerts/service/deps.go` — the last as bare literals — because
  CONTEXT.md §5.4 forbids those packages importing identity. This change moved three of them at
  once and two of the copies were missed; only `defaults_derivation_test.go` caught it, and a test
  that is the only guard is not a guard. The numbers now live in a constants-only leaf under
  `platform`, which every module may import and which may import no domain (CONTEXT.md §5.2c), and
  every other declaration is a reference the compiler resolves. A miss here is not loud: the copies
  are the fallback used when an org's settings fail to load, so a stale one means exactly the tenant
  already having a bad day runs the old arithmetic and is told nothing.
- **The derivation** is one test. It recomputes each default from the corpus constants, so changing a
  default without changing the reasoning fails the build rather than drifting quietly.
- **The corpus** is two tables in `docs/setup/tuning.md` and the constants at the top of that test.
  Replacing them with a customer's real numbers is a documentation change plus a constant.

What is **not** cheap to reverse is the direction of the flap change: an install that has been running
with flap damping unreachable has never seen the damper engage, and turning it on is a visible change
in behaviour that operators will notice. Turning it back off is a one-line setting, but the surprise
has already happened. That asymmetry is the argument for having done the arithmetic before the first
customer rather than after.

## Alternatives rejected

**Leave the defaults and document the arithmetic better.** Rejected outright: the defaults are what
almost every installation runs, so a wrong default is the product's actual behaviour. A page
explaining that the shipped default cannot work is not documentation, it is an admission.

**`refire_grace: 900` (3 × `group_interval`), which the page already recommended.** The most tempting
option, because it required changing nothing but a number. Rejected on the corpus: `900` still cannot
be reached by the modal `for: 15m` rule, whose earliest observable re-fire is 15 minutes and whose
typical one is 20. It fixes a quarter of the problem and looks like it fixed all of it.

**`refire_grace` large enough to cover every rule in the corpus (~4000s, for the `for: 1h` tail).**
Rejected on §5: it optimises for the 7.7 % of rules that need it by merging genuinely separate
incidents for the 76 % that do not, and it moves oto toward the *quiet* error, which is the wrong
direction for an alerting product.

**`flap_window: 10800` (3h), which the page's worked example already recommended.** Works, and
rejected as one guess replacing another: `7200` is what `flap_threshold × cycle` actually produces at
the mode. A number that falls out of the arithmetic can be re-derived by the next reader; a number
that merely works cannot.

**Lower `flap_threshold` instead of widening the window.** Rejected on the page's own standing
reasoning and on the bound that already encodes it: at `group_interval: 5m` a 30-minute window's
ceiling for the modal rule is 2, so the threshold would have to go to 1 — below the floor of 3, and
squarely in "one rolling deploy is now labelled as flapping" territory.

**Make `group_close_delay ≥ refire_grace` a server-side cross-key bound.** Rejected: a partial `PATCH`
legitimately arrives one key at a time, so the bound would reject a correct pair merely for arriving
in the wrong order, and the settings API's whole design is that omitting a key leaves it alone. The
screen warns, which is where the operator is.

**Fix the `firing_only` / `refired` verbosity contradiction here.** Rejected as scope: it changes what
a documented verbosity level means, it touches SPEC §H.6, ADR 0020, the OpenAPI schema and the
renderer, and it is a product decision about whether a channel may opt out of a transition ADR 0020
called unmissable. Recorded in §5.3 and in `docs/setup/tuning.md` instead, so it is visible rather
than discovered during an incident.

**Read the rules' `for:` off Prometheus instead of assuming it.** The genuinely correct fix, and out
of scope: oto already fetches rule text for snapshots, so a per-alert `for:` is reachable, and a
per-alert flap ceiling would end the need for a global assumption entirely. It is a feature, not a
default, and it does not remove the need for a shipped default for orgs whose sources are unreachable.
