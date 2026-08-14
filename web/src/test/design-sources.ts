/**
 * The two documents §M is written in, and the parsing that makes them comparable.
 *
 * §M.4 and §M.5 are not prose about colour: they are a CSS block and a table of
 * measured contrast ratios, and `web/src/design/tokens.css` is supposed to be
 * that CSS block. Two files, one source of truth, and until `tokens.test.ts` and
 * `contrast.test.ts` were written nothing compared them — a token could be added
 * to one theme and not the other, or edited in the stylesheet and not in the
 * SPEC, and the only thing that would notice was somebody's eye.
 *
 * ⛔ EVERY VALUE READ HERE COMES OFF DISK. Nothing in this file restates a hex,
 * a token name or a ratio. A helper that carried its own copy of the palette
 * would be a third place to edit, and the gate would then assert that two of the
 * three agree.
 */
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));

/** `web/src/test` → the repository root. */
export const REPO_ROOT = path.resolve(HERE, "..", "..", "..");

export const SPEC_PATH = path.join(REPO_ROOT, "docs", "design", "SPEC.md");
export const TOKENS_PATH = path.join(REPO_ROOT, "web", "src", "design", "tokens.css");

export const SPEC = readFileSync(SPEC_PATH, "utf8");
export const TOKENS_CSS = readFileSync(TOKENS_PATH, "utf8");

export type Theme = "light" | "dark";

/**
 * The SPEC from `heading` up to the next heading of the same or higher level.
 *
 * Fenced blocks are skipped when looking for that next heading, because a `#`
 * opening a line inside a code fence is a comment, not a section.
 */
export function specSection(heading: string): string {
  const lines = SPEC.split("\n");
  const start = lines.findIndex((line) => line.startsWith(heading));
  if (start === -1) {
    throw new Error(
      `${SPEC_PATH} has no line starting "${heading}" — this gate reads that section, so a ` +
        "renamed or renumbered heading must be followed here rather than silently skipped.",
    );
  }
  const depth = (/^#+/.exec(heading) ?? [""])[0].length;

  let fenced = false;
  let end = lines.length;
  for (let i = start + 1; i < lines.length; i++) {
    const line = lines[i] ?? "";
    if (line.startsWith("```")) {
      fenced = !fenced;
      continue;
    }
    if (fenced) continue;
    const next = /^(#+)\s/.exec(line);
    if (next?.[1] !== undefined && next[1].length <= depth) {
      end = i;
      break;
    }
  }
  return lines.slice(start, end).join("\n");
}

/**
 * Every `--oto-*` declaration inside the rule(s) whose prelude names `theme`.
 *
 * Works on `tokens.css` and on the CSS block §M.4/§M.5 quote, which is the whole
 * point: the same reader over both documents is what makes them diffable.
 *
 * ⚠️ A rule that names NEITHER theme contributes nothing — which is deliberate.
 * `:root { --oto-row-h }` and `[data-density="compact"]` are density, not palette,
 * and asking dark to define a row height would be asking the wrong question.
 */
export function themeTokens(css: string, theme: Theme): ReadonlyMap<string, string> {
  const out = new Map<string, string>();
  for (const rule of css.matchAll(/([^{}]*)\{([^{}]*)\}/g)) {
    if (!(rule[1] ?? "").includes(`[data-theme="${theme}"]`)) continue;
    for (const decl of (rule[2] ?? "").matchAll(/(--oto-[a-z0-9-]+)\s*:\s*([^;]+);/g)) {
      const [, name, value] = decl;
      if (name === undefined || value === undefined) continue;
      out.set(name, value.trim().toLowerCase());
    }
  }
  return out;
}

/** WCAG 2.x relative luminance of a `#rrggbb` colour. */
export function relativeLuminance(hex: string): number {
  const m = /^#([0-9a-f]{6})$/i.exec(hex.trim());
  if (m?.[1] === undefined) throw new Error(`not a six-digit hex colour: ${hex}`);
  const n = Number.parseInt(m[1], 16);
  const channel = (v: number): number => {
    const s = v / 255;
    return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  };
  return (
    0.2126 * channel((n >> 16) & 0xff) +
    0.7152 * channel((n >> 8) & 0xff) +
    0.0722 * channel(n & 0xff)
  );
}

/** WCAG 2.x contrast ratio, order-independent. */
export function contrastRatio(a: string, b: string): number {
  const [hi, lo] = [relativeLuminance(a), relativeLuminance(b)].sort((x, y) => y - x) as [
    number,
    number,
  ];
  return (hi + 0.05) / (lo + 0.05);
}
