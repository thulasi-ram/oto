package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/thulasiram/oto/internal/notification/domain"
)

// TestAnIncidentIsRoutedOnTheLabelsItsWholeStoryShares — ADR 0052 §5. A policy
// routes the whole Incident, so it matches against the labels EVERY current member
// carries with the same value: one member's label must not route the story
// somewhere the others never belonged, and a removed member says nothing about it.
func TestAnIncidentIsRoutedOnTheLabelsItsWholeStoryShares(t *testing.T) {
	t.Parallel()

	f := domain.IncidentFacts{Members: []domain.IncidentMemberFacts{
		{Labels: map[string]string{"namespace": "checkout", "pod": "a", "team": "payments"}},
		{Labels: map[string]string{"namespace": "checkout", "pod": "b", "team": "payments"}},
		{
			// A tombstone: removed, so it does not narrow what the story is about.
			Labels:    map[string]string{"namespace": "storage"},
			RemovedAt: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC),
		},
	}}
	assert.Equal(t, map[string]string{"namespace": "checkout", "team": "payments"}, f.MatchLabels())

	// No current member: no labels, never nil — a catch-all policy still claims it.
	empty := domain.IncidentFacts{}
	assert.NotNil(t, empty.MatchLabels())
	assert.Empty(t, empty.MatchLabels())
}
