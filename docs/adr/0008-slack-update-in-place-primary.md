# 0008 — Slack: `chat.update` in place is primary; threads are secondary; one colour attachment

**Status:** Accepted · 2026-08-07

## Context
Three verified facts drive this.

1. **Rate limits.** `chat.postMessage` is a "Special" tier — roughly **one message per second per
   channel**. `chat.update` is **Tier 3 (50+/min)** and is not per-channel limited. Updating is
   both nicer for humans and ~50× cheaper.
2. **`notification_reason`** (Alertmanager ≥ 0.32.0) tells us *why* a group was re-sent.
   `"repeat interval elapsed"` means *nothing changed, this is a nag*. Stock Alertmanager and
   Grafana Alerting both **repost** on it. That is the loudest, cheapest noise source in the category.
3. **Colour.** Slack: *"the color parameter currently does not have a block alternative."*
   Attachments are legacy but **not deprecated**, and are the only way to get a colour bar.

Also: the `header` block is `plain_text` only — no bold, no links — so a header costs the deep
link for nothing. Sentry, Grafana OnCall and Alertmanager all use a `section` for the title.

## Decision
- **`chat.update` on the existing root is the primary mechanism.** Thread replies are the
  exception, gated by a per-channel `verbosity` setting and by the `notification_reason` →
  Reason → mode table in SPEC §H.6.
- **`repeat interval elapsed` → update only. Never post a new message.**
- Exactly **one** attachment wraps **all** blocks and carries `color`.
  **Colour encodes STATE** (Grafana OnCall's verified palette: firing `#a30200`, acked `#daa038`,
  silenced `#dddddd`, resolved `#2eb886`); **severity encodes as a leading emoji.** The colour
  then always answers the question a human scanning a channel is actually asking: *do I need to act?*
- Title is a `section` with a bold mrkdwn link. The `header` block is not used. The Block Kit
  `alert` block is not used at all (it is modals-only, despite the name).
- On every state change the previous value is rendered **struck through** (`~Firing~ → Resolved`),
  so a reader who saw the card an hour ago can tell what changed at zero block cost.
- The top-level `text` is a complete sentence, written deliberately: it is the push notification,
  the sidebar preview, the search snippet, **and the only thing screen readers read**.
- oto **never reads Slack back** to reconstruct its own state. `(channel_id, ts)` in our DB is
  the memory of Slack. `ts` is stored as TEXT, never a float.

## Consequences
- A flapping or repeating alert costs one Tier-3 update instead of a per-channel-limited post.
  A `rendered_hash` check skips no-op updates entirely.
- The channel stays scannable: state is visible without opening a thread.
- We accept the legacy-attachment risk (Slack warns content *"may be wrapped, truncated, or
  hidden behind a 'show more'"*). Mitigated by keeping the card to ~7 blocks. No sunset date
  exists as of the research date.
- Renderers must be pure functions with checked-in golden files and a CI Block Kit validator,
  because a broken layout is `invalid_blocks` — a `config_invalid` dead delivery.

## Alternatives rejected
- **Post a new message per lifecycle fact** (stock Alertmanager's behaviour): the noise oto exists
  to eliminate, and 50× more expensive.
- **No attachment, emoji only** (Sentry's approach): viable at the highest tier of the market and
  avoids the legacy dependency, but forfeits the peripheral-vision cue in a wall-of-alerts channel.
- **`response_url` for long-lived updates:** 30 minutes and 5 uses. It is for the optimistic
  response to a click, nothing more.
- **Reading `conversations.history` for crash recovery:** non-Marketplace distributed apps are
  throttled to 1 req/min with 15-object pages. Never depend on reading Slack.
