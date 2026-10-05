package domain

// ONE CONFIGURED TOOLSERVER (ADR 0016, 0053 §1 §3 §6, 0054 §5; git-bug 2e9a086): an MCP
// server the operator runs, reached at a URL with the ToolServer's own access token, and
// the Tools it listed the last time oto asked.
//
// ⭐⭐ A TOOLSERVER IS WHERE TRUST STOPS, AND oto SHIPS NONE (ADR 0054 §5). oto holds no
// cluster credential: the ToolServer's ServiceAccount and RBAC are its operator's, and
// the one secret oto keeps is the ToolServer's access token — sealed in
// `channel_credentials` (kind `tool_server_token`), exactly as a model endpoint's key is.
// ToolServerConfig has no token field, so it can be logged, rendered or put in a Step
// whole.
//
// ⭐⭐ READ OR WRITE IS DECLARED PER TOOLSERVER, NOT GUESSED PER TOOL. An operator states
// `access: read` or `access: write` when configuring one, and that declaration is the
// trust boundary: an Investigator's allowlist may name Tools ONLY on `read` ToolServers
// (ADR 0053 §3: "while investigating it holds only read-only Tools"). A `write`
// ToolServer can be configured and discovered today and is never offered to a model; it
// is the slot a Remedy (ADR 0054, git-bug 4148256) binds its write Tool to — "the write
// Tool is never in the Investigator's hands; execution is a separate step" — and the
// thing its approval grant is held ON (§4). Two reasons it is the server and not the Tool:
//
//   - MCP's `readOnlyHint` is a HINT the server writes about itself, defaulting to false
//     and set by nobody; trusting it would let a ToolServer promote its own Tools into
//     an Investigator's hands. ⛔ It NEVER PROMOTES a Tool. It may only REFUSE one: a
//     Tool its own server marks not read-only (`readOnlyHint: false`) is not held while
//     investigating even on a `read` ToolServer (HeldWhileInvestigating, review D1) —
//     a lie in that direction gains a server nothing. A Tool with no annotations is
//     holdable, and then the declaration is the only guard; a mixed server is `write`.
//   - ADR 0054 §5 recommends a write ToolServer "run commands under RBAC narrower than
//     the read one" — two servers, two ServiceAccounts. The declaration names which is
//     which; a mixed server is configured as `write`, and so is never read through.
//
// ⭐ A TOOL IS NAMED `<toolserver>__<tool>` TO A MODEL AND IN AN ALLOWLIST. The model
// protocol's function-name alphabet is `[A-Za-z0-9_-]{1,64}` and 00092's allowlist CHECK
// is the same alphabet, so neither `/` nor `.` can separate the two halves. A ToolServer
// name has no underscore at all, so the FIRST `__` in a qualified name is always the
// separator, whatever the Tool's own name contains — and oto's built-in Tools
// (`oto_case_timeline`) contain no `__`, so the two namespaces cannot meet.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// Bounds mirroring migration 00093's CHECKs; the API DTO tags are the third copy.
const (
	// MaxToolServerNameLength is `tool_servers_name_ck`. Short, because the name is
	// half of every qualified Tool name and the whole is capped at 64.
	MaxToolServerNameLength = 24
	// MaxToolServerURLLength is `tool_servers_url_ck`.
	MaxToolServerURLLength = 2048
	// MaxToolServerTokenLength bounds the plaintext before it is sealed.
	MaxToolServerTokenLength = 4096

	// MinCallTimeoutSeconds and MaxCallTimeoutSeconds bound one Tool call
	// (`tool_servers_timeout_ck`). Two minutes is past any read a model should wait on
	// inside a run whose whole wall budget is minutes.
	MinCallTimeoutSeconds = 1
	MaxCallTimeoutSeconds = 120
	// DefaultCallTimeoutSeconds is a ToolServer's per-call timeout when its writer
	// names none.
	DefaultCallTimeoutSeconds = 15

	// MinResultBytes and MaxResultBytes bound what one call may hand the model
	// (`tool_servers_result_ck`). The ceiling leaves room under a Step's 65536-character
	// result for the truncation and redaction notes.
	MinResultBytes = 1024
	MaxResultBytes = 61440
	// DefaultResultBytes is a ToolServer's result cap when its writer names none.
	DefaultResultBytes = 16 << 10

	// MaxDiscoveredToolNameLength is `tool_server_tools_name_ck`: MCP's own ceiling.
	MaxDiscoveredToolNameLength = 128
	// MaxToolDescriptionLength is `tool_server_tools_desc_ck`, in characters. A longer
	// description is clipped, not refused: it is the ToolServer's prose, not oto's.
	MaxToolDescriptionLength = 4096
	// MaxToolSchemaBytes bounds a stored input schema. A larger one is recorded as
	// unusable rather than truncated — half a JSON Schema is not a schema.
	MaxToolSchemaBytes = 64 << 10
	// MaxDiscoveredTools bounds one discovery. A ToolServer listing more is refused
	// whole: an operator who exposes that many Tools has pointed oto at the wrong thing.
	// It is also the API's page ceiling, so a Tool list is always one page.
	MaxDiscoveredTools = 200
)

