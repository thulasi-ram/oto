package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	identitydomain "github.com/thulasiram/oto/internal/identity/domain"
	identityrepo "github.com/thulasiram/oto/internal/identity/repository"
	identityservice "github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
)

// ⭐ THE BOOTSTRAP IS THE ONLY WAY A FRESH INSTALL BECOMES USABLE.
//
// v1 has no org-creation API and no self-service signup, and `POST
// /auth/login` needs a `users` row that nothing can create. A fresh `oto migrate`
// therefore produced a schema that no credential could ever reach — every test
// against this product so far has seeded Postgres with hand-written SQL, which is
// not a documented install path and is not something a user will do.
//
// ⛔ IT IS NOT AN HTTP ROUTE AND MUST NEVER BECOME ONE. An unauthenticated
// endpoint that creates an org and a full-access token is an account-takeover
// endpoint with a friendly name; the fact that it "only works once" is a race,
// not a control. It is a subcommand, so running it requires a shell on the host
// and the database credentials — which is the same authority that could write the
// rows by hand anyway.

// BootstrapRequest is the first org, its first user and its first token.
type BootstrapRequest struct {
	// OrgSlug is the URL-safe tenant handle (orgs_slug_ck).
	OrgSlug string
	// OrgName is the display name.
	OrgName string
	// Email is the first user's address; it is what `POST /auth/login` takes.
	Email string
	// DisplayName is the first user's name.
	DisplayName string
	// Password is the first user's password. It is hashed with argon2id before it
	// touches the database and is never logged.
	Password string
	// TokenName labels the PAT this command mints.
	TokenName string
}

// BootstrapResult is what the operator needs to write down.
type BootstrapResult struct {
	OrgID  uuid.UUID
	UserID uuid.UUID
	// Token is the PAT, returned EXACTLY ONCE. Only its sha256 is stored, so
	// losing it means minting another.
	Token string
	// TokenPrefix identifies the token later without revealing it.
	TokenPrefix string
}

// MinBootstrapPasswordBytes is the shortest password this command will accept.
// It is deliberately longer than any interactive minimum: this credential can do
// everything inside the org and is typed once, by a person who is not in a hurry.
const MinBootstrapPasswordBytes = 12

// ErrAlreadyBootstrapped is returned when the deployment already has an org.
var ErrAlreadyBootstrapped = errors.New("this deployment already has an org; bootstrap runs once")

