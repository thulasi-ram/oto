/**
 * The Tools an Investigator's allowlist may name that oto itself provides.
 *
 * ⚠️ THIS IS A MIRROR, NOT A READ OFF THE CONTRACT. The API has no route that lists the built-in
 * Tools (a ToolServer's are listed by `GET /tool-servers/{id}/tools`), so these names are
 * written here from `docs/setup/investigators.md` §3. The server is still the authority: it refuses
 * any name it does not hold (`422 investigator_tools_invalid`, naming each), and the form shows that
 * refusal. A name saved earlier that is not in this list still appears in the picker, checked, so it
 * can be removed instead of silently carried forward.
 *
 * Not on any allowlist: `oto_classify`, which is offered whenever the org has written classes.
 */
export interface BuiltinTool {
  readonly name: string;
  readonly help: string;
}

/** Read oto's own history; they call no ToolServer. */
export const READING_TOOLS: readonly BuiltinTool[] = [
  { name: "oto_case_timeline", help: "The Case's timeline." },
  { name: "oto_rule_at_fire", help: "The alerting rule as it stood when the Case fired." },
  { name: "oto_prior_findings", help: "Earlier Findings about the same alert, or a digest's earlier windows." },
  { name: "oto_member_findings", help: "An Incident's member Cases' earlier Findings." },
  { name: "oto_digest_cases", help: "A digest window's Cases, read again at the call." },
];

/** Propose; a human decides. None of them changes anything by itself. */
export const PROPOSING_TOOLS: readonly BuiltinTool[] = [
  { name: "oto_suggest_count_condition", help: "Propose a Case-count condition; a human applies it." },
  { name: "oto_suggest_membership", help: "Propose moving a Case between Incidents; a human applies it." },
  { name: "oto_write_tools", help: "List the write Tools a Remedy may name. Calls none." },
  { name: "oto_propose_remedy", help: "Propose a cluster change; two people approve it unless your rules say one." },
];

export const KNOWN_BUILTIN: ReadonlySet<string> = new Set(
  [...READING_TOOLS, ...PROPOSING_TOOLS].map((t) => t.name),
);
