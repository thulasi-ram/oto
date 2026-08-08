package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// THE ONE KEYRING FOR THE WHOLE SYSTEM (SPEC §D.8).
//
// `channel_credentials.sealed` is "AES-256-GCM ciphertext, key from the
// platform/secrets keyring", and `key_version` is "the keyring generation that
// sealed this blob, so rotation can decrypt old rows". Two tables already point
// at that one store — `channel_credentials` itself, and
// `alert_sources.auth_credential_id`, which reuses it rather than growing a
// second one — so there must be exactly one implementation and it must live
// below every domain.
//
// The mechanism here is NOT new. It was implemented, reviewed and documented in
// `internal/channels/repository/seal.go` while this package was empty, behind
// the `Sealer` / `Unsealer` ports that file declares. This is that
// implementation promoted verbatim to the layer it was always meant to occupy:
// same envelope (12-byte nonce ‖ ciphertext ‖ tag), same AAD binding, same
// versioned generations, same canonical plaintext. Nothing about the wire format
// changed, so a blob sealed by the channels-local copy opens here and vice
// versa.
//
// ⛔ `internal/platform/**` may not import a domain (depguard), which is exactly
// why this type declares no ports of its own and names no table. It satisfies
// the consumer-declared ports — `channels/repository.Sealer`,
// `channels/repository.Unsealer`, `sources/repository.Unsealer` and
// `notification/service.CredentialUnsealer` — by having the same method sets,
// and `internal/app` is the one place the two ever meet.

// KeyBytes is the AES-256 key length. Anything else is refused at construction:
// a short key is not "weaker encryption", it is a boot that must not happen.
const KeyBytes = 32

// NonceBytes is the GCM standard nonce size. It is prefixed to every ciphertext,
// which is what makes `channel_credentials_seal_ck` (>= 29 bytes: 12 nonce + 16
// tag + at least one byte of plaintext) the right lower bound.
const NonceBytes = 12

// FirstGeneration is the key version a single-key deployment seals with.
// `channel_credentials_ver_ck` requires key_version >= 1.
const FirstGeneration = 1

// Keyring seals and unseals with AES-256-GCM over a versioned set of keys.
//
// Several generations may be present at once: Current seals, and any generation
// may unseal. That is the whole point of `channel_credentials.key_version` — a
// key rotation must not require rewriting every row in one transaction.
type Keyring struct {
	aeads   map[int]cipher.AEAD
	current int
}

// NewKeyring builds a keyring from raw 32-byte keys, keyed by generation.
// `current` names the generation new secrets are sealed with.
func NewKeyring(keys map[int][]byte, current int) (*Keyring, error) {
	if len(keys) == 0 {
		return nil, errs.New(errs.KindInternal, "secrets_no_key",
			"a credential keyring requires at least one key")
	}
	if _, ok := keys[current]; !ok {
		return nil, errs.Newf(errs.KindInternal, "secrets_no_current_key",
			"the keyring has no key for the current generation %d", current)
	}
	if current < FirstGeneration {
		return nil, errs.New(errs.KindInternal, "secrets_bad_generation",
			"a key generation is 1 or greater")
	}

	aeads := make(map[int]cipher.AEAD, len(keys))
	for version, key := range keys {
		if len(key) != KeyBytes {
			return nil, errs.Newf(errs.KindInternal, "secrets_bad_key_length",
				"key generation %d is %d bytes; AES-256-GCM requires %d", version, len(key), KeyBytes)
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, errs.Internal("secrets_cipher_failed", err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, errs.Internal("secrets_gcm_failed", err)
		}
		aeads[version] = aead
	}
	return &Keyring{aeads: aeads, current: current}, nil
}

// NewKeyringFromBase64 builds a single-generation keyring from the base64 key in
// `config.Security.SecretKey`. Both padded and raw base64 are accepted, because
// an operator pasting a key out of a secret manager should not have to know which
// alphabet it was written with.
func NewKeyringFromBase64(encoded string) (*Keyring, error) {
	key, err := DecodeKey(encoded)
	if err != nil {
		return nil, err
	}
	return NewKeyring(map[int][]byte{FirstGeneration: key}, FirstGeneration)
}

// DecodeKey decodes a base64 key in any of the four standard alphabets.
func DecodeKey(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, errs.New(errs.KindInternal, "secrets_key_missing",
			"security.secret_key is required to seal credentials")
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if raw, err := enc.DecodeString(encoded); err == nil {
			return raw, nil
		}
	}
	return nil, errs.New(errs.KindInternal, "secrets_key_malformed",
		"security.secret_key is not valid base64")
}

