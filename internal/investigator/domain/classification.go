package domain

// CLASSIFICATION IS THE OPERATOR'S VOCABULARY (ADR 0053 §5; git-bug 4298aa0).
//
// "A Finding's class is chosen from a closed set the operator wrote, or `unclassified`,
// which is always admissible and is the right answer under doubt. oto ships no classes —
// no `noise`, which would be oto's opinion of someone else's signal. A Finding keeps its
// class if the set later changes."
//
// ⭐⭐ THREE ANSWERS, AND THEY ARE NOT INTERCHANGEABLE:
//
//   - NO CLASSIFICATION ("" here, NULL in the row): the org wrote no classes, so none was
//     offered and none was asked for. Not `unclassified` — nobody was asked.
//   - `unclassified`: a set WAS offered and the run named nothing in it — out of doubt,
//     because it never said, or because it said a word outside the set.
//   - one of the operator's classes, exactly as they spelled it.
//
// ⛔ oto SHIPS NO CLASS. There is no default set, no seed, no constant naming a class here
// or anywhere else; ClassSet's zero value is the empty set and that is what an org has
// until its operator writes one.
//
// ⛔ A MODEL NEVER WIDENS THE SET. Its pick is checked against the set the run read
// (ClassSet.Admits); a word outside it is refused on the record and answered with the
// set, and a run that ends without a word inside it is `unclassified`. A value outside
// the set is never stored.
//
// ⛔ A CLASSIFICATION IS A MODEL'S JUDGEMENT AND NEVER DECIDES DELIVERY (ADR 0053 §2). It
// travels outbound with the Finding; a receiver that pages on it is paging on a model's
// judgement, and the docs say so.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// Unclassified is the one class oto names: always admissible, never the operator's to
// add or remove, and the right answer under doubt.
const Unclassified = "unclassified"

// MaxClasses bounds an org's set (`investigation_classes_position_ck`). Fifty is more
// than any operator should want a model choosing between, and every one of them is in
// every run's prompt.
const MaxClasses = 50

// MaxClassDescription mirrors `investigation_classes_desc_ck`, in characters.
const MaxClassDescription = 500

// classNamePattern is `investigation_classes_name_ck` and `investigations_class_ck`: a
// word a model can say back exactly, a card can quote and a receiver can match on.
var classNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// Class is one class the operator wrote: its name and what it means, which is what
// the model is told.
type Class struct {
	Name        string
	Description string
}

// ClassSet is an org's closed class set, in the operator's order. Build one with
// NewClassSet; the zero value is the empty set — no classes, no classification.
type ClassSet struct {
	classes []Class
}

// NewClassSet validates an operator's set: every name non-empty, in the alphabet,
// unique and not `unclassified`; every description within its bound; at most
// MaxClasses. Each violation names the class it is about, so a form can point at it.
func NewClassSet(classes []Class) (ClassSet, error) {
	var v []errs.Violation
	if len(classes) > MaxClasses {
		v = append(v, errs.Violation{Field: "classes", Code: "max_items",
			Message: fmt.Sprintf("at most %d classes", MaxClasses)})
	}
	seen := make(map[string]int, len(classes))
	out := make([]Class, 0, len(classes))
	for i, c := range classes {
		field := fmt.Sprintf("classes/%d", i)
		name := strings.TrimSpace(c.Name)
		desc := strings.TrimSpace(c.Description)
		switch {
		case name == "":
			v = append(v, errs.Violation{Field: field + "/name", Code: "required", Message: "a class needs a name"})
		case name == Unclassified:
			v = append(v, errs.Violation{Field: field + "/name", Code: "reserved",
				Message: "unclassified is always admissible and is not a class you write"})
		case !classNamePattern.MatchString(name):
			v = append(v, errs.Violation{Field: field + "/name", Code: "pattern",
				Message: "lower-case letters, digits, _ and -, starting with a letter, at most 63 characters"})
		default:
			if j, dup := seen[name]; dup {
				v = append(v, errs.Violation{Field: field + "/name", Code: "duplicate",
					Message: fmt.Sprintf("%s is already class %d", name, j)})
			}
			seen[name] = i
		}
		if utf8.RuneCountInString(desc) > MaxClassDescription {
			v = append(v, errs.Violation{Field: field + "/description", Code: "max_length",
				Message: fmt.Sprintf("at most %d characters", MaxClassDescription)})
		}
		out = append(out, Class{Name: name, Description: desc})
	}
	if len(v) > 0 {
		return ClassSet{}, errs.Validation("investigation_classes_invalid",
			"the class set is not one oto can offer a model", v...)
	}
	return ClassSet{classes: out}, nil
}

