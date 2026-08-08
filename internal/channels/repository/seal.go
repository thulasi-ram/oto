package repository

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

// ⛔ WHY THIS FILE EXISTS, AND WHERE IT SHOULD EVENTUALLY LIVE.
//
// SPEC §D.8 is explicit about the mechanism: `channel_credentials.sealed` is
// "AES-256-GCM ciphertext, key from the platform/secrets keyring", and
// `key_version` is "the keyring generation that sealed this blob, so rotation
// can decrypt old rows". `config.SecurityConfig.SecretKey` is already declared as
// "the base64 32-byte AES-256-GCM key used by platform/secrets".
//
// `internal/platform/secrets` currently contains a doc comment and NOTHING ELSE.
// A repository that stored a bot token in plaintext because the platform package
// was empty would be a security defect shipped for an ordering reason, so the
// specified mechanism is implemented here, in the one layer that already owns the
// ciphertext column. It is deliberately a self-contained envelope-encryption
// keyring with no dependency on anything in this module: when
// `platform/secrets` lands, this file is deleted and every consumer keeps
// compiling, because every consumer depends on the INTERFACE below and not on
// this type.

// Sealer seals plaintext credential values for storage.
//
// It is declared here, by the consumer, so that `platform/secrets` can satisfy it
// later without this package changing.
type Sealer interface {
	// Seal returns the ciphertext and the keyring generation that produced it.
	Seal(ctx context.Context, kind string, values map[string]string) ([]byte, int, error)
}

// Unsealer recovers plaintext credential values.
//
// The method set is byte-identical to `notification/service.CredentialUnsealer`
// so that one concrete satisfies both without an adapter.
type Unsealer interface {
	Unseal(ctx context.Context, kind string, sealed []byte, keyVersion int) (map[string]string, error)
}

// KeyBytes is the AES-256 key length. Anything else is refused at construction:
// a short key is not "weaker encryption", it is a boot that must not happen.
const KeyBytes = 32

// nonceBytes is the GCM standard nonce size. It is prefixed to every ciphertext,
// which is what makes `channel_credentials_seal_ck` (>= 29 bytes: 12 nonce + 16
// tag + at least one byte of plaintext) the right lower bound.
const nonceBytes = 12

// Keyring seals and unseals with AES-256-GCM over a versioned set of keys.
//
// Several generations may be present at once: `Current` seals, and any generation
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
	if current < 1 {
		// channel_credentials_ver_ck: key_version >= 1.
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
	key, err := decodeKey(encoded)
	if err != nil {
		return nil, err
	}
	return NewKeyring(map[int][]byte{1: key}, 1)
}

func decodeKey(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, errs.New(errs.KindInternal, "secrets_key_missing",
			"security.secret_key is required to seal channel credentials")
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
	nonce := make([]byte, nonceBytes)
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
	if len(sealed) <= nonceBytes {
		return nil, errs.New(errs.KindInternal, "credential_truncated",
			"the stored credential is too short to be a sealed value")
	}
	aead, ok := k.aeads[keyVersion]
	if !ok {
		return nil, errs.Newf(errs.KindInternal, "credential_unknown_key_version",
			"this deployment has no key for generation %d", keyVersion)
	}

	nonce, body := sealed[:nonceBytes], sealed[nonceBytes:]
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

// Compile-time proof that one Keyring satisfies both halves.
var (
	_ Sealer   = (*Keyring)(nil)
	_ Unsealer = (*Keyring)(nil)
)
