# Connecting oto to Slack

oto uses a **Slack app you create in your own workspace**. There is no OAuth flow,
no "Add to Slack" button, no Slack Marketplace listing and no oto-operated service
in the middle. You create an app, install it, and paste three credentials into your
own oto configuration. That is the whole integration. The reasoning is in
[ADR 0018](../adr/0018-slack-distribution-model.md).

Budget about ten minutes.

---

## 1. Create the app from the manifest

Ticking scopes by hand in the Slack UI is error-prone, and the mistakes surface
later as confusing runtime failures (`missing_scope` at 3am rather than at setup).
Use the manifest instead.

1. Go to <https://api.slack.com/apps> and click **Create New App**.
2. Choose **From an app manifest**.
3. Pick the workspace oto should post into.
4. Paste the contents of [`deploy/slack/manifest.yaml`](../../deploy/slack/manifest.yaml).
5. Review the summary Slack shows you, then **Create**.

The manifest is commented with a justification for every scope it asks for, and a
list of the scopes it deliberately does **not** ask for. If your security team
reviews app installs, that file is the document to send them.

**What it requests:** `chat:write`, `channels:read`, `groups:read`. Nothing else.
Notably absent: `chat:write.public` (oto must be invited to a channel, it cannot
post itself into any public channel), `users:read` (oto never reads your member
directory) and every `*:history` scope (oto never reads messages back — see
[ADR 0008](../adr/0008-slack-update-in-place-primary.md)).

---

## 2. Install it to the workspace

**OAuth & Permissions → Install to Workspace → Allow.**

Slack then shows you a **Bot User OAuth Token** beginning `xoxb-`. That is the
first of the three credentials. It does not expire; `token_rotation_enabled` is
`false` in the manifest because rotation exists to serve OAuth-distributed apps
and this is not one.

---

## 3. Create the app-level token (Socket Mode)

Socket Mode is oto's default transport because a self-hosted install usually has
no inbound HTTPS. oto dials **out** to Slack over a WebSocket and receives button
presses on it — no ingress rule, no TLS certificate, no public URL.

App-level token scopes **cannot be declared in a manifest** (the manifest schema
covers only bot and user OAuth scopes), so this one step is manual:

1. **Basic Information → App-Level Tokens → Generate Token and Scopes.**
2. Name it something like `oto-socket`.
3. Add the scope **`connections:write`**.
4. Generate. Copy the `xapp-…` value.

That is the second credential.

---

## 4. Copy the signing secret

**Basic Information → App Credentials → Signing Secret → Show.**

That is the third credential. It is only used by the **HTTP** interactivity
transport, which verifies an HMAC over the raw request body. In Socket Mode the
socket is pre-authenticated and no signature check exists, so you can skip this if
you are staying on Socket Mode. Copy it anyway — it costs nothing and it is what
you will need the day you move behind an ingress.

---

## 5. Where each credential goes in oto

| Slack value | Starts with | oto config field | Env var | Notes |
|---|---|---|---|---|
| Bot User OAuth Token | `xoxb-` | channel credential, `kind: "slack_bot_token"` | — | Per channel, stored in the database, sealed |
| App-Level Token | `xapp-` | `slack.app_token` | `OTO_SLACK_APP_TOKEN` | Process-level; Socket Mode only |
| Signing Secret | *(32 hex chars)* | `slack.signing_secret` | `OTO_SLACK_SIGNING_SECRET` | Process-level; **required** in HTTP mode |

Plus the two switches:

```bash
OTO_SLACK_ENABLED=true
OTO_SLACK_MODE=socket        # or `http`
OTO_SLACK_APP_TOKEN=xapp-...
OTO_SLACK_SIGNING_SECRET=... # MUST be non-empty when OTO_SLACK_MODE=http
```

oto refuses to boot with `mode=http` and an empty signing secret. That is
deliberate: an empty secret would accept forged requests, which means anyone on
the internet could acknowledge anyone's alert.

The **bot token is not an environment variable.** It is attached to a channel,
because one oto install can post into several workspaces:

