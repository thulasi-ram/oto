package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// IngestTokenPrefix is the mandatory prefix of an ingest secret
// (api_tokens_prefix_ck). A PAT (`oto_pat_…`) presented here is rejected before
// it ever reaches the database: an ingest token is scoped to exactly one source
// and can read nothing, and that narrowness is the whole security model of a
// credential that lives in an `alertmanager.yml` on every cluster.
const IngestTokenPrefix = "oto_ingest_"

// TokenAuthenticator resolves a presented ingest secret.
//
// Declared HERE, by the consumer, rather than imported from `identity`: this
// handler needs exactly two facts about the principal — which org, and which
// source it may push to — and depending on the whole identity module for that
// would invert the layering (§F.5 rule 4).
//
// It takes the sha256 DIGEST, never the plaintext, so the secret cannot reach a
// query log, an error string or a stack trace beyond this file.
type TokenAuthenticator interface {
	Lookup(ctx context.Context, digest []byte, now time.Time) (domain.IngestToken, error)
}

// Authenticator turns a request into a tenant scope, with a short positive cache.
//
// SPEC §G.1 budgets ONE indexed lookup on `api_tokens.token_hash`, LRU-cached for
// 60 s. The cache is the difference between one database round trip per webhook
// and two, on the only path in oto with a latency budget an upstream enforces.
//
// ONLY SUCCESSES ARE CACHED. Caching a failure would let one probe of a wrong
// token lock a source out for a minute, and it would turn the cache into an
// amplifier for exactly the traffic it should not serve.
type Authenticator struct {
	tokens TokenAuthenticator
	clk    clock.Clock
	ttl    time.Duration

	mu    sync.Mutex
	cache map[string]cachedToken
	// maxEntries bounds the map so that a flood of distinct valid tokens cannot
	// grow it without limit. Eviction is a full clear rather than an LRU chain:
	// the map is tiny, the TTL is 60 s, and a lock-free-ish clear beats
	// maintaining a linked list on the hot path.
	maxEntries int
}

type cachedToken struct {
	token   domain.IngestToken
	expires time.Time
}

// DefaultTokenTTL is how long a resolved token stays cached (§G.1).
const DefaultTokenTTL = 60 * time.Second

// defaultMaxTokens bounds the positive cache.
const defaultMaxTokens = 4096

// NewAuthenticator builds the authenticator.
func NewAuthenticator(tokens TokenAuthenticator, clk clock.Clock, ttl time.Duration) *Authenticator {
	if clk == nil {
		clk = clock.New()
	}
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	return &Authenticator{
		tokens:     tokens,
		clk:        clk,
		ttl:        ttl,
		cache:      make(map[string]cachedToken),
		maxEntries: defaultMaxTokens,
	}
}

// Authenticate resolves the request's bearer token and proves it is scoped to
// sourceID.
//
// Every failure is the SAME error: `errs.Unauthorized`, one message, no detail
// about which check failed. A caller learning "that token exists but is for a
// different source" is a caller enumerating sources.
//
// 401 is one of only three 4xx this endpoint may produce, and it qualifies for
// the same reason the other two do (§G.2): retrying the same bytes with the same
// bad credential could not succeed, so the alert Alertmanager is about to discard
// was never going to be recorded anyway.
func (a *Authenticator) Authenticate(r *http.Request, sourceID uuid.UUID) (db.TenantScope, domain.IngestToken, error) {
	secret, ok := bearerToken(r)
	if !ok || !strings.HasPrefix(secret, IngestTokenPrefix) {
		return db.TenantScope{}, domain.IngestToken{}, unauthorized()
	}

	digest := sha256.Sum256([]byte(secret))
	token, err := a.resolve(r.Context(), digest[:])
	if err != nil {
		return db.TenantScope{}, domain.IngestToken{}, unauthorized()
	}

	// Constant time, because the comparison is on attacker-influenced input and
	// the cost of doing it properly is nothing.
	if subtle.ConstantTimeCompare(token.SourceID[:], sourceID[:]) != 1 {
		return db.TenantScope{}, domain.IngestToken{}, unauthorized()
	}

	scope, err := db.NewTenantScope(token.OrgID)
	if err != nil {
		return db.TenantScope{}, domain.IngestToken{}, unauthorized()
	}
	return scope, token, nil
}

func (a *Authenticator) resolve(ctx context.Context, digest []byte) (domain.IngestToken, error) {
	key := string(digest)
	now := a.clk.Now()

	a.mu.Lock()
	hit, ok := a.cache[key]
	a.mu.Unlock()
	if ok && now.Before(hit.expires) {
		return hit.token, nil
	}

	token, err := a.tokens.Lookup(ctx, digest, now)
	if err != nil {
		return domain.IngestToken{}, err
	}

	a.mu.Lock()
	if len(a.cache) >= a.maxEntries {
		a.cache = make(map[string]cachedToken, a.maxEntries)
	}
	a.cache[key] = cachedToken{token: token, expires: now.Add(a.ttl)}
	a.mu.Unlock()

	return token, nil
}

// Forget drops a token from the positive cache, so a revocation can take effect
// before its TTL expires. Nothing calls it yet; it is the seam a future
// `POST /sources/{id}/rotate-token` needs, and leaving it out would make that a
// change to this file rather than a call into it.
func (a *Authenticator) Forget(digest []byte) {
	a.mu.Lock()
	delete(a.cache, string(digest))
	a.mu.Unlock()
}

// bearerToken extracts `Authorization: Bearer <secret>`.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	secret := strings.TrimSpace(h[len(prefix):])
	return secret, secret != ""
}

// unauthorized is the ONE error every authentication failure returns. Identical
// message, identical code, no hint about which check said no.
func unauthorized() error {
	return errs.Unauthorized("ingest_unauthorized",
		"a valid ingest token scoped to this source is required")
}
