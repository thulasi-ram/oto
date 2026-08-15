package db

// The §E.1 page bounds, and the ONE home of them. §E.1 says it in one sentence —
// "`limit` default 50, max 200" — and this package is where that sentence lands,
// because `Keyset.Limit` is the field being bounded and keyset pagination is what
// this package owns.
//
// They were previously spelled five times: `DefaultPageLimit`/`MaxPageLimit` in
// `platform/httpx`, and `DefaultLimit`/`MaxLimit` plus a byte-identical
// `clampLimit` in each of the alerts, channels, grouping and sources
// repositories. A §E.1 change therefore landed in four spellings and silently
// missed the fifth, which is the same failure ADR 0026 hit with the §D.1 tuning
// defaults. Every declaration elsewhere is now a REFERENCE to these, which the
// compiler keeps honest.
//
// They live here rather than in `platform/httpx` because a repository is the
// bottom layer: `httpx` is the HTTP server, router and SSE writer, imported today
// by `api`, `app` and `cmd` and by no service or repository, and CONTEXT.md §5.1
// forbids api and repository knowing each other in either direction. Four
// repositories importing the HTTP layer to share a three-line clamp would buy
// this de-duplication with a dependency inversion.
const (
	// DefaultPageLimit is the page size a caller gets for asking for nothing.
	DefaultPageLimit = 50
	// MaxPageLimit is the ceiling. A caller wanting more is going to page anyway.
	MaxPageLimit = 200
)

// ClampLimit applies the §E.1 page bounds: a limit of zero or less means the
// caller asked for nothing and gets the default, and a limit above the ceiling is
// capped rather than refused.
//
// It is not a convenience. A List that forgets to clamp is a List that asks
// Postgres for an unbounded scan, so this is applied at every List in every
// repository.
func ClampLimit(n int) int {
	switch {
	case n <= 0:
		return DefaultPageLimit
	case n > MaxPageLimit:
		return MaxPageLimit
	default:
		return n
	}
}
