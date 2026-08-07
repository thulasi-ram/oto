package decode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/thulasiram/oto/internal/ingestion/domain"
)

// ErrTooDeep is returned when a body exceeds bound B16.
var ErrTooDeep = errors.New("decode: json nesting exceeds the B16 depth limit")

// ErrNotEnvelope is returned when a body is JSON but not a webhook envelope.
var ErrNotEnvelope = errors.New("decode: body is not an alertmanager webhook envelope")

// Decode turns a raw webhook body into an Envelope, leniently and with a hard
// nesting bound (B16).
//
// The depth check runs BEFORE unmarshalling rather than after, because the point
// of B16 is to bound the work, not to describe it: `encoding/json` will happily
// recurse through a hundred thousand levels of `[[[[…]]]]` and exhaust the
// goroutine stack before any post-hoc check could run. Scanning tokens costs one
// pass and no allocation per level.
//
// A body that is well-formed JSON but not the expected envelope — the shape a
// custom `payload:` template produces — FAILS SOFT: the caller records
// `undecodable` and answers the batch, never silently discarding it.
func Decode(body []byte) (Envelope, error) {
	if err := CheckDepth(body, domain.MaxJSONDepth); err != nil {
		return Envelope{}, err
	}

	var env Envelope
	dec := json.NewDecoder(bytes.NewReader(body))
	// NO DisallowUnknownFields. See the Envelope doc comment: rejecting an unknown
	// field here would delete alerts on the next Alertmanager release (§L.3.1).
	if err := dec.Decode(&env); err != nil {
		if errors.Is(err, io.EOF) {
			return Envelope{}, fmt.Errorf("%w: empty body", ErrNotEnvelope)
		}
		return Envelope{}, fmt.Errorf("%w: %s", ErrNotEnvelope, err.Error())
	}
	return env, nil
}

// CheckDepth enforces bound B16 by scanning tokens and tracking bracket depth.
//
// It also rejects a body that is not a single JSON object: trailing content and
// a bare scalar are the same fact as excessive nesting — these bytes are not a
// webhook envelope, and no retry of the same bytes ever will be.
func CheckDepth(body []byte, maxDepth int) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	depth, maxSeen, topLevel := 0, 0, 0

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("decode: malformed json: %w", err)
		}

		delim, ok := tok.(json.Delim)
		if !ok {
			if depth == 0 {
				topLevel++ // a bare scalar at the top level
			}
			continue
		}
		switch delim {
		case '{', '[':
			if depth == 0 {
				topLevel++
			}
			depth++
			if depth > maxSeen {
				maxSeen = depth
			}
			if depth > maxDepth {
				return fmt.Errorf("%w: depth %d exceeds %d", ErrTooDeep, depth, maxDepth)
			}
		case '}', ']':
			depth--
		}
	}

	if maxSeen == 0 || topLevel != 1 {
		return fmt.Errorf("%w: expected exactly one top-level json object", ErrNotEnvelope)
	}
	return nil
}
