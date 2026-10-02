package domain

// ONE CONFIGURED MODEL ENDPOINT (git-bug 8f1f071 comment #1): a base URL, a model name
// and a sealed API key, stored per org in `model_providers` (migration 00091) and
// reached through the one Chat Completions adapter. Hosted APIs, gateways and
// self-hosted model servers are all this one shape; nothing here knows which.
//
// ⛔ THE KEY IS NOT A FIELD OF THIS TYPE. It is sealed in `channel_credentials` (kind
// `model_api_key`), the one sealed-secret store (SPEC §D.8), and ProviderConfig holds
// only the row's id. "The key is never returned" is therefore a property of the type
// rather than a habit of the code that builds it — the rule `CredentialMeta` keeps for
// channels — and a ProviderConfig can be logged, rendered or put in a Step whole.

import (
	"log/slog"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// Bounds mirroring migration 00091's CHECKs — R9's copies. There is no request DTO
// yet (the settings API is a follow-up), so the domain and the DDL are the two.
const (
	// MaxProviderNameLength is `model_providers_name_ck`.
	MaxProviderNameLength = 120
	// MaxBaseURLLength is `model_providers_base_url_ck`.
	MaxBaseURLLength = 2048
	// MaxModelNameLength is `model_providers_model_ck`. Model names with a vendor
	// prefix and a version suffix run long on a gateway (`gateway/vendor/model-
	// 2026-01-01-preview`); 200 is generous and still a bound.
	MaxModelNameLength = 200
	// MaxAPIKeyLength bounds the plaintext before it is sealed. `channel_credentials_
	// seal_ck` caps the ciphertext at 65536 bytes; a key is a few hundred at most.
	MaxAPIKeyLength = 4096
)

// CredentialKind is the `channel_credentials.kind` a model endpoint's key is sealed
// as. It is bound into the seal as additional authenticated data, so a Slack token's
// ciphertext moved onto this row fails to open rather than being sent to a model.
const CredentialKind = "model_api_key"

// CredentialValueKey is the one value the sealed map carries.
const CredentialValueKey = "api_key"

// ModelIdentity is what an Investigator version pins (ADR 0053 §6): WHICH endpoint and
// WHICH model. Two configs with the same identity answer the same way as far as oto
// can know; a change to either is a new Investigator version, so a Finding can always
// say which model produced it.
//
// ⚠️ THE ENDPOINT IS THE NORMALISED BASE URL, NOT THE CONFIG'S ID. Re-creating a
// config under a new id with the same URL and model is the same model; editing one in
// place to a new URL is not.
type ModelIdentity struct {
	Endpoint string
	Model    string
}

// String renders the identity as `<endpoint>#<model>`. It carries no secret: a base URL
// with credentials in it is refused by NormalizeBaseURL.
func (i ModelIdentity) String() string { return i.Endpoint + "#" + i.Model }

// IsZero reports whether the identity is unset.
func (i ModelIdentity) IsZero() bool { return i.Endpoint == "" && i.Model == "" }

// NormalizeBaseURL is the one spelling of an endpoint: lower-case scheme and host, no
// trailing slash, no query, no fragment.
//
// ⛔ A URL WITH USERINFO IS REFUSED, not stripped. `https://user:key@host/v1` puts a
// secret in a column that is shown, logged and pinned into every Finding; stripping it
// silently would also change which credential reaches the endpoint. The key goes in
// the sealed slot.
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	bad := func(msg string) error {
		return errs.Validation("model_base_url_invalid", msg,
			errs.Violation{Field: "base_url", Code: "format", Message: msg})
	}
	if raw == "" || len(raw) > MaxBaseURLLength {
		return "", bad("a base URL is 1 to 2048 characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", bad("the base URL does not parse")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", bad("a base URL is http or https")
	}
	if u.Host == "" {
		return "", bad("a base URL names a host")
	}
	if u.User != nil {
		return "", bad("a base URL may not carry credentials; the API key is stored sealed")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", bad("a base URL has no query or fragment")
	}
	out := scheme + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.EscapedPath(), "/")
	if len(out) > MaxBaseURLLength {
		return "", bad("a base URL is 1 to 2048 characters")
	}
	return out, nil
}

