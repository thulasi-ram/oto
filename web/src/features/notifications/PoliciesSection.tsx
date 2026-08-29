/**
 * Notification policies — matchers → channels → reasons.
 *
 * ⭐ THIS IS A DESTINATION NOW, NOT A SETTINGS SECTION (ADR 0034). It used to be
 * the third of five bands under `/settings`, filed beside access tokens and the
 * tuning knobs. Routing is not that kind of object: it is edited on the same
 * question the alert list is read on ("did anyone hear about this, and if not,
 * why not"), and it has a sibling — the activity log — that is pure operational
 * reading and would have been absurd under a gear icon. Both now live under
 * `/notifications`.
 *
 * A policy decides **whether** and **where**, never how a message is rendered.
 * That separation is why the form has no formatting controls: rendering belongs
 * to the channel's renderer, and offering it here would imply a coupling that
 * does not exist.
 *
 * The dry run is the point of the screen. "Given this alert, who is told, where,
 * and rendered how" is answerable *before* saving, against an unsaved draft,
 * using the real matcher and the real renderer — and it sends nothing. A routing
 * rule you cannot test is a routing rule you find out about during an incident.
 */
import {
  For,
  Match,
  Show,
  Switch,
  createEffect,
  createMemo,
  createSignal,
  type Component,
  type JSX,
} from "solid-js";
import { useMutation, useQuery, useQueryClient } from "@tanstack/solid-query";
import * as v from "valibot";

import {
  enumValuesOf,
  maxLengthOf,
  maxValueOf,
  minLengthOf,
  minValueOf,
  rangeOf,
  type Range,
} from "~/api/bounds";
import { violationsByField } from "~/api/client";
import {
  createChannel,
  createPolicy,
  deletePolicy,
  previewPolicy,
  resolveSlackConversation,
  updateChannel,
  updatePolicy,
} from "~/api/endpoints";
import {
  CreatePolicyRequestSchema,
  MatcherDTOSchema,
  NotificationReasonSchema,
  ThrottleDTOSchema,
  UuidSchema,
  VerbositySchema,
} from "~/api/generated/validators";
import { qk } from "~/api/keys";
import {
  channelConnectionsQuery,
  channelsQuery,
  channelTypesQuery,
  notificationTemplatesQuery,
  policiesQuery,
  recentAlertsQuery,
} from "~/api/queries";
import type {
  Alert,
  Channel,
  ChannelConnection,
  ChannelTypeDescriptor,
  CreatePolicyRequest,
  Matcher,
  NotificationReason,
  NotificationSuppressedReason,
  Policy,
  PolicyPreview,
  Verbosity,
} from "~/api/types";
import { Button } from "~/components/ui/Button";
import { Checkbox } from "~/components/ui/Checkbox";
import {
  Combobox,
  ComboboxContent,
  ComboboxControl,
  ComboboxInput,
  ComboboxItem,
  ComboboxLabel,
  ComboboxTrigger,
} from "~/components/ui/Combobox";
import {
  Modal,
  ModalContent,
  ModalDescription,
  ModalFooter,
  ModalHeader,
  ModalTitle,
} from "~/components/ui/Modal";
import {
  Select,
  SelectContent,
  SelectErrorMessage,
  SelectHiddenSelect,
  SelectItem,
  SelectLabel,
  SelectTrigger,
  SelectValue,
} from "~/components/ui/Select";
import { Chip, PageHeading, Panel } from "~/components/ui/surfaces";
import { ErrorBanner, ErrorState, LoadingLine, PageEmptyState } from "~/components/ui/states";
import {
  TextField,
  TextFieldDescription,
  TextFieldErrorMessage,
  TextFieldInput,
  TextFieldLabel,
} from "~/components/ui/TextField";
import { ToggleGroup, ToggleGroupItem } from "~/components/ui/ToggleGroup";
import { cn } from "~/lib/cn";
import { duration, idempotencyKey } from "~/lib/format";
import { formatMatchers, parseMatchers } from "~/lib/matchers";
import { MatcherInput } from "~/features/alerts/MatcherInput";
import { SchemaForm } from "~/features/settings/SchemaForm";
import {
  cleanConfig,
  initialConfig,
  readFields,
  validateConfig,
  type JsonValue,
} from "~/features/settings/jsonSchema";

/*
 * The form rhythm is `features/settings`', and the import path is the only thing
 * left of this screen's old address. It is deliberately shared rather than
 * copied: this dialog and the channel dialog sit two clicks apart and are the
 * same object drawn twice, so a second set of gap constants here would be the
 * exact drift `rhythm.ts` was written to end.
 */
import {
  CHECK_LABEL,
  CHECK_ROW,
  FIELD,
  FIELD_ROW,
  FORM,
  HELP,
  LABEL,
  LEGEND,
  ADD_FULL,
  CARD_LIST,
  PANEL_BODY,
  SECTION,
  SECTION_HEAD,
  SECTION_LEDE,
} from "~/features/settings/rhythm";

/**
 * Every fact a policy can choose to communicate — READ from the contract's own
 * enum rather than re-typed from it.
 *
 * ⛔ THIS LIST USED TO BE EIGHTEEN LITERALS. A copy can only ever be right about
 * the day it was written: a reason the server ADDS is one an operator silently
 * cannot select, and there is nothing on this screen that would look wrong. The
 * picklist below is the same object the request schema validates `reasons`
 * against, so the two cannot disagree by construction.
 */
const REASONS: readonly NotificationReason[] = NotificationReasonSchema.options;

/* -------------------------------------------------------------------------- */
/* The contract's bounds, read rather than repeated                           */
/* -------------------------------------------------------------------------- */

/**
 * ⛔ NOT ONE OF THESE NUMBERS IS WRITTEN HERE. The priority box shipped with no
 * `min`/`max` at all while the contract says 0–10000, and the four list and text
 * limits were unenforced — so the first thing an operator learned about any of
 * them was a 422 with the dialog still full of their work. They come off
 * `CreatePolicyRequestSchema`, which is generated from `api/openapi/openapi.yaml`
 * by gate G4 and is the very schema the request is gated through below.
 */
const NAME_MAX = maxLengthOf(CreatePolicyRequestSchema, "name");
const PRIORITY_MIN = minValueOf(CreatePolicyRequestSchema, "priority");
const PRIORITY_MAX = maxValueOf(CreatePolicyRequestSchema, "priority");
const MATCHERS_MAX = maxLengthOf(CreatePolicyRequestSchema, "matchers");
const REASONS_MIN = minLengthOf(CreatePolicyRequestSchema, "reasons");
const REASONS_MAX = maxLengthOf(CreatePolicyRequestSchema, "reasons");
const CHANNELS_MIN = minLengthOf(CreatePolicyRequestSchema, "channel_ids");
const CHANNELS_MAX = maxLengthOf(CreatePolicyRequestSchema, "channel_ids");

const PRIORITY_RANGE = `oto accepts ${PRIORITY_MIN}–${PRIORITY_MAX}. Lower is evaluated first.`;

/**
 * Which ALTITUDE of fact a policy is about — `alert`, `case` or `digest` — read
 * off the contract rather than re-typed.
 *
 * ⛔ THE CONTRACT STATES THIS PICKLIST **INLINE** ON THE PROPERTY, WHICH IS THE
 * ONLY REASON THIS IS AN ACCESSOR CALL AND NOT AN IMPORT. `NotificationReason` is
 * emitted as its own `NotificationReasonSchema`, so `REASONS` below can simply be
 * that object's `.options`; `subject_kinds` has no named component and therefore
 * no name to import, which leaves a screen exactly two choices — read it off the
 * request schema, or type the three words out again. `bounds.ts` documents this as
 * the case `enumValuesOf` exists for, and typing them out again is how this file
 * came to offer a reason the server answered 422 for.
 */
const SUBJECT_KINDS = enumValuesOf(CreatePolicyRequestSchema, "subject_kinds");
type SubjectKind = (typeof SUBJECT_KINDS)[number];

const SUBJECT_KINDS_MAX = maxLengthOf(CreatePolicyRequestSchema, "subject_kinds");

/**
 * One member of a contract-derived list, by value, or a throw naming what was
 * missing.
 *
 * ⭐ THIS IS WHAT LETS THE THREE CROSS-FIELD RULES BELOW NAME A VALUE WITHOUT
 * HAND-COPYING ONE. `"case"` and `"digest"` are load-bearing in the server's own
 * rules — a count condition must bind exactly `case`, a digest window must bind
 * `digest` and list the `digest` reason — so the screen has to be able to say the
 * words. Saying them as bare literals would compile forever after a rename and
 * silently stop enforcing anything; looking them up in the list the contract
 * published fails at import instead, which is where somebody sees it.
 */
function member<T extends string>(list: readonly T[], want: string): T {
  const found = list.find((value) => value === want);
  if (found === undefined) {
    throw new Error(
      `oto: the contract's list no longer contains \`${want}\`. The policy editor has a rule ` +
        `written in terms of it, and a screen must not guess what replaced it.`,
    );
  }
  return found;
}

/** The altitude a count condition must bind, and the one a digest is minted at. */
const CASE_KIND = member(SUBJECT_KINDS, "case");
const DIGEST_KIND = member(SUBJECT_KINDS, "digest");
/** The one fact a policy with a digest window must also carry. */
const DIGEST_REASON = member(REASONS, "digest");

const COUNT_MIN_RANGE = rangeOf(CreatePolicyRequestSchema, "count_min");
const COUNT_WINDOW_RANGE = rangeOf(CreatePolicyRequestSchema, "count_window_seconds");
const DIGEST_WINDOW_RANGE = rangeOf(CreatePolicyRequestSchema, "digest_window_seconds");
const DIGEST_FLOOR_RANGE = rangeOf(CreatePolicyRequestSchema, "digest_floor");
const THROTTLE_MAX_RANGE = rangeOf(ThrottleDTOSchema, "max");
const THROTTLE_WINDOW_RANGE = rangeOf(ThrottleDTOSchema, "window_seconds");

/**
 * Every digest window the server will actually accept, COMPUTED from the two
 * numbers the contract does publish.
 *
 * ⛔ THE DIVISOR RULE IS NOT EXPRESSIBLE IN JSON SCHEMA, AND THE CONTRACT SAYS SO
 * IN WRITING: *"this schema states the range only and a window that is in range
 * but not a divisor comes back as a 422"*. That sentence describes the exact bug
 * this screen is named after — a rule an operator learns by having their work
 * refused after Save — so the rule is reproduced here instead of imported. What is
 * NOT reproduced is a number: the alignment period is `DIGEST_WINDOW_RANGE.max`,
 * because the domain declares them the same fact (`MaxDigestWindow` is one day
 * *"which is also the alignment period: every admissible window divides it"*,
 * `internal/notification/domain/digest.go`). Move the ceiling and this list moves
 * with it.
 *
 * ⚠️ IT IS NOT A PICKLIST, DELIBERATELY. 86400 has 96 divisors and roughly half of
 * them clear the floor, so a `<Select>` of them would be a fifty-row menu of
 * numbers like 432 and 5400 that nobody wants — worse than the number box it
 * replaced. It is used to say which two admissible windows sit either side of a
 * rejected one, which is the part an operator cannot work out in their head.
 */
const ALIGNED_DIGEST_WINDOWS: readonly number[] = (() => {
  const day = DIGEST_WINDOW_RANGE.max;
  const found = new Set<number>();
  for (let d = 1; d * d <= day; d += 1) {
    if (day % d !== 0) continue;
    for (const w of [d, day / d]) {
      if (w >= DIGEST_WINDOW_RANGE.min && w <= day) found.add(w);
    }
  }
  return [...found].sort((a, b) => a - b);
})();

function digestWindowAligned(seconds: number): boolean {
  return Number.isInteger(seconds) && seconds > 0 && DIGEST_WINDOW_RANGE.max % seconds === 0;
}

/** The admissible windows either side of one that is not, for the refusal message. */
function nearestAlignedWindows(seconds: number): readonly number[] {
  const below = [...ALIGNED_DIGEST_WINDOWS].reverse().find((w) => w < seconds);
  const above = ALIGNED_DIGEST_WINDOWS.find((w) => w > seconds);
  return [below, above].filter((w): w is number => w !== undefined);
}

