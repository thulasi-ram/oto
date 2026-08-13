// Package scope is the SCOPE-BOUNDARY structural gate: SPEC §P-19's three tests
// backing AC-50, AC-51 and AC-52.
//
// ⭐⭐ WHY A PACKAGE OF ITS OWN, AND WHY NOT A GREP.
//
// CONTEXT.md §41-43 states the line this package guards:
//
//	`occurrence.acked_by = alice` is a fact about the occurrence — it was
//	acknowledged, by whom. `occurrence.assigned_to = alice` is a fact about
//	Alice — she owes work. Identical columns; opposite products.
//
// Until this package existed, that line was guarded entirely by `tools/lintvocab`
// — six `regexp.MustCompile` patterns matched against SOURCE TEXT, with a
// documented debt baseline. That tool is AC-49's gate and it keeps its job (see
// its own doc comment). What it cannot be is AC-50/51/52's gate, because a grep
// over source answers a question about FILES and all three ACs ask a question
// about the RUNNING SYSTEM:
//
//   - AC-50 asks what columns the DEPLOYED SCHEMA has. DDL run outside
//     `db/migrations/`, an expand/contract whose contract half never landed, or a
//     spelling the six literals do not cover all leave the source clean and the
//     database wrong. So this package asks `information_schema` on a live,
//     fully-migrated database.
//   - AC-51 asks what routes the MOUNTED ROUTER serves. A path assembled from a
//     constant, or mounted through a sub-router, is invisible to a grep and
//     present in the trie. So this package builds the real `app.Container` and
//     walks `chi`'s route tree.
//   - AC-52 asks what TYPE a field has in every layer. A grep for
//     `unacked_reminder_after_s` finds the name whether it is `*int` or `[]int`.
//     So this package asserts through the compiler, through `reflect`, through the
//     AST and against the column's own `data_type`.
//
// ⭐ EVERY GATE HERE HAS A COMPANION `…GateFires` TEST, and that is not optional
// decoration. All three properties hold today, so all three gates pass whether
// they work, are inverted, or are deleted outright. README.md:144-147 is the
// standard: "A gate that has never failed is a gate nobody knows works." Each
// companion plants the real violation — a forbidden column added inside a
// transaction, a `/resolve` route mounted through a sub-router from a constant, a
// field widened to a slice, the reminder column retyped to `integer[]` — and
// asserts the production checker reports it, with a message that says why.
//
// AC-52 has three, because it is asserted in three ways that can each go vacuous
// on their own: the reflect list, the AST sweep and the column. The fourth
// instrument, the compile-time `var _ *int = …` block, needs no companion — it
// cannot pass while being wrong, because a wrong assertion does not compile.
package scope