// ToolServerCredentialKind is the `channel_credentials.kind` a ToolServer's access token
// is sealed as. Bound into the seal as additional authenticated data, so a model key's
// ciphertext moved onto a ToolServer row fails to open rather than being presented to
// a cluster-facing server.
const ToolServerCredentialKind = "tool_server_token"

// ToolServerCredentialValueKey is the one value the sealed map carries.
const ToolServerCredentialValueKey = "token"

// QualifiedToolSeparator joins a ToolServer's name to one of its Tools' names.
const QualifiedToolSeparator = "__"

// reservedToolServerName is refused as a ToolServer name: `oto` prefixes the built-in
// Tools, and a ToolServer called `oto` would read as one of them in every transcript.
const reservedToolServerName = "oto"

var toolServerNamePattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,22}[a-z0-9])?$`)

// NewToolServerName checks a name against `tool_servers_name_ck`: 1 to 24 lower-case
// letters, digits and inner hyphens, starting with a letter. ⛔ No underscore — that is
// what keeps the first `__` of a qualified Tool name the separator.
func NewToolServerName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if !toolServerNamePattern.MatchString(name) {
		const msg = "a ToolServer's name is 1 to 24 lower-case letters, digits and inner hyphens, starting with a letter"
		return "", errs.Validation("tool_server_name_invalid", msg,
			errs.Violation{Field: "name", Code: "pattern", Message: msg})
	}
	if name == reservedToolServerName {
		const msg = "oto is the built-in Tools' prefix and cannot name a ToolServer"
		return "", errs.Validation("tool_server_name_reserved", msg,
			errs.Violation{Field: "name", Code: "reserved", Message: msg})
	}
	return name, nil
}

// ToolServerAccess is what an operator declares a ToolServer may do.
type ToolServerAccess string

// The two declarations (`tool_servers_access_ck`).
const (
	// AccessRead: every Tool it serves only reads. Only these Tools may be on an
	// Investigator's allowlist.
	AccessRead ToolServerAccess = "read"
	// AccessWrite: it serves at least one Tool that changes something. Never offered
	// to a model while investigating; reserved for executing an approved Remedy
	// (ADR 0054 §5).
	AccessWrite ToolServerAccess = "write"
)

// ParseToolServerAccess reads a declaration, refusing anything else.
func ParseToolServerAccess(s string) (ToolServerAccess, error) {
	switch a := ToolServerAccess(strings.TrimSpace(s)); a {
	case AccessRead, AccessWrite:
		return a, nil
	default:
		const msg = "a ToolServer's access is read or write"
		return "", errs.Validation("tool_server_access_invalid", msg,
			errs.Violation{Field: "access", Code: "enum", Message: msg})
	}
}

// ToolServerTransport is how oto speaks MCP to it. Both are HTTP: ⛔ there is no stdio
// transport, because oto runs no subprocess — a ToolServer is a server its operator
// runs and secures (ADR 0016: "a server the operator runs"; ADR 0054 §5).
type ToolServerTransport string

// The two transports (`tool_servers_transport_ck`).
const (
	// TransportStreamableHTTP is MCP's current HTTP transport.
	TransportStreamableHTTP ToolServerTransport = "streamable_http"
	// TransportSSE is the older HTTP+SSE transport, for servers that have not moved.
	TransportSSE ToolServerTransport = "sse"
)

// ParseToolServerTransport reads a transport; empty is the current one.
func ParseToolServerTransport(s string) (ToolServerTransport, error) {
	switch t := ToolServerTransport(strings.TrimSpace(s)); t {
	case "":
		return TransportStreamableHTTP, nil
	case TransportStreamableHTTP, TransportSSE:
		return t, nil
	default:
		const msg = "a ToolServer's transport is streamable_http or sse"
		return "", errs.Validation("tool_server_transport_invalid", msg,
			errs.Violation{Field: "transport", Code: "enum", Message: msg})
	}
}

// NormalizeToolServerURL is the one spelling of a ToolServer's endpoint: lower-case
// scheme and host, the path exactly as given (an MCP endpoint's path is the server's
// to choose, trailing slash included), and no query or fragment.
//
// ⛔ USERINFO, A QUERY AND A FRAGMENT ARE REFUSED, NOT STRIPPED. This column is shown
// and logged; a token belongs in the sealed slot, and `?api_key=` is a token.
func NormalizeToolServerURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	bad := func(msg string) error {
		return errs.Validation("tool_server_url_invalid", msg,
			errs.Violation{Field: "url", Code: "format", Message: msg})
	}
	if raw == "" || len(raw) > MaxToolServerURLLength {
		return "", bad("a ToolServer URL is 1 to 2048 characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", bad("the ToolServer URL does not parse")
	}
	scheme := strings.ToLower(u.Scheme)
	switch {
	case scheme != "https" && scheme != "http":
		return "", bad("a ToolServer URL is http or https")
	case u.Host == "":
		return "", bad("a ToolServer URL names a host")
	case u.User != nil:
		return "", bad("a ToolServer URL may not carry credentials; the access token is stored sealed")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", bad("a ToolServer URL has no query or fragment; the access token is stored sealed")
	}
	out := scheme + "://" + strings.ToLower(u.Host) + u.EscapedPath()
	if len(out) > MaxToolServerURLLength {
		return "", bad("a ToolServer URL is 1 to 2048 characters")
	}
	return out, nil
}

// TokenNeedsHTTPS refuses a token bound for a plaintext ToolServer — the rule
// KeyNeedsHTTPS states for a model key, for the same reason: a token is only ever sent
// over TLS, and a token-less ToolServer on the cluster network stays legal.
func TokenNeedsHTTPS(normalizedURL string, hasToken bool) error {
	if hasToken && !strings.HasPrefix(normalizedURL, "https://") {
		const msg = "an access token is only sent over https; use an https URL or store no token"
		return errs.Validation("tool_server_token_needs_https", msg,
			errs.Violation{Field: "url", Code: "https_required", Message: msg})
	}
	return nil
}

// CallLimits are one ToolServer's per-call controls (ADR 0053 §6: "Per-call timeout and
// result size cap — each Tool call"). They live on the ToolServer because its operator
// knows its latency and its verbosity; the Step records a timeout or a truncation and
// the run continues.
type CallLimits struct {
	Timeout        time.Duration
	MaxResultBytes int
}

// NewCallLimits builds the limits inside 00093's bounds. Zero takes the default.
func NewCallLimits(timeoutSeconds, maxResultBytes int) (CallLimits, error) {
	if timeoutSeconds == 0 {
		timeoutSeconds = DefaultCallTimeoutSeconds
	}
	if maxResultBytes == 0 {
		maxResultBytes = DefaultResultBytes
	}
	var v []errs.Violation
	if timeoutSeconds < MinCallTimeoutSeconds || timeoutSeconds > MaxCallTimeoutSeconds {
		v = append(v, errs.Violation{Field: "call_timeout_seconds", Code: "out_of_range", Message: "1 to 120 seconds"})
	}
	if maxResultBytes < MinResultBytes || maxResultBytes > MaxResultBytes {
		v = append(v, errs.Violation{Field: "max_result_bytes", Code: "out_of_range", Message: "1024 to 61440 bytes"})
	}
	if len(v) > 0 {
		return CallLimits{}, errs.Validation("tool_server_limits_invalid",
			"a ToolServer's per-call limits are outside the range oto will accept", v...)
	}
	return CallLimits{Timeout: time.Duration(timeoutSeconds) * time.Second, MaxResultBytes: maxResultBytes}, nil
}

// TimeoutSeconds is Timeout as the column stores it.
func (l CallLimits) TimeoutSeconds() int { return int(l.Timeout / time.Second) }

// ToolServerConfig is one stored ToolServer, as read. It has no token field.
type ToolServerConfig struct {
	ID        uuid.UUID
	OrgID     uuid.UUID
	Name      string
	URL       string
	Transport ToolServerTransport
	Access    ToolServerAccess
	// CredentialID is the sealed token's row; uuid.Nil sends no token.
	CredentialID uuid.UUID
	Limits       CallLimits
	// DiscoveredAt is when oto last listed its Tools successfully; zero for never.
	DiscoveredAt time.Time
	// DiscoveryFailedAt and DiscoveryError are the last listing that failed since
	// then; zero and "" once one succeeds.
	DiscoveryFailedAt time.Time
	DiscoveryError    string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// HasToken reports whether a sealed token is stored for this ToolServer.
func (c ToolServerConfig) HasToken() bool { return c.CredentialID != uuid.Nil }

// Readable reports whether an Investigator may hold this ToolServer's Tools.
func (c ToolServerConfig) Readable() bool { return c.Access == AccessRead }

// ToolServerDraft is what an operator submits to configure a ToolServer.
//
// ⛔ Token IS PLAINTEXT, alive for one create call and sealed before the row is
// written. String, GoString and LogValue redact it.
type ToolServerDraft struct {
	Name      string
	URL       string
	Transport ToolServerTransport
	Access    ToolServerAccess
	Token     string
	Limits    CallLimits
}

// String renders the draft with the token redacted.
func (d ToolServerDraft) String() string {
	tok := "none"
	if d.Token != "" {
		tok = "[redacted]"
	}
	return "ToolServerDraft{name=" + d.Name + " url=" + d.URL + " transport=" + string(d.Transport) +
		" access=" + string(d.Access) + " token=" + tok + "}"
}

// GoString redacts too, so `%#v` is as safe as `%v`.
func (d ToolServerDraft) GoString() string { return d.String() }

