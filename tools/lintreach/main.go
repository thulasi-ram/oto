// Command lintreach is the unreachable-feature gate.
//
// oto was written against a specification and barely executed until late, and the
// defect class that produced is not dead code in the compiler's sense: it is code
// that compiles, lints, type-checks, is exported, is schema-validated and is
// sometimes rendered in a UI form — and is wired to nothing. A setting the
// operator sets that no branch reads. A response field the contract promises that
// no line populates. A service that is fully implemented and never constructed.
// Fifteen-plus instances were found by RUNNING the product. This gate finds them
// by reading it.
//
//	go run ./tools/lintreach              # non-zero exit on any new finding
//	go run ./tools/lintreach -v           # also print known debt and suppressions
//	go run ./tools/lintreach -report      # never fail; print and exit 0
//	go run ./tools/lintreach -write-baseline > tools/lintreach/baseline.txt
//
// # THE THREE CHECKS
//
// All three run on one whole-module go/types pass. Packages are type-checked from
// source; their imports are resolved from the build cache's export data, so there
// is no dependency on x/tools and no second build.
//
//  1. nowrite / noread — a struct field with zero PRODUCTION writes, or zero
//     PRODUCTION reads. Test files are analysed but their reads and writes are
//     counted separately, so a field only a test ever touches is still a finding:
//     that is exactly the shape of RenderOptions.Verbosity.
//
//  2. spec-only / go-only — a `json:` tag name with no property of that name in
//     api/openapi/openapi.yaml, or a schema property with no Go counterpart. This
//     is the check that catches a contract shape with no Go home at all, which no
//     amount of Go-side analysis can see (GroupDetailDTO.threads).
//
//  3. noctor — an exported New* constructor with no caller outside its own file.
//     The Slack Acknowledge service was fully implemented, fully tested, and never
//     constructed; NewInteractionService had zero callers and nothing else in the
//     tree could tell.
//
// # WHAT COUNTS AS A WRITE, AND WHY THE ANSWER IS NOT OBVIOUS
//
// Writes: assignment or compound assignment to a selector, ++/--, taking the
// address of a field, a keyed OR positional composite literal, and a whole-struct
// conversion — `ViolationDTO(v)` writes every field of ViolationDTO and reads
// every field of v. Reads: every other selector use, plus every intermediate
// embedded field on a promoted selection, so `args.PayloadVersion()` counts as a
// read of the embedded `args.Payload`. Those two — conversions and promotion —
// were the ONLY false-positive classes the manual sweep hit, and both are handled
// here rather than baselined.
//
// # REFLECTION
//
// Three libraries write struct fields with no assignment anywhere in the tree:
// encoding/json when unmarshalling, koanf when unmarshalling config, and pgx when
// scanning a row. The rule this gate uses:
//
//   - pgx needs nothing special. Every scan in this tree is `rows.Scan(&x.F)`,
//     which is an address-of a field and therefore already a write.
//
//   - A struct that reaches a decoder is a DECODE TARGET, and its exported fields
//     are treated as written-by-library: the `nowrite` check is suppressed for
//     them. `noread` is NOT suppressed, because "the decoder filled it in and
//     nobody ever looked" is the single most common shape in this repo
//     (OTO_SLACK_APP_TOKEN, channels.config.transport, the whole OTO_INGEST_*
//     group). Unexported fields of a decode target are still checked: reflection
//     cannot reach them.
//
//   - Encoding is deliberately NOT modelled. A response DTO's fields are read by
//     the encoder and by nobody else, so `noread` would fire on every DTO in the
//     tree. Instead `noread` is skipped for a serialization-tagged field UNLESS
//     its struct is a decode target. `nowrite` still applies to every DTO, which
//     is the check that matters there and the one that finds AlertEventDTO.seq.
//
// Reaching a decoder is computed, not guessed: a fixpoint over call arguments and
// generic type arguments, seeded from the external decoder entry points, so
// `optionalBody[UnackRequest]` -> `httpx.Bind[T]` -> `httpx.Decode(dst any)` ->
// `(*json.Decoder).Decode` correctly marks UnackRequest. Struct-typed fields of a
// decode target are decode targets too, which is what carries koanf's single
// `UnmarshalWithConf("", &cfg)` down to every nested config block.
//
// # WHAT THIS GATE CANNOT SEE
//
// Two shapes from the manual sweep survive it, and both are value-level rather
// than reachability-level:
//
//   - A field whose ONLY read is its own defaulting branch. `slack.Config`'s
//     `Transport` is decoded, then `if c.Transport == "" { c.Transport = ... }`,
//     and nothing else ever looks — but that comparison is a read, so the field
//     is not "never read". Catching it needs to know that a read feeding only a
//     write of the same field is a no-op.
//   - A field that IS populated, always with the same dead value.
//     `SilenceDTO.AlertmanagerURL` is assigned from `alertmanagerURL()`, which
//     is `return ""`. Reachability is fine here; the constant is the defect.
//
// Both are named so nobody mistakes a green run for proof.
//
// # SUPPRESSION AND THE BASELINE
//
// `//oto:reachable-ok <reason>` on the declaration or in its doc comment exempts
// it. The reason is mandatory and a suppression that matches nothing is an error,
// so the escape hatch cannot rot.
//
// `baseline.txt` is `<key>\t<reason>`, one per line: findings that exist today and
// whose fix belongs to a change other than this one. They are reported and do not
// fail the build. A finding NOT on the list fails immediately, so the debt can
// only shrink, and a baseline line that matches nothing is itself an error, so a
// paid debt cannot keep its exemption.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	modulePath   = "github.com/thulasiram/oto"
	marker       = "//oto:reachable-ok"
	baselinePath = "tools/lintreach/baseline.txt"
	openAPIPath  = "api/openapi/openapi.yaml"
)

