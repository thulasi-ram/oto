-- git-bug 7570090, stage 6 of 7 — done-when 8. A policy declares WHICH ALTITUDE
-- of fact it is about, and HOW MANY of them have to have happened before it
-- speaks.
--
-- ⭐⭐ BOTH ARE FURTHER AXES ON MACHINERY THAT ALREADY SHIPS, WHICH IS THE RULING
-- THIS MIGRATION IS BUILT UNDER, and the axes are not new inventions dressed up as
-- old ones. `notification_policies.throttle` is `{"max":N,"window_s":S}` — a
-- CEILING over a span (00011: *"Per-subject rate cap"*). `digest_floor` over
-- `digest_window_s` is *"the minimum number of Cases that must have OPENED inside
-- the window"* (00058) — a FLOOR over a span. Two policy-level counts over
-- policy-level windows already exist. What neither can express is the third cell
-- of the same table:
--
--     span        | ceiling            | floor
--     ------------+--------------------+---------------------------
--     tiled       | —                  | digest_floor        (00058)
--     sliding     | throttle           | count_min           (HERE)
--
-- and what NEITHER can express is the UNIT, because 00058 chose it in a comment.
-- `digest_floor` counts Cases and no operator can say otherwise; `subject_kinds`
-- is that hardcoded choice becoming a column. So the two columns in this migration
-- are one idea: a count, and the thing being counted.
--
-- ⛔ WHAT THIS MIGRATION DOES NOT DO, so the next reader is not misled — the same
-- disclosure 00063 carried, for the same reason.
--
--   * `subject_kinds` IS LIVE ON THE ROUTING PATH from the moment the Go side
--     lands. `Policy.Handles` — the gate `PolicyService.Evaluate` already calls on
--     every lifecycle transition, and `Policy.Digests` through it — now asks the
--     binding as well as the reason list. There is no second call site to add and
--     no service change is owed.
--   * `count_min` / `count_window_s` ARE STORED, VALIDATED AND PUBLISHED, AND
--     NOTHING SUPPRESSES ON THEM YET. The evaluator is one comparison beside the
--     throttle's at `internal/notification/service/notify.go`, where the count query
--     and a `suppressed_reason` for "below the floor" both belong; that is a
--     separate change with a separate migration widening
--     `notifications_suppmap_ck`. Until it lands a count condition is a stored
--     intention, exactly as `group_by` was *"READ and HASHED but nothing routes on
--     the result"* between 00063 and 00065.
--
-- ⚠️ AND THAT IS SAID OUT LOUD BECAUSE A KNOB NOTHING HONOURS IS THIS REPOSITORY'S
-- MOST-REPEATED DEFECT — `0457f1f`, `35d4248`, `39e48e2`, `27a1860`, and
-- `refire_grace_s` in 00071, which was bounds-validated, defaulted from a measured
-- corpus, patchable, declaratively settable and read by nothing. The difference
-- between that and a stage is whether the remaining half is NAMED. It is named
-- above, in one sentence, with the file it lands in. If the next release does not
-- land it, these two columns are the fifth ghost and should be dropped rather than
-- left.
--
-- ⭐ A POLICY THAT DECLARES NEITHER BEHAVES EXACTLY AS TODAY, and that is what
-- makes the change monotone. `subject_kinds` defaults to `'{}'`, which is read as
-- "every altitude" — so every one of the rows this migration rewrites keeps
-- claiming everything it claimed yesterday, and the only new outcomes are
-- narrowings an operator asked for by name. `count_min` and `count_window_s`
-- default to NULL, which is "no condition". The failure mode on this path is a
-- `no_policy` SUPPRESSION rather than an error anybody sees, so the direction of
-- the default is the whole safety argument, exactly as it was for stage 1
-- (`d76ee0d`).
--
-- ⭐ THE BINDING OVERLAPS `reasons` AND THE OVERLAP IS CONCEDED RATHER THAN
-- CONCEALED. `Reason.Subject()` is a TOTAL Reason → SubjectKind map, so as a
-- routing filter `subject_kinds = {case}` selects exactly what a hand-narrowed
-- `reasons` list could have selected. Two things justify the column anyway, and
-- both are things a `reasons` list cannot do at all:
--
--   * it is the count's UNIT — `policies_count_subject_ck` below requires a count
--     condition to name exactly ONE kind, because summing an alert-subject fact and
--     a case-subject fact into one number is adding identities to episodes; and
--   * it is a DECLARATION AN OPERATOR CAN READ. Deriving "this policy is about
--     firing episodes" from a fifteen-element reason list means knowing
--     `reasonSubjects` by heart, and the coherence check in `Policy.Validate` then
--     refuses the combination that is otherwise SILENT — a binding admitting none
--     of the declared reasons, which routes nothing and records a `no_policy`
--     suppression per fact forever on a policy whose settings screen looks
--     configured.
--
-- ⛔ THE VOCABULARY IS ALL THREE KINDS AND NOT THE TWO THE TICKET NAMED. The ticket
-- says *"a subject-kind binding (alert or case)"*, because those are the two
-- altitudes an operator could not previously reach. `digest` is admitted anyway:
-- `subject_kinds` filters the same axis `notifications.subject_kind` holds, and a
-- filter over a closed set with one member arbitrarily excluded creates a value
-- that is storable in the column and unsayable in the filter — the drift
-- CONTEXT.md §5b exists to prevent. A digest-only policy may now say
-- `subject_kinds = {digest}` and mean it, and the coherence check makes that the
-- only binding a digest-only policy can legally carry besides the empty one.
--
-- ⛔ NEITHER WINDOW IS A SCHEDULE, and this migration does not weaken that
-- (SCOPE-BOUNDARY §4.8, SPEC §G.9.1). `count_window_s` says how far BACK to count.
-- It carries no timezone, no time of day and no weekday, and it never will: the
-- moment a policy can say "only tell me between 09:00 and 17:00" oto has quiet
-- hours, which is a person's schedule wearing a policy's clothes.
-- `notification_policies` gains no `user_ids`, `team_ids`, `schedule_id` or second
-- reminder stage here either.
--
-- ⭐ AND `count_window_s` IS A SLIDING LOOKBACK, WHICH IS WHY IT HAS NO DIVISOR
-- RULE AND `digest_window_s` DOES. A digest is a REPORT ABOUT A SPAN, so the span
-- has to be an object two pods can independently name — hence 00058's
-- `86400 % digest_window_s = 0`, which makes every boundary a wall-clock boundary
-- in UTC. A count window is `[now - W, now]`, re-derived at every evaluation from
-- the instant of the fact being evaluated, exactly as the throttle's window is.
-- Nothing is reported about the span, so nothing has to agree on where it starts,
-- and a divisor rule here would refuse ninety seconds for a reason that does not
-- apply to it.
--
-- ⚠️ THE BOUNDS ARE THE THROTTLE'S AND NOT THE DIGEST'S, DELIBERATELY.
-- `policies_throttle_ck`'s window is 60..86400 and `policies_digest_window_ck`'s is
-- 300..86400. A digest shorter than five minutes is the per-event stream it exists
-- to replace wearing a delay; a count condition SENDS the event, and "five of these
-- in the last ninety seconds" is a question somebody genuinely asks about a crash
-- loop. The threshold floor is 2, not 1, for the reason `digest_floor` excludes
-- zero: the fact being evaluated is itself inside the window, so a threshold of one
-- is cleared unconditionally and describes a behaviour that does not exist.
--
-- ⚠️ NO INDEX IS ADDED, WHICH IS A DECISION AND NOT AN OMISSION. 00058 added
-- `policies_digest_idx` because the digest tick has to FIND the policies that carry
-- a window — a query that runs once a minute per tenant forever. Nothing searches
-- for a count condition or for a binding: both are read off the policy row that
-- `policies_eval_idx` already returned, on a walk that stops at the first match. An
-- index over a column nothing filters on is the same dead weight as a knob nothing
-- reads.
--
-- EXPAND/CONTRACT (CONTEXT.md §6). This is an EXPAND step and every new constraint
-- is satisfied by every row release N can write: two columns are ADDED nullable,
-- one is ADDED `NOT NULL` with a DEFAULT that every existing row takes, and no
-- existing constraint is touched. Release N writes no `subject_kinds` and no count,
-- so it is safe to deploy under it — a release-N pod inserting a policy simply
-- takes the default and NULLs, which is today's behaviour by construction.
--
-- R9 / three copies. Each bound is written in exactly three places and this
-- migration is one of them: the CHECK here, the `validate` tag on the request DTOs
-- in `internal/notification/api/dto.go`, and the `Min…`/`Max…` constants in
-- `internal/notification/domain`. `MaxPolicySubjectKinds` is the fourth spelling of
-- the vocabulary SIZE and is `len(AllSubjectKinds())` BY RULE, so it follows the
-- enum in both directions rather than being a number somebody chose — the same
-- discipline `MaxPolicyReasons` is held to.