/**
 * What the dialog holds, before it is anything the API has a name for.
 *
 * ⭐ THE SIX OPTIONAL NUMBERS ARE FLAT AND CARRY THE WIRE'S OWN NAMES, WHICH IS
 * NOT A COSMETIC CHOICE. `localError` finds a complaint by `path[0].key` and
 * `violationsByField` keys the server's violations by their `field` — and the
 * server names exactly these: `count_min`, `count_window_seconds`,
 * `digest_window_seconds`, `digest_floor`, `subject_kinds`
 * (`internal/notification/domain/policy.go`). Nesting them into a `count: {…}`
 * object would give the local complaint a path the server's violation could never
 * land on, so the same rule would point at the control from one side and at
 * nothing from the other.
 *
 * `null` is OFF for every one of them, and it is off by construction rather than
 * by validation: the controls are revealed by a checkbox that sets both halves of
 * a pair at once, so the "one half without the other" shapes the server refuses
 * (`policies_count_pair_ck`, `policies_digest_pair_ck`, the throttle's
 * `incomplete`) cannot be built here in the first place. The checks below still
 * state them, because a policy is also loaded FROM the server and a form that only
 * enforces what its own widgets can produce is one refactor from enforcing nothing.
 */
interface PolicyForm {
  readonly name: string;
  readonly priority: number;
  readonly enabled: boolean;
  readonly matchers: readonly Matcher[];
  readonly reasons: readonly NotificationReason[];
  readonly channel_ids: readonly string[];
  /** "" is oto's own card, which is what a policy that names no template gets. */
  readonly template_id: string;
  /** Empty is EVERY altitude — the contract's default, and never `null`. */
  readonly subject_kinds: readonly SubjectKind[];
  readonly throttle_max: number | null;
  readonly throttle_window_seconds: number | null;
  readonly count_min: number | null;
  readonly count_window_seconds: number | null;
  readonly digest_window_seconds: number | null;
  readonly digest_floor: number | null;
}

function toCreatePolicyRequest(form: PolicyForm): CreatePolicyRequest {
  return {
    name: form.name.trim(),
    priority: form.priority,
    enabled: form.enabled,
    matchers: form.matchers.map((m) => ({ name: m.name, op: m.op, value: m.value })),
    reasons: [...form.reasons],
    channel_ids: [...form.channel_ids],
    // ⛔ OMITTED RATHER THAN SENT AS null. On create the contract's `template_id`
    // is a plain optional uuid: absent means oto's own card. `null` is meaningful
    // only on the PATCH, where it CLEARS a template the policy already had. The
    // five nullable numbers below follow the same rule for the same reason; the
    // patch adds the explicit `null`s back in one place, in the mutation.
    ...(form.template_id === "" ? {} : { template_id: form.template_id }),
    // ⛔ ALWAYS SENT, EVEN EMPTY. `subject_kinds` is the one new field that is not
    // nullable anywhere: "claims every altitude" is an ANSWER rather than an
    // absence, which is why the column is `NOT NULL DEFAULT '{}'` and why the
    // response DTO marks it required. Omitting it when empty would be the same
    // statement, but it would make the create body and the patch body disagree
    // about a field that has no `null` to disagree with.
    subject_kinds: [...form.subject_kinds],
    ...(form.throttle_max !== null && form.throttle_window_seconds !== null
      ? { throttle: { max: form.throttle_max, window_seconds: form.throttle_window_seconds } }
      : {}),
    ...(form.count_min === null ? {} : { count_min: form.count_min }),
    ...(form.count_window_seconds === null
      ? {}
      : { count_window_seconds: form.count_window_seconds }),
    ...(form.digest_window_seconds === null
      ? {}
      : { digest_window_seconds: form.digest_window_seconds }),
    ...(form.digest_floor === null ? {} : { digest_floor: form.digest_floor }),
  };
}

/**
 * One optional whole number, bounded by the contract and off when `null`.
 *
 * The sentences name the control and state the range, because "invalid" in a
 * dialog with six numbers in it is a scavenger hunt. `NaN` — what an unreadable
 * box parses to — fails `minValue` rather than `number`, which is the same route
 * the priority box has always taken.
 */
function optionalWhole(range: Range, what: string) {
  const stated = `${what} is ${range.min}–${range.max}.`;
  return v.nullable(
    v.pipe(
      v.number(`${what} is a whole number.`),
      v.integer(`${what} is a whole number.`),
      v.minValue(range.min, stated),
      v.maxValue(range.max, stated),
    ),
  );
}

/*
 * SPEC §L.8.1: the form schema stays hand-written — the sentences below are the
 * whole point of it — but it `v.pipe`s into the **generated**
 * `CreatePolicyRequestSchema` as its final gate. That last line is what makes
 * the difference between a form that agrees with the contract today and a form
 * that cannot construct a body the API would refuse. The draft used to be sent
 * raw.
 */
const PolicyFormSchema = v.pipe(
  v.strictObject({
    name: v.pipe(
      v.string(),
      v.trim(),
      v.minLength(1, "A policy needs a name — it is how it is referred to everywhere else."),
      v.maxLength(NAME_MAX, `A name is at most ${NAME_MAX} characters.`),
    ),
    priority: v.pipe(
      v.number("Priority is a whole number."),
      v.integer("Priority is a whole number."),
      v.minValue(PRIORITY_MIN, PRIORITY_RANGE),
      v.maxValue(PRIORITY_MAX, PRIORITY_RANGE),
    ),
    enabled: v.boolean(),
    matchers: v.pipe(
      v.array(MatcherDTOSchema),
      v.maxLength(MATCHERS_MAX, `At most ${MATCHERS_MAX} matchers on one policy.`),
    ),
    reasons: v.pipe(
      v.array(NotificationReasonSchema),
      v.minLength(
        REASONS_MIN,
        "Pick at least one fact. A policy that communicates nothing is a policy that matches and then stays silent.",
      ),
      v.maxLength(REASONS_MAX, `At most ${REASONS_MAX} facts on one policy.`),
    ),
    channel_ids: v.pipe(
      v.array(UuidSchema),
      v.minLength(
        CHANNELS_MIN,
        "Pick at least one channel. A policy with nowhere to send records every notification as suppressed.",
      ),
      v.maxLength(CHANNELS_MAX, `At most ${CHANNELS_MAX} channels on one policy.`),
    ),
    // "" is oto's own card and is the shipped default; anything else must be a
    // uuid, because the only other thing it can be is a template id.
    template_id: v.union([v.literal(""), UuidSchema]),
    subject_kinds: v.pipe(
      v.array(v.picklist(SUBJECT_KINDS)),
      v.maxLength(
        SUBJECT_KINDS_MAX,
        `There are only ${SUBJECT_KINDS_MAX} altitudes, and naming all of them is what leaving this empty already says.`,
      ),
    ),
    throttle_max: optionalWhole(THROTTLE_MAX_RANGE, "The ceiling"),
    throttle_window_seconds: optionalWhole(THROTTLE_WINDOW_RANGE, "The throttle window"),
    count_min: optionalWhole(COUNT_MIN_RANGE, "The count threshold"),
    count_window_seconds: optionalWhole(COUNT_WINDOW_RANGE, "The count window"),
    digest_window_seconds: optionalWhole(DIGEST_WINDOW_RANGE, "The digest window"),
    digest_floor: optionalWhole(DIGEST_FLOOR_RANGE, "The digest floor"),
  }),

  /*
   * The cross-field rules, each forwarded to THE CONTROL THE SERVER WOULD NAME.
   *
   * ⛔ EVERY ONE OF THESE IS A 422 THE OPERATOR WOULD OTHERWISE MEET AFTER SAVE,
   * which is the defect this whole screen is named after. They are transcriptions
   * of `Policy.Validate` — `policies_count_pair_ck`, `policies_count_case_ck`,
   * `policies_digest_pair_ck`, `policies_digest_reason_ck`, the alignment rule and
   * the throttle's `incomplete` — and the forwarded path is the server's own
   * `Violation.Field` in each case, so a rule that fires locally and a rule that
   * fires on the wire light the same control.
   *
   * ⛔ ONE SERVER RULE IS DELIBERATELY ABSENT: the general coherence check, which
   * refuses a binding admitting NONE of the policy's declared reasons. Evaluating
   * it needs the total Reason → SubjectKind allocation, and that map is NOT
   * published — the contract states `subject_kinds` and `NotificationReason` as two
   * independent picklists and nowhere relates them. Reproducing it here would mean
   * hand-copying fifteen pairs out of `internal/notification/domain/reason.go`,
   * which is precisely the kind of copy this file's header exists to forbid: it
   * would be wrong the day a Reason is added and would go on looking right. So the
   * server owns that one, and its violation lands on `subject_kinds` where the
   * control is.
   *
   * ⭐ THE THREE RULES THAT DO NAME A VALUE ARE SAFE FOR A REASON WORTH KEEPING
   * STRAIGHT: each is a rule ABOUT A FIELD that the contract states in its own
   * prose — a count binds `case`, a digest window binds `digest` and lists the
   * `digest` fact — rather than a fact derived from the allocation map. And each
   * gets its value from `member()`, which looks it up in a list the contract DID
   * publish and throws at import if it is not there.
   */
  v.forward(
    v.check(
      (f) => (f.throttle_max === null) === (f.throttle_window_seconds === null),
      "A throttle needs both a ceiling and a window, or neither.",
    ),
    ["throttle_max"],
  ),
  v.forward(
    v.check(
      (f) => (f.count_min === null) === (f.count_window_seconds === null),
      "A count condition needs both a threshold and a window: a threshold over an unbounded span is not something anything can evaluate.",
    ),
    ["count_min"],
  ),
  v.forward(
    v.check(
      (f) =>
        f.count_min === null ||
        (f.subject_kinds.length === 1 && f.subject_kinds[0] === CASE_KIND),
      // Lazy for the reason the digest-fact message below is: the label maps are
      // declared under this schema, and the sentence has to name the chip the
      // operator can actually see rather than the wire token behind it.
      () =>
        `A count condition counts firings, so it must be about exactly one altitude — “${SUBJECT_LABEL[CASE_KIND]}”. An alert's subject is its identity and does not change when it fires again, so counting those would never pass a threshold above one and would mute this policy permanently; a digest is minted against its own floor and never reads this number at all.`,
    ),
    ["subject_kinds"],
  ),
  v.forward(
    v.check(
      (f) => f.digest_floor === null || f.digest_window_seconds !== null,
      "A digest floor needs a digest window: a threshold over an unbounded span is not something anything can evaluate.",
    ),
    ["digest_floor"],
  ),
  v.forward(
    v.check(
      (f) => {
        const w = f.digest_window_seconds;
        // Only the alignment arm: the range is already stated on the field
        // itself, and complaining twice about one number is how a dialog gets
        // ignored.
        if (w === null || !Number.isFinite(w)) return true;
        if (w < DIGEST_WINDOW_RANGE.min || w > DIGEST_WINDOW_RANGE.max) return true;
        return digestWindowAligned(w);
      },
      // ⭐ THE MESSAGE NAMES THE TWO WINDOWS EITHER SIDE, because "must divide the
      // day evenly" is a rule an operator cannot apply in their head at 3am. It is
      // a function so it can read the value that was rejected.
      (issue) => {
        // `issue.input` is the whole form: `v.check` runs against the object and
        // `v.forward` only rewrites the PATH it is reported under, never the input.
        const asked = (issue.input as PolicyForm | undefined)?.digest_window_seconds ?? 0;
        const near = nearestAlignedWindows(asked);
        const suggestion =
          near.length === 0 ? "" : ` The nearest that do are ${near.join(" and ")}.`;
        return (
          `The digest window must divide the day evenly, so that every boundary is a wall-clock ` +
          `boundary in UTC.${suggestion}`
        );
      },
    ),
    ["digest_window_seconds"],
  ),
  v.forward(
    v.check(
      (f) =>
        f.digest_window_seconds === null ||
        f.subject_kinds.length === 0 ||
        f.subject_kinds.includes(DIGEST_KIND),
      () =>
        `A policy with a digest window must be about the “${SUBJECT_LABEL[DIGEST_KIND]}” altitude, or about every altitude: its digests are minted there, so a binding that omits it routes none of them.`,
    ),
    ["subject_kinds"],
  ),
  v.forward(
    v.check(
      (f) => f.digest_window_seconds === null || f.reasons.includes(DIGEST_REASON),
      // Lazy, because `REASON_LABEL` is declared below this schema and reading it
      // eagerly here would be a temporal-dead-zone throw at import.
      () =>
        `A policy with a digest window must also carry the “${REASON_LABEL[DIGEST_REASON]}” fact, or its digests would be recorded as suppressed once per window, forever.`,
    ),
    ["reasons"],
  ),
  // The annotation matters: `CreatePolicyRequest` marks the two defaulted keys
  // required (openapi-typescript fills a default in), while the generated
  // schema's *input* leaves them optional. Same contract, two honest readings
  // of it — so the transform is declared in the schema's own terms.
  v.transform((form): v.InferInput<typeof CreatePolicyRequestSchema> => toCreatePolicyRequest(form)),
  CreatePolicyRequestSchema, // the generated schema is the final gate
);

