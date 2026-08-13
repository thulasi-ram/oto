package domain

import "sort"

// Version is one entry of a rule's edit history: a snapshot at the position
// it holds within History.Versions, oldest first.
//
// A version's ordinal — "v3 of KubePodCrashLooping" — is DERIVED from that
// position and never stored on the Version itself: rule_snapshots is
// deduplicated by content, so the rows for one rule key already are its
// distinct texts in capture order, and a stored ordinal is exactly the kind of
// denormalisation that can drift out of step with the rows it numbers. At
// answers "what is version N" by indexing History.Versions; nothing needs the
// reverse question answered by a field.
type Version struct {
	Snapshot Snapshot
	// SupersededBy is the 1-based position of the next version, or 0 for the
	// newest.
	SupersededBy int
}

// History is the ordered edit history of one rule key, oldest first.
type History struct {
	Key      Key
	Versions []Version
}

// Len is the number of distinct texts this rule has had that oto has seen.
func (h History) Len() int { return len(h.Versions) }

// Latest returns the newest version, and false when the history is empty.
func (h History) Latest() (Version, bool) {
	if len(h.Versions) == 0 {
		return Version{}, false
	}
	return h.Versions[len(h.Versions)-1], true
}

// LatestDefinition returns the newest version that actually CARRIES a rule
// definition, and false when this rule has never been recovered.
//
// ⭐ IT IS Latest FOR EVERY QUESTION THAT ENDS "...COMPARED TO NOW". An
// `unavailable` capture is a real version and belongs in the list an operator
// reads — "oto looked at 03:00 and could not see it" is a fact worth showing —
// but it is not a rule the newest text can be compared against. Answering "has
// this changed since it fired?" with the outage row makes the alert card claim
// an edit whose evidence is an empty expression, on every alert in the source,
// for as long as the outage lasts.
func (h History) LatestDefinition() (Version, bool) {
	for i := len(h.Versions) - 1; i >= 0; i-- {
		if h.Versions[i].Snapshot.Available() {
			return h.Versions[i], true
		}
	}
	return Version{}, false
}

// At returns the version with the given 1-based number.
func (h History) At(number int) (Version, bool) {
	if number < 1 || number > len(h.Versions) {
		return Version{}, false
	}
	return h.Versions[number-1], true
}

// ByFingerprint finds the version carrying a content address, which is how an
// occurrence's bound snapshot is located within the history.
func (h History) ByFingerprint(fp string) (Version, bool) {
	for _, v := range h.Versions {
		if v.Snapshot.Fingerprint == fp {
			return v, true
		}
	}
	return Version{}, false
}

// Drifted reports whether the rule has been EDITED since the version carrying
// fp — that is, whether the newest text an occurrence could be compared against
// differs from the one it was bound to. This is SPEC §C.6's definition of drift,
// evaluated over the history rather than with a second query.
//
// ⛔ THE COMPARISON IS AGAINST LatestDefinition, and it goes through Drifted
// rather than comparing the digests, so that an outage or a change of recovery
// path in between is not reported as somebody editing the rule. When fp names a
// version this history holds, both snapshots are in hand and the full evidence
// rule applies; when it does not — a fingerprint from another key, or a row
// beyond DefaultHistoryLimit — the digests are all there is.
func (h History) Drifted(fp string) bool {
	if fp == "" {
		return false
	}
	latest, ok := h.LatestDefinition()
	if !ok {
		return false
	}
	if bound, found := h.ByFingerprint(fp); found {
		return Drifted(bound.Snapshot, latest.Snapshot)
	}
	return latest.Snapshot.Fingerprint != fp
}

// DiffVersions compares two versions by number, oldest-first regardless of the
// order the caller asked in.
func (h History) DiffVersions(a, b int) (Diff, bool) {
	if a > b {
		a, b = b, a
	}
	from, okA := h.At(a)
	to, okB := h.At(b)
	if !okA || !okB {
		return Diff{}, false
	}
	return Compare(from.Snapshot, to.Snapshot), true
}

// NewHistory numbers a set of snapshots for one rule key.
//
// Snapshots are sorted by capture time, with the content address as the
// tiebreak so that two rows captured in the same millisecond still order
// deterministically — a history that renumbers itself between two reads is
// worse than no history.
func NewHistory(key Key, snaps []Snapshot) History {
	ordered := make([]Snapshot, len(snaps))
	copy(ordered, snaps)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].CapturedAt.Equal(ordered[j].CapturedAt) {
			return ordered[i].CapturedAt.Before(ordered[j].CapturedAt)
		}
		return ordered[i].Fingerprint < ordered[j].Fingerprint
	})

	versions := make([]Version, 0, len(ordered))
	for i, s := range ordered {
		v := Version{Snapshot: s}
		if i+1 < len(ordered) {
			v.SupersededBy = i + 2
		}
		versions = append(versions, v)
	}
	return History{Key: key, Versions: versions}
}
