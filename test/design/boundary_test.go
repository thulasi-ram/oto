package design

import (
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// specPath is docs/design/SPEC.md seen from this package's directory.
func specPath() string { return filepath.Join("..", "..", "docs", "design", "SPEC.md") }

// h2Hex reads the hex out of one row of §H.2's state table.
var h2Hex = regexp.MustCompile("(?m)^\\|\\s*`[a-z]+`[^|]*\\|\\s*`(#[0-9a-f]{6})`\\s*\\|\\s*`:")

// slackHexes are the five §H.2 attachment colours, read off the page. The count is
// asserted by the callers: a parser that returns nothing must not read as "no
// violations found".
func slackHexes(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(specPath())
	if err != nil {
		t.Fatalf("read the SPEC: %v", err)
	}
	doc := string(raw)

	start := strings.Index(doc, "### H.2 Palettes (binding)")
	if start == -1 {
		t.Fatalf("%s has no §H.2 heading — this gate reads that section", specPath())
	}
	rest := doc[start+len("### H.2 Palettes (binding)"):]
	if next := regexp.MustCompile("(?m)^#{1,3} ").FindStringIndex(rest); next != nil {
		rest = rest[:next[0]]
	}

	var hexes []string
	for _, m := range h2Hex.FindAllStringSubmatch(rest, -1) {
		hexes = append(hexes, m[1])
	}
	if len(hexes) != 5 {
		t.Fatalf("read %d colours out of §H.2's state table and expected 5", len(hexes))
	}
	return hexes
}

// TestNoSlackHexAppearsInTheWebTree is the first half of §M.6's prohibition,
// enforced — SPEC §M.7, AC-48 ("…and appear nowhere in `web/`").
//
// ⭐ IT IS THE RULE `tokens.css` USED TO STATE AS A COMMENT. The stylesheet said
// "no `#a30200`-family literal may exist anywhere under web/", §M.6 claimed a
// lint rule forbade it, and neither a linter nor a test existed — git-bug
// `c49baaa`. A comment is not a rule.
//
// What it protects: the Slack palette is tuned for a 4 px bar on a channel
// background oto does not control, in a theme oto does not control (§M.6). Pasted
// into the UI it is an unverified pair against oto's own surfaces — and it makes
// the two systems look like one, which is the exact confusion that ends with
// somebody "harmonising" them.
// ⛔ IT WALKS `web/`, NOT `web/src/`, BECAUSE AC-48 SAYS `web/`. The scan used to
// start at `src/` while the criterion it cites says "…and appear nowhere in
// `web/`" — so `index.html`, which is the document every one of these colours
// would be painted into, `vite.config.ts`, `scripts/` and `public/` were all
// outside a rule written to cover them. A gate that reads a subset of what its rule
// names is a gate that is green about the part it did not look at.
//
// The skipped directories are build output and installed dependencies: `#a30200`
// inside `node_modules` is somebody else's stylesheet, and inside `dist/` it is
// this tree's own output, which is checked at its source. `.md` is not read for
// the same reason this file's own sentences are not a violation — prose that names
// a colour paints nothing.
func TestNoSlackHexAppearsInTheWebTree(t *testing.T) {
	t.Parallel()

	hexes := slackHexes(t)
	scanned := 0

	err := filepath.WalkDir(webRoot(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skippedWebDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), "-lock.json") {
			return nil
		}
		switch filepath.Ext(path) {
		case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".css", ".html", ".svg", ".json":
		default:
			return nil
		}
		scanned++
		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := strings.ToLower(string(source))

		for _, hex := range hexes {
			if strings.Contains(text, hex) {
				t.Errorf("%s contains %s, which is a §H.2 SLACK colour.\n"+
					"§M.6: the Slack palette and the oto UI tokens are two systems on purpose — "+
					"different substrate, different contrast contract, different provenance. The "+
					"UI's own state colours are the `--oto-state-*` tokens of §M.4/§M.5.",
					filepath.ToSlash(mustRel(t, webRoot(), path)), hex)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk web/: %v", err)
	}
	// A walk that read nothing reports no violations, which is the same output as a
	// clean tree.
	if scanned == 0 {
		t.Fatal("read no files under web/ — this gate is green over nothing")
	}
}

// webRoot is web/ seen from this package's directory. AC-48's scope is the whole
// tree, not `src/`.
func webRoot() string { return filepath.Join("..", "..", "web") }

// skippedWebDir names the directories under web/ that hold code this repository
// did not write or output it did not hand-edit.
var skippedWebDir = map[string]bool{
	"node_modules":      true,
	"dist":              true,
	"build":             true,
	"coverage":          true,
	"playwright-report": true,
	"test-results":      true,
	".vite":             true,
}

// TestNoOtoTokenReachesTheSlackRenderer is the other half of §M.6's prohibition:
// "A renderer MUST NOT read a `--oto-*` token."
//
// The renderer emits JSON for a surface with no stylesheet and no theme of oto's;
// a `--oto-*` name arriving there would either be dead text in a Slack payload or
// the beginning of somebody wiring the UI's tokens into the card.
//
// ⚠️ IT READS STRING LITERALS, NOT SOURCE TEXT. `palette.go`'s own doc comment
// states this rule, and several tests explain it; a substring search over the
// bytes would fail on the sentences that exist to prevent the thing. What can
// actually reach Slack is a literal.
func TestNoOtoTokenReachesTheSlackRenderer(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..", "internal", "channels", "render")
	files := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".go" {
			return nil
		}
		files++

		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		fset := token.NewFileSet()
		var sc scanner.Scanner
		// Mode 0: comments are skipped rather than returned.
		sc.Init(fset.AddFile(path, fset.Base(), len(source)), source, nil, 0)
		for {
			_, tok, lit := sc.Scan()
			if tok == token.EOF {
				break
			}
			if tok == token.STRING && strings.Contains(lit, "--oto-") {
				t.Errorf("%s carries the string %s, which names an --oto-* design token. §M.6: a "+
					"renderer must not read one — the card's colours are §H.2's, and they are a "+
					"separate system from the UI's.", path, lit)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if files == 0 {
		t.Fatalf("no Go files found under %s; this gate is asserting nothing", root)
	}
}
