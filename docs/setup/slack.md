# Connecting oto to Slack

oto uses a **Slack app you create in your own workspace**. There is no OAuth flow,
no "Add to Slack" button, no Slack Marketplace listing and no oto-operated service
in the middle. You create an app, install it, and paste two credentials into your
own oto configuration. That is the whole integration. The reasoning is in
[ADR 0018](../adr/0018-slack-distribution-model.md).

> **If you want the Acknowledge button on alert cards to work, [section 3](#3-turn-on-interactivity--required-for-the-acknowledge-button)
> is not optional and Socket Mode is not an option.** oto implements one
> interactivity transport: a public HTTPS Request URL with a signing secret.

Budget about ten minutes.

---

## 1. Create the app from the manifest

Ticking scopes by hand in the Slack UI is error-prone, and the mistakes surface
later as confusing runtime failures (`missing_scope` at 3am rather than at setup).
Use the manifest instead.

1. Go to <https://api.slack.com/apps> and click **Create New App**.
2. Choose **From an app manifest**.
3. Pick the workspace oto should post into.
4. Copy the contents of [`deploy/slack/manifest.yaml`](../../deploy/slack/manifest.yaml)
   and **replace the one placeholder in it** — `<your-oto-host>` in
   `settings.interactivity.request_url` — with the public HTTPS host oto is
   reachable at, so the line reads:

   ```yaml
   request_url: https://oto.your-company.example/api/v1/integrations/slack/interactions
   ```

   That is the endpoint the **Acknowledge** button POSTs to. Nothing else in the
   file needs editing. If you have not decided on a host yet, see
   [section 3](#3-turn-on-interactivity--required-for-the-acknowledge-button) —
   you can paste the manifest as-is and set the Request URL later, but the button
   will not work until you do.
5. Paste it into Slack.
6. Review the summary Slack shows you, then **Create**.

The manifest sets **`socket_mode_enabled: false`**, which is the only correct
value: **Socket Mode is not implemented in oto** (see
[section 3](#3-turn-on-interactivity--required-for-the-acknowledge-button) and
[Why there is no Socket Mode](#why-there-is-no-socket-mode)). Do not switch it on
in the Slack UI afterwards — Slack will drop the Request URL you just set, and
button presses will go to a WebSocket nothing is listening on.

The manifest is commented with a justification for every scope it asks for, and a
list of the scopes it deliberately does **not** ask for. If your security team
reviews app installs, that file is the document to send them.

**What it requests: `chat:write`, `channels:read`, `groups:read`. That is the
entire list.**

oto makes exactly five Slack calls — `chat.postMessage`, `chat.update`,
`auth.test`, `conversations.list` and `conversations.info` — and the third
needs no scope at all. The other two are `channels:read`/`groups:read`, added
back in [ADR 0047](../adr/0047-a-channel-answers-to-a-connection.md) so the
settings screen can turn a typed channel **name** into its **id**, or the
reverse, instead of you copying an id out of Slack's own UI by hand: **Settings
→ Connections → (your Slack connection) →** creating or editing a channel now
resolves one from the other live. They were requested once, cut for having no
caller, and are back with one: `POST
/api/v1/channel-connections/{id}/slack/resolve`.

Notably absent:

- `chat:write.public` — oto must be invited to a channel; it cannot post itself
  into any public channel in the workspace.
- `users:read` — oto never reads your member directory. An acknowledgement is
  attributed from the signed interaction payload.
- every `*:history` scope, and `conversations.replies` — oto never reads
  messages back (see [ADR 0008](../adr/0008-slack-update-in-place-primary.md)).
  `conversations.list`/`.info` return channel **metadata** (name, id, archive
  state), never message content.
- `files:write`, `incoming-webhook` — oto uploads nothing and posts under a bot
  token to destinations configured in oto, not to an install-time channel picker.
- `im:read` / `mpim:read` — the two calls above only ever ask about public and
  private channels, never a DM.

If you installed oto before this change, **reinstall from the current
manifest** to add the two scopes — the name/id resolver in Settings needs them
and will answer a plain "not configured" error without them.

---

## 2. Install it to the workspace

**OAuth & Permissions → Install to Workspace → Allow.**

Slack then shows you a **Bot User OAuth Token** beginning `xoxb-`. That is the
first of the three credentials. It does not expire; `token_rotation_enabled` is
`false` in the manifest because rotation exists to serve OAuth-distributed apps
and this is not one.

---

## 3. Turn on interactivity — required for the Acknowledge button

> **⛔ Read this section even if you skip every other one.**
>
> **Socket Mode is not implemented.** There is no WebSocket client anywhere in
> oto. `OTO_SLACK_MODE` still *defaults* to `socket`, and `OTO_SLACK_APP_TOKEN`
> is still *accepted* — but nothing reads either of them, so a deployment left on
> the defaults posts alert cards whose **Acknowledge button nothing is listening
> for**. The press is delivered to a Slack app with no Request URL, and the user
> sees a failure.
>
> **Interactivity in oto v1 means a public HTTPS Request URL plus a signing
> secret. There is no second option.** If you cannot expose one, oto still works
> — alerts, grouping, cards, threads, the whole delivery path — you simply have
> no buttons, and you acknowledge from oto's own UI instead.
>
> You do **not** need an app-level (`xapp-`) token. Do not generate one; it has
> no reader.

### 3.1 Point Slack at your oto

**The manifest already does this** — `interactivity.is_enabled: true`,
`socket_mode_enabled: false`, and a `request_url` you filled the host into in
[section 1](#1-create-the-app-from-the-manifest). So this section is a *check*,
not a chore, unless you pasted the manifest with the placeholder still in it.

1. Make oto reachable at a public HTTPS URL (an ingress, a load balancer, a
   tunnel — oto does not care which).
2. In the Slack app, open **Interactivity & Shortcuts**. **Interactivity** should
   already be **On** and **Request URL** should already read:

   ```
   https://<your-oto-host>/api/v1/integrations/slack/interactions
   ```

   with your own host in place of `<your-oto-host>`. If it is empty, or still has
   the literal placeholder in it, set it now and **Save**.

3. Slack does **not** verify the URL on save for interactivity, so a typo here
   surfaces later as a button that fails rather than as an error now. The path is
   fixed by oto's router — only the host is yours.

### 3.2 Copy the signing secret

**Basic Information → App Credentials → Signing Secret → Show.**

This is the second credential, and in HTTP mode it is **mandatory**. Every
interaction is authenticated by an HMAC-SHA256 over the **raw** request body,
compared in constant time, with anything more than five minutes from oto's clock
refused in either direction. There is no other authentication on that endpoint —
Slack has no session and no token — so the signing secret *is* the door.

oto **refuses to boot** with `mode=http` and an empty signing secret. That is
deliberate: an empty secret would accept forged requests, which means anyone on
the internet could acknowledge anyone's alert.

### 3.3 Set the two switches

```bash
OTO_SLACK_ENABLED=true
OTO_SLACK_MODE=http          # `socket` is accepted and does nothing
OTO_SLACK_SIGNING_SECRET=... # MUST be non-empty when OTO_SLACK_MODE=http
```

### What the button actually does

Pressing **Acknowledge** records a receipt on every still-open alert in that
group: *a human has seen this*. It is not an assignment, it does not change the
alert's state, and it does not say who is looking at it — an acked alert is still
firing, still whatever severity it was, and every surface keeps rendering it that
way. The card's colour and label change, and the acknowledgement appears on the
alert's timeline.

If the Slack member who pressed it is linked to an oto user, the timeline names
that user. **If they are not, the acknowledgement is still recorded**, attributed
to their Slack handle — losing a real acknowledgement because somebody has not
onboarded would be worse than the missing link.

oto answers Slack's request **before** it does any of that work, so a slow
database shows up as a card that updates a moment late, never as *"This app is
not responding"*.

---

## 5. Where each credential goes in oto

| Slack value | Starts with | oto config field | Env var | Notes |
|---|---|---|---|---|
| Bot User OAuth Token | `xoxb-` | channel credential, `kind: "slack_bot_token"` | — | Per channel, stored in the database, sealed |
| Signing Secret | *(32 hex chars)* | `slack.signing_secret` | `OTO_SLACK_SIGNING_SECRET` | Process-level; **required**, and the only thing authenticating the interactions endpoint |

Plus the two switches:

```bash
OTO_SLACK_ENABLED=true
OTO_SLACK_MODE=http
OTO_SLACK_SIGNING_SECRET=...
```

**Not in the table, on purpose:** `OTO_SLACK_APP_TOKEN` / `slack.app_token`. The
key is still accepted by the config loader and **is read by nothing** — it exists
for a Socket Mode client that was never built. Setting it has no effect. It will
be removed.

oto refuses to boot with `mode=http` and an empty signing secret. That is
deliberate: an empty secret would accept forged requests, which means anyone on
the internet could acknowledge anyone's alert.

The **bot token is not an environment variable.** It is attached to a
**connection**, not to a channel — one oto install can post into several
workspaces, and one workspace usually has several channels sharing the one
bot token that talks to it. Set the connection up once, in **Settings →
Connections**:

```http
POST /api/v1/channel-connections
Content-Type: application/json

{
  "type": "slack",
  "name": "Acme Slack workspace",
  "config": { "team_id": "T9TK3CUKW" },
  "credential": {
    "kind": "slack_bot_token",
    "values": { "bot_token": "xoxb-..." }
  }
}
```

Then create each channel against that connection — from the notification
policy screen where you are already naming a destination, or directly:

```http
POST /api/v1/channels
Content-Type: application/json

{
  "type": "slack",
  "name": "platform-alerts",
  "connection_id": "<the connection's id, from the response above>",
  "config": { "conversation_id": "C0123456789", "conversation_name": "platform-alerts" }
}
```

You do not have to know both halves of `config` by hand. Type just the name or
just the id and `POST /api/v1/channel-connections/{id}/slack/resolve` fills in
the other — that is what the Settings UI does behind its channel dialog. If
you are scripting against the API directly:

```http
POST /api/v1/channel-connections/<connection id>/slack/resolve
Content-Type: application/json

{ "name": "platform-alerts" }
```

```json
{ "conversation_id": "C0123456789", "conversation_name": "platform-alerts" }
```

(Or supply `"conversation_id"` instead of `"name"` to resolve the other way.)
This is the one place oto reads Slack back at all — a metadata lookup at
configuration time, never on the delivery path (ADR 0047).

### How oto stores these

All three are secret material and all three are treated as such.

- The bot token is sealed with **AES-256-GCM** against the keyring in
  `internal/platform/secrets` before it is written to `channel_credentials`. The
  key comes from `OTO_SECURITY_SECRET_KEY` (base64 of 32 random bytes —
  `openssl rand -base64 32`). The keyring is versioned so keys can be rotated
  without re-entering credentials.
- The oto API is **write-only** for secrets. `GET /api/v1/channel-connections`
  will tell you *which kind* of credential is attached and *when it was last
  rotated* — the channel itself (`GET /api/v1/channels`) carries none of that
  any more, only the `connection_id` to look it up by. There is no endpoint,
  anywhere, that returns a credential value. Nothing in the web UI can display
  one.
- The signing secret is process configuration and lives wherever you put your
  other environment secrets.

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

A **private** channel works the same way and needs no extra scope: `chat:write`
lets oto post to any conversation it is a member of, and the invite is what makes
it a member.

---

## 7. Verify

1. **In oto:** `POST /api/v1/channels/{id}/test`, or the **Test** button on the
   channel in the settings UI. This runs the same probe oto uses for health: an
   `auth.test`, which proves the token is alive and needs no scope.

   It deliberately does **not** check that the destination exists, even though
   oto now holds `channels:read`/`groups:read` for the settings resolver above —
   the probe stays scope-minimal in spirit and learns the same fact at the
   first delivery instead: `channel_not_found`, `is_archived` and
   `not_in_channel` are all terminal, and oto marks the channel degraded and
   tells you which one it was rather than retrying into a wall. So a green
   probe means "the token works";
   send a test alert to prove the destination.
2. The channel row should show the workspace and bot identity (`connected to Acme
   Corp as @oto`) rather than a bare green tick.
3. **In Slack:** fire a test alert and press **Acknowledge** on the card. The
   button should settle immediately, and the card should change to its
   acknowledged colour and label with your name against it. The same
   acknowledgement appears on the alert's timeline in oto.

   This only works if [section 3](#3-turn-on-interactivity--required-for-the-acknowledge-button)
   is done. With `OTO_SLACK_MODE=socket` — still the default — nothing is
   listening and the press fails.

If the button spins and then shows *"This app is not responding"*, interactivity
is not reaching oto — see the table below.

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
| `is_inactive` | Slack's `chat.update` reference defines this as a **frozen, archived or deleted** conversation — not, as this table used to say, only a DM with a deactivated account. | Point the oto channel at a live conversation. |

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
| `missing_scope`, `no_permission` | The app is missing a scope it needs for the call it just made. Since the scope list is one item long this should now only mean `chat:write` itself is missing — e.g. the app was installed from an older manifest, or a scope was un-ticked by hand. | Compare the app's installed scopes against `deploy/slack/manifest.yaml`. Add the missing one under **OAuth & Permissions**, then **reinstall the app** — Slack does not apply new scopes to an existing installation until you do. |
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
messages than expected.

⛔ **Do not go looking for flap damping — there is none.** It was retired
(git-bug `235f347`) and `flapping` left the suppression chain with storm collapse
([ADR 0042](../adr/0042-storm-damping-is-removed.md)); the `flap_*` tuning keys
outlived the mechanism they configured. oto damps nothing on its own judgement, and
a burst of real firings is reported in full, by design — one conversation per alert
([ADR 0045](../adr/0045-a-case-is-a-conversation-and-a-thread-per-alert-is-accepted.md)),
which is the accepted cost, not a bug to tune away.

Two things are actually actionable, and both are opt-in:

- A **notification policy count condition** (`count_min` over `count_window_s`) —
  "do not speak until this has happened five times in an hour". This is the
  supported replacement for what flap damping was reaching for (git-bug `7570090`,
  [ADR 0044](../adr/0044-a-count-condition-is-a-silence-the-operator-asked-for.md)).
  It binds to Cases.
- A **digest** (`digest_window_s`), which collapses a window into one message.

Then check your Alertmanager `group_by` in [tuning.md](tuning.md): oto reports what
it is sent, so a source fanning one event into many alerts is fixed at the source.

### The Acknowledge button does nothing

| Symptom | Cause | Fix |
|---|---|---|
| Button spins, then *"This app is not responding"* | **By far the most common cause: `OTO_SLACK_MODE` is still `socket`.** Socket Mode is not implemented, so nothing is listening for the press. | Set `OTO_SLACK_MODE=http`, set a signing secret, and configure the Request URL. See [section 3](#3-turn-on-interactivity--required-for-the-acknowledge-button). Generating an `xapp-` token will **not** help; nothing reads it. |
| Same, already on `mode=http` | Slack cannot reach `POST /api/v1/integrations/slack/interactions`, or the signature check is failing. | Confirm the **Interactivity → Request URL** matches your public URL exactly, path included. Confirm `OTO_SLACK_SIGNING_SECRET` matches **Basic Information → Signing Secret**. oto reports every verification failure identically on purpose (telling an attacker which half of a forgery to fix is not helpful), so check oto's logs for the specific `slack_signature_*` reason. |
| Signature failures that come and go | Clock drift. oto rejects any request whose timestamp is more than five minutes from its own clock, in either direction. | Fix NTP on the oto host. |
| Button settles, but the card never changes | The press was recorded and the follow-up notification has not gone out. The card is updated through the normal delivery path — deliberately, so thread ordering and rate limiting still apply — so a backed-up `notify` or `deliver_slack` queue delays it. | Check the Deliveries view and the queue depth metrics. The acknowledgement itself is already on the alert's timeline; only the card is late. |
| oto replies *"oto has no channel configured for this conversation"* | The press came from a Slack conversation no oto channel points at. oto resolves the tenant from `(team_id, conversation_id)` and will not guess. | Create an oto channel whose `config.conversation_id` is that channel's ID, in the right organisation. |
| oto replies *"already acknowledged"* | Somebody got there first — possibly you, twice. | Nothing to fix. An acknowledgement is a receipt, and the first one is the fact on the record. |
| Button works, but the ack is attributed to a bare Slack handle instead of an oto user | That Slack member is not linked to an oto user **in the organisation that owns this channel**. The mapping is per-organisation on purpose: one workspace can serve two oto tenants, and a link in one must not attribute a press in the other. | Link the identity in oto's user settings. oto records the ack either way rather than refusing it — losing an acknowledgement because of a missing link would be worse. The first press already stored the Slack identity, so linking is a pick-from-a-list rather than typing a member id. |

### `invalid_blocks`, `msg_blocks_too_long`, `too_many_attachments`, `metadata_*`

**This is an oto bug, not a configuration problem.** oto built a message Slack
would not accept. The delivery is dead on arrival, it is never retried — a
payload that is illegal is exactly as illegal on the twelfth attempt. It lands
as a dead delivery carrying the offending payload, and `oto_jobs_dead_total`
carries the rate. Please file an issue with the delivery ID.

The full set oto treats this way:
`invalid_blocks`, `invalid_blocks_format`, `block_mismatch`,
`msg_blocks_too_long`, `msg_too_long`, `too_many_attachments`,
`invalid_attachments`, `attachment_payload_limit_exceeded`, `metadata_too_large`,
`invalid_metadata_format`, `invalid_metadata_schema`,
`metadata_must_be_sent_from_app`, `no_dual_broadcast_content_update`, `no_text`,
`markdown_text_conflict`, `invalid_arguments`.

Two of those are worth knowing about individually.

- **`msg_too_long` is a `chat.update` error and is not returned by
  `chat.postMessage` at all.** Posting has no length rejection: Slack **silently
  truncates** a message over 40 000 characters. A card that was shortened without
  saying so is the one failure mode oto's whole render-validation story exists to
  prevent, and it is the one Slack will do quietly. oto's own cap is far below
  either number, so this should be unreachable.
- **`metadata_must_be_sent_from_app`** would mean **no oto card has ever been
  delivered** — see the note in [section 8](#8-verifying-oto-against-a-real-workspace-the-part-no-test-can-do).

---

## 8. Verifying oto against a real workspace — the part no test can do

> **Read this if you have a Slack workspace and thirty minutes. You are the only
> person who can close it.**

> ⭐ **There is now a step-by-step run sheet:
> [slack-live-verification.md](slack-live-verification.md).** Eleven numbered steps,
> each naming the exact observation it needs and which ADR unknown it discharges,
> ending in what to write down and where. Use it instead of the tables below if you
> are actually sitting down to do this; the tables here are the reasoning, and that
> document is the procedure. It covers the four behaviours that remain unobserved
> after the first live run (git-bug `2078a07`): the in-place `chat.update`, a mention
> that reaches a locked phone, the resolved and snoozed cards, and client parity
> across desktop, web, iOS and Android.

**A real workspace has been connected exactly once**, on 2026-08-09 — `a7cdec3`,
*"the Slack card defects found by running it for real"*. That run settled one
rendering question (what an in-channel `thread_broadcast` does with attachments,
colour and buttons; ADR 0020 Amendment 4) and found four card defects that no
offline check had caught. **Everything else oto claims about Slack's behaviour is
still checked by oto against oto** — every rule lives in
`internal/channels/render/slack/validate.go`, a closed loop that cannot detect a
wrong belief. Two ADRs rest on that loop:
[0008](../adr/0008-slack-update-in-place-primary.md) (update in place, never
watched happening) and
[0020](../adr/0020-broadcast-the-transitions-that-must-be-seen.md) (broadcast,
one client, one observer).

Two things have been done to make your thirty minutes count.

1. **The card payloads are checked in.** `test/fixtures/slack/` holds every card
   variant oto can emit, twice: `*.message.json` is the exact bytes oto sends,
   and `*.blockkit.json` is the same card in the shape
   [Block Kit Builder](https://app.slack.com/block-kit-builder) accepts.
   `index.json` lists them with the attachment colour of each.
2. **The client behaviour is already proved.** `test/harness/slack_conformance.go`
   is a Slack double that enforces Slack's *published* request contract, and
   `test/harness/slack_conformance_test.go` drives the real provider through
   root → reply → update → broadcast against it. So the arguments, the threading
   and the error handling are not what you are checking. **You are checking
   Slack's renderer**, which is the one thing a fake cannot be.

### 8.1 Five minutes, no workspace admin needed: Block Kit Builder

Open <https://app.slack.com/block-kit-builder>, and for each `*.blockkit.json`
file paste its contents over the sample payload.

| # | File | What must be true | What a failure looks like |
|---|---|---|---|
| 1 | `root_firing.blockkit.json` | Seven blocks. Title is a **bold clickable link**, cluster is a separate grey chip. Two-column field grid. Three buttons plus a `…` overflow with five entries. | A validation error in the right-hand pane names the offending block. A title that is not a link means the section/`header` decision (S1) is wrong. |
| 2 | `root_update_acked.blockkit.json` | Status field reads `~Firing~ → Acked by …`, i.e. **struck through**. | Literal tildes on screen means Slack's strikethrough is not single-`~`. |
| 3 | `root_resolved.blockkit.json` | The state trail context line renders as one line with `→` separators and **times in your own timezone**, not UTC. | `<!date^…>` shown raw means the token or its fallback is malformed. |
| 4 | `root_silenced.blockkit.json` | The rule expression renders inside a code span with a literal **`>`**, not `&gt;`. | `&gt;` on screen means oto is double-escaping mrkdwn. |
| 5 | `thread_reply_acked.blockkit.json` | One section. Emoji `:eyes:` renders as a glyph. | A literal `:eyes:` means shortcodes are not resolved in `mrkdwn`. |
| 6 | `broadcast_unacked_reminder.blockkit.json` | One section. | — |

> A seventh file, `storm_notice.blockkit.json`, was deleted with storm damping
> ([ADR 0042](../adr/0042-storm-damping-is-removed.md)).

⛔ **What this cannot check, and it is a lot.** Block Kit Builder renders
`blocks` only. It cannot render `attachments`, and *every* oto block lives inside
one attachment because that is the only way to get a colour bar (§H.1 S3). So the
builder proves nothing about the **colour bar**, the **top-level `text`** (the
push notification and the screen-reader content), the attachment **fallback**, or
the **metadata**. Those need 8.2.

### 8.2 Twenty-five minutes, with a workspace: the six behaviours

Install the app ([section 1](#1-create-the-app-from-the-manifest), with your host
filled into the manifest's `request_url`), invite it to a scratch channel, point
an oto channel at that conversation id, and fire a synthetic alert.

**Do the first check before anything else — it can invalidate every other one.**

| # | Behaviour | Do this | It passes if | It fails if |
|---|---|---|---|---|
| 0 | **Metadata is accepted from a bot token** | Send one alert. Look at the delivery record. | The delivery succeeded. | The delivery is dead with `metadata_must_be_sent_from_app`. Slack lists that error on both write methods with the text *"message metadata can only be posted or updated using an app-level token"*, and **oto attaches metadata to every card under an `xoxb-` bot token**. If it fires, no oto card has ever been deliverable and `rootMetadata` in `internal/channels/render/slack/root.go` must go. Nothing offline can decide this. |
| 1 | **The card renders** | Look at the firing card in the channel. | It matches `root_firing.blockkit.json` from 8.1, **plus** a red `#a30200` bar down the left edge. | No colour bar → attachments are no longer rendering, and §H.2's peripheral-vision cue is gone. Report it: ADR 0008 is built on the colour bar being the only thing that answers "do I need to act?" at a glance. |
| 2 | **The push notification is a sentence** | Lock your phone. Fire the alert. Read the banner **without unlocking**. | A complete sentence: severity, what, where, since when. Compare with `"text"` in `root_firing.message.json`. | The banner shows only the app name, or a fragment, or ends `…​.` — the top-level `text` is not doing its job, and it is the only thing a screen reader reads. |
| 3 | **`chat.update` edits in place** (ADR 0008) | Acknowledge, then resolve. Watch the channel — do not open the thread. | **One** message, changing colour and content: red → amber → green. No new message. The `ts` in oto's `channel_threads` row never changes. | A second card appears → the update path fell back to posting. A card that stops changing → check for `cant_update_message`, which is what a **rotated bot token** produces: only the token that posted a message may edit it. |
| 4 | **Threads carry the detail** | Open the thread on the card. | Replies are threaded under the root, in order, and no reply ever appears as a top-level channel message. | A reply in the channel body → `thread_ts` was omitted. Replies nested under each other → oto threaded off a reply's `ts` instead of the root's. |
| 5 | **`reply_broadcast` surfaces a reply** (ADR 0020) | Let a resolved alert re-fire so `refired` is delivered. | The reply appears **in the channel** as well as in the thread. | Only in the thread → `reply_broadcast` is not being set. ⛔ This step used to use the unacked reminder and its mention audience; both were removed (git-bug `bd0fb1d`) and the mention half was **never once observed working** (`2078a07`), which is part of why it went. |
| 6 | **The in-channel broadcast copy** | Look at the broadcast **in the channel body**, not in the thread. | Record exactly three things: does the **colour bar** show? do the **buttons** show? does the **top-level text** show in full? | Slack documents the `thread_broadcast` reference as carrying neither attachments nor buttons. ADR 0020 Amendment 4 claims the attachment survives and the buttons do not. **That claim is currently unverifiable from this repository** — see 8.3. Whatever you observe, write it down; it is the evidence Amendment 4 is missing. |

### 8.3 One thing to settle while you are there

ADR 0020 **Amendment 4** and several code comments in
`internal/channels/render/slack/` describe observations from the *"first live
Slack run"* — a `conversations.history` read, and a human looking at a message in a
client. That run is `a7cdec3` (2026-08-09), and git-bug **edb670f** — which said no
workspace had ever been connected — was closed against it. The ambiguity that used
to sit here is settled.

What is not settled is how far that one run reaches. **It is one client and one
observer**, and it is the half of the evidence that *contradicts* Slack's own
documentation, which is the half most likely to differ between clients or revert in
a release. Amendment 4 records that as an open unknown in its own words. If your run
contradicts it, say so in the ADR; if it confirms it, say on which clients. Two of
ADR 0020's binding rules point here.

### 8.4 What is still unverifiable after all of the above

These have no documented answer and no offline test. They are listed so nobody
mistakes their absence for a passing result.

- **Attachment block limit.** Slack documents 50 blocks per *message* and states
  nothing for an attachment's blocks. oto applies 50 anyway. Its own budget is
  seven, so it has never been near it.
- **`metadata_too_large` size.** The error is documented; the number is not,
  anywhere. oto guesses 8 000 bytes.
- **Total request size.** Undocumented. oto guesses 100 000 bytes.
- **Threading off a reply's `ts`.** Slack says *"avoid it"* and does not say what
  happens. oto never does it, which is the only defence that does not depend on
  knowing.
- **Whether a broadcast reply counts against the workspace-wide posting limit.**
  Undocumented either way. oto's rate limiter is **per conversation only** and
  models no workspace-wide bucket at all.

---

## Why there is no Socket Mode

Socket Mode is the transport most self-hosted installs would prefer: oto would
dial **out** to Slack over a WebSocket and receive button presses on it, with no
ingress rule, no TLS certificate and no public URL. The documentation used to say
that was the default, and the manifest used to ship `socket_mode_enabled: true`.
**It was never built** — there is no WebSocket client in oto, and
`OTO_SLACK_APP_TOKEN` has no reader. The manifest now ships
`socket_mode_enabled: false` and a Request URL, which is the only combination
that produces a working Acknowledge button.

Until it exists, interactivity means an HTTPS Request URL and a signing secret.
If you cannot expose one, run oto without buttons: alerts, grouping, cards,
threads and delivery all work unchanged, and you acknowledge from oto's own UI.

Two related leftovers you may notice, both inert:

- **`OTO_SLACK_MODE=socket`** is still the default and still accepted. It
  disables the HTTP interactions endpoint and enables nothing in its place.
- **`transport` in a channel's config** (`socket_mode` | `http`) is accepted by
  the config schema and read by nothing. It was never a per-channel decision:
  one oto process either has a Request URL or it does not. Leave it out of new
  channel configs; existing ones are harmless.