// declRoots are the trees whose declarations are CHECKED. Uses are counted from
// everywhere in the module, including cmd/ and test/, so a type declared in
// internal/ and only ever constructed from cmd/ is not a finding.
var declRoots = []string{modulePath + "/internal/", modulePath + "/pkg/", modulePath + "/cmd/"}

// decoderNames are the method and function names that write a struct through
// reflection. Matched only on packages OUTSIDE this module: a decoder inside the
// module is followed properly, by the fixpoint below.
var decoderNames = map[string]bool{
	"Unmarshal": true, "UnmarshalWithConf": true, "UnmarshalJSON": true,
	"UnmarshalYAML": true, "UnmarshalText": true, "Decode": true, "DecodeContext": true,
}

func main() {
	verbose := flag.Bool("v", false, "print known debt and accepted suppressions")
	report := flag.Bool("report", false, "report findings but always exit 0")
	writeBase := flag.Bool("write-baseline", false, "print a baseline covering every current finding")
	flag.Parse()

	a := newAnalyzer()
	if err := a.load(); err != nil {
		fmt.Fprintf(os.Stderr, "lintreach: %v\n", err)
		os.Exit(2)
	}
	a.resolveDecodeTargets()

	findings := a.findings()
	if err := a.openAPICheck(&findings); err != nil {
		fmt.Fprintf(os.Stderr, "lintreach: %v\n", err)
		os.Exit(2)
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].key < findings[j].key })

	kept, suppressed := a.applySuppressions(findings)
	if *writeBase {
		emitBaseline(kept)
		return
	}

	base, err := loadBaseline()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lintreach: %v\n", err)
		os.Exit(2)
	}

	var fresh, known []finding
	for _, f := range kept {
		if _, ok := base[f.key]; ok {
			base[f.key] = true
			known = append(known, f)
			continue
		}
		fresh = append(fresh, f)
	}

	if *verbose {
		for _, s := range suppressed {
			fmt.Printf("suppressed %s — %s\n", s.key, s.why)
		}
		for _, k := range known {
			fmt.Printf("known debt %s\n", k.String())
		}
	}
	for _, f := range fresh {
		fmt.Printf("%s\n", f.String())
	}

	var stale []string
	for k, hit := range base {
		if !hit {
			stale = append(stale, k)
		}
	}
	for _, s := range a.unusedMarkers() {
		stale = append(stale, "suppression "+s)
	}
	sort.Strings(stale)

	fmt.Printf("lintreach: %d packages, %d fields, %d decode targets — %d new, %d known debt, %d suppressed\n",
		a.pkgCount, len(a.fields), a.decodeCount, len(fresh), len(known), len(suppressed))

	// A package that did not type-check contributes no reads and no writes, so
	// every field it touches reads as unreachable. Silence here would be the
	// worst failure mode this gate has: a wall of confident false positives.
	if len(a.errs) > 0 {
		for _, e := range a.errs[:min(len(a.errs), 10)] {
			fmt.Fprintf(os.Stderr, "lintreach: type error: %s\n", e)
		}
		fmt.Fprintf(os.Stderr,
			"lintreach: %d type errors — the tree does not type-check, so these findings\n"+
				"are not trustworthy. Fix the build first.\n", len(a.errs))
		os.Exit(2)
	}

	if len(fresh) == 0 && len(stale) == 0 {
		return
	}
	if len(fresh) > 0 {
		fmt.Fprintf(os.Stderr,
			"\nlintreach: %d NEW unreachable declarations.\n"+
				"Wire it up, or delete it. If it is genuinely reachable by a route this analyzer\n"+
				"cannot see, put `%s <reason>` on the declaration or in its doc comment.\n",
			len(fresh), marker)
	}
	for _, s := range stale {
		fmt.Fprintf(os.Stderr, "lintreach: %s matches nothing — the debt was paid; delete the line.\n", s)
	}
	if *report {
		return
	}
	os.Exit(1)
}

// ---------------------------------------------------------------- the findings

type finding struct {
	key  string // stable, baseline-able identity
	pos  string // file:line, or "" for contract-level findings
	why  string
	note string
}

func (f finding) String() string {
	if f.pos == "" {
		return fmt.Sprintf("%s: %s", f.key, f.why)
	}
	return fmt.Sprintf("%s: %s\n    %s — %s", f.pos, f.key, f.why, f.note)
}

// ------------------------------------------------------------------- the model

type fieldRec struct {
	typeKey  string // "<pkgpath>.<TypeName>"
	name     string
	pos      token.Position
	tag      string
	exported bool
	embedded bool
	reads    [2]int // [prod, test]
	writes   [2]int
}

type ctorRec struct {
	key      string
	pos      token.Position
	declFile string
	uses     []ctorUse
}

// ctorUse is one reference to a New* function. viaExported records whether the
// reference sits inside an exported sibling: `NewKeyring` is called only by
// `NewKeyringFromBase64` in the same file, and that wrapper IS constructed from
// the composition root, so the constructor is reached — just indirectly.
type ctorUse struct {
	file        string
	viaExported bool
}

// argRef is what a call argument, or a generic type argument, refers to: a
// concrete struct type, a parameter of the enclosing function, or a type
// parameter of the enclosing function. It is the edge the decode fixpoint walks.
type argRef struct {
	typeKey  string // concrete named struct
	fnKey    string // enclosing function
	index    int    // param or type-param index within fnKey
	typeParm bool
}