/**
 * The same enum as `./vocabulary`'s `REASON_LABEL`, phrased differently on
 * purpose — and the difference is not cosmetic.
 *
 * These label things a policy MAY BE TOLD ABOUT: they are read as the text of a
 * toggle ("rule changed", pressed or not). `vocabulary.ts` labels the same enum
 * as things that HAVE HAPPENED, read beside a timestamp ("the rule changed").
 * One map cannot be right in both places, and if one had to be wrong it must not
 * be this one — here the label is the entire explanation of what the control
 * does. See that module's header for the full argument.
 */
const REASON_LABEL: Record<NotificationReason, string> = {
  fired: "started firing",
  all_resolved: "all resolved",
  repeat: "repeat",
  suppressed: "suppressed upstream",
  unsuppressed: "no longer suppressed",
  expired: "expired",
  refired: "fired again",
  acked: "acknowledged",
  unacked: "acknowledgement withdrawn",
  // The only two reasons a snooze does not itself suppress (§B.8.4): a damper
  // that cannot announce itself is a silent mute.
  snoozed: "snoozed",
  unsnoozed: "snooze ended",
  enriched: "enrichment arrived",
  rule_changed: "rule changed",
  comment: "comment added",
  // ⛔ `unacked_reminder` WAS HERE AND IS GONE FROM THE ENUM (git-bug bd0fb1d,
  // migration 00067). It was oto's own timer on an unacknowledged signal — ONE
  // stage that ended at a channel, with no ladder, no rota and no notion of who
  // was next, which is why it was never called what the rest of the industry
  // calls it. The owner withdrew it: oto sends nothing unprompted. `escalation`
  // remains a banned word (§A.1) for the reason it always was.
  // There is deliberately no `severity_raised` here: `severity` is an ordinary
  // Prometheus label and is hashed into `alert_key`, so two severities of one
  // rule are two Alerts rather than one Alert changing. Nothing can observe a
  // rise, so nothing could ever write it (openapi `NotificationReason`).
  //
  // ⛔ `storm` WAS HERE, LABELLED "(retired)", AND IS NOW GONE FROM THE ENUM
  // ENTIRELY (ADR 0042, migration `00060`). Keeping a selectable-but-inert value
  // was the honest reading of ADR 0042 §5 while `notifications_reason_ck` still
  // admitted the reason and a stored policy could still carry it. 00060 narrows
  // that CHECK and `policies_reasons_ck` follows the enum down to eighteen, so
  // neither a notification nor a policy can spell it. `REASONS` is still the
  // contract's own picklist and is still not hand-filtered: the value left the
  // contract, so it left the picklist by itself.
  // ⭐ THE WINDOW FACT, AND FOR A POLICY WITH A WINDOW IT IS NOT OPTIONAL. A
  // digest is one message about a window over a namespace rather than about any
  // object: at each closed boundary a tick counts the cases that opened inside
  // the window and sends if the count clears the policy's floor (migration
  // `00058`). The server refuses a policy that sets a digest window without this
  // fact selected — its digests would be recorded as suppressed `no_policy`, once
  // per window, forever (`policies_digest_reason_ck`, `Policy.Validate`). It
  // damps nothing: a policy with a window sends the digest IN ADDITION to
  // whatever else it routes.
  digest: "window summary",
};

/**
 * The three altitudes, in words an operator can pick between without knowing the
 * Reason-to-subject map by heart — which is the second of the two jobs
 * `subject_kinds` exists to do (the first is being the count condition's unit).
 *
 * ⛔ TYPED `Record<SubjectKind, string>` AGAINST THE CONTRACT-DERIVED UNION, for
 * the reason `SUPPRESSED_REASON` is: a fourth altitude added server-side becomes a
 * build failure here rather than a chip with no words on it. There *was* a fourth
 * — `alert_group` — until migration `00069` deleted it.
 */
const SUBJECT_LABEL: Record<SubjectKind, string> = {
  // The identity of the label set. True whether or not anything is firing, which
  // is why a snooze and a comment live here and an acknowledgement does not.
  alert: "the alert itself",
  // One firing episode. `acked` is the reason this altitude had to exist: a claim
  // projected onto the identity outlives the firing it was about.
  case: "one firing",
  // The only altitude that is not a row in the signal graph — a window over a
  // namespace, minted by the tick rather than by anything that happened.
  digest: "a window",
};

const SUBJECT_HELP: Record<SubjectKind, string> = {
  alert: "suppressed, snoozed, commented on",
  case: "started, acknowledged, resolved, enriched, rule changed",
  digest: "the periodic summary",
};

export const PoliciesSection: Component = () => {
  const [editing, setEditing] = createSignal<Policy | null>(null);
  const [creating, setCreating] = createSignal(false);

  const policies = useQuery(() => policiesQuery());
  const channels = useQuery(() => channelsQuery());

  const byId = createMemo(() => {
    const map = new Map<string, Channel>();
    for (const c of channels.data?.data ?? []) map.set(c.id, c);
    return map;
  });

  return (
    <div class={SECTION}>
      {/* ⭐ THE HEADING WEARS NO BORDER. It used to be a `PanelTitle` inside a
          `PanelHeader`, which put a box around the section's own name and made
          every policy under it a band in one long frame. A heading is page
          furniture; the border belongs on the policies, where it separates one
          rule from the next. */}
      <header class={SECTION_HEAD}>
        <PageHeading brush="swipe">Notification policies</PageHeading>
        <p class={SECTION_LEDE}>
          A policy decides who hears about a fact and who does not. With no policy at all every
          notification is recorded as suppressed with reason{" "}
          <code class="font-mono">no_policy</code> — oto keeps the whole history and tells nobody,
          which is a choice worth making on purpose rather than by omission.
        </p>
      </header>

      <Switch>
        <Match when={policies.isPending}>
          <LoadingLine />
        </Match>
        <Match when={policies.isError}>
          <ErrorState error={policies.error} onRetry={() => void policies.refetch()} />
        </Match>
        <Match when={(policies.data?.data.length ?? 0) === 0}>
          {/* ⛔ THE EMPTY STATE CARRIES NO ACTION OF ITS OWN, because the add
              control below is on screen at the same time. Two buttons saying
              "Add a policy" a hundred pixels apart is one button too many, and
              the one inside the empty state would move when the list filled. */}
          <PageEmptyState
            motif="kumo"
            title="No policies."
            body="Nothing is being told to anybody yet. Every notification oto would have sent is on the record under Activity, marked `no_policy`."
          />
        </Match>
        <Match when={true}>
          {/* ⭐ ONE CARD PER POLICY. A policy is a rule with six or seven facts
              stacked under it — when, tell, about, at, at most, once it has
              happened, and one summary every — and a hairline between two of
              those stacks is not enough to say where one stops. */}
          <ul class={CARD_LIST}>
            <For each={policies.data?.data ?? []}>
              {(p) => (
                <PolicyRow policy={p} channels={byId()} onEdit={() => setEditing(p)} />
              )}
            </For>
          </ul>
        </Match>
      </Switch>

      {/* Below the list on purpose — see `ADD_FULL`. It is rendered through
          every state including the error one: a failed READ says nothing about
          whether a WRITE would work, and a screen that hid its only action
          behind a fetch failure would strand an operator on a retry button. */}
      <Button variant="outline" class={ADD_FULL} onClick={() => setCreating(true)}>
        + Add a policy
      </Button>

      <PolicyDialog
        open={creating() || editing() !== null}
        policy={editing()}
        channels={channels.data?.data ?? []}
        onClose={() => {
          setCreating(false);
          setEditing(null);
        }}
      />
    </div>
  );
};

/* -------------------------------------------------------------------------- */

const PolicyRow: Component<{
  readonly policy: Policy;
  readonly channels: ReadonlyMap<string, Channel>;
  readonly onEdit: () => void;
}> = (props) => {
  const client = useQueryClient();
  const p = (): Policy => props.policy;

  const remove = useMutation(() => ({
    mutationFn: () => deletePolicy(p().id),
    onSuccess: () => void client.invalidateQueries({ queryKey: qk.settings.policies() }),
  }));

  return (
    <li>
      {/* ⛔ THE CARD IS THE `<Panel>` AND THE `<li>` IS BARE. A border on the list
          item and a border on a panel inside it would draw the same rectangle
          twice, one pixel apart, which is exactly what stacking the two produces
          — so the list contributes the gap and the panel contributes the box. */}
      <Panel class={cn(PANEL_BODY, "flex flex-col gap-sm", p().enabled ? "" : "opacity-60")}>
        <div class="flex min-h-8 flex-wrap items-center gap-sm">
          <span class="text-item font-medium text-ink">{p().name}</span>
          <Chip title="Lower is evaluated first.">priority {p().priority}</Chip>
          <Show when={!p().enabled}>
            <Chip>disabled</Chip>
          </Show>
          <div class="ml-auto flex items-center gap-sm">
            <Button size="sm" variant="secondary" onClick={props.onEdit}>
              Edit
            </Button>
            <Button size="sm" variant="destructive" busy={remove.isPending} onClick={() => remove.mutate()}>
              Remove
            </Button>
          </div>
        </div>

        <div class="flex flex-col gap-2xs text-meta text-ink-muted">
          <p>
            <span class="text-ink-subtle">when</span>{" "}
            <code class="font-mono text-ink">
              {p().matchers.length === 0
                ? "anything"
                : formatMatchers(
                    p().matchers.map((m) => ({ name: m.name, op: m.op, value: m.value })),
                  )}
            </code>
          </p>
          <p>
            <span class="text-ink-subtle">tell</span>{" "}
            {p().channel_ids.length === 0
              ? "nobody"
              : p()
                  .channel_ids.map((id) => props.channels.get(id)?.name ?? "a removed channel")
                  .join(", ")}
          </p>
          <p>
            <span class="text-ink-subtle">about</span>{" "}
            {p().reasons.map((r) => REASON_LABEL[r] ?? r).join(", ")}
          </p>
          {/* The binding, on its own line and only when it narrows something. An
              empty binding is every altitude, and a row that said "at every
              altitude" on every policy would be a word the eye learns to skip.
              Inline after the facts it read as one run-on clause — "about started
              firing, all resolved at one firing". */}
          <Show when={p().subject_kinds.length > 0}>
            <p>
              <span class="text-ink-subtle">at</span>{" "}
              {p()
                .subject_kinds.map((k) => SUBJECT_LABEL[k] ?? k)
                .join(", ")}
            </p>
          </Show>
          <Show when={p().throttle}>
            {(t) => (
              <p title="A throttled notification is recorded as suppressed with a reason, never silently dropped.">
                <span class="text-ink-subtle">at most</span> {t().max} per{" "}
                {duration(t().window_seconds)}
              </p>
            )}
          </Show>
          {/* ⚠️ THE PAIR IS READ OFF `count_min`, WHICH THE SERVER GUARANTEES COMES
              WITH ITS WINDOW (`policies_count_pair_ck`). A row that rendered each
              half independently would print "once it has happened 5 times in —" for
              a shape the database cannot hold. */}
          <Show when={p().count_min}>
            {(min) => (
              <p title="Below the threshold a notification is recorded as suppressed with reason `below_threshold`, never silently dropped.">
                <span class="text-ink-subtle">once it has happened</span> {min()} times in{" "}
                {duration(p().count_window_seconds)}
              </p>
            )}
          </Show>
          <Show when={p().digest_window_seconds}>
            {(window) => (
              <p title="One message per window, in addition to whatever else this policy routes.">
                <span class="text-ink-subtle">and one summary every</span> {duration(window())}
                <Show when={p().digest_floor}>
                  {(floor) => <> if at least {floor()} firings opened</>}
                </Show>
              </p>
            )}
          </Show>
        </div>

        <Show when={remove.error !== null}>
          <ErrorBanner error={remove.error} />
        </Show>
      </Panel>
    </li>
  );
};

