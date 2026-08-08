// Command lintvocab is the AC-49 vocabulary gate (SCOPE-BOUNDARY.md §5, SPEC §P-18).
//
// oto is a flight recorder for alerts. It is not an on-call product, and the
// single most reliable predictor of it becoming one is the vocabulary drifting
// first: a word arrives, then the concept the word names, then the column, then
// the rota. SCOPE-BOUNDARY AC-49 therefore bans a fixed list of words and a fixed
// list of person-subject columns across internal/, web/src/ and db/migrations/,
// and says "a lint rule enforces it". This is that lint rule.
//
//	go run ./tools/lintvocab          # non-zero exit on any violation
//	go run ./tools/lintvocab -v       # also print what was suppressed and why
//
// # WHAT IS SCANNED, AND WHY NOT COMMENTS
//
// Comments are stripped before matching. This is deliberate and it is the only
// design that makes the gate enforceable: this codebase's comments name the
// banned words constantly, and every one of those mentions FORBIDS the concept
// ("a policy that routes to a PERSON is a rota", "never MTTR"). A lint that fired
// on those would be turned off within a week, and a lint that is off enforces
// nothing. What is scanned is what the product actually IS: identifiers, literals
// and DDL — including SQL string literals, because a `COMMENT ON COLUMN` is not a
// comment, it is a row in pg_description that ships to every operator.
//
// # SUPPRESSION
//
// A line carrying the marker `vocab:allow` — on the line itself or within the two
// lines above it, which is what a multi-line SQL statement needs — is exempt, and
// the reason after the marker is printed by -v. Two things are exempt
// automatically:
//
//   - `-- +goose Down` sections of a migration. A Down MUST restore the world as
//     it was, banned words included, or the migration is not reversible.
//   - Vendored, minified and lockfile-shaped paths.
//
// Adding a suppression is a decision with a reason attached, which is the point:
// the gate cannot be silently widened.
//
// # THE BASELINE
//
// `baseline.txt` lists `<path>\t<term>` pairs that are known debt: violations
// that exist today and are owned by a module whose rename is a separate change.
// They are reported and do NOT fail the build. Two properties keep the baseline
// from becoming a permanent amnesty:
//
//   - a violation in any path/term NOT on the list fails immediately, so the debt
//     can only shrink;
//   - a baseline line that matches nothing is itself an error, so a fixed file
//     cannot silently keep its exemption.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// roots are the three trees SCOPE-BOUNDARY AC-49 names, verbatim.
var roots = []string{"internal", "web/src", "db/migrations"}

// scanExt is the set of file extensions worth scanning. Anything else (images,
// lockfiles, .gitkeep) cannot express a domain concept.
var scanExt = map[string]bool{
	".go": true, ".sql": true, ".ts": true, ".tsx": true,
	".js": true, ".jsx": true, ".json": true, ".css": true,
}

// rule is one banned term.
type rule struct {
	name string
	re   *regexp.Regexp
	why  string
}

// banned is the AC-49 list. The stems matter more than the exact spellings:
// `escalate_after_seconds` is the same violation as `escalation`, which is
// exactly how it survived the first pass (SPEC §P-20).
var banned = []rule{
	{"escalation", regexp.MustCompile(`(?i)escalat(e|es|ed|ing|ion|ions|ory)`),
		"oto has ONE reminder stage that ends at a CHANNEL; an escalation is a ladder that ends at a PERSON (SPEC §G.9.1). Say unacked_reminder."},
	{"on-call", regexp.MustCompile(`(?i)\bon[-_ ]?call\b`),
		"oto does not know who is on call and must never learn (FR-1, H-1)."},
	{"rota", regexp.MustCompile(`(?i)\brotas?\b`),
		"a schedule of people is the thing oto is not (SCOPE-BOUNDARY §5.3)."},
	{"assignee", regexp.MustCompile(`(?i)\bassign(ee|ees|ed[-_ ]to)\b`),
		"alerts are not assigned to people; there is no ownership axis (§5.1)."},
	{"owner", regexp.MustCompile(`(?i)\bowner(s|ship)?\b`),
		"a signal has no owner. Ownership is the first column of an incident tool (§5.1)."},
	{"responder", regexp.MustCompile(`(?i)\bresponders?\b`),
		"oto records signals, not the humans reacting to them (§5.1)."},
	{"triage", regexp.MustCompile(`(?i)\btriag(e|ed|es|ing)\b`),
		"triage is a workflow over people; oto has acknowledge and snooze, and nothing else (§5.2)."},
	{"postmortem", regexp.MustCompile(`(?i)\bpost[-_ ]?mortems?\b`),
		"the retrospective process is out of scope (§5.4)."},
	{"war room", regexp.MustCompile(`(?i)\bwar[-_ ]?rooms?\b`),
		"coordination surfaces are out of scope (§5.4)."},
	{"SLA", regexp.MustCompile(`\bSLAs?\b`),
		"oto makes no promise about human response time (§A.1)."},
	{"MTTA", regexp.MustCompile(`\bMTTA\b`),
		"time-to-acknowledge measures humans (§A.1)."},
	{"MTTR", regexp.MustCompile(`\bMTTR\b`),
		"time-to-resolve measures humans. oto measures the SIGNAL's firing duration (§A.1, R8)."},
}

