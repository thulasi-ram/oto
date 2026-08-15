package decode

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	alerts "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/ingestion/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// Note is a NON-FATAL bound violation: the alert or the batch was kept, and a
// row still goes to `ingest_rejections` so nothing is silent.
//
// The distinction between a Note and an error from Normalise is the whole shape
// of §L.3.2: B7, B8, B13, B14, B15 and B19 record and keep, B3-B6, B9-B12 and
// B18 record and drop THAT ALERT, and nothing at all drops the batch.
type Note struct {
	Reason domain.Reason
	Detail string
}

// TruncationMarker is appended to a value shortened by B8.
const TruncationMarker = "…"

// ---------------------------------------------------------------- batch bounds

// BatchOutcome is the result of applying the batch-level bounds B2 and B15.
type BatchOutcome struct {
	// Dropped is how many alerts B2 removed from the tail of the batch.
	Dropped int
	// Notes carries one entry per batch-level bound that fired.
	Notes []Note
}

// ApplyBatchBounds applies B2 (alerts per batch) and B15 (receiver / groupKey
// length) to a decoded envelope, IN PLACE.
//
// B2 TRUNCATES rather than rejects: nine thousand of ten thousand and one alerts
// recorded beats zero recorded, and the excess count is written to
// `ingest_rejections` so the loss is visible rather than inferred.
//
// B15 truncates two strings oto stores purely for observability — AM's `groupKey`
// is opaque and unbounded (C3), and neither field participates in any identity
// oto computes. It produces NO Note, because §L.3.2's B15 row assigns no
// rejection reason and `ingest_rejections_reason_ck` is a closed enum: inventing
// a member for it would be a migration, and a shortened display string is not a
// dropped observation. Truncated is recorded on the batch's own stored value.
func ApplyBatchBounds(env *Envelope) BatchOutcome {
	var out BatchOutcome

	if n := len(env.Alerts); n > domain.MaxAlertsPerBatch {
		out.Dropped = n - domain.MaxAlertsPerBatch
		env.Alerts = env.Alerts[:domain.MaxAlertsPerBatch]
		out.Notes = append(out.Notes, Note{
			Reason: domain.ReasonTooManyAlerts,
			Detail: fmt.Sprintf("batch carried %d alerts; truncated to %d, %d dropped",
				n, domain.MaxAlertsPerBatch, out.Dropped),
		})
	}

	env.Receiver = truncateBytes(env.Receiver, domain.MaxReceiverBytes)
	env.GroupKey = truncateBytes(env.GroupKey, domain.MaxGroupKeyBytes)
	return out
}

// ---------------------------------------------------------------- alert bounds

// AlertOptions is the per-source context the per-alert bounds need.
type AlertOptions struct {
	// Now is oto's clock, injected. B12 and B13 are windows around it, and a
	// hard-coded time.Now() here would make the whole bound untestable.
	Now time.Time
	// InjectLabels are `alert_sources.inject_labels`, merged BEFORE alert_key is
	// computed so that one Alertmanager can serve several logical clusters.
	InjectLabels map[string]string
}

// Normalised is one alert that survived B3-B14 and B18-B19.
type Normalised struct {
	Labels       alerts.LabelSet
	Annotations  map[string]string
	GeneratorURL string
	Status       string
	StartsAt     time.Time
	// EndsAt is the zero time when the upstream sent no end time, which is legal
	// and means "unknown for this payload" — never "forget the end time you had".
	EndsAt time.Time
	Value  *float64

	// Fingerprint is oto's own recomputation (C.3). WireFingerprint is what the
	// upstream claimed.
	Fingerprint     alerts.SourceFingerprint
	WireFingerprint string
	// FingerprintMismatch is true when the two disagree. oto stores ITS OWN and
	// records the mismatch as a metric; it NEVER fails the ingest on this (C10).
	FingerprintMismatch bool
	// Clamped is true when B13 pulled `endsAt` forward to `startsAt` because a
	// backward-skewed upstream clock claimed an end before the start (§B.3.2).
	Clamped bool

	// Notes are the non-fatal violations recorded alongside this kept alert.
	Notes []Note
}

