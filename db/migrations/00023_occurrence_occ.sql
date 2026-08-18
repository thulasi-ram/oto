-- +goose Up
-- +goose StatementBegin

-- Optimistic concurrency control for alert_occurrences.
--
-- Until now there was none at all: the transition write was WHERE org_id AND id
-- with no guard on the state it was computed from, no FOR UPDATE anywhere in the
-- codebase, and READ COMMITTED isolation. A webhook arriving while the reaper
-- was mid-sweep let the reaper stamp `expired` on a demonstrably firing alert —
-- fabricating a resolution, which section B is emphatic must never happen.
--
-- The fix in code compares a four-column pre-image (state, ended_at,
-- source_ends_at, reopen_count). state_version collapses that into a single
-- predicate: cheaper, and it cannot be partially specified by a future call site.
ALTER TABLE alert_occurrences  -- vocab:allow -- schema history: a shipped migration states the world as it was at its own version and is not editable. ADR 0036 renames this in 00052.
    ADD COLUMN state_version INT NOT NULL DEFAULT 1;

ALTER TABLE alert_occurrences  -- vocab:allow -- schema history: a shipped migration states the world as it was at its own version and is not editable. ADR 0036 renames this in 00052.
    ADD CONSTRAINT occ_sver_ck CHECK (state_version >= 1);

-- Re-suppression inside one episode was collapsing: T3 -> T4 -> T3 within a
-- single occurrence was indistinguishable from a single suppression, because
-- nothing counted it. This is the reopen_count analogue for the suppressed path.
ALTER TABLE alert_occurrences  -- vocab:allow -- schema history: a shipped migration states the world as it was at its own version and is not editable. ADR 0036 renames this in 00052.
    ADD COLUMN suppress_count INT NOT NULL DEFAULT 0;

ALTER TABLE alert_occurrences  -- vocab:allow -- schema history: a shipped migration states the world as it was at its own version and is not editable. ADR 0036 renames this in 00052.
    ADD CONSTRAINT occ_supcount_ck CHECK (suppress_count >= 0);

COMMENT ON COLUMN alert_occurrences.state_version IS  -- vocab:allow -- schema history: a shipped migration states the world as it was at its own version and is not editable. ADR 0036 renames this in 00052.
    'Optimistic lock. Every state transition is a compare-and-set on this value; a lost CAS is a conflict, never a silent overwrite.';
COMMENT ON COLUMN alert_occurrences.suppress_count IS  -- vocab:allow -- schema history: a shipped migration states the world as it was at its own version and is not editable. ADR 0036 renames this in 00052.
    'How many times this episode has entered suppressed. Distinguishes repeated silencing from a single suppression.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE alert_occurrences DROP CONSTRAINT IF EXISTS occ_supcount_ck;
ALTER TABLE alert_occurrences DROP COLUMN IF EXISTS suppress_count;
ALTER TABLE alert_occurrences DROP CONSTRAINT IF EXISTS occ_sver_ck;
ALTER TABLE alert_occurrences DROP COLUMN IF EXISTS state_version;

-- +goose StatementEnd
