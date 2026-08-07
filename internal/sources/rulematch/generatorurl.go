package rulematch

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// MaxGeneratorURLBytes mirrors ingest bound B14 (SPEC §L.3.2).
//
// B14 says to TRUNCATE an over-long generatorURL and keep the alert, and the
// ingest normaliser does exactly that. This package refuses to parse the
// truncated result: a PromQL expression cut off mid-token is not a rule
// definition, and recording it as one would put a lie in a RuleSnapshot.
const MaxGeneratorURLBytes = 8192

// MaxExprBytes mirrors rule_snapshots_exprlen_ck.
const MaxExprBytes = 65536

// Variant names the shape of a generatorURL's path. Prometheus builds the URL
// as `externalURL + strutil.TableLinkForExpression(expr)`, which is
// `/graph?g0.expr=…&g0.tab=1`; but externalURL may carry a routing prefix, a
// console template produces a /consoles/… link instead, and third-party alert
// senders put whatever they like there.
type Variant string

// The recognised generatorURL shapes.
const (
	// VariantGraph is the standard `<external>/graph?g0.expr=…` form.
	VariantGraph Variant = "graph"
	// VariantConsole is a `<external>/consoles/…` console template link.
	VariantConsole Variant = "consoles"
	// VariantFragment is a form that carries the expression in the fragment
	// rather than the query.
	VariantFragment Variant = "fragment"
	// VariantUnknown is anything else that still yielded an expression.
	VariantUnknown Variant = "unknown"
)

// Error codes this package produces. They are stable: the caller records them.
const (
	// CodeNoGeneratorURL means the alert carried no generatorURL at all.
	CodeNoGeneratorURL = "rulematch_generator_url_missing"
	// CodeGeneratorURLTooLarge means B14 truncation may have corrupted it.
	CodeGeneratorURLTooLarge = "rulematch_generator_url_too_large"
	// CodeGeneratorURLUnparseable means it is not a URL.
	CodeGeneratorURLUnparseable = "rulematch_generator_url_unparseable"
	// CodeNoExpr means it is a URL but carries no gN.expr parameter — the case
	// for a hand-POSTed alert or a non-Prometheus sender.
	CodeNoExpr = "rulematch_generator_url_no_expr"
)

// exprParamRe matches the gN.expr query parameter Prometheus emits. N is the
// panel index; a rule link is always g0, but the pattern accepts any index so a
// hand-edited link still parses.
var exprParamRe = regexp.MustCompile(`^g(\d+)\.expr$`)

// percentRe detects a string that still looks entirely percent-encoded, which is
// the signature of a proxy that encoded the URL a second time.
var percentRe = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)

// GeneratorURL is a parsed Prometheus generatorURL.
type GeneratorURL struct {
	// Raw is the URL as received.
	Raw string
	// ExternalURL is the origin Prometheus root, with the /graph or /consoles
	// suffix removed and no trailing slash. In a federated or sharded setup this
	// is THE way to know which of several Prometheis evaluated the rule
	// (research A7 pitfall 5); querying the wrong one returns nothing.
	ExternalURL string
	// Expr is the decoded PromQL, exactly as evaluated.
	Expr string
	// Index is the N of the gN.expr that supplied Expr.
	Index int
	// Variant is the recognised path shape.
	Variant Variant
	// DoubleDecoded reports that a second percent-decode was applied. It is a
	// heuristic and a reason to trust the expression slightly less.
	DoubleDecoded bool
}

