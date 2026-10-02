/**
 * Filling a catalog entry's choices into the copy an import stores (owner ruling
 * of 2026-10-02, ADR 0055 §2).
 *
 * ⛔ THE COPY HOLDS THE PICK, NOT THE QUESTION. A catalog entry that leaves a value
 * to the operator — PagerDuty's severity when no label gives one — writes
 * `<<choose:<name>>>` where the value goes and lists the choice beside it. Import
 * asks, and writes the LITERAL pick in place of every placeholder, so the
 * connection's mapping is plain text the operator can read and edit. The server
 * refuses a mapping that still holds a placeholder (`choice_unfilled`), so a copy
 * made without asking cannot be saved by accident.
 */
import type { PayloadMappingCatalogEntry } from "~/api/types";

const PLACEHOLDER = /<<choose:([^<>]*)>>/g;

/** The picks an entry still needs, by choice name. */
export function unansweredChoices(
  entry: PayloadMappingCatalogEntry,
  picks: Readonly<Record<string, string>>,
): readonly string[] {
  return entry.choices
    .filter((c) => {
      const pick = picks[c.name];
      return pick === undefined || !c.options.includes(pick);
    })
    .map((c) => c.name);
}

/**
 * The entry's mapping with every placeholder replaced by its pick. A placeholder
 * with no pick is left as it is, which the server then refuses at save rather
 * than sending it to the tool.
 */
export function fillChoices(
  entry: PayloadMappingCatalogEntry,
  picks: Readonly<Record<string, string>>,
): unknown {
  const walk = (v: unknown): unknown => {
    if (typeof v === "string") {
      return v.replace(
        PLACEHOLDER,
        (whole, name: string) => picks[name] ?? whole,
      );
    }
    if (Array.isArray(v)) return v.map(walk);
    if (v !== null && typeof v === "object") {
      return Object.fromEntries(
        Object.entries(v).map(([k, x]) => [k, walk(x)]),
      );
    }
    return v;
  };
  return walk(entry.mapping);
}
