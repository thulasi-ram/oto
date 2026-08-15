package design

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// webSrc is web/src seen from this package's directory.
func webSrc() string { return filepath.Join("..", "..", "web", "src") }

// tierBDeclaration reads one line of index.css's `@theme inline` block that
// publishes a Tier-B token as a Tailwind colour:
//
//	--color-firing-fill: var(--oto-state-firing-fill);
//
// The capture is the UTILITY name — `firing-fill` — which is the form the ban is
// actually written in. Nothing in the tree says `--oto-state-firing-fill`; it says
// `bg-firing-fill`, and a lint that looked for the token would find nothing and
// pass forever.
var tierBDeclaration = regexp.MustCompile(`(?m)^\s*--color-([a-z0-9-]+)\s*:\s*var\(\s*--oto-state-[a-z0-9-]+\s*\)`)

// concreteStateToken is a direct reference to a Tier-B custom property. The `[a-z]`
// is what keeps the several prose mentions of `--oto-state-*` — the glob, in
// comments explaining this very rule — from reading as violations of it.
var concreteStateToken = regexp.MustCompile(`--oto-state-[a-z]`)

// utilityPrefixes are the Tailwind utilities that can put a colour somewhere.
const utilityPrefixes = `bg|text|border|ring|outline|fill|stroke|from|via|to|shadow|accent|caret|decoration|divide|placeholder`

// chromeExceptions are the files §M.7's rule names as allowed to spend a state
// hue, each with the reason it qualifies.
//
// ⭐ ADDING A LINE HERE IS THE DECISION, NOT THE PAPERWORK. §M.2's whole argument
// is scarcity: saturated colour means "this is the state of an alert" because it
// appears nowhere else, and the way that dies is one reasonable exception at a
// time. A new entry says a new surface in the product is now a state surface.
// Removing one is free; adding one should be argued in review.
var chromeExceptions = map[string]string{
	"components/StateChip.tsx": "IS the state badge — the component §M.7 names first. Its whole " +
		"job is to be the one place a state hue is spent.",
	"features/alerts/AlertTable.tsx": "row status: the firing row's own tint, at 40% so it reads " +
		"as a row and not as a badge.",
	"features/alerts/GroupedAlerts.tsx": "row status: the same tint on a group's member rows.",
	"routes/groups.tsx": "row status: the live-group tint and the status dots, plus the legend " +
		"that decodes those dots — a legend that did not use the hue it explains would explain nothing.",
	"features/alerts/detail/eventKinds.ts": "timeline markers: the third surface §M.7 names. " +
		"One lifecycle colour per event kind, on the marker only.",
}

// TestNoStateHueInChrome is the Tier-A/Tier-B boundary of §M.2, enforced — SPEC §M.7, AC-47.
//
// ⭐⭐ THE BAN IS THE PRODUCT DECISION. §M.2: "Saturated colour on screen means
// exactly one thing: this is the state of an alert. Scarcity is what makes it
// loud." A red used for a destructive button, a green used for a success toast
// and an amber used for a warning banner are each individually defensible and
// collectively fatal — after the third one, the colour that was supposed to mean
// "act now" means "this is a UI".
//
// ⛔ IT READS THE UTILITY NAMES OUT OF `index.css`. The banned set is whatever
// `@theme inline` publishes from an `--oto-state-*` token, so a Tier-B colour
// added there is watched from the moment it exists, and a set that comes back
// empty fails rather than passing over nothing.
func TestNoStateHueInChrome(t *testing.T) {
	t.Parallel()

	indexCSS, err := os.ReadFile(filepath.Join(webSrc(), "index.css"))
	if err != nil {
		t.Fatalf("read web/src/index.css: %v", err)
	}

	var names []string
	for _, m := range tierBDeclaration.FindAllStringSubmatch(string(indexCSS), -1) {
		names = append(names, m[1])
	}
	// Guards the guard. Everything below is "does any file mention one of these",
	// and over an empty list the answer is always no.
	if len(names) < 12 {
		t.Fatalf("found %d Tier-B colour utilities in index.css's @theme block, which is fewer "+
			"than the six states this product has; the declaration reader is broken and this gate "+
			"is asserting nothing", len(names))
	}

	uses := map[string]*regexp.Regexp{}
	for _, name := range names {
		uses[name] = regexp.MustCompile(
			`(?:^|[^0-9A-Za-z_-])(?:` + utilityPrefixes + `)-` + regexp.QuoteMeta(name) + `(?:[^0-9A-Za-z_-]|$)`)
	}

	// The two stylesheets that DEFINE the tokens are not uses of them.
	declarers := map[string]bool{"index.css": true, "design/tokens.css": true}

	seen := map[string]bool{}
	err = filepath.WalkDir(webSrc(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch filepath.Ext(path) {
		case ".ts", ".tsx", ".css":
		default:
			return nil
		}
		rel := filepath.ToSlash(mustRel(t, webSrc(), path))
		if declarers[rel] || isTestSource(rel) {
			return nil
		}

		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := string(source)

		var found []string
		for name, use := range uses {
			if use.MatchString(text) {
				found = append(found, name)
			}
		}
		if concreteStateToken.MatchString(text) {
			found = append(found, "--oto-state-* (the token itself)")
		}
		if len(found) == 0 {
			return nil
		}
		sort.Strings(found)

		if why, ok := chromeExceptions[rel]; ok {
			seen[rel] = true
			t.Logf("%s spends %s — allowed: %s", rel, strings.Join(found, ", "), why)
			return nil
		}
		t.Errorf("%s uses the Tier-B state colour(s) %s.\n"+
			"§M.2: saturated colour on screen means exactly one thing — this is the state of an "+
			"alert — and it is loud only because it appears nowhere else. Chrome uses Tier A "+
			"(surface, line, ink, accent); a status that must read without colour uses an icon and "+
			"a word (AC-46).\n"+
			"If this file IS a state badge, a row status or a timeline marker, add it to "+
			"chromeExceptions in this file with the reason — in review, on purpose.",
			rel, strings.Join(found, ", "))
		return nil
	})
	if err != nil {
		t.Fatalf("walk web/src: %v", err)
	}

	// A stale exception is the other way this gate rots: the entry outlives the
	// use, nobody notices, and the next file to be added under that path inherits
	// a licence nobody granted. It is also the cheapest possible check that the
	// matcher above still matches anything at all.
	for rel := range chromeExceptions {
		if !seen[rel] {
			t.Errorf("chromeExceptions lists %s, which spends no Tier-B colour. Either the file "+
				"stopped needing the exception — delete the line — or the scan above has stopped "+
				"seeing uses, in which case this gate is green over nothing.", rel)
		}
	}
}

// isTestSource reports whether a path under web/src is test code rather than
// product chrome. A test that asserts the badge renders `bg-firing-solid` is
// naming the rule, not breaking it, and forcing an exception line for it would
// make the exception list mean two different things.
func isTestSource(rel string) bool {
	base := filepath.Base(rel)
	return strings.HasPrefix(rel, "test/") ||
		strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.")
}

func mustRel(t *testing.T, base, path string) string {
	t.Helper()

	rel, err := filepath.Rel(base, path)
	if err != nil {
		t.Fatalf("relativise %s against %s: %v", path, base, err)
	}
	return rel
}