/* -------------------------------------------------------------------------- */

const PolicyDialog: Component<{
  readonly open: boolean;
  readonly policy: Policy | null;
  readonly channels: readonly Channel[];
  readonly onClose: () => void;
}> = (props) => {
  const client = useQueryClient();
  const editing = (): boolean => props.policy !== null;

  const [name, setName] = createSignal("");
  const [priority, setPriority] = createSignal(100);
  const [enabled, setEnabled] = createSignal(true);
  const [matcherText, setMatcherText] = createSignal("");
  const [reasons, setReasons] = createSignal<readonly NotificationReason[]>(["fired", "all_resolved"]);
  const [channelIds, setChannelIds] = createSignal<readonly string[]>([]);
  const [templateId, setTemplateId] = createSignal("");
  const [subjectKinds, setSubjectKinds] = createSignal<readonly SubjectKind[]>([]);
  const [throttleMax, setThrottleMax] = createSignal<number | null>(null);
  const [throttleWindow, setThrottleWindow] = createSignal<number | null>(null);
  const [countMin, setCountMin] = createSignal<number | null>(null);
  const [countWindow, setCountWindow] = createSignal<number | null>(null);
  const [digestWindow, setDigestWindow] = createSignal<number | null>(null);
  const [digestFloor, setDigestFloor] = createSignal<number | null>(null);
  // "new" opens the create flow; a Channel opens it pre-filled for editing.
  const [channelDialog, setChannelDialog] = createSignal<Channel | "new" | null>(null);
  const [seeded, setSeeded] = createSignal(false);
  // Nothing complains until something has been typed: a dialog that opens
  // already shouting at an empty name is a dialog people learn to ignore.
  const [touched, setTouched] = createSignal(false);

  // Seed once per *opening*. The dialog stays mounted, so this must be an
  // effect: the component body runs only in the state it mounted in.
  createEffect(() => {
    if (!props.open) {
      if (seeded()) setSeeded(false);
      return;
    }
    if (seeded()) return;
    setSeeded(true);
    setTouched(false);
    {
      const p = props.policy;
      if (p !== null) {
        setName(p.name);
        setPriority(p.priority);
        setEnabled(p.enabled);
        setMatcherText(
          formatMatchers(p.matchers.map((m) => ({ name: m.name, op: m.op, value: m.value }))),
        );
        setReasons(p.reasons);
        setChannelIds(p.channel_ids);
        setTemplateId(p.template_id ?? "");
        setSubjectKinds(p.subject_kinds);
        setThrottleMax(p.throttle?.max ?? null);
        setThrottleWindow(p.throttle?.window_seconds ?? null);
        setCountMin(p.count_min ?? null);
        setCountWindow(p.count_window_seconds ?? null);
        setDigestWindow(p.digest_window_seconds ?? null);
        setDigestFloor(p.digest_floor ?? null);
      } else {
        setName("");
        setPriority(100);
        setEnabled(true);
        setMatcherText("");
        setReasons(["fired", "all_resolved"]);
        setChannelIds([]);
        setTemplateId("");
        // ⛔ EVERY ONE OF THE NEW AXES IS OFF ON A NEW POLICY, AND THAT IS THE
        // CONTRACT'S OWN DEFAULT RATHER THAN THIS SCREEN'S TASTE. An empty binding
        // is every altitude; a null threshold, window or ceiling is no condition
        // at all. A dialog that opened with a count condition pre-filled would be
        // shipping a mute nobody asked for, since a policy below its threshold
        // records `below_threshold` and says nothing.
        setSubjectKinds([]);
        setThrottleMax(null);
        setThrottleWindow(null);
        setCountMin(null);
        setCountWindow(null);
        setDigestWindow(null);
        setDigestFloor(null);
      }
    }
  });

  /**
   * A policy matcher is a `MatcherDTO`, and unlike the alert-list filter it
   * **does** accept `=~` and `!~` — the server evaluates those itself rather
   * than translating them to SQL. So the same parser is reused and nothing is
   * rejected here.
   */
  const matchers = createMemo<readonly Matcher[]>(() =>
    parseMatchers(matcherText()).matchers.map((m) => ({ name: m.name, op: m.op, value: m.value })),
  );

  const form = (): PolicyForm => ({
    name: name(),
    priority: priority(),
    enabled: enabled(),
    matchers: matchers(),
    reasons: reasons(),
    channel_ids: channelIds(),
    template_id: templateId(),
    subject_kinds: subjectKinds(),
    throttle_max: throttleMax(),
    throttle_window_seconds: throttleWindow(),
    count_min: countMin(),
    count_window_seconds: countWindow(),
    digest_window_seconds: digestWindow(),
    digest_floor: digestFloor(),
  });

  /**
   * One parse, through the generated request schema.
   *
   * ⛔ THIS IS THE GATE THE SCREEN DID NOT HAVE. The draft was handed straight
   * to `createPolicy`, so every bound in the contract was discovered as a 422 —
   * which is the exact shape of the bug this file is named after. The per-field
   * sentences above only decide which message a control shows; this decides
   * whether the request may leave the browser at all.
   */
  const gated = createMemo(() => v.safeParse(PolicyFormSchema, form()));

  /** The first complaint about one field, once the operator has touched anything. */
  const localError = (field: string): string | undefined => {
    if (!touched()) return undefined;
    const result = gated();
    if (result.success) return undefined;
    return result.issues.find((i) => i.path?.[0]?.key === field)?.message;
  };

  const draft = createMemo<CreatePolicyRequest>(() => toCreatePolicyRequest(form()));

  const mutation = useMutation(() => ({
    mutationFn: (body: CreatePolicyRequest) => {
      const p = props.policy;
      if (p === null) return createPolicy(body, idempotencyKey());
      /*
       * ⛔ EVERY NULLABLE FIELD IS SENT EXPLICITLY ON THE PATCH, AS null WHEN
       * CLEARED. The create body OMITS them when unset, because on a create absent
       * already means the default — oto's own card, no throttle, no condition, no
       * digest. On a patch, absent means "leave it alone", so reusing the create
       * shape here would make TURNING ANY OF THEM OFF impossible: the dialog would
       * show the throttle removed and the save would silently keep it.
       *
       * ⚠️ `subject_kinds` IS NOT IN THIS LIST AND MUST NOT JOIN IT. It has no
       * `null` on the wire in either direction — the column is `NOT NULL DEFAULT
       * '{}'` — so `body.subject_kinds` is already the whole statement, and `[]`
       * clears a binding exactly the way `null` clears the others.
       */
      return updatePolicy(p.id, {
        ...body,
        template_id: body.template_id ?? null,
        throttle: body.throttle ?? null,
        count_min: body.count_min ?? null,
        count_window_seconds: body.count_window_seconds ?? null,
        digest_window_seconds: body.digest_window_seconds ?? null,
        digest_floor: body.digest_floor ?? null,
      });
    },
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: qk.settings.policies() });
      setSeeded(false);
      props.onClose();
    },
  }));

  const violations = (): ReadonlyMap<string, string> => violationsByField(mutation.error);

  return (
    <>
    <Modal
      open={props.open}
      onOpenChange={(isOpen) => {
        if (!isOpen) {
          setSeeded(false);
          props.onClose();
        }
      }}
    >
      <ModalContent>
        <ModalHeader>
          <ModalTitle>
            {editing() ? `Edit ${props.policy?.name ?? "policy"}` : "Add a notification policy"}
          </ModalTitle>
          <ModalDescription>
            A policy decides whether and where a fact is communicated. It never decides how the
            message looks — that belongs to the channel's renderer.
          </ModalDescription>
        </ModalHeader>

        <div class={cn(FORM, "text-item leading-relaxed text-ink")}>
          <Show when={mutation.error !== null}>
            <ErrorBanner error={mutation.error} />
          </Show>

          <div class={FIELD_ROW}>
            <TextField
              class={cn(FIELD, "min-w-[12rem] flex-[2]")}
              value={name()}
              validationState={
                (localError("name") ?? violations().get("name")) ? "invalid" : "valid"
              }
              onChange={(value) => {
                setTouched(true);
                setName(value);
              }}
            >
              <TextFieldLabel>
                Name
                <span class="ml-0.5 text-ink-subtle" aria-hidden="true">
                  *
                </span>
              </TextFieldLabel>
              <TextFieldInput maxLength={NAME_MAX} placeholder="critical → #sre-alerts" />
              <TextFieldErrorMessage role="alert">
                {localError("name") ?? violations().get("name")}
              </TextFieldErrorMessage>
            </TextField>
            <TextField
              class={cn(FIELD, "w-28")}
              value={Number.isFinite(priority()) ? String(priority()) : ""}
              validationState={
                (localError("priority") ?? violations().get("priority")) ? "invalid" : "valid"
              }
              onChange={(value) => {
                setTouched(true);
                // An unreadable box becomes `NaN` rather than the last good
                // number: silently keeping the previous value would save
                // something the operator can no longer see.
                setPriority(Number.parseInt(value, 10));
              }}
            >
              <TextFieldLabel>Priority</TextFieldLabel>
              <TextFieldInput type="number" min={PRIORITY_MIN} max={PRIORITY_MAX} step={1} />
              <TextFieldDescription class={HELP}>{`Lower first. ${PRIORITY_MIN}–${PRIORITY_MAX}.`}</TextFieldDescription>
              <TextFieldErrorMessage role="alert">
                {localError("priority") ?? violations().get("priority")}
              </TextFieldErrorMessage>
            </TextField>
          </div>

        <div class={FIELD}>
          <label for="pol-matchers" class={LABEL}>
            Matchers
          </label>
          <MatcherInput
            id="pol-matchers"
            value={matcherText()}
            onChange={(next) => {
              setTouched(true);
              setMatcherText(next);
            }}
            onCommit={() => undefined}
          />
          <p class={HELP}>
            All matchers must match. An empty list matches everything, and at most {MATCHERS_MAX} may
            be given. Unlike the alert-list filter, a policy accepts{" "}
            <code class="font-mono">=~</code> and <code class="font-mono">!~</code> — the server
            evaluates those itself.
          </p>
          <Show when={localError("matchers") ?? violations().get("matchers")}>
            {(msg) => (
              <p class="text-meta font-medium text-ink" role="alert">
                {msg()}
              </p>
            )}
          </Show>
        </div>

        <TemplatePicker
          value={templateId()}
          onChange={(next) => {
            setTouched(true);
            setTemplateId(next);
          }}
        />

        <ChannelPicker
          channels={props.channels}
          value={channelIds()}
          onChange={(next) => {
            setTouched(true);
            setChannelIds(next);
          }}
          error={localError("channel_ids") ?? violations().get("channel_ids")}
          onCreateNew={() => setChannelDialog("new")}
          onEdit={(c) => setChannelDialog(c)}
        />

        <div class={FIELD}>
          <ToggleGroup
            showLegend
            legend="About these facts"
            multiple
            value={[...reasons()]}
            onChange={(next) => {
              setTouched(true);
              setReasons(next as NotificationReason[]);
            }}
          >
            <For each={REASONS}>
              {(r) => <ToggleGroupItem value={r}>{REASON_LABEL[r]}</ToggleGroupItem>}
            </For>
          </ToggleGroup>
          <Show when={localError("reasons") ?? violations().get("reasons")}>
            {(msg) => (
              <p class="text-meta font-medium text-ink" role="alert">
                {msg()}
              </p>
            )}
          </Show>
        </div>

        <div class={FIELD}>
          <ToggleGroup
            showLegend
            legend="About which altitude"
            multiple
            value={[...subjectKinds()]}
            onChange={(next) => {
              setTouched(true);
              setSubjectKinds(next as SubjectKind[]);
            }}
          >
            <For each={SUBJECT_KINDS}>
              {(k) => (
                <ToggleGroupItem value={k} title={SUBJECT_HELP[k]}>
                  {SUBJECT_LABEL[k]}
                </ToggleGroupItem>
              )}
            </For>
          </ToggleGroup>
          <p class={HELP}>
            Pick none and this policy is about all of them, which is the default and what every
            policy written before this control existed says. It narrows nothing a shorter list of
            facts could not — it is here because the count condition below needs a unit, and
            because “this policy is about firings” is worth being able to read off the screen.
          </p>
          <Show when={localError("subject_kinds") ?? violations().get("subject_kinds")}>
            {(msg) => (
              <p class="text-meta font-medium text-ink" role="alert">
                {msg()}
              </p>
            )}
          </Show>
        </div>

        <fieldset>
          <legend class={LEGEND}>Thresholds and windows</legend>
          <div class={FORM}>
            <p class={HELP}>
              All three are off by default, and oto is loud by default on purpose: a fact that is
              not sent is still recorded, with the reason it was held, so a silence configured here
              is always answerable. Nothing below can be set to half of itself: the server refuses a
              ceiling, a threshold or a floor with no window beside it, so the boxes arrive and
              leave in pairs.
            </p>

            <ConditionBlock
              id="pol-throttle"
              label="Send at most a fixed number per window"
              on={throttleMax() !== null}
              onToggle={(on) => {
                setTouched(true);
                setThrottleMax(on ? THROTTLE_MAX_RANGE.min : null);
                setThrottleWindow(on ? THROTTLE_WINDOW_RANGE.min : null);
              }}
              help="The ceiling. Past it a notification is recorded as suppressed with reason `throttled` — never silently dropped, and visible in the activity log."
              error={violations().get("throttle")}
            >
              <NumberField
                label="At most"
                range={THROTTLE_MAX_RANGE}
                value={throttleMax()}
                error={localError("throttle_max")}
                onChange={(next) => {
                  setTouched(true);
                  setThrottleMax(next);
                }}
              />
              <NumberField
                label="Per (seconds)"
                range={THROTTLE_WINDOW_RANGE}
                value={throttleWindow()}
                help={secondsHelp(throttleWindow())}
                error={localError("throttle_window_seconds")}
                onChange={(next) => {
                  setTouched(true);
                  setThrottleWindow(next);
                }}
              />
            </ConditionBlock>

            <ConditionBlock
              id="pol-count"
              label="Stay quiet until it has happened enough"
              on={countMin() !== null}
              onToggle={(on) => {
                setTouched(true);
                setCountMin(on ? COUNT_MIN_RANGE.min : null);
                setCountWindow(on ? COUNT_WINDOW_RANGE.min : null);
                // ⭐ SWITCHING IT ON BINDS THE ALTITUDE, because the server requires
                // exactly `case` beside a count and would otherwise refuse the save
                // for a field two groups up that the operator never touched. Turning
                // it back off leaves the binding alone: it is a legitimate thing to
                // have chosen on its own, and silently unpicking it would undo an
                // operator's own edit.
                if (on && !(subjectKinds().length === 1 && subjectKinds()[0] === CASE_KIND)) {
                  setSubjectKinds([CASE_KIND]);
                }
              }}
              help="The floor to the throttle's ceiling, and the same two fields read the other way round. Below it a notification is recorded as suppressed with reason `below_threshold`. It counts firings, so it is only about the `one firing` altitude."
            >
              <NumberField
                label="Once it has happened"
                range={COUNT_MIN_RANGE}
                value={countMin()}
                error={localError("count_min")}
                onChange={(next) => {
                  setTouched(true);
                  setCountMin(next);
                }}
              />
              <NumberField
                label="Within (seconds)"
                range={COUNT_WINDOW_RANGE}
                value={countWindow()}
                help={secondsHelp(countWindow())}
                error={localError("count_window_seconds")}
                onChange={(next) => {
                  setTouched(true);
                  setCountWindow(next);
                }}
              />
            </ConditionBlock>

            <ConditionBlock
              id="pol-digest"
              label="Send one summary per window"
              on={digestWindow() !== null}
              onToggle={(on) => {
                setTouched(true);
                setDigestWindow(on ? DIGEST_WINDOW_RANGE.min : null);
                if (!on) setDigestFloor(null);
                // The digest is minted at its own altitude and routed by its own
                // fact, and a policy missing either sends nothing while looking
                // configured. Both are added here rather than left as two refusals.
                if (on && !reasons().includes(DIGEST_REASON)) {
                  setReasons([...reasons(), DIGEST_REASON]);
                }
                if (on && subjectKinds().length > 0 && !subjectKinds().includes(DIGEST_KIND)) {
                  setSubjectKinds([...subjectKinds(), DIGEST_KIND]);
                }
              }}
              help="One message about the window rather than one per fact — it is added to what this policy already routes, and damps none of it. Windows are aligned to the UTC day and carry no timezone: this is a window over what happened, never a schedule of when oto may speak."
            >
              <NumberField
                label="Every (seconds)"
                range={DIGEST_WINDOW_RANGE}
                value={digestWindow()}
                help={secondsHelp(digestWindow())}
                error={localError("digest_window_seconds") ?? violations().get("digest_window_seconds")}
                onChange={(next) => {
                  setTouched(true);
                  setDigestWindow(next);
                }}
              />
              <NumberField
                label="Only if at least"
                range={DIGEST_FLOOR_RANGE}
                value={digestFloor()}
                help="firings opened inside it. Blank sends whenever the window was not empty."
                error={localError("digest_floor") ?? violations().get("digest_floor")}
                onChange={(next) => {
                  setTouched(true);
                  setDigestFloor(Number.isNaN(next) ? null : next);
                }}
                onClear={() => {
                  setTouched(true);
                  setDigestFloor(null);
                }}
              />
            </ConditionBlock>
          </div>
        </fieldset>

        <div class={CHECK_ROW}>
          <Checkbox id="pol-enabled" checked={enabled()} onChange={setEnabled} />
          <label for="pol-enabled-input" class={CHECK_LABEL}>
            Enabled
          </label>
        </div>

          <PolicyPreviewPanel draft={draft()} />
        </div>

        <ModalFooter>
          <Button size="sm" variant="secondary" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="default"
            busy={mutation.isPending}
            disabled={!gated().success}
            onClick={() => {
              setTouched(true);
              const parsed = gated();
              if (!parsed.success) return;
              mutation.mutate(parsed.output);
            }}
          >
            {editing() ? "Save" : "Create"}
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>

    <ChannelCreateDialog
      open={channelDialog() !== null}
      channel={(() => {
        const cd = channelDialog();
        return cd === "new" ? null : cd;
      })()}
      onClose={() => setChannelDialog(null)}
      onCreated={(id) => {
        setTouched(true);
        setChannelIds([...channelIds(), id]);
      }}
    />
    </>
  );
};

