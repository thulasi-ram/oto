package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/service"
	"github.com/thulasiram/oto/internal/platform/db"
)

type fakeStore struct {
	rows []domain.Wording
	err  error
}

func (f fakeStore) Resolve(context.Context, db.TenantScope, uuid.UUID) ([]domain.Wording, error) {
	return f.rows, f.err
}

var (
	testOrg     = uuid.MustParse("019fe297-d84f-7599-b5b2-1f231749104a")
	testChannel = uuid.MustParse("019fe297-d84f-7599-b5b2-1f231749104b")
)

func scope(t *testing.T) db.TenantScope {
	t.Helper()
	s, err := db.NewTenantScope(testOrg)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	return s
}

func wordingRow(stanza, tmpl string, ch *uuid.UUID, prio int, ms []domain.Matcher, reasons []string) domain.Wording {
	return domain.Wording{
		ID: uuid.New(), OrgID: testOrg, ChannelID: ch, Stanza: stanza, Template: tmpl,
		Matchers: ms, Reasons: reasons, Priority: prio, Enabled: true,
	}
}

func view(reason string, labels map[string]string) *domain.NotificationView {
	return &domain.NotificationView{
		Org:    domain.OrgRef{ID: testOrg.String(), Slug: "acme", Name: "Acme"},
		Reason: reason,
		Group:  domain.GroupView{ID: "g", State: "open", GroupLabels: labels},
		Alerts: []domain.AlertView{{
			ID: "a", AlertName: "HighErrorRate", Severity: "critical",
			Service: "checkout", Namespace: "payments", Labels: labels,
		}},
	}
}

// TestTheDestinationsOwnWordingBeatsTheHouseVoice — ADR 0049's precedence, which
// the store expresses as an ORDER BY and this walk honours.
func TestTheDestinationsOwnWordingBeatsTheHouseVoice(t *testing.T) {
	w := service.NewWordings(fakeStore{rows: []domain.Wording{
		wordingRow("body", "the destination's own", &testChannel, 100, nil, nil),
		wordingRow("body", "the house voice", nil, 10, nil, nil),
	}})
	got := w.For(context.Background(), scope(t), testChannel, view("fired", nil))
	if got["body"] != "the destination's own" {
		t.Errorf("a rule naming one destination is more specific than one naming a tenant; got %q", got["body"])
	}
}

// TestLowerPriorityWinsWithinAScope — the same sentence routing uses.
func TestLowerPriorityWinsWithinAScope(t *testing.T) {
	w := service.NewWordings(fakeStore{rows: []domain.Wording{
		wordingRow("body", "first", nil, 5, nil, nil),
		wordingRow("body", "second", nil, 50, nil, nil),
	}})
	got := w.For(context.Background(), scope(t), testChannel, view("fired", nil))
	if got["body"] != "first" {
		t.Errorf("priority orders LOWER FIRST and the first match wins; got %q", got["body"])
	}
}

