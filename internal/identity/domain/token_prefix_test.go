package domain

import (
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// ⭐ TestPrefixOfSecretWorksForBothKinds is the regression for the defect that
// made `POST /api/v1/sources` return 422 for every request ever sent to it.
//
// `TokenPrefixLen` was a single constant, 12, correct only for `oto_pat_`.
// `oto_ingest_` is ELEVEN characters, so a twelve-character slice of an ingest
// secret is `oto_ingest_X` — one random character — and api_tokens_prefix_ck
// wants four. Every ingest mint therefore failed, which meant no source could be
// created, which meant the product's primary path did not work on a fresh
// install.
func TestPrefixOfSecretWorksForBothKinds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		secret     string
		wantPrefix string
		wantLen    int
	}{
		{
			name:       "pat",
			secret:     SecretPrefixPAT + "AbCd" + strings.Repeat("x", 39),
			wantPrefix: "oto_pat_AbCd",
			wantLen:    TokenPrefixLenPAT,
		},
		{
			name:   "ingest",
			secret: SecretPrefixIngest + "Wx9Z" + strings.Repeat("y", 39),
			// ⚠️ FIFTEEN characters, not twelve. This literal is the whole bug.
			wantPrefix: "oto_ingest_Wx9Z",
			wantLen:    TokenPrefixLenIngest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := PrefixOfSecret(tc.secret)
			if err != nil {
				t.Fatalf("PrefixOfSecret(%s…) failed: %v", tc.secret[:12], err)
			}
			if got.String() != tc.wantPrefix {
				t.Fatalf("prefix = %q, want %q", got.String(), tc.wantPrefix)
			}
			if len(got.String()) != tc.wantLen {
				t.Fatalf("prefix length = %d, want %d", len(got.String()), tc.wantLen)
			}
		})
	}
}

// TestPrefixLengthsAreKindRelative pins the arithmetic the audit measured.
func TestPrefixLengthsAreKindRelative(t *testing.T) {
	t.Parallel()
	if TokenPrefixLenPAT != 12 {
		t.Errorf("a PAT prefix is len(%q)+4 = 12, got %d", SecretPrefixPAT, TokenPrefixLenPAT)
	}
	if TokenPrefixLenIngest != 15 {
		t.Errorf("an ingest prefix is len(%q)+4 = 15, got %d", SecretPrefixIngest, TokenPrefixLenIngest)
	}
	if PrefixLenOfKind(TokenKindPAT) != TokenPrefixLenPAT ||
		PrefixLenOfKind(TokenKindIngest) != TokenPrefixLenIngest {
		t.Fatal("PrefixLenOfKind disagrees with the per-kind constants")
	}
}

// ⭐ TestDerivedPrefixSatisfiesTheDDLCheck is the R9 assertion: the Go pattern and
// api_tokens_prefix_ck are kept byte-identical on purpose, and the prefix the
// code derives has to survive the one the database enforces.
//
// The literal below is copied from db/migrations/00003_identity.sql. It admits
// BOTH lengths and always did — the DDL was right and the Go constant was not, so
// this fix needs no migration and the `prefix` column (TEXT) needs no widening.
func TestDerivedPrefixSatisfiesTheDDLCheck(t *testing.T) {
	t.Parallel()

	const ddlCheck = `^oto_(pat|ingest)_[A-Za-z0-9]{4}$`
	if ddlCheck != PatternTokenPrefix {
		t.Fatalf("the Go pattern has drifted from api_tokens_prefix_ck:\n go: %s\nddl: %s",
			PatternTokenPrefix, ddlCheck)
	}
	re := regexp.MustCompile(ddlCheck)

	for _, secret := range []string{
		SecretPrefixPAT + "AbCd" + strings.Repeat("x", 39),
		SecretPrefixIngest + "Wx9Z" + strings.Repeat("y", 39),
	} {
		p, err := PrefixOfSecret(secret)
		if err != nil {
			t.Fatalf("PrefixOfSecret failed: %v", err)
		}
		if !re.MatchString(p.String()) {
			t.Fatalf("derived prefix %q would violate api_tokens_prefix_ck", p.String())
		}
	}
}

// TestNewAPITokenAcceptsAnIngestPrefix walks the whole constructor an ingest mint
// goes through, which is where the 422 actually surfaced.
func TestNewAPITokenAcceptsAnIngestPrefix(t *testing.T) {
	t.Parallel()

	secret := SecretPrefixIngest + "Q1w2" + strings.Repeat("z", 39)
	prefix, err := PrefixOfSecret(secret)
	if err != nil {
		t.Fatalf("PrefixOfSecret: %v", err)
	}
	if _, herr := NewTokenHash(make([]byte, TokenHashBytes-1)); herr == nil {
		t.Fatal("a 31-byte digest must not be accepted as a sha256")
	}
	hash, err := NewTokenHash(append(make([]byte, TokenHashBytes-1), 1))
	if err != nil {
		t.Fatalf("NewTokenHash: %v", err)
	}

	tok, err := NewAPIToken(NewAPITokenParams{
		ID:       uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		OrgID:    uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		Kind:     TokenKindIngest,
		Name:     "ingest:source",
		Hash:     hash,
		Prefix:   prefix,
		SourceID: uuid.MustParse("33333333-3333-4333-8333-333333333333"),
	})
	if err != nil {
		t.Fatalf("NewAPIToken refused a valid ingest token: %v", err)
	}
	if tok.Prefix.String() != "oto_ingest_Q1w2" {
		t.Fatalf("stored prefix = %q", tok.Prefix.String())
	}
}

// TestPrefixErrorsDoNotLeakTheRegex is §L3: a problem+json body must not publish
// an internal invariant to every caller that mistypes something.
func TestPrefixErrorsDoNotLeakTheRegex(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"oto_pat_!!", "nope", "oto_ingest_X"} {
		_, err := NewTokenPrefix(bad)
		if err == nil {
			t.Fatalf("NewTokenPrefix(%q) was accepted", bad)
		}
		if strings.Contains(err.Error(), "[A-Za-z0-9]") || strings.Contains(err.Error(), "^oto_") {
			t.Fatalf("the error leaks the regex: %v", err)
		}
	}
}