// LogValue redacts for slog.
func (d ToolServerDraft) LogValue() slog.Value { return slog.StringValue(d.String()) }

// NewToolServerDraft validates a ToolServer as an operator states it.
func NewToolServerDraft(name, rawURL, transport, access, token string, limits CallLimits) (ToolServerDraft, error) {
	n, err := NewToolServerName(name)
	if err != nil {
		return ToolServerDraft{}, err
	}
	u, err := NormalizeToolServerURL(rawURL)
	if err != nil {
		return ToolServerDraft{}, err
	}
	t, err := ParseToolServerTransport(transport)
	if err != nil {
		return ToolServerDraft{}, err
	}
	a, err := ParseToolServerAccess(access)
	if err != nil {
		return ToolServerDraft{}, err
	}
	tok := strings.TrimSpace(token)
	if len(tok) > MaxToolServerTokenLength {
		return ToolServerDraft{}, errs.Validation("tool_server_token_invalid",
			"an access token is at most 4096 bytes",
			errs.Violation{Field: "token", Code: "length", Message: "at most 4096 bytes"})
	}
	if err := TokenNeedsHTTPS(u, tok != ""); err != nil {
		return ToolServerDraft{}, err
	}
	if limits.Timeout <= 0 || limits.MaxResultBytes <= 0 {
		return ToolServerDraft{}, errs.New(errs.KindInternal, "tool_server_limits_missing",
			"a ToolServer draft carries limits built by NewCallLimits")
	}
	return ToolServerDraft{Name: n, URL: u, Transport: t, Access: a, Token: tok, Limits: limits}, nil
}

