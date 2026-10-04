package domain_test

// ADR 0053 §5's Classification set as pure rules (git-bug 4298aa0): what an operator may
// write, what a model may answer, and what a Finding is stored with.

import (
	"strings"
	"testing"

	"github.com/thulasiram/oto/internal/investigator/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

func TestOtoShipsNoClassesAndAnEmptySetAsksNothing(t *testing.T) {
	var zero domain.ClassSet
	if !zero.Empty() || len(zero.Classes()) != 0 {
		t.Fatal("the zero set is not empty")
	}
	// ⭐ Nothing is admissible from an empty set — not even unclassified: nobody asked.
	if zero.Admits(domain.Unclassified) || zero.Admits("noise") {
		t.Fatal("an empty set admitted a word")
	}
	if got := zero.Settle("capacity"); got != "" {
		t.Fatalf("an empty set settled %q, want no classification", got)
	}
}

func TestAnOperatorsSetIsValidatedClassByClass(t *testing.T) {
	ok, err := domain.NewClassSet([]domain.Class{
		{Name: " deploy-regression ", Description: " A change we shipped broke it. "},
		{Name: "capacity_2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := ok.Classes(); got[0].Name != "deploy-regression" || got[0].Description != "A change we shipped broke it." {
		t.Fatalf("the set is %+v, want names and descriptions trimmed", got)
	}

	for name, tc := range map[string]struct {
		classes []domain.Class
		field   string
		code    string
	}{
		"blank":         {[]domain.Class{{Name: "  "}}, "classes/0/name", "required"},
		"reserved":      {[]domain.Class{{Name: domain.Unclassified}}, "classes/0/name", "reserved"},
		"capital":       {[]domain.Class{{Name: "Noise"}}, "classes/0/name", "pattern"},
		"leading digit": {[]domain.Class{{Name: "5xx"}}, "classes/0/name", "pattern"},
		"space":         {[]domain.Class{{Name: "a b"}}, "classes/0/name", "pattern"},
		"too long":      {[]domain.Class{{Name: "a" + strings.Repeat("b", 63)}}, "classes/0/name", "pattern"},
		"duplicate":     {[]domain.Class{{Name: "capacity"}, {Name: "capacity"}}, "classes/1/name", "duplicate"},
		"description":   {[]domain.Class{{Name: "capacity", Description: strings.Repeat("x", 501)}}, "classes/0/description", "max_length"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := domain.NewClassSet(tc.classes)
			if errs.CodeOf(err) != "investigation_classes_invalid" {
				t.Fatalf("err = %v", err)
			}
			if !hasViolation(err, tc.field, tc.code) {
				t.Fatalf("err does not name %s (%s): %v", tc.field, tc.code, err)
			}
		})
	}

	many := make([]domain.Class, domain.MaxClasses+1)
	for i := range many {
		many[i] = domain.Class{Name: "c" + strings.Repeat("x", i)}
	}
	if _, err := domain.NewClassSet(many); !hasViolation(err, "classes", "max_items") {
		t.Fatalf("%d classes accepted: %v", len(many), err)
	}
}

func hasViolation(err error, field, code string) bool {
	e, ok := errs.As(err)
	if !ok {
		return false
	}
	for _, v := range e.Violations {
		if v.Field == field && v.Code == code {
			return true
		}
	}
	return false
}

func TestAModelMayAnswerOnlyTheOperatorsWordsOrUnclassified(t *testing.T) {
	set, err := domain.NewClassSet([]domain.Class{{Name: "deploy-regression"}, {Name: "capacity"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(set.Answers(), ","); got != "deploy-regression,capacity,unclassified" {
		t.Fatalf("answers = %s", got)
	}
	for word, want := range map[string]string{
		"capacity":          "capacity",
		"unclassified":      "unclassified",
		"":                  "unclassified", // silence
		"noise":             "unclassified", // ⛔ a word outside the set is never stored
		"Capacity":          "unclassified", // exactly as written
		"deploy-regression": "deploy-regression",
	} {
		if got := set.Settle(word); got != want {
			t.Errorf("Settle(%q) = %q, want %q", word, got, want)
		}
	}

	schema, err := set.ClassifySchema()
	if err != nil {
		t.Fatal(err)
	}
	if schema.Name != domain.ClassifyTool || !strings.Contains(string(schema.Parameters), `"enum":["deploy-regression","capacity","unclassified"]`) {
		t.Fatalf("schema = %s %s", schema.Name, schema.Parameters)
	}
	if got := domain.ParseClassifyCall(`{"class":" capacity "}`); got != "capacity" {
		t.Fatalf("ParseClassifyCall = %q", got)
	}
	for _, bad := range []string{``, `not json`, `{"class":7}`, `{"kind":"capacity"}`} {
		if got := domain.ParseClassifyCall(bad); got != "" {
			t.Errorf("ParseClassifyCall(%q) = %q, want none", bad, got)
		}
	}
	prompt := set.ClassifyPrompt()
	for _, w := range []string{domain.ClassifyTool, "- deploy-regression", "- capacity", "- unclassified", "right answer under doubt"} {
		if !strings.Contains(prompt, w) {
			t.Errorf("the prompt does not say %q:\n%s", w, prompt)
		}
	}
}

func TestAStoredClassificationIsNotReReadAgainstTodaysSet(t *testing.T) {
	// A Finding keeps its class when the set changes: restoring one checks only that it
	// is a class NAME, never that today's set still has it.
	for _, stored := range []string{"", "deploy-regression", "unclassified", "a-class-since-removed"} {
		if got, err := domain.RestoreClassification(stored); err != nil || got != stored {
			t.Errorf("RestoreClassification(%q) = %q, %v", stored, got, err)
		}
	}
	if _, err := domain.RestoreClassification("Not A Name"); err == nil {
		t.Error("a corrupt classification was restored")
	}
}