```http
POST /api/v1/channels
Content-Type: application/json

{
  "type": "slack",
  "name": "platform-alerts",
  "config": {
    "team_id": "T9TK3CUKW",
    "conversation_id": "C0123456789",
    "conversation_name": "platform-alerts",
    "transport": "socket_mode"
  },
  "credential": {
    "kind": "slack_bot_token",
    "values": { "bot_token": "xoxb-..." }
  }
}
```

`conversation_id` is a **channel ID, never a `#name`**. Names are ambiguous,
mutable, and resolve differently for different tokens. Get the ID from the channel
detail dialog in Slack (bottom of **About**), or from the channel URL.

### How oto stores these

All three are secret material and all three are treated as such.

- The bot token is sealed with **AES-256-GCM** against the keyring in
  `internal/platform/secrets` before it is written to `channel_credentials`. The
  key comes from `OTO_SECURITY_SECRET_KEY` (base64 of 32 random bytes —
  `openssl rand -base64 32`). The keyring is versioned so keys can be rotated
  without re-entering credentials.
- The oto API is **write-only** for secrets. `GET /api/v1/channels` will tell you
  *which kind* of credential is attached and *when it was last rotated*. There is
  no endpoint, anywhere, that returns a credential value. Nothing in the web UI
  can display one.
- The app-level token and signing secret are process configuration and live
  wherever you put your other environment secrets.

**oto transmits these to exactly one place: `slack.com`.** There is no telemetry
endpoint, no license server, no phone-home, and no oto-operated relay. This is a
consequence of the distribution model, not a promise layered on top of it — an
internal app has no third party in the trust path by construction.

---

## 6. Invite the bot to the channel

```
/invite @oto
```

in each channel oto should post to. This is required, and it is required by
design: oto does **not** request `chat:write.public`, so it can only reach channels
a human has explicitly let it into. That invite is an auditable event in the
channel's own history.

For a **private** channel, the invite is also what makes `groups:read` apply — the
scope only covers private channels the app has been added to.

---

## 7. Verify

1. **In oto:** `POST /api/v1/channels/{id}/test`, or the **Test** button on the
   channel in the settings UI. This runs the same probe oto uses for health: an
   `auth.test` to prove the token is alive, then a `conversations.info` to prove
   the bot can actually see the destination. The two fail differently on purpose —
   a token that works but a channel oto was removed from is the common real-world
   failure, and it should not read as "your token is broken".
2. The channel row should show the workspace and bot identity (`connected to Acme
   Corp as @oto`) rather than a bare green tick.
3. **In Slack:** fire a test alert and press **Acknowledge** on the card. The
   button should stop showing a spinner within three seconds, and the card's
   status line should change to `~Firing~ → Acked by @you`.

If the button spins and then shows *"This app is not responding"*, interactivity
is not reaching oto — jump to the Socket Mode row in the table below.

---

## Troubleshooting

oto classifies Slack's error codes by **what you should do about them**, not by
Slack's own taxonomy. The code appears verbatim in the delivery record and in the
channel health banner, so you can look it up here.

### The destination is gone — oto marks the thread dead and stops retrying

| Code | What actually happened | What to do |
|---|---|---|
| `channel_not_found` | The `conversation_id` in the channel config does not name a conversation this token can address. Nearly always a wrong or stale ID — or an ID copied from a **different workspace**. | Re-copy the channel ID from Slack. Check `team_id` matches the workspace the token belongs to. If the channel was deleted, point the oto channel somewhere else. |
| `not_in_channel` | The channel exists, the token is fine, the bot is simply not a member. Someone removed it, or it was never invited. | `/invite @oto` in that channel. Then re-run the channel test. |
| `is_archived` | Someone archived the channel. | Unarchive it, or point the oto channel at a live one. Do not expect oto to un-archive it — it has no scope to. |
| `is_inactive` | The conversation is a DM with a deactivated account. | Point the channel at a real channel. |

None of these retry, and that is correct: retrying twelve times against a channel
that will never exist again just delays the moment you find out. **Check the
Deliveries view in oto** — a dead delivery is recorded and shown, because oto's
silence must never be indistinguishable from "there was no alert".

### The credential is dead — oto marks the channel `auth_failed` and raises a banner

