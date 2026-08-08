-- +goose Up
-- +goose StatementBegin

-- Give the root amend its own delivery row.
--
-- deliveries_fanout_uniq was (notification_id, channel_id), which allows exactly
-- one delivery per fact per destination. But section H.6 requires BOTH an
-- update_root AND a thread_reply for the same notification, so the root amend
-- had nowhere to live: it rode along inside the reply's claim, writing no row.
--
-- The consequences were both real and silent. A failed amend vanished — no row,
-- so no retry, no dead-letter, nothing on the timeline, and the Slack card stayed
-- stale forever. A SUCCESSFUL amend was worse: LastRootHash reads only `sent`
-- deliveries, so the cached hash stayed pre-amend, and a later genuine
-- update_root rendering those same bytes was abandoned as `duplicate_render`,
-- leaving the card permanently wrong.
--
-- Adding `mode` to the key lets each mode carry its own row, so an amend is
-- claimed, retried, classified and hashed like any other delivery.

ALTER TABLE notification_deliveries
    DROP CONSTRAINT deliveries_fanout_uniq;

ALTER TABLE notification_deliveries
    ADD CONSTRAINT deliveries_fanout_uniq
    UNIQUE (notification_id, channel_id, mode);

COMMENT ON CONSTRAINT deliveries_fanout_uniq ON notification_deliveries IS
    'One delivery per (fact, destination, mode). Mode is in the key because section H.6 requires update_root and thread_reply for the same notification.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE notification_deliveries
    DROP CONSTRAINT deliveries_fanout_uniq;

ALTER TABLE notification_deliveries
    ADD CONSTRAINT deliveries_fanout_uniq
    UNIQUE (notification_id, channel_id);

-- +goose StatementEnd
