-- SPEC §D.1 -- comment text only, no DDL.
--
-- `api_tokens.prefix` is documented in two places and one of them was wrong.
--
--   * pg_description (written by 00003) said "leading identifiable chars of the
--     secret", which is true but says nothing about HOW MANY.
--   * the inline `--` beside the column in 00003 said "first 12 chars, for
--     display: oto_pat_AbCd", which is true for a PAT and WRONG for an ingest
--     token.
--
-- The length depends on the KIND, because the literal does: `oto_pat_` is eight
-- characters plus four random ones is twelve; `oto_ingest_` is ELEVEN plus four
-- is FIFTEEN. That is not a footnote. A single fixed length of 12 in the Go
-- constant is exactly how `POST /api/v1/sources` came to reject every request it
-- was ever sent: it stored `oto_ingest_X`, one random character, and
-- api_tokens_prefix_ck refused it.
--
-- 00003 is applied and is history; it is not edited. Its inline `--` therefore
-- stays wrong ON DISK forever, and the only durable correction available is the
-- one an operator actually reads — the pg_description row. This migration
-- rewrites it to state the arithmetic, so `\d+ api_tokens` answers the question
-- that the stale comment answers incorrectly. (Precedent: 00025.)
--
-- api_tokens_prefix_ck -- `^oto_(pat|ingest)_[A-Za-z0-9]{4}$` -- has ALWAYS
-- admitted both lengths and `prefix` is TEXT, so no DDL is implied. The DDL was
-- right; the prose was not.
--
-- This migration changes no table, column, type, constraint or index. It is safe
-- on a live system and holds no lock worth naming.

-- +goose Up

COMMENT ON COLUMN api_tokens.prefix IS
  'Leading identifiable chars of the secret, for display in the UI so an operator can tell two tokens apart without revealing either. Its LENGTH IS KIND-RELATIVE: the kind literal plus four random chars, so a pat prefix is 12 (oto_pat_AbCd) and an ingest prefix is 15 (oto_ingest_AbCd). Assuming a fixed 12 stores a truncated ingest prefix that api_tokens_prefix_ck rejects.';

-- +goose Down

COMMENT ON COLUMN api_tokens.prefix IS
  'Leading identifiable chars of the secret, for display in the UI so an operator can tell two tokens apart without revealing either.';