| Code | What actually happened | What to do |
|---|---|---|
| `invalid_auth`, `not_authed` | The token is malformed, empty, or not a bot token. Also produced by HTTP 401/403. | Confirm you pasted the `xoxb-` **Bot User OAuth Token**, not the `xapp-` app token and not the signing secret. |
| `token_revoked` | Someone uninstalled the app, or revoked the token in the Slack admin. | Reinstall the app and rotate the credential on the oto channel. |
| `account_inactive` | The workspace or the bot's account is deactivated. | Slack admin question, not an oto one. |
| `missing_scope`, `no_permission` | The app is missing a scope it needs for the call it just made. In practice this is almost always `groups:read` on a private channel, or `im:read` on a DM destination. | Compare the app's installed scopes against `deploy/slack/manifest.yaml`. Add the missing one under **OAuth & Permissions**, then **reinstall the app** — Slack does not apply new scopes to an existing installation until you do. |
| `not_allowed_token_type` | An app-level (`xapp-`) or user (`xoxp-`) token was supplied where a bot token belongs. | Use the `xoxb-` token. |

These never retry either. "Your token was revoked three days ago and nobody
noticed" is a failure oto reports rather than hides.

### oto's stored message pointer is gone — oto recovers by itself

`message_not_found`, `cannot_reply_to_message`, `edit_window_closed`,
`restricted_action_thread_locked`. The channel is fine; the specific message oto
was editing is not (someone deleted it, or the workspace restricts edits). oto
clears the stored pointer, posts a fresh root card with a `continued` marker and
re-points the thread. **No action needed.** If it happens constantly, check
whether your workspace has a message-retention policy shorter than your alert
lifetimes.

### Rate limiting

`ratelimited` / HTTP 429. oto honours `Retry-After` exactly and reschedules the
job. You should rarely see this, because oto's whole delivery model is built to
avoid it: `chat.update` (Tier 3, 50+/min) instead of `chat.postMessage` (~1
message/second/channel). If it is persistent, something is posting far more root
messages than expected — look at flap and storm damping in
[tuning.md](tuning.md).

### The Acknowledge button does nothing

| Symptom | Cause | Fix |
|---|---|---|
| Button spins, then *"This app is not responding"*, Socket Mode | oto is not connected to the socket. | Check `OTO_SLACK_ENABLED=true`, `OTO_SLACK_MODE=socket`, and that `OTO_SLACK_APP_TOKEN` holds an `xapp-` token **with the `connections:write` scope**. A token generated without that scope authenticates and then fails to open a connection. |
| Same, HTTP mode | Slack cannot reach `POST /api/v1/integrations/slack/interactions`, or the signature check is failing. | Confirm the **Interactivity → Request URL** in the Slack app matches your public URL. Confirm `OTO_SLACK_SIGNING_SECRET` matches **Basic Information → Signing Secret** exactly. oto reports every verification failure identically on purpose (telling an attacker which half of a forgery to fix is not helpful), so check oto's logs for the specific `slack_signature_*` reason. |
| Signature failures that come and go | Clock drift. oto rejects any request whose timestamp is more than five minutes from its own clock, in either direction. | Fix NTP on the oto host. |
| Button works, but the ack is attributed to a bare Slack handle instead of an oto user | That Slack member is not linked to an oto user. | Link the identity in oto's user settings. oto deliberately records the ack anyway rather than refusing it — losing an acknowledgement because of a missing link would be worse. |

### `invalid_blocks`, `msg_too_long`, `too_many_attachments`

**This is an oto bug, not a configuration problem.** oto built a message Slack
would not accept. The delivery is dead on arrival and oto alerts on itself
(`oto_render_invalid_total`). Please file an issue with the delivery ID.

---

## If you need HTTP instead of Socket Mode

Socket Mode caps at 10 concurrent connections per app and is not permitted for
Slack Marketplace apps. Neither constrains a self-hosted install, so switch only if
you already have public HTTPS ingress and want horizontal scale beyond that.

1. Set `OTO_SLACK_MODE=http` and a non-empty `OTO_SLACK_SIGNING_SECRET`.
2. In the Slack app: **Interactivity & Shortcuts → Request URL** →
   `https://<your-oto-host>/api/v1/integrations/slack/interactions`.
3. Turn **Socket Mode** off in the Slack app.
4. Set `transport: "http"` on the oto channel config.

Both transports run behind one handler, so behaviour is identical and a bug fixed
in one is fixed in both.