// ProviderConfig is one stored model endpoint, as read. It has no key field.
type ProviderConfig struct {
	ID    uuid.UUID
	OrgID uuid.UUID
	// Name is the operator's label for it, unique in the org.
	Name string
	// BaseURL is normalised (NormalizeBaseURL).
	BaseURL string
	Model   string
	// CredentialID is the sealed key's row. uuid.Nil means the endpoint takes no key —
	// a self-hosted model server on the cluster network — and none is sent.
	CredentialID uuid.UUID
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Identity is what an Investigator version built on this config pins.
func (c ProviderConfig) Identity() ModelIdentity {
	return ModelIdentity{Endpoint: c.BaseURL, Model: c.Model}
}

// HasKey reports whether a sealed key is stored for this endpoint.
func (c ProviderConfig) HasKey() bool { return c.CredentialID != uuid.Nil }

// ProviderDraft is what an operator submits to create a model endpoint.
//
// ⛔ APIKey IS PLAINTEXT. It lives for the duration of one create call, is sealed
// before the row is written, and is never read back. String, GoString and LogValue all
// redact it, so neither `%v` in a panic message nor a structured log line can leak it.
type ProviderDraft struct {
	Name    string
	BaseURL string
	Model   string
	APIKey  string
}

// String renders the draft with the key redacted.
func (d ProviderDraft) String() string {
	key := "none"
	if d.APIKey != "" {
		key = "[redacted]"
	}
	return "ProviderDraft{name=" + d.Name + " base_url=" + d.BaseURL + " model=" + d.Model + " api_key=" + key + "}"
}

// GoString redacts too, so `%#v` is as safe as `%v`.
func (d ProviderDraft) GoString() string { return d.String() }

// LogValue redacts for slog.
func (d ProviderDraft) LogValue() slog.Value { return slog.StringValue(d.String()) }

// Normalize validates a draft and returns it with its base URL normalised and its text
// trimmed. The key is checked for length and kept as given — whitespace inside a key
// is the key's business, around it is a paste accident.
func (d ProviderDraft) Normalize() (ProviderDraft, error) {
	out := ProviderDraft{
		Name:   strings.TrimSpace(d.Name),
		Model:  strings.TrimSpace(d.Model),
		APIKey: strings.TrimSpace(d.APIKey),
	}
	if n := utf8.RuneCountInString(out.Name); n == 0 || n > MaxProviderNameLength {
		return ProviderDraft{}, errs.Validation("model_provider_name_invalid",
			"a model endpoint's name is 1 to 120 characters",
			errs.Violation{Field: "name", Code: "length", Message: "1 to 120 characters"})
	}
	if n := utf8.RuneCountInString(out.Model); n == 0 || n > MaxModelNameLength {
		return ProviderDraft{}, errs.Validation("model_name_invalid",
			"a model name is 1 to 200 characters",
			errs.Violation{Field: "model", Code: "length", Message: "1 to 200 characters"})
	}
	if len(out.APIKey) > MaxAPIKeyLength {
		return ProviderDraft{}, errs.Validation("model_api_key_invalid",
			"an API key is at most 4096 bytes",
			errs.Violation{Field: "api_key", Code: "length", Message: "at most 4096 bytes"})
	}
	base, err := NormalizeBaseURL(d.BaseURL)
	if err != nil {
		return ProviderDraft{}, err
	}
	if err := KeyNeedsHTTPS(base, out.APIKey != ""); err != nil {
		return ProviderDraft{}, err
	}
	out.BaseURL = base
	return out, nil
}

// KeyNeedsHTTPS refuses an API key bound for a plaintext endpoint.
//
// ⛔ A KEY TRAVELS ONLY OVER TLS. The SDK refuses an authenticated request over http
// to anything but loopback, so a keyed http endpoint would be accepted here and fail
// every run; refusing it at configuration time is the same rule said where a person
// can still act on it. A keyless http endpoint — a self-hosted model on the cluster
// network — stays legal: there is no secret on that wire to lose.
func KeyNeedsHTTPS(normalizedBaseURL string, hasKey bool) error {
	if hasKey && !strings.HasPrefix(normalizedBaseURL, "https://") {
		const msg = "an API key is only sent over https; use an https base URL or store no key"
		return errs.Validation("model_api_key_needs_https", msg,
			errs.Violation{Field: "base_url", Code: "https_required", Message: msg})
	}
	return nil
}