// Normalise applies the per-alert bounds B3-B14, B18 and B19 to one wire alert.
//
// A returned error means THIS ALERT is dropped and recorded; the rest of the
// batch proceeds and the response is still 202. Use domain.ReasonFromError to
// turn the error into an `ingest_rejections.reason` — the shared kernel mints its
// validation codes as exactly those strings so that no mapping table is needed.
//
// Order matches §L.3.3: timestamp sanity (B12, B13) first, because a poisoned
// timestamp is what routes a row into a partition sixty years out, then the
// label and annotation bounds.
func Normalise(a Alert, opt AlertOptions) (Normalised, error) {
	out := Normalised{
		Status:          NormaliseStatus(a.Status),
		Value:           a.Value,
		WireFingerprint: a.Fingerprint,
	}

	startsAt, err := checkStartsAt(a.StartsAt, opt.Now)
	if err != nil {
		return Normalised{}, err
	}
	out.StartsAt = startsAt
	out.EndsAt, out.Clamped = clampEndsAt(a.EndsAt, startsAt, opt.Now, &out.Notes)

	// B14: truncate, keep the alert, no rejection row — §L.3.2's B14 row assigns
	// no reason and the reason enum is closed. generatorURL is a deep link, never
	// identity, so a shortened one costs a click and nothing else.
	out.GeneratorURL = truncateBytes(a.GeneratorURL, domain.MaxGeneratorURLBytes)

	// B3-B6, B9-B11 and B18 are the shared kernel's own invariants. Calling the
	// constructor rather than re-checking them here is what keeps layer 2 and
	// layer 3 from drifting: there is one definition of "a legal label set".
	labels, err := alerts.NewLabelSet(mergeLabels(a.Labels, opt.InjectLabels))
	if err != nil {
		return Normalised{}, err
	}
	out.Labels = labels
	out.Fingerprint = labels.Fingerprint()
	out.FingerprintMismatch = a.Fingerprint != "" && a.Fingerprint != out.Fingerprint.String()

	out.Annotations = boundAnnotations(a.Annotations, &out.Notes)
	return out, nil
}

// checkStartsAt is B12. A `startsAt` outside `now-365d … now+24h` drops the
// alert, because it is the one untrusted value that steers partition routing:
// an alert claiming 2087 would demand a partition sixty years out.
func checkStartsAt(startsAt, now time.Time) (time.Time, error) {
	floor := now.Add(-domain.MaxStartsAtPast)
	ceiling := now.Add(domain.MaxStartsAtFuture)

	if startsAt.Before(floor) || startsAt.After(ceiling) {
		return time.Time{}, errs.Newf(errs.KindValidation, domain.ReasonTimestampOutOfWindow.String(),
			"startsAt %s is outside the sanity window %s .. %s",
			startsAt.UTC().Format(time.RFC3339), floor.UTC().Format(time.RFC3339), ceiling.UTC().Format(time.RFC3339))
	}
	return startsAt.UTC(), nil
}

// clampEndsAt is B13. The zero time is LEGAL and means "no end time known for
// this payload". Anything else must satisfy `startsAt <= endsAt <= now+365d`;
// a violation CLAMPS to startsAt, records the reason, and keeps the alert.
//
// Clamping rather than rejecting is §B.3.2: a backward-skewed upstream clock must
// never abort an ingest transaction. The skew is measured and surfaced; it is
// never a reason to lose an alert.
func clampEndsAt(endsAt, startsAt, now time.Time, notes *[]Note) (time.Time, bool) {
	if endsAt.IsZero() {
		return time.Time{}, false
	}

	ceiling := now.Add(domain.MaxEndsAtFuture)
	switch {
	case endsAt.Before(startsAt):
		*notes = append(*notes, Note{
			Reason: domain.ReasonTimestampOutOfWindow,
			Detail: fmt.Sprintf("endsAt %s precedes startsAt %s; clamped",
				endsAt.UTC().Format(time.RFC3339), startsAt.UTC().Format(time.RFC3339)),
		})
		return startsAt, true
	case endsAt.After(ceiling):
		*notes = append(*notes, Note{
			Reason: domain.ReasonTimestampOutOfWindow,
			Detail: fmt.Sprintf("endsAt %s is beyond now+365d; clamped to startsAt",
				endsAt.UTC().Format(time.RFC3339)),
		})
		return startsAt, true
	default:
		return endsAt.UTC(), false
	}
}

// mergeLabels merges `inject_labels` under the upstream's own labels.
//
// UNDER, not over: an injected label loses to a label the upstream actually sent.
// oto adds context; it does not overwrite evidence, and a source misconfigured to
// inject `severity` must not be able to rewrite what Prometheus said.
func mergeLabels(wire, inject map[string]string) map[string]string {
	if len(inject) == 0 {
		return wire
	}
	out := make(map[string]string, len(wire)+len(inject))
	for name, value := range inject {
		out[name] = value
	}
	for name, value := range wire {
		out[name] = value
	}
	return out
}

// annotationPriority are the annotations oto itself reads. When B7 forces a drop
// these survive regardless of where they sort, because losing `summary` to
// alphabetical luck would blank the one line a human actually reads.
var annotationPriority = []string{
	alerts.AnnotationSummary,
	alerts.AnnotationDescription,
	alerts.AnnotationRunbookURL,
}

