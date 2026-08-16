package service

import (
	"crypto/sha256"
	"log/slog"
	"time"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// DefaultSessionTTL is how long a browser session lives when nothing configures
// it. It matches `security.session_ttl`'s default of thirty days.
const DefaultSessionTTL = 30 * 24 * time.Hour

// SecretEntropyBytes is how much randomness a minted secret carries. 32 bytes is
// 256 bits, which is why the stored sha256 needs no salt and no work factor: a
// password is guessable and needs argon2id, a 256-bit random string is not.
const SecretEntropyBytes = 32

// TouchInterval is how stale `api_tokens.last_used_at` is allowed to get before
// authentication refreshes it.
//
// Writing it on every request would put an UPDATE on the read path of every
// PAT-authenticated call and make each one contend on a single row. "Last used"
// is an operator convenience, not an audit record (§D.1), and five-minute
// resolution is what that convenience is worth.
const TouchInterval = 5 * time.Minute

// Deps is everything the service needs, injected. There is no global state and
// no package-level clock: `internal/app/container.go` builds this.
type Deps struct {
	Orgs     OrgReader
	Users    UserReader
	Tokens   TokenStore
	Sessions SessionStore
	Slack    SlackIdentityStore

	// Tx makes the ingest-token rotation atomic: the mint and the revocation
	// sweep beside it commit together (IssueIngestToken). Nil degrades to two
	// independent writes, which is what once left a source with no working
	// token at all; production wires it.
	Tx TxRunner

	// Hasher is the argon2id password hasher. It is a port so a test can swap in
	// a cheap one rather than spend 19 MiB per case.
	Hasher authn.PasswordHasher
	// Clock is the ONLY source of time in this package. Nothing here calls
	// time.Now (CONTEXT.md §5.2).
	Clock  clock.Clock
	Logger *slog.Logger
	// SessionTTL bounds a browser session. A non-positive value falls back to
	// DefaultSessionTTL: there is no configuration that yields a session which
	// never expires.
	SessionTTL time.Duration

	// Declarative is the deployment's own tuning, and it BEATS every org override
	// (see identity/domain.Declarative). It is process-wide rather than per-tenant
	// because it describes the deployment, not a customer, and the zero value —
	// "configuration states nothing" — is every install that has not opted in.
	//
	// It is injected already validated. `NewDeclarative` refuses an unusable
	// configuration at boot, which is the only moment a bad values file is cheap
	// to find out about.
	Declarative domain.Declarative

	// TrigramAvailable is the SAME process-lifetime `pg_trgm` capability bool
	// `alerts/repository.AlertRepository` is built with — both are computed
	// ONCE, in `internal/app`'s Container, from `db.TrigramAvailable`. identity
	// surfaces it on `GET /api/v1/me` (MeDTO.Search) purely as a display fact
	// for the UI; it never influences a query this package runs. Threading the
	// same bool through both modules, rather than having identity import
	// alerts to ask, is what keeps the two domains decoupled (ADR 0002).
	TrigramAvailable bool
}

// Service is the identity module's business logic. It MINTS the authn.Principal
// every other module consumes.
type Service struct {
	orgs     OrgReader
	users    UserReader
	tokens   TokenStore
	sessions SessionStore
	slack    SlackIdentityStore
	tx       TxRunner

	hasher     authn.PasswordHasher
	clk        clock.Clock
	log        *slog.Logger
	sessionTTL time.Duration
	// declarative is overlaid onto EVERY org read this service performs. Doing it
	// here, once, is what makes the hot path and the settings screen agree: there
	// is no second place an Org is assembled, so there is no path by which a
	// caller can obtain an Org whose Settings ignore the deployment.
	declarative domain.Declarative
	// trigramAvailable is a display fact only; see Deps.TrigramAvailable.
	trigramAvailable bool
}

// Compile-time proof that the service satisfies the port the auth middleware
// declares. If this stops compiling, the middleware and the resolver have
// drifted and every authenticated route is affected.
var _ authn.Resolver = (*Service)(nil)

// New builds the service.
func New(d Deps) *Service {
	clk := d.Clock
	if clk == nil {
		clk = clock.New()
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	hasher := d.Hasher
	if hasher == nil {
		hasher = authn.NewPasswordHasher()
	}
	ttl := d.SessionTTL
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}

	return &Service{
		orgs:             d.Orgs,
		users:            d.Users,
		tokens:           d.Tokens,
		sessions:         d.Sessions,
		slack:            d.Slack,
		tx:               d.Tx,
		hasher:           hasher,
		clk:              clk,
		log:              logger,
		sessionTTL:       ttl,
		declarative:      d.Declarative,
		trigramAvailable: d.TrigramAvailable,
	}
}

// SessionTTL is how long a session this service mints will live.
//
// It is exposed for wiring and diagnostics only. The api layer deliberately does
// NOT use it to compute the cookie's Max-Age — it uses the minted row's
// `expires_at`, so the browser and the database cannot disagree about when a
// session ends even if this value is changed while sessions are live.
func (s *Service) SessionTTL() time.Duration { return s.sessionTTL }

// digest is the ONE place a presented secret becomes the value stored in
// `token_hash`.
//
// sha256 and not argon2id, deliberately: the input is 256 bits of oto-generated
// randomness, so there is nothing to brute-force and a work factor would only
// tax the authentication path. The opposite reasoning applies to passwords,
// which is why those go through authn.PasswordHasher.
//
// It takes the plaintext and returns a digest. The plaintext goes no further:
// nothing in this package logs it, stores it or puts it in an error.
func digest(secret string) (domain.TokenHash, error) {
	sum := sha256.Sum256([]byte(secret))
	return domain.NewTokenHash(sum[:])
}

// unauthenticated is the ONE error every credential failure in this service
// returns — bad password, unknown address, revoked token, expired session,
// ambiguous email. Identical code, identical message.
//
// The uniformity is the point. Any distinction a caller can observe is an oracle
// it can query, and none of them helps a legitimate client: the remedy is "log
// in again" in every case.
func unauthenticated() error {
	return errs.Unauthorized("unauthenticated", "authentication required")
}

// invalidCredentials is the login-specific variant. It says no more than the
// above and exists only so the login endpoint's problem `code` is distinguishable
// in oto's own logs and metrics, never in what it reveals to the caller.
func invalidCredentials() error {
	return errs.Unauthorized("invalid_credentials", "email or password is incorrect")
}
