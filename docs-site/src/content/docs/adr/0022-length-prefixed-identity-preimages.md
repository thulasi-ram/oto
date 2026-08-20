---
title: 0022 — Length-prefixed identity pre-images
---
**Status:** Accepted · 2026-08-09 · amended 2026-08-20 (Amendment 1)
**Amends:** [0004](/adr/0004-alert-identity-key-and-fingerprint/) (which specified the `0x00` framing),
SPEC §C.0–§C.7
**Relates to:** [0021](/adr/0021-correctness-and-testing-strategy/) (the tests that found this)

## Context

Every identity oto computes — `alert_key`, `group_key`, `rule_fingerprint`,
`batch_dedup_key`, `notification.idempotency_key` — is a SHA-256 over a pre-image built by
concatenating fields. Until now the fields were separated by bytes: `0x01` and `0x02`
inside §C.1's label serialisation, `0x00` between the outer fields of every other key.

Separator framing is injective **only if no field can contain the separator.** Three
fields can:

- **label values** — arbitrary UTF-8 from the operator's monitoring system, and a JSON
  unicode escape decodes straight through the ingest path;
- **`receiver`** — free text from `alertmanager.yml`;
- **`expr`** — arbitrary PromQL;
- **Alertmanager's own `groupKey`** — which SPEC §C.4 explicitly documents as *unescaped
  and unbounded*.

Nothing constrained them. `NewLabels` validated label *names* against a charset and
bounded values only by *length*. So two different field splits produced one digest:

```
{alertname:"X", b:"1", c:"2"}      ->  alertname 01 X 02 b 01 1 02 c 01 2 02
{alertname:"X", b:"1\x02c\x012"}   ->  alertname 01 X 02 b 01 1 02 c 01 2 02
```

A three-label set and a two-label set, byte-identical. They become **one Alert**: one
row, one timeline, one Slack thread, with one alert's episodes written into another's
history. For a product whose only claim is that its record is true, that is the worst
available failure.

The outer framing was no better, and its witness is subtler — it does not even require a
field to contain a NUL in the obvious way:

```
("a", {b: ""})                            ->  61 00 | 00 00 00 01 62 00 00 00 00
("a\0\0\0\0\x01b\0\0\0", {})              ->  61 00 00 00 00 01 62 00 00 00 | 00
```

because `canon({b:""})` ends in a zero-length prefix, which absorbs the terminator the
receiver would otherwise have contributed.

The test that was supposed to prove this safe,
`TestGroupKey_ReceiverAndLabelsAreSeparateFields`, was **off by one byte** — its forgery
produced a pre-image one trailing NUL longer than the honest one, so it passed under the
old framing too and proved nothing.

## Decision

Every field in every §C pre-image is length-prefixed:

```
field(x) := uint32be(len(x)) || x        -- len() counts BYTES
```

Four bytes, big-endian, fixed width, applied uniformly. Nothing is escaped and no byte is
reserved. Each key writes N framed fields and then one final field **raw**, because a
remainder needs no prefix to be found; where that tail is itself a §C.1 serialisation it
is self-delimiting in turn. The one place a canonical blob is not last (§C.6's
`rule_labels`) is framed like any other field.

This is injective because the encoding is **uniquely decodable** — read four bytes as
`n`, take exactly `n` bytes, repeat — which gives it an explicit left inverse.

Separately, a label value may not contain `0x00`. That is a **storability** bound, not
sanitisation: Postgres `text` cannot hold U+0000 at all, so such a value fails at INSERT
however it is serialised. Nothing else is stripped, replaced or normalised.

`Labels.Fingerprint` (§C.3) is deliberately untouched. It reproduces
`prometheus/common`'s algorithm rather than oto's framing, and it is the join key for
`/api/v2/alerts` reconciliation — changing it would silently break every Alertmanager
match.

## Amendment 1 — §C.7 may append ONE fixed-width field after its tail (2026-08-20)

`notification.idempotency_key` now takes an optional trailing **occasion** — `field(uuid)`, written
only when the key names one — which makes it the single §C key that writes a field *after* the raw
tail. SPEC §C.7.1 has the product argument (a re-snooze was minting a byte-identical key and its
announcement was being dropped); this amendment records what it does to **this ADR's framing rule**,
because that rule is what makes the appended field safe rather than a second forgeable boundary.

The decision below says each key writes N framed fields and then one final field raw, "because a
remainder needs no prefix to be found". Appending after that remainder means the remainder is no
longer the end, so the boundary between them has to come from somewhere. It comes from **width**:

- the occasion is a uuid, hashed as its 16 raw bytes, so its field is a **fixed 20 bytes** whose
  first byte is the `0x00` of `uint32be(16)`;
- the tail it follows is `strconv.Itoa`, which emits only `0x30`–`0x39`;
- so the first `0x00` after the reason field is the split, always, and the encoding stays uniquely
  decodable — read digits to the first non-digit, and what remains is either nothing or exactly one
  framed 16-byte field.

**The two bounds this rests on are binding.** An occasion of variable width could open with a digit
(a length ≥ 48·2²⁴ does) and forge the boundary, and a tail that was not decimal would break it from
the other side. **No second field may be appended after a tail, and no appended field may be
variable-width.** Anything richer than one uuid belongs in front of the tail, which re-keys the whole
key and is a different decision from this one.

`uuid.Nil` writes **no bytes**, not sixteen zero ones, which is the property that made this additive:
every key that names no occasion is hashed over byte-identical input and **nothing stored moved** —
the opposite of this ADR's own headline consequence below, and the reason this amendment needed no
release-gated re-key.

## Consequences

- **Every `alert_key`, `group_key`, `rule_fingerprint`, `batch_dedup_key` and
  `idempotency_key` changes.** This is why it landed pre-release: there is no history to
  re-key. After release it would have cost a migration that splits every timeline in two,
  which for a flight recorder is the most expensive migration that exists.
- The same fix had to be applied in **four** places — `alerts/domain`, `ingestion/domain`,
  `rules/domain`, `notification/domain` — because §C.6 and §C.7 each have two
  implementations. That duplication is now cross-checked by tests and tracked as debt;
  it is a recurring cost, not a one-off.
- Pre-images grow by 8 bytes per label instead of 2, and by 4 bytes per outer field. At
  most 384 bytes against the 16 KiB label-set cap. Irrelevant to a hash.
- A label value carrying a NUL is now rejected into `ingest_rejections` rather than
  stored. It currently reports `undecodable`, which is lossy; a dedicated reason is
  tracked separately.
- Anyone recomputing an `alert_key` outside Go can reimplement fixed-width prefixes
  trivially. That was the argument for fixed width over a varint, along with
  `SerialisedSize` staying exact arithmetic.

## Alternatives rejected

**Escape the separators inside values.** Fully correct, and rejected on product grounds:
escaping means oto **editing an operator's bytes** in order to store them. oto is a
flight recorder; the record is what upstream said. Length prefixing achieves the same
injectivity while leaving every byte untouched.

**Reject values containing the separators.** The cheap fix, and the right one *after*
release — it changes no existing key. Rejected here only because the project is
unreleased, so the correct fix is free. It remains the fallback if a future format change
is ever needed post-release.

**Varint / LEB128 length prefixes.** Saves three bytes per field on short values.
Rejected: fixed width is trivially reimplementable by an external consumer, and it keeps
`SerialisedSize` exact rather than a length-of-length computation.

**Hash each field separately and combine the digests.** Also injective, and a larger
change to every call site for no benefit over prefixes.
