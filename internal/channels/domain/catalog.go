package domain

import "encoding/json"

// ADR 0055 §2: THE CATALOG OF PAYLOAD MAPPINGS (git-bug 2b5eecc).
//
// "A catalog of community mappings is a folder of files — contributed, reviewed and
// imported onto a Connection; no code ships with one." The folder is `mappings/` at
// the repository root, embedded in the binary; each file is one tool's mapping and
// what a reviewer needs to judge it.
//
// ⛔ IMPORT IS A COPY, NEVER A REFERENCE. A Connection that imports an entry stores
// the entry's Mapping as its own `payload_mapping`, and nothing on the Connection
// remembers where it came from, so a later catalog change cannot alter a live
// Connection. The copy is the operator's from then on, and so is any command they
// edit into it (§4).
//
// ⛔ AN ENTRY CARRIES NO SECRET. A vendor key in the body is `{{ secrets.<name> }}`
// and the operator seals the value on the Connection; Secrets lists the names an
// importer will need, and nothing else about them is in the file.

// CatalogMapping is one catalog file, read and shape-checked.
type CatalogMapping struct {
	// ID is the file's name without its extension: `pagerduty`, `incident-io`.
	ID string
	// Vendor is the tool, as its vendor writes it.
	Vendor string
	// Title names the API the mapping speaks to.
	Title string
	// Summary says what the mapping does with oto's facts.
	Summary string
	// Docs are the vendor's public docs the field names were checked against.
	Docs []string
	// CheckedOn is when they were checked, `YYYY-MM-DD`.
	CheckedOn string
	// Setup is what the operator does in the tool and on the Connection, in order.
	Setup []string
	// Commands are the tool's command fields and the values of them that would turn
	// a fact into a resolve, close, acknowledge or status change. The catalog check
	// refuses an entry that declares none, and fails one that renders any.
	Commands []CatalogCommand
	// SecretFields are body paths that carry a vendor key; each must render as a
	// `{{ secrets.<name> }}` reference and nothing else.
	SecretFields []string
	// Secrets is every secret name the mapping references, sorted — what the
	// operator must seal on the Connection before the import can be saved.
	Secrets []string
	// Mapping is the payload mapping document, exactly as a Connection stores it.
	Mapping json.RawMessage
}

// CatalogCommand is one command field of a tool's request body.
type CatalogCommand struct {
	// Field is a gjson path into the rendered body: `event_action`, `status`.
	Field string
	// Forbidden are the values of Field that would make a fact a command.
	Forbidden []string
}
