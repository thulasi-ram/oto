package repository_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/enrichment/repository"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/test/harness"
)

func TestMain(m *testing.M) { harness.Main(m) }

// `enrichment_cache` is DISPOSABLE. It has no foreign key, participates in no
// cascade, and may be truncated at any moment with no loss of meaning — the
// provenanced record lives in `enrichments`.
//
// ⚠️ ONE THING IN THIS FILE IS NOT DRIVEN BY THE INJECTED CLOCK, and it is worth
// naming rather than hiding. `Get` filters with `expires_at > now()` — the
// DATABASE's clock — so a test cannot make an entry expire by advancing
// h.Clock. Freshness is therefore expressed as a decade of margin either side of
// the container's wall clock: `farFuture` is alive by any clock, `longPast` is
// dead by any clock. `DeleteExpired` takes its instant as a PARAMETER and is
// driven by the harness clock exactly as it should be.
var (
	// farFuture is alive under any clock the container could hold.
	farFuture = harness.Epoch.Add(10 * 365 * 24 * time.Hour)
	// longPast is dead under any clock the container could hold: harness.Epoch
	// is months behind real time by construction.
	longPast = harness.Epoch.Add(time.Minute)
)

// entry builds a storable entry. It goes through the constructor because there
// is no other way in: `enrichment_cache_key_ck` and `enrichment_cache_exp_ck`
// are proven by domain.NewCacheEntry, so an entry the table would refuse cannot
// reach the repository at all — which is why this file no longer tests the
// repository's reaction to one.
func entry(t *testing.T, key string, orgID string, computed, expires time.Time, payload string) domain.CacheEntry {
	t.Helper()

	e, err := domain.NewCacheEntry(domain.CacheEntryParams{
		Key:        key,
		OrgID:      orgID,
		Payload:    []byte(payload),
		ComputedAt: computed,
		ExpiresAt:  expires,
	})
	require.NoError(t, err)
	return e
}

// cacheKey is a realistic key: the domain owns the derivation, and it embeds the
// org and the enricher version because an enricher cannot be trusted to
// remember either.
func cacheKey(orgID, enricher string, version int, seed string) string {
	return domain.CacheKey(orgID, enricher, version, seed)
}

// ----------------------------------------------------------------------- hit

func TestAWrittenEntryIsServedBackVerbatim(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewCacheRepository(h.Pool)

	key := cacheKey(org.ID.String(), "prom.rule", 1, "an-alert-key")
	require.NoError(t, repo.Put(h.Ctx, org.Scope,
		entry(t, key, org.ID.String(), harness.Epoch, farFuture, `{"expr":"up == 0","for_seconds":600}`)))

	got, ok, err := repo.Get(h.Ctx, org.Scope, key)
	require.NoError(t, err)
	require.True(t, ok, "a live entry is a hit")

	assert.Equal(t, key, got.Key())
	assert.Equal(t, org.ID.String(), got.OrgID())
	assert.JSONEq(t, `{"expr":"up == 0","for_seconds":600}`, string(got.Payload()),
		"the payload is handed back as raw JSON: decoding it is the reader's job")
	assert.True(t, got.ComputedAt().Equal(harness.Epoch))
	assert.True(t, got.ExpiresAt().Equal(farFuture))
	assert.Equal(t, time.UTC, got.ComputedAt().Location(), "every timestamp leaves this layer in UTC")
	assert.Equal(t, time.UTC, got.ExpiresAt().Location())
}