// Bootstrap creates the first org, its first user and its first API token,
// returning the token exactly once.
//
// ⛔ IT REFUSES TO RUN TWICE, explicitly rather than idempotently. "Idempotent"
// here would mean either silently doing nothing — leaving an operator who mistyped
// the address believing they have a login they do not — or resetting a password
// on an existing account, which is a takeover primitive. Refusing is the only
// answer that cannot be turned into either.
//
// Everything commits in ONE transaction. A half-bootstrapped deployment (an org
// with no user, a user with no token) is one that neither works nor can be
// bootstrapped again.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool, req BootstrapRequest, now time.Time) (BootstrapResult, error) {
	if pool == nil {
		return BootstrapResult{}, errors.New("bootstrap: a database pool is required")
	}
	req, err := req.normalise()
	if err != nil {
		return BootstrapResult{}, err
	}

	// Validate through the DOMAIN constructors, not the DDL. A CHECK violation
	// arrives as a 23514 naming a constraint; these say what to type instead.
	email, err := identitydomain.NewEmail(req.Email)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("bootstrap: email: %w", err)
	}

	hasher := authn.NewPasswordHasher()
	encoded, err := hasher.Hash(req.Password)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("bootstrap: hash password: %w", err)
	}
	passwordHash, err := identitydomain.NewPasswordHash(encoded)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("bootstrap: password hash: %w", err)
	}

	orgID := id.New()
	org, err := identitydomain.NewOrg(orgID, req.OrgSlug, req.OrgName, identitydomain.Settings{})
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("bootstrap: org: %w", err)
	}

	userID := id.New()
	user, err := identitydomain.NewUser(userID, orgID, email, req.DisplayName, passwordHash)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("bootstrap: user: %w", err)
	}

	// The PAT. Same mint as `POST /api/v1/api-tokens`, and the same promise: the
	// secret exists in this process and in the operator's terminal, nowhere else.
	secret := identitydomain.SecretPrefixPAT + id.Token(identityservice.SecretEntropyBytes)
	sum := sha256.Sum256([]byte(secret))
	hash, err := identitydomain.NewTokenHash(sum[:])
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("bootstrap: token hash: %w", err)
	}
	prefix, err := identitydomain.PrefixOfSecret(secret)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("bootstrap: token prefix: %w", err)
	}
	token, err := identitydomain.NewAPIToken(identitydomain.NewAPITokenParams{
		ID:        id.New(),
		OrgID:     orgID,
		UserID:    userID,
		Kind:      identitydomain.TokenKindPAT,
		Name:      req.TokenName,
		Hash:      hash,
		Prefix:    prefix,
		CreatedAt: now.UTC(),
	})
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("bootstrap: token: %w", err)
	}

	scope, err := db.NewTenantScope(orgID)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("bootstrap: scope: %w", err)
	}
	tokens := identityrepo.NewAPITokenRepository(pool)

	err = db.Tx(ctx, pool, func(ctx context.Context) error {
		q := db.FromContext(ctx, pool)

		// The once-only check runs INSIDE the transaction and takes a lock the
		// second caller waits on, so two concurrent bootstraps cannot both see an
		// empty table. `pg_advisory_xact_lock` releases with the transaction.
		if _, lerr := q.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, bootstrapLockKey); lerr != nil {
			return fmt.Errorf("bootstrap: lock: %w", lerr)
		}
		var existing int
		if serr := q.QueryRow(ctx, `SELECT count(*) FROM orgs`).Scan(&existing); serr != nil {
			return fmt.Errorf("bootstrap: count orgs: %w", serr)
		}
		if existing > 0 {
			return ErrAlreadyBootstrapped
		}

		if _, ierr := q.Exec(ctx, insertOrgSQL,
			// `{}` rather than a rendered settings blob: an absent key means "use the
			// documented default", and writing today's defaults into the row would
			// freeze them for this org the first time a default ever changes.
			org.ID, org.Slug, org.Name, []byte("{}"), now.UTC(),
		); ierr != nil {
			return fmt.Errorf("bootstrap: insert org: %w", ierr)
		}
		if _, ierr := q.Exec(ctx, insertUserSQL,
			user.ID, user.OrgID, user.Email.String(), user.DisplayName,
			user.PasswordHash.Encoded(), now.UTC(),
		); ierr != nil {
			return fmt.Errorf("bootstrap: insert user: %w", ierr)
		}
		if ierr := tokens.Insert(ctx, scope, token); ierr != nil {
			return fmt.Errorf("bootstrap: insert token: %w", ierr)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrAlreadyBootstrapped) {
			return BootstrapResult{}, ErrAlreadyBootstrapped
		}
		return BootstrapResult{}, err
	}

	return BootstrapResult{
		OrgID:       orgID,
		UserID:      userID,
		Token:       secret,
		TokenPrefix: token.Prefix.String(),
	}, nil
}

// bootstrapLockKey is an arbitrary but fixed advisory-lock key. Only this
// command takes it.
const bootstrapLockKey int64 = 0x0704_0B00_7570_0001

// ⚠️ These two INSERTs are the only raw writes in `internal/app`, and they are
// here rather than in `identity/repository` deliberately: creating an org or a
// user is not an operation the RUNNING product has, and adding repository
// methods for it would put "create a tenant" and "create a user" one call away
// from any service that already holds those repositories.
const insertOrgSQL = `
INSERT INTO orgs (id, slug, name, settings, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $5)`

const insertUserSQL = `
INSERT INTO users (id, org_id, email, display_name, password_hash, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $6)`

// normalise trims, applies defaults and enforces the bounds this command owns.
func (r BootstrapRequest) normalise() (BootstrapRequest, error) {
	r.OrgSlug = strings.ToLower(strings.TrimSpace(r.OrgSlug))
	r.OrgName = strings.TrimSpace(r.OrgName)
	r.Email = strings.TrimSpace(r.Email)
	r.DisplayName = strings.TrimSpace(r.DisplayName)
	r.TokenName = strings.TrimSpace(r.TokenName)

	if r.OrgSlug == "" {
		return r, errors.New("bootstrap: --org-slug is required")
	}
	if r.OrgName == "" {
		r.OrgName = r.OrgSlug
	}
	if r.Email == "" {
		return r, errors.New("bootstrap: --email is required")
	}
	if r.DisplayName == "" {
		r.DisplayName = r.Email
	}
	if r.TokenName == "" {
		r.TokenName = "bootstrap"
	}
	// ⚠️ Measured in BYTES, like the storage bound. A short password on the one
	// credential that can configure every source in the org is the weakest link in
	// a system whose whole point is that alerts arrive.
	if len(r.Password) < MinBootstrapPasswordBytes {
		return r, fmt.Errorf("bootstrap: --password must be at least %d characters", MinBootstrapPasswordBytes)
	}
	return r, nil
}
