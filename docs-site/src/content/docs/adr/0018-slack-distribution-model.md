---
title: "0018 — Slack distribution: a customer-built internal app, credentials in config"
---
**Status:** Accepted · 2026-08-08
**Relates to:** [0008](/oto/adr/0008-slack-update-in-place-primary/) (`chat.update` is the primary verb, and
oto never reads Slack back), [0013](/oto/adr/0013-alert-first-scope-boundary/) (oto tells the thing that
pages; it does not page)
**Artefacts:** `deploy/slack/manifest.yaml`, `docs/setup/slack.md`

## Context

oto is self-hosted. Every install runs in someone else's cluster, behind someone else's firewall,
against someone else's Slack workspace. There is no oto-operated service anywhere in the picture — no
control plane, no hosted tier, no shared hostname. That single fact decides the Slack integration, and
it decides it more firmly than it first appears.

A Slack integration can be distributed three ways: as a Marketplace app installed by OAuth, as an
unlisted app distributed by OAuth outside the Marketplace, or as an **internal app the customer creates
in their own workspace**. The first two require the app to have a stable identity — a client ID, a
client secret, a redirect URL, and an events/interactivity request URL. Every one of those is a
property of *the operator of the app*, and oto has no operator. There is no host to redirect to.

Two further facts, both verified, narrow this further:

