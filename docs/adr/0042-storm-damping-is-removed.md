# ADR 0042 — Storm damping is removed, because the defence was built before its object

- **Status**: Accepted
- **Date**: 2026-08-18
- **Resolves**: git-bug `a51a763` — *"Storm damping is a defence built before its object exists"*
- **Migration**: `00059_no_column_remembers_a_storm.sql`
- **Supersedes in part**: ADR 0020 **Amendment 1** — its "the set narrows to two" half stands and
  becomes final; its "the storm moves to the channel" half is deleted along with the storm. ADR 0020
  Constraint 3 ("broadcast must be damped during a storm") loses its subject.
- **Retires, does not delete**: `alert_events.type` values `group.storm_started` and
  `group.storm_ended`; `notifications.reason` value `storm`.
- **Amended by**:
  [Amendment 1](#amendment-1--the-storm-card-state-is-deleted-and-retirement-was-the-wrong-word-for-it)
  (2026-08-19, no migration) — the **Slack `Storm` card state, its `#7b1fa2` palette entry and the
  two view fields are DELETED, not retained.** §1 through §6 are untouched, and the retired STORED
  values in §7's first three paragraphs stand exactly as written.

> ⚠️ **§7's LAST paragraph is superseded.** It retained a card state, a hex and two struct fields on
> "the same terms as `refired`", and those terms do not reach them: `refired` is a value a stored row
> spells, and a card state is a reading taken at render time. What shipped is the deletion. See
> [Amendment 1](#amendment-1--the-storm-card-state-is-deleted-and-retirement-was-the-wrong-word-for-it),
> which states the boundary the paragraph crossed.

## 1. The decision

**Storm collapse is removed from oto.** Nothing withholds, downgrades, delays or announces a
notification because a group is busy. A burst of arrivals produces one notification per triggering
change, exactly as a trickle does.

The behaviour went first. `EvaluateStorm`, `ApplyStorm`, `claimStormNotice` and
`WarrantsChannelNotice` are deleted; `internal/notification/domain/mode.go:348` no longer drops a
reply on `in.StormMode`. What migration `00059` removes is the schema and the settings that were
left standing behind it: `alert_groups.storm_mode`, `alert_groups.storm_since`, `groups_storm_ck`,
`channels.storm_notice_at`, and the three org settings keys `storm_threshold`, `storm_window_s` and
`storm_cooldown_s`. `notifications_suppmap_ck` narrows from **eight** admitted values to **six**.

## 2. What storm collapse was

If more than `storm_threshold` (default 25) distinct alerts joined one AlertGroup generation inside
`storm_window` (default 60s), the generation entered `storm_mode`:

| it did this | and the effect was |
|---|---|
| posted and updated exactly ONE root message, carrying a count and a link | the members section on the card was replaced by "214 alerts in this group" |
| suppressed every per-alert thread reply | a `notifications` row with `status='suppressed'`, `suppressed_reason='storm'`, and no deliveries |
| broadcast one channel-level notice, behind a latch on `channels.storm_notice_at` whose window was `storm_cooldown_s` | the channel was told once that oto had started withholding, however many groups were storming (ADR 0020 Amendment 1) |
| ended after `storm_cooldown` (default 600s) with no new member | `group.storm_started` and `group.storm_ended` bracketed the episode on the timeline |

## 3. Why it was wrong, and not merely unused

**The defence had no object.** A storm is many *different* alerts arriving together. The thing that
owns many different alerts is an **Incident** — `correlation`, DEFERRED-POST-V1 (SPEC §I.1.1). The
damper was built before that object existed, so it had nowhere to put what it detected, and it put
the verdict in the notification layer instead: as a **withheld notification**. A detector with
nowhere to report becomes a damper, which is a different feature with a different failure mode.

**Damping made oto decide not to speak, and that is the one thing it may not do.** SPEC §B.6 states
the rule: a suppressed notification and a signal that never fired look identical to the person who
was not told. The collapse announced *itself* and called that visibility — but the thirty-nine
replies it withheld left no trace an operator could read, and the announcement said only that
something had been withheld, never what. oto may be quiet for a reason **outside its own judgement**
— nowhere to send, a human asking for less, a provider rate-limiting, nothing changed — and each of
those is a row with a reason and a place in the UI. Its own opinion that a real firing was not worth
mentioning is not on that list.

**Flooding a channel with two hundred real firings is a truthful report that something is badly
wrong.** That is the observation the whole ADR turns on. The flood was never the defect; it was the
signal.

The six suppression reasons `notifications_suppmap_ck` still admits make the argument by
composition: `channel_disabled` and `no_policy` mean there was nowhere to send, `snoozed` and
`verbosity` are a human asking for less, `throttled` is the world's rate limit, `duplicate_render` is
that nothing changed. **The two removed were the only two that were oto's own opinion about a
signal.**

## 4. What replaces it: nothing at the notification layer

There is no successor damper, no summarisation strategy and no "quiet mode". The levers that make
oto quieter are the ones that were always honest about being levers: `group_by` in
`alertmanager.yml`, a channel's `verbosity`, and **snooze** — a human asking, recorded and shown.

**The noise storm damping was reached for is handled one layer earlier, at case formation.** ADR 0041
Amendment 1 (migration `00057`) makes a Case stay open until its alert has *stayed* resolved for the
retention window **W**: an alert resolving and re-firing six times inside W produces one episode, one
root card and one reply instead of six. That is the same noise, removed by not creating it rather
than by refusing to report it — and it leaves nothing suppressed, because there was never a second
fact to suppress.

W does not address the burst case, and it is not meant to. Two hundred pods alerting at once are two
hundred true statements. **Storm's notification home is the Incident, once Incidents exist**, and
reintroducing it beside them is cheaper than carrying a half-feature that pretends the object is
already there.

## 5. What was refused

| refused | why |
|---|---|
| **Leave the knobs, set the threshold impossibly high** | A control that decides nothing is worse than no control: an operator who tunes it believes they have bought something. |
| **Keep the collapse, drop only the channel notice** | The notice was the only part that was visible. Removing it would leave silent withholding, which is strictly the worse half. |
| **Replace it with a digest of the withheld replies** | A digest is still oto deciding which firings are worth a line. It moves the judgement, it does not remove it. |
| **Move damping to the delivery layer as a rate limiter** | oto already has one, per conversation, and it is honest: `throttled` records that *the world* refused, not that oto declined. |
| **Retain the columns inert, as the previous pass did** | §6. It was the right call while the settings layer could CrashLoop an operator; that premise turned out to be false. |
| **Keep `alert_groups.storm_mode` as history** | It is not history. It is live state describing a generation right now, and a live-state column no writer can set again is a lie with a `NOT NULL DEFAULT false`. |

## 6. ⛔ One destructive migration, under an explicit no-installs authorisation

SPEC §D says **expand/contract only, never a destructive migration in one release.** `00059` drops
four schema objects and narrows a CHECK in one release. The exemption is recorded here because it is
not a general one.

The rule exists because release N and release N+1 run at the same time: a contract that lands before
every writer has stopped using the column takes down the release still writing it. **That premise is
falsifiable, and here it is false.** The owner has confirmed that **no oto database and no Helm
release exists anywhere outside a development laptop**, and the repository agrees on its own terms:
`git tag` is empty; `.github/workflows/release.yml` triggers only on
`push: tags: v[0-9]+.[0-9]+.[0-9]+*`, and it sets `flavor: latest=false` so no moving `latest` tag
exists to have been pulled by accident.

There is therefore no release N to be compatible with, no `orgs.settings` document in the world
carrying a `storm_*` key, and no `alert_groups` row with `storm_mode = true`. A three-migration
expand/backfill/contract dance to protect a deployment that does not exist would be ceremony, and it
would leave two more numbers of tombstone for the next reader to decode.

**The same emptiness is what reverses the previous ruling.** SPEC §B.6.1 previously retained the
three settings keys because deleting one makes `identity/domain/declarative.go` refuse an unknown key
**at BOOT**, and the storm knobs were documented Helm values — so an operator who had tuned storm
would CrashLoop on the next `helm upgrade`. That reasoning was sound and its subject does not exist.

⛔ **The narrowing fails loudly and rewrites nothing.** Migration 00018's rule is verbatim: an enum
narrowing with no downlevel mapping must FAIL rather than rewrite history. There is no honest value
to turn a stored `storm` or `flapping` suppression into, and inventing one would make the suppression
audit page report a reason that never applied. `00059` therefore carries **no** `UPDATE`;
`ADD CONSTRAINT` validates the existing rows, so a database that ever recorded either damper refuses
the migration with a `23514` naming the constraint, and the person holding it decides. On a laptop the
answer is `just reset`; there is no other holder.

**⛔ THE MOMENT A TAGGED RELEASE EXISTS THIS EXEMPTION IS SPENT.** The next removal of a settings key
or a CHECK value goes back to expand/contract, and this ADR is not a precedent for it.

## 7. The event types are retired, not deleted

`group.storm_started` and `group.storm_ended` stay in the closed `alert_events.type` enum (SPEC
§D.4.1), stay in `AllEventTypes()` and therefore stay in `components.schemas.AlertEventType`. They
are retired on migration 00051's exact terms, which ADR 0040 §7 restated for `case.reopened`:

**a value removed from the enum is a value `NewEventType` rejects on read, and a timeline that errors
instead of rendering.** `alerts/service.AppendTimelineEvent` refuses a retired type at the writer,
which is where "never again" is enforced rather than asserted. They leave the enum when the last
partition that could hold them is dropped.

`notifications.reason = 'storm'` is retired on the same principle and by a different mechanism:
`notification/domain.retiredReasons` (`reason.go:166`) keeps it decodable and `Valid`, and the mint
refuses it. **`notifications_reason_ck` is deliberately not touched by `00059`, and the asymmetry is
the point:** `reason` is what a stored row *says it was about* and a card must still render it;
`suppressed_reason` is oto's reading of *why nothing was sent*, which is exactly the judgement this
ADR removes. `SuppressedStorm` (`suppression.go:33`) is retired in Go for the case where a row
predates the narrowing — the schema no longer admits a new one.

⛔ **SUPERSEDED BY [Amendment 1](#amendment-1--the-storm-card-state-is-deleted-and-retirement-was-the-wrong-word-for-it).**
The paragraph below is kept because it records the ruling that was made and the reasoning that made
it wrong, and only its FIRST clause survives — the Slack `storm` reply is retained. Read it against
the amendment:

> The Slack `storm` reply (SPEC §H.5), the `Storm` card state (§H.4) and the `#7b1fa2` palette entry
> (§H.2) are retained on the same terms as `refired`: declared, renderable, produced by nothing.
> `channels/domain.GroupView.StormMode` and `NotificationView.StormCount` (§F.2) stay for the same
> reason and are retired with them — `notification/service/view.go:201` no longer populates either,
> so every view oto builds from now on reads `false` and `0`.

## 8. What this ADR does not decide

- **What a storm-shaped view looks like beside Incidents.** Correlation is DEFERRED-POST-V1 and this
  ADR does not design its notification behaviour. It only records that the notification layer is the
  wrong home for it, and why.
- **Whether flap damping survives in its current form.** ⛔ **ANSWERED — IT DOES NOT.** Recorded in
  [ADR 0041](0041-the-alert-case-allocation-and-the-rule-that-decides-it.md) Amendment 1 and SPEC
  §B.6.2, on 2026-08-19, the same day as this ADR: W makes the mechanism redundant AND makes the
  detector lie. `stateChangeCountsSQL` counts `case.opened`/`case.resolved`, and a flap absorbed
  inside W appends neither, so a damped episode contributes about two of the five transitions the
  threshold wanted — `is_flapping` read false exactly when an alert was flapping. `flap_score` and
  `is_flapping` are therefore RETIRED IN PLACE: `AlertRepository.SetFlap` was the only writer and is
  deleted, the `flap.score` job is gone, and both columns keep their last value and stay readable as
  history. Teaching the score to see was considered and refused. The text below was true when written
  and is kept because the question it left open is the one that got answered:
  > `flapping` left `notifications_suppmap_ck` with `storm`, so flap damping no longer records a
  > suppression; it switches `notify.evaluate` to update-only and emits one coalesced reply per
  > `flap_digest_interval`, and `alerts.is_flapping` stays a visible UI state. Whether W makes that
  > mechanism redundant is a real question and this ADR does not answer it.
- **What the ingest path should do at ten thousand alerts.** Shedding answers a 503 with a
  `Retry-After` (C17, ADR 0007) and that is unchanged. It is a statement about capacity, never about
  whether a signal deserved a mention.

## Amendment 1 — the Storm card state is DELETED, and "retirement" was the wrong word for it

- **Date**: 2026-08-19
- **Migration**: none. Nothing in this amendment touches the schema; `00059` already did the only
  destructive work storm's removal needs.
- **Amends**: §7's last paragraph, and SPEC §H.2, §H.4 and §F.2 with it.
- **Leaves untouched**: §1's decision, §2's account of what the collapse did, §3's argument, §4, §5,
  §6's migration exemption, and §7's first three paragraphs — the two `alert_events.type` values,
  `notifications.reason = 'storm'` and `SuppressedStorm` are retired exactly as stated there.

### §7 conflated a stored value with a live reading

**A retired STORED value must stay decodable, and every one of them does.** `notifications.reason`
still admits `storm` — `notifications_reason_ck` lists it (`00018_notification_reasons.sql:78`) and
`00059` deliberately does not narrow it — while `notification/domain.retiredReasons`
(`reason.go:166`) keeps it `Valid` on read and refuses it at the mint. The Slack renderer draws such
a row today: `reasonStorm` (`render/slack/reply.go:38`) names the reply, `replyLead` returns
`:zap: Storm damping on for:` (`reply.go:666`), and `reasonPhrase` still turns the value into the
words *storm damping* (`renderer.go:328`). `group.storm_started` and `group.storm_ended` stay in the
closed `alert_events.type` enum (`alerts/domain/event.go:115`, `:119`), in `retiredEventTypes`
(`:257`) and in `components.schemas.AlertEventType` (`openapi.yaml:5980`), so `NewEventType` parses a
thirteen-month-old row and an alert timeline renders rather than errors.

**The LIVE card state was unreachable BY CONSTRUCTION, which is a different category.** `CardStorm`
was never stored anywhere. It was computed at render time by `cardState`, from
`NotificationView.StormCount > 0` — and the only production writer of that field,
`notification/service.ViewService.project`, set it from `snap.Group.StormMode`, whose only source
was `alert_groups.storm_mode`. `00059` dropped the column, so the snapshot lost the field
(`notification/domain/snapshot.go:76`), the view service lost the assignment
(`notification/service/view.go:201`) and the repository lost the read
(`notification/repository/snapshot.go:612`). What was left was a branch no input could select.

**That is dead code, and this repository has a gate that says so.** `just lint-reachability`
(`tools/lintreach`) exists to fail on a declaration wired to nothing, and its baseline can only
shrink. A struct field with no writer and a switch arm with no reachable predicate are exactly what
it reads. So `CardStorm`, `#7b1fa2`, `GroupView.StormMode` and `NotificationView.StormCount` are
deleted — from `render/slack/palette.go`, `render/slack/renderer.go` and `channels/domain/view.go` —
and SPEC §H.2 is five rows, not six.

⭐ **The line is: does a ROW spell it?** `refired` and `storm` are reasons a stored `notifications`
row can literally contain, so decoding them is not optional and §H.5 keeps both. A card state, a
palette entry and a view field are how oto MAKES a card now; nothing on disk carries one. §7 reached
for `refired`'s terms because both things were "produced by nothing", and produced-by-nothing is not
the test — **readable-from-somewhere** is.

⚠️ **The Slack card's TRAIL lost its storm entries too, and by the same argument.** The two arms in
`render/slack/root.go` have no reachable input: `notification/service.trailKinds` no longer maps
either event type (`view.go:262`) and `groupTrailSQL` no longer selects them
(`notification/repository/snapshot.go:616`). The historical timeline that still renders a storm is
the **alert timeline**, which reads `alert_events` and decodes the retired types directly.
