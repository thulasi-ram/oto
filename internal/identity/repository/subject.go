package repository

import (
	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// subjectCols is the tail every credential-resolving query appends: the org and
// user facts a Principal needs, joined in rather than fetched afterwards.
//
// It is one struct rather than six loose scan targets so that the column list,
// the scan order and the mapping stay in one file. A join whose SELECT list and
// Scan call drift apart fails at runtime with a type error that names a column
// nobody wrote.
//
// ⛔ `email` IS A POINTER BECAUSE `users.email` IS NULLABLE SINCE 00074, and a
// `string` here was a runtime error rather than a type error: pgx scanning SQL NULL
// into a `*string` destination fails with "cannot scan NULL into *string". The
// failure would land on `resolveSessionSQL` or `resolveByPrefixSQL` — the two
// statements on the authenticated read path of EVERY request — as a 500 rather than
// a 401.
//
// ⚠️ IT SHOULD BE UNREACHABLE AND IS HANDLED ANYWAY. The only NULL-email rows are
// SHADOW MEMBERS, which cannot hold a session (nothing can log in as one) and
// cannot hold a PAT (`api_tokens` are minted for the authenticated principal, and a
// shadow row never becomes one). So no join in this file should ever return one.
// "Should never" is a claim about today's wiring, made by a scan that would fail
// closed on the request of whoever proved it wrong — and the cost of being right is
// one pointer.
type subjectCols struct {
	orgSlug     string
	orgName     string
	userID      uuid.UUID
	email       *string
	displayName string
}

// subjectColumns is the SELECT tail. Every query using subjectCols must end with
// exactly these five expressions, in this order.
const subjectColumns = `o.slug, o.name, u.id, u.email, u.display_name`

// targets returns the scan destinations, in subjectColumns order.
func (s *subjectCols) targets() []any {
	return []any{&s.orgSlug, &s.orgName, &s.userID, &s.email, &s.displayName}
}

func (s subjectCols) toDomain(orgID uuid.UUID) (domain.Subject, error) {
	// A NULL email leaves the zero domain.Email, which is what a SHADOW MEMBER's
	// absent address is (00074). Nothing on this path can be one — see the struct's
	// own comment — so this branch is the fail-closed reading of a state the wiring
	// does not produce, rather than a case the credential path serves.
	var email domain.Email
	if s.email != nil {
		var err error
		email, err = domain.NewEmail(*s.email)
		if err != nil {
			// users_email_ck should have made this unreachable; if it is reachable the
			// schema has drifted, which is oto's bug and not the caller's.
			return domain.Subject{}, errs.Internal("subject_row_invalid", err)
		}
	}
	return domain.Subject{
		OrgID:       orgID,
		OrgSlug:     s.orgSlug,
		OrgName:     s.orgName,
		UserID:      s.userID,
		Email:       email,
		DisplayName: s.displayName,
	}, nil
}