/* -------------------------------------------------------------------------- */
/* Where the policy sends                                                     */
/* -------------------------------------------------------------------------- */

/**
 * "Tell these channels", as a search box rather than a wall of checkboxes.
 *
 * ⭐ THE LIST IS AS LONG AS THE ORG DECIDES IT IS. A checkbox per channel is the
 * right control while there are six of them and the wrong one at forty: the
 * dialog grows a scroll region whose only navigation is the eye, and the fields
 * under it — the facts, the dry run, the Save button — get pushed below the fold
 * of a modal by data the operator does not control. A combobox is the same
 * multiple selection with a filter in front of it: what is picked stays visible
 * as chips at all times, and finding `#sre-alerts` costs three keystrokes rather
 * than a scan.
 *
 * ⛔ THE SEARCH MATCHES THE TYPE AND THE VERBOSITY TOO, not just the name.
 * `optionTextValue` is what Kobalte filters on, so building it out of the same
 * three facts the row *shows* is what makes "slack" a query that works. Filtering
 * on the name alone would render a row saying `#sre-alerts slack status_changes`
 * and then refuse to find it by two of the three words on it.
 *
 * ⛔ THE CAP IS STATED, NOT ENFORCED BY DISABLING. The checkbox list could grey
 * out the rows past `CHANNELS_MAX` because every row was on screen to be greyed;
 * a picker you have to type into cannot tell you about a row you have not found
 * yet. So the selection is accepted and the contract's own sentence — "At most N
 * channels on one policy." — appears under the control with Save disabled behind
 * it. The bound is discovered in the dialog either way, which is the property
 * that matters (a 422 after Save is what this screen is named after).
 */
const ChannelPicker: Component<{
  readonly channels: readonly Channel[];
  readonly value: readonly string[];
  readonly onChange: (next: readonly string[]) => void;
  readonly error: string | undefined;
  /** Opens the create-a-channel dialog. Channel creation lives here now, not
   * in Settings (ADR 0047) — this is where an operator is already naming a
   * destination for a routing rule. */
  readonly onCreateNew: () => void;
  /** Opens the same dialog against an already-picked channel, so its
   * verbosity/enabled/config can be changed without a trip anywhere else. */
  readonly onEdit: (channel: Channel) => void;
}> = (props) => {
  /**
   * The chosen channels as the OBJECTS the picker offers, resolved out of
   * `props.channels` rather than rebuilt.
   *
   * ⛔ IDENTITY IS THE WHOLE POINT OF DOING IT THIS WAY. Kobalte resyncs a
   * controlled `value` against `options` by reference, so handing it freshly
   * constructed rows — even structurally identical ones — puts a controlled
   * multi-select into an `onChange` ⇄ recompute loop the moment something is
   * picked. The dry-run picker below carries the same warning in longer form; it
   * is the one bug this primitive reliably produces.
   */
  const selected = createMemo<Channel[]>(() => {
    const chosen = new Set(props.value);
    return props.channels.filter((c) => chosen.has(c.id));
  });

  /** A channel the operator can no longer name — deleted under an open dialog. */
  const orphans = createMemo(() => {
    const known = new Set(props.channels.map((c) => c.id));
    return props.value.filter((id) => !known.has(id));
  });

  return (
    <div class={FIELD}>
      <Show
        when={props.channels.length > 0}
        fallback={
          <>
            <div class="flex items-center justify-between">
              <span class={LABEL}>Tell these channels</span>
              <Button size="sm" variant="secondary" onClick={props.onCreateNew}>
                + New channel
              </Button>
            </div>
            <p class={HELP}>
              There are no channels yet, so this policy would have nowhere to send. Create one
              under an existing connection — see Settings → Connections if none exist either.
            </p>
          </>
        }
      >
        <Combobox<Channel>
          multiple
          options={[...props.channels]}
          optionValue="id"
          optionLabel="name"
          optionTextValue={(c) => `${c.name} ${c.type} ${c.verbosity}`}
          value={selected()}
          // ⛔ THE ORPHANS ARE CARRIED THROUGH. The picker can only ever hand
          // back channels it offers, so mapping its answer straight onto the
          // form would delete a destination that merely could not be named —
          // a silent edit nobody asked for, on the field that decides who hears.
          onChange={(next) => props.onChange([...next.map((c) => c.id), ...orphans()])}
          validationState={props.error === undefined ? "valid" : "invalid"}
          itemComponent={(itemProps) => (
            <ComboboxItem item={itemProps.item}>
              {itemProps.item.rawValue.name}
              <span class="ml-sm text-meta text-ink-subtle">
                {itemProps.item.rawValue.type} · {itemProps.item.rawValue.verbosity}
                {itemProps.item.rawValue.enabled ? "" : " · disabled"}
              </span>
            </ComboboxItem>
          )}
        >
          <div class="flex items-center justify-between">
            <ComboboxLabel class="block">Tell these channels</ComboboxLabel>
            <Button size="sm" variant="secondary" onClick={props.onCreateNew}>
              + New channel
            </Button>
          </div>
          <ComboboxControl<Channel>>
            {(state) => (
              <>
                {/* What is already picked, never behind anything. A policy's
                    destinations are the answer to "where does this go", and a
                    control that only showed them once opened would hide it. */}
                <For each={state.selectedOptions()}>
                  {(c) => (
                    <span class="inline-flex items-center gap-2xs rounded-chip border border-line bg-raised py-0.5 pl-1.5 pr-0.5 text-meta text-ink">
                      <button
                        type="button"
                        class="hover:underline"
                        aria-label={`Edit ${c.name}`}
                        onClick={() => props.onEdit(c)}
                      >
                        {c.name}
                      </button>
                      <button
                        type="button"
                        class="flex size-4 items-center justify-center rounded-chip text-ink-subtle hover:bg-surface hover:text-ink"
                        aria-label={`Do not tell ${c.name}`}
                        onClick={() => state.remove(c)}
                      >
                        <svg
                          xmlns="http://www.w3.org/2000/svg"
                          viewBox="0 0 24 24"
                          fill="none"
                          stroke="currentColor"
                          stroke-width="2"
                          stroke-linecap="round"
                          aria-hidden="true"
                          class="size-3"
                        >
                          <path d="M18 6L6 18M6 6l12 12" />
                        </svg>
                      </button>
                    </span>
                  )}
                </For>
                {/* The placeholder is on the input, which is where the caret
                    goes; Kobalte's root-level `placeholder` is for the *value*
                    surface a `Select` has and a combobox does not. */}
                <ComboboxInput placeholder={state.selectedOptions().length > 0 ? "Add another…" : "Search channels…"} />
                <ComboboxTrigger aria-label="Show every channel" />
              </>
            )}
          </ComboboxControl>
          <ComboboxContent />
        </Combobox>
      </Show>

      {/* A channel deleted while this dialog was open is still on the policy and
          must not vanish from it silently — the save would then quietly drop a
          destination the operator never chose to drop. */}
      <Show when={orphans().length > 0}>
        <p class="text-meta leading-snug text-ink-muted">
          {orphans().length === 1
            ? "One channel on this policy no longer exists"
            : `${orphans().length} channels on this policy no longer exist`}
          . Saving keeps {orphans().length === 1 ? "it" : "them"} exactly as {orphans().length === 1 ? "it is" : "they are"}.
        </p>
      </Show>

      <Show when={props.error}>
        {(msg) => (
          <p class="text-meta font-medium text-ink" role="alert">
            {msg()}
          </p>
        )}
      </Show>
    </div>
  );
};

