// Package domain is the pure core of a delivery drill: the stage list, the
// verdict function over it, and the synthetic Alertmanager payload.
//
// It has NO I/O imports. The payload builder is a pure function of (drill id,
// cluster key, severity, clock), which is what lets a golden test pin the exact
// bytes oto pushes into its own ingest endpoint.
package domain
