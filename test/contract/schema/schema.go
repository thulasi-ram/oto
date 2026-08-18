// Package schema turns api/openapi/openapi.yaml into an executable assertion.
//
// # Why this exists
//
// Every finding in the conformance audit was a response body that had drifted
// away from the contract while nothing looked: `getVersion` with no {data,meta}
// envelope, `/readyz` in the wrong shape, `delivery_summary` missing two
// required members, a `NotificationReason` the server emitted and the contract
// rejected. The causal chain was one link long — nothing had ever checked.
//
// A test that re-states the expected shape BY HAND cannot close that gap: it is
// a second copy of the contract, and a second copy drifts exactly the way the
// DTOs did. The audit would simply have to be repeated against the tests.
//
// So the assertion here reads the ONE contract. `schema.Assert(t, "getSource",
// 200, body)` compiles the schema the spec declares for that operation and that
// status and validates the bytes the handler actually wrote against it. When
// someone adds a required member to `SourceResponse`, every handler that fails
// to emit it fails immediately, in the package that owns it, with the JSON
// pointer of the offending member in the failure message.
//
// OpenAPI 3.1 schemas ARE JSON Schema 2020-12 (that is the headline change of
// 3.1), so no translation layer is needed: the document is handed to a
// JSON-Schema compiler as-is and the response schema is addressed by its JSON
// pointer.
package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// ContractURL is the base URI the OpenAPI document is registered under. It is
// not fetched; it only has to be stable so that JSON pointers resolve.
const ContractURL = "https://oto.invalid/openapi.yaml"

// MediaTypeJSON is the media type every JSON response in this API uses.
const MediaTypeJSON = "application/json"

// ErrNoBody reports that the contract declares no body for a status — a 204,
// or a response whose media type is not JSON (SSE, Prometheus text, …).
var ErrNoBody = errors.New("the contract declares no JSON body for this response")

// Operation is one declared operation, as the contract states it.
type Operation struct {
	// ID is the operationId.
	ID string
	// Method is the upper-case HTTP method.
	Method string
	// Path is the templated path, e.g. /api/v1/sources/{id}.
	Path string
	// Statuses are the declared response codes, ascending.
	Statuses []int
	// PathParams are the {names} in Path, in order.
	PathParams []string
}

// HasPathParam reports whether the operation addresses a resource by id.
func (o Operation) HasPathParam(name string) bool {
	for _, p := range o.PathParams {
		if p == name {
			return true
		}
	}
	return false
}

// Declares reports whether the contract declares the given status for o.
func (o Operation) Declares(status int) bool {
	for _, s := range o.Statuses {
		if s == status {
			return true
		}
	}
	return false
}

// SuccessStatus is the first 2xx the operation declares, or 0.
func (o Operation) SuccessStatus() int {
	for _, s := range o.Statuses {
		if s >= 200 && s < 300 {
			return s
		}
	}
	return 0
}

type document struct {
	raw map[string]any
	// ops is indexed by operationId.
	ops map[string]Operation
	// order preserves document order, so a table-driven test over every
	// operation runs in a stable, reviewable sequence.
	order []string

	compiler *jsonschema.Compiler

	mu       sync.Mutex
	compiled map[string]*jsonschema.Schema
}

var (
	loadOnce sync.Once
	doc      *document
	loadErr  error
)

// load parses and indexes the contract exactly once per test binary.
func load() (*document, error) {
	loadOnce.Do(func() { doc, loadErr = parse() })
	return doc, loadErr
}

// ContractPath locates api/openapi/openapi.yaml by walking up from the working
// directory to the module root, so that a test in any package finds it.
func ContractPath() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			p := filepath.Join(dir, "api", "openapi", "openapi.yaml")
			if _, err := os.Stat(p); err != nil {
				return "", fmt.Errorf("module root %s has no api/openapi/openapi.yaml", dir)
			}
			return p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod above the working directory")
		}
		dir = parent
	}
}

