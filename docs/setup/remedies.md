# Remedies

A **Remedy** is a change to a cluster that an Investigator proposes with its Finding, and that oto
executes through one of **your** write Tools only after one or two different people approve it —
two unless **your** risk rules say one ([ADR 0054](../adr/0054-a-remedy-earns-the-write-path.md)). It is the one place oto writes to a
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
   86400 seconds, default 3600 — see [tuning](tuning.md).
5. **Optionally, risk rules** that let a harmless command need only one approval, and a risk model
   that may ask for two, applied from the host shell with `oto remedy-rules apply` — see
   [Risk rules](#risk-rules) below. With none, every Remedy needs two.

## What a Remedy says

**The exact command first.** A Remedy names the write Tool that would carry it out —
`<toolserver>__<tool>` — and the exact arguments it would be sent, byte for byte, with their
SHA-256. Directly under them: how many approvals it needs and **what set that** — the rule by name,
*no rule matched*, *the rules could not parse this command*, or *raised by the risk model*. Then
what it is made to (`target`), and only then the Investigator's own description of the change.
Read the command; the description is a model's words.

**Or it says that no configured Tool can carry it out.** The Investigator proposes the change it
judges right whether or not one of your Tools can make it. A Remedy that names no Tool says *"no
configured Tool can carry this out"*, **can never be approved** (the API refuses it with
`409 remedy_has_no_tool`, and the screen offers no approve control), and can still be declined; it
expires like any other. Its value is the Finding it stands on: make the change by hand, or
configure a Tool and ask for another Investigation.

## Approving