func TestWritingTheSameKeyTwiceOverwritesRatherThanConflicts(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewCacheRepository(h.Pool)

	key := cacheKey(org.ID.String(), "alert.history", 1, "an-alert-id")
	require.NoError(t, repo.Put(h.Ctx, org.Scope,
		entry(t, key, org.ID.String(), harness.Epoch, farFuture, `{"count_24h":1}`)))
	require.NoError(t, repo.Put(h.Ctx, org.Scope,
		entry(t, key, org.ID.String(), harness.Epoch.Add(time.Hour), farFuture, `{"count_24h":9}`)))

	got, ok, err := repo.Get(h.Ctx, org.Scope, key)
	require.NoError(t, err)
	require.True(t, ok)
	assert.JSONEq(t, `{"count_24h":9}`, string(got.Payload()), "the newer answer wins")
	assert.True(t, got.ComputedAt().Equal(harness.Epoch.Add(time.Hour)))

	var rows int
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT count(*) FROM enrichment_cache WHERE cache_key = $1`, key).Scan(&rows))
	assert.Equal(t, 1, rows, "one key, one row: the PRIMARY KEY is the whole idempotency story")
}

// ---------------------------------------------------------------------- miss

func TestAnAbsentKeyIsAMissAndNotAnError(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewCacheRepository(h.Pool)

	got, ok, err := repo.Get(h.Ctx, org.Scope,
		cacheKey(org.ID.String(), "prom.rule", 1, "never-written"))

	require.NoError(t, err, "a miss is the normal case, not a failure")
	assert.False(t, ok)
	assert.Equal(t, domain.CacheEntry{}, got, "and it carries nothing a caller could mistake for a hit")
}

// TestAnUnusableKeyIsAMissWithoutTouchingTheDatabase.
func TestAnUnusableKeyIsAMissWithoutTouchingTheDatabase(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewCacheRepository(h.Pool)

	for _, key := range []string{"", string(make([]byte, domain.MaxCacheKeyBytes+1))} {
		got, ok, err := repo.Get(h.Ctx, org.Scope, key)
		require.NoError(t, err, "a key the column cannot hold is a miss, never a 500")
		assert.False(t, ok)
		assert.Equal(t, domain.CacheEntry{}, got)
	}
}

// ------------------------------------------------------------------- expiry

// TestAnExpiredEntryIsNeverServed. Rows past their expiry are deleted, never
// served: a stale answer presented as a fresh fact is the failure this whole
// module exists to avoid.
func TestAnExpiredEntryIsNeverServed(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewCacheRepository(h.Pool)

	key := cacheKey(org.ID.String(), "prom.rule", 1, "long-dead")
	require.NoError(t, repo.Put(h.Ctx, org.Scope,
		entry(t, key, org.ID.String(), harness.Epoch, longPast, `{"expr":"up == 0"}`)))

	// The row is really there…
	var rows int
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT count(*) FROM enrichment_cache WHERE cache_key = $1`, key).Scan(&rows))
	require.Equal(t, 1, rows)

	// …and it is still a miss.
	got, ok, err := repo.Get(h.Ctx, org.Scope, key)
	require.NoError(t, err)
	assert.False(t, ok, "an expired entry and an absent one are the same answer")
	assert.Equal(t, domain.CacheEntry{}, got)
	assert.Empty(t, got.Payload(), "the dead payload does not leak out alongside the false")
}

// TestADerivedKeyIsAlwaysStorable. `enrichment_cache_key_ck` (1..512 bytes) is
// refused by domain.NewCacheEntry now — see the domain tests — so what is left
// to prove HERE is the other half of that bargain: a key the domain derived
// always fits the column, because CacheKey hashes the seed and an arbitrarily
// long seed full of label values still comes out 512 bytes or less.
func TestADerivedKeyIsAlwaysStorable(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewCacheRepository(h.Pool)

	derived := cacheKey(org.ID.String(), "prom.rule", 1, string(make([]byte, 64<<10)))
	require.LessOrEqual(t, len(derived), domain.MaxCacheKeyBytes)
	require.NoError(t, repo.Put(h.Ctx, org.Scope,
		entry(t, derived, org.ID.String(), harness.Epoch, farFuture, `{}`)))
}

func TestAnEmptyPayloadIsStoredAsAnObject(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewCacheRepository(h.Pool)

	key := cacheKey(org.ID.String(), "prom.rule", 1, "empty-payload")
	require.NoError(t, repo.Put(h.Ctx, org.Scope,
		entry(t, key, org.ID.String(), harness.Epoch, farFuture, "")))

	got, ok, err := repo.Get(h.Ctx, org.Scope, key)
	require.NoError(t, err)
	require.True(t, ok)
	assert.JSONEq(t, `{}`, string(got.Payload()), "an empty payload is stored as {}, never as null")
}

