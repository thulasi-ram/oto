// Package openapi embeds the published HTTP contract so that a single oto binary
// can serve `GET /openapi.json` (SPEC §E.2) without a sidecar file.
//
// SPEC §I.2 fixes this location: `api/openapi/openapi.yaml`, hand-maintained
// (C20). The TypeScript client and the contract test both read the same bytes,
// which is what stops the published contract and the served one from drifting.
package openapi

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// YAML is the contract exactly as it is checked in.
//
//go:embed openapi.yaml
var YAML []byte

// JSON returns the contract as OpenAPI 3.1 JSON, which is what `/openapi.json`
// serves. It is computed once at boot rather than per request: the document is
// 320 kB and re-encoding it on every poll of a docs page is pure waste.
func JSON() ([]byte, error) {
	var doc any
	if err := yaml.Unmarshal(YAML, &doc); err != nil {
		return nil, fmt.Errorf("openapi: parse contract: %w", err)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("openapi: encode contract: %w", err)
	}
	return b, nil
}
