package domain

// WHAT A REMEDY'S RISK RULES READ: THE COMMAND (ADR 0054 §3; git-bug eb4f21b).
//
// A Remedy names a write Tool and the exact JSON arguments it would be sent. The operator's
// risk rules match on four things about that — the Tool, the verb, the resource kind and the
// namespace — plus whether oto knows the verb to be reversible. ParseRemedyCommand reads
// those four out of the arguments, and nothing else reads them.
//
// ⭐⭐ TWO SHAPES, AND ONLY TWO — EACH A CLOSED SET OF MEMBERS (judgment 2, C1+C3).
//
//   - A COMMAND LINE: the arguments are ONE member, `command` (a string, or an array of
//     strings) or `args` (an array of strings), and nothing beside it — a sibling such as
//     `context` is a member the ToolServer may read and the rules cannot, so it is
//     unparseable. It is read as kubectl's command line — `kubectl` first, or kubectl's own
//     verb first — with a STRICT TOKENIZER: a string is split on spaces and tabs and on
//     nothing else, and NOTHING IS INTERPRETED. No quoting, no escaping, no expansion, no
//     globbing.
//   - STRUCTURED ARGUMENTS: no command line at all, and only these members, each a JSON
//     string: `verb`, `kind` | `resource` | `resourceType`, `namespace`, `name`. Any other
//     member, a nested object or array, a number, a boolean or a null is unparseable — a
//     `manifest` or a `patch` says what the write does, and the rules cannot read it.
//
// ⭐⭐ EVERY WORD IS CHECKED AGAINST AN ALLOWLIST, NOT A DENYLIST (judgment 2, C2). A word —
// of a `command` string, a `command` array or `args` — is ASCII letters, digits and
// `_ . : / = @ , + -` and nothing else (wordPattern). A no-break space, an em space, an
// ideographic space, a vertical tab, a form feed, NEL or a separator control is not split
// on and not allowed: a ToolServer that splits with Python's `str.split()` or Go's
// `strings.Fields` would run a different argv from the one the rules read, so the command is
// refused instead. The shell characters (commandMeta) survive only to say WHY in the record.
//
// ⭐⭐ THE DOCUMENT IS READ TOKEN BY TOKEN BEFORE ANYTHING IS DECODED (argumentKeys). A key
// twice at any depth, two keys that differ only in case, or a key that is not ASCII (the
// Kelvin sign that folds to `k`) is unparseable: a decoder that keeps the last of two
// `namespace`s, or one that folds `COMMAND` onto `command`, reads a different document from
// the one the rules read.
//
// ⛔⛔ A MODEL-WRITTEN `verb` IS NEVER REVERSIBLE. In structured arguments the verb is the
// model's claim about what a Tool does, not what the Tool does: `{"verb":"rollout restart"}`
// sent to a delete Tool would otherwise match a rule about reversible commands. A structured
// Remedy is therefore never Reversible, and a rule that asks for reversibility never lowers
// one; a rule that lowers one names its Tool (NewRiskRules: a single-approval rule names the
// write Tool it is about, so the OPERATOR says which Tools are kubectl-shaped).
//
// ⛔⛔ ANYTHING THE RULES CANNOT READ IS UNPARSEABLE, AND UNPARSEABLE IS TWO APPROVALS
// WHATEVER THE RULES SAY (§3). `sh -c …`, a pipe, `;`, `&&`, backticks, `$(…)`, a
// redirect, a quote, a glob, `--` and what follows it, a flag oto does not know — in ANY
// form, `--x=y` included (C4) —, a file (`-f`) the rules cannot see into, a flag that
// changes who kubectl runs as or where it connects (`--context`, `--cluster` among them),
// two kinds in one command, a kind oto does not know every spelling of (C5), a program that
// is not kubectl — each is a reason, said in the Remedy's record. Reading less is the
// fail-safe direction: a command the parser misreads could match a rule that lowers it, so
// the parser refuses rather than guesses.
//
// ⚠️ IT DOES NOT EXECUTE OR VALIDATE ANYTHING. Whether kubectl would accept the command is
// the ToolServer's business; this decides only what the rules may say about it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// verbFromMember: the verb is a structured `verb` member — the model's word for what the
	// Tool does, never evidence of it — so the command is never Reversible.
	verbFromMember bool
}