type edge struct {
	callee string
	index  int
	ref    argRef
}

type analyzer struct {
	fset     *token.FileSet
	fields   map[string]*fieldRec // "<pkgpath>.<Type>.<Field>"
	structs  map[string][]string  // typeKey -> field names, in declaration order
	fieldTyp map[string][]string  // typeKey -> struct typeKeys reachable from its fields
	ctors    map[string]*ctorRec
	apiTags  map[string]token.Position // json tag name -> first sighting, api packages only
	allTags  map[string]bool           // json tag name, anywhere in the module

	argEdges  []edge // callee param i  <- ref
	typeEdges []edge // callee type-param i <- ref
	seeds     []argRef

	decode      map[string]bool
	decodeCount int

	marks     map[string]map[int]string // file -> line -> reason
	markUsed  map[string]map[int]bool
	generated map[string]bool

	units    []unit
	pkgCount int
	errs     []string
}

func newAnalyzer() *analyzer {
	return &analyzer{
		fset: token.NewFileSet(), fields: map[string]*fieldRec{},
		structs: map[string][]string{}, fieldTyp: map[string][]string{},
		ctors: map[string]*ctorRec{}, apiTags: map[string]token.Position{},
		allTags: map[string]bool{},
		decode:  map[string]bool{}, marks: map[string]map[int]string{},
		markUsed: map[string]map[int]bool{}, generated: map[string]bool{},
	}
}

// ------------------------------------------------------------------ go/types

type listPkg struct {
	ImportPath    string
	Export        string
	Dir           string
	GoFiles       []string
	TestGoFiles   []string
	XTestGoFiles  []string
	ForTest       string
	Module        *struct{ Path string }
	DepOnly       bool
	CompiledGoFil []string
}

// load runs `go list` once for the export-data map and the file lists, then
// type-checks every package of this module from source.
func (a *analyzer) load() error {
	cmd := exec.Command("go", "list", "-e", "-deps", "-test", "-export",
		"-json=ImportPath,Export,Dir,GoFiles,TestGoFiles,XTestGoFiles,ForTest,Module,DepOnly", "./...")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("go list: %w", err)
	}

	exportOf := map[string]string{}
	var own []listPkg
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var p listPkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("go list output: %w", err)
		}
		// `pkg [pkg.test]` is the test-augmented variant of a package this loop
		// already has, and `pkg.test` is the synthetic test main, whose only
		// "GoFile" is a generated _testmain.go that lives in the build cache and
		// has nothing to do with this repository.
		if p.ForTest != "" || strings.Contains(p.ImportPath, " [") || strings.HasSuffix(p.ImportPath, ".test") {
			continue
		}
		if p.Export != "" {
			if _, seen := exportOf[p.ImportPath]; !seen {
				exportOf[p.ImportPath] = p.Export
			}
		}
		if p.Module != nil && p.Module.Path == modulePath && len(p.GoFiles) > 0 {
			own = append(own, p)
		}
	}
	sort.Slice(own, func(i, j int) bool { return own[i].ImportPath < own[j].ImportPath })

	// exportOnly, not the importer itself: go/types prefers an ImporterFrom and
	// calls ImportFrom(path, pkgDir), and gc's importer then resolves the export
	// file relative to pkgDir — joining two absolute paths into nonsense. Hiding
	// ImportFrom forces the srcDir-free Import(path), which is the only shape
	// that is correct when the caller already knows every export file's location.
	imp := exportOnly{importer.ForCompiler(a.fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exportOf[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %q", path)
		}
		return os.Open(f) //nolint:gosec // path comes from `go list -export` on this module
	})}

	// Phase one type-checks and declares. Phase two counts. They cannot be one
	// loop: a write to alerts/domain.AlertFilter lives in alerts/api, which is
	// checked first, and a count against a field not yet declared is a count lost
	// — which reads, from the outside, exactly like an unreachable field.
	for _, p := range own {
		if strings.HasPrefix(p.ImportPath, modulePath+"/tools/") {
			continue // the gate does not gate itself
		}
		a.pkgCount++
		a.checkOne(imp, p.ImportPath, p.Dir, append(append([]string{}, p.GoFiles...), p.TestGoFiles...))
		if len(p.XTestGoFiles) > 0 {
			a.checkOne(imp, p.ImportPath+"_test", p.Dir, p.XTestGoFiles)
		}
	}
	for _, u := range a.units {
		for _, f := range u.files {
			a.walk(f, u.pkg, u.info)
		}
	}
	return nil
}

// exportOnly narrows an importer to types.Importer, hiding ImportFrom.
type exportOnly struct{ inner types.Importer }

func (e exportOnly) Import(path string) (*types.Package, error) { return e.inner.Import(path) }

// unit is one type-checked package, held between the two phases.
type unit struct {
	pkg   *types.Package
	files []*ast.File
	info  *types.Info
}

func (a *analyzer) checkOne(imp types.Importer, path, dir string, names []string) {
	var files []*ast.File
	for _, n := range names {
		f, err := parser.ParseFile(a.fset, filepath.Join(dir, n), nil, parser.ParseComments)
		if err != nil {
			a.errs = append(a.errs, err.Error())
			continue
		}
		files = append(files, f)
		a.collectMarkers(f)
		if isGenerated(f) {
			a.generated[a.fset.Position(f.Pos()).Filename] = true
		}
	}
	if len(files) == 0 {
		return
	}
	info := &types.Info{
		Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Types:      map[ast.Expr]types.TypeAndValue{},
		Instances:  map[*ast.Ident]types.Instance{},
	}
	conf := types.Config{Importer: imp, Error: func(err error) { a.errs = append(a.errs, err.Error()) }}
	pkg, _ := conf.Check(path, a.fset, files, info)
	if pkg == nil {
		return
	}
	if !strings.HasSuffix(path, "_test") {
		a.declareStructs(pkg)
	}
	a.units = append(a.units, unit{pkg: pkg, files: files, info: info})
}

