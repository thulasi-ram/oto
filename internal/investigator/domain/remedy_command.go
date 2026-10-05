package domain

// WHAT A REMEDY'S RISK RULES READ: THE COMMAND (ADR 0054 §3; git-bug eb4f21b).
//
// A Remedy names a write Tool and the exact JSON arguments it would be sent. The operator's
// risk rules match on four things about that — the Tool, the verb, the resource kind and the
// namespace — plus whether oto knows the verb to be reversible. ParseRemedyCommand reads
// those four out of the arguments, and nothing else reads them.
//
// ⭐⭐ TWO SHAPES, AND ONLY TWO.
//
//   - A COMMAND LINE: the arguments carry `command` (a string, or an array of strings) or
//     `args` (an array of strings). It is read as kubectl's command line — `kubectl` first,
//     or kubectl's own verb first — with a STRICT TOKENIZER: words are split on spaces and
//     tabs and on nothing else, and NOTHING IS INTERPRETED. No quoting, no escaping, no
//     expansion, no globbing; a character a shell would treat specially makes the whole
//     command unparseable instead.
//   - STRUCTURED ARGUMENTS: no command line at all. The verb is the `verb` member, the kind
//     is `kind` (or `resource`), the namespace is `namespace` — each a string when present.
//
// ⛔⛔ ANYTHING THE RULES CANNOT READ IS UNPARSEABLE, AND UNPARSEABLE IS TWO APPROVALS
// WHATEVER THE RULES SAY (§3). `sh -c …`, a pipe, `;`, `&&`, backticks, `$(…)`, a
// redirect, a quote, a glob, `--` and what follows it, a flag oto does not know the arity
// of, a file (`-f`) the rules cannot see into, a flag that changes who kubectl runs as or
// where it connects, two kinds in one command, a program that is not kubectl — each is a
// reason, said in the Remedy's record. Reading less is the fail-safe direction: a command
// the parser misreads could match a rule that lowers it, so the parser refuses rather than
// guesses.
//
// ⚠️ IT DOES NOT EXECUTE OR VALIDATE ANYTHING. Whether kubectl would accept the command is
// the ToolServer's business; this decides only what the rules may say about it.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// AllNamespaces is the namespace of a command that names every namespace (`-A`). It is
// never in a rule's namespace list, so a rule restricted to namespaces never matches it.
const AllNamespaces = "*"

// RemedyCommand is what the risk rules read about one Remedy's command. Build it with
// ParseRemedyCommand.
type RemedyCommand struct {
	// Tool is the write Tool, qualified `<toolserver>__<tool>`.
	Tool string
	// Verb is kubectl's verb (`delete`, `rollout restart`), or the `verb` member; "" when
	// the command names none.
	Verb string
	// Kind is the resource kind, folded to kubectl's singular name for the built-in kinds
	// (`deploy`, `deployments` → `deployment`); "" when the command names none.
	Kind string
	// Namespace is the namespace the command names; "" when it names none, AllNamespaces
	// for `-A`.
	Namespace string
	// Unparseable says why the rules cannot read this command; "" when they can.
	Unparseable string
}

// Parsed reports whether the rules can read the command.
func (c RemedyCommand) Parsed() bool { return c.Unparseable == "" }

// Reversible reports whether oto KNOWS the command's verb to be reversible. Everything else —
// delete, drain, patch, apply, create, set, a verb oto does not know, or no verb — is not.
func (c RemedyCommand) Reversible() bool { return c.Parsed() && reversibleVerbs[c.Verb] }

// reversibleVerbs are the verbs whose change another command undoes with nothing lost: a
// restart, a pause or resume, an undo, a scale, a cordon. ⚠️ A CLOSED LIST, AND SHORT ON
// PURPOSE: a verb missing from it is treated as irreversible, which is the safe mistake.
var reversibleVerbs = map[string]bool{
	"rollout restart": true, "rollout pause": true, "rollout resume": true, "rollout undo": true,
	"scale": true, "cordon": true, "uncordon": true,
}