// Parsed reports whether the rules can read the command.
func (c RemedyCommand) Parsed() bool { return c.Unparseable == "" }

// Reversible reports whether oto KNOWS the command's verb to be reversible. Everything else —
// delete, drain, patch, apply, create, set, a verb oto does not know, no verb, or a verb the
// model wrote as a structured `verb` member — is not.
func (c RemedyCommand) Reversible() bool {
	return c.Parsed() && !c.verbFromMember && reversibleVerbs[c.Verb]
}

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

// commandMeta are the characters a shell would treat specially in a command line. ⚠️ NOT THE
// GUARANTEE — wordPattern is: this list only lets the record say "a shell would interpret"
// rather than name a code point.
const commandMeta = ";|&`$<>(){}[]*?~#\\'\"\n\r"

// valueMeta are the characters that make any string in the arguments unparseable: the ones
// that chain, substitute or redirect if a ToolServer ever builds a shell line from it.
// ⚠️ A BELT: structured values are held by per-member patterns and command words by
// wordPattern; this no longer carries the guarantee.
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
	// ⭐ The whole document, token by token, before any decoder can fold or drop a key.
	if why := argumentKeys(arguments); why != "" {
		return unparseable("%s", why)
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
	case hasCmd || hasArgs:
		// ⛔ A command line is the ONLY member: a sibling (`context`, `kubeconfig`, `env`) is
		// something the ToolServer may act on and the rules cannot read.
		if len(args) != 1 {
			return unparseable("its arguments carry %s beside the command, which the rules cannot read",
				quoteShort(firstOtherMember(args, "command", "args")))
		}
		if hasCmd {
			return parseCommandValue(c, "command", cmd)
		}
		var words []string
		if !isJSONArray(argv) || json.Unmarshal(argv, &words) != nil {
			return unparseable("args is not an array of strings")
		}
		return parseCommandWords(c, "args", words)
	default:
		return parseStructured(c, args)
	}
}

// argumentKeys walks the whole JSON document token by token and says why the rules cannot
// read it — "" when they can. ⛔ A key twice in one object, two keys equal but for case, or a
// key with a character outside ASCII is refused at ANY depth: a decoder that keeps the last
// duplicate, or folds case as Go's struct decoder does, would read another document.
func argumentKeys(arguments string) string {
	dec := json.NewDecoder(strings.NewReader(arguments))
	dec.UseNumber()
	type frame struct {
		object, wantKey bool
		keys            map[string]bool // lower-cased: a case variant is a duplicate
	}
	var stack []*frame
	values := 0
	afterValue := func() {
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].wantKey = true
		}
	}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "its arguments are not JSON"
		}
		if len(stack) == 0 {
			if values++; values > 1 {
				return "its arguments are not one JSON object"
			}
		}
		if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].wantKey {
			if d, ok := tok.(json.Delim); ok && d == '}' {
				stack = stack[:n-1]
				afterValue()
				continue
			}
			key, _ := tok.(string)
			for i := 0; i < len(key); i++ {
				if key[i] < 0x20 || key[i] > 0x7e {
					return fmt.Sprintf("its arguments have the key %s, with a character the rules do not read", quoteShort(key))
				}
			}
			folded := strings.ToLower(key)
			if stack[n-1].keys[folded] {
				return fmt.Sprintf("its arguments carry the key %s twice (or twice but for case), and the rules cannot tell which is read", quoteShort(key))
			}
			stack[n-1].keys[folded] = true
			stack[n-1].wantKey = false
			continue
		}
		switch d, _ := tok.(json.Delim); d {
		case '{':
			stack = append(stack, &frame{object: true, wantKey: true, keys: map[string]bool{}})
		case '[':
			stack = append(stack, &frame{})
		case ']':
			stack = stack[:len(stack)-1]
			afterValue()
		default:
			afterValue()
		}
	}
	return ""
}

