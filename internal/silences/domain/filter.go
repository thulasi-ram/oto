package domain

import "github.com/google/uuid"

// Filter is the compiled, validated form of the `listSilences` query string.
//
// It lives in the domain rather than in the repository so that `silences/api` can
// name it without importing storage: CONTEXT.md §5.1 forbids an api package from
// reaching into a repository, and a filter that only the repository could
// describe would force exactly that.
type Filter struct {
	// States restricts to the mirrored lifecycle states. Empty means all three.
	States []string
	// SourceID restricts to one AlertSource. Nil UUID means all.
	SourceID uuid.UUID
	// CreatedBy matches the silence's upstream creator string exactly. It is
	// whoever created the silence in Alertmanager, and is NOT an oto user.
	CreatedBy string
	// Query is free text over the comment and the rendered matchers.
	Query string
	// FilterHash must equal Cursor.Hash or the cursor is rejected (SPEC §E.1): a
	// keyset cursor minted against one filter is meaningless against another.
	FilterHash string
}
