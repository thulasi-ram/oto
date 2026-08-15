// Package arch holds the repo's structural gates: the module-direction gate on
// CONTEXT.md §4, which reads the real import graph and fails on any cross-module
// edge §4 does not draw; the table-ownership gate, which reads SQL string
// literals and fails on any table `internal/drill` names that §4's mechanism 4
// does not declare; the event-type gate, which refuses a second Go spelling of
// an `alerts/domain.EventType` value; and the per-tenant periodic gate, which
// reads the job seam and fails any scheduled kind that neither embeds
// jobs.TenantFanOut nor argues its exemption.
package arch
