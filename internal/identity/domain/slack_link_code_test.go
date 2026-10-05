package domain

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/thulasiram/oto/internal/platform/errs"
)

func TestASlackLinkCodeIsTenCharactersOfCrockfordShownInTwoGroups(t *testing.T) {
	c, err := NewSlackLinkCode(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	d := c.Display()
	if len(d) != 11 || d[5] != '-' {
		t.Fatalf("display %q is not two groups of five", d)
	}
	for _, r := range strings.ReplaceAll(d, "-", "") {
		if !strings.ContainsRune(slackLinkAlphabet, r) {
			t.Fatalf("display %q carries %q, outside Crockford base32", d, r)
		}
	}
	// ⛔ It never prints itself.
	if s := c.String(); strings.Contains(s, d[:5]) {
		t.Fatalf("String() = %q leaks the code", s)
	}
}

func TestTwoMintedCodesDiffer(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		c, err := NewSlackLinkCode(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if seen[c.Display()] {
			t.Fatalf("a 50-bit code repeated within 200 mints: %s", c.Display())
		}
		seen[c.Display()] = true
	}
}

func TestAShortEntropySourceFailsRatherThanMintingAWeakCode(t *testing.T) {
	if _, err := NewSlackLinkCode(bytes.NewReader([]byte{1, 2, 3})); !errs.IsKind(err, errs.KindInternal) {
		t.Fatalf("err = %v, want an internal error", err)
	}
}

func TestATypedCodeIsReadTheWayAPersonMeantIt(t *testing.T) {
	c, err := ParseSlackLinkCode("ab1d0-fghjk")
	if err != nil {
		t.Fatal(err)
	}
	for _, typed := range []string{"AB1D0FGHJK", " ab1d0 - fghjk ", "ABlDO-FGHJK", "abid0fghjk"} {
		got, err := ParseSlackLinkCode(typed)
		if err != nil {
			t.Fatalf("%q: %v", typed, err)
		}
		h1, _ := got.Hash()
		h2, _ := c.Hash()
		if h1 != h2 {
			t.Fatalf("%q did not normalise to %s", typed, c.Display())
		}
	}
}

func TestAMalformedCodeIsTheSameRefusalAsAWrongOne(t *testing.T) {
	for _, typed := range []string{"", "ABCDE", "ABCDE-FGHJKM", "ABCDE-FGHJU", "ÄBCDE-FGHJK", "ABCDE_FGHJK"} {
		_, err := ParseSlackLinkCode(typed)
		if errs.CodeOf(err) != SlackLinkCodeInvalidCode || !errs.IsKind(err, errs.KindValidation) {
			t.Fatalf("%q: err = %v, want %s", typed, err, SlackLinkCodeInvalidCode)
		}
	}
	if _, err := (SlackLinkCode{}).Hash(); errs.CodeOf(err) != SlackLinkCodeInvalidCode {
		t.Fatalf("the zero code hashed: %v", err)
	}
}

func TestTheLimitsAreTheDesignedOnes(t *testing.T) {
	if SlackLinkCodeLength*5 < 40 {
		t.Fatalf("a code carries %d bits, below the 40 the design requires", SlackLinkCodeLength*5)
	}
	if SlackLinkCodeTTL.Minutes() != 10 || SlackLinkWrongAttemptLimit != 5 || SlackLinkWrongAttemptWindow.Minutes() != 15 {
		t.Fatal("the code's life or the per-user limit moved")
	}
}
