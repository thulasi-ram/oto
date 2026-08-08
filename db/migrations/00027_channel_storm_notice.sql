-- ADR 0020 (SPEC §B.6, §H.6) -- channels gains the channel-level storm-notice latch.
--
-- WHY. §B.6 forbids a damper that engages silently: when oto starts withholding
-- individual notifications, the people in the channel have to be told, or the
-- quiet that follows is indistinguishable from nothing happening. ADR 0020's
-- first answer was to BROADCAST the per-group `storm` reply. That answer was
-- wrong in the one direction that matters: storm mode is decided PER GROUP, and a
-- channel carries many groups, so a real storm -- twenty generations collapsing
-- inside a minute -- produced twenty `chat.postMessage` calls into one channel.
-- oto shouting, once per group, about having started to be quiet: exactly the
-- flood the damping exists to prevent.
--
-- The fact is addressed to the CHANNEL, so it is now latched on the CHANNEL. The
-- per-group `storm` reply still lands on each group's own thread and the record
-- stays complete; at most one of those replies is allowed to surface in-channel
-- inside a window.
--
-- ⛔ A TIMESTAMP, NOT A COUNTER, AND THAT IS THE DESIGN. A reference count of
-- storming groups would be exact and would also LEAK: `Close` clears
-- `storm_mode` on an idle generation with no storm-end evaluation, so a closed
-- storming group would decrement nothing and the count would never return to
-- zero -- after which the channel is never told about another storm, ever,
-- silently. A window self-heals. The worst case of a window is one extra notice;
-- the worst case of a leaked counter is a damper whose own announcement is
-- permanently dead. §B.6 has a clear preference between those.
--
-- NULL means "never told", which is why the column is nullable and has no
-- default: a DEFAULT now() would make every existing channel look as though it
-- had just been warned about a storm that never happened.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). Adding a NULLable column is a pure widening:
-- the expand phase is the whole of the Up, there is nothing to backfill, and a
-- release-N writer that has never heard of the column is unaffected because
-- nothing reads it and nothing constrains it. The Down drops it, and dropping it
-- loses only "when was this channel last told about a storm", which is a damper's
-- own bookkeeping and not a fact about any alert.
--
-- (This number previously carried `severity_raised`, a notification Reason for a
-- transition oto cannot observe: `severity` is a Prometheus LABEL and is hashed
-- into `alert_key` (§C.2), so two severities of one rule are two Alerts, not one
-- Alert changing. Nothing was deployed, so the number was reused rather than
-- leaving a tombstone for an enum value with no writer. ADR 0020 records the
-- finding and `test/integration/alert_identity_test.go` proves it.)

-- +goose Up

ALTER TABLE channels ADD COLUMN storm_notice_at TIMESTAMPTZ;

COMMENT ON COLUMN channels.storm_notice_at IS
  'When this channel was last told, in-channel, that storm damping is on (ADR 0020). It is a LATCH, not history: a storm notice may broadcast only when this is NULL or older than the org storm_cooldown_s, so twenty groups collapsing at once produce ONE channel-level notice rather than twenty. The per-group storm reply still lands on each group thread. NULL means never told.';

-- +goose Down

ALTER TABLE channels DROP COLUMN storm_notice_at;