// --------------------------------------------------------------- tenancy

// TestOneOrgNeverReadsAnotherOrgsEntry.
//
// The org_id predicate in the SELECT is REDUNDANT against the primary key — the
// key already embeds the org — and it is there anyway. A cache is exactly the
// kind of table where a subtle key-collision bug becomes a cross-tenant read,
// and a redundant predicate costs nothing to enforce that it cannot.
func TestOneOrgNeverReadsAnotherOrgsEntry(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	alice, bob := h.Org(), h.Org()
	repo := repository.NewCacheRepository(h.Pool)

	// A key that COLLIDES: both tenants ask for the same physical row.
	shared := "e:prom.rule:v1:collision"
	require.NoError(t, repo.Put(h.Ctx, alice.Scope,
		entry(t, shared, alice.ID.String(), harness.Epoch, farFuture, `{"secret":"alice"}`)))

	got, ok, err := repo.Get(h.Ctx, bob.Scope, shared)
	require.NoError(t, err)
	assert.False(t, ok, "a cross-tenant cache hit would be a data leak, not a performance win")
	assert.Empty(t, got.Payload())

	// Alice still has hers.
	got, ok, err = repo.Get(h.Ctx, alice.Scope, shared)
	require.NoError(t, err)
	require.True(t, ok)
	assert.JSONEq(t, `{"secret":"alice"}`, string(got.Payload()))
}

// ------------------------------------------------------------- the sweep

// TestTheSweepEvictsOnlyWhatIsPastTheInstantItWasGiven.
//
// DeleteExpired takes its instant as a parameter, so this — unlike Get — is
// driven entirely by the harness clock.
func TestTheSweepEvictsOnlyWhatIsPastTheInstantItWasGiven(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewCacheRepository(h.Pool)

	oneMinute := cacheKey(org.ID.String(), "prom.rule", 1, "t+1m")
	fiveMinutes := cacheKey(org.ID.String(), "prom.rule", 1, "t+5m")
	alive := cacheKey(org.ID.String(), "prom.rule", 1, "alive")

	require.NoError(t, repo.Put(h.Ctx, org.Scope,
		entry(t, oneMinute, org.ID.String(), harness.Epoch, harness.Epoch.Add(time.Minute), `{}`)))
	require.NoError(t, repo.Put(h.Ctx, org.Scope,
		entry(t, fiveMinutes, org.ID.String(), harness.Epoch, harness.Epoch.Add(5*time.Minute), `{}`)))
	require.NoError(t, repo.Put(h.Ctx, org.Scope,
		entry(t, alive, org.ID.String(), harness.Epoch, farFuture, `{}`)))

	// Three minutes past Epoch: the first entry is dead, the second is not.
	h.Advance(3 * time.Minute)
	n, err := repo.DeleteExpired(h.Ctx, h.Now(), 100)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "the sweep is not allowed to run ahead of the clock it was given")

	assert.Equal(t, 2, countRows(t, h))

	// Ten minutes past Epoch: the second one goes too.
	h.Advance(7 * time.Minute)
	n, err = repo.DeleteExpired(h.Ctx, h.Now(), 100)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	// And the live one is untouched and still served.
	assert.Equal(t, 1, countRows(t, h))
	_, ok, err := repo.Get(h.Ctx, org.Scope, alive)
	require.NoError(t, err)
	assert.True(t, ok, "the sweep evicts the dead and leaves the living alone")
}

