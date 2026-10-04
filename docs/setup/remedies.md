# Remedies

A **Remedy** is a change to a cluster that an Investigator proposes with its Finding, and that oto
executes through one of **your** write Tools only after two different people approve it
([ADR 0054](../adr/0054-a-remedy-earns-the-write-path.md)). It is the one place oto writes to a
cluster, and this page is what you need to set one up and to know what each part does.

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
   86400 seconds, default 3600 — see [tuning](tuning.md).

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
(see [the webhook envelope](webhook.md)). They are **facts**: approval happens in oto, and oto
reads nothing back from your incident tool.