// DiscoveredTool is one Tool a ToolServer listed, as oto recorded it.
type DiscoveredTool struct {
	// Name is the ToolServer's own name for it.
	Name        string
	Description string
	// InputSchema is its arguments' JSON Schema object, or nil when the ToolServer
	// listed one oto cannot offer a model (not an object, or past 64 KiB).
	InputSchema json.RawMessage
	// ReadOnlyHint is what the ToolServer SAYS about the Tool — nil when it said
	// nothing. ⛔ Recorded and shown; never what decides whether it may be held
	// (ToolServerConfig.Access does).
	ReadOnlyHint *bool
}

// NewDiscoveredTool builds one listed Tool. A name MCP itself would not allow is an
// error (the listing was malformed); a schema oto cannot offer is not — the Tool is
// recorded, and Usable says why it cannot be held.
func NewDiscoveredTool(name, description string, inputSchema json.RawMessage, readOnlyHint *bool) (DiscoveredTool, error) {
	if name == "" || utf8.RuneCountInString(name) > MaxDiscoveredToolNameLength || !utf8.ValidString(name) {
		return DiscoveredTool{}, errs.Newf(errs.KindUpstreamDown, "tool_server_listing_invalid",
			"the ToolServer listed a Tool whose name is empty or longer than %d characters", MaxDiscoveredToolNameLength)
	}
	var schema json.RawMessage
	if len(inputSchema) <= MaxToolSchemaBytes && isJSONObject(inputSchema) {
		schema = append(json.RawMessage(nil), inputSchema...)
	}
	return DiscoveredTool{Name: name, Description: clip(strings.TrimSpace(description), MaxToolDescriptionLength),
		InputSchema: schema, ReadOnlyHint: readOnlyHint}, nil
}

