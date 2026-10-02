// Package mappings embeds oto's catalog of payload mappings (ADR 0055 §2), so a
// single oto binary can list them in Settings → Connections without a sidecar
// directory.
//
// ⛔ IT IS A FOLDER OF DATA AND NOTHING ELSE. "A catalog of community mappings is a
// folder of files — contributed, reviewed and imported onto a Connection; no code
// ships with one." This file is the only Go here, and all it does is embed the
// YAML beside it. Reading and checking the files is
// `internal/channels/service/catalog.go`, and the check every file must pass —
// that no catalog mapping turns a fact into a resolve, close or status change
// (§4) — is `internal/channels/service/catalog_test.go`.
package mappings

import "embed"

// FS holds every catalog file in this directory.
//
//go:embed *.yaml
var FS embed.FS