// TestResolutionIsPerStanza — one winner each, and they can come from different scopes.
func TestResolutionIsPerStanza(t *testing.T) {
	w := service.NewWordings(fakeStore{rows: []domain.Wording{
		wordingRow("body", "channel body", &testChannel, 100, nil, nil),
		wordingRow("title", "house title", nil, 100, nil, nil),
		wordingRow("footer", "house footer", nil, 100, nil, nil),
	}})
	got := w.For(context.Background(), scope(t), testChannel, view("fired", nil))
	want := map[string]string{"body": "channel body", "title": "house title", "footer": "house footer"}
	if len(got) != len(want) {
		t.Fatalf("got %d stanzas, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestAnEmptyClauseMatchesEverything — what makes one row a house voice.
func TestAnEmptyClauseMatchesEverything(t *testing.T) {
	w := service.NewWordings(fakeStore{rows: []domain.Wording{
		wordingRow("body", "always", nil, 100, nil, nil),
	}})
	for _, reason := range []string{"fired", "all_resolved", "acked", "expired", "digest"} {
		if got := w.For(context.Background(), scope(t), testChannel, view(reason, nil)); got["body"] != "always" {
			t.Errorf("reason %q did not match an empty clause: %v", reason, got)
		}
	}
}

func TestTheWhenClauseSelects(t *testing.T) {
	labels := map[string]string{"service": "checkout", "env": "prod"}

	cases := []struct {
		name     string
		matchers []domain.Matcher
		reasons  []string
		reason   string
		want     bool
	}{
		{"equal matches", []domain.Matcher{{Name: "service", Op: domain.MatchEq, Value: "checkout"}}, nil, "fired", true},
		{"equal misses", []domain.Matcher{{Name: "service", Op: domain.MatchEq, Value: "billing"}}, nil, "fired", false},
		{"not-equal matches", []domain.Matcher{{Name: "env", Op: domain.MatchNotEq, Value: "staging"}}, nil, "fired", true},
		{"regex matches", []domain.Matcher{{Name: "service", Op: domain.MatchRe, Value: "check.*"}}, nil, "fired", true},
		{"regex is anchored", []domain.Matcher{{Name: "service", Op: domain.MatchRe, Value: "check"}}, nil, "fired", false},
		{"negated regex matches", []domain.Matcher{{Name: "service", Op: domain.MatchNotRe, Value: "bill.*"}}, nil, "fired", true},
		{"a broken regex selects nothing", []domain.Matcher{{Name: "service", Op: domain.MatchRe, Value: "([unclosed"}}, nil, "fired", false},
		{"a broken NEGATED regex also selects nothing", []domain.Matcher{{Name: "service", Op: domain.MatchNotRe, Value: "([unclosed"}}, nil, "fired", false},
		{"an absent label is empty, not a wildcard", []domain.Matcher{{Name: "team", Op: domain.MatchEq, Value: "sre"}}, nil, "fired", false},
		{"an absent label equals empty", []domain.Matcher{{Name: "team", Op: domain.MatchEq, Value: ""}}, nil, "fired", true},
		{"all matchers must hold", []domain.Matcher{
			{Name: "service", Op: domain.MatchEq, Value: "checkout"},
			{Name: "env", Op: domain.MatchEq, Value: "staging"},
		}, nil, "fired", false},
		{"reason narrows", nil, []string{"acked"}, "acked", true},
		{"reason excludes", nil, []string{"acked"}, "fired", false},
		{"reason and matchers both apply", []domain.Matcher{{Name: "env", Op: domain.MatchEq, Value: "prod"}}, []string{"fired"}, "fired", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := service.NewWordings(fakeStore{rows: []domain.Wording{
				wordingRow("body", "selected", nil, 100, c.matchers, c.reasons),
			}})
			got := w.For(context.Background(), scope(t), testChannel, view(c.reason, labels))
			if (got["body"] == "selected") != c.want {
				t.Errorf("selected=%v, want %v (got %v)", got["body"] == "selected", c.want, got)
			}
		})
	}
}

// TestOtoOwnNamesAreReachableButUpstreamWins.
func TestOtoOwnNamesAreReachableButUpstreamWins(t *testing.T) {
	w := service.NewWordings(fakeStore{rows: []domain.Wording{
		wordingRow("body", "by severity", nil, 100,
			[]domain.Matcher{{Name: "severity", Op: domain.MatchEq, Value: "critical"}}, nil),
	}})
	if got := w.For(context.Background(), scope(t), testChannel, view("fired", nil)); got["body"] == "" {
		t.Error("severity should be reachable as a matcher name when no label carries it")
	}
	// A real label of the same name wins over oto's derived value.
	v := view("fired", map[string]string{"severity": "warning"})
	if got := w.For(context.Background(), scope(t), testChannel, v); got["body"] != "" {
		t.Error("the signal's own label must win over oto's derived value")
	}
}

// TestAnUnreadableStoreIsNotAFailedDelivery.
func TestAnUnreadableStoreIsNotAFailedDelivery(t *testing.T) {
	w := service.NewWordings(fakeStore{err: errors.New("database is on fire")})
	if got := w.For(context.Background(), scope(t), testChannel, view("fired", nil)); got != nil {
		t.Errorf("a store failure must yield no wordings, not a partial set: %v", got)
	}
}

// TestARefusedStanzaIsNeverResolved even if a row somehow exists for one.
func TestARefusedStanzaIsNeverResolved(t *testing.T) {
	w := service.NewWordings(fakeStore{rows: []domain.Wording{
		wordingRow("fields", "REPLACED", nil, 100, nil, nil),
		wordingRow("members", "REPLACED", nil, 100, nil, nil),
		wordingRow("trail", "REPLACED", nil, 100, nil, nil),
		wordingRow("actions", "REPLACED", nil, 100, nil, nil),
	}})
	if got := w.For(context.Background(), scope(t), testChannel, view("fired", nil)); got != nil {
		t.Errorf("a refused stanza was resolved: %v", got)
	}
}

// TestADisabledOrDeletedWordingIsSkipped — belt and braces over the partial index.
func TestADisabledOrDeletedWordingIsSkipped(t *testing.T) {
	off := wordingRow("body", "disabled", nil, 1, nil, nil)
	off.Enabled = false
	on := wordingRow("body", "live", nil, 2, nil, nil)
	w := service.NewWordings(fakeStore{rows: []domain.Wording{off, on}})
	if got := w.For(context.Background(), scope(t), testChannel, view("fired", nil)); got["body"] != "live" {
		t.Errorf("a disabled wording won: %v", got)
	}
}

// TestNoChannelMeansNoWordings — a delivery with no destination resolves nothing
// rather than falling back to the house voice, because "no destination" is a bug
// upstream and inventing a card's voice would hide it.
func TestNoChannelMeansNoWordings(t *testing.T) {
	w := service.NewWordings(fakeStore{rows: []domain.Wording{
		wordingRow("body", "house", nil, 100, nil, nil),
	}})
	if got := w.For(context.Background(), scope(t), uuid.Nil, view("fired", nil)); got != nil {
		t.Errorf("resolved against no destination: %v", got)
	}
}

// TestSaveTimeValidationRefusesWhatDeliveryWouldSurvive.
func TestSaveTimeValidationRefusesWhatDeliveryWouldSurvive(t *testing.T) {
	for _, c := range []struct{ name, stanza, tmpl string }{
		{"a refused stanza", "fields", `{{ alert.name }}`},
		{"an unknown filter", "body", `{{ alert.name | no_such_filter }}`},
		{"a misspelled field", "body", `{{ alert.nmae | default: "-" }}`},
		{"an unclosed expression", "body", `{{ alert.name`},
		{"nothing at all", "body", ``},
	} {
		t.Run(c.name, func(t *testing.T) {
			if v := service.ValidateWording(c.stanza, c.tmpl, nil, nil, 100); len(v) == 0 {
				t.Error("accepted")
			}
		})
	}
	if v := service.ValidateWording("body",
		`{{ alert.name | default: "a signal" }} is {{ case.ack_state | default: "unacked" }}`,
		[]domain.Matcher{{Name: "env", Op: domain.MatchEq, Value: "prod"}}, []string{"fired"}, 100,
	); len(v) != 0 {
		t.Errorf("a reasonable wording was refused: %+v", v)
	}
}