// RestoreClassSet rebuilds a set read from its rows, which the CHECKs already held to
// NewClassSet's rules; a row that is not is corruption, said as such.
func RestoreClassSet(classes []Class) (ClassSet, error) {
	s, err := NewClassSet(classes)
	if err != nil {
		return ClassSet{}, errs.Internal("investigation_classes_corrupt", err)
	}
	return s, nil
}

// Empty reports whether the org has written no classes — in which case nothing is
// offered and a Finding carries no classification.
func (s ClassSet) Empty() bool { return len(s.classes) == 0 }

// Classes is the set in the operator's order. A copy.
func (s ClassSet) Classes() []Class { return append([]Class(nil), s.classes...) }

// Admits reports whether a model's pick is a word it may give a Finding: one of the
// operator's classes exactly, or `unclassified`. Nothing is admitted from an empty
// set — not even `unclassified`, because no question was asked.
func (s ClassSet) Admits(name string) bool {
	if s.Empty() {
		return false
	}
	if name == Unclassified {
		return true
	}
	for _, c := range s.classes {
		if c.Name == name {
			return true
		}
	}
	return false
}

// Answers is every word the model may say, the operator's classes in their order and
// then `unclassified`.
func (s ClassSet) Answers() []string {
	out := make([]string, 0, len(s.classes)+1)
	for _, c := range s.classes {
		out = append(out, c.Name)
	}
	return append(out, Unclassified)
}

// Settle is the classification a run's Finding is stored with: "" when no set was
// offered, the run's valid pick, or `unclassified` when it made none.
func (s ClassSet) Settle(picked string) string {
	switch {
	case s.Empty():
		return ""
	case s.Admits(picked):
		return picked
	default:
		return Unclassified
	}
}

// RestoreClassification reads a stored classification: "" for NULL, or a word the
// CHECK admitted. It is NOT checked against today's set — a Finding keeps the class it
// was given when the set changes (ADR 0053 §5).
func RestoreClassification(stored string) (string, error) {
	if stored == "" || classNamePattern.MatchString(stored) {
		return stored, nil
	}
	return "", errs.Newf(errs.KindInternal, "investigation_classification_corrupt",
		"a stored classification %q is not a class name", stored)
}

// ClassifyTool is the name of the one Tool a run is offered to classify its Finding.
// It is not on any allowlist and reads nothing: it is the shape of the answer, offered
// only when the org has classes. The `oto_` prefix is the built-in Tools', which no
// ToolServer's `<toolserver>__<tool>` name can take.
const ClassifyTool = "oto_classify"

// ClassifyArgument is the one argument ClassifyTool takes.
const ClassifyArgument = "class"

// ClassifySchema is ClassifyTool as the model is told about it: one required `class`,
// an enum of exactly the admissible answers, so an endpoint that honours JSON Schema
// cannot even spell a word outside the set — and the loop refuses one anyway.
func (s ClassSet) ClassifySchema() (ToolSchema, error) {
	params, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			ClassifyArgument: map[string]any{
				"type":        "string",
				"enum":        s.Answers(),
				"description": "One of the operator's classes, exactly as written, or unclassified when none clearly fits.",
			},
		},
		"required":             []string{ClassifyArgument},
		"additionalProperties": false,
	})
	if err != nil {
		return ToolSchema{}, errs.Internal("classify_schema", err)
	}
	return NewToolSchema(ClassifyTool,
		"Classify your Finding in the operator's vocabulary. Call it once, before your final answer.", params)
}

// ClassifyPrompt is what is added to the Investigator's prompt when the org has
// classes: the set, each with the operator's own description, and the rule that
// `unclassified` is always admissible and is the answer under doubt.
func (s ClassSet) ClassifyPrompt() string {
	var b strings.Builder
	b.WriteString("Classification. Before your final answer, call " + ClassifyTool +
		" once with the one class below that fits what you found. The classes are this organisation's own; " +
		"do not invent one. If none clearly fits, or you are unsure, answer " + Unclassified +
		": it is always admissible and is the right answer under doubt.\n")
	for _, c := range s.classes {
		b.WriteString("- " + c.Name)
		if c.Description != "" {
			b.WriteString(": " + c.Description)
		}
		b.WriteString("\n")
	}
	b.WriteString("- " + Unclassified + ": none of the above clearly fits.")
	return b.String()
}

// ParseClassifyCall reads the class a ClassifyTool call named, or "" when its
// arguments do not name one — which the loop refuses on the record like a word
// outside the set.
func ParseClassifyCall(arguments string) string {
	var in map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(arguments)), &in) != nil {
		return ""
	}
	var class string
	if json.Unmarshal(in[ClassifyArgument], &class) != nil {
		return ""
	}
	return strings.TrimSpace(class)
}
