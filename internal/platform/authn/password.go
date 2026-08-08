package authn

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// Argon2id parameters. They are the OWASP-recommended second option (19 MiB,
// t=2, p=1), chosen because oto self-hosts on modest hardware and a login must
// not become a denial-of-service amplifier: 19 MiB × the number of concurrent
// logins is the memory a login storm can pin.
//
// They are encoded INTO every hash, so raising them later re-hashes on next
// login rather than invalidating every stored password.
const (
	argonTime    uint32 = 2
	argonMemory  uint32 = 19 * 1024 // KiB
	argonThreads uint8  = 1
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// PasswordHashPrefix is the encoding oto stores, mirroring `users_pw_ck`.
const PasswordHashPrefix = "$argon2id$"

// MinPasswordBytes and MaxPasswordBytes bound a submitted password. The ceiling
// is not a strength limit — it is a cost limit: argon2 hashes whatever it is
// given, so an unbounded password is an unbounded amount of work per request.
const (
	MinPasswordBytes = 8
	MaxPasswordBytes = 1024
)

// PasswordHasher is the port a service takes so that a test can substitute a
// cheap hasher and not spend 19 MiB and two passes per case.
type PasswordHasher interface {
	// Hash returns the argon2id encoding of password.
	Hash(password string) (string, error)
	// Verify reports whether password matches encoded. It returns false rather
	// than an error for a mismatch; an error means the STORED hash is unusable,
	// which is an operator problem and not the caller's.
	Verify(encoded, password string) (bool, error)
	// DummyVerify spends the same work Verify would, for a login whose subject
	// does not exist. It is on the interface rather than only on the concrete
	// type so that a login path CANNOT be written that skips it.
	DummyVerify(password string)
}

// Argon2id is the production hasher.
type Argon2id struct{}

// NewPasswordHasher returns the production hasher.
func NewPasswordHasher() PasswordHasher { return Argon2id{} }

// ErrPasswordBounds is returned for a password outside the accepted length.
var ErrPasswordBounds = errors.New("authn: password length out of bounds")

// Hash computes the argon2id encoding of password.
//
// The salt is 16 bytes from crypto/rand, per password. A shared or derived salt
// would let one rainbow table cover every account in the deployment.
func (Argon2id) Hash(password string) (string, error) {
	if len(password) < MinPasswordBytes || len(password) > MaxPasswordBytes {
		return "", ErrPasswordBounds
	}

	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("authn: read salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return encodeHash(salt, key, argonTime, argonMemory, argonThreads), nil
}

// Verify reports whether password matches the stored encoding.
//
// The comparison is subtle.ConstantTimeCompare. Byte-wise early exit on a
// derived key is a weaker leak than on a raw secret, but it is a leak, and the
// constant-time call costs nothing next to the KDF that produced the operands.
func (Argon2id) Verify(encoded, password string) (bool, error) {
	salt, want, t, m, p, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	if len(password) > MaxPasswordBytes {
		// Bounded before hashing: an oversized password must not be allowed to
		// spend the KDF budget just because a row exists to compare it against.
		return false, nil
	}

	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// DummyVerify burns one KDF evaluation against a throwaway input.
//
// It exists so that a login for an ADDRESS THAT DOES NOT EXIST costs the same
// wall-clock time as a login for one that does. Without it, the endpoint answers
// "no such user" in microseconds and "wrong password" in tens of milliseconds,
// and that difference is a user enumeration oracle that no amount of careful
// wording in the 401 body can close.
func (Argon2id) DummyVerify(password string) {
	if len(password) > MaxPasswordBytes {
		password = password[:MaxPasswordBytes]
	}
	var salt [argonSaltLen]byte
	_ = argon2.IDKey([]byte(password), salt[:], argonTime, argonMemory, argonThreads, argonKeyLen)
}

// b64 is the unpadded standard alphabet the reference argon2 encoding uses.
var b64 = base64.RawStdEncoding

func encodeHash(salt, key []byte, t, m uint32, p uint8) string {
	return fmt.Sprintf("%sv=%d$m=%d,t=%d,p=%d$%s$%s",
		PasswordHashPrefix, argon2.Version, m, t, p,
		b64.EncodeToString(salt), b64.EncodeToString(key))
}

// errBadHash is returned when a stored hash cannot be parsed. It is
// KindInternal, not KindUnauthorized: an unparseable hash is oto's bug or a
// corrupted row, and answering "wrong password" would hide it forever.
func errBadHash(cause error) error {
	return errs.Wrap(cause, errs.KindInternal, "password_hash_unreadable",
		"an internal error occurred")
}

func decodeHash(encoded string) (salt, key []byte, t, m uint32, p uint8, err error) {
	// $argon2id$v=19$m=19456,t=2,p=1$<salt>$<key>
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return nil, nil, 0, 0, 0, errBadHash(errors.New("authn: malformed argon2id encoding"))
	}

	var version int
	if _, e := fmt.Sscanf(parts[2], "v=%d", &version); e != nil {
		return nil, nil, 0, 0, 0, errBadHash(e)
	}
	if version != argon2.Version {
		return nil, nil, 0, 0, 0, errBadHash(errors.New("authn: unsupported argon2 version " + strconv.Itoa(version)))
	}

	var (
		mem     uint32
		time32  uint32
		threads uint8
	)
	if _, e := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &time32, &threads); e != nil {
		return nil, nil, 0, 0, 0, errBadHash(e)
	}

	salt, e := b64.DecodeString(parts[4])
	if e != nil {
		return nil, nil, 0, 0, 0, errBadHash(e)
	}
	key, e = b64.DecodeString(parts[5])
	if e != nil || len(key) == 0 {
		return nil, nil, 0, 0, 0, errBadHash(errors.New("authn: unreadable argon2id key"))
	}

	return salt, key, time32, mem, threads, nil
}
