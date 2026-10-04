package api

// THE CLASSIFICATION SET'S TRANSPORT, CHECKED AGAINST THE CONTRACT (ADR 0053 §5,
// git-bug 4298aa0).
//
//   - read and replace answer the shape the contract declares;
//   - a fresh org has NO classes — oto ships none — and an empty set is a legal write;
//   - `unclassified` is reserved, a name outside the alphabet and a duplicate are 422s,
//     each naming the class it is about, and nothing reaches the service;
//   - an Investigation carries `classification`, null when none was asked for.

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/contract/schema"
)

// fxClasses is the fake's per-org set. A package var, behind its own lock, because
// fakeInvestigators is shared by the other contract files and carries no field for it.
var fxClasses = struct {
	mu   sync.Mutex
	sets map[*fakeInvestigators]domain.ClassSet
}{sets: map[*fakeInvestigators]domain.ClassSet{}}

func (f *fakeInvestigators) ClassSet(_ context.Context, s db.TenantScope) (domain.ClassSet, error) {
	if !mine(s) {
		return domain.ClassSet{}, nil
	}
	fxClasses.mu.Lock()
	defer fxClasses.mu.Unlock()
	return fxClasses.sets[f], nil
}

func (f *fakeInvestigators) ReplaceClassSet(_ context.Context, s db.TenantScope, set domain.ClassSet) (domain.ClassSet, error) {
	f.record("replaceInvestigationClasses")
	if !mine(s) {
		return domain.ClassSet{}, errs.Internal("wrong_org", nil)
	}
	fxClasses.mu.Lock()
	defer fxClasses.mu.Unlock()
	fxClasses.sets[f] = set
	return set, nil
}

func TestTheClassSetIsReadAndReplacedWholeAndShipsEmpty(t *testing.T) {
	t.Parallel()
	_, c := newClient(t)

	resp := c.GET("/investigation-classes").MustStatus(t, http.StatusOK)
	schema.Assert(t, "getInvestigationClasses", http.StatusOK, resp.Body())
	if got := resp.JSON(t)["data"].(map[string]any)["classes"].([]any); len(got) != 0 {
		t.Fatalf("a fresh org has classes %v — oto ships none", got)
	}

	resp = c.PUT(t, "/investigation-classes", map[string]any{"classes": []map[string]any{
		{"name": "deploy-regression", "description": "A change we shipped broke it."},
		{"name": "capacity"},
	}}).MustStatus(t, http.StatusOK)
	schema.Assert(t, "replaceInvestigationClasses", http.StatusOK, resp.Body())
	got := resp.JSON(t)["data"].(map[string]any)["classes"].([]any)
	if len(got) != 2 || got[0].(map[string]any)["name"] != "deploy-regression" ||
		got[1].(map[string]any)["description"] != "" {
		t.Fatalf("the set came back as %v, want the operator's two in their order", got)
	}

	resp = c.GET("/investigation-classes").MustStatus(t, http.StatusOK)
	schema.Assert(t, "getInvestigationClasses", http.StatusOK, resp.Body())
	if n := len(resp.JSON(t)["data"].(map[string]any)["classes"].([]any)); n != 2 {
		t.Fatalf("the set read back has %d classes, want 2", n)
	}

	// An empty set is how an operator stops Findings being classified.
	resp = c.PUT(t, "/investigation-classes", map[string]any{"classes": []any{}}).MustStatus(t, http.StatusOK)
	schema.Assert(t, "replaceInvestigationClasses", http.StatusOK, resp.Body())
}

func TestAClassSetOtoCannotOfferAModelIsRefusedByName(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		body  map[string]any
		field string
	}{
		"unclassified is reserved": {map[string]any{"classes": []map[string]any{{"name": "unclassified"}}}, "classes/0/name"},
		"a capital":                {map[string]any{"classes": []map[string]any{{"name": "Noise"}}}, "classes/0/name"},
		"a space":                  {map[string]any{"classes": []map[string]any{{"name": "a b"}}}, "classes/0/name"},
		"a duplicate": {map[string]any{"classes": []map[string]any{
			{"name": "capacity"}, {"name": "capacity"}}}, "classes/1/name"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, c := newClient(t)
			resp := c.PUT(t, "/investigation-classes", tc.body).MustStatus(t, http.StatusUnprocessableEntity)
			schema.Assert(t, "replaceInvestigationClasses", http.StatusUnprocessableEntity, resp.Body())
			if body := string(resp.Body()); !strings.Contains(body, tc.field) {
				t.Fatalf("the 422 does not name %s: %s", tc.field, body)
			}
			if n := f.callCount(); n != 0 {
				t.Fatalf("a refused set reached the service (%d call(s))", n)
			}
		})
	}

	// A body without `classes` is not "an empty set": it is no set at all.
	f, c := newClient(t)
	c.PUT(t, "/investigation-classes", map[string]any{}).MustStatus(t, http.StatusUnprocessableEntity)
	if f.callCount() != 0 {
		t.Fatal("a body naming no set reached the service")
	}
}

func TestAnInvestigationCarriesItsClassificationOrNull(t *testing.T) {
	t.Parallel()
	_, c := newClient(t)
	resp := c.GET("/investigations/"+fxInvestigation.String()).MustStatus(t, http.StatusOK)
	if got := resp.JSON(t)["data"].(map[string]any)["classification"]; got != "deploy-regression" {
		t.Fatalf("classification = %v, want the class the Finding was given", got)
	}
	resp = c.POST(t, "/cases/"+fxCase.String()+"/investigations", map[string]any{"investigator_id": fxInvestigator.String()}).
		MustStatus(t, http.StatusAccepted)
	data := resp.JSON(t)["data"].(map[string]any)
	if v, ok := data["classification"]; !ok || v != nil {
		t.Fatalf("a run with no Finding answered classification %v (present %v), want null", v, ok)
	}
}