// ctorUse attributes one reference to an exported New* function. A reference
// from the declaring file proves nothing on its own: the Acknowledge service's
// own file referenced NewInteractionService and the composition root never did.
func (a *analyzer) ctorUse(id *ast.Ident, info *types.Info, enclosing *types.Func, file string) {
	fn, ok := info.Uses[id].(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Signature().Recv() != nil {
		return
	}
	if !fn.Exported() || !strings.HasPrefix(fn.Name(), "New") || !inDeclRoot(fn.Pkg().Path()) {
		return
	}
	key := fn.Pkg().Path() + "." + fn.Name()
	rec := a.ctors[key]
	if rec == nil {
		rec = &ctorRec{key: key}
		a.ctors[key] = rec
	}
	via := enclosing != nil && enclosing.Exported() && enclosing != fn
	rec.uses = append(rec.uses, ctorUse{file: file, viaExported: via})
}

// declareStructs records every named struct declared at package scope. Types
// declared inside a test file or a generated file are skipped: a test's own
// fixture struct is not product surface, and generated code is not edited.
func (a *analyzer) declareStructs(pkg *types.Package) {
	inRoot := false
	for _, r := range declRoots {
		if strings.HasPrefix(pkg.Path()+"/", r) {
			inRoot = true
		}
	}
	if !inRoot {
		return
	}
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		tn, ok := scope.Lookup(name).(*types.TypeName)
		if !ok || tn.IsAlias() {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok {
			continue
		}
		st, ok := named.Underlying().(*types.Struct)
		if !ok {
			continue
		}
		pos := a.fset.Position(tn.Pos())
		if isTestFile(pos.Filename) || a.generated[pos.Filename] {
			continue
		}
		typeKey := pkg.Path() + "." + name
		for i := range st.NumFields() {
			f := st.Field(i)
			fk := typeKey + "." + f.Name()
			if _, dup := a.fields[fk]; dup {
				continue
			}
			a.fields[fk] = &fieldRec{
				typeKey: typeKey, name: f.Name(), pos: a.fset.Position(f.Pos()),
				tag: st.Tag(i), exported: f.Exported(), embedded: f.Embedded(),
			}
			a.structs[typeKey] = append(a.structs[typeKey], f.Name())
			a.fieldTyp[typeKey] = append(a.fieldTyp[typeKey], structKeysIn(f.Type())...)
			if tag := jsonName(st.Tag(i)); tag != "" {
				a.allTags[tag] = true
				if _, seen := a.apiTags[tag]; !seen && strings.Contains(pkg.Path(), "/api") {
					a.apiTags[tag] = a.fset.Position(f.Pos())
				}
			}
		}
	}
}

// ------------------------------------------------------------------- the walk

// walk visits one file. Declarations are walked one at a time so the enclosing
// function is known exactly; a FuncLit resets it to nil, because a closure's
// parameter indices are not its outer function's.
func (a *analyzer) walk(f *ast.File, pkg *types.Package, info *types.Info) {
	file := a.fset.Position(f.Pos()).Filename
	v := &visitor{a: a, info: info, written: map[*ast.SelectorExpr]bool{}, file: file}
	if isTestFile(file) {
		v.slot = 1
	}
	for _, d := range f.Decls {
		dv := *v
		if fd, ok := d.(*ast.FuncDecl); ok {
			dv.fn, _ = info.Defs[fd.Name].(*types.Func)
			if fd.Recv == nil && fd.Name.IsExported() && strings.HasPrefix(fd.Name.Name, "New") &&
				!isTestFile(file) && !a.generated[file] && inDeclRoot(pkg.Path()) {
				key := pkg.Path() + "." + fd.Name.Name
				rec := a.ctors[key]
				if rec == nil {
					rec = &ctorRec{key: key}
					a.ctors[key] = rec
				}
				rec.pos, rec.declFile = a.fset.Position(fd.Name.Pos()), file
			}
		}
		ast.Walk(&dv, d)
	}
}

// visitor carries the per-declaration state the walk needs: which selectors are
// in a write position, whether this file is a test, and which function encloses
// the node being visited.
type visitor struct {
	a       *analyzer
	info    *types.Info
	written map[*ast.SelectorExpr]bool
	file    string
	slot    int
	fn      *types.Func
}

func (v *visitor) Visit(n ast.Node) ast.Visitor {
	a := v.a
	switch n := n.(type) {
	case nil:
		return nil
	case *ast.FuncLit:
		nv := *v
		nv.fn = nil
		return &nv
	case *ast.AssignStmt:
		for _, lhs := range n.Lhs {
			if sel, ok := lhs.(*ast.SelectorExpr); ok {
				v.written[sel] = true
				a.record(sel, v.info, v.slot, true)
				if n.Tok != token.ASSIGN && n.Tok != token.DEFINE {
					a.record(sel, v.info, v.slot, false) // x.F += y reads too
				}
			}
		}
	case *ast.IncDecStmt:
		if sel, ok := n.X.(*ast.SelectorExpr); ok {
			v.written[sel] = true
			a.record(sel, v.info, v.slot, true)
			a.record(sel, v.info, v.slot, false)
		}
	case *ast.UnaryExpr:
		if sel, ok := n.X.(*ast.SelectorExpr); ok && n.Op == token.AND {
			v.written[sel] = true
			a.record(sel, v.info, v.slot, true)
			a.record(sel, v.info, v.slot, false)
		}
	case *ast.Ident:
		a.ctorUse(n, v.info, v.fn, v.file)
	case *ast.CompositeLit:
		a.compositeLit(n, v.info, v.slot)
	case *ast.CallExpr:
		a.conversion(n, v.info, v.slot)
		a.call(n, v.info, v.fn)
	case *ast.SelectorExpr:
		// `h.mu.Lock()` is TWO selections: `h.mu` picks the field, and `.Lock`
		// picks a method whose receiver is that field. A pointer-receiver method
		// may mutate in place, so the field is written — which is the only thing
		// that stops every sync.Mutex and every bytes.Buffer in the tree from
		// reading as a field nothing ever populates. Pre-order walk means this
		// runs before the inner selector is visited.
		if s := v.info.Selections[n]; s != nil && s.Kind() != types.FieldVal && ptrRecv(s) {
			if inner, ok := n.X.(*ast.SelectorExpr); ok && !v.written[inner] {
				v.written[inner] = true
				a.record(inner, v.info, v.slot, true)
				a.record(inner, v.info, v.slot, false)
			}
		}
		if !v.written[n] {
			a.record(n, v.info, v.slot, false)
		}
	}
	return v
}

// ptrRecv reports whether a method selection's receiver is a pointer, which is
// the difference between "this call may have mutated the field" and "it cannot".
func ptrRecv(s *types.Selection) bool {
	fn, ok := s.Obj().(*types.Func)
	if !ok {
		return false
	}
	r := fn.Signature().Recv()
	if r == nil {
		return false
	}
	_, isPtr := types.Unalias(r.Type()).(*types.Pointer)
	return isPtr
}

// record attributes one selector to the field it names, and to every embedded
// field it traverses on the way. The intermediate hops are what make promoted
// access — `args.PayloadVersion()` reaching a method on the embedded Payload —
// count as a read of the embedded field rather than a false positive.
func (a *analyzer) record(sel *ast.SelectorExpr, info *types.Info, slot int, write bool) {
	s := info.Selections[sel]
	if s == nil {
		return
	}
	idx := s.Index()
	hops := len(idx)
	mutating := false
	if s.Kind() != types.FieldVal {
		hops-- // the final index selects a method, not a field
		// `w.buf.WriteString(..)` and `c.mu.Lock()` never assign to buf or mu,
		// and both fields are written all the same. A method with a POINTER
		// receiver may mutate its receiver in place, so calling one counts as a
		// write of the field it was called on. Without this every sync.Mutex and
		// every bytes.Buffer in the tree is a false positive.
		mutating = ptrRecv(s)
	}
	t := s.Recv()
	for k := range hops {
		named, st := namedOf(t), structOf(t)
		if st == nil || idx[k] >= st.NumFields() {
			return
		}
		fv := st.Field(idx[k])
		if named != nil && named.Obj().Pkg() != nil {
			key := named.Obj().Pkg().Path() + "." + named.Obj().Name() + "." + fv.Name()
			if rec := a.fields[key]; rec != nil {
				last := k == hops-1
				// Only the final hop can be a write; every hop before it is an
				// embedded field that had to be READ to reach the target.
				if write && last {
					rec.writes[slot]++
				} else {
					rec.reads[slot]++
				}
				if last && mutating {
					rec.writes[slot]++
				}
			}
		}
		t = fv.Type()
	}
}

func (a *analyzer) compositeLit(cl *ast.CompositeLit, info *types.Info, slot int) {
	t := info.Types[cl].Type
	named := namedOf(t)
	st := structOf(t)
	if named == nil || st == nil || named.Obj().Pkg() == nil {
		return
	}
	prefix := named.Obj().Pkg().Path() + "." + named.Obj().Name() + "."
	for i, elt := range cl.Elts {
		name := ""
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			id, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			name = id.Name
		} else if i < st.NumFields() {
			name = st.Field(i).Name()
		}
		if rec := a.fields[prefix+name]; rec != nil {
			rec.writes[slot]++
		}
	}
}

