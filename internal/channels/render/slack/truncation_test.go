package slack

// ⭐ AN INTERNAL TEST, WHICH EVERY OTHER TEST IN THIS DIRECTORY IS NOT — and the
// reason is the claim being pinned. `1cd496f` is about `truncateAt` returning MORE
// than the limit it exists to enforce, for a URL the caller never measured. The
// honest assertion is therefore over `truncateSection` and `truncateField`
// themselves across a range of URL lengths, not over one card that happens to
// exercise them: an external test can only reach them through a Reason whose body
// grows past 3 000 bytes, which pins the card and not the bound.
//
// The end-to-end consequence is already covered — `Validate`'s V5 check refuses an
// over-length section and `Render` returns terminal, which is how the defect
// presented (a dead delivery, not a clipped card).

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// urlOfLen builds an absolute URL of exactly n bytes, or panics if n is too small
// to be one. The lengths below are the point of the test, so they are exact.
func urlOfLen(n int) string {
	const head = "https://oto."
	const tail = ".example.com/groups/x"
	if n < len(head)+len(tail) {
		panic("url length below the shortest well-formed url this test builds")
	}
	return head + strings.Repeat("a", n-len(head)-len(tail)) + tail
}

// TestTruncationNeverExceedsItsOwnLimitForAnyURLLength is the regression this
// ticket exists for. 71 bytes was the old cliff: the suffix costs len(URL)+29
// against what used to be a fixed 100-byte allowance, and the shipped deep link
// (`https://oto.example.com/groups/<uuid>`) is 67 — four bytes of headroom.
func TestTruncationNeverExceedsItsOwnLimitForAnyURLLength(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 6000)

	for _, n := range []int{33, 40, 67, 70, 71, 72, 73, 84, 120, 500, 985, 986, 987,
		1000, 1485, 1486, 1487, 1500, 2000, 2971, 3000, 4000, 5900} {
		url := urlOfLen(n)
		if got := len(url); got != n {
			t.Fatalf("urlOfLen(%d) built %d bytes", n, got)
		}

		if out := truncateSection(long, url); len(out) > maxSectionText {
			t.Errorf("truncateSection with a %d-byte url returned %d bytes, limit %d",
				n, len(out), maxSectionText)
		}
		if out := truncateField(long, url); len(out) > maxFieldText {
			t.Errorf("truncateField with a %d-byte url returned %d bytes, limit %d",
				n, len(out), maxFieldText)
		}
	}
}

// TestTruncationDropsTheLinkRatherThanTheText pins the no-useful-head ruling: the
// text must be at least as long as the pointer to the rest of it, so a link whose
// suffix would take more than half the budget is dropped. The link is duplicated
// elsewhere on the card; the alert text is not duplicated anywhere.
func TestTruncationDropsTheLinkRatherThanTheText(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 6000)
	// The suffix is `"… "` (4 bytes, U+2026 is three) + `<` + url + `|` + the
	// 22-byte label + `>` = len(url)+29. It is kept while len(suffix)*2 <= hard,
	// i.e. while len(url) <= hard/2 - 29. For a section that is 1471.
	const sectionMaxKeptURL = maxSectionText/2 - 29

	kept := truncateSection(long, urlOfLen(sectionMaxKeptURL))
	if !strings.Contains(kept, "|"+seeFullDetail+">") {
		t.Errorf("a %d-byte url is exactly at the ceiling and its link should be kept: %q",
			sectionMaxKeptURL, tail(kept))
	}

	dropped := truncateSection(long, urlOfLen(sectionMaxKeptURL+1))
	if strings.Contains(dropped, seeFullDetail) {
		t.Errorf("a %d-byte url is one byte past the ceiling and its link should be dropped: %q",
			sectionMaxKeptURL+1, tail(dropped))
	}
	if !strings.HasSuffix(dropped, ellipsis) {
		t.Errorf("the link was dropped so a bare ellipsis must remain: %q", tail(dropped))
	}
	// Dropping the link must buy back text, not just shorten the result.
	if len(dropped) <= len(kept)-sectionMaxKeptURL {
		t.Errorf("dropping the link did not return the budget to the text: kept %d, dropped %d",
			len(kept), len(dropped))
	}
}

// TestTruncationLeavesNoDanglingMarkupAtAnyURLLength guards the two invariants the
// cut has always had to preserve, now that the cut position is derived rather than
// fixed: never split a rune, and never split a `<url|label>` span.
func TestTruncationLeavesNoDanglingMarkupAtAnyURLLength(t *testing.T) {
	t.Parallel()

	// A body that is itself full of links, so the cut lands inside one.
	var b strings.Builder
	for b.Len() < 6000 {
		b.WriteString("see <https://oto.example.com/alerts/019fe297|this alert> and ")
	}
	body := b.String()

	for n := 33; n <= 1600; n += 7 {
		out := truncateSection(body, urlOfLen(n))
		if !utf8.ValidString(out) {
			t.Fatalf("a %d-byte url produced invalid utf-8", n)
		}
		if strings.Count(out, "<") != strings.Count(out, ">") {
			t.Fatalf("a %d-byte url left unbalanced mrkdwn: %q", n, tail(out))
		}
		if len(out) > maxSectionText {
			t.Fatalf("a %d-byte url returned %d bytes", n, len(out))
		}
	}
}

// TestTruncationIsInertBelowTheLimit — the common case must not be touched.
func TestTruncationIsInertBelowTheLimit(t *testing.T) {
	t.Parallel()

	short := "an alert fired on a cluster"
	if out := truncateSection(short, urlOfLen(67)); out != short {
		t.Errorf("text under the limit was rewritten: %q", out)
	}
	at := strings.Repeat("y", maxSectionText)
	if out := truncateSection(at, urlOfLen(67)); out != at {
		t.Errorf("text exactly at the limit was rewritten to %d bytes", len(out))
	}
}

func tail(s string) string {
	if len(s) <= 90 {
		return s
	}
	return "…" + s[len(s)-90:]
}
