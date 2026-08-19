-- git-bug e05aaad.
--
-- `00057`'s header is emphatic that the flap columns are LIVE state -- "`alerts.flap_score`
-- AND `alerts.is_flapping` ARE NOT MADE DEAD BY W AND ARE NOT RETIRED HERE ... the API
-- filter and the rollup both read them" -- and the retirement that froze them landed the
-- same day. `00007`'s column comments, still the ones an operator sees at `\d+`, describe a
-- metric that a job maintains and a UI state that means something now. Both are false.
--
-- ⛔ `00007` AND `00057` ARE NOT EDITED. An applied migration states the schema as it was
-- at its own version; `00036:99` invokes that rule on itself. The correction is a new
-- version, and it is a comment change only -- no column, index or constraint moves.
--
-- ⭐ THE RETIREMENT IS SETTLED, AND THIS MIGRATION IS THE RECORD OF IT. It was the one open
-- question in this line of work: the detector was retired because the case retention window
-- W blinds `stateChangeCountsSQL` -- a flap absorbed inside W appends no
-- `case.opened`/`case.resolved` -- but W is `NOT NULL DEFAULT 0` over a table that starts
-- empty, so on an unconfigured deployment the detector still worked. git-bug `752cb18`
-- raised that. The owner ruled: the flap detector is not needed. So the columns are frozen
-- deliberately, and what remains is to stop the schema claiming otherwise.
--
-- The trail leads to ADR 0041 Amendment 1, not to `00057`'s promise.

-- +goose Up

-- +goose StatementBegin
COMMENT ON COLUMN alerts.flap_score IS
  'RETIRED IN PLACE, and READABLE BUT STALE (ADR 0041 Amendment 1, SPEC §B.6.2). The `flap.score` job that maintained this EWMA and `AlertRepository.SetFlap` -- the only statement in the tree that ever wrote it -- are deleted, so this column keeps the last value it was given and NOTHING RECOMPUTES IT. It is not zero and not null: it is a MEASUREMENT TAKEN AT A TIME. The words it can support are "the last stored verdict", never "is flapping now". Kept rather than dropped because stored rows still hold it and readers still serve it -- see alerts.is_flapping for which.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alerts.is_flapping IS
  'RETIRED IN PLACE, and READABLE BUT STALE (ADR 0041 Amendment 1, SPEC §B.6.2). Frozen with alerts.flap_score by the same retirement: no job recomputes it, so it reports what was true when the detector last ran. ⛔ TWO SURFACES STILL SERVE THIS FROZEN VALUE and a reader should know what they are getting: the `?flapping=` filter on the alert list (alerts/repository/alert.go) and the `flapping` counter in the alert rollup. Both answer about the LAST STORED VERDICT. The original claim on this column -- "a VISIBLE UI state, never silent suppression" (00007) -- was about a live detector and no longer holds; the retirement is the decision, not a regression.';
-- +goose StatementEnd

-- ⛔ AND ONE FOLLOW-UP IS VOIDED RATHER THAN LEFT STANDING. `00057`'s header scoped future
-- work: "Feeding the deferred resolve into that score needs a new `alert_events.type`, which
-- is an API-contract change and deliberately not bundled here." There is no score left to
-- feed. That sentence describes work nobody should start, and it is dead here so that a
-- reader who finds it in `00057` sees it answered rather than pending.

-- +goose Down

-- Byte-identical to what 00007 shipped, so a rolled-back database is in the state its
-- migration history describes -- including the claim that was true of the release 00007
-- belonged to, when a job really did maintain the score.

-- +goose StatementBegin
COMMENT ON COLUMN alerts.flap_score IS
  'Rolling flap metric from the flap.score job. Never negative.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN alerts.is_flapping IS
  'A VISIBLE UI state, never silent suppression -- silence destroys trust (SPEC §B.6).';
-- +goose StatementEnd