// conversion handles `Dst(src)` between two struct types. Every field of Dst is
// written and every field of src is read, which is the difference between a
// clean run and fifteen false positives.
func (a *analyzer) conversion(call *ast.CallExpr, info *types.Info, slot int) {
	if len(call.Args) != 1 || !info.Types[call.Fun].IsType() {
		return
	}
	dst, src := info.Types[call.Fun].Type, info.Types[call.Args[0]].Type
	if structOf(dst) == nil || structOf(src) == nil {
		return
	}
	a.touchAll(dst, slot, true)
	a.touchAll(src, slot, false)
}

func (a *analyzer) touchAll(t types.Type, slot int, write bool) {
	named := namedOf(t)
	if named == nil || named.Obj().Pkg() == nil {
		return
	}
	prefix := named.Obj().Pkg().Path() + "." + named.Obj().Name() + "."
	for _, name := range a.structs[named.Obj().Pkg().Path()+"."+named.Obj().Name()] {
		rec := a.fields[prefix+name]
		if rec == nil {
			continue
		}
		if write {
			rec.writes[slot]++
		} else {
			rec.reads[slot]++
		}
	}
}

// ------------------------------------------------------- the decode-target pass

// call records the edges the decode fixpoint needs, and seeds it whenever an
// argument reaches a decoder that lives outside this module.
func (a *analyzer) call(call *ast.CallExpr, info *types.Info, enclosing *types.Func) {
	fn := calleeFunc(call.Fun, info)
	if fn == nil || fn.Pkg() == nil {
		return
	}
	external := !strings.HasPrefix(fn.Pkg().Path(), modulePath)
	sink := external && decoderNames[fn.Name()]
	callee := funcKey(fn)

	for i, arg := range call.Args {
		for _, ref := range a.refsOf(arg, info, enclosing) {
			switch {
			case sink:
				a.seeds = append(a.seeds, ref)
			case !external:
				a.argEdges = append(a.argEdges, edge{callee: callee, index: i, ref: ref})
			}
		}
	}
	if external {
		return
	}
	id := calleeIdent(call.Fun)
	if id == nil {
		return
	}
	inst, ok := info.Instances[id]
	if !ok || inst.TypeArgs == nil {
		return
	}
	for i := range inst.TypeArgs.Len() {
		ta := inst.TypeArgs.At(i)
		if named := namedOf(ta); named != nil && named.Obj().Pkg() != nil {
			a.typeEdges = append(a.typeEdges, edge{callee: callee, index: i,
				ref: argRef{typeKey: named.Obj().Pkg().Path() + "." + named.Obj().Name()}})
			continue
		}
		if tp, ok := types.Unalias(ta).(*types.TypeParam); ok && enclosing != nil {
			a.typeEdges = append(a.typeEdges, a.tpEdge(callee, i, tp, enclosing))
		}
	}
}

