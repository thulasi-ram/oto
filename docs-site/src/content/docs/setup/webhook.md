---
title: The generic webhook and `oto.notification.v1`
---
The generic webhook is how anything that is not Slack hears from oto: an incident tool's alert
source, a bridge you run yourself, a log pipeline. oto POSTs one JSON document per message, the
**`oto.notification.v1` envelope**, and that envelope is the contract every integration builds on
([ADR 0055](/oto/adr/0055-an-incident-tool-integration-is-data-or-a-bridge-never-code-in-oto/) §1).
This page is the contract written down: what is sent, how to verify it came from oto, and what oto
promises not to change. Whether a tool wants this envelope as it is, a payload mapping, or a bridge
you run is [incident-tools.md](/oto/setup/incident-tools/).

---

## 1. Set it up

A webhook destination is two things, the same split Slack has
([ADR 0047](/oto/adr/0047-a-channel-answers-to-a-connection/)):

- A **Connection** — Settings → Connections → *Add a connection* → *Webhook*. It holds the secrets
  shared by every URL behind one receiver, in two independent slots:
  - **Authentication** (optional): a `bearer` token, sent as `Authorization: Bearer <token>`, or
    `basic` credentials. This gets oto *into* the receiver.
  - **Signing secret** (optional): oto signs every request with it, so the receiver can prove the
    request came from oto ([section 4](#4-verify-that-a-request-came-from-oto)).

  A connection may carry either, both, or neither. A receiver that requires a bearer token **and**
  verifies a signature is configured with one connection.
- A **Channel** — created from the notification policy screen, under that connection. Its config is
  the destination itself:

  | Key | Default | Meaning |
  |---|---|---|
  | `url` | — | `http://` or `https://`, at most 2048 characters. Private, loopback and link-local targets are refused unless the deployment sets `OTO_ALLOW_PRIVATE_WEBHOOK_TARGETS`. |
  | `method` | `POST` | `POST` or `PUT`. |
  | `headers` | `{}` | Up to 20 static headers. `Authorization` is refused (it belongs in the connection, sealed), and so is **every `X-Oto-*` name**: those are oto's own framing, below. |
  | `timeout_ms` | `5000` | 100–30000. |

oto never follows a redirect: a 3xx is a failed delivery, because a redirect is how an allowed target
becomes a forbidden one after the address check has passed.

---

## 2. What a request looks like

```http
POST /your/path HTTP/1.1
Content-Type: application/json
Accept: application/json
User-Agent: oto/1
X-Oto-Delivery-Id: 0199a1b2-7f00-7c3e-9d41-5be0c8a1d2e4
X-Oto-Content-Hash: 9f2c…
X-Oto-Timestamp: 1759400000
X-Oto-Signature: v1=e8ae2236d80ddab5f6b8da2c535675fede395d66a342cd33e5eeee61588bcb22
Authorization: Bearer …

{ "schema": "oto.notification.v1", … }
```

| Header | Present | Meaning |
|---|---|---|
| `X-Oto-Delivery-Id` | always on a delivery | oto's id for this delivery. **Stable across retries**: oto's queue is at-least-once, so the same message can arrive twice, and this is the key to drop the second one on. |
| `X-Oto-Content-Hash` | always | SHA-256 of the body, hex. Two deliveries with the same hash carry the same bytes. |
| `X-Oto-Timestamp` | when the connection signs | The send instant of **this attempt**, Unix seconds. A retry gets a new one. Covered by the signature. |
| `X-Oto-Signature` | when the connection signs | One `v1=<hex>` entry per signing secret in use, comma-separated. Two entries appear only during a rotation's overlap. |

An unsigned connection sends **neither** `X-Oto-Timestamp` nor `X-Oto-Signature` — never an empty
one.

> ⚠️ **BREAKING, once: the signature header changed shape.** Before this release a signed
> connection sent `X-Oto-Signature: sha256=<hex>`, an HMAC-SHA256 of the **body alone**, with no
> timestamp. That header was never documented, and it is gone: oto now sends `X-Oto-Timestamp` and
> `X-Oto-Signature: v1=<hex>` over `"v1:" + timestamp + ":" + body`, as
> [section 4](#4-verify-that-a-request-came-from-oto) describes, and never sends `sha256=` again. A
> receiver written against the old header rejects every signed request until it verifies `v1=`
> instead. This is the one change of its kind: from here the format moves only as
> [section 6](#6-the-compatibility-promise) promises
> ([ADR 0055](/oto/adr/0055-an-incident-tool-integration-is-data-or-a-bridge-never-code-in-oto/) §1).

---

## 3. The envelope

Every body is one JSON object. `schema` is always `"oto.notification.v1"`; check it, and you know
which fields below you can rely on.

### Top-level keys

| Key | Type | Meaning |
|---|---|---|
| `schema` | string | `"oto.notification.v1"`. |
| `reason` | string | **Why oto is speaking** — the fact this message reports. See the two tables below. |
| `mode` | string | How oto placed the message (`post_root`, …). The webhook keeps no thread, so treat it as informational. |
| `continued` | bool, optional | `true` when this message replaces one the destination already had for the same conversation (a recovery), rather than opening it. |
| `delivered_at` | RFC 3339 UTC | When oto rendered the message. |
| `org` | object | `id`, `slug`, `name` of the tenant. |
| `summary` | string | One human sentence. For display and logs — **do not parse it**. |
| `alerts` | array | The Alerts the message is about; `[]` on a digest and on every Incident fact. |
| `incident` | object, optional | Present on exactly the six Incident facts. See below. |
| `group` | object, optional | The conversation a Case fact belongs to: title, receiver, labels, `state`, counts. Absent on a digest and on an Incident fact. |
| `digest` | object, optional | A periodic summary: `count` and the half-open span `[covered_from, covered_to)`, and — when the policy names a digest Investigator whose run had ended with a Finding by the time the window closed — `finding`, in the same shape as `incident.finding` (`investigation_id`, `investigator`, `version`, `summary`, `classification`, `partial`, `concluded_at`). `finding` is **absent** for the built-in body. It is copied at the send and never amended: a digest never waits for a Finding and is never re-sent with a later one. |
| `occurrence` | object, optional | The Case (one firing episode) a Case fact is about: `id`, `state`, `ack_state`, `started_at`, `ended_at`, … The key keeps its v1 spelling. |
| `focus` | object, optional | The one Alert a Case fact is about, in `alerts[]` shape. |
| `rule`, `rule_change` | object, optional | What the alerting rule said, and what changed in it. |
| `enrichments` | object, optional | Results of oto's Enrichers, keyed by enricher. |
| `actor` | object, optional | Who caused a human-caused fact (`kind`, `id`, `label`). |
| `comment` | string, optional | What they wrote. |
| `actions` | array, optional | Card actions, as links (`id`, `label`, `url`). |
| `links` | object, optional | Deep links back into oto. |
| `previous` | object, optional | The state the conversation showed before this fact. |
| `rendered` | string, optional | The operator's own wording, when the routing policy names a template. |

**Case facts** — `reason` is one of `fired`, `all_resolved`, `repeat`, `suppressed`,
`unsuppressed`, `expired`, `refired`, `acked`, `unacked`, `snoozed`, `unsnoozed`, `enriched`,
`rule_changed`, `comment`, or `digest` for a periodic summary.

### `occurrence.resolve_reason` — how a Case ended

On a closed Case, `occurrence.resolve_reason` says **why it ended**
([ADR 0056](/oto/adr/0056-a-case-the-upstream-stopped-speaking-about-expires-and-says-why/) §4). It
is absent while the Case is open.

| Value | Reads as | The fact |
|---|---|---|
| `upstream` | **resolved** | An explicit `status="resolved"` arrived from upstream. The only resolution. |
| `timeout` | expired | Upstream's `endsAt` plus the org's resolve grace passed with no word, while every live source on the Case's cluster was healthy. |
| `silent` | expired | Every live source on the cluster was healthy and none said anything about the Case for longer than the cluster's max silence. |
| `source_removed` | expired | No live source has fed the Case's cluster for a resolve grace after its last one was deleted, so nothing is left that could say it ended. |

**Anything but `upstream` means expired** — oto stopped hearing about the signal, which is not the
same as the signal recovering. Treat it that way in your receiver: if you close something on a
resolve, close it on `upstream` only, or say *expired* when you close it on the others.

⚠️ **The set widened within `v1`.** It was `upstream` and `timeout`; `silent` and `source_removed`
were added under the additive-change rule of [section 6](#6-the-compatibility-promise), and a
deployment emits them only once its operator turns them on (`jobs.expire_silent_and_removed`,
[configuration](/oto/setup/configuration/)). A receiver written against the two-value set must not reject a
value it does not know — **read any value other than `upstream` as expired**, and it stays correct
when the set grows again.

### Incident facts

An Incident is a set of one or more Cases drawn as one story
([ADR 0052](/oto/adr/0052-an-incident-is-drawn-over-cases-and-its-response-is-handed-off/)). An
Incident fact carries `incident` and no `group`, `digest`, `occurrence` or `focus`; `reason` is one
of exactly twelve:

| `reason` | The fact |
|---|---|
| `drawn` | The Incident was drawn — by a human, or by a Correlator an operator wrote. |
| `case_added` | A Case joined it. |
| `case_removed` | A Case left it (removed, or moved to another Incident). |
| `quiet` | Every current member Case has closed. |
| `active_again` | A member Case is open again after the Incident was quiet. |
| `finding` | An Investigation of the Incident as a whole reached a new Finding (ADR 0053 §4), carried in `incident.finding`. |
| `remedy_proposed` | An Investigator proposed a **Remedy** — a change to the cluster — with its Finding (ADR 0054), carried in `incident.remedy`. |
| `remedy_approved` | A Remedy got the approvals it needs: one or two different holders of the grant on its write ToolServer, as the org's risk rules set when it was proposed. |
| `remedy_declined` | A person in oto said no to a Remedy. |
| `remedy_expired` | A Remedy's approval window passed before it was approved, or before it was executed. |
| `remedy_executed` | oto sent a Remedy's exact arguments to its write Tool, and the Tool did not report a failure. |
| `remedy_failed` | A Remedy was not executed, its write Tool reported a failure, or what happened is not known. It is never retried. |

⛔ **These are facts, never commands.** None of them means resolve, close or acknowledge, and no
envelope oto sends carries a status for your tool to adopt. **`quiet` is not `fixed`**: it says the
signals stopped, not that the response is over. If you want your incident tool to resolve on
`quiet`, that is a rule *you* write in your tool or bridge, and it ends a response the moment the
signals go quiet — including the times they went quiet because the thing that was emitting them
died.

The six `remedy_*` facts are the same: approval happens **in oto**, and oto reads nothing back.
Someone saying no in your incident tool is a fact oto cannot hear — say it in oto, where
**Decline** is. A Remedy on a Case that is in no Incident is declared nowhere. See
[Remedies](/oto/setup/remedies/).

The `incident` object:

| Key | Meaning |
|---|---|
| `id` | The Incident's id. **Stable for the Incident's whole life — this is your de-duplication key.** Key your external incident on it: every later fact about the same Incident carries the same `id`, so a receiver that upserts on it gets one external incident whose later facts are updates, not new incidents. A tool that opens a **new** incident for an event on a key whose incident was resolved — PagerDuty and incident.io do — is the exception: see §7 for routing it only `drawn` and `active_again`. |
| `number` | The number humans quote (`#12`). Unique per org; prefer `id` as a key. |
| `sequence` | Where **this fact** falls in the Incident's story: `1` for `drawn`, and higher for every fact after it. The same on every retry of the fact. **Use it to order facts that arrive out of order** — see below. |
| `state` | `active` (some current member Case is open) or `quiet` (none is). Derived by oto from the Cases; nobody sets it. |
| `drawn_at` | When it was drawn. |
| `drawn_by` | `{kind: "human", label}` or `{kind: "correlator", correlator_id}`. |
| `members` | Every spell of every Case that has been in it, current and removed, in the order they joined. |
| `link` | oto's own page for it, when oto has a public URL configured. |
| `remedy` | On the six `remedy_*` facts only: the transition. ⭐ **The exact command first** — `tool` is `{tool_server, tool, arguments, arguments_sha256}`, where `arguments` is the JSON object the write Tool would be (or was) sent, byte for byte; or `no_tool` says *"no configured Tool can carry this out"*, and there is no `tool`. Then `target`, the Investigator's `description`, `proposed_by`, `required_approvals` and what set it — `approvals_set_by` (`rule`, `no_rule`, `unparseable`, `risk_model`, `risk_model_failed`, `risk_model_budget`) and `approvals_rule` (the rule's name), both absent on a Remedy that names no Tool — `approvals` (`label`, `approved_at`), the `state` reached and the one it came `from`, the `actor` (`kind` is `investigator`, `user` or `system`, with a `label`), `at`, `expires_at`, and — when it failed or expired — `failure_reason` and `detail`. A snapshot of the transition, never re-read. |
| `finding` | The latest Finding an Investigation of the Incident reached, absent until one has: `investigation_id`, `investigator`, `version`, `summary`, `classification`, `partial` (a budget stopped the run first) and `concluded_at`. It is what a model concluded **at `concluded_at`**, never live state — and paging on it is paging on a model's judgement. It is on every Incident fact once one exists; `finding` is the fact that says a new one arrived. `classification` is one of **your** classes (Settings → Classification) or `unclassified`, and is absent when your org has written no classes — see *Classification*, below. |

Each member: `case_id`, `case_number`, `case_state` (`open`/`closed`), `alert_id`, `alert_name`,
`labels`, `added_at`, `added_by`, and on a removed spell `removed_at`, `removed_by_label` and — when
it was moved — `moved_to_number`; `link` to the Case.

**Ordering facts: keep the highest `sequence`, drop anything below it.** Each fact is its own
delivery with its own retries, so two facts about one Incident can reach you in the wrong order: a
`quiet` can land before the `case_removed` that caused it, because the removal's first attempt hit
a `503` and was retried. `delivered_at` cannot help — it is when oto *sent* the request, which is
exactly what a retry reorders. `sequence` is the order the facts **happened** in, numbered in the
same database transaction that recorded each one, so a receiver should:

- remember, per `incident.id`, the highest `sequence` it has applied;
- **drop a fact whose `sequence` is lower** than that — a newer fact has already been applied, and
  its envelope already describes the Incident as it is (`state` and `members` are read when each
  request is built, so the later fact's are the more recent);
- treat a fact with the **same** `sequence` as a redelivery of one it has seen: a retry carries the
  number the first attempt did.

The numbers increase but are **not gapless**: a fact your policy routes nowhere — or to another
destination — still takes its number, so you may see `1, 2, 4`. A gap is not a lost fact. The key is
**absent** on a fact declared before your oto had this field; treat such a fact as unordered. A
**test send** — a channel's test, or **Test the mapping** (§7) — always carries `1`, whichever fact
you pick, and names a new test Incident each time, so a test never makes a receiver drop a real fact
— nor a second test as a replay of the first.

**Severity.** oto holds none for an Incident and invents none. If your tool needs one, map it from
the member alerts' own labels (`members[].labels.severity`, typically) in your receiver or mapping.
For an Incident whose labels give none, the value is yours to choose, not oto's: a catalog starter
that needs one — PagerDuty's — asks you to pick it when you import it (§7), and the copy holds your
pick.

A full `case_added` envelope is checked in at
[`internal/channels/render/webhookjson/testdata/incident_case_added.golden.json`](../../internal/channels/render/webhookjson/testdata/incident_case_added.golden.json).

### Classification — paging on one is paging on a model's judgement

A Finding may carry a **classification**: the class an Investigator put it in, chosen from a closed
set **your operator wrote** (Settings → Classification, or `PUT /api/v1/investigation-classes`), or
`unclassified` — always allowed, and the model's answer whenever none of your classes clearly fits or
it is unsure. oto ships **no** classes: until you write some, no Finding is classified and the key is
absent. A Finding keeps the class it was given even if you later rename or remove that class.

It travels outbound — as `incident.finding.classification` on every Incident fact, as
`digest.finding.classification` on a digest that carried a Finding, and as
`enrichments["investigator.<name>"].payload.classification` on a Case's facts once its Finding is
published — so a receiver can route on it. Only the Finding and its classification go outbound; an
Investigation's Steps (its transcript) stay in oto, readable through
`GET /api/v1/investigations/{investigation_id}`.

> ⚠️ **Paging on a classification is paging on a model's judgement.** A class is what a model
> concluded from what it could read at `concluded_at`; it can be wrong, and `unclassified` means
> "it would not say", not "it is fine". oto itself never decides whether anyone is told on the
> strength of a Finding or its class. If your receiver pages on one, that is your receiver's
> decision, and it inherits the model's error rate.

---

## 4. Verify that a request came from oto

Give the connection a signing secret (Settings → Connections → the connection → *Signing secret*)
and put the same secret in your receiver. Then, for every request:

1. Read `X-Oto-Timestamp`. **Reject the request if it is more than 5 minutes from your clock**, in
   either direction. This is what stops a captured request being replayed later.
2. Compute `HMAC-SHA256(secret, "v1:" + timestamp + ":" + body)` and hex-encode it (lowercase).
   `body` is the **raw request bytes**, exactly as received — not re-serialised JSON.
3. Split `X-Oto-Signature` on `,`, trim each entry, and keep those that start `v1=`. **Accept the
   request if any one of them equals your value**, compared in constant time. Ignore entries with
   any other scheme: a future scheme is added beside `v1=`, never in place of it.

Both the timestamp and the body are under the signature, so editing either breaks it.

### Worked example

| | |
|---|---|
| secret | `oto-example-signing-secret` |
| `X-Oto-Timestamp` | `1759400000` |
| body | `{"schema":"oto.notification.v1","reason":"drawn"}` |
| signed base | `v1:1759400000:{"schema":"oto.notification.v1","reason":"drawn"}` |
| `X-Oto-Signature` | `v1=e8ae2236d80ddab5f6b8da2c535675fede395d66a342cd33e5eeee61588bcb22` |

```python
import hashlib, hmac, time

def verify(secret: bytes, headers, body: bytes, tolerance=300) -> bool:
    ts = headers["X-Oto-Timestamp"]
    if abs(time.time() - int(ts)) > tolerance:
        return False
    want = hmac.new(secret, b"v1:" + ts.encode() + b":" + body, hashlib.sha256).hexdigest()
    for entry in headers.get("X-Oto-Signature", "").split(","):
        scheme, _, got = entry.strip().partition("=")
        if scheme == "v1" and hmac.compare_digest(got, want):
            return True
    return False
```

This example is pinned by a test against oto's own signing code
(`internal/channels/providers/webhook/signature_test.go`), so the page and the code cannot drift.

### Rotating the secret

Set a new signing secret on the connection. From that moment:

- the **new** secret signs every request, and
- the **previous** secret keeps signing beside it **for 24 hours** — every request in that window
  carries two `v1=` entries, the new secret's first.

So a receiver still holding the old secret keeps verifying while you roll the new one out, and a
receiver that already has the new one verifies too. After 24 hours only the new secret signs. The
connection shows when the overlap ends. Rotating again inside the window retires the oldest secret
at once: there are never more than two signatures. Removing the signing secret stops signing
immediately, with no overlap.

---

## 5. What oto does with your response

| Your response | oto records | Retried? |
|---|---|---|
| `2xx` | `sent` | — |
| `429` | `rate_limited` | Yes, after `Retry-After` (capped at one hour). |
| `401`, `403` | `auth_expired`; the channel shows `auth_failed` | **No.** Fix the credential. |
| `408`, `5xx` | `retryable` | Yes, with backoff. |
| any other `4xx` | `permanent` | No. |
| `3xx`, timeout, connection error | retryable (a redirect is never followed) | Yes. |

oto keeps the status code, the body's **size** and the round-trip time. It never stores, logs or
displays the bytes of your response body: the webhook URL is operator-supplied and dialled from
inside the operator's network, and handing a response back through oto's API would make every
webhook a way to read an internal page. There is exactly one exception, below.

### Echoing your incident back

If your receiver opens (or finds) an incident in your tool for an **Incident fact**, it may say so
in its response, and oto will link to it from the Incident's Slack card and its page in oto:

```http
HTTP/1.1 201 Created
Content-Type: application/json

{"external_url": "https://tool.example/incidents/42", "external_id": "INC-42"}
```

- Only a **2xx** response is read, only its **top-level** `external_url` and `external_id` — or,
  when the connection's payload mapping names a `response` path (§7), **only** that path — and only
  on a delivery of one of the five Incident facts. Either key may be absent.
- `external_url` is kept only if it is an absolute `https://` URL of at most 2048 characters (no
  credentials in it, no spaces); `external_id` only if it is a JSON string of at most 255 printable
  characters. A value that fails is treated as absent.
- **Once per Incident per channel.** The first valid answer is kept; a retry of the same delivery,
  or a later fact on the same Incident answered with a different value, changes nothing. Key your
  receiver on `incident.id` and answer with the same incident every time.
- Nothing else in the response is read, the values are never sent back to any receiver, and oto
  never asks your tool about the incident again. It is a receipt — where the response is being
  handled — not the incident's state.
- A response without the keys, a non-JSON response, or a value that fails the checks is **not an
  error**: the delivery is `sent`, exactly as it would have been.

The link appears on the Incident's page at once. On the Slack card it appears the next time the
card is updated — the next fact about the Incident or one of its Cases — because recording the
receipt is not itself a fact oto posts about.

PagerDuty's Events API and incident.io's HTTP alert source both answer with a dedup key and open the
incident asynchronously, so they echo nothing; a small bridge you run in front of them
([ADR 0055](/oto/adr/0055-an-incident-tool-integration-is-data-or-a-bridge-never-code-in-oto/) §3,
and [incident-tools.md §3](/oto/setup/incident-tools/#3-the-bridge-pattern) for the pattern) can look the
incident up and return it.

---

## 6. The compatibility promise

`oto.notification.v1` is a public API. Within `v1`:

- **Additive changes only.** A new key may appear — an optional field, a new `reason` — and a
  receiver must ignore keys it does not know. Nothing a receiver already reads is moved, renamed,
  dropped or given a different meaning.
- **A removal or a change of meaning is `v2`, sent side by side for a stated period.** When `v2`
  ships, oto sends both envelopes, `v1` and `v2`, **for 180 days**, and announces the end date in the
  release notes before `v1` stops.
- The same holds for the headers in [section 2](#2-what-a-request-looks-like): the `v1=` signature
  scheme and its base string do not change; a new scheme is a new `v2=` entry sent beside `v1=` for
  the same 180 days.

What is *not* part of the promise: the wording of `summary` and `rendered`, the order of keys, and
whitespace. Parse the JSON; never match on its text.

---

## 7. Reaching a tool that wants a different shape: the payload mapping

A tool whose API wants its own JSON — incident.io's HTTP alert source requires top-level `title`
and `status` — is reached without a bridge by putting a **payload mapping** on the webhook
**connection** (Settings → Connections, [ADR 0055](/oto/adr/0055-an-incident-tool-integration-is-data-or-a-bridge-never-code-in-oto/)
§2). Without one, the plain envelope above is sent, byte for byte.

```json
{
  "body": "{\"title\": \"{{ summary }}\", \"status\": \"firing\", \"deduplication_key\": \"{{ incident.id }}\"}",
  "facts": {
    "quiet": "{\"title\": \"{{ summary }}\", \"status\": \"firing\", \"deduplication_key\": \"{{ incident.id }}\", \"description\": \"Every member Case has closed. Quiet is not fixed.\"}"
  },
  "headers": { "X-Source": "oto" },
  "response": { "external_url": "data.incident.url", "external_id": "data.incident.id" }
}
```

- **`body`** (required) is Liquid over the envelope of §3 — `{{ summary }}`, `{{ incident.id }}`,
  `{% for a in alerts %}…{% endfor %}` — and must render **one JSON object**. **`facts`** overrides it
  for the facts it names (the envelope's `reason`). A mapping cannot decline a fact: every one renders.
  An Incident fact's order is `{{ incident.sequence }}` (§3); it is digits, so it may stand outside
  quotes as a JSON number, written `{{ incident.sequence | default: 0 }}` so that a Case fact — which
  has no `incident` — still renders JSON.
- **Every interpolated value is JSON-escaped, with no opt-out**, so write it inside quotes. A label
  holding `"`, `\`, a newline or `</script>` lands as that string, never as structure.
- **`headers`** are Liquid too. `Authorization` and every `X-Oto-*` name are refused: a vendor's
  token is the connection's `credential`, and `X-Oto-*` is oto's framing, which still goes on every
  request — the signature (§4) covers the mapped body.
- **`response`** names, as [gjson](https://github.com/tidwall/gjson/blob/master/SYNTAX.md) paths,
  where a 2xx answer carries the incident's link and id. When present it **replaces** the top-level
  keys of §5; the values pass the same checks.
- **A mapping never holds a secret.** A key that must travel in the body (PagerDuty's `routing_key`)
  is a **mapping secret** sealed on the connection and written as `{{ secrets.routing_key }}`; oto
  fills it in as the request leaves. The stored mapping, the delivery record and the catalog file
  hold only the name. Mapping secrets are write-only and are replaced as a set.
- **The mapping itself is not secret.** `payload_mapping` is returned by every read of the
  connection, to **every member of the org**, and copied into every delivery record. So a header
  whose name reads like a credential — anything containing `key`, `token`, `secret`, `auth` or
  `password` — must take its value from `{{ secrets.<name> }}`; a mapping that writes one as text is
  refused at save (`secret_required`).
- **Write-only means unreadable, not unredirectable.** No endpoint returns a credential or a mapping
  secret. But oto fills them into whatever request the connection sends, to whatever URL its
  channels point at — so **any org member who can change a channel's URL or the connection's
  mapping can send those secrets somewhere else**, and read them there. Who may edit a connection or
  its channels is not yet narrower than "a member of the org": role-based access is deferred
  ([SPEC](/oto/design/spec/) R2 — "every authenticated principal has full access to its own org"). Until it lands, treat edit access to an org's
  connections as access to their secrets.
- **It is checked before it is saved**: rendered against an envelope for every fact, including
  hostile and empty ones; a mapping that does not render, or names a secret the connection does not
  hold, is refused with the fact named. **Test the mapping** on the connection sends one fact you
  choose through one of its channels — **and may open a real incident in the tool.**
- **A mapping that fails when sending is a failed delivery** — `dead`, `config_invalid`, with the
  attempt on the record, retryable from the delivery audit once fixed. oto never sends the plain
  envelope in its place: the tool could not parse it, and a missing incident is worse than a visible
  failure.
- **oto's own code sends no command.** A mapping that turns `quiet` into a resolve is a rule you
  wrote; `quiet` is not `fixed` (§3), and resolving on it ends a response the moment the signals stop
  — [incident-tools.md §2](/oto/setup/incident-tools/#2-quiet-is-not-fixed) says what that costs. The example
  above keeps the alert firing on `quiet` and only says so.
- Add a mapping **after** upgrading every oto pod: a pod older than the mapping sends the plain
  envelope.

### Starting from the catalog

oto ships a small **catalog** of mappings, one file per tool, in
[`mappings/`](../../mappings/README.md) — today incident.io's HTTP alert source and PagerDuty's
Events API v2, each with the vendor docs its field names were checked against and the date. On a
webhook connection, *Import from the catalog* shows what a mapping does, what it **never** sends,
and the setup it needs, and **copies** it into the mapping editor. Save stores the copy as the
connection's own, through the same check as above; a later catalog change never touches it. The
catalog holds no secret: a mapping that reads `{{ secrets.routing_key }}` is saved only once you add
that mapping secret beside it.

A starter may also leave a value to you. PagerDuty's needs a severity even when the Incident's
labels give none (no `severity` label, or one that is not `critical`, `error`, `warning` or
`info`), and oto invents none: the import asks you to pick a **default severity** from those four,
and writes your pick into the copy as plain text. A label that is one of the four still passes
through. The copy then holds no question — a mapping that still holds the catalog's
`<<choose:…>>` placeholder is refused at save — and the catalog check refuses a starter that answers
such a question itself.

**Route only `drawn` and `active_again` to a starter's channel.** Both starters key the tool's alert
on `incident.id` and send every fact as a further trigger, never a resolve. Once a human has
resolved the incident in the tool, a later trigger on that key — a `case_added`, a `case_removed`, a
`quiet` — **opens a new incident and pages again**, for a response somebody has just ended. A
mapping cannot decline a fact, so the fix is the policy: add a notification policy for the
channel whose reasons are **only** `drawn` and `active_again` (policies already filter by reason).
The tool then hears when an Incident needs a response and when it needs one again, and nothing in
between.

No catalog mapping turns a fact into a resolve, close, acknowledge or status change — a test holds
every file to that, for every fact. Edit the copy into one if you choose; it is then your rule.
