package domain

import (
	"strings"
	"testing"
)

// TestAnEchoKeepsOnlyWhatIsSafeToShow is ValidExternalIncident's whole table (ADR
// 0052 §5, git-bug 506ff21). These are a receiver's bytes, about to be stored and
// rendered as a link for a person to click, so each half is kept only when it is
// exactly what it claims to be — and a bad half is ABSENT, never an error.
func TestAnEchoKeepsOnlyWhatIsSafeToShow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		url, id      string
		wantURL, wID string
	}{
		{"both, well formed", "https://tool.example/incidents/42", "INC-42",
			"https://tool.example/incidents/42", "INC-42"},
		{"a url alone", "https://tool.example/i/1", "", "https://tool.example/i/1", ""},
		{"an id alone", "", "01HZX", "", "01HZX"},
		{"plaintext http is not kept", "http://tool.example/i/1", "INC-1", "", "INC-1"},
		{"a javascript: link is not kept", "javascript:alert(1)", "", "", ""},
		{"a relative url is not kept", "/incidents/42", "", "", ""},
		{"an upper-case scheme is not kept, so the table's CHECK can never refuse it",
			"HTTPS://tool.example/i/1", "", "", ""},
		{"userinfo is not kept", "https://user:pw@tool.example/i/1", "", "", ""},
		{"a url with a space is not kept", "https://tool.example/a b", "", "", ""},
		{"an oversized url is not kept",
			"https://tool.example/" + strings.Repeat("a", MaxExternalURLLength), "", "", ""},
		{"an id with a newline is not kept", "", "INC-1\nX-Injected: 1", "", ""},
		{"an id with surrounding space is not kept", "", " INC-1 ", "", ""},
		{"an oversized id is not kept", "", strings.Repeat("x", MaxExternalIDLength+1), "", ""},
		{"an id at the bound is kept", "", strings.Repeat("x", MaxExternalIDLength),
			"", strings.Repeat("x", MaxExternalIDLength)},
		{"invalid utf-8 is not kept", "", "INC-\xff", "", ""},
	}
	for _, tc := range cases {
		got := ValidExternalIncident(tc.url, tc.id)
		if got.URL != tc.wantURL || got.ID != tc.wID {
			t.Errorf("%s: ValidExternalIncident(%q, %q) = %+v, want {URL:%q ID:%q}",
				tc.name, tc.url, tc.id, got, tc.wantURL, tc.wID)
		}
	}
}
