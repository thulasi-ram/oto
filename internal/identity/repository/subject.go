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
type subjectCols struct {
	orgSlug     string
	orgName     string
	userID      uuid.UUID
	email       string
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
	email, err := domain.NewEmail(s.email)
	if err != nil {
		// users_email_ck should have made this unreachable; if it is reachable the
		// schema has drifted, which is oto's bug and not the caller's.
		return domain.Subject{}, errs.Internal("subject_row_invalid", err)
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
