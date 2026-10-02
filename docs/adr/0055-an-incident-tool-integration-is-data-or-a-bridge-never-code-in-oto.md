# 0055 — An incident-tool integration is data or a bridge, never code inside oto

**Status:** Accepted · 2026-10-02 — the owner's ruling in a design session.
**Amends:** [0052](0052-an-incident-is-drawn-over-cases-and-its-response-is-handed-off.md) §5 —
"oto never sends a resolve" narrows to "oto's own code and oto's catalog never send one" (§4).
**Extends:** [0050](0050-a-notification-template-is-one-whole-message.md) and
[0051](0051-a-raw-template-owns-the-message-and-its-replies.md) — a fourth NotificationTemplate
format, `json`, pinned to the webhook channel as `raw` is pinned to Slack.
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

`oto.notification.v1` is the surface every integration builds on. It gains what an API needs to be
built on by strangers: a **signature header** (HMAC over the body with a per-channel secret, so a
receiver can tell oto sent it), and a written **compatibility promise** — additive changes only
within `v1`; a removal or a change of meaning is `v2`, sent side by side for a stated period.

### 2. A plugin is data: a `json` NotificationTemplate

A `json` template, pinned to the webhook channel, renders a request **body and headers** from the
envelope, and may name **where in a 2xx response** the external incident's URL and id are found
(a path expression), feeding the outbound mapping of 0052 §5. Its rules are 0050's: one whole
document, every interpolated value JSON-escaped with no opt-out, and **any failure falls back to the
plain `oto.notification.v1` envelope**, so a template can never mark a delivery dead. A **catalog**
of community templates is a folder of files — contributed, reviewed and imported like any other
template; no code ships with one.

### 3. Anything that needs logic is a bridge, outside oto

OAuth flows, several calls per fact, a vendor's REST API for notes: a small service the operator
runs, which receives oto's webhook. It runs out of oto's process, in any language. oto documents the
pattern; it does not host bridges.

### 4. An operator's template may turn a fact into a command; oto's never does

An operator may write a template that maps `quiet` to a vendor's resolve. That is a rule the
operator wrote, keyed on a fact oto stated — 0044's test, passed — and it is the operator's, not
oto's. **oto's own code sends no command, and no template in oto's catalog maps a fact to a resolve,
close or status change**; a catalog review refuses one. The docs say plainly that `quiet` is not
`fixed`, and that a template which resolves on `quiet` ends a response the moment the signals stop.

### 5. Refused: code plugins inside oto

Go plugins, and WASM runtimes (wazero, Extism) running operator- or community-supplied code in oto's
process, are refused. oto's process holds every org's channel credentials and its database; 0054
refused running commands there for the same reason. Data (§2) and bridges (§3) cover the need
without it.

## Consequences

- The vendor matrix is the catalog's problem, not the code's: adding a tool is a pull request of
  one file.
- 0052 §5's sentence "oto never sends a resolve" is narrowed by §4 and must be re-read with it.