// CurrentVersion is the generation Seal writes.
func (k *Keyring) CurrentVersion() int { return k.current }

// Seal encrypts the credential values.
//
// `kind` is bound in as ADDITIONAL AUTHENTICATED DATA rather than merely stored
// alongside. A ciphertext sealed as a `slack_signing_secret` therefore cannot be
// moved onto a row claiming to be a `slack_bot_token`: the open fails instead of
// silently handing the wrong secret to the wrong provider.
func (k *Keyring) Seal(_ context.Context, kind string, values map[string]string) ([]byte, int, error) {
	if len(values) == 0 {
		return nil, 0, errs.New(errs.KindValidation, "credential_empty",
			"a credential must carry at least one value")
	}
	// Marshal a sorted, canonical object so the same secret seals to the same
	// plaintext bytes on every run — which keeps a rotation diff readable.
	plaintext, err := marshalCanonical(values)
	if err != nil {
		return nil, 0, err
	}

	aead := k.aeads[k.current]
	nonce := make([]byte, NonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return nil, 0, errs.Internal("secrets_nonce_failed", err)
	}
	sealed := aead.Seal(nonce, nonce, plaintext, []byte(kind))
	return sealed, k.current, nil
}

// Unseal decrypts a stored blob.
//
// ⛔ The returned map is a SECRET. It is never logged, never rendered into an
// errs.Message and never persisted in this shape. Every failure below returns a
// message that names the failure and nothing about the material.
func (k *Keyring) Unseal(_ context.Context, kind string, sealed []byte, keyVersion int) (map[string]string, error) {
	if len(sealed) <= NonceBytes {
		return nil, errs.New(errs.KindInternal, "credential_truncated",
			"the stored credential is too short to be a sealed value")
	}
	aead, ok := k.aeads[keyVersion]
	if !ok {
		return nil, errs.Newf(errs.KindInternal, "credential_unknown_key_version",
			"this deployment has no key for generation %d", keyVersion)
	}

	nonce, body := sealed[:NonceBytes], sealed[NonceBytes:]
	plaintext, err := aead.Open(nil, nonce, body, []byte(kind))
	if err != nil {
		return nil, errs.Wrap(errors.New("credential could not be opened"), errs.KindInternal,
			"credential_unseal_failed", "the stored credential could not be decrypted")
	}

	out := map[string]string{}
	if err := json.Unmarshal(plaintext, &out); err != nil {
		return nil, errs.Internal("credential_decode_failed", err)
	}
	return out, nil
}

// marshalCanonical renders a string map with its keys in sorted order.
func marshalCanonical(values map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	ordered := make([]byte, 0, 64)
	ordered = append(ordered, '{')
	for i, k := range keys {
		if i > 0 {
			ordered = append(ordered, ',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, errs.Internal("credential_encode_failed", err)
		}
		vb, err := json.Marshal(values[k])
		if err != nil {
			return nil, errs.Internal("credential_encode_failed", err)
		}
		ordered = append(ordered, kb...)
		ordered = append(ordered, ':')
		ordered = append(ordered, vb...)
	}
	ordered = append(ordered, '}')
	return ordered, nil
}
