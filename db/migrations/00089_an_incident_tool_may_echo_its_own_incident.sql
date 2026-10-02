-- ADR 0052 §5: THE OUTBOUND MAPPING, AS A RECEIPT (git-bug 506ff21, its governing comment).
-- An Incident fact delivered to an incident tool's receiver may get back, in a 2xx JSON
-- response, the tool's own `external_url` and/or `external_id` for the incident it opened
-- or updated. oto records them ONCE per (Incident, channel) and shows the link on the
-- Incident's Slack card and its /incidents page. This table is that record: the binding
-- 0052 §5 describes as `(destination, external incident id)`, "the way a ChannelThread
-- binds a Conversation to a Slack root".
--
-- ⭐⭐ IT IS THE RECEIPT OF A DELIVERY, NOT THE INCIDENT'S STATE. Nothing else in the
-- response is read, nothing here is ever re-read from the tool, and nothing oto decides
-- depends on it — outbound-only (0052 §5, "Nothing is read back") still holds. It is the
-- same kind of fact as the Slack `ts` a delivery records: where the message landed.
--
-- ⭐ ONCE, AND THE FIRST ONE WINS. The primary key is (incident_id, channel_id) and the
-- writer inserts ON CONFLICT DO NOTHING, so a retried delivery — oto's queue is
-- at-least-once — and every later fact on the same Incident record nothing new. A tool
-- that answered a later fact with a DIFFERENT url is not believed over its first answer:
-- the first is the incident it opened, and an Incident's link moving under a reader's
-- cursor would be worse than one stale link.
--
-- ⛔ THE TWO VALUES ARE THE ONLY RECEIVER BYTES OTO KEEPS, AND THEY ARE BOUNDED TWICE.
-- `notification_deliveries.provider_response` still carries no receiver byte (the SSRF
-- read-primitive argument in the webhook provider's recordResponse). These two are
-- validated before they reach the writer — an absolute https URL, a printable id — and
-- the CHECKs below restate the bounds so a writer that skipped the validation still
-- cannot store an unbounded or non-https string.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). One new table that release N neither reads nor
-- writes.

-- +goose Up

CREATE TABLE incident_outbound_mappings (
  org_id       UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  incident_id  UUID        NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
  -- A channel is soft-deleted in practice; a hard delete takes its receipts with it,
  -- because a link into a destination oto no longer has is a link nobody configured.
  channel_id   UUID        NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  external_url TEXT,
  external_id  TEXT,
  -- The delivery whose response carried the echo. SET NULL, not CASCADE: a drill's
  -- disposal or a rollback that deletes the delivery does not unmake the incident the
  -- tool opened.
  delivery_id  UUID        REFERENCES notification_deliveries(id) ON DELETE SET NULL,
  recorded_at  TIMESTAMPTZ NOT NULL,
  CONSTRAINT incident_outbound_mappings_pkey PRIMARY KEY (incident_id, channel_id),
  CONSTRAINT incident_outbound_mappings_some_ck
    CHECK (external_url IS NOT NULL OR external_id IS NOT NULL),
  CONSTRAINT incident_outbound_mappings_url_ck
    CHECK (external_url IS NULL OR (external_url LIKE 'https://%' AND length(external_url) <= 2048)),
  CONSTRAINT incident_outbound_mappings_id_ck
    CHECK (external_id IS NULL OR length(external_id) BETWEEN 1 AND 255)
);

-- Serves: the Incident page and card, which read every receipt for one Incident in one
-- org. The primary key leads with incident_id already; this adds the tenant predicate
-- every statement carries.
CREATE INDEX incident_outbound_mappings_org_idx ON incident_outbound_mappings (org_id, incident_id);

-- +goose StatementBegin
COMMENT ON TABLE incident_outbound_mappings IS
  'ADR 0052 §5''s outbound mapping, recorded as a delivery receipt (git-bug 506ff21): the external incident an incident tool echoed back (external_url / external_id in a 2xx JSON response) for an Incident fact delivered on one channel. Once per (Incident, channel), first answer wins, never overwritten, never re-read from the tool. Not the Incident''s state: nothing oto decides reads it.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN incident_outbound_mappings.external_url IS
  'The tool''s own link to its incident: an absolute https URL of at most 2048 characters, validated by the webhook provider before it is kept. Shown on the Incident''s Slack card and page. NULL when the tool echoed only an id.';
-- +goose StatementEnd

-- +goose Down

DROP TABLE incident_outbound_mappings;
