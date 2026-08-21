# 0047 — A channel answers to a connection, and Slack gets two scopes back

**Status:** Accepted · 2026-08-21
**Relates to:** [0018](0018-slack-distribution-model.md) (the manifest this amends), [0008](0008-slack-update-in-place-primary.md),
[0034](0034-notifications-as-a-top-level-destination.md) (where a channel is created)

## Context

A `channels` row was two things wearing one name: an org-wide credential — "the bot token for Slack
workspace T123" — and one destination — "#sre-alerts". Every Slack channel carried its own sealed bot
token, so five channels in one workspace meant the same token pasted five times into five separate
settings-screen dialogs, and there was nowhere to set the workspace up once and point several channels
at it. The settings screen that created channels was also, necessarily, the screen where the org-wide
setup happened — an admin surface and a per-destination one, sharing a form.

Two smaller defects rode along with the bigger one. First, `channels.config.team_id` and
`channels.config.conversation_id` were both required, but nothing resolved one from the other: an
operator typed a Slack channel ID by hand, copied out of Slack's own "Copy link" menu, with no
verification that it named the channel they thought it did. Second, moving channel *creation* out of
admin Settings and onto the Notification Policy screen — where an operator is already naming a
destination for a routing rule — had nowhere to put the org-wide credential if channels kept carrying
their own.

## Decision

**A `channel_connections` row is the org-wide setup — a Slack workspace's bot token and team id, or a
webhook receiver family's shared credential. A `channels` row is one destination, and references a
connection by id (`channels.connection_id`, NOT NULL). Several channels share one connection.**

- **Settings → Channels becomes Settings → Connections.** An admin sets up a Slack workspace or a
  webhook receiver family once. Nothing about one destination — a specific `#channel`, a specific URL —
  is configured there any more.
- **Channel creation moves to the Notification Policy screen.** That is where an operator is already
  naming a destination for a routing rule; requiring a trip to admin Settings for every new `#channel`
  was the cost this decision removes. Creating a channel there means picking an existing connection —
  admin setup stays admin setup, only the destination itself is created inline.
- **`channel_id` and `channel_name` resolve each other, live, for Slack.** Type a name, get the id back
  read-only; type an id, get the name back read-only. This is genuinely new behaviour, and it costs
  something specific — see below.
- **Both providers get a Connection, not just Slack.** A webhook connection may carry `none`, `basic`,
  or `bearer` (the same three kinds a channel used to carry directly, now shared across every URL under
  one connection), or a new kind, `webhook_signing_secret`: an HMAC-SHA256 secret the webhook provider
  signs the outbound JSON body with (`X-Oto-Signature: sha256=<hex>`), so a receiver can verify a
  payload actually came from oto. This is the direction ADR 0018's Slack signing secret already covers
  for interactivity; the webhook provider had nothing equivalent for its own outbound sends.
- **Migration is breaking, no backfill.** Every existing `channels` and `channel_credentials` row is
  dropped (migration `00075`). There is no way to group existing rows into connections without guessing
  which shared a workspace, and this is pre-release.

## This reopens a closed scope decision, on purpose

ADR 0018 fixed the Slack `API` interface at three methods — `chat.postMessage`, `chat.update`,
`auth.test` — and the manifest states, at length, that `channels:read`/`groups:read` and
`conversations.info` were requested, then **removed** after the first live run showed they served a
`Channel.Probe` and a `Channel.ResolveConversation` that **nothing called**. The manifest's own words:
*"A scope an app cannot use is a scope an admin approved for nothing."*

Name↔id resolution needs exactly the method that reasoning removed. `conversations.info` answers
id→name; there is no Slack method that answers name→id at all except walking `conversations.list`,
which needs the same two scopes. Restoring them is not an oversight repeating itself — the method now
has a real caller, `Provider.ResolveConversation` (`internal/channels/providers/slack/resolve.go`),
invoked from a settings-time HTTP endpoint
(`POST /api/v1/channel-connections/{id}/slack/resolve`) that a human's typing triggers. The distinction
ADR 0018 drew — "oto never reads Slack to reconstruct its own state" (C9, ADR 0008) — is unchanged:
this is metadata lookup at *configuration* time, once, when a channel is named, not a read on the
delivery path and not a probe nothing calls.

The cost is real and is not hidden: every self-hosted install that upgrades past this migration adds
two scopes to its Slack app and re-pastes `deploy/slack/manifest.yaml`, which a reviewer at that
company reads again. `channels:read`/`groups:read` remain workspace-wide metadata reads — they see
every channel's name and archive state, not just ones oto is configured against — and that breadth is
the whole reason they were cut once already. The trade this ADR makes is: a bidirectional name/id
control on the settings screen, permanently, is worth two metadata scopes, once, per install.

## Consequences

- **A shared credential is set up once.** Five Slack channels in one workspace now share one bot token,
  entered once. Rotating it rotates every channel under it in one PATCH.
- **The scope-removal narrative in `providers/slack/channel.go`, `provider.go`, and the manifest had to
  be rewritten, not appended to.** Those files stated "three methods, one scope" as settled fact in
  several places; leaving the old prose next to the new methods would have the code disagree with its
  own comments about what it does. All three are corrected in this change.
- **A connection still referenced by a channel cannot be deleted** (`ON DELETE RESTRICT` on
  `channels.connection_id`, surfaced as a `409` naming the channels, the same shape ADR 0034's
  channel/policy `409` already established one hop over).
- **The cross-table invariant "a channel's type must equal its connection's type" has no CHECK
  constraint** — two tables, and Postgres cannot express a CHECK across them. `channels/api` enforces
  it at create and update, the same way `checkRenderer` already catches a cross-provider renderer
  `channels_rend_ck` cannot see.
- **A webhook connection's shared credential is one row, one kind.** A connection needing both a
  bearer token (to authenticate INTO the receiver) and a signing secret (to let the receiver verify
  payloads FROM oto) cannot have both today — `channel_connections.credential_id` is a single FK.
  Nothing in v1 needs that combination; a connection that does would need a second credential slot,
  deliberately deferred rather than built for a case that does not exist yet.

## Alternatives rejected

**Keep the credential on the channel, add a "copy from another channel" convenience in the UI.** Copies
a value into a second row rather than sharing one; rotating the token still means finding and re-editing
every channel that copied it. Rejected: it treats the symptom (retyping) and leaves the actual defect (N
independent credentials for one workspace) in place.

**Resolve only id→name (keep `channels:read`/`groups:read` off, use `conversations.info` alone via some
other credential-free path).** There is no credential-free path to `conversations.info` — it is a scoped
Slack API call regardless of direction — so this saves nothing over the two-scope restoration and
delivers only half the control: an operator who has the name but not the id, which is the common case
when setting up a *new* channel, gets nothing.

**Leave channel creation in Settings, alongside connections.** Rejected per the redesign's own
premise: if channel creation stays in admin Settings, every new destination is an admin-settings
round trip even though the operator defining a notification policy is the one who knows which
`#channel` they want. Moving it to the Notification Policy screen is what makes the connection/channel
split pay for itself in fewer clicks, not just cleaner data.