/* -------------------------------------------------------------------------- */
/* Creating (or editing) one channel, inline                                  */
/* -------------------------------------------------------------------------- */

/** Every verbosity, read from the contract's own enum — same reasoning as REASONS. */
const VERBOSITIES: readonly Verbosity[] = VerbositySchema.options;

/**
 * The channel dialog that used to live in Settings.
 *
 * ⭐ THE CONNECTION IS PICKED FIRST, AND IT DECIDES EVERYTHING ELSE. A channel's
 * `type` is its connection's `type` — there is no separate provider choice
 * here, because asking for one would let an operator name a mismatch the
 * server would refuse anyway (`checkConnectionType`, ADR 0047). Once a
 * connection is picked, Slack gets the name↔id resolver; a webhook gets its
 * ordinary schema-driven config form.
 */
const ChannelCreateDialog: Component<{
  readonly open: boolean;
  readonly channel: Channel | null;
  readonly onClose: () => void;
  /** Fired once, on a successful CREATE only, so the new channel can be
   * added straight to the policy's own selection. */
  readonly onCreated: (id: string) => void;
}> = (props) => {
  const client = useQueryClient();
  const editing = (): boolean => props.channel !== null;

  const connections = useQuery(() => channelConnectionsQuery());
  const types = useQuery(() => channelTypesQuery());

  const [connectionId, setConnectionId] = createSignal<string | null>(null);
  const [name, setName] = createSignal("");
  const [verbosity, setVerbosity] = createSignal<Verbosity>("status_changes");
  const [enabled, setEnabled] = createSignal(true);
  const [config, setConfig] = createSignal<Record<string, JsonValue>>({});

  // Slack-only: the name↔id resolver's state. `locked` names which field was
  // TYPED — the other was DERIVED from it by the connection's own bot token,
  // and stays read-only until "Clear" gives both fields back.
  const [conversationName, setConversationName] = createSignal("");
  const [conversationId, setConversationId] = createSignal("");
  const [locked, setLocked] = createSignal<"name" | "id" | null>(null);
  const [resolving, setResolving] = createSignal<"name" | "id" | null>(null);

  const [showErrors, setShowErrors] = createSignal(false);
  const [dirty, setDirty] = createSignal(false);

  const connection = createMemo(() =>
    connections.data?.data.find((c) => c.id === connectionId()) ?? null,
  );
  const descriptor = createMemo<ChannelTypeDescriptor | undefined>(() =>
    types.data?.find((t) => t.type === connection()?.type),
  );
  const isSlack = createMemo(() => connection()?.type === "slack");
  const fields = createMemo(() => (isSlack() ? [] : readFields(descriptor()?.config_schema)));

  const seed = (): void => {
    const channel = props.channel;
    setLocked(null);
    setResolving(null);
    if (channel !== null) {
      setConnectionId(channel.connection_id);
      setName(channel.name);
      setVerbosity(channel.verbosity);
      setEnabled(channel.enabled);
      const cfg = channel.config as Record<string, JsonValue>;
      setConversationId(typeof cfg.conversation_id === "string" ? cfg.conversation_id : "");
      setConversationName(typeof cfg.conversation_name === "string" ? cfg.conversation_name : "");
      setConfig(cfg);
    } else {
      setConnectionId(null);
      setName("");
      setVerbosity("status_changes");
      setEnabled(true);
      setConversationId("");
      setConversationName("");
      setConfig({});
    }
    setShowErrors(false);
  };

  createEffect(() => {
    if (props.open && !dirty()) {
      setDirty(true);
      seed();
    } else if (!props.open && dirty()) {
      setDirty(false);
    }
  });

  // Switching connections on a fresh create clears whatever the previous
  // provider's fields held — a webhook URL means nothing once Slack is picked.
  createEffect(() => {
    const id = connectionId();
    if (editing() || id === null) return;
    setConversationId("");
    setConversationName("");
    setLocked(null);
    queueMicrotask(() => setConfig(initialConfig(fields())));
  });

  const resolve = useMutation(() => ({
    mutationFn: (query: { name?: string; conversation_id?: string }) => {
      const id = connectionId();
      if (id === null) throw new Error("no connection selected");
      return resolveSlackConversation(id, query);
    },
    onSuccess: (result) => {
      if (resolving() === "name") {
        setConversationId(result.conversation_id);
        setLocked("name");
      } else if (resolving() === "id") {
        setConversationName(result.conversation_name);
        setLocked("id");
      }
      setResolving(null);
    },
    onError: () => setResolving(null),
  }));

  const resolveFromName = (): void => {
    if (locked() === "id") return;
    const value = conversationName().trim();
    if (value === "") return;
    setResolving("name");
    resolve.mutate({ name: value });
  };

  const resolveFromId = (): void => {
    if (locked() === "name") return;
    const value = conversationId().trim();
    if (value === "") return;
    setResolving("id");
    resolve.mutate({ conversation_id: value });
  };

  const localErrors = createMemo(() => (isSlack() ? new Map() : validateConfig(fields(), config())));

  const canSubmit = createMemo(
    () =>
      connectionId() !== null &&
      name().trim() !== "" &&
      (isSlack() ? conversationId().trim() !== "" : localErrors().size === 0),
  );

  const mutation = useMutation(() => ({
    mutationFn: () => {
      const cfg = isSlack()
        ? {
            conversation_id: conversationId().trim(),
            ...(conversationName().trim() !== "" ? { conversation_name: conversationName().trim() } : {}),
          }
        : cleanConfig(fields(), config());
      const channel = props.channel;
      if (channel !== null) {
        return updateChannel(channel.id, {
          name: name().trim(),
          config: cfg,
          // The connection Select is disabled while editing, so this only ever
          // restates the channel's own connection_id.
          connection_id: connectionId() ?? channel.connection_id,
          verbosity: verbosity(),
          enabled: enabled(),
        });
      }
      return createChannel(
        {
          type: connection()?.type ?? "slack",
          name: name().trim(),
          config: cfg,
          connection_id: connectionId() ?? "",
          verbosity: verbosity(),
          enabled: enabled(),
          thread_updates: true,
          show_field_emoji: true,
        },
        idempotencyKey(),
      );
    },
    onSuccess: (result) => {
      void client.invalidateQueries({ queryKey: qk.settings.channels() });
      if (props.channel === null) props.onCreated(result.id);
      props.onClose();
    },
  }));

  const violations = (): ReadonlyMap<string, string> => violationsByField(mutation.error);

  return (
    <Modal
      open={props.open}
      onOpenChange={(isOpen) => {
        if (!isOpen) {
          setDirty(false);
          props.onClose();
        }
      }}
    >
      <ModalContent>
        <ModalHeader>
          <ModalTitle>{editing() ? `Edit ${props.channel?.name ?? "channel"}` : "Add a channel"}</ModalTitle>
          <ModalDescription>
            One destination under an existing connection. The connection carries the credential;
            nothing about it is set here.
          </ModalDescription>
        </ModalHeader>

        <div class={cn(FORM, "text-item leading-relaxed text-ink")}>
          <Show when={mutation.error !== null}>
            <ErrorBanner error={mutation.error} />
          </Show>

          <Show
            when={(connections.data?.data.length ?? 0) > 0}
            fallback={
              <p class={HELP}>
                No connections are set up yet. An admin sets one up once, in Settings →
                Connections — a Slack workspace's bot token, or a webhook receiver's shared
                credential.
              </p>
            }
          >
            <Select<ChannelConnection>
              class={FIELD}
              options={connections.data?.data ?? []}
              optionValue="id"
              optionTextValue={(c) => `${c.name} ${c.type}`}
              value={connection()}
              onChange={(next) => setConnectionId(next?.id ?? null)}
              disabled={editing()}
              itemComponent={(itemProps) => (
                <SelectItem item={itemProps.item}>
                  {itemProps.item.rawValue.name}
                  <span class="ml-sm text-meta text-ink-subtle">{itemProps.item.rawValue.type}</span>
                </SelectItem>
              )}
            >
              <SelectLabel>
                Connection
                <span class="ml-0.5 text-ink-subtle" aria-hidden="true">
                  *
                </span>
              </SelectLabel>
              <SelectTrigger id="ch-connection">
                <SelectValue<ChannelConnection>>
                  {(state) => `${state.selectedOption().name} (${state.selectedOption().type})`}
                </SelectValue>
              </SelectTrigger>
              <SelectHiddenSelect />
              <SelectContent />
            </Select>
          </Show>

          <TextField
            class={FIELD}
            value={name()}
            validationState={
              (violations().get("name") ??
              (showErrors() && name().trim() === "" ? "A name is required." : undefined))
                ? "invalid"
                : "valid"
            }
            onChange={setName}
          >
            <TextFieldLabel>
              Name
              <span class="ml-0.5 text-ink-subtle" aria-hidden="true">
                *
              </span>
            </TextFieldLabel>
            <TextFieldInput id="ch-name" placeholder="#sre-alerts" />
            <TextFieldDescription class={HELP}>
              Unique within the org, compared case-insensitively.
            </TextFieldDescription>
            <TextFieldErrorMessage role="alert">
              {violations().get("name") ??
                (showErrors() && name().trim() === "" ? "A name is required." : undefined)}
            </TextFieldErrorMessage>
          </TextField>

          <Show when={connection() !== null && isSlack()}>
            <fieldset>
              <legend class={LEGEND}>Slack channel</legend>
              <div class={FIELD_ROW}>
                <TextField
                  class={cn(FIELD, "flex-1")}
                  value={conversationName()}
                  disabled={locked() === "id"}
                  onChange={setConversationName}
                >
                  <TextFieldLabel>Channel name</TextFieldLabel>
                  <TextFieldInput
                    id="ch-conv-name"
                    placeholder="sre-alerts"
                    onBlur={resolveFromName}
                  />
                  <TextFieldDescription class={HELP}>
                    {locked() === "id"
                      ? "Filled in from the id — read-only until you clear it."
                      : "Leave the box to resolve its id."}
                  </TextFieldDescription>
                </TextField>
                <TextField
                  class={cn(FIELD, "flex-1")}
                  value={conversationId()}
                  disabled={locked() === "name"}
                  validationState={violations().get("config/conversation_id") ? "invalid" : "valid"}
                  onChange={setConversationId}
                >
                  <TextFieldLabel>Channel id</TextFieldLabel>
                  <TextFieldInput
                    id="ch-conv-id"
                    placeholder="C0123456789"
                    onBlur={resolveFromId}
                  />
                  <TextFieldDescription class={HELP}>
                    {locked() === "name"
                      ? "Filled in from the name — read-only until you clear it."
                      : "Or paste the id directly."}
                  </TextFieldDescription>
                  <TextFieldErrorMessage role="alert">
                    {violations().get("config/conversation_id")}
                  </TextFieldErrorMessage>
                </TextField>
              </div>
              <Show when={resolving() !== null}>
                <p class={HELP}>Asking Slack…</p>
              </Show>
              <Show when={resolve.isError}>
                <p class="text-meta font-medium text-ink" role="alert">
                  Could not resolve that channel — check the spelling and that oto's bot has been
                  invited to it.
                </p>
              </Show>
              <Show when={locked() !== null}>
                <Button
                  size="sm"
                  variant="secondary"
                  onClick={() => setLocked(null)}
                >
                  Clear, type both again
                </Button>
              </Show>
            </fieldset>
          </Show>

          <Show when={connection() !== null && !isSlack() && fields().length > 0}>
            <fieldset>
              <legend class={LEGEND}>Provider configuration</legend>
              <SchemaForm
                fields={fields()}
                value={config()}
                prefix="config"
                showErrors={showErrors()}
                violations={violations()}
                onChange={(key, next) => setConfig({ ...config(), [key]: next })}
              />
            </fieldset>
          </Show>

          <Select<Verbosity>
            class={FIELD}
            options={[...VERBOSITIES]}
            value={verbosity()}
            onChange={(next) => {
              if (next !== null) setVerbosity(next);
            }}
            validationState={violations().get("verbosity") ? "invalid" : "valid"}
            itemComponent={(itemProps) => (
              <SelectItem item={itemProps.item}>{itemProps.item.rawValue}</SelectItem>
            )}
          >
            <SelectLabel>Verbosity</SelectLabel>
            <SelectTrigger>
              <SelectValue<Verbosity>>{(state) => state.selectedOption()}</SelectValue>
            </SelectTrigger>
            <SelectHiddenSelect id="ch-verbosity" />
            <SelectErrorMessage role="alert">{violations().get("verbosity")}</SelectErrorMessage>
            <SelectContent />
          </Select>

          <div class={CHECK_ROW}>
            <Checkbox id="ch-enabled" checked={enabled()} onChange={setEnabled} />
            <label for="ch-enabled-input" class={CHECK_LABEL}>
              Enabled
            </label>
          </div>
        </div>

        <ModalFooter>
          <Button size="sm" variant="secondary" onClick={props.onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="default"
            busy={mutation.isPending}
            disabled={!canSubmit()}
            onClick={() => {
              setShowErrors(true);
              if (!canSubmit()) return;
              mutation.mutate();
            }}
          >
            {editing() ? "Save" : "Create"}
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};

/* -------------------------------------------------------------------------- */
/* The dry run                                                                */
/* -------------------------------------------------------------------------- */

/**
 * Every reason the dry run can give for a message not being sent.
 *
 * ⛔ TYPED AGAINST THE CONTRACT'S OWN ENUM, not `Record<string, string>`. It was
 * the loose type that let this map lose `snoozed`: the compiler had nothing to
 * check against, the lookup fell through to `?? r.suppressed_reason`, and the
 * dry run answered "Would not send — snoozed." — a wire token where a sentence
 * belongs, in the one place on the screen whose whole job is to explain a
 * silence before it happens. With the exhaustive type, the next reason the
 * server publishes is a build failure instead.
 *
 * ⛔ SEVEN VALUES, AND THE TWO THAT LEFT WERE THE ONLY TWO THAT WERE OTO'S OWN
 * OPINION ABOUT A SIGNAL. `storm` and `flapping` are gone from
 * `notifications_suppmap_ck` (migration `00059`, ADR 0042), which narrowed with
 * NO backfill and therefore FAILS on a database that ever recorded either — so
 * there is no stored row left for this map to explain. What the seven have in
 * common is the argument: two mean there was nowhere to send, two are a human
 * asking for less, two are a policy's own ceiling and floor over a window, one is
 * that nothing changed. `below_threshold` (migration `00073`) is the floor, and it
 * is admissible where `flapping` was not because its threshold is a column an
 * operator wrote rather than a constant compiled into oto.
 */
const SUPPRESSED_REASON: Record<NonNullable<NotificationSuppressedReason>, string> = {
  no_policy: "no policy matched",
  // §B.8.2 ranks a snooze above every automatic damper: it is a deliberate human
  // act, and therefore the most useful thing to say about a silence.
  snoozed: "someone is holding oto's notifications for this alert until a fixed time",
  throttled: "the throttle is already spent",
  // The same two policy fields as the throttle, read as a floor: not "too many
  // already" but "not enough yet".
  below_threshold: "the policy's count condition has not been met yet",
  verbosity: "the channel's verbosity does not carry this",
  channel_disabled: "the channel is disabled",
  duplicate_render: "the message would be identical to the last one",
};

/**
 * The sentence for one suppression, with the two honest fallbacks.
 *
 * A reason this build has never heard of renders as its raw wire value rather
 * than as a blank — gate G3 exists to make that a build failure — and a result
 * that carries no reason at all says so, because "would not send —  ." is how a
 * screen loses a fact without anybody noticing.
 */
function describeSuppression(reason: NotificationSuppressedReason | undefined): string {
  if (reason === undefined || reason === null) return "no reason was given";
  return SUPPRESSED_REASON[reason] ?? reason;
}

/**
 * An episode as the dry-run picker holds it: what to send, and what to call it.
 *
 * The label is captured with the choice rather than looked up later, because
 * the point of holding it is to survive the row leaving the list.
 *
 * ⛔ `caseId` IS WHAT GOES ON THE WIRE, AND THE ALERT ID IS NO LONGER SENT AT
 * ALL (git-bug 7570090). `PolicyPreviewRequest` took `alert_id` and `group_id`
 * and now takes `case_id` only: a Case IS the conversation, so a routing
 * question is asked about one firing episode rather than about an identity that
 * may not be firing. The LABEL still comes from the alert, because a case has no
 * name of its own — `case #19` names nothing an operator recognises, and
 * `KubePodCrashLooping · prod-eu · payments` is the whole reason this picker
 * exists instead of a UUID box.
 */
interface PickedCase {
  readonly caseId: string;
  readonly label: string;
}

/**
 * One alert as a pickable episode, or `null` when it has no open case.
 *
 * ⚠️ `null` IS THE COMMON CASE AND NOT AN ERROR. `RECENT_ALERTS` sorts by
 * `-last_seen_at` and filters nothing, so a resolved or expired alert is in the
 * list and has no `current_case` — there is no episode to dry-run against. It is
 * dropped from the options rather than offered and then rejected by the server,
 * because a picker that lets you choose something Preview cannot use is worse
 * than one that is shorter.
 */
function pickedCase(a: Alert): PickedCase | null {
  const id = a.current_case?.id;
  if (id === undefined || id === null) return null;
  return {
    caseId: id,
    label: `${a.alertname} · ${a.cluster_key}${a.namespace ? ` · ${a.namespace}` : ""}`,
  };
}

/**
 * "Who would this reach?" answered against an unsaved draft.
 *
 * The endpoint evaluates the inline policy **in addition to** the stored ones,
 * which is what makes the answer honest: it shows what would actually happen,
 * including the stored policy that would also fire, rather than what this
 * policy would do in isolation.
 */
const PolicyPreviewPanel: Component<{ readonly draft: CreatePolicyRequest }> = (props) => {
  // ⛔ THE CHOICE IS THE EPISODE, NOT ITS ID. The list is the twenty most
  // recently seen alerts and the stream reorders it, so an id held on its own can
  // stop naming any option the picker offers: `<Select>` would fall back to blank
  // while Preview stayed enabled, and the operator would dry-run a routing
  // policy against an episode the screen could no longer name. Holding what was
  // chosen means the picker can keep offering it.
  const [picked, setPicked] = createSignal<PickedCase | null>(null);
  const [reason, setReason] = createSignal<NotificationReason>("fired");

  // A short list of recent alerts to dry-run against, so nobody has to paste a
  // UUID to answer a routing question. `recentAlertsQuery` keys it under
  // `["alerts"]` so the stream reaches it, and rate-limits what it does about
  // that — the whole policy is stated there, not restated here.
  const recent = useQuery(() => recentAlertsQuery());

  /**
   * The recent twenty, as `PickedAlert`s.
   *
   * ⛔ KEPT AS ITS OWN MEMO, DEPENDENT ONLY ON `recent.data`. `options()` below
   * used to `.map(pickedAlert)` this same array inline, which reads `picked()`
   * in the same computation — so *every* selection re-ran the `.map` and handed
   * the controlled `Select` a fresh set of row objects it had never seen by
   * reference. Kobalte resyncs its controlled `value` against `options()` by
   * identity, so a picker driven through its real listbox (not the hidden
   * shim's `fireEvent.change`, which this bug was invisible to) spun into an
   * infinite `onChange` ⇄ recompute loop the instant something was actually
   * selected. Keeping `rows` stable across `picked()` changes is what breaks
   * that cycle.
   */
  // ⚠️ `.filter()` AFTER THE MAP, not a `flatMap` on the alert list: the twenty
  // rows include alerts with no open case, and those have no episode to preview.
  const rows = createMemo<readonly PickedCase[]>(() =>
    (recent.data?.data ?? []).map(pickedCase).filter((c): c is PickedCase => c !== null),
  );

  /**
   * What the picker offers: the recent twenty, plus the alert already chosen if
   * a refetch has since pushed it off the end.
   *
   * The chosen one goes last, and it is still the selected option, so nothing
   * silently changes under the operator between reading the list and pressing
   * Preview.
   */
  const options = createMemo<readonly PickedCase[]>(() => {
    const chosen = picked();
    if (chosen === null || rows().some((o) => o.caseId === chosen.caseId)) return rows();
    return [...rows(), chosen];
  });

  const preview = useMutation(() => ({
    mutationFn: (): Promise<PolicyPreview> =>
      previewPolicy({ case_id: picked()?.caseId ?? "", reason: reason(), policy: props.draft }),
  }));

  // Named, not boxed — the same change as the channel dialog's provider group.
  // The bordered box's `px-lg` put every control in this panel 17px right of the
  // policy's own fields; the panel is still obviously a separate thing because
  // its legend says so, in the same small caps `PanelTitle` uses.
  return (
    <fieldset>
      <legend class={LEGEND}>Dry run</legend>

      <p class={cn("mb-md", HELP)}>
        Runs the real policy matcher and the real renderer against a real alert, including this
        unsaved draft, and <span class="font-medium text-ink">sends nothing</span>.
      </p>

      <div class={FIELD_ROW}>
        <div class="min-w-[14rem] flex-1">
          {/* Labelled with Kobalte's own `SelectLabel` (matching
              `ChannelsSection.tsx`/`SourcesSection.tsx`), not a hand-written
              `<label for>` — a plain `for` cannot target `SelectTrigger`, since
              it renders a `<button>` and a `<button>` is not a labelable
              element (a native `<label for>` on one is left without an
              accessible name). `SelectLabel` wires `aria-labelledby` to the
              trigger itself, which is the real, accessible, interactive
              surface — not `SelectHiddenSelect`'s `aria-hidden` native shim,
              which exists only for browser autofill/native form submission and
              was never meant to be the primary interaction or testing surface.

              The picker is disabled, and says so, until `recent` actually has
              rows: `PolicyDialog` is a Kobalte `Modal`, whose content (this
              panel included) is presence-gated (`<Show when={contentPresent()}>`)
              and mounts only once the dialog opens — so `recentAlertsQuery()`
              does not even start fetching before then. Without a loading state
              an operator (or a test driving the real trigger) could open this
              picker while `options()` is still empty and "select" nothing,
              which is exactly the kind of honesty gap `FilterBar.tsx`'s own
              async-fed Cluster picker avoids by staying out of the way until
              it has something to offer. This picker cannot hide the same way —
              it is the point of the dry run — so it disables itself and says
              "Loading recent alerts…" instead. */}
          <Select<PickedCase>
            class={FIELD}
            options={[...options()]}
            optionValue="caseId"
            optionTextValue="label"
            value={picked()}
            onChange={setPicked}
            disabled={recent.isPending}
            placeholder={recent.isPending ? "Loading recent alerts…" : "— pick a recent alert —"}
            itemComponent={(itemProps) => (
              <SelectItem item={itemProps.item}>{itemProps.item.rawValue.label}</SelectItem>
            )}
          >
            {/* "episode", not "alert": the request names a case, and a label that
                said "alert" would promise the dry run covers an identity that is
                not currently firing. */}
            <SelectLabel class="block">Against this episode</SelectLabel>
            <SelectTrigger>
              <SelectValue<PickedCase>>{(state) => state.selectedOption().label}</SelectValue>
            </SelectTrigger>
            <SelectHiddenSelect />
            <SelectContent />
          </Select>
        </div>

        <div class="w-52">
          <Select<NotificationReason>
            class={FIELD}
            options={[...REASONS]}
            optionTextValue={(r) => REASON_LABEL[r]}
            value={reason()}
            onChange={(next) => {
              if (next !== null) setReason(next);
            }}
            itemComponent={(itemProps) => (
              <SelectItem item={itemProps.item}>{REASON_LABEL[itemProps.item.rawValue]}</SelectItem>
            )}
          >
            <SelectLabel class="block">Simulating</SelectLabel>
            <SelectTrigger>
              <SelectValue<NotificationReason>>
                {(state) => REASON_LABEL[state.selectedOption()]}
              </SelectValue>
            </SelectTrigger>
            <SelectHiddenSelect />
            <SelectContent />
          </Select>
        </div>

        {/* `self-end` earns its place here and only here: both fields on this
            line are a label over a trigger with nothing hanging below, so the
            row's bottom edge IS the control line. */}
        <Button
          class="self-end"
          busy={preview.isPending}
          disabled={picked() === null}
          onClick={() => preview.mutate()}
        >
          Preview
        </Button>
      </div>

      <Show when={preview.error !== null}>
        <ErrorBanner error={preview.error} class="mt-md" />
      </Show>

      <Show when={preview.data}>
        {(result) => (
          <div class="mt-lg">
            <Show
              when={result().matched}
              fallback={
                <p class="rounded-control border border-line-strong border-l-[3px] border-l-ink bg-surface px-md py-sm text-body font-medium leading-snug text-ink">
                  Nothing would be sent. No enabled policy — including this draft — matches this
                  alert for that fact, so it would go unreported.
                </p>
              }
            >
              <ul class="flex flex-col gap-sm">
                <For each={result().results}>
                  {(r) => (
                    <li
                      class={cn(
                        "flex flex-col gap-xs rounded-control border px-md py-sm",
                        r.would_send
                          ? "border-line bg-surface"
                          : "border-line-strong bg-sunken",
                      )}
                    >
                      <div class="flex flex-wrap items-center gap-sm text-body">
                        <span class="font-medium text-ink">{r.channel_name}</span>
                        <Chip>{r.channel_type}</Chip>
                        <Chip title="How the message would be placed in the thread.">{r.mode}</Chip>
                        <span class="ml-auto text-meta text-ink-subtle">
                          via {r.policy_name}
                        </span>
                      </div>

                      <Show
                        when={r.would_send}
                        fallback={
                          <p class="text-meta leading-snug text-ink-muted">
                            Would not send — {describeSuppression(r.suppressed_reason)}. It would
                            still be recorded with that reason.
                          </p>
                        }
                      >
                        <Show when={r.rendered_fallback}>
                          {(text) => (
                            <p class="border-l-2 border-line-strong pl-sm text-meta leading-snug text-ink">
                              {text()}
                            </p>
                          )}
                        </Show>
                      </Show>
                    </li>
                  )}
                </For>
              </ul>
            </Show>

            <Show when={(result().warnings ?? []).length > 0}>
              <ul class="mt-md flex flex-col gap-2xs">
                <For each={result().warnings ?? []}>
                  {(w) => <li class="text-meta leading-snug text-ink-muted">{w}</li>}
                </For>
              </ul>
            </Show>
          </div>
        )}
      </Show>
    </fieldset>
  );
};

/**
 * Which message template this policy's alerts are rendered with.
 *
 * ⭐ IT LIVES ON THE POLICY BECAUSE THE POLICY IS THE ROUTING DECISION (ADR
 * 0050). A template carries no matchers of its own — the predecessor did, and an
 * operator then had to hold two resolution rules in their head to predict a card:
 * the one that chose the channel and the one that chose the words. Here there is
 * one, and it is on the screen that already decided everything else.
 *
 * ⚠️ ONE TEMPLATE FOR EVERY DESTINATION THIS POLICY FANS OUT TO, AND THEY NEED
 * NOT SHARE A PROVIDER. `card` and `text` render anywhere; `raw` is Slack Block
 * Kit and degrades to oto's own card elsewhere. That is said here rather than
 * discovered, because the alternative is a Slack-shaped message arriving at a
 * webhook and nobody knowing why it looks like the default.
 */
/* -------------------------------------------------------------------------- */
/* Thresholds and windows                                                     */
/* -------------------------------------------------------------------------- */

/** A window's length in words, so nobody has to divide by 3600 in their head. */
function secondsHelp(seconds: number | null): string | undefined {
  if (seconds === null || !Number.isFinite(seconds) || seconds <= 0) return undefined;
  return duration(seconds);
}

/**
 * One optional condition: a checkbox that reveals its own fields.
 *
 * ⛔ THE CHECKBOX IS WHAT MAKES THE PAIR RULES UNREACHABLE. `count_min` without
 * `count_window_seconds`, a digest floor without a digest window and a throttle
 * ceiling without a throttle window are three separate 422s, and all three are
 * *shapes a form can simply refuse to build*: one control switches both halves on
 * and both halves off, so there is no sequence of clicks that produces half of a
 * condition. The schema still states the rules — a policy is also loaded FROM the
 * server, and a form that only enforces what its own widgets can emit is one
 * refactor away from enforcing nothing.
 *
 * ⚠️ THE HELP TEXT IS INSIDE THE `Show`, DELIBERATELY. Three paragraphs explaining
 * three conditions nobody has switched on is most of a modal spent on the state
 * every policy is already in; the label alone carries the unticked case.
 */
const ConditionBlock: Component<{
  readonly id: string;
  readonly label: string;
  readonly help: string;
  readonly on: boolean;
  readonly onToggle: (on: boolean) => void;
  readonly error?: string | undefined;
  readonly children: JSX.Element;
}> = (props) => (
  <div class={FIELD}>
    <div class={CHECK_ROW}>
      <Checkbox id={props.id} checked={props.on} onChange={props.onToggle} />
      <label for={`${props.id}-input`} class={CHECK_LABEL}>
        {props.label}
      </label>
    </div>
    <Show when={props.on}>
      <div class={cn(FIELD_ROW, "pl-lg")}>{props.children}</div>
      <p class={cn(HELP, "pl-lg")}>{props.help}</p>
      <Show when={props.error}>
        {(msg) => (
          <p class="pl-lg text-meta font-medium text-ink" role="alert">
            {msg()}
          </p>
        )}
      </Show>
    </Show>
  </div>
);

/**
 * One bounded whole number, with the contract's own range on the control.
 *
 * ⛔ `min` AND `max` ARE ATTRIBUTES AND NOT JUST VALIDATION, for the reason the
 * priority box carries them: the browser's own stepper and its native validation
 * then agree with the server instead of offering a range nobody enforces. Both
 * come off the schema — see the constants block at the top of this file, where not
 * one of these numbers is written down.
 *
 * `onClear` is what makes a field genuinely optional *inside* a condition that is
 * switched on: the digest floor may be blank while its window is set, and blanking
 * a box has to mean `null` rather than `NaN`. A field without `onClear` treats an
 * empty box as unreadable, which is the honest answer for a required half.
 */
const NumberField: Component<{
  readonly label: string;
  readonly range: Range;
  readonly value: number | null;
  readonly help?: string | undefined;
  readonly error?: string | undefined;
  readonly onChange: (next: number) => void;
  readonly onClear?: () => void;
}> = (props) => (
  <TextField
    class={cn(FIELD, "w-44")}
    value={props.value === null || !Number.isFinite(props.value) ? "" : String(props.value)}
    validationState={props.error === undefined ? "valid" : "invalid"}
    onChange={(raw) => {
      if (raw.trim() === "" && props.onClear !== undefined) {
        props.onClear();
        return;
      }
      props.onChange(Number.parseInt(raw, 10));
    }}
  >
    <TextFieldLabel>{props.label}</TextFieldLabel>
    <TextFieldInput type="number" min={props.range.min} max={props.range.max} step={1} />
    <Show when={props.help}>
      {(text) => <TextFieldDescription class={HELP}>{text()}</TextFieldDescription>}
    </Show>
    <TextFieldErrorMessage role="alert">{props.error}</TextFieldErrorMessage>
  </TextField>
);

/* -------------------------------------------------------------------------- */

const TemplatePicker: Component<{
  value: string;
  onChange: (next: string) => void;
}> = (props) => {
  const templates = useQuery(() => notificationTemplatesQuery());
  const rows = createMemo(() => (templates.data?.data ?? []).filter((t) => t.enabled));

  const chosen = createMemo(() => rows().find((t) => t.id === props.value) ?? null);

  return (
    <div class={FIELD}>
      <Select<string>
        value={props.value}
        onChange={(next) => props.onChange(next ?? "")}
        options={["", ...rows().map((t) => t.id)]}
        placeholder="oto's own card"
        itemComponent={(itemProps) => (
          <SelectItem item={itemProps.item}>
            {itemProps.item.rawValue === ""
              ? "oto's own card"
              : (rows().find((t) => t.id === itemProps.item.rawValue)?.name ??
                "a removed template")}
          </SelectItem>
        )}
      >
        <SelectLabel class={LABEL}>Message template</SelectLabel>
        <SelectTrigger>
          <SelectValue<string>>
            {(state) =>
              state.selectedOption() === ""
                ? "oto's own card"
                : (rows().find((t) => t.id === state.selectedOption())?.name ?? "oto's own card")
            }
          </SelectValue>
        </SelectTrigger>
        <SelectContent />
        <SelectHiddenSelect />
      </Select>
      <p class={HELP}>
        <Show
          when={chosen()}
          fallback="Leave this alone and every alert reads in oto's own voice. Write a template under Templates to change it."
        >
          {(t) => (
            <>
              {t().format === "raw"
                ? "Slack Block Kit — every non-Slack destination in this policy falls back to oto's own card."
                : "Portable — oto compiles it for whichever channel each alert goes to."}{" "}
              Written for {t().provider}.
            </>
          )}
        </Show>
      </p>
    </div>
  );
};