func parse() (*document, error) {
	path, err := ContractPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // a repo-relative path resolved from go.mod
	if err != nil {
		return nil, err
	}

	var tree any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	// Round-trip through JSON so that every number is a json.Number, which is
	// what the schema compiler requires, and so that YAML-only node types
	// (timestamps in examples, mostly) become their JSON spellings.
	asJSON, err := json.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("re-encode %s: %w", path, err)
	}
	jsonDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(asJSON))
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	root, ok := jsonDoc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is not an object", path)
	}
	stripAnnotations(root, "")

	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	// Formats are asserted. `format: date-time` and `format: uuid` are load
	// bearing here: a case timestamp emitted as a unix integer, or an id
	// emitted as an internal integer key, is precisely the drift this package
	// exists to catch, and without assertion both would pass as "a string".
	c.AssertFormat()
	if err := c.AddResource(ContractURL, jsonDoc); err != nil {
		return nil, fmt.Errorf("register %s: %w", path, err)
	}

	d := &document{raw: root, compiler: c, ops: map[string]Operation{}, compiled: map[string]*jsonschema.Schema{}}
	if err := d.index(); err != nil {
		return nil, err
	}
	return d, nil
}

var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

func (d *document) index() error {
	paths, ok := d.raw["paths"].(map[string]any)
	if !ok {
		return errors.New("the contract has no paths object")
	}
	// Document order is not recoverable from a Go map, so the operation list is
	// sorted by (path, method-order) instead — deterministic, which is all a
	// table-driven test needs.
	tmpl := make([]string, 0, len(paths))
	for p := range paths {
		tmpl = append(tmpl, p)
	}
	sortStrings(tmpl)

	for _, p := range tmpl {
		item, ok := paths[p].(map[string]any)
		if !ok {
			continue
		}
		for _, m := range httpMethods {
			op, ok := item[m].(map[string]any)
			if !ok {
				continue
			}
			id, _ := op["operationId"].(string)
			if id == "" {
				return fmt.Errorf("%s %s has no operationId", strings.ToUpper(m), p)
			}
			o := Operation{ID: id, Method: strings.ToUpper(m), Path: p, PathParams: pathParams(p)}
			if resp, ok := op["responses"].(map[string]any); ok {
				codes := make([]int, 0, len(resp))
				for code := range resp {
					n, err := strconv.Atoi(code)
					if err != nil {
						// `default` is a legal response key; it is not a status.
						continue
					}
					codes = append(codes, n)
				}
				sortInts(codes)
				o.Statuses = codes
			}
			if _, dup := d.ops[id]; dup {
				return fmt.Errorf("operationId %q is declared twice", id)
			}
			d.ops[id] = o
			d.order = append(d.order, id)
		}
	}
	return nil
}

func pathParams(p string) []string {
	var out []string
	for {
		i := strings.Index(p, "{")
		if i < 0 {
			return out
		}
		j := strings.Index(p[i:], "}")
		if j < 0 {
			return out
		}
		out = append(out, p[i+1:i+j])
		p = p[i+j+1:]
	}
}

// stripAnnotations removes OpenAPI's annotation-only keywords, which are not
// JSON Schema keywords and would otherwise be mis-read by the compiler — most
// importantly `examples`, which is a map in OpenAPI's Media Type Object and an
// array in JSON Schema.
//
// A key is only removed when it is a KEYWORD. Under `properties` the keys are
// property names, so a payload with a member genuinely called `example` keeps
// its schema.
func stripAnnotations(node any, parentKey string) {
	switch n := node.(type) {
	case map[string]any:
		inProperties := parentKey == "properties" || parentKey == "patternProperties" ||
			parentKey == "$defs" || parentKey == "definitions"
		for k, v := range n {
			if !inProperties && isAnnotationKeyword(k) {
				delete(n, k)
				continue
			}
			stripAnnotations(v, k)
		}
	case []any:
		for _, v := range n {
			stripAnnotations(v, parentKey)
		}
	}
}

func isAnnotationKeyword(k string) bool {
	switch k {
	case "example", "examples", "discriminator", "externalDocs", "xml":
		return true
	default:
		return false
	}
}