// ReversibleVerbs is that list, sorted, for the docs and the settings screen.
func ReversibleVerbs() []string {
	out := make([]string, 0, len(reversibleVerbs))
	for v := range reversibleVerbs {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}

// commandMeta are the characters a shell would treat specially in a command line. One of
// them anywhere makes the line unparseable: oto does not interpret it, and a ToolServer that
// hands the line to a shell would.
const commandMeta = ";|&`$<>(){}[]*?~#\\'\"\n\r"

// valueMeta are the characters that make a STRUCTURED string value unparseable: the ones
// that chain, substitute or redirect if a ToolServer ever builds a shell line from it.
const valueMeta = ";|&`$<>\n\r"

// ParseRemedyCommand reads one Remedy's command for the risk rules: the qualified Tool and
// the compact JSON object of its arguments.
func ParseRemedyCommand(tool RemedyTool, arguments string) RemedyCommand {
	c := RemedyCommand{Tool: tool.Qualified()}
	unparseable := func(format string, a ...any) RemedyCommand {
		c.Unparseable = fmt.Sprintf(format, a...)
		return c
	}
	if !tool.Named() {
		return unparseable("it names no Tool")
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal([]byte(arguments), &args); err != nil || args == nil {
		return unparseable("its arguments are not one JSON object")
	}
	// ⭐ Every string anywhere in the arguments — keys included — is checked first, so a
	// chained command cannot hide in a member the rules do not read.
	if why := metaInJSON(json.RawMessage(arguments)); why != "" {
		return unparseable("%s", why)
	}
	cmd, hasCmd := args["command"]
	argv, hasArgs := args["args"]
	switch {
	case hasCmd && hasArgs:
		return unparseable("its arguments carry both command and args, and the rules cannot tell which is run")
	case hasCmd:
		return parseCommandValue(c, "command", cmd)
	case hasArgs:
		var words []string
		if json.Unmarshal(argv, &words) != nil {
			return unparseable("args is not an array of strings")
		}
		return parseCommandWords(c, "args", words)
	default:
		return parseStructured(c, args)
	}
}

// parseCommandValue reads `command`: a string split on spaces and tabs, or an array of words.
func parseCommandValue(c RemedyCommand, member string, raw json.RawMessage) RemedyCommand {
	var line string
	if json.Unmarshal(raw, &line) == nil {
		if strings.ContainsAny(line, commandMeta) {
			c.Unparseable = fmt.Sprintf("its %s contains %s, which a shell would interpret", member, quoteMeta(line))
			return c
		}
		return parseCommandWords(c, member, strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' }))
	}
	var words []string
	if json.Unmarshal(raw, &words) != nil {
		c.Unparseable = member + " is neither a string nor an array of strings"
		return c
	}
	return parseCommandWords(c, member, words)
}

// kubectlVerbs are kubectl's verbs: a command line that does not begin with `kubectl` is
// read as kubectl's arguments only when it begins with one of these.
var kubectlVerbs = map[string]bool{
	"annotate": true, "apply": true, "attach": true, "auth": true, "autoscale": true, "certificate": true,
	"cordon": true, "cp": true, "create": true, "debug": true, "delete": true, "describe": true, "drain": true,
	"edit": true, "exec": true, "expose": true, "get": true, "label": true, "logs": true, "patch": true,
	"port-forward": true, "proxy": true, "replace": true, "rollout": true, "run": true, "scale": true,
	"set": true, "taint": true, "top": true, "uncordon": true, "wait": true,
}

// twoWordVerbs take a subcommand: `rollout restart`, `set image`, `auth reconcile`.
var twoWordVerbs = map[string]bool{"rollout": true, "set": true, "auth": true, "certificate": true, "top": true}

// implicitKinds are verbs whose resource is named by the verb itself.
var implicitKinds = map[string]string{
	"cordon": "node", "uncordon": "node", "drain": "node",
	"certificate approve": "certificatesigningrequest", "certificate deny": "certificatesigningrequest",
}

// The flags kubectl takes, by what the parser does with them.
var (
	// forbiddenFlags change who kubectl runs as, where it connects, or name resources in a
	// file the rules cannot see into.
	forbiddenFlags = map[string]string{
		"kubeconfig": "changes the credentials kubectl runs with", "token": "changes the credentials kubectl runs with",
		"as": "impersonates another identity", "as-group": "impersonates another identity",
		"as-uid": "impersonates another identity", "user": "changes the credentials kubectl runs with",
		"username": "changes the credentials kubectl runs with", "password": "changes the credentials kubectl runs with",
		"client-certificate": "changes the credentials kubectl runs with", "client-key": "changes the credentials kubectl runs with",
		"server": "changes the API server kubectl connects to", "s": "changes the API server kubectl connects to",
		"certificate-authority":    "changes how kubectl trusts the API server",
		"insecure-skip-tls-verify": "changes how kubectl trusts the API server",
		"filename":                 "names its resources in a file the rules cannot read", "f": "names its resources in a file the rules cannot read",
		"kustomize": "names its resources in a file the rules cannot read", "k": "names its resources in a file the rules cannot read",
		"raw": "sends a raw request the rules cannot read",
	}
	// valueFlags take a value: `--flag value`, `--flag=value`, `-n value`, `-nvalue`.
	valueFlags = map[string]bool{
		"namespace": true, "n": true, "context": true, "cluster": true, "selector": true, "l": true,
		"field-selector": true, "output": true, "o": true, "replicas": true, "current-replicas": true,
		"timeout": true, "grace-period": true, "cascade": true, "to-revision": true, "container": true, "c": true,
		"type": true, "patch": true, "p": true, "revision": true, "field-manager": true, "resource-version": true,
		"pod-selector": true, "chunk-size": true, "subresource": true, "min": true, "max": true, "cpu-percent": true,
		"image": true, "port": true, "reason": true,
	}
	// boolFlags take none.
	boolFlags = map[string]bool{
		"all": true, "all-namespaces": true, "A": true, "force": true, "now": true, "wait": true, "overwrite": true,
		"record": true, "local": true, "ignore-not-found": true, "ignore-daemonsets": true,
		"delete-emptydir-data": true, "disable-eviction": true, "dry-run": true, "no-headers": true,
		"show-labels": true, "watch": true, "w": true,
	}
)

// parseCommandWords reads a command line, already split into words.
func parseCommandWords(c RemedyCommand, member string, words []string) RemedyCommand {
	unparseable := func(format string, a ...any) RemedyCommand {
		c.Unparseable = fmt.Sprintf(format, a...)
		return c
	}
	for _, w := range words {
		if w == "" || strings.ContainsAny(w, commandMeta+" \t") {
			return unparseable("its %s has a word %s, which a shell would interpret or split", member, quoteMeta(w))
		}
	}
	if len(words) == 0 {
		return unparseable("its %s is empty", member)
	}
	switch first := words[0]; {
	case first == "kubectl":
		words = words[1:]
	case kubectlVerbs[first] || strings.HasPrefix(first, "-"):
		// kubectl's own arguments, without the program.
	default:
		return unparseable("it runs %s, and the rules read only kubectl's command lines", quoteShort(first))
	}

	var positional []string
	setNS := func(ns string) string {
		switch {
		case ns == "":
			return "it names an empty namespace"
		case c.Namespace != "" && c.Namespace != ns:
			return fmt.Sprintf("it names two namespaces, %s and %s", quoteShort(c.Namespace), quoteShort(ns))
		}
		c.Namespace = ns
		return ""
	}
	for i := 0; i < len(words); i++ {
		w := words[i]
		if w == "--" {
			return unparseable("everything after -- is another program's command line")
		}
		if w == "-" || !strings.HasPrefix(w, "-") {
			if w == "-" {
				return unparseable("it reads from standard input")
			}
			positional = append(positional, w)
			continue
		}
		var name, value string
		var hasValue bool
		if strings.HasPrefix(w, "--") {
			name, value, hasValue = strings.Cut(w[2:], "=")
		} else {
			// A short flag: `-n`, `-n=payments` or `-npayments`.
			name = w[1:2]
			if rest := w[2:]; rest != "" {
				if !valueFlags[name] {
					return unparseable("it has the flag %s, which oto cannot read", quoteShort(w))
				}
				value, hasValue = strings.TrimPrefix(rest, "="), true
			}
		}
		if why, ok := forbiddenFlags[name]; ok {
			return unparseable("its flag %s %s", quoteShort("-"+dashes(name)+name), why)
		}
		switch {
		case name == "A" || name == "all-namespaces":
			if hasValue && value != "true" {
				return unparseable("it has the flag %s, which oto cannot read", quoteShort(w))
			}
			if why := setNS(AllNamespaces); why != "" {
				return unparseable("%s", why)
			}
		case valueFlags[name]:
			if !hasValue {
				if i+1 >= len(words) {
					return unparseable("its flag %s has no value", quoteShort(w))
				}
				i++
				value = words[i]
			}
			if name == "namespace" || name == "n" {
				if why := setNS(value); why != "" {
					return unparseable("%s", why)
				}
			}
		case boolFlags[name]:
			// takes no value; `--flag=value` is kubectl's own business.
		case hasValue:
			// an unknown flag in `--flag=value` form: its arity is plain.
		default:
			return unparseable("it has the flag %s, and oto cannot tell whether it takes a value", quoteShort(w))
		}
	}

	if len(positional) == 0 {
		return unparseable("it names no verb")
	}
	verb, rest := positional[0], positional[1:]
	if !kubectlVerbs[verb] {
		return unparseable("%s is not a kubectl verb", quoteShort(verb))
	}
	if twoWordVerbs[verb] {
		if len(rest) == 0 {
			return unparseable("kubectl %s needs a subcommand, and it names none", verb)
		}
		verb, rest = verb+" "+rest[0], rest[1:]
	}
	c.Verb = verb
	if k, ok := implicitKinds[verb]; ok {
		c.Kind = k
		return c
	}
	if len(rest) == 0 {
		return c
	}
	if strings.Contains(rest[0], "/") {
		// `type/name` arguments: every one that is not an assignment names its kind.
		for _, a := range rest {
			if strings.Contains(a, "=") || !strings.Contains(a, "/") {
				continue
			}
			kind, _, _ := strings.Cut(a, "/")
			if why := c.addKind(kind); why != "" {
				return unparseable("%s", why)
			}
		}
		return c
	}
	if why := c.addKind(rest[0]); why != "" {
		return unparseable("%s", why)
	}
	return c
}

// addKind folds one kind into the command, refusing a second, different one.
func (c *RemedyCommand) addKind(raw string) string {
	if strings.Contains(raw, ",") {
		return fmt.Sprintf("it names more than one kind (%s)", quoteShort(raw))
	}
	k := CanonicalKind(raw)
	if k == "" {
		return fmt.Sprintf("%s is not a resource kind", quoteShort(raw))
	}
	if c.Kind != "" && c.Kind != k {
		return fmt.Sprintf("it names more than one kind (%s and %s)", c.Kind, k)
	}
	c.Kind = k
	return ""
}

// parseStructured reads arguments that carry no command line.
func parseStructured(c RemedyCommand, args map[string]json.RawMessage) RemedyCommand {
	str := func(member string) (string, bool, string) {
		raw, ok := args[member]
		if !ok || string(raw) == "null" {
			return "", false, ""
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", true, fmt.Sprintf("its %s is not a string", member)
		}
		return strings.TrimSpace(s), true, ""
	}
	verb, has, why := str("verb")
	if why != "" {
		c.Unparseable = why
		return c
	}
	if has {
		v := NormalizeRiskVerb(verb)
		if !riskVerbPattern.MatchString(v) {
			c.Unparseable = fmt.Sprintf("its verb %s is not a verb the rules can read", quoteShort(verb))
			return c
		}
		c.Verb = v
	}
	for _, member := range []string{"kind", "resource"} {
		kind, has, why := str(member)
		if why != "" {
			c.Unparseable = why
			return c
		}
		if has {
			if why := c.addKind(kind); why != "" {
				c.Unparseable = why
				return c
			}
		}
	}
	ns, has, why := str("namespace")
	if why != "" {
		c.Unparseable = why
		return c
	}
	if has {
		if !namespacePattern.MatchString(ns) {
			c.Unparseable = fmt.Sprintf("its namespace %s is not a namespace name", quoteShort(ns))
			return c
		}
		c.Namespace = ns
	}
	return c
}

// metaInJSON is why a JSON document's strings are unparseable — "" when none is.
func metaInJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "its arguments are not JSON"
	}
	var walk func(any) string
	walk = func(v any) string {
		switch t := v.(type) {
		case string:
			if strings.ContainsAny(t, valueMeta) {
				return "its arguments contain " + quoteMeta(t) + ", which chains, substitutes or redirects in a shell"
			}
		case []any:
			for _, e := range t {
				if why := walk(e); why != "" {
					return why
				}
			}
		case map[string]any:
			for k, e := range t {
				if why := walk(k); why != "" {
					return why
				}
				if why := walk(e); why != "" {
					return why
				}
			}
		}
		return ""
	}
	return walk(v)
}

// quoteMeta names the shell characters a string contains, for the record.
func quoteMeta(s string) string {
	var found []string
	for _, r := range commandMeta {
		if strings.ContainsRune(s, r) {
			switch r {
			case '\n':
				found = append(found, "a newline")
			case '\r':
				found = append(found, "a carriage return")
			case '`':
				found = append(found, "a backtick")
			default:
				found = append(found, "`"+string(r)+"`")
			}
		}
	}
	if len(found) == 0 {
		return quoteShort(s)
	}
	return strings.Join(found, " ")
}

func dashes(name string) string {
	if len(name) == 1 {
		return ""
	}
	return "-"
}

// kindAliases folds kubectl's plurals and short names for the built-in kinds onto one
// spelling. A kind not here is matched exactly as written, lower-cased.
var kindAliases = func() map[string]string {
	groups := map[string][]string{
		"pod":                       {"pods", "po"},
		"deployment":                {"deployments", "deploy"},
		"statefulset":               {"statefulsets", "sts"},
		"daemonset":                 {"daemonsets", "ds"},
		"replicaset":                {"replicasets", "rs"},
		"replicationcontroller":     {"replicationcontrollers", "rc"},
		"service":                   {"services", "svc"},
		"secret":                    {"secrets"},
		"configmap":                 {"configmaps", "cm"},
		"namespace":                 {"namespaces", "ns"},
		"node":                      {"nodes", "no"},
		"job":                       {"jobs"},
		"cronjob":                   {"cronjobs", "cj"},
		"ingress":                   {"ingresses", "ing"},
		"persistentvolumeclaim":     {"persistentvolumeclaims", "pvc"},
		"persistentvolume":          {"persistentvolumes", "pv"},
		"serviceaccount":            {"serviceaccounts", "sa"},
		"horizontalpodautoscaler":   {"horizontalpodautoscalers", "hpa"},
		"poddisruptionbudget":       {"poddisruptionbudgets", "pdb"},
		"networkpolicy":             {"networkpolicies", "netpol"},
		"role":                      {"roles"},
		"rolebinding":               {"rolebindings"},
		"clusterrole":               {"clusterroles"},
		"clusterrolebinding":        {"clusterrolebindings"},
		"customresourcedefinition":  {"customresourcedefinitions", "crd", "crds"},
		"endpoints":                 {"ep"},
		"event":                     {"events", "ev"},
		"storageclass":              {"storageclasses", "sc"},
		"limitrange":                {"limitranges", "limits"},
		"resourcequota":             {"resourcequotas", "quota"},
		"certificatesigningrequest": {"certificatesigningrequests", "csr"},
	}
	out := map[string]string{}
	for canonical, aliases := range groups {
		out[canonical] = canonical
		for _, a := range aliases {
			out[a] = canonical
		}
	}
	return out
}()

var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// CanonicalKind is the one spelling a rule and a command compare: lower-cased, without an
// API group (`deployments.apps` → `deployments`), and folded onto kubectl's singular name
// for a built-in kind. "" when it is not a kind at all.
func CanonicalKind(raw string) string {
	k := strings.ToLower(strings.TrimSpace(raw))
	k, _, _ = strings.Cut(k, ".")
	if canonical, ok := kindAliases[k]; ok {
		return canonical
	}
	if !kindPattern.MatchString(k) {
		return ""
	}
	return k
}

// riskVerbPattern is a verb a rule names and a structured `verb` may say: one or two
// lower-case words.
var riskVerbPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,30}( [a-z][a-z0-9_-]{0,30})?$`)

// NormalizeRiskVerb lower-cases a verb and joins its words with one space.
func NormalizeRiskVerb(raw string) string {
	return strings.Join(strings.Fields(strings.ToLower(raw)), " ")
}

// namespacePattern is a Kubernetes namespace name.
var namespacePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
