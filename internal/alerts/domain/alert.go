package domain

import (
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// MaxGeneratorURLBytes bounds the `generatorURL` an upstream supplies (bound B14).
const MaxGeneratorURLBytes = 8192

// Alert is THE IDENTITY OF A LABEL SET within (Org, Cluster).
//
// It is created on first sight and survives resolution forever — oto's answer to
// Sentry's Issue. Two label sets that differ only in an ignored label are the
// same Alert; the same label set in `prod-eu` and in `prod-us` are DIFFERENT
// Alerts, because they have different blast radii (C.2).
//
// The state and ack_state carried here are PROJECTIONS of the current open
// AlertOccurrence — or of the most recent one, when none is open. The
// authoritative machine runs on the Occurrence.
type Alert struct {
	id        uuid.UUID
	orgID     uuid.UUID
	clusterID uuid.UUID

	key         AlertKey
	fingerprint SourceFingerprint
	clusterKey  ClusterKey

	labels       LabelSet
	annotations  Annotations
	generatorURL string

	state               State
	ackState            AckState
	currentOccurrenceID uuid.UUID
	// snoozedUntil is the §B.8 projection of the active alert_snoozes row. It is
	// the THIRD ORTHOGONAL AXIS and it is NOT a state: a snoozed alert is still
	// firing, still whatever severity it was, and is still rendered that way.
	snoozedUntil time.Time

	firstSeenAt       time.Time
	lastSeenAt        time.Time
	lastStateChangeAt time.Time
	totalOccurrences  int
	flapScore         float32
	isFlapping        bool
}

// AlertParams is the full constructor input for an Alert. It is also the
// rehydration shape: a repository maps a row into it and the constructor
// re-proves every invariant.
type AlertParams struct {
	ID        uuid.UUID
	OrgID     uuid.UUID
	ClusterID uuid.UUID

	Key         AlertKey
	Fingerprint SourceFingerprint
	ClusterKey  ClusterKey

	Labels      LabelSet
	Annotations Annotations
	// GeneratorURL is the upstream link back to the expression that fired.
	GeneratorURL string

	State    State
	AckState AckState
	// CurrentOccurrenceID is the open occurrence, or uuid.Nil when none is open.
	CurrentOccurrenceID uuid.UUID
	// SnoozedUntil is the projection of the active alert_snoozes row, or the zero
	// time when the Alert is awake (§B.8).
	SnoozedUntil time.Time

	FirstSeenAt       time.Time
	LastSeenAt        time.Time
	LastStateChangeAt time.Time
	TotalOccurrences  int
	// FlapScore is an EWMA of state transitions per hour. It is a derived signal,
	// NEVER a state (§B.1).
	FlapScore  float32
	IsFlapping bool
}

// NewAlert builds an Alert, enforcing every §D.4 invariant.
func NewAlert(p AlertParams) (Alert, error) {
	if err := requireID("alert id", p.ID); err != nil {
		return Alert{}, err
	}
	if err := requireID("org_id", p.OrgID); err != nil {
		return Alert{}, err
	}
	if err := requireID("cluster_id", p.ClusterID); err != nil {
		return Alert{}, err
	}
	if p.Key.IsZero() {
		return Alert{}, errs.New(errs.KindValidation, "required", "alert_key is required")
	}
	if p.Fingerprint.IsZero() {
		return Alert{}, errs.New(errs.KindValidation, "required", "source_fingerprint is required")
	}
	if p.ClusterKey.IsZero() {
		return Alert{}, errs.New(errs.KindValidation, "required", "cluster_key is required")
	}
	if p.Labels.IsZero() {
		return Alert{}, errs.New(errs.KindValidation, "required", "an alert requires its label set")
	}
	if !p.State.IsOpen() && !p.State.IsTerminal() {
		return Alert{}, errs.New(errs.KindValidation, "required", "alert state is required")
	}
	if len(p.GeneratorURL) > MaxGeneratorURLBytes {
		return Alert{}, errs.Newf(errs.KindValidation, "max_length",
			"generator_url must have at most %d characters", MaxGeneratorURLBytes)
	}
	if p.TotalOccurrences < 0 {
		return Alert{}, errs.New(errs.KindValidation, "min", "total_occurrences must be >= 0")
	}
	if p.FlapScore < 0 {
		return Alert{}, errs.New(errs.KindValidation, "min", "flap_score must be >= 0")
	}
	if p.FirstSeenAt.IsZero() {
		return Alert{}, errs.New(errs.KindValidation, "required", "first_seen_at is required")
	}
	if p.LastSeenAt.Before(p.FirstSeenAt) {
		return Alert{}, errs.New(errs.KindValidation, "field_order",
			"last_seen_at must be >= first_seen_at")
	}
	if p.LastStateChangeAt.Before(p.FirstSeenAt) {
		return Alert{}, errs.New(errs.KindValidation, "field_order",
			"last_state_change_at must be >= first_seen_at")
	}

	ackState := p.AckState
	if ackState.IsZero() {
		ackState = AckStateUnacked
	}

	return Alert{
		id:                  p.ID,
		orgID:               p.OrgID,
		clusterID:           p.ClusterID,
		key:                 p.Key,
		fingerprint:         p.Fingerprint,
		clusterKey:          p.ClusterKey,
		labels:              p.Labels,
		annotations:         p.Annotations,
		generatorURL:        p.GeneratorURL,
		state:               p.State,
		ackState:            ackState,
		currentOccurrenceID: p.CurrentOccurrenceID,
		snoozedUntil:        utcOrZero(p.SnoozedUntil),
		firstSeenAt:         p.FirstSeenAt.UTC(),
		lastSeenAt:          p.LastSeenAt.UTC(),
		lastStateChangeAt:   p.LastStateChangeAt.UTC(),
		totalOccurrences:    p.TotalOccurrences,
		flapScore:           p.FlapScore,
		isFlapping:          p.IsFlapping,
	}, nil
}

// ID is the Alert's uuidv7.
func (a Alert) ID() uuid.UUID { return a.id }

// OrgID is the tenant this Alert belongs to.
func (a Alert) OrgID() uuid.UUID { return a.orgID }

// ClusterID is the Cluster this Alert's identity is scoped to.
func (a Alert) ClusterID() uuid.UUID { return a.clusterID }

// Key is the Alert's identity — the primary dedup key (C.2).
func (a Alert) Key() AlertKey { return a.key }

// Fingerprint is Alertmanager's fingerprint as oto recomputed it (C.3).
func (a Alert) Fingerprint() SourceFingerprint { return a.fingerprint }

// ClusterKey is the Cluster's identity string, which participates in Key.
func (a Alert) ClusterKey() ClusterKey { return a.clusterKey }

// Labels is the full label set, INCLUDING the labels the source ignores for
// identity. Ignored labels are stored; they are merely not hashed.
func (a Alert) Labels() LabelSet { return a.labels }

// Annotations is the Alert's display text.
func (a Alert) Annotations() Annotations { return a.annotations }

// GeneratorURL links back to the expression that fired.
func (a Alert) GeneratorURL() string { return a.generatorURL }

// AlertName is the `alertname` label — how a human recognises this Alert.
func (a Alert) AlertName() string { return a.labels.AlertName() }

// Severity is the promoted `severity` label, mapped onto the closed class set.
func (a Alert) Severity() Severity { return a.labels.Severity() }

// Namespace is the promoted `namespace` label.
func (a Alert) Namespace() string { return a.labels.Namespace() }

// Service is the promoted `service` label.
func (a Alert) Service() string { return a.labels.Service() }

// State is the projection of the current occurrence's state, or of the most
// recent occurrence when none is open.
func (a Alert) State() State { return a.state }

// AckState is the projection of the current occurrence's ack state.
func (a Alert) AckState() AckState { return a.ackState }

// CurrentOccurrenceID is the open AlertOccurrence, or uuid.Nil when none is open.
func (a Alert) CurrentOccurrenceID() uuid.UUID { return a.currentOccurrenceID }

// FirstSeenAt is when oto first saw this label set. It never moves.
func (a Alert) FirstSeenAt() time.Time { return a.firstSeenAt }

// LastSeenAt is when oto last heard about this label set.
func (a Alert) LastSeenAt() time.Time { return a.lastSeenAt }

// LastStateChangeAt is when the projected state last changed.
func (a Alert) LastStateChangeAt() time.Time { return a.lastStateChangeAt }

// TotalOccurrences is how many firing episodes this Alert has had.
func (a Alert) TotalOccurrences() int { return a.totalOccurrences }

// FlapScore is an EWMA of state transitions per hour — a derived signal, never a
// state.
func (a Alert) FlapScore() float32 { return a.flapScore }

// IsFlapping reports whether the Alert is above flap_threshold. Flapping is a
// VISIBLE UI state; damping is never silent (§B.6).
func (a Alert) IsFlapping() bool { return a.isFlapping }

// HasOpenOccurrence reports whether an episode is currently running.
func (a Alert) HasOpenOccurrence() bool { return a.currentOccurrenceID != uuid.Nil }

// SnoozedUntil is when oto's notifications about this Alert resume, or the zero
// time when it is awake. See IsSnoozedAt for the question you almost always mean.
func (a Alert) SnoozedUntil() time.Time { return a.snoozedUntil }

// IsSnoozedAt reports whether oto is holding its tongue about this Alert at the
// given instant. The instant is a PARAMETER: the domain never calls time.Now().
//
// This answers "is oto notifying?", never "what is the world doing?". The alert
// is still firing, still whatever severity it was, and every surface MUST keep
// rendering it that way (§B.8.1, §B.8.6).
func (a Alert) IsSnoozedAt(now time.Time) bool {
	return !a.snoozedUntil.IsZero() && a.snoozedUntil.After(now.UTC())
}

// Project returns the Alert with a new projection applied. It re-proves the
// Alert's invariants, so a projection can never make an Alert the database would
// refuse.
func (a Alert) Project(p AlertProjection) (Alert, error) {
	return NewAlert(AlertParams{
		ID:                  a.id,
		OrgID:               a.orgID,
		ClusterID:           a.clusterID,
		Key:                 a.key,
		Fingerprint:         a.fingerprint,
		ClusterKey:          a.clusterKey,
		Labels:              a.labels,
		Annotations:         a.annotations,
		GeneratorURL:        a.generatorURL,
		State:               p.State,
		AckState:            p.AckState,
		CurrentOccurrenceID: derefID(p.CurrentOccurrenceID),
		SnoozedUntil:        derefTime(p.SnoozedUntil),
		FirstSeenAt:         a.firstSeenAt,
		LastSeenAt:          p.LastSeenAt,
		LastStateChangeAt:   p.LastStateChangeAt,
		TotalOccurrences:    p.TotalOccurrences,
		FlapScore:           a.flapScore,
		IsFlapping:          a.isFlapping,
	})
}

// WithFlap returns the Alert carrying a recomputed flap score.
func (a Alert) WithFlap(score float32, flapping bool) (Alert, error) {
	if score < 0 {
		return Alert{}, errs.New(errs.KindValidation, "min", "flap_score must be >= 0")
	}
	a.flapScore = score
	a.isFlapping = flapping
	return a, nil
}

// Materially reports whether an incoming observation differs from this Alert in a
// way that deserves an `alert.mutated` event (§B.3 T2). Only these fields count:
// severity, any annotation, generator_url and the bound rule fingerprint. A
// repeat observation that changes nothing material emits NO event, which is what
// keeps a five-second scrape interval from drowning the timeline.
func (a Alert) Materially(labels LabelSet, annotations Annotations, generatorURL string) bool {
	if a.labels.Severity() != labels.Severity() {
		return true
	}
	if a.generatorURL != generatorURL {
		return true
	}
	return !a.annotations.Equal(annotations)
}
