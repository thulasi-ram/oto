// Package filter is the label-selector front end of the alert list: an
// Alertmanager-style matcher parser and the small filter AST every selector
// compiles into (ADR 0017).
//
// The AST exists so that the query surface and the SQL it becomes are not the
// same thing. Matchers compile to it today; if a richer expression language ever
// earns its place it compiles to the same nodes, and the set of predicates that
// survive translation into an indexed query becomes an explicit, testable list
// rather than an emergent surprise.
//
// ⭐ THE BINDING RULE (ADR 0017): a predicate that cannot be answered from an
// index is REJECTED AT PARSE TIME with a precise message. It is never quietly
// degraded to a sequential scan, because a filter that works in staging and times
// out during an incident is worse than a filter that refuses.
package filter