-- +goose Up

-- ------------------------------------------------------ notification_policies

-- `NOT NULL DEFAULT '{}'` rather than nullable, because there is no third state to
-- represent. `reasons` is NOT NULL for the same reason. "This policy claims every
-- altitude" and "this policy has not been told which altitude" are the same
-- instruction, so a NULL would be a second spelling of the empty array and every
-- reader would have to handle both.
ALTER TABLE notification_policies
  ADD COLUMN subject_kinds TEXT[] NOT NULL DEFAULT '{}'::text[];

-- ⛔ CONTAINMENT AND SET-NESS, AND NO CARDINALITY ARM. A set drawn from a
-- three-value vocabulary cannot exceed three by construction, so `cardinality(…) <=
-- 3` would be a number no row could ever test — which is exactly the defect 00046
-- removed from `policies_reasons_ck` when it stopped saying 32 ("32 stopped being a
-- number any row could reach"). The Go ceiling `MaxPolicySubjectKinds` exists for
-- the DTO's `maxItems`, not because the DDL needs it.
--
-- ⚠️ THE NULL-ELEMENT ARM IS NOT REDUNDANT WITH `<@`, and dropping it would be a
-- silent hole. `ARRAY[NULL]::text[] <@ ARRAY['alert']` evaluates to NULL, not
-- FALSE, and a CHECK admits NULL — so containment alone would let
-- `subject_kinds = '{NULL}'` through, where `array_position` is the only test that
-- answers TRUE or FALSE. `policies_reasons_ck` carries the same arm for the same
-- reason.
--
-- ⚠️ THE VOCABULARY IS A LITERAL HERE AND THAT IS THE COST OF A CLOSED SET IN DDL.
-- It is the third copy (with `subjectKinds` in `internal/notification/domain` and
-- the contract's enum), and narrowing or widening the SubjectKind vocabulary means
-- editing this constraint — which is precisely what happened when 00069 deleted
-- `alert_group` from `notifications_subjkind_ck` and `threads_subjkind_ck`. This
-- constraint joins those two as a place that has to move.
ALTER TABLE notification_policies ADD CONSTRAINT policies_subjkinds_ck
  CHECK (subject_kinds <@ ARRAY['alert','case','digest']::text[]
         AND array_position(subject_kinds, NULL) IS NULL
         AND oto_array_is_set(subject_kinds));

-- The count and its span. NULL for both is "no condition", which is what every
-- existing row says and what every policy created by release N will say.
ALTER TABLE notification_policies ADD COLUMN count_min      INT;
ALTER TABLE notification_policies ADD COLUMN count_window_s INT;

-- 2..10000. Two because the fact being evaluated is itself inside the window, so a
-- threshold of one clears unconditionally and states no condition — the same
-- argument that keeps zero out of `policies_digest_floor_ck`. The ceiling is
-- `digest_floor`'s, because it counts the same objects over the same kind of span
-- and two different ceilings on one arithmetic would be a number to reconcile
-- rather than a bound to honour.
ALTER TABLE notification_policies ADD CONSTRAINT policies_count_min_ck
  CHECK (count_min IS NULL OR count_min BETWEEN 2 AND 10000);

-- 60..86400, the THROTTLE's window bound and not the digest's, and with no divisor
-- rule. See the header for both.
ALTER TABLE notification_policies ADD CONSTRAINT policies_count_window_ck
  CHECK (count_window_s IS NULL OR count_window_s BETWEEN 60 AND 86400);

-- ⭐ SYMMETRIC, UNLIKE `policies_digest_pair_ck`, AND THE ASYMMETRY BETWEEN THE TWO
-- PAIR RULES IS THE POINT. A digest's floor is optional because its WINDOW alone is
-- a complete instruction — "summarise every ten minutes" sends whenever the window
-- was not empty — so 00058's rule is one-directional. Neither half of a count
-- condition means anything alone: a threshold over unbounded history is not
-- evaluable, and a window with no threshold counts facts and then compares the
-- number against nothing. Both are configuration mistakes that would silently mute
-- or silently un-mute a channel, so both are refused.
ALTER TABLE notification_policies ADD CONSTRAINT policies_count_pair_ck
  CHECK ((count_min IS NULL) = (count_window_s IS NULL));

-- ⭐⭐ THE UNIT RULE, AND IT IS WHY THE TWO AXES ARRIVE IN ONE MIGRATION RATHER
-- THAN TWO. A count is a number of somethings. The something is the policy's bound
-- subject kind, so a count condition on an UNRESTRICTED binding has no unit and a
-- count condition on a two-kind binding has two — and summing an alert-subject fact
-- (an identity, true across every firing it ever had) with a case-subject fact (one
-- firing episode) produces a number that is about nothing. It applies to the count
-- ONLY: a policy carrying no condition needs no unit and keeps every binding
-- `policies_subjkinds_ck` admits, including the empty one.
ALTER TABLE notification_policies ADD CONSTRAINT policies_count_subject_ck
  CHECK (count_min IS NULL OR cardinality(subject_kinds) = 1);

-- ⛔⛔ AND THE ONE KIND MUST BE `case`, WHICH IS NARROWER THAN THIS MIGRATION FIRST
-- CLAIMED AND IS WRITTEN DOWN HERE BECAUSE THE OTHER TWO BINDINGS DECIDE NOTHING AN
-- OPERATOR WOULD RECOGNISE. `policies_count_subject_ck` above gets the CARDINALITY
-- right and says nothing about WHICH kind, and both of the kinds it still admits are
-- broken in different directions:
--
--   * `{alert}` IS A PERMANENT MUTE. The numerator is `count(DISTINCT subject_id)` and
--     an alert-subject notification's `subject_id` is the alert IDENTITY -- one value,
--     unchanged across every firing it ever has. So an alert that re-fires five times
--     contributes ONE distinct subject, the evaluator's `+ 1` for the fact in hand
--     makes it one, and `1 < count_min` suppresses as `below_threshold` every
--     notification that policy would ever route, forever. The sentence this migration
--     advertises for the feature -- "tell me once the pod has restarted five times this
--     hour", the replacement for the hardcoded flap detection ADR 0042 deleted -- could
--     never be satisfied on that binding by the facts it describes. What the query
--     actually counted there was OTHER alert identities the same policy had recorded
--     facts about, which is a question nobody asked.
--   * `{digest}` IS AN INERT KNOB. A digest is minted by the tick in
--     `internal/notification/service/digest.go`, which evaluates `digest_floor` over
--     its own tiled window and never reaches the suppressors at all -- so `count_min`
--     on a digest-bound policy is READ BY NOTHING. That is the `refire_grace_s` shape
--     00071 deleted two settings for, arriving one migration later on a different
--     column. It is also the only binding a digest window and a count condition can
--     legally share, because a window needs `digest` bound and a count needs exactly
--     one kind, so refusing it is what stops one policy carrying both floors with one
--     of them inert.
--
-- Refusing beats reinterpreting. Making `{alert}` count OCCURRENCES would mean
-- `count(*)` over oto's own rows about one identity -- acked, enriched, snoozed, the
-- floor's own suppressions -- which is a THROTTLE's numerator wearing a floor's
-- clothes, and it would change what an operator observes into something no column
-- states. `{case}` needs no reinterpretation: five firings of one alert are five Cases
-- and therefore five distinct `subject_id`s, which is the count the feature was sold
-- on. `Policy.validateCount` refuses the same combination as a field-level violation
-- naming `subject_kinds`, so an operator meets the sentence and not a 23514.
--
-- ⚠️ THE LIMITATION IS THEREFORE PART OF THE FEATURE AND NOT A BUG LEFT OPEN: there is
-- no way to ask for a count over alert IDENTITIES, and there should not be one until
-- somebody names the question it answers. The published contract's `subject_kinds`
-- enum still admits all three values, because the column does; what it may not carry
-- is a count condition beside two of them.
ALTER TABLE notification_policies ADD CONSTRAINT policies_count_case_ck
  CHECK (count_min IS NULL OR subject_kinds = ARRAY['case']::text[]);

-- ⭐⭐ AND THE MIRROR RULE FOR THE DIGEST, WHICH `policies_digest_reason_ck` LEAVES A
-- HOLE IN NOW THAT A SECOND COLUMN CAN REFUSE A REASON. 00058's constraint says a
-- policy carrying `digest_window_s` must list the `digest` reason, and until this
-- migration that was the whole of "this policy can route its own digests". Since
-- `Policy.Handles` also consults `subject_kinds`, a row with a window, `digest` in
-- `reasons` and a binding that omits the `digest` altitude passes every existing CHECK
-- and routes NOTHING: `SweepOrg` logs "a digest policy does not route the digest
-- reason" once per tick forever, and `ReconcileOrg` skips the policy entirely, which
-- hides the very gap the skip creates. `UpdatePolicy` writes `subject_kinds` through a
-- bare `COALESCE`, so a PATCH that touches only the binding could produce exactly that
-- row with nothing but Go validation between it and the table.
--
-- The empty binding is admitted because empty means EVERY kind, which is the default
-- and includes `digest`.
ALTER TABLE notification_policies ADD CONSTRAINT policies_digest_subject_ck
  CHECK (digest_window_s IS NULL
         OR cardinality(subject_kinds) = 0
         OR 'digest' = ANY(subject_kinds));

-- +goose StatementBegin
COMMENT ON COLUMN notification_policies.subject_kinds IS
  'WHICH ALTITUDE OF FACT THIS POLICY IS ABOUT: a set over the notifications.subject_kind vocabulary (alert | case | digest). EMPTY IS THE DEFAULT AND MEANS EVERY KIND, which is what every row written before 00072 says and is today''s behaviour exactly -- the direction matters, because the failure mode on this path is a no_policy SUPPRESSION rather than an error anybody sees. Consulted by Policy.Handles, the gate PolicyService.Evaluate already calls on every lifecycle transition, so it is a further axis on that gate and not a second one. ⭐ ITS OTHER JOB IS TO BE THE COUNT''S UNIT: policies_count_subject_ck requires a count_min to name exactly one kind, because digest_floor counts Cases only because 00058 chose Cases in a comment, and this is that choice becoming a column. ⛔ AND policies_count_case_ck REQUIRES THAT ONE KIND TO BE case: an alert-subject subject_id is the alert identity, unchanged across every firing, so a count over it can never exceed one and would mute the policy permanently, and a digest-bound count is read by nothing because the digest tick evaluates digest_floor and never reaches a suppressor. ⛔ policies_digest_subject_ck REQUIRES A POLICY WITH digest_window_s TO BIND digest (or bind nothing, which is everything), because since this migration Handles refuses a reason the binding does not claim -- so a binding omitting digest makes the digest tick warn once per tick forever and the reconciler skip the policy. ⚠️ It OVERLAPS `reasons` and the overlap is conceded: Reason.Subject() is total, so as a filter this is derivable from a hand-narrowed reason list. It earns its place as the count''s unit and as a declaration an operator can read without knowing the fifteen-entry Reason-to-subject map by heart. ⛔ NOT A SCHEDULE and never gains one (SCOPE-BOUNDARY §4.8).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notification_policies.count_min IS
  'THE FLOOR TO throttle''S CEILING: stay silent until at least this many facts about this policy''s bound subject kind have happened inside count_window_s. 2..10000, or NULL for no condition -- ONE IS EXCLUDED because the fact being evaluated is itself inside the window, so a threshold of one clears unconditionally and describes a behaviour that does not exist, exactly as zero is excluded from digest_floor. ⭐ It is the same two fields as throttle with the opposite sense, and a policy may carry both: "tell me once the pod has restarted five times this hour, then no more than twice an hour after that" is ONE policy with a floor and a ceiling. ⛔ AS OF 00072 NOTHING SUPPRESSES ON IT YET -- the evaluator is one comparison beside the throttle''s in internal/notification/service, with the count query and a suppressed_reason for "below the floor", and that is a separate change widening notifications_suppmap_ck. Named here so this column is a STAGE and not the fifth knob nothing reads (see 00071 on refire_grace_s).';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN notification_policies.count_window_s IS
  'HOW FAR BACK count_min COUNTS, 60..86400 seconds, or NULL for no condition. ⭐ A SLIDING LOOKBACK AND NOT A TILED WINDOW, which is why it has no divisor rule where digest_window_s has one: a digest is a REPORT ABOUT A SPAN so the span must be an object two pods can independently name (86400 % digest_window_s = 0, every boundary a wall-clock boundary in UTC), whereas this is [now - W, now] re-derived at every evaluation from the instant of the fact, exactly as the throttle''s window is. Nothing is reported about the span, so nothing has to agree where it starts. Its bound is the THROTTLE''S 60..86400 and not the digest''s 300..86400: a digest shorter than five minutes is the per-event stream it replaces wearing a delay, whereas "five of these in the last ninety seconds" is a real question about a crash loop. ⛔ NOT A SCHEDULE (SCOPE-BOUNDARY §4.8): it says how far back to COUNT, carries no timezone, no time of day and no weekday, and never will -- quiet hours is the feature SCOPE-BOUNDARY §4.8 refuses, and a window over facts is not its first half.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_count_pair_ck ON notification_policies IS
  'Both halves of a count condition or neither. SYMMETRIC, unlike policies_digest_pair_ck: a digest WINDOW alone is a complete instruction ("summarise every ten minutes"), whereas a threshold over unbounded history is not evaluable and a window with no threshold counts facts and compares the number against nothing. Either half alone would silently mute or silently un-mute a channel.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_count_subject_ck ON notification_policies IS
  'A count condition must name EXACTLY ONE subject kind, because a count needs a unit. An unrestricted binding supplies none and a two-kind binding supplies two, and adding an alert-subject fact (an identity, true across every firing) to a case-subject fact (one firing episode) yields a number that is about nothing. It constrains the count only: a policy with no condition keeps every binding policies_subjkinds_ck admits.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_count_case_ck ON notification_policies IS
  'AND THE ONE KIND IS case. The count numerator is count(DISTINCT subject_id), and an alert-subject row''s subject_id is the alert IDENTITY -- one value across every firing -- so a count condition bound to {alert} sits at one forever and suppresses every notification the policy routes as below_threshold, permanently: "tell me once the pod has restarted five times this hour" is unsatisfiable on that binding by the facts it describes. A condition bound to {digest} is read by nothing at all, because digests are minted by the digest tick against digest_floor and never reach a suppressor -- the refire_grace_s shape 00071 deleted two settings for. {case} is the binding the feature was sold on and the only one whose arithmetic is honest: five firings of one alert are five Cases and five distinct subject_ids. Counting how many times one alert FIRED instead of how many identities there were would mean count(*) over oto''s own rows about that alert, which is a throttle''s numerator, so this refuses rather than reinterprets.';
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON CONSTRAINT policies_digest_subject_ck ON notification_policies IS
  'A policy carrying digest_window_s must BIND the digest altitude (or bind nothing, which is every altitude). policies_digest_reason_ck makes the window imply the digest REASON; since 00072 Policy.Handles also consults subject_kinds, so a binding that omits digest refuses the reason a second way and the row routes nothing while looking configured: the digest tick warns once per tick forever and DigestService.ReconcileOrg skips the policy, hiding the gap that results. UpdatePolicy writes subject_kinds through a bare COALESCE, so this is the layer that catches a PATCH touching only the binding.';
-- +goose StatementEnd

-- +goose Down

-- ⛔ INTEGRITY CHECK: A COUNT CONDITION MUST STILL NAME EXACTLY ONE SUBJECT KIND.
-- `policies_count_subject_ck` is about to be dropped, so this is the last moment
-- anything can observe whether it held. A violating row cannot have been written
-- through the service — `Policy.validateCount` refuses it as a field-level 422
-- before the insert, and the constraint refuses it as a 23514 after — so its
-- existence means the CHECK was disabled, dropped or bypassed, and a count over an
-- unstated or a mixed unit is a threshold whose number is about nothing. Finding
-- one AFTER the columns are gone is impossible, so it is found here.
-- +goose StatementBegin
DO $$
DECLARE bad BIGINT;
BEGIN
  SELECT count(*) INTO bad
    FROM notification_policies
   WHERE count_min IS NOT NULL
     AND cardinality(subject_kinds) <> 1;

  IF bad > 0 THEN
    RAISE EXCEPTION
      'migration 00072 down: % policy row(s) carry a count condition without exactly one subject kind, which policies_count_subject_ck forbids. A count needs a unit, so every such row holds a threshold whose number is about nothing -- the constraint has been disabled, dropped or bypassed. Inspect them with:  SELECT id, org_id, name, subject_kinds, count_min, count_window_s FROM notification_policies WHERE count_min IS NOT NULL AND cardinality(subject_kinds) <> 1;  then decide whether the binding or the condition is wrong. Do not clear the column to get past this without reading the rows first -- they are the only record of what an operator asked for.',
      bad;
  END IF;
END $$;
-- +goose StatementEnd

-- ⛔ INTEGRITY CHECK: AND THAT ONE KIND MUST STILL BE `case`, checked separately from
-- the cardinality above because it is a separate constraint with a separate failure.
-- A surviving `{alert}` condition is a policy that has been suppressing EVERY
-- notification it routes as `below_threshold` since the CHECK was bypassed -- silence
-- an operator experiences as oto being broken -- and a surviving `{digest}` one is a
-- threshold nothing has ever read. Both become unfindable once the columns are gone.
-- +goose StatementBegin
DO $$
DECLARE bad BIGINT;
BEGIN
  SELECT count(*) INTO bad
    FROM notification_policies
   WHERE count_min IS NOT NULL
     AND cardinality(subject_kinds) = 1
     AND subject_kinds <> ARRAY['case']::text[];

  IF bad > 0 THEN
    RAISE EXCEPTION
      'migration 00072 down: % policy row(s) carry a count condition bound to something other than {case}, which policies_count_case_ck forbids. An {alert} binding counts alert IDENTITIES, which do not change when an alert fires again, so that policy has been muting every notification it routes as below_threshold; a {digest} binding is read by nothing at all. Inspect them with:  SELECT id, org_id, name, subject_kinds, count_min, count_window_s FROM notification_policies WHERE count_min IS NOT NULL AND subject_kinds <> ARRAY[''case'']::text[];  and note what the operator asked for before the columns go -- the request is recoverable from these rows and from nowhere else.',
      bad;
  END IF;
END $$;
-- +goose StatementEnd

-- ⛔ INTEGRITY CHECK: THE PAIR RULE MUST STILL HOLD, for the same reason and at the
-- same last moment. A half-populated pair is a condition that is neither on nor
-- off, and the down below drops both columns, which would make it unfindable and
-- would silently resolve it in whichever direction the missing half implies.
-- +goose StatementBegin
DO $$
DECLARE bad BIGINT;
BEGIN
  SELECT count(*) INTO bad
    FROM notification_policies
   WHERE (count_min IS NULL) <> (count_window_s IS NULL);

  IF bad > 0 THEN
    RAISE EXCEPTION
      'migration 00072 down: % policy row(s) carry half a count condition, which policies_count_pair_ck forbids. Neither half means anything alone. Inspect them with:  SELECT id, org_id, name, count_min, count_window_s FROM notification_policies WHERE (count_min IS NULL) <> (count_window_s IS NULL);  then complete or clear the pair before rolling back.',
      bad;
  END IF;
END $$;
-- +goose StatementEnd

-- ⚠️ AND THE DESTRUCTION IS SAID OUT LOUD, BECAUSE BOTH DIRECTIONS ARE NOISY AND
-- NEITHER CAN BE DECIDED FOR THE OPERATOR. This is not the 00070 shape where the
-- rollback loses evidence; it loses CONFIGURATION, and unlike 00071's two dead
-- keys this configuration decides something the moment it is written:
--
--   * DROPPING `subject_kinds` WIDENS ROUTING. A policy narrowed to `{case}` starts
--     claiming alert-subject and digest-subject facts again, so notifications an
--     operator had deliberately excluded resume arriving in their channels. The
--     safe direction for data loss is the noisy direction for a human.
--   * DROPPING `count_min` REMOVES A FLOOR. Once the evaluator lands (see the
--     header), a policy that was staying quiet below its threshold begins speaking
--     on the first fact. Before the evaluator lands this drops an intention and
--     changes no delivery, which is the one window in which this rollback is cheap.
--
-- Neither is recoverable from anything the Up recorded: the columns ARE the record.
-- So the counts are stated and a human decides, rather than either blocking every
-- rollback or destroying an operator's routing in silence.
-- +goose StatementBegin
DO $$
DECLARE bound BIGINT; counted BIGINT; names TEXT;
BEGIN
  SELECT count(*) FILTER (WHERE cardinality(subject_kinds) > 0),
         count(*) FILTER (WHERE count_min IS NOT NULL)
    INTO bound, counted
    FROM notification_policies
   WHERE deleted_at IS NULL;

  SELECT string_agg(name::text, ', ' ORDER BY name) INTO names
    FROM notification_policies
   WHERE deleted_at IS NULL
     AND (cardinality(subject_kinds) > 0 OR count_min IS NOT NULL);

  IF bound > 0 OR counted > 0 THEN
    RAISE NOTICE 'migration 00072 down: destroying the subject binding on % live policy row(s) and the count condition on %. The columns ARE the record, so neither is recoverable afterwards. Dropping subject_kinds WIDENS routing -- a policy narrowed to one altitude claims all three again and notifications the operator excluded resume arriving -- and dropping count_min removes a floor. Affected policies: %. Record them with:  SELECT id, name, subject_kinds, count_min, count_window_s FROM notification_policies WHERE deleted_at IS NULL AND (cardinality(subject_kinds) > 0 OR count_min IS NOT NULL);  before continuing.',
      bound, counted, names;
  END IF;
END $$;
-- +goose StatementEnd

-- Dropped BY NAME, like every other constraint in this Down: the two rules added after
-- the first draft of this migration are no less droppable for having been added later,
-- and leaving either to `DROP COLUMN`'s cascade is the drop-order dependency 00065's
-- rule refuses.
ALTER TABLE notification_policies DROP CONSTRAINT policies_digest_subject_ck;
ALTER TABLE notification_policies DROP CONSTRAINT policies_count_case_ck;
ALTER TABLE notification_policies DROP CONSTRAINT policies_count_subject_ck;
ALTER TABLE notification_policies DROP CONSTRAINT policies_count_pair_ck;
ALTER TABLE notification_policies DROP CONSTRAINT policies_count_window_ck;
ALTER TABLE notification_policies DROP CONSTRAINT policies_count_min_ck;
ALTER TABLE notification_policies DROP COLUMN count_window_s;
ALTER TABLE notification_policies DROP COLUMN count_min;

ALTER TABLE notification_policies DROP CONSTRAINT policies_subjkinds_ck;
ALTER TABLE notification_policies DROP COLUMN subject_kinds;
