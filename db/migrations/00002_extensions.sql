-- SPEC §D.0 -- Extensions and helpers.
--
-- `pgcrypto` is created in 00001. `citext` is the second half of §D.0 and is
-- required by the very first table in §D.1: orgs.slug, users.email,
-- alert_sources.name, channels.name and notification_policies.name are all
-- CITEXT so that case-insensitive uniqueness is a property of the column rather
-- than a lower() call every implementer has to remember.
--
-- It lands in its own migration rather than being folded into 00001 because
-- 00001 may already be applied in a running database and goose checksums a file
-- by its contents. Expand-only, always.

-- +goose Up
CREATE EXTENSION IF NOT EXISTS citext;

-- +goose Down
-- Deliberately NOT dropped, for the same reason 00001 does not drop pgcrypto:
-- dropping an extension is destructive and other objects in the cluster may
-- depend on it. Rolling this migration back is a no-op by design.
SELECT 1;
