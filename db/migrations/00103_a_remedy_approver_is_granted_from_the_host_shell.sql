-- ADR 0054 §4: A REMEDY APPROVER IS GRANTED FROM THE HOST SHELL, PER TOOLSERVER, AND
-- NOTHING OVER HTTP CAN MINT ONE (git-bug 47f67c8). "A user holding the approval grant on
-- that Remedy's ToolServer" — the first and only piece of the deferred `authz` module,
-- and a prerequisite of every Remedy.
--
-- ⭐⭐ A ROW HERE IS WRITTEN BY `oto grant remedy-approver` AND DELETED BY `oto revoke
-- remedy-approver`, AND BY NOTHING ELSE. Like `oto bootstrap` and `oto reset-password`,
-- running either needs a shell on the host and the database credentials — the authority
-- that could write this row by hand anyway — and that is the answer to "who grants the
-- first grant". There is no in-app grant: double approval means two DIFFERENT holders
-- (§4), and a route that let one holder mint a second (an alt account) would defeat it.
-- `granted_by` is therefore pinned to `cli` by its CHECK: the column says who wrote the
-- row, and the CHECK says that nothing else may.
--
-- ⭐ ONLY A `write` TOOLSERVER CAN CARRY A GRANT. A Remedy executes through a write Tool
-- (ADR 0054 §5), and a `read` ToolServer's Tools are never a Remedy's, so a grant on one
-- would be a permission over nothing. The CLI refuses it with a sentence; the schema
-- refuses it too: the row carries `tool_server_access`, pinned to 'write', and the
-- composite foreign key names (org_id, id, access) on `tool_servers` — so a grant can
-- only point at a write ToolServer of its own org, and that ToolServer cannot be
-- re-declared `read` while a grant points at it.
--
-- ⭐ THE USER IS RESOLVED BY EMAIL, so a shadow member (no address, 00074) can never be
-- named. A DISABLED user's grant stays on the record and stops counting: the read that
-- decides an approval joins `users` and requires `disabled_at IS NULL` and an address.
-- Revoking deletes the row — a revoked grant is no grant.
--
-- ⭐ DELETING THE TOOLSERVER DELETES ITS GRANTS (ON DELETE CASCADE): a permission over a
-- ToolServer that no longer exists is a permission over nothing, and a later ToolServer
-- given the same name is a different server.
--
-- ⛔ IT IS A PERMISSION, NEVER AN OBLIGATION (H-1). Nothing here routes a Remedy to a
-- holder, notifies one, or makes a queue of what they have not approved.
--
-- ⛔ NO DEFAULT now() (CONTEXT.md §6): `granted_at` is the CLI's clock.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). One new table release N-1 never reads, and two unique
-- indexes over existing primary keys (so no existing row can violate either).

-- +goose Up

-- The targets of the two composite foreign keys below. Both are a primary key widened by
-- the org (and, for a ToolServer, its declared access), so neither can be violated by a
-- row that already exists.
CREATE UNIQUE INDEX tool_servers_org_id_access_uniq ON tool_servers (org_id, id, access);
CREATE UNIQUE INDEX users_org_id_uniq ON users (org_id, id);

CREATE TABLE remedy_approver_grants (
  org_id             UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  tool_server_id     UUID        NOT NULL,
  -- ⭐ Always 'write': the third column of the foreign key that keeps a grant off a read
  -- ToolServer.
  tool_server_access TEXT        NOT NULL,
  user_id            UUID        NOT NULL,
  granted_at         TIMESTAMPTZ NOT NULL,
  granted_by         TEXT        NOT NULL,
  CONSTRAINT remedy_approver_grants_pk        PRIMARY KEY (tool_server_id, user_id),
  CONSTRAINT remedy_approver_grants_access_ck CHECK (tool_server_access = 'write'),
  CONSTRAINT remedy_approver_grants_by_ck     CHECK (granted_by = 'cli'),
  CONSTRAINT remedy_approver_grants_tool_server_fk
    FOREIGN KEY (org_id, tool_server_id, tool_server_access) REFERENCES tool_servers (org_id, id, access)
    ON DELETE CASCADE,
  CONSTRAINT remedy_approver_grants_user_fk
    FOREIGN KEY (org_id, user_id) REFERENCES users (org_id, id) ON DELETE CASCADE
);

-- Serves: the user side of the foreign key (a user's grants, when one goes).
CREATE INDEX remedy_approver_grants_user_idx ON remedy_approver_grants (org_id, user_id);

-- +goose StatementBegin
COMMENT ON TABLE remedy_approver_grants IS
  'ADR 0054 §4 (00103, git-bug 47f67c8): who may approve a Remedy on a write ToolServer. Written ONLY by `oto grant remedy-approver` and deleted ONLY by `oto revoke remedy-approver`, from the host shell — no HTTP route writes it, so one holder can never mint a second approver and defeat double approval. A disabled user''s grant does not count; a shadow member can never be named. A permission, never an obligation: it routes nothing and creates no queue.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN remedy_approver_grants.granted_by IS
  'Who wrote the row: always cli (remedy_approver_grants_by_ck). There is no other writer.';
-- +goose StatementEnd

-- +goose Down

-- The grants go with the table. Below this migration nothing reads them and no Remedy
-- exists to approve.
DROP TABLE remedy_approver_grants;
DROP INDEX users_org_id_uniq;
DROP INDEX tool_servers_org_id_access_uniq;