// TestTheSweepIsBounded. An unbounded DELETE on a table that can hold millions
// of rows takes a long lock and blocks the pipeline that depends on it; the
// `cache.expire` job runs every 600 s and will catch up.
func TestTheSweepIsBounded(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewCacheRepository(h.Pool)

	for i := 0; i < 5; i++ {
		require.NoError(t, repo.Put(h.Ctx, org.Scope, entry(t,
			cacheKey(org.ID.String(), "prom.rule", 1, "dead-"+string(rune('a'+i))),
			org.ID.String(), harness.Epoch, harness.Epoch.Add(time.Duration(i+1)*time.Minute), `{}`)))
	}

	sweepAt := harness.Epoch.Add(time.Hour)

	n, err := repo.DeleteExpired(h.Ctx, sweepAt, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n, "a maintenance sweep that causes an incident is worse than a large cache")
	assert.Equal(t, 3, countRows(t, h))

	n, err = repo.DeleteExpired(h.Ctx, sweepAt, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)

	n, err = repo.DeleteExpired(h.Ctx, sweepAt, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "it catches up on the next pass")

	n, err = repo.DeleteExpired(h.Ctx, sweepAt, 2)
	require.NoError(t, err)
	assert.Zero(t, n, "and then it has nothing left to do")
	assert.Zero(t, countRows(t, h))
}

func TestAnUnboundedSweepIsGivenABound(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	org := h.Org()
	repo := repository.NewCacheRepository(h.Pool)

	require.NoError(t, repo.Put(h.Ctx, org.Scope, entry(t,
		cacheKey(org.ID.String(), "prom.rule", 1, "dead"),
		org.ID.String(), harness.Epoch, harness.Epoch.Add(time.Minute), `{}`)))

	n, err := repo.DeleteExpired(h.Ctx, harness.Epoch.Add(time.Hour), 0)
	require.NoError(t, err, "limit <= 0 must default, never mean LIMIT 0 and never mean unbounded")
	assert.Equal(t, int64(1), n)
}

// TestTheSweepIsGlobalAndTakesNoTenant. It runs under no principal over a table
// with no owner, which is why its signature has no TenantScope.
func TestTheSweepIsGlobalAndTakesNoTenant(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	alice, bob := h.Org(), h.Org()
	repo := repository.NewCacheRepository(h.Pool)

	require.NoError(t, repo.Put(h.Ctx, alice.Scope, entry(t,
		cacheKey(alice.ID.String(), "prom.rule", 1, "dead"),
		alice.ID.String(), harness.Epoch, harness.Epoch.Add(time.Minute), `{}`)))
	require.NoError(t, repo.Put(h.Ctx, bob.Scope, entry(t,
		cacheKey(bob.ID.String(), "prom.rule", 1, "dead"),
		bob.ID.String(), harness.Epoch, harness.Epoch.Add(time.Minute), `{}`)))

	n, err := repo.DeleteExpired(h.Ctx, harness.Epoch.Add(time.Hour), 100)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n, "one sweep, every tenant")
	assert.Zero(t, countRows(t, h))
}

// TestTheCacheSurvivesLosingAnOrg pins the "no FK, no cascade" decision: an
// enrichment cache row must not participate in a delete cascade, because the
// table is disposable and the record it would take with it is not.
func TestTheCacheHasNoForeignKeyToItsOrg(t *testing.T) {
	t.Parallel()

	h := harness.New(t)
	repo := repository.NewCacheRepository(h.Pool)

	// An org id that was never inserted. A FK would refuse this row.
	orphan := id.New()
	scope := harness.Scope(t, orphan)

	key := cacheKey(orphan.String(), "prom.rule", 1, "orphan")
	require.NoError(t, repo.Put(h.Ctx, scope, entry(t, key, orphan.String(), harness.Epoch, farFuture, `{}`)),
		"no FK: this is a disposable cache and must not participate in a delete cascade")

	_, ok, err := repo.Get(h.Ctx, scope, key)
	require.NoError(t, err)
	assert.True(t, ok)
}

func countRows(t *testing.T, h *harness.H) int {
	t.Helper()
	var n int
	require.NoError(t, h.Pool.QueryRow(h.Ctx, `SELECT count(*) FROM enrichment_cache`).Scan(&n))
	return n
}
