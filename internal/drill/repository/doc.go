// Package repository is the SQL behind delivery drills.
//
// ⚠️ IT READS SEVERAL MODULES' TABLES, AND THAT IS DELIBERATE. A staged result is
// a REPORT over artefacts other modules wrote — `ingest_batches`, `alerts`,
// `alert_cases`, `alert_groups`, `notifications`, `channel_threads`,
// `notification_deliveries` — in the same shape `stats/repository` reads five
// modules' tables to build one dashboard. The alternative, a port per table, would
// mean seven round trips through seven services to answer one screen, and every
// one of those services would need a read method that exists for nothing else.
//
// ⛔ IT WRITES EXACTLY ONE TABLE OF ITS OWN, `delivery_drills`, plus the disposal
// deletes — and disposal is the ONE place in oto that deletes a signal row, which
// is why it is confined to one function with the argument for it written above it.
// Everything a drill CAUSES is caused through a port some other module's service
// satisfies. A drill never writes an alert, never opens a group and never sends a
// message; it pushes a payload at the front door like everybody else.
package repository