// columns are the person-subject columns SCOPE-BOUNDARY forbids by name. A
// column is worse than a word: a word can be renamed, a column has rows.
var columns = []rule{
	{"assigned_to", regexp.MustCompile(`\bassigned_to\b`), "no alert is assigned to anyone."},
	{"owner_id", regexp.MustCompile(`\bowner_(id|team_id)\b`), "no alert has an owner."},
	{"watchers", regexp.MustCompile(`\bwatchers?\b|\bsubscriber_ids\b`), "no alert has watchers."},
	{"incident_id", regexp.MustCompile(`\bincident_id\b`), "oto records alerts, not incidents (§5.4)."},
	{"ticket_id", regexp.MustCompile(`\bticket_id\b`), "oto is not a ticket tracker (§5.4)."},
	{"sla_due_at", regexp.MustCompile(`\bsla_due_at\b`), "oto has no deadline on a human (§A.1)."},
}

const marker = "vocab:allow"

type finding struct {
	path string
	line int
	rule string
	why  string
	text string
}

func main() {
	verbose := flag.Bool("v", false, "print suppressed lines and their stated reasons")
	flag.Parse()

	var findings []finding
	var suppressed []finding
	files := 0

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if skipDir(d.Name()) {
					return fs.SkipDir
				}
				return nil
			}
			if !scanExt[strings.ToLower(filepath.Ext(path))] {
				return nil
			}
			files++
			f, s, err := scanFile(path)
			if err != nil {
				return err
			}
			findings = append(findings, f...)
			suppressed = append(suppressed, s...)
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "lintvocab: %v\n", err)
			os.Exit(2)
		}
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].path != findings[j].path {
			return findings[i].path < findings[j].path
		}
		return findings[i].line < findings[j].line
	})

	if *verbose {
		for _, s := range suppressed {
			fmt.Printf("suppressed %s:%d  [%s]  %s\n", s.path, s.line, s.rule, s.text)
		}
	}

	base, err := loadBaseline()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lintvocab: %v\n", err)
		os.Exit(2)
	}

	var fresh, known []finding
	for _, v := range findings {
		k := key{v.path, v.rule}
		if _, ok := base[k]; ok {
			base[k] = true
			known = append(known, v)
			continue
		}
		fresh = append(fresh, v)
	}

	for _, v := range known {
		fmt.Printf("known debt %s:%d [%s] %s\n", v.path, v.line, v.rule, v.text)
	}

	var stale []string
	for k, hit := range base {
		if !hit {
			stale = append(stale, k.path+"\t"+k.rule)
		}
	}
	sort.Strings(stale)

	for _, v := range fresh {
		fmt.Printf("%s:%d: banned vocabulary %q — %s\n    %s\n", v.path, v.line, v.rule, v.why, v.text)
	}

	if len(fresh) == 0 && len(stale) == 0 {
		fmt.Printf("lintvocab: %d files, %d new violations, %d known debt, %d suppressed (AC-49 clean)\n",
			files, len(fresh), len(known), len(suppressed))
		return
	}

	if len(fresh) > 0 {
		seen := map[string]bool{}
		for _, v := range fresh {
			seen[v.rule] = true
		}
		fmt.Fprintf(os.Stderr,
			"\nlintvocab: %d NEW violations across %d banned terms (SCOPE-BOUNDARY AC-49, SPEC §P-18).\n"+
				"Rename the identifier. If the word is genuinely unavoidable — migration history, a\n"+
				"foreign system's field name — put `%s <reason>` on the line or the line above.\n",
			len(fresh), len(seen), marker)
	}
	for _, s := range stale {
		fmt.Fprintf(os.Stderr,
			"lintvocab: %s in %s matches nothing — the debt was paid; delete the line.\n",
			baselinePath, strings.ReplaceAll(s, "\t", " "))
	}
	os.Exit(1)
}

type key struct{ path, rule string }

const baselinePath = "tools/lintvocab/baseline.txt"

// loadBaseline reads the known-debt list. The value tracks whether the entry was
// matched this run, which is how a stale exemption is caught.
func loadBaseline() (map[key]bool, error) {
	raw, err := os.ReadFile(baselinePath)
	if os.IsNotExist(err) {
		return map[key]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[key]bool{}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		path, rule, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("%s:%d: want '<path>\\t<term>', got %q", baselinePath, i+1, line)
		}
		out[key{strings.TrimSpace(path), strings.TrimSpace(rule)}] = false
	}
	return out, nil
}