- **One or two approvals, from different people**, each holding the grant on the Remedy's
  ToolServer. How many is set when the Remedy is proposed, by your [risk rules](#risk-rules), and
  never changes after. The same person approving twice — in the UI and again from somewhere else —
  counts once (`409 remedy_already_approved`).
- **You approve the arguments you were shown.** The approval names their SHA-256; if it is not the
  Remedy's, it is refused (`409 remedy_arguments_changed`).
- **The Tool is checked again, against your configuration as it is now.** If the ToolServer was
  removed, re-declared `read`, or no longer lists the Tool, the Remedy cannot be approved
  (`409 remedy_tool_unavailable`) and says why — it is never executed against nothing.
- A person without a counting grant is refused (`403 remedy_approver_required`).
- **Decline** is any member's: saying no is the safe direction. A Remedy that is proposed, or
  approved and not yet being executed, can be declined.

When the last approval it needs lands the Remedy is `approved`, and its window starts again: it
must be executed within `remedy_approval_window_s` of its approval.

### From Slack

When the Incident is a Slack conversation, a proposed Remedy's reply in its thread says the exact
command first, then its target, how many approvals it needs and what set that, who has approved
so far, and only then the Investigator's description — with **Approve** and **Decline** under it.

- A press is applied as the **oto user your Slack account is linked to**, through the same
  approval and decline the UI makes: the grant, the window, the Tool check and the count of
  different people are the same, and approving in Slack and again in the UI counts once.
- **An unlinked Slack account is refused**, and nothing is recorded: the reply names your Slack
  member and workspace ids for whoever runs oto to link, or you decide it in oto. A linked account
  without the grant gets `remedy_approver_required`'s answer.
- **Approve asks for confirmation first.** It is not offered for a Remedy no configured Tool can
  carry out, nor for one whose arguments the reply had to cut — approve those in oto, where the
  arguments are shown whole. Decline is always offered.
- The buttons stay on the reply after the Remedy moves on; a late press is answered with why it no
  longer applies. Each transition is its own reply, as it is to every other destination.

## Execution

When the last approval it needs lands, oto enqueues the Remedy's execution in the same
transaction. The executor then:

1. **Checks everything again, against now.** An approved Remedy past its window is recorded
   `expired`. If the ToolServer was removed or re-declared `read`, or no longer lists the Tool, it
   is `failed` with `tool_unavailable`. If its arguments no longer hash to what was approved,
   `arguments_changed`. If fewer **different** approvers of these arguments than it needs still
   hold the grant — one was revoked, or disabled — `approvals_withdrawn`. In every one of these **nothing is
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

**An executed Remedy is followed up.** The transaction that records it `executed` also asks for
**one** Investigation of its Incident — the one its facts are declared to — so the timeline says
whether the change helped. A Remedy on a Case that no Incident holds is followed up on that Case.

- **The Investigator that proposed it runs it**, whether or not it is opted into Incidents: it
  chose the command, and its earlier Finding is the "before" the new one is read against.
- **Every control holds.** A switched-off Investigator or org records nothing; a spent daily
  budget records the follow-up `skipped` with reason `budget`; it waits for the concurrency; and it
  coalesces under the Investigator's minimum interval like a membership change — into a run of
  the same Investigator on the same subject that has not started yet, or after the interval since
  the last one began. Several Remedies executed close together are one follow-up.
- **A failed Remedy is followed by nothing**, and a retried execution job never asks twice.
- Its Finding goes outbound like any other Incident Finding (`finding`); it never decides whether
  anyone is notified.

## Risk rules

How many approvals a Remedy needs is set **once, when it is proposed**, from rules you write in a
YAML file and apply from the host shell with `oto remedy-rules apply` — see
[Applying the rules](#applying-the-rules). oto ships no rule: with none, every Remedy needs two.
Settings → Remedy risk and `GET /api/v1/remedy-risk-rules` show them; nothing inside oto writes them.

### What a rule says

A rule has a **name** (lower-case letters, digits, `_`, `-`; it is what the approval screen shows),
**1 or 2 approvals**, and one or more **conditions**, every one of which must hold:

| Condition | Holds when |
|---|---|
| `tool` | the Remedy's write Tool is exactly this `<toolserver>__<tool>`. |
| `verbs` | the command's verb is one of these — `delete`, `scale`, `rollout restart`, `set image`. |
| `kinds` | the command's resource kind is one of these. kubectl's plurals and short names for the built-in kinds fold onto one name (`deploy`, `deployments`, `deployment.apps` are all `deployment`); any other kind matches as written, lower-cased. |
| `namespaces` | the command **names** one of these namespaces. A command that names no namespace, or names all of them (`-A`), matches no namespace condition. |
| `reversibility` | `reversible`: the verb is one oto knows to be reversible — `rollout restart`, `rollout pause`, `rollout resume`, `rollout undo`, `scale`, `cordon`, `uncordon`. `irreversible`: any other verb, including one oto does not know, and a command with no verb. |

A rule with no condition is refused: it would match every command.

### How the rules decide

1. **A command the rules cannot parse needs two, whatever the rules say** — see below. No rule and
   no model is asked.
2. Otherwise every rule is checked. **The most severe matching rule wins**: if any matching rule
   says 2, the Remedy needs two, named after the first such rule in your order; only if every
   matching rule says 1 does it need one, named after the first of those.
3. **No rule matches: two.**

Order therefore only decides which rule is *named*, never the tier. This is deliberate: under
"first match wins", a broad rule placed above a narrow one would silently lower what the narrow one
guards — `anything in staging → 1` above `delete secret → 2` would let a secret be deleted on one
approval. Here a rule that says 2 cannot be outvoted, and a rule that says 1 lowers only what no
2-rule matches. Write broad 1-rules and carve exceptions out of them with 2-rules.

Example:

```yaml
rules:
  - name: restart-payments
    verbs: [rollout restart]
    kinds: [deployment]
    namespaces: [payments]
    approvals: 1
  - name: secrets-need-two
    verbs: [delete]
    kinds: [secret]
    approvals: 2
```

`kubectl rollout restart deployment/api -n payments` needs one approval (`restart-payments`);
the same restart in `checkout` matches nothing and needs two; `kubectl delete secret db -n payments`
needs two (`secrets-need-two`).

### What the rules read: the command

A Remedy is a write Tool and its JSON arguments. The rules read them in one of two shapes:

- **A command line** — the arguments carry `command` (a string, or an array of words) or `args` (an
  array of words). It is read as **kubectl's** command line: `kubectl` first, or one of kubectl's
  own verbs first. Words are split on spaces and tabs and **nothing is interpreted** — no quoting,
  escaping, expansion or globbing. The verb is kubectl's (two words for `rollout`, `set`, `auth`,
  `certificate`, `top`); the kind is the next word, or the type in `type/name`; `cordon`, `uncordon`
  and `drain` name a `node`; the namespace is `-n`/`--namespace`.
- **Structured arguments** — no command line. The verb is the `verb` member, the kind is `kind`
  (or `resource`), the namespace is `namespace`, each a string when present. A Tool whose arguments
  carry none of these can still be matched by a rule on its `tool`.

**Unparseable — two approvals, whatever the rules say:**

- any of `; | & $ < > ( ) { } [ ] * ? ~ # \ ' "` `` ` `` or a newline in a command line — so `sh -c …`,
  pipes, `;`, `&&`, backticks, `$(…)`, redirects, quotes and globs;
- `; | & $ < > ` `` ` `` or a newline in **any** string anywhere in the arguments, keys included;
- a program other than kubectl (`sh`, `bash`, `helm`, …);
- `--` and whatever follows it (`kubectl exec … -- …`), or `-` (standard input);
- a flag that names resources in a file (`-f`, `-k`, `--raw`), changes who kubectl runs as
  (`--as`, `--token`, `--kubeconfig`, …) or where it connects (`--server`);
- a flag oto does not know, unless written `--flag=value`, so its arity is plain;
- two kinds (`secret,configmap`, or `deploy/a secret/b`), or two different namespaces.

The Remedy records which of these it was, and the approval screen shows it.

### The risk model

You may name one of your model endpoints as the **risk model**. When — and only when — the rules
say **one** approval, oto asks it, once, whether the change should need two. It may **raise** one
to two; it can never lower anything, and a Remedy the rules said needs two is not asked about.

- **It sees only the command** — the write Tool, its exact arguments, what kubectl's verb, kind and
  namespace were read as — **its target, and the rules' verdict** (the tier and the rule). Never the
  Investigation, its Steps, its Finding, the Investigator's description, a log line or a Tool's
  answer: logs are attacker-writable, and a log line is the obvious injection path.
- **It fails closed.** An error, a timeout (30 s), an answer without token usage, or an answer that
  is not 1 or 2 leaves the Remedy at **two**, recorded as *the risk model failed* with why.
- **With no risk model named, the rules' tier stands**, and the Remedy records that none was asked.
- **Its tokens are the org's.** What it said, which endpoint and model, and the tokens it cost are
  kept on the Remedy, and those tokens **count against the org's daily token budget**
  (`investigation_daily_tokens`, [Investigators](investigators.md)) beside every Investigation's.
- **A spent budget is two.** When the day's budget is already spent, the risk model is **not
  asked**, and a Remedy the rules said needs one needs **two**, recorded as *the day's token budget
  was spent* (`approvals_set_by: risk_model_budget`). The check fails closed: a question nobody
  paid for never lets one approval stand. The budget resets at 00:00 UTC.

### Applying the rules

The rules and the risk model are one YAML file, applied from the host shell — the same place an
approval grant is given (`oto grant remedy-approver`):

```sh
oto remedy-rules apply --org acme -f rules.yaml     # replace the rules and the risk model
oto remedy-rules show  --org acme > rules.yaml      # print what stands, as a file that applies
```

**Why the shell and not the app.** A rule that says 1 lets one grant holder approve alone. If any
member could write the rules, a grant holder could write a one-approval rule and then approve their
own way through — the loophole the grant itself was moved out of the app to close. Writing a rule
is the same authority as granting a second approver, so it takes the same thing: a shell on the
host and the database credentials. No HTTP route writes a rule, and the Settings screen shows them
read-only, *managed by `oto remedy-rules`*.

The file:

```yaml
# Optional: one of the org's model endpoints, by name, asked whether a Remedy the
# rules say needs one approval should need two. Omit it for none.
risk_model: risk-checker

# Required. The whole list, in order; `rules: []` makes every Remedy need two.
rules:
  - name: restart-payments          # required; lower-case, digits, _ and -; unique
    verbs: [rollout restart]        # optional conditions — at least one is required
    kinds: [deployment]
    namespaces: [payments]
    approvals: 1                    # required; 1 or 2
  - name: secrets-need-two
    tool: k8s-write__kubectl        # a qualified write Tool, <toolserver>__<tool>
    kinds: [secret]
    reversibility: irreversible     # reversible | irreversible; omit for either
    approvals: 2
```

- **Applied whole, in one transaction.** The file replaces every rule and the risk model; a rule
  you leave out is gone. `-f -` reads the file from standard input.
- **A malformed file changes nothing** and exits non-zero, naming the problem and where it is —
  YAML that does not parse, a key oto does not know (`namespace:` for `namespaces:` would silently
  drop a condition and broaden the rule, so it is refused), a missing `rules:`, a rule that breaks
  one of the rules above (`rules/0/namespaces/0: …`), a `risk_model` that is not one of the org's
  endpoints, or an unknown org.
- On success it prints each rule with its tier, the risk model, and how many rules it replaced.
- The last apply and when are recorded and shown on the Settings screen.
- ⚠️ Treat a change to the rules like a change to who may approve. Applying them **re-tiers no
  Remedy already proposed**.

## The trust boundary is your ToolServer

oto holds no cluster credential. It holds your write ToolServer's access token, sealed, and it
sends that ToolServer the approved arguments. **A ToolServer that accepts oto's credential will run
whatever oto sends it** — oto ships no ToolServer and does not inspect yours — so **oto's approval
(one or two different holders of the grant, as your risk rules say) is the only gate oto owns.** Everything past that is what you granted the ToolServer.

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
Incident fact; the envelope's `incident.remedy` carries the transition, the exact command first, with
`required_approvals` and what set it (`approvals_set_by`, `approvals_rule`)
(see [the webhook envelope](webhook.md)). They are **facts**: approval happens in oto, and oto
reads nothing back from your incident tool.
