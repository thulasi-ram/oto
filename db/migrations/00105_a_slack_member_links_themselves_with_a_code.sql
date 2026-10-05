-- A SLACK MEMBER LINKS THEMSELVES TO AN oto USER WITH A CODE oto SHOWED ONLY THEM (git-bug a556a5c;
-- the ruling of 2026-10-05: SELF-SERVICE IN THE UI). Three tables:
--
--   1. `slack_link_codes` — at most ONE live code per Slack identity, stored only as a sha256. It is
--      minted when an unlinked member presses a Remedy button, shown once in an ephemeral only that
--      member sees, lives ten minutes on the application clock, dies on first use, and dies after
--      five presentations. Issuing a new one for the same identity REPLACES the row, so the previous
--      code stops working at that moment.
--   2. `slack_link_attempts` — one row per WRONG code a signed-in user presents. Five inside fifteen
--      minutes and every further attempt by that user is a 429. Counted in the database, not in a
--      process, so N replicas do not give a guesser N budgets.
--   3. `slack_identity_links` — the recorded fact of every link and unlink, with the user who did it.
--
-- ⭐⭐ A LINK DECIDES WHOSE APPROVAL A SLACK CLICK COUNTS AS (ADR 0054 §4: double approval counts
-- DIFFERENT oto users). A code is therefore a credential: whoever enters it in THEIR oto session makes
-- the member's Slack clicks count as THEM. The code's short life, single use, the confirmation screen
-- and the recorded fact are the mitigation, and a link that already names another real user is never
-- moved — the route refuses it with a 409.
--
-- ⛔ NO DEFAULT now() (CONTEXT.md §6): every time here is the application's clock.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). Three new tables release N-1 never reads. Nothing existing changes.

-- +goose Up

CREATE TABLE slack_link_codes (
  slack_identity_id UUID        PRIMARY KEY REFERENCES slack_identities(id) ON DELETE CASCADE,
  org_id            UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  code_hash         BYTEA       NOT NULL,
  issued_at         TIMESTAMPTZ NOT NULL,
  expires_at        TIMESTAMPTZ NOT NULL,
  presentations     INTEGER     NOT NULL,
  consumed_at       TIMESTAMPTZ,
  consumed_by       UUID        REFERENCES users(id) ON DELETE SET NULL,
  CONSTRAINT slack_link_codes_hash_uniq     UNIQUE (code_hash),
  CONSTRAINT slack_link_codes_hash_ck       CHECK (octet_length(code_hash) = 32),
  CONSTRAINT slack_link_codes_life_ck       CHECK (expires_at > issued_at),
  CONSTRAINT slack_link_codes_presented_ck  CHECK (presentations >= 0),
  -- A consumer is only ever named on a consumed code; the name itself may later be nulled by the
  -- user's removal, so the pair is one-directional rather than all-or-nothing.
  CONSTRAINT slack_link_codes_consumed_ck   CHECK (consumed_by IS NULL OR consumed_at IS NOT NULL)
);

-- +goose StatementBegin
COMMENT ON TABLE slack_link_codes IS
  'git-bug a556a5c (00105): the one live link code per Slack identity, minted when an UNLINKED member presses a Remedy button and shown once, in an ephemeral only that member sees. Stored ONLY as the sha256 of the normalised code. Lives ten minutes (expires_at, application clock), dies on first use (consumed_at) and after five presentations; a new code for the same identity REPLACES this row, so the previous code dies. A code is a credential: whoever enters it in their own oto session links the member to THEMSELVES.';
-- +goose StatementEnd

CREATE TABLE slack_link_attempts (
  id           UUID        PRIMARY KEY,
  org_id       UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  attempted_at TIMESTAMPTZ NOT NULL
);

-- Serves: "how many wrong codes has this user presented in the last fifteen minutes".
CREATE INDEX slack_link_attempts_user_idx ON slack_link_attempts (org_id, user_id, attempted_at);

-- +goose StatementBegin
COMMENT ON TABLE slack_link_attempts IS
  'git-bug a556a5c (00105): one row per wrong, used or expired link code a signed-in user presented. Five inside fifteen minutes and that user''s further attempts are refused with 429. Rows older than the window are pruned when the user next presents a wrong code.';
-- +goose StatementEnd

CREATE TABLE slack_identity_links (
  id                UUID        PRIMARY KEY,
  org_id            UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  slack_identity_id UUID        NOT NULL REFERENCES slack_identities(id) ON DELETE CASCADE,
  team_id           TEXT        NOT NULL,
  slack_user_id     TEXT        NOT NULL,
  change            TEXT        NOT NULL,
  user_id           UUID        REFERENCES users(id) ON DELETE SET NULL,
  displaced_user_id UUID        REFERENCES users(id) ON DELETE SET NULL,
  actor_id          UUID        REFERENCES users(id) ON DELETE SET NULL,
  at                TIMESTAMPTZ NOT NULL,
  CONSTRAINT slack_identity_links_change_ck    CHECK (change IN ('linked', 'unlinked')),
  CONSTRAINT slack_identity_links_displaced_ck CHECK (change = 'linked' OR displaced_user_id IS NULL)
);

-- Serves: one identity's history, newest last.
CREATE INDEX slack_identity_links_identity_idx ON slack_identity_links (org_id, slack_identity_id, at);

-- +goose StatementBegin
COMMENT ON TABLE slack_identity_links IS
  'git-bug a556a5c (00105): the recorded fact of every Slack link and unlink. change linked: user_id is who the member now resolves to, displaced_user_id the SHADOW member that link retired (NULL when none). change unlinked: user_id is who it no longer resolves to. actor_id is the signed-in user who did it — on this path always the same person as user_id, because no route links or unlinks anybody else. team_id and slack_user_id are copied so the fact reads without a join.';
-- +goose StatementEnd

-- +goose Down

-- The codes, the attempt counts and the recorded facts go with their tables. Below this migration
-- nothing links a Slack member, so nothing reads any of them.
DROP TABLE slack_identity_links;
DROP TABLE slack_link_attempts;
DROP TABLE slack_link_codes;
