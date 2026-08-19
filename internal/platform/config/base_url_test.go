package config

import (
	"strings"
	"testing"
)

// ⭐ WHY THIS IS A BOOT-TIME TEST AND NOT A RENDERER ONE. `base_url` is the only
// unvalidated dependency of two operator-facing surfaces (git-bug `3f5e952`), and
// both of them degrade to the EMPTY STRING deliberately and silently rather than
// erroring: the Slack card's deep links vanish, and so does the webhook URL an
// operator pastes into Alertmanager. Nothing downstream can tell a blank base URL
// from a deployment that simply has no links, so the only place the distinction
// still exists is here, before anything starts.

func withBaseURL(v string) Config {
	cfg := Default()
	cfg.HTTP.BaseURL = v
	return cfg
}

func TestTheShippedDefaultConfigIsValid(t *testing.T) {
	t.Parallel()
	if err := Validate(Default()); err != nil {
		t.Fatalf("the shipped defaults must validate, or every zero-config start fails: %v", err)
	}
}

func TestABlankBaseURLRefusesToStartRatherThanStartingWithNoLinks(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"", "   "} {
		err := Validate(withBaseURL(v))
		if err == nil {
			t.Fatalf("base_url %q was accepted; oto would start with no deep link on any "+
				"card and no webhook URL to paste into Alertmanager", v)
		}
	}
}

// A scheme-less value is the one that passes a naive non-empty check and still
// produces a card whose link does nothing: Slack will not linkify `oto.example.com`.
func TestABaseURLWithoutASchemeIsRefused(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"oto.example.com", "//oto.example.com", "/groups", "ftp://oto.example.com"} {
		if err := Validate(withBaseURL(v)); err == nil {
			t.Errorf("base_url %q was accepted but is not an http(s) URL", v)
		}
	}
}

// ⛔ THE CHARACTERS THAT CORRUPT A SLACK LINK SILENTLY. `link()` escapes the label
// and never the URL, so each of these reaches the payload verbatim inside
// `<url|label>`: `|` gives the tag a second separator, `>` closes it early, and
// whitespace is not legal in a URL at all. Nothing errors and the card still ships.
func TestABaseURLCarryingMrkdwnControlCharactersIsRefused(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"pipe":     "http://oto.example.com/x|y",
		"gt":       "http://oto.example.com/x>y",
		"lt":       "http://oto.example.com/x<y",
		"space":    "http://oto.example.com/a b",
		"newline":  "http://oto.example.com/a\nb",
		"tab":      "http://oto.example.com/a\tb",
		"carriage": "http://oto.example.com/a\rb",
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := Validate(withBaseURL(v))
			if err == nil {
				t.Fatalf("base_url %q was accepted; it would render as broken mrkdwn on every card", v)
			}
			// ⭐ TWO DIFFERENT REFUSALS REACH HERE AND BOTH ARE CORRECT, which is worth
			// pinning rather than smoothing over. `\n`, `\r` and `\t` fail the
			// `http_url` TAG — Go's parser does reject those — and surface as
			// `Config.HTTP.BaseURL: failed "http_url"`. But `|`, `>`, `<` and a plain
			// SPACE pass the tag (Go's URL parser is permissive about them) and are
			// caught only by the explicit check, which names the character and the byte
			// offset. That asymmetry is exactly why the explicit check exists: the tag
			// alone would have let the four characters that corrupt a Slack link
			// through.
			msg := strings.ToLower(err.Error())
			if !strings.Contains(msg, "base_url") && !strings.Contains(msg, "baseurl") {
				t.Errorf("the refusal names neither spelling of the setting: %v", err)
			}
		})
	}
}

func TestAnOrdinaryBaseURLIsAccepted(t *testing.T) {
	t.Parallel()
	for _, v := range []string{
		"http://localhost:8080",
		"https://oto.example.com",
		"https://oto.internal.example.company.com",
		// Percent-encoding is legitimate and must survive: the Alertmanager silence
		// deep link is built the same way, which is why `link()` does not escape URLs.
		"https://oto.example.com/base%20path",
	} {
		if err := Validate(withBaseURL(v)); err != nil {
			t.Errorf("base_url %q was refused: %v", v, err)
		}
	}
}
