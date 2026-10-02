# The payload-mapping catalog

A folder of **payload mappings** for incident tools, one file per tool
([ADR 0055](../docs/adr/0055-an-incident-tool-integration-is-data-or-a-bridge-never-code-in-oto.md) §2).
A mapping turns oto's `oto.notification.v1` envelope into the request a tool's API expects; the
format, the Liquid it is written in and `{{ secrets.<name> }}` are described in
[docs/setup/webhook.md §7](../docs/setup/webhook.md#7-reaching-a-tool-that-wants-a-different-shape-the-payload-mapping).

The files are embedded in the oto binary. Settings → Connections lists them on a webhook connection
and **imports one by copying it** into that connection's mapping: the copy is the connection's own
document from then on, and a later change to this folder never alters a live connection. Importing
copies data only — the tool's key or token is set on the connection by you, as a credential or a
mapping secret, and never comes from here.

| File | Tool | Checked against the vendor's docs on |
|---|---|---|
| [`incident-io.yaml`](incident-io.yaml) | incident.io HTTP alert source | 2026-10-02 |
| [`pagerduty.yaml`](pagerduty.yaml) | PagerDuty Events API v2 | 2026-10-02 |

## What a catalog file is

YAML, so a reviewer can read the Liquid as it is written rather than as an escaped JSON string, and
so the file can carry its own comments. `mapping:` is exactly the document a connection stores — the
same `body`, `facts`, `headers` and `response` keys, nothing more — and the rest is about it:

| Key | Required | Meaning |
|---|---|---|
| `vendor` | yes | The tool, as its vendor writes it. |
| `title` | yes | One line naming the API the mapping speaks to. |
| `summary` | yes | What the mapping does with oto's facts, for the import list. |
| `docs` | yes | The vendor's public docs the field names were checked against. `https://` only. |
| `checked_on` | yes | When they were checked, `YYYY-MM-DD`. Re-check and bump it whenever you edit a field name. |
| `setup` | yes | What the operator does in the tool and on the connection, in order. |
| `commands` | yes, at least one | The tool's **command fields** and the values that would make a fact a command. See below. |
| `choices` | no | Values the tool requires that only the operator may choose. Each has a `name`, a `question` the import asks, the body `field` it decides (a gjson path) and at least two `options` (plain tokens). The mapping writes `<<choose:<name>>>` where the pick goes; see below. |
| `secret_fields` | no | Body fields that carry a vendor key and must render as a `{{ secrets.<name> }}` reference. |
| `mapping` | yes | The payload mapping itself. |

## The rule a catalog file is held to

> **oto's own code sends no command, and no mapping in oto's catalog turns a fact into a resolve,
> close or status change**; a catalog review refuses one. — ADR 0055 §4

An operator may write that mapping for themselves. The catalog never ships one, because `quiet` is
not `fixed`: a mapping that resolves on `quiet` ends a response the moment the signals stop —
including the times they stopped because the thing emitting them died.

A human reviewer reading a Liquid branch on `reason == "quiet"` can miss a `"resolve"` three lines
down, so the rule is checked by a test,
`internal/channels/service/catalog_test.go`, and **every catalog file must pass it**:

1. **It declares its command fields.** `commands` lists each field of the request body (a
   [gjson](https://github.com/tidwall/gjson/blob/master/SYNTAX.md) path) that the tool reads as an
   instruction, and the values of it that resolve, close, acknowledge or otherwise change an
   incident's status. PagerDuty's is `event_action` ∉ {`resolve`, `acknowledge`}; incident.io's is
   `status` ≠ `resolved`. A file that declares none is refused, not passed, so a contributor has to
   name the field and a reviewer has to agree it is the right one.
2. **No forbidden value is spelled in the mapping at all.** Not as a JSON value, not in a Liquid
   comparison, not in a branch nobody can reach — compared as a whole word, ignoring case. A mapping
   that cannot write the word cannot send it, and nobody has to decide whether a branch is reachable.
3. **Every fact renders, and none renders a command.** The mapping is rendered against the same
   corpus oto's save-time gate uses — one envelope for every one of the 20 facts, plus hostile and
   empty ones — and in every rendered body each command field must be **present** and must not
   equal a forbidden value. Present, because leaving the field out hands the choice to the tool's
   default.
4. **It would be accepted by a connection.** It passes the same save-time validation an operator's
   mapping does, given the secrets it names.
5. **A key is a reference.** Each `secret_fields` entry renders as exactly the secret it names, and
   nothing else, on every fact.
6. **A choice is the operator's, and the file does not answer it.** A file with `choices` is
   checked once per option — every copy an import could store passes 3 to 5 — and on every fact the
   choice's `field` must be one of its options and must either **follow the pick** or **pass through
   a value the fact itself carries**, the same whatever is picked. A value that is the same whatever
   is picked and that the fact does not carry is a fallback the file chose, and is refused; so is a
   choice no fact ever sends, and one that decides a command field.

The check is the same for every vendor — it knows nothing about PagerDuty or incident.io beyond what
the file declares. What it cannot know is whether the declaration is complete: that is the review.

Importing does not re-run the check. Once a mapping is copied onto a connection it is the
operator's, and editing it into a command is a rule the operator wrote (§4).

## Choices: what the catalog may not decide

PagerDuty requires a severity, and an Incident's labels do not always give one. oto invents none,
and neither may a catalog file (owner ruling, 2026-10-02): `pagerduty.yaml` passes a member's
`severity` label through when it is one of PagerDuty's four values and otherwise writes
`<<choose:default_severity>>`. *Import from the catalog* asks the operator to pick a default
severity and writes the **literal** pick in place of every placeholder, so the connection's copy is
plain text with no question left in it. A copy that still holds a placeholder — say, one stored
over the API without asking — is refused at save (`choice_unfilled`).

## Which facts a starter should hear

Both starters key the tool's alert on `incident.id` and send every fact as a further trigger. Once a
human has resolved the incident in the tool, a later trigger on that key opens a new incident and
pages again. A mapping cannot decline a fact, so each starter's `setup` says to add a notification
policy for its channel whose reasons are **only** `drawn` and `active_again`. A new starter for a
tool that behaves the same way says the same.

## Adding a tool

One file. Verify every field name against the vendor's **current public docs**, cite the URLs in
`docs` and in the header comment, set `checked_on`, declare the command fields, and put any key the
body carries behind `{{ secrets.<name> }}`. A tool that needs OAuth, several calls per fact, or
logic is not a mapping: it is a bridge you run yourself (ADR 0055 §3, and
[docs/setup/incident-tools.md](../docs/setup/incident-tools.md)).