// firstOtherMember is the first member, in sorted order, that is none of `known`.
func firstOtherMember(args map[string]json.RawMessage, known ...string) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		if !slices.Contains(known, k) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

func isJSONArray(raw json.RawMessage) bool {
	return len(raw) > 0 && raw[0] == '['
}

func isJSONString(raw json.RawMessage) bool {
	return len(raw) > 0 && raw[0] == '"'
}

// parseCommandValue reads `command`: a string split on spaces and tabs, or an array of words.
func parseCommandValue(c RemedyCommand, member string, raw json.RawMessage) RemedyCommand {
	var line string
	if isJSONString(raw) && json.Unmarshal(raw, &line) == nil {
		if strings.ContainsAny(line, commandMeta) {
			c.Unparseable = fmt.Sprintf("its %s contains %s, which a shell would interpret", member, quoteMeta(line))
			return c
		}
		// ⛔ Split on space and tab ONLY. Any other whitespace stays inside a word, where
		// wordPattern refuses it — it is never split on, so no ToolServer's split can differ.
		return parseCommandWords(c, member, strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' }))
	}
	var words []string
	if !isJSONArray(raw) || json.Unmarshal(raw, &words) != nil {
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
	// file the rules cannot see into. ⛔ `--context` and `--cluster` are here (C4): a rule
	// about `payments` was written about one cluster, and either flag points the same
	// command at another.
	forbiddenFlags = map[string]string{
		"context": "changes the cluster kubectl connects to", "cluster": "changes the cluster kubectl connects to",
		"tls-server-name": "changes how kubectl trusts the API server",
		"as-user-extra":   "impersonates another identity",
		"cache-dir":       "changes where kubectl reads its cached discovery from",
		"profile":         "writes a profile to a file", "profile-output": "writes a profile to a file",
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
		"namespace": true, "n": true, "selector": true, "l": true, "request-timeout": true,
		"field-selector": true, "output": true, "o": true, "replicas": true, "current-replicas": true,
		"timeout": true, "grace-period": true, "cascade": true, "to-revision": true, "container": true, "c": true,
		"type": true, "patch": true, "p": true, "revision": true, "field-manager": true, "resource-version": true,
		"pod-selector": true, "chunk-size": true, "subresource": true, "min": true, "max": true, "cpu-percent": true,
		"image": true, "port": true, "reason": true,
	}
	// boolFlags take none. ⛔ A flag in none of these lists is unparseable in EVERY form —
	// `--x=y` included (C4): its arity may be plain, but what it changes is not.
	boolFlags = map[string]bool{
		"all": true, "all-namespaces": true, "A": true, "force": true, "now": true, "wait": true, "overwrite": true,
		"record": true, "local": true, "ignore-not-found": true, "ignore-daemonsets": true,
		"delete-emptydir-data": true, "disable-eviction": true, "dry-run": true, "no-headers": true,
		"show-labels": true, "watch": true, "w": true,
	}
)

// wordPattern is every word of a command line: ASCII letters, digits and `_ . : / = @ , + -`.
// ⭐⭐ AN ALLOWLIST (C2). `deployment/api`, `--replicas=3`, `-l app=api,tier=web` and an image
// `reg.io/a/b:1.2@sha256:ab` are in it; a space a ToolServer might split on, a control
// character and every non-ASCII rune are not.
var wordPattern = regexp.MustCompile(`^[A-Za-z0-9_.:/=@,+-]+$`)

// wordRefusal is why one word is outside wordPattern — "" when it is inside.
func wordRefusal(member, w string) string {
	if w == "" {
		return fmt.Sprintf("its %s has an empty word", member)
	}
	if wordPattern.MatchString(w) {
		return ""
	}
	if strings.ContainsAny(w, commandMeta) {
		return fmt.Sprintf("its %s has a word %s, which a shell would interpret", member, quoteMeta(w))
	}
	for _, r := range w {
		if r == ' ' || r == '\t' {
			return fmt.Sprintf("its %s has a word with a space or tab inside it, which a shell would split", member)
		}
		if r > 0x7e || !wordPattern.MatchString(string(r)) {
			return fmt.Sprintf("its %s has a character the rules do not read: %U", member, r)
		}
	}
	return fmt.Sprintf("its %s has a word %s the rules do not read", member, quoteShort(w))
}

// parseCommandWords reads a command line, already split into words.
func parseCommandWords(c RemedyCommand, member string, words []string) RemedyCommand {
	unparseable := func(format string, a ...any) RemedyCommand {
		c.Unparseable = fmt.Sprintf(format, a...)
		return c
	}
	for _, w := range words {
		if why := wordRefusal(member, w); why != "" {
			return unparseable("%s", why)
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
				if value != "" && !namespacePattern.MatchString(value) {
					return unparseable("its namespace %s is not a namespace name", quoteShort(value))
				}
				if why := setNS(value); why != "" {
					return unparseable("%s", why)
				}
			}
		case boolFlags[name]:
			// takes no value; `--flag=value` is kubectl's own business.
		default:
			return unparseable("it has the flag %s, which oto does not know", quoteShort(w))
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
	k, known := knownKind(raw)
	if !known {
		if CanonicalKind(raw) == "" {
			return fmt.Sprintf("%s is not a resource kind", quoteShort(raw))
		}
		// ⛔ C5: a kind outside kindAliases is compared as written, so a rule naming its
		// singular would silently miss its plural — and under most-severe-wins a rule that
		// says TWO is the one that silently stops matching.
		return fmt.Sprintf("oto does not know the kind %s's other spellings, so no rule can be trusted to match it", quoteShort(raw))
	}
	if c.Kind != "" && c.Kind != k {
		return fmt.Sprintf("it names more than one kind (%s and %s)", c.Kind, k)
	}
	c.Kind = k
	return ""
}

// structuredKindMembers are the members that may name the kind, in the order they are read.
var structuredKindMembers = []string{"kind", "resource", "resourceType"}

// structuredMembers is the CLOSED set of members structured arguments may carry (C1+C3).
var structuredMembers = []string{"verb", "kind", "resource", "resourceType", "namespace", "name"}

// k8sNamePattern is a Kubernetes object name (a DNS subdomain).
var k8sNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

// parseStructured reads arguments that carry no command line. ⛔ Every member is one the
// rules read, every value a JSON string matching its member's pattern; anything else is a
// thing the ToolServer may act on and the rules cannot see, so it is unparseable.
func parseStructured(c RemedyCommand, args map[string]json.RawMessage) RemedyCommand {
	unparseable := func(format string, a ...any) RemedyCommand {
		c.Unparseable = fmt.Sprintf(format, a...)
		return c
	}
	if other := firstOtherMember(args, structuredMembers...); other != "" {
		return unparseable("its arguments carry %s, which the rules cannot read", quoteShort(other))
	}
	values := map[string]string{}
	for member, raw := range args {
		var s string
		if !isJSONString(raw) || json.Unmarshal(raw, &s) != nil {
			return unparseable("its %s is not a string", member)
		}
		for _, r := range s {
			if r < 0x20 || r > 0x7e {
				return unparseable("its %s has a character the rules do not read: %U", member, r)
			}
		}
		if strings.TrimSpace(s) != s {
			return unparseable("its %s begins or ends with a space", member)
		}
		values[member] = s
	}
	if verb, has := values["verb"]; has {
		v := NormalizeRiskVerb(verb)
		if !riskVerbPattern.MatchString(v) {
			return unparseable("its verb %s is not a verb the rules can read", quoteShort(verb))
		}
		// ⛔ The model's word for what the Tool does: never evidence it is reversible.
		c.Verb, c.verbFromMember = v, true
	}
	for _, member := range structuredKindMembers {
		if kind, has := values[member]; has {
			if why := c.addKind(kind); why != "" {
				return unparseable("%s", why)
			}
		}
	}
	if ns, has := values["namespace"]; has {
		if !namespacePattern.MatchString(ns) {
			return unparseable("its namespace %s is not a namespace name", quoteShort(ns))
		}
		c.Namespace = ns
	}
	if name, has := values["name"]; has && !k8sNamePattern.MatchString(name) {
		return unparseable("its name %s is not an object name", quoteShort(name))
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
// spelling. ⛔ A kind not here is UNPARSEABLE in a command and refused in a rule (C5): oto
// cannot know a custom kind's other spellings, so it cannot promise a rule naming one
// matches a command naming another.
var kindAliases = func() map[string]string {
	groups := map[string][]string{
		"pod":                            {"pods", "po"},
		"deployment":                     {"deployments", "deploy"},
		"statefulset":                    {"statefulsets", "sts"},
		"daemonset":                      {"daemonsets", "ds"},
		"replicaset":                     {"replicasets", "rs"},
		"replicationcontroller":          {"replicationcontrollers", "rc"},
		"service":                        {"services", "svc"},
		"secret":                         {"secrets"},
		"configmap":                      {"configmaps", "cm"},
		"namespace":                      {"namespaces", "ns"},
		"node":                           {"nodes", "no"},
		"job":                            {"jobs"},
		"cronjob":                        {"cronjobs", "cj"},
		"ingress":                        {"ingresses", "ing"},
		"persistentvolumeclaim":          {"persistentvolumeclaims", "pvc"},
		"persistentvolume":               {"persistentvolumes", "pv"},
		"serviceaccount":                 {"serviceaccounts", "sa"},
		"horizontalpodautoscaler":        {"horizontalpodautoscalers", "hpa"},
		"poddisruptionbudget":            {"poddisruptionbudgets", "pdb"},
		"networkpolicy":                  {"networkpolicies", "netpol"},
		"role":                           {"roles"},
		"rolebinding":                    {"rolebindings"},
		"clusterrole":                    {"clusterroles"},
		"clusterrolebinding":             {"clusterrolebindings"},
		"customresourcedefinition":       {"customresourcedefinitions", "crd", "crds"},
		"endpoints":                      {"ep"},
		"event":                          {"events", "ev"},
		"storageclass":                   {"storageclasses", "sc"},
		"limitrange":                     {"limitranges", "limits"},
		"resourcequota":                  {"resourcequotas", "quota"},
		"certificatesigningrequest":      {"certificatesigningrequests", "csr"},
		"validatingwebhookconfiguration": {"validatingwebhookconfigurations"},
		"mutatingwebhookconfiguration":   {"mutatingwebhookconfigurations"},
		"endpointslice":                  {"endpointslices"},
		"lease":                          {"leases"},
		"priorityclass":                  {"priorityclasses", "pc"},
		"ingressclass":                   {"ingressclasses"},
		"podtemplate":                    {"podtemplates"},
		"runtimeclass":                   {"runtimeclasses"},
		"controllerrevision":             {"controllerrevisions"},
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

// foldKind is a kind lower-cased and without its API group (`deployments.apps` → `deployments`).
func foldKind(raw string) string {
	k := strings.ToLower(strings.TrimSpace(raw))
	k, _, _ = strings.Cut(k, ".")
	return k
}

// knownKind is a kind folded onto kubectl's singular name, and whether oto knows it at all
// (kindAliases). Only a known kind is compared by a rule.
func knownKind(raw string) (string, bool) {
	canonical, ok := kindAliases[foldKind(raw)]
	return canonical, ok
}

// CanonicalKind is the one spelling a rule and a command compare: lower-cased, without an
// API group, and folded onto kubectl's singular name for a known kind. A kind oto does not
// know comes back as written, lower-cased — what a rule stored before C5 holds — and "" when
// it is not a kind at all. ⚠️ Whether it is KNOWN is knownKind's question: an unknown kind is
// unparseable in a command and refused in a new rule.
func CanonicalKind(raw string) string {
	if canonical, ok := knownKind(raw); ok {
		return canonical
	}
	if k := foldKind(raw); kindPattern.MatchString(k) {
		return k
	}
	return ""
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
