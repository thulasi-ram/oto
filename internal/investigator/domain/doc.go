// Package domain holds the pure investigator entities and invariants (ADR 0053): the
// ModelProvider port an Investigation talks to a model through, the turn it gets back,
// and the stored configuration of one model endpoint. It imports no I/O: no pgx, no
// net/http, and no model vendor's SDK — the one adapter that speaks a wire protocol
// lives in `investigator/models/openaicompat`, and no vendor's name appears here.
package domain
