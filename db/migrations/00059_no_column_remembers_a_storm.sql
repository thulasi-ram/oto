-- Storm damping leaves the SCHEMA. `alert_groups.storm_mode`,
-- `alert_groups.storm_since`, `groups_storm_ck` (00008) and
-- `channels.storm_notice_at` (00027) are dropped, and
-- `notifications_suppmap_ck` (00018) narrows from EIGHT admitted values to SIX.
--
-- ⭐⭐ THE BEHAVIOUR WENT FIRST AND THIS IS THE HALF THAT WAS DEFERRED. Nothing
-- evaluates a storm any more: `EvaluateStorm`, `ApplyStorm`, `claimStormNotice`,
-- `WarrantsChannelNotice` and the storm reply gate are deleted, and
-- `notification/domain` RETIRED `SuppressedStorm` and `ReasonStorm` at the one
-- writer that could record them. What was left behind was four inert schema
-- objects and three settings keys, all documented in `suppression.go` as
-- "deliberately deferred": deleting a settings key makes
-- `identity/domain/declarative.go` REFUSE AT BOOT, and the storm knobs were
-- documented Helm values, so an operator who had tuned storm would CrashLoop on
-- the next `helm upgrade`. That is the change this migration is the schema half
-- of.
--
-- ⭐ WHY THE DEFENCE WAS WRONG RATHER THAN MERELY UNUSED. A storm is many
-- DIFFERENT alerts arriving together, and the thing that owns many different
-- alerts is an incident — `correlation`, DEFERRED-POST-V1. The damper was built
-- before its object existed, so it had nowhere to put what it detected and put
-- it in the notification layer instead, as a WITHHELD notification. §B.6 refuses
-- exactly that: a suppressed notification an operator cannot distinguish from a
-- signal that never fired. A storm is a truthful report that something is badly
-- wrong, and flooding a channel is a faithful account of it. So the detection is
-- removed rather than quietened, and it belongs beside incidents when they
-- exist.
--
-- ⛔⛔ THIS IS A DESTRUCTIVE MIGRATION IN ONE RELEASE, AND `docs/design/SPEC.md`
-- §D SAYS "Expand/contract only. Never a destructive migration in one release."
-- THE EXEMPTION IS RECORDED HERE BECAUSE IT IS NOT A GENERAL ONE.
--
-- The rule exists because release N and release N+1 run at the same time: a
-- contract that lands before every writer has stopped using the column takes
-- down the release still writing it. That premise is FALSIFIABLE, and it is
-- false here. The maintainer has confirmed that NO oto database and NO Helm
-- release exists anywhere outside a development laptop, and the repository
-- agrees on its own terms:
--
--   * `git tag` is EMPTY — there has never been a tagged release;
--   * `.github/workflows/release.yml` publishes an image only on a `v*.*.*` tag
--     and publishes NO `latest`, so nothing can have pulled a release build
--     even by accident.
--
-- There is therefore no release N to be compatible with, no `orgs.settings`
-- document in the world carrying a `storm_*` key, and no `alert_groups` row with
-- `storm_mode = true`. A three-migration expand/backfill/contract dance to
-- protect a deployment that does not exist would be ceremony, and it would leave
-- two more numbers of tombstone for the next reader to decode. THE MOMENT A
-- TAGGED RELEASE EXISTS THIS EXEMPTION IS SPENT: the next removal of a settings
-- key or a CHECK value goes back to expand/contract, and this header is not a
-- precedent for it.
--
-- ⛔ THE NARROWING FAILS LOUDLY AND DOES NOT REWRITE A ROW. This is 00018's rule,
-- verbatim: an enum narrowing with NO DOWNLEVEL MAPPING must FAIL rather than
-- rewrite history — there is no honest value to turn a stored `storm` or
-- `flapping` suppression into, and inventing one would make the suppression
-- audit page report a reason that never applied. So there is deliberately NO
-- `UPDATE notifications SET suppressed_reason = ...` below. `ADD CONSTRAINT`
-- validates the existing rows, so a database that has ever recorded one of the
-- two dampers refuses this migration with a 23514 naming the constraint, and the
-- person holding it decides what to do. On a laptop the answer is `just db-reset`;
-- there is no other holder.
--
-- ⚠️ SIX VALUES REMAIN, NOT FOUR AND NOT TWO. 00018 widened the check to eight
-- (`channel_disabled`, `no_policy`, `snoozed`, `storm`, `throttled`, `verbosity`,
-- `duplicate_render`, `flapping`). Removing the two dampers leaves
-- `channel_disabled`, `no_policy`, `snoozed`, `throttled`, `verbosity`,
-- `duplicate_render` — and what those six have in common is the whole argument:
-- two mean there was NOWHERE TO SEND, two are A HUMAN ASKING FOR LESS, one is
-- THE WORLD'S RATE LIMIT and one is NOTHING CHANGED. Not one of them is oto's
-- own judgement that a real signal was not worth mentioning. The two being
-- removed were the only two that were.
--
-- ⛔ `notifications.reason` IS NOT TOUCHED, AND THE ASYMMETRY IS DELIBERATE.
-- `notifications_reason_ck` still admits `storm` (00018, re-issued by 00058) and
-- `notification/domain.retiredReasons` still decodes it. The difference is which
-- side of the wire the value sits on: `reason` is what a STORED ROW SAYS IT WAS
-- ABOUT and a card still has to render it, so the value stays declared, stays
-- `Valid` and stays refused at the mint. `suppressed_reason` narrows here only
-- because the same argument applies to it and the same emptiness of the world
-- makes it safe; the retirement mechanism in `suppression.go` is what keeps the
-- Go side honest either way, and it is not removed by this file.
--
-- ⛔ `alert_groups.storm_mode` IS A DIFFERENT KIND OF VALUE AND THAT IS WHY IT
-- GOES. It is not evidence about a past delivery — it is LIVE STATE describing a
-- generation right now, and a live-state column that no writer can ever set
-- again is not history, it is a lie with a NOT NULL DEFAULT false. Same for
-- `channels.storm_notice_at`, which was a LATCH ("when was this channel last
-- told") for an announcement that no longer has anything to announce.

-- +goose Up

-- --------------------------------------------------------------- alert_groups
--
-- The constraint goes first: `groups_storm_ck` is the all-or-nothing pairing
-- `storm_mode = (storm_since IS NOT NULL)`, and dropping either column while it
-- stands would leave it referencing a column that is gone.
ALTER TABLE alert_groups DROP CONSTRAINT groups_storm_ck;
ALTER TABLE alert_groups DROP COLUMN storm_mode;
ALTER TABLE alert_groups DROP COLUMN storm_since;

-- ------------------------------------------------------------------- channels
--
-- The once-per-channel storm-notice latch (00027, ADR 0020 Amendment 1). It
-- windowed a broadcast that no code can produce.
ALTER TABLE channels DROP COLUMN storm_notice_at;

-- -------------------------------------------------- notifications_suppmap_ck
--
-- ⛔ NO BACKFILL, ON PURPOSE. See the header. `ADD CONSTRAINT` validates every
-- existing row, and that validation IS the safety check: it fails with 23514 on
-- a database that has ever recorded a `storm` or `flapping` suppression rather
-- than rewriting the reason into one that never applied.
ALTER TABLE notifications DROP CONSTRAINT notifications_suppmap_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_suppmap_ck CHECK (suppressed_reason IS NULL OR
  suppressed_reason IN ('channel_disabled','no_policy','snoozed','throttled','verbosity',
                        'duplicate_render'));

COMMENT ON COLUMN notifications.suppressed_reason IS
  'Why nothing was sent. Present exactly when status=suppressed. Suppression is RECORDED and shown in the UI; silent suppression destroys trust (SPEC §B.6). When several apply the FIRST MATCH in this fixed order wins (SPEC §B.8.2): channel_disabled, no_policy, snoozed, throttled, verbosity, duplicate_render. Snooze outranks the automatic dampers because it is a deliberate human act and therefore the most actionable explanation. The two dampers that WERE oto own opinion about a signal are gone: every value here is either the absence of a destination, a human asking for less, the world rate limit, or nothing to say.';

-- +goose Down

-- The world as 00018, 00008 and 00027 left it. Every object comes back with the
-- shape, the default, the constraint and the COMMENT it had, because a Down that
-- restores a column without its documentation restores a column nobody can read.

ALTER TABLE notifications DROP CONSTRAINT notifications_suppmap_ck;
ALTER TABLE notifications ADD  CONSTRAINT notifications_suppmap_ck CHECK (suppressed_reason IS NULL OR
  suppressed_reason IN ('no_policy','throttled','storm','flapping','snoozed','verbosity',
                        'channel_disabled','duplicate_render'));

COMMENT ON COLUMN notifications.suppressed_reason IS
  'Why nothing was sent. Present exactly when status=suppressed. Suppression is RECORDED and shown in the UI; silent suppression destroys trust (SPEC §B.6). When several apply the FIRST MATCH in this fixed order wins (SPEC §B.8.2): channel_disabled, no_policy, snoozed, storm, flapping, throttled, verbosity, duplicate_render. Snooze outranks the automatic dampers because it is a deliberate human act and therefore the most actionable explanation.';

-- The widening direction is always safe: nothing on disk can violate a check
-- that admits strictly more, so this half never fails.

ALTER TABLE channels ADD COLUMN storm_notice_at TIMESTAMPTZ;

COMMENT ON COLUMN channels.storm_notice_at IS
  'When this channel was last told, in-channel, that storm damping is on (ADR 0020). It is a LATCH, not history: a storm notice may broadcast only when this is NULL or older than the org storm_cooldown_s, so twenty groups collapsing at once produce ONE channel-level notice rather than twenty. The per-group storm reply still lands on each group thread. NULL means never told.';

-- storm_mode returns NOT NULL DEFAULT false and storm_since returns NULL, which
-- is the only pair groups_storm_ck admits — so the constraint can be added
-- immediately after and validates without a backfill. The information the Up
-- destroyed (which generations were storming, and when each started) is NOT
-- recoverable and this Down does not pretend otherwise: it restores the SHAPE,
-- and every generation comes back reading "not storming", which is what the
-- release being rolled back to would have computed on its next evaluation
-- anyway.
ALTER TABLE alert_groups ADD COLUMN storm_mode  BOOLEAN     NOT NULL DEFAULT false;
ALTER TABLE alert_groups ADD COLUMN storm_since TIMESTAMPTZ;
ALTER TABLE alert_groups ADD CONSTRAINT groups_storm_ck CHECK (storm_mode = (storm_since IS NOT NULL));

COMMENT ON COLUMN alert_groups.storm_mode IS 'Storm collapse is ON by default and is a VISIBLE state, never silent suppression (SPEC §B.6). Paired all-or-nothing with storm_since.';
