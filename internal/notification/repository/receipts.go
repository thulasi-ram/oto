package repository

import (
	"context"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/db"
)

// IncidentReceiptRepository is the SQL over `incident_outbound_mappings` (migration
// 00089): ADR 0052 §5's outbound mapping, written as the receipt of a delivery.
//
// ⭐ THIS MODULE WRITES IT AND NEVER READS IT. The dispatcher is the only code that
// sees a receiver's answer, so it is the only writer; the Incident's page and card
// read the receipts through `incidents`, which owns the Incident they hang off.
type IncidentReceiptRepository struct {
	q db.Querier
}

// NewIncidentReceiptRepository builds the repository over a fallback querier.
func NewIncidentReceiptRepository(q db.Querier) *IncidentReceiptRepository {
	return &IncidentReceiptRepository{q: q}
}

func (r *IncidentReceiptRepository) db(ctx context.Context) db.Querier {
	return db.FromContext(ctx, r.q)
}

// ⭐ ON CONFLICT DO NOTHING IS THE WHOLE IDEMPOTENCY ARGUMENT. oto's queue is
// at-least-once, so the same delivery can be answered twice, and every later fact
// on the same Incident is answered again by the same tool. The first valid answer
// per (Incident, channel) is the incident the tool opened; nothing after it is
// believed over it. The `incidents` row is matched on org_id as well, so a
// receipt can never be written against another tenant's Incident.
//
// ⛔ THIS STATEMENT MUST NOT FAIL ON ITS DATA, AND IS WRITTEN SO IT CANNOT. It runs
// in the dispatcher's TX 2, beside the row that says the message went out, and
// Postgres aborts a transaction on any error — so a CHECK violation here would roll
// back `sent` and the job would deliver the same message again. A receipt is a
// courtesy; it may never un-send a delivery. So every way the row could be refused
// is turned into "no row" instead: a deleted Incident selects nothing (rather than
// tripping its foreign key), and the WHERE restates the table's own CHECKs, so a
// value that slipped past the provider's validation is dropped, not raised.
const recordReceiptSQL = `
INSERT INTO incident_outbound_mappings
       (org_id, incident_id, channel_id, external_url, external_id, delivery_id, recorded_at)
SELECT i.org_id, i.id, $3::uuid, NULLIF($4::text, ''), NULLIF($5::text, ''), $6::uuid, $7::timestamptz
  FROM incidents i
 WHERE i.org_id = $1 AND i.id = $2
   AND ($4::text <> '' OR $5::text <> '')
   AND ($4::text = '' OR ($4::text LIKE 'https://%' AND length($4::text) <= 2048))
   AND length($5::text) <= 255
ON CONFLICT (incident_id, channel_id) DO NOTHING`

// Record keeps the receipt unless one already exists for this (Incident,
// channel), and reports whether this call wrote it.
func (r *IncidentReceiptRepository) Record(
	ctx context.Context, s db.TenantScope, in domain.IncidentReceipt,
) (bool, error) {
	if err := db.RequireScope(s); err != nil {
		return false, err
	}
	if in.ExternalURL == "" && in.ExternalID == "" {
		return false, nil
	}
	// A receipt with no delivery behind it stores NULL rather than the nil UUID,
	// which would trip the foreign key — and with it, TX 2 (see recordReceiptSQL).
	var deliveryID *uuid.UUID
	if in.DeliveryID != uuid.Nil {
		deliveryID = &in.DeliveryID
	}
	tag, err := r.db(ctx).Exec(ctx, recordReceiptSQL, s.OrgID(), in.IncidentID, in.ChannelID,
		in.ExternalURL, in.ExternalID, deliveryID, in.RecordedAt)
	if err != nil {
		return false, mapErr(err, "incident_not_found", "record an incident receipt")
	}
	return tag.RowsAffected() == 1, nil
}
