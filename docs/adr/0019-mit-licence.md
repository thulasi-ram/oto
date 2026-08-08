# 0019 — MIT licence

**Status:** Accepted · 2026-08-08
**Resolves:** red-team memo §7, open decision **4** — *"What is the licence, exactly?"*
**Artefact:** `LICENSE` (MIT, committed)

## Context

oto is self-hosted software that runs inside a customer's cluster and reads their alerting stack. The
buyer is a platform engineer, the install is approved by someone's legal or security function, and the
first question that function asks is what the licence obliges them to do.

The competitive field is permissive, and the earlier assumption that it was not turned out to be wrong:

- **Keep**'s core is **MIT**, with a proprietary `ee/` directory holding AIOps correlation, RBAC, SSO
  and HA.
- **Robusta OSS** is **MIT**. Its web UI is not open source at all — self-hosting it requires an
  enterprise plan — but the OSS core carries no copyleft obligation.
- **Grafana OnCall OSS** was **AGPLv3**. It entered maintenance mode on 2025-03-11 and was **archived on
  2026-03-24**, with Cloud Connection and its APIs switched off. Grafana's replacement is a hosted paid
  product.

The last point is the one that matters most, and not for the reason it first appears. The archived
AGPL project is the vacuum oto is aimed at (ADR 0013's context; memo §1). Its users are, right now,
looking for somewhere to go. Asking them to move from one AGPL project to another AGPL project offers
them nothing they did not already have, while asking them to re-run the legal review they ran years ago.

Copyleft's actual protection is against a hyperscaler taking the code and offering it as a managed
service. That risk is essentially zero for a project with no users, and becomes non-zero only after
success. Its cost, by contrast, is paid immediately and by exactly the population oto needs first:
corporate self-hosters whose legal teams have a standing position on AGPL. The protection is deferred
and speculative; the friction is immediate and certain.

The licence decision is also close to irreversible. Once outside contributors exist, relicensing
requires every one of their consents.

## Decision

**oto is MIT-licensed.** `LICENSE` is committed at the repository root.

This is the whole decision. There is no `ee/` directory, no proprietary module, no feature behind a
licence key, and no CLA. If a commercial layer is ever added, three things bind it, recorded here so
that a later decision inherits them rather than re-deciding them:

1. **The open-core line, if drawn at all, falls on organisational scale** — SSO, RBAC, multi-cluster
   fleet management, audit export. Never on correctness or safety. **Rate limiting, deduplication,
   storm damping, flap damping, delivery reliability, retention controls and redaction are never
   paywalled.** Paywalling safety is how open-core products lose the community that made them worth
   buying.
2. **The self-hosted product is never degraded to sell a hosted one.** Self-hosted-first is the
   commercial position and the moral one; this category's buyers are structurally sceptical of shipping
   cluster telemetry to a third party.
3. **No code is copied from Keep or Robusta.** MIT permits reuse with attribution, but a project in this
   space silently vendoring a competitor's code is both a licence breach and a reputational failure.
   Read for ideas, write the code.

MIT rather than Apache-2.0 is a deliberately small choice. Both are permissive and either would serve.
MIT is shorter, is what a reader of this repository's peers already expects, and matches the licence the
two live competitors chose — which removes the licence as a topic of conversation entirely. Apache-2.0's
one substantive addition is its express patent grant. oto holds no patents and the project is not of a
size where that clause changes anyone's decision. If that ever stops being true, moving from MIT to
Apache-2.0 is a far easier conversation than moving away from copyleft would be.

## Consequences

- Anyone may run, fork, modify, embed or sell oto, including as a hosted service, without sharing
  changes back. This is accepted, not merely tolerated.
- Contributions arrive under MIT with no CLA, which means the project cannot unilaterally relicense
  later. That is a real constraint and it is the point: it is what makes the permissive promise credible
  rather than provisional.
- Legal review at a corporate self-hoster is a formality, which is the intended effect. The install
  decision should be about whether the product is good.
- Migrating from archived Grafana OnCall does not require a licence conversation. Given that population
  is the primary near-term audience, this is the largest practical consequence of the decision.
- oto forfeits the protection copyleft would have given against a cloud provider hosting it. If that
  becomes a live threat, the response will have to be product and trademark, not licence — the licence
  will no longer be movable.
- The competitive field is now uniform on licence, so oto cannot differentiate on openness. It has to
  differentiate on the product. Correct.

## Alternatives rejected

**AGPL-3.0.** Rejected. It buys protection against a risk that is zero today and pays for it in adoption
friction today, from precisely the corporate self-hosters oto needs first. It would also make oto the
*most* restrictive option in a field where both live competitors are MIT-cored — a strictly worse
position than either of them on a dimension the buyer actually checks. And it is the licence of the
archived project whose users oto is courting, which makes it the one licence with a specific reason not
to choose it.

**Apache-2.0.** A genuinely reasonable alternative and the closest call. Rejected only on the grounds
above: no patent portfolio makes the patent grant material, and MIT matches the field's convention. This
is a preference, not an argument, and it is recorded as such.

**BSL / Elastic-style source-available.** Rejected. It buys the same protection as AGPL while forfeiting
the "open source" label and the goodwill attached to it. For a product whose entire pitch is
"self-hosted, yours, no third party in the trust path", shipping under a licence that is not open source
undercuts the pitch at the point of sale.

**Open-core from the start** (permissive core plus a proprietary `ee/`) — what both live competitors
actually do. Rejected for v1 as premature, not as wrong. Open-core is a decision about which customers
pay, and there are no customers. Drawing that line before anyone has used the product would draw it in
the wrong place, and the constraints recorded in the Decision above are the guardrails for drawing it
later.

**No licence at all** (source published, rights unstated). Rejected. Unlicensed published source is
legally unusable by exactly the audience oto is for, and reads as carelessness.