/* -------------------------------------------------------------------------- */
/* The public assertions                                                      */
/* -------------------------------------------------------------------------- */

// Operations returns every operation the contract declares, in a deterministic
// order. A test that wants to be exhaustive over the API asks for this rather
// than keeping its own list, because a list kept by hand is how an operation
// gets added with no test.
func Operations(t testing.TB) []Operation {
	t.Helper()
	ops, err := AllOperations()
	if err != nil {
		t.Fatalf("load the OpenAPI contract: %v", err)
	}
	return ops
}

// AllOperations is Operations without a testing.TB, for callers outside a test.
func AllOperations() ([]Operation, error) {
	d, err := load()
	if err != nil {
		return nil, err
	}
	out := make([]Operation, 0, len(d.order))
	for _, id := range d.order {
		out = append(out, d.ops[id])
	}
	return out, nil
}

// Lookup is Op without a testing.TB.
func Lookup(operationID string) (Operation, error) {
	d, err := load()
	if err != nil {
		return Operation{}, err
	}
	o, ok := d.ops[operationID]
	if !ok {
		return Operation{}, fmt.Errorf("the contract declares no operation %q", operationID)
	}
	return o, nil
}

// Op returns one operation by id, failing the test if the contract has no such
// operation — which is itself a useful assertion: a handler test naming an
// operationId that no longer exists is a test asserting nothing.
func Op(t testing.TB, operationID string) Operation {
	t.Helper()
	d, err := load()
	if err != nil {
		t.Fatalf("load the OpenAPI contract: %v", err)
	}
	o, ok := d.ops[operationID]
	if !ok {
		t.Fatalf("the contract declares no operation %q", operationID)
	}
	return o
}