// ParseGeneratorURL decodes a Prometheus generatorURL.
//
// This is the PRIMARY rule-recovery strategy (SPEC §F.4, research A7): the
// expression arrives with the alert, needs no API call, cannot be ambiguous, and
// works in a multi-Prometheus setup where /api/v1/rules on the wrong server
// simply returns nothing. What it CANNOT give you is `for`, `keep_firing_for` or
// the rule's raw labels and annotations — that is what the rules API is for.
func ParseGeneratorURL(raw string) (GeneratorURL, error) {
	s := strings.TrimSpace(raw)
	switch {
	case s == "":
		return GeneratorURL{}, errs.New(errs.KindValidation, CodeNoGeneratorURL,
			"the alert carried no generatorURL")
	case len(s) > MaxGeneratorURLBytes:
		return GeneratorURL{}, errs.Newf(errs.KindValidation, CodeGeneratorURLTooLarge,
			"a generatorURL over %d bytes may have been truncated and cannot be trusted", MaxGeneratorURLBytes)
	}

	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return GeneratorURL{}, errs.Wrap(orErr(err), errs.KindValidation, CodeGeneratorURLUnparseable,
			"the generatorURL is not an absolute URL")
	}

	out := GeneratorURL{Raw: s, Variant: VariantUnknown, Index: -1}

	expr, idx, ok := pickExpr(u.Query())
	if !ok {
		// Some builds (and every hand-written link) put the panes in the
		// fragment. url.Parse leaves it undecoded, so it is parsed as a query.
		if frag, ferr := url.ParseQuery(u.Fragment); ferr == nil {
			if expr, idx, ok = pickExpr(frag); ok {
				out.Variant = VariantFragment
			}
		}
	}
	if !ok {
		return GeneratorURL{}, errs.New(errs.KindValidation, CodeNoExpr,
			"the generatorURL carries no gN.expr parameter")
	}

	expr, out.DoubleDecoded = maybeDecodeAgain(expr)
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return GeneratorURL{}, errs.New(errs.KindValidation, CodeNoExpr,
			"the generatorURL's gN.expr parameter is empty")
	}
	if len(expr) > MaxExprBytes {
		expr = expr[:MaxExprBytes]
	}

	out.Expr = expr
	out.Index = idx
	if out.Variant != VariantFragment {
		out.Variant = variantOf(u.Path)
	}
	out.ExternalURL = externalURL(u)
	return out, nil
}

// pickExpr returns the lowest-indexed gN.expr in v.
func pickExpr(v url.Values) (expr string, index int, ok bool) {
	best := -1
	for key, vals := range v {
		m := exprParamRe.FindStringSubmatch(key)
		if m == nil || len(vals) == 0 {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if best == -1 || n < best {
			best, expr = n, vals[0]
		}
	}
	if best == -1 {
		return "", -1, false
	}
	return expr, best, true
}

// maybeDecodeAgain unwraps a second layer of percent-encoding.
//
// Prometheus encodes the expression exactly once, and url.Values has already
// undone that. A reverse proxy that re-encodes the query leaves a result that is
// still entirely percent-escaped — no spaces, no braces, no parentheses, but
// plenty of `%xx`. That, and only that, earns a second decode; anything looser
// would mangle a legitimate expression containing a literal `%`.
func maybeDecodeAgain(expr string) (string, bool) {
	if !percentRe.MatchString(expr) {
		return expr, false
	}
	if strings.ContainsAny(expr, " {}()\"'") {
		return expr, false
	}
	decoded, err := url.QueryUnescape(expr)
	if err != nil || decoded == expr {
		return expr, false
	}
	return decoded, true
}

// variantOf classifies the path.
func variantOf(path string) Variant {
	p := strings.TrimRight(path, "/")
	switch {
	case p == "/graph" || strings.HasSuffix(p, "/graph"):
		return VariantGraph
	case p == "/consoles" || strings.Contains(p, "/consoles/"):
		return VariantConsole
	default:
		return VariantUnknown
	}
}

// externalURL strips the /graph or /consoles suffix to recover the Prometheus
// root, preserving any routing prefix in between (`--web.external-url` is
// routinely set to a subpath behind an ingress).
func externalURL(u *url.URL) string {
	p := strings.TrimRight(u.EscapedPath(), "/")
	switch {
	case p == "/graph" || strings.HasSuffix(p, "/graph"):
		p = strings.TrimSuffix(p, "/graph")
	case strings.Contains(p, "/consoles/"), strings.HasSuffix(p, "/consoles"):
		if i := strings.Index(p, "/consoles"); i >= 0 {
			p = p[:i]
		}
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(p, "/")
}

// orErr keeps errs.Wrap from swallowing a nil cause.
func orErr(err error) error {
	if err != nil {
		return err
	}
	return errs.New(errs.KindValidation, CodeGeneratorURLUnparseable, "no scheme or host")
}