func (a *analyzer) tpEdge(callee string, i int, tp *types.TypeParam, enclosing *types.Func) edge {
	return edge{callee: callee, index: i,
		ref: argRef{fnKey: funcKey(enclosing), index: tp.Index(), typeParm: true}}
}

// refOf classifies one argument expression. Only three shapes matter: a value of
// a concrete named struct type, an identifier that IS a parameter of the
// enclosing function, and the address of a local whose type is a type parameter
// (which is exactly `var dto T; Decode(w, r, &dto, ...)` in httpx.Bind).
func (a *analyzer) refsOf(arg ast.Expr, info *types.Info, enclosing *types.Func) []argRef {
	inner := arg
	if u, ok := arg.(*ast.UnaryExpr); ok && u.Op == token.AND {
		inner = u.X
	}
	t := info.Types[inner].Type
	if t == nil {
		return nil
	}
	var out []argRef
	if tp, ok := types.Unalias(deref(t)).(*types.TypeParam); ok && enclosing != nil {
		out = append(out, argRef{fnKey: funcKey(enclosing), index: tp.Index(), typeParm: true})
	}
	// Every named struct reachable through the argument's type, so that
	// `GetJSON(ctx, path, q, &w)` with `w []wireAlert` marks wireAlert and not
	// the anonymous slice that is the only thing the expression names.
	for _, k := range structKeysIn(t) {
		out = append(out, argRef{typeKey: k})
	}
	if id, ok := inner.(*ast.Ident); ok && enclosing != nil {
		if v, ok := info.Uses[id].(*types.Var); ok {
			sig := enclosing.Signature()
			for i := range sig.Params().Len() {
				if sig.Params().At(i) == v {
					out = append(out, argRef{fnKey: funcKey(enclosing), index: i})
				}
			}
		}
	}
	return out
}

