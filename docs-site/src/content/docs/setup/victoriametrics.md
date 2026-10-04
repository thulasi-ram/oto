---
title: Connecting oto to VictoriaMetrics (vmalert)
---
oto works with a VictoriaMetrics stack the way it works with Prometheus. **Alertmanager stays
in the middle, and you pair vmalert as the source's rule source.** That is the whole setup.
The rest of this page explains why each piece is needed, and which vmalert flags decide how
much of each rule oto can record.

Every vmalert flag named here was checked against the
[vmalert documentation](https://docs.victoriametrics.com/victoriametrics/vmalert/) and its
source (`app/vmalert/web.go`, `app/vmalert/rule/web.go`).

---

## 1. vmalert → Alertmanager → oto. There is no direct path.

```
vmalert ── -notifier.url ──▶ Alertmanager ── webhook receiver ──▶ oto
   ▲                              ▲
   │ GET /api/v1/rules            │ GET /api/v2/alerts (the reconciler)
   └──────────── oto ─────────────┘
```

Point vmalert's notifier at your Alertmanager, and Alertmanager's webhook receiver at the
source's ingest URL with its ingest token, exactly as you would for Prometheus:

```bash
vmalert \
  -datasource.url=http://victoriametrics:8428 \
  -notifier.url=http://alertmanager:9093 \
  -rule=/etc/vmalert/rules/*.yml
```

Do **not** point `-notifier.url` at oto. Two things rule it out:

- vmalert's notifier speaks Alertmanager's alert-posting API. oto accepts only Alertmanager's
  **webhook** payload, on `POST /api/v1/ingest/alertmanager/{source_id}`, so a vmalert posting
  directly would be refused.
- **The reconciler needs an Alertmanager.** Alertmanager drops a silenced or inhibited alert
  before its webhook, so a webhook never reports suppression. oto reads suppression from
  `GET /api/v2/alerts` on the source's Alertmanager instead
  ([ADR 0006](/oto/adr/0006-reconciler-is-mandatory/)). Without an Alertmanager, a silenced
  alert looks exactly like a resolved one, and oto would record the wrong one.

The source's **base URL** is the Alertmanager, as it is for Prometheus.

## 2. Set the source's Prometheus URL to vmalert

The source's `prometheus_url` is where oto reads a rule's definition when it fires: `for`,
`keep_firing_for`, labels and annotations. That makes it a **rule source**, and in a
VictoriaMetrics stack the rule source is **vmalert**, not VictoriaMetrics:

```
prometheus_url = http://vmalert:8880
```

vmalert serves `GET /api/v1/rules` and honours the three filters oto sends (`type=alert`,
`rule_name[]`, `exclude_alerts`). If vmalert runs behind a path prefix, include the prefix.

Two vmalert differences oto handles, so you don't have to:

- **No `/api/v1/status/buildinfo`.** vmalert does not serve it (vmselect does). When buildinfo
  fails with an answer rather than an outage, the source health check asks the rules API
  instead. A valid answer counts as reachable, so source health shows the rule source as up
  **with no version**. That is expected, not a fault.
- **`keep_firing_for` is spelled differently.** vmalert's rules API writes `keep_firing_for`;
  Prometheus's writes `keepFiringFor`. oto reads both, so a rule snapshot from vmalert records
  the real value, and the same rule fingerprints the same on either server.

## 3. Send the expression in the alert's link: `-external.alert.source`

Each alert carries a source link (Alertmanager's `generatorURL`). oto recovers a rule
**from the link first** ([SPEC §F.4](/oto/design/spec/)). This costs no API call and can't
be ambiguous, and it is the only strategy that still works when the rules API is unreachable.
For that to work, the link has to contain the expression.

vmalert's **default** link holds no expression:

```
http://<vmalert-addr>/vmalert/alert?group_id=<group_id>&alert_id=<alert_id>
```

Even so, oto uses that link to find the vmalert that evaluated the rule and reads the rule from
its `/api/v1/rules`. But an alert with only this link has no expression to fall back on if that
lookup fails.

**We recommend** the vmui form from the vmalert docs, so the expression travels with the alert:

```bash
vmalert \
  -external.url=http://<vmui-addr> \
  -external.alert.source='vmui/#/?g0.expr={{.Expr|queryEscape}}' \
  ...
```

`-external.alert.source` is resolved relative to `-external.url`, so `<vmui-addr>` is the
VictoriaMetrics instance that serves the UI (single-node `:8428`, or your vmselect). The link
then reads `http://<vmui-addr>/vmui/#/?g0.expr=…`. oto reads the expression from after the
`#/?` route, keeping a `+` as a `+`.

One consequence you should know about: that link names the **VictoriaMetrics UI**, not the
vmalert. When oto looks a rule up, it tries the server the link names first. VictoriaMetrics
returns an empty rules list unless it proxies to vmalert (`-vmalert.proxyURL`). On an empty
answer, oto **falls back to the source's `prometheus_url`**. So you need to set section 2's
URL either way: a followed link can only add to what oto finds, never take away.

## 4. `-external.label` is part of every alert's identity

`-external.label=cluster=east-1` adds a label to every alert vmalert sends. oto identifies an
Alert by its **label set**, so:

- **Changing or adding an external label starts new Alerts.** The old label sets stop arriving
  and their Cases close; they are not renamed. Decide on external labels before you rely on
  oto's history.
- **Rule matching is unaffected.** oto scores a candidate rule by how many of the **rule's**
  labels the alert carries, and ignores labels the alert has beyond them. That one-way
  comparison is what makes Prometheus's `external_labels` work, and it works the same way for
  vmalert's.

## 5. VictoriaLogs rules (`type: vlogs`)

A rule group with `type: vlogs` evaluates **LogsQL** against VictoriaLogs. oto records those
rules like any other, with one difference in what you can expect from the history:

- **The expression is stored verbatim.** A rule snapshot is a capture of what the rules API
  returned, and oto doesn't parse the expression to store it. So LogsQL is recorded exactly
  as written, and drift between two snapshots is detected exactly as it is for PromQL.
- **The expression diff is lexical, not LogsQL-aware.** oto compares two expressions by
  scanning them for numbers (`internal/rules/domain/exprdiff.go`). It says "the threshold moved
  from X to Y" only when it is sure. Otherwise it shows both versions and says the expression
  changed. Expect the second answer more often for LogsQL than for PromQL, because the scanner
  refuses to guess at tokens it can't interpret, such as LogsQL's `_time:5m` windows.
- With the recommended `-external.alert.source`, a vlogs alert's link points at the
  VictoriaMetrics UI with a LogsQL expression in it. oto still reads the expression correctly,
  but the link opens the metrics UI, which won't run LogsQL. To get working links, give vlogs
  groups their own vmalert with a link to the VictoriaLogs UI.

## 6. Try it locally: `just vm`

The compose file carries this whole shape as an optional profile, `vm`: a VictoriaMetrics
single-node on `:8428` and a vmalert on `:8880` that evaluates
[`deploy/vmalert/rules.yml`](../../deploy/vmalert/rules.yml) and notifies the **same**
Alertmanager the compose Prometheus uses. Nothing starts it unless you ask:

```bash
just vm            # just infra, then VictoriaMetrics + vmalert
```

vmalert runs with `-external.url=http://localhost:8880` and the **default** alert link, so its
alerts arrive with the expression-less `/vmalert/alert?group_id=…&alert_id=…` link of section 3,
which oto follows back to this vmalert. Then:

1. Create a source whose base URL is the Alertmanager and whose Prometheus URL is vmalert, and
   keep its ingest token (it is shown once):

   ```bash
   curl -sS -X POST http://localhost:8080/api/v1/sources \
     -H "authorization: Bearer $OTO_PAT" -H 'content-type: application/json' \
     -d '{"cluster_id":"…","name":"dev-vmalert","kind":"alertmanager",
          "base_url":"http://localhost:9093","prometheus_url":"http://localhost:8880"}'
   ```

2. Point the Alertmanager at it: set `OTO_AM_LOCAL_WEBHOOK` (the source's ingest URL) and
   `OTO_AM_LOCAL_INGEST_TOKEN` in `.env`, then `just am-wire`. Alertmanager reads both files on
   every send, so it needs no restart.
3. **Test** the source: `prometheus_ok` is `true`, and source health shows the rule source up with
   no version (section 2).
4. Within a minute `VmStackSmokeTest` (`for: 15s`) is a Case in oto. `just vm-toggle 1` fires
   `VmDevToggle` after its 30 s `for`; its rule snapshot records `for_seconds: 30` and
   `keep_firing_for_seconds: 120`. `just vm-toggle 0` resolves it two minutes later, which is the
   `keep_firing_for` at work.

`just down` stops these containers with the rest; the profile only decides what `up` starts.

## Checklist

- [ ] vmalert `-notifier.url` points at Alertmanager, not at oto.
- [ ] Alertmanager's webhook receiver points at the source's ingest URL with its ingest token.
- [ ] The source's base URL is the Alertmanager; its `prometheus_url` is vmalert.
- [ ] `-external.url` + `-external.alert.source='vmui/#/?g0.expr={{.Expr|queryEscape}}'` are set,
      so alerts carry their expression.
- [ ] External labels are settled before you rely on the history.
- [ ] Source health shows the rule source reachable. A blank version is expected for vmalert.

## Related pages

- [configuration.md](/oto/setup/configuration/): oto's own settings.
- [ADR 0006](/oto/adr/0006-reconciler-is-mandatory/): why the reconciler, and therefore
  Alertmanager, is required.
- [ADR 0009](/oto/adr/0009-rule-snapshot-versioning-at-fire-time/): what a rule snapshot is.
