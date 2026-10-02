# 0055 — An incident-tool integration is data or a bridge, never code inside oto

**Status:** Accepted · 2026-10-02 — the owner's ruling in a design session. **Revised the same day**
(§2): the vendor mapping first landed as a `json` NotificationTemplate and was moved onto the
webhook Connection; the reason is kept in §6.
**Amends:** [0052](0052-an-incident-is-drawn-over-cases-and-its-response-is-handed-off.md) §5 —
"oto never sends a resolve" narrows to "oto's own code and oto's catalog never send one" (§4).
**Extends:** [0047](0047-a-channel-answers-to-a-connection.md) — a webhook Connection may carry a
payload mapping.
**Leaves untouched:** [0050](0050-a-notification-template-is-one-whole-message.md) and
[0051](0051-a-raw-template-owns-the-message-and-its-replies.md) — templates stay wording.
**Relates to:** [0054](0054-a-remedy-earns-the-write-path.md) (commands refused in oto's process —
the same reason applies here), [0044](0044-a-count-condition-is-a-silence-the-operator-asked-for.md)
(the authority test §4 rests on).

## Context

The owner ruled that oto ships no vendor-specific incident provider (0052 §5, amended), and asked
for a plugin ecosystem built on the events oto already gives out. Those events exist: the versioned
`oto.notification.v1` envelope on the generic webhook, with retries, idempotency, delivery audit and
drills, and a stable `incident.id` on every Incident fact. What does not exist is a way to reach a
vendor whose API wants a different JSON shape, without writing a service.

## Decision

### 1. The event contract is the plugin API

`oto.notification.v1` is the surface every integration builds on. It already signs each body (`X-Oto-Signature`, HMAC with the Connection's secret); it gains what an
API built on by strangers needs on top: **secret rotation, a signed timestamp** against replay, and a written **compatibility promise** — additive changes only
within `v1`; a removal or a change of meaning is `v2`, sent side by side for a stated period.

### 2. A plugin is data: a payload mapping on the webhook Connection

A webhook Connection may carry a **payload mapping**: a document that renders the request **body and
headers** from the envelope for each fact, and may name **where in a 2xx response** the external
incident's URL and id are found (a path expression), feeding the outbound mapping of 0052 §5. A
Connection without one sends the plain `oto.notification.v1` envelope, exactly as before.

It is destination setup, configured with the Connection in Settings → Connections, and it is held
to that surface's stakes, not a template's:

- **Checked before it is saved.** It is rendered against a sample envelope for every fact type, and
  a test send is offered; a mapping that does not render cannot be saved.
- **A failure is a failed delivery.** A mapping that fails at send time marks the delivery failed,
  visible and retryable in the delivery audit. It never falls back to the plain envelope, which the
  vendor could not parse — that would turn a broken mapping into a missing incident.
- Every interpolated value is JSON-escaped, with no opt-out.
- **A mapping never holds a secret.** A vendor key that must travel in the body (PagerDuty's
  `routing_key`) is a sealed credential on the Connection, which the mapping refers to by name and
  oto fills in at send time; a mapping file is safe to share in the catalog because it carries none.

A **catalog** of community mappings is a folder of files — contributed, reviewed and imported onto a
Connection; no code ships with one.

### 3. Anything that needs logic is a bridge, outside oto

OAuth flows, several calls per fact, a vendor's REST API for notes: a small service the operator
runs, which receives oto's webhook. It runs out of oto's process, in any language. oto documents the
pattern; it does not host bridges.

### 4. An operator's mapping may turn a fact into a command; oto's never does

An operator may write a mapping that turns `quiet` into a vendor's resolve. That is a rule the
operator wrote, keyed on a fact oto stated — 0044's test, passed — and it is the operator's, not
oto's. **oto's own code sends no command, and no mapping in oto's catalog turns a fact into a
resolve, close or status change**; a catalog review refuses one. The docs say plainly that `quiet`
is not `fixed`, and that a mapping which resolves on `quiet` ends a response the moment the signals
stop.

### 5. Refused: code plugins inside oto

Go plugins, and WASM runtimes (wazero, Extism) running operator- or community-supplied code in oto's
process, are refused. oto's process holds every org's channel credentials and its database; 0054
refused running commands there for the same reason. Data (§2) and bridges (§3) cover the need
without it.

### 6. Why the mapping is not a template

The first draft made the mapping a fourth NotificationTemplate format, `json`. The owner refused it:
a template is **wording** — how a message reads — and 0050 makes it safe to edit by falling back to
oto's built-in card on any failure. A vendor mapping decides what the incident tool **does**. On the
template surface, a wording edit could switch "resolve on quiet" on, and the harmless fallback
would become a silent missed incident. Where a fact goes and in what wire format is the
destination's business, so the mapping lives with the destination.

## Consequences

- The vendor matrix is the catalog's problem, not the code's: adding a tool is a pull request of
  one file.
- 0052 §5's sentence "oto never sends a resolve" is narrowed by §4 and must be re-read with it.
- Templates keep three formats; nothing about them changes.
