-- git-bug bd0fb1d, the settings half. 00067 stopped a POLICY subscribing to the
-- retired reason; this removes the knobs that configured the mechanism itself.
--
-- ⛔ A KNOB NOTHING READS IS THE DEFECT, NOT A LEFTOVER. `notify.unacked_reminder`
-- and its sweep are gone in this same change, so every one of these five settings is
-- now a value an operator can save, read back, and never hear from again. That is
-- precisely what `0457f1f`, `35d4248`, `39e48e2` and `27a1860` were each filed and
-- closed for. Leaving them "in case the reminder comes back" would make five more.
--
-- ⭐ FIVE, NOT ONE, AND THE OWNER RULED ON THE OTHER FOUR DIRECTLY. Asked what the
-- point of `unacked_reminder_mention` is when the whole reminder flow is going, the
-- answer was that there is none: a mention is not a property of Slack delivery in
-- general, it is the AUDIENCE HALF of the one reminder and exists nowhere else. With
-- no reminder there is nobody to address, so the three mention keys go with the
-- delay they qualified.
--
-- ⭐ THIS ALSO STRENGTHENS SCOPE-BOUNDARY §5.2 RATHER THAN RETREATING FROM IT. That
-- ruling restricted `mention_on_reminder` to usergroups and `!here`/`!channel` and
-- dropped individual `<@U...>` mentions, because mentioning an individual names a
-- RESPONDER and oto must never know who is on call (H-1, FR-1). Deleting the mention
-- surface outright removes the only place oto could have crossed that line.
--
-- ⚠️ CONFIG, NOT HISTORY — the same division 00067 drew. `notifications.reason` still
-- holds `unacked_reminder` rows and this migration does not touch them: a reminder
-- that was sent really was sent. What goes is the configuration that asked for
-- future ones.

-- +goose Up

-- The per-policy delay. `policies_reminder_ck` bounded it (60..86400) and names the
-- column, so it is dropped first for the reason 00065 gave: leaving it to DROP
-- COLUMN's cascade makes the migration depend on drop order rather than on being
-- right. The constraint was `policies_esc_ck` until 00019 renamed it — the rename is
-- why the name does not say `escalation` any more.
ALTER TABLE notification_policies DROP CONSTRAINT policies_reminder_ck;
ALTER TABLE notification_policies DROP COLUMN unacked_reminder_after_s;

-- The org-level defaults. `orgs.settings` is JSONB, so these are key removals rather
-- than column drops, and `-` on a jsonb object is a no-op for a key that is absent —
-- which is what makes this safe on an org that never set any of them.
UPDATE orgs
   SET settings = settings
         - 'unacked_reminder_after_s'
         - 'unacked_reminder_mention'
         - 'unacked_reminder_mention_list'
         - 'unacked_reminder_mention_min_severity',
       updated_at = now()
 WHERE settings ?| ARRAY[
         'unacked_reminder_after_s',
         'unacked_reminder_mention',
         'unacked_reminder_mention_list',
         'unacked_reminder_mention_min_severity'];

-- +goose Down

ALTER TABLE notification_policies
  ADD COLUMN unacked_reminder_after_s INT;

ALTER TABLE notification_policies ADD CONSTRAINT policies_reminder_ck
  CHECK (unacked_reminder_after_s IS NULL
         OR unacked_reminder_after_s BETWEEN 60 AND 86400);

-- ⚠️ THE SHAPE COMES BACK AND THE VALUES DO NOT, in both halves. Every policy
-- returns with a NULL delay — "inherit the org default", which is also gone — and no
-- org gets its mention configuration back. The Up dropped the column and deleted the
-- JSONB keys, and neither records what it removed, so a mirroring restore would have
-- to invent which orgs wanted to mention whom. Inventing an audience for a mention
-- is the one fabrication this schema should never make. An operator finishing this
-- downgrade sets them again by hand, and saying so beats an UPDATE that would look
-- like a rollback.

-- +goose StatementBegin
COMMENT ON COLUMN notification_policies.unacked_reminder_after_s IS
  'How long a signal may go unacknowledged before this policy says so once. NULL inherits orgs.settings.unacked_reminder_after_s. ⛔ A SCALAR: one stage, forever (SPEC §G.9.1) -- a ladder is an escalation policy and is permanently out of scope.';
-- +goose StatementEnd