// boundAnnotations applies B7 (count), B8 (value length) and B19 (storability).
//
// All three KEEP the alert: an annotation is display text and is deliberately not
// part of any identity (§C.9.3), so no amount of annotation abuse justifies losing
// the signal underneath it. B19 is the reason this is not symmetric with the label
// path, where the same unstorable bytes reject the alert outright (B18) — a label
// value decides WHICH Alert this is, and oto will not rewrite an identity in order
// to store it. Prose it will rewrite, visibly, and say so.
//
// ⭐ THE ORDER IS BINDING: names are filtered BEFORE B7's count, and values are
// sanitised BEFORE B8's length bound.
//
//   - Filtering names first stops an annotation that is about to be dropped
//     anyway from consuming one of the 32 slots and evicting a good one.
//   - Sanitising before truncating is required because SanitiseText can GROW a
//     value — U+FFFD is three bytes where the byte it replaced was one — so a
//     value sanitised after the length check could leave here over the cap and
//     trip `alerts_annotations_ck` at layer 6.
func boundAnnotations(in map[string]string, notes *[]Note) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}

	// B19, names. A name is a jsonb KEY, so it is not sanitised: two different
	// unstorable names can sanitise to one string, and the second would silently
	// overwrite the first. Dropping the annotation loses one line of prose and
	// records that it did; merging two would lose one silently.
	storable := make(map[string]string, len(in))
	for name, value := range in {
		if why := alerts.UnstorableReason(name); why != "" {
			*notes = append(*notes, Note{
				Reason: domain.ReasonAnnotationUnstorable,
				Detail: fmt.Sprintf("annotation name %q contains %s; the annotation was dropped", name, why),
			})
			continue
		}
		storable[name] = value
	}

	kept := selectAnnotations(storable, notes)
	out := make(map[string]string, len(kept))
	for _, name := range kept {
		value := storable[name]

		// B19, values.
		if why := alerts.UnstorableReason(value); why != "" {
			value, _ = alerts.SanitiseText(value)
			*notes = append(*notes, Note{
				Reason: domain.ReasonAnnotationUnstorable,
				Detail: fmt.Sprintf("annotation %q value contains %s; replaced with U+FFFD", name, why),
			})
		}

		// B8.
		if len(value) > domain.MaxAnnotationValueBytes {
			value = truncateBytes(value, domain.MaxAnnotationValueBytes-len(TruncationMarker)) + TruncationMarker
			*notes = append(*notes, Note{
				Reason: domain.ReasonAnnotationTooLarge,
				Detail: fmt.Sprintf("annotation %q exceeded %d bytes and was truncated",
					name, domain.MaxAnnotationValueBytes),
			})
		}
		out[name] = value
	}
	return out
}

// selectAnnotations returns the names to keep under B7, priority names first and
// then the remainder in a stable sorted order so that the same payload always
// keeps the same annotations.
func selectAnnotations(in map[string]string, notes *[]Note) []string {
	if len(in) <= domain.MaxAnnotations {
		names := make([]string, 0, len(in))
		for name := range in {
			names = append(names, name)
		}
		return names
	}

	kept := make([]string, 0, domain.MaxAnnotations)
	seen := make(map[string]struct{}, domain.MaxAnnotations)
	for _, name := range annotationPriority {
		if _, ok := in[name]; ok {
			kept = append(kept, name)
			seen[name] = struct{}{}
		}
	}

	rest := make([]string, 0, len(in))
	for name := range in {
		if _, dup := seen[name]; !dup {
			rest = append(rest, name)
		}
	}
	slices.Sort(rest)

	for _, name := range rest {
		if len(kept) >= domain.MaxAnnotations {
			break
		}
		kept = append(kept, name)
	}

	*notes = append(*notes, Note{
		Reason: domain.ReasonTooManyAnnotations,
		Detail: fmt.Sprintf("alert carried %d annotations; kept %d, dropped %d",
			len(in), len(kept), len(in)-len(kept)),
	})
	return kept
}

// truncateBytes cuts s to at most n bytes without splitting a UTF-8 rune. A
// half-rune in a JSONB column is a value Postgres will refuse and a UI will
// render as a replacement character, so the cut is made on a boundary.
func truncateBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// FingerprintsOf projects a decoded envelope onto the C.5 dedup pre-image.
//
// It runs over the RAW wire alerts rather than normalised ones on purpose: the
// dedup key must be computable for a batch that is about to be rejected, or a
// hostile payload could evade replay suppression simply by being malformed.
func FingerprintsOf(env Envelope) []domain.AlertIdentity {
	out := make([]domain.AlertIdentity, 0, len(env.Alerts))
	for _, a := range env.Alerts {
		out = append(out, domain.AlertIdentity{
			Fingerprint: domain.RawFingerprint(a.Labels),
			Status:      NormaliseStatus(a.Status),
		})
	}
	return out
}

// AlertName reads the `alertname` label of a wire alert without constructing a
// LabelSet, for a log line or a rejection detail about an alert that never became
// one. It is deliberately bounded: an untrusted value must not reach a log
// unbounded.
func AlertName(a Alert) string {
	name := a.Labels[alerts.LabelAlertName]
	return strings.TrimSpace(truncateBytes(name, domain.MaxAlertNameBytes))
}