// Assert validates body against the schema the contract declares for
// (operationID, status) with an application/json body.
//
// It fails the test when the contract declares no such response, when the body
// is not JSON, or when the body does not satisfy the schema. A 204 (or any
// response the contract gives no JSON body) must be asserted with AssertNoBody.
func Assert(t testing.TB, operationID string, status int, body []byte) {
	t.Helper()
	sch, err := Response(operationID, status, MediaTypeJSON)
	if err != nil {
		t.Fatalf("%s %d: %v", operationID, status, err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s %d: the response body is not JSON: %v\nbody = %s", operationID, status, err, body)
	}
	if err := sch.Validate(v); err != nil {
		t.Fatalf("%s %d: the response does not satisfy the schema the contract declares.\n%v\nbody = %s",
			operationID, status, err, body)
	}
}

// AssertNoBody asserts that the contract declares no JSON body for
// (operationID, status) and that the handler wrote none.
func AssertNoBody(t testing.TB, operationID string, status int, body []byte) {
	t.Helper()
	if _, err := Response(operationID, status, MediaTypeJSON); !errors.Is(err, ErrNoBody) {
		t.Fatalf("%s %d: the contract DOES declare a JSON body here (%v); assert it with schema.Assert",
			operationID, status, err)
	}
	if len(bytes.TrimSpace(body)) != 0 {
		t.Fatalf("%s %d: the contract declares no body, but the handler wrote %d bytes: %s",
			operationID, status, len(body), body)
	}
}

// AssertProblem validates an RFC 9457 problem body against the schema the
// contract declares for that operation and status, and additionally checks the
// two members a caller actually branches on: `status` must agree with the status
// line, and `code` must be non-empty.
//
// A problem whose `status` member disagrees with its status line is a body that
// makes a client choose which one to believe.
func AssertProblem(t testing.TB, operationID string, status int, body []byte) {
	t.Helper()
	sch, err := Response(operationID, status, "application/problem+json")
	if err != nil {
		// Some responses declare the problem under application/json instead;
		// fall back rather than failing on the media type alone.
		sch, err = Response(operationID, status, MediaTypeJSON)
	}
	if err != nil {
		t.Fatalf("%s %d: %v", operationID, status, err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s %d: the problem body is not JSON: %v\nbody = %s", operationID, status, err, body)
	}
	if err := sch.Validate(v); err != nil {
		t.Fatalf("%s %d: the problem does not satisfy the contract's Problem schema.\n%v\nbody = %s",
			operationID, status, err, body)
	}

	var p struct {
		Status int    `json:"status"`
		Code   string `json:"code"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("%s %d: decode problem: %v", operationID, status, err)
	}
	if p.Status != status {
		t.Fatalf("%s: the problem says status %d but the status line said %d; a client cannot believe both",
			operationID, p.Status, status)
	}
	if p.Code == "" {
		t.Fatalf("%s %d: the problem carries no `code`; a client can only branch on the prose", operationID, status)
	}
}

// Response compiles the response schema the contract declares for
// (operationID, status, mediaType).
func Response(operationID string, status int, mediaType string) (*jsonschema.Schema, error) {
	d, err := load()
	if err != nil {
		return nil, err
	}
	op, ok := d.ops[operationID]
	if !ok {
		return nil, fmt.Errorf("the contract declares no operation %q", operationID)
	}

	ptr := "/paths/" + escape(op.Path) + "/" + strings.ToLower(op.Method) +
		"/responses/" + escape(strconv.Itoa(status))
	node, err := d.at(ptr)
	if err != nil {
		return nil, fmt.Errorf("the contract declares no %d for %s (%s %s): %w",
			status, operationID, op.Method, op.Path, err)
	}
	node, ptr, err = d.deref(node, ptr)
	if err != nil {
		return nil, err
	}

	obj, ok := node.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s %d: the response is not an object", operationID, status)
	}
	content, ok := obj["content"].(map[string]any)
	if !ok {
		return nil, ErrNoBody
	}
	mt, ok := content[mediaType].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s %d: %w (declared media types: %s)",
			operationID, status, ErrNoBody, strings.Join(keysOf(content), ", "))
	}
	if _, ok := mt["schema"]; !ok {
		return nil, fmt.Errorf("%s %d: %s has no schema", operationID, status, mediaType)
	}
	ptr += "/content/" + escape(mediaType) + "/schema"

	d.mu.Lock()
	defer d.mu.Unlock()
	if sch, ok := d.compiled[ptr]; ok {
		return sch, nil
	}
	sch, err := d.compiler.Compile(ContractURL + "#" + ptr)
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", ptr, err)
	}
	d.compiled[ptr] = sch
	return sch, nil
}

// RequestBody compiles the request-body schema for an operation, so that a test
// can prove the fixture it POSTs is one a real client could have sent.
func RequestBody(operationID string) (*jsonschema.Schema, error) {
	d, err := load()
	if err != nil {
		return nil, err
	}
	op, ok := d.ops[operationID]
	if !ok {
		return nil, fmt.Errorf("the contract declares no operation %q", operationID)
	}
	ptr := "/paths/" + escape(op.Path) + "/" + strings.ToLower(op.Method) + "/requestBody"
	node, err := d.at(ptr)
	if err != nil {
		return nil, fmt.Errorf("%s declares no request body: %w", operationID, err)
	}
	node, ptr, err = d.deref(node, ptr)
	if err != nil {
		return nil, err
	}
	obj, ok := node.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: requestBody is not an object", operationID)
	}
	content, ok := obj["content"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: requestBody has no content", operationID)
	}
	if _, ok := content[MediaTypeJSON].(map[string]any); !ok {
		return nil, fmt.Errorf("%s: requestBody has no %s (has %s)",
			operationID, MediaTypeJSON, strings.Join(keysOf(content), ", "))
	}
	ptr += "/content/" + escape(MediaTypeJSON) + "/schema"

	d.mu.Lock()
	defer d.mu.Unlock()
	if sch, ok := d.compiled[ptr]; ok {
		return sch, nil
	}
	sch, err := d.compiler.Compile(ContractURL + "#" + ptr)
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", ptr, err)
	}
	d.compiled[ptr] = sch
	return sch, nil
}

// AssertRequest validates a request fixture against the contract's request-body
// schema. It is what stops a handler test from "passing" with a body no client
// would ever be allowed to send.
func AssertRequest(t testing.TB, operationID string, body []byte) {
	t.Helper()
	sch, err := RequestBody(operationID)
	if err != nil {
		t.Fatalf("%s: %v", operationID, err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s: the request fixture is not JSON: %v", operationID, err)
	}
	if err := sch.Validate(v); err != nil {
		t.Fatalf("%s: the request fixture is not one the contract permits; no client could send it.\n%v\nbody = %s",
			operationID, err, body)
	}
}

// Component compiles a named schema from #/components/schemas, for the handful
// of assertions that are about a shared DTO rather than a whole response.
func Component(t testing.TB, name string) *jsonschema.Schema {
	t.Helper()
	d, err := load()
	if err != nil {
		t.Fatalf("load the OpenAPI contract: %v", err)
	}
	ptr := "/components/schemas/" + escape(name)
	if _, err := d.at(ptr); err != nil {
		t.Fatalf("the contract declares no schema %q: %v", name, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if sch, ok := d.compiled[ptr]; ok {
		return sch
	}
	sch, err := d.compiler.Compile(ContractURL + "#" + ptr)
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	d.compiled[ptr] = sch
	return sch
}

// MediaTypes returns the media types the contract declares for
// (operationID, status), sorted, or an empty slice when the response carries no
// body at all.
//
// It exists for gate G2, which drives a REAL server and therefore has to answer
// a question the handler-level assertions never ask: "is the Content-Type this
// handler actually wrote one the contract permits here?" A body that validates
// against the JSON schema while being served as `text/html` is still a response
// no generated client can read, and `Response` cannot report that — it takes the
// media type as an argument rather than returning it.
func MediaTypes(operationID string, status int) ([]string, error) {
	d, err := load()
	if err != nil {
		return nil, err
	}
	op, ok := d.ops[operationID]
	if !ok {
		return nil, fmt.Errorf("the contract declares no operation %q", operationID)
	}

	ptr := "/paths/" + escape(op.Path) + "/" + strings.ToLower(op.Method) +
		"/responses/" + escape(strconv.Itoa(status))
	node, err := d.at(ptr)
	if err != nil {
		return nil, fmt.Errorf("the contract declares no %d for %s (%s %s): %w",
			status, operationID, op.Method, op.Path, err)
	}
	if node, _, err = d.deref(node, ptr); err != nil {
		return nil, err
	}
	obj, ok := node.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s %d: the response is not an object", operationID, status)
	}
	content, ok := obj["content"].(map[string]any)
	if !ok {
		return []string{}, nil
	}
	out := keysOf(content)
	sortStrings(out)
	return out, nil
}

/* -------------------------------------------------------------------------- */
/* Pointer plumbing                                                           */
/* -------------------------------------------------------------------------- */

// at resolves a JSON pointer against the document.
func (d *document) at(ptr string) (any, error) {
	cur := any(d.raw)
	if ptr == "" || ptr == "/" {
		return cur, nil
	}
	for _, tok := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		tok = unescape(tok)
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%q: %q is not under an object", ptr, tok)
		}
		cur, ok = m[tok]
		if !ok {
			return nil, fmt.Errorf("%q: no member %q", ptr, tok)
		}
	}
	return cur, nil
}

// deref follows a chain of local $refs, returning the final node and pointer.
func (d *document) deref(node any, ptr string) (any, string, error) {
	for range 16 {
		m, ok := node.(map[string]any)
		if !ok {
			return node, ptr, nil
		}
		ref, ok := m["$ref"].(string)
		if !ok {
			return node, ptr, nil
		}
		if !strings.HasPrefix(ref, "#/") {
			return nil, "", fmt.Errorf("%s: external $ref %q is not supported", ptr, ref)
		}
		next := strings.TrimPrefix(ref, "#")
		n, err := d.at(next)
		if err != nil {
			return nil, "", err
		}
		node, ptr = n, next
	}
	return nil, "", fmt.Errorf("$ref cycle at %s", ptr)
}

func escape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func unescape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// sortStrings and sortInts are tiny insertion sorts, so that this package needs
// nothing but the compiler and the YAML parser.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func sortInts(s []int) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
