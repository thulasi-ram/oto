# The generic webhook and `oto.notification.v1`

The generic webhook is how anything that is not Slack hears from oto: an incident tool's alert
source, a bridge you run yourself, a log pipeline. oto POSTs one JSON document per message, the
**`oto.notification.v1` envelope**, and that envelope is the contract every integration builds on
([ADR 0055](../adr/0055-an-incident-tool-integration-is-data-or-a-bridge-never-code-in-oto.md) §1).
This page is the contract written down: what is sent, how to verify it came from oto, and what oto
promises not to change.

---

## 1. Set it up

A webhook destination is two things, the same split Slack has
([ADR 0047](../adr/0047-a-channel-answers-to-a-connection.md)):

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
| `incident` | object, optional | Present on exactly the five Incident facts. See below. |
| `group` | object, optional | The conversation a Case fact belongs to: title, receiver, labels, `state`, counts. Absent on a digest and on an Incident fact. |
| `digest` | object, optional | A periodic summary: `count` and the half-open span `[covered_from, covered_to)`. |
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

### Incident facts

An Incident is a set of one or more Cases drawn as one story
([ADR 0052](../adr/0052-an-incident-is-drawn-over-cases-and-its-response-is-handed-off.md)). An
Incident fact carries `incident` and no `group`, `digest`, `occurrence` or `focus`; `reason` is one
of exactly five:

| `reason` | The fact |
|---|---|
| `drawn` | The Incident was drawn — by a human, or by a Correlator an operator wrote. |
| `case_added` | A Case joined it. |
| `case_removed` | A Case left it (removed, or moved to another Incident). |
| `quiet` | Every current member Case has closed. |
| `active_again` | A member Case is open again after the Incident was quiet. |

⛔ **These are facts, never commands.** None of them means resolve, close or acknowledge, and no
envelope oto sends carries a status for your tool to adopt. **`quiet` is not `fixed`**: it says the
signals stopped, not that the response is over. If you want your incident tool to resolve on
`quiet`, that is a rule *you* write in your tool or bridge, and it ends a response the moment the
signals go quiet — including the times they went quiet because the thing that was emitting them
died.

The `incident` object:

| Key | Meaning |
|---|---|
| `id` | The Incident's id. **Stable for the Incident's whole life — this is your de-duplication key.** Key your external incident on it: every later fact about the same Incident carries the same `id`, so a receiver that upserts on it gets one external incident whose later facts are updates, not new incidents. |
| `number` | The number humans quote (`#12`). Unique per org; prefer `id` as a key. |
| `state` | `active` (some current member Case is open) or `quiet` (none is). Derived by oto from the Cases; nobody sets it. |
| `drawn_at` | When it was drawn. |
| `drawn_by` | `{kind: "human", label}` or `{kind: "correlator", correlator_id}`. |
| `members` | Every spell of every Case that has been in it, current and removed, in the order they joined. |
| `link` | oto's own page for it, when oto has a public URL configured. |

Each member: `case_id`, `case_number`, `case_state` (`open`/`closed`), `alert_id`, `alert_name`,
`labels`, `added_at`, `added_by`, and on a removed spell `removed_at`, `removed_by_label` and — when
it was moved — `moved_to_number`; `link` to the Case.

**Severity.** oto holds none for an Incident and invents none. If your tool needs one, map it from
the member alerts' own labels (`members[].labels.severity`, typically) in your receiver.

A full `case_added` envelope is checked in at
[`internal/channels/render/webhookjson/testdata/incident_case_added.golden.json`](../../internal/channels/render/webhookjson/testdata/incident_case_added.golden.json).

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
webhook a way to read an internal page.

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
