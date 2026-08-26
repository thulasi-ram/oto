---
title: 0016 — Cluster context arrives via MCP, on demand; never the Kubernetes event firehose
---
**Status:** Accepted · 2026-08-08
**Relates to:** [0014](/oto/adr/0014-postgres-only-no-analytical-store/) (this is the rule that protects it),
[0009](/oto/adr/0009-rule-snapshot-versioning-at-fire-time/) (snapshot-at-fire-time semantics),
[0013](/oto/adr/0013-alert-first-scope-boundary/) (oto is a flight recorder, not an observability store)
**Resolves:** the open question *"is oto k8s-enriched or only k8s-shaped?"*

## Context

All Kubernetes API enrichment was deferred: it needs cluster RBAC, it is Robusta's core competence, and
it is a large sink of effort. That left oto k8s-*shaped* — it groups by namespace and cluster and
installs by Helm — but unable to read a cluster.

The product owner's proposal is to close that gap through MCP servers and through Kubernetes events
oto has already pulled, rather than by holding cluster credentials.

This is a better answer than the one that was deferred, but it contains a trap, and the trap is the
reason this ADR exists.

## Decision

**Cluster context is fetched on demand at enrichment time, through an MCP client, from a server the
operator runs.** oto never holds cluster RBAC; the trust boundary sits at the MCP server. This is
materially easier to sell self-hosted than "give this thing cluster-reader".

Two constraints fall out of the existing enrichment design and are binding:

1. **MCP enrichers run in the async phase only**, never inside the pre-notification budget. An external
   server can be slow or down, and enrichment must never delay or block a notification.
2. **The result is a snapshot recorded at that time, not a live view.** It is stored as an
   `enrichments` row with provenance, exactly like a rule snapshot. The UI must render it as "this is
   what the cluster looked like when we asked", never as live state.

**⛔ The prohibition.** oto does not subscribe to, ingest, or store the Kubernetes Events stream.

This is the rule that keeps [0014](/oto/adr/0014-postgres-only-no-analytical-store/) true. Every argument for
Postgres-only rests on oto's data being human-scale. Kubernetes Events are machine-scale — a busy
cluster emits thousands per minute — and ingesting them would, in one decision, force a time-series
store, invert the storage architecture, and turn oto into an observability product. It would also
breach [0013](/oto/adr/0013-alert-first-scope-boundary/): a k8s Event stream is not a fact about a signal we
were told about, it is a general-purpose telemetry feed.

What we do instead: at enrichment time, fetch **only the events relevant to this alert** — same
namespace and object, within a bounded window — and store that bounded set as the enrichment payload.

## Consequences

- Cluster enrichment requires the operator to run an MCP server. That is a real onboarding step and it
  must be honestly documented, not hidden. Alerts remain fully useful without it.
- Enrichment quality depends on a third party's availability. Partial failure is already normal in the
  pipeline, so a missing enrichment degrades a card rather than failing a notification.
- Because results are snapshots, an alert investigated a week later shows what was true at fire time.
  This is a feature — it is the same property that makes rule snapshots the differentiator — but the UI
  must timestamp it explicitly or users will read it as current.
- We are choosing not to compete with Robusta on depth of cluster enrichment. That is a real
  competitive concession and should be stated plainly rather than papered over.

## Alternatives rejected

**An in-cluster agent that ships events to oto.** This is Robusta's model and it gives the richest
context. Rejected for v1: it makes oto a distributed system with an agent to version, deploy and
secure, and it is the path that leads directly to the firehose problem above.

**Direct Kubernetes API access with a cluster-reader ServiceAccount.** Simpler than MCP and needs no
extra server. Rejected because it puts cluster credentials inside oto, which is the single biggest
objection a security reviewer will raise against a self-hosted alerting tool.

**Storing the event firehose and querying it at enrichment time.** Rejected above; it is the decision
that would quietly invalidate the storage architecture.