// Usable reports the qualified name this Tool is held and offered under on the named
// ToolServer — or "" and why it cannot be.
func (t DiscoveredTool) Usable(server string) (qualified, reason string) {
	q := server + QualifiedToolSeparator + t.Name
	switch {
	case !toolNamePattern.MatchString(q):
		return "", "its qualified name " + quoteShort(q) + " is not 1 to 64 letters, digits, underscores or hyphens, which is all a model can be offered"
	case t.InputSchema == nil:
		return "", "its input schema is not one JSON object of at most 64 KiB"
	default:
		return q, ""
	}
}

// HeldWhileInvestigating is Usable plus the one thing a Tool's own server can say against
// itself (review D1): a Tool marked `readOnlyHint: false` is refused even on a `read`
// ToolServer, because ADR 0053 §3 holds an Investigator to read-only Tools and the server
// has just said this one is not. The hint never promotes: a nil hint (no annotations) or
// a true one leaves the per-server declaration as the guard. A Remedy's write Tool is
// bound with Usable, never with this.
func (t DiscoveredTool) HeldWhileInvestigating(server string) (qualified, reason string) {
	q, why := t.Usable(server)
	if why != "" {
		return "", why
	}
	if t.ReadOnlyHint != nil && !*t.ReadOnlyHint {
		return "", "the ToolServer says this Tool is not read-only (MCP readOnlyHint is false); an Investigator " +
			"holds only read-only Tools — move it to a write ToolServer"
	}
	return q, ""
}

// SplitQualifiedToolName reads `<toolserver>__<tool>`. ok is false for any name that is
// not one — a built-in Tool's, or a malformed one.
func SplitQualifiedToolName(name string) (server, tool string, ok bool) {
	i := strings.Index(name, QualifiedToolSeparator)
	if i <= 0 || i+len(QualifiedToolSeparator) >= len(name) {
		return "", "", false
	}
	server, tool = name[:i], name[i+len(QualifiedToolSeparator):]
	if !toolServerNamePattern.MatchString(server) {
		return "", "", false
	}
	return server, tool, true
}

// ToolServerTools are the qualified names on an allowlist, grouped by the ToolServer
// they name, in allowlist order.
func (a Allowlist) ToolServerTools() map[string][]string {
	out := map[string][]string{}
	for _, n := range a.names {
		if server, tool, ok := SplitQualifiedToolName(n); ok {
			out[server] = append(out[server], tool)
		}
	}
	return out
}

// ToolServerClient is the port one ToolServer is reached through, satisfied by
// `investigator/toolservers/mcpclient`. It speaks MCP over HTTP; nothing here names the
// SDK (depguard: `mcp-sdk-is-adapter-only`).
//
// Every implementation must never put the access token in an error or a result, and
// must answer a ToolServer's own `isError` as a ToolResult, not an error: the model
// reads why its call failed and the run continues.
type ToolServerClient interface {
	// ListTools lists every Tool the ToolServer serves, following its pages.
	ListTools(ctx context.Context) ([]DiscoveredTool, error)
	// CallTool calls one Tool by the ToolServer's own name for it.
	CallTool(ctx context.Context, name string, args json.RawMessage) (ToolResult, error)
	// Close ends the session.
	Close() error
}

// ToolResult is what one Tool call answered: its text, and whether the ToolServer said
// the call failed (MCP's `isError` — a failure the model should read, not a transport
// error).
type ToolResult struct {
	Text    string
	IsError bool
}

// RemedyApprover is one user holding the Remedy approval grant on a write ToolServer, as
// the settings read shows it (ADR 0054 §4, git-bug 47f67c8). The grant itself is the
// `identity` module's; this is its read-only face here, delivered through a port.
//
// ⛔ NOTHING IN THIS MODULE GRANTS OR REVOKES ONE. A grant is given and taken from the host
// shell only — `oto grant remedy-approver` / `oto revoke remedy-approver` — so no route
// here can mint a second approver and defeat double approval.
type RemedyApprover struct {
	UserID      uuid.UUID
	Email       string
	DisplayName string
	GrantedAt   time.Time
	// GrantedBy is who wrote the grant: always `cli`.
	GrantedBy string
	// Counts is false when the holder is disabled: the grant is shown and lets them
	// approve nothing.
	Counts bool
}
