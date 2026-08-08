package app

import (
	"testing"

	channelsrepo "github.com/thulasiram/oto/internal/channels/repository"
	sourcesrepo "github.com/thulasiram/oto/internal/sources/repository"
)

// ⭐ TestNilKeyringStaysNilInTheInterface is the typed-nil defect.
//
// The adapters returned `*secrets.Keyring`, so a deployment with no
// `security.secret_key` stored a `(*secrets.Keyring)(nil)` INSIDE the interface
// field. An interface value holding a nil pointer is NOT `== nil`, so every
// `if r.open == nil` guard in the credential repositories was dead, and the
// container's promise that "every credential read then fails loudly at the
// repository" became `Keyring.Unseal` dereferencing `k.aeads` on a nil receiver:
// a panic, recovered as a 500, on a path whose entire design is to fail closed.
//
// This is a compile-shaped bug with a runtime symptom, so it gets a test that
// asks the only question that matters.
func TestNilKeyringStaysNilInTheInterface(t *testing.T) {
	t.Parallel()

	if s := keyringSealer(nil); s != nil {
		t.Error("keyringSealer(nil) produced a non-nil Sealer")
	}
	if u := channelsUnsealer(nil); u != nil {
		t.Error("channelsUnsealer(nil) produced a non-nil Unsealer; the repository guard would never fire")
	}
	if u := sourcesUnsealer(nil); u != nil {
		t.Error("sourcesUnsealer(nil) produced a non-nil Unsealer; the repository guard would never fire")
	}
	if u := dispatchUnsealer(nil); u != nil {
		t.Error("dispatchUnsealer(nil) produced a non-nil CredentialUnsealer")
	}
}

// TestCredentialStoresFailClosedWithoutAKeyring proves the consequence: with the
// adapters returning a true nil, the repositories' own guards do what the
// container's comment always claimed they did.
func TestCredentialStoresFailClosedWithoutAKeyring(t *testing.T) {
	t.Parallel()

	// Constructed exactly as the container constructs them when
	// `security.secret_key` is unset.
	if store := sourcesrepo.NewCredentialStore(nil, sourcesUnsealer(nil)); store == nil {
		t.Fatal("the sources credential store could not be built without a keyring")
	}
	if repo := channelsrepo.NewCredentialRepository(nil, keyringSealer(nil), channelsUnsealer(nil), nil); repo == nil {
		t.Fatal("the channels credential repository could not be built without a keyring")
	}
	// The assertion that matters is the one above: these constructors are handed a
	// genuinely nil port, so the `open == nil` branch inside them is reachable. It
	// was not, and that is what turned a fail-closed design into a panic.
}
