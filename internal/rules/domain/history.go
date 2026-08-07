package domain

import "sort"

// Version is one entry of a rule's edit history: a snapshot plus the ordinal
// that makes it addressable as "v3 of KubePodCrashLooping".
//
// The ordinal is DERIVED, never stored. rule_snapshots is deduplicated by
// content, so the rows for one rule key already are its distinct texts in
// capture order; numbering them at read time means the number can never drift
// out of step with the rows, and a rule reverted to an earlier text reuses the
// earlier row rather than minting a fake new version.
type Version struct {
	// Number is 1-based, oldest first.
	Number   int
	Snapshot Snapshot
	// SupersededBy is the Number of the next version, or 0 for the newest.
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

// Drifted reports whether the rule has been edited since the version carrying
// fp — that is, whether the newest text differs from the one an occurrence was
// bound to. This is SPEC §C.6's definition of drift, evaluated over the
// history rather than with a second query.
func (h History) Drifted(fp string) bool {
	latest, ok := h.Latest()
	if !ok || fp == "" {
		return false
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
		v := Version{Number: i + 1, Snapshot: s}
		if i+1 < len(ordered) {
			v.SupersededBy = i + 2
		}
		versions = append(versions, v)
	}
	return History{Key: key, Versions: versions}
}