func skipDir(name string) bool {
	switch name {
	case "node_modules", "dist", "vendor", ".git", "testdata":
		return true
	}
	return false
}

func scanFile(path string) (bad, sup []finding, err error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path comes from WalkDir over the repo's own three roots
	if err != nil {
		return nil, nil, err
	}
	src := string(raw)
	rawLines := strings.Split(src, "\n")
	code := strings.Split(strip(src, filepath.Ext(path)), "\n")
	down := gooseDownLines(rawLines, filepath.Ext(path))

	for i, line := range code {
		if strings.TrimSpace(line) == "" {
			continue
		}
		hits := match(line)
		if len(hits) == 0 {
			continue
		}
		text := strings.TrimSpace(rawLines[i])
		if len(text) > 160 {
			text = text[:160] + "…"
		}
		for _, h := range hits {
			f := finding{path: path, line: i + 1, rule: h.name, why: h.why, text: text}
			switch {
			case down[i]:
				f.why = "goose Down section: a rollback must restore the old world"
				sup = append(sup, f)
			case allowed(rawLines, i):
				f.why = reason(rawLines, i)
				sup = append(sup, f)
			default:
				bad = append(bad, f)
			}
		}
	}
	return bad, sup, nil
}

func match(line string) []rule {
	var out []rule
	for _, r := range banned {
		if r.re.MatchString(line) {
			out = append(out, r)
		}
	}
	for _, r := range columns {
		if r.re.MatchString(line) {
			out = append(out, r)
		}
	}
	return out
}

// markerLookback is how far above a violation the marker may sit. Two lines is
// what `COMMENT ON COLUMN x IS\n  '<long text>';` needs: the banned word is in the
// literal, and the only sane place to explain it is above the statement.
const markerLookback = 2

// allowed reports whether line i carries a suppression marker, on itself or
// within markerLookback lines above it.
func allowed(lines []string, i int) bool {
	return markerLine(lines, i) >= 0
}

func markerLine(lines []string, i int) int {
	for j := i; j >= 0 && j >= i-markerLookback; j-- {
		if strings.Contains(lines[j], marker) {
			return j
		}
	}
	return -1
}

func reason(lines []string, i int) string {
	src := lines[markerLine(lines, i)]
	_, after, _ := strings.Cut(src, marker)
	after = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(after), ":—-"))
	if after == "" {
		return "(no reason given)"
	}
	return after
}

// gooseDownLines marks the lines of a migration that live below `+goose Down`.
func gooseDownLines(lines []string, ext string) map[int]bool {
	out := map[int]bool{}
	if !strings.EqualFold(ext, ".sql") {
		return out
	}
	inDown := false
	for i, l := range lines {
		if strings.Contains(l, "+goose Down") {
			inDown = true
		}
		if strings.Contains(l, "+goose Up") {
			inDown = false
		}
		out[i] = inDown
	}
	return out
}

// strip blanks out comments while preserving line numbering and string literals.
// It is a lexer, not a regex: `"https://x"` is a string, not a comment, and this
// distinction is the whole reason the naive grep in SCOPE-BOUNDARY §5 could never
// be wired into CI.
func strip(src, ext string) string {
	sql := strings.EqualFold(ext, ".sql")
	json := strings.EqualFold(ext, ".json")

	var b strings.Builder
	b.Grow(len(src))

	const (
		codeSt = iota
		lineComment
		blockComment
		str
	)
	state := codeSt
	var quote byte

	for i := 0; i < len(src); i++ {
		c := src[i]
		next := byte(0)
		if i+1 < len(src) {
			next = src[i+1]
		}

		switch state {
		case codeSt:
			switch {
			case !json && sql && c == '-' && next == '-':
				state = lineComment
				b.WriteString("  ")
				i++
			case !json && !sql && c == '/' && next == '/':
				state = lineComment
				b.WriteString("  ")
				i++
			case !json && !sql && c == '/' && next == '*':
				state = blockComment
				b.WriteString("  ")
				i++
			case c == '"' || c == '\'' || (!sql && !json && c == '`'):
				state, quote = str, c
				b.WriteByte(c)
			default:
				b.WriteByte(c)
			}

		case lineComment:
			if c == '\n' {
				state = codeSt
				b.WriteByte(c)
			} else {
				b.WriteByte(' ')
			}

		case blockComment:
			switch {
			case c == '*' && next == '/':
				state = codeSt
				b.WriteString("  ")
				i++
			case c == '\n':
				b.WriteByte(c)
			default:
				b.WriteByte(' ')
			}

		case str:
			b.WriteByte(c)
			switch {
			case c == '\\' && quote != '\'' && i+1 < len(src):
				// Go/TS escape. SQL escapes a quote by doubling it, handled below.
				b.WriteByte(next)
				i++
			case c == quote:
				if sql && quote == '\'' && next == '\'' {
					b.WriteByte(next)
					i++
					continue
				}
				state = codeSt
			}
		}
	}
	return b.String()
}