1. **Socket Mode removes the public-ingress requirement entirely**, and Socket Mode is
   [not permitted for Slack Marketplace apps](https://docs.slack.dev/apis/events-api/comparing-http-socket-mode/).
   The overwhelming majority of self-hosted installs have no inbound HTTPS. Choosing a distribution
   model that forbids Socket Mode is choosing to make onboarding an ingress-and-certificate ticket.
2. **The May 2025 rate-limit change for non-Marketplace apps** cut `conversations.history` and
   `conversations.replies` to 1 request/minute with 15-object pages — but explicitly exempted
   *"internal customer-built apps"*
   ([clarification, 2025-06-03](https://docs.slack.dev/changelog/2025/06/03/rate-limits-clarity/)).
   It is the loudest recent argument against unlisted OAuth distribution, and it does not touch the
   model chosen here.

The real cost of the internal-app model is that the customer builds the app themselves, which means
ticking scopes by hand. That is the error-prone step, and its failures are the worst kind: silent at
setup, then a `missing_scope` at 3am.

## Decision

**Slack integration is a customer-built internal app. The customer creates the app in their own
workspace, installs it there, and pastes the resulting credentials into their own oto config.**

- **No OAuth.** No client ID, no client secret, no redirect URL, no `oauth.v2.access` exchange, no
  installation store, no per-workspace token table keyed by an install event. None of that code exists
  and none of it should be written.
- **No token refresh.** `token_rotation_enabled: false`. Rotation machinery exists to serve
  OAuth-distributed apps; an internal app's bot token does not expire.
- **Three credentials, entered by a human:** the bot token (`xoxb-`, per channel, sealed in
  `channel_credentials` with AES-256-GCM against the `platform/secrets` keyring), the app-level token
  (`xapp-`, process config, Socket Mode only), and the signing secret (process config, HTTP mode only).
- **Setup is driven by a checked-in manifest**, `deploy/slack/manifest.yaml`, pasted into Slack's
  "Create an app from manifest" flow. The manifest is the mitigation for the one real cost of this
  model, and it is treated as a first-class artefact: every scope it requests carries a comment naming
  the Slack API method that requires it and the documentation URL that proves it, and the scopes
  deliberately *not* requested are listed with reasons. A reviewer at the customer's company reads that
  file before approving the install.
- **The scope list is derived from the code, not from convention.** The complete set of Slack methods
  oto can call is fixed by a four-method interface in `internal/channels/providers/slack/channel.go`:
  `chat.postMessage`, `chat.update`, `auth.test`, `conversations.info`. That yields `chat:write`,
  `channels:read` and `groups:read`. Anything not implied by those four methods is refused, including
  `chat:write.public` — oto must be invited to a channel by a human rather than being able to post into
  any public channel in the workspace.
- **Socket Mode is the default transport**; HTTP is a configuration flag for installs that already have
  ingress. Both run behind one handler.

## Consequences

- **No OAuth machinery is needed at all.** This is the largest consequence and it is a reduction: a
  whole subsystem — installation records, state parameters, redirect handling, token refresh, revocation
  webhooks, the multi-tenant token store keyed by install — simply does not exist. It is also a
  reduction in attack surface. There is no redirect URL to attack and no client secret to leak.
- **Socket Mode stays viable, and that is worth more than it looks.** oto runs inside a cluster. An
  integration that required a public inbound URL would turn "install oto" into a conversation with the
  network team about an ingress rule and a TLS certificate, and it is the single largest onboarding cost
  the product could have chosen to take on. Instead oto dials out. It also removes an entire class of
  failure: no signature-verification path in the default deployment, and no Slack-side auto-disable of
  event subscriptions when a deploy makes the endpoint briefly unavailable.
- **The customer bears the app-creation cost.** They must create an app, install it, generate an
  app-level token by hand (app-level token scopes cannot be declared in a manifest — the manifest schema
  covers only bot and user OAuth scopes), copy three values, and invite the bot to each channel. This is
  a real cost and it is not hidden. The manifest removes the error-prone part of it; the rest is
  documented step by step in `docs/setup/slack.md`.
- **The 2025 read-throttle change is moot twice over.** Internal customer-built apps were explicitly
  exempted, *and* oto never calls the throttled methods: `conversations.history` and
  `conversations.replies` are absent from the provider's API interface by design, because oto's own
  database is the memory of Slack (ADR 0008). Even if the exemption were withdrawn tomorrow, nothing in
  oto would change.
- **oto cannot be listed on the Slack Marketplace**, and cannot offer a one-click "Add to Slack" button.
  Accepted without regret: a Marketplace listing is a distribution channel for a hosted product, and oto
  is not one.
- **A support conversation about scopes is now a conversation about a file in the repository**, which is
  a much better conversation than one about a screenshot of a checkbox list.
- **Enterprise Grid org-wide deployment is possible but untested.** `org_deploy_enabled: false` in the
  manifest. Socket Mode apps can be distributed org-wide through enterprise deployment options; nothing
  here forecloses it, and nothing here validates it either.

## Alternatives rejected

**Slack Marketplace listing with OAuth distribution.** Near-incoherent for a self-hosted product. OAuth
requires a redirect URL and an interactivity request URL, and for a self-hosted install both would have
to be *the customer's own hostname* — one per install. That is not a distributed app; it is a
per-customer app wearing an OAuth costume, with all of OAuth's machinery and none of its benefit. It
would additionally forbid Socket Mode (a Marketplace requirement), forcing every install to expose
public HTTPS ingress, and it would put oto's release cadence behind Slack's app-review queue. Rejected
on all four counts.

**Unlisted OAuth distribution (a "distributed" app outside the Marketplace).** Same redirect-URL
incoherence as above, with the additional penalty that it lands squarely in the population the May 2025
rate-limit change targeted — *"commercially distributed apps that are not approved for the Slack
Marketplace"*. oto happens not to call the throttled methods, so the penalty would be nominal today, but
adopting a model whose limits are being actively tightened, in exchange for nothing, is a bad trade.

**Incoming webhooks.** The tempting one, because it is the simplest possible setup: one URL, no scopes,
no token. Rejected because it cannot do the product. An incoming webhook can only *post*. It cannot
`chat.update`, which is ADR 0008's primary verb and the entire reason oto is quieter than stock
Alertmanager — every state change would become a new message, which is precisely the noise oto exists
to eliminate. It cannot thread properly (no `ts` is returned, so there is no handle to thread off or to
edit). And it carries no interactivity, so there is no Acknowledge button, and therefore no ack
attribution and no `slack_identities`. Choosing webhooks would not be a simpler implementation of oto;
it would be a different, worse product.

**Ship a hosted oto that owns the Slack app.** Out of scope. It would resolve the redirect-URL problem
by making oto a SaaS, which is a business decision, not an integration one, and one nobody has made.
