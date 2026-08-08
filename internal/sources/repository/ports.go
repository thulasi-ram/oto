package repository

import (
	"github.com/thulasiram/oto/internal/sources/service"
)

// Compile-time proof that this package satisfies the ports `sources/service`
// declares for itself (SPEC §F.5). The service says what it needs; this is the
// Postgres concrete; `internal/app/container.go` injects it.
//
// These assertions are the whole reason the file exists. Without them a signature
// drift in the port surfaces as a nil interface at wiring time in `internal/app`,
// which is both far from the cause and, in a composition root, a runtime panic
// rather than a compile error.
var (
	_ service.SourceRepository = (*SourceRepository)(nil)
	_ service.CredentialStore  = (*CredentialStore)(nil)
)
