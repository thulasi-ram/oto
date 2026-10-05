---
title: Remedies
---
A **Remedy** is a change to a cluster that an Investigator proposes with its Finding, and that oto
executes through one of **your** write Tools only after two different people approve it
([ADR 0054](/oto/adr/0054-a-remedy-earns-the-write-path/)). It is the one place oto writes to a
cluster, and this page is what you need to set one up, to know what each part does, and to know
where oto's control ends and your ToolServer's begins.

```
proposed ──► approved ──► executed | failed
    │            │
    ├──► declined ◄┘
    └──► expired ◄─┘
```

Every transition is made by a named actor — the Investigator proposes, a person approves or
declines, oto (`system`) expires — and every transition is sent to the Incident as a fact.

## What you configure

1. **A write ToolServer.** oto ships no ToolServer: a Remedy runs through an MCP server **you** run,
   configured in oto with `access: write`. Only a `write` ToolServer's Tools can carry out a Remedy,
   and **no Investigator ever holds one** — an allowlist naming a write Tool is refused, and a run
   is never offered one. Discover it once, so oto knows the Tools it lists.
2. **Who may approve.** Approval is a grant held **on that ToolServer**, given and taken only from
   the host shell:

   ```sh
   oto grant remedy-approver --org <slug> --toolserver <name> --email <address>
   oto revoke remedy-approver --org <slug> --toolserver <name> --email <address>
   ```

   No route in oto can create, change or delete a grant — an in-app grant would let one holder mint
   a second approver and approve alone. `GET /api/v1/tool-servers/{id}/remedy-approvers` lists the
   holders, read-only.
3. **An Investigator that may propose.** Add `oto_propose_remedy` to its Tool allowlist, and
   `oto_write_tools` so it can read which write Tools exist and what arguments they take. Both are
   built in; neither calls a ToolServer.
4. **How long a Remedy waits.** `remedy_approval_window_s` (Settings → Tuning → Remedies), 60 to
   86400 seconds, default 3600 — see [tuning](/oto/setup/tuning/).

## What a Remedy says

**The exact command first.** A Remedy names the write Tool that would carry it out —
`<toolserver>__<tool>` — and the exact arguments it would be sent, byte for byte, with their
SHA-256. Below them: what it is made to (`target`), and only then the Investigator's own
description of the change. Read the command; the description is a model's words.

**Or it says that no configured Tool can carry it out.** The Investigator proposes the change it
judges right whether or not one of your Tools can make it. A Remedy that names no Tool says *"no
configured Tool can carry this out"*, **can never be approved** (the API refuses it with
`409 remedy_has_no_tool`, and the screen offers no approve control), and can still be declined; it
expires like any other. Its value is the Finding it stands on: make the change by hand, or
configure a Tool and ask for another Investigation.

## Approving

- **Two approvals, from two different people**, each holding the grant on the Remedy's ToolServer.
  Until operator-written risk rules exist, every Remedy needs two. The same person approving twice
  — in the UI and again from somewhere else — counts once (`409 remedy_already_approved`).
- **You approve the arguments you were shown.** The approval names their SHA-256; if it is not the
  Remedy's, it is refused (`409 remedy_arguments_changed`).
- **The Tool is checked again, against your configuration as it is now.** If the ToolServer was
  removed, re-declared `read`, or no longer lists the Tool, the Remedy cannot be approved
  (`409 remedy_tool_unavailable`) and says why — it is never executed against nothing.
- A person without a counting grant is refused (`403 remedy_approver_required`).
- **Decline** is any member's: saying no is the safe direction. A Remedy that is proposed, or
  approved and not yet being executed, can be declined.

When the second approval lands the Remedy is `approved`, and its window starts again: it must be
executed within `remedy_approval_window_s` of its approval.

## Execution

When the second approval lands, oto enqueues the Remedy's execution in the same transaction. The
executor then:

1. **Checks everything again, against now.** An approved Remedy past its window is recorded
   `expired`. If the ToolServer was removed or re-declared `read`, or no longer lists the Tool, it
   is `failed` with `tool_unavailable`. If its arguments no longer hash to what was approved,
   `arguments_changed`. If fewer than two **different** approvers of these arguments still hold the
   grant — one was revoked, or disabled — `approvals_withdrawn`. In every one of these **nothing is
   sent**, and the Remedy says so.
2. **Claims it.** The Remedy moves `approved → executing`, and that is committed **before** the
   write Tool is called.
3. **Calls your write Tool once**, through the MCP client, with the stored arguments — the exact
   bytes the approvers approved by hash, never decoded and re-encoded — inside the ToolServer's own
   per-call timeout.
4. **Records what came back.** `executed` when the Tool did not report a failure; `failed` with
   `tool_error` when it did. Either way the answer is kept with your redaction rules applied, the
   ToolServer's token scrubbed, and capped at 16 KiB — on the Remedy, not on the Incident fact.

**At most once.** Because the claim is committed before the call, a retried or duplicated job
finds the Remedy `executing` (or ended) and sends nothing. A worker that dies mid-call leaves it
`executing`; once seven minutes have passed with no answer, `remedies.sweep` records it `failed`
with **`outcome_unknown`** — the change may or may not have been made, and the record says exactly
that. A broken connection or a timeout during the call is `outcome_unknown` for the same reason.
A ToolServer that could not be reached at all is `tool_unavailable`: no session, so no call.

**A failed Remedy is never retried.** `failed` is final. If the change still matters, ask for
another Investigation: a retry is a **new** Remedy and a **new** approval.

## The trust boundary is your ToolServer

oto holds no cluster credential. It holds your write ToolServer's access token, sealed, and it
sends that ToolServer the approved arguments. **A ToolServer that accepts oto's credential will run
whatever oto sends it** — oto ships no ToolServer and does not inspect yours — so **oto's approval
(two different holders of the grant) and, once they exist, its risk rules are the only gate oto
owns.** Everything past that is what you granted the ToolServer.

So:

- **Give a write ToolServer narrower RBAC than a read one.** Run it under its own
  ServiceAccount, separate from your read ToolServer's, with exactly the verbs, resources and
  namespaces the Remedies you want to allow need — `patch` on `deployments` in the namespaces you
  name, say, not `*` on `*`. The read ToolServer may see more; the write one should be able to
  change less.
- **Expose only the Tools you mean.** Every Tool a `write` ToolServer lists is one a Remedy may
  name. A Tool that runs an arbitrary command or shell is an arbitrary change, approved by two
  people reading it.
- **Keep its token for oto alone**, and rotate it like any credential that can change a cluster.
- **Grant approval narrowly.** `oto grant remedy-approver` per ToolServer, to the people who
  should be able to change what that ToolServer can change.

## Expiry

A proposed Remedy that has not had its approvals within the window, or an approved one oto has not
executed within the window after its approval, **expires**. It reads as expired the moment the
window passes — nothing can approve, decline or execute it from then — and the `remedies.sweep`
job records the transition (by `system`) within a minute and declares it. Recorded, never silent.

## What goes outbound

Each transition is an Incident fact — `remedy_proposed`, `remedy_approved`, `remedy_declined`,
`remedy_expired`, `remedy_executed`, `remedy_failed` — sent to the Incident the Remedy is about,
or to the Incident holding its Case at the time. A Remedy on a Case that is in no Incident is
declared nowhere; its transitions record that. Route them with a notification policy like any
Incident fact; the envelope's `incident.remedy` carries the transition, the exact command first
(see [the webhook envelope](/oto/setup/webhook/)). They are **facts**: approval happens in oto, and oto
reads nothing back from your incident tool.
