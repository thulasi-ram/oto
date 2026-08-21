package webhook

import (
	_ "embed"

	"github.com/thulasiram/oto/internal/channels/configschema"
)

//go:embed connection_schema.json
var connectionSchemaBytes []byte

// ConnectionSchema is a webhook connection's configuration — currently empty.
//
// A webhook connection exists to hold a SHARED CREDENTIAL — basic/bearer auth
// to reach a receiver, or a signing secret so the receiver can verify a
// payload came from oto (see CredSigningSecret in provider.go) — not to carry
// settings of its own. The schema is still published and compiled, empty as it
// is, so the settings UI renders a connection-creation form the same
// schema-driven way for every provider rather than special-casing the one with
// nothing to configure.
var ConnectionSchema = configschema.MustCompile(
	"https://oto.dev/schemas/channel-connection/webhook/v1.json", connectionSchemaBytes)