// resolveDecodeTargets runs the fixpoint, then closes over struct-typed fields:
// koanf unmarshals into `*Config` exactly once, and every nested config block
// must inherit the same status or the whole config tree false-positives.
func (a *analyzer) resolveDecodeTargets() {
	sinks := map[string]bool{}  // "fnKey#i" — a parameter that reaches a decoder
	tsinks := map[string]bool{} // "fnKey#i" — a type parameter that does
	apply := func(r argRef) bool {
		switch {
		case r.typeKey != "":
			if a.decode[r.typeKey] {
				return false
			}
			a.decode[r.typeKey] = true
		case r.typeParm:
			k := r.fnKey + "#" + strconv.Itoa(r.index)
			if tsinks[k] {
				return false
			}
			tsinks[k] = true
		default:
			k := r.fnKey + "#" + strconv.Itoa(r.index)
			if sinks[k] {
				return false
			}
			sinks[k] = true
		}
		return true
	}
	for _, s := range a.seeds {
		apply(s)
	}
	for changed := true; changed; {
		changed = false
		for _, e := range a.argEdges {
			if sinks[e.callee+"#"+strconv.Itoa(e.index)] && apply(e.ref) {
				changed = true
			}
		}
		for _, e := range a.typeEdges {
			if tsinks[e.callee+"#"+strconv.Itoa(e.index)] && apply(e.ref) {
				changed = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for k := range a.decode {
			for _, ref := range a.fieldTyp[k] {
				if !a.decode[ref] {
					a.decode[ref] = true
					changed = true
				}
			}
		}
	}
	a.decodeCount = len(a.decode)
}

// ------------------------------------------------------------------- reporting

func (a *analyzer) findings() []finding {
	var out []finding
	for key, rec := range a.fields {
		if rec.embedded {
			// An embedded field is a composition device, not data. It is written
			// by every literal that names it and read by every promoted
			// selection through it, and neither is worth reporting: whether the
			// EMBEDDED TYPE is reachable is answered by its own fields.
			continue
		}
		decodeTarget := a.decode[rec.typeKey]
		libWritten := decodeTarget && rec.exported
		libRead := serialTag(rec.tag) && !decodeTarget

		switch {
		case rec.writes[0] == 0 && !libWritten:
			note := "nothing populates it"
			if rec.writes[1] > 0 {
				note = fmt.Sprintf("written by %d test site(s) and nothing else", rec.writes[1])
			}
			out = append(out, finding{key: "nowrite:" + key, pos: pos(rec.pos),
				why: "field is never written in production", note: note})
		case rec.reads[0] == 0 && !libRead:
			note := "the value goes nowhere"
			if rec.reads[1] > 0 {
				note = fmt.Sprintf("read by %d test site(s) and nothing else", rec.reads[1])
			}
			out = append(out, finding{key: "noread:" + key, pos: pos(rec.pos),
				why: "field is never read in production", note: note})
		}
	}
	for key, rec := range a.ctors {
		if rec.declFile == "" {
			continue
		}
		prod, test := 0, 0
		for _, u := range rec.uses {
			switch {
			case isTestFile(u.file):
				test++
			case u.file != rec.declFile || u.viaExported:
				prod++
			}
		}
		if prod > 0 {
			continue
		}
		note := "the thing it builds is never constructed"
		if test > 0 {
			note = fmt.Sprintf("called by %d test site(s) and nothing else", test)
		}
		out = append(out, finding{key: "noctor:" + key, pos: pos(rec.pos),
			why: "exported constructor has no caller outside its own file", note: note})
	}
	return out
}

// openAPICheck compares json tag names against the contract's property names, in
// both directions. It is deliberately name-level and not schema-level: a
// per-schema match would need a full $ref resolver and would trade the two
// findings that matter for a stream of shape-mismatch noise.
func (a *analyzer) openAPICheck(out *[]finding) error {
	raw, err := os.ReadFile(openAPIPath)
	if err != nil {
		return err
	}
	props, params := openAPIProperties(string(raw))
	for name := range props {
		if !a.allTags[name] {
			*out = append(*out, finding{key: "spec-only:" + name,
				why: "openapi.yaml declares this property; no Go json tag produces it"})
		}
	}
	for name, p := range a.apiTags {
		if !props[name] && !params[name] {
			*out = append(*out, finding{key: "go-only:" + name, pos: pos(p),
				why:  "a Go api DTO serialises this key; openapi.yaml declares no such property",
				note: "the contract and the server disagree"})
		}
	}
	return nil
}

var (
	keyRe   = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_.-]*):`)
	paramRe = regexp.MustCompile(`(?m)^\s*(?:-\s+)?name:\s*'?([A-Za-z_][A-Za-z0-9_.-]*)'?\s*$`)
)

// openAPIProperties collects every key that sits directly under a `properties:`
// mapping, at any depth, by tracking indentation.
func openAPIProperties(src string) (props, params map[string]bool) {
	out := map[string]bool{}
	var stack []int
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
			continue
		}
		ind := len(line) - len(trimmed)
		for len(stack) > 0 && ind <= stack[len(stack)-1] {
			stack = stack[:len(stack)-1]
		}
		m := keyRe.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		if len(stack) > 0 && ind == stack[len(stack)-1]+2 {
			out[m[1]] = true
		}
		if m[1] == "properties" {
			stack = append(stack, ind)
		}
	}
	// Query and path parameters are `- name: foo` list items, not properties. A
	// query DTO's json tags are contract names all the same, and without this
	// every one of them reads as a server field the contract never declared. They
	// are kept SEPARATE from properties: a parameter with no Go tag is not a
	// dangling response field, it is a parameter the router reads by name.
	params = map[string]bool{}
	for _, m := range paramRe.FindAllStringSubmatch(src, -1) {
		params[m[1]] = true
	}
	return out, params
}

// -------------------------------------------------------------- suppressions

func (a *analyzer) collectMarkers(f *ast.File) {
	file := a.fset.Position(f.Pos()).Filename
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			i := strings.Index(c.Text, marker)
			if i < 0 {
				continue
			}
			reason := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(c.Text[i+len(marker):]), ":—-"))
			if reason == "" {
				reason = "(NO REASON GIVEN)"
			}
			if a.marks[file] == nil {
				a.marks[file] = map[int]string{}
				a.markUsed[file] = map[int]bool{}
			}
			// Applies to the comment's own line and to the declaration that
			// immediately follows the comment group it belongs to.
			from := a.fset.Position(c.Pos()).Line
			to := a.fset.Position(cg.End()).Line + 1
			for l := from; l <= to; l++ {
				a.marks[file][l] = reason
			}
		}
	}
}

func (a *analyzer) applySuppressions(in []finding) (kept, sup []finding) {
	for _, f := range in {
		file, line, ok := splitPos(f.pos)
		if ok {
			if reason, hit := a.marks[file][line]; hit {
				a.markUsed[file][line] = true
				f.why = reason
				sup = append(sup, f)
				continue
			}
		}
		kept = append(kept, f)
	}
	return kept, sup
}

func (a *analyzer) unusedMarkers() []string {
	seen := map[string]bool{}
	var out []string
	for file, lines := range a.marks {
		for line, reason := range lines {
			if a.markUsed[file][line] {
				continue
			}
			// A marker covers a small line range; report the block once.
			rel, err := filepath.Rel(mustWD(), file)
			if err != nil {
				rel = file
			}
			k := fmt.Sprintf("%s [%s]", rel, reason)
			if seen[k] {
				continue
			}
			// Only complain if NO line in the block was used.
			used := false
			for l := line - 3; l <= line+3; l++ {
				if a.markUsed[file][l] {
					used = true
				}
			}
			if used {
				continue
			}
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// ------------------------------------------------------------------ baseline

func loadBaseline() (map[string]bool, error) {
	raw, err := os.ReadFile(baselinePath)
	if os.IsNotExist(err) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, reason, ok := strings.Cut(line, "\t")
		if !ok || strings.TrimSpace(reason) == "" {
			return nil, fmt.Errorf("%s:%d: want '<key>\\t<reason>', got %q", baselinePath, i+1, line)
		}
		out[strings.TrimSpace(key)] = false
	}
	return out, nil
}

// emitBaseline prints a baseline whose reason column is the ANALYZER'S OWN
// evidence for the finding, not a generic label. "written by 3 test site(s) and
// nothing else" is a reason a reviewer can act on; "known debt" is not.
func emitBaseline(fs []finding) {
	for _, f := range fs {
		reason := f.why
		if f.note != "" {
			reason += " — " + f.note
		}
		if f.pos != "" {
			reason += " (" + f.pos + ")"
		}
		fmt.Printf("%s\t%s\n", f.key, reason)
	}
}

// ------------------------------------------------------------------- helpers

func deref(t types.Type) types.Type {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		return types.Unalias(p.Elem())
	}
	return types.Unalias(t)
}

func namedOf(t types.Type) *types.Named {
	if t == nil {
		return nil
	}
	n, _ := deref(t).(*types.Named)
	return n
}

func structOf(t types.Type) *types.Struct {
	if t == nil {
		return nil
	}
	s, _ := deref(t).Underlying().(*types.Struct)
	return s
}

// structKeysIn lists the named struct types reachable one level down from a
// field's type, through pointers, slices, arrays and maps.
func structKeysIn(t types.Type) []string {
	var out []string
	var walk func(types.Type, int)
	walk = func(t types.Type, depth int) {
		if t == nil || depth > 4 {
			return
		}
		switch u := types.Unalias(t).(type) {
		case *types.Pointer:
			walk(u.Elem(), depth+1)
		case *types.Slice:
			walk(u.Elem(), depth+1)
		case *types.Array:
			walk(u.Elem(), depth+1)
		case *types.Map:
			walk(u.Elem(), depth+1)
		case *types.Named:
			if u.Obj().Pkg() == nil {
				return
			}
			if _, ok := u.Underlying().(*types.Struct); ok {
				out = append(out, u.Obj().Pkg().Path()+"."+u.Obj().Name())
			}
		}
	}
	walk(t, 0)
	return out
}

func calleeFunc(fun ast.Expr, info *types.Info) *types.Func {
	id := calleeIdent(fun)
	if id == nil {
		return nil
	}
	fn, _ := info.Uses[id].(*types.Func)
	return fn
}

func calleeIdent(fun ast.Expr) *ast.Ident {
	switch f := fun.(type) {
	case *ast.Ident:
		return f
	case *ast.SelectorExpr:
		return f.Sel
	case *ast.IndexExpr:
		return calleeIdent(f.X)
	case *ast.IndexListExpr:
		return calleeIdent(f.X)
	case *ast.ParenExpr:
		return calleeIdent(f.X)
	}
	return nil
}

func funcKey(fn *types.Func) string {
	if fn.Pkg() == nil {
		return fn.Name()
	}
	if recv := fn.Signature().Recv(); recv != nil {
		if named := namedOf(recv.Type()); named != nil {
			return fn.Pkg().Path() + "." + named.Obj().Name() + "." + fn.Name()
		}
	}
	return fn.Pkg().Path() + "." + fn.Name()
}

func jsonName(tag string) string {
	v := structTag(tag, "json")
	name, _, _ := strings.Cut(v, ",")
	if name == "-" {
		return ""
	}
	return name
}

func serialTag(tag string) bool {
	for _, k := range []string{"json", "yaml", "toml", "koanf"} {
		if structTag(tag, k) != "" {
			return true
		}
	}
	return false
}

func structTag(tag, key string) string {
	// reflect.StructTag.Get, without importing reflect for one call.
	for tag != "" {
		i := 0
		for i < len(tag) && tag[i] == ' ' {
			i++
		}
		tag = tag[i:]
		i = 0
		for i < len(tag) && tag[i] > ' ' && tag[i] != ':' && tag[i] != '"' {
			i++
		}
		if i == 0 || i+1 >= len(tag) || tag[i] != ':' || tag[i+1] != '"' {
			return ""
		}
		name := tag[:i]
		tag = tag[i+1:]
		i = 1
		for i < len(tag) && tag[i] != '"' {
			if tag[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(tag) {
			return ""
		}
		qval := tag[:i+1]
		tag = tag[i+1:]
		if name == key {
			v, err := strconv.Unquote(qval)
			if err != nil {
				return ""
			}
			return v
		}
	}
	return ""
}

func isTestFile(name string) bool {
	return strings.HasSuffix(name, "_test.go") || strings.Contains(name, "/test/")
}

func isGenerated(f *ast.File) bool {
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "// Code generated") && strings.Contains(c.Text, "DO NOT EDIT") {
				return true
			}
		}
	}
	return false
}

func inDeclRoot(path string) bool {
	for _, r := range declRoots {
		if strings.HasPrefix(path+"/", r) {
			return true
		}
	}
	return false
}

func pos(p token.Position) string {
	rel, err := filepath.Rel(mustWD(), p.Filename)
	if err != nil {
		rel = p.Filename
	}
	return fmt.Sprintf("%s:%d", rel, p.Line)
}

func splitPos(s string) (file string, line int, ok bool) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return "", 0, false
	}
	abs, err := filepath.Abs(s[:i])
	if err != nil {
		return "", 0, false
	}
	return abs, n, true
}

var wd string

func mustWD() string {
	if wd == "" {
		wd, _ = os.Getwd()
	}
	return wd
}
